# VirtRigaud CRD Status Update Logic

This document provides a comprehensive overview of the status update patterns for all Custom Resource Definitions (CRDs) in the VirtRigaud project.

## Table of Contents

1. [VirtualMachine](#virtualmachine)
2. [Provider](#provider)
3. [VMMigration](#vmmigration)
4. [VMSet](#vmset)
5. [VMPlacementPolicy](#vmplacementpolicy)
6. [VMSnapshot](#vmsnapshot)
7. [VMClass](#vmclass)
8. [VMImage](#vmimage)
9. [VMClone](#vmclone)
10. [VMNetworkAttachment](#vmnetworkattachment)

---

## VirtualMachine

**File**: `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`

### Status Fields

```go
type VirtualMachineStatus struct {
    // Core Fields
    ID                    string                      // Provider-specific VM identifier
    PowerState            PowerState                  // Current power state (On/Off/OffGraceful)
    Phase                 VirtualMachinePhase         // Current phase (Pending/Provisioning/Running/Stopped/Reconfiguring/Deleting/Failed)
    Message               string                      // Additional state details

    // Network & Console
    IPs                   []string                    // Assigned IP addresses
    ConsoleURL            string                      // VM console access URL

    // Async Operations
    LastTaskRef           string                      // Last async operation reference
    ReconfigureTaskRef    string                      // Reconfiguration operation reference
    LastReconfigureTime   *metav1.Time               // Last reconfiguration timestamp

    // Resources
    CurrentResources      *VirtualMachineResources    // Current resource allocation

    // Snapshots
    Snapshots             []VMSnapshotInfo            // Available snapshots

    // Metadata
    ObservedGeneration    int64                       // Observed generation
    Conditions            []metav1.Condition          // Standard K8s conditions
    Provider              map[string]string           // Provider-specific details
}
```

### Phases

- **Pending**: VM is waiting to be processed
- **Provisioning**: VM is being created
- **Running**: VM is running
- **Stopped**: VM is stopped
- **Reconfiguring**: VM is being reconfigured
- **Deleting**: VM is being deleted
- **Failed**: VM is in a failed state

### Condition Types

- `Ready`: Whether the VM is ready
- `Provisioning`: Whether the VM is being provisioned
- `Reconfiguring`: Whether the VM is being reconfigured
- `Deleting`: Whether the VM is being deleted

### Update Pattern

```go
import "k8s.io/apimachinery/pkg/api/meta"

// Update phase
vm.Status.Phase = VirtualMachinePhaseRunning
vm.Status.Message = "VM is running"

// Update condition
meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
    Type:               VirtualMachineConditionReady,
    Status:             metav1.ConditionTrue,
    Reason:             "ReconcileSucceeded",
    Message:            "VM synchronized successfully",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: vm.Generation,
})

// Update status subresource
if err := r.Status().Update(ctx, vm); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to update VirtualMachine status: %w", err)
}
```

---

## Provider

**File**: `api/infra.virtrigaud.io/v1beta1/provider_types.go`

### Status Fields

```go
type ProviderStatus struct {
    // Health
    Healthy             bool                        // Provider health status
    LastHealthCheck     *metav1.Time               // Last health check time

    // Runtime
    Runtime             *ProviderRuntimeStatus      // Runtime status (mode, endpoint, phase, replicas)

    // Capabilities
    Capabilities        []ProviderCapability        // Supported capabilities
    Version             string                      // Provider version

    // Resources
    ConnectedVMs        int32                       // Number of managed VMs
    ResourceUsage       *ProviderResourceUsage      // Resource usage statistics

    // Metadata
    ObservedGeneration  int64                       // Observed generation
    Conditions          []metav1.Condition          // Standard K8s conditions
}

type ProviderRuntimeStatus struct {
    Mode                ProviderRuntimeMode         // Remote
    Endpoint            string                      // gRPC endpoint (host:port)
    ServiceRef          *corev1.LocalObjectReference // K8s service reference
    Phase               ProviderRuntimePhase        // Pending/Starting/Running/Stopping/Failed
    Message             string                      // Additional details
    ReadyReplicas       int32                       // Number of ready replicas
    AvailableReplicas   int32                       // Number of available replicas
}
```

### Runtime Phases

- **Pending**: Runtime is being prepared
- **Starting**: Runtime is starting
- **Running**: Runtime is operational
- **Stopping**: Runtime is stopping
- **Failed**: Runtime has failed

### Condition Types

- `Ready`: Whether the provider is ready
- `Healthy`: Whether the provider is healthy
- `Connected`: Whether the provider is connected
- `RuntimeReady`: Whether the runtime is ready

### Update Pattern

```go
// Update health status
provider.Status.Healthy = true
provider.Status.LastHealthCheck = &metav1.Time{Time: time.Now()}

// Update runtime status
if provider.Status.Runtime == nil {
    provider.Status.Runtime = &ProviderRuntimeStatus{}
}
provider.Status.Runtime.Phase = ProviderRuntimePhaseRunning
provider.Status.Runtime.ReadyReplicas = 1
provider.Status.Runtime.AvailableReplicas = 1

// Update conditions
meta.SetStatusCondition(&provider.Status.Conditions, metav1.Condition{
    Type:               ProviderConditionReady,
    Status:             metav1.ConditionTrue,
    Reason:             "ProviderHealthy",
    Message:            "Provider is healthy and ready",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: provider.Generation,
})

// Update status
if err := r.Status().Update(ctx, provider); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to update Provider status: %w", err)
}
```

---

## VMMigration

**File**: `api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`

### Status Fields

```go
type VMMigrationStatus struct {
    // Phase & Message
    Phase                   MigrationPhase              // Current migration phase
    Message                 string                      // Additional state details

    // References
    TargetVMRef             *LocalObjectReference       // Created target VM
    SnapshotRef             string                      // Source snapshot reference
    SnapshotID              string                      // Provider-specific snapshot ID
    ExportID                string                      // Export operation ID
    ImportID                string                      // Import operation ID
    TaskRef                 string                      // Current task reference
    TargetVMID              string                      // Provider-specific target VM ID

    // Timing
    StartTime               *metav1.Time                // Migration start time
    CompletionTime          *metav1.Time                // Migration completion time

    // Progress
    Progress                *MigrationProgress          // Migration progress details

    // Disk & Storage
    DiskInfo                *MigrationDiskInfo          // Disk information
    StorageInfo             *MigrationStorageInfo       // Intermediate storage info
    StoragePVCName          string                      // PVC name for migration storage

    // Retry & Validation
    RetryCount              int32                       // Number of retries
    LastRetryTime           *metav1.Time                // Last retry time
    ValidationResults       *ValidationResults          // Validation check results

    // Metadata
    ObservedGeneration      int64                       // Observed generation
    Conditions              []metav1.Condition          // Standard K8s conditions
}
```

### Migration Phases

- **Pending**: Migration is waiting to be processed
- **Validating**: Migration is being validated
- **Snapshotting**: Snapshot is being created
- **Exporting**: Disk is being exported
- **Transferring**: Disk is being transferred
- **Converting**: Disk format is being converted
- **Importing**: Disk is being imported
- **Creating**: Target VM is being created
- **Validating-Target**: Target VM is being validated
- **Ready**: Migration is complete
- **Failed**: Migration failed

### Condition Types

- `Ready`: Whether the migration is ready
- `Validating`: Whether validation is in progress
- `Snapshotting`: Whether snapshotting is in progress
- `Exporting`: Whether disk export is in progress
- `Transferring`: Whether transfer is in progress
- `Importing`: Whether disk import is in progress
- `Failed`: Whether the migration has failed

### Update Pattern

```go
// Update phase
migration.Status.Phase = MigrationPhaseExporting
migration.Status.Message = "Exporting disk from source provider"

// Update progress
if migration.Status.Progress == nil {
    migration.Status.Progress = &MigrationProgress{}
}
migration.Status.Progress.CurrentPhase = MigrationPhaseExporting
migration.Status.Progress.Percentage = pointer.Int32(30)

// Update condition
meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
    Type:               VMMigrationConditionExporting,
    Status:             metav1.ConditionTrue,
    Reason:             VMMigrationReasonExporting,
    Message:            "Exporting disk from source provider",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: migration.Generation,
})

// Update status
if err := r.Status().Update(ctx, migration); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to update VMMigration status: %w", err)
}
```

---

## VMSet

**File**: `api/infra.virtrigaud.io/v1beta1/vmset_types.go`

### Status Fields

```go
type VMSetStatus struct {
    // Replicas
    Replicas              int32                       // VMs created by the controller
    ReadyReplicas         int32                       // Number of ready VMs
    AvailableReplicas     int32                       // Number of available VMs
    UpdatedReplicas       int32                       // Number of updated VMs
    CurrentReplicas       int32                       // Number of currently running VMs

    // Revisions
    CurrentRevision       string                      // Current VMSet revision
    UpdateRevision        string                      // Update VMSet revision
    CollisionCount        *int32                      // Hash collision count

    // Update Status
    UpdateStatus          *VMSetUpdateStatus          // Detailed update status

    // Per-VM Status
    VMStatus              []VMSetVMStatus             // Individual VM status (max 1000)

    // Metadata
    ObservedGeneration    int64                       // Observed generation
    Conditions            []metav1.Condition          // Standard K8s conditions
}

type VMSetUpdateStatus struct {
    Phase                 VMSetUpdatePhase            // Pending/InProgress/Paused/Completed/Failed
    Message               string                      // Additional details
    StartTime             *metav1.Time                // Update start time
    CompletionTime        *metav1.Time                // Update completion time
    UpdatedVMs            []string                    // List of updated VMs (max 1000)
    PendingVMs            []string                    // List of pending VMs (max 1000)
    FailedVMs             []VMSetFailedVM             // List of failed VMs (max 1000)
}
```

### Update Phases

- **Pending**: Update is pending
- **InProgress**: Update is in progress
- **Paused**: Update is paused
- **Completed**: Update is completed
- **Failed**: Update failed

### Condition Types

- `Ready`: Whether the VMSet is ready
- `Progressing`: Whether the VMSet is progressing
- `ReplicaFailure`: Failure to create/delete replicas
- `UpdateInProgress`: An update is in progress
- `Scaling`: Scaling is in progress

### Update Pattern

```go
// Update replica counts
vmset.Status.Replicas = 5
vmset.Status.ReadyReplicas = 4
vmset.Status.AvailableReplicas = 4
vmset.Status.UpdatedReplicas = 3

// Update status for rolling update
if vmset.Status.UpdateStatus == nil {
    vmset.Status.UpdateStatus = &VMSetUpdateStatus{}
}
vmset.Status.UpdateStatus.Phase = VMSetUpdatePhaseInProgress
vmset.Status.UpdateStatus.StartTime = &metav1.Time{Time: time.Now()}
vmset.Status.UpdateStatus.UpdatedVMs = append(vmset.Status.UpdateStatus.UpdatedVMs, "vm-1", "vm-2")

// Update condition
meta.SetStatusCondition(&vmset.Status.Conditions, metav1.Condition{
    Type:               VMSetConditionProgressing,
    Status:             metav1.ConditionTrue,
    Reason:             VMSetReasonUpdatingReplicas,
    Message:            "Rolling update in progress: 3/5 VMs updated",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: vmset.Generation,
})

// Update status
if err := r.Status().Update(ctx, vmset); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to update VMSet status: %w", err)
}
```

---

## VMPlacementPolicy

**File**: `api/infra.virtrigaud.io/v1beta1/vmplacementpolicy_types.go`

### Status Fields

```go
type VMPlacementPolicyStatus struct {
    // Usage
    UsedByVMs              []LocalObjectReference      // VMs using this policy (max 1000)

    // Validation
    ValidationResults      map[string]PolicyValidationResult // Per-provider validation

    // Statistics
    PlacementStats         *PlacementStatistics        // Placement statistics

    // Conflicts
    ConflictingPolicies    []PolicyConflict            // Conflicting policies (max 50)

    // Metadata
    ObservedGeneration     int64                       // Observed generation
    Conditions             []metav1.Condition          // Standard K8s conditions
}

type PlacementStatistics struct {
    TotalPlacements         int32                      // Total placements
    SuccessfulPlacements    int32                      // Successful placements
    FailedPlacements        int32                      // Failed placements
    AveragePlacementTime    *metav1.Duration           // Average placement time
    ConstraintViolations    int32                      // Number of violations
    LastPlacementTime       *metav1.Time               // Last placement time
    PlacementDistribution   map[string]int32           // Distribution across hosts/clusters
}
```

### Condition Types

- `Ready`: Whether the policy is ready
- `Validated`: Whether the policy is validated
- `Conflicts`: Whether the policy has conflicts
- `Supported`: Whether the policy is supported

### Update Pattern

```go
// Update validation results
if policy.Status.ValidationResults == nil {
    policy.Status.ValidationResults = make(map[string]PolicyValidationResult)
}
policy.Status.ValidationResults["vsphere"] = PolicyValidationResult{
    Valid:   true,
    Message: "Policy is valid for vSphere provider",
    LastValidated: &metav1.Time{Time: time.Now()},
}

// Update placement statistics
if policy.Status.PlacementStats == nil {
    policy.Status.PlacementStats = &PlacementStatistics{}
}
policy.Status.PlacementStats.TotalPlacements++
policy.Status.PlacementStats.SuccessfulPlacements++

// Update condition
meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
    Type:               VMPlacementPolicyConditionReady,
    Status:             metav1.ConditionTrue,
    Reason:             VMPlacementPolicyReasonValid,
    Message:            "Placement policy is valid and ready",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: policy.Generation,
})

// Update status
if err := r.Status().Update(ctx, policy); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to update VMPlacementPolicy status: %w", err)
}
```

---

## VMSnapshot

**File**: `api/infra.virtrigaud.io/v1beta1/vmsnapshot_types.go`

### Status Fields

```go
type VMSnapshotStatus struct {
    // Core Fields
    SnapshotID            string                      // Provider-specific snapshot ID
    Phase                 SnapshotPhase               // Pending/Creating/Ready/Deleting/Failed/Expired
    Message               string                      // Additional state details

    // Timing
    CreationTime          *metav1.Time                // Snapshot creation time
    CompletionTime        *metav1.Time                // Snapshot completion time
    ExpiryTime            *metav1.Time                // Snapshot expiry time

    // Size
    Size                  *resource.Quantity          // Snapshot size
    VirtualSize           *resource.Quantity          // Virtual size

    // Progress
    Progress              *SnapshotProgress           // Snapshot creation progress
    TaskRef               string                      // Ongoing async operation

    // Provider & Hierarchy
    ProviderStatus        map[string]ProviderSnapshotStatus // Per-provider status
    Children              []SnapshotRef               // Child snapshots
    Parent                *SnapshotRef                // Parent snapshot

    // Metadata
    ObservedGeneration    int64                       // Observed generation
    Conditions            []metav1.Condition          // Standard K8s conditions
}
```

### Snapshot Phases

- **Pending**: Snapshot is waiting to be processed
- **Creating**: Snapshot is being created
- **Ready**: Snapshot is ready for use
- **Deleting**: Snapshot is being deleted
- **Failed**: Snapshot operation failed
- **Expired**: Snapshot has expired

### Condition Types

- `Ready`: Whether the snapshot is ready
- `Creating`: Whether the snapshot is being created
- `Deleting`: Whether the snapshot is being deleted
- `Expired`: Whether the snapshot has expired

### Update Pattern

```go
// Update phase and timing
snapshot.Status.Phase = SnapshotPhaseCreating
snapshot.Status.CreationTime = &metav1.Time{Time: time.Now()}

// Update progress
if snapshot.Status.Progress == nil {
    snapshot.Status.Progress = &SnapshotProgress{}
}
snapshot.Status.Progress.Percentage = pointer.Int32(75)
snapshot.Status.Progress.StartTime = &metav1.Time{Time: time.Now()}

// Update condition
meta.SetStatusCondition(&snapshot.Status.Conditions, metav1.Condition{
    Type:               VMSnapshotConditionCreating,
    Status:             metav1.ConditionTrue,
    Reason:             VMSnapshotReasonCreating,
    Message:            "Snapshot creation in progress (75% complete)",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: snapshot.Generation,
})

// Update status
if err := r.Status().Update(ctx, snapshot); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to update VMSnapshot status: %w", err)
}
```

---

## Common Patterns

### 1. Using meta.SetStatusCondition

All CRDs use the standard Kubernetes condition pattern via `meta.SetStatusCondition`:

```go
import "k8s.io/apimachinery/pkg/api/meta"

meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
    Type:               "Ready",
    Status:             metav1.ConditionTrue,    // or ConditionFalse/ConditionUnknown
    Reason:             "ReconcileSucceeded",
    Message:            "Resource is ready",
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: resource.Generation,
})
```

### 2. Phase-Based Status

Most CRDs use a `Phase` field to represent the current state:

```go
resource.Status.Phase = ResourcePhaseRunning
resource.Status.Message = "Additional context about the phase"
```

### 3. ObservedGeneration

Always update `ObservedGeneration` to match the resource's `Generation`:

```go
resource.Status.ObservedGeneration = resource.Generation
```

### 4. Status Subresource Update

Always use the status subresource for updates:

```go
if err := r.Status().Update(ctx, resource); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
}
```

### 5. Error Handling Pattern

```go
// Set error condition
meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
    Type:               "Ready",
    Status:             metav1.ConditionFalse,
    Reason:             "ReconcileFailed",
    Message:            fmt.Sprintf("Failed to reconcile: %v", err),
    LastTransitionTime: metav1.Now(),
    ObservedGeneration: resource.Generation,
})

resource.Status.Phase = ResourcePhaseFailed
resource.Status.Message = err.Error()

// Update status even on error
_ = r.Status().Update(ctx, resource)
```

---

## Best Practices

1. **Always set ObservedGeneration**: Update it to match the resource's Generation to indicate which version was reconciled.

2. **Use Conditions for multiple states**: A resource can have multiple conditions (Ready, Progressing, Degraded) simultaneously.

3. **Phase for primary state**: Use the Phase field for the high-level state that users care about most.

4. **Message for debugging**: Use the Message field to provide human-readable context.

5. **Timestamp updates**: Update time fields (StartTime, CompletionTime, LastHealthCheck) for observability.

6. **Progress tracking**: For long-running operations, use Progress structs with percentage completion.

7. **Reference tracking**: Store references to related resources (TaskRef, TargetVMRef) for traceability.

8. **Provider-specific details**: Use provider-specific maps for implementation details.

9. **Error recovery**: Always attempt to update status even when reconciliation fails.

10. **Nil-safety**: Check for nil before updating nested status fields.

---

## Status Update Helpers

### Helper for Setting Ready Condition

```go
func SetReadyCondition(conditions *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, message string) {
    meta.SetStatusCondition(conditions, metav1.Condition{
        Type:               "Ready",
        Status:             status,
        Reason:             reason,
        Message:            message,
        LastTransitionTime: metav1.Now(),
        ObservedGeneration: generation,
    })
}

// Usage:
SetReadyCondition(&vm.Status.Conditions, vm.Generation, metav1.ConditionTrue, "Provisioned", "VM is running")
```

### Helper for Progress Updates

```go
func UpdateProgress(progress **ProgressType, percentage int32, message string) {
    if *progress == nil {
        *progress = &ProgressType{}
    }
    (*progress).Percentage = &percentage
    (*progress).Message = message
}

// Usage:
UpdateProgress(&migration.Status.Progress, 50, "Transferring disk")
```

---

## Summary

All VirtRigaud CRDs follow consistent patterns for status updates:

- Use **Phases** for high-level state
- Use **Conditions** (via `meta.SetStatusCondition`) for detailed state tracking
- Track **ObservedGeneration** for generation awareness
- Provide **Progress** for long-running operations
- Store **References** for related resources
- Include **Timing** information for observability
- Handle **Errors** gracefully by updating status even on failure

This consistency makes the operator predictable and maintainable, following Kubernetes best practices.
