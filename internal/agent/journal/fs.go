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
	"fmt"
	"io"
	"io/fs"
	"os"
)

// File is an open file of an FS.
type File interface {
	io.Reader
	io.Writer
	// Sync flushes the file's data to stable storage.
	Sync() error
	// Truncate changes the file's size.
	Truncate(size int64) error
	// Close closes the file. It does not imply Sync.
	Close() error
}

// FS is the file-system interface the journal and the operation store use for
// every file that is not the bbolt store itself: the anchor log, the store
// and quarantine markers, and witness files. Tests substitute an
// implementation that injects write, fsync, rename and O_EXCL faults and that
// drops unsynchronised data on a simulated crash.
type FS interface {
	// OpenFile opens a file as os.OpenFile does. With os.O_CREATE|os.O_EXCL it
	// fails with an error satisfying errors.Is(err, fs.ErrExist) when the file
	// exists.
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	// ReadFile returns the whole content of a file.
	ReadFile(name string) ([]byte, error)
	// Exists reports whether a file exists.
	Exists(name string) (bool, error)
	// Remove removes a file.
	Remove(name string) error
	// RemoveAll removes a directory tree. A missing path is not an error.
	RemoveAll(path string) error
	// Rename renames a file, replacing the target.
	Rename(oldpath, newpath string) error
	// MkdirAll creates a directory and its parents.
	MkdirAll(path string, perm fs.FileMode) error
	// SyncDir fsyncs a directory, making the creation, removal and renaming
	// of its entries durable.
	SyncDir(path string) error
}

// OSFS is the FS of the operating system.
type OSFS struct{}

var _ FS = OSFS{}

// OpenFile implements FS.
func (OSFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	f, err := os.OpenFile(name, flag, perm) // #nosec G304 -- the path is built by the journal from its own directory
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ReadFile implements FS.
func (OSFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name) // #nosec G304 -- the path is built by the journal from its own directory
}

// Exists implements FS.
func (OSFS) Exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Remove implements FS.
func (OSFS) Remove(name string) error { return os.Remove(name) }

// RemoveAll implements FS.
func (OSFS) RemoveAll(path string) error { return os.RemoveAll(path) }

// Rename implements FS.
func (OSFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// MkdirAll implements FS.
func (OSFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

// SyncDir implements FS.
func (OSFS) SyncDir(path string) error {
	d, err := os.Open(path) // #nosec G304 -- the path is built by the journal from its own directory
	if err != nil {
		return fmt.Errorf("open directory %s: %w", path, err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return fmt.Errorf("fsync directory %s: %w", path, serr)
	}
	if cerr != nil {
		return fmt.Errorf("close directory %s: %w", path, cerr)
	}
	return nil
}

// WriteFileDurable creates or replaces name with data and makes both the
// content and the directory entry durable: write, fsync the file, close,
// fsync the directory.
func WriteFileDurable(fsys FS, dir, name string, data []byte) error {
	f, err := fsys.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := fsys.SyncDir(dir); err != nil {
		return err
	}
	return nil
}
