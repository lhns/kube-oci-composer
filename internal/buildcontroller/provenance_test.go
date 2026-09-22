package buildcontroller

import (
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/build"
)

// TestABuildRecordsWhereItsContentCameFrom pins that recordSuccess fills status.history[].sources,
// so a build's digest can be traced to the revision that produced it (ADR 0026).
func TestABuildRecordsWhereItsContentCameFrom(t *testing.T) {
	obj := &ociv1alpha1.ImageBuild{}
	obj.Spec.Context = &ociv1alpha1.BuildContext{
		SourceRef: &ociv1alpha1.SourceRefSource{Kind: "GitRepository", Name: "app-src"},
	}
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

// TestTheRecordedRevisionDoesNotDriveRebuilds: the revision is provenance, not an input. The
// digest identifies the content, so a source-controller repack must not rebuild.
func TestTheRecordedRevisionDoesNotDriveRebuilds(t *testing.T) {
	base := build.Inputs{ContextDigest: "sha256:aaaa", ContextRevision: "v1@sha1:1111"}
	moved := base
	moved.ContextRevision = "v2@sha1:2222"

	if base.Hash() != moved.Hash() {
		t.Error("a changed revision over identical content changed the input hash; " +
			"provenance is not supposed to be an input")
	}
}
