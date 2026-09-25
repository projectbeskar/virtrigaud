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
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a VMSnapshot is Ready only after a SnapshotCreate RPC
// was issued for it. The create path must not persist the Creating phase
// before the RPC: the task poll reads Creating with no task as a finished
// synchronous create, so a provider lookup failure used to become Ready=True on
// the next reconcile without any snapshot having been taken.

const (
	snapPhaseNS   = "default"
	snapPhaseName = "snap"
)

// snapshotCreateSpy is a stubProvider that counts SnapshotCreate calls and
// completes each one synchronously (no task).
type snapshotCreateSpy struct {
	stubProvider
	creates atomic.Int32
}

// SnapshotCreate records the call and returns a per-call snapshot id.
func (p *snapshotCreateSpy) SnapshotCreate(_ context.Context, _ contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	n := p.creates.Add(1)
	return contracts.SnapshotCreateResponse{SnapshotId: fmt.Sprintf("snap-%d", n)}, nil
}

// snapshotlessProviderSpy is a snapshotCreateSpy that reports it cannot take
// snapshots, for the capability gate.
type snapshotlessProviderSpy struct {
	snapshotCreateSpy
}

// GetCapabilities makes snapshotlessProviderSpy a contracts.CapabilityReporter.
func (p *snapshotlessProviderSpy) GetCapabilities(_ context.Context) (contracts.Capabilities, error) {
	return contracts.Capabilities{SupportsSnapshots: false}, nil
}

var (
	_ contracts.Provider           = (*snapshotCreateSpy)(nil)
	_ contracts.CapabilityReporter = (*snapshotlessProviderSpy)(nil)
)

// pendingSnapshot returns a snapshot of VM "web" that already carries the
// finalizer, so a reconcile goes straight to the phase handling.
func pendingSnapshot() *infrav1beta1.VMSnapshot {
	return &infrav1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapPhaseName, Namespace: snapPhaseNS, Generation: 1,
			Finalizers: []string{"snapshot.infra.virtrigaud.io/finalizer"},
		},
		Spec: infrav1beta1.VMSnapshotSpec{VMRef: infrav1beta1.LocalObjectReference{Name: "web"}},
	}
}

func newSnapPhaseReconciler(t *testing.T, objs ...client.Object) (*VMSnapshotReconciler, *record.FakeRecorder) {
	t.Helper()
	s := coverageTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMSnapshot{}).Build()
	rec := record.NewFakeRecorder(20)
	return &VMSnapshotReconciler{Client: c, Scheme: s, Recorder: rec}, rec
}

// reconcileSnapshot runs one full reconcile of the snapshot and returns it as
// persisted afterwards.
func reconcileSnapshot(t *testing.T, r *VMSnapshotReconciler) *infrav1beta1.VMSnapshot {
	t.Helper()
	key := types.NamespacedName{Namespace: snapPhaseNS, Name: snapPhaseName}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	snap := &infrav1beta1.VMSnapshot{}
	require.NoError(t, r.Get(context.Background(), key, snap))
	return snap
}

// requireNotReady asserts that snap is not reported Ready in any way.
func requireNotReady(t *testing.T, snap *infrav1beta1.VMSnapshot) {
	t.Helper()
	assert.NotEqual(t, infrav1beta1.SnapshotPhaseReady, snap.Status.Phase)
	if c := meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionReady); c != nil {
		assert.NotEqual(t, metav1.ConditionTrue, c.Status, "Ready=%s: %s", c.Reason, c.Message)
	}
}

func TestVMSnapshot_ProviderLookupFailureIsNeverReportedReady(t *testing.T) {
	ctx := context.Background()
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	r, rec := newSnapPhaseReconciler(t, vm, pendingSnapshot())
	spy := &snapshotCreateSpy{}
	clientErr := errors.New("dial provider: connection refused")
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
		if clientErr != nil {
			return nil, clientErr
		}
		return spy, nil
	}

	requireRetried := func(t *testing.T, snap *infrav1beta1.VMSnapshot, message string) {
		t.Helper()
		requireNotReady(t, snap)
		assert.Empty(t, snap.Status.Phase, "the create is retried unchanged, not left Creating")
		assert.Empty(t, snap.Status.TaskRef)
		assert.Empty(t, snap.Status.SnapshotID)
		c := meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
		require.NotNil(t, c)
		assert.Equal(t, infrav1beta1.VMSnapshotReasonProviderError, c.Reason)
		assert.Contains(t, c.Message, message)
		assert.Contains(t, snap.Status.Message, message)
		assert.Nil(t, meta.FindStatusCondition(snap.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating),
			"nothing is reported as started")
	}

	// The Provider does not exist: no reconcile reaches the provider or
	// reports the snapshot Ready.
	for range 2 {
		requireRetried(t, reconcileSnapshot(t, r), "Failed to get provider:")
	}

	// The Provider exists but no client can be resolved for it.
	require.NoError(t, r.Create(ctx, runningProvider(snapPhaseNS, "prov")))
	for range 2 {
		requireRetried(t, reconcileSnapshot(t, r), "Failed to get provider instance: dial provider: connection refused")
	}
	assert.Zero(t, spy.creates.Load(), "no SnapshotCreate was issued")
	assert.NotContains(t, strings.Join(drainEvents(rec), "\n"), "SnapshotReady")

	// Once the provider can be reached the create is issued, exactly once,
	// and only then is the snapshot Ready.
	clientErr = nil
	snap := reconcileSnapshot(t, r)
	assert.Equal(t, infrav1beta1.SnapshotPhaseReady, snap.Status.Phase)
	assert.Equal(t, "snap-1", snap.Status.SnapshotID)
	assert.NotNil(t, snap.Status.CreationTime)
	assert.Equal(t, int32(1), spy.creates.Load())
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Normal SnapshotReady")

	reconcileSnapshot(t, r)
	assert.Equal(t, int32(1), spy.creates.Load(), "a Ready snapshot is not created again")
}

func TestVMSnapshot_PreRPCRefusalsAreNeverReportedReady(t *testing.T) {
	cases := []struct {
		name string
		// boundElsewhere binds the VM through a Provider other than the one
		// its spec.providerRef names (vmRefFor refuses it).
		boundElsewhere bool
		// snapshotless makes the provider report no snapshot support, with
		// capability enforcement on (the capability gate refuses it).
		snapshotless bool
		wantPhase    infrav1beta1.SnapshotPhase
	}{
		{name: "VM bound through another Provider", boundElsewhere: true, wantPhase: ""},
		{name: "provider reports no snapshot support", snapshotless: true, wantPhase: infrav1beta1.SnapshotPhaseFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
			if tc.boundElsewhere {
				vm.Status.BoundProvider = &infrav1beta1.BoundProviderRef{Namespace: snapPhaseNS, Name: "other"}
			}
			r, rec := newSnapPhaseReconciler(t, vm, runningProvider(snapPhaseNS, "prov"), pendingSnapshot())
			spy := &snapshotCreateSpy{}
			var prov contracts.Provider = spy
			if tc.snapshotless {
				snapshotless := &snapshotlessProviderSpy{}
				spy, prov = &snapshotless.snapshotCreateSpy, snapshotless
				r.EnforceCapabilities = true
			}
			r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
				return prov, nil
			}

			for range 2 {
				snap := reconcileSnapshot(t, r)
				requireNotReady(t, snap)
				assert.Equal(t, tc.wantPhase, snap.Status.Phase)
			}
			assert.Zero(t, spy.creates.Load(), "no SnapshotCreate was issued")
			assert.NotContains(t, strings.Join(drainEvents(rec), "\n"), "SnapshotReady")
		})
	}
}

func TestVMSnapshot_CreatingWithNoRecordedCreateIsRetried(t *testing.T) {
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	// The state older managers persisted when the provider could not be
	// resolved: Creating, with no task, snapshot id or creation time.
	snap := pendingSnapshot()
	snap.Status.Phase = infrav1beta1.SnapshotPhaseCreating
	snap.Status.Message = "Creating snapshot"
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: infrav1beta1.VMSnapshotConditionCreating, Status: metav1.ConditionTrue,
		Reason: infrav1beta1.VMSnapshotReasonCreating, Message: "Snapshot creation initiated",
	})
	r, rec := newSnapPhaseReconciler(t, vm, runningProvider(snapPhaseNS, "prov"), snap)
	spy := &snapshotCreateSpy{}
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
		return spy, nil
	}

	got := reconcileSnapshot(t, r)
	requireNotReady(t, got)
	assert.Empty(t, got.Status.Phase, "returned to the initial phase")
	assert.Equal(t, snapshotCreateNotIssuedMessage, got.Status.Message)
	assert.Zero(t, spy.creates.Load())
	assert.Empty(t, drainEvents(rec))

	got = reconcileSnapshot(t, r)
	assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
	assert.Equal(t, "snap-1", got.Status.SnapshotID)
	assert.Equal(t, int32(1), spy.creates.Load(), "the create is issued")
}

func TestVMSnapshot_RetentionSkipsAReadySnapshotWithNoCreationTime(t *testing.T) {
	vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
	// What older managers left behind: Ready with no create recorded.
	snap := pendingSnapshot()
	snap.Spec.RetentionPolicy = &infrav1beta1.SnapshotRetentionPolicy{MaxAge: &metav1.Duration{Duration: time.Nanosecond}}
	snap.Status.Phase = infrav1beta1.SnapshotPhaseReady
	r, _ := newSnapPhaseReconciler(t, vm, snap)

	require.NotPanics(t, func() {
		got := reconcileSnapshot(t, r)
		assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase, "left for the operator, not expired")
	})
}

func TestVMSnapshot_CreatingWithRecordedSynchronousCreateIsReady(t *testing.T) {
	cases := map[string]func(*infrav1beta1.VMSnapshot){
		"snapshot id recorded": func(s *infrav1beta1.VMSnapshot) { s.Status.SnapshotID = "snap-7" },
		"creation time recorded": func(s *infrav1beta1.VMSnapshot) {
			now := metav1.Now()
			s.Status.CreationTime = &now
		},
	}
	for name, recordCreate := range cases {
		t.Run(name, func(t *testing.T) {
			vm := sourceVMWithID(snapPhaseNS, "web", "prov", "vm-100")
			snap := pendingSnapshot()
			snap.Status.Phase = infrav1beta1.SnapshotPhaseCreating
			recordCreate(snap)
			r, _ := newSnapPhaseReconciler(t, vm, snap)

			got := reconcileSnapshot(t, r)
			assert.Equal(t, infrav1beta1.SnapshotPhaseReady, got.Status.Phase)
			c := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionTrue, c.Status)
		})
	}
}
