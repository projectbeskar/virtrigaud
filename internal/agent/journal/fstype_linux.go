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
	"fmt"
	"syscall"
)

// networkFilesystems are the statfs magic numbers of network, cluster and
// user-space file systems, on which fsync, flock and O_EXCL do not give the
// guarantees the journal relies on. The list is best-effort: a file system it
// does not name is not refused, so a local file system stays the operator's
// responsibility. Magic numbers are 32-bit; they are compared as uint32 so that
// values with the top bit set also match where Statfs_t.Type is a signed 32-bit
// integer.
var networkFilesystems = map[uint32]string{
	0x6969:     "nfs",
	0x517b:     "smb",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x00c36400: "ceph",
	0x5346414f: "afs",
	0x73757245: "coda",
	0x01021997: "9p",
	0x65735546: "fuse",
	0x0bd00bd0: "lustre",
	0x47504653: "gpfs",
	0x01161970: "gfs2",
	0x7461636f: "ocfs2",
	0x19830326: "beegfs",
	0x20030528: "orangefs",
}

// checkLocalFS refuses a state directory on a network or user-space file
// system.
func checkLocalFS(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	return classifyFS(uint32(st.Type), dir) //nolint:gosec,unconvert // #nosec G115 -- file-system magic numbers are 32-bit; Statfs_t.Type is int32 or int64 depending on the architecture
}

// classifyFS refuses the network and user-space file-system magic numbers.
func classifyFS(magic uint32, dir string) error {
	if name, ok := networkFilesystems[magic]; ok {
		return fmt.Errorf("%w: %s is on %s", ErrNetworkFS, dir, name)
	}
	return nil
}
