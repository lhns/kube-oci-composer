package controller

import (
	"strings"
	"testing"
)

// A build's manifest is UNTAGGED between being pushed and being named by this controller (ADR
// 0054), and untagged is exactly what a registry's collector reclaims. gcDelay is the only thing
// standing between the two.
//
// This is not hypothetical. The e2e ran gcDelay=1s and lost a build's manifest before it could be
// named; because it was the repository's only content the repository went too, which is why the
// read-back failed NAME_UNKNOWN rather than reporting a missing manifest. It was intermittent
// because zot walks repositories on a rotation, so a green run proved nothing.
func TestAShortGCDelayIsRefusedWhileUntaggedManifestsAreCollected(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "registry.retention.gcDelay=1s")
	for _, want := range []string{"gcDelay", "deleteUntagged", "untagged", "buildPollInterval"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal must explain itself in terms of %q, so the reader can act on it:\n%s",
				want, out)
		}
	}
}

// Turning untagged collection off removes the race rather than out-running it, so any delay is
// then safe. That combination is what a test wanting a one-second collector should use, and it is
// what the e2e now does.
func TestDisablingUntaggedCollectionPermitsAnyDelay(t *testing.T) {
	out := render(t,
		"--set", "registry.retention.gcDelay=1s",
		"--set", "registry.retention.deleteUntagged=false")
	if !strings.Contains(out, `"deleteUntagged": false`) {
		t.Errorf("deleteUntagged did not reach the registry config:\n%s", out)
	}
}

// The default has to stay on the permitted side of its own check -- a guard that the shipped
// values violate is a guard nobody can keep.
func TestTheDefaultRetentionSettingsRender(t *testing.T) {
	out := render(t)
	if !strings.Contains(out, `"deleteUntagged": true`) {
		t.Error("the default no longer collects untagged manifests; a build refused under " +
			"onConflict: Fail leaves content nothing reclaims")
	}
}

// The derived values must reproduce the literals they replaced, or this was a change to what is
// deployed rather than to how it is expressed -- and nobody asked for a change to what is deployed.
func TestTheDerivedDefaultsMatchTheValuesTheyReplaced(t *testing.T) {
	out := render(t)
	for _, want := range []string{
		`"gcDelay": "1h"`,
		`"gcInterval": "6h"`,
		`"pulledWithin": "720h"`,
		"--retention-refresh-interval=1h",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the derivation no longer produces %s", want)
		}
	}
}

// Shortening the poll shortens the gcDelay floor with it, which is what lets a test compress the
// whole clock and still have retention tests that measure something.
func TestTheNamingGapFloorTracksThePollInterval(t *testing.T) {
	out := render(t,
		"--set", "retention.window=30s",
		"--set", "retention.refreshInterval=1s",
		"--set", "registry.retention.gcFactor=6",
		"--set", "imageBuild.buildPollInterval=3s")
	if !strings.Contains(out, `"gcDelay": "9s"`) {
		t.Errorf("gcDelay did not follow buildPollInterval down to 3x it:\n%s", out)
	}
}

// keepUntagged with pulledWithin alone matches NOTHING for content that was just pushed and never
// pulled -- which is precisely what a build publishing by digest produces. It read as protection
// and was none.
func TestFreshlyPushedUntaggedContentIsKept(t *testing.T) {
	out := render(t)
	idx := strings.Index(out, `"keepUntagged"`)
	if idx < 0 {
		t.Fatal("no keepUntagged policy at all")
	}
	block := out[idx:min(idx+220, len(out))]
	if !strings.Contains(block, "pushedWithin") {
		t.Errorf("keepUntagged has no pushedWithin, so a manifest that was just pushed and never "+
			"pulled matches no rule:\n%s", block)
	}
}
