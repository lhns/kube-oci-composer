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
Whether the controllers need the registry's CA mounted.

Empty means its certificate is already trusted by the controller image: an ACME issuer, a corporate
CA baked in, or TLS off entirely. Callers use it only as a truthiness test -- registryCAVolume and
registryCAVolumeMount below emit the YAML.
*/}}
{{- define "kube-oci-composer.registryCAWanted" -}}
{{- if and .Values.registry.enabled .Values.registry.tls.enabled .Values.registry.tls.trust.enabled -}}
yes
{{- end -}}
{{- end -}}

{{- /*
Where the CA comes from, as a volume entry.

cert-manager and operator-supplied Secrets both carry it in the Secret's own ca.crt key, so it is
mounted from there rather than copied into a ConfigMap that could go stale behind a renewal. Only
the self-signed mode, where the chart generates the CA itself, uses a ConfigMap.
*/}}
{{- define "kube-oci-composer.registryCAVolume" -}}
{{- if include "kube-oci-composer.registryCAWanted" . -}}
- name: registry-ca
{{- if .Values.registry.tls.trust.existingConfigMap }}
  configMap:
    name: {{ .Values.registry.tls.trust.existingConfigMap }}
{{- else if eq .Values.registry.tls.mode "selfSigned" }}
  configMap:
    name: {{ include "kube-oci-composer.registryFullname" . }}-ca
{{- else }}
  secret:
    secretName: {{ include "kube-oci-composer.registryTLSSecretName" . }}
    items:
      - key: ca.crt
        path: ca.crt
{{- end }}
{{- end -}}
{{- end -}}

{{- define "kube-oci-composer.registryCAVolumeMount" -}}
{{- if include "kube-oci-composer.registryCAWanted" . -}}
- name: registry-ca
  mountPath: {{ include "kube-oci-composer.registryCADir" . }}
  readOnly: true
{{- end -}}
{{- end -}}

{{- /*
Where the CA is mounted, and the file --registry-ca-file names inside it. One definition because
the mount path and the flag have to agree, in two Deployments.
*/}}
{{- define "kube-oci-composer.registryCADir" -}}/etc/oci-composer/registry-ca{{- end -}}
{{- define "kube-oci-composer.registryCAFile" -}}{{ include "kube-oci-composer.registryCADir" . }}/ca.crt{{- end -}}

{{- /*
Refusals about clustering.

Every one of these is a combination that RENDERS and then does not work, mostly by losing data
rather than by erroring -- which is why they fail the install instead of warning in NOTES.
*/}}
{{- /*
Whether zot's config has to be a Secret rather than a ConfigMap.

zot's redis driver takes credentials only inside the URL -- redis://user:pass@host -- so a
credentialed cache URL puts a password in the rendered config. A ConfigMap is readable in every
`kubectl describe`, which is the rule TestChartCredentialsAreNotFlags already enforces for the
composer's own S3, so in that case the whole config moves to a Secret.

Kept conditional rather than always-Secret so the ordinary install still has an inspectable
ConfigMap; nothing is hidden unless there is something to hide.
*/}}
{{- define "kube-oci-composer.registryConfigIsSecret" -}}
{{- if and (eq .Values.registry.cache.driver "redis") (contains "@" .Values.registry.cache.redis.url) -}}
true
{{- end -}}
{{- end -}}

{{- define "kube-oci-composer.checkRegistryReplicas" -}}
{{- $r := .Values.registry -}}

{{- /*
registry.cluster is gone. Helm ignores unknown --set paths in silence, so without this an operator
carrying `--set registry.cluster.enabled=true` from 0.5.0-rc would get a working install with
one pod and no indication that their replica count had evaporated.
*/}}
{{- if $r.cluster -}}
{{- fail "registry.cluster no longer exists. It ran zot's scale-out mode, which SHARDS: each repository lived on exactly one member, so a member going down took ~1/N of the registry with it. The replacement is registry.readReplicas, which replicates reads instead -- see docs/adr/0041-one-writer-many-readers.md." -}}
{{- end -}}

{{- if and $r.enabled (lt (int $r.readReplicas) 0) -}}
{{- fail (printf "registry.readReplicas is %d. It counts replicas IN ADDITION to the single writer, so the smallest meaningful value is 0." (int $r.readReplicas)) -}}
{{- end -}}

{{- if and $r.enabled (gt (int $r.readReplicas) 0) -}}

{{- if eq $r.cache.driver "none" -}}
{{- fail "registry.readReplicas > 0 requires registry.cache.driver (redis or dynamodb). The default metadata store is BoltDB, a file one process opens exclusively, so replicas cannot share it. Worse than the sharing failure: per-pod metadata would mean `extensions.search` records a pull only on the pod that served it, so a refresh landing on one pod would not save the image from the writer's collector, and retention would delete content that is still in use (ADR 0031)." -}}
{{- end -}}

{{- if and $r.persistence.enabled (ne $r.persistence.accessMode "ReadWriteMany") -}}
{{- fail (printf "registry.readReplicas > 0 needs registry.persistence.accessMode=ReadWriteMany; it is %q. A second pod cannot mount a ReadWriteOnce volume and will sit in Multi-Attach error indefinitely. Note the chart can only check what you ASKED for -- whether the StorageClass really provides multi-writer access is not visible here, so confirm the claim with `kubectl get pvc` before relying on the replicas." $r.persistence.accessMode) -}}
{{- end -}}

{{- if and (eq $r.storage.driver "local") (not $r.persistence.enabled) -}}
{{- fail "registry.readReplicas > 0 with registry.storage.driver=local needs registry.persistence.enabled=true. An emptyDir is per-pod, so the replicas would be unrelated registries behind one Service name and a pull would 404 or not depending which one it reached -- and every restart would lose an ImageBuild's only copy (ADR 0025)." -}}
{{- end -}}

{{- end -}}

{{- /*
These apply whether or not there are replicas: an operator may use S3 with a single pod.
*/}}
{{- if and $r.enabled (eq $r.storage.driver "s3") (not $r.storage.s3.bucket) -}}
{{- fail "registry.storage.driver=s3 needs registry.storage.s3.bucket." -}}
{{- end -}}
{{- if and $r.enabled (eq $r.cache.driver "redis") (not $r.cache.redis.url) -}}
{{- fail "registry.cache.driver=redis needs registry.cache.redis.url." -}}
{{- end -}}
{{- if and $r.enabled (eq $r.cache.driver "dynamodb") (ne $r.storage.driver "s3") -}}
{{- fail "registry.cache.driver=dynamodb is only supported with registry.storage.driver=s3. zot refuses local storage with a non-redis remote database at startup, so this would crashloop rather than degrade." -}}
{{- end -}}

{{- end -}}

{{- define "kube-oci-composer.checkRegistryMigration" -}}
{{- if .Values.registry.enabled -}}
{{- $name := include "kube-oci-composer.registryFullname" . -}}
{{- if lookup "apps/v1" "Deployment" .Release.Namespace $name -}}
{{- fail (printf `the registry used to be a Deployment and is now a StatefulSet, so the old one has to go first:

  kubectl -n %s delete deployment %s
  helm upgrade ...

Your images are NOT affected: the PVC is annotated helm.sh/resource-policy: keep and was never owned by the Deployment, so the new pod mounts the same volume.

Without this, Helm creates the StatefulSet while the Deployment's ReplicaSet still owns a pod matching the same selector, and the two fight over one ReadWriteOnce volume.` .Release.Namespace $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
The registry arguments both controllers take.

One definition because they are the same questions for both -- where to publish, what a workload
pulls, whose credential, what to trust, what to attach -- and a flag added to one and not the other
is a controller that quietly behaves differently from its sibling.

Call with the supplyChain block, which is the only thing that differs:

    {{ include "kube-oci-composer.registryArgs" (dict "ctx" $ "supplyChain" .Values.operator.supplyChain) }}
*/}}
{{- define "kube-oci-composer.registryArgs" -}}
{{- $ctx := .ctx -}}
{{- $sc := .supplyChain -}}
{{- with (include "kube-oci-composer.defaultRegistry" $ctx) }}
- --default-registry={{ . }}
{{- end }}
{{- with (include "kube-oci-composer.publicRegistry" $ctx) }}
- --public-registry-host={{ . }}
{{- end }}
{{- with (include "kube-oci-composer.pushSecretName" $ctx) }}
- --default-push-secret={{ . }}
{{- end }}
{{- with (include "kube-oci-composer.insecureRegistries" $ctx) }}
- --insecure-registry={{ . }}
{{- end }}
{{- if (include "kube-oci-composer.registryCAWanted" $ctx) }}
- --registry-ca-file={{ include "kube-oci-composer.registryCAFile" $ctx }}
{{- end }}
{{- if $sc.sbom }}
- --sbom
{{- end }}
{{- if $sc.provenance }}
- --provenance
{{- end }}
{{- if $sc.signing.enabled }}
- --signing-key-secret={{ required "supplyChain.signing.existingSecret is required when signing is enabled" $sc.signing.existingSecret }}
{{- end }}
{{- end -}}
