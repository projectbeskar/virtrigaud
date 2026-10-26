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

package pvefake

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// How qemu-server changes a VM's boot order by itself (PVE::API2::Qemu,
// PVE::QemuServer), modelled so a test sees the boot order PVE would leave:
//
//   - create_vm without a `boot` parameter writes the default boot devices
//     (get_default_bootdevices): the first disk that is not a CD-ROM, the
//     first CD-ROM (a cloud-init drive counts), the first NIC, scanning ide,
//     scsi, virtio, sata in index order.
//   - update_vm, for a drive it sets that is a CD-ROM and not a cloud-init
//     drive, appends the drive's slot to the current order list when the list
//     is not empty and does not name it yet ("append new CD drives to
//     bootorder to mark them bootable"). `qm set` is that same update.
//   - update_vm removes a drive it deletes from the current order list.
//
// update_vm works on the order list of the config as it was before the
// update and iterates the update's parameters in Perl hash order, so an
// update that both sets `boot` and attaches a CD-ROM leaves either order
// depending on that iteration. The fake refuses such an update instead, so a
// test catches a provider that relies on it.

// bootDriveScan is qemu-server's valid_drive_names_for_boot order.
var bootDriveScan = func() []string {
	var out []string
	for _, bus := range []struct {
		name string
		n    int
	}{{"ide", 4}, {"scsi", 31}, {"virtio", 16}, {"sata", 6}} {
		for i := 0; i < bus.n; i++ {
			out = append(out, bus.name+strconv.Itoa(i))
		}
	}
	return out
}()

// mediaCDROM is the drive option of a CD-ROM.
const mediaCDROM = "media=cdrom"

// maxNets is qemu-server's $MAX_NETS.
const maxNets = 32

// isCDROMValue reports whether a drive value is a CD-ROM (media=cdrom).
func isCDROMValue(value string) bool {
	_, opts, _ := strings.Cut(value, ",")
	for _, o := range strings.Split(opts, ",") {
		if strings.TrimSpace(o) == mediaCDROM {
			return true
		}
	}
	return false
}

// isCloudInitValue reports whether a drive value is a PVE cloud-init drive:
// "<storage>:cloudinit" as asked for, or the vm-<vmid>-cloudinit volume.
func isCloudInitValue(value string) bool {
	head, _, _ := strings.Cut(value, ",")
	_, volume, _ := strings.Cut(head, ":")
	return volume == "cloudinit" || strings.Contains(volume, "-cloudinit")
}

// defaultBootOrder is get_default_bootdevices of config, printed as a boot
// value ("" when there is no device).
func defaultBootOrder(config map[string]string) string {
	var devs []string
	for _, cdrom := range []bool{false, true} {
		for _, key := range bootDriveScan {
			value := config[key]
			if value == "" || isCDROMValue(value) != cdrom {
				continue
			}
			devs = append(devs, key)
			break
		}
	}
	for i := 0; i < maxNets; i++ {
		if key := "net" + strconv.Itoa(i); config[key] != "" {
			devs = append(devs, key)
			break
		}
	}
	if len(devs) == 0 {
		return ""
	}
	return bootOrderPrefix + strings.Join(devs, ";")
}

// bootOrderList returns the devices of a boot value's order list.
func bootOrderList(boot string) []string {
	var out []string
	for _, part := range strings.Split(boot, ",") {
		order, ok := strings.CutPrefix(strings.TrimSpace(part), bootOrderPrefix)
		if !ok {
			continue
		}
		for _, dev := range strings.Split(order, ";") {
			if dev = strings.TrimSpace(dev); dev != "" {
				out = append(out, dev)
			}
		}
	}
	return out
}

// setBootOrderList writes devs as config's boot order (removing the key when
// devs is empty, as print_bootorder prints nothing).
func setBootOrderList(config map[string]string, devs []string) {
	if len(devs) == 0 {
		delete(config, configKeyBoot)
		return
	}
	config[configKeyBoot] = bootOrderPrefix + strings.Join(devs, ";")
}

// appendCDROMToBootOrder does what update_vm does for a CD-ROM drive (not a
// cloud-init drive) it sets in slot, given the order list before the update
// (before): when that list is not empty and does not name slot, slot is
// appended to it.
func appendCDROMToBootOrder(config map[string]string, before []string, slot, value string) []string {
	if len(before) == 0 || !isCDROMValue(value) || isCloudInitValue(value) {
		return before
	}
	for _, dev := range before {
		if dev == slot {
			return before
		}
	}
	after := append(append([]string(nil), before...), slot)
	setBootOrderList(config, after)
	return after
}

// removeFromBootOrder does what update_vm does for a drive it deletes: the
// drive leaves the order list.
func removeFromBootOrder(config map[string]string, key string) {
	devs := bootOrderList(config[configKeyBoot])
	kept := devs[:0]
	removed := false
	for _, dev := range devs {
		if dev == key {
			removed = true
			continue
		}
		kept = append(kept, dev)
	}
	if removed {
		setBootOrderList(config, kept)
	}
}

// newCDROMs returns, sorted, the drive keys of a config update form that set a
// CD-ROM other than a cloud-init drive.
func newCDROMs(form url.Values) []string {
	var out []string
	for key := range form {
		if driveKeyPattern.MatchString(key) && isCDROMValue(form.Get(key)) && !isCloudInitValue(form.Get(key)) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// checkBootWithCDROM refuses a config update that sets the boot order and
// attaches a CD-ROM together (see the comment at the top of this file).
func checkBootWithCDROM(form url.Values) error {
	if _, setBoot := form[configKeyBoot]; !setBoot {
		return nil
	}
	if cds := newCDROMs(form); len(cds) > 0 {
		return fmt.Errorf("pvefake: an update that sets the boot order and attaches CD-ROM %s has no defined boot order in PVE", strings.Join(cds, ","))
	}
	return nil
}
