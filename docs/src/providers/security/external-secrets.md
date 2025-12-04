# External Secrets Management

This guide covers integrating VirtRigaud providers with external secret management systems using ExternalSecrets operators and best practices for credential security.

## Overview

External secret management provides secure, centralized credential storage and automatic secret rotation. Supported systems include:

- **HashiCorp Vault**: Enterprise secret management with dynamic secrets
- **AWS Secrets Manager**: Cloud-native secret storage with automatic rotation
- **Azure Key Vault**: Azure-integrated secret management
- **Google Secret Manager**: GCP secret storage service
- **Kubernetes External Secrets**: Generic external secret integration

## External Secrets Operator Setup

### Installation

```bash
# Install External Secrets Operator
helm repo add external-secrets https://charts.external-secrets.io
helm repo update

helm install external-secrets external-secrets/external-secrets \
  --namespace external-secrets-system \
  --create-namespace \
  --set installCRDs=true
```

### Basic Configuration

```yaml
# ServiceAccount for External Secrets Operator
apiVersion: v1
kind: ServiceAccount
metadata:
  name: external-secrets
  namespace: virtrigaud-system
  annotations:
    # For AWS IRSA (IAM Roles for Service Accounts)
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/external-secrets-role

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: external-secrets
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create", "update", "patch", "delete", "get", "list", "watch"]
  - apiGroups: ["external-secrets.io"]
    resources: ["*"]
    verbs: ["*"]

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: external-secrets
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: external-secrets
subjects:
  - kind: ServiceAccount
    name: external-secrets
    namespace: virtrigaud-system
```

## HashiCorp Vault Integration

### Vault SecretStore

```yaml
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: vault-secret-store
  namespace: virtrigaud-system
spec:
  provider:
    vault:
      server: "https://vault.example.com:8200"
      path: "secret"
      version: "v2"
      auth:
        # Use Kubernetes service account for authentication
        kubernetes:
          mountPath: "kubernetes"
          role: "virtrigaud-role"
          serviceAccountRef:
            name: "external-secrets"

---
# For multi-namespace access
apiVersion: external-secrets.io/v1beta1
kind: ClusterSecretStore
metadata:
  name: vault-cluster-store
spec:
  provider:
    vault:
      server: "https://vault.example.com:8200"
      path: "secret"
      version: "v2"
      auth:
        kubernetes:
          mountPath: "kubernetes"
          role: "virtrigaud-cluster-role"
          serviceAccountRef:
            name: "external-secrets"
            namespace: "virtrigaud-system"
```

### Vault Policy Configuration

```hcl
# Vault policy for VirtRigaud secrets
path "secret/data/virtrigaud/*" {
  capabilities = ["read"]
}

path "secret/data/providers/*" {
  capabilities = ["read"]
}

# Dynamic database credentials
path "database/creds/readonly" {
  capabilities = ["read"]
}

# PKI for TLS certificates
path "pki/issue/virtrigaud" {
  capabilities = ["create", "update"]
}
```

### vSphere Credentials from Vault

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: vsphere-credentials
  namespace: vsphere-providers
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-secret-store
    kind: SecretStore
  target:
    name: vsphere-credentials
    creationPolicy: Owner
    template:
      type: Opaque
      data:
        username: "{{ .username }}"
        password: "{{ .password }}"
        server: "{{ .server }}"
        # Optional: TLS certificate
        ca.crt: "{{ .ca_cert | b64dec }}"
  data:
    - secretKey: username
      remoteRef:
        key: secret/data/providers/vsphere
        property: username
    - secretKey: password
      remoteRef:
        key: secret/data/providers/vsphere
        property: password
    - secretKey: server
      remoteRef:
        key: secret/data/providers/vsphere
        property: server
    - secretKey: ca_cert
      remoteRef:
        key: secret/data/providers/vsphere
        property: ca_cert
```

## AWS Secrets Manager Integration

### AWS SecretStore with IRSA

```yaml
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: aws-secrets-manager
  namespace: virtrigaud-system
spec:
  provider:
    aws:
      service: SecretsManager
      region: us-west-2
      auth:
        # Use IAM Roles for Service Accounts (IRSA)
        serviceAccount:
          name: external-secrets
          namespace: virtrigaud-system

---
# IAM Policy for the IRSA role
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "secretsmanager:GetSecretValue",
        "secretsmanager:DescribeSecret"
      ],
      "Resource": [
        "arn:aws:secretsmanager:us-west-2:ACCOUNT:secret:virtrigaud/*"
      ]
    }
  ]
}
```

### AWS Secret Configuration

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: aws-provider-credentials
  namespace: provider-namespace
spec:
  refreshInterval: 15m
  secretStoreRef:
    name: aws-secrets-manager
    kind: SecretStore
  target:
    name: provider-credentials
    creationPolicy: Owner
  data:
    - secretKey: credentials.json
      remoteRef:
        key: "virtrigaud/provider-credentials"
        property: "credentials"
    - secretKey: api-key
      remoteRef:
        key: "virtrigaud/api-keys"
        property: "provider-api-key"
```

## Azure Key Vault Integration

### Azure SecretStore

```yaml
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: azure-key-vault
  namespace: virtrigaud-system
spec:
  provider:
    azurekv:
      vaultUrl: "https://virtrigaud-vault.vault.azure.net/"
      authType: "ManagedIdentity"
      # Or use Service Principal:
      # authType: "ServicePrincipal"
      # authSecretRef:
      #   clientId:
      #     name: azure-secret
      #     key: client-id
      #   clientSecret:
      #     name: azure-secret
      #     key: client-secret
      tenantId: "tenant-id-here"

---
# Managed Identity setup (ARM template or Terraform)
apiVersion: v1
kind: Secret
metadata:
  name: azure-config
  namespace: virtrigaud-system
type: Opaque
data:
  # Base64 encoded values
  tenant-id: dGVuYW50LWlkLWhlcmU=
  client-id: Y2xpZW50LWlkLWhlcmU=
  client-secret: Y2xpZW50LXNlY3JldC1oZXJl
```

### Azure Key Vault Secret

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: azure-provider-secrets
  namespace: provider-namespace
spec:
  refreshInterval: 30m
  secretStoreRef:
    name: azure-key-vault
    kind: SecretStore
  target:
    name: provider-secrets
    creationPolicy: Owner
  data:
    - secretKey: subscription-id
      remoteRef:
        key: "azure-subscription-id"
    - secretKey: resource-group
      remoteRef:
        key: "azure-resource-group"
    - secretKey: client-certificate
      remoteRef:
        key: "azure-client-cert"
```

## Google Secret Manager Integration

### GCP SecretStore

```yaml
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata:
  name: gcp-secret-manager
  namespace: virtrigaud-system
spec:
  provider:
    gcpsm:
      projectId: "your-gcp-project"
      auth:
        # Use Workload Identity
        workloadIdentity:
          clusterLocation: us-central1
          clusterName: virtrigaud-cluster
          serviceAccountRef:
            name: external-secrets
            namespace: virtrigaud-system

---
# Workload Identity binding
apiVersion: v1
kind: ServiceAccount
metadata:
  name: external-secrets
  namespace: virtrigaud-system
  annotations:
    iam.gke.io/gcp-service-account: virtrigaud-secrets@PROJECT.iam.gserviceaccount.com
```

### GCP Secret Configuration

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: gcp-provider-secrets
  namespace: provider-namespace
spec:
  refreshInterval: 20m
  secretStoreRef:
    name: gcp-secret-manager
    kind: SecretStore
  target:
    name: gcp-provider-credentials
    creationPolicy: Owner
  data:
    - secretKey: service-account.json
      remoteRef:
        key: "virtrigaud-service-account"
        version: "latest"
    - secretKey: project-id
      remoteRef:
        key: "gcp-project-id"
        version: "latest"
```

## Provider-Specific Configurations

### vSphere Provider with Dynamic Credentials

```yaml
# Vault configuration for vSphere dynamic credentials
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: vsphere-dynamic-credentials
  namespace: vsphere-providers
spec:
  refreshInterval: 15m  # Short refresh for dynamic credentials
  secretStoreRef:
    name: vault-secret-store
    kind: SecretStore
  target:
    name: vsphere-dynamic-creds
    creationPolicy: Owner
    template:
      type: Opaque
      data:
        username: "{{ .username }}"
        password: "{{ .password }}"
        server: "{{ .server }}"
        session_ttl: "{{ .lease_duration }}"
  data:
    - secretKey: username
      remoteRef:
        key: "vsphere/creds/dynamic-role"
        property: "username"
    - secretKey: password
      remoteRef:
        key: "vsphere/creds/dynamic-role"
        property: "password"
    - secretKey: server
      remoteRef:
        key: "secret/data/vsphere/static"
        property: "server"
    - secretKey: lease_duration
      remoteRef:
        key: "vsphere/creds/dynamic-role"
        property: "lease_duration"

---
# Provider deployment using dynamic credentials
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vsphere-provider
  namespace: vsphere-providers
spec:
  template:
    spec:
      containers:
        - name: provider
          env:
            - name: VSPHERE_USERNAME
              valueFrom:
                secretKeyRef:
                  name: vsphere-dynamic-creds
                  key: username
            - name: VSPHERE_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: vsphere-dynamic-creds
                  key: password
            - name: VSPHERE_SERVER
              valueFrom:
                secretKeyRef:
                  name: vsphere-dynamic-creds
                  key: server
```

### Libvirt Provider with SSH Keys

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: libvirt-ssh-keys
  namespace: libvirt-providers
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-secret-store
    kind: SecretStore
  target:
    name: libvirt-ssh-credentials
    creationPolicy: Owner
    template:
      type: kubernetes.io/ssh-auth
      data:
        ssh-privatekey: "{{ .private_key }}"
        ssh-publickey: "{{ .public_key }}"
        known_hosts: "{{ .known_hosts }}"
  data:
    - secretKey: private_key
      remoteRef:
        key: "secret/data/libvirt/ssh"
        property: "private_key"
    - secretKey: public_key
      remoteRef:
        key: "secret/data/libvirt/ssh"
        property: "public_key"
    - secretKey: known_hosts
      remoteRef:
        key: "secret/data/libvirt/ssh"
        property: "known_hosts"

---
# Mount SSH keys in provider
apiVersion: apps/v1
kind: Deployment
metadata:
  name: libvirt-provider
spec:
  template:
    spec:
      containers:
        - name: provider
          volumeMounts:
            - name: ssh-keys
              mountPath: /home/provider/.ssh
              readOnly: true
          env:
            - name: SSH_AUTH_SOCK
              value: "/tmp/ssh-agent.sock"
      volumes:
        - name: ssh-keys
          secret:
            secretName: libvirt-ssh-credentials
            defaultMode: 0600
```

## TLS Certificate Management

### Automatic TLS with External Secrets

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: provider-tls-certs
  namespace: provider-namespace
spec:
  refreshInterval: 24h
  secretStoreRef:
    name: vault-secret-store
    kind: SecretStore
  target:
    name: provider-tls
    creationPolicy: Owner
    template:
      type: kubernetes.io/tls
      data:
        tls.crt: "{{ .certificate }}"
        tls.key: "{{ .private_key }}"
        ca.crt: "{{ .ca_certificate }}"
  data:
    - secretKey: certificate
      remoteRef:
        key: "pki/issue/virtrigaud"
        property: "certificate"
    - secretKey: private_key
      remoteRef:
        key: "pki/issue/virtrigaud"
        property: "private_key"
    - secretKey: ca_certificate
      remoteRef:
        key: "pki/issue/virtrigaud"
        property: "issuing_ca"

---
# Vault PKI configuration (run in Vault)
# vault write pki/roles/virtrigaud \
#   allowed_domains="virtrigaud.local,provider-service" \
#   allow_subdomains=true \
#   max_ttl="8760h" \
#   generate_lease=true
```

## Monitoring and Alerting

### ExternalSecret Monitoring

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: external-secrets-monitor
  namespace: monitoring
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: external-secrets
  endpoints:
    - port: metrics
      interval: 30s

---
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: external-secrets-alerts
  namespace: monitoring
spec:
  groups:
    - name: external-secrets.rules
      rules:
        - alert: ExternalSecretSyncFailure
          expr: increase(external_secrets_sync_calls_error[5m]) > 0
          for: 2m
          labels:
            severity: warning
          annotations:
            summary: "External secret sync failure"
            description: "ExternalSecret {{ $labels.name }} in namespace {{ $labels.namespace }} failed to sync"
        
        - alert: ExternalSecretStale
          expr: (time() - external_secrets_sync_calls_total) > 3600
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "External secret not refreshed"
            description: "ExternalSecret {{ $labels.name }} has not been refreshed for over 1 hour"
```

### Custom Monitoring

```go
package monitoring

import (
    "context"
    "time"
    
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
)

var (
    secretAge = promauto.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "virtrigaud_secret_age_seconds",
            Help: "Age of provider secrets in seconds",
        },
        []string{"secret_name", "namespace", "provider"},
    )
    
    secretRotationCount = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "virtrigaud_secret_rotations_total",
            Help: "Total number of secret rotations",
        },
        []string{"secret_name", "namespace", "provider"},
    )
)

type SecretMonitor struct {
    client kubernetes.Interface
}

func (sm *SecretMonitor) MonitorSecrets(ctx context.Context) {
    ticker := time.NewTicker(60 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            sm.updateSecretMetrics()
        }
    }
}

func (sm *SecretMonitor) updateSecretMetrics() {
    secrets, err := sm.client.CoreV1().Secrets("").List(context.TODO(), metav1.ListOptions{
        LabelSelector: "app.kubernetes.io/managed-by=external-secrets",
    })
    if err != nil {
        return
    }
    
    for _, secret := range secrets.Items {
        provider := secret.Labels["provider"]
        if provider == "" {
            continue
        }
        
        age := time.Since(secret.CreationTimestamp.Time).Seconds()
        secretAge.WithLabelValues(secret.Name, secret.Namespace, provider).Set(age)
    }
}
```

## Security Best Practices

### 1. Least Privilege Access

```yaml
# Minimal Vault policy for specific provider
path "secret/data/providers/vsphere/{{ identity.entity.aliases.auth_kubernetes_*.metadata.service_account_namespace }}" {
  capabilities = ["read"]
}

# Time-bound secrets
path "vsphere/creds/readonly" {
  capabilities = ["read"]
  allowed_parameters = {
    "ttl" = ["15m", "30m", "1h"]
  }
}
```

### 2. Secret Rotation Automation

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: rotate-provider-secrets
  namespace: virtrigaud-system
spec:
  schedule: "0 2 * * 0"  # Weekly on Sunday at 2 AM
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: secret-rotator
              image: virtrigaud/secret-rotator:latest
              command:
                - /bin/sh
                - -c
                - |
                  # Force refresh of all external secrets
                  kubectl annotate externalsecret --all \
                    force-sync="$(date +%s)" \
                    --namespace=vsphere-providers
                  
                  # Restart provider deployments to pick up new secrets
                  kubectl rollout restart deployment \
                    --selector=app.kubernetes.io/name=virtrigaud-provider-runtime \
                    --namespace=vsphere-providers
          restartPolicy: OnFailure
          serviceAccountName: secret-rotator

---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: secret-rotator
rules:
  - apiGroups: ["external-secrets.io"]
    resources: ["externalsecrets"]
    verbs: ["get", "list", "patch"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "patch"]
```

### 3. Audit Logging

```yaml
# Vault audit configuration
vault audit enable file file_path=/vault/logs/audit.log

# Example audit log entry structure
{
  "time": "2023-12-01T10:30:00Z",
  "type": "request",
  "auth": {
    "client_token": "hvs.xxx",
    "accessor": "hmac-sha256:xxx",
    "display_name": "kubernetes-virtrigaud-system-external-secrets",
    "policies": ["virtrigaud-policy"],
    "metadata": {
      "service_account_name": "external-secrets",
      "service_account_namespace": "virtrigaud-system"
    }
  },
  "request": {
    "id": "request-id",
    "operation": "read",
    "path": "secret/data/providers/vsphere",
    "data": null,
    "remote_address": "10.0.0.100"
  }
}
```

### 4. Emergency Procedures

```bash
#!/bin/bash
# emergency-secret-rotation.sh

echo "=== Emergency Secret Rotation ==="

# 1. Revoke all active leases for a provider
vault lease revoke -prefix vsphere/creds/

# 2. Force refresh all external secrets
kubectl get externalsecret --all-namespaces -o name | \
  xargs -I {} kubectl annotate {} force-sync="$(date +%s)"

# 3. Restart all provider deployments
kubectl get deployments --all-namespaces \
  -l app.kubernetes.io/name=virtrigaud-provider-runtime \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | \
  while read deployment; do
    kubectl rollout restart deployment $deployment
  done

# 4. Monitor rollout status
kubectl get deployments --all-namespaces \
  -l app.kubernetes.io/name=virtrigaud-provider-runtime \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | \
  while read deployment; do
    kubectl rollout status deployment $deployment --timeout=300s
  done

echo "Emergency rotation completed"
```

### 5. Secret Validation

```go
package validation

import (
    "crypto/x509"
    "encoding/pem"
    "fmt"
    "time"
)

func ValidateSecret(secretData map[string][]byte, secretType string) error {
    switch secretType {
    case "tls":
        return validateTLSSecret(secretData)
    case "ssh":
        return validateSSHSecret(secretData)
    case "credential":
        return validateCredentialSecret(secretData)
    }
    return nil
}

func validateTLSSecret(data map[string][]byte) error {
    cert, ok := data["tls.crt"]
    if !ok {
        return fmt.Errorf("missing tls.crt")
    }
    
    key, ok := data["tls.key"]
    if !ok {
        return fmt.Errorf("missing tls.key")
    }
    
    // Parse certificate
    block, _ := pem.Decode(cert)
    if block == nil {
        return fmt.Errorf("failed to parse certificate PEM")
    }
    
    parsedCert, err := x509.ParseCertificate(block.Bytes)
    if err != nil {
        return fmt.Errorf("failed to parse certificate: %w", err)
    }
    
    // Check expiration
    if time.Now().After(parsedCert.NotAfter) {
        return fmt.Errorf("certificate expired on %v", parsedCert.NotAfter)
    }
    
    if time.Now().Add(24*time.Hour).After(parsedCert.NotAfter) {
        return fmt.Errorf("certificate expires soon on %v", parsedCert.NotAfter)
    }
    
    // Validate key
    block, _ = pem.Decode(key)
    if block == nil {
        return fmt.Errorf("failed to parse private key PEM")
    }
    
    return nil
}
```

