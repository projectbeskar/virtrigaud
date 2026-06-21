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

package vsphere

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vmdk"

	"github.com/projectbeskar/virtrigaud/internal/diskutil"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// exportLocalVMDKToNFS writes a self-contained local VMDK (already downloaded and
// flattened by the ExportDisk pipeline) to the nfs:// export as qcow2, using the
// provider POD's qemu-img over libnfs (qemu's block-nfs driver). This is the
// vSphere SOURCE leg of the ADR-0006 Slice 4 NFS backend.
//
// Unlike libvirt/Proxmox — where a host/node-side qemu-img writes nfs:// — vSphere
// has no shell on the hypervisor, so it runs qemu-img IN THE POD, the same place it
// already converts qcow2→vmdk for the S3 import. The convert reads the local vmdk
// and writes qcow2 straight to nfs://; no S3 client, no second upload. The NFS
// backend always stages qcow2 (the controller derives import format qcow2 for nfs,
// matching what the libvirt/Proxmox TARGETs read).
//
// diskutil runs qemu-img via exec with an argv slice (no shell), so the nfs:// URL
// — built and C7'-hardened by the controller — passes through as a single argument
// with no shell-injection surface.
func (p *Provider) exportLocalVMDKToNFS(ctx context.Context, localVMDKPath, nfsURL string) error {
	qemuImg := diskutil.NewQemuImg()
	if !qemuImg.IsInstalled() {
		return fmt.Errorf("qemu-img is not available in the provider image; cannot write nfs export")
	}
	if err := qemuImg.Convert(ctx, diskutil.ConvertOptions{
		SourcePath:        localVMDKPath,
		DestinationPath:   nfsURL,
		SourceFormat:      diskutil.FormatVMDK,
		DestinationFormat: diskutil.FormatQCOW2,
		Compression:       false, // staged qcow2 is read once by the peer; skip compress
	}); err != nil {
		return fmt.Errorf("qemu-img convert vmdk -> nfs qcow2: %w", err)
	}
	return nil
}

// importDiskFromNFS implements the ADR-0006 Slice 4 vSphere TARGET path for the NFS
// backend: the provider POD's qemu-img reads the staged qcow2 directly from the
// nfs:// export over libnfs and converts it — in a single pass — to the
// streamOptimized vmdk the vCenter NFC HttpNfcLease import requires, then imports
// it onto a datastore as a native thin disk via the SAME NFC lease path the S3
// import uses (p.nfcImportStreamOptimized — see s3import.go). It is the NFS
// counterpart of importDiskFromS3; the only difference is the source leg (qemu-img
// nfs:// read instead of an S3 download + separate convert).
//
// MECHANISM:
//  1. qemu-img convert nfs://…qcow2 → /tmp/<id>.vmdk (-O vmdk subformat=streamOptimized).
//     streamOptimized is MANDATORY for the NFC lease (vmdk.Stat rejects any other
//     subformat with ErrInvalidFormat). block-nfs.so (qemu-block-extra) performs
//     the NFS read.
//  2. Resolve datastore + resource pool + folder + host placement.
//  3. nfcImportStreamOptimized: OVF descriptor → HttpNfcLease → upload → detach +
//     unregister the transient import VM, leaving "[<ds>] <id>/<id>.vmdk".
//  4. QueryVirtualDiskUuid(final) — an error/empty uuid means a corrupt import.
//  5. On any post-lease failure, best-effort delete the "[<ds>] <id>" folder.
//
// NFS integrity is the post-import uuid query (qemu-img emits no in-stream byte
// checksum over the libnfs read), so the ImportDiskResponse carries no Checksum.
func (p *Provider) importDiskFromNFS(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("vSphere client not configured", nil)
	}

	nfsURL := strings.TrimSpace(req.SourceUrl)
	if !strings.HasPrefix(nfsURL, "nfs://") {
		return nil, errors.NewInvalidSpec("nfs import source must be an nfs:// URL, got %q", nfsURL)
	}

	// <id> is the stable name for the temp vmdk, the import VM/disk name, the final
	// datastore path, and the returned DiskId. vmdk.Import derives the on-datastore
	// disk name from the local vmdk basename, so the local file MUST be "<id>.vmdk".
	id := req.TargetName
	if id == "" {
		id = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}

	p.logger.Info("Importing disk from NFS (qemu-img nfs:// -> streamOptimized vmdk + NFC lease)",
		"id", id, "source", nfsURL, "backend", req.BackendType, "storage_hint", req.StorageHint)

	// --- 1. READ + CONVERT in one pass: nfs:// qcow2 -> streamOptimized vmdk ---
	// The pod's qemu-img reads the staged qcow2 directly from nfs:// (block-nfs) and
	// writes the streamOptimized vmdk the NFC lease requires. No S3 download. The
	// nfs:// URL passes through diskutil's exec argv unquoted-but-unshelled (no
	// injection surface); it is C7'-hardened by the controller.
	vmdkLocal := fmt.Sprintf("/tmp/%s.vmdk", id)
	defer func() { _ = os.Remove(vmdkLocal) }()

	qemuImg := diskutil.NewQemuImg()
	if !qemuImg.IsInstalled() {
		return nil, errors.NewInternal("qemu-img is not available in the provider image; cannot convert nfs import", nil)
	}
	if err := qemuImg.Convert(ctx, diskutil.ConvertOptions{
		SourcePath:        nfsURL,
		DestinationPath:   vmdkLocal,
		SourceFormat:      diskutil.FormatQCOW2,
		DestinationFormat: diskutil.FormatVMDK,
		Subformat:         s3ImportSubformat, // streamOptimized — MANDATORY for the NFC lease
	}); err != nil {
		return nil, errors.NewInternal("failed to convert nfs import to streamOptimized vmdk", err)
	}
	// Fail fast if the converted file is not the streamOptimized format the NFC lease
	// requires (defends against a qemu-img default change or a Subformat regression).
	if _, statErr := vmdk.Stat(vmdkLocal); statErr != nil {
		return nil, errors.NewInternal("converted vmdk is not a valid streamOptimized image for NFC import", statErr)
	}

	// --- 2. RESOLVE datastore + placement (pool, folder, host) ---
	datastore, err := p.resolveImportDatastore(ctx, req.StorageHint)
	if err != nil {
		return nil, errors.NewInternal("failed to resolve target datastore", err)
	}
	dsName := datastore.Name()

	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, errors.NewInternal("failed to get datacenter", err)
	}
	p.finder.SetDatacenter(datacenter)

	pool, folder, host, err := p.resolveImportPlacement(ctx, datacenter)
	if err != nil {
		return nil, errors.NewInternal("failed to resolve import placement", err)
	}

	finalPath := fmt.Sprintf("[%s] %s/%s.vmdk", dsName, id, id)
	folderPath := fmt.Sprintf("[%s] %s", dsName, id)

	// From here on, on ANY failure best-effort delete the "[<ds>] <id>" folder so a
	// failed import leaves no orphan datastore files (partial vmdk, descriptor).
	importSucceeded := false
	dsFileManager := NewDatastoreFileManager(p)
	defer func() {
		if importSucceeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if err := dsFileManager.DeleteFile(cleanupCtx, folderPath); err != nil {
			p.logger.Warn("Failed to delete partial import folder after failure (manual cleanup may be needed)", "path", folderPath, "error", err)
		}
	}()

	// --- 3. IMPORT via the SHARED NFC lease path (identical to S3; see s3import.go) ---
	p.logger.Info("Importing streamOptimized vmdk via NFC lease", "dest", finalPath, "datastore", dsName)
	if err := p.nfcImportStreamOptimized(ctx, vmdkLocal, id, datastore, datacenter, pool, folder, host); err != nil {
		return nil, errors.NewInternal("failed to import vmdk via NFC lease", err)
	}

	// --- 4. VERIFY: a real, queryable disk uuid proves a sound import ---
	vdm := object.NewVirtualDiskManager(p.client.Client)
	uuid, err := vdm.QueryVirtualDiskUuid(ctx, finalPath, datacenter)
	if err != nil {
		return nil, errors.NewInternal("imported disk failed uuid query (likely corrupt import)", err)
	}
	if uuid == "" {
		return nil, errors.NewInternal("imported disk has empty uuid (likely corrupt import)", nil)
	}

	importSucceeded = true
	p.logger.Info("Disk import from NFS completed", "id", id, "path", finalPath)

	return &providerv1.ImportDiskResponse{
		DiskId: id,
		Path:   finalPath,
		Task:   nil, // synchronous
		// No ActualSizeBytes/Checksum: qemu-img emits no in-stream byte SHA256 over
		// the libnfs read; integrity is the post-import uuid query (ADR-0006 Slice 4).
	}, nil
}
