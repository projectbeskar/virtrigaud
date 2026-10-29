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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a Reconfiguring=False/ReconcileSuccess recorded before
// a resize reservation never settles it: every reservation write marks the
// outcome pending (Reconfiguring=True/ResizeReserved) in the same checked
// status write, and only the Reconfigure's own outcome replaces it.

// earlierResizeSucceeded records on vm the settled outcome of an earlier,
// completed resize.
func earlierResizeSucceeded(vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type: k8s.ConditionReconfiguring, Status: metav1.ConditionFalse,
		Reason: k8s.ReasonReconcileSuccess, Message: "VM reconfigured successfully",
	})
	return vm
}

// withdrawResize sets the VM's requested CPU back to cpu in the API server.
func withdrawResize(t *testing.T, r *VirtualMachineReconciler, name string, cpu int32) {
	t.Helper()
	vm := getVM(t, r, name)
	wantsCPU(vm, cpu)
	require.NoError(t, r.Update(context.Background(), vm))
}

// TestResizeReservation_StaleSuccessAfterRestart: an earlier resize left
// Reconfiguring=False/ReconcileSuccess. A newer resize is admitted and
// reserved, its Reconfigure is sent, and the manager stops before recording
// the outcome. On a new manager the user withdraws the resize while Describe
// still reports the original size. The earlier success must not settle the
// newer reservation: it stays stored and counted, so no other VM can take the
// room the in-flight Reconfigure may still use.
func TestResizeReservation_StaleSuccessAfterRestart(t *testing.T) {
	prov := reportingProvider(2, 4096) // the domain still reports its original size
	r := resizeFixture(t, prov, wantsCPU(earlierResizeSucceeded(sized("app", 2)), 4), capVM("newvm"))

	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted)
	stored := getVM(t, r, "app")
	require.NotNil(t, stored.Status.Placement.ResizeReservation)
	c := reconfiguringCondition(stored)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status, "the reservation write replaced the earlier success")
	assert.Equal(t, k8s.ReasonResizeReserved, c.Reason)

	_, err = prov.Reconfigure(context.Background(), contracts.VMRef{HostID: "host-alpha"}, contracts.CreateRequest{})
	require.NoError(t, err) // sent; the manager dies before recording its outcome

	r2 := restartedVM(r)
	withdrawResize(t, r2, "app", 2)
	for range 2 {
		_, err = r2.reconcileVM(context.Background(), getVM(t, r2, "app"))
		require.NoError(t, err)
	}
	got := getVM(t, r2, "app")
	require.NotNil(t, got.Status.Placement.ResizeReservation, "the earlier success does not settle the newer reservation")
	assert.Equal(t, int32(4), got.Status.Placement.ResizeReservation.CPU)
	assert.Equal(t, k8s.ReasonResizeReserved, reconfiguringCondition(got).Reason, "the outcome stays unresolved")
	assert.Equal(t, int32(4), admittedFootprint(got, smallVMClass(capNS)).CPU, "still counted at the reservation")
	assert.False(t, newVMFits(t, r2), "a competing VM cannot overbook host-alpha: 4 + 4 of 8 vCPU")
	assert.Len(t, prov.reconfigureRefs, 1, "nothing more is sent for a withdrawn resize")
}

// TestResizeReservation_ConfirmedOutcomeSettles: with an earlier success on
// the VM, a new resize is sent only once its reservation is stored with the
// pending outcome, and is released after the provider confirms it and
// Describe reports the applied size.
func TestResizeReservation_ConfirmedOutcomeSettles(t *testing.T) {
	prov := reportingProvider(2, 4096)
	r := resizeFixture(t, prov, wantsCPU(earlierResizeSucceeded(sized("app", 2)), 4), capVM("newvm"))
	var atCall *metav1.Condition
	prov.onReconfigure = func() {
		atCall = reconfiguringCondition(getVM(t, r, "app"))
		prov.describeResp.VCPUs = 4 // applied
	}

	_, err := r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	require.Len(t, prov.reconfigureRefs, 1)
	require.NotNil(t, atCall)
	assert.Equal(t, k8s.ReasonResizeReserved, atCall.Reason, "stored pending before the call")
	got := getVM(t, r, "app")
	assert.Equal(t, k8s.ReasonReconcileSuccess, reconfiguringCondition(got).Reason, "the confirmed outcome replaces it")
	require.NotNil(t, got.Status.Placement.ResizeReservation, "released in a later pass, after Describe")

	_, err = r.reconcileVM(context.Background(), getVM(t, r, "app"))
	require.NoError(t, err)
	got = getVM(t, r, "app")
	assert.Nil(t, got.Status.Placement.ResizeReservation, "released once confirmed and described")
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU)
	assert.False(t, newVMFits(t, r), "the record counts the 4 vCPU")
}

// TestResizeReservation_ConfirmedReservationAdmitsAgain: a reservation whose
// Reconfigure was confirmed (awaiting release) does not let a later request
// within it be sent without a write. The request is admitted again and its
// reservation write records the pending outcome first, so the earlier
// confirmation cannot settle the later Reconfigure.
func TestResizeReservation_ConfirmedReservationAdmitsAgain(t *testing.T) {
	vm := earlierResizeSucceeded(sized("app", 3)) // confirmed at 3 under a 4-vCPU reservation
	vm.Status.Placement.ResizeReservation = &infravirtrigaudiov1beta1.PlacementResources{CPU: 4, MemoryMiB: 4096}
	r := resizeFixture(t, runningRoutingProvider(), wantsCPU(vm, 4), capVM("newvm"))

	admitted, err := admitApp(t, r, getVM(t, r, "app"))
	require.NoError(t, err)
	require.True(t, admitted, "4 + 4 of 8 vCPU fits")
	got := getVM(t, r, "app")
	assert.Equal(t, k8s.ReasonResizeReserved, reconfiguringCondition(got).Reason, "written before the Reconfigure")
	require.NotNil(t, got.Status.Placement.ResizeReservation)
	assert.Equal(t, int32(4), got.Status.Placement.ResizeReservation.CPU, "never lowered")

	// While the outcome is pending, a request within the reservation goes on
	// under it without another write, as before.
	admitted, err = admitApp(t, r, wantsCPU(getVM(t, r, "app"), 4))
	require.NoError(t, err)
	assert.True(t, admitted)
}

// TestResizeReservation_UnstoredWriteKeepsPriorCondition: a reservation write
// the API server refused sends nothing and leaves both the stored and the
// in-memory Reconfiguring condition as they were.
func TestResizeReservation_UnstoredWriteKeepsPriorCondition(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	refuse := interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "virtualmachines"}, "app", errors.New("denied"))
	}}
	lc := laggingClientWith(t, coverageTestScheme(t), refuse, withRuntime(clusteredProviderCR("prov-cluster", capNS)),
		hostPoolCR("pool-a", capNS, "prov-cluster"), capHost("host-alpha", 8), smallVMClass(capNS), minimalVMImage(capNS),
		sized("neighbour", 4), wantsCPU(earlierResizeSucceeded(sized("app", 2)), 4))
	r := safetyReconciler(lc, &now)

	vm := readVM(t, lc, "app")
	admitted, err := admitApp(t, r, vm)
	require.Error(t, err)
	assert.False(t, admitted)
	assert.Nil(t, vm.Status.Placement.ResizeReservation)
	assert.Equal(t, k8s.ReasonReconcileSuccess, reconfiguringCondition(vm).Reason, "in memory: restored")
	assert.Equal(t, k8s.ReasonReconcileSuccess, reconfiguringCondition(readVM(t, lc.Client, "app")).Reason, "stored: unchanged")
}

// TestResizeReservation_AmbiguousWriteStoresPendingOutcome: a reservation
// write whose answer was lost but which landed stored the pending outcome
// with the reservation, so it stays fail-closed: nothing settles it and no
// Reconfigure is sent until a later admission sees it.
func TestResizeReservation_AmbiguousWriteStoresPendingOutcome(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	objs := []client.Object{withRuntime(clusteredProviderCR("prov-cluster", capNS)), hostPoolCR("pool-a", capNS, "prov-cluster"),
		capHost("host-alpha", 8), smallVMClass(capNS), minimalVMImage(capNS), sized("neighbour", 4),
		wantsCPU(earlierResizeSucceeded(sized("app", 2)), 4)}
	lc := laggingClientWith(t, coverageTestScheme(t), ambiguousOnce("app", true), objs...)
	r := safetyReconciler(lc, &now)

	admitted, err := admitApp(t, r, readVM(t, lc, "app"))
	require.Error(t, err)
	assert.False(t, admitted)
	stored := readVM(t, lc.Client, "app")
	require.NotNil(t, stored.Status.Placement.ResizeReservation)
	assert.Equal(t, k8s.ReasonResizeReserved, reconfiguringCondition(stored).Reason)
	assert.False(t, resizeOutcomeEstablished(stored, contracts.DescribeResponse{VCPUs: 2, MaxMemoryMiB: 4096}),
		"the landed write cannot be settled by the earlier success")
}
