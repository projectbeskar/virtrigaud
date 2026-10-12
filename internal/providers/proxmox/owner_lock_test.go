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
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// TestOwnerLock_OneCallPerOwner (L2): while a Create or Clone for an owner
// runs in the provider, another one for the same owner is answered
// outcome-unknown and makes no PVE call; other owners are not held.
func TestOwnerLock_OneCallPerOwner(t *testing.T) {
	p, srv := newStampTestProvider(t)
	require.True(t, p.ownerLocks.tryLock(testOwnerUID))

	_, err := p.Create(context.Background(), createReq(testOwner(), templateImageJSON))
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	target := testOwner()
	_, err = p.Clone(context.Background(), &providerv1.CloneRequest{SourceVmId: "100", TargetName: "c", TargetVm: ownerProto(target)})
	assert.Equal(t, classOutcomeUnknown, classOf(t, err))
	assert.Empty(t, srv.Requests(), "nothing is asked of PVE")

	_, err = p.Create(context.Background(), createReq(otherOwner(), ""))
	require.NoError(t, err, "another owner is not held")

	p.ownerLocks.unlock(testOwnerUID)
	_, err = p.Create(context.Background(), createReq(testOwner(), ""))
	require.NoError(t, err)
	assert.False(t, p.ownerLocks.busy[testOwnerUID], "the lock is released when the call ends")
}

// TestRollback_NeverRemovesAVMAnotherCallMarkedReady (L2): a rollback that
// expects its own "creating" stamp refuses a VM stamped ready; one that wrote
// "ready" itself may remove it.
func TestRollback_NeverRemovesAVMAnotherCallMarkedReady(t *testing.T) {
	p, srv := newStampTestProvider(t)
	ready := testStamp(testOwnerUID, ownerStampPhaseReady)
	creating := testStamp(testOwnerUID, ownerStampPhaseCreating)
	srv.PutVM(pvefake.VM{VMID: 4700, Name: testOwnerName, Node: "pve", Status: "stopped", Description: ready.line()})

	require.Error(t, p.rollbackCreatedVM(context.Background(), "pve", 4700, testOwnerName, &creating, true))
	_, exists := srv.GetVM(4700)
	assert.True(t, exists)

	require.NoError(t, p.rollbackCreatedVM(context.Background(), "pve", 4700, testOwnerName, &ready, true))
	_, exists = srv.GetVM(4700)
	assert.False(t, exists)
}
