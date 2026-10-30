<!--
Release notes for VirtRigaud v0.4.2; title the release "v0.4.2" and, for a release candidate,
"v0.4.2-rc<N>" (mark it as a pre-release). v0.4.2 is a patch release of v0.4.1. The notes are
cumulative since v0.4.1 and are updated for each candidate; this file starts with rc1. The
v0.4.1 notes are kept in v0.4.1.md. See docs/upgrading.md ("Upgrading from v0.4.1 to v0.4.2")
for the upgrade guide these notes link to, and CHANGELOG.md for the complete, dated changelog.
GitHub's limit for a release body is 125,000 characters; this file is far below it, so it is
pasted whole (with relative links rewritten to absolute links at the release tag).
-->

This is **v0.4.2**, a patch release of v0.4.1. It makes the manager scale to large clustered
Providers, closes the gaps in how a clustered Provider's capacity is reserved, and updates
`golang.org/x/net` for two security advisories. It changes no API field's meaning and no gRPC
message. Although it is a patch release, it **adds one optional `VirtualMachine` status field
and two manager flags**, and **rolling back to v0.4.1 is unsupported while a clustered VM holds a
resize reservation**: read [Upgrade](#upgrade) first.

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
- **Security:** `golang.org/x/net` v0.62.0 in every VirtRigaud Go module and binary (CVE-2026-78661,
  CVE-2026-97045). Roll the providers as well as the manager to pick it up.
- **Compatibility:** install the CRDs before the manager (the chart's hook does); **do not roll
  back while a clustered VM holds a resize reservation**, and roll the manager and the chart
  back together. See [Upgrade](#upgrade).

## Performance

Two kinds of measurement back this section, and they are not production measurements:

- **End-to-end (Spike 1B):** a disposable kind cluster with a **fake** clustered provider
  (1,000 Hosts and 10,000 VMs at most; method, environment and raw results in
  [`docs/performance/spike-1b-control-plane-baseline.md`](../performance/spike-1b-control-plane-baseline.md)).
  It measures the control plane only: the fake provider answers at once, so these figures say
  nothing about how fast a real hypervisor creates, changes or starts VMs.
- **Microbenchmarks (Spike 1A):** Go benchmarks of single functions on one workstation
  ([`docs/performance/spike-1a-scheduler-baseline.md`](../performance/spike-1a-scheduler-baseline.md)).

Your numbers depend on your API server, hypervisors and manager resources.

- **Steady-state polling (F1/F2, end-to-end).** Every running VM was reconciled every 10 seconds instead of
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
- **Full scans (F4/F5, microbenchmarks).** `Provider.status.connectedVMs` is counted through a field index
  instead of a deep copy of every `VirtualMachine` in the cluster, and the Provider controller no
  longer reconciles on every Host heartbeat (a 60-second resync replaces that trigger for
  clustered Providers). A Host's committed-capacity gauges and its in-use check read the VMs
  placed on that host from a per-host index. In benchmarks at 100,000 VMs on one Provider, the
  count fell from 245 ms to 77 ms per call and the per-heartbeat read from 116 ms to 46 µs.
- **Post-create readiness (F7, end-to-end).** A new VM is described as soon as its `Create` is recorded,
  instead of after a fixed 5 seconds. With 1,000 Hosts and 10,000 VMs, a 10-VM burst reaches
  `Ready` at p50 1.3 s instead of 6.3 s. In a 100-VM burst the median stays at 20.5 s, because
  VMs queue behind the Provider's serialized placement decisions; the p99 falls from 24.1–24.7 s
  to 22.9 s.
- **Placement decisions (C′, microbenchmarks).** A decision no longer copies every `VirtualMachine`; it reads the
  informer's index directly and sums capacity per host. In benchmarks, a decision at 100 hosts
  and 10,000 VMs takes 7.9 ms instead of 18.4 ms and allocates 435 KB instead of 16.9 MB, and the
  Provider's lock is held for a p50 of 7.7 ms instead of 20.4 ms in a contention benchmark. The
  decisions are identical (equivalence tests). A Provider's decisions are still serialized by
  its lock.

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
  held with `Reconfiguring=False/PlacementError` instead of being sent unreserved. The same write
  sets `Reconfiguring=True/ResizeReserved`, so a success left by an earlier resize can never
  release the new reservation. The reservation counts until the provider has confirmed a
  `Reconfigure` sent under it: `Reconfiguring=False` with reason `ReconcileSuccess`, and a
  `Describe` that reports the VM's vCPUs and memory maximum. Any other outcome keeps it: failed,
  unanswered or never recorded, pending a restart, asynchronous, refused before it was sent, or
  a reason the manager does not recognise.
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
  manager rollout, same limits). Upgrade with `helm upgrade --reset-then-reuse-values`, not
  `--reuse-values`: `--reuse-values` keeps the v0.4.1 chart's image tags, so the manager is not
  upgraded while the new CRDs are. With `--reuse-values` and the image tags set, the manager
  starts without the flags and applies its defaults. Both options keep image tags you set
  yourself: check `helm get values` and override any pinned v0.4.1 tag.
- **Manager image:** the manager is now built from the `cmd/manager` package, not a single file.
  This only matters if you build the image yourself from `build/Dockerfile.manager`.
- **gRPC contract and providers:** unchanged; providers need no configuration change. Their
  images are rebuilt with the `golang.org/x/net` update (see [Security](#security)), so roll them
  after the manager to pick it up.
- **ImagePrepare from an older manager is still served** (ADR-0009 D7). An identity-less request
  from a manager older than v0.4.0 is served in deprecated legacy mode, logged and counted in
  `virtrigaud_provider_image_prepare_legacy_requests_total`, as in v0.4.0 and v0.4.1. The
  refusal that the v0.4.1 documentation announced for "the release after v0.4.1" is deferred to
  a later release, not chosen yet. The `WARN` line now ends `refused in a later release`
  (it said `refused from the next release`); the counter is unchanged.

## Security

- **`golang.org/x/net` v0.62.0** in the root, `sdk`, `proto` and `test/proxmox-grpc-test` Go
  modules (it was v0.60.0; the test module v0.48.0). It fixes **CVE-2026-78661**
  (GO-2026-6860) and **CVE-2026-97045** (GO-2026-6866). govulncheck reported both against the
  dependency only: no vulnerable function was reachable from VirtRigaud code. The update also
  moves `golang.org/x/crypto` to v0.58.0, `x/sys` v0.49.0, `x/term` v0.47.0, `x/text` v0.43.0,
  `x/sync` v0.24.0 and `x/tools` v0.51.0, which the update requires; nothing else changed.
- **Not every advisory is resolved.** Two known findings remain, neither of them reachable from
  VirtRigaud code according to govulncheck:
  - GO-2026-6094 (GHSA-gcjh-h69q-9w9g) in `github.com/google/cel-go` v0.22.0: the vulnerable
    package is imported (through `k8s.io/apiserver`), but the affected functions are not
    called. The fixed version (v0.30.0) is newer than the one the Kubernetes and
    controller-runtime libraries in use require; it is not part of this release.
  - GO-2026-5932, the deprecated `golang.org/x/crypto/openpgp` package: no VirtRigaud build
    imports it, and there is no fixed version.
- **Container images.** The manager image has no HIGH or CRITICAL finding. The provider images
  are based on Debian 12.15 and carry HIGH and CRITICAL operating-system package advisories (83
  distinct CVEs, 4 of them CRITICAL: in GLib, libxml2, zlib's MiniZip and the Proxmox image's
  OpenSSH client) for which Debian 12 has no fixed package: they are marked affected, fix
  deferred or will not fix. The release scan ignores unfixed findings, so these do not fail it;
  that is not a statement that they are harmless. Most are in packages the providers do not use
  at run time (for example QEMU device emulation in the vSphere image, which uses only
  `qemu-img`). The chart's CRD-hook image (`kubectl`) ships the upstream `kubectl` 1.35.9 binary,
  built with Go 1.26.8, `golang.org/x/net` v0.56.0 and `x/text` v0.39.0; Trivy reports six HIGH
  advisories against it. It runs only during `helm install` and `helm upgrade`, and talks only to
  the cluster's API server.

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
- **Future asynchronous clustered providers** must give
  each `Reconfigure` a recoverable operation identity and accept a repeated request
  idempotently before they are admitted for clustered resizes. The libvirt provider's
  `Reconfigure` is synchronous and declarative.
- **A withdrawn resize can keep its reservation indefinitely.** Setting the spec back to the
  recorded size sends nothing, so a reservation whose last `Reconfiguring` reason is
  `ValidationError`, `InsufficientHostCapacity`, `InheritedAdmissionOverCapacity` or
  `ResizeReserved` (an outcome a stopped manager never recorded) is not released. No timer
  releases it, because expiring it could free room a `Reconfigure` sent earlier may still use. It
  stays, and keeps counting, until the next resize the provider confirms or the VM is deleted:
  this over-reserves capacity, it never overbooks a host. Do not remove it by hand without
  proof that no earlier `Reconfigure` can still execute
  ([details](../clustered-provider-inventory.md#a-withdrawn-resize-can-keep-its-reservation-indefinitely)).
  A safe recovery mechanism is tracked in
  [#468](https://github.com/projectbeskar/virtrigaud/issues/468).
- **Rolling back to v0.4.1 is unsupported while a clustered VM holds a resize reservation**, and
  the manager and the chart must be rolled back together. See
  [Rolling back to v0.4.1](../upgrading.md#rolling-back-to-v041).
- **Clustered libvirt Providers (`topology: cluster`) remain experimental**, as in v0.4.0 and
  v0.4.1 ([`docs/clustered-provider-inventory.md`](../clustered-provider-inventory.md)). The
  admission and accounting changes in this release apply to them only.
- **Placement is still serialized per Provider.** With the client rate limit raised, the
  per-Provider placement step, slowed by CPU throttling, was the next limit measured.
- **The client rate limits apply to every resource type alike** and change only at a restart.

## Upgrade

Read **[Upgrading from v0.4.1 to v0.4.2](../upgrading.md#upgrading-from-v041-to-v042)**
before upgrading. It covers CRD ordering, the new manager's re-check of inherited admissions,
the client rate limits, and **rolling back**. A `helm rollback` can succeed and still be unsafe
while any clustered VM holds `status.placement.resizeReservation`: the v0.4.1 manager drops the
field and counts the VM at its recorded size. Roll the manager and the chart back together; a
v0.4.1 manager does not start with the new flags. Roll the providers too, by setting
`spec.runtime.image` on each `Provider` (Helm does not change it): they carry the
`golang.org/x/net` update. Then check the images that actually run, as the upgrade guide shows.

## Changelog

The complete, dated changelog is in [`CHANGELOG.md`](../../CHANGELOG.md).
