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
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"

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
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	vp := libvirtProvider.virshProvider

	if !strings.Contains(vp.uri, "ssh://") {
		// Relay-to-host conversion needs an SSH transport to stream into the
		// host's qemu-img. A local connection is not the Slice 1 target shape.
		return nil, fmt.Errorf("s3 import requires an ssh:// libvirt transport (host-side qemu-img conversion); got %q", vp.uri)
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
	storageProvider := NewStorageProvider(vp)
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

	volumeName := req.TargetName
	if volumeName == "" {
		volumeName = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}
	volumeName = sanitizeVolumeName(volumeName)
	poolPath := strings.TrimRight(poolInfo.Path, "/")
	targetPath := fmt.Sprintf("%s/%s.qcow2", poolPath, volumeName)

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
	// S3 download (DownloadStream, SHA256 verified in-stream) to the SSH stdin so
	// the disk is never buffered whole in the pod.
	pr, pw := io.Pipe()
	stageCmd := fmt.Sprintf("cat > %s", shellQuote(stagePath))

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
		// Closing the writer with the download error propagates it to the SSH
		// stdin reader so cat sees EOF (clean) or a broken pipe (error).
		_ = pw.CloseWithError(derr)
		dlCh <- dlResult{resp: resp, err: derr}
	}()

	stageErr := runSSHStdin(ctx, vp, pr, stageCmd)
	// If the SSH/cat side exited (especially on error) the download goroutine may
	// still be blocked writing into the pipe. Unblock it with a closed-read-end
	// error so it returns promptly instead of leaking; the DownloadStream error
	// is then observed on dlCh.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	dl := <-dlCh

	// Cleanup the staged vmdk ALWAYS — success or failure — so a failed import
	// never leaks a multi-GB temp on the host. Best-effort; WARN on failure.
	defer func() {
		if _, rmErr := vp.runVirshCommand(context.Background(), "!", "rm", "-f", stagePath); rmErr != nil {
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

	// --- CONVERT (ADR D4) ---
	// qemu-img reads the staged file (seekable regular file) and writes the
	// target qcow2. On failure, surface qemu-img's stderr directly so the real
	// cause is visible (no io.Pipe "closed pipe" masking).
	if res, err := vp.runVirshCommand(ctx, "!", "qemu-img", "convert", "-f", stagedFormat, "-O", "qcow2",
		shellQuote(stagePath), shellQuote(targetPath)); err != nil {
		return nil, fmt.Errorf("host-side qemu-img convert (%s→qcow2) failed: %w%s", stagedFormat, err, qemuImgStderr(res))
	}

	log.Printf("INFO Staged %s converted to qcow2 on host: target=%s", stagedFormat, targetPath)

	// --- VALIDATE (ADR D5 part 2) ---
	// qemu-img check on the converted qcow2. Surface its stderr on failure too.
	if res, err := vp.runVirshCommand(ctx, "!", "qemu-img", "check", shellQuote(targetPath)); err != nil {
		return nil, fmt.Errorf("qemu-img check failed on converted qcow2 %s: %w%s", targetPath, err, qemuImgStderr(res))
	}

	// Make libvirt aware of the new volume.
	if _, err := vp.runVirshCommand(ctx, "pool-refresh", poolName); err != nil {
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

// runSSHStdin runs a single command on the libvirt host over SSH, streaming
// stdin from r (a pipe), and returns when the command exits. Unlike
// runVirshCommandOnce it does NOT buffer the input in memory — it wires r to the
// remote process's stdin so a multi-GB disk streams through. It reuses the same
// host-key policy and ControlMaster multiplexing as the virsh/scp paths
// (#149/ADR-0004, #194) so trust material and connections are shared.
func runSSHStdin(ctx context.Context, vp *VirshProvider, r io.Reader, remoteCmd string) error {
	parsedURI, err := url.Parse(vp.uri)
	if err != nil {
		return fmt.Errorf("failed to parse libvirt URI: %w", err)
	}
	host := parsedURI.Host
	user := parsedURI.User.Username()

	// Host-key pre-flight: re-emit the audit line and hard-fail if verification
	// is on but no usable known_hosts is present (no TOFU), matching scp.
	vp.hostKey.logVerificationMode(vp.logger, host)
	if err := vp.hostKey.verifyKnownHostsPresent(host); err != nil {
		return fmt.Errorf("ssh stdin host-key verification pre-flight failed: %w", err)
	}

	var cmd *exec.Cmd
	if vp.credentials.Password != "" {
		sshArgs := []string{
			"-e", // read password from SSHPASS
			"ssh",
			"-o", "PasswordAuthentication=yes",
			"-o", "PubkeyAuthentication=no",
			"-o", "LogLevel=ERROR",
		}
		sshArgs = append(sshArgs, vp.hostKey.sshHostKeyOptions()...)
		sshArgs = append(sshArgs, sshMultiplexOptions()...)
		sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, host), remoteCmd)
		cmd = exec.CommandContext(ctx, "sshpass", sshArgs...)
		cmd.Env = append(os.Environ(), fmt.Sprintf("SSHPASS=%s", vp.credentials.Password))
	} else {
		sshArgs := []string{"-o", "LogLevel=ERROR"}
		if strings.TrimSpace(vp.credentials.SSHPrivateKey) != "" {
			sshArgs = append(sshArgs, sshKeyAuthOptions(resolveSSHKeyFile(parsedURI))...)
		}
		sshArgs = append(sshArgs, vp.hostKey.sshHostKeyOptions()...)
		sshArgs = append(sshArgs, sshMultiplexOptions()...)
		sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, host), remoteCmd)
		cmd = exec.CommandContext(ctx, "ssh", sshArgs...)
		cmd.Env = vp.env
	}

	cmd.Stdin = r
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard

	log.Printf("DEBUG Executing SSH stdin stream: ssh %s@%s %q", user, host, remoteCmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// shellQuote single-quotes a path for safe interpolation into a remote shell
// command, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// qemuImgStderr formats a VirshResult's stderr for appending to a wrapped error
// so the underlying qemu-img message is surfaced instead of being masked. It
// returns "" when there is no result or no stderr, keeping the error tidy.
func qemuImgStderr(res *VirshResult) string {
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
