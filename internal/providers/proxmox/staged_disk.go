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

package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Markers the node-side delete script prints on stdout: whether the staged
// file existed (and was removed) or was absent.
const (
	nfsStagedDeletedMarker = "VIRTRIGAUD_STAGED_DISK_DELETED"
	nfsStagedAbsentMarker  = "VIRTRIGAUD_STAGED_DISK_ABSENT"
)

// DeleteStagedDisk removes a VMMigration's staging object (ADR-0011). The
// object is derived from the migration identity (migration.ResolveStagedDisk;
// the request carries no location). s3 objects are removed from the provider
// pod, the S3 client of the relay path; nfs files are unlinked on the PVE node
// through the same kernel mount, and as the same uid/gid, as the NFS import
// (nfs.go). Nothing is logged that names a credential; the location is logged
// at debug only.
//
// When the request names the migration's target VM (target_vm), the node-side
// import stage ImportDisk wrote for that VM is removed first
// (deleteImportStages): the outcome reports the staged object, and a stage
// that could not be removed fails the call (retryable) before the object is
// touched, so a retry still finds it.
func (p *Provider) DeleteStagedDisk(ctx context.Context, req *providerv1.DeleteStagedDiskRequest) (*providerv1.DeleteStagedDiskResponse, error) {
	disk, err := migration.ResolveStagedDisk(req)
	if err != nil {
		return nil, err
	}
	migrationRef := req.GetMigration().GetNamespace() + "/" + req.GetMigration().GetName()
	p.logger.Debug("Deleting a migration's staged disk", "migration", migrationRef, "backend", disk.Backend, "key", disk.Key)

	if err := p.deleteImportStages(ctx, req.GetTargetVm()); err != nil {
		p.logger.Warn("Could not remove a migration's node-side import stage", "migration", migrationRef,
			"reason", status.Convert(err).Message())
		p.logger.Debug("Import stage removal failure cause", "migration", migrationRef, "error", err.Error())
		return nil, err
	}

	var outcome providerv1.StagedDiskDeleteOutcome
	switch disk.Backend {
	case migration.BackendS3:
		outcome, err = migration.DeleteS3StagedDisk(ctx, disk, req.GetCredentials())
	default: // migration.BackendNFS: ResolveStagedDisk accepts nothing else.
		outcome, err = p.deleteNFSStagedDisk(ctx, disk)
	}
	if err != nil {
		// The status message names no location; the cause (stderr, host,
		// path) is logged at debug only.
		p.logger.Warn("Could not delete a migration's staged disk", "migration", migrationRef, "backend", disk.Backend,
			"reason", status.Convert(err).Message())
		p.logger.Debug("Staged disk delete failure cause", "migration", migrationRef, "error", err.Error())
		return nil, err
	}
	p.logger.Info("Migration staged disk removed", "migration", migrationRef, "backend", disk.Backend, "outcome", outcome.String())
	return &providerv1.DeleteStagedDiskResponse{Outcome: outcome}, nil
}

// deleteImportStages removes the node-side import stages of the VM target
// names (importStagePath in every importStageFormats format; `rm -f`, so an
// absent stage is not an error). It removes nothing when target is unset, and
// only paths isImportStagePath accepts: never a legacy /var/tmp stage, the
// stage directory, or anything else on the node. Without the SSH data plane
// nothing can have been staged (ImportDisk needs it), so nothing is removed.
//
// A Create of that VM may be running: `qm importdisk` keeps reading a stage it
// has opened after it is unlinked; one that has not reached importdisk fails
// its stage check and removes its VM shell. The manager names the target VM
// only for a migration that is done (Ready or failed for good) or being
// deleted, that sent an import, and when no other in-flight migration targets
// the same VM.
func (p *Provider) deleteImportStages(ctx context.Context, target *providerv1.ObjectIdentity) error {
	if target.GetNamespace() == "" && target.GetName() == "" {
		return nil
	}
	if err := validStageIdentity(target.GetNamespace(), target.GetName()); err != nil {
		return &migration.StagedDiskError{Code: codes.InvalidArgument,
			Msg: "DeleteStagedDisk: the target VM cannot name a Proxmox import stage", Cause: err}
	}
	if p.ssh == nil {
		p.logger.Warn("The migration SSH data plane is not configured: no node-side import stage to remove")
		return nil
	}
	quoted := make([]string, 0, len(importStageFormats))
	for _, format := range importStageFormats {
		path := importStagePath(target.GetNamespace(), target.GetName(), format)
		if !isImportStagePath(path) {
			return &migration.StagedDiskError{Code: codes.InvalidArgument,
				Msg: "DeleteStagedDisk: the target VM cannot name a Proxmox import stage"}
		}
		quoted = append(quoted, proxmoxShellQuote(path))
	}
	if _, stderr, err := p.ssh.runSSH(ctx, "rm -f -- "+strings.Join(quoted, " ")); err != nil {
		return &migration.StagedDiskError{Code: codes.Unavailable,
			Msg:   "DeleteStagedDisk: the node-side import stage removal failed; the cause is in the provider log",
			Cause: fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr))}
	}
	return nil
}

// deleteNFSStagedDisk unlinks the staged file on the export through a kernel
// NFS mount on the node (pve-qemu-kvm has no libnfs driver, nfs.go). The file
// is the request-derived key under the export's path, resolved relative to the
// mount by nfsInMountRel (which rejects traversal a second time). The check and
// the unlink run as the migration's uid/gid (setpriv) when they are set, the
// identity the export wrote the file with.
func (p *Provider) deleteNFSStagedDisk(ctx context.Context, disk migration.StagedDisk) (providerv1.StagedDiskDeleteOutcome, error) {
	unspecified := providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_UNSPECIFIED
	if p.ssh == nil {
		return unspecified, &migration.StagedDiskError{Code: codes.Unavailable,
			Msg: "DeleteStagedDisk: the migration SSH data plane is not configured (set PROVIDER_ENDPOINT and SSH credentials)"}
	}
	rel, err := nfsInMountRel(disk.NFSURL, disk.Options.Export)
	if err != nil {
		return unspecified, &migration.StagedDiskError{Code: codes.InvalidArgument,
			Msg: "DeleteStagedDisk: the staged nfs object is not a safe path within the export", Cause: err}
	}
	stdout, stderr, err := p.ssh.runSSH(ctx, buildNFSStagedDeleteScript(disk.Options.Server, disk.Options.Export, rel,
		disk.Options.UID, disk.Options.GID))
	if err != nil {
		return unspecified, &migration.StagedDiskError{Code: codes.Unavailable,
			Msg:   "DeleteStagedDisk: the node-side nfs delete failed; the cause is in the provider log",
			Cause: fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr))}
	}
	switch {
	case strings.Contains(stdout, nfsStagedDeletedMarker):
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_DELETED, nil
	case strings.Contains(stdout, nfsStagedAbsentMarker):
		return providerv1.StagedDiskDeleteOutcome_STAGED_DISK_DELETE_OUTCOME_NOT_FOUND, nil
	default:
		return unspecified, &migration.StagedDiskError{Code: codes.Internal,
			Msg:   "DeleteStagedDisk: the node-side nfs delete printed no result",
			Cause: errors.New(strings.TrimSpace(stdout))}
	}
}

// buildNFSStagedDeleteScript builds the node-side script that mounts the
// export (buildNFSKernelMountScript: a fresh temp mount point, unmounted and
// removed on exit) and runs nfsStagedDeleteCmd against the staged file at rel.
func buildNFSStagedDeleteScript(server, export, rel string, uid, gid *int64) string {
	return buildNFSKernelMountScript(server, export, nfsStagedDeleteCmd(rel, uid, gid), "")
}

// nfsStagedDeleteCmd is the command, run with $MNT set to the mounted export,
// that removes the staged file at "$MNT"/rel — as the migration's uid/gid when
// set (setpriv). It refuses when the file's directory cannot be searched (so a
// permission problem is never mistaken for an absent file), and prints
// nfsStagedDeletedMarker only after it removed an existing file (or symbolic
// link: rm removes the link, never its target) AND checked it is gone: an rm
// that fails exits 4, a file still there exits 5, so a failed delete is never
// reported deleted. It prints nfsStagedAbsentMarker when there is no file. rel is shell-quoted and passed as an argument, never spliced into the
// inner script.
func nfsStagedDeleteCmd(rel string, uid, gid *int64) string {
	inner := `d=$(dirname -- "$1"); ` +
		`if [ ! -x "$d" ]; then echo "staging directory is not searchable" >&2; exit 3; fi; ` +
		`if [ -e "$1" ] || [ -L "$1" ]; then ` +
		`rm -f -- "$1" || exit 4; ` +
		`if [ -e "$1" ] || [ -L "$1" ]; then echo "staged file is still present after rm" >&2; exit 5; fi; ` +
		`echo ` + nfsStagedDeletedMarker + `; ` +
		`else echo ` + nfsStagedAbsentMarker + `; fi`
	return nfsSetprivPrefix(uid, gid) + "sh -c " + proxmoxShellQuote(inner) + ` sh "$MNT"/` + proxmoxShellQuote(rel)
}
