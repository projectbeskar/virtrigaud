# vSphere Hardware Version Management

This document describes how to configure and upgrade VM hardware compatibility versions in VMware vSphere environments using virtrigaud.

## Overview

VMware vSphere virtual machines have a hardware compatibility version (also called virtual hardware version) that determines which features and capabilities are available to the VM. Higher hardware versions provide access to newer features but require compatible ESXi hosts.

**Note: Hardware version management is specific to VMware vSphere and is not available for other providers (LibVirt, Proxmox, etc.).**

## Hardware Version Numbers

Common hardware versions and their corresponding VMware products:

| Hardware Version | vSphere/ESXi Version | Key Features |
|------------------|---------------------|--------------|
| 10 | ESXi 5.5 | Legacy baseline |
| 11 | ESXi 6.0 | Enhanced graphics, larger VM memory |
| 13 | ESXi 6.5 | Enhanced security, more CPU/memory |
| 14 | ESXi 6.7 | Persistent memory, enhanced security |
| 15 | ESXi 6.7 U2 | Enhanced graphics, more vCPU |
| 17 | ESXi 7.0 | TPM 2.0, enhanced security |
| 18 | ESXi 7.0 U1 | Enhanced networking |
| 19 | ESXi 7.0 U2 | Precision time protocol |
| 20 | ESXi 7.0 U3 | Enhanced graphics, more memory |
| 21 | ESXi 8.0 | Latest features, DPU support |

## Setting Hardware Version During VM Creation

Configure the hardware version in the VMClass using the `extraConfig` field:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: modern-vm-class
  namespace: virtrigaud-system
spec:
  cpu: 4
  memory: 8Gi
  firmware: UEFI
  
  # vSphere-specific hardware version configuration
  extraConfig:
    vsphere.hardwareVersion: "21"  # Use latest hardware version
  
  diskDefaults:
    type: thin
    sizeGiB: 50
---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: modern-vm
  namespace: default
spec:
  providerRef:
    name: vsphere-provider
    namespace: virtrigaud-system
  
  classRef:
    name: modern-vm-class  # Uses hardware version 21
    namespace: virtrigaud-system
  
  imageRef:
    name: ubuntu-22-04
    namespace: virtrigaud-system
```

## Upgrading Hardware Version for Existing VMs

You can upgrade the hardware version of existing VMs using the dedicated hardware upgrade API:

### Using kubectl with Raw gRPC

```bash
# First, ensure the VM is powered off
kubectl patch vm my-vm --type='merge' -p='{"spec":{"powerState":"Off"}}'

# Wait for VM to be powered off, then upgrade hardware version
# Note: This requires direct access to the provider gRPC endpoint
# A kubectl plugin or controller extension would be needed for this operation
```

### Programmatic Upgrade (Example Go Code)

```go
package main

import (
    "context"
    "fmt"
    "log"
    
    providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
    "google.golang.org/grpc"
)

func upgradeVMHardwareVersion(vmID string, targetVersion int32) error {
    // Connect to vSphere provider
    conn, err := grpc.Dial("vsphere-provider:9090", grpc.WithInsecure())
    if err != nil {
        return fmt.Errorf("failed to connect: %w", err)
    }
    defer conn.Close()
    
    client := providerv1.NewProviderClient(conn)
    
    // Upgrade hardware version
    req := &providerv1.HardwareUpgradeRequest{
        Id:            vmID,
        TargetVersion: targetVersion,
    }
    
    resp, err := client.HardwareUpgrade(context.Background(), req)
    if err != nil {
        return fmt.Errorf("hardware upgrade failed: %w", err)
    }
    
    log.Printf("Hardware upgrade completed: %+v", resp)
    return nil
}
```

## Requirements and Limitations

### Prerequisites

1. **VM Must Be Powered Off**: Hardware version upgrades require the VM to be completely powered off
2. **ESXi Host Compatibility**: Target hardware version must be supported by the ESXi host
3. **VMware Tools**: For best results, ensure VMware Tools is installed and up-to-date
4. **Backup Recommended**: Take a snapshot before upgrading hardware version

### Limitations

1. **One-Way Operation**: Hardware version upgrades cannot be downgraded
2. **vSphere Only**: This feature is not available for LibVirt, Proxmox, or other providers
3. **Host Requirements**: Upgrading to newer versions may prevent VM from running on older ESXi hosts
4. **Compatibility**: Some older guest operating systems may not support newer hardware versions

## Best Practices

### Choosing Hardware Version

1. **Match ESXi Version**: Use the hardware version that matches your ESXi environment
2. **Conservative Approach**: Don't always use the latest version unless you need specific features
3. **Test First**: Test hardware version upgrades in development before production

### Upgrade Process

1. **Plan Maintenance Window**: VMs must be powered off during upgrade
2. **Backup First**: Always take a snapshot before upgrading
3. **Batch Operations**: Group VMs by hardware requirements for efficient upgrades
4. **Verify Compatibility**: Ensure all ESXi hosts in your cluster support the target version

### Example VMClass Configurations

#### Legacy Environment (ESXi 6.5)
```yaml
extraConfig:
  vsphere.hardwareVersion: "13"
```

#### Modern Environment (ESXi 7.0)
```yaml
extraConfig:
  vsphere.hardwareVersion: "17"
```

#### Latest Features (ESXi 8.0)
```yaml
extraConfig:
  vsphere.hardwareVersion: "21"
```

## Troubleshooting

### Common Issues

1. **VM Not Powered Off**
   ```
   Error: VM must be powered off for hardware upgrade, current state: poweredOn
   ```
   **Solution**: Power off the VM first using `powerState: Off`

2. **Unsupported Hardware Version**
   ```
   Error: target version vmx-21 is not supported by ESXi host
   ```
   **Solution**: Check ESXi host compatibility and use a supported version

3. **Version Not Newer**
   ```
   Error: target version vmx-15 is not newer than current version vmx-17
   ```
   **Solution**: Hardware versions can only be upgraded, not downgraded

### Validation

After upgrading, verify the hardware version:

```bash
# Check VM configuration in vSphere
kubectl get vm my-vm -o jsonpath='{.status.provider}'
```

## Integration Examples

### Complete VM Lifecycle with Hardware Version

```yaml
# 1. Create VMClass with specific hardware version
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: production-vm-class
spec:
  cpu: 8
  memory: 16Gi
  firmware: UEFI
  extraConfig:
    vsphere.hardwareVersion: "19"  # ESXi 7.0 U2 compatible

---
# 2. Create VM using the class
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: production-vm
spec:
  powerState: On
  providerRef:
    name: vsphere-provider
    namespace: virtrigaud-system
  classRef:
    name: production-vm-class
    namespace: virtrigaud-system
  imageRef:
    name: ubuntu-22-04
    namespace: virtrigaud-system

---
# 3. Update to newer hardware version (requires separate upgrade operation)
# This would typically be done through a controller or manual gRPC call
# after powering off the VM
```

This vSphere-specific feature provides fine-grained control over VM hardware capabilities while maintaining compatibility with your ESXi infrastructure.
