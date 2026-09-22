package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// The conformance suite: every backend runs the same tests, so they are substitutable.

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
		key := MustKey(NamespaceInputs, "sha256:aa11")
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

// TestMissIsErrNotFound: a miss is control flow for callers, so it must be ErrNotFound.
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

// TestDeleteIsIdempotent: deleting an absent key succeeds.
func TestDeleteIsIdempotent(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceInputs, "sha256:bb22")
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

// TestOverwriteWithIdenticalContentSucceeds: two reconciles racing on one key is normal.
func TestOverwriteWithIdenticalContentSucceeds(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceInputs, "sha256:cc33")
		put(t, s, key, "same")
		put(t, s, key, "same")

		if got := read(t, s, key); got != "same" {
			t.Fatalf("content is %q after rewrite, want %q", got, "same")
		}
	})
}

// TestConcurrentWritesDoNotCorrupt: a reader never observes a half-written object.
func TestConcurrentWritesDoNotCorrupt(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceInputs, "sha256:dd44")
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

// TestLargeObjectRoundTrips: these hold real artifact layers.
func TestLargeObjectRoundTrips(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		key := MustKey(NamespaceInputs, "sha256:ee55")
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

// TestKeyRejectsTraversal: digests are user input from a CRD field.
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
			if k, err := Key(NamespaceInputs, digest); err == nil {
				t.Fatalf("accepted %q, producing key %q", digest, k)
			}
		})
	}
}

// TestDiskRejectsEscapingKeys: the same guard where it reaches the filesystem, for keys built by
// hand.
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
