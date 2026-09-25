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

package assume

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// fakeClock is a settable clock, safe for concurrent use.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)} }

func assumption(uid, host string) Assumption {
	return Assumption{UID: uid, Namespace: "ns", Name: "vm-" + uid, HostID: host,
		Resources: scheduler.ResourceRequest{CPU: 2, MemoryMiB: 1024}}
}

func uids(as []Assumption) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.UID)
	}
	sort.Strings(out)
	return out
}

func TestAssumeListForget(t *testing.T) {
	c := New(time.Minute, newClock().now)
	c.Assume("p1", assumption("a", "h1"))
	c.Assume("p1", assumption("b", "h2"))
	c.Assume("p2", assumption("c", "h1"))

	assert.Equal(t, []string{"a", "b"}, uids(c.List("p1", nil)))
	assert.Equal(t, []string{"c"}, uids(c.List("p2", nil)), "assumptions are per Provider")
	assert.Equal(t, 3, c.Len())

	c.Forget("a")
	c.Forget("does-not-exist")
	assert.Equal(t, []string{"b"}, uids(c.List("p1", nil)))
}

func TestAssumeReplacesTheVMsEarlierAssumption(t *testing.T) {
	c := New(time.Minute, newClock().now)
	c.Assume("p1", assumption("a", "h1"))
	c.Assume("p1", assumption("a", "h2"))
	got := c.List("p1", nil)
	require.Len(t, got, 1, "one VM has at most one assumption")
	assert.Equal(t, "h2", got[0].HostID)

	// Even across Providers (a VM re-pointed before it was bound).
	c.Assume("p2", assumption("a", "h9"))
	assert.Empty(t, c.List("p1", nil))
	assert.Len(t, c.List("p2", nil), 1)
}

func TestListDropsSettledAssumptions(t *testing.T) {
	c := New(time.Minute, newClock().now)
	c.Assume("p1", assumption("recorded", "h1"))
	c.Assume("p1", assumption("in-flight", "h1"))
	c.Assume("p2", assumption("other-provider", "h1"))

	// The informer now shows "recorded"'s pendingHost: the record takes over.
	got := c.List("p1", func(a Assumption) bool { return a.UID == "recorded" })
	assert.Equal(t, []string{"in-flight"}, uids(got))
	assert.Equal(t, 2, c.Len(), "the settled assumption is gone for good")
	assert.Equal(t, []string{"in-flight"}, uids(c.List("p1", nil)))
	assert.Equal(t, []string{"other-provider"}, uids(c.List("p2", func(Assumption) bool { return false })),
		"another Provider's assumptions are never settled by this Provider's snapshot")
}

func TestAssumptionExpiresAfterTTL(t *testing.T) {
	clk := newClock()
	c := New(2*time.Minute, clk.now)
	c.Assume("p1", assumption("a", "h1"))
	c.Assume("p2", assumption("b", "h1"))

	clk.advance(2*time.Minute - time.Second)
	assert.Len(t, c.List("p1", nil), 1, "still live just before the TTL")

	clk.advance(time.Second)
	assert.Empty(t, c.List("p1", nil), "gone at the TTL")
	assert.Zero(t, c.Len(), "List drops expired assumptions of every Provider")

	// A re-assumption restarts the TTL.
	c.Assume("p1", assumption("a", "h1"))
	clk.advance(time.Minute)
	c.Assume("p1", assumption("a", "h1"))
	clk.advance(90 * time.Second)
	assert.Len(t, c.List("p1", nil), 1)
}

func TestTouchRestartsTheTTLAndAssumeCopiesLabels(t *testing.T) {
	clk := newClock()
	c := New(2*time.Minute, clk.now)
	labels := map[string]string{"app": "db"}
	a := assumption("a", "h1")
	a.Labels = labels
	a.Resize = true
	c.Assume("p1", a)
	labels["app"] = "mutated"

	clk.advance(90 * time.Second)
	c.Touch("a")
	c.Touch("no-such-vm")
	clk.advance(90 * time.Second)
	got := c.List("p1", nil)
	require.Len(t, got, 1, "touched 90s ago: still live")
	assert.True(t, got[0].Resize)
	assert.Equal(t, "db", got[0].Labels["app"], "Assume keeps its own copy of the labels")

	clk.advance(2 * time.Minute)
	assert.Empty(t, c.List("p1", nil))
}

func TestLockIsPerProvider(t *testing.T) {
	c := New(time.Minute, nil)
	unlock1 := c.Lock("p1")
	// A different Provider's lock is free while p1's is held (this would
	// deadlock the test if Lock were global).
	unlock2 := c.Lock("p2")
	unlock2()
	unlock1()
	// And p1's lock is reusable once released.
	c.Lock("p1")()
}

// TestLockWithinGivesUp (review M1): a waiter gives up after its bound or when
// its context ends, instead of parking forever behind a held lock.
func TestLockWithinGivesUp(t *testing.T) {
	c := New(time.Minute, nil)
	unlock, ok := c.LockWithin(context.Background(), "p1", time.Second)
	require.True(t, ok, "a free lock is taken at once")

	_, ok = c.LockWithin(context.Background(), "p1", 10*time.Millisecond)
	assert.False(t, ok, "a held lock is not taken within the bound")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok = c.LockWithin(ctx, "p1", time.Hour)
	assert.False(t, ok, "a cancelled context gives up")

	other, ok := c.LockWithin(context.Background(), "p2", 10*time.Millisecond)
	require.True(t, ok, "another Provider's lock is independent")
	other()

	unlock()
	again, ok := c.LockWithin(context.Background(), "p1", 10*time.Millisecond)
	require.True(t, ok, "free again once released")
	again()
}

// scheduleConcurrently runs one goroutine per VM, each doing what the
// controller does under the Provider lock: read the committed placements
// (here: none durable, so only assumptions), schedule, assume. It returns the
// host each VM got ("" for a no-fit).
func scheduleConcurrently(t *testing.T, c *Cache, vms []Assumption, base scheduler.Request) map[string]string {
	t.Helper()
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		result = map[string]string{}
		errs   []error
		start  = make(chan struct{})
	)
	for _, vm := range vms {
		wg.Add(1)
		go func(vm Assumption) {
			defer wg.Done()
			<-start
			unlock := c.Lock("p1")
			defer unlock()
			req := base
			req.VMUID = vm.UID
			req.Resources = vm.Resources
			for _, a := range c.List("p1", nil) {
				req.PlacedVMs = append(req.PlacedVMs, scheduler.PlacedVM{
					Name: a.Name, UID: a.UID, HostID: a.HostID, Labels: a.Labels, Resources: a.Resources,
					CapacityOnly: a.Namespace != vm.Namespace,
				})
			}
			res, err := scheduler.Schedule(req)
			host := ""
			if err == nil {
				host = res.HostID
				vm.HostID = host
				c.Assume("p1", vm)
			}
			mu.Lock()
			result[vm.UID] = host
			if err != nil {
				errs = append(errs, err)
			}
			mu.Unlock()
		}(vm)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.ErrorIs(t, err, scheduler.ErrNoFeasibleHost, "only a no-fit may fail a schedule")
	}
	return result
}

func readyHost(name string, cpu int32, memMiB int64) v1beta1.Host {
	return v1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1beta1.HostSpec{Schedulable: true},
		Status: v1beta1.HostStatus{
			Health: v1beta1.HostHealthReady, AllocatableCPU: &cpu, AllocatableMemoryMiB: &memMiB,
		},
	}
}

// TestConcurrentSchedulesNeverOverbook: 20 VMs race for a host that fits
// exactly 7 of them. Exactly 7 are placed and 13 are not.
func TestConcurrentSchedulesNeverOverbook(t *testing.T) {
	const n, k = 20, 7
	for round := 0; round < 25; round++ {
		c := New(time.Minute, nil)
		var vms []Assumption
		for i := 0; i < n; i++ {
			vms = append(vms, assumption(fmt.Sprintf("vm-%02d", i), ""))
		}
		base := scheduler.Request{
			Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
			Candidates: []v1beta1.Host{readyHost("h1", 2*k, 1<<20)},
		}
		got := scheduleConcurrently(t, c, vms, base)
		placed := 0
		for _, h := range got {
			if h != "" {
				placed++
			}
		}
		require.Equal(t, k, placed, "round %d: exactly %d placements", round, k)
		require.Equal(t, k, c.Len())
	}
}

// TestConcurrentMutualHardAntiAffinityNeverColocates: two db VMs that must not
// share a host race for two hosts that could each fit both.
func TestConcurrentMutualHardAntiAffinityNeverColocates(t *testing.T) {
	antiDB := &v1beta1.VMPlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "anti-db"},
		Spec: v1beta1.VMPlacementPolicySpec{
			AntiAffinity: &v1beta1.AntiAffinityRules{
				VMAntiAffinity: &v1beta1.VMAntiAffinity{
					RequiredDuringScheduling: []v1beta1.VMAffinityTerm{{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
					}},
				},
			},
		},
	}
	for round := 0; round < 100; round++ {
		c := New(time.Minute, nil)
		a, b := assumption("db-0", ""), assumption("db-1", "")
		a.Labels = map[string]string{"app": "db"}
		b.Labels = map[string]string{"app": "db"}
		base := scheduler.Request{
			Policy:     antiDB,
			Pool:       v1beta1.HostPoolSpec{Strategy: v1beta1.PoolStrategySpread},
			Candidates: []v1beta1.Host{readyHost("h1", 16, 1<<20), readyHost("h2", 16, 1<<20)},
		}
		got := scheduleConcurrently(t, c, []Assumption{a, b}, base)
		require.NotEmpty(t, got["db-0"], "round %d", round)
		require.NotEmpty(t, got["db-1"], "round %d", round)
		require.NotEqual(t, got["db-0"], got["db-1"], "round %d: hard anti-affine VMs landed together", round)
	}
}
