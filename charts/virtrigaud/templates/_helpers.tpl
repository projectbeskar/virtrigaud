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
Name of the dedicated, least-privilege ServiceAccount for a chart-templated
provider Deployment. Each provider gets its OWN token-less identity with NO
RoleBinding/ClusterRoleBinding — it never borrows the manager's SA (which holds
cluster-wide secrets access). Call with a dict:
  {{ include "virtrigaud.providerServiceAccountName" (dict "root" . "provider" "libvirt") }}
*/}}
{{- define "virtrigaud.providerServiceAccountName" -}}
{{- printf "%s-provider-%s" (include "virtrigaud.fullname" .root) .provider -}}
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
Provider mTLS env vars (ADR-0003 v0.3.7 PR-3).

Renders the VIRTRIGAUD_PROVIDER_* env entries for a chart-templated
provider Deployment based on .Values.providerTLS. Emits nothing unless
either a Secret is referenced or the insecure escape hatch is set, so the
caller can include it unconditionally without producing an empty `env:`.

  - secretName set        → VIRTRIGAUD_PROVIDER_ALLOWED_SANS (if any).
                            TLS is mandatory by virtue of the mounted
                            cert files; no insecure var is rendered.
  - secretName empty +
    insecure=true         → VIRTRIGAUD_PROVIDER_INSECURE=true.
  - secretName empty +
    insecure=false        → nothing (provider hard-exits — secure default).

Include with the env-item indentation of the call site, e.g.
  {{- include "virtrigaud.providerTLSEnv" . | nindent 8 }}
*/}}
{{- define "virtrigaud.providerTLSEnv" -}}
{{- if .Values.providerTLS.secretName }}
{{- with .Values.providerTLS.allowedSANs }}
- name: VIRTRIGAUD_PROVIDER_ALLOWED_SANS
  value: {{ join "," . | quote }}
{{- end }}
{{- else if .Values.providerTLS.insecure }}
- name: VIRTRIGAUD_PROVIDER_INSECURE
  value: "true"
{{- end }}
{{- end }}

{{/*
Provider mTLS volumeMount (ADR-0003 v0.3.7 PR-3).

Renders the read-only mount of the TLS Secret at /etc/virtrigaud/tls
(the canonical path the SDK's ResolveTLSAndAuth reads). Emits nothing
when no Secret is referenced.
*/}}
{{- define "virtrigaud.providerTLSVolumeMount" -}}
{{- if .Values.providerTLS.secretName }}
- name: provider-tls
  mountPath: /etc/virtrigaud/tls
  readOnly: true
{{- end }}
{{- end }}

{{/*
Provider mTLS volume (ADR-0003 v0.3.7 PR-3).

Renders the Secret volume sourcing the externally-provisioned TLS
material. Emits nothing when no Secret is referenced.
*/}}
{{- define "virtrigaud.providerTLSVolume" -}}
{{- if .Values.providerTLS.secretName }}
- name: provider-tls
  secret:
    secretName: {{ .Values.providerTLS.secretName }}
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
DNS egress rule shared by the manager and provider NetworkPolicies (single
source of truth so the two copies can't diverge). Allows UDP+TCP 53 to
kube-dns/CoreDNS via the configured namespace+pod selectors, plus any
dnsEgressCIDRs as ipBlock peers (for NodeLocal DNSCache, whose node-local
link-local resolver IP a selector cannot match). Call with the root context and
the egress-list indentation of the call site, e.g.
  {{ include "virtrigaud.dnsEgressRule" . | nindent 4 }}
*/}}
{{- define "virtrigaud.dnsEgressRule" -}}
{{- $np := .Values.networkPolicy -}}
- to:
    - namespaceSelector:
        {{- toYaml $np.dnsNamespaceSelector | nindent 8 }}
      podSelector:
        {{- toYaml $np.dnsPodSelector | nindent 8 }}
    {{- range $np.dnsEgressCIDRs }}
    - ipBlock:
        cidr: {{ . }}
    {{- end }}
  ports:
    - protocol: UDP
      port: 53
    - protocol: TCP
      port: 53
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

{{/*
Validates manager.maxDiskSize (the manager's --max-disk-size) and returns it as
a string: "" (unset, as in an older values file under --reuse-values) or a
quantity with a unit of at least G (such as 500Gi or 2Ti). A missing unit
("500" is 500 bytes), a smaller unit, zero or "none" fails the render: the
manager would refuse to start with it. So does a value below 1Gi (e.g. "1G"
or "0.5Gi"), which the manager refuses too. Takes the root context.
*/}}
{{- define "virtrigaud.maxDiskSizeValue" -}}
{{- $v := "" -}}
{{- if and .Values.manager (not (kindIs "invalid" .Values.manager.maxDiskSize)) -}}
{{- $v = toString .Values.manager.maxDiskSize -}}
{{- end -}}
{{- if ne $v "" -}}
{{- if or (not (regexMatch "^[0-9]+(\\.[0-9]+)?(Gi|Ti|Pi|Ei|G|T|P|E)$" $v)) (regexMatch "^0+(\\.0+)?[A-Za-z]*$" $v) -}}
{{- fail (printf "manager.maxDiskSize: invalid disk-size limit %q; use a quantity of at least 1Gi such as 500Gi or 2Ti, or leave it empty for no limit" $v) -}}
{{- end -}}
{{- /* Below 1Gi is possible only with a G or Gi unit: refuse it here, as the
       manager does (it would otherwise crash-loop, and with failurePolicy
       Fail block VM and VMClass writes). 1Gi is 1.073741824G. */ -}}
{{- $num := regexFind "^[0-9]+(\\.[0-9]+)?" $v -}}
{{- $unit := trimPrefix $num $v -}}
{{- if or (and (eq $unit "Gi") (lt (float64 $num) 1.0)) (and (eq $unit "G") (lt (float64 $num) 1.073741824)) -}}
{{- fail (printf "manager.maxDiskSize: invalid disk-size limit %q; it must be at least 1Gi (1Gi is about 1.074G)" $v) -}}
{{- end -}}
{{- /* At most 64Pi (the manager refuses more): 64Pi in each unit. */ -}}
{{- $max := dict "Gi" 67108864.0 "G" 72057594.037927936 "Ti" 65536.0 "T" 72057.594037927936 "Pi" 64.0 "P" 72.057594037927936 "Ei" 0.0625 "E" 0.072057594037927936 -}}
{{- if gt (float64 $num) (get $max $unit) -}}
{{- fail (printf "manager.maxDiskSize: invalid disk-size limit %q; it must be at most 64Pi" $v) -}}
{{- end -}}
{{- /* A whole number of bytes, as the manager requires ("1.1Gi" is not). */ -}}
{{- $unitBytes := dict "Gi" 1073741824.0 "G" 1000000000.0 "Ti" 1099511627776.0 "T" 1000000000000.0 "Pi" 1125899906842624.0 "P" 1000000000000000.0 "Ei" 1152921504606846976.0 "E" 1000000000000000000.0 -}}
{{- $bytes := mulf (float64 $num) (get $unitBytes $unit) -}}
{{- if ne $bytes (floor $bytes) -}}
{{- fail (printf "manager.maxDiskSize: invalid disk-size limit %q; it must be a whole number of bytes (1.1Gi is not; 1.5Gi is)" $v) -}}
{{- end -}}
{{- end -}}
{{- $v -}}
{{- end }}

{{/*
Validates a /tmp sizeLimit setting and returns it as a string: "" (unset, as in
an older values file under --reuse-values), "none", or a positive resource
quantity such as 20Gi. Anything else fails the render, so a typo never reaches
the API server (or the manager, which would refuse to start). Takes a dict with
"value" and "name" (the values key, for the error message).
*/}}
{{- define "virtrigaud.tmpSizeLimitValue" -}}
{{- $v := "" -}}
{{- if not (kindIs "invalid" .value) -}}
{{- $v = toString .value -}}
{{- end -}}
{{- if and (ne $v "") (ne $v "none") -}}
{{- if or (not (regexMatch "^[0-9]+(\\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$" $v)) (regexMatch "^0+(\\.0+)?[A-Za-z]*$" $v) -}}
{{- fail (printf "%s: invalid /tmp sizeLimit %q; use a positive quantity such as 20Gi, \"none\", or leave it empty" .name $v) -}}
{{- end -}}
{{- end -}}
{{- $v -}}
{{- end }}

{{/*
The emptyDir source of a writable /tmp volume, in flow style: {} when the
sizeLimit is empty, unset or "none", else {sizeLimit: "<limit>"}. Takes a dict
with "value" and "name" (see virtrigaud.tmpSizeLimitValue).
*/}}
{{- define "virtrigaud.tmpEmptyDir" -}}
{{- $v := include "virtrigaud.tmpSizeLimitValue" . -}}
{{- if and $v (ne $v "none") -}}
{sizeLimit: {{ $v | quote }}}
{{- else -}}
{}
{{- end -}}
{{- end }}
