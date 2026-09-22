package buildcontroller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestEnablingTheExportOnAConvergedObjectWritesIt: writeRefTo is not in the input hash, so adding
// it to an already-built object must still write the ConfigMap on the converged path.
func TestEnablingTheExportOnAConvergedObjectWritesIt(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	r.Export = recon.ExportOptions{Namespaces: []string{"flux-system"}}

	// Build it, with no export configured.
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after success: %v", err)
	}

	converged := reload(t, r, obj)
	if converged.Status.Artifact == nil || converged.Status.InputHash == "" {
		t.Fatal("the object did not converge, so this test cannot say anything about the cheap path")
	}
	before := converged.Status.InputHash

	// Now ask for the export, changing nothing else.
	converged.Spec.Push.WriteRefTo = &ociv1alpha1.RefExport{
		Namespace: "flux-system",
		Keys:      ociv1alpha1.RefExportKeys{Ref: "APP_REF", Digest: "APP_DIGEST"},
	}
	converged.Generation++
	if err := r.Update(context.Background(), converged); err != nil {
		t.Fatalf("enabling the export: %v", err)
	}
	if _, err := reconcileOnce(t, r, converged); err != nil {
		t.Fatalf("reconcile after enabling the export: %v", err)
	}

	// Premise: still on the converged path, or the publish branch wrote the export.
	after := reload(t, r, obj)
	if after.Status.InputHash != before {
		t.Fatalf("adding writeRefTo moved the input hash (%s -> %s); the cheap path was not "+
			"exercised and this test no longer covers what it was written for",
			before, after.Status.InputHash)
	}

	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: "flux-system", Name: "imagebuild-" + obj.Namespace + "-" + obj.Name}
	if err := r.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("the export was never written for a converged object: %v\n"+
			"push.writeRefTo is not in the input hash, so nothing will force a rebuild and the "+
			"ConfigMap a consumer substitutes from never appears", err)
	}
	if cm.Data["APP_REF"] == "" || cm.Data["APP_DIGEST"] != after.Status.Artifact.Digest {
		t.Errorf("the export does not describe what status reports: %v vs %s",
			cm.Data, after.Status.Artifact.Digest)
	}
	if rec := after.Status.RefExport; rec == nil || rec.Namespace != "flux-system" {
		t.Errorf("status.refExport = %v; without it nothing can clean the ConfigMap up later", rec)
	}
}

// TestMovingTheExportOnAConvergedObjectFollowsIt: moving writeRefTo leaves the hash alone, yet the
// ConfigMap must move with it (ADR 0056).
func TestMovingTheExportOnAConvergedObjectFollowsIt(t *testing.T) {
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.WriteRefTo = &ociv1alpha1.RefExport{
			Namespace: "flux-system",
			Keys:      ociv1alpha1.RefExportKeys{Ref: "APP_REF"},
		}
	})
	r := harness(t, pinnedFrom, obj)
	r.Export = recon.ExportOptions{Namespaces: []string{"flux-system", "other-ns"}}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after success: %v", err)
	}

	name := "imagebuild-" + obj.Namespace + "-" + obj.Name
	assertExportIn(t, r, "flux-system", name, true)

	moved := reload(t, r, obj)
	moved.Spec.Push.WriteRefTo.Namespace = "other-ns"
	moved.Generation++
	if err := r.Update(context.Background(), moved); err != nil {
		t.Fatalf("moving the export: %v", err)
	}
	if _, err := reconcileOnce(t, r, moved); err != nil {
		t.Fatalf("reconcile after moving the export: %v", err)
	}

	after := reload(t, r, obj)
	if after.Status.InputHash != moved.Status.InputHash || after.Status.Artifact == nil {
		t.Fatalf("the object left the converged path (hash %q -> %q, artifact %v); this test "+
			"no longer covers what it was written for",
			moved.Status.InputHash, after.Status.InputHash, after.Status.Artifact != nil)
	}

	assertExportIn(t, r, "other-ns", name, true)
	assertExportIn(t, r, "flux-system", name, false)
}

func assertExportIn(t *testing.T, r *ImageBuildReconciler, namespace, name string, want bool) {
	t.Helper()
	var cm corev1.ConfigMap
	err := r.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &cm)
	switch {
	case want && err != nil:
		t.Errorf("expected an export in %s: %v", namespace, err)
	case !want && err == nil:
		t.Errorf("the export in %s survived the move; a consumer goes on substituting from a "+
			"reference nothing maintains", namespace)
	}
}

// TestExportingDoesNotCostTheObjectItsStatus: recordExport must not patch status mid-reconcile,
// which would drop the unpersisted Artifact and InputHash so the object never converges and
// rewrites the export every pass.
func TestExportingDoesNotCostTheObjectItsStatus(t *testing.T) {
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.WriteRefTo = &ociv1alpha1.RefExport{
			Namespace: "flux-system",
			Keys:      ociv1alpha1.RefExportKeys{Ref: "APP_REF"},
		}
	})
	r := harness(t, pinnedFrom, obj)
	r.Export = recon.ExportOptions{Namespaces: []string{"flux-system"}}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after success: %v", err)
	}

	got := reload(t, r, obj)
	if got.Status.Artifact == nil || got.Status.InputHash == "" {
		t.Fatalf("a successful build that also exported recorded neither artifact nor input hash "+
			"(artifact=%v hash=%q). The object cannot converge: it rebuilds every pass, and each "+
			"rebuild moves the digest and rewrites the export a consumer is reading.",
			got.Status.Artifact != nil, got.Status.InputHash)
	}
	if got.Status.RefExport == nil {
		t.Error("the export was not recorded, so nothing can clean it up later")
	}
}
