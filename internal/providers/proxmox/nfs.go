/*
Copyright 2025.

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
	"net/url"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// ADR-0006 Slice 4 — Proxmox NFS transport: KERNEL MOUNT, not libnfs.
//
// libvirt and vSphere stage to NFS via qemu-img's native nfs:// driver (libnfs).
// Proxmox cannot: pve-qemu-kvm is built WITHOUT the libnfs block driver, so the
// PVE node's qemu-img rejects nfs:// with "Unknown protocol 'nfs'" (lab-confirmed).
// Instead the Proxmox provider mounts the export with the node's kernel NFS client
// — exactly how PVE's first-class NFS storage mounts — and runs qemu-img against
// the mounted file. The mount is done as root over SSH (PVE nodes mount NFS as
// root routinely); the qemu-img I/O is dropped to the migration's nfs.uid/gid via
// setpriv, so AUTH_SYS presents the SAME identity libvirt/vSphere present through
// the libnfs URL — uid/gid stays uniform across providers and no_root_squash is
// not required on the export.

// nfsInMountRel resolves the staged object's path RELATIVE to the export root from
// the controller-built nfs:// URL. The URL is nfs://<server><export>[/<path>]/<key>
// (NFSURL joins export+path+key); stripping the export prefix yields the path that,
// appended to the kernel mount point, names the file. Rejects traversal and
// out-of-export paths (defence in depth atop the controller's C7' sanitisation).
func nfsInMountRel(rawURL, export string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse nfs url: %w", err)
	}
	rel := strings.TrimPrefix(u.Path, strings.TrimRight(export, "/"))
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.Contains(rel, "..") || strings.ContainsAny(rel, "\x00\n\r") {
		return "", fmt.Errorf("nfs url path %q is not a safe object within export %q", u.Path, export)
	}
	return rel, nil
}

// buildNFSKernelMountScript wraps innerCmd in a node-side shell script that mounts
// the NFS export read-write at a fresh temp dir ($MNT), runs innerCmd (which names
// the staged file as "$MNT"/<rel>), and unmounts + removes the temp dir on exit —
// success OR failure — via a trap. extraCleanup (e.g. removing a local temp) is
// appended to the trap. vers=3 matches the numeric-uid AUTH_SYS model the libnfs
// path uses (NFSv4 string id-mapping would re-map uids via idmapd).
func buildNFSKernelMountScript(server, export, innerCmd, extraCleanup string) string {
	target := proxmoxShellQuote(server + ":" + export)
	cleanup := `umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true`
	if extraCleanup != "" {
		cleanup += "; " + extraCleanup
	}
	return "set -e; MNT=$(mktemp -d /var/tmp/.virtrigaud-nfsmnt-XXXXXX); " +
		"trap '" + cleanup + "' EXIT; " +
		"mount -t nfs -o vers=3 " + target + " \"$MNT\"; " + innerCmd
}

// buildNFSKernelExportScript builds the TWO-STAGE node-side export: qemu-img as the
// SSH user (root) flattens the root-owned source disk to a local temp qcow2, then —
// after the NFS mount — the temp is copied onto the export AS the migration's
// uid/gid (setpriv). The split is unavoidable for a kernel mount: a single process
// cannot be root (to read the root-owned PVE source) and the share's uid (to write
// the export) at once — only libnfs can set the NFS identity independently of the
// process uid. The local temp is chmod 0644 so the dropped-privilege copy can read
// it, and is removed by the trap on exit.
func buildNFSKernelExportScript(server, export, rel, srcFormat, srcPath, hostTmp string, uid, gid *int64) string {
	qt := proxmoxShellQuote(hostTmp)
	flatten := fmt.Sprintf("TMP=%s; qemu-img convert -U -f %s -O qcow2 %s \"$TMP\"; chmod 0644 \"$TMP\"; ",
		qt, proxmoxShellQuote(srcFormat), proxmoxShellQuote(srcPath))
	cp := nfsSetprivPrefix(uid, gid) + `cp "$TMP" "$MNT"/` + proxmoxShellQuote(rel)
	// The mount wrapper provides "set -e", $MNT, the mount, and the trap; we prepend
	// the flatten via a leading subshell-free sequence and extend the trap to rm TMP.
	target := proxmoxShellQuote(server + ":" + export)
	cleanup := `umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true; rm -f "$TMP" 2>/dev/null || true`
	return "set -e; " + flatten +
		"MNT=$(mktemp -d /var/tmp/.virtrigaud-nfsmnt-XXXXXX); " +
		"trap '" + cleanup + "' EXIT; " +
		"mount -t nfs -o vers=3 " + target + " \"$MNT\"; " + cp
}

// nfsSetprivPrefix returns the setpriv prefix that runs the following command as
// the migration's nfs.uid/gid (AUTH_SYS identity), dropping supplementary groups.
// Both uid and gid must be set; otherwise qemu-img runs as the SSH user (root) and
// the export's squash policy decides access. setpriv is part of util-linux (always
// present on a PVE node).
func nfsSetprivPrefix(uid, gid *int64) string {
	if uid == nil || gid == nil {
		return ""
	}
	return fmt.Sprintf("setpriv --reuid %d --regid %d --clear-groups ", *uid, *gid)
}

// exportDiskToNFS implements the ADR-0006 Slice 4 Proxmox SOURCE path for the NFS
// backend via a kernel NFS mount (see the package note): the node flattens the
// source disk straight onto the mounted export as qcow2, with no provider-pod hop
// and no S3 client.
func (p *Provider) exportDiskToNFS(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}
	if p.ssh == nil {
		return nil, errors.NewUnavailable("migration SSH data plane not configured (set PROVIDER_ENDPOINT and SSH credentials)", nil)
	}

	nfsURL := strings.TrimSpace(req.DestinationUrl)
	if !strings.HasPrefix(nfsURL, "nfs://") {
		return nil, errors.NewInvalidSpec("nfs export destination must be an nfs:// URL, got %q", nfsURL)
	}
	opts, err := migration.ParseStorageOptions(req.StorageOptionsJson)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid nfs export storage options: %v", err)
	}
	if opts.Server == "" || opts.Export == "" {
		return nil, errors.NewInvalidSpec("nfs export storage options require server and export")
	}
	rel, err := nfsInMountRel(nfsURL, opts.Export)
	if err != nil {
		return nil, errors.NewInvalidSpec("nfs export: %v", err)
	}

	// Resolve the disk's PVE storage:volid via GetDiskInfo (returns it in Path).
	diskInfo, err := p.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return nil, errors.NewInternal("failed to get disk info for nfs export", err)
	}
	volid := strings.TrimSpace(diskInfo.Path)
	if volid == "" {
		return nil, errors.NewInvalidSpec("source disk %q has no resolvable storage volume", req.DiskId)
	}
	srcFormat := diskInfo.Format
	if srcFormat == "" {
		srcFormat = "qcow2"
	}
	srcPath, err := p.resolveNodeDiskPath(ctx, volid)
	if err != nil {
		return nil, errors.NewInternal("failed to resolve on-node disk path", err)
	}

	// Never log credentials — only the backend, volid, paths.
	p.logger.Info("Exporting disk from Proxmox node to NFS (kernel mount)",
		"backend", req.BackendType, "vm", req.VmId, "volid", volid, "src_path", srcPath,
		"destination", nfsURL)

	// Two-stage: flatten the root-owned source to a local temp as root (-U reads a
	// possibly-running source), then copy the temp onto the mounted export as the
	// migration's uid/gid (setpriv). See buildNFSKernelExportScript for why the
	// split is unavoidable with a kernel mount.
	hostTmp := proxmoxExportStagePath(req.VmId)
	script := buildNFSKernelExportScript(opts.Server, opts.Export, rel, srcFormat, srcPath, hostTmp, opts.UID, opts.GID)
	if _, stderr, err := p.ssh.runSSH(ctx, script); err != nil {
		return nil, errors.NewInternal(
			fmt.Sprintf("node-side nfs-mount qemu-img export failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}

	p.logger.Info("Source disk written to NFS export", "destination", nfsURL)

	return &providerv1.ExportDiskResponse{
		ExportId: fmt.Sprintf("export-proxmox-nfs-%s-%d", sanitizeProxmoxName(req.VmId), time.Now().Unix()),
		// qemu-img emits no byte SHA256 over the convert; NFS integrity is the
		// target-side qemu-img check on the staged qcow2 (ADR-0006 Slice 4).
	}, nil
}

// importDiskFromNFS implements the ADR-0006 Slice 4 Proxmox TARGET path via a
// kernel NFS mount: the node reads the staged qcow2 from the mounted export and
// writes a node-local stage file, which the Create path feeds to `qm importdisk`
// (Proxmox cannot importdisk a network URL — it takes a local file).
func (p *Provider) importDiskFromNFS(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}
	if p.ssh == nil {
		return nil, errors.NewUnavailable("migration SSH data plane not configured (set PROVIDER_ENDPOINT and SSH credentials)", nil)
	}

	nfsURL := strings.TrimSpace(req.SourceUrl)
	if !strings.HasPrefix(nfsURL, "nfs://") {
		return nil, errors.NewInvalidSpec("nfs import source must be an nfs:// URL, got %q", nfsURL)
	}
	opts, err := migration.ParseStorageOptions(req.StorageOptionsJson)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid nfs import storage options: %v", err)
	}
	if opts.Server == "" || opts.Export == "" {
		return nil, errors.NewInvalidSpec("nfs import storage options require server and export")
	}
	rel, err := nfsInMountRel(nfsURL, opts.Export)
	if err != nil {
		return nil, errors.NewInvalidSpec("nfs import: %v", err)
	}

	// NFS always stages qcow2 (the controller derives import format qcow2 for nfs).
	id := sanitizeProxmoxName(req.TargetName)
	if req.TargetName == "" {
		id = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}
	stagePath := proxmoxImportStagePath(id, "qcow2")

	p.logger.Info("Importing disk from NFS to Proxmox node (kernel mount; stage for qm importdisk at Create)",
		"id", id, "source", nfsURL, "stage_path", stagePath, "storage_hint", req.StorageHint)

	// Convert from the mounted export to the node-local stage file. The convert
	// reads NFS as the migration's uid/gid (setpriv); the stage lands in /var/tmp
	// (world-writable) and is consumed by `qm importdisk` at Create.
	mountFile := `"$MNT"/` + proxmoxShellQuote(rel)
	inner := nfsSetprivPrefix(opts.UID, opts.GID) +
		fmt.Sprintf("qemu-img convert -O qcow2 %s %s", mountFile, proxmoxShellQuote(stagePath))
	script := buildNFSKernelMountScript(opts.Server, opts.Export, inner, "")
	if _, stderr, err := p.ssh.runSSH(ctx, script); err != nil {
		p.cleanupNodeFile(stagePath)
		return nil, errors.NewInternal(
			fmt.Sprintf("node-side nfs-mount qemu-img import failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}

	// Validate the staged qcow2 (local file, runs as the SSH user — root reads any
	// file). ADR-0006 D5 structural integrity for NFS (no in-stream byte checksum).
	if _, stderr, err := p.ssh.runSSH(ctx, fmt.Sprintf("qemu-img check %s", proxmoxShellQuote(stagePath))); err != nil {
		p.cleanupNodeFile(stagePath)
		return nil, errors.NewInternal(
			fmt.Sprintf("qemu-img check failed on staged qcow2 %s (stderr: %s)", stagePath, strings.TrimSpace(stderr)), err)
	}

	p.logger.Info("Disk import from NFS staged on node (awaiting Create → qm importdisk)",
		"id", id, "stage_path", stagePath)

	return &providerv1.ImportDiskResponse{
		DiskId: id,
		Path:   stagePath,
		Task:   nil, // synchronous
		// No ActualSizeBytes/Checksum: qemu-img emits no byte SHA256 over the convert;
		// integrity is the node-side qemu-img check above (ADR-0006 Slice 4).
	}, nil
}
