package controller

import (
	"context"
	"strings"
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/reconciler"
)

// unpinnedComposition consumes a source without naming a revision, which is legal by default.
func unpinnedComposition() *ociv1alpha1.ImageComposition {
	return composition("git", ociv1alpha1.Layer{
		Name: "config",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "platform-config",
		},
		To: "/config",
	})
}

// TestRequirePinnedSourcesRefusesAnUnpinnedLayer covers threat-model gap T1: `sourceRef.revision`
// is optional (ADR 0026), and this flag lets an operator require it cluster-wide. An unpinned
// source moves with no generation bump, so the layer silently becomes whatever the branch is now.
func TestRequirePinnedSourcesRefusesAnUnpinnedLayer(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	r := reconcilerWith(t, repo)
	r.RequirePinnedSources = true

	_, _, err := r.resolveInputs(context.Background(), unpinnedComposition(), t.TempDir())
	if err == nil {
		t.Fatal("an unpinned source must be refused under --require-pinned-sources")
	}
	// TERMINAL, not Pending (ADR 0009): editing this spec is the fix, and it bumps the generation.
	if !reconciler.IsTerminal(err) {
		t.Fatalf("must be terminal -- editing the spec is what fixes it; got %v", err)
	}
	if !strings.Contains(err.Error(), "require-pinned-sources") {
		t.Fatalf("the message must name the flag that caused it: %v", err)
	}
}

// TestAnUnpinnedLayerIsFineByDefault keeps ADR 0026 intact: the flag is opt-in, or every
// composition that tracks a branch would break.
func TestAnUnpinnedLayerIsFineByDefault(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	r := reconcilerWith(t, repo) // RequirePinnedSources not set
	if _, _, err := r.resolveInputs(context.Background(), unpinnedComposition(), t.TempDir()); err != nil {
		t.Fatalf("an unpinned source must remain legal by default: %v", err)
	}
}

// TestAPinnedLayerStillResolvesUnderTheFlag — the flag refuses an ABSENT pin and nothing else.
func TestAPinnedLayerStillResolvesUnderTheFlag(t *testing.T) {
	url, digest := contentServer(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	obj := composition("git", ociv1alpha1.Layer{
		Name: "config",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "platform-config", Revision: "main@sha1:abcd",
		},
		To: "/config",
	})
	r := reconcilerWith(t, repo)
	r.RequirePinnedSources = true

	inputs, _, err := r.resolveInputs(context.Background(), obj, t.TempDir())
	if err != nil {
		t.Fatalf("a pinned source must resolve under the flag: %v", err)
	}
	if inputs[0].Digest != digest {
		t.Fatalf("digest %q, want %q", inputs[0].Digest, digest)
	}
}
