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

package scheduler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// resizeReq: a VM "self" holding 2 vCPU / 2048 MiB on an 8 vCPU / 8192 MiB
// host, next to "other" holding 4 vCPU / 4096 MiB.
func resizeReq(desired ResourceRequest) ResizeRequest {
	return ResizeRequest{
		Host:    newHost("host-a", 8, 8192),
		Pool:    v1beta1.HostPoolSpec{},
		VMUID:   "self",
		Current: ResourceRequest{CPU: 2, MemoryMiB: 2048},
		Desired: desired,
		PlacedVMs: []PlacedVM{
			holding("self", "host-a", 2, 2048),
			holding("other", "host-a", 4, 4096),
		},
	}
}

func TestCheckResize(t *testing.T) {
	// Free for self: 8 - 4 = 4 vCPU, 8192 - 4096 = 4096 MiB (its own 2/2048 excluded).
	assert.NoError(t, CheckResize(resizeReq(ResourceRequest{CPU: 4, MemoryMiB: 4096})), "grows into exactly what is free")

	err := CheckResize(resizeReq(ResourceRequest{CPU: 5, MemoryMiB: 2048}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrResizeDoesNotFit))
	var rf *ResizeDoesNotFitError
	require.True(t, errors.As(err, &rf))
	assert.Equal(t, "resizing to 5 vCPU and 2048 MiB exceeds the free capacity of its host host-a", err.Error())
	assert.Equal(t, []string{rejInsufficientCPU + ": requested 5, free 4 (committed 4 of 8)"}, rf.Detail())

	err = CheckResize(resizeReq(ResourceRequest{CPU: 2, MemoryMiB: 8192}))
	require.ErrorAs(t, err, &rf)
	assert.Equal(t, rejInsufficientMem, rf.Shortfalls[0].Reason)
}

func TestCheckResize_ShrinkIsAlwaysAllowed(t *testing.T) {
	req := resizeReq(ResourceRequest{CPU: 1, MemoryMiB: 1024})
	req.Pool.Overcommit = oc("0.1", "0.1") // the host is now far over-committed
	assert.NoError(t, CheckResize(req))
	assert.False(t, Grows(req.Current, req.Desired))
}

func TestCheckResize_OnlyGrowingResourcesAreChecked(t *testing.T) {
	// Memory over-committed (ratio 0.5: 4096 bookable, 4096 used by other) but
	// only CPU grows: allowed.
	req := resizeReq(ResourceRequest{CPU: 4, MemoryMiB: 1024})
	req.Pool.Overcommit = oc("", "0.5")
	assert.NoError(t, CheckResize(req))
}

func TestCheckResize_TheVMsOwnEntriesNeverCount(t *testing.T) {
	// An earlier resize assumption of self (6 vCPU) is ignored like its record.
	req := resizeReq(ResourceRequest{CPU: 4, MemoryMiB: 2048})
	req.PlacedVMs = append(req.PlacedVMs, holding("self", "host-a", 6, 2048))
	assert.NoError(t, CheckResize(req))
}

func TestPlacedEntriesMergeAtTheLargerSize(t *testing.T) {
	// "other" is recorded at 2 vCPU and has an admitted resize to 4 not yet
	// applied: it counts at 4, once.
	req := baseReq(newHost("host-a", 8, 65536))
	req.Resources.CPU = 5
	req.PlacedVMs = []PlacedVM{holding("other", "host-a", 2, 1024), holding("other", "host-a", 4, 512)}
	_, err := Schedule(req)
	nf := requireNoFit(t, err)
	assert.Equal(t, CapacityShortfall{Requested: 5, Capacity: 8, Committed: 4}, *nf.Rejections[0].Shortfall)

	req.Resources.CPU = 4
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID)
}
