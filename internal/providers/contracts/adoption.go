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

import "context"

// VMInfoUUIDKey is the VMInfo.ProviderRaw key under which a provider reports a
// listed VM's immutable identifier when it has one (libvirt: the domain UUID).
// The clustered adoption flow passes it back as
// TransferOwnerRequest.ExpectedUUID, so that a VM replaced since the list is
// never stamped.
const VMInfoUUIDKey = "uuid"

// TransferOwnerRequest asks a clustered provider to hand one VM on one of its
// hosts to a VirtualMachine by re-stamping the VM's owner (ADR-0007 Addendum
// A, slice 4). It is the manager-side mirror of the provider.v1
// TransferOwnerRequest message.
type TransferOwnerRequest struct {
	// VM addresses the VM: its ID (VMInfo.ID), the host it is on
	// (VMInfo.HostID) and, as Owner, the identity of the VirtualMachine that
	// takes it over. The provider stamps Owner onto the VM.
	VM VMRef
	// ReplaceableOwnerUIDs are the owner UIDs recorded on the VM
	// (VMInfoOwnerUIDKey) that the manager verified belong to no existing
	// VirtualMachine. Only these stamps may be replaced; empty takes over only
	// an unstamped VM (or one already stamped with Owner, an idempotent retry).
	ReplaceableOwnerUIDs []string
	// ExpectedUUID is the VM's immutable identifier (VMInfoUUIDKey). The
	// provider refuses to stamp a VM that no longer carries it.
	ExpectedUUID string
}

// OwnerTransferrer is an optional capability of a Provider: it re-stamps a VM
// on a clustered provider's host with the VirtualMachine that takes it over
// (TransferOwner), compare-and-swap. The manager gRPC client implements it;
// the adoption controller type-asserts a Provider to OwnerTransferrer, and uses
// it only when the provider reports Capabilities.SupportsRoutedAdoption, so
// the core Provider interface and the test fakes that do not adopt are
// unaffected. This mirrors the Cloner pattern.
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
	// ExpectedUUID) is not on the host.
	TransferOwner(ctx context.Context, req TransferOwnerRequest) error
}
