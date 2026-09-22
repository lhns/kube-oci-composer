package controller

import (
	"strings"
	"testing"
)

// TestChartRefusesAHalfSuppliedRegistryCredential: an own push credential with a chart-generated
// htpasswd leaves the two halves disagreeing about the password. It must be refused, not repaired:
// gating the htpasswd Secret on auth.enabled alone leaves no `-push` Secret to reuse the password
// from, so every upgrade would mint a new one that exists nowhere else.
func TestChartRefusesAHalfSuppliedRegistryCredential(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "defaultRegistry.existingPushSecret=mine")

	for _, want := range []string{
		"existingPushSecret",
		// Every way out must be named.
		"registry.auth.password",
		"registry.auth.existingHtpasswdSecret",
		"registry.auth.enabled=false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal must mention %q, so it can be acted on; got:\n%s", want, out)
		}
	}
}

// TestChartAcceptsEveryResolutionOfTheCredentialSplit: the guard must not fire on correct
// configurations.
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
			// No bundled registry, so there is no second half to disagree with.
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
