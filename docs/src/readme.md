# VirtRigaud Documentation

Welcome to the VirtRigaud documentation. VirtRigaud is a Kubernetes operator for managing virtual machines across multiple hypervisors including vSphere, Libvirt/KVM, and Proxmox VE.

## Quick Navigation

### Getting Started
- [15-Minute Quickstart](getting-started/quickstart.md) - Get up and running quickly
- [Installation Guide](install-helm-only.md) - Helm installation instructions
- [Helm CRD Upgrades](helm-crd-upgrades.md) - Managing CRD updates

### Core Documentation
- [Custom Resource Definitions](crds.md) - Complete API reference
- [Examples](examples.md) - Practical configuration examples
- [Provider Documentation](providers.md) - Provider development guide
- [Provider Capabilities Matrix](providers-capabilities.md) - Feature comparison

### Provider-Specific Guides
- [vSphere Provider](providers/vsphere.md) - VMware vCenter/ESXi integration
- [Libvirt Provider](providers/libvirt.md) - KVM/QEMU virtualization
- [Proxmox VE Provider](providers/proxmox.md) - Proxmox Virtual Environment
- [Provider Tutorial](providers/tutorial.md) - Build your own provider
- [Provider Versioning](providers/versioning.md) - Version management

### Advanced Features
- [VM Lifecycle Management](advanced-lifecycle.md) - Advanced VM operations
- [Nested Virtualization](nested-virtualization.md) - Run hypervisors in VMs
- [Graceful Shutdown](graceful-shutdown.md) - Proper VM shutdown handling
- [VM Snapshots](advanced-lifecycle.md#snapshots) - Backup and restore
- [Remote Providers](remote-providers.md) - Provider architecture

### Operations & Administration
- [Observability](observability.md) - Monitoring and metrics
- [Security](security.md) - Security best practices
- [Resilience](resilience.md) - High availability and fault tolerance
- [Upgrade Guide](upgrade.md) - Version upgrade procedures
- [vSphere Hardware Versions](vsphere-hardware-version.md) - Hardware compatibility

### Security Configuration
- [Bearer Token Authentication](providers/security/bearer-token.md)
- [mTLS Configuration](providers/security/mtls.md)
- [External Secrets](providers/security/external-secrets.md)
- [Network Policies](providers/security/network-policies.md)

### API Reference
- [CLI Tools Reference](cli.md) - Command-line interface guide
- [CLI API Reference](api-reference/cli.md) - Detailed CLI documentation
- [Metrics Catalog](api-reference/metrics.md) - Available metrics
- [Provider Catalog](catalog.md) - Available providers

### Development
- [Testing Workflows Locally](testing-workflows-locally.md) - Local CI/CD testing
- [Contributing](../CONTRIBUTING.md) - Contribution guidelines
- [Development Guide](../DEVELOPMENT.md) - Developer setup

### Examples Directory
- [Example README](examples/README.md) - Overview of all examples
- [Complete Examples](examples/) - Working configuration files
- [Advanced Examples](examples/advanced/) - Complex scenarios
- [Security Examples](examples/security/) - Security configurations

## Version Information

This documentation covers **VirtRigaud v0.2.3**.

### Recent Changes
- **v0.2.3**: Provider feature parity - Reconfigure, Clone, TaskStatus, ConsoleURL
- **v0.2.2**: Nested virtualization, TPM support, snapshot management
- **v0.2.1**: Critical fixes and documentation updates
- **v0.2.0**: Production-ready vSphere and Libvirt providers

See [CHANGELOG.md](../CHANGELOG.md) for complete version history.

## Provider Status

| Provider | Status | Maturity | Documentation |
|----------|--------|----------|---------------|
| vSphere | Production Ready | Stable | [Guide](providers/vsphere.md) |
| Libvirt/KVM | Production Ready | Stable | [Guide](providers/libvirt.md) |
| Proxmox VE | Production Ready | Beta | [Guide](providers/proxmox.md) |
| Mock | Complete | Testing | [providers.md](providers.md) |

## Support

- **GitHub Issues**: [github.com/projectbeskar/virtrigaud/issues](https://github.com/projectbeskar/virtrigaud/issues)
- **Discussions**: [github.com/projectbeskar/virtrigaud/discussions](https://github.com/projectbeskar/virtrigaud/discussions)
- **Slack**: #virtrigaud on Kubernetes Slack

## Quick Links

- [Main README](../README.md) - Project overview
- [CHANGELOG](../CHANGELOG.md) - Version history
- [Contributing](../CONTRIBUTING.md) - How to contribute
- [License](../LICENSE) - Apache License 2.0
