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

package libvirt

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A disk-only snapshot whose overlay file already exists (V12, v0.4.0 lab).
//
// `virsh snapshot-create-as --disk-only` without a --diskspec file names each
// disk's new overlay after the disk's current source, its last extension
// replaced by the snapshot's name: <dir>/<domain>-disk.<snapshot> for the
// primary disk (libvirt's virDomainSnapshotDefAssignExternalNames). When that
// file already exists — an earlier snapshot of the same name left it on the
// host, for example the merged overlay a disk-only snapshot's delete leaves
// while a memory snapshot is stored in it (V11) — libvirt refuses the
// snapshot before it changes anything (qemuSnapshotPrepareDiskExternal:
// "unsupported configuration: external snapshot file for disk vda already
// exists and is not a block device: <path>"; a file of size 0 is reused).
// The answer used to be a generic error: the manager could not read it as
// "nothing was done" and re-sent the create for ever (clustered) or answered
// "not created" with no hint (single host).
//
// The create now probes, before it runs, the overlay path of every disk the
// snapshot would cover, and refuses a collision as a definite Conflict
// (AlreadyExists on the wire, constant text, no host path): nothing was
// created, and the same request can never succeed — the VMSnapshot fails, and
// the operator uses another snapshot name or removes the leftover file once
// nothing references it (docs/libvirt-clones.md). The leftover file is never
// removed or overwritten here: it may hold another snapshot's data. A probe
// that cannot see the file (the provider's SSH user cannot stat it) falls
// through to libvirt, whose refusal is classified the same way after the
// fact (isOverlayExistsRefusal).

// wireSnapshotOverlayExists is the requester-facing text of the refusal. It
// names no host path and fits the manager's 256-byte status detail with the
// operation prefix the manager adds.
const wireSnapshotOverlayExists = "snapshot create refused: an overlay file this disk-only snapshot would create " +
	"already exists on the libvirt host, left by an earlier snapshot of the same name; nothing was created. " +
	"Use another snapshot name"

// overlayCollisionScript is the fixed `sh -c` script of the probe: for each
// path argument (1-based) it prints its index when the path is what libvirt
// refuses as an existing overlay — it exists, is not a block device and is
// not empty (stat follows symbolic links, as libvirt's stat does).
const overlayCollisionScript = `i=0; for f in "$@"; do i=$((i+1)); if [ -s "$f" ] && [ ! -b "$f" ]; then echo "$i"; fi; done`

// snapshotDiskModesWithoutOverlay are the per-disk snapshot modes for which
// libvirt makes no external overlay.
var snapshotDiskModesWithoutOverlay = map[string]bool{"no": true, "internal": true, "manual": true}

// externalOverlayPath returns the overlay libvirt names for a disk whose
// current source is source in a disk-only snapshot named snapshot: the source
// with its last extension (a '.' in the last path element) dropped, then
// "." and the snapshot name.
func externalOverlayPath(source, snapshot string) string {
	base := source
	if i := strings.LastIndexByte(base, '.'); i >= 0 && !strings.ContainsRune(base[i:], '/') {
		base = base[:i]
	}
	return base + "." + snapshot
}

// snapshotOverlayTargets returns, in document order, the target and overlay
// path of every disk of d that a disk-only snapshot named snapshot gives an
// external overlay of libvirt's default name: a writable, file-backed disk
// that is not a removable medium and whose own snapshot mode does not opt it
// out. Read-only disks and media are excluded by the snapshot itself
// (snapshotExcludedTargets); a network, block or volume disk gets no default
// overlay name (libvirt refuses such a snapshot itself).
func (d *domainDisksDoc) snapshotOverlayTargets(snapshot string) (targets, paths []string) {
	for _, e := range d.Devices.Disks {
		if e.excludedFromSnapshots() || e.file() == "" || snapshotDiskModesWithoutOverlay[strings.TrimSpace(e.Snapshot)] {
			continue
		}
		targets = append(targets, e.target())
		paths = append(paths, externalOverlayPath(e.file(), snapshot))
	}
	return targets, paths
}

// refuseSnapshotOverlayCollision probes, on h's host, the overlay files a
// disk-only snapshot named snapshot of domain (described by doc) would create,
// and refuses (wireSnapshotOverlayExists, a contracts Conflict) when one
// already exists. The colliding disk and path are logged here only. A probe
// that cannot run, or answers anything but indexes of the probed paths, is an
// error: nothing was created, and the create is not run.
func refuseSnapshotOverlayCollision(ctx context.Context, h hostCommandRunner, domain, snapshot string, doc *domainDisksDoc) error {
	targets, paths := doc.snapshotOverlayTargets(snapshot)
	if len(paths) == 0 {
		return nil
	}
	res, err := runHost(ctx, h, append([]string{"sh", "-c", overlayCollisionScript, "sh"}, paths...)...)
	if err != nil {
		return fmt.Errorf("check for existing snapshot overlay files: %w", err)
	}
	var collided []string
	for _, line := range strings.Fields(res.Stdout) {
		i, err := strconv.Atoi(line)
		if err != nil || i < 1 || i > len(paths) {
			return fmt.Errorf("check for existing snapshot overlay files: unexpected answer %q", line)
		}
		collided = append(collided, fmt.Sprintf("%s (%s)", targets[i-1], paths[i-1]))
	}
	if len(collided) == 0 {
		return nil
	}
	log.Printf("ERROR Refusing snapshot %s of domain %s: the overlay file(s) it would create already exist: %s. "+
		"They were left by an earlier snapshot of the same name and may hold another snapshot's data; nothing is "+
		"removed or overwritten. Use another snapshot name, or remove the file once no domain and no snapshot "+
		"references it", snapshot, domain, strings.Join(collided, ", "))
	return contracts.NewConflictError(wireSnapshotOverlayExists, errSnapshotOverlayExists)
}

// errSnapshotOverlayExists is the cause of the overlay-collision refusal.
var errSnapshotOverlayExists = stderrors.New("a snapshot overlay file already exists")

// isOverlayExistsRefusal reports whether err is a virsh command whose first
// "error:" line is libvirt's refusal of an external snapshot whose overlay
// file already exists (qemuSnapshotPrepareDiskExternal), raised while the
// snapshot is prepared, before anything is changed.
func isOverlayExistsRefusal(err error) bool {
	var ve *VirshError
	if !stderrors.As(err, &ve) {
		return false
	}
	line := firstErrorLine(ve.Stderr)
	return strings.Contains(line, "external snapshot file for disk") && strings.Contains(line, "already exists")
}

// overlayExistsRefusal answers libvirt's own refusal of a snapshot create
// whose overlay file already exists (isOverlayExistsRefusal) with the
// overlay-collision Conflict, when it is proven: virsh ended on its own with
// an exit status, did not lose libvirtd, and no snapshot of the name exists
// on domain afterwards. It returns nil otherwise (the caller keeps its own
// answer).
func overlayExistsRefusal(ctx context.Context, conn libvirtConn, domain, snapshot string, err error) error {
	if !isOverlayExistsRefusal(err) || !hostCommandEnded(ctx, err) || virshConnectionLost(err) {
		return nil
	}
	if exists, listErr := conn.snapshotExists(ctx, domain, snapshot); listErr != nil || exists {
		return nil
	}
	log.Printf("ERROR libvirt refused snapshot %s of domain %s: an overlay file it would create already exists "+
		"(the provider's check could not see it): %v", snapshot, domain, err)
	return contracts.NewConflictError(wireSnapshotOverlayExists, errSnapshotOverlayExists)
}
