package buildcontroller

import (
	"context"
	"strings"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestRequirePinnedSourcesRefusesAnUnpinnedContext covers threat T1: an unpinned context builds
// whatever the branch is at now, and a build's output cannot be checked afterwards (ADR 0025).
func TestRequirePinnedSourcesRefusesAnUnpinnedContext(t *testing.T) {
	r := harness(t, "FROM scratch@sha256:"+strings.Repeat("c", 64))
	r.RequirePinnedSources = true

	// sampleBuild's context names no revision, which is the case under test.
	_, _, err := r.resolveInputs(context.Background(), buildOf(t, nil))
	if err == nil {
		t.Fatal("an unpinned build context must be refused under --require-pinned-sources")
	}
	// Terminal: the fix is a spec edit, which bumps the generation.
	if !reconciler.IsTerminal(err) {
		t.Fatalf("must be terminal -- editing the spec is what fixes it; got %v", err)
	}
	if !strings.Contains(err.Error(), "require-pinned-sources") {
		t.Fatalf("the message must name the flag that caused it: %v", err)
	}
}

// TestAnUnpinnedContextIsFineByDefault keeps pinning opt-in (ADR 0026).
func TestAnUnpinnedContextIsFineByDefault(t *testing.T) {
	r := harness(t, "FROM scratch@sha256:"+strings.Repeat("c", 64))
	if _, _, err := r.resolveInputs(context.Background(), buildOf(t, nil)); err != nil {
		t.Fatalf("an unpinned context must remain legal by default: %v", err)
	}
}
