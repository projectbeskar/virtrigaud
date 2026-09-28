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
	"fmt"
	"log"
	"strings"
	"time"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// exportDiskToS3 implements the ADR-0006 Slice 2 libvirt SOURCE path: read the
// VM's disk on the host, flatten it to a standalone qcow2, and stream that
// qcow2 up to S3. It is the symmetric sibling of importDiskFromS3 (the Slice 1
// libvirt TARGET path) and the reverse of the vSphere ExportDisk S3 path: where
// vSphere exports its native vmdk for libvirt to convert, libvirt here exports
// its native qcow2 for the vSphere TARGET to convert (vSphere owns the
// qcow2→monolithicSparse-vmdk conversion on import, ADR D4).
//
// IMPORTANT — the FLATTEN step. The migration controller snapshots the source
// VM before export, which (for qcow2) creates an external overlay whose backing
// chain points at the original base image. Streaming just the overlay would
// upload a near-empty delta with a dangling backing reference — useless to the
// target. So we run `qemu-img convert -f qcow2 -O qcow2 <srcPath> <hostTmp>` on
// the host, which reads the FULL backing chain and writes one standalone qcow2
// carrying all the data. The standalone qcow2 is then streamed to S3.
//
// The bytes never land in a temp file in the pod and never traverse a CSI PVC:
// they flow host (`cat <hostTmp.qcow2>`) → the per-host Conn's SSH-backed
// Stream → pod → S3 (storage.UploadStream reads directly from the
// io.ReadCloser Conn.Stream returns; no manual pipe-shuttling here since
// ADR-0008 PR 3 — Stream already returns a pull-style reader). Integrity is
// the in-stream SHA256 UploadStream computes, reported as the Checksum the
// target verifies on download (ADR D5).
//
// The host-side flattened temp file (hostTmp.qcow2) lands transiently on the
// host (host disk usage = flattened qcow2 during export). It is removed
// unconditionally afterwards (best-effort, WARN on failure), exactly like the
// import path's staged temp. True streaming that avoids the host-side flatten is
// an ADR-0006 follow-up.
//
// Crash-resume of an interrupted transfer is OUT of scope: a failure retries the
// whole export. This is the documented follow-up.
func (s *Server) exportDiskToS3(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, err
	}

	if !strings.Contains(conn.uri(), "ssh://") {
		// The flatten + stream-out runs on the libvirt host over SSH. A local
		// connection is not the Slice 2 source shape.
		return nil, fmt.Errorf("s3 export requires an ssh:// libvirt transport (host-side qemu-img flatten + stream); got %q", conn.uri())
	}

	// Resolve the source disk path on the host via GetDiskInfo (e.g. for
	// demo-ubuntu-libvirt vda=/var/lib/libvirt/images/ubuntu-libvirt-demo.qcow2).
	diskInfo, err := s.provider.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{
		VM:         contracts.VMRef{ID: req.VmId, HostID: req.TargetHostId},
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to resolve source disk info: %w", err)
	}
	srcPath := diskInfo.Path
	if srcPath == "" {
		return nil, fmt.Errorf("source disk %q has no resolvable host path", req.DiskId)
	}

	// Build the S3 client (pod is the S3 client). Options come from
	// storage_options_json; credentials from the credentials map. Never logged.
	storageConfig, err := migration.S3StorageConfigFromRequest(req.StorageOptionsJson, req.Credentials)
	if err != nil {
		return nil, fmt.Errorf("invalid s3 export configuration: %w", err)
	}
	s3client, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create s3 client: %w", err)
	}
	defer s3client.Close()

	exportID := fmt.Sprintf("export-libvirt-%s-%d", req.VmId, time.Now().Unix())

	log.Printf("INFO Exporting disk from libvirt host to S3: backend=s3 vm=%s src=%s dest=%s",
		req.VmId, srcPath, req.DestinationUrl)

	// --- FLATTEN (ADR D4) ---
	hostTmp, cleanup, err := flattenForExport(ctx, hostConnRunner{conn: conn}, srcPath, req.VmId)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	log.Printf("INFO Source disk flattened to standalone qcow2 on host: hostTmp=%s", hostTmp)

	// --- STREAM (ADR D5) ---
	// conn.Stream opens the remote `cat <hostTmp.qcow2>` and hands back its
	// stdout as an io.ReadCloser; storage.UploadStream reads directly from it
	// (SHA256 computed in-stream), so the disk is never buffered whole in the
	// pod. Closing rc — including on an early return below — is what
	// guarantees the remote command is torn down even if UploadStream gives up
	// reading before EOF (e.g. an S3-side failure): Stream's reader is backed
	// by an io.Pipe, so Close unblocks the host-side copy goroutine with
	// io.ErrClosedPipe instead of leaving it blocked forever.
	rc, err := conn.Stream(ctx, "cat", hostTmp)
	if err != nil {
		return nil, fmt.Errorf("failed to start host-side stream (cat %s): %w", hostTmp, err)
	}
	defer func() { _ = rc.Close() }()

	ul, err := s3client.UploadStream(ctx, storage.StreamUploadRequest{
		DestinationURL: req.DestinationUrl,
		Reader:         rc,
		ContentLength:  -1, // size unknown; minio streaming auto-multipart
	})
	if err != nil {
		return nil, fmt.Errorf("s3 upload failed during stream: %w", err)
	}

	log.Printf("INFO Disk export to S3 completed: export_id=%s bytes=%d checksum=%s",
		exportID, ul.BytesTransferred, ul.Checksum)

	return &providerv1.ExportDiskResponse{
		ExportId:           exportID,
		Task:               nil, // synchronous
		EstimatedSizeBytes: ul.BytesTransferred,
		Checksum:           ul.Checksum, // SHA256 of the staged (qcow2) object
	}, nil
}

// exportStageUmask is the umask the flatten writes the export's staging file
// under: it stays 0600 (mktemp creates it so), private to the SSH user — it is
// a full copy of a VM's disk.
const exportStageUmask = "0177"

// hostExportStageSuffix ends the export's staging file name: the staged (and
// uploaded) object's format.
const hostExportStageSuffix = ".qcow2"

// flattenForExport collapses the (possibly snapshot-overlay) backing chain of
// srcPath into one standalone qcow2 on the host behind h and returns its path
// and the function that removes it.
//
// The file is made for this export alone before the flatten runs (mktemp:
// created exclusively, unpredictable name, mode 0600 — kept by the
// exportStageUmask the flatten runs under), in the source disk's directory,
// so the convert stays within one filesystem. Its removal is armed as soon as
// it exists — the flatten failing, or the request being cancelled, still
// removes it (removeHostPath runs detached from the request's cancellation,
// bounded): a failed export used to leave a world-readable multi-GB copy
// behind under a predictable name.
//
// -f qcow2 forces the source driver (no format probing of the overlay); -O
// qcow2 keeps the native format the target expects. -U skips the shared-disk
// lock so a still-running source (e.g. powerOffBeforeMigration not yet
// honored, or createSnapshot=false) can be read — this is a crash-consistent
// copy; a consistent copy still requires the source to be powered off or
// snapshotted first.
func flattenForExport(ctx context.Context, h hostCommandRunner, srcPath, vmID string) (string, func(), error) {
	hostTmp, err := makeHostTempSuffix(ctx, h, hostExportStageTemplate(srcPath, vmID), hostExportStageSuffix, false)
	if err != nil {
		return "", nil, fmt.Errorf("create the export staging file on the host: %w", err)
	}
	cleanup := func() { removeHostPath(ctx, h, hostTmp, false) }
	if res, err := runHost(ctx, h, withUmask(exportStageUmask, "qemu-img", "convert", "-U", "-f", "qcow2", "-O", "qcow2",
		srcPath, hostTmp)...); err != nil {
		cleanup()
		stderr := ""
		if res != nil && strings.TrimSpace(res.Stderr) != "" {
			stderr = fmt.Sprintf(" (qemu-img stderr: %s)", strings.TrimSpace(res.Stderr))
		}
		return "", nil, fmt.Errorf("host-side qemu-img flatten (qcow2→standalone qcow2) failed: %w%s", err, stderr)
	}
	return hostTmp, cleanup, nil
}

// hostExportStageTemplate returns the mktemp template of the transient
// host-side flattened qcow2 for an S3 export: in the SAME directory as the
// source disk (so the flatten convert reads/writes within one filesystem, no
// cross-device copy), under a dot-prefixed name carrying the (sanitized) VM
// id, so it is distinguishable and hidden from a casual directory listing;
// mktemp replaces the trailing mktempTemplateSuffix with random characters
// and appends hostExportStageSuffix.
func hostExportStageTemplate(srcPath, vmID string) string {
	dir := srcPath
	if idx := strings.LastIndex(srcPath, "/"); idx >= 0 {
		dir = srcPath[:idx]
	}
	dir = strings.TrimRight(dir, "/")
	return fmt.Sprintf("%s/.virtrigaud-export-%s.%s", dir, sanitizeVolumeName(vmID), mktempTemplateSuffix)
}
