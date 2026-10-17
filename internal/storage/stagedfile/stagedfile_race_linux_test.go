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

package stagedfile

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// swapDuration is how long each concurrent-swap test lets the tenant race.
const swapDuration = 500 * time.Millisecond

// withOpenat2 replaces the openat2 system call for one test.
func withOpenat2(t *testing.T, fn func(dirfd int, path string, how *unix.OpenHow) (int, error)) {
	t.Helper()
	prev := openat2Fn
	openat2Fn = fn
	t.Cleanup(func() { openat2Fn = prev })
}

// TestOpenRead_FallsBackOnENOSYSAndEPERM: an openat2 the kernel lacks
// (ENOSYS) or a seccomp profile denies (EPERM) falls back to the O_NOFOLLOW
// walk, which is just as strict.
func TestOpenRead_FallsBackOnENOSYSAndEPERM(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ENOSYS, unix.EPERM} {
		t.Run(errno.Error(), func(t *testing.T) {
			calls := 0
			withOpenat2(t, func(int, string, *unix.OpenHow) (int, error) {
				calls++
				return -1, errno
			})
			f := newFixture(t)
			p := f.stage(t)

			file, err := OpenRead(f.root, stagedRel)
			require.NoError(t, err, "the fallback walk opens a regular file")
			assert.Equal(t, stagedContent, readAll(t, file))
			assert.Equal(t, 1, calls, "openat2 was tried first, once")

			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink(f.secret, p))
			_, err = OpenRead(f.root, stagedRel)
			assertRefused(t, err, f)

			parent := filepath.Join(f.root, "vmmigrations")
			require.NoError(t, os.RemoveAll(parent))
			require.NoError(t, os.Symlink(filepath.Dir(f.secret), parent))
			_, err = OpenRead(f.root, stagedRel)
			assertRefused(t, err, f)
		})
	}
}

// TestOpenRead_RetriesEAGAINThenFailsClosed: EAGAIN (a racing rename or mount
// under RESOLVE_BENEATH) is retried; a persistent EAGAIN fails the open
// without falling back.
func TestOpenRead_RetriesEAGAINThenFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.stage(t)

	calls := 0
	withOpenat2(t, func(dirfd int, path string, how *unix.OpenHow) (int, error) {
		calls++
		if calls < 3 {
			return -1, unix.EAGAIN
		}
		return unix.Openat2(dirfd, path, how)
	})
	file, err := OpenRead(f.root, stagedRel)
	require.NoError(t, err)
	assert.Equal(t, stagedContent, readAll(t, file))
	assert.Equal(t, 3, calls)

	calls = 0
	withOpenat2(t, func(int, string, *unix.OpenHow) (int, error) {
		calls++
		return -1, unix.EAGAIN
	})
	_, err = OpenRead(f.root, stagedRel)
	require.Error(t, err)
	assert.True(t, errors.Is(err, unix.EAGAIN))
	assert.False(t, IsUnsafe(err))
	assert.Equal(t, msgOpenFailed, err.Error())
	assert.Equal(t, openat2Attempts, calls, "bounded retries, no fallback")
}

// TestOpenRead_MissingMountIsNamedInDetail: a missing staging root is a
// not-found error whose log detail says the mount is missing.
func TestOpenRead_MissingMountIsNamedInDetail(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-mounted")
	_, err := OpenRead(root, stagedRel)
	require.Error(t, err)
	assert.True(t, errors.Is(err, fs.ErrNotExist))
	assert.Equal(t, msgNotFound, err.Error())
	assert.Contains(t, LogDetail(err), "staging mount")
	assert.Contains(t, LogDetail(err), "is missing")
}

// tenantSwapper races the tenant against the provider: until ctx ends it
// replaces the migration directory, and the staged file inside it, with
// symbolic links into the credential directory and back.
func tenantSwapper(ctx context.Context, t *testing.T, f fixture, wg *sync.WaitGroup) {
	t.Helper()
	mig := filepath.Join(f.root, "vmmigrations", "ns", "mig")
	migReal := mig + ".real"
	staged := filepath.Join(mig, "export.qcow2")
	creds := filepath.Dir(f.secret)
	// The credential directory holds a file of the staged file's name.
	require.NoError(t, os.WriteFile(filepath.Join(creds, "export.qcow2"), []byte(secretContent), 0o600))
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			if i%2 == 0 {
				// Parent directory swap.
				if os.Rename(mig, migReal) == nil {
					_ = os.Symlink(creds, mig)
					_ = os.Remove(mig)
					_ = os.Rename(migReal, mig)
				}
				continue
			}
			// Final component swap.
			if os.Rename(staged, staged+".real") == nil {
				_ = os.Symlink(f.secret, staged)
				_ = os.Remove(staged)
				_ = os.Rename(staged+".real", staged)
			}
		}
	}()
}

// TestOpenRead_ConcurrentSwapNeverReadsTheSecret: while the tenant swaps the
// parent directory and the staged file for links to the credentials, every
// read either fails or returns the staged bytes.
func TestOpenRead_ConcurrentSwapNeverReadsTheSecret(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		f.stage(t)
		ctx, cancel := context.WithTimeout(context.Background(), swapDuration)
		defer cancel()
		var wg sync.WaitGroup
		tenantSwapper(ctx, t, f, &wg)

		reads := 0
		for ctx.Err() == nil {
			file, err := OpenRead(f.root, stagedRel)
			if err != nil {
				continue
			}
			b, err := io.ReadAll(file)
			_ = file.Close()
			require.NoError(t, err)
			require.Equal(t, stagedContent, string(b), "only the staged bytes are ever read")
			reads++
		}
		cancel()
		wg.Wait()
		t.Logf("%d successful reads during the swap", reads)
	})
}

// TestCreate_ConcurrentSwapNeverWritesOutsideTheRoot: while the tenant swaps
// the parent directory and the staged file for links to the credentials,
// Create never writes through a link: the credential files keep their content
// and no file appears in their directory.
func TestCreate_ConcurrentSwapNeverWritesOutsideTheRoot(t *testing.T) {
	f := newFixture(t)
	f.stage(t)
	creds := filepath.Dir(f.secret)
	ctx, cancel := context.WithTimeout(context.Background(), swapDuration)
	defer cancel()
	var wg sync.WaitGroup
	tenantSwapper(ctx, t, f, &wg)

	for ctx.Err() == nil {
		file, err := Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
		if err != nil {
			continue
		}
		_, werr := file.WriteString("payload")
		_ = file.Close()
		require.NoError(t, werr)
	}
	cancel()
	wg.Wait()

	got, err := os.ReadFile(f.secret)
	require.NoError(t, err)
	assert.Equal(t, secretContent, string(got))
	got, err = os.ReadFile(filepath.Join(creds, "export.qcow2"))
	require.NoError(t, err)
	assert.Equal(t, secretContent, string(got))
	entries, err := os.ReadDir(creds)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "nothing new in the credential directory")
}
