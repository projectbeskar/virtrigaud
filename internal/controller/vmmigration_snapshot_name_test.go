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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// recordingSnapshotProvider is snapshotTaskProvider that also records the
// SnapshotCreate requests.
type recordingSnapshotProvider struct {
	snapshotTaskProvider
	reqs []contracts.SnapshotCreateRequest
}

func (p *recordingSnapshotProvider) SnapshotCreate(ctx context.Context, req contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.reqs = append(p.reqs, req)
	return p.snapshotTaskProvider.SnapshotCreate(ctx, req)
}

// TestMigrationSnapshot_RetryNamesItsSnapshotAfterTheAttempt (re-review L-b):
// with cleanupPolicy Never, a discarded unconfirmed snapshot stays on the
// source VM; the retry's snapshot name hint and request token carry the
// attempt, so they never collide with it. The first attempt is unchanged.
func TestMigrationSnapshot_RetryNamesItsSnapshotAfterTheAttempt(t *testing.T) {
	vm, p, m := snapshottingMigration()
	m.Spec.Options = &infrav1beta1.MigrationOptions{CleanupPolicy: infrav1beta1.CleanupPolicyNever}
	m.Status.RetryCount = 1
	m.Status.SnapshotID = "source-vm-migration-uid-snap"
	m.Status.Conditions = []metav1.Condition{{Type: infrav1beta1.VMMigrationConditionSnapshotting,
		Status: metav1.ConditionFalse, Reason: migrationReasonSnapshotFailed, Message: "x", LastTransitionTime: metav1.Now()}}
	prov := &recordingSnapshotProvider{snapshotTaskProvider: snapshotTaskProvider{sync: true}}
	r := newSnapshotTaskReconciler(t, prov, vm, p, m)

	_, got := reconcileSnapshotStep(t, r, m) // forgets the unconfirmed snapshot (Never)
	assert.Zero(t, prov.deletes.Load())
	_, _ = reconcileSnapshotStep(t, r, got) // takes a new one
	require.Len(t, prov.reqs, 1)
	assert.Equal(t, "source-vm-migration-uid-snap-r1", prov.reqs[0].NameHint)
	assert.Equal(t, "uid-snap-1-r1", prov.reqs[0].RequestToken)

	first := &infrav1beta1.VMMigration{}
	first.UID = m.UID
	first.Spec.Source.VMRef.Name = "source-vm"
	name, token := migrationSnapshotIdentity(first)
	assert.Equal(t, "source-vm-migration-uid-snap", name, "the first attempt is unchanged")
	assert.Equal(t, "uid-snap-1", token)
}
