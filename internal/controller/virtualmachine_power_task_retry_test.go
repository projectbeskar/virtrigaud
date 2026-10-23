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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the PR #425 review findings about a power task that ends in
// failure (M1, M2) and about the "Adjusting power state" marker taking the
// place of a real reconfigure's condition (L3).

var errPowerTask = errors.New("task failed: start failed: not enough memory")

// failPowerTask makes the VM's power task end in failure and reconciles it.
func failPowerTask(t *testing.T, r *VirtualMachineReconciler, prov *asyncPowerProvider, vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	prov.taskDone, prov.taskErr = true, errPowerTask
	got := reconcileOnce(t, r, vm)
	prov.taskDone, prov.taskErr = false, nil
	return got
}

// TestReconcileVM_FailedPowerTask_NoReconfigureIsSent (M1): the failure is on
// Ready, and the Reconfiguring condition is settled with its own reason, not
// ProviderError, which pendingReconfigureRecheck reads as a failed Reconfigure
// to send again: no Reconfigure follows a failed power task.
func TestReconcileVM_FailedPowerTask_NoReconfigureIsSent(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOff)
	got := reconcileOnce(t, r, vm)
	require.True(t, powerAdjusting(got))

	got = failPowerTask(t, r, prov, got)
	c := reconfiguringOf(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonPowerTaskFailed, c.Reason)
	ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonProviderError, ready.Reason)

	// The VM was started by other means; the next reconcile finds its power
	// state satisfied and reaches the reconfigure check.
	prov.describe.PowerState = string(contracts.PowerStateOn)
	got = reconcileOnce(t, r, got)
	assert.Zero(t, prov.reconfigures, "a failed power task never makes the manager send a Reconfigure")
	assert.NotEqual(t, infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring, got.Status.Phase)
}

// TestReconcileVM_FailedPowerTask_Backoff (M2): a power operation whose task
// keeps failing is sent again on a per-VM backoff, 5 s doubling to 5 min, not
// on every reconcile; a power task that ends well resets it.
func TestReconcileVM_FailedPowerTask_Backoff(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOff)
	now := time.Unix(1_800_000_000, 0)
	r.clock = func() time.Time { return now }
	advance := func(d time.Duration) { now = now.Add(d) }

	got := reconcileOnce(t, r, vm)
	require.Equal(t, 1, prov.powers)

	// First failure: 5 s.
	got = failPowerTask(t, r, prov, got)
	got = reconcileOnce(t, r, got)
	assert.Equal(t, 1, prov.powers, "not sent again at once")
	advance(2 * time.Second)
	got = reconcileOnce(t, r, got)
	assert.Equal(t, 1, prov.powers, "nor within the wait")
	advance(4 * time.Second)
	got = reconcileOnce(t, r, got)
	assert.Equal(t, 2, prov.powers, "sent again after 5 s")

	// Second failure: 10 s.
	got = failPowerTask(t, r, prov, got)
	advance(6 * time.Second)
	got = reconcileOnce(t, r, got)
	assert.Equal(t, 2, prov.powers, "the wait doubled")
	advance(5 * time.Second)
	got = reconcileOnce(t, r, got)
	assert.Equal(t, 3, prov.powers)

	// The task ends well: the backoff starts over.
	prov.taskDone = true
	prov.describe.PowerState = string(contracts.PowerStateOn)
	got = reconcileOnce(t, r, got)
	assert.Empty(t, got.Status.LastTaskRef)
	assert.Zero(t, r.powerRetry.remaining(vmSchedulingUID(got), now))
	assert.Equal(t, providerErrorRetryInterval, r.powerRetry.nextWithin(vmSchedulingUID(got), now, providerErrorRetryInterval, reconfigureRetryMaxInterval),
		"a later failure waits 5 s again")
}

// TestReconcileVM_FailedPowerTask_BackoffEndsWhenSpecChanges: a spec change
// since the failure sends the power operation at once.
func TestReconcileVM_FailedPowerTask_BackoffEndsWhenSpecChanges(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOff)
	now := time.Unix(1_800_000_000, 0)
	r.clock = func() time.Time { return now }

	got := reconcileOnce(t, r, vm)
	got = failPowerTask(t, r, prov, got)
	got = reconcileOnce(t, r, got)
	require.Equal(t, 1, prov.powers)

	got.Generation++
	require.NoError(t, r.Update(context.Background(), got))
	got = reconcileOnce(t, r, got)
	assert.Equal(t, 2, prov.powers, "the spec changed since the failure: sent at once")
}

// TestReconcileVM_PowerTask_KeepsRealReconfigureCondition (L3): starting a
// power task does not overwrite the condition of a real reconfigure, and a
// power task that ends in failure is still recognised and surfaced.
func TestReconcileVM_PowerTask_KeepsRealReconfigureCondition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status metav1.ConditionStatus
		reason string
	}{
		{"restart required", metav1.ConditionTrue, k8s.ReasonRestartRequired},
		{"resize pending power off", metav1.ConditionFalse, k8s.ReasonResizePendingPowerOff},
		{"failed reconfigure", metav1.ConditionFalse, k8s.ReasonProviderError},
		{"no honest reconfigure", metav1.ConditionFalse, k8s.ReasonProviderLacksHonestReconfigure},
		{"reconfigure in progress", metav1.ConditionTrue, k8s.ReasonTaskInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOff)
			k8s.SetReconfiguringCondition(&vm.Status.Conditions, tc.status, tc.reason, "kept", vm.Generation)
			require.NoError(t, r.Status().Update(context.Background(), vm))

			got := reconcileOnce(t, r, vm)
			require.Equal(t, powerTaskRef, got.Status.LastTaskRef, "the power operation was sent")
			c := reconfiguringOf(got)
			require.NotNil(t, c)
			assert.Equal(t, tc.status, c.Status)
			assert.Equal(t, tc.reason, c.Reason)
			assert.Equal(t, "kept", c.Message)
			assert.False(t, powerAdjusting(got))

			// The task ends in failure: recognised without the marker.
			got = failPowerTask(t, r, prov, got)
			assert.Empty(t, got.Status.LastTaskRef)
			c = reconfiguringOf(got)
			require.NotNil(t, c)
			assert.Equal(t, tc.reason, c.Reason, "the reconfigure's condition is still untouched")
			ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Contains(t, ready.Message, "Power state change failed")
		})
	}
}

// TestMayMarkPowerAdjusting: the marker takes the place of no condition, the
// marker itself, or a settled one, and never of a reconfigure's.
func TestMayMarkPowerAdjusting(t *testing.T) {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	assert.True(t, mayMarkPowerAdjusting(vm), "no condition")

	set := func(status metav1.ConditionStatus, reason, message string) {
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, status, reason, message, 1)
	}
	set(metav1.ConditionTrue, k8s.ReasonUpdating, powerAdjustingMessage)
	assert.True(t, mayMarkPowerAdjusting(vm), "the marker")
	set(metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM reconfigured successfully")
	assert.True(t, mayMarkPowerAdjusting(vm), "settled")
	set(metav1.ConditionFalse, k8s.ReasonPowerTaskFailed, "x")
	assert.True(t, mayMarkPowerAdjusting(vm), "settled after a failed power task")
	set(metav1.ConditionTrue, k8s.ReasonUpdating, "VM reconfiguration in progress")
	assert.False(t, mayMarkPowerAdjusting(vm), "a reconfigure in flight")
	set(metav1.ConditionFalse, k8s.ReasonProviderError, "x")
	assert.False(t, mayMarkPowerAdjusting(vm), "a failed Reconfigure")
	set(metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "ok")
	vm.Status.ReconfigureTaskRef = "UPID:pve:1:reconfigure"
	assert.False(t, mayMarkPowerAdjusting(vm), "a recorded reconfigure task")
}
