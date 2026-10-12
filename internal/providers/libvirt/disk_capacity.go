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

package libvirt

import (
	"context"
	"encoding/json"
	"log"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// diskCapacityTargets returns, for every disk device of the domain (not
// CD-ROM/floppy media, not a cloud-init seed), its host file — or "" for a
// disk that is not file-backed (block, volume or network source), whose size
// GetDiskInfo does not read.
func (d *domainDisksDoc) diskCapacityTargets() []domainDiskElem {
	var out []domainDiskElem
	for _, e := range d.Devices.Disks {
		if e.Device != "" && e.Device != diskDeviceDisk {
			continue
		}
		if f := e.file(); f != "" && isCloudInitSeedName(f) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// domainDiskCapacities lists every disk of the domain (diskCapacityTargets)
// with its virtual size, read from the disk file itself with qemu-img in the
// format its definition names (readDiskInfoOnHost). A disk that is not
// file-backed, or whose size could not be read, is listed with size 0, which
// the manager treats as unknown (it fails closed under a disk-size limit).
func domainDiskCapacities(ctx context.Context, h hostCommandRunner, doc *domainDisksDoc) []contracts.DiskCapacity {
	targets := doc.diskCapacityTargets()
	out := make([]contracts.DiskCapacity, 0, len(targets))
	for _, e := range targets {
		path := e.file()
		c := contracts.DiskCapacity{ID: path, Path: path}
		if path != "" {
			c.VirtualSizeBytes = qemuImgVirtualSize(ctx, h, path, e.format())
		}
		out = append(out, c)
	}
	return out
}

// qemuImgVirtualSize is the virtual size qemu-img reports for path read as
// format, or 0 when it cannot be read.
func qemuImgVirtualSize(ctx context.Context, h hostCommandRunner, path, format string) int64 {
	res, err := readDiskInfoOnHost(ctx, h, path, format)
	if err != nil {
		log.Printf("WARN qemu-img info failed for %s (its size is reported as unknown): %v", path, err)
		return 0
	}
	var qi struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &qi); err != nil {
		log.Printf("WARN Failed to parse qemu-img info for %s: %v", path, err)
		return 0
	}
	return qi.VirtualSize
}
