package controller

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/oci"
)

// countingOrigin serves a tar.gz and counts requests; fail makes it return 503.
type countingOrigin struct {
	url, digest string
	requests    *atomic.Int64
	fail        *atomic.Bool
}

func newCountingOrigin(t *testing.T, files map[string]string) *countingOrigin {
	t.Helper()

	payload := tarGz(t, files)
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

	o.url = srv.URL + "/content.tar.gz"
	o.digest = sha256Digest(payload)
	return o
}

// TestSteadyStateReconcileDoesNotFetch pins the inputHash short-circuit: an unchanged spec must
// converge without downloading any layer. The origin is taken away to prove it is not touched.
func TestSteadyStateReconcileDoesNotFetch(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("steady", urlLayer("core", origin.url, origin.digest, "/core"))
	r, _ := registryReconciler(t, obj)

	first := build(t, r, obj, "first build")
	if got := origin.requests.Load(); got != 1 {
		t.Fatalf("first build made %d origin requests, want 1", got)
	}

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
	r, _ := registryReconciler(t, obj)

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
func TestTargetChangeAloneRebuilds(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("retarget", urlLayer("core", origin.url, origin.digest, "/core"))
	r, _ := registryReconciler(t, obj)

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
	r, _ := registryReconciler(t, obj)

	first := build(t, r, obj, "first build")

	// Simulate a restart: the status survives on the object, the served store does not.
	fresh, _ := registryReconciler(t, obj)
	fresh.Client = r.Client

	second := build(t, fresh, obj, "rebuild after restart")
	if second.Digest != first.Digest {
		t.Fatalf("rebuild was not reproducible: %s then %s", first.Digest, second.Digest)
	}
	if origin.requests.Load() < 2 {
		t.Fatal("nothing was re-fetched, so nothing was republished into the empty store")
	}
}

// TestInputHashIgnoresIncidentalFields — name, URL and temp path must not affect the hash, or a
// mirror switch or rename would rebuild byte-identical content.
func TestInputHashIgnoresIncidentalFields(t *testing.T) {
	base := []oci.LayerInput{{
		Name: "core", URL: "https://a.example/x.tgz", Path: "/tmp/one",
		Digest: "sha256:abcd", Unpack: oci.UnpackTarGz, Target: "/core",
	}}
	renamed := []oci.LayerInput{{
		Name: "renamed", URL: "https://mirror.example/y.tgz", Path: "/tmp/two",
		Digest: "sha256:abcd", Unpack: oci.UnpackTarGz, Target: "/core",
	}}

	if oci.InputHash(base, oci.Config{}, "", nil) != oci.InputHash(renamed, oci.Config{}, "", nil) {
		t.Fatal("a rename, a mirror change or a different temp path changed the input hash")
	}
}

// TestInputHashIsUnambiguous — fields are length-prefixed; plain concatenation would make these
// two inputs collide and a real change be skipped.
func TestInputHashIsUnambiguous(t *testing.T) {
	a := oci.InputHash([]oci.LayerInput{
		{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/ab"},
		{Digest: "sha256:22", Unpack: oci.UnpackNone, Target: "/c"},
	}, oci.Config{}, "", nil)
	b := oci.InputHash([]oci.LayerInput{
		{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/a"},
		{Digest: "sha256:22", Unpack: oci.UnpackNone, Target: "/bc"},
	}, oci.Config{}, "", nil)
	if a == b {
		t.Fatal("input hash is ambiguous across field boundaries")
	}
}

// TestInputHashCoversConfig — labels, env, entrypoint and cmd all land in the image config and
// therefore in the output digest, so all of them must move the input hash.
func TestInputHashCoversConfig(t *testing.T) {
	layers := []oci.LayerInput{{Digest: "sha256:11", Unpack: oci.UnpackNone, Target: "/x"}}
	baseline := oci.InputHash(layers, oci.Config{}, "", nil)

	variants := map[string]oci.Config{
		"labels":     {Labels: map[string]string{"a": "b"}},
		"env":        {Env: []string{"A=b"}},
		"entrypoint": {Entrypoint: []string{"/bin/sh"}},
		"cmd":        {Cmd: []string{"-c", "true"}},
	}
	for name, cfg := range variants {
		t.Run(name, func(t *testing.T) {
			if oci.InputHash(layers, cfg, "", nil) == baseline {
				t.Fatalf("changing %s did not change the input hash", name)
			}
		})
	}

	// Map iteration order must not leak into the hash, or the controller would rebuild at random.
	many := oci.Config{Labels: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}}
	first := oci.InputHash(layers, many, "", nil)
	for i := 0; i < 20; i++ {
		if oci.InputHash(layers, many, "", nil) != first {
			t.Fatal("label map iteration order leaked into the input hash")
		}
	}
}

// TestInputHashIsPinned guards the hash against accidental change: changing its computation or
// bumping oci.AssemblyVersion rebuilds every artifact in every cluster. If this fails, either the
// change was deliberate (update the constant) or it is a bug.
func TestInputHashIsPinned(t *testing.T) {
	// Last re-recorded for AssemblyVersion 3 (ADR 0057). See also
	// TestUnsetPlatformMatchesTheOldHardcodedDefault.
	const want = "sha256:f0faf228562e7c0c249bb4d987ee06877d98cf1944ef1b95027e9a8ee387510c"

	got := oci.InputHash([]oci.LayerInput{
		{Name: "core", URL: "https://example/x.tgz", Digest: "sha256:1111", Unpack: oci.UnpackTarGz, Target: "/core"},
		{Name: "s3", URL: "https://example/y.tgz", Digest: "sha256:2222", Unpack: oci.UnpackTarGz, Target: "/s3"},
	}, oci.Config{
		Labels:     map[string]string{"b": "2", "a": "1"},
		Env:        []string{"A=1"},
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "true"},
	}, "", nil)

	if got != want {
		t.Fatalf("input hash changed.\n  got:  %s\n  want: %s\n"+
			"If this was deliberate (a change to InputHash or a bump of oci.AssemblyVersion), "+
			"update the constant. Be aware it rebuilds every artifact in every cluster.", got, want)
	}
}
