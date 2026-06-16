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
	"path/filepath"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/ovf"
	"github.com/vmware/govmomi/vim25/soap"
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
	// We drive the HttpNfcLease manually rather than calling vmdk.Import for two
	// reasons specific to this remote-provider topology:
	//   (a) the lease's device URL points at the ESXi host by FQDN (e.g.
	//       esxi.lab.k8); the provider pod often cannot resolve that name via
	//       cluster DNS, so we rewrite the host to the vCenter host, which proxies
	//       NFC and is always reachable from the pod (it is PROVIDER_ENDPOINT);
	//   (b) we Unregister (not Destroy) the transient import VM — vCenter 8 faults
	//       Destroy of a freshly imported VM ("file ... is attached to vm").
	// Net result is identical to vmdk.Import: "[<ds>] <id>/<id>.vmdk".
	p.logger.Info("Importing streamOptimized vmdk via NFC lease", "dest", finalPath, "datastore", dsName)
	if err := p.nfcImportStreamOptimized(ctx, vmdkLocal, id, datastore, datacenter, pool, folder, host); err != nil {
		return nil, errors.NewInternal("failed to import vmdk via NFC lease", err)
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

// nfcImportStreamOptimized imports a local streamOptimized vmdk into the target
// datastore as a native thin disk, driving the HttpNfcLease by hand so it works
// from a remote provider pod. It mirrors govmomi's vmdk.Import but with two
// topology-specific differences (see the call site): it rewrites each lease
// device URL host to the vCenter host (the ESXi FQDN the lease returns is not
// resolvable from the pod, and vCenter proxies NFC), and it Unregisters — rather
// than Destroys — the transient import VM (vCenter 8 faults Destroy of a
// just-imported VM). On success the disk lands at "[<ds>] <id>/<id>.vmdk".
func (p *Provider) nfcImportStreamOptimized(
	ctx context.Context,
	localVMDK, id string,
	datastore *object.Datastore,
	datacenter *object.Datacenter,
	pool *object.ResourcePool,
	folder *object.Folder,
	host *object.HostSystem,
) error {
	// Inspect the streamOptimized vmdk (capacity, on-disk size, geometry) and
	// derive the single-disk OVF descriptor govmomi uses for the import spec.
	disk, err := vmdk.Stat(localVMDK)
	if err != nil {
		return fmt.Errorf("stat streamOptimized vmdk %q: %w", localVMDK, err)
	}
	disk.ImportName = id // OVF EntityName => folder + disk basename become "<id>"
	descriptor, err := disk.OVF()
	if err != nil {
		return fmt.Errorf("build single-disk OVF descriptor: %w", err)
	}

	// Best-effort delete the entire leftover import folder "[<ds>] <id>" (Force
	// semantics) before importing. The import id is deterministic per migration
	// (the controller uses "<target>-migrated"), so a retry after a prior attempt
	// — or after a deleted target VM that left its disk folder behind — would
	// otherwise find the folder occupied; ImportVApp then places the disk in a
	// collision-suffixed folder ("<id>_1"), which defeats the post-import uuid
	// query on the expected "[<ds>] <id>/<id>.vmdk" path. Deleting just the target
	// .vmdk is not enough when the folder still holds other leftovers (a prior
	// .vmx/.nvram). fm.Delete on a missing path returns an error we intentionally
	// ignore.
	fm := datastore.NewFileManager(datacenter, true)
	_ = fm.Delete(ctx, id)

	ovfManager := ovf.NewManager(p.client.Client)
	spec, err := ovfManager.CreateImportSpec(ctx, descriptor, pool, datastore, &types.OvfCreateImportSpecParams{
		DiskProvisioning: string(types.OvfCreateImportSpecParamsDiskProvisioningTypeThin),
		EntityName:       id,
	})
	if err != nil {
		return fmt.Errorf("create import spec: %w", err)
	}
	if spec.Error != nil {
		return fmt.Errorf("import spec rejected: %s", spec.Error[0].LocalizedMessage)
	}

	lease, err := pool.ImportVApp(ctx, spec.ImportSpec, folder, host)
	if err != nil {
		return fmt.Errorf("ImportVApp (acquire NFC lease): %w", err)
	}
	info, err := lease.Wait(ctx, spec.FileItem)
	if err != nil {
		return fmt.Errorf("wait for NFC lease ready: %w", err)
	}

	// (a) Rewrite the lease device URLs: the ESXi FQDN they carry is unresolvable
	// from the provider pod; the vCenter host proxies NFC and is always reachable.
	vcHost := p.client.Client.URL().Host
	for i := range info.Items {
		info.Items[i].URL.Host = vcHost
	}

	f, err := os.Open(filepath.Clean(localVMDK))
	if err != nil {
		_ = lease.Abort(ctx, nil)
		return fmt.Errorf("open local vmdk for upload: %w", err)
	}
	uploadErr := func() error {
		defer func() { _ = f.Close() }()
		updater := lease.StartUpdater(ctx, info)
		defer updater.Done()
		if len(info.Items) == 0 {
			return fmt.Errorf("NFC lease returned no upload items")
		}
		if err := lease.Upload(ctx, info.Items[0], f, soap.Upload{ContentLength: disk.Size}); err != nil {
			return fmt.Errorf("NFC upload: %w", err)
		}
		return nil
	}()
	if uploadErr != nil {
		_ = lease.Abort(ctx, nil)
		return uploadErr
	}
	if err := lease.Complete(ctx); err != nil {
		return fmt.Errorf("complete NFC lease: %w", err)
	}

	// (b) Detach the disk (keep the file) and Unregister the transient import VM.
	// Unregister leaves the disk — and a small leftover .vmx/.nvram in the folder,
	// which is harmless: the subsequent Create attaches "[<ds>] <id>/<id>.vmdk".
	vm := object.NewVirtualMachine(p.client.Client, info.Entity)
	if devs, derr := vm.Device(ctx); derr == nil {
		if disks := devs.SelectByType((*types.VirtualDisk)(nil)); len(disks) > 0 {
			_ = vm.RemoveDevice(ctx, true, disks...)
		}
	}
	if err := vm.Unregister(ctx); err != nil {
		p.logger.Warn("Failed to unregister transient import VM (leftover .vmx may remain in folder)", "vm", info.Entity.Value, "error", err)
	}
	return nil
}

// resolveImportPlacement resolves the resource pool, VM folder, and host used by
// the NFC import's ImportVApp/HttpNfcLease. It mirrors the Create path's placement
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
