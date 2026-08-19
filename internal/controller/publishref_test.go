package controller

import (
	"slices"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

func TestPublishRefDrivesTheTag(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("viaref", urlLayer("core", url, digest, "/core"))
	obj.Spec.Publish = &ociv1alpha1.Publish{
		Name: "viaref",
		// Exactly what the transformer produces: full reference, host and repo included.
		Ref: "oci-composer.internal/viaref:s0123456789abcdef",
	}
	r, host := servingReconciler(t, obj)

	art := build(t, r, obj, "publish via ref")
	if want := []string{"oci.test/viaref:s0123456789abcdef"}; !slices.Equal(art.Tags, want) {
		t.Fatalf("tags %v, want %v", art.Tags, want)
	}

	ref, err := name.ParseReference(host+"/viaref:s0123456789abcdef", name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatalf("the tag from publish.ref does not resolve: %v", err)
	}
	if desc.Digest.String() != art.Digest {
		t.Fatalf("resolves to %s, want %s", desc.Digest, art.Digest)
	}
}

// TestUntemplatedRefPublishesByDigest — the manifest as written, before anything rewrites it.
// It must not invent a tag; publishing by digest alone is correct and the missing rewrite shows
// up at the consumer instead.
func TestUntemplatedRefPublishesByDigest(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("untemplated", urlLayer("core", url, digest, "/core"))
	obj.Spec.Publish = &ociv1alpha1.Publish{Name: "untemplated", Ref: "untemplated"}
	r, host := servingReconciler(t, obj)

	art := build(t, r, obj, "untemplated ref")
	if len(art.Tags) != 0 {
		t.Fatalf("tags %v, want none — a bare ref must not become :latest", art.Tags)
	}

	ref, err := name.ParseReference(host+"/untemplated@"+art.Digest, name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := remote.Head(ref); err != nil {
		t.Fatalf("not pullable by digest: %v", err)
	}
}
