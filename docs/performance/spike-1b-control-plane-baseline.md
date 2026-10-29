# Spike 1B: end-to-end control-plane scalability baseline

| | |
|---|---|
| **Status** | Experiment report. No production code changed; no architecture decided. |
| **Date** | 2026-10-25 / 26 |
| **Author** | William Rizzo ([@wrkode](https://github.com/wrkode)) |
| **Revision measured** | `ed74c8fee01c1b88168b3d96597ed873949196b6` (main), manager built with the experiment-only patch [`hack/bench/spike1b/manager-experiment.patch`](../../hack/bench/spike1b/manager-experiment.patch) |
| **Preceded by** | [Spike 1A](spike-1a-scheduler-baseline.md) (scheduler microbenchmarks) |
| **Raw results** | [`spike-1b/results/`](spike-1b/results/) |

Spike 1A measured the scheduler in isolation. This spike runs the **real
VirtRigaud manager** against a **real Kubernetes API server**, with a
test-only clustered provider standing in for host agents. It answers which
limit a manager hits first on the full VM lifecycle, and why.

Throughout, **observed** means measured here, **estimate** means arithmetic
from observed numbers, and **hypothesis** means not measured.

---

## 0. Summary

### Measured limiting factors, in the order they bite

1. **Client QPS, even at 1,000 VMs.** It is driven by a steady-state polling
   defect.
   - Every running VM is reconciled every **10 s**, not the intended 2 min.
     `getRequeueInterval` (`internal/controller/virtualmachine_controller.go:3175`)
     compares the provider's power state with vSphere's raw strings
     `"poweredOn"`/`"poweredOff"`, but every provider returns the canonical
     `"On"`/`"Off"` (`internal/providers/contracts/types.go:358`;
     `internal/providers/vsphere/server.go:1519`). The unit tests feed the raw
     strings, so they pass.
   - Each reconcile costs **one status PUT, one Describe RPC and one Validate
     RPC**. That is 100 requests/s per 1,000 VMs.
   - With the default budget of 20 QPS / 40 burst, the manager is saturated
     at 1,000 VMs:
     - VM queue depth 790;
     - client rate-limiter wait p99 0.81 s;
     - a 1,000-VM burst reaches Ready at p50 **365 s** (2.4 VMs/s).
   - Raising QPS to 100 **without** more CPU gives 3.4× the create throughput
     (Ready p50 66 s, 8.2 VMs/s). Raising CPU without QPS changes nothing
     measurable.
2. **At 10,000 VMs the budget is gone even at QPS 100.** Polling demand is
   about 1,000 reconciles/s. The queue stays near 9,000, and each VM is
   effectively polled every ~90 s.
   - At QPS 20, new VMs **never get placed**: 0 of 100 reached
     `pendingHost` within 15 minutes.
   - At QPS 100, a 1,000-VM burst reaches Ready at p50 351 s (2 CPU) or 547 s
     (500m CPU, 56% of CFS periods throttled).
3. **At 100,000 VMs, memory and CPU fail before QPS.**
   - The manager is **OOM-killed at 512 MiB** about 20 s after every start,
     and **at 2 GiB** during a burst and then on every restart.
   - With a 6 GiB diagnostic limit it peaks at a **2.1 GiB working set,
     1.8 GiB heap, 545 MB/s allocation, 1.77 of 2 cores**.
   - About **70% of its CPU** goes to two full scans of every VirtualMachine:
     - `ProviderReconciler.countConnectedVMs`
       (`internal/controller/provider_controller.go:1352`) lists *every*
       VirtualMachine in the cluster with deep copies on every Provider
       reconcile, just to count them.
     - `HostReconciler.publishCommitted` → `committedOnHost` repeats Spike
       1A's O(V) scan on every host heartbeat.
   - Starved of CPU, the Host controller falls to 2.9 syncs/s against the
     16.7/s that 1,000 hosts need. **673 of 1,000 Hosts never got a heartbeat**
     after a restart, so they were not Ready. As a result, **a 1,000-VM burst
     placed nothing**.
4. **Provider RPC latency limits throughput through worker occupancy.**
   `Create` runs synchronously inside a reconcile. Moving the fake's Create
   from 200 ms to 2 s (everything else fixed) slowed a 500-VM burst's Ready
   p50 from **40 s to 126 s**. Steady-state polling dropped from 93 to 26
   Describes/s while workers waited.
5. **The scheduler is not the bottleneck anywhere measured.**
   - Placement-lock hold is 3–15 ms at 1,000 VMs and 32–64 ms at 10,000
     (2 CPU); 260 ms when CPU-throttled at 500m.
   - At 10,000 VMs, the lock is held for ~9% of a burst's wall time and is a
     **~0.01% share** of a VM's 351 s time-to-Ready.
   - No lock acquisition timed out in any run.

### Partitioning, correctness and architecture

**Partitioning into Providers made things worse at this layer.** We took the
same 1,000 hosts and 10,000 VMs and split them into four Providers instead of
one:
- Ready p50 **549 s vs 359 s**;
- manager CPU 1.80 vs 1.48 cores;
- worst host heartbeat 97 s vs 61 s.

Per-Provider work that scans the whole cluster (`countConnectedVMs`) grows
with the number of Providers. Splitting into Providers only pays off once
that O(V) per-Provider and per-Host work is gone.

**Correctness:** 25 invariant audits found **zero violations**. The audits
compare VM spec, placement status and the fake agents' execution state. They
ran after steady state, every burst, churn up to 20 creates/s, 20% pre-execution
Create failures, 20% lost Create replies, deletes during in-flight Creates,
and unreachable hosts during a burst. Two classes of apparent violations were
traced to the test fixture and the fake, not to VirtRigaud (section 6.3).

**The next engineering increment is steady-state cost, not scheduling
architecture.** Fixing the poll interval, skipping unchanged status writes,
not calling `Validate` on every reconcile, and removing the per-event full
scans are each small changes. Together they remove the first three limits
above. Shared-state scheduling workers address none of the observed limits.

---

## 1. What this spike is, and what it is not

| Real | Simulated | Absent |
|---|---|---|
| The VirtRigaud manager, built from `ed74c8fe` (+ instrumentation, below), deployed by the chart | Host agents: the [fake provider](../../test/spike1b/fakeprovider/) keeps a record per VM per simulated host, enforces owner identity and per-host execution concurrency, and sleeps a configurable latency | Hypervisors, VM processes, disks, networks, images, guest agents |
| The provider gRPC contract, served by the real provider SDK server (TLS/auth resolution, health, middleware) | Host inventory: `GetHostInfo` answers configured capacity; health is Ready unless a host is configured unreachable | Host failure modes beyond "refuses calls" (partial failure, reboots, lost agent state) |
| Kubernetes API server and etcd (kind, v1.34.0), the CRDs, informers, the chart's RBAC and admission policy | Network partitions: an unreachable host answers HOST_UNAVAILABLE instantly | mTLS between manager and provider (TLS disabled on the Provider; ADR-0003 overhead not measured) |
| Manager reconciliation, scheduling, lock, assume cache, status writes, finalizers, Host/Provider controllers | — | Leader election failover, multiple manager replicas, a real host agent |

**A successful status write is never treated as proof of execution.** The
auditor (section 6) compares the API's view with the fake agents' records, and
only those records count as "executing".

## 2. Environment and configurations

| | |
|---|---|
| Machine | Intel Core i9-10900X (10 cores / 20 threads), 125 GiB, Ubuntu 24.04.2, kernel 6.8.0-55. Shared with other idle containers (another kind cluster, a registry). Not dedicated. |
| Cluster | kind v0.30.0, `kindest/node:v1.34.0`, 1 control-plane node (API server + etcd) and 1 worker (manager + fake providers). [`kind-config.yaml`](../../hack/bench/spike1b/kind-config.yaml) |
| API server / etcd limits | **None.** API server requests 250m CPU; etcd 100m / 100 MiB; no limits on either. etcd `quota-backend-bytes` raised to 8 GiB so object count is not capped by the 2 GiB default. API Priority and Fairness defaults. |
| Tools | Go 1.26.10, helm v3.17.2, kubectl v1.32.3 |
| Manager | Chart `charts/virtrigaud` at the revision, one replica, `--leader-elect`, `MaxConcurrentReconciles` unchanged (VM 10, Provider 5, Host 1) |
| Fake provider | One pod per Provider, limits 4 CPU / 4 GiB; default latency: Create 200±100 ms, Delete 100±50 ms, Describe 5±3 ms, GetHostInfo 20±10 ms, Validate 1 ms; 8 concurrent executions per host |

**Manager configurations** ([`values-A..D.yaml`](../../hack/bench/spike1b/)):

| Config | CPU limit | Memory limit | Client QPS / burst |
|---|---|---|---|
| A (baseline = chart defaults) | 500m | 512 MiB | 20 / 40 |
| B (API budget only) | 500m | 512 MiB | 100 / 200 |
| C (resources only) | 2 | 2 GiB | 20 / 40 |
| D (both) | 2 | 2 GiB | 100 / 200 |

QPS and burst are hard-coded in `cmd/manager/main.go:335-336`. The
experiment patch adds an environment override that defaults to the
production values. **No production default changed.**

**Experiment-only instrumentation** (the patch, applied to an exported copy
of the revision at image build time; the working tree is untouched):

- `spike_manager_api_requests_total{verb,resource,subresource,code}` and
  request latency: a transport wrapper on the manager's REST config.
- `spike_manager_client_rate_limiter_wait_seconds`: client-go's rate-limiter
  latency hook.
- `spike_placement_lock_wait_seconds{acquired}` and
  `spike_placement_lock_hold_seconds`: around `LockWithin` in
  `scheduleUnderLock`.
- pprof on `127.0.0.1:6060`.
- `build/Dockerfile.manager` builds `./cmd/manager` (not `main.go` alone) so
  the patch's new file compiles.

Everything else comes from metrics VirtRigaud and controller-runtime already
export (reconcile counts and times, work queue, provider RPC histograms, Go
runtime), the API server, etcd and cAdvisor.

## 3. Method

**Stages as run** (seeded VMs are pre-populated with a bound, running
status while the manager is scaled to 0; the fake provider builds the
matching execution state from the same deterministic
[workload](../../test/spike1b/workload/workload.go)):

| Stage | Providers × hosts | Seeded VMs | Configs | Workloads |
|---|---|---|---|---|
| A | 1 × 100 | 1,000 | A B C D | steady 240 s; bursts 100, 1,000 |
| B | 1 × 100 | 10,000 | A B C D | steady 240 s; bursts 100, 1,000 (A: 1,000 aborted; C: 100 only) |
| C | 1 × 1,000 | 10,000 | A, D | steady 300 s + Host heartbeat ages; D: burst 1,000 |
| D | 1 × 1,000 | 100,000 | D, A (+ 6 GiB diagnostic) | steady; burst 1,000; A: start-up only |
| P4 | **4** × 250 | 10,000 (2,500 each) | D | steady; burst 1,000 (compare with stage C / D) |
| extras | 1 × 100 | 1,000 | D | churn 2/5/10/20 creates/s; Create latency 200 ms vs 2 s; fault injection; delete while creating; unreachable hosts |
| follow-ups | 1 × 100 / 1 × 10 | 1,000 / 0 | D | deletes during in-flight Creates (5 s Create latency); API cost of one lifecycle on an empty cluster |

**Each configuration:**
1. `helm upgrade` and a manager restart;
2. 180–300 s warm-up (the start-up storm reconciles every VM once);
3. the steady-state window;
4. bursts, each followed by deleting its VMs;
5. an invariant audit.

The [collector](../../hack/bench/spike1b/collect.sh) snapshots every metrics
source every 15 s. [`analyze.py`](../../hack/bench/spike1b/analyze.py) differences
counters over each marked window. The [driver](../../test/spike1b/driver/)
creates VMs with its own high-QPS client and times every lifecycle
transition through its own informer.

**Deviations from the requested plan** (decided during the run to shorten
it; section 8 covers what they leave untested):
- Stage B configuration A's 1,000-VM burst and configuration C's 1,000-VM
  burst were not run. Their 100-VM bursts had already shown no VM placed
  within the timeout.
- Stage C ran configurations A and D only, and stage D only D. Configuration
  A was observed only at start-up, and it never stayed up.
- Burst timeouts were cut from 20 to 10–12 minutes after stage A.
- The 5,000-VM burst was not run. A 1,000-VM burst already saturates every
  configuration at 10,000 VMs.

## 4. Results

Medians unless stated. Each configuration ran once; run-to-run variance was
not measured (section 8). Full tables:
[`tables.md`](spike-1b/results/tables.md); per-window analysis:
`results/<stage>/<config>/analysis.json`.

### 4.1 Steady state (no creates)

| Stage | Config | VM reconciles/s | API req/s | Rate-limit wait p99 | VM queue depth (mean) | Describe + Validate RPC/s | Manager CPU (cores) | CFS throttled | Working set | Alloc |
|---|---|---|---|---|---|---|---|---|---|---|
| A 1k | A | 20.0 | 27 | 0.81 s | 790 | 20 + 23 | 0.13 | 11% | 43 MiB | 9 MB/s |
| A 1k | B | 99.9 | 107 | 0.00 s | 1 | 100 + 103 | 0.28 | 21% | 45 MiB | 15 MB/s |
| A 1k | C | 20.0 | 27 | 0.81 s | 790 | 20 + 23 | 0.13 | 0% | 48 MiB | 10 MB/s |
| A 1k | D | 100.8 | 109 | 0.01 s | 0 | 101 + 104 | 0.28 | 0% | 43 MiB | 17 MB/s |
| B 10k | A | 20.0 | 26 | 0.81 s | 6,656 | 18 + 21 | 0.25 | 40% | 272 MiB | 43 MB/s |
| B 10k | B | 97.0 | 102 | 0.10 s | 9,047 | 97 + 99 | 0.42 | 56% | 261 MiB | 50 MB/s |
| B 10k | C | 20.0 | 25 | 0.81 s | 9,790 | 20 + 22 | 0.23 | 0% | 221 MiB | 40 MB/s |
| B 10k | D | 100.1 | 106 | 0.10 s | 8,990 | 100 + 103 | 0.44 | 0% | 233 MiB | 58 MB/s |
| C 10k / 1,000 hosts | A | 20.0 | 30 | 0.81 s | 6,594 | 15 + 21 | 0.50 | **100%** | 241 MiB | 86 MB/s |
| C 10k / 1,000 hosts | D | 100.0 | 139 | 0.10 s | 8,990 | 100 + 122 | 1.48 | 1% | 261 MiB | 387 MB/s |
| P4 10k / 4 Providers | D | 100.0 | 158 | 0.10 s | 5,093 | 100 + 122 | 1.80 | 6% | 415 MiB | 511 MB/s |
| D 100k (6 GiB diag) | D′ | 93.4 | 104 | 0.17 s | 84,595 | 17 + 21 | 1.77 | — | 2,111 MiB | 545 MB/s |

**Observations:**
- **The manager's request rate sits at its QPS budget** (20 or 100) in every
  configuration from 1,000 VMs up. The work queue absorbs the rest.
- **At 10,000 VMs the mean queue wait is ~90 s** (`workqueue_queue_duration`,
  stage B/C D). Each VM is effectively polled every ~100 s instead of 10 s.
  That is a backlog, not a design choice.
- **Steady-state status PUTs are almost all no-ops in etcd.** At stage A the
  manager sent 20–100 VM status PUTs/s while etcd recorded **0** VM updates;
  the API server drops an unchanged update before storage. They still cost a
  client token, a full request and API-server CPU (1.06–1.32 cores for
  110–160 req/s here). At stages B-A and C-A the first post-restart pass
  wrote real updates (17.6 of 18.2/s), because the manager was still
  completing its first reconcile of the seeded VMs.
- **`Validate` is called on every reconcile** (the Resolver validates the
  cached client before each `GetProvider`, `internal/runtime/remote/resolver.go:127-139`).
  That roughly doubles provider RPCs: Validate ≥ Describe in every row.
- **Adding hosts adds CPU, not API load.** 1,000 hosts instead of 100 at
  the same VM count (stage C vs B, config D) raised manager CPU from 0.44 to
  1.48 cores and allocation from 58 to 387 MB/s. The API rate stayed at the
  budget. The cause is the per-heartbeat O(V) scan (section 4.6).

**Cost per managed VM-hour, observed at stage A config D** (unsaturated
queue, 1,000 VMs):

| Unit | Per VM-hour | Note |
|---|---|---|
| VM reconciles | 363 | 100.8/s ÷ 1,000 × 3,600 |
| Manager API requests | 392 | ~360 of them unchanged status PUTs |
| Provider RPCs | 735 | Describe 363 + Validate 374 |
| Manager CPU | ~1.0 CPU-second | 0.28 cores ÷ 1,000 VMs |
| etcd writes | ~0 | unchanged status |

At the intended 2-minute poll (estimate), every row except the fixed per-VM
overheads divides by 12: about 30 reconciles, 60 RPCs and 30 status
requests per VM-hour.

### 4.2 Creation bursts

| Stage | Config | Burst | Reached Ready | First reconcile p50 | `pendingHost` p50 / p99 | `status.id` p50 | Ready p50 / p99 | Ready/s | Lock hold mean / p99 | Lock wait p99 |
|---|---|---|---|---|---|---|---|---|---|---|
| A 1k | A | 100 | 100 | 42 s | 95 / 105 s | 96 s | 154 / 159 s | 0.63 | 4 / 10 ms | 0 |
| A 1k | A | 1,000 | 1,000 | 65 s | 215 / 318 s | 216 s | 365 / 417 s | 2.39 | 8 / 89 ms | 0 |
| A 1k | B | 100 | 100 | 0.1 s | 1.6 / 3.3 s | 1.8 s | 10.2 / 11.8 s | 8.45 | 7 / 61 ms | 41 ms |
| A 1k | B | 1,000 | 1,000 | 5.5 s | 49 / 104 s | 50 s | 66 / 120 s | 8.24 | 15 / 148 ms | 84 ms |
| A 1k | C | 1,000 | 1,000 | 65 s | 215 / 318 s | 216 s | 365 / 417 s | 2.39 | 6 / 25 ms | 0 |
| A 1k | D | 100 | 100 | 0.5 s | 3.1 / 5.0 s | 3.4 s | 12.8 / 15.0 s | 6.65 | 3 / 10 ms | 0 |
| A 1k | D | 1,000 | 1,000 | 5.5 s | 50 / 104 s | 50 s | 66 / 120 s | 8.23 | 5 / 20 ms | 0 |
| B 10k | A | 100 | **0** | 393 s | 896 / 906 s | 896 s | — | — | 126 / 609 ms | 492 ms |
| B 10k | B | 1,000 | 963 (10 min) | 95 s | 334 / 491 s | 334 s | 547 / 599 s | 1.60 | 271 / 1,123 ms | 2.5 s |
| B 10k | C | 100 | **0** | 493 s | none | — | — | — | — | — |
| B 10k | D | 100 | 100 | 91 s | 183 / 185 s | 183 s | 282 / 284 s | 0.35 | 32 / 80 ms | 123 ms |
| B 10k | D | 1,000 | 1,000 | 95 s | 235 / 299 s | 235 s | 351 / 405 s | 2.46 | 37 / 96 ms | 133 ms |
| C 10k / 1,000 hosts | D | 1,000 | 1,000 | 95 s | 242 / 306 s | 242 s | 359 / 412 s | 2.42 | 64 / 269 ms | 594 ms |
| P4 10k / 4 Providers | D | 1,000 | 996 (10 min) | 69 s | 429 / 494 s | 429 s | 549 / 599 s | 1.66 | 27 / 146 ms | 0 |
| D 100k | D | 1,000 | **0** (12 min) | 426 s | none | — | — | — | — | — |
| empty cluster (follow-up) | D | 100 | 100 | 0.3 s | 1.3 / 2.5 s | 1.5 s | 6.5 / 7.7 s | — | — | — |

**Every create waits at least ~5 s between `status.id` and Ready.** After a
successful Create the controller requeues after a fixed 5 s
(`virtualmachine_controller.go:1898-1899`) and only then Describes and sets
Ready. On an empty cluster that is ~75% of time-to-Ready.

**Saturation point and primary cause, per stage:**

| Stage | First saturated | Evidence |
|---|---|---|
| A, config A / C | Client QPS | Rate-limiter wait p99 0.81 s; 27 req/s flat; CPU 0.13 cores; raising CPU (C) changed nothing, raising QPS (B) gave 3.4× |
| A, config B / D | Client QPS (at 100) | 100 status PUT/s of polling exactly fills the budget; 1,000-VM burst throughput 8.2 VMs/s with or without more CPU |
| B, config A / C | Client QPS → work queue | Queue 6,600–9,800; new VMs wait full queue passes for each of their ~3–4 reconciles; none Ready in 15–20 min |
| B, config B | CPU (CFS quota) | 56% of periods throttled; lock hold 271 ms vs 37 ms with 2 CPU; Ready p50 547 s vs 351 s |
| B, config D | Client QPS | Unthrottled; 100 req/s flat; queue wait ~90 s |
| C, config D | Client QPS, then Host controller | 1.48 cores, unthrottled; Host syncs 16.5/s vs 16.7/s needed (section 4.5) |
| D, config A | Memory | OOM-killed ~20 s after every start (cache initial list) |
| D, config D | Memory, then CPU | OOM at 2 GiB during the burst and on restart; at 6 GiB, 1.77 cores and 67% of Hosts without a heartbeat |

**The scheduler's share of the lifecycle** (observed lock hold against
Spike 1A):
- At stage B config D the lock was held a mean of **37 ms** per decision.
  Spike 1A's microbenchmark predicted 21 ms for 100 hosts × 10k VMs on 20
  threads; the difference is real objects and a manager doing other work.
- 1,000 decisions × 37 ms = 37 s of serialized lock time, inside a burst
  whose last VM became Ready after 405 s.
- Per VM, the decision is ~0.01% of time-to-Ready.
- The lock is never where a VM waits: wait p99 is at most 133 ms with 2 CPU
  and 2.5 s when CPU-throttled. No acquisition timed out (`busy` = 0 in
  every run).

### 4.3 Controlled comparisons

| Change (all else fixed) | Effect | Evidence |
|---|---|---|
| QPS 20 → 100, same 500m CPU (A → B, stage A) | 1,000-VM burst Ready p50 365 → 66 s; throughput 2.4 → 8.2 VMs/s; steady reconciles 20 → 100/s | 4.1, 4.2 |
| CPU 500m → 2, same QPS 20 (A → C, stage A) | No measurable change | 4.1, 4.2 |
| CPU 500m → 2 at QPS 100 (B → D, stage B) | Ready p50 547 → 351 s; lock hold 271 → 37 ms; throttling 56% → 0% | 4.2 |
| Fake Create latency 200 ms → 2 s (extras, 500-VM burst, config D) | Ready p50 40 → 126 s, p99 63 → 131 s; Describe rate 26/s during the 2 s burst vs ~93/s in the comparable fault-injection burst window (workers blocked in Create) | `extras/b500*-summary.json`, `analysis.json` `latency2s` |
| 1 Provider → 4 Providers, same 1,000 hosts / 10k VMs (C-D → P4-D) | Ready p50 359 → 549 s; Provider reconciles 5.2 → 11.4/s; manager CPU 1.48 → 1.80 cores; alloc 387 → 511 MB/s; Host heartbeat max age 61 → 97 s; lock hold 64 → 27 ms | 4.1, 4.2, 4.5 |
| 100 → 1,000 hosts, same 10k VMs (B-D → C-D) | Burst unchanged (351 → 359 s); manager CPU 0.44 → 1.48 cores; alloc 58 → 387 MB/s | 4.1 |

**Partitioning shortened the lock (64 → 27 ms) but lengthened time-to-Ready.**
Per-Provider and per-Host work that scans every VM grew with the number of
Providers. The client budget, shared by all Providers in one manager, did
not grow at all.

### 4.4 Sustained churn (stage extras: 1,000 VMs, config D)

Small VMs are created at the requested rate for 180 s, and each is deleted as
soon as it is Ready. Then load stops and the backlog drains.

| Requested creates/s | Achieved Ready/s | Ready p50 / p99 | Backlog at t = 180 s: created-not-Ready / deleting | Drained at |
|---|---|---|---|---|
| 2 | 1.9 | 10.3 / 10.4 s | 20 / 2 | 195 s |
| 5 | 4.5 | 20.8 / 21.6 s | 106 / 25 | 205 s |
| 10 | 7.9 | 47.7 / 55.2 s | 532 / 140 | 240 s |
| 20 | 9.4 | 137 / 204 s | 2,270 / 216 | 395 s (215 s after load stopped) |

- **Sustainable churn here is about 8–9 create+delete cycles/s.** Above
  that, the backlog grows linearly with time.
- **The backlog recovers once load drops:** 2,270 VMs drained in 215 s. Its
  drain rate (~10/s) matches the same QPS-bound throughput.
- Time series: `extras/c*-series.csv`.

### 4.5 Host inventory

| Stage | Config | Hosts | Host syncs/s (needed: hosts ÷ 60) | Heartbeat age p50 / p90 / max | Hosts never heartbeated |
|---|---|---|---|---|---|
| A, B | D | 100 | 1.7 (1.7) | 21–54 / 23–55 / 24–56 s | 0 |
| C | A | 1,000 | 5.3 (16.7) | 98 / 172 / 192 s | 0 |
| C | D | 1,000 | 16.5 (16.7) | 29 / 55 / 61 s | 0 |
| P4 | D | 4 × 250 | 10.4 (16.7) | 43 / 83 / 95 s | 0 |
| D (100k) | D | 1,000 | 2.9 (16.7) | 785 / 1,026 / 1,030 s | **673** |

The single-worker Host controller (`host_controller.go:507-516`) keeps its
60 s interval at 1,000 hosts **only** with QPS 100 and an unloaded CPU, and
then only just (16.5 of 16.7 syncs/s).

**Each sync costs (observed):**
- one `GetHostInfo` (21–32 ms at the fake's 20 ms);
- one status PUT, a real etcd write because `lastHeartbeatTime` changes;
- an O(V) scan for the committed-capacity gauges.

**Estimate:** with a real agent answering in 100 ms, one worker serves at
most ~600 hosts per 60 s, before counting the scan.

**Effect on scheduling (observed at 100k):** Hosts that never heartbeated
after the restart stayed not-Ready, and the filter removes not-Ready hosts.
The 1,000-VM burst got **no `pendingHost` at all** in 12 minutes. A slow
inventory loop is therefore not only a staleness problem; it removes
capacity.

### 4.6 100,000 VMs: memory and the full scans

| | Observed |
|---|---|
| Populate 100k VMs + status + marker (driver, QPS 3,000) | 122 s |
| Config A (512 MiB) | OOM-killed (`anon-rss` 521 MB) ~20 s after each start; CrashLoopBackOff |
| Config D (2 GiB), first start | Survived; steady working set 863 MiB |
| Config D, 1,000-VM burst | Working set 1.58 GiB, then OOM-killed (`anon-rss` 2.09 GB) twice; restarts crash-looped at 2 GiB once the objects carried manager-written status |
| Diagnostic 6 GiB | Peak working set 2.1 GiB, heap 1.8 GiB, 545 MB/s allocation, 0.75 GC/s, 1.77 cores |
| Cache initialisation (container start → VirtualMachine workers started; includes leader election) | 10k VMs: **18 s** (peak working set 195 MiB in the first 2 min). 100k VMs: **30 s** at 2 GiB, then OOM-killed twice within 2 min (peak 1,882 MiB seen); **28 s** at 6 GiB, peak 2,274 MiB. Separate runs: [`cache-init/`](spike-1b/results/cache-init/) |

**CPU profile at 100k** (diagnostic run,
[`stage-D/diag-6Gi/profiles/`](spike-1b/results/stage-D/diag-6Gi/profiles/)):

| Path | CPU share | Allocation share |
|---|---|---|
| `ProviderReconciler.countConnectedVMs`: `r.List` of **all** VirtualMachines, with deep copies, to count one Provider's | 36% | 55% |
| `HostReconciler.publishCommitted` → `committedOnHost` → `listProviderVMs` (+ `SetList`): one Provider-wide scan per host heartbeat | 35% | 41% |
| GC | ~17% | — |

**Every Host status change re-enqueues its Provider** (Host watch,
`provider_controller.go:1456-1463`). With 1,000 hosts heartbeating each
minute, the Provider reconcile and its cluster-wide deep-copying List run
several times per second. Neither scan is in the placement path that
Spike 1A measured; both run on events placement does not trigger.

### 4.7 API server and etcd

**The control plane was never saturated:**
- 0 API Priority and Fairness rejections in any run;
- at most 54 requests in flight;
- API latency mean 7–15 ms, p99 ≤ 105 ms;
- API-server CPU 1.06–1.32 cores, unlimited in kind;
- etcd CPU ≤ 0.22 cores.

All the limits above are VirtRigaud's own. This holds on this kind cluster
with no limits on the control plane. A production API server shared with
other tenants, or a managed one with lower APF shares, could become the
limit at the higher QPS settings. That is not measured here.

### 4.8 API and RPC cost of one VM lifecycle

Measured on an empty cluster (10 hosts, no seeded VMs), with 100 VMs created,
Ready, then deleted. All counts are per VM:

| Manager → API server | Count |
|---|---|
| VM PUT (finalizer add, remove) | 2.0 |
| VM PATCH (placement-uid marker) | 1.0 |
| VM status PATCH (`pendingHost`) | 1.0 |
| VM status PUT (bind, Ready, …) | 3.0 |
| Provider GET (not from cache) | 1.0 |
| **Total** | **~8** |

| Manager → provider | Count |
|---|---|
| Create, Delete, Describe | 1.0 each |
| ListVMs (pre-schedule uniqueness check, R4) | 1.0 |
| GetCapabilities (live, per create) | 1.1 |
| Validate | 3.2 |
| **Total** | **~8–9** |

The ~8 API requests mean that at QPS 20, creates alone cap at about 2.5/s,
**before** any polling. That matches the 2.4 VMs/s observed at stage A
config A (estimate; the burst also competes with polling).

## 5. Defects and inefficiencies found in production code (not fixed)

Each was observed in this spike's measurements, and each is cited to the
code at `ed74c8fe`. None is fixed here; per the stop condition, these feed
the next increment.

| # | Finding | Location | Measured effect |
|---|---|---|---|
| F1 | Poll interval never matches: provider power states are `On`/`Off`, the switch expects `poweredOn`/`poweredOff`; every VM polls every 10 s | `virtualmachine_controller.go:3175-3204`; `contracts/types.go:358-360` | 12× (On) to 30× (Off) the intended steady load; saturates QPS 20 at 1,000 VMs |
| F2 | Full VM status PUT on every reconcile, changed or not | `virtualmachine_controller.go:959`, `updateStatus` `:2985` | 1 API request per reconcile; etcd write avoided by the API server, client token and API CPU not |
| F3 | `Validate` RPC before every use of a cached provider client | `internal/runtime/remote/resolver.go:127-139` | Doubles provider RPCs (Validate ≈ Describe + 3/s) |
| F4 | `countConnectedVMs` lists every VirtualMachine in the cluster, deep-copied, on every Provider reconcile; Provider reconciles on every Host status change | `provider_controller.go:1352-1375`, Host watch `:1456-1463` | 36% CPU, 55% allocation at 100k; makes per-Provider partitioning cost more |
| F5 | Host committed-capacity gauges re-scan the Provider's VMs on every heartbeat | `host_controller.go:193-209` → `committedOnHost` | 35% CPU, 41% allocation at 100k; 387 MB/s allocation at 10k × 1,000 hosts |
| F6 | One Host worker; each sync is a provider RPC plus a status write | `host_controller.go:507-516`, `:468-477` | Heartbeat max 192 s at QPS 20 (1,000 hosts); 673 Hosts never heartbeated at 100k |
| F7 | Fixed 5 s requeue between a successful Create and Ready | `virtualmachine_controller.go:1898-1899` | ~5 s floor on time-to-Ready (6.5 s p50 unloaded) |
| F8 | Create runs synchronously in the reconcile; provider latency occupies VM workers | `createVMOn`, `virtualmachine_controller.go:1804` | Create 2 s: Ready p50 3.1× slower, steady polling −72% |
| F9 | Chart defaults (500m / 512 MiB, QPS 20) | `charts/virtrigaud/values.yaml:23-30`, `cmd/manager/main.go:335-336` | OOM at 100k VMs; QPS-bound at 1,000 |

## 6. Correctness and failure injection

### 6.1 The auditor

[`driver audit`](../../test/spike1b/driver/audit.go) reads every
VirtualMachine and every fake agent's execution records, then checks:

| Invariant | How it is checked |
|---|---|
| No allocation beyond admitted capacity | Per host, the sum of footprints of VMs whose `placement.host` or `pendingHost` names it, and the sum of executed VMs, ≤ allocatable × 1.0. Hosts holding only seeded VMs are reported separately (6.3). |
| No duplicate execution identity | No owner UID (or unadopted seeded name) executes on two hosts. The fake also counts it live (`spike_fake_duplicate_identity_total`, 0 in every run). |
| Execution only where placement says | Every execution's VM names that host in `placement.host` or `pendingHost`. |
| Bound VMs execute | Every VM with `status.id` has a matching execution owned by its UID on its bound host. |
| No allocation leak after deletion | No execution without a VirtualMachine. |
| Durable `pendingHost` before execution | Covered by the third row: an execution on a host the VM's placement never named would be flagged. |

### 6.2 Results

| Scenario | Audit | Notes |
|---|---|---|
| Stage A, all configs, after steady + bursts | 0 violations | |
| Stage B / C / D / P4 after bursts | 0 violations, once seed artefacts are excluded (6.3) | |
| Churn 2, 5, 10, 20/s (6,660 VMs created and deleted) | 0 | |
| Create latency 2 s | 0 | |
| **Faults:** 20% of Creates fail with HOST_UNAVAILABLE before executing, 20% execute and return OUTCOME_UNKNOWN_RETRY (lost reply); 300-VM burst | 0 | All 300 Ready; `status.id` p99 148 s (retries with backoff); lost replies re-sent and bound the existing VM by owner (`SupportsOwnerRebind`); no second execution |
| Delete while creating: 300 VMs deleted 3 s after submit | 0 | Deleted before their first reconcile |
| **Delete during in-flight Create** (follow-up): Create 5 s, delete at 4 s, 200 VMs | 0 | 10 VMs had their `pendingHost` recorded (Create sent or about to be sent) when deleted; all objects gone; no orphan execution. Only 10 reached this window, so the coverage is narrow |
| **Unreachable hosts:** 2 of 100 hosts answer HOST_UNAVAILABLE during a 200-VM burst | 0 | 196 Ready; 4 held `Placed=False/HostUnavailable` with `pendingHost` on the unreachable host: **not re-scheduled** (ADR-0007 A2) |
| …after the hosts return (180 s) | 0 | All 200 bound (`status.id`) on the hosts their placement named, 198 of them Ready within the 180 s (2 still inside their blocked-retry backoff); no duplicate |
| After deleting every test VM | 0 | 1,000 executions = the seed; no leaks |

### 6.3 Apparent violations traced to the experiment, not VirtRigaud

- **Seed over capacity (stages B, C, D).** The seed generator assigned VM *i*
  to host *i* mod H and class *i* mod 10. With H a multiple of 10, every host
  with index ending in 9 received only xlarge VMs and started over capacity.
  The scheduler correctly placed nothing there. The auditor now reports such
  hosts as `seeded_hosts_over_capacity` (informational), and the generator is
  decorrelated for the later runs (P4, extras, follow-ups).
- **Two orphan executions after stage B config A.** These were VMs deleted
  while their Create was in flight. The manager's cleanup Delete addresses a
  pending create by its bare name plus owner identity, as documented on
  `pendingCreateRef` (`internal/controller/vmref.go:186-202`). The libvirt
  provider resolves that name through the owner; the fake did not, so it
  answered NotFound. After fixing the fake, the in-flight follow-up above
  produced no orphan. **A real host agent must implement the same
  resolution** (section 10).

### 6.4 What cannot be proved without real agents and hypervisors

- **Execution.** The fake's record is the execution. A real VM can fail
  after "start", be killed by the host's OOM killer, or keep running after
  its agent loses its state.
- **Partitions.** "Unreachable" here means instant, consistent refusal. A
  partition in which the agent still runs VMs but the provider cannot reach
  it, and the VM keeps writing to shared disk, is exactly the case D8/A4
  forbid failover for. The no-reschedule behaviour was observed; fencing
  cannot be tested without hardware.
- **Agent restart and recovery.** After an agent restart, its local state
  must be reconciled with what is actually running. A durable per-host
  identity store is needed and was not modelled.
- **Concurrency inside a host** (real resource contention, slow disks) and
  image/disk preparation time.
- **Clock and ordering** effects of a real network: here every RPC ran on
  one machine.

## 7. Analysis: what saturates first

The order of limits, by evidence:

1. **Steady-state polling consumes the API budget** (F1 + F2, with F3
   doubling the RPCs). Everything else in a busy manager queues behind it:
   creates, deletes, host heartbeats.
2. **Once the budget is raised, per-VM CPU work becomes the limit.** At the
   500m default this shows as throttling. At 10k VMs × 1,000 hosts and 100k
   VMs it shows as the full scans F4/F5, which also drive allocation, GC and
   memory, until the manager is OOM-killed.
3. **Provider latency limits throughput through worker occupancy** (F8),
   independent of the API budget.
4. **Host inventory degrades under all three**, and degraded inventory then
   removes capacity from the scheduler (F6).
5. **The scheduler** (placement decision under the lock) is a minor
   contributor at every measured size.

This is consistent with Spike 1A:
- The decision costs tens of milliseconds at 10k VMs, which is negligible
  next to queue waits of tens to hundreds of seconds.
- It reaches ~240 ms only at 100k VMs, where the manager has already failed
  for other reasons.

**When more scheduler concurrency would help:** only when lock hold
dominates time-to-Ready. That would take the steady-state fixes,
QPS/CPU, inventory and full-scan work all done first, plus Spike 1A's O(V)
decision at ≥ 100k VMs per scheduling domain. Nothing measured here is in
that regime.

## 8. Recommendations (next increments, prioritised by measured benefit)

| Priority | Change | Expected benefit (estimate from these measurements) | Risk / note |
|---|---|---|---|
| 1 | **Fix F1** (map `On`/`Off` in `getRequeueInterval`, fix the tests that encode the vSphere strings) | Steady reconciles ÷12 for running VMs, ÷30 for stopped. 10k VMs need ~83 req/s instead of ~1,000, fitting QPS 100 with headroom | Bug fix; observable behaviour change (status freshness 2 min) |
| 2 | **Skip unchanged status writes (F2)** | Removes ~1 API request per steady reconcile; with F1, steady API load falls to near zero | Must keep `ObservedGeneration` and condition semantics; compare before write |
| 3 | **Stop validating on every call (F3)**: validate on failure or on a timer | Provider RPCs ÷2 | Keep fail-closed behaviour on a broken client |
| 4 | **Remove per-event full scans (F4, F5)**: count connected VMs and committed capacity from an index or incrementally; do not re-enqueue Providers on every Host heartbeat | Removes ~70% of manager CPU and ~95% of allocation at 100k; prerequisite for > 10k VMs per manager and for partitioning to help | F5 overlaps Spike 1A's incremental committed index: one shared index serves both |
| 5 | **Host heartbeat (F6)**: parallel workers, skip writes when only the timestamp moves (or a Lease-style heartbeat), decouple gauges | Keeps inventory fresh at 1,000+ hosts; avoids losing capacity to stale Hosts | A heartbeat redesign touches ADR-0007 D3 observed-state semantics |
| 6 | **Make the client budget configurable** (QPS/burst flags), with chart defaults sized for the target fleet | 3.4× create throughput at 1k VMs (A → B) | Do it after 1–3, or it only moves load onto the API server |
| 7 | **Raise default resources** for large fleets, or document sizing | Avoids OOM at 100k (≥ 2 GiB needed before item 4; less after) | Documentation plus chart values |
| 8 | **Asynchronous Create** (return a task, poll) or a separate create worker pool (F8) | Provider latency stops starving polling and other creates | Interacts with the durable-marker and unknown-outcome rules (A2); design first |
| 9 | Remove the fixed 5 s post-create requeue (F7) | Time-to-Ready floor from ~6.5 s to ~1.5 s unloaded | Small |
| 10 | Incremental committed-capacity index (Spike 1A) | Lock hold from ~37 ms (10k) and ~240 ms (100k, Spike 1A) to O(hosts) | Becomes relevant after 1–5; share with item 4 |
| — | **Increase `MaxConcurrentReconciles`** | **None now**: workers wait in the rate limiter or the queue, not idle | Revisit after 1–3 and 8 |
| — | **Internally shard one logical scheduling domain** | **Not supported by evidence yet.** Partitioning by Provider made things worse here (F4) | Re-test after item 4 |
| — | **Shared-state scheduling workers** | **Not supported by evidence.** The scheduler is < 1% of lifecycle latency everywhere measured | The single-logical-cluster target keeps it open; the trigger is lock hold dominating after items 1–10 |

**Recommended next engineering task:** a "steady-state cost" increment with
items 1, 2 and 3 (small, testable, no API change), followed by item 4. Then
**re-run Spike 1B stages B and C** with the same harness to confirm. The
harness is reusable as it stands.

## 9. Limitations and untested scenarios

- **One run per configuration**: no variance estimate. Bursts at saturation
  are sensitive to timing.
- **Shared workstation, single kind node pair.** The API server, etcd,
  manager and fake share one host's 20 threads. An unlimited control plane
  likely hides API-server limits a production cluster would have.
- **The fake answers instantly to "unreachable"**, has no state loss and no
  partial failures. RPC latency is a configured distribution, not a
  measurement of a real agent.
- **TLS between manager and provider was disabled.** mTLS handshake and
  encryption costs are not measured.
- **Not run:**
  - 5,000-VM bursts;
  - configurations B and C at stages C/D;
  - B and C 1,000-VM bursts at stage B;
  - churn above 1,000 VMs;
  - leader failover and multiple manager replicas;
  - snapshots, clones, migrations, reconfigure and resize admission;
  - VMs without guest IPs (the 10 s "waiting for IP" path);
  - powered-off VMs.
- **Cache-initialisation figures** come from separate runs (section 4.6), not
  from the stage runs (the in-stage capture failed on log format).
- **The 100k analysis** relies on the 6 GiB diagnostic run plus gauges from
  the 2 GiB run, because the 2 GiB manager kept restarting and most of its
  counter snapshots are missing.

## 10. Implications for the Cloud Hypervisor Cluster Architecture

**One logical host cluster.**
- Nothing measured requires splitting the logical cluster into Providers to
  scale the *scheduler*.
- Splitting into Providers made the system slower today, because per-Provider
  work scans every VM (F4).
- The limits are per-manager costs (API budget, per-VM polling, full scans),
  and they grow with VMs and hosts, not with the number of scheduling
  domains.
- A single logical cluster is viable for the control plane once those costs
  are removed. Partitioning should be an internal implementation detail
  chosen later by measurement, not an operator-visible Provider split.

**Scheduler architecture.**
- Keep the centralized scheduler.
- The placement decision was ≤ 0.01% of a VM's time-to-Ready at 10k VMs.
  Its O(V) cost (Spike 1A) matters only beyond the sizes where the manager
  already fails for other reasons.
- The incremental committed index belongs with the F4/F5 fixes, because they
  need the same data.
- Shared-state workers stay an option for a cluster that, after those fixes,
  shows lock hold dominating creation latency. The current measurements
  provide no case for them.

**Host inventory.**
- At 1,000 hosts the existing heartbeat path (one worker, one status write
  and one O(V) scan per host per minute) is at its limit.
- Under load it went stale enough to take capacity away from the scheduler.
- For a large Cloud Hypervisor cluster, inventory should be a separate,
  cheap, parallel path:
  - agents push or are polled concurrently;
  - status is written only when capacity or health changes;
  - liveness is tracked separately from capacity.
- The point is that inventory staleness must not silently remove hosts from
  placement. Deciding liveness for fencing purposes is still P5.

**Provider scaling.**
- The fake provider used ≤ 0.2 cores even at 100+ RPC/s. The provider pod
  was not the limit here.
- The real cost is in the manager's per-VM RPCs: Describe every poll,
  Validate on every call, a live GetCapabilities and ListVMs per create.
- A Cloud Hypervisor provider fronting hundreds of hosts will see that RPC
  rate multiplied. Reducing manager-side calls (items 1–3) matters more than
  provider HA for throughput.
- The single-replica, Recreate provider deployment remains an availability
  concern, not a throughput one.

**Agent-based execution.**
- The fake modelled what an agent must do, and the spike showed which
  contract details matter:
  - **Owner identity:** stamping, rebinding a repeated Create
    (`supports_owner_rebind`), and refusing another owner's name were
    exercised by lost replies and repeated Creates, with no duplicate
    execution.
  - **Pending-create addressing:** an agent must resolve a Delete or Describe
    sent with the bare VM name plus owner, the `pendingCreateRef` contract.
    The fake's omission produced exactly the orphan this contract prevents.
  - **Bounded per-host concurrency:** latency per call directly limits
    throughput while Create stays synchronous (F8).
- An agent with real latency (seconds per start) makes asynchronous Create
  (item 8) important.

**API and state-management boundaries.**
- The Kubernetes API is the bottleneck VirtRigaud creates for itself. Every
  poll becomes an API request, even when nothing changes.
- For a large cluster, the API should carry desired state and meaningful
  observed transitions (bound, Ready, failed), not periodic observation of
  every VM.
- High-frequency observed state (power, IPs, host capacity, heartbeats)
  should stay in the provider/agent path and in memory. It should reach
  etcd only when it changes.
- This keeps etcd as the source of truth for decisions and durable intent
  (ADR-0007 D1, A2) without making it the transport for telemetry.

## Reproducing

```bash
hack/bench/spike1b/build-images.sh                       # experiment manager + fake provider images
hack/bench/spike1b/stage.sh A 1 100 1000 10 "A B C D" "100 1000"
hack/bench/spike1b/stage.sh B 1 100 10000 10 "A B C D" "100 1000"
hack/bench/spike1b/stage.sh C 1 1000 10000 10 "A D" "1000"
hack/bench/spike1b/stage.sh D 1 1000 100000 10 "D" "1000"
hack/bench/spike1b/stage.sh P4 4 1000 10000 10 "D" "1000"
hack/bench/spike1b/extras.sh                              # churn, latency, faults, unreachable hosts
hack/bench/spike1b/followups.sh                           # in-flight deletes, empty-cluster API cost
hack/bench/spike1b/cacheinit.sh 1000 100000 D 6Gi         # cache initialisation
python3 hack/bench/spike1b/tables.py docs/performance/spike-1b/results > docs/performance/spike-1b/results/tables.md
```

Every script uses the kind context `kind-virtrigaud-spike1b` explicitly and
restores the caller's current kubeconfig context after `kind create`.

**Files added by this spike** (untracked; nothing committed):

```
hack/bench/spike1b/   build-images.sh, kind-config.yaml, values-{A,B,C,D}.yaml,
                      manager-experiment.patch, Dockerfile.fakeprovider, run.sh, stage.sh,
                      extras.sh, followups.sh, cacheinit.sh, collect.sh, analyze.py, tables.py
test/spike1b/         fakeprovider/, driver/, workload/   (test-only; no production import)
docs/performance/spike-1b-control-plane-baseline.md
docs/performance/spike-1b/results/   (raw snapshots, summaries, CSVs, profiles, audits)
```
