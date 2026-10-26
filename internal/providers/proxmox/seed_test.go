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
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These tests pin R35: a Proxmox VM's cloud-init user-data reaches the guest
// whole, through a NoCloud seed CD-ROM the provider builds on the node
// (seed.go), validated against pvefake and a fake node (seedNode); the seed
// script itself runs against a local shell in seed_script_test.go.

// seedSecretMarker is a password in the test user-data: it must never reach a
// log, an error, a command line or a PVE config value.
const seedSecretMarker = "S3CR3T-MARKER-PASSWORD"

// seedUserData has what the line-scan parser used to drop or merge: two users
// with their own keys (one as a flow list), sudo, write_files, runcmd,
// packages and chpasswd.
var seedUserData = []byte(`#cloud-config
users:
  - name: alice
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIalice alice@example.com
  - name: bob
    ssh_authorized_keys: [ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIbob bob@example.com]
packages: [nginx]
write_files:
  - path: /etc/motd
    content: |
      managed by VirtRigaud
runcmd:
  - [sh, -c, "echo done > /var/tmp/done"]
chpasswd:
  expire: false
  users:
    - {name: alice, password: ` + seedSecretMarker + `, type: text}
`)

// seedTemplateMAC is the MAC of the seed tests' template's net0.
const seedTemplateMAC = "AA:BB:CC:00:00:01"

// seedTestProvider is a provider on a fake PVE (tasks end at once) whose
// template 9000 has a boot disk and one NIC, with an SSH data plane to a fake
// node that builds seeds, and its logs in the returned buffer.
func seedTestProvider(t *testing.T) (*Provider, *pvefake.Server, *seedNode, *bytes.Buffer) {
	t.Helper()
	p, srv := newStampTestProvider(t)
	tmpl, ok := srv.GetVM(seededTemplateID)
	require.True(t, ok)
	tmpl.Config = map[string]string{
		"scsi0": "tank-ssd:vm-9000-disk-0,size=32G",
		"net0":  "virtio=" + seedTemplateMAC + ",bridge=vmbr0",
	}
	srv.PutVM(tmpl)
	node := newSeedNode(t, srv)
	p.ssh = node.transport()
	logs := &bytes.Buffer{}
	p.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return p, srv, node, logs
}

// seedCreateReq is a Create for testOwner() from the template with
// seedUserData.
func seedCreateReq() *providerv1.CreateRequest {
	req := createReq(testOwner(), templateImageJSON)
	req.UserData = seedUserData
	return req
}

// noSecrets asserts that none of texts carries the user-data's secret.
func noSecrets(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		assert.NotContains(t, s, seedSecretMarker)
		assert.NotContains(t, s, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIalice")
	}
}

// TestSeed_UserDataReachesTheSeedByteForByte: the whole user-data is the
// seed's user-data, unchanged, and none of PVE's cloud-init keys or drives is
// set beside the seed; the template's boot disk stays the only boot device.
func TestSeed_UserDataReachesTheSeedByteForByte(t *testing.T) {
	p, srv, node, logs := seedTestProvider(t)
	resp, err := p.Create(context.Background(), seedCreateReq())
	require.NoError(t, err)
	vm := vmByID(t, srv, resp.Id)

	seeds := node.seedsOf(vm.VMID)
	require.Len(t, seeds, 1)
	assert.Equal(t, seedUserData, seeds[0].files[seedFileUserData])
	assert.Equal(t, "tank-ssd", seeds[0].storage, "the seed follows the boot disk's storage")
	assert.Equal(t, cloudInitDefaultSlot, seeds[0].slot)
	assertSeed(t, vm.Config["ide2"], "tank-ssd")
	assert.Equal(t, "order=scsi0", vm.Config[pveConfigKeyBoot], "the seed is not in the boot order")

	cfg := configOf(t, p, vm.VMID)
	for _, key := range []string{"ciuser", "cipassword", "sshkeys", "ipconfig0", "nameserver", "cicustom"} {
		assert.NotContains(t, cfg, key, "no PVE cloud-init key %s beside the seed", key)
	}
	_, _, hasDrive := pveapi.CloudInitDrive(cfg)
	assert.False(t, hasDrive, "no PVE cloud-init drive beside the seed")
	assert.Equal(t, []string{seeds[0].volume}, srv.Volumes())

	ready := stampOf(t, vm)
	assert.Equal(t, ownerStampPhaseReady, ready.phase)

	var configValues []string
	for _, v := range cfg {
		if s, ok := v.(string); ok {
			configValues = append(configValues, s)
		}
	}
	noSecrets(t, append(append(configValues, logs.String()), node.allCommands()...)...)
}

// TestSeed_MetaData: the provider renders the meta-data: instance-id from the
// owner VirtualMachine's UID (the same for a re-sent Create, new for any other
// VirtualMachine) and local-hostname from the VM name. A request meta-data's
// other keys are kept; its instance-id and local-hostname are the provider's.
// Without an owner UID the instance-id names the VMID.
func TestSeed_MetaData(t *testing.T) {
	read := func(t *testing.T, data []byte) map[string]interface{} {
		t.Helper()
		out := map[string]interface{}{}
		require.NoError(t, yaml.Unmarshal(data, &out))
		return out
	}

	t.Run("rendered", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		resp, err := p.Create(context.Background(), seedCreateReq())
		require.NoError(t, err)
		meta := read(t, node.seedsOf(vmByID(t, srv, resp.Id).VMID)[0].files[seedFileMetaData])
		assert.Equal(t, map[string]interface{}{"instance-id": "virtrigaud-" + testOwnerUID, "local-hostname": testOwnerName}, meta)
	})

	t.Run("merged with the request's", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		req := seedCreateReq()
		req.MetaData = []byte("instance-id: chosen-by-user\nlocal-hostname: other\npublic-keys:\n  - ssh-ed25519 AAAAkey\nregion: lab\n")
		resp, err := p.Create(context.Background(), req)
		require.NoError(t, err)
		meta := read(t, node.seedsOf(vmByID(t, srv, resp.Id).VMID)[0].files[seedFileMetaData])
		assert.Equal(t, "virtrigaud-"+testOwnerUID, meta["instance-id"])
		assert.Equal(t, testOwnerName, meta["local-hostname"])
		assert.Equal(t, "lab", meta["region"])
		assert.Equal(t, []interface{}{"ssh-ed25519 AAAAkey"}, meta["public-keys"])
	})

	t.Run("no owner UID", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		req := seedCreateReq()
		req.Owner = nil
		req.Name = "plain"
		resp, err := p.Create(context.Background(), req)
		require.NoError(t, err)
		vmid := vmByID(t, srv, resp.Id).VMID
		meta := read(t, node.seedsOf(vmid)[0].files[seedFileMetaData])
		assert.Equal(t, "virtrigaud-pve-"+strconv.Itoa(vmid), meta["instance-id"])
		assert.Equal(t, "plain", meta["local-hostname"])
	})

	t.Run("not a mapping: refused before anything is made", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		req := seedCreateReq()
		req.MetaData = []byte("- " + seedSecretMarker + "\n- b\n")
		_, err := p.Create(context.Background(), req)
		assert.Equal(t, classInvalidSpec, classOf(t, err))
		assert.Equal(t, seedMetaDataInvalidMessage, status.Convert(err).Message())
		assert.Empty(t, srv.MutatingRequests())
		assert.Empty(t, node.allCommands())
	})
}

// TestRenderNetworkConfig: one version-2 entry per IP setting, matched by its
// NIC's MAC and named eth<N> as PVE's own nocloud network data names it.
func TestRenderNetworkConfig(t *testing.T) {
	cfg := map[string]interface{}{
		"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0",
		"net1": "e1000,bridge=vmbr1,macaddr=BC:24:11:00:00:02",
		"net2": "virtio,bridge=vmbr2",
	}
	out, skipped, err := renderNetworkConfig([]pveapi.IPConfig{
		{Index: 0, IP: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: "192.0.2.53"},
		{Index: 1, IP: "2001:db8::10/64", Gateway: "2001:db8::1"},
		{Index: 2, DHCP: true},
		{Index: 3, DHCP: true},
	}, cfg)
	require.NoError(t, err)
	assert.Equal(t, []int{2, 3}, skipped, "a NIC without a MAC, a NIC the VM lacks")
	var doc nocloudNetwork
	require.NoError(t, yaml.Unmarshal(out, &doc))
	dns := &nocloudNameservers{Addresses: []string{"192.0.2.53"}}
	assert.Equal(t, nocloudNetwork{Version: 2, Ethernets: map[string]nocloudEthernet{
		"eth0": {Match: nocloudMatch{MACAddress: "bc:24:11:00:00:01"}, SetName: "eth0", Addresses: []string{"192.0.2.10/24"},
			Routes: []nocloudRoute{{To: "0.0.0.0/0", Via: "192.0.2.1"}}, Nameservers: dns},
		"eth1": {Match: nocloudMatch{MACAddress: "bc:24:11:00:00:02"}, SetName: "eth1", Addresses: []string{"2001:db8::10/64"},
			Routes: []nocloudRoute{{To: "::/0", Via: "2001:db8::1"}}, Nameservers: dns},
	}}, doc)

	none, _, err := renderNetworkConfig([]pveapi.IPConfig{{Index: 0, DHCP: true}}, map[string]interface{}{})
	require.NoError(t, err)
	assert.Nil(t, none, "no NIC: no network-config (cloud-init's fallback)")
}

// TestSeed_NoUserDataPathUnchanged: a Create without user-data runs nothing on
// the node and gets no seed: PVE's native path as before.
func TestSeed_NoUserDataPathUnchanged(t *testing.T) {
	p, srv, node, _ := seedTestProvider(t)
	resp, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	require.NoError(t, err)
	vm := vmByID(t, srv, resp.Id)
	assert.Empty(t, node.allCommands())
	assert.Empty(t, vm.Config["ide2"])
	assert.Empty(t, srv.Volumes())
	// The changes the Create makes are the clone (carrying the owner stamp), the
	// VMClass sizing and the ready stamp, as before R35.
	var changes []string
	for _, r := range srv.MutatingRequests() {
		changes = append(changes, r.Method+" "+r.Path)
	}
	id := strconv.Itoa(vm.VMID)
	assert.Equal(t, []string{
		"POST /api2/json/nodes/pve/qemu/" + strconv.Itoa(seededTemplateID) + "/clone",
		"PUT /api2/json/nodes/pve/qemu/" + id + "/config",
		"PUT /api2/json/nodes/pve/qemu/" + id + "/config",
	}, changes)
}

// TestSeed_MissingPrerequisitesAreDefinite: user-data the provider cannot
// apply is refused before anything is created, with a non-retryable error
// naming what is missing; never a VM silently without its user-data.
func TestSeed_MissingPrerequisitesAreDefinite(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(p *Provider, node *seedNode)
		want   string
	}{
		"no SSH data plane":  {func(p *Provider, _ *seedNode) { p.ssh = nil }, seedNoSSHMessage},
		"no SSH credentials": {func(p *Provider, _ *seedNode) { p.ssh.creds = sshCredentials{User: "root"} }, seedNoSSHCredentialsMessage},
		"no known_hosts":     {func(p *Provider, _ *seedNode) { p.ssh.hostKey = proxmoxHostKeyPolicy{} }, seedNoKnownHostsMessage},
		"no genisoimage":     {func(_ *Provider, node *seedNode) { node.noGenisoimage = true }, seedNoGenisoimageMessage},
	} {
		t.Run(name, func(t *testing.T) {
			p, srv, node, logs := seedTestProvider(t)
			tc.mutate(p, node)
			_, err := p.Create(context.Background(), seedCreateReq())
			assert.Equal(t, classInvalidSpec, classOf(t, err))
			assert.Equal(t, tc.want, status.Convert(err).Message())
			assert.NotContains(t, err.Error(), "pve.example.com", "no host name on the wire")
			assert.Empty(t, srv.MutatingRequests(), "nothing is created")
			noSecrets(t, logs.String(), err.Error())
		})
	}
}

// TestSeed_TooLargeIsRefused: user-data above 1 MiB is refused up front.
func TestSeed_TooLargeIsRefused(t *testing.T) {
	p, srv, _, _ := seedTestProvider(t)
	req := seedCreateReq()
	req.UserData = bytes.Repeat([]byte("a"), maxSeedFileBytes+1)
	_, err := p.Create(context.Background(), req)
	assert.Equal(t, classInvalidSpec, classOf(t, err))
	assert.Empty(t, srv.MutatingRequests())
}

// TestSeed_FailedBuildRollsBack: a seed build that failed on the node (its own
// exit status) removes the VM, which takes any seed volume with it; one whose
// SSH session broke, or that found a `qm importdisk` of the VM still running,
// may still be going on: the VM is kept and the outcome is unknown.
func TestSeed_FailedBuildRollsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		exit     int
		wantKept bool
		want     errorClass
	}{
		"build failed":              {1, false, classNothingRemainsVM},
		"import unclear":            {seedExitImportUnclear, false, classNothingRemainsVM},
		"ssh broke":                 {sshExitConnectionFailed, true, classOutcomeUnknown},
		"importdisk still running":  {seedExitImportBusy, true, classOutcomeUnknown},
		"genisoimage gone mid-flow": {seedExitNoGenisoimage, false, classInvalidSpec},
		"VM on another node":        {seedExitNotOnNode, false, classInvalidSpec},
		"qm importdisk failed":      {seedExitQMImport, false, classNothingRemainsVM},
	} {
		t.Run(name, func(t *testing.T) {
			p, srv, node, logs := seedTestProvider(t)
			node.seedExit = tc.exit
			_, err := p.Create(context.Background(), seedCreateReq())
			assert.Equal(t, tc.want, classOf(t, err))
			if tc.wantKept {
				assert.Len(t, srv.VMsNamed(testOwnerName), 1, "the VM is kept while the build may still run")
			} else {
				assert.Empty(t, srv.VMsNamed(testOwnerName), "the VM is removed")
			}
			assert.Empty(t, srv.Volumes())
			noSecrets(t, logs.String(), err.Error())
		})
	}
}

// TestSeed_DeleteFreesTheSeed: Delete, and a rollback after the seed was
// attached, free the seed volume with the VM (destroy-unreferenced-disks);
// the fake, like qemu-server, never frees a CD-ROM's volume otherwise.
func TestSeed_DeleteFreesTheSeed(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		p, srv, _, _ := seedTestProvider(t)
		resp, err := p.Create(context.Background(), seedCreateReq())
		require.NoError(t, err)
		require.Len(t, srv.Volumes(), 1)
		_, err = p.Delete(context.Background(), &providerv1.DeleteRequest{Id: resp.Id, Owner: ownerProto(testOwner())})
		require.NoError(t, err)
		assert.Empty(t, srv.Volumes(), "the seed is removed with the VM")
	})

	t.Run("rollback after the seed is attached", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		// Marking the VM ready (a config update) fails after the seed is
		// attached; the boot order update before it goes through.
		node.afterSeed = func() {
			srv.InjectFault(pvefake.Fault{Method: http.MethodPut, PathContains: "/config", PassThrough: true, Times: 1})
			srv.InjectFault(pvefake.Fault{Method: http.MethodPut, PathContains: "/config", Status: http.StatusInternalServerError, Body: "boom"})
		}
		_, err := p.Create(context.Background(), seedCreateReq())
		require.Error(t, err)
		assert.Empty(t, srv.VMsNamed(testOwnerName))
		assert.Empty(t, srv.Volumes(), "the rollback removes the seed with the VM")
	})

	t.Run("a destroy without destroy-unreferenced-disks keeps a CD-ROM's volume", func(t *testing.T) {
		p, srv, _, _ := seedTestProvider(t)
		resp, err := p.Create(context.Background(), seedCreateReq())
		require.NoError(t, err)
		vmid, _ := strconv.Atoi(resp.Id)
		task, err := p.client.DeleteVM(context.Background(), "pve", vmid, false)
		require.NoError(t, err)
		require.NoError(t, p.client.WaitForTask(context.Background(), "pve", task))
		assert.Len(t, srv.Volumes(), 1, "why Delete passes destroy-unreferenced-disks")
	})
}

// TestSeed_ResentCreateAttachesNoSecondSeed: a Create re-sent for a VM that is
// ready, or that was interrupted after its seed was attached, never gets a
// second seed; one interrupted before gets exactly one.
func TestSeed_ResentCreateAttachesNoSecondSeed(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		first, err := p.Create(context.Background(), seedCreateReq())
		require.NoError(t, err)
		second, err := p.Create(context.Background(), seedCreateReq())
		require.NoError(t, err)
		assert.Equal(t, first.Id, second.Id)
		assert.Len(t, node.seedsOf(vmByID(t, srv, first.Id).VMID), 1)
		assert.Len(t, srv.Volumes(), 1)
	})

	for name, imageJSON := range map[string]string{"template clone": templateImageJSON, "new VM": ""} {
		t.Run("interrupted after the seed: "+name, func(t *testing.T) {
			p, srv, node, _ := seedTestProvider(t)
			req := seedCreateReq()
			req.ImageJson = imageJSON
			resp, err := p.Create(context.Background(), req)
			require.NoError(t, err)
			vmid := vmByID(t, srv, resp.Id).VMID
			srv.SetDescription(vmid, testStamp(testOwnerUID, ownerStampPhaseCreating).line())

			again, err := p.Create(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, resp.Id, again.Id)
			assert.Len(t, node.seedsOf(vmid), 1, "no second seed")
			assert.Equal(t, ownerStampPhaseReady, stampOf(t, vmByID(t, srv, resp.Id)).phase)
		})
	}

	t.Run("interrupted before the seed: new VM", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		const halfBuilt = 4960
		srv.PutVM(pvefake.VM{VMID: halfBuilt, Name: testOwnerName, Node: "pve", Status: "stopped",
			Config:      map[string]string{"net0": "virtio=BC:24:11:00:13:60,bridge=vmbr0"},
			Description: testStamp(testOwnerUID, ownerStampPhaseCreating).line()})
		req := seedCreateReq()
		req.ImageJson = ""
		resp, err := p.Create(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(halfBuilt), resp.Id)
		seeds := node.seedsOf(halfBuilt)
		require.Len(t, seeds, 1)
		assert.Contains(t, string(seeds[0].files[seedFileNetworkConfig]), "bc:24:11:00:13:60")
	})
}

// TestSeed_NewVM: a VM made without a template gets its seed once its create
// task has ended, with a network-config for the MACs PVE gave its NICs, and
// none of PVE's cloud-init keys.
func TestSeed_NewVM(t *testing.T) {
	p, srv, node, _ := seedTestProvider(t)
	srv.SetAssignCreateMACs(true)
	srv.SetTaskDelay(0)
	p.client.Config().DefaultStorage = "vmstore"
	req := seedCreateReq()
	req.ImageJson = ""
	resp, err := p.Create(context.Background(), req)
	require.NoError(t, err)
	assert.Nil(t, resp.Task, "the VM is complete when Create answers")
	vm := vmByID(t, srv, resp.Id)
	assertSeed(t, vm.Config["ide2"], "vmstore")
	assert.NotContains(t, vm.Config, "ipconfig0")
	seeds := node.seedsOf(vm.VMID)
	require.Len(t, seeds, 1)
	assert.Contains(t, string(seeds[0].files[seedFileNetworkConfig]), strings.ToLower(pvefake.CloneMAC(vm.VMID, 0)))
	assert.Equal(t, ownerStampPhaseReady, stampOf(t, vm).phase)
}

// seededSource creates a VM with seedUserData for otherOwner() and returns
// its id: a VMClone source with a seed.
func seededSource(t *testing.T, p *Provider) string {
	t.Helper()
	req := createReq(otherOwner(), templateImageJSON)
	req.UserData = seedUserData
	resp, err := p.Create(context.Background(), req)
	require.NoError(t, err)
	return resp.Id
}

// TestSeed_CloneGetsItsOwnSeed: PVE's clone copies its source's seed CD-ROM
// line. The clone gets a seed of its own in that slot, with a new
// instance-id (the target VirtualMachine's UID), the target's hostname, the
// VMClone's customization user-data or a cloud-config that configures
// nothing (never the
// source's user-data), DHCP on its own MACs; the source's seed is untouched,
// a re-sent Clone attaches no second seed, and deleting the clone keeps the
// source's seed.
func TestSeed_CloneGetsItsOwnSeed(t *testing.T) {
	for name, tc := range map[string]struct {
		customize    string
		wantUserData string
		wantHost     string
	}{
		"no customization":      {"", seedEmptyUserData, cloneTargetName},
		"customization":         {`{"hostname":"web-2","domain":"example.com","userData":{"cloudInit":{"inline":"#cloud-config\nruncmd: [true]\n"}}}`, "#cloud-config\nruncmd: [true]\n", "web-2.example.com"},
		"secret-ref user-data":  {`{"userData":{"cloudInit":{"secretRef":{"name":"ud"}}}}`, seedEmptyUserData, cloneTargetName},
		"unapplied fields only": {`{"timeZone":"UTC"}`, seedEmptyUserData, cloneTargetName},
	} {
		t.Run(name, func(t *testing.T) {
			p, srv, node, logs := seedTestProvider(t)
			srcID := seededSource(t, p)
			src := vmByID(t, srv, srcID)
			srcSeed := src.Config["ide2"]

			req := stampedCloneReq(srcID)
			req.SourceOwner = ownerProto(otherOwner())
			req.CustomizeJson = tc.customize
			resp, err := p.Clone(context.Background(), req)
			require.NoError(t, err)
			clone := vmByID(t, srv, resp.TargetVmId)

			owner, ok := seedDrive(clone.Config["ide2"])
			require.True(t, ok, "the clone has a seed: %q", clone.Config["ide2"])
			assert.Equal(t, clone.VMID, owner, "the clone's own seed volume, not its source's")
			assert.Equal(t, srcSeed, vmByID(t, srv, srcID).Config["ide2"], "the source's seed is untouched")

			seeds := node.seedsOf(clone.VMID)
			require.Len(t, seeds, 1)
			assert.Equal(t, tc.wantUserData, string(seeds[0].files[seedFileUserData]))
			meta := map[string]interface{}{}
			require.NoError(t, yaml.Unmarshal(seeds[0].files[seedFileMetaData], &meta))
			assert.Equal(t, "virtrigaud-"+testOwnerUID, meta["instance-id"])
			assert.NotEqual(t, "virtrigaud-"+testOtherUID, meta["instance-id"])
			assert.Equal(t, tc.wantHost, meta["local-hostname"])
			network := string(seeds[0].files[seedFileNetworkConfig])
			assert.Contains(t, network, strings.ToLower(pvefake.CloneMAC(clone.VMID, 0)))
			assert.Contains(t, network, "dhcp4: true")

			// A re-sent Clone attaches no second seed.
			again, err := p.Clone(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, resp.TargetVmId, again.TargetVmId)
			assert.Len(t, node.seedsOf(clone.VMID), 1)

			// Deleting the clone frees its seed, not the source's.
			_, err = p.Delete(context.Background(), &providerv1.DeleteRequest{Id: resp.TargetVmId, Owner: ownerProto(cloneTargetOwnerID())})
			require.NoError(t, err)
			srcVolume, _, _ := strings.Cut(srcSeed, ",")
			assert.Equal(t, []string{srcVolume}, srv.Volumes())
			noSecrets(t, logs.String(), string(seeds[0].files[seedFileUserData]))
		})
	}
}

// TestSeed_CloneWithoutSourceSeed: a clone of a VM without a seed runs
// nothing on the node and reads nothing more (R26: PVE regenerates its own
// drive), unless the VMClone gives inline user-data: that reaches the guest
// through a seed of the clone's own, which replaces the PVE cloud-init drive
// the clone has.
func TestSeed_CloneWithoutSourceSeed(t *testing.T) {
	t.Run("no customization user-data", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		req := stampedCloneReq(seededCloneSource(srv))
		req.SourceOwner = ownerProto(otherOwner())
		_, err := p.Clone(context.Background(), req)
		require.NoError(t, err)
		assert.Empty(t, node.allCommands())
	})

	t.Run("customization user-data", func(t *testing.T) {
		p, srv, node, _ := seedTestProvider(t)
		srv.PutVM(pvefake.VM{VMID: 300, Name: "web", Status: "stopped", Node: "pve", CPUs: 1,
			Config:      map[string]string{"scsi0": "tank-ssd:vm-300-disk-0", "ide2": "tank-ssd:vm-300-cloudinit,media=cdrom", "net0": "virtio=BC:24:11:00:03:00,bridge=vmbr0"},
			Description: (&ownerStamp{owner: otherOwner(), phase: ownerStampPhaseReady}).line()})
		req := stampedCloneReq("300")
		req.SourceOwner = ownerProto(otherOwner())
		req.CustomizeJson = `{"userData":{"cloudInit":{"inline":"#cloud-config\nruncmd: [true]\n"}}}`
		resp, err := p.Clone(context.Background(), req)
		require.NoError(t, err)
		clone := vmByID(t, srv, resp.TargetVmId)
		seeds := node.seedsOf(clone.VMID)
		require.Len(t, seeds, 1)
		assert.Equal(t, "#cloud-config\nruncmd: [true]\n", string(seeds[0].files[seedFileUserData]))
		assertSeed(t, clone.Config["ide2"], "tank-ssd")
		_, _, hasDrive := pveapi.CloudInitDrive(configOf(t, p, clone.VMID))
		assert.False(t, hasDrive, "the seed replaced the clone's PVE cloud-init drive")
	})
}

// TestSeed_NodeMismatchIsDefinite: the SSH data plane reaching another node
// than the VM's is refused before anything is created or cloned.
func TestSeed_NodeMismatchIsDefinite(t *testing.T) {
	p, srv, node, _ := seedTestProvider(t)
	node.nodeName = "pve2"
	_, err := p.Create(context.Background(), seedCreateReq())
	assert.Equal(t, classInvalidSpec, classOf(t, err))
	assert.Equal(t, seedNotOnNodeMessage, status.Convert(err).Message())
	assert.Empty(t, srv.MutatingRequests())
}

// TestSeed_TemplateWithASeedIsNotShared: a template converted from a VM with
// a seed gives its clones a CD-ROM that references the template's seed. A
// Create without user-data detaches it (the clone must not boot with the
// template's user-data and instance-id); one with user-data replaces it with
// the VM's own seed. The template's volume is never freed.
func TestSeed_TemplateWithASeedIsNotShared(t *testing.T) {
	setup := func(t *testing.T) (*Provider, *pvefake.Server, *seedNode) {
		p, srv, node, _ := seedTestProvider(t)
		tmpl, ok := srv.GetVM(seededTemplateID)
		require.True(t, ok)
		tmpl.Config["ide2"] = "tank-ssd:vm-9000-disk-1,media=cdrom"
		srv.PutVM(tmpl)
		return p, srv, node
	}
	t.Run("without user-data", func(t *testing.T) {
		p, srv, node := setup(t)
		resp, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
		require.NoError(t, err)
		vm := vmByID(t, srv, resp.Id)
		assert.Empty(t, vm.Config["ide2"], "the template's seed is detached")
		assert.Empty(t, node.allCommands())
		tmpl, _ := srv.GetVM(seededTemplateID)
		assert.Equal(t, "tank-ssd:vm-9000-disk-1,media=cdrom", tmpl.Config["ide2"])
	})
	t.Run("with user-data", func(t *testing.T) {
		p, srv, node := setup(t)
		resp, err := p.Create(context.Background(), seedCreateReq())
		require.NoError(t, err)
		vm := vmByID(t, srv, resp.Id)
		owner, ok := seedDrive(vm.Config["ide2"])
		require.True(t, ok)
		assert.Equal(t, vm.VMID, owner, "the VM's own seed")
		assert.Len(t, node.seedsOf(vm.VMID), 1)
	})
}

// TestSeed_InterruptedCloneGetsItsOwnSeed: a Clone interrupted before its
// seed was built ("creating", still referencing its source's seed) gets its
// own seed when it is re-sent; one interrupted after gets no second one.
func TestSeed_InterruptedCloneGetsItsOwnSeed(t *testing.T) {
	p, srv, node, _ := seedTestProvider(t)
	srcID := seededSource(t, p)
	src := vmByID(t, srv, srcID)
	const halfCloned = 4970
	srv.PutVM(pvefake.VM{VMID: halfCloned, Name: cloneTargetName, Node: "pve", Status: "stopped",
		Config:      map[string]string{"scsi0": "tank-ssd:vm-4970-disk-0", "ide2": src.Config["ide2"], "net0": "virtio=BC:24:11:00:49:70,bridge=vmbr0"},
		Description: (&ownerStamp{owner: cloneTargetOwnerID(), phase: ownerStampPhaseCreating}).line()})
	req := stampedCloneReq(srcID)
	req.SourceOwner = ownerProto(otherOwner())
	resp, err := p.Clone(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(halfCloned), resp.TargetVmId)
	owner, ok := seedDrive(vmByID(t, srv, resp.TargetVmId).Config["ide2"])
	require.True(t, ok)
	assert.Equal(t, halfCloned, owner)
	assert.Equal(t, src.Config["ide2"], vmByID(t, srv, srcID).Config["ide2"], "the source's seed is untouched")

	srv.SetDescription(halfCloned, (&ownerStamp{owner: cloneTargetOwnerID(), phase: ownerStampPhaseCreating}).line())
	_, err = p.Clone(context.Background(), req)
	require.NoError(t, err)
	assert.Len(t, node.seedsOf(halfCloned), 1, "no second seed")
}

// TestSeed_CloneOfSeededSourceNeedsSSH: a clone that would need its own seed
// is refused before anything is cloned when the provider cannot build one.
func TestSeed_CloneOfSeededSourceNeedsSSH(t *testing.T) {
	p, srv, _, _ := seedTestProvider(t)
	srcID := seededSource(t, p)
	before := len(srv.MutatingRequests())
	p.ssh = nil
	req := stampedCloneReq(srcID)
	req.SourceOwner = ownerProto(otherOwner())
	_, err := p.Clone(context.Background(), req)
	assert.Equal(t, classInvalidSpec, classOf(t, err))
	assert.Equal(t, seedNoSSHMessage, status.Convert(err).Message())
	assert.Len(t, srv.MutatingRequests(), before, "nothing is cloned")
}

// TestSeed_ImportedDiskGetsASeed: a migration's imported-disk create with
// user-data gets a seed on the import's storage, after its boot disk, and no
// PVE cloud-init key.
func TestSeed_ImportedDiskGetsASeed(t *testing.T) {
	p, srv, node, _ := seedTestProvider(t)
	rec := &recordingNode{}
	node.delegate = rec.run
	req := createReq(testOwner(), ownStageImageJSON)
	req.UserData = seedUserData
	resp, err := p.Create(context.Background(), req)
	require.NoError(t, err)
	vm := vmByID(t, srv, resp.Id)
	seeds := node.seedsOf(vm.VMID)
	require.Len(t, seeds, 1)
	assert.Equal(t, proxmoxImportTargetStorage, seeds[0].storage)
	assert.Equal(t, seedUserData, seeds[0].files[seedFileUserData])
	assert.NotContains(t, vm.Config, "ipconfig0")
	importAt, seedAt := -1, -1
	for i, cmd := range node.allCommands() {
		switch {
		case strings.Contains(cmd, "qm set ") && strings.Contains(cmd, "--scsi0"):
			importAt = i
		case strings.Contains(cmd, `qm importdisk "$VMID" "$W/seed.iso"`):
			seedAt = i
		}
	}
	assert.True(t, importAt >= 0 && seedAt > importAt, "the seed is attached after the boot disk")
}

// TestClone_LinkedIsRefused: the provider makes full clones only, does not
// advertise linked clones, and refuses a linked Clone before anything is done.
func TestClone_LinkedIsRefused(t *testing.T) {
	p, srv := newStampTestProvider(t)
	_, err := p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "9000", TargetName: "linked", Linked: true})
	assert.Equal(t, classInvalidSpec, classOf(t, err))
	assert.Equal(t, linkedCloneUnsupportedMessage, status.Convert(err).Message())
	assert.Empty(t, srv.MutatingRequests())

	caps, err := p.GetCapabilities(context.Background(), &providerv1.GetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.False(t, caps.SupportsLinkedClones)
}

// TestSeedDrive: what counts as a seed CD-ROM and who owns it.
func TestSeedDrive(t *testing.T) {
	for value, want := range map[string]int{
		"local-lvm:vm-101-disk-1,media=cdrom":         101,
		"dirst:101/vm-101-disk-2.raw,media=cdrom":     101,
		"local-lvm:vm-7-disk-0,media=cdrom,size=4M":   7,
		"local-lvm:vm-101-disk-1":                     -1,
		"local-lvm:vm-101-cloudinit,media=cdrom":      -1,
		"local:iso/ubuntu.iso,media=cdrom":            -1,
		"none,media=cdrom":                            -1,
		"bad storage:vm-101-disk-1,media=cdrom":       -1,
		"local-lvm:base-101-disk-1,media=cdrom":       -1,
		"local-lvm:vm-101-disk-1.qcow2x,media=cdrom":  -1,
		"local-lvm:../vm-101-disk-1,media=cdrom":      -1,
		"local-lvm:vm-101-disk-1,media=disk":          -1,
		"local-lvm:vm-101-disk-1,MEDIA=CDROM":         101,
		"local-lvm:vm-101-disk-1, media=cdrom":        101,
		"local-lvm:vm-101-disk-1,media=cdrom,ro=1":    101,
		"local-lvm:vm-1a-disk-1,media=cdrom":          -1,
		"":                                            -1,
		"local-lvm:vm-101-disk-1,size=4M,media=cdrom": 101,
	} {
		owner, ok := seedDrive(value)
		if want < 0 {
			assert.False(t, ok, value)
			continue
		}
		assert.True(t, ok, value)
		assert.Equal(t, want, owner, value)
	}
}

// TestSeedSlotFor: a referenced foreign seed's slot, else the PVE cloud-init
// drive's, else the first free IDE/SATA slot, ide2 first.
func TestSeedSlotFor(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  map[string]interface{}
		want string
		ok   bool
	}{
		"empty":          {map[string]interface{}{}, "ide2", true},
		"iso in ide2":    {map[string]interface{}{"ide2": "local:iso/x.iso,media=cdrom"}, "ide0", true},
		"cloud-init":     {map[string]interface{}{"ide2": "local:iso/x.iso,media=cdrom", "sata3": "st:vm-5-cloudinit,media=cdrom"}, "sata3", true},
		"foreign seed":   {map[string]interface{}{"ide2": "st:vm-4-disk-1,media=cdrom", "sata3": "st:vm-5-cloudinit,media=cdrom"}, "ide2", true},
		"all ide taken":  {map[string]interface{}{"ide0": "a:b", "ide1": "a:b", "ide2": "a:b", "ide3": "a:b"}, "sata0", true},
		"nothing free":   {allSlotsTaken(), "", false},
		"empty ide2 key": {map[string]interface{}{"ide2": ""}, "ide2", true},
	} {
		t.Run(name, func(t *testing.T) {
			slot, ok := seedSlotFor(tc.cfg, 5)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, slot)
		})
	}
}

func allSlotsTaken() map[string]interface{} {
	cfg := map[string]interface{}{}
	for _, s := range seedSlotCandidates {
		cfg[s] = "x:y"
	}
	return cfg
}

// TestBuildSeedScript_NoContentNoUnquotedValues: the script carries no seed
// content, quotes the storage, and its importdisk pattern cannot match its
// own command line.
func TestBuildSeedScript_NoContentNoUnquotedValues(t *testing.T) {
	script := buildSeedScript(seedStageDir, pveQemuConfDir, 4242, "tank-ssd", "ide2", "")
	assert.Contains(t, script, "VMID=4242;")
	assert.Contains(t, script, `"$W/seed.iso" 'tank-ssd' --format raw`)
	assert.Contains(t, script, `qm set "$VMID" --ide2 "$V,media=cdrom"`)
	assert.False(t, regexp.MustCompile(importdiskRunningPattern(4242)).MatchString(script),
		"pgrep -f would match the script's own command line")
	assert.True(t, regexp.MustCompile(importdiskRunningPattern(4242)).MatchString("/usr/bin/perl /usr/sbin/qm importdisk 4242 /x/seed.iso st"),
		"it matches a running qm importdisk of the VM")
	noSecrets(t, script)
}
