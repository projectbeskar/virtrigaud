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
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tracked storage volumes: the cloud-init seed volumes the Proxmox provider's
// seed script imports (`qm importdisk`) and attaches as CD-ROMs. The fake
// frees them the way qemu-server's destroy does: a volume the VM owns that
// its config references as a disk or an unused disk; never a CD-ROM's
// volume (destroy_vm skips CD-ROMs, except a cloud-init drive) unless the
// destroy passes destroy-unreferenced-disks=1, which frees every volume of
// the VMID. A config update that deletes an unusedN key frees its volume.

// driveKeyPattern matches the config keys of a VM's drives.
var driveKeyPattern = regexp.MustCompile(`^(?:ide|sata|scsi|virtio)\d+$`)

// unusedKeyPattern matches the config keys of a VM's unused disks.
var unusedKeyPattern = regexp.MustCompile(`^unused\d+$`)

// diskIndexPattern finds the N of the vm-<vmid>-disk-N volumes in a config
// value.
var diskIndexPattern = regexp.MustCompile(`vm-(\d+)-disk-(\d+)`)

// AttachSeedVolume does on the fake what the provider's seed script does on a
// node: it imports a seed into storage as VM vmid's next disk volume
// (vm-<vmid>-disk-N; <vmid>/vm-<vmid>-disk-N.raw on a directory storage,
// SetDirStorages), tracks it, and attaches it in slot as a CD-ROM (replacing
// whatever was there, as `qm set` does, which also appends the slot to a
// boot order list that does not name it, bootorder.go). It returns the
// volume.
func (s *Server) AttachSeedVolume(vmid int, storage, slot string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return "", fmt.Errorf("configuration file for VM %d does not exist", vmid)
	}
	next := 0
	seen := configView(vm, vmid)
	for volid, owner := range s.volumes {
		if owner == vmid {
			seen["tracked:"+volid] = volid
		}
	}
	for _, v := range seen {
		str, isString := v.(string)
		if !isString {
			continue
		}
		for _, m := range diskIndexPattern.FindAllStringSubmatch(str, -1) {
			if id, _ := strconv.Atoi(m[1]); id != vmid {
				continue
			}
			if n, _ := strconv.Atoi(m[2]); n >= next {
				next = n + 1
			}
		}
	}
	volid := fmt.Sprintf("%s:vm-%d-disk-%d", storage, vmid, next)
	if s.dirStorages[storage] {
		volid = fmt.Sprintf("%s:%d/vm-%d-disk-%d.raw", storage, vmid, vmid, next)
	}
	s.volumes[volid] = vmid
	if vm.Config == nil {
		vm.Config = map[string]string{}
	}
	value := volid + "," + mediaCDROM
	vm.Config[slot] = value
	// `qm set` is qemu-server's config update: it appends a new CD-ROM to the
	// boot order (bootorder.go).
	appendCDROMToBootOrder(vm.Config, bootOrderList(vm.Config[configKeyBoot]), slot, value)
	return volid, nil
}

// Volumes lists, sorted, the tracked volumes that still exist. Safe for
// concurrent use.
func (s *Server) Volumes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.volumes))
	for v := range s.volumes {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// freeOnDestroy frees the tracked volumes the destroy of VM vmid (whose raw
// config is cfg) frees: see the comment at the top of this file. The caller
// holds s.mu.
func (s *Server) freeOnDestroy(cfg map[string]string, vmid int, unreferenced bool) {
	for key, value := range cfg {
		head, opts, _ := strings.Cut(value, ",")
		if owner, tracked := s.volumes[head]; !tracked || owner != vmid {
			continue
		}
		cdrom := strings.Contains(","+opts+",", ",media=cdrom,")
		if unusedKeyPattern.MatchString(key) || (driveKeyPattern.MatchString(key) && !cdrom) {
			delete(s.volumes, head)
		}
	}
	if unreferenced {
		for volid, owner := range s.volumes {
			if owner == vmid {
				delete(s.volumes, volid)
			}
		}
	}
}

// applyDriveUpdate applies, to VM vmid's raw config, the drive keys of a
// config update form other than the cloud-init drives it asks for (asked):
// each is stored as written. Each unusedN or drive key in the form's delete
// list is removed (an unusedN volume is freed). The caller holds s.mu.
func (s *Server) applyDriveUpdate(vm *VM, form url.Values, asked map[string]string) {
	for key := range form {
		if !driveKeyPattern.MatchString(key) {
			continue
		}
		if _, isCloudInit := asked[key]; isCloudInit {
			continue
		}
		if vm.Config == nil {
			vm.Config = map[string]string{}
		}
		vm.Config[key] = form.Get(key)
	}
	for _, key := range strings.Split(form.Get("delete"), ",") {
		key = strings.TrimSpace(key)
		if !driveKeyPattern.MatchString(key) && !unusedKeyPattern.MatchString(key) {
			continue
		}
		if unusedKeyPattern.MatchString(key) {
			head, _, _ := strings.Cut(vm.Config[key], ",")
			delete(s.volumes, head)
		}
		if vm.Config != nil {
			vm.Config[key] = ""
			removeFromBootOrder(vm.Config, key)
		}
	}
}
