# Architecture Diagrams

Comprehensive visual diagrams showing VirtRigaud's architecture, components, and data flows.

## System Architecture

```mermaid
graph TB
    subgraph "Kubernetes Cluster"
        subgraph "Custom Resources"
            VM[VirtualMachine]
            VMC[VMClass]
            VMI[VMImage]
            PR[Provider]
            VMS[VMSet]
            VMM[VMMigration]
            VMP[VMPlacementPolicy]
        end

        subgraph "VirtRigaud Controller (Go)"
            WA[Watch API<br/>controller-runtime]

            subgraph "Reconcilers"
                VMR[VirtualMachine<br/>Reconciler]
                PRR[Provider<br/>Reconciler]
                VMSR[VMSet<br/>Reconciler]
                VMMR[VMMigration<br/>Reconciler]
            end

            subgraph "Core Components"
                PF[Provider<br/>Factory]
                PM[Provider<br/>Manager]
                FC[Finalizer<br/>Controller]
            end
        end

        subgraph "Provider Pods (gRPC)"
            VSP[vSphere<br/>Provider]
            LVP[Libvirt<br/>Provider]
            PXP[Proxmox<br/>Provider]
        end

        subgraph "Kubernetes Resources"
            DEP[Deployments]
            SVC[Services]
            SEC[Secrets]
            CM[ConfigMaps]
        end
    end

    subgraph "Hypervisors"
        VCS[vCenter Server]
        ESX[ESXi Hosts]
        KVM[KVM/Libvirt]
        PVE[Proxmox VE]
    end

    subgraph "Virtual Machines"
        VM1[VM Instance 1<br/>vSphere]
        VM2[VM Instance 2<br/>Libvirt]
        VM3[VM Instance 3<br/>Proxmox]
    end

    %% Custom Resource relationships
    VM -.references.-> VMC
    VM -.references.-> VMI
    VM -.references.-> PR
    VMS -.manages.-> VM
    VMM -.migrates.-> VM
    VMP -.constrains.-> VM

    %% Watch relationships
    VM --> WA
    PR --> WA
    VMC --> WA
    VMI --> WA
    VMS --> WA
    VMM --> WA
    VMP --> WA

    %% Reconciler routing
    WA --> VMR
    WA --> PRR
    WA --> VMSR
    WA --> VMMR

    %% Component interactions
    VMR --> PF
    PRR --> PM
    VMSR --> VMR
    VMMR --> VMR
    VMR --> FC

    %% Provider selection
    PF --> VSP
    PF --> LVP
    PF --> PXP

    %% K8s resource management
    PRR --> DEP
    PRR --> SVC
    PRR --> SEC
    PRR --> CM

    %% Provider to hypervisor communication
    VSP -.gRPC/REST.-> VCS
    LVP -.libvirt.-> KVM
    PXP -.REST API.-> PVE

    %% Hypervisor to VM
    VCS --> ESX
    ESX --> VM1
    KVM --> VM2
    PVE --> VM3

    %% Styling
    style VM fill:#e1f5ff
    style VMC fill:#e1f5ff
    style VMI fill:#e1f5ff
    style PR fill:#e1f5ff
    style VMS fill:#fff4e1
    style VMM fill:#fff4e1
    style VMP fill:#fff4e1
    style WA fill:#f0f0f0
    style VMR fill:#d4e8d4
    style PRR fill:#d4e8d4
    style VMSR fill:#d4e8d4
    style VMMR fill:#d4e8d4
    style PF fill:#ffd4d4
    style PM fill:#ffd4d4
    style FC fill:#ffd4d4
    style VSP fill:#e8d4ff
    style LVP fill:#e8d4ff
    style PXP fill:#e8d4ff
```

## VirtualMachine Reconciliation Flow

```mermaid
sequenceDiagram
    participant U as User
    participant K as Kubernetes API
    participant C as VM Controller
    participant P as Provider
    participant H as Hypervisor

    U->>K: Create VirtualMachine CR
    K->>C: Watch Event (Create)
    C->>K: Get VirtualMachine
    C->>K: Get Provider
    C->>K: Get VMClass
    C->>P: CreateVM(spec)
    P->>H: Create VM via API
    H-->>P: VM Created (ID: vm-123)
    P-->>C: VMStatus
    C->>K: Update VM Status
    C->>C: Add Finalizer
    C->>K: Update VirtualMachine
    K-->>U: VM Ready

    Note over C,P: Reconciliation Loop<br/>Every 5 minutes

    U->>K: Delete VirtualMachine
    K->>C: Watch Event (Delete)
    C->>C: Finalizer Present?
    C->>P: DeleteVM(vm-123)
    P->>H: Delete VM
    H-->>P: VM Deleted
    P-->>C: Success
    C->>C: Remove Finalizer
    C->>K: Update VirtualMachine
    K->>K: Delete VirtualMachine
    K-->>U: VM Deleted
```

## Provider Architecture

```mermaid
graph LR
    subgraph Controller["VirtRigaud Controller"]
        VMR[VirtualMachine<br/>Reconciler]
        PF[Provider<br/>Factory]
        PI[Provider Interface<br/>CreateVM DeleteVM<br/>GetVM UpdateVM<br/>PowerOn PowerOff]
    end

    subgraph InProcess["In-Process Providers"]
        LOCAL[Local Provider<br/>Implementation]
    end

    subgraph Remote["Remote Providers - gRPC"]
        GRPC_CLIENT[gRPC Client]
        VSP[vSphere Provider<br/>Service]
        LVP[Libvirt Provider<br/>Service]
        PXP[Proxmox Provider<br/>Service]
    end

    subgraph Hypervisors["Hypervisors"]
        VC[vCenter API]
        LV[Libvirt API]
        PV[Proxmox API]
    end

    VMR --> PF
    PF --> PI
    PI --> LOCAL
    PI --> GRPC_CLIENT

    GRPC_CLIENT -.gRPC:9443.-> VSP
    GRPC_CLIENT -.gRPC:9443.-> LVP
    GRPC_CLIENT -.gRPC:9443.-> PXP

    VSP -.HTTPS:443.-> VC
    LVP -.libvirt.-> LV
    PXP -.HTTPS:8006.-> PV

    style VMR fill:#d4e8d4
    style PF fill:#ffd4d4
    style PI fill:#ffe4d4
    style LOCAL fill:#e8d4ff
    style GRPC_CLIENT fill:#d4e8ff
    style VSP fill:#e8d4ff
    style LVP fill:#e8d4ff
    style PXP fill:#e8d4ff
```

## VM Lifecycle State Machine

```mermaid
stateDiagram-v2
    [*] --> Pending : Create VirtualMachine CR

    Pending --> Creating : Reconciler picks up
    Creating --> Running : VM created successfully
    Creating --> Error : Creation failed

    Running --> Updating : Spec changed
    Running --> Stopping : powerState off
    Running --> Suspended : powerState suspended
    Running --> Migrating : VMMigration created

    Updating --> Running : Update successful
    Updating --> Error : Update failed

    Stopping --> Stopped : VM powered off
    Stopped --> Starting : powerState on
    Starting --> Running : VM powered on

    Suspended --> Resuming : powerState on
    Resuming --> Running : VM resumed

    Migrating --> Running : Migration successful
    Migrating --> Error : Migration failed

    Error --> Deleting : Delete CR
    Running --> Deleting : Delete CR
    Stopped --> Deleting : Delete CR

    Deleting --> [*] : VM deleted from hypervisor
```

## VMSet Scaling Flow

```mermaid
sequenceDiagram
    participant U as User
    participant K as Kubernetes API
    participant VS as VMSet Controller
    participant VM as VM Controller
    participant P as Provider

    U->>K: Create VMSet (replicas: 3)
    K->>VS: Watch Event
    VS->>K: Get VMSet
    VS->>VS: Calculate desired VMs

    loop For each replica (1..3)
        VS->>K: Create VirtualMachine
        K->>VM: Watch Event
        VM->>P: CreateVM()
        P-->>VM: VMStatus
        VM->>K: Update VM Status
    end

    VS->>K: Update VMSet Status (ready: 3/3)
    K-->>U: VMSet Ready

    Note over U,P: Scale Up Event

    U->>K: Update VMSet (replicas: 5)
    K->>VS: Watch Event (Update)
    VS->>K: List VirtualMachines
    VS->>VS: Need 2 more VMs

    loop For new replicas (4..5)
        VS->>K: Create VirtualMachine
        K->>VM: Watch Event
        VM->>P: CreateVM()
        P-->>VM: VMStatus
        VM->>K: Update VM Status
    end

    VS->>K: Update VMSet Status (ready: 5/5)
```

## Migration Flow

```mermaid
sequenceDiagram
    participant U as User
    participant K as Kubernetes API
    participant MC as Migration Controller
    participant VM as VM Controller
    participant SP as Source Provider
    participant TP as Target Provider
    participant SH as Source Hypervisor
    participant TH as Target Hypervisor

    U->>K: Create VMMigration CR
    K->>MC: Watch Event
    MC->>K: Get VMMigration
    MC->>K: Get source VirtualMachine
    MC->>K: Get source Provider
    MC->>K: Get target Provider

    alt Cold Migration
        MC->>VM: Update VM (powerState: off)
        VM->>SP: PowerOff(vm-123)
        SP->>SH: Power off VM
        SH-->>SP: VM powered off
        SP-->>VM: Success

        MC->>SP: ExportVM(vm-123)
        SP->>SH: Export VM disk
        SH-->>SP: Disk data stream
        SP-->>MC: Disk data

        MC->>TP: ImportVM(data)
        TP->>TH: Create VM
        TH-->>TP: VM created (vm-456)
        TP-->>MC: Success

        MC->>VM: Update VM (providerRef: target, powerState: on)
        VM->>TP: PowerOn(vm-456)
        TP->>TH: Power on VM
        TH-->>TP: VM running
        TP-->>VM: Success

        MC->>SP: DeleteVM(vm-123)
        SP->>SH: Delete VM
    else Live Migration
        Note over MC,TH: Live migration if supported
        MC->>SP: LiveMigrate(vm-123, target)
        SP->>SH: Prepare migration
        SH->>TH: Transfer memory state
        TH->>SH: Migration complete
        SH-->>SP: Success
        SP-->>MC: Migration complete
    end

    MC->>K: Update VMMigration Status (phase: Complete)
    MC->>K: Update VM Status (provider: target)
    K-->>U: Migration Complete
```

## Security Architecture

```mermaid
graph TB
    subgraph "Kubernetes Cluster"
        subgraph "VirtRigaud Namespace"
            CTRL[Controller<br/>ServiceAccount]
            SEC[Provider Secrets<br/>Credentials]
        end

        subgraph "RBAC"
            CR[ClusterRole<br/>virtrigaud-controller]
            CRB[ClusterRoleBinding]
            R[Role<br/>leader-election]
        end

        subgraph "Network Policies"
            NP[NetworkPolicy<br/>Allow egress to providers]
        end

        CRB --> CTRL
        CRB --> CR
        R --> CTRL
        NP --> CTRL
    end

    subgraph "Provider Communication"
        subgraph "mTLS"
            CERT[TLS Certificates<br/>cert-manager]
            CTRL_CERT[Controller Cert]
            PROV_CERT[Provider Certs]
        end

        subgraph "Authentication"
            TOKEN[Bearer Tokens]
            MTLS[Mutual TLS]
        end
    end

    subgraph "External Systems"
        subgraph "Secrets Management"
            ESO[External Secrets<br/>Operator]
            VAULT[Vault]
            ASM[AWS Secrets<br/>Manager]
        end

        subgraph "Hypervisors"
            VC[vCenter<br/>HTTPS:443]
            KVM[Libvirt<br/>TLS]
            PVE[Proxmox<br/>HTTPS:8006]
        end
    end

    CTRL --> SEC
    SEC -.referenced by.-> ESO
    ESO --> VAULT
    ESO --> ASM

    CTRL --> CERT
    CERT --> CTRL_CERT
    CERT --> PROV_CERT

    CTRL -.mTLS + Token.-> VC
    CTRL -.mTLS.-> KVM
    CTRL -.Bearer Token.-> PVE

    style CTRL fill:#d4e8d4
    style SEC fill:#ffd4d4
    style CR fill:#e8d4ff
    style NP fill:#e8d4ff
    style CERT fill:#ffe4d4
    style ESO fill:#fff4e1
```

## Controller Components (Go)

```mermaid
graph TB
    subgraph "Main Process (cmd/manager/)"
        MAIN[main.go<br/>Manager Setup]
    end

    subgraph "API Definitions (api/v1beta1/)"
        CRD_VM[VirtualMachine]
        CRD_VMC[VMClass]
        CRD_VMI[VMImage]
        CRD_PR[Provider]
        CRD_VMS[VMSet]
        CRD_VMM[VMMigration]
        CRD_VMP[VMPlacementPolicy]
    end

    subgraph "Controllers (internal/controller/)"
        CTRL_VM[virtualmachine/<br/>reconciler.go]
        CTRL_PR[provider/<br/>reconciler.go]
        CTRL_VMS[vmset/<br/>reconciler.go]
        CTRL_VMM[vmmigration/<br/>reconciler.go]
    end

    subgraph "Providers (internal/provider/)"
        PI[interface.go<br/>Provider Interface]

        subgraph "Implementations"
            VSP[vsphere/<br/>provider.go]
            LVP[libvirt/<br/>provider.go]
            PXP[proxmox/<br/>provider.go]
        end

        PF[factory.go<br/>Provider Factory]
    end

    subgraph "gRPC (pkg/grpc/)"
        GRPC_SERVER[server/<br/>server.go]
        GRPC_CLIENT[client/<br/>client.go]
        PROTO[proto/<br/>provider.proto]
    end

    subgraph "Utilities (pkg/util/)"
        UTIL[Helpers<br/>Validators<br/>Converters]
    end

    MAIN --> CTRL_VM
    MAIN --> CTRL_PR
    MAIN --> CTRL_VMS
    MAIN --> CTRL_VMM

    CRD_VM --> CTRL_VM
    CRD_PR --> CTRL_PR
    CRD_VMS --> CTRL_VMS
    CRD_VMM --> CTRL_VMM

    CTRL_VM --> PF
    CTRL_PR --> PF

    PF --> PI
    PI --> VSP
    PI --> LVP
    PI --> PXP
    PI --> GRPC_CLIENT

    GRPC_CLIENT --> PROTO
    GRPC_SERVER --> PROTO
    GRPC_SERVER --> VSP
    GRPC_SERVER --> LVP
    GRPC_SERVER --> PXP

    CTRL_VM --> UTIL
    CTRL_PR --> UTIL

    style MAIN fill:#e1f5ff
    style CTRL_VM fill:#d4e8d4
    style CTRL_PR fill:#d4e8d4
    style CTRL_VMS fill:#d4e8d4
    style CTRL_VMM fill:#d4e8d4
    style PI fill:#ffe4d4
    style PF fill:#ffd4d4
```

## Data Flow

```mermaid
graph LR
    subgraph "User Actions"
        UC[kubectl apply<br/>VirtualMachine]
        UD[kubectl delete<br/>VirtualMachine]
        UU[kubectl edit<br/>VirtualMachine]
    end

    subgraph "Kubernetes API Server"
        API[API Server<br/>Validation<br/>Admission]
        ETCD[(etcd<br/>State Store)]
    end

    subgraph "VirtRigaud Controller"
        WATCH[Watch Manager<br/>Informers]
        QUEUE[Work Queue]
        RECON[Reconciler<br/>Business Logic]
        STATUS[Status<br/>Updater]
    end

    subgraph "Provider Layer"
        CACHE[Provider<br/>Cache]
        PROV[Provider<br/>Client]
    end

    subgraph "Hypervisor"
        HYP[Hypervisor API]
        VMS[(Virtual Machines)]
    end

    UC --> API
    UD --> API
    UU --> API

    API --> ETCD
    ETCD --> WATCH
    WATCH --> QUEUE
    QUEUE --> RECON

    RECON --> CACHE
    CACHE --> PROV
    PROV --> HYP
    HYP --> VMS

    VMS -.status.-> HYP
    HYP -.response.-> PROV
    PROV -.state.-> CACHE
    CACHE -.state.-> RECON

    RECON --> STATUS
    STATUS --> API
    API --> ETCD

    style UC fill:#e1f5ff
    style API fill:#d4e8d4
    style RECON fill:#ffd4d4
    style PROV fill:#e8d4ff
    style HYP fill:#ffe4d4
```

## High Availability Setup

```mermaid
graph TB
    subgraph "Kubernetes Cluster - Region 1"
        subgraph "Control Plane"
            LE[Leader Election<br/>ConfigMap Lock]
        end

        subgraph "VirtRigaud Pods"
            C1[Controller-1<br/>LEADER]
            C2[Controller-2<br/>Standby]
            C3[Controller-3<br/>Standby]
        end

        subgraph "Provider Pods"
            P1[vSphere Provider-1]
            P2[vSphere Provider-2]
        end

        C1 --> LE
        C2 -.standby.-> LE
        C3 -.standby.-> LE

        C1 --> P1
        C1 --> P2
    end

    subgraph "Hypervisor - Region 1"
        VC1[vCenter 1<br/>Primary]
        VC2[vCenter 2<br/>Secondary]

        P1 --> VC1
        P2 --> VC2
    end

    subgraph "Kubernetes Cluster - Region 2"
        subgraph "VirtRigaud Pods DR"
            C4[Controller-4<br/>Standby]
            C5[Controller-5<br/>Standby]
        end

        subgraph "Provider Pods DR"
            P3[vSphere Provider-3]
        end
    end

    subgraph "Hypervisor - Region 2"
        VC3[vCenter 3<br/>DR]

        P3 --> VC3
    end

    VC1 -.replication.-> VC3

    C4 -.monitors.-> LE
    C5 -.monitors.-> LE

    style C1 fill:#d4e8d4
    style C2 fill:#e8e8e8
    style C3 fill:#e8e8e8
    style C4 fill:#e8e8e8
    style C5 fill:#e8e8e8
    style LE fill:#ffe4d4
```

## Related Documentation

- [Architecture Overview](architecture.md) - Detailed architecture explanation
- [Provider Architecture](../providers.md) - Provider implementation details
- [Remote Providers](../remote-providers.md) - gRPC provider architecture
- [Security](../security.md) - Security model and best practices
- [High Availability](../advanced/ha.md) - HA deployment patterns
