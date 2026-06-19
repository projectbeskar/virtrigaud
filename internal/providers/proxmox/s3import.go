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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// proxmoxImportStageDir is the directory on the PVE node into which the import
// path stages the downloaded qcow2 before the Create path consumes it with
// `qm importdisk`. /var/tmp is a real on-disk path on every PVE node, local to
// the node, and large enough for a multi-GB disk. The staged file persists across
// the ImportDisk → Create handoff (two separate RPCs); Create removes it after a
// successful importdisk, and a best-effort sweep removes stale stages.
const proxmoxImportStageDir = "/var/tmp"

// importDiskFromS3 implements the ADR-0006 Proxmox TARGET path: download the
// staged qcow2 object from S3 and STAGE it as a regular file on the PVE node, then
// return that node path. It is the symmetric sibling of exportDiskToS3.
//
// The disk never lands in a temp file in the pod and never traverses a CSI PVC:
// bytes flow S3 → pod → SSH stdin → node (`cat > <hostTmp>`). Integrity is
// verified in-stream against the source-reported checksum (ADR D5), then
// `qemu-img check` validates the staged qcow2 on the node.
//
// IMPORT MECHANISM (DESIGN DECISION). Proxmox's `qm importdisk <vmid> <file>
// <storage>` requires an EXISTING VM to attach the disk to, but the migration
// flow is Importing(ImportDisk) → Creating(Create) — at ImportDisk time no target
// VM exists yet. Two options were considered:
//
//	(a) ImportDisk stages the qcow2 to a node path and returns that path; the
//	    Proxmox Create path then runs `qm importdisk` into the freshly-created VM.
//	(b) ImportDisk creates a throwaway VM, importdisk into it, then detach — so it
//	    returns an already-imported volid.
//
// We chose (a) — staging — for three reasons: it mirrors how the controller
// already threads the path (ImportDisk returns Path → VMMigration
// Status.DiskInfo.TargetPath → VirtualMachine Spec.ImportedDisk.Path → Create's
// image_json Path, the SAME contract vSphere uses); it avoids leaking a throwaway
// VMID on a failure between the two RPCs; and `qm importdisk` only makes sense
// against the real target VM (it lays the disk down in that VM's storage with the
// correct vm-<vmid>-disk-N naming). The Create path (server.go) consumes the
// staged path: see attachImportedDisk.
//
// The staged qcow2 lands transiently on the node (node disk usage = staged
// qcow2 until Create consumes it). True streaming that avoids the node-side stage
// is an ADR-0006 follow-up. Crash-resume is OUT of scope: a failure retries the
// whole import.
func (p *Provider) importDiskFromS3(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}
	if p.ssh == nil {
		return nil, errors.NewUnavailable("migration SSH data plane not configured (set PROVIDER_ENDPOINT and SSH credentials)", nil)
	}

	// Source format: the staged object is the SOURCE provider's native flattened
	// format. For a libvirt/proxmox source it is qcow2; vSphere stages vmdk. The
	// controller threads the staged format via req.Format; default to qcow2.
	srcFormat := req.Format
	if srcFormat == "" {
		srcFormat = "qcow2"
	}
	if srcFormat != "qcow2" && srcFormat != "raw" && srcFormat != "vmdk" {
		return nil, errors.NewInvalidSpec("unsupported s3 import source format: %s", srcFormat)
	}

	id := sanitizeProxmoxName(req.TargetName)
	if req.TargetName == "" {
		id = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}

	// Build the S3 client (the pod is the S3 client). Options come from
	// storage_options_json; credentials from the credentials map. Never logged.
	storageConfig, err := migration.S3StorageConfigFromRequest(req.StorageOptionsJson, req.Credentials)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid s3 import configuration: %v", err)
	}
	s3client, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create s3 client", err)
	}
	defer s3client.Close()

	stagePath := proxmoxImportStagePath(id, srcFormat)

	// Never log credentials — only the backend, paths, format.
	p.logger.Info("Importing disk from S3 to Proxmox node (stage for qm importdisk at Create)",
		"backend", req.BackendType, "id", id, "source", req.SourceUrl,
		"src_format", srcFormat, "stage_path", stagePath, "storage_hint", req.StorageHint)

	// --- STAGE (ADR D5) ---
	// S3 → SSH stdin → `cat > <stagePath>` on the node. cat writes sequentially
	// (no seek), so the non-seekable pipe is fine. The pipe couples DownloadStream
	// (SHA256 verified in-stream) to the SSH stdin so the disk is never buffered
	// whole in the pod.
	pr, pw := io.Pipe()
	stageCmd := fmt.Sprintf("cat > %s", proxmoxShellQuote(stagePath))

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
		_ = pw.CloseWithError(derr)
		dlCh <- dlResult{resp: resp, err: derr}
	}()

	stageErr := p.ssh.runSSHStdin(ctx, pr, stageCmd)
	_ = pr.CloseWithError(io.ErrClosedPipe)
	dl := <-dlCh

	// Surface the REAL stage failure: the download/checksum error is the root
	// cause when the stream broke; only on a clean download do we attribute a
	// stage failure to the node `cat`. On ANY stage failure best-effort remove the
	// partial stage so a failed import leaves no multi-GB temp.
	if dl.err != nil || stageErr != nil {
		p.cleanupNodeFile(stagePath)
	}
	if dl.err != nil {
		return nil, errors.NewInternal("s3 download/transfer failed during stage", dl.err)
	}
	if stageErr != nil {
		return nil, errors.NewInternal(fmt.Sprintf("node-side stage (cat to %s) failed", stagePath), stageErr)
	}

	p.logger.Info("S3 object staged on node",
		"bytes", dl.resp.BytesTransferred, "sha256_verified", req.ExpectedChecksum != "")

	// --- VALIDATE (ADR D5) ---
	// qemu-img check on the staged qcow2 (only meaningful for qcow2; raw has no
	// internal structure to check). Surface stderr on failure.
	if srcFormat == "qcow2" {
		if _, stderr, err := p.ssh.runSSH(ctx, fmt.Sprintf("qemu-img check %s", proxmoxShellQuote(stagePath))); err != nil {
			p.cleanupNodeFile(stagePath)
			return nil, errors.NewInternal(
				fmt.Sprintf("qemu-img check failed on staged qcow2 %s (stderr: %s)", stagePath, strings.TrimSpace(stderr)), err)
		}
	}

	p.logger.Info("Disk import from S3 staged on node (awaiting Create → qm importdisk)",
		"id", id, "stage_path", stagePath, "bytes", dl.resp.BytesTransferred)

	// Return the node STAGE PATH as Path. The migration controller propagates it
	// to VirtualMachine Spec.ImportedDisk.Path; the Proxmox Create path consumes
	// it via attachImportedDisk (qm importdisk into the freshly-created VM).
	return &providerv1.ImportDiskResponse{
		DiskId:          id,
		Path:            stagePath,
		Task:            nil, // synchronous
		ActualSizeBytes: dl.resp.BytesTransferred,
		Checksum:        dl.resp.Checksum, // SHA256 of the transferred qcow2
	}, nil
}

// cleanupNodeFile best-effort removes a node-side file over SSH and WARNs on
// failure. Used to clean up a partial/leaked stage so a failed import does not
// leave a multi-GB temp on the node.
func (p *Provider) cleanupNodeFile(path string) {
	if p.ssh == nil || path == "" {
		return
	}
	if _, _, err := p.ssh.runSSH(context.Background(), fmt.Sprintf("rm -f %s", proxmoxShellQuote(path))); err != nil {
		p.logger.Warn("Failed to remove node-side temp (manual cleanup may be needed)", "path", path, "error", err)
	}
}

// proxmoxImportStagePath returns the path of the node-side staging file for an S3
// import. It lives in proxmoxImportStageDir under a dot-prefixed name keyed on the
// import id (deterministic per migration so a retry overwrites the same stage
// rather than leaking). The suffix matches the staged object's format.
func proxmoxImportStagePath(id, srcFormat string) string {
	return fmt.Sprintf("%s/.virtrigaud-import-%s.%s",
		strings.TrimRight(proxmoxImportStageDir, "/"), sanitizeProxmoxName(id), srcFormat)
}

// proxmoxImportTargetStorage is the default PVE storage `qm importdisk` lands the
// imported disk into when neither the request nor the VMConfig names one.
const proxmoxImportTargetStorage = "local-lvm"

// proxmoxImportedDiskInterface is the disk bus the imported disk is attached to.
// virtio-scsi (scsi0) is the broadly-compatible, performant default for an
// imported Linux guest. The matching controller (--scsihw virtio-scsi-pci) is set
// in the same `qm set`.
const proxmoxImportedDiskInterface = "scsi0"

// createFromImportedDisk implements the ADR-0006 Proxmox TARGET Create path:
// build a bare VM shell, attach the pre-staged disk with `qm importdisk` over
// SSH, set it as the boot disk, wire cloud-init, and clean up the stage. It is
// invoked from Create when vmConfig.ImportedDiskPath is set.
//
// SEQUENCE (each step is greppable in logs; secrets never logged):
//  1. CreateVM(vmConfig) — a diskless shell with the requested cpu/mem/net (the
//     PVE API has no disk param here, so no template disk is laid down).
//  2. `qm importdisk <vmid> <stagedPath> <storage> --format qcow2` — lays the
//     staged qcow2 into the VM's storage as an UNUSED disk (unused0).
//  3. `qm set <vmid> --scsihw virtio-scsi-pci --scsi0 <storage>:<imported-volume>
//     --boot order=scsi0 [--ide2 <storage>:cloudinit --ciuser/--sshkeys]` — wires
//     the imported disk as the boot disk and attaches cloud-init when present.
//  4. Remove the node-side stage (best-effort).
//
// On a failure AFTER the shell is created, the shell VMID is best-effort deleted
// so a failed import does not leak a half-built VM. The staged disk is left in
// place on failure so a retry can re-attach it (the stage path is deterministic).
func (p *Provider) createFromImportedDisk(
	ctx context.Context,
	req *providerv1.CreateRequest,
	vmConfig *pveapi.VMConfig,
	node string,
) (*providerv1.CreateResponse, error) {
	if p.ssh == nil {
		return nil, errors.NewUnavailable("migration SSH data plane not configured; cannot attach imported disk via qm importdisk", nil)
	}

	// Resolve the target storage: explicit VM config wins, then the provider's
	// configured default (PROVIDER_DEFAULT_STORAGE / PVE_DEFAULT_STORAGE), then the
	// compiled-in fallback. A PVE node may not have local-lvm (e.g. a dir-storage
	// node), so the default must be operator-configurable.
	storageName := vmConfig.Storage
	if storageName == "" && p.client != nil {
		storageName = p.client.Config().DefaultStorage
	}
	if storageName == "" {
		storageName = proxmoxImportTargetStorage
	}
	importFormat := vmConfig.ImportedDiskFormat
	if importFormat == "" {
		importFormat = "qcow2"
	}

	p.logger.Info("Creating VM from imported disk (ADR-0006 Proxmox TARGET)",
		"vmid", vmConfig.VMID, "name", req.Name, "node", node,
		"stage_path", vmConfig.ImportedDiskPath, "storage", storageName, "format", importFormat)

	// 1. Bare VM shell (diskless). Clear any template/clone hints so this is a
	// from-scratch create, not a clone.
	vmConfig.Template = ""
	vmConfig.Clone = ""
	taskID, err := p.client.CreateVM(ctx, node, vmConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create VM shell for imported disk", err)
	}
	if taskID != "" {
		if err := p.client.WaitForTask(ctx, node, taskID); err != nil {
			return nil, errors.NewInternal("VM shell create task failed", err)
		}
	}

	// From here on, on ANY failure best-effort delete the half-built shell so a
	// failed import leaves no orphan VM. The staged disk is intentionally kept.
	attachOK := false
	defer func() {
		if attachOK {
			return
		}
		// purge=true so the rollback also reclaims any disk already imported into
		// the half-built shell (the shell is never started, so no stop is needed).
		delTask, derr := p.client.DeleteVM(context.Background(), node, vmConfig.VMID, true)
		if derr != nil {
			p.logger.Warn("Failed to delete half-built VM shell after imported-disk failure (manual cleanup may be needed)",
				"vmid", vmConfig.VMID, "error", derr)
			return
		}
		if delTask != "" {
			_ = p.client.WaitForTask(context.Background(), node, delTask)
		}
	}()

	// 2. importdisk: lay the staged qcow2 into the VM's storage as an unused disk.
	importCmd := buildImportDiskCommand(vmConfig.VMID, vmConfig.ImportedDiskPath, storageName, importFormat)
	importStdout, importStderr, err := p.ssh.runSSH(ctx, importCmd)
	if err != nil {
		return nil, errors.NewInternal(
			fmt.Sprintf("qm importdisk failed for vmid %d (stderr: %s)", vmConfig.VMID, strings.TrimSpace(importStderr)), err)
	}

	// 3a. Attach the imported disk as the virtio-scsi boot disk over SSH. The volid
	// `qm importdisk` created differs by storage type (a directory store uses
	// "<storage>:<vmid>/vm-<vmid>-disk-0.<ext>", LVM/ZFS use
	// "<storage>:vm-<vmid>-disk-0"), so resolve it from the VM config's `unusedN:`
	// entry — robust across PVE versions + storage types. Fall back to parsing
	// importdisk's own output, then the conventional name.
	importedVolume := ""
	if cfgOut, _, cfgErr := p.ssh.runSSH(ctx, fmt.Sprintf("qm config %d", vmConfig.VMID)); cfgErr == nil {
		importedVolume = parseUnusedVolid(cfgOut)
	}
	if importedVolume == "" {
		importedVolume = parseImportedVolid(importStdout + "\n" + importStderr)
	}
	if importedVolume == "" {
		importedVolume = importedDiskVolume(storageName, vmConfig.VMID)
		p.logger.Warn("Could not resolve imported volid from qm config or importdisk output; using conventional name",
			"vmid", vmConfig.VMID, "assumed_volume", importedVolume)
	}
	setCmd := buildImportedDiskSetCommand(vmConfig.VMID, importedVolume)
	if _, stderr, err := p.ssh.runSSH(ctx, setCmd); err != nil {
		return nil, errors.NewInternal(
			fmt.Sprintf("qm set (attach imported disk) failed for vmid %d (stderr: %s)", vmConfig.VMID, strings.TrimSpace(stderr)), err)
	}

	// 3b. Wire cloud-init via the PVE API (ReconfigureVMRaw url-encodes the values
	// — notably multi-line SSH keys — the SAME way the clone+cloud-init Create
	// path does). Doing this over the API rather than `qm set --sshkeys` avoids
	// the shell/PVE encoding pitfalls of passing a multi-line key on a command
	// line. Only attached when the migration delivered user-data, so a migrated
	// VM that brings its own configured guest is not forced onto cloud-init.
	if len(req.UserData) > 0 {
		ciValues := buildImportedDiskCloudInitValues(storageName, vmConfig)
		if len(ciValues) > 0 {
			ciTask, ciErr := p.client.ReconfigureVMRaw(ctx, node, vmConfig.VMID, ciValues)
			if ciErr != nil {
				return nil, errors.NewInternal(
					fmt.Sprintf("failed to attach cloud-init to imported-disk VM %d", vmConfig.VMID), ciErr)
			}
			if ciTask != "" {
				if err := p.client.WaitForTask(ctx, node, ciTask); err != nil {
					return nil, errors.NewInternal("cloud-init reconfigure task failed", err)
				}
			}
		}
	}

	attachOK = true

	// 4. Clean up the node-side stage (best-effort). The disk now lives in PVE
	// storage as a managed volume; the stage temp is no longer needed.
	p.cleanupNodeFile(vmConfig.ImportedDiskPath)

	p.logger.Info("VM created from imported disk", "vmid", vmConfig.VMID, "boot_disk", importedVolume)

	return &providerv1.CreateResponse{
		Id: fmt.Sprintf("%d", vmConfig.VMID),
	}, nil
}

// importedDiskVolume returns the conventional PVE volid for the first imported
// disk of a VM on a block storage (LVM/ZFS): "<storage>:vm-<vmid>-disk-0". It is
// only a FALLBACK for parseImportedVolid — a directory storage uses a different
// shape ("<storage>:<vmid>/vm-<vmid>-disk-0.<ext>"), so always prefer the volid
// parsed from the actual `qm importdisk` output.
func importedDiskVolume(storage string, vmid int) string {
	return fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid)
}

// importedVolidRe matches the volid `qm importdisk` reports it created, e.g.:
//
//	Successfully imported disk as 'unused0:dir_store:910544/vm-910544-disk-0.raw'
//
// capturing the volid with the leading "unusedN:" assignment stripped (the form
// `qm set --scsiN <volid>` expects).
var importedVolidRe = regexp.MustCompile(`imported disk as ['"]?(?:unused\d+:)?([^'"\s]+)`)

// parseImportedVolid extracts the PVE volid from `qm importdisk` output, working
// for every storage type (it uses whatever importdisk actually created rather
// than assuming a naming scheme). Returns "" when no volid can be parsed.
func parseImportedVolid(output string) string {
	if m := importedVolidRe.FindStringSubmatch(output); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// unusedVolidRe matches the volid on an `unusedN:` line of `qm config` output:
//
//	unused0: dir_store:911288/vm-911288-disk-0.raw
var unusedVolidRe = regexp.MustCompile(`(?m)^unused\d+:\s*(\S+)`)

// parseUnusedVolid returns the volid of the first `unusedN:` entry in `qm config`
// output — the disk `qm importdisk` just laid down on a fresh VM — or "" if none.
// This is the version- and storage-robust way to learn the imported disk's volid.
func parseUnusedVolid(qmConfig string) string {
	if m := unusedVolidRe.FindStringSubmatch(qmConfig); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// buildImportDiskCommand builds the `qm importdisk` command that lays the staged
// disk image into the VM's storage. The path and storage are shell-quoted; the
// format is passed explicitly so PVE does not have to probe. It is a free
// function so the exact argv is unit-testable without SSH.
func buildImportDiskCommand(vmid int, stagePath, storage, format string) string {
	return fmt.Sprintf("qm importdisk %d %s %s --format %s",
		vmid, proxmoxShellQuote(stagePath), proxmoxShellQuote(storage), proxmoxShellQuote(format))
}

// buildImportedDiskSetCommand builds the `qm set` command that attaches the
// imported volume as the virtio-scsi boot disk. It sets ONLY simple,
// encoding-safe values (the disk volid and the boot order); cloud-init is wired
// separately via the API (buildImportedDiskCloudInitValues) so multi-line SSH
// keys are url-encoded correctly. It is a free function so the exact argv is
// unit-testable without SSH.
func buildImportedDiskSetCommand(vmid int, importedVolume string) string {
	parts := []string{
		fmt.Sprintf("qm set %d", vmid),
		"--scsihw virtio-scsi-pci",
		fmt.Sprintf("--%s %s", proxmoxImportedDiskInterface, proxmoxShellQuote(importedVolume)),
		fmt.Sprintf("--boot order=%s", proxmoxImportedDiskInterface),
	}
	return strings.Join(parts, " ")
}

// buildImportedDiskCloudInitValues builds the url.Values for the API
// ReconfigureVMRaw call that attaches a cloud-init drive and propagates the SSH
// keys / user already parsed into vmConfig. Using url.Values (the same path the
// clone+cloud-init Create flow uses) means PVE receives correctly-encoded values,
// notably multi-line SSH keys, with none of the shell/command-line pitfalls of
// `qm set --sshkeys`. Returns an empty map when there is nothing to attach. It is
// a free function so the exact values are unit-testable without a live PVE.
func buildImportedDiskCloudInitValues(storage string, vmConfig *pveapi.VMConfig) url.Values {
	values := url.Values{}
	values.Set("ide2", storage+":cloudinit")
	if vmConfig.CIUser != "" {
		values.Set("ciuser", vmConfig.CIUser)
	}
	if vmConfig.SSHKeys != "" {
		values.Set("sshkeys", strings.TrimSpace(vmConfig.SSHKeys))
	}
	return values
}
