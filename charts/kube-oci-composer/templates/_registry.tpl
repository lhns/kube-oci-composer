{{- /*
Refusals about the bundled registry's own coherence: each catches a configuration that would render
and then not work. See _retention.tpl for the retention checks.
*/}}

{{- define "kube-oci-composer.checkRegistryAuth" -}}
{{- if and .Values.registry.enabled .Values.registry.auth.enabled -}}

{{- /*
A supplied push credential with a chart-generated registry password cannot match. Worse, with no
`-push` Secret to read back, registryPassword mints a new password on every upgrade.
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
The registry's TLS material as one JSON blob. Call it ONLY from registry-tls.yaml: genCA is random,
so a second caller would produce an unrelated CA (see that file). Reuses the Secret already in the
cluster, so an upgrade does not mint a new CA.
*/}}
{{- define "kube-oci-composer.registryTLSMaterial" -}}
{{- $svc := printf "%s.%s.svc" (include "kube-oci-composer.registryFullname" .) .Release.Namespace -}}
{{- $names := list
      (include "kube-oci-composer.registryFullname" .)
      (printf "%s.%s" (include "kube-oci-composer.registryFullname" .) .Release.Namespace)
      $svc
      (printf "%s.%s" $svc .Values.registry.clusterDomain)
-}}
{{- /* The public hostname too: a NodePort serves this cert directly to containerd. */}}
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
The Secret holding the certificate; the Secret, the Certificate, the registry volume and the
controllers' CA mount must agree on it.
*/}}
{{- define "kube-oci-composer.registryTLSSecretName" -}}
{{- if .Values.registry.tls.secretName -}}
{{- .Values.registry.tls.secretName -}}
{{- else -}}
{{- printf "%s-tls" (include "kube-oci-composer.registryFullname" .) -}}
{{- end -}}
{{- end -}}

{{- /*
Refuse a self-signed certificate that has expired or is about to. The lookup-reuse above cannot tell
(sprig has no PEM parser), so the expiry is stored at generation. An expired cert stops the
retention refresh, which deletes live images one window later (ADR 0031) -- hence `fail`, not a
NOTES warning. tls.mode=certManager renews instead.
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

It is NOT renewed automatically. An expired certificate here stops the retention refresh, and a registry with an expiry policy then reclaims images your workloads are still running, one window later (ADR 0031).

Rotate it:
  kubectl -n %s delete secret %s
  helm upgrade ...            # mints a new CA and certificate
  kubectl -n %s rollout restart deploy    # zot and both controllers read certs once at startup

Every client has to learn the new CA, including the containerd drop-in on each node. To avoid doing this by hand, use registry.tls.mode=certManager.` $notAfter .Release.Namespace (include "kube-oci-composer.registryTLSSecretName" .) .Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
Non-empty when the controllers need the registry's CA mounted (TLS on and not already trusted).
Used only as a truthiness test.
*/}}
{{- define "kube-oci-composer.registryCAWanted" -}}
{{- if and .Values.registry.enabled .Values.registry.tls.enabled .Values.registry.tls.trust.enabled -}}
yes
{{- end -}}
{{- end -}}

{{- /*
The CA volume. cert-manager and supplied Secrets carry the CA in their own ca.crt, mounted directly so
it cannot go stale behind a renewal; only selfSigned uses the chart's ConfigMap.
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

{{- /* The CA mount path and the file --registry-ca-file names; both Deployments must agree. */}}
{{- define "kube-oci-composer.registryCADir" -}}/etc/oci-composer/registry-ca{{- end -}}
{{- define "kube-oci-composer.registryCAFile" -}}{{ include "kube-oci-composer.registryCADir" . }}/ca.crt{{- end -}}

{{- /*
Whether zot's config must be a Secret: zot's redis driver takes credentials only inside the URL, and
a ConfigMap is readable in every `kubectl describe`. Otherwise it stays an inspectable ConfigMap.
*/}}
{{- define "kube-oci-composer.registryConfigIsSecret" -}}
{{- if and (eq .Values.registry.cache.driver "redis") (contains "@" .Values.registry.cache.redis.url) -}}
true
{{- end -}}
{{- end -}}

{{- /*
Refusals about read replicas and storage. Each would render and then lose data or crashloop.
*/}}
{{- define "kube-oci-composer.checkRegistryReplicas" -}}
{{- $r := .Values.registry -}}

{{- /* Helm ignores unknown --set paths, so a leftover registry.cluster would silently mean one pod. */}}
{{- if $r.cluster -}}
{{- fail "registry.cluster no longer exists. It ran zot's scale-out mode, which SHARDS: each repository lived on exactly one member, so a member going down took ~1/N of the registry with it. The replacement is registry.readReplicas, which replicates reads instead -- see docs/adr/0041-one-writer-many-readers.md." -}}
{{- end -}}

{{- if and $r.enabled (lt (int $r.readReplicas) 0) -}}
{{- fail (printf "registry.readReplicas is %d. It counts replicas IN ADDITION to the single writer, so the smallest meaningful value is 0." (int $r.readReplicas)) -}}
{{- end -}}

{{- if and $r.enabled (gt (int $r.readReplicas) 0) -}}

{{- if eq $r.cache.driver "none" -}}
{{- fail "registry.readReplicas > 0 requires registry.cache.driver (redis or dynamodb). The default BoltDB metadata store is a file one process opens exclusively, and per-pod metadata would record a pull only on the pod that served it, so retention would delete content that is still in use (ADR 0031)." -}}
{{- end -}}

{{- if and $r.persistence.enabled (ne $r.persistence.accessMode "ReadWriteMany") -}}
{{- fail (printf "registry.readReplicas > 0 needs registry.persistence.accessMode=ReadWriteMany; it is %q. A second pod cannot mount a ReadWriteOnce volume and will sit in Multi-Attach error indefinitely. The chart can only check what you ASKED for, so confirm the claim with `kubectl get pvc` before relying on the replicas." $r.persistence.accessMode) -}}
{{- end -}}

{{- if and (eq $r.storage.driver "local") (not $r.persistence.enabled) -}}
{{- fail "registry.readReplicas > 0 with registry.storage.driver=local needs registry.persistence.enabled=true. An emptyDir is per-pod, so the replicas would be unrelated registries behind one Service name -- and every restart would lose an ImageBuild's only copy (ADR 0025)." -}}
{{- end -}}

{{- end -}}

{{- /* These apply with or without replicas. */}}
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
The registry flags both controllers take, so they cannot drift apart. Only supplyChain differs:

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

{{- /*
The zot container, shared by the writer and the read replicas; the role lives in the config.
Call with (dict "ctx" $ "resources" <resources>).
*/}}
{{- define "kube-oci-composer.registryContainer" -}}
{{- $r := .ctx.Values.registry -}}
- name: registry
  image: {{ $r.image | quote }}
  args: ["serve", "/etc/zot/config.json"]
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop: [ALL]
  ports:
    - name: registry
      containerPort: 5000
  volumeMounts:
    - {name: config, mountPath: /etc/zot}
    - {name: data, mountPath: /var/lib/registry}
    {{- if $r.auth.enabled }}
    - {name: auth, mountPath: /etc/zot/auth, readOnly: true}
    {{- end }}
    {{- if $r.tls.enabled }}
    - {name: tls, mountPath: /etc/zot/tls, readOnly: true}
    {{- end }}
  {{- with $r.storage.s3.existingSecret }}
  {{- /* S3 credentials as env from a Secret, never in the config file. */}}
  envFrom:
    - secretRef:
        name: {{ . }}
  {{- end }}
  {{- /*
  The scheme follows the listener. The kubelet's HTTPS prober skips verification (it probes the pod
  IP), so a self-signed cert is fine; not tcpSocket, which would pass while /v2/ answered 500.
  */}}
  {{- $scheme := ternary "HTTPS" "HTTP" $r.tls.enabled }}
  readinessProbe:
    httpGet: {path: /v2/, port: registry, scheme: {{ $scheme }}}
    initialDelaySeconds: 2
  livenessProbe:
    httpGet: {path: /v2/, port: registry, scheme: {{ $scheme }}}
    initialDelaySeconds: 10
  resources:
    {{- toYaml .resources | nindent 4 }}
{{- end -}}

{{- /* The zot pod volumes. Call with (dict "ctx" $ "config" <config object name>). */}}
{{- define "kube-oci-composer.registryVolumes" -}}
{{- $r := .ctx.Values.registry -}}
- name: config
  {{- if include "kube-oci-composer.registryConfigIsSecret" .ctx }}
  secret:
    secretName: {{ .config }}
  {{- else }}
  configMap:
    name: {{ .config }}
  {{- end }}
{{- if $r.tls.enabled }}
- name: tls
  secret:
    secretName: {{ include "kube-oci-composer.registryTLSSecretName" .ctx }}
{{- end }}
{{- if $r.auth.enabled }}
- name: auth
  secret:
    secretName: {{ $r.auth.existingHtpasswdSecret | default (printf "%s-htpasswd" (include "kube-oci-composer.registryFullname" .ctx)) }}
{{- end }}
- name: data
{{- if $r.persistence.enabled }}
  persistentVolumeClaim:
    claimName: {{ include "kube-oci-composer.registryFullname" .ctx }}
{{- else }}
  {{- /* Lost on every restart: for ImageBuild, content that cannot be rebuilt. */}}
  emptyDir: {}
{{- end }}
{{- end -}}
