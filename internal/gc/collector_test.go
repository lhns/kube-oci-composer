package gc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/store"
)

// The safety rails are tested before the deletion behaviour, and deliberately so: a collector
// that deletes the right things but also deletes live data on an incomplete view is worse than
// one that never runs.

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("client-go scheme: %v", err)
	}
	if err := ociv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("api scheme: %v", err)
	}
	return s
}

// staticPending reports a fixed set of unreconciled objects.
type staticPending struct {
	pending []string
	err     error
}

func (s staticPending) Pending(context.Context) ([]string, error) { return s.pending, s.err }

func blobDigest(n int) string {
	return fmt.Sprintf("sha256:%064x", n)
}

// oldStore returns a memory store whose objects all look older than any grace period, so tests
// that are not about the grace period do not have to fight it.
func oldStore(t *testing.T, digests ...string) *store.Memory {
	t.Helper()
	s := store.NewMemory()
	ancient := time.Now().Add(-24 * time.Hour)
	for _, d := range digests {
		k := store.MustKey(store.NamespaceBlobs, d)
		if err := s.Write(context.Background(), k, strings.NewReader("blob")); err != nil {
			t.Fatalf("seeding %s: %v", d, err)
		}
		s.SetModTime(k, ancient)
	}
	return s
}

func composition(name string, history ...ociv1alpha1.BuildRecord) *ociv1alpha1.ImageComposition {
	return &ociv1alpha1.ImageComposition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: ociv1alpha1.ImageCompositionSpec{
			Publish: &ociv1alpha1.Publish{Name: name, Tags: []string{"main"}},
		},
		Status: ociv1alpha1.ImageCompositionStatus{History: history},
	}
}

func newCollector(t *testing.T, blobs store.Store, pending PendingLister, objs ...client.Object) *Collector {
	t.Helper()
	return &Collector{
		Client:  fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build(),
		Blobs:   blobs,
		Pending: pending,
		Grace:   time.Hour,
	}
}

// TestSweepIsSkippedWhenTheViewIsIncomplete is the most important test in this package.
//
// Marking derives the live set from objects the controller has reconciled. An object it has not
// seen contributes nothing, so its blobs are indistinguishable from garbage. Skipping costs one
// interval of growth; not skipping costs data.
func TestSweepIsSkippedWhenTheViewIsIncomplete(t *testing.T) {
	orphan := blobDigest(1)
	blobs := oldStore(t, orphan)

	c := newCollector(t, blobs, staticPending{pending: []string{"default/not-yet-reconciled"}})

	result, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !result.Skipped {
		t.Fatal("swept despite an incomplete view")
	}
	if !strings.Contains(result.SkipReason, "not-yet-reconciled") {
		t.Fatalf("skip reason does not name the object: %q", result.SkipReason)
	}
	if blobs.Len() != 1 {
		t.Fatal("something was deleted during a skipped cycle")
	}
}

// TestPendingFailureDoesNotSweep — being unable to evaluate the gate must not be treated as
// permission to proceed.
func TestPendingFailureDoesNotSweep(t *testing.T) {
	blobs := oldStore(t, blobDigest(1))
	c := newCollector(t, blobs, staticPending{err: errors.New("api server unreachable")})

	if _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("collect succeeded despite being unable to check the gate")
	}
	if blobs.Len() != 1 {
		t.Fatal("something was deleted when the gate could not be evaluated")
	}
}

// TestGracePeriodProtectsFreshObjects — a build writes its blobs before recording them in status.
// A sweep landing in that window must not delete content that is moments from being referenced.
func TestGracePeriodProtectsFreshObjects(t *testing.T) {
	fresh := blobDigest(2)
	blobs := store.NewMemory()
	if err := blobs.Write(context.Background(), store.MustKey(store.NamespaceBlobs, fresh),
		strings.NewReader("just written")); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	c := newCollector(t, blobs, staticPending{})

	result, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.BlobsDeleted != 0 {
		t.Fatalf("deleted %d fresh blobs", result.BlobsDeleted)
	}
	if blobs.Len() != 1 {
		t.Fatal("a blob inside the grace period was deleted")
	}
}

// TestRetainedBuildsSurvive — everything status.History references must be kept, and only what
// falls off the end reclaimed.
func TestRetainedBuildsSurvive(t *testing.T) {
	kept, dropped := blobDigest(10), blobDigest(11)
	blobs := oldStore(t, kept, dropped)

	obj := composition("app", ociv1alpha1.BuildRecord{
		Digest: blobDigest(100),
		Blobs:  []string{kept},
	})
	c := newCollector(t, blobs, staticPending{}, obj)

	result, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.BlobsDeleted != 1 {
		t.Fatalf("deleted %d blobs, want 1", result.BlobsDeleted)
	}
	if _, err := blobs.Stat(context.Background(), store.MustKey(store.NamespaceBlobs, kept)); err != nil {
		t.Fatal("a retained build's blob was deleted")
	}
	if _, err := blobs.Stat(context.Background(), store.MustKey(store.NamespaceBlobs, dropped)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("an unreferenced blob survived")
	}
}

// TestCurrentArtifactSurvivesEvenWithoutHistory — belt and braces. Whatever else is true, what is
// published right now must stay pullable.
func TestCurrentArtifactSurvivesEvenWithoutHistory(t *testing.T) {
	current := blobDigest(20)
	blobs := oldStore(t, current)

	obj := composition("app")
	obj.Status.Artifact = &ociv1alpha1.ArtifactStatus{Digest: current}
	c := newCollector(t, blobs, staticPending{}, obj)

	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := blobs.Stat(context.Background(), store.MustKey(store.NamespaceBlobs, current)); err != nil {
		t.Fatal("the currently published artifact was deleted")
	}
}

// TestCacheEntriesAreMarkedFromSpecNotStatus — a declared but not-yet-built layer is already in
// the cache. Reclaiming it would force an immediate re-fetch of something about to be used.
func TestCacheEntriesAreMarkedFromSpec(t *testing.T) {
	declared, orphan := blobDigest(60), blobDigest(61)

	cacheStore := store.NewMemory()
	ancient := time.Now().Add(-24 * time.Hour)
	for _, d := range []string{declared, orphan} {
		k := store.MustKey(store.NamespaceInputs, d)
		if err := cacheStore.Write(context.Background(), k, strings.NewReader("x")); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		cacheStore.SetModTime(k, ancient)
	}

	obj := composition("app")
	obj.Spec.Layers = []ociv1alpha1.Layer{{
		Name:  "core",
		Fetch: &ociv1alpha1.FetchSource{URL: "https://example/x.tgz", Digest: declared},
		To:    "/x",
	}}

	c := newCollector(t, nil, staticPending{}, obj)
	c.Cache = cacheStore

	result, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.CacheDeleted != 1 {
		t.Fatalf("deleted %d cache entries, want 1", result.CacheDeleted)
	}
	if _, err := cacheStore.Stat(context.Background(), store.MustKey(store.NamespaceInputs, declared)); err != nil {
		t.Fatal("a declared layer's cache entry was reclaimed")
	}
}

// TestDryRunDeletesNothing — the escape hatch has to actually be one.
func TestDryRunDeletesNothing(t *testing.T) {
	blobs := oldStore(t, blobDigest(70), blobDigest(71))

	c := newCollector(t, blobs, staticPending{})
	c.DryRun = true

	result, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.BlobsDeleted != 2 {
		t.Fatalf("dry run reported %d reclaimable, want 2", result.BlobsDeleted)
	}
	if blobs.Len() != 2 {
		t.Fatalf("dry run deleted %d objects", 2-blobs.Len())
	}
}

// TestUnrecognisedKeysAreLeftAlone — anything the collector cannot parse is not its to delete.
func TestUnrecognisedKeysAreLeftAlone(t *testing.T) {
	blobs := store.NewMemory()
	if err := blobs.Write(context.Background(), "blobs/something-else", strings.NewReader("?")); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	blobs.SetModTime("blobs/something-else", time.Now().Add(-24*time.Hour))

	c := newCollector(t, blobs, staticPending{})

	result, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.BlobsDeleted != 0 {
		t.Fatal("deleted a key it could not parse")
	}
}

// TestListFailureSkipsTheNamespace — a partial listing reads as "these objects do not exist",
// which would make everything missing from it look unreferenced.
func TestListFailureSkipsTheNamespace(t *testing.T) {
	c := newCollector(t, failingList{store.NewMemory()}, staticPending{})

	result, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.BlobsDeleted != 0 {
		t.Fatal("deleted something despite a failed listing")
	}
	if result.Errors == 0 {
		t.Fatal("a failed listing was not reported as an error")
	}
}

type failingList struct{ *store.Memory }

func (failingList) List(context.Context, string) ([]store.Info, error) {
	return nil, errors.New("backend unavailable")
}

func TestDigestKeyRoundTrip(t *testing.T) {
	for _, d := range []string{blobDigest(1), "sha256:abcdef"} {
		key := store.MustKey(store.NamespaceBlobs, d)
		got, ok := digestFromKey(key)
		if !ok || got != d {
			t.Fatalf("key %q parsed to %q (ok=%v), want %q", key, got, ok, d)
		}
	}
	for _, bad := range []string{"blobs", "blobs/sha256", "a/b/c/d", ""} {
		if _, ok := digestFromKey(bad); ok {
			t.Fatalf("parsed nonsense key %q", bad)
		}
	}
}

// TestIndexChildManifestsSurvive is the failure ADR 0018 predicted and this guards against.
//
// Marking is derived from status and never parses manifest bytes, so nothing tells the collector
// that an index points at its children. Without BuildRecord.Manifests they are unreferenced, get
// swept, and leave a retained index resolving to nothing — which does not fail at collection time
// but at pull time, on a reference status still reports as published.
func TestIndexChildManifestsSurvive(t *testing.T) {
	index := blobDigest(200)
	childA, childB := blobDigest(201), blobDigest(202)
	orphan := blobDigest(203)

	s := store.NewMemory()
	ancient := time.Now().Add(-24 * time.Hour)
	for _, d := range []string{index, childA, childB, orphan} {
		k := store.MustKey(store.NamespaceManifests, d)
		if err := s.Write(context.Background(), k, strings.NewReader("manifest")); err != nil {
			t.Fatalf("seeding %s: %v", d, err)
		}
		s.SetModTime(k, ancient)
	}

	obj := composition("app", ociv1alpha1.BuildRecord{
		Digest:    index,
		Manifests: []string{childA, childB},
	})
	c := newCollector(t, s, staticPending{}, obj)

	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}

	for name, d := range map[string]string{"index": index, "child A": childA, "child B": childB} {
		if _, err := s.Stat(context.Background(), store.MustKey(store.NamespaceManifests, d)); err != nil {
			t.Fatalf("%s was reclaimed under a retained build: %v", name, err)
		}
	}
	if _, err := s.Stat(context.Background(), store.MustKey(store.NamespaceManifests, orphan)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("an unreferenced manifest survived, so this test proves nothing")
	}
}
