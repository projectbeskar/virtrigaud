# Clustered-provider inventory: `Host` and `HostPool`

> **Status:** ADR-0007 P1, in progress. **Additive and v1beta1-safe.** Single-host
> providers and their VMs are unchanged (ADR-0007 D9). Shipped so far: the two
> inventory CRDs (`Host`/`HostPool`), the `ListHosts`/`GetHostInfo` gRPC contract
> (stubbed in every provider), the `Provider.spec.topology` discriminator, the
> operator side of the projected-Secret pipeline (rendering host **metadata** into
> a Provider-owned Secret mounted into the provider pod), **per-host credential
> inlining** into that Secret (the SSH private key + known_hosts resolved from each
> Host's / the Provider's `credentialSecretRef`), the **libvirt provider-side
> consumption** of that file — parsing it into N host-keyed connections and
> hot-reloading them on change — and the **libvirt `ListHosts`/`GetHostInfo`
> implementation**: the clustered libvirt provider now *answers* those RPCs with
> live per-host facts (`virsh nodeinfo` / `capabilities` / `domcapabilities` /
> `pool-info`) and advertises `supports_clustering = true`. Still to come: the
> inventory-sync controller that *calls* those RPCs to populate `Host.status`,
> host-targeted `Create`/`Migrate` (via `target_host_id`), the `topology: cluster`
> validating webhook, the scheduler, and migration (later P1/P2 slices).

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

**libvirt now implements these RPCs for real in clustered topology.** A libvirt
provider running with `topology: cluster` answers `ListHosts`/`GetHostInfo` with
live per-host facts and advertises `supports_clustering = true`. vSphere,
Proxmox, and the mock provider — and a **single-host** libvirt provider — still
return `codes.Unimplemented` and advertise `supports_clustering = false`
(honesty-first, ADR-0007 D7/D9): these are clustered-only RPCs.

### What `ListHosts` reports per host (libvirt)

For each host the registry fronts, the clustered libvirt provider borrows a
short-lived connection lease and runs a small, read-only `virsh` query set —
each `HostInfo` field is fed by exactly one command:

| `HostInfo` field | Source (libvirt) |
|------------------|------------------|
| `id`, `address`, `labels` | The parsed host inventory (no query). `address` is the host's endpoint; `labels` are its placement facts. **Never** the connection secrets. |
| `allocatable_cpu` | `virsh nodeinfo` → `CPU(s)` (logical CPU count). |
| `allocatable_mem_mib` | `virsh nodeinfo` → `Memory size` (KiB, converted to MiB). |
| `cpu_model`, `cpu_features` | `virsh capabilities` → `<host><cpu><model>` and the `<feature>` flags (parsed via typed `libvirtxml.Caps`). |
| `machine_types` | `virsh domcapabilities` → the default `<machine>` (typed `libvirtxml.DomainCaps`). See the note below. |
| `emulator_version` | `virsh domcapabilities` → `<path>` (the emulator binary path). See the note below. |
| `allocatable_storage` | `virsh pool-list --all` + `virsh pool-info --bytes` on each **active** pool, summing `Available`. Best-effort. |
| `health` | `nodeinfo` reachability: `Ready` when the connection and `nodeinfo` succeed, `NotReady` otherwise. |

**`allocatable` = host TOTAL, not free-after-overcommit.** `allocatable_cpu` and
`allocatable_mem_mib` are the host's **raw schedulable capacity** (total logical
CPUs, total memory). This layer does **not** apply `HostPool` overcommit ratios
and does **not** subtract already-bound VMs — the operator-side scheduler does
that later, on top of these totals (ADR-0007). The field name follows the wire
contract; read it as "capacity the scheduler starts from".

**Health does not fail the whole call.** One unreachable host is reported
`NotReady` (with the id/address/labels the registry still knows) while its
siblings render normally — a single down host never aborts `ListHosts`. Only a
non-clustered provider errors (with `Unimplemented`).

**Best-effort fields.** `nodeinfo` is the one *core* query (it gates health); if
it fails the host is `NotReady` and the richer queries are skipped. `capabilities`,
`domcapabilities`, and the storage pools are **best-effort**: a failure there
leaves that field empty/`0` and is logged, never flipping health. Storage is
whole-host today because the inventory file carries no per-pool configuration yet.

**Two documented judgment calls (deviations from a naive reading):**

- `machine_types` is the host's **default** machine type only — `virsh
  domcapabilities` reports a single `<machine>` for the default emulator/arch, not
  the full enumeration. A complete list (from `virsh capabilities` `<guest>`
  arches) is a follow-up.
- `emulator_version` carries the emulator **binary path** (e.g.
  `/usr/bin/qemu-system-x86_64`), because `domcapabilities` exposes no numeric QEMU
  version. The path is the emulator *identity* the ADR-0007 migration pre-flight's
  "same emulator" check compares; a true version string (via `virsh version`) is a
  follow-up.

The lease is **always released** on every path (success, query error, even if the
host is removed mid-collection), so gathering inventory never severs the
graceful-drain guarantee the registry provides.

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
      "credentials": {
        "sshPrivateKey": "<base64 of the SSH private key>",
        "knownHosts": "<base64 of the known_hosts entry>"
      }
    }
  ]
}
```

`credentials` carries the SSH connection material as base64-encoded bytes (the
JSON encoding of Go `[]byte`), copied verbatim from the source credential Secret
so a PEM key round-trips with no corruption. It mirrors the libvirt credential
Secret keys — `sshPrivateKey` ← `ssh-privatekey`, `knownHosts` ← `known_hosts` —
so the provider consumes it through the same code path it uses for single-host
credentials today. A rendered host always carries `sshPrivateKey`; `knownHosts`
is present only when the source Secret has one (an unpopulated `credentials` still
renders as `{}` for schema stability). The SSH **user** is not inlined — it is part
of the `endpoint` URI (`qemu+ssh://user@host/system`); **password** auth is not
inlined either (the clustered model standardizes on key-based SSH with
`known_hosts` verification, ADR-0004).

See [`examples/provider-libvirt-clustered.yaml`](../examples/provider-libvirt-clustered.yaml)
for a `topology: cluster` Provider that pairs with the `hostpool-clustered.yaml`
inventory.

### How each host's credentials are resolved

For every fronted `Host`, the Provider controller resolves one credential Secret
and inlines its SSH material into that host's `credentials`:

1. **Per-host override first.** If `Host.spec.credentialSecretRef` is set, that
   Secret is used (its namespace defaults to the Host's namespace).
2. **Provider default otherwise.** Otherwise the Provider's own
   `spec.credentialSecretRef` is used (in the Provider's namespace — the same
   Secret the single-host credential mount uses).

The controller extracts the libvirt credential-Secret keys `ssh-privatekey`
(mandatory) and `known_hosts` (optional) and copies the raw bytes verbatim into
the rendered document. The **operator** performs these Secret reads with the RBAC
it already holds (`secrets: get;list;watch`); **no new RBAC is added**, and the
provider still reads only the mounted file — never the Kubernetes API (#297).

#### Missing or malformed credentials → skip that host, never fail the reconcile

If a host's credential Secret is **missing**, **unreadable**, or **has no
`ssh-privatekey`**, the controller does **not** render a half-usable host and does
**not** fail the whole reconcile. It **skips that one host** from the inventory and
continues with the rest, then surfaces the skip:

- a `HostCredentialsReady` **condition** on the Provider goes `False`
  (reason `CredentialsUnresolved`), listing the skipped **host ids** and a coarse
  **reason** (e.g. `credential secret default/foo not found`);
- a **Warning event** (`HostCredentialsUnresolved`) is emitted on the Provider;
- a structured **log** line records the same.

None of these ever carry a credential value — only host ids and coarse reasons.
This is deliberately **fail-safe, never fail-open**: an unresolvable host is
withheld until its credentials are fixed (which re-triggers a re-render), rather
than shipped in a broken state. `known_hosts` is intentionally **not** required at
render time — the provider keeps enforcing its ADR-0004 host-key policy (hard-fail
unless the audit-flagged insecure escape hatch is set) at connect time, so that
one security decision stays in a single place.

### How the libvirt provider consumes the inventory (ADR-0007 D3)

The libvirt provider reads the mounted file — never the Kubernetes API (#297) —
and turns it into N host-keyed connections it hot-reloads as the file changes.

**Mode detection.** At startup the provider checks for the inventory file at
`/etc/virtrigaud/hosts/hosts.json` (overridable via `VIRTRIGAUD_LIBVIRT_HOSTS_FILE`
for tests):

- **Present** → **clustered mode**: build one lazily-dialed connection per host,
  keyed by `host.id`, from the file's endpoints + inlined credential material.
- **Absent** → **single-host mode**: exactly today's behavior from
  `PROVIDER_ENDPOINT` + the credential mount — **byte-for-byte unchanged**
  (ADR-0007 D9). No inventory, no watcher, no new code path.

An empty (zero-host) or malformed file never crashes the provider: a zero-host
inventory is a valid "fronts no hosts right now" state (empty registry), and a
malformed/unreadable file at startup is logged and treated as an empty host set
that the watcher reconciles once the file becomes valid — a clustered provider is
never brought down by one bad render (fail-safe, not fail-closed).

**Lazy-open.** A host in the inventory is **registered but not dialed**; the SSH
connection opens on first use of that host, not at load or reload time. Adding ten
hosts to the file costs zero connections until work is actually routed to one.

**Hot-reload — watching the directory, not the file.** A Kubernetes Secret update
lands as an **atomic `..data` symlink swap**: the kubelet writes the new content
into a fresh timestamped directory and renames the `..data` symlink to point at
it, so the visible file's inode is swapped out from under any watch registered on
the file itself — an `fsnotify` watch on the file inode goes deaf after the first
update. The provider therefore watches the **parent directory** and re-reads the
canonical path on any event (plus a low-frequency backstop re-read in case an
event is ever missed). On each successful re-read it reconciles the connection
set:

- **host-added** → registered lazily (dialed on first use);
- **host-removed** → **graceful-drain**: it stops taking new work immediately, and
  its live connection is closed only once it is **idle** — a reload **never severs
  an in-flight operation**;
- **host-changed** (endpoint or credential material differs) → drain the old
  connection (again, only once idle) and reopen lazily; a **label-only** change is
  not a connection change and does not drain.

A malformed or unreadable file on reload is logged and **ignored** — the last-good
host set is kept, so a bad write never drains every host.

**Credential sourcing — the same code path, per host.** Each clustered host feeds
its own inlined `sshPrivateKey` and `knownHosts` bytes into the exact single-host
SSH transport and ADR-0004 host-key verification: the private key is parsed
in-memory, and the `known_hosts` bytes are materialised to a private per-host file
so the identical `knownhosts` verification runs against **that host's** trust
material. ADR-0004 is preserved per host and **not weakened** — a host with empty
`knownHosts` hard-fails verification at connect time unless the audit-flagged
insecure escape hatch is set, exactly as the single-host path does.

`sshPrivateKey` and `knownHosts` travel as base64 in the inventory JSON because
they are Go `[]byte` fields; the provider decodes them **exactly once** (the JSON
unmarshal) back to the raw PEM / raw `known_hosts` bytes and hands those straight
to `ssh.ParsePrivateKey` / the `knownhosts` file. There is deliberately **no
second decode** on the consume side — a spurious extra base64 layer would hand
non-PEM bytes to the parser and fail with `ssh: no key found`. This single-layer
round-trip is pinned by an end-to-end regression test (a real key rendered exactly
as the controller renders it, consumed exactly as the provider consumes it, then
authenticated over an in-memory SSH server), never logging the key material.

**Readiness (`Validate`) — the registry, not a single host.** In clustered mode
the provider has **no single `PROVIDER_ENDPOINT`**, so the readiness `Validate`
the manager calls before it starts syncing hosts does **not** run the single-host
`virsh -c <endpoint> list` probe (which, with no endpoint, would run `virsh -c ""
list` and fail "cannot connect to the hypervisor"). Instead it validates the
**clustered setup**: the connection registry is initialized and fronts **at least
one** host loaded from the mounted inventory. It dials nothing — **per-host**
reachability is reported through `ListHosts`/`GetHostInfo`, which mark an
unreachable host `NotReady` without failing the whole provider. A registry that
currently fronts zero hosts (e.g. a malformed inventory the watcher has not
reconciled yet) reports a **retryable** not-ready so the manager re-checks once the
inventory is valid. The single-host `Validate` path is unchanged.

## Host inventory-sync controller (operator side)

The **`Host` controller** (`internal/controller/host_controller.go`) is the
operator half of ADR-0007 D1's "brain in the operator": it turns each `Host` CR's
admin-authored desired inventory into **observed state** by asking the clustered
provider what the host can do right now, and writing that into `Host.status` (D3).
It is a **read-only, status-only** sync — it never mutates a `Host` spec and holds
**no finalizer** (there is no external resource to clean up here).

### The reconcile loop

Per `Host`, one reconcile:

1. **Resolve the `Provider`** named by `spec.providerRef` (namespace defaults to
   the Host's). Missing → `health=Unknown`, `Ready=False` (`ProviderUnavailable`),
   short-backoff requeue. Never a hard error.
2. **Require a clustered provider.** If the Provider is not `topology: cluster`
   (D9), short-circuit *before* any RPC — a single-host provider cannot answer the
   inventory RPC — with `health=Unknown`, `Ready=False` (`ProviderNotClustered`).
3. **Obtain the provider client** through the same manager-side resolver the
   VirtualMachine controller uses (no new provider path). A not-ready runtime
   (no endpoint / phase ≠ Running) surfaces here → `ProviderUnavailable`, backoff.
4. **Call `GetHostInfo(host.Name)`** — the *cheap single-host refresh*, not a full
   `ListHosts` poll. The host id is the `Host` CR name.
5. **Map the result into `Host.status`** and stamp `lastHeartbeatTime`.

`GetHostInfo` outcomes map to health + the `Ready` condition:

| Outcome | `status.health` | `Ready` condition (reason) |
|---------|-----------------|----------------------------|
| host `Ready` | `Ready` | `True` (`HostReady`) |
| host `NotReady` | `NotReady` | `False` (`HostNotReady`) |
| health unspecified | `Unknown` | `Unknown` (`HostHealthUnknown`) |
| `Unimplemented` (provider not clustered) | `Unknown` | `False` (`ProviderNotClustered`) |
| host id not in inventory (`NotFound`) | `Unknown` | `False` (`HostNotFound`) |
| transient / unreachable | `Unknown` | `False` (`ProviderUnavailable`) |

A bad or unreachable provider is **always** recorded on status and requeued —
never a crash-loop.

### Heartbeat cadence

- **60 s** steady-state requeue after a successful sync (and for config-level
  states that will not self-heal faster, e.g. a non-clustered reference), keeping
  `status.lastHeartbeatTime` live so a stale Host is visible.
- **15 s** shorter backoff after a transient failure, so a Host recovers promptly
  once its provider returns.

These are `RequeueAfter` intervals (independent of the Host watch), so the
heartbeat runs even though the controller ignores its own status writes (it
watches on generation change, avoiding a self-trigger loop). They reflect the real
per-host probe cost, not artificial tight loops.

### What `Host.status` now carries

`health`, `allocatableCPU`, `allocatableMemoryMiB`, `allocatableStorageBytes`,
`cpuModel`, `cpuFeatures`, `machineTypes`, `emulatorVersion`, `lastHeartbeatTime`,
`observedGeneration`, and a `Ready` condition (which carries its own
`observedGeneration`). `boundVMs` is **deliberately left `0`** in this slice: it
is derived from `VirtualMachine.status.placement.host`, which does not exist yet
(a later ADR-0007 slice owns it).

So `kubectl get hosts` shows a live **Health** column (alongside Pool /
Schedulable / Age), and `kubectl describe host` / `-o yaml` shows the synced
capacity, CPU, and machine-type facts:

```
$ kubectl get hosts
NAME          HEALTH     POOL      SCHEDULABLE   AGE
host-alpha    Ready      pool-a    true          5m
host-bravo    NotReady   pool-a    true          5m
```

Security: `Host.status` carries capacity/health only, **never** connection secrets
(ADR-0007 Security), and the controller writes **only** the status subresource
under a least-privilege `hosts/status` grant (no `hosts` spec write, no
finalizers).

### What is deliberately NOT wired yet

The operator now **calls** `GetHostInfo` to sync `Host.status` (see *Host
inventory-sync controller* above); the remaining pieces are still rendered
separately, under their own reviews:

- **No host-targeted placement.** Host-targeted `Create`/`Migrate` (via
  `target_host_id`), the scheduler, and `VMHostMigration` land in later PRs; the
  connections exist and reconcile but no create/migrate routes to a chosen host.
- **No webhook.** The `topology: cluster` validating webhook (ADR-0007 D2) is a
  later PR.

Security posture: the rendered object is an `Opaque` `Secret` (so connection
material stays out of namespace-readable config), owned by the Provider and GC'd
with it; provider pods still hold **no** Kubernetes RBAC and no projected API
token (#297); and the manager writes **only** the one `<provider>-hosts` Secret it
owns (it never gains secret-delete). Inlining the material **duplicates** it
(source credential Secret → rendered inventory Secret) — a deliberate #297
tradeoff: the no-API-access invariant requires the provider to read connection
material from a file, so the operator copies it there once. Both Secrets share the
same protection boundary (`Opaque`, owner-referenced, namespace-scoped), and the
render path never logs, stores in Status, or events any credential value.

## What "clustered" does not mean (yet)

- **No automatic HA / failover.** v1 detects and surfaces host-down and supports
  operator-initiated evacuation, but does **not** auto-restart VMs elsewhere.
  Without fencing/STONITH that would risk split-brain disk corruption; automatic
  HA is a deferred future ADR (ADR-0007 D8/P5).
- **No scheduler or migration in this slice.** This PR ships the `Host` and
  `HostPool` CRDs only. The inventory-sync controller, the filter+score scheduler,
  `target_host_id` on create, and live migration land in later ADR-0007 slices.
