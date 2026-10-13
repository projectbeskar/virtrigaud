/*
Copyright 2025.

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// powerSpyProvider plays the source hypervisor for ensureSourcePoweredOff:
// Describe reports On until poweredOff is set (by the test, playing the
// VirtualMachine controller), then Off; describeErr makes Describe fail; Power
// records each op it receives — the migration controller must send none.
type powerSpyProvider struct {
	stubProvider
	poweredOff  atomic.Bool
	describeErr error
	mu          sync.Mutex
	powerOps    []contracts.PowerOp
}

func (p *powerSpyProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	if p.describeErr != nil {
		return contracts.DescribeResponse{}, p.describeErr
	}
	state := string(contracts.PowerStateOn)
	if p.poweredOff.Load() {
		state = string(contracts.PowerStateOff)
	}
	return contracts.DescribeResponse{Exists: true, PowerState: state}, nil
}

func (p *powerSpyProvider) Power(_ context.Context, _ contracts.VMRef, op contracts.PowerOp) (string, error) {
	p.mu.Lock()
	p.powerOps = append(p.powerOps, op)
	p.mu.Unlock()
	p.poweredOff.Store(true)
	return "", nil
}

func (p *powerSpyProvider) ops() []contracts.PowerOp {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.PowerOp(nil), p.powerOps...)
}

var _ contracts.Provider = (*powerSpyProvider)(nil)

// storedMigration re-reads migration through c.
func storedMigration(t *testing.T, c client.Client, m *infrav1beta1.VMMigration) *infrav1beta1.VMMigration {
	t.Helper()
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	return got
}

// timedOutPowerOffWait returns a SourcePowerOff condition recording a wait for
// the source power-off that began longer than migrationSourcePowerOffTimeout
// ago, next to a Validating condition rewritten just now by another check of
// the phase (which must not restart the budget).
func timedOutPowerOffWait() []metav1.Condition {
	return []metav1.Condition{{
		Type: migrationConditionSourcePowerOff, Status: metav1.ConditionFalse,
		Reason: reasonWaitingForSourcePowerOff, Message: waitingForSourcePowerOffMessage,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-migrationSourcePowerOffTimeout - time.Minute)),
	}, {
		Type: infrav1beta1.VMMigrationConditionValidating, Status: metav1.ConditionFalse,
		Reason: "SourceProviderNotReady", Message: "Source provider is not ready",
		LastTransitionTime: metav1.Now(),
	}}
}

// TestEnsureSourcePoweredOff_WaitsForTheVMControllerThenProceeds verifies the
// one-actor powerOffBeforeMigration gate (v0.4.0-rc1 F3): a running source gets
// its spec.powerState set to Off and the migration waits on a constant
// Validating=False/WaitingForSourcePowerOff condition — it sends NO Power call
// itself, and a repeated wait writes nothing — then proceeds once the source is
// observed Off (powered off by the VirtualMachine controller).
func TestEnsureSourcePoweredOff_WaitsForTheVMControllerThenProceeds(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{}
	sourceVM, sourceProvider, migration := snapshotFixture()
	sourceVM.Spec.PowerState = infrav1beta1.PowerStateOn
	migration.Spec.Source.PowerOffBeforeMigration = true
	r, c := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	done, res, err := r.ensureSourcePoweredOff(ctx, migration)
	require.NoError(t, err)
	assert.False(t, done, "must wait while the source is still On")
	assert.Equal(t, migrationPowerOffPollInterval, res.RequeueAfter)
	assert.Empty(t, prov.ops(), "the migration controller never powers the source off itself")

	var got infrav1beta1.VirtualMachine
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sourceVM), &got))
	assert.Equal(t, infrav1beta1.PowerStateOff, got.Spec.PowerState,
		"the source VM's desired power state is set to Off for the VirtualMachine controller to act on")

	stored := storedMigration(t, c, migration)
	cond := meta.FindStatusCondition(stored.Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonWaitingForSourcePowerOff, cond.Reason)
	assert.Equal(t, waitingForSourcePowerOffMessage, cond.Message)
	assert.Equal(t, waitingForSourcePowerOffMessage, stored.Status.Message)

	// A repeated wait writes nothing.
	rv := stored.ResourceVersion
	done, _, err = r.ensureSourcePoweredOff(ctx, stored)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, rv, storedMigration(t, c, migration).ResourceVersion, "an unchanged wait writes no status")

	// The VirtualMachine controller powers the source off: the migration proceeds.
	prov.poweredOff.Store(true)
	done, _, err = r.ensureSourcePoweredOff(ctx, stored)
	require.NoError(t, err)
	assert.True(t, done, "must proceed once the source is Off")
	assert.Empty(t, prov.ops(), "no Power call at any point")
}

// TestEnsureSourcePoweredOff_AlreadyOff verifies an already-stopped source is a
// no-op: it returns done=true immediately and issues no power-off.
func TestEnsureSourcePoweredOff_AlreadyOff(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{}
	prov.poweredOff.Store(true) // already Off
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Source.PowerOffBeforeMigration = true
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	done, _, err := r.ensureSourcePoweredOff(ctx, migration)
	require.NoError(t, err)
	assert.True(t, done, "an already-off source proceeds immediately")
	assert.Empty(t, prov.ops(), "must not power off a source that is already Off")
}

// TestEnsureSourcePoweredOff_DescribeErrorWaits: a failed Describe (provider
// restarting, host briefly unreachable) is waited out, never a terminal
// failure, and its text never reaches the condition.
func TestEnsureSourcePoweredOff_DescribeErrorWaits(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{describeErr: errors.New("failed to perform power operation on host kvm-01")}
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Source.PowerOffBeforeMigration = true
	r, c := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	done, res, err := r.ensureSourcePoweredOff(ctx, migration)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, migrationPowerOffPollInterval, res.RequeueAfter)
	cond := meta.FindStatusCondition(storedMigration(t, c, migration).Status.Conditions, infrav1beta1.VMMigrationConditionValidating)
	require.NotNil(t, cond)
	assert.Equal(t, reasonWaitingForSourcePowerOff, cond.Reason)
	assert.Equal(t, waitingForSourcePowerOffMessage, cond.Message, "no provider text in the condition")
	assert.Empty(t, prov.ops())
}

// TestEnsureSourcePoweredOff_TimesOut: a source not observed Off within
// migrationSourcePowerOffTimeout ends the wait with errSourcePowerOffTimedOut —
// the only terminal outcome of the wait — and still sends no Power call.
func TestEnsureSourcePoweredOff_TimesOut(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{}
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Source.PowerOffBeforeMigration = true
	migration.Status.Conditions = timedOutPowerOffWait()
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	done, _, err := r.ensureSourcePoweredOff(ctx, migration)
	assert.False(t, done)
	require.ErrorIs(t, err, errSourcePowerOffTimedOut)
	assert.Empty(t, prov.ops(), "a timeout sends no Power call either")
}

// TestValidatingPhase_SourcePowerOffWaitThenExport drives the Validating
// phase: it waits (phase kept, no Power call) while the source is On, and
// moves to Exporting once the source is observed Off.
func TestValidatingPhase_SourcePowerOffWaitThenExport(t *testing.T) {
	ctx := context.Background()
	migration, objs := xnsMigration("")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	migration.Spec.Source.PowerOffBeforeMigration = true
	prov := &powerSpyProvider{}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)

	res, err := r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, migrationPowerOffPollInterval, res.RequeueAfter)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseValidating, got.Status.Phase, "waiting is not failing")
	assert.Empty(t, prov.ops())

	prov.poweredOff.Store(true)
	_, err = r.handleValidatingPhase(ctx, got)
	require.NoError(t, err)
	got = getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
	assert.Empty(t, prov.ops(), "the source was powered off by the VirtualMachine controller only")
}

// TestValidatingPhase_SourcePowerOffTimeoutFails: after the timeout the
// migration fails with the constant timeout message.
func TestValidatingPhase_SourcePowerOffTimeoutFails(t *testing.T) {
	ctx := context.Background()
	migration, objs := xnsMigration("")
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	migration.Spec.Source.PowerOffBeforeMigration = true
	migration.Status.Conditions = timedOutPowerOffWait()
	prov := &powerSpyProvider{}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)

	_, err := r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Equal(t, sourcePowerOffTimedOutMessage, got.Status.Message)
	assert.Empty(t, prov.ops())
}

// TestEnsureSourcePoweredOff_BudgetSurvivesValidatingFlaps: other checks of
// the Validating phase rewrite the Validating condition (a provider not
// ready, a binding or mount wait); the power-off budget is measured from the
// SourcePowerOff condition, which they never touch, so it is not restarted.
func TestEnsureSourcePoweredOff_BudgetSurvivesValidatingFlaps(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{}
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Source.PowerOffBeforeMigration = true
	r, c := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	_, _, err := r.ensureSourcePoweredOff(ctx, migration)
	require.NoError(t, err)
	stored := storedMigration(t, c, migration)
	started := meta.FindStatusCondition(stored.Status.Conditions, migrationConditionSourcePowerOff)
	require.NotNil(t, started)
	assert.Equal(t, metav1.ConditionFalse, started.Status)

	// Another check flaps Validating; the wait resumes.
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type: infrav1beta1.VMMigrationConditionValidating, Status: metav1.ConditionFalse,
		Reason: "TargetProviderNotReady", Message: "Target provider is not ready",
		LastTransitionTime: metav1.NewTime(time.Now().Add(time.Second)), ObservedGeneration: stored.Generation,
	})
	require.NoError(t, c.Status().Update(ctx, stored))
	_, _, err = r.ensureSourcePoweredOff(ctx, stored)
	require.NoError(t, err)
	resumed := meta.FindStatusCondition(storedMigration(t, c, migration).Status.Conditions, migrationConditionSourcePowerOff)
	require.NotNil(t, resumed)
	assert.True(t, started.LastTransitionTime.Equal(&resumed.LastTransitionTime), "the budget's start is kept")

	// Once the source is Off the gate is recorded True.
	prov.poweredOff.Store(true)
	done, _, err := r.ensureSourcePoweredOff(ctx, stored)
	require.NoError(t, err)
	assert.True(t, done)
	assert.True(t, meta.IsStatusConditionTrue(stored.Status.Conditions, migrationConditionSourcePowerOff))
}

// TestPendingPhase_RestartsThePowerOffBudget: a retried migration (Failed ->
// Pending -> Validating) gets a fresh power-off budget.
func TestPendingPhase_RestartsThePowerOffBudget(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Source.PowerOffBeforeMigration = true
	migration.Status.Phase = infrav1beta1.MigrationPhasePending
	migration.Status.Conditions = timedOutPowerOffWait()
	r, c := newSnapshotReconciler(t, &powerSpyProvider{}, sourceVM, sourceProvider, migration)

	_, err := r.handlePendingPhase(ctx, migration)
	require.NoError(t, err)
	got := storedMigration(t, c, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseValidating, got.Status.Phase)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, migrationConditionSourcePowerOff))
}

// notFoundSourceProvider answers that the source VM does not exist.
type notFoundSourceProvider struct {
	powerSpyProvider
	asError bool
}

func (p *notFoundSourceProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	if p.asError {
		return contracts.DescribeResponse{}, contracts.NewNotFoundError("domain not found", nil)
	}
	return contracts.DescribeResponse{Exists: false}, nil
}

// TestValidatingPhase_SourceNotFoundFailsFast: a source its provider does not
// find can never be observed Off, so the migration fails at once with a
// constant message instead of waiting out the budget.
func TestValidatingPhase_SourceNotFoundFailsFast(t *testing.T) {
	for name, asError := range map[string]bool{"Exists=false": false, "NotFound error": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			migration, objs := xnsMigration("")
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
			migration.Spec.Source.PowerOffBeforeMigration = true
			prov := &notFoundSourceProvider{asError: asError}
			r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)

			_, err := r.handleValidatingPhase(ctx, migration)
			require.NoError(t, err)
			got := getXNSMigration(t, r, migration)
			assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.Equal(t, sourceVMNotFoundMessage, got.Status.Message)
			assert.Empty(t, prov.ops())
		})
	}
}
