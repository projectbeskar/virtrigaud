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

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestVMClone_Clustered_ProviderMustRouteClonesBeforeAnythingIsCreated: a
// clustered provider that does not route clones (older than ADR-0007
// Addendum A slice 3), or does not report capabilities, fails the clone before
// its target VM is created; a capability query that fails is retried, still
// without creating anything.
func TestVMClone_Clustered_ProviderMustRouteClonesBeforeAnythingIsCreated(t *testing.T) {
	for name, tc := range map[string]struct {
		prep  func(cp *clonerProvider)
		phase infrav1beta1.ClonePhase
	}{
		"older clustered provider": {func(cp *clonerProvider) { cp.caps.SupportsRoutedClone = false }, infrav1beta1.ClonePhaseFailed},
		"capability query fails":   {func(cp *clonerProvider) { cp.capsErr = errors.New("unavailable") }, infrav1beta1.ClonePhasePending},
	} {
		t.Run(name, func(t *testing.T) {
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
			r, clone := clusteredCloneFixture(t, boundSource(), cp)
			tc.prep(cp)
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

			assert.Zero(t, cp.cloneCnt)
			assert.Equal(t, tc.phase, getClone(t, r, clone).Status.Phase)
			assert.Error(t, r.Get(context.Background(), targetKey, &infrav1beta1.VirtualMachine{}), "no target VM is created")
		})
	}
	t.Run("provider does not report capabilities", func(t *testing.T) {
		src := boundSource()
		prov := runningProvider("default", "prov-c")
		prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
		prov.Spec.Topology = infrav1beta1.ProviderTopologyCluster
		clone := &infrav1beta1.VMClone{
			ObjectMeta: metav1.ObjectMeta{Name: "clone-c", Namespace: "default"},
			Spec: infrav1beta1.VMCloneSpec{
				Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: src.Name}},
				Target: infrav1beta1.VMCloneTarget{Name: "clone-c-target"},
			},
		}
		cp := &clonerProviderNoCaps{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
		r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, prov, src, clone, readyCloneHost("host-alpha", "prov-c"))
		reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

		assert.Zero(t, cp.cloneCnt)
		assert.Equal(t, infrav1beta1.ClonePhaseFailed, getClone(t, r, clone).Status.Phase)
		assert.Error(t, r.Get(context.Background(), targetKey, &infrav1beta1.VirtualMachine{}), "no target VM is created")
	})
}
