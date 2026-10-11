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
	"fmt"

	"github.com/vmware/govmomi/fault"
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
// object" fault: the VM or snapshot it names was removed.
func isManagedObjectGone(err error) bool {
	return err != nil && fault.Is(err, &types.ManagedObjectNotFound{})
}
