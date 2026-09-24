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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VirtualMachine controller's side of ADR-0007 Addendum A
// (slice 1): VMRef threading for per-VM calls, the pendingHost
// write-before-Create (A2), the owner-checked finalizer cleanup, the
// no-recreate rule (A4) and the Placed condition — plus proof that a
// single-host VM's calls carry no host at all (D9).

// routingProvider records the VMRef (and owner) every per-VM call receives.
type routingProvider struct {
	stubProvider

	onCreate   func(req contracts.CreateRequest) (contracts.CreateResponse, error)
	createReqs []contracts.CreateRequest

	describeRefs []contracts.VMRef
	describeResp contracts.DescribeResponse
	describeErr  error

	deleteRefs   []contracts.VMRef
	deleteOwners []contracts.ObjectIdentity
	deleteErr    error

	powerRefs []contracts.VMRef
	powerErr  error
}

func (p *routingProvider) Create(_ context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.createReqs = append(p.createReqs, req)
	if p.onCreate != nil {
		return p.onCreate(req)
	}
	return contracts.CreateResponse{ID: req.Name}, nil
}

func (p *routingProvider) Describe(_ context.Context, vm contracts.VMRef) (contracts.DescribeResponse, error) {
	p.describeRefs = append(p.describeRefs, vm)
	return p.describeResp, p.describeErr
}

func (p *routingProvider) Delete(_ context.Context, vm contracts.VMRef) (string, error) {
	p.deleteRefs = append(p.deleteRefs, vm)
	p.deleteOwners = append(p.deleteOwners, vm.Owner)
	return "", p.deleteErr
}

func (p *routingProvider) Power(_ context.Context, vm contracts.VMRef, _ contracts.PowerOp) (string, error) {
	p.powerRefs = append(p.powerRefs, vm)
	return "", p.powerErr
}

const clusteredNS = "default"

// clusteredFixture seeds a clustered Provider with one pool and one Ready host,
// the VM's class and image, and vm itself.
func clusteredFixture(t *testing.T, prov contracts.Provider, vm *infravirtrigaudiov1beta1.VirtualMachine, extra ...client.Object) *VirtualMachineReconciler {
	t.Helper()
	providerCR := withRuntime(clusteredProviderCR("prov-cluster", clusteredNS))
	pool := hostPoolCR("pool-a", clusteredNS, providerCR.Name)
	host := readyHost("host-alpha", clusteredNS, pool.Name, providerCR.Name)
	objs := append([]client.Object{vm, providerCR, pool, host, smallVMClass(clusteredNS), minimalVMImage(clusteredNS)}, extra...)
	return newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, objs...)
}

// withRuntime gives a Provider CR the runtime status reconcileVM logs.
func withRuntime(p *infravirtrigaudiov1beta1.Provider) *infravirtrigaudiov1beta1.Provider {
	p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
	return p
}

func getVM(t *testing.T, r *VirtualMachineReconciler, name string) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: name}, vm))
	return vm
}

func placedCondition(vm *infravirtrigaudiov1beta1.VirtualMachine) *metav1.Condition {
	return k8s.GetCondition(vm.Status.Conditions, k8s.ConditionPlaced)
}

// ─── VMRef helper ─────────────────────────────────────────────────────────────

func TestVMRefFor(t *testing.T) {
	bound := clusterVM("vm", clusteredNS, "prov-cluster")
	bound.UID = "uid-vm"
	bound.Status.ID = "vm"
	bound.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-a", PendingHost: "ignored"}
	boundOwner := contracts.ObjectIdentity{UID: "uid-vm", Namespace: clusteredNS, Name: "vm"}

	t.Run("cluster, bound: host is the confirmed binding, owner is the VM", func(t *testing.T) {
		ref, err := vmRefFor(bound, clusteredProviderCR("prov-cluster", clusteredNS))
		require.NoError(t, err)
		assert.Equal(t, contracts.VMRef{ID: "vm", HostID: "host-a", Owner: boundOwner}, ref)
	})

	t.Run("cluster, unbound: typed Unbound error, no ref", func(t *testing.T) {
		unbound := bound.DeepCopy()
		unbound.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-b"}
		ref, err := vmRefFor(unbound, clusteredProviderCR("prov-cluster", clusteredNS))
		require.Error(t, err)
		assert.True(t, isVMUnbound(err), "an empty binding is the typed unbound error")
		var ue *UnboundVMError
		require.ErrorAs(t, err, &ue)
		assert.Equal(t, "prov-cluster", ue.Provider)
		assert.Equal(t, contracts.VMRef{}, ref, "a pending host is never used as the binding")

		unbound.Status.Placement = nil
		_, err = vmRefFor(unbound, clusteredProviderCR("prov-cluster", clusteredNS))
		assert.True(t, isVMUnbound(err))
	})

	t.Run("single-host: bare id, no host, no owner", func(t *testing.T) {
		plain := bound.DeepCopy()
		plain.Status.Placement = nil
		ref, err := vmRefFor(plain, singleProviderCR("prov-single", clusteredNS))
		require.NoError(t, err)
		assert.Equal(t, contracts.VMRef{ID: "vm"}, ref)
		assert.False(t, ref.Routed())
	})

	t.Run("single-host with a recorded clustered placement fails closed", func(t *testing.T) {
		for name, pl := range map[string]*infravirtrigaudiov1beta1.PlacementStatus{
			"bound":   {Host: "host-a"},
			"pending": {PendingHost: "host-b"},
		} {
			vm := bound.DeepCopy()
			vm.Status.Placement = pl
			ref, err := vmRefFor(vm, singleProviderCR("prov-single", clusteredNS))
			require.Error(t, err, name)
			assert.True(t, isPlacementTopologyMismatch(err), name)
			assert.False(t, isVMUnbound(err), name)
			assert.Equal(t, contracts.VMRef{}, ref, "%s: no ref, so no provider call", name)
			assert.Equal(t, k8s.ReasonPlacementTopologyMismatch, vmRefErrorReason(err))
		}
	})

	t.Run("pendingCreateRef routes the create name to the pending host", func(t *testing.T) {
		vm := clusterVM("web", clusteredNS, "prov-cluster")
		vm.UID = "uid-web"
		_, ok := pendingCreateRef(vm)
		assert.False(t, ok)
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-b"}
		ref, ok := pendingCreateRef(vm)
		require.True(t, ok)
		assert.Equal(t, contracts.VMRef{ID: "web", HostID: "host-b",
			Owner: contracts.ObjectIdentity{UID: "uid-web", Namespace: clusteredNS, Name: "web"}}, ref)
	})
}

// ─── A2: pendingHost write-before-Create ──────────────────────────────────────

// TestCreateVM_Clustered_PendingHostPersistedBeforeCreate proves the attempted
// host is durably in the API server BEFORE Create runs, and that a successful
// Create promotes it into the binding.
func TestCreateVM_Clustered_PendingHostPersistedBeforeCreate(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-a2", clusteredNS, "prov-cluster")
	vm.UID = "uid-a2"
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, vm)
	var persistedAtCreate string
	prov.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		stored := getVM(t, r, "vm-a2")
		if stored.Status.Placement != nil {
			persistedAtCreate = stored.Status.Placement.PendingHost
			assert.Empty(t, stored.Status.Placement.Host, "the binding is not claimed before Create confirms")
		}
		return contracts.CreateResponse{ID: req.Name}, nil
	}

	live := getVM(t, r, "vm-a2")
	_, err := r.createVM(ctx, live, prov, clusteredProviderCR("prov-cluster", clusteredNS), smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)

	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, "host-alpha", prov.createReqs[0].TargetHostID)
	assert.Equal(t, "host-alpha", persistedAtCreate, "pendingHost must be persisted before Create is called")

	after := getVM(t, r, "vm-a2")
	require.NotNil(t, after.Status.Placement)
	assert.Equal(t, "host-alpha", after.Status.Placement.Host, "success promotes pendingHost into the binding")
	assert.Empty(t, after.Status.Placement.PendingHost, "and clears pendingHost")
	assert.Equal(t, "pool-a", after.Status.Placement.Pool)
	assert.Equal(t, "vm-a2", after.Status.ID)
	placed := placedCondition(after)
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionTrue, placed.Status)
	assert.Equal(t, k8s.ReasonBound, placed.Reason)
	assert.Equal(t, after.Generation, placed.ObservedGeneration)
}

// TestCreateVM_Clustered_PendingHostConflictRequeuesWithoutCreate proves a lost
// resourceVersion race on the pendingHost write never reaches Create and never
// re-applies the scheduler's choice to a fresh copy.
func TestCreateVM_Clustered_PendingHostConflictRequeuesWithoutCreate(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-conflict", clusteredNS, "prov-cluster")
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, vm)

	stale := getVM(t, r, "vm-conflict")
	other := getVM(t, r, "vm-conflict")
	other.Labels = map[string]string{"touched": "by-another-writer"}
	require.NoError(t, r.Update(ctx, other), "bump the resourceVersion behind the reconcile's back")

	res, err := r.createVM(ctx, stale, prov, clusteredProviderCR("prov-cluster", clusteredNS), smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)
	assert.True(t, res.Requeue, "a conflict requeues")
	assert.Empty(t, prov.createReqs, "Create must never run when the pending host was not durably recorded")

	after := getVM(t, r, "vm-conflict")
	assert.Nil(t, after.Status.Placement, "the scheduler choice is not re-applied after a conflict")
}

// TestCreateVM_Clustered_RetryReusesPendingHost proves a VM with a create in
// flight is sent back to its pending host without consulting the scheduler.
// The pending host is deliberately NOT a feasible candidate (and no pool
// exists), so re-scheduling would have either failed or chosen differently.
func TestCreateVM_Clustered_RetryReusesPendingHost(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-retry", clusteredNS, "prov-cluster")
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-beta", Pool: "pool-a"}
	prov := &routingProvider{}
	providerCR := clusteredProviderCR("prov-cluster", clusteredNS)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, providerCR,
		smallVMClass(clusteredNS), minimalVMImage(clusteredNS)) // no HostPool, no Host

	live := getVM(t, r, "vm-retry")
	_, err := r.createVM(ctx, live, prov, providerCR, smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)
	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, "host-beta", prov.createReqs[0].TargetHostID, "the retry goes to the pending host as-is")
	assert.NotEqual(t, k8s.ReasonNoHostPool, provisioningReason(getVM(t, r, "vm-retry")), "the scheduler was not re-run")
	assert.Equal(t, "host-beta", getVM(t, r, "vm-retry").Status.Placement.Host)
}

// TestCreateVM_Clustered_UnreachablePendingHost proves a host-scoped
// unavailability on Create keeps the VM pinned to its pending host (never
// re-scheduled) and reports Placed=False/HostUnavailable.
func TestCreateVM_Clustered_UnreachablePendingHost(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("vm-unreach", clusteredNS, "prov-cluster")
	prov := &routingProvider{onCreate: func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, contracts.NewHostUnavailableError("create: connect to target host \"host-alpha\"", nil)
	}}
	r := clusteredFixture(t, prov, vm)

	for i := 0; i < 2; i++ {
		live := getVM(t, r, "vm-unreach")
		res, err := r.createVM(ctx, live, prov, clusteredProviderCR("prov-cluster", clusteredNS), smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
		require.NoError(t, err)
		assert.Equal(t, pendingHostUnavailableRetryInterval, res.RequeueAfter)
	}
	require.Len(t, prov.createReqs, 2)
	for _, req := range prov.createReqs {
		assert.Equal(t, "host-alpha", req.TargetHostID, "every retry goes to the same pending host")
	}
	after := getVM(t, r, "vm-unreach")
	require.NotNil(t, after.Status.Placement)
	assert.Equal(t, "host-alpha", after.Status.Placement.PendingHost)
	assert.Empty(t, after.Status.Placement.Host)
	placed := placedCondition(after)
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionFalse, placed.Status)
	assert.Equal(t, k8s.ReasonHostUnavailable, placed.Reason)
}

// TestCreateVM_Clustered_ProviderUnavailableIsNotHostUnavailable pins that a
// provider-level transient failure (not host-scoped) is not reported as the
// pending host being unavailable: it stays CreatePending on the normal cadence,
// still pinned to the same host.
func TestCreateVM_Clustered_ProviderUnavailableIsNotHostUnavailable(t *testing.T) {
	vm := clusterVM("vm-provdown", clusteredNS, "prov-cluster")
	prov := &routingProvider{onCreate: func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, contracts.NewRetryableError("create: provider pod unavailable", nil)
	}}
	r := clusteredFixture(t, prov, vm)
	res, err := r.createVM(context.Background(), getVM(t, r, "vm-provdown"), prov, clusteredProviderCR("prov-cluster", clusteredNS),
		smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
	after := getVM(t, r, "vm-provdown")
	assert.Equal(t, "host-alpha", after.Status.Placement.PendingHost)
	assert.Equal(t, k8s.ReasonCreatePending, placedCondition(after).Reason)
}

// ─── finalizer ────────────────────────────────────────────────────────────────

func deletingClusterVM(t *testing.T, r *VirtualMachineReconciler, name string) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm := getVM(t, r, name)
	return markForDeletion(t, r, vm)
}

// TestHandleDeletion_Clustered_PendingHostOwnerCheckedDelete proves a VM whose
// create is in flight (pendingHost set, no Status.ID) is cleaned up with an
// owner-checked Delete routed to its pending host, so a domain the create
// already made there does not leak.
func TestHandleDeletion_Clustered_PendingHostOwnerCheckedDelete(t *testing.T) {
	for name, deleteErr := range map[string]error{
		"deleted":                           nil,
		"not ours (provider says NotFound)": contracts.NewNotFoundError("delete: domain not owned by this VirtualMachine", nil),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-web"
			vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha"}
			prov := &routingProvider{deleteErr: deleteErr}
			r := clusteredFixture(t, prov, vm)

			_, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "web"))
			require.NoError(t, err)

			require.Len(t, prov.deleteRefs, 1)
			assert.Equal(t, "web", prov.deleteRefs[0].ID)
			assert.Equal(t, "host-alpha", prov.deleteRefs[0].HostID)
			assert.Equal(t, contracts.ObjectIdentity{UID: "uid-web", Namespace: clusteredNS, Name: "web"}, prov.deleteOwners[0],
				"the delete carries the owner the provider checks the stamp against")
			getErr := r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "web"}, &infravirtrigaudiov1beta1.VirtualMachine{})
			assert.True(t, apierrors.IsNotFound(getErr), "finalizer released")
		})
	}
}

// TestHandleDeletion_Clustered_BoundDeleteIsRoutedWithOwner proves a bound
// clustered VM's delete is routed to its host and carries the owner.
func TestHandleDeletion_Clustered_BoundDeleteIsRoutedWithOwner(t *testing.T) {
	ctx := context.Background()
	vm := clusterVM("db", clusteredNS, "prov-cluster")
	vm.UID = "uid-db"
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Status.ID = "db"
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha"}
	prov := &routingProvider{deleteErr: stderrors.New("host-alpha: transient")}
	r := clusteredFixture(t, prov, vm)

	res, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "db"))
	require.NoError(t, err)
	require.Len(t, prov.deleteRefs, 1)
	assert.Equal(t, contracts.VMRef{ID: "db", HostID: "host-alpha",
		Owner: contracts.ObjectIdentity{UID: "uid-db", Namespace: clusteredNS, Name: "db"}}, prov.deleteRefs[0])
	assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter, "a failed delete retains the finalizer, unchanged")
}

// TestHandleDeletion_Clustered_UnboundNeverCallsProvider proves a clustered VM
// with an id but no binding is never sent a Delete; the finalizer is retained
// (Placed=False/Unbound) unless the force-delete escape hatch is set.
func TestHandleDeletion_Clustered_UnboundNeverCallsProvider(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "force-delete"}[force], func(t *testing.T) {
			ctx := context.Background()
			vm := clusterVM("lost", clusteredNS, "prov-cluster")
			vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
			vm.Status.ID = "lost"
			if force {
				vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			}
			prov := &routingProvider{}
			r := clusteredFixture(t, prov, vm)

			res, err := r.handleDeletion(ctx, deletingClusterVM(t, r, "lost"))
			require.NoError(t, err)
			assert.Empty(t, prov.deleteRefs, "an unbound clustered VM is never sent a per-VM call")

			var after infravirtrigaudiov1beta1.VirtualMachine
			getErr := r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "lost"}, &after)
			if force {
				assert.True(t, apierrors.IsNotFound(getErr), "force-delete releases the finalizer")
				return
			}
			require.NoError(t, getErr)
			assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
			assert.Contains(t, after.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
			placed := placedCondition(&after)
			require.NotNil(t, placed)
			assert.Equal(t, k8s.ReasonUnbound, placed.Reason)
		})
	}
}

// ─── reconcile: routing, A4, Unbound ──────────────────────────────────────────

func boundClusterVM(name string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := clusterVM(name, clusteredNS, "prov-cluster")
	vm.Status.ID = name
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	return vm
}

// TestReconcileVM_Clustered_DescribeRoutedAndPlacedBound proves the bound host
// rides every per-VM call (Describe, Power) and Placed=True/Bound is asserted.
func TestReconcileVM_Clustered_DescribeRoutedAndPlacedBound(t *testing.T) {
	prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "Off"}}
	r := clusteredFixture(t, prov, boundClusterVM("app"))

	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	want := contracts.VMRef{ID: "app", HostID: "host-alpha", Owner: contracts.ObjectIdentity{Namespace: clusteredNS, Name: "app"}}
	assert.Equal(t, []contracts.VMRef{want}, prov.describeRefs, "Describe carries the host and the owner the provider checks")
	assert.Equal(t, []contracts.VMRef{want}, prov.powerRefs, "power (desired On) is routed to the bound host too")
	placed := placedCondition(getVM(t, r, "app"))
	require.NotNil(t, placed)
	assert.Equal(t, k8s.ReasonBound, placed.Reason)
}

// TestReconcileVM_Clustered_RoutedOpNotSupportedBacksOff proves a per-VM call a
// clustered provider does not route yet (Unimplemented -> NotSupported) is
// re-checked slowly, not every 5s.
func TestReconcileVM_Clustered_RoutedOpNotSupportedBacksOff(t *testing.T) {
	prov := &routingProvider{
		describeResp: contracts.DescribeResponse{Exists: true, PowerState: "Off"},
		powerErr:     contracts.NewNotSupportedError("power: not yet routed on a clustered provider"),
	}
	r := clusteredFixture(t, prov, boundClusterVM("slow"))
	res, err := r.reconcileVM(context.Background(), getVM(t, r, "slow"))
	require.NoError(t, err)
	assert.Equal(t, routedOpNotSupportedRetryInterval, res.RequeueAfter)
}

// TestReconcileVM_Clustered_NotFoundIsNeverRecreated is A4: a clustered VM its
// bound host reports missing is NOT re-created — on either not-found shape.
func TestReconcileVM_Clustered_NotFoundIsNeverRecreated(t *testing.T) {
	cases := map[string]*routingProvider{
		"exists=false":    {describeResp: contracts.DescribeResponse{Exists: false}},
		"not-found error": {describeErr: contracts.NewNotFoundError("describe: domain not found", nil)},
	}
	for name, prov := range cases {
		t.Run(name, func(t *testing.T) {
			r := clusteredFixture(t, prov, boundClusterVM("gone"))
			res, err := r.reconcileVM(context.Background(), getVM(t, r, "gone"))
			require.NoError(t, err)

			assert.Empty(t, prov.createReqs, "A4: a clustered VM is never re-created")
			assert.Equal(t, vmMissingOnHostRetryInterval, res.RequeueAfter)
			after := getVM(t, r, "gone")
			assert.Equal(t, "gone", after.Status.ID, "Status.ID is kept")
			assert.Equal(t, "host-alpha", after.Status.Placement.Host, "the binding is kept")
			ready := k8s.GetCondition(after.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Equal(t, k8s.ReasonVMMissingOnHost, ready.Reason)
			assert.Equal(t, after.Generation, ready.ObservedGeneration)
		})
	}
}

// TestReconcileVM_SingleHost_NotFoundStillRecreates pins that A4 is
// clustered-only: a single-host VM keeps the historical recreate path, and its
// Describe carries no host.
func TestReconcileVM_SingleHost_NotFoundStillRecreates(t *testing.T) {
	vm := clusterVM("legacy", clusteredNS, "prov-single")
	vm.Status.ID = "legacy"
	prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: false}}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm,
		withRuntime(singleProviderCR("prov-single", clusteredNS)), smallVMClass(clusteredNS), minimalVMImage(clusteredNS))

	_, err := r.reconcileVM(context.Background(), getVM(t, r, "legacy"))
	require.NoError(t, err)
	assert.Equal(t, []contracts.VMRef{{ID: "legacy"}}, prov.describeRefs, "single-host Describe carries no host")
	require.Len(t, prov.createReqs, 1, "single-host: the historical recreate path is unchanged")
	assert.Empty(t, prov.createReqs[0].TargetHostID)
	assert.Nil(t, placedCondition(getVM(t, r, "legacy")), "Placed is never set on a single-host VM")
}

// TestReconcileVM_Clustered_UnboundNeverCallsProvider proves a clustered VM
// with an id but no binding gets Placed=False/Unbound and no per-VM call.
func TestReconcileVM_Clustered_UnboundNeverCallsProvider(t *testing.T) {
	vm := clusterVM("orphan", clusteredNS, "prov-cluster")
	vm.Status.ID = "orphan"
	prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
	r := clusteredFixture(t, prov, vm)

	res, err := r.reconcileVM(context.Background(), getVM(t, r, "orphan"))
	require.NoError(t, err)
	assert.Empty(t, prov.describeRefs)
	assert.Empty(t, prov.powerRefs)
	assert.Empty(t, prov.createReqs)
	assert.Equal(t, placementUnboundRetryInterval, res.RequeueAfter)
	placed := placedCondition(getVM(t, r, "orphan"))
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionFalse, placed.Status)
	assert.Equal(t, k8s.ReasonUnbound, placed.Reason)
}

// TestHandleDeletion_SingleHost_DeleteCarriesNoHost pins D9 for delete: the
// single-host delete is the bare id (the owner rides along and is ignored by
// the single-host provider).
func TestHandleDeletion_SingleHost_DeleteCarriesNoHost(t *testing.T) {
	ctx := context.Background()
	vm := deletionVM("vm-single-del")
	vm.UID = "uid-single"
	prov := &routingProvider{}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, deletionProviderCR())
	_, err := r.handleDeletion(ctx, markForDeletion(t, r, vm))
	require.NoError(t, err)
	assert.Equal(t, []contracts.VMRef{{ID: "100"}}, prov.deleteRefs)
}

// guard against an accidental tight loop constant regression.
func TestClusteredRequeueCadencesAreNotTight(t *testing.T) {
	for _, d := range []time.Duration{placementUnboundRetryInterval, pendingHostUnavailableRetryInterval,
		vmMissingOnHostRetryInterval, routedOpNotSupportedRetryInterval} {
		assert.GreaterOrEqual(t, d, 30*time.Second)
	}
}
