package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// tarFile is one entry to place in a fixture tar. Shared with the tar and zip tests, not only the
// deb ones — a deb's payload is an ordinary tar, which is the whole reason this fixture generalises.
type tarFile struct {
	name string // verbatim, so a fixture can carry dpkg's "./" prefix or anything else
	body string
	link string // non-empty makes it a symlink
	dir  bool
}

// buildTar writes a tar of the given entries, in the given order.
func buildTar(t *testing.T, files []tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: 0o644}
		switch {
		case f.dir:
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
		case f.link != "":
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, f.link
		default:
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(f.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", f.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(f.body)); err != nil {
				t.Fatalf("write body %q: %v", f.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// compressData applies a data.tar suffix. No .bz2: the standard library decodes bzip2 but cannot
// write it, which is why debDecompress's .bz2 branch has no test.
func compressData(t *testing.T, raw []byte, suffix string) []byte {
	t.Helper()
	if suffix == "" {
		return raw
	}
	var out bytes.Buffer
	var zw io.WriteCloser
	var err error
	switch suffix {
	case ".gz":
		zw = gzip.NewWriter(&out)
	case ".xz":
		zw, err = xz.NewWriter(&out)
	case ".zst":
		zw, err = zstd.NewWriter(&out)
	default:
		t.Fatalf("fixture cannot produce %q", suffix)
	}
	if err != nil {
		t.Fatalf("%s writer: %v", suffix, err)
	}
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("%s write: %v", suffix, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("%s close: %v", suffix, err)
	}
	return out.Bytes()
}

// arMember appends one ar member, including the odd-size padding byte.
func arMember(buf *bytes.Buffer, name string, body []byte) {
	fmt.Fprintf(buf, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(body))
	buf.Write(body)
	if len(body)%2 == 1 {
		buf.WriteByte('\n')
	}
}

// buildDeb assembles a package with the three members dpkg emits, in dpkg's order.
func buildDeb(t *testing.T, suffix string, files []tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	// Deliberately odd-sized, so every test exercises the padding byte: get that wrong and every
	// subsequent member header is off by one.
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, "control.tar"+suffix, compressData(t, buildTar(t, []tarFile{
		{name: "./control", body: "Package: fixture\n"},
	}), suffix))
	arMember(&buf, "data.tar"+suffix, compressData(t, buildTar(t, files), suffix))
	return buf.Bytes()
}

// byName indexes extracted entries for assertions.
func byName(entries []tarEntry) map[string]tarEntry {
	out := make(map[string]tarEntry, len(entries))
	for _, e := range entries {
		out[e.name] = e
	}
	return out
}

// TestExtractDebTakesOnlyTheDataMember — control.tar is a tar as well, so reading the wrong
// member yields a plausible-looking layer full of packaging metadata rather than an error.
func TestExtractDebTakesOnlyTheDataMember(t *testing.T) {
	deb := buildDeb(t, ".xz", []tarFile{{name: "./usr/bin/tool", body: "payload"}})

	entries, err := extractDeb(bytes.NewReader(deb), "", "")
	if err != nil {
		t.Fatalf("extractDeb: %v", err)
	}
	got := byName(entries)
	if _, ok := got["control"]; ok {
		t.Error("control member leaked into the layer")
	}
	if e, ok := got["usr/bin/tool"]; !ok {
		t.Errorf("payload missing, got %v", entries)
	} else if string(e.body) != "payload" {
		t.Errorf("body = %q, want %q", e.body, "payload")
	}
}

// TestExtractDebEveryCompression — dpkg picks the compressor, so a caller cannot know which to
// expect and all of them have to work.
func TestExtractDebEveryCompression(t *testing.T) {
	for _, suffix := range []string{"", ".gz", ".xz", ".zst"} {
		t.Run("data.tar"+suffix, func(t *testing.T) {
			deb := buildDeb(t, suffix, []tarFile{{name: "./usr/bin/tool", body: "payload"}})
			entries, err := extractDeb(bytes.NewReader(deb), "", "")
			if err != nil {
				t.Fatalf("extractDeb: %v", err)
			}
			if e, ok := byName(entries)["usr/bin/tool"]; !ok || string(e.body) != "payload" {
				t.Errorf("payload not recovered, got %v", entries)
			}
		})
	}
}

// TestExtractDebPreservesRelativeSymlinks — the case this feature was added for. Debian ships a
// native library as a real file plus a symlink under a versioned directory, and the symlink is
// relative. Flattening it to a copy, or dropping it, breaks the consumer's lookup path.
func TestExtractDebPreservesRelativeSymlinks(t *testing.T) {
	deb := buildDeb(t, ".xz", []tarFile{
		{name: "./usr/lib/x86_64-linux-gnu/liblua5.4-ldap.so.0.0.0", body: "ELF"},
		{name: "./usr/lib/x86_64-linux-gnu/lua/5.4/", dir: true},
		{name: "./usr/lib/x86_64-linux-gnu/lua/5.4/lualdap.so", link: "../../liblua5.4-ldap.so.0.0.0"},
	})

	entries, err := extractDeb(bytes.NewReader(deb), "", "usr/lib/x86_64-linux-gnu")
	if err != nil {
		t.Fatalf("extractDeb: %v", err)
	}
	got := byName(entries)
	if _, ok := got["liblua5.4-ldap.so.0.0.0"]; !ok {
		t.Errorf("real library missing, got %v", entries)
	}
	e, ok := got["lua/5.4/lualdap.so"]
	if !ok {
		t.Fatalf("symlink missing, got %v", entries)
	}
	// Unchanged means still relative, which is what makes it resolve inside the artifact once
	// subpath has stripped the shared prefix off both ends.
	if e.link != "../../liblua5.4-ldap.so.0.0.0" {
		t.Errorf("link = %q, want the relative target unchanged", e.link)
	}
}

// TestExtractDebRebasesUnderTarget — subpath and target compose the same way they do for tar, so
// a package's layout does not dictate the layout in the image.
func TestExtractDebRebasesUnderTarget(t *testing.T) {
	deb := buildDeb(t, ".gz", []tarFile{
		{name: "./usr/share/doc/fixture/copyright", body: "MIT"},
		{name: "./usr/lib/thing.so", body: "ELF"},
	})

	entries, err := extractDeb(bytes.NewReader(deb), "opt/vendor", "usr/lib")
	if err != nil {
		t.Fatalf("extractDeb: %v", err)
	}
	got := byName(entries)
	if _, ok := got["opt/vendor/thing.so"]; !ok {
		t.Errorf("payload not rebased, got %v", entries)
	}
	for name := range got {
		if strings.Contains(name, "copyright") {
			t.Errorf("subpath did not exclude %q", name)
		}
	}
}

// TestExtractDebRejectsMalformed — each of these is a case where guessing would produce a
// plausible-looking but wrong layer, so they must fail rather than degrade.
func TestExtractDebRejectsMalformed(t *testing.T) {
	valid := buildDeb(t, ".xz", []tarFile{{name: "./usr/bin/tool", body: "payload"}})

	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"not an archive", []byte("this is a tarball, not a deb"), "ar magic"},
		{"truncated header", valid[:len(arMagic)+20], "truncated ar header"},
		{"no data member", func() []byte {
			var b bytes.Buffer
			b.WriteString(arMagic)
			arMember(&b, "debian-binary", []byte("2.0\n"))
			return b.Bytes()
		}(), "no data member"},
		{"unknown compression", func() []byte {
			var b bytes.Buffer
			b.WriteString(arMagic)
			arMember(&b, "data.tar.lzma", []byte("whatever"))
			return b.Bytes()
		}(), "unsupported compression"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := extractDeb(bytes.NewReader(tc.in), "", "")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestAssembleUnpackDeb wires the mode through the layer path the reconciler actually uses,
// rather than testing extractDeb alone — collectEntries' switch is where a new mode gets
// forgotten.
func TestAssembleUnpackDeb(t *testing.T) {
	deb := buildDeb(t, ".xz", []tarFile{{name: "./usr/lib/thing.so", body: "ELF"}})
	src := writeBytes(t, "input.deb", deb)

	entries := entriesOf(t, []LayerInput{{
		Name: "vendor", Path: src, Unpack: UnpackDeb, Subpath: "usr/lib", Target: "/opt/vendor",
	}}, Config{})

	for _, e := range entries {
		if e.Name == "opt/vendor/thing.so" {
			return
		}
	}
	t.Errorf("deb payload did not reach the layer, got %v", entries)
}
