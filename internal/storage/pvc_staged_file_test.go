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

package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/storage/stagedfile"
)

const (
	pvcTestURL      = "pvc://staging/vmmigrations/ns/mig/export.qcow2"
	pvcTestRel      = "vmmigrations/ns/mig/export.qcow2"
	pvcTestSecret   = "provider-credential-password"
	pvcTestDiskData = "qcow2 disk bytes"
)

// pvcFixture is a PVC mount (writable by the tenant) and, next to it, a file
// standing in for the provider pod's credentials.
func pvcFixture(t *testing.T) (*PVCStorage, string, string) {
	t.Helper()
	base := t.TempDir()
	mount := filepath.Join(base, "migration-storage", "staging")
	require.NoError(t, os.MkdirAll(mount, 0o755))
	creds := filepath.Join(base, "credentials")
	require.NoError(t, os.MkdirAll(creds, 0o700))
	secret := filepath.Join(creds, "password")
	require.NoError(t, os.WriteFile(secret, []byte(pvcTestSecret), 0o600))
	p, err := NewPVCStorage(StorageConfig{Type: "pvc", MountPath: mount})
	require.NoError(t, err)
	return p, mount, secret
}

// assertStagedRefusal: the refusal is typed, matches stagedfile.ErrUnsafe, and
// its text is constant — no mount path, no credential path, no secret.
func assertStagedRefusal(t *testing.T, err error, mount, secret string) {
	t.Helper()
	require.Error(t, err)
	var se *StorageError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, ErrorTypeUnsafePath, se.Type)
	assert.True(t, stagedfile.IsUnsafe(err))
	assert.Equal(t, msgStagedFileRefused+": "+stagedfile.ErrUnsafe.Error(), err.Error())
	assert.NotContains(t, err.Error(), mount)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), pvcTestSecret)
}

func TestPVCStorage_RoundTrip(t *testing.T) {
	p, mount, _ := pvcFixture(t)
	ctx := context.Background()

	up, err := p.Upload(ctx, UploadRequest{DestinationURL: pvcTestURL, Reader: strings.NewReader(pvcTestDiskData)})
	require.NoError(t, err)
	assert.Equal(t, int64(len(pvcTestDiskData)), up.BytesTransferred)
	fi, err := os.Lstat(filepath.Join(mount, pvcTestRel))
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())

	// A retried export replaces the earlier file.
	_, err = p.Upload(ctx, UploadRequest{DestinationURL: pvcTestURL, Reader: strings.NewReader(pvcTestDiskData)})
	require.NoError(t, err)

	var buf bytes.Buffer
	dl, err := p.Download(ctx, DownloadRequest{SourceURL: pvcTestURL, Writer: &buf, VerifyChecksum: true, ExpectedChecksum: up.Checksum})
	require.NoError(t, err)
	assert.Equal(t, pvcTestDiskData, buf.String())
	assert.Equal(t, up.Checksum, dl.Checksum)

	md, err := p.GetMetadata(ctx, pvcTestURL)
	require.NoError(t, err)
	assert.Equal(t, int64(len(pvcTestDiskData)), md.Size)
	assert.Equal(t, up.Checksum, md.Checksum)

	require.NoError(t, p.Delete(ctx, pvcTestURL))
	require.NoError(t, p.Delete(ctx, pvcTestURL), "deleting a missing file is not an error")

	_, err = p.Download(ctx, DownloadRequest{SourceURL: pvcTestURL, Writer: &buf})
	var se *StorageError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, ErrorTypeNotFound, se.Type)
	assert.NotContains(t, err.Error(), mount)
}

func TestPVCStorage_DownloadRefusesSymlinkedStagedFile(t *testing.T) {
	p, mount, secret := pvcFixture(t)
	staged := filepath.Join(mount, pvcTestRel)
	require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
	require.NoError(t, os.Symlink(secret, staged))

	var buf bytes.Buffer
	_, err := p.Download(context.Background(), DownloadRequest{SourceURL: pvcTestURL, Writer: &buf})
	assertStagedRefusal(t, err, mount, secret)
	assert.Zero(t, buf.Len(), "not one byte of the link target is read")

	_, err = p.GetMetadata(context.Background(), pvcTestURL)
	assertStagedRefusal(t, err, mount, secret)
}

func TestPVCStorage_DownloadRefusesSymlinkedParentDirectory(t *testing.T) {
	p, mount, secret := pvcFixture(t)
	// The tenant points vmmigrations at a directory outside the PVC whose
	// "ns/mig/export.qcow2" is a plain copy of a secret.
	outside := filepath.Join(filepath.Dir(secret), "planted")
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "ns", "mig"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "ns", "mig", "export.qcow2"), []byte(pvcTestSecret), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(mount, "vmmigrations")))

	var buf bytes.Buffer
	_, err := p.Download(context.Background(), DownloadRequest{SourceURL: pvcTestURL, Writer: &buf})
	assertStagedRefusal(t, err, mount, secret)
	assert.Zero(t, buf.Len())
}

func TestPVCStorage_UploadRefusesPlantedSymlinks(t *testing.T) {
	ctx := context.Background()

	t.Run("at the staged file's name", func(t *testing.T) {
		p, mount, secret := pvcFixture(t)
		staged := filepath.Join(mount, pvcTestRel)
		require.NoError(t, os.MkdirAll(filepath.Dir(staged), 0o755))
		require.NoError(t, os.Symlink(secret, staged))
		_, err := p.Upload(ctx, UploadRequest{DestinationURL: pvcTestURL, Reader: strings.NewReader("overwrite")})
		assertStagedRefusal(t, err, mount, secret)
		got, err := os.ReadFile(secret)
		require.NoError(t, err)
		assert.Equal(t, pvcTestSecret, string(got), "the link target is not written")
	})

	t.Run("as a parent directory", func(t *testing.T) {
		p, mount, secret := pvcFixture(t)
		outside := filepath.Join(filepath.Dir(secret), "planted")
		require.NoError(t, os.MkdirAll(outside, 0o755))
		require.NoError(t, os.Symlink(outside, filepath.Join(mount, "vmmigrations")))
		_, err := p.Upload(ctx, UploadRequest{DestinationURL: pvcTestURL, Reader: strings.NewReader("data")})
		assertStagedRefusal(t, err, mount, secret)
		entries, err := os.ReadDir(outside)
		require.NoError(t, err)
		assert.Empty(t, entries, "nothing is created outside the PVC")
	})
}

func TestPVCStorage_DeleteDoesNotFollowParentSymlink(t *testing.T) {
	p, mount, secret := pvcFixture(t)
	outside := filepath.Join(filepath.Dir(secret), "planted")
	victim := filepath.Join(outside, "ns", "mig", "export.qcow2")
	require.NoError(t, os.MkdirAll(filepath.Dir(victim), 0o755))
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(mount, "vmmigrations")))

	err := p.Delete(context.Background(), pvcTestURL)
	assertStagedRefusal(t, err, mount, secret)
	_, err = os.Stat(victim)
	assert.NoError(t, err, "nothing outside the PVC is removed")
}

func TestPVCStorage_ParsePathRejectsEmptyAndTraversal(t *testing.T) {
	p, _, _ := pvcFixture(t)
	for _, u := range []string{"pvc://staging", "pvc://staging/", "pvc://staging/../x", "pvc://staging//"} {
		assert.Error(t, p.ValidateURL(u), u)
	}
	assert.NoError(t, p.ValidateURL(pvcTestURL))
}
