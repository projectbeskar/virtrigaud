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

package proxmox

import (
	"context"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	// seededTemplateID is the pvefake seeded template (9000).
	seededTemplateID = 9000
	// templateImageJSON clones the seeded template.
	templateImageJSON = `{"TemplateName":"9000"}`
	// sizedClassJSON is a VMClass whose sizing a clone must get.
	sizedClassJSON = `{"CPU": 4, "MemoryMiB": 4096}`
)

// newStampTestProvider starts a fake PVE whose tasks finish at once and a
// provider that polls them quickly.
func newStampTestProvider(t *testing.T) (*Provider, *pvefake.Server) {
	t.Helper()
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	srv.SetTaskDelay(0)
	client, err := pveapi.NewClient(&pveapi.Config{
		Endpoint:           endpoint,
		TokenID:            "test@pve!token",
		TokenSecret:        "secret",
		InsecureSkipVerify: true,
		TaskPollInterval:   5 * time.Millisecond,
	})
	require.NoError(t, err)
	p := New()
	p.client = client
	return p, srv
}

func ownerProto(o contracts.ObjectIdentity) *providerv1.ObjectIdentity {
	return &providerv1.ObjectIdentity{Uid: o.UID, Namespace: o.Namespace, Name: o.Name}
}

func otherOwner() contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: testOtherUID, Namespace: "team-b", Name: testOwnerName}
}

// vmByID returns the fake VM a provider id names.
func vmByID(t *testing.T, srv *pvefake.Server, id string) pvefake.VM {
	t.Helper()
	vmid, err := strconv.Atoi(id)
	require.NoError(t, err, "id %q", id)
	vm, ok := srv.GetVM(vmid)
	require.True(t, ok, "VM %d must exist", vmid)
	return vm
}

// stampOf reads the one readable owner stamp of a fake VM.
func stampOf(t *testing.T, vm pvefake.VM) ownerStamp {
	t.Helper()
	read := readOwnerStamp(vm.Description)
	require.Empty(t, read.state, "description %q", vm.Description)
	require.NotNil(t, read.stamp, "description %q", vm.Description)
	return *read.stamp
}

func createReq(owner contracts.ObjectIdentity, image string) *providerv1.CreateRequest {
	return &providerv1.CreateRequest{
		Name:      owner.Name,
		ClassJson: sizedClassJSON,
		ImageJson: image,
		Owner:     ownerProto(owner),
	}
}

func TestCreate_StampsDirectCreate(t *testing.T) {
	p, srv := newStampTestProvider(t)
	resp, err := p.Create(context.Background(), createReq(testOwner(), ""))
	require.NoError(t, err)

	s := stampOf(t, vmByID(t, srv, resp.Id))
	assert.Equal(t, testOwner(), s.owner)
	assert.Equal(t, ownerStampPhaseReady, s.phase, "a plain create has no later step")
}

func TestCreate_StampsTemplateCloneAndKeepsTemplateNotes(t *testing.T) {
	p, srv := newStampTestProvider(t)
	// The template carries notes and, hand-copied, someone else's stamp.
	srv.SetDescription(seededTemplateID, testUserNotes+"\n"+testStamp(testOtherUID, ownerStampPhaseReady).line())

	resp, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	require.NoError(t, err)

	vm := vmByID(t, srv, resp.Id)
	s := stampOf(t, vm)
	assert.Equal(t, testOwner(), s.owner)
	assert.Equal(t, ownerStampPhaseReady, s.phase)
	assert.Equal(t, testUserNotes+"\n"+s.line(), vm.Description, "the template's notes are kept, its stamp is not")
	assert.Equal(t, 4, vm.CPUs)
}

func TestCreate_WithoutOwnerIsUnstampedAndStripsInheritedStamp(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.SetDescription(seededTemplateID, testStamp(testOtherUID, ownerStampPhaseReady).line())

	resp, err := p.Create(context.Background(), &providerv1.CreateRequest{Name: "legacy", ImageJson: templateImageJSON})
	require.NoError(t, err)
	vm := vmByID(t, srv, resp.Id)
	assert.False(t, isOwnerStampLine(vm.Description), "an owner-less clone never inherits a stamp: %q", vm.Description)

	resp, err = p.Create(context.Background(), &providerv1.CreateRequest{Name: "legacy-direct"})
	require.NoError(t, err)
	assert.Empty(t, vmByID(t, srv, resp.Id).Description)
}

// TestCreate_ResentCreateRebinds: a Create sent again for the same owner
// returns the VM the first one made and creates nothing.
func TestCreate_ResentCreateRebinds(t *testing.T) {
	for name, image := range map[string]string{"direct": "", "template": templateImageJSON} {
		t.Run(name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			ctx := context.Background()
			first, err := p.Create(ctx, createReq(testOwner(), image))
			require.NoError(t, err)

			mutatingBefore := len(srv.MutatingRequests())
			second, err := p.Create(ctx, createReq(testOwner(), image))
			require.NoError(t, err)
			assert.Equal(t, first.Id, second.Id)
			assert.Len(t, srv.VMsNamed(testOwnerName), 1, "no duplicate VM")
			assert.Equal(t, mutatingBefore, len(srv.MutatingRequests()), "a ready VM is returned without any change")
		})
	}
}

// TestCreate_ResumesInterruptedClone: a VM left "creating" (the provider died
// after the clone) is finished and marked ready by the re-sent Create.
func TestCreate_ResumesInterruptedClone(t *testing.T) {
	p, srv := newStampTestProvider(t)
	half := testStamp(testOwnerUID, ownerStampPhaseCreating)
	srv.PutVM(pvefake.VM{VMID: 4242, Name: testOwnerName, Status: "stopped", Node: "pve", CPUs: 1,
		Description: "notes\n" + half.line()})

	resp, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	require.NoError(t, err)
	assert.Equal(t, "4242", resp.Id)
	vm := vmByID(t, srv, "4242")
	assert.Equal(t, 4, vm.CPUs, "the interrupted clone's sizing is applied")
	s := stampOf(t, vm)
	assert.Equal(t, ownerStampPhaseReady, s.phase)
	assert.Equal(t, "notes\n"+s.line(), vm.Description)
	assert.Len(t, srv.VMsNamed(testOwnerName), 1)
}

// TestCreate_LockedInterruptedVMIsLeftAlone: a VM a PVE task still works on is
// not touched, and the answer leaves the outcome unknown.
func TestCreate_LockedInterruptedVMIsLeftAlone(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 4243, Name: testOwnerName, Status: "stopped", Node: "pve", Lock: "clone",
		Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()})

	_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.False(t, contracts.IsCreateNotPerformed(err), "a VM exists: never CREATE_NOT_PERFORMED")
	assert.Len(t, srv.VMsNamed(testOwnerName), 1)
	assert.Empty(t, srv.MutatingRequests())
}

// TestCreate_RebindAddressesVMOnAnotherNode: a re-bound VM that lives on a
// node other than the default one is returned as "<node>:<vmid>".
func TestCreate_RebindAddressesVMOnAnotherNode(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 4244, Name: testOwnerName, Status: "running", Node: "pve2",
		Description: testStamp(testOwnerUID, ownerStampPhaseReady).line()})

	resp, err := p.Create(context.Background(), createReq(testOwner(), ""))
	require.NoError(t, err)
	assert.Equal(t, "pve2:4244", resp.Id)
}

// TestCreate_VMOfAnotherOwnerIsNotRebound: same name, another owner's stamp:
// a new VM is created next to it (PVE names are not unique).
func TestCreate_VMOfAnotherOwnerIsNotRebound(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 4245, Name: testOwnerName, Status: "running", Node: "pve",
		Description: (&ownerStamp{owner: otherOwner(), phase: ownerStampPhaseReady}).line()})

	resp, err := p.Create(context.Background(), createReq(testOwner(), ""))
	require.NoError(t, err)
	assert.NotEqual(t, "4245", resp.Id)
	assert.Equal(t, testOwner(), stampOf(t, vmByID(t, srv, resp.Id)).owner)
	other, _ := srv.GetVM(4245)
	assert.Equal(t, otherOwner(), stampOf(t, other).owner, "the other owner's VM is untouched")
}

// TestCreate_UnprovableOwnershipIsRefused: the cases where the provider cannot
// tell which VM the requester owns fail closed (AlreadyExists) and create
// nothing.
func TestCreate_UnprovableOwnershipIsRefused(t *testing.T) {
	mine := testStamp(testOwnerUID, ownerStampPhaseReady).line()
	cases := map[string][]pvefake.VM{
		"same-named VM with an unreadable stamp": {
			{VMID: 4250, Name: testOwnerName, Status: "stopped", Node: "pve", Description: "virtrigaud.owner v1 uid=" + testOwnerUID},
		},
		"same-named VM with two owners": {
			{VMID: 4251, Name: testOwnerName, Status: "stopped", Node: "pve",
				Description: mine + "\n" + testStamp(testOtherUID, ownerStampPhaseReady).line()},
		},
		"template with this owner's stamp": {
			{VMID: 4252, Name: testOwnerName, Status: "stopped", Node: "pve", Template: 1, Description: mine},
		},
		"two VMs with this owner's stamp": {
			{VMID: 4253, Name: testOwnerName, Status: "stopped", Node: "pve", Description: mine},
			{VMID: 4254, Name: testOwnerName, Status: "stopped", Node: "pve2", Description: mine},
		},
		"this owner's UID with another namespace": {
			{VMID: 4255, Name: testOwnerName, Status: "stopped", Node: "pve",
				Description: (&ownerStamp{owner: contracts.ObjectIdentity{UID: testOwnerUID, Namespace: "team-z", Name: testOwnerName},
					phase: ownerStampPhaseReady}).line()},
		},
	}
	for name, vms := range cases {
		t.Run(name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			for _, vm := range vms {
				srv.PutVM(vm)
			}
			_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
			require.Error(t, err)
			assert.Equal(t, codes.AlreadyExists, status.Code(err))
			assert.NotContains(t, err.Error(), "team-z")
			assert.Empty(t, srv.MutatingRequests(), "nothing is created or changed")
		})
	}
}

// TestCreate_UnreadableTemplateOfSameNameIsIgnored: a template is never bound,
// so its stamp does not block a create.
func TestCreate_UnreadableTemplateOfSameNameIsIgnored(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 4256, Name: testOwnerName, Status: "stopped", Node: "pve", Template: 1, Description: "virtrigaud.owner ?"})
	_, err := p.Create(context.Background(), createReq(testOwner(), ""))
	require.NoError(t, err)
}

func TestCreate_InvalidOwnerIsRefusedBeforeAnyCall(t *testing.T) {
	p, srv := newStampTestProvider(t)
	req := createReq(testOwner(), "")
	req.Owner.Namespace = "team a"
	_, err := p.Create(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, srv.Requests())
}

// TestClone_StampsAndRebindsWithTargetUID: a clone for a target VirtualMachine
// with a UID is stamped with it (never with the source's owner), and a
// re-sent Clone returns it.
func TestClone_StampsAndRebindsWithTargetUID(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()
	src, err := p.Create(ctx, createReq(otherOwner(), ""))
	require.NoError(t, err)

	target := contracts.ObjectIdentity{UID: testOwnerUID, Namespace: testNamespace, Name: "web-clone"}
	req := &providerv1.CloneRequest{SourceVmId: src.Id, TargetName: target.Name, TargetVm: ownerProto(target), ClassJson: sizedClassJSON}
	first, err := p.Clone(ctx, req)
	require.NoError(t, err)
	vm := vmByID(t, srv, first.TargetVmId)
	s := stampOf(t, vm)
	assert.Equal(t, target, s.owner)
	assert.Equal(t, ownerStampPhaseReady, s.phase)
	assert.Equal(t, 4, vm.CPUs)

	second, err := p.Clone(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, first.TargetVmId, second.TargetVmId)
	assert.Len(t, srv.VMsNamed("web-clone"), 1)
}

// TestClone_WithoutTargetUIDDropsSourceStamp: a clone whose target
// VirtualMachine has no UID yet is unstamped; it never claims the source's
// owner (PVE copies the source's description when the request has none).
func TestClone_WithoutTargetUIDDropsSourceStamp(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()
	src, err := p.Create(ctx, createReq(otherOwner(), ""))
	require.NoError(t, err)
	require.True(t, isOwnerStampLine(vmByID(t, srv, src.Id).Description))

	// L5: the source's description is only its stamp, so the clone request
	// carries an explicit note (PVE ignores an empty one and would copy the
	// stamp); no description fix-up is needed after the clone.
	putsBefore := countRequests(srv, http.MethodPut, "/config")
	resp, err := p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: src.Id, TargetName: "copy",
		TargetVm: &providerv1.ObjectIdentity{Namespace: testNamespace, Name: "copy"}})
	require.NoError(t, err)
	assert.Equal(t, unstampedCloneNote, vmByID(t, srv, resp.TargetVmId).Description)
	assert.Equal(t, putsBefore, countRequests(srv, http.MethodPut, "/config"), "the stamp is never inherited, not even briefly")

	// A source without a description: nothing is sent, nothing is copied.
	srv.SetDescription(mustAtoi(t, src.Id), "")
	resp, err = p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: src.Id, TargetName: "copy0"})
	require.NoError(t, err)
	assert.Empty(t, vmByID(t, srv, resp.TargetVmId).Description)

	// Notes of the source are kept.
	srv.SetDescription(mustAtoi(t, src.Id), testUserNotes+"\n"+(&ownerStamp{owner: otherOwner(), phase: ownerStampPhaseReady}).line())
	resp, err = p.Clone(ctx, &providerv1.CloneRequest{SourceVmId: src.Id, TargetName: "copy2"})
	require.NoError(t, err)
	assert.Equal(t, testUserNotes, vmByID(t, srv, resp.TargetVmId).Description)
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

// TestListVMs_ReportsOwnerStamps: owner_uid and owner_stamp_state per VM; a VM
// whose config cannot be read is listed as unreadable, never skipped.
func TestListVMs_ReportsOwnerStamps(t *testing.T) {
	p, srv := newStampTestProvider(t)
	mine := testStamp(testOwnerUID, ownerStampPhaseReady).line()
	other := testStamp(testOtherUID, ownerStampPhaseReady).line()
	srv.PutVM(pvefake.VM{VMID: 501, Name: "owned", Status: "running", Node: "pve", Description: "notes\n" + mine})
	srv.PutVM(pvefake.VM{VMID: 502, Name: "unstamped", Status: "stopped", Node: "pve", Description: "notes"})
	srv.PutVM(pvefake.VM{VMID: 503, Name: "broken", Status: "stopped", Node: "pve", Description: "virtrigaud.owner v1 uid=x"})
	srv.PutVM(pvefake.VM{VMID: 504, Name: "two", Status: "stopped", Node: "pve", Description: mine + "\n" + other})
	srv.PutVM(pvefake.VM{VMID: 505, Name: "creating", Status: "stopped", Node: "pve", Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()})
	srv.PutVM(pvefake.VM{VMID: 506, Name: "unreadable-config", Status: "stopped", Node: "pve"})
	srv.PutVM(pvefake.VM{VMID: 507, Name: "stamped-template", Status: "stopped", Node: "pve", Template: 1, Description: mine})
	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/506/config", Status: http.StatusInternalServerError, Body: "boom"})

	resp, err := p.ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err)
	byID := map[string]*providerv1.VMInfo{}
	for _, vm := range resp.Vms {
		byID[vm.Id] = vm
	}

	assert.Equal(t, testOwnerUID, byID["501"].ProviderRaw[contracts.VMInfoOwnerUIDKey])
	assert.NotContains(t, byID["501"].ProviderRaw, contracts.VMInfoOwnerStampStateKey)
	assert.Empty(t, byID["501"].OwnerNamespace, "a thin-client provider leaves the typed owner fields empty")

	assert.NotContains(t, byID["502"].ProviderRaw, contracts.VMInfoOwnerUIDKey)
	assert.NotContains(t, byID["502"].ProviderRaw, contracts.VMInfoOwnerStampStateKey)

	assert.Equal(t, contracts.OwnerStampUnreadable, byID["503"].ProviderRaw[contracts.VMInfoOwnerStampStateKey])

	assert.Equal(t, contracts.OwnerStampMultiple, byID["504"].ProviderRaw[contracts.VMInfoOwnerStampStateKey])
	assert.Equal(t, testOwnerUID+","+testOtherUID, byID["504"].ProviderRaw[contracts.VMInfoOwnerUIDKey])

	assert.Equal(t, testOwnerUID, byID["505"].ProviderRaw[contracts.VMInfoOwnerUIDKey], "a half-created VM is owned too")

	require.Contains(t, byID, "506", "a VM whose config cannot be read is still listed")
	assert.Equal(t, contracts.OwnerStampUnreadable, byID["506"].ProviderRaw[contracts.VMInfoOwnerStampStateKey])

	assert.NotContains(t, byID, "507", "templates are skipped")
	assert.NotContains(t, byID, strconv.Itoa(seededTemplateID))
}

// TestListVMs_NodeDownListsItsVMsAsUnreadable: ListVMs is built on
// /cluster/resources, which lists a down node's VMs (status "unknown"). Their
// config cannot be read, so they are reported unreadable — never left out,
// and the other nodes' VMs are listed as usual.
func TestListVMs_NodeDownListsItsVMsAsUnreadable(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 520, Name: "on-down-node", Node: "pve2", Status: "running",
		Description: testStamp(testOwnerUID, ownerStampPhaseReady).line()})
	srv.SetNodeOffline("pve2", true)

	resp, err := p.ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err, "one node down does not fail the listing")
	byID := map[string]*providerv1.VMInfo{}
	for _, vm := range resp.Vms {
		byID[vm.Id] = vm
	}
	require.Contains(t, byID, "520")
	assert.Equal(t, contracts.OwnerStampUnreadable, byID["520"].ProviderRaw[contracts.VMInfoOwnerStampStateKey])
	assert.Equal(t, "unknown", byID["520"].ProviderRaw["status"])
	assert.Contains(t, byID, "100", "the up node's VMs are listed")
}

func TestListVMs_FailsWhenTheClusterCannotBeListed(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/cluster/resources", Status: http.StatusInternalServerError, Body: "boom"})
	_, err := p.ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.Error(t, err, "a partial listing is never answered")
}

func TestListVMs_HonoursNodeSelector(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 521, Name: "other-node", Node: "pve2", Status: "stopped"})
	p.client.Config().NodeSelector = []string{"pve"}
	resp, err := p.ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err)
	for _, vm := range resp.Vms {
		assert.NotEqual(t, "521", vm.Id)
	}
}

func deleteReq(id string, owner *contracts.ObjectIdentity) *providerv1.DeleteRequest {
	req := &providerv1.DeleteRequest{Id: id}
	if owner != nil {
		req.Owner = ownerProto(*owner)
	}
	return req
}

// TestDelete_OwnerChecks: with an owner on the request, Delete destroys only
// a VM that is this owner's or unstamped; a VM stamped for another owner, or a
// template, is NotFound and untouched; an unreliable stamp holds the delete.
func TestDelete_OwnerChecks(t *testing.T) {
	mine := testOwner()
	cases := []struct {
		name        string
		vm          pvefake.VM
		owner       *contracts.ObjectIdentity
		wantCode    codes.Code
		wantRemoved bool
		wantReason  string
	}{
		{"own VM", pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseReady).line()}, &mine, codes.OK, true, ""},
		{"own half-created VM", pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()}, &mine, codes.OK, true, ""},
		{"unstamped (pre-upgrade) VM with the VirtualMachine's name", pvefake.VM{Description: "notes"}, &mine, codes.OK, true, ""},
		{"unstamped VM with another name (a reused VMID)", pvefake.VM{Name: "someone-else", Description: "notes"}, &mine, codes.NotFound, false, contracts.VMNotOwnedReason},
		{"VM of another owner", pvefake.VM{Description: testStamp(testOtherUID, ownerStampPhaseReady).line()}, &mine, codes.NotFound, false, contracts.VMNotOwnedReason},
		{"template", pvefake.VM{Template: 1}, &mine, codes.FailedPrecondition, false, contracts.VMIsImageArtifactReason},
		{"template, no owner on the request", pvefake.VM{Template: 1}, nil, codes.FailedPrecondition, false, contracts.VMIsImageArtifactReason},
		{"unreadable stamp", pvefake.VM{Description: "virtrigaud.owner v1 tampered"}, &mine, codes.FailedPrecondition, false, contracts.OwnerStampUnreadableReason},
		{"two owners", pvefake.VM{Description: testStamp(testOwnerUID, ownerStampPhaseReady).line() + "\n" +
			testStamp(testOtherUID, ownerStampPhaseReady).line()}, &mine, codes.FailedPrecondition, false, contracts.OwnerStampUnreadableReason},
		{"no owner on the request: unchanged", pvefake.VM{Description: testStamp(testOtherUID, ownerStampPhaseReady).line()}, nil, codes.OK, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			vm := tc.vm
			vm.VMID, vm.Node, vm.Status = 600, "pve", "running"
			if vm.Name == "" {
				vm.Name = testOwnerName
			}
			srv.PutVM(vm)

			_, err := p.Delete(context.Background(), deleteReq("600", tc.owner))
			assert.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
			if tc.wantReason != "" {
				assert.Equal(t, []string{tc.wantReason}, deleteErrorReasons(err))
			}
			_, exists := srv.GetVM(600)
			assert.Equal(t, !tc.wantRemoved, exists)
			if !tc.wantRemoved {
				got, _ := srv.GetVM(600)
				assert.Equal(t, "running", got.Status, "a refused delete does not even stop the VM")
			}
		})
	}
}

// TestDelete_OwnerChecks_AdoptedName: an unstamped VM adopted under the
// adoption controller's name for it (contracts.AdoptedVMName: the sanitized
// PVE name, or "vm-<vmid>") is deletable with its VirtualMachine's owner; an
// unstamped VM with any other name is still NotFound and untouched.
func TestDelete_OwnerChecks_AdoptedName(t *testing.T) {
	cases := []struct {
		name        string
		pveName     string
		ownerName   string
		wantCode    codes.Code
		wantRemoved bool
	}{
		{"sanitized PVE name", "Web_01.Prod", "web-01-prod", codes.OK, true},
		{"fallback vm-<vmid> for a name with nothing usable", "日本", "vm-610", codes.OK, true},
		{"exact PVE name", "web", "web", codes.OK, true},
		{"another name (a reused VMID)", "Database_01", "web-01-prod", codes.NotFound, false},
		{"fallback name of another VMID", "日本", "vm-611", codes.NotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			srv.PutVM(pvefake.VM{VMID: 610, Name: tc.pveName, Node: "pve", Status: "stopped", Description: "notes"})
			owner := contracts.ObjectIdentity{UID: testOwnerUID, Namespace: testNamespace, Name: tc.ownerName}

			_, err := p.Delete(context.Background(), deleteReq("610", &owner))
			assert.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
			_, exists := srv.GetVM(610)
			assert.Equal(t, !tc.wantRemoved, exists)
		})
	}
}

func TestDelete_ConfigReadFailureHoldsTheDelete(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 601, Name: "victim", Node: "pve", Status: "stopped"})
	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/601/config", Status: http.StatusInternalServerError, Body: "boom"})
	mine := testOwner()
	_, err := p.Delete(context.Background(), deleteReq("601", &mine))
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	_, exists := srv.GetVM(601)
	assert.True(t, exists)
}

// deleteErrorReasons returns the google.rpc.ErrorInfo reasons err carries.
func deleteErrorReasons(err error) []string {
	var out []string
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			out = append(out, info.GetReason())
		}
	}
	return out
}
