# Automatic CRD Upgrades in VirtRigaud Helm Chart

## Overview

VirtRigaud Helm chart now supports **automatic CRD upgrades** during `helm upgrade`. This eliminates the need for manual CRD management and provides a seamless upgrade experience.

## The Problem

By default, Helm has a limitation:
- CRDs are installed during `helm install`
- CRDs are **NOT upgraded** during `helm upgrade`

This means users had to manually apply CRD updates before upgrading, which was:
- Error-prone
- Easy to forget
- Breaks GitOps workflows
- Causes version drift between chart and CRDs

## The Solution

VirtRigaud uses **Helm Hooks** with a Kubernetes Job to automatically apply CRDs during both install and upgrade:

### kubectl Image

VirtRigaud builds and publishes its own `kubectl` image as part of the release process. This image:

- Based on Alpine Linux for minimal size (~50MB)
- Includes kubectl 1.31.0 binary from official Kubernetes releases
- Includes bash and shell for scripting support
- Runs as non-root user (UID 65532)
- Verified with SHA256 checksums
- Signed with Cosign and includes SBOM

The image is automatically built and tagged to match each VirtRigaud release version, ensuring version consistency across all components.

**Image Location**: `ghcr.io/projectbeskar/virtrigaud/kubectl:<version>`

### How It Works

1. **Pre-Upgrade Hook**: Before the main upgrade starts, a Job is created
2. **CRD Application**: The Job applies all CRDs using `kubectl apply --server-side`
3. **Safe Upgrades**: Server-side apply handles conflicts gracefully
4. **Automatic Cleanup**: Job is deleted after successful completion

### Architecture

```
helm upgrade virtrigaud
    ↓
[Pre-Upgrade Hook -10]
    ↓
ConfigMap with CRDs created
    ↓
[Pre-Upgrade Hook -5]
    ↓
ServiceAccount + RBAC created
    ↓
[Pre-Upgrade Hook 0]
    ↓
Job applies CRDs via kubectl
    ↓
[Standard Helm Resources]
    ↓
Manager & Providers deployed
    ↓
[Hook Cleanup]
    ↓
Job & Hook resources deleted
```

## Features

### Enabled by Default

No configuration needed - just works:

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud -n virtrigaud-system
```

### Server-Side Apply

Uses `kubectl apply --server-side` for:
- Safe conflict resolution
- Field management
- No ownership conflicts

### GitOps Compatible

Works seamlessly with:
- **ArgoCD**: Helm hooks execute properly
- **Flux**: Compatible with HelmRelease CRD upgrades
- **Terraform**: Helm provider handles hooks

### Configurable

Customize the upgrade behavior:

```yaml
crdUpgrade:
  enabled: true  # Enable/disable automatic upgrades
  
  image:
    repository: ghcr.io/projectbeskar/virtrigaud/kubectl  # VirtRigaud kubectl image
    tag: "v0.2.0"  # Auto-updated to match release version
  
  backoffLimit: 3
  ttlSecondsAfterFinished: 300
  waitSeconds: 5
  
  resources:
    limits:
      cpu: 100m
      memory: 128Mi
```

## Usage Examples

### Standard Upgrade (Automatic CRDs)

```bash
# CRDs are automatically upgraded
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system
```

### Disable Automatic CRD Upgrade

```bash
# Disable if you manage CRDs separately
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --set crdUpgrade.enabled=false
```

### Manual CRD Management

```bash
# Apply CRDs manually before upgrade
kubectl apply -f charts/virtrigaud/crds/

# Then upgrade without CRD management
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --set crdUpgrade.enabled=false
```

### Skip CRDs Entirely

```bash
# Skip CRDs during upgrade (for external CRD management)
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --skip-crds \
  --set crdUpgrade.enabled=false
```

## GitOps Integration

### ArgoCD

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: virtrigaud
spec:
  source:
    chart: virtrigaud
    targetRevision: 0.2.2
    helm:
      values: |
        crdUpgrade:
          enabled: true  # Automatic upgrades work!
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

**Note**: ArgoCD executes Helm hooks properly, so CRDs will be upgraded automatically.

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
      version: 0.2.2
  values:
    crdUpgrade:
      enabled: true  # Automatic upgrades work!
  install:
    crds: CreateReplace
  upgrade:
    crds: CreateReplace
```

**Note**: Flux's `crds: CreateReplace` works alongside our hook-based upgrades for maximum compatibility.

## Troubleshooting

### Check CRD Upgrade Job

```bash
# View job status
kubectl get jobs -n virtrigaud-system -l app.kubernetes.io/component=crd-upgrade

# View job logs
kubectl logs -n virtrigaud-system -l app.kubernetes.io/component=crd-upgrade

# View job details
kubectl describe job -n virtrigaud-system -l app.kubernetes.io/component=crd-upgrade
```

### Common Issues

#### 1. RBAC Permissions

**Symptom**: Job fails with "forbidden" errors

**Solution**: Ensure the ServiceAccount has CRD permissions:

```bash
kubectl get clusterrole -l app.kubernetes.io/component=crd-upgrade
kubectl describe clusterrole <role-name>
```

#### 2. Image Pull Failures

**Symptom**: Job fails to start, ImagePullBackOff

**Solution**: Check image configuration:

```yaml
crdUpgrade:
  image:
    repository: ghcr.io/projectbeskar/virtrigaud/kubectl
    tag: "v0.2.2-rc1"  # Use matching VirtRigaud version
    pullPolicy: IfNotPresent
```

#### 3. CRD Conflicts

**Symptom**: Apply errors about field conflicts

**Solution**: Server-side apply handles this automatically, but you can force:

```bash
kubectl apply --server-side=true --force-conflicts -f charts/virtrigaud/crds/
```

#### 4. Job Not Cleaning Up

**Symptom**: Old jobs remain after upgrade

**Solution**: Adjust TTL or manually clean:

```bash
kubectl delete jobs -n virtrigaud-system -l app.kubernetes.io/component=crd-upgrade
```

### Debug Mode

Enable verbose logging:

```bash
helm upgrade virtrigaud virtrigaud/virtrigaud \
  -n virtrigaud-system \
  --debug
```

## Migration Guide

### Migrating from Manual CRD Management

If you were previously managing CRDs manually:

1. **Enable automatic upgrades**:
   ```bash
   helm upgrade virtrigaud virtrigaud/virtrigaud \
     -n virtrigaud-system \
     --set crdUpgrade.enabled=true
   ```

2. **Verify CRDs are upgraded**:
   ```bash
   kubectl get crd -l app.kubernetes.io/name=virtrigaud
   ```

3. **Remove manual steps from your upgrade process**

### Migrating to External CRD Management

If you want to manage CRDs externally (e.g., separate Helm chart):

1. **Disable automatic upgrades**:
   ```yaml
   crdUpgrade:
     enabled: false
   ```

2. **Extract CRDs**:
   ```bash
   helm show crds virtrigaud/virtrigaud > my-crds.yaml
   ```

3. **Manage CRDs separately**:
   ```bash
   kubectl apply -f my-crds.yaml
   ```

## Technical Details

### Hook Weights

The upgrade process uses weighted hooks for proper ordering:

| Weight | Resource | Purpose |
|--------|----------|---------|
| `-10` | ConfigMap | Store CRD content |
| `-5` | RBAC | Create permissions |
| `0` | Job | Apply CRDs |

### Resource Requirements

The CRD upgrade job is lightweight:

```yaml
resources:
  limits:
    cpu: 100m
    memory: 128Mi
  requests:
    cpu: 50m
    memory: 64Mi
```

### Security

- Runs as non-root user (65532)
- Read-only root filesystem
- No privilege escalation
- Minimal RBAC (only CRD permissions)
- Automatic cleanup after completion

### Compatibility

- **Kubernetes**: 1.25+
- **Helm**: 3.8+
- **kubectl**: 1.24+ (in Job image)

## Best Practices

1. **Use Automatic Upgrades**: Enable by default for best UX
2. **Monitor Job Logs**: Check logs during first upgrade
3. **Test in Dev First**: Verify upgrades in non-production
4. **Backup CRDs**: Keep backups before major upgrades
5. **Review Changelogs**: Check for breaking CRD changes

## FAQ

### Q: Will this delete my existing resources?

**A**: No. CRD upgrades are additive and preserve existing Custom Resources.

### Q: What happens if the job fails?

**A**: Helm upgrade will fail, leaving your cluster in the previous state. Fix the issue and retry.

### Q: Can I use this with ArgoCD?

**A**: Yes! ArgoCD properly executes Helm hooks.

### Q: Does this work with Flux?

**A**: Yes! Flux HelmRelease handles hooks correctly.

### Q: How do I roll back?

**A**: Use `helm rollback`. CRDs are not rolled back (Kubernetes limitation).

### Q: Can I customize the kubectl image?

**A**: Yes, via `crdUpgrade.image.repository` and `crdUpgrade.image.tag`. The default uses the official Kubernetes kubectl image from `registry.k8s.io`.

## References

- [Helm Hooks Documentation](https://helm.sh/docs/topics/charts_hooks/)
- [Kubernetes CRD Documentation](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/)
- [Server-Side Apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)

