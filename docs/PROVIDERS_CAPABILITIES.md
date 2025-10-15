# Provider Capabilities Matrix

This document provides a comprehensive overview of VirtRigaud provider capabilities as of v0.2.3.

## Overview

VirtRigaud supports multiple hypervisor platforms through a provider architecture. Each provider implements the core VirtRigaud API while supporting platform-specific features and capabilities.

## Core Provider Interface

All providers implement these core operations:

- **Validate**: Test provider connectivity and credentials
- **Create**: Create new virtual machines
- **Delete**: Remove virtual machines and cleanup resources
- **Power**: Control VM power state (On/Off/Reboot)
- **Describe**: Query VM state and properties
- **GetCapabilities**: Report provider-specific capabilities

## Provider Status

| Provider | Status | Implementation | Maturity |
|----------|--------|---------------|----------|
| **vSphere** | ✅ Production Ready | govmomi-based | Stable |
| **Libvirt/KVM** | ✅ Production Ready | virsh-based | Stable |
| **Proxmox VE** | ✅ Production Ready | REST API-based | Beta |
| **Mock** | ✅ Complete | In-memory simulation | Testing |

## Comprehensive Capability Matrix

### Core Operations

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **VM Create** | ✅ | ✅ | ✅ | ✅ | All providers support VM creation |
| **VM Delete** | ✅ | ✅ | ✅ | ✅ | With resource cleanup |
| **Power On/Off** | ✅ | ✅ | ✅ | ✅ | Basic power management |
| **Reboot** | ✅ | ✅ | ✅ | ✅ | Graceful and forced restart |
| **Suspend** | ✅ | ❌ | ✅ | ✅ | Memory state preservation |
| **Describe** | ✅ | ✅ | ✅ | ✅ | VM state and properties |
| **Reconfigure** | ✅ | ⚠️ | ✅ | ✅ | CPU/Memory/Disk changes (Libvirt requires restart) |
| **TaskStatus** | ✅ | N/A | ✅ | ✅ | Async operation tracking |
| **ConsoleURL** | ✅ | ✅ | ⚠️ | ✅ | Remote console access (Proxmox planned) |

### Resource Management

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **CPU Configuration** | ✅ | ✅ | ✅ | ✅ | Cores, sockets, threading |
| **Memory Allocation** | ✅ | ✅ | ✅ | ✅ | Static memory sizing |
| **Hot CPU Add** | ✅ | ❌ | ✅ | ✅ | Online CPU expansion |
| **Hot Memory Add** | ✅ | ❌ | ✅ | ✅ | Online memory expansion |
| **Resource Reservations** | ✅ | ❌ | ✅ | ✅ | Guaranteed resources |
| **Resource Limits** | ✅ | ❌ | ✅ | ✅ | Resource capping |

### Storage Operations

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **Disk Creation** | ✅ | ✅ | ✅ | ✅ | Virtual disk provisioning |
| **Disk Expansion** | ✅ | ✅ | ✅ | ✅ | Online disk growth |
| **Multiple Disks** | ✅ | ✅ | ✅ | ✅ | Multi-disk VMs |
| **Thin Provisioning** | ✅ | ✅ | ✅ | ✅ | Space-efficient disks |
| **Thick Provisioning** | ✅ | ✅ | ✅ | ✅ | Pre-allocated storage |
| **Storage Policies** | ✅ | ❌ | ✅ | ✅ | Policy-based placement |
| **Storage Pools** | ✅ | ✅ | ✅ | ✅ | Organized storage management |

### Network Configuration

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **Basic Networking** | ✅ | ✅ | ✅ | ✅ | Single network interface |
| **Multiple NICs** | ✅ | ✅ | ✅ | ✅ | Multi-interface VMs |
| **VLAN Support** | ✅ | ✅ | ✅ | ✅ | Network segmentation |
| **Static IP** | ✅ | ✅ | ✅ | ✅ | Fixed IP assignment |
| **DHCP** | ✅ | ✅ | ✅ | ✅ | Dynamic IP assignment |
| **Bridge Networks** | ❌ | ✅ | ✅ | ✅ | Direct host bridging |
| **Distributed Switches** | ✅ | ❌ | ❌ | ✅ | Advanced vSphere networking |

### VM Lifecycle

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **Template Deployment** | ✅ | ✅ | ✅ | ✅ | Deploy from templates |
| **Clone Operations** | ✅ Complete | ✅ | ✅ | ✅ | Full VM duplication with snapshot support |
| **Linked Clones** | ✅ | ❌ | ✅ | ✅ | COW-based clones with automatic snapshot creation |
| **Full Clones** | ✅ | ✅ | ✅ | ✅ | Independent copies |
| **VM Reconfiguration** | ✅ Complete | ⚠️ Restart Required | ✅ | ✅ | Online resource modification |

### Snapshot Operations

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **Create Snapshots** | ✅ | ✅ | ✅ | ✅ | Point-in-time captures |
| **Delete Snapshots** | ✅ | ✅ | ✅ | ✅ | Snapshot cleanup |
| **Revert Snapshots** | ✅ | ✅ | ✅ | ✅ | Restore VM state |
| **Memory Snapshots** | ✅ | ❌ | ✅ | ✅ | Include RAM state |
| **Quiesced Snapshots** | ✅ | ❌ | ✅ | ✅ | Consistent filesystem |
| **Snapshot Trees** | ✅ | ✅ | ✅ | ✅ | Hierarchical snapshots |

### Image Management

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **OVA/OVF Import** | ✅ | ❌ | ✅ | ✅ | Standard VM formats |
| **Cloud Image Download** | ❌ | ✅ | ✅ | ✅ | Remote image fetch |
| **Content Libraries** | ✅ | ❌ | ❌ | ✅ | Centralized image management |
| **Image Conversion** | ❌ | ✅ | ✅ | ✅ | Format transformation |
| **Image Caching** | ✅ | ✅ | ✅ | ✅ | Performance optimization |

### Guest Operating System

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **Cloud-Init** | ✅ | ✅ | ✅ | ✅ | Guest initialization |
| **Guest Tools** | ✅ | ✅ | ✅ | ✅ | Enhanced guest integration |
| **Guest Agent** | ✅ | ✅ | ✅ | ✅ | Runtime guest communication |
| **Guest Customization** | ✅ | ✅ | ✅ | ✅ | OS-specific customization |
| **Guest Monitoring** | ✅ | ✅ | ✅ | ✅ | Resource usage tracking |

### Advanced Features

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **High Availability** | ✅ | ❌ | ✅ | ✅ | Automatic failover |
| **DRS/Load Balancing** | ✅ | ❌ | ❌ | ✅ | Resource optimization |
| **Fault Tolerance** | ✅ | ❌ | ❌ | ✅ | Zero-downtime protection |
| **vMotion/Migration** | ✅ | ❌ | ✅ | ✅ | Live VM migration |
| **Resource Pools** | ✅ | ❌ | ✅ | ✅ | Hierarchical resource mgmt |
| **Affinity Rules** | ✅ | ❌ | ✅ | ✅ | VM placement policies |

### Monitoring & Observability

| Capability | vSphere | Libvirt | Proxmox | Mock | Notes |
|------------|---------|---------|---------|------|-------|
| **Performance Metrics** | ✅ | ✅ | ✅ | ✅ | CPU, memory, disk, network |
| **Event Logging** | ✅ | ✅ | ✅ | ✅ | Operation audit trail |
| **Health Checks** | ✅ | ✅ | ✅ | ✅ | VM and guest health |
| **Alerting** | ✅ | ❌ | ✅ | ✅ | Threshold-based notifications |
| **Historical Data** | ✅ | ❌ | ✅ | ✅ | Performance history |
| **Console URL Generation** | ✅ | ✅ | ⚠️ | ✅ | Web/VNC console access (Proxmox planned) |
| **Guest Agent Integration** | ✅ | ✅ | ✅ Complete | ✅ | IP detection and guest info |

## Provider-Specific Features

### vSphere Exclusive

- **vCenter Integration**: Full vCenter Server and ESXi support
- **Content Library**: Centralized template and ISO management
- **Distributed Resource Scheduler (DRS)**: Automatic load balancing
- **vMotion**: Live migration between hosts
- **High Availability (HA)**: Automatic VM restart on host failure
- **Fault Tolerance**: Zero-downtime VM protection
- **Storage vMotion**: Live storage migration
- **vSAN Integration**: Hyper-converged storage
- **NSX Integration**: Software-defined networking
- **Hot Reconfiguration**: Online CPU/memory/disk changes with hot-add support
- **TaskStatus Tracking**: Real-time async operation monitoring via govmomi
- **Clone Operations**: Full and linked clones with automatic snapshot handling
- **Web Console URLs**: Direct vSphere web client console access

### Libvirt/KVM Exclusive

- **Virsh Integration**: Command-line management
- **QEMU Guest Agent**: Advanced guest OS integration
- **KVM Optimization**: Native Linux virtualization
- **Bridge Networking**: Direct host network bridging
- **Storage Pool Flexibility**: Multiple storage backend support
- **Cloud Image Support**: Direct cloud image deployment
- **Host Device Passthrough**: Hardware device assignment
- **Reconfiguration Support**: CPU/memory/disk changes via virsh (restart required)
- **VNC Console Access**: Direct VNC console URL generation for remote viewers

### Proxmox VE Exclusive

- **Web UI Integration**: Built-in management interface
- **Container Support**: LXC container management
- **Backup Integration**: Built-in backup and restore
- **Cluster Management**: Multi-node cluster support
- **ZFS Integration**: Advanced filesystem features
- **Ceph Integration**: Distributed storage
- **Guest Agent IP Detection**: Accurate IP address extraction via QEMU guest agent
- **Hot-plug Reconfiguration**: Online CPU/memory/disk modifications
- **Complete CRD Integration**: Full Kubernetes custom resource support

### Mock Provider Features

- **Testing Scenarios**: Configurable failure modes
- **Performance Simulation**: Controllable operation delays
- **Sample Data**: Pre-populated demonstration VMs
- **Development Support**: Full API coverage for testing

## Supported Disk Types

| Provider | Disk Formats | Notes |
|----------|-------------|--------|
| **vSphere** | thin, thick, eagerZeroedThick | vSphere native formats |
| **Libvirt** | qcow2, raw, vmdk | QEMU-supported formats |
| **Proxmox** | qcow2, raw, vmdk | Proxmox storage formats |
| **Mock** | thin, thick, raw, qcow2 | Simulated formats |

## Supported Network Types

| Provider | Network Types | Notes |
|----------|--------------|--------|
| **vSphere** | distributed, standard, vlan | vSphere networking |
| **Libvirt** | virtio, e1000, rtl8139 | QEMU network adapters |
| **Proxmox** | virtio, e1000, rtl8139 | Proxmox network models |
| **Mock** | bridge, nat, distributed | Simulated network types |

## Provider Images

All provider images are available from the GitHub Container Registry:

- **vSphere**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.3`
- **Libvirt**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.2.3`
- **Proxmox**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.3`
- **Mock**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.2.3`

## Choosing a Provider

### Use vSphere When:
- You have existing VMware infrastructure
- You need enterprise features (HA, DRS, vMotion)
- You require advanced networking (NSX, distributed switches)
- You need centralized management (vCenter)

### Use Libvirt/KVM When:
- You want open-source virtualization
- You're running on Linux hosts
- You need cost-effective virtualization
- You want direct host integration

### Use Proxmox VE When:
- You need both VMs and containers
- You want integrated backup solutions
- You need cluster management
- You want web-based management

### Use Mock Provider When:
- You're developing or testing VirtRigaud
- You need to simulate VM operations
- You're creating demos or training materials
- You're testing VirtRigaud without hypervisors

## Performance Considerations

### vSphere
- **Best for**: Large-scale enterprise deployments
- **Scalability**: Hundreds to thousands of VMs
- **Overhead**: Higher due to feature richness
- **Resource Efficiency**: Excellent with DRS

### Libvirt/KVM
- **Best for**: Linux-based deployments
- **Scalability**: Moderate to large deployments
- **Overhead**: Low, near-native performance
- **Resource Efficiency**: Good with proper tuning

### Proxmox VE
- **Best for**: SMB and mixed workloads
- **Scalability**: Small to medium deployments
- **Overhead**: Moderate
- **Resource Efficiency**: Good with clustering

## Future Roadmap

### Planned Enhancements

#### vSphere
- vSphere 8.0 support
- Enhanced NSX integration
- GPU passthrough support
- vSAN policy automation

#### Libvirt
- Live migration support
- SR-IOV networking
- NUMA topology optimization
- Enhanced performance monitoring

#### Proxmox
- HA configuration
- Storage replication
- Advanced networking
- Performance optimizations

## Support Matrix

| Feature Category | vSphere | Libvirt | Proxmox | Mock |
|-----------------|---------|---------|---------|------|
| **Production Ready** | ✅ | ✅ | ✅ Beta | ✅ Testing |
| **Documentation** | Complete | Complete | Complete | Complete |
| **Community Support** | Active | Active | Growing | N/A |
| **Enterprise Support** | Available | Available | Available | N/A |

## Version History

- **v0.2.3**: Provider feature parity - Reconfigure, Clone, TaskStatus, ConsoleURL
- **v0.2.2**: Nested virtualization, TPM support, comprehensive snapshot management
- **v0.2.1**: Critical fixes, documentation updates, VMClass disk settings
- **v0.2.0**: Production-ready vSphere and Libvirt providers
- **v0.1.0**: Initial provider framework and mock implementation

---

*This document reflects VirtRigaud v0.2.3 capabilities. For the latest updates, see the [VirtRigaud documentation](https://projectbeskar.github.io/virtrigaud/).*
