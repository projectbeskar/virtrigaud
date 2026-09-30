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
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// The format of a clone's disk (ADR-0007 Slice 5 lab follow-up).
//
// Every clone disk VirtRigaud writes is qcow2, whatever the source's format:
// a full clone's copy is `qemu-img convert -O qcow2` (as root or as the SSH
// user), and a linked clone's overlay is `qemu-img create -f qcow2`, both at
// "<domain>-disk.qcow2". The clone is defined from the source's XML, so a
// source whose disk is raw used to hand its clone `<driver type='raw'>` over a
// qcow2 file: QEMU then presented the qcow2 container itself to the guest as a
// raw disk, and the clone did not boot. The clone's primary disk is therefore
// always declared in the format written (setDiskDriverType). Writing qcow2 for
// every source — rather than keeping raw for a raw source — is the one rule
// that holds on every path: the SSH-user fallback copy does not know the
// source's format (it lets qemu-img probe it), the disk's name says qcow2, and
// every other VM disk VirtRigaud creates is qcow2.

// cloneDiskFormat is the format of every clone disk VirtRigaud writes.
const cloneDiskFormat = "qcow2"

// driverTypeAttrRE matches the type attribute of a <driver> start tag (either
// quote style). The leading whitespace keeps it from matching an attribute
// that merely ends in "type".
var driverTypeAttrRE = regexp.MustCompile(`\stype\s*=\s*(?:'[^']*'|"[^"]*")`)

// diskDriverPath is where setDiskDriverType looks for disks: the direct
// children of /domain/devices, none in an XML namespace.
var diskDriverPath = []string{"domain", "devices"}

// setDiskDriverType declares the file-backed disk element of domainXML whose
// own <source file=...> is diskFile in format: its direct <driver> child's
// type attribute is set to format (added when absent), or, when the disk has
// no <driver>, a `<driver name='qemu' type='<format>'/>` is inserted right
// after the disk's start tag. Only those bytes change; every other byte of the
// document is kept, and a driver already of that type is left untouched. A
// document with no such disk is returned unchanged. format must be a qemu
// format name.
func setDiskDriverType(domainXML, diskFile, format string) (string, error) {
	if !backingFormatRE.MatchString(format) {
		return "", fmt.Errorf("invalid disk format %q", format)
	}
	type span struct{ start, end int64 }
	type diskState struct {
		startTagEnd int64
		driver      *span
		file        string
	}
	dec := xml.NewDecoder(strings.NewReader(domainXML))
	var stack []xml.Name
	var cur *diskState
	var found *diskState
	for {
		offset := dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse domain XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name == (xml.Name{Local: "disk"}) && atUnqualifiedPath(stack, diskDriverPath):
				cur = &diskState{startTagEnd: dec.InputOffset()}
			case cur != nil && len(stack) == len(diskDriverPath)+1 && t.Name == (xml.Name{Local: "driver"}) && cur.driver == nil:
				cur.driver = &span{start: offset, end: dec.InputOffset()}
			case cur != nil && len(stack) == len(diskDriverPath)+1 && t.Name == (xml.Name{Local: "source"}):
				for _, a := range t.Attr {
					if a.Name == (xml.Name{Local: "file"}) {
						cur.file = a.Value
					}
				}
			}
			stack = append(stack, t.Name)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			if cur != nil && len(stack) == len(diskDriverPath) {
				if cur.file == diskFile && found == nil {
					found = cur
				}
				cur = nil
			}
		}
	}
	if found == nil {
		return domainXML, nil
	}
	if found.driver == nil {
		insert := fmt.Sprintf("<driver name='qemu' type='%s'/>", format)
		return domainXML[:found.startTagEnd] + insert + domainXML[found.startTagEnd:], nil
	}
	tag := domainXML[found.driver.start:found.driver.end]
	want := fmt.Sprintf(" type='%s'", format)
	var next string
	if m := driverTypeAttrRE.FindString(tag); m != "" {
		if strings.Contains(m, "'"+format+"'") || strings.Contains(m, `"`+format+`"`) {
			return domainXML, nil
		}
		next = strings.Replace(tag, m, want, 1)
	} else {
		cut := len(tag) - len(">")
		if strings.HasSuffix(tag, "/>") {
			cut = len(tag) - len("/>")
		}
		next = tag[:cut] + want + tag[cut:]
	}
	return domainXML[:found.driver.start] + next + domainXML[found.driver.end:], nil
}
