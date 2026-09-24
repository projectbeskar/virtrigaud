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
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VirtualMachine controller's side of ADR-0007 Addendum A
// slice 2: routed, owner-carrying Power and Reconfigure; a host-scoped
// unavailability or a not-found on either handled like the same answer to
// Describe; and the A2 amendment — a Create refused with a name conflict
// releases its pending host, excludes it, and re-schedules.

// resizedBoundClusterVM is a bound clustered VM whose recorded resources differ
// from its class (smallVMClass: 2 vCPU / 4Gi), so a reconcile that finds it
// powered On reconfigures it.
func resizedBoundClusterVM(name string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := boundClusterVM(name)
	vm.UID = types.UID("uid-" + name)
	cpu, mem := int32(1), int64(4096)
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
	return vm
}

// ─── routed Power / Reconfigure ───────────────────────────────────────────────

func TestReconcileVM_Clustered_PowerAndReconfigureCarryHostAndOwner(t *testing.T) {
	want := func(name string) contracts.VMRef {
		return contracts.VMRef{ID: name, HostID: "host-alpha",
			Owner: contracts.ObjectIdentity{UID: "uid-" + name, Namespace: clusteredNS, Name: name}}
	}

	t.Run("power", func(t *testing.T) {
		prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "Off"}}
		r := clusteredFixture(t, prov, resizedBoundClusterVM("pwr"))
		_, err := r.reconcileVM(context.Background(), getVM(t, r, "pwr"))
		require.NoError(t, err)
		assert.Equal(t, []contracts.VMRef{want("pwr")}, prov.powerRefs)
	})

	t.Run("reconfigure", func(t *testing.T) {
		prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
		r := clusteredFixture(t, prov, resizedBoundClusterVM("rcf"))
		_, err := r.reconcileVM(context.Background(), getVM(t, r, "rcf"))
		require.NoError(t, err)
		assert.Empty(t, prov.powerRefs)
		assert.Equal(t, []contracts.VMRef{want("rcf")}, prov.reconfigureRefs)
		assert.Equal(t, int32(2), *getVM(t, r, "rcf").Status.CurrentResources.CPU, "a successful reconfigure is recorded")
	})
}

// routedOpCase drives either Power (the VM is Off, desired On) or Reconfigure
// (the VM is On with resources that differ from its class) to a scripted error.
type routedOpCase struct {
	name  string
	state string
	set   func(p *routingProvider, err error)
}

var routedOpCases = []routedOpCase{
	{name: "power", state: "Off", set: func(p *routingProvider, err error) { p.powerErr = err }},
	{name: "reconfigure", state: "On", set: func(p *routingProvider, err error) { p.reconfigureErr = err }},
}

// TestReconcileVM_Clustered_RoutedOpHostUnavailable: a host-scoped
// Unavailable on Power or Reconfigure is Ready=False/HostUnavailable with the
// 30s re-check (not 5s), and the binding and id are kept; nothing is re-created.
func TestReconcileVM_Clustered_RoutedOpHostUnavailable(t *testing.T) {
	for _, tc := range routedOpCases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: tc.state}}
			tc.set(prov, contracts.NewHostUnavailableError(tc.name+": connect to host \"host-alpha\"", nil))
			r := clusteredFixture(t, prov, resizedBoundClusterVM("app"))

			res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
			require.NoError(t, err)
			assert.Equal(t, boundHostUnavailableRetryInterval, res.RequeueAfter)
			assert.Empty(t, prov.createReqs)
			after := getVM(t, r, "app")
			assert.Equal(t, "app", after.Status.ID)
			assert.Equal(t, "host-alpha", after.Status.Placement.Host)
			ready := k8s.GetCondition(after.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, k8s.ReasonHostUnavailable, ready.Reason)
			assert.Equal(t, after.Generation, ready.ObservedGeneration)
		})
	}
}

// TestReconcileVM_Clustered_RoutedOpNotFoundIsA4: a not-found on Power or
// Reconfigure — the domain is gone, or its owner stamp is not this VM's — is
// handled like a missing VM on Describe: Ready=False/VMMissingOnHost, the slow
// re-check, and never a re-create.
func TestReconcileVM_Clustered_RoutedOpNotFoundIsA4(t *testing.T) {
	for _, tc := range routedOpCases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: tc.state}}
			tc.set(prov, contracts.NewNotFoundError(`libvirt domain "app" on host host-alpha is not owned by this VirtualMachine`, nil))
			r := clusteredFixture(t, prov, resizedBoundClusterVM("app"))

			res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
			require.NoError(t, err)
			assert.Equal(t, vmMissingOnHostRetryInterval, res.RequeueAfter)
			assert.Empty(t, prov.createReqs, "A4: never re-created")
			after := getVM(t, r, "app")
			assert.Equal(t, "app", after.Status.ID)
			assert.Equal(t, "host-alpha", after.Status.Placement.Host)
			assert.Equal(t, k8s.ReasonVMMissingOnHost, readyReason(after))
		})
	}
}

// TestReconcileVM_SingleHost_PowerErrorsKeepHistoricalHandling pins D9: a
// single-host VM's Power error — even one classified NotFound or
// HostUnavailable — takes the historical 5s ProviderError path.
func TestReconcileVM_SingleHost_PowerErrorsKeepHistoricalHandling(t *testing.T) {
	for name, perr := range map[string]error{
		"not-found":        contracts.NewNotFoundError("power: gone", nil),
		"host-unavailable": contracts.NewHostUnavailableError("power: host", nil),
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("legacy", clusteredNS, "prov-single")
			vm.Status.ID = "legacy"
			prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "Off"}, powerErr: perr}
			r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm,
				withRuntime(singleProviderCR("prov-single", clusteredNS)), smallVMClass(clusteredNS), minimalVMImage(clusteredNS))

			res, err := r.reconcileVM(context.Background(), getVM(t, r, "legacy"))
			require.NoError(t, err)
			assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
			assert.Equal(t, []contracts.VMRef{{ID: "legacy"}}, prov.powerRefs, "single-host Power carries no host and no owner")
			assert.Equal(t, k8s.ReasonProviderError, readyReason(getVM(t, r, "legacy")))
		})
	}
}

// ─── A2 amendment: a Create refused with a name conflict ─────────────────────

// conflictOn answers Create with a name conflict on the listed hosts and
// succeeds elsewhere.
func conflictOn(hosts ...string) func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
	return func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		for _, h := range hosts {
			if req.TargetHostID == h {
				return contracts.CreateResponse{}, contracts.NewConflictError(fmt.Sprintf(
					"libvirt domain %q already exists on this host and is not owned by this VirtualMachine", req.Name), nil)
			}
		}
		return contracts.CreateResponse{ID: req.Name}, nil
	}
}

// createClustered runs one createVM of the clustered VM name, as a reconcile
// would, and returns its result.
func createClustered(t *testing.T, r *VirtualMachineReconciler, prov contracts.Provider, name string) ctrlResult {
	t.Helper()
	out, err := r.createVM(context.Background(), getVM(t, r, name), prov, clusteredProviderCR("prov-cluster", clusteredNS),
		smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
	require.NoError(t, err)
	return ctrlResult{requeue: out.Requeue, after: out.RequeueAfter.String()}
}

// ctrlResult is a comparable view of a ctrl.Result.
type ctrlResult struct {
	requeue bool
	after   string
}

// TestCreateVM_Clustered_ConflictExcludesHostAndReschedules is the tracked fix
// from the slice 1 security review: a Create refused with a name conflict on
// the pending host clears pendingHost (it proves this VM created nothing
// there), excludes the host durably, and the next reconcile schedules the VM
// onto another host; binding it clears the exclusions.
func TestCreateVM_Clustered_ConflictExcludesHostAndReschedules(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	prov := &routingProvider{}
	// host-alpha comes from clusteredFixture; host-bravo is bigger, so Spread
	// picks it first.
	bravo := readyHost("host-bravo", clusteredNS, "pool-a", "prov-cluster")
	bravo.Status.AllocatableCPU = i32p(64)
	bravo.Status.AllocatableMemoryMiB = i64p(1 << 20)
	r := clusteredFixture(t, prov, vm, bravo)
	prov.onCreate = conflictOn("host-bravo")

	first := createClustered(t, r, prov, "web")
	assert.Equal(t, ctrlResult{after: createConflictRescheduleInterval.String()}, first, "re-scheduled promptly, not on the 2m conflict cadence")
	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, "host-bravo", prov.createReqs[0].TargetHostID)

	afterConflict := getVM(t, r, "web")
	require.NotNil(t, afterConflict.Status.Placement)
	assert.Empty(t, afterConflict.Status.Placement.PendingHost, "the conflicting host no longer pins the VM")
	assert.Empty(t, afterConflict.Status.Placement.Host)
	assert.Equal(t, []string{"host-bravo"}, afterConflict.Status.Placement.ExcludedHosts, "the exclusion is persisted")
	assert.Empty(t, afterConflict.Status.ID, "nothing is bound")
	placed := placedCondition(afterConflict)
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionFalse, placed.Status)
	assert.Equal(t, k8s.ReasonHostExcluded, placed.Reason)
	assert.Equal(t, afterConflict.Generation, placed.ObservedGeneration)
	assert.Equal(t, k8s.ReasonProviderConflict, provisioningReason(afterConflict))

	second := createClustered(t, r, prov, "web")
	assert.Equal(t, ctrlResult{after: providerErrorRetryInterval.String()}, second)
	require.Len(t, prov.createReqs, 2)
	assert.Equal(t, "host-alpha", prov.createReqs[1].TargetHostID, "the excluded host is never chosen again")

	bound := getVM(t, r, "web")
	assert.Equal(t, "host-alpha", bound.Status.Placement.Host)
	assert.Empty(t, bound.Status.Placement.ExcludedHosts, "binding clears the exclusions")
	assert.Equal(t, k8s.ReasonBound, placedCondition(bound).Reason)
}

// TestCreateVM_Clustered_ConflictOnPendingHostRetryReleasesIt: the conflict
// also releases a pending host that a PREVIOUS reconcile recorded (the retry
// path, which does not consult the scheduler).
func TestCreateVM_Clustered_ConflictOnPendingHostRetryReleasesIt(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a",
		ExcludedHosts: []string{"host-old"}}
	prov := &routingProvider{onCreate: conflictOn("host-alpha")}
	r := clusteredFixture(t, prov, vm)

	createClustered(t, r, prov, "web")
	after := getVM(t, r, "web")
	assert.Empty(t, after.Status.Placement.PendingHost)
	assert.Equal(t, []string{"host-old", "host-alpha"}, after.Status.Placement.ExcludedHosts)
}

// TestCreateVM_Clustered_AllHostsExcluded: once every candidate is excluded,
// the VM is Placed=False/AllHostsExcluded, no Create is sent, and it is only
// re-checked on the slow conflict cadence — no hot loop.
func TestCreateVM_Clustered_AllHostsExcluded(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	prov := &routingProvider{onCreate: conflictOn("host-alpha")}
	r := clusteredFixture(t, prov, vm) // host-alpha is the only candidate

	createClustered(t, r, prov, "web")
	require.Len(t, prov.createReqs, 1)

	for i := 0; i < 3; i++ {
		res := createClustered(t, r, prov, "web")
		assert.Equal(t, ctrlResult{after: vmCreateConflictRetryInterval.String()}, res)
	}
	assert.Len(t, prov.createReqs, 1, "no further Create once every candidate is excluded")

	after := getVM(t, r, "web")
	placed := placedCondition(after)
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionFalse, placed.Status)
	assert.Equal(t, k8s.ReasonAllHostsExcluded, placed.Reason)
	assert.Contains(t, placed.Message, "host-alpha")
	assert.Contains(t, placed.Message, "status.placement.excludedHosts", "the message says how to recover")
	assert.Equal(t, k8s.ReasonUnschedulable, provisioningReason(after))
	assert.Empty(t, after.Status.Placement.PendingHost)
}

// TestCreateVM_Clustered_ExcludedButOtherwiseInfeasibleIsOrdinaryUnschedulable:
// when some candidate is rejected for another reason (here: cordoned), the VM
// is ordinarily Unschedulable on the normal cadence — that may clear by itself.
func TestCreateVM_Clustered_ExcludedButOtherwiseInfeasibleIsOrdinaryUnschedulable(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{ExcludedHosts: []string{"host-alpha"}}
	cordoned := readyHost("host-bravo", clusteredNS, "pool-a", "prov-cluster")
	cordoned.Spec.Schedulable = false
	prov := &routingProvider{}
	r := clusteredFixture(t, prov, vm, cordoned)

	res := createClustered(t, r, prov, "web")
	assert.Equal(t, ctrlResult{after: placementUnschedulableRetryInterval.String()}, res)
	assert.Empty(t, prov.createReqs)
	assert.Equal(t, k8s.ReasonUnschedulable, provisioningReason(getVM(t, r, "web")))
}

// TestHandleDeletion_Clustered_AfterConflictNeverCallsProvider: a VM whose only
// create attempt was refused with a name conflict owns nothing on any host, so
// its finalizer is released without a provider Delete.
func TestHandleDeletion_Clustered_AfterConflictNeverCallsProvider(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	prov := &routingProvider{onCreate: conflictOn("host-alpha")}
	r := clusteredFixture(t, prov, vm)
	createClustered(t, r, prov, "web")

	_, err := r.handleDeletion(context.Background(), deletingClusterVM(t, r, "web"))
	require.NoError(t, err)
	assert.Empty(t, prov.deleteRefs)
	getErr := r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "web"}, &infravirtrigaudiov1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(getErr), "finalizer released")
}

func TestExcludeHost_DedupesTrimsAndStaysBounded(t *testing.T) {
	pl := &infravirtrigaudiov1beta1.PlacementStatus{}
	assert.False(t, excludeHost(pl, " host-a "))
	assert.False(t, excludeHost(pl, "host-a"))
	assert.False(t, excludeHost(pl, ""))
	assert.Equal(t, []string{"host-a"}, pl.ExcludedHosts)

	dropped := 0
	for i := 0; i < maxExcludedHosts+5; i++ {
		if excludeHost(pl, "h-"+strconv.Itoa(i)) {
			dropped++
		}
	}
	require.Len(t, pl.ExcludedHosts, maxExcludedHosts)
	assert.Equal(t, 6, dropped, "every addition past the cap reports a dropped entry")
	assert.Equal(t, "h-"+strconv.Itoa(maxExcludedHosts+4), pl.ExcludedHosts[maxExcludedHosts-1], "the newest is kept")
	assert.NotContains(t, pl.ExcludedHosts, "host-a", "the oldest is dropped")
}

// TestCreateVM_Clustered_ConflictWithAFullListBacksOff: when the exclusion
// list is already full, a further conflict drops the oldest entry and the VM
// waits the slow conflict cadence — a pool with more conflicting hosts than
// the cap cannot become a fast create loop.
func TestCreateVM_Clustered_ConflictWithAFullListBacksOff(t *testing.T) {
	full := make([]string, 0, maxExcludedHosts)
	for i := 0; i < maxExcludedHosts; i++ {
		full = append(full, "host-old-"+strconv.Itoa(i))
	}
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-alpha", ExcludedHosts: full}
	prov := &routingProvider{onCreate: conflictOn("host-alpha")}
	r := clusteredFixture(t, prov, vm)

	res := createClustered(t, r, prov, "web")
	assert.Equal(t, ctrlResult{after: vmCreateConflictRetryInterval.String()}, res)
	after := getVM(t, r, "web").Status.Placement
	require.Len(t, after.ExcludedHosts, maxExcludedHosts)
	assert.Equal(t, "host-alpha", after.ExcludedHosts[maxExcludedHosts-1])
	assert.NotContains(t, after.ExcludedHosts, "host-old-0")
}

// TestExcludedHostsCapMatchesCRD pins maxExcludedHosts to the CRD's maxItems,
// so the controller never writes a list the API server would reject.
func TestExcludedHostsCapMatchesCRD(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/infra.virtrigaud.io_virtualmachines.yaml")
	require.NoError(t, err)
	lines := strings.Split(string(raw), "\n")
	found := false
	for i, l := range lines {
		if strings.TrimSpace(l) != "excludedHosts:" {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			if v, ok := strings.CutPrefix(strings.TrimSpace(next), "maxItems: "); ok {
				assert.Equal(t, strconv.Itoa(maxExcludedHosts), v)
				found = true
			}
		}
	}
	assert.True(t, found, "status.placement.excludedHosts must declare maxItems")
}
