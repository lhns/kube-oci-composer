package controller

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	recon "github.com/lhns/kube-oci-composer/internal/reconciler"
)

// Image layers and bases taken from a multi-architecture index (ADRs 0015, 0024): spec.platforms
// selects the child, and an image layer is then built per platform.

var (
	linuxAMD64 = v1.Platform{OS: "linux", Architecture: "amd64"}
	linuxARM64 = v1.Platform{OS: "linux", Architecture: "arm64"}
)

// publishToolIndex pushes an index whose amd64 and arm64 children hold a different file each, so a
// test can see which child was flattened.
func publishToolIndex(t *testing.T, host, repo string) string {
	t.Helper()
	idx := mutate.IndexMediaType(empty.Index, "application/vnd.oci.image.index.v1+json")
	for _, c := range []struct {
		plat  v1.Platform
		files map[string]string
	}{
		{linuxAMD64, map[string]string{"tool-amd64": "x"}},
		{linuxARM64, map[string]string{"tool-arm64": "y"}},
	} {
		img := contentImage(t, c.files)
		cf, err := img.ConfigFile()
		if err != nil {
			t.Fatalf("config: %v", err)
		}
		cf = cf.DeepCopy()
		cf.OS, cf.Architecture = c.plat.OS, c.plat.Architecture
		if img, err = mutate.ConfigFile(img, cf); err != nil {
			t.Fatalf("mutate config: %v", err)
		}
		plat := c.plat
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add: img, Descriptor: v1.Descriptor{Platform: &plat},
		})
	}
	return publishIndex(t, host, repo, idx)
}

func toolLayer(host, repo, digest string) ociv1alpha1.Layer {
	return ociv1alpha1.Layer{
		Name:  "tool",
		Image: &ociv1alpha1.ImageSource{Ref: host + "/" + repo + ":v1@" + digest},
		To:    "/opt",
	}
}

func TestIndexImageLayerWithOnePlatform(t *testing.T) {
	obj := composition("idxlayer-one")
	r, host := registryReconciler(t, obj)
	obj.Spec.Layers = []ociv1alpha1.Layer{toolLayer(host, "tools", publishToolIndex(t, host, "tools"))}
	obj.Spec.Platforms = []string{"linux/arm64"}

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("an index image layer with spec.platforms should be accepted: %v", err)
	}
	if len(res.Record.Manifests) != 0 {
		t.Fatalf("one platform must stay a single image, got children %v", res.Record.Manifests)
	}
	entries, layers := entriesOfArtifact(t, host, "default/idxlayer-one", res.Artifact.Digest)
	if layers != 1 {
		t.Errorf("artifact has %d layers, want 1", layers)
	}
	if _, ok := entries["opt/tool-arm64"]; !ok {
		t.Errorf("the arm64 child was not flattened: %v", keysOf(entries))
	}
	if _, ok := entries["opt/tool-amd64"]; ok {
		t.Errorf("the amd64 child leaked into an arm64 build: %v", keysOf(entries))
	}
}

func TestIndexImageLayerWithTwoPlatforms(t *testing.T) {
	obj := composition("idxlayer-two")
	r, host := registryReconciler(t, obj)
	obj.Spec.Layers = []ociv1alpha1.Layer{toolLayer(host, "tools", publishToolIndex(t, host, "tools"))}
	obj.Spec.Platforms = []string{"linux/amd64", "linux/arm64"}

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Record.Manifests) != 2 {
		t.Fatalf("want 2 children, got %v", res.Record.Manifests)
	}
	// Two configs and one image layer PER platform: nothing is shared, so GC must keep all four.
	if len(res.Record.Blobs) != 4 {
		t.Fatalf("want 2 configs + 2 per-platform layers recorded, got %d: %v",
			len(res.Record.Blobs), res.Record.Blobs)
	}

	idx, err := remote.Index(mustRef(t, host+"/default/idxlayer-two@"+res.Artifact.Digest))
	if err != nil {
		t.Fatalf("pulling the index: %v", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatalf("index manifest: %v", err)
	}
	for _, m := range im.Manifests {
		want := "opt/tool-" + m.Platform.Architecture
		entries, _ := entriesOfArtifact(t, host, "default/idxlayer-two", m.Digest.String())
		delete(entries, "opt/")
		if _, ok := entries[want]; !ok || len(entries) != 1 {
			t.Errorf("%s child holds %v, want only %q", m.Platform.Architecture, keysOf(entries), want)
		}
	}
}

func TestIndexImageLayerLacksARequestedPlatform(t *testing.T) {
	obj := composition("idxlayer-missing")
	r, host := registryReconciler(t, obj)
	obj.Spec.Layers = []ociv1alpha1.Layer{toolLayer(host, "tools", publishToolIndex(t, host, "tools"))}
	obj.Spec.Platforms = []string{"linux/amd64", "linux/s390x"}

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "s390x") || !recon.IsTerminal(err) {
		t.Fatalf("want a terminal error naming s390x, got %v", err)
	}
}

func TestIndexImageLayerWithoutPlatformsIsRefused(t *testing.T) {
	obj := composition("idxlayer-none")
	r, host := registryReconciler(t, obj)
	obj.Spec.Layers = []ociv1alpha1.Layer{toolLayer(host, "tools", publishToolIndex(t, host, "tools"))}

	_, err := r.reconcileArtifact(context.Background(), obj)
	if err == nil || !strings.Contains(err.Error(), "spec.platforms") || !recon.IsTerminal(err) {
		t.Fatalf("want a terminal error naming spec.platforms, got %v", err)
	}
}

// A single-manifest image has no platform to select, so it stays one layer shared by every child,
// as before index image layers existed.
func TestSingleManifestImageLayerStaysSharedAcrossPlatforms(t *testing.T) {
	obj := composition("single-shared")
	r, host := registryReconciler(t, obj)
	ref := publishContentImage(t, host, "plain", map[string]string{"tool": "ELF"})
	obj.Spec.Layers = []ociv1alpha1.Layer{{Name: "tool", Image: &ociv1alpha1.ImageSource{Ref: ref}, To: "/opt"}}
	obj.Spec.Platforms = []string{"linux/amd64", "linux/arm64"}

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Record.Blobs) != 3 {
		t.Fatalf("want 2 configs + 1 shared layer, got %d: %v", len(res.Record.Blobs), res.Record.Blobs)
	}
}

// The base half of ADR 0015's amendment: one declared platform selects from an index base too.
func TestIndexBaseIsAcceptedWithOnePlatform(t *testing.T) {
	obj := composition("indexbase-one")
	r, host := registryReconciler(t, obj)
	digest := publishIndex(t, host, "onebase", platformIndex(t, linuxAMD64, linuxARM64))

	withBase(obj, host+"/onebase", digest)
	obj.Spec.Platforms = []string{"linux/arm64"}
	obj.Spec.Layers = []ociv1alpha1.Layer{removeLayer("noop", "/placeholder")}

	res, err := r.reconcileArtifact(context.Background(), obj)
	if err != nil {
		t.Fatalf("an index base with one platform should be accepted: %v", err)
	}
	if res.Artifact == nil || len(res.Record.Manifests) != 0 {
		t.Fatalf("want a single image, got %+v", res.Record)
	}
}
