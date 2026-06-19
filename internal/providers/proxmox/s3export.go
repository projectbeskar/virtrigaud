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
	"io"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// proxmoxExportStageDir is the directory on the PVE node into which the export
// path flattens the source disk before streaming it to S3. /var/tmp is a real
// on-disk path on every PVE node (unlike /tmp on some hardened nodes), survives
// the duration of the transfer, and is local to the node so the qemu-img flatten
// is not a cross-mount copy of the qcow2.
const proxmoxExportStageDir = "/var/tmp"

// exportDiskToS3 implements the ADR-0006 Proxmox SOURCE path: resolve the VM
// disk's real on-node file path, flatten it to a standalone qcow2 on the node,
// and stream that qcow2 up to S3. It mirrors the libvirt SOURCE export
// (s3export.go) — Proxmox's native flattened format is qcow2, so like libvirt it
// exports qcow2 and the TARGET provider owns any further conversion (ADR D4).
//
// The bytes never land in a temp file in the pod and never traverse a CSI PVC:
// they flow node (`cat <hostTmp.qcow2>`) → SSH stdout → pod → S3 via an io.Pipe
// coupling runSSHStdout to storage.UploadStream (SHA256 computed in-stream,
// ADR D5).
//
// PATH RESOLUTION (Proxmox-specific). GetDiskInfo returns the disk as a PVE
// "storage:volid" reference (e.g. "local-lvm:vm-100-disk-0" or
// "local:100/vm-100-disk-0.qcow2"), NOT a filesystem path. The on-node path is
// resolved with `pvesm path <storage:volid>`, which is the supported way to turn
// a volid into a path and works uniformly across directory, LVM-thin, ZFS and
// Ceph storages (it returns a /dev/... path for block storages). qemu-img reads
// any of those with -f.
//
// FLATTEN. `qemu-img convert -U -f <srcfmt> -O qcow2 <path> <hostTmp>` collapses
// any snapshot/backing chain into one standalone qcow2 (-U skips the shared lock
// so a still-running source can be read crash-consistently; a clean copy still
// requires the source powered off or snapshotted first). req.Compress is honored
// by adding -c (genuine only for the qcow2 target). The flattened temp is removed
// unconditionally afterwards (best-effort, WARN on failure).
//
// Crash-resume of an interrupted transfer is OUT of scope: a failure retries the
// whole export. This is the documented follow-up.
func (p *Provider) exportDiskToS3(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}
	if p.ssh == nil {
		return nil, errors.NewUnavailable("migration SSH data plane not configured (set PROVIDER_ENDPOINT and SSH credentials)", nil)
	}

	// Resolve the disk's PVE storage:volid via GetDiskInfo (returns it in Path).
	diskInfo, err := p.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return nil, errors.NewInternal("failed to get disk info for s3 export", err)
	}
	volid := strings.TrimSpace(diskInfo.Path)
	if volid == "" {
		return nil, errors.NewInvalidSpec("source disk %q has no resolvable storage volume", req.DiskId)
	}
	srcFormat := diskInfo.Format
	if srcFormat == "" {
		srcFormat = "qcow2"
	}

	// Resolve the real on-node filesystem path from the volid via `pvesm path`.
	srcPath, err := p.resolveNodeDiskPath(ctx, volid)
	if err != nil {
		return nil, errors.NewInternal("failed to resolve on-node disk path", err)
	}

	// Build the S3 client (the pod is the S3 client). Options come from
	// storage_options_json; credentials from the credentials map. Never logged.
	storageConfig, err := migration.S3StorageConfigFromRequest(req.StorageOptionsJson, req.Credentials)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid s3 export configuration: %v", err)
	}
	s3client, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create s3 client", err)
	}
	defer s3client.Close()

	exportID := fmt.Sprintf("export-proxmox-%s-%d", sanitizeProxmoxName(req.VmId), time.Now().Unix())
	hostTmp := proxmoxExportStagePath(req.VmId)

	// Never log credentials — only the backend, volid, paths.
	p.logger.Info("Exporting disk from Proxmox node to S3",
		"backend", req.BackendType, "vm", req.VmId, "volid", volid, "src_path", srcPath,
		"host_tmp", hostTmp, "destination", req.DestinationUrl, "compress", req.Compress)

	// --- FLATTEN (ADR D4) on the node ---
	flattenCmd := buildExportFlattenCommand(srcFormat, srcPath, hostTmp, req.Compress)
	if _, stderr, err := p.ssh.runSSH(ctx, flattenCmd); err != nil {
		return nil, errors.NewInternal(fmt.Sprintf("node-side qemu-img flatten failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}

	// Cleanup the flattened temp ALWAYS — success or failure — so a failed export
	// never leaks a multi-GB temp on the node. Best-effort; WARN on failure.
	defer func() {
		if _, _, rmErr := p.ssh.runSSH(context.Background(), fmt.Sprintf("rm -f %s", proxmoxShellQuote(hostTmp))); rmErr != nil {
			p.logger.Warn("Failed to remove flattened export temp on node (manual cleanup may be needed)",
				"host_tmp", hostTmp, "error", rmErr)
		}
	}()

	p.logger.Info("Source disk flattened to standalone qcow2 on node", "host_tmp", hostTmp)

	// --- STREAM (ADR D5) ---
	// node (`cat <hostTmp.qcow2>` → SSH stdout) → pod → S3. The pipe couples
	// runSSHStdout to storage.UploadStream so the disk is never buffered whole in
	// the pod; UploadStream computes the SHA256 in-stream.
	pr, pw := io.Pipe()
	streamCmd := fmt.Sprintf("cat %s", proxmoxShellQuote(hostTmp))

	type ulResult struct {
		resp storage.UploadResponse
		err  error
	}
	ulCh := make(chan ulResult, 1)
	go func() {
		resp, uerr := s3client.UploadStream(ctx, storage.StreamUploadRequest{
			DestinationURL: req.DestinationUrl,
			Reader:         pr,
			ContentLength:  -1, // size unknown; streaming auto-multipart
		})
		_ = pr.CloseWithError(uerr)
		ulCh <- ulResult{resp: resp, err: uerr}
	}()

	streamErr := p.ssh.runSSHStdout(ctx, pw, streamCmd)
	_ = pw.CloseWithError(streamErr)
	ul := <-ulCh

	// Surface the REAL failure. When BOTH sides report an error, the upload error
	// is almost always the root cause: a failed S3 upload closes the pipe's read
	// end, which breaks the node-side `cat` with SIGPIPE — and ssh then reports a
	// bare "exit status 255" with no stderr. Reporting only the cat error there
	// masks the actual cause, so we include both, leading with the upload error
	// (mirrors libvirt s3export.go).
	if ul.err != nil && streamErr != nil {
		return nil, errors.NewInternal(
			fmt.Sprintf("disk export stream failed: s3 upload (node-side stream cat %s also failed: %v)", hostTmp, streamErr),
			ul.err)
	}
	if streamErr != nil {
		return nil, errors.NewInternal(fmt.Sprintf("node-side stream (cat %s) failed", hostTmp), streamErr)
	}
	if ul.err != nil {
		return nil, errors.NewInternal("s3 upload failed during stream", ul.err)
	}

	p.logger.Info("Disk export to S3 completed",
		"export_id", exportID, "bytes", ul.resp.BytesTransferred, "checksum", ul.resp.Checksum)

	return &providerv1.ExportDiskResponse{
		ExportId:           exportID,
		Task:               nil, // synchronous
		EstimatedSizeBytes: ul.resp.BytesTransferred,
		Checksum:           ul.resp.Checksum, // SHA256 of the staged qcow2 object
	}, nil
}

// resolveNodeDiskPath turns a PVE "storage:volid" into the real on-node
// filesystem (or block device) path via `pvesm path <volid>`. pvesm prints the
// resolved path on stdout; an empty result is treated as a failure. This is the
// supported, storage-type-agnostic resolution and replaces the old guess-the-path
// stub (storage_helper.go) that only worked when the pod ran on the node.
func (p *Provider) resolveNodeDiskPath(ctx context.Context, volid string) (string, error) {
	stdout, stderr, err := p.ssh.runSSH(ctx, fmt.Sprintf("pvesm path %s", proxmoxShellQuote(volid)))
	if err != nil {
		return "", fmt.Errorf("pvesm path %q: %w (stderr: %s)", volid, err, strings.TrimSpace(stderr))
	}
	path := parsePvesmPath(stdout)
	if path == "" {
		return "", fmt.Errorf("pvesm path %q returned no usable path (stdout: %q)", volid, strings.TrimSpace(stdout))
	}
	return path, nil
}

// parsePvesmPath extracts the resolved path from `pvesm path` stdout. pvesm
// prints exactly one line — the path — but tolerate surrounding whitespace and
// any trailing blank lines, returning the first non-empty trimmed line.
func parsePvesmPath(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

// buildExportFlattenCommand builds the node-side qemu-img convert command that
// flattens the source disk into a standalone qcow2 at hostTmp. -U skips the
// shared-disk lock (read a still-running source crash-consistently); -f forces
// the source driver (no format probing of an overlay); -O qcow2 keeps the native
// format. When compress is true, -c is added (genuine compression only for the
// qcow2 target). Both paths are shell-quoted. It is a free function so the exact
// argv is unit-testable without SSH.
func buildExportFlattenCommand(srcFormat, srcPath, hostTmp string, compress bool) string {
	args := []string{"qemu-img", "convert", "-U", "-f", srcFormat, "-O", "qcow2"}
	if compress {
		args = append(args, "-c")
	}
	args = append(args, proxmoxShellQuote(srcPath), proxmoxShellQuote(hostTmp))
	return strings.Join(args, " ")
}

// proxmoxExportStagePath returns the path of the transient node-side flattened
// qcow2 for an S3 export. It lives in proxmoxExportStageDir under a dot-prefixed,
// unix-ts-suffixed name so it is distinguishable, hidden from a casual listing,
// and unlikely to collide. The .qcow2 suffix matches the staged object's format.
func proxmoxExportStagePath(vmID string) string {
	return fmt.Sprintf("%s/.virtrigaud-export-%s-%d.qcow2",
		strings.TrimRight(proxmoxExportStageDir, "/"), sanitizeProxmoxName(vmID), time.Now().Unix())
}

// sanitizeProxmoxName strips path separators and whitespace from an identifier so
// it cannot escape the staging directory when interpolated into a path.
func sanitizeProxmoxName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "..", "-")
	if name == "" {
		name = fmt.Sprintf("disk-%d", time.Now().Unix())
	}
	return name
}
