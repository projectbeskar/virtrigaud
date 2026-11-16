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

package inventory

import (
	"errors"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

// TestPidfsHandle_FilteredCallIsNotSupported: on a kernel where a file handle
// of a process file descriptor can be taken, open_by_handle_at refused by
// the unit's system call filter (EPERM) or by anything else means "not
// supported", never an error: the probe reports the feature absent and the
// collection goes on. The same holds for a refused name_to_handle_at.
func TestPidfsHandle_FilteredCallIsNotSupported(t *testing.T) {
	if _, ok, err := openPidfdSelf(); err != nil || !ok {
		t.Skip("pidfd_open is not available here")
	}
	origName, origOpen := nameToHandleAt, openByHandleAt
	t.Cleanup(func() { nameToHandleAt, openByHandleAt = origName, origOpen })
	nameToHandleAt = func(int, string, int) (unix.FileHandle, int, error) {
		return unix.NewFileHandle(1, []byte{1, 2, 3, 4, 5, 6, 7, 8}), 0, nil
	}
	for _, refusal := range []error{syscall.EPERM, syscall.EACCES, syscall.ENOSYS, errors.New("other")} {
		openByHandleAt = func(int, unix.FileHandle, int) (int, error) { return -1, refusal }
		ok, err := SystemHost{}.PidfsHandle()
		assert.NoError(t, err, "%v", refusal)
		assert.False(t, ok, "%v", refusal)
	}
	nameToHandleAt = func(int, string, int) (unix.FileHandle, int, error) {
		return unix.FileHandle{}, 0, syscall.EPERM
	}
	ok, err := SystemHost{}.PidfsHandle()
	assert.NoError(t, err)
	assert.False(t, ok)
}
