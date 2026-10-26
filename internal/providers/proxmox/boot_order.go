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
	"strconv"
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
// (buildImportedDiskSetCommand: `--boot order=scsi0`). A seed attached after
// the order is set is appended to it by qemu-server and taken out again
// (seedFreeBootOrder, attachSeed). The value never names a device the config
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
// from its template, else — whenever that order names no such disk, or there
// is none — the legacy bootdisk when it is one, else
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

// seedFreeBootOrder returns the boot value cfg (a VM's config) gets so that
// its order list names none of the seed slots seeds, and whether that differs
// from the value cfg has. qemu-server appends a CD-ROM that a config update
// (`qm set`) attaches to a non-empty order list ("append new CD drives to
// bootorder to mark them bootable"), so attaching a seed puts its slot there;
// a slot already in the list (e.g. a template's install-ISO slot, now the
// seed's) stays there. The other devices the config has keep their order (one
// it lacks is left out: PVE refuses an order that names it). When nothing else
// would be left, the order names the VM's boot disk, else its first NIC
// (bootFallbackDevice); a VM with neither keeps its order. A config without an
// order list (no boot value, or a legacy one) is left as it is: qemu-server
// appends nothing to it.
func seedFreeBootOrder(cfg map[string]interface{}, seeds []string) (string, bool) {
	boot, _ := cfg[pveConfigKeyBoot].(string)
	if boot == "" || len(seeds) == 0 {
		return "", false
	}
	drop := make(map[string]bool, len(seeds))
	for _, slot := range seeds {
		drop[slot] = true
	}
	options := strings.Split(boot, ",")
	changed := false
	for i, option := range options {
		order, isOrder := strings.CutPrefix(strings.TrimSpace(option), pveBootOrderPrefix)
		if !isOrder {
			continue
		}
		var kept []string
		dropped := false
		for _, dev := range strings.Split(order, ";") {
			dev = strings.TrimSpace(dev)
			value, _ := cfg[dev].(string)
			switch {
			case drop[dev]:
				dropped = true
			case value == "":
				// Not in the config: PVE refuses an order that names it.
			default:
				kept = append(kept, dev)
			}
		}
		if !dropped {
			continue
		}
		if len(kept) == 0 {
			fallback := bootFallbackDevice(cfg)
			if fallback == "" {
				continue
			}
			kept = []string{fallback}
		}
		options[i] = pveBootOrderPrefix + strings.Join(kept, ";")
		changed = true
	}
	if !changed {
		return "", false
	}
	return strings.Join(options, ","), true
}

// bootFallbackDevice is the device a boot order that would otherwise be empty
// names: the VM's boot disk (cloneBootDisk), else its NIC with the lowest
// index, else "".
func bootFallbackDevice(cfg map[string]interface{}) string {
	if disk := cloneBootDisk(cfg); disk != "" {
		return disk
	}
	first := -1
	for key, v := range cfg {
		if s, ok := v.(string); !ok || s == "" {
			continue
		}
		if index, ok := netKeyIndex(key); ok && (first < 0 || index < first) {
			first = index
		}
	}
	if first < 0 {
		return ""
	}
	return pveNetKeyPrefix + strconv.Itoa(first)
}
