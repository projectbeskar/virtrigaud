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
	"time"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// legacyBindingSkewMargin is the clock skew allowed between the manager, which
// wrote a legacy snapshot's status.creationTime, and the API server, which
// set the VM's creationTimestamp. A VM created within it of the snapshot is
// ambiguous and treated as another VM.
const legacyBindingSkewMargin = 30 * time.Second

// A VMSnapshot is bound to the VirtualMachine — by UID — and the Provider it
// was taken through (status.vmUID, status.boundProvider), not only to the VM
// name in spec.vmRef. A VM deleted and re-created under the same name, maybe
// on another Provider, is another VM: its snapshot ids are not this
// snapshot's, and on libvirt and Proxmox a snapshot id is a name, so a delete
// sent through it could remove another VMSnapshot's same-named snapshot.

// recordSnapshotBinding records, on the snapshot whose create is being
// issued, the VM it is taken of and the Provider it goes through.
func recordSnapshotBinding(snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine, provider *infrav1beta1.Provider) {
	snapshot.Status.VMUID = string(vm.UID)
	snapshot.Status.BoundProvider = boundProviderRefFor(provider)
}

// snapshotOfOtherVM reports whether vm, the VirtualMachine spec.vmRef names
// now, is not the one the snapshot was taken of: a recorded status.vmUID that
// differs from vm's UID, or, for a snapshot taken by an older manager (no
// record), legacySnapshotOfVM says it is another VM.
func snapshotOfOtherVM(snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) bool {
	if snapshot.Status.VMUID != "" {
		return snapshot.Status.VMUID != string(vm.UID)
	}
	known, same := legacySnapshotOfVM(snapshot, vm)
	return known && !same
}

// legacySnapshotOfVM judges whether a snapshot taken by an older manager (no
// status.vmUID) is of vm. known is false when it cannot be judged (no
// status.creationTime: today's behaviour).
//
//   - vm was created before the VMSnapshot object (both API-server
//     timestamps, one clock): vm existed before the snapshot was asked for
//     and still has the same UID — it is the snapshot's VM;
//   - otherwise vm was created after the VMSnapshot object (a snapshot can
//     wait for its VM). Its creation is compared with the snapshot's
//     status.creationTime, written by the manager's clock: more than
//     legacyBindingSkewMargin before it, the VM existed when the snapshot was
//     taken; anything later — after it, or within the margin, where the two
//     clocks cannot tell — is another VM.
func legacySnapshotOfVM(snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) (known, same bool) {
	ct := snapshot.Status.CreationTime
	if ct == nil {
		return false, false
	}
	if !snapshot.CreationTimestamp.IsZero() && vm.CreationTimestamp.Before(&snapshot.CreationTimestamp) {
		return true, true
	}
	return true, vm.CreationTimestamp.Time.Before(ct.Time.Add(-legacyBindingSkewMargin))
}

// backfillSnapshotBinding binds a snapshot taken by an older manager (no
// status.vmUID) to vm when legacySnapshotOfVM proves vm is its VM. The
// Provider is the one vm is bound through (status.boundProvider), when
// recorded. It reports whether it changed the status.
func backfillSnapshotBinding(snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) bool {
	if snapshot.Status.VMUID != "" || vm.UID == "" {
		return false
	}
	if known, same := legacySnapshotOfVM(snapshot, vm); !known || !same {
		return false
	}
	snapshot.Status.VMUID = string(vm.UID)
	if bp := vm.Status.BoundProvider; bp != nil && snapshot.Status.BoundProvider == nil {
		cp := *bp
		if cp.Namespace == "" {
			cp.Namespace = vm.Namespace
		}
		snapshot.Status.BoundProvider = &cp
	}
	return true
}

// snapshotProviderMismatch reports whether provider — the Provider the VM's
// spec.providerRef resolves to now — is not the one the snapshot was taken
// through (status.boundProvider: namespace and name and, when both are
// recorded, UID). A snapshot with no recorded Provider is not judged.
func snapshotProviderMismatch(snapshot *infrav1beta1.VMSnapshot, provider *infrav1beta1.Provider) bool {
	bp := snapshot.Status.BoundProvider
	if bp == nil || provider == nil {
		return false
	}
	if bp.Namespace != provider.Namespace || bp.Name != provider.Name {
		return true
	}
	return bp.UID != "" && provider.UID != "" && bp.UID != string(provider.UID)
}
