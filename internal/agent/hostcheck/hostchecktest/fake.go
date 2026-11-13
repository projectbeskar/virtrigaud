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

// Package hostchecktest provides a fake host for the agent's start-up checks.
package hostchecktest

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"

	"github.com/projectbeskar/virtrigaud/internal/agent/hostcheck"
)

// Initial namespace inode numbers of a healthy host.
const (
	InitPID    uint64 = 0xEFFFFFFC
	InitCgroup uint64 = 0xEFFFFFFB
	InitUser   uint64 = 0xEFFFFFFD
	InitTime   uint64 = 0xEFFFFFFA
)

// TrustFiles are the entries a healthy fake trust directory holds.
var TrustFiles = []string{"grant-anchors.pem", "host-ca.pem", "provider-ca.pem", "provider-sans"}

// PKIFiles are the entries a healthy fake key material directory holds.
var PKIFiles = []string{"host.crt"}

// Fake is a hostcheck.Probe over a fixed, editable description of a host.
// The zero value is not usable; use Healthy.
type Fake struct {
	mu sync.Mutex
	// Files describes every path the probe knows.
	Files map[string]hostcheck.FileInfo
	// Content holds the content of files that are read.
	Content map[string][]byte
	// Mounts are the host's mounts.
	Mounts []hostcheck.Mount
	// Namespaces maps a namespace kind to its inode; a missing kind does
	// not exist.
	Namespaces map[string]uint64
	// WritablePaths are the paths the process may write.
	WritablePaths map[string]bool
	// ReadablePaths are the paths the process may read; DeniedPaths are the
	// paths whose open is refused by permission; any other path does not
	// exist (an error).
	ReadablePaths map[string]bool
	// DeniedPaths are the paths the process is refused by permission.
	DeniedPaths map[string]bool
	// UID is the process's effective uid.
	UID uint32
}

var _ hostcheck.Probe = (*Fake)(nil)

// Healthy returns a Fake on which every check of cfg passes, with the
// provisioning marker naming hostURI.
func Healthy(cfg hostcheck.Config, hostURI string) *Fake {
	f := &Fake{
		Files:         map[string]hostcheck.FileInfo{},
		Content:       map[string][]byte{},
		Namespaces:    map[string]uint64{"pid": InitPID, "cgroup": InitCgroup, "user": InitUser, "time": InitTime},
		WritablePaths: map[string]bool{},
		ReadablePaths: map[string]bool{},
		DeniedPaths:   map[string]bool{},
		UID:           cfg.UID,
		Mounts: []hostcheck.Mount{
			{MountPoint: "/", FSType: "ext4", Source: "/dev/vda1"},
			{MountPoint: hostcheck.CgroupRoot, FSType: "cgroup2", Source: "cgroup2"},
		},
	}
	for d := cfg.TrustDir; ; d = filepath.Dir(d) {
		f.Files[d] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o755}
		if d == "/" {
			break
		}
	}
	for _, n := range TrustFiles {
		f.Files[filepath.Join(cfg.TrustDir, n)] = hostcheck.FileInfo{Mode: 0o644}
	}
	f.Files[cfg.StateDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o700, UID: cfg.UID}
	f.Files[cfg.PKIDir] = hostcheck.FileInfo{Mode: fs.ModeDir | 0o700, UID: cfg.UID}
	for _, n := range PKIFiles {
		f.Files[filepath.Join(cfg.PKIDir, n)] = hostcheck.FileInfo{Mode: 0o600, UID: cfg.UID}
	}
	if cfg.SignerKeyFile != "" {
		f.DeniedPaths[cfg.SignerKeyFile] = true
	}
	if cfg.SignerSocketDir != "" {
		f.Files[cfg.SignerSocketDir] = hostcheck.FileInfo{Mode: fs.ModeDir | fs.ModeSetgid | 0o750, UID: cfg.SignerUID}
	}
	marker := filepath.Join(cfg.StateDir, hostcheck.HostMarkerFile)
	f.Files[marker] = hostcheck.FileInfo{Mode: 0o600, UID: cfg.UID}
	f.Content[marker] = []byte(hostURI + "\n")
	return f
}

// Edit runs fn with the Fake locked.
func (f *Fake) Edit(fn func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Lstat implements hostcheck.Probe.
func (f *Fake) Lstat(path string) (hostcheck.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, ok := f.Files[path]
	if !ok {
		return hostcheck.FileInfo{}, &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrNotExist}
	}
	return fi, nil
}

// ReadDir implements hostcheck.Probe: the known paths directly under path.
func (f *Fake) ReadDir(path string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fi, ok := f.Files[path]; !ok || !fi.Mode.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: fs.ErrNotExist}
	}
	var names []string
	for p := range f.Files {
		if p != path && filepath.Dir(p) == path {
			names = append(names, filepath.Base(p))
		}
	}
	sort.Strings(names)
	return names, nil
}

// ReadFile implements hostcheck.Probe.
func (f *Fake) ReadFile(path string, limit int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.Content[path]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: path, Err: fs.ErrNotExist}
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return append([]byte(nil), b...), nil
}

// MountOf implements hostcheck.Probe: the mount with the longest mount point
// that contains path.
func (f *Fake) MountOf(path string) (hostcheck.Mount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best hostcheck.Mount
	found := false
	for _, m := range f.Mounts {
		in := m.MountPoint == "/" || path == m.MountPoint || len(path) > len(m.MountPoint) && path[:len(m.MountPoint)+1] == m.MountPoint+"/"
		if in && (!found || len(m.MountPoint) >= len(best.MountPoint)) {
			best, found = m, true
		}
	}
	if !found {
		return hostcheck.Mount{}, fmt.Errorf("no mount holds %s", path)
	}
	return best, nil
}

// NamespaceInode implements hostcheck.Probe.
func (f *Fake) NamespaceInode(kind string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ino, ok := f.Namespaces[kind]
	if !ok {
		return 0, &fs.PathError{Op: "readlink", Path: kind, Err: fs.ErrNotExist}
	}
	return ino, nil
}

// Writable implements hostcheck.Probe.
func (f *Fake) Writable(path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.WritablePaths[path], nil
}

// Readable implements hostcheck.Probe.
func (f *Fake) Readable(path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ReadablePaths[path] {
		return true, nil
	}
	if f.DeniedPaths[path] {
		return false, nil
	}
	return false, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

// EUID implements hostcheck.Probe.
func (f *Fake) EUID() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.UID
}
