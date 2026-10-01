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

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that VMClone spec.options.powerOn — and VMMigration
// spec.target.powerOn — decide the produced VirtualMachine's power state.
// Both default to false (the documented default), so a clone or migration
// that omits them leaves its VM powered off; before, the field was never read
// and the produced VM (with an empty spec.powerState, which the VirtualMachine
// controller treats as On) was always powered on.

// powerOnCases are the spec.options of a clone and the desired power state
// its target VM must get.
var powerOnCases = map[string]struct {
	options *infrav1beta1.CloneOptions
	want    infrav1beta1.PowerState
}{
	"options omitted":   {options: nil, want: infrav1beta1.PowerStateOff},
	"powerOn omitted":   {options: &infrav1beta1.CloneOptions{Type: infrav1beta1.CloneTypeFullClone}, want: infrav1beta1.PowerStateOff},
	"powerOn: false":    {options: &infrav1beta1.CloneOptions{PowerOn: false}, want: infrav1beta1.PowerStateOff},
	"powerOn: true":     {options: &infrav1beta1.CloneOptions{PowerOn: true}, want: infrav1beta1.PowerStateOn},
	"powerOn: true, FC": {options: &infrav1beta1.CloneOptions{Type: infrav1beta1.CloneTypeFullClone, PowerOn: true}, want: infrav1beta1.PowerStateOn},
}

func TestVMClone_SingleHost_PowerOnSetsTheTargetsPowerState(t *testing.T) {
	for name, tc := range powerOnCases {
		t.Run(name, func(t *testing.T) {
			clone := &infrav1beta1.VMClone{
				ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default"},
				Spec: infrav1beta1.VMCloneSpec{
					Source:  infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
					Target:  infrav1beta1.VMCloneTarget{Name: "clone-target"},
					Options: tc.options,
				},
			}
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-target"}}
			r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, runningProvider("default", "prov-1"),
				sourceVMWithID("default", "src-vm", "prov-1", "default.src-vm"), clone)
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

			require.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
			target := &infrav1beta1.VirtualMachine{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-target"}, target))
			assert.Equal(t, tc.want, target.Spec.PowerState, "spec.options.powerOn decides the target's power state")
		})
	}
}

func TestVMClone_Clustered_PowerOnSetsTheTargetsPowerState(t *testing.T) {
	for name, tc := range powerOnCases {
		t.Run(name, func(t *testing.T) {
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
			r, clone := clusteredCloneFixture(t, boundSource(), cp)
			stored := getClone(t, r, clone)
			stored.Spec.Options = tc.options
			require.NoError(t, r.Update(context.Background(), stored))

			// The clustered target is created before the Clone RPC: it already
			// carries the power state when the clone is sent.
			var atClone *infrav1beta1.VirtualMachine
			cp.onClone = func() { atClone = getTarget(t, r) }
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

			require.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
			require.NotNil(t, atClone)
			assert.Equal(t, tc.want, atClone.Spec.PowerState)
			assert.Equal(t, tc.want, getTarget(t, r).Spec.PowerState)
		})
	}
}

// TestVMClone_TargetPowerStateIsHonouredWithoutAFlap: every provider leaves a
// clone powered off. The VirtualMachine controller then leaves a powerOn:
// false target off — no power call at all, and Ready — and powers a powerOn:
// true target on exactly once.
func TestVMClone_TargetPowerStateIsHonouredWithoutAFlap(t *testing.T) {
	for name, tc := range map[string]struct {
		powerOn bool
		wantOps []contracts.PowerOp
	}{
		"powerOn: false": {powerOn: false, wantOps: nil},
		"powerOn: true":  {powerOn: true, wantOps: []contracts.PowerOp{contracts.PowerOpOn}},
	} {
		t.Run(name, func(t *testing.T) {
			clone := &infrav1beta1.VMClone{Spec: infrav1beta1.VMCloneSpec{
				Target:  infrav1beta1.VMCloneTarget{Name: "test-vm"},
				Options: &infrav1beta1.CloneOptions{PowerOn: tc.powerOn},
			}}
			// The bound target VM as the VirtualMachine controller sees it: the
			// cloned VM is reported powered off, as every provider leaves it.
			prov := &powerRecordingProvider{fakeDescribeProvider: fakeDescribeProvider{
				DescribeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
					return contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}, nil
				},
			}}
			k8sProv, class := providerAndClass("default")
			r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, k8sProv, class)
			vm := baseVM("default")
			vm.Spec.PowerState = cloneTargetPowerState(clone)
			vm.Status.ID = "default.test-vm"
			cpu, mem := int32(4), int64(8192) // the class's size: nothing to reconfigure
			vm.Status.CurrentResources = &infrav1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}

			_, err := r.reconcileVM(context.Background(), vm)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOps, prov.powerOps)
			assert.Zero(t, prov.reconfigures)
			if !tc.powerOn {
				ready := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
				require.NotNil(t, ready)
				assert.Equal(t, metav1.ConditionTrue, ready.Status, "a VM that is off as asked is Ready")
			}
		})
	}
}

// TestCreatingPhase_PowerOnSetsTheTargetsPowerState: a migration's target VM
// gets spec.powerState from spec.target.powerOn — Off unless it is true.
func TestCreatingPhase_PowerOnSetsTheTargetsPowerState(t *testing.T) {
	for name, tc := range map[string]struct {
		powerOn bool
		want    infrav1beta1.PowerState
	}{
		"powerOn: false (default)": {powerOn: false, want: infrav1beta1.PowerStateOff},
		"powerOn: true":            {powerOn: true, want: infrav1beta1.PowerStateOn},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, sourceProvider, targetProvider, migration := directionFixture(
				infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
			migration.Spec.Target.PowerOn = tc.powerOn
			migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
			migration.Status.ImportID = "target-vm-migrated"
			migration.Status.DiskInfo.TargetDiskID = "target-vm-migrated"
			migration.Status.DiskInfo.TargetFormat = "qcow2"
			r, c := directionReconciler(t, &capturingMigrationProvider{}, sourceProvider, targetProvider, migration)

			_, err := r.handleCreatingPhase(ctx, migration)
			require.NoError(t, err)

			created := &infrav1beta1.VirtualMachine{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "target-vm"}, created))
			assert.Equal(t, tc.want, created.Spec.PowerState, "spec.target.powerOn decides the target's power state")
		})
	}
}
