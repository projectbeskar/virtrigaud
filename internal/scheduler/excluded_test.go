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

// These tests pin Request.ExcludedHosts (ADR-0007 Addendum A, A2 amendment):
// a host the caller excluded for this VM is never chosen, whatever its fit, and
// a no-fit in which every candidate was excluded is recognisable (AllExcluded).

func TestScheduleExcludedHostIsNeverChosen(t *testing.T) {
	// host-big would win Spread by a wide margin; it is excluded.
	req := baseReq(newHost("host-big", 64, 1<<20), newHost("host-small", 4, 8192))
	req.ExcludedHosts = []string{"host-big"}
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-small", res.HostID)
}

func TestScheduleExcludedHostBeatsTheCurrentBinding(t *testing.T) {
	req := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 8, 16384))
	req.CurrentBinding = "host-a"
	req.ExcludedHosts = []string{"host-a"}
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-b", res.HostID, "an excluded host is not re-selected even as the current binding")
}

func TestScheduleAllCandidatesExcluded(t *testing.T) {
	req := baseReq(
		newHost("host-a", 8, 16384),
		newHost("host-b", 8, 16384, hHealth(v1beta1.HostHealthNotReady)), // excluded wins over any other reason
	)
	req.ExcludedHosts = []string{"host-a", "host-b", "host-gone"}
	_, err := Schedule(req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoFeasibleHost))
	assert.True(t, AllExcluded(err))

	var nfe *NoFeasibleHostError
	require.True(t, errors.As(err, &nfe))
	assert.Equal(t, 2, nfe.Tally()[RejectionExcludedForVM])
	assert.Contains(t, err.Error(), RejectionExcludedForVM)
}

func TestAllExcluded_OnlyWhenEveryCandidateIsExcluded(t *testing.T) {
	mixed := baseReq(newHost("host-a", 8, 16384), newHost("host-b", 1, 16384)) // host-b: insufficient CPU
	mixed.ExcludedHosts = []string{"host-a"}
	_, err := Schedule(mixed)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoFeasibleHost))
	assert.False(t, AllExcluded(err), "a candidate rejected for capacity may become feasible; not all-excluded")

	_, err = Schedule(baseReq())
	assert.False(t, AllExcluded(err), "an empty pool is not all-excluded")
	assert.False(t, AllExcluded(nil))
	assert.False(t, AllExcluded(errors.New("boom")))
}
