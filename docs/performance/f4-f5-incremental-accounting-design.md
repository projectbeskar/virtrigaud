# F4/F5: incremental VM accounting — design proposal

| | |
|---|---|
| **Status** | Proposal for review. Nothing implemented. |
| **Date** | 2026-10-26 |
| **Author** | William Rizzo ([@wrkode](https://github.com/wrkode)) |
| **Branch** | `feature/cloud-hypervisor-scalability` (on top of F1/F2/F3) |
| **Evidence** | [Spike 1A](spike-1a-scheduler-baseline.md), [Spike 1B](spike-1b-control-plane-baseline.md) §4.6, §5 (F4, F5) |

## 0. Summary

**F4 and F5 do not need a new subsystem.** They need a watch predicate, one
field index and a count read from that index.

The CPU at 100,000 VMs is not only scan cost, it is scan cost **times
trigger rate**:

- **F4:** every Host heartbeat status write re-enqueues its Provider (the
  Host watch has no predicate). The Provider reconcile then deep-copies
  **every VirtualMachine in the cluster** to count one Provider's VMs.
- **F5:** every Host heartbeat scans every VM of its Provider to publish two
  gauges.

Fixing the triggers and narrowing the reads with the informer cache's own
indexes removes both. There is no new state to keep consistent.

**The scheduler is the only consumer that needs aggregated, incrementally
maintained state.** For it the proposal is a single informer-driven **placement
ledger** per manager:

- it is a projection of the cache, never consulted for anything but
  placement arithmetic;
- it is shadow-compared against today's scan before it becomes
  authoritative;
- it falls back to the scan whenever it is not provably in sync.

Recommended order:

1. Host-watch predicate + F4 count.
2. Per-host field index for F5 and the Host in-use check.
3. Placement ledger in shadow mode.
4. Ledger authoritative.

---

## 1. What the three consumers actually compute

| | F4 `countConnectedVMs` | F5 `publishCommitted` / `committedOnHost` (+ `vmsUsingHost`) | Scheduler `committedPlacementsFrom` + assume cache |
|---|---|---|---|
| Location | `provider_controller.go:1352` | `host_controller.go:193`, `:278`; `virtualmachine_placement_capacity.go:548` | `virtualmachine_placement_capacity.go:433`, `:615` |
| Key | `spec.providerRef` (`vmProviderKey`), **not** the placement Provider | placement Provider (`placementProviderKey`) + one host | placement Provider, all its hosts |
| Selection | every VM, any state | `holdsPlacement` and host ∈ {`placement.host`, `pendingHost`} | same, plus the VM's own record (`recorded`) for settling assumptions |
| Per-VM data | none (count) | `admittedFootprint` (recorded size, else `pendingResources`, else VMClass with the consumer grant; memory ceiling) | footprint + namespace + labels (affinity) + UID |
| Output | `Provider.status.connectedVMs` (display) | two gauges; the Host finalizer's in-use list | `[]PlacedVM` → `Schedule` |
| Read today | cluster-wide `r.List`, **deep copies** | Provider-wide index List (`SetList` copy) per heartbeat | Provider-wide index List per decision |
| Trigger rate | every Provider event, **including every Host status write** (`provider_controller.go:1456-1458`) | every Host sync (one per Host per 60 s) | every clustered create / resize / clone decision |
| Measured (100k VMs, Spike 1B) | 36% CPU, 55% allocation | 35% CPU, 41% allocation | ~240 ms per decision (Spike 1A) |

**Common data:** "which VMs hold a placement on which host of which Provider,
and at what size".

**Differences:**
- F4 counts by `spec.providerRef`, a different key on purpose (display of
  references, including unbound VMs).
- F5 needs one host at a time.
- Only the scheduler needs all hosts at once, labels, and per-UID records for
  settling assumptions.

## 2. Alternatives

| | A. Fix triggers + cache indexes | B. Shared informer-driven ledger with projections | C. Per-controller incremental indexes |
|---|---|---|---|
| F4 | Predicate + index count: O(V_provider) refs, no deep copy, rare | Count projection: O(1) | Same as B, owned by the Provider controller |
| F5 | Per-host field index: O(V_host) per heartbeat | Host projection: O(1) | Same, owned by the Host controller |
| Scheduler | Unchanged O(V_provider) per decision | O(hosts) per decision | Scheduler keeps its own |
| New state | None (the indexes are the cache's) | One ledger (~20 MB at 100k, estimate) | Two or three overlapping copies |
| Consistency risk | None new: reads are the cache | Projection vs cache lag (handled, §4) | The same risk, multiplied |
| Complexity | Small | Medium, one place | Highest, scattered |

**Recommendation: A for F4 and F5, plus B restricted to the scheduler.**
- A removes the measured F4/F5 cost with no state of its own.
- C duplicates B's hardest part (lag, VMClass dependence, restart) per
  controller, so it is rejected.
- B is justified only where O(V) per decision remains after A, which is the
  scheduler at large V_provider.
- The ledger can later feed the Host gauges too. That is optional and not
  needed for the measured win.

## 3. Design

### 3.1 Increment 1: F4 trigger and count (A)

- **Host watch predicate.** Enqueue the Provider only on Host create, delete,
  a generation change (spec: endpoint, credential reference, labels, pool
  reference, provider reference) or a deletion-timestamp change. Host status
  writes no longer re-run the Provider reconcile.
  - The inventory render reads Host spec only (`provider_hostinventory.go`,
    no Host `.Status` read).
  - The Provider's periodic requeue (30 s – 1 min) keeps
    `status.connectedVMs` fresh.
- **Count from an index.**
  - Register a `virtrigaud.io/spec-provider` field index on VirtualMachine
    (value: `vmProviderKey`), next to the existing `placementProviderIndex`.
  - `countConnectedVMs` lists through it with `UnsafeDisableDeepCopy`: the
    same semantics, O(V_provider) shallow copies, no deep copy and no
    cluster-wide list.
  - Optional: read `len(indexer.ByIndex(...))` from the shared informer to
    avoid even the `SetList` copy. That is a type assertion on the cache
    informer; it can wait until the simple version is measured.

### 3.2 Increment 2: F5 per-host index (A)

- **Field index** `virtrigaud.io/placement-host` on VirtualMachine, with
  values `"<placement provider>|<host>"` for each of `placementHosts(vm)`.
  That is empty when `!holdsPlacement`, so a VM being deleted whose finalizer
  is gone drops out exactly as today.
- **`committedOnHost` and `vmsUsingHost`** list through it and run the same
  per-VM code (`placementProviderKey` re-check, `sizeForAccounting` with its
  VMClass memo). The cost becomes O(VMs on the host).
- **Semantics are unchanged by construction.** It is the same functions over
  a narrower, cache-consistent list.
- **Memory:** one or two index keys per VM (estimate: tens of bytes each).

### 3.3 Increments 3–4: the placement ledger (B, scheduler only)

**Data structures** (`internal/controller/placement_ledger.go`, one per
manager, owned by the VirtualMachine controller like the assume cache):

```
ledger
  mu        sync.RWMutex
  synced    bool                         // initial list replayed (registration HasSynced)
  byUID     map[UID]*entry
  byProv    map[providerKey]*provLedger
  classDeps map[classKey]set[UID]        // entries whose size depends on a VMClass

entry   { uid, ns, name, provider providerKey, hosts []string,
          size ResourceRequest, sized bool,   // false: VMClass-dependent, resolve at read
          labels map[string]string,           // the cached object's map (immutable per version)
          rv string }
provLedger { committed map[host]committed; perHost map[host]set[UID] }
```

**Lifecycle:**
- **Initialisation:** an event handler on the VirtualMachine informer the
  manager already runs (`mgr.GetCache().GetInformer`). Adds replay the cache.
  `synced` becomes true only when the handler registration's `HasSynced()`
  reports true. That is client-go's guarantee that this handler has seen the
  initial list, not just that the store has.
- **Add/Update:** compute the new contribution from the event's object with
  the *existing* helpers (`placementProviderKey`, `holdsPlacement`,
  `placementHosts`, `admittedFootprint`). Subtract the old contribution
  (looked up by UID), add the new one, and record `rv`.
  - A VM whose footprint needs its VMClass gets `sized=false`. It is counted
    at read time through the same `sizeForAccounting` path (memoised), and
    indexed in `classDeps`.
  - Such VMs are those with no recorded `currentResources` and no
    `pendingResources`, or with no recorded memory ceiling under hot-add:
    legacy and transitional VMs only.
  - VMClass and Namespace events therefore need no handler: unsized entries
    are always evaluated live.
- **Delete** (including `DeletedFinalStateUnknown` tombstones after a watch
  gap): remove by UID.
- **Restart:** nothing persists. The ledger is rebuilt from the informer's
  initial list, exactly as the cache is. Kubernetes stays the only source of
  truth.

**Read path (scheduler)** keeps the existing protocol. Under the Provider's
assume lock it:
1. takes `ledger.mu.RLock`;
2. builds `PlacedVM`s from `provLedger`:
   - per-host committed sums for capacity;
   - per-host entries, own-namespace only, for affinity, read only when the
     policy has VM (anti-)affinity;
   - unsized entries resolved through the cache;
3. settles assumptions **against the ledger's own `byUID`**, never the cache.
   The current code already requires that settling uses "the same snapshot
   the committed placements were read from" (`assume.go` `List`). With two
   sources, mixing the cache's view with the ledger's could drop an assumption
   before the ledger counts its record, which would undercount capacity.
4. calls `Schedule` unchanged.

To make `Schedule` O(hosts), `Request` gains an optional precomputed
`CommittedByHost` used instead of summing `PlacedVMs` for capacity. `PlacedVMs`
then carries only affinity-relevant entries and assumptions. `indexPlaced`
keeps its (UID, host) merge-at-larger-size rule for assumptions vs records, by
adding each assumption only when its UID is absent from that host's ledger set
or its size is larger.

## 4. Consistency and failure handling

| Situation | Behaviour |
|---|---|
| Ledger not synced (start-up, after a re-list) | Scheduler uses today's scan (authoritative and correct). Not "fail closed" but a **demonstrably safe fallback**: it is the current implementation. |
| Ledger lags the cache (handlers run after the store update) | Harmless: the ledger is internally consistent and settles assumptions against itself. An assumption whose record the ledger has not seen stays counted. A lag makes it count a VM that is gone a little longer (overcount, safe), never miss one. |
| Divergence detected (shadow mode, or a periodic self-check: recompute one Provider by scan when idle) | Metric + log; the ledger is marked unsynced for that Provider and the scan is used until a rebuild (re-list from cache under the lock). |
| Stale or out-of-order events | Events are applied per UID with the object's `resourceVersion` stored. An informer never delivers an older version after a newer one for the same key; a delete removes by UID whatever version it holds. |
| VM UID reuse (delete + re-create with the same name) | Keyed by UID, so the new UID is a new entry; the old one leaves on its delete event. |
| Provider re-created (new UID, same name) | Unchanged from today: accounting keys on `placementProviderKey` (namespace/name); the bound-provider checks handle a re-created Provider before scheduling. |
| Pending vs committed, two hosts during transitions | Same `placementHosts`: host and `pendingHost` both count, de-duplicated once. |
| Resize, `pendingResources`, memory ceiling | Same `admittedFootprint`; admitted resizes stay assumptions until `currentResources` records them (unchanged rule). |
| VMClass-dependent sizing, consumer grants | Never cached: evaluated live at read time with the existing memo. |
| Failed or ambiguous creates | Unchanged: `pendingHost` is written before Create and stays until resolved, so the ledger counts it like the scan does. |
| Forced deletion, orphan-on-delete | Unchanged: such a VM leaves the accounting when its VirtualMachine finalizer goes, as today (`holdsPlacement`). The ADR-0007 A5 warning about leftover domains still applies. |
| Concurrent reconciles | Ledger writes come from informer handlers; reads under `RLock` inside the per-Provider assume lock. The assume lock still serialises decisions (single leader, ADR-0007 A5). |
| Leader change | A new leader rebuilds the ledger from its initial list and uses the scan until synced. |

## 5. ADR compatibility

- **ADR-0007 D1** (etcd is the source of truth; the operator owns decisions):
  kept. The ledger is a derived cache, rebuilt from the informer and never
  written anywhere.
- **D4** (`Schedule` is a pure function): kept. The ledger only prepares
  inputs; the optional `CommittedByHost` is an input field, not I/O.
- **A2** (`pendingHost` before Create, never re-schedule on unknown): unchanged.
- **A5 scheduler-accuracy amendment** (committed = bound + pending + assumed;
  admitted size; in-process assume cache correct under a single leader): kept
  bit-for-bit through the same helpers. This proposal adds one rule: when the
  ledger is the source, assumptions settle against the ledger. A5 states the
  equivalent rule for the cache.
- **ADR-0008 D5 precedent:** shadow-compare a new read path against the
  authoritative one, with a divergence metric and a soak gate before flipping.
- **No ADR change is needed** for increments 1–2. Increment 4 (ledger
  authoritative) is worth a short A5 note recording the settle-against-ledger
  rule. The maintainer decides.

## 6. Increments

| # | Change | Files | Equivalence test |
|---|---|---|---|
| 1 | Host-watch predicate on the Provider controller; `spec-provider` index; `countConnectedVMs` through it | `provider_controller.go`, `placement_index.go` (registration), tests | Count equals the old full-list count over a fixture (bound, unbound, cross-namespace, other Providers); a Host status-only update enqueues nothing, a spec change and a delete do |
| 2 | `placement-host` index; `committedOnHost` and `vmsUsingHost` through it | `placement_index.go`, `virtualmachine_placement_capacity.go`, `host_controller.go`, tests | For random fixtures (pending, bound, both, deleting with and without finalizer, unsized VMs, foreign Provider with same host name), per-host results equal the Provider-wide scan |
| 3 | Placement ledger + **shadow mode**: authoritative path unchanged; on each decision (or a sample) compare ledger-derived inputs with the scan; metric `virtrigaud_placement_ledger_divergence_total` | new `placement_ledger.go`, `virtualmachine_placement_capacity.go` (shadow hook), `cmd/manager` flag (default shadow off or sampled) | Property test: random event sequences (create, update, resize, pending→bound, delete, tombstone, UID reuse) applied to ledger and cache give identical per-host committed sums and settled sets; plus the existing contention correctness tests run against the ledger |
| 4 | Ledger authoritative (scan as fallback when unsynced or divergent); optional `Request.CommittedByHost` | `virtualmachine_placement_capacity.go`, `internal/scheduler` (input field), tests | Existing scheduler and placement tests unchanged; Spike 1A harness reports decision time vs V |

Each increment is independently revertible. Increments 1–2 are useful on
their own and carry no state.

## 7. Minimal correctness tests

- **Equivalence:** old function vs new function on the same cache contents,
  over generated fixtures covering every row of §4 (increments 1, 2, 4).
- **Triggers:** the Host-watch predicate's enqueue/no-enqueue table.
- **Ledger property test:** random event streams, including out-of-order
  delivery across UIDs, tombstones and re-lists, compared against a fresh scan
  after each step.
- **Assumption interplay:** an assumption is never dropped before the ledger
  counts its record (ledger lag injected); the existing
  `TestSchedBenchContentionNeverOverbooks` passes with the ledger as source.
- **Fallback:** an unsynced ledger, or one marked divergent, schedules through
  the scan (decision identical to the old path).

## 8. Expected benefit and uncertainty

| | Today (100k VMs, 1,000 hosts, Spike 1A/1B) | After (estimate) |
|---|---|---|
| F4 | Cluster-wide deep-copied list per Provider reconcile, triggered by every heartbeat: 36% CPU | One indexed count per Provider reconcile at its own cadence: ≈0 |
| F5 | Provider-wide list per heartbeat: 35% CPU | ~100 VMs per heartbeat (V/H): ≈0 |
| Scheduler decision | ~236 ms, O(V) | O(hosts) + unsized VMs: low milliseconds |
| Memory | Allocation churn ~545 MB/s | Ledger ~20 MB at 100k (estimate); allocation churn largely gone |

**Uncertain until measured:**
- the ledger's real memory per VM;
- event-handler lag under status-write bursts (lower after F2);
- whether the remaining per-reconcile costs (Describe RPC, status
  comparison) become the next limit at 100k;
- the share of VMs that stay "unsized" in real clusters (expected small:
  legacy and transitional VMs only).

Re-running Spike 1B stages C and D after increments 2 and 4 answers these.
