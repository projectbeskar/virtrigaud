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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// Committed capacity and assumptions on the clustered create path (ADR-0007
// Addendum A, scheduler-accuracy amendment).

const capNS = "default"

// ─── fixtures ────────────────────────────────────────────────────────────────

// capHost is a Ready host in pool-a of prov-cluster with cpu vCPUs and ample
// memory.
func capHost(name string, cpu int32) *infravirtrigaudiov1beta1.Host {
	h := readyHost(name, capNS, "pool-a", "prov-cluster")
	h.Status.AllocatableCPU = i32p(cpu)
	h.Status.AllocatableMemoryMiB = i64p(1 << 20)
	return h
}

// capVM is a VM of prov-cluster with a UID, sized by smallVMClass (2 vCPU).
func capVM(name string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := clusterVM(name, capNS, "prov-cluster")
	vm.UID = types.UID("uid-" + name)
	return vm
}

// withPlacement records host / pendingHost on vm.
func withPlacement(vm *infravirtrigaudiov1beta1.VirtualMachine, host, pending string) *infravirtrigaudiov1beta1.VirtualMachine {
	vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: host, PendingHost: pending, Pool: "pool-a"}
	if host != "" {
		vm.Status.ID = vm.Name
	}
	return vm
}

// capBase returns the Provider, pool, class and image every capacity test uses.
func capBase() []client.Object {
	return []client.Object{
		clusteredProviderCR("prov-cluster", capNS),
		hostPoolCR("pool-a", capNS, "prov-cluster"),
		smallVMClass(capNS),
		minimalVMImage(capNS),
	}
}

// capCreateReq is the create request resolveClusterPlacement receives for a
// smallVMClass VM.
func capCreateReq() contracts.CreateRequest {
	return contracts.CreateRequest{Class: contracts.VMClass{CPU: 2, MemoryMiB: 4096}}
}

// resolve runs resolveClusterPlacement for vm and returns the host it chose
// ("" when it did not place the VM) and the result.
func resolve(t *testing.T, r *VirtualMachineReconciler, vm *infravirtrigaudiov1beta1.VirtualMachine) (string, ctrl.Result) {
	t.Helper()
	p, res, err := r.resolveClusterPlacement(context.Background(), vm, clusteredProviderCR("prov-cluster", capNS), capCreateReq(), nil)
	require.NoError(t, err)
	if p == nil {
		return "", res
	}
	return p.hostID, res
}

// concurrentCreateProvider records every Create's target host; safe for
// concurrent use.
type concurrentCreateProvider struct {
	stubProvider
	mu    sync.Mutex
	hosts []string
}

func (p *concurrentCreateProvider) Create(_ context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hosts = append(p.hosts, req.TargetHostID)
	return contracts.CreateResponse{ID: req.Name}, nil
}

func (p *concurrentCreateProvider) created() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

// laggingClient models an informer cache that has not caught up: every read
// comes from a snapshot, every write goes to the live store. catchUp refreshes
// the snapshot from the live store.
type laggingClient struct {
	client.Client // live: writes and Status()

	scheme *runtime.Scheme
	mu     sync.RWMutex
	reader client.Reader
}

func newLaggingClient(t *testing.T, s *runtime.Scheme, objs ...client.Object) *laggingClient {
	t.Helper()
	build := func() client.Client {
		copies := make([]client.Object, 0, len(objs))
		for _, o := range objs {
			c, ok := o.DeepCopyObject().(client.Object)
			require.True(t, ok, "deep copy of %T is not a client.Object", o)
			copies = append(copies, c)
		}
		return fake.NewClientBuilder().WithScheme(s).WithObjects(copies...).
			WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).Build()
	}
	return &laggingClient{Client: build(), scheme: s, reader: build()}
}

func (c *laggingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c *laggingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.reader.List(ctx, list, opts...)
}

// catchUp makes the snapshot show everything the live store has.
func (c *laggingClient) catchUp(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var objs []client.Object
	lists := []client.ObjectList{
		&infravirtrigaudiov1beta1.ProviderList{}, &infravirtrigaudiov1beta1.HostPoolList{},
		&infravirtrigaudiov1beta1.HostList{}, &infravirtrigaudiov1beta1.VMClassList{},
		&infravirtrigaudiov1beta1.VMImageList{}, &infravirtrigaudiov1beta1.VirtualMachineList{},
		&infravirtrigaudiov1beta1.VMPlacementPolicyList{},
	}
	for _, l := range lists {
		require.NoError(t, c.Client.List(ctx, l))
		items, err := metaItems(l)
		require.NoError(t, err)
		objs = append(objs, items...)
	}
	fresh := fake.NewClientBuilder().WithScheme(c.scheme).WithObjects(objs...).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).Build()
	c.mu.Lock()
	c.reader = fresh
	c.mu.Unlock()
}

// metaItems returns a list's items as client.Objects.
func metaItems(l client.ObjectList) ([]client.Object, error) {
	var out []client.Object
	switch v := l.(type) {
	case *infravirtrigaudiov1beta1.ProviderList:
		for i := range v.Items {
			out = append(out, &v.Items[i])
		}
	case *infravirtrigaudiov1beta1.HostPoolList:
		for i := range v.Items {
			out = append(out, &v.Items[i])
		}
	case *infravirtrigaudiov1beta1.HostList:
		for i := range v.Items {
			out = append(out, &v.Items[i])
		}
	case *infravirtrigaudiov1beta1.VMClassList:
		for i := range v.Items {
			out = append(out, &v.Items[i])
		}
	case *infravirtrigaudiov1beta1.VMImageList:
		for i := range v.Items {
			out = append(out, &v.Items[i])
		}
	case *infravirtrigaudiov1beta1.VirtualMachineList:
		for i := range v.Items {
			out = append(out, &v.Items[i])
		}
	case *infravirtrigaudiov1beta1.VMPlacementPolicyList:
		for i := range v.Items {
			out = append(out, &v.Items[i])
		}
	default:
		return nil, fmt.Errorf("unexpected list %T", l)
	}
	return out, nil
}

// readVM reads a VM from c (the snapshot for a laggingClient).
func readVM(t *testing.T, c client.Reader, name string) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: capNS, Name: name}, vm))
	return vm
}

// createConcurrently runs createVM for every named VM at once, each reading
// its VM from r's (lagging) client, and waits for all of them.
func createConcurrently(t *testing.T, reconcilers func(i int) *VirtualMachineReconciler, prov contracts.Provider, names []string) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			<-start
			r := reconcilers(i)
			vm := &infravirtrigaudiov1beta1.VirtualMachine{}
			if err := r.Get(context.Background(), types.NamespacedName{Namespace: capNS, Name: name}, vm); err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = r.createVM(context.Background(), vm, prov, clusteredProviderCR("prov-cluster", capNS),
				smallVMClass(capNS), minimalVMImage(capNS), nil)
		}(i, name)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, names[i])
	}
}

// ─── committed capacity ──────────────────────────────────────────────────────

// TestClusteredCapacity_CommittedSources is the table of what counts toward a
// host's committed capacity. The host has 4 vCPU; every VM is 2 vCPU, so one
// other 2 vCPU VM on it leaves room for the new one and two do not.
func TestClusteredCapacity_CommittedSources(t *testing.T) {
	deleting := func(vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
		now := metav1.Now()
		vm.DeletionTimestamp = &now
		vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
		return vm
	}
	otherNS := func(vm *infravirtrigaudiov1beta1.VirtualMachine) *infravirtrigaudiov1beta1.VirtualMachine {
		vm.Namespace = "tenant-b"
		vm.Spec.ProviderRef.Namespace = capNS
		vm.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{Name: "test-class", Namespace: capNS}
		return vm
	}
	bound := func(name string) *infravirtrigaudiov1beta1.VirtualMachine {
		return withPlacement(capVM(name), "host-alpha", "")
	}

	cases := []struct {
		name   string
		others []client.Object
		fits   bool
	}{
		{name: "empty host", fits: true},
		{name: "one bound VM", others: []client.Object{bound("a")}, fits: true},
		{name: "two bound VMs", others: []client.Object{bound("a"), bound("b")}, fits: false},
		{name: "a pendingHost-only VM counts", others: []client.Object{bound("a"), withPlacement(capVM("p"), "", "host-alpha")}, fits: false},
		{name: "host == pendingHost counts once", others: []client.Object{withPlacement(capVM("x"), "host-alpha", "host-alpha")}, fits: true},
		{name: "a VM being deleted still counts", others: []client.Object{bound("a"), deleting(bound("d"))}, fits: false},
		{
			name: "a VM being deleted whose finalizer is gone does not count (a foreign finalizer holds nothing)",
			others: []client.Object{bound("a"), func() client.Object {
				vm := deleting(bound("gone"))
				vm.Finalizers = []string{"example.com/someone-else"}
				return vm
			}()},
			fits: true,
		},
		{name: "another namespace's VM on this Provider counts", others: []client.Object{bound("a"), otherNS(bound("t"))}, fits: false},
		{
			name: "a VM of another Provider on a same-named host does not count",
			others: []client.Object{bound("a"), func() client.Object {
				vm := bound("foreign")
				vm.Spec.ProviderRef.Name = "other-provider"
				return vm
			}()},
			fits: true,
		},
		{
			name: "a VM bound through this Provider counts even if its providerRef was re-pointed",
			others: []client.Object{bound("a"), func() client.Object {
				vm := bound("repointed")
				vm.Spec.ProviderRef.Name = "other-provider"
				vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: capNS, Name: "prov-cluster"}
				return vm
			}()},
			fits: false,
		},
		{name: "an unplaced VM does not count", others: []client.Object{bound("a"), capVM("unplaced")}, fits: true},
		{
			name: "excludedHosts are not a placement",
			others: []client.Object{bound("a"), func() client.Object {
				vm := capVM("excluded")
				vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{ExcludedHosts: []string{"host-alpha"}}
				return vm
			}()},
			fits: true,
		},
		{
			name: "a resize in progress counts at its larger recorded size",
			others: []client.Object{func() client.Object {
				vm := bound("resized")
				vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(4)}
				return vm
			}()},
			fits: false,
		},
		{
			name: "a spec.resources override larger than the class counts",
			others: []client.Object{func() client.Object {
				vm := bound("override")
				vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(3)}
				return vm
			}()},
			fits: false,
		},
		{
			name: "a VM whose class is gone is sized from its current resources",
			others: []client.Object{bound("a"), func() client.Object {
				vm := bound("classless")
				vm.Spec.ClassRef.Name = "deleted-class"
				vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2)}
				return vm
			}()},
			fits: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := capVM("new")
			objs := append(capBase(), capHost("host-alpha", 4), vm)
			objs = append(objs, tc.others...)
			r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)

			host, res := resolve(t, r, vm)
			if tc.fits {
				assert.Equal(t, "host-alpha", host)
				return
			}
			assert.Empty(t, host)
			assert.Equal(t, placementUnschedulableRetryInterval, res.RequeueAfter)
			placed := placedCondition(vm)
			require.NotNil(t, placed)
			assert.Equal(t, k8s.ReasonUnschedulable, placed.Reason)
			assert.Contains(t, placed.Message, "insufficient CPU on 1 of 1 candidate host(s): requested 2 vCPU")
			assert.Equal(t, k8s.ReasonUnschedulable, provisioningReason(vm))
		})
	}
}

// TestClusteredCapacity_SelfIsNotCommittedAgainstItself: a VM with a binding on
// a host that is otherwise full re-selects it.
func TestClusteredCapacity_SelfIsNotCommittedAgainstItself(t *testing.T) {
	vm := withPlacement(capVM("self"), "host-alpha", "")
	vm.Status.ID = ""
	objs := append(capBase(), capHost("host-alpha", 2), vm)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)
	host, _ := resolve(t, r, vm)
	assert.Equal(t, "host-alpha", host)
}

// TestClusteredCapacity_UnschedulableMessageIsBoundedAndNamesNoOtherVM: a full
// pool's message carries numbers only.
func TestClusteredCapacity_UnschedulableMessageIsBoundedAndNamesNoOtherVM(t *testing.T) {
	objs := capBase()
	for i := 0; i < 30; i++ {
		objs = append(objs, capHost(fmt.Sprintf("host-%02d", i), 2))
		other := withPlacement(capVM(fmt.Sprintf("tenant-secret-%02d", i)), fmt.Sprintf("host-%02d", i), "")
		other.Namespace = "tenant-b"
		other.Spec.ProviderRef.Namespace = capNS
		other.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{Name: "test-class", Namespace: capNS}
		objs = append(objs, other)
	}
	vm := capVM("new")
	objs = append(objs, vm)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)

	host, _ := resolve(t, r, vm)
	require.Empty(t, host)
	msg := placedCondition(vm).Message
	assert.Less(t, len(msg), 512, msg)
	assert.NotContains(t, msg, "tenant-secret")
	assert.NotContains(t, msg, "tenant-b")
	assert.Contains(t, msg, "insufficient CPU on 30 of 30 candidate host(s): requested 2 vCPU, at most 0 free (committed 2 of 2 after overcommit)")
}

// TestClusteredCapacity_UnschedulableBacksOff: consecutive no-fits wait longer,
// up to the cap; a successful schedule resets the backoff.
func TestClusteredCapacity_UnschedulableBacksOff(t *testing.T) {
	vm := capVM("new")
	blocker := withPlacement(capVM("blocker"), "host-alpha", "")
	objs := append(capBase(), capHost("host-alpha", 2), vm, blocker)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)

	var got []time.Duration
	for i := 0; i < 4; i++ {
		_, res := resolve(t, r, vm)
		got = append(got, res.RequeueAfter)
	}
	assert.Equal(t, []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute}, got)
	for _, d := range got {
		assert.GreaterOrEqual(t, d, placementUnschedulableRetryInterval, "never a tight loop")
	}

	// Capacity frees up: the VM is placed and its backoff forgotten.
	require.NoError(t, r.Delete(context.Background(), blocker))
	host, _ := resolve(t, r, vm)
	require.Equal(t, "host-alpha", host)
	r.placementAssumptions().Forget(vmSchedulingUID(vm))
	require.NoError(t, r.Create(context.Background(), withPlacement(capVM("blocker-2"), "host-alpha", "")))
	_, res := resolve(t, r, vm)
	assert.Equal(t, placementUnschedulableRetryInterval, res.RequeueAfter, "the backoff restarts after a success")
}

func TestUnschedulableBackoffForgetsIdleRecords(t *testing.T) {
	var b unschedulableBackoff
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, 30*time.Second, b.next("a", now))
	assert.Equal(t, time.Minute, b.next("a", now))
	assert.Equal(t, 30*time.Second, b.next("b", now), "per VM")
	assert.Equal(t, 30*time.Second, b.next("a", now.Add(placementUnschedulableForget+time.Minute)),
		"a record idle for longer than the forget window starts over")
	b.reset("a")
	assert.Equal(t, 30*time.Second, b.next("a", now))
}

func TestFootprintTakesTheLargestSize(t *testing.T) {
	vm := capVM("x")
	assert.Equal(t, int32(2), footprint(vm, 2, 4096).CPU)

	vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(1), MemoryMiB: i64p(8192)}
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(6), MemoryMiB: i64p(1024)}
	got := footprint(vm, 2, 4096)
	assert.Equal(t, int32(6), got.CPU)
	assert.Equal(t, int64(8192), got.MemoryMiB)

	class := smallVMClass(capNS)
	class.Spec.Memory = resource.MustParse("16Gi")
	assert.Equal(t, int64(16384), classFootprint(vm, class).MemoryMiB)
	assert.Equal(t, int32(6), classFootprint(vm, nil).CPU, "no class: overrides and current resources only")
}

// ─── assumptions ─────────────────────────────────────────────────────────────

// TestClusteredCapacity_ConcurrentCreatesNeverOverbook: 20 VMs are created at
// once against a host that fits exactly 5, through a client whose reads never
// see the other reconciles' pendingHost writes (an informer cache that has not
// caught up). Exactly 5 are created; the other 15 are Unschedulable. The same
// race without a shared assume cache overbooks, which proves the cache — not
// luck or the cache lag — is what holds the line.
func TestClusteredCapacity_ConcurrentCreatesNeverOverbook(t *testing.T) {
	const n, k = 20, 5
	s := coverageTestScheme(t)
	objs := append(capBase(), capHost("host-alpha", 2*k))
	var names []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("vm-%02d", i)
		names = append(names, name)
		objs = append(objs, capVM(name))
	}

	t.Run("shared assume cache", func(t *testing.T) {
		lc := newLaggingClient(t, s, objs...)
		r := &VirtualMachineReconciler{Client: lc, Scheme: s}
		prov := &concurrentCreateProvider{}
		createConcurrently(t, func(int) *VirtualMachineReconciler { return r }, prov, names)

		created := prov.created()
		require.Len(t, created, k, "exactly %d creates", k)
		for _, h := range created {
			assert.Equal(t, "host-alpha", h)
		}
		var placed, unschedulable int
		for _, name := range names {
			vm := readVM(t, lc.Client, name)
			switch {
			case vm.Status.Placement != nil && (vm.Status.Placement.Host != "" || vm.Status.Placement.PendingHost != ""):
				placed++
			case placedCondition(vm) != nil && placedCondition(vm).Reason == k8s.ReasonUnschedulable:
				unschedulable++
				assert.Contains(t, placedCondition(vm).Message,
					fmt.Sprintf("insufficient CPU on 1 of 1 candidate host(s): requested 2 vCPU, at most 0 free (committed %d of %d after overcommit)", 2*k, 2*k))
			}
		}
		assert.Equal(t, k, placed)
		assert.Equal(t, n-k, unschedulable)
	})

	t.Run("control: separate reconcilers overbook", func(t *testing.T) {
		lc := newLaggingClient(t, s, objs...)
		prov := &concurrentCreateProvider{}
		createConcurrently(t, func(int) *VirtualMachineReconciler {
			return &VirtualMachineReconciler{Client: lc, Scheme: s}
		}, prov, names)
		assert.Greater(t, len(prov.created()), k, "without a shared assume cache the lagging reads overbook")
	})
}

// TestClusteredCapacity_ConcurrentMutualHardAntiAffinity: two VMs that must not
// share a host, created at once through lagging reads, never land together.
func TestClusteredCapacity_ConcurrentMutualHardAntiAffinity(t *testing.T) {
	s := coverageTestScheme(t)
	antiDB := &infravirtrigaudiov1beta1.VMPlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "anti-db", Namespace: capNS},
		Spec: infravirtrigaudiov1beta1.VMPlacementPolicySpec{
			AntiAffinity: &infravirtrigaudiov1beta1.AntiAffinityRules{
				VMAntiAffinity: &infravirtrigaudiov1beta1.VMAntiAffinity{
					RequiredDuringScheduling: []infravirtrigaudiov1beta1.VMAffinityTerm{{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
					}},
				},
			},
		},
	}
	db := func(name string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := capVM(name)
		vm.Labels = map[string]string{"app": "db"}
		vm.Spec.PlacementRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: antiDB.Name}
		return vm
	}
	for round := 0; round < 30; round++ {
		objs := append(capBase(), capHost("host-alpha", 16), capHost("host-bravo", 16), antiDB.DeepCopy(), db("db-0"), db("db-1"))
		lc := newLaggingClient(t, s, objs...)
		r := &VirtualMachineReconciler{Client: lc, Scheme: s}
		prov := &concurrentCreateProvider{}
		createConcurrently(t, func(int) *VirtualMachineReconciler { return r }, prov, []string{"db-0", "db-1"})
		created := prov.created()
		require.Len(t, created, 2, "round %d", round)
		require.NotEqual(t, created[0], created[1], "round %d: hard anti-affine VMs landed on one host", round)
	}
}

// TestClusteredCapacity_AssumptionSettlesOnInformerConfirmation: once the
// cache shows the VM's record on the assumed host, the record counts and the
// assumption is dropped — the VM is counted once, not twice.
func TestClusteredCapacity_AssumptionSettlesOnInformerConfirmation(t *testing.T) {
	s := coverageTestScheme(t)
	objs := append(capBase(), capHost("host-alpha", 4), capVM("a"), capVM("b"), capVM("c"))
	lc := newLaggingClient(t, s, objs...)
	r := &VirtualMachineReconciler{Client: lc, Scheme: s}
	prov := &concurrentCreateProvider{}
	create := func(name string) {
		_, err := r.createVM(context.Background(), readVM(t, lc, name), prov, clusteredProviderCR("prov-cluster", capNS),
			smallVMClass(capNS), minimalVMImage(capNS), nil)
		require.NoError(t, err)
	}

	create("a")
	require.Equal(t, 1, r.placementAssumptions().Len(), "a is assumed: the cache has not seen its record")

	lc.catchUp(t) // the cache now shows a bound on host-alpha
	create("b")
	assert.Equal(t, []string{"host-alpha", "host-alpha"}, prov.created(),
		"b fits: a is counted once (its record), not also as an assumption")
	assert.Equal(t, 1, r.placementAssumptions().Len(), "a's assumption settled; only b's is left")
	assert.Equal(t, []string{"uid-b"}, assumedUIDs(r))

	create("c") // no catch-up: b is still only an assumption, and it counts
	assert.Len(t, prov.created(), 2)
	assert.Equal(t, k8s.ReasonUnschedulable, placedCondition(readVM(t, lc.Client, "c")).Reason)
}

// assumedUIDs lists the reconciler's live assumptions for prov-cluster.
func assumedUIDs(r *VirtualMachineReconciler) []string {
	var out []string
	for _, a := range r.placementAssumptions().List(capNS+"/prov-cluster", nil) {
		out = append(out, a.UID)
	}
	return out
}

// TestClusteredCapacity_StaleRecordOnAnotherHostDoesNotSettle: a record on a
// different host than the assumption (e.g. a pendingHost the VM has since
// released) leaves the assumption counting.
func TestClusteredCapacity_StaleRecordOnAnotherHostDoesNotSettle(t *testing.T) {
	stale := withPlacement(capVM("x"), "", "host-alpha")
	vm := capVM("new")
	objs := append(capBase(), capHost("host-alpha", 16), capHost("host-bravo", 2), stale, vm)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)
	r.placementAssumptions().Assume(capNS+"/prov-cluster", assume.Assumption{
		UID: "uid-x", Namespace: capNS, Name: "x", HostID: "host-bravo",
		Resources: scheduler.ResourceRequest{CPU: 2, MemoryMiB: 4096},
	})

	// host-bravo (2 vCPU) is full because of x's assumption, host-alpha has
	// room: the new VM lands on host-alpha, and x's assumption survives.
	host, _ := resolve(t, r, vm)
	assert.Equal(t, "host-alpha", host)
	assert.Contains(t, assumedUIDs(r), "uid-x")
}

// TestClusteredCapacity_AssumptionExpiresAfterTTL: an assumption that nothing
// wrote or forgot (a reconcile that died in between) stops counting after the
// TTL.
func TestClusteredCapacity_AssumptionExpiresAfterTTL(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	a, b := capVM("a"), capVM("b")
	objs := append(capBase(), capHost("host-alpha", 2), a, b)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)
	r.clock = func() time.Time { return now }

	host, _ := resolve(t, r, a) // assumed; no pendingHost write follows
	require.Equal(t, "host-alpha", host)
	host, _ = resolve(t, r, b)
	require.Empty(t, host, "a's assumption holds the host")

	now = now.Add(placementAssumeTTL - time.Second)
	host, _ = resolve(t, r, b)
	require.Empty(t, host, "still held just before the TTL")

	now = now.Add(time.Second)
	host, _ = resolve(t, r, b)
	assert.Equal(t, "host-alpha", host, "released at the TTL")
}

// TestClusteredCapacity_DeletedVMSettlesItsAssumption: an assumption whose VM
// is gone from the cache stops counting at once.
func TestClusteredCapacity_DeletedVMSettlesItsAssumption(t *testing.T) {
	a, b := capVM("a"), capVM("b")
	objs := append(capBase(), capHost("host-alpha", 2), a, b)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)

	host, _ := resolve(t, r, a)
	require.Equal(t, "host-alpha", host)
	require.NoError(t, r.Delete(context.Background(), a))
	host, _ = resolve(t, r, b)
	assert.Equal(t, "host-alpha", host)
	assert.Equal(t, []string{"uid-b"}, assumedUIDs(r))
}

// TestClusteredCapacity_FailedPendingHostWriteForgetsTheAssumption: a lost
// resourceVersion race on the pendingHost write leaves nothing assumed.
func TestClusteredCapacity_FailedPendingHostWriteForgetsTheAssumption(t *testing.T) {
	ctx := context.Background()
	vm := capVM("conflict")
	objs := append(capBase(), capHost("host-alpha", 4), vm)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)

	stale := readVM(t, r, "conflict")
	other := readVM(t, r, "conflict")
	other.Labels = map[string]string{"touched": "by-another-writer"}
	require.NoError(t, r.Update(ctx, other))

	prov := &concurrentCreateProvider{}
	res, err := r.createVM(ctx, stale, prov, clusteredProviderCR("prov-cluster", capNS), smallVMClass(capNS), minimalVMImage(capNS), nil)
	require.NoError(t, err)
	assert.True(t, res.Requeue)
	assert.Empty(t, prov.created())
	assert.Zero(t, r.placementAssumptions().Len(), "nothing durable holds the host, so nothing is assumed")
}

// TestClusteredCapacity_DeletionForgetsTheAssumption: deleting a VM drops its
// assumption.
func TestClusteredCapacity_DeletionForgetsTheAssumption(t *testing.T) {
	vm := capVM("gone")
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	objs := append(capBase(), capHost("host-alpha", 4), vm)
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &concurrentCreateProvider{}}, objs...)
	host, _ := resolve(t, r, vm)
	require.Equal(t, "host-alpha", host)
	require.Equal(t, 1, r.placementAssumptions().Len())

	require.NoError(t, r.Delete(context.Background(), vm)) // the finalizer keeps it, with a deletionTimestamp
	_, err := r.handleDeletion(context.Background(), readVM(t, r, "gone"))
	require.NoError(t, err)
	assert.Zero(t, r.placementAssumptions().Len())
}

// ─── single-host providers are untouched ─────────────────────────────────────

// TestSingleHost_NeverSchedulesOrAssumes: a single-host Provider's create and
// delete never consult the scheduler, the committed-capacity accounting or the
// assume cache (ADR-0007 D9) — even with a full clustered-looking pool and
// placed VMs around it.
func TestSingleHost_NeverSchedulesOrAssumes(t *testing.T) {
	ctx := context.Background()
	providerCR := singleProviderCR("prov-single", capNS)
	vm := clusterVM("vm-single", capNS, providerCR.Name)
	vm.UID = "uid-single"
	full := readyHost("host-full", capNS, "pool-s", providerCR.Name)
	full.Status.AllocatableCPU = i32p(0)
	neighbour := withPlacement(clusterVM("neighbour", capNS, providerCR.Name), "host-full", "")
	prov := &concurrentCreateProvider{}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, providerCR,
		hostPoolCR("pool-s", capNS, providerCR.Name), full, neighbour, smallVMClass(capNS), minimalVMImage(capNS))

	res, err := r.createVM(ctx, vm, prov, providerCR, smallVMClass(capNS), minimalVMImage(capNS), nil)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, res.RequeueAfter)
	assert.Equal(t, []string{""}, prov.created(), "created, with no target host")
	assert.Nil(t, vm.Status.Placement)
	assert.Nil(t, placedCondition(vm), "no Placed condition on a single-host VM")
	assert.Nil(t, r.placements.Load(), "the assume cache is never created")

	stored := readVM(t, r, "vm-single")
	stored.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	require.NoError(t, r.Update(ctx, stored))
	require.NoError(t, r.Delete(ctx, stored))
	_, err = r.handleDeletion(ctx, readVM(t, r, "vm-single"))
	require.NoError(t, err)
	assert.Nil(t, r.placements.Load(), "nor by the deletion path")

	// A message check for completeness: nothing mentions scheduling.
	for _, c := range vm.Status.Conditions {
		assert.False(t, strings.Contains(c.Message, "feasible host"), c.Message)
	}
}

// ─── committed-capacity gauges (Host controller) ─────────────────────────────

// gaugeValue returns the value of the gauge series name{labels}, and whether it
// exists.
func gaugeValue(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := metrics.GetRegistry().Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) && len(m.GetLabel()) == len(labels) {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// TestHostReconciler_PublishesCommittedCapacity: the Host controller publishes
// the scheduler's committed sum for its host, and drops the series with the
// Host.
func TestHostReconciler_PublishesCommittedCapacity(t *testing.T) {
	ctx := context.Background()
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-gauge")
	host := hostCR("host-gauge", "prov-gauge", nil)
	vm := func(name, bound, pending string) *infravirtrigaudiov1beta1.VirtualMachine {
		return withPlacement(clusterVM(name, "default", "prov-gauge"), bound, pending)
	}
	foreign := vm("foreign", "host-gauge", "")
	foreign.Spec.ProviderRef.Name = "another-provider"
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) {
		return healthyHostInfo(id), nil
	}}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host, smallVMClass("default"),
		vm("bound-1", "host-gauge", ""), vm("bound-2", "host-gauge", ""), vm("pending", "", "host-gauge"),
		vm("elsewhere", "host-other", ""), foreign)

	_, err := r.Reconcile(ctx, hostReq("host-gauge"))
	require.NoError(t, err)
	labels := map[string]string{"provider": "default/prov-gauge", "host": "host-gauge"}
	cpu, ok := gaugeValue(t, "virtrigaud_host_committed_cpu", labels)
	require.True(t, ok)
	assert.Equal(t, 6.0, cpu, "two bound and one pending 2 vCPU VM")
	mem, ok := gaugeValue(t, "virtrigaud_host_committed_memory_mib", labels)
	require.True(t, ok)
	assert.Equal(t, 3*4096.0, mem)

	// Deleting the Host (nothing bound any more) removes its series.
	for _, name := range []string{"bound-1", "bound-2", "pending"} {
		stored := &infravirtrigaudiov1beta1.VirtualMachine{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored))
		require.NoError(t, r.Delete(ctx, stored))
	}
	require.NoError(t, r.Delete(ctx, getHost(t, r.Client, "host-gauge")))
	_, err = r.Reconcile(ctx, hostReq("host-gauge"))
	require.NoError(t, err)
	_, ok = gaugeValue(t, "virtrigaud_host_committed_cpu", labels)
	assert.False(t, ok, "the series goes with the Host")
}

// TestPlacementProviderKey (L9): one helper keys every placement lookup.
func TestPlacementProviderKey(t *testing.T) {
	vm := capVM("x")
	assert.Equal(t, types.NamespacedName{Namespace: capNS, Name: "prov-cluster"}, placementProviderKey(vm), "spec.providerRef, namespace defaulted")

	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Name: "bound"}
	assert.Equal(t, types.NamespacedName{Namespace: capNS, Name: "bound"}, placementProviderKey(vm), "an empty boundProvider namespace is the VM's own")

	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "infra", Name: "bound"}
	assert.Equal(t, types.NamespacedName{Namespace: "infra", Name: "bound"}, placementProviderKey(vm), "the bound Provider wins over spec.providerRef")
}
