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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMMigration controller's enforcement of
// spec.consumerNamespaceSelector: the source VM's Provider must select the
// migration's namespace; the target Provider must select the migration's
// namespace and the target namespace; the target VMClass must select the
// target namespace. A refusal is Ready=False/ConsumerNotAllowed, not a failure,
// and no provider is resolved or called.

// countedMigrationReconciler wraps newXNSMigrationReconciler, counting every
// provider resolution.
func countedMigrationReconciler(t *testing.T, spy contracts.Provider, objs ...client.Object) (*VMMigrationReconciler, *atomic.Int32) {
	t.Helper()
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), spy, objs...)
	var resolved atomic.Int32
	r.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
		resolved.Add(1)
		return spy, nil
	}
	return r, &resolved
}

func reconcileMigration(t *testing.T, r *VMMigrationReconciler, m *infrav1beta1.VMMigration, times int) reconcile.Result {
	t.Helper()
	var res reconcile.Result
	for i := 0; i < times; i++ {
		var err error
		res, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)})
		require.NoError(t, err)
	}
	return res
}

// requireMigrationConsumerRefused asserts Ready=False/ConsumerNotAllowed for
// kind at the current generation, and that the migration did not fail.
func requireMigrationConsumerRefused(t *testing.T, got *infrav1beta1.VMMigration, kind, ns, name string) {
	t.Helper()
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonConsumerNotAllowed, ready.Reason)
	assert.Equal(t, got.Generation, ready.ObservedGeneration)
	assert.Equal(t, (&ConsumerNotAllowedError{Kind: kind, Namespace: ns, Name: name}).Error(), ready.Message)
	assert.NotEqual(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase, "a refusal is recoverable, not a failure")
	assert.Zero(t, got.Status.RetryCount)
}

func noMigrationSideEffects(t *testing.T, spy *migrationSpy) {
	t.Helper()
	imports, exports, snapshots, power := spy.calls()
	assert.Zero(t, imports+exports+snapshots+power, "no provider side effect")
}

func TestVMMigrationConsumer_SourceProviderInAnotherNamespace(t *testing.T) {
	ctx := context.Background()
	migration, objs := xnsMigration("")
	src, ok := objs[0].(*infrav1beta1.VirtualMachine)
	require.True(t, ok)
	src.Spec.ProviderRef = infrav1beta1.ObjectRef{Name: "src-prov", Namespace: cgOwnerNS}
	shared := readyProvider(cgOwnerNS, "src-prov")
	shared.Spec.Type = infrav1beta1.ProviderTypeVSphere
	objs = append(objs, shared, labeledNamespace(xnsSource, nil), migration)
	spy := &migrationSpy{}
	r, resolved := countedMigrationReconciler(t, spy, objs...)

	res := reconcileMigration(t, r, migration, 4)
	assert.Equal(t, consumerNotAllowedRetryInterval, res.RequeueAfter)
	got := getXNSMigration(t, r, migration)
	requireMigrationConsumerRefused(t, got, consumerKindProvider, cgOwnerNS, "src-prov")
	assert.Zero(t, resolved.Load(), "no Provider is resolved to a client")
	noMigrationSideEffects(t, spy)

	// The source provider's cleanup path refuses too.
	err := r.deleteSourceSnapshot(ctx, got)
	require.Error(t, err)
	assert.True(t, isConsumerNotAllowed(err))
	assert.Zero(t, resolved.Load())

	// Sharing the Provider with the migration's namespace lets it proceed and
	// clears the refusal.
	p := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(shared), p))
	p.Spec.ConsumerNamespaceSelector = sharedWithNamespace(xnsSource)
	require.NoError(t, r.Update(ctx, p))
	reconcileMigration(t, r, migration, 2) // Pending -> Validating -> Exporting
	got = getXNSMigration(t, r, migration)
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionReady)
	assert.True(t, ready == nil || ready.Reason != k8s.ReasonConsumerNotAllowed, "the refusal is cleared once granted")
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
}

func TestVMMigrationConsumer_TargetProviderInAnotherNamespace(t *testing.T) {
	migration, objs := xnsMigration("")
	migration.Spec.Target.ProviderRef = infrav1beta1.ObjectRef{Name: "tgt-prov", Namespace: cgOwnerNS}
	for name, tc := range map[string]struct {
		sel     *metav1.LabelSelector
		allowed bool
	}{
		"no selector":  {nil, false},
		"non-matching": {sharedWithNamespace("team-z"), false},
		"matching":     {sharedWithNamespace(xnsSource), true},
		"empty":        {&metav1.LabelSelector{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			tgt := readyProvider(cgOwnerNS, "tgt-prov")
			tgt.Spec.Type = infrav1beta1.ProviderTypeLibvirt
			tgt.Spec.ConsumerNamespaceSelector = tc.sel
			m := migration.DeepCopy()
			spy := &migrationSpy{}
			r, resolved := countedMigrationReconciler(t, spy, append(append([]client.Object{}, objs...), tgt, labeledNamespace(xnsSource, nil), m)...)

			reconcileMigration(t, r, m, 4)
			got := getXNSMigration(t, r, m)
			if tc.allowed {
				assert.Equal(t, infrav1beta1.MigrationPhaseExporting, got.Status.Phase)
				return
			}
			requireMigrationConsumerRefused(t, got, consumerKindProvider, cgOwnerNS, "tgt-prov")
			assert.Zero(t, resolved.Load())
			noMigrationSideEffects(t, spy)
		})
	}
}

func TestVMMigrationConsumer_CrossNamespaceTargetNeedsTheTargetNamespaceGrant(t *testing.T) {
	ctx := context.Background()
	migration, objs := xnsMigration(xnsTarget)
	tgt, ok := objs[2].(*infrav1beta1.Provider)
	require.True(t, ok)
	// Shared with the migration's namespace only: the import may run, but the
	// target VM in team-b could not use it.
	tgt.Spec.ConsumerNamespaceSelector = sharedWithNamespace(xnsSource)
	objs = append(objs, labeledNamespace(xnsSource, nil), grantNamespace(xnsTarget, strPtr(xnsSource)), migration)
	spy := &migrationSpy{}
	r, resolved := countedMigrationReconciler(t, spy, objs...)

	res := reconcileMigration(t, r, migration, 4)
	assert.Equal(t, consumerNotAllowedRetryInterval, res.RequeueAfter)
	got := getXNSMigration(t, r, migration)
	requireMigrationConsumerRefused(t, got, consumerKindProvider, xnsSource, "tgt-prov")
	assert.Zero(t, resolved.Load())
	noMigrationSideEffects(t, spy)
	assertNothingInNamespace(t, r.Client, xnsTarget)

	// Share the Provider with every namespace; the class ("cls", shared in the
	// fixture) is then what the target VM needs next — revoke it to prove the
	// VMClass is checked too.
	p := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(tgt), p))
	p.Spec.ConsumerNamespaceSelector = &metav1.LabelSelector{}
	require.NoError(t, r.Update(ctx, p))
	cls := &infrav1beta1.VMClass{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsSource, Name: "cls"}, cls))
	cls.Spec.ConsumerNamespaceSelector = nil
	require.NoError(t, r.Update(ctx, cls))
	reconcileMigration(t, r, migration, 2)
	requireMigrationConsumerRefused(t, getXNSMigration(t, r, migration), consumerKindVMClass, xnsSource, "cls")
	noMigrationSideEffects(t, spy)

	cls.Spec.ConsumerNamespaceSelector = sharedWithAll()
	require.NoError(t, r.Update(ctx, cls))
	reconcileMigration(t, r, migration, 2) // Pending -> Validating -> Exporting
	assert.Equal(t, infrav1beta1.MigrationPhaseExporting, getXNSMigration(t, r, migration).Status.Phase)
}

func TestVMMigrationConsumer_TargetNamespaceGrantIsEvaluatedFirst(t *testing.T) {
	// An ungranted target namespace is reported as such (#340), even though
	// the target Provider does not select it either.
	migration, objs := xnsMigration(xnsTarget)
	tgt, ok := objs[2].(*infrav1beta1.Provider)
	require.True(t, ok)
	tgt.Spec.ConsumerNamespaceSelector = nil
	spy := &migrationSpy{}
	r, _ := countedMigrationReconciler(t, spy, append(objs, grantNamespace(xnsTarget, nil), migration)...)

	reconcileMigration(t, r, migration, 4)
	assertMigrationRefused(t, getXNSMigration(t, r, migration), xnsTarget)
	noMigrationSideEffects(t, spy)
}

func TestVMMigrationConsumer_ImportDiskNotIssuedWhenRevokedLive(t *testing.T) {
	ctx := context.Background()
	prov := &capturingMigrationProvider{importResp: contracts.ImportDiskResponse{
		DiskId: "team-b.target-vm-migrated", Path: "/var/lib/libvirt/images/team-b.target-vm-migrated.qcow2",
	}}
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt)
	migration.Spec.Target.Namespace = xnsTarget
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret(),
		grantNamespace(xnsTarget, strPtr("default")))
	revoked := targetProvider.DeepCopy()
	revoked.Spec.ConsumerNamespaceSelector = nil
	r.APIReader = newLiveReader(t, grantNamespace(xnsTarget, strPtr("default")), nil, revoked)

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, consumerNotAllowedRetryInterval, res.RequeueAfter)
	assert.Zero(t, prov.importCalls, "no ImportDisk when the live read shows the Provider no longer shared")
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseImporting, got.Status.Phase)
	requireMigrationConsumerRefused(t, got, consumerKindProvider, "default", "target-provider")
	assert.False(t, r.longOpAlreadyStarted(got, longOpImport), "a refusal must not claim the import guard")

	r.APIReader = newLiveReader(t, grantNamespace(xnsTarget, strPtr("default")), nil, targetProvider)
	_, err = r.handleImportingPhase(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.Equal(t, 1, prov.importCalls)
}

func TestVMMigrationConsumer_CreateNotIssuedForUngrantedClass(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := creatingFixture(xnsTarget)
	r, _ := directionReconciler(t, &capturingMigrationProvider{}, sourceVM, sourceProvider, targetProvider, migration,
		grantNamespace(xnsTarget, strPtr("default")), grantedClass("default", "cls", nil))

	res, err := r.handleCreatingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, consumerNotAllowedRetryInterval, res.RequeueAfter)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infrav1beta1.MigrationPhaseCreating, got.Status.Phase)
	requireMigrationConsumerRefused(t, got, consumerKindVMClass, "default", "cls")
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

func TestVMMigrationConsumer_RefusedMigrationsMapping(t *testing.T) {
	mk := func(name string, phase infrav1beta1.MigrationPhase, refused bool) *infrav1beta1.VMMigration {
		m, _ := xnsMigration(xnsTarget)
		m.Name = name
		m.Status.Phase = phase
		if refused {
			m.Status.Conditions = []metav1.Condition{{Type: infrav1beta1.VMMigrationConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonConsumerNotAllowed}}
		}
		return m
	}
	r, _ := countedMigrationReconciler(t, &migrationSpy{},
		mk("refused", infrav1beta1.MigrationPhaseImporting, true),
		mk("running", infrav1beta1.MigrationPhaseImporting, false),
		mk("done", infrav1beta1.MigrationPhaseReady, true))

	reqs := r.migrationsRefusedAsConsumers(context.Background(), "")
	require.Len(t, reqs, 1)
	assert.Equal(t, "refused", reqs[0].Name)
}
