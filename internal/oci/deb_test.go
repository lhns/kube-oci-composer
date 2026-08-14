package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// debFile is one entry to place in a fixture package's payload.
type debFile struct {
	name string // as dpkg writes it, i.e. with the "./" prefix
	body string
	link string // non-empty makes it a symlink
	dir  bool
}

// buildDataTar writes the payload tar dpkg would produce.
func buildDataTar(t *testing.T, files []debFile) []byte {
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

// compress applies the named data.tar suffix.
func compress(t *testing.T, raw []byte, suffix string) []byte {
	t.Helper()
	var out bytes.Buffer
	switch suffix {
	case "":
		return raw
	case ".gz":
		zw := gzip.NewWriter(&out)
		if _, err := zw.Write(raw); err != nil {
			t.Fatalf("gzip: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	case ".xz":
		zw, err := xz.NewWriter(&out)
		if err != nil {
			t.Fatalf("xz writer: %v", err)
		}
		if _, err := zw.Write(raw); err != nil {
			t.Fatalf("xz: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("xz close: %v", err)
		}
	case ".zst":
		zw, err := zstd.NewWriter(&out)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		if _, err := zw.Write(raw); err != nil {
			t.Fatalf("zstd: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("zstd close: %v", err)
		}
	default:
		t.Fatalf("fixture cannot produce %q", suffix)
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
func buildDeb(t *testing.T, suffix string, files []debFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(arMagic)
	// Deliberately odd-sized, so every test exercises the padding byte: get that wrong and every
	// subsequent member header is off by one.
	arMember(&buf, "debian-binary", []byte("2.0\n"))
	arMember(&buf, "control.tar"+suffix, compress(t, buildDataTar(t, []debFile{
		{name: "./control", body: "Package: fixture\n"},
	}), suffix))
	arMember(&buf, "data.tar"+suffix, compress(t, buildDataTar(t, files), suffix))
	return buf.Bytes()
}

// writeTemp puts bytes on disk, because unpackLayer takes a path rather than a reader.
func writeTemp(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.deb")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	return p
}

// byName indexes extracted entries for assertions.
func byName(entries []tarEntry) map[string]tarEntry {
	out := make(map[string]tarEntry, len(entries))
	for _, e := range entries {
		out[e.name] = e
	}
	return out
}

// TestExtractDebTakesOnlyTheDataMember — the control member is a tar too, and it has a ./control
// entry that would land in the artifact if the member name were not checked. A package that
// quietly ships its own packaging metadata into the image is the failure this guards.
func TestExtractDebTakesOnlyTheDataMember(t *testing.T) {
	deb := buildDeb(t, ".xz", []debFile{{name: "./usr/bin/tool", body: "payload"}})

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

// TestExtractDebEveryCompression — which compressor a package uses is the packager's choice, so
// all of them must work or `unpack: deb` is a promise the controller only sometimes keeps.
func TestExtractDebEveryCompression(t *testing.T) {
	for _, suffix := range []string{"", ".gz", ".xz", ".zst"} {
		t.Run("data.tar"+suffix, func(t *testing.T) {
			deb := buildDeb(t, suffix, []debFile{{name: "./usr/bin/tool", body: "payload"}})
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
	deb := buildDeb(t, ".xz", []debFile{
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
	if e.link != "../../liblua5.4-ldap.so.0.0.0" {
		t.Errorf("link = %q, want the relative target unchanged", e.link)
	}
	// The point of keeping it relative: with subpath stripping the prefix, it still resolves
	// inside the artifact rather than pointing at an absolute path that does not exist there.
	if strings.HasPrefix(e.link, "/") {
		t.Error("link was rewritten to an absolute path")
	}
}

// TestExtractDebRebasesUnderTarget — subpath and target compose the same way they do for tar, so
// a package's layout does not dictate the layout in the image.
func TestExtractDebRebasesUnderTarget(t *testing.T) {
	deb := buildDeb(t, ".gz", []debFile{
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
	valid := buildDeb(t, ".xz", []debFile{{name: "./usr/bin/tool", body: "payload"}})

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
// rather than testing extractDeb alone — the switch in unpackLayer is where a new mode gets
// forgotten.
func TestAssembleUnpackDeb(t *testing.T) {
	deb := buildDeb(t, ".xz", []debFile{{name: "./usr/lib/thing.so", body: "ELF"}})
	src := writeTemp(t, deb)

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
