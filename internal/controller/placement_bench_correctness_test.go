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
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// Spike 1A correctness checks. They keep the benchmark harness honest: the
// seam reads what the controller would read, and the contention harness —
// which drives the production lock, assume cache and accounting — never
// overbooks a host, never lets one Provider's decisions touch another's
// capacity, and makes no decision without the lock. Settling of assumptions
// against landed records is covered in depth by
// virtualmachine_placement_capacity_test.go; these run the same rules under
// concurrency.

// smallFixture returns a small, highly utilized pool for one Provider; its
// Hosts and VMs are prefixed with the Provider's name.
func smallFixture(provider string, seed uint64) *schedbench.Fixture {
	return schedbench.Build(schedbench.Config{
		Hosts: 20, VMs: 400, Utilization: 0.9, Strategy: infravirtrigaudiov1beta1.PoolStrategySpread,
		Namespaces: 5, PendingFraction: 0.05, UnrecordedFraction: 0.05,
		ProviderNamespace: schedbench.ProviderNamespace, ProviderName: provider, Seed: seed,
		NamePrefix: provider + "-",
	})
}

// placedKeys renders placed VMs as sortable strings.
func placedKeys(placed []scheduler.PlacedVM) []string {
	out := make([]string, 0, len(placed))
	for _, p := range placed {
		out = append(out, fmt.Sprintf("%s@%s:%d/%d:%t", p.UID, p.HostID, p.Resources.CPU, p.Resources.MemoryMiB, p.CapacityOnly))
	}
	slices.Sort(out)
	return out
}

// TestSchedBenchSeamMatchesFakeClient checks that committedPlacementsFrom
// returns the same placements through the informerStore seam as through
// controller-runtime's fake client with the same field index.
func TestSchedBenchSeamMatchesFakeClient(t *testing.T) {
	ctx := context.Background()
	f := smallFixture(schedbench.ProviderName, 7)
	w := newBenchWorld(f)
	vm := f.NewVM("probe", schedbench.SelfNamespace, schedbench.MediumRequest, selfApp)

	objs := make([]client.Object, 0, len(f.VMs)+len(f.Classes))
	for i := range f.VMs {
		objs = append(objs, f.VMs[i].DeepCopy())
	}
	for i := range f.Classes {
		objs = append(objs, f.Classes[i].DeepCopy())
	}
	fc := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithObjects(objs...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementHostIndex, placementHostIndexValue).Build()

	p := w.providers[0].provider
	viaSeam, err := committedPlacementsFrom(ctx, w.r.Client, p, vm)
	if err != nil {
		t.Fatal(err)
	}
	viaFake, err := committedPlacementsFrom(ctx, fc, p, vm)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := placedKeys(viaSeam.placed), placedKeys(viaFake.placed); !slices.Equal(got, want) {
		t.Fatalf("seam and fake client disagree: %d vs %d placements", len(got), len(want))
	}
	if len(viaSeam.placed) != len(f.VMs) {
		t.Fatalf("expected every fixture VM placed once, got %d of %d", len(viaSeam.placed), len(f.VMs))
	}
}

// TestSchedBenchContentionNeverOverbooks runs 16 workers against one nearly
// full Provider, with informer lag, asking for more VMs than fit: every
// decision is made under the lock, some fail as no-fit, none overbooks.
func TestSchedBenchContentionNeverOverbooks(t *testing.T) {
	for _, lag := range []time.Duration{0, 2 * time.Millisecond} {
		t.Run(fmt.Sprintf("lag%s", lag), func(t *testing.T) {
			w := newBenchWorld(smallFixture(schedbench.ProviderName, 11))
			res := runContention(t, w, contentionConfig{
				workers: 16, decisions: 600, landLag: lag, class: schedbench.MediumRequest,
			})
			if res.busy != 0 {
				t.Fatalf("%d lock acquisitions timed out", res.busy)
			}
			if res.placed+res.nofit != 600 {
				t.Fatalf("placed %d + nofit %d != 600", res.placed, res.nofit)
			}
			if res.nofit == 0 {
				t.Fatal("expected the pool to fill up (no no-fit decisions)")
			}
			verifyNoOverbook(t, w)
		})
	}
}

// TestSchedBenchPartitionsIsolated fills Provider A and checks Provider B:
// its committed placements name only its own hosts and VMs, A's
// assumptions never reach it, and B still places.
func TestSchedBenchPartitionsIsolated(t *testing.T) {
	ctx := context.Background()
	a := smallFixture(schedbench.ProviderName+"-a", 21)
	b := smallFixture(schedbench.ProviderName+"-b", 22)
	w := newBenchWorld(a, b)
	pa, pb := w.providers[0], w.providers[1]
	if got, want := len(w.store.vms.List()), len(a.VMs)+len(b.VMs); got != want {
		t.Fatalf("store holds %d VMs, want %d (fixture names collide)", got, want)
	}

	// Assume on A without landing: A's assumptions are live.
	for i := 0; i < 50; i++ {
		vm := w.scenarioVM(pa, fmt.Sprintf("a-%d", i), schedbench.MediumRequest)
		req := pa.fixture.Request(schedbench.MediumRequest)
		_, locked, _ := w.r.scheduleUnderLock(ctx, w.cache, pa.key, pa.provider, vm, &req)
		if !locked {
			t.Fatal("lock not acquired")
		}
	}
	probe := w.scenarioVM(pb, "b-probe", schedbench.MediumRequest)
	placed, _, err := placedWithAssumptionsFrom(ctx, w.r.Client, w.cache, pb.key, pb.provider, probe)
	if err != nil {
		t.Fatal(err)
	}
	bHosts := map[string]bool{}
	for i := range b.Hosts {
		bHosts[b.Hosts[i].Name] = true
	}
	bUIDs := map[string]bool{}
	for i := range b.VMs {
		bUIDs[string(b.VMs[i].UID)] = true
	}
	for _, p := range placed {
		if !bUIDs[p.UID] {
			t.Fatalf("provider B sees VM %s that is not B's", p.UID)
		}
		if !bHosts[p.HostID] {
			t.Fatalf("provider B sees a placement on host %s that is not B's", p.HostID)
		}
	}
	if len(placed) != len(b.VMs) {
		t.Fatalf("provider B: %d placements, want %d", len(placed), len(b.VMs))
	}
	req := pb.fixture.Request(schedbench.MediumRequest)
	if _, locked, err := w.r.scheduleUnderLock(ctx, w.cache, pb.key, pb.provider, probe, &req); !locked || err != nil {
		t.Fatalf("provider B decision: locked=%t err=%v", locked, err)
	}
}

// TestSchedBenchLockBusyMakesNoDecision holds a Provider's lock and checks
// that scheduleUnderLock gives up without scheduling or assuming anything —
// the path a reconcile takes before requeueing (placementLockBusyRetry).
func TestSchedBenchLockBusyMakesNoDecision(t *testing.T) {
	w := newBenchWorld(smallFixture(schedbench.ProviderName, 31))
	p := w.providers[0]
	vm := w.scenarioVM(p, "blocked", schedbench.MediumRequest)
	unlock := w.cache.Lock(p.key)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := p.fixture.Request(schedbench.MediumRequest)
	res, locked, err := w.r.scheduleUnderLock(ctx, w.cache, p.key, p.provider, vm, &req)
	if locked || err != nil || res.HostID != "" {
		t.Fatalf("expected no decision: locked=%t err=%v host=%q", locked, err, res.HostID)
	}
	if req.PlacedVMs != nil {
		t.Fatal("the committed placements were read without the lock")
	}
	if n := w.cache.Len(); n != 0 {
		t.Fatalf("%d assumptions recorded without the lock", n)
	}
}
