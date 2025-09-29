# VirtRigaud Examples

This directory contains comprehensive examples for VirtRigaud v0.2.1+, showcasing all features and capabilities.

## Quick Start Examples

### Basic Examples
- **[complete-example.yaml](complete-example.yaml)** - Complete end-to-end example with v0.2.1 features
- **[vm-ubuntu-small.yaml](vm-ubuntu-small.yaml)** - Simple Ubuntu VM with graceful shutdown
- **[vmclass-small.yaml](vmclass-small.yaml)** - Basic VMClass with hardware version support

### Provider Examples
- **[provider-vsphere.yaml](provider-vsphere.yaml)** - Basic vSphere provider configuration
- **[provider-libvirt.yaml](provider-libvirt.yaml)** - Basic LibVirt provider configuration

### Resource Examples
- **[vmimage-ubuntu.yaml](vmimage-ubuntu.yaml)** - VM image configuration
- **[vmnetwork-app.yaml](vmnetwork-app.yaml)** - Network attachment configuration

## v0.2.1 Feature Examples

### New in v0.2.1
- **[v021-feature-showcase.yaml](v021-feature-showcase.yaml)** - **🌟 COMPREHENSIVE DEMO** - All v0.2.1 features in one example
- **[graceful-shutdown-examples.yaml](graceful-shutdown-examples.yaml)** - OffGraceful shutdown configurations
- **[vsphere-hardware-versions.yaml](vsphere-hardware-versions.yaml)** - Hardware version management
- **[disk-sizing-examples.yaml](disk-sizing-examples.yaml)** - Disk size configuration tests

### Advanced Provider Examples
- **[vsphere-advanced-example.yaml](vsphere-advanced-example.yaml)** - Advanced vSphere with v0.2.1 features
- **[libvirt-advanced-example.yaml](libvirt-advanced-example.yaml)** - Advanced LibVirt configuration
- **[proxmox-complete-example.yaml](proxmox-complete-example.yaml)** - Complete Proxmox setup

### Multi-Provider Examples
- **[multi-provider-example.yaml](multi-provider-example.yaml)** - Multiple providers in one cluster
- **[libvirt-complete-example.yaml](libvirt-complete-example.yaml)** - Complete LibVirt deployment

## v0.2.1 Feature Summary

### 🔄 Enhanced Power Management
```yaml
# Graceful shutdown with VMware Tools
powerState: "OffGraceful"

lifecycle:
  gracefulShutdownTimeout: "90s"  # Configurable timeout
  preStop:
    exec:
      command: ["/bin/bash", "-c", "systemctl stop nginx && sync"]
```

### ⚙️ Hardware Version Management (vSphere)
```yaml
extraConfig:
  "vsphere.hardwareVersion": "21"  # ESXi 8.0+ features
```

### 💾 Proper Disk Sizing
```yaml
diskDefaults:
  type: thin
  size: "100Gi"  # Now properly respected across all providers

disks:
- name: data
  sizeGiB: 500  # Additional disks with correct sizing
```

### 🚀 Enhanced Lifecycle Management
```yaml
lifecycle:
  postStart:
    exec:
      command: ["/bin/bash", "-c", "echo 'VM started' >> /var/log/startup.log"]
  preStop:
    exec:
      command: ["/bin/bash", "-c", "systemctl stop services && sync"]
```

## Usage Patterns

### Testing v0.2.1 Features

1. **Start with the feature showcase**:
   ```bash
   kubectl apply -f v021-feature-showcase.yaml
   ```

2. **Test graceful shutdown**:
   ```bash
   kubectl patch virtualmachine v021-feature-demo --type='merge' \
     -p='{"spec":{"powerState":"OffGraceful"}}'
   ```

3. **Monitor graceful shutdown**:
   ```bash
   kubectl logs -f deployment/virtrigaud-provider-[provider-name]
   ```

4. **Verify hardware version** (vSphere):
   ```bash
   kubectl exec -it vm-name -- vmware-toolbox-cmd stat raw text session
   ```

5. **Check disk sizes**:
   ```bash
   kubectl exec -it vm-name -- df -h
   ```

### Development Workflow

1. **Choose base example** based on your use case
2. **Customize** provider, class, and VM specifications
3. **Test locally** with your infrastructure
4. **Iterate** based on your requirements

### Production Deployment

1. **Start with complete-example.yaml**
2. **Add security configurations** from security/ subdirectory
3. **Configure secrets** from secrets/ subdirectory  
4. **Apply advanced patterns** from advanced/ subdirectory

## File Organization

```
docs/examples/
├── README.md                          # This file
├── complete-example.yaml             # Complete setup guide
├── v021-feature-showcase.yaml        # 🌟 v0.2.1 comprehensive demo
├── vm-ubuntu-small.yaml             # Simple VM example
├── vmclass-small.yaml               # Basic VMClass
├── provider-*.yaml                  # Provider configurations
├── graceful-shutdown-examples.yaml  # OffGraceful demos
├── vsphere-hardware-versions.yaml   # Hardware version examples
├── disk-sizing-examples.yaml        # Disk sizing tests
├── advanced/                        # Complex scenarios
├── secrets/                         # Secret management
└── security/                        # Security configurations
```

## Version Compatibility

- **v0.2.1+**: All examples with v0.2.1 features (OffGraceful, hardware version, disk sizing)
- **v0.2.0**: Examples without v0.2.1-specific features still work
- **v0.1.x**: Legacy examples in git history

## Need Help?

- 📖 **Documentation**: [../README.md](../README.md)
- 🚀 **Quick Start**: [../getting-started/quickstart.md](../getting-started/quickstart.md)
- 🔧 **CLI Tools**: [../CLI.md](../CLI.md)
- 📋 **Upgrade Guide**: [../UPGRADE.md](../UPGRADE.md)
- 🏗️ **Contributing**: [../../CONTRIBUTING.md](../../CONTRIBUTING.md)

---

**Pro Tip**: Start with `v021-feature-showcase.yaml` to see all v0.2.1 capabilities in action! 🚀
