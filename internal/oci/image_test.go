package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"path"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// imageLayer builds one image layer from the given entries, in order.
//
// Entry names are written verbatim so a fixture can carry a whiteout (".wh." prefix), which is how
// OCI expresses deletion and the thing flattening has to honour.
func imageLayer(t *testing.T, files []tarFile) v1.Layer {
	t.Helper()
	raw := buildTar(t, files)
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatalf("building layer: %v", err)
	}
	return layer
}

// imageOf stacks layers into an image, earliest first.
func imageOf(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatalf("building image: %v", err)
	}
	return img
}

// TestExtractImageFlattensToOneLayer is the constraint the verb exists under.
//
// ADR 0016 hoisted the base out of the layer list because "an image entry contributes many layers
// where every other entry contributes exactly one". A three-layer source image must therefore
// still produce exactly one layer here, or this verb reinstates the exception that removed.
func TestExtractImageFlattensToOneLayer(t *testing.T) {
	src := imageOf(t,
		imageLayer(t, []tarFile{{name: "a.txt", body: "one"}}),
		imageLayer(t, []tarFile{{name: "b.txt", body: "two"}}),
		imageLayer(t, []tarFile{{name: "c.txt", body: "three"}}),
	)

	img, err := Assemble(nil, []LayerInput{{
		Name: "vendor", Image: src, Unpack: UnpackImage, Target: "/opt",
	}}, Config{}, t.TempDir())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	if len(layers) != 1 {
		t.Fatalf("a %d-layer image produced %d layers, want exactly 1", 3, len(layers))
	}

	got := byName(mustExtractImage(t, src, "opt", ""))
	for _, want := range []string{"opt/a.txt", "opt/b.txt", "opt/c.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%q missing from the flattened result", want)
		}
	}
}

// TestExtractImageAppliesWhiteouts — flattening must produce the filesystem a runtime would see.
// A later layer deleting a file from an earlier one is expressed as a ".wh." sibling, and carrying
// that marker through instead of applying it would put a literal ".wh.secret" file in the layer.
func TestExtractImageAppliesWhiteouts(t *testing.T) {
	src := imageOf(t,
		imageLayer(t, []tarFile{
			{name: "keep.txt", body: "kept"},
			{name: "secret.txt", body: "should be gone"},
		}),
		imageLayer(t, []tarFile{{name: ".wh.secret.txt", body: ""}}),
	)

	got := byName(mustExtractImage(t, src, "", ""))

	if _, ok := got["keep.txt"]; !ok {
		t.Error("keep.txt was lost")
	}
	if _, ok := got["secret.txt"]; ok {
		t.Error("secret.txt survived a whiteout")
	}
	for name := range got {
		if strings.Contains(path.Base(name), ".wh.") {
			t.Errorf("whiteout marker %q leaked into the layer instead of being applied", name)
		}
	}
}

// TestExtractImageLastLayerWins — the other half of flattening: an overwritten file takes the
// later layer's content.
func TestExtractImageLastLayerWins(t *testing.T) {
	src := imageOf(t,
		imageLayer(t, []tarFile{{name: "conf.ini", body: "old"}}),
		imageLayer(t, []tarFile{{name: "conf.ini", body: "new"}}),
	)

	got := byName(mustExtractImage(t, src, "", ""))
	if e, ok := got["conf.ini"]; !ok || string(e.body) != "new" {
		t.Errorf("conf.ini = %q, want %q", e.body, "new")
	}
}

// TestExtractImageSubpathAndTarget — an image layer is ordinary content, so the options that mean
// something for every other verb mean the same here. "give me /usr/local/bin from this image, at
// /opt/tools" is the shape this verb was added for.
func TestExtractImageSubpathAndTarget(t *testing.T) {
	src := imageOf(t, imageLayer(t, []tarFile{
		{name: "usr/local/bin/tool", body: "ELF"},
		{name: "usr/share/doc/readme", body: "docs"},
	}))

	got := byName(mustExtractImage(t, src, "opt/tools", "usr/local/bin"))

	if _, ok := got["opt/tools/tool"]; !ok {
		t.Errorf("subpath contents did not land at the target, got %v", got)
	}
	for name := range got {
		if strings.Contains(name, "usr") || strings.Contains(name, "readme") {
			t.Errorf("entry %q came from outside the subpath", name)
		}
	}

	if _, err := extractImage(src, "opt", "nope"); err == nil {
		t.Error("a subpath matching nothing was accepted")
	}
}

// TestExtractImageNormalisesModes — the same normalisation every other source gets, so an image
// built with odd permissions cannot vary the output digest through them.
func TestExtractImageNormalisesModes(t *testing.T) {
	src := imageOf(t, imageLayer(t, []tarFile{
		{name: "bin/tool", body: "ELF"},
		{name: "etc/conf", body: "x"},
	}))

	entries := mustExtractImage(t, src, "", "")
	for _, e := range entries {
		if e.dir {
			continue
		}
		if e.mode != 0o644 && e.mode != 0o755 {
			t.Errorf("entry %q has un-normalised mode %o", e.name, e.mode)
		}
	}
}

// TestAssembleImageLayerIsDeterministic — the property the whole project rests on has to hold for
// this verb too: the same source image assembles to the same digest every time.
func TestAssembleImageLayerIsDeterministic(t *testing.T) {
	src := imageOf(t,
		imageLayer(t, []tarFile{{name: "lib/a.so", body: "aaa"}}),
		imageLayer(t, []tarFile{{name: "bin/tool", body: "ELF"}}),
	)

	in := []LayerInput{{Name: "vendor", Image: src, Unpack: UnpackImage, Target: "/opt"}}
	first := assembleDigest(t, in, Config{})
	for i := range 3 {
		if got := assembleDigest(t, in, Config{}); got != first {
			t.Fatalf("repeat %d produced %s, want %s", i, got, first)
		}
	}
}

// TestImageLayerAndTarballAgree — an image and a tarball of the same content must produce the same
// layer. Different packaging, same bytes: if these diverge, the verb is doing something to the
// content that the other sources do not.
func TestImageLayerAndTarballAgree(t *testing.T) {
	files := []tarFile{
		{name: "lib/a.so", body: "aaa"},
		{name: "bin/tool", body: "ELF"},
	}

	fromImage := assembleDigest(t, []LayerInput{{
		Name: "vendor", Image: imageOf(t, imageLayer(t, files)), Unpack: UnpackImage, Target: "/opt",
	}}, Config{})

	fromTar := assembleDigest(t, []LayerInput{{
		Name:   "vendor",
		Path:   writeBytes(t, "input.tar", buildTar(t, files)),
		Unpack: UnpackTar,
		Target: "/opt",
	}}, Config{})

	if fromImage != fromTar {
		t.Errorf("image layer gave %s, tarball gave %s: the same content must give the same layer",
			fromImage, fromTar)
	}
}

// mustExtractImage is the common shape of these assertions.
func mustExtractImage(t *testing.T, img v1.Image, target, subpath string) []tarEntry {
	t.Helper()
	entries, err := extractImage(img, target, subpath)
	if err != nil {
		t.Fatalf("extractImage: %v", err)
	}
	return entries
}

// readTarNames is a small guard that buildTar writes what these fixtures assume — a whiteout name
// must survive verbatim rather than being cleaned away by the writer.
func readTarNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading fixture tar: %v", err)
		}
		out = append(out, hdr.Name)
	}
	return out
}

func TestWhiteoutFixtureIsWrittenVerbatim(t *testing.T) {
	names := readTarNames(t, buildTar(t, []tarFile{{name: ".wh.secret.txt"}}))
	if len(names) != 1 || names[0] != ".wh.secret.txt" {
		t.Errorf("fixture wrote %v, want the whiteout name verbatim", names)
	}
}
