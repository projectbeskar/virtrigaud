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
		ImageImport().
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
		// ADR-0006 Slice 0: advertise the status quo honestly. ExportDisk/
		// ImportDisk only implement the pod-side (pvc, relay-shaped) staging
		// path; nfs/s3 and direct transfer are not yet implemented.
		ExportBackends(migration.PVCOnlyExportBackends()...).
		ImportBackends(migration.PVCOnlyImportBackends()...).
		TransferModes(migration.RelayOnlyTransferModes()...).
		DiskTypes("raw", "qcow2").
		NetworkTypes("bridge", "vlan").
		Build()
}
