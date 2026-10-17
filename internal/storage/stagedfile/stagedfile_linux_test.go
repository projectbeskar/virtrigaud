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
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	stagedRel     = "vmmigrations/ns/mig/export.qcow2"
	stagedContent = "staged disk bytes"
	secretContent = "provider-credential-password"
)

// fixture is a staging root (the tenant-writable PVC) next to a directory
// standing in for the provider pod's credentials.
type fixture struct {
	root   string
	secret string // a file outside root, standing in for /etc/virtrigaud/credentials/password
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "pvc")
	creds := filepath.Join(base, "credentials")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(creds, 0o700))
	secret := filepath.Join(creds, "password")
	require.NoError(t, os.WriteFile(secret, []byte(secretContent), 0o600))
	return fixture{root: root, secret: secret}
}

func (f fixture) stage(t *testing.T) string {
	t.Helper()
	p := filepath.Join(f.root, stagedRel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(stagedContent), 0o644))
	return p
}

// bothResolvers runs fn with openat2 and with the O_NOFOLLOW walk fallback.
func bothResolvers(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, mode := range []struct {
		name    string
		openat2 bool
	}{{"openat2", true}, {"walk", false}} {
		t.Run(mode.name, func(t *testing.T) {
			prev := useOpenat2
			useOpenat2 = mode.openat2
			t.Cleanup(func() { useOpenat2 = prev })
			fn(t)
		})
	}
}

// assertRefused checks a refusal: it matches ErrUnsafe, its text is constant
// and names neither the staged path nor the secret.
func assertRefused(t *testing.T, err error, f fixture) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, IsUnsafe(err), "refusal must match ErrUnsafe: %v", err)
	assert.Equal(t, ErrUnsafe.Error(), err.Error(), "constant error text")
	assert.NotContains(t, err.Error(), f.root)
	assert.NotContains(t, err.Error(), f.secret)
	assert.NotContains(t, LogDetail(err), secretContent)
}

func readAll(t *testing.T, file *os.File) string {
	t.Helper()
	defer func() { _ = file.Close() }()
	b, err := io.ReadAll(file)
	require.NoError(t, err)
	return string(b)
}

func TestOpenRead_RegularFile(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		f.stage(t)
		file, err := OpenRead(f.root, stagedRel)
		require.NoError(t, err)
		assert.Equal(t, stagedContent, readAll(t, file))
	})
}

func TestOpenRead_SymlinkedFinalComponent(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		p := f.stage(t)
		require.NoError(t, os.Remove(p))
		require.NoError(t, os.Symlink(f.secret, p))
		_, err := OpenRead(f.root, stagedRel)
		assertRefused(t, err, f)
	})
}

func TestOpenRead_SymlinkToEtcFile(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		p := f.stage(t)
		require.NoError(t, os.Remove(p))
		require.NoError(t, os.Symlink("/etc/hostname", p))
		_, err := OpenRead(f.root, stagedRel)
		assertRefused(t, err, f)
	})
}

func TestOpenRead_SymlinkedParentDirectory(t *testing.T) {
	for _, parent := range []string{"vmmigrations", "vmmigrations/ns", "vmmigrations/ns/mig"} {
		t.Run(strings.ReplaceAll(parent, "/", "_"), func(t *testing.T) {
			bothResolvers(t, func(t *testing.T) {
				f := newFixture(t)
				f.stage(t)
				// The tenant replaces a parent directory with a link to a
				// directory holding a file of the staged file's name.
				outside := filepath.Join(filepath.Dir(f.secret), "evil")
				rest, err := filepath.Rel(parent, stagedRel)
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(outside, rest)), 0o755))
				require.NoError(t, os.Symlink(f.secret, filepath.Join(outside, rest)))
				require.NoError(t, os.WriteFile(filepath.Join(outside, rest+".plain"), []byte(secretContent), 0o600))
				require.NoError(t, os.RemoveAll(filepath.Join(f.root, parent)))
				require.NoError(t, os.Symlink(outside, filepath.Join(f.root, parent)))

				_, err = OpenRead(f.root, stagedRel)
				assertRefused(t, err, f)
				_, err = OpenRead(f.root, stagedRel+".plain")
				assertRefused(t, err, f)
			})
		})
	}
}

func TestOpenRead_SymlinkInsideRootIsRefusedToo(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		p := f.stage(t)
		link := filepath.Join(f.root, "vmmigrations/ns/mig/alias.qcow2")
		require.NoError(t, os.Symlink(p, link))
		_, err := OpenRead(f.root, "vmmigrations/ns/mig/alias.qcow2")
		assertRefused(t, err, f)
	})
}

func TestOpenRead_HardLinkRefused(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		p := f.stage(t)
		// A hard link can only name a file on the same filesystem (link(2)
		// cannot cross one, so the pod's credential volumes are unreachable);
		// a staged disk always has a single link, so any other count is refused.
		require.NoError(t, os.Link(p, filepath.Join(f.root, "second-name")))
		_, err := OpenRead(f.root, stagedRel)
		assertRefused(t, err, f)
	})
}

func TestOpenRead_FIFORefusedWithoutBlocking(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		p := f.stage(t)
		require.NoError(t, os.Remove(p))
		require.NoError(t, syscall.Mkfifo(p, 0o644))
		// No writer ever opens the FIFO: a blocking open would hang here.
		_, err := OpenRead(f.root, stagedRel)
		assertRefused(t, err, f)
	})
}

func TestOpenRead_DeviceRefused(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		if _, err := os.Stat("/dev/null"); err != nil {
			t.Skip("no /dev/null")
		}
		_, err := OpenRead("/dev", "null")
		require.Error(t, err)
		assert.True(t, IsUnsafe(err))
	})
}

func TestOpenRead_DirectoryRefused(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		f.stage(t)
		_, err := OpenRead(f.root, "vmmigrations/ns")
		assertRefused(t, err, f)
	})
}

func TestOpenRead_RegularFileAsParentRefused(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		f.stage(t)
		_, err := OpenRead(f.root, stagedRel+"/below")
		assertRefused(t, err, f)
	})
}

func TestOpenRead_BadPaths(t *testing.T) {
	f := newFixture(t)
	f.stage(t)
	for _, rel := range []string{"", ".", "/etc/hostname", "../credentials/password", "vmmigrations/../../credentials/password", "a/.."} {
		_, err := OpenRead(f.root, rel)
		assertRefused(t, err, f)
	}
	_, err := OpenRead("relative-root", stagedRel)
	require.Error(t, err)
}

func TestOpenRead_MissingIsNotExist(t *testing.T) {
	bothResolvers(t, func(t *testing.T) {
		f := newFixture(t)
		_, err := OpenRead(f.root, stagedRel)
		require.Error(t, err)
		assert.False(t, IsUnsafe(err))
		assert.True(t, errors.Is(err, fs.ErrNotExist))
		assert.Equal(t, msgNotFound, err.Error())
		assert.Contains(t, LogDetail(err), stagedRel, "the log detail names the path")
	})
}

func TestCreate_NewFileAndParents(t *testing.T) {
	f := newFixture(t)
	file, err := Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
	require.NoError(t, err)
	_, err = file.WriteString(stagedContent)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	got, err := os.ReadFile(filepath.Join(f.root, stagedRel))
	require.NoError(t, err)
	assert.Equal(t, stagedContent, string(got))
}

func TestCreate_ReplacesEarlierRegularFile(t *testing.T) {
	f := newFixture(t)
	p := f.stage(t)
	// A hard link to the earlier file keeps its content: the name is replaced,
	// the old inode is never written.
	other := filepath.Join(f.root, "other-name")
	require.NoError(t, os.Link(p, other))
	file, err := Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
	require.NoError(t, err)
	_, err = file.WriteString("new")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	got, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	got, err = os.ReadFile(other)
	require.NoError(t, err)
	assert.Equal(t, stagedContent, string(got))
}

func TestCreate_PlantedSymlinkAtNameRefused(t *testing.T) {
	f := newFixture(t)
	p := f.stage(t)
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(f.secret, p))
	_, err := Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
	assertRefused(t, err, f)
	got, err := os.ReadFile(f.secret)
	require.NoError(t, err)
	assert.Equal(t, secretContent, string(got), "the link target is untouched")
	fi, err := os.Lstat(p)
	require.NoError(t, err)
	assert.Equal(t, fs.ModeSymlink, fi.Mode()&fs.ModeType, "the planted link is left alone")
}

func TestCreate_PlantedDanglingSymlinkAtNameRefused(t *testing.T) {
	f := newFixture(t)
	p := f.stage(t)
	require.NoError(t, os.Remove(p))
	target := filepath.Join(filepath.Dir(f.secret), "would-be-created")
	require.NoError(t, os.Symlink(target, p))
	_, err := Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
	assertRefused(t, err, f)
	_, err = os.Lstat(target)
	assert.True(t, os.IsNotExist(err), "nothing is created through the link")
}

func TestCreate_SymlinkedParentRefused(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(filepath.Dir(f.secret), "evil")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.root, "vmmigrations")))
	_, err := Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
	assertRefused(t, err, f)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is created through the link")
}

func TestCreate_FIFOAndDirectoryAtNameRefused(t *testing.T) {
	f := newFixture(t)
	p := f.stage(t)
	require.NoError(t, os.Remove(p))
	require.NoError(t, syscall.Mkfifo(p, 0o644))
	_, err := Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
	assertRefused(t, err, f)

	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Mkdir(p, 0o755))
	_, err = Create(f.root, stagedRel, DefaultDirPerm, DefaultFilePerm)
	assertRefused(t, err, f)
}

func TestRemove(t *testing.T) {
	f := newFixture(t)
	p := f.stage(t)
	require.NoError(t, Remove(f.root, stagedRel))
	_, err := os.Lstat(p)
	assert.True(t, os.IsNotExist(err))

	err = Remove(f.root, stagedRel)
	assert.True(t, errors.Is(err, fs.ErrNotExist))
	assert.False(t, IsUnsafe(err))
}

func TestRemove_SymlinkedParentRefused(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(filepath.Dir(f.secret), "evil")
	rest := "ns/mig/export.qcow2"
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(outside, rest)), 0o755))
	victim := filepath.Join(outside, rest)
	require.NoError(t, os.WriteFile(victim, []byte(secretContent), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.root, "vmmigrations")))
	err := Remove(f.root, stagedRel)
	assertRefused(t, err, f)
	_, err = os.Stat(victim)
	assert.NoError(t, err, "nothing outside the root is removed")
}

func TestRemove_SymlinkAtNameRemovesOnlyTheLink(t *testing.T) {
	f := newFixture(t)
	p := f.stage(t)
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(f.secret, p))
	require.NoError(t, Remove(f.root, stagedRel))
	_, err := os.Lstat(p)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(f.secret)
	assert.NoError(t, err, "the link target is untouched")
}
