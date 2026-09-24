{{/*
Expand the name of the chart.
*/}}
{{- define "rancher-fip-manager-webhook.name" -}}
{{- default "rancher-fip-manager-webhook" .Values.webhook.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "rancher-fip-manager-webhook.fullname" -}}
{{- if .Values.webhook.fullnameOverride }}
{{- .Values.webhook.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default "webhook" .Values.webhook.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "rancher-fip-manager-webhook.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "rancher-fip-manager-webhook.labels" -}}
helm.sh/chart: {{ include "rancher-fip-manager-webhook.chart" . }}
{{ include "rancher-fip-manager-webhook.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "rancher-fip-manager-webhook.selectorLabels" -}}
app.kubernetes.io/name: {{ include "rancher-fip-manager-webhook.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "rancher-fip-manager-webhook.serviceAccountName" -}}
{{- if .Values.webhook.serviceAccount.create }}
{{- default (include "rancher-fip-manager-webhook.fullname" .) .Values.webhook.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.webhook.serviceAccount.name }}
{{- end }}
{{- end }}
