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
	stderrors "errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// asyncDeleteProvider is a provider whose Delete answers a task reference (or
// a synchronous result) and whose TaskStatus answers are scripted. It counts
// both RPCs.
type asyncDeleteProvider struct {
	stubProvider
	mu sync.Mutex
	// deleteTask is the task reference Delete answers ("" = synchronous).
	deleteTask string
	// deleteErr is the error Delete answers.
	deleteErr error
	// status and statusErr are what TaskStatus answers.
	status    contracts.TaskStatus
	statusErr error
	// taskRefs lists the references TaskStatus was asked about.
	taskRefs    []string
	deleteCalls atomic.Int32
	statusCalls atomic.Int32
	// desc and descErr are what Describe answers (default: the VM exists).
	desc    contracts.DescribeResponse
	descErr error
	// describeRefs lists the addresses Describe was asked about.
	describeRefs []contracts.VMRef
	// isTaskCompleteCalls counts IsTaskComplete, which the delete path must
	// never use: TaskStatus tells a failed task from an unknown outcome.
	isTaskCompleteCalls atomic.Int32
}

func (p *asyncDeleteProvider) Delete(_ context.Context, _ contracts.VMRef) (string, error) {
	p.deleteCalls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deleteTask, p.deleteErr
}

func (p *asyncDeleteProvider) TaskStatus(_ context.Context, ref string) (contracts.TaskStatus, error) {
	p.statusCalls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.taskRefs = append(p.taskRefs, ref)
	return p.status, p.statusErr
}

func (p *asyncDeleteProvider) IsTaskComplete(_ context.Context, _ string) (bool, error) {
	p.isTaskCompleteCalls.Add(1)
	return false, stderrors.New("IsTaskComplete must not be called on the delete path")
}

func (p *asyncDeleteProvider) Describe(_ context.Context, vm contracts.VMRef) (contracts.DescribeResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.describeRefs = append(p.describeRefs, vm)
	return p.desc, p.descErr
}

func (p *asyncDeleteProvider) answerDescribe(desc contracts.DescribeResponse, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.desc, p.descErr = desc, err
}

func (p *asyncDeleteProvider) describeCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.describeRefs)
}

func (p *asyncDeleteProvider) answer(st contracts.TaskStatus, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.statusErr = st, err
}

func (p *asyncDeleteProvider) answerDelete(task string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteTask, p.deleteErr = task, err
}

// statusWriteCounter counts the VirtualMachine status writes (update or
// patch) and can fail the next n of them.
type statusWriteCounter struct {
	writes atomic.Int32
	failN  atomic.Int32
}

func (c *statusWriteCounter) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine); ok && sub == "status" {
				c.writes.Add(1)
				if c.failN.Load() > 0 {
					c.failN.Add(-1)
					return apierrors.NewServiceUnavailable("injected status write failure")
				}
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine); ok && sub == "status" {
				c.writes.Add(1)
				if c.failN.Load() > 0 {
					c.failN.Add(-1)
					return apierrors.NewServiceUnavailable("injected status write failure")
				}
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
}

// asyncDeleteFixture is a single-host VM being deleted, a reconciler on a fake
// client that counts status writes, and a provider whose Delete answers a task.
type asyncDeleteFixture struct {
	t        *testing.T
	client   client.Client
	r        *VirtualMachineReconciler
	prov     *asyncDeleteProvider
	resolver *stubResolver
	counter  *statusWriteCounter
	recorder *record.FakeRecorder
	key      types.NamespacedName
	now      time.Time
}

func newAsyncDeleteFixture(t *testing.T, mutate func(*infravirtrigaudiov1beta1.VirtualMachine)) *asyncDeleteFixture {
	t.Helper()
	s := coverageTestScheme(t)
	vm := deletionVM("vm-async")
	vm.UID = "uid-vm-async"
	if mutate != nil {
		mutate(vm)
	}
	counter := &statusWriteCounter{}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(vm, deletionProviderCR()).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementHostIndex, placementHostIndexValue).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).
		WithInterceptorFuncs(counter.funcs()).
		Build()
	prov := &asyncDeleteProvider{deleteTask: "task-delete-1", desc: contracts.DescribeResponse{Exists: true, PowerState: "Off"}}
	f := &asyncDeleteFixture{
		t: t, client: fc, prov: prov, counter: counter,
		resolver: &stubResolver{provider: prov},
		recorder: record.NewFakeRecorder(100),
		key:      client.ObjectKeyFromObject(vm),
		now:      time.Now(),
	}
	f.r = f.newReconciler()
	require.NoError(t, fc.Delete(context.Background(), vm))
	// The clock starts at the deletion, so backoffs counted from it are exact.
	f.now = f.get().DeletionTimestamp.Time
	return f
}

// newReconciler is a fresh reconciler on the fixture's client: nothing in
// memory, as after a manager restart.
func (f *asyncDeleteFixture) newReconciler() *VirtualMachineReconciler {
	return &VirtualMachineReconciler{
		Client: f.client, Scheme: f.client.Scheme(), RemoteResolver: f.resolver, Recorder: f.recorder,
		clock:         func() time.Time { return f.now },
		recordBackoff: &wait.Backoff{Steps: 1, Duration: time.Millisecond},
	}
}

// get returns the stored VM, or nil once it is gone.
func (f *asyncDeleteFixture) get() *infravirtrigaudiov1beta1.VirtualMachine {
	f.t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := f.client.Get(context.Background(), f.key, vm); err != nil {
		require.True(f.t, apierrors.IsNotFound(err), "%v", err)
		return nil
	}
	return vm
}

// reconcile runs handleDeletion on the stored VM with r.
func (f *asyncDeleteFixture) reconcile(r *VirtualMachineReconciler) ctrl.Result {
	f.t.Helper()
	vm := f.get()
	require.NotNil(f.t, vm, "the VM must still exist")
	res, err := r.handleDeletion(context.Background(), vm)
	require.NoError(f.t, err)
	require.Zero(f.t, f.prov.isTaskCompleteCalls.Load(), "the delete path polls with TaskStatus only")
	return res
}

// requireHeld asserts the VM is still present with its finalizer.
func (f *asyncDeleteFixture) requireHeld() *infravirtrigaudiov1beta1.VirtualMachine {
	f.t.Helper()
	vm := f.get()
	require.NotNil(f.t, vm, "the VM must not be released")
	require.Contains(f.t, vm.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	return vm
}

func (f *asyncDeleteFixture) events() []string { return eventsOf(f.recorder.Events) }

func deleteTaskCondition(vm *infravirtrigaudiov1beta1.VirtualMachine) *metav1.Condition {
	return meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDeleteTask)
}

// 1. A synchronous Delete releases the finalizer at once, with no TaskStatus
// and no status write beyond phase Deleting.
func TestAsyncDelete_SynchronousDeleteUnchanged(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.prov.answerDelete("", nil)
	res := f.reconcile(f.r)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Nil(t, f.get(), "a synchronous Delete releases the finalizer at once")
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	assert.Zero(t, f.prov.statusCalls.Load(), "no TaskStatus for a synchronous Delete")
	assert.EqualValues(t, 1, f.counter.writes.Load(), "only phase Deleting is written")
	assert.Empty(t, f.events())
}

// 12. A VM whose status.lastTaskRef holds a task from before the deletion (a
// power task: no DeleteTask condition) is deleted exactly as before.
func TestAsyncDelete_LeftoverPowerTaskIsNotADeleteTask(t *testing.T) {
	f := newAsyncDeleteFixture(t, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.LastTaskRef = "power-task-7"
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	})
	f.prov.answerDelete("", nil)
	f.reconcile(f.r)
	assert.Nil(t, f.get())
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	assert.Zero(t, f.prov.statusCalls.Load(), "a leftover power task is never polled on the delete path")
	assert.Zero(t, f.counter.writes.Load(), "no status write at all for a synchronous Delete")
}

// 2 and 9. A Delete answered with a task keeps the finalizer, records the
// task durably, and later reconciles poll it without sending Delete again
// or rewriting the status.
func TestAsyncDelete_TaskRunningKeepsFinalizer(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)

	res := f.reconcile(f.r)
	assert.Equal(t, vmDeleteTaskPollMin, res.RequeueAfter)
	vm := f.requireHeld()
	assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef, "the task is recorded in status")
	c := deleteTaskCondition(vm)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, k8s.ReasonTaskInProgress, c.Reason)
	assert.Equal(t, vm.Generation, c.ObservedGeneration)
	ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonDeleting, ready.Reason)
	assert.Equal(t, infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting, vm.Status.Phase)
	assert.Zero(t, f.prov.statusCalls.Load(), "the task is polled only after its record is stored")
	writes := f.counter.writes.Load()

	for i := 0; i < 5; i++ {
		res = f.reconcile(f.r)
		assert.GreaterOrEqual(t, res.RequeueAfter, vmDeleteTaskPollMin)
		assert.LessOrEqual(t, res.RequeueAfter, vmDeleteTaskPollMax)
	}
	f.requireHeld()
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "no second Delete while the task runs")
	assert.EqualValues(t, 5, f.prov.statusCalls.Load(), "one TaskStatus per reconcile")
	assert.Equal(t, writes, f.counter.writes.Load(), "an unchanged poll writes no status")
	for _, ref := range f.prov.taskRefs {
		assert.Equal(t, "task-delete-1", ref)
	}
}

// 3. The task completed successfully: the finalizer is removed.
func TestAsyncDelete_TaskCompletedReleases(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	f.reconcile(f.r)
	f.reconcile(f.r)
	f.requireHeld()

	f.prov.answer(contracts.TaskStatus{IsCompleted: true}, nil)
	res := f.reconcile(f.r)
	assert.Equal(t, ctrl.Result{}, res)
	assert.Nil(t, f.get(), "a completed Delete task releases the finalizer")
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	assert.EqualValues(t, 2, f.prov.statusCalls.Load())
	_, remembered := f.r.deleteTasks.Load("uid-vm-async")
	assert.False(t, remembered, "the release forgets the task")
}

// 4. A task that ended in failure clears the reference, says so, and the
// Delete is sent again only after the backoff.
func TestAsyncDelete_TaskFailedRetriesDeleteAfterBackoff(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.reconcile(f.r) // Delete -> task recorded
	f.prov.answer(contracts.TaskStatus{IsCompleted: true, Error: "Destroy_Task failed: file locked"}, nil)

	res := f.reconcile(f.r)
	assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
	vm := f.requireHeld()
	assert.Empty(t, vm.Status.LastTaskRef, "a failed task is no longer recorded")
	c := deleteTaskCondition(vm)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonDeleteTaskFailed, c.Reason)
	assert.NotContains(t, c.Message, "file locked", "the provider's answer goes to the log only")
	assert.Contains(t, c.Message, forceDeleteAnnotation, "the message names the recovery path")
	assert.Equal(t, 1, countPrefixed(f.events(), "Warning "+eventReasonDeleteTaskFailed))

	// A reconcile before the backoff is over sends nothing.
	f.now = f.now.Add(5 * time.Second)
	res = f.reconcile(f.r)
	assert.Equal(t, 10*time.Second, res.RequeueAfter)
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "no Delete before the backoff is over")
	assert.EqualValues(t, 1, f.prov.statusCalls.Load())

	// After it, the Delete is sent again; a VM already gone answers NotFound.
	f.now = f.now.Add(11 * time.Second)
	f.prov.answerDelete("", contracts.NewNotFoundError("delete: VM not found", nil))
	f.reconcile(f.r)
	assert.EqualValues(t, 2, f.prov.deleteCalls.Load())
	assert.Nil(t, f.get(), "a Delete that finds the VM gone releases the finalizer")
}

// 4 (continued). Each failed task waits as long as the deletion has taken so
// far (at least 15 s, at most 5 min): the waits grow.
func TestAsyncDelete_RepeatedTaskFailureBacksOff(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	failed := contracts.TaskStatus{IsCompleted: true, Error: "failed"}
	f.reconcile(f.r)
	f.prov.answer(failed, nil)
	assert.Equal(t, vmDeleteRetryInterval, f.reconcile(f.r).RequeueAfter)
	f.now = f.now.Add(40 * time.Second)
	f.reconcile(f.r) // Delete again -> new task recorded
	assert.EqualValues(t, 2, f.prov.deleteCalls.Load())
	assert.Equal(t, "task-delete-1", f.requireHeld().Status.LastTaskRef)
	assert.Equal(t, 40*time.Second, f.reconcile(f.r).RequeueAfter, "the second failure waits as long as the delete took so far")
	f.now = f.now.Add(time.Hour)
	f.reconcile(f.r)
	assert.EqualValues(t, 3, f.prov.deleteCalls.Load())
	assert.Equal(t, blockedRetryMax, f.reconcile(f.r).RequeueAfter, "bounded")
}

// S3. The failure backoff is anchored on the DeleteTask condition: a
// restarted manager does not send the Delete before it is over.
func TestAsyncDelete_RestartAfterTaskFailedKeepsBackoff(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.reconcile(f.r)
	f.prov.answer(contracts.TaskStatus{IsCompleted: true, Error: "failed"}, nil)
	f.reconcile(f.r)
	vm := f.requireHeld()
	require.Equal(t, metav1.ConditionFalse, deleteTaskCondition(vm).Status)

	restarted := f.newReconciler()
	f.now = f.now.Add(5 * time.Second)
	res := f.reconcile(restarted)
	assert.InDelta(t, float64(10*time.Second), float64(res.RequeueAfter), float64(time.Second),
		"the remaining wait comes from the stored condition")
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "no Delete before the backoff is over, after a restart too")

	f.now = f.now.Add(11 * time.Second)
	f.prov.answerDelete("", nil)
	f.reconcile(restarted)
	assert.EqualValues(t, 2, f.prov.deleteCalls.Load())
	assert.Nil(t, f.get())
}

// N1. A Delete sent again after a failed task that then fails synchronously
// (or is refused) replaces the stale DeleteTask=False and its Ready message.
func TestAsyncDelete_StaleFailedConditionClearedOnRetry(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"ordinary failure": {err: stderrors.New("VM is running - destroy failed"), reason: k8s.ReasonProviderError},
		"disk in use": {err: contracts.NewConflictError("delete: in use",
			fmt.Errorf("%w: x", contracts.ErrVMDiskInUse)), reason: k8s.ReasonDeleteBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAsyncDeleteFixture(t, nil)
			f.reconcile(f.r)
			f.prov.answer(contracts.TaskStatus{IsCompleted: true, Error: "failed"}, nil)
			f.reconcile(f.r)
			f.now = f.now.Add(vmDeleteRetryInterval)
			f.prov.answerDelete("", tc.err)
			f.reconcile(f.r)
			vm := f.requireHeld()
			assert.EqualValues(t, 2, f.prov.deleteCalls.Load())
			assert.Nil(t, deleteTaskCondition(vm), "the failed task's condition is gone")
			ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.reason, ready.Reason)
			assert.NotEqual(t, deleteTaskFailedMessage, ready.Message)
			// The next attempt is the ordinary retry, not the task backoff.
			f.now = f.now.Add(vmDeleteRetryInterval)
			f.reconcile(f.r)
			assert.EqualValues(t, 3, f.prov.deleteCalls.Load())
		})
	}
}

// 5. A TaskStatus error is an unknown outcome: the task stays recorded, the
// finalizer is kept, no Delete is sent and the status is written once.
func TestAsyncDelete_UnknownOutcomeKeepsTask(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.reconcile(f.r)
	f.prov.answer(contracts.TaskStatus{}, contracts.NewRetryableError("taskStatus: connection reset", nil))

	waits := []time.Duration{}
	for i := 0; i < 4; i++ {
		waits = append(waits, f.reconcile(f.r).RequeueAfter)
	}
	assert.Equal(t, []time.Duration{blockedRetryMin, 2 * blockedRetryMin, 4 * blockedRetryMin, 8 * blockedRetryMin}, waits)
	vm := f.requireHeld()
	assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef, "an unknown outcome keeps the task")
	c := deleteTaskCondition(vm)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, k8s.ReasonDeleteTaskStatusUnknown, c.Reason)
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "a transient TaskStatus error never re-sends Delete")
	assert.Equal(t, 1, countPrefixed(f.events(), "Warning "+eventReasonDeleteTaskStatusUnknown), "one event per transition")
	writesAfterUnknown := f.counter.writes.Load()

	// The provider answers again: running, then completed.
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	f.reconcile(f.r)
	assert.Equal(t, k8s.ReasonTaskInProgress, deleteTaskCondition(f.requireHeld()).Reason)
	assert.Equal(t, writesAfterUnknown+1, f.counter.writes.Load())
	f.prov.answer(contracts.TaskStatus{IsCompleted: true}, nil)
	f.reconcile(f.r)
	assert.Nil(t, f.get())
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
}

// 5 (backoff ceiling). The unknown-outcome backoff is bounded.
func TestAsyncDelete_UnknownOutcomeBackoffIsBounded(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.reconcile(f.r)
	f.prov.answer(contracts.TaskStatus{}, stderrors.New("taskStatus failed: deadline exceeded"))
	var last time.Duration
	for i := 0; i < 10; i++ {
		last = f.reconcile(f.r).RequeueAfter
	}
	assert.Equal(t, blockedRetryMax, last)
	f.requireHeld()
}

// 6. The provider becomes unavailable while a task is recorded: the finalizer
// and the record are kept, nothing is sent; when it is back the same task is
// polled.
func TestAsyncDelete_ProviderUnavailableKeepsTask(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	f.reconcile(f.r)

	f.resolver.err = stderrors.New("provider runtime not running")
	res := f.reconcile(f.r)
	assert.Greater(t, res.RequeueAfter, time.Duration(0))
	vm := f.requireHeld()
	assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef, "the task survives the provider's absence")
	assert.True(t, meta.IsStatusConditionTrue(vm.Status.Conditions, k8s.ConditionDeleteTask))
	assert.Zero(t, f.prov.statusCalls.Load())

	f.resolver.err = nil
	f.prov.answer(contracts.TaskStatus{IsCompleted: true}, nil)
	f.reconcile(f.r)
	assert.Nil(t, f.get())
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "no second Delete after the provider came back")
	assert.Equal(t, []string{"task-delete-1"}, f.prov.taskRefs)
}

// 7. A manager restart during deletion: a new reconciler, with nothing in
// memory, resumes the recorded task from the API server's state alone.
func TestAsyncDelete_ManagerRestartResumesTask(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	f.reconcile(f.r)
	f.reconcile(f.r)

	restarted := f.newReconciler()
	writes := f.counter.writes.Load()
	f.reconcile(restarted)
	f.requireHeld()
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "a restarted manager does not send Delete again")
	assert.EqualValues(t, 2, f.prov.statusCalls.Load())
	assert.Equal(t, writes, f.counter.writes.Load(), "a restarted manager's poll writes nothing new")

	f.prov.answer(contracts.TaskStatus{IsCompleted: true}, nil)
	f.reconcile(restarted)
	assert.Nil(t, f.get())
}

// staleAnswers are the TaskStatus answers with which a provider says it does
// not know a task: ambiguous, never a failure that re-sends Delete.
var staleAnswers = map[string]struct {
	st  contracts.TaskStatus
	err error
}{
	"not found error":     {err: contracts.NewNotFoundError("taskStatus: task host is not in the registry", nil)},
	"invalid reference":   {err: contracts.NewInvalidSpecError("taskStatus: not host-encoded", nil)},
	"unsupported":         {err: contracts.NewNotSupportedError("taskStatus: not implemented")},
	"terminal not found":  {st: contracts.TaskStatus{IsCompleted: true, Error: "task not found"}},
	"not reported by PVE": {st: contracts.TaskStatus{IsCompleted: true, Error: "PVE does not report this task: 400"}},
	"invalid task id":     {st: contracts.TaskStatus{IsCompleted: true, Error: "invalid task ID format: x"}},
	"vSphere purged task": {st: contracts.TaskStatus{IsCompleted: true, Error: "task failed: ServerFaultCode: The object " +
		"'vim.Task:task-4211' has already been deleted or has not been completely created"}},
}

// 8. A stale reference — the provider no longer knows the task (a restarted
// provider, a reference it never issued) — is ambiguous: DeleteTask=Unknown,
// no Delete is sent again, and an owner-checked Describe that reports the VM
// absent (or another owner's) releases it.
func TestAsyncDelete_StaleTaskReferenceConfirmedAbsent(t *testing.T) {
	absentAnswers := map[string]struct {
		desc contracts.DescribeResponse
		err  error
	}{
		"exists false": {desc: contracts.DescribeResponse{Exists: false}},
		"not found":    {err: contracts.NewNotFoundError("describe: VM not found", nil)},
		"not owned":    {err: contracts.NewNotFoundError("describe: not ours", fmt.Errorf("%w: x", contracts.ErrVMNotOwned))},
	}
	for name, answer := range staleAnswers {
		for absentName, absent := range absentAnswers {
			t.Run(name+"/"+absentName, func(t *testing.T) {
				f := newAsyncDeleteFixture(t, nil)
				f.reconcile(f.r)
				f.prov.answer(answer.st, answer.err)
				f.prov.answerDescribe(absent.desc, absent.err)
				f.reconcile(f.newReconciler())
				assert.Nil(t, f.get(), "the VM is confirmed absent")
				assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "an ambiguous answer never re-sends Delete")
				require.Equal(t, 1, f.prov.describeCalls())
				assert.Equal(t, "uid-vm-async", f.prov.describeRefs[0].Owner.UID, "the Describe is owner-checked")
				events := f.events()
				assert.Equal(t, 1, countPrefixed(events, "Warning "+eventReasonDeleteTaskOutcomeUnknown))
				if absentName == "not owned" {
					assert.Equal(t, 1, countPrefixed(events, "Warning "+eventReasonVMLeftOnHypervisor))
				} else {
					assert.Equal(t, 1, countPrefixed(events, "Normal "+eventReasonDeleteConfirmedAbsent))
				}
			})
		}
	}
}

// 8 (still present). The VM is still there (or Describe fails): the finalizer
// is kept with DeleteTask=Unknown, nothing is deleted again, only Describe is
// repeated with a bounded backoff — also after a manager restart.
func TestAsyncDelete_StaleTaskReferenceVMStillPresent(t *testing.T) {
	for name, answer := range staleAnswers {
		t.Run(name, func(t *testing.T) {
			f := newAsyncDeleteFixture(t, nil)
			f.reconcile(f.r)
			f.prov.answer(answer.st, answer.err)
			res := f.reconcile(f.r)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			vm := f.requireHeld()
			c := deleteTaskCondition(vm)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionUnknown, c.Status)
			assert.Equal(t, k8s.ReasonDeleteTaskOutcomeUnknown, c.Reason)
			assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef, "the reference is kept as the record of what was sent")
			assert.Contains(t, c.Message, forceDeleteAnnotation)
			assert.Contains(t, c.Message, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
			writes := f.counter.writes.Load()

			restarted := f.newReconciler()
			f.prov.answerDescribe(contracts.DescribeResponse{}, stderrors.New("describe: timeout"))
			var waits []time.Duration
			for _, step := range []time.Duration{20 * time.Second, time.Minute, 10 * time.Minute} {
				f.now = f.now.Add(step)
				waits = append(waits, f.reconcile(restarted).RequeueAfter)
			}
			assert.InDelta(t, float64(20*time.Second), float64(waits[0]), float64(time.Second))
			assert.InDelta(t, float64(80*time.Second), float64(waits[1]), float64(time.Second))
			assert.Equal(t, blockedRetryMax, waits[2], "bounded")
			f.requireHeld()
			assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "no Delete is ever sent again")
			assert.EqualValues(t, 1, f.prov.statusCalls.Load(), "TaskStatus is not asked again")
			assert.Equal(t, 4, f.prov.describeCalls())
			assert.Equal(t, writes, f.counter.writes.Load(), "an unchanged hold writes no status")
			assert.Equal(t, 1, countPrefixed(f.events(), "Warning "+eventReasonDeleteTaskOutcomeUnknown))

			// The VM disappears: released.
			f.prov.answerDescribe(contracts.DescribeResponse{Exists: false}, nil)
			f.reconcile(restarted)
			assert.Nil(t, f.get())
			assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
		})
	}
}

// An explicit terminal error is a failure, not an unknown task.
func TestDeleteTaskAnswerUnknown(t *testing.T) {
	for name, answer := range staleAnswers {
		assert.True(t, deleteTaskAnswerUnknown(answer.st, answer.err), name)
	}
	assert.False(t, deleteTaskAnswerUnknown(contracts.TaskStatus{IsCompleted: true, Error: "Destroy_Task failed: file locked"}, nil))
	assert.False(t, deleteTaskAnswerUnknown(contracts.TaskStatus{}, contracts.NewRetryableError("taskStatus: reset", nil)))
	assert.False(t, deleteTaskAnswerUnknown(contracts.TaskStatus{IsCompleted: false}, nil))
	// The same vSphere fault about the VM (not the task) is a failure.
	assert.False(t, deleteTaskAnswerUnknown(contracts.TaskStatus{IsCompleted: true, Error: "task failed: ServerFaultCode: " +
		"The object 'vim.VirtualMachine:vm-17' has already been deleted or has not been completely created"}, nil))
	assert.False(t, deleteTaskAnswerUnknown(contracts.TaskStatus{IsCompleted: true, Error: "task failed: The object has " +
		"already been deleted or has not been completely created"}, nil), "no object type: not safely a purged task")
}

// 10. The task record is a status patch with no resourceVersion
// precondition: an object edited since it was read still records it.
func TestAsyncDelete_RecordSurvivesResourceVersionConflict(t *testing.T) {
	f := newAsyncDeleteFixture(t, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	})
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	stale := f.get()
	// Another writer edits the VM after it was read.
	fresh := f.get()
	fresh.Labels = map[string]string{"example.com/edited": "during-delete"}
	require.NoError(t, f.client.Update(context.Background(), fresh))
	require.NotEqual(t, stale.ResourceVersion, f.get().ResourceVersion)

	_, err := f.r.handleDeletion(context.Background(), stale)
	require.NoError(t, err)
	vm := f.requireHeld()
	assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef, "the record lands despite the stale resourceVersion")
	assert.Equal(t, "during-delete", vm.Labels["example.com/edited"], "the other writer's edit is kept")
}

// 10 (lost write). Delete succeeded but its record could not be written: the
// finalizer is kept, no second Delete is sent, and the record is written
// again before the task is polled.
func TestAsyncDelete_RecordWriteFailsNoSecondDelete(t *testing.T) {
	f := newAsyncDeleteFixture(t, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	})
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	f.counter.failN.Store(1)
	res := f.reconcile(f.r)
	assert.Equal(t, vmDeleteTaskRecordRetryInterval, res.RequeueAfter)
	vm := f.requireHeld()
	assert.Empty(t, vm.Status.LastTaskRef, "the record did not land")
	assert.Equal(t, 1, countPrefixed(f.events(), "Warning "+eventReasonDeleteTaskNotRecorded))

	// The record fails once more: still no Delete, no TaskStatus.
	f.counter.failN.Store(1)
	assert.Equal(t, vmDeleteTaskRecordRetryInterval, f.reconcile(f.r).RequeueAfter)
	assert.Zero(t, f.prov.statusCalls.Load(), "a task is not polled before its record is stored")

	f.reconcile(f.r)
	vm = f.requireHeld()
	assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef, "the record is written again")
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "no second Delete after a lost record")
	assert.EqualValues(t, 1, f.prov.statusCalls.Load())
}

// 11. orphan-on-delete set while a task is recorded releases the VM without
// any provider call.
func TestAsyncDelete_OrphanOnDeleteReleasesWithoutProviderCall(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	f.reconcile(f.r)
	vm := f.get()
	vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
	require.NoError(t, f.client.Update(context.Background(), vm))
	f.reconcile(f.r)
	assert.Nil(t, f.get())
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	assert.Zero(t, f.prov.statusCalls.Load(), "orphan-on-delete calls no provider")
}

// 11. force-delete set while the task is unresolved releases the VM with the
// VMLeftOnHypervisor audit event; a completed task is still a plain release.
func TestAsyncDelete_ForceDelete(t *testing.T) {
	for name, tc := range map[string]struct {
		st       contracts.TaskStatus
		err      error
		leftOver bool
	}{
		"running":   {st: contracts.TaskStatus{IsCompleted: false}, leftOver: true},
		"unknown":   {err: stderrors.New("taskStatus failed: unavailable"), leftOver: true},
		"failed":    {st: contracts.TaskStatus{IsCompleted: true, Error: "failed"}, leftOver: true},
		"ambiguous": {st: contracts.TaskStatus{IsCompleted: true, Error: "task not found"}, leftOver: true},
		"completed": {st: contracts.TaskStatus{IsCompleted: true}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAsyncDeleteFixture(t, nil)
			f.reconcile(f.r)
			vm := f.get()
			vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			require.NoError(t, f.client.Update(context.Background(), vm))
			f.prov.answer(tc.st, tc.err)
			f.reconcile(f.r)
			assert.Nil(t, f.get())
			assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
			assert.EqualValues(t, 1, f.prov.statusCalls.Load(), "the task is still asked once")
			left := countPrefixed(f.events(), "Warning "+eventReasonVMLeftOnHypervisor)
			if tc.leftOver {
				assert.Equal(t, 1, left)
			} else {
				assert.Zero(t, left)
			}
		})
	}
}

// 11. force-delete set before the deletion: a Delete answered with a task is
// still recorded first (as a successful synchronous Delete would release at
// once), and released on the next reconcile while unresolved.
func TestAsyncDelete_ForceDeleteFromTheStart(t *testing.T) {
	f := newAsyncDeleteFixture(t, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	})
	f.prov.answer(contracts.TaskStatus{IsCompleted: true}, nil)
	f.reconcile(f.r)
	f.requireHeld()
	f.reconcile(f.r)
	assert.Nil(t, f.get())
	assert.Zero(t, countPrefixed(f.events(), "Warning "+eventReasonVMLeftOnHypervisor), "a completed task is a confirmed delete")
}

// The messages are constant: a repeated answer never rewrites the status.
func TestAsyncDelete_MessagesNameRecovery(t *testing.T) {
	for _, msg := range []string{deleteTaskInProgressMessage, deleteTaskStatusUnknownMessage, deleteTaskFailedMessage} {
		assert.True(t, strings.Contains(msg, forceDeleteAnnotation) &&
			strings.Contains(msg, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation), msg)
	}
}

// A clustered (routed) VM: the host-encoded task reference is recorded and
// polled verbatim, a DeleteBlocked hold recorded while the provider was
// unreachable is cleared once the task answers, and the finalizer is kept
// until the task completes.
func TestAsyncDelete_ClusteredHostEncodedTask(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("db", clusteredNS, "prov-cluster")
	vm.UID = "uid-db"
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Status.ID = "db"
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}
	const ref = "host-task/v1/host-alpha/inner-7"
	prov := &asyncDeleteProvider{deleteTask: ref}
	prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	r := clusteredFixture(t, prov, vm)
	resolver, ok := r.RemoteResolver.(*stubResolver)
	require.True(t, ok)

	_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "db"))
	require.NoError(t, err)
	got := getVM(t, r, "db")
	assert.Equal(t, ref, got.Status.LastTaskRef)
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, k8s.ConditionDeleteTask))

	// The provider is unreachable: the clustered hold is recorded, the task kept.
	resolver.err = stderrors.New("provider runtime not running")
	_, err = r.handleDeletion(ctx, getVM(t, r, "db"))
	require.NoError(t, err)
	got = getVM(t, r, "db")
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, k8s.ConditionDeleteBlocked))
	assert.Equal(t, ref, got.Status.LastTaskRef)

	// It is back and the task still runs: the stale hold is cleared.
	resolver.err = nil
	_, err = r.handleDeletion(ctx, getVM(t, r, "db"))
	require.NoError(t, err)
	got = getVM(t, r, "db")
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionDeleteBlocked))
	assert.Equal(t, k8s.ReasonDeleting, meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady).Reason)

	prov.answer(contracts.TaskStatus{IsCompleted: true}, nil)
	_, err = r.handleDeletion(ctx, getVM(t, r, "db"))
	require.NoError(t, err)
	gone := &infravirtrigaudiov1beta1.VirtualMachine{}
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "db"}, gone)))
	assert.EqualValues(t, 1, prov.deleteCalls.Load())
	assert.Equal(t, []string{ref, ref}, prov.taskRefs)
}

// S4. A stale cached object (no DeleteTask condition yet) read after the
// record was written: the in-memory record wins, no second Delete is sent and
// the task is polled.
func TestAsyncDelete_StaleCacheWhileMemoryHoldsRef(t *testing.T) {
	f := newAsyncDeleteFixture(t, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting
	})
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	stale := f.get() // what a lagging cache still returns afterwards
	f.reconcile(f.r)
	require.Equal(t, "task-delete-1", f.requireHeld().Status.LastTaskRef, "the record was written")
	require.Nil(t, deleteTaskCondition(stale))

	_, err := f.r.handleDeletion(context.Background(), stale)
	require.NoError(t, err)
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load(), "a stale cache never sends a second Delete")
	assert.EqualValues(t, 1, f.prov.statusCalls.Load(), "the remembered task is polled")
	vm := f.requireHeld()
	assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef)
	assert.True(t, meta.IsStatusConditionTrue(vm.Status.Conditions, k8s.ConditionDeleteTask))
}

// S2. The Provider object is deleted while a Delete task is recorded: the
// finalizer is kept (DeleteTaskProviderNotFound, one event) instead of the
// release a VM with no task gets; force-delete releases it with the audit
// event; once the Provider is back, the task is polled again.
func TestAsyncDelete_ProviderGoneWithRecordedTask(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in memory", true: "from status"}[restart], func(t *testing.T) {
			f := newAsyncDeleteFixture(t, nil)
			f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
			f.reconcile(f.r)
			require.NoError(t, f.client.Delete(context.Background(), deletionProviderCR()))
			r := f.r
			if restart {
				r = f.newReconciler()
			}
			for range 2 {
				res := f.reconcile(r)
				assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin)
			}
			vm := f.requireHeld()
			c := deleteTaskCondition(vm)
			require.NotNil(t, c)
			assert.Equal(t, k8s.ReasonDeleteTaskProviderNotFound, c.Reason)
			assert.Equal(t, metav1.ConditionTrue, c.Status, "still a task to poll once the Provider is back")
			assert.Equal(t, "task-delete-1", vm.Status.LastTaskRef)
			assert.Equal(t, 1, countPrefixed(f.events(), "Warning "+eventReasonDeleteTaskProviderNotFound))

			require.NoError(t, f.client.Create(context.Background(), deletionProviderCR()))
			f.reconcile(r)
			assert.Equal(t, k8s.ReasonTaskInProgress, deleteTaskCondition(f.requireHeld()).Reason)
			f.prov.answer(contracts.TaskStatus{IsCompleted: true}, nil)
			f.reconcile(r)
			assert.Nil(t, f.get())
			assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
		})
	}
}

// S2. With force-delete, a Provider gone while a task is recorded releases the
// VM with the VMLeftOnHypervisor event.
func TestAsyncDelete_ProviderGoneWithRecordedTaskForceDelete(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.prov.answer(contracts.TaskStatus{IsCompleted: false}, nil)
	f.reconcile(f.r)
	require.NoError(t, f.client.Delete(context.Background(), deletionProviderCR()))
	vm := f.get()
	vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, f.client.Update(context.Background(), vm))
	f.reconcile(f.r)
	assert.Nil(t, f.get())
	assert.Equal(t, 1, countPrefixed(f.events(), "Warning "+eventReasonVMLeftOnHypervisor))
}

// S2 (unchanged). A VM with no Delete task whose Provider is gone is released
// as before, without any provider call.
func TestAsyncDelete_ProviderGoneWithoutTaskReleases(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	require.NoError(t, f.client.Delete(context.Background(), deletionProviderCR()))
	f.reconcile(f.r)
	assert.Nil(t, f.get())
	assert.Zero(t, f.prov.deleteCalls.Load())
	assert.Empty(t, f.events())
}

// A VM whose last task explicitly failed has no task in flight: with its
// Provider object gone it is released, as a VM with no task always was.
func TestAsyncDelete_ProviderGoneAfterTaskFailedReleases(t *testing.T) {
	f := newAsyncDeleteFixture(t, nil)
	f.reconcile(f.r)
	f.prov.answer(contracts.TaskStatus{IsCompleted: true, Error: "failed"}, nil)
	f.reconcile(f.r)
	require.NoError(t, f.client.Delete(context.Background(), deletionProviderCR()))
	f.reconcile(f.newReconciler())
	assert.Nil(t, f.get())
	assert.EqualValues(t, 1, f.prov.deleteCalls.Load())
	assert.Zero(t, countPrefixed(f.events(), "Warning "+eventReasonDeleteTaskProviderNotFound))
}
