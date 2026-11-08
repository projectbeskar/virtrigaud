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

package journal_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
)

// storeFD returns this process's descriptor of path, found in /proc/self/fd.
func storeFD(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	want, _ := filepath.EvalSymlinks(path)
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil || target != want {
			continue
		}
		var fd int
		for _, c := range e.Name() {
			fd = fd*10 + int(c-'0')
		}
		return fd
	}
	t.Fatalf("no descriptor for %s", path)
	return -1
}

// A real fdatasync failure inside bbolt's commit. The store file's descriptor
// is replaced by /dev/null: bbolt's page writes succeed and its fdatasync
// fails with EINVAL, which bbolt v1.4.3 returns as a bare syscall.Errno
// (pinned here). The journal must quarantine the store and commit nothing
// more, and the quarantine must survive a reopen.
func TestCommit_BoltFdatasyncFailureQuarantines(t *testing.T) {
	e := newEnv(t)
	s := e.ready()
	put(t, s, "a", "1")
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	fd := storeFD(t, filepath.Join(e.dir, journal.StoreFile))
	if err := unix.Dup2(int(devnull.Fd()), fd); err != nil { // #nosec G115 -- a file descriptor fits in int
		t.Fatal(err)
	}
	_, err = s.Update(context.Background(), func(tx *journal.Tx) error {
		return tx.Put(testBucket, []byte("b"), []byte("2"))
	})
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		t.Fatalf("err = %v: bbolt no longer returns the fdatasync errno", err)
	}
	var pe *fs.PathError
	var se *os.SyscallError
	if errors.As(err, &pe) || errors.As(err, &se) {
		t.Fatalf("err = %v: bbolt now wraps the fdatasync errno; re-check the commit-error classification", err)
	}
	if !errors.Is(err, journal.ErrLost) {
		t.Fatalf("err = %v: a failed bbolt fdatasync did not quarantine the store", err)
	}
	wantLost(t, s, journal.LostIOError)
	e.powerCut(s)
	wantLost(t, e.open(), journal.LostIOError)
}
