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
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Reconfigure: which disk each size goes to, and what a running VM can take.
//
// Disks. A Reconfigure resizes the disks Create made, matched the way Create
// made them (post_create.go):
//
//   - the primary disk — the first VirtualDisk that is not a recorded
//     additional disk, the disk Create grows — to the VMClass
//     spec.diskDefaults.size (Class.DiskDefaults.SizeGiB). That size is a
//     floor, as at Create: a disk already that large (a template's larger
//     disk) is left as it is;
//   - spec.disks[i] (Disks[i]) to the additional disk Create attached for it:
//     the disk recorded under the name record (additionalDiskNameKey) holding
//     the entry's name — the n-th entry of a name to the n-th disk recorded
//     with it — or, on a VM whose disk was recorded without a name (created
//     before names were recorded), the disk recorded at position i+1. The
//     disk is found by the file its record names (or that file's base name,
//     after a storage migration). Its size is the disk's size: it is grown,
//     and a smaller size is refused (InvalidArgument, nothing changed) —
//     a disk is never shrunk.
//
// A size of 0 is "leave this disk as it is" (the manager withholds a growth
// above the disk-size limit that way). A spec.disks entry with no disk
// recorded for it — added to spec.disks after the VM was created, or on a VM
// created before Create recorded its disks — is left alone and logged:
// Reconfigure never adds a disk.
//
// CPU and memory on a running VM. vCenter changes the CPU count of a powered-on
// VM only with CPU hot-add (more vCPUs) or hot-remove (fewer) enabled on it,
// and its memory only upwards, with memory hot-add enabled and within the
// VM's hot-plug memory limit. Otherwise the change cannot be applied, and
// vSphere has no pending state to stage it for the next boot. So it is
// refused before anything is changed — disk growth included — with
// FailedPrecondition carrying RECONFIGURE_NEEDS_POWER_OFF, which the manager
// shows as Reconfiguring=False/ResizePendingPowerOff; the same Reconfigure
// sent once the VM is powered off applies it. A suspended VM is treated as
// powered on.

const (
	// additionalDiskNameKeySuffix forms the ExtraConfig key recording the
	// spec.disks name of additional disk N: "<additionalDiskKeyPrefix>N.name".
	additionalDiskNameKeySuffix = ".name"
	// kibPerGiB is the number of KiB in a GiB (VirtualDisk.CapacityInKB).
	kibPerGiB int64 = 1024 * 1024
	// diskShrinkRefusedFormat is the refusal of a spec.disks size below the
	// disk's current size (name, requested GiB, current GiB).
	diskShrinkRefusedFormat = "spec.disks[%s] asks for %d GiB but the disk already has %d GiB: a disk is never shrunk; " +
		"set its size to %d GiB or more"
	// reconfigureNeedsPowerOffFormat is the refusal of a CPU/memory change the
	// running VM cannot take (the reasons, joined).
	reconfigureNeedsPowerOffFormat = "the VM is powered on and cannot take this change online: %s; nothing was changed, " +
		"and the change is applied once the VM is powered off"
)

// additionalDiskNameKey is the ExtraConfig key recording the spec.disks name
// of additional disk index (1-based).
func additionalDiskNameKey(index int) string {
	return additionalDiskExtraConfigKey(index) + additionalDiskNameKeySuffix
}

// reconfigureDesired is the part of a marshaled contracts.CreateRequest a
// vSphere Reconfigure applies. contracts.VMClass, DiskDefaults and DiskSpec
// carry no json tags, so the keys are their Go field names.
type reconfigureDesired struct {
	Class struct {
		CPU          int32 `json:"CPU"`
		MemoryMiB    int32 `json:"MemoryMiB"`
		DiskDefaults *struct {
			SizeGiB int32 `json:"SizeGiB"`
		} `json:"DiskDefaults"`
	} `json:"Class"`
	Disks []struct {
		Name    string `json:"Name"`
		SizeGiB int32  `json:"SizeGiB"`
	} `json:"Disks"`
}

// reconfigureProperties are the VM properties a Reconfigure reads.
var reconfigureProperties = []string{
	"config.hardware.numCPU",
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
	task, err := vm.Reconfigure(ctx, *spec)
	if err != nil {
		return nil, fmt.Errorf("failed to start reconfiguration: %w", err)
	}
	if _, err := task.WaitForResult(ctx, nil); err != nil {
		return nil, fmt.Errorf("reconfiguration failed: %w", err)
	}
	p.logger.Info("VM reconfigured successfully", "vm_id", req.Id)

	// Completed synchronously. RestartRequired is false: vSphere has no
	// pending state — ReconfigVM_Task applied the change, or it was refused
	// above (a running VM that cannot take it) or failed.
	return &providerv1.TaskResponse{RestartRequired: false}, nil
}

// planReconfigure builds the config spec that takes the VM vmMo (read with
// reconfigureProperties) to desired, or nil when nothing changes. It refuses,
// before anything is changed, a spec.disks size below its disk's
// (InvalidArgument) and a CPU/memory change the running VM cannot take
// (FailedPrecondition, RECONFIGURE_NEEDS_POWER_OFF).
func (p *Provider) planReconfigure(vmID string, vmMo *mo.VirtualMachine, desired *reconfigureDesired) (*types.VirtualMachineConfigSpec, error) {
	cfg := vmMo.Config
	spec := &types.VirtualMachineConfigSpec{}
	changed := false

	cpuFrom, cpuTo := cfg.Hardware.NumCPU, cfg.Hardware.NumCPU
	if desired.Class.CPU > 0 && desired.Class.CPU != cfg.Hardware.NumCPU {
		cpuTo = desired.Class.CPU
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

	edits, err := p.planDiskGrowth(vmID, cfg, desired)
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

	if vmMo.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
		if reasons := onlineResizeBlockers(cfg, cpuFrom, cpuTo, memFrom, memTo); len(reasons) > 0 {
			p.logger.Info("Refusing a CPU/memory change the running VM cannot take; it is applied once the VM is powered off",
				"vm_id", vmID, "power_state", vmMo.Runtime.PowerState, "reasons", reasons)
			return nil, statusWithReasons(codes.FailedPrecondition,
				fmt.Sprintf(reconfigureNeedsPowerOffFormat, strings.Join(reasons, "; ")),
				contracts.ReconfigureNeedsPowerOffReason).Err()
		}
	}
	return spec, nil
}

// onlineResizeBlockers lists why a VM that is not powered off cannot take a
// change of its CPU count from cpuFrom to cpuTo and its memory from memFrom to
// memTo MiB, or nothing when it can (cfg: its config).
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

// matchSpecDisks maps each entry of names (the spec.disks names, in order) to
// the record of the disk Create attached for it, or nil (see the package
// comment above): by recorded name — the n-th entry of a name to the n-th
// record with it — else, for a record without a name, by position.
func matchSpecDisks(names []string, records []additionalDiskRecord) []*additionalDiskRecord {
	byName := map[string][]*additionalDiskRecord{}
	byIndex := map[int]*additionalDiskRecord{}
	for i := range records {
		rec := &records[i]
		byIndex[rec.index] = rec
		if rec.name != "" {
			byName[rec.name] = append(byName[rec.name], rec)
		}
	}
	seen := map[string]int{}
	out := make([]*additionalDiskRecord, len(names))
	for i, name := range names {
		n := seen[name]
		seen[name] = n + 1
		if named := byName[name]; n < len(named) {
			out[i] = named[n]
			continue
		}
		if rec := byIndex[i+1]; rec != nil && rec.name == "" {
			out[i] = rec
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

// grownDisk returns the edit that grows disk to sizeGiB.
func grownDisk(disk *types.VirtualDisk, sizeGiB int64) types.BaseVirtualDeviceConfigSpec {
	grown := *disk
	grown.CapacityInKB = sizeGiB * kibPerGiB
	grown.CapacityInBytes = sizeGiB * kibPerGiB * 1024
	return &types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationEdit, Device: &grown}
}

// planDiskGrowth returns the disk edits desired asks for on the VM whose
// config is cfg: the primary disk grown to the VMClass size (a floor), and
// each spec.disks entry's own disk grown to its size. A spec.disks size below
// its disk's is refused (InvalidArgument) before anything is changed.
func (p *Provider) planDiskGrowth(vmID string, cfg *types.VirtualMachineConfigInfo, desired *reconfigureDesired) ([]types.BaseVirtualDeviceConfigSpec, error) {
	var disks []*types.VirtualDisk
	for _, dev := range cfg.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			disks = append(disks, d)
		}
	}
	records := additionalDiskRecords(cfg.ExtraConfig)
	additional := map[int32]bool{} // device keys of recorded additional disks
	for i := range records {
		if d := findRecordedDisk(disks, &records[i]); d != nil {
			additional[d.Key] = true
		}
	}

	var edits []types.BaseVirtualDeviceConfigSpec
	// The primary disk: the first disk that is not a recorded additional disk.
	if dd := desired.Class.DiskDefaults; dd != nil && dd.SizeGiB > 0 {
		var primary *types.VirtualDisk
		for _, d := range disks {
			if !additional[d.Key] {
				primary = d
				break
			}
		}
		switch {
		case primary == nil:
			p.logger.Info("No primary disk found; the VMClass disk size is not applied", "vm_id", vmID)
		case int64(dd.SizeGiB)*kibPerGiB > primary.CapacityInKB:
			p.logger.Info("Primary disk grow requested", "vm_id", vmID,
				"old_gb", primary.CapacityInKB/kibPerGiB, "new_gb", dd.SizeGiB)
			edits = append(edits, grownDisk(primary, int64(dd.SizeGiB)))
		}
	}

	names := make([]string, len(desired.Disks))
	for i, d := range desired.Disks {
		names[i] = d.Name
	}
	for i, rec := range matchSpecDisks(names, records) {
		want := int64(desired.Disks[i].SizeGiB)
		if want <= 0 {
			continue // withheld (above the disk-size limit) or unset: leave it
		}
		var disk *types.VirtualDisk
		if rec != nil {
			disk = findRecordedDisk(disks, rec)
		}
		if disk == nil {
			p.logger.Info("No disk on the VM was attached for this spec.disks entry; Reconfigure does not add disks, leaving it",
				"vm_id", vmID, "disk_name", names[i], "position", i+1)
			continue
		}
		currentGiB := disk.CapacityInKB / kibPerGiB
		if want < currentGiB {
			return nil, sdkerrors.NewInvalidSpec(diskShrinkRefusedFormat, names[i], want, currentGiB, currentGiB)
		}
		if want*kibPerGiB > disk.CapacityInKB {
			p.logger.Info("Additional disk grow requested", "vm_id", vmID, "disk_name", names[i],
				"disk_index", rec.index, "old_gb", currentGiB, "new_gb", want)
			edits = append(edits, grownDisk(disk, want))
		}
	}
	return edits, nil
}
