package serve

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/lhns/kube-oci-composer/internal/store"
)

var testHash = v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("ab", 32)}

func seed(t *testing.T, s store.Store, h v1.Hash, body string) {
	t.Helper()
	k := store.NamespaceBlobs + "/" + h.Algorithm + "/" + h.Hex
	if err := s.Write(context.Background(), k, strings.NewReader(body)); err != nil {
		t.Fatalf("seeding blob: %v", err)
	}
}

func TestBlobHandlerRoundTrip(t *testing.T) {
	s := store.NewMemory()
	h := NewBlobHandler(s, false)
	seed(t, s, testHash, "layer bytes")

	size, err := h.Stat(context.Background(), "any/repo", testHash)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if size != int64(len("layer bytes")) {
		t.Fatalf("size %d, want %d", size, len("layer bytes"))
	}

	rc, err := h.Get(context.Background(), "any/repo", testHash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "layer bytes" {
		t.Fatalf("read %q", got)
	}
}

// TestMissIsRegistryErrNotFound — pkg/registry turns registry.ErrNotFound into a 404 and anything
// else into a 500. Getting this wrong means a missing blob reads to the client as a broken
// server, and containerd retries a permanent condition.
func TestMissIsRegistryErrNotFound(t *testing.T) {
	h := NewBlobHandler(store.NewMemory(), false)

	if _, err := h.Stat(context.Background(), "repo", testHash); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Stat returned %v, want registry.ErrNotFound", err)
	}
	if _, err := h.Get(context.Background(), "repo", testHash); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Get returned %v, want registry.ErrNotFound", err)
	}
}

// TestBlobsAreSharedAcrossRepositories — the repo argument is ignored on purpose. The same digest
// is the same bytes, and keying by repo would store a shared layer once per composition.
func TestBlobsAreSharedAcrossRepositories(t *testing.T) {
	s := store.NewMemory()
	h := NewBlobHandler(s, false)

	if err := h.Put(context.Background(), "repo-a", testHash,
		io.NopCloser(strings.NewReader("shared"))); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := h.Stat(context.Background(), "repo-b", testHash); err != nil {
		t.Fatalf("a blob written under one repo was not visible under another: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("stored %d objects for one digest, want 1", s.Len())
	}
}

func TestDeleteRemovesTheBlob(t *testing.T) {
	s := store.NewMemory()
	h := NewBlobHandler(s, false)
	seed(t, s, testHash, "bytes")

	if err := h.Delete(context.Background(), "repo", testHash); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := h.Stat(context.Background(), "repo", testHash); !errors.Is(err, registry.ErrNotFound) {
		t.Fatal("blob survived deletion")
	}
	// Idempotent, because a garbage-collection sweep is not a transaction.
	if err := h.Delete(context.Background(), "repo", testHash); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// TestPutClosesTheReader — pkg/registry hands over a ReadCloser wrapping the request body. Not
// closing it leaks a connection per upload.
func TestPutClosesTheReader(t *testing.T) {
	s := store.NewMemory()
	h := NewBlobHandler(s, false)

	rc := &trackingReadCloser{Reader: strings.NewReader("bytes")}
	if err := h.Put(context.Background(), "repo", testHash, rc); err != nil {
		t.Fatalf("put: %v", err)
	}
	if !rc.closed {
		t.Fatal("Put did not close the reader it was handed")
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (t *trackingReadCloser) Close() error { t.closed = true; return nil }

// presigningStore is a Store that can hand out URLs.
type presigningStore struct {
	*store.Memory
	url string
	err error
}

func (p presigningStore) Presign(ctx context.Context, key string, _ time.Duration) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	if _, err := p.Stat(ctx, key); err != nil {
		return "", err
	}
	return p.url, nil
}

// TestPresignRedirectsGetButNotStat is the split that makes presigning usable.
//
// Stat answers HEAD, where a client is asking whether a blob exists and how big it is — a
// redirect there forces a round trip to object storage to answer a question the controller
// already knows. GET is where the bytes are, so GET is where the redirect belongs.
func TestPresignRedirectsGetButNotStat(t *testing.T) {
	mem := store.NewMemory()
	s := presigningStore{Memory: mem, url: "https://s3.example.com/presigned"}
	h := NewBlobHandler(s, true)
	seed(t, mem, testHash, "layer bytes")

	size, err := h.Stat(context.Background(), "repo", testHash)
	if err != nil {
		t.Fatalf("Stat should answer directly, got %v", err)
	}
	if size != int64(len("layer bytes")) {
		t.Fatalf("Stat returned size %d", size)
	}

	_, err = h.Get(context.Background(), "repo", testHash)
	var redirect registry.RedirectError
	if !errors.As(err, &redirect) {
		t.Fatalf("Get returned %v, want a RedirectError", err)
	}
	if redirect.Location != s.url {
		t.Fatalf("redirect location %q", redirect.Location)
	}
	// 307 preserves the method; some clients downgrade a 302 to GET.
	if redirect.Code != http.StatusTemporaryRedirect {
		t.Fatalf("redirect code %d, want %d", redirect.Code, http.StatusTemporaryRedirect)
	}
}

// TestPresignFailureFallsBackToStreaming — a presigning failure is a reason to be slow, not a
// reason to fail a pull.
func TestPresignFailureFallsBackToStreaming(t *testing.T) {
	mem := store.NewMemory()
	s := presigningStore{Memory: mem, err: errors.New("signing key unavailable")}
	h := NewBlobHandler(s, true)
	seed(t, mem, testHash, "layer bytes")

	rc, err := h.Get(context.Background(), "repo", testHash)
	if err != nil {
		t.Fatalf("Get failed instead of falling back: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "layer bytes" {
		t.Fatalf("fallback served %q", got)
	}
}

// TestPresignMissIsStillNotFound — a missing blob must 404 rather than handing the client a
// presigned URL that will 404 later, which reads as a corrupt registry instead of a clean miss.
func TestPresignMissIsStillNotFound(t *testing.T) {
	s := presigningStore{Memory: store.NewMemory(), url: "https://s3.example.com/presigned"}
	h := NewBlobHandler(s, true)

	if _, err := h.Get(context.Background(), "repo", testHash); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Get returned %v, want registry.ErrNotFound", err)
	}
}

// TestPresignIgnoredWhenBackendCannot — asking for presigning against a plain disk store must
// quietly serve the bytes rather than failing every pull.
func TestPresignIgnoredWhenBackendCannot(t *testing.T) {
	s := store.NewMemory()
	h := NewBlobHandler(s, true)
	if h.Presign != nil {
		t.Fatal("presigning was enabled on a backend that does not support it")
	}
	seed(t, s, testHash, "bytes")

	rc, err := h.Get(context.Background(), "repo", testHash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rc.Close()
}
