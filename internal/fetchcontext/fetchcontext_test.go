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
	"runtime"
	"strings"
	"testing"
)

// file is one entry for tarGzMany.
type file struct{ name, body string }

// tarGzMany builds a multi-entry archive, which is what a real artifact looks like: several files,
// and a bare "." for the root. tarGz writes one entry and cannot express either.
func tarGzMany(t *testing.T, files ...file) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}
		if strings.HasSuffix(f.name, "/") {
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

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

// TestAReadOnlyParentStillFetches — the staging bug, which only a cluster found.
//
// The download was staged in filepath.Dir(dest). In the build pod dest is /workspace, so that is
// the container root: not writable by uid 1000. Every build WITH a context died on "permission
// denied" while the context-less ones passed, because they never fetch.
//
// A read-only parent is the only thing that reproduces it -- asserting on what is left behind
// cannot, since the staging directory is removed before Run returns either way.
func TestAReadOnlyParentStillFetches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict directory writes on Windows, so this cannot reproduce")
	}
	blob := tarGz(t, "Dockerfile", "FROM scratch\n")
	parent := t.TempDir()
	dest := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if err := Run(t.Context(), Options{
		Kind: "fetch", URL: serve(t, blob), Digest: digestOf(blob),
		Unpack: "tar.gz", Dest: dest,
	}); err != nil {
		t.Fatalf("fetching into a dest whose parent is read-only: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dest, "Dockerfile")); err != nil {
		t.Fatalf("reading the extracted Dockerfile: %v", err)
	}
}

// TestTheStagingDirectoryDoesNotSurvive — whatever the fetcher stages must not reach the build,
// which reads dest as its context.
func TestTheStagingDirectoryDoesNotSurvive(t *testing.T) {
	blob := tarGz(t, "Dockerfile", "FROM scratch\n")
	dest := filepath.Join(t.TempDir(), "workspace")

	if err := Run(t.Context(), Options{
		Kind: "fetch", URL: serve(t, blob), Digest: digestOf(blob),
		Unpack: "tar.gz", Dest: dest,
	}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".fetch-") {
			t.Errorf("staging directory %q was left in the build context", e.Name())
		}
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

// TestAFluxArtifactArrivesUntouched is the regression this file previously asserted backwards.
//
// It used to claim source-controller wraps its tree in an unpredictable directory and that the
// fetcher removes it. It does not wrap: a GitRepository artifact carries a bare "." entry and then
// files at the ROOT. Removing a level therefore dropped every root-level file -- Dockerfile,
// package.json -- and moved every nested path up one, so `subpath: ui` matched nothing. ADR 0045.
//
// The old fixture agreed with the belief, which is why nothing caught it.
func TestAFluxArtifactArrivesUntouched(t *testing.T) {
	blob := tarGzMany(t,
		file{"./", ""},
		file{"Dockerfile", "FROM scratch\n"},
		file{"ui/Button.tsx", "export {}\n"})
	dest := filepath.Join(t.TempDir(), "workspace")

	err := Run(t.Context(), Options{
		Kind: "sourceRef", URL: serve(t, blob), Digest: digestOf(blob),
		Unpack: "tar.gz", Dest: dest,
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Dockerfile")); err != nil {
		t.Errorf("a root-level file was dropped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "ui", "Button.tsx")); err != nil {
		t.Errorf("a nested path was moved: %v", err)
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
