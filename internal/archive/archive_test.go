package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func tarball(t *testing.T, gz bool, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w *tar.Writer
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = tar.NewWriter(zw)
	} else {
		w = tar.NewWriter(&buf)
	}
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)),
			Typeflag: flag, Linkname: e.linkname}
		if flag == tar.TypeDir {
			hdr.Size = 0
			hdr.Mode = 0o755
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if flag == tar.TypeReg {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

func TestExtractPlacesFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		gz   bool
		mode Mode
	}{
		{"tar", false, ModeTar},
		{"tar.gz", true, ModeTarGz},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			blob := tarball(t, tc.gz,
				entry{name: "Dockerfile", body: "FROM scratch\n"},
				entry{name: "src/", typeflag: tar.TypeDir},
				entry{name: "src/main.go", body: "package main\n"})

			if err := Extract(bytes.NewReader(blob), tc.mode, dest, "", 0); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got := read(t, dest, "Dockerfile"); got != "FROM scratch\n" {
				t.Errorf("Dockerfile = %q", got)
			}
			if got := read(t, dest, "src/main.go"); got != "package main\n" {
				t.Errorf("src/main.go = %q", got)
			}
		})
	}
}

// TestSubpathStripsThePrefix: naming a directory places its CONTENTS at the root.
func TestSubpathStripsThePrefix(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, true,
		entry{name: "app-1.2.3/", typeflag: tar.TypeDir},
		entry{name: "app-1.2.3/Dockerfile", body: "FROM scratch\n"},
		entry{name: "elsewhere/ignored", body: "no\n"})

	if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "app-1.2.3", 0); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := read(t, dest, "Dockerfile"); got != "FROM scratch\n" {
		t.Errorf("Dockerfile = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "elsewhere")); !os.IsNotExist(err) {
		t.Error("content outside the subpath was extracted")
	}
}

// TestASubpathThatMatchesNothingIsRefused: a typo must not hand BuildKit an empty context.
func TestASubpathThatMatchesNothingIsRefused(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, false, entry{name: "app/Dockerfile", body: "FROM scratch\n"})

	err := Extract(bytes.NewReader(blob), ModeTar, dest, "aap", 0)
	if err == nil {
		t.Fatal("a subpath present in no entry extracted cleanly")
	}
	if !strings.Contains(err.Error(), "aap") {
		t.Errorf("the error does not name the subpath: %v", err)
	}
}

// TestASubpathPresentButEmptyIsAccepted: the directory entry alone proves the subpath exists.
func TestASubpathPresentButEmptyIsAccepted(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, false, entry{name: "app/", typeflag: tar.TypeDir})

	if err := Extract(bytes.NewReader(blob), ModeTar, dest, "app", 0); err != nil {
		t.Fatalf("an empty but present subpath was refused: %v", err)
	}
}

// TestAnArchivesPathsAreThePaths: with no strip, a Flux artifact's root-level and nested entries
// land exactly where the archive puts them (source-controller does not wrap its tree).
func TestAnArchivesPathsAreThePaths(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, true,
		entry{name: "./", typeflag: tar.TypeDir},
		entry{name: "./Dockerfile", body: "FROM scratch\n"},
		entry{name: "ui/Button.tsx", body: "export {}\n"})

	if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", 0); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := read(t, dest, "Dockerfile"); got != "FROM scratch\n" {
		t.Errorf("a root-level file did not survive: Dockerfile = %q", got)
	}
	if got := read(t, dest, "ui/Button.tsx"); got != "export {}\n" {
		t.Errorf("a nested file moved: ui/Button.tsx = %q", got)
	}
}

// TestASubpathSelectsAFluxArtifactsDirectory: a subpath names what the archive actually contains.
func TestASubpathSelectsAFluxArtifactsDirectory(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, true,
		entry{name: "./", typeflag: tar.TypeDir},
		entry{name: "Dockerfile", body: "FROM scratch\n"},
		entry{name: "ui/Button.tsx", body: "export {}\n"})

	if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "ui", 0); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := read(t, dest, "Button.tsx"); got != "export {}\n" {
		t.Errorf("Button.tsx = %q", got)
	}
}

// TestStripComponentsRemovesLeadingLevels: what a release tarball with a wrapper directory needs.
func TestStripComponentsRemovesLeadingLevels(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, true,
		entry{name: "app-1.2.3/", typeflag: tar.TypeDir},
		entry{name: "app-1.2.3/Dockerfile", body: "FROM scratch\n"})

	if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", 1); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := read(t, dest, "Dockerfile"); got != "FROM scratch\n" {
		t.Errorf("Dockerfile = %q", got)
	}
}

// TestStrippingHappensBeforeSubpath: subpath names a path as the build sees it, after stripping.
func TestStrippingHappensBeforeSubpath(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, true,
		entry{name: "app-1.2.3/ui/Button.tsx", body: "export {}\n"},
		entry{name: "app-1.2.3/server/main.go", body: "package main\n"})

	if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "ui", 1); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := read(t, dest, "Button.tsx"); got != "export {}\n" {
		t.Errorf("Button.tsx = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "main.go")); !os.IsNotExist(err) {
		t.Error("content outside the subpath was extracted")
	}
}

// TestStrippingEverythingIsRefused: an emptied tree fails loudly.
func TestStrippingEverythingIsRefused(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, true, entry{name: "Dockerfile", body: "FROM scratch\n"})

	err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", 2)
	if err == nil {
		t.Fatal("stripping past every entry produced an empty tree rather than an error")
	}
	if !strings.Contains(err.Error(), "stripComponents") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestTraversalIsRefused: archive contents are attacker-influenced.
func TestTraversalIsRefused(t *testing.T) {
	for _, name := range []string{"../escape", "../../etc/passwd", "/etc/passwd", "a/../../escape"} {
		t.Run(name, func(t *testing.T) {
			dest := t.TempDir()
			blob := tarball(t, true, entry{name: name, body: "owned\n"})

			err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", 0)
			if err == nil {
				t.Fatalf("entry %q was extracted rather than refused", name)
			}
			if !strings.Contains(err.Error(), "escapes") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
			// Nothing may have been written before the refusal.
			if entries, _ := os.ReadDir(dest); len(entries) != 0 {
				t.Errorf("the destination is not empty after a refusal: %v", entries)
			}
		})
	}
}

// TestSymlinksOutOfTheTreeAreRefused: the entry lands inside the tree; only its target leaves it,
// which a name check alone would miss.
func TestSymlinksOutOfTheTreeAreRefused(t *testing.T) {
	for _, link := range []string{"/etc/passwd", "../../secret"} {
		t.Run(link, func(t *testing.T) {
			dest := t.TempDir()
			blob := tarball(t, true, entry{name: "link", typeflag: tar.TypeSymlink, linkname: link})

			if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", 0); err == nil {
				t.Fatalf("a symlink to %q was created", link)
			}
		})
	}

	t.Run("a link inside the tree is kept", func(t *testing.T) {
		// Windows needs extra privilege to create symlinks. The refusals above run everywhere,
		// because they fail before creating one.
		if !canSymlink(t) {
			t.Skip("this OS does not permit creating symlinks without extra privilege")
		}
		dest := t.TempDir()
		blob := tarball(t, true,
			entry{name: "real", body: "hello\n"},
			entry{name: "link", typeflag: tar.TypeSymlink, linkname: "real"})

		if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", 0); err != nil {
			t.Fatalf("a relative link inside the tree was refused: %v", err)
		}
	})
}

// TestAnUnknownModeIsRefused: fail loudly rather than produce an empty context.
func TestAnUnknownModeIsRefused(t *testing.T) {
	err := Extract(bytes.NewReader(nil), Mode("zip"), t.TempDir(), "", 0)
	if err == nil {
		t.Fatal("an unimplemented mode was accepted")
	}
	if !strings.Contains(err.Error(), "zip") {
		t.Errorf("the error should name the mode: %v", err)
	}
}

// canSymlink reports whether this machine can create one at all.
func canSymlink(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	return os.Symlink("target", filepath.Join(dir, "probe")) == nil
}
