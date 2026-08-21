package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// writeTarGz builds a small gzipped tar on disk for use as layer input.
func writeTarGz(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("writing header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("writing body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}

	p := filepath.Join(t.TempDir(), "input.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	return p
}

// TestAssembleIsDeterministic is the load-bearing test of this project. The reconcile loop
// skips work by comparing a computed digest against what is already published; provenance
// claims the output is a pure function of the inputs. Both are false if this fails.
func TestAssembleIsDeterministic(t *testing.T) {
	src := writeTarGz(t, map[string]string{
		"lib/a.jar": "aaa",
		"lib/b.jar": "bbb",
		"README":    "hello",
	})

	digestOf := func() string {
		img, err := Assemble(nil, []LayerInput{{
			Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/core",
		}}, Config{Labels: map[string]string{"a": "b"}}, t.TempDir())
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return d.String()
	}

	first := digestOf()
	for i := 0; i < 3; i++ {
		if got := digestOf(); got != first {
			t.Fatalf("assembly is not deterministic: run %d gave %s, want %s", i+2, got, first)
		}
	}
}

// TestAssembleDigestChangesWithContent guards the other half: identical inputs must give
// identical digests, but *different* inputs must not collide.
func TestAssembleDigestChangesWithContent(t *testing.T) {
	a := writeTarGz(t, map[string]string{"lib/a.jar": "aaa"})
	b := writeTarGz(t, map[string]string{"lib/a.jar": "different"})

	digestOf := func(src, target string) string {
		img, err := Assemble(nil, []LayerInput{{Name: "l", Path: src, Unpack: UnpackTarGz, Target: target}}, Config{}, t.TempDir())
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return d.String()
	}

	if digestOf(a, "/core") == digestOf(b, "/core") {
		t.Fatal("different content produced the same digest")
	}
	if digestOf(a, "/core") == digestOf(a, "/other") {
		t.Fatal("different target produced the same digest")
	}
}

// TestAssembleLayerOrderMatters — layers overlay, so order is part of the meaning and must be
// part of the digest.
func TestAssembleLayerOrderMatters(t *testing.T) {
	a := writeTarGz(t, map[string]string{"a": "1"})
	b := writeTarGz(t, map[string]string{"b": "2"})

	mk := func(inputs []LayerInput) string {
		img, err := Assemble(nil, inputs, Config{}, t.TempDir())
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		d, _ := img.Digest()
		return d.String()
	}

	ab := mk([]LayerInput{
		{Name: "a", Path: a, Unpack: UnpackTarGz, Target: "/"},
		{Name: "b", Path: b, Unpack: UnpackTarGz, Target: "/"},
	})
	ba := mk([]LayerInput{
		{Name: "b", Path: b, Unpack: UnpackTarGz, Target: "/"},
		{Name: "a", Path: a, Unpack: UnpackTarGz, Target: "/"},
	})
	if ab == ba {
		t.Fatal("layer order did not affect the digest")
	}
}

// TestAssemblePlacesContentAtTarget checks the thing the whole project exists to do.
func TestAssemblePlacesContentAtTarget(t *testing.T) {
	src := writeTarGz(t, map[string]string{"x/y.jar": "jar-bytes"})

	img, err := Assemble(nil, []LayerInput{{
		Name: "core", Path: src, Unpack: UnpackTarGz, Target: "/core",
	}}, Config{}, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	layers, err := img.Layers()
	if err != nil || len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d (err %v)", len(layers), err)
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		t.Fatalf("uncompressed: %v", err)
	}
	defer rc.Close()

	found := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := os.ReadFile(os.DevNull) // placeholder to keep imports honest
			_ = b
			buf := new(bytes.Buffer)
			if _, err := buf.ReadFrom(tr); err != nil {
				t.Fatalf("reading entry: %v", err)
			}
			found[hdr.Name] = buf.String()
		}
		// Every entry must carry the fixed epoch, or determinism is a lie.
		if !hdr.ModTime.Equal(epoch) {
			t.Fatalf("entry %q has ModTime %v, want %v", hdr.Name, hdr.ModTime, epoch)
		}
	}

	if got, ok := found["core/x/y.jar"]; !ok {
		t.Fatalf("expected core/x/y.jar in layer, got entries: %v", found)
	} else if got != "jar-bytes" {
		t.Fatalf("content mismatch: %q", got)
	}
}

// TestAssembleMatchesItsGoldenDigest is the mechanism behind AssemblyVersion.
//
// AssemblyVersion exists so that a controller which assembles differently cannot serve artifacts
// built by the old algorithm under an unchanged input hash. It is a constant a human must remember
// to bump, and TestAssembleIsDeterministic cannot help: it runs the algorithm twice in one process,
// so any change agrees with itself.
//
// This pins the actual bytes. Change the tar writer, the gzip level, the config, the ordering, or
// the toolchain's flate output, and this fails — which is the prompt to decide whether the change
// is intended and whether AssemblyVersion must move. Updating the constant below is part of making
// that change, not a nuisance to route around.
//
// Fixed to linux/amd64 on purpose: the platform of a base-less artifact is the controller's own
// architecture (ADR 0002), so leaving it unset would make this assert the host rather than the
// algorithm.
func TestAssembleMatchesItsGoldenDigest(t *testing.T) {
	const (
		goldenDigest      = "sha256:d0c6f98ca8519b07b0cb2a4e12c7568790e02cfeb9575cea5a9db7aa478b4ed0"
		goldenAssemblyVer = 2
	)
	if AssemblyVersion != goldenAssemblyVer {
		t.Fatalf("AssemblyVersion is %d but this golden digest was recorded at %d; re-record the "+
			"digest below deliberately, having checked the output really is meant to change",
			AssemblyVersion, goldenAssemblyVer)
	}

	inputs := []LayerInput{{
		Name:   "bundle",
		Digest: "sha256:1111",
		Unpack: UnpackTarGz,
		Target: "/plugins",
		Path: writeTarGz(t, map[string]string{
			"lib/a.jar": "aaa",
			"lib/b.jar": "bbb",
			"README":    "hello",
		}),
	}}

	img, err := AssembleAs(nil, inputs, Config{}, Platform{OS: "linux", Architecture: "amd64"}, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if digest.String() != goldenDigest {
		t.Errorf("assembled digest is %s, want %s.\n"+
			"The output format changed. If that is intended, bump AssemblyVersion so unchanged "+
			"specs rebuild, and re-record this digest in the same commit.", digest, goldenDigest)
	}
}
