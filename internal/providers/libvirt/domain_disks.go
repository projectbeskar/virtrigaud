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
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"
)

// Structural reading of a domain's own disks.
//
// `virsh dumpxml` of a RUNNING domain lists each disk's whole image chain as
// nested <backingStore><source file='...'/></backingStore> elements. For a
// linked clone — a qcow2 overlay made with `qemu-img create -b <source disk>`
// (clone.go) — that chain holds the SOURCE VM's disk. Delete used to scan the
// document line by line and collect every <source file=...>, so deleting a
// running linked clone deleted its source VM's disk too.
//
// Everything here reads the document with encoding/xml and looks only at the
// top-level <devices><disk> elements and their own <source> child. A
// <backingStore> (or a block job's <mirror>) is a child of <disk>, so its
// <source> is never matched; cdrom and floppy media are kept apart from disks.

// Disk element values read from a domain definition.
const (
	// diskDeviceDisk is <disk device='disk'>, libvirt's default device.
	diskDeviceDisk = "disk"
	// diskDeviceCDROM is <disk device='cdrom'>.
	diskDeviceCDROM = "cdrom"
	// diskDeviceFloppy is <disk device='floppy'>.
	diskDeviceFloppy = "floppy"
	// diskTypeFile is <disk type='file'>: a disk backed by a host file.
	diskTypeFile = "file"

	// cloudInitISOName is the file name of the seed ISO PrepareCloudInit
	// builds in a seed directory (and earlier releases built in theirs).
	cloudInitISOName = "cloud-init.iso"
	// legacyCloudInitSeedRoot is the directory, inside the staging directory,
	// that earlier releases kept one seed directory per domain name in:
	// <staging>/virtrigaud-cloudinit/<domain name>/cloud-init.iso.
	legacyCloudInitSeedRoot = "virtrigaud-cloudinit"
)

// domainDisksDoc is the part of a domain definition that Delete, the disk
// dependency guard and disk resolution read.
type domainDisksDoc struct {
	XMLName xml.Name `xml:"domain"`
	Name    string   `xml:"name"`
	UUID    string   `xml:"uuid"`
	Devices struct {
		Disks []domainDiskElem `xml:"disk"`
	} `xml:"devices"`
}

// domainDiskElem is one top-level <devices><disk> element. Source is its OWN
// <source> child only: the <source> of a nested <backingStore> or <mirror> is
// not a child of <disk> and is never decoded into it.
type domainDiskElem struct {
	Type   string `xml:"type,attr"`
	Device string `xml:"device,attr"`
	Source *struct {
		File string `xml:"file,attr"`
	} `xml:"source"`
}

// parseDomainDisks decodes a `virsh dumpxml` document. A document whose root is
// not <domain> is an error.
func parseDomainDisks(domainXML string) (*domainDisksDoc, error) {
	var d domainDisksDoc
	if err := xml.Unmarshal([]byte(domainXML), &d); err != nil {
		return nil, fmt.Errorf("parse domain definition: %w", err)
	}
	d.Name = strings.TrimSpace(d.Name)
	d.UUID = strings.TrimSpace(d.UUID)
	return &d, nil
}

// file returns the host file a file-backed disk element's own <source>
// names, or "" for any other element.
func (e domainDiskElem) file() string {
	if e.Type != diskTypeFile || e.Source == nil {
		return ""
	}
	return e.Source.File
}

// isMedia reports whether the element is removable media (cdrom or floppy).
func (e domainDiskElem) isMedia() bool {
	return e.Device == diskDeviceCDROM || e.Device == diskDeviceFloppy
}

// isCloudInitSeedName reports whether path names a cloud-init seed ISO by the
// file-name conventions this provider has used (…cloud-init.iso, …-cidata.iso).
func isCloudInitSeedName(path string) bool {
	return strings.HasSuffix(path, "-cidata.iso") || strings.HasSuffix(path, cloudInitISOName)
}

// diskFiles returns the host files of the domain's own file-backed disks —
// top-level <disk type='file' device='disk'> elements (device='disk' is
// libvirt's default when the attribute is absent) — in document order, the
// primary disk first. It never returns a <backingStore> source (another image
// in the disk's chain, such as a linked clone's source disk), cdrom or floppy
// media, or a cloud-init seed.
func (d *domainDisksDoc) diskFiles() []string {
	var files []string
	for _, e := range d.Devices.Disks {
		if e.Device != "" && e.Device != diskDeviceDisk {
			continue
		}
		f := e.file()
		if f == "" || isCloudInitSeedName(f) {
			continue
		}
		files = append(files, f)
	}
	return files
}

// cloudInitSeedDir returns the VirtRigaud cloud-init seed directory the
// domain's removable media reference, or "" when there is none. It is the
// directory of a top-level cdrom/floppy source named cloud-init.iso that is
// either
//
//   - a per-create seed directory directly inside stagingDir
//     (<staging>/virtrigaud-cloudinit-<domain>.<random>, PrepareCloudInit), or
//   - the per-name seed directory of an earlier release,
//     <staging>/virtrigaud-cloudinit/<this domain's name>.
//
// Any other ISO — a seed elsewhere, an installer image — is not VirtRigaud's to
// remove, and neither is a directory named after another domain.
func (d *domainDisksDoc) cloudInitSeedDir(stagingDir string) string {
	staging := filepath.Clean(stagingDir)
	for _, e := range d.Devices.Disks {
		f := e.file()
		if !e.isMedia() || f == "" || !strings.HasPrefix(f, "/") {
			continue
		}
		f = filepath.Clean(f)
		if filepath.Base(f) != cloudInitISOName {
			continue
		}
		dir := filepath.Dir(f)
		parent, base := filepath.Dir(dir), filepath.Base(dir)
		switch {
		case parent == staging && strings.HasPrefix(base, cloudInitSeedDirPrefix) && len(base) > len(cloudInitSeedDirPrefix):
			return dir
		case parent == filepath.Join(staging, legacyCloudInitSeedRoot) && d.Name != "" && base == d.Name:
			return dir
		}
	}
	return ""
}
