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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These tests pin Request.HeartbeatMaxAge: for the hosts of a Provider whose
// type runs a per-host agent, a Ready host whose last successful sync is
// absent or older than the maximum age is rejected (a stalled writer cannot
// keep a Ready health alive); other Providers' requests check nothing.

func withHeartbeat(at time.Time) hostOpt {
	return func(h *v1beta1.Host) {
		t := metav1.NewTime(at)
		h.Status.LastHeartbeatTime = &t
	}
}

func TestScheduleHeartbeatStaleIsRejected(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	req := baseReq(
		newHost("host-big", 64, 1<<20, withHeartbeat(now.Add(-301*time.Second))), // would win, but stale
		newHost("host-small", 4, 8192, withHeartbeat(now.Add(-299*time.Second))),
		newHost("host-never", 64, 1<<20), // never synced
	)
	req.HeartbeatMaxAge, req.Now = 300*time.Second, now
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-small", res.HostID)

	req.Candidates = req.Candidates[:1]
	req.Candidates = append(req.Candidates, newHost("host-never", 64, 1<<20))
	_, err = Schedule(req)
	var nfe *NoFeasibleHostError
	require.True(t, errors.As(err, &nfe))
	assert.Equal(t, 2, nfe.Tally()[RejectionHeartbeatStale])
}

func TestScheduleHeartbeatStaleNeverRebindsToAStaleHost(t *testing.T) {
	now := time.Now()
	req := baseReq(newHost("host-a", 8, 16384, withHeartbeat(now.Add(-time.Hour))),
		newHost("host-b", 8, 16384, withHeartbeat(now)))
	req.CurrentBinding = "host-a"
	req.HeartbeatMaxAge = 5 * time.Minute
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-b", res.HostID, "a stale host is not re-selected")
}

func TestScheduleHeartbeatUncheckedForOtherProviders(t *testing.T) {
	req := baseReq(newHost("host-never", 8, 16384))
	res, err := Schedule(req)
	require.NoError(t, err, "HeartbeatMaxAge zero: a host without a heartbeat is a candidate as before")
	assert.Equal(t, "host-never", res.HostID)
}

func TestScheduleNotReadyHostOfAgentProviderIsSkipped(t *testing.T) {
	// A host whose inventory conditions fail is folded to NotReady by the
	// Host controller: the health gate rejects it before any other check.
	now := time.Now()
	req := baseReq(newHost("host-a", 64, 1<<20, hHealth(v1beta1.HostHealthNotReady), withHeartbeat(now)),
		newHost("host-b", 4, 8192, withHeartbeat(now)))
	req.HeartbeatMaxAge = 5 * time.Minute
	res, err := Schedule(req)
	require.NoError(t, err)
	assert.Equal(t, "host-b", res.HostID)
}
