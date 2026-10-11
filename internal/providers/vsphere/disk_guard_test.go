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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
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
// for the VM named vmName in ownerTeamA's namespace, on the simulator's
// default datastore.
func ownMigratedDiskPath(vmName string) string {
	return landingPathIn(ownerTeamA.Namespace, vmName)
}

// landingPathIn is the landing path of namespace/vmName on the default
// datastore.
func landingPathIn(namespace, vmName string) string {
	return "[" + simDatastore + "] " + landingRelPath(namespace, vmName)
}

// ownerNamed is o's identity on the wire, for the VM named name (the owner of
// a create names the VM being created).
func ownerNamed(o contracts.ObjectIdentity, name string) *providerv1.ObjectIdentity {
	o.Name = name
	return wireOwner(o)
}

// importedDiskImageJSON is the ImageJson the manager sends for a VM created
// from the imported disk at diskPath; imported is contracts.VMImage.ImportedDisk.
func importedDiskImageJSON(diskPath string, imported bool) string {
	return fmt.Sprintf(`{"Path":%q,"Format":"vmdk","ImportedDisk":%t}`, diskPath, imported)
}

// makeOwnMigratedDisk lands vmName's migration disk (ownerTeamA's namespace)
// at ownMigratedDiskPath and returns the ImageJson of a Create that attaches
// it (ImportedDisk set).
func makeOwnMigratedDisk(t *testing.T, p *Provider, vmName string) string {
	t.Helper()
	return makeLandingDisk(t, p, ownerTeamA.Namespace, vmName)
}

// makeLandingDisk lands namespace/vmName's migration disk at landingPathIn and
// returns the ImageJson of a Create that attaches it (ImportedDisk set).
func makeLandingDisk(t *testing.T, p *Provider, namespace, vmName string) string {
	t.Helper()
	diskPath := landingPathIn(namespace, vmName)
	makeImportedDisk(t, p, "["+simDatastore+"] "+landingName(namespace, vmName), diskPath)
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

// failVMInventoryView makes the disk scan's datastore view (from the root
// folder) fail, as a vCenter error would.
type failVMInventoryView struct {
	soap.RoundTripper
	root types.ManagedObjectReference
}

func (r *failVMInventoryView) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.CreateContainerViewBody); ok && body.Req != nil && body.Req.Container == r.root {
		for _, kind := range body.Req.Type {
			if kind == datastoreMoType {
				return fmt.Errorf("fixture: simulated vCenter failure naming %q", "[secret-ds] other-tenant/disk.vmdk")
			}
		}
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// hideVMConfig makes vCenter return no configuration for VM hidden, as for an
// inaccessible or orphaned VM.
type hideVMConfig struct {
	soap.RoundTripper
	hidden types.ManagedObjectReference
}

func (r *hideVMConfig) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if err := r.RoundTripper.RoundTrip(ctx, req, res); err != nil {
		return err
	}
	body, ok := res.(*methods.RetrievePropertiesExBody)
	if !ok || body.Res == nil || body.Res.Returnval == nil {
		return nil
	}
	for i := range body.Res.Returnval.Objects {
		oc := &body.Res.Returnval.Objects[i]
		if oc.Obj != r.hidden {
			continue
		}
		var kept []types.DynamicProperty
		for _, prop := range oc.PropSet {
			if !strings.HasPrefix(prop.Name, "config") {
				kept = append(kept, prop)
			}
		}
		oc.PropSet = kept
	}
	return nil
}

// --- pure helpers ------------------------------------------------------------

func TestCheckInPlaceDiskPath(t *testing.T) {
	const vm = "web"
	owner := contracts.ObjectIdentity{UID: "u", Namespace: "team-a", Name: vm}
	ok := "[ds1] team-a.web-migrated/team-a.web-migrated.vmdk"
	ds, err := checkInPlaceDiskPath(ok, owner, vm, true)
	require.NoError(t, err)
	assert.Equal(t, "ds1", ds)
	_, err = checkInPlaceDiskPath("[my datastore] team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true)
	require.NoError(t, err, "datastore names may contain spaces")
	_, err = checkInPlaceDiskPath(ok, contracts.ObjectIdentity{Namespace: "team-a"}, vm, true)
	require.NoError(t, err, "an owner without a name is the VM being created")

	noNamespace := owner
	noNamespace.Namespace = ""
	otherName := owner
	otherName.Name = "db"
	for name, tc := range map[string]struct {
		path     string
		owner    contracts.ObjectIdentity
		vm       string
		imported bool
	}{
		"not verified by the operator": {ok, owner, vm, false},
		"owner without a namespace":    {ok, noNamespace, vm, true},
		"owner names another VM":       {ok, otherName, vm, true},
		"same name, another namespace": {"[ds1] team-b.web-migrated/team-b.web-migrated.vmdk", owner, vm, true},
		"another VM's landing disk":    {"[ds1] team-a.db-migrated/team-a.db-migrated.vmdk", owner, vm, true},
		"another VM's own disk":        {"[ds1] web/web.vmdk", owner, vm, true},
		"name prefix only":             {ok, contracts.ObjectIdentity{Namespace: "team-a"}, "we", true},
		"namespace prefix only":        {ok, contracts.ObjectIdentity{Namespace: "team"}, vm, true},
		"other folder":                 {"[ds1] x/team-a.web-migrated.vmdk", owner, vm, true},
		"nested":                       {"[ds1] a/team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"dot-dot":                      {"[ds1] team-a.web-migrated/../victim/victim.vmdk", owner, vm, true},
		"no space":                     {"[ds1]team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"two spaces":                   {"[ds1]  team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"trailing text":                {ok + " ", owner, vm, true},
		"case variant":                 {"[ds1] TEAM-A.web-migrated/TEAM-A.web-migrated.vmdk", owner, vm, true},
		"flat extent":                  {"[ds1] team-a.web-migrated/team-a.web-migrated-flat.vmdk", owner, vm, true},
		"empty datastore":              {"[] team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"padded datastore":             {"[ ds1] team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"bracket in datastore":         {"[d[s] team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"glob in datastore":            {"[ds*] team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"host path":                    {"/vmfs/volumes/ds1/team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"control character":            {"[ds\x001] team-a.web-migrated/team-a.web-migrated.vmdk", owner, vm, true},
		"empty":                        {"", owner, vm, true},
		"no VM name":                   {ok, owner, "", true},
		"namespace not a DNS label":    {"[ds1] a.b.web-migrated/a.b.web-migrated.vmdk", contracts.ObjectIdentity{Namespace: "a.b"}, vm, true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := checkInPlaceDiskPath(tc.path, tc.owner, tc.vm, tc.imported)
			requireCode(t, err, codes.InvalidArgument)
			assert.Equal(t, inPlaceDiskRejectedMessage, status.Convert(err).Message(),
				"the refusal is a constant message: never the caller's path")
		})
	}

	t.Run("pre-v0.4.0 bare landing name", func(t *testing.T) {
		_, err := checkInPlaceDiskPath("[ds1] web-migrated/web-migrated.vmdk", owner, vm, true)
		requireCode(t, err, codes.InvalidArgument)
		assert.Equal(t, legacyLandingMessage, status.Convert(err).Message())
	})
}

func TestImportLandingName(t *testing.T) {
	req := func(ns, name, targetName string) *providerv1.ImportDiskRequest {
		r := &providerv1.ImportDiskRequest{TargetName: targetName}
		if ns != "" || name != "" {
			r.TargetVm = &providerv1.ObjectIdentity{Namespace: ns, Name: name}
		}
		return r
	}
	id, err := importLandingName(req("team-a", "web", "web-migrated"))
	require.NoError(t, err)
	assert.Equal(t, "team-a.web-migrated", id)
	id, err = importLandingName(req("team-b", "web", ""))
	require.NoError(t, err)
	assert.Equal(t, "team-b.web-migrated", id, "the same VM name in another namespace lands elsewhere")

	for name, r := range map[string]*providerv1.ImportDiskRequest{
		"no target VM (older manager)":  req("", "", "web-migrated"),
		"no namespace":                  req("", "web", "web-migrated"),
		"target name of another VM":     req("team-a", "web", "db-migrated"),
		"target name aims at a landing": req("team-a", "web", "team-b.web-migrated"),
		"namespace with a dot":          req("team.a", "web", ""),
		"name with a slash":             req("team-a", "a/b", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := importLandingName(r)
			requireCode(t, err, codes.InvalidArgument)
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
		Owner:     ownerNamed(ownerTeamA, "migrated"),
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
		Owner:     ownerNamed(ownerTeamA, "migrated"),
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
	t.Run("a re-created VirtualMachine in another folder", func(t *testing.T) {
		p, _ := newOwnershipSim(t)
		ctx := context.Background()
		imageJSON := makeOwnMigratedDisk(t, p, "web")
		first, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA)})
		require.NoError(t, err)

		_, err = defaultVMFolder(t, p).CreateFolder(ctx, simOtherDir)
		require.NoError(t, err)
		_, err = p.Create(ctx, &providerv1.CreateRequest{
			Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA),
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

	_, err := p.Create(context.Background(), &providerv1.CreateRequest{Name: "migrated", ImageJson: imageJSON, Owner: ownerNamed(ownerTeamA, "migrated")})
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
	resp, err := p.Create(ctx, &providerv1.CreateRequest{Name: "migrated", ImageJson: makeOwnMigratedDisk(t, p, "migrated"), Owner: ownerNamed(ownerTeamA, "migrated")})
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
	id := landingName(ownerTeamA.Namespace, "web")

	require.NoError(t, p.ensureImportFolderUnused(ctx, simDatastore, id, true), "a free landing folder is fine")

	_, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: makeOwnMigratedDisk(t, p, "web"), Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err)
	err = p.ensureImportFolderUnused(ctx, simDatastore, id, true)
	requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))

	p.client.RoundTripper = &failVMInventoryView{RoundTripper: p.client.RoundTripper, root: p.client.ServiceContent.RootFolder}
	requireCode(t, p.ensureImportFolderUnused(ctx, simDatastore, landingName(ownerTeamA.Namespace, "db"), true), codes.Unavailable)
}

// --- namespaced landing names --------------------------------------------------

// TestCreate_InPlaceDisk_SameNameOtherNamespaceCannotAttach: VM names repeat
// across namespaces. team-b's "web" can neither attach team-a's landing disk
// nor reach it through its own landing name.
func TestCreate_InPlaceDisk_SameNameOtherNamespaceCannotAttach(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	teamA := makeLandingDisk(t, p, ownerTeamA.Namespace, "web")
	teamB := makeLandingDisk(t, p, ownerTeamB.Namespace, "web")
	require.NotEqual(t, teamA, teamB, "the two namespaces land in different folders")

	_, err := defaultVMFolder(t, p).CreateFolder(ctx, simOtherDir)
	require.NoError(t, err)
	_, err = p.Create(ctx, &providerv1.CreateRequest{
		Name: "web", ImageJson: teamA, Owner: wireOwner(ownerTeamB),
		PlacementJson: fmt.Sprintf(`{"Folder":%q}`, simOtherDir),
	})
	requireCode(t, err, codes.InvalidArgument)
	assert.Equal(t, inPlaceDiskRejectedMessage, status.Convert(err).Message())

	b, err := p.Create(ctx, &providerv1.CreateRequest{
		Name: "web", ImageJson: teamB, Owner: wireOwner(ownerTeamB),
		PlacementJson: fmt.Sprintf(`{"Folder":%q}`, simOtherDir),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{landingPathIn(ownerTeamB.Namespace, "web")}, vmDiskFiles(t, p, b.Id))

	a, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: teamA, Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err, "team-a's landing disk is untouched and still its own")
	assert.Equal(t, []string{landingPathIn(ownerTeamA.Namespace, "web")}, vmDiskFiles(t, p, a.Id))
}

// TestCreate_InPlaceDisk_RefusesWithoutOwnerNamespace: a create without the
// requesting VirtualMachine's namespace (an older manager) cannot prove the
// landing disk is its own.
func TestCreate_InPlaceDisk_RefusesWithoutOwnerNamespace(t *testing.T) {
	p, _ := newOwnershipSim(t)
	imageJSON := makeOwnMigratedDisk(t, p, "migrated")
	for name, owner := range map[string]*providerv1.ObjectIdentity{
		"no owner":            nil,
		"owner, no namespace": {Uid: ownerTeamA.UID, Name: "migrated"},
		"owner of another VM": ownerNamed(ownerTeamA, "other"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Create(context.Background(), &providerv1.CreateRequest{Name: "migrated", ImageJson: imageJSON, Owner: owner})
			requireCode(t, err, codes.InvalidArgument)
		})
	}
}

// TestCreate_InPlaceDisk_LegacyLandingNeedsANewMigration: a disk a release
// before v0.4.0 landed under the bare "<vm>-migrated" name (an in-flight
// migration at upgrade) is refused with a message that says to redo it.
func TestCreate_InPlaceDisk_LegacyLandingNeedsANewMigration(t *testing.T) {
	p, _ := newOwnershipSim(t)
	legacy := "[" + simDatastore + "] " + legacyLandingRelPath("migrated")
	makeImportedDisk(t, p, "["+simDatastore+"] migrated"+contracts.ImportedDiskNameSuffix, legacy)

	_, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name: "migrated", ImageJson: importedDiskImageJSON(legacy, true), Owner: ownerNamed(ownerTeamA, "migrated"),
	})
	requireCode(t, err, codes.InvalidArgument)
	assert.Equal(t, legacyLandingMessage, status.Convert(err).Message())
	refs, err := p.vmsNamedInFolder(context.Background(), defaultVMFolder(t, p), "migrated")
	require.NoError(t, err)
	assert.Empty(t, refs)
	assert.True(t, datastoreFileExists(t, p, legacy), "the old landing disk is left in place")
}

// TestCreate_InPlaceDisk_CheckRunsUnderTheLandingLock: the in-use check runs
// while the landing folder's lock is held, so work that finishes on the same
// landing disk while a create waits for the lock — here another VM attaching
// it — is seen, and the create refuses. (Two Creates cannot run concurrently
// in this test: the provider's shared govmomi Finder is not safe for that.)
func TestCreate_InPlaceDisk_CheckRunsUnderTheLandingLock(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	imageJSON := makeOwnMigratedDisk(t, p, "web")
	other, err := p.Create(ctx, templateCreateRequest("other", "", wireOwner(ownerTeamB)))
	require.NoError(t, err)

	key := landingLockKeyOfPath(ownMigratedDiskPath("web"))
	unlock, err := p.landingLocks.lock(ctx, key)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA)})
		done <- err
	}()
	require.Eventually(t, func() bool {
		p.landingLocks.mu.Lock()
		defer p.landingLocks.mu.Unlock()
		l := p.landingLocks.locks[key]
		return l != nil && l.refs == 2
	}, 10*time.Second, 5*time.Millisecond, "the create waits for the landing lock")

	attachDiskInPlace(t, p, other.Id, ownMigratedDiskPath("web"))
	unlock()

	err = <-done
	requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))
}

// TestCleanupFailedImport: a failed import's cleanup deletes its landing
// folder only when, checked again right before the delete, no VM uses it.
func TestCleanupFailedImport(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	_, err := p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: makeOwnMigratedDisk(t, p, "web"), Owner: wireOwner(ownerTeamA)})
	require.NoError(t, err)
	p.cleanupFailedImport(simDatastore, landingName(ownerTeamA.Namespace, "web"))
	assert.True(t, datastoreFileExists(t, p, ownMigratedDiskPath("web")), "a folder in use is never deleted")

	makeOwnMigratedDisk(t, p, "db")
	p.cleanupFailedImport(simDatastore, landingName(ownerTeamA.Namespace, "db"))
	assert.False(t, datastoreFileExists(t, p, ownMigratedDiskPath("db")), "an unused failed landing is cleaned up")
}

func TestKeyedMutex(t *testing.T) {
	var k keyedMutex
	ctx := context.Background()
	unlockA, err := k.lock(ctx, "a")
	require.NoError(t, err)
	unlockB, err := k.lock(ctx, "b")
	require.NoError(t, err, "another key is independent")

	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = k.lock(short, "a")
	require.ErrorIs(t, err, context.DeadlineExceeded, "a held key blocks")

	unlockA()
	unlockA() // idempotent
	again, err := k.lock(ctx, "a")
	require.NoError(t, err)
	again()
	unlockB()
	assert.Empty(t, k.locks, "unused keys are dropped")
}

// --- datastore-scoped scan ------------------------------------------------------

// TestCreate_InPlaceDisk_UnreadableVMOnTheDatastoreFailsClosed: a VM on the
// disk's datastore whose configuration vCenter cannot load could be using the
// disk, so the attach is refused (retryable).
func TestCreate_InPlaceDisk_UnreadableVMOnTheDatastoreFailsClosed(t *testing.T) {
	p, _ := newOwnershipSim(t)
	imageJSON := makeOwnMigratedDisk(t, p, "migrated")
	p.client.RoundTripper = &hideVMConfig{RoundTripper: p.client.RoundTripper, hidden: vmRef(seededVMID(t, p, simUnstamped))}

	_, err := p.Create(context.Background(), &providerv1.CreateRequest{
		Name: "migrated", ImageJson: imageJSON, Owner: ownerNamed(ownerTeamA, "migrated"),
	})
	requireCode(t, err, codes.Unavailable)
	assert.Equal(t, []string{contracts.VMDiskCheckFailedReason}, errorReasons(err))
}

// TestCreate_InPlaceDisk_UnreadableVMElsewhereDoesNotBlock: a VM whose
// configuration cannot be loaded blocks only checks on the datastores it has
// files on.
func TestCreate_InPlaceDisk_UnreadableVMElsewhereDoesNotBlock(t *testing.T) {
	p, _ := newOwnershipSim(t, func(m *simulator.Model) { m.Datastore = 2 })
	ctx := context.Background()
	const otherDS = "LocalDS_1"
	diskPath := "[" + otherDS + "] " + landingRelPath(ownerTeamA.Namespace, "migrated")
	makeImportedDisk(t, p, "["+otherDS+"] "+landingName(ownerTeamA.Namespace, "migrated"), diskPath)
	// Every seeded VM lives on LocalDS_0; hide one of them.
	p.client.RoundTripper = &hideVMConfig{RoundTripper: p.client.RoundTripper, hidden: vmRef(seededVMID(t, p, simUnstamped))}

	resp, err := p.Create(ctx, &providerv1.CreateRequest{
		Name: "migrated", ImageJson: importedDiskImageJSON(diskPath, true), Owner: ownerNamed(ownerTeamA, "migrated"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{diskPath}, vmDiskFiles(t, p, resp.Id))
}

func TestDiskScanCache(t *testing.T) {
	var c diskScanCache
	ctx := context.Background()
	var calls atomic.Int32
	release := make(chan struct{})
	scan := func(context.Context) (*inventoryFileUsers, error) {
		calls.Add(1)
		<-release
		return newInventoryFileUsers(), nil
	}

	// Concurrent callers share one scan.
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.get(ctx, "ds", false, scan)
			assert.NoError(t, err)
		}()
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond) // let the other callers join the flight
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load(), "one scan for concurrent callers")

	_, err := c.get(ctx, "ds", false, scan)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load(), "reused within the TTL")
	_, err = c.get(ctx, "ds", true, scan)
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load(), "fresh scans again")
	c.invalidate("ds")
	_, err = c.get(ctx, "ds", false, scan)
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load(), "invalidated scans again")
	_, err = c.get(ctx, "other", false, scan)
	require.NoError(t, err)
	assert.Equal(t, int32(4), calls.Load(), "keys are independent")

	failing := func(context.Context) (*inventoryFileUsers, error) { calls.Add(1); return nil, errors.New("boom") }
	_, err = c.get(ctx, "bad", false, failing)
	require.Error(t, err)
	_, err = c.get(ctx, "bad", false, failing)
	require.Error(t, err)
	assert.Equal(t, int32(6), calls.Load(), "a failed scan is never reused")
}

// --- disk identity --------------------------------------------------------------

// vmWithDisks is a VM (for inventoryFileUsers) whose devices have the given
// backings.
func vmWithDisks(id string, backings ...types.BaseVirtualDeviceBackingInfo) mo.VirtualMachine {
	vm := mo.VirtualMachine{Config: &types.VirtualMachineConfigInfo{}}
	vm.Self = vmRef(id)
	for _, b := range backings {
		vm.Config.Hardware.Device = append(vm.Config.Hardware.Device,
			&types.VirtualDisk{VirtualDevice: types.VirtualDevice{Backing: b}})
	}
	return vm
}

// flat is a flat disk backing at file with uuid and optional parent.
func flat(file, uuid string, parent *types.VirtualDiskFlatVer2BackingInfo) *types.VirtualDiskFlatVer2BackingInfo {
	return &types.VirtualDiskFlatVer2BackingInfo{
		VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: file}, Uuid: uuid, Parent: parent,
	}
}

func TestInventoryFileUsers_Identity(t *testing.T) {
	const (
		landing     = "[nfs-a] team-a.web-migrated/team-a.web-migrated.vmdk"
		landingUUID = "60 00 C2 9a 11 22 33 44-55 66 77 88 99 aa bb cc" // QueryVirtualDiskUuid spelling
	)
	f, ok := datastoreFileOf(landing)
	require.True(t, ok)
	none := types.ManagedObjectReference{}

	cases := map[string]struct {
		vm      mo.VirtualMachine
		used    bool
		unknown bool
	}{
		"same path, other case": {vm: vmWithDisks("vm-1", flat("[NFS-A] Team-A.web-migrated/team-a.web-migrated.vmdk", "", nil)), used: true},
		"same disk under another datastore name (UUID)": {
			vm: vmWithDisks("vm-2", flat("[nfs-b] team-a.web-migrated/team-a.web-migrated.vmdk", "6000C29a-1122-3344-5566-778899aabbcc", nil)), used: true},
		"same disk under a host path (UUID)": {
			vm: vmWithDisks("vm-3", flat("[] /vmfs/volumes/5f0e/team-a.web-migrated/team-a.web-migrated.vmdk", "6000c29a11223344556677889 9aabbcc", nil)), used: true},
		"linked-clone delta whose parent is the disk": {
			vm: vmWithDisks("vm-4", flat("[nfs-a] other/other-000001.vmdk", "6000c29a00000000000000000000beef", flat(landing, "", nil))), used: true},
		"host path without UUID": {vm: vmWithDisks("vm-5", flat("[] /vmfs/volumes/5f0e/x/x.vmdk", "", nil)), unknown: true},
		"another disk":           {vm: vmWithDisks("vm-6", flat("[nfs-a] other/other.vmdk", "6000c29a00000000000000000000dead", nil))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			u := newInventoryFileUsers()
			u.addVM(&tc.vm)
			assert.Equal(t, tc.used, u.usedBy(f, landingUUID, none), "in use")
			assert.Equal(t, tc.unknown, u.unknownOtherThan(none), "unknown")
			assert.False(t, u.usedBy(f, landingUUID, tc.vm.Self), "a VM never counts against itself")
		})
	}

	t.Run("an unparseable CD-ROM is not an unknown disk", func(t *testing.T) {
		vm := mo.VirtualMachine{Config: &types.VirtualMachineConfigInfo{}}
		vm.Self = vmRef("vm-7")
		vm.Config.Hardware.Device = append(vm.Config.Hardware.Device, &types.VirtualCdrom{VirtualDevice: types.VirtualDevice{
			Backing: &types.VirtualCdromIsoBackingInfo{VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: "/usr/lib/vmware/isoimages/linux.iso"}},
		}})
		u := newInventoryFileUsers()
		u.addVM(&vm)
		assert.False(t, u.unknownOtherThan(none))
	})
}

// TestCreate_InPlaceDisk_RefusesDiskUnderALinkedCloneDelta: a VM whose disk is
// a delta on top of the landing disk (a linked clone, a snapshot) uses it,
// even though no device names it directly.
func TestCreate_InPlaceDisk_RefusesDiskUnderALinkedCloneDelta(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	imageJSON := makeOwnMigratedDisk(t, p, "web")
	other := object.NewVirtualMachine(p.client.Client, vmRef(seededVMID(t, p, simUnstamped)))
	devices, err := other.Device(ctx)
	require.NoError(t, err)
	ctrl, err := devices.FindDiskController("")
	require.NoError(t, err)
	delta := &types.VirtualDisk{VirtualDevice: types.VirtualDevice{
		Key: -1, ControllerKey: ctrl.GetVirtualController().Key, UnitNumber: types.NewInt32(5),
		Backing: flat("["+simDatastore+"] "+simUnstamped+"/"+simUnstamped+"-000001.vmdk", "", flat(ownMigratedDiskPath("web"), "", nil)),
	}}
	task, err := other.Reconfigure(ctx, types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd,
			FileOperation: types.VirtualDeviceConfigSpecFileOperationCreate, Device: delta},
	}})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))

	_, err = p.Create(ctx, &providerv1.CreateRequest{Name: "web", ImageJson: imageJSON, Owner: wireOwner(ownerTeamA)})
	requireCode(t, err, codes.FailedPrecondition)
	assert.Equal(t, []string{contracts.VMDiskInUseReason}, errorReasons(err))
}

// --- account permissions --------------------------------------------------------

// withoutSystemRead removes System.Read from the privileges vCenter reports
// for entities of type kind, as for an account restricted below the root.
type withoutSystemRead struct {
	soap.RoundTripper
	kind string
}

func (r *withoutSystemRead) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if err := r.RoundTripper.RoundTrip(ctx, req, res); err != nil {
		return err
	}
	body, ok := res.(*methods.FetchUserPrivilegeOnEntitiesBody)
	if !ok || body.Res == nil {
		return nil
	}
	for i := range body.Res.Returnval {
		pr := &body.Res.Returnval[i]
		if pr.Entity.Type != r.kind {
			continue
		}
		var kept []string
		for _, priv := range pr.Privileges {
			if priv != privSystemRead {
				kept = append(kept, priv)
			}
		}
		pr.Privileges = kept
	}
	return nil
}

// TestCreate_InPlaceDisk_RequiresSystemRead: VMs the account may not see are
// left out of every listing, so without System.Read on the root folder and
// the datastore nothing is attached in place, and nothing is imported.
func TestCreate_InPlaceDisk_RequiresSystemRead(t *testing.T) {
	for _, kind := range []string{"Folder", datastoreMoType} {
		t.Run(kind, func(t *testing.T) {
			p, _ := newOwnershipSim(t)
			imageJSON := makeOwnMigratedDisk(t, p, "migrated")
			p.client.RoundTripper = &withoutSystemRead{RoundTripper: p.client.RoundTripper, kind: kind}

			_, err := p.Create(context.Background(), &providerv1.CreateRequest{
				Name: "migrated", ImageJson: imageJSON, Owner: ownerNamed(ownerTeamA, "migrated"),
			})
			requireCode(t, err, codes.Unavailable)
			assert.Equal(t, []string{contracts.VMDiskCheckFailedReason}, errorReasons(err))
			assert.Equal(t, inventoryNotReadableMessage, status.Convert(err).Message())

			err = p.ensureImportFolderUnused(context.Background(), simDatastore, landingName(ownerTeamA.Namespace, "db"), true)
			requireCode(t, err, codes.Unavailable)
		})
	}
}
