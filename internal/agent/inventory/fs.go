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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// ErrNotRegular is returned by FS.Open for anything but a regular file.
var ErrNotRegular = errors.New("not a regular file")

// DirEntry is one entry of a directory.
type DirEntry struct {
	// Name is the entry's name.
	Name string
	// Mode holds the entry's type bits (fs.ModeDir, fs.ModeSymlink, ...).
	Mode fs.FileMode
}

// File is an open regular file, read as data.
type File interface {
	io.Reader
	io.ReaderAt
	io.Closer
	// Size is the file's size when it was opened.
	Size() int64
}

// FS is the collector's only way to read the host. It never executes
// anything, and every read the collector makes is bounded by its caller.
type FS interface {
	// ReadDir returns at most limit entries of the directory at path, sorted
	// by name, and whether more existed.
	ReadDir(path string, limit int) (entries []DirEntry, more bool, err error)
	// Lstat returns the type bits and size of path without following a
	// symbolic link.
	Lstat(path string) (mode fs.FileMode, size int64, err error)
	// Open opens a regular file for reading. It follows a symbolic link only
	// when follow is set, and returns ErrNotRegular for anything else than a
	// regular file.
	Open(path string, follow bool) (File, error)
}

// OSFS is the FS of the running host. Root, when set, is a directory that
// every absolute path is taken relative to (development and tests); it is not
// a security boundary.
type OSFS struct {
	// Root prefixes every path; empty for the host's own file system.
	Root string
}

var _ FS = OSFS{}

// resolve returns the host path of path.
func (o OSFS) resolve(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	if o.Root == "" {
		return path, nil
	}
	return filepath.Join(o.Root, path), nil
}

// ReadDir implements FS.
func (o OSFS) ReadDir(path string, limit int) ([]DirEntry, bool, error) {
	p, err := o.resolve(path)
	if err != nil {
		return nil, false, err
	}
	d, err := os.Open(p) // #nosec G304 -- a directory of the agent's own catalog
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = d.Close() }()
	list, err := d.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	more := len(list) > limit
	if more {
		list = list[:limit]
	}
	out := make([]DirEntry, 0, len(list))
	for _, e := range list {
		out = append(out, DirEntry{Name: e.Name(), Mode: e.Type()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, more, nil
}

// Lstat implements FS.
func (o OSFS) Lstat(path string) (fs.FileMode, int64, error) {
	p, err := o.resolve(path)
	if err != nil {
		return 0, 0, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, 0, err
	}
	return fi.Mode(), fi.Size(), nil
}

// osFile is a regular file opened by OSFS.
type osFile struct {
	*os.File
	size int64
}

// Size implements File.
func (f osFile) Size() int64 { return f.size }

// Open implements FS.
func (o OSFS) Open(path string, follow bool) (File, error) {
	p, err := o.resolve(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, openFlags(follow), 0) // #nosec G304 -- a path of the agent's own catalog
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("open %s: %w", filepath.Base(p), ErrNotRegular)
	}
	return osFile{File: f, size: fi.Size()}, nil
}

// readLimited reads at most limit bytes of f and reports a larger file as
// tooLarge.
func readLimited(f File, limit int64) (data []byte, tooLarge bool, err error) {
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > limit {
		return nil, true, nil
	}
	return b, false, nil
}

// isNotExist reports whether err means the path is absent (a missing path or
// a component that is not a directory).
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
