{{- /*
D7: the retention window and the refresh interval have to be set together, and until this chart
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

{{- define "kube-oci-composer.durationHours" -}}
  {{- $d := . | toString -}}
  {{- if or (eq $d "0") (eq $d "0s") (eq $d "0m") (eq $d "0h") -}}
    {{- /* Go accepts a bare 0 for a duration, and `--set x=0` yields the number 0, not "0s". This
           is the disabled case and the one most worth catching, so it is matched explicitly rather
           than falling into the unparseable branch below. */ -}}
    {{- 0.0 -}}
  {{- else if hasSuffix "h" $d -}}
    {{- trimSuffix "h" $d | float64 -}}
  {{- else if hasSuffix "m" $d -}}
    {{- divf (trimSuffix "m" $d | float64) 60.0 -}}
  {{- else if hasSuffix "s" $d -}}
    {{- divf (trimSuffix "s" $d | float64) 3600.0 -}}
  {{- else -}}
    {{- /* Unparseable: return -1 so the caller skips rather than guesses. A Go duration can also be
           "1h30m", and rejecting a valid value would be worse than not checking it. */ -}}
    {{- -1.0 -}}
  {{- end -}}
{{- end -}}

{{- define "kube-oci-composer.checkRetention" -}}
{{- if and .Values.registry.enabled .Values.registry.retention.window -}}
{{- $window := include "kube-oci-composer.durationHours" .Values.registry.retention.window | float64 -}}
{{- $minMargin := 24.0 -}}
{{- range $c := list
      (dict "name" "imageComposition" "on" .Values.imageComposition.enabled "iv" .Values.operator.retention.refreshInterval)
      (dict "name" "imageBuild"       "on" .Values.imageBuild.enabled       "iv" .Values.imageBuild.retention.refreshInterval) -}}
{{- if $c.on -}}
{{- $interval := include "kube-oci-composer.durationHours" $c.iv | float64 -}}
{{- if eq $interval 0.0 -}}
{{- fail (printf `%s.retention.refreshInterval is 0, which disables refreshing entirely, but registry.retention.window is %s -- so the registry WILL reclaim images that live objects still reference (ADR 0031). Set an interval, or set registry.retention.window to "" to turn expiry off.` $c.name $.Values.registry.retention.window) -}}
{{- end -}}
{{- if and (gt $interval 0.0) (gt $window 0.0) -}}
{{- if lt (divf $window $interval) $minMargin -}}
{{- fail (printf "registry.retention.window (%s) is only %.1fx %s.retention.refreshInterval (%s). At least %.0fx is required, because the margin IS the guarantee: below it, a refresher down for a weekend loses images that live objects still reference (ADR 0031). Lengthen the window or shorten the interval." $.Values.registry.retention.window (divf $window $interval) $c.name $c.iv $minMargin) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
