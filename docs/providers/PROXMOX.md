# Proxmox VE Provider

This document describes the Proxmox VE provider for VirtRigaud, including configuration, usage examples, and feature details.

## Overview

The Proxmox provider enables VirtRigaud to manage virtual machines on Proxmox VE clusters. It supports:

- ✅ Full VM lifecycle management (create, delete, power operations)
- ✅ Template-based provisioning with full and linked clones
- ✅ Multiple storage types (local-lvm, NFS, Ceph, etc.)
- ✅ Network configuration with bridges and VLANs
- ✅ Cloud-init integration
- ✅ Snapshot management (create, delete, revert)
- ✅ VM reconfiguration (CPU, memory, disk)
- ✅ Multi-node cluster support

## Configuration

### Provider Setup

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pve-credentials
  namespace: default
type: Opaque
stringData:
  # Option 1: API Token (Recommended)
  token_id: "root@pam!mytoken"
  token_secret: "your-token-secret-here"
  
  # Option 2: Username/Password
  # username: "root@pam"
  # password: "your-password"
---
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: proxmox-prod
  namespace: default
spec:
  type: proxmox
  endpoint: https://pve.example.com:8006
  insecureSkipVerify: false  # Set to true for self-signed certs
  credentialSecretRef:
    name: pve-credentials
  runtime:
    mode: Remote
    image: "ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.2"
    service:
      port: 9090
```

### Creating API Tokens in Proxmox

1. Log into Proxmox web UI
2. Go to **Datacenter → Permissions → API Tokens**
3. Click **Add** and create a token for your user
4. **Important**: Uncheck "Privilege Separation" to inherit user permissions
5. Copy the Token ID and Secret

## VMImage Configuration

### Using Template by ID

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-24-04
  namespace: default
spec:
  source:
    proxmox:
      templateID: 9000           # VMID of template
      storage: "vms"             # Storage for cloned VM
      node: "pve01"              # Node where template exists
      fullClone: true            # Full clone vs linked clone
      format: "qcow2"
  distribution:
    name: "ubuntu"
    version: "24.04"
    architecture: "amd64"
```

### Using Template by Name

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: debian-12
  namespace: default
spec:
  source:
    proxmox:
      templateName: "debian-12-template"
      storage: "local-lvm"
      fullClone: false           # Linked clone for faster provisioning
```

### Template Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `templateID` | int | No* | VMID of the template (e.g., 9000) |
| `templateName` | string | No* | Name of the template |
| `storage` | string | No | Storage pool for cloned VM (default: provider default) |
| `node` | string | No | Node where template exists (default: auto-select) |
| `fullClone` | bool | No | `true` for full clone, `false` for linked (default: `true`) |
| `format` | string | No | Disk format: `qcow2`, `raw`, `vmdk` (default: `qcow2`) |

*Either `templateID` or `templateName` must be specified.

## Network Configuration

### Basic Bridge Configuration

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: lan-network
  namespace: default
spec:
  network:
    proxmox:
      bridge: "vmbr0"            # Linux bridge name
      model: "virtio"            # Network card model
  ipAllocation:
    type: "DHCP"
```

### VLAN-Tagged Network

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: vlan-network
  namespace: default
spec:
  network:
    proxmox:
      bridge: "vmbr1"
      model: "virtio"
      vlanTag: 100               # VLAN ID
      firewall: true             # Enable Proxmox firewall
      rateLimit: 100             # Bandwidth limit in MB/s
  ipAllocation:
    type: "DHCP"
```

### Network Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `bridge` | string | Yes | Linux bridge name (e.g., `vmbr0`, `vmbr1`) |
| `model` | string | No | NIC model: `virtio`, `e1000`, `rtl8139`, `vmxnet3` (default: `virtio`) |
| `vlanTag` | int | No | VLAN tag (1-4094) |
| `firewall` | bool | No | Enable Proxmox firewall (default: `false`) |
| `rateLimit` | int | No | Bandwidth limit in MB/s |
| `mtu` | int | No | MTU size (68-65520, default: 1500) |

## Complete Example

See [`examples/proxmox/complete-vm.yaml`](../../examples/proxmox/complete-vm.yaml) for a full working example.

## Storage Types

Proxmox supports various storage types:

- **local-lvm**: Local LVM-thin storage (fast, node-local)
- **local**: Local directory storage
- **nfs**: Network File System shares
- **ceph**: Ceph RBD storage (cluster-wide)
- **zfs**: ZFS storage pools
- **iscsi**: iSCSI targets

Specify the storage in the `VMImage` spec:

```yaml
spec:
  source:
    proxmox:
      templateID: 9000
      storage: "ceph-storage"  # Use Ceph storage
```

## Cloud-Init Support

Proxmox VMs support cloud-init for guest OS initialization:

```yaml
spec:
  userData:
    cloudInit:
      inline: |
        #cloud-config
        hostname: my-vm
        users:
          - name: admin
            sudo: ALL=(ALL) NOPASSWD:ALL
            ssh_authorized_keys:
              - ssh-ed25519 AAAA...
        packages:
          - nginx
        runcmd:
          - systemctl enable nginx
```

## Snapshots

Create and manage VM snapshots:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSnapshot
metadata:
  name: my-vm-snapshot
  namespace: default
spec:
  virtualMachineRef:
    name: my-vm
  snapshotName: "backup-20250113"
  description: "Before system upgrade"
  includeMemory: false
```

## Best Practices

### Template Preparation

1. **Create a base template**:
   ```bash
   # Download cloud image
   wget https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img
   
   # Create VM
   qm create 9000 --name ubuntu-24-04-template --memory 2048 --net0 virtio,bridge=vmbr0
   
   # Import disk
   qm importdisk 9000 ubuntu-24.04-server-cloudimg-amd64.img vms
   
   # Configure VM
   qm set 9000 --scsihw virtio-scsi-pci --scsi0 vms:vm-9000-disk-0
   qm set 9000 --ide2 vms:cloudinit
   qm set 9000 --boot c --bootdisk scsi0
   qm set 9000 --serial0 socket --vga serial0
   
   # Convert to template
   qm template 9000
   ```

2. **Use full clones for production**: Safer, independent VMs
3. **Use linked clones for testing**: Faster provisioning, less space

### Node Selection

For multi-node clusters, you can specify the node in the `VMImage` spec. If not specified, VirtRigaud will auto-select an available node.

### Storage Recommendations

- **Development**: `local-lvm` (fast, simple)
- **Production**: `ceph` or `nfs` (cluster-wide, HA-capable)
- **Performance**: Local NVMe with ZFS

## Troubleshooting

### VM Creation Fails

1. **Check template exists**:
   ```bash
   qm list | grep 9000
   ```

2. **Verify storage**:
   ```bash
   pvesm status
   ```

3. **Check provider logs**:
   ```bash
   kubectl logs -n default deployment/virtrigaud-provider-proxmox-prod
   ```

### Network Issues

1. **Verify bridge exists**:
   ```bash
   ip link show vmbr0
   ```

2. **Check VLAN configuration** on the Proxmox host

### Authentication Issues

- Ensure API token has proper permissions
- For token authentication, disable "Privilege Separation"
- Check endpoint URL is correct (include port 8006)

## Feature Matrix

| Feature | Status | Notes |
|---------|--------|-------|
| VM Create/Delete | ✅ | Full support |
| Power Operations | ✅ | On, Off, Reboot, Graceful Shutdown |
| Snapshots | ✅ | Create, Delete, Revert |
| Full Clones | ✅ | Independent VM copies |
| Linked Clones | ✅ | Fast, space-efficient |
| Cloud-Init | ✅ | User data, network config |
| Multiple Networks | ✅ | Up to 32 NICs |
| VLAN Tagging | ✅ | 802.1Q support |
| Reconfigure | ✅ | CPU, Memory, Disk resize |
| HA Integration | ✅ | Works with Proxmox HA |
| Multi-Node | ✅ | Cluster-aware |

## API Reference

For complete API documentation, see:
- [VMImage CRD](../crds/vmimage.md)
- [VMNetworkAttachment CRD](../crds/vmnetworkattachment.md)
- [VirtualMachine CRD](../crds/virtualmachine.md)

