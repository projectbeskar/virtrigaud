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
// reason when spec.storage.nfs sets uid or gid to 0 and a libvirt Provider is
// the migration's source or target. The migration fails at Validating, before
// any side effect.
const ReasonNFSRootIdentityNotAllowed = "NFSRootIdentityNotAllowed"

// nfsRootIdentityRefusal returns why an nfs migration must not run, or "" when
// it may. A libvirt host's qemu-img presents spec.storage.nfs.uid and gid to
// the NFS server as its AUTH_SYS identity, both when it writes the export and
// when it reads the import. AUTH_SYS identities are whatever the client
// asserts, so uid or gid 0 is root on an export that does not squash root
// (no_root_squash): enough to read or overwrite every file on the export,
// other migrations' staged disks included. Leaving them unset presents the
// provider's SSH user, as it always has.
//
// This is a typed check rather than a CRD minimum of 1 because it depends on
// the providers: the vSphere (pod-side) and Proxmox transports keep accepting
// the values they accept today, and tightening the v1beta1 schema would also
// reject updates to stored objects that carry uid 0.
func nfsRootIdentityRefusal(migration *infrav1beta1.VMMigration, sourceProvider, targetProvider *infrav1beta1.Provider) string {
	if migrationBackendType(migration) != storagemigration.BackendNFS {
		return ""
	}
	storage := migration.Spec.Storage
	if storage == nil || storage.NFS == nil {
		return ""
	}
	var sides []string
	if sourceProvider != nil && sourceProvider.Spec.Type == infrav1beta1.ProviderTypeLibvirt {
		sides = append(sides, "source")
	}
	if targetProvider != nil && targetProvider.Spec.Type == infrav1beta1.ProviderTypeLibvirt {
		sides = append(sides, "target")
	}
	if len(sides) == 0 {
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
	return fmt.Sprintf("%s set to 0 (root) is not allowed when the migration's %s is a libvirt Provider: the libvirt "+
		"host presents this identity to the NFS server, where it is root on an export without root_squash. Leave "+
		"uid and gid unset (the provider's SSH user) or set a non-root identity the export allows",
		strings.Join(fields, " and "), strings.Join(sides, " and "))
}
