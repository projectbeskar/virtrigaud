# LibVirt/KVM Provider

The LibVirt provider enables VirtRigaud to manage virtual machines on KVM/QEMU hypervisors using the LibVirt API. This provider runs as a dedicated pod that communicates with LibVirt daemons locally or remotely, making it ideal for development, on-premises deployments, and cloud environments.

## Overview

This provider implements the VirtRigaud provider interface to manage VM lifecycle operations on LibVirt/KVM:

- **Create**: Create VMs from existing images/templates with cloud-init support
- **Delete**: Remove VMs and associated storage volumes  
- **Power**: Start, stop, and reboot virtual machines
- **Describe**: Query VM state, resource usage, and network information
- **Reconfigure**: Modify CPU/memory resources (requires VM restart)
- **Clone**: Create new VMs based on existing VM configurations
- **Snapshot**: Create, delete, and revert VM snapshots
- **ImagePrepare**: Import and prepare VM templates from various sources

## Prerequisites

The LibVirt provider connects to a LibVirt daemon (libvirtd) which can run locally or remotely. This makes it flexible for both development and production environments.

### Connection Options:
- **Local LibVirt**: Connects to local libvirtd via `qemu:///system` (ideal for development)
- **Remote LibVirt**: Connects to remote libvirtd over SSH/TLS (production)
- **Container LibVirt**: Works with containerized libvirt or KubeVirt

### Requirements:
- **LibVirt daemon** (libvirtd) running locally or accessible remotely
- **KVM/QEMU** hypervisor support (hardware virtualization recommended)
- **Storage pools** configured for VM disk storage  
- **Network bridges** or interfaces for VM networking
- **Appropriate permissions** for VM management operations

### Development Setup:
For local development, you can:
- **Linux**: Install `libvirt-daemon-system` and `qemu-kvm` packages
- **macOS/Windows**: Use remote LibVirt or nested virtualization
- **Testing**: The provider can connect to local libvirtd without complex infrastructure

## Authentication & Connection

The LibVirt provider supports multiple connection methods:

### Local LibVirt Connection

For connecting to a LibVirt daemon on the same host as the provider pod:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-local
  namespace: default
spec:
  type: libvirt
  endpoint: "qemu:///system"  # Local system connection
  credentialSecretRef:
    name: libvirt-local-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-libvirt:latest"
    service:
      port: 9090
```

**Note**: When using local connections, ensure the provider pod has appropriate permissions to access the LibVirt socket.

### Remote Connection with SSH

For remote LibVirt over SSH:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-remote
  namespace: default
spec:
  type: libvirt
  endpoint: "qemu+ssh://user@libvirt-host/system"
  credentialSecretRef:
    name: libvirt-ssh-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-libvirt:latest"
    service:
      port: 9090
```

Create SSH credentials secret:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: libvirt-ssh-credentials
  namespace: default
type: Opaque
stringData:
  username: "libvirt-user"
  # For key-based auth (recommended):
  tls.key: |
    -----BEGIN PRIVATE KEY-----
    # Your SSH private key here
    -----END PRIVATE KEY-----
  # For password auth (less secure):
  password: "your-password"
```

### Remote Connection with TLS

For remote LibVirt over TLS:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-tls
  namespace: default
spec:
  type: libvirt
  endpoint: "qemu+tls://libvirt-host:16514/system"
  credentialSecretRef:
    name: libvirt-tls-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-libvirt:latest"
    service:
      port: 9090
```

Create TLS credentials secret:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: libvirt-tls-credentials
  namespace: default
type: kubernetes.io/tls
data:
  tls.crt: # Base64 encoded client certificate
  tls.key: # Base64 encoded client private key
  ca.crt:  # Base64 encoded CA certificate
```

## Configuration

### Connection URIs

The LibVirt provider supports standard LibVirt connection URIs:

| URI Format | Description | Use Case |
|------------|-------------|----------|
| `qemu:///system` | Local system connection | Development, single-host |
| `qemu+ssh://user@host/system` | SSH connection | Remote access with SSH |
| `qemu+tls://host:16514/system` | TLS connection | Secure remote access |
| `qemu+tcp://host:16509/system` | TCP connection | Insecure remote (testing only) |

**⚠️ Note**: All LibVirt URI schemes are now supported in the CRD validation pattern.

### Deployment Configuration

#### Using Helm Values

```yaml
# values.yaml
providers:
  libvirt:
    enabled: true
    endpoint: "qemu:///system"  # Adjust for your environment
    # For remote connections:
    # endpoint: "qemu+ssh://user@libvirt-host/system"
    credentialSecretRef:
      name: libvirt-credentials  # Optional for local connections
```

#### Development Configuration

```yaml
# For local development with LibVirt
providers:
  libvirt:
    enabled: true
    endpoint: "qemu:///system"
    runtime:
      # Mount host libvirt socket (for local access)
      volumes:
      - name: libvirt-sock
        hostPath:
          path: /var/run/libvirt/libvirt-sock
      volumeMounts:
      - name: libvirt-sock
        mountPath: /var/run/libvirt/libvirt-sock
```

#### Production Configuration

```yaml
# For production with remote LibVirt
apiVersion: v1
kind: Secret
metadata:
  name: libvirt-credentials
  namespace: virtrigaud-system
type: Opaque
stringData:
  username: "virtrigaud-service"
  tls.crt: |
    -----BEGIN CERTIFICATE-----
    # Client certificate for TLS authentication
    -----END CERTIFICATE-----
  tls.key: |
    -----BEGIN PRIVATE KEY-----
    # Client private key
    -----END PRIVATE KEY-----
  ca.crt: |
    -----BEGIN CERTIFICATE-----
    # CA certificate
    -----END CERTIFICATE-----

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-production
  namespace: virtrigaud-system
spec:
  type: libvirt
  endpoint: "qemu+tls://libvirt.example.com:16514/system"
  credentialSecretRef:
    name: libvirt-credentials
```

## Storage Configuration

### Storage Pools

LibVirt requires storage pools for VM disks. Common configurations:

```bash
# Create directory-based storage pool
virsh pool-define-as default dir --target /var/lib/libvirt/images
virsh pool-build default
virsh pool-start default
virsh pool-autostart default

# Create LVM-based storage pool (performance)
virsh pool-define-as lvm-pool logical --source-name vg-libvirt --target /dev/vg-libvirt
virsh pool-start lvm-pool
virsh pool-autostart lvm-pool
```

### VMClass Storage Specification

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: standard
spec:
  cpus: 2
  memory: "4Gi"
  # LibVirt-specific storage settings
  spec:
    storage:
      pool: "default"        # Storage pool name
      format: "qcow2"        # Disk format (qcow2, raw)
      cache: "writethrough"  # Cache mode
      io: "threads"          # I/O mode
```

## Network Configuration

### Network Setup

Configure LibVirt networks for VM connectivity:

```bash
# Create NAT network (default)
virsh net-define /usr/share/libvirt/networks/default.xml
virsh net-start default
virsh net-autostart default

# Create bridge network (for external access)
cat > /tmp/bridge-network.xml << EOF
<network>
  <name>br0</name>
  <forward mode='bridge'/>
  <bridge name='br0'/>
</network>
EOF
virsh net-define /tmp/bridge-network.xml
virsh net-start br0
```

### Network Bridge Mapping

| Network Name | LibVirt Network | Use Case |
|--------------|-----------------|----------|
| `default`, `nat` | default | NAT networking |
| `bridge`, `br0` | br0 | Bridged networking |
| `isolated` | isolated | Host-only networking |

### VM Network Configuration

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server
spec:
  providerRef:
    name: libvirt-local
  networks:
    # Use default NAT network
    - name: default
    # Use bridged network for external access
    - name: bridge
      bridge: br0
      mac: "52:54:00:12:34:56"  # Optional MAC address
```

## VM Configuration

### VMClass Specification

Define hardware resources and LibVirt-specific settings:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: development
spec:
  cpus: 2
  memory: "4Gi"
  # LibVirt-specific configuration
  spec:
    machine: "pc-i440fx-2.12"  # Machine type
    cpu:
      mode: "host-model"       # CPU mode (host-model, host-passthrough)
      topology:
        sockets: 1
        cores: 2
        threads: 1
    features:
      acpi: true
      apic: true
      pae: true
    clock:
      offset: "utc"
      timers:
        rtc: "catchup"
        pit: "delay"
        hpet: false
```

### VMImage Specification

Reference existing disk images or templates:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22-04
spec:
  source:
    # Path to existing image in storage pool
    disk: "/var/lib/libvirt/images/ubuntu-22.04-base.qcow2"
    # Or reference by pool and volume
    # pool: "default"
    # volume: "ubuntu-22.04-base"
  format: "qcow2"
  
  # Cloud-init preparation
  cloudInit:
    enabled: true
    userDataTemplate: |
      #cloud-config
      hostname: {{"{{ .Name }}"}}
      users:
        - name: ubuntu
          sudo: ALL=(ALL) NOPASSWD:ALL
          ssh_authorized_keys:
            - {{"{{ .SSHPublicKey }}"}}
```

### Complete VM Example

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: dev-workstation
spec:
  providerRef:
    name: libvirt-local
  classRef:
    name: development
  imageRef:
    name: ubuntu-22-04
  powerState: On
  
  # Disk configuration
  disks:
    - name: root
      size: "50Gi"
      storageClass: "fast-ssd"  # Maps to LibVirt storage pool
  
  # Network configuration  
  networks:
    - name: default  # NAT network for internet
    - name: bridge   # Bridge for LAN access
      staticIP:
        address: "192.168.1.100/24"
        gateway: "192.168.1.1"
        dns: ["8.8.8.8", "1.1.1.1"]
  
  # Cloud-init user data
  userData:
    cloudInit:
      inline: |
        #cloud-config
        hostname: dev-workstation
        users:
          - name: developer
            sudo: ALL=(ALL) NOPASSWD:ALL
            shell: /bin/bash
            ssh_authorized_keys:
              - "ssh-ed25519 AAAA..."
        packages:
          - build-essential
          - docker.io
          - code
        runcmd:
          - systemctl enable docker
          - usermod -aG docker developer
```

## Cloud-Init Integration

### Automatic Configuration

The LibVirt provider automatically handles cloud-init setup:

- **ISO Generation**: Creates cloud-init ISO with user-data and meta-data
- **Attachment**: Attaches ISO as CD-ROM device to VM
- **Network Config**: Generates network configuration from VM spec
- **User Data**: Renders templates with VM-specific values

### Advanced Cloud-Init

```yaml
userData:
  cloudInit:
    inline: |
      #cloud-config
      hostname: {{"{{ .Name }}"}}
      
      # Network configuration (if not using DHCP)
      network:
        version: 2
        ethernets:
          ens3:
            addresses: [192.168.1.100/24]
            gateway4: 192.168.1.1
            nameservers:
              addresses: [8.8.8.8, 1.1.1.1]
      
      # Storage configuration
      disk_setup:
        /dev/vdb:
          table_type: gpt
          layout: true
      
      fs_setup:
        - device: /dev/vdb1
          filesystem: ext4
          label: data
      
      mounts:
        - [/dev/vdb1, /data, ext4, defaults]
      
      # Package installation
      packages:
        - qemu-guest-agent  # Enable guest agent
        - cloud-init
        - curl
      
      # Enable services
      runcmd:
        - systemctl enable qemu-guest-agent
        - systemctl start qemu-guest-agent
```

## Performance Optimization

### KVM Optimization

```yaml
# VMClass with performance optimizations
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: high-performance
spec:
  cpus: 8
  memory: "16Gi"
  spec:
    cpu:
      mode: "host-passthrough"  # Best performance
      topology:
        sockets: 1
        cores: 8
        threads: 1
    # NUMA topology for large VMs
    numa:
      cells:
        - id: 0
          cpus: "0-7"
          memory: "16"
    
    # Virtio devices for performance
    devices:
      disk:
        bus: "virtio"
        cache: "none"
        io: "native"
      network:
        model: "virtio"
      video:
        model: "virtio"
```

### Storage Performance

```bash
# Create high-performance storage pool
virsh pool-define-as ssd-pool logical --source-name vg-ssd --target /dev/vg-ssd
virsh pool-start ssd-pool

# Use raw format for better performance (larger disk usage)
virsh vol-create-as ssd-pool vm-disk 100G --format raw

# Enable native AIO and disable cache for direct I/O
# (configured automatically by provider based on VMClass)
```

## Troubleshooting

### Common Issues

#### ❌ Connection Failed

**Symptom**: `failed to connect to Libvirt: <error>`

**Causes & Solutions**:

1. **Local connection issues**:
   ```bash
   # Check libvirtd status
   sudo systemctl status libvirtd
   
   # Start if not running
   sudo systemctl start libvirtd
   sudo systemctl enable libvirtd
   
   # Test connection
   virsh -c qemu:///system list
   ```

2. **Remote SSH connection**:
   ```bash
   # Test SSH connectivity
   ssh user@libvirt-host virsh list
   
   # Check SSH key permissions
   chmod 600 ~/.ssh/id_rsa
   ```

3. **Remote TLS connection**:
   ```bash
   # Verify certificates
   openssl x509 -in client-cert.pem -text -noout
   
   # Test TLS connection
   virsh -c qemu+tls://host:16514/system list
   ```

#### ❌ Permission Denied

**Symptom**: `authentication failed` or `permission denied`

**Solutions**:
```bash
# Add user to libvirt group
sudo usermod -a -G libvirt $USER

# Check libvirt group membership
groups $USER

# Verify permissions on libvirt socket
ls -la /var/run/libvirt/libvirt-sock

# For containerized providers, ensure socket is mounted
```

#### ❌ Storage Pool Not Found

**Symptom**: `storage pool 'default' not found`

**Solution**:
```bash
# List available pools
virsh pool-list --all

# Create default pool if missing
virsh pool-define-as default dir --target /var/lib/libvirt/images
virsh pool-build default
virsh pool-start default
virsh pool-autostart default

# Verify pool is active
virsh pool-info default
```

#### ❌ Network Not Available

**Symptom**: `network 'default' not found`

**Solution**:
```bash
# List networks
virsh net-list --all

# Start default network
virsh net-start default
virsh net-autostart default

# Create bridge network if needed
virsh net-define /usr/share/libvirt/networks/default.xml
```

#### ❌ KVM Not Available

**Symptom**: `KVM is not available` or `hardware acceleration not available`

**Solutions**:
1. **Check virtualization support**:
   ```bash
   # Check CPU virtualization features
   egrep -c '(vmx|svm)' /proc/cpuinfo
   
   # Check KVM modules
   lsmod | grep kvm
   
   # Load KVM modules if missing
   sudo modprobe kvm
   sudo modprobe kvm_intel  # or kvm_amd
   ```

2. **BIOS/UEFI settings**: Enable Intel VT-x or AMD-V
3. **Nested virtualization**: If running in a VM, enable nested virtualization

### Validation Commands

Test your LibVirt setup before deploying:

```bash
# 1. Test LibVirt connection
virsh -c qemu:///system list

# 2. Check storage pools
virsh pool-list --all

# 3. Check networks
virsh net-list --all

# 4. Test VM creation (simple test)
virt-install --name test-vm --memory 512 --vcpus 1 \
  --disk size=1 --network network=default \
  --boot cdrom --noautoconsole --dry-run

# 5. From within Kubernetes pod
kubectl run debug --rm -i --tty --image=ubuntu:22.04 -- bash
# Then test virsh commands if socket is mounted
```

### Debug Logging

Enable verbose logging for the LibVirt provider:

```yaml
providers:
  libvirt:
    env:
      - name: LOG_LEVEL
        value: "debug"
      - name: LIBVIRT_DEBUG
        value: "1"
    endpoint: "qemu:///system"
```

## Advanced Configuration

### High Availability Setup

```yaml
# Multiple LibVirt hosts for HA
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-cluster
spec:
  type: libvirt
  # Use load balancer or failover endpoint
  endpoint: "qemu+tls://libvirt-cluster.example.com:16514/system"
  runtime:
    replicas: 2  # Multiple provider instances
    affinity:
      podAntiAffinity:
        preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            labelSelector:
              matchLabels:
                app: libvirt-provider
            topologyKey: kubernetes.io/hostname
```

### GPU Passthrough

```yaml
# VMClass with GPU passthrough
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: gpu-workstation
spec:
  cpus: 8
  memory: "32Gi"
  spec:
    devices:
      hostdev:
        - type: "pci"
          source:
            address:
              domain: "0x0000"
              bus: "0x01"
              slot: "0x00"
              function: "0x0"
          managed: true
```

## API Reference

For complete API reference, see the [Provider API Documentation](../api-reference/).

## Contributing

To contribute to the LibVirt provider:

1. See the [Provider Development Guide](../tutorial.md)
2. Check the [GitHub repository](https://github.com/projectbeskar/virtrigaud)
3. Review [open issues](https://github.com/projectbeskar/virtrigaud/labels/provider%2Flibvirt)

## Support

- **Documentation**: [VirtRigaud Docs](https://projectbeskar.github.io/virtrigaud/)
- **Issues**: [GitHub Issues](https://github.com/projectbeskar/virtrigaud/issues)
- **Community**: [Discord](https://discord.gg/projectbeskar)
- **LibVirt**: [libvirt.org](https://libvirt.org/docs.html)
