package fetchcontext

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// flaky serves body, but fails the first `failures` requests with the given status.
//
// Returns the URL and a counter of how many requests actually arrived, which is what the
// not-retried assertions read: an error alone cannot tell "refused once" from "refused six times".
func flaky(t *testing.T, failures int32, status int, body []byte) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= failures {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

// TestATransientFailureIsRetried is the outage this retry exists for.
//
// The first dial happens at t=0 of a brand-new pod, before the CNI has programmed NetworkPolicy,
// and the Job runs with BackoffLimit: 0 -- so without a retry one unlucky dial fails the build
// permanently. Asserting the CONTENT as well as the success: each attempt reuses one staged file,
// so a retry that forgot to truncate would hash the concatenation of two responses and fail
// verification in a way that reads like the server serving the wrong bytes.
func TestATransientFailureIsRetried(t *testing.T) {
	blob := tarGz(t, "Dockerfile", "FROM scratch\n")
	url, hits := flaky(t, 2, http.StatusServiceUnavailable, blob)
	dest := filepath.Join(t.TempDir(), "workspace")

	if err := Run(t.Context(), Options{
		Kind: "fetch", URL: url, Digest: digestOf(blob), Unpack: "tar.gz", Dest: dest,
	}); err != nil {
		t.Fatalf("a fetch that failed twice and then succeeded did not recover: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server saw %d requests, want 3", got)
	}
	got, err := os.ReadFile(filepath.Join(dest, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the extracted Dockerfile: %v", err)
	}
	if string(got) != "FROM scratch\n" {
		t.Errorf("Dockerfile = %q; a retry that did not truncate would produce exactly this", got)
	}
}

// TestA404IsNotRetried — the URL, the token or the object is wrong, and five more requests will not
// change any of them. Asserted on the REQUEST COUNT, because the error is identical either way.
func TestA404IsNotRetried(t *testing.T) {
	blob := tarGz(t, "Dockerfile", "FROM scratch\n")
	url, hits := flaky(t, 99, http.StatusNotFound, blob)
	dest := filepath.Join(t.TempDir(), "workspace")

	if err := Run(t.Context(), Options{
		Kind: "fetch", URL: url, Digest: digestOf(blob), Unpack: "tar.gz", Dest: dest,
	}); err == nil {
		t.Fatal("a 404 was treated as success")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server saw %d requests for a 404, want 1", got)
	}
}

// TestADigestMismatchIsNotRetried — a mismatch is terminal, and retrying it would turn a spec error
// into a thirty-second stall before the same failure.
func TestADigestMismatchIsNotRetried(t *testing.T) {
	blob := tarGz(t, "Dockerfile", "FROM scratch\n")
	url, hits := flaky(t, 0, 0, blob)
	dest := filepath.Join(t.TempDir(), "workspace")

	err := Run(t.Context(), Options{
		Kind: "fetch", URL: url, Digest: digestOf([]byte("something else")),
		Unpack: "tar.gz", Dest: dest,
	})
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want a MismatchError, got %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server saw %d requests for a digest mismatch, want 1", got)
	}
}
