# Virtrigaud Documentation

Welcome to the Virtrigaud documentation. Virtrigaud is a Kubernetes operator for managing virtual machines across multiple hypervisors including vSphere and Libvirt/KVM.

## Quick Navigation

### Getting Started
- [15-Minute Quickstart](getting-started/quickstart.md)
- [Installation Guide](install-helm-only.md)
- [Your First VM](getting-started/quickstart.md)

### Core Concepts
- [Architecture Overview](concepts/architecture.md)
- [Providers and Remote Runtimes](concepts/providers.md)
- [Custom Resource Definitions](concepts/crds.md)
- [VM Lifecycle](concepts/vm-lifecycle.md)

### User Guides
- [Managing Virtual Machines](user-guides/virtual-machines.md)
- [VM Snapshots](user-guides/snapshots.md)
- [VM Cloning](user-guides/cloning.md)
- [VM Sets and Scaling](user-guides/vmsets.md)
- [Image Management](user-guides/images.md)
- [Network Configuration](user-guides/networking.md)

### Administration
- [Installation Options](admin-guides/installation.md)
- [Provider Configuration](admin-guides/providers.md)
- [Security and RBAC](admin-guides/security.md)
- [Monitoring and Observability](admin-guides/monitoring.md)
- [Backup and Recovery](admin-guides/backup.md)
- [Troubleshooting](admin-guides/troubleshooting.md)

### Security
- [Security Model](security/model.md)
- [TLS and mTLS Configuration](security/tls.md)
- [Secrets Management](security/secrets.md)
- [Network Policies](security/network-policies.md)
- [Supply Chain Security](security/supply-chain.md)

### API Reference
- [Custom Resource Definitions](CRDs.md)
- [CLI Reference](api-reference/cli.md)
- [Metrics Catalog](api-reference/metrics.md)
- [Provider Capabilities Matrix](PROVIDERS_CAPABILITIES.md)

### Developer Resources
- [Provider Development Guide](developer/provider-guide.md)
- [Contributing](../CONTRIBUTING.md)
- [Architecture Deep Dive](developer/architecture.md)

## Version Information

This documentation covers Virtrigaud v0.2.0.

## Support

- GitHub Issues: [github.com/projectbeskar/virtrigaud/issues](https://github.com/projectbeskar/virtrigaud/issues)
- Discussions: [github.com/projectbeskar/virtrigaud/discussions](https://github.com/projectbeskar/virtrigaud/discussions)
- Slack: #virtrigaud on Kubernetes Slack
