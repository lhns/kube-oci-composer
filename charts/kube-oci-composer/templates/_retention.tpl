{{- /*
Retention is ONE decision: `retention.window` is the base and everything else derives from it.

  window          how long the registry keeps content nothing references.
  refreshInterval how often the controllers re-pull what live objects reference.
                  = window / refreshFactor. The margin is the guarantee (ADR 0031); refused below 24x.
  gcInterval      how often the registry sweeps. = window / gcFactor. Promptness only: late is safe,
                  early is not. It does not simply set how soon a repository is collected (zot walks
                  repositories in rounds); measure before tuning it.
  gcDelay         minimum age before collection. Derived from the refresh interval, but never below
                  the naming-gap floor: a build's manifest is untagged from push until the controller
                  names it (ADR 0054), and untagged is what a collector reclaims.

With an external registry the operator DECLARES its window, so the refresh check runs in both cases.
*/}}

{{- /*
A Go duration in seconds; -1 means unparseable and callers skip rather than guess. Compound
durations ("1h30m") are summed, and the parse is validated by reassembling the input, so a partial
match cannot silently become 0 ("no expiry").
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
Seconds back to the largest whole unit ("1h", not "3600s"), so derived values read like the
literals they replace.
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
The refresh interval both controllers use unless one overrides it. With no window it still refreshes
hourly: an operator wrong about "no expiry" would otherwise lose images.
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
Per-component override; empty means the shared one. Not `default`, which treats 0 as absent -- and
0 ("never refresh") is exactly what the checks must see.
*/ -}}
{{- define "kube-oci-composer.composerRefreshInterval" -}}
  {{- $v := .Values.operator.retention.refreshInterval | toString -}}
  {{- if eq $v "" -}}{{- include "kube-oci-composer.refreshInterval" . -}}{{- else -}}{{- $v -}}{{- end -}}
{{- end -}}

{{- define "kube-oci-composer.builderRefreshInterval" -}}
  {{- $v := .Values.imageBuild.retention.refreshInterval | toString -}}
  {{- if eq $v "" -}}{{- include "kube-oci-composer.refreshInterval" . -}}{{- else -}}{{- $v -}}{{- end -}}
{{- end -}}

{{- /* Whether any enabled component falls back to the derived interval. */ -}}
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
The naming gap: how long a build's manifest can sit untagged. The controller names it within one
buildPollInterval of the push; tripled for its round trips, since losing this race costs a build.
*/ -}}
{{- define "kube-oci-composer.namingGapSeconds" -}}
  {{- $poll := include "kube-oci-composer.durationSeconds" .Values.imageBuild.buildPollInterval | float64 -}}
  {{- mulf (max $poll 1.0) 3.0 -}}
{{- end -}}

{{- /* Derived like the rest, but never below the naming gap. */ -}}
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
D7: the refresh interval must be far shorter than the window. The RATIO is the guarantee: at 720h
against 1h, refreshing must fail for a month before a live image is at risk. Minimum margin 24 (a
day's worth of refreshes) -- far below the default, to catch wrong configurations rather than
enforce the default. Both controllers are checked; either going quiet loses its own images.
*/}}
{{- define "kube-oci-composer.checkRetention" -}}
{{- if .Values.retention.window -}}
{{- $window := include "kube-oci-composer.windowSeconds" . | float64 -}}
{{- /* An unparseable window is refused: it reaches zot verbatim while every check here would skip it. */ -}}
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
Refuse a DERIVED refresh interval under 30s (e.g. factor 720 against a 2h window). Setting
retention.refreshInterval explicitly bypasses this; the e2e does so on purpose.
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
The naming-gap race (ADR 0054): a build pushes untagged and the controller names it later, so gcDelay
must outlast that gap or the collector can take the build's output (seen as NAME_UNKNOWN when it was
the repository's only content). With keepUntagged off (ADR 0060) this is the only cover, and
repositories outside a scoped policy are collected by zot's default regardless. Costs a rebuild, not
data. deleteUntagged: false removes the race and is accepted at any delay.

Gated on registry.enabled because gcDelay is zot's setting; for an external registry the race is
documented instead.
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
A sweep less often than the window makes the window describe nothing. Late collection is safe, so
this is a misconfiguration rather than a hazard, but still refused.
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
