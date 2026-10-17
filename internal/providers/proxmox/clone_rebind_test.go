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
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin rc2 R4 on Proxmox (pvefake only; there is no real PVE
// cluster in CI): a VMClone's Clone, which now names its target
// VirtualMachine's UID, is never made twice. A re-sent Clone — after a lost
// answer, while PVE still clones, or after a provider restart — re-binds the
// clone the first attempt made and never allocates a second VMID; what the
// provider cannot prove is its own is refused, as for a Create.

// cloneTargetName is the target VirtualMachine's name of the clones below.
const cloneTargetName = "web-clone"

// cloneTargetOwnerID is the target VirtualMachine the clones are made for.
func cloneTargetOwnerID() contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: testOwnerUID, Namespace: testNamespace, Name: cloneTargetName}
}

// seededCloneSource puts a stopped source VM (VMID 300, owned by another
// VirtualMachine) on the fake and returns its provider id.
func seededCloneSource(srv *pvefake.Server) string {
	srv.PutVM(pvefake.VM{VMID: 300, Name: "web", Status: "stopped", Node: "pve", CPUs: 1,
		Description: (&ownerStamp{owner: otherOwner(), phase: ownerStampPhaseReady}).line()})
	return "300"
}

// stampedCloneReq clones source for cloneTargetOwnerID, sized by
// sizedClassJSON.
func stampedCloneReq(source string) *providerv1.CloneRequest {
	return &providerv1.CloneRequest{SourceVmId: source, TargetName: cloneTargetName,
		TargetVm: ownerProto(cloneTargetOwnerID()), ClassJson: sizedClassJSON}
}

// TestGetCapabilities_AdvertisesOwnerRebind: the production Proxmox capability
// set reports that a re-sent Create or Clone re-binds the VM stamped with its
// owner (rc2 R4); an image without it is never sent one again.
func TestGetCapabilities_AdvertisesOwnerRebind(t *testing.T) {
	p := &Provider{capabilities: GetProviderCapabilities()}
	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerRebind())
}

// TestClone_ResentWhilePVEStillClonesMakesNoSecondVMID: the first Clone gave
// up waiting for PVE's clone task. Re-sent by the same provider (its in-flight
// record) and by a restarted one (the unnamed guest locked for clone, with
// this PVE user's running task), it is answered outcome-unknown and starts no
// second clone. Once the task ends, the re-sent Clone finishes that VM and
// returns it.
func TestClone_ResentWhilePVEStillClonesMakesNoSecondVMID(t *testing.T) {
	srv, endpoint := startSlowFake(t)
	ctx := context.Background()
	src := seededCloneSource(srv)
	p := slowTaskProvider(t, endpoint)

	_, err := p.Clone(ctx, stampedCloneReq(src))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err), "the clone is still running")
	require.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"))

	_, err = p.Clone(ctx, stampedCloneReq(src))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"), "no second clone")

	restarted := slowTaskProvider(t, endpoint)
	_, err = restarted.Clone(ctx, stampedCloneReq(src))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"), "no second clone after a restart")

	srv.CompleteTasks()
	srv.SetTaskDelay(0)
	resp, err := restarted.Clone(ctx, stampedCloneReq(src))
	require.NoError(t, err)
	vms := srv.VMsNamed(cloneTargetName)
	require.Len(t, vms, 1, "one clone, one VMID")
	assert.Equal(t, strconv.Itoa(vms[0].VMID), resp.TargetVmId)
	s := stampOf(t, vms[0])
	assert.Equal(t, cloneTargetOwnerID(), s.owner)
	assert.Equal(t, ownerStampPhaseReady, s.phase)
	assert.Equal(t, 4, vms[0].CPUs, "the interrupted clone's sizing is applied")
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"))
}

// TestClone_LostAnswerIsReboundNotRepeated: PVE made the clone but its answer
// to the clone request was lost; the re-sent Clone finds the VM by its name
// and stamp, finishes it, and returns it — no second VMID.
func TestClone_LostAnswerIsReboundNotRepeated(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ctx := context.Background()
	src := seededCloneSource(srv)
	srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusBadGateway,
		Body: "lost", AfterApply: true, Times: 1})

	_, err := p.Clone(ctx, stampedCloneReq(src))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	require.Len(t, srv.VMsNamed(cloneTargetName), 1, "PVE made the clone")

	resp, err := p.Clone(ctx, stampedCloneReq(src))
	require.NoError(t, err)
	vms := srv.VMsNamed(cloneTargetName)
	require.Len(t, vms, 1, "no second VMID")
	assert.Equal(t, strconv.Itoa(vms[0].VMID), resp.TargetVmId)
	assert.Equal(t, ownerStampPhaseReady, stampOf(t, vms[0]).phase)
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"))
}

// TestClone_UnprovableOwnershipIsRefused: a same-named VM the provider cannot
// prove is the target's own clone fails closed (AlreadyExists), and nothing is
// cloned — the classes a Create uses.
func TestClone_UnprovableOwnershipIsRefused(t *testing.T) {
	mine := (&ownerStamp{owner: cloneTargetOwnerID(), phase: ownerStampPhaseReady}).line()
	cases := map[string]pvefake.VM{
		"an unreadable stamp": {VMID: 4260, Name: cloneTargetName, Status: "stopped", Node: "pve",
			Description: "virtrigaud.owner v1 uid=" + testOwnerUID},
		"a template with the target's stamp": {VMID: 4261, Name: cloneTargetName, Status: "stopped", Node: "pve",
			Template: 1, Description: mine},
		"the target's UID with another namespace": {VMID: 4262, Name: cloneTargetName, Status: "stopped", Node: "pve",
			Description: (&ownerStamp{owner: contracts.ObjectIdentity{UID: testOwnerUID, Namespace: "team-z", Name: cloneTargetName},
				phase: ownerStampPhaseReady}).line()},
	}
	for name, vm := range cases {
		t.Run(name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			src := seededCloneSource(srv)
			srv.PutVM(vm)
			_, err := p.Clone(context.Background(), stampedCloneReq(src))
			require.Error(t, err)
			assert.Equal(t, codes.AlreadyExists, status.Code(err), "%v", err)
			assert.NotContains(t, err.Error(), "team-z")
			assert.Empty(t, srv.MutatingRequests(), "nothing is cloned or changed")
		})
	}
}

// TestClone_SameNamedVMOfAnotherOwnerIsLeftAlone: PVE names are not unique,
// so a same-named VM stamped for another VirtualMachine (or unstamped) is
// neither re-bound nor a conflict: the clone is made next to it, stamped.
func TestClone_SameNamedVMOfAnotherOwnerIsLeftAlone(t *testing.T) {
	p, srv := newStampTestProvider(t)
	src := seededCloneSource(srv)
	srv.PutVM(pvefake.VM{VMID: 4263, Name: cloneTargetName, Status: "stopped", Node: "pve",
		Description: (&ownerStamp{owner: otherOwner(), phase: ownerStampPhaseReady}).line()})

	resp, err := p.Clone(context.Background(), stampedCloneReq(src))
	require.NoError(t, err)
	assert.NotEqual(t, "4263", resp.TargetVmId)
	assert.Equal(t, cloneTargetOwnerID(), stampOf(t, vmByID(t, srv, resp.TargetVmId)).owner)
	other, _ := srv.GetVM(4263)
	assert.Equal(t, otherOwner(), stampOf(t, other).owner, "the other VM is untouched")
}
