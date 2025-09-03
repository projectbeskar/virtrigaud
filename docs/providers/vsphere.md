# vSphere Provider

The vSphere provider enables VirtRigaud to manage virtual machines on VMware vSphere environments, including vCenter Server and standalone ESXi hosts. This provider is designed for enterprise production environments with comprehensive support for vSphere features.

## Overview

This provider implements the VirtRigaud provider interface to manage VM lifecycle operations on VMware vSphere:

- **Create**: Create VMs from templates, content libraries, or OVF/OVA files
- **Delete**: Remove VMs and associated storage (with configurable retention)
- **Power**: Start, stop, restart, and suspend virtual machines
- **Describe**: Query VM state, resource usage, guest info, and vSphere properties
- **Reconfigure**: Hot-add CPU/memory, resize disks, modify network adapters
- **Clone**: Create full or linked clones from existing VMs or templates
- **Snapshot**: Create, delete, and revert VM snapshots with memory state
- **ImagePrepare**: Import OVF/OVA, deploy from content library, or ensure template existence

## Prerequisites

**⚠️ IMPORTANT: Active vSphere Environment Required**

The vSphere provider connects to VMware vSphere infrastructure and requires active vCenter Server or ESXi hosts.

### Requirements:
- **vCenter Server 7.0+** or **ESXi 7.0+** (running and accessible)
- **User account** with appropriate privileges for VM management
- **Network connectivity** from VirtRigaud to vCenter/ESXi (HTTPS/443)
- **vSphere infrastructure**:
  - Configured datacenters, clusters, and hosts
  - Storage (datastores) for VM files
  - Networks (port groups) for VM connectivity
  - Resource pools for VM placement (optional)

### Testing/Development:
For development environments:
- Use **VMware vSphere Hypervisor (ESXi)** free version
- **vCenter Server Appliance** evaluation license
- **VMware Workstation/Fusion** with nested ESXi
- **EVE-NG** or **GNS3** with vSphere emulation

## Authentication

The vSphere provider supports multiple authentication methods:

### Username/Password Authentication (Common)

Standard vSphere user authentication:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  namespace: default
spec:
  type: vsphere
  endpoint: https://vcenter.example.com
  credentialSecretRef:
    name: vsphere-credentials
  # Optional: Skip TLS verification (development only)
  insecureSkipVerify: false
  runtime:
    mode: Remote
    image: "virtrigaud/provider-vsphere:latest"
    service:
      port: 9090
```

Create credentials secret:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: vsphere-credentials
  namespace: default
type: Opaque
stringData:
  username: "virtrigaud@vsphere.local"
  password: "SecurePassword123!"
```

### Session Token Authentication (Advanced)

For environments using external authentication:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: vsphere-token
  namespace: default
type: Opaque
stringData:
  token: "vmware-api-session-id:abcd1234..."
```

### Service Account Authentication (Recommended)

Create a dedicated service account with minimal required privileges:

```yaml
# vSphere privileges for VirtRigaud service account:
# - Datastore: Allocate space, Browse datastore, Low level file operations
# - Network: Assign network  
# - Resource: Assign virtual machine to resource pool
# - Virtual machine: All privileges (or subset based on requirements)
# - Global: Enable methods, Disable methods, Licenses
```

## Configuration

### Connection Endpoints

| Endpoint Type | Format | Use Case |
|---------------|--------|----------|
| vCenter Server | `https://vcenter.example.com` | Multi-host management (recommended) |
| vCenter FQDN | `https://vcenter.corp.local` | Internal domain environments |
| vCenter IP | `https://192.168.1.10` | Direct IP access |
| ESXi Host | `https://esxi-host.example.com` | Single host environments |

### Deployment Configuration

#### Using Helm Values

```yaml
# values.yaml
providers:
  vsphere:
    enabled: true
    endpoint: "https://vcenter.example.com"
    insecureSkipVerify: false  # Set to true for self-signed certificates
    credentialSecretRef:
      name: vsphere-credentials
      namespace: virtrigaud-system
```

#### Production Configuration with TLS

```yaml
# Create secret with credentials and TLS certificates
apiVersion: v1
kind: Secret
metadata:
  name: vsphere-secure-credentials
  namespace: virtrigaud-system
type: Opaque
stringData:
  username: "svc-virtrigaud@vsphere.local"
  password: "SecurePassword123!"
  # Optional: Custom CA certificate for vCenter
  ca.crt: |
    -----BEGIN CERTIFICATE-----
    # Your vCenter CA certificate here
    -----END CERTIFICATE-----

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-production
  namespace: virtrigaud-system
spec:
  type: vsphere
  endpoint: https://vcenter.prod.example.com
  credentialSecretRef:
    name: vsphere-secure-credentials
  insecureSkipVerify: false
```

#### Development Configuration

```yaml
# For development with self-signed certificates
providers:
  vsphere:
    enabled: true
    endpoint: "https://esxi-dev.local"
    insecureSkipVerify: true  # Only for development!
    credentialSecretRef:
      name: vsphere-dev-credentials
```

#### Multi-vCenter Configuration

```yaml
# Deploy multiple providers for different vCenters
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-datacenter-a
spec:
  type: vsphere
  endpoint: https://vcenter-a.example.com
  credentialSecretRef:
    name: vsphere-credentials-a

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-datacenter-b
spec:
  type: vsphere
  endpoint: https://vcenter-b.example.com
  credentialSecretRef:
    name: vsphere-credentials-b
```

## vSphere Infrastructure Setup

### Required vSphere Objects

The provider expects the following vSphere infrastructure to be configured:

#### Datacenters and Clusters
```bash
# Example vSphere hierarchy:
Datacenter: "Production"
├── Cluster: "Compute-Cluster"
│   ├── ESXi Host: esxi-01.example.com
│   ├── ESXi Host: esxi-02.example.com
│   └── ESXi Host: esxi-03.example.com
├── Datastores:
│   ├── "datastore-ssd"     # High-performance storage
│   ├── "datastore-hdd"     # Standard storage
│   └── "datastore-backup"  # Backup storage
└── Networks:
    ├── "VM Network"        # Default VM network
    ├── "DMZ-Network"       # DMZ port group
    └── "Management"        # Management network
```

#### Resource Pools (Optional)
```bash
# Create resource pools for workload isolation
Datacenter: "Production"
└── Cluster: "Compute-Cluster"
    └── Resource Pools:
        ├── "Development"    # Dev workloads (lower priority)
        ├── "Production"     # Prod workloads (high priority)
        └── "Testing"        # Test workloads (medium priority)
```

## VM Configuration

### VMClass Specification

Define CPU, memory, and vSphere-specific settings:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: standard-vm
spec:
  cpus: 4
  memory: "8Gi"
  # vSphere-specific configuration
  spec:
    # VM hardware settings
    hardware:
      version: "vmx-19"              # Hardware version
      firmware: "efi"                # BIOS or EFI
      secureBoot: true               # Secure boot (EFI only)
      enableCpuHotAdd: true          # Hot-add CPU
      enableMemoryHotAdd: true       # Hot-add memory
    
    # CPU configuration
    cpu:
      coresPerSocket: 2              # CPU topology
      enableVirtualization: false    # Nested virtualization
      reservationMHz: 1000           # CPU reservation
      limitMHz: 4000                 # CPU limit
    
    # Memory configuration  
    memory:
      reservationMB: 2048            # Memory reservation
      limitMB: 8192                  # Memory limit
      shareLevel: "normal"           # Memory shares (low/normal/high)
    
    # Storage configuration
    storage:
      diskFormat: "thin"             # thick/thin/eagerZeroedThick
      storagePolicy: "VM Storage Policy - SSD"  # vSAN storage policy
    
    # vSphere placement
    placement:
      datacenter: "Production"       # Target datacenter
      cluster: "Compute-Cluster"     # Target cluster  
      resourcePool: "Production"     # Target resource pool
      datastore: "datastore-ssd"     # Preferred datastore
      folder: "/vm/virtrigaud"       # VM folder
```

### VMImage Specification

Reference vSphere templates, content library items, or OVF files:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22-04-template
spec:
  # Template from vSphere inventory
  source:
    template: "ubuntu-22.04-template"
    datacenter: "Production"
    folder: "/vm/templates"
  
  # Or from content library
  # source:
  #   contentLibrary: "OS Templates"
  #   item: "ubuntu-22.04-cloud"
  
  # Or from OVF/OVA URL
  # source:
  #   ovf: "https://releases.ubuntu.com/22.04/ubuntu-22.04-server-cloudimg-amd64.ova"
  
  # Guest OS identification
  guestOS: "ubuntu64Guest"
  
  # Customization specification
  customization:
    type: "cloudInit"              # cloudInit, sysprep, or linux
    spec: "ubuntu-cloud-init"      # Reference to customization spec
```

### Complete VM Example

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-application
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: standard-vm
  imageRef:
    name: ubuntu-22-04-template
  powerState: On
  
  # Disk configuration
  disks:
    - name: root
      size: "100Gi"
      storageClass: "ssd-storage"
      # vSphere-specific disk options
      spec:
        diskMode: "persistent"       # persistent, independent_persistent, independent_nonpersistent
        diskFormat: "thin"           # thick, thin, eagerZeroedThick
        controllerType: "scsi"       # scsi, ide, nvme
        unitNumber: 0                # SCSI unit number
    
    - name: data
      size: "500Gi" 
      storageClass: "hdd-storage"
      spec:
        diskFormat: "thick"
        controllerType: "scsi"
        unitNumber: 1
  
  # Network configuration
  networks:
    # Primary application network
    - name: app-network
      portGroup: "VM Network"
      # Optional: Static IP assignment
      staticIP:
        address: "192.168.100.50/24"
        gateway: "192.168.100.1"
        dns: ["192.168.1.10", "8.8.8.8"]
    
    # Management network
    - name: mgmt-network
      portGroup: "Management"
      # DHCP assignment (default)
  
  # vSphere-specific placement
  placement:
    datacenter: "Production"
    cluster: "Compute-Cluster"
    resourcePool: "Production"
    folder: "/vm/applications"
    datastore: "datastore-ssd"      # Override class default
    host: "esxi-01.example.com"      # Pin to specific host (optional)
  
  # Guest customization
  userData:
    cloudInit:
      inline: |
        #cloud-config
        hostname: web-application
        users:
          - name: ubuntu
            sudo: ALL=(ALL) NOPASSWD:ALL
            ssh_authorized_keys:
              - "ssh-ed25519 AAAA..."
        packages:
          - nginx
          - docker.io
          - open-vm-tools          # VMware tools for guest integration
        runcmd:
          - systemctl enable nginx
          - systemctl enable docker
          - systemctl enable open-vm-tools
```

## Advanced Features

### Template Management

#### Creating Templates

```yaml
# Convert existing VM to template
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMTemplate
metadata:
  name: create-ubuntu-template
spec:
  sourceVM: "ubuntu-base-vm"
  datacenter: "Production"
  targetFolder: "/vm/templates"
  templateName: "ubuntu-22.04-template"
  
  # Template metadata
  annotation: |
    Ubuntu 22.04 LTS Template
    Created: 2024-01-15
    Includes: cloud-init, open-vm-tools
  
  # Template customization
  powerOff: true                   # Power off before conversion
  removeSnapshots: true           # Clean up snapshots
  updateTools: true               # Update VMware tools
```

#### Content Library Integration

```yaml
# Deploy from content library
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage  
metadata:
  name: centos-stream-9
spec:
  source:
    contentLibrary: "OS Templates"
    item: "CentOS-Stream-9"
    datacenter: "Production"
  
  # Content library item properties
  properties:
    version: "9.0"
    provider: "CentOS"
    osType: "linux"
```

### Storage Policies

```yaml
# VMClass with vSAN storage policy
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: high-performance
spec:
  cpus: 8
  memory: "32Gi"
  spec:
    storage:
      # vSAN storage policies
      homePolicy: "VM Storage Policy - Performance"    # VM home/config files
      diskPolicy: "VM Storage Policy - SSD Only"       # Virtual disks
      swapPolicy: "VM Storage Policy - Standard"        # Swap files
      
      # Traditional storage
      datastoreCluster: "DatastoreCluster-SSD"         # Datastore cluster
      antiAffinityRules: true                          # VM anti-affinity
```

### Network Advanced Configuration

```yaml
# Advanced networking with distributed switches
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: advanced-networking
spec:
  networks:
    # Distributed port group
    - name: frontend
      portGroup: "DPG-Frontend-VLAN100"
      distributedSwitch: "DSwitch-Production"
      vlan: 100
      
    # NSX-T logical switch
    - name: backend  
      portGroup: "LS-Backend-App"
      nsx: true
      securityPolicy: "Backend-Security-Policy"
      
    # SR-IOV for high performance
    - name: storage
      portGroup: "DPG-Storage-VLAN200"
      sriov: true
      bandwidth:
        reservation: 1000  # Mbps
        limit: 10000      # Mbps
        shares: 100       # Priority
```

### High Availability

```yaml
# VM with HA/DRS settings
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: critical-application
spec:
  providerRef:
    name: vsphere-prod
  # ... other config ...
  
  # High availability configuration
  availability:
    # HA restart priority
    restartPriority: "high"          # disabled, low, medium, high
    isolationResponse: "powerOff"    # none, powerOff, shutdown
    vmMonitoring: "vmMonitoringOnly" # vmMonitoringDisabled, vmMonitoringOnly, vmAndAppMonitoring
    
    # DRS configuration
    drsAutomationLevel: "fullyAutomated"  # manual, partiallyAutomated, fullyAutomated
    drsVmBehavior: "fullyAutomated"       # manual, partiallyAutomated, fullyAutomated
    
    # Anti-affinity rules
    antiAffinityGroups: ["web-tier", "database-tier"]
    
    # Host affinity (pin to specific hosts)
    hostAffinityGroups: ["production-hosts"]
```

### Snapshot Management

```yaml
# Advanced snapshot configuration
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSnapshot
metadata:
  name: pre-upgrade-snapshot
spec:
  vmRef:
    name: web-application
  
  # Snapshot settings
  name: "Pre-upgrade snapshot"
  description: "Snapshot before application upgrade"
  memory: true                    # Include memory state
  quiesce: true                   # Quiesce guest filesystem
  
  # Retention policy
  retention:
    maxSnapshots: 3               # Keep max 3 snapshots
    maxAge: "7d"                  # Delete after 7 days
    
  # Schedule (optional)
  schedule: "0 2 * * 0"          # Weekly at 2 AM Sunday
```

## Troubleshooting

### Common Issues

#### ❌ Connection Failed

**Symptom**: `failed to connect to vSphere: connection refused`

**Causes & Solutions**:

1. **Network connectivity**:
   ```bash
   # Test connectivity to vCenter
   telnet vcenter.example.com 443
   
   # Test from Kubernetes pod
   kubectl run debug --rm -i --tty --image=curlimages/curl -- \
     curl -k https://vcenter.example.com
   ```

2. **DNS resolution**:
   ```bash
   # Test DNS resolution
   nslookup vcenter.example.com
   
   # Use IP address if DNS fails
   ```

3. **Firewall rules**: Ensure port 443 is accessible from Kubernetes cluster

#### ❌ Authentication Failed

**Symptom**: `Login failed: incorrect user name or password`

**Solutions**:

1. **Verify credentials**:
   ```bash
   # Test credentials manually
   kubectl get secret vsphere-credentials -o yaml
   
   # Decode and verify
   echo "base64-password" | base64 -d
   ```

2. **Check user permissions**:
   - Verify user exists in vCenter
   - Check assigned roles and privileges
   - Ensure user is not locked out

3. **Test login via vSphere Client**: Verify credentials work in the GUI

#### ❌ Insufficient Privileges

**Symptom**: `operation requires privilege 'VirtualMachine.Interact.PowerOn'`

**Solution**: Grant required privileges to the service account:

```bash
# Required privileges for VirtRigaud:
# - Datastore privileges:
#   * Datastore.AllocateSpace
#   * Datastore.Browse  
#   * Datastore.FileManagement
# - Network privileges:
#   * Network.Assign
# - Resource privileges:
#   * Resource.AssignVMToPool
# - Virtual machine privileges:
#   * VirtualMachine.* (all) or specific subset
# - Global privileges:
#   * Global.EnableMethods
#   * Global.DisableMethods
```

#### ❌ Template Not Found

**Symptom**: `template 'ubuntu-template' not found`

**Solutions**:
```bash
# List available templates
govc ls /datacenter/vm/templates/

# Check template path and permissions
govc object.collect -s vm/templates/ubuntu-template summary.config.name

# Verify template is properly marked as template
govc object.collect -s vm/templates/ubuntu-template config.template
```

#### ❌ Datastore Issues

**Symptom**: `insufficient disk space` or `datastore not accessible`

**Solutions**:
```bash
# Check datastore capacity
govc datastore.info datastore-name

# List accessible datastores
govc datastore.ls

# Check datastore cluster configuration
govc cluster.ls
```

#### ❌ Network Configuration

**Symptom**: `network 'VM Network' not found`

**Solutions**:
```bash
# List available networks
govc ls /datacenter/network/

# Check distributed port groups
govc dvs.portgroup.info

# Verify network accessibility from cluster
govc cluster.network.info
```

### Validation Commands

Test your vSphere setup before deploying:

```bash
# 1. Install and configure govc CLI tool
export GOVC_URL='https://vcenter.example.com'
export GOVC_USERNAME='administrator@vsphere.local'
export GOVC_PASSWORD='password'
export GOVC_INSECURE=1  # for self-signed certificates

# 2. Test connectivity
govc about

# 3. List datacenters
govc ls

# 4. List clusters and hosts
govc ls /datacenter/host/

# 5. List datastores
govc ls /datacenter/datastore/

# 6. List networks
govc ls /datacenter/network/

# 7. List templates
govc ls /datacenter/vm/templates/

# 8. Test VM creation (dry run)
govc vm.create -c 1 -m 1024 -g ubuntu64Guest -net "VM Network" test-vm
govc vm.destroy test-vm
```

### Debug Logging

Enable verbose logging for the vSphere provider:

```yaml
providers:
  vsphere:
    env:
      - name: LOG_LEVEL
        value: "debug"
      - name: GOVMOMI_DEBUG
        value: "true"
    endpoint: "https://vcenter.example.com"
```

Monitor vSphere tasks:
```bash
# Monitor recent tasks in vCenter
govc task.ls

# Get details of specific task
govc task.info task-123
```

## Performance Optimization

### Resource Allocation

```yaml
# High-performance VMClass
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: performance-optimized
spec:
  cpus: 16
  memory: "64Gi"
  spec:
    cpu:
      coresPerSocket: 8            # Match physical CPU topology
      reservationMHz: 8000         # Guarantee CPU resources
      shares: 2000                 # High priority (normal=1000)
      enableVirtualization: false  # Disable if not needed for performance
    
    memory:
      reservationMB: 65536         # Guarantee memory
      shares: 2000                 # High priority
      shareLevel: "high"           # Alternative to shares value
    
    hardware:
      enableCpuHotAdd: false       # Better performance when disabled
      enableMemoryHotAdd: false    # Better performance when disabled
      
    # NUMA configuration for large VMs
    numa:
      enabled: true
      coresPerSocket: 8            # Align with NUMA topology
```

### Storage Optimization

```yaml
# Storage-optimized configuration
spec:
  storage:
    diskFormat: "eagerZeroedThick"  # Best performance, more space usage
    controllerType: "pvscsi"        # Paravirtual SCSI for better performance
    multiwriter: false              # Disable unless needed
    
    # vSAN optimization
    storagePolicy: "Performance-Tier"
    cachingPolicy: "writethrough"   # or "writeback" for better performance
    
    # Multiple controllers for high IOPS
    scsiControllers:
      - type: "pvscsi"
        busNumber: 0
        maxDevices: 15
      - type: "pvscsi" 
        busNumber: 1
        maxDevices: 15
```

### Network Optimization

```yaml
# High-performance networking
networks:
  - name: high-performance
    portGroup: "DPG-HighPerf-SR-IOV"
    adapter: "vmxnet3"             # Best performance adapter
    sriov: true                    # SR-IOV for near-native performance
    bandwidth:
      reservation: 1000            # Guaranteed bandwidth (Mbps)
      limit: 10000                 # Maximum bandwidth (Mbps)
      shares: 100                  # Priority level
```

## API Reference

For complete API reference, see the [Provider API Documentation](../api-reference/).

## Contributing

To contribute to the vSphere provider:

1. See the [Provider Development Guide](../tutorial.md)
2. Check the [GitHub repository](https://github.com/projectbeskar/virtrigaud)
3. Review [open issues](https://github.com/projectbeskar/virtrigaud/labels/provider%2Fvsphere)

## Support

- **Documentation**: [VirtRigaud Docs](https://projectbeskar.github.io/virtrigaud/)
- **Issues**: [GitHub Issues](https://github.com/projectbeskar/virtrigaud/issues)
- **Community**: [Discord](https://discord.gg/projectbeskar)
- **VMware**: [vSphere API Documentation](https://developer.vmware.com/apis/vsphere-automation/)
- **govc**: [govc CLI Tool](https://github.com/vmware/govmomi/tree/master/govc)
