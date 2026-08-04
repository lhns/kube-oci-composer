package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"k8s.io/client-go/tools/record"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/serve"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// restart builds a reconciler sharing an existing blob store but with a fresh registry, which is
// exactly what a restarted pod looks like: the manifests map is in memory and starts empty.
func restart(t *testing.T, blobs store.Store, obj *ociv1alpha1.ImageComposition) (*ImageCompositionReconciler, string) {
	t.Helper()

	srv, err := serve.New("oci.test", ":0", blobs, false)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	host := strings.TrimPrefix(httpSrv.URL, "http://")
	srv.Addr = host[strings.LastIndex(host, ":"):]

	r, _ := servingReconciler(t, obj)
	r.Server = srv
	r.Recorder = record.NewFakeRecorder(32)
	r.Fetcher = oci.NewFetcher()
	return r, host
}

func resolves(t *testing.T, ref string) bool {
	t.Helper()
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parsing %s: %v", ref, err)
	}
	_, err = remote.Head(parsed)
	return err == nil
}

// TestOlderDigestSurvivesRestart is the defect ADR 0013 records.
//
// image-automation writes a digest into git, and between a build and the commit that rolls the
// workload onto it, a pod is pinned to the PREVIOUS digest. Before manifest persistence that
// digest returned 404 after any restart — so the pod could not start on a fresh node, and the
// failure surfaced somewhere unrelated to the restart that caused it.
func TestOlderDigestSurvivesRestart(t *testing.T) {
	first := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	second := newCountingOrigin(t, map[string]string{"lib/a.jar": "bbb"})

	obj := composition("durable", urlLayer("core", first.url, first.digest, "/core"))
	blobs := store.NewMemory() // survives the restart, as a PVC or S3 bucket would

	// Each build carries its own tag, as the spec-hash pattern produces: a changed spec is a
	// changed tag, so the old one has to keep resolving on its own rather than being superseded.
	obj.Spec.Publish.Tags = []string{"sOLD"}

	r1, _ := restart(t, blobs, obj)
	older := build(t, r1, obj, "first build")

	// A newer build supersedes it, so the older one is history rather than current.
	obj.Spec.Layers[0] = urlLayer("core", second.url, second.digest, "/core")
	obj.Spec.Publish.Tags = []string{"sNEW"}
	newer := build(t, r1, obj, "second build")
	if newer.Digest == older.Digest {
		t.Fatal("the two builds are identical; the test proves nothing")
	}

	// Restart.
	r2, host := restart(t, blobs, obj)
	build(t, r2, obj, "reconcile after restart")

	if !resolves(t, host+"/durable@"+newer.Digest) {
		t.Fatal("the current build's digest does not resolve after a restart")
	}
	if !resolves(t, host+"/durable@"+older.Digest) {
		t.Fatal("an older build's DIGEST reference does not resolve after a restart; " +
			"a pod pinned to it by image automation could not start")
	}
	if !resolves(t, host+"/durable:sOLD") {
		t.Fatal("an older build's TAG does not resolve after a restart; " +
			"rolling back a commit to a previous spec-hash tag would not start")
	}
}

// TestReplayHappensOncePerProcess — replay is per object per process, not per reconcile. Doing it
// every interval would mean a HEAD and a PUT per retained build, forever, for no benefit.
func TestReplayHappensOncePerProcess(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("once", urlLayer("core", origin.url, origin.digest, "/core"))
	blobs := store.NewMemory()

	r, _ := restart(t, blobs, obj)
	key := objKey(obj)

	// The very first build has no history, so there is nothing to replay and nothing is marked.
	// Marking it anyway would mean a genuine restart never replayed at all.
	build(t, r, obj, "first build")
	if !r.replay.mark(key) {
		t.Fatal("an object with no history was marked as replayed")
	}
	r.replay.forget(key)

	// The second reconcile sees history, so it replays and marks.
	build(t, r, obj, "second build")
	if r.replay.mark(key) {
		t.Fatal("an object with history was not marked as replayed")
	}
	if r.replay.mark(key) {
		t.Fatal("marking is not idempotent")
	}

	r.replay.forget(key)
	if !r.replay.mark(key) {
		t.Fatal("forget did not clear the marker, so a recreated object would never replay")
	}
}

// TestReplayToleratesAMissingManifest — objects published before manifest persistence existed,
// or whose manifest was reclaimed, must not break the reconcile. They are simply as unavailable
// as they already were.
func TestReplayToleratesAMissingManifest(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("legacy", urlLayer("core", origin.url, origin.digest, "/core"))
	blobs := store.NewMemory()

	// History references a build whose manifest was never stored.
	obj.Status.History = []ociv1alpha1.BuildRecord{{
		Tags:   []string{"main"},
		Digest: "sha256:" + strings.Repeat("d", 64),
		Blobs:  []string{"sha256:" + strings.Repeat("e", 64)},
	}}

	r, _ := restart(t, blobs, obj)
	if art := build(t, r, obj, "build with unreplayable history"); art.Digest == "" {
		t.Fatal("a missing stored manifest broke the build")
	}
}

// TestPushModeDoesNotPersistManifests — an external registry owns its own storage, so there is
// nothing for this controller to replay and no reason to hold a copy.
func TestPushModeDoesNotPersistManifests(t *testing.T) {
	blobs := store.NewMemory()
	obj := composition("external")
	obj.Spec.Publish = nil
	obj.Spec.Push = &ociv1alpha1.Push{Repository: "registry.example.com/external", Tags: []string{"v1"}}

	r, _ := restart(t, blobs, obj)
	r.replayHistory(t.Context(), obj)

	if blobs.Len() != 0 {
		t.Fatalf("push-mode object touched local storage (%d objects)", blobs.Len())
	}
}
