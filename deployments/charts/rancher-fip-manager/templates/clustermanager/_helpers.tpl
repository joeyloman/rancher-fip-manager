{{/*
Expand the name of the chart.
*/}}
{{- define "rancher-fip-cluster-manager.name" -}}
{{- default .Chart.Name .Values.clustermanager.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "rancher-fip-cluster-manager.fullname" -}}
{{- if .Values.clustermanager.fullnameOverride }}
{{- .Values.clustermanager.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.clustermanager.nameOverride }}
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
{{- define "rancher-fip-cluster-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "rancher-fip-cluster-manager.labels" -}}
helm.sh/chart: {{ include "rancher-fip-cluster-manager.chart" . }}
{{ include "rancher-fip-cluster-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "rancher-fip-cluster-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "rancher-fip-cluster-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "rancher-fip-cluster-manager.serviceAccountName" -}}
{{- if .Values.clustermanager.serviceAccount.create }}
{{- default (include "rancher-fip-cluster-manager.fullname" .) .Values.clustermanager.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.clustermanager.serviceAccount.name }}
{{- end }}
{{- end }}
