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

package signer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyDir returns a private directory owned by the test user.
func keyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "keys")
	require.NoError(t, os.Mkdir(dir, 0o700))
	return dir
}

func TestCreateKeyWritesAPrivateKeyOnce(t *testing.T) {
	dir := keyDir(t)
	path := filepath.Join(dir, "host.key")
	uid := EUID()

	k, created, err := CreateKey(path, uid)
	require.NoError(t, err)
	require.True(t, created)
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "the key is private to its owner")
	loaded, err := LoadKey(path, uid)
	require.NoError(t, err, "the signer can load what enrolment wrote")
	assert.True(t, loaded.Equal(k))

	// A second run never replaces the key.
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	k2, created, err := CreateKey(path, uid)
	require.NoError(t, err)
	assert.False(t, created)
	assert.True(t, k2.Equal(k), "the existing key is returned unchanged")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the key file is unchanged")
	onDisk, err := LoadKey(path, uid)
	require.NoError(t, err)
	assert.True(t, onDisk.PublicKey.Equal(&k.PublicKey), "the key on disk is still the first one")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file is left behind")
}

func TestCreateKeyRefusals(t *testing.T) {
	uid := EUID()
	t.Run("group-readable directory", func(t *testing.T) {
		dir := keyDir(t)
		require.NoError(t, os.Chmod(dir, 0o750))
		_, _, err := CreateKey(filepath.Join(dir, "host.key"), uid)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "grant nothing")
	})
	t.Run("directory of another owner", func(t *testing.T) {
		_, _, err := CreateKey(filepath.Join(keyDir(t), "host.key"), uid+1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not owned")
	})
	t.Run("relative path", func(t *testing.T) {
		_, _, err := CreateKey("host.key", uid)
		require.Error(t, err)
	})
	t.Run("existing key readable by others is not adopted", func(t *testing.T) {
		dir := keyDir(t)
		path := filepath.Join(dir, "host.key")
		_, _, err := CreateKey(path, uid)
		require.NoError(t, err)
		require.NoError(t, os.Chmod(path, 0o644))
		_, _, err = CreateKey(path, uid)
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "read the host key"))
	})
	t.Run("symbolic link as the key path", func(t *testing.T) {
		dir := keyDir(t)
		other := filepath.Join(t.TempDir(), "other.key")
		require.NoError(t, os.WriteFile(other, []byte("x"), 0o600))
		path := filepath.Join(dir, "host.key")
		require.NoError(t, os.Symlink(other, path))
		_, _, err := CreateKey(path, uid)
		require.Error(t, err, "a link is never followed and never replaced")
		got, rerr := os.ReadFile(other)
		require.NoError(t, rerr)
		assert.Equal(t, "x", string(got))
	})
}

func TestCreateKeyRemovesStrayTemporaryFiles(t *testing.T) {
	dir := keyDir(t)
	stray := filepath.Join(dir, ".host.key.deadbeef")
	require.NoError(t, os.WriteFile(stray, []byte("left by a killed run"), 0o600))
	other := filepath.Join(dir, "unrelated")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o600))
	_, created, err := CreateKey(filepath.Join(dir, "host.key"), EUID())
	require.NoError(t, err)
	require.True(t, created)
	_, err = os.Lstat(stray)
	assert.True(t, os.IsNotExist(err), "the stray key material is gone")
	_, err = os.Lstat(other)
	assert.NoError(t, err, "other files are left alone")
}
