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

// Package journaltest provides a fault-injecting, crash-simulating in-memory
// implementation of journal.FS for the tests of the per-host agent's
// operation store. It is test support only; no binary imports it.
//
// The model: every file has a process view (what reads see) and a durable
// view (what survives a crash). File content becomes durable on Sync; the
// creation, removal and renaming of a directory entry become durable on
// SyncDir of its parent directory. Crash discards everything that is not
// durable. Directories themselves are implicit and always durable.
package journaltest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// Op names a file operation a fault can target.
type Op string

// The operations a fault can target.
const (
	OpWrite   Op = "write"
	OpSync    Op = "sync"
	OpSyncDir Op = "syncdir"
	OpRename  Op = "rename"
	OpCreate  Op = "create"
)

// ErrInjected is the error an injected fault returns.
var ErrInjected = errors.New("injected I/O error")

type inode struct {
	data   []byte
	synced []byte
}

// MemFS is the in-memory FS. It is safe for concurrent use.
type MemFS struct {
	mu      sync.Mutex
	names   map[string]*inode // process view
	durable map[string]*inode // durable directory entries
	faults  []fault
	syncs   map[string]int
}

type fault struct {
	op    Op
	match func(path string) bool
	left  int // remaining hits before it fires; fires once
	err   error
}

var _ journal.FS = (*MemFS)(nil)

// NewMemFS returns an empty MemFS.
func NewMemFS() *MemFS {
	return &MemFS{names: map[string]*inode{}, durable: map[string]*inode{}, syncs: map[string]int{}}
}

// Fail makes the next (skip+1)-th operation op on a path for which match is
// true fail with ErrInjected, once.
func (m *MemFS) Fail(op Op, match func(path string) bool, skip int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults = append(m.faults, fault{op: op, match: match, left: skip, err: ErrInjected})
}

// HasSuffix returns a matcher for paths ending in suffix.
func HasSuffix(suffix string) func(string) bool {
	return func(p string) bool { return strings.HasSuffix(p, suffix) }
}

// Contains returns a matcher for paths containing sub.
func Contains(sub string) func(string) bool {
	return func(p string) bool { return strings.Contains(p, sub) }
}

// injected returns the fault for op on path, if one fires. The caller holds
// m.mu.
func (m *MemFS) injected(op Op, path string) error {
	for i := range m.faults {
		f := &m.faults[i]
		if f.op != op || f.err == nil || !f.match(path) {
			continue
		}
		if f.left > 0 {
			f.left--
			continue
		}
		err := f.err
		f.err = nil
		return fmt.Errorf("%s %s: %w", op, path, err)
	}
	return nil
}

// Syncs returns how many times files matching match were fsynced.
func (m *MemFS) Syncs(match func(string) bool) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for p, c := range m.syncs {
		if match(p) {
			n += c
		}
	}
	return n
}

// Crash simulates a power cut: every file reverts to its durable content and
// every directory entry to its durable state. Open file handles stay usable
// only for Close.
func (m *MemFS) Crash() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names = map[string]*inode{}
	for p, n := range m.durable {
		n.data = append([]byte(nil), n.synced...)
		m.names[p] = n
	}
	m.faults = nil
}

// Snapshot returns a copy of the process view of every file.
func (m *MemFS) Snapshot() map[string][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]byte{}
	for p, n := range m.names {
		out[p] = append([]byte(nil), n.data...)
	}
	return out
}

// Restore replaces every file with snap, durably.
func (m *MemFS) Restore(snap map[string][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names, m.durable = map[string]*inode{}, map[string]*inode{}
	for p, d := range snap {
		n := &inode{data: append([]byte(nil), d...), synced: append([]byte(nil), d...)}
		m.names[p], m.durable[p] = n, n
	}
}

// Put writes a file durably (test setup).
func (m *MemFS) Put(name string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := &inode{data: append([]byte(nil), data...), synced: append([]byte(nil), data...)}
	m.names[filepath.Clean(name)], m.durable[filepath.Clean(name)] = n, n
}

// OpenFile implements journal.FS.
func (m *MemFS) OpenFile(name string, flag int, _ fs.FileMode) (journal.File, error) {
	name = filepath.Clean(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.names[name]
	switch {
	case ok && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
	case !ok && flag&os.O_CREATE == 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	case !ok:
		if err := m.injected(OpCreate, name); err != nil {
			return nil, err
		}
		n = &inode{}
		m.names[name] = n
	}
	if flag&os.O_TRUNC != 0 {
		n.data = nil
	}
	return &memFile{fs: m, name: name, n: n, append: flag&os.O_APPEND != 0}, nil
}

// ReadFile implements journal.FS.
func (m *MemFS) ReadFile(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.names[filepath.Clean(name)]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), n.data...), nil
}

// Exists implements journal.FS.
func (m *MemFS) Exists(name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.names[filepath.Clean(name)]
	return ok, nil
}

// Remove implements journal.FS.
func (m *MemFS) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	name = filepath.Clean(name)
	if _, ok := m.names[name]; !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	delete(m.names, name)
	return nil
}

// RemoveAll implements journal.FS.
func (m *MemFS) RemoveAll(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = filepath.Clean(path)
	for p := range m.names {
		if p == path || strings.HasPrefix(p, path+string(filepath.Separator)) {
			delete(m.names, p)
		}
	}
	return nil
}

// Rename implements journal.FS.
func (m *MemFS) Rename(oldpath, newpath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	oldpath, newpath = filepath.Clean(oldpath), filepath.Clean(newpath)
	if err := m.injected(OpRename, oldpath); err != nil {
		return err
	}
	n, ok := m.names[oldpath]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	delete(m.names, oldpath)
	m.names[newpath] = n
	return nil
}

// ReadDir implements journal.FS: the first path components below name of
// every file under it, sorted and without duplicates.
func (m *MemFS) ReadDir(name string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name = filepath.Clean(name) + string(filepath.Separator)
	seen := map[string]bool{}
	var out []string
	for p := range m.names {
		if !strings.HasPrefix(p, name) {
			continue
		}
		first := strings.SplitN(p[len(name):], string(filepath.Separator), 2)[0]
		if !seen[first] {
			seen[first] = true
			out = append(out, first)
		}
	}
	sort.Strings(out)
	return out, nil
}

// MkdirAll implements journal.FS. Directories are implicit.
func (m *MemFS) MkdirAll(string, fs.FileMode) error { return nil }

// SyncDir implements journal.FS: the entries directly in path become
// durable as they are in the process view.
func (m *MemFS) SyncDir(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = filepath.Clean(path)
	if err := m.injected(OpSyncDir, path); err != nil {
		return err
	}
	for p := range m.durable {
		if filepath.Dir(p) == path {
			if _, ok := m.names[p]; !ok {
				delete(m.durable, p)
			}
		}
	}
	for p, n := range m.names {
		if filepath.Dir(p) == path {
			m.durable[p] = n
		}
	}
	return nil
}

type memFile struct {
	fs     *MemFS
	name   string
	n      *inode
	append bool
	off    int
	closed bool
}

func (f *memFile) Read(p []byte) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.off >= len(f.n.data) {
		return 0, io.EOF
	}
	k := copy(p, f.n.data[f.off:])
	f.off += k
	return k, nil
}

func (f *memFile) Write(p []byte) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if err := f.fs.injected(OpWrite, f.name); err != nil {
		return 0, err
	}
	if f.append {
		f.off = len(f.n.data)
	}
	end := f.off + len(p)
	if end > len(f.n.data) {
		f.n.data = append(f.n.data, make([]byte, end-len(f.n.data))...)
	}
	copy(f.n.data[f.off:], p)
	f.off = end
	return len(p), nil
}

func (f *memFile) Sync() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.fs.injected(OpSync, f.name); err != nil {
		return err
	}
	f.n.synced = append([]byte(nil), f.n.data...)
	f.fs.syncs[f.name]++
	return nil
}

func (f *memFile) Truncate(size int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if size < int64(len(f.n.data)) {
		f.n.data = f.n.data[:size]
	}
	return nil
}

func (f *memFile) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	f.closed = true
	return nil
}

// Stat implements journal.File: a regular file of the current size.
func (f *memFile) Stat() (fs.FileInfo, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	return memInfo{name: filepath.Base(f.name), size: int64(len(f.n.data))}, nil
}

// memInfo is the fs.FileInfo of a memFile.
type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o600 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
