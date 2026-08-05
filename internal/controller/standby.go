package controller

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/serve"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// StandbyReplay keeps EVERY replica's registry populated, so every replica can serve pulls.
//
// Without it, only the leader can serve, and the endpoint is a single point of failure. That is
// tolerable when artifacts are pulled once and cached by containerd, and not tolerable at all
// when they are pulled often — which is the normal case with spec-hash tags, because every change
// to a spec produces a new tag and therefore a new pull on every node that runs the workload.
//
// Two things stopped a standby from serving, and this addresses the second:
//
//  1. The blob store was node-local. Solved outside this type, by pointing every replica at
//     shared storage (an RWX volume or S3). The operator asserts that with --shared-storage,
//     because a process cannot tell whether the directory it was handed is shared.
//  2. Manifests live in the registry's in-memory map, filled in by publishing. A replica that
//     never publishes therefore knows no manifests and would answer 404 for artifacts whose blobs
//     are sitting right there in the shared store.
//
// So this walks status.history — the same source the leader's own post-restart replay uses — and
// writes each retained manifest into the local registry. Blobs are already shared; only the small
// manifests need restoring.
//
// It reads the API and writes to its own in-process registry. It never writes to the API, never
// fetches, never assembles, and never deletes, so it is safe to run without leader election.
type StandbyReplay struct {
	Client    client.Client
	Server    *serve.Server
	Readiness *Readiness

	// Interval is how often to look for builds published by the leader since the last pass.
	Interval time.Duration
}

// NeedLeaderElection is false: this exists precisely so non-leaders are useful.
func (s *StandbyReplay) NeedLeaderElection() bool { return false }

// Start replays until ctx is cancelled.
//
// The first pass is retried quickly because the manager starts non-leader runnables immediately,
// possibly before the client's cache has synced; a failed List there is expected rather than
// alarming. Later passes use the configured interval.
func (s *StandbyReplay) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}

	delay := time.Second
	for {
		if err := s.replayAll(ctx); err != nil {
			log.FromContext(ctx).V(1).Info("standby replay pass failed; will retry", "error", err)
		} else {
			delay = interval
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

func (s *StandbyReplay) replayAll(ctx context.Context) error {
	var list ociv1alpha1.ImageCompositionList
	if err := s.Client.List(ctx, &list); err != nil {
		return err
	}

	for i := range list.Items {
		obj := &list.Items[i]
		// Push-mode objects are served by someone else's registry, and a deleted object is on
		// its way out; neither has anything to restore here.
		if obj.Spec.Push != nil || !obj.DeletionTimestamp.IsZero() {
			continue
		}
		s.replayOne(ctx, obj)
	}
	return nil
}

func (s *StandbyReplay) replayOne(ctx context.Context, obj *ociv1alpha1.ImageComposition) {
	logger := log.FromContext(ctx).WithValues("imagecomposition", objKey(obj).String())
	repoPath := publishName(obj)

	for _, h := range obj.Status.History {
		if h.Digest == "" || s.Server.HasManifest(ctx, repoPath, h.Digest) {
			continue
		}

		raw, err := s.Server.LoadManifest(ctx, h.Digest)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				logger.Error(err, "could not read a stored manifest", "digest", h.Digest)
			}
			// Not found is ordinary: published before manifest persistence existed, or already
			// reclaimed. Nothing to restore and nothing alarming.
			continue
		}

		// Digest first, as in the leader's replay: it is the reference that is always correct,
		// and the only one a build published without tags has.
		if err := s.Server.PutManifest(ctx, repoPath, h.Digest, raw); err != nil {
			logger.Error(err, "could not restore a manifest by digest", "digest", h.Digest)
			continue
		}
		for _, tag := range h.Tags {
			if err := s.Server.PutManifest(ctx, repoPath, tag, raw); err != nil {
				logger.Error(err, "could not restore a tag", "tag", tag, "digest", h.Digest)
			}
		}
	}

	// Observed on ATTEMPT, matching Readiness.Observe's own rule. A replica that has considered
	// every object is as warm as it is going to get; gating on success instead would let one
	// object with an unreclaimable manifest hold the whole endpoint out of the Service.
	if s.Readiness != nil {
		s.Readiness.Observe(types.NamespacedName{Namespace: obj.Namespace, Name: obj.Name})
	}
}
