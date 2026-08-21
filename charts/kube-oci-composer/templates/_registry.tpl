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

{{- /*
The registry's TLS material, as one JSON blob.

CALLED FROM EXACTLY ONE PLACE (registry-tls.yaml). See the note at the top of that file for why
calling it twice produces two unrelated CAs and a cluster that fails on first install.

The lookup-then-generate shape is the same one registryPassword uses: reuse what is already in the
cluster, generate only when there is nothing. Without the reuse, every `helm upgrade` would mint a
new CA and every client would have to be restarted to learn it.
*/}}
{{- define "kube-oci-composer.registryTLSMaterial" -}}
{{- $svc := printf "%s.%s.svc" (include "kube-oci-composer.registryFullname" .) .Release.Namespace -}}
{{- $names := list
      (include "kube-oci-composer.registryFullname" .)
      (printf "%s.%s" (include "kube-oci-composer.registryFullname" .) .Release.Namespace)
      $svc
      (printf "%s.%s" $svc .Values.registry.clusterDomain)
-}}
{{- /*
The public hostname too, so the same certificate is valid at the edge -- a NodePort deployment
serves this cert directly to containerd, which verifies the name in the reference.
*/}}
{{- with include "kube-oci-composer.publicHostname" . }}{{- $names = append $names . -}}{{- end -}}
{{- with .Values.registry.tls.dnsNames }}{{- $names = concat $names . -}}{{- end -}}
{{- $names = $names | uniq -}}

{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kube-oci-composer.registryTLSSecretName" .) -}}
{{- if and $existing $existing.data (index $existing.data "tls.crt") (index $existing.data "ca.crt") -}}
{{- dict "cert" (index $existing.data "tls.crt" | b64dec)
         "key"  (index $existing.data "tls.key" | b64dec)
         "ca"   (index $existing.data "ca.crt" | b64dec)
         "notAfter" (index $existing.data "notAfter" | default "" | b64dec)
         "dnsNames" $names | toJson -}}
{{- else -}}
{{- $ca := genCA (printf "%s-ca" (include "kube-oci-composer.registryFullname" .)) (int .Values.registry.tls.selfSigned.caDays) -}}
{{- $cert := genSignedCert (first $names) (.Values.registry.tls.ipAddresses | default list) $names (int .Values.registry.tls.selfSigned.certDays) $ca -}}
{{- dict "cert" $cert.Cert "key" $cert.Key "ca" $ca.Cert
         "notAfter" (now | dateModify (printf "%dh" (mul (int .Values.registry.tls.selfSigned.certDays) 24)) | date "2006-01-02T15:04:05Z07:00")
         "dnsNames" $names | toJson -}}
{{- end -}}
{{- end -}}

{{- /*
Which Secret holds the certificate. One helper because four places need to agree: the Secret, the
Certificate, the registry's volume, and the controllers' CA mount.
*/}}
{{- define "kube-oci-composer.registryTLSSecretName" -}}
{{- if .Values.registry.tls.secretName -}}
{{- .Values.registry.tls.secretName -}}
{{- else -}}
{{- printf "%s-tls" (include "kube-oci-composer.registryFullname" .) -}}
{{- end -}}
{{- end -}}

{{- /*
Refuse a certificate that has expired, or is about to.

This exists because the lookup-reuse pattern above cannot tell. sprig has no PEM parser, so without
a stored expiry the chart would happily re-emit a certificate that expired last week, forever.

The failure that produces is not an outage, it is deletion: the controllers refuse the expired
cert, the retention refresh stops running, and a registry with an expiry policy reclaims the images
live objects still reference one window later (ADR 0031). It fails silently and in the deleting
direction, which is why this is a `fail` and not a NOTES warning.

The way out is deliberately obstructive, because rotation IS that sequence:
  kubectl delete secret <release>-kube-oci-composer-registry-tls
  helm upgrade ...
  kubectl rollout restart deploy   # all three: zot reads certs once at startup

Anyone who would rather not do that by hand should use tls.mode=certManager, which exists to renew.
*/}}
{{- define "kube-oci-composer.checkRegistryCert" -}}
{{- if and .Values.registry.enabled .Values.registry.tls.enabled (eq .Values.registry.tls.mode "selfSigned") -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kube-oci-composer.registryTLSSecretName" .) -}}
{{- if and $existing $existing.data (index $existing.data "notAfter") -}}
{{- $notAfter := index $existing.data "notAfter" | b64dec -}}
{{- $left := sub (toDate "2006-01-02T15:04:05Z07:00" $notAfter).Unix now.Unix -}}
{{- $failWithin := mul (int .Values.registry.tls.selfSigned.failWithinDays) 86400 -}}
{{- if lt (int64 $left) (int64 $failWithin) -}}
{{- fail (printf `the registry's generated certificate expires at %s, which is too soon to keep serving.

It is NOT renewed automatically: the chart reuses whatever is already in the cluster, and sprig cannot read a PEM to notice. An expired certificate here does not merely break pushes -- it stops the retention refresh, and a registry with an expiry policy then reclaims images your workloads are still running, one window later (ADR 0031).

Rotate it:
  kubectl -n %s delete secret %s
  helm upgrade ...            # mints a new CA and certificate
  kubectl -n %s rollout restart deploy    # zot and both controllers read certs once at startup

Every client has to learn the new CA, including the containerd drop-in on each node. If that is more than you want to do by hand, use registry.tls.mode=certManager instead.` $notAfter .Release.Namespace (include "kube-oci-composer.registryTLSSecretName" .) .Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
Whether the controllers need a CA mounted, and where it comes from.

Empty means the registry's certificate is already trusted by the controller image -- an ACME
issuer, a corporate CA baked into the image, or TLS turned off entirely.
*/}}
{{- define "kube-oci-composer.registryCASource" -}}
{{- if and .Values.registry.enabled .Values.registry.tls.enabled .Values.registry.tls.trust.enabled -}}
{{- if .Values.registry.tls.trust.existingConfigMap -}}
configMap:{{ .Values.registry.tls.trust.existingConfigMap }}
{{- else if eq .Values.registry.tls.mode "selfSigned" -}}
configMap:{{ include "kube-oci-composer.registryFullname" . }}-ca
{{- else -}}
{{- /*
cert-manager and supplied Secrets both carry the CA in the Secret's own ca.crt key, so mounting
that directly beats copying it into a ConfigMap that can go stale behind a renewal.
*/}}
secret:{{ include "kube-oci-composer.registryTLSSecretName" . }}
{{- end -}}
{{- end -}}
{{- end -}}
