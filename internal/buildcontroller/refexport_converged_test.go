package buildcontroller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// Turning on push.writeRefTo must work on an object that has already built.
//
// It did not, and nothing noticed. exportRef was reached only from the build-succeeded branch, and
// writeRefTo is not part of the input hash -- adding it changes nothing about what to build -- so a
// Ready object took the cheap path and returned before ever reaching the export. The ConfigMap a
// consuming Kustomization substitutes from was simply never written, until something unrelated
// forced a rebuild.
//
// That is the ordinary way to adopt the feature: you have a working build, and you want its digest
// somewhere a consumer can read. So the common case was the broken one, and it was invisible --
// every existing test either built first and exported in the same pass, or tested the export
// helper directly.
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

	// The premise: this object is still on the cheap path. If the hash moved, the export would
	// have been written by the publish branch and this test would prove nothing.
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

// Moving the export is the same problem one step on: the spec changes, the input hash does not.
//
// Without the cheap-path call the old ConfigMap stays where it was and the new one never appears,
// so a consumer goes on substituting from a reference nothing maintains -- which is the failure
// the lifecycle in ADR 0056 exists to prevent.
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

// An export must not cost the object its own status.
//
// recordExport used to take its own status patch mid-reconcile. controller-runtime writes the
// server's response back into the object, and at that moment the server still held the status from
// BEFORE this reconcile -- so Artifact and InputHash, set in memory by recordSuccess and not yet
// persisted, were silently replaced with nothing.
//
// The object then never converged. Every pass rebuilt, produced a different digest (this kind is
// not reproducible), and rewrote the very ConfigMap the export exists to publish -- so anything
// consuming it rolled continuously, for as long as the object existed.
//
// Nothing caught it because every test either set no export, or asserted the ConfigMap rather than
// the status. The export was written correctly the whole time.
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
