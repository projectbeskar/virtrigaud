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

package libvirt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	sdkerrors "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// exportDiskToNFS implements the ADR-0006 Slice 4 libvirt SOURCE path for the NFS
// backend: the libvirt HOST's qemu-img writes the flattened qcow2 directly to the
// NFS export over libnfs (qemu's block-nfs driver), with no provider-pod hop and
// no S3 client. This is the host-side `direct`-style transport that is natural
// for NFS (the host already has the disk and NFS reachability). The destination
// is the controller-built, C7'-hardened nfs:// URL.
func (s *Server) exportDiskToNFS(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if s.provider == nil {
		return nil, nothingDone(wireProviderNotInitialized, fmt.Errorf("libvirt provider not initialized"))
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, nothingDone(wireExportNotPerformed, err)
	}
	if !strings.Contains(conn.uri(), "ssh://") {
		return nil, refusal(codes.FailedPrecondition, wireExportNeedsSSH,
			fmt.Errorf("nfs export requires an ssh:// libvirt transport (host-side qemu-img → nfs://); got %q", conn.uri()))
	}

	nfsURL := strings.TrimSpace(req.DestinationUrl)
	if !strings.HasPrefix(nfsURL, "nfs://") {
		return nil, refusal(codes.InvalidArgument, wireExportRefused,
			fmt.Errorf("nfs export destination must be an nfs:// URL, got %q", nfsURL))
	}

	// The source is one of the domain's own disks, read in the format its
	// definition names (exportSourceOn).
	vp, err := virshOf(conn)
	if err != nil {
		return nil, nothingDone(wireExportNotPerformed, err)
	}
	srcPath, srcFormat, err := exportSourceOn(ctx, vp, byName(req.VmId), req.DiskId)
	if err != nil {
		return nil, nothingDone(wireExportNotPerformed, fmt.Errorf("failed to resolve the source disk: %w", err))
	}
	resp, err := exportConvertToNFS(ctx, conn, req, srcPath, srcFormat, nil)
	if err != nil {
		return nil, singleHostExportFailure(ctx, err)
	}
	return resp, nil
}

// exportConvertToNFS is the NFS export core, run on conn — the single host's
// connection, or a clustered host's leased connection after the owner check
// (exportDiskRouted): the host's qemu-img flattens srcPath straight into the
// nfs:// destination. guard wraps the convert (flock + timeout on a clustered
// host, routed_budget.go); a nil guard is the single-host path.
//
// The source is read as root when passwordless sudo allows it
// (privileged_copy.go; a chain that is not safe to copy is refused): a
// snapshotted VM's active disk is libvirt's 0600 overlay, which the SSH user
// cannot read. The root qemu-img writes to the destination's server and path
// with exactly the SSH user's uid and gid and no other libnfs option
// (nfsURLForRoot) — the identity the export has always used. A destination
// that names another uid or gid (a VMMigration's spec.storage.nfs.uid/gid) is
// never written as root: the convert runs as the SSH user, which presents
// that identity as it always has, with the source format still pinned.
func exportConvertToNFS(ctx context.Context, conn libvirtConn, req *providerv1.ExportDiskRequest, srcPath, srcFormat string,
	guard *hostCmdGuard) (*providerv1.ExportDiskResponse, error) {
	nfsURL := strings.TrimSpace(req.DestinationUrl)

	log.Printf("INFO Exporting disk from libvirt host to NFS: backend=nfs vm=%s src=%s dest=%s",
		req.VmId, srcPath, nfsURL)

	// Flatten + write straight to the NFS export. -U reads a possibly-running
	// source (crash-consistent; a consistent copy still needs power-off/snapshot
	// first). qemu-img's libnfs driver performs the NFS write — no pod buffering.
	// RunHost is argv-safe (the transport shell-quotes every element), so the
	// values are passed raw — pre-quoting them would now double-quote.
	// It writes no host file (the destination is the NFS export), so the
	// guard has no host target to check.
	h := hostConnRunner{conn: conn}
	convert := diskCopy{
		src:       srcPath,
		srcFormat: srcFormat,
		args:      []string{"convert", "-U", "-f", srcFormat, "-O", "qcow2", srcPath, nfsURL},
	}
	if rootURL, err := nfsURLForRoot(ctx, h, nfsURL); err != nil {
		log.Printf("WARN Exporting %s to NFS as the provider's SSH user, not as root: %v", srcPath, err)
	} else {
		convert.privArgs = []string{"convert", "-U", "-f", srcFormat, "-O", "qcow2", srcPath, rootURL}
	}
	if res, err := convert.run(ctx, h, guard); err != nil {
		var roe *routedOpError
		if errors.As(err, &roe) {
			return nil, err
		}
		// A failure of the checks before the convert is tagged stageBefore
		// (diskCopy.run); an untagged one is the convert itself, which writes
		// the destination in place (stageCommand).
		if _, _, tagged := stageOf(err); !tagged {
			err = atStage(stageCommand, err)
		}
		return nil, fmt.Errorf("host-side qemu-img convert to nfs failed: %w%s", err, copyStderr(res))
	}

	log.Printf("INFO Source disk written to NFS export: dest=%s", nfsURL)

	return &providerv1.ExportDiskResponse{
		ExportId: fmt.Sprintf("export-libvirt-nfs-%s-%d", req.VmId, time.Now().Unix()),
		// qemu-img does not emit a byte SHA256 over the libnfs write; NFS integrity
		// is established by the target's `qemu-img check` on the converted qcow2
		// (ADR-0006 Slice 4 — no in-stream checksum for the qemu-img transport).
	}, nil
}

// nfsStagedCopySuffix names the private byte copy of a staged NFS object
// that an import checks and converts.
const nfsStagedCopySuffix = ".nfs-staged"

// stagedHeadScript prints the first "$2" bytes of the file "$1" as one line of
// base64 (the header bytes travel over the text channel intact); it fails when
// the file cannot be read.
const stagedHeadScript = `head -c "$2" -- "$1" >/dev/null && head -c "$2" -- "$1" | base64 -w0`

// checkStagedHostImageBytes parses the leading bytes of the staged image at
// path on the host WITHOUT qemu (migration.CheckStagedImageBytes), before
// inspectHostImageAs lets `qemu-img info` open it: qemu opens a qcow2
// external data file and VMDK extents while it reads a header, so a crafted
// header could make the host open a device node, a host file or an
// nbd:/http: target before any check refuses it. A declared virtual size above
// limit (migration.StagedCopyLimit of the request) is refused too. A refused
// header is InvalidSpec; a host command failure is retryable.
func checkStagedHostImageBytes(ctx context.Context, h hostCommandRunner, path, format string, limit int64) error {
	res, err := runHost(ctx, h, "sh", "-c", stagedHeadScript, "sh", path, strconv.Itoa(migration.StagedImageHeadBytes))
	if err != nil {
		return hostCheckFailed("read the staged image header", err)
	}
	head, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		return hostCheckFailed("decode the staged image header", err)
	}
	if _, err := migration.CheckStagedImageBytes(head, format, limit); err != nil {
		return newImageRejection(importedImageSubject, err.Error())
	}
	return nil
}

// boundedNFSCopy copies the staged NFS object byte for byte to dst, bounded
// (re-review Medium 1): the tenant's NFS server controls the object's size.
// Its size is read first (`qemu-img info -f raw`, which opens nothing but that
// file) and refused above limit (ErrStagedObjectTooLarge, InvalidSpec) or when
// the private directory's filesystem has less than StagedCopySpaceFactor times
// it free (ErrStagingSpaceExhausted, retryable). The copy itself is
// `qemu-img dd` of at most the measured size + 1 MiB, so an object that grows after it
// was measured is still bounded by the measured size, and a copy longer than
// the measured size is refused. A refused or failed copy is removed with wd.
func boundedNFSCopy(ctx context.Context, conn libvirtConn, wd *diskWriteDir, nfsURL, dst string, limit int64) error {
	h := hostConnRunner{conn: conn}
	res, err := conn.RunHost(ctx, "qemu-img", "info", "-U", "--output=json", "-f", "raw", nfsURL)
	if err != nil {
		return fmt.Errorf("host-side size check of the staged nfs object failed: %w%s", err, qemuImgStderr(res))
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil {
		return hostCheckFailed("parse the staged object size", err)
	}
	free, err := hostFreeBytes(ctx, h, wd.dir)
	if err != nil {
		return err
	}
	if err := stagedCopyRefusal(migration.CheckStagedCopy(info.VirtualSize, limit, free)); err != nil {
		return err
	}
	if res, err := conn.RunHost(ctx, withUmask(vmDiskUmask, "qemu-img", "dd", "-f", "raw", "-O", "raw", "bs=1M",
		"count="+strconv.FormatInt(migration.StagedCopyDDCount(info.VirtualSize), 10), "if="+nfsURL, "of="+dst)...); err != nil {
		return atStage(stageCommand, fmt.Errorf("host-side copy of the staged nfs object failed: %w%s", err, qemuImgStderr(res)))
	}
	sz, err := runHost(ctx, h, "stat", "-c", "%s", "--", dst)
	if err != nil {
		return hostCheckFailed("read the staged copy size", err)
	}
	copied, err := strconv.ParseInt(strings.TrimSpace(sz.Stdout), 10, 64)
	if err != nil {
		return hostCheckFailed("parse the staged copy size", err)
	}
	if copied > info.VirtualSize || copied > limit {
		return stagedCopyRefusal(migration.ErrStagedObjectTooLarge)
	}
	return nil
}

// hostFreeBytes is the free space (available blocks x fragment size) of the
// host filesystem holding dir.
func hostFreeBytes(ctx context.Context, h hostCommandRunner, dir string) (int64, error) {
	fs, err := runHost(ctx, h, "stat", "-f", "-c", "%a %S", "--", dir)
	if err != nil {
		return 0, hostCheckFailed("read the free space for the staged copy", err)
	}
	var avail, frsize int64
	if _, err := fmt.Sscanf(strings.TrimSpace(fs.Stdout), "%d %d", &avail, &frsize); err != nil {
		return 0, hostCheckFailed("parse the free space for the staged copy", err)
	}
	return avail * frsize, nil
}

// stagedDownloadBound is the most bytes an S3 staged object may be downloaded
// into dir on the host: its reported size, refused (constant) above the import
// limit or when dir's filesystem cannot hold StagedCopySpaceFactor times it.
func stagedDownloadBound(ctx context.Context, h hostCommandRunner, s3client storage.Storage, sourceURL, dir string, limit int64) (int64, error) {
	meta, err := s3client.GetMetadata(ctx, sourceURL)
	if err != nil {
		return 0, fmt.Errorf("s3 import: read the size of the staged object: %w", err)
	}
	free, err := hostFreeBytes(ctx, h, dir)
	if err != nil {
		return 0, err
	}
	if err := stagedCopyRefusal(migration.CheckStagedCopy(meta.Size, limit, free)); err != nil {
		return 0, err
	}
	return meta.Size, nil
}

// stagedCopyRefusal maps a migration.CheckStagedCopy refusal to the provider
// error: too large is InvalidSpec (permanent), no space is ResourceExhausted
// (retryable). Both constant; nil stays nil.
func stagedCopyRefusal(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, migration.ErrStagedObjectTooLarge):
		return sdkerrors.NewInvalidSpec("%s", "staged import: "+err.Error())
	default:
		return status.Error(codes.ResourceExhausted, "staged import: "+err.Error())
	}
}

// copyCheckConvertNFSObject reads the staged NFS object ONCE — a byte copy
// (`qemu-img convert -f raw -O raw`, which interprets no header and opens no
// other file) into the private directory wd — then checks the header of that
// copy — first its bytes without qemu (checkStagedHostImageBytes), then with
// qemu-img (inspectHostImageAs: no backing file, data file or extent, qcow2) — and
// converts that same copy to the qcow2 volume name in wd, which it returns.
// The tenant-controlled NFS server can therefore not swap the object between
// the check and the convert. The copy is removed once converted (and with wd
// in any case).
func copyCheckConvertNFSObject(ctx context.Context, conn libvirtConn, wd *diskWriteDir, nfsURL, name string, limit int64) (string, error) {
	h := hostConnRunner{conn: conn}
	stagedCopy := wd.file(name + nfsStagedCopySuffix)
	if err := boundedNFSCopy(ctx, conn, wd, nfsURL, stagedCopy, limit); err != nil {
		return "", err
	}
	if err := checkStagedHostImageBytes(ctx, h, stagedCopy, migration.StagedFormatQCOW2, limit); err != nil {
		return "", fmt.Errorf("inspect staged nfs object: %w", err)
	}
	if _, err := inspectHostImageAs(ctx, h, importedImageSubject, stagedCopy, "qcow2"); err != nil {
		return "", fmt.Errorf("inspect staged nfs object: %w", err)
	}
	newDisk := wd.file(name)
	if res, err := conn.RunHost(ctx, withUmask(vmDiskUmask, "qemu-img", "convert", "-f", "qcow2", "-O", "qcow2",
		stagedCopy, newDisk)...); err != nil {
		return "", atStage(stageCommand, fmt.Errorf("host-side qemu-img convert of the staged nfs object failed: %w%s", err, qemuImgStderr(res)))
	}
	removeHostPath(ctx, h, stagedCopy, false)
	return newDisk, nil
}

// importDiskFromNFS implements the ADR-0006 Slice 4 libvirt TARGET path: the
// host's qemu-img reads the staged qcow2 directly from the NFS export over libnfs
// and writes it into the target storage pool. No download, no pod staging.
func (s *Server) importDiskFromNFS(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	resp, err := s.importDiskFromNFSSteps(ctx, req)
	if err != nil {
		return nil, singleHostImportFailure(ctx, err)
	}
	return resp, nil
}

// importDiskFromNFSSteps is the NFS import. As in the S3 import, every step
// before the rename onto the landing name (wd.publish, tagged stagePublish)
// works in a private directory that is removed whatever happens.
func (s *Server) importDiskFromNFSSteps(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if s.provider == nil {
		return nil, nothingDone(wireProviderNotInitialized, fmt.Errorf("libvirt provider not initialized"))
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(conn.uri(), "ssh://") {
		return nil, refusal(codes.FailedPrecondition, wireImportNeedsSSH,
			fmt.Errorf("nfs import requires an ssh:// libvirt transport (host-side qemu-img from nfs://); got %q", conn.uri()))
	}

	nfsURL := strings.TrimSpace(req.SourceUrl)
	if !strings.HasPrefix(nfsURL, "nfs://") {
		return nil, atStage(stageInvalid, fmt.Errorf("nfs import source must be an nfs:// URL, got %q", nfsURL))
	}

	// Resolve the target pool and its host path.
	poolName := "default"
	if req.StorageHint != "" {
		poolName = req.StorageHint
	}
	storageProvider := conn.storageProvider()
	if err := storageProvider.EnsureDefaultStoragePool(ctx); err != nil {
		return nil, fmt.Errorf("failed to ensure storage pool: %w", err)
	}
	poolInfo, err := storageProvider.GetPoolInfo(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve target pool %q: %w", poolName, err)
	}
	if poolInfo.Path == "" {
		return nil, fmt.Errorf("target pool %q has no resolvable host path", poolName)
	}

	volumeName, err := importVolumeName(req)
	if err != nil {
		return nil, err
	}
	if volumeName == "" {
		volumeName = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}
	volumeName = sanitizeVolumeName(volumeName)
	poolPath := strings.TrimRight(poolInfo.Path, "/")
	targetPath := fmt.Sprintf("%s/%s.qcow2", poolPath, volumeName)
	// Never land over a disk another domain uses (e.g. a VM already running on
	// the disk a second import of the same name would replace).
	if err := ensureDiskTargetFree(ctx, hostConnRunner{conn: conn}, importedDiskSubject(volumeName), targetPath); err != nil {
		return nil, err
	}

	log.Printf("INFO Importing disk from NFS to libvirt host: backend=nfs pool=%s volume=%s src=%s target=%s",
		poolName, volumeName, nfsURL, targetPath)

	// SECURITY: the staged object is on an NFS server the tenant controls, so
	// it can change between two reads. It is read ONCE: copied byte for byte
	// (`-f raw` interprets no header and opens nothing else) into a private
	// directory next to the pool volume. The header check (a backing file or
	// data file would make the convert flatten a host file into
	// <vm>-migrated.qcow2) and the convert both read that private copy, so they
	// see the same bytes. The pool volume is created with vmDiskMode (withUmask:
	// Create adopts it without a chmod) in the same private directory, checked
	// there and then renamed into place (diskWriteDir). Raw values: RunHost
	// shell-quotes every argv element itself.
	h := hostConnRunner{conn: conn}
	wd, err := newDiskWriteDir(ctx, h, poolPath)
	if err != nil {
		return nil, fmt.Errorf("nfs import: %w", err)
	}
	defer wd.cleanup(ctx)
	// The copy is bounded by the import's disk-size limit too (#382).
	limit, err := migration.StagedCopyLimit(req.MaxVirtualSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("nfs import: %w", err)
	}
	newDisk, err := copyCheckConvertNFSObject(ctx, conn, wd, nfsURL, filepath.Base(targetPath), limit)
	if err != nil {
		return nil, err
	}

	// Validate the converted qcow2 (ADR-0006 D5 structural integrity for NFS).
	if res, err := conn.RunHost(ctx, "qemu-img", "check", "-f", "qcow2", newDisk); err != nil {
		return nil, fmt.Errorf("qemu-img check failed on imported qcow2 %s: %w%s", targetPath, err, qemuImgStderr(res))
	}
	// The disk-size limit, on the converted disk in the private directory,
	// right before it is published (the removed directory lands nothing).
	if err := checkHostImportSize(ctx, hostConnRunner{conn: conn}, newDisk, "qcow2", req.MaxVirtualSizeBytes); err != nil {
		return nil, err
	}
	if err := wd.publish(ctx, filepath.Base(targetPath), targetPath); err != nil {
		return nil, atStage(stagePublish, fmt.Errorf("nfs import: %w", err))
	}

	if _, err := conn.Virsh(ctx, "pool-refresh", poolName); err != nil {
		log.Printf("WARN pool-refresh failed after import (volume may still be usable by path): %v", err)
	}

	log.Printf("INFO NFS object imported to pool volume: target=%s", targetPath)

	return &providerv1.ImportDiskResponse{
		DiskId: volumeName,
		Path:   targetPath,
		Task:   nil,
	}, nil
}
