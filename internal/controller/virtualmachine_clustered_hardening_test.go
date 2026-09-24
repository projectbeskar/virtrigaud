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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the security-review hardening of ADR-0007 Addendum A slice 1:
// fail-closed routing when a VM's recorded placement no longer matches its
// Provider's topology, host-scoped unavailability on a routed Describe, the
// scheduler treating a deleting Host as cordoned, and the bounded HostInUse
// message.

func readyReason(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if c := k8s.GetCondition(vm.Status.Conditions, k8s.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

// singleFixture seeds a SINGLE-topology Provider named prov-cluster (as if a
// clustered one had been flipped) plus the VM's class and image.
func singleFixture(t *testing.T, prov contracts.Provider, vm *infravirtrigaudiov1beta1.VirtualMachine) *VirtualMachineReconciler {
	t.Helper()
	return newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm,
		withRuntime(singleProviderCR("prov-cluster", clusteredNS)), smallVMClass(clusteredNS), minimalVMImage(clusteredNS))
}

// TestReconcileVM_PlacementTopologyMismatch_NoProviderCall: a VM that records a
// clustered placement on a Provider that is no longer clustered is failed
// closed — no Describe, Power or Create — with Ready=False/PlacementTopologyMismatch.
func TestReconcileVM_PlacementTopologyMismatch_NoProviderCall(t *testing.T) {
	cases := map[string]func(*infravirtrigaudiov1beta1.VirtualMachine){
		"bound": func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.ID = "web"
			vm.Status.Placement.Host = "host-alpha"
		},
		"pending": func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.Placement.PendingHost = "host-alpha" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{}
			mutate(vm)
			prov := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
			r := singleFixture(t, prov, vm)

			res, err := r.reconcileVM(context.Background(), getVM(t, r, "web"))
			require.NoError(t, err)
			assert.Equal(t, placementConfigRetryInterval, res.RequeueAfter)
			assert.Empty(t, prov.describeRefs, "no Describe")
			assert.Empty(t, prov.powerRefs, "no Power")
			assert.Empty(t, prov.createReqs, "no Create on the single-host path")
			assert.Equal(t, k8s.ReasonPlacementTopologyMismatch, readyReason(getVM(t, r, "web")))
		})
	}
}

// TestHandleDeletion_PlacementTopologyMismatch_NeverCallsProvider: after a
// cluster -> single flip, a placed VM's delete must not go down the
// un-owner-checked single-host path (where it could destroy another tenant's
// same-named domain). The finalizer is retained unless force-delete is set.
func TestHandleDeletion_PlacementTopologyMismatch_NeverCallsProvider(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*infravirtrigaudiov1beta1.VirtualMachine)
		force  bool
	}{
		{"bound", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.ID = "web"
			vm.Status.Placement.Host = "host-alpha"
		}, false},
		{"pending", func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.Placement.PendingHost = "host-alpha" }, false},
		{"bound, force-delete", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.ID = "web"
			vm.Status.Placement.Host = "host-alpha"
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{}
			tc.mutate(vm)
			if tc.force {
				vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			}
			prov := &routingProvider{}
			r := singleFixture(t, prov, vm)

			res, err := r.handleDeletion(ctx, markForDeletion(t, r, getVM(t, r, "web")))
			require.NoError(t, err)
			assert.Empty(t, prov.deleteRefs, "a mismatched VM is never sent a Delete")

			var after infravirtrigaudiov1beta1.VirtualMachine
			getErr := r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "web"}, &after)
			if tc.force {
				assert.True(t, apierrors.IsNotFound(getErr), "force-delete releases the finalizer")
				return
			}
			require.NoError(t, getErr)
			assert.Equal(t, vmDeleteRetryInterval, res.RequeueAfter)
			assert.Contains(t, after.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
			assert.Equal(t, k8s.ReasonPlacementTopologyMismatch, readyReason(&after))
		})
	}
}

// TestReconcileVM_Clustered_BoundHostUnavailableBacksOff: a host-scoped
// unavailability on a routed Describe is re-checked every 30s (not 5s), keeps
// the binding and the id, and never re-creates.
func TestReconcileVM_Clustered_BoundHostUnavailableBacksOff(t *testing.T) {
	prov := &routingProvider{describeErr: contracts.NewHostUnavailableError("describe: connect to host \"host-alpha\"", nil)}
	r := clusteredFixture(t, prov, boundClusterVM("app"))

	res, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	assert.Equal(t, boundHostUnavailableRetryInterval, res.RequeueAfter)
	assert.GreaterOrEqual(t, boundHostUnavailableRetryInterval, 30*time.Second)
	assert.Empty(t, prov.createReqs)
	after := getVM(t, r, "app")
	assert.Equal(t, "app", after.Status.ID)
	assert.Equal(t, "host-alpha", after.Status.Placement.Host)
	assert.Equal(t, k8s.ReasonHostUnavailable, readyReason(after))
}

// deletingHost is a Host that is being deleted (held by the in-use finalizer).
func deletingHost(name string) *infravirtrigaudiov1beta1.Host {
	h := readyHost(name, clusteredNS, "pool-a", "prov-cluster")
	now := metav1.Now()
	h.DeletionTimestamp = &now
	h.Finalizers = []string{infravirtrigaudiov1beta1.HostInUseFinalizer}
	return h
}

// TestResolveClusterPlacement_DeletingHostIsCordoned: the scheduler never
// places a new VM on a Host that is being deleted — otherwise tenants would
// keep re-arming its in-use finalizer and block the deletion forever.
func TestResolveClusterPlacement_DeletingHostIsCordoned(t *testing.T) {
	t.Run("the other host is chosen", func(t *testing.T) {
		vm := clusterVM("vm-new", clusteredNS, "prov-cluster")
		prov := &routingProvider{}
		// clusteredFixture seeds host-alpha (Ready); add a deleting host-bravo
		// with more free capacity so Spread would otherwise prefer it.
		bravo := deletingHost("host-bravo")
		bravo.Status.AllocatableCPU = i32p(64)
		bravo.Status.AllocatableMemoryMiB = i64p(1 << 20)
		r := clusteredFixture(t, prov, vm, bravo)

		_, err := r.createVM(context.Background(), getVM(t, r, "vm-new"), prov, clusteredProviderCR("prov-cluster", clusteredNS),
			smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
		require.NoError(t, err)
		require.Len(t, prov.createReqs, 1)
		assert.Equal(t, "host-alpha", prov.createReqs[0].TargetHostID, "a deleting host is never chosen")
	})

	t.Run("only a deleting host: unschedulable, no create", func(t *testing.T) {
		vm := clusterVM("vm-none", clusteredNS, "prov-cluster")
		prov := &routingProvider{}
		providerCR := withRuntime(clusteredProviderCR("prov-cluster", clusteredNS))
		r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, providerCR,
			hostPoolCR("pool-a", clusteredNS, "prov-cluster"), deletingHost("host-alpha"),
			smallVMClass(clusteredNS), minimalVMImage(clusteredNS))

		_, err := r.createVM(context.Background(), getVM(t, r, "vm-none"), prov, providerCR,
			smallVMClass(clusteredNS), minimalVMImage(clusteredNS), nil)
		require.NoError(t, err)
		assert.Empty(t, prov.createReqs)
		assert.Equal(t, k8s.ReasonUnschedulable, provisioningReason(getVM(t, r, "vm-none")))
	})
}

// TestHostInUseMessage_Bounded: the HostInUse message gives the full count but
// lists at most hostInUseListedVMs names, so it stays far below the 32768-byte
// condition-message limit however many VMs use the Host.
func TestHostInUseMessage_Bounded(t *testing.T) {
	var users []string
	for i := 0; i < 5000; i++ {
		users = append(users, fmt.Sprintf("tenant-%04d/a-rather-long-virtual-machine-name-%04d", i, i))
	}
	msg := hostInUseMessage(users)
	assert.Contains(t, msg, "5000 VirtualMachine(s)")
	assert.Contains(t, msg, users[0])
	assert.Contains(t, msg, users[hostInUseListedVMs-1])
	assert.NotContains(t, msg, users[hostInUseListedVMs], "names beyond the cap are not listed")
	assert.Contains(t, msg, fmt.Sprintf("and %d more", 5000-hostInUseListedVMs))
	assert.Less(t, len(msg), 2048)

	small := hostInUseMessage([]string{"ns/a", "ns/b"})
	assert.Contains(t, small, "(ns/a, ns/b)")
	assert.False(t, strings.Contains(small, "more"))
}
