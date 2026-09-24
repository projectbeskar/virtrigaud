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

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMClone controller's side of ADR-0007 Addendum A, A1:
// the clone source is addressed through vmRefFor (routed to its bound host on a
// clustered provider, never sent without one), and the cloned target VM's
// binding is written in the same status write as its Status.ID.

func clusteredCloneFixture(t *testing.T, src *infrav1beta1.VirtualMachine, cp *clonerProvider) (*VMCloneReconciler, *infrav1beta1.VMClone) {
	t.Helper()
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
	return newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, prov, src, clone), clone
}

func TestVMClone_Clustered_SourceRoutedAndTargetBoundInSameWrite(t *testing.T) {
	src := sourceVMWithID("default", "src-c", "prov-c", "src-c")
	src.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "clone-c-target"}}
	r, clone := clusteredCloneFixture(t, src, cp)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.Equal(t, 1, cp.cloneCnt)
	assert.Equal(t, contracts.VMRef{ID: "src-c", HostID: "host-alpha"}, cp.lastClone.Source,
		"the clone is routed to the source VM's bound host")

	target := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-c-target"}, target))
	assert.Equal(t, "clone-c-target", target.Status.ID)
	require.NotNil(t, target.Status.Placement, "the target's binding lands in the same write as Status.ID")
	assert.Equal(t, "host-alpha", target.Status.Placement.Host)
	assert.Equal(t, "pool-a", target.Status.Placement.Pool)
	assert.Empty(t, target.Status.Placement.PendingHost)
}

func TestVMClone_Clustered_UnboundSourceNeverCallsProvider(t *testing.T) {
	src := sourceVMWithID("default", "src-u", "prov-c", "src-u") // id, but no binding
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	r, clone := clusteredCloneFixture(t, src, cp)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt, "an unbound clustered source is never sent a per-VM call")
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "no confirmed host binding")
}
