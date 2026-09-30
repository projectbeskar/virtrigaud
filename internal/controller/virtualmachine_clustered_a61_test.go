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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the manager side of ADR-0007 A6.1, R2: a clustered Create
// (or Clone) answered with VM_PREVIOUS_INCARNATION holds the VM on its pending
// host — pendingHost kept, the host NOT excluded, nothing re-scheduled — with
// Placed=False/RestorePending and one Warning event, while a plain name
// conflict keeps the slice 2 exclusion.

// previousIncarnationErr is the error the transport maps a provider's
// AlreadyExists + VM_PREVIOUS_INCARNATION to.
func previousIncarnationErr(domain string) error {
	return contracts.NewConflictError(
		fmt.Sprintf("create: create of libvirt domain %q refused: a domain VirtRigaud created for this VirtualMachine's "+
			"namespace and name (a previous incarnation of it) exists on a host of this Provider", domain),
		fmt.Errorf("%w: rpc error", contracts.ErrVMPreviousIncarnation))
}

// previousIncarnationOn answers Create with VM_PREVIOUS_INCARNATION on the
// listed hosts and succeeds elsewhere.
func previousIncarnationOn(hosts ...string) func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
	return func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		for _, h := range hosts {
			if req.TargetHostID == h {
				return contracts.CreateResponse{}, previousIncarnationErr("default." + req.Name)
			}
		}
		return contracts.CreateResponse{ID: req.Name}, nil
	}
}

// TestCreateVM_Clustered_PreviousIncarnationHoldsOnThePendingHost: the first
// Create (scheduled onto host-bravo) is answered VM_PREVIOUS_INCARNATION. The
// VM keeps host-bravo as its pendingHost — it is NOT excluded and the VM is
// NOT re-scheduled onto host-alpha, where a second domain for the same
// namespace and name would be made — and every retry goes to host-bravo on
// the slow RestorePending cadence, with one Warning event.
func TestCreateVM_Clustered_PreviousIncarnationHoldsOnThePendingHost(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	prov := &routingProvider{onCreate: previousIncarnationOn("host-bravo")}
	bravo := readyHost("host-bravo", clusteredNS, "pool-a", "prov-cluster")
	bravo.Status.AllocatableCPU = i32p(64)
	bravo.Status.AllocatableMemoryMiB = i64p(1 << 20)
	r := clusteredFixture(t, prov, vm, bravo)
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	for i := 0; i < 3; i++ {
		res := createClustered(t, r, prov, "web")
		assert.Equal(t, ctrlResult{after: blockedRetryMin.String()}, res, "attempt %d: the first retries of the backoff", i)
	}
	require.Len(t, prov.createReqs, 3)
	for i, req := range prov.createReqs {
		assert.Equal(t, "host-bravo", req.TargetHostID, "attempt %d stays on the pending host", i)
	}

	held := getVM(t, r, "web")
	require.NotNil(t, held.Status.Placement)
	assert.Equal(t, "host-bravo", held.Status.Placement.PendingHost, "pendingHost is kept")
	assert.Empty(t, held.Status.Placement.ExcludedHosts, "the host is NOT excluded")
	assert.NotNil(t, held.Status.Placement.PendingResources, "the VM keeps counting on its pending host")
	assert.Empty(t, held.Status.ID)
	assert.Empty(t, held.Status.Placement.Host)

	placed := placedCondition(held)
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionFalse, placed.Status)
	assert.Equal(t, k8s.ReasonRestorePending, placed.Reason)
	assert.Equal(t, held.Generation, placed.ObservedGeneration)
	assert.Contains(t, placed.Message, "docs/clustered-restore.md", "the message points at the runbook")
	assert.NotContains(t, placed.Message, "host-bravo", "no host is named")
	assert.NotContains(t, placed.Message, "default.web", "no domain is named")
	assert.Equal(t, k8s.ReasonRestorePending, provisioningReason(held))

	events := drainEvents(rec)
	require.Len(t, events, 1, "one Warning event for the hold, not one per retry: %v", events)
	assert.True(t, strings.HasPrefix(events[0], "Warning "+k8s.ReasonRestorePending), events[0])
}

// TestCreateVM_Clustered_PreviousIncarnationOnARecordedPendingHost: the retry
// path (a pendingHost recorded by an earlier reconcile, the scheduler not
// consulted) holds the same way.
func TestCreateVM_Clustered_PreviousIncarnationOnARecordedPendingHost(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a", ExcludedHosts: []string{"host-old"}}
	prov := &routingProvider{onCreate: previousIncarnationOn("host-alpha")}
	r := clusteredFixture(t, prov, vm)

	createClustered(t, r, prov, "web")
	after := getVM(t, r, "web")
	assert.Equal(t, "host-alpha", after.Status.Placement.PendingHost)
	assert.Equal(t, []string{"host-old"}, after.Status.Placement.ExcludedHosts, "nothing is added to the exclusions")
	assert.Equal(t, k8s.ReasonRestorePending, placedCondition(after).Reason)
}

// TestCreateVM_Clustered_PlainConflictStillExcludes: a foreign or unstamped
// same-named domain (a plain AlreadyExists) keeps the slice 2 behaviour —
// the host is excluded and the VM re-scheduled — so only a previous
// incarnation pins.
func TestCreateVM_Clustered_PlainConflictStillExcludes(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a"}
	prov := &routingProvider{onCreate: conflictOn("host-alpha")}
	r := clusteredFixture(t, prov, vm)

	createClustered(t, r, prov, "web")
	after := getVM(t, r, "web")
	assert.Empty(t, after.Status.Placement.PendingHost)
	assert.Equal(t, []string{"host-alpha"}, after.Status.Placement.ExcludedHosts)
	assert.Equal(t, k8s.ReasonHostExcluded, placedCondition(after).Reason)
}

// TestHandleDeletion_Clustered_HeldVMNeverTouchesThePreviousIncarnation: a VM
// held by R2 keeps its pendingHost, so deleting it sends the owner-checked
// Delete to that host with the VM's own UID — which the provider answers
// NotFound for the previous incarnation (stamped with another UID), never
// deleting it — and the finalizer is released.
func TestHandleDeletion_Clustered_HeldVMNeverTouchesThePreviousIncarnation(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
	prov := &routingProvider{onCreate: previousIncarnationOn("host-alpha"),
		deleteErr: contracts.NewNotFoundError("delete: libvirt domain \"default.web\" is not owned by this VirtualMachine", nil)}
	r := clusteredFixture(t, prov, vm)
	createClustered(t, r, prov, "web")
	require.Equal(t, "host-alpha", getVM(t, r, "web").Status.Placement.PendingHost)

	_, err := r.handleDeletion(context.Background(), deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	require.Len(t, prov.deleteRefs, 1)
	assert.Equal(t, "host-alpha", prov.deleteRefs[0].HostID)
	assert.Equal(t, "uid-web", prov.deleteRefs[0].Owner.UID, "the Delete is owner-checked with the held VM's own UID")
	getErr := r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "web"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(getErr), "finalizer released")
}

// ownDomainElsewhereErr is the error the transport maps a provider's
// AlreadyExists + VM_PREVIOUS_INCARNATION of kind "own" to.
func ownDomainElsewhereErr(domain string) error {
	return contracts.NewConflictError(
		fmt.Sprintf("create: create of libvirt domain %q refused: a domain of this VirtualMachine — stamped with its own "+
			"UID — already exists on another host of this Provider", domain),
		fmt.Errorf("%w: %w: rpc error", contracts.ErrVMPreviousIncarnation, contracts.ErrVMOwnDomainElsewhere))
}

// TestClustered_OwnDomainElsewhere (security review of A6.1, item 8): a
// create answered with the VM's OWN domain on another host is held like a
// previous incarnation, with its own message (move pendingHost, no re-stamp)
// naming no host; deleting the held VM keeps the finalizer — its owner-checked
// Delete on the pending host finds nothing, and releasing it would leave its
// own domain running — with DeleteBlocked=True/OwnDomainOnAnotherHost, until
// force-delete (or pendingHost pointing at that host). A previous incarnation
// under another UID still releases (it is not this VM's domain).
func TestClustered_OwnDomainElsewhere(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
	prov := &routingProvider{
		onCreate: func(contracts.CreateRequest) (contracts.CreateResponse, error) {
			return contracts.CreateResponse{}, ownDomainElsewhereErr("default.web")
		},
		deleteErr: contracts.NewNotFoundError("delete: libvirt domain \"web\" not found on host host-alpha", nil),
	}
	r := clusteredFixture(t, prov, vm)
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	createClustered(t, r, prov, "web")
	held := getVM(t, r, "web")
	placed := placedCondition(held)
	require.NotNil(t, placed)
	assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, placed.Reason, "a dedicated reason, not RestorePending (fix verification N7)")
	assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, provisioningReason(held))
	assert.Contains(t, placed.Message, "stamped with its own UID")
	assert.Contains(t, placed.Message, "No re-stamp is needed")
	assert.NotContains(t, placed.Message, "host-alpha")
	assert.Equal(t, "host-alpha", held.Status.Placement.PendingHost)
	assert.Empty(t, held.Status.Placement.ExcludedHosts)

	res, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	kept := getVM(t, r, "web")
	assert.Contains(t, kept.Finalizers, infrav1beta1.VirtualMachineFinalizer, "its own domain elsewhere keeps the finalizer")
	blocked := meta.FindStatusCondition(kept.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, blocked.Reason)
	assert.NotContains(t, blocked.Message, "host-alpha")
	require.Len(t, prov.deleteRefs, 1, "the owner-checked Delete still went to the pending host first")

	kept.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(ctx, kept))
	_, err = r.handleDeletion(ctx, getVM(t, r, "web"))
	require.NoError(t, err)
	getErr := r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "web"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(getErr), "force-delete releases it")
}

// TestVMClone_Clustered_PreviousIncarnationOfTheTargetHolds: a Clone answered
// VM_PREVIOUS_INCARNATION keeps the target's pendingHost (no exclusion), marks
// the target Placed=False/RestorePending, and leaves the clone Pending — never
// Failed, which would remove the target — re-checking slowly on the same
// host.
func TestVMClone_Clustered_PreviousIncarnationOfTheTargetHolds(t *testing.T) {
	cp := &clonerProvider{cloneErr: previousIncarnationErr("default.clone-c-target")}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.GreaterOrEqual(t, cp.cloneCnt, 1)
	assert.Equal(t, "host-alpha", cp.lastClone.TargetHostID, "retried on the same host only")
	target := getTarget(t, r)
	assert.Empty(t, target.Status.ID)
	require.NotNil(t, target.Status.Placement)
	assert.Equal(t, "host-alpha", target.Status.Placement.PendingHost, "the target keeps its pending host")
	assert.Empty(t, target.Status.Placement.ExcludedHosts, "the host is NOT excluded")
	placed := meta.FindStatusCondition(target.Status.Conditions, k8s.ConditionPlaced)
	require.NotNil(t, placed)
	assert.Equal(t, k8s.ReasonRestorePending, placed.Reason)

	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "held, never failed")
	cond := cloneReadyCondition(t, got)
	assert.Equal(t, k8s.ReasonRestorePending, cond.Reason)
	assert.Contains(t, cond.Message, "docs/clustered-restore.md")
	assert.NotContains(t, cond.Message, "host-alpha")

	var warnings int
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+k8s.ReasonRestorePending) {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings, "one Warning event for the hold")
}

// TestVMClone_Clustered_OwnDomainOfTheTargetHolds (A6.1 fix verification,
// N7): a Clone answered with the TARGET's own domain on another host holds
// the target with the dedicated reason OwnDomainOnAnotherHost (not
// RestorePending), so deleting that target keeps its finalizer
// (DeleteBlocked=True/OwnDomainOnAnotherHost) instead of releasing it on the
// pending host's NotFound and leaving its domain running.
func TestVMClone_Clustered_OwnDomainOfTheTargetHolds(t *testing.T) {
	cp := &clonerProvider{cloneErr: ownDomainElsewhereErr("default.clone-c-target")}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	target := getTarget(t, r)
	assert.Empty(t, target.Status.ID)
	require.NotNil(t, target.Status.Placement)
	assert.Equal(t, "host-alpha", target.Status.Placement.PendingHost, "the target keeps its pending host")
	placed := meta.FindStatusCondition(target.Status.Conditions, k8s.ConditionPlaced)
	require.NotNil(t, placed)
	assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, placed.Reason)
	assert.Contains(t, placed.Message, "stamped with its own UID")
	assert.True(t, heldForOwnDomainElsewhere(target), "a delete of the target is held")

	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "held, never failed")
	assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, cloneReadyCondition(t, got).Reason)
}

// TestHeldForOwnDomainElsewhere: the hold is recognized by its dedicated
// reason alone — never by message text — on an unbound VM only.
func TestHeldForOwnDomainElsewhere(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	assert.False(t, heldForOwnDomainElsewhere(vm), "no Placed condition")
	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonRestorePending, restorePendingOwnMessage)
	assert.False(t, heldForOwnDomainElsewhere(vm), "the message alone does not make the hold")
	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonOwnDomainOnAnotherHost, "any wording")
	assert.True(t, heldForOwnDomainElsewhere(vm))
	vm.Status.ID = "default.web"
	assert.False(t, heldForOwnDomainElsewhere(vm), "a bound VM is deleted by its id")
}
