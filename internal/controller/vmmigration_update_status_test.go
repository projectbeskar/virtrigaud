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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// conflictingStatusClient builds a fake client seeded with m whose first
// `conflicts` status updates answer Conflict; *calls counts every status
// update.
func conflictingStatusClient(t *testing.T, m *infrav1beta1.VMMigration, conflicts int, calls *int) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(capGatingScheme(t)).
		WithObjects(m).
		WithStatusSubresource(&infrav1beta1.VMMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				*calls++
				if *calls <= conflicts {
					return apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmmigrations"},
						obj.GetName(), nil)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
}

// TestVMMigrationUpdateStatus_RetriesConflictOnTheLiveObject pins that a
// status conflict is retried (retry.RetryOnConflict, no time.Sleep) on the
// live object with this reconcile's status carried over.
func TestVMMigrationUpdateStatus_RetriesConflictOnTheLiveObject(t *testing.T) {
	_, _, m := snapshotFixture()
	calls := 0
	c := conflictingStatusClient(t, m.DeepCopy(), 2, &calls)
	r := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}

	cur := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), cur))
	cur.Status.Message = "written after two conflicts"
	require.NoError(t, r.updateStatus(context.Background(), cur))
	assert.Equal(t, 3, calls)

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	assert.Equal(t, "written after two conflicts", got.Status.Message)
}

// TestVMMigrationUpdateStatus_NeverWritesARecreatedObject pins that a
// conflict caused by the VMMigration being re-created (another UID) is not
// retried onto the new object.
func TestVMMigrationUpdateStatus_NeverWritesARecreatedObject(t *testing.T) {
	_, _, m := snapshotFixture()
	calls := 0
	c := conflictingStatusClient(t, m.DeepCopy(), 1, &calls)
	r := &VMMigrationReconciler{Client: c, Recorder: record.NewFakeRecorder(5)}

	stale := m.DeepCopy()
	stale.UID = types.UID("uid-of-the-deleted-one")
	stale.Status.Message = "stale"
	require.Error(t, r.updateStatus(context.Background(), stale))
	assert.Equal(t, 1, calls, "nothing is written to the re-created object")

	got := &infrav1beta1.VMMigration{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(m), got))
	assert.NotEqual(t, "stale", got.Status.Message)
}
