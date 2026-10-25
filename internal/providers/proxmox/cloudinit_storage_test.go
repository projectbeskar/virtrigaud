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
	stderrors "errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// These tests pin R33 (v0.4.0-rc4 validation on PVE 9.2): the cloud-init drive
// of a VM with user-data was always "local:cloudinit" for a template clone,
// because the clone path never carried a storage. `local` is a directory
// storage without the `images` content type on a stock PVE, so PVE refused to
// start every such VM. The storage is now the request's/image's, else the one
// the cloned primary disk lives on, else the operator's PVE_DEFAULT_STORAGE.
// They also pin the PR #425 review findings: a clone of a template that has a
// cloud-init drive owns a volume of its own, which PVE cannot allocate again.
// Validated against pvefake only, not a real PVE.

// cloudInitUserData is user data that turns the cloud-init drive on.
var cloudInitUserData = []byte("#cloud-config\nhostname: ci-vm\n")

// cloneTemplateID is the VMID of the template the tests below clone.
const cloneTemplateID = 100

// ciTemplateBootDisk is the template's boot disk (on its own storage).
const ciTemplateBootDisk = "tank-ssd:vm-100-disk-0,size=32G"

// ciProviderWith returns a provider on a fake PVE whose template (VMID 100) has
// the raw config tmplConfig, with defaultStorage as the operator's default,
// and an SSH data plane to a fake node that builds seeds (seedNode).
func ciProviderWith(t *testing.T, defaultStorage string, tmplConfig map[string]string) (*Provider, *pvefake.Server) {
	t.Helper()
	provider, srv, _ := ciProviderNode(t, defaultStorage, tmplConfig)
	return provider, srv
}

// ciProviderNode is ciProviderWith, also returning the fake node.
func ciProviderNode(t *testing.T, defaultStorage string, tmplConfig map[string]string) (*Provider, *pvefake.Server, *seedNode) {
	t.Helper()
	srv, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	tmpl, ok := srv.GetVM(cloneTemplateID)
	require.True(t, ok)
	tmpl.Config = tmplConfig
	srv.PutVM(tmpl)
	srv.SetTaskDelay(time.Millisecond)
	provider := createTestProvider(endpoint)
	provider.client.Config().DefaultStorage = defaultStorage
	provider.client.Config().TaskPollInterval = 5 * time.Millisecond
	node := newSeedNode(t, srv)
	provider.ssh = node.transport()
	return provider, srv, node
}

// assertSeed asserts that value is a seed CD-ROM (a VM's disk volume as
// media=cdrom) on storage.
func assertSeed(t *testing.T, value, storage string) {
	t.Helper()
	assert.Equal(t, storage, pveapi.DiskStorage(value), "storage of %q", value)
	_, ok := seedDrive(value)
	assert.True(t, ok, "a seed CD-ROM: %q", value)
}

// ciProvider is ciProviderWith a template whose only drive is its boot disk.
func ciProvider(t *testing.T, defaultStorage string) (*Provider, *pvefake.Server) {
	t.Helper()
	return ciProviderWith(t, defaultStorage, map[string]string{"scsi0": ciTemplateBootDisk})
}

// ciRequest is a Create of a VM with user data from imageJSON.
func ciRequest(imageJSON string) *providerv1.CreateRequest {
	return &providerv1.CreateRequest{
		Name:      "ci-vm",
		ClassJson: `{"CPU": 2, "MemoryMiB": 2048}`,
		ImageJson: imageJSON,
		UserData:  cloudInitUserData,
	}
}

// createCIVM creates a VM from req and returns its fake VM.
func createCIVM(t *testing.T, provider *Provider, srv *pvefake.Server, req *providerv1.CreateRequest) pvefake.VM {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.Create(ctx, req)
	require.NoError(t, err)
	if resp.Task != nil {
		require.NoError(t, waitForTask(ctx, provider, resp.Task.Id))
	}
	vmid, err := strconv.Atoi(resp.Id)
	require.NoError(t, err)
	vm, ok := srv.GetVM(vmid)
	require.True(t, ok)
	return vm
}

// createCI creates a VM with user data from imageJSON and returns its
// cloud-init drive (ide2) config value.
func createCI(t *testing.T, provider *Provider, srv *pvefake.Server, imageJSON string) string {
	t.Helper()
	return createCIVM(t, provider, srv, ciRequest(imageJSON)).Config["ide2"]
}

// assertDrive asserts that value is VM vmid's own cloud-init volume on storage.
func assertDrive(t *testing.T, value, storage string) {
	t.Helper()
	assert.Equal(t, storage, pveapi.DiskStorage(value), "storage of %q", value)
	assert.Contains(t, value, "-cloudinit", "a cloud-init volume: %q", value)
}

// TestCloudInitStorage_CloneNoHintFollowsPrimaryDisk: no placement and no image
// storage, so the drive goes where the cloned primary disk lives, never to
// `local`, whatever the operator's default is (the default is for imports and
// VMs without a disk).
func TestCloudInitStorage_CloneNoHintFollowsPrimaryDisk(t *testing.T) {
	for _, def := range []string{"", "vmstore"} {
		provider, srv := ciProvider(t, def)
		assertSeed(t, createCI(t, provider, srv, `{"TemplateName": "100"}`), "tank-ssd")
	}
}

// TestCloudInitStorage_CloneNoStorageBackedDiskUsesDefaultStorage: the
// operator's PVE_DEFAULT_STORAGE is next when the config shows no storage-backed
// disk.
func TestCloudInitStorage_CloneNoStorageBackedDiskUsesDefaultStorage(t *testing.T) {
	provider, srv := ciProviderWith(t, "vmstore", map[string]string{"scsi0": "/dev/disk/by-id/foo"})
	assertSeed(t, createCI(t, provider, srv, `{"TemplateName": "100"}`), "vmstore")
}

// TestCloudInitStorage_ExplicitContractsStorageWins: the contracts.VMImage
// "storage" hint beats the primary disk and the operator default.
func TestCloudInitStorage_ExplicitContractsStorageWins(t *testing.T) {
	provider, srv := ciProvider(t, "vmstore")
	assertSeed(t, createCI(t, provider, srv, `{"TemplateName": "100", "storage": "nfs-vm"}`), "nfs-vm")
}

// TestCloudInitStorage_ImageSpecStorageReadWithTemplate: source.proxmox.storage
// of a VMImageSpec is read although the template is named, and beats the
// primary disk and the operator default.
func TestCloudInitStorage_ImageSpecStorageReadWithTemplate(t *testing.T) {
	provider, srv := ciProvider(t, "vmstore")
	assertSeed(t, createCI(t, provider, srv,
		`{"TemplateName": "100", "source": {"proxmox": {"templateID": 100, "storage": "nfs-vm"}}}`), "nfs-vm")
}

// TestCloudInitStorage_NeverLocalWhenDiskElsewhere: whichever way the storage
// is chosen, a template whose disk lives elsewhere never gets a cloud-init
// drive on `local`.
func TestCloudInitStorage_NeverLocalWhenDiskElsewhere(t *testing.T) {
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
			assert.NotEqual(t, "local", pveapi.DiskStorage(ide2))
		})
	}
}

// TestCloudInitStorage_DisklessCreate: a VM made from nothing has no disk to
// follow, so the operator's default is used, and `local` only when nothing
// names a storage.
func TestCloudInitStorage_DisklessCreate(t *testing.T) {
	provider, srv := ciProvider(t, "vmstore")
	assertSeed(t, createCI(t, provider, srv, ""), "vmstore")

	provider, srv = ciProvider(t, "")
	assertSeed(t, createCI(t, provider, srv, ""), "local")

	provider, srv = ciProvider(t, "vmstore")
	assertSeed(t, createCI(t, provider, srv, `{"storage": "nfs-vm"}`), "nfs-vm")
}

// TestCloudInitStorage_NoStorageBackedDiskNoDefaultFallsBackToLocal: a clone
// whose config reads fine but shows no storage-backed disk, with no default
// storage, goes to the last resort.
func TestCloudInitStorage_NoStorageBackedDiskNoDefaultFallsBackToLocal(t *testing.T) {
	provider, srv := ciProviderWith(t, "", map[string]string{"scsi0": "/dev/disk/by-id/foo"})
	assertSeed(t, createCI(t, provider, srv, `{"TemplateName": "100"}`), "local")
}

// TestSeed_ReplacesTheTemplateCloudInitDrive (R35): a clone of a template
// that has a PVE cloud-init drive gets its seed in that drive's slot, which
// replaces the drive (PVE frees its volume): two cidata sources would be
// undefined behaviour. No second CD-ROM goes to ide2.
func TestSeed_ReplacesTheTemplateCloudInitDrive(t *testing.T) {
	for _, tc := range []struct{ slot, storage string }{
		{"ide2", "tank-ssd"}, {"ide3", "tank-ssd"}, {"sata1", "tank-ssd"}, {"scsi5", "tank-ssd"}, {"ide2", "other-st"},
	} {
		t.Run(tc.slot+" on "+tc.storage, func(t *testing.T) {
			provider, srv := ciProviderWith(t, "", map[string]string{
				"scsi0": ciTemplateBootDisk,
				tc.slot: tc.storage + ":vm-100-cloudinit,media=cdrom",
			})
			vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100"}`))
			assertSeed(t, vm.Config[tc.slot], "tank-ssd")
			_, _, hasDrive := pveapi.CloudInitDrive(configOf(t, provider, vm.VMID))
			assert.False(t, hasDrive, "no PVE cloud-init drive beside the seed")
			if tc.slot != "ide2" {
				assert.Empty(t, vm.Config["ide2"], "no second CD-ROM in ide2")
			}
		})
	}
}

// configOf reads VM vmid's config through the provider's client.
func configOf(t *testing.T, provider *Provider, vmid int) map[string]interface{} {
	t.Helper()
	cfg, err := provider.client.GetVMConfig(context.Background(), "pve", vmid)
	require.NoError(t, err)
	return cfg
}

// TestSeed_ExplicitStorageWinsOverTheTemplateDrive: the request's storage
// holds the seed, also when the template has a cloud-init drive.
func TestSeed_ExplicitStorageWinsOverTheTemplateDrive(t *testing.T) {
	provider, srv := ciProviderWith(t, "", map[string]string{
		"scsi0": ciTemplateBootDisk,
		"ide2":  "tank-ssd:vm-100-cloudinit,media=cdrom",
	})
	vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100", "storage": "nfs-vm"}`))
	assertSeed(t, vm.Config["ide2"], "nfs-vm")
}

// TestSeed_DirectoryStorageName: on a directory storage the seed `qm
// importdisk --format raw` lays down is a raw file <id>/vm-<id>-disk-N.raw.
func TestSeed_DirectoryStorageName(t *testing.T) {
	provider, srv := ciProviderWith(t, "", map[string]string{
		"scsi0": "dirst:100/vm-100-disk-0.qcow2,size=32G",
		"ide2":  "dirst:100/vm-100-cloudinit.raw,media=cdrom",
	})
	srv.SetDirStorages("dirst")
	vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100"}`))
	id := strconv.Itoa(vm.VMID)
	assert.Equal(t, "dirst:"+id+"/vm-"+id+"-disk-1.raw,media=cdrom", vm.Config["ide2"])
	assert.Equal(t, "dirst:"+id+"/vm-"+id+"-disk-0.qcow2,size=32G", vm.Config["scsi0"])
}

// TestSeed_ConfigureClonedVMIsIdempotent: a re-sent Create that finds its VM
// interrupted runs configureClonedVM again; the VM already has its seed, so
// no second one is built or attached.
func TestSeed_ConfigureClonedVMIsIdempotent(t *testing.T) {
	for name, tmpl := range map[string]map[string]string{
		"template without a drive": {"scsi0": ciTemplateBootDisk},
		"template with a drive":    {"scsi0": ciTemplateBootDisk, "ide2": "tank-ssd:vm-100-cloudinit,media=cdrom"},
	} {
		t.Run(name, func(t *testing.T) {
			provider, srv, node := ciProviderNode(t, "", tmpl)
			req := ciRequest(`{"TemplateName": "100"}`)
			vm := createCIVM(t, provider, srv, req)
			before := vm.Config["ide2"]
			assertSeed(t, before, "tank-ssd")

			for i := 0; i < 2; i++ {
				require.NoError(t, provider.configureClonedVM(context.Background(), req, &pveapi.VMConfig{VMID: vm.VMID}, "pve"))
			}
			after, ok := srv.GetVM(vm.VMID)
			require.True(t, ok)
			assert.Equal(t, before, after.Config["ide2"])
			assert.Len(t, node.seedsOf(vm.VMID), 1, "one seed only")
			assert.Len(t, srv.Volumes(), 1)
		})
	}
}

// TestCloudInitStorage_UnreadableConfigFailsClosed (PR #425 M4): when the
// clone's config cannot be read the drive's storage is not guessed (a guess
// is `local`, the R33 failure): the call fails and sets nothing.
func TestCloudInitStorage_UnreadableConfigFailsClosed(t *testing.T) {
	provider, srv := ciProvider(t, "")
	vm := createCIVM(t, provider, srv, ciRequest(`{"TemplateName": "100"}`))
	before := len(srv.MutatingRequests())

	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/qemu/" + strconv.Itoa(vm.VMID) + "/config", Status: http.StatusInternalServerError, Body: "boom"})
	err := provider.configureClonedVM(context.Background(), ciRequest(`{"TemplateName": "100"}`), &pveapi.VMConfig{VMID: vm.VMID}, "pve")
	require.Error(t, err)
	assert.Equal(t, before, len(srv.MutatingRequests()), "nothing was changed")
}

// TestCloudInitStorage_InvalidConfiguredStorageIsInvalidSpec (PR #425 L2): a
// storage from the request, the image or the environment that is not a PVE
// storage ID is refused before anything is created and never reaches a config
// value or a message.
func TestCloudInitStorage_InvalidConfiguredStorageIsInvalidSpec(t *testing.T) {
	const evil = "x,ide2=none"
	for name, tc := range map[string]struct {
		defaultStorage string
		imageJSON      string
	}{
		"contracts storage": {"", `{"TemplateName": "100", "storage": "` + evil + `"}`},
		"image spec":        {"", `{"TemplateName": "100", "source": {"proxmox": {"storage": "` + evil + `"}}}`},
		"default storage":   {evil, `{"TemplateName": "100"}`},
		"diskless":          {"", `{"storage": "` + evil + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			provider, srv := ciProvider(t, tc.defaultStorage)
			_, err := provider.Create(context.Background(), ciRequest(tc.imageJSON))
			require.Error(t, err)
			var perr *errors.ProviderError
			require.True(t, stderrors.As(err, &perr))
			assert.Equal(t, codes.InvalidArgument, perr.Code)
			assert.NotContains(t, err.Error(), evil)
			assert.Empty(t, srv.MutatingRequests(), "nothing was created")
		})
	}
}

// TestTaskStatus_QueryFailureIsUnavailableNotFailed (PR #425 M3): a failed
// task-status query says nothing about the task, so it is an Unavailable
// answer (the manager keeps the task); a task that ended with a non-OK exit
// status is Done with an error.
func TestTaskStatus_QueryFailureIsUnavailableNotFailed(t *testing.T) {
	provider, srv := ciProvider(t, "")
	ctx := context.Background()

	running := srv.StartTask("pve", "qmstart", "100", testTokenID)
	srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/tasks/", Status: http.StatusInternalServerError, Body: "blip", Times: 1})
	_, err := provider.TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: running}})
	require.Error(t, err)
	var perr *errors.ProviderError
	require.True(t, stderrors.As(err, &perr))
	assert.Equal(t, codes.Unavailable, perr.Code)

	// The fault was one-shot: the task is still running.
	resp, err := provider.TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: running}})
	require.NoError(t, err)
	assert.False(t, resp.Done)
	assert.Empty(t, resp.Error)

	srv.FailTasks("qmstart")
	srv.CompleteTasks()
	resp, err = provider.TaskStatus(ctx, &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: running}})
	require.NoError(t, err)
	assert.True(t, resp.Done)
	assert.Contains(t, resp.Error, "task failed")
}

// TestTaskStatus_UnknownTaskEndsTransientErrorsDoNot (M-1): an error of the
// query itself (a malformed or unknown UPID: 4xx, or a 5xx that says "no such
// task") ends the task with an error, so no manager polls it for ever; the
// answers that say nothing about the task (the token, rate limiting, other
// 5xx) stay Unavailable.
func TestTaskStatus_UnknownTaskEndsTransientErrorsDoNot(t *testing.T) {
	for name, tc := range map[string]struct {
		status   int
		body     string
		terminal bool
	}{
		"400 bad UPID":      {http.StatusBadRequest, "unable to parse worker upid", true},
		"404":               {http.StatusNotFound, "not found", true},
		"500 no such task":  {http.StatusInternalServerError, "no such task", true},
		"500 other":         {http.StatusInternalServerError, "blip", false},
		"502":               {http.StatusBadGateway, "bad gateway", false},
		"401":               {http.StatusUnauthorized, "no ticket", false},
		"403":               {http.StatusForbidden, "permission check failed", false},
		"429":               {http.StatusTooManyRequests, "slow down", false},
		"595 node down":     {595, "no route to host", false},
		"500 unparsed UPID": {http.StatusInternalServerError, "unable to parse worker upid 'x'", true},
	} {
		t.Run(name, func(t *testing.T) {
			provider, srv := ciProvider(t, "")
			running := srv.StartTask("pve", "qmstart", "100", testTokenID)
			srv.InjectFault(pvefake.Fault{Method: http.MethodGet, PathContains: "/tasks/", Status: tc.status, Body: tc.body})
			resp, err := provider.TaskStatus(context.Background(), &providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: running}})
			if tc.terminal {
				require.NoError(t, err)
				assert.True(t, resp.Done)
				assert.NotEmpty(t, resp.Error)
				return
			}
			require.Error(t, err)
			var perr *errors.ProviderError
			require.True(t, stderrors.As(err, &perr))
			assert.Equal(t, codes.Unavailable, perr.Code)
		})
	}
}

// TestCloudInitDriveChange: what the clone's config update does about the drive.
func TestCloudInitDriveChange(t *testing.T) {
	cfg := func(kv ...string) map[string]interface{} {
		m := map[string]interface{}{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for name, tc := range map[string]struct {
		cfg       map[string]interface{}
		storage   string
		wantSlot  string
		wantValue string
		wantSet   bool
	}{
		"no drive":          {cfg("scsi0", ciTemplateBootDisk), "tank-ssd", "ide2", "tank-ssd:cloudinit", true},
		"same storage":      {cfg("ide2", "tank-ssd:vm-9-cloudinit,media=cdrom"), "tank-ssd", "ide2", "", false},
		"other storage":     {cfg("ide2", "other:vm-9-cloudinit,media=cdrom"), "tank-ssd", "ide2", "tank-ssd:cloudinit", true},
		"drive in ide3":     {cfg("ide3", "tank-ssd:vm-9-cloudinit,media=cdrom"), "tank-ssd", "ide3", "", false},
		"other in sata0":    {cfg("sata0", "other:vm-9-cloudinit,media=cdrom"), "tank-ssd", "sata0", "tank-ssd:cloudinit", true},
		"iso in ide2":       {cfg("ide2", "local:iso/x.iso,media=cdrom"), "tank-ssd", "ide2", "tank-ssd:cloudinit", true},
		"non-string config": {map[string]interface{}{"ide2": 5}, "tank-ssd", "ide2", "tank-ssd:cloudinit", true},
	} {
		t.Run(name, func(t *testing.T) {
			slot, value, set := cloudInitChange(tc.cfg, tc.storage)
			assert.Equal(t, tc.wantSlot, slot)
			assert.Equal(t, tc.wantValue, value)
			assert.Equal(t, tc.wantSet, set)
		})
	}
}

// TestDefaultStorageValidation: the configured storages are checked as PVE
// storage IDs.
func TestDefaultStorageValidation(t *testing.T) {
	_, endpoint, err := pvefake.StartFakeServer()
	require.NoError(t, err)
	provider := createTestProvider(endpoint)

	def, err := provider.defaultStorage()
	require.NoError(t, err)
	assert.Empty(t, def)
	provider.client.Config().DefaultStorage = "vmstore"
	def, err = provider.defaultStorage()
	require.NoError(t, err)
	assert.Equal(t, "vmstore", def)
	provider.client.Config().DefaultStorage = "a b"
	_, err = provider.defaultStorage()
	require.Error(t, err)

	// Validate fails with a clear message, not repeating the value (L-e).
	resp, vErr := provider.Validate(context.Background(), &providerv1.ValidateRequest{})
	require.NoError(t, vErr)
	assert.False(t, resp.Ok)
	assert.Contains(t, resp.Message, "PVE_DEFAULT_STORAGE")
	assert.NotContains(t, resp.Message, "a b")
	provider.client.Config().DefaultStorage = "vmstore"
	resp, vErr = provider.Validate(context.Background(), &providerv1.ValidateRequest{})
	require.NoError(t, vErr)
	assert.True(t, resp.Ok, resp.Message)

	require.NoError(t, checkRequestStorage(""))
	require.NoError(t, checkRequestStorage("nfs-vm"))
	require.Error(t, checkRequestStorage("../x"))
	require.False(t, strings.Contains(checkRequestStorage("../x").Error(), "../x"))
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
