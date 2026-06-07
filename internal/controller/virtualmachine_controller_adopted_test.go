/*
Copyright 2025.

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

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// createCountingProvider records how many times Create is invoked. It embeds
// stubProvider (Describe returns Exists=false by default, which would normally
// trigger a create on the non-adopted path).
type createCountingProvider struct {
	stubProvider
	createCnt int
}

func (p *createCountingProvider) Create(_ context.Context, _ contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.createCnt++
	return contracts.CreateResponse{ID: "vm-newly-created"}, nil
}

// TestReconcileVM_AdoptedEmptyID_DoesNotCreate is the Part B guard test: an
// adopted VM (virtrigaud.io/adopted=true) with an empty Status.ID must NOT be
// sent to provider Create — its ID is set by the adoption/clone controller. The
// controller should instead requeue and wait (issue #179).
func TestReconcileVM_AdoptedEmptyID_DoesNotCreate(t *testing.T) {
	s := coverageTestScheme(t)
	ns := "default"
	prov, class := providerAndClass(ns)

	adoptedVM := baseVM(ns)
	adoptedVM.Name = "adopted-vm"
	adoptedVM.Labels = map[string]string{AdoptedLabel: AdoptedLabelValue}
	// Status.ID is intentionally empty.

	cp := &createCountingProvider{}
	r := newTestReconciler(s, &stubResolver{provider: cp}, prov, class, adoptedVM)

	res, err := r.reconcileVM(context.Background(), adoptedVM)
	require.NoError(t, err)

	assert.Equal(t, 0, cp.createCnt, "adopted VM with empty Status.ID must NOT be created")
	assert.True(t, res.RequeueAfter > 0, "expected a requeue while waiting for Status.ID to be set")
}

// TestReconcileVM_NonAdoptedEmptyID_DoesCreate is the negative control: a NORMAL
// (non-adopted) VM with an empty Status.ID still follows the create path, so the
// guard does not regress normal VMs.
func TestReconcileVM_NonAdoptedEmptyID_DoesCreate(t *testing.T) {
	s := coverageTestScheme(t)
	ns := "default"
	prov, class := providerAndClass(ns)

	normalVM := baseVM(ns)
	normalVM.Name = "normal-vm"
	// An ImportedDisk so createVM's "imageRef or importedDisk" validation passes.
	normalVM.Spec.ImportedDisk = &infravirtrigaudiov1beta1.ImportedDiskRef{
		DiskID: "disk-1",
		Format: "qcow2",
		Source: "manual",
	}
	// No adopted label, empty Status.ID.

	cp := &createCountingProvider{}
	r := newTestReconciler(s, &stubResolver{provider: cp}, prov, class, normalVM)

	_, err := r.reconcileVM(context.Background(), normalVM)
	require.NoError(t, err)

	assert.Equal(t, 1, cp.createCnt, "normal VM with empty Status.ID must be created (no regression)")
}

// TestVMIsAdopted unit-tests the pure guard helper.
func TestVMIsAdopted(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"nil labels", nil, false},
		{"no adopted label", map[string]string{"app": "x"}, false},
		{"adopted true", map[string]string{AdoptedLabel: AdoptedLabelValue}, true},
		{"adopted false", map[string]string{AdoptedLabel: "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Labels: tc.labels},
			}
			assert.Equal(t, tc.want, vmIsAdopted(vm))
		})
	}
}
