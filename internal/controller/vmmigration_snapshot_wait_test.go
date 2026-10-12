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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestMigrationSnapshot_TaskWaitIsBounded (re-review L-a): a snapshot task
// that never finishes fails the snapshot after spec.options.timeout, or the
// 2 h default, counted from when the wait began.
func TestMigrationSnapshot_TaskWaitIsBounded(t *testing.T) {
	for name, tc := range map[string]struct {
		timeout   *metav1.Duration
		waited    time.Duration
		wantPhase infrav1beta1.MigrationPhase
	}{
		"default, within":   {waited: time.Hour, wantPhase: infrav1beta1.MigrationPhaseSnapshotting},
		"default, exceeded": {waited: 3 * time.Hour, wantPhase: infrav1beta1.MigrationPhaseFailed},
		"spec timeout, within": {timeout: &metav1.Duration{Duration: 30 * time.Minute}, waited: 10 * time.Minute,
			wantPhase: infrav1beta1.MigrationPhaseSnapshotting},
		"spec timeout, exceeded": {timeout: &metav1.Duration{Duration: 30 * time.Minute}, waited: 40 * time.Minute,
			wantPhase: infrav1beta1.MigrationPhaseFailed},
	} {
		t.Run(name, func(t *testing.T) {
			vm, p, m := snapshottingMigration()
			m.Status.SnapshotID = "snap-t"
			m.Status.TaskRef = "task-snap"
			if tc.timeout != nil {
				m.Spec.Options = &infrav1beta1.MigrationOptions{Timeout: tc.timeout}
			}
			m.Status.Conditions = []metav1.Condition{{Type: infrav1beta1.VMMigrationConditionSnapshotting,
				Status: metav1.ConditionTrue, Reason: migrationReasonSnapshotInProgress, Message: migrationSnapshotInProgressMessage,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-tc.waited))}}
			prov := &snapshotTaskProvider{status: contracts.TaskStatus{IsCompleted: false}}
			r := newSnapshotTaskReconciler(t, prov, vm, p, m)

			_, got := reconcileSnapshotStep(t, r, m)
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			if tc.wantPhase == infrav1beta1.MigrationPhaseFailed {
				assert.Contains(t, got.Status.Message, "did not finish within")
				assert.Equal(t, migrationReasonSnapshotFailed, snapshottingCondition(got).Reason)
				assert.Equal(t, "snap-t", got.Status.SnapshotID, "kept for cleanup")
			}
		})
	}
}
