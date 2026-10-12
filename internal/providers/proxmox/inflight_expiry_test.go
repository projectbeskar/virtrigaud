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

package proxmox

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInflightTracker_Expiry(t *testing.T) {
	now := time.Now()
	tr := &inflightTracker{now: func() time.Time { return now }}
	tr.record("a", inflightTask{node: "pve", vmid: 1, upid: "U1", maxAge: time.Minute})
	tr.record("b", inflightTask{node: "pve", vmid: 2, upid: "U2"})

	_, ok := tr.get("a")
	assert.True(t, ok)
	assert.True(t, tr.tracks(1, ""))

	now = now.Add(2 * time.Minute)
	_, ok = tr.get("a")
	assert.False(t, ok, "a record older than its maxAge is dropped")
	assert.False(t, tr.tracks(1, ""))
	assert.False(t, tr.tracks(0, "U1"))
	_, ok = tr.get("b")
	assert.True(t, ok, "a record without maxAge is kept")
}

// TestInflight_ExpiredRecordFallsBackToTheOwnerLookup (L-a): once an in-flight
// record is older than twice the task timeout it is dropped; a clone still
// running is then caught by the restart guard (nothing new is started), and
// once it ends the owner lookup finds the VM.
func TestInflight_ExpiredRecordFallsBackToTheOwnerLookup(t *testing.T) {
	srv, endpoint := startSlowFake(t)
	ctx := context.Background()
	p := slowTaskProvider(t, endpoint)
	assert.Equal(t, 100*time.Millisecond, p.inflightMaxAge(), "2 x the 50ms task timeout")

	_, err := p.Create(ctx, createReq(testOwner(), templateImageJSON))
	require.Equal(t, classOutcomeUnknown, classOf(t, err))
	_, recorded := p.inflight.get(testOwnerUID)
	require.True(t, recorded)

	later := time.Now().Add(time.Hour)
	p.inflight.now = func() time.Time { return later }
	_, recorded = p.inflight.get(testOwnerUID)
	require.False(t, recorded, "the record expired")

	_, err = p.Create(ctx, createReq(testOwner(), templateImageJSON))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err), "the restart guard holds the still-running clone")
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"))

	srv.CompleteTasks()
	srv.SetTaskDelay(0)
	_, err = p.Create(ctx, createReq(testOwner(), templateImageJSON))
	require.NoError(t, err)
	assert.Len(t, srv.VMsNamed(testOwnerName), 1)
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"))
}
