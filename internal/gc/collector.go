// Package gc reclaims storage that no longer belongs to anything.
//
// Every build adds a content tag and its layer blobs, and every fetched layer source stays in the
// cache. Without collection both grow forever. With it, the failure mode changes from "runs out
// of disk eventually" to "deletes something still in use" — which is far worse, so most of what
// is here is about not doing that.
//
// The approach is mark and sweep. Marking walks every ImageComposition and collects what it
// references; sweeping deletes what marking did not reach. That is only sound if marking saw
// everything, which is why the first and most important rail is refusing to sweep at all when the
// controller's view is incomplete.
package gc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// KNOWN LIMITATION, recorded here rather than discovered later.
//
// Retention is the ONLY thing protecting an old build. There is deliberately no "protect what a
// running Pod references" rail, because it cannot be built on the current foundations: a Pod
// names a MANIFEST digest, the store holds BLOBS, and the only manifest-to-blob mapping is
// status.history — which is exactly what marking already reads. Scanning Pods would therefore
// protect nothing that retention does not already protect, while costing a Pod informer and
// cluster-wide pods list/watch. It was implemented, measured to have no effect, and removed.
//
// The same missing piece has a larger consequence: manifests live in go-containerregistry's
// in-memory map, and only the CURRENT build is republished at startup, so a controller restart
// already makes older content tags unresolvable regardless of garbage collection. Fixing either
// properly means persisting manifests, which is tracked separately.
//
// Defaults. Deliberately conservative: the cost of keeping something too long is disk, and the
// cost of reclaiming it too early is a workload that cannot start.
const (
	DefaultInterval = time.Hour
	DefaultGrace    = time.Hour
)

// PendingLister reports objects the controller has not yet reconciled. Satisfied by
// controller.Readiness; an interface so this package does not depend on the controller package.
type PendingLister interface {
	Pending(ctx context.Context) ([]string, error)
}

// Collector reclaims unreferenced blobs and cache entries.
type Collector struct {
	client.Client

	// Blobs holds served artifact content. Optional; nil in push-only deployments, where the
	// external registry owns its own lifecycle and this controller has no business deleting
	// from it.
	Blobs store.Store

	// Cache holds fetched layer sources. Optional.
	Cache store.Store

	// Pending gates the sweep. Required: without it there is no way to know whether marking saw
	// every object, and sweeping on an incomplete view deletes live content.
	Pending PendingLister

	// Interval between sweeps.
	Interval time.Duration

	// Grace protects recently written objects. A build in flight has written its blobs but not
	// yet recorded them in status, so without this a sweep landing in that window would delete
	// content that is moments from being referenced.
	Grace time.Duration

	// DryRun logs what would be deleted and deletes nothing.
	DryRun bool
}

// Result summarises one cycle.
type Result struct {
	Skipped          bool
	SkipReason       string
	BlobsKept        int
	BlobsDeleted     int
	ManifestsKept    int
	ManifestsDeleted int
	CacheKept        int
	CacheDeleted     int
	Errors           int
}

// NeedLeaderElection keeps collection on the leader. Two collectors marking from the same API
// server would reach the same conclusion, but only one should be issuing deletes.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start runs collection on an interval until ctx is cancelled.
func (c *Collector) Start(ctx context.Context) error {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := log.FromContext(ctx).WithName("gc")

	// Deliberately not run immediately at startup. Right after start the controller has observed
	// nothing, so the Pending gate would refuse anyway — and waiting one interval means the
	// first cycle runs against a settled view rather than racing the initial reconciles.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			result, err := c.Collect(ctx)
			if err != nil {
				// A failed cycle is not fatal. The next one runs on schedule, and storage
				// growing for another interval is preferable to the controller exiting.
				logger.Error(err, "garbage collection cycle failed")
				continue
			}
			if result.Skipped {
				logger.Info("garbage collection skipped", "reason", result.SkipReason)
				continue
			}
			logger.Info("garbage collection complete",
				"blobsDeleted", result.BlobsDeleted, "blobsKept", result.BlobsKept,
				"manifestsDeleted", result.ManifestsDeleted,
				"cacheDeleted", result.CacheDeleted, "cacheKept", result.CacheKept,
				"errors", result.Errors, "dryRun", c.DryRun)
		}
	}
}

// Collect runs one mark-and-sweep cycle.
func (c *Collector) Collect(ctx context.Context) (Result, error) {
	logger := log.FromContext(ctx).WithName("gc")

	// RAIL 1, and the one that matters most. Marking derives the live set from the objects the
	// controller knows about. An object it has not reconciled contributes nothing, so its blobs
	// and cache entries look exactly like garbage. Skipping the cycle costs one interval of
	// growth; not skipping it costs data.
	pending, err := c.Pending.Pending(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("checking whether the view is complete: %w", err)
	}
	if len(pending) > 0 {
		return Result{
			Skipped: true,
			SkipReason: fmt.Sprintf(
				"%d ImageComposition(s) not yet reconciled, so the live set would be incomplete: %s",
				len(pending), strings.Join(pending, ", ")),
		}, nil
	}

	var list ociv1alpha1.ImageCompositionList
	if err := c.List(ctx, &list); err != nil {
		return Result{}, fmt.Errorf("listing ImageCompositions: %w", err)
	}

	liveBlobs, liveInputs, liveManifests := c.mark(list.Items)

	logger.V(1).Info("marked",
		"blobs", len(liveBlobs), "inputs", len(liveInputs), "manifests", len(liveManifests))

	result := Result{}
	cutoff := time.Now().Add(-c.grace())

	if c.Blobs != nil {
		kept, deleted, errs := c.sweep(ctx, c.Blobs, store.NamespaceBlobs, liveBlobs, cutoff)
		result.BlobsKept, result.BlobsDeleted, result.Errors = kept, deleted, result.Errors+errs
	}
	if c.Blobs != nil {
		kept, deleted, errs := c.sweep(ctx, c.Blobs, store.NamespaceManifests, liveManifests, cutoff)
		result.ManifestsKept, result.ManifestsDeleted = kept, deleted
		result.Errors += errs
	}
	if c.Cache != nil {
		kept, deleted, errs := c.sweep(ctx, c.Cache, store.NamespaceInputs, liveInputs, cutoff)
		result.CacheKept, result.CacheDeleted, result.Errors = kept, deleted, result.Errors+errs
	}
	return result, nil
}

// mark collects every digest that must survive.
func (c *Collector) mark(items []ociv1alpha1.ImageComposition) (blobs, inputs, manifests map[string]struct{}) {
	blobs = make(map[string]struct{})
	inputs = make(map[string]struct{})
	manifests = make(map[string]struct{})

	for i := range items {
		obj := &items[i]

		// Spec layers, not just status: a layer that has been declared but not yet built is
		// already in the cache, and reclaiming it would force an immediate re-fetch.
		//
		// Only fetch entries have a spec-declared digest. sourceRef and configMap digests are
		// resolved at build time and appear in the cache under whatever they resolved to, so they
		// are covered by the grace period until the next build records them.
		for _, l := range obj.Spec.Layers {
			if l.Fetch != nil && l.Fetch.Digest != "" {
				inputs[l.Fetch.Digest] = struct{}{}
			}
		}

		// Retained builds. status.History is already capped at the retention limit by the
		// reconciler, so trimming happens there and marking simply honours it.
		for _, h := range obj.Status.History {
			for _, b := range h.Blobs {
				blobs[b] = struct{}{}
			}
			// The manifest that NAMES those blobs. Reclaiming it while keeping them would leave
			// content nothing can address, which is the same as having deleted it. See ADR 0013.
			if h.Digest != "" {
				manifests[h.Digest] = struct{}{}
			}
			// For a multi-platform build, Digest above is the INDEX and these are its children.
			// They have to be marked explicitly: marking is derived from status and never parses
			// manifest bytes, so nothing here would otherwise know the index points at them.
			// Sweeping them leaves a retained index that resolves to nothing — a failure that
			// surfaces as a 404 at pull time, long after the collection that caused it, and on a
			// reference status still reports as published. See ADR 0018.
			for _, m := range h.Manifests {
				manifests[m] = struct{}{}
			}
		}

		// The current artifact, even if history is somehow missing or truncated. Belt and
		// braces: whatever else is true, what is published right now must remain pullable.
		if obj.Status.Artifact != nil && obj.Status.Artifact.Digest != "" {
			blobs[obj.Status.Artifact.Digest] = struct{}{}
			manifests[obj.Status.Artifact.Digest] = struct{}{}
		}
	}
	return blobs, inputs, manifests
}

// sweep deletes everything in a namespace that marking did not reach.
func (c *Collector) sweep(ctx context.Context, s store.Store, namespace string, live map[string]struct{}, cutoff time.Time) (kept, deleted, errs int) {
	logger := log.FromContext(ctx).WithName("gc").WithValues("namespace", namespace)

	objects, err := s.List(ctx, namespace)
	if err != nil {
		// A partial listing reads as "these objects do not exist", which would make everything
		// missing from it look unreferenced. The Store contract requires List to be complete or
		// fail, and this honours that by treating a failure as a reason to do nothing.
		logger.Error(err, "listing failed; skipping this namespace rather than sweeping a partial view")
		return 0, 0, 1
	}

	var reclaimed []string
	for _, info := range objects {
		digest, ok := digestFromKey(info.Key)
		if !ok {
			// An unrecognised key is not ours to delete.
			logger.V(1).Info("ignoring unrecognised key", "key", info.Key)
			kept++
			continue
		}
		if _, isLive := live[digest]; isLive {
			kept++
			continue
		}

		// RAIL 2. A build writes its blobs before recording them in status, so a sweep landing
		// in that window would see content nothing references yet. Age is a crude proxy for "not
		// mid-flight", but it is a safe one.
		if info.ModTime.After(cutoff) {
			logger.V(1).Info("within grace period, keeping", "key", info.Key, "age", time.Since(info.ModTime))
			kept++
			continue
		}

		if c.DryRun {
			logger.Info("would delete", "key", info.Key, "size", info.Size)
			reclaimed = append(reclaimed, info.Key)
			deleted++
			continue
		}
		if err := s.Delete(ctx, info.Key); err != nil {
			logger.Error(err, "could not delete", "key", info.Key)
			errs++
			continue
		}
		reclaimed = append(reclaimed, info.Key)
		deleted++
	}

	// Every deletion is logged. A collector that silently reclaims is impossible to audit after
	// something goes missing.
	if len(reclaimed) > 0 {
		sort.Strings(reclaimed)
		logger.Info("reclaimed", "count", len(reclaimed), "keys", reclaimed, "dryRun", c.DryRun)
	}
	return kept, deleted, errs
}

// digestFromKey turns "blobs/sha256/abc..." back into "sha256:abc...".
func digestFromKey(key string) (string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return "", false
	}
	if parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[1] + ":" + parts[2], true
}

func (c *Collector) grace() time.Duration {
	if c.Grace <= 0 {
		return DefaultGrace
	}
	return c.Grace
}

// SetupWithManager registers the collector as a managed runnable.
func (c *Collector) SetupWithManager(mgr ctrl.Manager) error {
	if c.Pending == nil {
		return fmt.Errorf("garbage collection requires a pending-object source; refusing to sweep blind")
	}
	return mgr.Add(c)
}
