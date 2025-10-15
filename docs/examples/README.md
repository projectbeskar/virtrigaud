# VirtRigaud Examples

This directory contains comprehensive examples for VirtRigaud v0.2.3+, showcasing all features and capabilities.

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

## v0.2.3 Feature Summary

### 🔧 VM Reconfiguration (vSphere, Libvirt, Proxmox)
```yaml
# Online resource changes (vSphere, Proxmox)
# Offline changes (Libvirt - requires restart)
spec:
  vmClassRef: medium  # Change from small to medium
  powerState: "On"
```

### 📋 Async Task Tracking (vSphere, Proxmox)
```yaml
# Automatic tracking of long-running operations
# Real-time progress and error reporting
```

### 🖥️ Console Access (vSphere, Libvirt)
```yaml
# Web console URLs automatically generated
status:
  consoleURL: "https://vcenter.example.com/ui/app/vm..."  # vSphere
  # or
  consoleURL: "vnc://libvirt-host:5900"  # Libvirt VNC
```

### 🌐 Guest Agent Integration (Proxmox)
```yaml
# Accurate IP detection via QEMU guest agent
status:
  ipAddresses:
    - 192.168.1.100
    - fd00::1234:5678:9abc:def0
```

### 📦 VM Cloning (vSphere)
```yaml
# Full and linked clones with automatic snapshot handling
spec:
  vmImageRef: source-vm
  cloneType: linked  # or "full"
```

### 🔄 Previous Features (v0.2.1)

- **Graceful Shutdown**: OffGraceful power state with VMware Tools
- **Hardware Version Management**: vSphere hardware version control
- **Proper Disk Sizing**: Correct disk allocation across providers
- **Enhanced Lifecycle Management**: postStart/preStop hooks

## Usage Patterns

### Testing v0.2.3 Features

1. **Test VM reconfiguration**:
   ```bash
   # Change VM class to trigger reconfiguration
   kubectl patch virtualmachine my-vm --type='merge' \
     -p='{"spec":{"vmClassRef":"medium"}}'
   
   # Watch the reconfiguration process
   kubectl get vm my-vm -w
   ```

2. **Access VM console**:
   ```bash
   # Get console URL from VM status
   kubectl get vm my-vm -o jsonpath='{.status.consoleURL}'
   
   # For VNC (Libvirt): Use any VNC client
   vncviewer $(kubectl get vm my-vm -o jsonpath='{.status.consoleURL}' | sed 's/vnc:\/\///')
   ```

3. **Monitor async tasks** (vSphere, Proxmox):
   ```bash
   # Watch task progress in provider logs
   kubectl logs -f deployment/virtrigaud-provider-vsphere
   ```

4. **Verify guest agent** (Proxmox):
   ```bash
   # Check IP addresses from guest agent
   kubectl get vm my-vm -o jsonpath='{.status.ipAddresses}'
   ```

5. **Test VM cloning** (vSphere):
   ```bash
   # Create a clone of existing VM
   kubectl apply -f - <<EOF
   apiVersion: infra.virtrigaud.io/v1beta1
   kind: VirtualMachine
   metadata:
     name: web-server-clone
   spec:
     vmClassRef: small
     vmImageRef: web-server-01
     cloneType: linked
   EOF
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

- **v0.2.3+**: All examples with v0.2.3 features (Reconfigure, Clone, TaskStatus, ConsoleURL, Guest Agent)
- **v0.2.2**: Nested virtualization, TPM support, snapshot management
- **v0.2.1**: Graceful shutdown, hardware version, disk sizing fixes
- **v0.2.0**: Initial production-ready providers
- **v0.1.x**: Legacy examples in git history

## Need Help?

- 📖 **Documentation**: [../README.md](../README.md)
- 🚀 **Quick Start**: [../getting-started/quickstart.md](../getting-started/quickstart.md)
- 🔧 **CLI Tools**: [../CLI.md](../CLI.md)
- 📋 **Upgrade Guide**: [../UPGRADE.md](../UPGRADE.md)
- 🏗️ **Contributing**: [../../CONTRIBUTING.md](../../CONTRIBUTING.md)

---

**Pro Tip**: Start with `v021-feature-showcase.yaml` to see all v0.2.1 capabilities in action! 🚀
