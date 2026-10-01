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

// These tests pin that a clone the provider accepted is never lost to a
// concurrent edit of its VMClone. A libvirt single-host Clone is synchronous
// (a full disk copy, minutes long); a tenant editing the VMClone's metadata
// meanwhile made the post-RPC status update fail with a Conflict, so the
// target VM ID was lost, the next reconcile sent the Clone again, the provider
// refused it ("already exists"), the clone failed, and the copy was left
// untracked on the host.

// annotateDuringClone makes the provider's Clone edit the VMClone's metadata
// while it runs (as a tenant's kubectl annotate would), which moves its
// resourceVersion past the one the reconcile read.
func annotateDuringClone(t *testing.T, r *VMCloneReconciler, cp *clonerProvider, clone *infrav1beta1.VMClone) {
	t.Helper()
	cp.onClone = func() {
		cur := getClone(t, r, clone)
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations["example.com/poke"] = cur.ResourceVersion
		require.NoError(t, r.Update(context.Background(), cur))
	}
}

func TestVMClone_AcceptedCloneSurvivesAConcurrentEdit(t *testing.T) {
	for name, tc := range map[string]struct {
		fixture  func(t *testing.T, cp *clonerProvider) (*VMCloneReconciler, *infrav1beta1.VMClone)
		targetID string
	}{
		"single-host": {
			fixture: func(t *testing.T, cp *clonerProvider) (*VMCloneReconciler, *infrav1beta1.VMClone) {
				clone := &infrav1beta1.VMClone{
					ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default"},
					Spec: infrav1beta1.VMCloneSpec{
						Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
						Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
					},
				}
				return newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, runningProvider("default", "prov-1"),
					sourceVMWithID("default", "src-vm", "prov-1", "default.src-vm"), clone), clone
			},
			targetID: "default.clone-target",
		},
		"clustered": {
			fixture: func(t *testing.T, cp *clonerProvider) (*VMCloneReconciler, *infrav1beta1.VMClone) {
				return clusteredCloneFixture(t, boundSource(), cp)
			},
			targetID: "default.clone-c-target",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, async := range []bool{false, true} {
				cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: tc.targetID}}
				if async {
					cp.cloneResp.TaskRef = "task-1"
				}
				r, clone := tc.fixture(t, cp)
				annotateDuringClone(t, r, cp, clone)

				// Up to and including the reconcile that sends the Clone.
				for i := 0; i < 4 && cp.cloneCnt == 0; i++ {
					reconcileClone(t, r, clone, 1)
				}
				require.Equal(t, 1, cp.cloneCnt)
				got := getClone(t, r, clone)
				assert.Equal(t, tc.targetID, got.Status.TargetVMID, "the accepted clone is recorded despite the edit")
				if async {
					assert.Equal(t, "task-1", got.Status.TaskRef)
				}
				assert.NotNil(t, got.Status.StartTime)
				assert.NotEmpty(t, got.Annotations["example.com/poke"], "the concurrent edit is kept")

				// The clone completes without a second Clone.
				cp.onClone = nil
				reconcileClone(t, r, clone, 3)
				assert.Equal(t, 1, cp.cloneCnt, "never cloned twice")
				got = getClone(t, r, clone)
				assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
				target := &infrav1beta1.VirtualMachine{}
				require.NoError(t, r.Get(context.Background(),
					client.ObjectKey{Namespace: "default", Name: clone.Spec.Target.Name}, target))
				assert.Equal(t, tc.targetID, target.Status.ID, "the target is bound to the clone that was made")
			}
		})
	}
}

// TestVMClone_Clustered_TargetUIDSurvivesAConcurrentEdit: the record of the
// target VirtualMachine a clustered clone created is not lost to an edit of
// the VMClone either, so a clone that then fails still removes it.
func TestVMClone_Clustered_TargetUIDSurvivesAConcurrentEdit(t *testing.T) {
	cp := &clonerProvider{cloneErr: contracts.NewHostUnavailableError(`clone: host "host-alpha" is unreachable`, nil)}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	stale := getClone(t, r, clone)
	reconcileClone(t, r, clone, 1) // adds the finalizer

	// An edit lands between the reconcile's read and its record of the target.
	r.Client = &updateBeforeStatusWrite{Client: r.Client, t: t, key: client.ObjectKeyFromObject(stale)}
	reconcileClone(t, r, clone, 1)
	got := getClone(t, r, clone)
	assert.NotEmpty(t, got.Status.TargetUID, "the created target is recorded despite the edit")
	assert.Equal(t, string(getTarget(t, r).UID), got.Status.TargetUID)
}

// updateBeforeStatusWrite is a client that edits the VMClone's metadata right
// before each of its status writes, as a concurrent tenant edit would.
type updateBeforeStatusWrite struct {
	client.Client
	t   *testing.T
	key client.ObjectKey
}

// Status returns a status writer that edits the VMClone before writing.
func (c *updateBeforeStatusWrite) Status() client.SubResourceWriter {
	return &editingStatusWriter{SubResourceWriter: c.Client.Status(), c: c}
}

// editingStatusWriter edits the VMClone's metadata before each write.
type editingStatusWriter struct {
	client.SubResourceWriter
	c *updateBeforeStatusWrite
}

// edit bumps the VMClone's resourceVersion with a metadata-only update.
func (w *editingStatusWriter) edit(ctx context.Context, obj client.Object) {
	if _, ok := obj.(*infrav1beta1.VMClone); !ok {
		return
	}
	cur := &infrav1beta1.VMClone{}
	require.NoError(w.c.t, w.c.Get(ctx, w.c.key, cur))
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations["example.com/poke"] = cur.ResourceVersion
	require.NoError(w.c.t, w.c.Update(ctx, cur))
}

// Update edits the VMClone, then writes.
func (w *editingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.edit(ctx, obj)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// Patch edits the VMClone, then writes.
func (w *editingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.edit(ctx, obj)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// staleCloneClient answers a Get of one VMClone with a fixed copy (an
// informer cache that has not caught up) and passes everything else through.
type staleCloneClient struct {
	client.Client
	stale *infrav1beta1.VMClone
}

// Get returns the stale copy for the VMClone.
func (c *staleCloneClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if clone, ok := obj.(*infrav1beta1.VMClone); ok && key == client.ObjectKeyFromObject(c.stale) {
		c.stale.DeepCopyInto(clone)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestVMClone_NoCloneFromAStaleCache: a reconcile that reads the clone from a
// cache that does not show yet that it Failed (or that its clone was recorded)
// re-reads it live and sends no Clone; the stored state is untouched.
func TestVMClone_NoCloneFromAStaleCache(t *testing.T) {
	ctx := context.Background()
	for name, settle := range map[string]func(c *infrav1beta1.VMClone){
		"stored Failed": func(c *infrav1beta1.VMClone) {
			c.Status.Phase = infrav1beta1.ClonePhaseFailed
			c.Status.Message = "clone failed: boom"
		},
		"stored clone recorded": func(c *infrav1beta1.VMClone) {
			c.Status.Phase = infrav1beta1.ClonePhaseCloning
			c.Status.TargetVMID = "default.clone-target"
		},
	} {
		t.Run(name, func(t *testing.T) {
			clone := &infrav1beta1.VMClone{
				ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default", Finalizers: []string{vmCloneFinalizer}},
				Spec: infrav1beta1.VMCloneSpec{
					Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
					Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
				},
				Status: infrav1beta1.VMCloneStatus{Phase: infrav1beta1.ClonePhasePending},
			}
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-target"}}
			r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, runningProvider("default", "prov-1"),
				sourceVMWithID("default", "src-vm", "prov-1", "default.src-vm"), clone)
			stale := getClone(t, r, clone)
			stored := stale.DeepCopy()
			settle(stored)
			require.NoError(t, r.Status().Update(ctx, stored))
			want := getClone(t, r, clone)

			live := r.Client
			r.APIReader = live
			r.Client = &staleCloneClient{Client: live, stale: stale}
			res := reconcileClone(t, r, clone, 1)

			assert.Zero(t, cp.cloneCnt, "no Clone is sent from a stale read")
			assert.Equal(t, cloneStaleReadRetryInterval, res.RequeueAfter, "retried shortly, from a fresh read")
			got := &infrav1beta1.VMClone{}
			require.NoError(t, live.Get(ctx, client.ObjectKeyFromObject(clone), got))
			assert.Equal(t, want.ResourceVersion, got.ResourceVersion, "the stored clone is not written")
			assert.Equal(t, want.Status, got.Status)
		})
	}
}

// TestVMClone_Clustered_NoTargetFromAStaleCache: a clustered clone read from a
// cache that does not show yet that it Failed creates no target VirtualMachine
// and records nothing: its live status is untouched.
func TestVMClone_Clustered_NoTargetFromAStaleCache(t *testing.T) {
	ctx := context.Background()
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	stored := getClone(t, r, clone)
	stored.Finalizers = []string{vmCloneFinalizer}
	require.NoError(t, r.Update(ctx, stored))
	stale := getClone(t, r, clone) // Pending/empty, as the cache still shows it
	failed := stale.DeepCopy()
	failed.Status.Phase = infrav1beta1.ClonePhaseFailed
	failed.Status.Message = "clone failed: boom"
	require.NoError(t, r.Status().Update(ctx, failed))
	want := getClone(t, r, clone)

	live := r.Client
	r.APIReader = live
	r.Client = &staleCloneClient{Client: live, stale: stale}
	res := reconcileClone(t, r, clone, 1)

	assert.Equal(t, cloneStaleReadRetryInterval, res.RequeueAfter, "retried shortly, from a fresh read")
	err := live.Get(ctx, targetKey, &infrav1beta1.VirtualMachine{})
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "no target VirtualMachine is created from a stale read")
	got := &infrav1beta1.VMClone{}
	require.NoError(t, live.Get(ctx, client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, want.ResourceVersion, got.ResourceVersion, "the live Failed is not written over")
	assert.Equal(t, want.Status, got.Status)
	assert.Zero(t, cp.cloneCnt)
}
