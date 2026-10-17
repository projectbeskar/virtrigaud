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
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Landed-disk delete (rc2 R7).
//
// On Proxmox the disk a migration's ImportDisk lands is its node-side import
// stage (import_stage.go): "<proxmoxImportStageDir>/<ns>_<vm>-migrated.<ext>",
// named from the target VM. The target VM's Create copies it into the VM's own
// volume with `qm importdisk` and removes it, so a stage is never a disk a VM
// uses; one a migration left — it failed for good, or was deleted, before its
// target VM was created — is removed here (DeleteLandedDisk). Only the stage
// paths of the request's target VM (every importStageFormats format, each
// accepted by isImportStagePath) are touched; a recorded path, when sent, must
// be one of them. A stage a `qm importdisk` is still reading is unlinked
// safely: the reader keeps its open file. Legacy /var/tmp stages are never
// removed (import_stage.go).

// Markers the node-side landed-stage delete prints on stdout.
const (
	landedStageDeletedMarker = "VIRTRIGAUD_LANDED_DISK_DELETED"
	landedStageAbsentMarker  = "VIRTRIGAUD_LANDED_DISK_ABSENT"
)

const (
	// landedDiskInvalidMessage refuses a request without a valid target VM
	// identity.
	landedDiskInvalidMessage = "DeleteLandedDisk: the target VirtualMachine's namespace and name do not name a " +
		"Proxmox import stage"
	// landedDiskPathMismatchMessage refuses a recorded path that is not one
	// of the target VM's import stages. It never echoes the path.
	landedDiskPathMismatchMessage = "DeleteLandedDisk: the recorded landing path is not the target VirtualMachine's " +
		"import stage"
	// landedDiskNoSSHMessage refuses a delete the provider cannot perform: the
	// migration SSH data plane is not configured.
	landedDiskNoSSHMessage = "DeleteLandedDisk: the migration SSH data plane is not configured, so the node-side " +
		"import stage cannot be checked or removed"
	// landedDiskImportInFlightMessage is the retryable answer while an import,
	// or another delete, of the same stage runs in this provider.
	landedDiskImportInFlightMessage = "DeleteLandedDisk: an import of this VM's stage is in progress; will retry"
	// landedDiskFailedMessage is the retryable answer of a node-side delete
	// that failed; the cause goes to the provider log (debug) only.
	landedDiskFailedMessage = "DeleteLandedDisk: the node-side import stage could not be removed; the cause is in " +
		"the provider log; will retry"
)

// DeleteLandedDisk removes the import stage a migration's ImportDisk landed
// for req.target_vm (see above): DELETED when one existed, NOT_FOUND
// otherwise. Without the SSH data plane it can check nothing and answers
// FailedPrecondition (never NOT_FOUND); while an import of the same stage
// runs in this process it answers Unavailable (landing_guard.go).
func (p *Provider) DeleteLandedDisk(ctx context.Context, req *providerv1.DeleteLandedDiskRequest) (*providerv1.DeleteLandedDiskResponse, error) {
	tv := req.GetTargetVm()
	if tv.GetNamespace() == "" || tv.GetName() == "" || validStageIdentity(tv.GetNamespace(), tv.GetName()) != nil {
		return nil, status.Error(codes.InvalidArgument, landedDiskInvalidMessage)
	}
	paths := make([]string, 0, len(importStageFormats))
	for _, format := range importStageFormats {
		path := importStagePath(tv.GetNamespace(), tv.GetName(), format)
		if !isImportStagePath(path) {
			return nil, status.Error(codes.InvalidArgument, landedDiskInvalidMessage)
		}
		paths = append(paths, path)
	}
	if recorded := req.GetPath(); recorded != "" && !slices.Contains(paths, recorded) {
		return nil, status.Error(codes.InvalidArgument, landedDiskPathMismatchMessage)
	}
	migrationRef := req.GetMigration().GetNamespace() + "/" + req.GetMigration().GetName()
	notFound := &providerv1.DeleteLandedDiskResponse{Outcome: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_NOT_FOUND}
	if p.ssh == nil {
		// Nothing can be checked, so nothing is claimed: not NOT_FOUND (which
		// the manager would record as deleted) but a refusal.
		p.logger.Warn("Cannot delete a migration's import stage: the migration SSH data plane is not configured", "migration", migrationRef)
		return nil, status.Error(codes.FailedPrecondition, landedDiskNoSSHMessage)
	}
	release, ok := stageGuards.beginDelete(importStageName(tv.GetNamespace(), tv.GetName()))
	if !ok {
		return nil, status.Error(codes.Unavailable, landedDiskImportInFlightMessage)
	}
	defer release()
	stdout, stderr, err := p.ssh.runSSH(ctx, buildLandedStageDeleteScript(paths))
	switch {
	case err != nil:
		p.logger.Warn("Could not remove a migration's node-side import stage", "migration", migrationRef)
		p.logger.Debug("Import stage removal failure cause", "migration", migrationRef, "error", err.Error(),
			"stderr", strings.TrimSpace(stderr))
		return nil, status.Error(codes.Unavailable, landedDiskFailedMessage)
	case strings.Contains(stdout, landedStageDeletedMarker):
		p.logger.Info("Migration landed disk (node-side import stage) removed", "migration", migrationRef)
		return &providerv1.DeleteLandedDiskResponse{Outcome: providerv1.LandedDiskDeleteOutcome_LANDED_DISK_DELETE_OUTCOME_DELETED}, nil
	case strings.Contains(stdout, landedStageAbsentMarker):
		return notFound, nil
	default:
		p.logger.Debug("Import stage removal answered without a marker", "migration", migrationRef, "stdout", stdout)
		return nil, status.Error(codes.Unavailable, landedDiskFailedMessage)
	}
}

// buildLandedStageDeleteScript is the node-side script that removes every
// existing path of paths (each shell-quoted, never interpolated unquoted) and
// prints landedStageDeletedMarker when at least one existed,
// landedStageAbsentMarker when none did. A path that is still there after
// `rm -f` (a failed unlink) exits 4 without a marker. `rm` removes a symbolic
// link itself, never what it points to.
func buildLandedStageDeleteScript(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, proxmoxShellQuote(p))
	}
	return fmt.Sprintf(`found=0; for f in %s; do `+
		`if [ -e "$f" ] || [ -L "$f" ]; then rm -f -- "$f" || exit 4; `+
		`if [ -e "$f" ] || [ -L "$f" ]; then exit 4; fi; found=1; fi; done; `+
		`if [ "$found" = 1 ]; then echo %s; else echo %s; fi`,
		strings.Join(quoted, " "), landedStageDeletedMarker, landedStageAbsentMarker)
}
