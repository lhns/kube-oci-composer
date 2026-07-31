package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/oci"
)

// countingOrigin serves the payload and counts requests, so a test can assert on how many times
// the controller actually reached out over the network rather than inferring it from timing.
type countingOrigin struct {
	url, digest string
	requests    *atomic.Int64
	fail        *atomic.Bool
}

func newCountingOrigin(t *testing.T, files map[string]string) *countingOrigin {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	payload := buf.Bytes()

	o := &countingOrigin{requests: &atomic.Int64{}, fail: &atomic.Bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.requests.Add(1)
		if o.fail.Load() {
			http.Error(w, "origin is offline", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	sum := sha256.Sum256(payload)
	o.url = srv.URL + "/content.tar.gz"
	o.digest = "sha256:" + hex.EncodeToString(sum[:])
	return o
}

// TestSteadyStateReconcileDoesNotFetch is the test this change exists for.
//
// Before the inputHash short-circuit, the output digest could only be learned by downloading
// every layer and assembling them — so an hourly interval meant re-pulling tens of megabytes
// from upstream, forever, to discover that nothing had changed. Taking the origin away after the
// first build is the only honest way to prove the second reconcile does not touch it.
func TestSteadyStateReconcileDoesNotFetch(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("steady", urlLayer("core", origin.url, origin.digest, "/core"))
	r, _ := servingReconciler(t, obj)

	first := build(t, r, obj, "first build")
	if got := origin.requests.Load(); got != 1 {
		t.Fatalf("first build made %d origin requests, want 1", got)
	}

	// The origin is now gone. A reconcile that still needs it will fail loudly.
	origin.fail.Store(true)

	second := build(t, r, obj, "steady-state reconcile")
	if got := origin.requests.Load(); got != 1 {
		t.Fatalf("steady-state reconcile hit the origin %d times, want 0 more than the first build", got-1)
	}
	if second.Digest != first.Digest {
		t.Fatalf("digest changed without any input changing: %s then %s", first.Digest, second.Digest)
	}
}

// TestChangedSpecStillRebuilds — the short-circuit must not become a way to miss real changes.
func TestChangedSpecStillRebuilds(t *testing.T) {
	a := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	b := newCountingOrigin(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("changing", urlLayer("core", a.url, a.digest, "/core"))
	r, _ := servingReconciler(t, obj)

	first := build(t, r, obj, "first build")

	obj.Spec.Layers[0] = urlLayer("core", b.url, b.digest, "/core")
	second := build(t, r, obj, "rebuild")

	if b.requests.Load() == 0 {
		t.Fatal("the new layer was never fetched; the short-circuit swallowed a real change")
	}
	if second.Digest == first.Digest {
		t.Fatal("changed input produced the same digest")
	}
}

// TestTargetChangeAloneRebuilds — the same bytes at a different path are a different artifact.
// Hashing only the layer digests would miss this, and the workload would silently keep the old
// layout.
func TestTargetChangeAloneRebuilds(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("retarget", urlLayer("core", origin.url, origin.digest, "/core"))
	r, _ := servingReconciler(t, obj)

	first := build(t, r, obj, "first build")

	obj.Spec.Layers[0] = urlLayer("core", origin.url, origin.digest, "/plugins")
	second := build(t, r, obj, "rebuild at a new target")

	if second.Digest == first.Digest {
		t.Fatal("changing only the target did not change the output")
	}
}

// TestMissingPublishedArtifactForcesRebuild — an unchanged input hash is not enough on its own.
// If the serving store was emptied by a restart, the artifact must come back rather than the
// controller reporting Ready over a 404.
func TestMissingPublishedArtifactForcesRebuild(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("restarted", urlLayer("core", origin.url, origin.digest, "/core"))
	r, _ := servingReconciler(t, obj)

	first := build(t, r, obj, "first build")

	// Simulate a restart: the status survives on the object, the served store does not.
	fresh, _ := servingReconciler(t, obj)
	fresh.Client = r.Client

	second := build(t, fresh, obj, "rebuild after restart")
	if second.Digest != first.Digest {
		t.Fatalf("rebuild was not reproducible: %s then %s", first.Digest, second.Digest)
	}
	if origin.requests.Load() < 2 {
		t.Fatal("nothing was re-fetched, so nothing was republished into the empty store")
	}
}

// TestInputHashIgnoresIncidentalFields — name and URL must not affect the hash. Switching to a
// mirror or renaming an entry would otherwise force a pointless rebuild of byte-identical
// content, which is the exact cost this whole mechanism exists to avoid.
func TestInputHashIgnoresIncidentalFields(t *testing.T) {
	base := []oci.LayerInput{{
		Name: "core", URL: "https://a.example/x.tgz", Path: "/tmp/one",
		Digest: "sha256:abcd", Unpack: oci.UnpackTarGz, Target: "/core",
	}}
	renamed := []oci.LayerInput{{
		Name: "renamed", URL: "https://mirror.example/y.tgz", Path: "/tmp/two",
		Digest: "sha256:abcd", Unpack: oci.UnpackTarGz, Target: "/core",
	}}

	if oci.InputHash(base, oci.Config{}) != oci.InputHash(renamed, oci.Config{}) {
		t.Fatal("a rename, a mirror change or a different temp path changed the input hash")
	}
}

// TestInputHashIsUnambiguous — fields are length-prefixed so no rearrangement across a field
// boundary can collide. Plain concatenation would make these two byte streams identical, and a
// real spec change would then be silently skipped.
func TestInputHashIsUnambiguous(t *testing.T) {
	a := oci.InputHash([]oci.LayerInput{
		{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/ab"},
		{Digest: "sha256:22", Unpack: oci.UnpackNone, Target: "/c"},
	}, oci.Config{})
	b := oci.InputHash([]oci.LayerInput{
		{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/a"},
		{Digest: "sha256:22", Unpack: oci.UnpackNone, Target: "/bc"},
	}, oci.Config{})
	if a == b {
		t.Fatal("input hash is ambiguous across field boundaries")
	}
}

// TestInputHashCoversConfig — labels, env, entrypoint and cmd all land in the image config and
// therefore in the output digest, so all of them must move the input hash.
func TestInputHashCoversConfig(t *testing.T) {
	layers := []oci.LayerInput{{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/x"}}
	baseline := oci.InputHash(layers, oci.Config{})

	variants := map[string]oci.Config{
		"labels":     {Labels: map[string]string{"a": "b"}},
		"env":        {Env: []string{"A=b"}},
		"entrypoint": {Entrypoint: []string{"/bin/sh"}},
		"cmd":        {Cmd: []string{"-c", "true"}},
	}
	for name, cfg := range variants {
		t.Run(name, func(t *testing.T) {
			if oci.InputHash(layers, cfg) == baseline {
				t.Fatalf("changing %s did not change the input hash", name)
			}
		})
	}

	// Map iteration order must not leak into the hash, or the controller would rebuild at random.
	many := oci.Config{Labels: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}}
	first := oci.InputHash(layers, many)
	for i := 0; i < 20; i++ {
		if oci.InputHash(layers, many) != first {
			t.Fatal("label map iteration order leaked into the input hash")
		}
	}
}

// TestInputHashIsPinned guards the hash against accidental change.
//
// The input hash decides whether a build is skipped, so changing how it is computed — or bumping
// oci.AssemblyVersion — invalidates every recorded hash in every cluster and rebuilds everything
// on the next reconcile. That is sometimes exactly right, and it must never happen by accident.
// If this test fails, the change was either deliberate (update the constant below) or a bug.
func TestInputHashIsPinned(t *testing.T) {
	const want = "sha256:73cafd2a6f5f486391ff0aeead95570e52d9cb08b09f0bb118fd0121774e86cf"

	got := oci.InputHash([]oci.LayerInput{
		{Name: "core", URL: "https://example/x.tgz", Digest: "sha256:1111", Unpack: oci.UnpackTarGz, Target: "/core"},
		{Name: "s3", URL: "https://example/y.tgz", Digest: "sha256:2222", Unpack: oci.UnpackTarGz, Target: "/s3"},
	}, oci.Config{
		Labels:     map[string]string{"b": "2", "a": "1"},
		Env:        []string{"A=1"},
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "true"},
	})

	if got != want {
		t.Fatalf("input hash changed.\n  got:  %s\n  want: %s\n"+
			"If this was deliberate (a change to InputHash or a bump of oci.AssemblyVersion), "+
			"update the constant. Be aware it rebuilds every artifact in every cluster.", got, want)
	}
}
