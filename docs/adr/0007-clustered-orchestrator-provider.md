# ADR-0007: Clustered orchestrator provider — VirtRigaud as the cluster manager for hypervisors without a native one

## Status

**Accepted (2026-07-13; blocking decisions resolved 2026-09-21; accepted 2026-09-22).**
The five cross-ADR blocking decisions are settled (see `0007-0008-blocking-decisions.md`
and the folded D-sections below).

**Implementation status (2026-09-24):** P1's inventory, placement and admission
halves are merged (#312–#325; security-hardened by #330, #331, #333 and #334): `Host`/`HostPool`,
`ListHosts`/`GetHostInfo` + inventory sync, the filter+score scheduler, and
`target_host_id` + `status.placement.host` binding **at create**. **Not yet done:**
routing every *post-create* per-VM RPC to the bound host. Today only `Create` is
host-aware, so a clustered VM can be created but not described, powered, reconfigured,
snapshotted or deleted. The contract for that is **Addendum A** below; until it lands,
`topology: cluster` must be treated as experimental. This ADR proposes a new *class* of provider — a **clustered /
orchestrator** provider — that makes VirtRigaud itself the cluster manager for
hypervisors that lack a native one: register N individual bare hosts, present
them as one cluster, and do placement + cross-node migration. The first target
is **libvirt/KVM multi-node**; the second is **cloud-hypervisor multi-node**. It
is **explicitly not** an attempt to orchestrate raw ESXi or raw Proxmox hosts
(see *Non-goals*).

Every CRD and proto change below is **additive and v1beta1-safe** (new optional
fields, new messages, new RPCs — no renames, no removals, no proto major bump).
Every new host-level capability is **capability-gated and honesty-first**: a
provider never advertises `live_migration` / `block_migration` / `shared_storage`
it cannot actually perform, and an unsupported `storage_mode` is **rejected**,
never silently downgraded or faked.

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Hypervisors in scope**: libvirt/KVM (P1–P3), cloud-hypervisor (P4).
**Out of scope**: vSphere, Proxmox VE — they already have vCenter / pve-cluster
and VirtRigaud stays a thin client of them.

**Related**:
- [ADR-0001](./0001-transport-grpc-and-capi-integration.md) — gRPC is the
  manager↔provider transport; `ListHosts` / `MigrateVM` ride it unchanged.
- [ADR-0003](./0003-mtls-and-provider-grpc-auth.md) — mTLS protects the
  manager↔provider channel that now also carries host inventory + migration
  control.
- [ADR-0004](./0004-libvirt-ssh-host-key-verification.md) — SSH host-key /
  `known_hosts` verification; the N-host executor reuses it **per host**, and a
  future cloud-hypervisor host-agent should inherit it.
- [ADR-0005](./0005-image-preparation-trigger-model.md) — the capability
  double-gate (`contracts.X` type-assert + `Status.ReportedCapabilities` flag)
  is reused here for `supports_clustering` / `live_migration` / etc.
- [ADR-0006](./0006-storage-backend-agnostic-cross-hypervisor-migration.md) —
  the cold export→stage→import pipeline is reused as the **intra-cluster cold
  fallback** (`storage_mode=stage_copy`); for intra-cluster *same-hypervisor*
  migration it degenerates to a straight qcow2→qcow2 copy (no format conversion).
- [ADR-0008](./0008-libvirt-pure-go-driver-and-ssh-transport.md) — **added
  2026-07-20.** The pure-Go libvirt driver + in-process SSH transport. Its
  **PR 2–4 (connection seam, in-process SSH, connection lifecycle) are P1
  prerequisites for this ADR** — see *Phasing*. It also corrects this ADR's
  `execSem` claim and fixes the migration mode for v1 (see the amendments below).

**Prior art** (this is a solved shape; we are following it deliberately):
oVirt's **Engine / VDSM** split and OpenStack's **nova-scheduler / nova-compute**
split — a central brain that owns inventory + scheduling + state, and stateless
per-host executors that only do what they are told and report what they see.

---

## Context

### The current model, traced through the live code

Today a `Provider` CR is **one gRPC process bound to one backend**, and the two
kinds of provider we run map onto that very differently:

1. **vSphere and Proxmox are thin clients of a fat external orchestrator.** The
   provider process talks to vCenter / the pve-cluster API, which already owns
   host inventory, scheduling (DRS / the PVE HA manager), and cross-node
   migration. VirtRigaud does not schedule or migrate — it asks the orchestrator
   to. The cluster brain lives *outside* VirtRigaud.

2. **libvirt is one process bound to one host.** The provider is keyed on a
   single `PROVIDER_ENDPOINT` (`qemu+ssh://user@host/system`):
   `internal/providers/libvirt/provider.go:56` (the lone `Endpoint` field),
   `:96` (`os.Getenv("PROVIDER_ENDPOINT")`), `:165`
   (`Endpoint: provider.Spec.Endpoint`). `virsh` is run in-pod over that SSH
   transport, and `runRemoteVirshCommand` (the `"!"` prefix) execs `virsh` **on**
   the host over the same SSH channel (`internal/providers/libvirt/virsh.go:347`,
   `:368`). There is **no host inventory and no scheduler** — there is exactly
   one host, so there is nothing to schedule.

The **gRPC `Provider` service is VM-centric and effectively stateless**
(`proto/provider/v1/provider.proto`): `Create` / `Delete` / `Power` /
`Reconfigure` / `Clone` / `ListVMs` / `Describe`, async via `TaskRef` +
`TaskStatus`. `CreateRequest.placement_json` (`provider.proto:40`) and
`CloneRequest.placement_json` (`:133`) exist but carry **no host selector** — a
`Placement` today is cluster/datastore/folder/resource-pool hints consumed by an
*external* orchestrator, never a "run this VM on host X" instruction to a
VirtRigaud-owned scheduler. **There is no host-inventory RPC and no migrate RPC.**

Two more facts shape the design:

- **`VMPlacementPolicy` already models the scheduling vocabulary but has nowhere
  to act.** `api/.../vmplacementpolicy_types.go` carries hard/soft
  `PlacementConstraints` (hosts, node-selector, networks, datastores),
  `HostAffinity` / `HostAntiAffinity`, `VMAffinity` / `VMAntiAffinity`,
  `ResourceConstraints` (min CPU/mem per host, required features), and
  `SecurityConstraints`. Its status is **validation-only** (`ValidationResults`,
  `PlacementStats`) — because on a single-host libvirt provider there is no set
  of candidate hosts to filter and score. This CRD is the scheduler's input
  language, waiting for a scheduler.

- **The host-fork budget is already a live constraint.** `virsh` fan-out was
  recently capped (commit `c8fd36c`): `internal/providers/libvirt/virsh.go:45`
  (`defaultMaxConcurrentVirsh = 4`), acquired in `runVirshCommandOnce` (`:506`).

  > **Amended 2026-07-20 (ADR-0008).** This bullet originally described the cap
  > as *"a single per-process semaphore"* needing a redesign into a **per-host**
  > budget. **That was wrong.** `execSem` is already a **per-`VirshProvider`
  > field** (`virsh.go:69`), allocated per instance in `NewVirshProvider`
  > (`:96`). It is "global" only because exactly **one instance exists today** —
  > so N connections ⇒ N semaphores, nearly free, **no redesign needed**.
  >
  > The *real* concurrency risks are different and both are live today:
  > **(a) seven call sites bypass the semaphore entirely** — `s3import.go:248,258`,
  > `s3export.go:230,237`, `server.go:902,909,913`, i.e. the disk-streaming and
  > `scp` paths, the ones most likely to run concurrently during a migration (a
  > latent instance of the #288 fork exhaustion `c8fd36c` was meant to close);
  > and **(b) long-lived streaming transfers share the same 4 slots as short
  > control calls**, so a 40 GB export can starve inventory probes. The fix is a
  > **separate budget for streaming vs control**, not a per-host split.
  > ADR-0008 **PR 1** closes both.

### Scope in one paragraph, and explicit non-goals

**In scope**: hypervisors with **no native cluster manager** — libvirt/KVM
first, cloud-hypervisor second — where VirtRigaud registers N bare hosts, gives
them one logical cluster, places VMs on hosts, and migrates VMs between hosts.
**Explicitly NOT in scope**: orchestrating raw **ESXi** or raw **Proxmox** nodes
into a VirtRigaud-managed cluster. Those already have vCenter / pve-cluster +
corosync, with mature DRS/HA/fencing that VirtRigaud will not and should not
reimplement. VirtRigaud stays a **thin client** of them. "A vCenter-like control
plane for ESXi/Proxmox hosts" is a conceivable *future*, but it is out of scope
here and would need its own ADR. This boundary is the whole reason the design is
tractable: the operator brain is hypervisor-agnostic, and we only add executors
for hypervisors that bring *nothing* to conflict with.

---

## Decision

### D1 — Brain in the operator (CRDs), hands in the provider

**All decisions and all durable state live in the operator and CRDs; etcd is the
source of truth. The provider is a stateless executor.** Host inventory,
scheduling, the VM→host binding, the migration state machine, and health
tracking are owned by controllers and persisted in CRD status. The clustered
provider only ever: (a) executes `Create` / `Power` / `Delete` / `Migrate` on a
**named** host, and (b) reports each host's live capacity/health when asked
(`ListHosts` / `GetHostInfo`). This is the oVirt **Engine/VDSM** and OpenStack
**nova-scheduler/nova-compute** split, and it is *why* the design generalizes:
the brain is hypervisor-agnostic; only the executor RPCs differ per hypervisor.

**Explicit guardrail — the provider pod must NOT become "vCenter-in-a-pod".** It
holds no hidden inventory database, no scheduler, no persisted VM→host map, no
migration state. It holds only *connection material* for its configured hosts
(N keyed SSH/agent connections) — configuration, not authority. On operator
restart the operator rebuilds its world from the `Host` CRs (etcd) plus a fresh
`ListHosts` / `ListVMs` sync; on provider restart the provider re-reads its
connection config and reconnects. If the provider crashes, no decision or binding
is lost, because none of it lived there.

*Rationale*: it keeps the "remote-provider model is sacrosanct" invariant intact
— a provider failure cannot cascade into lost cluster state — and it keeps the
provider reviewable/hardenable as a narrow executor rather than a stateful
control plane.

### D2 — Scope / non-goals are a load-bearing constraint, not a caveat

**The clustered provider targets only hypervisors with no native cluster
manager.** libvirt/KVM first; cloud-hypervisor second. It is **not** a path to
cluster raw ESXi or raw Proxmox nodes — see *Scope and non-goals* above. This is
a decision, not a limitation to be "fixed later": pointing VirtRigaud's scheduler
at hosts that already answer to vCenter/pve would create two brains fighting over
the same hosts. The `topology: cluster` discriminator (D9) is therefore only
meaningful for `type: libvirt` (and later `cloudhypervisor`); a validating
webhook rejects `topology: cluster` on `type: vsphere|proxmox`.

### D3 — New CRDs: `Host` and `HostPool`; the binding lives in `VirtualMachine.status`

**Model the cluster with two new namespaced CRDs plus a status binding on the
VM.** All additive, v1beta1-safe.

- **`Host`** (a.k.a. HypervisorHost, shortName `hvh`) — one bare host.
  `spec` is **admin-authored desired inventory**: which clustered `Provider`
  owns it, which `HostPool` it joins, its connection info (SSH endpoint / future
  agent addr), an optional per-host credential ref, and placement labels
  (storage-pool + network visibility, zone/rack). `status` is **operator-synced
  observed state** from `ListHosts`/`GetHostInfo`: allocatable cpu/mem/storage,
  health enum, CPU model + features, machine types, emulator version, last
  heartbeat. This is the clean desired/observed split — the admin declares *what
  hosts exist*; the provider reports *what they can do right now*.
- **`HostPool`** (a.k.a. Cluster, shortName `hp`) — a named group of `Host`s
  under one clustered provider, carrying **cluster policy**: scheduling strategy
  (spread/binpack), CPU/mem overcommit ratios, storage-pool references, network
  references, and the default migration policy (live vs cold, storage mode, TLS,
  bandwidth/downtime caps). `status` aggregates host counts + readiness.
- **`VirtualMachine.status.placement.host`** — the scheduler's binding, and the
  **durable source of truth** for where a VM runs. Written by the operator
  **only after the provider confirms** the VM is on that host (create success,
  or migration `TaskStatus.done`); never speculatively (honesty-first).

*Membership direction*: `Host.spec.poolRef → HostPool` (each host declares its
pool). *Inventory ownership*: the operator owns the `Host` list; the provider is
**told** which hosts to operate on via a **single projected Secret** — settled
2026-09-21 (blocking decision; resolves Open question 2). The Provider controller
renders the pool's `Host` CRs into one **versioned-schema Secret** mounted into the
provider pod; the provider **watches the file and hot-reloads** it — **lazy-open on
host-add, graceful-drain on host-remove**, so a reload never severs an in-flight
operation. A **Secret** (not a ConfigMap) keeps `known_hosts`/connection material
out of namespace-readable config and stays uniform with today's credential flow;
reading it from a mounted file — never the Kubernetes API — preserves the
**no-API-access invariant** hardened in **#297** (dedicated no-RBAC ServiceAccount,
no automounted token). `ListHosts` then reports
live status for exactly the hosts the provider is configured for — the provider
enumerates *connections it holds*, not an inventory *it decides*.

### D4 — Placement / scheduling is a small, testable, re-runnable function in the operator

**Add a filter+score scheduler in the operator; record its decision in
`VM.status`; make it a pure function.** Filter candidate hosts by fit
(allocatable cpu/mem ≥ request) and by the **hard constraints** already
expressible in `VMPlacementPolicy` (node-selector, host affinity/anti-affinity,
required features) plus D6's storage/network visibility; score the survivors by
strategy (spread or binpack). **Reuse `VMPlacementPolicy`** as the input
language — it already has the vocabulary (D-context) and today has nowhere to
act; this ADR gives it candidate hosts at last. Start deliberately dumb;
pluggable / DRS-like rebalancing is later.

Because the operator owns the decision, placement becomes an **explicit
`target_host_id` on the wire** — not buried in `placement_json`. `placement_json`
stays for external-orchestrator hints (vSphere/Proxmox); `target_host_id` is the
clustered path's instruction. The scheduler is a pure `schedule(vm, []HostInfo,
policy) → host_id` function so it is unit-testable without a cluster and
idempotent on re-run (a VM already bound re-selects its current host unless
drained).

### D5 — Two distinct migration classes, kept as separate workflows

**(a) Intra-cluster (host→host, same hypervisor)** — ideally **LIVE** (libvirt
native live migration; CH live migration). **New.** **(b) Cross-hypervisor** —
**COLD** export→stage→import, already shipped in ADR-0006. Do not conflate them:
their state machines are unrelated (live migration has no export/convert/import
phases; cold has no RAM iteration/switchover).

A **`storage_mode` enum** selects the intra-cluster data path:
`shared | block_all | block_inc | stage_copy`.
- `shared` — both hosts see the same disk at the same path; transfer RAM +
  device state only (fast, low-risk). The clean case.
- `block_all` / `block_inc` — libvirt live block migration (`--copy-storage-all`
  / `--copy-storage-inc`) when the disk is host-local and the VM must stay up.
- `stage_copy` — **reuse ADR-0006's staging** for tolerate-downtime local-disk
  moves. Intra-cluster same-hypervisor means qcow2→qcow2 with **no format
  conversion**, so it degenerates to a cheap straight copy of the already-built,
  already-validated pipeline.

*Rationale*: one enum lets the operator express the full spectrum (shared-storage
live down to cold copy) while capabilities (D7) constrain what each hypervisor
actually honors — CH gets `shared | stage_copy` only; `block_*` is rejected, not
faked.

**Trigger CRD — resolved 2026-09-21 (blocking decision; resolves Open question 3;
overrides the draft's discriminator lean).** Live intra-cluster migration is
triggered by a **new `VMHostMigration` kind — not** a `spec.class` discriminator on
ADR-0006's `VMMigration`. The two share only a noun: cold cross-hypervisor
export/stage/import (ADR-0006) and live host→host RAM transfer are different domains
with **unrelated state machines** (as this decision already argues above). A
v1beta1 union-type `VMMigration` would age badly and be automatic-HA's (P5) awkward
base, whereas **separate kinds give independent concurrency, RBAC, and rollback** —
which high-volume host-drain evacuations need. The extra-CRD cost is mitigated
deliberately: `VMHostMigration` **shares the internal migration primitives and the
condition vocabulary, not the CRD schema** — the same phase/condition constants and
shared libraries, over its own live-migration state machine (no
export/convert/import phases). This is a CRD-shape decision only; the `MigrateVM`
RPC (below) is unchanged.

### D6 — Storage & networking are modeled now as scheduling constraints, implemented later

**VirtRigaud consumes pre-set-up shared storage and networking; it does not
deploy them.** NFS is the v1 default (low setup, RAM-only live migration, best
starting point); Ceph RBD is the serious-deployment option (strong HA, no SPOF)
but too heavy to *require* in v1. Model storage as **named pools with per-host
visibility** and networks (bridge on a shared VLAN / overlay) as **per-host
labels**. Both are **hard placement constraints**: only schedule or migrate a VM
to a host that can see the VM's storage pool **and** has the VM's network.

Put the CRD slots in now (pool refs on `HostPool`, visibility labels on `Host`)
so we do not corner ourselves; implementation of the constraint checks lands with
migration (P2+). *Rationale*: the nastiest silent failure in this space —
migrating to a host whose bridge name matches but whose VLAN is wrong, so the NIC
attaches and traffic blackholes — is a *modeling* problem first. Reserving the
constraint surface early is cheap; retrofitting it after VMs are stranded is not.

### D7 — Capability-gated, honesty-first

**Extend `GetCapabilities` with host-clustering flags and never advertise one
that isn't real.** New flags: `supports_clustering`, `live_migration`,
`block_migration`, `shared_storage`, `host_evacuation`. cloud-hypervisor live
migration is **experimental / opt-in** — it requires an identical CH build on
both hosts + shared storage + no VFIO/passthrough/CoCo, and has **no block
migration**, so cold staging is the only local-disk path. A provider that cannot
satisfy a requested `storage_mode` **rejects the `MigrateVM` at the door**
(`InvalidArgument`), exactly as ADR-0006's `EnsureRelayMode` rejects an
unsupportable `direct`. The double-gate from ADR-0005 applies: the manager acts
only when the provider both type-asserts the clustering contract **and**
advertises the flag on `Provider.status.reportedCapabilities`.

### D8 — Failure handling is report-only in v1; automatic HA + fencing is DEFERRED

**v1 detects and surfaces host-down and supports operator-initiated (opt-in)
evacuation — it does NOT do automatic failover.** This is a loud, deliberate
deferral, not an oversight. Auto-restarting a VM elsewhere on **shared storage**
without **fencing/STONITH** is a corruption generator: an unreachable-but-alive
host (network partition, not death) plus a restart-elsewhere gives two QEMU
processes writing one disk → instant split-brain corruption. Safe auto-HA
requires (i) reliable failure detection that can distinguish *dead* from
*partitioned*, and (ii) a fencing subsystem (IPMI/BMC/PDU power-off or storage
fencing) that guarantees the old writer is gone *before* the restart. Both are
large, hardware-specific subsystems VirtRigaud does not have. v1 therefore
reports `Host.status.health=NotReady`, raises a condition/event, and lets an
operator *choose* to evacuate; automatic HA is its own future ADR (P5).

### D9 — Coexistence & rollout: the clustered kind is additive, single-host is untouched

**Introduce a `spec.topology: single | cluster` discriminator on `Provider`
(default `single`); single-host libvirt is byte-for-byte unchanged.** A VM whose
`providerRef` points at a `topology: single` provider keeps today's direct path
(no scheduler, no `target_host_id`). A VM pointing at a `topology: cluster`
provider goes through the operator scheduler, gets a `status.placement.host`
binding, and is created with `target_host_id`. cloud-hypervisor additionally
needs a new `type: cloudhypervisor` enum value (additive enum extension) since it
is a new hypervisor family; `topology` then distinguishes single vs clustered CH
just as it does libvirt.

*Rationale*: a discriminator keeps the provider-type taxonomy from forking into
`libvirt` vs `libvirt-cluster` (which would duplicate every future libvirt
capability across two enum values) and makes the clustered behavior a property of
the *deployment topology*, which is what it actually is. **Settled 2026-09-21
(blocking decision; resolves Open question 1):** `topology` is a **string enum, not
a bool** — `single` today, `cluster` next — so a future third topology mode is a
purely additive enum value, never a breaking type change. The distinct
`libvirt-cluster`-provider-type alternative is **rejected** (it would fork every
hypervisor × topology combination into its own type). Default `single` keeps every
existing single-host provider byte-for-byte unchanged.

---

## CRD / API changes (additive, v1beta1-safe)

Go types in `api/infra.virtrigaud.io/v1beta1/` are the source of truth; all of
this regenerates via `make update-crds` — never hand-edit `config/crd/bases/` or
`charts/virtrigaud/crds/`.

> **Amendment (2026-09-24, security fix, pre-release):** the sketches below show
> `Host.spec.providerRef`, `Host.spec.credentialSecretRef` and
> `HostPool.spec.providerRef` as `ObjectRef` (with an optional namespace). As
> implemented they are **`LocalObjectReference`** — the *same-namespace model*:
> a clustered Provider, its Hosts, HostPools and their credential Secrets all
> live in the Provider's namespace, and `Host.spec.endpoint` carries an
> admission pattern (`qemu+ssh://[user@]host[:port]/system|session` or
> `grpc://host:port`). A cross-namespace reference let any namespace enrol a
> Host into another namespace's Provider (inheriting its SSH identity) and make
> the manager copy a Secret out of an arbitrary namespace; an unvalidated
> endpoint path reached the hypervisor host's shell. Both kinds were unreleased,
> so this is not a released-API break. See `docs/clustered-provider-inventory.md`
> (*Same-namespace model*, *Endpoint validation*).

### New kind: `Host`

```go
// HostSpec is admin-authored desired inventory: one bare hypervisor host.
type HostSpec struct {
    // ProviderRef is the clustered Provider that executes on this host.
    ProviderRef ObjectRef `json:"providerRef"`

    // PoolRef is the HostPool this host belongs to.
    PoolRef LocalObjectReference `json:"poolRef"`

    // Endpoint is the host connection URI (libvirt: qemu+ssh://user@host/system;
    // a future cloud-hypervisor host-agent: grpc://host:port).
    Endpoint string `json:"endpoint"`

    // CredentialSecretRef optionally overrides the Provider's credentials for
    // this host (SSH key / known_hosts material). Defaults to the Provider's.
    // +optional
    CredentialSecretRef *ObjectRef `json:"credentialSecretRef,omitempty"`

    // Labels are placement facts: storage-pool visibility, network/bridge
    // visibility, zone/rack. Consumed as hard constraints by the scheduler (D6).
    // e.g. {"storage.virtrigaud.io/pool-nfs01":"true","net.virtrigaud.io/br-vlan100":"true","topology.virtrigaud.io/rack":"r7"}
    // +optional
    Labels map[string]string `json:"labels,omitempty"`

    // Schedulable, when false, cordons the host: no new placement, existing VMs
    // stay. Set true→false to drain-then-evacuate (D8).
    // +optional
    // +kubebuilder:default=true
    Schedulable bool `json:"schedulable"` // NO omitempty (defaulted-bool footgun, ADR-0006/PR#235)
}

// HostStatus is operator-synced observed state from ListHosts/GetHostInfo.
type HostStatus struct {
    // Health is the live host health (D-context probe: libvirtd reachable |
    // agent reachable + CH runnable).
    // +optional
    Health HostHealth `json:"health,omitempty"` // Ready|NotReady|Unknown

    // AllocatableCPU / AllocatableMemoryMiB / AllocatableStorageBytes are the
    // schedulable capacity the provider reports live.
    // +optional
    AllocatableCPU        *int32 `json:"allocatableCPU,omitempty"`
    // +optional
    AllocatableMemoryMiB  *int64 `json:"allocatableMemoryMiB,omitempty"`
    // +optional
    AllocatableStorageBytes *int64 `json:"allocatableStorageBytes,omitempty"`

    // CPUModel / CPUFeatures drive the migration CPU-compatibility pre-check
    // (virsh cpu-baseline / cpu-compare). Per-hypervisor meaning (D-brief §6).
    // +optional
    CPUModel    string   `json:"cpuModel,omitempty"`
    // +optional
    CPUFeatures []string `json:"cpuFeatures,omitempty"`

    // MachineTypes / EmulatorVersion gate machine-type + emulator match for
    // live migration.
    // +optional
    MachineTypes    []string `json:"machineTypes,omitempty"`
    // +optional
    EmulatorVersion string   `json:"emulatorVersion,omitempty"`

    // BoundVMs is the count of VMs the operator has bound here (from VM status,
    // not from the provider — the operator owns the binding, D1).
    // +optional
    BoundVMs int32 `json:"boundVMs,omitempty"`

    // +optional
    LastHeartbeatTime  *metav1.Time       `json:"lastHeartbeatTime,omitempty"`
    // +optional
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    // +optional
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:validation:Enum=Ready;NotReady;Unknown
type HostHealth string
```

### New kind: `HostPool`

```go
type HostPoolSpec struct {
    // ProviderRef is the clustered Provider that owns this pool.
    ProviderRef ObjectRef `json:"providerRef"`

    // Strategy is the default scheduling strategy for VMs placed in this pool.
    // +optional
    // +kubebuilder:default=Spread
    // +kubebuilder:validation:Enum=Spread;BinPack
    Strategy string `json:"strategy,omitempty"`

    // Overcommit ratios applied when computing allocatable capacity.
    // +optional
    Overcommit *OvercommitRatios `json:"overcommit,omitempty"` // {cpu: "4.0", memory: "1.0"}

    // StoragePools are the named shared/local storage pools available in this
    // pool. Per-host visibility is asserted via Host.spec.labels (D6). Consumed,
    // not deployed, by VirtRigaud.
    // +optional
    StoragePools []StoragePoolRef `json:"storagePools,omitempty"`

    // Networks are the named networks (bridge/VLAN/overlay) available in this
    // pool; per-host presence via Host.spec.labels (D6).
    // +optional
    Networks []PoolNetworkRef `json:"networks,omitempty"`

    // Migration is the default migration policy for VMs in this pool.
    // +optional
    Migration *PoolMigrationPolicy `json:"migration,omitempty"`
}

type PoolMigrationPolicy struct {
    // +optional
    // +kubebuilder:default=true
    DefaultLive bool `json:"defaultLive"` // NO omitempty (defaulted-bool footgun)
    // +optional
    // +kubebuilder:default=shared
    // +kubebuilder:validation:Enum=shared;block_all;block_inc;stage_copy
    DefaultStorageMode string `json:"defaultStorageMode,omitempty"`
    // RequireTLS forces QEMU-native --tls on the migration data path (banking).
    // +optional
    // +kubebuilder:default=false
    RequireTLS bool `json:"requireTLS,omitempty"`
    // +optional
    BandwidthMbps  *int32 `json:"bandwidthMbps,omitempty"`
    // +optional
    MaxDowntimeMs  *int32 `json:"maxDowntimeMs,omitempty"`
}
```

### New kind: `VMHostMigration`

The trigger for live intra-cluster migration (D5), **settled 2026-09-21** as a
**separate kind** from ADR-0006's `VMMigration`: the two share migration
*primitives* and *condition vocabulary*, **never the CRD schema**. `VMMigration`
stays the cold cross-hypervisor export/stage/import kind; `VMHostMigration` is the
live host→host one, with its own concurrency/RBAC/rollback surface for host-drain
evacuations. Fields are illustrative; the exact shape lands at **P2**.

```go
// VMHostMigrationSpec triggers a live move of one VM between two Hosts of the same
// HostPool. Additive, v1beta1-safe.
type VMHostMigrationSpec struct {
    // VMRef is the VirtualMachine to move.
    VMRef LocalObjectReference `json:"vmRef"`

    // TargetHost is the destination Host. When empty (e.g. an operator-initiated
    // host drain), the scheduler (D4) selects it under the pool's constraints.
    // +optional
    TargetHost string `json:"targetHost,omitempty"`

    // Live requests a RAM-transfer live move; false tolerates downtime and may
    // fall back to stage_copy per the pool policy.
    // +optional
    // +kubebuilder:default=true
    Live bool `json:"live"` // NO omitempty (defaulted-bool footgun, ADR-0006/PR#235)

    // StorageMode selects the intra-cluster data path (D5); defaults from the
    // HostPool migration policy.
    // +optional
    // +kubebuilder:validation:Enum=shared;block_all;block_inc;stage_copy
    StorageMode string `json:"storageMode,omitempty"`
}

// VMHostMigrationStatus runs a live-migration state machine (no
// export/convert/import phases) but reuses the SHARED migration condition
// vocabulary — the same condition types/reasons as VMMigration (D5: share
// primitives, not schema).
type VMHostMigrationStatus struct {
    // +optional
    Phase string `json:"phase,omitempty"`
    // +optional
    SourceHost string `json:"sourceHost,omitempty"`
    // +optional
    TargetHost string `json:"targetHost,omitempty"`
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### Additive fields on existing kinds

```go
// ProviderSpec — the topology discriminator (D9). A string enum, not a bool, so a
// future third topology mode is a purely additive enum value (settled 2026-09-21).
// +optional
// +kubebuilder:default=single
// +kubebuilder:validation:Enum=single;cluster
Topology string `json:"topology,omitempty"`

// ProviderType enum gains one additive value for P4:
//   +kubebuilder:validation:Enum=vsphere;libvirt;firecracker;qemu;proxmox;cloudhypervisor

// VirtualMachineStatus — the durable binding (D3/D4). New optional sub-struct.
// +optional
Placement *PlacementStatus `json:"placement,omitempty"`

type PlacementStatus struct {
    // Host is the scheduler's binding — the host this VM runs on. Written only
    // after the provider confirms (create success / migration done). The durable
    // placement source of truth (D3).
    // +optional
    Host string `json:"host,omitempty"`
    // Pool is the HostPool the VM was scheduled into.
    // +optional
    Pool string `json:"pool,omitempty"`
    // +optional
    LastScheduledTime *metav1.Time `json:"lastScheduledTime,omitempty"`
    // Reason records why this host was chosen (scheduler decision trace).
    // +optional
    Reason string `json:"reason,omitempty"`
}

// ReportedCapabilities (api/.../provider_types.go) gains the clustering flags
// (D7), mirroring the proto GetCapabilitiesResponse additions below:
//   SupportsClustering, SupportsLiveMigration, SupportsBlockMigration,
//   SupportsSharedStorage, SupportsHostEvacuation bool
//   SupportedStorageModes []string  // "shared"|"block_all"|"block_inc"|"stage_copy"
```

A validating webhook enforces D2 (`topology: cluster` only on
`type: libvirt|cloudhypervisor`), that a `Host`/`HostPool` referencing a
`Provider` references a `topology: cluster` one, and that a `VMHostMigration`
targets a VM on a `topology: cluster` provider (a live host→host move is
meaningless on a single-host or thin-client provider).

---

## gRPC contract changes (`proto/provider/v1/provider.proto`, additive — no major bump)

New field numbers are illustrative; assign the next free number at implementation
(do not reuse). All additions are backward-compatible: existing single-host
providers ignore the new RPCs (return `Unimplemented`) and never receive
`target_host_id`.

```proto
// --- Host inventory (D1/D3) -------------------------------------------------
message ListHostsRequest {}
message ListHostsResponse { repeated HostInfo hosts = 1; }

message HostInfo {
  string id = 1;                       // operator-assigned host_id (== Host CR). AGNOSTIC.
  string address = 2;                  // SSH endpoint / agent addr. AGNOSTIC.
  int32  allocatable_cpu = 3;          // AGNOSTIC shape; PER-HV source (virsh nodeinfo | host OS/agent).
  int64  allocatable_mem_mib = 4;      // AGNOSTIC shape; PER-HV source.
  int64  allocatable_storage = 5;      // PER-HV (per-pool virsh pool-info | host-fs free).
  HostHealth health = 6;               // Ready|NotReady|Unknown. AGNOSTIC enum; PER-HV probe.
  map<string,string> labels = 7;       // storage-pool + network visibility, zone/rack. AGNOSTIC — constraints live here.
  string cpu_model = 8;                // PER-HV: baseline-model (libvirt) vs same-CPU-family (CH).
  repeated string cpu_features = 9;    // PER-HV.
  repeated string machine_types = 10;  // PER-HV (virsh domcapabilities | CH binary version).
  string emulator_version = 11;        // PER-HV.
}
enum HostHealth { HOST_HEALTH_UNSPECIFIED = 0; HOST_HEALTH_READY = 1; HOST_HEALTH_NOT_READY = 2; }

// Optional single-host refresh (cheaper than a full ListHosts poll).
message GetHostInfoRequest { string host_id = 1; }
// returns HostInfo

// --- Placement on the wire (D4) --------------------------------------------
// ADDITIVE fields on existing requests (not in placement_json — operator owns it):
message CreateRequest { /* ...existing 1..9... */ string target_host_id = 10; } // AGNOSTIC. The single most load-bearing new field.
message CloneRequest  { /* ...existing 1..6... */ string target_host_id = 7;  } // AGNOSTIC.

// --- Intra-cluster migration (D5) ------------------------------------------
message MigrateVMRequest {
  string vm_id = 1;                    // AGNOSTIC
  string src_host_id = 2;             // AGNOSTIC
  string dst_host_id = 3;             // AGNOSTIC
  bool   live = 4;                    // AGNOSTIC intent; PER-HV envelope (CH experimental)
  string storage_mode = 5;           // "shared"|"block_all"|"block_inc"|"stage_copy"; PER-HV support (CH: shared|stage_copy only)
  // Advisory tuning hints (libvirt maps to virsh migrate flags; CH ignores most):
  int32  bandwidth_mbps  = 6;
  int32  max_downtime_ms = 7;
  bool   auto_converge   = 8;         // libvirt --auto-converge
  bool   postcopy        = 9;         // libvirt --postcopy (BREAKS safe-abort — gated, see Security)
  bool   tls             = 10;        // QEMU-native --tls on the data path (banking)
}
// returns TaskResponse; progress/abort reuse the existing TaskStatus poll.
// libvirt: virsh migrate blocks; progress via `virsh domjobinfo`, abort via
// `virsh domjobabort`; full storage_mode set. CH: `ch-remote send-migration`,
// coarse status; block_* REJECTED via capabilities (not faked).

// --- Capabilities (D7) ------------------------------------------------------
message GetCapabilitiesResponse {
  /* ...existing 1..16... */
  bool supports_clustering   = 17;
  bool live_migration        = 18;
  bool block_migration       = 19;
  bool shared_storage        = 20;
  bool host_evacuation       = 21;
  repeated string supported_storage_modes = 22; // "shared"|"block_all"|"block_inc"|"stage_copy"
}

service Provider {
  /* ...existing RPCs... */
  rpc ListHosts(ListHostsRequest) returns (ListHostsResponse);
  rpc GetHostInfo(GetHostInfoRequest) returns (HostInfo);
  rpc MigrateVM(MigrateVMRequest) returns (TaskResponse);
}
```

**Agnostic vs per-hypervisor (from the virtualization brief)** — *agnostic*:
`id`, `address`, `allocatable_cpu/mem`, `health` enum, `labels`, `target_host_id`,
`MigrateVM(vm, src, dst, live)`, `TaskStatus` reuse. *Per-hypervisor
interpretation*: `cpu_model`/`cpu_features` + `machine_type` (baseline-model vs
same-family); `storage_mode` (libvirt full set vs CH `{shared, stage_copy}`);
`allocatable_storage` (per-pool vs host-fs); migration progress granularity (fine
vs coarse); health probe (daemon reachable vs synthesized).

---

## Addendum A (2026-09-24): post-create lifecycle routing contract

**Status: Accepted (2026-09-24).** This extends D1, D3, D4, D5, D8 and D9, and
changes none of them. It was reviewed against the code as of `main` 685cf6c.

**Problem.** Only `Create` carries a host (`target_host_id`, #322). Every later
per-VM call on a clustered provider went to a placeholder single-host handle with
an empty URI (`internal/providers/libvirt/provider.go`). The same was true of
`ImagePrepare`. So a clustered VM could be created on host A but never described,
powered, reconfigured, snapshotted or deleted. A force-delete also left its domain
and disks on A.

The create path had a second flaw. The binding is written only after `Create`
confirms, and nothing recorded which host an in-flight `Create` was aimed at. If
the status write was lost, or `Create` ran past its deadline after
`virsh define`, the retry could schedule the VM onto host B and create a
**second** domain with the same name.

### A1: the operator passes the host on every per-VM call

Under D1 (brain in the operator), only the operator knows where a VM runs, so the
provider **never looks a VM's host up by itself**. Doing so would mean polling every
host on each call, would be ambiguous when two hosts have a domain of the same
name, and would take the source of truth out of etcd.

**Wire change (additive, no proto major bump).** Add `string target_host_id` at
the next free field number to each per-VM request:

- `DeleteRequest`, `PowerRequest`, `ReconfigureRequest` and `HardwareUpgradeRequest`.
  `HardwareUpgrade` gets the proto field only: it has no `contracts` or transport
  method, and nothing calls it.
- `DescribeRequest`.
- `SnapshotCreateRequest`, `SnapshotDeleteRequest` and `SnapshotRevertRequest`.
- `ExportDiskRequest` and `GetDiskInfoRequest`.

Each field's comment carries the same rules as `CreateRequest.target_host_id`:
AGNOSTIC; ignored by (and never sent to) single-host and thin-client providers;
**required** by a clustered provider.

`CloneRequest` works differently. It gains `source_host_id` for routing. The
planned `target_host_id` keeps the meaning the main ADR gives it: the host where
the clone lands. In v1 a clustered provider requires the two to be equal, because
disks are host-local and a linked clone depends on its source disk. It returns
`InvalidArgument` if they differ.

**Calls that are not per-VM, or are host-scoped:**

- `TaskStatus` is not per-VM. A clustered provider's async task refs **must**
  include the host id, so that polling for `MigrateVM` progress (D5) can be routed.
- `ImagePrepare` is host-scoped. Until it gains `target_host_id` and records
  prepare status per host, a clustered provider reports
  `supports_image_import=false`.
- `ImportDisk` and `ListVMs` are not per-VM. `ImportDisk` gets a target host only
  when migration *into* a clustered provider is designed.
- Until P3, the VMMigration controller rejects a clustered **target** provider.

**Operator side:**

- Per-VM methods on the manager's `contracts.Provider` take a
  `contracts.VMRef{ID, HostID}`. The host is never passed as a context value, so the
  compiler finds every call site.
- One shared helper builds the `VMRef` from a VirtualMachine for the VM, snapshot,
  clone, migration and adoption controllers. For a Provider with
  `topology: cluster`, it fills `HostID` from `status.placement.host`.
- A clustered VM with no confirmed binding is never sent a per-VM call. The helper
  returns an unbound error, the controller sets `Placed=False/Unbound` and
  requeues, and it never lets the provider pick a host.
- The clone controller's `bindTargetVM` writes the target VM's `placement.host` in
  the same write as its `Status.ID`.
- Adoption keys discovered VMs on `(host_id, id)`, and fills `placement.host` from
  `VMInfo.host_id`.
- For single-host providers `HostID` is empty and behaviour is unchanged (D9).

**Provider side.** One helper, `withHostConn(ctx, hostID, fn)`, does the routing:

1. Lease the host's connection from `clusterReg.ConnFor(hostID)`.
2. Run the shared core with the leased `libvirtConn`, so the virsh read, the
   native read and the shadow read all use the same host.
3. Close the lease.

Each call's core is refactored to take that `libvirtConn`, as `createVM` was in
#322 but at the connection layer instead of `*VirshProvider`. That way the
ADR-0008 PR 5 native switch does not have to undo the routing.

- In clustered mode `p.virshProvider` becomes a handle that always fails. A test
  drives every routed call and proves none of them reaches it.
- An empty `target_host_id` on a clustered provider fails with `InvalidArgument`.
  It never defaults to a host (D9, honesty-first).
- An unknown or removed host returns a retryable `Unavailable`.
- A draining host's lease still completes an in-flight call; the graceful drain in
  D3 does not cut it off.
- The Host controller adds an in-use finalizer. A Host cannot be deleted while any
  VM names it in `placement.host` or `placement.pendingHost`.

### A2: record the attempted host before `Create`, separately from the binding

D3 stands: `status.placement.host` is the **confirmed** binding, written only
after the provider confirms. This addendum adds an **unconfirmed** field next to it:

- **`status.placement.pendingHost`** (additive, optional). The operator writes it,
  together with the pool, **before** calling `Create`.
- The write is a checked `Status().Update` with a resourceVersion precondition. It
  cannot go through the existing error-swallowing `updateStatus`. On a conflict the
  reconcile requeues, and it never reapplies its own scheduler choice.
- When `Create` succeeds, the operator moves `pendingHost` into `host` and clears
  `pendingHost`.
- On a retry, a non-empty `pendingHost` is reused as-is; the scheduler is not re-run
  for a VM with a create in flight.
- `pendingHost` is used for two things only: routing the `Create` retry, and
  finalizer cleanup.

A retry after a lost write lands on the same host, and the provider's ownership
check makes it safe. This depends on the owner-metadata fix for the released
domain-name takeover, which is a **prerequisite**:

- `Create` stamps the VM's owner identity (UID, namespace and name) into the libvirt
  domain's metadata.
- An existing domain of the same name whose owner UID matches is an idempotent
  success.
- A domain owned by anyone else, or with no metadata, gets a non-retryable
  `AlreadyExists` and is never bound.

**Deleting a VM while its create is pending.** The finalizer today calls `Delete`
only when `Status.ID` is set, so a domain already created on the pending host would
leak. For a VM with `pendingHost` set but no `Status.ID`, the finalizer sends an
**owner-checked** `Delete` to `pendingHost`. `DeleteRequest` gains the same additive
`owner` identity. A clustered `Delete` of a domain whose owner is different or
missing returns not-found and **never destroys it**. A cleanup that follows an
`AlreadyExists` can then never delete someone else's domain.

**An unreachable pending host** (`Create` keeps returning `Unavailable`) is never
re-scheduled automatically, because a domain may already exist there. The operator
sets `Placed=False/HostUnavailable` and waits for the host to return or for an
administrator to clear `pendingHost` (D8, report-only).

> **Amendment (2026-09-24, slice 2): a name conflict releases the pending host.**
> `AlreadyExists` on the pending host is the one `Create` failure that proves
> this VM has no domain there. The provider checks for a same-named domain it
> does not own *before* creating anything, so this attempt created nothing
> either. It does **not** prove that the host holds nothing of this VM's: an
> earlier attempt that failed part-way, before the domain was defined, may have
> left a `<name>-disk` volume or cloud-init files behind. The finalizer could
> never remove those either, because its owner-checked `Delete` acts only on a
> domain this VM owns and a clustered host is never cleaned up by name. So
> releasing the host loses no cleanup, and such leftovers stay for an
> administrator. Keeping `pendingHost` would pin the VM to that host until an
> administrator stepped in. So on `AlreadyExists` the operator:
>
> - clears `pendingHost` and adds the host to the additive
>   `status.placement.excludedHosts`, in one checked `Status().Update`. If the
>   write is lost, the retry goes to the same host and conflicts again, so
>   nothing is lost.
> - sets `Placed=False/HostExcluded` and re-schedules.
>
> The scheduler input (`Request.ExcludedHosts`) filters excluded hosts before
> any other check, even when the host is the current binding. The list holds
> at most 16 hosts, drops its oldest entry when full, and is cleared when the
> VM is bound. A conflict that has to drop an entry makes the next attempt wait
> 2 minutes, so a pool with more conflicting hosts than the cap cannot become a
> fast create loop. When every candidate is excluded, the operator sets
> `Placed=False/AllHostsExcluded` and sends no `Create`. It re-checks every 2
> minutes (no hot loop) until an administrator resolves the collisions and
> clears the list. The finalizer loses nothing: an owner-checked `Delete` on
> that host would find no domain of this VM's.
>
> An *unreachable* pending host is unchanged: still never re-scheduled.

**Condition vocabulary.** There is one positive condition, `Placed`, with reasons
`Bound`, `CreatePending`, `HostUnavailable` and `Unbound`, plus `HostExcluded`
and `AllHostsExcluded` from the slice 2 amendment above, and `Unschedulable`
from the scheduler-accuracy amendment in A5.

### A3: `ListVMs` runs across all hosts on a clustered provider

`ListVMs` is the one call that does **not** go to a single host. A clustered
provider runs it on every routable host in its registry, each with its own
deadline so one dead host cannot starve the rest. Every `VMInfo` is tagged with
its host through a new additive `host_id` field. `ListVMsResponse` gains
`repeated string unreachable_host_ids`, so callers treat those hosts as *unknown*,
not *empty*. An unreachable host is also reported on its Host's condition. It is
never silently dropped from the result as if it had no VMs.

### A4: no automatic failover or re-creation for clustered VMs

D8 already rules out automatic HA in v1. This makes explicit what that means for
the VM controller's generic *"VM no longer exists, recreating"* path
(`virtualmachine_controller.go`):

- For a clustered VM, a `Describe` that reports *not found* on the bound host sets
  a condition and stops. The VM is **not** re-created, whether on the bound host or
  elsewhere.
- The scheduler never moves a bound VM off a `NotReady` host.
- A bound VM changes host (D4's "unless drained") only through `VMHostMigration`,
  never through `Create`.

Recovering without fencing risks two running copies of one disk: exactly the
split-brain that D8 defers to P5.

That path lies dormant for libvirt today only because virsh `Describe` returns an
error on not-found. The ADR-0008 native `Describe` returns `exists=false`, so A4
must ship with routed `Describe`, and before the ADR-0008 PR 5 native switch.

### A5: rollout slices

**Prerequisite (met):** the libvirt domain owner-metadata fix, #333, is merged.
It is the security fix for the released domain-name takeover.

| Slice | Scope |
|---|---|
| 0 | This addendum. |
| 1 | `VMRef` threading for **all** per-VM calls (proto, contracts, transport, every controller); `withHostConn` on `libvirtConn`; the always-failing clustered placeholder with its test; routed `Describe` and `Delete` (including the owner-checked `Delete`); A2 (`pendingHost`, the checked update, the Host in-use finalizer); A4; the `Placed` condition. `Describe` goes first because every reconcile calls it before `Power` or `Reconfigure`. |
| 2 | Routed `Power` and `Reconfigure`. `HardwareUpgrade` gets the proto field only. |
| 3 | The snapshot family, `Clone` (`source_host_id` equal to the landing host), `ExportDisk` / `GetDiskInfo`, and host-encoded task refs. |
| 4 | `ListVMs` across all hosts (A3); adoption keyed on `(host_id, id)`. |
| 5 | End-to-end lab validation: schedule, create, power, describe, snapshot and delete a real VM on a clustered provider. This is the first real clustered VM. |

**Capability honesty (D7).** A clustered provider's `GetCapabilities` hides each
per-VM capability until the slice that routes it has landed.

**ADR-0008 shadow soak.**
- Every shadow read uses the same lease as its virsh read, and the list shadow
  compares on `(host_id, id)`.
- Slices 1 and 4 must not change single-host behaviour.
- The pinned D5 soak image is not redeployed as part of this work.
- Any change to single-host behaviour restarts the 14-day window.

`topology: cluster` stays **experimental** in docs and release notes until slice 5
passes.

**Scheduler accuracy (closed by the amendment below).** Two scheduler-accuracy
gaps had to close before the scheduler's placements could be relied on:
committed capacity (VMs bound to a host, or pending on it, were never subtracted
from its capacity) and reservations (nothing held a host between `Schedule` and
the binding). Both are closed by the *scheduler accuracy* amendment that
follows; A2's `pendingHost` is its prerequisite.

> **Amendment (2026-09-25, scheduler accuracy): committed capacity and
> assumptions.** This closes the two gaps above and resolves open
> implementation question 5.
>
> **What `allocatable` means.** `Host.status.allocatableCPU` and
> `allocatableMemoryMiB` are host **totals**. The libvirt provider fills them
> from `virsh nodeinfo` (`CPU(s)` and `Memory size`,
> `internal/providers/libvirt/hostinfo.go`), with no reserve and nothing
> subtracted for running domains, so they do not move when a VM starts or
> stops. `allocatableStorageBytes` is different: it is live free space (the
> `Available` bytes of the active pools), and the scheduler uses it only for
> the `MinDiskSpacePerHost` floor, never for fit.
>
> **Accounting model: capacity-based.** Because allocatable is a total, the
> operator subtracts what it has committed, and nothing is counted twice:
>
> - *free = allocatable × the pool's overcommit ratio − committed.* The ratio
>   scales the capacity, never the committed sum. A ratio lowered below what is
>   already committed leaves *free* negative: the host takes no new VM, and no
>   VM is moved.
> - *committed(host)* is the sum of the footprints of every VirtualMachine whose
>   placement belongs to the Provider and whose `status.placement.host` or
>   `.pendingHost` names the host. One helper computes the placement Provider
>   everywhere: `status.boundProvider` (an empty namespace meaning the VM's
>   own), else `spec.providerRef`. VMs in every namespace count, found through a
>   cache field index on that key. A VM being deleted counts until the
>   VirtualMachine finalizer is gone, because its domain and disks are still on
>   the host; once only another controller's finalizer keeps it, it counts no
>   more (nor does it block a Host's deletion). A VM that names the host in both
>   fields counts once. The VM being scheduled never counts against itself.
> - Another VM's *footprint* is its **admitted** size, never a size its owner
>   merely asks for: `status.currentResources` when recorded (only the operator
>   writes status); for a VM whose create is still pending, the effective size
>   it was scheduled at (its VMClass with any `spec.resources` override
>   applied — what its Create sends), recorded in
>   `status.placement.pendingResources`; otherwise its VMClass size. A
>   created VM's `spec.resources` and `spec.classRef` are ignored, so editing
>   them costs a tenant nothing and blocks nobody. A VMClass the VM's namespace
>   may not use (no consumer grant) sizes nothing. Every VM counts at least
>   1 vCPU and 128 MiB. While a create is pending, a CRD rule makes
>   `spec.classRef` and `spec.resources` immutable. The rule freezes the
>   references, not the VMClass they point to, so a retried `Create` whose
>   effective size exceeds `pendingResources`, or that would gain a memory
>   hot-add ceiling it was not admitted with (below), is not sent: the VM gets
>   `Placed=False/PendingSizeGrew` and `Provisioning=False/PendingSizeGrew`,
>   keeps its `pendingHost` (a domain may already exist there, A2) and counts
>   at its admitted size until the VMClass is restored or the VM is deleted.
>   The manager's readiness check requires the rule, `pendingResources` and
>   `memoryCeilingMiB`.
> - A VM whose VMClass enables **memory hot-add** counts at its memory
>   *ceiling*, not its current memory. The libvirt provider creates it with a
>   balloon maximum (`<memory>`) of 4× its memory, at least its memory + 1 MiB
>   and without a cap (`contracts.HotplugCeilingMemoryMiB`, which the provider
>   and the scheduler share), and the guest can deflate its balloon up to that
>   at any time, so counting less could over-book the host. The ceiling is
>   recorded when the VM is scheduled, in `status.placement.memoryCeilingMiB`
>   (`0` means none), and counts from then on whatever the VMClass later says.
>   It is not lowered after a resize: the provider does not verify that it
>   lowered `<memory>` offline, so after a shrink the VM may be over-counted,
>   never under-counted. A VM without the record (scheduled by an older
>   manager, or a clone) falls back to its VMClass's current flag. A memory
>   grow within the ceiling commits nothing new and is admitted without a
>   capacity check. CPU hot-add is **not** counted at its ceiling: vCPUs above
>   the current count are offline until a resize brings them online, and that
>   resize is checked.
> - `status.boundProvider` and `status.placement` are trusted inputs to this
>   accounting: tenants must never be granted write on
>   `virtualmachines/status`.
> - Other namespaces' VMs count toward capacity only. VM (anti-)affinity and the
>   host-anti-affinity VM cap stay scoped to the VM's own namespace, as before.
> - Not modelled: domains on a host that VirtRigaud does not manage, and the
>   hypervisor's own use. An administrator keeps room for them with an overcommit
>   ratio below 1 (for example memory `"0.9"`) or by cordoning the host.
> - Rejected alternative: subtracting live free memory. It moves with guest
>   activity and ballooning, it would count running VMs twice, and it cannot see
>   a VM that is pending but not started yet.
>
> **Reservations: an in-process assume cache** (`internal/scheduler/assume`),
> kube-scheduler's *assume* step. For each clustered create, the VM controller
>
> 1. takes the Provider's lock, waiting at most 5 s (otherwise it requeues
>    after about a second, with jitter, instead of parking its worker),
> 2. reads the committed placements from the informer cache, adds the live
>    assumptions, runs `Schedule` and records the pick as an assumption, and
> 3. releases the lock (deferred, so a panic releases it too). Only then does
>    it write anything: the `Placed` condition of a VM that did not fit (a
>    status write bounded to 30 s), or `pendingHost` (A2's checked update,
>    bounded to one minute).
>
> The lock covers informer-cache reads and in-memory work only, never an API
> call, so a slow API server cannot park other reconciles behind it; each read
> under it is bounded by the same 5 s.
> Concurrent reconciles of one Provider therefore see each other's picks, as
> committed capacity and as placed VMs for affinity and anti-affinity.
> Reconciles of different Providers never wait on each other. An assumption
> ends when:
>
> - the informer cache shows the VM's record on the assumed host, or no longer
>   has the VM. From then on the record counts. A record on another host (for
>   example a `pendingHost` the VM has since released) does not end it.
> - the API server refused the `pendingHost` write outright (conflict,
>   invalid, bad request, forbidden, not found), a name conflict releases the
>   host (the A2 amendment), or the VM is deleted. The controller forgets it.
>   After an ambiguous failure (a timeout, a 5xx, a broken connection) the
>   write may have landed, so the assumption stays until the record shows up
>   or the TTL passes.
> - its TTL passes: for a create, twice the one-minute bound of the
>   `pendingHost` write; for an admitted resize, the 5-minute deadline of the
>   `Reconfigure` call plus the 30 s bound of the status write that records
>   it, so it cannot lapse while the call it admitted still runs. The TTL is
>   only a safety net for a reconcile that died in between.
>
> **Resizes are admitted too** (a decision of this review round). A
> `Reconfigure` that grows the CPU or memory of a VM on a clustered Provider is
> checked, under the same lock, against the free capacity of the VM's host,
> the VM's own current footprint excluded. Only the growing resources are
> checked, and host health and cordon do not matter (a resize does not move
> the VM). If it does not fit, no provider call is made, the VM keeps its
> size, gets `Reconfiguring=False/InsufficientHostCapacity` (the message holds
> the VM's own sizes only, no committed or free figure) and is retried with the
> same backoff. A host that is not registered, or whose HostPool is missing or
> belongs to another Provider, fails closed. An admitted resize is assumed at
> its new size until `status.currentResources` records it (the
> assumption is kept alive while a reconfigure task runs), and the scheduler
> counts each VM once per host at the larger of its listed sizes, so a
> concurrent create or resize sees it before it is applied.
>
> A **shrink** is never refused on capacity, but on a clustered Provider it is
> applied only while the VM is powered off. Live, libvirt's `setmem --live`
> moves only the balloon, which the guest can re-inflate up to `<memory>`, and
> a failed live `setvcpus` is reported as success with a restart required.
> Recording the smaller size would let the scheduler give the difference to
> another VM while this one can still use it. So while the VM runs no
> `Reconfigure` is sent: the VM keeps counting at its current size, gets
> `Reconfiguring=False/ShrinkPendingPowerOff`, and is checked again with the
> same backoff. When it is next observed powered off (`spec.powerState: Off`,
> or a shutdown from inside the guest), the shrink is applied before any power
> change and only then recorded in `status.currentResources`; the next
> reconcile powers the VM on again if its spec asks. VirtRigaud never powers a
> VM off to apply a shrink. A change that shrinks any resource waits as a
> whole, including the resources it grows. Single-host Providers are
> unchanged: a shrink is sent live, as before.
>
> The cache is in process, which is correct because only the elected leader
> runs reconcilers. A new leader starts with an empty cache but waits for its
> informer cache to sync, and every placement the old leader made durable is in
> that cache. `pendingHost` is written before `Create`, so no VM is ever created
> on a host that only an assumption held.
>
> **Reporting.** When no host fits, the VM gets `Placed=False/Unschedulable` (a
> Placed reason this amendment adds) and `Provisioning=False/Unschedulable`. The
> message holds the per-category tally. When capacity was short it adds the
> VM's own request only: *requested R vCPU and M MiB, which exceeds the free
> capacity of every candidate host*. It carries no committed, capacity or free
> figure. Those figures are derived from other tenants' VMs, and the VM's owner
> can read the message; the same goes for the success trace in
> `status.placement.reason`. The per-host arithmetic goes to the manager log at
> V(1) and to the gauges below. The message does not grow with the number of
> hosts or VMs, and it names no host and no other VM. The retry backs
> off per VM, from 30 s doubling to 2 min. The VM controller does not watch
> Hosts or other VMs, so 2 min is the longest a VM waits to notice freed
> capacity.
>
> **Metrics.** `virtrigaud_host_committed_cpu` and
> `virtrigaud_host_committed_memory_mib`, labelled by provider
> (`namespace/name`) and host only. The Host controller refreshes them on each
> sync and deletes them with the Host, or when its `spec.providerRef` changes.
> They are **administrator-sensitive**: they show how full each host is, across
> tenants. The manager serves `/metrics` over plain HTTP when
> `--metrics-secure=false`, the chart's current default, so restrict it with
> the chart's NetworkPolicy (`networkPolicy.enabled`, which admits `/metrics`
> scrapes only from `networkPolicy.monitoringNamespaceSelector`) or enable
> `--metrics-secure`.
>
> **Orphan-on-delete on a clustered Provider.** A VM detached with
> `virtrigaud.io/orphan-on-delete` keeps running on its host but leaves the
> accounting. So a VM in another namespace than its clustered Provider is
> detached only when the Provider carries
> `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"` (set by its
> administrator). Otherwise the finalizer is kept, the VM gets
> `Ready=False/OrphanOnDeleteNotAllowed`, and removing the annotation deletes it
> normally.
>
> `virtrigaud.io/force-delete` has the same effect in one case: when the
> provider `Delete` fails (for example the host is unreachable) and the
> annotation releases the finalizer, the domain may keep running on its host
> outside the accounting. A tenant cannot bring that about on purpose — the
> `Delete` has to fail — but an administrator who force-deletes a clustered VM
> should check the host and cordon it, or lower its overcommit ratio, until the
> leftover domain is removed.
>
> **No per-tenant quota.** v0.4.0 has none on a shared clustered Provider:
> any namespace the Provider's `spec.consumerNamespaceSelector` admits can fill
> its hosts, first come first served, up to their capacity. Administrators who
> share a clustered Provider limit consumers with Kubernetes `ResourceQuota` on
> `virtualmachines` counts, or with separate Providers and HostPools.
> *Follow-up:* an ADR for a per-consumer quota on clustered Providers.
>
> **Still not covered:**
>
> - A clone lands on its source host (A1) without being scheduled or checked
>   against capacity. It counts from the moment `bindTargetVM` writes its
>   `placement.host`, and at its VMClass size: the clustered clone bind
>   (`bindTargetVM` / `clonedPlacement`) records no `currentResources` or
>   memory ceiling yet. The ADR-0007 Slice 3 branch adds the clone fit check
>   and records `currentResources` at bind.
> - The libvirt provider's `Reconfigure` reports success for a change it did
>   not apply (a failed `setvcpus` or `setmem`, live or offline, sets
>   `requiresRestart` and returns no error), so the operator records a size the
>   domain does not have. For an unapplied grow that over-counts, which is
>   safe; an unapplied shrink under-counts. The clustered shrink deferral above
>   keeps shrinks off the live path, where they fail, but an offline failure
>   would still be recorded. The provider fix affects single-host Providers
>   too and is tracked separately.
>
> Single-host and thin-client Providers never schedule, so none of this reaches
> them (D9).

### A6: open question

Restoring from a backup (for example with Velero) drops the status subresource,
and with it `placement.host` and `placement.pendingHost`. The next reconcile would
then schedule the VM again. Should the binding also be copied into an annotation, or
into a spec-side "last bound host" hint that a restore brings back? Decide before
slice 5.

---

## Per-provider implementation sketch

### libvirt-cluster (P1–P3)

The single largest code change is that the provider stops being 1:1 with
`PROVIDER_ENDPOINT` and **holds N connections keyed by `host_id`**
(`internal/providers/libvirt/provider.go:56/96/165` become a map). Each
connection reuses ADR-0004's per-host known_hosts material.

- **`ListHosts`** — per host: `virsh nodeinfo` + `virsh nodememstats`
  (cpu/mem), `virsh pool-info` per configured pool (storage), `virsh
  capabilities` / `domcapabilities` (cpu model/features, machine types, emulator),
  libvirtd reachability (health).
- **Create with `target_host_id`** — select the host's connection, create as
  today on that libvirtd.
- **`MigrateVM`** — **pod-brokered managed (non-p2p)** migration: the provider
  pod is the coordinating `virsh` client to *both* hosts (it already holds SSH to
  each). `virsh -c qemu+ssh://user@src/system migrate --live [--auto-converge]
  <dom> qemu+ssh://user@dst/system [--migrateuri tcp://dst-mignic:port] [--tls]
  [--persistent --undefinesource]`. Control: pod→src daemon + pod→dst daemon
  (both already trusted). Data: src QEMU → dst QEMU on ports **49152–49215**
  (the *only* new connectivity requirement — host↔host TCP, no host↔host SSH).
  `storage_mode`: `shared` → RAM+device only; `block_all/inc` →
  `--copy-storage-all/inc` (dst volume pre-created); `stage_copy` → hand off to
  the ADR-0006 pipeline (qcow2→qcow2, no conversion). Progress via `virsh
  domjobinfo`; the operator polls `TaskStatus`.
- **Pre-flight the operator must do** (libvirt aborts safely, but only if these
  hold): dst CPU ⊇ guest flags (cluster CPU-baseline, surfaced via
  `HostInfo.cpu_model`); same machine type + emulator; same bridge name present
  on dst; migration-safe cache; ports open; NTP-synced clocks.

### cloud-hypervisor-cluster (P4)

CH is a per-VM VMM with **no daemon, no inventory, no scheduler** — which is
exactly why VirtRigaud-as-orchestrator is the *only* clustering path for it (and
the cleanest greenfield: nothing native to conflict with). **v1 supervision:
SSH + systemd**, mirroring libvirt-over-SSH — the pod SSHes to the host, drives a
`cloud-hypervisor@<vmid>.service` systemd template unit (or `systemd-run`) to
start/stop CH, and runs `ch-remote --api-socket <sock> <verb>` over the same SSH
channel. Reuses ADR-0004 host-key/known_hosts. (v2: a thin VirtRigaud host-agent
exposing CH lifecycle + host metrics — cleaner real inventory, more to build and
secure.)

- **`ListHosts`** — **synthesized** (no daemon): SSH/agent aggregates per-process
  `ch-remote info` + host-OS metrics.
- **`MigrateVM`** — dst pre-starts a receiver
  (`ch-remote --api-socket dst.sock receive-migration tcp:0.0.0.0:<port>`),
  src `ch-remote send-migration tcp:<dstHost>:<port>`. **Shared storage assumed**
  (RAM + device state only; no `--copy-storage-all` equivalent). `storage_mode`
  restricted to `shared | stage_copy`; `block_*` → `InvalidArgument` via
  capabilities. **Experimental / opt-in**: identical CH build + version-pinned
  state format + homogeneous CPU (little CPU-model abstraction) + no
  VFIO/passthrough/CoCo. Outside that envelope, **cold staging is the default**.

Honest gaps (CH's narrow live-migration envelope; libvirt block migration's
convergence risk on large write-heavy disks) are **declared via capabilities**,
never faked.

---

## Capability matrix (v1 realism)

| Capability | libvirt-cluster | cloud-hypervisor-cluster |
|---|---|---|
| Live migration | **Yes** (mature engine) | **Experimental / opt-in** (identical build + shared storage + no passthrough) |
| Requires shared storage for live | No (shared preferred; block covers non-shared) | **Yes** (RAM + device only, no block) |
| Block / non-shared migration | **Yes** (`--copy-storage-all/inc`) | **No** |
| Cold fallback (`stage_copy`) | **Yes** (ADR-0006, intra-cluster qcow2→qcow2, no conversion) | **Yes** (only option for CH local disk) |
| Host health / inventory | **Yes** (`virsh nodeinfo`/`capabilities`/`pool-info`) | **Yes but synthesized** (no daemon; SSH/agent aggregates) |
| Automatic HA / failover | **Deferred** (no fencing) | **Deferred** |

*Honesty notes*: libvirt's migration engine is mature — the hard parts are
*operational* (CPU baseline, shared storage, ports, fencing), not code. CH
migration is real but narrow; advertise it experimental/opt-in and lead with cold
staging outside the envelope.

---

## Security / compliance

VirtRigaud is documented as deployable in regulated banking environments; the
migration data path forces several explicit calls:

- **Migration RAM stream is cleartext by default.** SSH/TLS on libvirt's
  *control* channel does **not** encrypt the QEMU *data* stream; direct-mode data
  is cleartext unless **QEMU-native `--tls`** (needs a migration cert/CA on the
  hosts). For the banking posture, default to **pod-brokered managed + `--tls`**
  and expose `RequireTLS` on the `HostPool` migration policy. The
  `--p2p --tunnelled` alternative encrypts data via the mgmt channel but needs
  **host↔host** management trust (SSH/16514) the pod model does not establish —
  and costs throughput; not the default.
- **Minimal host-to-host trust.** Pod-brokered managed migration keeps trust
  minimal: control is pod→each-host (already established); the *only* host↔host
  requirement is data ports **49152–49215**. This is deliberately weaker than
  `--p2p`/`--tunnelled`, which would require the source host to reach the
  destination *daemon*.
- **Concurrency multiplier.** An N-host executor holds N SSH/virsh connections,
  multiplying the fork pressure capped in `c8fd36c` (`virsh.go:45`).

  > **Amended 2026-07-20 (ADR-0008).** The per-host budget this bullet asked for
  > **already exists** — `execSem` is a per-`VirshProvider` field (`virsh.go:69`),
  > so N connections ⇒ N semaphores automatically. What actually needs fixing is
  > the **7 bypass sites** and the **shared streaming/control budget** (see the
  > amended Context bullet). ADR-0008 **PR 1**.

- **Migration mode is fixed for v1: `virsh migrate`, not p2p.**

  > **Added 2026-07-20 (ADR-0008 D4).** The migration sketch below stays
  > **`virsh`-subprocess-based in v1**, run over ADR-0008's in-process
  > `*ssh.Client`. Managed migration lives in `libvirt.so`
  > (`virDomainMigrateVersion3Full`), **not** in the RPC wire protocol, so the
  > pure-Go client exposes the five-phase primitives but no orchestration.
  > Reimplementing that flow was rejected (safety-critical, with an inherent
  > unrecoverable `PERFORM3_DONE` window regardless of implementation quality).
  >
  > `VIR_MIGRATE_PEER2PEER` is **deferred to P5**, not rejected: it is
  > technically cleaner (one call, survives pod death, reattachable via
  > `DomainGetJobStats`; Nova and oVirt/vdsm both do this) but requires a TLS
  > listener and PKI. Since **this ADR's D8 already defers automatic HA**, every
  > v1 migration is human-triggered and supervised — exactly when p2p's
  > survive-pod-death advantage matters least. Keeping `virsh` preserves this
  > ADR's pod-brokered trust boundary **exactly**, needs no PKI and no new
  > listener, and keeps the `virsh domjobinfo`/`domjobabort` progress and abort
  > specified below working unchanged.
  >
  > Two honest costs: `libvirt-clients` (and its C stack) stays in the provider
  > image for this one command, so ADR-0008's image/CVE win is **partial until
  > P5**; and **pod death mid-migration leaves a VM paused** needing a manual
  > `virsh resume` — document this in the migration runbook.
  >
  > ADR-0008 D4 carries a full **pre-analysis of what p2p would require** (a
  > dedicated migration sub-CA, the `tls_allowed_dn_list` empty-vs-unset trap,
  > the `listen_addr`-ignored-under-socket-activation trap, CRL default-path
  > constraints, no OCSP, and the residual that a migration client cert is
  > **root-equivalent and cannot be narrowed**) so the P5 decision is *made*,
  > not re-researched.
- **Reuse ADR-0004** SSH host-key/`known_hosts` verification **per host**; no
  TOFU. A future CH host-agent inherits the same machinery.
- **`postcopy` is a footgun** and must be opt-in + surfaced: it converges
  non-convergent migrations but **breaks safe-abort** (a network drop mid-post-copy
  loses the VM). Default off; require an explicit pool/VM opt-in.
- **Fencing is a new privileged surface** (IPMI/BMC/PDU) — **deferred** (D8) and
  will need its own `security-architect` threat model when P5 is designed.
- **No secrets in Status/events/logs** — host credentials live only in the
  referenced Secret; `Host.status` carries capacity/health, never connection
  secrets. The `internal/obs/logging` redactor must cover any new host-credential
  keys; add a test.

---

## Phasing

- **P1 — inventory + scheduler + create-on-host (libvirt-cluster, no migration).**
  `Host`/`HostPool` CRDs, `ListHosts`, operator inventory sync into `Host.status`,
  the filter+score scheduler consuming `VMPlacementPolicy`, `target_host_id` on
  `CreateRequest`, and the `status.placement.host` binding. The N-keyed libvirt
  connection map lands here. No migration yet — this is the smallest thing that
  proves the brain/hands split end-to-end.

  > **Amended 2026-07-20 — P1 has prerequisites (ADR-0008).** This ADR was
  > written before ADR-0008 and is otherwise silent on the pure-Go driver, while
  > **P1 rewrites the same `provider.go` code ADR-0008 restructures.** To avoid
  > building the N-host connection map twice, **ADR-0008 PR 2–4 are P1
  > prerequisites**:
  > - **PR 2 — the connection seam** (`internal/providers/libvirt/hostconn/`:
  >   `HostID`/`Conn`/`Registry`). This *is* the "N-keyed connection map" P1
  >   needs; ADR-0008 builds it holding **one** host from `PROVIDER_ENDPOINT`, and
  >   P1 changes **only the constructor** to project N from `Host` CRs. It also
  >   deletes the six `s.provider.(*Provider)` type assertions in `server.go`
  >   (`:269,358,400,454,501,669`) that currently weld the gRPC layer to the
  >   concrete virsh implementation.
  > - **PR 3 — in-process SSH.** Required before clustering because it deletes
  >   the `LIBVIRT_DEFAULT_URI` process-environment hop (`virsh.go:273`) that
  >   **hard-codes a single host into the process**. N connections are not
  >   reachable while the URI lives in `os.Environ()`.
  > - **PR 4 — connection lifecycle** (redial, stale-handle close, keepalive
  >   prober, per-call watchdog). P1 holds N long-lived connections; without this,
  >   one dead host produces the invisible-and-total failure mode documented as
  >   PR #291 finding **B3**.
  >
  > P1's own scope (CRDs, `ListHosts`, scheduler, `target_host_id`, binding) is
  > unchanged.
- **P2 — libvirt intra-cluster LIVE migration on shared storage
  (`storage_mode=shared`).** The new `VMHostMigration` kind (D5) + `MigrateVM` +
  `virsh domjobinfo` progress + operator CPU-baseline/machine-type/network/ports
  pre-flight + operator-initiated evacuation (drain a cordoned host). The clean,
  low-risk case first.
- **P3 — libvirt block migration (`block_all`/`block_inc`) + cold staging reuse.**
  Local-disk-must-stay-up (`--copy-storage-*`, with `--auto-converge`/bandwidth
  caps) and tolerate-downtime (`stage_copy` → ADR-0006 pipeline, no conversion).
- **P4 — cloud-hypervisor-cluster executor.** `type: cloudhypervisor` +
  `topology: cluster`; SSH + systemd template unit; experimental live migration
  (shared storage only) with cold staging as the default fallback.
- **P5 — DEFERRED, own ADR: automatic HA + fencing/STONITH.** Reliable
  dead-vs-partitioned detection + a fencing subsystem. Do not start until P1–P4
  are solid; this is where corruption risk concentrates. *(Amended 2026-07-20:
  P5 **also owns the migration-mode decision** — `virsh` vs
  `VIR_MIGRATE_PEER2PEER` — and, if p2p, the migration PKI. See Open question 8.)*

*Storage/networking automation* (deploying NFS/Ceph rather than consuming it)
threads through as its **own separate track**, not on this critical path — v1
consumes pre-set-up storage/networking (D6).

Recommendation: **libvirt before CH, shared-storage live before block before
cold, report-only before any automation of failover.**

---

## Consequences

**Positive** — VirtRigaud gains real multi-node capability for hypervisors that
have none, via a clean, industry-proven brain/hands split (Engine/VDSM,
nova-scheduler/nova-compute). The design generalizes across hypervisors because
the brain is hypervisor-agnostic. `VMPlacementPolicy` finally has a scheduler to
act on. Single-host libvirt and the vSphere/Proxmox thin-client model are
untouched (additive `topology` discriminator). Every risky capability is
capability-gated; the honesty-first stance kills the silent-no-op class before it
starts. The remote-provider invariant holds — the provider stays a stateless
executor, so its failure cannot lose cluster state.

**Negative / the honest reality — we are building a cluster manager, and each of
these is nontrivial:**
- **A scheduler** (filter+score, constraints, affinity) — starts dumb but is a
  new subsystem to build, test, and keep re-runnable.
- **A migration state machine** distinct from ADR-0006's, with libvirt's
  five-phase protocol, convergence handling (`auto-converge`/`timeout`/`postcopy`
  trade-offs), and safe-abort semantics to preserve.
- **Live host inventory + health** synced from N connections, with per-host
  concurrency budgets so it does not re-trigger the fork storm `c8fd36c` fixed.
- **CPU-compatibility** is a first-class operational gate (heterogeneous
  generations + host-passthrough abort migrations); the cluster needs a
  CPU-baseline, surfaced and pre-checked — CH clusters must be homogeneous.
- **Storage & networking are pushed onto the operator** as consumed constraints;
  the silent wrong-VLAN blackhole is a documentation + validation burden.
- **Automatic HA is explicitly absent** — without fencing it would corrupt disks,
  so v1 ships report-only failure handling and defers the scariest subsystem.
  Users must understand "clustered" does **not** yet mean "self-healing".
- **cloud-hypervisor live migration is narrow** — advertised experimental; most
  real CH moves will be cold staging.

The net: this is a multi-release epic, not a feature. Its value is real, but so
is the surface area, and the phasing exists to keep each slice shippable and
honest.

---

## Open implementation questions

1. **Provider-type vs topology discriminator (D9). ✅ Resolved 2026-09-21 (see
   D9).** `spec.topology: single|cluster` — a **string enum, not a bool** (default
   `single`), so a third topology mode stays additive. The distinct
   `libvirt-cluster` provider type is rejected (type explosion).
2. **How the `Host` inventory projects into the provider pod. ✅ Resolved
   2026-09-21 (see D3).** A **single projected, versioned-schema Secret** the
   provider **watches and hot-reloads** (lazy-open on add, graceful-drain on
   remove); a Secret (not a ConfigMap) keeps connection material out of
   namespace-readable config and preserves the #297 no-API-access invariant.
3. **Intra-cluster migration trigger: new kind vs discriminated `VMMigration`.
   ✅ Resolved 2026-09-21 (see D5) — a new `VMHostMigration` kind**, *not* a
   `VMMigration` discriminator. The two are different domains sharing only a noun; a
   union-type `VMMigration` ages badly and is automatic-HA's awkward base.
   `VMHostMigration` shares internal migration primitives and the condition
   vocabulary, **not** the CRD schema. (Overrides the draft's earlier discriminator
   lean.)
4. **Host membership: `Host.spec.poolRef` (explicit) vs `HostPool` label
   selector.** Recommended explicit `poolRef`; selector is more k8s-idiomatic but
   fuzzier for an inventory-of-record.
5. **Overcommit + allocatable computation. ✅ Resolved 2026-09-25 (see the
   *scheduler accuracy* amendment in Addendum A, A5).** The provider reports
   host totals; the operator applies the pool ratio to them and subtracts the
   footprints of the VMs bound to, pending on, or assumed on each host.
6. **Migration data-NIC selection** — how `--migrateuri tcp://<host-mignic>` is
   modeled (per-host label? `HostPool` migration-network ref?) so the RAM stream
   can be steered onto a dedicated NIC.
7. **CPU-baseline lifecycle** — who computes the cluster baseline (`virsh
   cpu-baseline` across hosts), where it is stored (`HostPool.status`?), and how
   VMs are pinned to it at create so they stay migratable.
8. **Migration mode and migration PKI at P5 — decide with the fencing ADR.**
   *(Added 2026-07-20, ADR-0008 D4.)* v1 is fixed: **`virsh migrate`**, no PKI,
   trust boundary unchanged. P5 must decide whether automatic evacuation moves to
   **`VIR_MIGRATE_PEER2PEER`** — which survives pod death and is reattachable via
   `DomainGetJobStats`, and is therefore *much* more attractive once migrations
   become machine-triggered rather than human-supervised — at the cost of a TLS
   listener and a **dedicated migration sub-CA**. ADR-0008 D4 contains the full
   pre-analysis (traps and all). The decision is **not** just "is p2p nicer": it
   requires explicitly accepting that **a migration client certificate is
   root-equivalent on the hypervisor and cannot be narrowed** — polkit does not
   apply to TLS clients, and the DN allowlist gates *whether you connect*, never
   *what you may do*. **One stolen certificate opens the whole HostPool; pool size
   IS blast-radius size.** Bundle this into the P5 fencing ADR and its
   `security-architect` threat model; do not decide it separately.

## Follow-ups this ADR creates

- `crd-update`: new `host_types.go` + `hostpool_types.go` +
  `vmhostmigration_types.go` (the live-migration trigger kind, D5); additive fields
  on `provider_types.go` (`Topology`, `ReportedCapabilities` clustering flags,
  `cloudhypervisor` enum value) and `virtualmachine_types.go`
  (`status.placement`). Regenerate + `sync-helm-crds`; update examples + docs.
- `proto-update`: `ListHosts` / `GetHostInfo` / `MigrateVM` + `HostInfo` /
  `MigrateVMRequest` + `target_host_id` on Create/Clone + `GetCapabilities`
  clustering flags; regenerate; stub `Unimplemented` in **all** providers
  (vsphere/proxmox/mock return `Unimplemented`; libvirt implements per phase).
- `sdk/provider/capabilities/`: new `Capability` constants
  (`clustering`, `live_migration`, `block_migration`, `shared_storage`,
  `host_evacuation`) + a `ProfileCluster` profile.
- New operator subsystems: `internal/scheduler/` (pure filter+score),
  `internal/controller/host_controller.go` +
  `hostpool_controller.go` (inventory sync **and rendering the projected,
  versioned host-inventory Secret** the provider watches, D3), and the
  `vmhostmigration_controller.go` live-migration state machine (the
  `VMHostMigration` kind per D5 — reusing the shared migration primitives and
  condition vocabulary, not the `VMMigration` schema).
- `internal/providers/libvirt/`: N-keyed connection map + `MigrateVM`
  implementation. *(Amended 2026-07-20: the connection map is **ADR-0008 PR 2**'s
  `hostconn.Registry` — build it there, not here. The "per-host exec semaphore"
  originally listed is **already the case** (`virsh.go:69`); what PR 1 actually
  fixes is the 7 bypass sites and the shared streaming/control budget.)*
- `security-architect`: review the migration data-path TLS posture, the per-host
  concurrency budget, and (separately, at P5) the fencing privileged surface.
- `virtualization-specialist`: own the CPU-baseline mechanism, the machine-type/
  emulator/bridge pre-flight, and the CH live-migration envelope.
- `tech-writer` / website: a "clustered provider" concept doc — what "clustered"
  does and does **not** mean in v1 (no automatic HA), and the pre-set-up
  storage/networking requirements — coordinated with implementation, not ahead.
- **New ADR (P5)**: automatic HA + fencing/STONITH.
- **Addendum A** (post-create lifecycle routing), delivered in slices 1–5 (see A5):
  - Proto: `target_host_id` on every per-VM request; `CloneRequest.source_host_id`;
    `owner` on `DeleteRequest`; `VMInfo.host_id` and
    `ListVMsResponse.unreachable_host_ids`.
  - Operator: `contracts.VMRef`; `status.placement.pendingHost`; the Host in-use
    finalizer; the `Placed` condition.
  - Provider: the `withHostConn` helper.
- **New ADR: per-consumer quota on a shared clustered Provider.** The
  scheduler-accuracy amendment (A5) counts every consumer's VMs against a
  host's capacity, but nothing limits how much of a shared Provider one
  consumer namespace may take (v0.4.0 has no per-tenant quota).
- Capacity check for a clone, which lands on its source host without being
  scheduled, and `currentResources` recorded at the clustered clone bind
  (`bindTargetVM` / `clonedPlacement`), which today writes `placement.host`
  only (A5, *Still not covered*). Planned on the ADR-0007 Slice 3 branch.
- The libvirt provider's `Reconfigure` must return an error when a requested
  change was not applied (today a failed `setvcpus` or `setmem`, live or
  offline, returns success with `requiresRestart`). This affects single-host
  Providers too, so it is tracked separately from ADR-0007.
