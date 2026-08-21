package controller

import (
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/cache"
	"github.com/lhns/kube-oci-composer/internal/store"
)

func withCache(t *testing.T, r *ImageCompositionReconciler, remote store.Store) {
	t.Helper()
	c, err := cache.New(t.TempDir(), remote)
	if err != nil {
		t.Fatalf("creating cache: %v", err)
	}
	r.Cache = c
}

// TestRestartRebuildsFromCacheNotUpstream is the cold-start case the remote tier exists for.
//
// A restarted controller has an empty serving store, so it must rebuild and republish before it
// can serve anything. Without a shared cache that means pulling every layer from upstream again,
// which for a real artifact is tens of megabytes and the difference between a pod being unready
// for seconds and for minutes.
func TestRestartRebuildsFromCacheNotUpstream(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("restart-cache", urlLayer("core", origin.url, origin.digest, "/core"))

	remote := store.NewMemory()

	warm, _ := registryReconciler(t, obj)
	withCache(t, warm, remote)
	first := build(t, warm, obj, "first build")

	if origin.requests.Load() != 1 {
		t.Fatalf("first build made %d origin requests, want 1", origin.requests.Load())
	}

	// A new reconciler with an empty serving store and an empty local cache dir, sharing only the
	// remote tier — i.e. the pod came back on another node.
	restarted, _ := registryReconciler(t, obj)
	restarted.Client = warm.Client
	withCache(t, restarted, remote)
	origin.fail.Store(true)

	second := build(t, restarted, obj, "rebuild after restart")

	if origin.requests.Load() != 1 {
		t.Fatalf("restart re-fetched from upstream (%d requests total)", origin.requests.Load())
	}
	if second.Digest != first.Digest {
		t.Fatalf("rebuild from cache was not reproducible: %s then %s", first.Digest, second.Digest)
	}
}

// TestLayerSharedBetweenCompositionsIsFetchedOnce — content addressing means two compositions
// naming the same digest share one cache entry, with no coordination between them.
func TestLayerSharedBetweenCompositionsIsFetchedOnce(t *testing.T) {
	shared := newCountingOrigin(t, map[string]string{"lib/common.jar": "shared"})

	a := composition("consumer-a", urlLayer("core", shared.url, shared.digest, "/core"))
	b := composition("consumer-b", urlLayer("core", shared.url, shared.digest, "/plugins"))

	r, _ := registryReconciler(t, a, b)
	withCache(t, r, store.NewMemory())

	artA := build(t, r, a, "build a")
	artB := build(t, r, b, "build b")

	if shared.requests.Load() != 1 {
		t.Fatalf("the shared layer was fetched %d times, want 1", shared.requests.Load())
	}
	// Different targets, so the artifacts must still differ despite the shared input.
	if artA.Digest == artB.Digest {
		t.Fatal("two compositions with different targets produced the same artifact")
	}
}

// TestBuildStillWorksWithoutACache — the cache is optional, and a controller configured without
// one must behave exactly as before.
func TestBuildStillWorksWithoutACache(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa"})
	obj := composition("no-cache", urlLayer("core", origin.url, origin.digest, "/core"))

	r, _ := registryReconciler(t, obj)
	if r.Cache != nil {
		t.Fatal("expected no cache by default")
	}

	if art := build(t, r, obj, "build"); art.Digest == "" {
		t.Fatal("build produced no digest")
	}
	if origin.requests.Load() != 1 {
		t.Fatalf("made %d origin requests, want 1", origin.requests.Load())
	}
}

// TestCachedInputProducesTheSameDigest — a layer read from the cache must assemble to exactly the
// artifact a freshly fetched one does. If it did not, the inputHash short-circuit would be
// comparing against something that could no longer be reproduced.
func TestCachedInputProducesTheSameDigest(t *testing.T) {
	origin := newCountingOrigin(t, map[string]string{"lib/a.jar": "aaa", "README": "hello"})

	uncached := composition("uncached", urlLayer("core", origin.url, origin.digest, "/core"))
	r1, _ := registryReconciler(t, uncached)
	fresh := build(t, r1, uncached, "uncached build")

	cached := composition("cached", urlLayer("core", origin.url, origin.digest, "/core"))
	r2, _ := registryReconciler(t, cached)
	withCache(t, r2, store.NewMemory())
	build(t, r2, cached, "warm the cache")

	// Clearing the status defeats the inputHash short-circuit, forcing a genuine rebuild — this
	// time reading the layer from the cache rather than the origin.
	cached.Status = ociv1alpha1.ImageCompositionStatus{}
	origin.fail.Store(true)
	again := build(t, r2, cached, "cached build")

	if again.Digest != fresh.Digest {
		t.Fatalf("cached input assembled differently: %s vs %s", again.Digest, fresh.Digest)
	}
}
