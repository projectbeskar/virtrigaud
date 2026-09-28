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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a CLUSTERED clone that fails for good (terminal Failed
// phase) removes the target VirtualMachine it created before its Clone RPC —
// and nothing else: never a VM the clone did not create, never a target that
// got bound, never while the clone is only retrying or waiting, never a
// single-host clone's target.

// targetGone reports whether the clone's target VM no longer exists.
func targetGone(t *testing.T, r *VMCloneReconciler) bool {
	t.Helper()
	err := r.Get(context.Background(), targetKey, &infrav1beta1.VirtualMachine{})
	if apierrors.IsNotFound(err) {
		return true
	}
	require.NoError(t, err)
	return false
}

// recordedEvents drains the reconciler's fake recorder.
func recordedEvents(t *testing.T, r *VMCloneReconciler) []string {
	t.Helper()
	var out []string
	rec, ok := r.Recorder.(*record.FakeRecorder)
	require.True(t, ok, "the test reconciler records into a FakeRecorder")
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestVMClone_Clustered_TerminalFailureRemovesTheTargetItCreated(t *testing.T) {
	cp := &clonerProvider{cloneErr: errors.New("clone: failed to clone VM on host \"host-alpha\"")}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)

	reconcileClone(t, r, clone, 2)
	require.Equal(t, 1, cp.cloneCnt)
	require.Equal(t, infrav1beta1.ClonePhaseFailed, getClone(t, r, clone).Status.Phase)
	// markFailed persisted the phase; the Failed reconcile removes the target.
	reconcileClone(t, r, clone, 1)

	assert.True(t, targetGone(t, r), "the target VM this clone created is deleted: its finalizer cleans up on the pending host")
	var removed string
	for _, e := range recordedEvents(t, r) {
		if strings.Contains(e, cloneReasonTargetRemoved) {
			removed = e
		}
	}
	assert.Contains(t, removed, "default/clone-c-target", "an event on the VMClone says the target was removed")
	assert.Contains(t, removed, "host-alpha")

	reconcileClone(t, r, clone, 1)
	assert.Equal(t, 1, cp.cloneCnt, "a failed clone is never retried")
}

// TestVMClone_Clustered_NonTerminalStatesKeepTheTarget: a retryable answer, a
// host that could not be reached, and a name conflict that excludes the host
// all leave the clone Pending (retried or blocked) — its target is kept.
func TestVMClone_Clustered_NonTerminalStatesKeepTheTarget(t *testing.T) {
	for name, err := range map[string]error{
		"retryable (copy in progress)": contracts.NewRetryableError(`clone: the same copy is still running`, nil),
		"host unavailable":             contracts.NewHostUnavailableError(`clone: host "host-alpha" is unreachable`, nil),
		"name conflict (host blocked)": contracts.NewConflictError(`clone: domain already exists`, nil),
	} {
		t.Run(name, func(t *testing.T) {
			cp := &clonerProvider{cloneErr: err}
			r, clone := clusteredCloneFixture(t, boundSource(), cp)
			reconcileClone(t, r, clone, 4)

			assert.Equal(t, infrav1beta1.ClonePhasePending, getClone(t, r, clone).Status.Phase)
			assert.False(t, targetGone(t, r), "a clone that is only retrying or waiting keeps its target")
		})
	}
}

// failedCloneWithTarget is a clustered VMClone already in the terminal Failed
// phase, with target as the VM under its target name.
func failedCloneWithTarget(t *testing.T, target *infrav1beta1.VirtualMachine, topology string, opts ...func(*interceptor.Funcs)) (*VMCloneReconciler, *infrav1beta1.VMClone) {
	t.Helper()
	prov := runningProvider("default", "prov-c")
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	prov.Spec.Topology = topology
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-c", Namespace: "default", UID: "uid-clone-c", Finalizers: []string{vmCloneFinalizer}},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-c"}},
			Target: infrav1beta1.VMCloneTarget{Name: targetKey.Name},
		},
		Status: infrav1beta1.VMCloneStatus{Phase: infrav1beta1.ClonePhaseFailed, TargetUID: "uid-target"},
	}
	funcs := interceptor.Funcs{}
	for _, o := range opts {
		o(&funcs)
	}
	s := cloneTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(prov, target, clone).
		WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}).
		WithInterceptorFuncs(funcs).Build()
	return &VMCloneReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: &clonerProvider{}},
		Recorder: record.NewFakeRecorder(20)}, clone
}

// cloneTargetVM is the clone's target VM: created by clone uid (a marker) with
// the given status.
func cloneTargetVM(cloneUID string, st infrav1beta1.VirtualMachineStatus) *infrav1beta1.VirtualMachine {
	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: targetKey.Name, Namespace: targetKey.Namespace, UID: "uid-target"},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "src-class"},
		},
		Status: st,
	}
	if cloneUID != "" {
		vm.Labels = map[string]string{AdoptedLabel: AdoptedLabelValue}
		vm.Annotations = map[string]string{CloneAnnotationCloneUID: cloneUID}
	}
	return vm
}

func TestVMClone_FailedCloneRemovesOnlyItsOwnUnboundClusteredTarget(t *testing.T) {
	pending := infrav1beta1.VirtualMachineStatus{Placement: &infrav1beta1.PlacementStatus{PendingHost: "host-alpha"}}
	for name, tc := range map[string]struct {
		target   *infrav1beta1.VirtualMachine
		topology string
		removed  bool
	}{
		"its own unbound target, pending on the host": {cloneTargetVM("uid-clone-c", pending), infrav1beta1.ProviderTopologyCluster, true},
		"its own target, created before the pending host was recorded": {
			cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{}), infrav1beta1.ProviderTopologyCluster, true},
		"a VM the clone did not create": {cloneTargetVM("", pending), infrav1beta1.ProviderTopologyCluster, false},
		"a VM another clone created":    {cloneTargetVM("uid-other-clone", pending), infrav1beta1.ProviderTopologyCluster, false},
		"its own target, bound anyway (the clone succeeded, its status write was lost)": {
			cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{ID: "default.clone-c-target",
				Placement: &infrav1beta1.PlacementStatus{Host: "host-alpha"}}), infrav1beta1.ProviderTopologyCluster, false},
		"its own target with an id but no placement yet": {
			cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{ID: "default.clone-c-target"}), infrav1beta1.ProviderTopologyCluster, false},
		"a single-host clone's target": {cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{}), infrav1beta1.ProviderTopologySingle, false},
	} {
		t.Run(name, func(t *testing.T) {
			r, clone := failedCloneWithTarget(t, tc.target, tc.topology)
			reconcileClone(t, r, clone, 1)
			assert.Equal(t, tc.removed, targetGone(t, r))
			assert.Equal(t, infrav1beta1.ClonePhaseFailed, getClone(t, r, clone).Status.Phase, "the clone stays Failed")
		})
	}
}

// TestVMClone_FailedCloneNeverDeletesATargetBoundMeanwhile: the target is
// bound between the check and the delete (a racing bind). The delete carries
// the checked resourceVersion, so it is refused, and the re-check keeps the
// now-bound target.
func TestVMClone_FailedCloneNeverDeletesATargetBoundMeanwhile(t *testing.T) {
	bindFirst := func(f *interceptor.Funcs) {
		f.Delete = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			vm := &infrav1beta1.VirtualMachine{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), vm))
			vm.Status.ID = "default.clone-c-target"
			vm.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha"}
			require.NoError(t, c.Status().Update(ctx, vm))
			return c.Delete(ctx, obj, opts...)
		}
	}
	target := cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{Placement: &infrav1beta1.PlacementStatus{PendingHost: "host-alpha"}})
	r, clone := failedCloneWithTarget(t, target, infrav1beta1.ProviderTopologyCluster, bindFirst)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
	require.NoError(t, err)
	assert.True(t, res.Requeue, "a delete refused by the precondition is checked again")
	assert.False(t, targetGone(t, r), "a target bound meanwhile is never deleted")

	// The re-check sees the binding and leaves it.
	r.Client = fakeWithoutInterceptors(t, r)
	reconcileClone(t, r, clone, 1)
	assert.False(t, targetGone(t, r))
}

// fakeWithoutInterceptors copies r's objects into a plain fake client.
func fakeWithoutInterceptors(t *testing.T, r *VMCloneReconciler) client.Client {
	t.Helper()
	ctx := context.Background()
	var objs []client.Object
	vms := &infrav1beta1.VirtualMachineList{}
	require.NoError(t, r.List(ctx, vms))
	for i := range vms.Items {
		objs = append(objs, &vms.Items[i])
	}
	clones := &infrav1beta1.VMCloneList{}
	require.NoError(t, r.List(ctx, clones))
	for i := range clones.Items {
		objs = append(objs, &clones.Items[i])
	}
	provs := &infrav1beta1.ProviderList{}
	require.NoError(t, r.List(ctx, provs))
	for i := range provs.Items {
		objs = append(objs, &provs.Items[i])
	}
	for _, o := range objs {
		o.SetResourceVersion("")
	}
	return fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}).Build()
}

// TestVMClone_FailedCloneRespectsTheTargetNamespaceGrant: a cross-namespace
// target is removed only while its namespace still grants the clone's
// namespace; once the grant is revoked, the clone writes nothing there — it
// does not even delete its own target.
func TestVMClone_FailedCloneRespectsTheTargetNamespaceGrant(t *testing.T) {
	for name, grant := range map[string]*string{"granted": strPtr("default"), "revoked": nil} {
		t.Run(name, func(t *testing.T) {
			target := cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{})
			target.Namespace = "team-b"
			prov := runningProvider("default", "prov-c")
			prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
			prov.Spec.Topology = infrav1beta1.ProviderTopologyCluster
			target.Spec.ProviderRef = infrav1beta1.ObjectRef{Name: "prov-c", Namespace: "default"}
			clone := &infrav1beta1.VMClone{
				ObjectMeta: metav1.ObjectMeta{Name: "clone-c", Namespace: "default", UID: "uid-clone-c", Finalizers: []string{vmCloneFinalizer}},
				Spec: infrav1beta1.VMCloneSpec{
					Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-c"}},
					Target: infrav1beta1.VMCloneTarget{Name: targetKey.Name, Namespace: "team-b"},
				},
				Status: infrav1beta1.VMCloneStatus{Phase: infrav1beta1.ClonePhaseFailed, TargetUID: "uid-target"},
			}
			s := xnsCloneScheme(t)
			fc := fake.NewClientBuilder().WithScheme(s).WithObjects(prov, target, clone, grantNamespace("team-b", grant)).
				WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}).Build()
			r := &VMCloneReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: &clonerProvider{}},
				Recorder: record.NewFakeRecorder(20)}

			reconcileClone(t, r, clone, 1)
			err := r.Get(context.Background(), types.NamespacedName{Namespace: "team-b", Name: targetKey.Name}, &infrav1beta1.VirtualMachine{})
			if grant != nil {
				assert.True(t, apierrors.IsNotFound(err), "a granted namespace: the clone's own target is removed (got %v)", err)
			} else {
				assert.NoError(t, err, "a revoked grant: the target is left alone")
			}
		})
	}
}
