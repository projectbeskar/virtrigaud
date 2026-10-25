/*
Copyright 2026.

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

import "github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"

// pveConfigKeyBoot is the VM config key of the boot options
// ("order=<device>[;<device>...]").
const pveConfigKeyBoot = "boot"

// cloneBootOrder returns the boot config value a VM cloned from a template is
// given: "order=<primary disk>" (pveapi.PrimaryDiskFromConfig of cfg, the
// clone's config), or "" when cfg shows no disk, in which case the caller sets
// no boot order and the clone keeps the template's.
//
// The cloud-init drive is not in the order. It is a CD-ROM that cloud-init in
// the guest reads as a data source; nothing boots from it. This is PVE's own
// convention (its Cloud-Init guide sets `--boot order=scsi0` after adding the
// cloud-init drive "to restrict BIOS to boot from this disk only"), and what
// the migration import path does (buildImportedDiskSetCommand: `--boot
// order=scsi0`, the drive attached afterwards). The value never names a
// device the config lacks, nor has an empty element: PVE refuses a config
// update whose boot order names a device the VM does not have ("invalid
// bootorder: device '<dev>' does not exist").
func cloneBootOrder(cfg map[string]interface{}) string {
	disk, value := pveapi.PrimaryDiskFromConfig(cfg)
	if value == "" {
		return ""
	}
	return "order=" + disk
}
