package controller

import (
	"strings"
	"testing"
)

// TestChartRefusesARetentionMarginThatIsTooThin (threat-model gap D7): ADR 0031's guarantee is the
// RATIO of retention window to refresh interval, i.e. how long refreshing may be broken before live
// content is reclaimed. A thin margin fails the render, because its symptom is a deleted image one
// window later.
func TestChartRefusesARetentionMarginThatIsTooThin(t *testing.T) {
	tooThin := []struct {
		name string
		args []string
		want string
	}{
		{
			// Checked for external registries too.
			name: "an external registry whose declared window the refresher cannot outrun",
			args: []string{
				"--set", "registry.enabled=false",
				"--set", "defaultRegistry.host=ghcr.io/example",
				"--set", "retention.window=2h",
				"--set", "retention.refreshInterval=1h",
			},
			want: "only 2.0x",
		},
		{
			// The interval is derived from the window, so a thin margin needs it pinned.
			name: "a window barely wider than an interval someone pinned",
			args: []string{
				"--set", "retention.window=2h",
				"--set", "retention.refreshInterval=1h",
			},
			want: "only 2.0x",
		},
		{
			// A bare short window derives an absurd interval (factor 720: 2h -> 10s).
			name: "a window too short to derive a sane interval from",
			args: []string{"--set", "retention.window=2h"},
			want: "derives a refresh interval of 10s",
		},
		{
			// Each controller refreshes its own objects' images, so both intervals are checked.
			name: "the builder's interval alone",
			args: []string{"--set", "imageBuild.retention.refreshInterval=48h"},
			want: "imageBuild's refresh interval",
		},
		{
			name: "refreshing disabled with a window still set",
			args: []string{"--set", "operator.retention.refreshInterval=0"},
			want: "refreshing is disabled",
		},
		{
			// `--set x=0s` and `--set x=0` reach the template as different types. Both mean off.
			name: "refreshing disabled, written as 0s",
			args: []string{"--set", "imageBuild.retention.refreshInterval=0s"},
			want: "refreshing is disabled",
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

// TestChartAcceptsRetentionSettingsThatAreMerelyUnusual: the required margin is 24x, far below the
// default 720x, so the guard catches wrong settings without enforcing the default.
func TestChartAcceptsRetentionSettingsThatAreMerelyUnusual(t *testing.T) {
	fine := []struct {
		name string
		args []string
	}{
		{"the defaults", nil},
		{"exactly the minimum margin", []string{"--set", "retention.window=24h"}},
		{
			"a compressed but proportionate pair",
			[]string{
				"--set", "retention.window=30m",
				"--set", "operator.retention.refreshInterval=1m",
				"--set", "imageBuild.retention.refreshInterval=1m",
			},
		},
		{
			// The window declares the storing registry's expiry, so "expires nothing" is what makes
			// refreshing off safe, not the absence of the bundled registry.
			"refreshing off against an external registry that expires nothing",
			[]string{
				"--set", "operator.retention.refreshInterval=0",
				"--set", "retention.window=",
				"--set", "registry.enabled=false",
				"--set", "defaultRegistry.host=ghcr.io/example",
			},
		},
		{
			"refreshing off when the registry expires nothing",
			[]string{
				"--set", "operator.retention.refreshInterval=0",
				"--set", "retention.window=",
			},
		},
		{
			// A controller that is not installed cannot fail to refresh.
			"a thin margin with both controllers disabled",
			[]string{
				"--set", "retention.window=2h",
				"--set", "imageComposition.enabled=false",
				"--set", "imageBuild.enabled=false",
			},
		},
		{
			// A compound duration is parsed, and accepted with a proportionate interval.
			"a compound duration, parsed, with a proportionate interval",
			[]string{
				"--set", "retention.window=1h30m",
				"--set", "retention.refreshInterval=1m",
			},
		},
	}
	for _, tc := range fine {
		t.Run(tc.name, func(t *testing.T) {
			render(t, tc.args...)
		})
	}
}

// TestCompoundDurationsAreUnderstood: the window reaches zot verbatim while the guards read it
// through the template's own parser, so compound durations ("1h30m", "1h0m0s") must parse to their
// real value, never to 0 ("no expiry"), which would skip every check.
func TestCompoundDurationsAreUnderstood(t *testing.T) {
	// Each pair is one duration written two ways; the chart must treat them alike.
	for _, tc := range []struct{ compound, simple string }{
		{"1h30m", "90m"},
		{"1h0m0s", "60m"},
		{"0h30m0s", "30m"},
	} {
		t.Run(tc.compound, func(t *testing.T) {
			compound := renderRawExpectingFailure(t, append(append([]string{}, installable...),
				"--set", "retention.window="+tc.compound)...)
			simple := renderRawExpectingFailure(t, append(append([]string{}, installable...),
				"--set", "retention.window="+tc.simple)...)

			// Both derive a refused refresh interval. The messages quote the window as written,
			// so compare only the derivation part.
			for _, want := range []string{"derives a refresh interval of"} {
				if !strings.Contains(compound, want) {
					t.Errorf("%s was not understood as a duration; the guard did not fire:\n%s",
						tc.compound, compound)
				}
				if !strings.Contains(simple, want) {
					t.Errorf("%s did not fire the guard either, so this test proves nothing:\n%s",
						tc.simple, simple)
				}
			}
		})
	}
}

// TestAnUnparseableWindowDoesNotReadAsNoExpiry: an unparseable window must be refused, never read
// as zero ("no expiry"), which switches the checks off.
func TestAnUnparseableWindowDoesNotReadAsNoExpiry(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "retention.window=soon")
	if !strings.Contains(out, "is not a duration") {
		t.Errorf("an unparseable window was accepted. It reaches zot verbatim as the expiry "+
			"policy while every check here skips it, because the parser answers -1 and -1 is not "+
			"a margin to compare against:\n%s", out)
	}
}
