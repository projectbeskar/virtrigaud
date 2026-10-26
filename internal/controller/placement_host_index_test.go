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
	"slices"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// legacyCommittedOnHost is committedOnHost as it was before the
// placementHostIndex: every VM of the Provider listed, filtered by host.
func legacyCommittedOnHost(ctx context.Context, reader client.Reader, provider types.NamespacedName, host string) (cpu, memMiB int64, err error) {
	vms, err := listProviderVMs(ctx, reader, provider)
	if err != nil {
		return 0, 0, err
	}
	classes := classMemo{}
	for i := range vms {
		vm := &vms[i]
		if placementProviderKey(vm) != provider || !slices.Contains(placementHosts(vm), host) {
			continue
		}
		fp, err := sizeForAccounting(ctx, reader, vm, classes)
		if err != nil {
			return 0, 0, err
		}
		cpu += int64(fp.CPU)
		memMiB += fp.MemoryMiB
	}
	return cpu, memMiB, nil
}

// legacyVMsUsingHost is vmsUsingHost as it was before the placementHostIndex.
func legacyVMsUsingHost(ctx context.Context, reader client.Reader, provider types.NamespacedName, host string) ([]string, error) {
	vms, err := listProviderVMs(ctx, reader, provider)
	if err != nil {
		return nil, err
	}
	var users []string
	for i := range vms {
		vm := &vms[i]
		if placementProviderKey(vm) != provider {
			continue
		}
		if slices.Contains(placementHosts(vm), host) {
			users = append(users, vm.Namespace+"/"+vm.Name)
		}
	}
	sort.Strings(users)
	return users, nil
}

// indexedClient is a fake client with both placement indexes over objs.
func indexedClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(objs...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementHostIndex, placementHostIndexValue).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).Build()
}

// assertHostAccountingEquivalent compares the indexed and the legacy per-host
// results for every (provider, host) pair.
func assertHostAccountingEquivalent(t *testing.T, c client.Client, providers []types.NamespacedName, hosts []string) {
	t.Helper()
	ctx := context.Background()
	r := &HostReconciler{Client: c}
	for _, p := range providers {
		for _, h := range hosts {
			wantCPU, wantMem, err := legacyCommittedOnHost(ctx, c, p, h)
			require.NoError(t, err)
			gotCPU, gotMem, err := committedOnHost(ctx, c, p, h)
			require.NoError(t, err)
			assert.Equal(t, [2]int64{wantCPU, wantMem}, [2]int64{gotCPU, gotMem}, "committed %s %s", p, h)

			wantUsers, err := legacyVMsUsingHost(ctx, c, p, h)
			require.NoError(t, err)
			host := &infravirtrigaudiov1beta1.Host{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: h},
				Spec: infravirtrigaudiov1beta1.HostSpec{ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: p.Name}}}
			gotUsers, err := r.vmsUsingHost(ctx, host)
			require.NoError(t, err)
			assert.Equal(t, wantUsers, gotUsers, "users %s %s", p, h)
		}
	}
}

// TestPlacementHostIndex_FixtureEquivalence: the Spike 1A fixture (bound VMs
// with recorded sizes, pending creates with pendingResources, bound VMs
// without a recorded size sized from their VMClass, 10 namespaces).
func TestPlacementHostIndex_FixtureEquivalence(t *testing.T) {
	f := schedbench.Build(schedbench.Config{
		Hosts: 8, VMs: 240, Utilization: 0.8, Strategy: infravirtrigaudiov1beta1.PoolStrategySpread,
		Namespaces: 10, PendingFraction: 0.1, UnrecordedFraction: 0.1,
		ProviderNamespace: schedbench.ProviderNamespace, ProviderName: schedbench.ProviderName, Seed: 5,
	})
	objs := make([]client.Object, 0, len(f.VMs)+len(f.Classes))
	for i := range f.VMs {
		objs = append(objs, f.VMs[i].DeepCopy())
	}
	for i := range f.Classes {
		objs = append(objs, f.Classes[i].DeepCopy())
	}
	hosts := make([]string, 0, len(f.Hosts)+1)
	for i := range f.Hosts {
		hosts = append(hosts, f.Hosts[i].Name)
	}
	hosts = append(hosts, "no-such-host")
	c := indexedClient(t, objs...)
	assertHostAccountingEquivalent(t, c,
		[]types.NamespacedName{{Namespace: schedbench.ProviderNamespace, Name: schedbench.ProviderName}, {Namespace: "infra", Name: "other"}}, hosts)
}

// edgeVM builds a VM of provider infra/<provider> in ns.
func edgeVM(ns, name, provider string, mutate func(*infravirtrigaudiov1beta1.VirtualMachine)) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(ns + "-" + name),
			Finalizers: []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: provider, Namespace: "infra"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "std"},
		},
	}
	mutate(vm)
	return vm
}

// edgeCaseObjects covers the transitions and identities the per-host
// accounting must preserve.
func edgeCaseObjects() []client.Object {
	now := metav1.Now()
	i32 := func(v int32) *int32 { return &v }
	i64 := func(v int64) *int64 { return &v }
	recorded := func(vm *infravirtrigaudiov1beta1.VirtualMachine, host string, cpu int32, mem int64) {
		vm.Status.ID = vm.Namespace + "." + vm.Name
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: host, MemoryCeilingMiB: i64(0)}
		vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32(cpu), MemoryMiB: i64(mem)}
	}
	std := &infravirtrigaudiov1beta1.VMClass{ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "std"},
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")}}
	hot := &infravirtrigaudiov1beta1.VMClass{ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "hot"},
		Spec: infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("2Gi"),
			PerformanceProfile: &infravirtrigaudiov1beta1.PerformanceProfile{MemoryHotAddEnabled: true}}}
	return []client.Object{
		std, hot,
		// Bound, recorded size.
		edgeVM("t1", "bound", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) { recorded(vm, "h1", 4, 8192) }),
		// Pending create with pendingResources.
		edgeVM("t1", "pending", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "h1",
				PendingResources: &infravirtrigaudiov1beta1.PlacementResources{CPU: 3, MemoryMiB: 3000}, MemoryCeilingMiB: i64(0)}
		}),
		// Dual placement during a transition: bound on h1, pending on h2.
		edgeVM("t1", "dual", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			recorded(vm, "h1", 1, 1024)
			vm.Status.Placement.PendingHost = "h2"
		}),
		// The same host in both fields counts once.
		edgeVM("t1", "same-twice", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			recorded(vm, "h2", 2, 2048)
			vm.Status.Placement.PendingHost = "h2"
		}),
		// Legacy sizing: bound, no recorded size, VMClass size.
		edgeVM("t1", "legacy", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.ID = "t1.legacy"
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "h2"}
		}),
		// Memory hot-add with no recorded ceiling: counted at its ceiling.
		edgeVM("t1", "hotadd", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.ClassRef.Name = "hot"
			vm.Status.ID = "t1.hotadd"
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "h1"}
		}),
		// A class the VM's namespace may not use: minimum footprint.
		edgeVM("t2", "ungranted", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{Name: "std", Namespace: "t1"}
			vm.Status.ID = "t2.ungranted"
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "h1"}
		}),
		// Being deleted, finalizer still on: still counts.
		edgeVM("t1", "deleting", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			recorded(vm, "h1", 2, 2048)
			vm.DeletionTimestamp = &now
		}),
		// Being deleted, only another controller's finalizer left: no longer counts.
		edgeVM("t1", "released", "a", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			recorded(vm, "h1", 8, 8192)
			vm.DeletionTimestamp = &now
			vm.Finalizers = []string{"other.example.com/hold"}
		}),
		// Bound through Provider a while its spec names b (boundProvider wins).
		edgeVM("t1", "rebound", "b", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			recorded(vm, "h1", 1, 512)
			vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "infra", Name: "a"}
		}),
		// Provider b with the same host names: isolated from a.
		edgeVM("t3", "b-on-h1", "b", func(vm *infravirtrigaudiov1beta1.VirtualMachine) { recorded(vm, "h1", 16, 65536) }),
		edgeVM("t3", "b-pending-h2", "b", func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "h2",
				PendingResources: &infravirtrigaudiov1beta1.PlacementResources{CPU: 5, MemoryMiB: 5000}, MemoryCeilingMiB: i64(0)}
		}),
		// Unplaced VM: no host.
		edgeVM("t1", "unplaced", "a", func(*infravirtrigaudiov1beta1.VirtualMachine) {}),
	}
}

var edgeProviders = []types.NamespacedName{{Namespace: "infra", Name: "a"}, {Namespace: "infra", Name: "b"}}
var edgeHosts = []string{"h1", "h2", "h3"}

// TestPlacementHostIndex_EdgeCaseEquivalence: pending, bound, dual and
// same-host placements, legacy and hot-add sizing, ungranted classes,
// deletion with and without the finalizer, a re-bound VM and two Providers
// with identical host names.
func TestPlacementHostIndex_EdgeCaseEquivalence(t *testing.T) {
	c := indexedClient(t, edgeCaseObjects()...)
	assertHostAccountingEquivalent(t, c, edgeProviders, edgeHosts)

	// Non-vacuous: the expected per-host results.
	cpu, _, err := committedOnHost(context.Background(), c, edgeProviders[1], "h1")
	require.NoError(t, err)
	assert.Equal(t, int64(16), cpu, "Provider b's h1 holds only its own VM")
	users, err := (&HostReconciler{Client: c}).vmsUsingHost(context.Background(), &infravirtrigaudiov1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "h2"},
		Spec:       infravirtrigaudiov1beta1.HostSpec{ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "a"}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"t1/dual", "t1/legacy", "t1/same-twice"}, users)
}

// TestPlacementHostIndex_FollowsChanges: the index follows resizes, the
// pending -> bound promotion, a host release and finalizer removal, matching
// the scan after each.
func TestPlacementHostIndex_FollowsChanges(t *testing.T) {
	ctx := context.Background()
	c := indexedClient(t, edgeCaseObjects()...)
	get := func(ns, name string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vm))
		return vm
	}
	steps := []func(){
		func() { // resize recorded
			vm := get("t1", "bound")
			cpu := int32(6)
			vm.Status.CurrentResources.CPU = &cpu
			require.NoError(t, c.Status().Update(ctx, vm))
		},
		func() { // pending -> bound on its pending host
			vm := get("t1", "pending")
			vm.Status.ID = "t1.pending"
			vm.Status.Placement.Host, vm.Status.Placement.PendingHost = vm.Status.Placement.PendingHost, ""
			require.NoError(t, c.Status().Update(ctx, vm))
		},
		func() { // dual -> pending host released
			vm := get("t1", "dual")
			vm.Status.Placement.PendingHost = ""
			require.NoError(t, c.Status().Update(ctx, vm))
		},
		func() { // pending moved to h3 (conflict exclusion re-schedule)
			vm := get("t3", "b-pending-h2")
			vm.Status.Placement.PendingHost = "h3"
			require.NoError(t, c.Status().Update(ctx, vm))
		},
		func() { // VM deleted outright
			require.NoError(t, c.Delete(ctx, get("t1", "legacy")))
		},
	}
	for i, step := range steps {
		step()
		t.Run(fmt.Sprintf("after step %d", i), func(t *testing.T) {
			assertHostAccountingEquivalent(t, c, edgeProviders, edgeHosts)
		})
	}
}

// TestPlacementHostIndexValue pins the index keys: Provider-qualified, both
// placement fields once each, nothing for a released VM or another kind.
func TestPlacementHostIndexValue(t *testing.T) {
	byName := map[string]*infravirtrigaudiov1beta1.VirtualMachine{}
	for _, o := range edgeCaseObjects() {
		if vm, ok := o.(*infravirtrigaudiov1beta1.VirtualMachine); ok {
			byName[vm.Name] = vm
		}
	}
	assert.Equal(t, []string{"infra/a|h1", "infra/a|h2"}, placementHostIndexValue(byName["dual"]))
	assert.Equal(t, []string{"infra/a|h2"}, placementHostIndexValue(byName["same-twice"]))
	assert.Equal(t, []string{"infra/a|h1"}, placementHostIndexValue(byName["rebound"]))
	assert.Equal(t, []string{"infra/b|h1"}, placementHostIndexValue(byName["b-on-h1"]))
	assert.Nil(t, placementHostIndexValue(byName["released"]))
	assert.Nil(t, placementHostIndexValue(byName["unplaced"]))
	assert.Nil(t, placementHostIndexValue(&infravirtrigaudiov1beta1.Host{}))
}
