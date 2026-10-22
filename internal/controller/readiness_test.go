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

// TestUnobservedObjectsAreReportedPending — retention must know whether its view is complete,
// because refreshing on a partial view under-protects whatever is missing from it (ADR 0035).
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

// TestStalledObjectIsNotPending — pending means UNOBSERVED, not unhealthy. Otherwise one failing
// object would stop the refresher for the whole cluster.
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

// TestEveryUnobservedObjectCounts — no exemption for objects with spec.push. Every object
// publishes to a registry, so an exemption would make Pending always empty, and the refresher
// would take an unobserved view as complete and let images expire.
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
