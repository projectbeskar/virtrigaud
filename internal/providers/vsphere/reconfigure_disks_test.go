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
	"encoding/json"
	"fmt"
	"strings"
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

// Reconfigure sends each spec.disks size to the additional disk Create
// attached for that entry, and never touches the root disk (reconfigure.go).
// Before this fix every Reconfigure grew the FIRST disk — the root disk — to
// spec.disks[0].sizeGiB.

const kibGiB = kibPerGiB // KiB per GiB, for brevity

// multiDiskVM creates "web" from the template with the additional disks
// data (1 GiB) and logs (2 GiB), and returns its id and its three disks'
// capacities in KiB (root, data, logs).
func multiDiskVM(t *testing.T) (*Provider, string, []int64) {
	t.Helper()
	p, _ := newOwnershipSim(t)
	req := templateCreateRequest("web", "", wireOwner(ownerTeamA))
	req.DisksJson = twoDisksJSON
	resp, err := p.Create(context.Background(), req)
	require.NoError(t, err)
	files, capacityKB := vmDisks(t, p, resp.Id)
	require.Len(t, files, 3)
	require.Contains(t, files[1], additionalDiskFileName("web", 1))
	require.Contains(t, files[2], additionalDiskFileName("web", 2))
	require.Equal(t, []int64{1 * kibGiB, 2 * kibGiB}, capacityKB[1:])
	return p, resp.Id, capacityKB
}

// reconfigure sends desired to VM id.
func reconfigure(t *testing.T, p *Provider, id string, desired contracts.CreateRequest) error {
	t.Helper()
	body, err := json.Marshal(desired)
	require.NoError(t, err)
	_, err = p.Reconfigure(context.Background(), &providerv1.ReconfigureRequest{Id: id, DesiredJson: string(body)})
	return err
}

// capacities returns the KiB capacities of the disks of multiDiskVM's VM id,
// identified by their files: root, data (additional disk 1), logs (additional
// disk 2). vcsim moves an edited device to the end of the device list, so the
// list order is not the disks' identity.
func capacities(t *testing.T, p *Provider, id string) []int64 {
	t.Helper()
	files, kb := vmDisks(t, p, id)
	require.Len(t, files, 3, "no disk is added or removed")
	out := make([]int64, 3)
	for i, f := range files {
		switch {
		case strings.HasSuffix(f, additionalDiskFileName("web", 1)):
			out[1] = kb[i]
		case strings.HasSuffix(f, additionalDiskFileName("web", 2)):
			out[2] = kb[i]
		default:
			out[0] = kb[i]
		}
	}
	return out
}

// TestReconfigure_SpecDisksGrowTheirOwnDisk is the regression test for the
// wrong-disk resize: spec.disks sizes grow the additional disks Create made
// for them and never the root disk; the VMClass size is not applied by
// Reconfigure at all (Create applies it).
func TestReconfigure_SpecDisksGrowTheirOwnDisk(t *testing.T) {
	p, id, before := multiDiskVM(t)
	rootKB := before[0]

	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}, {Name: "logs", SizeGiB: 4}},
	}))
	assert.Equal(t, []int64{rootKB, 3 * kibGiB, 4 * kibGiB}, capacities(t, p, id),
		"each spec.disks size goes to its own disk; the root disk is untouched")

	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{DiskDefaults: &contracts.DiskDefaults{SizeGiB: int32(rootKB/kibGiB) + 5}}, // #nosec G115 -- a simulator disk size
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}, {Name: "logs", SizeGiB: 4}},
	}))
	assert.Equal(t, []int64{rootKB, 3 * kibGiB, 4 * kibGiB}, capacities(t, p, id),
		"the VMClass size never grows the root disk on Reconfigure")
}

// TestReconfigure_SpecDisksMatchedByName: a disk is found by the spec.disks
// name recorded when it was attached, so reordering or removing entries never
// sends a size to another entry's disk.
func TestReconfigure_SpecDisksMatchedByName(t *testing.T) {
	p, id, before := multiDiskVM(t)
	_, _, _, extraConfig := vmState(t, p, id)
	name, ok := extraConfigValue(extraConfig, additionalDiskNameKey(1))
	require.True(t, ok)
	assert.Equal(t, "data", name, "the name is recorded with the disk")

	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Disks: []contracts.DiskSpec{{Name: "logs", SizeGiB: 5}, {Name: "data", SizeGiB: 6}},
	}))
	assert.Equal(t, []int64{before[0], 6 * kibGiB, 5 * kibGiB}, capacities(t, p, id), "reordered: each name keeps its disk")

	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Disks: []contracts.DiskSpec{{Name: "logs", SizeGiB: 8}},
	}))
	assert.Equal(t, []int64{before[0], 6 * kibGiB, 8 * kibGiB}, capacities(t, p, id), "removed entry: the other keeps its disk")
}

// TestReconfigure_NamelessRecordsAreLeftAlone: a VM whose additional disks
// were recorded without a name (created by an earlier release) keeps them as
// they are: the spec.disks list may have been edited since, so position
// proves nothing. (main never resized an additional disk either.)
func TestReconfigure_NamelessRecordsAreLeftAlone(t *testing.T) {
	p, id, before := multiDiskVM(t)
	vm := object.NewVirtualMachine(p.client.Client, vmRef(id))
	task, err := vm.Reconfigure(context.Background(), types.VirtualMachineConfigSpec{ExtraConfig: []types.BaseOptionValue{
		&types.OptionValue{Key: additionalDiskNameKey(1), Value: ""},
		&types.OptionValue{Key: additionalDiskNameKey(2), Value: ""},
	}})
	require.NoError(t, err)
	require.NoError(t, task.Wait(context.Background()))

	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 3},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}, {Name: "logs", SizeGiB: 4}},
	}))
	assert.Equal(t, before, capacities(t, p, id))
	var m mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"config.hardware.numCPU"}, &m))
	assert.Equal(t, int32(3), m.Config.Hardware.NumCPU, "the CPU change is applied")
}

// TestReconfigure_RepeatedSpecDiskNameIsLeftAlone: a name used by two
// spec.disks entries is ambiguous: neither size goes to its disk.
func TestReconfigure_RepeatedSpecDiskNameIsLeftAlone(t *testing.T) {
	p, id, before := multiDiskVM(t)
	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}, {Name: "data", SizeGiB: 7}, {Name: "logs", SizeGiB: 4}},
	}))
	assert.Equal(t, []int64{before[0], before[1], 4 * kibGiB}, capacities(t, p, id))
}

// TestReconfigure_UnattachedSpecDiskIsLeftAlone: a spec.disks entry no disk
// was attached for (added after the VM was created) is not added, and no
// other disk takes its size; the rest of the change is applied.
func TestReconfigure_UnattachedSpecDiskIsLeftAlone(t *testing.T) {
	p, id, before := multiDiskVM(t)
	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 3},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 1}, {Name: "logs", SizeGiB: 2}, {Name: "extra", SizeGiB: 50}},
	}))
	assert.Equal(t, before, capacities(t, p, id), "no disk is added or grown for the unattached entry")
	var m mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"config.hardware.numCPU"}, &m))
	assert.Equal(t, int32(3), m.Config.Hardware.NumCPU)
}

// TestReconfigure_SpecDiskShrinkIsRefused: a spec.disks size below its disk's
// is refused with InvalidArgument before anything — the CPU change in the
// same request included — is changed; a withheld size (0) leaves the disk.
func TestReconfigure_SpecDiskShrinkIsRefused(t *testing.T) {
	p, id, before := multiDiskVM(t)
	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 4}},
	}))

	err := reconfigure(t, p, id, contracts.CreateRequest{
		Class: contracts.VMClass{CPU: 3},
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 2}, {Name: "logs", SizeGiB: 9}},
	})
	requireCode(t, err, codes.InvalidArgument)
	assert.Contains(t, err.Error(), "spec.disks[data] asks for 2 GiB but the disk already has 4 GiB")
	assert.Equal(t, []int64{before[0], 4 * kibGiB, 2 * kibGiB}, capacities(t, p, id), "nothing was changed")
	var m mo.VirtualMachine
	require.NoError(t, property.DefaultCollector(p.client.Client).RetrieveOne(context.Background(), vmRef(id),
		[]string{"config.hardware.numCPU"}, &m))
	assert.NotEqual(t, int32(3), m.Config.Hardware.NumCPU, "the CPU change was not applied either")

	require.NoError(t, reconfigure(t, p, id, contracts.CreateRequest{
		Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 0}, {Name: "logs", SizeGiB: 0}},
	}))
	assert.Equal(t, []int64{before[0], 4 * kibGiB, 2 * kibGiB}, capacities(t, p, id), "a withheld size never resizes a disk")
}

// TestReconfigure_TaskFaultStaysOutOfTheBreaker: a ReconfigVM_Task vCenter
// fails is answered with VM_OPERATION_FAILED and a constant message (the
// fault, which can name datastores, stays in the provider log), so the
// manager never counts one VM's failing reconfigure toward the Provider's
// circuit breaker.
func TestReconfigure_TaskFaultStaysOutOfTheBreaker(t *testing.T) {
	p, id, _ := multiDiskVM(t)
	p.client.RoundTripper = &taskFault{RoundTripper: p.client.RoundTripper}
	err := reconfigure(t, p, id, contracts.CreateRequest{Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}}})
	requireCode(t, err, codes.Unknown)
	assert.Equal(t, []string{contracts.VMOperationFailedReason}, errorReasons(err))
	assert.Contains(t, err.Error(), reconfigureTaskFailedMessage)
	assert.NotContains(t, err.Error(), "secret-ds")
}

// startFault answers the next ReconfigVM_Task call itself with err, as
// vCenter (a fault) or the transport would before any task exists.
type startFault struct {
	soap.RoundTripper
	err error
}

func (r *startFault) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if _, ok := req.(*methods.ReconfigVM_TaskBody); ok {
		return r.err
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// TestReconfigure_StartFaultStaysOutOfTheBreaker: vCenter refusing to start
// the task (TaskInProgress during a backup snapshot, InvalidState,
// InvalidPowerState) is an answer about this VM: VM_OPERATION_FAILED with
// the constant message, kept out of the breaker. A transport failure, and a
// NotAuthenticated session fault, still count (plain error, no reason).
func TestReconfigure_StartFaultStaysOutOfTheBreaker(t *testing.T) {
	p, id, _ := multiDiskVM(t)
	inner := p.client.RoundTripper
	desired := contracts.CreateRequest{Disks: []contracts.DiskSpec{{Name: "data", SizeGiB: 3}}}

	for name, f := range map[string]types.BaseMethodFault{
		"TaskInProgress":    &types.TaskInProgress{},
		"InvalidState":      &types.InvalidState{},
		"InvalidPowerState": &types.InvalidPowerState{},
	} {
		t.Run(name, func(t *testing.T) {
			p.client.RoundTripper = &startFault{RoundTripper: inner, err: soap.WrapVimFault(f)}
			err := reconfigure(t, p, id, desired)
			requireCode(t, err, codes.Unknown)
			assert.Equal(t, []string{contracts.VMOperationFailedReason}, errorReasons(err))
			assert.Contains(t, err.Error(), reconfigureTaskFailedMessage)
		})
	}
	t.Run("SOAP fault", func(t *testing.T) {
		sf := &soap.Fault{String: "fixture"}
		sf.Detail.Fault = &types.TaskInProgress{}
		p.client.RoundTripper = &startFault{RoundTripper: inner, err: soap.WrapSoapFault(sf)}
		err := reconfigure(t, p, id, desired)
		assert.Equal(t, []string{contracts.VMOperationFailedReason}, errorReasons(err))
	})

	for name, cause := range map[string]error{
		"transport":        fmt.Errorf("fixture: connection reset to vcenter"),
		"NotAuthenticated": soap.WrapVimFault(&types.NotAuthenticated{}),
	} {
		t.Run(name, func(t *testing.T) {
			p.client.RoundTripper = &startFault{RoundTripper: inner, err: cause}
			err := reconfigure(t, p, id, desired)
			require.Error(t, err)
			assert.Empty(t, errorReasons(err), "not a VM answer: it still counts toward the breaker")
		})
	}
}

// taskFault makes vcsim fail every ReconfigVM_Task that edits a disk AS A
// TASK (not as a SOAP fault): the edited device key is replaced by one the VM
// does not have, which vCenter answers with InvalidDeviceSpec.
type taskFault struct {
	soap.RoundTripper
}

func (r *taskFault) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.ReconfigVM_TaskBody); ok && body.Req != nil {
		for _, dc := range body.Req.Spec.DeviceChange {
			if s := dc.GetVirtualDeviceConfigSpec(); s.Operation == types.VirtualDeviceConfigSpecOperationEdit {
				s.Device.GetVirtualDevice().Key = 99999
			}
		}
	}
	return r.RoundTripper.RoundTrip(ctx, req, res)
}

// TestMatchSpecDisks pins the matching rules on records alone.
func TestMatchSpecDisks(t *testing.T) {
	records := []additionalDiskRecord{
		{index: 1, file: "a", name: "data"},
		{index: 2, file: "b"},
		{index: 3, file: "c", name: "logs"},
		{index: 4, file: "d", name: "logs"},
	}
	got := matchSpecDisks([]string{"data", "x", "logs", "y", "y", ""}, records)
	require.Len(t, got, 6)
	require.NotNil(t, got[0].rec)
	assert.Equal(t, 1, got[0].rec.index)
	assert.Nil(t, got[1].rec, "an unnamed record is never matched by position")
	assert.Equal(t, specDiskUnrecorded, got[1].why)
	assert.Nil(t, got[2].rec)
	assert.Equal(t, specDiskNameAmbiguous, got[2].why, "two disks recorded under one name")
	assert.Equal(t, specDiskNameRepeated, got[3].why, "one name used by two entries")
	assert.Equal(t, specDiskNameRepeated, got[4].why)
	assert.Nil(t, got[5].rec)
}

// TestAdditionalDiskRecords reads records case-insensitively and drops
// cleared ones.
func TestAdditionalDiskRecords(t *testing.T) {
	recs := additionalDiskRecords([]types.BaseOptionValue{
		&types.OptionValue{Key: "VirtRigaud.Disk.Additional.2", Value: "[ds] w/w-additional-2.vmdk"},
		&types.OptionValue{Key: "virtrigaud.disk.additional.2.name", Value: "logs"},
		&types.OptionValue{Key: "virtrigaud.disk.additional.1", Value: "[ds] w/w-additional-1.vmdk"},
		&types.OptionValue{Key: "virtrigaud.disk.additional.3", Value: ""},
		&types.OptionValue{Key: "virtrigaud.disk.additional.3.name", Value: "gone"},
		&types.OptionValue{Key: "virtrigaud.disk.additional.x", Value: "junk"},
	})
	assert.Equal(t, []additionalDiskRecord{
		{index: 1, file: "[ds] w/w-additional-1.vmdk"},
		{index: 2, file: "[ds] w/w-additional-2.vmdk", name: "logs"},
	}, recs)
}
