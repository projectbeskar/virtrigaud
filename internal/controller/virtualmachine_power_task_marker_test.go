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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin R34 (v0.4.0-rc4 validation on Proxmox): a provider whose
// power operations are asynchronous (Proxmox answers a UPID) made the manager
// set Reconfiguring=True "Adjusting power state" with status.lastTaskRef; when
// that task completed only lastTaskRef was cleared, so the VM sat in phase
// Reconfiguring for ever although it was Ready. vSphere and libvirt power is
// synchronous and never set the marker.

const powerTaskRef = "UPID:pve:0001:power"

// asyncPowerProvider reports describe, answers every Power with powerTaskRef,
// and answers IsTaskComplete by taskDone/taskErr.
type asyncPowerProvider struct {
	stubProvider
	describe contracts.DescribeResponse
	taskDone bool
	taskErr  error
	// asked holds the task refs IsTaskComplete was asked about.
	asked []string
}

func (p *asyncPowerProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	return p.describe, nil
}

func (p *asyncPowerProvider) Power(_ context.Context, _ contracts.VMRef, _ contracts.PowerOp) (string, error) {
	return powerTaskRef, nil
}

func (p *asyncPowerProvider) IsTaskComplete(_ context.Context, taskRef string) (bool, error) {
	p.asked = append(p.asked, taskRef)
	return p.taskDone, p.taskErr
}

// asyncPowerFixture is a bound single-host VM, spec.powerState desired, on a
// provider that reports it in the power state reported.
func asyncPowerFixture(t *testing.T, desired infravirtrigaudiov1beta1.PowerState, reported contracts.PowerState) (*VirtualMachineReconciler, *asyncPowerProvider, *infravirtrigaudiov1beta1.VirtualMachine) {
	t.Helper()
	prov := &asyncPowerProvider{describe: contracts.DescribeResponse{Exists: true, PowerState: string(reported), IPs: []string{"10.0.0.5"}}}
	r, vm := boundSingleHostVM(t, &stubResolver{provider: prov})
	vm.Spec.PowerState = desired
	require.NoError(t, r.Update(context.Background(), vm))
	// Recorded at the class's size (4 vCPU, 8 GiB): no reconfigure is due, so
	// only the power task and its marker are under test.
	cpu, mem := int32(4), int64(8192)
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
	require.NoError(t, r.Status().Update(context.Background(), vm))
	return r, prov, vm
}

// reconcileOnce reconciles vm and returns the VM as stored.
func reconcileOnce(t *testing.T, r *VirtualMachineReconciler, vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	ctx := context.Background()
	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), got))
	return got
}

func reconfiguringOf(vm *infravirtrigaudiov1beta1.VirtualMachine) *metav1.Condition {
	return meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
}

// TestReconcileVM_AsyncPowerTask_PhaseReconfiguringThenRunning: the power
// operation answers a task: the VM is Reconfiguring while it runs and Running,
// with the marker settled, once it has completed.
func TestReconcileVM_AsyncPowerTask_PhaseReconfiguringThenRunning(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOff)

	got := reconcileOnce(t, r, vm)
	assert.Equal(t, powerTaskRef, got.Status.LastTaskRef)
	c := reconfiguringOf(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring, got.Status.Phase)

	// The task is still running: nothing changes.
	got = reconcileOnce(t, r, got)
	assert.Equal(t, powerTaskRef, got.Status.LastTaskRef)
	assert.True(t, powerAdjusting(got), "the marker stays while the task runs")
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring, got.Status.Phase)

	// The task completed and the VM is on.
	prov.taskDone = true
	prov.describe.PowerState = string(contracts.PowerStateOn)
	got = reconcileOnce(t, r, got)
	assert.Empty(t, got.Status.LastTaskRef)
	c = reconfiguringOf(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonReconcileSuccess, c.Reason)
	assert.Equal(t, got.Generation, c.ObservedGeneration)
	ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseRunning, got.Status.Phase)
}

// TestReconcileVM_AsyncPowerTask_PowerOffEndsStopped: the same for a power-off:
// the VM ends Stopped, not Reconfiguring.
func TestReconcileVM_AsyncPowerTask_PowerOffEndsStopped(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOff, contracts.PowerStateOn)
	got := reconcileOnce(t, r, vm)
	require.True(t, powerAdjusting(got))

	prov.taskDone = true
	prov.describe.PowerState = string(contracts.PowerStateOff)
	got = reconcileOnce(t, r, got)
	assert.False(t, powerAdjusting(got))
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseStopped, got.Status.Phase)
}

// TestReconcileVM_AsyncPowerTask_FailedTaskSurfacedAndSettled: a power task
// that ends in failure is surfaced on Ready and Reconfiguring, forgotten, and
// the VM is not left in phase Reconfiguring.
func TestReconcileVM_AsyncPowerTask_FailedTaskSurfacedAndSettled(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOff)
	got := reconcileOnce(t, r, vm)
	require.True(t, powerAdjusting(got))

	// A failed task is answered done together with its error.
	prov.taskDone = true
	prov.taskErr = errors.New("task failed: start failed: not enough memory")
	got = reconcileOnce(t, r, got)

	assert.Empty(t, got.Status.LastTaskRef, "the dead task is forgotten, not polled for ever")
	c := reconfiguringOf(got)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonProviderError, c.Reason)
	assert.Equal(t, got.Generation, c.ObservedGeneration)
	ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonProviderError, ready.Reason)
	assert.Contains(t, ready.Message, "Power state change failed")
	assert.NotEqual(t, infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring, got.Status.Phase)
}

// TestReconcileVM_PowerTaskErrorWithoutMarkerKeepsHistoricalHandling: a task
// that errors while the Reconfiguring condition is not the power marker (a
// create task, say) keeps its historical handling: Provisioning=False with the
// task still recorded.
func TestReconcileVM_PowerTaskErrorWithoutMarkerKeepsHistoricalHandling(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOn)
	vm.Status.LastTaskRef = "create-task"
	require.NoError(t, r.Status().Update(context.Background(), vm))
	prov.taskDone = true
	prov.taskErr = errors.New("task failed: boom")

	got := reconcileOnce(t, r, vm)
	assert.Equal(t, "create-task", got.Status.LastTaskRef)
	prov1 := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionProvisioning)
	require.NotNil(t, prov1)
	assert.Equal(t, metav1.ConditionFalse, prov1.Status)
	assert.Equal(t, k8s.ReasonProviderError, prov1.Reason)
}

// TestReconcileVM_PowerTaskDoneKeepsRealReconfigureCondition: the completion of
// the power task never clobbers a Reconfiguring condition that belongs to a
// real reconfigure.
func TestReconcileVM_PowerTaskDoneKeepsRealReconfigureCondition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reason  string
		message string
	}{
		{"resize pending power off", k8s.ReasonResizePendingPowerOff, "power the VM off to resize it"},
		{"shrink pending power off", k8s.ReasonShrinkPendingPowerOff, "power the VM off to shrink it"},
		{"insufficient host capacity", k8s.ReasonInsufficientHostCapacity, "the host has no room"},
		{"updating, a reconfigure message", k8s.ReasonUpdating, "VM reconfiguration in progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOn)
			vm.Status.LastTaskRef = powerTaskRef
			k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, tc.reason, tc.message, vm.Generation)
			require.NoError(t, r.Status().Update(context.Background(), vm))
			prov.taskDone = true

			got := reconcileOnce(t, r, vm)
			assert.Empty(t, got.Status.LastTaskRef, "the finished power task is forgotten")
			c := reconfiguringOf(got)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status, "the reconfigure's condition is untouched")
			assert.Equal(t, tc.reason, c.Reason)
			assert.Equal(t, tc.message, c.Message)
		})
	}
}

// TestReconcileVM_PowerTaskDoneWithReconfigureTaskKeepsCondition: with a
// reconfigure task recorded, a power marker is not settled by the power task's
// completion.
func TestReconcileVM_PowerTaskDoneWithReconfigureTaskKeepsCondition(t *testing.T) {
	r, prov, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOn)
	vm.Status.LastTaskRef = powerTaskRef
	vm.Status.ReconfigureTaskRef = "UPID:pve:0002:reconfigure"
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, powerAdjustingMessage, vm.Generation)
	require.NoError(t, r.Status().Update(context.Background(), vm))
	prov.taskDone = false // neither task has ended

	got := reconcileOnce(t, r, vm)
	assert.Equal(t, powerTaskRef, got.Status.LastTaskRef)
	assert.Equal(t, "UPID:pve:0002:reconfigure", got.Status.ReconfigureTaskRef)
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring, got.Status.Phase)
}

// TestReconcileVM_StalePowerMarkerFromOlderManagerIsSettled: a status an older
// manager left with the marker and no task (the rc4 bug) heals on the next
// reconcile.
func TestReconcileVM_StalePowerMarkerFromOlderManagerIsSettled(t *testing.T) {
	r, _, vm := asyncPowerFixture(t, infravirtrigaudiov1beta1.PowerStateOn, contracts.PowerStateOn)
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, powerAdjustingMessage, vm.Generation)
	vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring
	require.NoError(t, r.Status().Update(context.Background(), vm))

	got := reconcileOnce(t, r, vm)
	assert.False(t, powerAdjusting(got))
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseRunning, got.Status.Phase)
}

// TestPowerAdjusting: only the exact marker counts.
func TestPowerAdjusting(t *testing.T) {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	assert.False(t, powerAdjusting(vm), "no condition")
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, powerAdjustingMessage, 1)
	assert.True(t, powerAdjusting(vm))
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonUpdating, powerAdjustingMessage, 1)
	assert.False(t, powerAdjusting(vm), "settled")
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonTaskInProgress, powerAdjustingMessage, 1)
	assert.False(t, powerAdjusting(vm), "another reason")
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonRestartRequired, powerAdjustingMessage, 1)
	assert.False(t, powerAdjusting(vm), "RestartRequired")
	k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, "VM reconfiguration in progress", 1)
	assert.False(t, powerAdjusting(vm), "another message")
}
