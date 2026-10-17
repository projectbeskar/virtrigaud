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
	stderrors "errors"

	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A clone or a migration reads and copies its source VM's disks. A provider
// that checks the owner of those calls (Proxmox, whose VMIDs are reused)
// refuses a source VM that is not the source VirtualMachine's with
// VM_NOT_OWNED, and one whose owner stamp cannot be read with
// OWNER_STAMP_UNREADABLE, before any disk is read or copied. Neither changes
// on a retry, and a disk-size check that met one is not "a size we could not
// read": the copy is failed at once, with a constant message (the provider's
// answer goes to the log only), before any export, snapshot or power-off.

// migrationSourceNotOwnedMessage is the failure message of a migration whose
// source VM the provider refused as not the source VirtualMachine's, or as
// one whose owner cannot be told. It is constant.
const migrationSourceNotOwnedMessage = "The source VM's provider refused it: the VM at the source VirtualMachine's id " +
	"is not that VirtualMachine's (its id was most likely reused after its VM was removed outside VirtRigaud), or its " +
	"owner stamp cannot be read. Nothing was exported or migrated; check the source VirtualMachine's Ready condition"

// cloneSourceNotOwnedMessage is the failure message of a clone whose source
// VM the provider refused the same way. It is constant.
const cloneSourceNotOwnedMessage = "The provider refused the source VM: the VM at the source VirtualMachine's id is " +
	"not that VirtualMachine's (its id was most likely reused after its VM was removed outside VirtRigaud), or its " +
	"owner stamp cannot be read. Nothing was cloned; check the source VirtualMachine's Ready condition"

// isOwnerRefusal reports whether err is a provider's owner refusal of a
// per-VM call: VM_NOT_OWNED (contracts.IsVMNotOwned) or
// OWNER_STAMP_UNREADABLE (contracts.IsOwnerStampUnreadable).
func isOwnerRefusal(err error) bool {
	return contracts.IsVMNotOwned(err) || contracts.IsOwnerStampUnreadable(err)
}

// ownerRefusalReason is the condition reason of an owner refusal err:
// VMNotOwned or OwnerStampUnreadable.
func ownerRefusalReason(err error) string {
	if contracts.IsOwnerStampUnreadable(err) {
		return k8s.ReasonOwnerStampUnreadable
	}
	return k8s.ReasonVMNotOwned
}

// sourceOwnerRefusedError is returned by the disk-size checks of a clone or a
// migration when the source's provider refused the disk read as an owner
// refusal (isOwnerRefusal): the caller fails the copy instead of holding it
// as unverifiable or retrying.
type sourceOwnerRefusedError struct {
	err error
}

// Error implements error.
func (e *sourceOwnerRefusedError) Error() string {
	return "the source VM's provider refused to read its disks: " + e.err.Error()
}

// Unwrap returns the provider's answer.
func (e *sourceOwnerRefusedError) Unwrap() error { return e.err }

// isSourceOwnerRefused reports whether err is, or wraps, a
// *sourceOwnerRefusedError.
func isSourceOwnerRefused(err error) bool {
	var refused *sourceOwnerRefusedError
	return stderrors.As(err, &refused)
}
