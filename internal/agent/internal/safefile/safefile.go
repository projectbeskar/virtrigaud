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

// Package safefile reads the per-host agent's configuration files safely: it
// opens a file without following a symbolic link, checks what it opened (a
// regular file, its owner, its mode, its size) on the open descriptor, and
// reads at most the allowed size, so that no check is made on one file and
// the read on another.
package safefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
)

// ErrRefused is wrapped by every refusal of a file.
var ErrRefused = errors.New("file refused")

// Policy says what a file must be.
type Policy struct {
	// Owner, when set, is the uid the file must be owned by.
	Owner *uint32
	// Owners, when set, are the uids the file may be owned by (any one of
	// them); Owner, when also set, must match too.
	Owners []uint32
	// DenyMode are permission bits the file must not have (for example
	// 0o022: not writable by group or others).
	DenyMode fs.FileMode
	// Max is the largest size accepted, in bytes. Required.
	Max int64
}

// Read opens path without following a symbolic link, checks the open file
// against p and returns its content.
func Read(path string, p Policy) ([]byte, error) {
	if p.Max <= 0 {
		return nil, fmt.Errorf("read %s: no size bound", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|noFollow, 0) // #nosec G304 -- a path of the agent's own configuration
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrRefused, path)
	}
	if fi.Mode().Perm()&p.DenyMode != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o", ErrRefused, path, fi.Mode().Perm())
	}
	if p.Owner != nil {
		uid, ok := ownerOf(fi)
		if !ok || uid != *p.Owner {
			return nil, fmt.Errorf("%w: %s is not owned by uid %d", ErrRefused, path, *p.Owner)
		}
	}
	if len(p.Owners) > 0 {
		uid, ok := ownerOf(fi)
		if !ok || !slices.Contains(p.Owners, uid) {
			return nil, fmt.Errorf("%w: %s is not owned by an allowed user", ErrRefused, path)
		}
	}
	if fi.Size() > p.Max {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrRefused, path, p.Max)
	}
	b, err := io.ReadAll(io.LimitReader(f, p.Max+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(b)) > p.Max {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrRefused, path, p.Max)
	}
	return b, nil
}
