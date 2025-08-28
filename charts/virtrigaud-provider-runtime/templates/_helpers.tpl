{{/*
Expand the name of the chart.
*/}}
{{- define "virtrigaud-provider-runtime.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "virtrigaud-provider-runtime.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
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
{{- define "virtrigaud-provider-runtime.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "virtrigaud-provider-runtime.labels" -}}
helm.sh/chart: {{ include "virtrigaud-provider-runtime.chart" . }}
{{ include "virtrigaud-provider-runtime.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "virtrigaud-provider-runtime.selectorLabels" -}}
app.kubernetes.io/name: {{ include "virtrigaud-provider-runtime.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "virtrigaud-provider-runtime.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "virtrigaud-provider-runtime.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Generate environment variables
*/}}
{{- define "virtrigaud-provider-runtime.env" -}}
{{- range .Values.env }}
- name: {{ .name }}
  {{- if .value }}
  value: {{ .value | quote }}
  {{- else if .valueFrom }}
  valueFrom:
    {{- toYaml .valueFrom | nindent 4 }}
  {{- end }}
{{- end }}
{{- end }}

{{/*
Generate volume mounts for TLS
*/}}
{{- define "virtrigaud-provider-runtime.tlsVolumeMounts" -}}
{{- if .Values.tls.enabled }}
- name: tls-certs
  mountPath: /etc/tls
  readOnly: true
{{- end }}
{{- end }}

{{/*
Generate volumes for TLS
*/}}
{{- define "virtrigaud-provider-runtime.tlsVolumes" -}}
{{- if .Values.tls.enabled }}
- name: tls-certs
  secret:
    secretName: {{ .Values.tls.secretName }}
{{- end }}
{{- end }}

{{/*
Generate volume mounts for credentials
*/}}
{{- define "virtrigaud-provider-runtime.credentialsVolumeMounts" -}}
{{- if .Values.credentials.secretName }}
- name: credentials
  mountPath: /etc/credentials
  readOnly: true
{{- end }}
{{- end }}

{{/*
Generate volumes for credentials
*/}}
{{- define "virtrigaud-provider-runtime.credentialsVolumes" -}}
{{- if .Values.credentials.secretName }}
- name: credentials
  secret:
    secretName: {{ .Values.credentials.secretName }}
{{- end }}
{{- end }}
