# VM Creation Failure During Migration - Root Cause Analysis & Solutions

## Executive Summary

**Status**: ✅ **Disk Import: SUCCESSFUL** | ❌ **VM Creation: BLOCKED**

The migration successfully completed the export and import phases:
- Disk exported from vSphere: 3.15 GB
- Disk copied to remote libvirt host via SCP: ✅ WORKING
- Disk imported to: `/var/lib/libvirt/images/rbc-demo-migrated-migrated.qcow2`
- Checksums verified: Source and target match

**Failure Point**: VM Creation phase
```
VirtualMachine.infra.virtrigaud.io "rbc-demo-migrated" is invalid: 
spec.imageRef.name: Invalid value: "": 
spec.imageRef.name in body should match '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
```

---

## Root Cause Analysis

### 1. Problem Location
**File**: `internal/controller/vmmigration_controller.go`  
**Function**: `handleCreatingPhase` (lines 837-850)

```go
targetVM := &infrav1beta1.VirtualMachine{
    ObjectMeta: metav1.ObjectMeta{
        Name:      targetVMName,
        Namespace: targetNamespace,
        Labels:    migration.Spec.Target.Labels,
        // ...
    },
    Spec: infrav1beta1.VirtualMachineSpec{
        ProviderRef: migration.Spec.Target.ProviderRef,  // ✅ Set
        // ClassRef: MISSING initially (added later if provided)
        // ImageRef: ❌ NEVER SET - THIS IS THE PROBLEM
        // Disks: Not configured to reference imported disk
    },
}

// Line 872-873: TODO comment acknowledges the issue
// TODO: Set disks - need to reference the imported disk
// For now, we'll let the provider handle disk attachment based on imported disk ID
```

### 2. CRD Validation Constraint

**File**: `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go` (lines 28-81)

```go
type VirtualMachineSpec struct {
    ProviderRef ObjectRef `json:"providerRef"`  // Required
    ClassRef    ObjectRef `json:"classRef"`     // Required
    ImageRef    ObjectRef `json:"imageRef"`     // Required - NO +optional marker
    Networks    []VMNetworkRef `json:"networks,omitempty"`     // Optional
    Disks       []DiskSpec     `json:"disks,omitempty"`        // Optional
    // ...
}

type ObjectRef struct {
    // Name MUST match pattern and cannot be empty
    // +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
    Name string `json:"name"`
    // ...
}
```

**The Constraint**: `ImageRef` is a **required field** but has no meaningful value for migrated VMs.

### 3. Design Gap

The VirtRigaud system was designed with two VM creation patterns:
1. **Template-based**: VMs created from VMImage templates (normal workflow)
2. **Disk-based**: VMs created from pre-existing disk images (migration workflow) **← NOT FULLY IMPLEMENTED**

Migrated VMs don't need `ImageRef` because they already have an imported disk, but the CRD validation doesn't account for this use case.

---

## Enterprise-Quality Solutions

### Solution 1: Make ImageRef Optional with Mutually Exclusive Validation ⭐ **RECOMMENDED**

**Approach**: Modify the CRD to make `ImageRef` optional when disks are provided from migration.

#### Implementation

**1.1. Update VirtualMachine CRD** (`api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`):

```go
type VirtualMachineSpec struct {
    ProviderRef ObjectRef `json:"providerRef"`
    ClassRef    ObjectRef `json:"classRef"`
    
    // ImageRef references the VMImage to use as base template
    // Either ImageRef OR ImportedDisk must be specified
    // +optional
    ImageRef *ObjectRef `json:"imageRef,omitempty"`
    
    // ImportedDisk references a pre-imported disk (e.g., from migration)
    // Either ImageRef OR ImportedDisk must be specified
    // +optional
    ImportedDisk *ImportedDiskRef `json:"importedDisk,omitempty"`
    
    // ... rest of fields
}

// ImportedDiskRef references a disk that was imported via migration or other means
type ImportedDiskRef struct {
    // DiskID is the provider-specific disk identifier
    // +kubebuilder:validation:Required
    DiskID string `json:"diskID"`
    
    // Path is the optional disk path (provider-specific)
    // +optional
    Path string `json:"path,omitempty"`
    
    // Format specifies the disk format (qcow2, vmdk, raw, etc.)
    // +optional
    // +kubebuilder:default="qcow2"
    Format string `json:"format,omitempty"`
    
    // Source indicates where the disk came from
    // +optional
    // +kubebuilder:validation:Enum=migration;clone;import;snapshot
    Source string `json:"source,omitempty"`
    
    // MigrationRef references the VMMigration that imported this disk
    // +optional
    MigrationRef *LocalObjectReference `json:"migrationRef,omitempty"`
}
```

**1.2. Add Webhook Validation** (new file: `api/infra.virtrigaud.io/v1beta1/virtualmachine_webhook.go`):

```go
func (r *VirtualMachine) ValidateCreate() error {
    // Validate that either ImageRef or ImportedDisk is specified
    if r.Spec.ImageRef == nil && r.Spec.ImportedDisk == nil {
        return fmt.Errorf("either imageRef or importedDisk must be specified")
    }
    
    // Validate mutual exclusivity
    if r.Spec.ImageRef != nil && r.Spec.ImportedDisk != nil {
        return fmt.Errorf("imageRef and importedDisk are mutually exclusive")
    }
    
    return nil
}
```

**1.3. Update VMMigration Controller**:

```go
// In handleCreatingPhase function
targetVM := &infrav1beta1.VirtualMachine{
    ObjectMeta: metav1.ObjectMeta{
        Name:      targetVMName,
        Namespace: targetNamespace,
        Labels:    migration.Spec.Target.Labels,
        Annotations: map[string]string{
            "virtrigaud.io/migrated-from": fmt.Sprintf("%s/%s", migration.Namespace, migration.Spec.Source.VMRef.Name),
            "virtrigaud.io/migration":     fmt.Sprintf("%s/%s", migration.Namespace, migration.Name),
            "virtrigaud.io/imported-disk-id": migration.Status.ImportID,
        },
    },
    Spec: infrav1beta1.VirtualMachineSpec{
        ProviderRef: migration.Spec.Target.ProviderRef,
        ClassRef: infrav1beta1.ObjectRef{
            Name:      migration.Spec.Target.ClassRef.Name,
            Namespace: migration.Namespace,
        },
        // Use ImportedDisk instead of ImageRef
        ImportedDisk: &infrav1beta1.ImportedDiskRef{
            DiskID:  migration.Status.ImportID,
            Format:  migration.Status.DiskInfo.TargetFormat,
            Source:  "migration",
            MigrationRef: &infrav1beta1.LocalObjectReference{
                Name: migration.Name,
            },
        },
        Networks: migration.Spec.Target.Networks,
    },
}
```

**1.4. Update VirtualMachine Controller** (`internal/controller/virtualmachine_controller.go`):

```go
func (r *VirtualMachineReconciler) buildCreateRequest(
    vm *infravirtrigaudiov1beta1.VirtualMachine,
    vmClass *infravirtrigaudiov1beta1.VMClass,
    vmImage *infravirtrigaudiov1beta1.VMImage,
    networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) contracts.CreateRequest {
    // ... existing code ...
    
    // Handle imported disk scenario
    var image contracts.VMImage
    if vm.Spec.ImportedDisk != nil {
        // VM uses an imported disk
        image = contracts.VMImage{
            Path:   vm.Spec.ImportedDisk.Path,
            Format: vm.Spec.ImportedDisk.Format,
        }
        
        // For libvirt: convert disk ID to path if path not provided
        if image.Path == "" {
            // Path: /var/lib/libvirt/images/{diskID}.qcow2
            image.Path = fmt.Sprintf("/var/lib/libvirt/images/%s.qcow2", vm.Spec.ImportedDisk.DiskID)
        }
    } else if vmImage != nil {
        // Normal template-based VM creation
        image = contracts.VMImage{
            TemplateName: extractTemplateName(vmImage),
            Path:         extractImagePath(vmImage),
            URL:          extractImageURL(vmImage),
            // ...
        }
    }
    
    return contracts.CreateRequest{
        Name:      vm.Name,
        Class:     class,
        Image:     image,
        Networks:  networkAttachments,
        Disks:     disks,
        UserData:  userData,
        Placement: placement,
        Tags:      vm.Spec.Tags,
    }
}
```

#### Pros ✅
- **Clean Architecture**: Explicitly separates template-based and disk-based VM creation
- **Type Safety**: Strong validation at the API level
- **Audit Trail**: Migration reference preserved in the VM spec
- **Extensible**: Can support other disk import scenarios (clones, snapshots, external imports)
- **Industry Standard**: Follows Kubernetes patterns for mutually exclusive fields
- **Backward Compatible**: Existing VMs continue to work with ImageRef

#### Cons ❌
- **CRD Migration Required**: Need to regenerate CRD manifests and apply to cluster
- **Multi-Controller Update**: VMMigration and VirtualMachine controllers both need changes
- **Testing Overhead**: Need comprehensive tests for both paths
- **Documentation Update**: Need to document the new ImportedDisk field

#### Deployment Impact
- **Breaking Change**: No (optional field, existing VMs unaffected)
- **Downtime Required**: No
- **Rollback Plan**: Simple (previous CRD version can be reapplied)
- **Estimated Effort**: 6-8 hours (development + testing)

---

### Solution 2: Create Synthetic VMImage for Migrated Disks

**Approach**: Auto-create a temporary VMImage resource that references the imported disk.

#### Implementation

```go
// In handleImportingPhase, after successful import:
func (r *VMMigrationReconciler) createSyntheticVMImage(
    ctx context.Context,
    migration *infrav1beta1.VMMigration,
) error {
    imageName := fmt.Sprintf("%s-migrated-disk", migration.Name)
    
    syntheticImage := &infrav1beta1.VMImage{
        ObjectMeta: metav1.ObjectMeta{
            Name:      imageName,
            Namespace: migration.Namespace,
            Labels: map[string]string{
                "virtrigaud.io/synthetic":  "true",
                "virtrigaud.io/migration":  migration.Name,
                "virtrigaud.io/disk-id":    migration.Status.ImportID,
            },
            Annotations: map[string]string{
                "virtrigaud.io/description": "Synthetic image for migrated disk",
            },
            OwnerReferences: []metav1.OwnerReference{
                {
                    APIVersion: migration.APIVersion,
                    Kind:       migration.Kind,
                    Name:       migration.Name,
                    UID:        migration.UID,
                    Controller: pointer.Bool(true),
                },
            },
        },
        Spec: infrav1beta1.VMImageSpec{
            Source: infrav1beta1.ImageSource{
                Libvirt: &infrav1beta1.LibvirtImageSource{
                    Path: fmt.Sprintf("/var/lib/libvirt/images/%s.qcow2", 
                        migration.Status.ImportID),
                    Format: migration.Status.DiskInfo.TargetFormat,
                },
            },
            Metadata: &infrav1beta1.ImageMetadata{
                Description: fmt.Sprintf("Migrated disk from %s", 
                    migration.Spec.Source.VMRef.Name),
            },
        },
    }
    
    if err := r.Create(ctx, syntheticImage); err != nil {
        return fmt.Errorf("failed to create synthetic VMImage: %w", err)
    }
    
    // Store image name in migration status
    migration.Status.SyntheticImageName = imageName
    
    return nil
}

// In handleCreatingPhase:
targetVM.Spec.ImageRef = infrav1beta1.ObjectRef{
    Name:      migration.Status.SyntheticImageName,
    Namespace: migration.Namespace,
}
```

#### Pros ✅
- **No CRD Changes**: Works with existing VirtualMachine CRD
- **Fast Implementation**: Minimal code changes
- **Existing Workflows**: Leverages current VM creation path
- **Automatic Cleanup**: OwnerReference ensures cleanup with migration

#### Cons ❌
- **Conceptual Confusion**: VMImage represents a template, not a one-off disk
- **Resource Proliferation**: Creates extra K8s resources for internal plumbing
- **Namespace Pollution**: Synthetic images clutter `kubectl get vmimage` output
- **Semantic Mismatch**: Misuses VMImage for something it wasn't designed for
- **Provider Confusion**: Providers need to handle synthetic images differently

#### Deployment Impact
- **Breaking Change**: No
- **Downtime Required**: No
- **Rollback Plan**: Simple
- **Estimated Effort**: 2-3 hours

---

### Solution 3: Annotation-Based Disk Reference (Workaround)

**Approach**: Use annotations to signal imported disk usage and relax validation.

#### Implementation

```go
// In handleCreatingPhase:
targetVM := &infrav1beta1.VirtualMachine{
    ObjectMeta: metav1.ObjectMeta{
        Name:      targetVMName,
        Namespace: targetNamespace,
        Annotations: map[string]string{
            "virtrigaud.io/migration": migration.Name,
            "virtrigaud.io/disk-source": "imported",
            "virtrigaud.io/imported-disk-id": migration.Status.ImportID,
            "virtrigaud.io/imported-disk-path": fmt.Sprintf(
                "/var/lib/libvirt/images/%s.qcow2",
                migration.Status.ImportID,
            ),
        },
    },
    Spec: infrav1beta1.VirtualMachineSpec{
        ProviderRef: migration.Spec.Target.ProviderRef,
        ClassRef: migration.Spec.Target.ClassRef,
        // Use a special marker imageRef
        ImageRef: infrav1beta1.ObjectRef{
            Name: "imported-disk", // Special marker value
        },
        Networks: migration.Spec.Target.Networks,
    },
}

// In VirtualMachine controller, buildCreateRequest:
func (r *VirtualMachineReconciler) buildCreateRequest(...) contracts.CreateRequest {
    var image contracts.VMImage
    
    // Check for imported disk annotation
    if diskSource, ok := vm.Annotations["virtrigaud.io/disk-source"]; ok && diskSource == "imported" {
        // Use imported disk path from annotation
        if diskPath, ok := vm.Annotations["virtrigaud.io/imported-disk-path"]; ok {
            image = contracts.VMImage{
                Path: diskPath,
                Format: "qcow2",
            }
        }
    } else {
        // Normal template-based creation
        image = extractImageFromVMImage(vmImage)
    }
    
    return contracts.CreateRequest{
        Image: image,
        // ...
    }
}
```

Create a placeholder VMImage:
```yaml
# Create once in cluster
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: imported-disk
  namespace: default
spec:
  source:
    libvirt:
      path: "/dev/null"  # Placeholder
  metadata:
    description: "Placeholder for migrated VMs"
```

#### Pros ✅
- **Minimal CRD Changes**: No structural changes to VirtualMachine
- **Quick Implementation**: Mostly controller logic
- **Flexible**: Annotations can evolve without CRD updates

#### Cons ❌
- **Brittle**: Relies on string matching and convention
- **Poor Discoverability**: Hard to understand without documentation
- **Weak Type Safety**: Annotation values not validated
- **Maintenance Burden**: Special-case logic scattered across controllers
- **Not Self-Documenting**: Behavior not obvious from CRD definition
- **Hack**: Feels like a workaround rather than a proper solution

#### Deployment Impact
- **Breaking Change**: No
- **Downtime Required**: No
- **Estimated Effort**: 3-4 hours

---

### Solution 4: Webhook Mutation for Migrated VMs

**Approach**: Use a mutating webhook to auto-populate ImageRef for migrated VMs.

#### Implementation

```go
// New file: internal/webhook/virtualmachine_mutating.go
type VirtualMachineMutator struct {
    Client client.Client
}

func (m *VirtualMachineMutator) Default(ctx context.Context, obj runtime.Object) error {
    vm := obj.(*infrav1beta1.VirtualMachine)
    
    // Check if this VM is from a migration
    if migrationName, ok := vm.Annotations["virtrigaud.io/migration"]; ok {
        // Fetch the migration resource
        migration := &infrav1beta1.VMMigration{}
        err := m.Client.Get(ctx, client.ObjectKey{
            Name:      migrationName,
            Namespace: vm.Namespace,
        }, migration)
        if err != nil {
            return err
        }
        
        // Auto-create synthetic image if needed
        imageName := fmt.Sprintf("%s-disk", vm.Name)
        
        // Check if synthetic image exists
        syntheticImage := &infrav1beta1.VMImage{}
        err = m.Client.Get(ctx, client.ObjectKey{
            Name:      imageName,
            Namespace: vm.Namespace,
        }, syntheticImage)
        
        if err != nil && apierrors.IsNotFound(err) {
            // Create synthetic image
            syntheticImage = &infrav1beta1.VMImage{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      imageName,
                    Namespace: vm.Namespace,
                    OwnerReferences: []metav1.OwnerReference{
                        {
                            APIVersion: vm.APIVersion,
                            Kind:       vm.Kind,
                            Name:       vm.Name,
                            UID:        vm.UID,
                            Controller: pointer.Bool(true),
                        },
                    },
                },
                Spec: infrav1beta1.VMImageSpec{
                    Source: infrav1beta1.ImageSource{
                        Libvirt: &infrav1beta1.LibvirtImageSource{
                            Path: fmt.Sprintf("/var/lib/libvirt/images/%s.qcow2", 
                                migration.Status.ImportID),
                        },
                    },
                },
            }
            
            if err := m.Client.Create(ctx, syntheticImage); err != nil {
                return err
            }
        }
        
        // Set ImageRef automatically
        if vm.Spec.ImageRef.Name == "" {
            vm.Spec.ImageRef = infrav1beta1.ObjectRef{
                Name:      imageName,
                Namespace: vm.Namespace,
            }
        }
    }
    
    return nil
}
```

#### Pros ✅
- **Transparent**: Controllers don't need special handling
- **Centralized**: All mutation logic in one place
- **Automatic**: Happens seamlessly during VM creation

#### Cons ❌
- **Webhook Complexity**: Adds operational overhead
- **Race Conditions**: Webhook needs to query migration state
- **Dependency**: Webhook must be available or VM creation blocks
- **Still Creates Synthetic Images**: Same issues as Solution 2
- **Harder Debugging**: Mutation happens "invisibly"

#### Deployment Impact
- **Breaking Change**: No
- **Downtime Required**: No
- **Webhook Registration Required**: Yes
- **Estimated Effort**: 6-8 hours

---

## Comparison Matrix

| Solution | Type Safety | Scalability | Maintainability | Implementation Time | Enterprise Readiness |
|----------|-------------|-------------|-----------------|---------------------|---------------------|
| **1. Optional ImageRef + ImportedDisk** | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐⭐ | 6-8 hours | ⭐⭐⭐⭐⭐ |
| **2. Synthetic VMImage** | ⭐⭐⭐ | ⭐⭐⭐ | ⭐⭐ | 2-3 hours | ⭐⭐ |
| **3. Annotation-Based** | ⭐⭐ | ⭐⭐⭐ | ⭐⭐ | 3-4 hours | ⭐⭐ |
| **4. Webhook Mutation** | ⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐ | 6-8 hours | ⭐⭐⭐ |

---

## Recommendation

### Primary Recommendation: **Solution 1 - Optional ImageRef with ImportedDisk**

**Rationale**:
1. **Architecturally Sound**: Properly models the two distinct VM creation paths
2. **Kubernetes Native**: Follows K8s patterns for mutually exclusive fields
3. **Type Safe**: Validation enforced at the API layer
4. **Future-Proof**: Extensible to other disk import scenarios
5. **Self-Documenting**: API clearly shows what's happening
6. **Maintainable**: Clean separation of concerns

### Quick Fix: **Solution 2 - Synthetic VMImage** (if immediate unblock needed)

If you need to unblock migrations within 1-2 hours, use Solution 2 as a temporary measure, then migrate to Solution 1 in the next sprint.

---

## Implementation Plan for Solution 1

### Phase 1: API Changes (2 hours)
1. Add `ImportedDiskRef` type to `virtualmachine_types.go`
2. Make `ImageRef` optional (`*ObjectRef`)
3. Add webhook validation for mutual exclusivity
4. Regenerate CRD manifests (`make manifests`)
5. Update API documentation

### Phase 2: Controller Updates (3 hours)
1. Update `VMMigrationReconciler.handleCreatingPhase`:
   - Set `ImportedDisk` instead of leaving `ImageRef` empty
   - Add disk path information
2. Update `VirtualMachineReconciler.buildCreateRequest`:
   - Add logic to handle `ImportedDisk`
   - Convert disk ID to path for providers
3. Add unit tests for both scenarios

### Phase 3: Provider Updates (1 hour)
1. Verify libvirt provider handles imported disk paths correctly
2. Add logging for imported disk scenarios
3. Test with actual migrated disk

### Phase 4: Testing & Validation (2 hours)
1. Unit tests for CRD validation
2. Integration test: Complete migration end-to-end
3. Test normal VM creation (ensure no regression)
4. Test error cases (no ImageRef and no ImportedDisk)

### Phase 5: Documentation (1 hour)
1. Update migration guide
2. Document ImportedDisk field in API reference
3. Add example manifests
4. Update troubleshooting guide

---

## Risk Mitigation

### Risk 1: CRD Update Failure
- **Mitigation**: Test CRD update in dev cluster first
- **Rollback**: Previous CRD version can be quickly reapplied

### Risk 2: Breaking Existing VMs
- **Mitigation**: `ImageRef` made optional, not removed
- **Validation**: Existing VMs continue to work as-is
- **Testing**: Comprehensive backward compatibility tests

### Risk 3: Provider Compatibility
- **Mitigation**: Libvirt provider already supports file-based images
- **Validation**: Test with imported disks before rollout
- **Fallback**: Can default to existing path if new field empty

---

## Next Steps

1. **Decision**: Review and approve solution (recommend Solution 1)
2. **Branch**: Create feature branch `feature/imported-disk-support`
3. **Implementation**: Follow phased plan above
4. **Review**: Code review with focus on backward compatibility
5. **Deploy**: Dev → Staging → Production
6. **Monitor**: Watch for any issues with existing VMs
7. **Document**: Update all relevant documentation

---

## Additional Considerations

### Multi-Provider Support
The recommended solution (Solution 1) works across providers:
- **Libvirt**: Path-based disk references
- **vSphere**: Can reference existing VMDK by datastore path
- **Proxmox**: Can reference existing qcow2/raw disks
- **Others**: Extensible to any provider supporting disk paths

### Compliance & Audit
The `ImportedDisk.MigrationRef` field provides:
- Traceability: Which migration created this VM
- Compliance: Audit trail for disk provenance
- Troubleshooting: Easy to track disk history

### Performance
No performance impact:
- No additional API calls
- No complex computations
- Simple field check in controller logic

---

**Document Version**: 1.0  
**Date**: 2025-11-08  
**Status**: Draft for Review

