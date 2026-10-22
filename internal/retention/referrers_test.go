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
)

// pathRecorder notes every GET path, so a test can prove a referrer was actually pulled.
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

// artifact pushes an empty image to a throwaway registry and returns where it landed.
func artifact(t *testing.T) (name.Repository, v1.Descriptor) {
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
	return repo, v1.Descriptor{MediaType: mt, Digest: digest, Size: size}
}

// artifactWithAttestation is artifact plus one attestation attached the way the controllers do it.
func artifactWithAttestation(t *testing.T) (name.Repository, v1.Hash, v1.Hash) {
	t.Helper()
	repo, desc := artifact(t)

	attestation, err := attest.Push(repo, desc,
		attest.PredicateSPDX, []byte(`{"spdxVersion":"SPDX-2.3"}`), false, nil)
	if err != nil {
		t.Fatalf("attaching the attestation: %v", err)
	}
	return repo, desc.Digest, attestation
}

// TestReferrersAreRefreshedToo: referrer manifests are UNTAGGED, so under the shipped
// deleteUntagged policy an attestation survives only if something pulls it (threat D6). Cosign's
// .sig is a tag and needs nothing here.
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

// TestRefreshingReferrersIsHarmlessWithoutAny: no referrers must not count as a failure.
func TestRefreshingReferrersIsHarmlessWithoutAny(t *testing.T) {
	repo, desc := artifact(t)

	out := &Result{}
	(&Refresher{}).refreshReferrers(repo.Digest(desc.Digest.String()), nil, out)

	if out.Failed != 0 {
		t.Errorf("an artifact with no referrers must not be counted as a failure: %+v", out)
	}
}

// TestOnlyDigestsHaveReferrers: the Referrers API takes digests, so a tag is not attempted.
func TestOnlyDigestsHaveReferrers(t *testing.T) {
	repo, _, _ := artifactWithAttestation(t)

	out := &Result{}
	(&Refresher{}).refreshReferrers(repo.Tag("v1"), nil, out)

	if out.Refreshed != 0 || out.Failed != 0 {
		t.Errorf("a tag reference should be skipped, not attempted: %+v", out)
	}
}

// TestTheRefreshLoopActuallyRefreshesReferrers guards the call site: the tests above pass even if
// refreshObject never calls refreshReferrers.
func TestTheRefreshLoopActuallyRefreshesReferrers(t *testing.T) {
	repo, digest, attestation := artifactWithAttestation(t)

	// Not buildWith: this needs a real registry that speaks the Referrers API.
	obj := &ociv1alpha1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
	}
	obj.Spec.Push = &ociv1alpha1.Push{Repository: repo.Name()}
	obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: digest.String()}
	obj.Generation = 1
	obj.Status.ObservedGeneration = 1

	rec := &pathRecorder{inner: remote.DefaultTransport}
	r, _ := refresherFor(t, repo.RegistryStr(), obj)
	r.Transport = rec

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
