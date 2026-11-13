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

package hostcheck

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// procSelf is the process's own /proc directory.
const procSelf = "/proc/self"

// maxMountinfoBytes bounds the mount table read.
const maxMountinfoBytes = 16 << 20

// System is the Probe of the running Linux host. It reads /proc/self, which
// needs no privilege.
type System struct{}

var _ Probe = System{}

// Lstat implements Probe.
func (System) Lstat(path string) (FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return FileInfo{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileInfo{}, fmt.Errorf("stat %s: no owner information", path)
	}
	return FileInfo{Mode: fi.Mode(), UID: st.Uid}, nil
}

// ReadDir implements Probe.
func (System) ReadDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// ReadFile implements Probe. It does not follow a symbolic link.
func (System) ReadFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0) // #nosec G304 -- a path of the agent's own configuration
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return b, nil
}

// MountOf implements Probe: the mount with the longest mount point that
// contains path, from /proc/self/mountinfo.
func (s System) MountOf(path string) (Mount, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Mount{}, err
	}
	raw, err := s.ReadFile(filepath.Join(procSelf, "mountinfo"), maxMountinfoBytes)
	if err != nil {
		return Mount{}, err
	}
	return mountOf(string(raw), resolved)
}

// mountOf finds the mount of path in a mountinfo table. Later entries win
// over earlier ones at the same mount point (the later mount covers the
// earlier).
func mountOf(table, path string) (Mount, error) {
	var best Mount
	found := false
	sc := bufio.NewScanner(strings.NewReader(table))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		m, ok := parseMountinfoLine(sc.Text())
		if !ok || !within(path, m.MountPoint) {
			continue
		}
		if !found || len(m.MountPoint) >= len(best.MountPoint) {
			best, found = m, true
		}
	}
	if err := sc.Err(); err != nil {
		return Mount{}, fmt.Errorf("read mountinfo: %w", err)
	}
	if !found {
		return Mount{}, fmt.Errorf("no mount holds %s", path)
	}
	return best, nil
}

// within reports whether path is mountPoint or lies under it.
func within(path, mountPoint string) bool {
	if mountPoint == "/" || path == mountPoint {
		return true
	}
	return strings.HasPrefix(path, mountPoint+"/")
}

// parseMountinfoLine parses one line of /proc/<pid>/mountinfo:
//
//	id parent major:minor root mount-point options [optional...] - fstype source super-options
func parseMountinfoLine(line string) (Mount, bool) {
	fields := strings.Fields(line)
	sep := -1
	for i, f := range fields {
		if f == "-" && i >= 6 {
			sep = i
			break
		}
	}
	if sep < 0 || len(fields) < sep+3 {
		return Mount{}, false
	}
	return Mount{
		MountPoint: unescapeMountinfo(fields[4]),
		FSType:     fields[sep+1],
		Source:     unescapeMountinfo(fields[sep+2]),
	}, true
}

// unescapeMountinfo decodes the octal escapes (\040 and the like) the
// kernel writes for space, tab, newline and backslash.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// NamespaceInode implements Probe from the /proc/self/ns/<kind> link, which
// reads as "<kind>:[<inode>]".
func (System) NamespaceInode(kind string) (uint64, error) {
	link, err := os.Readlink(filepath.Join(procSelf, "ns", kind))
	if err != nil {
		return 0, err
	}
	rest, ok := strings.CutPrefix(link, kind+":[")
	if !ok || !strings.HasSuffix(rest, "]") {
		return 0, fmt.Errorf("unexpected %s namespace link %q", kind, link)
	}
	return strconv.ParseUint(strings.TrimSuffix(rest, "]"), 10, 64)
}

// Writable implements Probe with access(2) and W_OK. A refusal (EACCES,
// EROFS, EPERM) is "not writable"; any other error is reported.
func (System) Writable(path string) (bool, error) {
	err := unix.Access(path, unix.W_OK)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EROFS), errors.Is(err, unix.EPERM):
		return false, nil
	}
	return false, fmt.Errorf("access %s: %w", path, err)
}

// Readable implements Probe by really trying to open path for reading
// (O_RDONLY, O_NOFOLLOW, O_NONBLOCK, never read from): a refusal by
// permission (EACCES, EPERM) is "not readable"; success is "readable"; any
// other error, a missing path included, is reported, because "cannot tell"
// must not pass for "cannot read" (a misspelled key path would otherwise
// pass the check).
func (System) Readable(path string) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		_ = unix.Close(fd)
		return true, nil
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return false, nil
	}
	return false, fmt.Errorf("open %s: %w", path, err)
}

// EUID implements Probe.
func (System) EUID() uint32 {
	return uint32(os.Geteuid()) // #nosec G115 -- a uid is a 32-bit value on Linux
}
