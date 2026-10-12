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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// testTokenID is the API token the test providers authenticate with; the fake
// records it as the user of the tasks they start.
const testTokenID = "test@pve!token"

// slowTaskProvider returns a provider on srv whose task waits give up after
// 50ms, as a Create whose RPC deadline expired does.
func slowTaskProvider(t *testing.T, endpoint string) *Provider {
	t.Helper()
	client, err := pveapi.NewClient(&pveapi.Config{
		Endpoint: endpoint, TokenID: testTokenID, TokenSecret: "secret", InsecureSkipVerify: true,
		TaskPollInterval: 5 * time.Millisecond, TaskTimeout: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	p := New()
	p.client = client
	return p
}

// startSlowFake starts a fake PVE whose tasks run for an hour unless ended
// with CompleteTasks.
func startSlowFake(t *testing.T) (*pvefake.Server, string) {
	t.Helper()
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	srv.SetTaskDelay(time.Hour)
	return srv, endpoint
}

// countRequests counts the received requests with method whose path ends with
// suffix.
func countRequests(srv *pvefake.Server, method, suffix string) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			n++
		}
	}
	return n
}

// TestInflight_ResentCreateWaitsForTheRunningClone (H1): while PVE clones,
// the clone's config is temporary (lock clone, no name, no description), so
// the owner lookup cannot see it. A Create re-sent after the first one gave up
// waiting is answered outcome-unknown, and no second clone is started — both
// by the same provider (its in-flight record) and after a provider restart
// (the unnamed locked guest with this provider's running clone task). Once
// the clone ends, the re-sent Create finishes and returns that VM.
func TestInflight_ResentCreateWaitsForTheRunningClone(t *testing.T) {
	srv, endpoint := startSlowFake(t)
	ctx := context.Background()
	p := slowTaskProvider(t, endpoint)

	_, err := p.Create(ctx, createReq(testOwner(), templateImageJSON))
	require.Error(t, err)
	assert.Equal(t, classOutcomeUnknown, classOf(t, err), "the clone is still running")
	require.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"))

	// The same provider: its record.
	_, err = p.Create(ctx, createReq(testOwner(), templateImageJSON))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"), "no second clone")

	// A restarted provider: no record, but the unnamed guest locked for clone
	// and this PVE user's running clone task.
	restarted := slowTaskProvider(t, endpoint)
	_, err = restarted.Create(ctx, createReq(testOwner(), templateImageJSON))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	assert.False(t, contracts.IsCreateNotPerformed(err))
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"), "no second clone after a restart")

	// The clone ends: the re-sent Create finishes it.
	srv.CompleteTasks()
	srv.SetTaskDelay(0)
	resp, err := restarted.Create(ctx, createReq(testOwner(), templateImageJSON))
	require.NoError(t, err)
	vms := srv.VMsNamed(testOwnerName)
	require.Len(t, vms, 1)
	assert.Equal(t, resp.Id, strconv.Itoa(vms[0].VMID))
	assert.Equal(t, ownerStampPhaseReady, stampOf(t, vms[0]).phase)
	assert.Equal(t, 4, vms[0].CPUs)
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/clone"))
}

// TestInflight_LostCreateAnswerWhileTaskRuns: the answer to a plain create was
// lost after PVE started it; the re-sent Create sees the temporary config at
// the VMID it used and waits.
func TestInflight_LostCreateAnswerWhileTaskRuns(t *testing.T) {
	srv, endpoint := startSlowFake(t)
	ctx := context.Background()
	p := slowTaskProvider(t, endpoint)
	srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/nodes/pve/qemu", Status: http.StatusBadGateway,
		Body: "lost", AfterApply: true, Times: 1})

	_, err := p.Create(ctx, createReq(testOwner(), ""))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))

	_, err = p.Create(ctx, createReq(testOwner(), ""))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err), "the create PVE started is still running")
	assert.Equal(t, 1, countRequests(srv, http.MethodPost, "/nodes/pve/qemu"))

	srv.CompleteTasks()
	resp, err := p.Create(ctx, createReq(testOwner(), ""))
	require.NoError(t, err)
	vms := srv.VMsNamed(testOwnerName)
	require.Len(t, vms, 1)
	assert.Equal(t, strconv.Itoa(vms[0].VMID), resp.Id)
}

// TestInflight_OtherOwnersAndOtherUsersDoNotHold: a running clone recorded
// for another owner, or one started by another PVE user (e.g. from the GUI),
// does not hold a Create.
func TestInflight_OtherOwnersAndOtherUsersDoNotHold(t *testing.T) {
	srv, endpoint := startSlowFake(t)
	ctx := context.Background()
	p := slowTaskProvider(t, endpoint)

	_, err := p.Create(ctx, createReq(otherOwner(), templateImageJSON))
	require.Equal(t, classOutcomeUnknown, classOf(t, err), "another owner's clone keeps running")

	// Another PVE user's clone in progress.
	srv.PutVM(pvefake.VM{VMID: 4600, Node: "pve", Status: "stopped", Lock: "clone"})
	srv.StartTask("pve", "qmclone", "9000", "admin@pam")

	srv.SetTaskDelay(0)
	_, err = p.Create(ctx, createReq(testOwner(), ""))
	require.NoError(t, err)
	assert.Len(t, srv.VMsNamed(testOwnerName), 1)
}

// TestInflight_UnreadableTasksHold: when the node's tasks cannot be listed, an
// unattributed clone in progress may be this owner's: hold.
func TestInflight_UnreadableTasksHold(t *testing.T) {
	srv, endpoint := startSlowFake(t)
	p := slowTaskProvider(t, endpoint)
	srv.PutVM(pvefake.VM{VMID: 4601, Node: "pve", Status: "stopped", Lock: "clone"})
	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/nodes/pve/tasks", Status: http.StatusInternalServerError, Body: "boom"})
	_, err := p.Create(context.Background(), createReq(testOwner(), ""))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	assert.Empty(t, srv.VMsNamed(testOwnerName))
}
