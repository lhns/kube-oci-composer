{{/*
The build-determining part of the ImageComposition spec, and nothing else.

This partial is the single source of two things: what gets built, and what the result is called.
Keeping it separate is the whole trick — it is rendered once and hashed, then rendered again into
the ImageComposition, so the tag cannot disagree with the spec that produced it.

What must NOT appear here:

  publish / push   Where the artifact goes does not change what it is. Including them would also
                   be circular, since publish.tags is derived from this hash.
  interval         Reconcile cadence is not an input.
  suspend          Nor is whether reconciling happens at all.

What MUST appear here: base, layers, config — everything the composer feeds into assembly.
*/}}
{{- define "spec-hash-tag.build" -}}
layers:
{{- toYaml .Values.build.layers | nindent 0 }}
{{- end -}}

{{/*
The tag itself.

Hashing the RENDERED partial rather than .Values is deliberate: a change to the template that
alters the spec without altering values — a new field, a changed default, a conditional — must
still move the tag. Hashing values alone would miss it and republish different content under an
unchanged tag, which is exactly the hazard `onConflict: Fail` exists to catch.

The `s` prefix is not decoration: an OCI tag may not begin with `-` or `.`, and a leading letter
keeps the value valid whatever the hash starts with. 16 hex characters is 64 bits, which is
ample here — and a collision would be caught rather than silently published, because two
different specs writing one tag is precisely what `onConflict: Fail` refuses.
*/}}
{{- define "spec-hash-tag.tag" -}}
s{{ include "spec-hash-tag.build" . | sha256sum | trunc 16 }}
{{- end -}}

{{/*
The full pullable reference used by the workload. Derived from the same tag, so there is exactly
one place either can change.
*/}}
{{- define "spec-hash-tag.image" -}}
{{ .Values.registry }}/{{ .Values.repository }}:{{ include "spec-hash-tag.tag" . }}
{{- end -}}
