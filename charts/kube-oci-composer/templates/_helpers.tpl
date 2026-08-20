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
Port extracted from a ":8080" style bind address, so the container port and the flag can never
disagree.
*/}}
{{- define "kube-oci-composer.port" -}}
{{- regexReplaceAll "^.*:" . "" -}}
{{- end -}}

{{/*
Selector labels for ONE component of this chart.

Three workloads now share a release -- the composer, the builder and the registry -- and
`selectorLabels` alone does not tell them apart. Without this, every Service in the chart selects
every pod in the release: the registry pod was backing the composer's Service purely because it
carried the same two labels.

Callers pass (dict "ctx" . "component" "composer"). The component name is part of the SELECTOR, so
it cannot be changed on a live release without recreating the Deployment -- selectors are immutable.
*/}}
{{- define "kube-oci-composer.componentSelectorLabels" -}}
{{ include "kube-oci-composer.selectorLabels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "kube-oci-composer.componentLabels" -}}
{{ include "kube-oci-composer.labels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
The builder's names. A separate ServiceAccount and a separate Role, in the same namespace: merging
the charts must not merge the PERMISSIONS. The builder's role can create Jobs -- that is, run
arbitrary containers -- and the composer's cannot create a single object. See ADR 0025.
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
The registry's names, and the host the controllers push to.

defaultRegistry.host wins when set -- that is the bring-your-own case. Otherwise it is the bundled
zot's in-cluster Service DNS, which is what the CONTROLLERS use. Workloads pulling need a
node-resolvable name instead (registry.host); containerd resolves image references with the node's
resolver, which cannot see cluster DNS.
*/}}
{{- /*
ONE host string, used for both the push and the reference recorded in status.
It has to be one string: the controller writes to it and a workload pulls the same reference back.
registry.host wins when set, so status.artifact.ref names something a node can resolve; unset, it
falls back to the Service DNS, which works for the controllers and NOT for pulls -- which is why the
chart warns about it at install time.
*/}}
{{- define "kube-oci-composer.registryFullname" -}}
{{- printf "%s-registry" (include "kube-oci-composer.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-oci-composer.defaultRegistry" -}}
{{- if .Values.defaultRegistry.host -}}
{{- .Values.defaultRegistry.host -}}
{{- else if and .Values.registry.enabled .Values.registry.host -}}
{{- .Values.registry.host -}}
{{- else if .Values.registry.enabled -}}
{{- printf "%s.%s.svc.cluster.local:%d" (include "kube-oci-composer.registryFullname" .) .Release.Namespace (int .Values.registry.service.port) -}}
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
The generated registry password, stable across upgrades.

Reuses the value already in the cluster when there is one, so `helm upgrade` does not roll a new
password and lock the controllers out of every image they have published. `lookup` returns nothing
during `helm template` and `--dry-run`, which is why the generated branch has to be deterministic
enough to render -- it is only ever WRITTEN on a real install.
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
Hosts the controllers may reach over plain HTTP.

The bundled registry is added automatically. It serves HTTP inside the cluster -- there is no
certificate for a .svc.cluster.local name and terminating TLS for one would mean managing a CA -- so
without this every push to it fails in the TLS handshake, on a default install, with an error that
points at the registry rather than at the missing flag.

Matched on host, so naming it does not downgrade any other registry the same controller talks to.
*/}}
{{- define "kube-oci-composer.insecureRegistries" -}}
{{- $hosts := list -}}
{{- with .Values.operator.insecureRegistry }}{{- $hosts = concat $hosts (splitList "," .) -}}{{- end -}}
{{- /*
ONLY the in-cluster Service name is added automatically. That one is always plain HTTP -- there is
no certificate for a .svc.cluster.local name and no way to get one.
registry.host is deliberately NOT added: it may well be an ingress terminating TLS, and marking it
insecure would force plain HTTP and break the very deployment that took the trouble to set it up.
A plain-HTTP registry.host (a NodePort, say) opts in through defaultRegistry.insecure.
*/}}
{{- if and .Values.registry.enabled (not .Values.defaultRegistry.host) (not .Values.registry.host) -}}
{{- $hosts = append $hosts (include "kube-oci-composer.defaultRegistry" .) -}}
{{- end -}}
{{- with .Values.defaultRegistry.insecure }}{{- $hosts = concat $hosts (splitList "," .) -}}{{- end -}}
{{- join "," (compact $hosts) -}}
{{- end -}}
