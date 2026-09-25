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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the cross-namespace target rule for VMClone: the own
// namespace is unchanged; any other namespace must list the clone's namespace
// in AllowedSourceNamespacesAnnotation, or nothing is created, bound or read
// there — and the refusal is re-checked on every reconcile, so a revocation
// stops a clone that is already in flight.

const (
	xnsSource = "team-a"
	xnsTarget = "team-b"
)

// hookedClonerProvider is a Cloner whose Clone runs onClone first — used to
// revoke a grant while the (synchronous) Clone RPC is in flight.
type hookedClonerProvider struct {
	clonerProvider
	onClone func()
}

func (p *hookedClonerProvider) Clone(ctx context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	if p.onClone != nil {
		p.onClone()
	}
	return p.clonerProvider.Clone(ctx, req)
}

var _ contracts.Cloner = (*hookedClonerProvider)(nil)

// xnsCloneScheme registers corev1 (Namespaces, Secrets, PVCs) next to the
// infra types.
func xnsCloneScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := cloneTestScheme(t)
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// xnsClone is a VMClone in team-a whose target lives in targetNamespace.
func xnsClone(targetNamespace string) *infrav1beta1.VMClone {
	return &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-x", Namespace: xnsSource, Generation: 1, UID: "uid-clone-x"},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{
				Name:        "web",
				Namespace:   targetNamespace,
				Labels:      map[string]string{"tenant": "team-a"},
				Annotations: map[string]string{"note": "from team-a"},
			},
		},
	}
}

// xnsSharedProvider and xnsSharedClass are the source VM's Provider and
// VMClass in team-a, shared with every namespace
// (spec.consumerNamespaceSelector: {}), so a granted cross-namespace target —
// which references them from team-b — may use them.
func xnsSharedProvider() *infrav1beta1.Provider {
	p := runningProvider(xnsSource, "prov-1")
	p.Spec.ConsumerNamespaceSelector = &metav1.LabelSelector{}
	return p
}

func xnsSharedClass() *infrav1beta1.VMClass {
	c := smallVMClass(xnsSource)
	c.Name = "src-class"
	c.Spec.ConsumerNamespaceSelector = &metav1.LabelSelector{}
	return c
}

// newXNSCloneReconciler builds a clone reconciler over a fake client holding
// the source VM + (shared) provider and class in team-a, the clone, and extra
// objects.
func newXNSCloneReconciler(t *testing.T, cp contracts.Provider, clone *infrav1beta1.VMClone, extra ...client.Object) (*VMCloneReconciler, *record.FakeRecorder) {
	t.Helper()
	s := xnsCloneScheme(t)
	objs := append([]client.Object{
		xnsSharedProvider(),
		xnsSharedClass(),
		sourceVMWithID(xnsSource, "src-vm", "prov-1", "vm-source-123"),
		clone,
	}, extra...)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}).
		Build()
	rec := record.NewFakeRecorder(100)
	return &VMCloneReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: cp}, Recorder: rec}, rec
}

func reconcileClone(t *testing.T, r *VMCloneReconciler, clone *infrav1beta1.VMClone, times int) reconcile.Result {
	t.Helper()
	var res reconcile.Result
	for i := 0; i < times; i++ {
		var err error
		res, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
		require.NoError(t, err)
	}
	return res
}

func getClone(t *testing.T, r *VMCloneReconciler, clone *infrav1beta1.VMClone) *infrav1beta1.VMClone {
	t.Helper()
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	return got
}

// assertNothingInNamespace fails if any VirtualMachine, Secret or PVC exists
// in ns.
func assertNothingInNamespace(t *testing.T, c client.Client, ns string) {
	t.Helper()
	ctx := context.Background()
	vms := &infrav1beta1.VirtualMachineList{}
	require.NoError(t, c.List(ctx, vms, client.InNamespace(ns)))
	assert.Empty(t, vms.Items, "no VirtualMachine may be created in %s", ns)
	secrets := &corev1.SecretList{}
	require.NoError(t, c.List(ctx, secrets, client.InNamespace(ns)))
	assert.Empty(t, secrets.Items, "no Secret may be created in %s", ns)
	pvcs := &corev1.PersistentVolumeClaimList{}
	require.NoError(t, c.List(ctx, pvcs, client.InNamespace(ns)))
	assert.Empty(t, pvcs.Items, "no PVC may be created in %s", ns)
}

// assertCloneRefused checks the refusal status: Ready=False /
// TargetNamespaceNotAllowed at the current generation, naming both namespaces
// and the annotation.
func assertCloneRefused(t *testing.T, got *infrav1beta1.VMClone) {
	t.Helper()
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, ReasonTargetNamespaceNotAllowed, ready.Reason)
	assert.Equal(t, got.Generation, ready.ObservedGeneration)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
	assert.Equal(t, targetNamespaceNotAllowedMessage(xnsSource, xnsTarget), ready.Message)
	assert.Equal(t, ready.Message, got.Status.Message)
	assert.NotEqual(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase, "a refusal is recoverable, not terminal")
	assert.NotEqual(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
}

// countEvents drains rec and counts the events whose text mentions reason.
func countEvents(rec *record.FakeRecorder, reason string) int {
	n := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				n++
			}
		default:
			return n
		}
	}
}

// TestVMCloneXNS_SameNamespaceUnchanged: an empty target namespace and one
// equal to the clone's own both work with no grant and no Namespace read (the
// scheme has no corev1 types, so a Namespace read would fail the reconcile).
func TestVMCloneXNS_SameNamespaceUnchanged(t *testing.T) {
	for _, target := range []string{"", xnsSource} {
		t.Run("target="+target, func(t *testing.T) {
			s := cloneTestScheme(t) // infra types only: no Namespace can be read
			clone := xnsClone(target)
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
			fc := fake.NewClientBuilder().WithScheme(s).
				WithObjects(runningProvider(xnsSource, "prov-1"), sourceVMWithID(xnsSource, "src-vm", "prov-1", "vm-source-123"), clone).
				WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}).
				Build()
			r := &VMCloneReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: cp}, Recorder: record.NewFakeRecorder(50)}

			reconcileClone(t, r, clone, 4)

			got := getClone(t, r, clone)
			assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
			assert.Equal(t, 1, cp.cloneCnt)
			vm := &infrav1beta1.VirtualMachine{}
			require.NoError(t, fc.Get(context.Background(), client.ObjectKey{Namespace: xnsSource, Name: "web"}, vm))
			assert.Equal(t, "vm-clone-1", vm.Status.ID)
			assert.Equal(t, infrav1beta1.ObjectRef{Name: "prov-1"}, vm.Spec.ProviderRef,
				"a same-namespace target keeps the implicit provider reference")
			assert.Equal(t, infrav1beta1.ObjectRef{Name: "src-class"}, vm.Spec.ClassRef)
		})
	}
}

// TestVMCloneXNS_RefusedWithoutGrant: a target namespace that is missing, has
// no annotation, an empty one, or lists only other namespaces is refused —
// with the same message in every case (a refusal never reveals whether the
// namespace exists) — and nothing is created there, not even a provider-side
// clone.
func TestVMCloneXNS_RefusedWithoutGrant(t *testing.T) {
	cases := map[string][]client.Object{
		"namespace does not exist": nil,
		"no annotation":            {grantNamespace(xnsTarget, nil)},
		"empty annotation":         {grantNamespace(xnsTarget, strPtr(""))},
		"other namespaces only":    {grantNamespace(xnsTarget, strPtr("team-c, team-d"))},
		"wildcard is not a grant":  {grantNamespace(xnsTarget, strPtr("*"))},
		"prefix is not a grant":    {grantNamespace(xnsTarget, strPtr("team"))},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			clone := xnsClone(xnsTarget)
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
			r, rec := newXNSCloneReconciler(t, cp, clone, extra...)

			res := reconcileClone(t, r, clone, 5)

			assert.Equal(t, crossNamespaceRecheckInterval, res.RequeueAfter, "a refusal rechecks slowly, never hot-loops")
			got := getClone(t, r, clone)
			assertCloneRefused(t, got)
			assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
			assert.Zero(t, cp.cloneCnt, "no provider-side clone may be issued for a refused target")
			assertNothingInNamespace(t, r.Client, xnsTarget)
			assert.Equal(t, 1, countEvents(rec, ReasonTargetNamespaceNotAllowed),
				"the Warning event is emitted once, on the transition, not on every recheck")
		})
	}
}

// TestVMCloneXNS_AllowedWithGrant: a target namespace listing the clone's
// namespace (whitespace tolerated) gets the VM, and the VM's provider and
// class references are pinned to the namespace the clone resolved them in.
func TestVMCloneXNS_AllowedWithGrant(t *testing.T) {
	for _, grant := range []string{"team-a", " team-c ,team-a ", "team-a,team-a"} {
		t.Run(grant, func(t *testing.T) {
			clone := xnsClone(xnsTarget)
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
			r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(grant)))

			reconcileClone(t, r, clone, 4)

			got := getClone(t, r, clone)
			assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
			require.NotNil(t, cp.lastClone)
			assert.Equal(t, contracts.ObjectIdentity{Namespace: xnsTarget, Name: "web"}, cp.lastClone.TargetVM)

			vm := &infrav1beta1.VirtualMachine{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: xnsTarget, Name: "web"}, vm))
			assert.Equal(t, "vm-clone-1", vm.Status.ID)
			assert.Equal(t, infrav1beta1.ObjectRef{Name: "prov-1", Namespace: xnsSource}, vm.Spec.ProviderRef,
				"the target must reference the Provider the clone ran on, not a same-named one in its own namespace")
			assert.Equal(t, infrav1beta1.ObjectRef{Name: "src-class", Namespace: xnsSource}, vm.Spec.ClassRef)
		})
	}
}

// TestVMCloneXNS_ClassOverridePinnedCrossNamespace: a class override is read
// from the clone's namespace, so the cross-namespace target references it
// there.
func TestVMCloneXNS_ClassOverridePinnedCrossNamespace(t *testing.T) {
	clone := xnsClone(xnsTarget)
	clone.Spec.Target.ClassRef = &infrav1beta1.LocalObjectReference{Name: "big"}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	big := xnsSharedClass()
	big.Name = "big"
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)), big)

	reconcileClone(t, r, clone, 4)

	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: xnsTarget, Name: "web"}, vm))
	assert.Equal(t, infrav1beta1.ObjectRef{Name: "big", Namespace: xnsSource}, vm.Spec.ClassRef)
}

// TestVMCloneXNS_RecoversWhenGranted: a refused clone proceeds once an
// administrator adds the grant, and does not keep reporting the refusal.
func TestVMCloneXNS_RecoversWhenGranted(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone(xnsTarget)
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	ns := grantNamespace(xnsTarget, nil)
	r, _ := newXNSCloneReconciler(t, cp, clone, ns)

	reconcileClone(t, r, clone, 3)
	assertCloneRefused(t, getClone(t, r, clone))

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: xnsSource}
	require.NoError(t, r.Update(ctx, ns))

	reconcileClone(t, r, clone, 2)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsTarget, Name: "web"}, &infrav1beta1.VirtualMachine{}))
}

// TestVMCloneXNS_RecoversWhenSpecMovesToOwnNamespace: editing the target back
// to the clone's own namespace lifts the refusal without any grant.
func TestVMCloneXNS_RecoversWhenSpecMovesToOwnNamespace(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone(xnsTarget)
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone)

	reconcileClone(t, r, clone, 3)
	assertCloneRefused(t, getClone(t, r, clone))

	latest := getClone(t, r, clone)
	latest.Spec.Target.Namespace = ""
	require.NoError(t, r.Update(ctx, latest))

	reconcileClone(t, r, clone, 2)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsSource, Name: "web"}, &infrav1beta1.VirtualMachine{}))
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

// TestVMCloneXNS_RevokedDuringCloneRPC: the grant is revoked while the
// synchronous Clone RPC runs. The bind step re-checks, so no VirtualMachine is
// created in the target namespace; the provider's target ID is kept so a
// renewed grant resumes the bind instead of cloning again.
func TestVMCloneXNS_RevokedDuringCloneRPC(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone(xnsTarget)
	ns := grantNamespace(xnsTarget, strPtr(xnsSource))
	cp := &hookedClonerProvider{clonerProvider: clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}}
	r, _ := newXNSCloneReconciler(t, cp, clone, ns)
	setGrant := func(value *string) {
		latest := &corev1.Namespace{}
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), latest))
		latest.Annotations = nil
		if value != nil {
			latest.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: *value}
		}
		require.NoError(t, r.Update(ctx, latest))
	}
	cp.onClone = func() { setGrant(nil) }

	reconcileClone(t, r, clone, 3)

	require.Equal(t, 1, cp.cloneCnt)
	got := getClone(t, r, clone)
	assertCloneRefused(t, got)
	assert.Equal(t, infrav1beta1.ClonePhaseCloning, got.Status.Phase, "an in-flight clone keeps its phase")
	assert.Equal(t, "vm-clone-1", got.Status.TargetVMID, "the provider's target ID is kept for a later bind")
	assertNothingInNamespace(t, r.Client, xnsTarget)

	// Re-grant: the bind resumes from the recorded ID, with no second clone.
	cp.onClone = nil
	setGrant(strPtr(xnsSource))
	reconcileClone(t, r, clone, 2)
	assert.Equal(t, 1, cp.cloneCnt, "resuming never re-clones")
	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsTarget, Name: "web"}, vm))
	assert.Equal(t, "vm-clone-1", vm.Status.ID)
}

// TestVMCloneXNS_RevokedWhileTaskInFlight: an async clone whose grant is
// revoked while its task runs stops before polling/binding; nothing is created
// in the target namespace until the grant is back.
func TestVMCloneXNS_RevokedWhileTaskInFlight(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone(xnsTarget)
	ns := grantNamespace(xnsTarget, strPtr(xnsSource))
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1", TaskRef: "task-1"}}
	polls := 0
	cp.IsTaskCompleteFn = func(context.Context, string) (bool, error) { polls++; return true, nil }
	r, _ := newXNSCloneReconciler(t, cp, clone, ns)

	// Finalizer, then the clone is issued and the task is recorded.
	reconcileClone(t, r, clone, 2)
	require.Equal(t, 1, cp.cloneCnt)
	require.Equal(t, "task-1", getClone(t, r, clone).Status.TaskRef)

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Annotations = nil
	require.NoError(t, r.Update(ctx, ns))

	res := reconcileClone(t, r, clone, 3)
	assert.Equal(t, crossNamespaceRecheckInterval, res.RequeueAfter)
	assert.Zero(t, polls, "a refused clone does not even poll its task")
	got := getClone(t, r, clone)
	assertCloneRefused(t, got)
	assert.Equal(t, "task-1", got.Status.TaskRef, "the task is kept for a renewed grant")
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

// TestVMCloneXNS_ReadyCloneUntouchedByRevocation: revoking after the clone is
// Ready changes nothing — already-created objects are kept as they are.
func TestVMCloneXNS_ReadyCloneUntouchedByRevocation(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone(xnsTarget)
	ns := grantNamespace(xnsTarget, strPtr(xnsSource))
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, ns)
	reconcileClone(t, r, clone, 4)
	require.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
	ns.Annotations = nil
	require.NoError(t, r.Update(ctx, ns))
	reconcileClone(t, r, clone, 2)

	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsTarget, Name: "web"}, &infrav1beta1.VirtualMachine{}),
		"the already-created target VM is kept")
}

// TestVMCloneXNS_ExistingTargetNotProbed: without a grant the clone never even
// reads the target namespace, so it cannot be used to learn whether a VM of a
// given name exists there.
func TestVMCloneXNS_ExistingTargetNotProbed(t *testing.T) {
	clone := xnsClone(xnsTarget)
	victim := sourceVMWithID(xnsTarget, "web", "prov-b", "vm-victim")
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, nil), victim)

	reconcileClone(t, r, clone, 3)

	got := getClone(t, r, clone)
	assertCloneRefused(t, got)
	assert.NotContains(t, got.Status.Message, "already exists")
	vm := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(victim), vm))
	assert.Equal(t, "vm-victim", vm.Status.ID, "the victim's VM is untouched")
	assert.Empty(t, vm.Annotations[CloneAnnotationClone])
}

// TestVMCloneXNS_NamespaceWatchMapsOnlyCrossNamespaceClones: a grant change on
// a namespace re-drives exactly the unfinished clones in OTHER namespaces that
// target it.
func TestVMCloneXNS_NamespaceWatchMapsOnlyCrossNamespaceClones(t *testing.T) {
	mk := func(ns, name, target string, phase infrav1beta1.ClonePhase) *infrav1beta1.VMClone {
		c := xnsClone(target)
		c.Namespace, c.Name = ns, name
		c.Status.Phase = phase
		return c
	}
	objs := []client.Object{
		mk(xnsSource, "pending", xnsTarget, infrav1beta1.ClonePhasePending),
		mk(xnsSource, "cloning", xnsTarget, infrav1beta1.ClonePhaseCloning),
		mk(xnsSource, "ready", xnsTarget, infrav1beta1.ClonePhaseReady),
		mk(xnsSource, "failed", xnsTarget, infrav1beta1.ClonePhaseFailed),
		mk(xnsSource, "own-ns", "", infrav1beta1.ClonePhasePending),
		mk(xnsSource, "elsewhere", "team-c", infrav1beta1.ClonePhasePending),
		mk(xnsTarget, "inside-target", xnsTarget, infrav1beta1.ClonePhasePending),
	}
	s := xnsCloneScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&infrav1beta1.VMClone{}).Build()
	r := &VMCloneReconciler{Client: fc, Scheme: s, Recorder: record.NewFakeRecorder(10)}

	reqs := r.clonesTargetingNamespace(context.Background(), grantNamespace(xnsTarget, strPtr(xnsSource)))

	var names []string
	for _, rq := range reqs {
		names = append(names, rq.Namespace+"/"+rq.Name)
	}
	assert.ElementsMatch(t, []string{"team-a/pending", "team-a/cloning"}, names)
}

// TestVMCloneXNS_ReadErrorFailsClosed: when the target Namespace cannot be
// read the reconcile errors (backoff) and nothing is issued.
func TestVMCloneXNS_ReadErrorFailsClosed(t *testing.T) {
	clone := xnsClone(xnsTarget)
	s := cloneTestScheme(t) // no corev1: every Namespace read fails
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	fc := fake.NewClientBuilder().WithScheme(s).
		WithObjects(runningProvider(xnsSource, "prov-1"), sourceVMWithID(xnsSource, "src-vm", "prov-1", "vm-source-123"), clone).
		WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}).
		Build()
	r := &VMCloneReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: cp}, Recorder: record.NewFakeRecorder(10)}
	key := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)}

	_, err := r.Reconcile(context.Background(), key) // finalizer
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), key)
	require.Error(t, err)
	assert.Zero(t, cp.cloneCnt)
	err = fc.Get(context.Background(), client.ObjectKey{Namespace: xnsTarget, Name: "web"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err))
}
