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
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// vcsim (simulator.VPX) seeded VMs the listing tests turn into prepared images.
const (
	simStampedTemplate = "DC0_C0_RP0_VM0" // stamped, then marked as a template
	simImportInFlight  = "DC0_C0_RP0_VM1" // stamped, NOT a template (import in flight)
)

// reconfigureExtraConfig sets extraConfig on VM id.
func reconfigureExtraConfig(t *testing.T, p *Provider, id string, extraConfig ...types.BaseOptionValue) {
	t.Helper()
	ctx := context.Background()
	task, err := object.NewVirtualMachine(p.client.Client, vmRef(id)).Reconfigure(ctx,
		types.VirtualMachineConfigSpec{ExtraConfig: extraConfig})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
}

// TestListVMs_OnlyAdoptableVMs builds every kind of object a vCenter holds and
// checks what reaches the manager over the wire — the VMAdoption discovery
// feed. Templates (stamped or not), image imports in flight and objects with
// an image stamp or an unreadable owner stamp are never offered for adoption;
// ordinary VMs are, and a VirtRigaud-created VM carries its owner's UID so the
// adoption controller skips it while that VirtualMachine exists.
func TestListVMs_OnlyAdoptableVMs(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	stamp := imageStampExtraConfig(*stampAt(testDigestA, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))

	plainTemplate := seededVMID(t, p, simTemplate) // marked as a template by newOwnershipSim, no stamp
	ordinary := seededVMID(t, p, simUnstamped)

	stampedTemplate := seededVMID(t, p, simStampedTemplate)
	reconfigureExtraConfig(t, p, stampedTemplate, stamp...)
	markAsTemplate(t, p, stampedTemplate)

	importInFlight := seededVMID(t, p, simImportInFlight)
	reconfigureExtraConfig(t, p, importInFlight, stamp...)

	owned, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)

	// An image-stamp key VirtRigaud would not accept as a stamp still counts.
	badImage, err := p.Create(ctx, templateCreateRequest("bad-image", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	reconfigureExtraConfig(t, p, badImage.Id, &types.OptionValue{Key: imageStampKeyUID, Value: "not-a-uid"})

	// A second owner UID key differing only in case: the owner stamp is
	// ambiguous (VMX keys are case-insensitive) and cannot be read.
	badOwner, err := p.Create(ctx, templateCreateRequest("bad-owner", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	reconfigureExtraConfig(t, p, badOwner.Id, &types.OptionValue{Key: "VIRTRIGAUD.OWNER.UID", Value: ownerTeamB.UID})
	var badOwnerMo mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(ctx, vmRef(badOwner.Id),
		[]string{propConfigExtraConfig}, &badOwnerMo))
	require.NotNil(t, badOwnerMo.Config)
	_, ownerErr := ownerFromExtraConfig(badOwnerMo.Config.ExtraConfig)
	require.Error(t, ownerErr, "fixture: the owner stamp must be unreadable")

	listed, err := serveOverGRPC(t, p).ListVMs(ctx)
	require.NoError(t, err)
	byID := make(map[string]contracts.VMInfo, len(listed.VMs))
	for _, vm := range listed.VMs {
		byID[vm.ID] = vm
	}

	for name, id := range map[string]string{
		"unstamped template":             plainTemplate,
		"stamped prepared template":      stampedTemplate,
		"stamped image import in flight": importInFlight,
		"invalid image stamp key":        badImage.Id,
		"unreadable owner stamp":         badOwner.Id,
	} {
		assert.NotContains(t, byID, id, "%s must never be offered for adoption", name)
	}

	require.Contains(t, byID, ordinary, "an ordinary VM is still listed for adoption")
	assert.NotContains(t, byID[ordinary].ProviderRaw, contracts.VMInfoOwnerUIDKey, "an unstamped VM reports no owner")

	require.Contains(t, byID, owned.Id, "a VirtRigaud-created VM is listed")
	assert.Equal(t, ownerTeamA.UID, byID[owned.Id].ProviderRaw[contracts.VMInfoOwnerUIDKey],
		"its owner UID is reported, so adoption skips it while that VirtualMachine exists")
}

func TestListExclusionFor(t *testing.T) {
	opt := func(key string, value any) types.BaseOptionValue { return &types.OptionValue{Key: key, Value: value} }
	stamp := imageStampExtraConfig(*stampAt(testDigestA, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))

	cases := map[string]struct {
		cfg  *types.VirtualMachineConfigInfo
		want listExclusion
	}{
		"no config": {nil, excludedNoConfig},
		"plain VM":  {&types.VirtualMachineConfigInfo{}, listedVM},
		"plain VM with unrelated keys": {&types.VirtualMachineConfigInfo{ExtraConfig: []types.BaseOptionValue{
			opt("guestinfo.userdata", "x"), opt("virtrigaud.imagex", "not the image namespace")}}, listedVM},
		"template":         {&types.VirtualMachineConfigInfo{Template: true}, excludedTemplate},
		"stamped template": {&types.VirtualMachineConfigInfo{Template: true, ExtraConfig: stamp}, excludedTemplate},
		"stamped VM":       {&types.VirtualMachineConfigInfo{ExtraConfig: stamp}, excludedImageStamp},
		"one stamp key, upper case": {&types.VirtualMachineConfigInfo{ExtraConfig: []types.BaseOptionValue{
			opt("VirtRigaud.Image.SourceDigest", testDigestA)}}, excludedImageStamp},
		"non-string stamp value": {&types.VirtualMachineConfigInfo{ExtraConfig: []types.BaseOptionValue{
			opt(imageStampKeyUID, int32(1))}}, excludedImageStamp},
		"cleared stamp (empty values)": {&types.VirtualMachineConfigInfo{ExtraConfig: clearedImageStampExtraConfig()}, listedVM},
		"owner-stamped VM":             {&types.VirtualMachineConfigInfo{ExtraConfig: ownerExtraConfig(ownerTeamA)}, listedVM},
		"repeated owner key": {&types.VirtualMachineConfigInfo{ExtraConfig: []types.BaseOptionValue{
			opt(ownerExtraConfigKeyUID, ownerTeamA.UID), opt("Virtrigaud.Owner.UID", ownerTeamB.UID)}}, excludedUnreadableOwnerStamp},
		"non-string owner value": {&types.VirtualMachineConfigInfo{ExtraConfig: []types.BaseOptionValue{
			opt(ownerExtraConfigKeyUID, int32(1))}}, excludedUnreadableOwnerStamp},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, listExclusionFor(tc.cfg))
		})
	}
}

// cancelAfterFirstVMRead wraps the vim25 round-tripper: it counts ListVMs'
// per-VM property reads (the ones asking for summary.config.name) and cancels
// the call's context once the first one has been answered.
type cancelAfterFirstVMRead struct {
	soap.RoundTripper
	cancel  context.CancelFunc
	vmReads atomic.Int32
}

func (r *cancelAfterFirstVMRead) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	perVM := false
	if body, ok := req.(*methods.RetrievePropertiesExBody); ok && body.Req != nil {
		for _, spec := range body.Req.SpecSet {
			for _, ps := range spec.PropSet {
				perVM = perVM || slices.Contains(ps.PathSet, "summary.config.name")
			}
		}
	}
	if perVM {
		r.vmReads.Add(1)
	}
	err := r.RoundTripper.RoundTrip(ctx, req, res)
	if perVM {
		r.cancel()
	}
	return err
}

// TestListVMs_StopsWhenCancelled: once the call is cancelled, ListVMs reads no
// further VM and answers Canceled instead of a partial listing (or a warning
// per remaining VM).
func TestListVMs_StopsWhenCancelled(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &cancelAfterFirstVMRead{RoundTripper: p.client.RoundTripper, cancel: cancel}
	p.client.RoundTripper = rt

	resp, err := p.ListVMs(ctx, &providerv1.ListVMsRequest{})
	require.Error(t, err)
	assert.Nil(t, resp, "a partial listing is never answered")
	assert.Equal(t, codes.Canceled, status.Code(err))
	assert.EqualValues(t, 1, rt.vmReads.Load(), "no VM is read after the cancellation")
}
