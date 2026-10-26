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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// legacyCountConnectedVMs is countConnectedVMs as it was before the
// specProviderIndex: every VirtualMachine listed (deep-copied) and matched on
// spec.providerRef. The equivalence tests compare the indexed count with it.
func legacyCountConnectedVMs(ctx context.Context, c client.Reader, provider *infravirtrigaudiov1beta1.Provider) (int32, error) {
	vmList := &infravirtrigaudiov1beta1.VirtualMachineList{}
	if err := c.List(ctx, vmList); err != nil {
		return 0, err
	}
	count := int32(0)
	for _, vm := range vmList.Items {
		providerNamespace := vm.Namespace
		if vm.Spec.ProviderRef.Namespace != "" {
			providerNamespace = vm.Spec.ProviderRef.Namespace
		}
		if vm.Spec.ProviderRef.Name == provider.Name && providerNamespace == provider.Namespace {
			count++
		}
	}
	return count, nil
}

// connectedVMFixture is a mix of VMs referencing Providers p (infra) and q
// (infra), from the Provider's namespace and others, bound and unbound,
// deleting, and a same-named reference resolving to another namespace.
func connectedVMFixture() []client.Object {
	now := metav1.Now()
	vm := func(ns, name, refNS, refName string, mutate func(*infravirtrigaudiov1beta1.VirtualMachine)) client.Object {
		v := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: refName, Namespace: refNS},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			},
		}
		if mutate != nil {
			mutate(v)
		}
		return v
	}
	return []client.Object{
		vm("infra", "own-implicit", "", "p", nil),
		vm("infra", "own-explicit", "infra", "p", nil),
		vm("tenant-a", "cross-ns", "infra", "p", nil),
		vm("tenant-a", "same-name-own-ns", "", "p", nil), // resolves to tenant-a/p, not infra/p
		vm("tenant-b", "bound", "infra", "p", func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Status.ID = "tenant-b.bound"
		}),
		vm("tenant-b", "deleting", "infra", "p", func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
			v.DeletionTimestamp = &now
		}),
		vm("infra", "other-provider", "", "q", nil),
		vm("tenant-c", "other-provider-cross", "infra", "q", nil),
	}
}

// TestCountConnectedVMs_MatchesFullScan: the indexed count equals the old
// cluster-wide count for every Provider key, bound or unbound VMs, any
// namespace.
func TestCountConnectedVMs_MatchesFullScan(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(connectedVMFixture()...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, specProviderIndex, specProviderIndexValue).Build()
	r := &ProviderReconciler{Client: c}

	for _, key := range []types.NamespacedName{
		{Namespace: "infra", Name: "p"}, {Namespace: "infra", Name: "q"},
		{Namespace: "tenant-a", Name: "p"}, {Namespace: "infra", Name: "absent"},
	} {
		p := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
		want, err := legacyCountConnectedVMs(ctx, c, p)
		require.NoError(t, err)
		got, err := r.countConnectedVMs(ctx, p)
		require.NoError(t, err)
		assert.Equal(t, want, got, "provider %s", key)
	}
	// The fixture's expected counts, so the equivalence is not vacuous.
	got, _ := r.countConnectedVMs(ctx, &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "p"}})
	assert.Equal(t, int32(5), got)
}

// TestCountConnectedVMs_FollowsSpecChanges: re-pointing a VM's providerRef
// moves it between Providers' counts, as the cluster-wide scan does.
func TestCountConnectedVMs_FollowsSpecChanges(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(connectedVMFixture()...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, specProviderIndex, specProviderIndexValue).Build()
	r := &ProviderReconciler{Client: c}
	p := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "p"}}
	q := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "q"}}

	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "own-implicit"}, vm))
	vm.Spec.ProviderRef.Name = "q"
	require.NoError(t, c.Update(ctx, vm))

	for _, prov := range []*infravirtrigaudiov1beta1.Provider{p, q} {
		want, err := legacyCountConnectedVMs(ctx, c, prov)
		require.NoError(t, err)
		got, err := r.countConnectedVMs(ctx, prov)
		require.NoError(t, err)
		assert.Equal(t, want, got, prov.Name)
	}
}

// TestHostInventoryChanged pins the Provider controller's Host watch filter:
// status-only writes (heartbeat, capacity, health, conditions) and finalizer
// changes enqueue nothing; spec, metadata label/annotation, UID and deletion
// changes, creation and deletion do.
func TestHostInventoryChanged(t *testing.T) {
	base := func() *infravirtrigaudiov1beta1.Host {
		cpu := int32(16)
		return &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "h1", UID: "u1", Generation: 1,
				Labels: map[string]string{"zone": "a"}, Annotations: map[string]string{"note": "x"}},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "p"},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "qemu+ssh://root@h1/system",
				Labels:      map[string]string{"storage.virtrigaud.io/pool-nfs": "true"},
				Schedulable: true,
			},
			Status: infravirtrigaudiov1beta1.HostStatus{Health: infravirtrigaudiov1beta1.HostHealthReady, AllocatableCPU: &cpu},
		}
	}
	now := metav1.NewTime(time.Now())
	cases := []struct {
		name   string
		mutate func(h *infravirtrigaudiov1beta1.Host)
		want   bool
	}{
		{"heartbeat", func(h *infravirtrigaudiov1beta1.Host) { h.Status.LastHeartbeatTime = &now }, false},
		{"health", func(h *infravirtrigaudiov1beta1.Host) { h.Status.Health = infravirtrigaudiov1beta1.HostHealthNotReady }, false},
		{"capacity", func(h *infravirtrigaudiov1beta1.Host) { c := int32(32); h.Status.AllocatableCPU = &c }, false},
		{"conditions", func(h *infravirtrigaudiov1beta1.Host) {
			h.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "X"}}
		}, false},
		{"finalizer", func(h *infravirtrigaudiov1beta1.Host) {
			h.Finalizers = []string{infravirtrigaudiov1beta1.HostInUseFinalizer}
		}, false},
		{"endpoint", func(h *infravirtrigaudiov1beta1.Host) { h.Spec.Endpoint = "qemu+ssh://root@h1b/system" }, true},
		{"spec labels", func(h *infravirtrigaudiov1beta1.Host) { h.Spec.Labels = map[string]string{"x": "y"} }, true},
		{"provider ref", func(h *infravirtrigaudiov1beta1.Host) { h.Spec.ProviderRef.Name = "other" }, true},
		{"spec without generation bump", func(h *infravirtrigaudiov1beta1.Host) { h.Spec.Schedulable = false }, true},
		{"generation only", func(h *infravirtrigaudiov1beta1.Host) { h.Generation = 2 }, true},
		{"metadata labels", func(h *infravirtrigaudiov1beta1.Host) { h.Labels = map[string]string{"zone": "b"} }, true},
		{"annotations", func(h *infravirtrigaudiov1beta1.Host) { h.Annotations = nil }, true},
		{"uid", func(h *infravirtrigaudiov1beta1.Host) { h.UID = "u2" }, true},
		{"deletion", func(h *infravirtrigaudiov1beta1.Host) { h.DeletionTimestamp = &now }, true},
	}
	pred := hostInventoryChanged()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldHost, newHost := base(), base()
			tc.mutate(newHost)
			assert.Equal(t, tc.want, pred.Update(event.UpdateEvent{ObjectOld: oldHost, ObjectNew: newHost}))
		})
	}
	assert.True(t, pred.Create(event.CreateEvent{Object: base()}))
	assert.True(t, pred.Delete(event.DeleteEvent{Object: base()}))
	assert.True(t, pred.Generic(event.GenericEvent{Object: base()}))
}

// TestCountConnectedVMs_ScalesWithProviderNotCluster is a sanity check of the
// read: VMs of other Providers are not listed.
func TestCountConnectedVMs_ScalesWithProviderNotCluster(t *testing.T) {
	ctx := context.Background()
	var objs []client.Object
	for i := 0; i < 50; i++ {
		objs = append(objs, &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: "t", Name: fmt.Sprintf("vm-%d", i)},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: fmt.Sprintf("p%d", i%5), Namespace: "infra"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			},
		})
	}
	listed := 0
	base := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(objs...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, specProviderIndex, specProviderIndexValue).Build()
	counting := &listCountingReader{Reader: base, items: &listed}
	got, err := countSpecProviderVMs(ctx, counting, types.NamespacedName{Namespace: "infra", Name: "p0"})
	require.NoError(t, err)
	assert.Equal(t, int32(10), got)
	assert.Equal(t, 10, listed, "only the Provider's own VMs are listed")
}

// listCountingReader counts the VirtualMachines its List calls return.
type listCountingReader struct {
	client.Reader
	items *int
}

// List counts returned VirtualMachines.
func (r *listCountingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if l, ok := list.(*infravirtrigaudiov1beta1.VirtualMachineList); ok {
		*r.items += len(l.Items)
	}
	return nil
}

// TestWithClusteredResync: a successful clustered Provider reconcile requeues
// within clusteredProviderResync (the Host heartbeat no longer re-enqueues
// it); single-topology and deleting Providers, errors, immediate and shorter
// requeues are unchanged.
func TestWithClusteredResync(t *testing.T) {
	clustered := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Topology: infravirtrigaudiov1beta1.ProviderTopologyCluster}}
	single := &infravirtrigaudiov1beta1.Provider{}
	now := metav1.Now()
	deleting := clustered.DeepCopy()
	deleting.DeletionTimestamp = &now
	boom := fmt.Errorf("boom")

	cases := []struct {
		name     string
		provider *infravirtrigaudiov1beta1.Provider
		in       ctrl.Result
		err      error
		want     ctrl.Result
	}{
		{"clustered, no requeue", clustered, ctrl.Result{}, nil, ctrl.Result{RequeueAfter: clusteredProviderResync}},
		{"clustered, longer requeue", clustered, ctrl.Result{RequeueAfter: 5 * time.Minute}, nil, ctrl.Result{RequeueAfter: clusteredProviderResync}},
		{"clustered, shorter requeue", clustered, ctrl.Result{RequeueAfter: 30 * time.Second}, nil, ctrl.Result{RequeueAfter: 30 * time.Second}},
		{"clustered, immediate requeue", clustered, ctrl.Result{Requeue: true}, nil, ctrl.Result{Requeue: true}},
		{"clustered, error", clustered, ctrl.Result{}, boom, ctrl.Result{}},
		{"clustered, deleting", deleting, ctrl.Result{}, nil, ctrl.Result{}},
		{"single", single, ctrl.Result{}, nil, ctrl.Result{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, withClusteredResync(tc.provider, tc.in, tc.err))
		})
	}
}
