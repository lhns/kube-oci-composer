package source

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var (
	amd64 = v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 = v1.Platform{OS: "linux", Architecture: "arm64"}
)

func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// child is a random image whose config declares p, so the tests can tell which one was selected.
func child(t *testing.T, size int64, p v1.Platform) v1.Image {
	t.Helper()
	img, err := random.Image(size, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture, cf.Variant = p.OS, p.Architecture, p.Variant
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatalf("mutate config: %v", err)
	}
	return img
}

func digestOf(t *testing.T, d interface{ Digest() (v1.Hash, error) }) string {
	t.Helper()
	h, err := d.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return h.String()
}

func publishIndex(t *testing.T, host, repo string, children ...v1.Image) string {
	t.Helper()
	idx := mutate.IndexMediaType(empty.Index, "application/vnd.oci.image.index.v1+json")
	for _, img := range children {
		cf, err := img.ConfigFile()
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{
				OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant}},
		})
	}
	d := digestOf(t, idx)
	ref, err := name.NewDigest(host + "/" + repo + "@" + d)
	if err != nil {
		t.Fatalf("ref: %v", err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("publishing index: %v", err)
	}
	return d
}

func TestPullImageForSelectsAChildOfAnIndex(t *testing.T) {
	host := testRegistry(t)
	a, b := child(t, 64, amd64), child(t, 65, arm64)
	d := publishIndex(t, host, "idx", a, b)

	img, byPlatform, err := PullImageFor(context.Background(), host+"/idx", d, []v1.Platform{arm64, amd64})
	if err != nil {
		t.Fatalf("PullImageFor: %v", err)
	}
	if img != nil {
		t.Fatal("an index returned a single image")
	}
	if len(byPlatform) != 2 {
		t.Fatalf("want 2 children, got %d", len(byPlatform))
	}
	if got, want := digestOf(t, byPlatform["linux/arm64"]), digestOf(t, b); got != want {
		t.Errorf("linux/arm64 is %s, want %s", got, want)
	}
	if got, want := digestOf(t, byPlatform["linux/amd64"]), digestOf(t, a); got != want {
		t.Errorf("linux/amd64 is %s, want %s", got, want)
	}
}

func TestPullImageForRefusesAnIndexWithoutPlatforms(t *testing.T) {
	host := testRegistry(t)
	d := publishIndex(t, host, "idx", child(t, 64, amd64))

	_, _, err := PullImageFor(context.Background(), host+"/idx", d, nil)
	var bad *ErrBadReference
	if !errors.As(err, &bad) {
		t.Fatalf("want ErrBadReference, got %v", err)
	}
	if !strings.Contains(err.Error(), "spec.platforms") {
		t.Errorf("the error does not point at spec.platforms: %v", err)
	}
}

func TestPullImageForNamesAPlatformTheIndexLacks(t *testing.T) {
	host := testRegistry(t)
	d := publishIndex(t, host, "idx", child(t, 64, amd64))

	_, _, err := PullImageFor(context.Background(), host+"/idx", d, []v1.Platform{arm64})
	var bad *ErrBadReference
	if !errors.As(err, &bad) {
		t.Fatalf("want ErrBadReference, got %v", err)
	}
	if !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("the error does not name the missing platform: %v", err)
	}
}

// A single manifest is returned as it is, whatever platforms are asked for: it is plain content
// for a layer, and a base under one declared platform was never checked against it either.
func TestPullImageForReturnsASingleManifestUnchecked(t *testing.T) {
	host := testRegistry(t)
	img := child(t, 64, amd64)
	d := digestOf(t, img)
	ref, err := name.NewDigest(host + "/single@" + d)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	for _, platforms := range [][]v1.Platform{nil, {amd64}, {arm64}, {amd64, arm64}} {
		got, byPlatform, err := PullImageFor(context.Background(), host+"/single", d, platforms)
		if err != nil {
			t.Fatalf("platforms %v: %v", platforms, err)
		}
		if byPlatform != nil || got == nil {
			t.Fatalf("platforms %v: want the image alone, got %v / %v", platforms, got, byPlatform)
		}
		if digestOf(t, got) != d {
			t.Errorf("platforms %v: wrong image", platforms)
		}
	}
}
