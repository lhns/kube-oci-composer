package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

func check(t *testing.T, r *Readiness) error {
	t.Helper()
	return r.Check(httptest.NewRequest("GET", "/readyz", nil))
}

// TestNotReadyUntilArtifactsAreBuilt is the whole point: the pod must stay out of the Service
// while the store is still empty, or every pull in that window is a 404 and workloads land in
// ImagePullBackOff for no reason.
func TestNotReadyUntilArtifactsAreBuilt(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("warming", urlLayer("core", url, digest, "/core"))
	r, _ := servingReconciler(t, obj)
	r.Readiness = &Readiness{Client: r.Client}

	err := check(t, r.Readiness)
	if err == nil {
		t.Fatal("reported ready before anything was built")
	}
	if !strings.Contains(err.Error(), "default/warming") {
		t.Fatalf("error should name the pending object, got: %v", err)
	}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := check(t, r.Readiness); err != nil {
		t.Fatalf("still not ready after building: %v", err)
	}
}

// TestStalledObjectDoesNotBlockReadiness — one bad digest must not hold the endpoint out of the
// Service for every unrelated artifact. Readiness covers the startup window; conditions report
// health.
func TestStalledObjectDoesNotBlockReadiness(t *testing.T) {
	url, _ := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("broken", urlLayer("core", url, "sha256:"+strings.Repeat("0", 64), "/core"))
	r, _ := servingReconciler(t, obj)
	r.Readiness = &Readiness{Client: r.Client}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := check(t, r.Readiness); err != nil {
		t.Fatalf("a permanently stalled object blocked readiness: %v", err)
	}
}

// TestPushModeDoesNotGateReadiness — an artifact published to an external registry is served by
// that registry whether this pod is up or not, so it has no business holding readiness back.
func TestPushModeDoesNotGateReadiness(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("external", urlLayer("core", url, digest, "/core"))
	obj.Spec.Publish = nil
	obj.Spec.Push = &ociv1alpha1.Push{Repository: "registry.example.com/external", Tags: []string{"v1"}}
	r, _ := servingReconciler(t, obj)
	r.Readiness = &Readiness{Client: r.Client}

	if err := check(t, r.Readiness); err != nil {
		t.Fatalf("a push-mode object gated readiness: %v", err)
	}
}

// TestForgetDropsDeletedObjects — the tracker must not grow without bound.
func TestForgetDropsDeletedObjects(t *testing.T) {
	tracker := &Readiness{}
	key := types.NamespacedName{Namespace: "default", Name: "gone"}

	tracker.Observe(key)
	if !tracker.observed(key) {
		t.Fatal("Observe did not record the object")
	}
	tracker.Forget(key)
	if tracker.observed(key) {
		t.Fatal("Forget did not drop the object")
	}
}
