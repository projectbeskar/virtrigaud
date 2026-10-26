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
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/schedbench"
)

// Spike 1A controller-level benchmarks (docs/performance/spike-1a-scheduler-baseline.md).
//
// They run the production placement path that executes under a Provider's
// assume lock — scheduleUnderLock → scheduleAndAssume → placementRequest →
// committedPlacementsFrom (+ assume.Cache.List) → scheduler.Schedule →
// assume.Cache.Assume — against synthetic fixtures held in the informerStore
// seam (placement_bench_seam_test.go). Everything outside the lock (the
// pendingHost status write, the Create RPC, the informer's watch) is outside
// the spike; the contention harness models only how long it keeps a worker
// away from the lock (writeDelay) and when the write becomes visible (landing).
// Run with hack/bench/spike1a.sh.

// benchEnv names the harness knobs read from the environment.
const (
	// contentionDecisionsEnv overrides how many decisions a contention run
	// makes (default: per scenario, see contentionDecisions).
	contentionDecisionsEnv = "SCHEDBENCH_DECISIONS"
	// selfApp is the label the scheduled VMs carry.
	selfApp = 3
)

// benchPlacement is one Provider's fixture loaded into an informerStore,
// with a reconciler reading through it.
type benchPlacement struct {
	fixture  *schedbench.Fixture
	provider types.NamespacedName
	key      string
}

// benchWorld is one or more Providers sharing one store, one reconciler and
// one assume cache — as they share one manager.
type benchWorld struct {
	store     *informerStore
	r         *VirtualMachineReconciler
	cache     *assume.Cache
	providers []*benchPlacement
}

// newBenchWorld loads the fixtures into one store.
func newBenchWorld(fixtures ...*schedbench.Fixture) *benchWorld {
	w := &benchWorld{store: newInformerStore()}
	w.r = &VirtualMachineReconciler{Client: &storeClient{store: w.store}}
	w.cache = w.r.placementAssumptions()
	seenClass := map[string]bool{}
	for _, f := range fixtures {
		for i := range f.VMs {
			w.store.mustAdd(&f.VMs[i])
		}
		for i := range f.Classes {
			k := f.Classes[i].Namespace + "/" + f.Classes[i].Name
			if !seenClass[k] {
				seenClass[k] = true
				w.store.mustAdd(&f.Classes[i])
			}
		}
		p := types.NamespacedName{Namespace: f.Config.ProviderNamespace, Name: f.Config.ProviderName}
		w.providers = append(w.providers, &benchPlacement{fixture: f, provider: p, key: p.String()})
	}
	return w
}

// heapMiB returns the live heap after a GC, in MiB.
func heapMiB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

// scenarioVM returns the unplaced VM a scenario schedules, added to the store
// (the VM being reconciled is in the informer cache, unplaced).
func (w *benchWorld) scenarioVM(p *benchPlacement, name string, class schedbench.Class) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := p.fixture.NewVM(name, schedbench.SelfNamespace, class, selfApp)
	w.store.mustAdd(vm)
	return vm
}

// scenarioRequest is the Request resolveClusterPlacement hands to
// scheduleUnderLock: pool, candidates, policy and exclusions, nothing
// committed yet.
func scenarioRequest(p *benchPlacement, s schedbench.Scenario) scheduler.Request {
	req := p.fixture.Request(s.Class)
	s.Apply(p.fixture, &req)
	return req
}

// lastWorld memoises the most recent single-Provider world (consecutive
// scenarios share a fixture); keeping one bounds the live heap.
var (
	lastWorldCfg schedbench.Config
	lastWorld    *benchWorld
)

// worldFor builds (or reuses) the single-Provider world of cfg.
func worldFor(cfg schedbench.Config) *benchWorld {
	if lastWorld == nil || lastWorldCfg != cfg {
		lastWorld = nil
		runtime.GC()
		lastWorld = newBenchWorld(schedbench.Build(cfg))
		lastWorldCfg = cfg
	}
	return lastWorld
}

// expectOutcome fails b when the decision contradicts the scenario.
func expectOutcome(b *testing.B, s schedbench.Scenario, err error) {
	b.Helper()
	switch {
	case err == nil && s.Expect == schedbench.ExpectNoFit:
		b.Fatalf("%s: expected no feasible host", s.Name)
	case err != nil && !errors.Is(err, scheduler.ErrNoFeasibleHost):
		b.Fatalf("%s: %v", s.Name, err)
	case err != nil && s.Expect == schedbench.ExpectFit:
		b.Fatalf("%s: expected a feasible host: %v", s.Name, err)
	}
}

// BenchmarkPlacementScan times committedPlacementsFrom: the informer-cache
// List of the Provider's VMs through the placement-provider index and the
// per-VM accounting (holdsPlacement, placementHosts, sizeForAccounting).
// store-heap-MiB is the live heap with the fixture loaded.
func BenchmarkPlacementScan(b *testing.B) {
	ctx := context.Background()
	for _, s := range baseScenarios() {
		b.Run(s.Name, func(b *testing.B) {
			w := worldFor(s.Config)
			p := w.providers[0]
			vm := w.scenarioVM(p, "scan-"+s.Name, s.Class)
			heap := heapMiB()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := committedPlacementsFrom(ctx, w.r.Client, p.provider, vm); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(heap, "store-heap-MiB")
		})
	}
}

// BenchmarkPlacementDecision times one complete placement decision as the
// controller makes it: scheduleUnderLock (uncontended lock, scan, assumption
// settle, Schedule, Assume). Each iteration schedules the same VM, so its
// assumption is replaced rather than accumulated.
func BenchmarkPlacementDecision(b *testing.B) {
	ctx := context.Background()
	for _, s := range schedbench.Scenarios() {
		b.Run(s.Name, func(b *testing.B) {
			w := worldFor(s.Config)
			p := w.providers[0]
			vm := w.scenarioVM(p, "decide", s.Class)
			base := scenarioRequest(p, s)
			req := base
			_, locked, err := w.r.scheduleUnderLock(ctx, w.cache, p.key, p.provider, vm, &req)
			if !locked {
				b.Fatal("uncontended lock not acquired")
			}
			expectOutcome(b, s, err)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := base
				_, _, _ = w.r.scheduleUnderLock(ctx, w.cache, p.key, p.provider, vm, &req)
			}
			b.StopTimer()
			w.cache.Forget(vmSchedulingUID(vm))
		})
	}
}

// BenchmarkPlacementBreakdown splits a decision into the committed-placement
// read (placedWithAssumptionsFrom: scan + assumption settle) and Schedule, and
// reports each as a custom metric and the scan's share. It calls the same
// functions scheduleAndAssume calls, in the same order.
func BenchmarkPlacementBreakdown(b *testing.B) {
	ctx := context.Background()
	for _, s := range schedbench.Scenarios() {
		b.Run(s.Name, func(b *testing.B) {
			w := worldFor(s.Config)
			p := w.providers[0]
			vm := w.scenarioVM(p, "breakdown", s.Class)
			base := scenarioRequest(p, s)
			var scan, sched time.Duration
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := base
				t0 := time.Now()
				placed, _, err := placedWithAssumptionsFrom(ctx, w.r.Client, w.cache, p.key, p.provider, vm)
				if err != nil {
					b.Fatal(err)
				}
				req.PlacedVMs = placed
				req.VMUID = vmSchedulingUID(vm)
				t1 := time.Now()
				_, err = scheduler.Schedule(req)
				t2 := time.Now()
				if i == 0 {
					expectOutcome(b, s, err)
				}
				scan += t1.Sub(t0)
				sched += t2.Sub(t1)
			}
			b.StopTimer()
			n := float64(b.N)
			b.ReportMetric(float64(scan.Nanoseconds())/n, "scan-ns/op")
			b.ReportMetric(float64(sched.Nanoseconds())/n, "schedule-ns/op")
			b.ReportMetric(100*float64(scan)/float64(scan+sched), "scan-%")
		})
	}
}

// baseScenarios returns the grid's base scenarios.
func baseScenarios() []schedbench.Scenario {
	var out []schedbench.Scenario
	for _, s := range schedbench.Scenarios() {
		if len(s.Name) > 5 && s.Name[len(s.Name)-5:] == "/base" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Contention harness
// ---------------------------------------------------------------------------

// contentionConfig is one contention run.
type contentionConfig struct {
	// workers is the number of concurrent schedulers (the controller's
	// MaxConcurrentReconciles is 10).
	workers int
	// decisions is how many VMs are scheduled in total.
	decisions int
	// writeDelay is how long a worker stays away from the lock after a
	// decision: a stand-in for the pendingHost status write, which the
	// controller makes after releasing the lock.
	writeDelay time.Duration
	// landLag is how long after the write the record becomes visible in the
	// store (informer lag); 0 lands it synchronously after writeDelay.
	landLag time.Duration
	// lockWait is the bound on waiting for the lock; production uses
	// placementLockWait (5 s).
	lockWait time.Duration
	// class is the scheduled VMs' size.
	class schedbench.Class
}

// contentionResult is what one run observed.
type contentionResult struct {
	wall    time.Duration
	placed  int
	nofit   int
	busy    int
	waits   []time.Duration
	crit    []time.Duration
	byProv  []int
	landing sync.WaitGroup
}

// runContention schedules cfg.decisions new VMs from cfg.workers goroutines.
// Decision n goes to Provider n mod len(w.providers). Each worker repeats the
// body of scheduleUnderLock — LockWithin, lockBoundContext, scheduleAndAssume,
// unlock — with its own clock reads around LockWithin, so lock wait and
// critical section are timed apart. It then models the status write: after
// writeDelay (outside the lock) and landLag it stores the VM with
// placement.pendingHost, pendingResources and memoryCeilingMiB set, as the
// informer would after recordPendingHost.
func runContention(tb testing.TB, w *benchWorld, cfg contentionConfig) *contentionResult {
	tb.Helper()
	if cfg.lockWait == 0 {
		cfg.lockWait = placementLockWait
	}
	ctx := context.Background()
	res := &contentionResult{byProv: make([]int, len(w.providers))}
	var next atomic.Int64
	var mu sync.Mutex
	vms := make([]*infravirtrigaudiov1beta1.VirtualMachine, cfg.decisions)
	for n := range vms {
		p := w.providers[n%len(w.providers)]
		vms[n] = w.scenarioVM(p, fmt.Sprintf("c%07d", n), cfg.class)
	}
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var waits, crits []time.Duration
			placed, nofit, busy := 0, 0, 0
			perProv := make([]int, len(w.providers))
			for {
				n := int(next.Add(1)) - 1
				if n >= cfg.decisions {
					break
				}
				pi := n % len(w.providers)
				p := w.providers[pi]
				vm := vms[n]
				req := p.fixture.Request(cfg.class)

				t0 := time.Now()
				unlock, ok := w.cache.LockWithin(ctx, p.key, cfg.lockWait)
				t1 := time.Now()
				if !ok {
					busy++
					waits = append(waits, t1.Sub(t0))
					continue
				}
				lockCtx, cancel := lockBoundContext(ctx)
				result, err := w.r.scheduleAndAssume(lockCtx, w.cache, p.key, p.provider, vm, &req)
				cancel()
				unlock()
				t2 := time.Now()
				waits = append(waits, t1.Sub(t0))
				crits = append(crits, t2.Sub(t1))
				if err != nil {
					if !errors.Is(err, scheduler.ErrNoFeasibleHost) {
						tb.Errorf("schedule %s: %v", vm.Name, err)
					}
					nofit++
					continue
				}
				placed++
				perProv[pi]++
				if cfg.writeDelay > 0 {
					time.Sleep(cfg.writeDelay)
				}
				landed := vm.DeepCopy()
				zero := int64(0)
				landed.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{
					PendingHost:      result.HostID,
					PendingResources: &infravirtrigaudiov1beta1.PlacementResources{CPU: req.Resources.CPU, MemoryMiB: req.Resources.MemoryMiB},
					MemoryCeilingMiB: &zero,
				}
				landed.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: p.provider.Namespace, Name: p.provider.Name}
				land := func() {
					if err := w.store.vms.Update(landed); err != nil {
						tb.Errorf("land %s: %v", landed.Name, err)
					}
				}
				if cfg.landLag > 0 {
					res.landing.Add(1)
					time.AfterFunc(cfg.landLag, func() { defer res.landing.Done(); land() })
				} else {
					land()
				}
			}
			mu.Lock()
			res.waits = append(res.waits, waits...)
			res.crit = append(res.crit, crits...)
			res.placed += placed
			res.nofit += nofit
			res.busy += busy
			for i, c := range perProv {
				res.byProv[i] += c
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	res.wall = time.Since(start)
	res.landing.Wait()
	return res
}

// verifyNoOverbook checks every Provider's hosts against the accounting the
// controller uses (committedOnHost, the Host gauges' sum): committed CPU and
// memory never exceed allocatable × the pool's overcommit ratio. It also
// checks that every assumption has settled once all records landed.
func verifyNoOverbook(tb testing.TB, w *benchWorld) {
	tb.Helper()
	ctx := context.Background()
	for _, p := range w.providers {
		for i := range p.fixture.Hosts {
			h := &p.fixture.Hosts[i]
			cpu, mem, err := committedOnHost(ctx, w.r.Client, p.provider, h.Name)
			if err != nil {
				tb.Fatalf("committedOnHost %s: %v", h.Name, err)
			}
			if cpu > int64(*h.Status.AllocatableCPU) || mem > *h.Status.AllocatableMemoryMiB {
				tb.Errorf("provider %s host %s overbooked: committed %d vCPU / %d MiB, capacity %d vCPU / %d MiB",
					p.key, h.Name, cpu, mem, *h.Status.AllocatableCPU, *h.Status.AllocatableMemoryMiB)
			}
		}
		probe := p.fixture.NewVM("settle-probe", schedbench.SelfNamespace, schedbench.MediumRequest, selfApp)
		if _, _, err := placedWithAssumptionsFrom(ctx, w.r.Client, w.cache, p.key, p.provider, probe); err != nil {
			tb.Fatalf("placedWithAssumptionsFrom: %v", err)
		}
	}
	if n := w.cache.Len(); n != 0 {
		tb.Errorf("%d assumptions left after every record landed", n)
	}
}

// percentile returns the q-quantile of ds (sorted in place).
func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	slices.Sort(ds)
	return ds[min(len(ds)-1, int(q*float64(len(ds))))]
}

// sum adds ds.
func sum(ds []time.Duration) time.Duration {
	var t time.Duration
	for _, d := range ds {
		t += d
	}
	return t
}

// report publishes a run's metrics on b.
func (res *contentionResult) report(b *testing.B) {
	decided := float64(res.placed + res.nofit)
	b.ReportMetric(decided/res.wall.Seconds(), "decisions/s")
	b.ReportMetric(float64(percentile(res.waits, 0.5).Microseconds()), "wait-p50-us")
	b.ReportMetric(float64(percentile(res.waits, 0.99).Microseconds()), "wait-p99-us")
	b.ReportMetric(float64(percentile(res.crit, 0.5).Microseconds()), "cs-p50-us")
	b.ReportMetric(float64(percentile(res.crit, 0.99).Microseconds()), "cs-p99-us")
	waitT, critT := sum(res.waits), sum(res.crit)
	if waitT+critT > 0 {
		b.ReportMetric(100*float64(waitT)/float64(waitT+critT), "wait-%")
	}
	b.ReportMetric(float64(res.busy), "busy")
	b.ReportMetric(float64(res.nofit), "nofit")
}

// contentionDecisions is how many decisions a run makes: enough for stable
// percentiles, few enough that the 100,000-VM fixture finishes in minutes.
func contentionDecisions(vms int) int {
	if v, err := strconv.Atoi(os.Getenv(contentionDecisionsEnv)); err == nil && v > 0 {
		return v
	}
	if vms >= 100000 {
		return 400
	}
	return 2000
}

// contentionShape is one fixture the contention benchmarks run on.
type contentionShape struct {
	name  string
	hosts int
	vms   int
}

// contentionShapes are the single-Provider shapes; the partitioned runs split
// the largest into 2, 4 and 8 Providers of equal size.
var contentionShapes = []contentionShape{
	{name: "h100_v10k", hosts: 100, vms: 10000},
	{name: "h1000_v100k", hosts: 1000, vms: 100000},
}

// partitionedFixtures splits hosts/VMs into n equal Providers.
func partitionedFixtures(hosts, vms, n int) []*schedbench.Fixture {
	out := make([]*schedbench.Fixture, 0, n)
	for i := 0; i < n; i++ {
		cfg := schedbench.Config{
			Hosts: hosts / n, VMs: vms / n, Utilization: 0.7, Strategy: infravirtrigaudiov1beta1.PoolStrategySpread,
			Namespaces: 50, PendingFraction: 0.01, UnrecordedFraction: 0.01,
			ProviderNamespace: schedbench.ProviderNamespace, ProviderName: fmt.Sprintf("%s-p%d", schedbench.ProviderName, i),
			Seed: uint64(1000 + i), NamePrefix: fmt.Sprintf("p%d-", i),
		}
		out = append(out, schedbench.Build(cfg))
	}
	return out
}

// workerCounts are the concurrency levels measured.
var workerCounts = []int{1, 4, 8, 16}

// BenchmarkPlacementContention runs the contention harness: W workers
// scheduling against one Provider (same-provider) and against P Providers of
// the same total size (partitioned), with and without a 5 ms stand-in for the
// status write outside the lock. Run with -benchtime=1x: each b.N iteration
// is a whole run. Each run gets a fresh world, so runs do not fill each
// other's hosts.
func BenchmarkPlacementContention(b *testing.B) {
	if os.Getenv(schedbench.SkipLargeEnv) == "true" {
		contentionShapes = contentionShapes[:1]
	}
	for _, shape := range contentionShapes {
		for _, providers := range []int{1, 2, 4, 8} {
			for _, delay := range []time.Duration{0, 5 * time.Millisecond} {
				for _, workers := range workerCounts {
					name := fmt.Sprintf("%s/providers%d/delay%s/w%d", shape.name, providers, delay, workers)
					b.Run(name, func(b *testing.B) {
						for i := 0; i < b.N; i++ {
							b.StopTimer()
							lastWorld = nil
							runtime.GC()
							w := newBenchWorld(partitionedFixtures(shape.hosts, shape.vms, providers)...)
							b.StartTimer()
							res := runContention(b, w, contentionConfig{
								workers: workers, decisions: contentionDecisions(shape.vms),
								writeDelay: delay, class: schedbench.MediumRequest,
							})
							b.StopTimer()
							verifyNoOverbook(b, w)
							res.report(b)
						}
					})
				}
			}
		}
	}
}
