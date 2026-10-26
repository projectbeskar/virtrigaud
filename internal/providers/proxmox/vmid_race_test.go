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
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// These tests pin rc2 finding B1: two Creates got the same VMID from
// /cluster/nextid; the winner's clone was still running when the loser's
// request arrived, and PVE refused that request synchronously ("unable to
// create VM <vmid>: config file already exists", HTTP 500, no task). The
// winner's VM is then a temporary config (locked, no name, no description),
// whose owner cannot be read from it. The refusal is "not performed" when the
// winner's running task is recorded by this provider, for the winner's owner,
// as making that VMID, and no other clone or create task of this provider's
// PVE user that could be the loser's own runs on the node; it stays unknown
// when the VM may be this owner's.

// raceProvider returns a provider whose task waits give up after 50ms, on a
// fake whose tasks run until CompleteTasks, with a recording SSH node (for
// the imported-disk create).
func raceProvider(t *testing.T) (*Provider, *pvefake.Server) {
	t.Helper()
	srv, endpoint := startSlowFake(t)
	p := slowTaskProvider(t, endpoint)
	node := &recordingNode{}
	p.ssh = &sshTransport{host: "pve", logger: slog.Default(), runOverride: node.run}
	return p, srv
}

// winnerTakesVMID makes the next POST whose path contains path race with
// winner: before PVE serves it, winner makes (and leaves being made) a VM at
// the VMID the request names (form key idKey), as another request given the
// same VMID by /cluster/nextid does. It returns a func that reports that
// VMID.
func winnerTakesVMID(t *testing.T, srv *pvefake.Server, path, idKey string, winner func(t *testing.T)) func() int {
	t.Helper()
	var mu sync.Mutex
	target := 0
	srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: path, PassThrough: true, Times: 1,
		// Then runs on the fake's handler goroutine: assert, never require.
		Then: func(r *http.Request) {
			if !assert.NoError(t, r.ParseForm()) {
				return
			}
			id, err := strconv.Atoi(r.FormValue(idKey))
			if !assert.NoError(t, err) {
				return
			}
			mu.Lock()
			target = id
			mu.Unlock()
			srv.SetNextID(id)
			winner(t)
			vm, ok := srv.GetVM(id)
			if assert.True(t, ok, "the winner made a VM at the loser's VMID") {
				assert.NotEmpty(t, vm.Lock, "the winner's VM is still being made")
			}
		}})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return target
	}
}

// winnerOwner is the owner of the request that wins the VMID.
func winnerOwner() contracts.ObjectIdentity {
	return contracts.ObjectIdentity{UID: "3c2b1a09-8f7e-4d6c-9b5a-4f3e2d1c0b9a", Namespace: "team-c", Name: "winner"}
}

// winnerClones is a winner that clones the seeded template for winnerOwner()
// through p: its clone task keeps running, and p keeps its record.
func winnerClones(p *Provider) func(t *testing.T) {
	return func(t *testing.T) {
		_, err := p.Create(context.Background(), createReq(winnerOwner(), templateImageJSON))
		assert.False(t, contracts.IsCreateNotPerformed(err), "the winner's clone is still running: %v", err)
		assert.Error(t, err, "the winner's clone is still running")
	}
}

// winnerCreates is a winner that creates a VM without a template for
// winnerOwner() through p (its create task is not awaited and keeps running).
func winnerCreates(p *Provider) func(t *testing.T) {
	return func(t *testing.T) {
		_, err := p.Create(context.Background(), createReq(winnerOwner(), ""))
		assert.NoError(t, err)
	}
}

// TestVMIDRace_SyncRefusalWhileWinnerIsBeingMade (B1): every create path
// answers PVE's synchronous refusal of its request, while another owner's
// VM at that VMID is still being made by this provider, with
// CREATE_NOT_PERFORMED + VM_OPERATION_FAILED; the winner's VM is not
// touched, and the loser's re-sent request succeeds with a new VMID.
func TestVMIDRace_SyncRefusalWhileWinnerIsBeingMade(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		idKey  string
		winner func(p *Provider) func(t *testing.T)
		loser  func(p *Provider, srv *pvefake.Server) func() (string, error)
	}{
		{"create from a template", "/clone", "newid", winnerClones,
			func(p *Provider, _ *pvefake.Server) func() (string, error) {
				return func() (string, error) {
					resp, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
					return resp.GetId(), err
				}
			}},
		{"create without a template", "/nodes/pve/qemu", "vmid", winnerCreates,
			func(p *Provider, _ *pvefake.Server) func() (string, error) {
				return func() (string, error) {
					resp, err := p.Create(context.Background(), createReq(testOwner(), ""))
					return resp.GetId(), err
				}
			}},
		{"create from an imported disk", "/nodes/pve/qemu", "vmid", winnerCreates,
			func(p *Provider, _ *pvefake.Server) func() (string, error) {
				return func() (string, error) {
					resp, err := p.Create(context.Background(), createReq(testOwner(), ownStageImageJSON))
					return resp.GetId(), err
				}
			}},
		{"VMClone", "/clone", "newid", winnerClones,
			func(p *Provider, srv *pvefake.Server) func() (string, error) {
				source := seededCloneSource(srv)
				return func() (string, error) {
					req := stampedCloneReq(source)
					req.SourceOwner = ownerProto(otherOwner())
					resp, err := p.Clone(context.Background(), req)
					return resp.GetTargetVmId(), err
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := raceProvider(t)
			loser := tc.loser(p, srv)
			target := winnerTakesVMID(t, srv, tc.path, tc.idKey, tc.winner(p))

			_, err := loser()
			assert.Equal(t, classNothingRemainsVM, classOf(t, err), "error: %v", err)
			winner, ok := srv.GetVM(target())
			require.True(t, ok, "the winner's VM is not touched")
			assert.NotEmpty(t, winner.Lock, "the winner's VM is still being made")
			_, recorded := p.inflight.get(testOwnerUID)
			_, cloneRecorded := p.inflight.get(cloneTargetOwnerID().UID)
			assert.False(t, recorded || cloneRecorded, "nothing of the loser's is in flight")

			// The re-sent request gets a new VMID and makes its VM.
			srv.CompleteTasks()
			srv.SetTaskDelay(0)
			id, err := loser()
			require.NoError(t, err)
			assert.NotEqual(t, strconv.Itoa(target()), id, "a new VMID")
		})
	}
}

// TestVMIDRace_WinnersRecordExpired: a winner whose in-flight record expired
// while its clone still runs is no longer a known maker of the VMID: the
// refusal stays unknown.
func TestVMIDRace_WinnersRecordExpired(t *testing.T) {
	p, srv := raceProvider(t)
	winnerTakesVMID(t, srv, "/clone", "newid", func(t *testing.T) {
		winnerClones(p)(t)
		p.inflight.mu.Lock()
		p.inflight.now = func() time.Time { return time.Now().Add(time.Hour) }
		p.inflight.mu.Unlock()
	})
	_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
}

// TestVMIDRace_SyncRefusalStaysUnknownWhenTheVMMayBeThisOwners: the VM at the
// VMID is being made, and it may be this owner's: a clone task of this
// provider's PVE user runs on the node that the provider has no record of
// (an earlier attempt whose answer was lost, or that a provider before a
// restart started), the node's tasks cannot be listed, or the VM was
// finished with this owner's stamp between the reads. The outcome stays
// unknown.
func TestVMIDRace_SyncRefusalStaysUnknownWhenTheVMMayBeThisOwners(t *testing.T) {
	beingMadeAt := func(srv *pvefake.Server) func(r *http.Request) {
		return func(r *http.Request) {
			id, err := strconv.Atoi(r.FormValue("newid"))
			if err == nil {
				srv.PutVM(pvefake.VM{VMID: id, Node: "pve", Status: "stopped", Lock: "clone"})
			}
		}
	}
	refuse := func(srv *pvefake.Server, then func(r *http.Request)) {
		srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", Status: http.StatusInternalServerError,
			Body: "unable to create VM: config file already exists", Times: 1, Then: then})
	}

	t.Run("an unrecorded clone task of this PVE user", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		refuse(srv, func(r *http.Request) {
			beingMadeAt(srv)(r)
			srv.StartTask("pve", "qmclone", strconv.Itoa(seededTemplateID), testTokenID)
		})
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	})

	t.Run("an unrecorded create task of this PVE user", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		refuse(srv, func(r *http.Request) {
			beingMadeAt(srv)(r)
			srv.StartTask("pve", "qmcreate", "4700", testTokenID)
		})
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	})

	t.Run("the node's tasks cannot be listed", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		refuse(srv, beingMadeAt(srv))
		srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/nodes/pve/tasks", Status: http.StatusInternalServerError, Body: "boom"})
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	})

	t.Run("finished with this owner's stamp between the reads", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		var id int
		refuse(srv, func(r *http.Request) {
			beingMadeAt(srv)(r)
			id, _ = strconv.Atoi(r.FormValue("newid"))
		})
		srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/nodes/pve/tasks", PassThrough: true, Times: 1,
			Then: func(*http.Request) {
				srv.PutVM(pvefake.VM{VMID: id, Name: testOwnerName, Node: "pve", Status: "stopped",
					Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()})
			}})
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	})
}

// TestInflightTracker_TaskRecordedFor: a UPID is attributed only to a
// recorded task of an owner other than the one asked about.
func TestInflightTracker_TaskRecordedFor(t *testing.T) {
	var tr inflightTracker
	tr.record("a", inflightTask{node: "pve", vmid: 1, upid: "U1"})
	tr.record("b", inflightTask{node: "pve", vmid: 2})
	assert.True(t, tr.taskRecordedFor("U1", ""), "any owner")
	assert.True(t, tr.taskRecordedFor("U1", "b"), "another owner's task")
	assert.False(t, tr.taskRecordedFor("U1", "a"), "the owner's own task is not another's")
	assert.False(t, tr.taskRecordedFor("U2", ""), "not recorded")
	assert.False(t, tr.taskRecordedFor("", ""), "a record without a UPID matches nothing")
}
