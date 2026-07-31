package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The conformance suite. Every backend runs the same tests, because the whole point of the
// interface is that the input cache and the serving endpoint cannot tell which one they have.
// A backend that passes here is substitutable; one that only passes its own tests is not.

func eachBackend(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, NewMemory()) })
	t.Run("disk", func(t *testing.T) {
		s, err := NewDisk(t.TempDir())
		if err != nil {
			t.Fatalf("creating disk store: %v", err)
		}
		fn(t, s)
	})
}

func put(t *testing.T, s Store, key, body string) {
	t.Helper()
	if err := s.Write(context.Background(), key, strings.NewReader(body)); err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
}

func read(t *testing.T, s Store, key string) string {
	t.Helper()
	rc, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("open %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return string(b)
}

func TestRoundTrip(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceBlobs, "sha256:aa11")
		put(t, s, key, "hello")

		if got := read(t, s, key); got != "hello" {
			t.Fatalf("read back %q, want %q", got, "hello")
		}
		info, err := s.Stat(context.Background(), key)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Size != 5 {
			t.Fatalf("size %d, want 5", info.Size)
		}
		if info.Key != key {
			t.Fatalf("Info.Key is %q, want %q", info.Key, key)
		}
	})
}

// TestMissIsErrNotFound — callers treat a miss as ordinary control flow: the cache falls through
// to the origin and the serving endpoint returns 404. A backend-specific error would turn both
// into failures.
func TestMissIsErrNotFound(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceInputs, "sha256:ffff")

		if _, err := s.Stat(context.Background(), key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat on a missing key returned %v, want ErrNotFound", err)
		}
		if _, err := s.Open(context.Background(), key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Open on a missing key returned %v, want ErrNotFound", err)
		}
	})
}

// TestDeleteIsIdempotent — garbage collection is not a transaction. A key vanishing between the
// listing and the delete must not fail the sweep.
func TestDeleteIsIdempotent(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceBlobs, "sha256:bb22")
		put(t, s, key, "x")

		for i := 0; i < 3; i++ {
			if err := s.Delete(context.Background(), key); err != nil {
				t.Fatalf("delete %d: %v", i+1, err)
			}
		}
		if _, err := s.Stat(context.Background(), key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("key survived deletion: %v", err)
		}
	})
}

// TestOverwriteWithIdenticalContentSucceeds — two reconciles racing on the same content-addressed
// key is the normal case, not an error.
func TestOverwriteWithIdenticalContentSucceeds(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceBlobs, "sha256:cc33")
		put(t, s, key, "same")
		put(t, s, key, "same")

		if got := read(t, s, key); got != "same" {
			t.Fatalf("content is %q after rewrite, want %q", got, "same")
		}
	})
}

// TestConcurrentWritesDoNotCorrupt — the disk backend writes via temp file and rename precisely
// so that a reader never observes a half-written object.
func TestConcurrentWritesDoNotCorrupt(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceBlobs, "sha256:dd44")
		body := strings.Repeat("payload", 4096)

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = s.Write(context.Background(), key, strings.NewReader(body))
			}()
		}
		wg.Wait()

		if got := read(t, s, key); got != body {
			t.Fatalf("content corrupted by concurrent writes: got %d bytes, want %d", len(got), len(body))
		}
	})
}

func TestListIsScopedToPrefix(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		put(t, s, MustKey(NamespaceBlobs, "sha256:1111"), "a")
		put(t, s, MustKey(NamespaceBlobs, "sha256:2222"), "b")
		put(t, s, MustKey(NamespaceInputs, "sha256:3333"), "c")

		blobs, err := s.List(context.Background(), NamespaceBlobs)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(blobs) != 2 {
			t.Fatalf("listed %d blobs, want 2: %v", len(blobs), blobs)
		}
		for _, info := range blobs {
			if !strings.HasPrefix(info.Key, NamespaceBlobs+"/") {
				t.Fatalf("listing leaked across namespaces: %q", info.Key)
			}
			if info.Size == 0 {
				t.Fatalf("listing reported size 0 for %q", info.Key)
			}
		}
	})
}

// TestListEmptyNamespaceIsNotAnError — a namespace that has never been written to must list as
// empty. If it errored, the first garbage-collection cycle on a fresh install would fail.
func TestListEmptyNamespaceIsNotAnError(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		got, err := s.List(context.Background(), NamespaceInputs)
		if err != nil {
			t.Fatalf("listing an empty namespace failed: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("listed %d objects in an empty namespace", len(got))
		}
	})
}

// TestLargeObjectRoundTrips — these hold real artifact layers, not test strings.
func TestLargeObjectRoundTrips(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceBlobs, "sha256:ee55")
		body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB

		if err := s.Write(context.Background(), key, bytes.NewReader(body)); err != nil {
			t.Fatalf("write: %v", err)
		}
		info, err := s.Stat(context.Background(), key)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Size != int64(len(body)) {
			t.Fatalf("size %d, want %d", info.Size, len(body))
		}
		if got := read(t, s, key); got != string(body) {
			t.Fatal("large object did not round-trip intact")
		}
	})
}

// TestKeyRejectsTraversal — digests come from a CRD field, so they are user input. A key that
// escapes its namespace would let a crafted digest read or write anywhere the process can.
func TestKeyRejectsTraversal(t *testing.T) {
	bad := []string{
		"sha256:../../etc/passwd",
		"sha256:..",
		"../sha256:aaaa",
		"sha256:a/b",
		`sha256:a\b`,
		"noalgorithm",
		"sha256:",
		":abcd",
	}
	for _, digest := range bad {
		t.Run(digest, func(t *testing.T) {
			if k, err := Key(NamespaceBlobs, digest); err == nil {
				t.Fatalf("accepted %q, producing key %q", digest, k)
			}
		})
	}
}

// TestDiskRejectsEscapingKeys — the same guard one level down, where it would actually reach the
// filesystem. A future caller building a key by hand must not get past this.
func TestDiskRejectsEscapingKeys(t *testing.T) {
	root := t.TempDir()
	s, err := NewDisk(root)
	if err != nil {
		t.Fatalf("creating disk store: %v", err)
	}

	for _, key := range []string{"../escape", "blobs/../../escape", "/etc/passwd", ""} {
		t.Run(fmt.Sprintf("%q", key), func(t *testing.T) {
			if err := s.Write(context.Background(), key, strings.NewReader("x")); err == nil {
				t.Fatalf("write accepted escaping key %q", key)
			}
			if _, err := s.Open(context.Background(), key); err == nil {
				t.Fatalf("open accepted escaping key %q", key)
			}
		})
	}
}

// TestDiskIgnoresInFlightWrites — a temp file must never appear in a listing. Garbage collection
// deletes whatever a listing reports as unreferenced, and a blob that is moments from being
// committed and referenced is exactly the wrong thing to delete.
func TestDiskIgnoresInFlightWrites(t *testing.T) {
	root := t.TempDir()
	s, err := NewDisk(root)
	if err != nil {
		t.Fatalf("creating disk store: %v", err)
	}
	put(t, s, MustKey(NamespaceBlobs, "sha256:aaaa"), "committed")

	// Simulate a write in progress by planting a temp file the way Write does.
	inflight := filepath.Join(root, "blobs", "sha256", ".tmp-inflight")
	if err := os.WriteFile(inflight, []byte("half written"), 0o600); err != nil {
		t.Fatalf("planting temp file: %v", err)
	}

	got, err := s.List(context.Background(), NamespaceBlobs)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listing reported %d objects, want only the committed one: %v", len(got), got)
	}
}
