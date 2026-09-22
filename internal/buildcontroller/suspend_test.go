package buildcontroller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestSuspendingStillAdvancesObservedGeneration: suspending bumps the generation, and an object
// left "not yet reconciled" makes the retention refresher skip its whole cycle for every object.
func TestSuspendingStillAdvancesObservedGeneration(t *testing.T) {
	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app", Namespace: "team-a", Generation: 7,
		},
		Spec: ociv1alpha1.ImageBuildSpec{
			Suspend: true,
			Push:    &ociv1alpha1.Push{Repository: "ghcr.io/me/app"},
		},
		Status: ociv1alpha1.ImageBuildStatus{ObservedGeneration: 6},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(obj).
		WithStatusSubresource(obj).
		Build()
	r := &ImageBuildReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	}); err != nil {
		t.Fatalf("reconciling a suspended object: %v", err)
	}

	var got ociv1alpha1.ImageBuild
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != 7 {
		t.Errorf("observedGeneration = %d, want 7. Left behind, this object reads as pending "+
			"forever and the refresher skips every cycle -- for every object, not just this one.",
			got.Status.ObservedGeneration)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, ociv1alpha1.ReadyCondition); ready == nil ||
		ready.Reason != ociv1alpha1.ReasonSuspended {
		t.Errorf("a suspended object must still say so: %+v", got.Status.Conditions)
	}
}

// TestASuspendedObjectStillFinishesDeleting: the finalizer is removed only past the suspend branch,
// so suspend must not short-circuit deletion.
func TestASuspendedObjectStillFinishesDeleting(t *testing.T) {
	now := metav1.Now()
	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app", Namespace: "team-a", Generation: 3,
			Finalizers:        []string{ociv1alpha1.Finalizer},
			DeletionTimestamp: &now,
		},
		Spec: ociv1alpha1.ImageBuildSpec{
			Suspend: true,
			Push: &ociv1alpha1.Push{
				Repository: "ghcr.io/me/app",
				WriteRefTo: &ociv1alpha1.RefExport{
					Namespace: "flux-system",
					Keys:      ociv1alpha1.RefExportKeys{Ref: "REF"},
				},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(obj).
		WithStatusSubresource(obj).
		Build()
	r := &ImageBuildReconciler{
		Client: c,
		Export: recon.ExportOptions{Namespaces: []string{"flux-system"}},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	}); err != nil {
		t.Fatalf("reconciling a suspended object being deleted: %v", err)
	}

	var got ociv1alpha1.ImageBuild
	err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), &got)
	if err == nil && controllerutil.ContainsFinalizer(&got, ociv1alpha1.Finalizer) {
		t.Error("the finalizer survived on a suspended object, so it can never finish deleting: " +
			"the suspend branch returned before anything looked at DeletionTimestamp")
	}
}
