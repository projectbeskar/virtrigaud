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
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// qcow2Image crafts the leading bytes of a qcow2 image: version, backing
// offset/size, crypt method, incompatible features, and header extensions
// (type, payload) after the header. The layout matches what qemu-img 8.2
// writes (checked against real images when this parser was written).
type qcow2Image struct {
	version       uint32
	backingOffset uint64
	backingSize   uint32
	clusterBits   uint32
	crypt         uint32
	incompat      uint64
	headerLength  uint32
	exts          []qcow2Ext
	noEnd         bool
}

type qcow2Ext struct {
	typ  uint32
	data []byte
}

func (q qcow2Image) bytes() []byte {
	b := make([]byte, StagedImageHeadBytes)
	be := binary.BigEndian
	be.PutUint32(b[0:], qcow2Magic)
	be.PutUint32(b[4:], q.version)
	be.PutUint64(b[8:], q.backingOffset)
	be.PutUint32(b[16:], q.backingSize)
	be.PutUint32(b[20:], q.clusterBits)
	be.PutUint64(b[24:], 1<<30)
	be.PutUint32(b[32:], q.crypt)
	off := uint32(qcow2V2HeaderLength)
	if q.version == 3 {
		be.PutUint64(b[72:], q.incompat)
		be.PutUint32(b[96:], 4)
		be.PutUint32(b[100:], q.headerLength)
		off = q.headerLength
	}
	for _, e := range q.exts {
		be.PutUint32(b[off:], e.typ)
		be.PutUint32(b[off+4:], uint32(len(e.data)))
		copy(b[off+8:], e.data)
		off += 8 + uint32((len(e.data)+7)/8*8)
	}
	if q.noEnd {
		for i := off; i+8 <= uint32(len(b)); i += 8 {
			be.PutUint32(b[i:], 0x6803f857) // feature name table, forever
			be.PutUint32(b[i+4:], 0)
		}
	}
	return b
}

func goodQCOW2(version uint32) qcow2Image {
	return qcow2Image{version: version, clusterBits: 16, headerLength: 112,
		exts: []qcow2Ext{{typ: 0x6803f857, data: make([]byte, 48)}}} // feature name table
}

func TestCheckStagedImageBytes_QCOW2(t *testing.T) {
	assert.NoError(t, checkBytes(goodQCOW2(3).bytes(), StagedFormatQCOW2))
	assert.NoError(t, checkBytes(goodQCOW2(2).bytes(), StagedFormatQCOW2))
	big := goodQCOW2(3)
	big.clusterBits = 21
	assert.NoError(t, checkBytes(big.bytes(), StagedFormatQCOW2), "2 MiB clusters")

	for name, mutate := range map[string]func(q *qcow2Image){
		"backing file offset":            func(q *qcow2Image) { q.backingOffset = 0x10000 },
		"backing file size only":         func(q *qcow2Image) { q.backingSize = 16 },
		"encrypted (crypt method)":       func(q *qcow2Image) { q.crypt = 2 },
		"external data file bit":         func(q *qcow2Image) { q.incompat = qcow2IncompatDataFile },
		"unknown incompatible feature":   func(q *qcow2Image) { q.incompat = 1 << 40 },
		"data file name extension":       func(q *qcow2Image) { q.exts = append(q.exts, qcow2Ext{qcow2ExtDataFile, []byte("nbd://10.0.0.1/x")}) },
		"encryption extension":           func(q *qcow2Image) { q.exts = append(q.exts, qcow2Ext{qcow2ExtFullDiskEncryption, make([]byte, 16)}) },
		"extensions never terminated":    func(q *qcow2Image) { q.noEnd = true },
		"version 1":                      func(q *qcow2Image) { q.version = 1 },
		"cluster bits out of range":      func(q *qcow2Image) { q.clusterBits = 40 },
		"header length not 8-aligned":    func(q *qcow2Image) { q.headerLength = 105 },
		"header length below v3 minimum": func(q *qcow2Image) { q.headerLength = 96 },
	} {
		t.Run(name, func(t *testing.T) {
			q := goodQCOW2(3)
			mutate(&q)
			err := checkBytes(q.bytes(), StagedFormatQCOW2)
			assert.ErrorIs(t, err, ErrStagedImageRejected)
		})
	}

	assert.ErrorIs(t, checkBytes([]byte("QFI\xfb"), StagedFormatQCOW2), ErrStagedImageRejected, "truncated")
	assert.ErrorIs(t, checkBytes(make([]byte, 4096), StagedFormatQCOW2), ErrStagedImageRejected, "no magic")
}

// vmdkImage crafts a sparse VMDK whose embedded descriptor (at sector 1) is
// desc; descSectors 0 means no embedded descriptor.
func vmdkImage(desc string, descSectors uint64) []byte {
	b := make([]byte, StagedImageHeadBytes)
	le := binary.LittleEndian
	le.PutUint32(b[0:], vmdkSparseMagic)
	le.PutUint32(b[4:], 1)
	le.PutUint64(b[vmdkCapacityAt:], 2097152) // 1 GiB in sectors
	le.PutUint64(b[vmdkGDOffsetAt:], 30)
	if descSectors > 0 {
		le.PutUint64(b[vmdkDescriptorOffsetAt:], 1)
		le.PutUint64(b[vmdkDescriptorSizeAt:], descSectors)
		copy(b[vmdkSectorSize:], desc)
	}
	return b
}

func vmdkDesc(createType, extentLine, extra string) string {
	return fmt.Sprintf("# Disk DescriptorFile\nversion=1\nCID=12345678\nparentCID=ffffffff\ncreateType=%q\n%s\n# Extent description\n%s\n",
		createType, extra, extentLine)
}

func TestCheckStagedImageBytes_VMDK(t *testing.T) {
	for _, ct := range []string{"monolithicSparse", "streamOptimized"} {
		assert.NoError(t, checkBytes(vmdkImage(vmdkDesc(ct, `RW 2097152 SPARSE "disk.vmdk"`, ""), 20), StagedFormatVMDK), ct)
	}
	assert.NoError(t, checkBytes(vmdkImage("", 0), StagedFormatVMDK), "no embedded descriptor")

	for name, img := range map[string][]byte{
		"descriptor text file":         []byte(vmdkDesc("monolithicFlat", `RW 2097152 FLAT "/dev/sda" 0`, "")),
		"flat extent in a sparse file": vmdkImage(vmdkDesc("monolithicSparse", `RW 2097152 FLAT "/etc/shadow" 0`, ""), 20),
		"two extents": vmdkImage(vmdkDesc("monolithicSparse",
			"RW 1048576 SPARSE \"a.vmdk\"\nRW 1048576 SPARSE \"/root/.ssh/id_rsa\"", ""), 20),
		"parent hint":             vmdkImage(vmdkDesc("monolithicSparse", `RW 2097152 SPARSE "disk.vmdk"`, `parentFileNameHint="/var/lib/x.vmdk"`), 20),
		"parent CID":              vmdkImage(`createType="monolithicSparse"`+"\nparentCID=0badcafe\nRW 2097152 SPARSE \"disk.vmdk\"\n", 20),
		"twoGbMaxExtentSparse":    vmdkImage(vmdkDesc("twoGbMaxExtentSparse", `RW 2097152 SPARSE "disk-s001.vmdk"`, ""), 20),
		"descriptor out of range": vmdkImage(vmdkDesc("monolithicSparse", `RW 1 SPARSE "x"`, ""), 1<<20),
		"vmdk3 (COWD)":            append([]byte("COWD"), make([]byte, 4092)...),
		"not vmdk":                goodQCOW2(3).bytes(),
	} {
		assert.ErrorIs(t, checkBytes(img, StagedFormatVMDK), ErrStagedImageRejected, name)
	}
}

func TestCheckStagedImageBytes_RawAndOthers(t *testing.T) {
	assert.NoError(t, checkBytes(make([]byte, 4096), StagedFormatRaw))
	assert.NoError(t, checkBytes([]byte("hello world"), StagedFormatRaw))
	for name, img := range map[string][]byte{
		"qcow2 posing as raw": goodQCOW2(3).bytes(),
		"vmdk posing as raw":  vmdkImage("", 0),
		"luks posing as raw":  append([]byte{'L', 'U', 'K', 'S', 0xba, 0xbe}, make([]byte, 64)...),
		"vhdx posing as raw":  []byte("vhdxfile...."),
	} {
		assert.ErrorIs(t, checkBytes(img, StagedFormatRaw), ErrStagedImageRejected, name)
	}
	assert.ErrorIs(t, checkBytes(goodQCOW2(3).bytes(), "vpc"), ErrStagedImageRejected, "unsupported format")
}

// TestCheckStagedImageBytes_VMDKQemuPaths (re-review N2): the qemu paths that
// read more than the declared header and descriptor are refused.
func TestCheckStagedImageBytes_VMDKQemuPaths(t *testing.T) {
	le := binary.LittleEndian
	good := vmdkDesc("streamOptimized", `RW 2097152 SPARSE "disk.vmdk"`, "")

	zeroCap := vmdkImage(good, 20)
	le.PutUint64(zeroCap[vmdkCapacityAt:], 0)
	assert.ErrorIs(t, checkBytes(zeroCap, StagedFormatVMDK), ErrStagedImageRejected, "capacity 0 is read as a descriptor file")

	atEnd := vmdkImage(good, 20)
	le.PutUint64(atEnd[vmdkGDOffsetAt:], vmdkGDAtEnd)
	assert.ErrorIs(t, checkBytes(atEnd, StagedFormatVMDK), ErrStagedImageRejected, "the footer cannot be checked")

	// A descriptor that fills its declared sectors with no NUL.
	unterminated := vmdkImage("", 0)
	le.PutUint64(unterminated[vmdkDescriptorOffsetAt:], 1)
	le.PutUint64(unterminated[vmdkDescriptorSizeAt:], 1)
	text := []byte(good)
	for len(text) < vmdkSectorSize {
		text = append(text, '#')
	}
	copy(unterminated[vmdkSectorSize:], text[:vmdkSectorSize])
	copy(unterminated[2*vmdkSectorSize:], "\nRW 1 FLAT \"/dev/sda\" 0\n")
	assert.ErrorIs(t, checkBytes(unterminated, StagedFormatVMDK), ErrStagedImageRejected, "no NUL within the declared size")

	for _, name := range []string{"/etc/shadow", "../disk.vmdk", "nbd://10.0.0.1/x", "json:{}", "dir/disk.vmdk", ""} {
		img := vmdkImage(vmdkDesc("monolithicSparse", fmt.Sprintf(`RW 2097152 SPARSE %q`, name), ""), 20)
		assert.ErrorIs(t, checkBytes(img, StagedFormatVMDK), ErrStagedImageRejected, "extent file %q", name)
	}

	// A parent named in the region qemu searches, though the header claims no
	// embedded descriptor.
	hidden := vmdkImage("", 0)
	copy(hidden[vmdkSectorSize:], "parentFileNameHint=\"/var/lib/libvirt/images/other.vmdk\"\n")
	assert.ErrorIs(t, checkBytes(hidden, StagedFormatVMDK), ErrStagedImageRejected)
	hiddenCID := vmdkImage("", 0)
	copy(hiddenCID[vmdkSectorSize:], "parentCID=12345678\n")
	assert.ErrorIs(t, checkBytes(hiddenCID, StagedFormatVMDK), ErrStagedImageRejected)
}

// checkBytes is CheckStagedImageBytes with no virtual-size bound.
func checkBytes(head []byte, format string) error {
	_, err := CheckStagedImageBytes(head, format, 0)
	return err
}

// TestCheckStagedImageBytes_VirtualSize (re-review N4a): the declared virtual
// size is returned and bounded.
func TestCheckStagedImageBytes_VirtualSize(t *testing.T) {
	q := goodQCOW2(3).bytes()
	v, err := CheckStagedImageBytes(q, StagedFormatQCOW2, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1)<<30, v)
	_, err = CheckStagedImageBytes(q, StagedFormatQCOW2, (1<<30)-1)
	assert.ErrorIs(t, err, ErrStagedImageRejected, "above the bound")
	_, err = CheckStagedImageBytes(q, StagedFormatQCOW2, 1<<30)
	assert.NoError(t, err)

	vm := vmdkImage("", 0)
	v, err = CheckStagedImageBytes(vm, StagedFormatVMDK, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2097152)*512, v)
	huge := vmdkImage("", 0)
	binary.LittleEndian.PutUint64(huge[vmdkCapacityAt:], 1<<62)
	_, err = CheckStagedImageBytes(huge, StagedFormatVMDK, 1<<40)
	assert.ErrorIs(t, err, ErrStagedImageRejected, "no overflow past the bound")

	v, err = CheckStagedImageBytes(make([]byte, 512), StagedFormatRaw, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(-1), v, "raw: the caller bounds the file size")
}

// TestCheckStagedImageBytes_RawProbedAsVMDKDescriptor (re-review Low): a raw
// image whose text qemu's vmdk_probe would read as a VMDK descriptor is refused.
func TestCheckStagedImageBytes_RawProbedAsVMDKDescriptor(t *testing.T) {
	for _, text := range []string{
		"version=1\nRW 1 FLAT \"/dev/sda\" 0\n",
		"# comment\n\n   # another\nversion=3\r\n",
		"  \n\tversion=2\n",
	} {
		assert.ErrorIs(t, checkBytes([]byte(text), StagedFormatRaw), ErrStagedImageRejected, "%q", text)
	}
	for _, text := range []string{"version=4\n", "data version=1\n", "# only a comment", "version=1"} {
		assert.NoError(t, checkBytes([]byte(text), StagedFormatRaw), "%q", text)
	}
}

// TestBoundedWriter (re-review N4b): writes up to Max pass; one more byte fails
// with ErrStagedObjectTooLarge and is not written.
func TestBoundedWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &BoundedWriter{W: &buf, Max: 5}
	_, err := w.Write([]byte("abc"))
	require.NoError(t, err)
	_, err = w.Write([]byte("de"))
	require.NoError(t, err)
	_, err = w.Write([]byte("f"))
	assert.ErrorIs(t, err, ErrStagedObjectTooLarge)
	assert.Equal(t, "abcde", buf.String())
}
