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

package contracts

import (
	"context"
	"fmt"
	"slices"
)

// VMInfoUUIDKey is the VMInfo.ProviderRaw key under which a provider reports a
// listed VM's immutable identifier when it has one (libvirt: the domain UUID).
// The clustered adoption flow passes it back as
// TransferOwnerRequest.ExpectedUUID, so that a VM replaced since the list is
// never stamped.
const VMInfoUUIDKey = "uuid"

// VMInfoOwnerStampStateKey is the VMInfo.ProviderRaw key under which a
// provider that stamps the VMs it creates with their owner reports that a
// listed VM's owner stamp cannot be relied on: OwnerStampUnreadable or
// OwnerStampMultiple. The clustered libvirt provider reports it (ADR-0007
// Addendum A, slice 4), reading both definitions of an active domain; the
// single-host libvirt provider reports it too, from the definition it lists.
// Absent when the VM carries no stamp or exactly one readable owner. Adoption,
// single-host and clustered, skips a VM carrying this key, whatever its value
// (even empty or one this release does not know): who owns it cannot be told.
const VMInfoOwnerStampStateKey = "owner_stamp_state"

const (
	// OwnerStampUnreadable: a stamp (in the running or the persistent
	// definition) could not be read, or records no UID.
	OwnerStampUnreadable = "unreadable"
	// OwnerStampMultiple: more than one owner is recorded across the VM's
	// definitions (only a hand-edited definition has that).
	OwnerStampMultiple = "multiple"
)

// VMInfoArtifactKey is the VMInfo.ProviderRaw key under which a provider marks
// a listed object that is shared image state rather than a VM: ArtifactTemplate
// or ArtifactImage.
//
// ListVMs is the adoption discovery feed, and every VM it returns can become a
// tenant-owned VirtualMachine whose delete destroys the object. A provider
// therefore does not list templates or image artifacts at all (vSphere leaves
// them out in listExclusionFor; Proxmox skips template == 1). A provider that
// does list one MUST set this key on it. Adoption, single-host and clustered,
// skips any VM carrying the key, whatever its value (including a value this
// release does not know), and never copies it into a VirtualMachine's status.
//
// The provider-side exclusion is the primary control; this key is the
// manager-side backstop for third-party and SDK providers. A VirtRigaud
// provider keeps excluding these objects rather than returning them tagged:
// an older manager does not know the key, and the upgrade order rolls
// providers before (or independently of) the manager.
const VMInfoArtifactKey = "artifact"

const (
	// ArtifactTemplate: a hypervisor template (vSphere config.template,
	// Proxmox template == 1) that VMs are cloned from.
	ArtifactTemplate = "template"
	// ArtifactImage: a VirtRigaud prepared image or an image import in flight
	// (an object carrying a prepared-image stamp), template or not.
	ArtifactImage = "image"
)

// TransferOwnerRequest asks a provider to hand one VM to a VirtualMachine by
// re-stamping the VM's owner: on a clustered provider a VM on one of its hosts
// (ADR-0007 Addendum A, slice 4), on a single-host provider that reports
// Capabilities.SupportsOwnerTransfer a VM addressed by its id alone. It is the
// manager-side mirror of the provider.v1 TransferOwnerRequest message.
type TransferOwnerRequest struct {
	// VM addresses the VM: its ID (VMInfo.ID), the host it is on
	// (VMInfo.HostID; empty on a single-host provider) and, as Owner, the
	// identity of the VirtualMachine that takes it over. The provider stamps
	// Owner onto the VM.
	VM VMRef
	// ReplaceableOwnerUIDs are the owner UIDs recorded on the VM
	// (VMInfoOwnerUIDKey) that the manager verified belong to no existing
	// VirtualMachine. Only these stamps may be replaced; empty takes over only
	// an unstamped VM (or one already stamped with Owner, an idempotent retry).
	ReplaceableOwnerUIDs []string
	// ExpectedUUID is the VM's immutable identifier (VMInfoUUIDKey), when the
	// provider lists one. The provider refuses to stamp a VM that no longer
	// carries it.
	ExpectedUUID string
}

// OwnerTransferDecision is the compare-and-swap decision every provider's
// TransferOwner makes once it has read the owner stamps recorded on the VM
// (none for an unstamped VM): may the VM be stamped with owner?
//
//   - unstamped: yes (already false);
//   - exactly owner's stamp (UID, namespace and name): already true — an
//     idempotent retry, nothing is written;
//   - one stamp whose UID is in replaceable (owners the manager verified no
//     longer exist): yes;
//   - anything else — more than one stamp, a stamp without a UID, owner's UID
//     under another namespace or name (an edited stamp), or another owner's
//     UID that is not replaceable: refused (a non-nil error). The error names
//     no owner and is meant for the provider log; the caller answers the
//     constant refusal (Conflict) on the wire.
func OwnerTransferDecision(owner ObjectIdentity, recorded []ObjectIdentity, replaceable []string) (already bool, err error) {
	switch {
	case len(recorded) == 0:
		return false, nil
	case len(recorded) > 1:
		return false, fmt.Errorf("it carries %d owner stamps", len(recorded))
	}
	got := recorded[0]
	switch {
	case got.UID == "":
		return false, fmt.Errorf("its owner stamp has no uid")
	case got.UID == owner.UID && got == owner:
		return true, nil
	case got.UID == owner.UID:
		return false, fmt.Errorf("it carries the new owner's uid under another namespace or name (an edited stamp)")
	case slices.Contains(replaceable, got.UID):
		return false, nil
	}
	return false, fmt.Errorf("it is stamped for a VirtualMachine the manager did not report as gone")
}

// OwnerTransferrer is an optional capability of a Provider: it re-stamps a VM
// with the VirtualMachine that takes it over (TransferOwner),
// compare-and-swap. The manager gRPC client implements it; the adoption
// controller type-asserts a Provider to OwnerTransferrer, and uses it only
// when the provider reports Capabilities.SupportsRoutedAdoption (clustered)
// or Capabilities.SupportsOwnerTransfer (single-host), so the core Provider
// interface and the test fakes that do not adopt are unaffected. This mirrors
// the Cloner pattern.
//
// A6 (restore re-binding) can reuse it: a VirtualMachine restored from a
// backup has a new UID, so its VM's stamp names a VirtualMachine that no longer
// exists and is replaceable exactly as for adoption.
type OwnerTransferrer interface {
	// TransferOwner stamps req.VM.Owner onto the VM req.VM addresses. It
	// succeeds when the VM now carries exactly that owner's stamp (including
	// when it already did); it refuses — Conflict, never touching the VM —
	// when the VM carries a stamp that is neither the owner's nor replaceable,
	// or a stamp that cannot be read, and returns NotFound when the VM (with
	// ExpectedUUID) is not on the host. A provider that does not stamp answers
	// NotSupported.
	TransferOwner(ctx context.Context, req TransferOwnerRequest) error
}
