/*
Copyright 2025.

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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

func TestUndefineNVRAMFor(t *testing.T) {
	const name = "team-a.web"
	dom := func(os string) string {
		return "<domain type='kvm'><name>" + name + "</name>" + os + "<devices/></domain>"
	}
	for _, tc := range []struct {
		desc, xml string
		want      undefineNVRAM
	}{
		{"BIOS", dom("<os><type arch='x86_64' machine='pc'>hvm</type></os>"), undefineNoNVRAM},
		{"own store", dom("<os firmware='efi'><nvram template='/t'>/var/lib/libvirt/qemu/nvram/team-a.web_VARS.fd</nvram></os>"), undefineRemoveNVRAM},
		{"firmware efi, store not named yet", dom("<os firmware='efi'><type arch='x86_64' machine='q35'>hvm</type></os>"), undefineRemoveNVRAM},
		{"clone-rewritten own store in a custom directory", dom("<os><nvram>/srv/nvram/team-a.web_VARS.fd</nvram></os>"), undefineRemoveNVRAM},
		{"another name's store (adopted, hand-made)", dom("<os><nvram>/srv/nvram/shared_VARS.fd</nvram></os>"), undefineKeepNVRAM},
		{"unparseable", "<domain", undefineNoNVRAM},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.want, undefineNVRAMFor(name, tc.xml))
		})
	}
}

// uefiCreateReq is a secure-boot create of team-a/web from base.
func (c *createHost) uefiCreateReq(base string) contracts.CreateRequest {
	req := c.createReq(ownerTeamA, base)
	req.Class.SecurityProfile = &contracts.SecurityProfile{SecureBoot: true}
	return req
}

// withNVRAMDir makes the fake libvirt fill in UEFI variable store paths below
// a fresh directory, which it returns.
func (c *createHost) withNVRAMDir() string {
	c.t.Helper()
	dir := filepath.Join(c.base, "nvram")
	require.NoError(c.t, os.MkdirAll(dir, 0o750))
	require.NoError(c.t, os.WriteFile(filepath.Join(c.root, "h1", "nvram-dir"), []byte(dir), 0o600))
	return dir
}

// TestCreate_UEFINeverInheritsALeftoverVarstore: a new UEFI domain never
// starts from a variable store an earlier domain of the same name left.
func TestCreate_UEFINeverInheritsALeftoverVarstore(t *testing.T) {
	ctx := context.Background()

	t.Run("no file: nothing removed", func(t *testing.T) {
		c := newCreateHost(t)
		c.withNVRAMDir()
		resp, err := c.p.Create(ctx, c.uefiCreateReq(c.file(c.images, "ubuntu.qcow2")))
		require.NoError(t, err)
		assert.Equal(t, "team-a.web", resp.ID)
		assert.Empty(t, c.removals())
		assert.Contains(t, c.log("virsh"), "dumpxml team-a.web --inactive")
	})

	t.Run("leftover file no domain uses: removed before the first start", func(t *testing.T) {
		c := newCreateHost(t)
		store := filepath.Join(c.withNVRAMDir(), "team-a.web_VARS.fd")
		require.NoError(t, os.WriteFile(store, []byte("old secure boot state"), 0o600))

		resp, err := c.p.Create(ctx, c.uefiCreateReq(c.file(c.images, "ubuntu.qcow2")))
		require.NoError(t, err)
		assert.Equal(t, "team-a.web", resp.ID)
		assert.Equal(t, []string{store}, c.removals(), "the leftover store is removed, nothing else")
	})

	t.Run("store path is a symlink: refused, new domain undefined keeping the file, disk removed", func(t *testing.T) {
		c := newCreateHost(t)
		dir := c.withNVRAMDir()
		target := c.file(c.outside, "victim_VARS.fd")
		require.NoError(t, os.Symlink(target, filepath.Join(dir, "team-a.web_VARS.fd")))

		_, err := c.p.Create(ctx, c.uefiCreateReq(c.file(c.images, "ubuntu.qcow2")))
		require.Error(t, err)
		assert.True(t, contracts.IsConflict(err), "%v", err)
		assert.NotContains(t, err.Error(), target, "no host path in the answer")
		assert.Contains(t, c.virshCalls("h1"), "undefine team-a.web --keep-nvram")
		_, statErr := os.Stat(filepath.Join(c.root, "h1", "dom-team-a.web.xml"))
		assert.True(t, os.IsNotExist(statErr), "the new domain is undefined again")
		assert.Equal(t, []string{filepath.Join(c.images, "team-a.web-disk.qcow2")}, c.removals(),
			"only the attempt's disk is removed, never the store or its target")
		assert.Empty(t, c.stagingEntries())
	})

	t.Run("store path is another domain's store: refused", func(t *testing.T) {
		c := newCreateHost(t)
		dir := c.withNVRAMDir()
		store := filepath.Join(dir, "team-a.web_VARS.fd")
		require.NoError(t, os.WriteFile(store, []byte("in use"), 0o600))
		other := "<domain type='kvm'><name>legacy</name><uuid>99999999-2222-4333-8444-555555555555</uuid>" +
			"<os><nvram>" + store + "</nvram></os><devices/></domain>"
		c.define("h1", "legacy", "99999999-2222-4333-8444-555555555555", other)
		require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte("legacy\n"), 0o600))

		_, err := c.p.Create(ctx, c.uefiCreateReq(c.file(c.images, "ubuntu.qcow2")))
		require.Error(t, err)
		assert.True(t, contracts.IsConflict(err), "%v", err)
		for _, r := range c.removals() {
			assert.NotEqual(t, store, r, "another domain's store is never removed")
		}
		assert.FileExists(t, store)
	})

	t.Run("BIOS create: no varstore check", func(t *testing.T) {
		c := newCreateHost(t)
		c.withNVRAMDir()
		_, err := c.p.Create(ctx, c.createReq(ownerTeamA, c.file(c.images, "ubuntu.qcow2")))
		require.NoError(t, err)
		assert.NotContains(t, c.log("virsh"), "--inactive")
	})
}

// TestDelete_UEFIDomainRemovesOnlyItsOwnVarstore: Delete passes --nvram for
// the domain's own store, --keep-nvram for any other path, and nothing for a
// BIOS domain.
func TestDelete_UEFIDomainRemovesOnlyItsOwnVarstore(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		desc, osXML, want string
	}{
		{"own store", "<os firmware='efi'><nvram>/var/lib/libvirt/qemu/nvram/team-a.web_VARS.fd</nvram></os>", "undefine team-a.web --nvram"},
		{"foreign store", "<os><nvram>/srv/shared_VARS.fd</nvram></os>", "undefine team-a.web --keep-nvram"},
		{"BIOS", "<os><type arch='x86_64' machine='pc'>hvm</type></os>", "undefine team-a.web"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			c := newCreateHost(t)
			const uuid = "11111111-2222-4333-8444-555555555555"
			x := "<domain type='kvm'><name>team-a.web</name><uuid>" + uuid + "</uuid>" + tc.osXML + "<devices/></domain>"
			c.define("h1", "team-a.web", uuid, x)
			require.NoError(t, os.WriteFile(filepath.Join(c.root, "h1", "names"), []byte("team-a.web\n"), 0o600))

			_, err := c.p.Delete(ctx, contracts.VMRef{ID: "team-a.web"})
			require.NoError(t, err)
			var undefines []string
			for _, call := range c.virshCalls("h1") {
				if strings.HasPrefix(call, "undefine ") {
					undefines = append(undefines, call)
				}
			}
			assert.Equal(t, []string{tc.want}, undefines)
		})
	}
}
