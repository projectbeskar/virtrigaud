# Basic Concepts

Understanding VirtRigaud's core concepts and architecture.

## What is VirtRigaud?

VirtRigaud is a Kubernetes operator that manages virtual machines across multiple hypervisors using Kubernetes-native APIs.

## Key Concepts

### Custom Resources

VirtRigaud defines several Kubernetes Custom Resources:
- **VirtualMachine** - Represents a VM instance
- **Provider** - Hypervisor connection
- **VMClass** - VM resource template
- **VMImage** - VM image/template
- **VMMigration** - VM migration operations

See [Custom Resource Definitions](../crds.md) for details.

### Providers

Providers abstract hypervisor-specific operations:
- vSphere
- Libvirt/KVM
- Proxmox

See [Provider Architecture](../providers.md).

### Reconciliation

VirtRigaud uses the Kubernetes controller pattern:
1. User creates/updates a VirtualMachine CR
2. Controller detects the change
3. Controller reconciles actual state with desired state
4. Controller updates resource status

See [Status Update Logic](status-update-logic.md).

## Architecture

```
┌─────────────────────────────────────────┐
│         Kubernetes Cluster              │
│  ┌──────────────────────────────────┐   │
│  │   VirtRigaud Controller          │   │
│  │   - Reconciles VMs               │   │
│  │   - Manages providers            │   │
│  └──────────────────────────────────┘   │
│         │                  │             │
│    gRPC │                  │ gRPC        │
│         ▼                  ▼             │
│  ┌─────────────┐    ┌─────────────┐     │
│  │   vSphere   │    │   Libvirt   │     │
│  │   Provider  │    │   Provider  │     │
│  └─────────────┘    └─────────────┘     │
└─────────────────────────────────────────┘
         │                    │
         ▼                    ▼
   ┌─────────┐          ┌─────────┐
   │ vCenter │          │   KVM   │
   └─────────┘          └─────────┘
```

See [Architecture Overview](architecture.md) for more details.

## What's Next?

- [Architecture Overview](architecture.md)
- [Provider Architecture](../providers.md)
- [Status Update Logic](status-update-logic.md)
