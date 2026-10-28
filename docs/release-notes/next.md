<!--
Release notes for the VirtRigaud release after v0.4.1. No version is chosen yet; title the
release when it is. The notes are cumulative since v0.4.1 and describe the move from v0.4.1.
See docs/upgrading.md ("Upgrading from v0.4.1 to the next release") for the upgrade guide these
notes link to, and CHANGELOG.md for the complete, dated changelog. The v0.4.1 notes are kept in
v0.4.1.md. When this release ships, this file is archived under its version and a new next.md
is started.
-->

This release makes the manager scale to large clustered Providers and closes the gaps in how a
clustered Provider's capacity is reserved. It changes no API field's meaning, no provider and no
gRPC message; it adds one optional `VirtualMachine` status field.

- **Performance:** running VMs are polled every 2 minutes (5 when powered off) instead of every
  10 seconds, unchanged status is not rewritten, a provider connection is validated once instead
  of on every reconcile, Provider VM counts and Host committed capacity are read from indexes
  instead of full scans, a new VM is described at once after its `Create`, and a placement
  decision allocates a fraction of what it did.
- **Admission safety (clustered Providers):** a placement is released only on evidence, never on
  a timer alone; an admitted resize-up is reserved durably before it is sent; an admission
  inherited from an earlier manager is checked again before it is executed; a VM whose size
  cannot be established is never counted at a guessed size.
- **Operations:** the manager's Kubernetes client rate limits are configurable
  (`--client-qps`, `--client-burst`; chart `manager.clientQPS`, `manager.clientBurst`). The
  defaults are unchanged (20 and 40).
- **Compatibility:** install the CRDs before the manager (the chart's hook does); **do not roll
  back while a clustered VM holds a resize reservation**, and roll the manager and the chart
  back together. See [Upgrade](#upgrade).

## Performance

Measured in a disposable kind cluster with a fake clustered provider (Spike 1B; the method,
environment and raw results are in
[`docs/performance/spike-1b-control-plane-baseline.md`](../performance/spike-1b-control-plane-baseline.md)),
and with Go benchmarks on one machine (Spike 1A). They are not production measurements: your
numbers depend on your API server, hypervisors and manager resources.

- **Steady-state polling (F1/F2).** Every running VM was reconciled every 10 seconds instead of
  every 2 minutes, because the requeue interval compared the provider's power state with
  vSphere's raw strings; each reconcile also rewrote the VM's status. Both are fixed: a status
  write that would store nothing is skipped. Spike 1B smoke test at 1,000 VMs with the chart
  defaults: steady VM status writes 20/s → 0, manager API requests 27.2/s → 7.1/s, and a 100-VM
  burst reached `Ready` at p50 154 s → 21 s.
- **Provider connections (F3).** A cached provider client is reused without a `Validate` RPC while
  its Provider's configuration (UID, generation, type, endpoint, TLS Secret version), its
  connection and its circuit breaker are unchanged. This removes one provider RPC per VM
  reconcile (on libvirt, a call to the host). A changed configuration, a failing connection or an
  open breaker validates again.
- **Full scans (F4/F5).** `Provider.status.connectedVMs` is counted through a field index
  instead of a deep copy of every `VirtualMachine` in the cluster, and the Provider controller no
  longer reconciles on every Host heartbeat (a 60-second resync replaces that trigger for
  clustered Providers). A Host's committed-capacity gauges and its in-use check read the VMs
  placed on that host from a per-host index. In benchmarks at 100,000 VMs on one Provider, the
  count fell from 245 ms to 77 ms per call and the per-heartbeat read from 116 ms to 46 µs.
- **Post-create readiness (F7).** A new VM is described as soon as its `Create` is recorded,
  instead of after a fixed 5 seconds. With 1,000 Hosts and 10,000 VMs, a 10-VM burst reaches
  `Ready` at p50 1.3 s instead of 6.3 s. In a 100-VM burst the median stays at 20.5 s, because
  VMs queue behind the Provider's serialized placement decisions; the p99 falls from 24.1–24.7 s
  to 22.9 s.
- **Placement decisions (C′).** A decision no longer copies every `VirtualMachine`; it reads the
  informer's index directly and sums capacity per host. In benchmarks, a decision at 100 hosts
  and 10,000 VMs takes 7.9 ms instead of 18.4 ms and allocates 435 KB instead of 16.9 MB, and the
  Provider's lock is held for a p50 of 7.7 ms instead of 20.4 ms. The decisions are identical
  (equivalence tests). A Provider's decisions are still serialized by its lock.

## Admission safety

These apply to `topology: cluster` Providers. Single-host Providers are unchanged. Details:
[`docs/clustered-provider-inventory.md`](../clustered-provider-inventory.md) and ADR-0007's
amendments.

- **A placement is released on evidence, never on a timer alone (G1/G2/G4).** An in-memory
  placement assumption expired after two minutes even if its `pendingHost` write had landed and
  the cache did not show it yet. A `pendingHost` write that timed out after landing could also be
  followed by a second placement of the same VM. Either let a competing VM overbook the host. A
  pick is now released only when the cache shows its record, the API server rejected its write,
  or an authoritative read resolves an ambiguous write. While a write is unresolved the VM is
  not placed again.
- **A resize-up is reserved durably before it is sent (G3).** The admitted footprint is written to
  the new `status.placement.resizeReservation` (a resourceVersion-checked status write) before
  `Reconfigure` is sent. If the API server does not keep the field (an older CRD), the resize is
  held with `Reconfiguring=False/PlacementError` instead of being sent unreserved. The reservation
  counts until the provider has confirmed the applied size: `Reconfiguring=False` with reason
  `ReconcileSuccess`, and a `Describe` that reports the VM's vCPUs and memory maximum. Any other
  outcome keeps it: failed, unanswered, pending a restart, asynchronous, refused before it was
  sent, or a reason the manager does not recognise.
- **Inherited admissions are checked before they are executed (C1).** Leader election does not
  fence writes, so a `pendingHost` or reservation write sent by a previous leader can land after
  the new leader gave the capacity to another VM. A durable admission this manager instance did
  not record itself (a pending create, a pending clone target, or a resize reservation) is
  checked once against the host's committed capacity before `Create`, `Clone` or `Reconfigure`
  is sent. One that no longer fits is held with `InheritedAdmissionOverCapacity`: its record is
  kept and keeps counting, and nothing is cleared automatically. Bound VMs (describe, power,
  delete) are never held.
- **A VM whose size cannot be established is never counted at a guessed size.** A placed VM whose
  VMClass is deleted, or no longer granted to its namespace, used to count at the minimum
  footprint (1 vCPU, 128 MiB). It now counts from its recorded size (`status.currentResources`,
  or `status.placement.pendingResources` while its create is pending). While no memory ceiling
  is recorded for it, it counts at the memory hot-add ceiling of that size. When nothing records
  its size (a VM bound by a build that did not record it), capacity admissions on its Provider
  are refused and retried until the VMClass or the grant is restored. The manager logs the VM's name.

## Operations

- **Configurable client rate limits.** The manager's client-side Kubernetes API rate limits are
  set by `--client-qps` (requests per second; default 20) and `--client-burst` (default 40);
  in the chart, `manager.clientQPS` and `manager.clientBurst`. Zero, negative and non-numeric
  values fail the chart render, and the manager refuses to start with them. Each of the
  manager's REST clients (one per resource type) has its own bucket with these limits. They are
  client-side limits, not API-server limits. The effective values are logged once at startup.
  They take effect at a manager restart.

  Measured once, in the Spike 1B environment (1,000 Hosts, 10,000 VMs, manager limits 500m CPU
  and 512 MiB, a 100-VM burst): at QPS 100 / burst 40 instead of 20 / 40, `Ready` p50 fell from
  20.52 s to 7.86 s and throughput rose from 4.3 to 12.2 VMs/s (2.8×, not 5×). Manager CPU rose
  from 0.19 to 0.34 cores, with 36% of periods throttled. Placement-lock waits appeared (p50
  177 ms), as did 6 retried conflicts; there were no API server 429s, flow-control rejections or
  5xx. This is not a recommendation for production clusters; see
  [Kubernetes API client rate limits](../../charts/virtrigaud/README.md#kubernetes-api-client-rate-limits)
  for what raising them costs.

## Compatibility

- **CRD:** `VirtualMachine` gains `status.placement.resizeReservation` (additive, optional). No
  other schema change. Existing objects need no migration.
- **Upgrade order:** CRDs, then the manager, then the providers, as before. With an older CRD the
  new manager fails readiness, naming the missing field.
- **Chart:** the manager Deployment now passes `--client-qps=20 --client-burst=40` by default (one
  manager rollout, same limits). `helm upgrade --reuse-values` from an older values file renders
  without the flags; the manager's defaults apply.
- **Manager image:** the manager is now built from the `cmd/manager` package, not a single file.
  This only matters if you build the image yourself from `build/Dockerfile.manager`.
- **gRPC contract and providers:** unchanged. No provider rollout is required.

## Known limitations

- **No provider-side fencing.** A paused previous leader that resumes can still act on an
  answer it receives before it notices it lost its lease. The inherited-admission check stops the
  new leader from executing an admission that no longer fits, but it does not stop the old one.
  Restarting a provider pod does not cancel an operation it accepted. Releasing a held admission
  is an administrator decision that requires positive proof
  ([recovery procedure](../clustered-provider-inventory.md#admissions-inherited-from-an-earlier-manager)).
- **Leader-transition window.** A `pendingHost` write the old leader sent but never saw answered
  can be stored after the new leader's cache synced. Until the new leader's cache shows it, the
  host can be overbooked. That admission is not executed if it no longer fits.
- **Future asynchronous clustered providers** (a Cloud Hypervisor provider included) must give
  each `Reconfigure` a recoverable operation identity and accept a repeated request
  idempotently before they are admitted for clustered resizes. The libvirt provider's
  `Reconfigure` is synchronous and declarative.
- **A reservation can outlive its resize.** A resize reservation is released only on a confirmed
  `Reconfigure`. If a resize is refused before it is sent, and the spec is then set back to the
  recorded size, the reservation stays until the next resize the provider confirms
  (over-reserving only).
- **Placement is still serialized per Provider.** With the client rate limit raised, the
  per-Provider placement step, slowed by CPU throttling, was the next limit measured.
- **The client rate limits apply to every resource type alike** and change only at a restart.

## Upgrade

Read **[Upgrading from v0.4.1 to the next release](../upgrading.md#upgrading-from-v041-to-the-next-release)**
before upgrading. It covers CRD ordering, the new manager's re-check of inherited admissions,
the client rate limits, and **rolling back**. A `helm rollback` can succeed and still be unsafe
while any clustered VM holds `status.placement.resizeReservation`: the v0.4.1 manager drops the
field and counts the VM at its recorded size. Roll the manager and the chart back together; a
v0.4.1 manager does not start with the new flags.

## Changelog

The complete, dated changelog is in [`CHANGELOG.md`](../../CHANGELOG.md).
