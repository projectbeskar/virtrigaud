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
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// These tests pin rc1 finding O3: two Creates that got the same VMID from
// /cluster/nextid both clone to it; qemu-server's clone worker of the loser
// cannot take the VMID's config lock (the winner's clone holds it) and dies
// with "can't lock file '/var/lock/qemu-server/lock-<vmid>.conf' - got
// timeout" before it makes anything. That answer is definite (nothing was
// cloned), and the winner's VM at the VMID is never touched.

// lockTimeoutStatus is the exit status of a qemu-server task that could not
// lock VM vmid's config.
func lockTimeoutStatus(vmid int) string {
	return fmt.Sprintf("can't lock file '/var/lock/qemu-server/lock-%d.conf' - got timeout", vmid)
}

// raceWinner makes the fake answer like a node where another request's clone
// to the same VMID is running: the clone task fails with exitStatus(target),
// and the winner's VM (locked "clone", another owner's) is at the target VMID
// once the task has ended. It returns a func that reports the target VMID.
func raceWinner(t *testing.T, srv *pvefake.Server, exitStatus func(target int) string) func() int {
	t.Helper()
	var mu sync.Mutex
	target := 0
	srv.InjectFault(pvefake.Fault{Method: http.MethodPost, PathContains: "/clone", PassThrough: true, Times: 1,
		Then: func(r *http.Request) {
			require.NoError(t, r.ParseForm())
			id, err := strconv.Atoi(r.FormValue("newid"))
			require.NoError(t, err)
			mu.Lock()
			target = id
			mu.Unlock()
			srv.FailTasksWith("qmclone", exitStatus(id))
		}})
	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/tasks/", PassThrough: true, Times: 1,
		Then: func(*http.Request) {
			mu.Lock()
			id := target
			mu.Unlock()
			srv.PutVM(pvefake.VM{VMID: id, Name: "winner", Node: "pve", Status: "stopped", Lock: "clone",
				Description: (&ownerStamp{owner: otherOwner(), phase: ownerStampPhaseCreating}).line()})
		}})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return target
	}
}

// TestClone_LockTimeoutIsNotPerformed: the template-clone Create and the
// Clone RPC answer a clone task that could not lock the VMID's (or the
// source's) config with CREATE_NOT_PERFORMED + VM_OPERATION_FAILED, and the
// winner's VM is left as it is.
func TestClone_LockTimeoutIsNotPerformed(t *testing.T) {
	for name, lockOf := range map[string]func(source, target int) int{
		"target lock": func(_, target int) int { return target },
		"source lock": func(source, _ int) int { return source },
	} {
		t.Run("create/"+name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			target := raceWinner(t, srv, func(target int) string { return lockTimeoutStatus(lockOf(seededTemplateID, target)) })
			_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
			assert.Equal(t, classNothingRemainsVM, classOf(t, err))
			winner, ok := srv.GetVM(target())
			require.True(t, ok, "the winner's VM is not touched")
			assert.Equal(t, "winner", winner.Name)
		})
		t.Run("clone/"+name, func(t *testing.T) {
			p, srv := newStampTestProvider(t)
			source := seededCloneSource(srv)
			sourceID, err := strconv.Atoi(source)
			require.NoError(t, err)
			target := raceWinner(t, srv, func(target int) string { return lockTimeoutStatus(lockOf(sourceID, target)) })
			req := stampedCloneReq(source)
			req.SourceOwner = ownerProto(otherOwner())
			_, err = p.Clone(context.Background(), req)
			assert.Equal(t, classNothingRemainsVM, classOf(t, err))
			winner, ok := srv.GetVM(target())
			require.True(t, ok, "the winner's VM is not touched")
			assert.Equal(t, "winner", winner.Name)
		})
	}

	t.Run("target config already exists", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		target := raceWinner(t, srv, func(target int) string { return targetExistsStatus(target) })
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classNothingRemainsVM, classOf(t, err))
		winner, ok := srv.GetVM(target())
		require.True(t, ok, "the winner's VM is not touched")
		assert.Equal(t, "winner", winner.Name)
	})

	t.Run("another VM's lock is not definite", func(t *testing.T) {
		p, srv := newStampTestProvider(t)
		raceWinner(t, srv, func(int) string { return lockTimeoutStatus(42) })
		_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		assert.Equal(t, classOutcomeUnknown, classOf(t, err), "the winner's locked VM cannot be removed: unknown, as before")
	})
}

// TestCloneTaskLockedOut: only a finished task (ErrTaskFailed) whose exit
// status is the config lock timeout of the source or the target counts.
func TestCloneTaskLockedOut(t *testing.T) {
	failed := func(status string) error { return fmt.Errorf("%w with exit code: %s", pveapi.ErrTaskFailed, status) }
	assert.True(t, cloneTaskLockedOut(failed(lockTimeoutStatus(200001)), 9000, 200001))
	assert.True(t, cloneTaskLockedOut(failed(lockTimeoutStatus(9000)), 9000, 200001))
	assert.False(t, cloneTaskLockedOut(failed(lockTimeoutStatus(7)), 9000, 200001), "another VM's lock")
	assert.False(t, cloneTaskLockedOut(failed("clone failed: "+lockTimeoutStatus(200001)), 9000, 200001),
		"a failure inside the clone, after it made something")
	assert.False(t, cloneTaskLockedOut(failed("can't lock file '/var/lock/pve-manager/pve-fw-200001.lck' - got timeout"), 9000, 200001))
	assert.False(t, cloneTaskLockedOut(fmt.Errorf("task timeout: %s", lockTimeoutStatus(200001)), 9000, 200001),
		"a task whose end is unknown")
	assert.False(t, cloneTaskLockedOut(failed("ERROR"), 9000, 200001))
	assert.True(t, cloneTaskLockedOut(failed(targetExistsStatus(200001)), 9000, 200001))
	assert.False(t, cloneTaskLockedOut(failed(targetExistsStatus(9000)), 9000, 200001), "only the target's config")
	assert.False(t, cloneTaskLockedOut(failed("clone failed: "+targetExistsStatus(200001)), 9000, 200001))
}

// targetExistsStatus is the exit status of a qemu-server clone task whose
// target VM vmid's config already existed.
func targetExistsStatus(vmid int) string {
	return fmt.Sprintf("unable to create VM %d: config file already exists", vmid)
}
