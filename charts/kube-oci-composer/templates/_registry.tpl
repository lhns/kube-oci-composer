{{- /*
Refusals about the bundled registry's own coherence.

Each one exists because the alternative is a configuration that RENDERS and then does not work --
which is the failure mode this project keeps deciding to convert into a render error instead. See
_retention.tpl for the same reasoning applied to the retention margin.
*/}}

{{- define "kube-oci-composer.checkRegistryAuth" -}}
{{- if and .Values.registry.enabled .Values.registry.auth.enabled -}}

{{- /*
Bringing your own push credential while the chart still generates the registry's password is not a
configuration with a right answer -- the two halves are a matched pair and you have replaced one of
them. The controllers would authenticate with the Secret you supplied while zot expected a password
generated here, and (worse) that generated password would be a NEW random string on every upgrade,
because the `-push` Secret `registryPassword` reads back no longer exists.

Refused rather than guessed at, with the three ways out named.
*/}}
{{- if and .Values.defaultRegistry.existingPushSecret
          (not .Values.registry.auth.password)
          (not .Values.registry.auth.existingHtpasswdSecret) -}}
{{- fail (printf `defaultRegistry.existingPushSecret is set, so the chart no longer generates the registry's password -- but registry.auth is still enabled, so the bundled registry still needs one, and nothing would agree on what it is.

Pick one:
  * registry.auth.username / registry.auth.password -- set them to the credential inside %q, so both halves match
  * registry.auth.existingHtpasswdSecret -- supply the htpasswd Secret yourself
  * registry.auth.enabled=false -- run the bundled registry without authentication (anyone in the cluster could then push to it)` .Values.defaultRegistry.existingPushSecret) -}}
{{- end -}}

{{- end -}}
{{- end -}}
