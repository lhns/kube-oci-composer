package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/oci"
	"github.com/lhns/kube-oci-composer/internal/serve"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// servingOn wires a Server onto an EXISTING store, so two of them can share one — which is what
// shared storage means in production and what these tests need to model.
func servingOn(t *testing.T, blobs store.Store) (*serve.Server, string) {
	t.Helper()
	srv, err := serve.New("oci.test", ":0", blobs, false)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	host := strings.TrimPrefix(httpSrv.URL, "http://")
	srv.Addr = host[strings.LastIndex(host, ":"):]
	return srv, host
}

// leaderOn is a reconciler publishing into the given store — the replica that actually builds.
func leaderOn(t *testing.T, blobs store.Store, objs ...*ociv1alpha1.ImageComposition) *ImageCompositionReconciler {
	t.Helper()
	srv, _ := servingOn(t, blobs)

	builder := fake.NewClientBuilder().WithScheme(testScheme(t))
	for _, o := range objs {
		builder = builder.WithObjects(o).WithStatusSubresource(o)
	}
	return &ImageCompositionReconciler{
		Client:   builder.Build(),
		Scheme:   testScheme(t),
		Recorder: record.NewFakeRecorder(64),
		Server:   srv,
		Fetcher:  oci.NewFetcher(),
	}
}

// TestStandbyReplicaServesFromSharedStorage is the point of the feature: a replica that has never
// published anything must still answer pulls, so the registry stops being a single point of
// failure. With spec-hash tags every spec change is a new tag and therefore a new pull, so this
// is the common path rather than a rare one.
func TestStandbyReplicaServesFromSharedStorage(t *testing.T) {
	shared, err := store.NewDisk(t.TempDir())
	if err != nil {
		t.Fatalf("creating shared store: %v", err)
	}

	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("shared", urlLayer("core", url, digest, "/core"))
	obj.Spec.Publish = &ociv1alpha1.Publish{Name: "shared", Tags: []string{"sSHARED"}}

	leader := leaderOn(t, shared, obj)
	art := build(t, leader, obj, "leader build")

	// The standby: same store, its own registry, nothing ever published through it.
	standby, standbyHost := servingOn(t, shared)

	ref, err := name.ParseReference(standbyHost+"/shared:sSHARED", name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := remote.Head(ref); err == nil {
		t.Fatal("the standby served the tag before replay, so this test proves nothing")
	}

	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj).WithStatusSubresource(obj).Build()
	readiness := &Readiness{Client: k8s}
	sr := &StandbyReplay{Client: k8s, Server: standby, Readiness: readiness}
	if err := sr.replayAll(t.Context()); err != nil {
		t.Fatalf("replayAll: %v", err)
	}

	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatalf("the standby still cannot serve the tag after replay: %v", err)
	}
	if desc.Digest.String() != art.Digest {
		t.Fatalf("standby serves %s, want %s", desc.Digest, art.Digest)
	}

	// The digest reference matters as much as the tag: it is what a pinned workload names.
	byDigest, err := name.ParseReference(standbyHost+"/shared@"+art.Digest, name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := remote.Head(byDigest); err != nil {
		t.Fatalf("the standby cannot serve by digest: %v", err)
	}

	// It must also report ready, or it would serve correctly while staying out of the Service —
	// which is exactly the active/standby behaviour this replaces.
	if err := readiness.Check(httptest.NewRequest("GET", "/readyz", nil)); err != nil {
		t.Fatalf("standby is serving but not ready: %v", err)
	}
}

// TestStandbyReplayIsIdempotent — it runs on an interval, so a repeat pass must be free and must
// not disturb what is already served.
func TestStandbyReplayIsIdempotent(t *testing.T) {
	shared, err := store.NewDisk(t.TempDir())
	if err != nil {
		t.Fatalf("creating shared store: %v", err)
	}

	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("idem", urlLayer("core", url, digest, "/core"))
	obj.Spec.Publish = &ociv1alpha1.Publish{Name: "idem", Tags: []string{"sIDEM"}}

	leader := leaderOn(t, shared, obj)
	art := build(t, leader, obj, "leader build")

	standby, standbyHost := servingOn(t, shared)
	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj).WithStatusSubresource(obj).Build()
	sr := &StandbyReplay{Client: k8s, Server: standby, Readiness: &Readiness{Client: k8s}}

	for i := range 3 {
		if err := sr.replayAll(t.Context()); err != nil {
			t.Fatalf("replayAll pass %d: %v", i, err)
		}
	}

	ref, err := name.ParseReference(standbyHost+"/idem:sIDEM", name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatalf("after repeated replay the tag does not resolve: %v", err)
	}
	if desc.Digest.String() != art.Digest {
		t.Fatalf("resolves to %s, want %s", desc.Digest, art.Digest)
	}
}

// TestStandbyReplaySkipsPushMode — a push-mode artifact lives in someone else's registry, so there
// is nothing local to replay, and it must not hold this replica's readiness back either.
func TestStandbyReplaySkipsPushMode(t *testing.T) {
	shared, err := store.NewDisk(t.TempDir())
	if err != nil {
		t.Fatalf("creating shared store: %v", err)
	}
	standby, _ := servingOn(t, shared)

	obj := composition("external")
	obj.Spec.Publish = nil
	obj.Spec.Push = &ociv1alpha1.Push{Repository: "registry.example.com/external", Tags: []string{"v1"}}
	obj.Status.History = []ociv1alpha1.BuildRecord{{
		Digest: "sha256:" + strings.Repeat("d", 64), Tags: []string{"v1"},
	}}

	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj).WithStatusSubresource(obj).Build()
	readiness := &Readiness{Client: k8s}
	sr := &StandbyReplay{Client: k8s, Server: standby, Readiness: readiness}

	if err := sr.replayAll(t.Context()); err != nil {
		t.Fatalf("replayAll: %v", err)
	}
	if err := readiness.Check(httptest.NewRequest("GET", "/readyz", nil)); err != nil {
		t.Fatalf("a push-mode object held readiness back: %v", err)
	}
}

// TestLeaderElectionFollowsSharedStorage pins the switch that decides whether standbys serve.
// Getting either half wrong is silent: leader-only serving looks fine until the leader dies, and
// serving from a node-local store looks fine until a pull lands on the wrong replica.
func TestLeaderElectionFollowsSharedStorage(t *testing.T) {
	s := &serve.Server{}
	if !s.NeedLeaderElection() {
		t.Fatal("without shared storage the endpoint must stay leader-only, or a standby serves an empty store")
	}
	s.SharedStorage = true
	if s.NeedLeaderElection() {
		t.Fatal("with shared storage the endpoint must run on every replica, or there is no point")
	}

	if (&StandbyReplay{}).NeedLeaderElection() {
		t.Fatal("standby replay must run WITHOUT leader election; that is its entire purpose")
	}
}

// TestStandbyReplayRepairsAMissingTag — the skip must consider every reference a build published,
// not just its digest.
//
// A tag PUT during replay only logs on failure. Skipping on the digest alone therefore made one
// failure permanent: the digest stayed present, so the build was skipped on every later pass, and
// that tag 404'd on this replica for the life of the process while another replica served it. From
// a client that is indistinguishable from a registry that intermittently lacks the image.
func TestStandbyReplayRepairsAMissingTag(t *testing.T) {
	shared, err := store.NewDisk(t.TempDir())
	if err != nil {
		t.Fatalf("creating shared store: %v", err)
	}

	url, digest := contentServer(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("partial", urlLayer("core", url, digest, "/core"))
	obj.Spec.Publish = &ociv1alpha1.Publish{Name: "partial", Tags: []string{"sPARTIAL"}}

	leader := leaderOn(t, shared, obj)
	art := build(t, leader, obj, "leader build")

	standby, standbyHost := servingOn(t, shared)

	// The state a failed tag PUT leaves behind: the digest is served, the tag is not.
	raw, err := standby.LoadManifest(t.Context(), art.Digest)
	if err != nil {
		t.Fatalf("loading the stored manifest: %v", err)
	}
	if err := standby.PutManifest(t.Context(), "partial", art.Digest, raw); err != nil {
		t.Fatalf("restoring by digest: %v", err)
	}
	if !standby.HasManifest(t.Context(), "partial", art.Digest) {
		t.Fatal("the digest is not present, so this test does not reproduce the skip")
	}

	k8s := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(obj).WithStatusSubresource(obj).Build()
	sr := &StandbyReplay{Client: k8s, Server: standby, Readiness: &Readiness{Client: k8s}}
	if err := sr.replayAll(t.Context()); err != nil {
		t.Fatalf("replayAll: %v", err)
	}

	ref, err := name.ParseReference(standbyHost+"/partial:sPARTIAL", name.Insecure)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatalf("the tag was not repaired, so this replica serves 404 for it forever: %v", err)
	}
	if desc.Digest.String() != art.Digest {
		t.Fatalf("repaired tag serves %s, want %s", desc.Digest, art.Digest)
	}
}
