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
	"encoding/base64"
	stderrors "errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage"
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

// nfsKernelMountOptions are the options of every kernel NFS mount of a
// migration's (tenant-controlled) export: vers=3 matches the numeric-uid
// AUTH_SYS model the libnfs path uses (NFSv4 string id-mapping would re-map
// uids via idmapd); nosymfollow makes the node never follow a symbolic link
// the NFS server serves — a link at the staging name to /etc/pve/priv/* or
// /root/.ssh/* would otherwise be resolved on the NODE, as root (kernel 5.10+,
// PVE 7+); nosuid, nodev and noexec make nothing on the export a privilege or
// a device on the node.
const nfsKernelMountOptions = "vers=3,nosymfollow,nosuid,nodev,noexec"

// buildNFSKernelMountScript wraps innerCmd in a node-side shell script that mounts
// the NFS export read-write at a fresh temp dir ($MNT), runs innerCmd (which names
// the staged file as "$MNT"/<rel>), and unmounts + removes the temp dir on exit —
// success OR failure — via a trap. extraCleanup (e.g. removing a local temp) is
// appended to the trap. The mount uses nfsKernelMountOptions.
func buildNFSKernelMountScript(server, export, innerCmd, extraCleanup string) string {
	target := proxmoxShellQuote(server + ":" + export)
	cleanup := `umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true`
	if extraCleanup != "" {
		cleanup += "; " + extraCleanup
	}
	return "set -e; MNT=$(mktemp -d /var/tmp/.virtrigaud-nfsmnt-XXXXXX); " +
		"trap '" + cleanup + "' EXIT; " +
		"mount -t nfs -o " + nfsKernelMountOptions + " " + target + " \"$MNT\"; " + innerCmd
}

// nfsNotRegularFileMessage is what nfsStageCopy prints (stderr) when the
// staged object is not a regular file.
const nfsNotRegularFileMessage = "staged object is not a regular file"

// nfsNotRegularFileExit is nfsStageCopy's exit status for a staged object
// that is not a regular file; qemuImgFailedExit is qemu-img's exit status
// when it cannot open or read an image (as opposed to ssh's 255).
const (
	nfsNotRegularFileExit = 6
	qemuImgFailedExit     = 1
	// nfsTooLargeExit and nfsNoSpaceExit are nfsStageCopy's exit statuses
	// for a staged object above the copy bound (or that grew past its
	// measured size during the copy), and for one that does not fit in the
	// node's free space.
	nfsTooLargeExit = 7
	nfsNoSpaceExit  = 8
)

// remoteExitCode is the exit status of the remote command behind a runSSH
// error, or -1 when there is none (ssh could not run it).
func remoteExitCode(err error) int {
	var exitErr *exec.ExitError
	if stderrors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// checkStagedNodeImage checks the header of a staged migration image on the
// node (a node-local file the tenant can no longer change) before anything
// converts or imports it: its leading bytes are parsed first, without qemu
// (migration.CheckStagedImageBytes), then it must probe as wantFormat and
// reference no other file (migration.CheckStagedImageHeader). It is read
// WITHOUT -f, exactly as `qm importdisk` (which probes its source) reads it at
// Create. A declared virtual size above maxVirtualBytes (when positive) is
// refused. A header that is refused, or that qemu-img cannot read, is
// InvalidSpec; an SSH failure is Internal (retryable).
func (p *Provider) checkStagedNodeImage(ctx context.Context, path, wantFormat string, maxVirtualBytes int64) error {
	// 1. Parse the leading bytes WITHOUT qemu: qemu opens a qcow2 external
	// data file and VMDK extents while it reads a header, so `qemu-img info`
	// on a crafted image could open a device node, a node file or an
	// nbd:/http: target as root before anything refuses it.
	qp := proxmoxShellQuote(path)
	headOut, stderr, err := p.ssh.runSSH(ctx, fmt.Sprintf("head -c %d -- %s >/dev/null && head -c %d -- %s | base64 -w0",
		migration.StagedImageHeadBytes, qp, migration.StagedImageHeadBytes, qp))
	if err != nil {
		return errors.NewInternal(
			fmt.Sprintf("reading the staged image header failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}
	head, err := base64.StdEncoding.DecodeString(strings.TrimSpace(headOut))
	if err != nil {
		return errors.NewInternal("decoding the staged image header failed", err)
	}
	if _, err := migration.CheckStagedImageBytes(head, wantFormat, maxVirtualBytes); err != nil {
		p.logger.Warn("Refused a staged migration image", "reason", err.Error())
		p.logger.Debug("Refused staged migration image", "path", path)
		return errors.NewInvalidSpec("refused the staged migration image: %v", err)
	}
	// 2. Only then ask qemu-img (no network namespace, shared read) — probed
	// without -f, exactly as `qm importdisk` reads it at Create.
	infoOut, stderr, err := p.ssh.runSSH(ctx, "unshare -n -- qemu-img info -U --output=json -- "+qp)
	if err != nil {
		if remoteExitCode(err) == qemuImgFailedExit {
			return errors.NewInvalidSpec("the staged migration image is not a readable disk image")
		}
		return errors.NewInternal(
			fmt.Sprintf("qemu-img info on the staged image failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}
	if err := migration.CheckStagedImageHeader(infoOut, wantFormat, path); err != nil {
		p.logger.Warn("Refused a staged migration image", "reason", err.Error())
		p.logger.Debug("Refused staged migration image", "path", path)
		return errors.NewInvalidSpec("refused the staged migration image: %v", err)
	}
	return nil
}

// stagedDownloadBound is the most bytes an S3 staged object may be downloaded
// into dir on the node: its reported size, refused (constant) above the import
// limit (InvalidSpec) or when dir's filesystem cannot hold
// StagedCopySpaceFactor times it (ResourceExhausted).
func (p *Provider) stagedDownloadBound(ctx context.Context, s3client storage.Storage, sourceURL, dir string, requestLimit int64) (int64, error) {
	limit, err := migration.StagedCopyLimit(requestLimit)
	if err != nil {
		return 0, errors.NewInternal("invalid staged import size limit", err)
	}
	meta, err := s3client.GetMetadata(ctx, sourceURL)
	if err != nil {
		return 0, errors.NewInternal("failed to read the size of the staged s3 object", err)
	}
	// The stage directory is created by the stage script on first use: measure
	// the filesystem of its nearest existing ancestor.
	out, stderr, err := p.ssh.runSSH(ctx, `d=`+proxmoxShellQuote(dir)+`; while [ ! -d "$d" ]; do d=$(dirname -- "$d"); done; `+
		`df -P -B1 -- "$d" | awk 'NR==2 {print $4}'`)
	if err != nil {
		return 0, errors.NewInternal(fmt.Sprintf("reading the node's free space failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}
	free, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, errors.NewInternal("parsing the node's free space failed", err)
	}
	switch err := migration.CheckStagedCopy(meta.Size, limit, free); {
	case stderrors.Is(err, migration.ErrStagedObjectTooLarge):
		return 0, errors.NewInvalidSpec("s3 import: %s", err.Error())
	case err != nil:
		return 0, status.Error(codes.ResourceExhausted, "s3 import: "+err.Error())
	}
	return meta.Size, nil
}

// nfsStageCopyMeasure is the script nfsStageCopy runs first, as the
// migration's uid/gid when set (the NFS server decides what that identity may
// see), with "$1" the staged file: it refuses a staged object that is not a
// regular file or is a symbolic link (exit 6), and prints its size. It reads
// no data. The bound and free-space checks run in the root shell
// (nfsStageCopy): the stage directory's ancestors are created under umask 077,
// so a dropped-privilege df could not reach it.
var nfsStageCopyMeasure = fmt.Sprintf(`f="$1"; `+
	`if [ ! -f "$f" ] || [ -L "$f" ]; then echo "%s" >&2; exit %d; fi; `+
	`stat -c %%s -- "$f"`,
	nfsNotRegularFileMessage, nfsNotRegularFileExit)

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
		"mount -t nfs -o " + nfsKernelMountOptions + " " + target + " \"$MNT\"; " + cp
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
	// The stage is named after the target VirtualMachine (import_stage.go).
	id, stagePath, err := importStageForRequest(req, "qcow2")
	if err != nil {
		return nil, err
	}

	p.logger.Info("Importing disk from NFS to Proxmox node (kernel mount; stage for qm importdisk at Create)",
		"id", id, "source", nfsURL, "stage_path", stagePath, "storage_hint", req.StorageHint)

	// SECURITY: the staged object comes from an NFS server the tenant controls.
	// It is copied byte for byte, ONCE, into a root-owned temp in the root-only
	// stage directory and renamed onto the stage (buildNFSImportStageScript):
	// the copy refuses a non-regular file (the mount is also nosymfollow), is
	// bounded by the object's measured size and by the import's disk-size limit
	// (#382), needs room for twice that on the node, and the dropped-privilege
	// reader holds only a pipe (nfsStageCopy). The stage's header is then parsed
	// without qemu and checked (checkStagedNodeImage) before anything reads it
	// as an image: a qcow2 backing file, data file or extent could otherwise
	// make the node's qemu-img (root) flatten /etc/pve/priv/*, /root/.ssh/* or a
	// block device into the tenant's disk. The stage stays in the root-only
	// directory until Create, which checks it again; a refused one is removed.
	limit, err := migration.StagedCopyLimit(req.MaxVirtualSizeBytes)
	if err != nil {
		return nil, errors.NewInternal("invalid staged import size limit", err)
	}
	script := buildNFSImportStageScript(proxmoxImportStageDir, opts.Server, opts.Export, rel, stagePath, opts.UID, opts.GID, limit)
	if _, stderr, err := p.ssh.runSSH(ctx, script); err != nil {
		// The script's trap removed its temp; nothing was published.
		switch code := remoteExitCode(err); {
		case code == nfsNotRegularFileExit && strings.Contains(stderr, nfsNotRegularFileMessage):
			return nil, errors.NewInvalidSpec("nfs import: the staged object is not a regular file")
		case code == nfsTooLargeExit && strings.Contains(stderr, migration.ErrStagedObjectTooLarge.Error()):
			return nil, errors.NewInvalidSpec("nfs import: %s", migration.ErrStagedObjectTooLarge.Error())
		case code == nfsNoSpaceExit && strings.Contains(stderr, migration.ErrStagingSpaceExhausted.Error()):
			return nil, status.Error(codes.ResourceExhausted, "nfs import: "+migration.ErrStagingSpaceExhausted.Error())
		}
		return nil, errors.NewInternal(
			fmt.Sprintf("node-side nfs-mount stage copy failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}
	if err := p.checkStagedNodeImage(ctx, stagePath, "qcow2", limit); err != nil {
		p.cleanupNodeFile(stagePath)
		return nil, err
	}

	// Validate the staged qcow2 (local file, runs as the SSH user — root reads any
	// file). ADR-0006 D5 structural integrity for NFS (no in-stream byte checksum).
	// The stage is the tenant's bytes as copied: pin the format so a vmdk
	// descriptor or raw file is refused here instead of being probed.
	if _, stderr, err := p.ssh.runSSH(ctx, fmt.Sprintf("qemu-img check -f qcow2 -- %s", proxmoxShellQuote(stagePath))); err != nil {
		p.cleanupNodeFile(stagePath)
		return nil, errors.NewInternal(
			fmt.Sprintf("qemu-img check failed on staged qcow2 %s (stderr: %s)", stagePath, strings.TrimSpace(stderr)), err)
	}

	// The disk-size limit, on the converted node-local stage `qm importdisk`
	// will land (the NFS export may change between two reads of it).
	if err := p.checkNodeImportSize(ctx, stagePath, "qcow2", req.MaxVirtualSizeBytes); err != nil {
		return nil, err
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
