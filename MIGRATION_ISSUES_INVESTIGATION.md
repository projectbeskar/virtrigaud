# Migration Issues Investigation

## Issue Summary

Two critical issues identified:

1. **Migration stuck in "Creating" phase** despite VM being ready
2. **VM doesn't contain source files/hostname** - appears to be fresh install instead of migrated disk

---

## Issue 1: Migration Phase Stuck in "Creating"

### Root Cause

**BUG**: The VMMigration controller checks for a `Phase` value that never gets set.

**Location**: `internal/controller/vmmigration_controller.go`

- **Line 811**: `if existingVM.Status.Phase == "Ready"` ❌
- **Line 941**: `if targetVM.Status.Phase != "Ready"` ❌

**Problem**: 
- The `VirtualMachineStatus` has a `Phase` field (type `VirtualMachinePhase`)
- Valid enum values: `Pending`, `Provisioning`, `Running`, `Stopped`, `Reconfiguring`, `Deleting`, `Failed`
- **"Ready" is NOT a valid phase value!**

**How VMs Actually Signal Readiness**:
- `virtualmachine_controller.go` line 199:
  ```go
  k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionTrue, ...)
  ```
- The controller sets a **Ready condition**, NOT a `Phase = "Ready"`

**Evidence**:
```
Manager logs show:
"Target VM exists but not ready, waiting", "vm": "rbc-demo-migrated", "phase": ""

VM Status shows:
conditions:
  - type: Ready
    status: "True"
phase: ""  <-- EMPTY!
```

### Solution

The migration controller must check the **Ready condition** instead of phase:

```go
// WRONG (current):
if existingVM.Status.Phase == "Ready" {

// CORRECT (should be):
readyCondition := meta.FindStatusCondition(existingVM.Status.Conditions, "Ready")
if existingVM.Status.ID != "" && readyCondition != null && readyCondition.Status == metav1.ConditionTrue {
```

---

## Issue 2: VM Contains Fresh Install Instead of Migrated Data

### Symptoms

- Target VM hostname is "rbc-demo-migrated" (not source hostname)
- Files and programs from source VM are missing
- Appears to be a fresh Ubuntu installation

### Investigation

**VM Spec is CORRECT**:
```yaml
spec:
  importedDisk:
    diskID: rbc-demo-migrated-migrated
    format: qcow2
    source: migration
```

**Manager logs confirm ImportedDisk is recognized**:
```
"image": "imported:rbc-demo-migrated-migrated"
```

**Problem**: Need to verify if the libvirt provider's `CreateVM` method is actually:
1. Using the imported disk
2. OR falling back to creating from a template/image

###Hypothesis

The libvirt provider's `buildCreateRequest` method may not be correctly handling the `ImportedDisk` case. Possible scenarios:

1. **Missing Implementation**: `buildCreateRequest` doesn't check for `ImportedDisk`
2. **Fallback Logic**: Provider falls back to template if `ImageRef` is nil
3. **Disk Not Found**: Imported disk exists but provider can't find it, creates new disk
4. **Wrong Disk Used**: Provider uses template disk instead of imported disk

### Files to Check

1. `internal/controller/virtualmachine_controller.go` - `buildCreateRequest` method
2. `internal/providers/libvirt/server.go` - `Create` method implementation
3. Verify the imported disk actually exists on the libvirt host at the expected location

### Next Steps for Issue 2

1. Check `buildCreateRequest` implementation to see how it handles `ImportedDisk`
2. Check libvirt provider's `Create` method to see how it processes the image/disk info
3. Verify the imported disk file exists on the libvirt host:
   ```bash
   ssh wrkode@172.16.56.8 "ls -lh /var/lib/libvirt/images/ | grep rbc-demo-migrated"
   ```
4. Check libvirt VM's disk configuration:
   ```bash
   ssh wrkode@172.16.56.8 "virsh domblklist rbc-demo-migrated"
   ```
5. If disk is correct, the issue might be cloud-init or similar initialization changing hostname

---

## Priority

1. **FIX Issue 1 FIRST** - This blocks migration completion
   - Quick fix: Change phase check to condition check
   - Test: Migration should transition to `ValidatingTarget` → `Ready`

2. **INVESTIGATE Issue 2** - This is the critical data integrity issue
   - If files are missing, the migration is not actually migrating data
   - This would mean the entire migration feature is broken
   - Need thorough investigation of disk import and VM creation flow

---

## Impact

**Issue 1**: Operational - migrations appear stuck but VM is actually working  
**Issue 2**: CRITICAL - if true, migrations are creating fresh VMs instead of migrating data

Both issues must be resolved before the migration feature can be considered functional.

---

## FIXES IMPLEMENTED

### Fix 1: Migration Controller Ready Check ✅

**File**: `internal/controller/vmmigration_controller.go`

**Changed** (lines 811, 941):
```go
// WRONG:
if existingVM.Status.Phase == "Ready" {

// CORRECT:
readyCondition := meta.FindStatusCondition(existingVM.Status.Conditions, "Ready")
isReady := existingVM.Status.ID != "" && readyCondition != nil && readyCondition.Status == metav1.ConditionTrue
if isReady {
```

**Impact**: Migration will now correctly detect when target VM is ready and transition to `ValidatingTarget` → `Ready` phase.

### Fix 2: Libvirt Use Imported Disk Directly ✅

**File**: `internal/providers/libvirt/storage.go`

**Problem**: `CreateVolumeFromImageFile` was always copying/converting disks, even when they were already in the correct location and format (imported disks).

**Solution**: Added check to detect when source disk is already in pool directory with correct format:
```go
sourceDir := filepath.Dir(sourceImagePath)
poolPath := poolInfo.Path

if sourceDir == poolPath && strings.HasSuffix(sourceImagePath, ".qcow2") {
    log.Printf("INFO Using existing disk directly without copying (typical for imported/migrated disks)")
    // Use disk in-place, just fix permissions and refresh pool
    return &StorageVolume{...}
}
```

**Impact**: Migrated VMs will now use the actual imported disk with all original data, instead of creating a fresh copy from a template.

---

## Testing Required

1. Delete the current migration and target VM
2. Delete the duplicate disk files
3. Deploy new version (v0.3.61-dev)
4. Run new migration
5. **Verify**:
   - Migration completes and transitions to `Ready` phase
   - Only ONE disk file exists in `/var/lib/libvirt/images/`
   - Target VM has correct hostname from source
   - Target VM has all files/programs from source

