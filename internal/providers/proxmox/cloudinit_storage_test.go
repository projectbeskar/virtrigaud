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

package proxmox

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin R33 (v0.4.0-rc4 validation on PVE 9.2): the cloud-init drive
// of a VM with user data was always "local:cloudinit" for a template clone,
// because the clone path never carried a storage. `local` is a directory
// storage without the `images` content type on a stock PVE, so PVE refused to
// start every such VM. The storage is now the request's/image's, else the
// operator's PVE_DEFAULT_STORAGE, else the one the cloned boot disk lives on.
// Validated against pvefake only, not a real PVE.

// cloudInitUserData is user data that turns the cloud-init drive on.
var cloudInitUserData = []byte("#cloud-config\nhostname: ci-vm\n")

// cloneTemplateID is the VMID of the template the tests below clone.
const cloneTemplateID = 100

// ciTemplateBootDisk is the template's boot disk (on its own storage).
const ciTemplateBootDisk = "tank-ssd:vm-100-disk-0,size=32G"

// ciProvider returns a provider on a fake PVE whose template (VMID 100) has its
// boot disk on tank-ssd, with defaultStorage as the operator's default.
func ciProvider(t *testing.T, defaultStorage string) (*Provider, *pvefake.Server) {
	t.Helper()
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	tmpl, ok := srv.GetVM(cloneTemplateID)
	require.True(t, ok)
	tmpl.Config = map[string]string{"scsi0": ciTemplateBootDisk}
	srv.PutVM(tmpl)
	srv.SetTaskDelay(time.Millisecond)
	provider := createTestProvider(endpoint)
	provider.client.Config().DefaultStorage = defaultStorage
	provider.client.Config().TaskPollInterval = 5 * time.Millisecond
	return provider, srv
}

// createCI creates a VM with user data from imageJSON and returns its fake
// config's cloud-init drive (ide2).
func createCI(t *testing.T, provider *Provider, srv *pvefake.Server, imageJSON string) string {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.Create(ctx, &providerv1.CreateRequest{
		Name:      "ci-vm",
		ClassJson: `{"CPU": 2, "MemoryMiB": 2048}`,
		ImageJson: imageJSON,
		UserData:  cloudInitUserData,
	})
	require.NoError(t, err)
	if resp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, resp.Task.Id))
	}
	vmid, err := strconv.Atoi(resp.Id)
	require.NoError(t, err)
	vm, ok := srv.GetVM(vmid)
	require.True(t, ok)
	return vm.Config["ide2"]
}

// TestCloudInitStorage_CloneNoHintFollowsBootDisk: no placement, no image
// storage and no operator default, so the drive goes where the cloned boot disk
// lives, never to `local`.
func TestCloudInitStorage_CloneNoHintFollowsBootDisk(t *testing.T) {
	provider, srv := ciProvider(t, "")
	ide2 := createCI(t, provider, srv, `{"TemplateName": "100"}`)
	assert.Equal(t, "tank-ssd:cloudinit", ide2)
}

// TestCloudInitStorage_CloneNoHintUsesDefaultStorage: the operator's
// PVE_DEFAULT_STORAGE beats the boot disk's storage.
func TestCloudInitStorage_CloneNoHintUsesDefaultStorage(t *testing.T) {
	provider, srv := ciProvider(t, "vmstore")
	ide2 := createCI(t, provider, srv, `{"TemplateName": "100"}`)
	assert.Equal(t, "vmstore:cloudinit", ide2)
}

// TestCloudInitStorage_ExplicitContractsStorageWins: the contracts.VMImage
// "storage" hint beats the operator default and the boot disk.
func TestCloudInitStorage_ExplicitContractsStorageWins(t *testing.T) {
	provider, srv := ciProvider(t, "vmstore")
	ide2 := createCI(t, provider, srv, `{"TemplateName": "100", "storage": "nfs-vm"}`)
	assert.Equal(t, "nfs-vm:cloudinit", ide2)
}

// TestCloudInitStorage_ImageSpecStorageReadWithTemplate: source.proxmox.storage
// of a VMImageSpec is read although the template is named, and beats the
// operator default and the boot disk.
func TestCloudInitStorage_ImageSpecStorageReadWithTemplate(t *testing.T) {
	provider, srv := ciProvider(t, "vmstore")
	ide2 := createCI(t, provider, srv,
		`{"TemplateName": "100", "source": {"proxmox": {"templateID": 100, "storage": "nfs-vm"}}}`)
	assert.Equal(t, "nfs-vm:cloudinit", ide2)
}

// TestCloudInitStorage_NeverLocalWhenBootDiskElsewhere: whichever way the
// storage is chosen, a template whose disk lives elsewhere never gets a
// cloud-init drive on `local`.
func TestCloudInitStorage_NeverLocalWhenBootDiskElsewhere(t *testing.T) {
	for name, tc := range map[string]struct {
		defaultStorage string
		imageJSON      string
	}{
		"no hint":         {"", `{"TemplateName": "100"}`},
		"default storage": {"vmstore", `{"TemplateName": "100"}`},
		"image storage":   {"", `{"TemplateName": "100", "storage": "nfs-vm"}`},
	} {
		t.Run(name, func(t *testing.T) {
			provider, srv := ciProvider(t, tc.defaultStorage)
			ide2 := createCI(t, provider, srv, tc.imageJSON)
			assert.NotEmpty(t, ide2)
			assert.NotContains(t, ide2, "local:")
		})
	}
}

// TestCloudInitStorage_DisklessCreate: a VM made from nothing has no boot disk
// to follow, so the operator's default is used, and `local` only when nothing
// names a storage.
func TestCloudInitStorage_DisklessCreate(t *testing.T) {
	provider, srv := ciProvider(t, "vmstore")
	assert.Equal(t, "vmstore:cloudinit", createCI(t, provider, srv, ""))

	provider, srv = ciProvider(t, "")
	assert.Equal(t, "local:cloudinit", createCI(t, provider, srv, ""), "the last resort, with nothing to go by")

	provider, srv = ciProvider(t, "vmstore")
	assert.Equal(t, "nfs-vm:cloudinit", createCI(t, provider, srv, `{"storage": "nfs-vm"}`),
		"an explicit image storage beats the default")
}

// TestCloudInitStorage_UnreadableBootDiskFallsBackToLocal: a clone whose
// config names no disk storage (a passthrough device) goes to the last resort.
func TestCloudInitStorage_UnreadableBootDiskFallsBackToLocal(t *testing.T) {
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	tmpl, ok := srv.GetVM(cloneTemplateID)
	require.True(t, ok)
	tmpl.Config = map[string]string{"scsi0": "/dev/disk/by-id/foo"}
	srv.PutVM(tmpl)
	srv.SetTaskDelay(time.Millisecond)
	provider := createTestProvider(endpoint)
	provider.client.Config().TaskPollInterval = 5 * time.Millisecond

	assert.Equal(t, "local:cloudinit", createCI(t, provider, srv, `{"TemplateName": "100"}`))
}

// TestConfiguredCloudInitStorage: the request's storage beats the operator's
// default; neither gives "".
func TestConfiguredCloudInitStorage(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	assert.Empty(t, provider.configuredCloudInitStorage(""))
	provider.client.Config().DefaultStorage = "vmstore"
	assert.Equal(t, "vmstore", provider.configuredCloudInitStorage(""))
	assert.Equal(t, "nfs-vm", provider.configuredCloudInitStorage("nfs-vm"))
}

// TestDiskStorage: the storage of a disk's config value.
func TestDiskStorage(t *testing.T) {
	for value, want := range map[string]string{
		"local-lvm:vm-100-disk-0,size=32G":       "local-lvm",
		"tank-ssd:100/vm-100-disk-0.qcow2":       "tank-ssd",
		"ceph.pool_1:vm-100-disk-0,discard=on":   "ceph.pool_1",
		"  zfs-pool:vm-100-disk-0 ":              "zfs-pool",
		"/dev/disk/by-id/ata-x":                  "",
		"/mnt/a:b":                               "",
		"none":                                   "",
		"":                                       "",
		"local-lvm:":                             "",
		"bad storage:vm-100-disk-0":              "",
		"local-lvm,size=32G":                     "",
		"evil,ide2=x:vm-100-disk-0":              "",
		"-lead:vm-100-disk-0":                    "",
		"local-lvm:vm-100-disk-0,media=cdrom":    "local-lvm",
		"UPPER:vm-100-disk-0":                    "UPPER",
		"a:vm-1":                                 "",
		"ab:vm-1":                                "ab",
		"local-lvm:vm-100-cloudinit,media=cdrom": "local-lvm",
	} {
		assert.Equal(t, want, pveapi.DiskStorage(value), "value %q", value)
	}
}
