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

// countingOrigin writes body to a temp file and counts how many times it was asked to. The count
// is the whole point: this package exists to make that number stop growing.
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

// TestRemoteTierSurvivesLocalLoss — this is the restart case. The pod comes back with an empty
// emptyDir, and without a remote tier every layer would be pulled from upstream again before the
// endpoint could serve anything.
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

// TestRemoteWriteFailureDoesNotFailTheBuild — the cache is an optimisation. If object storage is
// down, builds must still work, just slowly.
func TestRemoteWriteFailureDoesNotFailTheBuild(t *testing.T) {
	body := "content"
	origin, _, _ := countingOrigin(t, body)
	c := newCache(t, brokenStore{})

	got := mustPath(t, c, digestOf(body), origin)
	if contents(t, got) != body {
		t.Fatal("build did not get its content when the remote tier was broken")
	}
}

// TestCorruptRemoteEntryIsRejectedAndRemoved — the remote tier is shared and durable, so it can
// hold bytes this process never verified. Serving them would defeat the digest pinning the whole
// design rests on, and leaving them in place would make every future lookup fail the same way.
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

	// The corrupt bytes must be gone. What replaces them is the verified content, admitted on
	// the way back from the origin — leaving the key empty would be correct but wasteful, since
	// the next lookup would fall through again.
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

// TestCorruptRemoteEntryIsReplaced — after rejecting the bad copy, the good one must be admitted,
// so the next lookup is a hit rather than another fall-through.
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

// TestOriginFailureIsReturned — a genuine fetch failure must reach the caller, not be swallowed
// into an empty file that then gets cached under a digest it does not match.
func TestOriginFailureIsReturned(t *testing.T) {
	origin, _, fail := countingOrigin(t, "unused")
	fail.Store(true)
	c := newCache(t, store.NewMemory())

	if _, err := c.Path(context.Background(), digestOf("x"), origin); err == nil {
		t.Fatal("a failing origin did not produce an error")
	}
}

// TestSharedLayerIsFetchedOnce — two compositions naming the same layer digest share one entry.
// That falls out of content addressing rather than needing any special handling, but it is worth
// pinning because it is a stated property.
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

// TestMalformedDigestIsRejected — digests come from a CRD field. A key that escaped the cache
// directory would be a path traversal with attacker-controlled content.
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

// TestCachedFileLivesUnderTheCacheDir — the returned path must be the managed one, or garbage
// collection would never see the file it is supposed to account for.
func TestCachedFileLivesUnderTheCacheDir(t *testing.T) {
	body := "content"
	origin, _, _ := countingOrigin(t, body)
	c := newCache(t, nil)

	got := mustPath(t, c, digestOf(body), origin)
	rel, err := filepath.Rel(c.Dir, got)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("cached file %q is not under the cache dir %q", got, c.Dir)
	}
	if !c.Referenced(context.Background(), digestOf(body)) {
		t.Fatal("cache does not report the entry it just wrote")
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
func (brokenStore) List(context.Context, string) ([]store.Info, error) {
	return nil, errors.New("unreachable")
}
