package oci

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipEntry describes one entry for the fixture builder.
type zipEntry struct {
	name string
	body string
	link string      // non-empty makes it a symlink; the target is stored as the body
	mode fs.FileMode // zero means 0o644, or 0o755|ModeDir for a trailing-slash name
	// dosOnly writes the entry with an MS-DOS creator and no unix mode, the way a zip made on
	// Windows arrives.
	dosOnly bool
	// method overrides the compression method; zero means Deflate.
	method uint16
	store  bool
}

// buildZip assembles an archive in memory, in the given order.
//
// SetMode is what records a unix creator version in the header, which is what makes f.Mode()
// decode permissions and the symlink bit on the way back out — so symlinks are testable here,
// unlike the bzip2 branch in compress.go.
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.store {
			hdr.Method = zip.Store
		}
		if e.method != 0 {
			hdr.Method = e.method
		}

		switch {
		case e.dosOnly:
			// CreatorVersion stays 0 (FAT), so no unix mode is recorded at all.
		case e.link != "":
			hdr.SetMode(0o777 | fs.ModeSymlink)
		case e.mode != 0:
			hdr.SetMode(e.mode)
		case strings.HasSuffix(e.name, "/"):
			hdr.SetMode(0o755 | fs.ModeDir)
		default:
			hdr.SetMode(0o644)
		}

		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("creating entry %q: %v", e.name, err)
		}
		body := e.body
		if e.link != "" {
			body = e.link
		}
		if body != "" {
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatalf("writing entry %q: %v", e.name, err)
			}
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes()
}

// writeZip puts a built archive on disk, since extractZip takes a path.
func writeZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.zip")
	if err := os.WriteFile(p, buildZip(t, entries), 0o600); err != nil {
		t.Fatalf("writing zip: %v", err)
	}
	return p
}

// TestExtractZipMapsEntryKinds — zip has no typeflag, so every kind is inferred from a mode word
// and a trailing slash. The symlink assertion is the one that matters: a symlink is an ordinary
// entry whose body is the link target, so reading entries as files produces a layer that looks
// completely plausible and is wrong in a way nothing downstream can detect.
func TestExtractZipMapsEntryKinds(t *testing.T) {
	src := writeZip(t, []zipEntry{
		{name: "lib/"},
		{name: "lib/data.txt", body: "plain", mode: 0o644},
		{name: "bin/tool", body: "ELF", mode: 0o755},
		{name: "lib/liblua.so", link: "../real/liblua.so.0"},
		{name: "dev/pipe", mode: 0o644 | fs.ModeNamedPipe},
	})

	entries, err := extractZip(src, "", "")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	got := byName(entries)

	if e, ok := got["lib/data.txt"]; !ok || string(e.body) != "plain" || e.mode != 0o644 {
		t.Errorf("regular file wrong: %+v", e)
	}
	if e, ok := got["bin/tool"]; !ok || e.mode != 0o755 {
		t.Errorf("executable did not keep its exec bit: %+v", e)
	}
	if e, ok := got["lib"]; !ok || !e.dir {
		t.Errorf("directory entry wrong: %+v", e)
	}

	link, ok := got["lib/liblua.so"]
	if !ok {
		t.Fatal("symlink missing entirely")
	}
	if link.link != "../real/liblua.so.0" {
		t.Errorf("link target = %q, want %q", link.link, "../real/liblua.so.0")
	}
	if len(link.body) != 0 {
		t.Errorf("symlink kept a body of %q; the target belongs in link, not the body", link.body)
	}

	if _, ok := got["dev/pipe"]; ok {
		t.Error("a fifo reached the layer")
	}
}

// TestExtractZipSynthesisesParentDirs — plenty of writers store no directory entries at all, and a
// tar whose files have no parents is not reliably extractable.
func TestExtractZipSynthesisesParentDirs(t *testing.T) {
	entries, err := extractZip(writeZip(t, []zipEntry{
		{name: "a/b/c.txt", body: "x"},
	}), "", "")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	got := byName(entries)
	for _, want := range []string{"a", "a/b"} {
		if e, ok := got[want]; !ok || !e.dir {
			t.Errorf("parent directory %q was not synthesised", want)
		}
	}

	// An explicit directory entry alongside the synthesised one must not double up.
	entries, err = extractZip(writeZip(t, []zipEntry{
		{name: "a/"},
		{name: "a/b/"},
		{name: "a/b/c.txt", body: "x"},
	}), "", "")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	counts := make(map[string]int)
	for _, e := range entries {
		counts[e.name]++
	}
	for name, n := range counts {
		if n != 1 {
			t.Errorf("entry %q appears %d times, want once", name, n)
		}
	}
}

// TestExtractZipNormalisesBackslashSeparators — the format specifies "/", but zips written on
// Windows do arrive with "\", and treating it as a literal filename character produces one junk
// file instead of a directory tree.
func TestExtractZipNormalisesBackslashSeparators(t *testing.T) {
	entries, err := extractZip(writeZip(t, []zipEntry{
		{name: `dir\sub\file.txt`, body: "x"},
	}), "", "")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	got := byName(entries)
	if _, ok := got["dir/sub/file.txt"]; !ok {
		t.Errorf("backslashes were not normalised, got %v", got)
	}
}

// TestExtractZipRefusesTraversal — the backslash cases are the point. Normalising separators has to
// happen BEFORE the traversal check, or "..\..\etc\passwd" reads as a single odd filename and
// sails past a guard that only looks for "../".
func TestExtractZipRefusesTraversal(t *testing.T) {
	for _, name := range []string{
		"../etc/passwd",
		"a/../../etc/passwd",
		"..",
		`..\..\etc\passwd`,
		`..\`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := extractZip(writeZip(t, []zipEntry{{name: name, body: "x"}}), "opt", "")
			if err == nil {
				t.Fatalf("entry %q was accepted; it escapes the target directory", name)
			}
			if !strings.Contains(err.Error(), "escapes the target directory") {
				t.Errorf("error %q does not name the escape", err)
			}
		})
	}
}

// TestExtractZipRefusesDuplicateNames — duplicates are legal in a zip and common in repacked jars.
// Picking one silently would mean the layer depended on which, so this refuses; directory repeats
// stay ordinary.
func TestExtractZipRefusesDuplicateNames(t *testing.T) {
	_, err := extractZip(writeZip(t, []zipEntry{
		{name: "a.txt", body: "first"},
		{name: "a.txt", body: "second"},
	}), "", "")
	if err == nil {
		t.Fatal("a duplicate file name was accepted")
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error %q does not explain the duplicate", err)
	}

	if _, err := extractZip(writeZip(t, []zipEntry{
		{name: "d/"},
		{name: "d/"},
		{name: "d/x", body: "y"},
	}), "", ""); err != nil {
		t.Errorf("a repeated directory entry was refused: %v", err)
	}
}

// TestExtractZipRefusesEncryptedEntries — archive/zip neither supports encryption nor checks the
// flag, so without this an encrypted entry becomes a layer full of ciphertext.
func TestExtractZipRefusesEncryptedEntries(t *testing.T) {
	raw := buildZip(t, []zipEntry{{name: "secret.txt", body: "x"}})
	// Set general-purpose bit 0 in the local header and in the central directory. Both carry the
	// flags; archive/zip reads the central directory, so patching every occurrence is simplest.
	patched := bytes.ReplaceAll(raw, []byte("PK\x03\x04"), []byte("PK\x03\x04"))
	idx := 0
	for {
		i := bytes.Index(patched[idx:], []byte("PK\x01\x02"))
		if i < 0 {
			break
		}
		// Central directory header: flags live at offset 8.
		patched[idx+i+8] |= zipEncryptedFlag
		idx += i + 4
	}

	p := filepath.Join(t.TempDir(), "enc.zip")
	if err := os.WriteFile(p, patched, 0o600); err != nil {
		t.Fatalf("writing zip: %v", err)
	}

	_, err := extractZip(p, "", "")
	if err == nil {
		t.Fatal("an encrypted entry was accepted")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q does not mention encryption", err)
	}
}

// TestExtractZipRefusesUnsupportedMethods — LZMA and friends exist in the wild (7-Zip and some
// .NET writers emit them); the message should name the method rather than surfacing a bare library
// error.
//
// The fixture is patched rather than written, because archive/zip cannot WRITE these methods
// either — the same limitation the bzip2 branch in compress.go has. Only the method field needs to
// be plausible: the refusal happens on Open, before any payload is decompressed.
func TestExtractZipRefusesUnsupportedMethods(t *testing.T) {
	const methodLZMA = 14
	raw := buildZip(t, []zipEntry{{name: "a.txt", body: "x", store: true}})

	patch := func(sig []byte, methodOffset int) {
		for i := 0; ; {
			j := bytes.Index(raw[i:], sig)
			if j < 0 {
				return
			}
			at := i + j + methodOffset
			raw[at], raw[at+1] = methodLZMA, 0
			i += j + 4
		}
	}
	// Local file header: signature, version, flags, then method at offset 8.
	patch([]byte("PK\x03\x04"), 8)
	// Central directory header carries an extra "version made by", so method sits at offset 10.
	// This is the one archive/zip actually reads.
	patch([]byte("PK\x01\x02"), 10)

	p := filepath.Join(t.TempDir(), "lzma.zip")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("writing zip: %v", err)
	}

	_, err := extractZip(p, "", "")
	if err == nil {
		t.Fatal("an LZMA entry was accepted")
	}
	if !strings.Contains(err.Error(), "compression method") {
		t.Errorf("error %q does not name the method", err)
	}
}

// TestExtractZipNormalisesWindowsModes — a zip made on Windows records no unix permissions, so
// archive/zip reports 0666 and normaliseMode lands everything at 0644, executables included.
//
// This is pinned deliberately rather than worked around. It is fully reproducible — the input is
// digest-pinned — just surprising, and the fix belongs in the spec (mode: {file: "0755"}) rather
// than in a heuristic here, since sniffing content would make the output depend on something other
// than the declared spec.
func TestExtractZipNormalisesWindowsModes(t *testing.T) {
	entries, err := extractZip(writeZip(t, []zipEntry{
		{name: "tool.exe", body: "MZ", dosOnly: true},
	}), "", "")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	e, ok := byName(entries)["tool.exe"]
	if !ok {
		t.Fatal("entry missing")
	}
	if e.mode != 0o644 {
		t.Errorf("mode = %o, want 0644: a DOS-authored zip carries no exec bit", e.mode)
	}
}

// TestExtractZipSubpathAndTarget — the composition users actually write: strip a version-named
// wrapper directory and land its contents somewhere specific.
func TestExtractZipSubpathAndTarget(t *testing.T) {
	src := writeZip(t, []zipEntry{
		{name: "plugin-1.2.3/"},
		{name: "plugin-1.2.3/lib/a.jar", body: "aaa"},
		{name: "plugin-1.2.3/README", body: "docs"},
		{name: "other/ignored", body: "no"},
	})

	entries, err := extractZip(src, "opt/plugins", "plugin-1.2.3")
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	got := byName(entries)
	if _, ok := got["opt/plugins/lib/a.jar"]; !ok {
		t.Errorf("subpath contents did not land at the target, got %v", got)
	}
	for name := range got {
		if strings.Contains(name, "plugin-1.2.3") {
			t.Errorf("entry %q kept the stripped prefix", name)
		}
		if strings.Contains(name, "ignored") {
			t.Errorf("entry %q came from outside the subpath", name)
		}
	}

	// A subpath matching nothing is a stall, not a silently empty layer.
	if _, err := extractZip(src, "opt", "nope"); err == nil {
		t.Error("a subpath matching nothing was accepted")
	}
}

// TestAssembleUnpackZip wires the mode through the layer path the reconciler actually uses, rather
// than testing extractZip alone — collectEntries' switch is where a new mode gets forgotten.
func TestAssembleUnpackZip(t *testing.T) {
	src := writeZip(t, []zipEntry{
		{name: "plugin-1.2.3/lib/a.jar", body: "aaa"},
	})

	entries := entriesOf(t, []LayerInput{{
		Name: "plugin", Path: src, Unpack: UnpackZip, Subpath: "plugin-1.2.3", Target: "/opt/plugins",
	}}, Config{})

	for _, e := range entries {
		if e.Name == "opt/plugins/lib/a.jar" {
			return
		}
	}
	t.Errorf("zip payload did not reach the layer, got %v", entries)
}

// TestAssembleUnpackZipIsDeterministic is the load-bearing test for this format.
//
// The first half is the obvious property: the same archive assembles to the same digest. The second
// is the one that matters, because zip carries far more incidental variation than tar — entry
// order, compression method, timestamps, redundant directory entries and permission bits nobody
// looks at. Two archives with the same logical content must produce the same layer, or the whole
// premise that the output is a function of the content fails for this format.
func TestAssembleUnpackZipIsDeterministic(t *testing.T) {
	digestOf := func(src string) string {
		t.Helper()
		img, err := Assemble(nil, []LayerInput{{
			Name: "plugin", Path: src, Unpack: UnpackZip, Target: "/opt",
		}}, Config{}, t.TempDir())
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return d.String()
	}

	plain := []zipEntry{
		{name: "lib/a.jar", body: "aaa", mode: 0o644},
		{name: "bin/tool", body: "ELF", mode: 0o755},
	}

	first := digestOf(writeZip(t, plain))
	for i := range 3 {
		if got := digestOf(writeZip(t, plain)); got != first {
			t.Fatalf("repeat %d produced %s, want %s", i, got, first)
		}
	}

	// Same content, deliberately different packing: reversed order, Store instead of Deflate, an
	// extra directory entry, and setuid/sticky bits on top of the same permissions.
	varied := []zipEntry{
		{name: "bin/", store: true},
		{name: "bin/tool", body: "ELF", mode: 0o755 | fs.ModeSetuid, store: true},
		{name: "lib/a.jar", body: "aaa", mode: 0o644 | fs.ModeSticky, store: true},
	}
	if got := digestOf(writeZip(t, varied)); got != first {
		t.Errorf("a differently packed zip with the same content produced %s, want %s", got, first)
	}
}
