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

import "github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"

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
		// ExportCompression is intentionally NOT advertised — the export path
		// does not compress today.
		DiskExport("qcow2", "raw", "vmdk").
		DiskImport("qcow2", "raw", "vmdk").
		DiskTypes("raw", "qcow2").
		NetworkTypes("bridge", "vlan").
		Build()
}
