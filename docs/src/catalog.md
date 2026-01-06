# Provider Catalog

*Last updated: 2025-08-26T14:30:00Z*

The VirtRigaud Provider Catalog lists all verified and community providers available for the VirtRigaud virtualization management platform. All providers in this catalog have been tested for conformance and compatibility.

## Provider Overview

| Provider | Description | Capabilities | Conformance | Maintainer | License |
|----------|-------------|--------------|-------------|------------|----------|
| **Mock Provider** | A mock provider for testing and demonstrations | core, snapshot, clone, image-prepare, advanced | ![Conformance](https://img.shields.io/badge/conformance-pass-green) | virtrigaud@projectbeskar.com | Apache-2.0 |
| **vSphere Provider** | VMware vSphere provider for VirtRigaud | core, snapshot, clone, advanced | ![Conformance](https://img.shields.io/badge/conformance-pass-green) | virtrigaud@projectbeskar.com | Apache-2.0 |
| **Libvirt Provider** | Libvirt/KVM provider for VirtRigaud | core, snapshot, clone | ![Conformance](https://img.shields.io/badge/conformance-partial-yellow) | virtrigaud@projectbeskar.com | Apache-2.0 |

## Quick Start

### Installing a Provider

To install a provider in your Kubernetes cluster, use the VirtRigaud provider runtime Helm chart:

```bash
# Install a provider using the runtime chart from OCI registry
# No authentication required - charts are publicly accessible
helm install my-vsphere-provider oci://ghcr.io/projectbeskar/virtrigaud/charts/virtrigaud-provider-runtime \
  --namespace vsphere-providers \
  --create-namespace \
  --set image.repository=ghcr.io/projectbeskar/virtrigaud/provider-vsphere \
  --set image.tag=0.1.1 \
  --set env[0].name=VSPHERE_SERVER \
  --set env[0].value=vcenter.example.com
```

### Provider Discovery

Once installed, providers automatically register with the VirtRigaud manager. You can list available providers:

```bash
kubectl get providers -n virtrigaud-system
```

## Provider Details

### Mock Provider

- **Image:** `ghcr.io/projectbeskar/virtrigaud/provider-mock:0.1.1`
- **Repository:** [https://github.com/projectbeskar/virtrigaud](https://github.com/projectbeskar/virtrigaud)
- **Maturity:** stable
- **Tags:** testing, development, demo
- **Documentation:** [https://projectbeskar.github.io/virtrigaud/providers/mock/](https://projectbeskar.github.io/virtrigaud/providers/mock/)

The mock provider is perfect for:
- Testing VirtRigaud functionality
- Development and CI/CD pipelines
- Learning provider concepts
- Demonstrating VirtRigaud capabilities

**Installation:**
```bash
helm install mock-provider virtrigaud/virtrigaud-provider-runtime \
  --namespace development \
  --create-namespace \
  --set image.repository=ghcr.io/projectbeskar/virtrigaud/provider-mock \
  --set image.tag=0.1.1 \
  --set env[0].name=LOG_LEVEL \
  --set env[0].value=debug
```

### vSphere Provider

- **Image:** `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:0.1.1`
- **Repository:** [https://github.com/projectbeskar/virtrigaud](https://github.com/projectbeskar/virtrigaud)
- **Maturity:** beta
- **Tags:** vmware, vsphere, enterprise
- **Documentation:** [https://projectbeskar.github.io/virtrigaud/providers/vsphere/](https://projectbeskar.github.io/virtrigaud/providers/vsphere/)

The vSphere provider enables VirtRigaud to manage VMware vSphere environments, including:
- VM lifecycle management (create, update, delete)
- Power operations (on, off, restart, suspend)
- Snapshot management
- VM cloning and templates
- Resource allocation and configuration

**Prerequisites:**
- VMware vSphere 6.7 or later
- vCenter Server credentials
- Network connectivity to vCenter API

**Installation:**
```bash
# Create secret for vSphere credentials
kubectl create secret generic vsphere-credentials \
  --namespace vsphere-providers \
  --from-literal=username=your-username \
  --from-literal=password=your-password

# Install provider
helm install vsphere-provider virtrigaud/virtrigaud-provider-runtime \
  --namespace vsphere-providers \
  --create-namespace \
  --set image.repository=ghcr.io/projectbeskar/virtrigaud/provider-vsphere \
  --set image.tag=0.1.1 \
  --set env[0].name=VSPHERE_SERVER \
  --set env[0].value=vcenter.example.com \
  --set env[1].name=VSPHERE_USERNAME \
  --set env[1].valueFrom.secretKeyRef.name=vsphere-credentials \
  --set env[1].valueFrom.secretKeyRef.key=username \
  --set env[2].name=VSPHERE_PASSWORD \
  --set env[2].valueFrom.secretKeyRef.name=vsphere-credentials \
  --set env[2].valueFrom.secretKeyRef.key=password
```

### Libvirt Provider

- **Image:** `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:0.1.1`
- **Repository:** [https://github.com/projectbeskar/virtrigaud](https://github.com/projectbeskar/virtrigaud)
- **Maturity:** beta
- **Tags:** libvirt, kvm, qemu, open-source
- **Documentation:** [https://projectbeskar.github.io/virtrigaud/providers/libvirt/](https://projectbeskar.github.io/virtrigaud/providers/libvirt/)

The libvirt provider manages KVM/QEMU virtual machines through libvirt, supporting:
- VM lifecycle management
- Power state control
- Snapshot operations
- Basic cloning capabilities
- Local and remote libvirt connections

**Prerequisites:**
- Libvirt daemon running on target hosts
- SSH access for remote connections
- Shared storage for multi-host deployments

**Installation:**
```bash
helm install libvirt-provider virtrigaud/virtrigaud-provider-runtime \
  --namespace libvirt-providers \
  --create-namespace \
  --set image.repository=ghcr.io/projectbeskar/virtrigaud/provider-libvirt \
  --set image.tag=0.1.1 \
  --set env[0].name=LIBVIRT_URI \
  --set env[0].value=qemu:///system \
  --set securityContext.runAsUser=0 \
  --set podSecurityContext.runAsUser=0
```

## Capability Profiles

VirtRigaud defines several capability profiles that providers can implement:

### Core Profile
**Required for all providers**
- `vm.create` - Create virtual machines
- `vm.read` - Get virtual machine information
- `vm.update` - Update virtual machine configuration
- `vm.delete` - Delete virtual machines
- `vm.power` - Control power state (on/off/restart)
- `vm.list` - List virtual machines

### Snapshot Profile
**Optional - for providers supporting VM snapshots**
- `vm.snapshot.create` - Create VM snapshots
- `vm.snapshot.list` - List VM snapshots
- `vm.snapshot.delete` - Delete VM snapshots
- `vm.snapshot.restore` - Restore VM from snapshot

### Clone Profile
**Optional - for providers supporting VM cloning**
- `vm.clone` - Clone virtual machines
- `vm.template` - Create and manage VM templates

### Image Prepare Profile
**Optional - for providers with image management**
- `image.prepare` - Prepare VM images
- `image.list` - List available images
- `image.upload` - Upload custom images

### Advanced Profile
**Optional - for advanced provider features**
- `vm.migrate` - Live migrate VMs between hosts
- `vm.resize` - Dynamic resource allocation
- `vm.backup` - Backup and restore operations
- `vm.monitoring` - Advanced monitoring and metrics

## Contributing a Provider

Want to add your provider to the catalog? Follow these steps:

### 1. Develop Your Provider

Use the [Provider Developer Tutorial](./providers/tutorial.md) to create your provider using the VirtRigaud SDK.

### 2. Ensure Conformance

Run the VirtRigaud Conformance Test Suite (VCTS) to verify your provider meets the requirements:

```bash
# Install the VCTS tool
go install github.com/projectbeskar/virtrigaud/cmd/vcts@latest

# Run conformance tests
vcts run --provider-endpoint=localhost:9443 --profile=core
```

### 3. Publish to Catalog

Use the `vrtg-provider publish` command to submit your provider:

```bash
vrtg-provider publish \
  --name your-provider \
  --image ghcr.io/yourorg/your-provider \
  --tag v1.0.0 \
  --repo https://github.com/yourorg/your-provider \
  --maintainer your-email@example.com \
  --license Apache-2.0
```

This will:
1. Run conformance tests
2. Generate provider badges
3. Create a catalog entry
4. Open a pull request to add your provider

### 4. Catalog Requirements

To be included in the catalog, providers must:

- ✅ Pass VCTS core profile tests
- ✅ Include comprehensive documentation
- ✅ Provide Helm chart for deployment
- ✅ Follow security best practices
- ✅ Include proper error handling
- ✅ Support health checks and metrics
- ✅ Have active maintenance and support

## Provider Support Matrix

| Provider | Kubernetes | VirtRigaud | Go Version | Platforms |
|----------|------------|------------|------------|-----------|
| Mock | 1.25+ | 0.1.0+ | 1.23+ | linux/amd64, linux/arm64 |
| vSphere | 1.25+ | 0.1.0+ | 1.23+ | linux/amd64, linux/arm64 |
| Libvirt | 1.25+ | 0.1.0+ | 1.23+ | linux/amd64 |

## Community and Support

- **Documentation:** [VirtRigaud Provider Docs](https://projectbeskar.github.io/virtrigaud/providers/)
- **Issues:** [GitHub Issues](https://github.com/projectbeskar/virtrigaud/issues)
- **Discussions:** [GitHub Discussions](https://github.com/projectbeskar/virtrigaud/discussions)
- **Slack:** [VirtRigaud Community](https://virtrigaud.slack.com)

## Versioning and Compatibility

Providers follow semantic versioning (SemVer) and maintain compatibility with VirtRigaud versions:

- **Major versions** (1.0.0 → 2.0.0): Breaking changes to APIs or behavior
- **Minor versions** (1.0.0 → 1.1.0): New features, backward compatible
- **Patch versions** (1.0.0 → 1.0.1): Bug fixes, security updates

**Compatibility Policy:**
- Current VirtRigaud version supports providers from current major version
- Providers should support at least 2 minor versions of VirtRigaud
- Breaking changes require migration documentation

## License and Legal

All providers in this catalog are open source and follow the licensing terms specified in their individual repositories. The catalog itself is maintained under the Apache 2.0 license.

**Trademark Notice:** VMware and vSphere are trademarks of VMware, Inc. KVM and QEMU are trademarks of their respective owners. All trademarks are the property of their respective owners.

