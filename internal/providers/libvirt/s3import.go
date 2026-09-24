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
	"io"
	"log"
	"strings"
	"time"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// importDiskFromS3 implements the ADR-0006 Slice 1 libvirt TARGET path: download
// the staged vmdk object from S3 and convert it to qcow2 ON THE HOST (the
// target-owned vmdk→qcow2 conversion, ADR D4). The disk never lands in a temp
// file in the pod and never traverses a CSI PVC: bytes flow S3 → pod → SSH stdin
// → host. Integrity is dual-checked (ADR D5): the S3 object's SHA256 is verified
// against the source-reported checksum while it streams in (stage), then
// `qemu-img check` validates the converted qcow2 on the host (convert).
//
// IMPORTANT: this is a two-step host flow (stage → convert), NOT a single
// streamed `qemu-img convert /dev/stdin`. qemu-img's vmdk+file driver requires a
// seekable REGULAR file — it seeks to read the streamOptimized footer/grain
// directory — and refuses a non-seekable pipe (`/dev/stdin`) with "the 'file'
// driver requires '<path>' to be a regular file". So we first stage the full
// vmdk to a regular file on the host (via `cat > <hostTmp>`, a sequential
// pipe-friendly write), then run `qemu-img convert` against that seekable file.
//
// Trade-off (documented): the full vmdk lands transiently on the host (host disk
// usage = staged vmdk + converted qcow2 during conversion). The temp file is
// removed unconditionally afterwards. True streaming / `direct` mode that avoids
// the host-side stage is the ADR-0006 follow-up.
//
// Crash-resume of an interrupted transfer is OUT of scope for Slice 1: a failure
// retries the whole import. This is the documented follow-up.
func (s *Server) importDiskFromS3(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, err
	}

	if !strings.Contains(conn.uri(), "ssh://") {
		// Relay-to-host conversion needs an SSH transport to stream into the
		// host's qemu-img. A local connection is not the Slice 1 target shape.
		return nil, fmt.Errorf("s3 import requires an ssh:// libvirt transport (host-side qemu-img conversion); got %q", conn.uri())
	}

	// Build the S3 client (pod is the S3 client). Options come from
	// storage_options_json; credentials from the credentials map. Never logged.
	storageConfig, err := migration.S3StorageConfigFromRequest(req.StorageOptionsJson, req.Credentials)
	if err != nil {
		return nil, fmt.Errorf("invalid s3 import configuration: %w", err)
	}
	s3client, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create s3 client: %w", err)
	}
	defer s3client.Close()

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

	// The staged S3 object is in the SOURCE provider's native flattened format,
	// threaded by the controller as req.Format (vmdk from a vSphere source, qcow2
	// from a libvirt/proxmox source). Default to vmdk for older callers that don't
	// set it (the Slice 1 vSphere→libvirt assumption).
	stagedFormat := strings.ToLower(strings.TrimSpace(req.Format))
	if stagedFormat == "" {
		stagedFormat = "vmdk"
	}
	// Stage the source-format object to a regular file in the SAME directory as the
	// target so the later qemu-img convert reads/writes within one filesystem (no
	// cross-device copy) and a leaked temp is co-located with the pool for cleanup.
	stagePath := hostStagePath(poolPath, volumeName, stagedFormat)

	log.Printf("INFO Importing disk from S3 to libvirt host: backend=s3 pool=%s volume=%s stage=%s target=%s",
		poolName, volumeName, stagePath, targetPath)

	// --- STAGE (ADR D5 part 1) ---
	// Stream S3 → SSH stdin → `cat > <stagePath>` on the host. cat writes
	// sequentially (no seek), so the non-seekable pipe is fine here — unlike
	// `qemu-img convert /dev/stdin`, which fails on a pipe. The pipe couples the
	// S3 download (DownloadStream, SHA256 verified in-stream) to conn.StreamIn's
	// blocking call so the disk is never buffered whole in the pod.
	pr, pw := io.Pipe()
	// The redirect lives in a fixed `sh -c` script; stagePath is its positional
	// "$1", never interpolated into shell text (writeStdinToFileArgv).
	stageArgv := writeStdinToFileArgv(stagePath)

	type dlResult struct {
		resp storage.DownloadResponse
		err  error
	}
	dlCh := make(chan dlResult, 1)
	go func() {
		resp, derr := s3client.DownloadStream(ctx, storage.StreamDownloadRequest{
			SourceURL:        req.SourceUrl,
			Writer:           pw,
			ExpectedChecksum: req.ExpectedChecksum,
		})
		// Closing the writer with the download error propagates it to
		// StreamIn's reader so cat sees EOF (clean) or a broken pipe (error).
		_ = pw.CloseWithError(derr)
		dlCh <- dlResult{resp: resp, err: derr}
	}()

	stageErr := conn.StreamIn(ctx, pr, stageArgv...)
	// If the SSH/cat side exited (especially on error) the download goroutine may
	// still be blocked writing into the pipe. Unblock it with a closed-read-end
	// error so it returns promptly instead of leaking; the DownloadStream error
	// is then observed on dlCh.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	dl := <-dlCh

	// Cleanup the staged vmdk ALWAYS — success or failure — so a failed import
	// never leaks a multi-GB temp on the host. Best-effort; WARN on failure.
	defer func() {
		if _, rmErr := conn.RunHost(context.Background(), "rm", "-f", stagePath); rmErr != nil {
			log.Printf("WARN failed to remove staged import temp %s on host (manual cleanup may be needed): %v",
				stagePath, rmErr)
		}
	}()

	// Surface the REAL stage failure: the download/checksum error is the root
	// cause when the stream broke (e.g. checksum mismatch); only if the download
	// was clean do we attribute a stage failure to the host `cat`.
	if dl.err != nil {
		return nil, fmt.Errorf("s3 download/transfer failed during stage: %w", dl.err)
	}
	if stageErr != nil {
		return nil, fmt.Errorf("host-side stage (cat to %s) failed: %w", stagePath, stageErr)
	}

	log.Printf("INFO S3 object staged on host: bytes=%d sha256-verified=%t",
		dl.resp.BytesTransferred, req.ExpectedChecksum != "")

	// SECURITY: refuse a staged object whose header references another host
	// file (qcow2 backing / data file, VMDK extent): the convert would flatten
	// that file into <vm>-migrated.qcow2. Read it in the format the convert
	// forces, so the check sees exactly what the convert will open.
	if _, err := inspectHostImageAs(ctx, hostConnRunner{conn: conn}, importedImageSubject, stagePath, stagedFormat); err != nil {
		return nil, fmt.Errorf("inspect staged s3 object: %w", err)
	}

	// --- CONVERT (ADR D4) ---
	// qemu-img reads the staged file (seekable regular file) and writes the
	// target qcow2. On failure, surface qemu-img's stderr directly so the real
	// cause is visible (no io.Pipe "closed pipe" masking).
	// Raw values: RunHost shell-quotes every argv element itself (this also
	// covers stagedFormat, which was previously interpolated unquoted).
	if res, err := conn.RunHost(ctx, "qemu-img", "convert", "-f", stagedFormat, "-O", "qcow2",
		stagePath, targetPath); err != nil {
		return nil, fmt.Errorf("host-side qemu-img convert (%s→qcow2) failed: %w%s", stagedFormat, err, qemuImgStderr(res))
	}

	log.Printf("INFO Staged %s converted to qcow2 on host: target=%s", stagedFormat, targetPath)

	// --- VALIDATE (ADR D5 part 2) ---
	// qemu-img check on the converted qcow2. Surface its stderr on failure too.
	if res, err := conn.RunHost(ctx, "qemu-img", "check", targetPath); err != nil {
		return nil, fmt.Errorf("qemu-img check failed on converted qcow2 %s: %w%s", targetPath, err, qemuImgStderr(res))
	}

	// Make libvirt aware of the new volume.
	if _, err := conn.Virsh(ctx, "pool-refresh", poolName); err != nil {
		log.Printf("WARN pool-refresh failed after import (volume may still be usable by path): %v", err)
	}

	return &providerv1.ImportDiskResponse{
		DiskId: volumeName,
		Path:   targetPath,
		Task:   nil, // synchronous
		// Report the bytes transferred from S3 (the staged vmdk). The converted
		// qcow2's on-host size is not byte-comparable (conversion is not size-
		// deterministic), matching ADR D5's "no after-conversion byte equality".
		ActualSizeBytes: dl.resp.BytesTransferred,
		Checksum:        dl.resp.Checksum, // SHA256 of the transferred (pre-conversion) object
	}, nil
}

// hostStagePath returns the path of the transient host-side staging file for an
// import. It lives in the pool directory (same filesystem as the target so the
// convert is intra-device) under a dot-prefixed, unix-ts-suffixed name so it is
// distinguishable, hidden from a casual pool listing, and unlikely to collide
// with a real volume. The suffix matches the staged object's source format
// (vmdk from a vSphere source, qcow2 from a libvirt/proxmox source) so qemu-img's
// format probing has the right hint.
func hostStagePath(poolPath, volumeName, format string) string {
	if format == "" {
		format = "vmdk"
	}
	return fmt.Sprintf("%s/.virtrigaud-import-%s-%d.%s",
		strings.TrimRight(poolPath, "/"), volumeName, time.Now().Unix(), format)
}

// qemuImgStderr formats a hostconn.Result's stderr for appending to a wrapped
// error so the underlying qemu-img message is surfaced instead of being
// masked. It returns "" when there is no result or no stderr, keeping the
// error tidy.
func qemuImgStderr(res *hostconn.Result) string {
	if res == nil {
		return ""
	}
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return fmt.Sprintf(" (qemu-img stderr: %s)", s)
	}
	return ""
}

// sanitizeVolumeName strips path separators and whitespace from a volume name so
// it cannot escape the pool directory when interpolated into the target path.
func sanitizeVolumeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "..", "-")
	name = strings.TrimSuffix(name, ".qcow2")
	if name == "" {
		name = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}
	return name
}
