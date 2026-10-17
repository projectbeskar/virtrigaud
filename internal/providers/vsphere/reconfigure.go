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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Reconfigure: what it changes, and what a running VM can take.
//
// CPU and memory are set to the VMClass values (with any spec.resources
// override applied).
//
// Disks. Reconfigure never touches the primary (root) disk: the VMClass
// spec.diskDefaults.size is applied by Create only. It grows only the
// additional disks Create attached for spec.disks entries, each found by the
// spec.disks name recorded with it when it was attached
// (additionalDiskNameKey) and then by the file its record names (or that
// file's base name, after a storage migration). Such a disk is grown to the
// entry's size; a smaller size is refused (InvalidArgument, nothing changed) —
// a disk is never shrunk — and a size of 0 (the manager withholds a growth
// above the disk-size limit that way) leaves it. Left alone, and logged:
//
//   - an entry no disk is recorded under its name for: added to spec.disks
//     after the VM was created, or a disk an earlier release recorded without
//     a name (Reconfigure never matches by position: the list may have been
//     edited since), or an adopted VM. Reconfigure never adds a disk;
//   - an entry whose name is ambiguous: used by more than one spec.disks
//     entry, or recorded on more than one disk;
//   - a disk vCenter cannot extend: a delta disk (the VM has snapshots, or it
//     is a linked clone) or a physical-mode RDM.
//
// What a VM that is not powered off can take. A suspended VM takes no
// hardware change at all. A running VM changes its CPU count only with CPU
// hot-add (more vCPUs) or hot-remove (fewer) enabled on it, its memory only
// upwards, with memory hot-add enabled and within its hot-plug memory limit,
// and a disk on a shared SCSI bus or with multi-writer sharing only while it
// is off. vSphere has no pending state to stage anything for the next boot.
// So such a change is refused before anything is changed — the rest of the
// request included — with FailedPrecondition carrying
// RECONFIGURE_NEEDS_POWER_OFF, which the manager shows as
// Reconfiguring=False/ResizePendingPowerOff; the same Reconfigure sent once the
// VM is powered off applies it.
//
// A CPU count that is not a multiple of the VM's cores per socket is refused
// (InvalidArgument) before anything is changed. A ReconfigVM_Task that vCenter
// fails is answered with VM_OPERATION_FAILED and a constant message (the fault
// is logged), so one VM's failing reconfigure never counts toward the
// manager's per-Provider circuit breaker.

const (
	// additionalDiskNameKeySuffix forms the ExtraConfig key recording the
	// spec.disks name of additional disk N: "<additionalDiskKeyPrefix>N.name".
	additionalDiskNameKeySuffix = ".name"
	// kibPerGiB is the number of KiB in a GiB (VirtualDisk.CapacityInKB).
	kibPerGiB int64 = 1024 * 1024
	// diskShrinkRefusedFormat is the refusal of a spec.disks size below the
	// disk's current size (name, requested GiB, current GiB, current GiB).
	diskShrinkRefusedFormat = "spec.disks[%s] asks for %d GiB but the disk already has %d GiB: a disk is never shrunk; " +
		"set its size to %d GiB or more"
	// cpuNotMultipleOfCoresFormat is the refusal of a CPU count that is not a
	// multiple of the VM's cores per socket (vCPUs, cores per socket).
	cpuNotMultipleOfCoresFormat = "%d vCPUs is not a multiple of the VM's %d cores per socket, which vCenter refuses; " +
		"ask for a multiple of it"
	// reconfigureNeedsPowerOffFormat is the refusal of a change the VM cannot
	// take unless it is powered off (the reasons, joined).
	reconfigureNeedsPowerOffFormat = "the VM is not powered off and cannot take this change now: %s; nothing was changed, " +
		"and the change is applied once the VM is powered off"
	// reconfigureTaskFailedMessage is the answer to a ReconfigVM_Task vCenter
	// failed; the fault (which can name datastores and files) is logged.
	reconfigureTaskFailedMessage = "vCenter failed the reconfigure task of this VM (details are in the provider log); " +
		"it is retried"
	// suspendedVMReason is why a suspended VM takes no change.
	suspendedVMReason = "it is suspended, and vCenter changes no hardware of a suspended VM"
)

// additionalDiskNameKey is the ExtraConfig key recording the spec.disks name
// of additional disk index (1-based).
func additionalDiskNameKey(index int) string {
	return additionalDiskExtraConfigKey(index) + additionalDiskNameKeySuffix
}

// reconfigureDesired is the part of a marshaled contracts.CreateRequest a
// vSphere Reconfigure applies. contracts.VMClass and DiskSpec carry no json
// tags, so the keys are their Go field names. Class.DiskDefaults is not read:
// the VMClass disk size is applied by Create only.
type reconfigureDesired struct {
	Class struct {
		CPU       int32 `json:"CPU"`
		MemoryMiB int32 `json:"MemoryMiB"`
	} `json:"Class"`
	Disks []struct {
		Name    string `json:"Name"`
		SizeGiB int32  `json:"SizeGiB"`
	} `json:"Disks"`
}

// reconfigureProperties are the VM properties a Reconfigure reads.
var reconfigureProperties = []string{
	"config.hardware.numCPU",
	"config.hardware.numCoresPerSocket",
	"config.hardware.memoryMB",
	"config.hardware.device",
	"config.cpuHotAddEnabled",
	"config.cpuHotRemoveEnabled",
	"config.memoryHotAddEnabled",
	"config.hotPlugMemoryLimit",
	"runtime.powerState",
	propConfigTemplate,
	propConfigExtraConfig,
}

// reconfigureVM applies req.DesiredJson to vm (the body of Reconfigure, after
// the VM reference is resolved): it reads the VM, plans the change
// (planReconfigure) and runs it as one ReconfigVM_Task.
func (p *Provider) reconfigureVM(ctx context.Context, vm *object.VirtualMachine, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	var vmMo mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), reconfigureProperties, &vmMo); err != nil {
		return nil, fmt.Errorf("failed to get VM properties: %w", err)
	}
	// Never resize a template or an image artifact (vm_artifact_guard.go).
	if err := p.checkNotImageArtifact(req.Id, "reconfigure", vmMo.Config); err != nil {
		return nil, err
	}
	if vmMo.Config == nil {
		return nil, fmt.Errorf("VM %s has no readable configuration", req.Id)
	}

	var desired reconfigureDesired
	if err := json.Unmarshal([]byte(req.DesiredJson), &desired); err != nil {
		return nil, fmt.Errorf("failed to parse desired configuration: %w", err)
	}

	spec, err := p.planReconfigure(req.Id, &vmMo, &desired)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		p.logger.Info("No configuration changes needed", "vm_id", req.Id)
		return &providerv1.TaskResponse{}, nil
	}

	p.logger.Info("Applying VM reconfiguration", "vm_id", req.Id)
	t, err := vm.Reconfigure(ctx, *spec)
	if err != nil {
		return nil, fmt.Errorf("failed to start reconfiguration: %w", err)
	}
	if _, err := t.WaitForResult(ctx, nil); err != nil {
		if isTaskFault(err) {
			p.logger.Error("vCenter failed the reconfigure task", "vm_id", req.Id, "error", err)
			return nil, statusWithReasons(codes.Unknown, reconfigureTaskFailedMessage, contracts.VMOperationFailedReason).Err()
		}
		return nil, fmt.Errorf("reconfiguration failed: %w", err)
	}
	p.logger.Info("VM reconfigured successfully", "vm_id", req.Id)

	// Completed synchronously. RestartRequired is false: vSphere has no
	// pending state — ReconfigVM_Task applied the change, or it was refused
	// above (a VM that cannot take it now) or failed.
	return &providerv1.TaskResponse{RestartRequired: false}, nil
}

// isTaskFault reports whether err is a vCenter task's fault (the task ran and
// failed), as opposed to a failure to reach vCenter or to wait for the task.
func isTaskFault(err error) bool {
	var v task.Error
	var ptr *task.Error
	return stderrors.As(err, &v) || stderrors.As(err, &ptr)
}

// planReconfigure builds the config spec that takes the VM vmMo (read with
// reconfigureProperties) to desired, or nil when nothing changes. Before
// anything is changed it refuses a CPU count that is not a multiple of the
// cores per socket and a spec.disks size below its disk's (InvalidArgument),
// and a change the VM cannot take unless it is powered off
// (FailedPrecondition, RECONFIGURE_NEEDS_POWER_OFF).
func (p *Provider) planReconfigure(vmID string, vmMo *mo.VirtualMachine, desired *reconfigureDesired) (*types.VirtualMachineConfigSpec, error) {
	cfg := vmMo.Config
	spec := &types.VirtualMachineConfigSpec{}
	changed := false

	cpuFrom, cpuTo := cfg.Hardware.NumCPU, cfg.Hardware.NumCPU
	if desired.Class.CPU > 0 && desired.Class.CPU != cfg.Hardware.NumCPU {
		cpuTo = desired.Class.CPU
		if cps := cfg.Hardware.NumCoresPerSocket; cps > 1 && cpuTo%cps != 0 {
			return nil, sdkerrors.NewInvalidSpec(cpuNotMultipleOfCoresFormat, cpuTo, cps)
		}
		p.logger.Info("CPU change requested", "vm_id", vmID, "old", cpuFrom, "new", cpuTo)
		spec.NumCPUs = cpuTo
		changed = true
	}
	// MemoryMiB is MiB, the unit of vSphere's MemoryMB.
	memFrom, memTo := int64(cfg.Hardware.MemoryMB), int64(cfg.Hardware.MemoryMB)
	if desired.Class.MemoryMiB > 0 && int64(desired.Class.MemoryMiB) != memFrom {
		memTo = int64(desired.Class.MemoryMiB)
		p.logger.Info("Memory change requested", "vm_id", vmID, "old_mb", memFrom, "new_mb", memTo)
		spec.MemoryMB = memTo
		changed = true
	}

	edits, offOnly, err := p.planDiskGrowth(vmID, cfg, desired)
	if err != nil {
		return nil, err
	}
	if len(edits) > 0 {
		spec.DeviceChange = edits
		changed = true
	}
	if !changed {
		return nil, nil
	}

	var reasons []string
	switch vmMo.Runtime.PowerState {
	case types.VirtualMachinePowerStatePoweredOff:
	case types.VirtualMachinePowerStateSuspended:
		reasons = []string{suspendedVMReason}
	default:
		reasons = append(onlineResizeBlockers(cfg, cpuFrom, cpuTo, memFrom, memTo), offOnly...)
	}
	if len(reasons) > 0 {
		p.logger.Info("Refusing a change the VM cannot take unless it is powered off; it is applied once the VM is off",
			"vm_id", vmID, "power_state", vmMo.Runtime.PowerState, "reasons", reasons)
		return nil, statusWithReasons(codes.FailedPrecondition,
			fmt.Sprintf(reconfigureNeedsPowerOffFormat, strings.Join(reasons, "; ")),
			contracts.ReconfigureNeedsPowerOffReason).Err()
	}
	return spec, nil
}

// onlineResizeBlockers lists why a running VM cannot take a change of its CPU
// count from cpuFrom to cpuTo and its memory from memFrom to memTo MiB, or
// nothing when it can (cfg: its config).
func onlineResizeBlockers(cfg *types.VirtualMachineConfigInfo, cpuFrom, cpuTo int32, memFrom, memTo int64) []string {
	var reasons []string
	switch {
	case cpuTo > cpuFrom && !boolValue(cfg.CpuHotAddEnabled):
		reasons = append(reasons, "CPU hot-add is not enabled on it")
	case cpuTo < cpuFrom && !boolValue(cfg.CpuHotRemoveEnabled):
		reasons = append(reasons, "CPU hot-remove is not enabled on it")
	}
	switch {
	case memTo > memFrom && !boolValue(cfg.MemoryHotAddEnabled):
		reasons = append(reasons, "memory hot-add is not enabled on it")
	case memTo > memFrom && cfg.HotPlugMemoryLimit > 0 && memTo > cfg.HotPlugMemoryLimit:
		reasons = append(reasons, fmt.Sprintf("%d MiB is above its hot-plug memory limit of %d MiB", memTo, cfg.HotPlugMemoryLimit))
	case memTo < memFrom:
		reasons = append(reasons, "memory is never removed from a running VM")
	}
	return reasons
}

// boolValue is *b, false for nil.
func boolValue(b *bool) bool {
	return b != nil && *b
}

// additionalDiskRecord is an additional disk Create recorded in the VM's
// ExtraConfig: its 1-based index, the disk file, and its spec.disks name
// ("" when recorded without one).
type additionalDiskRecord struct {
	index int
	file  string
	name  string
}

// additionalDiskRecords reads the additional-disk records of extraConfig, in
// index order. Keys are matched case-insensitively, as VMX keys are.
func additionalDiskRecords(extraConfig []types.BaseOptionValue) []additionalDiskRecord {
	byIndex := map[int]*additionalDiskRecord{}
	prefix := strings.ToLower(additionalDiskKeyPrefix)
	for _, bov := range extraConfig {
		if bov == nil {
			continue
		}
		ov := bov.GetOptionValue()
		if ov == nil {
			continue
		}
		rest, ok := strings.CutPrefix(strings.ToLower(ov.Key), prefix)
		if !ok {
			continue
		}
		value, _ := ov.Value.(string)
		idxText, isName := strings.CutSuffix(rest, additionalDiskNameKeySuffix)
		index, err := strconv.Atoi(idxText)
		if err != nil || index < 1 {
			continue
		}
		rec := byIndex[index]
		if rec == nil {
			rec = &additionalDiskRecord{index: index}
			byIndex[index] = rec
		}
		if isName {
			rec.name = value
		} else {
			rec.file = value
		}
	}
	out := make([]additionalDiskRecord, 0, len(byIndex))
	for _, rec := range byIndex {
		if rec.file != "" {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

// specDiskMatch is the record of the disk Create attached for one spec.disks
// entry, or why there is none.
type specDiskMatch struct {
	rec *additionalDiskRecord
	why string
}

// Reasons a spec.disks entry has no disk (specDiskMatch.why).
const (
	specDiskUnrecorded = "no disk is recorded under this name (added after the VM was created, or recorded " +
		"by an earlier release without a name)"
	specDiskNameRepeated = "the name is used by more than one spec.disks entry"
	specDiskNameAmbiguous = "more than one disk is recorded under this name"
)

// matchSpecDisks maps each entry of names (the spec.disks names, in order) to
// the record of the disk Create attached for it: the one record carrying the
// entry's name. A name used by more than one entry, or recorded on more than
// one disk, matches nothing; neither does a record without a name.
func matchSpecDisks(names []string, records []additionalDiskRecord) []specDiskMatch {
	inSpec := map[string]int{}
	for _, n := range names {
		inSpec[n]++
	}
	byName := map[string][]*additionalDiskRecord{}
	for i := range records {
		if rec := &records[i]; rec.name != "" {
			byName[rec.name] = append(byName[rec.name], rec)
		}
	}
	out := make([]specDiskMatch, len(names))
	for i, name := range names {
		switch recs := byName[name]; {
		case inSpec[name] > 1:
			out[i].why = specDiskNameRepeated
		case len(recs) > 1:
			out[i].why = specDiskNameAmbiguous
		case len(recs) == 0 || name == "":
			out[i].why = specDiskUnrecorded
		default:
			out[i].rec = recs[0]
		}
	}
	return out
}

// diskFile returns the backing file of disk, or "".
func diskFile(disk *types.VirtualDisk) string {
	if fb, ok := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
		return fb.GetVirtualDeviceFileBackingInfo().FileName
	}
	return ""
}

// datastoreFileBase returns the base name of a datastore path
// ("[ds] dir/x.vmdk" → "x.vmdk").
func datastoreFileBase(p string) string {
	var dp object.DatastorePath
	if dp.FromString(p) {
		return path.Base(dp.Path)
	}
	return path.Base(p)
}

// findRecordedDisk returns the disk of disks whose file is rec's — or, when no
// disk has it (the files moved), the one disk whose file has the same base
// name — or nil.
func findRecordedDisk(disks []*types.VirtualDisk, rec *additionalDiskRecord) *types.VirtualDisk {
	for _, d := range disks {
		if diskFile(d) == rec.file {
			return d
		}
	}
	base := datastoreFileBase(rec.file)
	var found *types.VirtualDisk
	for _, d := range disks {
		if f := diskFile(d); f != "" && datastoreFileBase(f) == base {
			if found != nil {
				return nil // ambiguous: touch neither
			}
			found = d
		}
	}
	return found
}

// diskNotExtendable says why vCenter cannot extend disk — a delta disk (the
// VM has snapshots, or it is a linked clone) or a physical-mode RDM — or "".
func diskNotExtendable(disk *types.VirtualDisk) string {
	const delta = "it is a delta disk (the VM has snapshots, or it is a linked clone)"
	switch b := disk.Backing.(type) {
	case *types.VirtualDiskFlatVer2BackingInfo:
		if b.Parent != nil {
			return delta
		}
	case *types.VirtualDiskSeSparseBackingInfo:
		if b.Parent != nil {
			return delta
		}
	case *types.VirtualDiskSparseVer2BackingInfo:
		if b.Parent != nil {
			return delta
		}
	case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
		if b.CompatibilityMode == string(types.VirtualDiskCompatibilityModePhysicalMode) {
			return "it is a physical-mode RDM"
		}
		if b.Parent != nil {
			return delta
		}
	}
	return ""
}

// diskSharing returns the sharing mode of disk's backing ("" when unset).
func diskSharing(disk *types.VirtualDisk) string {
	switch b := disk.Backing.(type) {
	case *types.VirtualDiskFlatVer2BackingInfo:
		return b.Sharing
	case *types.VirtualDiskRawDiskMappingVer1BackingInfo:
		return b.Sharing
	}
	return ""
}

// diskGrowNeedsPowerOff says why disk can be grown only while the VM is off —
// it is on a SCSI controller whose bus is shared, or it is shared
// multi-writer — or "" (name: its spec.disks entry; devices: the VM's devices).
func diskGrowNeedsPowerOff(name string, disk *types.VirtualDisk, devices []types.BaseVirtualDevice) string {
	if diskSharing(disk) == string(types.VirtualDiskSharingSharingMultiWriter) {
		return fmt.Sprintf("the disk of spec.disks[%s] has multi-writer sharing", name)
	}
	for _, dev := range devices {
		ctrl, ok := dev.(types.BaseVirtualSCSIController)
		if !ok {
			continue
		}
		c := ctrl.GetVirtualSCSIController()
		if c.Key == disk.ControllerKey && c.SharedBus != "" && c.SharedBus != types.VirtualSCSISharingNoSharing {
			return fmt.Sprintf("the disk of spec.disks[%s] is on a SCSI controller with a shared bus (%s)", name, c.SharedBus)
		}
	}
	return ""
}

// grownDisk returns the edit that grows disk to sizeGiB.
func grownDisk(disk *types.VirtualDisk, sizeGiB int64) types.BaseVirtualDeviceConfigSpec {
	grown := *disk
	grown.CapacityInKB = sizeGiB * kibPerGiB
	grown.CapacityInBytes = sizeGiB * kibPerGiB * 1024
	return &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: &grown}
}

// planDiskGrowth returns the edits that grow each spec.disks entry's own disk
// to its size on the VM whose config is cfg, and why any of them needs the VM
// powered off (offOnly). A spec.disks size below its disk's is refused
// (InvalidArgument) before anything is changed. The primary disk is never
// touched.
func (p *Provider) planDiskGrowth(vmID string, cfg *types.VirtualMachineConfigInfo, desired *reconfigureDesired) (edits []types.BaseVirtualDeviceConfigSpec, offOnly []string, err error) {
	var disks []*types.VirtualDisk
	for _, dev := range cfg.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			disks = append(disks, d)
		}
	}
	names := make([]string, len(desired.Disks))
	for i, d := range desired.Disks {
		names[i] = d.Name
	}
	for i, m := range matchSpecDisks(names, additionalDiskRecords(cfg.ExtraConfig)) {
		want := int64(desired.Disks[i].SizeGiB)
		if want <= 0 {
			continue // withheld (above the disk-size limit) or unset: leave it
		}
		if m.rec == nil {
			p.logger.Info("Leaving a spec.disks entry's disk as it is: no single disk is recorded for it; Reconfigure never adds a disk",
				"vm_id", vmID, "disk_name", names[i], "why", m.why)
			continue
		}
		disk := findRecordedDisk(disks, m.rec)
		if disk == nil {
			p.logger.Info("Leaving a spec.disks entry's disk as it is: its recorded disk is not on the VM",
				"vm_id", vmID, "disk_name", names[i], "disk_index", m.rec.index)
			continue
		}
		if why := diskNotExtendable(disk); why != "" {
			p.logger.Info("Leaving a spec.disks entry's disk as it is: vCenter cannot extend it",
				"vm_id", vmID, "disk_name", names[i], "disk_index", m.rec.index, "why", why)
			continue
		}
		currentGiB := disk.CapacityInKB / kibPerGiB
		if want < currentGiB {
			return nil, nil, sdkerrors.NewInvalidSpec(diskShrinkRefusedFormat, names[i], want, currentGiB, currentGiB)
		}
		if want*kibPerGiB <= disk.CapacityInKB {
			continue
		}
		p.logger.Info("Additional disk grow requested", "vm_id", vmID, "disk_name", names[i],
			"disk_index", m.rec.index, "old_gb", currentGiB, "new_gb", want)
		if why := diskGrowNeedsPowerOff(names[i], disk, cfg.Hardware.Device); why != "" {
			offOnly = append(offOnly, why)
		}
		edits = append(edits, grownDisk(disk, want))
	}
	return edits, offOnly, nil
}
