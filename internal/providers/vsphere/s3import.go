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
	"github.com/vmware/govmomi/vmdk"

	"github.com/projectbeskar/virtrigaud/internal/diskutil"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// s3ImportSubformat is the qemu-img vmdk subformat the S3 import path MUST
// produce. The vCenter NFC HttpNfcLease import (vmdk.Import) only accepts
// streamOptimized — vmdk.Stat rejects every other subformat with
// ErrInvalidFormat. This was de-risked GREEN against lab vCenter 8.0.2; the
// earlier monolithicSparse + CopyVirtualDisk approach is rejected with "A
// specified parameter was not correct: fileType" (Bug J). See hack/slice2probe.
const s3ImportSubformat = "streamOptimized"

// importDiskFromS3 implements the ADR-0006 Slice 2 vSphere TARGET path: download
// the staged qcow2 object from S3, convert it to a streamOptimized vmdk in the
// pod, then import it onto a datastore as a native, attachable thin disk via the
// vCenter NFC HttpNfcLease transport (github.com/vmware/govmomi/vmdk.Import — the
// same path govc's "import.vmdk" uses). It is the reverse of the libvirt TARGET
// path (Slice 1) and the counterpart of the libvirt SOURCE export (s3export.go):
// libvirt exports its native qcow2 and vSphere — the TARGET — owns the
// qcow2→vmdk conversion (ADR D4).
//
// MECHANISM (de-risked GREEN in-cluster against lab vCenter 8.0.2 — see
// hack/slice2probe). The earlier "monolithicSparse + VirtualDiskManager.
// CopyVirtualDisk" approach was found NOT to work on this vCenter: a hosted
// (Workstation-format) vmdk uploaded as a raw datastore-HTTP file is rejected by
// CopyVirtualDisk with "A specified parameter was not correct: fileType" for
// EVERY combination of subformat (monolithicSparse / monolithicFlat /
// streamOptimized), destination VirtualDiskSpec vs FileBackedVirtualDiskSpec,
// DiskType (thin/preallocated/eagerZeroedThick/thick) and AdapterType
// (lsiLogic/busLogic/ide). CopyVirtualDisk cannot ingest a foreign hosted vmdk
// as a copy SOURCE. The supported path is the NFC import lease, which requires a
// streamOptimized vmdk:
//
//  1. Download s3://… → /tmp/<id>.qcow2 (SHA256 verified vs ExpectedChecksum).
//  2. qemu-img convert -O vmdk -o subformat=streamOptimized → /tmp/<id>.vmdk.
//     streamOptimized is MANDATORY for the NFC lease (vmdk.Stat rejects any other
//     subformat with ErrInvalidFormat).
//  3. Resolve datastore + resource pool + folder + host placement.
//  4. vmdk.Import: builds an OVF descriptor for the single disk, gets an
//     HttpNfcLease via ImportVApp, uploads the streamOptimized stream over the
//     lease, then detaches the disk and destroys the transient import VM —
//     leaving a native thin VMDK at "[<ds>] <id>/<id>.vmdk".
//  5. QueryVirtualDiskUuid(final) — an error or empty uuid means the import is
//     corrupt; fail loudly.
//  6. On any failure after the lease has touched the datastore, best-effort
//     delete the "[<ds>] <id>" folder so a failed import leaves no orphans.
//
// The disk lands transiently in /tmp in the pod (qcow2 + vmdk during convert);
// both temps are removed unconditionally. True streaming is an ADR-0006
// follow-up. Crash-resume of an interrupted transfer is OUT of scope: a failure
// retries the whole import.
func (p *Provider) importDiskFromS3(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("vSphere client not configured", nil)
	}

	// <id> is the stable name used for the temp files, the import VM/disk name,
	// the final datastore path, and the returned DiskId. It comes from
	// req.TargetName. vmdk.Import derives the on-datastore disk name from the local
	// vmdk basename, so the local file MUST be named "<id>.vmdk".
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
	p.logger.Info("Importing disk from S3 (streamOptimized + NFC lease vmdk.Import)",
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
	// The local vmdk MUST be named "<id>.vmdk": vmdk.Import uses the basename
	// (minus .vmdk) as the import entity / on-datastore disk name.
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

	// --- 2. CONVERT (ADR D4) — qcow2 → streamOptimized vmdk ---
	// streamOptimized is MANDATORY for the NFC lease import path: vmdk.Stat checks
	// the compressed-sparse flag and rejects any other subformat with
	// ErrInvalidFormat. Pass it EXPLICITLY via Subformat — do not rely on the
	// qemu-img default (monolithicSparse), which the NFC reader cannot ingest.
	qemuImg := diskutil.NewQemuImg()
	if !qemuImg.IsInstalled() {
		return nil, errors.NewInternal("qemu-img is not available in the provider image; cannot convert s3 import", nil)
	}
	if err := qemuImg.Convert(ctx, diskutil.ConvertOptions{
		SourcePath:        srcLocal,
		DestinationPath:   vmdkLocal,
		SourceFormat:      diskutil.SupportedFormat(srcFormat),
		DestinationFormat: diskutil.FormatVMDK,
		Subformat:         s3ImportSubformat, // MANDATORY for the NFC lease import path
	}); err != nil {
		return nil, errors.NewInternal("failed to convert s3 import to streamOptimized vmdk", err)
	}
	p.logger.Info("Converted s3 import to streamOptimized vmdk", "vmdk", vmdkLocal)

	// Fail fast with a clear error if the converted file is not the streamOptimized
	// format the NFC lease requires (defends against a future qemu-img default
	// change or a Subformat regression).
	if _, statErr := vmdk.Stat(vmdkLocal); statErr != nil {
		return nil, errors.NewInternal("converted vmdk is not a valid streamOptimized image for NFC import", statErr)
	}

	// --- 3. RESOLVE datastore + placement (pool, folder, host) ---
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

	// --- 4. IMPORT via NFC lease: streamOptimized vmdk → native thin disk ---
	// vmdk.Import builds a single-disk OVF, acquires an HttpNfcLease via
	// ImportVApp, uploads the streamOptimized stream, then detaches the disk and
	// destroys the transient import VM — leaving "[<ds>] <id>/<id>.vmdk".
	p.logger.Info("Importing streamOptimized vmdk via NFC lease", "dest", finalPath, "datastore", dsName)
	importErr := vmdk.Import(ctx, p.client.Client, vmdkLocal, datastore, vmdk.ImportParams{
		Path:       id, // subdir => lands at [<ds>] <id>/<id>.vmdk
		Type:       types.VirtualDiskTypeThin,
		Force:      true, // overwrite a stale leftover from a previous failed attempt
		Datacenter: datacenter,
		Pool:       pool,
		Folder:     folder,
		Host:       host,
	})
	if importErr != nil {
		return nil, errors.NewInternal("failed to import vmdk via NFC lease", importErr)
	}

	// --- 5. VERIFY: a real, queryable disk uuid proves a sound import ---
	vdm := object.NewVirtualDiskManager(p.client.Client)
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

// resolveImportPlacement resolves the resource pool, VM folder, and host used by
// vmdk.Import's ImportVApp/HttpNfcLease. It mirrors the Create path's placement
// resolution: the resource pool comes from the provider's DefaultCluster (the
// import VM is transient — it is destroyed immediately after the disk lands — so
// any valid pool works), the folder from DefaultFolder (falling back to the
// datacenter's default "vm" folder), and an explicit host is required by some
// vCenter configurations for the NFC import to succeed.
func (p *Provider) resolveImportPlacement(ctx context.Context, datacenter *object.Datacenter) (*object.ResourcePool, *object.Folder, *object.HostSystem, error) {
	// Resource pool from the default cluster.
	cluster, err := p.finder.ClusterComputeResource(ctx, p.config.DefaultCluster)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("find cluster %q: %w", p.config.DefaultCluster, err)
	}
	pool, err := cluster.ResourcePool(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get resource pool from cluster %q: %w", p.config.DefaultCluster, err)
	}

	// Folder: provider default, falling back to the datacenter's default VM folder.
	folder, err := p.finder.Folder(ctx, p.config.DefaultFolder)
	if err != nil {
		folder, err = p.finder.Folder(ctx, datacenter.Name()+"/vm")
		if err != nil {
			return nil, nil, nil, fmt.Errorf("find VM folder (default %q and datacenter default): %w", p.config.DefaultFolder, err)
		}
	}

	// Host: pick the first host of the cluster. ImportVApp on some vCenter
	// configurations returns a 500 without an explicit host placement.
	var host *object.HostSystem
	if hosts, herr := cluster.Hosts(ctx); herr == nil && len(hosts) > 0 {
		host = hosts[0]
	}

	return pool, folder, host, nil
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
