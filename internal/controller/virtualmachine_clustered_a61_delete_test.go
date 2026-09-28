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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the manager side of the A6.1 security review, item 3: a
// clustered delete the provider fails closed on (HOST_UNAVAILABLE,
// VM_DISK_CHECK_FAILED) keeps the finalizer with DeleteBlocked=True, a
// constant message naming no host, one Warning event per transition, and an
// exponential backoff (15 s doubling to 5 min) — while force-delete is acted on
// at once. Single-host deletes are unchanged.

// boundClusterVMForDelete is a clustered VM bound to host-alpha, with the
// finalizer.
func boundClusterVMForDelete() *infrav1beta1.VirtualMachine {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
	vm.Status.ID = "default.web"
	vm.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	return vm
}

// holdStartedAgo moves the VM's DeleteBlocked condition's start back by ago.
func holdStartedAgo(t *testing.T, r *VirtualMachineReconciler, ago time.Duration) {
	t.Helper()
	vm := getVM(t, r, "web")
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, c)
	c.LastTransitionTime = metav1.NewTime(time.Now().Add(-ago))
	require.NoError(t, r.Status().Update(context.Background(), vm))
}

func TestHandleDeletion_Clustered_UncheckedDeleteIsHeldWithBackoff(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"host unreachable": {contracts.NewHostUnavailableError(`delete VM: host "host-alpha" is unreachable`, nil), k8s.ReasonHostUnreachable},
		"disk check failed": {contracts.NewRetryableError("delete: could not verify on every host",
			fmt.Errorf("%w: rpc error", contracts.ErrVMDiskCheckFailed)), k8s.ReasonDiskCheckFailed},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			prov := &routingProvider{deleteErr: tc.err}
			r := clusteredFixture(t, prov, boundClusterVMForDelete())
			rec := record.NewFakeRecorder(16)
			r.Recorder = rec

			res, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
			require.NoError(t, err)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the first retry of the backoff")

			held := getVM(t, r, "web")
			assert.Contains(t, held.Finalizers, infrav1beta1.VirtualMachineFinalizer, "nothing was deleted: the finalizer stays")
			blocked := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
			require.NotNil(t, blocked)
			assert.Equal(t, metav1.ConditionTrue, blocked.Status)
			assert.Equal(t, tc.reason, blocked.Reason)
			assert.Equal(t, held.Generation, blocked.ObservedGeneration)
			assert.NotContains(t, blocked.Message, "host-alpha", "no host is named")
			assert.Contains(t, blocked.Message, infrav1beta1.VirtualMachineOrphanOnDeleteAnnotation, "the way out is named")
			ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, k8s.ReasonDeleteBlocked, ready.Reason)

			// The backoff grows with the time already spent held.
			holdStartedAgo(t, r, 3*time.Minute)
			res, err = r.handleDeletion(ctx, getVM(t, r, "web"))
			require.NoError(t, err)
			assert.GreaterOrEqual(t, res.RequeueAfter, 3*time.Minute)
			assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMax)
			holdStartedAgo(t, r, time.Hour)
			res, err = r.handleDeletion(ctx, getVM(t, r, "web"))
			require.NoError(t, err)
			assert.Equal(t, blockedRetryMax, res.RequeueAfter, "bounded at 5 minutes")

			var warnings int
			for _, e := range drainEvents(rec) {
				if strings.HasPrefix(e, "Warning "+k8s.ReasonDeleteBlocked) {
					warnings++
				}
			}
			assert.Equal(t, 1, warnings, "one Warning event per transition, not one per retry")

			// force-delete, set while held, is acted on at once.
			vm := getVM(t, r, "web")
			vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			require.NoError(t, r.Update(ctx, vm))
			_, err = r.handleDeletion(ctx, getVM(t, r, "web"))
			require.NoError(t, err)
			getErr := r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "web"}, &infrav1beta1.VirtualMachine{})
			assert.True(t, apierrors.IsNotFound(getErr), "force-delete releases the finalizer")
		})
	}
}

// TestHandleDeletion_Clustered_HeldDeleteReasonChangeIsATransition: a change
// of reason is a new Warning event, but the backoff keeps counting from when
// the hold began.
func TestHandleDeletion_Clustered_HeldDeleteReasonChangeIsATransition(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{deleteErr: contracts.NewHostUnavailableError("delete VM: unreachable", nil)}
	r := clusteredFixture(t, prov, boundClusterVMForDelete())
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec
	_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	holdStartedAgo(t, r, 2*time.Minute)

	prov.deleteErr = contracts.NewRetryableError("delete: check failed", fmt.Errorf("%w: rpc", contracts.ErrVMDiskCheckFailed))
	res, err := r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.RequeueAfter, 2*time.Minute, "the hold did not restart")
	assert.Equal(t, k8s.ReasonDiskCheckFailed,
		meta.FindStatusCondition(getVM(t, r, "web").Status.Conditions, k8s.ConditionDeleteBlocked).Reason)
	assert.Len(t, drainEvents(rec), 2, "one event per transition")
}

// TestHandleDeletion_SingleHost_DiskCheckFailedUnchanged: a single-host VM's
// delete answered VM_DISK_CHECK_FAILED keeps its historical handling — the
// fixed delete retry, no DeleteBlocked condition.
func TestHandleDeletion_SingleHost_DiskCheckFailedUnchanged(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	prov := &deleteStubProvider{err: contracts.NewRetryableError("delete: check failed", fmt.Errorf("%w: rpc", contracts.ErrVMDiskCheckFailed))}
	vm := deletionVM("vm-single")
	r := newTestReconciler(s, &stubResolver{provider: prov}, vm, deletionProviderCR())
	res, err := r.handleDeletion(ctx, markForDeletion(t, r, vm))
	require.NoError(t, err)
	assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
	var after infrav1beta1.VirtualMachine
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}, &after))
	assert.Nil(t, meta.FindStatusCondition(after.Status.Conditions, k8s.ConditionDeleteBlocked))
}

// TestVMUpdateNeedsReconcile: a status-only update is filtered out, but every
// update of a VM being deleted — an annotation included — is reconciled.
func TestVMUpdateNeedsReconcile(t *testing.T) {
	old := clusterVM("web", clusteredNS, "prov-cluster")
	old.Generation = 3
	statusOnly := old.DeepCopy()
	statusOnly.Status.ID = "x"
	assert.False(t, vmUpdateNeedsReconcile(old, statusOnly))
	specChange := old.DeepCopy()
	specChange.Generation = 4
	assert.True(t, vmUpdateNeedsReconcile(old, specChange))

	deleting := old.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	annotated := deleting.DeepCopy()
	annotated.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	assert.True(t, vmUpdateNeedsReconcile(deleting, annotated), "force-delete on a held delete is acted on at once")
}

// TestBlockedRetryBackoff pins the pacing: the time already held, bounded.
func TestBlockedRetryBackoff(t *testing.T) {
	assert.Equal(t, blockedRetryMin, blockedRetryBackoff(time.Time{}))
	assert.Equal(t, blockedRetryMin, blockedRetryBackoff(time.Now()))
	got := blockedRetryBackoff(time.Now().Add(-time.Minute))
	assert.True(t, got >= time.Minute && got < time.Minute+time.Second, "got %s", got)
	assert.Equal(t, blockedRetryMax, blockedRetryBackoff(time.Now().Add(-24*time.Hour)))
}
