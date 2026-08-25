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

			if err := Extract(bytes.NewReader(blob), tc.mode, dest, "", false); err != nil {
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

// TestSubpathStripsThePrefix — a release tarball wraps its tree in a version-named directory, and
// naming it must leave the CONTENTS at the root rather than the directory itself.
func TestSubpathStripsThePrefix(t *testing.T) {
	dest := t.TempDir()
	blob := tarball(t, true,
		entry{name: "app-1.2.3/", typeflag: tar.TypeDir},
		entry{name: "app-1.2.3/Dockerfile", body: "FROM scratch\n"},
		entry{name: "elsewhere/ignored", body: "no\n"})

	if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "app-1.2.3", false); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got := read(t, dest, "Dockerfile"); got != "FROM scratch\n" {
		t.Errorf("Dockerfile = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "elsewhere")); !os.IsNotExist(err) {
		t.Error("content outside the subpath was extracted")
	}
}

// TestStripWrapperOnlyStripsARealWrapper.
//
// The rule that has to match build.MatchesContextPath. When the two disagreed, an unpinned FROM was
// correctly refused and every build that passed the check then failed inside BuildKit — so the
// negative case matters as much as the positive one: an archive whose files sit at the root must
// still arrive intact rather than being silently emptied.
func TestStripWrapperOnlyStripsARealWrapper(t *testing.T) {
	t.Run("a wrapper is stripped", func(t *testing.T) {
		dest := t.TempDir()
		blob := tarball(t, true,
			entry{name: "src-abc123/", typeflag: tar.TypeDir},
			entry{name: "src-abc123/Dockerfile", body: "FROM scratch\n"})

		if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", true); err != nil {
			t.Fatalf("extract: %v", err)
		}
		if got := read(t, dest, "Dockerfile"); got != "FROM scratch\n" {
			t.Errorf("Dockerfile = %q", got)
		}
	})

	t.Run("files at the root survive", func(t *testing.T) {
		dest := t.TempDir()
		blob := tarball(t, true, entry{name: "./Dockerfile", body: "FROM scratch\n"})

		if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", false); err != nil {
			t.Fatalf("extract: %v", err)
		}
		if got := read(t, dest, "Dockerfile"); got != "FROM scratch\n" {
			t.Errorf("Dockerfile = %q", got)
		}
	})
}

// TestTraversalIsRefused — a build context is attacker-influenced by definition, since it is
// whatever the referenced archive happens to contain.
func TestTraversalIsRefused(t *testing.T) {
	for _, name := range []string{"../escape", "../../etc/passwd", "/etc/passwd", "a/../../escape"} {
		t.Run(name, func(t *testing.T) {
			dest := t.TempDir()
			blob := tarball(t, true, entry{name: name, body: "owned\n"})

			err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", false)
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

// TestSymlinksOutOfTheTreeAreRefused — the traversal check on names alone would miss this, because
// the entry itself lands inside the tree and only what it POINTS AT leaves it.
func TestSymlinksOutOfTheTreeAreRefused(t *testing.T) {
	for _, link := range []string{"/etc/passwd", "../../secret"} {
		t.Run(link, func(t *testing.T) {
			dest := t.TempDir()
			blob := tarball(t, true, entry{name: "link", typeflag: tar.TypeSymlink, linkname: link})

			if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", false); err == nil {
				t.Fatalf("a symlink to %q was created", link)
			}
		})
	}

	t.Run("a link inside the tree is kept", func(t *testing.T) {
		// Creating a symlink needs a privilege Windows does not grant by default, so this asserts
		// the code path only where the OS allows it. The REFUSAL cases above run everywhere,
		// because they fail before any symlink is created.
		if !canSymlink(t) {
			t.Skip("this OS does not permit creating symlinks without extra privilege")
		}
		dest := t.TempDir()
		blob := tarball(t, true,
			entry{name: "real", body: "hello\n"},
			entry{name: "link", typeflag: tar.TypeSymlink, linkname: "real"})

		if err := Extract(bytes.NewReader(blob), ModeTarGz, dest, "", false); err != nil {
			t.Fatalf("a relative link inside the tree was refused: %v", err)
		}
	})
}

// TestAnUnknownModeIsRefused — failing loudly rather than producing an empty directory, which would
// look like a build whose context simply had nothing in it.
func TestAnUnknownModeIsRefused(t *testing.T) {
	err := Extract(bytes.NewReader(nil), Mode("zip"), t.TempDir(), "", false)
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
