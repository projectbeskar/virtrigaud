//go:build unix

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
	"os"
	"path/filepath"
	"syscall"
)

// noFollow refuses to open a symbolic link as the last path component.
const noFollow = syscall.O_NOFOLLOW

// lockRetries bounds the attempts to lock a lock file that is replaced while
// it is being locked.
const lockRetries = 5

// fileLock is the held single-writer lock: an flock on the state directory
// itself and one on the lock file.
type fileLock struct{ dir, f *os.File }

// acquireLock takes the single-writer lock of dir without waiting:
//
//  1. an exclusive flock on the directory itself. A directory that holds
//     files cannot be removed, so deleting or replacing the lock file does not
//     defeat this lock, and a refused Open has created nothing;
//  2. an exclusive flock on the lock file, re-opened when the file at path is
//     no longer the inode that was locked (it was replaced in between), at
//     most lockRetries times.
//
// A lock held by another open file description is ErrLocked.
func acquireLock(path string) (*fileLock, error) {
	dir, err := os.Open(filepath.Dir(path)) // #nosec G304 -- the state directory
	if err != nil {
		return nil, fmt.Errorf("open state directory: %w", err)
	}
	if err := flockNB(dir, path); err != nil {
		_ = dir.Close()
		return nil, err
	}
	for i := 0; i < lockRetries; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600) // #nosec G304 -- the path is built from the state directory
		if err != nil {
			_ = dir.Close()
			return nil, fmt.Errorf("open lock file: %w", err)
		}
		if err := flockNB(f, path); err != nil {
			_ = f.Close()
			_ = dir.Close()
			return nil, err
		}
		same, err := sameInode(f, path)
		if err != nil {
			_ = f.Close()
			_ = dir.Close()
			return nil, err
		}
		if same {
			return &fileLock{dir: dir, f: f}, nil
		}
		_ = f.Close()
	}
	_ = dir.Close()
	return nil, fmt.Errorf("lock %s: the lock file keeps being replaced", path)
}

func flockNB(f *os.File, path string) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor fits in int
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLocked
		}
		return fmt.Errorf("lock %s: %w", path, err)
	}
	return nil
}

// sameInode reports whether the open file f is the file now at path.
func sameInode(f *os.File, path string) (bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat lock file: %w", err)
	}
	pi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat lock file: %w", err)
	}
	return os.SameFile(fi, pi), nil
}

// release unlocks and closes the lock file and the directory. The lock file
// itself stays.
func (l *fileLock) release() error {
	if l == nil {
		return nil
	}
	var errs []error
	for _, f := range []*os.File{l.f, l.dir} {
		if f != nil {
			errs = append(errs, f.Close()) // closing the last descriptor releases the flock
		}
	}
	l.f, l.dir = nil, nil
	return errors.Join(errs...)
}

// unlockFile drops the flock of f explicitly. Closing the descriptor is not
// enough when a memory mapping of the file outlives it: the mapping keeps the
// open file description, and with it the lock, alive.
func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor fits in int
}
