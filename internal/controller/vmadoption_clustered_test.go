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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestVMAdoption_ClusteredProviderRefused pins that adoption is refused on a
// clustered provider (ADR-0007 Addendum A, A1/A3): an adopted VM would have no
// host binding and every per-VM call for it would be refused as unbound. The
// refusal happens before any provider call (no RemoteResolver is wired, so a
// call would fail differently) and creates no VirtualMachine.
func TestVMAdoption_ClusteredProviderRefused(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	prov := readyProvider("default", "prov-c")
	prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	prov.Annotations = map[string]string{AdoptionAnnotation: "true"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(prov).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).Build()
	r := &VMAdoptionReconciler{Client: c, Scheme: s}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "prov-c"}})
	require.NoError(t, err)

	got := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "prov-c"}, got))
	require.NotNil(t, got.Status.Adoption)
	assert.Equal(t, clusteredAdoptionUnsupportedMessage, got.Status.Adoption.Message)
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, c.List(ctx, &vms))
	assert.Empty(t, vms.Items)
}
