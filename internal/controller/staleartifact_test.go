package controller

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// The incident these tests exist for, plainly:
//
// A generator bumped a GitRepository's spec.ref.tag from v0.6.5 to v0.6.8 and rotated the
// composition's spec-hash publish tag in the SAME apply. The composition reconciles immediately on
// a spec change; the GitRepository does not — source-controller has to clone the new tag first. In
// that window the GitRepository was Ready=True with a status.artifact still describing v0.6.5, and
// the composer believed it. The new tag did not exist yet, so nothing short-circuited, the layer
// cache served the old tarball from disk without a single network call, and tag
// `sf1ddb722b12a49a2` — the spec hash for v0.6.8 — was published pointing at v0.6.5's content.
//
// That is PERMANENT. The immutable-tag guard refuses to MOVE a tag; it cannot validate a tag's
// FIRST publish, because there is nothing yet to conflict with. When the composer later rebuilt
// correctly it produced a different digest, the guard correctly refused, and the object went
// Stalled/ImmutableTagConflict — which is how the bug was found at all. See ADR 0026.

// staleSource takes a caught-up source and moves its spec forward without its status: exactly what
// the API server leaves behind between a spec write and source-controller's next reconcile.
func staleSource(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.SetGeneration(obj.GetGeneration() + 1)
	return obj
}

// notReadySource keeps the artifact but flips the Ready condition, the shape of a source whose last
// fetch failed and which is therefore still advertising the previous revision's tarball.
func notReadySource(obj *unstructured.Unstructured, reason string) *unstructured.Unstructured {
	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Ready", "status": "False", "reason": reason},
	}, "status", "conditions")
	return obj
}

func sourceRefComposition(name, sourceName string) *ociv1alpha1.ImageComposition {
	return composition(name, ociv1alpha1.Layer{
		Name:      "content",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: sourceName},
		To:        "/content",
	})
}

// TestStaleSourceArtifactIsNeverPublished — the regression test for the incident above.
//
// The source's status.artifact describes revision A while its spec has already moved to revision B.
// Building here publishes A's content under B's tag, permanently, because a tag's first publish is
// the one thing the immutability guard cannot catch. So: no build, no publish, and Reconciling with
// DependencyNotReady until the source catches up.
func TestStaleSourceArtifactIsNeverPublished(t *testing.T) {
	// The artifact still served is the PREVIOUS release's, which is the whole hazard: it fetches
	// perfectly, hashes perfectly, and assembles into a perfectly valid image of the wrong version.
	url, digest := tarball(t, map[string]string{"app/version.json": `{"version": "0.6.5"}`})
	repo := staleSource(gitRepository("app", "default", url, digest, "v0.6.5@sha1:aaaa"))

	obj := sourceRefComposition("app-image", "app")
	r := pendingReconciler(t, obj, repo)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("waiting for a source to catch up must not be an error: %v", err)
	}

	// Asserted first because it is the damage: everything below is how the object should REPORT
	// the wait, and this is what happens if it does not wait at all.
	got := reload(t, r, obj)
	if got.Status.Artifact != nil {
		t.Fatalf("published %q from a source whose status describes the PREVIOUS revision; "+
			"that tag can never be corrected", got.Status.Artifact.Ref)
	}
	// A short fixed retry, not the spec interval: the source is seconds from catching up.
	if res.RequeueAfter != pendingRetryInterval {
		t.Fatalf("RequeueAfter %v, want %v", res.RequeueAfter, pendingRetryInterval)
	}
	if len(got.Status.History) != 0 {
		t.Fatalf("recorded %d builds from a stale source, want none", len(got.Status.History))
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReadyCondition) {
		t.Fatal("Ready=True while the referenced source has not observed its own spec")
	}
	// Not Stalled: source-controller catching up bumps no generation on this object, so a stall
	// would wait for an event that cannot arrive.
	if meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.StalledCondition) != nil {
		t.Fatal("a source that has not caught up yet must never set Stalled")
	}
	reconciling := meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReconcilingCondition)
	if reconciling == nil || reconciling.Reason != ociv1alpha1.ReasonDependencyNotReady {
		t.Fatalf("expected Reconciling/%s, got %+v", ociv1alpha1.ReasonDependencyNotReady, got.Status.Conditions)
	}
}

// TestStaleSourceIsPendingNotTerminal — the triage half, at the resolver where the decision is made.
// Terminal would be wrong for the same reason it is wrong for a missing source: the fix happens in
// another object and raises no generation change here.
func TestStaleSourceIsPendingNotTerminal(t *testing.T) {
	url, digest := tarball(t, map[string]string{"a": "1"})
	repo := staleSource(gitRepository("app", "default", url, digest, "v0.6.5@sha1:aaaa"))
	obj := sourceRefComposition("app-image", "app")
	r := reconcilerWith(t, repo)

	_, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err == nil {
		t.Fatal("resolved a source whose status.artifact predates its own spec")
	}
	var te *recon.TerminalError
	if asTerminalErr(err, &te) {
		t.Fatal("a source that has not caught up must not be terminal")
	}
	var pe *recon.PendingError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a recon.PendingError, got %T: %v", err, err)
	}
}

// TestNotReadySourceIsNotConsumed — a source whose fetch failed keeps serving its last good
// artifact. That artifact is by definition not the revision the spec now names, so it is waited for
// rather than built from.
func TestNotReadySourceIsNotConsumed(t *testing.T) {
	url, digest := tarball(t, map[string]string{"app/version.json": `{"version": "0.6.5"}`})
	repo := notReadySource(gitRepository("app", "default", url, digest, "v0.6.5@sha1:aaaa"),
		"GitOperationFailed")

	obj := sourceRefComposition("app-image", "app")
	r := pendingReconciler(t, obj, repo)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("a not-Ready source must not be returned to the queue as an error: %v", err)
	}
	got := reload(t, r, obj)
	if got.Status.Artifact != nil {
		t.Fatalf("published %q from a source reporting Ready=False", got.Status.Artifact.Ref)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != ociv1alpha1.ReasonDependencyNotReady {
		t.Fatalf("expected Ready=False/%s, got %+v", ociv1alpha1.ReasonDependencyNotReady, got.Status.Conditions)
	}
}

// TestCaughtUpSourceStillBuilds — the counterweight. A refusal that is too eager is worse than the
// bug it prevents: it would stall every sourceRef composition in the cluster on a check nobody
// asked for. A source with generation == observedGeneration and Ready=True must build exactly as
// it always did.
func TestCaughtUpSourceStillBuilds(t *testing.T) {
	url, digest := tarball(t, map[string]string{"app/version.json": `{"version": "0.6.8"}`})
	repo := gitRepository("app", "default", url, digest, "v0.6.8@sha1:bbbb")

	obj := sourceRefComposition("app-image", "app")
	r := pendingReconciler(t, obj, repo)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := reload(t, r, obj)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReadyCondition) {
		t.Fatalf("a caught-up source did not build: %+v", got.Status.Conditions)
	}
	if got.Status.Artifact == nil || got.Status.Artifact.Ref == "" {
		t.Fatal("expected a published artifact from a caught-up source")
	}
}

// TestSourceChangeEnqueuesReferencingCompositions — the companion fix. Without a watch, a source
// finishing its fetch is invisible until spec.interval, which defaults to an hour: the composition
// above would sit correctly refusing to build for up to an hour after the content it wants arrived.
//
// Cross-namespace matters here and is the reason this mapping lists cluster-wide: pointing a
// composition at a shared source in flux-system is the ordinary arrangement.
func TestSourceChangeEnqueuesReferencingCompositions(t *testing.T) {
	sameNamespace := sourceRefComposition("same-namespace", "app")

	crossNamespace := composition("cross-namespace", ociv1alpha1.Layer{
		Name: "content",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "app", Namespace: "flux-system",
		},
		To: "/content",
	})

	// Same name, different kind — must not match, or a Bucket edit would rebuild a Git-backed
	// composition.
	otherKind := composition("other-kind", ociv1alpha1.Layer{
		Name:      "content",
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "Bucket", Name: "app"},
		To:        "/content",
	})
	otherName := sourceRefComposition("other-name", "unrelated")

	r := reconcilerWith(t, sameNamespace, crossNamespace, otherKind, otherName)

	changed := &unstructured.Unstructured{}
	changed.SetName("app")
	changed.SetNamespace("flux-system")

	got := r.compositionsForSource("GitRepository")(context.Background(), changed)

	// Only the composition that explicitly names flux-system: "same-namespace" defaults to its own
	// namespace, which is a different source that happens to share a name.
	want := []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "cross-namespace"},
	}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("enqueued %v, want %v", got, want)
	}

	changed.SetNamespace("default")
	got = r.compositionsForSource("GitRepository")(context.Background(), changed)
	if len(got) != 1 || got[0].Name != "same-namespace" {
		t.Fatalf("enqueued %v, want only same-namespace", got)
	}
}
