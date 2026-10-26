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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// statusWriteVM is a bound, running VM as a steady-state reconcile leaves it.
func statusWriteVM() *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Generation: 1},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "p"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
		},
	}
	vm.Status.ID = "default.web"
	vm.Status.PowerState = infravirtrigaudiov1beta1.ObservedPowerStateOn
	vm.Status.IPs = []string{"192.0.2.10"}
	setVMReady(vm)
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM provisioned", vm.Generation)
	applyDerivedPhase(vm)
	return vm
}

// countingStatusReconciler returns a reconciler over a fake client holding vm
// and a pointer to the number of status Update calls it receives.
func countingStatusReconciler(t *testing.T, vm *infravirtrigaudiov1beta1.VirtualMachine) (*VirtualMachineReconciler, *int) {
	t.Helper()
	writes := 0
	fc := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(vm).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				writes++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	return &VirtualMachineReconciler{Client: fc, Scheme: coverageTestScheme(t)}, &writes
}

// readBack returns the stored copy of vm.
func readBack(t *testing.T, r *VirtualMachineReconciler, vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	got := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vm), got))
	return got
}

// TestUpdateStatus_UnchangedStatusIsNotWritten pins F2: recomputing the same
// status at the same resourceVersion sends no API write.
func TestUpdateStatus_UnchangedStatusIsNotWritten(t *testing.T) {
	r, writes := countingStatusReconciler(t, statusWriteVM())
	vm := readBack(t, r, statusWriteVM())

	// A steady-state poll recomputes the same conditions and fields.
	setVMReady(vm)
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM provisioned", vm.Generation)
	rv := vm.ResourceVersion
	r.updateStatus(context.Background(), vm)

	assert.Equal(t, 0, *writes, "an unchanged status must not be written")
	assert.Equal(t, rv, readBack(t, r, vm).ResourceVersion)
}

// TestUpdateStatus_ChangedStatusIsWritten pins that every meaningful change
// is still persisted: a new IP, a condition change and a placement record.
func TestUpdateStatus_ChangedStatusIsWritten(t *testing.T) {
	for name, change := range map[string]func(vm *infravirtrigaudiov1beta1.VirtualMachine){
		"ips": func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.IPs = []string{"192.0.2.11"} },
		"condition": func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, "ProviderError", "boom", vm.Generation)
		},
		"pendingHost": func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-a"}
		},
		"observedGeneration": func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Status.ObservedGeneration = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			r, writes := countingStatusReconciler(t, statusWriteVM())
			vm := readBack(t, r, statusWriteVM())
			change(vm)
			r.updateStatus(context.Background(), vm)

			assert.Equal(t, 1, *writes)
			got := readBack(t, r, vm)
			assert.Equal(t, vm.Status.IPs, got.Status.IPs)
			assert.Equal(t, vm.Status.Placement, got.Status.Placement)
			assert.Equal(t, vm.Status.ObservedGeneration, got.Status.ObservedGeneration)
			assert.Equal(t, vm.Status.Phase, got.Status.Phase)
		})
	}
}

// TestUpdateStatus_StaleObjectStillWrites pins that a VM held at an older
// resourceVersion is never skipped on equality: the write is attempted, and
// its conflict surfaces exactly as before (and is not applied).
func TestUpdateStatus_StaleObjectStillWrites(t *testing.T) {
	r, writes := countingStatusReconciler(t, statusWriteVM())
	stale := readBack(t, r, statusWriteVM())

	// Someone else changes the VM's status: the stored version moves on.
	other := readBack(t, r, stale)
	other.Status.IPs = []string{"192.0.2.99"}
	require.NoError(t, r.Status().Update(context.Background(), other))
	*writes = 0

	// The stale copy recomputes a status equal to what it read.
	r.updateStatus(context.Background(), stale)

	assert.Equal(t, 1, *writes, "a stale copy must attempt its write, not skip on equality")
	assert.Equal(t, []string{"192.0.2.99"}, readBack(t, r, stale).Status.IPs, "the conflicting write must not apply")
}

// TestUpdateStatus_NoResourceVersionWrites pins that an object without a
// resourceVersion (never read from the API) is always written.
func TestUpdateStatus_NoResourceVersionWrites(t *testing.T) {
	r, writes := countingStatusReconciler(t, statusWriteVM())
	vm := readBack(t, r, statusWriteVM())
	vm.ResourceVersion = ""
	r.updateStatus(context.Background(), vm)
	assert.Equal(t, 1, *writes)
}
