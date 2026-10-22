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
)

// These tests pin Request.RequiredStorageBytes (ADR-0007 Addendum B, B7.4): a
// migration target is placed only on a host whose reported allocatable
// storage can hold the source disk.

func TestScheduleRequiredStorageFiltersSmallAndUnreportedHosts(t *testing.T) {
	const gib = int64(1 << 30)
	bigBytes, smallBytes := 100*gib, 10*gib
	big := newHost("host-big", 8, 16384)
	big.Status.AllocatableStorageBytes = &bigBytes
	small := newHost("host-small", 64, 1<<20) // would win Spread, but cannot hold the disk
	small.Status.AllocatableStorageBytes = &smallBytes
	unreported := newHost("host-unknown", 64, 1<<20)
	req := baseReq(big, small, unreported)
	req.RequiredStorageBytes = 20 * gib
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-big", res.HostID, "only the host that can hold the disk is feasible")

	req.RequiredStorageBytes = 200 * gib
	_, err = Schedule(req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoFeasibleHost))
	var nfe *NoFeasibleHostError
	require.True(t, errors.As(err, &nfe))
	assert.Equal(t, 3, nfe.Tally()[rejInsufficientStorage], "the small, the big and the unreported host")
}

func TestScheduleNoRequiredStorageIgnoresStorage(t *testing.T) {
	req := baseReq(newHost("host-unknown", 8, 16384))
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-unknown", res.HostID, "without a disk-space need, an unreported storage figure filters nothing")
}

// TestScheduleRequiredHost pins Request.RequiredHost (ADR-0007 Addendum B): a
// VM whose disk was imported to one host is placed only there.
func TestScheduleRequiredHost(t *testing.T) {
	req := baseReq(newHost("host-big", 64, 1<<20), newHost("host-a", 4, 8192))
	req.RequiredHost = "host-a"
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-a", res.HostID)

	req.ExcludedHosts = []string{"host-a"}
	_, err = Schedule(req)
	require.Error(t, err)
	var nfe *NoFeasibleHostError
	require.True(t, errors.As(err, &nfe))
	assert.Equal(t, 1, nfe.Tally()[RejectionNotRequiredHost])
	assert.False(t, AllExcluded(err))
}
