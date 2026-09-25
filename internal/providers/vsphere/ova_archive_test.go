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

package vsphere

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmware/govmomi/ovf"
	"google.golang.org/grpc/codes"
)

// writeTar writes a tar of members (name → content, in order) to a file in a
// temp directory and returns its path.
func writeTar(t *testing.T, members [][2]string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range members {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: m[0], Mode: 0o644, Size: int64(len(m[1])), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(m[1]))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	p := filepath.Join(t.TempDir(), "staged.ova")
	require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o600))
	return p
}

func readMember(t *testing.T, a *ovfPackage, name string) (string, error) {
	t.Helper()
	r, size, err := a.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, int64(len(b)), size)
	return string(b), nil
}

func TestOVFHrefError(t *testing.T) {
	for _, ok := range []string{"disk1.vmdk", "ubuntu-24.04-server-cloudimg-amd64.vmdk", "seed.iso", "disk..vmdk", "a b.vmdk"} {
		assert.Empty(t, ovfHrefError(ok), "%q is a plain file name", ok)
	}
	for _, bad := range []string{
		"", ".", "..",
		"/etc/virtrigaud/credentials/password",
		"../../etc/passwd", "dir/disk.vmdk", `..\..\secret`, `C:\secret`,
		"http://attacker.example/x.vmdk", "file:secret",
		"*", "*.vmdk", "disk?.vmdk", "[a-z]",
		"disk\x00.vmdk", "disk\n.vmdk",
		strings.Repeat("a", maxOVFHrefBytes+1),
	} {
		assert.NotEmpty(t, ovfHrefError(bad), "%q must be refused", bad)
	}
}

func TestValidateOVFFileRefs(t *testing.T) {
	env := &ovf.Envelope{References: []ovf.File{{Href: "disk1.vmdk"}, {Href: "seed.iso"}}}
	hrefs, err := validateOVFFileRefs(env)
	require.NoError(t, err)
	assert.Equal(t, []string{"disk1.vmdk", "seed.iso"}, hrefs)

	_, err = validateOVFFileRefs(&ovf.Envelope{References: []ovf.File{{Href: "disk1.vmdk"}, {Href: "/etc/shadow"}}})
	requireCode(t, err, codes.InvalidArgument)
	assert.Contains(t, err.Error(), "#2")
	assert.NotContains(t, err.Error(), "/etc/shadow", "the reference itself goes to the log only")

	hrefs, err = validateOVFFileRefs(&ovf.Envelope{})
	require.NoError(t, err)
	assert.Empty(t, hrefs)
}

func TestOVFPackage_ServesOnlyPermittedExactMembers(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(sentinel, []byte("SENTINEL"), 0o600))

	path := writeTar(t, [][2]string{
		{"._x.ovf", "applesingle"},
		{"./x.ovf", "<Envelope/>"},
		{"disk1.vmdk", "DISK"},
		{"sub/disk2.vmdk", "NESTED"},
		{"__MACOSX/._disk1.vmdk", "sidecar"},
	})
	descriptor, err := findOVADescriptorName(path)
	require.NoError(t, err)
	assert.Equal(t, "x.ovf", descriptor, "a leading ./ is ignored; the AppleDouble sidecar is skipped")
	pkg := newTarPackage(path, descriptor)

	got, err := readMember(t, pkg, "x.ovf")
	require.NoError(t, err)
	assert.Equal(t, "<Envelope/>", got)

	_, err = readMember(t, pkg, "disk1.vmdk")
	require.ErrorIs(t, err, os.ErrNotExist, "a member is served only once permitted")

	pkg.permit([]string{"disk1.vmdk"})
	got, err = readMember(t, pkg, "disk1.vmdk")
	require.NoError(t, err)
	assert.Equal(t, "DISK", got)

	for _, name := range []string{"*", "*.vmdk", "disk?.vmdk", "disk2.vmdk", "sub/disk2.vmdk", sentinel, "../" + filepath.Base(sentinel)} {
		pkg.permit([]string{name}) // even if something permitted it
		got, err := readMember(t, pkg, name)
		assert.ErrorIs(t, err, os.ErrNotExist, "%q", name)
		assert.NotEqual(t, "SENTINEL", got)
	}

	present, err := pkg.contains("disk1.vmdk")
	require.NoError(t, err)
	assert.True(t, present)
	for _, name := range []string{"disk2.vmdk", "*", sentinel} {
		present, err := pkg.contains(name)
		require.NoError(t, err)
		assert.False(t, present, "%q", name)
	}
}

func TestOVFPackage_BareOVFServesOnlyItself(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "virtrigaud-ova-1.ovf")
	require.NoError(t, os.WriteFile(staged, []byte("<Envelope/>"), 0o600))
	sibling := filepath.Join(dir, "disk1.vmdk")
	require.NoError(t, os.WriteFile(sibling, []byte("SENTINEL"), 0o600))

	pkg := newBareOVFPackage(staged)
	got, err := readMember(t, pkg, pkg.descriptor)
	require.NoError(t, err)
	assert.Equal(t, "<Envelope/>", got)

	pkg.permit([]string{"disk1.vmdk", sibling})
	for _, name := range []string{"disk1.vmdk", sibling} {
		_, err := readMember(t, pkg, name)
		assert.ErrorIs(t, err, os.ErrNotExist, "a bare .ovf never reaches its directory (%q)", name)
		present, err := pkg.contains(name)
		require.NoError(t, err)
		assert.False(t, present)
	}
}
