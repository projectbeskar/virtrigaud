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
	"bytes"
	"context"
	stderrors "errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the delete of a VirtualMachine whose Provider exists but
// cannot be reached (Resolver.GetProvider fails: runtime not Running, no
// endpoint, TLS / gRPC client / Validate failing — every provider rollout).
// Nothing was sent to the provider, so the hypervisor VM may still exist: the
// finalizer is kept (it used to be released, orphaning the VM), with a
// constant message, one Warning event per transition, the blocked-VM backoff
// and no status write on a repeated hold; force-delete and orphan-on-delete
// still release it.

// switchableResolver is a ProviderResolver whose failure a test can switch on
// and off, counting every resolution.
type switchableResolver struct {
	provider contracts.Provider
	err      error
	calls    atomic.Int32
}

func (s *switchableResolver) GetProvider(_ context.Context, _ *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return s.provider, nil
}

// Two resolver failures with different text: the held status must not follow
// the text, or every retry would rewrite it.
var (
	errRuntimeNotReady = stderrors.New("remote provider runtime is not ready: phase=Pending")
	errValidateFailed  = stderrors.New("remote provider validation failed: rpc error: code = Unavailable desc = " +
		"connection error: dial tcp 10.96.12.7:9443: connect: connection refused")
)

// warningDeleteBlockedEvents counts the Warning DeleteBlocked events buffered
// in rec.
func warningDeleteBlockedEvents(rec *record.FakeRecorder) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+k8s.ReasonDeleteBlocked) {
			n++
		}
	}
	return n
}

// TestHandleDeletion_ProviderUnavailable_KeepsFinalizer is the core fix: a
// single-host VM deleted while its Provider cannot be resolved keeps its
// finalizer, gets Ready=False/ProviderUnavailable with a constant message, is
// retried on the blocked-VM backoff, does not rewrite its status on a repeated
// hold, and is deleted through the provider once it answers again.
func TestHandleDeletion_ProviderUnavailable_KeepsFinalizer(t *testing.T) {
	ctx := context.Background()
	prov := &deleteStubProvider{}
	resolver := &switchableResolver{provider: prov, err: errRuntimeNotReady}
	vm := deletionVM("vm-unreachable")
	r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec
	marked := markForDeletion(t, r, vm)

	res, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the first retry of the blocked-VM backoff")
	assert.EqualValues(t, 1, resolver.calls.Load())
	assert.Zero(t, prov.calls.Load(), "no provider, no Delete")

	var held infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &held),
		"the VM must still exist: releasing the finalizer would orphan the hypervisor VM")
	assert.Contains(t, held.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready, "the owner is told why the delete does not complete")
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonProviderUnavailable, ready.Reason)
	assert.Equal(t, held.Generation, ready.ObservedGeneration)
	assert.Equal(t, deleteBlockedMessages[k8s.ReasonProviderUnavailable], ready.Message, "a constant message")
	assert.Contains(t, ready.Message, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, "the way out is named")
	assert.Contains(t, ready.Message, forceDeleteAnnotation)
	assert.NotContains(t, ready.Message, "phase=Pending", "the resolver's error stays out of the condition")
	assert.Nil(t, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked),
		"a single-host VM never carries the clustered DeleteBlocked condition")
	assert.Equal(t, 1, warningDeleteBlockedEvents(rec), "one Warning event when the hold begins")

	// A repeated hold — with a different resolver error — writes nothing and
	// records no event: the deleting VM is reconciled on every update, so a
	// write would re-trigger it at once instead of waiting for the backoff.
	resolver.err = errValidateFailed
	res, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	assert.Zero(t, prov.calls.Load())
	var again infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &again))
	assert.Equal(t, held.ResourceVersion, again.ResourceVersion, "a repeated hold does not write the VM")
	assert.True(t, equality.Semantic.DeepEqual(held.Status, again.Status), "a repeated hold leaves the status unchanged")
	assert.Zero(t, warningDeleteBlockedEvents(rec), "no event per retry")

	// The Provider answers again: the hypervisor VM is deleted and the
	// finalizer released.
	resolver.err = nil
	_, err = r.handleDeletion(ctx, again.DeepCopy())
	require.NoError(t, err)
	assert.EqualValues(t, 1, prov.calls.Load(), "the delete is sent once the Provider can be reached")
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})),
		"the finalizer is released once the provider deleted the VM")
}

// TestHandleDeletion_ProviderUnavailable_BackoffFromDeletion: a single-host
// hold backs off from when the VM's deletion was requested, bounded at
// blockedRetryMax.
func TestHandleDeletion_ProviderUnavailable_BackoffFromDeletion(t *testing.T) {
	ctx := context.Background()
	resolver := &switchableResolver{err: errRuntimeNotReady}
	vm := deletionVM("vm-old-delete")
	r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	for _, tc := range []struct {
		ago      time.Duration
		min, max time.Duration
	}{
		{ago: 3 * time.Minute, min: 3 * time.Minute, max: blockedRetryMax},
		{ago: time.Hour, min: blockedRetryMax, max: blockedRetryMax},
	} {
		deleting := marked.DeepCopy()
		since := metav1.NewTime(time.Now().Add(-tc.ago))
		deleting.DeletionTimestamp = &since
		res, err := r.handleDeletion(ctx, deleting)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, res.RequeueAfter, tc.min, "deleted %s ago", tc.ago)
		assert.LessOrEqual(t, res.RequeueAfter, tc.max, "deleted %s ago", tc.ago)
	}
}

// TestHandleDeletion_Clustered_ProviderUnavailableIsHeld: a clustered VM is
// held like every other clustered delete — DeleteBlocked=True/
// ProviderUnavailable and Ready=False/DeleteBlocked with a constant message
// naming no host or endpoint — and the routed, owner-checked Delete is sent
// once the Provider answers.
func TestHandleDeletion_Clustered_ProviderUnavailableIsHeld(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, boundClusterVMForDelete())
	resolver := &switchableResolver{provider: prov, err: errValidateFailed}
	r.RemoteResolver = resolver
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	res, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	assert.Empty(t, prov.deleteRefs, "no Delete without a provider client")

	held := getVM(t, r, "web")
	assert.Contains(t, held.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	blocked := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, metav1.ConditionTrue, blocked.Status)
	assert.Equal(t, k8s.ReasonProviderUnavailable, blocked.Reason)
	assert.Equal(t, held.Generation, blocked.ObservedGeneration)
	assert.Equal(t, deleteBlockedMessages[k8s.ReasonProviderUnavailable], blocked.Message)
	assert.NotContains(t, blocked.Message, "host-alpha", "no host is named")
	assert.NotContains(t, blocked.Message, "10.96.12.7", "no endpoint is named")
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonDeleteBlocked, ready.Reason)
	assert.Equal(t, 1, warningDeleteBlockedEvents(rec))

	// Repeated hold, different error text: nothing is written, no event.
	resolver.err = errRuntimeNotReady
	res, err = r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	again := getVM(t, r, "web")
	assert.Equal(t, held.ResourceVersion, again.ResourceVersion, "a repeated hold does not write the VM")
	assert.True(t, equality.Semantic.DeepEqual(held.Status, again.Status))
	assert.Zero(t, warningDeleteBlockedEvents(rec))

	// The backoff grows with the time already held.
	holdStartedAgo(t, r, 3*time.Minute)
	res, err = r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.RequeueAfter, 3*time.Minute)
	assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMax)

	// The Provider answers again: the routed, owner-checked Delete runs.
	resolver.err = nil
	_, err = r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	require.Len(t, prov.deleteRefs, 1)
	assert.Equal(t, "host-alpha", prov.deleteRefs[0].HostID)
	assert.Equal(t, "uid-web", prov.deleteOwners[0].UID, "owner-checked")
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(held), &infravirtrigaudiov1beta1.VirtualMachine{})))
}

// TestHandleDeletion_ProviderUnavailable_ForceDelete: force-delete releases
// the finalizer without any provider call, whether it was set before the
// delete or while the delete is held.
func TestHandleDeletion_ProviderUnavailable_ForceDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("set before the delete", func(t *testing.T) {
		prov := &deleteStubProvider{}
		resolver := &switchableResolver{provider: prov, err: errRuntimeNotReady}
		vm := deletionVM("vm-force-now")
		vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())

		_, err := r.handleDeletion(ctx, markForDeletion(t, r, vm))
		require.NoError(t, err)
		assert.Zero(t, prov.calls.Load())
		assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})),
			"force-delete releases the finalizer although the Provider cannot be reached")
	})

	t.Run("set while held", func(t *testing.T) {
		prov := &deleteStubProvider{}
		resolver := &switchableResolver{provider: prov, err: errRuntimeNotReady}
		vm := deletionVM("vm-force-later")
		r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())
		_, err := r.handleDeletion(ctx, markForDeletion(t, r, vm))
		require.NoError(t, err)

		var held infravirtrigaudiov1beta1.VirtualMachine
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &held))
		held.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		require.NoError(t, r.Update(ctx, &held))
		_, err = r.handleDeletion(ctx, &held)
		require.NoError(t, err)
		assert.Zero(t, prov.calls.Load())
		assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
	})

	t.Run("clustered", func(t *testing.T) {
		prov := &routingProvider{}
		vm := boundClusterVMForDelete()
		vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		r := clusteredFixture(t, prov, vm)
		r.RemoteResolver = &switchableResolver{provider: prov, err: errValidateFailed}

		_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
		require.NoError(t, err)
		assert.Empty(t, prov.deleteRefs)
		assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
	})
}

// TestHandleDeletion_ProviderUnavailable_OrphanOnDelete: orphan-on-delete
// still detaches the VM before any Provider is resolved, so an unreachable
// Provider does not hold it.
func TestHandleDeletion_ProviderUnavailable_OrphanOnDelete(t *testing.T) {
	ctx := context.Background()
	prov := &deleteStubProvider{}
	resolver := &switchableResolver{provider: prov, err: errRuntimeNotReady}
	vm := deletionVM("vm-detach")
	vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
	r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	_, err := r.handleDeletion(ctx, markForDeletion(t, r, vm))
	require.NoError(t, err)
	assert.Zero(t, resolver.calls.Load(), "no Provider is resolved")
	assert.Zero(t, prov.calls.Load())
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.True(t, strings.HasPrefix(events[0], "Normal "+eventReasonOrphaned), "got %q", events[0])
}

// TestHandleDeletion_ProviderUnavailable_ThenDeleteFails: once the Provider
// answers but its Delete fails for an ordinary reason, Ready no longer claims
// the Provider is unreachable: it becomes Ready=False/ProviderError with a
// constant message, the delete is retried on the ordinary cadence, and a
// repeated failure writes nothing.
func TestHandleDeletion_ProviderUnavailable_ThenDeleteFails(t *testing.T) {
	ctx := context.Background()
	prov := &deleteStubProvider{err: stderrors.New("VM 100 is running - destroy failed")}
	resolver := &switchableResolver{provider: prov, err: errRuntimeNotReady}
	vm := deletionVM("vm-then-fails")
	r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())
	_, err := r.handleDeletion(ctx, markForDeletion(t, r, vm))
	require.NoError(t, err)

	resolver.err = nil
	var held infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &held))
	res, err := r.handleDeletion(ctx, &held)
	require.NoError(t, err)
	assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
	assert.EqualValues(t, 1, prov.calls.Load())

	var failed infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &failed))
	assert.Contains(t, failed.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	ready := meta.FindStatusCondition(failed.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonProviderError, ready.Reason)
	assert.Equal(t, providerDeleteFailedMessage, ready.Message)
	assert.NotContains(t, ready.Message, "destroy failed", "the provider's answer goes to the log")

	_, err = r.handleDeletion(ctx, failed.DeepCopy())
	require.NoError(t, err)
	var again infravirtrigaudiov1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), &again))
	assert.Equal(t, failed.ResourceVersion, again.ResourceVersion, "a repeated ordinary failure writes nothing")
}

// pendingClusterVMForDelete is a clustered VM whose create is in flight
// (status.id empty, pendingHost host-alpha), with the finalizer.
func pendingClusterVMForDelete() *infravirtrigaudiov1beta1.VirtualMachine {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a"}
	return vm
}

// TestHandleDeletion_Clustered_PendingCreate_ProviderUnavailableIsHeld: a
// clustered VM deleted while its create is in flight (no status.id, a
// pendingHost) may already have a domain on that host, so a resolve failure
// holds it like a bound VM — it is not released — and the owner-checked
// Delete goes to the pending host once the Provider answers.
func TestHandleDeletion_Clustered_PendingCreate_ProviderUnavailableIsHeld(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, pendingClusterVMForDelete())
	resolver := &switchableResolver{provider: prov, err: errRuntimeNotReady}
	r.RemoteResolver = resolver
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	res, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	assert.EqualValues(t, 1, resolver.calls.Load(), "the pending create's Provider was resolved")
	assert.Empty(t, prov.deleteRefs)

	held := getVM(t, r, "web")
	assert.Contains(t, held.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer,
		"a domain the create made on the pending host must not be orphaned")
	blocked := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, k8s.ReasonProviderUnavailable, blocked.Reason)
	assert.Equal(t, k8s.ReasonDeleteBlocked, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
	assert.Equal(t, 1, warningDeleteBlockedEvents(rec))

	resolver.err = nil
	_, err = r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	require.Len(t, prov.deleteRefs, 1)
	assert.Equal(t, contracts.VMRef{ID: "web", HostID: "host-alpha", Owner: contracts.ObjectIdentity{UID: "uid-web", Namespace: clusteredNS, Name: "web"}},
		prov.deleteRefs[0], "an owner-checked delete on the pending host")
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(held), &infravirtrigaudiov1beta1.VirtualMachine{})))
}

// TestHandleDeletion_Clustered_OwnDomainHoldThenProviderUnavailable: a held
// delete whose reason changes from OwnDomainOnAnotherHost to
// ProviderUnavailable updates DeleteBlocked (reason and constant message) and
// Ready, records one more Warning event for the transition, and keeps the
// backoff counting from when the hold began.
func TestHandleDeletion_Clustered_OwnDomainHoldThenProviderUnavailable(t *testing.T) {
	ctx := context.Background()
	vm := pendingClusterVMForDelete()
	vm.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionPlaced, Status: metav1.ConditionFalse,
		Reason: k8s.ReasonOwnDomainOnAnotherHost, Message: restorePendingOwnMessage, LastTransitionTime: metav1.Now()}}
	prov := &routingProvider{deleteErr: contracts.NewNotFoundError("delete: no domain of this VirtualMachine on the host", nil)}
	r := clusteredFixture(t, prov, vm)
	resolver := &switchableResolver{provider: prov}
	r.RemoteResolver = resolver
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	require.Len(t, prov.deleteRefs, 1)
	require.Equal(t, k8s.ReasonOwnDomainOnAnotherHost,
		meta.FindStatusCondition(getVM(t, r, "web").Status.Conditions, k8s.ConditionDeleteBlocked).Reason)
	require.Equal(t, 1, warningDeleteBlockedEvents(rec))
	holdStartedAgo(t, r, 2*time.Minute)

	resolver.err = errValidateFailed
	res, err := r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	assert.Len(t, prov.deleteRefs, 1, "no Delete without a provider client")
	assert.GreaterOrEqual(t, res.RequeueAfter, 2*time.Minute, "the hold did not restart")

	held := getVM(t, r, "web")
	assert.Contains(t, held.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	blocked := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, k8s.ReasonProviderUnavailable, blocked.Reason)
	assert.Equal(t, deleteBlockedMessages[k8s.ReasonProviderUnavailable], blocked.Message)
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonDeleteBlocked, ready.Reason)
	assert.Equal(t, deleteBlockedMessages[k8s.ReasonProviderUnavailable], ready.Message)
	assert.Equal(t, 1, warningDeleteBlockedEvents(rec), "one event for the reason change")

	// Repeated: unchanged, no event.
	_, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, held.ResourceVersion, getVM(t, r, "web").ResourceVersion)
	assert.Zero(t, warningDeleteBlockedEvents(rec))
}

// leftOnHypervisorEvents returns the VMLeftOnHypervisor Warning events
// buffered in rec (draining it).
func leftOnHypervisorEvents(rec *record.FakeRecorder) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+eventReasonVMLeftOnHypervisor) {
			out = append(out, e)
		}
	}
	return out
}

// TestHandleDeletion_ForceDeleteRecordsVMLeftOnHypervisor: every force-delete
// release that removes the finalizer without a successful provider Delete
// records exactly one VMLeftOnHypervisor Warning event naming the VM, its id
// and the constant cause — never the resolver's error, an endpoint, the host or
// the provider's answer. A completed or already-done delete records none.
func TestHandleDeletion_ForceDeleteRecordsVMLeftOnHypervisor(t *testing.T) {
	ctx := context.Background()
	forced := map[string]string{forceDeleteAnnotation: "true"}
	leaks := []string{"phase=Pending", "10.96.12.7", "host-alpha", "destroy failed", "unreachable host", "rpc error"}

	singleHost := func(t *testing.T, resolver *switchableResolver, annotations map[string]string) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine) {
		t.Helper()
		vm := deletionVM("vm-left")
		vm.Annotations = annotations
		r := newTestReconciler(coverageTestScheme(t), resolver, vm, deletionProviderCR())
		rec := record.NewFakeRecorder(16)
		r.Recorder = rec
		return r, rec, markForDeletion(t, r, vm)
	}
	clustered := func(t *testing.T, prov *routingProvider, resolverErr error, vm *infravirtrigaudiov1beta1.VirtualMachine) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine) {
		t.Helper()
		vm.Annotations = forced
		r := clusteredFixture(t, prov, vm)
		r.RemoteResolver = &switchableResolver{provider: prov, err: resolverErr}
		rec := record.NewFakeRecorder(16)
		r.Recorder = rec
		return r, rec, deletingClusterVM(t, r, vm.Name)
	}

	for name, tc := range map[string]struct {
		run   func(t *testing.T) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine)
		cause string
	}{
		"single-host, Provider unreachable": {func(t *testing.T) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine) {
			return singleHost(t, &switchableResolver{err: errRuntimeNotReady}, forced)
		}, vmLeftProviderUnavailable},
		"single-host, Delete failed": {func(t *testing.T) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine) {
			return singleHost(t, &switchableResolver{provider: &deleteStubProvider{err: stderrors.New("VM 100 is running - destroy failed")}}, forced)
		}, vmLeftDeleteFailed},
		"clustered, Provider unreachable": {func(t *testing.T) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine) {
			return clustered(t, &routingProvider{}, errValidateFailed, boundClusterVMForDelete())
		}, vmLeftProviderUnavailable},
		"clustered, host unreachable": {func(t *testing.T) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine) {
			prov := &routingProvider{deleteErr: contracts.NewHostUnavailableError(`delete VM: host "host-alpha" is an unreachable host`, nil)}
			return clustered(t, prov, nil, boundClusterVMForDelete())
		}, vmLeftDeleteFailed},
		"clustered, unbound (not routable)": {func(t *testing.T) (*VirtualMachineReconciler, *record.FakeRecorder, *infravirtrigaudiov1beta1.VirtualMachine) {
			vm := boundClusterVMForDelete()
			vm.Status.Placement = nil
			return clustered(t, &routingProvider{}, nil, vm)
		}, vmLeftNotRoutable},
	} {
		t.Run(name, func(t *testing.T) {
			r, rec, deleting := tc.run(t)
			_, err := r.handleDeletion(ctx, deleting)
			require.NoError(t, err)
			assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(deleting), &infravirtrigaudiov1beta1.VirtualMachine{})),
				"force-delete releases the finalizer")
			events := leftOnHypervisorEvents(rec)
			require.Len(t, events, 1, "one audit event per force-delete release")
			assert.Contains(t, events[0], deleting.Namespace+"/"+deleting.Name)
			assert.Contains(t, events[0], `"`+deleting.Status.ID+`"`)
			assert.Contains(t, events[0], tc.cause)
			assert.Contains(t, events[0], "may remain")
			for _, leak := range leaks {
				assert.NotContains(t, events[0], leak)
			}
		})
	}

	for name, deleteErr := range map[string]error{
		"deleted":       nil,
		"already gone":  contracts.NewNotFoundError("delete: VM not found", nil),
		"not forced ok": nil,
	} {
		t.Run("no event when the provider confirmed: "+name, func(t *testing.T) {
			annotations := forced
			if name == "not forced ok" {
				annotations = nil
			}
			r, rec, deleting := singleHost(t, &switchableResolver{provider: &deleteStubProvider{err: deleteErr}}, annotations)
			_, err := r.handleDeletion(ctx, deleting)
			require.NoError(t, err)
			assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(deleting), &infravirtrigaudiov1beta1.VirtualMachine{})))
			assert.Empty(t, leftOnHypervisorEvents(rec))
		})
	}
}

// logCapture returns a context whose controller-runtime logger writes every
// line into the returned buffer.
func logCapture() (context.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	sink := funcr.New(func(prefix, args string) {
		buf.WriteString(prefix)
		buf.WriteString(args)
		buf.WriteByte('\n')
	}, funcr.Options{Verbosity: 10})
	return ctrllog.IntoContext(context.Background(), logr.New(sink.GetSink())), &buf
}

// TestHandleDeletion_ProviderUnavailable_OneSearchableLogLine: single-host and
// clustered holds both log providerUnavailableDeleteLogMessage — the line the
// docs tell operators to search for — with the resolver's error (and, for a
// clustered VM, its host), which appear in no condition or event.
func TestHandleDeletion_ProviderUnavailable_OneSearchableLogLine(t *testing.T) {
	t.Run("single-host", func(t *testing.T) {
		ctx, logs := logCapture()
		vm := deletionVM("vm-logged")
		r := newTestReconciler(coverageTestScheme(t), &switchableResolver{err: errRuntimeNotReady}, vm, deletionProviderCR())
		_, err := r.handleDeletion(ctx, markForDeletion(t, r, vm))
		require.NoError(t, err)
		assert.Contains(t, logs.String(), providerUnavailableDeleteLogMessage)
		assert.Contains(t, logs.String(), "phase=Pending", "the cause is logged")
	})
	t.Run("clustered", func(t *testing.T) {
		ctx, logs := logCapture()
		prov := &routingProvider{}
		r := clusteredFixture(t, prov, boundClusterVMForDelete())
		r.RemoteResolver = &switchableResolver{err: errValidateFailed}
		rec := record.NewFakeRecorder(16)
		r.Recorder = rec
		_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
		require.NoError(t, err)
		assert.Contains(t, logs.String(), providerUnavailableDeleteLogMessage, "the same line as a single-host hold")
		assert.Contains(t, logs.String(), "10.96.12.7", "the cause is logged")
		assert.Contains(t, logs.String(), "host-alpha", "the host is logged")
		for _, e := range drainEvents(rec) {
			assert.NotContains(t, e, "host-alpha")
			assert.NotContains(t, e, "10.96.12.7")
		}
	})
}

// TestHandleDeletion_Clustered_ProviderUnavailableThenDeleteFails: a clustered
// VM held as ProviderUnavailable whose Provider then answers but fails the
// Delete for an ordinary reason loses the stale hold entirely — DeleteBlocked
// is removed and Ready=False/DeleteBlocked becomes Ready=False/ProviderError
// with the constant message — and a repeated failure writes nothing.
func TestHandleDeletion_Clustered_ProviderUnavailableThenDeleteFails(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{deleteErr: stderrors.New("delete: the hypervisor said no")}
	r := clusteredFixture(t, prov, boundClusterVMForDelete())
	resolver := &switchableResolver{provider: prov, err: errValidateFailed}
	r.RemoteResolver = resolver

	_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	held := getVM(t, r, "web")
	require.NotNil(t, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked))
	require.Equal(t, k8s.ReasonDeleteBlocked, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)

	resolver.err = nil
	res, err := r.handleDeletion(ctx, held)
	require.NoError(t, err)
	assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
	require.Len(t, prov.deleteRefs, 1, "the Delete was sent once the Provider answered")

	failed := getVM(t, r, "web")
	assert.Contains(t, failed.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	assert.Nil(t, meta.FindStatusCondition(failed.Status.Conditions, k8s.ConditionDeleteBlocked),
		"DeleteBlocked no longer applies")
	ready := meta.FindStatusCondition(failed.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonProviderError, ready.Reason, "Ready no longer carries the old hold")
	assert.Equal(t, providerDeleteFailedMessage, ready.Message)
	assert.Equal(t, failed.Generation, ready.ObservedGeneration)

	_, err = r.handleDeletion(ctx, failed.DeepCopy())
	require.NoError(t, err)
	assert.Len(t, prov.deleteRefs, 2)
	again := getVM(t, r, "web")
	assert.Equal(t, failed.ResourceVersion, again.ResourceVersion, "a repeated ordinary failure writes nothing")
}
