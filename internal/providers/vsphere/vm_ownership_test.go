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
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	transportgrpc "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// vcsim (simulator.VPX) inventory the ownership tests build on.
const (
	simTemplate  = "DC0_H0_VM0" // a seeded VM, used as the clone template
	simUnstamped = "DC0_H0_VM1" // a seeded VM in DC0/vm that VirtRigaud never created
	simCluster   = "DC0_C0"     // provider DefaultCluster
	simDatastore = "LocalDS_0"  // provider DefaultDatastore
	simOtherDir  = "team-b-vms" // a second VM folder created by the tests under DC0/vm
	simTemplateJ = `{"TemplateName":"` + simTemplate + `"}`
)

var (
	ownerTeamA = contracts.ObjectIdentity{UID: "3f0c7d52-1d4e-4a57-9f55-0a1b2c3d4e5f", Namespace: "team-a", Name: "web"}
	ownerTeamB = contracts.ObjectIdentity{UID: "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d", Namespace: "team-b", Name: "web"}
)

func wireOwner(o contracts.ObjectIdentity) *providerv1.ObjectIdentity {
	return &providerv1.ObjectIdentity{Uid: o.UID, Namespace: o.Namespace, Name: o.Name}
}

// vcenterCloneSemantics wraps the vim25 round-tripper so the simulator honours
// CloneVM_Task's ExtraConfig the way vCenter does. vcsim (govmomi v0.52.0,
// simulator/virtual_machine.go CloneVMTask) copies spec.config into a VALUE
// copy of the new VM's config (`if dst, src := config, req.Spec.Config; ...`),
// so it silently ignores spec.config.extraConfig, and it does not copy the
// source VM's ExtraConfig to the clone either. vCenter does both: the clone
// starts with the source's ExtraConfig and spec.config.extraConfig is applied
// on top (a key set to "" is removed) as part of the same task.
//
// After a successful CloneVM_Task this fixture reconfigures the new VM with the
// source's virtrigaud.* ExtraConfig followed by the spec's ExtraConfig, before
// the caller observes the task result — reproducing both halves for the keys
// under test.
type vcenterCloneSemantics struct {
	soap.RoundTripper
	c *vim25.Client
}

func (r *vcenterCloneSemantics) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if err := r.RoundTripper.RoundTrip(ctx, req, res); err != nil {
		return err
	}
	cloneReq, ok := req.(*methods.CloneVM_TaskBody)
	if !ok || cloneReq.Req == nil {
		return nil
	}
	cloneRes, ok := res.(*methods.CloneVM_TaskBody)
	if !ok || cloneRes.Res == nil {
		return nil // a fault: the caller sees it
	}

	info, err := object.NewTask(r.c, cloneRes.Res.Returnval).WaitForResult(ctx, nil)
	if err != nil {
		return nil // the caller's own wait reports the failure
	}
	newVM, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return fmt.Errorf("fixture: unexpected clone result %T", info.Result)
	}

	var src mo.VirtualMachine
	if err := property.DefaultCollector(r.c).RetrieveOne(ctx, cloneReq.Req.This, []string{"config.extraConfig"}, &src); err != nil {
		return fmt.Errorf("fixture: read clone source ExtraConfig: %w", err)
	}
	var extra []types.BaseOptionValue
	if src.Config != nil {
		for _, bov := range src.Config.ExtraConfig {
			if o := bov.GetOptionValue(); o != nil && strings.HasPrefix(strings.ToLower(o.Key), "virtrigaud.") {
				extra = append(extra, bov)
			}
		}
	}
	if cfg := cloneReq.Req.Spec.Config; cfg != nil {
		extra = append(extra, cfg.ExtraConfig...)
	}
	if len(extra) == 0 {
		return nil
	}
	task, err := object.NewVirtualMachine(r.c, newVM).Reconfigure(ctx, types.VirtualMachineConfigSpec{ExtraConfig: extra})
	if err != nil {
		return fmt.Errorf("fixture: apply clone ExtraConfig: %w", err)
	}
	return task.Wait(ctx)
}

// newOwnershipSim starts an in-memory vCenter (vcsim VPX model) and returns a
// Provider connected to it with the placement defaults the create path needs,
// plus the model so a test can shape inventory the API itself forbids. Clone
// ExtraConfig semantics are made vCenter-faithful by vcenterCloneSemantics.
func newOwnershipSim(t *testing.T) (*Provider, *simulator.Model) {
	t.Helper()
	model := simulator.VPX()
	require.NoError(t, model.Create())
	server := model.Service.NewServer()

	u := server.URL
	pw, _ := u.User.Password()
	cfg := &Config{
		Endpoint:           (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(),
		Username:           u.User.Username(),
		Password:           pw,
		InsecureSkipVerify: true,
		DefaultCluster:     simCluster,
		DefaultDatastore:   simDatastore,
	}
	client, finder, err := createVSphereClient(cfg)
	require.NoError(t, err)
	client.RoundTripper = &vcenterCloneSemantics{RoundTripper: client.RoundTripper, c: client.Client}
	t.Cleanup(func() {
		_ = client.Logout(context.Background())
		server.Close()
		model.Remove()
	})

	p := &Provider{
		client: client,
		finder: finder,
		config: cfg,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The tests' image servers listen on 127.0.0.1.
		allowLoopbackImageSources: true,
	}
	// The clone source must be a real vSphere template (template_source.go).
	markAsTemplate(t, p, seededVMID(t, p, simTemplate))
	return p, model
}

// markAsTemplate powers VM id off (if needed) and marks it as a vSphere template.
func markAsTemplate(t *testing.T, p *Provider, id string) {
	t.Helper()
	ctx := context.Background()
	vm := object.NewVirtualMachine(p.client.Client, vmRef(id))
	if task, err := vm.PowerOff(ctx); err == nil {
		_ = task.Wait(ctx) // already powered off is fine
	}
	require.NoError(t, vm.MarkAsTemplate(ctx))
}

// templateCreateRequest is a template-clone CreateRequest for name, placed in
// folder (empty = provider default), carrying owner (nil = older manager).
func templateCreateRequest(name, folder string, owner *providerv1.ObjectIdentity) *providerv1.CreateRequest {
	req := &providerv1.CreateRequest{Name: name, ImageJson: simTemplateJ, Owner: owner}
	if folder != "" {
		req.PlacementJson = fmt.Sprintf(`{"Folder":%q}`, folder)
	}
	return req
}

func vmRef(id string) types.ManagedObjectReference {
	return types.ManagedObjectReference{Type: virtualMachineMoType, Value: id}
}

// vmState reads the owner stamp, the parent folder and the name of VM id.
func vmState(t *testing.T, p *Provider, id string) (owner contracts.ObjectIdentity, parent types.ManagedObjectReference, name string, extraConfig []types.BaseOptionValue) {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"name", "parent", "config.extraConfig"}, &vm))
	require.NotNil(t, vm.Config)
	require.NotNil(t, vm.Parent)
	owner, err := ownerFromExtraConfig(vm.Config.ExtraConfig)
	require.NoError(t, err)
	return owner, *vm.Parent, vm.Name, vm.Config.ExtraConfig
}

// defaultVMFolder returns DC0's default VM folder.
func defaultVMFolder(t *testing.T, p *Provider) *object.Folder {
	t.Helper()
	ctx := context.Background()
	dc, err := p.finder.DefaultDatacenter(ctx)
	require.NoError(t, err)
	p.finder.SetDatacenter(dc)
	folder, err := p.finder.DefaultFolder(ctx)
	require.NoError(t, err)
	return folder
}

// seededVMID returns the MOID of a VM the simulator seeded, found by name.
func seededVMID(t *testing.T, p *Provider, name string) string {
	t.Helper()
	refs, err := p.vmsNamedInFolder(context.Background(), defaultVMFolder(t, p), name)
	require.NoError(t, err)
	require.Len(t, refs, 1, "simulator should seed exactly one %q in DC0/vm", name)
	return refs[0].Value
}

func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, want, status.Code(err), "error: %v", err)
}

// --- pure helpers ------------------------------------------------------------

func TestInvalidVMNameError(t *testing.T) {
	valid := []string{
		"web", "web-01", "vm-web", "vm-1a", "vm-", "my-vm-12", "host-12", "domain-controller",
		"group-v4", "datastore-backup", "a.b.c", "VM-12",
	}
	for _, name := range valid {
		assert.NoError(t, invalidVMNameError(name), "name %q must be accepted", name)
	}

	invalid := []string{
		"", "   ",
		"vm-1", "vm-1234", "vm-0042", // vCenter VirtualMachine MOID shape
		"a/b", "/web", `a\b`, "a%2fb", // inventory path / vSphere-escaped characters
		"VirtualMachine:vm-12", // "Type:value" managed object reference form
	}
	for _, name := range invalid {
		err := invalidVMNameError(name)
		requireCode(t, err, codes.InvalidArgument)
	}
}

func TestOwnerExtraConfig(t *testing.T) {
	got, err := ownerFromExtraConfig(ownerExtraConfig(ownerTeamA))
	require.NoError(t, err)
	assert.Equal(t, ownerTeamA, got, "stamp must round-trip")

	for _, bov := range ownerExtraConfig(ownerTeamA) {
		assert.False(t, strings.HasPrefix(bov.GetOptionValue().Key, "guestinfo."),
			"owner keys must not be guest-readable guestinfo.* keys")
	}

	// A zero owner (and a namespace/name without a UID, which proves nothing)
	// yields the three keys with empty values, which clears an inherited stamp.
	for _, o := range []contracts.ObjectIdentity{{}, {Namespace: "ns", Name: "n"}} {
		entries := ownerExtraConfig(o)
		require.Len(t, entries, 3)
		for _, bov := range entries {
			assert.Equal(t, "", bov.GetOptionValue().Value, "key %s must be cleared", bov.GetOptionValue().Key)
		}
	}
}

func TestOwnerFromExtraConfig(t *testing.T) {
	ov := func(k string, v any) types.BaseOptionValue { return &types.OptionValue{Key: k, Value: v} }

	t.Run("unrelated keys ignored, keys case-insensitive", func(t *testing.T) {
		got, err := ownerFromExtraConfig([]types.BaseOptionValue{
			ov("guestinfo.userdata", "x"), nil, ov("Virtrigaud.Owner.UID", "u1"), ov("VIRTRIGAUD.OWNER.NAME", "web"),
		})
		require.NoError(t, err)
		assert.Equal(t, contracts.ObjectIdentity{UID: "u1", Name: "web"}, got)
	})
	t.Run("no stamp is zero", func(t *testing.T) {
		got, err := ownerFromExtraConfig([]types.BaseOptionValue{ov("govcsim", "TRUE")})
		require.NoError(t, err)
		assert.True(t, got.IsZero())
	})
	t.Run("repeated key fails closed", func(t *testing.T) {
		_, err := ownerFromExtraConfig([]types.BaseOptionValue{
			ov(ownerExtraConfigKeyUID, ownerTeamA.UID), ov(strings.ToUpper(ownerExtraConfigKeyUID), ownerTeamB.UID),
		})
		assert.Error(t, err)
	})
	t.Run("non-string value fails closed", func(t *testing.T) {
		_, err := ownerFromExtraConfig([]types.BaseOptionValue{ov(ownerExtraConfigKeyUID, int32(7))})
		assert.Error(t, err)
	})
}

func TestRequesterOwnsVM(t *testing.T) {
	cases := []struct {
		name                string
		requester, recorded contracts.ObjectIdentity
		want                bool
	}{
		{"same uid", ownerTeamA, ownerTeamA, true},
		{"same uid, different informational fields", ownerTeamA, contracts.ObjectIdentity{UID: ownerTeamA.UID, Namespace: "x", Name: "y"}, true},
		{"different uid", ownerTeamB, ownerTeamA, false},
		{"same namespace/name, different uid", contracts.ObjectIdentity{UID: "other", Namespace: "team-a", Name: "web"}, ownerTeamA, false},
		{"requester without uid", contracts.ObjectIdentity{Namespace: "team-a", Name: "web"}, ownerTeamA, false},
		{"unstamped vm", ownerTeamA, contracts.ObjectIdentity{}, false},
		{"both zero", contracts.ObjectIdentity{}, contracts.ObjectIdentity{}, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, requesterOwnsVM(c.requester, c.recorded), c.name)
	}
}

// TestCreate_RejectsUnsafeNamesBeforeAnyLookup proves the name check runs
// before any vCenter call: the provider has no finder, so any lookup would
// panic on a nil pointer.
func TestCreate_RejectsUnsafeNamesBeforeAnyLookup(t *testing.T) {
	p := &Provider{client: &govmomi.Client{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), config: &Config{}}

	_, err := p.Create(context.Background(), templateCreateRequest("vm-1234", "", wireOwner(ownerTeamA)))
	requireCode(t, err, codes.InvalidArgument)

	_, err = p.Create(context.Background(), templateCreateRequest("", "", wireOwner(ownerTeamA)))
	requireCode(t, err, codes.InvalidArgument)

	_, err = p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "vm-1", TargetName: "vm-99"})
	requireCode(t, err, codes.InvalidArgument)

	// An unsafe template reference is rejected before any lookup as well.
	for _, tmpl := range []string{"vm-12", "VirtualMachine:vm-12", `a\b`, "50%", "/DC0/vm/../vm", "/DC0/vm/vm-7"} {
		_, err = p.Create(context.Background(), &providerv1.CreateRequest{
			Name:      "web",
			ImageJson: fmt.Sprintf(`{"TemplateName":%q}`, tmpl),
			Owner:     wireOwner(ownerTeamA),
		})
		requireCode(t, err, codes.InvalidArgument)
	}
}

// --- vcsim: why MOID-shaped names are rejected --------------------------------

// TestGovmomiFinderResolvesMOIDShapedNames pins the govmomi behaviour the name
// check guards against: find.Finder resolves a bare "vm-N" argument as the
// managed object reference vm-N (object.ReferenceFromString), not as a name, so
// the pre-fix Create lookup `finder.VirtualMachine(ctx, name)` bound a
// VirtualMachine named "vm-N" to whatever VM had that MOID.
func TestGovmomiFinderResolvesMOIDShapedNames(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	victim := seededVMID(t, p, simUnstamped)
	require.True(t, vmMoIDPattern.MatchString(victim), "vcsim MOIDs have the vCenter shape, got %q", victim)

	vm, err := p.finder.VirtualMachine(ctx, victim)
	require.NoError(t, err, "no VM is NAMED %q, yet the finder resolves it", victim)
	assert.Equal(t, victim, vm.Reference().Value)
	assert.Error(t, invalidVMNameError(victim), "such a name must be rejected by Create")
}

// --- vcsim: Create ownership --------------------------------------------------

func TestCreate_StampsOwnerOnTemplateClone(t *testing.T) {
	p, _ := newOwnershipSim(t)

	resp, err := p.Create(context.Background(), templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	require.NotEmpty(t, resp.Id)

	owner, parent, name, _ := vmState(t, p, resp.Id)
	assert.Equal(t, ownerTeamA, owner, "the new VM must carry the requester's stamp")
	assert.Equal(t, "web", name)
	assert.Equal(t, defaultVMFolder(t, p).Reference(), parent, "created in the resolved target folder")
}

func TestCreate_StampsOwnerOnImportedDiskCreate(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	// CreateVM_Task attaches an existing disk; make one on the simulated datastore.
	dc, err := p.finder.DefaultDatacenter(ctx)
	require.NoError(t, err)
	const diskPath = "[" + simDatastore + "] imported/disk.vmdk"
	require.NoError(t, object.NewFileManager(p.client.Client).MakeDirectory(ctx, "["+simDatastore+"] imported", dc, true))
	task, err := object.NewVirtualDiskManager(p.client.Client).CreateVirtualDisk(ctx, diskPath, dc, &types.FileBackedVirtualDiskSpec{
		VirtualDiskSpec: types.VirtualDiskSpec{DiskType: string(types.VirtualDiskTypeThin), AdapterType: string(types.VirtualDiskAdapterTypeLsiLogic)},
		CapacityKb:      1024,
	})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))

	resp, err := p.Create(ctx, &providerv1.CreateRequest{
		Name:      "migrated",
		ImageJson: fmt.Sprintf(`{"Path":%q,"Format":"vmdk"}`, diskPath),
		Owner:     wireOwner(ownerTeamA),
	})
	require.NoError(t, err)

	owner, _, _, _ := vmState(t, p, resp.Id)
	assert.Equal(t, ownerTeamA, owner, "the imported-disk (CreateVM_Task) path must stamp the owner too")
}

func TestCreate_ExistingVMInTargetFolder(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	first, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)

	t.Run("same owner binds idempotently", func(t *testing.T) {
		again, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
		require.NoError(t, err)
		assert.Equal(t, first.Id, again.Id)

		refs, err := p.vmsNamedInFolder(ctx, defaultVMFolder(t, p), "web")
		require.NoError(t, err)
		assert.Len(t, refs, 1, "a retried create must not make a second VM")
	})

	t.Run("different owner is refused without disclosing the owner", func(t *testing.T) {
		resp, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamB)))
		requireCode(t, err, codes.AlreadyExists)
		assert.Nil(t, resp)
		msg := status.Convert(err).Message()
		assert.Contains(t, msg, `"web"`)
		for _, secret := range []string{ownerTeamA.UID, ownerTeamA.Namespace, first.Id} {
			assert.NotContains(t, msg, secret, "the conflict message must not disclose the other owner or the VM")
		}
	})

	t.Run("request without owner is refused", func(t *testing.T) {
		_, err := p.Create(ctx, templateCreateRequest("web", "", nil))
		requireCode(t, err, codes.AlreadyExists)
	})

	t.Run("owner with the same namespace/name but another UID is refused", func(t *testing.T) {
		recreated := ownerTeamA
		recreated.UID = "11111111-2222-4333-8444-555555555555" // same CR name, recreated => new UID
		_, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(recreated)))
		requireCode(t, err, codes.AlreadyExists)
	})

	// The stamp on the existing VM is untouched by the refused creates.
	owner, _, _, _ := vmState(t, p, first.Id)
	assert.Equal(t, ownerTeamA, owner)
}

// TestCreate_RefusesUnstampedPreExistingVM is the reported vulnerability: a
// VirtualMachine named like a VM VirtRigaud did not create used to be bound to
// it (and deleting the VirtualMachine then destroyed that VM).
func TestCreate_RefusesUnstampedPreExistingVM(t *testing.T) {
	p, _ := newOwnershipSim(t)
	victim := seededVMID(t, p, simUnstamped)

	resp, err := p.Create(context.Background(), templateCreateRequest(simUnstamped, "", wireOwner(ownerTeamA)))
	requireCode(t, err, codes.AlreadyExists)
	assert.Nil(t, resp)

	owner, _, _, _ := vmState(t, p, victim)
	assert.True(t, owner.IsZero(), "a refused create must not stamp the existing VM")
}

// TestCreate_SameNameInAnotherFolder: vSphere allows same-named VMs in
// different folders. A VM in another folder is neither bound (the pre-fix
// datacenter-wide search returned it) nor blocks the create in the target
// folder; a create targeting ITS folder is refused.
func TestCreate_SameNameInAnotherFolder(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	_, err := defaultVMFolder(t, p).CreateFolder(ctx, simOtherDir)
	require.NoError(t, err)

	foreign, err := p.Create(ctx, templateCreateRequest("shared", simOtherDir, wireOwner(ownerTeamB)))
	require.NoError(t, err)
	_, foreignParent, _, _ := vmState(t, p, foreign.Id)
	require.NotEqual(t, defaultVMFolder(t, p).Reference(), foreignParent, "setup: foreign VM lives in the other folder")

	mine, err := p.Create(ctx, templateCreateRequest("shared", "", wireOwner(ownerTeamA)))
	require.NoError(t, err, "a same-named VM in another folder must not block the create")
	assert.NotEqual(t, foreign.Id, mine.Id, "must never bind to the VM in the other folder")

	owner, parent, _, _ := vmState(t, p, mine.Id)
	assert.Equal(t, ownerTeamA, owner)
	assert.Equal(t, defaultVMFolder(t, p).Reference(), parent)

	foreignOwner, _, _, _ := vmState(t, p, foreign.Id)
	assert.Equal(t, ownerTeamB, foreignOwner, "the other folder's VM is untouched")

	_, err = p.Create(ctx, templateCreateRequest("shared", simOtherDir, wireOwner(ownerTeamA)))
	requireCode(t, err, codes.AlreadyExists)
}

// TestCreate_MultipleSameNamedVMsInFolderFailsClosed: vCenter keeps VM names
// unique per folder, so two matches mean an unexpected inventory; the create
// is refused even though one of them carries the requester's stamp. The API
// forbids producing the duplicate, so the simulator's registry is edited.
func TestCreate_MultipleSameNamedVMsInFolderFailsClosed(t *testing.T) {
	p, model := newOwnershipSim(t)
	ctx := context.Background()

	mine, err := p.Create(ctx, templateCreateRequest("dup", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	other, err := p.Create(ctx, templateCreateRequest("dup-other", "", wireOwner(ownerTeamB)))
	require.NoError(t, err)

	reg := model.Map()
	obj, ok := reg.Get(vmRef(other.Id)).(*simulator.VirtualMachine)
	require.True(t, ok)
	reg.WithLock(model.Service.Context, obj, func() { obj.Name = "dup" })

	refs, err := p.vmsNamedInFolder(ctx, defaultVMFolder(t, p), "dup")
	require.NoError(t, err)
	require.Len(t, refs, 2, "setup: two VMs named dup in one folder")

	resp, err := p.Create(ctx, templateCreateRequest("dup", "", wireOwner(ownerTeamA)))
	requireCode(t, err, codes.AlreadyExists)
	assert.Nil(t, resp)
	_ = mine
}

// TestCreate_NoOwnerStillCreatesWhenNameIsFree: an older manager (no owner)
// can still create new VMs; they are simply unstamped (and so can never be
// bound by a later create).
func TestCreate_NoOwnerStillCreatesWhenNameIsFree(t *testing.T) {
	p, _ := newOwnershipSim(t)

	resp, err := p.Create(context.Background(), templateCreateRequest("legacy-manager", "", nil))
	require.NoError(t, err)
	owner, _, _, _ := vmState(t, p, resp.Id)
	assert.True(t, owner.IsZero())
}

// TestClone_DropsSourceOwnerStamp: CloneRequest carries no owner, and the clone
// must not claim the source VirtualMachine's owner. vCenter copies the source's
// ExtraConfig (and so its stamp) to a clone; the Clone RPC's spec clears it.
func TestClone_DropsSourceOwnerStamp(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	src, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)

	// Control: a plain clone (no config) inherits the source's stamp, so the
	// assertion below can only pass because the Clone RPC clears it.
	srcVM := object.NewVirtualMachine(p.client.Client, vmRef(src.Id))
	cluster, err := p.finder.ClusterComputeResource(ctx, simCluster)
	require.NoError(t, err)
	pool, err := cluster.ResourcePool(ctx)
	require.NoError(t, err)
	poolRef := pool.Reference()
	task, err := srcVM.Clone(ctx, defaultVMFolder(t, p), "plain-copy", types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{Pool: &poolRef},
	})
	require.NoError(t, err)
	info, err := task.WaitForResult(ctx, nil)
	require.NoError(t, err)
	plainRef, ok := info.Result.(types.ManagedObjectReference)
	require.True(t, ok)
	inherited, _, _, _ := vmState(t, p, plainRef.Value)
	require.Equal(t, ownerTeamA, inherited, "control: a clone inherits the source's ExtraConfig")

	resp, err := p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: src.Id, TargetName: "web-copy"})
	require.NoError(t, err)

	owner, _, name, _ := vmState(t, p, resp.TargetVmId)
	assert.Equal(t, "web-copy", name)
	assert.True(t, owner.IsZero(), "a clone must not claim the source's owner, got %+v", owner)
}

// TestCreate_TemplateStampIsNeverInherited: a template that carries
// an owner stamp (e.g. converted from a VirtRigaud-created VM) must not pass it
// to a VM created by a request without an owner, and a request WITH an owner
// replaces it.
func TestCreate_TemplateStampIsNeverInherited(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	tmpl, err := p.Create(ctx, templateCreateRequest("golden", "", wireOwner(ownerTeamB)))
	require.NoError(t, err)
	markAsTemplate(t, p, tmpl.Id)
	fromTemplate := func(name string, owner *providerv1.ObjectIdentity) *providerv1.CreateRequest {
		return &providerv1.CreateRequest{Name: name, ImageJson: `{"TemplateName":"golden"}`, Owner: owner}
	}

	noOwner, err := p.Create(ctx, fromTemplate("from-golden-legacy", nil))
	require.NoError(t, err)
	owner, _, _, _ := vmState(t, p, noOwner.Id)
	assert.True(t, owner.IsZero(), "an ownerless create must not inherit the template's stamp, got %+v", owner)

	withOwner, err := p.Create(ctx, fromTemplate("from-golden", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	owner, _, _, _ = vmState(t, p, withOwner.Id)
	assert.Equal(t, ownerTeamA, owner)
}

// --- vcsim: Describe -----------------------------------------------------------

// TestDescribe_OnlyMissingVMReportsNotExists: the manager answers Exists=false
// by clearing Status.ID and calling Create again, so only a definitive
// ManagedObjectNotFound may produce it; a transient failure must be an error.
func TestDescribe_OnlyMissingVMReportsNotExists(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	existing := seededVMID(t, p, simUnstamped)

	resp, err := p.Describe(ctx, &providerv1.DescribeRequest{Id: existing})
	require.NoError(t, err)
	assert.True(t, resp.Exists)

	resp, err = p.Describe(ctx, &providerv1.DescribeRequest{Id: "vm-987654"})
	require.NoError(t, err)
	assert.False(t, resp.Exists, "a VM that does not exist is reported as such")

	// Lose the session: the lookup now fails with NotAuthenticated, which is
	// not evidence that the VM is gone.
	require.NoError(t, p.client.Logout(ctx))
	resp, err = p.Describe(ctx, &providerv1.DescribeRequest{Id: existing})
	require.Error(t, err, "a transient failure must not be reported as Exists=false")
	assert.Nil(t, resp)
}

// --- over the wire ---------------------------------------------------------------

// serveOverGRPC serves p over a loopback gRPC listener and returns the
// MANAGER-side transport client dialed to it, so a test observes exactly what
// crosses the wire and how the manager classifies it.
func serveOverGRPC(t *testing.T, p *Provider) *transportgrpc.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gsrv := grpc.NewServer()
	providerv1.RegisterProviderServer(gsrv, p)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	c, err := transportgrpc.NewClient(context.Background(), lis.Addr().String(), "vsphere", "owner-roundtrip", nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestCreate_OwnershipOverTheWire serves the real vSphere Provider over gRPC
// and drives it with the manager's transport client, pinning that the owner
// reaches the provider and that the refusals come back typed: Conflict (from
// codes.AlreadyExists, non-retryable) and InvalidSpec (codes.InvalidArgument).
func TestCreate_OwnershipOverTheWire(t *testing.T) {
	p, _ := newOwnershipSim(t)
	c := serveOverGRPC(t, p)
	ctx := context.Background()

	req := func(name string, owner contracts.ObjectIdentity) contracts.CreateRequest {
		return contracts.CreateRequest{Name: name, Image: contracts.VMImage{TemplateName: simTemplate}, Owner: owner}
	}

	created, err := c.Create(ctx, req("web", ownerTeamA))
	require.NoError(t, err)
	owner, _, _, _ := vmState(t, p, created.ID)
	assert.Equal(t, ownerTeamA, owner, "the manager's owner is stamped on the VM")

	again, err := c.Create(ctx, req("web", ownerTeamA))
	require.NoError(t, err)
	assert.Equal(t, created.ID, again.ID)

	_, err = c.Create(ctx, req("web", ownerTeamB))
	require.Error(t, err)
	assert.True(t, contracts.IsConflict(err), "got %v", err)
	var pe *contracts.ProviderError
	require.ErrorAs(t, err, &pe)
	assert.False(t, pe.IsRetryable())

	_, err = c.Create(ctx, req("vm-12", ownerTeamA))
	assert.True(t, contracts.IsInvalidSpec(err), "got %v", err)
}
