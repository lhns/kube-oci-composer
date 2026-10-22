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

// tarGzMany builds a multi-entry archive, including directory entries such as "./".
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

// tarGz builds a single-file archive.
func tarGz(t *testing.T, name, body string) []byte {
	t.Helper()
	return tarGzMany(t, file{name, body})
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

// TestAReadOnlyParentStillFetches: in the build pod dest's parent is the container root, which uid
// 1000 cannot write, so nothing may be staged there.
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

// TestTheStagingDirectoryDoesNotSurvive into dest, which the build reads as its context.
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

// TestAMismatchLeavesNothingBehind: verify before unpack, or the declared digest is decorative.
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

// TestNoDigestIsRefused, for the Flux path too.
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

// TestAFluxArtifactArrivesUntouched: a GitRepository artifact has a bare "." entry and files at the
// root, not a wrapper directory, so nothing may be stripped. ADR 0045.
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

// TestAFetchedArchiveKeepsItsLayout: nothing is stripped unless the spec says so; `subpath` selects
// a wrapper directory.
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
