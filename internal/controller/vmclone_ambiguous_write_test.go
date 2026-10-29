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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// A clustered clone's target pendingHost write whose answer is lost (G2/G4 for
// clones): the admission keeps counting, the retry made while the cache is
// still stale neither moves the clone nor overbooks the host, and the Clone RPC
// is sent once, only after the write is resolved.

// cloneReconcile runs one pass of the clone reconciler.
func cloneReconcile(cr *VMCloneReconciler, clone *infrav1beta1.VMClone) error {
	_, err := cr.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	return err
}

// liveTarget reads the clone target "copy" from the live store.
func liveTarget(t *testing.T, lc *laggingClient) *infrav1beta1.VirtualMachine {
	t.Helper()
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, lc.Client.Get(context.Background(), client.ObjectKey{Namespace: capNS, Name: "copy"}, vm))
	return vm
}

func TestVMClone_Clustered_AmbiguousPendingHostWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{"the write landed but its answer was lost", true},
		{"the write timed out without landing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// host-a: 4 vCPU, the source holds 2, room for exactly one more 2 vCPU VM.
			vmr, cr, cp, clone, lc := createAndCloneFixtureWith(t, 4, ambiguousOnce("copy", tc.landed))

			// 1. The pendingHost write times out: nothing is cloned.
			require.Error(t, cloneReconcile(cr, clone))
			assert.Zero(t, cp.cloneCnt, "no Clone while the pendingHost write is unresolved")
			if tc.landed {
				assert.Equal(t, "host-a", liveTarget(t, lc).Status.Placement.PendingHost, "the write is stored at the API server")
			} else {
				assert.Nil(t, liveTarget(t, lc).Status.Placement, "the write did not land")
			}
			a, _, ok := vmr.placementAssumptions().Lookup("uid-copy")
			require.True(t, ok, "the clone's admission keeps counting")
			assert.Equal(t, assume.WriteInFlight, a.Write)
			assert.Equal(t, "host-a", a.HostID)

			// 2. While the cache is stale (it shows no pendingHost), the host's
			// last capacity is not given to a create.
			host, _ := resolve(t, vmr, readVM(t, lc, "a"))
			assert.Empty(t, host, "the unresolved clone admission keeps its capacity")

			// 3. A retry from the stale cache re-admits the same host and writes
			// again with the stale resourceVersion: the API server rejects it
			// (the target changed since the snapshot). A rejected retry does not
			// release a pick whose earlier write is unresolved, and no Clone is
			// sent while the outcome is uncertain.
			for range 2 {
				_ = cloneReconcile(cr, clone)
			}
			assert.Zero(t, cp.cloneCnt, "no Clone from a stale cache while the earlier write is unresolved")
			a, _, ok = vmr.placementAssumptions().Lookup("uid-copy")
			require.True(t, ok, "the admission keeps counting")
			assert.Equal(t, "host-a", a.HostID, "the admitted host is never changed")
			host, _ = resolve(t, vmr, readVM(t, lc, "a"))
			assert.Empty(t, host, "the retry cannot overbook host-a")
			if pl := liveTarget(t, lc).Status.Placement; pl != nil {
				assert.Equal(t, "host-a", pl.PendingHost)
			}

			// 4. Once the cache shows the record, the clone completes on host-a,
			// with exactly one Clone RPC, and the bound target keeps counting.
			for range 3 {
				catchUpWithClones(t, lc)
				_ = cloneReconcile(cr, clone)
			}
			catchUpWithClones(t, lc)
			assert.Equal(t, 1, cp.cloneCnt, "exactly one Clone RPC")
			assert.Equal(t, "host-a", boundHost(readVM(t, lc, "copy")))
			host, _ = resolve(t, vmr, readVM(t, lc, "a"))
			assert.Empty(t, host, "the bound clone counts at its recorded size")
		})
	}
}

// rejectOnce fails the first status write of the VM named name with a
// definite rejection (the API server refused it; nothing was stored).
func rejectOnce(name string) interceptor.Funcs {
	var once sync.Once
	return interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			fail := false
			if obj.GetName() == name {
				once.Do(func() { fail = true })
			}
			if fail {
				return apierrors.NewInvalid(schema.GroupKind{Group: "infra.virtrigaud.io", Kind: "VirtualMachine"}, name, nil)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
}

// TestVMClone_Clustered_RejectedPendingHostWriteReleasesOnlyItsOwn: a definite
// rejection of the clone target's pendingHost write releases the clone's
// admission and nothing else: a create admitted on the same host keeps its own.
func TestVMClone_Clustered_RejectedPendingHostWriteReleasesOnlyItsOwn(t *testing.T) {
	// host-a: 8 vCPU, the source holds 2: room for the create and the clone.
	vmr, cr, cp, clone, lc := createAndCloneFixtureWith(t, 8, rejectOnce("copy"))
	host, _ := resolve(t, vmr, readVM(t, lc, "a"))
	require.Equal(t, "host-a", host)
	_, _, ok := vmr.placementAssumptions().Lookup("uid-a")
	require.True(t, ok)

	require.Error(t, cloneReconcile(cr, clone))
	assert.Zero(t, cp.cloneCnt, "a rejected pendingHost write sends no Clone")
	_, _, ok = vmr.placementAssumptions().Lookup("uid-copy")
	assert.False(t, ok, "the rejected write releases the clone's own admission")
	a, _, ok := vmr.placementAssumptions().Lookup("uid-a")
	require.True(t, ok, "the create's admission on the same host is untouched")
	assert.Equal(t, "host-a", a.HostID)

	// Once the cache is current, the clone is admitted again and sent once.
	for range 2 {
		catchUpWithClones(t, lc)
		require.NoError(t, cloneReconcile(cr, clone))
	}
	assert.Equal(t, 1, cp.cloneCnt)
	assert.Equal(t, "host-a", liveTarget(t, lc).Status.Placement.Host)
}
