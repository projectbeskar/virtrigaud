# F7: post-Create readiness latency

> **Status:** implemented on `feature/f7-post-create-readiness` (manager only; no
> API, CRD or gRPC change). Measured on the Spike 1B kind harness with the fake
> clustered provider. Defect F7 is described in
> [Spike 1B §5](spike-1b-control-plane-baseline.md#5-defects-and-inefficiencies-found-in-production-code-not-fixed).

## Root cause

A provider `Create` answers only the VM's id (and an optional task reference);
power state, IPs and `Ready` come from a `Describe`. After a successful
clustered Create the controller wrote the id and returned a fixed
`RequeueAfter: 5s`, and the update filter (`vmUpdateNeedsReconcile`) dropped
that status write as status-only. Every new VM therefore waited 5 s before the
`Describe` that makes it `Ready`. The single-host path waits the same 5 s after
`recordCreatedVM`.

## Change

`vmUpdateNeedsReconcile` passes one more update: the first recording of
`status.id` (empty to set) on a VM that is not being deleted (`createRecorded`
in `internal/controller/virtualmachine_controller.go`). That write now starts
the first `Describe` at once, through the existing watch:

- the event is delivered after the informer cache holds the id, so the
  reconcile it starts takes the `Describe` path, never the create path;
- that reconcile records no new id, so it cannot wake itself again;
- the 5 s requeue stays as the fallback for a failed status write
  (`createdDescribeFallback`), so a VM is still re-checked at the historical
  cadence when the event never comes.

Steady-state polling, error retries, the create marker, `pendingHost`,
unknown-outcome handling and the delete-path filter are unchanged. The clone
and adoption controllers' id writes now also wake the VM at once.

## Measured effect

All runs: one kind cluster at steady state with 1 clustered Provider, 1,000
Hosts and 10,000 VMs; manager at the chart defaults (500m CPU, 512 MiB, client
QPS 20 / burst 40); the only difference between runs is the manager image.
Latencies are from each VM's creation. Raw results and their provenance:
[`spike-1b/results/stage-F7/`](spike-1b/results/stage-F7/README.md).

**10-VM burst (idle VM queue):**

| Manager | `Ready` p50 / p90 / max | bound → `Ready` min / p50 / max |
|---|---|---|
| F1–F5 | 6.3 / 6.7 / 6.9 s | 5.0 / 5.0 / 5.1 s |
| F1–F5 + F7 | **1.3 / 2.0 / 2.0 s** | **0.0 / 0.0 / 0.2 s** |

**100-VM burst:**

| Manager | Runs | `Ready` p50 / p95 / p99 | bound → `Ready` min | VMs bound ≥ 18 s into the burst: bound → `Ready` median |
|---|---|---|---|---|
| F1–F5 | 3 controls | 20.5 / 22.9–23.8 / 24.1–24.7 s | 5.0 s in each | 5.0 s |
| F1–F5 + F7 | 1 | 20.5 / 22.7 / 22.9 s | **2.6 s** | **3.8 s** |

With an idle queue F7 removes the full 5 s. In a 100-VM burst the median does
not move: VMs bound early wait 10–15 s in every run behind the burst's own
placement decisions, which are serialized per Provider (lock hold ~150 ms per
decision at this scale and CPU limit), so the fixed 5 s overlapped that queue
wait. Only the tail improves. Every run reached `Ready` 100/100 (10/10), steady
windows made no VM status writes, and every audit found 0 violations.

The F1–F5 + F7 100-VM result is a single run against three closely agreeing
controls. The fourth F1–F5 reference in the table's range, the post-convergence
burst of the F1–F5 validation, was run on the same cluster earlier and is kept
locally with that branch's results.

## Cost and follow-up

- **One redundant `Describe` per new VM.** The fallback requeue still fires
  once, about 5 s after the Create, after the event-driven reconcile already
  described the VM. It writes nothing (the status is unchanged). Removing it
  needs `updateStatus` to report whether the write landed, so the fallback is
  armed only on failure — a possible follow-up, not part of this change.
- **Single-host VMs** go through the same filter; this harness measures the
  clustered path only.
- **Burst latency at scale** remains bounded by serialized placement decisions
  (Spike 1B; the F1–F5 validation), which this change does not address.
