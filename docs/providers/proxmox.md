# Proxmox VE Provider

The Proxmox VE provider enables VirtRigaud to manage virtual machines on Proxmox Virtual Environment (PVE) clusters using the native Proxmox API.

## Overview

This provider implements the VirtRigaud provider interface to manage VM lifecycle operations on Proxmox VE:

- **Create**: Create VMs from templates or ISO images with cloud-init support
- **Delete**: Remove VMs and associated resources
- **Power**: Start, stop, and reboot virtual machines
- **Describe**: Query VM state, IPs, and console access
- **Clone**: Create linked or full clones of existing VMs
- **Snapshot**: Create, delete, and revert VM snapshots (planned)
- **ImagePrepare**: Import and prepare VM templates (planned)

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

## Security Best Practices

1. **Use API Tokens**: Prefer API tokens over username/password
2. **Least Privilege**: Grant minimal required permissions
3. **TLS Verification**: Always verify certificates in production
4. **Secret Management**: Use Kubernetes secrets for credentials
5. **Network Policies**: Restrict provider network access
6. **Regular Rotation**: Rotate API tokens periodically

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
