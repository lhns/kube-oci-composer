{{- define "kube-oci-composer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-oci-composer.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kube-oci-composer.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kube-oci-composer.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "kube-oci-composer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-oci-composer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kube-oci-composer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kube-oci-composer.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Port of a ":8080" style bind address, so the container port and the flag cannot disagree.
*/}}
{{- define "kube-oci-composer.port" -}}
{{- regexReplaceAll "^.*:" . "" -}}
{{- end -}}

{{/*
Selector labels for ONE component; without the component every Service would select every pod in
the release. Part of the selector, so immutable on a live release.
Call with (dict "ctx" . "component" "composer").
*/}}
{{- define "kube-oci-composer.componentSelectorLabels" -}}
{{ include "kube-oci-composer.selectorLabels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- /*
Marks every pod that serves the registry API (writer and read replicas); the read Service, the
Ingress, the NetworkPolicy and the PDB select on it. A label rather than a component because
component=registry is the writer's immutable StatefulSet selector.
*/}}
{{- define "kube-oci-composer.registryServeRoleLabel" -}}
oci-composer.lhns.de/registry-role: serve
{{- end -}}

{{- define "kube-oci-composer.registryServeSelectorLabels" -}}
{{ include "kube-oci-composer.selectorLabels" . }}
{{ include "kube-oci-composer.registryServeRoleLabel" . }}
{{- end -}}

{{/*
Labels for a registry pod. One helper rather than two includes: both carry name and instance, and
duplicate YAML keys make Flux's post-renderer refuse the release.
*/}}
{{- define "kube-oci-composer.registryPodLabels" -}}
{{ include "kube-oci-composer.componentSelectorLabels" . }}
{{ include "kube-oci-composer.registryServeRoleLabel" . }}
{{- end -}}

{{- /*
The `from:` list admitting whole namespaces, shared by the registry and builder-context policies.
Call with (dict "allowed" <list> "namespace" .Release.Namespace) and indent the result.
*/}}
{{- define "kube-oci-composer.namespaceIngressFrom" -}}
{{- if .allowed -}}
{{- range .allowed }}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ . | quote }}
{{- end }}
{{- /*
The release namespace is always admitted: both controllers live there, and blocking the registry's
retention refresh deletes images one window later (ADR 0031).
*/}}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ .namespace | quote }}
{{- else }}
{{- /* Every namespace: a build can land anywhere. */}}
- namespaceSelector: {}
{{- end }}
{{- end -}}

{{- define "kube-oci-composer.componentLabels" -}}
{{ include "kube-oci-composer.labels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
The builder gets its own ServiceAccount and Role: its role can create Jobs, i.e. run arbitrary
containers, and must not be shared with the composer. ADR 0025, ADR 0056.
*/}}
{{- define "kube-oci-composer.builderFullname" -}}
{{- printf "%s-builder" (include "kube-oci-composer.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-oci-composer.builderServiceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kube-oci-composer.builderFullname" .) .Values.imageBuild.serviceAccountName -}}
{{- else -}}
{{- default "default" .Values.imageBuild.serviceAccountName -}}
{{- end -}}
{{- end -}}

{{/*
push.writeRefTo flags, identical on both controllers. Each renders independently: exporting into the
object's own namespace needs no refExport.namespaces.
*/}}
{{- define "kube-oci-composer.refExportArgs" -}}
{{- with .Values.refExport.namespaces }}
- --ref-export-namespaces={{ join "," . }}
{{- end }}
{{- with .Values.refExport.labels }}
- --ref-export-labels={{ . }}
{{- end }}
{{- with .Values.refExport.allowedLabels }}
- --ref-export-allowed-labels={{ join "," . }}
{{- end }}
{{- with .Values.refExport.allowedAnnotations }}
- --ref-export-allowed-annotations={{ join "," . }}
{{- end }}
{{- end -}}

{{- define "kube-oci-composer.registryFullname" -}}
{{- printf "%s-registry" (include "kube-oci-composer.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- /*
Where the CONTROLLERS reach the registry: the in-cluster Service, never registry.host. A cluster DNS
name is unresolvable from a kubelet and a node name often from cluster DNS, so the push address and
the pull address (publicRegistry) are kept separate.
*/}}
{{- define "kube-oci-composer.defaultRegistry" -}}
{{- if .Values.defaultRegistry.host -}}
{{- .Values.defaultRegistry.host -}}
{{- else if .Values.registry.enabled -}}
{{- printf "%s.%s.svc.%s:%d" (include "kube-oci-composer.registryFullname" .) .Release.Namespace .Values.registry.clusterDomain (int .Values.registry.service.port) -}}
{{- end -}}
{{- end -}}

{{- /*
registry.host without its port. Ingress rule hosts and certificate SANs are hostnames; with a port
the Ingress silently matches nothing.
*/}}
{{- define "kube-oci-composer.publicHostname" -}}
{{- $h := .Values.registry.host | default "" -}}
{{- if contains ":" $h -}}
{{- (splitList ":" $h) | first -}}
{{- else -}}
{{- $h -}}
{{- end -}}
{{- end -}}

{{- /*
What WORKLOADS pull from (status.artifact.ref and .tags). Nothing that dials the registry may use
it. Empty for an external registry, whose one name already works from both places.
*/}}
{{- define "kube-oci-composer.publicRegistry" -}}
{{- if and .Values.registry.enabled .Values.registry.host -}}
{{- .Values.registry.host -}}
{{- end -}}
{{- end -}}

{{/*
The Secret holding the push credential both controllers read from their own namespace.
*/}}
{{- define "kube-oci-composer.pushSecretName" -}}
{{- if .Values.defaultRegistry.existingPushSecret -}}
{{- .Values.defaultRegistry.existingPushSecret -}}
{{- else if .Values.registry.enabled -}}
{{- printf "%s-push" (include "kube-oci-composer.registryFullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
The generated registry password, reused from the cluster so an upgrade does not lock the
controllers out. `lookup` is empty under `helm template` and `--dry-run`, so those render a fresh
random one that is never written.
*/}}
{{- define "kube-oci-composer.registryPassword" -}}
{{- if .Values.registry.auth.password -}}
{{- .Values.registry.auth.password -}}
{{- else -}}
{{- $name := printf "%s-push" (include "kube-oci-composer.registryFullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $name -}}
{{- if and $existing $existing.data (index $existing.data "password") -}}
{{- index $existing.data "password" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Hosts the controllers (and BuildKit, as registry.insecure=true) may reach over plain HTTP. Matched
on host, so listing one downgrades nothing else.
*/}}
{{- define "kube-oci-composer.insecureRegistries" -}}
{{- $hosts := list -}}
{{- with .Values.operator.insecureRegistry }}{{- $hosts = concat $hosts (splitList "," .) -}}{{- end -}}
{{- /*
The bundled registry's Service speaks plain HTTP until registry.tls is on; it must leave this list
then, or builds keep pushing credentials in the clear. registry.host is never added: it may be a
TLS-terminating ingress. A plain-HTTP public host opts in via defaultRegistry.insecure.
*/}}
{{- if and .Values.registry.enabled (not .Values.registry.tls.enabled) (not .Values.defaultRegistry.host) -}}
{{- $hosts = append $hosts (include "kube-oci-composer.defaultRegistry" .) -}}
{{- end -}}
{{- with .Values.defaultRegistry.insecure }}{{- $hosts = concat $hosts (splitList "," .) -}}{{- end -}}
{{- join "," (compact $hosts) -}}
{{- end -}}

{{- /*
The image running `oci-builder fetch-context` as each build's init container. Defaults to the
builder's own image, which contains the fetcher subcommand.
*/}}
{{- define "kube-oci-composer.fetcherImage" -}}
{{- if .Values.imageBuild.fetcherImage -}}
{{- .Values.imageBuild.fetcherImage -}}
{{- else -}}
{{- printf "%s:%s" .Values.imageBuild.image.repository (.Values.imageBuild.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}
