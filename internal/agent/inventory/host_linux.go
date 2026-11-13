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

package inventory

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// cgroupRoot is where a unified cgroup hierarchy is mounted.
const cgroupRoot = "/sys/fs/cgroup"

// maxProcValueBytes bounds a /proc value read.
const maxProcValueBytes = 256

// SystemHost is the Host of the running Linux kernel.
type SystemHost struct{}

var _ Host = SystemHost{}

// NamespaceInode implements Host from the /proc/self/ns/<kind> link, which
// reads as "<kind>:[<inode>]".
func (SystemHost) NamespaceInode(kind string) (uint64, error) {
	link, err := os.Readlink("/proc/self/ns/" + kind)
	if err != nil {
		return 0, err
	}
	rest, ok := strings.CutPrefix(link, kind+":[")
	if !ok || !strings.HasSuffix(rest, "]") {
		return 0, fmt.Errorf("unexpected %s namespace link", kind)
	}
	return strconv.ParseUint(strings.TrimSuffix(rest, "]"), 10, 64)
}

// Cgroup2Mounted implements Host with statfs on the cgroup root.
func (SystemHost) Cgroup2Mounted() (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &st); err != nil {
		return false, err
	}
	return st.Type == unix.CGROUP2_SUPER_MAGIC, nil
}

// KernelRelease implements Host from /proc/sys/kernel/osrelease.
func (SystemHost) KernelRelease() (string, error) {
	f, err := os.Open("/proc/sys/kernel/osrelease")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxProcValueBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return "", err
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// PidfdOpenSelf implements Host: pidfd_open on the agent's own process.
func (SystemHost) PidfdOpenSelf() error {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// Magic number the probes check.
const (
	pidfsMagic = 0x50494446
)

// openPidfdSelf opens a process file descriptor of the agent itself. The
// caller closes it. An absent pidfd_open is reported as (-1, false, nil).
func openPidfdSelf() (fd int, ok bool, err error) {
	fd, err = unix.PidfdOpen(os.Getpid(), 0)
	switch {
	case err == nil:
		return fd, true, nil
	case errors.Is(err, unix.ENOSYS):
		return -1, false, nil
	}
	return -1, false, err
}

// PidfsHandle implements Host: a file handle taken from the agent's own
// process file descriptor must open the same process again (open_by_handle_at
// on a descriptor of the same file system), which compares equal by inode.
// Any refusal of either call means the host does not support persistent
// process handles to this agent.
func (SystemHost) PidfsHandle() (bool, error) {
	fd, ok, err := openPidfdSelf()
	if err != nil || !ok {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()
	h, _, err := unix.NameToHandleAt(fd, "", unix.AT_EMPTY_PATH)
	if err != nil {
		return false, nil //nolint:nilerr // a refusal means "not supported"
	}
	again, err := unix.OpenByHandleAt(fd, h, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return false, nil //nolint:nilerr // a refusal means "not supported"
	}
	defer func() { _ = unix.Close(again) }()
	var a, b unix.Stat_t
	if unix.Fstat(fd, &a) != nil || unix.Fstat(again, &b) != nil {
		return false, nil
	}
	return a.Ino == b.Ino, nil
}

// PidfdOnPidfs implements Host: fstatfs on the agent's own process file
// descriptor.
func (SystemHost) PidfdOnPidfs() (bool, error) {
	fd, ok, err := openPidfdSelf()
	if err != nil || !ok {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return false, err
	}
	return uint64(st.Type) == pidfsMagic, nil // #nosec G115 -- a file system magic number
}

// FSMagic implements Host with statfs.
func (SystemHost) FSMagic(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Type), nil // #nosec G115 -- a file system magic number
}
