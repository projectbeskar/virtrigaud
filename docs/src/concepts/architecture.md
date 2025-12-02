# Architecture Overview

VirtRigaud's architecture and design principles.

## Design Goals

1. **Kubernetes-native** - Use CRDs and controllers
2. **Multi-hypervisor** - Support vSphere, Libvirt, Proxmox
3. **Scalable** - Handle thousands of VMs
4. **Extensible** - Easy to add new providers
5. **Secure** - Zero-trust security model

## High-Level Architecture

```mermaid
graph TB
    subgraph "Kubernetes Cluster"
        subgraph "Custom Resources"
            VM[VirtualMachine]
            PR[Provider]
            VMC[VMClass]
        end

        subgraph "VirtRigaud Controller"
            CTRL[Controller Manager<br/>Reconciliation Engine]
            PF[Provider Factory]
        end

        subgraph "Providers"
            VSP[vSphere Provider]
            LVP[Libvirt Provider]
            PXP[Proxmox Provider]
        end
    end

    subgraph "Hypervisors"
        VC[vCenter]
        KVM[KVM/Libvirt]
        PVE[Proxmox VE]
    end

    subgraph "Virtual Machines"
        VM1[VM Instances]
    end

    VM --> CTRL
    PR --> CTRL
    VMC --> CTRL

    CTRL --> PF
    PF --> VSP
    PF --> LVP
    PF --> PXP

    VSP -.API.-> VC
    LVP -.API.-> KVM
    PXP -.API.-> PVE

    VC --> VM1
    KVM --> VM1
    PVE --> VM1

    style VM fill:#e1f5ff
    style PR fill:#e1f5ff
    style VMC fill:#e1f5ff
    style CTRL fill:#d4e8d4
    style PF fill:#ffd4d4
    style VSP fill:#e8d4ff
    style LVP fill:#e8d4ff
    style PXP fill:#e8d4ff
```

## Components

### Controller Manager

Core reconciliation logic:
- Watches CRD changes
- Reconciles desired vs actual state
- Updates resource status
- Manages finalizers

### Providers

Provider implementations:
- **In-process** - Compiled into controller
- **Remote** - Separate pods via gRPC

See [Remote Providers](../remote-providers.md).

### CRDs

Kubernetes Custom Resource Definitions:
- VirtualMachine
- Provider
- VMClass
- VMImage
- VMMigration
- VMSet
- VMPlacementPolicy

See [CRDs](../crds.md).

## Data Flow

```mermaid
sequenceDiagram
    participant U as User
    participant K as Kubernetes API
    participant C as Controller
    participant P as Provider
    participant H as Hypervisor

    U->>K: kubectl apply VirtualMachine
    K->>C: Watch Event
    C->>K: Get VirtualMachine
    C->>K: Get Provider
    C->>K: Get VMClass
    C->>P: CreateVM(spec)
    P->>H: Create VM via API
    H-->>P: VM Created
    P-->>C: VMStatus
    C->>K: Update VM Status
    K-->>U: VM Ready
```

## Reconciliation Loop

```mermaid
stateDiagram-v2
    [*] --> Watch: Controller starts
    Watch --> Queue: Event received
    Queue --> Reconcile: Dequeue item
    Reconcile --> GetResources: Fetch CR, Provider, VMClass
    GetResources --> CompareState: Get current state from provider
    CompareState --> NoChange: Desired = Actual
    CompareState --> Create: VM doesn't exist
    CompareState --> Update: VM exists, spec changed
    CompareState --> Delete: VM being deleted

    NoChange --> UpdateStatus: Update conditions
    Create --> CreateVM: Call provider.CreateVM()
    Update --> UpdateVM: Call provider.UpdateVM()
    Delete --> Finalizer: Check finalizer

    CreateVM --> UpdateStatus
    UpdateVM --> UpdateStatus

    Finalizer --> DeleteVM: Finalizer present
    Finalizer --> RemoveFinalizer: VM deleted from hypervisor

    DeleteVM --> RemoveFinalizer
    RemoveFinalizer --> [*]

    UpdateStatus --> Requeue: Requeue after 5m
    Requeue --> Watch
```

The reconciliation process:

1. **Watch** - Monitor CRD changes via Kubernetes API
2. **Queue** - Add reconciliation requests to work queue
3. **Fetch** - Get VirtualMachine, Provider, VMClass resources
4. **Compare** - Fetch current state from provider and compare with desired
5. **Execute** - Create, update, or delete VM as needed
6. **Update Status** - Set status conditions reflecting current state
7. **Requeue** - Schedule next reconciliation (default: 5 minutes)

See [Status Update Logic](status-update-logic.md) for detailed status management.

## Provider Architecture

See [Provider Architecture](../providers.md) for details on the provider abstraction layer.

## Security Model

- mTLS for provider communication
- Kubernetes RBAC for access control
- Secret-based credential storage
- Network policies for isolation

See [Security](../security.md).
