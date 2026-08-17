{{- define "kube-oci-builder.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kube-oci-builder.fullname" -}}
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

{{- define "kube-oci-builder.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kube-oci-builder.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kube-oci-builder.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kube-oci-builder.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kube-oci-builder.serviceAccountName" -}}
{{- default (include "kube-oci-builder.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/*
The service account the BUILD pods run as. Deliberately distinct from the controller's: a pod
running code from a git repository must not carry the token of the thing that created it.
*/}}
{{- define "kube-oci-builder.buildServiceAccountName" -}}
{{- printf "%s-build" (include "kube-oci-builder.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
