# VM Migration Guide

This guide explains how to migrate VMs between providers using VirtRigaud's VM migration feature.

## Overview

VirtRigaud supports migrating VMs across different providers (Libvirt, Proxmox, vSphere) without data loss. The migration uses a cold migration approach:

1. **Snapshot**: Create a snapshot of the source VM (optional)
2. **Export**: Export the VM disk from the source provider
3. **Transfer**: Upload the disk to intermediate storage (S3/HTTP/NFS)
4. **Import**: Download and import the disk to the target provider
5. **Create**: Create the target VM with the migrated disk
6. **Validate**: Verify the migration succeeded

## Prerequisites

### Storage Backend

You must configure intermediate storage for disk transfer. Supported options:

- **S3-compatible storage** (AWS S3, MinIO)
- **HTTP/HTTPS file server**
- **NFS shared storage**

### Provider Requirements

- Source and target providers must be running and healthy
- Source VM must be provisioned and accessible
- Target provider must have sufficient storage space
- Network connectivity between providers and storage

## Basic Migration

### Step 1: Create Storage Configuration

If using S3:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: migration-storage-credentials
  namespace: virtrigaud-system
type: Opaque
stringData:
  accessKey: "YOUR_ACCESS_KEY"
  secretKey: "YOUR_SECRET_KEY"
```

### Step 2: Create VMMigration Resource

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: migrate-vm-to-proxmox
  namespace: default
spec:
  # Source VM configuration
  source:
    vmRef:
      name: my-libvirt-vm
    createSnapshot: true  # Create snapshot before migration
  
  # Target configuration
  target:
    name: my-vm-on-proxmox
    providerRef:
      name: proxmox-provider
      namespace: virtrigaud-system
    classRef:
      name: medium-vm
    networks:
      - name: default
  
  # Storage configuration
  storage:
    type: s3
    bucket: vm-migrations
    region: us-east-1
    endpoint: https://s3.amazonaws.com
    credentialsSecretRef:
      name: migration-storage-credentials
  
  # Migration options
  options:
    diskFormat: qcow2
    compress: true
    verifyChecksums: true
    timeout: 4h
    retryPolicy:
      maxRetries: 3
      retryDelay: 5m
      backoffMultiplier: 2
```

### Step 3: Monitor Migration Progress

```bash
# Watch migration status
kubectl get vmmigration migrate-vm-to-proxmox -w

# View detailed status
kubectl describe vmmigration migrate-vm-to-proxmox

# Check migration logs
kubectl logs -n virtrigaud-system deployment/virtrigaud-manager -f | grep migrate-vm-to-proxmox
```

### Step 4: Verify Migration

Once the migration reaches `Ready` phase:

```bash
# Check target VM was created
kubectl get virtualmachine my-vm-on-proxmox

# Verify VM is running
kubectl describe virtualmachine my-vm-on-proxmox
```

## Advanced Features

### Using Existing Snapshot

To migrate from a specific snapshot:

```yaml
spec:
  source:
    vmRef:
      name: my-vm
    snapshotRef:
      name: pre-migration-snapshot
    createSnapshot: false  # Use existing snapshot
```

### Delete Source VM After Migration

```yaml
spec:
  source:
    vmRef:
      name: my-vm
    deleteAfterMigration: true  # Delete source VM after successful migration
```

### Cross-Namespace Migration

```yaml
spec:
  source:
    vmRef:
      name: vm-in-prod
      namespace: production
    providerRef:
      name: vsphere-provider
      namespace: infrastructure
  
  target:
    name: vm-in-staging
    namespace: staging
    providerRef:
      name: proxmox-provider
      namespace: infrastructure
```

### NFS Storage Backend

For on-premises deployments:

```yaml
spec:
  storage:
    type: nfs
    endpoint: /mnt/nfs-migration-share
```

The NFS share must be mounted on all provider pods at the same path.

### HTTP Storage Backend

For simple file server storage:

```yaml
spec:
  storage:
    type: http
    endpoint: http://fileserver.local/migrations
```

## Migration Phases

The migration goes through the following phases:

| Phase | Description |
|-------|-------------|
| `Pending` | Migration created, awaiting processing |
| `Validating` | Validating source VM, target provider, storage |
| `Snapshotting` | Creating snapshot of source VM |
| `Exporting` | Exporting disk from source provider |
| `Importing` | Importing disk to target provider |
| `Creating` | Creating target VM |
| `ValidatingTarget` | Verifying target VM is ready |
| `Ready` | Migration completed successfully |
| `Failed` | Migration failed (will retry if configured) |

## Monitoring

### Status Conditions

```bash
kubectl get vmmigration migrate-vm -o jsonpath='{.status.conditions}' | jq
```

Key conditions:
- `Ready`: Migration completed
- `Validating`: Validation in progress
- `Exporting`: Disk export in progress
- `Importing`: Disk import in progress
- `Failed`: Migration failed

### Progress Tracking

```bash
kubectl get vmmigration migrate-vm -o jsonpath='{.status.progress}' | jq
```

Shows:
- Bytes transferred
- Overall percentage
- Current phase
- Estimated time remaining

## Troubleshooting

### Migration Stuck in Exporting Phase

**Problem**: Export task not completing

**Solutions**:
1. Check source provider is healthy: `kubectl get provider <name>`
2. Verify storage credentials are correct
3. Check network connectivity to storage
4. Review provider logs for errors

### Migration Failed with Storage Error

**Problem**: Can't access intermediate storage

**Solutions**:
1. Verify storage configuration is correct
2. Check credentials secret exists and is valid
3. Test connectivity: `aws s3 ls s3://bucket` or curl for HTTP
4. Ensure storage has sufficient space

### Target VM Creation Failed

**Problem**: VM creation fails on target provider

**Solutions**:
1. Check target provider has sufficient resources
2. Verify VMClass and network references exist
3. Review target provider logs
4. Check imported disk is valid

### Migration Keeps Retrying

**Problem**: Migration retries but keeps failing

**Solutions**:
1. Check the error message in status.message
2. Review the retry count: `kubectl get vmmigration -o jsonpath='{.status.retryCount}'`
3. If max retries reached, migration will stay failed
4. Delete and recreate migration after fixing root cause

## Cleanup

### Manual Cleanup

Delete the migration resource:

```bash
kubectl delete vmmigration migrate-vm
```

This automatically cleans up:
- Intermediate storage artifacts
- Migration-created snapshots
- Partially created target VMs (if failed)

### Cleanup Configuration

Control cleanup behavior:

```yaml
spec:
  source:
    deleteAfterMigration: true  # Delete source VM after success
  
  options:
    cleanupPolicy:
      keepSnapshots: false  # Delete migration snapshots
      keepIntermediateStorage: false  # Delete storage artifacts
```

## Best Practices

### 1. Pre-Migration Checklist

- [ ] Verify source VM is in a consistent state
- [ ] Create a manual snapshot for rollback
- [ ] Test storage connectivity
- [ ] Verify sufficient storage space (2x VM disk size)
- [ ] Plan for downtime if source VM must be powered off

### 2. Storage Selection

- **S3**: Best for cloud deployments, multi-site migrations
- **NFS**: Best for on-premises, high-speed local network
- **HTTP**: Simple setups, temporary migrations

### 3. Network Considerations

- Migration time depends on network bandwidth
- 100GB VM over 1Gbps network: ~15 minutes
- 1TB VM over 100Mbps network: ~24 hours
- Use compression for slow networks

### 4. Validation

After migration:
- Test VM boots successfully
- Verify applications work correctly
- Check network connectivity
- Validate disk integrity

### 5. Rollback Plan

Before migration:
1. Document source VM configuration
2. Create manual snapshot
3. Test restore procedure
4. Keep source VM until validation complete

## Performance Tuning

### Compression

Enable compression for slower networks:

```yaml
spec:
  options:
    compress: true  # Trade CPU for bandwidth
```

### Parallel Migrations

Run multiple migrations concurrently:

```bash
# Migrate multiple VMs
for vm in vm1 vm2 vm3; do
  kubectl apply -f migration-${vm}.yaml
done
```

### Timeout Configuration

Adjust timeout for large VMs:

```yaml
spec:
  options:
    timeout: 8h  # For very large VMs
```

## Security Considerations

### Storage Credentials

- Store credentials in Kubernetes Secrets
- Use RBAC to restrict access
- Rotate credentials regularly
- Use encrypted storage (S3 SSE, HTTPS)

### Network Security

- Use HTTPS/TLS for all transfers
- Verify checksums are enabled
- Use VPN for cross-site migrations
- Isolate migration traffic if possible

## Examples

See `docs/examples/` for complete examples:

- `vmmigration-basic.yaml`: Simple migration
- `vmmigration-s3.yaml`: Migration with S3 storage
- `vmmigration-nfs.yaml`: Migration with NFS storage
- `vmmigration-advanced.yaml`: Advanced configuration
- `vmmigration-cross-namespace.yaml`: Cross-namespace migration

## API Reference

For detailed API documentation, see:
- [VMMigration CRD Reference](API_REFERENCE.md#vmmigration)
- [Migration Status Fields](API_REFERENCE.md#vmmigrationstatus)
- [Storage Configuration](API_REFERENCE.md#migrationstorage)

