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
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestSourceSnapshotUnaddressable_NeverReportedDeleted (security review M1): a
// source snapshot that nothing can address any more — its source VM or its
// own-namespace Provider is gone — is never reported deleted. A terminal
// failure keeps the cleanup pending; the Ready-phase cleanup keeps the
// snapshot recorded; a deletion releases with a SnapshotLeftOnHypervisor
// Warning, never a Normal "deleted".
func TestSourceSnapshotUnaddressable_NeverReportedDeleted(t *testing.T) {
	objsWithout := func(drop string, vm *infrav1beta1.VirtualMachine, p *infrav1beta1.Provider, m *infrav1beta1.VMMigration) []client.Object {
		objs := []client.Object{m}
		if drop != "vm" {
			objs = append(objs, vm)
		}
		if drop != "provider" {
			objs = append(objs, p)
		}
		return objs
	}
	for name, drop := range map[string]string{"source VM gone": "vm", "source Provider gone": "provider"} {
		t.Run(name+": terminal failure holds", func(t *testing.T) {
			vm, p, m := terminalFailedMigration()
			prov := &scriptedSnapshotDeleteProvider{}
			h := newSnapshotHoldHarness(t, prov, objsWithout(drop, vm, p, m)...)

			res := h.reconcile(t, m)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			assert.Equal(t, "snap-abc", h.get(t, m).Status.SnapshotID, "not cleared: nothing was deleted")
			events := strings.Join(drainRecordedEvents(h.rec), "\n")
			assert.NotContains(t, events, migrationReasonSourceSnapshotDeleted)
			assert.Contains(t, events, migrationReasonSnapshotCleanupPending)
			assert.Zero(t, prov.calls.Load())
		})

		t.Run(name+": Ready-phase cleanup keeps it recorded", func(t *testing.T) {
			vm, p, m := snapshotFixture()
			m.Status.Phase = infrav1beta1.MigrationPhaseReady
			h := newSnapshotHoldHarness(t, &scriptedSnapshotDeleteProvider{}, objsWithout(drop, vm, p, m)...)

			res, err := h.r.handleReadyPhase(context.Background(), m)
			assert.NoError(t, err)
			assert.Positive(t, res.RequeueAfter)
			assert.Equal(t, "snap-abc", m.Status.SnapshotID)
		})

		t.Run(name+": deletion releases with a Warning", func(t *testing.T) {
			vm, p, m := deletingMigration()
			h := newSnapshotHoldHarness(t, &scriptedSnapshotDeleteProvider{}, objsWithout(drop, vm, p, m)...)

			h.reconcile(t, m)
			assert.Nil(t, h.get(t, m), "released: holding would wait forever")
			events := strings.Join(drainRecordedEvents(h.rec), "\n")
			assert.Contains(t, events, "Warning "+eventReasonSnapshotLeftOnHypervisor)
			assert.Contains(t, events, `"snap-abc"`)
			assert.NotContains(t, events, "Normal")
		})
	}
}
