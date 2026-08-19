package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/lhns/kube-oci-composer/internal/store"
)

// A resumed pull is the case this exists for: containerd asks for `bytes=N-` when continuing an
// interrupted layer download, upstream's Sscanf cannot parse that, and the answer was a permanent
// 416 rather than the rest of the blob.
//
// Driven through the real server so it covers the wiring as well as the parsing — a unit test over
// the helper alone would still pass if the middleware were never installed.
func TestResumingABlobDownloadServesTheRest(t *testing.T) {
	body := []byte(strings.Repeat("layer-bytes-", 64))
	srv, digest := serverWithBlob(t, body)

	const offset = 100
	rec := get(t, srv, "/v2/team/app/blobs/"+digest, fmt.Sprintf("bytes=%d-", offset))

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("resumed download answered %d, want 206; an interrupted pull can never finish",
			rec.Code)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, body[offset:]) {
		t.Errorf("served %d bytes from offset %d, want %d", len(got), offset, len(body)-offset)
	}
	want := fmt.Sprintf("bytes %d-%d/%d", offset, len(body)-1, len(body))
	if got := rec.Header().Get("Content-Range"); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
}

// The closed form upstream already understands must keep working untouched.
func TestClosedRangeIsUnchanged(t *testing.T) {
	body := []byte(strings.Repeat("x", 500))
	srv, digest := serverWithBlob(t, body)

	rec := get(t, srv, "/v2/team/app/blobs/"+digest, "bytes=10-19")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("code = %d, want 206", rec.Code)
	}
	if got := rec.Body.Len(); got != 10 {
		t.Errorf("served %d bytes, want 10", got)
	}
}

// A whole-blob GET must not acquire a Range header just because the middleware is in the path.
func TestUnrangedGetIsUnaffected(t *testing.T) {
	body := []byte("small")
	srv, digest := serverWithBlob(t, body)

	rec := get(t, srv, "/v2/team/app/blobs/"+digest, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
}

// Shapes the rewrite must decline, so it cannot invent behaviour for cases nothing exercises.
func TestOpenEndedParsingIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		header string
		start  int64
		ok     bool
	}{
		{"bytes=0-", 0, true},
		{"bytes=1024-", 1024, true},
		{"bytes=10-20", 0, false}, // closed: upstream handles it
		{"bytes=-500", 0, false},  // suffix: upstream rejects, containerd does not send it
		{"bytes=0-,5-", 0, false}, // multi-range: upstream cannot serve it
		{"items=0-", 0, false},    // not a byte range
		{"bytes=-", 0, false},     // no start
		{"bytes=abc-", 0, false},  // not a number
		{"", 0, false},            // absent
	} {
		got, ok := parseOpenEnded(tc.header)
		if ok != tc.ok || (ok && got != tc.start) {
			t.Errorf("parseOpenEnded(%q) = (%d, %v), want (%d, %v)", tc.header, got, ok, tc.start, tc.ok)
		}
	}
}

// An offset past the end is left for the handler to reject rather than rewritten into nonsense.
func TestOffsetBeyondTheBlobIsNotRewritten(t *testing.T) {
	body := []byte("0123456789")
	srv, digest := serverWithBlob(t, body)

	rec := get(t, srv, "/v2/team/app/blobs/"+digest, "bytes=99-")
	if rec.Code == http.StatusPartialContent {
		t.Errorf("an offset past the blob returned 206 with %d bytes", rec.Body.Len())
	}
}

func serverWithBlob(t *testing.T, body []byte) (*Server, string) {
	t.Helper()
	disk, err := store.NewDisk(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv, err := New("oci.test", ":5000", disk, false)
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	h, err := v1.NewHash(digest)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	bh := NewBlobHandler(disk, false)
	if err := bh.Put(context.Background(), "team/app", h, io.NopCloser(bytes.NewReader(body))); err != nil {
		t.Fatalf("storing blob: %v", err)
	}
	return srv, digest
}

func get(t *testing.T, srv *Server, path, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
