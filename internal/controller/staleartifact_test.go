package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// ADR 0026: a source whose spec has moved on but whose status.artifact still describes the
// previous revision must not be built from. The new tag's FIRST publish would carry the old
// content, and the immutable-tag guard cannot catch a first publish.

// staleSource bumps a caught-up source's generation without its status, as between a spec write
// and source-controller's next reconcile.
func staleSource(obj *unstructured.Unstructured) *unstructured.Unstructured {
	obj.SetGeneration(obj.GetGeneration() + 1)
	return obj
}

// notReadySource keeps the artifact but sets Ready=False: a failed fetch still advertising the
// previous revision.
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

// TestStaleSourceArtifactIsNeverPublished — no build, no publish, and Reconciling/DependencyNotReady
// until the source catches up.
func TestStaleSourceArtifactIsNeverPublished(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"app/version.json": `{"version": "0.6.5"}`})
	repo := staleSource(gitRepository("app", "default", url, digest, "v0.6.5@sha1:aaaa"))

	obj := sourceRefComposition("app-image", "app")
	r := pendingReconciler(t, obj, repo)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("waiting for a source to catch up must not be an error: %v", err)
	}

	got := reload(t, r, obj)
	if got.Status.Artifact != nil {
		t.Fatalf("published %q from a source whose status describes the PREVIOUS revision; "+
			"that tag can never be corrected", got.Status.Artifact.Ref)
	}
	// A short fixed retry, not the spec interval: the source is seconds from catching up.
	if res.RequeueAfter != recon.PendingRetryInterval {
		t.Fatalf("RequeueAfter %v, want %v", res.RequeueAfter, recon.PendingRetryInterval)
	}
	if len(got.Status.History) != 0 {
		t.Fatalf("recorded %d builds from a stale source, want none", len(got.Status.History))
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReadyCondition) {
		t.Fatal("Ready=True while the referenced source has not observed its own spec")
	}
	// Not Stalled: the source catching up bumps no generation on this object.
	if meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.StalledCondition) != nil {
		t.Fatal("a source that has not caught up yet must never set Stalled")
	}
	reconciling := meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReconcilingCondition)
	if reconciling == nil || reconciling.Reason != ociv1alpha1.ReasonDependencyNotReady {
		t.Fatalf("expected Reconciling/%s, got %+v", ociv1alpha1.ReasonDependencyNotReady, got.Status.Conditions)
	}
}

// TestStaleSourceIsPendingNotTerminal — the triage half, at the resolver: the fix happens in
// another object and raises no generation change here.
func TestStaleSourceIsPendingNotTerminal(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"a": "1"})
	repo := staleSource(gitRepository("app", "default", url, digest, "v0.6.5@sha1:aaaa"))
	obj := sourceRefComposition("app-image", "app")
	r := reconcilerWith(t, repo)

	_, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err == nil {
		t.Fatal("resolved a source whose status.artifact predates its own spec")
	}
	if recon.IsTerminal(err) {
		t.Fatal("a source that has not caught up must not be terminal")
	}
	if !recon.IsPending(err) {
		t.Fatalf("expected a recon.PendingError, got %T: %v", err, err)
	}
}

// TestNotReadySourceIsNotConsumed — a failed source keeps serving its last good artifact, which is
// not the revision its spec names, so it is waited for rather than built from.
func TestNotReadySourceIsNotConsumed(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"app/version.json": `{"version": "0.6.5"}`})
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

// TestCaughtUpSourceStillBuilds — the counterweight: generation == observedGeneration and
// Ready=True must build, or every sourceRef composition would stall.
func TestCaughtUpSourceStillBuilds(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"app/version.json": `{"version": "0.6.8"}`})
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

// TestSourceChangeEnqueuesReferencingCompositions — without this watch a source catching up would
// go unnoticed until spec.interval (an hour). The mapping lists cluster-wide because a shared
// source in flux-system is the ordinary arrangement.
func TestSourceChangeEnqueuesReferencingCompositions(t *testing.T) {
	sameNamespace := sourceRefComposition("same-namespace", "app")

	crossNamespace := composition("cross-namespace", ociv1alpha1.Layer{
		Name: "content",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "app", Namespace: "flux-system",
		},
		To: "/content",
	})

	// Same name, different kind: must not match.
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

	// Only the one naming flux-system: "same-namespace" defaults to its own namespace.
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
