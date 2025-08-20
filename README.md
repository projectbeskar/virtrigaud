# virtrigaud

A Kubernetes operator for managing virtual machines across multiple hypervisors.

## Overview

Virtrigaud is a Kubernetes operator that enables declarative management of virtual machines across different hypervisor platforms. It provides a unified API for provisioning and managing VMs on vSphere, Libvirt/KVM, and other hypervisors through a clean provider interface.

## Features

- **Multi-Hypervisor Support**: Manage VMs across vSphere (ready), Libvirt/KVM (planned), and more
- **Declarative API**: Define VM resources using Kubernetes CRDs
- **Production-Ready vSphere Provider**: Full vSphere integration with govmomi
- **Cloud-Init Support**: Initialize VMs with cloud-init configuration
- **Network Management**: Configure VM networking with provider-specific settings
- **Power Management**: Control VM power state (On/Off/Reboot)
- **Async Task Support**: Handles long-running vSphere operations
- **Resource Management**: CPU, memory, disk configuration and reconfiguration
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

3. **Create provider and VM resources**:
   ```bash
   # Complete example with all components
   kubectl apply -f examples/complete-example.yaml
   
   # Or step by step:
   kubectl create secret generic vsphere-creds \
     --from-literal=username=administrator@vsphere.local \
     --from-literal=password=your-password
   kubectl apply -f examples/provider-vsphere.yaml
   kubectl apply -f examples/vmclass-small.yaml
   kubectl apply -f examples/vmimage-ubuntu.yaml
   kubectl apply -f examples/vmnetwork-app.yaml
   kubectl apply -f examples/vm-ubuntu-small.yaml
   ```

4. **Monitor VM creation**:
   ```bash
   kubectl get virtualmachine -w
   ```

For detailed instructions, see [QUICKSTART.md](QUICKSTART.md).

## CRDs

- **VirtualMachine**: Represents a virtual machine instance
- **VMClass**: Defines resource allocation (CPU, memory, etc.)
- **VMImage**: References base templates/images
- **VMNetworkAttachment**: Defines network configurations
- **Provider**: Configures hypervisor connection details

## Supported Providers

- **vSphere**: ✅ Production ready (govmomi-based)
  - VM creation from templates
  - Power management (On/Off/Reboot)
  - Resource configuration (CPU/Memory/Disks)
  - Cloud-init support via guestinfo
  - Network configuration with portgroups
  - Async task monitoring
- **Libvirt**: 🚧 Planned for Stage 2
- **Firecracker**: 📋 Future roadmap
- **QEMU**: 📋 Future roadmap

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

- [Quick Start Guide](QUICKSTART.md) - Get started in 5 minutes
- [CRD Reference](docs/CRDs.md) - Complete API documentation
- [Examples](docs/EXAMPLES.md) - Practical examples and use cases
- [Provider Development](docs/PROVIDERS.md) - How to add new hypervisors

## Contributing

Contributions are welcome! Please see our [contribution guidelines](CONTRIBUTING.md).

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.