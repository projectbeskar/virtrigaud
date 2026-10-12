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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pvefake"
)

// TestInflight_GuestOnDownNodeDoesNotHold (M-A): a nameless guest locked for
// clone on a node that is down cannot be in progress; it must not hold every
// Create of the Provider. The same guest on a node that is up does.
func TestInflight_GuestOnDownNodeDoesNotHold(t *testing.T) {
	p, srv := newStampTestProvider(t)
	srv.PutVM(pvefake.VM{VMID: 4900, Node: "pve2", Status: "stopped", Lock: "clone"})
	srv.StartTask("pve2", "qmclone", "9000", testTokenID)

	_, err := p.Create(context.Background(), createReq(testOwner(), ""))
	require.Equal(t, classOutcomeUnknown, classOf(t, err), "node up: the guest may be this owner's")

	srv.SetNodeOffline("pve2", true)
	_, err = p.Create(context.Background(), createReq(testOwner(), ""))
	require.NoError(t, err, "node down: the guest cannot be in progress")
	assert.Len(t, srv.VMsNamed(testOwnerName), 1)
}
