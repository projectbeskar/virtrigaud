# C′: allocation-efficient scheduler snapshots

> **Status:** implemented on `perf/placement-snapshot-accounting` (manager
> only; no API, CRD or gRPC change, no new state). Measured with the Spike 1A
> harness ([`spike-1a-scheduler-baseline.md`](spike-1a-scheduler-baseline.md)).

## What every placement decision did

Under the Provider's assume lock, each decision — a create, a resize or clone
admission, an inherited-admission revalidation (C1) — read the Provider's
committed placements:

1. `listProviderVMs`: a `List` through the cache with `MatchingFields` and
   `UnsafeDisableDeepCopy`. The objects were not deep-copied, but
   controller-runtime's cache reader then builds the typed list with
   `apimeta.SetList`, which copies every 728-byte `VirtualMachine` **struct**
   into a new `Items` array, on every decision.
2. `committedPlacementsFrom`: for every VM a `placementHosts` slice, a
   `recorded` map entry (used only to settle assumptions) and one
   `scheduler.PlacedVM` per (VM, host).
3. `scheduler.Schedule` → `indexPlaced`: a second V-sized slice and a V-sized
   `(UID, host)` map to merge duplicates, then the per-host map of PlacedVMs and
   the per-host committed sums.

Spike 1A measured the scan at 77–83 % of a decision, `SetList` at 42 % of the
bytes allocated, and garbage collection at about half of the CPU.

## What it does now

One walk of the same informer index, at the same instant:

- **Direct index read** (`indexedReader`, `placement_index.go`): the
  VirtualMachine informer's indexer is read with the same `ByIndex` lookup the
  cache reader makes for that `List`, without the list. The result is a slice
  of pointers to the cache's own objects; they are only read, and nothing of
  them but strings and label maps reaches the result, which lives only for the
  decision (as before).
- **Per-host sums**: a VM that nothing needs individually is summed into
  `scheduler.Request.Committed` (CPU, memory, number of VMs, number in the
  affinity scope) by host. No per-VM record, slice or map entry.
- **Listed only when needed**: a VM is still listed as a `PlacedVM` when an
  assumption of its UID exists (so the scheduler merges the assumption with the
  record at the larger size, as before), or when it is in the affinity scope
  and the policy has VM affinity or anti-affinity terms
  (`scheduler.NeedsPlacedVMLabels`: label matching needs its labels).
- **Settlement from the same snapshot**: `recorded` is built only for the UIDs
  that have assumptions (`assume.Cache.UIDs`, read under the Provider's lock,
  where assumptions are made, so it can only shrink meanwhile) and for the
  scheduled VM. An assumption of any other UID is reported `Unknown` to
  `Observe` — kept and counted, nothing recorded — which cannot happen under
  the lock but never settles anything on incomplete information.
- **No allocation for hosts**: `placementHostPair` returns the one or two hosts
  in a fixed array.

The scheduler adds `Request.Committed` to what `PlacedVMs` commits and to the
bound-VM counts, with the same saturating sums; it refuses summed VMs in the
affinity scope when the policy matches labels (a caller error, never a
decision). Scheduling UIDs are Kubernetes UIDs, unique per object, and a VM
names each host once, so summing needs no de-duplication.

Everything else is unchanged: `admittedFootprint` (reservations, ceilings,
VMClass sizing, pending sizes), consumer-grant class reads, cross-namespace
accounting, G1/G2/G4 tokens and settlement, G3 reservations, C1 revalidation,
the lock, and the Host gauges.

## Dependency on controller-runtime internals

The direct read uses two names controller-runtime keeps in an internal package
(`pkg/cache/internal`): a field index is registered in the informer's indexer
as `field:<name>`, and its cluster-wide key is `__all_namespaces/<value>`.
The manager obtains the indexer with `cache.GetInformer` and a type assertion to
client-go's `SharedIndexInformer`.

- If the informer is unavailable or the assertion fails (another cache
  implementation), the reconcilers use the `List` path and log it once.
- A missing index is an error, never an empty result. A direct read that finds
  no VM is confirmed through the `List` (an index whose key naming changed
  would answer "none" for every Provider), so an empty Provider is never taken
  as holding nothing on the direct read alone; a failed read fails the
  decision. `placement_index_fallback_test.go` pins all three cases.
- `TestPlacementSnapshot_BytesStayBelowAVMCopy` fails if the snapshot regresses
  to the `List` path (which copies at least 728 bytes per VM); the benchmark
  seam reads the index directly, as production does.
- The envtest spec *Placement index read directly from the informer* checks,
  against a real manager cache, that the direct read returns exactly what the
  `List` returns. A controller-runtime upgrade that renames either key fails it.
- Safety rests on client-go's informer contract: objects in the store are
  replaced, never mutated, so a pointer read from `ByIndex` stays a consistent
  object; and `ByIndex` returns the index's members at one instant under the
  store's lock, the same snapshot a `List` would take.

## Equivalence

The previous accounting is kept verbatim as a test oracle
(`placement_reference_test.go`). `placement_snapshot_equivalence_test.go` runs
both on identical fixtures — every schedbench variant (spread, binpack,
heterogeneous, high and low utilisation, small and large requests,
unschedulable, hard constraints, VM affinity), with pending, dual-host,
reserved, ceiling-raised, being-deleted and class-sized VMs mixed in — and on
identical assumption caches (creates that settle and do not, resizes covered
and not, VMs the snapshot does not have, every write state):

- per-host committed CPU and memory and affinity counts: equal;
- `Schedule`: the same host, trace and no-fit error;
- `CheckResize` (resize, clone and C1 revalidation) from placed VMs' own points
  of view: the same verdicts and arithmetic;
- the same assumptions settled.

Seeded mutations (dropping the affinity counts of summed VMs, never settling,
counting assumed VMs twice, summing VMs a VM-affinity policy must see) each
fail these tests. The existing contention, isolation and no-overbooking tests,
the G1/G2/G3/C1 tests and the clustered envtest specs pass unchanged.

## Measured effect

Spike 1A harness (`BenchmarkPlacement{Scan,Decision,Breakdown}`, `-count 5`,
medians; `BenchmarkPlacementContention`, 1,500 decisions, 3 runs), the same
machine (i9-10900X, 20 threads, Go default GOGC), the baseline binary built
from `e2a84c61` and the change built from this branch. Medians per benchmark:
[`c-prime/results/summary.txt`](c-prime/results/summary.txt).

**One placement decision** (`scheduleUnderLock`: lock, snapshot, settle,
`Schedule`, `Assume` — the time the Provider's lock is held):

| Shape | Time before → after | Bytes/decision | Allocs/decision |
|---|---|---|---|
| 100 hosts, 1,000 VMs | 1.55 ms → 0.35 ms (−78 %) | 1.65 MB → 83 KB (−95 %) | 1,264 → 211 (−83 %) |
| 100 hosts, 10,000 VMs | 18.4 ms → 7.9 ms (−57 %) | 16.9 MB → 435 KB (−97 %) | 10,891 → 522 (−95 %) |
| 1,000 hosts, 10,000 VMs | 19.9 ms → 8.1 ms (−59 %) | 17.0 MB → 946 KB (−94 %) | 11,828 → 1,467 (−88 %) |
| 1,000 hosts, 100,000 VMs | 230 ms → 107 ms (−53 %) | 166 MB → 3.2 MB (−98 %) | 104,814 → 1,664 (−98 %) |

Every variant at 100 hosts / 10,000 VMs (binpack, heterogeneous, high and low
utilisation, small and large requests, unschedulable, hard constraints) moves
the same way: 5.3–6.2 ms, 431–448 KB and 442–1,016 allocations per decision,
from 18.9–19.6 ms, ~16.9 MB and ~10,900. The **VM-affinity** variant gains
less, as designed — 33.1 ms → 26.4 ms (−20 %), 22.6 MB → 12.7 MB (−44 %): the
VMs in the scheduled VM's namespace are still listed for label matching, and
`Schedule`'s affinity evaluation (17.9 ms, unchanged) dominates.

**Where the time goes** (`BenchmarkPlacementBreakdown`, base shapes):

| Shape | Snapshot (read + account + settle) | `Schedule` |
|---|---|---|
| 100 hosts, 1,000 VMs | 1.26 ms → 0.28 ms | 0.36 ms → 0.05 ms |
| 100 hosts, 10,000 VMs | 16.1 ms → 5.5 ms | 2.81 ms → 0.05 ms |
| 1,000 hosts, 10,000 VMs | 16.1 ms → 6.7 ms | 3.08 ms → 0.42 ms |
| 1,000 hosts, 100,000 VMs | 195 ms → 100 ms | 54.6 ms → 0.51 ms |

`Schedule` no longer merges V entries (`indexPlaced` sees only the few listed
ones); its cost is now the per-host filter and score. What remains is the walk
itself: visiting every VM and computing its footprint.

**Lock hold under contention** (100 hosts / 10,000 VMs, one Provider, no
write delay; `cs` is the critical section, i.e. the lock hold):

| Workers | Lock hold p50 | Lock hold p99 | Decisions/s |
|---|---|---|---|
| 1 | 20.4 ms → 7.7 ms (−62 %) | 31.2 ms → 13.8 ms (−56 %) | 48 → 126 |
| 8 | 20.5 ms → 7.6 ms (−63 %) | 31.0 ms → 13.8 ms (−55 %) | 48 → 130 |

One Provider's decisions are still serialized by its lock (by design), so
throughput is the inverse of the lock hold. GC overhead was not profiled
separately; it follows the bytes allocated per decision, which fell by
94–98 % on every base shape.

## Limits

- Still O(V) per decision: every VM of the Provider is visited under the lock.
  What is gone is the per-VM copying and per-VM allocation, not the walk.
- With a VM affinity or anti-affinity policy, the VMs in the scheduled VM's
  namespace are still listed individually (the scheduler matches their
  labels); other namespaces' VMs are summed.
- Clustered Providers only: single-host Providers never schedule.
