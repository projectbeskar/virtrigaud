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

// LandedDiskDeleteRequest asks a migration's target provider to remove the
// disk the migration's ImportDisk landed for TargetVM, when no VM uses it (rc2
// R7). It names the target VM, never a free-form location: the provider
// derives the landed disk from TargetVM with the rule its ImportDisk names it
// with, and Path — when set — must be exactly that disk.
type LandedDiskDeleteRequest struct {
	// Migration is the VMMigration whose import landed the disk;
	// informational (audit, logs).
	Migration ObjectIdentity
	// TargetVM is the VirtualMachine the import landed the disk for
	// (namespace and name). Required.
	TargetVM ObjectIdentity
	// Path is the landing path the import returned (status.diskInfo.
	// targetPath), or empty when no import result was recorded.
	Path string
	// TargetHostID is the host of a CLUSTERED target provider the import
	// landed the disk on (the recorded import target host; wire:
	// target_host_id, ADR-0007 Addendum B, B4). Empty for single-host and
	// thin-client providers; a clustered provider refuses an empty value.
	TargetHostID string
}

// LandedDiskDeleteOutcome is what a DeleteLandedDisk did.
type LandedDiskDeleteOutcome string

const (
	// LandedDiskDeleted means the landed disk existed and was removed.
	LandedDiskDeleted LandedDiskDeleteOutcome = "Deleted"
	// LandedDiskNotFound means no landed disk existed.
	LandedDiskNotFound LandedDiskDeleteOutcome = "NotFound"
)

// LandedDiskDeleter is an optional capability of a Provider: it removes the
// disk a migration's import landed for a target VM that does not use it (rc2
// R7). The manager gRPC client implements it; the VMMigration controller
// type-asserts a Provider to it AND requires
// Capabilities.SupportsLandedDiskDelete before it sends the request, so
// in-process providers and test fakes without it are unaffected.
//
// It is idempotent: a disk that does not exist is LandedDiskNotFound, not an
// error. A disk a VM uses is refused with a Conflict marked ErrVMDiskInUse
// (IsVMDiskInUse) and left in place. A provider that does not implement the
// RPC answers a NotSupported error (IsNotSupported); a request it refuses is
// an InvalidSpec error (IsInvalidSpec); anything else may succeed on a retry.
type LandedDiskDeleter interface {
	// DeleteLandedDisk removes the landed disk req names.
	DeleteLandedDisk(ctx context.Context, req LandedDiskDeleteRequest) (LandedDiskDeleteOutcome, error)
}
