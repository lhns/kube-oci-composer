package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// These tests assert the behaviours a Flux-ecosystem controller is expected to have. They are
// easy to get subtly wrong and nothing else would notice: a missing observedGeneration makes
// `kubectl wait` lie, and a Stalled object that still requeues turns a bad spec into a hot loop.

func reconcileOnce(t *testing.T, r *ImageCompositionReconciler, obj *ociv1alpha1.ImageComposition) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace},
	})
}

func reload(t *testing.T, r *ImageCompositionReconciler, obj *ociv1alpha1.ImageComposition) *ociv1alpha1.ImageComposition {
	t.Helper()
	var out ociv1alpha1.ImageComposition
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), &out); err != nil {
		t.Fatalf("reloading: %v", err)
	}
	return &out
}

// TestReadyAndObservedGeneration — `kubectl wait --for=condition=Ready` is only meaningful if
// both are set together on success.
func TestReadyAndObservedGeneration(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("ready", urlLayer("core", url, digest, "/core"))
	obj.Generation = 4
	obj.Spec.Interval = &metav1.Duration{Duration: 30 * time.Minute}
	r, _ := registryReconciler(t, obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Minute {
		t.Fatalf("requeue after %v, want the spec interval 30m", res.RequeueAfter)
	}

	got := reload(t, r, obj)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReadyCondition) {
		t.Fatalf("not Ready: %+v", got.Status.Conditions)
	}
	if got.Status.ObservedGeneration != 4 {
		t.Fatalf("observedGeneration %d, want 4", got.Status.ObservedGeneration)
	}
	if got.Status.Artifact == nil || got.Status.Artifact.Digest == "" {
		t.Fatal("status.artifact was not recorded")
	}
	if meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReconcilingCondition) != nil {
		t.Fatal("Reconciling should be cleared once Ready")
	}
}

// TestFinalizerIsAdded — Flux controllers own a finalizer so deletion is observable rather than
// instantaneous.
func TestFinalizerIsAdded(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("finalizer", urlLayer("core", url, digest, "/core"))
	r, _ := registryReconciler(t, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := reload(t, r, obj)
	if !controllerutilContainsFinalizer(got, ociv1alpha1.Finalizer) {
		t.Fatalf("finalizer missing: %v", got.Finalizers)
	}
}

// TestStalledDoesNotRequeue is the split that most defines whether a controller feels
// Flux-shaped. A terminal error must not be retried on a timer — the fix is a spec change, and
// the resulting watch event is what wakes it.
func TestStalledDoesNotRequeue(t *testing.T) {
	url, _ := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("stalled", urlLayer("core", url, "sha256:"+strings.Repeat("0", 64), "/core"))
	r, _ := registryReconciler(t, obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("a terminal error must not be returned to the queue: %v", err)
	}
	// An empty Result is the whole assertion: no requeue of any kind. Comparing the struct rather
	// than named fields also keeps this honest as ctrl.Result evolves — Requeue is deprecated in
	// favour of RequeueAfter, and checking either one alone would quietly stop covering the other.
	if res != (ctrl.Result{}) {
		t.Fatalf("stalled object requeued: %+v", res)
	}

	got := reload(t, r, obj)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.StalledCondition) {
		t.Fatalf("expected Stalled=True, got %+v", got.Status.Conditions)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReadyCondition) {
		t.Fatal("Ready must not be true alongside Stalled")
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.StalledCondition)
	if cond.Reason != ociv1alpha1.ReasonDigestMismatch {
		t.Fatalf("reason %q, want %q", cond.Reason, ociv1alpha1.ReasonDigestMismatch)
	}
	if got.Status.Artifact != nil {
		t.Fatal("status.artifact was recorded despite a terminal failure")
	}
}

// TestTransientFailureRequeuesWithBackoff — the other half of the split. An unreachable URL is
// not the user's fault and must be retried.
func TestTransientFailureRequeuesWithBackoff(t *testing.T) {
	// Port 1 on loopback refuses connections immediately, so this fails fast and locally.
	obj := composition("transient", urlLayer("core", "http://127.0.0.1:1/missing.tgz",
		"sha256:"+strings.Repeat("a", 64), "/core"))
	r, _ := registryReconciler(t, obj)

	if _, err := reconcileOnce(t, r, obj); err == nil {
		t.Fatal("a transient failure must be returned so controller-runtime backs off")
	}

	got := reload(t, r, obj)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.ReconcilingCondition) {
		t.Fatalf("expected Reconciling=True, got %+v", got.Status.Conditions)
	}
	if meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.StalledCondition) != nil {
		t.Fatal("a transient failure must not set Stalled")
	}
}

// TestSuspendHaltsReconciliation — suspend must stop work without publishing and without
// deleting what already exists.
func TestSuspendHaltsReconciliation(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("suspended", urlLayer("core", url, digest, "/core"))
	obj.Spec.Suspend = true
	r, _ := registryReconciler(t, obj)

	res, err := reconcileOnce(t, r, obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("a suspended object should not be requeued, got %v", res.RequeueAfter)
	}

	got := reload(t, r, obj)
	cond := meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReadyCondition)
	if cond == nil || cond.Reason != ociv1alpha1.ReasonSuspended {
		t.Fatalf("expected Ready=False/Suspended, got %+v", got.Status.Conditions)
	}
	if got.Status.Artifact != nil {
		t.Fatal("a suspended object published something")
	}
}

// TestReconcileRequestAnnotationIsEchoed — `flux reconcile` decides whether it worked by
// comparing the annotation against status.lastHandledReconcileAt.
func TestReconcileRequestAnnotationIsEchoed(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("annotated", urlLayer("core", url, digest, "/core"))
	obj.Annotations = map[string]string{ociv1alpha1.ReconcileRequestAnnotation: "2026-01-01T00:00:00Z"}
	r, _ := registryReconciler(t, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := reload(t, r, obj)
	if got.Status.LastHandledReconcileAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("lastHandledReconcileAt %q was not echoed", got.Status.LastHandledReconcileAt)
	}
}

// TestFailedReconcileStillEchoesAndObserves — both fields describe the pass, not its outcome.
//
// Echoing only on success makes `flux reconcile` wait for a token that never arrives and then
// report a timeout, hiding the failure the object is already describing. A stale
// observedGeneration reads to kstatus as "still working" rather than "failed". Both are worst
// exactly when something is broken, which is when someone is looking.
func TestFailedReconcileStillEchoesAndObserves(t *testing.T) {
	url, _ := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("failed-echo", urlLayer("core", url, "sha256:"+strings.Repeat("0", 64), "/core"))
	obj.Annotations = map[string]string{ociv1alpha1.ReconcileRequestAnnotation: "2026-01-01T00:00:00Z"}
	r, _ := registryReconciler(t, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("a terminal error must not be returned to the queue: %v", err)
	}

	got := reload(t, r, obj)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, ociv1alpha1.StalledCondition) {
		t.Fatalf("expected this object to have failed: %+v", got.Status.Conditions)
	}
	if got.Status.LastHandledReconcileAt != "2026-01-01T00:00:00Z" {
		t.Errorf("a failed reconcile did not echo the request (%q), so `flux reconcile` would time "+
			"out instead of reporting the failure", got.Status.LastHandledReconcileAt)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration %d is behind generation %d after a failed reconcile, which "+
			"reads as still-in-progress rather than failed",
			got.Status.ObservedGeneration, got.Generation)
	}
}

// TestDeletionRemovesTheFinalizer — and deliberately leaves published artifacts alone, since a
// running workload may still be pulling them.
func TestDeletionRemovesTheFinalizer(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("deleted", urlLayer("core", url, digest, "/core"))
	r, _ := registryReconciler(t, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	current := reload(t, r, obj)
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}

	var out ociv1alpha1.ImageComposition
	err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), &out)
	if err == nil && controllerutilContainsFinalizer(&out, ociv1alpha1.Finalizer) {
		t.Fatal("finalizer was not removed, so the object can never be deleted")
	}
}
