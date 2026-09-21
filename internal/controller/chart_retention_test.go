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
			// The check used to be gated on the bundled registry being installed, so the
			// deployment with the LEAST help from the chart -- somebody else's registry, whose
			// policy it cannot read -- was the one it declined to check.
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
			// Needs the interval stated, now that it is normally DERIVED from the window: shrink
			// the window alone and the interval shrinks with it, so the margin holds and there is
			// nothing to catch. A thin margin is only reachable by overriding one of the two,
			// which is the point of deriving them.
			name: "a window barely wider than an interval someone pinned",
			args: []string{
				"--set", "retention.window=2h",
				"--set", "retention.refreshInterval=1h",
			},
			want: "only 2.0x",
		},
		{
			// The failure a bare short window produces instead: 720 is a sensible factor against
			// 30 days and derives a 10-second refresh against two hours.
			name: "a window too short to derive a sane interval from",
			args: []string{"--set", "retention.window=2h"},
			want: "derives a refresh interval of 10s",
		},
		{
			// Each controller refreshes its own objects' images, so the builder's interval is as
			// load-bearing as the composer's. An earlier version checked only one.
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
			// An external registry that expires nothing. The window is a DECLARATION about
			// whichever registry stores the images, so saying it expires nothing is what makes
			// turning refreshing off safe -- not the absence of the bundled one.
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
			// A compound duration, now that the parser sums units instead of matching one trailing
			// one. This case used to assert the opposite -- that "1h30m" rendered BECAUSE the check
			// could not read it -- on the reasoning that refusing a valid duration would be worse
			// than a missed comparison. That was a fair trade only while the parser was the
			// limitation: it returned 0 for "1h30m", 0 means "no expiry", and the window still
			// reached zot verbatim. So the missed comparison was not missed evenly; it was missed
			// exactly where a real 90-minute expiry was configured.
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

// A compound Go duration must mean what it says, because the window reaches zot verbatim while
// every guard here reads it through the template's own parser.
//
// "1h30m" and "1h0m0s" are ordinary durations -- the second is what time.Duration.String() prints.
// An earlier parser matched only a single trailing unit, so "1h30m" trimmed to "1h30", sprig's
// float64 swallowed the error and returned 0, and 0 reads as "no expiry". The registry got a real
// 90-minute window while checkRetention saw nothing to check: a 1.5x margin rendered happily, where
// anything under 24x is supposed to be refused.
func TestCompoundDurationsAreUnderstood(t *testing.T) {
	// Each pair is the same duration written two ways. Whatever the chart does with one it must do
	// with the other -- that equivalence is the property, not any particular outcome.
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

			// Both are short enough that the derived refresh interval is refused. The messages
			// quote the window as written, so compare the part that describes the derivation.
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

// A window this cannot parse must read as unparseable, never as zero -- zero is "no expiry", which
// switches off the very checks that would have caught the misconfiguration.
func TestAnUnparseableWindowDoesNotReadAsNoExpiry(t *testing.T) {
	out := renderExpectingFailure(t, "--set", "retention.window=soon")
	if !strings.Contains(out, "is not a duration") {
		t.Errorf("an unparseable window was accepted. It reaches zot verbatim as the expiry "+
			"policy while every check here skips it, because the parser answers -1 and -1 is not "+
			"a margin to compare against:\n%s", out)
	}
}
