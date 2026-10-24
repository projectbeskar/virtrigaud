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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin V4: spec.disks on a Provider that does not attach them
// (libvirt, Proxmox) is reported on DisksApplied=False/NotSupportedByProvider
// and one Warning event per generation; nothing is refused and Ready is not
// changed by it.

// disksWarnings returns the NotSupportedByProvider Warning events in rec.
func disksWarnings(rec *record.FakeRecorder) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+k8s.ReasonNotSupportedByProvider) {
			n++
		}
	}
	return n
}

func TestNoteUnattachedDisks(t *testing.T) {
	libvirt := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeLibvirt}}
	proxmox := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeProxmox}}
	vsphere := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeVSphere}}
	rec := record.NewFakeRecorder(16)
	r := &VirtualMachineReconciler{Recorder: rec}
	vm := baseVM("default")
	vm.Generation = 1
	vm.Spec.Disks = []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 1}}

	cond := func() *metav1.Condition { return meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionDisksApplied) }

	r.noteUnattachedDisks(vm, libvirt)
	require.NotNil(t, cond())
	assert.Equal(t, metav1.ConditionFalse, cond().Status)
	assert.Equal(t, k8s.ReasonNotSupportedByProvider, cond().Reason)
	assert.Contains(t, cond().Message, "type libvirt")
	assert.Contains(t, cond().Message, "1 disk(s)")
	assert.Equal(t, int64(1), cond().ObservedGeneration)
	assert.Equal(t, 1, disksWarnings(rec), "warned when first set")

	r.noteUnattachedDisks(vm, libvirt)
	r.noteUnattachedDisks(vm, libvirt)
	assert.Equal(t, 0, disksWarnings(rec), "not per reconcile")

	vm.Generation = 2 // a spec change
	vm.Spec.Disks = append(vm.Spec.Disks, infravirtrigaudiov1beta1.DiskSpec{Name: "logs", SizeGiB: 2})
	r.noteUnattachedDisks(vm, libvirt)
	assert.Contains(t, cond().Message, "2 disk(s)")
	assert.Equal(t, int64(2), cond().ObservedGeneration)
	assert.Equal(t, 1, disksWarnings(rec), "once per generation")

	r.noteUnattachedDisks(vm, proxmox)
	assert.Contains(t, cond().Message, "type proxmox")

	r.noteUnattachedDisks(vm, vsphere)
	assert.Nil(t, cond(), "vSphere attaches spec.disks: no condition")

	r.noteUnattachedDisks(vm, libvirt)
	require.NotNil(t, cond())
	vm.Generation = 3
	vm.Spec.Disks = nil
	r.noteUnattachedDisks(vm, libvirt)
	assert.Nil(t, cond(), "cleared when spec.disks is emptied")
	assert.Equal(t, 1, disksWarnings(rec), "the re-set above warned once; removal does not warn")
}

// TestReconcileVM_DisksAppliedAtCreateAndOnARunningVM drives reconcileVM: the
// condition is written with the status of a VM being created and of a VM that
// exists, and Ready is still set by the VM's own state.
func TestReconcileVM_DisksAppliedAtCreateAndOnARunningVM(t *testing.T) {
	for name, existing := range map[string]bool{"at create": false, "on a running VM": true} {
		t.Run(name, func(t *testing.T) {
			s := coverageTestScheme(t)
			k8sProv, class := providerAndClass("default")
			k8sProv.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
			prov := &fakeDescribeProvider{DescribeFn: func(_ context.Context, _ string) (contracts.DescribeResponse, error) {
				return contracts.DescribeResponse{Exists: true, PowerState: "On"}, nil
			}}
			vm := baseVM("default")
			vm.Spec.Disks = []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 1}}
			vm.Spec.ImageRef = &infravirtrigaudiov1beta1.ObjectRef{Name: "test-image"}
			if existing {
				vm.Status.ID = "vm-abc"
			}
			r := newTestReconciler(s, &stubResolver{provider: prov}, k8sProv, class, minimalVMImage("default"), vm)
			rec := record.NewFakeRecorder(32)
			r.Recorder = rec

			stored := &infravirtrigaudiov1beta1.VirtualMachine{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vm), stored))
			_, err := r.reconcileVM(context.Background(), stored)
			require.NoError(t, err)

			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vm), got))
			c := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionDisksApplied)
			require.NotNil(t, c, "the condition is persisted with the reconcile's status write")
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, k8s.ReasonNotSupportedByProvider, c.Reason)
			if existing {
				ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
				require.NotNil(t, ready)
				assert.Equal(t, metav1.ConditionTrue, ready.Status, "the VM itself is fine: Ready is not blocked")
			}
			assert.Equal(t, 1, disksWarnings(rec))

			// The next reconcile of the same generation warns no more.
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vm), stored))
			_, err = r.reconcileVM(context.Background(), stored)
			require.NoError(t, err)
			assert.Equal(t, 0, disksWarnings(rec))
		})
	}
}
