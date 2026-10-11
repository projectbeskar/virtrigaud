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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These tests pin the migration retry backoff: retry n waits
// retryDelay × backoffMultiplier^(n-1) after the failure it retries — the
// first retry included (it used to run at once) — and a failure is never
// followed by an immediate requeue.

func TestMigrationRetryDelay(t *testing.T) {
	policy := &infrav1beta1.MigrationRetryPolicy{RetryDelay: &metav1.Duration{Duration: time.Minute}, BackoffMultiplier: ptr.To(int32(3))}
	for retriesDone, want := range []time.Duration{time.Minute, 3 * time.Minute, 9 * time.Minute, 27 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		assert.Equal(t, want, migrationRetryDelay(policy, int32(retriesDone)), "retry %d", retriesDone+1)
	}
	assert.Equal(t, 5*time.Minute, migrationRetryDelay(&infrav1beta1.MigrationRetryPolicy{}, 0), "default retryDelay")
	assert.Equal(t, 10*time.Minute, migrationRetryDelay(&infrav1beta1.MigrationRetryPolicy{}, 1), "default multiplier 2")
	big := &infrav1beta1.MigrationRetryPolicy{RetryDelay: &metav1.Duration{Duration: 24 * time.Hour}, BackoffMultiplier: ptr.To(int32(10))}
	assert.Equal(t, migrationMaxRetryDelay, migrationRetryDelay(big, 10), "capped, no overflow")
}

// retryingMigration is a finalized migration that failed failedAgo ago with
// retriesDone retries done, under a 1-minute retryDelay, multiplier 2, and 3
// retries allowed.
func retryingMigration(failedAgo time.Duration, retriesDone int32) *infrav1beta1.VMMigration {
	_, _, m := snapshotFixture()
	m.Finalizers = []string{vmMigrationFinalizer}
	m.Status.SnapshotID = ""
	m.Status.Phase = infrav1beta1.MigrationPhaseFailed
	m.Status.RetryCount = retriesDone
	if failedAgo >= 0 {
		m.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-failedAgo)}
	}
	m.Spec.Options = &infrav1beta1.MigrationOptions{RetryPolicy: &infrav1beta1.MigrationRetryPolicy{
		MaxRetries: ptr.To(int32(3)), RetryDelay: &metav1.Duration{Duration: time.Minute}, BackoffMultiplier: ptr.To(int32(2)),
	}}
	return m
}

func TestMigrationRetry_FirstRetryWaitsRetryDelay(t *testing.T) {
	m := retryingMigration(5*time.Second, 0)
	r := newSnapshotTaskReconciler(t, &snapshotTaskProvider{}, m)

	res, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase, "the first retry is not immediate")
	assert.Zero(t, got.Status.RetryCount)
	assert.InDelta(t, (55 * time.Second).Seconds(), res.RequeueAfter.Seconds(), 3)
}

func TestMigrationRetry_RetriesOnceTheDelayHasPassed(t *testing.T) {
	m := retryingMigration(61*time.Second, 0)
	r := newSnapshotTaskReconciler(t, &snapshotTaskProvider{}, m)

	res, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhasePending, got.Status.Phase)
	assert.EqualValues(t, 1, got.Status.RetryCount)
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter, "never an immediate requeue after the write")
}

func TestMigrationRetry_LaterRetriesBackOff(t *testing.T) {
	// The second retry waits 1 min × 2 = 2 min after its failure.
	m := retryingMigration(90*time.Second, 1)
	r := newSnapshotTaskReconciler(t, &snapshotTaskProvider{}, m)

	res, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.InDelta(t, (30 * time.Second).Seconds(), res.RequeueAfter.Seconds(), 3)
}

func TestMigrationRetry_UnrecordedFailureTimeStartsTheWaitNow(t *testing.T) {
	m := retryingMigration(-1, 0)
	r := newSnapshotTaskReconciler(t, &snapshotTaskProvider{}, m)

	res, got := reconcileSnapshotStep(t, r, m)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	require.NotNil(t, got.Status.CompletionTime, "the wait gets a fixed start")
	assert.Equal(t, time.Minute, res.RequeueAfter)
}

func TestMigrationTransitionToFailed_NeverRequeuesAtOnce(t *testing.T) {
	_, _, m := snapshotFixture()
	r := newSnapshotTaskReconciler(t, &snapshotTaskProvider{}, m)

	res, err := r.transitionToFailed(context.Background(), m, "export failed")
	require.NoError(t, err)
	assert.False(t, res.Requeue) //nolint:staticcheck // asserting the deprecated field is unset
	assert.Equal(t, migrationPhaseAdvanceRequeue, res.RequeueAfter)
	require.NotNil(t, m.Status.CompletionTime)
}
