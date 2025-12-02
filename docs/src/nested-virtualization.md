# Nested Virtualization Support

This document describes how to enable and configure nested virtualization in VirtRigaud virtual machines across different hypervisor providers.

## Overview

Nested virtualization allows virtual machines to run hypervisors and create their own virtual machines. This is useful for:

- Development and testing of virtualization software
- Running container orchestration platforms like Kubernetes
- Creating nested lab environments
- Educational purposes for learning virtualization concepts

VirtRigaud supports nested virtualization through the `PerformanceProfile` configuration in VMClass resources.

## Prerequisites

### vSphere Provider
- ESXi 6.0 or later
- VM hardware version 9 or later (recommended: version 14+)
- ESXi host must have VT-x/AMD-V enabled in BIOS
- Sufficient CPU and memory resources on the ESXi host

### LibVirt Provider
- QEMU/KVM hypervisor
- Host CPU with VT-x (Intel) or AMD-V (AMD) support
- Nested virtualization enabled in host kernel modules
- libvirt 1.2.13 or later

### Proxmox Provider
- Proxmox VE 6.0 or later
- Host CPU with nested virtualization support
- Nested virtualization enabled in Proxmox configuration

## Enabling Nested Virtualization

Nested virtualization is configured at the VMClass level using the `PerformanceProfile` section:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: nested-vm-class
  namespace: virtrigaud-system
spec:
  cpu: 4
  memory: 8Gi
  firmware: UEFI  # Recommended for modern features
  
  # Enable nested virtualization
  performanceProfile:
    nestedVirtualization: true
    # Optional: Enable additional features
    virtualizationBasedSecurity: true
    cpuHotAddEnabled: true
    memoryHotAddEnabled: true
  
  # Optional: Security features that work well with nested virtualization
  securityProfile:
    secureBoot: false  # May interfere with some nested hypervisors
    tpmEnabled: false  # Optional, depending on nested OS requirements
    vtdEnabled: true   # Enable VT-d/AMD-Vi for better performance
  
  diskDefaults:
    type: thin
    size: 100Gi  # Larger disk for nested VMs
```

## Complete Example

Here's a complete example showing how to create a VM with nested virtualization support:

```yaml
---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: hypervisor-class
  namespace: default
spec:
  cpu: 8
  memory: 16Gi
  firmware: UEFI
  
  performanceProfile:
    nestedVirtualization: true
    virtualizationBasedSecurity: false  # May conflict with nested hypervisors
    cpuHotAddEnabled: true
    memoryHotAddEnabled: true
    latencySensitivity: low  # Better performance for nested VMs
    hyperThreadingPolicy: prefer
  
  securityProfile:
    secureBoot: false  # Disable for compatibility
    tpmEnabled: false
    vtdEnabled: true   # Enable for better I/O performance
  
  resourceLimits:
    cpuReservation: 4000  # Reserve 4GHz for nested VMs
    memoryReservation: 8Gi
  
  diskDefaults:
    type: thin
    size: 200Gi
    storageClass: fast-ssd

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-server-22-04
  namespace: default
spec:
  source:
    libvirt:
      url: "https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img"
      checksum: "sha256:de5e632e17b8965f2baf4ea6d2b824788e154d9a65df4fd419ec4019898e15cd"

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: nested-hypervisor
  namespace: default
spec:
  providerRef:
    name: my-provider
  classRef:
    name: hypervisor-class
  imageRef:
    name: ubuntu-server-22-04
  
  userData:
    cloudInit:
      inline: |
        #cloud-config
        hostname: nested-hypervisor
        users:
          - name: ubuntu
            sudo: ALL=(ALL) NOPASSWD:ALL
            ssh_authorized_keys:
              - ssh-rsa AAAAB3NzaC1yc2E... # Your SSH key
        
        packages:
          - qemu-kvm
          - libvirt-daemon-system
          - libvirt-clients
          - bridge-utils
          - virt-manager
        
        runcmd:
          # Enable nested virtualization verification
          - echo "Checking nested virtualization support..."
          - cat /proc/cpuinfo | grep -E "(vmx|svm)"
          - ls -la /dev/kvm
          
          # Configure libvirt
          - systemctl enable libvirtd
          - systemctl start libvirtd
          - usermod -aG libvirt ubuntu
          
          # Verify nested KVM support
          - modprobe kvm_intel nested=1 || modprobe kvm_amd nested=1
          - echo "Nested virtualization setup complete"
  
  powerState: On
```

## Provider-Specific Configuration

### vSphere Provider

For vSphere, nested virtualization is enabled using the following VM configuration:

- `vhv.enable = TRUE` - Enables hardware-assisted virtualization
- `vhv.allowNestedPageTables = TRUE` - Improves nested VM performance
- Hardware version 14+ recommended for best compatibility

Additional considerations:
- Use UEFI firmware for modern guest operating systems
- Ensure sufficient CPU and memory allocation
- Consider enabling VT-d for better I/O performance

### LibVirt Provider

For LibVirt/KVM, nested virtualization requires:

- Host kernel modules: `kvm_intel nested=1` or `kvm_amd nested=1`
- CPU features: `vmx` (Intel) or `svm` (AMD) passed through to guest
- QEMU machine type: `q35` recommended for modern features

The LibVirt provider automatically configures:
```xml
<cpu mode='host-model' check='partial'>
  <feature policy='require' name='vmx'/>  <!-- Intel -->
  <feature policy='require' name='svm'/>  <!-- AMD -->
</cpu>
```

### Proxmox Provider

For Proxmox VE, nested virtualization is configured through:

- CPU type: `host` or `kvm64` with nested features
- Enable nested virtualization in VM CPU configuration
- Ensure host has nested virtualization enabled

## Verification

After creating a VM with nested virtualization enabled, verify the setup:

### On Linux Guests

```bash
# Check for virtualization extensions
grep -E "(vmx|svm)" /proc/cpuinfo

# Verify KVM device availability
ls -la /dev/kvm

# Check nested virtualization status
cat /sys/module/kvm_intel/parameters/nested  # Intel
cat /sys/module/kvm_amd/parameters/nested    # AMD

# Test with a simple nested VM
virt-host-validate
```

### On Windows Guests

```powershell
# Check Hyper-V compatibility
systeminfo | findstr /i hyper

# Verify virtualization extensions
Get-ComputerInfo | Select-Object HyperV*
```

## Performance Considerations

### CPU Allocation
- Allocate sufficient CPU cores (minimum 4, recommended 8+)
- Consider CPU reservation for consistent performance
- Enable CPU hot-add for flexibility

### Memory Configuration
- Allocate generous memory (minimum 8GB, recommended 16GB+)
- Consider memory reservation for nested VMs
- Enable memory hot-add for dynamic scaling

### Storage
- Use fast storage (SSD/NVMe) for better nested VM performance
- Allocate sufficient disk space for multiple nested VMs
- Consider thin provisioning for efficient space usage

### Network
- Configure appropriate network topology
- Consider SR-IOV for high-performance networking
- Plan IP address allocation for nested environments

## Troubleshooting

### Common Issues

1. **Nested virtualization not working**
   - Verify host CPU supports VT-x/AMD-V
   - Check host BIOS settings
   - Ensure hypervisor nested virtualization is enabled

2. **Poor performance in nested VMs**
   - Increase CPU and memory allocation
   - Enable CPU/memory reservations
   - Use faster storage
   - Verify nested page tables are enabled

3. **Guest OS doesn't detect virtualization extensions**
   - Check VM hardware version (vSphere)
   - Verify CPU feature passthrough (LibVirt)
   - Ensure proper CPU type configuration (Proxmox)

### Debugging Commands

```bash
# Check virtualization support on host
lscpu | grep Virtualization

# Verify KVM nested support
cat /sys/module/kvm_*/parameters/nested

# Check VM CPU features (inside guest)
lscpu | grep -E "(vmx|svm|Virtualization)"

# Test nested VM creation
virt-install --name test-nested --memory 1024 --vcpus 1 --disk size=10 --cdrom /path/to/iso
```

## Security Considerations

### Isolation
- Nested VMs add additional attack surface
- Consider network isolation for nested environments
- Implement proper access controls

### Resource Limits
- Set appropriate resource limits to prevent resource exhaustion
- Monitor nested VM resource usage
- Implement quotas for nested environments

### Updates and Patches
- Keep host hypervisor updated
- Maintain guest hypervisor software
- Apply security patches to nested VMs

## Best Practices

1. **Planning**
   - Design nested architecture carefully
   - Plan resource allocation in advance
   - Consider network topology requirements

2. **Configuration**
   - Use UEFI firmware for modern features
   - Enable VT-d/AMD-Vi for better performance
   - Configure appropriate CPU and memory reservations

3. **Monitoring**
   - Monitor resource usage at all levels
   - Set up alerting for resource exhaustion
   - Track performance metrics

4. **Maintenance**
   - Regular backup of nested environments
   - Plan for hypervisor updates
   - Test disaster recovery procedures

## Limitations

### vSphere Provider
- Requires ESXi 6.0+ and hardware version 9+
- Performance overhead of 10-20% typical
- Some advanced features may not be available in nested VMs

### LibVirt Provider
- Requires host kernel support
- Performance depends on host CPU features
- Limited to x86_64 architecture

### Proxmox Provider
- Requires Proxmox VE 6.0+
- Performance overhead varies by workload
- Some clustering features may not work in nested environments

## Support Matrix

| Provider | Min Version | Nested Support | Performance | Security Features |
|----------|-------------|----------------|-------------|-------------------|
| vSphere  | ESXi 6.0    | Full           | Good        | TPM, Secure Boot  |
| LibVirt  | 1.2.13      | Full           | Good        | TPM, Secure Boot  |
| Proxmox  | PVE 6.0     | Planned        | Good        | Limited           |

For more information, see the provider-specific documentation in the `docs/providers/` directory.
