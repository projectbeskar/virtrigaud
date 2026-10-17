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

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A SnapshotDelete answered VM_NOT_OWNED (contracts.IsVMNotOwned) is a
// NotFound: the VM at the VirtualMachine's id is not its own, so the snapshot
// is released like one that is gone — retrying cannot delete it. But unlike a
// plain NotFound, a VM is known to still run at that id, and it may be the
// VirtualMachine's own after all (renamed on the hypervisor, or the
// VirtualMachine re-created under a new UID), with the snapshot still on it.
// So the release is recorded as a SnapshotLeftOnHypervisor Warning, never as
// "already gone" — mirroring recordVMNotOwnedRelease for a VM's delete.

// snapshotNotOwnedLeftMessage is the SnapshotLeftOnHypervisor Warning of a
// snapshot released on a VM_NOT_OWNED delete. It is constant: it names no VM,
// owner or provider text.
const snapshotNotOwnedLeftMessage = "The snapshot was released without being deleted: the provider reports that " +
	"the VM at this VirtualMachine's id is not that VirtualMachine's (another owner's stamp, or another name), and " +
	"left it untouched. The snapshot may be left on the VM at this id if that VM is in fact the VirtualMachine's " +
	"(renamed on the hypervisor, or the VirtualMachine re-created under a new UID): check the hypervisor and remove it"

// recordSnapshotNotOwnedRelease records that the provider snapshot snapshotID
// of the VM at vmID, owned by obj (a VMSnapshot, or a VMMigration's source
// snapshot), is released on a VM_NOT_OWNED delete: an error-level log line
// with the ids (logr has no warning level; the object may go away moments
// later, so the log is the durable record), the vm-not-owned metric, and a
// SnapshotLeftOnHypervisor Warning with the constant
// snapshotNotOwnedLeftMessage.
func recordSnapshotNotOwnedRelease(ctx context.Context, recorder record.EventRecorder, obj runtime.Object, vmID, snapshotID string) {
	log.FromContext(ctx).Error(contracts.ErrVMNotOwned, "The provider reports that the VM at this id is not the "+
		"VirtualMachine's; releasing its snapshot without deleting it, the snapshot may be left on that VM",
		"vm_id", vmID, "snapshot_id", snapshotID)
	metrics.RecordError(errReasonVMNotOwned, metrics.ComponentManager)
	if recorder != nil {
		recorder.Event(obj, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, snapshotNotOwnedLeftMessage)
	}
}
