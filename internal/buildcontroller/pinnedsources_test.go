package buildcontroller

import (
	"context"
	"strings"
	"testing"

	"github.com/lhns/kube-oci-composer/internal/reconciler"
)

// TestRequirePinnedSourcesRefusesAnUnpinnedContext covers threat-model gap T1 on the kind where it
// bites hardest.
//
// On a composition, an unpinned source means the content moved. On a build it means the CODE moved:
// the Job runs whatever the branch is at now, and an ImageBuild's output is an observation rather
// than a function of its spec (ADR 0025) -- so afterwards there is nothing to check it against.
func TestRequirePinnedSourcesRefusesAnUnpinnedContext(t *testing.T) {
	r := harness(t, "FROM scratch@sha256:"+strings.Repeat("c", 64))
	r.RequirePinnedSources = true

	// sampleBuild's context names no revision, which is the case under test.
	_, _, err := r.resolveInputs(context.Background(), buildOf(t, nil))
	if err == nil {
		t.Fatal("an unpinned build context must be refused under --require-pinned-sources")
	}
	// Terminal: editing this spec is what fixes it, and that bumps the generation. A Pending would
	// wait for an event that never arrives.
	if !reconciler.IsTerminal(err) {
		t.Fatalf("must be terminal -- editing the spec is what fixes it; got %v", err)
	}
	if !strings.Contains(err.Error(), "require-pinned-sources") {
		t.Fatalf("the message must name the flag that caused it: %v", err)
	}
}

// TestAnUnpinnedContextIsFineByDefault keeps ADR 0026's optionality intact. The flag adds a way to
// opt out; it must not become the default.
func TestAnUnpinnedContextIsFineByDefault(t *testing.T) {
	r := harness(t, "FROM scratch@sha256:"+strings.Repeat("c", 64))
	if _, _, err := r.resolveInputs(context.Background(), buildOf(t, nil)); err != nil {
		t.Fatalf("an unpinned context must remain legal by default: %v", err)
	}
}
