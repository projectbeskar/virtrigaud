# API Version Migration Examples

This directory contains examples for migrating from v1alpha1 to v1beta1 API versions.

## Structure

- `alpha/` - Contains v1alpha1 examples (for reference during migration)
- `beta/` - Contains v1beta1 examples (showing new structure and features)

## Migration Guide

For detailed migration instructions, see [UPGRADE_GUIDE_v1beta1.md](../../docs/UPGRADE_GUIDE_v1beta1.md).

## Quick Comparison

The main differences between v1alpha1 and v1beta1:

### VirtualMachine
- **Networks**: `ipPolicy: dhcp` → `ipAddress: ""` with explicit `networkRef`
- **Disks**: `sizeGiB: 50` → `size: 50Gi` (resource.Quantity)
- **Tags**: Moved from array to `metadata.labels` structure

### Provider  
- **Connection**: `endpoint` and `credentialSecretRef` → structured `connection` object
- **TLS**: Added explicit `tls.insecure` configuration

### VMClass
- **Memory**: `memoryMiB: 4096` → `memory: 4Gi` (resource.Quantity) 
- **Disk**: `diskDefaults.sizeGiB` → `diskDefaults.size` (resource.Quantity)

### VMImage
- **Source**: Provider-specific fields moved under `source` object
- **Metadata**: Added structured metadata for description, distribution, version

### VMNetworkAttachment
- **IP Policy**: `ipPolicy: dhcp` → `ipAllocation.type: dhcp`
- **Structure**: Provider-specific fields moved under `provider` object

## Validation

You can validate your migrated examples with:

```bash
kubectl apply --dry-run=server -f examples/
```
