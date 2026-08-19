package controller

import (
	"strings"
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// Keep is the value the old two-valued immutable could not express, and it is the one the pattern
// this project actually recommends needs: with a tag derived from a hash of the spec, a tag that
// already exists means the content is ALREADY PUBLISHED and correct. Fail stalls over a
// non-problem; Overwrite rewrites bytes that were already right.
//
// The dangerous part of Keep is that the object reports Ready while NOT having published what its
// spec produces. That is the shape of the incident behind ADR 0026 -- healthy status, diverged
// content -- so the divergence has to be recorded, and these tests are what hold that line.
func TestKeepLeavesTheExistingTagAndRecordsWhatWasDropped(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("kept", urlLayer("core", urlA, digestA, "/core"))
	obj.Spec.Publish = &ociv1alpha1.Publish{Name: "kept", Tags: []string{"v1"}}
	r, _ := servingReconciler(t, obj)

	first := build(t, r, obj, "first")

	// Same tag, different content -- and now the tag is to be kept rather than refused.
	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	obj.Spec.Publish.OnConflict = ociv1alpha1.ConflictKeep

	res, err := r.reconcileArtifact(t.Context(), obj)
	if err != nil {
		t.Fatalf("Keep must not fail: %v", err)
	}

	if res.Conflict == nil {
		t.Fatal("nothing recorded the divergence; status would describe content the spec no " +
			"longer produces while the object reads healthy")
	}
	if res.Conflict.Tag != "v1" {
		t.Errorf("conflict.tag = %q, want v1", res.Conflict.Tag)
	}
	if res.Conflict.Existing != first.Digest {
		t.Errorf("conflict.existing = %q, want the digest the tag actually holds (%s)",
			res.Conflict.Existing, first.Digest)
	}
	if res.Conflict.Dropped == "" || res.Conflict.Dropped == first.Digest {
		t.Errorf("conflict.dropped = %q, want the digest this spec produced and discarded",
			res.Conflict.Dropped)
	}
	if res.Conflict.ObservedAt == nil {
		t.Error("conflict.observedAt is unset; a divergence with no timestamp cannot be aged")
	}

	// status.artifact must describe what a consumer PULLS, which under Keep is the content that was
	// already there. Reporting the dropped digest would name bytes nobody can fetch.
	if res.Artifact.Digest != first.Digest {
		t.Errorf("artifact.digest = %q, want the existing %s: status must describe what the tag "+
			"actually serves", res.Artifact.Digest, first.Digest)
	}
}

// A resolved divergence must stop being reported, or the field becomes a permanent scar on an
// object that is now correct.
func TestAPublishThatDoesNotConflictClearsTheRecord(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("cleared", urlLayer("core", urlA, digestA, "/core"))
	obj.Spec.Publish = &ociv1alpha1.Publish{
		Name: "cleared", Tags: []string{"v1"}, OnConflict: ociv1alpha1.ConflictKeep,
	}
	r, _ := servingReconciler(t, obj)

	build(t, r, obj, "first")

	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	res, err := r.reconcileArtifact(t.Context(), obj)
	if err != nil {
		t.Fatalf("keeping: %v", err)
	}
	if res.Conflict == nil {
		t.Fatal("expected a divergence to be recorded first")
	}

	// Moving to a fresh tag removes the conflict entirely.
	obj.Spec.Publish.Tags = []string{"v2"}
	res, err = r.reconcileArtifact(t.Context(), obj)
	if err != nil {
		t.Fatalf("publishing under a fresh tag: %v", err)
	}
	if res.Conflict != nil {
		t.Errorf("conflict = %+v, want nil: the object now publishes exactly what its spec "+
			"produces, and a stale record would keep it looking diverged forever", res.Conflict)
	}
}

// Fail is the default and must survive the rename of the field that used to express it. The message
// is asserted because reasonFor classifies on it -- see reasonFor's own comment.
func TestFailIsTheDefaultWhenNothingIsSaid(t *testing.T) {
	urlA, digestA := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	urlB, digestB := contentServer(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("default", urlLayer("core", urlA, digestA, "/core"))
	// Neither onConflict nor immutable set: the safe answer must not depend on a schema default,
	// because onConflict deliberately has none.
	obj.Spec.Publish = &ociv1alpha1.Publish{Name: "default", Tags: []string{"v1"}}
	r, _ := servingReconciler(t, obj)

	build(t, r, obj, "first")

	obj.Spec.Layers[0] = urlLayer("core", urlB, digestB, "/core")
	_, err := r.reconcileArtifact(t.Context(), obj)
	if err == nil {
		t.Fatal("an unset policy allowed a tag to be remeaned; the default must be Fail")
	}
	if reasonFor(err) != ociv1alpha1.ReasonImmutableConflict {
		t.Fatalf("reason %q, want %q", reasonFor(err), ociv1alpha1.ReasonImmutableConflict)
	}
	if !strings.Contains(err.Error(), "onConflict: Overwrite") {
		t.Errorf("the message does not name the field that fixes it: %q", err)
	}
}
