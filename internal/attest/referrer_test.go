package attest

import (
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// pushArtifact puts a tiny image in a local registry and returns its repository and descriptor.
func pushArtifact(t *testing.T) (name.Repository, v1.Descriptor) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)

	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/team-a/app")
	if err != nil {
		t.Fatalf("parsing the repository: %v", err)
	}
	img := empty.Image
	if err := remote.Write(repo.Tag("latest"), img); err != nil {
		t.Fatalf("pushing the artifact: %v", err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	size, err := img.Size()
	if err != nil {
		t.Fatal(err)
	}
	mt, err := img.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	return repo, v1.Descriptor{MediaType: mt, Digest: digest, Size: size}
}

// TestAnAttestationIsDiscoverableAsAReferrer: push predicates, then find each by predicate type the
// way a consumer would. Existing must tell them apart, or Ensure would re-push them every reconcile.
func TestAnAttestationIsDiscoverableAsAReferrer(t *testing.T) {
	repo, subject := pushArtifact(t)

	sbom, err := Push(repo, subject, PredicateSPDX, []byte(`{"spdxVersion":"SPDX-2.3"}`), false, nil)
	if err != nil {
		t.Fatalf("pushing the SBOM: %v", err)
	}
	prov, err := Push(repo, subject, PredicateSLSA, []byte(`{"buildType":"test"}`), false, nil)
	if err != nil {
		t.Fatalf("pushing the provenance: %v", err)
	}

	found, err := Existing(repo, subject.Digest, nil)
	if err != nil {
		t.Fatalf("listing referrers: %v", err)
	}
	if got := found[PredicateSPDX]; got != sbom {
		t.Errorf("the SBOM is not discoverable by predicate type: got %v, want %v", got, sbom)
	}
	if got := found[PredicateSLSA]; got != prov {
		t.Errorf("the provenance is not discoverable by predicate type: got %v, want %v", got, prov)
	}
	if len(found) != 2 {
		t.Errorf("expected exactly two referrers, got %v", found)
	}
}

// TestPushingTheSamePredicateTwiceIsStable: re-pushing a deterministic payload yields the same
// manifest digest and no second referrer.
func TestPushingTheSamePredicateTwiceIsStable(t *testing.T) {
	repo, subject := pushArtifact(t)
	payload := []byte(`{"spdxVersion":"SPDX-2.3"}`)

	first, err := Push(repo, subject, PredicateSPDX, payload, false, nil)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	second, err := Push(repo, subject, PredicateSPDX, payload, false, nil)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if first != second {
		t.Fatalf("the same payload produced two manifests: %v and %v", first, second)
	}

	found, err := Existing(repo, subject.Digest, nil)
	if err != nil {
		t.Fatalf("listing referrers: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("re-pushing must not add a second referrer; got %v", found)
	}
}

// TestTheAttestationDoesNotTouchTheArtifact: the subject link lives in the referrer's manifest, so
// the artifact's bytes must not change.
func TestTheAttestationDoesNotTouchTheArtifact(t *testing.T) {
	repo, subject := pushArtifact(t)

	before, err := remote.Get(repo.Digest(subject.Digest.String()))
	if err != nil {
		t.Fatalf("reading the artifact: %v", err)
	}

	if _, err := Push(repo, subject, PredicateSLSA, []byte(`{"buildType":"test"}`), false, nil); err != nil {
		t.Fatalf("pushing: %v", err)
	}

	after, err := remote.Get(repo.Digest(subject.Digest.String()))
	if err != nil {
		t.Fatalf("re-reading the artifact: %v", err)
	}
	if after.Digest != before.Digest {
		t.Fatalf("attaching an attestation changed the artifact digest: %v -> %v", before.Digest, after.Digest)
	}
	// Bytes too, not just the digest.
	if string(after.Manifest) != string(before.Manifest) {
		t.Fatal("the artifact's manifest bytes changed")
	}
}

// An attestation is tagged after its own digest: without keepUntagged, untagged content is
// reclaimed by age regardless of pulls (ADR 0060).
func TestAnAttestationCarriesItsOwnName(t *testing.T) {
	repo, subject := pushArtifact(t)

	sbom, err := Push(repo, subject, PredicateSPDX, []byte(`{"spdxVersion":"SPDX-2.3"}`), false, nil)
	if err != nil {
		t.Fatalf("pushing the SBOM: %v", err)
	}

	desc, err := remote.Head(repo.Tag(OwnTag(sbom)))
	if err != nil {
		t.Fatalf("the attestation has no tag of its own, so nothing protects it but pull recency "+
			"on untagged content: %v", err)
	}
	if desc.Digest != sbom {
		t.Fatalf("%s resolves to %s, want the attestation %s", OwnTag(sbom), desc.Digest, sbom)
	}
}
