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
	require.NoError(w.c.t, w.c.Client.Get(ctx, w.c.key, cur))
	if cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	cur.Annotations["example.com/poke"] = cur.ResourceVersion
	require.NoError(w.c.t, w.c.Client.Update(ctx, cur))
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
