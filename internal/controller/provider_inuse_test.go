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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// These tests pin the Provider in-use finalizer (ADR-0010): a Provider being
// deleted is held, with its runtime kept up, while any VirtualMachine (in any
// namespace), VMSnapshot, VMClone or VMMigration still needs it; it is
// released once nothing does, or at once on force-delete.

const inUseProviderName = "prov-a"

// inUseProvider is a plaintext (tls.enabled=false) Provider in "default" that
// already carries the in-use finalizer, so a reconcile provisions its runtime.
func inUseProvider() *infravirtrigaudiov1beta1.Provider {
	p := providerWithRuntime(inUseProviderName, &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	p.Finalizers = []string{infravirtrigaudiov1beta1.ProviderInUseFinalizer}
	return p
}

// sharedInUseProvider is inUseProvider shared with every namespace
// (spec.consumerNamespaceSelector: {}), so VMs in other namespaces may use it.
func sharedInUseProvider() *infravirtrigaudiov1beta1.Provider {
	p := inUseProvider()
	p.Spec.ConsumerNamespaceSelector = &metav1.LabelSelector{}
	return p
}

// inUseProviderKey is the key of inUseProvider.
var inUseProviderKey = types.NamespacedName{Namespace: "default", Name: inUseProviderName}

// inUseVM is a VirtualMachine ns/name on the Provider providerNS/providerName
// (an empty providerNS meaning the VM's own namespace), carrying the
// VirtualMachine finalizer as a live VM does.
func inUseVM(ns, name, providerNS, providerName string) *infravirtrigaudiov1beta1.VirtualMachine {
	return &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Finalizers: []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer},
		},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: providerName, Namespace: providerNS},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
		},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "vm-" + name},
	}
}

// newInUseReconciler builds a ProviderReconciler over a fake client with the
// field indexes the manager registers, and a recorder for its events.
func newInUseReconciler(t *testing.T, objs ...client.Object) (*ProviderReconciler, *record.FakeRecorder) {
	t.Helper()
	sch := newProviderSAScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithIndex(&infravirtrigaudiov1beta1.VMSnapshot{}, providerInUseIndex, providerInUseIndexValues).
		WithIndex(&infravirtrigaudiov1beta1.VMClone{}, providerInUseIndex, providerInUseIndexValues).
		WithIndex(&infravirtrigaudiov1beta1.VMMigration{}, providerInUseIndex, providerInUseIndexValues).
		Build()
	rec := record.NewFakeRecorder(64)
	return &ProviderReconciler{Client: cli, Scheme: sch, Recorder: rec}, rec
}

// reconcileProvider reconciles the in-use test Provider once.
func reconcileProvider(t *testing.T, r *ProviderReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: inUseProviderKey})
	require.NoError(t, err)
	return res
}

// runProviderRuntime reconciles the test Provider, marks its Deployment ready
// (no deployment controller runs here) and reconciles again, so its runtime
// phase is Running.
func runProviderRuntime(t *testing.T, r *ProviderReconciler) {
	t.Helper()
	ctx := context.Background()
	reconcileProvider(t, r)
	dep := &appsv1.Deployment{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: r.getDeploymentName(inUseProvider())}, dep))
	dep.Status.ReadyReplicas = 1
	require.NoError(t, r.Status().Update(ctx, dep))
	reconcileProvider(t, r)
	require.Equal(t, infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning, getInUseProvider(t, r).Status.Runtime.Phase)
}

// getInUseProvider reads the test Provider; it fails the test if it is gone.
func getInUseProvider(t *testing.T, r *ProviderReconciler) *infravirtrigaudiov1beta1.Provider {
	t.Helper()
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), inUseProviderKey, p))
	return p
}

// providerGone reports whether the test Provider no longer exists.
func providerGone(t *testing.T, r *ProviderReconciler) bool {
	t.Helper()
	err := r.Get(context.Background(), inUseProviderKey, &infravirtrigaudiov1beta1.Provider{})
	if err != nil && !apierrors.IsNotFound(err) {
		require.NoError(t, err)
	}
	return apierrors.IsNotFound(err)
}

// deleteInUseProvider requests the deletion of the test Provider (its
// finalizer keeps it, with a deletionTimestamp).
func deleteInUseProvider(t *testing.T, r *ProviderReconciler) {
	t.Helper()
	require.NoError(t, r.Delete(context.Background(), getInUseProvider(t, r)))
}

// finishVMDelete simulates the VirtualMachine controller finishing a VM's
// delete through the Provider: deletion requested, then its finalizer removed.
func finishVMDelete(t *testing.T, r *ProviderReconciler, ns, name string) {
	t.Helper()
	ctx := context.Background()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vm))
	require.NoError(t, r.Delete(ctx, vm))
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vm))
	vm.Finalizers = nil
	require.NoError(t, r.Update(ctx, vm))
}

// runtimeObjectsExist reports whether the test Provider's Deployment, Service
// and ServiceAccount exist (all three, or none, are expected).
func runtimeObjectsExist(t *testing.T, r *ProviderReconciler) (dep, svc, sa bool) {
	t.Helper()
	ctx := context.Background()
	name := types.NamespacedName{Namespace: "default", Name: r.getDeploymentName(inUseProvider())}
	exists := func(obj client.Object) bool {
		err := r.Get(ctx, name, obj)
		if err != nil && !apierrors.IsNotFound(err) {
			require.NoError(t, err)
		}
		return err == nil
	}
	return exists(&appsv1.Deployment{}), exists(&corev1.Service{}), exists(&corev1.ServiceAccount{})
}

func TestProviderInUse_AddsFinalizerToAnExistingProvider(t *testing.T) {
	// A Provider created before the upgrade has no finalizer.
	legacy := providerWithRuntime(inUseProviderName, &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false})
	r, _ := newInUseReconciler(t, legacy)

	reconcileProvider(t, r)

	got := getInUseProvider(t, r)
	assert.Contains(t, got.Finalizers, infravirtrigaudiov1beta1.ProviderInUseFinalizer)
	dep, svc, sa := runtimeObjectsExist(t, r)
	assert.True(t, dep && svc && sa, "the runtime is provisioned in the same reconcile")
}

func TestProviderInUse_HeldWithRuntimeKeptThenReleasedWhenVMsAreDeleted(t *testing.T) {
	r, rec := newInUseReconciler(t, inUseProvider(),
		inUseVM("default", "web", "", inUseProviderName),
		inUseVM("default", "db", "", inUseProviderName))
	runProviderRuntime(t, r)
	drainEvents(rec)

	deleteInUseProvider(t, r)
	res := reconcileProvider(t, r)
	assert.Equal(t, providerInUseRecheckInterval, res.RequeueAfter, "a held Provider is re-checked on the backstop")

	held := getInUseProvider(t, r)
	assert.Contains(t, held.Finalizers, infravirtrigaudiov1beta1.ProviderInUseFinalizer)
	cond := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonProviderInUse, cond.Reason)
	assert.Equal(t, held.Generation, cond.ObservedGeneration)
	assert.Contains(t, cond.Message, "2 VirtualMachine(s)")
	assert.Contains(t, cond.Message, "namespace(s) default (2)")
	assert.NotContains(t, cond.Message, "web", "the message names no object")
	assert.Equal(t, infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning, held.Status.Runtime.Phase,
		"the runtime stays Running so the VMs can still be deleted through it")
	dep, svc, sa := runtimeObjectsExist(t, r)
	assert.True(t, dep && svc && sa, "the runtime is kept while the Provider is held")

	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning "+eventReasonProviderDeleteBlocked)

	// One VM is deleted through the Provider: still held, the count drops.
	finishVMDelete(t, r, "default", "web")
	reconcileProvider(t, r)
	cond = meta.FindStatusCondition(getInUseProvider(t, r).Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "1 VirtualMachine(s)")
	assert.Empty(t, drainEvents(rec), "a hold that continues records no new event")

	// The last one goes: the Provider is released and its runtime torn down.
	finishVMDelete(t, r, "default", "db")
	res = reconcileProvider(t, r)
	assert.Zero(t, res.RequeueAfter)
	assert.True(t, providerGone(t, r), "the Provider is released once nothing uses it")
	dep, svc, sa = runtimeObjectsExist(t, r)
	assert.False(t, dep || svc || sa, "its runtime is torn down on release")
}

func TestProviderInUse_RepeatedHoldWritesNothing(t *testing.T) {
	r, rec := newInUseReconciler(t, inUseProvider(), inUseVM("default", "web", "", inUseProviderName))
	runProviderRuntime(t, r)
	deleteInUseProvider(t, r)
	reconcileProvider(t, r)
	drainEvents(rec)
	before := getInUseProvider(t, r)

	for i := 0; i < 3; i++ {
		reconcileProvider(t, r)
	}

	after := getInUseProvider(t, r)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a repeated hold writes nothing to the Provider")
	assert.Empty(t, drainEvents(rec), "a repeated hold records no event")
}

func TestProviderInUse_CrossNamespaceConsumerHolds(t *testing.T) {
	r, _ := newInUseReconciler(t, sharedInUseProvider(), inUseVM("team-b", "api", "default", inUseProviderName))
	runProviderRuntime(t, r)

	deleteInUseProvider(t, r)
	reconcileProvider(t, r)

	held := getInUseProvider(t, r)
	cond := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, cond, "a VM in another namespace holds the Provider like one in its own")
	assert.Contains(t, cond.Message, "namespace(s) team-b (1)")
	assert.NotContains(t, cond.Message, "api")

	finishVMDelete(t, r, "team-b", "api")
	reconcileProvider(t, r)
	assert.True(t, providerGone(t, r))
}

func TestProviderInUse_ForceDeleteReleasesWithWarning(t *testing.T) {
	r, rec := newInUseReconciler(t, sharedInUseProvider(),
		inUseVM("default", "web", "", inUseProviderName),
		inUseVM("team-b", "api", "default", inUseProviderName))
	runProviderRuntime(t, r)
	deleteInUseProvider(t, r)
	reconcileProvider(t, r)
	drainEvents(rec)

	p := getInUseProvider(t, r)
	p.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(context.Background(), p))
	reconcileProvider(t, r)

	assert.True(t, providerGone(t, r), "force-delete releases the Provider at once")
	dep, svc, sa := runtimeObjectsExist(t, r)
	assert.False(t, dep || svc || sa, "its runtime is torn down")
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning "+eventReasonProviderForceDeleted)
	assert.Contains(t, events[0], "2 VirtualMachine(s)")
	assert.Contains(t, events[0], "namespace(s) default (1), team-b (1)")

	// The VMs are not touched.
	for _, key := range []types.NamespacedName{{Namespace: "default", Name: "web"}, {Namespace: "team-b", Name: "api"}} {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		require.NoError(t, r.Get(context.Background(), key, vm))
		assert.True(t, vm.DeletionTimestamp.IsZero())
	}
}

func TestProviderInUse_ForceDeleteOfAnUnusedProviderRecordsNoWarning(t *testing.T) {
	r, rec := newInUseReconciler(t, inUseProvider())
	runProviderRuntime(t, r)
	drainEvents(rec)
	p := getInUseProvider(t, r)
	p.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(context.Background(), p))
	deleteInUseProvider(t, r)

	reconcileProvider(t, r)

	assert.True(t, providerGone(t, r))
	assert.Empty(t, drainEvents(rec), "nothing was left unmanaged")
}

// TestProviderInUse_ForceDeleteWorksWhenTheCheckCannotRun pins that the escape
// hatch never depends on the in-use lookup: without the field indexes the
// check fails, a plain delete is retried, and force-delete still releases.
func TestProviderInUse_ForceDeleteWorksWhenTheCheckCannotRun(t *testing.T) {
	ctx := context.Background()
	sch := newProviderSAScheme(t)
	p := inUseProvider()
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(p).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).Build() // no indexes
	rec := record.NewFakeRecorder(8)
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: rec}
	deleteInUseProvider(t, r)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: inUseProviderKey})
	require.Error(t, err, "a check that cannot run never releases the Provider")
	assert.False(t, providerGone(t, r))

	got := getInUseProvider(t, r)
	got.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	require.NoError(t, r.Update(ctx, got))
	reconcileProvider(t, r)
	assert.True(t, providerGone(t, r))
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning "+eventReasonProviderForceDeleted)
	assert.Contains(t, events[0], "could not be checked")
}

// TestProviderInUse_OnlyAllowedNamespacesHold pins that an object in another
// namespace holds the Provider only when the Provider would serve it: a VM bound
// through it, or an object in a namespace its consumerNamespaceSelector allows
// now. A refused (ungranted) VM or migration neither holds it nor shows up in
// its DeleteBlocked message.
func TestProviderInUse_OnlyAllowedNamespacesHold(t *testing.T) {
	selective := func() *infravirtrigaudiov1beta1.Provider {
		p := inUseProvider()
		p.Spec.ConsumerNamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tenant": "granted"}}
		return p
	}
	namespaces := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "granted", Labels: map[string]string{"tenant": "granted"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "outsider"}},
	}
	bound := func(vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
		vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: inUseProviderName}
		return vm
	}
	// unbound leaves status.boundProvider unset (status.id stays set, so only
	// the grant decides whether the VM holds the Provider).
	unbound := func(vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
		vm.Status.BoundProvider = nil
		return vm
	}
	lingeringUnbound := func(ns, name string) *infravirtrigaudiov1beta1.VirtualMachine {
		// Being deleted, its VirtualMachine finalizer gone: only its snapshot
		// could still need the Provider.
		now := metav1.Now()
		vm := unbound(inUseVM(ns, name, "default", inUseProviderName))
		vm.Finalizers = []string{"example.com/someone-else"}
		vm.DeletionTimestamp = &now
		return vm
	}
	migrationInto := func(ns string) *infravirtrigaudiov1beta1.VMMigration {
		return &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns, Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src"}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "dst",
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: inUseProviderName, Namespace: "default"}},
			},
			Status: infravirtrigaudiov1beta1.VMMigrationStatus{Phase: infravirtrigaudiov1beta1.MigrationPhaseImporting},
		}
	}
	cases := []struct {
		name     string
		provider *infravirtrigaudiov1beta1.Provider
		obj      client.Object
		extra    []client.Object
		holds    bool
		want     string
	}{
		{name: "an unallowed, unbound VM (no selector)", provider: inUseProvider(),
			obj: unbound(inUseVM("outsider", "squat", "default", inUseProviderName))},
		{name: "an unallowed, unbound VM (selector does not match)", provider: selective(),
			obj: unbound(inUseVM("outsider", "squat", "default", inUseProviderName))},
		{name: "an unallowed snapshot follows its unallowed VM", provider: inUseProvider(),
			obj: lingeringUnbound("outsider", "squat"),
			extra: []client.Object{&infravirtrigaudiov1beta1.VMSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "outsider", Finalizers: []string{infravirtrigaudiov1beta1.VMSnapshotFinalizer}},
				Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "squat"}},
			}}},
		{name: "an unallowed migration into the Provider", provider: selective(), obj: migrationInto("outsider")},
		{name: "an allowed VM", provider: selective(),
			obj: unbound(inUseVM("granted", "api", "default", inUseProviderName)), holds: true, want: "namespace(s) granted (1)"},
		{name: "a VM bound through the Provider, though no longer allowed", provider: inUseProvider(),
			obj: bound(inUseVM("outsider", "legacy", "default", inUseProviderName)), holds: true, want: "namespace(s) outsider (1)"},
		{name: "an allowed migration into the Provider", provider: selective(), obj: migrationInto("granted"),
			holds: true, want: "1 VMMigration(s), in namespace(s) granted (1)"},
		{name: "a migration into the Provider from its own namespace", provider: inUseProvider(), obj: migrationInto("default"),
			holds: true, want: "1 VMMigration(s)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(append([]client.Object{tc.provider, tc.obj}, tc.extra...), namespaces...)
			r, _ := newInUseReconciler(t, objs...)
			deleteInUseProvider(t, r)
			reconcileProvider(t, r)
			if !tc.holds {
				assert.True(t, providerGone(t, r), "a namespace that may not use the Provider cannot hold it")
				return
			}
			cond := meta.FindStatusCondition(getInUseProvider(t, r).Status.Conditions, k8s.ConditionDeleteBlocked)
			require.NotNil(t, cond)
			assert.Contains(t, cond.Message, tc.want)
		})
	}
}

// TestProviderInUse_NamespaceReadForbiddenStillHolds pins the namespace-scoped
// RBAC case: a manager that may not read Namespaces treats the runtime as
// keepable, so a held Provider still shows DeleteBlocked/ProviderInUse instead
// of failing every reconcile, and says so in its log once, not every time.
func TestProviderInUse_NamespaceReadForbiddenStillHolds(t *testing.T) {
	sch := newProviderSAScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inUseProvider(), inUseVM("default", "web", "", inUseProviderName)).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithIndex(&infravirtrigaudiov1beta1.VMSnapshot{}, providerInUseIndex, providerInUseIndexValues).
		WithIndex(&infravirtrigaudiov1beta1.VMClone{}, providerInUseIndex, providerInUseIndexValues).
		WithIndex(&infravirtrigaudiov1beta1.VMMigration{}, providerInUseIndex, providerInUseIndexValues).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return apierrors.NewForbidden(corev1.Resource("namespaces"), key.Name, fmt.Errorf("namespace-scoped RBAC"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &ProviderReconciler{Client: cli, Scheme: sch, Recorder: record.NewFakeRecorder(16)}

	var forbiddenLogs int
	logger := funcr.New(func(_, args string) {
		if strings.Contains(args, "Cannot read the Provider's Namespace") {
			forbiddenLogs++
		}
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), logger)

	deleteInUseProvider(t, r)
	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: inUseProviderKey})
		require.NoError(t, err, "a forbidden Namespace read never fails the reconcile")
	}

	cond := meta.FindStatusCondition(getInUseProvider(t, r).Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, reasonProviderInUse, cond.Reason)
	assert.Equal(t, 1, forbiddenLogs, "logged once, not on every reconcile")
}

// TestProviderInUse_UnboundVMHoldsOnlyIfItMayExistOnTheHypervisor pins the
// rule for an unbound VM (no status.boundProvider): it holds the Provider only
// with a provider id, a pending clustered create, or the CreateRequested
// marker. One held before any Create (e.g. ProviderDeleting) has nothing to
// delete through the Provider and must not hold it, or the two deletions would
// wait on each other.
func TestProviderInUse_UnboundVMHoldsOnlyIfItMayExistOnTheHypervisor(t *testing.T) {
	neverCreated := func() *infravirtrigaudiov1beta1.VirtualMachine {
		vm := inUseVM("default", "web", "", inUseProviderName)
		vm.Status.ID = ""
		vm.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionProvisioning, Status: metav1.ConditionFalse,
			Reason: "ProviderDeleting", Message: "held", LastTransitionTime: metav1.Now()}}
		return vm
	}
	cases := map[string]struct {
		vm    func() *infravirtrigaudiov1beta1.VirtualMachine
		holds bool
	}{
		"no id, pendingHost or marker": {vm: neverCreated},
		"status.id set": {holds: true, vm: func() *infravirtrigaudiov1beta1.VirtualMachine {
			vm := neverCreated()
			vm.Status.ID = "vm-123"
			return vm
		}},
		"status.placement.pendingHost set": {holds: true, vm: func() *infravirtrigaudiov1beta1.VirtualMachine {
			vm := neverCreated()
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "host-a"}
			return vm
		}},
		"Provisioning reason CreateRequested": {holds: true, vm: func() *infravirtrigaudiov1beta1.VirtualMachine {
			vm := neverCreated()
			vm.Status.Conditions[0].Reason = k8s.ReasonCreateRequested
			return vm
		}},
		"bound, without any of them": {holds: true, vm: func() *infravirtrigaudiov1beta1.VirtualMachine {
			vm := neverCreated()
			vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: inUseProviderName}
			return vm
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := newInUseReconciler(t, inUseProvider(), tc.vm())
			deleteInUseProvider(t, r)
			reconcileProvider(t, r)
			assert.Equal(t, !tc.holds, providerGone(t, r))
		})
	}
}

// TestProviderInUse_WhatHolds pins ADR-0010 D2, case by case.
func TestProviderInUse_WhatHolds(t *testing.T) {
	now := metav1.Now()
	lingering := func(name string) *infravirtrigaudiov1beta1.VirtualMachine {
		// Being deleted, its VirtualMachine finalizer gone: only another
		// controller's finalizer keeps the object.
		vm := inUseVM("default", name, "", inUseProviderName)
		vm.Finalizers = []string{"example.com/someone-else"}
		vm.DeletionTimestamp = &now
		return vm
	}
	boundThrough := func(vm *infravirtrigaudiov1beta1.VirtualMachine, providerName string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: providerName}
		return vm
	}
	snapshot := func(vmName string, finalizer bool) *infravirtrigaudiov1beta1.VMSnapshot {
		s := &infravirtrigaudiov1beta1.VMSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-" + vmName, Namespace: "default"},
			Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: vmName}},
		}
		if finalizer {
			s.Finalizers = []string{infravirtrigaudiov1beta1.VMSnapshotFinalizer}
		}
		return s
	}
	clone := func(vmName string, phase infravirtrigaudiov1beta1.ClonePhase) *infravirtrigaudiov1beta1.VMClone {
		return &infravirtrigaudiov1beta1.VMClone{
			ObjectMeta: metav1.ObjectMeta{Name: "clone-" + vmName + "-" + strings.ToLower(string(phase)), Namespace: "default",
				Finalizers: []string{infravirtrigaudiov1beta1.VMCloneFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMCloneSpec{
				Source: infravirtrigaudiov1beta1.CloneSource{VMRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: vmName}},
				Target: infravirtrigaudiov1beta1.VMCloneTarget{Name: "copy"},
			},
			Status: infravirtrigaudiov1beta1.VMCloneStatus{Phase: phase},
		}
	}
	migration := func(name, sourceVM string, phase infravirtrigaudiov1beta1.MigrationPhase, deleting bool) *infravirtrigaudiov1beta1.VMMigration {
		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: sourceVM}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "moved",
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: inUseProviderName}},
			},
			Status: infravirtrigaudiov1beta1.VMMigrationStatus{Phase: phase},
		}
		if deleting {
			m.DeletionTimestamp = &now
		}
		return m
	}

	cases := []struct {
		name  string
		objs  []client.Object
		holds bool
		want  string
	}{
		{name: "a live VM", objs: []client.Object{inUseVM("default", "web", "", inUseProviderName)}, holds: true, want: "1 VirtualMachine(s)"},
		{name: "a VM being deleted with its finalizer", objs: []client.Object{func() client.Object {
			vm := inUseVM("default", "web", "", inUseProviderName)
			vm.DeletionTimestamp = &now
			return vm
		}()}, holds: true, want: "1 VirtualMachine(s)"},
		{name: "a VM being deleted whose finalizer is gone", objs: []client.Object{lingering("web")}, holds: false},
		{name: "a VM of another Provider", objs: []client.Object{inUseVM("default", "web", "", "prov-b")}, holds: false},
		{name: "spec names this Provider but bound through another",
			objs: []client.Object{boundThrough(inUseVM("default", "web", "", inUseProviderName), "prov-b")}, holds: false},
		{name: "bound through this Provider, spec names another",
			objs: []client.Object{boundThrough(inUseVM("default", "web", "", "prov-b"), inUseProviderName)}, holds: true, want: "1 VirtualMachine(s)"},
		{name: "a snapshot with a delete to run, of a lingering VM",
			objs: []client.Object{lingering("web"), snapshot("web", true)}, holds: true, want: "1 VMSnapshot(s)"},
		{name: "a snapshot without its finalizer", objs: []client.Object{lingering("web"), snapshot("web", false)}, holds: false},
		{name: "a snapshot of a VM of another Provider",
			objs: []client.Object{inUseVM("default", "other", "", "prov-b"), snapshot("other", true)}, holds: false},
		{name: "a clone in flight", objs: []client.Object{lingering("web"), clone("web", infravirtrigaudiov1beta1.ClonePhaseCloning)},
			holds: true, want: "1 VMClone(s)"},
		{name: "a finished clone", objs: []client.Object{lingering("web"), clone("web", infravirtrigaudiov1beta1.ClonePhaseReady)}, holds: false},
		{name: "a migration in flight into this Provider",
			objs:  []client.Object{migration("mig", "elsewhere", infravirtrigaudiov1beta1.MigrationPhaseImporting, false)},
			holds: true, want: "1 VMMigration(s)"},
		{name: "a finished migration into this Provider",
			objs: []client.Object{migration("mig", "elsewhere", infravirtrigaudiov1beta1.MigrationPhaseReady, false)}, holds: false},
		{name: "a finished migration being deleted (its cleanup runs)",
			objs:  []client.Object{migration("mig", "elsewhere", infravirtrigaudiov1beta1.MigrationPhaseReady, true)},
			holds: true, want: "1 VMMigration(s)"},
		{name: "a migration out of a lingering VM of this Provider, counted once",
			objs: []client.Object{lingering("web"),
				migration("mig", "web", infravirtrigaudiov1beta1.MigrationPhaseExporting, false)},
			holds: true, want: "in use by 1 VMMigration(s), in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newInUseReconciler(t, append([]client.Object{inUseProvider()}, tc.objs...)...)
			deleteInUseProvider(t, r)
			reconcileProvider(t, r)
			if !tc.holds {
				assert.True(t, providerGone(t, r), "nothing holds the Provider")
				return
			}
			cond := meta.FindStatusCondition(getInUseProvider(t, r).Status.Conditions, k8s.ConditionDeleteBlocked)
			require.NotNil(t, cond)
			assert.Contains(t, cond.Message, tc.want)
		})
	}
}

func TestProviderInUse_NamespaceBeingDeletedLeavesTheRuntimeAlone(t *testing.T) {
	now := metav1.Now()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default", DeletionTimestamp: &now, Finalizers: []string{"example.com/hold"}},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
	}
	p := inUseProvider()
	p.DeletionTimestamp = &now
	// The runtime was Running when the namespace deletion began; the
	// namespace controller has since deleted its objects.
	p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{
		Mode: infravirtrigaudiov1beta1.RuntimeModeRemote, Phase: infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning,
		Endpoint: "virtrigaud-provider-default-prov-a.default.svc.cluster.local:9443",
	}
	r, rec := newInUseReconciler(t, ns, p, inUseVM("default", "web", "", inUseProviderName))

	res := reconcileProvider(t, r)
	assert.Equal(t, providerInUseRecheckInterval, res.RequeueAfter)

	held := getInUseProvider(t, r)
	cond := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, reasonProviderInUseRuntimeNotKept, cond.Reason)
	assert.Contains(t, cond.Message, "namespace is being deleted")
	assert.Equal(t, infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning, held.Status.Runtime.Phase,
		"status.runtime is left as it was, so VM deletes keep trying the provider while its pod answers")
	dep, svc, sa := runtimeObjectsExist(t, r)
	assert.False(t, dep || svc || sa, "nothing is created in a namespace being deleted")
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "Warning "+eventReasonProviderDeleteBlocked)

	// The VM is deleted through the provider while its pod still answers.
	finishVMDelete(t, r, "default", "web")
	reconcileProvider(t, r)
	assert.True(t, providerGone(t, r), "the Provider goes once its VMs are gone")
}

func TestProviderInUse_ForegroundDeletionDoesNotRecreateTheRuntime(t *testing.T) {
	now := metav1.Now()
	p := inUseProvider()
	p.Finalizers = append(p.Finalizers, metav1.FinalizerDeleteDependents)
	p.DeletionTimestamp = &now
	r, _ := newInUseReconciler(t, p, inUseVM("default", "web", "", inUseProviderName))

	reconcileProvider(t, r)

	cond := meta.FindStatusCondition(getInUseProvider(t, r).Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, cond)
	assert.Equal(t, reasonProviderInUseRuntimeNotKept, cond.Reason)
	assert.Contains(t, cond.Message, "foreground cascading deletion")
	dep, svc, sa := runtimeObjectsExist(t, r)
	assert.False(t, dep || svc || sa, "the runtime is not re-created against the garbage collector")
}

func TestProviderInUse_ProviderWithoutTheFinalizerIsCleanedUpAsBefore(t *testing.T) {
	r, _ := newInUseReconciler(t, inUseProvider(), inUseVM("default", "web", "", inUseProviderName))
	runProviderRuntime(t, r)
	// Deleted before the upgrade: only someone else's finalizer keeps it.
	p := getInUseProvider(t, r)
	p.Finalizers = []string{"example.com/someone-else"}
	require.NoError(t, r.Update(context.Background(), p))
	deleteInUseProvider(t, r)

	reconcileProvider(t, r)

	got := getInUseProvider(t, r)
	assert.Equal(t, []string{"example.com/someone-else"}, got.Finalizers)
	dep, svc, sa := runtimeObjectsExist(t, r)
	assert.False(t, dep || svc || sa, "its runtime is torn down, as before")
}

func TestProviderInUse_MessageBoundsNamespacesAndIsStable(t *testing.T) {
	var u providerUsage
	for i := 0; i < providerInUseListedNamespaces+2; i++ {
		u.add(fmt.Sprintf("tenant-%02d", i), &u.vms)
	}
	u.add("tenant-00", &u.snapshots)

	msg := providerInUseMessage(reasonProviderInUse, false, u)
	assert.Contains(t, msg, "12 VirtualMachine(s) and 1 VMSnapshot(s)")
	assert.Contains(t, msg, "tenant-00 (2), tenant-01 (1)")
	assert.Contains(t, msg, "tenant-09 (1) and 2 more namespace(s)")
	assert.NotContains(t, msg, "tenant-10")
	assert.Contains(t, msg, forceDeleteAnnotation+"=true")
	assert.Equal(t, msg, providerInUseMessage(reasonProviderInUse, false, u), "the message depends only on the usage")
	assert.Less(t, len(msg), 2048)
}

func TestProviderInUse_WatchesEnqueueOnlyAProviderBeingDeleted(t *testing.T) {
	ctx := context.Background()
	vm := inUseVM("team-b", "api", "default", inUseProviderName)
	snap := &infravirtrigaudiov1beta1.VMSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "team-b"},
		Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "api"}},
	}
	mig := &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "team-c"},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src"}},
			Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "dst",
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: inUseProviderName, Namespace: "default"}},
		},
	}
	r, _ := newInUseReconciler(t, inUseProvider(), vm, snap, mig)
	want := []ctrl.Request{{NamespacedName: inUseProviderKey}}

	assert.Empty(t, r.providersForInUseVM(ctx, vm), "a live Provider is not re-driven")
	assert.Empty(t, r.providersForInUseDependent(ctx, snap))

	deleteInUseProvider(t, r)
	assert.Equal(t, want, r.providersForInUseVM(ctx, vm))
	assert.Equal(t, want, r.providersForInUseDependent(ctx, snap), "a snapshot maps through its VM")
	assert.Equal(t, want, r.providersForInUseDependent(ctx, mig), "a migration maps to its target Provider")
}

func TestProviderInUse_WatchPredicate(t *testing.T) {
	pred := providerUseChanged()
	vm := inUseVM("default", "web", "", inUseProviderName)

	statusOnly := vm.DeepCopy()
	statusOnly.Status.Message = "busy"
	statusOnly.Annotations = map[string]string{"example.com/poke": "1"}
	assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: vm, ObjectNew: statusOnly}),
		"a status or metadata write that does not change what the VM holds re-drives nothing")

	now := metav1.Now()
	released := vm.DeepCopy()
	released.DeletionTimestamp = &now
	released.Finalizers = []string{"example.com/someone-else"}
	assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: vm, ObjectNew: released}))
	assert.True(t, pred.Delete(event.DeleteEvent{Object: vm}))

	clone := &infravirtrigaudiov1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default", Finalizers: []string{infravirtrigaudiov1beta1.VMCloneFinalizer}},
		Spec: infravirtrigaudiov1beta1.VMCloneSpec{
			Source: infravirtrigaudiov1beta1.CloneSource{VMRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "web"}},
		},
		Status: infravirtrigaudiov1beta1.VMCloneStatus{Phase: infravirtrigaudiov1beta1.ClonePhaseCloning},
	}
	done := clone.DeepCopy()
	done.Status.Phase = infravirtrigaudiov1beta1.ClonePhaseReady
	assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: clone, ObjectNew: done}), "a clone that finishes stops holding")
}

func TestProviderInUse_BackstopIsNotATightLoop(t *testing.T) {
	assert.GreaterOrEqual(t, providerInUseRecheckInterval, time.Minute,
		"the watches re-drive a held Provider; the backstop must not poll")
}
