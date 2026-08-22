package controller

import (
	"strings"
	"testing"
)

// TestEveryChartGuardIsActuallyReached checks that each `fail` the chart relies on is somewhere
// Helm will execute it.
//
// Helm loads files whose names begin with an underscore as DEFINITIONS ONLY and never renders
// them, so a `fail` sitting inside `_registry.tpl` or `_retention.tpl` runs only if something
// Helm does render calls it — which is what `validate.yaml` exists for.
//
// This is not hypothetical. The first version of the retention guard had its `include` at the
// bottom of the partials file itself, so it was never invoked: every configuration that should
// have been rejected rendered cleanly, and the guard's own falsification cases all passed while it
// did nothing at all.
//
// One test rather than one per guard, because the failure mode is identical for all of them and
// three copies of this explanation is two too many.
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
			args:  []string{"--set", "registry.retention.window=2h"},
		},
		{
			guard: "registry credentials",
			args:  []string{"--set", "defaultRegistry.existingPushSecret=mine"},
		},
		{
			guard: "clustering prerequisites",
			args:  []string{"--set", "registry.cluster.enabled=true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.guard, func(t *testing.T) {
			args := tc.args
			// Every guard but the publish-mode one needs a valid mode first, or it fails for that
			// reason instead and proves nothing about the guard under test.
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
