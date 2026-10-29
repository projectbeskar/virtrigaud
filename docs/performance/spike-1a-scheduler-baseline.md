# Spike 1A: scheduling performance and contention baseline

| | |
|---|---|
| **Status** | Experiment report. No production code changed; no architecture decided. |
| **Date** | 2026-10-25 |
| **Author** | William Rizzo ([@wrkode](https://github.com/wrkode)) |
| **Revision measured** | `ed74c8fee01c1b88168b3d96597ed873949196b6` (main), plus the untracked benchmark files listed in [Reproducing](#reproducing) |
| **Related** | [ADR-0007](../adr/0007-clustered-orchestrator-provider.md) Addendum A (scheduler-accuracy amendment: committed capacity and the assume cache) |
| **Raw results** | [`spike-1a/results/`](spike-1a/results/) |

This spike measures the clustered placement path that exists today: the
committed-capacity scan, the scheduler, the per-Provider assume lock and the
in-process assume cache. It is the baseline for choosing between (A) an
improved centralized scheduler, (B) shared-state scheduling with optimistic
concurrency, and (C) partitioned scheduling. It does not choose. Spike 1B
covers the Kubernetes API, reconciliation and provider costs this spike leaves
out ([section 9](#9-recommendation-for-spike-1b)).

Throughout, **observed** means measured on the environment below,
**projection** means arithmetic from observed numbers, and **hypothesis**
means not measured.

## Environment

| | |
|---|---|
| CPU | Intel Core i9-10900X @ 3.70 GHz, 10 cores / 20 threads |
| Memory | 125 GiB |
| OS | Ubuntu 24.04.2 LTS, kernel 6.8.0-55-generic x86_64 |
| Go | go1.26.10 linux/amd64, `GOMAXPROCS=20` (default) unless stated, `GOGC=100` |
| controller-runtime / client-go | v0.20.4 / v0.32.1 |
| Repetitions | `-count=5` for the micro-benchmarks (median reported; spread is half the min–max range); one run per contention configuration |

The machine was otherwise idle during the measured runs. It is a workstation,
not the manager's production shape: the chart's default manager limits are
**500m CPU and 512 MiB memory** (`charts/virtrigaud/values.yaml`). Section 4.5
shows the effect of a single CPU; nothing here measures CFS throttling under a
500m quota.

---

## 0. Summary

**Observed:**

- **One placement decision costs time and memory proportional to the number
  of VMs on the Provider. The number of hosts barely matters.** Decision
  latency is 1.9–2.2 ms at 1,000 VMs, 21 ms at 10,000 and 235–239 ms at 100,000.
  Going from 100 to 1,000 hosts at a fixed VM count changes it by less than
  the run-to-run spread.
- **77–83% of a decision is the committed-placement scan**
  (`committedPlacementsFrom`), for every scenario without VM affinity. It
  allocates about 1.4 KB per VM (133 MiB per scan at 100,000 VMs).
- **Inside the scheduler, filtering and scoring are cheap.** At 1,000 hosts,
  filter and score together take about 0.16 ms. Almost all of `Schedule`'s
  time is `indexPlaced`, which re-aggregates every placed VM: 28 ms at 100,000
  VMs.
- **About half of the CPU time of a decision is garbage collection**, driven
  by those allocations. With one CPU (`-cpu 1`), a 100,000-VM decision takes
  354–370 ms instead of 236 ms.
- **Adding workers to one Provider does not add throughput.** It stays flat
  at about 4.2 decisions/s (100,000 VMs) or about 40/s (10,000 VMs) for 1, 4,
  8 and 16 workers. Extra workers only queue on the lock: the wait share rises
  to 75% (4 workers), 87% (8) and 94% (16). At 16 workers and 100,000 VMs, the
  p99 lock wait is 3.8 s, close to the 5 s `placementLockWait` bound. No
  acquisition timed out in any run.
- **Partitioning the same pool into independent Providers scales more than
  linearly.** 1,000 hosts and 100,000 VMs split into 2, 4 and 8 Providers give
  16, 59 and 205 decisions/s with 16 workers, against 4.2/s for one Provider.
  Each partition scans fewer VMs, and partitions run in parallel.
- **VM (anti-)affinity is the most expensive scheduler feature.** It
  re-parses each label selector for every placed VM on every host: 365 ms per
  decision at 100,000 VMs in one namespace, about 1 million allocations.
- **Correctness held in every run.** No host was ever overbooked, every
  assumption settled, partitions stayed isolated, and a busy lock produced no
  decision. A throwaway negative control without the lock overbooked all 20
  hosts, which shows the check can fail.
- **Memory:** a real informer cache holds about 132 MiB for 100,000 of these
  VMs, and the benchmark process peaked at 541 MiB RSS, above the chart's
  512 MiB manager limit, before any other controller work.
- **The test seam is faithful.** Against a real API server and a real
  controller-runtime informer cache, the scan takes the same time and
  allocates the same bytes as through the seam, at 10,000 and 100,000 VMs
  ([section 4.9](#49-seam-fidelity-against-a-real-informer-cache)).

**What this does and does not say:**

- **The centralized design is not CPU-bound on the scheduling algorithm.** It
  is bound by a full re-scan of committed state on every decision. That scan
  can become incremental without changing the accounting model
  ([section 7](#7-immediate-opportunities-that-keep-correctness)).
- **Nothing here measures the API server, the status writes, provider RPCs or
  the controller's queue.** Those are where Spike 1B starts. The discovery
  report's projections put the 20 QPS client limit ahead of the scheduler for
  creates.
- **Nothing here justifies shared-state scheduling yet.** The observed
  bottleneck is per-decision work, not lock serialization of short critical
  sections. Optimistic concurrency would run the same O(V) scan in every
  worker and add conflicts on top
  ([section 5](#5-answers), questions 9–10).

---

## 1. Scope and what the benchmarks do not represent

**In scope:** everything that runs while a Provider's assume lock is held,
plus the lock itself:

```
resolveClusterPlacement (virtualmachine_controller.go:1986)
 └─ scheduleUnderLock            (virtualmachine_placement_capacity.go:114)
     ├─ assume.Cache.LockWithin  (assume.go:160)        lock, 5 s bound
     └─ scheduleAndAssume        (…placement_capacity.go:147)
         ├─ placementRequest → placedWithAssumptionsFrom (:615)
         │   ├─ committedPlacementsFrom (:433)
         │   │   ├─ listProviderVMs (placement_index.go:72)   informer List by field index
         │   │   └─ per VM: placementProviderKey, holdsPlacement, placementHosts,
         │   │              sizeForAccounting (:496) → classForSizing (:513) when needed
         │   └─ assume.Cache.List (assume.go:221)              settle + live assumptions
         ├─ scheduler.Schedule (scheduler.go:255)
         │   ├─ newEvalContext → indexPlaced (evalcontext.go:75, :106)
         │   ├─ filterHost × candidates (filter.go:71)
         │   └─ pickBest (score.go:56)
         └─ assume.Cache.Assume (assume.go:181)
```

**Out of scope, and so not represented:**

| Not represented | Why it matters | Where it is measured next |
|---|---|---|
| The pre-lock half of `resolveClusterPlacement`: HostPool/Host/policy reads, `guardFirstPlacement`, `markPlacementUID` | Runs per decision, outside the lock | Spike 1B (reconcile profile) |
| The `pendingHost` status write (`recordPendingHost`, `virtualmachine_clustered.go:75`) and every other API write | API latency, 20 QPS client limit, etcd | Spike 1B |
| The provider `Create` RPC and host execution | Dominates time-to-Ready | Spike 1B / provider spike |
| The informer's watch goroutine and real decode cost; informer lag | The harness applies updates synchronously or after a fixed lag | Section 4.9 checks the read side; Spike 1B the rest |
| controller-runtime's work queue, rate limiter, requeue backoffs, `MaxConcurrentReconciles: 10` scheduling | The harness drives workers in a tight loop; real reconciles interleave other work | Spike 1B |
| Steady-state reconciles of placed VMs (Describe, status writes) competing for the same CPU and cache | The manager is one process | Spike 1B |
| CFS throttling at a 500m CPU limit; memory limit pressure | Section 4.5 approximates with one CPU only | Spike 1B in a real pod |
| Resize admission (`admitClusteredResize`) and clone admission, which take the same lock | More lock holders per Provider | Not measured; same critical section shape |

## 2. The scheduling critical path

Complexity uses V = VMs placed on the Provider (all namespaces), H = candidate
hosts, A = live assumptions (all Providers), S = VMs in the scheduled VM's
namespace on one host, T = affinity terms. "Measured" is the share of a 100,000-VM, 1,000-host
decision (CPU profile [`decision.cpu.cum.txt`](spike-1a/results/profiles/decision.cpu.cum.txt)
and the breakdown benchmark).

| Step | Function | Complexity | Allocation behaviour | Measured |
|---|---|---|---|---|
| Acquire lock | `assume.Cache.LockWithin` (`assume.go:160`); per-Provider capacity-1 channel, 5 s bound | O(1); waits for the holder | none | 0 when uncontended; see 4.6 |
| Enumerate committed VMs | `listProviderVMs` → informer `ByIndex` + `apimeta.SetList` | O(V) | `UnsafeDisableDeepCopy` avoids deep copies, but `SetList` still copies every 728-byte `VirtualMachine` struct into a new `Items` array: ~73 MB at 100k VMs | 15% of CPU; 42% of bytes |
| Per-VM accounting | `committedPlacementsFrom` loop: `placementProviderKey`, `holdsPlacement`, `placementHosts`, `sizeForAccounting` | O(V); VMClass Get only for VMs without a recorded size (memoised per class and namespace) | one `[]string` per VM (`placementHosts`), a `recorded` map entry per VM (keyed by UID), an un-presized `placed` slice | with the List: 77–83% of wall time; 34% of bytes |
| Settle and add assumptions | `assume.Cache.List` (`assume.go:221`) under the cache's single mutex | O(A) over **all Providers'** assumptions | small | negligible in these runs |
| Index placed VMs | `indexPlaced` (`evalcontext.go:106`) | O(V): de-duplicates (UID, host) through a map of V entries, sums per host | three maps and a slice sized by V, although only H hosts exist | 10% of CPU; 15% of bytes; ~99% of `Schedule` time |
| Filter hosts | `filterHost` × H (`filter.go:71`) | O(H × (lists + features)); VM affinity adds O(H × S × T) | none on the normal path; capacity rejections build `HostRejection` records | 0.06 ms at 1,000 hosts (no affinity) |
| VM (anti-)affinity | `hostRunsMatchingVM` → `termMatchesVM` (`affinity.go:40, :57`) | O(H × S × T), **with a `LabelSelectorAsSelector` parse per placed VM** | selector and regexp allocations per placed VM | 110 ms filter at 100k VMs in one namespace |
| Score | `pickBest` (`score.go:56`): single min-scan, deterministic tie-break | O(H); **no sorting anywhere** | one `hostScore` per feasible host | 0.1 ms at 1,000 hosts |
| Record assumption | `assume.Cache.Assume` | O(1) | clones the VM's labels | negligible |
| Release lock | deferred `unlock` | O(1) | none | — |
| GC | runtime | proportional to bytes allocated above | — | ~45% of process CPU (`gcBgMarkWorker`) |

Two consequences of the code shape that the measurements confirm:

- **The decision does not get cheaper as the pool gets bigger in hosts.** It
  gets more expensive in VMs, because every decision rebuilds the committed
  view from scratch. Nothing is cached between decisions except the
  assumptions.
- **The work under the lock is pure CPU and memory.** No API call is made
  under it; the benchmark's client panics on any call other than Get and List.
  The lock's hold time is therefore exactly the decision cost above.

## 3. Method

### 3.1 Harness and test seams

| Component | File | What is real, what is a seam |
|---|---|---|
| Fixtures | `internal/scheduler/schedbench/fixture.go`, `scenarios.go` | Synthetic `Host`, `HostPool`, `VMClass`, `VirtualMachine` objects. Deterministic (seeded PCG). |
| Scheduler-only benchmarks | `internal/scheduler/schedule_bench_test.go` | Real `scheduler.Schedule`. `PlacedVMs` built by the fixture to match what the controller produces. |
| Phase seam | `internal/scheduler/export_bench_test.go` | Exposes `newEvalContext`, the filter loop and `pickBest` to the external test package. The filter loop repeats `Schedule`'s loop line for line, so phases can be timed apart. |
| Controller-level benchmarks | `internal/controller/placement_bench_test.go` | Real `scheduleUnderLock`, `scheduleAndAssume`, `committedPlacementsFrom`, `assume.Cache`, `committedOnHost`, on a real `VirtualMachineReconciler`. |
| Informer seam | `internal/controller/placement_bench_seam_test.go` | The store **is** client-go's `ThreadSafeStore` indexer, with the namespace index and the placement-provider field index registered as controller-runtime's informer cache registers them. Get and List repeat controller-runtime v0.20.4 `CacheReader` (an internal package that cannot be imported). Left out: the informer lookup by GVK (a map read) and the watch goroutine. |
| Contention harness | `runContention` in `placement_bench_test.go` | W goroutines repeat `scheduleUnderLock`'s body (`LockWithin`, `lockBoundContext`, `scheduleAndAssume`, unlock) with clock reads around `LockWithin`, so wait and hold are timed apart. The lock, constant and functions are the production ones; the lock is not replaced. After each decision a worker optionally sleeps `writeDelay` outside the lock (stand-in for the `pendingHost` write), then stores the VM with `pendingHost`, `pendingResources` and `memoryCeilingMiB`, as the informer would show it. |
| Correctness tests | `internal/controller/placement_bench_correctness_test.go` | Run in `make test` (about 12 s with `-race`). |
| Seam fidelity | `internal/controller/placement_bench_envtest_test.go` (`-tags schedbench_envtest`) | Real API server (envtest), real controller-runtime informer cache. |

### 3.2 Fixtures

- **Hosts:** Ready, schedulable, zone and rack labels, storage and network
  visibility labels (network on 90%), a 35-entry CPU feature list (`avx2` on
  80%, `avx512f` on 25%).
- **Host sizing:** every VM is placed first, weighted by host size; each host
  is then sized so its committed CPU and memory are exactly the target
  utilization (overcommit 1.0). Heterogeneous pools use 1×/2×/4× host classes
  (50/35/15%).
- **VMs:** class mix of 40% 1 vCPU/1 GiB, 35% 2/4 GiB, 20% 4/16 GiB and 5%
  8/32 GiB. Spread over 50 tenant namespaces, so 2% of them are in the
  scheduled VM's affinity scope. 1% have a create pending (`pendingHost`,
  `pendingResources`) and 1% are bound without a recorded size (forcing VMClass
  reads); the rest are bound with `currentResources` and `memoryCeilingMiB`.
  This is the steady state the accounting sees after a manager upgrade.
- **Grid** (hosts × VMs): 10×1k, 100×1k, 100×10k, 100×100k (dense microVM
  case), 500×10k, 500×100k, 1000×10k, 1000×100k. Left out: 10 hosts with 10k
  or more VMs (implausible density), and 500+ hosts with 1k VMs (fixture setup
  dominates).
- **Variants**, on 100×10k and 1000×100k:

| Variant | Change |
|---|---|
| `base` | 70% utilization, uniform, Spread, 2 vCPU / 4 GiB request |
| `util-low` / `util-high` | 30% / 95% utilization |
| `hetero` | 1×/2×/4× host classes |
| `binpack` | BinPack strategy |
| `req-small` / `req-large` | 1 vCPU / 1 GiB; 16 vCPU / 64 GiB |
| `unschedulable` | memory request of twice the largest host (every host rejected on capacity) |
| `constrained` | 16 excluded hosts (the `excludedHosts` cap), hard zone node-selector (25% of hosts), required `avx2`, required storage pool and network. The controller does not fill `RequiredStoragePools` today (TODO(ADR-0007 D6)); the scenario sets it to measure the filter as designed. |
| `affinity` | one namespace (every VM in affinity scope), required VM anti-affinity to a rare app, preferred VM affinity to a common tier |

### 3.3 Contention configurations

- **Shapes:** 100×10k and 1000×100k, each as 1 Provider or split into 2, 4
  and 8 equal Providers sharing one store, reconciler and assume cache (as
  they share one manager).
- **Workers:** 1, 4, 8 and 16. The controller's `MaxConcurrentReconciles` is
  10.
- **Write delay:** 0 (pure lock contention) and 5 ms outside the lock.
- **Decisions:** 2,000 per run for the 10k shape, 400 for the 100k shape.
- **Correctness:** checked after every run with the controller's own
  `committedOnHost`.

## 4. Results

All tables are medians of 5 runs unless stated; ± is half the min–max range.
Full data: [`controller.txt`](spike-1a/results/controller.txt),
[`scheduler.txt`](spike-1a/results/scheduler.txt), benchstat summaries next to
them.

### 4.1 Decision latency against VM and host count (observed)

`BenchmarkPlacementDecision/<shape>/base`: one complete `scheduleUnderLock`.

| Hosts \ VMs | 1,000 | 10,000 | 100,000 |
|---|---|---|---|
| 10 | 1.91 ms ±3% | — | — |
| 100 | 2.20 ms ±6% | 21.2 ms ±1% | 239 ms ±12% |
| 500 | — | 21.2 ms ±2% | 235 ms ±4% |
| 1,000 | — | 21.0 ms ±5% | 236 ms ±6% |

- **VM count:** latency grows linearly with V, about 2.1–2.4 µs per placed VM.
- **Host count:** 100 → 1,000 hosts at 10k VMs: 21.2 → 21.0 ms. At 100k VMs:
  239 → 236 ms. No measurable effect.

### 4.2 Where the decision goes (observed)

`BenchmarkPlacementBreakdown`: the same calls as `scheduleAndAssume`, timed apart.

| Shape | Scan (List + accounting + settle) | `Schedule` | Scan share |
|---|---|---|---|
| 10×1k | 1.64 ms | 0.42 ms | 79.5% |
| 100×10k | 17.3 ms | 4.1 ms | 80.7% |
| 1000×10k | 17.9 ms | 3.6 ms | 83.5% |
| 100×100k | 191 ms | 51 ms | 78.8% |
| 1000×100k | 187 ms | 56 ms | 77.1% |
| 100×10k affinity | 14.7 ms | 22.7 ms | 39.3% |
| 1000×100k affinity | 193 ms | 176 ms | 52.8% |

**Inside `Schedule`** (`BenchmarkSchedulePhases`, scheduler-only, `PlacedVMs`
prebuilt):

| Shape | index (`indexPlaced`) | filter | score | `Schedule` total |
|---|---|---|---|---|
| 100×10k | 2.89 ms | 0.009 ms | 0.012 ms | 3.14 ms |
| 1000×10k | 3.27 ms | 0.094 ms | 0.125 ms | 4.02 ms |
| 1000×100k | 28.3 ms | 0.057 ms | 0.106 ms | 27.8 ms |
| 1000×100k unschedulable | 29.4 ms | 0.43 ms | — | 32.0 ms |
| 1000×100k constrained | 28.9 ms | 0.23 ms | 0.016 ms | 32.4 ms |
| 1000×100k affinity | 43.7 ms | **110 ms** | 0.77 ms | 163 ms |

`Schedule` costs about 28 ms standalone at 100k VMs but 51–56 ms inside the
controller path. The difference is the garbage collector working off the scan's
allocations while `Schedule` runs. This is interference, not a separate cost.

### 4.3 Scenario variants (observed)

Decision latency (`BenchmarkPlacementDecision`):

| Variant | 100 hosts × 10k VMs | 1,000 hosts × 100k VMs |
|---|---|---|
| base | 21.2 ms | 236 ms |
| util-low (30%) | 21.3 ms | 234 ms |
| util-high (95%) | 21.0 ms | 240 ms |
| hetero | 21.8 ms | 238 ms |
| binpack | 21.2 ms | 236 ms |
| req-small | 20.9 ms | 237 ms |
| req-large | 21.6 ms | 237 ms |
| unschedulable | 21.1 ms | 236 ms |
| constrained | 20.8 ms | 251 ms |
| affinity | **36.1 ms** | **365 ms** |

Utilization, host heterogeneity, strategy, request size and no-fit outcomes
change nothing measurable: the decision is dominated by work that does not
depend on them. Only VM affinity matters.

### 4.4 Allocations and memory (observed)

| Shape | Scan B/op | Scan allocs/op | Decision B/op | Decision allocs/op | Fixture live heap |
|---|---|---|---|---|---|
| 10×1k | 1.22 MiB | 1.1k | 1.57 MiB | 1.2k | 4.6 MiB |
| 100×10k | 13.2 MiB | 10.5k | 16.2 MiB | 10.9k | 18.2 MiB |
| 1000×10k | 13.2 MiB | 10.5k | 16.2 MiB | 11.8k | 19.8 MiB |
| 1000×100k | 133 MiB | 101k | 158 MiB | 105k | 157 MiB |
| 1000×100k affinity | — | — | 213 MiB | 988k | — |

Allocation sources at 1000×100k
([`decision.alloc_space.top.txt`](spike-1a/results/profiles/decision.alloc_space.top.txt)):

| Source | Share of bytes |
|---|---|
| `SetList`'s `Items` array | 40% |
| `committedPlacementsFrom`'s own maps and slices | 34% |
| `indexPlaced` | 15% |

Process peak: **541 MiB max RSS** for the 1000×100k scan benchmark
([`maxrss.txt`](spike-1a/results/maxrss.txt)). That is a 157 MiB live set
(the fixture plus the store that indexes it) plus about 140 MB of garbage per
scan and GC headroom. It is already above the chart's 512 MiB manager limit.
Section 4.9 measures 132 MiB for a real informer cache's copy of 100,000 of
these VMs. [Projection] A real manager adds every other cached kind and
larger production VMs to the live set, and the per-decision garbage on top of
that.

### 4.5 One CPU (observed)

`BenchmarkPlacementDecision -cpu 1`: the GC cannot run on spare cores.

| Shape | 20 CPUs | 1 CPU | Ratio |
|---|---|---|---|
| 10×1k | 1.91 ms | 1.28 ms | 0.67 |
| 100×10k | 21.2 ms | 25.1 ms | 1.18 |
| 1000×10k | 21.0 ms | 26.7 ms | 1.27 |
| 100×100k | 239 ms | 361 ms | 1.51 |
| 1000×100k | 236 ms | 354 ms | 1.50 |

[Hypothesis] A 500m CFS quota is worse than one dedicated CPU: other reconciles
share it, and throttling adds latency in 100 ms periods. Not measured.

### 4.6 Contention (observed)

Full table: [`contention.txt`](spike-1a/results/contention.txt). "wait" is time
in `LockWithin`; "hold" is time holding the lock (the critical section);
"wait %" is wait / (wait + hold) summed over decisions. No run had a lock
timeout (`busy` = 0) or a no-fit.

**One Provider, 1,000 hosts × 100,000 VMs, no write delay:**

| Workers | Decisions/s | Hold p50 | Hold p99 | Wait p50 | Wait p99 | Wait % |
|---|---|---|---|---|---|---|
| 1 | 4.18 | 236 ms | 347 ms | 0 | 0 | 0% |
| 4 | 4.32 | 231 ms | 275 ms | 0.69 s | 0.76 s | 75% |
| 8 | 4.28 | 234 ms | 289 ms | 1.63 s | 1.75 s | 87% |
| 16 | 4.24 | 235 ms | 297 ms | 3.52 s | 3.79 s | 94% |

**One Provider, 100 hosts × 10,000 VMs, no write delay:**

| Workers | Decisions/s | Hold p50 | Wait p99 | Wait % |
|---|---|---|---|---|
| 1 | 44.3 | 21.9 ms | 0 | 0% |
| 4 | 42.7 | 22.9 ms | 100 ms | 75% |
| 8 | 39.2 | 24.7 ms | 259 ms | 87% |
| 16 | 38.7 | 24.0 ms | 810 ms | 94% |

With a 5 ms write delay outside the lock the single-Provider numbers are the
same within noise. Workers spend so long waiting that the time away from the
lock does not matter.

**Partitioned: the same 1,000 hosts × 100,000 VMs as P equal Providers, 16
workers:**

| Providers | VMs per Provider | Decisions/s | Speed-up vs 1 | Hold p50 | Wait p99 | Wait % |
|---|---|---|---|---|---|---|
| 1 | 100,000 | 4.24 | 1.0× | 235 ms | 3.79 s | 94% |
| 2 | 50,000 | 15.9 | 3.7× | 123 ms | 1.29 s | 87% |
| 4 | 25,000 | 58.9 | 13.9× | 67 ms | 280 ms | 75% |
| 8 | 12,500 | 205 | 48.4× | 39 ms | 101 ms | 46% |

**The same, with the fewest workers that keep every partition busy:**

| Providers | Workers | Decisions/s | Hold p50 |
|---|---|---|---|
| 1 | 1 | 4.18 | 236 ms |
| 2 | 4 | 16.1 | 122 ms |
| 4 | 4 | 57.0 | 67 ms |
| 8 | 8 | 196 | 36 ms |

Partitioning gains twice: each partition's scan is P times smaller, and P
partitions run in parallel. Hold times grow with concurrency at 8 Providers:
p50 is 20.6 ms with 1 worker and 39 ms with 16. Concurrent scans compete for
cores, memory bandwidth and the GC. The single assume-cache mutex did not
show up in the block profile.

The block profile of the 16-worker, one-Provider run
([`contention.block.top.txt`](spike-1a/results/profiles/contention.block.top.txt))
attributes all scheduling-side blocking to `assume.(*Cache).LockWithin`, which
is the production per-Provider lock.

### 4.7 Profiles (observed)

CPU, 1000×100k decision ([`decision.cpu.top.txt`](spike-1a/results/profiles/decision.cpu.top.txt),
[`decision.cpu.cum.txt`](spike-1a/results/profiles/decision.cpu.cum.txt)):

| Area | Share of process CPU |
|---|---|
| GC mark (`gcBgMarkWorker`) | 45% |
| `committedPlacementsFrom` | 39% |
| …of which the List (`SetList` copy) | 15% |
| `indexPlaced` | 10% |
| String hashing (`aeshashbody`), map access | ~17% flat, mostly the `recorded` and de-duplication maps |
| Scheduling filters and scoring | under 1% |
| Sorting | none; the scheduler does not sort |

VM affinity, 1000×100k ([`affinity.cpu.top.txt`](spike-1a/results/profiles/affinity.cpu.top.txt)):
`hostRunsMatchingVM` is 35% of CPU, with `regexp` backtracking from
`LabelSelectorAsSelector` validating the same selector once per placed VM. GC
is about 45%.

**Unexpectedly expensive:**

1. `apimeta.SetList` copies every `VirtualMachine` struct into a new `Items`
   array even with `UnsafeDisableDeepCopy`: about 73 MB per decision at 100k
   VMs.
2. `committedPlacementsFrom` builds a `recorded` map of every VM, but
   `settled` only looks up the few UIDs that have assumptions.
3. `indexPlaced` sizes its de-duplication and per-host maps by V, not H, and
   de-duplicates every entry. Only assumption entries can duplicate a record.
4. The affinity selector parse is repeated per placed VM per host.

### 4.8 Correctness (observed)

| Check | Test | Result |
|---|---|---|
| The seam returns what controller-runtime's fake client returns | `TestSchedBenchSeamMatchesFakeClient` | pass |
| 16 workers, 600 requests on a 90%-full pool, informer lag 0 and 2 ms: no host above allocatable × ratio; placed + no-fit = 600; every assumption settled | `TestSchedBenchContentionNeverOverbooks` | pass |
| Every contention benchmark run (64 configurations): no overbooking, assumptions settled | `verifyNoOverbook` after each run | pass |
| Two Providers in one store: B sees only its own VMs and hosts while A holds 50 live assumptions; B still places | `TestSchedBenchPartitionsIsolated` | pass |
| Lock held elsewhere: `scheduleUnderLock` returns `locked=false`, reads nothing, assumes nothing | `TestSchedBenchLockBusyMakesNoDecision` | pass |
| Negative control: the same 16 workers calling `placementRequest` + `Schedule` without the lock or `Assume` | throwaway, not kept | all 20 hosts overbooked (for example 88 vCPU on a 74 vCPU host) — the check detects violations |

These checks cover the decision only. They do not cover the durable binding
(the `pendingHost` write and its conflict handling) or host execution, which
are outside this spike. Assumption settling against landed records is covered
in depth by the existing `virtualmachine_placement_capacity_test.go`.

### 4.9 Seam fidelity against a real informer cache

`BenchmarkEnvtestScan` (`-tags schedbench_envtest`,
[`envtest-fidelity.txt`](spike-1a/results/envtest-fidelity.txt)) loads the
fixture into a real API server (envtest, kube-apiserver 1.32.0 from `bin/k8s`), reads it back
through a real controller-runtime informer cache with the placement-provider
index, and runs `committedPlacementsFrom` against that cache and against the
seam loaded with the very objects the cache decoded. Three runs of 10
iterations each:

| VMs | Real cache | Seam | Real cache B/op | Seam B/op | Cache live heap |
|---|---|---|---|---|---|
| 10,000 | 19.0 ms (16.7–19.1) | 18.4 ms (17.4–20.2) | 13.90 MB | 13.90 MB | 13.1 MiB |
| 100,000 | 183 ms (181–192) | 186 ms (185–187) | 139.9 MB | 139.9 MB | 131.8 MiB |

**The seam matches the real cache within run-to-run noise in time,
allocated bytes and allocation count (the real cache makes 1% more
allocations: the per-call informer lookup).** It also matches the
seam-only benchmark of section 4.1 (17.4 ms at 10k, 191–201 ms at 100k). The
real cache held 131.8 MiB for 100,000 of these VMs, about 1.35 KB each,
managed fields included. Production VMs with disks, networks, conditions and
IPs in their status are larger. The `SetList` copy does not grow with them
(the struct is 728 bytes either way), but GC scanning of the live cache
does.

Loading 100,000 VMs with status into envtest took about 3 minutes (64
writers at QPS 5,000). That is a property of envtest, not a measurement of
the API server.

## 5. Answers

1. **How does decision latency scale with VM count?** Linearly: 1.9–2.2 ms
   (1k), 21 ms (10k), 235–239 ms (100k), about 2.1–2.4 µs per placed VM on
   this CPU. With one CPU, 354–361 ms at 100k.

2. **With host count?** Not measurably at these sizes. Filtering and scoring
   1,000 hosts costs about 0.16 ms; 100 → 1,000 hosts moves a 10k-VM decision
   from 21.2 to 21.0 ms. Host count would matter only with policies whose
   per-host cost grows (VM affinity: O(H × S × T)).

3. **What share is committed-placement enumeration?** 77–83% of the decision
   (`committedPlacementsFrom`, including the informer List) without affinity;
   39–53% with VM affinity. Counting `indexPlaced`, which re-aggregates the
   same list inside `Schedule`, per-VM work is about 95% of a non-affinity
   decision.

4. **At what concurrency does lock contention become significant?** From the
   second worker on a single Provider. Throughput is flat from 1 to 16
   workers, so every worker beyond the first only waits. Wait is 75% of a
   worker's scheduling time at 4 workers, 94% at 16. Whether that matters
   depends on decision cost:
   - At 10k VMs, the p99 wait with 16 workers is 0.8 s.
   - At 100k VMs it is 3.8 s against a 5 s timeout.
   - [Projection] With the controller's 10 workers at 100k VMs, about 2.1–2.4 s
     on 20 CPUs. With per-decision cost at the one-CPU figure (354 ms), nine
     waiters reach about 3.2 s. A 500m manager that halves that again would
     exceed 5 s, and reconciles would start requeueing with `placementLockBusy`.

5. **How does memory scale?** Linearly in V:
   - about 1.4 KB allocated per VM per decision (158 MiB at 100k);
   - a live fixture of 157 MiB at 100k with seam-sized objects;
   - 541 MiB peak RSS for a scan loop at 100k VMs.

   The transient allocation rate, not the live set, drives the GC cost
   (45% of CPU).

6. **Do more workers improve throughput?** No, not for one Provider: 4.18 →
   4.24 decisions/s (100k) and 44 → 39 (10k, slightly worse as GC contention
   grows). Workers help only when they work on different Providers.

7. **How much parallelism across Provider partitions?** Each Provider's lock
   is independent, and partitions also shrink each scan:
   - 2/4/8 partitions of 100k VMs: 3.7×/13.9×/48× the single-Provider
     throughput at 16 workers.
   - 8 partitions of 10k VMs: 2,261 decisions/s.

   Per-decision hold time inflates 1.9× at 8 concurrent partitions from CPU
   and GC sharing. [Projection] On a 500m manager, cross-partition parallelism
   is capped by the quota, not the locks.

8. **Immediate opportunities that keep correctness:** see section 7. The
   largest is replacing the per-decision scan with an incremental per-host
   committed index. That changes O(V) to O(H) per decision, and must be
   proven equal to the scan.

9. **Evidence for keeping the centralized scheduler:**
   - Filtering and scoring, the part a shared-state design would parallelize,
     costs under 1% of a decision (0.16 ms for 1,000 hosts).
   - The dominant cost is rebuilding committed state, which every worker in a
     shared-state design would also have to do or keep incrementally.
   - Partitioning by Provider, which exists today, already gives 3.7–48×.
   - The lock never timed out and never let an overbooking through in any run.

   **Not evidence:** the low microbenchmark latency at 1k–10k VMs says nothing
   about end-to-end create throughput. That is limited by API writes and RPCs
   this spike does not measure.

10. **Evidence that would justify exploring shared-state scheduling:**
    - After the scan is made incremental (section 7), the decision under the
      lock is still a large fraction of end-to-end create latency, and the
      measured create rate per Provider is below the target. Hold time, not
      API writes, must be the limit (Spike 1B measures this).
    - Workloads require one large logical pool, so cannot be partitioned into
      Providers, and need more decisions per second than one serialized
      decision loop gives at that pool's H.
    - Per-decision compute grows with H rather than V: rich policies, GPU or
      NUMA topology filters, scoring that is expensive per host.
    - A conflict-rate model (Spike 2 of the discovery report) shows that, at
      the target utilization and scoring strategy, optimistic commits rarely
      collide. The current deterministic tie-break would make them collide
      often.

    None of these is shown by this spike.

## 6. Bottlenecks ranked by observed impact

| Rank | Bottleneck | Observed impact | Evidence |
|---|---|---|---|
| 1 | Full re-scan of the Provider's VMs on every decision (`committedPlacementsFrom` + `listProviderVMs`) | 77–83% of decision time; O(V); 133 MiB/decision at 100k | 4.1, 4.2, 4.7 |
| 2 | GC driven by per-decision allocation | ~45% of process CPU; ×1.5 decision latency on one CPU at 100k | 4.5, 4.7 |
| 3 | Serialization of the whole decision under one per-Provider lock | Throughput fixed at 1 / decision-time per Provider; p99 wait 3.8 s at 16 workers / 100k | 4.6 |
| 4 | `indexPlaced` re-aggregating all V entries inside `Schedule` | ~99% of `Schedule`, 28 ms at 100k | 4.2 |
| 5 | VM affinity: selector parsed per placed VM per host | +70% decision time at 10k, +55% at 100k (single namespace); 1M allocs at 100k | 4.2, 4.3 |
| 6 | Memory headroom | 541 MiB RSS for the scan loop alone at 100k vs a 512 MiB chart limit | 4.4 |
| — | Filtering, scoring, host count, assumption bookkeeping | under 1% each | 4.2 |

## 7. Immediate opportunities that keep correctness

None of these is implemented in this spike. Each changes production code and
needs its own review and an equivalence test against the current scan. They
are listed in order of expected impact, which is an estimate.

1. **Incremental committed index.** Maintain per-Provider, per-host committed
   sums and the affinity-visible VM labels from informer events (add, update,
   delete), keyed by UID. The scan would become an O(H) read of the index plus
   the assumptions.
   - It must reproduce `admittedFootprint`, `holdsPlacement`, `placementHosts`
     and the VMClass sizing exactly.
   - A VMClass change must re-size the VMs that depend on it.
   - It needs a shadow mode that compares it with the scan before it is
     trusted.
   - It keeps the accounting model, the lock and the single-writer assumption
     unchanged.
   - It is the largest change and the largest win.
2. **Read from the informer's indexer without `SetList`.** Use
   `GetInformer(...).GetIndexer().ByIndex` to get pointers instead of copying
   728-byte structs into a typed list. Expected: about 40% of the bytes and
   about 15% of CPU. Small change, same snapshot semantics.
3. **Shrink `committedPlacementsFrom`'s bookkeeping.**
   - Build the `recorded` map only for UIDs that have live assumptions (List
     can take the set first).
   - Pre-size `placed`.
   - Avoid the per-VM `placementHosts` slice for the common one-host case.
4. **Size `indexPlaced`'s maps by host count, and de-duplicate only where
   duplicates can occur** (assumption entries against records). This must keep
   the documented contract that any caller's duplicates are merged at the
   larger size; a cheap pre-check can keep that.
5. **Parse each affinity selector once per `Schedule` call**, not per placed VM
   per host.
6. **Measure before tuning `MaxConcurrentReconciles`.** More workers do not
   speed up placement for one Provider; they only queue. (Not a code change,
   but it affects how Spike 1B should be configured.)

Expected effect, as a hypothesis: items 2–5 together might halve decision
time; item 1 removes the V dependence. Both need measuring with these same
benchmarks.

## 8. Limitations

- **Synthetic objects.** The fixture's VMs have the fields the accounting
  reads, not the disks, networks, conditions and IPs of production VMs. Section
  4.9 shows the read path behaves the same through a real cache. Larger real
  objects would raise the live set and GC scanning, not the per-decision copy
  (728 bytes per struct either way).
- **Steady-state mix only.** 1% pending and 1% unrecorded. A burst of creates
  raises the pending share and the class reads, though class reads are
  memoised per class and namespace.
- **No other work in the process.** Real reconciles of 100k VMs compete for
  the same CPU, cache and GC.
- **A workstation with 20 hardware threads** and no CPU quota. Section 4.5 is
  only a proxy for a 500m limit.
- **Contention runs are single runs** with 400 decisions (100k) or 2,000
  (10k). Percentiles above p99 are not meaningful at 400 samples.
- **The write delay and informer lag are modelled, not measured.**
- **Not measured:** resize and clone admission under the same lock;
  `AssumeFor` TTL expiry under load; leader handover.
- **Run-to-run spread** is up to ±12% at 100k VMs. Comparisons below that are
  noise.

## 9. Recommendation for Spike 1B

**Question:** with the scheduler cost now known, which limit does a
real manager hit first on the create path and in steady state: the client QPS
budget, the per-Provider lock, the GC and CPU quota, or the provider RPCs?

**Minimal experiment:**

- **Environment:**
  - A kind (or kwok) cluster with the real manager image, deployed by the
    chart at its default limits (500m / 512 MiB) and at a raised profile
    (2 CPU / 2 GiB, QPS 100).
  - One clustered Provider backed by a fake provider process that implements
    `GetHostInfo`, `target_host_id` routing and owner-idempotent `Create` /
    `Describe` / `Delete` with configurable latency (for example 50 ms p50,
    500 ms p99) and no hypervisor.
  - The fake can live in the test tree. It must not become a supported
    provider, and no new CRD is needed.
- **Scales:**
  - 100 Hosts / 10,000 VMs and 1,000 Hosts / 100,000 VMs. Pre-load the VMs
    with status, as the envtest fidelity check does, so the steady state
    exists before measuring.
  - Repeat with the same pool as 1 and as 4 Providers.
- **Workloads:**
  1. **Steady state:** no creates; measure the reconcile, status-write and
     Describe-RPC rates the VMs alone generate at the default poll intervals.
  2. **Create burst:** 1,000 VMs created at once, then 5,000.
  3. **Sustained churn:** creates and deletes at a fixed rate, stepped up
     until time-to-`pendingHost` grows without bound.
  4. **Host inventory:** 1,000 Hosts on the default 60 s heartbeat with the
     single-worker Host controller.
- **Instrumentation needed** (in the experiment build or as metrics; nothing
  here exists today):
  - Placement-lock wait and hold histograms, and `placementLockBusy` requeues.
  - Time from VM creation to `pendingHost`, to `status.id`, and to `Ready`.
  - Client-side rate-limiter wait (client-go's `rest_client_rate_limiter_duration`).
  - API requests per create by verb and resource; apiserver and etcd request
    rates.
  - Manager CPU, CPU throttling (`container_cpu_cfs_throttled_seconds_total`),
    RSS, GC pause and GC CPU fraction.
  - Work-queue depth and latency per controller (controller-runtime's
    workqueue metrics).
  - Provider RPC latency by method. The current histogram stops at about 32 s.
  - Host status writes per second; Host heartbeat age.
- **Decision it informs:** whether the next engineering step is
  write-discipline and quota work (if QPS or CPU saturates first), the
  incremental committed index (if lock hold dominates create latency), or
  something else entirely (if provider RPCs dominate). Only if the lock still
  dominates after the index would shared-state scheduling be worth a spike of
  its own.

## Reproducing

```bash
hack/bench/spike1a.sh                     # full run, ~1.5 h on this machine
QUICK=true hack/bench/spike1a.sh          # without the 100,000-VM scenarios
ENVTEST=true hack/bench/spike1a.sh        # also the real-informer-cache comparison
```

Individual commands (all from the repository root):

```bash
go test ./internal/scheduler/ -run '^$' -bench 'BenchmarkSchedule(Phases)?$' -benchmem -count 5
go test ./internal/controller/ -run '^$' -bench 'BenchmarkPlacement(Scan|Decision|Breakdown)$' -benchmem -count 5
go test ./internal/controller/ -run '^$' -bench 'BenchmarkPlacementDecision$' -cpu 1 -benchmem -count 5
go test ./internal/controller/ -run '^$' -bench 'BenchmarkPlacementContention$' -benchtime 1x
go test ./internal/controller/ -run 'TestSchedBench' -race -v
SCHEDBENCH_SCENARIOS=h1000_v100k/base go test ./internal/controller/ -run '^$' \
  -bench 'BenchmarkPlacementDecision$' -benchtime 20x -cpuprofile decision.cpu.pb.gz -memprofile decision.mem.pb.gz
```

Knobs:

| Variable | Effect |
|---|---|
| `SCHEDBENCH_SCENARIOS` | Comma-separated scenario-name substrings |
| `SCHEDBENCH_SKIP_LARGE=true` | Skips 100,000-VM scenarios |
| `SCHEDBENCH_DECISIONS` | Decisions per contention run |
| `SCHEDBENCH_ENVTEST_VMS` | Size of the envtest fidelity fixture |

**Files added by this spike** (untracked at the measured revision):

```
hack/bench/spike1a.sh
internal/scheduler/schedbench/{fixture.go,scenarios.go}
internal/scheduler/{schedule_bench_test.go,export_bench_test.go}
internal/controller/placement_bench_{test,seam_test,correctness_test,envtest_test}.go
docs/performance/spike-1a-scheduler-baseline.md
docs/performance/spike-1a/results/   (raw output, benchstat summaries, profiles)
```

`results/env.txt` was written by an earlier version of the script whose
"dirty" count included these files. The tree measured was `ed74c8fe` plus
exactly the files above, and the unrelated untracked `.wolf/` and `hack/*`
spikes that no package imports.
