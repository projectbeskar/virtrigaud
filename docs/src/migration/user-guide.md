# VirtRigaud VM Migration - User Guide

## Overview

VirtRigaud provides comprehensive VM migration capabilities that enable you to move virtual machines between different hypervisor platforms (Libvirt/KVM, Proxmox VE, VMware vSphere) using intermediate storage (S3, HTTP, NFS).

### Key Features

- **Cross-Platform Migration**: Migrate VMs between Libvirt, Proxmox, and vSphere
- **Multiple Storage Backends**: Use S3, HTTP servers, or NFS for intermediate storage
- **Format Conversion**: Automatic conversion between qcow2, VMDK, and raw disk formats
- **Progress Tracking**: Monitor migration progress in real-time
- **Checksum Validation**: Ensure data integrity with SHA256 checksums
- **State Management**: Track migration phases from start to completion

### Architecture

```
Source Provider         Storage Layer          Target Provider
   (Export)         →   (S3/HTTP/NFS)    →       (Import)
      ↓                       ↓                      ↓
  Disk Access          Upload/Download         Disk Creation
  Format Convert       Checksum Verify         Format Convert
```

## Getting Started

### Prerequisites

1. **VirtRigaud Installed**: Manager and provider components running
2. **Storage Backend**: Access to S3, HTTP server, or NFS mount
3. **qemu-img**: Installed on provider hosts (for format conversion)
4. **Network Access**: Providers can reach storage backend
5. **Credentials**: Storage credentials in Kubernetes secrets

### Quick Start

Here's a simple migration from Libvirt to vSphere via S3:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: migrate-vm-to-vsphere
  namespace: default
spec:
  sourceName: my-libvirt-vm          # Source VM name
  sourceNamespace: default            # Source VM namespace
  targetProviderRef:
    name: vsphere-prod                # Target provider
    namespace: default
  targetName: migrated-vm             # New VM name
  storage:
    type: s3
    bucket: vm-migrations
    endpoint: s3.amazonaws.com
    region: us-east-1
    credentialsSecretRef:
      name: s3-credentials
      namespace: default
  cleanupPolicy:
    deleteIntermediate: true          # Clean up S3 after migration
    deleteSnapshot: true               # Delete source snapshot
    deleteSource: false                # Keep source VM
  retryPolicy:
    maxAttempts: 3
    backoffDuration: 5m
```

Apply the migration:

```bash
kubectl apply -f migration.yaml
```

Monitor progress:

```bash
kubectl get vmmigration migrate-vm-to-vsphere -w
```

## Storage Backend Configuration

### S3 Storage

#### AWS S3

```yaml
storage:
  type: s3
  bucket: my-bucket
  region: us-east-1
  credentialsSecretRef:
    name: aws-s3-creds
    namespace: default
```

Create credentials secret:

```bash
kubectl create secret generic aws-s3-creds \
  --from-literal=accessKey=AKIAIOSFODNN7EXAMPLE \
  --from-literal=secretKey=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

#### MinIO

```yaml
storage:
  type: s3
  bucket: vm-migrations
  endpoint: minio.example.com:9000
  region: us-east-1
  credentialsSecretRef:
    name: minio-creds
    namespace: default
```

Create MinIO credentials:

```bash
kubectl create secret generic minio-creds \
  --from-literal=accessKey=minioadmin \
  --from-literal=secretKey=minioadmin \
  --from-literal=endpoint=http://minio.example.com:9000
```

### HTTP Storage

```yaml
storage:
  type: http
  endpoint: https://storage.example.com/vm-exports
  credentialsSecretRef:
    name: http-creds
    namespace: default
```

Create HTTP credentials:

```bash
kubectl create secret generic http-creds \
  --from-literal=token=Bearer_your-api-token-here
```

### NFS Storage

```yaml
storage:
  type: nfs
  path: /vm-migrations
  endpoint: nfs.example.com
```

**Note**: NFS storage requires the NFS share to be mounted on provider hosts at the specified path.

## Migration Scenarios

### Scenario 1: Libvirt → vSphere

**Use Case**: Moving VMs from on-premises KVM to VMware vCenter

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: kvm-to-vsphere
spec:
  sourceName: ubuntu-server
  sourceNamespace: kvm-vms
  targetProviderRef:
    name: vcenter-prod
    namespace: default
  targetName: ubuntu-server-vcenter
  targetStorageHint: datastore1      # vSphere datastore
  storage:
    type: s3
    bucket: migrations
    region: us-east-1
    credentialsSecretRef:
      name: s3-creds
  cleanupPolicy:
    deleteIntermediate: true
    deleteSnapshot: true
    deleteSource: false               # Keep source for rollback
```

**What Happens**:
1. Creates snapshot of source VM (qcow2 format)
2. Exports disk from Libvirt storage pool
3. Converts qcow2 → VMDK (via qemu-img)
4. Uploads VMDK to S3
5. Downloads VMDK from S3
6. Uploads VMDK to vSphere datastore
7. Creates new VM in vSphere with migrated disk

### Scenario 2: vSphere → Proxmox

**Use Case**: Migrating from VMware to Proxmox VE

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: vsphere-to-proxmox
spec:
  sourceName: windows-server
  sourceNamespace: vmware-vms
  targetProviderRef:
    name: proxmox-cluster
    namespace: default
  targetName: windows-server-pve
  targetStorageHint: local-lvm       # Proxmox storage
  storage:
    type: nfs
    path: /mnt/migrations
    endpoint: nfs.local
  cleanupPolicy:
    deleteIntermediate: true
    deleteSnapshot: true
    deleteSource: false
```

**What Happens**:
1. Creates snapshot of source VM (VMDK format)
2. Downloads VMDK from vSphere datastore
3. Converts VMDK → qcow2 (via qemu-img)
4. Copies qcow2 to NFS share
5. Downloads qcow2 from NFS
6. Uploads to Proxmox storage
7. Creates new VM in Proxmox with migrated disk

### Scenario 3: Proxmox → Libvirt

**Use Case**: Moving VMs from Proxmox to standalone KVM host

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
metadata:
  name: proxmox-to-kvm
spec:
  sourceName: database-server
  sourceNamespace: proxmox-vms
  targetProviderRef:
    name: kvm-host1
    namespace: default
  targetName: database-server-kvm
  targetStorageHint: default         # Libvirt storage pool
  storage:
    type: http
    endpoint: https://fileserver.example.com/exports
    credentialsSecretRef:
      name: fileserver-token
  cleanupPolicy:
    deleteIntermediate: true
    deleteSnapshot: true
    deleteSource: false
  retryPolicy:
    maxAttempts: 5
    backoffDuration: 10m
```

**What Happens**:
1. Creates snapshot of source VM (qcow2 format)
2. Exports disk from Proxmox storage
3. Uploads qcow2 to HTTP server
4. Downloads qcow2 from HTTP server
5. Copies to Libvirt storage pool
6. Creates new VM in Libvirt with migrated disk

## Advanced Configuration

### Custom Disk Format

Specify the target disk format explicitly:

```yaml
spec:
  targetDiskFormat: raw              # Options: qcow2, vmdk, raw
```

### Multiple Disks

For VMs with multiple disks, VirtRigaud will migrate all disks automatically. Each disk is handled separately through the migration pipeline.

### Network Configuration

Ensure target VM networking is configured:

```yaml
spec:
  targetName: migrated-vm
  targetNetworkHints:
    - name: eth0
      network: "VM Network"           # vSphere network
      # or
      bridge: virbr0                  # Libvirt bridge
      # or
      bridge: vmbr0                   # Proxmox bridge
```

### Resource Allocation

Override default resource allocations:

```yaml
spec:
  targetResourceHints:
    cpu: 4
    memory: 8192                      # MB
    disk: 100                         # GB
```

## Monitoring Migration Progress

### Check Migration Status

```bash
kubectl get vmmigration migrate-vm -o yaml
```

Look for `status.phase` and `status.conditions`:

```yaml
status:
  phase: Transferring
  progress: 45
  startTime: "2025-10-16T10:00:00Z"
  conditions:
  - type: Validated
    status: "True"
    lastTransitionTime: "2025-10-16T10:00:05Z"
  - type: Snapshotted
    status: "True"
    lastTransitionTime: "2025-10-16T10:00:30Z"
  - type: Exported
    status: "True"
    lastTransitionTime: "2025-10-16T10:05:00Z"
  - type: Transferring
    status: "True"
    message: "Uploading disk (45% complete, 4.5GB/10GB)"
    lastTransitionTime: "2025-10-16T10:05:05Z"
```

### Watch Migration Progress

```bash
kubectl get vmmigration migrate-vm -w
```

### View Detailed Events

```bash
kubectl describe vmmigration migrate-vm
```

## Migration Phases

VirtRigaud migrations progress through these phases:

1. **Pending**: Migration created, awaiting processing
2. **Validating**: Validating source VM and target provider
3. **Snapshotting**: Creating source VM snapshot
4. **Exporting**: Exporting disk from source provider
5. **Transferring**: Uploading to intermediate storage
6. **Converting**: Converting disk format (if needed)
7. **Importing**: Downloading from intermediate storage
8. **Creating**: Creating VM on target provider
9. **ValidatingTarget**: Validating target VM creation
10. **Ready**: Migration completed successfully
11. **Failed**: Migration failed (check conditions for details)

## Cleanup Policies

Control what gets cleaned up after migration:

```yaml
cleanupPolicy:
  # Delete intermediate storage files (S3/HTTP/NFS)
  deleteIntermediate: true           # Default: true
  
  # Delete source VM snapshot
  deleteSnapshot: true               # Default: true
  
  # Delete source VM after successful migration
  deleteSource: false                # Default: false (DANGEROUS!)
```

**Warning**: Setting `deleteSource: true` will permanently delete the source VM. Only use this if you're certain the migration succeeded and the target VM is working.

## Retry Policies

Configure automatic retry on failure:

```yaml
retryPolicy:
  # Maximum number of retry attempts
  maxAttempts: 3                     # Default: 3
  
  # Backoff duration between retries
  backoffDuration: 5m                # Default: 5m
  
  # Maximum backoff duration
  maxBackoffDuration: 30m            # Default: 30m
```

Exponential backoff is applied: 5m, 10m, 20m (capped at 30m)

## Troubleshooting

### Migration Stuck in Transferring

**Symptom**: Migration stays in "Transferring" phase for a long time

**Solutions**:
1. Check network connectivity between provider and storage
2. Verify storage credentials are correct
3. Check provider logs: `kubectl logs -n virtrigaud-system provider-xxxxx`
4. Check disk size - large disks take time

### Migration Failed with "Invalid Credentials"

**Symptom**: Phase: Failed, Condition: "Storage authentication failed"

**Solutions**:
1. Verify secret exists: `kubectl get secret s3-creds`
2. Check secret contents: `kubectl get secret s3-creds -o yaml`
3. Ensure secret is in same namespace as VMMigration
4. Verify credentials work with storage backend

### Format Conversion Failed

**Symptom**: Phase: Failed, Condition: "Failed to convert disk format"

**Solutions**:
1. Verify qemu-img is installed on provider host
2. Check provider logs for qemu-img errors
3. Ensure sufficient disk space in /tmp
4. Try without format conversion (use native format)

### Target VM Creation Failed

**Symptom**: Phase: Failed, Condition: "Failed to create VM"

**Solutions**:
1. Check target provider is accessible
2. Verify target storage exists (datastore, storage pool)
3. Check resource availability on target
4. Review target provider logs

## Best Practices

### 1. Test Migrations in Non-Production First

Always test migration workflows in a non-production environment before migrating critical VMs.

### 2. Keep Source VMs Running During Migration

Don't delete source VMs until target VMs are validated. Set `deleteSource: false`.

### 3. Use Snapshots for Rollback

VirtRigaud creates snapshots automatically. Keep them until migration is validated.

### 4. Monitor Disk Space

Ensure adequate space:
- Provider hosts: `/tmp` should have space for largest disk
- Storage backend: Should accommodate all disk images
- Target provider: Should have space for new VMs

### 5. Use S3 for Large Migrations

S3 is recommended for:
- Large disk images (>100GB)
- Cross-region migrations
- Migrations with intermittent network

### 6. Network Performance

For best performance:
- Use same region for storage and providers
- Enable compression if bandwidth-limited (future feature)
- Consider direct provider-to-provider migration (future feature)

### 7. Security Considerations

- Store credentials in Kubernetes secrets (never in YAML)
- Use HTTPS for HTTP storage backend
- Enable SSL for S3 connections
- Restrict network access to storage backend

## Performance Optimization

### Disk Format Selection

| Format | Use Case | Performance | Compatibility |
|--------|----------|-------------|---------------|
| **raw** | Best performance | Fastest | Good |
| **qcow2** | Space efficient | Fast | Excellent |
| **vmdk** | vSphere native | Fast | VMware only |

### Storage Backend Selection

| Backend | Best For | Speed | Cost |
|---------|----------|-------|------|
| **S3** | Large migrations, cross-region | Medium | Low |
| **HTTP** | Custom workflows | Medium-Fast | Varies |
| **NFS** | Same datacenter | Fast | Low |

### Tips

1. **Use NFS for same-datacenter migrations** (fastest)
2. **Use S3 for cross-datacenter** (most reliable)
3. **Disable compression** for faster transfers (default)
4. **Use raw format** if performance is critical

## API Reference

See [API Documentation](./api-reference.md) for complete CRD reference.

## Examples

See [examples/](../../examples/migration/) directory for more migration scenarios:

- `libvirt-to-vsphere.yaml` - Complete Libvirt → vSphere example
- `vsphere-to-proxmox.yaml` - vSphere → Proxmox with NFS
- `proxmox-to-libvirt.yaml` - Proxmox → Libvirt with S3
- `multi-disk-migration.yaml` - VM with multiple disks
- `windows-migration.yaml` - Windows VM migration
- `large-vm-migration.yaml` - Large VM (>500GB) migration

## Support

For issues and questions:
- GitHub Issues: https://github.com/projectbeskar/virtrigaud/issues
- Documentation: https://virtrigaud.io/docs
- Community: #virtrigaud on Kubernetes Slack

---

**Next**: Read the [API Reference](./api-reference.md) for detailed CRD specifications.

