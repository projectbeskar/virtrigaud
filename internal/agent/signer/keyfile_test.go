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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeKeyFile(t *testing.T, dir string, k *ecdsa.PrivateKey, mode fs.FileMode) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	path := filepath.Join(dir, "host.key")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestLoadKey(t *testing.T) {
	k := newKey(t)
	k384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, dir string) (path string, uid uint32)
		ok   bool
	}{
		{"private key owned by the signer", func(t *testing.T, dir string) (string, uint32) {
			return writeKeyFile(t, dir, k, 0o600), EUID()
		}, true},
		{"readable by group", func(t *testing.T, dir string) (string, uint32) {
			return writeKeyFile(t, dir, k, 0o640), EUID()
		}, false},
		{"readable by others", func(t *testing.T, dir string) (string, uint32) {
			return writeKeyFile(t, dir, k, 0o604), EUID()
		}, false},
		{"owned by another user", func(t *testing.T, dir string) (string, uint32) {
			return writeKeyFile(t, dir, k, 0o600), EUID() + 1
		}, false},
		{"symbolic link", func(t *testing.T, dir string) (string, uint32) {
			real := writeKeyFile(t, dir, k, 0o600)
			link := filepath.Join(dir, "link.key")
			require.NoError(t, os.Symlink(real, link))
			return link, EUID()
		}, false},
		{"P-384 key", func(t *testing.T, dir string) (string, uint32) {
			return writeKeyFile(t, dir, k384, 0o600), EUID()
		}, false},
		{"not a key", func(t *testing.T, dir string) (string, uint32) {
			path := filepath.Join(dir, "host.key")
			require.NoError(t, os.WriteFile(path, []byte("hello"), 0o600))
			return path, EUID()
		}, false},
		{"missing", func(_ *testing.T, dir string) (string, uint32) { return filepath.Join(dir, "none"), EUID() }, false},
		{"too large", func(t *testing.T, dir string) (string, uint32) {
			path := filepath.Join(dir, "host.key")
			require.NoError(t, os.WriteFile(path, make([]byte, maxKeyFileBytes+1), 0o600))
			return path, EUID()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, uid := tc.prep(t, t.TempDir())
			got, err := LoadKey(path, uid)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, got.Equal(k))
		})
	}
}

func TestCheckSocketDir(t *testing.T) {
	mk := func(t *testing.T, mode fs.FileMode) string {
		dir := filepath.Join(t.TempDir(), "d")
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.Chmod(dir, mode))
		return dir
	}
	require.NoError(t, CheckSocketDir(mk(t, 0o700), EUID()))
	require.NoError(t, CheckSocketDir(mk(t, 0o750), EUID()))
	require.NoError(t, CheckSocketDir(mk(t, 0o2750), EUID()))
	require.Error(t, CheckSocketDir(mk(t, 0o755), EUID()), "open to others")
	require.Error(t, CheckSocketDir(mk(t, 0o770), EUID()), "group may write")
	require.Error(t, CheckSocketDir(mk(t, 0o700), EUID()+1), "owned by another user")
	require.Error(t, CheckSocketDir("relative", EUID()))
	require.Error(t, CheckSocketDir("", EUID()))
	require.Error(t, CheckSocketDir(filepath.Join(t.TempDir(), "missing"), EUID()))
	real := mk(t, 0o700)
	link := real + "-link"
	require.NoError(t, os.Symlink(real, link))
	require.Error(t, CheckSocketDir(link, EUID()), "symbolic link")
	f := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(f, nil, 0o600))
	require.Error(t, CheckSocketDir(f, EUID()), "not a directory")
}

func TestListen(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")
	lis, err := Listen(sock)
	require.NoError(t, err)
	fi, err := os.Lstat(sock)
	require.NoError(t, err)
	assert.Equal(t, SocketMode, fi.Mode().Perm())
	assert.NotZero(t, fi.Mode()&fs.ModeSocket)
	require.NoError(t, lis.Close())

	// A stale socket (left by a killed signer) is replaced.
	stale, err := net.Listen("unix", sock)
	require.NoError(t, err)
	unixLis, ok := stale.(*net.UnixListener)
	require.True(t, ok)
	unixLis.SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())
	lis, err = Listen(sock)
	require.NoError(t, err)
	require.NoError(t, lis.Close())

	// Anything else at the path is left alone.
	other := filepath.Join(dir, "regular")
	require.NoError(t, os.WriteFile(other, []byte("keep"), 0o600))
	_, err = Listen(other)
	require.Error(t, err)
	b, err := os.ReadFile(other)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(b))
}

func TestResolveUID(t *testing.T) {
	uid, err := ResolveUID("1234")
	require.NoError(t, err)
	assert.EqualValues(t, 1234, uid)
	uid, err = ResolveUID("root")
	require.NoError(t, err)
	assert.EqualValues(t, 0, uid)
	_, err = ResolveUID("no-such-user-virtrigaud")
	require.Error(t, err)
}
