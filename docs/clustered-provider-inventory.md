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
> `pool-info`) and advertises `supports_clustering = true`, the operator-side
> **inventory-sync controller** that *calls* those RPCs to populate `Host.status`,
> the **pure filter+score placement scheduler** (`internal/scheduler`) that
> chooses a host from that inventory, and — **new in this slice** — the
> **VirtualMachine-controller wiring** that *calls* that scheduler on the clustered
> create path: it binds a VM to a `HostPool` host, sends the choice as
> `target_host_id`, and records `status.placement` **after** the provider confirms
> the VM (honesty-first), and — closing out P1's admission guards — the
> **`vprovider.kb.io` validating webhook** that enforces the topology×type rule at
> admission (ADR-0007 D2): `topology: cluster` is accepted only on `type: libvirt`
> and rejected on every other type. Still to come: host→host migration (later
> P1/P2 slices).

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
meaningful for hypervisors with no native cluster manager (libvirt first).

**This is now enforced at admission (ADR-0007 D2).** The `vprovider.kb.io`
validating webhook (`api/infra.virtrigaud.io/v1beta1/provider_webhook.go`) rejects
`spec.topology: cluster` on any provider type **except** the allow-listed ones.
The allow-list is **`libvirt` only** today; `cloudhypervisor` joins it when that
type is added (ADR-0007 P4). `topology: single` and the empty/unset value (which
the apiserver defaults to `single`, D9) are accepted for **all** types, so today's
providers are byte-for-byte unaffected. A rejected write returns a field-scoped
`spec.topology` invalid error that names the offending type and the allowed
type(s), so an operator can self-correct. The webhook guards both `create` and
`update` — including a patch that flips an existing `vsphere`/`proxmox` provider to
`cluster`.

#### Enabling the webhook via Helm

The webhook is **off by default** (`webhooks.enabled: false`); default installs are
byte-for-byte unaffected and render **zero** webhook resources. The manager
registers the webhook only when started with `--webhook-cert-path` (i.e. when
serving certs are mounted); with no certs it skips registration and starts exactly
as before — so single-host installs stay unaffected even with the chart's webhook
templates present.

To turn it on, enable the master switch and pick a certificate source:

```bash
# Self-signed (default source) — zero extra setup; the chart generates the cert.
helm upgrade --install virtrigaud oci://.../virtrigaud \
  --set webhooks.enabled=true
```

With `webhooks.enabled=true` the chart renders (for the default `self-signed`
source): a `kubernetes.io/tls` **Secret** (`virtrigaud-webhook-certs`), the webhook
**Service** (`<release>-webhook`), and the **`ValidatingWebhookConfiguration`**
(`vprovider.kb.io` only — there are no mutating or conversion webhooks). The
manager Deployment gains the `--webhook-cert-path` flag, the cert volume mount, and
the `9443` container port.

**The self-signed default is now coherent.** The chart generates the CA + serving
cert **once** and reuses the **same** CA for both the serving Secret's `ca.crt` and
the `ValidatingWebhookConfiguration` `caBundle`, and the serving cert's SANs cover
the webhook Service DNS (`<svc>.<ns>.svc` and `<svc>.<ns>.svc.cluster.local`). So
the API server trusts the webhook's TLS out of the box. **Upgrade behaviour:** on
`helm upgrade` the chart reuses the existing serving Secret (via `lookup`) instead
of rotating it, so an in-place upgrade does not open a `failurePolicy: Fail`
admission gap. (A first install, or `helm template` with no cluster, generates a
fresh coherent cert.)

Certificate `source` options (`webhooks.certificates.source`):

| Source | What you provide | What the chart renders |
|---|---|---|
| `self-signed` (default) | nothing | CA + serving-cert Secret + coherent `caBundle` |
| `cert-manager` | cert-manager installed + an `Issuer`/`ClusterIssuer` (named in `webhooks.certificates.certManager`) | a cert-manager **`Certificate`** (its `secretName` == the mounted serving Secret) and `cert-manager.io/inject-ca-from` pointing at that Certificate; cert-manager injects the `caBundle` |
| `manual` | the serving Secret (`secretName`) out-of-band **and** the matching CA in `webhooks.certificates.caBundle` (base64 PEM) | just the `caBundle` you supplied |

> `failurePolicy: Fail` is deliberate and safe here: `Provider` CRs are
> **user-created** (never manager-created), so a fail-closed policy cannot deadlock
> the manager's own startup. The only failure mode to avoid — a rendered webhook
> whose `caBundle` does not match the serving cert — is exactly what the coherence
> fix and the `hack/verify-webhook-render.sh` CI guard prevent.

#### Production / GitOps / banking notes (from the security review)

- **GitOps / Argo CD → use `cert-manager`.** The self-signed default persists its
  cert across upgrades via Helm `lookup`, which only works under `helm
  install/upgrade` with an identity that can read Secrets in the release namespace.
  Under `helm template` render-then-apply (Argo CD, `helm template | kubectl
  apply`) `lookup` is always empty, so **every sync regenerates** the CA + serving
  cert + `caBundle` — churning the Secret and briefly reopening a `failurePolicy:
  Fail` admission gap while the manager reloads the projected cert. `cert-manager`
  keeps `caBundle`↔cert coherence server-side (cainjector), independent of render
  idempotency, and never writes the private key into Helm release history — so it
  is the recommended posture for production and banking.
- **Renames need a Secret delete.** The self-signed reuse-on-upgrade path keeps the
  cert's original SANs. If you change `nameOverride`/`fullnameOverride`/`secretName`
  (or switch `certificates.source`) on an existing release, delete the serving
  Secret first so the chart mints a fresh, correctly-SAN'd cert; otherwise the API
  server dials the new Service DNS against a stale-SAN cert and admission fails.
- **Run the manager HA when webhooks are on.** With `failurePolicy: Fail` the
  manager sits on the Provider-admission path, so a single replica is a single point
  of failure for admission during node loss/eviction. Set `manager.replicaCount: 2`
  plus a PodDisruptionBudget and anti-affinity (the webhook server is not
  leader-gated, so every replica serves it — 2+ gives real webhook HA).
- **Misconfigurations fail fast.** The chart `fail`s the render on an unknown
  `certificates.source` or `source: manual` with an empty `caBundle`, so a one-line
  values typo is caught at `helm template` / CI time rather than bricking admission
  in the cluster.

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
`observedGeneration`). `boundVMs` is **deliberately left `0`** in this slice: the
binding controller now writes `VirtualMachine.status.placement.host`, but
aggregating that into a per-host count on `Host.status.boundVMs` is the Host
inventory controller's job and lands in a later ADR-0007 slice.

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

### What is still deferred to later slices

The operator now **calls** `GetHostInfo` to sync `Host.status` (see *Host
inventory-sync controller* above) **and** calls `Schedule` on the clustered create
path (see *Placement binding* below); the remaining pieces are still rendered
separately, under their own reviews:

- **The scheduler is wired for create.** The VirtualMachine controller now calls
  the pure filter+score scheduler (`internal/scheduler`) when a `topology: cluster`
  provider's VM is created, sends the chosen host as `target_host_id`, and writes
  `VirtualMachine.status.placement` **after** `Create` confirms (see *Placement
  binding* below). Still out of scope here: **rescheduling** an already-created VM
  and **`Migrate` / `VMHostMigration`**, which land in later PRs.
- **Admission webhook — shipped.** The `vprovider.kb.io` validating webhook
  (ADR-0007 D2) now rejects `spec.topology: cluster` at admission on every type
  except the allow-listed `libvirt` (see *The `topology` discriminator* above,
  including the serving-cert requirement).

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

## Placement scheduler (operator side)

The **placement scheduler** (`internal/scheduler`) is the operator-side "brain owns
the decision" from ADR-0007 D1/D4: given a VM's resource request, its optional
`VMPlacementPolicy`, the `HostPool` policy, and the pool's `Host`s (with the live
`status` the inventory-sync controller populates), it chooses **one host**. It is a
**pure function** — no Kubernetes client, no I/O, no clock, no package state — so it
is unit-testable without a cluster and deterministic on re-run:

```go
func Schedule(req scheduler.Request) (scheduler.Result, error)
```

`Result` carries the chosen `HostID` and a human-readable `Reason` (a decision
trace destined for `status.placement.reason`); a no-fit is a typed
`*NoFeasibleHostError` (matching `ErrNoFeasibleHost` via `errors.Is`) that lists,
per host, which filter eliminated it — so "unschedulable" is always explainable.

### Filter, then score

**Filter** reduces the candidates to the feasible set — every check is a HARD
constraint that must hold:

| Filter | Rule |
|--------|------|
| Health + cordon | `status.health == Ready` **and** `spec.schedulable == true` (a drained or NotReady host is never a target). |
| Capacity fit | `allocatableCPU` and `allocatableMemoryMiB` ≥ the request **after** the pool's overcommit ratios. |
| Hard host list | `VMPlacementPolicy.Hard.Hosts` allow-list / `Hard.ExcludedHosts` deny-list (a host id is its `Host` CR name). |
| Hard node-selector | `Hard.NodeSelector` matched against `Host.spec.labels`. |
| Storage/network visibility (D6) | the VM's required pools/networks as a `Host.spec.labels` requirement: `storage.virtrigaud.io/pool-<name>` / `net.virtrigaud.io/<name>` == `"true"`. |
| Required CPU features | `ResourceConstraints.RequiredFeatures` ⊆ `status.cpuFeatures`. |
| Required machine type | the caller-resolved machine type ∈ `status.machineTypes`. |
| Minimum-per-host resources | `ResourceConstraints.Min{CPU,Memory,DiskSpace}PerHost` floors against the host's raw allocatable. |
| Strict host (anti-)affinity | `HostAffinity` / `HostAntiAffinity` with `scope: strict` (the anti-affinity per-host VM cap defaults to 1 when `maxVMsPerHost` is unset). |
| Required VM (anti-)affinity | `VMAffinity` / `VMAntiAffinity` `requiredDuringScheduling` evaluated against the already-placed VMs. |

**Score** ranks the survivors and picks the best, deterministically:

1. **Soft preferences** (the primary axis): `Soft.*` constraints, `PreferredFeatures`, and **preferred** host/VM (anti-)affinity apply as a bonus/penalty that ranks a preferred host above raw packing.
2. **Strategy** (`HostPool.spec.strategy`): **Spread** favors the most free capacity, then the fewest bound VMs; **BinPack** favors the tightest host that still fits.
3. **Host id** ascending — the final tie-break, so the same inputs always yield the same host regardless of candidate order.

Overcommit ratios and bound-VM counts feed both the capacity fit and the score:
`allocatable` (the host TOTAL the inventory layer reports) is multiplied by the
pool's overcommit ratio to get the bookable capacity, and bound-VM counts come from
the already-placed set the caller supplies (the operator owns the binding, D1).
This slice deliberately does **not** subtract per-VM reservations from `allocatable`
— where and how running-VM reservations yield true free capacity is ADR-0007 **open
question 5** — so the bound-VM count is the load-aware secondary signal, and a
provider that reports live (already-net) `allocatable` makes the fit exact.

### Idempotent on re-run

When the VM is already bound (its current `status.placement.host`) and that host
still passes every filter, the scheduler **re-selects it without scoring** — a
re-reconcile never churns a healthy placement, even if another host now scores
better. The one exception is a **drained** host: a bound host that is now cordoned
(`schedulable=false`) or `NotReady` fails the filter and the VM is re-placed
elsewhere, which is exactly what an operator-initiated host drain needs.

### How each `VMPlacementPolicy` construct maps (and what is deferred)

The scheduler **reuses `VMPlacementPolicy`** as its input language (ADR-0007 D4). It
honors the constructs that map cleanly onto the flat `Host` + labels model and
**documents** — rather than silently ignoring — the ones that do not (it invents no
new API):

- **Honored as hard filters:** `Hard.Hosts` / `Hard.ExcludedHosts` / `Hard.NodeSelector`; `ResourceConstraints.RequiredFeatures` and `Min*PerHost`; strict `HostAffinity` / `HostAntiAffinity`; required `VMAffinity` / `VMAntiAffinity`.
- **Honored as soft scores:** `Soft.Hosts` / `ExcludedHosts` / `NodeSelector`; `ResourceConstraints.PreferredFeatures`; preferred `HostAffinity` / `HostAntiAffinity` and preferred `VMAffinity` / `VMAntiAffinity` (each term's `weight`).
- **Deferred (documented, not faked):** the vSphere/Proxmox external-orchestrator vocabulary carried in `placement_json` — `Hard.Clusters` / `Datastores` / `Folders` / `ResourcePools` / `Networks` / `Zones` / `Regions` / `Tolerations` and the cluster/datastore/zone/application (anti-)affinity rules (the flat P1 host model has no such topology; a rack/zone label goes through `NodeSelector`); `ResourceConstraints.Max*Utilization` (no live-utilization telemetry in `Host.status` yet); and all of `SecurityConstraints` (secure-boot / TPM / encryption / NUMA / isolation / trust are not reported by `Host.status` — label the hosts and use `NodeSelector`). VM affinity `TopologyKey` / `Namespaces` / `NamespaceSelector` are treated as host-level co-location; the caller supplies the already-placed VM set it wants considered. A rule's `scope` is treated as a HARD filter **only** when it is explicitly `strict`; any other value (including the empty default) is a soft preference, so a mis-set scope can never accidentally make every host infeasible.

A malformed input the admin must fix — an unparseable overcommit ratio, a malformed
affinity label selector — is returned as an ordinary error (distinct from a no-fit),
never a panic.

### Wired — the VirtualMachine controller calls `Schedule` on create

The scheduler stays a **pure function**, but the operator now calls it. The
VirtualMachine controller's create path, for a `topology: cluster` provider,
resolves the scheduler's inputs (the provider's single `HostPool`, its candidate
`Host`s with live `status`, the optional `VMPlacementPolicy`, and the pool's
already-placed VMs), calls `Schedule`, sends the chosen host as `target_host_id`,
and writes `VirtualMachine.status.placement` **after** `Create` confirms (see
*Placement binding* below). A single-host / thin-client provider skips all of this
— its create path is byte-for-byte unchanged. The `Migrate` / `VMHostMigration`
path is still a later slice.

## Placement binding: `target_host_id` on the wire + `status.placement` on the VM

The scheduler decides; the **binding** is how that decision reaches the provider
and how it is durably recorded. It has two halves — a **contract half** (shipped
in this slice: the wire field, the status field, and the libvirt provider honoring
the wire field) and an **operator half** (the binding controller PR that follows).

### The wire: `CreateRequest.target_host_id` (contract half — shipped)

Host-targeted placement is an **explicit** field on the create RPC —
`CreateRequest.target_host_id` (field 10) — deliberately **not** folded into
`placement_json`. `placement_json` stays for the external-orchestrator hints a
thin-client provider (vSphere/Proxmox) forwards to vCenter/pve; `target_host_id`
is the operator scheduler's instruction to a clustered provider, so the two never
overload one field (ADR-0007 D4). It is **additive and backward-compatible**:
existing single-host and thin-client providers ignore it (and, in single-host
topology, never receive it).

- **libvirt, `topology: cluster`** — `Create` routes onto the named host: it
  borrows that host's connection from the N-host registry (`ConnFor`, the same
  per-borrow **lease** `GetHostInfo` uses), runs the existing create pipeline
  (storage → cloud-init → `virsh define`) over **that** host's libvirtd, and
  **always releases the lease** — on the error path too — so a concurrent
  host-remove drains gracefully rather than severing the create (D3
  non-severing). An **empty** `target_host_id` in clustered mode is a **typed
  error**, never a silent default-host create: the operator must schedule the VM
  first (honesty-first, D9).
- **libvirt, `topology: single`** — `target_host_id` is **ignored**; the
  single-host create path is byte-for-byte unchanged.
- **vSphere / Proxmox / mock** — thin-client and single-host providers ignore the
  new field; it simply compiles through their unchanged `Create`.

### The record: `VirtualMachine.status.placement` (now written by the binding controller)

`status.placement` is the **durable source of truth** for where a VM runs
(ADR-0007 D3):

```go
type PlacementStatus struct {
    Host              string       // the bound Host (CR name) — the durable truth
    Pool              string       // the HostPool it was scheduled into
    LastScheduledTime *metav1.Time // when the scheduler last (re)bound it
    Reason            string       // the scheduler's decision trace
}
```

The field is **additive** — a VM with no operator-owned placement (every
single-host / thin-client VM, and every clustered VM before its first confirmed
create) omits it entirely, so existing VM status is unchanged. **Honesty-first
(D3): the operator writes it ONLY after the provider confirms** the VM is on that
host (create success, later a migration reporting `done`) — never speculatively —
so `status.placement.host` never claims a host the provider has not accepted the
VM on. The VirtualMachine controller writes it after a confirmed clustered create.

### What the binding controller wires

The **operator half** now lands: on the create path for a `topology: cluster`
provider, the VirtualMachine controller resolves the candidates as the hosts of
the provider's `HostPool` (**v1 assumes exactly one HostPool per clustered
provider** — zero or many is a typed configuration error surfaced on the VM's
`Provisioning=False` condition, never a silent guess; multi-pool selection is a
later follow-up), feeds them to `Schedule`, sends the chosen host as
`target_host_id`, and writes `status.placement` **after** `Create` confirms (D3
honesty-first — a failed `Create` leaves `status.placement` unwritten). The
scheduler's idempotent re-selection (D4) is fed for free by passing the VM's
current `status.placement.host` as the binding, so a re-reconcile of a still-feasible
VM re-selects the same host without churn.

**Scheduler inputs wired vs deferred.** The VM's CPU/memory request (from the
`VMClass`, reusing what the create request already resolved) and its **required
networks** (each resolved network attachment's libvirt network — the network name,
or the bridge when no name is set — as a D6 `net.virtrigaud.io/<name>` host-label
constraint) are wired. **`RequiredStoragePools` and `RequiredMachineType` are
deliberately left empty** (a `// TODO(ADR-0007 D6)` in the controller): a VM's
disks carry no storage-pool name (`DiskSpec.StorageClass` is a Kubernetes
StorageClass, not a libvirt/NFS pool) and a `VMClass` carries no machine type
(only firmware), so there is no unambiguous mapping yet — and the scheduler treats
an empty required-set as "no constraint", which is the honest, correct behavior
until those inputs exist. Inventing either mapping would be a scheduling bug, not a
feature.

## What "clustered" does not mean (yet)

- **No automatic HA / failover.** v1 detects and surfaces host-down and supports
  operator-initiated evacuation, but does **not** auto-restart VMs elsewhere.
  Without fencing/STONITH that would risk split-brain disk corruption; automatic
  HA is a deferred future ADR (ADR-0007 D8/P5).
- **No rescheduling or migration yet.** Placement binding at **create** is now
  wired end-to-end: the VirtualMachine controller schedules a clustered VM onto a
  `HostPool` host, sends `target_host_id`, and writes `status.placement` after the
  provider confirms, and the `vprovider.kb.io` validating webhook now enforces the
  topology×type rule at admission (ADR-0007 D2). What is still **not** wired is
  moving an *already-created* VM — rescheduling a bound VM to a different host, and
  host→host `Migrate` / `VMHostMigration`. Those land in later ADR-0007 slices.
