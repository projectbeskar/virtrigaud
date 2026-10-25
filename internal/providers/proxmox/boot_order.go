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

import (
	"strings"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
)

const (
	// pveConfigKeyBoot is the VM config key of the boot options
	// ("order=<device>[;<device>...]", or a legacy "cdn"-style value).
	pveConfigKeyBoot = "boot"

	// pveConfigKeyBootDisk is the legacy VM config key naming the boot disk,
	// which PVE still reads when "boot" has no order list.
	pveConfigKeyBootDisk = "bootdisk"

	// pveBootOrderPrefix starts the order list of a boot value.
	pveBootOrderPrefix = "order="

	// pveMediaCDROM is the drive option of a CD-ROM (the cloud-init drive
	// included).
	pveMediaCDROM = "media=cdrom"
)

// cloneBootOrder returns the boot config value a VM cloned from a template is
// given: "order=<boot disk>" (cloneBootDisk of cfg, the clone's config), or ""
// when cfg shows no disk, in which case the caller sets no boot order and the
// clone keeps the template's.
//
// The cloud-init drive is not in the order. It is a CD-ROM that cloud-init in
// the guest finds by its filesystem label and reads as a data source; nothing
// boots from it. This is PVE's own convention (its Cloud-Init guide sets
// `--boot order=scsi0` after adding the cloud-init drive "to restrict BIOS to
// boot from this disk only"), and what the migration import path does
// (buildImportedDiskSetCommand: `--boot order=scsi0`; the drive is attached
// by a later config update). The value never names a device the config
// lacks, nor has an empty element: PVE refuses a config update whose boot
// order names a device the VM does not have ("invalid bootorder: device
// '<dev>' does not exist").
func cloneBootOrder(cfg map[string]interface{}) string {
	disk := cloneBootDisk(cfg)
	if disk == "" {
		return ""
	}
	return pveBootOrderPrefix + disk
}

// cloneBootDisk returns the config key of the disk a clone boots from: the
// first attached disk (isAttachedDisk) in the boot order the clone inherited
// from its template, else the legacy bootdisk when it is one, else
// pveapi.PrimaryDiskFromConfig (the first disk scanning virtio, scsi, sata,
// ide), or "" when cfg has no disk. Following the template's own order first
// keeps a template whose boot disk is not the first disk of that scan (a data
// disk on virtio0, the boot disk on scsi0) booting from its boot disk.
func cloneBootDisk(cfg map[string]interface{}) string {
	if boot, ok := cfg[pveConfigKeyBoot].(string); ok {
		for _, option := range strings.Split(boot, ",") {
			order, isOrder := strings.CutPrefix(strings.TrimSpace(option), pveBootOrderPrefix)
			if !isOrder {
				continue
			}
			for _, dev := range strings.Split(order, ";") {
				if dev = strings.TrimSpace(dev); isAttachedDisk(cfg, dev) {
					return dev
				}
			}
		}
	}
	if legacy, ok := cfg[pveConfigKeyBootDisk].(string); ok && isAttachedDisk(cfg, strings.TrimSpace(legacy)) {
		return strings.TrimSpace(legacy)
	}
	disk, value := pveapi.PrimaryDiskFromConfig(cfg)
	if value == "" {
		return ""
	}
	return disk
}

// isAttachedDisk reports whether key is a bus disk (pveBusDiskKey) that cfg
// has, with a value that is not a CD-ROM.
func isAttachedDisk(cfg map[string]interface{}, key string) bool {
	if !pveBusDiskKey.MatchString(key) {
		return false
	}
	value, ok := cfg[key].(string)
	return ok && value != "" && !strings.Contains(strings.ToLower(value), pveMediaCDROM)
}
