package buildcontroller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// A running build is in flight, not Ready: anything waiting on the object (Flux wait: true,
// kubectl wait, kstatus) would otherwise proceed with the previous image (ADR 0061).

func wantProgressing(t *testing.T, got *ociv1alpha1.ImageBuild, stillPublished string) {
	t.Helper()
	ready := conditionOf(got, ociv1alpha1.ReadyCondition)
	if ready == nil || ready.Status != metav1.ConditionUnknown || ready.Reason != ociv1alpha1.ReasonProgressing {
		t.Fatalf("Ready = %+v, want Unknown/Progressing while the build runs", ready)
	}
	if c := conditionOf(got, ociv1alpha1.ReconcilingCondition); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Reconciling = %+v, want True: kstatus reads it as in progress", c)
	}
	if stillPublished != "" && !strings.Contains(ready.Message, stillPublished) {
		t.Errorf("Ready message %q does not say %s is still what is published", ready.Message, stillPublished)
	}
	// Advanced regardless: the retention refresher skips its cycle while any object lags.
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}

func wantReady(t *testing.T, got *ociv1alpha1.ImageBuild) {
	t.Helper()
	if c := conditionOf(got, ociv1alpha1.ReadyCondition); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want True", c)
	}
	if c := conditionOf(got, ociv1alpha1.ReconcilingCondition); c != nil {
		t.Errorf("Reconciling = %+v survived the finished build", c)
	}
}

// TestARebuildIsNotReadyForThePreviousImage is the reported case: a spec change starts a new build,
// and until it finishes the object must not report Ready for the image it replaces.
func TestARebuildIsNotReadyForThePreviousImage(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	wantProgressing(t, reload(t, r, obj), "")

	succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after the first build: %v", err)
	}
	first := reload(t, r, obj)
	wantReady(t, first)
	for _, j := range jobsIn(t, r, obj.Namespace) {
		if err := r.Delete(context.Background(), &j); err != nil {
			t.Fatalf("deleting job: %v", err)
		}
	}

	first.Spec.Platforms = []string{"linux/arm64"}
	first.Spec.Push.Tags = []string{"v2"}
	mustUpdate(t, r, first)
	for i := range 2 { // the pass that starts the Job, and one that finds it still running
		if _, err := reconcileOnce(t, r, obj); err != nil {
			t.Fatalf("reconcile %d of the rebuild: %v", i, err)
		}
		wantProgressing(t, reload(t, r, obj), first.Status.Artifact.Ref)
	}

	digest := succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after the rebuild: %v", err)
	}
	got := reload(t, r, obj)
	wantReady(t, got)
	if got.Status.Artifact.Digest != digest {
		t.Errorf("artifact = %s, want the rebuild's %s", got.Status.Artifact.Digest, digest)
	}
}

// TestAnAdoptedBuildIsInFlight: a running Job status does not record (a patch lost after Create, or
// a restart) is still a build in flight.
func TestAnAdoptedBuildIsInFlight(t *testing.T) {
	obj := buildOf(t, nil)
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	lost := reload(t, r, obj)
	lost.Status.BuildRef = nil
	lost.Status.Conditions = nil
	if err := r.Status().Update(context.Background(), lost); err != nil {
		t.Fatalf("dropping buildRef: %v", err)
	}

	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := reload(t, r, obj)
	wantProgressing(t, got, "")
	if got.Status.BuildRef == nil {
		t.Error("status.buildRef does not name the adopted Job")
	}
}

// TestKeepAfterABuildEndsInFlight: onConflict: Keep that only finds the tag taken once the Job has
// run leaves nothing in flight, so the object is Ready rather than Progressing forever.
func TestKeepAfterABuildEndsInFlight(t *testing.T) {
	obj := buildOf(t, func(b *ociv1alpha1.ImageBuild) {
		b.Spec.Push.OnConflict = ociv1alpha1.ConflictKeep
	})
	r := harness(t, pinnedFrom, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Someone else takes the tag while the build runs.
	_, other := pushByDigest(t, obj.Spec.Push.Repository)
	tagAs(t, obj.Spec.Push.Repository, "v1", other)

	succeedJob(t, r, obj)
	if _, err := reconcileOnce(t, r, obj); err != nil {
		t.Fatalf("reconcile after the build: %v", err)
	}
	got := reload(t, r, obj)
	wantReady(t, got)
	if got.Status.Conflict == nil || got.Status.BuildRef != nil {
		t.Errorf("conflict = %+v, buildRef = %+v; want the kept tag recorded and nothing in flight",
			got.Status.Conflict, got.Status.BuildRef)
	}
}
