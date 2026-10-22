// Package retention keeps the images of live objects from being reclaimed by a registry.
//
// The guarantee (ADR 0031): an image named by the retained status.history of any live
// ImageComposition or ImageBuild is never deleted. Expiry beyond that is best-effort.
//
// The mechanism is a lease: every live object periodically PULLS the manifests its history names,
// by digest and by tag, and the registry keeps whatever was pulled recently. This package therefore
// needs no write or delete permission, objects sharing a digest need no coordination, and eviction
// needs no action (the expiry window doubles as an undo period).
//
// It fails UNSAFE: if refreshing stops for longer than the registry's window, live content is
// deleted. Hence the loud failure reporting and the large interval-to-window margin.
package retention

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// DefaultInterval is how often every live object's images are refreshed.
//
// Against a 30-day registry window this is a margin of 720. The ratio is the guarantee: lowering the
// registry's window means lowering this with it.
const DefaultInterval = time.Hour

// DegradedAfter is how many consecutive failed cycles for one object raise a warning on it. More
// than one, so a single unreachable-registry blip does not train operators to ignore the signal.
const DegradedAfter = 3

// PendingLister reports objects the controller has not yet reconciled.
//
// Required: an object missing from a partial view silently stops being refreshed, and the symptom
// arrives one retention window later. The refresher refuses to run on a partial view.
type PendingLister interface {
	Pending(ctx context.Context) ([]string, error)
}

// Target is one object whose images must be kept alive.
//
// The Refresher takes these rather than listing kinds itself because the composer and builder have
// separate RBAC (ADR 0004); neither may list the other's kind.
type Target struct {
	// Object is what an Event is recorded against.
	Object client.Object
	// Push describes where the object publishes. Nil means the operator's default registry.
	Push *ociv1alpha1.Push
	// Artifact is the current publication, which may not be in History yet.
	Artifact *ociv1alpha1.ArtifactStatus
	// History is the retention record, and the authority on what must stay alive.
	History []ociv1alpha1.BuildRecord
}

// Source yields the objects one component owns.
type Source interface {
	Targets(ctx context.Context) ([]Target, error)
}

// CompositionSource lists ImageCompositions.
type CompositionSource struct{ client.Client }

func (s CompositionSource) Targets(ctx context.Context) ([]Target, error) {
	var list ociv1alpha1.ImageCompositionList
	if err := s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing compositions: %w", err)
	}
	out := make([]Target, 0, len(list.Items))
	for i := range list.Items {
		obj := &list.Items[i]
		out = append(out, Target{Object: obj, Push: obj.Spec.Push,
			Artifact: obj.Status.Artifact, History: obj.Status.History})
	}
	return out, nil
}

// BuildSource lists ImageBuilds.
type BuildSource struct{ client.Client }

func (s BuildSource) Targets(ctx context.Context) ([]Target, error) {
	var list ociv1alpha1.ImageBuildList
	if err := s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}
	out := make([]Target, 0, len(list.Items))
	for i := range list.Items {
		obj := &list.Items[i]
		out = append(out, Target{Object: obj, Push: obj.Spec.Push,
			Artifact: obj.Status.Artifact, History: obj.Status.History})
	}
	return out, nil
}

// Pending reports builds whose observedGeneration lags their generation. The builder has no
// Readiness like the composer's, so this is its whole completeness check.
func (s BuildSource) Pending(ctx context.Context) ([]string, error) {
	var list ociv1alpha1.ImageBuildList
	if err := s.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}
	var pending []string
	for i := range list.Items {
		obj := &list.Items[i]
		if obj.Status.ObservedGeneration != obj.Generation {
			pending = append(pending, obj.Namespace+"/"+obj.Name)
		}
	}
	return pending, nil
}

// Refresher renews the lease on every live object's images.
type Refresher struct {
	client.Client

	// Source yields the objects to refresh. Required.
	Source Source

	// Interval between cycles.
	Interval time.Duration

	// Pending gates a cycle. See PendingLister.
	Pending PendingLister

	// Recorder surfaces sustained failure, which otherwise ends silently in deletion.
	Recorder record.EventRecorder

	// Default is the operator's registry and credential. See recon.DefaultRegistry.
	Default recon.DefaultRegistry

	// Transport, when set, trusts an additional CA on top of the system roots. See recon.Transport.
	Transport http.RoundTripper

	// InsecureRegistries are hosts that may be reached over plain HTTP, matched exactly as the
	// builder matches them.
	InsecureRegistries []string

	// mu guards failures: RefreshNow runs from a reconcile while a cycle may be running.
	mu sync.Mutex
	// failures counts consecutive failed cycles per object, keyed by namespace/name.
	failures map[string]int
	// skips counts consecutive cycles that refreshed NOTHING because the view was partial.
	skips int
}

// Result summarises one cycle.
type Result struct {
	Skipped     bool
	SkipReason  string
	Objects     int
	References  int
	Refreshed   int
	Failed      int
	NotFound    int
	Unsupported int
}

// NeedLeaderElection keeps refreshing on the leader. Concurrent refreshes are harmless; this only
// avoids multiplying registry traffic.
func (r *Refresher) NeedLeaderElection() bool { return true }

// Start refreshes on an interval until ctx is cancelled.
func (r *Refresher) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := log.FromContext(ctx).WithName("retention")

	// Run once immediately, unlike the collector: refreshing early can only keep things alive, and
	// after a long outage an early cycle is what is needed.
	r.cycle(ctx, logger)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.cycle(ctx, logger)
		}
	}
}

func (r *Refresher) cycle(ctx context.Context, logger interface {
	Info(string, ...any)
	Error(error, string, ...any)
}) {
	result, err := r.RefreshOnce(ctx)
	switch {
	case err != nil:
		r.skips = 0
		logger.Error(err, "RETENTION REFRESH FAILED; live images lose their protection if this "+
			"continues for the registry's retention window")
	case result.Skipped:
		// A skipped cycle protects nothing, and one object stuck behind its generation blocks the
		// refresh for every object in the cluster, so persistent skips escalate like failures.
		r.skips++
		if r.skips < DegradedAfter {
			logger.Info("retention refresh skipped", "reason", result.SkipReason,
				"consecutiveSkips", r.skips)
			return
		}
		logger.Error(fmt.Errorf("%s", result.SkipReason),
			"RETENTION REFRESH HAS REFRESHED NOTHING FOR "+
				"SEVERAL CYCLES; live images lose their protection just as surely as if it were "+
				"failing, and this will not clear until every object has been reconciled",
			"consecutiveSkips", r.skips)
	default:
		r.skips = 0
		logger.Info("retention refresh complete",
			"objects", result.Objects, "references", result.References,
			"refreshed", result.Refreshed, "failed", result.Failed,
			"notFound", result.NotFound, "unsupported", result.Unsupported)
	}
}

// RefreshOnce runs one cycle over every object the Source yields.
func (r *Refresher) RefreshOnce(ctx context.Context) (Result, error) {
	if r.Pending == nil {
		return Result{}, fmt.Errorf("no pending lister configured; refusing to refresh on a view " +
			"that may be incomplete")
	}
	if r.Source == nil {
		return Result{}, fmt.Errorf("no source configured; nothing would be refreshed and every " +
			"live image would silently lose its protection")
	}
	pending, err := r.Pending.Pending(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("checking for unreconciled objects: %w", err)
	}
	if len(pending) > 0 {
		return Result{Skipped: true, SkipReason: fmt.Sprintf(
			"%d objects not yet reconciled (%v); a partial view would under-refresh",
			len(pending), pending)}, nil
	}

	targets, err := r.Source.Targets(ctx)
	if err != nil {
		return Result{}, err
	}

	var out Result
	for _, target := range targets {
		r.refreshObject(ctx, target, &out)
	}
	return out, nil
}

// refreshObject pulls every reference one object still names.
//
// Driven by status alone, never by whether the object reconciled: a Stalled object must keep its
// published images alive (ADR 0031).
func (r *Refresher) refreshObject(ctx context.Context, target Target, out *Result) {
	obj := target.Object
	namespace, objName := obj.GetNamespace(), obj.GetName()
	push := target.Push
	// Resolved as the publish path resolves it, so an object with no push block (default registry)
	// is still refreshed.
	repo := ""
	switch {
	case push != nil && push.Repository != "":
		repo = push.Repository
	case r.Default.Configured():
		repo = r.Default.RepositoryFor(namespace, objName)
	}

	if repo == "" {
		// Nowhere it could have published, so nothing to keep alive.
		out.Unsupported++
		return
	}

	refs := refsOf(repo, target.Artifact, target.History)
	if len(refs) == 0 {
		return
	}
	out.Objects++
	out.References += len(refs)

	opts, err := r.remoteOptions(ctx, namespace, repo, push)
	if err != nil {
		out.Failed += len(refs)
		r.noteFailure(ctx, obj, namespace, objName, fmt.Sprintf("registry credentials: %v", err))
		return
	}
	var refOpts []name.Option
	if recon.InsecureHost(repo, r.InsecureRegistries) {
		refOpts = append(refOpts, name.Insecure)
	}

	// References of the CURRENT artifact: losing one is not history expiring (ADR 0049, amended).
	current := map[string]bool{}
	for _, ref := range refsOf(repo, target.Artifact, nil) {
		current[ref] = true
	}

	var failed, gone, lostCurrent int
	var lastErr, lastGone, lastCurrent error
	for _, ref := range refs {
		parsed, err := name.ParseReference(ref, refOpts...)
		if err != nil {
			failed++
			lastErr = err
			continue
		}

		// A GET, not a HEAD: registries renew recency on a pull, and a HEAD need not count as one.
		// remote.Image fetches only the manifest; layers are lazy.
		img, err := remote.Image(parsed, opts...)
		if err == nil {
			_, err = img.Manifest()
		}
		switch {
		case err == nil:
			out.Refreshed++
			// Referrers (SBOM, provenance, attestations) are untagged and would otherwise be
			// reclaimed by deleteUntagged while their image lives (threat D6). Cosign's .sig is a
			// tag, so keepTags covers it.
			r.refreshReferrers(parsed, opts, out)
		case recon.IsNotFound(err):
			// Already deleted: permanent, so it is reported but does not count as a failure, or
			// expired history would keep the object Degraded forever (ADR 0049). The current
			// artifact is the exception; see lostCurrent below.
			out.NotFound++
			if current[ref] {
				lostCurrent++
				lastCurrent = fmt.Errorf("%s is gone: %w", ref, err)
				continue
			}
			gone++
			lastGone = fmt.Errorf("%s is gone: %w", ref, err)
		default:
			out.Failed++
			failed++
			lastErr = err
		}
	}

	// Reported every cycle as a summary; it does not touch the failure count.
	if gone > 0 {
		recon.Event(r.Recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonRetentionLost,
			fmt.Sprintf("%d of %d references this object published are already gone from the "+
				"registry and cannot be refreshed back into existence (%v).",
				gone, len(refs), lastGone))
	}

	// A missing current artifact is what workloads pull, so it counts towards escalation. Its
	// controller repairs it on the next reconcile; one still missing DegradedAfter cycles later is
	// not being repaired. (A moving tag caused exactly this before ADR 0060.)
	if lostCurrent > 0 {
		recon.Event(r.Recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonArtifactLost,
			fmt.Sprintf("The artifact this object currently reports -- what a workload referencing "+
				"it pulls -- is gone from the registry (%v). Its controller republishes or rebuilds "+
				"it on the next reconcile.", lastCurrent))
		failed += lostCurrent
		if lastErr == nil {
			lastErr = lastCurrent
		}
	}

	if failed > 0 {
		r.noteFailure(ctx, obj, namespace, objName,
			fmt.Sprintf("%d of %d references: %v", failed, len(refs), lastErr))
		return
	}
	// Gone history references do not hold the counter open, so an old loss cannot mask an outage.
	r.clearFailure(namespace, objName)
}

// noteFailure counts consecutive failures and gets loud once they persist.
func (r *Refresher) noteFailure(ctx context.Context, obj client.Object, namespace, objName, detail string) {
	key := namespace + "/" + objName
	r.mu.Lock()
	if r.failures == nil {
		r.failures = map[string]int{}
	}
	r.failures[key]++
	n := r.failures[key]
	r.mu.Unlock()

	log.FromContext(ctx).WithName("retention").Error(fmt.Errorf("%s", detail),
		"refresh failed", "object", key, "consecutiveFailures", n)

	if n < DegradedAfter {
		return
	}
	// An Event, not a condition: the reconciler owns the conditions, and a second writer would race it.
	recon.Event(r.Recorder, obj, corev1.EventTypeWarning, ociv1alpha1.ReasonRetentionDegraded,
		fmt.Sprintf("Retention refresh has failed %d times in a row (%s). Images this object "+
			"published are protected only while they are refreshed; if this continues for the "+
			"registry's retention window they will be deleted.", n, detail))
}

func (r *Refresher) clearFailure(namespace, objName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures != nil {
		delete(r.failures, namespace+"/"+objName)
	}
}

// RefreshNow renews the lease on one object's images without waiting for the next cycle.
//
// Called straight after a publish: until then the artifact has no lease (zot can even carry an old
// pull timestamp onto a new tag for a known digest), and a GC pass in the gap would collect it.
// Not gated on Pending, which guards against partial views of a whole cycle, not one known object.
func (r *Refresher) RefreshNow(ctx context.Context, target Target) Result {
	var out Result
	r.refreshObject(ctx, target, &out)
	return out
}

// refsOf lists every reference an object still needs kept alive: the digest AND each tag of the
// current artifact and every retained record. A registry can govern tagged and untagged manifests
// differently, so pulling only the digest would let the tag be collected. repo is never empty.
func refsOf(repo string, artifact *ociv1alpha1.ArtifactStatus,
	history []ociv1alpha1.BuildRecord) []string {

	seen := map[string]struct{}{}
	var refs []string
	add := func(ref string) {
		if ref == "" {
			return
		}
		if _, dup := seen[ref]; dup {
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}

	if artifact != nil {
		add(digestRef(repo, artifact.Digest))
		for _, tag := range artifact.Tags {
			add(qualify(repo, tag))
		}
	}
	for _, rec := range history {
		add(digestRef(repo, rec.Digest))
		for _, tag := range rec.Tags {
			add(qualify(repo, tag))
		}
	}
	return refs
}

func digestRef(repo, digest string) string {
	if digest == "" {
		return ""
	}
	return repo + "@" + digest
}

// qualify rebuilds a stored tag against the resolved repository, keeping only the tag itself.
//
// Stored tags use the PUBLIC host (Ingress/NodePort), which a pod may not resolve; using them as-is
// failed every tag refresh while digests succeeded, so tags got reclaimed (ADR 0048).
func qualify(repo, tag string) string {
	t := bareTag(tag)
	if t == "" {
		return ""
	}
	return repo + ":" + t
}

// bareTag strips any repository the stored value carries.
//
// The tag follows the last ":" AFTER the last "/", which survives a host port
// ("host:30500/ns/app:v1"). A repository with no tag ("host:30500/ns/app") or a digest reference
// yields "", since appending it to repo would fabricate a reference that was never published.
func bareTag(tag string) string {
	if tag == "" {
		return ""
	}
	// Checked first: a digest contains a colon. Digests reach the refresh set through digestRef.
	if strings.Contains(tag, "@") {
		return ""
	}
	colon := strings.LastIndex(tag, ":")
	slash := strings.LastIndex(tag, "/")
	if colon > slash {
		return tag[colon+1:]
	}
	if slash >= 0 {
		return ""
	}
	return tag
}

// remoteOptions reads the push credential. Only pull access is ever used, so a pull-scoped
// credential in that Secret suffices.
func (r *Refresher) remoteOptions(
	ctx context.Context, namespace, repository string, push *ociv1alpha1.Push,
) ([]remote.Option, error) {
	return recon.RemoteAuth{
		Reader:    r.Client,
		Transport: r.Transport,
		Default:   r.Default,
		// A plain error: a refresh failure is counted and escalated, not surfaced on a status.
		Soft: fmt.Errorf,
	}.Options(ctx, namespace, repository, push)
}

// SetupWithManager registers the refresher as a leader-elected runnable.
func (r *Refresher) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(r)
}

// refreshReferrers pulls whatever is attached to a digest, so untagged attestations are kept alive
// alongside the artifact.
//
// Failures are counted but never fatal: a registry without a Referrers API must not turn a working
// refresh into a reported failure.
func (r *Refresher) refreshReferrers(ref name.Reference, opts []remote.Option, out *Result) {
	dig, ok := ref.(name.Digest)
	if !ok {
		// Referrers hang off digests; tags are refreshed as tags.
		return
	}

	idx, err := remote.Referrers(dig, opts...)
	if err != nil {
		return
	}
	mf, err := idx.IndexManifest()
	if err != nil {
		return
	}
	for _, d := range mf.Manifests {
		attached := dig.Context().Digest(d.Digest.String())
		if _, err := remote.Get(attached, opts...); err != nil {
			out.Failed++
			continue
		}
		out.Refreshed++
	}
}
