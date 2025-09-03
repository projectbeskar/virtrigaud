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

**⚠️ IMPORTANT: Active Proxmox VE Server Required**

The Proxmox provider requires a running Proxmox VE server to function. Unlike some providers that can operate in simulation mode, this provider performs actual API calls to Proxmox VE during startup and operation.

### Requirements:
- **Proxmox VE 7.0 or later** (running and accessible)
- **API token or user account** with appropriate privileges  
- **Network connectivity** from VirtRigaud to Proxmox API (port 8006/HTTPS)
- **Valid TLS configuration** (production) or skip verification (development)

### Testing/Development:
If you don't have a Proxmox VE server available:
- Use [Proxmox VE in a VM](https://pve.proxmox.com/wiki/Installation) for testing
- Consider alternative providers (libvirt, vSphere) for local development  
- The provider will fail startup validation without a reachable Proxmox endpoint

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
     namespace: default
   spec:
     type: proxmox
     endpoint: https://pve.example.com:8006
     credentialSecretRef:
       name: pve-credentials
     runtime:
       mode: Remote
       image: "virtrigaud/provider-proxmox:latest"
       service:
         port: 9090
   ```

3. **Create Credentials Secret**:
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: pve-credentials
     namespace: default
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
  namespace: default
type: Opaque
stringData:
  username: "virtrigaud@pve"
  password: "secure-password"
```

## Deployment Configuration

### Required Environment Variables

The Proxmox provider **requires** environment variables to connect to your Proxmox VE server. Configure these variables in your Helm values file:

| Variable | Required | Description | Example |
|----------|----------|-------------|---------|
| `PVE_ENDPOINT` | ✅ **Yes** | Proxmox VE API endpoint URL | `https://pve.example.com:8006` |
| `PVE_USERNAME` | ✅ **Yes**\* | Username for password auth | `root@pam` or `user@realm` |
| `PVE_PASSWORD` | ✅ **Yes**\* | Password for username | `secure-password` |
| `PVE_TOKEN_ID` | ✅ **Yes**\*\* | API token ID (alternative) | `user@realm!tokenid` |
| `PVE_TOKEN_SECRET` | ✅ **Yes**\*\* | API token secret (alternative) | `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` |
| `PVE_INSECURE_SKIP_VERIFY` | 🔵 Optional | Skip TLS verification | `true` (dev only) |

> **\*** Either username/password OR token authentication is required  
> **\*\*** API token authentication is recommended for production

### Helm Configuration Examples

#### Username/Password Authentication

```yaml
# values.yaml
providers:
  proxmox:
    enabled: true
    env:
      - name: PVE_ENDPOINT
        value: "https://your-proxmox-server.example.com:8006"
      - name: PVE_USERNAME
        value: "root@pam"
      - name: PVE_PASSWORD
        value: "your-secure-password"
```

#### API Token Authentication (Recommended)

```yaml
# values.yaml  
providers:
  proxmox:
    enabled: true
    env:
      - name: PVE_ENDPOINT
        value: "https://your-proxmox-server.example.com:8006"
      - name: PVE_TOKEN_ID
        value: "virtrigaud@pve!automation"
      - name: PVE_TOKEN_SECRET
        value: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
```

#### Using Kubernetes Secrets (Production)

For production environments, use Kubernetes secrets:

```yaml
# Create secret first
apiVersion: v1
kind: Secret
metadata:
  name: proxmox-credentials
type: Opaque
stringData:
  PVE_ENDPOINT: "https://your-proxmox-server.example.com:8006"
  PVE_TOKEN_ID: "virtrigaud@pve!automation"  
  PVE_TOKEN_SECRET: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"

---
# values.yaml - Reference the secret
providers:
  proxmox:
    enabled: true
    env:
      - name: PVE_ENDPOINT
        valueFrom:
          secretKeyRef:
            name: proxmox-credentials
            key: PVE_ENDPOINT
      - name: PVE_TOKEN_ID
        valueFrom:
          secretKeyRef:
            name: proxmox-credentials
            key: PVE_TOKEN_ID
      - name: PVE_TOKEN_SECRET
        valueFrom:
          secretKeyRef:
            name: proxmox-credentials
            key: PVE_TOKEN_SECRET
```

### Configuration Validation

The provider validates configuration at startup and will **fail to start** if:

- ✅ `PVE_ENDPOINT` is missing or invalid
- ✅ Neither username/password nor token credentials are provided
- ✅ Proxmox server is unreachable
- ✅ Authentication fails

#### Error Examples

```bash
# Missing endpoint
ERROR Failed to create PVE client error="endpoint is required"

# Invalid endpoint format  
ERROR Failed to create PVE client error="invalid endpoint URL"

# Authentication failure
ERROR Failed to authenticate error="authentication failed: invalid credentials"

# Connection failure
ERROR Failed to connect error="dial tcp: no route to host"
```

### Development vs Production

| Environment | Endpoint | Authentication | TLS | Notes |
|-------------|----------|---------------|-----|-------|
| **Development** | `https://pve-test.local:8006` | Username/Password | Skip verify | Use `PVE_INSECURE_SKIP_VERIFY=true` |
| **Staging** | `https://pve-staging.company.com:8006` | API Token | Custom CA | Configure CA bundle |
| **Production** | `https://pve.company.com:8006` | API Token | Valid cert | Use Kubernetes secrets |

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

### Required Environment Variables

**⚠️ The provider requires environment variables to connect to Proxmox VE:**

| Variable | Description | Required | Default | Example |
|----------|-------------|----------|---------|---------|
| `PVE_ENDPOINT` | Proxmox API endpoint URL | **Yes** | - | `https://pve.example.com:8006/api2` |
| `PVE_TOKEN_ID` | API token identifier | Yes* | - | `virtrigaud@pve!vrtg-token` |
| `PVE_TOKEN_SECRET` | API token secret | Yes* | - | `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` |
| `PVE_USERNAME` | Username for session auth | Yes* | - | `virtrigaud@pve` |
| `PVE_PASSWORD` | Password for session auth | Yes* | - | `secure-password` |
| `PVE_NODE_SELECTOR` | Preferred nodes (comma-separated) | No | Auto-detect | `pve-node-1,pve-node-2` |
| `PVE_INSECURE_SKIP_VERIFY` | Skip TLS verification | No | `false` | `true` |
| `PVE_CA_BUNDLE` | Custom CA certificate | No | - | `-----BEGIN CERTIFICATE-----...` |

\* Either token (`PVE_TOKEN_ID` + `PVE_TOKEN_SECRET`) or username/password (`PVE_USERNAME` + `PVE_PASSWORD`) is required

### Deployment Configuration

The provider needs environment variables to connect to Proxmox. Here are complete deployment examples:

#### Using Helm Values

```yaml
# values.yaml
providers:
  proxmox:
    enabled: true
    env:
      - name: PVE_ENDPOINT
        value: "https://pve.example.com:8006/api2"
      - name: PVE_TOKEN_ID
        value: "virtrigaud@pve!vrtg-token"
      - name: PVE_TOKEN_SECRET
        value: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
      - name: PVE_INSECURE_SKIP_VERIFY
        value: "true"  # Only for development!
      - name: PVE_NODE_SELECTOR
        value: "pve-node-1,pve-node-2"  # Optional
```

#### Using Kubernetes Secrets (Recommended)

```yaml
# Create secret with credentials
apiVersion: v1
kind: Secret
metadata:
  name: proxmox-credentials
  namespace: virtrigaud-system
type: Opaque
stringData:
  PVE_ENDPOINT: "https://pve.example.com:8006/api2"
  PVE_TOKEN_ID: "virtrigaud@pve!vrtg-token"
  PVE_TOKEN_SECRET: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
  PVE_INSECURE_SKIP_VERIFY: "false"

---
# Reference secret in deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: virtrigaud-provider-proxmox
spec:
  template:
    spec:
      containers:
      - name: provider-proxmox
        image: ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.0
        envFrom:
        - secretRef:
            name: proxmox-credentials
```

#### Development/Testing Configuration

```yaml
# For development with a local Proxmox VE instance
providers:
  proxmox:
    enabled: true
    env:
      - name: PVE_ENDPOINT
        value: "https://192.168.1.100:8006/api2"
      - name: PVE_USERNAME
        value: "root@pam"
      - name: PVE_PASSWORD
        value: "your-password"
      - name: PVE_INSECURE_SKIP_VERIFY
        value: "true"
```

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

## Troubleshooting

### Common Issues

#### ❌ "endpoint is required" Error

**Symptom**: Provider pod crashes with `ERROR Failed to create PVE client error="endpoint is required"`

**Cause**: Missing or empty `PVE_ENDPOINT` environment variable

**Solution**:
```yaml
# Ensure PVE_ENDPOINT is set in deployment
env:
  - name: PVE_ENDPOINT
    value: "https://your-proxmox.example.com:8006/api2"
```

#### ❌ Connection Timeout/Refused

**Symptom**: Provider fails with connection timeouts or "connection refused"

**Cause**: Network connectivity issues or wrong endpoint URL

**Solutions**:
1. **Verify endpoint**: Test from a pod in the cluster:
   ```bash
   kubectl run test-curl --rm -i --tty --image=curlimages/curl -- \
     curl -k https://your-proxmox.example.com:8006/api2/json/version
   ```

2. **Check firewall**: Ensure port 8006 is accessible from Kubernetes cluster

3. **Verify URL format**: Should be `https://hostname:8006/api2` (note the `/api2` path)

#### ❌ TLS Certificate Errors

**Symptom**: `x509: certificate signed by unknown authority`

**Solutions**:
- **Development**: Set `PVE_INSECURE_SKIP_VERIFY=true` (not for production!)
- **Production**: Provide valid TLS certificates or CA bundle

#### ❌ Authentication Failures

**Symptom**: `401 Unauthorized` or `authentication failure`

**Solutions**:
1. **Verify token permissions**:
   ```bash
   # Test API token manually
   curl -k "https://pve.example.com:8006/api2/json/version" \
     -H "Authorization: PVEAPIToken=USER@REALM!TOKENID=SECRET"
   ```

2. **Check user privileges**: Ensure user has VM management permissions
3. **Verify token format**: Should be `user@realm!tokenid` (note the `!`)

#### ❌ Provider Not Starting

**Symptom**: Pod in `CrashLoopBackOff` or `0/1 Ready`

**Diagnostic Steps**:
```bash
# Check pod logs
kubectl logs -n virtrigaud-system deployment/virtrigaud-provider-proxmox

# Check environment variables
kubectl describe pod -n virtrigaud-system -l app.kubernetes.io/component=provider-proxmox

# Verify configuration
kubectl get secret proxmox-credentials -o yaml
```

### Validation Commands

Test your Proxmox connection before deploying:

```bash
# 1. Test network connectivity
telnet your-proxmox.example.com 8006

# 2. Test API endpoint
curl -k https://your-proxmox.example.com:8006/api2/json/version

# 3. Test authentication
curl -k "https://your-proxmox.example.com:8006/api2/json/nodes" \
  -H "Authorization: PVEAPIToken=USER@REALM!TOKENID=SECRET"

# 4. Test from within cluster
kubectl run debug --rm -i --tty --image=curlimages/curl -- sh
# Then run curl commands from inside the pod
```

### Debug Logging

Enable verbose logging for the provider:

```yaml
providers:
  proxmox:
    env:
      - name: LOG_LEVEL
        value: "debug"
      - name: PVE_ENDPOINT
        value: "https://pve.example.com:8006/api2"
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
