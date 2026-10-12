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
	"fmt"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// snapshotNotFoundError is the answer of a snapshot operation whose snapshot
// does not exist — the VM has no snapshot tree, the snapshot is not in it, the
// snapshot or the VM itself was removed (ManagedObjectNotFound): gRPC
// NotFound, which the manager maps to contracts.IsNotFound. A delete of a
// snapshot that is already gone is then an idempotent success for the manager
// (a VMSnapshot or VMMigration finalizer releases) instead of an error that is
// retried forever — e.g. a removal and consolidation that finished after the
// manager stopped waiting, or a delete whose success was answered but whose
// finalizer update then conflicted.
func snapshotNotFoundError(format string, args ...any) error {
	return status.Error(codes.NotFound, fmt.Sprintf(format, args...))
}

// isManagedObjectGone reports whether err is vCenter's "no such managed
// object" fault. It names SOME object — the VM, the snapshot, or a task
// (e.g. after a vpxd restart) — so on its own it proves nothing about the
// snapshot: see answerSnapshotObjectNotFound.
func isManagedObjectGone(err error) bool {
	return err != nil && fault.Is(err, &types.ManagedObjectNotFound{})
}

// answerSnapshotObjectNotFound answers a snapshot operation on vm that hit a
// ManagedObjectNotFound (cause) for snapshot snapshotID. The fault may name
// the task or another object, so the VM's snapshot tree is read again: the
// answer is NotFound only when the VM itself is gone (a not-found for the VM's
// own reference — e.g. it was unregistered, or re-registered under another
// id) or the snapshot is no longer in its tree. Otherwise — the snapshot is
// still there, or the VM cannot be read — the answer is Unavailable: the
// manager retries, and never releases a snapshot that may still exist.
func (p *Provider) answerSnapshotObjectNotFound(ctx context.Context, vm *object.VirtualMachine, snapshotID string, cause error) error {
	var vmObj mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"snapshot"}, &vmObj)
	switch {
	case isNotFound(err, vm.Reference()):
		return snapshotNotFoundError("VM %s not found, so it has no snapshot %s", vm.Reference().Value, snapshotID)
	case err != nil:
		p.logger.Warn("Could not re-read the VM's snapshots after a not-found answer", "vm_id", vm.Reference().Value,
			"snapshot_id", snapshotID, "cause", cause, "error", err)
		return status.Error(codes.Unavailable, fmt.Sprintf("could not confirm whether snapshot %s of VM %s still exists",
			snapshotID, vm.Reference().Value))
	case vmObj.Snapshot == nil || p.findSnapshotByID(vmObj.Snapshot.RootSnapshotList, snapshotID) == nil:
		return snapshotNotFoundError("snapshot not found: %s", snapshotID)
	}
	p.logger.Warn("A not-found answer did not concern the snapshot, which still exists; retry", "vm_id", vm.Reference().Value,
		"snapshot_id", snapshotID, "cause", cause)
	return status.Error(codes.Unavailable, fmt.Sprintf("snapshot %s of VM %s still exists after a not-found answer for "+
		"another object; retry", snapshotID, vm.Reference().Value))
}
