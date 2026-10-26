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
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin rc1 finding B1: qemu-server appends the CD-ROM a `qm set`
// attaches to a non-empty boot order (pvefake models it, bootorder.go), so a
// seed attached after the boot order was set ended up in it
// (`order=scsi0;ide2`, `order=net0;ide2`). Every path that attaches a seed
// leaves a final boot order without the seed's slot.

// bootOrderDevices returns the devices of a fake VM's boot order.
func bootOrderDevices(vm pvefake.VM) []string {
	var out []string
	for _, option := range strings.Split(vm.Config[pveConfigKeyBoot], ",") {
		if order, ok := strings.CutPrefix(strings.TrimSpace(option), pveBootOrderPrefix); ok {
			out = append(out, strings.Split(order, ";")...)
		}
	}
	return out
}

// assertSeedNotInBootOrder asserts that the fake VM has a seed of its own
// and that no seed slot is in its boot order.
func assertSeedNotInBootOrder(t *testing.T, vm pvefake.VM) {
	t.Helper()
	cfg := map[string]interface{}{}
	for k, v := range vm.Config {
		cfg[k] = v
	}
	own, foreign := seedSlots(cfg, vm.VMID)
	require.NotEmpty(t, own, "VM %d has a seed of its own", vm.VMID)
	for _, slot := range append(own, foreign...) {
		assert.NotContains(t, bootOrderDevices(vm), slot, "the seed slot %s is not in the boot order %q", slot, vm.Config[pveConfigKeyBoot])
	}
}

// qmSetImportedDisk matches the imported-disk create's `qm set` that attaches
// the boot disk and sets the boot order.
var qmSetImportedDisk = regexp.MustCompile(`^qm set (\d+) .*--scsi0 '([^']+)' --boot (order=\S+)$`)

// applyingImportNode answers an imported-disk create's node commands like
// recordingNode, and applies its `qm set` (boot disk and boot order) to the
// fake, as the node would.
func applyingImportNode(t *testing.T, srv *pvefake.Server) func(ctx context.Context, cmd string) (string, string, error) {
	rec := &recordingNode{}
	return func(ctx context.Context, cmd string) (string, string, error) {
		if m := qmSetImportedDisk.FindStringSubmatch(cmd); m != nil {
			vmid, err := strconv.Atoi(m[1])
			require.NoError(t, err)
			vm, ok := srv.GetVM(vmid)
			require.True(t, ok)
			if vm.Config == nil {
				vm.Config = map[string]string{}
			}
			vm.Config["scsi0"] = m[2]
			vm.Config[pveConfigKeyBoot] = m[3]
			srv.PutVM(vm)
		}
		return rec.run(ctx, cmd)
	}
}

// TestSeed_BootOrderNeverHasTheSeed: the template-clone Create, the Create of
// a new VM, the imported-disk Create and the Clone RPC (a seeded source, and
// inline user-data) all end with a boot order that does not name the seed.
func TestSeed_BootOrderNeverHasTheSeed(t *testing.T) {
	t.Run("template clone", func(t *testing.T) {
		p, srv, _, _ := seedTestProvider(t)
		resp, err := p.Create(context.Background(), seedCreateReq())
		require.NoError(t, err)
		vm := vmByID(t, srv, resp.Id)
		assert.Equal(t, "order=scsi0", vm.Config[pveConfigKeyBoot])
		assertSeedNotInBootOrder(t, vm)
	})

	t.Run("new VM without a template", func(t *testing.T) {
		p, srv, _, _ := seedTestProvider(t)
		srv.SetAssignCreateMACs(true)
		p.client.Config().DefaultStorage = "vmstore"
		req := seedCreateReq()
		req.ImageJson = ""
		resp, err := p.Create(context.Background(), req)
		require.NoError(t, err)
		vm := vmByID(t, srv, resp.Id)
		assert.Equal(t, "order=net0", vm.Config[pveConfigKeyBoot], "PVE's default order of a VM without a disk, without the seed")
		assertSeedNotInBootOrder(t, vm)
	})

	t.Run("imported disk", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		node.delegate = applyingImportNode(t, srv)
		req := createReq(testOwner(), ownStageImageJSON)
		req.UserData = seedUserData
		resp, err := p.Create(context.Background(), req)
		require.NoError(t, err)
		vm := vmByID(t, srv, resp.Id)
		assert.Equal(t, "order=scsi0", vm.Config[pveConfigKeyBoot])
		assertSeedNotInBootOrder(t, vm)
	})

	t.Run("clone of a seeded source", func(t *testing.T) {
		p, srv, _, _ := seedTestProvider(t)
		srcID := seededSource(t, p)
		req := stampedCloneReq(srcID)
		req.SourceOwner = ownerProto(otherOwner())
		resp, err := p.Clone(context.Background(), req)
		require.NoError(t, err)
		clone := vmByID(t, srv, resp.TargetVmId)
		assert.Equal(t, "order=scsi0", clone.Config[pveConfigKeyBoot])
		assertSeedNotInBootOrder(t, clone)
	})

	t.Run("clone with inline user-data", func(t *testing.T) {
		p, srv, _, _ := seedTestProvider(t)
		srv.PutVM(pvefake.VM{VMID: 300, Name: "web", Status: "stopped", Node: "pve", CPUs: 1,
			Config: map[string]string{"scsi0": "tank-ssd:vm-300-disk-0", "ide2": "tank-ssd:vm-300-cloudinit,media=cdrom",
				"net0": "virtio=BC:24:11:00:03:00,bridge=vmbr0", pveConfigKeyBoot: "order=scsi0;net0"},
			Description: (&ownerStamp{owner: otherOwner(), phase: ownerStampPhaseReady}).line()})
		req := stampedCloneReq("300")
		req.SourceOwner = ownerProto(otherOwner())
		req.CustomizeJson = `{"userData":{"cloudInit":{"inline":"#cloud-config\nruncmd: [true]\n"}}}`
		resp, err := p.Clone(context.Background(), req)
		require.NoError(t, err)
		clone := vmByID(t, srv, resp.TargetVmId)
		assert.Equal(t, "order=scsi0;net0", clone.Config[pveConfigKeyBoot], "the source's order, without the seed")
		assertSeedNotInBootOrder(t, clone)
	})

	t.Run("a re-sent Create finishes an order an interrupted one left", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		req := seedCreateReq()
		resp, err := p.Create(context.Background(), req)
		require.NoError(t, err)
		vm := vmByID(t, srv, resp.Id)
		// Interrupted between the seed's `qm set` and the order update.
		vm.Config[pveConfigKeyBoot] = "order=scsi0;ide2"
		vm.Description = testStamp(testOwnerUID, ownerStampPhaseCreating).line()
		srv.PutVM(vm)

		again, err := p.Create(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, resp.Id, again.Id)
		vm = vmByID(t, srv, resp.Id)
		assert.Equal(t, "order=scsi0", vm.Config[pveConfigKeyBoot])
		assert.Len(t, node.seedsOf(vm.VMID), 1, "no second seed")
	})
}

// TestSeedFreeBootOrder: the seed slots leave the order list; the other
// devices the config has keep their order; an order left empty names the
// boot disk, else the first NIC; a config without an order list is left as
// it is.
func TestSeedFreeBootOrder(t *testing.T) {
	disk := "tank:vm-1-disk-0,size=8G"
	seed := "tank:vm-1-disk-1,media=cdrom"
	nic := "virtio=AA:BB:CC:00:00:01,bridge=vmbr0"
	for name, tc := range map[string]struct {
		cfg     map[string]interface{}
		seeds   []string
		want    string
		changed bool
	}{
		"appended seed": {map[string]interface{}{"boot": "order=scsi0;ide2", "scsi0": disk, "ide2": seed}, []string{"ide2"}, "order=scsi0", true},
		"seed in the middle": {map[string]interface{}{"boot": "order=scsi0;ide2;net0", "scsi0": disk, "ide2": seed, "net0": nic},
			[]string{"ide2"}, "order=scsi0;net0", true},
		"no seed in the order":   {map[string]interface{}{"boot": "order=scsi0", "scsi0": disk, "ide2": seed}, []string{"ide2"}, "", false},
		"no seed":                {map[string]interface{}{"boot": "order=scsi0;ide2", "scsi0": disk, "ide2": "local:iso/x.iso,media=cdrom"}, nil, "", false},
		"diskless":               {map[string]interface{}{"boot": "order=net0;ide2", "net0": nic, "ide2": seed}, []string{"ide2"}, "order=net0", true},
		"only the seed: disk":    {map[string]interface{}{"boot": "order=ide2", "virtio0": disk, "ide2": seed}, []string{"ide2"}, "order=virtio0", true},
		"only the seed: NIC":     {map[string]interface{}{"boot": "order=ide2", "net3": nic, "net1": nic, "ide2": seed}, []string{"ide2"}, "order=net1", true},
		"only the seed: nothing": {map[string]interface{}{"boot": "order=ide2", "ide2": seed}, []string{"ide2"}, "", false},
		"a missing device is left out": {map[string]interface{}{"boot": "order=scsi0;net0;ide2", "scsi0": disk, "ide2": seed},
			[]string{"ide2"}, "order=scsi0", true},
		"legacy boot value": {map[string]interface{}{"boot": "cdn", "scsi0": disk, "ide2": seed}, []string{"ide2"}, "", false},
		"no boot value":     {map[string]interface{}{"scsi0": disk, "ide2": seed}, []string{"ide2"}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, changed := seedFreeBootOrder(tc.cfg, tc.seeds)
			assert.Equal(t, tc.changed, changed)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSeedEmptyUserData_IsNotAnEmptyCloudConfig pins rc1 finding O1: the
// user-data of a clone's seed without VMClone user-data is a #cloud-config
// whose YAML body is a mapping. cloud-init loads the body with
// yaml.safe_load and reports "empty cloud config" (a WARNING, `cloud-init
// status` exit 2) when that yields no document; an empty mapping is a
// document that configures nothing.
func TestSeedEmptyUserData_IsNotAnEmptyCloudConfig(t *testing.T) {
	require.True(t, strings.HasPrefix(seedEmptyUserData, "#cloud-config\n"), "cloud-init recognises the part by its first line")
	var body interface{}
	require.NoError(t, yaml.Unmarshal([]byte(seedEmptyUserData), &body))
	mapping, ok := body.(map[string]interface{})
	require.True(t, ok, "the body is a YAML mapping, not an empty document: %#v", body)
	assert.Empty(t, mapping, "it configures nothing")

	var empty interface{}
	require.NoError(t, yaml.Unmarshal([]byte("#cloud-config\n"), &empty))
	assert.Nil(t, empty, "the rc1 value was an empty document")
}

// TestDelete_RemovesTheSeedLock pins rc1 finding O5 on the provider side: a
// Delete that destroyed the VM runs the lock removal for its VMID on the
// node; a removal that fails does not fail the Delete.
func TestDelete_RemovesTheSeedLock(t *testing.T) {
	p, srv, node, _ := seedTestProvider(t)
	resp, err := p.Create(context.Background(), seedCreateReq())
	require.NoError(t, err)
	vmid := vmByID(t, srv, resp.Id).VMID
	_, err = p.Delete(context.Background(), &providerv1.DeleteRequest{Id: resp.Id, Owner: ownerProto(testOwner())})
	require.NoError(t, err)
	assert.Contains(t, node.allCommands(), seedLockRemoveScript(seedStageDir, vmid))

	resp, err = p.Create(context.Background(), seedCreateReq())
	require.NoError(t, err)
	vmid = vmByID(t, srv, resp.Id).VMID
	node.delegate = func(context.Context, string) (string, string, error) {
		return "", "", exitErr(t, sshExitConnectionFailed)
	}
	_, err = p.Delete(context.Background(), &providerv1.DeleteRequest{Id: resp.Id, Owner: ownerProto(testOwner())})
	require.NoError(t, err, "a failed lock removal never fails the Delete")
	_, exists := srv.GetVM(vmid)
	assert.False(t, exists)
}
