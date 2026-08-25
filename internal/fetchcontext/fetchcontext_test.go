package fetchcontext

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarGz(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func serve(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAVerifiedArchiveIsExtracted(t *testing.T) {
	blob := tarGz(t, "Dockerfile", "FROM scratch\n")
	dest := filepath.Join(t.TempDir(), "workspace")

	err := Run(t.Context(), Options{
		Kind: "fetch", URL: serve(t, blob), Digest: digestOf(blob),
		Unpack: "tar.gz", Dest: dest,
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the extracted Dockerfile: %v", err)
	}
	if string(got) != "FROM scratch\n" {
		t.Errorf("Dockerfile = %q", got)
	}
}

// TestAMismatchLeavesNothingBehind is the assertion that makes "verify before unpack" real.
//
// Asserting only that an error came back would pass just as well if the archive had been extracted
// and THEN checked — at which point a build could already read content nothing vouched for, and the
// declared digest would be decorative.
func TestAMismatchLeavesNothingBehind(t *testing.T) {
	blob := tarGz(t, "Dockerfile", "FROM scratch\n")
	dest := filepath.Join(t.TempDir(), "workspace")

	err := Run(t.Context(), Options{
		Kind: "fetch", URL: serve(t, blob), Digest: "sha256:" + strings.Repeat("0", 64),
		Unpack: "tar.gz", Dest: dest,
	})
	if err == nil {
		t.Fatal("a wrong digest was accepted")
	}
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("the error is not distinguishable as a mismatch, so it cannot be reported as "+
			"a spec problem: %v", err)
	}

	entries, err := os.ReadDir(dest)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".blob") {
			t.Errorf("the destination holds %q after a mismatch; the archive was unpacked before "+
				"it was verified", e.Name())
		}
	}
}

// TestNoDigestIsRefused. The Flux path fetched with no verification at all before this existed,
// which is the gap this closes rather than a compatibility case to preserve.
func TestNoDigestIsRefused(t *testing.T) {
	err := Run(t.Context(), Options{Kind: "sourceRef", URL: "https://example.invalid/x.tgz",
		Unpack: "tar.gz", Dest: t.TempDir()})
	if err == nil {
		t.Fatal("a context with no digest was accepted")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("the error should say what is missing: %v", err)
	}
}

// TestAFluxArtifactHasItsWrapperStripped — source-controller wraps the tree in one directory whose
// name nobody can predict, and buildctl looks for the Dockerfile at the root.
func TestAFluxArtifactHasItsWrapperStripped(t *testing.T) {
	blob := tarGz(t, "src-abc123/Dockerfile", "FROM scratch\n")
	dest := filepath.Join(t.TempDir(), "workspace")

	err := Run(t.Context(), Options{
		Kind: "sourceRef", URL: serve(t, blob), Digest: digestOf(blob),
		Unpack: "tar.gz", Dest: dest,
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Dockerfile")); err != nil {
		t.Errorf("the wrapper directory was not stripped: %v", err)
	}
}

// TestAFetchedArchiveKeepsItsLayout is the other half.
//
// A fetched tarball is whatever the publisher made it, so stripping a segment would silently
// discard a real top-level directory. `subpath` is how a version-named wrapper is named there.
func TestAFetchedArchiveKeepsItsLayout(t *testing.T) {
	blob := tarGz(t, "app-1.2.3/Dockerfile", "FROM scratch\n")
	dest := filepath.Join(t.TempDir(), "workspace")

	err := Run(t.Context(), Options{
		Kind: "fetch", URL: serve(t, blob), Digest: digestOf(blob),
		Unpack: "tar.gz", Dest: dest,
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "app-1.2.3", "Dockerfile")); err != nil {
		t.Errorf("a fetched archive lost its top-level directory: %v", err)
	}
}
