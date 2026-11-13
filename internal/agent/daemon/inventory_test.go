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

package daemon_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/projectbeskar/virtrigaud/internal/agent/inventory"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// inventoryOf asks GetHostInfo for the host inventory and returns it, or nil.
func (r *running) inventoryOf(requested uint32) *providerv1.HostInventory {
	r.h.t.Helper()
	info, err := providerv1.NewProviderClient(r.conn()).GetHostInfo(callCtx(r.h.t),
		&providerv1.GetHostInfoRequest{AnswerNonce: bytes.Repeat([]byte{4}, 16), InventoryVersion: requested})
	require.NoError(r.h.t, err)
	require.NotNil(r.h.t, info.GetHostAnswer())
	a := &providerv1.HostAnswer{}
	require.NoError(r.h.t, proto.Unmarshal(info.GetHostAnswer().GetBody(), a))
	return a.GetInventory()
}

// TestHostInventoryAnswered: the running agent collects its host inventory in
// the background and puts it in the signed answer of GetHostInfo.
func TestHostInventoryAnswered(t *testing.T) {
	h := newHost(t)
	write := func(p string, content string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(h.invRoot, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(h.invRoot, p), []byte(content), 0o644))
	}
	write(inventory.ReadOnlyManifestDir+"/ch.json", `{"schemaVersion":1,"component":"cloud-hypervisor","version":"1.2.3","source":"package"}`)
	write("/usr/bin/cloud-hypervisor", "binary")
	write("/var/lib/dpkg/status", "Package: cloud-hypervisor\nStatus: install ok installed\nVersion: 1.2.3\n\n")
	r, _ := h.run()
	var inv *providerv1.HostInventory
	require.Eventually(t, func() bool {
		inv = r.inventoryOf(1)
		return inv != nil && inv.GetState() == providerv1.InventoryState_INVENTORY_STATE_COMPLETE
	}, 10*time.Second, 20*time.Millisecond, "the first collection finishes")
	require.NoError(t, inventory.Validate(inv, 1))
	var ch *providerv1.HostComponent
	for _, c := range inv.GetComponents() {
		if c.GetName() == "cloud-hypervisor" {
			ch = c
		}
	}
	require.NotNil(t, ch)
	assert.Equal(t, providerv1.ComponentState_COMPONENT_STATE_READY, ch.GetState())
	assert.Nil(t, r.inventoryOf(0), "no inventory unless asked for")
	assert.Nil(t, r.hello().GetInventory(), "never in the internal service")
	assert.EqualValues(t, 2, r.hello().GetAgentApiVersion())
}
