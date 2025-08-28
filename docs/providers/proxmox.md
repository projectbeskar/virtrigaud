# Proxmox VE Provider

The Proxmox VE provider enables VirtRigaud to manage virtual machines on Proxmox Virtual Environment (PVE) clusters using the native Proxmox API.

## Overview

This provider implements the VirtRigaud provider interface to manage VM lifecycle operations on Proxmox VE:

- **Create**: Create VMs from templates or ISO images with cloud-init support
- **Delete**: Remove VMs and associated resources
- **Power**: Start, stop, and reboot virtual machines
- **Describe**: Query VM state, IPs, and console access
- **Reconfigure**: Hot-plug CPU/memory changes, disk expansion
- **Clone**: Create linked or full clones of existing VMs
- **Snapshot**: Create, delete, and revert VM snapshots with memory state
- **ImagePrepare**: Import and prepare VM templates from URLs or ensure existence

## Prerequisites

- Proxmox VE 7.0 or later
- API token or user account with appropriate privileges
- Network connectivity from VirtRigaud to Proxmox API (port 8006)

## Authentication

The Proxmox provider supports two authentication methods:

### API Token Authentication (Recommended)

API tokens provide secure, scope-limited access without exposing user passwords.

1. **Create API Token in Proxmox**:
   ```bash
   # In Proxmox web UI: Datacenter -> Permissions -> API Tokens
   # Or via CLI:
   pveum user token add <USER@REALM> <TOKENID> --privsep 0
   ```

2. **Configure Provider**:
   ```yaml
   apiVersion: infra.virtrigaud.io/v1beta1
   kind: Provider
   metadata:
     name: proxmox-prod
   spec:
     type: proxmox
     endpoint: https://pve.example.com:8006
     credentialSecretRef:
       name: pve-credentials
     runtime:
       mode: Remote
       image: ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.1.0
   ```

3. **Create Credentials Secret**:
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: pve-credentials
   type: Opaque
   stringData:
     token_id: "virtrigaud@pve!vrtg-token"
     token_secret: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
   ```

### Session Cookie Authentication (Optional)

For environments that cannot use API tokens:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pve-credentials
type: Opaque
stringData:
  username: "virtrigaud@pve"
  password: "secure-password"
```

## TLS Configuration

### Self-Signed Certificates (Development)

For test environments with self-signed certificates:

```yaml
spec:
  runtime:
    env:
      - name: PVE_INSECURE_SKIP_VERIFY
        value: "true"
```

### Custom CA Certificate (Production)

For production with custom CA:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pve-credentials
type: Opaque
stringData:
  ca.crt: |
    -----BEGIN CERTIFICATE-----
    MIIDXTCCAkWgAwIBAgIJAL...
    -----END CERTIFICATE-----
```

## Reconfiguration Support

### Online Reconfiguration

The Proxmox provider supports online (hot-plug) reconfiguration for:

- **CPU**: Add/remove vCPUs while VM is running (guest OS support required)
- **Memory**: Increase memory using balloon driver (guest tools required)
- **Disk Expansion**: Expand disks online (disk shrinking not supported)

### Reconfigure Matrix

| Operation | Online Support | Requirements | Notes |
|-----------|---------------|--------------|-------|
| CPU increase | ✅ Yes | Guest OS support | Most modern Linux/Windows |
| CPU decrease | ✅ Yes | Guest OS support | May require guest cooperation |
| Memory increase | ✅ Yes | Balloon driver | Install qemu-guest-agent |
| Memory decrease | ⚠️ Limited | Balloon driver + guest | May require power cycle |
| Disk expand | ✅ Yes | Online resize support | Filesystem resize separate |
| Disk shrink | ❌ No | Not supported | Security/data protection |

### Example Reconfiguration

```yaml
# Scale up VM resources
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server
spec:
  # ... existing spec ...
  classRef:
    name: large  # Changed from 'small'
---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: large
spec:
  cpus: 8        # Increased from 2
  memory: "16Gi" # Increased from 4Gi
```

## Snapshot Management

### Snapshot Features

- **Memory Snapshots**: Include VM memory state for consistent restore
- **Crash-Consistent**: Without memory for faster snapshots
- **Snapshot Trees**: Nested snapshots with parent-child relationships
- **Metadata**: Description and timestamp tracking

### Snapshot Operations

```yaml
# Create snapshot with memory
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSnapshot
metadata:
  name: before-upgrade
spec:
  vmRef:
    name: web-server
  description: "Pre-maintenance snapshot"
  includeMemory: true  # Include running memory state
```

```bash
# Create snapshot via kubectl
kubectl create vmsnapshot before-upgrade \
  --vm=web-server \
  --description="Before major upgrade" \
  --include-memory=true
```

## Multi-NIC Networking

### Network Configuration

The provider supports multiple network interfaces with:

- **Bridge Assignment**: Map to Proxmox bridges (vmbr0, vmbr1, etc.)
- **VLAN Tagging**: 802.1Q VLAN support
- **Static IPs**: Cloud-init integration for network configuration
- **MAC Addresses**: Custom MAC assignment

### Example Multi-NIC VM

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: multi-nic-vm
spec:
  providerRef:
    name: proxmox-prod
  classRef:
    name: medium
  imageRef:
    name: ubuntu-22
  networks:
    # Primary LAN interface
    - name: lan
      bridge: vmbr0
      staticIP:
        address: "192.168.1.100/24"
        gateway: "192.168.1.1"
        dns: ["8.8.8.8", "1.1.1.1"]
    
    # DMZ interface with VLAN
    - name: dmz
      bridge: vmbr1
      vlan: 100
      staticIP:
        address: "10.0.100.50/24"
    
    # Management interface
    - name: mgmt
      bridge: vmbr2
      mac: "02:00:00:aa:bb:cc"
```

### Network Bridge Mapping

| Network Name | Default Bridge | Use Case |
|--------------|---------------|----------|
| `lan`, `default` | vmbr0 | General LAN connectivity |
| `dmz` | vmbr1 | DMZ/public services |
| `mgmt`, `management` | vmbr2 | Management network |
| `vmbr*` | Same name | Direct bridge reference |

## Configuration

### Environment Variables

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `PVE_ENDPOINT` | Proxmox API endpoint URL | Yes | - |
| `PVE_TOKEN_ID` | API token identifier | Yes* | - |
| `PVE_TOKEN_SECRET` | API token secret | Yes* | - |
| `PVE_USERNAME` | Username for session auth | Yes* | - |
| `PVE_PASSWORD` | Password for session auth | Yes* | - |
| `PVE_NODE_SELECTOR` | Preferred nodes (comma-separated) | No | Auto-detect |
| `PVE_INSECURE_SKIP_VERIFY` | Skip TLS verification | No | `false` |
| `PVE_CA_BUNDLE` | Custom CA certificate | No | - |

\* Either token or username/password is required

### Node Selection

The provider can be configured to prefer specific nodes:

```yaml
env:
  - name: PVE_NODE_SELECTOR
    value: "pve-node-1,pve-node-2"
```

If not specified, the provider will automatically select nodes based on availability.

## VM Configuration

### VMClass Specification

Define CPU and memory resources:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: small
spec:
  cpus: 2
  memory: "4Gi"
  # Proxmox-specific settings
  spec:
    machine: "q35"
    bios: "uefi"
```

### VMImage Specification

Reference Proxmox templates:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22
spec:
  source: "ubuntu-22-template"  # Template name in Proxmox
  # Or clone from existing VM:
  # source: "9000"  # VMID to clone from
```

### VirtualMachine Example

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server
spec:
  providerRef:
    name: proxmox-prod
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  powerState: On
  networks:
    - name: lan
      # Maps to Proxmox bridge or VLAN configuration
  disks:
    - name: root
      size: "40Gi"
  userData:
    cloudInit:
      inline: |
        #cloud-config
        hostname: web-server
        users:
          - name: ubuntu
            ssh_authorized_keys:
              - "ssh-ed25519 AAAA..."
        packages:
          - nginx
```

## Cloud-Init Integration

The provider automatically configures cloud-init for supported VMs:

### Automatic Configuration

- **IDE2 Device**: Attached as cloudinit drive
- **User Data**: Rendered from VirtualMachine spec
- **Network Config**: Generated from network specifications
- **SSH Keys**: Extracted from userData or secrets

### Static IP Configuration

Configure static IPs using cloud-init:

```yaml
userData:
  cloudInit:
    inline: |
      #cloud-config
      write_files:
        - path: /etc/netplan/01-static.yaml
          content: |
            network:
              version: 2
              ethernets:
                ens18:
                  addresses: [192.168.1.100/24]
                  gateway4: 192.168.1.1
                  nameservers:
                    addresses: [8.8.8.8, 1.1.1.1]
```

Or use Proxmox IP configuration:

```yaml
# This would be handled by the provider internally
# when processing network specifications
```

## Cloning Behavior

### Linked Clones (Default)

Efficient space usage, faster creation:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClone
metadata:
  name: web-clone
spec:
  sourceVMRef:
    name: template-vm
  linkedClone: true  # Default
```

### Full Clones

Independent copies, slower creation:

```yaml
spec:
  linkedClone: false
```

## Snapshots

Create and manage VM snapshots:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSnapshot
metadata:
  name: before-upgrade
spec:
  vmRef:
    name: web-server
  description: "Snapshot before system upgrade"
```

## Troubleshooting

### Common Issues

#### Authentication Failures

```
Error: failed to connect to Proxmox VE: authentication failed
```

**Solutions**:
- Verify API token permissions
- Check token expiration
- Ensure user has VM.* privileges

#### TLS Certificate Errors

```
Error: x509: certificate signed by unknown authority
```

**Solutions**:
- Add custom CA certificate to credentials secret
- Use `PVE_INSECURE_SKIP_VERIFY=true` for testing
- Verify certificate chain

#### VM Creation Failures

```
Error: create VM failed with status 400: storage 'local-lvm' does not exist
```

**Solutions**:
- Verify storage configuration in Proxmox
- Check node availability
- Ensure sufficient resources

### Debug Logging

Enable debug logging for troubleshooting:

```yaml
env:
  - name: LOG_LEVEL
    value: "debug"
```

### Health Checks

Monitor provider health:

```bash
# Check provider pod logs
kubectl logs -n virtrigaud-system deployment/provider-proxmox

# Test connectivity
kubectl exec -n virtrigaud-system deployment/provider-proxmox -- \
  curl -k https://pve.example.com:8006/api2/json/version
```

## Performance Considerations

### Resource Allocation

For production environments:

```yaml
resources:
  requests:
    cpu: 100m
    memory: 256Mi
  limits:
    cpu: 500m
    memory: 512Mi
```

### Concurrent Operations

The provider handles concurrent VM operations efficiently but consider:

- Node capacity limits
- Storage I/O constraints
- Network bandwidth

### Task Polling

Task completion is polled every 2 seconds with a 5-minute timeout. These can be tuned via environment variables if needed.

## Minimal Proxmox VE Permissions

### Required API Token Permissions

Create an API token with these minimal privileges:

```bash
# Create user for VirtRigaud
pveum user add virtrigaud@pve --comment "VirtRigaud Provider"

# Create API token
pveum user token add virtrigaud@pve vrtg-token --privsep 1

# Grant minimal required permissions
pveum acl modify / --users virtrigaud@pve --roles PVEVMAdmin,PVEDatastoreUser

# Custom role with minimal permissions (alternative)
pveum role add VirtRigaud --privs "VM.Allocate,VM.Audit,VM.Config.CPU,VM.Config.Memory,VM.Config.Disk,VM.Config.Network,VM.Config.Options,VM.Monitor,VM.PowerMgmt,VM.Snapshot,VM.Clone,Datastore.Allocate,Datastore.AllocateSpace,Pool.Allocate"
pveum acl modify / --users virtrigaud@pve --roles VirtRigaud
```

### Permission Details

| Permission | Usage | Required |
|------------|-------|----------|
| `VM.Allocate` | Create new VMs | ✅ Core |
| `VM.Audit` | Read VM configuration | ✅ Core |
| `VM.Config.*` | Modify VM settings | ✅ Reconfigure |
| `VM.Monitor` | VM status monitoring | ✅ Core |
| `VM.PowerMgmt` | Power operations | ✅ Core |
| `VM.Snapshot` | Snapshot operations | ⚠️ Optional |
| `VM.Clone` | VM cloning | ⚠️ Optional |
| `Datastore.Allocate` | Create VM disks | ✅ Core |
| `Pool.Allocate` | Resource pool usage | ⚠️ Optional |

### Token Rotation Procedure

```bash
# 1. Create new token
NEW_TOKEN=$(pveum user token add virtrigaud@pve vrtg-token-2 --privsep 1 --output-format json | jq -r '.value')

# 2. Update Kubernetes secret
kubectl patch secret pve-credentials -n virtrigaud-system --type='merge' -p='{"stringData":{"token_id":"virtrigaud@pve!vrtg-token-2","token_secret":"'$NEW_TOKEN'"}}'

# 3. Restart provider to use new token
kubectl rollout restart deployment provider-proxmox -n virtrigaud-system

# 4. Verify new token works
kubectl logs deployment/provider-proxmox -n virtrigaud-system

# 5. Remove old token
pveum user token remove virtrigaud@pve vrtg-token
```

## NetworkPolicy Examples

### Production NetworkPolicy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: provider-proxmox-netpol
  namespace: virtrigaud-system
spec:
  podSelector:
    matchLabels:
      app: provider-proxmox
  policyTypes: [Ingress, Egress]
  
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: virtrigaud-manager
    ports: [9443, 8080]
  
  egress:
  # DNS resolution
  - to: []
    ports: [53]
  
  # Proxmox VE API
  - to:
    - ipBlock:
        cidr: 192.168.1.0/24  # Your PVE network
    ports: [8006]
```

### Development NetworkPolicy

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: provider-proxmox-dev-netpol
  namespace: virtrigaud-system
spec:
  podSelector:
    matchLabels:
      app: provider-proxmox
      environment: development
  egress:
  - to: []  # Allow all egress for development
```

## Storage and Placement

### Storage Class Mapping

Configure storage placement for different workloads:

```yaml
# High-performance storage
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: high-performance
spec:
  cpus: 8
  memory: "32Gi"
  storage:
    class: "nvme-storage"  # Maps to PVE storage
    type: "thin"           # Thin provisioning
    
# Standard storage
apiVersion: infra.virtrigaud.io/v1beta1  
kind: VMClass
metadata:
  name: standard
spec:
  cpus: 4
  memory: "8Gi"
  storage:
    class: "ssd-storage"
    type: "thick"          # Thick provisioning
```

### Placement Policies

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMPlacementPolicy
metadata:
  name: production-placement
spec:
  nodeSelector:
    - "pve-node-1"
    - "pve-node-2"
  antiAffinity:
    - key: "vm.type"
      operator: "In"
      values: ["database"]
  constraints:
    maxVMsPerNode: 10
    minFreeMemory: "4Gi"
```

## Performance Testing

### Load Test Results

Performance benchmarks using virtrigaud-loadgen against fake PVE server:

| Operation | P50 Latency | P95 Latency | Throughput | Notes |
|-----------|-------------|-------------|------------|-------|
| Create VM | 2.3s | 4.1s | 12 ops/min | Including cloud-init |
| Power On | 800ms | 1.2s | 45 ops/min | Async operation |
| Power Off | 650ms | 1.1s | 50 ops/min | Graceful shutdown |
| Describe | 120ms | 200ms | 200 ops/min | Status query |
| Reconfigure CPU | 1.8s | 3.2s | 15 ops/min | Online hot-plug |
| Snapshot Create | 3.5s | 6.8s | 8 ops/min | With memory |
| Clone (Linked) | 1.9s | 3.4s | 12 ops/min | Fast COW clone |

### Running Performance Tests

```bash
# Deploy fake PVE server for testing
kubectl apply -f test/performance/proxmox-loadtest.yaml

# Run performance test
kubectl create job proxmox-perf-test --from=cronjob/proxmox-performance-test

# View results
kubectl logs job/proxmox-perf-test -f
```

## Security Best Practices

1. **Use API Tokens**: Prefer API tokens over username/password
2. **Least Privilege**: Grant minimal required permissions (see above)
3. **TLS Verification**: Always verify certificates in production
4. **Secret Management**: Use Kubernetes secrets with proper RBAC
5. **Network Policies**: Restrict provider network access (see examples)
6. **Regular Rotation**: Rotate API tokens quarterly
7. **Audit Logging**: Enable PVE audit logs for provider actions
8. **Resource Quotas**: Limit provider resource consumption

## Examples

### Multi-Node Setup

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: proxmox-cluster
spec:
  type: proxmox
  endpoint: https://pve-cluster.example.com:8006
  runtime:
    env:
      - name: PVE_NODE_SELECTOR
        value: "pve-1,pve-2,pve-3"
```

### High-Availability Configuration

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: provider-proxmox
spec:
  replicas: 2
  template:
    spec:
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              labelSelector:
                matchLabels:
                  app: provider-proxmox
              topologyKey: kubernetes.io/hostname
```

## API Reference

For complete API reference, see the [Provider API Documentation](../api-reference/).

## Contributing

To contribute to the Proxmox provider:

1. See the [Provider Development Guide](../tutorial.md)
2. Check the [GitHub repository](https://github.com/projectbeskar/virtrigaud)
3. Review [open issues](https://github.com/projectbeskar/virtrigaud/labels/provider%2Fproxmox)

## Support

- **Documentation**: [VirtRigaud Docs](https://projectbeskar.github.io/virtrigaud/)
- **Issues**: [GitHub Issues](https://github.com/projectbeskar/virtrigaud/issues)
- **Community**: [Discord](https://discord.gg/projectbeskar)
