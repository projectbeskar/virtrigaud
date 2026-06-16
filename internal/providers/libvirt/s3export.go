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
// they flow host (`cat <hostTmp.qcow2>`) → SSH stdout → pod → S3 via an
// io.Pipe coupling runSSHStdout to storage.UploadStream (SHA256 computed
// in-stream). Integrity is the in-stream SHA256 of the staged qcow2, reported
// as the Checksum the target verifies on download (ADR D5).
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
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	vp := libvirtProvider.virshProvider

	if !strings.Contains(vp.uri, "ssh://") {
		// The flatten + stream-out runs on the libvirt host over SSH. A local
		// connection is not the Slice 2 source shape.
		return nil, fmt.Errorf("s3 export requires an ssh:// libvirt transport (host-side qemu-img flatten + stream); got %q", vp.uri)
	}

	// Resolve the source disk path on the host via GetDiskInfo (e.g. for
	// demo-ubuntu-libvirt vda=/var/lib/libvirt/images/ubuntu-libvirt-demo.qcow2).
	diskInfo, err := libvirtProvider.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{
		VmId:       req.VmId,
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
	hostTmp := hostExportStagePath(srcPath, req.VmId)

	log.Printf("INFO Exporting disk from libvirt host to S3: backend=s3 vm=%s src=%s hostTmp=%s dest=%s",
		req.VmId, srcPath, hostTmp, req.DestinationUrl)

	// --- FLATTEN (ADR D4) ---
	// Collapse the (possibly snapshot-overlay) backing chain into one standalone
	// qcow2 on the host. -f qcow2 forces the source driver (no format probing of
	// the overlay); -O qcow2 keeps the native format the target expects. -U skips
	// the shared-disk lock so a still-running source (e.g. powerOffBeforeMigration
	// not yet honored, or createSnapshot=false) can be read — this is a
	// crash-consistent copy; a consistent copy still requires the source to be
	// powered off or snapshotted first.
	if res, err := vp.runVirshCommand(ctx, "!", "qemu-img", "convert", "-U", "-f", "qcow2", "-O", "qcow2",
		shellQuote(srcPath), shellQuote(hostTmp)); err != nil {
		return nil, fmt.Errorf("host-side qemu-img flatten (qcow2→standalone qcow2) failed: %w%s", err, qemuImgStderr(res))
	}

	// Cleanup the flattened temp ALWAYS — success or failure — so a failed export
	// never leaks a multi-GB temp on the host. Best-effort; WARN on failure.
	defer func() {
		if _, rmErr := vp.runVirshCommand(context.Background(), "!", "rm", "-f", hostTmp); rmErr != nil {
			log.Printf("WARN failed to remove flattened export temp %s on host (manual cleanup may be needed): %v",
				hostTmp, rmErr)
		}
	}()

	log.Printf("INFO Source disk flattened to standalone qcow2 on host: hostTmp=%s", hostTmp)

	// --- STREAM (ADR D5) ---
	// Stream host (`cat <hostTmp.qcow2>` → SSH stdout) → pod → S3. The pipe
	// couples runSSHStdout's stdout to storage.UploadStream so the disk is never
	// buffered whole in the pod; UploadStream computes the SHA256 in-stream.
	pr, pw := io.Pipe()
	streamCmd := fmt.Sprintf("cat %s", shellQuote(hostTmp))

	type ulResult struct {
		resp storage.UploadResponse
		err  error
	}
	ulCh := make(chan ulResult, 1)
	go func() {
		resp, uerr := s3client.UploadStream(ctx, storage.StreamUploadRequest{
			DestinationURL: req.DestinationUrl,
			Reader:         pr,
			ContentLength:  -1, // size unknown; minio streaming auto-multipart
		})
		// Closing the read end with the upload error unblocks the SSH/cat side if
		// the upload failed mid-stream (broken pipe) so it returns promptly.
		_ = pr.CloseWithError(uerr)
		ulCh <- ulResult{resp: resp, err: uerr}
	}()

	streamErr := runSSHStdout(ctx, vp, pw, streamCmd)
	// Close the write end so UploadStream sees EOF (clean) or, on a cat failure,
	// an error that aborts the upload rather than uploading a truncated object.
	_ = pw.CloseWithError(streamErr)
	ul := <-ulCh

	// Surface the REAL failure: a host-side `cat` failure is the root cause when
	// the stream broke at the source; only if the host side was clean do we
	// attribute a stream failure to the S3 upload.
	if streamErr != nil {
		return nil, fmt.Errorf("host-side stream (cat %s) failed: %w", hostTmp, streamErr)
	}
	if ul.err != nil {
		return nil, fmt.Errorf("s3 upload failed during stream: %w", ul.err)
	}

	log.Printf("INFO Disk export to S3 completed: export_id=%s bytes=%d checksum=%s",
		exportID, ul.resp.BytesTransferred, ul.resp.Checksum)

	return &providerv1.ExportDiskResponse{
		ExportId:           exportID,
		Task:               nil, // synchronous
		EstimatedSizeBytes: ul.resp.BytesTransferred,
		Checksum:           ul.resp.Checksum, // SHA256 of the staged (qcow2) object
	}, nil
}

// runSSHStdout runs a single command on the libvirt host over SSH, streaming the
// command's stdout into w (a pipe), and returns when the command exits. It is
// the symmetric sibling of runSSHStdin: instead of wiring a reader to the
// remote process's stdin (cmd.Stdin = r) it wires the remote process's stdout to
// w (cmd.Stdout = w), so a multi-GB disk streams OUT of the host without being
// buffered in the pod. It reuses the same host-key policy and ControlMaster
// multiplexing as runSSHStdin / the virsh/scp paths (#149/ADR-0004, #194) so
// trust material and connections are shared.
func runSSHStdout(ctx context.Context, vp *VirshProvider, w io.Writer, remoteCmd string) error {
	parsedURI, err := url.Parse(vp.uri)
	if err != nil {
		return fmt.Errorf("failed to parse libvirt URI: %w", err)
	}
	host := parsedURI.Host
	user := parsedURI.User.Username()

	// Host-key pre-flight: re-emit the audit line and hard-fail if verification
	// is on but no usable known_hosts is present (no TOFU), matching scp/stdin.
	vp.hostKey.logVerificationMode(vp.logger, host)
	if err := vp.hostKey.verifyKnownHostsPresent(host); err != nil {
		return fmt.Errorf("ssh stdout host-key verification pre-flight failed: %w", err)
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
		sshArgs = append(sshArgs, vp.hostKey.sshHostKeyOptions()...)
		sshArgs = append(sshArgs, sshMultiplexOptions()...)
		sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", user, host), remoteCmd)
		cmd = exec.CommandContext(ctx, "ssh", sshArgs...)
		cmd.Env = vp.env
	}

	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdin = nil

	log.Printf("DEBUG Executing SSH stdout stream: ssh %s@%s %q", user, host, remoteCmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// hostExportStagePath returns the path of the transient host-side flattened
// qcow2 for an S3 export. It lives in the SAME directory as the source disk (so
// the flatten convert reads/writes within one filesystem, no cross-device copy)
// under a dot-prefixed, unix-ts-suffixed name so it is distinguishable, hidden
// from a casual directory listing, and unlikely to collide with a real volume.
// The .qcow2 suffix matches the staged (and uploaded) object's format.
func hostExportStagePath(srcPath, vmID string) string {
	dir := srcPath
	if idx := strings.LastIndex(srcPath, "/"); idx >= 0 {
		dir = srcPath[:idx]
	}
	dir = strings.TrimRight(dir, "/")
	return fmt.Sprintf("%s/.virtrigaud-export-%s-%d.qcow2", dir, sanitizeVolumeName(vmID), time.Now().Unix())
}
