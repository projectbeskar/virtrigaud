//go:build linux

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

package journal

import (
	"errors"
	"testing"
)

// TestClassifyFS_SignedType checks that a magic number with the top bit set
// still matches when the platform's Statfs_t.Type is a signed 32-bit integer.
func TestClassifyFS_SignedType(t *testing.T) {
	for _, magic := range []int32{-0x00acb2be /* 0xff534d42 cifs */, -0x01acb2be /* 0xfe534d42 smb2 */} {
		if err := classifyFS(uint32(magic), "/state"); !errors.Is(err, ErrNetworkFS) {
			t.Errorf("%#x accepted", uint32(magic))
		}
	}
}

func TestClassifyFS(t *testing.T) {
	for magic, name := range networkFilesystems {
		if err := classifyFS(magic, "/state"); !errors.Is(err, ErrNetworkFS) {
			t.Errorf("%s (%#x) accepted", name, magic)
		}
	}
	for _, local := range []uint32{0xef53 /* ext4 */, 0x58465342 /* xfs */, 0x9123683e /* btrfs */, 0x01021994 /* tmpfs */, 0x794c7630 /* overlayfs */, 0x2fc12fc1 /* zfs */} {
		if err := classifyFS(local, "/state"); err != nil {
			t.Errorf("%#x refused: %v", local, err)
		}
	}
}
