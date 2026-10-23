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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// poolTestNS is the namespace the HostPool unit tests place their pools in.
const poolTestNS = "default"

// testHostPool builds a HostPool in poolTestNS owned by providerName.
func testHostPool(name, providerName string) *infravirtrigaudiov1beta1.HostPool {
	return &infravirtrigaudiov1beta1.HostPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: poolTestNS, Generation: 3},
		Spec: infravirtrigaudiov1beta1.HostPoolSpec{
			ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: providerName},
			Strategy:    infravirtrigaudiov1beta1.PoolStrategySpread,
		},
	}
}

// poolHost builds a Host in ns, member of poolName, fronted by providerName,
// with the given health and spec.schedulable.
func poolHost(ns, name, poolName, providerName string, health infravirtrigaudiov1beta1.HostHealth, schedulable bool) *infravirtrigaudiov1beta1.Host {
	return &infravirtrigaudiov1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.HostSpec{
			ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: providerName},
			PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: poolName},
			Endpoint:    "qemu+ssh://virt@" + name + "/system",
			Schedulable: schedulable,
		},
		Status: infravirtrigaudiov1beta1.HostStatus{Health: health},
	}
}

// newHostPoolReconciler builds a HostPoolReconciler on a fake client with the
// HostPool status subresource enabled. patches counts the status patches it
// sends.
func newHostPoolReconciler(t *testing.T, patches *atomic.Int32, objs ...client.Object) *HostPoolReconciler {
	t.Helper()
	s := coverageTestScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&infravirtrigaudiov1beta1.HostPool{}, &infravirtrigaudiov1beta1.Host{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*infravirtrigaudiov1beta1.HostPool); ok && patches != nil {
					patches.Add(1)
				}
				return c.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		}).
		Build()
	return &HostPoolReconciler{Client: fc, Scheme: s}
}

func reconcilePool(t *testing.T, r *HostPoolReconciler, name string) *infravirtrigaudiov1beta1.HostPool {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: poolTestNS, Name: name}})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res, "a HostPool is event-driven: no periodic requeue")
	got := &infravirtrigaudiov1beta1.HostPool{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: poolTestNS, Name: name}, got))
	return got
}

func poolReady(t *testing.T, pool *infravirtrigaudiov1beta1.HostPool) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(pool.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c, "Ready condition must be present")
	return c
}

// The counts follow the scheduler's definition: Ready = health Ready;
// schedulable = Ready AND spec.schedulable AND not being deleted AND fronted
// by the pool's Provider. Non-members (another pool, another namespace) are
// never counted.
func TestCountPoolHosts(t *testing.T) {
	pool := testHostPool("pool-a", "prov-a")
	deleting := poolHost(poolTestNS, "deleting", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	hosts := []infravirtrigaudiov1beta1.Host{
		*poolHost(poolTestNS, "ready-sched", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true),
		*poolHost(poolTestNS, "ready-cordoned", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, false),
		*poolHost(poolTestNS, "notready", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthNotReady, true),
		*poolHost(poolTestNS, "unknown", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthUnknown, true),
		*poolHost(poolTestNS, "never-synced", "pool-a", "prov-a", "", true),
		*poolHost(poolTestNS, "other-provider", "pool-a", "prov-b", infravirtrigaudiov1beta1.HostHealthReady, true),
		*deleting,
		// Not members:
		*poolHost(poolTestNS, "other-pool", "pool-b", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true),
		*poolHost("tenant", "other-ns", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true),
	}

	got := countPoolHosts(pool, hosts)
	assert.Equal(t, hostPoolCounts{total: 7, ready: 4, schedulable: 1}, got)
}

func TestHostPoolReadiness(t *testing.T) {
	for name, tc := range map[string]struct {
		counts hostPoolCounts
		status metav1.ConditionStatus
		reason string
	}{
		"empty pool":             {hostPoolCounts{}, metav1.ConditionFalse, reasonHostPoolNoMemberHosts},
		"members, none Ready":    {hostPoolCounts{total: 2}, metav1.ConditionFalse, reasonHostPoolNoSchedulableHosts},
		"Ready but all cordoned": {hostPoolCounts{total: 2, ready: 2}, metav1.ConditionFalse, reasonHostPoolNoSchedulableHosts},
		"one schedulable":        {hostPoolCounts{total: 3, ready: 2, schedulable: 1}, metav1.ConditionTrue, reasonHostPoolHostsAvailable},
	} {
		t.Run(name, func(t *testing.T) {
			status, reason, msg := hostPoolReadiness(tc.counts)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.reason, reason)
			assert.NotEmpty(t, msg)
		})
	}
}

// A pool with Ready, NotReady and cordoned members reports the three counts,
// Ready=True/HostsAvailable, and the spec generation it observed; a Host in
// another namespace naming the same pool is not counted.
func TestHostPoolReconciler_CountsAndReadyCondition(t *testing.T) {
	r := newHostPoolReconciler(t, nil,
		testHostPool("pool-a", "prov-a"),
		poolHost(poolTestNS, "h1", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true),
		poolHost(poolTestNS, "h2", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, false),
		poolHost(poolTestNS, "h3", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthNotReady, true),
		poolHost("tenant", "h4", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true),
	)

	got := reconcilePool(t, r, "pool-a")
	assert.Equal(t, int32(3), got.Status.TotalHosts)
	assert.Equal(t, int32(2), got.Status.ReadyHosts)
	assert.Equal(t, int32(1), got.Status.SchedulableHosts)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)

	c := poolReady(t, got)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, reasonHostPoolHostsAvailable, c.Reason)
	assert.Equal(t, "1 of 3 member host(s) are schedulable (2 Ready)", c.Message)
	assert.Equal(t, got.Generation, c.ObservedGeneration)
}

// An empty pool is Ready=False/NoMemberHosts; a pool whose members are all
// unusable is Ready=False/NoSchedulableHosts.
func TestHostPoolReconciler_NotReadyReasons(t *testing.T) {
	r := newHostPoolReconciler(t, nil,
		testHostPool("empty", "prov-a"),
		testHostPool("cordoned", "prov-a"),
		poolHost(poolTestNS, "h1", "cordoned", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, false),
		poolHost(poolTestNS, "h2", "cordoned", "prov-a", infravirtrigaudiov1beta1.HostHealthNotReady, true),
	)

	empty := reconcilePool(t, r, "empty")
	assert.Zero(t, empty.Status.TotalHosts)
	assert.Zero(t, empty.Status.ReadyHosts)
	assert.Zero(t, empty.Status.SchedulableHosts)
	assert.Equal(t, empty.Generation, empty.Status.ObservedGeneration)
	c := poolReady(t, empty)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, reasonHostPoolNoMemberHosts, c.Reason)

	cordoned := reconcilePool(t, r, "cordoned")
	assert.Equal(t, int32(2), cordoned.Status.TotalHosts)
	assert.Equal(t, int32(1), cordoned.Status.ReadyHosts)
	assert.Zero(t, cordoned.Status.SchedulableHosts)
	c = poolReady(t, cordoned)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, reasonHostPoolNoSchedulableHosts, c.Reason)
	assert.Contains(t, c.Message, "0 of 2 member host(s) are schedulable (1 Ready)")
	assert.NotContains(t, c.Message, "qemu+ssh", "the message never names a host endpoint")
}

// A reconcile that would not change the status writes nothing; a changed
// count is written once.
func TestHostPoolReconciler_NoWriteWhenUnchanged(t *testing.T) {
	var patches atomic.Int32
	r := newHostPoolReconciler(t, &patches,
		testHostPool("pool-a", "prov-a"),
		poolHost(poolTestNS, "h1", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true),
	)

	first := reconcilePool(t, r, "pool-a")
	require.Equal(t, int32(1), patches.Load(), "the first reconcile writes the status")
	for i := 0; i < 3; i++ {
		again := reconcilePool(t, r, "pool-a")
		assert.Equal(t, first.ResourceVersion, again.ResourceVersion)
	}
	assert.Equal(t, int32(1), patches.Load(), "an unchanged status is never written")

	// A heartbeat-like Host status change that moves no count writes nothing.
	h := &infravirtrigaudiov1beta1.Host{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: poolTestNS, Name: "h1"}, h))
	beat := metav1.Now()
	h.Status.LastHeartbeatTime = &beat
	require.NoError(t, r.Status().Update(context.Background(), h))
	reconcilePool(t, r, "pool-a")
	assert.Equal(t, int32(1), patches.Load())

	// The host goes NotReady: one write, and the condition flips.
	h.Status.Health = infravirtrigaudiov1beta1.HostHealthNotReady
	require.NoError(t, r.Status().Update(context.Background(), h))
	got := reconcilePool(t, r, "pool-a")
	assert.Equal(t, int32(2), patches.Load())
	assert.Zero(t, got.Status.ReadyHosts)
	assert.Zero(t, got.Status.SchedulableHosts)
	assert.Equal(t, reasonHostPoolNoSchedulableHosts, poolReady(t, got).Reason)
}

// A missing pool is not an error, and a pool being deleted is left alone.
func TestHostPoolReconciler_MissingOrDeletingPool(t *testing.T) {
	var patches atomic.Int32
	deleting := testHostPool("going", "prov-a")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"example.com/hold"}
	r := newHostPoolReconciler(t, &patches, deleting,
		poolHost(poolTestNS, "h1", "going", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: poolTestNS, Name: "absent"}})
	require.NoError(t, err)
	got := reconcilePool(t, r, "going")
	assert.Zero(t, got.Status.TotalHosts)
	assert.Zero(t, patches.Load())
}

// A Host maps to the pool its spec.poolRef names, in its own namespace.
func TestHostPoolForHost(t *testing.T) {
	h := poolHost("ns-1", "h1", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true)
	reqs := hostPoolForHost(context.Background(), h)
	require.Len(t, reqs, 1)
	assert.Equal(t, types.NamespacedName{Namespace: "ns-1", Name: "pool-a"}, reqs[0].NamespacedName)

	h.Spec.PoolRef.Name = ""
	assert.Empty(t, hostPoolForHost(context.Background(), h))
	assert.Empty(t, hostPoolForHost(context.Background(), testHostPool("p", "prov-a")))
}

// Only a Host update that can move a pool's counts passes the predicate; a
// heartbeat does not.
func TestHostPoolMembershipChanged(t *testing.T) {
	p := hostPoolMembershipChanged()
	base := poolHost(poolTestNS, "h1", "pool-a", "prov-a", infravirtrigaudiov1beta1.HostHealthReady, true)

	for name, tc := range map[string]struct {
		mutate func(h *infravirtrigaudiov1beta1.Host)
		want   bool
	}{
		"heartbeat only": {func(h *infravirtrigaudiov1beta1.Host) {
			beat := metav1.Now()
			h.Status.LastHeartbeatTime = &beat
			cpu := int32(8)
			h.Status.AllocatableCPU = &cpu
		}, false},
		"labels only":       {func(h *infravirtrigaudiov1beta1.Host) { h.Spec.Labels = map[string]string{"a": "b"} }, false},
		"poolRef moved":     {func(h *infravirtrigaudiov1beta1.Host) { h.Spec.PoolRef.Name = "pool-b" }, true},
		"providerRef moved": {func(h *infravirtrigaudiov1beta1.Host) { h.Spec.ProviderRef.Name = "prov-b" }, true},
		"cordoned":          {func(h *infravirtrigaudiov1beta1.Host) { h.Spec.Schedulable = false }, true},
		"health changed": {func(h *infravirtrigaudiov1beta1.Host) {
			h.Status.Health = infravirtrigaudiov1beta1.HostHealthNotReady
		}, true},
		"deletion started": {func(h *infravirtrigaudiov1beta1.Host) {
			now := metav1.Now()
			h.DeletionTimestamp = &now
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			newHost := base.DeepCopy()
			tc.mutate(newHost)
			assert.Equal(t, tc.want, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: newHost}))
		})
	}
	assert.True(t, p.Create(event.CreateEvent{Object: base}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: base}))
}

// The manager may write HostPool status in both chart RBAC scopes and in the
// kubebuilder-generated role, and never the HostPool itself.
func TestRBAC_HostPoolStatusWritable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "virtrigaud", "templates", "manager-rbac.yaml"))
	require.NoError(t, err)
	tpl := string(raw)
	clusterIdx := strings.Index(tpl, "\nkind: ClusterRole\n")
	roleIdx := strings.Index(tpl, "\nkind: Role\n")
	require.Positive(t, clusterIdx)
	require.Less(t, clusterIdx, roleIdx)
	statusVerbs := []string{"get", "patch", "update"}
	for name, section := range map[string]string{
		"ClusterRole (rbac.scope=cluster)": tpl[clusterIdx:roleIdx],
		"Role (rbac.scope=namespace)":      tpl[roleIdx:],
	} {
		status := chartRuleVerbs(t, section, "hostpools/status")
		require.Len(t, status, 1, "%s: exactly one rule covers hostpools/status", name)
		assert.Equal(t, statusVerbs, status[0], name)
		pools := chartRuleVerbs(t, section, "hostpools")
		require.Len(t, pools, 1, "%s: exactly one rule covers hostpools", name)
		assert.Equal(t, readOnlyVerbs, pools[0], "%s: hostpools is read-only", name)
	}

	gen, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	require.NoError(t, err)
	var role struct {
		Rules []struct {
			Resources []string `json:"resources"`
			Verbs     []string `json:"verbs"`
		} `json:"rules"`
	}
	require.NoError(t, yaml.Unmarshal(gen, &role))
	var found bool
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			if res == "hostpools/status" {
				found = true
				assert.Equal(t, statusVerbs, rule.Verbs)
			}
		}
	}
	assert.True(t, found, "config/rbac/role.yaml grants hostpools/status")
}
