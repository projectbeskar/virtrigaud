/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vsphere

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Post-create steps.
//
// After CreateVM_Task / CloneVM_Task, Create grows the primary disk to the
// VMClass disk size and attaches the additional disks. A failure there used to
// be logged and Create returned success, so the VM silently lacked a disk; and
// a Create retried after the manager's deadline bound the existing VM without
// redoing the skipped steps.
//
// The steps are now idempotent and run on BOTH paths — after a fresh create,
// and when a retried create binds the requester's own VM — and a failure fails
// the Create with a retryable error (codes.Unavailable with
// VM_OPERATION_FAILED, kept out of the manager's circuit breaker). The VM is
// left in place; the manager's next Create binds it and re-runs the steps:
//
//   - the disk grow is grow-only (resizeVMDisk): a disk at or above the size
//     is left alone;
//   - additional disk N is recorded in the VM's ExtraConfig under
//     additionalDiskExtraConfigKey(N), in the same ReconfigVM_Task that adds it,
//     so a disk is never added twice.
//
// A VM created by an earlier release carries no postCreateMarkerKey: its steps
// ran (or were skipped) when it was created, its disks are not recorded, and
// re-running them could add duplicate disks, so a binding create leaves it
// alone.

const (
	// postCreateMarkerKey marks a VM created by a release whose post-create
	// steps are idempotent (see above). Its value is postCreateMarkerValue.
	postCreateMarkerKey = "virtrigaud.create.poststeps"
	// postCreateMarkerValue is the current post-create steps version.
	postCreateMarkerValue = "v1"
	// additionalDiskKeyPrefix prefixes the ExtraConfig key recording
	// additional disk N (1-based) as attached; its value is the disk's file.
	additionalDiskKeyPrefix = "virtrigaud.disk.additional."
	// additionalDiskFileInfix forms an additional disk's file name,
	// "<vm>-additional-<N>.vmdk": distinct from the "<vm>.vmdk" and
	// "<vm>_<N>.vmdk" names vCenter gives a clone's disks, so it never collides
	// with a disk the template brought.
	additionalDiskFileInfix = "-additional-"
)

// additionalDiskExtraConfigKey is the ExtraConfig key recording additional disk
// index (1-based) as attached.
func additionalDiskExtraConfigKey(index int) string {
	return additionalDiskKeyPrefix + strconv.Itoa(index)
}

// additionalDiskFileName is the file name of additional disk index of vmName.
func additionalDiskFileName(vmName string, index int) string {
	return vmName + additionalDiskFileInfix + strconv.Itoa(index) + vmdkExt
}

// postCreateExtraConfig returns the ExtraConfig entries a create writes into
// the new VM's config spec: the marker, and every additional-disk record the
// spec's disks use cleared ("" removes a key), so records a clone inherits
// from its template never make Create skip a disk.
func postCreateExtraConfig(spec *VMSpec) []types.BaseOptionValue {
	out := []types.BaseOptionValue{&types.OptionValue{Key: postCreateMarkerKey, Value: postCreateMarkerValue}}
	for i := range spec.AdditionalDisks {
		out = append(out,
			&types.OptionValue{Key: additionalDiskExtraConfigKey(i + 1), Value: ""},
			&types.OptionValue{Key: additionalDiskNameKey(i + 1), Value: ""})
	}
	return out
}

// additionalDiskRecordExtraConfig returns the ExtraConfig entries recording
// additional disk index (1-based) as attached: its file, and the spec.disks
// name it was attached for (when it has one), by which Reconfigure finds the
// disk of a spec.disks entry even after the list was edited.
func additionalDiskRecordExtraConfig(index int, file, name string) []types.BaseOptionValue {
	out := []types.BaseOptionValue{&types.OptionValue{Key: additionalDiskExtraConfigKey(index), Value: file}}
	if name != "" {
		out = append(out, &types.OptionValue{Key: additionalDiskNameKey(index), Value: name})
	}
	return out
}

// extraConfigValue returns the string value of key (case-insensitive, as VMX
// keys are) in extraConfig, and whether it is set to a non-empty string.
func extraConfigValue(extraConfig []types.BaseOptionValue, key string) (string, bool) {
	for _, bov := range extraConfig {
		if bov == nil {
			continue
		}
		ov := bov.GetOptionValue()
		if ov == nil || !strings.EqualFold(ov.Key, key) {
			continue
		}
		s, ok := ov.Value.(string)
		return s, ok && s != ""
	}
	return "", false
}

// errPostCreateStep logs a failed post-create step and returns the retryable
// error the requester sees. The vCenter fault can name datastores and files,
// so it stays in the provider log.
func (p *Provider) errPostCreateStep(vmName, vmID, step string, err error) error {
	p.logger.Error("Post-create step failed; the VM is kept and the next create completes it",
		"vm_name", vmName, "vm_id", vmID, "step", step, "error", err)
	msg := fmt.Sprintf("VM %q was created but %s failed (details are in the provider log); "+
		"the create is retried and completes it", vmName, step)
	return statusWithReasons(codes.Unavailable, msg, contracts.VMOperationFailedReason).Err()
}

// applyPostCreateSteps grows vm's primary disk to spec.DiskSizeGB (grow-only)
// and attaches spec.AdditionalDisks not yet recorded as attached. It is
// idempotent, so a retried create re-runs it safely. The first failure is
// returned (errPostCreateStep).
func (p *Provider) applyPostCreateSteps(ctx context.Context, vm *object.VirtualMachine, spec *VMSpec) error {
	vmID := vm.Reference().Value
	if spec.DiskSizeGB > 0 {
		if err := p.resizeVMDisk(ctx, vm, spec.DiskSizeGB, vmID); err != nil {
			return p.errPostCreateStep(spec.Name, vmID, "growing its primary disk to the VMClass disk size", err)
		}
	}
	for i, diskSpec := range spec.AdditionalDisks {
		if err := p.attachAdditionalDisk(ctx, vm, diskSpec, i+1); err != nil {
			return p.errPostCreateStep(spec.Name, vmID, fmt.Sprintf("attaching additional disk %d", i+1), err)
		}
	}
	return nil
}

// applyPostCreateStepsOnBind re-runs the post-create steps on a VM a retried
// create bound. A VM without postCreateMarkerKey (created by an earlier
// release) is left alone.
func (p *Provider) applyPostCreateStepsOnBind(ctx context.Context, vmID string, spec *VMSpec) error {
	if spec.DiskSizeGB <= 0 && len(spec.AdditionalDisks) == 0 {
		return nil
	}
	vm := object.NewVirtualMachine(p.client.Client, types.ManagedObjectReference{Type: virtualMachineMoType, Value: vmID})
	var vmMo mo.VirtualMachine
	if err := property.DefaultCollector(p.client.Client).RetrieveOne(ctx, vm.Reference(),
		[]string{propConfigExtraConfig}, &vmMo); err != nil {
		return p.errPostCreateStep(spec.Name, vmID, "reading the VM's configuration", err)
	}
	if vmMo.Config == nil {
		return p.errPostCreateStep(spec.Name, vmID, "reading the VM's configuration",
			fmt.Errorf("VM %s has no readable configuration", vmID))
	}
	if v, ok := extraConfigValue(vmMo.Config.ExtraConfig, postCreateMarkerKey); !ok || v != postCreateMarkerValue {
		p.logger.Info("Bound VM predates idempotent post-create steps; leaving its disks as they are",
			"vm_name", spec.Name, "vm_id", vmID)
		return nil
	}
	return p.applyPostCreateSteps(ctx, vm, spec)
}
