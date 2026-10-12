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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestDeletionPersistsTheOutcomeBeforeReleasingTheFinalizer (review L): the
// staged-disk outcome is stored (StagingCleanup=True) before the finalizer is
// removed, so a finalizer update that conflicts re-runs neither the delete RPC
// nor its event.
func TestDeletionPersistsTheOutcomeBeforeReleasingTheFinalizer(t *testing.T) {
	prov := newStagingProvider()
	m, objs := stagingFixture(infrav1beta1.MigrationPhaseReady)
	now := metav1.Now()
	m.DeletionTimestamp = &now

	var conflicts atomic.Int32
	scheme := stagingScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, m)...).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*infrav1beta1.VMMigration); ok && conflicts.Add(1) == 1 {
					return apierrors.NewConflict(schema.GroupResource{Resource: "vmmigrations"}, obj.GetName(), nil)
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	rec := record.NewFakeRecorder(50)
	r := &VMMigrationReconciler{Client: c, Scheme: scheme, Recorder: rec,
		providerInstanceFn: func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return prov, nil }}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err, "the finalizer update conflicted")
	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, got))
	assert.Equal(t, migrationReasonStagedDiskDeleted, stagingCondition(got).Reason, "the outcome was stored first")

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), req.NamespacedName, got)), "released")
	assert.EqualValues(t, 1, prov.calls.Load(), "no second delete RPC")
	assert.Len(t, filterEvents(drainRecordedEvents(rec), migrationReasonStagedDiskDeleted), 1, "no second event")
}
