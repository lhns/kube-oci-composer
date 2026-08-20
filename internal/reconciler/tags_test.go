package reconciler

import (
	"slices"
	"testing"
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
		{name: "bare placeholder has no tag", ref: "plugin-bundle", want: ""},
		{name: "repo path but no tag", ref: "oci-composer.internal/plugin-bundle", want: ""},

		{name: "full reference", ref: "oci-composer.internal/plugin-bundle:s1a2b3c4d", want: "s1a2b3c4d"},
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
			got, err := TagFromRef(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("TagFromRef(%q) = %q, want an error", tc.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("TagFromRef(%q): %v", tc.ref, err)
			}
			if got != tc.want {
				t.Fatalf("TagFromRef(%q) = %q, want %q", tc.ref, got, tc.want)
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
			got, err := EffectiveTags(tc.tags, tc.ref)
			if err != nil {
				t.Fatalf("EffectiveTags: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("EffectiveTags(%v, %q) = %v, want %v", tc.tags, tc.ref, got, tc.want)
			}
		})
	}
}

// TestPublishRefDrivesTheTag is the point of the field: a reference rewritten by something like
// kustomize's images transformer publishes under that tag, without publish.tags being touched.
