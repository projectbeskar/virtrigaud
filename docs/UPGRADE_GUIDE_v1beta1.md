# Virtrigaud API Upgrade Guide: v1alpha1 to v1beta1

This guide provides instructions for upgrading from Virtrigaud v1alpha1 to v1beta1 APIs.

## Overview

Virtrigaud v1beta1 introduces significant enhancements while maintaining backward compatibility through automatic conversion. All CRDs now use v1beta1 as the storage version with comprehensive OpenAPI validation.

## Storage Version Change

**Important**: The storage version has changed from v1alpha1 to v1beta1. Existing resources will be automatically converted when accessed.

- **v1alpha1**: Served=true, Storage=false (deprecated)
- **v1beta1**: Served=true, Storage=true (current)

## Conversion Support

All conversions between v1alpha1 and v1beta1 are **lossless** and **automatic**. The controller-runtime conversion webhooks handle the transformation transparently.

## Field Mapping Highlights

### VirtualMachine

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.powerState` (string) | `spec.powerState` (PowerState enum) | Converted to typed enum. Note: Conversions do not apply defaults - if your resources relied on implicit defaults for powerState, set it explicitly or enable mutating webhooks |
| `spec.networks[].ipPolicy` | Removed | Replaced with explicit IP configuration |
| `spec.networks[].staticIP` | `spec.networks[].ipAddress` | Renamed for clarity |
| `spec.networks[].name` | `spec.networks[].networkRef.name` | Enhanced network reference |
| `spec.disks[].type` (string) | `spec.disks[].type` (DiskType enum) | Converted to typed enum |
| - | `spec.disks[].expandPolicy` | New field (default: "Offline") |
| - | `spec.disks[].storageClass` | New field for storage class specification |
| `spec.userData.cloudInit` | `spec.userData.cloudInit` | Structure preserved |
| - | `spec.userData.ignition` | New field for Ignition support |
| `spec.placement` | `spec.placement` | Enhanced with additional fields |
| - | `spec.placement.host` | New field for host-specific placement |
| - | `spec.placement.resourcePool` | New field for resource pool |
| - | `spec.lifecycle` | New field for VM lifecycle management |
| `status.powerState` (string) | `status.powerState` (PowerState enum) | Converted to typed enum |
| - | `status.phase` | New field for VM phase tracking |
| - | `status.message` | New field for status messages |

### Provider

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.type` (string) | `spec.type` (ProviderType enum) | Converted to typed enum |
| `spec.runtime.tls` | `spec.runtime.service.tls` | Moved under service configuration |
| - | `spec.runtime.imagePullPolicy` | New field (default: "IfNotPresent") |
| - | `spec.runtime.imagePullSecrets` | New field for image pull secrets |
| - | `spec.runtime.livenessProbe` | New field for liveness probe |
| - | `spec.runtime.readinessProbe` | New field for readiness probe |
| - | `spec.healthCheck` | New field for health check configuration |
| - | `spec.connectionPooling` | New field for connection pooling |
| `spec.defaults` | `spec.defaults` | Enhanced with additional fields |
| - | `spec.defaults.resourcePool` | New field for default resource pool |
| - | `spec.defaults.network` | New field for default network |
| `status.runtime.phase` (string) | `status.runtime.phase` (ProviderRuntimePhase enum) | Converted to typed enum |
| - | `status.capabilities` | New field listing provider capabilities |
| - | `status.version` | New field for provider version |
| - | `status.connectedVMs` | New field for VM count |
| - | `status.resourceUsage` | New field for resource usage statistics |

### VMClass

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.memoryMiB` (int32) | `spec.memory` (Quantity) | Converted to Kubernetes resource quantity |
| `spec.firmware` (string) | `spec.firmware` (FirmwareType enum) | Converted to typed enum |
| `spec.diskDefaults.sizeGiB` (int32) | `spec.diskDefaults.size` (Quantity) | Converted to Kubernetes resource quantity |
| `spec.diskDefaults.type` (string) | `spec.diskDefaults.type` (DiskType enum) | Converted to typed enum |
| `spec.guestToolsPolicy` (string) | `spec.guestToolsPolicy` (GuestToolsPolicy enum) | Converted to typed enum |
| - | `spec.diskDefaults.iops` | New field for IOPS limits |
| - | `spec.diskDefaults.storageClass` | New field for storage class |
| - | `spec.resourceLimits` | New field for resource limits |
| - | `spec.performanceProfile` | New field for performance settings |
| - | `spec.securityProfile` | New field for security settings |
| - | `status.usedByVMs` | New field tracking VM usage |
| - | `status.supportedProviders` | New field listing supported providers |
| - | `status.validationResults` | New field for validation results |

### VMImage

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.vsphere` | `spec.source.vsphere` | Moved under source configuration |
| `spec.libvirt` | `spec.source.libvirt` | Moved under source configuration |
| - | `spec.source.http` | New field for HTTP/HTTPS sources |
| - | `spec.source.registry` | New field for container registry sources |
| - | `spec.source.dataVolume` | New field for DataVolume sources |
| - | `spec.metadata` | New field for image metadata |
| - | `spec.distribution` | New field for OS distribution info |
| `spec.prepare.onMissing` (string) | `spec.prepare.onMissing` (ImageMissingAction enum) | Converted to typed enum |
| - | `spec.prepare.optimization` | New field for image optimization |
| `status.phase` (string) | `status.phase` (ImagePhase enum) | Enhanced with more phases |
| - | `status.size` | New field for image size |
| - | `status.checksum` | New field for actual checksum |
| - | `status.format` | New field for actual format |
| - | `status.providerStatus` | New field for provider-specific status |

### VMNetworkAttachment

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.vsphere` | `spec.network.vsphere` | Moved under network configuration |
| `spec.libvirt` | `spec.network.libvirt` | Moved under network configuration |
| `spec.ipPolicy` (string) | `spec.ipAllocation.type` (IPAllocationType enum) | Enhanced IP allocation |
| `spec.macAddress` | Removed | Replaced with enhanced MAC configuration |
| - | `spec.network.type` | New field for network type |
| - | `spec.network.mtu` | New field for MTU configuration |
| - | `spec.ipAllocation` | New field for comprehensive IP management |
| - | `spec.security` | New field for network security settings |
| - | `spec.qos` | New field for Quality of Service |
| - | `spec.metadata` | New field for network metadata |
| - | `status.connectedVMs` | New field for connected VM count |
| - | `status.ipAllocations` | New field for IP allocation tracking |
| - | `status.providerStatus` | New field for provider-specific status |

### VMSnapshot

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.nameHint` | `spec.snapshotConfig.name` | Moved under snapshot configuration |
| `spec.memory` | `spec.snapshotConfig.includeMemory` | Renamed for clarity |
| `spec.description` | `spec.snapshotConfig.description` | Moved under snapshot configuration |
| - | `spec.snapshotConfig.type` | New field for snapshot type |
| - | `spec.snapshotConfig.compression` | New field for compression |
| - | `spec.snapshotConfig.encryption` | New field for encryption |
| - | `spec.snapshotConfig.consistencyLevel` | New field for consistency level |
| - | `spec.schedule` | New field for automated scheduling |
| - | `spec.metadata` | New field for snapshot metadata |
| `status.phase` (string) | `status.phase` (SnapshotPhase enum) | Enhanced with more phases |
| - | `status.virtualSize` | New field for virtual size |
| - | `status.progress` | New field for progress tracking |
| - | `status.providerStatus` | New field for provider-specific status |
| - | `status.children` | New field for snapshot tree |
| - | `status.parent` | New field for parent snapshot |
| - | `status.expiryTime` | New field for expiry time |

### VMClone

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.sourceRef` | `spec.source.vmRef` | Moved under source configuration |
| `spec.linked` | `spec.options.type` | Replaced with clone type enum |
| `spec.powerOn` | `spec.options.powerOn` | Moved under options |
| - | `spec.source.snapshotRef` | New field for snapshot-based cloning |
| - | `spec.source.templateRef` | New field for template-based cloning |
| - | `spec.source.imageRef` | New field for image-based cloning |
| - | `spec.options.type` | New field for clone type (Full/Linked/Instant) |
| - | `spec.options.timeout` | New field for operation timeout |
| - | `spec.options.retryPolicy` | New field for retry configuration |
| - | `spec.options.storage` | New field for storage-specific options |
| - | `spec.options.performance` | New field for performance options |
| - | `spec.customization.sysprep` | New field for Windows sysprep |
| - | `spec.customization.guestCommands` | New field for guest commands |
| - | `spec.customization.certificates` | New field for certificate installation |
| - | `spec.metadata` | New field for clone metadata |
| `status.linkedClone` | `status.actualCloneType` | Enhanced clone type tracking |
| - | `status.progress` | New field for detailed progress |
| - | `status.customizationStatus` | New field for customization status |

### VMSet

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.updateStrategy.rollingUpdate` | Enhanced | Added PodManagementPolicy |
| - | `spec.persistentVolumeClaimRetentionPolicy` | New field for PVC retention |
| - | `spec.ordinals` | New field for ordinal configuration |
| - | `spec.serviceName` | New field for service governance |
| - | `spec.volumeClaimTemplates` | New field for PVC templates |
| - | `status.updateStatus` | New field for detailed update status |
| - | `status.vmStatus` | New field for per-VM status |

### VMPlacementPolicy

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|--------|
| `spec.hard` | `spec.hard` | Enhanced with additional constraints |
| `spec.soft` | `spec.soft` | Enhanced with additional constraints |
| - | `spec.resourceConstraints` | New field for resource-based constraints |
| - | `spec.securityConstraints` | New field for security-based constraints |
| - | `spec.priority` | New field for policy priority |
| - | `spec.weight` | New field for policy weight |
| - | `status.placementStats` | New field for placement statistics |
| - | `status.conflictingPolicies` | New field for conflict tracking |

## Migration Steps

### Step 1: Backup Current Resources

```bash
# Backup all virtrigaud resources
kubectl get providers,virtualmachines,vmclasses,vmimages,vmnetworkattachments,vmsnapshots,vmclones,vmsets,vmplacementpolicies -o yaml > virtrigaud-backup.yaml
```

### Step 2: Upgrade Virtrigaud

```bash
# Upgrade using Helm
helm upgrade virtrigaud ./charts/virtrigaud --version v0.2.0

# Or using kubectl
kubectl apply -f https://github.com/projectbeskar/virtrigaud/releases/download/v0.2.0/install.yaml
```

### Step 3: Verify Conversion

```bash
# Check that resources are accessible in both versions
kubectl get virtualmachines.v1alpha1.infra.virtrigaud.io
kubectl get virtualmachines.v1beta1.infra.virtrigaud.io

# Verify specific resource conversion
kubectl get vm demo-web-01 -o yaml
```

### Step 4: Update Client Code (Optional)

If you have custom controllers or client code, update to use v1beta1:

```go
// Old
import "github.com/projectbeskar/virtrigaud/api/v1alpha1"

// New
import "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
```

### Step 5: Validate New Features

Test new v1beta1 features like:

- Enhanced network configuration
- Lifecycle management
- Advanced placement policies
- Improved validation

## Validation Script

Use this script to validate your conversion:

```bash
#!/bin/bash
# validate-conversion.sh

echo "Validating Virtrigaud v1beta1 conversion..."

# Check CRD versions
for crd in providers virtualmachines vmclasses vmimages vmnetworkattachments vmsnapshots vmclones vmsets vmplacementpolicies; do
    echo "Checking ${crd}..."
    kubectl get crd ${crd}.infra.virtrigaud.io -o jsonpath='{.spec.versions[*].name}' | grep -q v1beta1
    if [ $? -eq 0 ]; then
        echo "✓ ${crd} supports v1beta1"
    else
        echo "✗ ${crd} missing v1beta1 support"
        exit 1
    fi
done

# Test conversion
kubectl get virtualmachines.v1alpha1.infra.virtrigaud.io -o yaml > /tmp/alpha.yaml 2>/dev/null || true
kubectl get virtualmachines.v1beta1.infra.virtrigaud.io -o yaml > /tmp/beta.yaml 2>/dev/null || true

if [ -s /tmp/alpha.yaml ] && [ -s /tmp/beta.yaml ]; then
    echo "✓ Conversion working - resources accessible in both versions"
else
    echo "⚠ Limited resources to test conversion"
fi

echo "Validation complete!"
```

## Deprecation Timeline

- **v0.1.x**: v1alpha1 served and stored
- **v0.2.x**: v1beta1 served and stored, v1alpha1 served (deprecated)
- **v0.3.x**: v1beta1 only, v1alpha1 removed

## Common Issues

### Issue: Conversion Webhook Not Available

**Symptoms**: `conversion webhook for infra.virtrigaud.io/v1alpha1, Kind=VirtualMachine is not available`

**Solution**:
1. Ensure virtrigaud controller is running
2. Check webhook certificates are valid
3. Verify network connectivity to webhook service

### Issue: Validation Errors on New Fields

**Symptoms**: Validation errors on v1beta1-specific fields

**Solution**:
1. Update to use valid enum values
2. Ensure required fields are set
3. Check field constraints in CRD OpenAPI schema

### Issue: Performance Impact During Conversion

**Symptoms**: Slow response times when accessing v1alpha1 resources

**Solution**:
1. This is expected during conversion
2. Migrate to v1beta1 clients for better performance
3. Consider upgrading resources to v1beta1 storage

## Support

For questions or issues during migration:

- Check the [troubleshooting guide](TROUBLESHOOTING.md)
- Review [API reference documentation](api-reference/)
- Open an issue on [GitHub](https://github.com/projectbeskar/virtrigaud/issues)

## Rollback Procedure

If you need to rollback to v1alpha1:

```bash
# Rollback Helm release
helm rollback virtrigaud

# Or reapply v1alpha1 CRDs
kubectl apply -f https://github.com/projectbeskar/virtrigaud/releases/download/v0.1.x/crds.yaml
```

**Note**: Rollback is only possible if no v1beta1-specific features were used.
