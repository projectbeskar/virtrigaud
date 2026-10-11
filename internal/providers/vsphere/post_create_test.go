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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Post-create steps (post_create.go): a failed disk grow or additional-disk
// attach fails the Create with a retryable error, and the retried Create —
// which binds the VM — completes the steps without repeating any.

// existingDiskCapacity reports, on CreateVM_Task, the capacity of an existing
// disk attached in place, as vCenter does from the file. vcsim does not read
// it and leaves the capacity the spec carries (none).
type existingDiskCapacity struct {
	soap.RoundTripper
	kb map[string]int64
}

func (r *existingDiskCapacity) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.CreateVM_TaskBody); ok && body.Req != nil {
		for _, dc := range body.Req.Config.DeviceChange {
			spec := dc.GetVirtualDeviceConfigSpec()
			disk, isDisk := spec.Device.(*types.VirtualDisk)
			if !isDisk || spec.FileOperation != "" {
				continue
			}
			if fb, ok := disk.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
				if kb, known := r.kb[fb.GetVirtualDeviceFileBackingInfo().FileName]; known {
					disk.CapacityInKB = kb
				}
			}
		}
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// withExistingDiskCapacity installs existingDiskCapacity on p.
func withExistingDiskCapacity(p *Provider, kb map[string]int64) {
	p.client.RoundTripper = &existingDiskCapacity{RoundTripper: p.client.RoundTripper, kb: kb}
}

// failReconfigure fails the next `remaining` ReconfigVM_Task calls that match.
type failReconfigure struct {
	soap.RoundTripper
	mu        sync.Mutex
	remaining int
	match     func(types.VirtualMachineConfigSpec) bool
}

func (r *failReconfigure) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.ReconfigVM_TaskBody); ok && body.Req != nil && r.match(body.Req.Spec) {
		r.mu.Lock()
		fail := r.remaining > 0
		if fail {
			r.remaining--
		}
		r.mu.Unlock()
		if fail {
			return fmt.Errorf("fixture: simulated vCenter fault on %q", "[secret-ds] x.vmdk")
		}
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// addsDisk matches a reconfigure that creates a disk file.
func addsDisk(spec types.VirtualMachineConfigSpec) bool {
	for _, dc := range spec.DeviceChange {
		s := dc.GetVirtualDeviceConfigSpec()
		if _, ok := s.Device.(*types.VirtualDisk); ok && s.Operation == types.VirtualDeviceConfigSpecOperationAdd &&
			s.FileOperation == types.VirtualDeviceConfigSpecFileOperationCreate {
			return true
		}
	}
	return false
}

// growsDisk matches a reconfigure that edits a disk (the grow).
func growsDisk(spec types.VirtualMachineConfigSpec) bool {
	for _, dc := range spec.DeviceChange {
		s := dc.GetVirtualDeviceConfigSpec()
		if _, ok := s.Device.(*types.VirtualDisk); ok && s.Operation == types.VirtualDeviceConfigSpecOperationEdit {
			return true
		}
	}
	return false
}

// vmDisks returns the backing file and capacity (KB) of every disk of VM id,
// in device order.
func vmDisks(t *testing.T, p *Provider, id string) (files []string, capacityKB []int64) {
	t.Helper()
	var vm mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"config.hardware.device"}, &vm))
	for _, dev := range vm.Config.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			name := ""
			if fb, ok := d.Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
				name = fb.GetVirtualDeviceFileBackingInfo().FileName
			}
			files = append(files, name)
			capacityKB = append(capacityKB, d.CapacityInKB)
		}
	}
	return files, capacityKB
}

// requireRetryablePostCreateFailure asserts err is the post-create step
// failure: Unavailable with VM_OPERATION_FAILED, no vCenter fault text.
func requireRetryablePostCreateFailure(t *testing.T, err error) {
	t.Helper()
	requireCode(t, err, codes.Unavailable)
	assert.Equal(t, []string{contracts.VMOperationFailedReason}, errorReasons(err))
	assert.NotContains(t, err.Error(), "secret-ds", "the vCenter fault stays in the provider log")
}

const twoDisksJSON = `[{"Name":"data","SizeGiB":1},{"Name":"logs","SizeGiB":2}]`

// TestCreate_AdditionalDiskFailureIsRetriedNotSwallowed: a failed attach fails
// the Create (retryable); the retried Create binds the VM and attaches only the
// disks still missing; a further retry adds nothing.
func TestCreate_AdditionalDiskFailureIsRetriedNotSwallowed(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	// Additional disk 1 attaches; the attach of disk 2 fails once.
	fault := &failReconfigure{RoundTripper: p.client.RoundTripper, remaining: 1, match: func(spec types.VirtualMachineConfigSpec) bool {
		return addsDisk(spec) && hasExtraConfigKey(spec.ExtraConfig, additionalDiskExtraConfigKey(2))
	}}
	p.client.RoundTripper = fault

	req := templateCreateRequest("web", "", wireOwner(ownerTeamA))
	req.DisksJson = twoDisksJSON

	_, err := p.Create(ctx, req)
	requireRetryablePostCreateFailure(t, err)
	refs, err := p.vmsNamedInFolder(ctx, defaultVMFolder(t, p), "web")
	require.NoError(t, err)
	require.Len(t, refs, 1, "the VM is kept for the retry")
	files, _ := vmDisks(t, p, refs[0].Value)
	require.Len(t, files, 2, "the template disk and additional disk 1")

	again, err := p.Create(ctx, req)
	require.NoError(t, err, "the retry binds the VM and completes the steps")
	assert.Equal(t, refs[0].Value, again.Id)
	files, capacityKB := vmDisks(t, p, again.Id)
	require.Len(t, files, 3, "additional disk 2 is attached, disk 1 is not added again")
	assert.Contains(t, files[1], additionalDiskFileName("web", 1))
	assert.Contains(t, files[2], additionalDiskFileName("web", 2))
	assert.Equal(t, []int64{1024 * 1024, 2 * 1024 * 1024}, capacityKB[1:])

	_, err = p.Create(ctx, req)
	require.NoError(t, err)
	files, _ = vmDisks(t, p, again.Id)
	assert.Len(t, files, 3, "a further retry adds nothing")
}

// TestCreate_DiskGrowFailureIsRetriedNotSwallowed: a failed grow fails the
// Create (retryable); the retried Create grows the disk.
func TestCreate_DiskGrowFailureIsRetriedNotSwallowed(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	imageJSON := makeOwnMigratedDisk(t, p, "migrated")
	withExistingDiskCapacity(p, map[string]int64{ownMigratedDiskPath("migrated"): 1024})
	fault := &failReconfigure{RoundTripper: p.client.RoundTripper, match: growsDisk, remaining: 1}
	p.client.RoundTripper = fault

	req := &providerv1.CreateRequest{
		Name:      "migrated",
		ClassJson: `{"CPU":1,"MemoryMiB":512,"DiskDefaults":{"SizeGiB":2}}`,
		ImageJson: imageJSON,
		Owner:     ownerNamed(ownerTeamA, "migrated"),
	}
	_, err := p.Create(ctx, req)
	requireRetryablePostCreateFailure(t, err)

	resp, err := p.Create(ctx, req)
	require.NoError(t, err, "the retry binds the VM (its own disk is not 'used by another VM') and grows the disk")
	_, capacityKB := vmDisks(t, p, resp.Id)
	assert.Equal(t, []int64{2 * 1024 * 1024}, capacityKB)
}

// TestCreate_BindLeavesPreMarkerVMAlone: a VM created by an earlier release
// (no post-create marker) is bound as-is: its disks were never recorded, so
// re-running the steps could duplicate them.
func TestCreate_BindLeavesPreMarkerVMAlone(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	first, err := p.Create(ctx, templateCreateRequest("web", "", wireOwner(ownerTeamA)))
	require.NoError(t, err)
	task, err := object.NewVirtualMachine(p.client.Client, vmRef(first.Id)).Reconfigure(ctx, types.VirtualMachineConfigSpec{
		ExtraConfig: []types.BaseOptionValue{&types.OptionValue{Key: postCreateMarkerKey, Value: ""}},
	})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
	_, _, _, extraConfig := vmState(t, p, first.Id)
	require.False(t, hasExtraConfigKey(extraConfig, postCreateMarkerKey), "setup: marker removed")

	req := templateCreateRequest("web", "", wireOwner(ownerTeamA))
	req.DisksJson = twoDisksJSON
	again, err := p.Create(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, first.Id, again.Id)
	files, _ := vmDisks(t, p, first.Id)
	assert.Len(t, files, 1, "no disk added to a pre-marker VM")
}

// TestCreate_InheritedDiskRecordsAreCleared: additional-disk records a
// template carries (e.g. made from a VirtRigaud VM) never make Create skip a
// disk.
func TestCreate_InheritedDiskRecordsAreCleared(t *testing.T) {
	p, _ := newOwnershipSim(t)
	ctx := context.Background()
	tmplID := seededVMID(t, p, simTemplate)
	tmpl := object.NewVirtualMachine(p.client.Client, vmRef(tmplID))
	cluster, err := p.finder.ClusterComputeResource(ctx, simCluster)
	require.NoError(t, err)
	pool, err := cluster.ResourcePool(ctx)
	require.NoError(t, err)
	require.NoError(t, tmpl.MarkAsVirtualMachine(ctx, *pool, nil), "setup: a template cannot be reconfigured")
	task, err := tmpl.Reconfigure(ctx, types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
		&types.OptionValue{Key: postCreateMarkerKey, Value: postCreateMarkerValue},
		&types.OptionValue{Key: additionalDiskExtraConfigKey(1), Value: "[" + simDatastore + "] other/other-additional-1.vmdk"},
	}})
	require.NoError(t, err)
	require.NoError(t, task.Wait(ctx))
	markAsTemplate(t, p, tmplID)

	req := templateCreateRequest("web", "", wireOwner(ownerTeamA))
	req.DisksJson = `[{"Name":"data","SizeGiB":1}]`
	resp, err := p.Create(ctx, req)
	require.NoError(t, err)
	files, _ := vmDisks(t, p, resp.Id)
	require.Len(t, files, 2)
	assert.Contains(t, files[1], additionalDiskFileName("web", 1))
	_, _, _, extraConfig := vmState(t, p, resp.Id)
	v, ok := extraConfigValue(extraConfig, additionalDiskExtraConfigKey(1))
	require.True(t, ok)
	assert.Equal(t, files[1], v, "the record names the VM's own disk")
}
