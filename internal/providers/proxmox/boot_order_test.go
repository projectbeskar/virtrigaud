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
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin R38: a cloned VM's boot order was "<primary disk>;<slot>".
// With SSH keys and no user-data the slot was always ide2, whatever slot the
// clone's drive was really in, or with no drive at all; and the disk was
// "scsi0" when the clone had none. PVE refuses a boot order that names a
// device the VM lacks ("invalid bootorder: device '<dev>' does not exist"),
// which pvefake now checks, so such a clone failed and was rolled back. The
// order is now the boot disk only (the template's own boot disk when its
// boot settings name one), as PVE's Cloud-Init guide and the migration import
// path do.

// bootTestSSHKey is a public key (not a secret) for the keys-only cases.
const bootTestSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBootOrderTestKeyOnly test@example.com"

// bootTemplateBootOrder is a boot order the template carries, which a clone
// inherits and keeps when the provider sets none.
const bootTemplateBootOrder = "order=net0"

// TestCloneBootOrder: the boot value is "order=<boot disk>": the first disk of
// the inherited boot order, else the legacy bootdisk, else the first disk of
// the bus scan. It never names the cloud-init drive, a CD-ROM, a missing
// device or an empty element.
func TestCloneBootOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  map[string]interface{}
		want string
	}{
		"nil config":            {nil, ""},
		"no disk":               {map[string]interface{}{"cores": 2}, ""},
		"cloud-init drive only": {map[string]interface{}{"ide2": "local-lvm:vm-101-cloudinit,media=cdrom"}, ""},
		"cd-rom only":           {map[string]interface{}{"ide0": "local:iso/x.iso,media=cdrom"}, ""},
		"empty disk value":      {map[string]interface{}{"scsi0": ""}, ""},
		"scsi0 and drive": {map[string]interface{}{
			"scsi0": "local-lvm:vm-101-disk-0,size=32G",
			"ide2":  "local-lvm:vm-101-cloudinit,media=cdrom",
		}, "order=scsi0"},
		"virtio0 and drive in sata1": {map[string]interface{}{
			"virtio0": "tank:vm-101-disk-0,size=8G",
			"sata1":   "tank:vm-101-cloudinit,media=cdrom",
		}, "order=virtio0"},
		"template order names scsi0, data disk on virtio0": {map[string]interface{}{
			pveConfigKeyBoot: "order=scsi0;ide2;net0",
			"virtio0":        "tank:vm-101-disk-1,size=100G",
			"scsi0":          "tank:vm-101-disk-0,size=8G",
			"ide2":           "tank:vm-101-cloudinit,media=cdrom",
		}, "order=scsi0"},
		"template order starts with the cd-rom": {map[string]interface{}{
			pveConfigKeyBoot: "order=ide2;sata0;net0",
			"sata0":          "tank:vm-101-disk-0,size=8G",
			"ide2":           "tank:vm-101-cloudinit,media=cdrom",
		}, "order=sata0"},
		"template order names only net0: bus scan": {map[string]interface{}{
			pveConfigKeyBoot: bootTemplateBootOrder,
			"virtio0":        "tank:vm-101-disk-1,size=100G",
			"scsi0":          "tank:vm-101-disk-0,size=8G",
		}, "order=virtio0"},
		"template order names a missing disk: bus scan": {map[string]interface{}{
			pveConfigKeyBoot: "order=scsi5",
			"scsi0":          "tank:vm-101-disk-0,size=8G",
		}, "order=scsi0"},
		"legacy bootdisk": {map[string]interface{}{
			pveConfigKeyBoot:     "cdn",
			pveConfigKeyBootDisk: "sata0",
			"virtio0":            "tank:vm-101-disk-1,size=100G",
			"sata0":              "tank:vm-101-disk-0,size=8G",
		}, "order=sata0"},
		"legacy bootdisk on a cd-rom: bus scan": {map[string]interface{}{
			pveConfigKeyBootDisk: "ide2",
			"ide2":               "tank:vm-101-cloudinit,media=cdrom",
			"scsi0":              "tank:vm-101-disk-0,size=8G",
		}, "order=scsi0"},
		"template order with no disk at all": {map[string]interface{}{
			pveConfigKeyBoot: "order=ide2;net0",
			"ide2":           "tank:vm-101-cloudinit,media=cdrom",
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, cloneBootOrder(tc.cfg))
		})
	}
}

// cloneWithoutUserData clones the template (whose raw config is tmpl) with no
// user-data, so configureClonedVM has done nothing about cloud-init yet.
func cloneWithoutUserData(t *testing.T, tmpl map[string]string) (*Provider, *pvefake.Server, pvefake.VM) {
	t.Helper()
	provider, srv := ciProviderWith(t, "", tmpl)
	req := ciRequest(`{"TemplateName": "100"}`)
	req.UserData = nil
	return provider, srv, createCIVM(t, provider, srv, req)
}

// configureWithKeysOnly runs configureClonedVM on vm with SSH keys and no
// user-data, and returns the VM as the fake has it afterwards.
func configureWithKeysOnly(t *testing.T, provider *Provider, srv *pvefake.Server, vm pvefake.VM) pvefake.VM {
	t.Helper()
	err := provider.configureClonedVM(context.Background(), &providerv1.CreateRequest{Name: "ci-vm"},
		&pveapi.VMConfig{VMID: vm.VMID, SSHKeys: bootTestSSHKey}, "pve")
	require.NoError(t, err)
	after, ok := srv.GetVM(vm.VMID)
	require.True(t, ok)
	// PVE takes sshkeys URL-encoded inside the form value; the client encodes it.
	keys, err := url.PathUnescape(after.Config["sshkeys"])
	require.NoError(t, err)
	assert.Equal(t, bootTestSSHKey, keys, "the keys are written")
	return after
}

// TestCloneBootOrder_SSHKeysWithoutUserData: SSH keys reach the guest only
// through a cloud-init drive. The clone's own drive is used in whatever slot it
// is in, one is added in ide2 when it has none, and the boot order is the
// clone's disk only.
func TestCloneBootOrder_SSHKeysWithoutUserData(t *testing.T) {
	t.Run("drive in ide2", func(t *testing.T) {
		provider, srv, vm := cloneWithoutUserData(t, map[string]string{
			"scsi0": ciTemplateBootDisk,
			"ide2":  "tank-ssd:vm-100-cloudinit,media=cdrom",
		})
		before := vm.Config["ide2"]
		after := configureWithKeysOnly(t, provider, srv, vm)
		assert.Equal(t, "order=scsi0", after.Config[pveConfigKeyBoot])
		assert.Equal(t, before, after.Config["ide2"], "the clone's own drive is kept")
	})

	t.Run("drive in another slot", func(t *testing.T) {
		provider, srv, vm := cloneWithoutUserData(t, map[string]string{
			"scsi0": ciTemplateBootDisk,
			"sata0": "tank-ssd:vm-100-cloudinit,media=cdrom",
		})
		after := configureWithKeysOnly(t, provider, srv, vm)
		assert.Equal(t, "order=scsi0", after.Config[pveConfigKeyBoot])
		assertDrive(t, after.Config["sata0"], "tank-ssd")
		assert.Empty(t, after.Config["ide2"], "no second drive")
	})

	t.Run("no drive", func(t *testing.T) {
		provider, srv, vm := cloneWithoutUserData(t, map[string]string{"scsi0": ciTemplateBootDisk})
		require.Empty(t, vm.Config["ide2"])
		after := configureWithKeysOnly(t, provider, srv, vm)
		assert.Equal(t, "order=scsi0", after.Config[pveConfigKeyBoot])
		assertDrive(t, after.Config[cloudInitDefaultSlot], "tank-ssd")
	})
}

// TestCloneBootOrder_UserData: a clone with user-data boots from its boot disk
// only, wherever its cloud-init drive is.
func TestCloneBootOrder_UserData(t *testing.T) {
	t.Run("normal: drive added in ide2", func(t *testing.T) {
		provider, srv := ciProvider(t, "")
		vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100"}`))
		assert.Equal(t, "order=scsi0", vm.Config[pveConfigKeyBoot])
		assertDrive(t, vm.Config["ide2"], "tank-ssd")
	})

	t.Run("drive in sata1, disk on virtio0", func(t *testing.T) {
		provider, srv := ciProviderWith(t, "", map[string]string{
			"scsi0":   "",
			"virtio0": ciTemplateBootDisk,
			"sata1":   "tank-ssd:vm-100-cloudinit,media=cdrom",
		})
		vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100"}`))
		assert.Equal(t, "order=virtio0", vm.Config[pveConfigKeyBoot])
		assertDrive(t, vm.Config["sata1"], "tank-ssd")
		assert.Empty(t, vm.Config["ide2"])
	})

	t.Run("the template's boot disk wins over a data disk on an earlier bus", func(t *testing.T) {
		provider, srv := ciProviderWith(t, "", map[string]string{
			"scsi0":          ciTemplateBootDisk,
			"virtio0":        "tank-ssd:vm-100-disk-1,size=100G",
			pveConfigKeyBoot: "order=scsi0;ide2;net0",
		})
		vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100"}`))
		assert.Equal(t, "order=scsi0", vm.Config[pveConfigKeyBoot])
	})

	t.Run("no primary disk: the template's boot order is kept", func(t *testing.T) {
		// "scsi0": "" removes the fake's default disk: the template has none.
		provider, srv := ciProviderWith(t, "vmstore", map[string]string{
			"scsi0":          "",
			pveConfigKeyBoot: bootTemplateBootOrder,
		})
		vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100"}`))
		assert.Equal(t, bootTemplateBootOrder, vm.Config[pveConfigKeyBoot])
		assertDrive(t, vm.Config["ide2"], "vmstore")
	})

	t.Run("re-run is idempotent", func(t *testing.T) {
		provider, srv := ciProvider(t, "")
		req := ciRequest(`{"TemplateName": "100"}`)
		vm := createCIVM(t, provider, srv, req)
		require.NoError(t, provider.configureClonedVM(context.Background(), req, &pveapi.VMConfig{VMID: vm.VMID}, "pve"))
		after, ok := srv.GetVM(vm.VMID)
		require.True(t, ok)
		assert.Equal(t, "order=scsi0", after.Config[pveConfigKeyBoot])
	})
}

// TestCloneBootOrder_NoCloudInitLeavesBootAlone: a clone with no cloud-init
// data keeps the template's boot order, as before.
func TestCloneBootOrder_NoCloudInitLeavesBootAlone(t *testing.T) {
	_, _, vm := cloneWithoutUserData(t, map[string]string{
		"scsi0":          ciTemplateBootDisk,
		pveConfigKeyBoot: bootTemplateBootOrder,
	})
	assert.Equal(t, bootTemplateBootOrder, vm.Config[pveConfigKeyBoot])
}

// TestPVEFakeChecksBootOrder: the fake refuses, like qemu-server's update API,
// a boot order that names a device the VM does not have, and (more strictly
// than PVE) an empty element; a device the same update adds is accepted.
func TestPVEFakeChecksBootOrder(t *testing.T) {
	provider, srv := ciProvider(t, "")
	ctx := context.Background()
	for _, bad := range []string{"order=scsi0;ide2", "order=;scsi0", "order=scsi7"} {
		_, err := provider.client.ReconfigureVMRaw(ctx, "pve", cloneTemplateID, map[string][]string{pveConfigKeyBoot: {bad}})
		require.Error(t, err, bad)
	}
	_, err := provider.client.ReconfigureVMRaw(ctx, "pve", cloneTemplateID, map[string][]string{
		pveConfigKeyBoot: {"order=scsi0;ide2"},
		"ide2":           {"tank-ssd:cloudinit"},
	})
	require.NoError(t, err)
	vm, ok := srv.GetVM(cloneTemplateID)
	require.True(t, ok)
	assert.Equal(t, "order=scsi0;ide2", vm.Config[pveConfigKeyBoot])
	assert.Equal(t, "tank-ssd:vm-"+strconv.Itoa(cloneTemplateID)+"-cloudinit,media=cdrom", vm.Config["ide2"])
}
