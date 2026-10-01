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
	"fmt"
	"strings"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// ReasonNFSRootIdentityNotAllowed is the VMMigration Validating condition
// reason when spec.storage.nfs sets uid or gid to 0. The migration fails at
// Validating, before any read of its source or providers and before any side
// effect, whatever its source and target provider types.
const ReasonNFSRootIdentityNotAllowed = "NFSRootIdentityNotAllowed"

// nfsRootIdentityRefusal returns why an nfs migration must not run, or "" when
// it may: spec.storage.nfs.uid or gid is 0. Every provider's NFS client
// presents that identity to the server as its AUTH_SYS credential — libvirt's
// host qemu-img and vSphere's pod qemu-img through the libnfs URL, Proxmox's
// node through setpriv — and AUTH_SYS identities are whatever the client
// asserts, so uid or gid 0 is root on an export that does not squash root
// (no_root_squash): enough to read or overwrite every file on the export,
// other migrations' staged disks included. A dedicated non-zero uid/gid that
// owns the export does what the field is for.
//
// This is a typed check rather than a CRD minimum of 1: tightening the
// v1beta1 schema is a breaking API change (an ADR under the project's rules),
// and the API server's "should be greater than or equal to 1" would not tell
// the requester why. The condition does.
func nfsRootIdentityRefusal(migration *infrav1beta1.VMMigration) string {
	if migrationBackendType(migration) != storagemigration.BackendNFS {
		return ""
	}
	storage := migration.Spec.Storage
	if storage == nil || storage.NFS == nil {
		return ""
	}
	var fields []string
	if storage.NFS.UID != nil && *storage.NFS.UID == 0 {
		fields = append(fields, "spec.storage.nfs.uid")
	}
	if storage.NFS.GID != nil && *storage.NFS.GID == 0 {
		fields = append(fields, "spec.storage.nfs.gid")
	}
	if len(fields) == 0 {
		return ""
	}
	return fmt.Sprintf("%s set to 0 (root) is not allowed: the provider presents this identity to the NFS server, "+
		"where it is root on an export without root_squash. Use a dedicated non-zero uid/gid that owns the export, "+
		"or leave them unset", strings.Join(fields, " and "))
}
