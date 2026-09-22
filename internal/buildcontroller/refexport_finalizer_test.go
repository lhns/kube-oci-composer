package buildcontroller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestTheFinalizerTracksWhetherAnythingNeedsCleaningUp: only a foreign-namespace export needs a
// finalizer; an own-namespace export is owner-referenced, and a needless finalizer would make
// deletion depend on this controller running.
func TestTheFinalizerTracksWhetherAnythingNeedsCleaningUp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		want      bool
	}{
		{"a foreign export needs one", "flux-system", true},
		{"an own-namespace export does not", "team-a", false},
		{"no export at all does not", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := exportingBuild(nil)
			var exp *ociv1alpha1.RefExport
			if tc.namespace != "" {
				exp = &ociv1alpha1.RefExport{
					Namespace: tc.namespace,
					Keys:      ociv1alpha1.RefExportKeys{Ref: "REF"},
				}
				obj.Spec.Push.WriteRefTo = exp
			}
			r := exportReconciler(t, obj)

			if _, done, err := r.reconcileExportLifecycle(context.Background(), obj, exp); err != nil {
				t.Fatalf("lifecycle: %v", err)
			} else if done {
				t.Fatal("stopped the reconcile on a live object")
			}
			if got := controllerutil.ContainsFinalizer(obj, ociv1alpha1.Finalizer); got != tc.want {
				t.Errorf("finalizer = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTheFinalizerIsRemovedWhenTheForeignExportIsGone: otherwise uninstalling the operator strands
// every object that ever exported across namespaces.
func TestTheFinalizerIsRemovedWhenTheForeignExportIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		then *ociv1alpha1.RefExport
	}{
		{"moved into its own namespace", &ociv1alpha1.RefExport{
			Namespace: "team-a", Keys: ociv1alpha1.RefExportKeys{Ref: "REF"},
		}},
		{"withdrawn entirely", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := exportingBuild([]string{ociv1alpha1.Finalizer})
			obj.Status.RefExport = &ociv1alpha1.RefExportStatus{
				Name: "imagebuild-team-a-app", Namespace: "flux-system",
			}
			obj.Spec.Push.WriteRefTo = tc.then

			export := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "imagebuild-team-a-app", Namespace: "flux-system",
				Labels: map[string]string{
					recon.ManagedByLabel:          "kube-oci-composer",
					"oci.lhns.de/owner-namespace": "team-a",
					"oci.lhns.de/owner-name":      "app",
				},
			}}
			r := exportReconciler(t, obj, export)

			if _, _, err := r.reconcileExportLifecycle(context.Background(), obj, tc.then); err != nil {
				t.Fatalf("lifecycle: %v", err)
			}
			if controllerutil.ContainsFinalizer(obj, ociv1alpha1.Finalizer) {
				t.Error("the finalizer outlived the foreign export it existed for, so this object " +
					"can now only be deleted while this controller runs")
			}
		})
	}
}

// TestDeletionRemovesTheForeignExportThenTheFinalizer: in that order, or the object vanishes with
// its only record of what to clean up.
func TestDeletionRemovesTheForeignExportThenTheFinalizer(t *testing.T) {
	now := metav1.Now()
	obj := exportingBuild([]string{ociv1alpha1.Finalizer})
	obj.DeletionTimestamp = &now
	obj.Status.RefExport = &ociv1alpha1.RefExportStatus{
		Name: "imagebuild-team-a-app", Namespace: "flux-system",
	}
	export := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "imagebuild-team-a-app", Namespace: "flux-system",
		Labels: map[string]string{
			recon.ManagedByLabel:          "kube-oci-composer",
			"oci.lhns.de/owner-namespace": "team-a",
			"oci.lhns.de/owner-name":      "app",
		},
	}}
	r := exportReconciler(t, obj, export)

	_, done, err := r.reconcileExportLifecycle(context.Background(), obj, obj.Spec.Push.WriteRefTo)
	if err != nil {
		t.Fatalf("finalizing: %v", err)
	}
	if !done {
		t.Error("the reconcile continued past a deletion")
	}
	if controllerutil.ContainsFinalizer(obj, ociv1alpha1.Finalizer) {
		t.Error("the finalizer survived, so the object never finishes deleting")
	}
	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: "flux-system", Name: "imagebuild-team-a-app",
	}, &cm); err == nil {
		t.Error("the export outlived its object; nothing else will ever remove it")
	}
}

func exportingBuild(finalizers []string) *ociv1alpha1.ImageBuild {
	return &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app", Namespace: "team-a", Finalizers: finalizers,
		},
		Spec: ociv1alpha1.ImageBuildSpec{
			Push: &ociv1alpha1.Push{
				Repository: "ghcr.io/me/app",
				WriteRefTo: &ociv1alpha1.RefExport{
					Namespace: "flux-system",
					Keys:      ociv1alpha1.RefExportKeys{Ref: "REF"},
				},
			},
		},
	}
}

func exportReconciler(t *testing.T, obj *ociv1alpha1.ImageBuild, extra ...client.Object) *ImageBuildReconciler {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append([]client.Object{obj}, extra...)...).
		WithStatusSubresource(obj).
		Build()
	return &ImageBuildReconciler{
		Client: c,
		Export: recon.ExportOptions{Namespaces: []string{"flux-system"}},
	}
}
