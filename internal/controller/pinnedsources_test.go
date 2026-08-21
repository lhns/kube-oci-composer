package controller

import (
	"context"
	"strings"
	"testing"

	ociv1alpha1 "github.com/lhns/kube-oci-composer/api/v1alpha1"
	"github.com/lhns/kube-oci-composer/internal/reconciler"
)

// unpinned builds a composition consuming a source without naming a revision. That is legal, and
// under the default configuration it must stay legal -- see the second test.
func unpinnedComposition() *ociv1alpha1.ImageComposition {
	return composition("git", ociv1alpha1.Layer{
		Name: "config",
		SourceRef: &ociv1alpha1.SourceRefSource{
			Kind: "GitRepository", Name: "platform-config",
		},
		To: "/config",
	})
}

// TestRequirePinnedSourcesRefusesAnUnpinnedLayer covers threat-model gap T1.
//
// `sourceRef.revision` is optional on purpose (ADR 0026): a composition that tracks a branch is a
// legitimate thing to want. What T1 recorded was that an operator had no way to decide otherwise
// for their cluster, so an unpinned source was an unreviewable gap rather than a choice.
//
// A branch or semver range moves with NO generation bump here, which is why this matters: the
// staleness check has nothing to compare against, and the layer silently becomes whatever the
// branch is at now.
func TestRequirePinnedSourcesRefusesAnUnpinnedLayer(t *testing.T) {
	url, digest := tarball(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	r := reconcilerWith(t, repo)
	r.RequirePinnedSources = true

	_, _, err := r.resolveInputs(context.Background(), unpinnedComposition(), t.TempDir())
	if err == nil {
		t.Fatal("an unpinned source must be refused under --require-pinned-sources")
	}
	// TERMINAL, not Pending. What fixes an absent pin is editing this spec, which bumps the
	// generation; a Pending would wait forever for an event that cannot come. That is the same
	// distinction ADR 0009 draws, and getting it backwards here would hang the object.
	if !reconciler.IsTerminal(err) {
		t.Fatalf("must be terminal -- editing the spec is what fixes it; got %v", err)
	}
	if !strings.Contains(err.Error(), "require-pinned-sources") {
		t.Fatalf("the message must name the flag that caused it: %v", err)
	}
}

// TestAnUnpinnedLayerIsFineByDefault is the half that keeps ADR 0026's decision intact.
//
// Optionality is deliberate. This flag adds a way to opt out of it; it must not quietly become the
// default, because that would break every composition that legitimately tracks a branch.
func TestAnUnpinnedLayerIsFineByDefault(t *testing.T) {
	url, digest := tarball(t, map[string]string{"config/app.conf": "x"})
	repo := gitRepository("platform-config", "default", url, digest, "main@sha1:abcd")

	r := reconcilerWith(t, repo) // RequirePinnedSources not set
	if _, _, err := r.resolveInputs(context.Background(), unpinnedComposition(), t.TempDir()); err != nil {
		t.Fatalf("an unpinned source must remain legal by default: %v", err)
	}
}

// TestAPinnedLayerStillResolvesUnderTheFlag — the flag refuses an ABSENT pin, and nothing else.
// A pinned source must resolve exactly as before, or turning the flag on would break the
// configurations it is meant to require.
func TestAPinnedLayerStillResolvesUnderTheFlag(t *testing.T) {
	url, digest := tarball(t, map[string]string{"config/app.conf": "x"})
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
