# virtrigaud

A Kubernetes operator for managing virtual machines across multiple hypervisors.

## Overview

Virtrigaud is a Kubernetes operator that enables declarative management of virtual machines across different hypervisor platforms. It provides a unified API for provisioning and managing VMs on vSphere, Libvirt/KVM, and other hypervisors through a clean provider interface.

## Features

- **Multi-Hypervisor Support**: Manage VMs across vSphere, Libvirt/KVM, and more
- **Declarative API**: Define VM resources using Kubernetes CRDs
- **Provider Interface**: Clean, extensible interface for adding new hypervisors
- **Cloud-Init Support**: Initialize VMs with cloud-init configuration
- **Network Management**: Configure VM networking with provider-specific settings
- **Power Management**: Control VM power state (On/Off)
- **Finalizer-based Cleanup**: Ensures proper cleanup of external resources

## Architecture

```
┌─────────────────┐    ┌───────────────┐    ┌─────────────────┐
│  VirtualMachine │    │   VMClass     │    │     VMImage     │
│      CRD        │    │     CRD       │    │      CRD        │
└─────────────────┘    └───────────────┘    └─────────────────┘
         │                       │                       │
         └───────────────────────┼───────────────────────┘
                                 │
                    ┌─────────────────┐
                    │   Controller    │
                    │   (Reconciler)  │
                    └─────────────────┘
                                 │
                    ┌─────────────────┐
                    │    Provider     │
                    │   Interface     │
                    └─────────────────┘
                         │       │
              ┌──────────┘       └──────────┐
    ┌─────────────────┐           ┌─────────────────┐
    │    vSphere      │           │    Libvirt     │
    │   Provider      │           │   Provider     │
    └─────────────────┘           └─────────────────┘
```

## Quick Start

1. **Install the CRDs**:
   ```bash
   make install
   ```

2. **Run the controller**:
   ```bash
   make run
   ```

3. **Create a provider secret**:
   ```bash
   kubectl apply -f examples/secrets/vsphere-creds.yaml
   ```

4. **Create provider and VM resources**:
   ```bash
   kubectl apply -f examples/provider-vsphere.yaml
   kubectl apply -f examples/vmclass-small.yaml
   kubectl apply -f examples/vmimage-ubuntu.yaml
   kubectl apply -f examples/vmnetwork-app.yaml
   kubectl apply -f examples/vm-ubuntu-small.yaml
   ```

## CRDs

- **VirtualMachine**: Represents a virtual machine instance
- **VMClass**: Defines resource allocation (CPU, memory, etc.)
- **VMImage**: References base templates/images
- **VMNetworkAttachment**: Defines network configurations
- **Provider**: Configures hypervisor connection details

## Supported Providers

- **vSphere**: Production ready
- **Libvirt**: Planned for Stage 2
- **Firecracker**: Future roadmap
- **QEMU**: Future roadmap

## Development

### Prerequisites

- Go 1.22+
- Docker
- kubectl
- A Kubernetes cluster

### Building

```bash
# Build the manager binary
make build

# Build the container image
make docker-build

# Run tests
make test

# Generate code and manifests
make generate manifests
```

### Running locally

```bash
# Install CRDs
make install

# Run the controller
make run
```

## Documentation

- [CRD Reference](docs/CRDs.md)
- [Provider Development](docs/PROVIDERS.md)
- [Examples](docs/EXAMPLES.md)

## Contributing

Contributions are welcome! Please see our [contribution guidelines](CONTRIBUTING.md).

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.