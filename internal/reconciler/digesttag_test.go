package reconciler

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/lhns/kube-oci-composer/internal/attest"
)

const aDigest = "sha256:7e35cc903ff8ef186dfff1db5a96f1f06bd0fa3553ff11a4978fad3d8c09fde3"

// The derived tag has to be a tag the registry and the CRD would both accept, or every publish
// fails at the last step.
func TestTheDigestsOwnTagIsAValidTag(t *testing.T) {
	got := DigestTag(aDigest)
	if want := "digest-7e35cc903ff8ef186dfff1db5a96f1f06bd0fa3553ff11a4978fad3d8c09fde3"; got != want {
		t.Fatalf("DigestTag = %q, want %q", got, want)
	}
	if !tagPattern.MatchString(got) {
		t.Errorf("%q does not match the CRD's tag pattern %s", got, tagPattern)
	}
	// The OCI distribution spec's own limit.
	if len(got) > 128 {
		t.Errorf("%q is %d characters; a tag is at most 128", got, len(got))
	}
	// The whole digest, never a prefix: two builds sharing a prefix would share a tag, and a
	// shared tag MOVES -- which is the failure this exists to prevent.
	if !regexp.MustCompile(`^digest-[0-9a-f]{64}$`).MatchString(got) {
		t.Errorf("%q is not the full digest", got)
	}
}

// The name must not be one a registry reserves. "sha256-<hex>" was, and it made every artifact
// immortal: it is the OCI referrers tag schema, and zot matches it with the unanchored regex below
// (pkg/common/common.go, IsReferrersTag, v2.1.21), never records such a tag in its metadata, and so
// never lets retention evaluate it. cosign's conventions are reserved the same way in practice.
func TestTheDigestsOwnTagIsNotAReservedName(t *testing.T) {
	got := DigestTag(aDigest)
	for _, reserved := range []struct{ what, pattern string }{
		{"zot's referrers tag (OCI referrers tag schema)", `sha256\-[A-Za-z0-9]*$`},
		{"cosign's signature, attestation and SBOM tags", `^sha256-[0-9a-f]+\.(sig|att|sbom)$`},
	} {
		if regexp.MustCompile(reserved.pattern).MatchString(got) {
			t.Errorf("%q matches %s (%s); a registry treats it as that rather than as a tag, "+
				"and zot keeps such a tag forever", got, reserved.what, reserved.pattern)
		}
	}
}

func TestPublishTagsAppendsTheDigestsOwnTagLast(t *testing.T) {
	own := DigestTag(aDigest)
	for _, tc := range []struct {
		name string
		tags []string
		want []string
	}{
		// Last, so tags[0] -- what revision and ref are built from -- does not change.
		{name: "after the spec's tags", tags: []string{"main", "v1"}, want: []string{"main", "v1", own}},
		// Digest-only publications are named too; they are what a collector reclaims by age.
		{name: "digest only", tags: nil, want: []string{own}},
		// A spec that already asks for it gets it once.
		{name: "not duplicated", tags: []string{own, "main"}, want: []string{"main", own}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PublishTags(tc.tags, aDigest); !slices.Equal(got, tc.want) {
				t.Fatalf("PublishTags(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

func TestHasDigestTagReadsEitherShapeStatusStores(t *testing.T) {
	own := DigestTag(aDigest)
	for _, tc := range []struct {
		name string
		tags []string
		want bool
	}{
		{name: "qualified, as status.artifact stores it", tags: []string{"host/ns/app:main", "host/ns/app:" + own}, want: true},
		{name: "bare, as a composition's history stores it", tags: []string{"main", own}, want: true},
		{name: "absent: published before the tag existed", tags: []string{"host/ns/app:main"}, want: false},
		{name: "another digest's tag is not this one's", tags: []string{"host/ns/app:digest-" + strings.Repeat("0", 64)}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasDigestTag(tc.tags, aDigest); got != tc.want {
				t.Fatalf("HasDigestTag(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

// attest repeats the transformation rather than importing this package. Two copies of a naming rule
// drift apart quietly, and a drifted copy names attestations something the refresher, the chart and
// the docs do not know about.
func TestAttestationsAreNamedTheSameWay(t *testing.T) {
	h, err := v1.NewHash(aDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := attest.OwnTag(h), DigestTag(aDigest); got != want {
		t.Fatalf("attest.OwnTag = %q, DigestTag = %q", got, want)
	}
}
