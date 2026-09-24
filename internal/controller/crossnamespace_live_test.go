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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The cached grant check (informer) can lag a revocation. These tests make the
// cache and the API server disagree — the reconciler's Client still shows the
// grant, its APIReader (the live read) shows it revoked — and pin that none of
// the side-effecting calls runs: the Clone RPC, the ImportDisk call, and the
// target VirtualMachine Create for a clone bind and for a migration.

// liveNamespaceReader is an uncached-reader stand-in holding ns, counting the
// Namespace reads made through it. err, when set, fails every read.
type liveNamespaceReader struct {
	client.Client
	reads int
}

func newLiveReader(t *testing.T, ns *corev1.Namespace, err error) *liveNamespaceReader {
	t.Helper()
	lr := &liveNamespaceReader{}
	b := fake.NewClientBuilder().WithScheme(crossNSTestScheme(t))
	if ns != nil {
		b = b.WithObjects(ns)
	}
	lr.Client = b.WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isNS := obj.(*corev1.Namespace); isNS {
				lr.reads++
			}
			if err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	return lr
}

// TestLiveGrant_CloneRPCNotIssuedWhenRevokedLive: the cache still grants, the
// API server does not — the Clone RPC is never sent.
func TestLiveGrant_CloneRPCNotIssuedWhenRevokedLive(t *testing.T) {
	clone := xnsClone(xnsTarget)
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)))
	live := newLiveReader(t, grantNamespace(xnsTarget, nil), nil)
	r.APIReader = live

	reconcileClone(t, r, clone, 4)

	assert.Zero(t, cp.cloneCnt, "no Clone RPC when the live read shows the grant revoked")
	assert.Positive(t, live.reads, "the grant was re-read live before the RPC")
	got := getClone(t, r, clone)
	assertCloneRefused(t, got)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	cloning := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionCloning)
	assert.True(t, cloning == nil || cloning.Status != metav1.ConditionTrue, "the clone is not reported as started")
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

// TestLiveGrant_CloneBindCreateNotIssuedWhenRevokedLive: the provider-side
// clone already exists (target ID recorded); the bind's VirtualMachine Create
// is not issued while the live read shows the grant revoked, and the target ID
// is kept for a later bind.
func TestLiveGrant_CloneBindCreateNotIssuedWhenRevokedLive(t *testing.T) {
	clone := xnsClone(xnsTarget)
	clone.Finalizers = []string{vmCloneFinalizer}
	clone.Status.Phase = infrav1beta1.ClonePhaseCloning
	clone.Status.TargetVMID = "vm-clone-1"
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)))
	live := newLiveReader(t, grantNamespace(xnsTarget, strPtr("team-c")), nil)
	r.APIReader = live

	reconcileClone(t, r, clone, 3)

	assert.Zero(t, cp.cloneCnt)
	assert.Positive(t, live.reads)
	got := getClone(t, r, clone)
	assertCloneRefused(t, got)
	assert.Equal(t, "vm-clone-1", got.Status.TargetVMID)
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

// TestLiveGrant_CloneProceedsWhenBothAgree: with the grant in the cache AND
// live, the clone runs; the live reader is consulted for the RPC and the
// Create.
func TestLiveGrant_CloneProceedsWhenBothAgree(t *testing.T) {
	clone := xnsClone(xnsTarget)
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)))
	live := newLiveReader(t, grantNamespace(xnsTarget, strPtr(xnsSource)), nil)
	r.APIReader = live

	reconcileClone(t, r, clone, 4)

	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
	assert.Equal(t, 1, cp.cloneCnt)
	assert.Equal(t, 2, live.reads, "one live read before the Clone RPC, one before the Create")
}

// TestLiveGrant_SameNamespaceNeverReadsLive: the own namespace needs no grant,
// so no live read (no extra API load) happens.
func TestLiveGrant_SameNamespaceNeverReadsLive(t *testing.T) {
	clone := xnsClone("")
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone)
	live := newLiveReader(t, nil, nil)
	r.APIReader = live

	reconcileClone(t, r, clone, 4)

	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
	assert.Zero(t, live.reads)
}

// TestLiveGrant_CloneLiveReadErrorFailsClosed: a failing live read returns an
// error and sends nothing.
func TestLiveGrant_CloneLiveReadErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone(xnsTarget)
	clone.Finalizers = []string{vmCloneFinalizer}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)))
	r.APIReader = newLiveReader(t, nil, errors.New("apiserver unavailable"))

	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.Error(t, err)
	assert.Zero(t, cp.cloneCnt)
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

// TestLiveGrant_ImportDiskNotIssuedWhenRevokedLive: the cache still grants,
// the API server does not — ImportDisk is never sent, and the in-memory
// import guard is not claimed, so the import runs once the grant is live.
func TestLiveGrant_ImportDiskNotIssuedWhenRevokedLive(t *testing.T) {
	ctx := context.Background()
	prov := &capturingMigrationProvider{importResp: contracts.ImportDiskResponse{
		DiskId: "team-b.target-vm-migrated", Path: "/var/lib/libvirt/images/team-b.target-vm-migrated.qcow2",
	}}
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
	migration.Spec.Target.Namespace = xnsTarget
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret(),
		grantNamespace(xnsTarget, strPtr("default")))
	live := newLiveReader(t, grantNamespace(xnsTarget, nil), nil)
	r.APIReader = live

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, crossNamespaceRecheckInterval, res.RequeueAfter)
	assert.Zero(t, prov.importCalls, "no ImportDisk when the live read shows the grant revoked")
	assert.Positive(t, live.reads)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase)
	assertMigrationRefused(t, got, xnsTarget)
	assert.False(t, r.longOpAlreadyStarted(got, longOpImport), "a refusal must not claim the import guard")

	// The grant becomes live: the same generation imports.
	r.APIReader = newLiveReader(t, grantNamespace(xnsTarget, strPtr("default")), nil)
	_, err = r.handleImportingPhase(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.Equal(t, 1, prov.importCalls)
}

// TestLiveGrant_MigrationCreateNotIssuedWhenRevokedLive: the target
// VirtualMachine Create is not issued while the live read shows the grant
// revoked.
func TestLiveGrant_MigrationCreateNotIssuedWhenRevokedLive(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture(xnsTarget)
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration,
		grantNamespace(xnsTarget, strPtr("default")))
	live := newLiveReader(t, grantNamespace(xnsTarget, strPtr("team-c")), nil)
	r.APIReader = live

	res, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, crossNamespaceRecheckInterval, res.RequeueAfter)
	assert.Positive(t, live.reads)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
	assertMigrationRefused(t, got, xnsTarget)
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

// TestLiveGrant_MigrationLiveReadErrorFailsClosed: a failing live read returns
// an error before ImportDisk.
func TestLiveGrant_MigrationLiveReadErrorFailsClosed(t *testing.T) {
	prov := &capturingMigrationProvider{}
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
	migration.Spec.Target.Namespace = xnsTarget
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret(),
		grantNamespace(xnsTarget, strPtr("default")))
	r.APIReader = newLiveReader(t, nil, errors.New("apiserver unavailable"))

	_, err := r.handleImportingPhase(context.Background(), migration)
	require.Error(t, err)
	assert.Zero(t, prov.importCalls)
}

// TestLiveGrant_ConstructorsWireTheAPIReader: the production constructors
// store the uncached reader they are given.
func TestLiveGrant_ConstructorsWireTheAPIReader(t *testing.T) {
	live := newLiveReader(t, nil, nil)
	s := xnsCloneScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	assert.Same(t, live, NewVMCloneReconciler(c, live, s, nil, record.NewFakeRecorder(1)).APIReader)
	assert.Same(t, live, NewVMMigrationReconciler(c, live, s, nil, record.NewFakeRecorder(1), false).APIReader)
}
