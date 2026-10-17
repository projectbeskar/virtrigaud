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
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// adoptedOwner is the VirtualMachine adoption creates for the PVE VM named
// "Web_01" (contracts.AdoptedVMName): it takes the VM over in these tests.
var adoptedOwner = contracts.ObjectIdentity{UID: "5e1f0a2b-3c4d-4e5f-8a6b-7c8d9e0f1a2b", Namespace: "infra", Name: "web-01"}

// detachedOwner is the VirtualMachine that created the VM and was detached
// with orphan-on-delete.
var detachedOwner = contracts.ObjectIdentity{UID: testOwnerUID, Namespace: testNamespace, Name: "web-01"}

// putAdoptable seeds VM 700 "Web_01", stopped, with description.
func putAdoptable(srv *pvefake.Server, description string) {
	srv.PutVM(pvefake.VM{VMID: 700, Name: "Web_01", Node: "pve", Status: "stopped", Description: description})
}

func transferReq(replaceable ...string) *providerv1.TransferOwnerRequest {
	return &providerv1.TransferOwnerRequest{Id: "700", Owner: ownerProto(adoptedOwner), ReplaceableOwnerUids: replaceable}
}

// TestTransferOwner_ReAdoptedVMIsDeletableByItsNewOwner is the rc1 known
// limitation fixed end to end: a VM stamped by a VirtualMachine that was
// detached (orphan-on-delete) and then adopted again. Without the transfer
// the new VirtualMachine's Delete is VM_NOT_OWNED and leaves the VM; once the
// manager hands the VM over (the old owner reported replaceable), the stamp is
// the new owner's — the notes are kept — and its Delete removes the VM.
func TestTransferOwner_ReAdoptedVMIsDeletableByItsNewOwner(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()
	putAdoptable(srv, "team notes\n"+ownerStamp{owner: detachedOwner, phase: ownerStampPhaseReady}.line())

	_, err := p.Delete(ctx, deleteReq("700", &adoptedOwner))
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Contains(t, deleteErrorReasons(err), contracts.VMNotOwnedReason, "the limitation this transfer removes")

	_, err = p.TransferOwner(ctx, transferReq(detachedOwner.UID))
	require.NoError(t, err)
	vm := vmByID(t, srv, "700")
	assert.Equal(t, ownerStamp{owner: adoptedOwner, phase: ownerStampPhaseReady}, stampOf(t, vm))
	assert.True(t, strings.HasPrefix(vm.Description, "team notes\n"), "the VM's notes are kept: %q", vm.Description)

	listed, err := p.ListVMs(ctx, &providerv1.ListVMsRequest{})
	require.NoError(t, err)
	for _, info := range listed.GetVms() {
		if info.GetId() == "700" {
			assert.Equal(t, adoptedOwner.UID, info.GetProviderRaw()[contracts.VMInfoOwnerUIDKey])
		}
	}

	_, err = p.TransferOwner(ctx, transferReq(detachedOwner.UID))
	require.NoError(t, err, "an idempotent retry succeeds")

	_, err = p.Delete(ctx, deleteReq("700", &adoptedOwner))
	require.NoError(t, err)
	_, exists := srv.GetVM(700)
	assert.False(t, exists)
}

// TestTransferOwner_StampsAnUnstampedVM: an unstamped VM (never created by
// VirtRigaud, or created before owner stamps) is stamped.
func TestTransferOwner_StampsAnUnstampedVM(t *testing.T) {
	p, srv := newStampTestProvider(t)
	putAdoptable(srv, "")

	_, err := p.TransferOwner(context.Background(), transferReq())
	require.NoError(t, err)
	assert.Equal(t, adoptedOwner, stampOf(t, vmByID(t, srv, "700")).owner)
}

// TestTransferOwner_Refusals: what is never stamped, and with which answer —
// always without a config write.
func TestTransferOwner_Refusals(t *testing.T) {
	live := otherOwner()
	cases := []struct {
		name        string
		vm          pvefake.VM
		replaceable []string
		want        codes.Code
	}{
		{"stamped by an owner not reported gone", pvefake.VM{Name: "Web_01",
			Description: ownerStamp{owner: live, phase: ownerStampPhaseReady}.line()}, nil, codes.AlreadyExists},
		{"another uid reported gone does not unlock a live owner", pvefake.VM{Name: "Web_01",
			Description: ownerStamp{owner: live, phase: ownerStampPhaseReady}.line()}, []string{detachedOwner.UID}, codes.AlreadyExists},
		{"unreadable stamp", pvefake.VM{Name: "Web_01", Description: "virtrigaud.owner v1 uid=x"}, nil, codes.AlreadyExists},
		{"create not finished (phase=creating)", pvefake.VM{Name: "Web_01",
			Description: ownerStamp{owner: detachedOwner, phase: ownerStampPhaseCreating}.line()}, []string{detachedOwner.UID}, codes.AlreadyExists},
		{"template", pvefake.VM{Name: "Web_01", Template: 1}, nil, codes.AlreadyExists},
		{"VMID reused by a VM of another name", pvefake.VM{Name: "Database_01"}, nil, codes.NotFound},
		{"locked by a PVE task", pvefake.VM{Name: "Web_01", Lock: "backup"}, nil, codes.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			vm := tc.vm
			vm.VMID, vm.Node, vm.Status = 700, "pve", "stopped"
			srv.PutVM(vm)
			before := len(srv.MutatingRequests())

			_, err := p.TransferOwner(context.Background(), transferReq(tc.replaceable...))
			require.Error(t, err)
			assert.Equal(t, tc.want, status.Code(err), "error: %v", err)
			assert.NotContains(t, err.Error(), live.UID, "a refusal never names another owner")
			assert.NotContains(t, err.Error(), live.Namespace)
			assert.Len(t, srv.MutatingRequests(), before, "nothing was written")
			after, _ := srv.GetVM(700)
			assert.Equal(t, vm.Description, after.Description)
		})
	}
}

// TestTransferOwner_GoneVMAndBadRequests: a VMID free cluster-wide is
// NotFound; a malformed request is InvalidArgument before any PVE call.
func TestTransferOwner_GoneVMAndBadRequests(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()

	_, err := p.TransferOwner(ctx, &providerv1.TransferOwnerRequest{Id: "7777", Owner: ownerProto(adoptedOwner)})
	assert.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)

	before := len(srv.Requests())
	for name, req := range map[string]*providerv1.TransferOwnerRequest{
		"no owner":          {Id: "700"},
		"owner without ns":  {Id: "700", Owner: &providerv1.ObjectIdentity{Uid: adoptedOwner.UID, Name: "web-01"}},
		"malformed id":      {Id: "web", Owner: ownerProto(adoptedOwner)},
		"a host":            {Id: "700", TargetHostId: "pve", Owner: ownerProto(adoptedOwner)},
		"malformed replace": {Id: "700", Owner: ownerProto(adoptedOwner), ReplaceableOwnerUids: []string{"a,b"}},
	} {
		_, err := p.TransferOwner(ctx, req)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", name, err)
	}
	assert.Len(t, srv.Requests(), before, "no PVE call for a malformed request")
}

// TestTransferOwner_ConcurrentChangeIsNeverOverwritten: the description is
// written with the config digest the decision was made on. Another writer
// that re-stamps the VM in between (another adopter) wins: PVE refuses the
// stale write, this transfer fails retryably (VM_OPERATION_FAILED, kept out
// of the circuit breaker), and its retry is refused — the VM is someone
// else's now.
func TestTransferOwner_ConcurrentChangeIsNeverOverwritten(t *testing.T) {
	p, srv := newStampTestProvider(t)
	putAdoptable(srv, "")
	racer := otherOwner()
	racer.Name = "web-01"
	srv.InjectFault(pvefake.Fault{Method: http.MethodPut, PathContains: "/qemu/700/config", Times: 1, PassThrough: true,
		Then: func(*http.Request) {
			srv.SetDescription(700, ownerStamp{owner: racer, phase: ownerStampPhaseReady}.line())
		}})

	_, err := p.TransferOwner(context.Background(), transferReq())
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, []string{contracts.VMOperationFailedReason}, deleteErrorReasons(err))
	assert.Equal(t, racer, stampOf(t, vmByID(t, srv, "700")).owner, "the concurrent writer's stamp is kept")

	_, err = p.TransferOwner(context.Background(), transferReq())
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
}

// TestGetCapabilities_AdvertisesOwnerTransfer: the Proxmox provider reports
// supports_owner_transfer.
func TestGetCapabilities_AdvertisesOwnerTransfer(t *testing.T) {
	caps, err := New().GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerTransfer())
	assert.False(t, caps.GetSupportsRoutedAdoption())
}
