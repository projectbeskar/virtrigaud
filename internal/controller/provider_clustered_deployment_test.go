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
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestReconcileDeployment_ClusteredProviderRunsOneReplicaWithRecreate: a
// clustered Provider's Deployment always has one replica and the Recreate
// strategy (ADR-0007 Addendum A, slice 4: its owner transfers and host guards
// are serialized inside one process), also when it existed with a rolling
// update; a single-host Provider keeps its replicas and the default strategy.
func TestReconcileDeployment_ClusteredProviderRunsOneReplicaWithRecreate(t *testing.T) {
	ctx := context.Background()
	three := int32(3)

	clustered := providerWithRuntime("clustered", nil)
	clustered.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	clustered.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	clustered.Spec.Runtime.Replicas = &three
	maxSurge := intstr.FromInt32(1)
	existing := &appsv1.Deployment{}
	existing.Name, existing.Namespace = "dep-clustered", "default"
	existing.Spec.Replicas = &three
	existing.Spec.Strategy = appsv1.DeploymentStrategy{
		Type:          appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &maxSurge},
	}

	single := providerWithRuntime("single", nil)
	single.Spec.Runtime.Replicas = &three

	sch := newProviderTLSScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(clustered, single, existing).Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch}

	_, err := r.reconcileDeployment(ctx, clustered, "dep-clustered")
	require.NoError(t, err)
	got := &appsv1.Deployment{}
	require.NoError(t, cli.Get(ctx, types.NamespacedName{Namespace: "default", Name: "dep-clustered"}, got))
	assert.EqualValues(t, 1, *got.Spec.Replicas)
	assert.Equal(t, appsv1.RecreateDeploymentStrategyType, got.Spec.Strategy.Type)
	assert.Nil(t, got.Spec.Strategy.RollingUpdate, "Recreate carries no rolling-update parameters")

	_, err = r.reconcileDeployment(ctx, single, "dep-single")
	require.NoError(t, err)
	got = &appsv1.Deployment{}
	require.NoError(t, cli.Get(ctx, types.NamespacedName{Namespace: "default", Name: "dep-single"}, got))
	assert.EqualValues(t, 3, *got.Spec.Replicas, "single-host is unchanged")
	assert.Empty(t, got.Spec.Strategy.Type, "single-host keeps the default strategy")
}
