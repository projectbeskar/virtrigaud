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
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
)

// GetProviderCapabilities returns the capabilities for the Proxmox provider
func GetProviderCapabilities() *capabilities.Manager {
	return capabilities.NewBuilder().
		Core().
		Snapshots().
		MemorySnapshots().
		LinkedClones().
		OnlineReconfigure().
		OnlineDiskExpansion().
		// ImageImport routes import-style VMImage sources (source.http) to
		// ImagePrepare, which in this release refuses them with InvalidSpec
		// (ADR-0009 D10): the Proxmox URL import never produced a usable
		// template. Advertising it lets that honest refusal reach the VMImage
		// (reason InvalidSource). Without it the manager would skip the
		// prepare and send the unprepared source straight to Create.
		ImageImport().
		// ImageArtifactIdentity: this provider never reuses a prepared-image
		// artifact by bare name (it prepares none until ADR-0009 Slice 6), and
		// refuses identity and legacy URL imports alike. Not advertising it would
		// make the manager hold such VMImages with a false "upgrade the provider
		// image" reason (ProviderLacksArtifactIdentity) (ADR-0009 D10).
		ImageArtifactIdentity().
		// Disk export/import RPCs are implemented (ExportDisk/ImportDisk/
		// GetDiskInfo accept qcow2/raw/vmdk); advertise them so capability
		// gating (#176) doesn't wrongly block Proxmox disk migration (#198).
		// ExportCompression is advertised: ExportDisk honors req.Compress by
		// forcing a qemu-img convert pass with `-c` for qcow2 targets (#219).
		// Compression is genuine only for qcow2; raw/vmdk exports remain
		// uncompressed even when Compress=true, and the default (Compress=false)
		// stays uncompressed for export speed.
		DiskExport("qcow2", "raw", "vmdk").
		ExportCompression().
		DiskImport("qcow2", "raw", "vmdk").
		// ADR-0006: two real data paths for Proxmox, both node-side. (1) The S3
		// relay — bytes flow node ↔ provider-pod ↔ S3 over SSH (the pod is the S3
		// client). (2) The NFS qemu-img path (Slice 4) — the node's qemu-img reads/
		// writes nfs:// directly over libnfs, no pod hop. The legacy pvc path
		// (storage_helper.go) does os.Open on node-local image paths, which a
		// remote provider pod can never reach, so pvc is NOT advertised (advertising
		// it lets a migration select a backend that always fails).
		ExportBackends(migration.S3AndNFSExportBackends()...).
		ImportBackends(migration.S3AndNFSImportBackends()...).
		// NFS runs node-side ("direct"-shaped), but the controller treats nfs's
		// transfer mode as informational and exempts it from the relay check, so
		// advertising relay (for the S3 path) remains accurate.
		TransferModes(migration.RelayOnlyTransferModes()...).
		DiskTypes("raw", "qcow2").
		NetworkTypes("bridge", "vlan").
		Build()
}
