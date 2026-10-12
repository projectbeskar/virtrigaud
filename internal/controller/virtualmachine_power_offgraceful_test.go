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
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A VM whose spec.powerState is OffGraceful and which its provider reports Off
// is where it should be: it is not sent a graceful shutdown on every
// reconcile (a vSphere InvalidPowerState fault and Ready=False/ProviderError
// every few seconds), and it becomes Ready.

// offGracefulProvider reports describe and records every Power operation.
type offGracefulProvider struct {
	stubProvider
	describe contracts.DescribeResponse
	ops      []contracts.PowerOp
}

func (p *offGracefulProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	return p.describe, nil
}

func (p *offGracefulProvider) Power(_ context.Context, _ contracts.VMRef, op contracts.PowerOp) (string, error) {
	p.ops = append(p.ops, op)
	return "", nil
}

// reconcilePower reconciles a bound single-host VM of a Provider of type
// providerType with spec.powerState desired, which the provider reports in
// power state reported; it returns the Power operations sent and the VM as
// stored afterwards.
func reconcilePower(t *testing.T, providerType infravirtrigaudiov1beta1.ProviderType, desired infravirtrigaudiov1beta1.PowerState,
	reported string) ([]contracts.PowerOp, *infravirtrigaudiov1beta1.VirtualMachine) {
	t.Helper()
	ctx := context.Background()
	prov := &offGracefulProvider{describe: contracts.DescribeResponse{Exists: true, PowerState: reported, IPs: []string{"10.0.0.5"}}}
	r, vm := boundSingleHostVM(t, &stubResolver{provider: prov})
	providerCR := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-prov"}, providerCR))
	providerCR.Spec.Type = providerType
	require.NoError(t, r.Update(ctx, providerCR))
	vm.Spec.PowerState = desired
	require.NoError(t, r.Update(ctx, vm))

	_, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	got := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), got))
	return prov.ops, got
}

func TestReconcileVM_OffGracefulObservedOffIsSatisfied(t *testing.T) {
	// Every provider reports a powered-off VM as "Off": vSphere poweredOff,
	// libvirt shut off, Proxmox stopped; the mock provider (no Provider type of
	// its own) reports "Off" too.
	for _, pt := range []infravirtrigaudiov1beta1.ProviderType{
		infravirtrigaudiov1beta1.ProviderTypeVSphere,
		infravirtrigaudiov1beta1.ProviderTypeLibvirt,
		infravirtrigaudiov1beta1.ProviderTypeProxmox,
	} {
		t.Run(string(pt), func(t *testing.T) {
			ops, got := reconcilePower(t, pt, infravirtrigaudiov1beta1.PowerStateOffGraceful, string(contracts.PowerStateOff))
			assert.Empty(t, ops, "a VM already off is not sent a graceful shutdown")
			ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionTrue, ready.Status, "and it is Ready")
			assert.Equal(t, infravirtrigaudiov1beta1.ObservedPowerStateOff, got.Status.PowerState)
		})
	}
}

func TestReconcileVM_PowerStateDecisions(t *testing.T) {
	cases := []struct {
		name     string
		desired  infravirtrigaudiov1beta1.PowerState
		reported contracts.PowerState
		ops      []contracts.PowerOp
	}{
		{name: "OffGraceful, running: shut down gracefully", desired: infravirtrigaudiov1beta1.PowerStateOffGraceful,
			reported: contracts.PowerStateOn, ops: []contracts.PowerOp{contracts.PowerOpShutdownGraceful}},
		{name: "Off, off: nothing", desired: infravirtrigaudiov1beta1.PowerStateOff, reported: contracts.PowerStateOff},
		{name: "On, off: power on", desired: infravirtrigaudiov1beta1.PowerStateOn, reported: contracts.PowerStateOff,
			ops: []contracts.PowerOp{contracts.PowerOpOn}},
		{name: "On, running: nothing", desired: infravirtrigaudiov1beta1.PowerStateOn, reported: contracts.PowerStateOn},
		{name: "Off, running: power off", desired: infravirtrigaudiov1beta1.PowerStateOff, reported: contracts.PowerStateOn,
			ops: []contracts.PowerOp{contracts.PowerOpOff}},
		// R1 / H4 / L3: a suspended VM is powered off (hard) only for Off;
		// OffGraceful is held, never turned into a hard power-off.
		{name: "OffGraceful, suspended: held (L3)", desired: infravirtrigaudiov1beta1.PowerStateOffGraceful,
			reported: contracts.PowerStateSuspended},
		{name: "Off, suspended: hard power-off (H4)", desired: infravirtrigaudiov1beta1.PowerStateOff,
			reported: contracts.PowerStateSuspended, ops: []contracts.PowerOp{contracts.PowerOpOff}},
		{name: "OffGraceful, unknown: left alone (R1)", desired: infravirtrigaudiov1beta1.PowerStateOffGraceful,
			reported: contracts.PowerStateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, _ := reconcilePower(t, infravirtrigaudiov1beta1.ProviderTypeLibvirt, tc.desired, string(tc.reported))
			assert.Equal(t, tc.ops, ops)
		})
	}
}

func TestPowerStateSatisfied(t *testing.T) {
	assert.True(t, powerStateSatisfied(infravirtrigaudiov1beta1.PowerStateOffGraceful, "Off"))
	assert.True(t, powerStateSatisfied(infravirtrigaudiov1beta1.PowerStateOff, "Off"))
	assert.True(t, powerStateSatisfied(infravirtrigaudiov1beta1.PowerStateOn, "On"))
	assert.False(t, powerStateSatisfied(infravirtrigaudiov1beta1.PowerStateOffGraceful, "On"))
	assert.False(t, powerStateSatisfied(infravirtrigaudiov1beta1.PowerStateOffGraceful, "Suspended"))
	assert.False(t, powerStateSatisfied(infravirtrigaudiov1beta1.PowerStateOff, "Suspended"))
	assert.False(t, powerStateSatisfied(infravirtrigaudiov1beta1.PowerStateOn, "Off"))
}
