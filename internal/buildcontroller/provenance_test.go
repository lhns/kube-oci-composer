package buildcontroller

import (
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/build"
)

// status.history[].sources is in the CRD for both kinds, and until now only the composer ever wrote
// it. A build's record therefore carried a digest and no way to learn which revision produced it —
// exactly the question ADR 0026's incident was stuck on, where a layer's content and its apparent
// version disagreed and nothing in status could adjudicate.
//
// This asserts through recordSuccess rather than through the type, because a field that exists and
// is never assigned is precisely the failure being fixed.
func TestABuildRecordsWhereItsContentCameFrom(t *testing.T) {
	obj := &ociv1alpha1.ImageBuild{}
	obj.Spec.Context = ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "app-src"}
	obj.Spec.Push = &ociv1alpha1.Push{
		Repository: "ghcr.io/example/app",
		Tags:       []string{"v1"},
	}

	inputs := build.Inputs{
		ContextDigest:   "sha256:aaaa",
		ContextRevision: "v0.6.8@sha1:b739efb5",
	}

	r := &ImageBuildReconciler{}
	r.recordSuccess(obj, inputs, "hash-1", "sha256:bbbb")

	if len(obj.Status.History) == 0 {
		t.Fatal("recordSuccess wrote no history")
	}
	got := obj.Status.History[0].Sources
	if len(got) != 1 {
		t.Fatalf("sources = %+v, want exactly the build context", got)
	}
	want := ociv1alpha1.SourceRecord{
		Name:     "app-src",
		Revision: "v0.6.8@sha1:b739efb5",
		Digest:   "sha256:aaaa",
	}
	if got[0] != want {
		t.Errorf("sources[0] = %+v, want %+v", got[0], want)
	}
}

// The revision is deliberately NOT part of the input hash: the digest already identifies the
// content, so hashing both would rebuild on a repack that changed nothing. If this ever starts
// failing, provenance has been wired into convergence and every source-controller repack becomes a
// rebuild.
func TestTheRecordedRevisionDoesNotDriveRebuilds(t *testing.T) {
	base := build.Inputs{ContextDigest: "sha256:aaaa", ContextRevision: "v1@sha1:1111"}
	moved := base
	moved.ContextRevision = "v2@sha1:2222"

	if base.Hash() != moved.Hash() {
		t.Error("a changed revision over identical content changed the input hash; " +
			"provenance is not supposed to be an input")
	}
}
