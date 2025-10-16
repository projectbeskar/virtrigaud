# VM Migration Examples

This directory contains practical examples for migrating VMs between different hypervisor platforms using VirtRigaud.

## Examples

### Basic Cross-Platform Migrations

1. **[libvirt-to-vsphere.yaml](./libvirt-to-vsphere.yaml)**
   - Migrate Linux VM from Libvirt/KVM to VMware vSphere
   - Uses S3 for intermediate storage
   - Demonstrates qcow2 → VMDK conversion
   - Production-ready example with retry policy

2. **[vsphere-to-proxmox.yaml](./vsphere-to-proxmox.yaml)**
   - Migrate Windows Server from vSphere to Proxmox VE
   - Uses NFS for intermediate storage
   - Demonstrates VMDK → qcow2 conversion
   - Includes resource and network configuration

3. **[proxmox-to-libvirt.yaml](./proxmox-to-libvirt.yaml)**
   - Migrate database server from Proxmox to Libvirt/KVM
   - Uses HTTP server for intermediate storage
   - Best practices for database migration
   - Conservative cleanup policy

## Quick Start

### 1. Choose an Example

Select the example that matches your migration scenario:

```bash
# Libvirt to vSphere
kubectl apply -f libvirt-to-vsphere.yaml

# vSphere to Proxmox
kubectl apply -f vsphere-to-proxmox.yaml

# Proxmox to Libvirt
kubectl apply -f proxmox-to-libvirt.yaml
```

### 2. Customize for Your Environment

Edit the YAML file to match your environment:

```yaml
spec:
  sourceName: your-vm-name              # Your source VM
  sourceNamespace: your-namespace
  
  targetProviderRef:
    name: your-provider                 # Your target provider
  targetName: your-new-vm-name
  
  storage:
    type: s3                            # or http, nfs
    bucket: your-bucket
    credentialsSecretRef:
      name: your-credentials
```

### 3. Update Credentials

Create appropriate credentials secret:

```bash
# For S3
kubectl create secret generic aws-s3-creds \
  --from-literal=accessKey=YOUR_ACCESS_KEY \
  --from-literal=secretKey=YOUR_SECRET_KEY

# For HTTP
kubectl create secret generic http-creds \
  --from-literal=token=Bearer_YOUR_TOKEN

# NFS typically doesn't need credentials
```

### 4. Monitor Migration

```bash
# Watch migration status
kubectl get vmmigration your-migration-name -w

# View detailed status
kubectl describe vmmigration your-migration-name

# Check events
kubectl get events --field-selector involvedObject.name=your-migration-name
```

## Example Scenarios

### Development → Production

Promote a VM from dev to production environment:

```yaml
sourceName: app-server-dev
sourceNamespace: development
targetName: app-server-prod
targetNamespace: production
```

### Cross-Region Migration

Migrate VM to different region/datacenter:

```yaml
storage:
  type: s3
  bucket: migrations-us-west
  region: us-west-2
```

### Test Migration

Test migration without deleting source:

```yaml
cleanupPolicy:
  deleteSource: false               # Keep source
targetName: test-migration-vm       # Different name
```

## Storage Backend Selection

| Backend | Best For | Speed | Cost |
|---------|----------|-------|------|
| **S3** | Cross-region, large VMs | Medium | Low |
| **HTTP** | Custom workflows | Fast | Varies |
| **NFS** | Same datacenter | Fastest | Low |

### When to Use Each

- **S3**: Different data centers, cloud storage, large migrations
- **HTTP**: When you have existing file storage infrastructure
- **NFS**: Same data center, fastest performance, local network

## Best Practices

### Before Migration

1. ✅ Test migration in non-production first
2. ✅ Take application-level backup
3. ✅ Document current VM configuration
4. ✅ Plan downtime window (if needed)
5. ✅ Verify network connectivity to storage
6. ✅ Check disk space on all systems

### During Migration

1. 👁️ Monitor migration progress
2. 👁️ Watch for errors or warnings
3. 👁️ Check provider logs if issues occur
4. 👁️ Verify network/storage performance

### After Migration

1. ✅ Validate target VM boots correctly
2. ✅ Test application functionality
3. ✅ Verify data integrity
4. ✅ Update DNS/load balancers
5. ✅ Monitor for 24-48 hours
6. ✅ Document any issues/changes

### Safety

- **Always keep source VM** until target is validated
- **Never set deleteSource: true** on first migration
- **Test rollback procedures** before prod migration
- **Have backup plan** in case migration fails

## Troubleshooting

### Migration Stuck

```bash
# Check phase
kubectl get vmmigration my-migration -o jsonpath='{.status.phase}'

# Check conditions
kubectl get vmmigration my-migration -o jsonpath='{.status.conditions}'

# Check provider logs
kubectl logs -n virtrigaud-system deployment/provider-libvirt
```

### Storage Errors

```bash
# Verify credentials exist
kubectl get secret aws-s3-creds

# Test storage access manually
aws s3 ls s3://my-bucket  # For S3
curl -H "Authorization: Bearer TOKEN" https://storage/  # For HTTP
ls /mnt/migrations  # For NFS
```

### Format Conversion Failed

```bash
# Check qemu-img is installed
# On provider host:
qemu-img --version

# Check disk space
df -h /tmp
```

## Additional Resources

- [User Guide](../../docs/migration/user-guide.md) - Complete migration guide
- [API Reference](../../docs/migration/api-reference.md) - Full API documentation
- [Implementation Status](../../MIGRATION_IMPLEMENTATION_STATUS.md) - Technical details

## Support

For issues and questions:
- GitHub Issues: https://github.com/projectbeskar/virtrigaud/issues
- Documentation: https://virtrigaud.io/docs

## Contributing

Have a useful migration example? Please contribute!

1. Create example YAML file
2. Add clear comments explaining the scenario
3. Include prerequisites and expected behavior
4. Test the example
5. Submit a pull request

