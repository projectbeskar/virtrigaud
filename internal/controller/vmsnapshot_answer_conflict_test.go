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
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a SnapshotCreate's outcome — the snapshot made, a
// definite failure, or "not performed" — replaces the SnapshotRequested
// marker although the VMSnapshot was edited while the provider ran the create
// (recordSnapshotAnswer). The status write used to conflict, the marker
// survived, and a single-host VMSnapshot was held as SnapshotOutcomeUnknown
// for an operator — even with the snapshot made.

// editingSnapshotter is a snapshotRecorder that labels the stored VMSnapshot
// before it answers each SnapshotCreate.
type editingSnapshotter struct {
	*snapshotRecorder
	r   *VMSnapshotReconciler
	key client.ObjectKey
	t   *testing.T
}

// SnapshotCreate edits the VMSnapshot, then answers as the snapshotRecorder.
func (p *editingSnapshotter) SnapshotCreate(ctx context.Context, req contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	snap := &infrav1beta1.VMSnapshot{}
	require.NoError(p.t, p.r.Get(ctx, p.key, snap))
	if snap.Labels == nil {
		snap.Labels = map[string]string{}
	}
	snap.Labels["edited"] = fmt.Sprintf("during-create-%d", len(p.creates))
	require.NoError(p.t, p.r.Update(ctx, snap))
	return p.snapshotRecorder.SnapshotCreate(ctx, req)
}

func TestVMSnapshot_SingleHost_OutcomeSurvivesAnEditDuringCreate(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		phase infrav1beta1.SnapshotPhase
		id    string
	}{
		"the snapshot is made": {phase: infrav1beta1.SnapshotPhaseReady, id: "snap-1"},
		"a definite failure":   {err: contracts.NewInvalidSpecError("bad name", nil), phase: infrav1beta1.SnapshotPhaseFailed},
		"not performed": {err: contracts.NewRetryableError("create snapshot: not performed",
			fmt.Errorf("%w: %w", contracts.ErrOperationNotPerformed, errors.New("rpc error"))), phase: ""},
	} {
		t.Run(name, func(t *testing.T) {
			r, rec, snap := markerSnapshotFixture(t, false, &statusWriteFaults{})
			rec.createErr = tc.err
			es := &editingSnapshotter{snapshotRecorder: rec, r: r, key: client.ObjectKeyFromObject(snap), t: t}
			r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return es, nil }

			_, got := reconcileSnapshotOnce(t, r, snap)
			require.Len(t, rec.creates, 1)
			assert.Equal(t, "during-create-0", got.Labels["edited"], "the edit is kept")
			assert.False(t, snapshotRequested(got), "the outcome replaced the marker")
			assert.Equal(t, tc.phase, got.Status.Phase)
			assert.Equal(t, tc.id, got.Status.SnapshotID)
			if ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady); ready != nil {
				assert.NotEqual(t, snapshotReasonOutcomeUnknown, ready.Reason)
			}

			// The next reconcile does not hold it for an operator.
			_, got = reconcileSnapshotOnce(t, r, snap)
			if ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady); ready != nil {
				assert.NotEqual(t, snapshotReasonOutcomeUnknown, ready.Reason, "never held as an unknown outcome")
			}
		})
	}
}
