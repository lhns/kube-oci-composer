package controller

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	gcrtarball "github.com/google/go-containerregistry/pkg/v1/tarball"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
)

// The image layer verb, through the reconcile path a cluster actually uses.
//
// internal/oci covers the flattening itself; what these cover is the wiring — that the reference
// is parsed, the image pulled with the right credentials-free path, the digest fed to the input
// hash, and the result placed as one layer.

// publishContentImage pushes a multi-layer image whose filesystem is the given files, and returns
// the "repo:tag@digest" reference for it.
func publishContentImage(t *testing.T, host, repo string, layerFiles ...map[string]string) string {
	t.Helper()

	img := empty.Image
	for _, files := range layerFiles {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
			if body == "" && strings.Contains(name, ".wh.") {
				hdr.Size = 0
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatalf("writing header %q: %v", name, err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatalf("writing body %q: %v", name, err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatalf("closing tar: %v", err)
		}

		raw := buf.Bytes()
		layer, err := gcrtarball.LayerFromOpener(func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		})
		if err != nil {
			t.Fatalf("building layer: %v", err)
		}
		if img, err = mutate.AppendLayers(img, layer); err != nil {
			t.Fatalf("appending layer: %v", err)
		}
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	repository := host + "/" + repo
	if err := remote.Write(mustRef(t, repository+"@"+digest.String()), img); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	return repository + ":v1@" + digest.String()
}

// entriesOfArtifact reads back every path in the published artifact's last layer.
func entriesOfArtifact(t *testing.T, host, repo, digest string) (map[string]string, int) {
	t.Helper()
	img, err := remote.Image(mustRef(t, host+"/"+repo+"@"+digest))
	if err != nil {
		t.Fatalf("pulling the result: %v", err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}

	out := map[string]string{}
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			t.Fatalf("uncompressed: %v", err)
		}
		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("reading layer: %v", err)
			}
			body, _ := io.ReadAll(tr)
			out[hdr.Name] = string(body)
		}
		_ = rc.Close()
	}
	return out, len(layers)
}

// TestImageLayerIsContributedAsOneLayer — the headline case: a CI-built image lands at a path, and
// a three-layer source still contributes exactly one layer.
func TestImageLayerIsContributedAsOneLayer(t *testing.T) {
	obj := composition("from-image")
	r, host := servingReconciler(t, obj)

	ref := publishContentImage(t, host, "ci-build",
		map[string]string{"tool": "ELF"},
		map[string]string{"lib/helper.so": "so"},
		map[string]string{"doc/readme": "docs"},
	)

	obj.Spec.Layers = []ociv1alpha1.Layer{{
		Name:  "app",
		Image: &ociv1alpha1.ImageSource{Ref: ref},
		To:    "/opt/app",
	}}

	art := build(t, r, obj, "compose from an image layer")
	entries, layers := entriesOfArtifact(t, host, "from-image", art.Digest)

	if layers != 1 {
		t.Errorf("artifact has %d layers, want 1 — an image entry must flatten", layers)
	}
	for _, want := range []string{"opt/app/tool", "opt/app/lib/helper.so", "opt/app/doc/readme"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("%q missing; got %v", want, keysOf(entries))
		}
	}
}

// TestImageLayerSubpath — "take /usr/local/bin out of the image and put it at /opt/tools", which
// is the shape this verb was added for.
func TestImageLayerSubpath(t *testing.T) {
	obj := composition("image-subpath")
	r, host := servingReconciler(t, obj)

	ref := publishContentImage(t, host, "toolbox", map[string]string{
		"usr/local/bin/tool": "ELF",
		"usr/share/doc/x":    "docs",
	})

	obj.Spec.Layers = []ociv1alpha1.Layer{{
		Name:  "tools",
		Image: &ociv1alpha1.ImageSource{Ref: ref, Subpath: "usr/local/bin"},
		To:    "/opt/tools",
	}}

	art := build(t, r, obj, "compose an image subpath")
	entries, _ := entriesOfArtifact(t, host, "image-subpath", art.Digest)

	if _, ok := entries["opt/tools/tool"]; !ok {
		t.Errorf("subpath contents did not land at the target; got %v", keysOf(entries))
	}
	for name := range entries {
		if strings.Contains(name, "share") {
			t.Errorf("entry %q came from outside the subpath", name)
		}
	}
}

// TestImageLayerAppliesWhiteouts — the source image's own deletions must be resolved during
// flattening, not carried into the artifact as literal ".wh." files.
func TestImageLayerAppliesWhiteouts(t *testing.T) {
	obj := composition("image-whiteout")
	r, host := servingReconciler(t, obj)

	ref := publishContentImage(t, host, "pruned",
		map[string]string{"keep": "kept", "drop": "gone"},
		map[string]string{".wh.drop": ""},
	)

	obj.Spec.Layers = []ociv1alpha1.Layer{{
		Name:  "app",
		Image: &ociv1alpha1.ImageSource{Ref: ref},
		To:    "/opt",
	}}

	art := build(t, r, obj, "flatten a whiteout")
	entries, _ := entriesOfArtifact(t, host, "image-whiteout", art.Digest)

	if _, ok := entries["opt/keep"]; !ok {
		t.Error("opt/keep was lost")
	}
	if _, ok := entries["opt/drop"]; ok {
		t.Error("opt/drop survived the source image's whiteout")
	}
	for name := range entries {
		if strings.Contains(name, ".wh.") {
			t.Errorf("whiteout marker %q leaked into the artifact", name)
		}
	}
}

// TestImageLayerDigestIsInTheInputHash — the pinned digest is the content address, so moving it
// must rebuild. Without this the short-circuit would serve the old artifact after a repin.
func TestImageLayerDigestIsInTheInputHash(t *testing.T) {
	obj := composition("image-hash")
	r, host := servingReconciler(t, obj)

	first := publishContentImage(t, host, "app", map[string]string{"x": "one"})
	obj.Spec.Layers = []ociv1alpha1.Layer{{
		Name: "app", Image: &ociv1alpha1.ImageSource{Ref: first}, To: "/opt",
	}}
	before := build(t, r, obj, "first image")

	second := publishContentImage(t, host, "app", map[string]string{"x": "two"})
	obj.Spec.Layers[0].Image.Ref = second
	after := build(t, r, obj, "repinned image")

	if before.Digest == after.Digest {
		t.Error("repinning the image digest did not change the artifact")
	}
}

// TestBaseRefIsEquivalentToImageAndDigest — the two spellings must produce the same artifact AND
// the same input hash. If they differed, rewriting a spec from one to the other would republish
// every artifact for no change in content.
func TestBaseRefIsEquivalentToImageAndDigest(t *testing.T) {
	split := composition("base-split")
	r, host := servingReconciler(t, split)

	repo, digest := publishBaseImage(t, host, "base", 2, nil)
	origin := newCountingOrigin(t, map[string]string{"plugins/core.jar": "jar"})
	layer := urlLayer("plugins", origin.url, origin.digest, "/plugins")

	split.Spec.Base = &ociv1alpha1.BaseImage{Image: repo, Digest: digest}
	split.Spec.Layers = []ociv1alpha1.Layer{layer}
	splitArt := build(t, r, split, "base as image+digest")
	splitHash := split.Status.InputHash

	combined := composition("base-ref")
	combined.Spec.Base = &ociv1alpha1.BaseImage{Ref: repo + ":v1@" + digest}
	combined.Spec.Layers = []ociv1alpha1.Layer{layer}
	if err := r.Create(t.Context(), combined); err != nil {
		t.Fatalf("creating: %v", err)
	}
	combinedArt := build(t, r, combined, "base as ref")

	if splitArt.Digest != combinedArt.Digest {
		t.Errorf("ref gave %s, image+digest gave %s: the spellings must agree",
			combinedArt.Digest, splitArt.Digest)
	}
	if splitHash != combined.Status.InputHash {
		t.Errorf("input hashes differ (%s vs %s): switching spelling would republish everything",
			splitHash, combined.Status.InputHash)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ v1.Image = empty.Image
