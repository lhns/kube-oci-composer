package controller

import (
	"strings"
	"testing"
)

// TestEveryChartGuardIsActuallyReached checks that each `fail` the chart relies on fires from
// validate.yaml. Helm never renders `_*.tpl` files, so a guard there runs only if a rendered
// template includes it; otherwise it is silently dead.
func TestEveryChartGuardIsActuallyReached(t *testing.T) {
	cases := []struct {
		guard string
		args  []string
	}{
		{
			guard: "publish mode",
			args:  nil, // the default values name no mode
		},
		{
			guard: "retention margin",
			// Pin the interval: it is derived from the window, so shrinking the window alone
			// keeps the margin.
			args: []string{
				"--set", "retention.window=2h",
				"--set", "retention.refreshInterval=1h",
			},
		},
		{
			guard: "derived refresh floor",
			args:  []string{"--set", "retention.window=2h"},
		},
		{
			guard: "untagged naming gap",
			args:  []string{"--set", "registry.retention.gcDelay=1s"},
		},
		{
			guard: "sweep slower than the window",
			args:  []string{"--set", "registry.retention.gcFactor=0.5"},
		},
		{
			guard: "registry credentials",
			args:  []string{"--set", "defaultRegistry.existingPushSecret=mine"},
		},
		{
			guard: "read replica prerequisites",
			args:  []string{"--set", "registry.readReplicas=2"},
		},
		{
			guard: "registry.cluster is gone",
			args:  []string{"--set", "registry.cluster.enabled=true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.guard, func(t *testing.T) {
			args := tc.args
			// Every other guard needs a valid mode, or it fails for that reason instead.
			if tc.guard != "publish mode" {
				args = append(installable, args...)
			}

			out := renderRawExpectingFailure(t, args...)
			if !strings.Contains(out, "validate.yaml") {
				t.Fatalf("the %s guard did not fail from a template Helm renders, so it is dead "+
					"code that would let every bad configuration through:\n%s", tc.guard, out)
			}
		})
	}
}

// registry.retention.window moved to retention.window in 0.6.0. An old values file still setting it
// must be refused: ignored, a lengthened window silently fell back to the default and content was
// collected sooner than the operator had asked for.
func TestTheOldRetentionWindowKeyIsRefused(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "registry.retention.window=2160h")
	if !strings.Contains(out, "retention.window") || !strings.Contains(out, "moved") {
		t.Errorf("the refusal must say where the key went:\n%s", out)
	}
}
