# VirtRigaud Helm Chart

This Helm chart installs VirtRigaud, a Kubernetes operator for managing virtual machines across multiple hypervisors.

## Features

- **Automatic CRD Upgrades**: CRDs are automatically upgraded during `helm upgrade` (not just during initial install)
- **Multi-provider Support**: Deploy providers for vSphere, Libvirt/KVM, and Proxmox VE
- **Production Ready**: Secure defaults, RBAC, and resource limits
- **Flexible Configuration**: Extensive customization via values.yaml

## Prerequisites

- Kubernetes 1.25+
- Helm 3.8+

## Installation

### Quick Start

```bash
# Add the Helm repository
helm repo add virtrigaud https://projectbeskar.github.io/virtrigaud
helm repo update

# Install VirtRigaud
helm install virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --create-namespace
```

### Custom Installation

```bash
# Install with custom values
helm install virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --create-namespace \
  --set manager.replicaCount=2 \
  --set providers.vsphere.enabled=true \
  --set providers.libvirt.enabled=true
```

### Installation from Local Chart

```bash
# From repository root
helm install virtrigaud charts/virtrigaud \
  -n virtrigaud-system \
  --create-namespace
```

## Upgrading

### Automatic CRD Upgrades (Default)

By default, VirtRigaud automatically upgrades CRDs during `helm upgrade`:

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system
```

**How it works:**
- A Kubernetes Job runs before upgrade (Helm pre-upgrade hook)
- The Job applies all CRDs using `kubectl apply --server-side`
- CRDs are safely updated without data loss
- Job automatically cleans up after successful upgrade

**Benefits:**
- ✅ No manual CRD management needed
- ✅ Works seamlessly with GitOps tools (ArgoCD, Flux)
- ✅ Prevents CRD version drift
- ✅ Safe server-side apply with conflict resolution

### Disable Automatic CRD Upgrades

If you prefer to manage CRDs manually:

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --set crdUpgrade.enabled=false
```

Then manually apply CRDs before upgrade:

```bash
kubectl apply -f charts/virtrigaud/crds/
```

### Skip CRDs Entirely

If CRDs are managed externally (e.g., separate Helm chart):

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --skip-crds \
  --set crdUpgrade.enabled=false
```

## Configuration

### CRD Upgrade Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `crdUpgrade.enabled` | Enable automatic CRD upgrade during helm upgrade | `true` |
| `crdUpgrade.image.repository` | kubectl image repository | `alpine/k8s` |
| `crdUpgrade.image.tag` | kubectl image tag | `1.31.0` |
| `crdUpgrade.backoffLimit` | Job retry limit | `3` |
| `crdUpgrade.ttlSecondsAfterFinished` | Job cleanup time | `300` |
| `crdUpgrade.waitSeconds` | Wait time after applying CRDs | `5` |

### Manager Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `manager.replicaCount` | Number of manager replicas | `1` |
| `manager.image.repository` | Manager image repository | `projectbeskar/virtrigaud/manager` |
| `manager.image.tag` | Manager image tag | `v0.2.0` |
| `manager.resources.limits.cpu` | CPU limit | `500m` |
| `manager.resources.limits.memory` | Memory limit | `512Mi` |

### Provider Configuration

Each provider (vSphere, Libvirt, Proxmox) can be enabled/disabled:

```yaml
providers:
  vsphere:
    enabled: true
    replicaCount: 1
    image:
      tag: "v0.2.0"
  
  libvirt:
    enabled: true
    replicaCount: 1
  
  proxmox:
    enabled: false
```

See [values.yaml](values.yaml) for complete configuration options.

## GitOps Integration

### ArgoCD

VirtRigaud works seamlessly with ArgoCD. The automatic CRD upgrade feature uses Helm hooks, which ArgoCD executes properly:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: virtrigaud
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://projectbeskar.github.io/virtrigaud
    chart: virtrigaud
    targetRevision: 0.2.2
    helm:
      values: |
        crdUpgrade:
          enabled: true  # CRDs will upgrade automatically
  destination:
    server: https://kubernetes.default.svc
    namespace: virtrigaud-system
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
```

### Flux

Works with Flux HelmRelease:

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2beta1
kind: HelmRelease
metadata:
  name: virtrigaud
  namespace: virtrigaud-system
spec:
  chart:
    spec:
      chart: virtrigaud
      sourceRef:
        kind: HelmRepository
        name: virtrigaud
      version: 0.2.2
  values:
    crdUpgrade:
      enabled: true  # CRDs will upgrade automatically
  install:
    crds: CreateReplace
  upgrade:
    crds: CreateReplace
```

## Troubleshooting

### CRD Upgrade Job Fails

Check the job logs:

```bash
kubectl logs -n virtrigaud-system -l app.kubernetes.io/component=crd-upgrade
```

Common issues:
- **RBAC permissions**: Ensure ServiceAccount has CRD permissions
- **Image pull failures**: Check image repository and pull secrets
- **CRD conflicts**: Review conflict messages in logs

### Manual CRD Management

If automatic upgrades fail, manually apply CRDs:

```bash
# Apply all CRDs
kubectl apply -f charts/virtrigaud/crds/

# Or apply with server-side apply (recommended)
kubectl apply --server-side=true -f charts/virtrigaud/crds/
```

### Disable Automatic Upgrades Temporarily

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --set crdUpgrade.enabled=false \
  --reuse-values
```

## Uninstallation

```bash
# Uninstall the chart
helm uninstall virtrigaud -n virtrigaud-system

# Delete CRDs (WARNING: This deletes all VirtualMachine resources!)
kubectl delete crd -l app.kubernetes.io/name=virtrigaud
```

## Values File

See [values.yaml](values.yaml) for the complete list of configuration options.

## More Information

- [VirtRigaud Documentation](https://github.com/projectbeskar/virtrigaud/tree/main/docs)
- [Quick Start Guide](https://github.com/projectbeskar/virtrigaud/blob/main/docs/getting-started/quickstart.md)
- [Provider Documentation](https://github.com/projectbeskar/virtrigaud/blob/main/docs/PROVIDERS.md)
- [CRD Reference](https://github.com/projectbeskar/virtrigaud/blob/main/docs/CRDs.md)

