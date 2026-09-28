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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// powerRecordingProvider is a fakeDescribeProvider that records Power and
// Reconfigure calls.
type powerRecordingProvider struct {
	fakeDescribeProvider
	powerOps     []contracts.PowerOp
	reconfigures int
}

func (p *powerRecordingProvider) Power(_ context.Context, _ contracts.VMRef, op contracts.PowerOp) (string, error) {
	p.powerOps = append(p.powerOps, op)
	return "", nil
}

func (p *powerRecordingProvider) Reconfigure(_ context.Context, _ contracts.VMRef, _ contracts.CreateRequest) (contracts.ReconfigureResult, error) {
	p.reconfigures++
	return contracts.ReconfigureResult{}, nil
}

func TestObservedPowerState(t *testing.T) {
	cases := map[string]infravirtrigaudiov1beta1.ObservedPowerState{
		"On":          "On",
		"Off":         "Off",
		"Suspended":   infravirtrigaudiov1beta1.ObservedPowerStateSuspended,
		"Unknown":     infravirtrigaudiov1beta1.ObservedPowerStateUnknown,
		"":            "",
		"poweredOn":   infravirtrigaudiov1beta1.ObservedPowerStateUnknown,
		"hibernating": infravirtrigaudiov1beta1.ObservedPowerStateUnknown,
	}
	for reported, want := range cases {
		assert.Equal(t, want, observedPowerState(reported), "reported %q", reported)
	}
	assert.Equal(t, infravirtrigaudiov1beta1.PowerStateOff, adoptedDesiredPowerState("Off"))
	for _, reported := range []string{"On", "Suspended", "Unknown", ""} {
		assert.Equal(t, infravirtrigaudiov1beta1.PowerStateOn, adoptedDesiredPowerState(reported),
			"a VM that is not powered off is adopted On, never Off (%q)", reported)
	}
}

// TestReconcileVM_SuspendedOrUnknown_NeitherPoweredNorReconfigured (review
// R1): a VM the provider reports Suspended or Unknown is recorded as such,
// marked Ready=False/PowerStateUnmanaged, and neither powered on nor
// reconfigured — even when its spec asks for a different size. An Unknown VM
// is not powered off either (the Suspended + spec Off case is
// TestReconcileVM_SuspendedWithSpecOff_IsPoweredOff).
func TestReconcileVM_SuspendedOrUnknown_NeitherPoweredNorReconfigured(t *testing.T) {
	for _, reported := range []string{"Suspended", "Unknown"} {
		for _, desired := range []infravirtrigaudiov1beta1.PowerState{infravirtrigaudiov1beta1.PowerStateOn, infravirtrigaudiov1beta1.PowerStateOff} {
			if reported == "Suspended" && desired == infravirtrigaudiov1beta1.PowerStateOff {
				continue
			}
			t.Run(reported+"/desired-"+string(desired), func(t *testing.T) {
				prov := &powerRecordingProvider{fakeDescribeProvider: fakeDescribeProvider{
					DescribeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
						return contracts.DescribeResponse{Exists: true, PowerState: reported}, nil
					},
				}}
				k8sProv, class := providerAndClass("default")
				r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, k8sProv, class)
				vm := baseVM("default")
				vm.Spec.PowerState = desired
				vm.Status.ID = "vm-1"
				// Recorded smaller than the class: a reconfigure would be due.
				cpu, mem := int32(2), int64(4096)
				vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}

				_, err := r.reconcileVM(context.Background(), vm)
				require.NoError(t, err)

				assert.Empty(t, prov.powerOps, "a suspended or unknown VM is never powered on or off")
				assert.Zero(t, prov.reconfigures, "nor reconfigured")
				assert.Equal(t, infravirtrigaudiov1beta1.ObservedPowerState(reported), vm.Status.PowerState)
				ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
				require.NotNil(t, ready)
				assert.Equal(t, metav1.ConditionFalse, ready.Status)
				assert.Equal(t, k8s.ReasonPowerStateUnmanaged, ready.Reason)
				assert.Equal(t, int32(2), *vm.Status.CurrentResources.CPU, "the recorded size is untouched")
			})
		}
	}
}

// TestReconcileVM_SuspendedWithSpecOff_IsPoweredOff (review H4): a Suspended
// VM whose spec asks for Off (or OffGraceful — a suspended guest cannot shut
// down gracefully) is powered off (destroy); it is still not reconfigured.
func TestReconcileVM_SuspendedWithSpecOff_IsPoweredOff(t *testing.T) {
	for _, desired := range []infravirtrigaudiov1beta1.PowerState{infravirtrigaudiov1beta1.PowerStateOff, infravirtrigaudiov1beta1.PowerStateOffGraceful} {
		t.Run(string(desired), func(t *testing.T) {
			prov := &powerRecordingProvider{fakeDescribeProvider: fakeDescribeProvider{
				DescribeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
					return contracts.DescribeResponse{Exists: true, PowerState: "Suspended"}, nil
				},
			}}
			k8sProv, class := providerAndClass("default")
			r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, k8sProv, class)
			vm := baseVM("default")
			vm.Spec.PowerState = desired
			vm.Status.ID = "vm-1"
			cpu, mem := int32(2), int64(4096)
			vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}

			_, err := r.reconcileVM(context.Background(), vm)
			require.NoError(t, err)
			assert.Equal(t, []contracts.PowerOp{contracts.PowerOpOff}, prov.powerOps, "powered off (destroy), as the spec asks")
			assert.Zero(t, prov.reconfigures)
		})
	}
}
