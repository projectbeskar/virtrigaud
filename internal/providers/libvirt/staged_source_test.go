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

package libvirt

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

const (
	stagedTestPVC     = "mig-staging"
	stagedTestRel     = "vmmigrations/team-a/web/export.qcow2"
	stagedTestURL     = "pvc://" + stagedTestPVC + "/" + stagedTestRel
	stagedTestDisk    = "qcow2 bytes"
	stagedTestSecret  = "provider-credential-password"
	stagedTestCredDir = "credentials"
)

// stagedPVC points stagedPVCMountRoot at a temp dir holding one PVC mount and,
// next to it, a file standing in for the provider pod's credentials. It
// returns the PVC mount and the credential file.
func stagedPVC(t *testing.T) (mount, secret string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "migration-storage")
	mount = filepath.Join(root, stagedTestPVC)
	require.NoError(t, os.MkdirAll(filepath.Join(mount, filepath.Dir(stagedTestRel)), 0o755))
	creds := filepath.Join(base, stagedTestCredDir)
	require.NoError(t, os.MkdirAll(creds, 0o700))
	secret = filepath.Join(creds, "password")
	require.NoError(t, os.WriteFile(secret, []byte(stagedTestSecret), 0o600))
	prev := stagedPVCMountRoot
	stagedPVCMountRoot = root
	t.Cleanup(func() { stagedPVCMountRoot = prev })
	return mount, secret
}

// stagedPVCURL returns the pvc:// URL of the staged file (or directory) at
// path, pointing stagedPVCMountRoot at its grandparent for the rest of the
// test: path's directory stands in for the PVC mount, so its name must be a
// DNS-1123 subdomain (t.TempDir's "001" is).
func stagedPVCURL(t *testing.T, path string) string {
	t.Helper()
	pvcDir := filepath.Dir(path)
	prev := stagedPVCMountRoot
	stagedPVCMountRoot = filepath.Dir(pvcDir)
	t.Cleanup(func() { stagedPVCMountRoot = prev })
	return "pvc://" + filepath.Base(pvcDir) + "/" + filepath.Base(path)
}

// assertStagedDiskRefused: FailedPrecondition with the constant message, which
// names neither the staging path nor the credential.
func assertStagedDiskRefused(t *testing.T, err error, mount, secret string) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.FailedPrecondition, st.Code())
	assert.Equal(t, stagedDiskRefusedMessage, st.Message())
	assert.NotContains(t, st.Message(), mount)
	assert.NotContains(t, st.Message(), secret)
}

func TestOpenStagedImportSource_RegularFile(t *testing.T) {
	mount, _ := stagedPVC(t)
	require.NoError(t, os.WriteFile(filepath.Join(mount, stagedTestRel), []byte(stagedTestDisk), 0o644))
	f, p, err := openStagedImportSource(stagedTestURL)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	assert.Equal(t, filepath.Join(mount, stagedTestRel), p)
	b, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, stagedTestDisk, string(b))
}

func TestOpenStagedImportSource_Refusals(t *testing.T) {
	cases := map[string]func(t *testing.T, mount, secret string){
		"symlinked staged file to a credential": func(t *testing.T, mount, secret string) {
			require.NoError(t, os.Symlink(secret, filepath.Join(mount, stagedTestRel)))
		},
		"symlinked staged file to an /etc file": func(t *testing.T, mount, _ string) {
			require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(mount, stagedTestRel)))
		},
		"symlinked parent directory": func(t *testing.T, mount, secret string) {
			outside := filepath.Join(filepath.Dir(secret), "planted")
			require.NoError(t, os.MkdirAll(filepath.Join(outside, "web"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(outside, "web", "export.qcow2"), []byte(stagedTestSecret), 0o600))
			team := filepath.Join(mount, "vmmigrations", "team-a")
			require.NoError(t, os.RemoveAll(team))
			require.NoError(t, os.Symlink(outside, team))
		},
		"symlinked top-level staging directory": func(t *testing.T, mount, secret string) {
			require.NoError(t, os.RemoveAll(filepath.Join(mount, "vmmigrations")))
			require.NoError(t, os.Symlink(filepath.Dir(secret), filepath.Join(mount, "vmmigrations")))
		},
		"hard-linked staged file": func(t *testing.T, mount, _ string) {
			p := filepath.Join(mount, stagedTestRel)
			require.NoError(t, os.WriteFile(p, []byte(stagedTestDisk), 0o644))
			require.NoError(t, os.Link(p, filepath.Join(mount, "other-name")))
		},
		"FIFO": func(t *testing.T, mount, _ string) {
			require.NoError(t, syscall.Mkfifo(filepath.Join(mount, stagedTestRel), 0o644))
		},
		"directory": func(t *testing.T, mount, _ string) {
			require.NoError(t, os.Mkdir(filepath.Join(mount, stagedTestRel), 0o755))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			mount, secret := stagedPVC(t)
			plant(t, mount, secret)
			_, _, err := openStagedImportSource(stagedTestURL)
			assertStagedDiskRefused(t, err, mount, secret)
		})
	}
}

func TestOpenStagedImportSource_MissingAndBadURLs(t *testing.T) {
	stagedPVC(t)
	_, _, err := openStagedImportSource(stagedTestURL)
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Equal(t, stagedDiskNotFoundMessage, status.Convert(err).Message())

	for _, u := range []string{
		"pvc://" + stagedTestPVC,
		"pvc://" + stagedTestPVC + "/",
		"pvc://../etc/hostname",
		"pvc://Not_A_PVC/x",
		"pvc://" + stagedTestPVC + "/../../credentials/password",
		"file://relative/path",
		"file:///etc/hostname",
		"file:///proc/self/environ",
		"http://example.invalid/disk.qcow2",
	} {
		_, _, err := openStagedImportSource(u)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), u)
		assert.Equal(t, stagedDiskBadURLMessage, status.Convert(err).Message(), u)
	}
}

// TestOpenStagedImportSource_FileSchemeRefused: file:// is not a source, even
// for a real regular file (the manager never sends it; it would let a caller
// import any regular file of the pod).
func TestOpenStagedImportSource_FileSchemeRefused(t *testing.T) {
	mount, _ := stagedPVC(t)
	p := filepath.Join(mount, stagedTestRel)
	require.NoError(t, os.WriteFile(p, []byte(stagedTestDisk), 0o644))
	_, _, err := openStagedImportSource("file://" + p)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Equal(t, stagedDiskBadURLMessage, status.Convert(err).Message())

	fc := &fakeSeamConn{id: "host-a"}
	s := &Server{provider: &fakeSeamProvider{hostConn: fc}}
	_, err = s.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{SourceUrl: "file://" + p, Format: "qcow2"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Zero(t, fc.copyCalls)
}

// TestServer_ImportDisk_LocalConnectionRefusesSymlinkedStagedDisk: over a
// local libvirt connection (the private-copy branch) a planted link to a
// credential is refused and nothing lands in the image pool.
func TestServer_ImportDisk_LocalConnectionRefusesSymlinkedStagedDisk(t *testing.T) {
	c := newCreateHost(t)
	s := c.withRegistry()
	conn, err := s.provider.conn(context.Background())
	require.NoError(t, err)
	require.NotContains(t, conn.uri(), "ssh://", "the local-connection branch")

	secret := c.file(c.outside, "password")
	pvcDir := filepath.Join(t.TempDir(), "staging")
	require.NoError(t, os.MkdirAll(pvcDir, 0o755))
	staged := filepath.Join(pvcDir, "export.qcow2")
	require.NoError(t, os.Symlink(secret, staged))
	before, err := os.ReadDir(c.images)
	require.NoError(t, err)

	_, err = c.importFor(s, staged, ownerTeamA, "web"+contracts.ImportedDiskNameSuffix)
	assertStagedDiskRefused(t, err, pvcDir, secret)
	after, err := os.ReadDir(c.images)
	require.NoError(t, err)
	assert.Equal(t, len(before), len(after), "nothing lands in the image pool")

	// The same staged file as a regular file imports.
	require.NoError(t, os.Remove(staged))
	require.NoError(t, os.WriteFile(staged, []byte("qcow2"), 0o644))
	_, err = c.importFor(s, staged, ownerTeamA, "web"+contracts.ImportedDiskNameSuffix)
	require.NoError(t, err)
}

// TestServer_ImportDisk_RefusesSymlinkedStagedDisk: over an SSH connection a
// planted link is refused before anything is copied to the host.
func TestServer_ImportDisk_RefusesSymlinkedStagedDisk(t *testing.T) {
	mount, secret := stagedPVC(t)
	require.NoError(t, os.Symlink(secret, filepath.Join(mount, stagedTestRel)))
	fc := &fakeSeamConn{id: "host-a"}
	s := &Server{provider: &fakeSeamProvider{hostConn: fc}}

	_, err := s.ImportDisk(context.Background(), &providerv1.ImportDiskRequest{SourceUrl: stagedTestURL, Format: "qcow2"})
	assertStagedDiskRefused(t, err, mount, secret)
	assert.Zero(t, fc.copyCalls, "nothing is copied to the host")
	assert.Empty(t, fc.virshCalls, "nothing runs on the host")
}
