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
	"k8s.io/apimachinery/pkg/types"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Adoption creates VirtualMachines (and their VMClass) in the Provider's own
// namespace, so it never needs a consumer grant; it only refuses to bind a
// pre-existing adopted-labelled VirtualMachine that references an ungranted
// cross-namespace VMClass or VMImage.

func TestAdoptVM_ExistingAdoptedVMWithUngrantedCrossNamespaceRefsIsNotBound(t *testing.T) {
	ctx := context.Background()
	prov := singleProviderCR("prov-a", "infra")
	prov.UID = "uid-a"
	adopted := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-db", Namespace: "infra",
			Labels: map[string]string{AdoptedLabel: AdoptedLabelValue}},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "prov-a"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "c", Namespace: "platform"},
		},
	}
	for name, tc := range map[string]struct {
		sel   *metav1.LabelSelector
		bound bool
	}{
		"class not shared: not bound": {nil, false},
		"class shared: bound":         {&metav1.LabelSelector{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newBPAdoptionReconciler(t, prov, adopted.DeepCopy(), grantedClass("platform", "c", tc.sel))
			require.NoError(t, r.adoptVM(ctx, prov, contracts.VMInfo{ID: "101", Name: "legacy-db"}))
			vm := &infrav1beta1.VirtualMachine{}
			require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "legacy-db"}, vm))
			if tc.bound {
				assert.Equal(t, "101", vm.Status.ID)
				return
			}
			assert.Empty(t, vm.Status.ID, "adoption never binds a VM the VirtualMachine controller would refuse")
			assert.Nil(t, vm.Status.BoundProvider)
		})
	}

	t.Run("a newly adopted VM references only the Provider's own namespace", func(t *testing.T) {
		r := newBPAdoptionReconciler(t, prov)
		require.NoError(t, r.adoptVM(ctx, prov, contracts.VMInfo{ID: "102", Name: "new-db", CPU: 2, MemoryMiB: 2048}))
		vm := &infrav1beta1.VirtualMachine{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "new-db"}, vm))
		assert.False(t, vmHasCrossNamespaceRef(vm), "adoption creates VMs, and their class, in the Provider's namespace")
		require.NoError(t, checkVMConsumerRefs(ctx, r.Client, vm))
		assert.Equal(t, "102", vm.Status.ID)
	})
}
