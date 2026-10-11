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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// vmSnapshotCount returns how many root snapshots VM id has, and whether its
// configuration is still readable (the object exists).
func vmSnapshotCount(t *testing.T, p *Provider, id string) (int, int32) {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"snapshot", "config.hardware.numCPU"}, &vm), "the object must still exist")
	require.NotNil(t, vm.Config)
	n := 0
	if vm.Snapshot != nil {
		n = len(vm.Snapshot.RootSnapshotList)
	}
	return n, vm.Config.Hardware.NumCPU
}

// TestPerVMOperations_RefuseImageArtifacts drives the real vSphere provider
// over gRPC with the manager's transport client. A VirtualMachine that
// addresses a template (adopted before the ListVMs fix) or a stamped image
// import in flight must never destroy or change it: every per-VM operation is
// refused, typed as a non-retryable Conflict marked VM_IS_IMAGE_ARTIFACT, and
// the object is left exactly as it was.
func TestPerVMOperations_RefuseImageArtifacts(t *testing.T) {
	p, _ := newOwnershipSim(t)
	c := serveOverGRPC(t, p)
	ctx := context.Background()
	stamp := imageStampExtraConfig(*stampAt(testDigestA, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))

	plainTemplate := seededVMID(t, p, simTemplate) // a template, no stamp (newOwnershipSim)

	// An import in flight: a plain VM carrying the image stamp. It gets a
	// snapshot first, so snapshot delete and revert can be attempted.
	importInFlight := seededVMID(t, p, simImportInFlight)
	snapTask, err := object.NewVirtualMachine(p.client.Client, vmRef(importInFlight)).
		CreateSnapshot(ctx, "pre", "", false, false)
	require.NoError(t, err)
	snapInfo, err := snapTask.WaitForResult(ctx, nil)
	require.NoError(t, err)
	snapRef, ok := snapInfo.Result.(types.ManagedObjectReference)
	require.True(t, ok, "fixture: CreateSnapshot returns the snapshot reference")
	snapshotID := snapRef.Value
	reconfigureExtraConfig(t, p, importInFlight, stamp...)

	requireRefused := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		assert.True(t, contracts.IsVMIsImageArtifact(err), "typed as an image-artifact refusal: %v", err)
		assert.True(t, contracts.IsConflict(err), "a non-retryable Conflict: %v", err)
		assert.False(t, contracts.IsRetryable(err))
		assert.False(t, contracts.IsNotFound(err), "never read as gone (that would release the finalizer)")
		assert.NotContains(t, err.Error(), testDigestA, "the message names no stamp value")
	}

	for name, id := range map[string]string{"template": plainTemplate, "image import in flight": importInFlight} {
		t.Run(name, func(t *testing.T) {
			ref := contracts.VMRef{ID: id}
			snapsBefore, cpuBefore := vmSnapshotCount(t, p, id)

			_, err := c.Delete(ctx, ref)
			requireRefused(t, err)
			_, err = c.Power(ctx, ref, contracts.PowerOpOn)
			requireRefused(t, err)
			_, err = c.Reconfigure(ctx, ref, contracts.CreateRequest{Class: contracts.VMClass{CPU: cpuBefore + 2}})
			requireRefused(t, err)
			_, err = c.SnapshotCreate(ctx, contracts.SnapshotCreateRequest{VM: ref, NameHint: "tenant"})
			requireRefused(t, err)

			snapsAfter, cpuAfter := vmSnapshotCount(t, p, id)
			assert.Equal(t, snapsBefore, snapsAfter, "no snapshot taken")
			assert.Equal(t, cpuBefore, cpuAfter, "not resized")
		})
	}

	t.Run("snapshot delete and revert on an image import in flight", func(t *testing.T) {
		ref := contracts.VMRef{ID: importInFlight}
		_, err := c.SnapshotRevert(ctx, ref, snapshotID)
		requireRefused(t, err)
		_, err = c.SnapshotDelete(ctx, ref, snapshotID)
		requireRefused(t, err)
		n, _ := vmSnapshotCount(t, p, importInFlight)
		assert.Equal(t, 1, n, "the snapshot is still there")
	})

	t.Run("hardware upgrade", func(t *testing.T) {
		_, err := p.HardwareUpgrade(ctx, &providerv1.HardwareUpgradeRequest{Id: plainTemplate, TargetVersion: 21})
		requireArtifactStatus(t, err)
	})

	t.Run("an ordinary VM is still deleted and resized", func(t *testing.T) {
		owned, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
		require.NoError(t, err)
		ref := contracts.VMRef{ID: owned.Id}
		_, cpu := vmSnapshotCount(t, p, owned.Id)
		_, err = c.Reconfigure(ctx, ref, contracts.CreateRequest{Class: contracts.VMClass{CPU: cpu + 1}})
		require.NoError(t, err)
		_, cpuAfter := vmSnapshotCount(t, p, owned.Id)
		assert.Equal(t, cpu+1, cpuAfter)
		_, err = c.Delete(ctx, ref)
		require.NoError(t, err)
		var vm mo.VirtualMachine
		assert.Error(t, property.DefaultCollector(p.client.Client).RetrieveOne(ctx, vmRef(owned.Id), []string{"name"}, &vm),
			"the ordinary VM is gone")
	})
}

// requireArtifactStatus asserts the provider's wire form of the refusal:
// FailedPrecondition (never counted toward the manager's circuit breaker) with
// the VM_IS_IMAGE_ARTIFACT ErrorInfo and the constant message.
func requireArtifactStatus(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Equal(t, imageArtifactRefusalMessage, st.Message())
	var reasons []string
	for _, d := range st.Details() {
		if info, isInfo := d.(*errdetails.ErrorInfo); isInfo && info.GetDomain() == contracts.ErrorInfoDomain {
			reasons = append(reasons, info.GetReason())
		}
	}
	assert.Equal(t, []string{contracts.VMIsImageArtifactReason}, reasons)
}

func TestImageArtifactRefusal(t *testing.T) {
	opt := func(key string, value any) types.BaseOptionValue { return &types.OptionValue{Key: key, Value: value} }
	stamp := imageStampExtraConfig(*stampAt(testDigestA, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))

	for name, cfg := range map[string]*types.VirtualMachineConfigInfo{
		"template":                  {Template: true},
		"stamped template":          {Template: true, ExtraConfig: stamp},
		"stamped VM":                {ExtraConfig: stamp},
		"one stamp key, upper case": {ExtraConfig: []types.BaseOptionValue{opt("VirtRigaud.Image.SourceDigest", testDigestA)}},
		"non-string stamp value":    {ExtraConfig: []types.BaseOptionValue{opt(imageStampKeyUID, int32(1))}},
	} {
		t.Run(name, func(t *testing.T) { requireArtifactStatus(t, imageArtifactRefusal(cfg)) })
	}

	for name, cfg := range map[string]*types.VirtualMachineConfigInfo{
		"plain VM":                     {},
		"owner-stamped VM":             {ExtraConfig: ownerExtraConfig(ownerTeamA)},
		"cleared stamp (empty values)": {ExtraConfig: clearedImageStampExtraConfig()},
	} {
		t.Run(name, func(t *testing.T) { assert.NoError(t, imageArtifactRefusal(cfg)) })
	}

	t.Run("configuration unavailable fails closed", func(t *testing.T) {
		err := imageArtifactRefusal(nil)
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		st, _ := status.FromError(err)
		assert.Empty(t, st.Details(), "not marked as an image artifact: it may not be one")
	})
}
