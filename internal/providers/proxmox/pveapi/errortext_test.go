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
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func respWith(status, body string) *http.Response {
	return &http.Response{Status: status, Body: io.NopCloser(strings.NewReader(body))}
}

// TestPVEErrorText: PVE's error message is read from the status line's reason
// phrase (real PVE, body {"data":null}) and from the body; a text that names
// nothing known matches nothing.
func TestPVEErrorText(t *testing.T) {
	inStatus := pveErrorText(respWith("500 snapshot 'mig-1' does not exist", `{"data":null}`))
	assert.True(t, pveSnapshotDoesNotExist(inStatus, "mig-1"), inStatus)

	inBody := pveErrorText(respWith("500 Internal Server Error", `{"errors":{"error":"snapshot 'mig-1' does not exist"}}`))
	assert.True(t, pveSnapshotDoesNotExist(inBody, "mig-1"), inBody)

	other := pveErrorText(respWith("500 Configuration file 'nodes/pve/qemu-server/100.conf' does not exist", `{"data":null}`))
	assert.False(t, pveSnapshotDoesNotExist(other, "mig-1"), "a VM not on the node is no snapshot answer")
	assert.False(t, pveSnapshotDoesNotExist(pveErrorText(respWith("404 Not Found", "")), "mig-1"))
	assert.Equal(t, "", pveErrorText(nil))
}
