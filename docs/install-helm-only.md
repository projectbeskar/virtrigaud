# Helm-only Installation & Verify Conversion

This guide covers installing virtrigaud using only Helm (without pre-applying CRDs via Kustomize) and verifying that API conversion is working correctly.

## Helm-only Install

Virtrigaud can be installed using only Helm, which will automatically install all required CRDs including conversion webhook configuration.

### Prerequisites

- Kubernetes cluster (1.26+)
- Helm 3.8+
- `kubectl` configured to access your cluster

### Installation

```bash
# Add the virtrigaud Helm repository (if available)
helm repo add virtrigaud https://projectbeskar.github.io/virtrigaud
helm repo update

# Or install directly from source
git clone https://github.com/projectbeskar/virtrigaud.git
cd virtrigaud

# Install virtrigaud with CRDs
helm install virtrigaud charts/virtrigaud \
  --namespace virtrigaud \
  --create-namespace \
  --wait \
  --timeout 10m
```

### Skip CRDs (if already installed)

If you need to install the chart without CRDs (e.g., they're managed separately):

```bash
helm install virtrigaud charts/virtrigaud \
  --namespace virtrigaud \
  --create-namespace \
  --skip-crds \
  --wait
```

## Verify Conversion

After installation, verify that API conversion is working correctly.

### Check CRD Conversion Configuration

```bash
# Verify all CRDs have conversion webhook configuration
kubectl get crd virtualmachines.infra.virtrigaud.io -o yaml | yq '.spec.conversion'
```

Expected output:
```yaml
strategy: Webhook
webhook:
  clientConfig:
    service:
      name: virtrigaud-webhook
      namespace: virtrigaud
      path: /convert
  conversionReviewVersions:
  - v1
```

### Check API Versions

Verify that both v1alpha1 and v1beta1 versions are available:

```bash
# Check available versions for VirtualMachine CRD
kubectl get crd virtualmachines.infra.virtrigaud.io -o jsonpath='{.spec.versions[*].name}' | tr ' ' '\n'
```

Expected output:
```
v1alpha1
v1beta1
```

### Verify Storage Version

Confirm that v1beta1 is set as the storage version:

```bash
# Check storage version
kubectl get crd virtualmachines.infra.virtrigaud.io -o jsonpath='{.spec.versions[?(@.storage==true)].name}'
```

Expected output:
```
v1beta1
```

### Test Conversion

Create resources using different API versions and verify conversion works:

```bash
# Create a VM using v1alpha1 API
cat <<EOF | kubectl apply -f -
apiVersion: infra.virtrigaud.io/v1alpha1
kind: VirtualMachine
metadata:
  name: test-vm-alpha
  namespace: default
spec:
  providerRef:
    name: test-provider
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  powerState: "On"
EOF

# Read it back as v1beta1
kubectl get vm test-vm-alpha -o yaml | grep "apiVersion:"
# Should show: apiVersion: infra.virtrigaud.io/v1beta1

# Create a VM using v1beta1 API
cat <<EOF | kubectl apply -f -
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: test-vm-beta
  namespace: default
spec:
  providerRef:
    name: test-provider
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  powerState: On
EOF

# Clean up test resources
kubectl delete vm test-vm-alpha test-vm-beta
```

## Troubleshooting

### Conversion Webhook Missing

If the conversion webhook is missing or not configured:

```bash
# Check if webhook service exists
kubectl get svc virtrigaud-webhook -n virtrigaud

# Check webhook pod logs
kubectl logs -l app.kubernetes.io/name=virtrigaud -n virtrigaud

# Verify webhook certificate
kubectl get secret virtrigaud-webhook-certs -n virtrigaud
```

### Conversion Webhook Failing

If conversion is failing:

```bash
# Check conversion webhook logs
kubectl logs -l app.kubernetes.io/name=virtrigaud -n virtrigaud | grep conversion

# Test webhook connectivity
kubectl get --raw "/api/v1/namespaces/virtrigaud/services/virtrigaud-webhook:webhook/proxy/convert"

# Check webhook certificate validity
kubectl get secret virtrigaud-webhook-certs -n virtrigaud -o yaml
```

### API Version Issues

If certain API versions aren't working:

```bash
# List all available APIs
kubectl api-resources | grep virtrigaud

# Check specific CRD status
kubectl describe crd virtualmachines.infra.virtrigaud.io

# Verify controller is running
kubectl get pods -l app.kubernetes.io/name=virtrigaud -n virtrigaud
```

## Integration with GitOps

### ArgoCD

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: virtrigaud
spec:
  source:
    chart: virtrigaud
    repoURL: https://projectbeskar.github.io/virtrigaud
    targetRevision: "1.0.0"
    helm:
      values: |
        manager:
          image:
            repository: ghcr.io/projectbeskar/virtrigaud/manager
            tag: v1.0.0
```

### Flux

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2beta1
kind: HelmRelease
metadata:
  name: virtrigaud
spec:
  chart:
    spec:
      chart: virtrigaud
      sourceRef:
        kind: HelmRepository
        name: virtrigaud
      version: "1.0.0"
  values:
    manager:
      image:
        repository: ghcr.io/projectbeskar/virtrigaud/manager
        tag: v1.0.0
```

## Migration from Kustomize to Helm

If you're currently using Kustomize for CRD management and want to switch to Helm:

1. **Backup existing resources:**
   ```bash
   kubectl get vms,providers,vmclasses -A -o yaml > virtrigaud-backup.yaml
   ```

2. **Uninstall Kustomize-managed CRDs (optional):**
   ```bash
   kubectl delete -k config/default
   ```

3. **Install via Helm:**
   ```bash
   helm install virtrigaud charts/virtrigaud --namespace virtrigaud --create-namespace
   ```

4. **Restore resources:**
   ```bash
   kubectl apply -f virtrigaud-backup.yaml
   ```

The conversion webhook will handle any necessary API version transformations automatically.
