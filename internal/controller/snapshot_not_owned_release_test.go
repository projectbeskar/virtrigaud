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

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A SnapshotDelete answered VM_NOT_OWNED releases the VMSnapshot — the
// snapshot cannot be deleted through a VM that is not the VirtualMachine's —
// but never as "already gone": a SnapshotLeftOnHypervisor Warning says the
// snapshot may be left on the VM at that id.
func TestVMSnapshot_DeleteAnsweredVMNotOwnedReleasesWithAWarning(t *testing.T) {
	r, spy, get, events := deletingSnapshot(t, nil)
	spy.answer(vmNotOwnedAnswer("snapshotDelete"))

	_, err := r.handleDeletion(context.Background(), get())
	require.NoError(t, err)
	assert.Nil(t, get(), "the finalizer is released")
	evs := strings.Join(eventsOf(events), "\n")
	assert.Contains(t, evs, "Warning "+eventReasonSnapshotLeftOnHypervisor+" "+snapshotNotOwnedLeftMessage)
	assert.NotContains(t, evs, "Normal SnapshotDeleted", "never reported as deleted or already gone")
	assert.NotContains(t, evs, "rpc error")
}

// The migration's source-snapshot cleanup does the same: the snapshot id is
// cleared (the cleanup is done) and the release is recorded as a Warning on
// the VMMigration.
func TestDeleteSourceSnapshot_VMNotOwnedReleasesWithAWarning(t *testing.T) {
	vm, p, m := snapshotFixture()
	prov := &scriptedSnapshotDeleteProvider{err: vmNotOwnedAnswer("snapshotDelete")}
	h := newSnapshotHoldHarness(t, prov, vm, p, m)

	err := h.r.deleteSourceSnapshot(context.Background(), m)
	require.NoError(t, err)
	assert.EqualValues(t, 1, prov.calls.Load())
	assert.Empty(t, m.Status.SnapshotID, "the cleanup is done")
	evs := strings.Join(drainRecordedEvents(h.rec), "\n")
	assert.Contains(t, evs, "Warning "+eventReasonSnapshotLeftOnHypervisor+" "+snapshotNotOwnedLeftMessage)
}
