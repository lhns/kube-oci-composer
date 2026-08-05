package controller

import (
	"slices"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// TestTagFromRef covers the parsing, where every interesting case is an edge case.
func TestTagFromRef(t *testing.T) {
	for _, tc := range []struct {
		name, ref, want string
		wantErr         bool
	}{
		{name: "empty", ref: "", want: ""},

		// The reason this is hand-parsed. An untemplated placeholder must contribute NOTHING;
		// name.ParseReference would turn it into index.docker.io/library/x:latest and we would
		// publish a moving tag nobody asked for.
		{name: "bare placeholder has no tag", ref: "freshrss-extensions", want: ""},
		{name: "repo path but no tag", ref: "oci-composer.internal/freshrss-extensions", want: ""},

		{name: "full reference", ref: "oci-composer.internal/freshrss-extensions:s1a2b3c4d", want: "s1a2b3c4d"},
		{name: "host and repo are ignored", ref: "some.other.host/a/b/c:v1.2.3", want: "v1.2.3"},

		// A colon that belongs to a port is not a tag.
		{name: "port without tag", ref: "registry:5000/repo", want: ""},
		{name: "port with tag", ref: "registry:5000/repo:tag", want: "tag"},

		// A digest is an output, not something to publish under.
		{name: "digest form rejected", ref: "repo@sha256:" + zeroDigest, wantErr: true},
		{name: "tag and digest rejected", ref: "repo:v1@sha256:" + zeroDigest, wantErr: true},

		{name: "invalid tag rejected", ref: "repo:-nope", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tagFromRef(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("tagFromRef(%q) = %q, want an error", tc.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("tagFromRef(%q): %v", tc.ref, err)
			}
			if got != tc.want {
				t.Fatalf("tagFromRef(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}
}

const zeroDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// TestEffectiveTags — ref contributes alongside tags, in order, without duplicates.
func TestEffectiveTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		ref  string
		want []string
	}{
		{name: "tags only", tags: []string{"main"}, want: []string{"main"}},
		{name: "ref only", ref: "host/repo:sABC", want: []string{"sABC"}},
		{name: "both", tags: []string{"main"}, ref: "host/repo:sABC", want: []string{"main", "sABC"}},
		{name: "deduplicated", tags: []string{"sABC"}, ref: "host/repo:sABC", want: []string{"sABC"}},
		{name: "untemplated ref contributes nothing", ref: "repo", want: []string{}},
		{name: "nothing at all", want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := effectiveTags(tc.tags, tc.ref)
			if err != nil {
				t.Fatalf("effectiveTags: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("effectiveTags(%v, %q) = %v, want %v", tc.tags, tc.ref, got, tc.want)
			}
		})
	}
}

// TestPublishRefDrivesTheTag is the point of the field: a reference rewritten by something like
// kustomize's images transformer publishes under that tag, without publish.tags being touched.
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
