package buildcontroller

import (
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// startRegistry runs a real OCI registry for the duration of a test, for the controller to tag in.
func startRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushByDigest puts content in the registry the way the build Job does: uploaded, unnamed.
func pushByDigest(t *testing.T, repo string) (v1.Image, string) {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("building a test image: %v", err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digesting: %v", err)
	}
	ref, err := name.NewDigest(repo+"@"+digest.String(), name.Insecure)
	if err != nil {
		t.Fatalf("parsing %s@%s: %v", repo, digest, err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("pushing by digest: %v", err)
	}
	return img, digest.String()
}

// tagResolvesTo reports what a tag currently holds, or "" when it holds nothing.
func tagResolvesTo(t *testing.T, repo, tag string) string {
	t.Helper()
	ref, err := name.NewTag(repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("parsing %s:%s: %v", repo, tag, err)
	}
	desc, err := remote.Get(ref)
	if err != nil {
		return ""
	}
	return desc.Digest.String()
}

// tagAs points a tag at content already in the registry.
func tagAs(t *testing.T, repo, tag, digest string) {
	t.Helper()
	dref, err := name.NewDigest(repo+"@"+digest, name.Insecure)
	if err != nil {
		t.Fatalf("parsing digest ref: %v", err)
	}
	desc, err := remote.Get(dref)
	if err != nil {
		t.Fatalf("reading %s: %v", digest, err)
	}
	tref, err := name.NewTag(repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("parsing tag ref: %v", err)
	}
	if err := remote.Tag(tref, desc); err != nil {
		t.Fatalf("tagging: %v", err)
	}
}
