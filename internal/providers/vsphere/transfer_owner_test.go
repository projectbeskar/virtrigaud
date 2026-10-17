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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// adopterTeamC is the VirtualMachine that adopts a VM in these tests.
var adopterTeamC = contracts.ObjectIdentity{UID: "5e1f0a2b-3c4d-4e5f-8a6b-7c8d9e0f1a2b", Namespace: "team-c", Name: "dc0-h0-vm1"}

// transferRequest is the manager's single-host TransferOwner request for VM id.
func transferRequest(id string, replaceable ...string) *providerv1.TransferOwnerRequest {
	return &providerv1.TransferOwnerRequest{Id: id, Owner: wireOwner(adopterTeamC), ReplaceableOwnerUids: replaceable}
}

// stampVM sets VM id's owner stamp to owner, as Create would have.
func stampVM(t *testing.T, p *Provider, id string, owner contracts.ObjectIdentity) {
	t.Helper()
	ctx := context.Background()
	task, err := object.NewVirtualMachine(p.client.Client, vmRef(id)).Reconfigure(ctx,
		types.VirtualMachineConfigSpec{ExtraConfig: ownerExtraConfig(owner)})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
}

// listedOwnerUID is the owner UID ListVMs reports for VM id.
func listedOwnerUID(t *testing.T, p *Provider, id string) string {
	t.Helper()
	resp, err := p.ListVMs(context.Background(), &providerv1.ListVMsRequest{})
	require.NoError(t, err)
	for _, vm := range resp.GetVms() {
		if vm.GetId() == id {
			return vm.GetProviderRaw()[contracts.VMInfoOwnerUIDKey]
		}
	}
	t.Fatalf("VM %s not listed", id)
	return ""
}

// TestTransferOwner_StampsAnUnstampedVM: a VM VirtRigaud never created is
// stamped with its adopter (uid, namespace, name); ListVMs then reports it
// owned, and a retry is idempotent.
func TestTransferOwner_StampsAnUnstampedVM(t *testing.T) {
	p, _ := newOwnershipSim(t)
	id := seededVMID(t, p, simUnstamped)
	require.Empty(t, listedOwnerUID(t, p, id))

	_, err := p.TransferOwner(context.Background(), transferRequest(id))
	require.NoError(t, err)
	owner, _, _, _ := vmState(t, p, id)
	assert.Equal(t, adopterTeamC, owner)
	assert.Equal(t, adopterTeamC.UID, listedOwnerUID(t, p, id))

	_, err = p.TransferOwner(context.Background(), transferRequest(id))
	require.NoError(t, err, "an idempotent retry succeeds")
}

// TestTransferOwner_ReplacesOnlyAReplaceableStamp: a VM stamped by a
// VirtualMachine the manager did not report gone is refused (AlreadyExists,
// a message naming no owner) and untouched; once that UID is reported
// replaceable, the VM is re-stamped.
func TestTransferOwner_ReplacesOnlyAReplaceableStamp(t *testing.T) {
	p, _ := newOwnershipSim(t)
	id := seededVMID(t, p, simUnstamped)
	stampVM(t, p, id, ownerTeamA)

	_, err := p.TransferOwner(context.Background(), transferRequest(id))
	requireCode(t, err, codes.AlreadyExists)
	assert.NotContains(t, err.Error(), ownerTeamA.UID)
	assert.NotContains(t, err.Error(), ownerTeamA.Namespace)
	owner, _, _, _ := vmState(t, p, id)
	assert.Equal(t, ownerTeamA, owner, "a stamp the manager did not report gone is never overwritten")

	_, err = p.TransferOwner(context.Background(), transferRequest(id, ownerTeamB.UID))
	requireCode(t, err, codes.AlreadyExists)

	_, err = p.TransferOwner(context.Background(), transferRequest(id, ownerTeamA.UID))
	require.NoError(t, err)
	owner, _, _, _ = vmState(t, p, id)
	assert.Equal(t, adopterTeamC, owner)
}

// TestTransferOwner_RefusesTemplatesGoneVMsAndBadRequests: a template is
// refused untouched, a VM that does not exist (or is not the instance UUID
// listed) is NotFound, and a request naming a host or without a complete
// owner is InvalidArgument before any vCenter call.
func TestTransferOwner_RefusesTemplatesGoneVMsAndBadRequests(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()

	_, err := p.TransferOwner(ctx, transferRequest(seededVMID(t, p, simTemplate)))
	requireCode(t, err, codes.AlreadyExists)

	_, err = p.TransferOwner(ctx, transferRequest("vm-999999"))
	requireCode(t, err, codes.NotFound)

	id := seededVMID(t, p, simUnstamped)
	wrongUUID := transferRequest(id)
	wrongUUID.ExpectedUuid = "00000000-0000-4000-8000-000000000000"
	_, err = p.TransferOwner(ctx, wrongUUID)
	requireCode(t, err, codes.NotFound)

	withHost := transferRequest(id)
	withHost.TargetHostId = "esx-1"
	_, err = p.TransferOwner(ctx, withHost)
	requireCode(t, err, codes.InvalidArgument)

	noOwner := transferRequest(id)
	noOwner.Owner = &providerv1.ObjectIdentity{Uid: adopterTeamC.UID}
	_, err = p.TransferOwner(ctx, noOwner)
	requireCode(t, err, codes.InvalidArgument)

	_, err = p.TransferOwner(ctx, transferRequest("DC0_H0_VM1"))
	requireCode(t, err, codes.InvalidArgument)

	owner, _, _, _ := vmState(t, p, id)
	assert.True(t, owner.IsZero(), "nothing was stamped")
}

// vcenterChangeVersionSemantics makes vcsim honour
// VirtualMachineConfigSpec.changeVersion as vCenter does: a ReconfigVM_Task
// whose spec carries a changeVersion other than the VM's current one fails
// (vCenter answers ConcurrentAccess) and changes nothing. vcsim (govmomi
// v0.52.0) ignores the field. When race is set, another writer re-stamps the
// VM with racer between the provider's read and its write.
type vcenterChangeVersionSemantics struct {
	soap.RoundTripper
	c     *vim25.Client
	race  atomic.Bool
	racer contracts.ObjectIdentity
}

func (r *vcenterChangeVersionSemantics) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	body, ok := req.(*methods.ReconfigVM_TaskBody)
	if !ok || body.Req == nil || body.Req.Spec.ChangeVersion == "" {
		return r.RoundTripper.RoundTrip(ctx, req, res)
	}
	if r.race.Swap(false) {
		task, err := object.NewVirtualMachine(r.c, body.Req.This).Reconfigure(ctx,
			types.VirtualMachineConfigSpec{ExtraConfig: ownerExtraConfig(r.racer)})
		if err != nil {
			return fmt.Errorf("fixture: racing reconfigure: %w", err)
		}
		if err := task.Wait(ctx); err != nil {
			return fmt.Errorf("fixture: racing reconfigure: %w", err)
		}
	}
	var vm mo.VirtualMachine
	if err := property.DefaultCollector(r.c).RetrieveOne(ctx, body.Req.This, []string{"config.changeVersion"}, &vm); err != nil {
		return err
	}
	if vm.Config == nil || vm.Config.ChangeVersion != body.Req.Spec.ChangeVersion {
		return fmt.Errorf("fixture: ConcurrentAccess: the VM's configuration changed since changeVersion %q was read",
			body.Req.Spec.ChangeVersion)
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// TestTransferOwner_ConcurrentReStampIsNeverOverwritten: the stamp is written
// with the changeVersion the decision was made on. A second adopter that
// stamps the VM in between wins; this transfer fails retryably
// (VM_OPERATION_FAILED, kept out of the circuit breaker), and the retry is
// then refused because the VM is now owned by someone else.
func TestTransferOwner_ConcurrentReStampIsNeverOverwritten(t *testing.T) {
	p, _ := newOwnershipSim(t)
	id := seededVMID(t, p, simUnstamped)
	sem := &vcenterChangeVersionSemantics{RoundTripper: p.client.RoundTripper, c: p.client.Client, racer: ownerTeamB}
	p.client.RoundTripper = sem
	sem.race.Store(true)

	_, err := p.TransferOwner(context.Background(), transferRequest(id))
	requireCode(t, err, codes.Unavailable)
	assert.Equal(t, []string{contracts.VMOperationFailedReason}, errorReasons(err))
	owner, _, _, _ := vmState(t, p, id)
	assert.Equal(t, ownerTeamB, owner, "the concurrent writer's stamp is kept")

	_, err = p.TransferOwner(context.Background(), transferRequest(id))
	requireCode(t, err, codes.AlreadyExists)
}

// TestGetCapabilities_AdvertisesOwnerTransfer: the vSphere provider reports
// supports_owner_transfer (and is not clustered).
func TestGetCapabilities_AdvertisesOwnerTransfer(t *testing.T) {
	caps, err := (&Provider{}).GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.True(t, caps.GetSupportsOwnerTransfer())
	assert.False(t, caps.GetSupportsRoutedAdoption())
}
