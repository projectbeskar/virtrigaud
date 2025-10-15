# Examples

This document provides practical examples for using VirtRigaud with the Remote provider architecture.

## Quick Start Examples

All VirtRigaud providers now run as Remote providers. Here are the essential examples to get started:

### Basic Provider Setup

- **[vSphere Provider](examples/provider-vsphere.yaml)** - Basic vSphere provider configuration
- **[LibVirt Provider](examples/provider-libvirt.yaml)** - Basic LibVirt provider configuration

### Complete Working Examples

- **[Complete vSphere Setup](examples/complete-example.yaml)** - End-to-end vSphere VM creation
- **[Advanced vSphere Setup](examples/vsphere-advanced-example.yaml)** - Production-ready vSphere configuration
- **[LibVirt Complete Setup](examples/libvirt-complete-example.yaml)** - End-to-end LibVirt VM creation
- **[Multi-Provider Setup](examples/multi-provider-example.yaml)** - Using multiple providers together

### Individual Resource Examples

- **[VMClass](examples/vmclass-small.yaml)** - VM resource allocation template
- **[VMImage](examples/vmimage-ubuntu.yaml)** - VM image/template definition
- **[VMNetworkAttachment](examples/vmnetwork-app.yaml)** - Network configuration
- **[Simple VM](examples/vm-ubuntu-small.yaml)** - Basic virtual machine

### Advanced Examples

- **[Security Configuration](examples/security/)** - RBAC, network policies, external secrets
- **[Advanced Operations](examples/advanced/)** - Snapshots, reconfiguration, lifecycle management

## Example Directory Structure

```
docs/examples/
├── provider-*.yaml          # Provider configurations
├── complete-example.yaml    # Full working setup
├── *-advanced-example.yaml  # Production configurations
├── vm*.yaml                 # Individual resource definitions
├── advanced/                # Advanced operations
├── security/                # Security configurations
└── secrets/                 # Credential examples
```

## Key Changes from Previous Versions

### Remote-Only Architecture

All providers now run as separate pods with the Remote runtime:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: my-provider
spec:
  type: vsphere  # or libvirt, proxmox
  endpoint: https://vcenter.example.com
  credentialSecretRef:
    name: provider-creds
  runtime:
    mode: Remote              # Required - only mode supported
    image: "ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.3"
    service:
      port: 9090
```

### Current API Schema (v0.2.3)

- **VMClass**: Standard Kubernetes resource quantities (`cpus: 4`, `memory: "4Gi"`)
- **VMImage**: Provider-specific source configurations
- **VMNetworkAttachment**: Network provider abstractions
- **VirtualMachine**: Declarative power state management

### Configuration Management

Providers receive configuration through:
- **Endpoint**: Environment variable `PROVIDER_ENDPOINT`
- **Credentials**: Mounted secret files in `/etc/virtrigaud/credentials/`
- **Runtime**: Managed automatically by the provider controller

## Getting Started

1. **Choose your provider** from the basic examples above
2. **Create credentials secret** (see `examples/secrets/`)
3. **Apply provider configuration** with required `runtime` section
4. **Define VM resources** (VMClass, VMImage, VMNetworkAttachment)
5. **Create VirtualMachine** referencing your resources

For detailed setup instructions, see:
- [Getting Started Guide](getting-started/quickstart.md)
- [Remote Providers Documentation](REMOTE_PROVIDERS.md)
- [Provider-Specific Guides](providers/)

## Need Help?

- Check the [Remote Providers documentation](REMOTE_PROVIDERS.md) for architecture details
- Review [provider-specific guides](providers/) for setup instructions
- Look at [complete examples](examples/) for working configurations
- See [troubleshooting tips](getting-started/quickstart.md#troubleshooting) for common issues