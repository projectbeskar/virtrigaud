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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestReconcile_CapabilityReadRecordedOnlyWithItsStatusWrite pins that a
// capability read whose Provider status write fails is not recorded: the next
// reconcile reads the capabilities again and writes them, instead of leaving
// them off the status until the next settle or 10-minute read.
func TestReconcile_CapabilityReadRecordedOnlyWithItsStatusWrite(t *testing.T) {
	ctx := context.Background()
	sch := newProviderSAScheme(t)
	var failStatusWrite atomic.Bool
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inUseProvider()).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*infravirtrigaudiov1beta1.Provider); ok && failStatusWrite.Load() {
					return errors.New("injected status write failure")
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()
	reader := &countingCapabilityReader{}
	reader.answer(contracts.Capabilities{SupportsSnapshots: true, SupportsHonestReconfigure: true})
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: record.NewFakeRecorder(64), CapabilityReader: reader}
	reconcile := func() error {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: inUseProviderKey})
		return err
	}
	readKey := capabilityReadKey(inUseProvider())

	// Create the runtime, then mark its rollout complete (no deployment
	// controller runs here).
	require.NoError(t, reconcile())
	dep := &appsv1.Deployment{}
	depKey := types.NamespacedName{Namespace: "default", Name: r.getDeploymentName(inUseProvider())}
	require.NoError(t, cli.Get(ctx, depKey, dep))
	dep.Generation = 1
	require.NoError(t, cli.Update(ctx, dep))
	require.NoError(t, cli.Get(ctx, depKey, dep))
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1,
		ReadyReplicas: 1, AvailableReplicas: 1}
	require.NoError(t, cli.Status().Update(ctx, dep))
	require.Zero(t, reader.reads.Load(), "nothing is read before the runtime is Running")

	// The reconcile that reads the capabilities cannot write the status.
	failStatusWrite.Store(true)
	require.Error(t, reconcile())
	assert.Equal(t, int32(1), reader.reads.Load())
	assert.Nil(t, getInUseProvider(t, r).Status.ReportedCapabilities)
	_, recorded := r.capabilityReads.Load(readKey)
	assert.False(t, recorded, "a read whose status write failed is not recorded")

	// The next reconcile reads them again and writes them.
	failStatusWrite.Store(false)
	require.NoError(t, reconcile())
	assert.Equal(t, int32(2), reader.reads.Load())
	got := getInUseProvider(t, r).Status.ReportedCapabilities
	require.NotNil(t, got)
	assert.True(t, got.SupportsHonestReconfigure)
	_, recorded = r.capabilityReads.Load(readKey)
	assert.True(t, recorded)

	// Recorded: a further reconcile within the settle delay reads nothing.
	require.NoError(t, reconcile())
	assert.Equal(t, int32(2), reader.reads.Load())
}
