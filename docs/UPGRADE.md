# VirtRigaud Upgrade Guide

This guide covers upgrading VirtRigaud installations, including CRD updates and breaking changes.

## Quick Upgrade

### Helm-based Upgrade (Recommended)

```bash
# 1. Update Helm repository
helm repo update

# 2. Check for breaking changes
helm diff upgrade virtrigaud virtrigaud/virtrigaud --version v0.2.1

# 3. Upgrade CRDs first (required for schema changes)
helm pull virtrigaud/virtrigaud --version v0.2.1 --untar
kubectl apply -f virtrigaud/crds/

# 4. Upgrade VirtRigaud
helm upgrade virtrigaud virtrigaud/virtrigaud \
  --namespace virtrigaud-system \
  --version v0.2.1
```

### Alternative: Direct CRD Download

```bash
# Download and apply CRDs from release
curl -L "https://github.com/projectbeskar/virtrigaud/releases/download/v0.2.1/virtrigaud-crds.yaml" | kubectl apply -f -

# Upgrade application
helm upgrade virtrigaud virtrigaud/virtrigaud --version v0.2.1
```

## Version-Specific Upgrade Notes

### v0.2.0 → v0.2.1

**Breaking Changes:**
- ✅ PowerState validation fixed (OffGraceful now supported)
- ✅ Hardware version management added (vSphere only)
- ✅ Disk size configuration respected

**Required Actions:**
1. **CRD Update Required**: New powerState validation and schema changes
2. **Provider Image Update**: Ensure providers use v0.2.1+ images for new features
3. **Field Testing**: Verify OffGraceful, hardware version, and disk sizing work correctly

**Upgrade Steps:**
```bash
# 1. Backup existing resources
kubectl get virtualmachines,vmclasses,providers -A -o yaml > virtrigaud-backup-v021.yaml

# 2. Update CRDs (fixes OffGraceful validation)
kubectl apply -f https://github.com/projectbeskar/virtrigaud/releases/download/v0.2.1/virtrigaud-crds.yaml

# 3. Upgrade VirtRigaud
helm upgrade virtrigaud virtrigaud/virtrigaud --version v0.2.1

# 4. Verify OffGraceful works
kubectl patch virtualmachine <vm-name> --type='merge' -p='{"spec":{"powerState":"OffGraceful"}}'
```

## Rollback Procedures

### Rollback to Previous Version

```bash
# 1. Rollback application
helm rollback virtrigaud <revision>

# 2. Rollback CRDs (if schema breaking changes)
kubectl apply -f https://github.com/projectbeskar/virtrigaud/releases/download/v0.2.0/virtrigaud-crds.yaml

# 3. Verify resources still work
kubectl get virtualmachines -A
```

### Emergency Recovery

```bash
# 1. Restore from backup
kubectl apply -f virtrigaud-backup-v021.yaml

# 2. Check controller logs
kubectl logs -n virtrigaud-system deployment/virtrigaud-manager

# 3. Force reconciliation
kubectl annotate virtualmachine <vm-name> virtrigaud.io/force-sync="$(date)"
```

## Automated Upgrade with GitOps

### ArgoCD

```yaml
apiVersion: argoproj.io/v1beta1
kind: Application
metadata:
  name: virtrigaud
spec:
  source:
    chart: virtrigaud
    repoURL: https://projectbeskar.github.io/virtrigaud
    targetRevision: "0.2.1"
    helm:
      parameters:
      - name: manager.image.tag
        value: "v0.2.1"
  syncPolicy:
    syncOptions:
    - CreateNamespace=true
    - Replace=true  # Required for CRD updates
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
      version: "0.2.1"
      sourceRef:
        kind: HelmRepository
        name: virtrigaud
  upgrade:
    crds: CreateReplace  # Ensure CRDs are updated
```

## Troubleshooting Upgrades

### CRD Validation Errors

```bash
# Check CRD status
kubectl get crd virtualmachines.infra.virtrigaud.io -o yaml

# Fix validation conflicts
kubectl patch crd virtualmachines.infra.virtrigaud.io --type='json' -p='[{"op": "remove", "path": "/spec/versions/0/schema/openAPIV3Schema/properties/spec/properties/powerState/allOf"}]'
```

### Provider Image Mismatch

```bash
# Check provider images
kubectl get providers -o jsonpath='{.items[*].spec.runtime.image}'

# Update provider image
kubectl patch provider <provider-name> --type='merge' -p='{"spec":{"runtime":{"image":"ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.1"}}}'
```

### Resource Conflicts

```bash
# Check for resource conflicts
kubectl get events --sort-by=.metadata.creationTimestamp

# Force resource refresh
kubectl delete pod -l app.kubernetes.io/name=virtrigaud -n virtrigaud-system
```

## Best Practices

### Pre-Upgrade Checklist

- [ ] Backup all VirtRigaud resources
- [ ] Check for breaking changes in release notes
- [ ] Test upgrade in staging environment
- [ ] Verify provider connectivity
- [ ] Plan rollback strategy

### Post-Upgrade Verification

- [ ] All CRDs updated successfully
- [ ] Controller manager running
- [ ] Providers healthy and responsive
- [ ] Existing VMs still manageable
- [ ] New features working (OffGraceful, hardware version, etc.)

### Monitoring During Upgrade

```bash
# Watch controller logs
kubectl logs -n virtrigaud-system deployment/virtrigaud-manager -f

# Monitor VM status
kubectl get virtualmachines -A --watch

# Check provider health
kubectl get providers -o custom-columns=NAME:.metadata.name,STATUS:.status.conditions[0].type,MESSAGE:.status.conditions[0].message
```

## Support and Recovery

If you encounter issues during upgrade:

1. **Check Release Notes**: https://github.com/projectbeskar/virtrigaud/releases
2. **Review Logs**: Controller and provider logs for error details
3. **Community Support**: GitHub issues and discussions
4. **Emergency Rollback**: Use documented rollback procedures

Remember: Always test upgrades in non-production environments first!

## Development Workflow (v0.2.1+)

### Automated CRD Synchronization

Starting with v0.2.1, VirtRigaud includes automated tooling to ensure CRDs stay in sync between development and Helm chart deployments.

#### For Developers

```bash
# Generate and sync CRDs automatically
make sync-helm-crds

# Verify CRDs are in sync
make verify-helm-crds

# Package Helm chart with latest CRDs
make helm-package
```

#### Pre-commit Hooks

Install pre-commit hooks to automatically sync CRDs:

```bash
# Install pre-commit
pip install pre-commit

# Install hooks
pre-commit install

# CRDs will now sync automatically on commits that modify:
# - api/**.go files
# - config/crd/**.yaml files
```

#### CI/CD Integration

The CI/CD pipeline now automatically:

1. **Validates CRD sync** on every pull request
2. **Syncs CRDs** before Helm chart packaging in releases  
3. **Fails builds** if Helm chart CRDs are out of sync

This prevents the v0.2.1-rc2 issue where OffGraceful validation failed due to stale Helm chart CRDs.

### Repository Workflow

```bash
# 1. Make API changes
vim api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go

# 2. Generate and sync CRDs (automated by pre-commit)
make sync-helm-crds

# 3. Commit (hooks will verify sync)
git add .
git commit -m "feat: add new VM power states"

# 4. CI validates everything is in sync
git push origin feature-branch
```
