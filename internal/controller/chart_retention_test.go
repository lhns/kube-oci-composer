package controller

import (
	"strings"
	"testing"
)

// TestChartRefusesARetentionMarginThatIsTooThin covers threat-model gap D7.
//
// The guarantee in ADR 0031 is a RATIO, not either number: the registry expires what it has not
// seen pulled, the controllers pull to prevent that, and the margin between the window and the
// refresh interval is how long refreshing may be broken before something a live object still
// references is reclaimed. Until one chart rendered both numbers, nothing could compare them --
// one was a controller flag, the other a registry's config, and D7 said so.
//
// The failure mode is why this fails the render instead of warning: shrinking the window costs
// nothing visible, the margin silently becomes a race, and the symptom arrives one window later as
// a deleted image.
func TestChartRefusesARetentionMarginThatIsTooThin(t *testing.T) {
	tooThin := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "a window barely wider than the interval",
			args: []string{"--set", "registry.retention.window=2h"},
			want: "only 2.0x",
		},
		{
			// Each controller refreshes its own objects' images, so the builder's interval is as
			// load-bearing as the composer's. An earlier version checked only one.
			name: "the builder's interval alone",
			args: []string{"--set", "imageBuild.retention.refreshInterval=48h"},
			want: "imageBuild.retention.refreshInterval",
		},
		{
			name: "refreshing disabled with a window still set",
			args: []string{"--set", "operator.retention.refreshInterval=0"},
			want: "disables refreshing entirely",
		},
		{
			// `--set x=0s` and `--set x=0` reach the template as different types. Both mean off.
			name: "refreshing disabled, written as 0s",
			args: []string{"--set", "imageBuild.retention.refreshInterval=0s"},
			want: "disables refreshing entirely",
		},
	}
	for _, tc := range tooThin {
		t.Run(tc.name, func(t *testing.T) {
			out := renderExpectingFailure(t, tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("the render failed, but not for this reason -- wanted %q in:\n%s", tc.want, out)
			}
		})
	}
}

// TestChartAcceptsRetentionSettingsThatAreMerelyUnusual is the other half, and it is the half that
// stops the check from becoming an obstacle.
//
// The margin required is 24, far below the default's 720, because this exists to catch settings
// that are WRONG rather than to enforce the default on someone who has thought about it. A check
// that fires on a deliberate, safe configuration gets disabled, and then it protects nothing.
func TestChartAcceptsRetentionSettingsThatAreMerelyUnusual(t *testing.T) {
	fine := []struct {
		name string
		args []string
	}{
		{"the defaults", nil},
		{"exactly the minimum margin", []string{"--set", "registry.retention.window=24h"}},
		{
			"a compressed but proportionate pair",
			[]string{
				"--set", "registry.retention.window=30m",
				"--set", "operator.retention.refreshInterval=1m",
				"--set", "imageBuild.retention.refreshInterval=1m",
			},
		},
		{
			// Nothing to compare against: the operator's registry has its own policy, or none.
			"refreshing off when the bundled registry is not installed",
			[]string{
				"--set", "operator.retention.refreshInterval=0",
				"--set", "registry.enabled=false",
				"--set", "defaultRegistry.host=ghcr.io/example",
			},
		},
		{
			"refreshing off when the registry expires nothing",
			[]string{
				"--set", "operator.retention.refreshInterval=0",
				"--set", "registry.retention.window=",
			},
		},
		{
			// A controller that is not installed cannot fail to refresh.
			"a thin margin with both controllers disabled",
			[]string{
				"--set", "registry.retention.window=2h",
				"--set", "imageComposition.enabled=false",
				"--set", "imageBuild.enabled=false",
			},
		},
		{
			// Go accepts "1h30m" and the template cannot parse it. Not checking is the right
			// answer there; refusing a valid duration would be worse than a missed comparison.
			"a compound duration the check cannot parse",
			[]string{"--set", "registry.retention.window=1h30m"},
		},
	}
	for _, tc := range fine {
		t.Run(tc.name, func(t *testing.T) {
			render(t, tc.args...)
		})
	}
}

// TestTheRetentionCheckIsActuallyReached guards the defect that made the first version of this
// check pass all eleven cases above while doing nothing.
//
// The validation lived in templates/_retention.tpl with an `include` at the bottom. Helm loads
// underscore-prefixed files as definitions and never renders them, so that call never ran -- and
// every case that should have failed rendered cleanly. The check now lives behind validate.yaml,
// and this test fails if it is ever moved back somewhere Helm will not execute it.
func TestTheRetentionCheckIsActuallyReached(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "registry.retention.window=2h")
	if !strings.Contains(out, "validate.yaml") {
		t.Fatalf("the retention check must be invoked from a template Helm renders; got:\n%s", out)
	}
}
