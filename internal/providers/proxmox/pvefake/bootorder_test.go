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

package pvefake

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRequest serves one form request on s and returns the status code.
func fakeRequest(t *testing.T, s *Server, method, path string, form url.Values) int {
	t.Helper()
	r := httptest.NewRequest(method, "/api2/json"+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Authorization", "PVEAPIToken=test@pve!t=secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w.Code
}

// TestDefaultBootOrder: create_vm's default boot devices: the first disk,
// the first CD-ROM (a cloud-init drive counts), the first NIC.
func TestDefaultBootOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		config map[string]string
		want   string
	}{
		"no device":    {map[string]string{}, ""},
		"NIC only":     {map[string]string{"net1": "virtio,bridge=vmbr0", "net0": "virtio,bridge=vmbr0"}, "order=net0"},
		"disk and NIC": {map[string]string{"virtio0": "st:vm-1-disk-0", "scsi0": "st:vm-1-disk-1", "net0": "virtio"}, "order=scsi0;net0"},
		"cloud-init drive": {map[string]string{"scsi0": "st:vm-1-disk-0", "ide2": "st:vm-1-cloudinit,media=cdrom", "net0": "virtio"},
			"order=scsi0;ide2;net0"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, defaultBootOrder(tc.config))
		})
	}
}

// TestCreateVM_DefaultBootOrder: a create without a boot parameter gets the
// default boot devices; one with a boot parameter keeps it.
func TestCreateVM_DefaultBootOrder(t *testing.T) {
	s := NewServer()
	s.SetTaskDelay(0)
	code := fakeRequest(t, s, http.MethodPost, "/nodes/pve/qemu", url.Values{"vmid": {"801"}, "net0": {"virtio,bridge=vmbr0"}})
	require.Equal(t, http.StatusOK, code)
	vm, ok := s.GetVM(801)
	require.True(t, ok)
	assert.Equal(t, "order=net0", vm.Config[configKeyBoot])

	code = fakeRequest(t, s, http.MethodPost, "/nodes/pve/qemu", url.Values{"vmid": {"802"}, "net0": {"virtio,bridge=vmbr0"}, "boot": {"order=net0"}})
	require.Equal(t, http.StatusOK, code)
	vm, ok = s.GetVM(802)
	require.True(t, ok)
	assert.Equal(t, "order=net0", vm.Config[configKeyBoot])
}

// TestBootOrder_NewCDROMIsAppended: a CD-ROM a config update or a seed `qm
// set` attaches is appended to a non-empty order list that does not name it;
// a cloud-init drive is not; an empty or legacy order gets nothing; a deleted
// drive leaves the order; an update that sets the order and attaches a CD-ROM
// together is refused.
func TestBootOrder_NewCDROMIsAppended(t *testing.T) {
	newVM := func(s *Server, boot string) {
		cfg := map[string]string{"scsi0": "st:vm-900-disk-0", "net0": "virtio=AA:BB:CC:00:00:01,bridge=vmbr0"}
		if boot != "" {
			cfg[configKeyBoot] = boot
		}
		s.PutVM(VM{VMID: 900, Node: "pve", Status: "stopped", Config: cfg})
	}
	bootOf := func(s *Server) string {
		vm, ok := s.GetVM(900)
		require.True(t, ok)
		return vm.Config[configKeyBoot]
	}

	t.Run("seed qm set", func(t *testing.T) {
		s := NewServer()
		newVM(s, "order=scsi0")
		_, err := s.AttachSeedVolume(900, "st", "ide2")
		require.NoError(t, err)
		assert.Equal(t, "order=scsi0;ide2", bootOf(s))
	})
	t.Run("slot already in the order", func(t *testing.T) {
		s := NewServer()
		newVM(s, "order=scsi0;ide2;net0")
		_, err := s.AttachSeedVolume(900, "st", "ide2")
		require.NoError(t, err)
		assert.Equal(t, "order=scsi0;ide2;net0", bootOf(s))
	})
	t.Run("no order list", func(t *testing.T) {
		s := NewServer()
		newVM(s, "")
		_, err := s.AttachSeedVolume(900, "st", "ide2")
		require.NoError(t, err)
		assert.Empty(t, bootOf(s))
	})
	t.Run("config update with an ISO", func(t *testing.T) {
		s := NewServer()
		newVM(s, "order=scsi0")
		require.Equal(t, http.StatusOK, fakeRequest(t, s, http.MethodPut, "/nodes/pve/qemu/900/config",
			url.Values{"ide0": {"local:iso/x.iso,media=cdrom"}}))
		assert.Equal(t, "order=scsi0;ide0", bootOf(s))
	})
	t.Run("config update with a cloud-init drive", func(t *testing.T) {
		s := NewServer()
		newVM(s, "order=scsi0")
		require.Equal(t, http.StatusOK, fakeRequest(t, s, http.MethodPut, "/nodes/pve/qemu/900/config",
			url.Values{"ide2": {"st:cloudinit"}}))
		assert.Equal(t, "order=scsi0", bootOf(s))
	})
	t.Run("delete", func(t *testing.T) {
		s := NewServer()
		newVM(s, "order=scsi0;ide2;net0")
		_, err := s.AttachSeedVolume(900, "st", "ide2")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, fakeRequest(t, s, http.MethodPut, "/nodes/pve/qemu/900/config",
			url.Values{"delete": {"ide2"}}))
		assert.Equal(t, "order=scsi0;net0", bootOf(s))
	})
	t.Run("boot and a CD-ROM together", func(t *testing.T) {
		s := NewServer()
		newVM(s, "order=scsi0")
		code := fakeRequest(t, s, http.MethodPut, "/nodes/pve/qemu/900/config",
			url.Values{"ide0": {"local:iso/x.iso,media=cdrom"}, "boot": {"order=scsi0"}})
		assert.Equal(t, http.StatusInternalServerError, code)
		assert.Equal(t, "order=scsi0", bootOf(s), "nothing changed")
	})
}
