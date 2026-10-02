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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// In-place disk confinement (disk_guard.go): Create attaches a datastore file
// in place only when it is the VM's own migration landing disk, verified by
// the operator (ImportedDisk) and used by no other VM; Delete never destroys a
// disk the VM does not own; a migration import never deletes or overwrites a
// disk another VM uses.

// ownMigratedDiskPath is the landing path ImportDisk gives the disk it imports
// for the VM named vmName on the simulator's default datastore.
func ownMigratedDiskPath(vmName string) string {
	return "[" + simDatastore + "] " + importedDiskRelPath(vmName)
}

// importedDiskImageJSON is the ImageJson the manager sends for a VM created
// from the imported disk at diskPath; imported is contracts.VMImage.ImportedDisk.
func importedDiskImageJSON(diskPath string, imported bool) string {
	return fmt.Sprintf(`{"Path":%q,"Format":"vmdk","ImportedDisk":%t}`, diskPath, imported)
}

// makeOwnMigratedDisk lands vmName's migration disk at ownMigratedDiskPath and
// returns the ImageJson of a Create that attaches it (ImportedDisk set).
func makeOwnMigratedDisk(t *testing.T, p *Provider, vmName string) string {
	t.Helper()
	diskPath := ownMigratedDiskPath(vmName)
	makeImportedDisk(t, p, "["+simDatastore+"] "+importedDiskName(vmName), diskPath)
	return importedDiskImageJSON(diskPath, true)
}

// datastoreFileExists reports whether datastore path name exists.
func datastoreFileExists(t *testing.T, p *Provider, name string) bool {
	t.Helper()
	ctx := context.Background()
	var dp object.DatastorePath
	require.True(t, dp.FromString(name))
	ds, err := p.finder.Datastore(ctx, dp.Datastore)
	require.NoError(t, err)
	_, err = ds.Stat(ctx, dp.Path)
	if err != nil {
		var noFile object.DatastoreNoSuchFileError
		var noDir object.DatastoreNoSuchDirectoryError
		require.True(t, errors.As(err, &noFile) || errors.As(err, &noDir), "stat %s: %v", name, err)
		return false
	}
	return true
}

// vmDiskFiles returns the backing file of every disk of VM id.
func vmDiskFiles(t *testing.T, p *Provider, id string) []string {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"config.hardware.device"}, &vm))
	var files []string
	for _, dev := range vm.Config.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			if fb, ok := d.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
				files = append(files, fb.GetVirtualDeviceFileBackingInfo().FileName)
			}
		}
	}
	return files
}

// vmExists reports whether a VM with MOID id is in the inventory.
func vmExists(t *testing.T, p *Provider, id string) bool {
	t.Helper()
	var vm mo.VirtualMachine
	err := property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id), []string{"name"}, &vm)
	return err == nil
}

// attachDiskInPlace adds the existing file diskPath to VM id as a disk on a
// new SCSI controller: the state a VM created before the in-place confinement
// (or edited in vCenter) can be in.
func attachDiskInPlace(t *testing.T, p *Provider, id, diskPath string) {
	t.Helper()
	ctx := context.Background()
	vm := object.NewVirtualMachine(p.client.Client, vmRef(id))
	devices, err := vm.Device(ctx)
	require.NoError(t, err)
	ctrl, err := devices.CreateSCSIController("lsilogic")
	require.NoError(t, err)
	scsi, ok := ctrl.(types.BaseVirtualSCSIController)
	require.True(t, ok)
	scsi.GetVirtualSCSIController().BusNumber = 3
	require.NoError(t, vm.AddDevice(ctx, ctrl))
	devices, err = vm.Device(ctx)
	require.NoError(t, err)
	var key int32
	for _, d := range devices {
		if c, ok := d.(types.BaseVirtualSCSIController); ok && c.GetVirtualSCSIController().BusNumber == 3 {
			key = c.GetVirtualSCSIController().Key
		}
	}
	require.NotZero(t, key)
	unit := int32(0)
	require.NoError(t, vm.AddDevice(ctx, &types.VirtualDisk{VirtualDevice: types.VirtualDevice{
		Key: -1, ControllerKey: key, UnitNumber: &unit,
		Backing: &types.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: diskPath},
			DiskMode:                     string(types.VirtualDiskModePersistent),
		},
	}}))
	assert.Contains(t, vmDiskFiles(t, p, id), diskPath, "setup: the disk is attached in place")
}

// vcenterDestroySemantics makes Destroy_Task of a VM delete every disk file
// attached to it, wherever the file is, as vCenter does. vcsim only deletes the
// VM's own directory, which would hide a disk delete of another VM's file.
type vcenterDestroySemantics struct {
	soap.RoundTripper
	c *vim25.Client
}

func (r *vcenterDestroySemantics) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	body, ok := req.(*methods.Destroy_TaskBody)
	if !ok || body.Req == nil || body.Req.This.Type != virtualMachineMoType {
		return r.RoundTripper.RoundTrip(ctx, req, res)
	}
	var vm mo.VirtualMachine
	if err := property.DefaultCollector(r.c).RetrieveOne(ctx, body.Req.This, []string{"config.hardware.device"}, &vm); err != nil {
		return r.RoundTripper.RoundTrip(ctx, req, res)
	}
	var disks []string
	for _, dev := range vm.Config.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			if fb, ok := d.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
				disks = append(disks, fb.GetVirtualDeviceFileBackingInfo().FileName)
			}
		}
	}
	if err := r.RoundTripper.RoundTrip(ctx, req, res); err != nil {
		return err
	}
	destroyRes, ok := res.(*methods.Destroy_TaskBody)
	if !ok || destroyRes.Res == nil {
		return nil
	}
	if _, err := object.NewTask(r.c, destroyRes.Res.Returnval).WaitForResult(ctx, nil); err != nil {
		return nil // the caller's own wait reports the failure
	}
	dc, err := find.NewFinder(r.c).DefaultDatacenter(ctx)
	if err != nil {
		return fmt.Errorf("fixture: datacenter: %w", err)
	}
	vdm := object.NewVirtualDiskManager(r.c)
	for _, name := range disks {
		if task, err := vdm.DeleteVirtualDisk(ctx, name, dc); err == nil {
			_ = task.Wait(ctx)
		}
	}
	return nil
}

// withVCenterDestroySemantics installs vcenterDestroySemantics on p.
func withVCenterDestroySemantics(p *Provider) {
	p.client.RoundTripper = &vcenterDestroySemantics{RoundTripper: p.client.RoundTripper, c: p.client.Client}
}

// failVMInventoryView makes the inventory scan's container view (VMs, from
// the root folder) fail, as a vCenter error would.
type failVMInventoryView struct {
	soap.RoundTripper
	root types.ManagedObjectReference
}

func (r *failVMInventoryView) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.CreateContainerViewBody); ok && body.Req != nil && body.Req.Container == r.root {
		for _, kind := range body.Req.Type {
			if kind == virtualMachineMoType {
				return fmt.Errorf("fixture: simulated vCenter failure naming %q", "[secret-ds] other-tenant/disk.vmdk")
			}
		}
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// --- pure helpers ------------------------------------------------------------

func TestCheckInPlaceDiskPath(t *testing.T) {
	const vm = "web"
	ok := "[ds1] web-migrated/web-migrated.vmdk"
	ds, err := checkInPlaceDiskPath(ok, vm, true)
	require.NoError(t, err)
	assert.Equal(t, "ds1", ds)
	_, err = checkInPlaceDiskPath("[my datastore] web-migrated/web-migrated.vmdk", vm, true)
	require.NoError(t, err, "datastore names may contain spaces")

	for name, tc := range map[string]struct {
		path     string
		vm       string
		imported bool
	}{
		"not verified by the operator": {ok, vm, false},
		"another VM's migrated disk":   {"[ds1] db-migrated/db-migrated.vmdk", vm, true},
		"another VM's own disk":        {"[ds1] web/web.vmdk", vm, true},
		"name prefix only":             {"[ds1] web-migrated/web-migrated.vmdk", "we", true},
		"other folder":                 {"[ds1] x/web-migrated.vmdk", vm, true},
		"nested":                       {"[ds1] a/web-migrated/web-migrated.vmdk", vm, true},
		"dot-dot":                      {"[ds1] web-migrated/../victim/victim.vmdk", vm, true},
		"no space":                     {"[ds1]web-migrated/web-migrated.vmdk", vm, true},
		"two spaces":                   {"[ds1]  web-migrated/web-migrated.vmdk", vm, true},
		"trailing text":                {ok + " ", vm, true},
		"case variant":                 {"[ds1] WEB-migrated/WEB-migrated.vmdk", vm, true},
		"flat extent":                  {"[ds1] web-migrated/web-migrated-flat.vmdk", vm, true},
		"empty datastore":              {"[] web-migrated/web-migrated.vmdk", vm, true},
		"padded datastore":             {"[ ds1] web-migrated/web-migrated.vmdk", vm, true},
		"bracket in datastore":         {"[d[s] web-migrated/web-migrated.vmdk", vm, true},
		"host path":                    {"/vmfs/volumes/ds1/web-migrated/web-migrated.vmdk", vm, true},
		"control character":            {"[ds\x001] web-migrated/web-migrated.vmdk", vm, true},
		"empty":                        {"", vm, true},
		"no VM name":                   {ok, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := checkInPlaceDiskPath(tc.path, tc.vm, tc.imported)
			requireCode(t, err, codes.InvalidArgument)
			assert.Equal(t, inPlaceDiskRejectedMessage, status.Convert(err).Message(),
				"the refusal is a constant message: never the caller's path")
		})
	}
}

func TestDatastoreFileOf(t *testing.T) {
	f, ok := datastoreFileOf("[DS1] Dir//a/../Disk.vmdk")
	require.True(t, ok)
	assert.Equal(t, datastoreFile{datastore: "ds1", path: "dir/disk.vmdk"}, f)
	assert.Equal(t, datastoreFile{datastore: "ds1", path: "dir"}, f.dir())
	assert.True(t, f.inDir(datastoreFile{datastore: "ds1", path: "dir"}))
	assert.False(t, f.inDir(datastoreFile{datastore: "ds1", path: "di"}))
	assert.False(t, f.inDir(datastoreFile{datastore: "ds2", path: "dir"}))
	for _, bad := range []string{"", "/vmfs/volumes/x/y.vmdk", "[] a.vmdk", "[ds"} {
		_, ok := datastoreFileOf(bad)
		assert.False(t, ok, bad)
	}
}

// --- Create ------------------------------------------------------------------

// TestCreate_InPlaceDisk_RefusesForeignPath: a VirtualMachine naming another
// VM's disk in spec.importedDisk.path is refused before anything is created,
// even when the request claims ImportedDisk.
func TestCreate_InPlaceDisk_RefusesForeignPath(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	victim, err := p.Create(ctx, templateCreateRequest("victim", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	victimDisks := vmDiskFiles(t, p, victim.Id)
	require.NotEmpty(t, victimDisks)

	for name, imported := range map[string]bool{"claimed own": true, "not verified": false} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Create(ctx, &providerv1.CreateRequest{
				Name:      "attacker",
				ClassJson: `{"CPU":1,"MemoryMiB":512,"DiskDefaults":{"SizeGiB":2}}`,
				ImageJson: importedDiskImageJSON(victimDisks[0], imported),
				Owner:     wireOwner(ownerTeamB),
			})
			requireCode(t, err, codes.InvalidArgument)
			assert.NotContains(t, err.Error(), victimDisks[0], "the refusal never echoes the path")
			refs, lerr := p.vmsNamedInFolder(ctx, defaultVMFolder(t, p), "attacker")
			require.NoError(t, lerr)
			assert.Empty(t, refs, "nothing is created")
		})
	}
}

// TestCreate_InPlaceDisk_RefusesUnverifiedOwnPath: the VM's own landing-disk
// path without ImportedDisk (the operator did not verify it as this VM's
// migration disk) is refused too.
func TestCreate_InPlaceDisk_RefusesUnverifiedOwnPath(t *testing.T) {
	p, _ := newOwnershipSim(t)
	makeOwnMigratedDisk(t, p, "migrated")

	_, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name:      "migrated",
		ImageJson: importedDiskImageJSON(ownMigratedDiskPath("migrated"), false),
		Owner:     wireOwner(ownerTeamA),
	})
	requireCode(t, err, codes.InvalidArgument)
}

// TestCreate_InPlaceDisk_AcceptsOwnMigratedDisk: the VM's own landing disk,
// verified and unused, is attached in place.
func TestCreate_InPlaceDisk_AcceptsOwnMigratedDisk(t *testing.T) {
	p, _ := newOwnershipSim(t)
	resp, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name:      "migrated",
		ImageJson: makeOwnMigratedDisk(t, p, "migrated"),
		Owner:     wireOwner(ownerTeamA),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{ownMigratedDiskPath("migrated")}, vmDiskFiles(t, p, resp.Id))
}

// TestCreate_InPlaceDisk_IgnoresOtherProvidersPath: a template-based image
// that also carries a path (another provider's source) clones the template.
func TestCreate_InPlaceDisk_IgnoresOtherProvidersPath(t *testing.T) {
	p, _ := newOwnershipSim(t)
	resp, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name:      "web",
		ImageJson: `{"TemplateName":"` + simTemplate + `","Path":"/var/lib/libvirt/images/base.qcow2","Format":"qcow2"}`,
		Owner:     wireOwner(ownerTeamA),
	})
	require.NoError(t, err)
	for _, f := range vmDiskFiles(t, p, resp.Id) {
		assert.NotContains(t, f, "libvirt")
	}
}

// TestCreate_InPlaceDisk_RefusesDiskUsedByAnotherVM: a landing disk some other
// VM or template references is never attached, whoever claims it.
func TestCreate_InPlaceDisk_RefusesDiskUsedByAnotherVM(t *testing.T) {
	t.Run("same-named VM in another folder (another namespace)", func(t *testing.T) {
		p, _ := newOwnershipSim(t)
		ctx := context.Background()
		imageJSON := makeOwnMigratedDisk(t, p, "web")
		first, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA)})
		require.NoError(t, err)

		_, err = defaultVMFolder(t, p).CreateFolder(ctx, simOtherDir)
		require.NoError(t, err)
		_, err = p.Create(ctx, &providerv1.CreateRequest{
			Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamB),
			PlacementJson: fmt.Sprintf(`{"Folder":%q}`, simOtherDir),
		})
		requireCode(t, err, codes.FailedPrecondition)
		assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))
		assert.Equal(t, diskInUseMessage, status.Convert(err).Message(), "never names the other VM")
		assert.Equal(t, []string{ownMigratedDiskPath("web")}, vmDiskFiles(t, p, first.Id), "the first VM keeps its disk")
	})

	t.Run("template", func(t *testing.T) {
		p, _ := newOwnershipSim(t)
		imageJSON := makeOwnMigratedDisk(t, p, "web")
		attachDiskInPlace(t, p, seededVMID(t, p, simUnstamped), ownMigratedDiskPath("web"))
		markAsTemplate(t, p, seededVMID(t, p, simUnstamped))

		_, err := p.Create(context.Background(), &providerv1.CreateRequest{Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA)})
		requireCode(t, err, codes.FailedPrecondition)
	})
}

// TestCreate_InPlaceDisk_ScanFailureFailsClosed: when the inventory cannot be
// scanned, nothing is attached; the error is retryable, kept out of the
// circuit breaker, and names nothing from vCenter.
func TestCreate_InPlaceDisk_ScanFailureFailsClosed(t *testing.T) {
	p, _ := newOwnershipSim(t)
	imageJSON := makeOwnMigratedDisk(t, p, "migrated")
	p.client.RoundTripper = &failVMInventoryView{RoundTripper: p.client.RoundTripper, root: p.client.ServiceContent.RootFolder}

	_, err := p.Create(context.Background(), &providerv1.CreateRequest{Name: "migrated", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA)})
	requireCode(t, err, codes.Unavailable)
	assert.Equal(t, []string{contracts.VMDiskCheckFailedReason}, errorReasons(err))
	assert.NotContains(t, err.Error(), "secret-ds")
	refs, lerr := p.vmsNamedInFolder(context.Background(), defaultVMFolder(t, p), "migrated")
	require.NoError(t, lerr)
	assert.Empty(t, refs, "nothing is created")
}

// --- Delete ------------------------------------------------------------------

// TestDelete_KeepsForeignDisk: a VM that has another VM's disk attached in
// place (created before the confinement) is deleted without deleting that
// disk; its own disks are deleted.
func TestDelete_KeepsForeignDisk(t *testing.T) {
	p, _ := newOwnershipSim(t)
	withVCenterDestroySemantics(p)
	ctx := context.Background()
	victim, err := p.Create(ctx, templateCreateRequest("victim", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	victimDisk := vmDiskFiles(t, p, victim.Id)[0]
	attacker, err := p.Create(ctx, templateCreateRequest("attacker", "", wireOwner(ownerTeamB)))
	require.NoError(t, err)
	ownDisk := vmDiskFiles(t, p, attacker.Id)[0]
	attachDiskInPlace(t, p, attacker.Id, victimDisk)

	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: attacker.Id})
	require.NoError(t, err)
	assert.False(t, vmExists(t, p, attacker.Id))
	assert.True(t, datastoreFileExists(t, p, victimDisk), "another VM's disk must survive the delete")
	assert.False(t, datastoreFileExists(t, p, ownDisk), "the VM's own disk is deleted")
	assert.Equal(t, []string{victimDisk}, vmDiskFiles(t, p, victim.Id))
}

// TestDelete_DeletesOwnMigratedDisk: a migrated VM's own landing disk is its
// disk and is deleted with it.
func TestDelete_DeletesOwnMigratedDisk(t *testing.T) {
	p, _ := newOwnershipSim(t)
	withVCenterDestroySemantics(p)
	ctx := context.Background()
	resp, err := p.Create(ctx, &providerv1.CreateRequest{Name: "migrated", ImageJson: makeOwnMigratedDisk(t, p, "migrated"), Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err)

	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: resp.Id})
	require.NoError(t, err)
	assert.False(t, vmExists(t, p, resp.Id))
	assert.False(t, datastoreFileExists(t, p, ownMigratedDiskPath("migrated")))
}

// TestDelete_KeepsOwnNamedDiskAnotherVMUses: a VM whose in-place disk carries
// its own landing-disk name but is another VM's disk (a same-named VM in
// another namespace, created before the confinement) keeps that disk.
func TestDelete_KeepsOwnNamedDiskAnotherVMUses(t *testing.T) {
	p, _ := newOwnershipSim(t)
	withVCenterDestroySemantics(p)
	ctx := context.Background()
	imageJSON := makeOwnMigratedDisk(t, p, "web")
	owner, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err)

	_, err = defaultVMFolder(t, p).CreateFolder(ctx, simOtherDir)
	require.NoError(t, err)
	other, err := p.Create(ctx, templateCreateRequest("web", simOtherDir, wireOwner(ownerTeamB)))
	require.NoError(t, err)
	attachDiskInPlace(t, p, other.Id, ownMigratedDiskPath("web"))

	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: other.Id})
	require.NoError(t, err)
	assert.False(t, vmExists(t, p, other.Id))
	assert.True(t, datastoreFileExists(t, p, ownMigratedDiskPath("web")))
	assert.Equal(t, []string{ownMigratedDiskPath("web")}, vmDiskFiles(t, p, owner.Id))
}

// TestDelete_ForeignDiskWithSnapshotsIsRefused: a foreign disk cannot be
// detached safely while snapshots reference it, so the delete is refused and
// the VM kept.
func TestDelete_ForeignDiskWithSnapshotsIsRefused(t *testing.T) {
	p, _ := newOwnershipSim(t)
	withVCenterDestroySemantics(p)
	ctx := context.Background()
	victim, err := p.Create(ctx, templateCreateRequest("victim", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	victimDisk := vmDiskFiles(t, p, victim.Id)[0]
	attacker, err := p.Create(ctx, templateCreateRequest("attacker", "", wireOwner(ownerTeamB)))
	require.NoError(t, err)
	attachDiskInPlace(t, p, attacker.Id, victimDisk)
	task, err := object.NewVirtualMachine(p.client.Client, vmRef(attacker.Id)).CreateSnapshot(ctx, "s1", "", false, false)
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))

	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: attacker.Id})
	requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))
	assert.True(t, vmExists(t, p, attacker.Id))
	assert.True(t, datastoreFileExists(t, p, victimDisk))
}

// TestDelete_TemplateCloneUnchanged: a VM with only its own disks is deleted
// with them, without an inventory scan.
func TestDelete_TemplateCloneUnchanged(t *testing.T) {
	p, _ := newOwnershipSim(t)
	withVCenterDestroySemantics(p)
	ctx := context.Background()
	resp, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	disks := vmDiskFiles(t, p, resp.Id)
	// The scan would fail: proving Delete does not need it here.
	p.client.RoundTripper = &failVMInventoryView{RoundTripper: p.client.RoundTripper, root: p.client.ServiceContent.RootFolder}

	_, err = p.Delete(ctx, &providerv1.DeleteRequest{Id: resp.Id})
	require.NoError(t, err)
	for _, d := range disks {
		assert.False(t, datastoreFileExists(t, p, d))
	}
}

// --- ImportDisk landing folder -----------------------------------------------

// TestEnsureImportFolderUnused: a migration import may delete or overwrite its
// landing folder only when no VM uses a file in it.
func TestEnsureImportFolderUnused(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	id := importedDiskName("web")

	require.NoError(t, p.ensureImportFolderUnused(ctx, simDatastore, id), "a free landing folder is fine")

	_, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: makeOwnMigratedDisk(t, p, "web"), Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err)
	err = p.ensureImportFolderUnused(ctx, simDatastore, id)
	requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))

	p.client.RoundTripper = &failVMInventoryView{RoundTripper: p.client.RoundTripper, root: p.client.ServiceContent.RootFolder}
	requireCode(t, p.ensureImportFolderUnused(ctx, simDatastore, importedDiskName("db")), codes.Unavailable)
}
