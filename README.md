# VirtRigaud

A Kubernetes operator for managing virtual machines across multiple hypervisors.

## Overview

Virtrigaud is a Kubernetes operator that enables declarative management of virtual machines across different hypervisor platforms. It provides a unified API for provisioning and managing VMs on vSphere, Libvirt/KVM, Proxmox VE, and other hypervisors through a clean provider interface.

## Features

- **Multi-Hypervisor Support**: Manage VMs across vSphere, Libvirt/KVM, and Proxmox VE simultaneously
- **Declarative API**: Define VM resources using Kubernetes CRDs with stable v1beta1 API
- **Production-Ready Providers**: Full integration for vSphere (govmomi), Libvirt/KVM, and Proxmox VE
- **Cloud-Init Support**: Initialize VMs with cloud-init configuration across all providers
- **Network Management**: Configure VM networking with provider-specific settings
- **Power Management**: Control VM power state (On/Off/Reboot) uniformly
- **Async Task Support**: Handles long-running operations (vSphere) and synchronous operations (Libvirt)
- **Resource Management**: CPU, memory, disk configuration across hypervisors
- **Storage Management**: Provider-specific storage handling (datastores vs storage pools)
- **Finalizer-based Cleanup**: Ensures proper cleanup of external resources

## API Support

**API Version**: v1beta1 - The stable production-ready API. This is the only supported API version for virtrigaud.

All resources use the v1beta1 API with comprehensive OpenAPI validation and type safety.

## Architecture

VirtRigaud uses a **Remote Provider** architecture for optimal scalability and reliability:

### Key Benefits

- **Scalability**: Each provider runs as independent pods with dedicated resources
- **Reliability**: Provider failures don't affect the manager or other providers  
- **Security**: Provider credentials are isolated to their respective pods
- **Flexibility**: Scale providers independently based on workload demands
- **Maintainability**: Update providers without affecting the core manager
- **Multi-tenancy**: Different providers for different teams/environments
- **Updates**: Rolling updates of providers without manager downtime

### Architecture Overview Diagram

```mermaid
graph TB
    %% Kubernetes Cluster boundary
    subgraph "Kubernetes Cluster"
        
        %% CRDs
        subgraph "Custom Resources"
            VM[VirtualMachine CRD]
            VMC[VMClass CRD]
            VMI[VMImage CRD]
            PR[Provider CRD]
        end
        
        %% Controller
        CTRL[VirtRigaud Controller<br/>Manager]
        
        %% Remote Providers
        subgraph "Remote Providers (gRPC)"
            VSP[vSphere Provider<br/>Pod]
            LVP[Libvirt Provider<br/>Pod] 
            PXP[Proxmox Provider<br/>Pod]
        end
        
        %% Connections within cluster
        VM -.-> CTRL
        VMC -.-> CTRL
        VMI -.-> CTRL
        PR -.-> CTRL
        
        CTRL -->|gRPC/TLS| VSP
        CTRL -->|gRPC/TLS| LVP
        CTRL -->|gRPC/TLS| PXP
    end
    
    %% External Infrastructure
    subgraph "External Infrastructure"
        subgraph "vSphere Environment"
            VCENTER[vCenter Server]
            ESXI[ESXi Hosts]
        end
        
        subgraph "KVM Environment"
            LIBVIRT[Libvirt Hosts]
            QEMU[QEMU/KVM VMs]
        end
        
        subgraph "Proxmox Environment"
            PVE[Proxmox VE Cluster]
            NODES[PVE Nodes]
        end
    end
    
    %% External connections
    VSP -->|govmomi API| VCENTER
    LVP -->|libvirt API| LIBVIRT
    PXP -->|REST API| PVE
```

## Quick Start

### Installation via Helm (Recommended)

1. **Add the Helm repository**:
   ```bash
   helm repo add virtrigaud https://projectbeskar.github.io/virtrigaud
   helm repo update
   ```

2. **Install VirtRigaud**:
   ```bash
   # Basic installation (CRDs included automatically)
   helm install virtrigaud virtrigaud/virtrigaud -n virtrigaud-system --create-namespace
   
   # With custom values
   helm install virtrigaud virtrigaud/virtrigaud \
     -n virtrigaud-system --create-namespace \
     --set providers.vsphere.enabled=true \
     --set providers.libvirt.enabled=true \
     --set providers.proxmox.enabled=false \
     --version 0.2.0
   
   # Skip CRDs if already installed separately
   helm install virtrigaud virtrigaud/virtrigaud -n virtrigaud-system --create-namespace --skip-crds
   ```

3. **Verify the installation**:
   ```bash
   # Check pods
   kubectl get pods -n virtrigaud-system
   
   # Check CRDs
   kubectl get crd | grep virtrigaud-system
   
   # Verify API conversion is working
   kubectl get crd virtualmachines.infra.virtrigaud.io -o yaml | yq '.spec.conversion'
   ```

### Development Installation

1. **Install the CRDs**:
   ```bash
   make install
   ```

2. **Run the controller**:
   ```bash
   make run
   ```

### Using VirtRigaud

1. **Create a Provider** (one-time setup per hypervisor):
   ```bash
   # Create credentials secret
   kubectl create secret generic vsphere-creds \
     --from-literal=username=admin \
     --from-literal=password=yourpassword
   
   # Create Provider resource
   kubectl apply -f - <<EOF
   apiVersion: infra.virtrigaud.io/v1beta1
   kind: Provider
   metadata:
     name: vsphere-datacenter
     namespace: default
   spec:
     type: vsphere
     endpoint: "https://vcenter.example.com:443"
     credentialSecretRef:
       name: vsphere-creds
     runtime:
       mode: Remote
       image: "virtrigaud/provider-vsphere:latest"
       service:
         port: 9090
   EOF
   ```

   > 📚 **How it works**: VirtRigaud automatically translates your Provider configuration into command-line arguments and environment variables for the provider pod. See [Remote Provider Documentation](docs/REMOTE_PROVIDERS.md#configuration-flow-provider-resource--provider-pod) for details.

2. **Create VM resources using the Provider**:
   ```bash
   # Apply VM definition that references the provider
   kubectl apply -f examples/complete-example.yaml
   
   # Proxmox VE example (v1beta1 API)
   kubectl apply -f examples/proxmox-complete-example.yaml
   
   # Only v1beta1 API is supported as of v0.2.0
   
   # Multi-provider example (vSphere, Libvirt, and Proxmox)
   kubectl apply -f examples/multi-provider-example.yaml
   
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

2. **Monitor VM creation**:
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

### Production-Ready Providers

- **vSphere** (govmomi-based) - **Pre-release**
  - VM creation from templates
  - Power management (On/Off/Reboot)
  - Resource configuration (CPU/Memory/Disks)
  - Cloud-init support via guestinfo
  - Network configuration with portgroups
  - Async task monitoring
  
- **Libvirt/KVM** (libvirt-go-based) - **In Development**
  - VM creation from qcow2 images
  - Power management (On/Off/Reboot)  
  - Resource configuration (CPU/Memory/Disks)
  - Cloud-init support via nocloud ISO
  - Network configuration with bridges/networks
  - Storage pool and volume management

- **Proxmox VE** (REST API-based) - **In Development**
  - VM creation from templates or ISO
  - Power management (On/Off/Reboot)
  - **Hot-plug reconfiguration** (CPU/Memory/Disk)
  - **Snapshot management** (with memory state)
  - **Multi-NIC networking** with VLAN support
  - **Linked/Full cloning**
  - **Image import** from URLs
  - Cloud-init support with static IPs
  - Async task monitoring with jittered backoff

### Provider Feature Matrix

| Feature | vSphere | Libvirt | Proxmox | Notes |
|---------|---------|---------|---------|-------|
| **Core Operations** | ✅ | ✅ | ✅ | Create/Delete/Power/Describe |
| **Hot Reconfiguration** | ⚠️ Limited | ❌ | ✅ | CPU/Memory online changes |
| **Disk Expansion** | ✅ | ✅ | ✅ | Online disk growth |
| **Snapshots** | ✅ | ✅ | ✅ | VM state snapshots |
| **Memory Snapshots** | ✅ | ❌ | ✅ | Include RAM in snapshots |
| **Cloning** | ✅ | ✅ | ✅ | VM duplication |
| **Linked Clones** | ✅ | ❌ | ✅ | COW-based fast clones |
| **Image Import** | ❌ | ✅ | ✅ | Import from URLs/files |
| **Multi-NIC** | ✅ | ✅ | ✅ | Multiple network interfaces |
| **VLAN Support** | ✅ | ✅ | ✅ | 802.1Q VLAN tagging |
| **Static IPs** | ✅ | ✅ | ✅ | Cloud-init network config |
| **Remote Deployment** | ✅ | ✅ | ✅ | gRPC-based providers |

### Future Roadmap

- **Firecracker**: Serverless microVM support
- **QEMU**: Direct QEMU integration
- **Hyper-V**: Windows virtualization platform
- **OpenStack**: Cloud infrastructure integration

## Troubleshooting

### API Conversion Issues

As of v0.2.0, only v1beta1 API is supported. For migration from legacy v1alpha1:

1. **Verify conversion webhook is running**:
   ```bash
   kubectl get pods -n virtrigaud -l app=virtrigaud-webhook
   ```

2. **Check conversion webhook configuration**:
   ```bash
   kubectl get crd virtualmachines.infra.virtrigaud.io -o yaml | yq '.spec.conversion'
   ```

3. **Verify webhook service**:
   ```bash
   kubectl get svc -n virtrigaud virtrigaud-webhook
   ```

4. **Check webhook logs**:
   ```bash
   kubectl logs -n virtrigaud -l app=virtrigaud-webhook
   ```

### Missing CRDs

If CRDs are missing after Helm install:

1. **Check if CRDs were skipped**:
   ```bash
   helm get values virtrigaud -n virtrigaud | grep skip-crds
   ```

2. **Manually install CRDs**:
   ```bash
   kubectl apply -f charts/virtrigaud/crds/
   ```

3. **Re-install with CRDs**:
   ```bash
   helm uninstall virtrigaud -n virtrigaud-system
   helm install virtrigaud virtrigaud/virtrigaud -n virtrigaud-system --create-namespace
   ```

## Development

### Prerequisites

- Go 1.22+
- Docker
- kubectl
- A Kubernetes cluster

### Local Testing

Test GitHub Actions workflows locally before pushing to save costs and catch issues early:

```bash
# Setup local testing environment
./hack/test-workflows-locally.sh setup

# Quick lint check (run before every commit)
./hack/test-lint-locally.sh

# Comprehensive CI testing (run before PRs)
./hack/test-ci-locally.sh

# Test Helm charts with Kind cluster
./hack/test-helm-locally.sh

# Simulate release workflow
./hack/test-release-locally.sh v0.2.0-test
```

See [TESTING_WORKFLOWS_LOCALLY.md](TESTING_WORKFLOWS_LOCALLY.md) for detailed instructions.

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

- [Quick Start Guide](docs/getting-started/quickstart.md) - Get started in 15 minutes
- [CRD Reference](docs/CRDs.md) - Complete API documentation
- [Examples](docs/EXAMPLES.md) - Practical examples and use cases
- [CLI Tools Reference](docs/CLI.md) - Command-line interface guide
- [Upgrade Guide](docs/UPGRADE.md) - Version upgrade procedures
- [Provider Development](docs/PROVIDERS.md) - How to add new hypervisors
- [Provider Catalog](docs/catalog.md) - Browse available providers
- [Provider Tutorial](docs/providers/tutorial.md) - Complete provider development guide
- [Versioning & Breaking Changes](docs/providers/versioning.md)

### Provider-Specific Documentation

- [vSphere Provider](docs/providers/vsphere.md) - vCenter/ESXi integration
- [Libvirt Provider](docs/providers/libvirt.md) - KVM/QEMU virtualization  
- [Proxmox VE Provider](docs/providers/proxmox.md) - Proxmox Virtual Environment

## Contributing

Contributions are welcome! Please see our [contribution guidelines](CONTRIBUTING.md).

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.