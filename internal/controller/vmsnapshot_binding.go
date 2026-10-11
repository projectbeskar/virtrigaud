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
	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

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
// now, is not the one the snapshot was taken of:
//
//   - a recorded status.vmUID that differs from vm's UID;
//   - no record (a snapshot taken by an older manager) and vm created after
//     the snapshot was (status.creationTime): it cannot be the snapshot's VM.
//
// A snapshot with no record and no creation time is never judged (today's
// behaviour).
func snapshotOfOtherVM(snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) bool {
	if snapshot.Status.VMUID != "" {
		return snapshot.Status.VMUID != string(vm.UID)
	}
	ct := snapshot.Status.CreationTime
	return ct != nil && vm.CreationTimestamp.After(ct.Time)
}

// backfillSnapshotBinding binds a snapshot taken by an older manager (no
// status.vmUID) to vm when vm existed before the snapshot was taken
// (creation time earlier than the snapshot's): that is the snapshot's VM. The
// Provider is the one vm is bound through (status.boundProvider), when
// recorded. It reports whether it changed the status.
func backfillSnapshotBinding(snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) bool {
	ct := snapshot.Status.CreationTime
	if snapshot.Status.VMUID != "" || ct == nil || vm.UID == "" || vm.CreationTimestamp.After(ct.Time) {
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
