{{- /*
Retention is ONE decision with several consequences.

An operator has an opinion about how long images are kept -- "30 days" is a thing you can check
against a backup policy. Nobody has an opinion about how often a collector should sweep. So
`retention.window` is the base and everything else derives from it, because the alternative is
asking four questions whose only correct answers are functions of the first one.

  window          how long the registry keeps content nothing references. THE BASE.
  refreshInterval how often the controllers re-pull what live objects reference.
                  = window / refreshFactor. MUST be much shorter than the window: the margin is
                  the guarantee, not either number (ADR 0031). Refused below 24x.
  gcInterval      how often the registry sweeps. = window / gcFactor. Promptness only: collecting
                  late is safe, collecting early is not. Note that it does NOT straightforwardly
                  set how soon any one repository is collected -- zot walks repositories in rounds,
                  and shortening the sweep measurably made collection SLOWER in the e2e rather than
                  faster. Anything tuning this should measure rather than reason.
  gcDelay         how old something must be before it can be collected. Derived like the rest --
                  from the refresh interval, so indirectly from the window -- but never below the
                  naming-gap floor, because it also guards a WALL-CLOCK gap: a build's manifest is
                  untagged from the moment it is pushed until this controller names it (ADR 0054),
                  and untagged is what a collector reclaims. Compress the window for a test and
                  gcDelay stops following it down exactly where it must.

The window applies wherever the images live. With the bundled registry the chart both declares and
configures it; with somebody else's the operator DECLARES what their registry does, because the
refresh cadence has to derive from something and the chart cannot read their policy. The refresh
check therefore runs in both cases -- it used to be skipped when registry.enabled was false, which
withheld it from exactly the deployment that gets no other help.
*/}}

{{- /*
A Go duration in seconds. -1 means unparseable, and the caller skips rather than guesses.

Every unit is summed, because Go durations are compound: "1h30m" and "1h0m0s" are both ordinary,
and the second is what time.Duration.String() prints. An earlier version matched only a single
trailing unit, so "1h30m" took the "m" branch, trimmed it to "1h30", and sprig's float64 swallowed
the conversion error and returned 0 -- which reads as "no expiry" rather than "unparseable". The
window was then passed to zot verbatim while every guard here saw 0 and skipped, so
retention.window: 1h30m gave a registry that expired content after 90 minutes and controllers that
refreshed hourly: a margin of 1.5x, where the render is supposed to refuse anything under 24x.

Validated by reconstruction: if the units found do not reassemble the input exactly, it is not a
duration this can be trusted to have understood, and -1 says so.
*/ -}}
{{- define "kube-oci-composer.durationSeconds" -}}
  {{- $d := . | toString | trim -}}
  {{- if or (eq $d "") (eq $d "0") (eq $d "0s") (eq $d "0m") (eq $d "0h") -}}
    {{- 0.0 -}}
  {{- else -}}
    {{- /* ms/us/ns before m/s, or "1500ms" matches the "s" branch. */ -}}
    {{- $parts := regexFindAll `[0-9]+(\.[0-9]+)?(ms|us|ns|h|m|s)` $d -1 -}}
    {{- if or (eq (len $parts) 0) (ne (join "" $parts) $d) -}}
      {{- -1.0 -}}
    {{- else -}}
      {{- $total := 0.0 -}}
      {{- range $p := $parts -}}
        {{- if hasSuffix "ms" $p -}}
          {{- $total = addf $total (divf (trimSuffix "ms" $p | float64) 1000.0) -}}
        {{- else if hasSuffix "us" $p -}}
          {{- $total = addf $total (divf (trimSuffix "us" $p | float64) 1000000.0) -}}
        {{- else if hasSuffix "ns" $p -}}
          {{- $total = addf $total (divf (trimSuffix "ns" $p | float64) 1000000000.0) -}}
        {{- else if hasSuffix "h" $p -}}
          {{- $total = addf $total (mulf (trimSuffix "h" $p | float64) 3600.0) -}}
        {{- else if hasSuffix "m" $p -}}
          {{- $total = addf $total (mulf (trimSuffix "m" $p | float64) 60.0) -}}
        {{- else -}}
          {{- $total = addf $total (trimSuffix "s" $p | float64) -}}
        {{- end -}}
      {{- end -}}
      {{- $total -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- /*
Seconds back to the largest whole unit, so a derived value reads like one a human would have
written. 3600 is "1h", not "3600s" -- which also means the derived defaults render identically to
the literals they replace, and a diff of the rendered output shows nothing at all.
*/ -}}
{{- define "kube-oci-composer.formatDuration" -}}
  {{- $s := . | float64 | floor | int64 -}}
  {{- if and (ge $s 3600) (eq (mod $s 3600) 0) -}}
    {{- printf "%dh" (div $s 3600) -}}
  {{- else if and (ge $s 60) (eq (mod $s 60) 0) -}}
    {{- printf "%dm" (div $s 60) -}}
  {{- else -}}
    {{- printf "%ds" $s -}}
  {{- end -}}
{{- end -}}

{{- /* 0 means the operator declared no expiry at all. */ -}}
{{- define "kube-oci-composer.windowSeconds" -}}
  {{- include "kube-oci-composer.durationSeconds" .Values.retention.window -}}
{{- end -}}

{{- /*
The refresh interval both controllers use unless one overrides it.

With no window there is nothing to derive from and nothing to outrun -- but an operator who is WRONG
about their registry having no expiry loses images, and that mistake is the unrecoverable one. So
this still refreshes, on a plain hourly default, rather than not at all.
*/ -}}
{{- define "kube-oci-composer.refreshInterval" -}}
  {{- if .Values.retention.refreshInterval -}}
    {{- .Values.retention.refreshInterval -}}
  {{- else -}}
    {{- $w := include "kube-oci-composer.windowSeconds" . | float64 -}}
    {{- if le $w 0.0 -}}
      {{- "1h" -}}
    {{- else -}}
      {{- include "kube-oci-composer.formatDuration" (divf $w (.Values.retention.refreshFactor | float64)) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- /*
Per-component, so the two may differ; empty means the shared one.

NOT `default`, which treats 0 as absent -- and 0 is a real setting here that means "never refresh".
Falling back on it would turn the one value the checks must catch into the one they cannot see.
*/ -}}
{{- define "kube-oci-composer.composerRefreshInterval" -}}
  {{- $v := .Values.operator.retention.refreshInterval | toString -}}
  {{- if eq $v "" -}}{{- include "kube-oci-composer.refreshInterval" . -}}{{- else -}}{{- $v -}}{{- end -}}
{{- end -}}

{{- define "kube-oci-composer.builderRefreshInterval" -}}
  {{- $v := .Values.imageBuild.retention.refreshInterval | toString -}}
  {{- if eq $v "" -}}{{- include "kube-oci-composer.refreshInterval" . -}}{{- else -}}{{- $v -}}{{- end -}}
{{- end -}}

{{- /* Whether any enabled component actually falls back to the derived interval. */ -}}
{{- define "kube-oci-composer.usesDerivedRefresh" -}}
  {{- if and .Values.imageComposition.enabled (eq (.Values.operator.retention.refreshInterval | toString) "") -}}yes
  {{- else if and .Values.imageBuild.enabled (eq (.Values.imageBuild.retention.refreshInterval | toString) "") -}}yes
  {{- end -}}
{{- end -}}

{{- define "kube-oci-composer.gcInterval" -}}
  {{- if .Values.registry.retention.gcInterval -}}
    {{- .Values.registry.retention.gcInterval -}}
  {{- else -}}
    {{- $w := include "kube-oci-composer.windowSeconds" . | float64 -}}
    {{- if le $w 0.0 -}}
      {{- "6h" -}}
    {{- else -}}
      {{- include "kube-oci-composer.formatDuration" (divf $w (.Values.registry.retention.gcFactor | float64)) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- /*
The naming gap: how long a build's manifest exists with no tag pointing at it.

The controller re-observes a running Job every buildPollInterval, so that bounds how long after the
push it learns the digest and applies the names. Tripled for the round trips it makes first, and
because losing this race costs a build.
*/ -}}
{{- define "kube-oci-composer.namingGapSeconds" -}}
  {{- $poll := include "kube-oci-composer.durationSeconds" .Values.imageBuild.buildPollInterval | float64 -}}
  {{- mulf (max $poll 1.0) 3.0 -}}
{{- end -}}

{{- /* Derived like the rest, but never below the naming gap. See the header. */ -}}
{{- define "kube-oci-composer.gcDelay" -}}
  {{- if .Values.registry.retention.gcDelay -}}
    {{- .Values.registry.retention.gcDelay -}}
  {{- else -}}
    {{- $refresh := include "kube-oci-composer.durationSeconds" (include "kube-oci-composer.refreshInterval" .) | float64 -}}
    {{- $floor := include "kube-oci-composer.namingGapSeconds" . | float64 -}}
    {{- include "kube-oci-composer.formatDuration" (max $refresh $floor) -}}
  {{- end -}}
{{- end -}}

{{- /*
D7: the refresh interval and the retention window have to be set together, and until this chart
existed they lived in different systems -- a controller flag and a registry's config -- so nothing
could compare them. Now one render sees both, so it checks.

The RATIO is the guarantee. A window of 720h against an interval of 1h means refreshing has to fail
continuously for a month before an image a live object still references is at risk. Shrink the
window without shrinking the interval and that margin quietly becomes a race, whose failure mode is
deletion -- which is why this fails the render rather than warning in NOTES nobody reads.

MINIMUM MARGIN is 24: the window must be at least a day's worth of refreshes wide. Below that a
weekend of a broken registry, a controller crash-looping on a bad flag, or a long node drain lands
inside the window. It is deliberately far below the default's 720, because this exists to catch
configurations that are WRONG, not to enforce the default on people who have thought about it.

Both controllers are checked, because either one going quiet loses its own objects' images.
*/}}
{{- define "kube-oci-composer.checkRetention" -}}
{{- if .Values.retention.window -}}
{{- $window := include "kube-oci-composer.windowSeconds" . | float64 -}}
{{- /*
A window this cannot parse is refused rather than passed through.

It reaches zot verbatim, so leaving it alone would hand the registry a policy it cannot read while
every check here skips -- the parser answers -1 for "I did not understand this", and -1 is not a
duration to compare margins against. Failing here says which value is wrong; failing at the
registry says a policy is invalid, hours later, somewhere else.
*/ -}}
{{- if lt $window 0.0 -}}
{{- fail (printf "retention.window (%s) is not a duration this chart can read. Use a Go duration such as 720h, 30m or 1h30m -- it is passed to the registry as its expiry policy, and the refresh margin is checked against it." $.Values.retention.window) -}}
{{- end -}}
{{- $minMargin := 24.0 -}}
{{- range $c := list
      (dict "name" "imageComposition" "on" .Values.imageComposition.enabled "iv" (include "kube-oci-composer.composerRefreshInterval" .))
      (dict "name" "imageBuild"       "on" .Values.imageBuild.enabled       "iv" (include "kube-oci-composer.builderRefreshInterval" .)) -}}
{{- if $c.on -}}
{{- $interval := include "kube-oci-composer.durationSeconds" $c.iv | float64 -}}
{{- if eq $interval 0.0 -}}
{{- fail (printf `%s refreshing is disabled (interval 0), but retention.window is %s -- so the registry WILL reclaim images that live objects still reference (ADR 0031). Set an interval, or set retention.window to "" to turn expiry off.` $c.name $.Values.retention.window) -}}
{{- end -}}
{{- if and (gt $interval 0.0) (gt $window 0.0) -}}
{{- if lt (divf $window $interval) $minMargin -}}
{{- fail (printf "retention.window (%s) is only %.1fx %s's refresh interval (%s). At least %.0fx is required, because the margin IS the guarantee: below it, a refresher down for a weekend loses images that live objects still reference (ADR 0031). Lengthen the window, lower retention.refreshFactor, or set the interval explicitly." $.Values.retention.window (divf $window $interval) $c.name $c.iv $minMargin) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
A window short enough to derive a nonsense refresh interval is refused rather than rendered.

720 is a sensible factor against 30 days and a ridiculous one against two hours, where it derives a
ten-second refresh and both controllers re-pull every live image six times a minute. The floor
applies only to the DERIVED value: setting retention.refreshInterval explicitly is the escape hatch,
and is how the e2e runs a one-second refresh on purpose.
*/}}
{{- define "kube-oci-composer.checkDerivedFloors" -}}
{{- if and .Values.retention.window (not .Values.retention.refreshInterval) (include "kube-oci-composer.usesDerivedRefresh" .) -}}
{{- $w := include "kube-oci-composer.windowSeconds" . | float64 -}}
{{- $derived := include "kube-oci-composer.durationSeconds" (include "kube-oci-composer.refreshInterval" .) | float64 -}}
{{- if and (gt $w 0.0) (gt $derived 0.0) (lt $derived 30.0) -}}
{{- fail (printf "retention.window (%s) over retention.refreshFactor (%v) derives a refresh interval of %s, which would have both controllers re-pull every live image that often. Lengthen the window, lower the factor, or set retention.refreshInterval explicitly if you mean it." $.Values.retention.window $.Values.retention.refreshFactor (include "kube-oci-composer.refreshInterval" .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
The OTHER race, and it is not about the window at all.

A build uploads its image with NO TAG and the controller names it a moment later, which is what
makes the conflict check exact (ADR 0054). Until that name is applied the manifest is untagged, and
untagged is precisely what the collector reclaims. When it was the repository's only content the
repository goes with it, which is why losing this race reads as NAME_UNKNOWN rather than a missing
manifest.

keepUntagged.pushedWithin protects a freshly pushed manifest in any repository the policy MATCHES,
so for the shipped repositories: ["**"] this guard is belt-and-braces. It still earns its place:
scope repositories to a prefix and everything outside it matches no policy at all, where zot's
default is to collect untagged manifests and there is no keepUntagged to save them. That is the
configuration the e2e runs, and the one this was written after.

Refused rather than warned, for the same reason as the window check: the failure mode is deletion.
deleteUntagged: false removes the race instead of out-running it and is accepted at any delay.

Gated on registry.enabled because gcDelay is zot's setting. The race is NOT zot-specific -- any
registry that reclaims untagged manifests can take a build's output before it is named -- so the
external-registry path documents it instead, and the controller's Pending message names the cause.
*/}}
{{- define "kube-oci-composer.checkUntaggedWindow" -}}
{{- if and .Values.registry.enabled .Values.registry.retention.deleteUntagged .Values.imageBuild.enabled -}}
{{- $delay := include "kube-oci-composer.durationSeconds" (include "kube-oci-composer.gcDelay" .) | float64 -}}
{{- $gap := include "kube-oci-composer.namingGapSeconds" . | float64 -}}
{{- if and (ge $delay 0.0) (lt $delay $gap) -}}
{{- fail (printf "the registry's gcDelay resolves to %s, shorter than the %s a build's manifest can spend untagged while this controller names it (3x imageBuild.buildPollInterval, which is %s). The collector would be free to reclaim a build's own output. Lengthen gcDelay, shorten buildPollInterval, or set registry.retention.deleteUntagged=false to remove the race instead of out-running it. See ADR 0054." (include "kube-oci-composer.gcDelay" .) (include "kube-oci-composer.formatDuration" $gap) $.Values.imageBuild.buildPollInterval) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
Sweeping less often than the window makes the window a lower bound rather than a description:
content stays pullable long after it was due to go, and an operator reading `window` is told
something untrue. Collecting LATE is safe, so this is a misconfiguration rather than a hazard -- but
one worth refusing while the render can still say so.
*/}}
{{- define "kube-oci-composer.checkGcInterval" -}}
{{- if and .Values.registry.enabled .Values.retention.window -}}
{{- $window := include "kube-oci-composer.windowSeconds" . | float64 -}}
{{- $sweep := include "kube-oci-composer.durationSeconds" (include "kube-oci-composer.gcInterval" .) | float64 -}}
{{- if and (gt $window 0.0) (gt $sweep $window) -}}
{{- fail (printf "the registry's gcInterval resolves to %s, longer than retention.window (%s). Nothing would be collected until long after it expired, so the window would describe nothing. Raise registry.retention.gcFactor or set gcInterval explicitly." (include "kube-oci-composer.gcInterval" .) $.Values.retention.window) -}}
{{- end -}}
{{- end -}}
{{- end -}}
