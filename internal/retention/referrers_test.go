package retention

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/attest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// pathRecorder notes every GET path, so a test can prove a referrer was actually pulled rather
// than merely that nothing errored.
type pathRecorder struct {
	inner http.RoundTripper
	mu    sync.Mutex
	gets  []string
}

func (p *pathRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		p.mu.Lock()
		p.gets = append(p.gets, req.URL.Path)
		p.mu.Unlock()
	}
	return p.inner.RoundTrip(req)
}

// artifactWithAttestation pushes an image and attaches one attestation to it, using the same code
// path the controllers use.
func artifactWithAttestation(t *testing.T) (name.Repository, v1.Hash, v1.Hash) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)

	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/team-a/app")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Tag("v1"), empty.Image); err != nil {
		t.Fatalf("pushing the artifact: %v", err)
	}
	digest, err := empty.Image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	size, err := empty.Image.Size()
	if err != nil {
		t.Fatal(err)
	}
	mt, err := empty.Image.MediaType()
	if err != nil {
		t.Fatal(err)
	}

	attestation, err := attest.Push(repo,
		v1.Descriptor{MediaType: mt, Digest: digest, Size: size},
		attest.PredicateSPDX, []byte(`{"spdxVersion":"SPDX-2.3"}`), false, nil)
	if err != nil {
		t.Fatalf("attaching the attestation: %v", err)
	}
	return repo, digest, attestation
}

// TestReferrersAreRefreshedToo guards a failure that would arrive a retention window after anyone
// enabled attestations, silently, in the deleting direction.
//
// The registry policy this project ships uses `deleteUntagged` with `keepUntagged.pulledWithin`,
// and a referrer manifest is UNTAGGED. So an SBOM or a provenance statement stays alive only if
// something pulls it — and the refresher pulled the artifact's digest and its tags, which is not
// the same thing. That is threat D6 on a new object type.
//
// Signatures need nothing here: cosign's `.sig` is a tag, so `keepTags` already covers it. That is
// a small, real argument for the tag convention chosen in internal/attest/sign.go.
func TestReferrersAreRefreshedToo(t *testing.T) {
	repo, digest, attestation := artifactWithAttestation(t)

	rec := &pathRecorder{inner: remote.DefaultTransport}
	out := &Result{}
	(&Refresher{}).refreshReferrers(repo.Digest(digest.String()),
		[]remote.Option{remote.WithTransport(rec)}, out)

	if out.Refreshed == 0 {
		t.Fatal("no referrer was refreshed; an untagged attestation would expire while its image lived on")
	}

	var pulled bool
	for _, p := range rec.gets {
		if strings.Contains(p, attestation.String()) {
			pulled = true
		}
	}
	if !pulled {
		t.Fatalf("the attestation manifest %s was never fetched; GETs were %v", attestation, rec.gets)
	}
}

// TestRefreshingReferrersIsHarmlessWithoutAny — the ordinary case, and the one that must not turn a
// working retention refresh into a reported failure on a registry with no Referrers API at all.
func TestRefreshingReferrersIsHarmlessWithoutAny(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()

	repo, err := name.NewRepository(strings.TrimPrefix(srv.URL, "http://") + "/team-a/app")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Tag("v1"), empty.Image); err != nil {
		t.Fatal(err)
	}
	digest, err := empty.Image.Digest()
	if err != nil {
		t.Fatal(err)
	}

	out := &Result{}
	(&Refresher{}).refreshReferrers(repo.Digest(digest.String()), nil, out)

	if out.Failed != 0 {
		t.Errorf("an artifact with no referrers must not be counted as a failure: %+v", out)
	}
}

// TestOnlyDigestsHaveReferrers — a tag reference is refreshed as a tag, and asking a registry for
// the referrers of a tag is not a question the API answers.
func TestOnlyDigestsHaveReferrers(t *testing.T) {
	repo, _, _ := artifactWithAttestation(t)

	out := &Result{}
	(&Refresher{}).refreshReferrers(repo.Tag("v1"), nil, out)

	if out.Refreshed != 0 || out.Failed != 0 {
		t.Errorf("a tag reference should be skipped, not attempted: %+v", out)
	}
}

// TestTheRefreshLoopActuallyRefreshesReferrers is the call-site guard, and it exists because
// removing the call from refreshObject left every other test in this file passing.
//
// The helper tests above prove refreshReferrers works. This proves the refresher USES it, which is
// a different claim and the one that matters: an attestation nothing pulls is an attestation the
// registry reclaims a window later, silently, while the image it describes lives on.
func TestTheRefreshLoopActuallyRefreshesReferrers(t *testing.T) {
	repo, digest, attestation := artifactWithAttestation(t)

	// Built directly rather than with buildWith, which takes the recording registry this test does
	// not use: the real attest.Push needs a registry that speaks the Referrers API.
	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
	}
	obj.Spec.Push = &ociv1alpha1.Push{Repository: repo.Name()}
	obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: digest.String()}
	obj.Generation = 1
	obj.Status.ObservedGeneration = 1

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(obj).Build()
	rec := &pathRecorder{inner: remote.DefaultTransport}
	r := &Refresher{
		Client:             c,
		Source:             sourceFor(obj, c),
		Pending:            allReconciled{},
		Recorder:           record.NewFakeRecorder(50),
		InsecureRegistries: []string{repo.RegistryStr()},
		Transport:          rec,
	}

	if _, err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}

	for _, p := range rec.gets {
		if strings.Contains(p, attestation.String()) {
			return
		}
	}
	t.Fatalf("the refresh loop never pulled the attestation %s.\n"+
		"It will be reclaimed one retention window from now, while the image it describes stays alive.\n"+
		"GETs were: %v", attestation, rec.gets)
}
