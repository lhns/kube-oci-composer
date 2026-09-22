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

// The e2e reads gcDelay, gcInterval and deleteUntagged back out of the deployed ConfigMap and
// refuses to run a retention test whose watch window cannot outlast them. That is what stops a
// retention test passing while measuring nothing.
//
// It depends on this JSON shape. If the chart ever moved or renamed one of these, the helpers would
// read an empty string, the assertions would never fire, and the tests would go quiet in exactly the
// way they exist to prevent -- so the shape is asserted here, where no cluster is needed.
func TestTheRenderedConfigCarriesWhatTheE2EAssertsAgainst(t *testing.T) {
	storage, ok := registryConfig(t)["storage"].(map[string]any)
	if !ok {
		t.Fatal("no storage block; the e2e helpers read gcDelay and gcInterval from it")
	}
	for _, key := range []string{"gcDelay", "gcInterval"} {
		if v, _ := storage[key].(string); v == "" {
			t.Errorf("storage.%s is absent or not a string, so the e2e would read no value and "+
				"skip the precondition it exists to enforce", key)
		}
	}

	retention, ok := storage["retention"].(map[string]any)
	if !ok {
		t.Fatal("no storage.retention block")
	}
	policies, ok := retention["policies"].([]any)
	if !ok || len(policies) == 0 {
		t.Fatal("no retention policies; the e2e cannot tell whether untagged collection is on")
	}
	first, _ := policies[0].(map[string]any)
	if _, present := first["deleteUntagged"]; !present {
		t.Error("the policy carries no deleteUntagged key, so the e2e cannot detect the " +
			"configuration that once left a retention test measuring nothing")
	}
	if _, present := first["repositories"]; !present {
		t.Error("the policy carries no repositories key, so the e2e cannot tell which policy " +
			"governs the repository it is testing")
	}
}

// The scheduler delay is set where values.yaml documents it -- registry.retention -- and reaches
// the registry from there.
//
// It did not. The template read registry.gcMaxSchedulerDelay while values.yaml documented
// registry.retention.gcMaxSchedulerDelay, so an operator setting the documented key got zot's
// default, silently. The e2e passed only because up.sh set the undocumented one; nothing rendered
// the documented path until this.
func TestTheSchedulerDelayIsReadFromWhereItIsDocumented(t *testing.T) {
	storage, _ := registryConfig(t, "--set", "registry.retention.gcMaxSchedulerDelay=1s")["storage"].(map[string]any)
	if got, _ := storage["gcMaxSchedulerDelay"].(string); got != "1s" {
		t.Errorf("registry.retention.gcMaxSchedulerDelay=1s rendered gcMaxSchedulerDelay=%q", got)
	}
	// And empty by default: the jitter exists for registries holding thousands of repositories.
	storage, _ = registryConfig(t)["storage"].(map[string]any)
	if _, present := storage["gcMaxSchedulerDelay"]; present {
		t.Error("gcMaxSchedulerDelay is rendered by default; it is a test's setting")
	}
}
