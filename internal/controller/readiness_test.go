package controller

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

func pendingOf(t *testing.T, r *Readiness) []string {
	t.Helper()
	pending, err := r.Pending(t.Context())
	if err != nil {
		t.Fatalf("listing pending objects: %v", err)
	}
	return pending
}

// TestUnobservedObjectsAreReportedPending — the completeness question retention depends on.
//
// This used to gate the readiness probe, keeping the pod out of the Service until the served store
// was warm. There is no store now (ADR 0035); what survives is the same fact serving a different
// purpose, because refreshing on a partial view under-protects whatever is missing from it.
func TestUnobservedObjectsAreReportedPending(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("warming", urlLayer("core", url, digest, "/core"))
	r, _ := registryReconciler(t, obj)
	r.Readiness = &Readiness{Client: r.Client}

	pending := pendingOf(t, r.Readiness)
	if len(pending) == 0 {
		t.Fatal("an object this process has never reconciled was not reported pending")
	}
	if !slices.Contains(pending, "default/warming") {
		t.Fatalf("pending %v should name the unobserved object", pending)
	}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pending := pendingOf(t, r.Readiness); len(pending) != 0 {
		t.Fatalf("pending %v after reconciling every object", pending)
	}
}

// TestStalledObjectIsNotPending — pending means UNOBSERVED, not unhealthy.
//
// An object that reconciled and failed has been seen; the refresher knows what it published and can
// keep it alive. Treating it as pending would stop refreshing everything else in the cluster because
// one object has a bad digest.
func TestStalledObjectIsNotPending(t *testing.T) {
	url, _ := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("broken", urlLayer("core", url, "sha256:"+strings.Repeat("0", 64), "/core"))
	r, _ := registryReconciler(t, obj)
	r.Readiness = &Readiness{Client: r.Client}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pending := pendingOf(t, r.Readiness); len(pending) != 0 {
		t.Fatalf("a stalled object was reported pending %v, which would stop the refresher "+
			"running at all", pending)
	}
}

// TestEveryUnobservedObjectCounts — there is no exemption for objects with spec.push.
//
// There used to be: a push-mode object was not served from here, so it could not hold the readiness
// probe back. Every object publishes to a registry now, so that exemption would match everything and
// Pending would always return empty -- which the retention refresher reads as "the view is complete"
// while having observed nothing. It would refresh nothing, report success, and images would start
// disappearing one retention window later.
func TestEveryUnobservedObjectCounts(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("external", urlLayer("core", url, digest, "/core"))
	obj.Spec.Push = &ociv1alpha1.Push{Repository: "registry.example.com/external", Tags: []string{"v1"}}
	r, _ := registryReconciler(t, obj)
	r.Readiness = &Readiness{Client: r.Client}

	if pending := pendingOf(t, r.Readiness); !slices.Contains(pending, "default/external") {
		t.Fatalf("pending %v omits an unobserved object because it names its own repository; "+
			"the refresher would treat an empty view as a complete one", pending)
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
