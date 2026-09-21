package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// The export outlives the spec that asked for it, which is the part the first design missed: it
// knew how to clean up the ConfigMap the spec CURRENTLY names, and a spec that moved or dropped
// the field stranded the previous one. status.refExport is what makes the difference, so this
// walks the transitions in sequence rather than testing them one at a time -- the bug was in going
// from one state to the next, not in any single state.
func TestAnExportFollowsTheSpecThatAsksForIt(t *testing.T) {
	obj := &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: "base", Namespace: "team-a"},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(obj).
		WithStatusSubresource(obj).
		Build()

	r := &ImageCompositionReconciler{
		Client: c,
		Scheme: c.Scheme(),
		Export: recon.ExportOptions{Namespaces: []string{"flux-system"}},
	}
	art := &ociv1alpha1.ArtifactStatus{
		Digest: testExportDigest,
		Ref:    "registry.example/team-a/base@" + testExportDigest,
	}
	ctx := context.Background()
	const name = "imagecomposition-team-a-base"

	// Asked for: written, and recorded.
	obj.Spec.Push = &ociv1alpha1.Push{WriteRefTo: &ociv1alpha1.RefExport{
		Namespace: "team-a",
		Keys:      ociv1alpha1.RefExportKeys{Ref: "BASE_REF"},
	}}
	if err := r.exportRef(ctx, obj, art); err != nil {
		t.Fatalf("exporting: %v", err)
	}
	assertExports(t, c, "after the first export", name, "team-a")
	if got := currentRecord(t, c, obj); got == nil || got.Namespace != "team-a" {
		t.Fatalf("status.refExport = %v, want team-a/%s", got, name)
	}

	// Moved to another namespace: the old one goes, rather than being left for a consumer to keep
	// substituting from.
	refetch(t, c, obj)
	obj.Spec.Push.WriteRefTo.Namespace = "flux-system"
	if err := r.exportRef(ctx, obj, art); err != nil {
		t.Fatalf("moving the export: %v", err)
	}
	assertExports(t, c, "after moving namespace", name, "flux-system")

	// No longer asked for: removed entirely.
	refetch(t, c, obj)
	obj.Spec.Push.WriteRefTo = nil
	if err := r.exportRef(ctx, obj, art); err != nil {
		t.Fatalf("withdrawing the export: %v", err)
	}
	assertExports(t, c, "after removing writeRefTo", name)
	if got := currentRecord(t, c, obj); got != nil {
		t.Errorf("status still claims an export at %v", got)
	}
}

// TestDeletingAnObjectTakesItsForeignExportWithIt.
//
// An own-namespace export is owner-referenced and reclaimed by Kubernetes -- which the fake client
// does not simulate, so this covers the case that genuinely needs the controller to act.
func TestDeletingAnObjectTakesItsForeignExportWithIt(t *testing.T) {
	obj := &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "base", Namespace: "team-a", Finalizers: []string{ociv1alpha1.Finalizer},
		},
		Status: ociv1alpha1.ImageCompositionStatus{
			RefExport: &ociv1alpha1.RefExportStatus{
				Name: "imagecomposition-team-a-base", Namespace: "flux-system",
			},
		},
	}
	export := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "imagecomposition-team-a-base", Namespace: "flux-system",
		Labels: map[string]string{
			recon.ManagedByLabel:          "kube-oci-composer",
			"oci.lhns.de/owner-namespace": "team-a",
			"oci.lhns.de/owner-name":      "base",
		},
	}}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(obj, export).
		WithStatusSubresource(obj).
		Build()

	r := &ImageCompositionReconciler{Client: c, Scheme: c.Scheme()}
	if _, err := r.finalize(context.Background(), obj); err != nil {
		t.Fatalf("finalizing: %v", err)
	}
	assertExports(t, c, "after deleting the object", "imagecomposition-team-a-base")
}

const testExportDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// assertExports states the whole world: the ConfigMap named must exist in exactly the namespaces
// listed and nowhere else. Asserting only that the new one appeared is what let a stranded one go
// unnoticed.
func assertExports(t *testing.T, c client.Client, when, name string, namespaces ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, ns := range namespaces {
		want[ns] = true
	}
	for _, ns := range []string{"team-a", "flux-system"} {
		var cm corev1.ConfigMap
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &cm)
		switch {
		case err == nil && !want[ns]:
			t.Errorf("%s: %s/%s is still there; a consumer goes on substituting from a ConfigMap "+
				"nothing maintains", when, ns, name)
		case err != nil && want[ns]:
			t.Errorf("%s: %s/%s is missing: %v", when, ns, name, err)
		}
	}
}

func currentRecord(t *testing.T, c client.Client, obj *ociv1alpha1.ImageComposition) *ociv1alpha1.RefExportStatus {
	t.Helper()
	var latest ociv1alpha1.ImageComposition
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), &latest); err != nil {
		t.Fatal(err)
	}
	return latest.Status.RefExport
}

// refetch carries the status written by the previous step back onto the object under test, the way
// a fresh reconcile would read it.
func refetch(t *testing.T, c client.Client, obj *ociv1alpha1.ImageComposition) {
	t.Helper()
	var latest ociv1alpha1.ImageComposition
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), &latest); err != nil {
		t.Fatal(err)
	}
	obj.Status = latest.Status
	obj.ResourceVersion = latest.ResourceVersion
}
