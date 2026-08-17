package oci

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBytes puts fixture bytes on disk under a chosen name, since some unpack modes take a path
// and the extension is occasionally load-bearing for a reader's own sanity checks.
func writeBytes(t *testing.T, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return p
}

// TestUnpackTarEveryCompression — the codec table is shared with the deb path, so the risk is not
// the codecs themselves but the mapping: a mode wired to the wrong compression hands a compressed
// stream to the tar reader and reports it as a corrupt archive.
//
// No tar.bz2 case: the standard library decodes bzip2 but cannot write it, which is the same reason
// debDecompress's .bz2 branch has no test. The branch is two lines of stdlib shared with the
// already-shipped deb path, and adding a bzip2 writer dependency to cover it would cost more than
// it proves.
func TestUnpackTarEveryCompression(t *testing.T) {
	cases := map[UnpackMode]string{
		UnpackTar:     "",
		UnpackTarGz:   ".gz",
		UnpackTarXz:   ".xz",
		UnpackTarZstd: ".zst",
	}

	for mode, suffix := range cases {
		t.Run(string(mode), func(t *testing.T) {
			raw := buildDataTar(t, []debFile{{name: "usr/bin/tool", body: "payload"}})
			src := writeBytes(t, "input.tar"+suffix, compressData(t, raw, suffix))

			entries := entriesOf(t, []LayerInput{{
				Name: "vendor", Path: src, Unpack: mode, Target: "/opt",
			}}, Config{})

			for _, e := range entries {
				if e.Name == "opt/usr/bin/tool" {
					return
				}
			}
			t.Errorf("payload did not reach the layer, got %v", entries)
		})
	}
}

// TestUnpackTarCompressionsAgree — every compression of the same tar must produce the same layer.
// The codec is packaging, not content, so it must not reach the digest.
func TestUnpackTarCompressionsAgree(t *testing.T) {
	raw := buildDataTar(t, []debFile{
		{name: "usr/bin/tool", body: "payload"},
		{name: "usr/lib/thing.so", body: "ELF"},
	})

	var want string
	for _, tc := range []struct {
		mode   UnpackMode
		suffix string
	}{
		{UnpackTar, ""},
		{UnpackTarGz, ".gz"},
		{UnpackTarXz, ".xz"},
		{UnpackTarZstd, ".zst"},
	} {
		src := writeBytes(t, "input.tar"+tc.suffix, compressData(t, raw, tc.suffix))
		img, err := Assemble(nil, []LayerInput{{
			Name: "vendor", Path: src, Unpack: tc.mode, Target: "/opt",
		}}, Config{}, t.TempDir())
		if err != nil {
			t.Fatalf("%s: assemble: %v", tc.mode, err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatalf("%s: digest: %v", tc.mode, err)
		}
		if want == "" {
			want = d.String()
			continue
		}
		if d.String() != want {
			t.Errorf("%s produced %s, want %s", tc.mode, d, want)
		}
	}
}

// gzipBytes compresses a payload as a bare gzip stream, not a tar.
func gzipBytes(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	// A real .gz usually records the original filename. Set one deliberately: the layer must take
	// its name from the spec, so this must NOT show up anywhere in the output.
	zw.Name = "original-name.so"
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestUnpackGzPlacesASingleFile — gz is not an archive; it is `none` with one decompression first.
func TestUnpackGzPlacesASingleFile(t *testing.T) {
	src := writeBytes(t, "lib.so.gz", gzipBytes(t, "ELF payload"))

	entries := entriesOf(t, []LayerInput{{
		Name: "lib", Path: src, Unpack: UnpackGz, Target: "/opt/lib/mylib.so",
	}}, Config{})

	var found bool
	for _, e := range entries {
		if e.Name == "opt/lib/mylib.so" {
			found = true
		}
		if strings.Contains(e.Name, "original-name") {
			t.Errorf("the gzip header's filename reached the layer as %q", e.Name)
		}
	}
	if !found {
		t.Errorf("payload did not land at the target, got %v", entries)
	}
}

// TestUnpackGzRequiresAFileTarget — the output name comes from the spec and nowhere else, so there
// has to be one. Deriving it from the URL instead would make the layer depend on a field InputHash
// deliberately excludes, and two mirrors of identical bytes would then disagree under one hash.
func TestUnpackGzRequiresAFileTarget(t *testing.T) {
	src := writeBytes(t, "lib.so.gz", gzipBytes(t, "ELF"))

	for _, target := range []string{"", "/", "/opt/lib/"} {
		_, err := Assemble(nil, []LayerInput{{
			Name: "lib", Path: src, Unpack: UnpackGz, Target: target,
		}}, Config{}, t.TempDir())
		if err == nil {
			t.Errorf("target %q was accepted; it does not name a file", target)
			continue
		}
		if !strings.Contains(err.Error(), "must name a file") {
			t.Errorf("target %q: error %q does not explain the requirement", target, err)
		}
	}
}

// TestUnpackGzRejectsSubpath — there is nothing to select from, and ignoring the field silently
// would leave a spec mistake looking like it worked.
func TestUnpackGzRejectsSubpath(t *testing.T) {
	src := writeBytes(t, "lib.so.gz", gzipBytes(t, "ELF"))

	_, err := Assemble(nil, []LayerInput{{
		Name: "lib", Path: src, Unpack: UnpackGz, Target: "/opt/lib.so", Subpath: "somewhere",
	}}, Config{}, t.TempDir())
	if err == nil {
		t.Fatal("a subpath was accepted with unpack: gz")
	}
	if !strings.Contains(err.Error(), "subpath") {
		t.Errorf("error %q does not mention the subpath", err)
	}
}

// TestUnknownUnpackModeIsTerminal — the CRD's enum normally shields this arm, but it is reachable
// when the CRD is newer than the controller, which the chart makes easy: CRDs ship under crds/,
// and Helm installs those without ever upgrading them.
//
// It must be a typed error so the reconciler reports Stalled. Untyped, it was retried with backoff
// forever and no condition ever said why.
func TestUnknownUnpackModeIsTerminal(t *testing.T) {
	src := writeBytes(t, "input.bin", []byte("x"))

	_, err := Assemble(nil, []LayerInput{{
		Name: "vendor", Path: src, Unpack: "rpm", Target: "/opt/x",
	}}, Config{}, t.TempDir())
	if err == nil {
		t.Fatal("an unimplemented unpack mode was accepted")
	}

	var unsupported *ErrUnsupportedUnpack
	if !errors.As(err, &unsupported) {
		t.Fatalf("error %q is not an *ErrUnsupportedUnpack, so it cannot be mapped to Stalled", err)
	}
	if unsupported.Mode != "rpm" {
		t.Errorf("Mode = %q, want %q", unsupported.Mode, "rpm")
	}
}
