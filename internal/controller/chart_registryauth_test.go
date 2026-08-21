package controller

import (
	"strings"
	"testing"
)

// TestChartRefusesAHalfSuppliedRegistryCredential covers the configuration that used to wedge the
// registry pod, and explains why refusing is the right answer rather than repairing.
//
// `defaultRegistry.existingPushSecret` says "I brought my own credential". `registry.auth.enabled`
// says "the bundled registry wants a password the chart generates". Those are two halves of one
// matched pair and only one of them has been replaced, so nothing in the release agrees on what the
// password is.
//
// Repairing it silently is worse than refusing, and the tempting repair is the trap: gate the
// htpasswd Secret on `auth.enabled` alone and `registryPassword` has no `-push` Secret left to read
// the previous value out of, so every `helm upgrade` mints a fresh `randAlphaNum 32`. The registry
// then demands a password that exists nowhere — including in the credential the operator supplied.
// It renders forever and never works.
func TestChartRefusesAHalfSuppliedRegistryCredential(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "defaultRegistry.existingPushSecret=mine")

	for _, want := range []string{
		"existingPushSecret",
		// The message must name every way out, because an operator who hits this has no way to
		// derive them and the right choice depends on how they manage Secrets.
		"registry.auth.password",
		"registry.auth.existingHtpasswdSecret",
		"registry.auth.enabled=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal must mention %q, so it can be acted on; got:\n%s", want, out)
		}
	}
}

// TestChartAcceptsEveryResolutionOfTheCredentialSplit is the other half. A guard that fires on
// correct configurations gets deleted, and all three of these are correct.
func TestChartAcceptsEveryResolutionOfTheCredentialSplit(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			"the operator supplies both halves",
			[]string{
				"--set", "defaultRegistry.existingPushSecret=mine",
				"--set", "registry.auth.existingHtpasswdSecret=my-htpasswd",
			},
		},
		{
			"the operator pins the password the chart hashes",
			[]string{
				"--set", "defaultRegistry.existingPushSecret=mine",
				"--set", "registry.auth.password=hunter2",
			},
		},
		{
			"the bundled registry is unauthenticated",
			[]string{
				"--set", "defaultRegistry.existingPushSecret=mine",
				"--set", "registry.auth.enabled=false",
			},
		},
		{
			// The ordinary BYO-registry shape: no bundled zot at all, so there is no second half
			// to disagree with.
			"an external registry entirely",
			[]string{
				"--set", "registry.enabled=false",
				"--set", "defaultRegistry.host=ghcr.io/example",
				"--set", "defaultRegistry.existingPushSecret=mine",
			},
		},
		{"the defaults", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			render(t, tc.args...)
		})
	}
}

// TestTheRegistryAuthCheckIsActuallyReached guards the same defect that made the first version of
// the retention check pass all eight of its falsification cases while doing nothing: Helm loads
// underscore-prefixed files as definitions and never renders them, so an `include` inside one is
// dead code. The check lives in `_registry.tpl` and must be invoked from `validate.yaml`.
func TestTheRegistryAuthCheckIsActuallyReached(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "defaultRegistry.existingPushSecret=mine")
	if !strings.Contains(out, "validate.yaml") {
		t.Fatalf("the refusal must come from a template Helm renders; got:\n%s", out)
	}
}
