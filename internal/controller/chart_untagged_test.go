package controller

import (
	"strings"
	"testing"
)

// TestAShortGCDelayIsRefusedWhileUntaggedManifestsAreCollected: a build's manifest is untagged
// between push and being named (ADR 0054), and gcDelay is all that keeps the collector off it.
func TestAShortGCDelayIsRefusedWhileUntaggedManifestsAreCollected(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "registry.retention.gcDelay=1s")
	for _, want := range []string{"gcDelay", "deleteUntagged", "untagged", "buildPollInterval"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal must explain itself in terms of %q, so the reader can act on it:\n%s",
				want, out)
		}
	}
}

// TestDisablingUntaggedCollectionPermitsAnyDelay: with untagged collection off there is no race,
// so any delay is safe (the e2e relies on this).
func TestDisablingUntaggedCollectionPermitsAnyDelay(t *testing.T) {
	out := render(t,
		"--set", "registry.retention.gcDelay=1s",
		"--set", "registry.retention.deleteUntagged=false")
	if !strings.Contains(out, `"deleteUntagged": false`) {
		t.Errorf("deleteUntagged did not reach the registry config:\n%s", out)
	}
}

// TestTheDefaultRetentionSettingsRender: the shipped defaults pass their own guard.
func TestTheDefaultRetentionSettingsRender(t *testing.T) {
	out := render(t)
	if !strings.Contains(out, `"deleteUntagged": true`) {
		t.Error("the default no longer collects untagged manifests; a build refused under " +
			"onConflict: Fail leaves content nothing reclaims")
	}
}

// TestTheDerivedDefaultsMatchTheValuesTheyReplaced: deriving the defaults did not change them.
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

// TestTheNamingGapFloorTracksThePollInterval: the gcDelay floor scales with buildPollInterval, so
// tests can compress the whole clock.
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

// TestFreshlyPushedUntaggedContentIsKept: keepUntagged with pulledWithin alone matches nothing
// just pushed and never pulled. Off by default (ADR 0060); this holds when turned on.
func TestFreshlyPushedUntaggedContentIsKept(t *testing.T) {
	out := render(t, "--set", "registry.retention.keepUntagged=true")
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

// TestTheRenderedConfigCarriesWhatTheE2EAssertsAgainst: the e2e reads gcDelay, gcInterval and
// deleteUntagged from the deployed config to check its retention tests can measure something; if
// the shape moved, those helpers would read "" and the checks would silently never fire.
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

// TestKeepUntaggedOffLeavesNoRuleBehind: off means ABSENT, not `{}`. Any keepUntagged rule makes zot
// retain untagged manifests that lost their statistics with their last tag. ADR 0060.
func TestKeepUntaggedOffLeavesNoRuleBehind(t *testing.T) {
	cfg := registryConfig(t, "--set", "registry.retention.keepUntagged=false")
	storage, _ := cfg["storage"].(map[string]any)
	retention, _ := storage["retention"].(map[string]any)
	policies, _ := retention["policies"].([]any)
	if len(policies) == 0 {
		t.Fatal("no retention policy rendered")
	}
	policy, _ := policies[0].(map[string]any)
	if _, present := policy["keepUntagged"]; present {
		t.Errorf("keepUntagged=false still renders a keepUntagged rule, which is what pins retired "+
			"manifests forever: %v", policy["keepUntagged"])
	}
	// gcDelay alone now covers the naming gap.
	if policy["deleteUntagged"] != true {
		t.Errorf("deleteUntagged = %v; with keepUntagged off, nothing would reclaim anything", policy["deleteUntagged"])
	}
	if v, _ := storage["gcDelay"].(string); v == "" {
		t.Error("no gcDelay, which with keepUntagged off is the only cover for a build's unnamed output")
	}
}

// TestKeepUntaggedIsOffByDefault: on, zot keeps every manifest whose last tag expired (ADR 0060).
func TestKeepUntaggedIsOffByDefault(t *testing.T) {
	cfg := registryConfig(t)
	storage, _ := cfg["storage"].(map[string]any)
	retention, _ := storage["retention"].(map[string]any)
	policies, _ := retention["policies"].([]any)
	policy, _ := policies[0].(map[string]any)
	if _, present := policy["keepUntagged"]; present {
		t.Error("keepUntagged is configured by default, which keeps every retired image on disk " +
			"forever (ADR 0060)")
	}
}

// TestTheSchedulerDelayIsReadFromWhereItIsDocumented: registry.retention.gcMaxSchedulerDelay, as
// values.yaml documents it, reaches the registry.
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
