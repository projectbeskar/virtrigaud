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
	"strings"
	"time"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// exportDiskToNFS implements the ADR-0006 Slice 4 Proxmox SOURCE path for the NFS
// backend: the PVE NODE's qemu-img flattens the source disk and writes the
// resulting qcow2 directly to the NFS export over libnfs (qemu's block-nfs
// driver), with no provider-pod hop and no S3 client. This mirrors the libvirt
// host-side NFS export — the node already has the disk and NFS reachability, so
// the bytes never transit the pod (unlike the S3 relay). The destination is the
// controller-built, C7'-hardened nfs:// URL.
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

	// Resolve the real on-node filesystem path from the volid via `pvesm path`.
	srcPath, err := p.resolveNodeDiskPath(ctx, volid)
	if err != nil {
		return nil, errors.NewInternal("failed to resolve on-node disk path", err)
	}

	// Never log credentials — only the backend, volid, paths.
	p.logger.Info("Exporting disk from Proxmox node to NFS",
		"backend", req.BackendType, "vm", req.VmId, "volid", volid, "src_path", srcPath,
		"destination", nfsURL)

	// Flatten + write straight to the NFS export via the node's qemu-img (libnfs).
	// Reuse the tested S3-export flatten builder: it emits `qemu-img convert -U -f
	// <srcFormat> -O qcow2 '<src>' '<dst>'` with both operands shell-quoted — the
	// destination being an nfs:// URL instead of a node temp is immaterial to the
	// builder, and we inherit its quoting guarantee for the security-sensitive URL.
	// -U reads a possibly-running source (crash-consistent; a consistent copy still
	// needs power-off/snapshot first). No hostTmp, no pod buffering — qemu-img's
	// block-nfs driver performs the NFS write directly. Compression stays off: the
	// target qcow2 lands on the NFS export for the peer to read, not a local cache.
	convertCmd := buildExportFlattenCommand(srcFormat, srcPath, nfsURL, false)
	if _, stderr, err := p.ssh.runSSH(ctx, convertCmd); err != nil {
		return nil, errors.NewInternal(
			fmt.Sprintf("node-side qemu-img convert to nfs failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}

	p.logger.Info("Source disk written to NFS export", "destination", nfsURL)

	return &providerv1.ExportDiskResponse{
		ExportId: fmt.Sprintf("export-proxmox-nfs-%s-%d", sanitizeProxmoxName(req.VmId), time.Now().Unix()),
		// qemu-img does not emit a byte SHA256 over the libnfs write; NFS integrity
		// is established by the target's `qemu-img check` on the converted qcow2
		// (ADR-0006 Slice 4 — no in-stream checksum for the qemu-img transport).
	}, nil
}

// importDiskFromNFS implements the ADR-0006 Slice 4 Proxmox TARGET path: the PVE
// node's qemu-img reads the staged qcow2 directly from the NFS export over libnfs
// and writes it to a node-local stage file, which the Proxmox Create path then
// feeds to `qm importdisk` (Proxmox cannot importdisk an nfs:// URL directly — it
// takes a local file). No download via the pod, no S3 client; the bytes flow
// NFS → node, not NFS → pod → node.
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

	// NFS always stages qcow2 (the controller derives import format qcow2 for the
	// nfs backend; the source provider's export wrote qcow2 to the export).
	id := sanitizeProxmoxName(req.TargetName)
	if req.TargetName == "" {
		id = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}
	stagePath := proxmoxImportStagePath(id, "qcow2")

	// Never log credentials — only the paths.
	p.logger.Info("Importing disk from NFS to Proxmox node (stage for qm importdisk at Create)",
		"id", id, "source", nfsURL, "stage_path", stagePath, "storage_hint", req.StorageHint)

	// Read the staged qcow2 straight from NFS and write the node-local stage file.
	// `qm importdisk` (run by the Create path) needs a local file, not an nfs:// URL.
	convertCmd := buildNFSImportConvertCommand(nfsURL, stagePath)
	if _, stderr, err := p.ssh.runSSH(ctx, convertCmd); err != nil {
		p.cleanupNodeFile(stagePath)
		return nil, errors.NewInternal(
			fmt.Sprintf("node-side qemu-img convert from nfs failed (stderr: %s)", strings.TrimSpace(stderr)), err)
	}

	// Validate the staged qcow2 on the node (ADR-0006 D5 structural integrity for
	// NFS — there is no in-stream byte checksum from qemu-img).
	if _, stderr, err := p.ssh.runSSH(ctx, fmt.Sprintf("qemu-img check %s", proxmoxShellQuote(stagePath))); err != nil {
		p.cleanupNodeFile(stagePath)
		return nil, errors.NewInternal(
			fmt.Sprintf("qemu-img check failed on staged qcow2 %s (stderr: %s)", stagePath, strings.TrimSpace(stderr)), err)
	}

	p.logger.Info("Disk import from NFS staged on node (awaiting Create → qm importdisk)",
		"id", id, "stage_path", stagePath)

	// The disk is staged on the node. The Proxmox Create path consumes it via
	// attachImportedDisk (qm importdisk into the freshly-created VM).
	return &providerv1.ImportDiskResponse{
		DiskId: id,
		Path:   stagePath,
		Task:   nil, // synchronous
		// No ActualSizeBytes/Checksum: qemu-img emits no byte SHA256 over the libnfs
		// read; integrity is the node-side qemu-img check above (ADR-0006 Slice 4).
	}, nil
}

// buildNFSImportConvertCommand builds the node-side qemu-img argv that reads the
// staged qcow2 from the nfs:// source and writes the node-local stage file for
// `qm importdisk`. The source is a static staged file (not a running disk), so no
// -U is needed; both the nfs:// URL and the stage path are shell-quoted (the URL
// is security-sensitive — it is C7'-hardened by the controller, and this is the
// single place its node-side quoting is pinned).
func buildNFSImportConvertCommand(nfsURL, stagePath string) string {
	return fmt.Sprintf("qemu-img convert -f qcow2 -O qcow2 %s %s",
		proxmoxShellQuote(nfsURL), proxmoxShellQuote(stagePath))
}
