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
	"syscall"
)

// noFollow refuses to open a symbolic link as the last path component.
const noFollow = syscall.O_NOFOLLOW

// fileLock is the held single-writer lock.
type fileLock struct{ f *os.File }

// acquireLock creates (if needed) and locks path exclusively, without
// waiting. A lock held by another open file description is ErrLocked.
func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600) // #nosec G304 -- the path is built from the state directory
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &fileLock{f: f}, nil
}

// release unlocks and closes the lock file. The file itself stays.
func (l *fileLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close() // closing the last descriptor releases the flock
	l.f = nil
	return err
}
