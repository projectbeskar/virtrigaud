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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Create leaves every new VM powered off: the VirtualMachine controller applies
// spec.powerState afterwards through the Power RPC (empty means On), as it does
// for libvirt and Proxmox. These tests pin that for each vSphere create path.

// guestinfoUserData is the ExtraConfig key the cloud-init user-data is
// delivered under (addCloudInitToConfigSpec).
const guestinfoUserData = "guestinfo.userdata"

// runtimePowerState reads VM id's runtime power state straight from vCenter.
func runtimePowerState(t *testing.T, p *Provider, id string) types.VirtualMachinePowerState {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"runtime.powerState"}, &vm))
	return vm.Runtime.PowerState
}

// makeImportedDisk creates a small thin VMDK at diskPath ("[<ds>] dir/file.vmdk")
// on the simulated datastore, standing in for a disk ImportDisk produced.
func makeImportedDisk(t *testing.T, p *Provider, dir, diskPath string) {
	t.Helper()
	ctx := context.Background()
	dc, err := p.finder.DefaultDatacenter(ctx)
	require.NoError(t, err)
	require.NoError(t, object.NewFileManager(p.client.Client).MakeDirectory(ctx, dir, dc, true))
	task, err := object.NewVirtualDiskManager(p.client.Client).CreateVirtualDisk(ctx, diskPath, dc, &types.FileBackedVirtualDiskSpec{
		VirtualDiskSpec: types.VirtualDiskSpec{DiskType: string(types.VirtualDiskTypeThin), AdapterType: string(types.VirtualDiskAdapterTypeLsiLogic)},
		CapacityKb:      1024,
	})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
}

// requireCreatedOffThenPowersOn asserts the VM Create returned is powered off
// (in vCenter and as Describe reports it), then drives the controller's next
// step — Power(ON) — and asserts the VM runs.
func requireCreatedOffThenPowersOn(t *testing.T, p *Provider, id string) {
	t.Helper()
	ctx := context.Background()

	assert.Equal(t, types.VirtualMachinePowerStatePoweredOff, runtimePowerState(t, p, id),
		"Create must leave the VM powered off; power is the VM controller's job")
	desc, err := p.Describe(ctx, &providerv1.DescribeRequest{Id: id})
	require.NoError(t, err)
	require.True(t, desc.Exists)
	assert.Equal(t, "Off", desc.PowerState,
		"Describe must report Off so the controller powers the VM on only when spec.powerState asks for it")

	_, err = p.Power(ctx, &providerv1.PowerRequest{Id: id, Op: providerv1.PowerOp_POWER_OP_ON})
	require.NoError(t, err, "a later Power(ON) must start the VM Create left off")
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOn, runtimePowerState(t, p, id))
	desc, err = p.Describe(ctx, &providerv1.DescribeRequest{Id: id})
	require.NoError(t, err)
	assert.Equal(t, "On", desc.PowerState)
}

// TestCreate_TemplateCloneLeavesVMPoweredOff: the template-clone path
// (CloneVM_Task) never powers the new VM on, and the cloud-init guestinfo is
// already on the powered-off VM, so the guest reads it on its first boot
// whenever the controller performs it.
func TestCreate_TemplateCloneLeavesVMPoweredOff(t *testing.T) {
	p, _ := newOwnershipSim(t)

	req := templateCreateRequest("web", "", wireOwner(ownerTeamA))
	req.UserData = []byte("#cloud-config\nhostname: web\n")
	resp, err := p.Create(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Id)

	_, _, _, extraConfig := vmState(t, p, resp.Id)
	assert.True(t, hasExtraConfigKey(extraConfig, guestinfoUserData),
		"cloud-init user-data must be set at create time, before the first boot")

	requireCreatedOffThenPowersOn(t, p, resp.Id)
}

// TestCreate_ImportedDiskLeavesVMPoweredOff: the imported-disk path
// (CreateVM_Task, the VMMigration target create) never powers the new VM on.
// This is the migration case: a target with powerOn: false must never boot
// beside a still-running source carrying the same IP and hostname.
func TestCreate_ImportedDiskLeavesVMPoweredOff(t *testing.T) {
	p, _ := newOwnershipSim(t)
	imageJSON := makeOwnMigratedDisk(t, p, "migrated")

	resp, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name:      "migrated",
		ImageJson: imageJSON,
		Owner:     wireOwner(ownerTeamA),
		UserData:  []byte("#cloud-config\nhostname: migrated\n"),
	})
	require.NoError(t, err)

	_, _, _, extraConfig := vmState(t, p, resp.Id)
	assert.True(t, hasExtraConfigKey(extraConfig, guestinfoUserData),
		"cloud-init user-data must be set at create time, before the first boot")

	requireCreatedOffThenPowersOn(t, p, resp.Id)
}

// TestCreate_ImportedDiskAppliesPostCreateSteps: the steps after
// CreateVM_Task address the NEW VM. The imported-disk path once shadowed the
// VM reference, so the VMClass disk growth (and the additional-disk attach)
// ran against an empty reference and were silently skipped.
func TestCreate_ImportedDiskAppliesPostCreateSteps(t *testing.T) {
	p, _ := newOwnershipSim(t)
	imageJSON := makeOwnMigratedDisk(t, p, "migrated")

	resp, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name:      "migrated",
		ClassJson: `{"CPU":1,"MemoryMiB":512,"DiskDefaults":{"SizeGiB":2}}`,
		ImageJson: imageJSON,
		Owner:     wireOwner(ownerTeamA),
	})
	require.NoError(t, err)

	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(resp.Id),
		[]string{"config.hardware.device"}, &vm))
	var capacityKB []int64
	for _, dev := range vm.Config.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			capacityKB = append(capacityKB, d.CapacityInKB)
		}
	}
	require.Len(t, capacityKB, 1, "the imported disk is the VM's only disk")
	assert.Equal(t, int64(2*1024*1024), capacityKB[0], "the imported disk must be grown to the VMClass disk size")
}

// TestCreate_BindingRetryKeepsPowerState: a retried create that binds the
// requester's own existing VM changes nothing about its power state: a VM the
// controller left off stays off, one it powered on stays on.
func TestCreate_BindingRetryKeepsPowerState(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	first, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)

	again, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	require.Equal(t, first.Id, again.Id)
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOff, runtimePowerState(t, p, first.Id),
		"binding must not power the VM on")

	_, err = p.Power(ctx, &providerv1.PowerRequest{Id: first.Id, Op: providerv1.PowerOp_POWER_OP_ON})
	require.NoError(t, err)
	again, err = p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	require.Equal(t, first.Id, again.Id)
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOn, runtimePowerState(t, p, first.Id),
		"binding must not power the VM off")
}

// TestClone_LeavesClonePoweredOff: the Clone RPC (VMClone) leaves the clone
// powered off even when its source runs; the VMClone's powerOn decides,
// through the produced VirtualMachine's spec.powerState.
func TestClone_LeavesClonePoweredOff(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	src, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	_, err = p.Power(ctx, &providerv1.PowerRequest{Id: src.Id, Op: providerv1.PowerOp_POWER_OP_ON})
	require.NoError(t, err)

	resp, err := p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: src.Id, TargetName: "web-copy"})
	require.NoError(t, err)
	require.NotEmpty(t, resp.TargetVmId)

	assert.Equal(t, types.VirtualMachinePowerStatePoweredOff, runtimePowerState(t, p, resp.TargetVmId),
		"Clone must leave the clone powered off")
	assert.Equal(t, types.VirtualMachinePowerStatePoweredOn, runtimePowerState(t, p, src.Id),
		"Clone must not change the source's power state")
}

// hasExtraConfigKey reports whether extraConfig carries a non-empty key.
func hasExtraConfigKey(extraConfig []types.BaseOptionValue, key string) bool {
	for _, bov := range extraConfig {
		if o := bov.GetOptionValue(); o != nil && o.Key == key {
			s, ok := o.Value.(string)
			return ok && s != ""
		}
	}
	return false
}

// TestCreate_AdditionalDiskLandsInVMHome: an additional disk is created in
// the new VM's own home directory (config.files.vmPathName), not in
// "[<placement datastore>] <vm name>/". A VM created from an imported disk has
// its home on the imported disk's datastore, which here is not the provider's
// default datastore.
func TestCreate_AdditionalDiskLandsInVMHome(t *testing.T) {
	p, _ := newOwnershipSim(t, func(m *simulator.Model) { m.Datastore = 2 })
	const otherDS = "LocalDS_1"
	require.NotEqual(t, otherDS, p.config.DefaultDatastore)
	diskPath := "[" + otherDS + "] " + importedDiskRelPath("migrated")
	makeImportedDisk(t, p, "["+otherDS+"] "+importedDiskName("migrated"), diskPath)

	resp, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name:      "migrated",
		ImageJson: importedDiskImageJSON(diskPath, true),
		DisksJson: `[{"Name":"data","SizeGiB":1,"Type":"thin"}]`,
		Owner:     wireOwner(ownerTeamA),
	})
	require.NoError(t, err)

	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(resp.Id),
		[]string{"config.files.vmPathName"}, &vm))
	home, ok := datastoreFileOf(vm.Config.Files.VmPathName)
	require.True(t, ok)
	require.Equal(t, strings.ToLower(otherDS), home.datastore, "setup: the VM's home is on the imported disk's datastore")

	files := vmDiskFiles(t, p, resp.Id)
	require.Len(t, files, 2, "the imported disk and the additional disk")
	assert.Equal(t, diskPath, files[0])
	added, ok := datastoreFileOf(files[1])
	require.True(t, ok)
	assert.True(t, added.inDir(home.dir()), "additional disk %s must be in the VM's home directory %s", files[1], vm.Config.Files.VmPathName)
	assert.True(t, datastoreFileExists(t, p, files[1]))
}
