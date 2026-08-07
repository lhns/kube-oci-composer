package controller

import (
	"context"
	"errors"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// replayer tracks which objects have had their history replayed into the registry this process.
type replayer struct {
	mu   sync.Mutex
	done map[types.NamespacedName]struct{}
}

func (r *replayer) mark(key types.NamespacedName) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done == nil {
		r.done = make(map[types.NamespacedName]struct{})
	}
	if _, ok := r.done[key]; ok {
		return false
	}
	r.done[key] = struct{}{}
	return true
}

func (r *replayer) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.done, key)
}

// replayHistory restores previously published builds into the registry.
//
// The registry's manifest store is in-memory, so after a restart it knows only what has been
// pushed since. The reconcile republishes the CURRENT build, but every older one — its content
// tag and, more importantly, its digest reference — would 404 until it aged out of history
// entirely. That breaks a pod pinned to the previous digest by image automation, which is the
// normal state of affairs between a build and the commit that rolls the workload onto it.
//
// Blobs are already in the store, so this writes one small manifest per retained build.
//
// Failures here are logged and not returned. A build that cannot be replayed is exactly as
// unavailable as it was a moment ago, whereas failing the reconcile would also stop the current
// build from being published — trading a stale reference for no reference at all.
func (r *ImageCompositionReconciler) replayHistory(ctx context.Context, obj *ociv1alpha1.ImageComposition) {
	if r.Server == nil || obj.Spec.Push != nil || len(obj.Status.History) == 0 {
		return
	}
	key := objKey(obj)
	if !r.replay.mark(key) {
		return // already done this process
	}

	logger := log.FromContext(ctx).WithValues("imagecomposition", key.String())
	repoPath := publishName(obj)

	var restored, missing int
	for _, h := range obj.Status.History {
		if h.Digest == "" {
			continue
		}
		// Skip what the registry already has: the current build was just republished by the
		// reconcile, and there is no reason to rewrite it.
		if r.Server.HasManifest(ctx, repoPath, h.Digest) {
			continue
		}

		raw, err := r.Server.LoadManifest(ctx, h.Digest)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Published before manifest persistence existed, or its manifest was reclaimed.
				// Nothing to restore, and nothing alarming.
				missing++
				continue
			}
			logger.Error(err, "could not read a stored manifest", "digest", h.Digest)
			continue
		}

		// CHILDREN FIRST, for a multi-platform build. An index restored on its own resolves —
		// the reference exists, HEAD succeeds, status looks right — and every pull that follows
		// its descriptors 404s. Restoring children first means the index is never published over
		// an incomplete set, and a child that cannot be restored skips this entry rather than
		// leaving that state behind permanently.
		if !restoreChildren(ctx, r.Server, logger, repoPath, h) {
			missing++
			continue
		}

		// The digest reference first: that is what a workload pinned by image automation uses,
		// and it is the one that matters if a tag write fails. It is also the only reference a
		// build published without tags has.
		if err := r.Server.PutManifest(ctx, repoPath, h.Digest, raw); err != nil {
			logger.Error(err, "could not restore a manifest by digest", "digest", h.Digest)
			continue
		}
		for _, tag := range h.Tags {
			if err := r.Server.PutManifest(ctx, repoPath, tag, raw); err != nil {
				logger.Error(err, "could not restore a tag", "tag", tag, "digest", h.Digest)
			}
		}
		restored++
	}

	if restored > 0 || missing > 0 {
		logger.Info("replayed published history",
			"restored", restored, "withoutStoredManifest", missing, "retained", len(obj.Status.History))
	}
}

// objKey is the tracker key for an object.
func objKey(obj *ociv1alpha1.ImageComposition) types.NamespacedName {
	return types.NamespacedName{Namespace: obj.Namespace, Name: obj.Name}
}
