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

// StagedDiskDeleteRequest asks a provider to remove the staging object a
// VMMigration's ExportDisk wrote to an nfs or s3 backend (ADR-0011). It names
// the migration, never a location: the provider derives the object from
// Migration.Namespace, Migration.Name and Stage with the same layout the
// manager builds the export and import URLs from.
type StagedDiskDeleteRequest struct {
	// Migration is the VMMigration whose staged object is removed. Its
	// namespace and name name the object; its UID is informational.
	Migration ObjectIdentity
	// Stage is the staging stage ("export").
	Stage string
	// BackendType is the staging backend ("nfs" or "s3").
	BackendType string
	// StorageOptionsJSON is the same non-secret options JSON as the export's.
	StorageOptionsJSON string
	// Credentials are the backend credentials (s3). Secret material: never
	// logged, never placed in status or events.
	Credentials map[string]string
	// Layout is the staging layout the object was written with ("v1" or "v2";
	// empty means "v1"), as the manager recorded it when the export was sent.
	Layout string
}

// StagedDiskDeleteOutcome is what a DeleteStagedDisk did.
type StagedDiskDeleteOutcome string

const (
	// StagedDiskDeleted means the staged object existed and was removed.
	StagedDiskDeleted StagedDiskDeleteOutcome = "Deleted"
	// StagedDiskNotFound means no staged object existed.
	StagedDiskNotFound StagedDiskDeleteOutcome = "NotFound"
	// StagedDiskEmptied means the provider could not unlink the object (libnfs
	// through qemu-img) and truncated it to zero bytes: its data is gone and an
	// empty file with the staging name remains.
	StagedDiskEmptied StagedDiskDeleteOutcome = "Emptied"
)

// StagedDiskDeleter is an optional capability of a Provider: it removes a
// migration's staging object (ADR-0011). The manager gRPC client implements
// it; the VMMigration controller type-asserts a Provider to it AND requires the
// migration's backend in Capabilities.SupportedStagedDiskDeleteBackends before
// it sends the request, so in-process providers and test fakes without it are
// unaffected.
//
// It is idempotent: an object that does not exist is StagedDiskNotFound, not an
// error. A provider that does not implement the RPC answers a NotSupported
// error (IsNotSupported); a request it refuses is an InvalidSpec error
// (IsInvalidSpec); anything else may succeed on a retry.
type StagedDiskDeleter interface {
	// DeleteStagedDisk removes the staged object req names.
	DeleteStagedDisk(ctx context.Context, req StagedDiskDeleteRequest) (StagedDiskDeleteOutcome, error)
}
