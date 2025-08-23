{{/*
Expand the name of the chart.
*/}}
{{- define "virtrigaud.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "virtrigaud.fullname" -}}
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
{{- define "virtrigaud.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "virtrigaud.labels" -}}
helm.sh/chart: {{ include "virtrigaud.chart" . }}
{{ include "virtrigaud.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "virtrigaud.selectorLabels" -}}
app.kubernetes.io/name: {{ include "virtrigaud.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "virtrigaud.serviceAccountName" -}}
{{- if .Values.manager.serviceAccount.create }}
{{- default (include "virtrigaud.fullname" .) .Values.manager.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.manager.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the ClusterRole to use
*/}}
{{- define "virtrigaud.clusterRoleName" -}}
{{- if eq .Values.rbac.scope "cluster" }}
{{- include "virtrigaud.fullname" . }}-manager
{{- else }}
{{- include "virtrigaud.fullname" . }}-manager-{{ .Release.Namespace }}
{{- end }}
{{- end }}

{{/*
Create the name of the ClusterRoleBinding to use
*/}}
{{- define "virtrigaud.clusterRoleBindingName" -}}
{{- if eq .Values.rbac.scope "cluster" }}
{{- include "virtrigaud.fullname" . }}-manager
{{- else }}
{{- include "virtrigaud.fullname" . }}-manager-{{ .Release.Namespace }}
{{- end }}
{{- end }}

{{/*
Pod security labels for Pod Security Standards
*/}}
{{- define "virtrigaud.podSecurityLabels" -}}
{{- if eq .Values.security.podSecurityProfile "strict" }}
pod-security.kubernetes.io/enforce: strict
pod-security.kubernetes.io/audit: strict
pod-security.kubernetes.io/warn: strict
{{- else if eq .Values.security.podSecurityProfile "baseline" }}
pod-security.kubernetes.io/enforce: baseline
pod-security.kubernetes.io/audit: baseline
pod-security.kubernetes.io/warn: baseline
{{- else if eq .Values.security.podSecurityProfile "privileged" }}
pod-security.kubernetes.io/enforce: privileged
pod-security.kubernetes.io/audit: privileged
pod-security.kubernetes.io/warn: privileged
{{- end }}
{{- end }}

{{/*
Generate certificates for webhook
*/}}
{{- define "virtrigaud.webhookCerts" -}}
{{- if eq .Values.webhooks.certificates.source "self-signed" }}
{{- $ca := genCA "virtrigaud-ca" 3650 }}
{{- $cert := genSignedCert (include "virtrigaud.fullname" .) nil (list (printf "%s-webhook.%s.svc" (include "virtrigaud.fullname" .) .Release.Namespace) (printf "%s-webhook.%s.svc.cluster.local" (include "virtrigaud.fullname" .) .Release.Namespace)) 3650 $ca }}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
ca.crt: {{ $ca.Cert | b64enc }}
{{- end }}
{{- end }}

{{/*
Generate CA certificate for webhook validation
*/}}
{{- define "virtrigaud.webhookCaCert" -}}
{{- if eq .Values.webhooks.certificates.source "self-signed" }}
{{- $ca := genCA "virtrigaud-ca" 3650 }}
{{- $ca.Cert | b64enc }}
{{- end }}
{{- end }}

{{/*
Generate certificates for provider gRPC
*/}}
{{- define "virtrigaud.providerCerts" -}}
{{- if eq .Values.security.tls.source "self-signed" }}
{{- $ca := genCA "virtrigaud-provider-ca" 3650 }}
{{- $cert := genSignedCert "virtrigaud-provider" nil (list "localhost" "127.0.0.1") 3650 $ca }}
tls.crt: {{ $cert.Cert | b64enc }}
tls.key: {{ $cert.Key | b64enc }}
ca.crt: {{ $ca.Cert | b64enc }}
{{- end }}
{{- end }}

{{/*
Generate network policy peer selectors
*/}}
{{- define "virtrigaud.networkPolicyPeerSelectors" -}}
- podSelector:
    matchLabels:
      {{- include "virtrigaud.selectorLabels" . | nindent 6 }}
{{- end }}

{{/*
Manager service name
*/}}
{{- define "virtrigaud.managerServiceName" -}}
{{- include "virtrigaud.fullname" . }}-manager
{{- end }}

{{/*
Webhook service name
*/}}
{{- define "virtrigaud.webhookServiceName" -}}
{{- include "virtrigaud.fullname" . }}-webhook
{{- end }}

{{/*
Provider service name
*/}}
{{- define "virtrigaud.providerServiceName" -}}
{{- printf "%s-provider-%s" (include "virtrigaud.fullname" .) .provider }}
{{- end }}

{{/*
Prometheus ServiceMonitor labels
*/}}
{{- define "virtrigaud.serviceMonitorLabels" -}}
{{- with .Values.observability.prometheus.serviceMonitor.labels }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Prometheus PrometheusRule labels
*/}}
{{- define "virtrigaud.prometheusRuleLabels" -}}
{{- with .Values.observability.prometheus.prometheusRule.labels }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Grafana dashboard labels
*/}}
{{- define "virtrigaud.grafanaDashboardLabels" -}}
{{- with .Values.observability.grafana.dashboards.labels }}
{{- toYaml . }}
{{- end }}
{{- end }}
