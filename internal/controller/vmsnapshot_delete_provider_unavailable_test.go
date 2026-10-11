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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMSnapshot delete held while the VM's Provider cannot be
// reached: the finalizer is kept with a constant ProviderUnavailable hold
// (no resolver text in status or events), retried with the blocked-VM
// backoff, and released by force-delete with a SnapshotLeftOnHypervisor
// Warning. Before, the delete was retried every 30 s with no status and
// force-delete was ignored.

// unresolvableProvider makes r's provider resolution fail with resolver-style
// text (leakyResolverDetail) until the returned restore func is called.
func unresolvableProvider(r *VMSnapshotReconciler, spy *snapshotDeleteSpy) (restore func()) {
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
		return nil, errors.New(leakyResolverDetail)
	}
	return func() {
		r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return spy, nil }
	}
}

// assertNoResolverText fails when any of texts carries resolver detail.
func assertNoResolverText(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		assert.NotContains(t, text, "10.96.12.7")
		assert.NotContains(t, text, "provider-tls")
		assert.NotContains(t, text, "phase Pending")
	}
}

func TestVMSnapshot_DeleteHeldWhileProviderUnavailable(t *testing.T) {
	ctx := context.Background()
	r, spy, get, events := deletingSnapshot(t, nil)
	restore := unresolvableProvider(r, spy)

	res, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the blocked-VM backoff, not a fixed 30 s")
	assert.Zero(t, spy.calls.Load(), "nothing is sent without a client")

	snap := get()
	require.NotNil(t, snap, "the finalizer is kept: the snapshot may still be on the hypervisor")
	for _, condType := range []string{infrav1beta1.VMSnapshotConditionReady, infrav1beta1.VMSnapshotConditionDeleting} {
		c := meta.FindStatusCondition(snap.Status.Conditions, condType)
		require.NotNil(t, c, condType)
		assert.Equal(t, metav1.ConditionFalse, c.Status, condType)
		assert.Equal(t, k8s.ReasonProviderUnavailable, c.Reason, condType)
		assert.Equal(t, snapshotDeleteProviderUnavailableMessage, c.Message, condType)
	}
	evs := eventsOf(events)
	assert.Contains(t, strings.Join(evs, "\n"), "Warning SnapshotDeleteFailed")
	assertNoResolverText(t, append(evs, snap.Status.Message)...)

	// A repeated hold writes nothing and records no event.
	rv := snap.ResourceVersion
	_, err = r.handleDeletion(ctx, snap)
	require.NoError(t, err)
	assert.Equal(t, rv, get().ResourceVersion)
	assert.Empty(t, eventsOf(events))

	// The backoff grows with the time the delete has been held.
	snap = get()
	for i := range snap.Status.Conditions {
		if snap.Status.Conditions[i].Type == infrav1beta1.VMSnapshotConditionDeleting {
			snap.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
		}
	}
	res, err = r.handleDeletion(ctx, snap)
	require.NoError(t, err)
	assert.InDelta(t, (2 * time.Minute).Seconds(), res.RequeueAfter.Seconds(), 5)

	// The Provider answers again: the snapshot is deleted and the finalizer released.
	restore()
	_, err = r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.EqualValues(t, 1, spy.calls.Load())
	assert.Nil(t, get())
}

// TestVMSnapshot_ProviderGoneReleasesWithAWarning (security review L3): a
// VMSnapshot whose VM's Provider no longer exists in its own namespace is
// released — nothing can delete the snapshot, and holding would wait forever
// — with a SnapshotLeftOnHypervisor Warning naming it, as the VirtualMachine
// and VMMigration controllers do; never silently.
func TestVMSnapshot_ProviderGoneReleasesWithAWarning(t *testing.T) {
	ctx := context.Background()
	r, spy, get, events := deletingSnapshot(t, nil)
	prov := &infrav1beta1.Provider{}
	prov.Namespace, prov.Name = snapPhaseNS, "prov"
	require.NoError(t, r.Delete(ctx, prov))

	_, err := r.handleDeletion(ctx, get())
	require.NoError(t, err)
	assert.Zero(t, spy.calls.Load())
	assert.Nil(t, get(), "released")
	evs := strings.Join(eventsOf(events), "\n")
	assert.Contains(t, evs, "Warning "+eventReasonSnapshotLeftOnHypervisor)
	assert.Contains(t, evs, `snapshot "snap-1" of VM "vm-100"`)
	assert.NotContains(t, evs, "Normal SnapshotDeleted")
}

func TestVMSnapshot_ForceDeleteReleasesWhileProviderUnavailable(t *testing.T) {
	t.Run("set before the delete", func(t *testing.T) {
		r, spy, get, events := deletingSnapshot(t, map[string]string{forceDeleteAnnotation: "true"})
		unresolvableProvider(r, spy)

		_, err := r.handleDeletion(context.Background(), get())
		require.NoError(t, err)
		assert.Nil(t, get(), "force-delete releases the finalizer")
		assert.Zero(t, spy.calls.Load())
		evs := eventsOf(events)
		joined := strings.Join(evs, "\n")
		assert.Contains(t, joined, "Warning "+eventReasonSnapshotLeftOnHypervisor)
		assert.Contains(t, joined, `snapshot "snap-1" of VM "vm-100"`)
		assert.NotContains(t, joined, "Normal SnapshotDeleted")
		assertNoResolverText(t, evs...)
	})

	t.Run("set while held", func(t *testing.T) {
		ctx := context.Background()
		r, spy, get, events := deletingSnapshot(t, nil)
		unresolvableProvider(r, spy)
		_, err := r.handleDeletion(ctx, get())
		require.NoError(t, err)
		require.NotNil(t, get())
		eventsOf(events)

		snap := get()
		snap.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		require.NoError(t, r.Update(ctx, snap))
		_, err = r.handleDeletion(ctx, get())
		require.NoError(t, err)
		assert.Nil(t, get())
		assert.Contains(t, strings.Join(eventsOf(events), "\n"), "Warning "+eventReasonSnapshotLeftOnHypervisor)
	})
}
