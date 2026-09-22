package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/store"
)

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// countingOrigin writes body to a temp file and counts how many times it was asked to.
func countingOrigin(t *testing.T, body string) (Origin, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	calls := &atomic.Int64{}
	fail := &atomic.Bool{}

	return func(ctx context.Context, digest string) (string, error) {
		calls.Add(1)
		if fail.Load() {
			return "", errors.New("origin is unreachable")
		}
		f, err := os.CreateTemp(t.TempDir(), "origin-*")
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.WriteString(body); err != nil {
			return "", err
		}
		return f.Name(), nil
	}, calls, fail
}

func newCache(t *testing.T, remote store.Store) *Cache {
	t.Helper()
	c, err := New(t.TempDir(), remote)
	if err != nil {
		t.Fatalf("creating cache: %v", err)
	}
	return c
}

func mustPath(t *testing.T, c *Cache, digest string, origin Origin) string {
	t.Helper()
	p, err := c.Path(context.Background(), digest, origin)
	if err != nil {
		t.Fatalf("resolving %s: %v", digest, err)
	}
	return p
}

func contents(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestSecondLookupDoesNotHitTheOrigin is the reason this package exists.
func TestSecondLookupDoesNotHitTheOrigin(t *testing.T) {
	body := "layer content"
	origin, calls, fail := countingOrigin(t, body)
	c := newCache(t, nil)

	first := mustPath(t, c, digestOf(body), origin)
	if contents(t, first) != body {
		t.Fatal("first lookup returned the wrong content")
	}
	if calls.Load() != 1 {
		t.Fatalf("first lookup made %d origin calls, want 1", calls.Load())
	}

	// Take the origin away entirely; a cache that still reaches for it will fail loudly.
	fail.Store(true)

	second := mustPath(t, c, digestOf(body), origin)
	if calls.Load() != 1 {
		t.Fatalf("second lookup hit the origin (%d calls total)", calls.Load())
	}
	if contents(t, second) != body {
		t.Fatal("second lookup returned the wrong content")
	}
}

// TestRemoteTierSurvivesLocalLoss: after a restart empties the local tier, the remote tier serves.
func TestRemoteTierSurvivesLocalLoss(t *testing.T) {
	body := "durable content"
	origin, calls, fail := countingOrigin(t, body)
	remote := store.NewMemory()

	warm := newCache(t, remote)
	mustPath(t, warm, digestOf(body), origin)
	if calls.Load() != 1 {
		t.Fatalf("warming made %d origin calls, want 1", calls.Load())
	}

	// A new local directory stands in for a restarted pod. The remote tier is the same.
	restarted := newCache(t, remote)
	fail.Store(true)

	got := mustPath(t, restarted, digestOf(body), origin)
	if calls.Load() != 1 {
		t.Fatalf("restart hit the origin (%d calls total); the remote tier was not used", calls.Load())
	}
	if contents(t, got) != body {
		t.Fatal("content pulled from the remote tier is wrong")
	}
}

// TestRemoteWriteFailureDoesNotFailTheBuild: the cache is an optimisation.
func TestRemoteWriteFailureDoesNotFailTheBuild(t *testing.T) {
	body := "content"
	origin, _, _ := countingOrigin(t, body)
	c := newCache(t, brokenStore{})

	got := mustPath(t, c, digestOf(body), origin)
	if contents(t, got) != body {
		t.Fatal("build did not get its content when the remote tier was broken")
	}
}

// TestCorruptRemoteEntryIsRejectedAndRemoved: unverified remote bytes must be neither served nor
// left in place.
func TestCorruptRemoteEntryIsRejectedAndRemoved(t *testing.T) {
	body := "the real content"
	want := digestOf(body)
	remote := store.NewMemory()

	key := store.MustKey(store.NamespaceInputs, want)
	if err := remote.Write(context.Background(), key, strings.NewReader("TAMPERED")); err != nil {
		t.Fatalf("seeding corrupt entry: %v", err)
	}

	origin, calls, _ := countingOrigin(t, body)
	c := newCache(t, remote)

	got := mustPath(t, c, want, origin)
	if contents(t, got) != body {
		t.Fatal("corrupt remote content was served to the caller")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected a fall-through to the origin, got %d calls", calls.Load())
	}

	// Replaced by the verified content from the origin.
	rc, err := remote.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("remote entry is missing after repair: %v", err)
	}
	defer rc.Close()
	repaired, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading repaired entry: %v", err)
	}
	if string(repaired) == "TAMPERED" {
		t.Fatal("corrupt entry was left in the remote tier to poison the next lookup")
	}
	if string(repaired) != body {
		t.Fatalf("remote entry holds %q after repair, want %q", repaired, body)
	}
}

// TestCorruptRemoteEntryIsReplaced: after rejecting the bad copy, the next lookup is a hit.
func TestCorruptRemoteEntryIsReplaced(t *testing.T) {
	body := "good content"
	want := digestOf(body)
	remote := store.NewMemory()

	key := store.MustKey(store.NamespaceInputs, want)
	if err := remote.Write(context.Background(), key, strings.NewReader("bad")); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	origin, calls, fail := countingOrigin(t, body)
	c := newCache(t, remote)
	mustPath(t, c, want, origin)

	fresh := newCache(t, remote)
	fail.Store(true)
	got := mustPath(t, fresh, want, origin)

	if calls.Load() != 1 {
		t.Fatalf("the repaired entry was not reused: %d origin calls", calls.Load())
	}
	if contents(t, got) != body {
		t.Fatal("repaired entry has the wrong content")
	}
}

// TestOriginFailureIsReturned: a fetch failure must reach the caller, not be cached as empty.
func TestOriginFailureIsReturned(t *testing.T) {
	origin, _, fail := countingOrigin(t, "unused")
	fail.Store(true)
	c := newCache(t, store.NewMemory())

	if _, err := c.Path(context.Background(), digestOf("x"), origin); err == nil {
		t.Fatal("a failing origin did not produce an error")
	}
}

// TestSharedLayerIsFetchedOnce: compositions naming the same layer digest share one entry.
func TestSharedLayerIsFetchedOnce(t *testing.T) {
	body := "shared jar"
	origin, calls, _ := countingOrigin(t, body)
	c := newCache(t, store.NewMemory())

	for i := 0; i < 3; i++ {
		mustPath(t, c, digestOf(body), origin)
	}
	if calls.Load() != 1 {
		t.Fatalf("a shared layer was fetched %d times", calls.Load())
	}
}

// TestMalformedDigestIsRejected: digests are user input and must not escape the cache directory.
func TestMalformedDigestIsRejected(t *testing.T) {
	origin, calls, _ := countingOrigin(t, "x")
	c := newCache(t, nil)

	for _, bad := range []string{"sha256:../../escape", "notadigest", "sha256:", "sha256:a/b"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := c.Path(context.Background(), bad, origin); err == nil {
				t.Fatalf("accepted malformed digest %q", bad)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("a malformed digest reached the origin")
	}
}

// TestCachedFileLivesUnderTheCacheDir: the returned path is the one the local tier manages.
func TestCachedFileLivesUnderTheCacheDir(t *testing.T) {
	body := "content"
	origin, _, _ := countingOrigin(t, body)
	c := newCache(t, nil)

	got := mustPath(t, c, digestOf(body), origin)
	rel, err := filepath.Rel(c.Dir, got)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("cached file %q is not under the cache dir %q", got, c.Dir)
	}
	key := store.MustKey(store.NamespaceInputs, digestOf(body))
	if _, err := c.Local.Stat(context.Background(), key); err != nil {
		t.Fatalf("the local tier does not hold the entry just written: %v", err)
	}
}

// brokenStore fails every operation, standing in for object storage being unreachable.
type brokenStore struct{}

func (brokenStore) Stat(context.Context, string) (store.Info, error) {
	return store.Info{}, errors.New("unreachable")
}
func (brokenStore) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unreachable")
}
func (brokenStore) Write(context.Context, string, io.Reader) error { return errors.New("unreachable") }
func (brokenStore) Delete(context.Context, string) error           { return errors.New("unreachable") }
