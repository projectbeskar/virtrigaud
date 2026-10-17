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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin rc2 R4 on vSphere (vcsim): a Clone that names its target
// VirtualMachine's UID stamps the clone with it, a re-sent Clone returns that
// VM instead of cloning again, and a same-named VM the target does not own is
// refused with AlreadyExists — before the source is read or changed.

// cloneTargetVM is the target VirtualMachine the clones below are made for.
var cloneTargetVM = contracts.ObjectIdentity{UID: "6b1c2d3e-4f50-4a61-8b72-9c8d7e6f5a4b", Namespace: "team-a", Name: "web-copy"}

// cloneReqFor clones source into cloneTargetVM's name, carrying target (nil:
// a manager older than the stamp).
func cloneReqFor(source string, target *providerv1.ObjectIdentity) *providerv1.CloneRequest {
	return &providerv1.CloneRequest{SourceVmId: source, TargetName: cloneTargetVM.Name, TargetVm: target}
}

// vmsNamed returns the VMs named name in DC0's default VM folder.
func vmsNamed(t *testing.T, p *Provider, name string) []string {
	t.Helper()
	refs, err := p.vmsNamedInFolder(context.Background(), defaultVMFolder(t, p), name)
	require.NoError(t, err)
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.Value)
	}
	return ids
}

// hasSnapshot reports whether VM id has any snapshot.
func hasSnapshot(t *testing.T, p *Provider, id string) bool {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, object.NewVirtualMachine(p.client.Client, vmRef(id)).Properties(context.Background(), vmRef(id),
		[]string{"snapshot"}, &vm))
	return vm.Snapshot != nil && len(vm.Snapshot.RootSnapshotList) > 0
}

// TestGetCapabilities_AdvertisesOwnerRebind: vSphere reports that a re-sent
// Create or Clone re-binds the VM stamped with its owner (rc2 R4).
func TestGetCapabilities_AdvertisesOwnerRebind(t *testing.T) {
	caps, err := (&Provider{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerRebind())
}

func TestClone_StampsTheTargetAndRebindsAResend(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	src, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)

	first, err := p.Clone(ctx, cloneReqFor(src.Id, wireOwner(cloneTargetVM)))
	require.NoError(t, err)
	owner, _, name, _ := vmState(t, p, first.TargetVmId)
	assert.Equal(t, cloneTargetVM.Name, name)
	assert.Equal(t, cloneTargetVM, owner, "the clone is stamped with its target VirtualMachine, never the source's owner")

	second, err := p.Clone(ctx, cloneReqFor(src.Id, wireOwner(cloneTargetVM)))
	require.NoError(t, err, "a re-sent Clone re-binds")
	assert.Equal(t, first.TargetVmId, second.TargetVmId)
	assert.Equal(t, []string{first.TargetVmId}, vmsNamed(t, p, cloneTargetVM.Name), "no second clone")
}

// TestClone_RefusesATargetItDoesNotOwn: a VM of the target name that is
// unstamped, stamped for another VirtualMachine or a template — or any VM
// there when the request carries no UID — is never returned: AlreadyExists,
// and nothing is cloned. A linked clone's base snapshot is never taken on the
// source either.
func TestClone_RefusesATargetItDoesNotOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		existingOwner *providerv1.ObjectIdentity
		template      bool
		req           *providerv1.ObjectIdentity
	}{
		"unstamped":                     {existingOwner: nil, req: wireOwner(cloneTargetVM)},
		"another VirtualMachine's":      {existingOwner: wireOwner(ownerTeamB), req: wireOwner(cloneTargetVM)},
		"a template with its own stamp": {existingOwner: wireOwner(cloneTargetVM), template: true, req: wireOwner(cloneTargetVM)},
		"a request without a UID":       {existingOwner: wireOwner(cloneTargetVM), req: nil},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := newOwnershipSim(t)
			ctx := context.Background()
			src, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
			require.NoError(t, err)
			existing, err := p.Create(ctx, templateCreateRequest(cloneTargetVM.Name, "", tc.existingOwner))
			require.NoError(t, err)
			if tc.template {
				markAsTemplate(t, p, existing.Id)
			}

			req := cloneReqFor(src.Id, tc.req)
			req.Linked = true
			_, err = p.Clone(ctx, req)
			requireCode(t, err, codes.AlreadyExists)
			assert.Contains(t, status.Convert(err).Message(), "is not owned by the clone's target VirtualMachine")
			assert.NotContains(t, status.Convert(err).Message(), ownerTeamB.UID, "the other owner is never named")
			assert.Equal(t, []string{existing.Id}, vmsNamed(t, p, cloneTargetVM.Name), "nothing is cloned")
			assert.False(t, hasSnapshot(t, p, src.Id), "the source is not changed")
		})
	}
}

// TestClone_WithoutAUIDIsUnchanged: a manager older than the stamp gets
// today's unstamped clone when the target name is free.
func TestClone_WithoutAUIDIsUnchanged(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	src, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)

	resp, err := p.Clone(ctx, cloneReqFor(src.Id, &providerv1.ObjectIdentity{Namespace: "team-a", Name: cloneTargetVM.Name}))
	require.NoError(t, err)
	owner, _, _, _ := vmState(t, p, resp.TargetVmId)
	assert.True(t, owner.IsZero(), "no UID, no stamp: got %+v", owner)
}
