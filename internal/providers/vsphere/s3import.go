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
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/projectbeskar/virtrigaud/internal/diskutil"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// importDiskFromS3 implements the ADR-0006 Slice 2 vSphere TARGET path: download
// the staged qcow2 object from S3, convert it to a monolithicSparse vmdk in the
// pod, upload that vmdk to a datastore via the vCenter datastore-HTTP endpoint,
// then ask VirtualDiskManager.CopyVirtualDisk to inflate it into a native,
// attachable disk. It is the reverse of the libvirt TARGET path (Slice 1) and
// the counterpart of the libvirt SOURCE export (s3export.go): libvirt exports
// its native qcow2 and vSphere — the TARGET — owns the qcow2→vmdk conversion
// (ADR D4).
//
// APPROACH 2 — why this exact sequence. This was de-risked GREEN in-cluster
// against lab vCenter 8.0.2. The critical constraint:
//
//	streamOptimized is REJECTED by this ESXi at BOTH the NFC and the
//	VirtualDiskManager layers, so it must NEVER be used for import.
//
// monolithicSparse, by contrast, uploads cleanly over the datastore-HTTP path
// and CopyVirtualDisk inflates it to a thin native disk. Therefore:
//
//  1. Download s3://… → /tmp/<id>.qcow2 (SHA256 verified vs ExpectedChecksum).
//  2. qemu-img convert -O vmdk -o subformat=monolithicSparse → /tmp/<id>.vmdk.
//  3. Upload /tmp/<id>.vmdk to a STAGING datastore path via the vCenter
//     datastore-HTTP endpoint (DatastoreFileManager.UploadFile — NEVER a
//     direct-ESXi URL).
//  4. CopyVirtualDisk(staged → final) inflates the staged monolithicSparse vmdk
//     into a native thin disk.
//  5. QueryVirtualDiskUuid(final) — an error or empty uuid means the import is
//     corrupt; fail loudly.
//  6. Delete the staging vmdk; clean up /tmp/<id>.*. On any post-upload failure,
//     best-effort delete both staging and final so a failure leaves no orphans.
//
// The disk lands transiently in /tmp in the pod (qcow2 + vmdk during convert);
// both temps are removed unconditionally. True streaming is an ADR-0006
// follow-up. Crash-resume of an interrupted transfer is OUT of scope: a failure
// retries the whole import.
func (p *Provider) importDiskFromS3(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("vSphere client not configured", nil)
	}

	// <id> is the stable name used for the temp files, the staging/final datastore
	// paths, and the returned DiskId. It comes from req.TargetName.
	id := req.TargetName
	if id == "" {
		id = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}

	// Source format: the reverse-relay staged object is qcow2 (libvirt's native
	// export). Default to qcow2 when req.Format is empty; honor an explicit
	// override (e.g. raw) for completeness.
	srcFormat := req.Format
	if srcFormat == "" {
		srcFormat = "qcow2"
	}
	if srcFormat != "qcow2" && srcFormat != "raw" && srcFormat != "vmdk" {
		return nil, errors.NewInvalidSpec("unsupported s3 import source format: %s", srcFormat)
	}

	// Never log the credentials map (secret material); log only the backend.
	p.logger.Info("Importing disk from S3 (Approach 2: monolithicSparse + CopyVirtualDisk)",
		"id", id, "source", req.SourceUrl, "src_format", srcFormat, "backend", req.BackendType, "storage_hint", req.StorageHint)

	// --- 1. DOWNLOAD (ADR D5) ---
	// Build the S3 client (the pod is the S3 client). Options come from
	// storage_options_json; credentials from the credentials map. Never logged.
	storageConfig, err := migration.S3StorageConfigFromRequest(req.StorageOptionsJson, req.Credentials)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid s3 import configuration: %s", err.Error())
	}
	s3client, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create s3 client", err)
	}
	defer s3client.Close()

	srcLocal := fmt.Sprintf("/tmp/%s.%s", id, srcFormat)
	vmdkLocal := fmt.Sprintf("/tmp/%s.vmdk", id)
	defer func() {
		_ = os.Remove(srcLocal)
		_ = os.Remove(vmdkLocal)
	}()

	srcFile, err := os.Create(srcLocal)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp download file", err)
	}
	dl, dlErr := s3client.DownloadStream(ctx, storage.StreamDownloadRequest{
		SourceURL:        req.SourceUrl,
		Writer:           srcFile,
		ExpectedChecksum: req.ExpectedChecksum, // SHA256 verified in-stream when set
	})
	closeErr := srcFile.Close()
	if dlErr != nil {
		return nil, errors.NewInternal("failed to download disk from s3", dlErr)
	}
	if closeErr != nil {
		return nil, errors.NewInternal("failed to flush downloaded disk", closeErr)
	}
	p.logger.Info("S3 object downloaded to pod",
		"bytes", dl.BytesTransferred, "sha256-verified", req.ExpectedChecksum != "")

	// --- 2. CONVERT (ADR D4) — qcow2 → monolithicSparse vmdk ---
	// monolithicSparse is MANDATORY (streamOptimized is rejected by ESXi at both
	// the NFC and VirtualDiskManager layers). Pass it EXPLICITLY via the new
	// Subformat option — do not rely on the qemu-img default subformat.
	qemuImg := diskutil.NewQemuImg()
	if !qemuImg.IsInstalled() {
		return nil, errors.NewInternal("qemu-img is not available in the provider image; cannot convert s3 import", nil)
	}
	if err := qemuImg.Convert(ctx, diskutil.ConvertOptions{
		SourcePath:        srcLocal,
		DestinationPath:   vmdkLocal,
		SourceFormat:      diskutil.SupportedFormat(srcFormat),
		DestinationFormat: diskutil.FormatVMDK,
		Subformat:         "monolithicSparse", // MANDATORY for this ESXi import path
	}); err != nil {
		return nil, errors.NewInternal("failed to convert s3 import to monolithicSparse vmdk", err)
	}
	p.logger.Info("Converted s3 import to monolithicSparse vmdk", "vmdk", vmdkLocal)

	// --- 3. RESOLVE datastore + datacenter ---
	datastore, err := p.resolveImportDatastore(ctx, req.StorageHint)
	if err != nil {
		return nil, errors.NewInternal("failed to resolve target datastore", err)
	}
	dsName := datastore.Name()

	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, errors.NewInternal("failed to get datacenter", err)
	}

	stagedPath := fmt.Sprintf("[%s] %s/%s-staged.vmdk", dsName, id, id)
	finalPath := fmt.Sprintf("[%s] %s/%s.vmdk", dsName, id, id)

	// --- 4. UPLOAD staged vmdk via vCenter datastore-HTTP ---
	// DatastoreFileManager.UploadFile uses the vCenter datastore stream endpoint
	// (datastore.Upload), NEVER a direct-ESXi URL — the de-risked transport.
	dsManager := NewDatastoreFileManager(p)
	uploadFile, err := os.Open(vmdkLocal)
	if err != nil {
		return nil, errors.NewInternal("failed to open vmdk for datastore upload", err)
	}
	stat, err := uploadFile.Stat()
	if err != nil {
		_ = uploadFile.Close()
		return nil, errors.NewInternal("failed to stat vmdk", err)
	}
	uploadErr := dsManager.UploadFile(ctx, uploadFile, stagedPath, stat.Size(), nil)
	_ = uploadFile.Close()
	if uploadErr != nil {
		return nil, errors.NewInternal("failed to upload staged vmdk to datastore", uploadErr)
	}
	p.logger.Info("Uploaded staged monolithicSparse vmdk to datastore (datastore-HTTP)", "staged", stagedPath)

	// From here on, on ANY failure best-effort delete the staging AND final vmdk
	// so a failed import leaves no orphan datastore files.
	importSucceeded := false
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// Always remove the staging vmdk (it is intermediate even on success).
		if err := dsManager.DeleteFile(cleanupCtx, stagedPath); err != nil {
			p.logger.Warn("Failed to delete staging vmdk (manual cleanup may be needed)", "path", stagedPath, "error", err)
		}
		// Remove the final vmdk only if the import did NOT complete cleanly.
		if !importSucceeded {
			if err := dsManager.DeleteFile(cleanupCtx, finalPath); err != nil {
				p.logger.Warn("Failed to delete partial final vmdk after import failure", "path", finalPath, "error", err)
			}
		}
	}()

	// --- 5. INFLATE: CopyVirtualDisk(staged → final) to a native thin disk ---
	vdm := object.NewVirtualDiskManager(p.client.Client)
	spec := &types.VirtualDiskSpec{
		DiskType:    string(types.VirtualDiskTypeThin),
		AdapterType: string(types.VirtualDiskAdapterTypeLsiLogic),
	}
	p.logger.Info("Inflating staged vmdk to native disk via CopyVirtualDisk", "source", stagedPath, "dest", finalPath)
	task, err := vdm.CopyVirtualDisk(ctx, stagedPath, datacenter, finalPath, datacenter, spec, false)
	if err != nil {
		return nil, errors.NewInternal("failed to start CopyVirtualDisk (staged → final)", err)
	}
	if err := task.Wait(ctx); err != nil {
		return nil, errors.NewInternal("CopyVirtualDisk (staged → final) failed", err)
	}

	// --- 6. VERIFY: a real, queryable disk uuid proves a sound import ---
	uuid, err := vdm.QueryVirtualDiskUuid(ctx, finalPath, datacenter)
	if err != nil {
		return nil, errors.NewInternal("imported disk failed uuid query (likely corrupt import)", err)
	}
	if uuid == "" {
		return nil, errors.NewInternal("imported disk has empty uuid (likely corrupt import)", nil)
	}

	importSucceeded = true
	p.logger.Info("Disk import from S3 completed", "id", id, "path", finalPath, "bytes", dl.BytesTransferred)

	return &providerv1.ImportDiskResponse{
		DiskId:          id,
		Path:            finalPath,
		Task:            nil, // synchronous
		ActualSizeBytes: dl.BytesTransferred,
		Checksum:        dl.Checksum, // SHA256 of the transferred (pre-conversion) qcow2
	}, nil
}

// resolveImportDatastore resolves the target datastore for an S3 import. It
// reuses the same resolution shape as Create/ImportDisk: storageHint (or the
// provider's DefaultDatastore) is tried as a datastore name first, and — when
// that name actually identifies a Datastore Cluster (StoragePod) — falls back to
// resolveDatastoreFromStoragePod to pick the member with the most free space
// (lightweight SDRS). The returned object.Datastore has its Name() populated for
// building the "[<ds>] …" paths.
func (p *Provider) resolveImportDatastore(ctx context.Context, storageHint string) (*object.Datastore, error) {
	name := storageHint
	if name == "" {
		name = p.config.DefaultDatastore
	}
	if name == "" {
		return nil, fmt.Errorf("no datastore: set req.StorageHint or the provider's DefaultDatastore")
	}

	// Try as a plain datastore name first (the common case).
	if ds, err := p.finder.Datastore(ctx, name); err == nil {
		return ds, nil
	}

	// Fall back to treating the hint as a StoragePod (Datastore Cluster) and pick
	// the member with the most free space (lightweight SDRS), mirroring Create.
	ds, err := p.resolveDatastoreFromStoragePod(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("datastore/StoragePod %q not found: %w", name, err)
	}
	return ds, nil
}
