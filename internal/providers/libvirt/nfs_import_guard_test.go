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
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/libvirt/hostconn"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// nfsImportHost is a libvirt host whose `qemu-img info` answers infoJSON and
// that records every host command.
type nfsImportHost struct {
	fakeSeamConn
	infoJSON string
	head     []byte
	// size is the staged object's size (default 1 GiB), copiedSize what the
	// bounded copy wrote (default size), freeBytes the free space (default
	// 1 TiB).
	size, copiedSize, freeBytes int64
	commands                    [][]string
}

// minimalQCOW2Head is the leading bytes of a self-contained qcow2 v2 image.
func minimalQCOW2Head() []byte {
	b := make([]byte, 4096)
	copy(b, "QFI\xfb")
	b[7] = 2   // version
	b[23] = 16 // cluster bits
	return b
}

func (h *nfsImportHost) RunHost(_ context.Context, argv ...string) (*hostconn.Result, error) {
	h.commands = append(h.commands, argv)
	if len(argv) > 2 && argv[0] == "sh" && argv[2] == stagedHeadScript {
		head := h.head
		if head == nil {
			head = minimalQCOW2Head()
		}
		return &hostconn.Result{Stdout: base64.StdEncoding.EncodeToString(head)}, nil
	}
	line := strings.Join(argv, " ")
	switch {
	case strings.HasPrefix(line, "qemu-img info -U --output=json -f raw "):
		return &hostconn.Result{Stdout: fmt.Sprintf(`{"virtual-size": %d}`, h.objectSize())}, nil
	case strings.HasPrefix(line, "stat -f "):
		free := h.freeBytes
		if free == 0 {
			free = 1 << 40
		}
		return &hostconn.Result{Stdout: fmt.Sprintf("%d 4096\n", free/4096)}, nil
	case strings.HasPrefix(line, "stat -c %s "):
		copied := h.copiedSize
		if copied == 0 {
			copied = h.objectSize()
		}
		return &hostconn.Result{Stdout: fmt.Sprintf("%d\n", copied)}, nil
	}
	for i, a := range argv {
		if a == "qemu-img" && i+1 < len(argv) && argv[i+1] == "info" {
			return &hostconn.Result{Stdout: h.infoJSON}, nil
		}
	}
	return &hostconn.Result{}, nil
}

func (h *nfsImportHost) objectSize() int64 {
	if h.size == 0 {
		return 1 << 30
	}
	return h.size
}

// joined returns every recorded command as one line each.
func (h *nfsImportHost) joined() []string {
	out := make([]string, 0, len(h.commands))
	for _, c := range h.commands {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

const stagedNFSURL = "nfs://nfs.example/exports/vmmigrations_team-a_web_export.qcow2?gid=3000&uid=3000"

// TestCopyCheckConvertNFSObject_ReadsTheTenantObjectOnce (review addition):
// the staged NFS object is read once, as a byte copy into the private
// directory; the header check and the convert both read that copy, so the
// NFS server cannot swap the object in between; a header that references
// another file is refused before any convert.
func TestCopyCheckConvertNFSObject_ReadsTheTenantObjectOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		info    string
		wantErr bool
	}{
		"self-contained qcow2": {info: `{"format":"qcow2","virtual-size":1048576}`},
		"backing file":         {info: `{"format":"qcow2","backing-filename":"/etc/libvirt/secrets/x.base64"}`, wantErr: true},
		"json: backing":        {info: `{"format":"qcow2","full-backing-filename":"json:{\"file.driver\":\"nfs\"}"}`, wantErr: true},
		"data file":            {info: `{"format":"qcow2","format-specific":{"type":"qcow2","data":{"data-file":"/dev/vda"}}}`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			host := &nfsImportHost{infoJSON: tc.info}
			wd := &diskWriteDir{h: hostConnRunner{conn: host}, dir: "/var/lib/libvirt/images/.virtrigaud-write.abc"}
			newDisk, err := copyCheckConvertNFSObject(context.Background(), host, wd, stagedNFSURL, "team-a.web-migrated.qcow2", testCopyLimit)

			urlReads := 0
			var convert string
			for _, c := range host.joined() {
				if strings.Contains(c, stagedNFSURL) && !strings.HasPrefix(c, "qemu-img info -U --output=json -f raw ") {
					urlReads++ // anything but the size check (raw: opens no other file, reads no data)
				}
				if strings.Contains(c, "convert -f qcow2 -O qcow2") {
					convert = c
				}
			}
			assert.Equal(t, 1, urlReads, "the tenant-controlled object's bytes are read once")
			require.NotEmpty(t, host.commands)
			var dd string
			for _, c := range host.joined() {
				if strings.Contains(c, "qemu-img dd") {
					dd = c
				}
			}
			assert.Contains(t, dd, "qemu-img dd -f raw -O raw bs=1M count=")
			assert.Contains(t, dd, "if="+stagedNFSURL+
				" of=/var/lib/libvirt/images/.virtrigaud-write.abc/team-a.web-migrated.qcow2"+nfsStagedCopySuffix)
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, convert, "a refused header is never converted")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "/var/lib/libvirt/images/.virtrigaud-write.abc/team-a.web-migrated.qcow2", newDisk)
			assert.Contains(t, convert, "team-a.web-migrated.qcow2"+nfsStagedCopySuffix+" "+newDisk,
				"the convert reads the checked private copy")
		})
	}
}

// TestCopyCheckConvertNFSObject_ParsesTheHeaderBeforeQemuOpensIt (re-review
// Medium 2): a copy whose header names an external data file is refused from
// its bytes alone; `qemu-img info` never opens it.
func TestCopyCheckConvertNFSObject_ParsesTheHeaderBeforeQemuOpensIt(t *testing.T) {
	head := minimalQCOW2Head()
	head[7] = 3     // version 3
	head[79] = 4    // incompatible features: external data file
	head[103] = 104 // header length
	host := &nfsImportHost{infoJSON: `{"format":"qcow2"}`, head: head}
	wd := &diskWriteDir{h: hostConnRunner{conn: host}, dir: "/var/lib/libvirt/images/.virtrigaud-write.abc"}
	_, err := copyCheckConvertNFSObject(context.Background(), host, wd, stagedNFSURL, "team-a.web-migrated.qcow2", testCopyLimit)
	require.Error(t, err)
	for _, c := range host.joined() {
		assert.NotContains(t, c, "info --output=json -f qcow2", "qemu never opens a refused header as qcow2")
		assert.NotContains(t, c, "convert -f qcow2")
	}
}

// testCopyLimit is the copy bound of these tests: 10 GiB.
const testCopyLimit = int64(10) << 30

// TestCopyCheckConvertNFSObject_IsBounded (re-review Medium 1): the tenant's
// NFS server controls the staged object's size. An object larger than the
// bound, or one that does not fit twice in the pool's free space, is refused
// before any copy; an object that grew past the bound during the copy is
// refused after it. Nothing is converted.
func TestCopyCheckConvertNFSObject_IsBounded(t *testing.T) {
	for name, tc := range map[string]struct {
		host     *nfsImportHost
		wantCode codes.Code
		wantDD   bool
	}{
		"larger than the bound":     {host: &nfsImportHost{size: testCopyLimit + 1}, wantCode: codes.InvalidArgument},
		"not enough free space":     {host: &nfsImportHost{size: 4 << 30, freeBytes: 6 << 30}, wantCode: codes.ResourceExhausted},
		"grew past the bound in dd": {host: &nfsImportHost{size: 1 << 30, copiedSize: testCopyLimit + 1}, wantCode: codes.InvalidArgument, wantDD: true},
	} {
		t.Run(name, func(t *testing.T) {
			tc.host.infoJSON = `{"format":"qcow2"}`
			wd := &diskWriteDir{h: hostConnRunner{conn: tc.host}, dir: "/var/lib/libvirt/images/.virtrigaud-write.abc"}
			_, err := copyCheckConvertNFSObject(context.Background(), tc.host, wd, stagedNFSURL, "team-a.web-migrated.qcow2", testCopyLimit)
			require.Error(t, err)
			assert.Equal(t, tc.wantCode, status.Code(err), "got %v", err)
			assert.NotContains(t, status.Convert(err).Message(), "nfs.example", "no location in the refusal")
			ran := strings.Join(tc.host.joined(), "\n")
			assert.Equal(t, tc.wantDD, strings.Contains(ran, "qemu-img dd"))
			assert.NotContains(t, ran, "convert -f qcow2", "nothing is converted")
		})
	}
	assert.Equal(t, int64(10241), migration.StagedCopyDDCount(testCopyLimit), "dd copies at most limit+1 bytes, in MiB")
}
