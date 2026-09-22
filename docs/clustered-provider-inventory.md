# Clustered-provider inventory: `Host` and `HostPool`

> **Status:** ADR-0007 P1, in progress. **Additive and v1beta1-safe.** Single-host
> providers and their VMs are unchanged (ADR-0007 D9). Shipped so far: the two
> inventory CRDs (`Host`/`HostPool`), the `ListHosts`/`GetHostInfo` gRPC contract
> (stubbed in every provider), the `Provider.spec.topology` discriminator, and the
> operator side of the projected-Secret pipeline (rendering host **metadata** into
> a Provider-owned Secret mounted into the provider pod). Still to come: credential
> inlining into that Secret (a dedicated security-reviewed PR), the provider-side
> file consumption / hot-reload, the `topology: cluster` validating webhook, the
> scheduler, and migration (later P1/P2 slices).

VirtRigaud is adding a new *class* of provider — a **clustered / orchestrator**
provider — that makes VirtRigaud itself the cluster manager for hypervisors that
lack a native one (libvirt/KVM first). Instead of one gRPC process bound to one
host, a clustered provider fronts N bare hosts, and VirtRigaud owns the host
inventory, placement, and (later) cross-host migration.

The design follows the industry-proven **brain/hands split** (oVirt Engine/VDSM,
OpenStack nova-scheduler/nova-compute): all durable state and all decisions live
in the operator and CRDs; the provider stays a stateless executor (ADR-0007 D1).
The two CRDs below are the durable inventory that the brain reasons over.

See [ADR-0007](adr/0007-clustered-orchestrator-provider.md) for the full design,
rationale, and phasing.

## `HostPool` — a named group of hosts under one clustered provider

`HostPool` (shortName `hp`) carries **cluster policy** for a group of hosts:

| Field | Purpose |
|-------|---------|
| `spec.providerRef` | The clustered `Provider` that owns this pool. |
| `spec.strategy` | Default scheduling strategy: `Spread` (default) or `BinPack`. |
| `spec.overcommit` | CPU/memory overcommit ratios (decimal strings, e.g. `"4.0"`). |
| `spec.storagePools[]` | Named shared/local storage pools available in the pool. |
| `spec.networks[]` | Named networks (bridge/VLAN/overlay) available in the pool. |
| `spec.migration` | Default migration policy: `defaultLive` (default `true`), `defaultStorageMode` (`shared`\|`block_all`\|`block_inc`\|`stage_copy`, default `shared`), `requireTLS` (default `false`), and optional `bandwidthMbps` / `maxDowntimeMs` caps. |

`status` aggregates member-host counts and readiness (`totalHosts`, `readyHosts`,
`schedulableHosts`).

VirtRigaud **consumes** pre-set-up storage and networking; it does not deploy
them (ADR-0007 D6).

## `Host` — one bare hypervisor host

`Host` (shortName `hvh`) is **admin-authored desired inventory** in `spec`, and
**operator-synced observed state** in `status` — the clean desired/observed split:
the admin declares *what hosts exist*; the provider reports *what they can do
right now*.

| `spec` field | Purpose |
|-------|---------|
| `providerRef` | The clustered `Provider` that executes on this host. |
| `poolRef` | The `HostPool` this host joins (membership is declared by the host, ADR-0007 D3). |
| `endpoint` | Connection URI (libvirt: `qemu+ssh://user@host/system`). |
| `credentialSecretRef` | Optional per-host credential override; defaults to the Provider's. |
| `labels` | Placement facts consumed as **hard scheduling constraints** (ADR-0007 D6): storage-pool visibility, network/bridge visibility, zone/rack. |
| `schedulable` | Defaults `true`; set `false` to cordon (no new placement, existing VMs stay). |

`status` (populated later by the inventory-sync controller) reports `health`
(`Ready`\|`NotReady`\|`Unknown`), allocatable CPU/memory/storage, CPU
model/features, machine types, emulator version, bound-VM count, and last
heartbeat. It carries capacity and health only — **never connection secrets**
(ADR-0007 Security).

### Placement labels are load-bearing

The nastiest silent failure in clustering is migrating a VM to a host whose
bridge name matches but whose VLAN is wrong — the NIC attaches and traffic
blackholes. VirtRigaud models this as a scheduling constraint from day one:
a VM is only placed or migrated onto a host that can **see its storage pool** and
**has its network**, asserted via `Host.spec.labels` matching the pool's
`storagePools` / `networks`. Reserving that constraint surface early is cheap;
retrofitting it after VMs are stranded is not.

## Example

See [`examples/hostpool-clustered.yaml`](../examples/hostpool-clustered.yaml):
one `HostPool` (Spread strategy) with two `Host`s that both see storage pool
`nfs01` and bridge `br-vlan100`, making shared-storage live migration between
them possible once the migration slice ships.

## Provider gRPC contract for inventory

The wire contract the inventory-sync controller will call is now defined in
`proto/provider/v1/provider.proto` (additive, no package bump):

| Addition | Purpose |
|----------|---------|
| `rpc ListHosts(ListHostsRequest) returns (ListHostsResponse)` | Enumerate every host a clustered provider fronts. |
| `rpc GetHostInfo(GetHostInfoRequest) returns (HostInfo)` | Cheaper single-host refresh (reconciling one `Host`). |
| `message HostInfo` (fields 1–11) + `enum HostHealth` | Per-host inventory: id, address, allocatable CPU/mem/storage, health, labels, CPU model/features, machine types, emulator version. Mirrors `Host.status`. |
| `bool supports_clustering` on `GetCapabilitiesResponse` (field 17) | A provider advertises here that it fronts a host set and implements the two RPCs above. |

**These RPCs are contract-only today.** Every production provider (vSphere,
libvirt, Proxmox) and the mock provider returns `codes.Unimplemented` for
`ListHosts`/`GetHostInfo` and advertises `supports_clustering = false` —
honesty-first (ADR-0007 D7). libvirt is the first hypervisor slated to implement
them for real (N host connections keyed by `host_id`, reading `virsh nodeinfo` /
`pool-info` / `domcapabilities`); that lands in a later ADR-0007 P1 PR alongside
the projected-Secret and the inventory-sync controller. The manager-side gRPC
client and the provider SDK already map the new messages, so the real
implementation only has to fill in the host queries.

## The `topology` discriminator and the projected-Secret pipeline

A `Provider` selects its deployment topology with `spec.topology` (ADR-0007 D9):

| Value | Meaning |
|-------|---------|
| `single` (default) | One gRPC process bound to one host — today's behavior, **byte-for-byte unchanged**. No host inventory, no projected Secret. |
| `cluster` | The provider fronts N bare hosts registered as `Host` CRs under a `HostPool`; the operator owns inventory/placement/migration. |

It is a **string enum, not a bool** (settled 2026-09-21), so a future third
topology mode is a purely additive enum value. Defaulting `""` → `single` keeps
every existing single-host `Provider` unchanged. `topology: cluster` is only
meaningful for hypervisors with no native cluster manager (libvirt first); a
validating webhook that rejects it on `type: vsphere|proxmox` (ADR-0007 D2) is a
later PR.

### How a clustered provider learns its hosts (ADR-0007 D3)

A thin, API-less provider (post-#297) must **not** read the Kubernetes API. So the
operator tells it which hosts it fronts through a **single projected Secret**, not
an API query:

1. The **Provider controller** gathers the `Host` CRs whose `spec.providerRef`
   names a `topology: cluster` Provider and renders their metadata into a
   **versioned-schema document**.
2. That document is written to a **Secret** (never a ConfigMap) named
   **`<provider>-hosts`**, **owner-referenced** to the Provider, in the provider's
   namespace. Re-renders are deterministic (hosts sorted by id), so an unchanged
   inventory produces no Secret write.
3. The Secret is **mounted read-only** into the provider pod at
   **`/etc/virtrigaud/hosts/`** (key `hosts.json`), mirroring the existing
   credential mount. The provider will **read that file — never the API**,
   preserving the no-API-access invariant hardened in **#297**.

The document schema (`schemaVersion` gates format evolution):

```json
{
  "schemaVersion": 1,
  "hosts": [
    {
      "id": "host-a",
      "endpoint": "qemu+ssh://virt@host-a/system",
      "labels": { "storage.virtrigaud.io/pool-nfs01": "true" },
      "credentials": {}
    }
  ]
}
```

See [`examples/provider-libvirt-clustered.yaml`](../examples/provider-libvirt-clustered.yaml)
for a `topology: cluster` Provider that pairs with the `hostpool-clustered.yaml`
inventory.

### What is deliberately NOT wired yet

This is the **structural half** of the pipeline. Rendered separately, under their
own reviews:

- **No credential material.** The `credentials` sub-document is **defined but left
  empty** in this slice — the render path never reads or logs any credential
  value. Resolving each Host's (or the Provider's) `credentialSecretRef` and
  inlining the material is a **dedicated, security-reviewed PR**.
- **No provider consumption.** The provider mounts the Secret but does **not** read
  it yet; the file-watch / hot-reload (lazy-open on host-add, graceful-drain on
  host-remove) and the N-host connection map land in a later PR.
- **No webhook.** The `topology: cluster` validating webhook (ADR-0007 D2) is a
  later PR.

Security posture, unchanged from single-host: the rendered object is a `Secret`
(so connection material stays out of namespace-readable config), owned by the
Provider and GC'd with it; provider pods still hold **no** Kubernetes RBAC and no
projected API token (#297); and the manager writes **only** the one
`<provider>-hosts` Secret it owns (it never gains secret-delete).

## What "clustered" does not mean (yet)

- **No automatic HA / failover.** v1 detects and surfaces host-down and supports
  operator-initiated evacuation, but does **not** auto-restart VMs elsewhere.
  Without fencing/STONITH that would risk split-brain disk corruption; automatic
  HA is a deferred future ADR (ADR-0007 D8/P5).
- **No scheduler or migration in this slice.** This PR ships the `Host` and
  `HostPool` CRDs only. The inventory-sync controller, the filter+score scheduler,
  `target_host_id` on create, and live migration land in later ADR-0007 slices.
