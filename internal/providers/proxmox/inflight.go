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
	stderrors "errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
)

// In-flight creates.
//
// While PVE clones or creates a VM, the VM's configuration is a temporary one
// with `lock: clone` (or `lock: create`) and NO name and NO description: the
// owner stamp, sent with the request, is written only when the task ends. A
// Create re-sent while that task runs (e.g. after the manager's RPC deadline)
// cannot find the VM by its name and stamp, and would start a second clone.
//
// Two guards close that window:
//
//   - The provider records every create/clone task it starts for an owner
//     (inflightTracker, keyed by the owner's UID) until it sees the task end.
//     A Create or Clone for an owner whose task still runs (or whose state
//     cannot be read) is answered outcome-unknown, and nothing is started.
//   - The record does not survive a provider restart, and a request whose
//     answer was lost leaves a task whose UPID was never seen. So the owner
//     lookup also treats every unnamed guest locked for clone/create that is
//     not one of the provider's own recorded tasks as possibly this owner's,
//     when this provider's PVE user has an unrecorded clone/create task
//     running on that guest's node (or the node's tasks cannot be listed):
//     outcome unknown, never a new clone. Another user's clone (e.g. from the
//     GUI) does not hold creates.

// pveLockClone and pveLockCreate are the config locks of a VM PVE is still
// cloning or creating.
const (
	pveLockClone  = "clone"
	pveLockCreate = "create"
)

// pveTaskTypeClone and pveTaskTypeCreate are the PVE task types of a clone
// and a create (the clone task's id is the SOURCE VMID).
const (
	pveTaskTypeClone  = "qmclone"
	pveTaskTypeCreate = "qmcreate"
)

// pveTaskStatusStopped is the status of a PVE task that has ended.
const pveTaskStatusStopped = "stopped"

// inflightTask is a create or clone task the provider started for an owner and
// has not seen end.
type inflightTask struct {
	// node is the node the task runs on (and the VM's temporary config is on).
	node string
	// vmid is the VMID of the VM the task makes.
	vmid int
	// upid is the task id; "" when the request's answer was lost, so it is not
	// known whether the task started.
	upid string
	// recordedAt is when the task was (last) recorded.
	recordedAt time.Time
	// maxAge is how long the record is kept: past it, the record is dropped and
	// the owner lookup (and its restart guard, which then no longer sees the
	// task as recorded) decides alone. Zero keeps it until it is cleared.
	maxAge time.Duration
}

// expired reports whether the record is older than its maxAge at now.
func (task inflightTask) expired(now time.Time) bool {
	return task.maxAge > 0 && now.Sub(task.recordedAt) > task.maxAge
}

// inflightTracker records the in-flight create/clone task of each owner UID.
// The zero value is ready to use.
type inflightTracker struct {
	mu      sync.Mutex
	byOwner map[string]inflightTask
	// now is the clock (time.Now when nil; tests set it).
	now func() time.Time
}

// clock returns the tracker's current time.
func (t *inflightTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// pruneLocked drops expired records. The caller holds t.mu.
func (t *inflightTracker) pruneLocked() {
	now := t.clock()
	for uid, task := range t.byOwner {
		if task.expired(now) {
			delete(t.byOwner, uid)
		}
	}
}

// record sets the in-flight task of owner uid, recorded now.
func (t *inflightTracker) record(uid string, task inflightTask) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byOwner == nil {
		t.byOwner = make(map[string]inflightTask)
	}
	task.recordedAt = t.clock()
	t.byOwner[uid] = task
}

// get returns the in-flight task of owner uid (none once it expired).
func (t *inflightTracker) get(uid string) (inflightTask, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	task, ok := t.byOwner[uid]
	return task, ok
}

// clear forgets the in-flight task of owner uid if it is still the one making
// vmid.
func (t *inflightTracker) clear(uid string, vmid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task, ok := t.byOwner[uid]; ok && task.vmid == vmid {
		delete(t.byOwner, uid)
	}
}

// tracks reports whether some owner's recorded task makes vmid, or has upid.
func (t *inflightTracker) tracks(vmid int, upid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	for _, task := range t.byOwner {
		if (vmid != 0 && task.vmid == vmid) || (upid != "" && task.upid == upid) {
			return true
		}
	}
	return false
}

// ownerLocks lets one Create or Clone at a time run for an owner UID in this
// provider, for its whole course including a rollback. The zero value is
// ready to use.
type ownerLocks struct {
	mu   sync.Mutex
	busy map[string]bool
}

// tryLock takes the lock of owner uid; false when a call for it already runs.
func (l *ownerLocks) tryLock(uid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy[uid] {
		return false
	}
	if l.busy == nil {
		l.busy = make(map[string]bool)
	}
	l.busy[uid] = true
	return true
}

// unlock releases the lock of owner uid.
func (l *ownerLocks) unlock(uid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.busy, uid)
}

// lockOwner takes the per-owner lock for a Create or Clone and returns its
// release; a call already running for the owner is answered outcome-unknown
// (it may still make the VM). A request without an owner takes no lock.
func (p *Provider) lockOwner(uid string) (func(), error) {
	if uid == "" {
		return func() {}, nil
	}
	if !p.ownerLocks.tryLock(uid) {
		return nil, p.outcomeUnknown("another Create or Clone for this VirtualMachine is still running in the provider; will retry", nil)
	}
	return func() { p.ownerLocks.unlock(uid) }, nil
}

// checkInflight answers whether a Create or Clone for owner uid may go on: an
// earlier one's task that still runs, or whose state cannot be read, makes
// the answer outcome-unknown. A task seen to have ended is forgotten; the
// owner lookup then finds what it made (or nothing).
func (p *Provider) checkInflight(ctx context.Context, uid string) error {
	task, ok := p.inflight.get(uid)
	if !ok {
		return nil
	}
	if task.upid != "" {
		st, err := p.client.GetTaskStatus(ctx, task.node, task.upid)
		if err != nil {
			return p.outcomeUnknown(fmt.Sprintf("the state of PVE task for VM %d, started by an earlier Create for this "+
				"VirtualMachine, cannot be read; will retry", task.vmid), err)
		}
		if st.Status != pveTaskStatusStopped {
			return p.outcomeUnknown(fmt.Sprintf("Proxmox is still making VM %d for an earlier Create of this "+
				"VirtualMachine; will retry", task.vmid), nil)
		}
		p.inflight.clear(uid, task.vmid)
		return nil
	}
	// The request's answer was lost: look at the VMID it used, wherever it
	// is now (a clone's config moves to its target node when it ends).
	cfg, err := p.vmConfigAnywhere(ctx, task.vmid)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		p.inflight.clear(uid, task.vmid)
		return nil
	}
	if err != nil {
		return p.outcomeUnknown(fmt.Sprintf("VM %d, which an earlier Create for this VirtualMachine may have started, "+
			"cannot be checked; will retry", task.vmid), err)
	}
	if beingMade(cfg) {
		return p.outcomeUnknown(fmt.Sprintf("Proxmox is still making VM %d for an earlier Create of this "+
			"VirtualMachine; will retry", task.vmid), nil)
	}
	p.inflight.clear(uid, task.vmid)
	return nil
}

// vmConfigAnywhere reads the config of vmid on whichever node of the cluster
// has it. It returns pveapi.ErrVMNotFound only when the VMID is free in the
// whole cluster (vmGone).
func (p *Provider) vmConfigAnywhere(ctx context.Context, vmid int) (map[string]interface{}, error) {
	vms, err := p.client.ListClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	for _, vm := range vms {
		if vm.VMID == vmid {
			return p.client.GetVMConfig(ctx, vm.Node, vmid)
		}
	}
	free, err := p.client.VMIDFree(ctx, vmid)
	switch {
	case err != nil:
		return nil, err
	case !free:
		return nil, fmt.Errorf("VMID %d is in use but not listed in the cluster's resources", vmid)
	}
	return nil, pveapi.ErrVMNotFound
}

// trackTask records the task that makes VM vmid on node for stamp's owner
// (upid "" before its answer arrives). Nothing is recorded without an owner.
func (p *Provider) trackTask(stamp *ownerStamp, node string, vmid int, upid string) {
	if stamp != nil {
		p.inflight.record(stamp.owner.UID, inflightTask{node: node, vmid: vmid, upid: upid, maxAge: p.inflightMaxAge()})
	}
}

// inflightMaxAgeFactor times the PVE task timeout is how long an in-flight
// record is kept.
const inflightMaxAgeFactor = 2

// inflightMaxAge is how long an in-flight record is kept: twice the client's
// task timeout (the longest the provider waits for a task). Past it the
// owner lookup decides alone: a VM made by then is named and stamped, and a
// task still running is caught by the restart guard.
func (p *Provider) inflightMaxAge() time.Duration {
	timeout := pveapi.DefaultTaskTimeout
	if p.client != nil && p.client.Config().TaskTimeout > 0 {
		timeout = p.client.Config().TaskTimeout
	}
	return inflightMaxAgeFactor * timeout
}

// untrackTask forgets the recorded task that makes vmid for stamp's owner:
// it ended.
func (p *Provider) untrackTask(stamp *ownerStamp, vmid int) {
	if stamp != nil {
		p.inflight.clear(stamp.owner.UID, vmid)
	}
}

// submitFailed handles the failure err (already classified) of the request
// that makes VM vmid: when PVE certainly started nothing, the recorded task
// is forgotten; otherwise it stays, so a re-sent request waits for it.
func (p *Provider) submitFailed(stamp *ownerStamp, vmid int, err error) error {
	if nothingStarted(err) {
		p.untrackTask(stamp, vmid)
	}
	return err
}

// nothingStarted reports whether a classified Create/Clone error says that
// nothing was started: CREATE_NOT_PERFORMED, InvalidSpec or PermissionDenied.
func nothingStarted(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.PermissionDenied:
		return true
	}
	return contracts.IsCreateNotPerformed(err)
}

// beingMade reports whether a VM config is the temporary one of a VM PVE is
// still cloning or creating: locked for clone/create, and without a name.
func beingMade(cfg map[string]interface{}) bool {
	lock := configString(cfg, pveConfigKeyLock)
	return (lock == pveLockClone || lock == pveLockCreate) && configString(cfg, pveConfigKeyName) == ""
}

// unattributedInflight reports whether the unnamed guest vmid on node may be a
// VM this provider is still making for an owner it no longer has a record
// of: its config is the temporary one of a clone/create, it is not one of the
// provider's recorded tasks, and this provider's PVE user has a clone/create
// task running on node that is not recorded either (or the node's tasks
// cannot be listed). Any failure to tell answers true.
func (p *Provider) unattributedInflight(ctx context.Context, node string, vmid int) bool {
	if p.inflight.tracks(vmid, "") {
		return false
	}
	cfg, err := p.client.GetVMConfig(ctx, node, vmid)
	if stderrors.Is(err, pveapi.ErrVMNotFound) {
		return false // finished or removed since the list
	}
	if err != nil || !beingMade(cfg) {
		return err != nil
	}
	me := p.client.Identity()
	tasks, err := p.client.ListActiveTasks(ctx, node)
	if err != nil || me == "" {
		return true
	}
	for _, t := range tasks {
		if (t.Type == pveTaskTypeClone || t.Type == pveTaskTypeCreate) && t.User == me && !p.inflight.tracks(0, t.UPID) {
			return true
		}
	}
	return false
}
