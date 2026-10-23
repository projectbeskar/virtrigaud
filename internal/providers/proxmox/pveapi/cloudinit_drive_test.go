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

package pveapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCloudInitDrive: only a vm-<id>-cloudinit volume is a cloud-init drive,
// whatever the storage type names it.
func TestCloudInitDrive(t *testing.T) {
	for name, tc := range map[string]struct {
		slot  string
		value string
		want  bool
	}{
		"lvm":                  {"ide2", "local-lvm:vm-199-cloudinit,media=cdrom", true},
		"rbd":                  {"ide2", "ceph-pool:vm-199-cloudinit,media=cdrom", true},
		"dir raw":              {"ide2", "local:198/vm-198-cloudinit.raw,media=cdrom", true},
		"dir qcow2":            {"sata1", "local:197/vm-197-cloudinit.qcow2,media=cdrom", true},
		"vmdk":                 {"scsi3", "nfs-vm:197/vm-197-cloudinit.vmdk,media=cdrom,size=4M", true},
		"no options":           {"ide3", "tank-ssd:vm-9-cloudinit", true},
		"ISO named cloudinit":  {"ide2", "local:iso/cloudinit-seed.iso,media=cdrom", false},
		"ISO in a subdir":      {"ide2", "local:iso/vm-9-cloudinit.iso,media=cdrom", false},
		"disk named cloudinit": {"scsi0", "local-lvm:vm-9-disk-0-cloudinit,size=1G", false},
		"other extension":      {"ide2", "local:9/vm-9-cloudinit.iso,media=cdrom", false},
		"not a cloud-init":     {"ide2", "local-lvm:vm-9-disk-1,size=8G", false},
		"suffix of a name":     {"ide2", "local-lvm:xvm-9-cloudinit,media=cdrom", false},
	} {
		t.Run(name, func(t *testing.T) {
			slot, value, ok := CloudInitDrive(map[string]interface{}{tc.slot: tc.value})
			assert.Equal(t, tc.want, ok)
			if tc.want {
				assert.Equal(t, tc.slot, slot)
				assert.Equal(t, tc.value, value)
			}
		})
	}

	_, _, ok := CloudInitDrive(map[string]interface{}{"ide2": 5, "net0": "virtio=AA:BB"})
	assert.False(t, ok, "a non-string value is no drive")
}

// TestIsTerminalTaskQueryError: only an error of the query itself ends a task.
func TestIsTerminalTaskQueryError(t *testing.T) {
	api := func(status int, text string) error {
		return fmt.Errorf("failed to get task status: %w", &APIError{Operation: "get task status", StatusCode: status, Reason: text})
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"400":                   {api(http.StatusBadRequest, "bad"), true},
		"404":                   {api(http.StatusNotFound, "gone"), true},
		"500 no such task":      {api(http.StatusInternalServerError, "No such task"), true},
		"500 bad upid":          {api(http.StatusInternalServerError, "unable to parse worker upid"), true},
		"500 other":             {api(http.StatusInternalServerError, "boom"), false},
		"502":                   {api(http.StatusBadGateway, "bad gateway"), false},
		"401":                   {api(http.StatusUnauthorized, ""), false},
		"403":                   {api(http.StatusForbidden, ""), false},
		"429":                   {api(http.StatusTooManyRequests, ""), false},
		"transport error":       {errors.New("dial tcp: connection refused"), false},
		"decode error":          {errors.New("failed to decode response: EOF"), false},
		"wrapped transport":     {fmt.Errorf("failed to get task status: %w", deadlineError{}), false},
		"nil":                   {nil, false},
		"redirect (not an 4xx)": {api(http.StatusFound, "moved"), false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsTerminalTaskQueryError(tc.err))
		})
	}
}

// deadlineError stands in for a timeout error.
type deadlineError struct{}

func (deadlineError) Error() string { return "context deadline exceeded" }
