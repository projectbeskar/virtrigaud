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
	"os"
	"path/filepath"
	"testing"
)

const testMountinfo = `22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
23 22 0:21 / /sys/fs/cgroup rw,nosuid shared:9 - cgroup2 cgroup2 rw
24 22 0:30 / /var/lib rw shared:12 - tmpfs tmpfs rw
25 24 252:2 / /var/lib/virtrigaud\040agent rw shared:13 - xfs /dev/vdb1 rw
26 22 0:31 / /var/lib rw shared:14 - btrfs /dev/vdc1 rw
not a mountinfo line
`

func TestMountOf(t *testing.T) {
	for _, tc := range []struct{ path, mp, fstype, src string }{
		{"/etc/x", "/", "ext4", "/dev/vda1"},
		{"/sys/fs/cgroup", "/sys/fs/cgroup", "cgroup2", "cgroup2"},
		{"/var/lib/virtrigaud agent/state", "/var/lib/virtrigaud agent", "xfs", "/dev/vdb1"},
		// The later mount at the same point covers the earlier one.
		{"/var/lib/other", "/var/lib", "btrfs", "/dev/vdc1"},
		// A sibling with a common prefix is not under the mount point.
		{"/var/library", "/", "ext4", "/dev/vda1"},
	} {
		m, err := mountOf(testMountinfo, tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if m.MountPoint != tc.mp || m.FSType != tc.fstype || m.Source != tc.src {
			t.Errorf("%s: mount = %+v", tc.path, m)
		}
	}
	if _, err := mountOf("", "/x"); err == nil {
		t.Error("empty table: no error")
	}
}

// TestSystemProbe: the real probe reads this process's own namespaces,
// mounts and files without privilege.
func TestSystemProbe(t *testing.T) {
	var p System
	for _, kind := range []string{NamespacePID, NamespaceCgroup, NamespaceUser} {
		if _, err := p.NamespaceInode(kind); err != nil {
			t.Errorf("%s namespace: %v", kind, err)
		}
	}
	dir := t.TempDir()
	if _, err := p.MountOf(dir); err != nil {
		t.Errorf("mount of %s: %v", dir, err)
	}
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w, err := p.Writable(f); err != nil || !w {
		t.Errorf("own file writable = %v, %v", w, err)
	}
	if err := os.Chmod(f, 0o400); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if w, err := p.Writable(f); err != nil || w {
			t.Errorf("read-only file writable = %v, %v", w, err)
		}
	}
	fi, err := p.Lstat(f)
	if err != nil || fi.UID != p.EUID() || fi.Mode.Perm() != 0o400 {
		t.Errorf("lstat = %+v, %v", fi, err)
	}
	if b, err := p.ReadFile(f, 4); err != nil || string(b) != "data" {
		t.Errorf("read = %q, %v", b, err)
	}
	if _, err := p.ReadFile(f, 3); err == nil {
		t.Error("over-limit read: no error")
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadFile(link, 4); err == nil {
		t.Error("read through a symlink: no error")
	}
	if names, err := p.ReadDir(dir); err != nil || len(names) != 2 || names[0] != "f" {
		t.Errorf("readdir = %v, %v", names, err)
	}
}

// TestSystemProbeReadable: the real probe tells a file it can open from one
// refused by permission, and reports a missing path or a symbolic link as an
// error instead of "not readable".
func TestSystemProbeReadable(t *testing.T) {
	var p System
	dir := t.TempDir()
	f := filepath.Join(dir, "k")
	if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := p.Readable(f); err != nil || !r {
		t.Errorf("own file readable = %v, %v", r, err)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(f, 0o000); err != nil {
			t.Fatal(err)
		}
		if r, err := p.Readable(f); err != nil || r {
			t.Errorf("mode 000 file readable = %v, %v", r, err)
		}
		sub := filepath.Join(dir, "closed")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		inner := filepath.Join(sub, "k")
		if err := os.WriteFile(inner, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sub, 0o000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(sub, 0o700) }()
		if r, err := p.Readable(inner); err != nil || r {
			t.Errorf("file in an unsearchable directory readable = %v, %v", r, err)
		}
	}
	if _, err := p.Readable(filepath.Join(dir, "absent")); err == nil {
		t.Error("absent file: no error")
	}
	if _, err := p.Readable(filepath.Join(f, "below")); err == nil {
		t.Error("path below a file: no error")
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Readable(link); err == nil {
		t.Error("symbolic link: no error")
	}
}
