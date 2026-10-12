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

package migration

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
)

// StagedImageHeadBytes is how many leading bytes of a staged image
// CheckStagedImageBytes needs: the qcow2 header with its extensions, or the
// VMDK sparse header with its embedded descriptor. An image whose header does
// not fit is refused.
const StagedImageHeadBytes = 64 * 1024

// Image formats a staged image can be checked as.
const (
	// StagedFormatQCOW2 is a qcow2 image.
	StagedFormatQCOW2 = "qcow2"
	// StagedFormatVMDK is a VMDK image (only the single-file sparse variants).
	StagedFormatVMDK = "vmdk"
	// StagedFormatRaw is a raw image (no header).
	StagedFormatRaw = "raw"
)

// qcow2 header layout (big-endian), docs/interop/qcow2.txt.
const (
	qcow2Magic                 = 0x514649fb // "QFI\xfb"
	qcow2SizeAt                = 24
	qcow2V2HeaderLength        = 72
	qcow2V3MinHeaderLength     = 104
	qcow2IncompatDirty         = 1 << 0
	qcow2IncompatCorrupt       = 1 << 1
	qcow2IncompatDataFile      = 1 << 2
	qcow2IncompatCompression   = 1 << 3
	qcow2IncompatExtendedL2    = 1 << 4
	qcow2IncompatKnown         = qcow2IncompatDirty | qcow2IncompatCorrupt | qcow2IncompatDataFile | qcow2IncompatCompression | qcow2IncompatExtendedL2
	qcow2ExtEnd                = 0x00000000
	qcow2ExtDataFile           = 0x44415441 // "DATA": external data file name
	qcow2ExtFullDiskEncryption = 0x0537be77
	qcow2MinClusterBits        = 9
	qcow2MaxClusterBits        = 21
)

// VMDK sparse header layout (little-endian), VMware "Virtual Disk Format 5.0".
const (
	vmdkSparseMagic          = 0x564d444b // "KDMV"
	vmdkSectorSize           = 512
	vmdkCapacityAt           = 12
	vmdkGDOffsetAt           = 56
	vmdkGDAtEnd              = 0xffffffffffffffff
	vmdkParentRegionBytes    = 20 * vmdkSectorSize
	vmdkDescriptorOffsetAt   = 28
	vmdkDescriptorSizeAt     = 36
	vmdkSparseHeaderMinBytes = 79
	vmdkNoParentCID          = "ffffffff"
)

// otherImageMagics are the leading bytes of image formats that qemu probes
// and that a staged "raw" image must therefore not start with (a probing
// consumer, such as `qm importdisk`, would read it as that format).
var otherImageMagics = [][]byte{
	{'Q', 'F', 'I', 0xfb},                   // qcow2
	{'K', 'D', 'M', 'V'},                    // vmdk sparse
	{'C', 'O', 'W', 'D'},                    // vmdk3
	[]byte("# Disk DescriptorFile"),         // vmdk descriptor
	{'Q', 'E', 'D', 0x00},                   // qed
	[]byte("conectix"),                      // vpc / vhd
	[]byte("vhdxfile"),                      // vhdx
	[]byte("<<< Oracle VM VirtualBox"),      // vdi
	{'L', 'U', 'K', 'S', 0xba, 0xbe},        // luks
	[]byte("WithoutFreeSpace"),              // parallels
	[]byte("WithouFreSpacExt"),              // parallels
	[]byte("Bochs Virtual HD Image"),        // bochs
	{'c', 'l', 'o', 'o', 'p'},               // cloop (shell header follows)
	[]byte("#!/bin/sh\n#V2.0 Format\nmodp"), // cloop
}

// CheckStagedImageBytes checks the leading bytes of a staged migration image
// (at least StagedImageHeadBytes, or the whole file when it is shorter)
// WITHOUT qemu. It runs before anything — `qemu-img info` included — opens the
// image as wantFormat, because qemu opens a qcow2 external data file and VMDK
// extents while it reads the header: a crafted header could otherwise make the
// node or pod open a device node, a host file or an nbd:/http: target, as
// root, before any check refuses it.
//
//   - qcow2: the magic and version 2 or 3; no backing file
//     (backing_file_offset and backing_file_size 0); not encrypted; no
//     incompatible feature other than the known ones and not the external
//     data file bit; header extensions terminated within the bytes read, with
//     no external data file name and no encryption extension.
//   - vmdk: only the single-file sparse variants (monolithicSparse,
//     streamOptimized): the sparse magic; a non-zero capacity (qemu reads a
//     zero-capacity header with a descriptor offset as a descriptor FILE);
//     no grain directory at end (its footer header is not in the bytes read);
//     no parent in the region qemu searches; an embedded descriptor, if any,
//     within the bytes read and NUL-terminated within its declared size, of
//     createType monolithicSparse or streamOptimized, with at most one extent,
//     of type SPARSE, whose file name is a plain name (the file itself: no
//     path, no protocol). A descriptor text file is refused.
//   - raw: no header to trust; refused when it starts like another image
//     format, or when qemu would probe it as a VMDK descriptor (its text rule).
//
// Any other format is refused. It returns the virtual size the header
// declares (qcow2 size, VMDK capacity; -1 for raw, whose size is its file
// size) and refuses one above maxVirtualBytes when that is positive (the
// caller's StagedCopyLimit; #382's import cap at the rebase). The error wraps
// ErrStagedImageRejected and names no path.
func CheckStagedImageBytes(head []byte, wantFormat string, maxVirtualBytes int64) (int64, error) {
	var virtual int64
	switch wantFormat {
	case StagedFormatQCOW2:
		if err := checkQCOW2Header(head); err != nil {
			return 0, err
		}
		virtual = int64(min(binary.BigEndian.Uint64(head[qcow2SizeAt:]), uint64(1)<<62)) //nolint:gosec // clamped
	case StagedFormatVMDK:
		if err := checkVMDKSparseHeader(head); err != nil {
			return 0, err
		}
		virtual = int64(min(binary.LittleEndian.Uint64(head[vmdkCapacityAt:]), uint64(1)<<53) * vmdkSectorSize) //nolint:gosec // clamped
	case StagedFormatRaw:
		for _, magic := range otherImageMagics {
			if bytes.HasPrefix(head, magic) {
				return 0, rejected("the raw image starts like another image format")
			}
		}
		if looksLikeVMDKDescriptor(head) {
			return 0, rejected("the raw image would be probed as a VMDK descriptor")
		}
		// A raw image's virtual size is its file size: the caller bounds it.
		return -1, nil
	default:
		return 0, rejected("format %q is not accepted for a staged image", wantFormat)
	}
	if maxVirtualBytes > 0 && virtual > maxVirtualBytes {
		return virtual, rejected("the image's virtual size is larger than the import size limit")
	}
	return virtual, nil
}

// looksLikeVMDKDescriptor emulates qemu's vmdk_probe text rule on the first
// probe-buffer bytes: after blank space and '#' comment lines, a line
// "version=1", "version=2" or "version=3" makes qemu probe the file as a VMDK
// descriptor (whose extents it would then open).
func looksLikeVMDKDescriptor(head []byte) bool {
	const probeBytes = 2048
	p := head[:min(len(head), probeBytes)]
	for len(p) > 0 {
		switch p[0] {
		case '#':
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				return false
			}
			p = p[i+1:]
		case ' ', '\t', '\r', '\n':
			p = p[1:]
		default:
			for _, v := range []string{"version=1", "version=2", "version=3"} {
				if bytes.HasPrefix(p, []byte(v+"\n")) || bytes.HasPrefix(p, []byte(v+"\r\n")) {
					return true
				}
			}
			return false
		}
	}
	return false
}

// rejected builds an ErrStagedImageRejected error.
func rejected(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrStagedImageRejected, fmt.Sprintf(format, args...))
}

// checkQCOW2Header implements the qcow2 part of CheckStagedImageBytes.
func checkQCOW2Header(head []byte) error {
	if len(head) < qcow2V2HeaderLength {
		return rejected("the image is too short to be qcow2")
	}
	be := binary.BigEndian
	if be.Uint32(head[0:]) != qcow2Magic {
		return rejected("the image is not qcow2")
	}
	version := be.Uint32(head[4:])
	if version != 2 && version != 3 {
		return rejected("qcow2 version %d is not accepted", version)
	}
	if be.Uint64(head[8:]) != 0 || be.Uint32(head[16:]) != 0 {
		return rejected("the image has a backing file; a staged image must be self-contained")
	}
	clusterBits := be.Uint32(head[20:])
	if clusterBits < qcow2MinClusterBits || clusterBits > qcow2MaxClusterBits {
		return rejected("qcow2 cluster size is out of range")
	}
	if be.Uint32(head[32:]) != 0 {
		return rejected("the image is encrypted")
	}
	extStart := uint64(qcow2V2HeaderLength)
	if version == 3 {
		if len(head) < qcow2V3MinHeaderLength {
			return rejected("the qcow2 v3 header is truncated")
		}
		incompat := be.Uint64(head[72:])
		if incompat&qcow2IncompatDataFile != 0 {
			return rejected("the image uses an external data file; a staged image must be self-contained")
		}
		if incompat&^uint64(qcow2IncompatKnown) != 0 {
			return rejected("the image uses unknown incompatible qcow2 features")
		}
		headerLength := uint64(be.Uint32(head[100:]))
		if headerLength < qcow2V3MinHeaderLength || headerLength%8 != 0 {
			return rejected("the qcow2 v3 header length is invalid")
		}
		extStart = headerLength
	}
	// Header extensions: (type, length, data padded to 8 bytes) until type 0.
	off := extStart
	for {
		if off+8 > uint64(len(head)) {
			return rejected("the qcow2 header extensions do not fit in the bytes checked")
		}
		extType := be.Uint32(head[off:])
		extLen := uint64(be.Uint32(head[off+4:]))
		if extType == qcow2ExtEnd {
			return nil
		}
		switch extType {
		case qcow2ExtDataFile:
			return rejected("the image names an external data file; a staged image must be self-contained")
		case qcow2ExtFullDiskEncryption:
			return rejected("the image is encrypted")
		}
		off += 8 + (extLen+7)/8*8
	}
}

// checkVMDKSparseHeader implements the vmdk part of CheckStagedImageBytes.
func checkVMDKSparseHeader(head []byte) error {
	if bytes.HasPrefix(head, []byte("# Disk DescriptorFile")) {
		return rejected("a VMDK descriptor file references other files; only a single-file sparse VMDK is accepted")
	}
	if len(head) < vmdkSparseHeaderMinBytes || binary.LittleEndian.Uint32(head[0:]) != vmdkSparseMagic {
		return rejected("the image is not a sparse VMDK")
	}
	le := binary.LittleEndian
	// qemu (vmdk_open_vmdk4) reads a sparse header whose capacity is 0 and
	// whose descriptor offset is set as a DESCRIPTOR FILE, from that offset to
	// end of file or 1 MiB (not descriptor size), and opens every extent it
	// names. A staged disk always has a capacity.
	if le.Uint64(head[vmdkCapacityAt:]) == 0 {
		return rejected("the VMDK capacity is 0 (qemu would read it as a descriptor file)")
	}
	// A grain directory "at end" makes qemu replace this header with the
	// footer near the end of the file, which the bytes checked here do not
	// include: refused. qemu-img's own streamOptimized output (what the
	// vSphere export writes) does not use it.
	if le.Uint64(head[vmdkGDOffsetAt:]) == vmdkGDAtEnd {
		return rejected("the VMDK keeps its header in a footer (grain directory at end), which cannot be checked")
	}
	// qemu looks for a parent (parentFileNameHint, parentCID) in the fixed
	// region after the header, whatever the descriptor offset says.
	if err := checkVMDKParentRegion(head); err != nil {
		return err
	}
	descOffset := le.Uint64(head[vmdkDescriptorOffsetAt:])
	descSize := le.Uint64(head[vmdkDescriptorSizeAt:])
	if descOffset == 0 {
		// No embedded descriptor: qemu opens it as a single sparse extent.
		return nil
	}
	if descSize == 0 || descOffset > uint64(len(head))/vmdkSectorSize ||
		descSize > uint64(len(head))/vmdkSectorSize ||
		(descOffset+descSize)*vmdkSectorSize > uint64(len(head)) {
		return rejected("the VMDK descriptor does not fit in the bytes checked")
	}
	desc := head[descOffset*vmdkSectorSize : (descOffset+descSize)*vmdkSectorSize]
	i := bytes.IndexByte(desc, 0)
	if i < 0 {
		// qemu would read past the declared descriptor size.
		return rejected("the VMDK descriptor is not terminated within its declared size")
	}
	return checkVMDKDescriptor(string(desc[:i]))
}

// checkVMDKParentRegion refuses a parent named in the fixed region qemu's
// vmdk_parent_open searches (the bytes after the 512-byte header, up to the
// default descriptor size), read up to its first NUL.
func checkVMDKParentRegion(head []byte) error {
	if len(head) <= vmdkSectorSize {
		return nil
	}
	region := head[vmdkSectorSize:min(len(head), vmdkSectorSize+vmdkParentRegionBytes)]
	if i := bytes.IndexByte(region, 0); i >= 0 {
		region = region[:i]
	}
	if bytes.Contains(region, []byte("parentFileNameHint")) {
		return rejected("the VMDK has a parent; a staged image must be self-contained")
	}
	if j := bytes.Index(region, []byte("parentCID")); j >= 0 {
		rest := strings.TrimLeft(string(region[j+len("parentCID"):]), " \t")
		value, _, _ := strings.Cut(strings.TrimPrefix(rest, "="), "\n")
		if !strings.EqualFold(strings.Trim(strings.TrimSpace(value), `"`), vmdkNoParentCID) {
			return rejected("the VMDK has a parent; a staged image must be self-contained")
		}
	}
	return nil
}

// vmdkExtentLine is a descriptor extent line: access, size in sectors, type,
// quoted file name, optional offset.
var vmdkExtentLine = regexp.MustCompile(`^(RW|RDONLY|NOACCESS)\s+\d+\s+(\S+)\s+"([^"]*)"(\s+\d+)?$`)

// vmdkExtentFileIsSelf reports whether an embedded descriptor's extent file
// name can only name the image itself: a plain file name, with no directory,
// no protocol or URL, no "." or "..", and no control character.
func vmdkExtentFileIsSelf(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// checkVMDKDescriptor checks the embedded descriptor of a sparse VMDK.
func checkVMDKDescriptor(desc string) error {
	createType := ""
	extents := 0
	sc := bufio.NewScanner(strings.NewReader(desc))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, isAssignment := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch {
		case isAssignment && strings.EqualFold(key, "createType"):
			createType = value
		case isAssignment && strings.EqualFold(key, "parentFileNameHint"):
			return rejected("the VMDK has a parent; a staged image must be self-contained")
		case isAssignment && strings.EqualFold(key, "parentCID"):
			if !strings.EqualFold(value, vmdkNoParentCID) {
				return rejected("the VMDK has a parent; a staged image must be self-contained")
			}
		case strings.HasPrefix(line, "RW") || strings.HasPrefix(line, "RDONLY") || strings.HasPrefix(line, "NOACCESS"):
			extents++
			m := vmdkExtentLine.FindStringSubmatch(line)
			if m == nil || m[2] != "SPARSE" || !vmdkExtentFileIsSelf(m[3]) {
				return rejected("the VMDK extent is not a sparse extent of the file itself")
			}
		}
	}
	if createType != "monolithicSparse" && createType != "streamOptimized" {
		return rejected("VMDK createType %q is not accepted (only monolithicSparse or streamOptimized)", createType)
	}
	if extents > 1 {
		return rejected("the VMDK has more than one extent; a staged image must be self-contained")
	}
	return nil
}
