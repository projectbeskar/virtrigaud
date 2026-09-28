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
> and rejected on every other type. **ADR-0007 Addendum A, slice 1** adds
> post-create routing: every per-VM call carries the VM's host, `Describe` and
> `Delete` are routed to the bound host (the delete owner-checked), the create
> records its target host in `status.placement.pendingHost` before calling the
> provider, and a `Host` cannot be deleted while a VM uses it. **Slice 2** routes
> `Power` and `Reconfigure` to the bound host, owner-checked, and releases a
> pending host whose create hit a name conflict (the host is excluded and the VM
> re-scheduled). **Slice 3** routes the snapshot family, `Clone` (onto the
> source VM's host, stamped with its target VM), `GetDiskInfo` and the host-side
> `ExportDisk` (s3 / nfs), all owner-checked, and host-encodes task references
> so `TaskStatus` is routed too — see
> [Post-create routing](#post-create-routing-adr-0007-addendum-a).
> **Slice 4** lists VMs across every host (`VMInfo.host_id`,
> `ListVMsResponse.unreachable_host_ids`) and adopts from a clustered provider,
> keyed on (host, id) — see [Listing and adoption](#listing-and-adoption-slice-4).
> `topology: cluster` stays **experimental** until Addendum A slice 5. Still to
> come: host→host migration (P2).

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
| `spec.providerRef` | The clustered `Provider` that owns this pool. A **namespace-local** reference (`name` only): the pool must be in the Provider's namespace. |
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
| `providerRef` | The clustered `Provider` that executes on this host. **Namespace-local** (`name` only): the Host must be in the Provider's namespace. |
| `poolRef` | The `HostPool` this host joins (membership is declared by the host, ADR-0007 D3). |
| `endpoint` | Connection URI, validated at admission (see below). Only `qemu+ssh://[user@]host[:port]/system`, `qemu+ssh://[user@]host[:port]/session`, or (future host agent) `grpc://host:port`. |
| `credentialSecretRef` | Optional per-host credential override; defaults to the Provider's. **Namespace-local**: the Secret must be in the Host's (= the Provider's) namespace. |
| `labels` | Placement facts consumed as **hard scheduling constraints** (ADR-0007 D6): storage-pool visibility, network/bridge visibility, zone/rack. |
| `schedulable` | Defaults `true`; set `false` to cordon (no new placement, existing VMs stay). |

`status` (populated later by the inventory-sync controller) reports `health`
(`Ready`\|`NotReady`\|`Unknown`), allocatable CPU/memory/storage, CPU
model/features, machine types, emulator version, bound-VM count, and last
heartbeat. It carries capacity and health only — **never connection secrets**
(ADR-0007 Security).

### Same-namespace model (security)

A `HostPool`, its `Host`s, every credential `Secret` they use, and the clustered
`Provider` they name **all live in one namespace — the Provider's**. The
references on `Host` / `HostPool` (`providerRef`, `poolRef`,
`credentialSecretRef`) are namespace-local and have no `namespace` field, and the
operator enforces the same rule on its side:

- the inventory render lists `Host`s with `client.InNamespace(<provider ns>)`,
  so a `Host` created in any other namespace is ignored — it can neither enrol
  into another team's Provider nor inherit that Provider's SSH identity;
- every credential `Secret` is read from the Provider's namespace only. A
  clustered Provider whose own `spec.credentialSecretRef.namespace` names a
  **different** namespace does not get that Secret read: hosts that would fall
  back to it are skipped and `HostCredentialsReady=False` with reason
  `CredentialRefNamespaceRejected` (the manager's cluster-wide Secret access is
  never used to copy SSH material out of a namespace);
- the `Host` inventory-sync controller resolves `spec.providerRef` in the Host's
  own namespace, and the VirtualMachine controller looks up `HostPool`s / `Host`s
  in the **resolved Provider's namespace** (a VM may live elsewhere and reference
  the Provider cross-namespace; its placement still uses the Provider's pool).

Why: the inventory Secret carries each host's SSH private key, and the host
endpoint is ultimately handed to the hypervisor host. Letting a namespace-scoped
author steer either across namespaces turned "can create a `Host`" into "can read
another namespace's Secrets" and "can run commands on another team's hypervisor".

### Endpoint validation (security)

`Host.spec.endpoint` is validated by the CRD schema (`minLength: 1`,
`maxLength: 512`, and a pattern) and **re-validated by the provider** for every
entry it loads from the mounted inventory, so a hand-edited inventory Secret
cannot bypass admission. Accepted:

```
qemu+ssh://[user@]host[:port]/system
qemu+ssh://[user@]host[:port]/session
grpc://host:port                          # reserved for a future host agent
```

`user` is `[A-Za-z0-9._-]+`; `host` is a DNS name, an IPv4 address, or a
bracketed IPv6 address (`[2001:db8::1]`). Query strings, fragments,
percent-encoding, and any other path are rejected. The libvirt provider
additionally shell-quotes every argument it sends to the host over SSH and only
ever forwards `system` / `session` as the remote `virsh -c` instance, so even a
released single-host `Provider` endpoint with an unusual path cannot inject a
command.

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
that, on top of these totals (see *Committed capacity* under *Placement
scheduler* below). The field name follows the wire contract; read it as
"capacity the scheduler starts from".

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

**`spec.topology` is immutable (ADR-0007 Addendum A).** VMs record where they run
(`status.placement`) under one topology, and every per-VM call is routed and
owner-checked by it, so a flip would send a placed VM's calls down the wrong path
(`cluster` → `single` would even route its delete to the un-owner-checked
single-host path). The CRD rejects any change with a CEL transition rule
(`self == oldSelf`), independent of whether the webhook is enabled, and the
webhook's `update` check rejects it too. The apiserver compares the defaulted
values, so a `Provider` created before the field existed (read back as `single`),
or a client that omits the field on update, keeps working; only a real change is
refused. To change topology, create a new `Provider`. If a VM nevertheless records
a clustered placement while its Provider is not clustered (an object edited before
this rule), the operator fails it closed: no provider call is made for it, it
shows `Ready=False` (`PlacementTopologyMismatch`), and its finalizer is kept unless
`virtrigaud.io/force-delete: "true"` is set.

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

1. The **Provider controller** gathers the `Host` CRs **in the Provider's
   namespace** whose `spec.providerRef` names a `topology: cluster` Provider and
   renders them — metadata **and inlined SSH credentials** — into a
   **versioned-schema document**.
2. That document is written to a **Secret** (never a ConfigMap) named
   **`<provider>-hosts`**, **owner-referenced** to the Provider, in the provider's
   namespace. Re-renders are deterministic (hosts sorted by id), so an unchanged
   inventory produces no Secret write. Host ids are unique by construction (one
   namespace); a duplicated id is never resolved by "first wins" — every entry
   carrying it is skipped (operator side, Warning event
   `HostInventoryEntrySkipped`) and treated as unroutable (provider side).
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
   Secret is used — always from the Provider's namespace (the ref has no
   namespace field).
2. **Provider default otherwise.** Otherwise the Provider's own
   `spec.credentialSecretRef` is used, in the Provider's namespace (the same
   Secret the single-host credential mount uses). If that ref names a
   **different** namespace, it is **not read**: the host is skipped with reason
   `CredentialRefNamespaceRejected` (see *Same-namespace model* above).

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
  (reason `CredentialsUnresolved`, or `CredentialRefNamespaceRejected` when a
  host was skipped because its credential reference points outside the
  Provider's namespace), listing the skipped **host ids** and a coarse
  **reason** (e.g. `credential secret default/foo not found`);
- a **Warning event** (`HostCredentialsUnresolved`) is emitted on the Provider;
- a structured **log** line records the same.

A host whose `endpoint` fails re-validation or whose id is duplicated is skipped
the same way, but surfaced with a separate Warning event
(`HostInventoryEntrySkipped`) and log line rather than the credential condition
— the endpoint value itself is never echoed.

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
It never mutates a `Host` spec. Its one metadata write is the **in-use
finalizer** `host.infra.virtrigaud.io/in-use` (ADR-0007 Addendum A, A1): a `Host`
is not deleted while any VirtualMachine of its Provider names it in
`status.placement.host` or `status.placement.pendingHost`, because every per-VM
call for that VM — and the VM's own finalizer cleanup — is routed to it. While a
deletion is blocked the Host shows `Ready=False` (`HostInUse`) naming the VMs,
and is re-checked every 30 s; the finalizer is released as soon as no VM uses the
Host. The condition message gives the number of VMs and names at most 10 of them,
so it stays well under the condition-message size limit and does not list every
tenant's VMs on an admin object. The match follows the same-namespace model: a VM
uses this Host only when its `spec.providerRef` resolves to the Host's Provider
(in the Host's namespace), so a same-named Host of another Provider never blocks
it. While a Host is being deleted the scheduler treats it as cordoned, so new VMs
are never placed on it (they would keep re-arming the finalizer).

### The reconcile loop

Per `Host`, one reconcile:

1. **Resolve the `Provider`** named by `spec.providerRef` — always in the Host's
   own namespace. Missing → `health=Unknown`, `Ready=False` (`ProviderUnavailable`),
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
(ADR-0007 Security). The controller writes the status subresource (`hosts/status`)
and, for the in-use finalizer only, the Host object (`hosts` update plus
`hosts/finalizers`); it never changes a Host spec.

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
| Capacity fit | the request fits in what is **free**: `allocatableCPU` / `allocatableMemoryMiB` times the pool's overcommit ratio, **minus** what the VMs bound to, pending on or assumed on the host already hold (see *Committed capacity* below). |
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
2. **Strategy** (`HostPool.spec.strategy`): **Spread** favors the most free (uncommitted) capacity, then the fewest bound VMs; **BinPack** favors the tightest host that still fits.
3. **Host id** ascending — the final tie-break, so the same inputs always yield the same host regardless of candidate order.

### Committed capacity

A host's free capacity is what the pool lets VMs book, minus what VMs already
hold there (ADR-0007 Addendum A, *scheduler accuracy* amendment):

```
free = allocatable × overcommit ratio − committed
```

- **`allocatable`** is the host total the inventory layer reports (see above).
  It does not change when VMs start or stop, so subtracting the operator's own
  VMs counts nothing twice.
- **The overcommit ratio** scales the capacity, never the committed sum. With
  4 physical vCPUs and `cpu: "2.0"`, 8 vCPUs are bookable. If you lower a ratio
  below what is already committed, the host takes no new VM until enough VMs
  leave; no VM is moved.
- **`committed`** is the sum of the footprints of every VirtualMachine of this
  Provider whose `status.placement.host` or `status.placement.pendingHost` names
  the host. A VM belongs to the Provider it is bound through
  (`status.boundProvider`), else to the one its `spec.providerRef` names.
  - VMs in **every namespace** count, not only the new VM's.
  - A VM being **deleted** counts until its VirtualMachine finalizer is gone.
    Once only another controller's finalizer keeps it, it no longer counts, and
    it no longer blocks the Host's deletion.
  - A VM whose Create is still **pending** there counts.
  - A VM naming the host in both fields counts once.
- **Another VM's footprint is its admitted size**, never what its owner merely
  asks for:
  - a created VM counts at `status.currentResources` (the size the provider
    applied), or at its VMClass size when nothing is recorded; editing its
    `spec.resources` or `spec.classRef` changes nothing until a resize is
    admitted (below);
  - a VM whose create is pending counts at the size it was scheduled at: its
    VMClass with any `spec.resources` override applied, recorded in
    `status.placement.pendingResources`. While the create is pending, the CRD
    rejects any change to `spec.classRef` and `spec.resources`. If the VMClass
    itself is edited so that the retried Create would be larger (or would gain
    memory hot-add), the retry is not sent: the VM gets `Placed=False` and
    `Provisioning=False` with reason `PendingSizeGrew` and keeps its pending
    host until the VMClass is restored or the VM is deleted;
  - a VM whose VMClass enables **memory hot-add** counts at its memory
    *ceiling*: 4× its memory, the balloon maximum the libvirt provider gives
    it, which the guest can use at any time. The ceiling is recorded when the
    VM is scheduled (`status.placement.memoryCeilingMiB`, `0` for none), so
    turning hot-add off in the VMClass later does not lower it. A VM scheduled
    before that field existed gets it recorded once from its provider (the
    domain's actual memory maximum, reported by `Describe`), after which the
    VMClass flag no longer matters. A recorded ceiling is lowered, never
    raised, when the provider reports less — after a memory shrink, which
    lowers the domain's maximum with it. CPU hot-add is not counted at its ceiling,
    because extra vCPUs stay offline until a resize (which is checked) brings
    them online;
  - a VMClass in another namespace that the VM's namespace may not use
    (no consumer grant) sizes nothing;
  - every VM counts at least 1 vCPU and 128 MiB.
- **Not counted**: domains on the host that VirtRigaud does not manage, and the
  hypervisor's own use. To keep room for them, set a ratio below 1 (for example
  `memory: "0.9"`) or cordon the host.
- **Affinity is unchanged.** VM (anti-)affinity and the host-anti-affinity VM
  cap still consider only VMs in the new VM's own namespace. Other namespaces'
  VMs take up capacity; their labels are never matched.
- **Trusted status.** The accounting trusts `status.boundProvider` and
  `status.placement`, which only the operator writes. Never grant tenants
  write access to `virtualmachines/status`.

**Resizing a VM up is checked too.** When the size a clustered VM asks for
(its VMClass, or its `spec.resources` override) grows its CPU or memory, the
controller checks, under the same per-Provider lock, that the growth fits in
its host's free capacity, not counting the VM's own current size. A memory
grow that stays within a hot-add VM's ceiling is already counted and is not
checked. If it fits, the resize is sent and counted at its new size straight
away. If it does not,
nothing is sent, the VM keeps running at its current size, and it gets
`Reconfiguring=False` with reason `InsufficientHostCapacity`, for example:

```
resizing to 8 vCPU and 8192 MiB exceeds the free capacity of its host kvm-01; the VM keeps
4 vCPU and 8192 MiB and the resize is retried (next check in 30s)
```

The resize is retried after 30 s, 1 min, then every 2 min. Only the resources
that grow are checked. The host's health and cordon do not matter, because a
resize does not move the VM. A VM whose host is not registered as a `Host`, or
whose `HostPool` is missing or belongs to another Provider, cannot be resized
up. Single-host Providers are not affected.

**Shrinking a running VM waits for it to be powered off.** A shrink is never
refused, but on a clustered Provider it is applied only while the VM is off. A
running guest can take back memory that was removed live (the balloon), and a
live vCPU removal can fail, so recording the smaller size early could let
another VM be placed on capacity this one still uses. While the VM runs, nothing is sent, the VM keeps counting at its current
size, and it gets `Reconfiguring=False` with reason `ShrinkPendingPowerOff`.
Power it off (`spec.powerState: Off`, or shut it down from inside the guest)
and the shrink is applied and recorded; set `spec.powerState: On` again to
restart it. If the VM is found off while its spec still says `On`, the shrink
is applied first and the VM is powered on in the next reconcile. VirtRigaud
never powers a VM off by itself to apply a shrink. A change that shrinks one
resource and grows another waits as a whole. Single-host Providers still send
a shrink of a running VM at once; it is applied at the VM's next power cycle
(below).

A VM reported **`Suspended`** (paused, or suspended to RAM by its guest) is not
powered off: it resumes at the size it has. The manager neither powers it on nor
resizes it while it is suspended (`Ready=False/PowerStateUnmanaged`) — it powers
it off only if its `spec.powerState` is `Off` —
and the libvirt provider refuses to change a domain that is active but not
running. New clustered domains are created with guest suspend to RAM and to
disk disabled.

**A change the running VM cannot take waits for its next power cycle.** The
libvirt provider applies each change to the domain's persistent definition
first, then to the running domain. When the running domain cannot take it — a
vCPU count beyond its running maximum, vCPUs that are not hotpluggable, memory
beyond its running balloon maximum, or any memory shrink — the change is kept in
the definition and reported as *restart required*: the VM gets
`Reconfiguring=True` with reason `RestartRequired`, and takes the new size the
next time it is powered off and on (a reboot from inside the guest is not
enough). Until then `status.currentResources` holds, per resource, the larger of
the size it runs with and the size it will boot with — a grow is counted at
once, a shrink only once applied — and the provider is asked again every
2 minutes (or at once after a spec change). A change the provider cannot apply
at all fails (`Reconfiguring=False/ProviderError`); since it may have applied
part of the change, the VM is then counted at the larger of its old and its
requested size, and the call is retried on a per-VM backoff (5 s doubling to
5 min) until one succeeds. The libvirt provider grows the disk before any CPU or
memory change, so a disk grow the host refuses fails the call before anything
else changes. A VM is also never counted below the vCPUs, or the memory
maximum, its provider reports it has.

**A Provider must report the honest Reconfigure result.** A resize is sent to a
clustered Provider only if it reports
`status.reportedCapabilities.supportsHonestReconfigure` (the current libvirt
provider does). Otherwise — an older provider image — the VM keeps its size and
gets `Reconfiguring=False` with reason `ProviderLacksHonestReconfigure`.

**Detaching (orphan-on-delete) needs the Provider's permission.** A VM detached
with `virtrigaud.io/orphan-on-delete` keeps running but stops counting. So a VM
in another namespace than its clustered Provider is detached only when the
Provider carries `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"`.
Otherwise its deletion is held with `Ready=False/OrphanOnDeleteNotAllowed`
until the administrator allows it or the annotation is removed (see
[`vm-provider-binding.md`](vm-provider-binding.md)). The same caution applies
to `virtrigaud.io/force-delete`: if it releases a VM whose provider `Delete`
failed (for example the host was unreachable), the domain may keep running
outside the accounting. That cannot be done on purpose, since the `Delete`
has to fail, but after a force-delete check the host and cordon it, or lower
its overcommit ratio, until the leftover domain is gone.

**No per-tenant quota.** Nothing limits how much of a shared clustered Provider
one consumer namespace may take: hosts fill first come, first served. If you
share a Provider across tenants, limit each tenant's VM count with a Kubernetes
`ResourceQuota` (for example `count/virtualmachines.infra.virtrigaud.io`), or
give tenants separate Providers and HostPools. A per-consumer quota is a
planned follow-up.

**Many VMs created at once.** The VM controller reconciles several VMs in
parallel, and its cache may not show another reconcile's `pendingHost` write
yet. So each clustered create takes a per-Provider lock and reads what is
committed. It adds the placements other reconciles have picked but not yet
recorded (an in-process *assume* cache). Then it schedules, records its own
pick and releases the lock before it writes `pendingHost`. Two VMs never book
the same capacity, and two VMs with mutual hard anti-affinity never land on the
same host. A picked placement stops being tracked once the cache shows its
`pendingHost` or `host`, and at the latest after 2 minutes (an admitted resize:
once `status.currentResources` records it, at the latest after 5.5 minutes,
the longest a `Reconfigure` call and its status write may take). The manager runs
under leader election, so one in-process cache is authoritative.

**When nothing fits**, the VM gets `Placed=False` and `Provisioning=False`, both
with reason `Unschedulable`, and a message such as:

```
no feasible host in pool "pool-a": no feasible host: 0 of 3 candidate host(s) passed the filters
[insufficient CPU capacity: 3]; requested 4 vCPU and 4096 MiB, which exceeds the free capacity
of every candidate host
```

The message states only the VM's own request. It never shows how much is
committed or free on a host, because those figures come from other tenants'
VMs; neither does the success trace in `status.placement.reason`.
Administrators find the per-host arithmetic in the manager log (verbosity 1)
and in the gauges below. The message never names a host or another VM, and its
size does not grow with the pool. The VM is retried after 30 s, then 1 min, then every 2 min,
until capacity frees up. The controller does not watch Hosts or other VMs, so a
freed host is noticed within 2 minutes.

**Metrics.** `virtrigaud_host_committed_cpu` and
`virtrigaud_host_committed_memory_mib` give each Host's committed sum, labelled
`provider` (`namespace/name`) and `host`. The Host controller refreshes them
about once a minute and removes them when the Host is deleted or moved to
another Provider. Compare them with `Host.status.allocatableCPU` /
`allocatableMemoryMiB` times the pool ratio to see how full a host is.

These gauges are **for administrators**: they show how full each host is,
across tenants. The manager serves `/metrics` over plain HTTP when
`--metrics-secure=false` (the chart's current default). Enable the chart's
NetworkPolicy (`networkPolicy.enabled`, which admits scrapes only from
`networkPolicy.monitoringNamespaceSelector`) or run the manager with
`--metrics-secure`.

The capacity check holds the Provider's lock only while it reads the informer
cache and schedules. Every API write (a condition, `pendingHost`) happens
after the lock is released, each with a time limit, so a slow API server does
not stall other VMs. A VM that cannot get the lock within 5 seconds is retried
about a second later.

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

The clustered create runs the same create core as the single-host path, so the
libvirt domain-ownership rule (`CreateRequest.owner`, field 11 — an existing
same-named domain on the target host is bound only if it is stamped with the
requesting VirtualMachine's UID) applies on the chosen host too; see
[`libvirt-domain-ownership.md`](libvirt-domain-ownership.md).

### The record: `VirtualMachine.status.placement` (now written by the binding controller)

`status.placement` is the **durable source of truth** for where a VM runs
(ADR-0007 D3):

```go
type PlacementStatus struct {
    Host              string       // the bound Host (CR name) — the durable truth
    PendingHost       string       // the Host an unconfirmed Create is aimed at (Addendum A, A2)
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
the provider's `HostPool` — looked up in the **Provider's namespace** (not the
VM's), and only hosts that are in that pool **and** name that Provider — (**v1 assumes exactly one HostPool per clustered
provider** — zero or many is a typed configuration error surfaced on the VM's
`Provisioning=False` condition, never a silent guess; multi-pool selection is a
later follow-up), feeds them to `Schedule`, sends the chosen host as
`target_host_id`, and writes `status.placement.host` **after** `Create` confirms
(D3 honesty-first — a failed `Create` never writes the binding). The
scheduler's idempotent re-selection (D4) is fed for free by passing the VM's
current `status.placement.host` as the binding, so a re-reconcile of a still-feasible
VM re-selects the same host without churn.

**The attempted host is recorded first (Addendum A, A2).** Before `Create` is
sent, the chosen host is written to `status.placement.pendingHost` (with the pool
and the decision trace) through a **resourceVersion-checked** status update. If
that write loses a race, the reconcile requeues **without** creating and without
re-applying its scheduler choice; the next reconcile re-reads the VM. Once the
write is durable, `Create` goes to `pendingHost`; on success the host moves into
`status.placement.host` and `pendingHost` is cleared. A retry — after a lost
status write, or a `Create` that ran past its deadline after `virsh define` —
**reuses `pendingHost` as-is** and never re-runs the scheduler, so it lands on the
same host, where the domain-ownership check makes it an idempotent success. If
the pending host is unreachable (`Create` keeps answering `Unavailable`), the VM
shows `Placed=False` (`HostUnavailable`) and is **never re-scheduled
automatically** (a domain may already exist there); it waits for the host, or for
an administrator to clear `status.placement.pendingHost`.

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

## Post-create routing (ADR-0007 Addendum A)

Only the operator knows where a VM runs, so **the operator names the host on
every per-VM call** and the provider never looks it up (A1, D1). Slice 1 routed
`Describe` and `Delete`; slice 2 routed `Power` and `Reconfigure`; slice 3 routes
the snapshot family, `Clone`, `GetDiskInfo`, `ExportDisk` and `TaskStatus`;
slice 4 runs `ListVMs` on every host and adds `TransferOwner` for adoption.

### On the wire and in the manager

- Every per-VM request carries `target_host_id` (`DeleteRequest`, `PowerRequest`,
  `ReconfigureRequest`, `HardwareUpgradeRequest`, `DescribeRequest`, the three
  snapshot requests, `ExportDiskRequest`, `GetDiskInfoRequest`);
  `CloneRequest.source_host_id` routes a clone by its source VM and
  `CloneRequest.target_host_id` (slice 3) names the host it lands on; and
  the `owner` of the `Describe`, `Delete`, `Power`, `Reconfigure` (slice 2),
  snapshot, `ExportDisk` and `GetDiskInfo` requests plus
  `CloneRequest.source_owner` (slice 3) carry the requesting VirtualMachine's
  identity. The owner is sent only together with a host. All additive:
  single-host and thin-client providers ignore them and are never sent them.
- `TaskStatus` carries no host, so a clustered provider **host-encodes** every
  task reference a routed call returns: `host-task/v1/<host id>/<host-local
  reference>`. The host id is a `Host` name (a DNS-1123 subdomain, so it never
  contains the `/` that ends it); the rest is the host-local reference,
  verbatim. The provider routes `TaskStatus` to that host and refuses any
  reference it could not have issued: one that is not host-encoded or is
  malformed is `InvalidArgument`, and one naming a host that is not in its own
  host registry is `NotFound` — the reference is never turned into an endpoint
  and nothing is dialed. The reference is opaque to the operator. Single-host
  task references are unchanged. (libvirt calls are synchronous today, so no
  routed call returns a task yet; the encoding is in place for `MigrateVM`.)
- Manager-side, every per-VM method takes a `contracts.VMRef{ID, HostID, Owner}`,
  and one helper builds it from a VirtualMachine and its Provider: for `topology:
  cluster` the host is `status.placement.host` and the owner is the VM's
  identity; for everything else both are empty (unchanged calls). A clustered VM with **no** confirmed binding (for example
  after a backup restore dropped its status) is **never sent a per-VM call**: the
  VM shows `Placed=False` (`Unbound`) and is re-checked every 30 s; snapshots,
  clones and migrations of it wait the same way.

### What the libvirt provider routes today

| RPC on a clustered provider | Behaviour |
|---|---|
| `Create` | Routed to `target_host_id` (shipped in P1). A same-named domain there stamped for the VM's namespace and name under another UID is answered `VM_PREVIOUS_INCARNATION`, and a disk file that already exists where the VM's disk goes is written only when no domain on **any** host of the Provider uses it (A6.1; see [the cluster-wide disk guard](#shared-storage-the-cluster-wide-disk-guard-a61)) |
| `Describe` | **Routed** to `target_host_id` and **owner-checked** (slice 1): the domain's state is returned only if its owner stamp is the requester's UID. An absent domain, or one whose stamp is missing, unreadable or foreign, is reported `exists=false` and none of its state is read; a domain replaced between the check and the read (different UUID) is reported absent too |
| `Delete` | **Routed** and **owner-checked** (slice 1): destroyed only if its owner stamp is the requester's UID; a missing, unreadable or foreign stamp is answered `NotFound` and the domain is never touched. Since slice 2 the teardown addresses the checked domain **by its UUID**, like `Power` and `Reconfigure`. Since A6.1 every **other** host of the Provider is checked too before anything is destroyed or removed: a disk file a domain on another host uses refuses the delete (`VM_DISK_IN_USE`), and a host that cannot be checked fails it closed (see [the cluster-wide disk guard](#shared-storage-the-cluster-wide-disk-guard-a61)) |
| `Power` (on, off, reboot, graceful shutdown) | **Routed** and **owner-checked** (slice 2): nothing happens unless the owner stamp is the requester's UID (otherwise `NotFound`, domain untouched); the operation then addresses the checked domain **by its UUID**, so a domain replaced after the check is not acted on. The post-start persistent-XML sync runs on the same host |
| `Reconfigure` (offline and online CPU/memory, disk grow) | **Routed** and **owner-checked** (slice 2), addressed by UUID like `Power`. Every step runs on the bound host: `setvcpus`/`setmem`, the disk resize, `blockresize`, and the best-effort in-guest filesystem grow through that host's guest agent. The disk that is resized is the checked domain's **own** primary disk, read with `domblklist --details` and resized by its path — never a volume found by name. Offline, it is resized with `vol-resize`; online, a block-device disk is resized before `blockresize`, while a file-backed disk is grown by `blockresize` alone (resizing a qcow2 under a running QEMU would be unsafe). Offline, a disk already at the requested size is not resized, and a disk with no host path (a network disk) that needs to grow fails the call. Each CPU/memory change is applied to the persistent definition first, then live; one the running domain cannot take is reported as `restart_required`; any failure fails the call (`VM_OPERATION_FAILED`); a domain that is paused, suspended to RAM or shutting down is refused unchanged |
| `HardwareUpgrade` | `Unimplemented` (libvirt has no hardware versions; the request carries `target_host_id` for a future provider) |
| `SnapshotCreate`, `SnapshotDelete`, `SnapshotRevert` | **Routed** and **owner-checked** (slice 3), addressed by UUID like `Power`: a domain this VM does not own is `NotFound` and none of its snapshots is created, deleted or reverted. A snapshot id this provider could not have created (for example one starting with `-`, which virsh would read as an option) is `InvalidArgument` before any host is touched. Memory snapshots work as on a single host. As on a single host (#358), each refuses while another domain on the host depends on one of the VM's disks — a linked clone's overlay — (`FailedPrecondition`, `VM_DISK_IN_USE` + `VM_OPERATION_FAILED`, never naming the other domain), and is retried when that cannot be checked (`Unavailable`, `VM_DISK_CHECK_FAILED` + `VM_OPERATION_FAILED`) |
| `Clone` (full clones only) | **Routed** to the source VM's bound host and **owner-checked** on the source (slice 3). A **linked** clone is `InvalidArgument` on a clustered provider in v0.4.0 (its overlay would depend on the source's disk for its whole life) and is not advertised. `target_host_id` must equal `source_host_id` (disks are host-local; no cross-host clone in v1), and an empty or different value is `InvalidArgument` — the provider never picks a landing host. The request must carry the target VirtualMachine's identity **with its uid**: the clone is stamped with it (never with the source's stamp). A domain of the clone's name that the target does not own is `AlreadyExists` and nothing is copied — with `VM_PREVIOUS_INCARNATION` when it is stamped for the target's namespace and name under another UID (A6.1); one it does own is an earlier attempt's clone and is reported as done (a retry is idempotent). A file already at the clone's disk or varstore path is overwritten only when no domain on any host of the Provider uses it (A6.1, [the cluster-wide disk guard](#shared-storage-the-cluster-wide-disk-guard-a61)). The clone gets **its own copy of the cloud-init seed ISO** in a per-clone seed directory, so deleting either VM never removes the other's seed. It is defined from the source's **persistent** definition (`dumpxml --inactive`), without the source disk's `<backingStore>` chain (the clone's disk is a standalone copy; only `/domain/devices/disk/backingStore` is removed, never another tool's `<metadata>`). **Single-disk sources only** for now: a source with more than one writable medium (`device='disk'`, `'lun'` or `'floppy'`, of any type) or a disk with an external data file (`<source><dataStore>`) is `InvalidArgument` before anything is copied, because only the primary disk is re-pointed at the clone's copy and any other would be shared by source and clone. Only read-only CD-ROM media (the seed among them) are not counted |
| `GetDiskInfo` | **Routed** and **owner-checked** (slice 3): the disks are read from the checked domain's own definition, nothing is looked up by volume name, and an explicit disk path must be one of that domain's disks |
| `ExportDisk` | **Routed** and **owner-checked** (slice 3) for the host-side backends only: `s3` (the host flattens the disk, the pod streams it to S3) and `nfs` (the host writes it to the export). The `pvc` export (and the empty legacy backend, which means `pvc`) reads the disk from the provider pod and is refused (`Unimplemented`), as on Proxmox. The export runs on the owner-checked domain's own disk; a host that is not reached over `ssh://` is refused naming the host id only, never its endpoint |
| `TaskStatus` | **Routed** to the host encoded in the task reference (slice 3; see above) |
| `ListVMs` | Runs on **every routable host** (slice 4; see [Listing and adoption](#listing-and-adoption-slice-4)): each `VMInfo` carries its `host_id`, and every host that could not be listed is named in `unreachable_host_ids` — never dropped, never a failed call |
| `TransferOwner` | **Routed** to `target_host_id` (slice 4): the owner re-stamp adoption uses — a serialized check-and-set with read-back (see below) |
| `ImagePrepare`, `ImportDisk` | `Unimplemented` (host-scoped, no target host yet) |

An empty `target_host_id` is `InvalidArgument` (never a default host). An unknown,
removed, draining or unreachable host is a retryable **host-scoped** `Unavailable`:
the status carries a `google.rpc.ErrorInfo` with reason `HOST_UNAVAILABLE`, which
the manager maps to its own error class and keeps **out of the per-Provider
circuit breaker** — one dead host must not fast-fail every VM on every other host
of the Provider (a provider-level `Unavailable` still trips the breaker). This
includes a host that dies **after** its connection was first used — the
registry reuses a dialed connection without probing it — because a call that
never gets an exit status back from the host (SSH dial, handshake or session
failure, a connection dropped mid-command), or a virsh that cannot reach the
host's libvirtd, is classified as host-scoped. Any other failure of a routed
call on its host (the host answered, but the operation on that one VM failed —
for example a disk grow the host cannot satisfy) keeps its historical code and
carries an `ErrorInfo` with reason `VM_OPERATION_FAILED`, which the
manager also keeps out of the breaker: one tenant's failing VM, retried every
few seconds, cannot open it for every VM of the Provider. **Routed errors carry
only a categorized message** — the operation and the host id, for example
`failed to create snapshot on host "host-b"` or `connect to host "host-b"` —
because they end up in tenant-readable conditions and events. The cause (SSH
dial address, virsh or qemu-img output, host paths) is in the provider's log
only. Single-host errors are unchanged.

**Long routed calls have a time budget (slice 3).** `Clone`, `SnapshotCreate`
and `ExportDisk` run under a budget that ends 15 s before the manager's
deadline (5 min for `Clone` and `SnapshotCreate`, 30 min for `ExportDisk`), so
an overrun is answered as `VM_OPERATION_FAILED` ("did not finish within its time
budget and was stopped") instead of reaching the manager as `DeadlineExceeded`,
which its circuit breaker counts. The qemu-img that copies a clone's disk or
flattens an export runs on the host as `flock -n -E 75 <lock> timeout
--kill-after=10s <budget>s qemu-img ...`: `timeout` stops it when the budget
runs out (cancelling the call alone would only close the SSH session and leave
it running), and `flock` holds a lock, so a retry while an earlier copy still
runs is answered `Unavailable` ("the same copy is still running", retryable,
still out of the breaker) instead of starting a second copy. **Clustered hosts
need `flock` (util-linux) and `timeout` (coreutils)**; a host without them fails
these calls with a message naming them and never runs the copy unguarded.

**A failed or stopped copy leaves no partial copy of the tenant's disk on the
host.** A clone whose copy fails, is stopped by its budget, or fails later
before its domain is defined removes the clone's disk
(`<pool>/<namespace>.<name>-disk.qcow2`) again, under the clone's lock: it
waits a few seconds for a copy that `timeout` just stopped, and leaves the disk
alone when another attempt still holds the lock (that attempt owns it) or when
the define's outcome is unknown (the domain may exist and use it; its
owner-checked Delete removes it). An `s3` export's flattened copy is created
by `mktemp` (exclusively, mode `0600`, unpredictable name) next to the source
disk, and its removal is registered before the flatten runs, so a flatten that
fails or is stopped never leaves a readable partial copy. The cleanup runs even
after the budget ran out and is bounded so the answer still reaches the manager
in time. An `nfs` export writes no host file: a stopped export leaves a partial
object at the migration's own destination on the NFS export, which the next
attempt overwrites (the manager does not yet delete NFS staging objects, even
after a successful migration). Single-host exports are unchanged.

The locks are separate files, `clone-<domain>.lock` and
`export-<domain>.lock`, in a directory VirtRigaud owns on each host:
`/tmp/virtrigaud-locks` (under the host staging directory), made mode `0700`
by the first guarded command. They are never the disk file itself: on an
NFS-backed pool `flock` is emulated as a whole-file POSIX lock that would
conflict with qemu's own image locks, and an export would otherwise need write
access to the source disk's directory just for its lock. Before every guarded
command the host checks that the lock directory is a directory, not a symbolic
link, owned by the SSH user, and that neither the lock nor the file the command
writes (the clone's disk, an export's temporary copy) is a symbolic link; if
any check fails the call is refused (`FailedPrecondition`, "was refused",
`VM_OPERATION_FAILED`) and nothing is copied. A lock directory another user
created first cannot be used: remove it (as root) and the next call recreates
it. Its predictable name is an accepted risk: a local user of a host can only
make clustered clones and exports on that host fail closed, never redirect
them. Every refusal increments
`virtrigaud_provider_host_guard_refusals_total{provider_type, reason}` (reason
`lock_dir_unsafe`, `lock_symlink`, `target_symlink` or `unknown`; no host,
path or tenant label) on the provider's `/metrics`, and is logged as
`ERROR ALERT host guard refused …` naming the host. Alert on any increase: it
means a host path VirtRigaud writes to was tampered with or pre-created.
*Follow-up:* make the staging (and lock) directory configurable per Host, so
clustered hosts can keep it outside the world-writable `/tmp`.

**Keep pool directories sticky or private.** A routed call writes files into
the storage pool directory (a clone's disk) or next to the source disk (an s3
export's temporary copy). It uses the single-host write path (#358): a
clone's disk is written with mode `0640` inside a private `mktemp -d`
directory next to its name and renamed onto the name (`mv -f -T`, which
replaces a symbolic link there rather than following it), a symbolic link
already at the name is refused, and the disk is given to the qemu user with
`chown -h` — never `chmod`'ed; a UEFI clone's varstore is created by `dd`
with `O_NOFOLLOW`/`O_EXCL` under a `0600` umask. Still, if that directory is
writable by a local user other than root and the provider's SSH user and has
no sticky bit (for example mode `2777`), that user can replace or remove a
finished file, which no check VirtRigaud makes can prevent. The provider logs
a `WARN` naming the directory once per host connection and directory (the
same check as single-host, `warnIfDiskDirUnsafe`); it does not refuse, so
existing hosts keep working. Fix it with `chmod +t <dir>`, or make the
directory writable only by root and the SSH user.

`virsh snapshot-create-as` is bounded by the budget but not wrapped: killing the virsh client would not stop
libvirtd's snapshot job, and libvirt refuses a second concurrent job on the
same domain. So a `SnapshotCreate` whose budget runs out is **not** reported as
stopped: the snapshot may still complete on the host (a memory snapshot writing
the guest's RAM), so the answer is `Unavailable` ("its outcome is unknown ...
retry", `VM_OPERATION_FAILED`), as is a retry that finds the earlier job still
holding the domain's job lock. The routed create is **idempotent per request,
not per name**: the manager sends the VMSnapshot's uid (a migration's uid for
its snapshot) as `request_token`, the provider records it at the end of the
snapshot's description (`[virtrigaud-request-token=<uid>]`), and a snapshot of
the requested (sanitized) name already on the owner-checked domain is returned
as the result only when it records the same token. Any other snapshot of that
name — a leftover of an earlier VMSnapshot of the same name (force-deleted, or
whose finalizer was dropped), or one made by hand — is `AlreadyExists`: it is
never adopted and never replaced, and the VMSnapshot fails saying so (delete
the leftover, or use another snapshot name). The VMSnapshot controller retries
a routed create with an unknown outcome (every 30 s) instead of failing it, so
a snapshot that completes late is found and tracked, never left on the host
untracked. A clone whose disk takes longer to copy than the budget cannot
complete (as on a single host, where the same 5-minute limit applies). A bound
VM whose host is unavailable — on `Describe`, `Power` or `Reconfigure` — shows
`Ready=False` (`HostUnavailable`) and is re-checked every 30 s. A host that
starts draining mid-call still completes that call. The Describe, Delete, Power,
Reconfigure, snapshot, Clone, GetDiskInfo and export cores are shared with
single-host mode — only the connection differs — and single-host calls emit
exactly the virsh / host command sequence, response and error they did before
routing (pinned by golden files). The ADR-0008 shadow read of a routed
`Describe` runs on the same leased connection. The single-host connection handle
a clustered provider used to carry is now an **always-failing placeholder**: any
call not routed to a host fails instead of reaching some unintended connection.

Clustered `GetCapabilities` is honest about this (D7): it advertises
`supports_clustering`; since slice 2, online reconfigure and online disk
expansion, exactly as a single-host provider does; and since slice 3,
snapshots, memory snapshots and disk export — the export exactly
as served: backends `s3` and `nfs` only, relay mode, `qcow2` only, no
compression — and `supportsRoutedClone` (it routes `Clone` to the source's host). It still hides linked clones and disk import, and reports
`supports_image_import=false`. The operator therefore skips image preparation
for a clustered provider.

### Lifecycle rules in the operator

- **`Placed` condition** (one positive condition): `True`/`Bound` once the
  provider confirmed the VM on its host; `False` with `CreatePending`,
  `HostUnavailable`, `Unbound`, `HostExcluded` or `AllHostsExcluded` (the last
  two since slice 2, see below), or `RestorePending` (A6.1, see
  [Previous incarnations](#previous-incarnations-and-the-a6-runbook)). It is
  never set on single-host VMs.
- **No re-creation (A4).** If the bound host reports a clustered VM missing — or
  the domain there is not this VM's (owner-checked `Describe`, `Power` or
  `Reconfigure` answers not-found) — the VM shows `Ready=False`
  (`VMMissingOnHost`) and is **not re-created**, on that host or elsewhere —
  without fencing that could leave two running copies of one disk. It is
  re-checked every 2 minutes, so a domain an administrator restores is picked
  up. (Single-host VMs keep the historical recreate behaviour.)
- **A create refused with a name conflict releases its pending host (slice 2).**
  An unreachable pending host still pins the VM (a domain may already exist
  there). But when the provider answers `AlreadyExists` — a same-named domain
  that this VM does not own is on the pending host — it has checked that
  *before* creating anything, so this VM has no domain there and that attempt
  created nothing. (An earlier attempt that failed part-way, before the domain
  was defined, may have left a `<name>-disk` volume or cloud-init files behind;
  the finalizer's owner-checked delete could not remove those either, so an
  administrator has to clean them up.) The operator
  then clears `status.placement.pendingHost`, adds the host to
  `status.placement.excludedHosts`, sets `Placed=False` (`HostExcluded`) and
  schedules the VM again; the scheduler never picks an excluded host. The list
  holds at most 16 hosts (the oldest is dropped, and the next attempt then
  waits 2 minutes) and is cleared when the VM is bound. If **every** candidate
  host is excluded, the VM shows `Placed=False`
  (`AllHostsExcluded`), no create is sent, and it is re-checked every 2
  minutes: resolve the name conflicts (or rename the VirtualMachine), then clear
  the list, for example with
  `kubectl patch virtualmachines.infra.virtrigaud.io <name> --subresource=status --type=json -p '[{"op":"remove","path":"/status/placement/excludedHosts"}]'`.
  **Except for a previous incarnation (A6.1):** when the conflicting domain is
  stamped with the VM's own namespace and name under another UID, the answer
  is `VM_PREVIOUS_INCARNATION` and the VM is **held**, not moved: see
  [Previous incarnations](#previous-incarnations-and-the-a6-runbook).
- **Deleting a VM whose create is in flight.** A VM with `pendingHost` set and no
  `status.id` gets an owner-checked `Delete` sent to the pending host, so a domain
  the create already made there does not leak — and one that is not the VM's own
  (e.g. after an `AlreadyExists`) is never deleted. The `Delete` addresses the VM
  by its bare name; the libvirt provider names a new domain `<namespace>.<name>`
  (see [`libvirt-domain-ownership.md`](libvirt-domain-ownership.md#domain-names)),
  so it also looks that domain up, under the same owner check. A VM whose only create was
  refused with a name conflict has no pending host left and is deleted without
  any provider call.
- **Deleting an unbound clustered VM** retains the finalizer (`Placed=False`,
  `Unbound`): the operator cannot know which host to clean up. The existing
  `virtrigaud.io/force-delete: "true"` annotation releases it (the hypervisor VM
  may then be left behind).
- **`Power` and `Reconfigure` failures (slice 2).** A host-scoped unavailability
  is `Ready=False` (`HostUnavailable`, 30 s re-check) and a not-found is A4, as
  above. A routed call — `Describe`, `Power` or `Reconfigure` — that the
  provider answers `Unimplemented` (for example an older clustered provider
  image that does not route it yet) is re-checked every 2 minutes. Any other
  failure is reported as before (`ProviderError`, retried after 5 s).
- **Snapshots (slice 3).** A `VMSnapshot` of a clustered VM is sent to its bound
  host with the VM's owner. A VM without a confirmed binding — including one
  whose create is only pending — is never snapshotted: the snapshot waits
  (`Creating` condition, `Unbound`). Deleting a `VMSnapshot` of a VM whose
  domain the provider reports as not this VM's removes the finalizer without
  touching any snapshot.
- **Clones (slice 3).** A `VMClone` of a clustered VM is a full clone (a linked
  one is refused before anything is created: the provider does not advertise
  linked clones, and the controller's linked-clone gate fails closed). It lands
  on the source VM's bound host, and only there:
  - The Provider must report `supportsRoutedClone` (a clustered provider older than slice 3 does not): otherwise the clone fails (`Unsupported`), and a failed capability query makes it wait — before anything is created. The source's host is checked next. A `Host` that is gone (or not this
    Provider's), being deleted or cordoned (`spec.schedulable: false`), or not
    `Ready` stops the clone: it stays `Pending` with `Ready=False`
    (`SourceHostGone`, `SourceHostCordoned` or `SourceHostNotReady`) and is
    re-checked every 2 minutes. No other host is ever tried and nothing is
    created.
  - The clone then creates its target VirtualMachine **before** the Clone call.
  - **The clone must fit on its host.** It is admitted against the free
    capacity of the source's host — the only host it can land on — exactly as
    a create or a resize-up is: everything the Provider's VMs, in any
    namespace, commit to that host, plus placements other reconciles have
    chosen but not yet recorded, under the same per-Provider lock (the VMClone
    and VirtualMachine controllers share it, so a concurrent create and clone
    never book the same capacity). The clone counts at the size it will have:
    the class named by `spec.target.classRef` (the provider applies it), or
    else the source VM's own size (its `status.currentResources`, including a
    `spec.resources` override), raised to its balloon ceiling when it has
    memory hot-add (for a clone of the source's size, when the source was
    provisioned with a ceiling — its recorded `memoryCeilingMiB` — never a
    class flag its tenant can change). A clone that does not fit is not sent: its target VM reports
    `Placed=False/Unschedulable` with the clone's own size and host only (no
    other tenant's figures), the VMClone stays `Pending` and is re-checked with
    a backoff of up to 2 minutes, and no other host is ever tried. A clone
    whose size cannot be read (its VMClass is missing or not granted) waits
    instead of being admitted blind.
  - It then records the target's `status.placement.pendingHost` (the source's
    host), with the admitted size (`pendingResources`) and the balloon ceiling
    the clone is provisioned with (`memoryCeilingMiB`: the source's for a clone
    of its size), in a checked status update before anything is created on
    the host (A2). **The size is frozen from then on:** the Clone call carries
    exactly the admitted vCPU, memory and headroom as its class override, so a
    VMClass edited after admission cannot change what is created or make it
    outgrow what was admitted (once bound, the VM converges to its class
    through the resize gate like any VM). A clone whose VMClass can no longer
    be read waits with a condition; it is never sent without its override.
    The clone is stamped with the target's identity, so a lost answer is safe:
    the retry lands on the same host, where the clone is reported as done, and
    deleting the target runs the owner-checked cleanup on the pending host.
  - A name conflict on the host (`AlreadyExists`) excludes the host for the
    target and clears its pending host, as for a create; since the clone can
    land nowhere else it waits (`SourceHostExcluded`) until an administrator
    resolves the conflict and clears the target's `excludedHosts`. A previous
    incarnation of the target (`VM_PREVIOUS_INCARNATION`, A6.1) is not a name
    conflict: the target keeps its pending host, shows
    `Placed=False/RestorePending`, and the clone stays `Pending`
    (`RestorePending`, re-checked with a backoff from 15 s doubling to
    5 minutes, never failed) until an
    administrator re-attaches or removes that domain. An
    unreachable host keeps the pending host, and the clone is retried on the
    same host (`HostUnavailable`, every 30 s). Any other failure fails the
    clone (terminal `Failed`).
  - **A clone that fails for good removes the target VirtualMachine it
    created** (event `TargetRemoved` on the VMClone). The target's finalizer
    then runs the owner-checked Delete on its pending host, which removes a
    domain the clone may have defined there stamped as that VM's. Only the
    object the clone created is ever deleted: it records that object's uid in
    `status.targetUID` when it creates it (the `virtrigaud.io/clone-uid`
    annotation alone can be copied onto a VM re-created under the same name,
    by GitOps or `kubectl get -o yaml | kubectl apply`, and is never enough),
    and a VM under the target name with another uid is neither cloned onto nor
    removed. The target must also be unbound (no `status.id`, no bound host),
    not refused by the clone (`TargetConflict`), and in a namespace that still
    grants the clone's namespace, and the delete is conditional on the uid and
    version that were checked, so a target bound in the meantime is kept. The
    decision is made **once** and recorded as the VMClone condition
    `TargetCleanup` (`True/TargetRemoved`, or `False/TargetKept` with why); a
    failed VMClone never acts on its target name again. A clone that is only retrying or waiting (`Pending`,
    including a blocked or excluded host) never removes its target. Single-host
    clones are unchanged: their target is created only after the clone
    succeeded.
  - On success the target is bound in one status write: `status.id`, the host,
    and the pending host cleared (`Placed=True`, `Bound`), with
    `status.currentResources` at the size actually cloned (the admitted size
    the Clone call sent) and its `status.placement.memoryCeilingMiB` kept, so
    the clone counts at its admitted size — never at its class floor or at a
    hot-add flag its tenant controls.
- **Migrations from a clustered VM (slice 3).** A clustered VM can be a
  `VMMigration` **source**: its disk is exported on its bound host through the
  `s3` or `nfs` backend. A `pvc` migration from it fails validation up front,
  because the pvc export reads the disk from the provider pod.
  - **`s3`:** the host flattens the disk and the provider pod streams it to S3
    over the host's SSH connection. The S3 credentials stay in the provider
    pod; they are never sent to the host. The host needs no route to S3.
  - **`nfs`:** the **host's** `qemu-img` writes the disk straight to the NFS
    export (libnfs). So every host of the pool that a migration source may be
    bound to needs network egress to the NFS server (TCP 2049, and 111 for
    NFSv3 mount/portmapper), and the export must allow those hosts' addresses,
    not only the provider pod's. The share is accessed with AUTH_SYS: the
    server trusts the uid/gid the client presents (`nfs.uid` / `nfs.gid` in
    the migration's storage options) and the host's address, over cleartext
    NFSv3 with no Kerberos. Anyone with root on a pool host, or able to send
    from its address, can therefore read or write the export: use a dedicated
    export per trust domain, restrict it to the pool hosts' addresses, and keep
    the storage network isolated (ADR-0006 C6).
- **A clone carries the source's cloud-init seed.** Every clone is a copy of
  the source's disk and definition, cloud-init CD-ROM included: the clustered
  clone gets its own copy of the source's seed ISO, so its user-data — and any
  provisioning secrets in it (passwords, SSH keys, tokens) — is the source's.
  A cross-namespace clone (#340 grant) therefore hands the source's user-data
  to the target namespace. See
  [`cross-namespace-targets.md`](cross-namespace-targets.md#a-clone-carries-the-sources-cloud-init-seed).
- **Adoption** works on a clustered provider since slice 4, keyed on
  (host, id); see [Listing and adoption](#listing-and-adoption-slice-4). A
  **VMMigration into** a clustered provider fails validation until P3.

### Listing and adoption (slice 4)

`ListVMs` is the one call that does not go to a single host (A3).

**The listing.** The provider lists every routable host of its registry
(draining hosts are being removed and are not listed):

- at most **8 hosts at a time**, each on its own lease with its own
  **60-second deadline**, so one dead or hung host cannot starve the others;
- inside the caller's deadline: the manager gives `ListVMs` 2 minutes, and the
  provider keeps 5 seconds of it back, so it always answers in time. A host not
  finished (or not yet started) by then is reported, not waited for. Each call
  starts the fan-out at another host, so a spent budget does not always leave
  the same hosts unlisted;
- bounded in size: a host with more than 2000 domains is reported unreachable
  (logged) instead of read; the manager accepts an answer of up to 64 MiB; and
  a command run over a host's SSH connection keeps at most 64 MiB of stdout —
  a larger stdout fails the command instead of being parsed truncated. Its
  stderr never fails it: the first 64 KiB and the last 960 KiB are kept (the
  error text comes last) and the middle is dropped and marked, so a large
  download's progress output does not fail a create that succeeded. Each domain's definition is still read with its own
  `virsh dumpxml`, and a running domain's persistent definition with a second
  one (`dumpxml --inactive`, to see a stamp that is only there), so a host of
  running domains takes about two SSH calls per domain. A host with more than
  roughly 600–1000 running domains may not finish within its 60 s deadline
  and is then reported unreachable (unknown). *Follow-up:* a batched or
  native (ADR-0008 go-libvirt) read;
- every `VMInfo` carries `host_id` (the `Host` name). On a clustered provider a
  VM is identified by **(`host_id`, `id`)**, never by its name: two hosts may
  each have a domain named `web`, and they are two entries;
- every `VMInfo` also carries the namespace and name recorded in the domain's
  owner stamp (`owner_namespace`, `owner_name`; the UIDs stay in
  `provider_raw["owner_uid"]`), empty for an unstamped domain or one with more
  than one owner. For an active domain the stamps of **both** definitions are
  read (a transfer stamps both), and `provider_raw["owner_stamp_state"]` says
  `unreadable` (a stamp cannot be read or has no UID) or `multiple` (more than
  one owner) — adoption skips such a VM before creating anything. They are informational — only the UID identifies an owner —
  and A6's pre-schedule uniqueness check (R4) looks a VM's previous
  incarnations up by them;
- every host whose VMs could not be listed — unknown or draining in the
  registry, unreachable, past its deadline, a failed `virsh list`, or a
  connection that dropped while its domains were read — is named in
  `unreachable_host_ids`. **Its VMs are unknown, not absent.** The call does not
  fail for it, so a dead host never counts toward the Provider's circuit
  breaker; the provider logs the cause (classified like the routed calls:
  host unavailable, deadline, or a failed listing). A domain whose definition
  the host returned but that cannot be used (unparseable, an unexpected memory
  unit) is skipped, as on a single host;
- only a provider-level failure fails the call (its host registry is not
  initialized), as a sanitized routed error;
- the ADR-0008 list shadow runs per host, on the same lease as that host's
  `virsh` list, and compares on (`host_id`, `id`).

Single-host `ListVMs` is unchanged (no `host_id`, never an unreachable host);
`testdata/single_host_listvms.golden.json` pins it byte for byte.

The `Host` object reports its own reachability on its `Ready` condition, from
the Host controller's `GetHostInfo` heartbeat (a host whose connection cannot be
leased is `HostNotReady`); `ListVMs` does not write `Host` status.

**Adoption** (`virtrigaud.io/adopt-vms` on the Provider) runs only when the
provider reports `supportsRoutedAdoption` (D7): a clustered provider older than
slice 4 is refused with a message and nothing is listed. For each listed VM:

1. **Managed?** A VM is managed, and left alone, when a VirtualMachine of this
   Provider is bound to its (`host_id`, `id`) — `status.placement.host` and
   `status.id` — or when it is stamped with the UID of a VirtualMachine that
   still exists (in any namespace, through any Provider object), exactly as on a
   single host. A VM without a `host_id` or UUID is never adopted.
   **Only unstamped domains are adopted** (until ADR-0007 A6.4). A domain with
   a VirtRigaud owner stamp whose VirtualMachine no longer exists is the
   *previous incarnation* of a VirtualMachine deleted with `orphan-on-delete`
   or restored from a backup with a new UID. It is skipped and named in
   `Provider.status.adoption.message` — by the namespace/name its stamp
   records, e.g. `team-a/web (team-a.web on host-a)` — with the hint to
   re-attach it per the A6 runbook or remove it. Unlike single-host adoption,
   a clustered one never takes it over.
   **Cross-host duplicates are not adopted either.** A brownfield domain whose
   UUID is listed on more than one host, or one of whose disk paths a VM listed
   on another host uses (common with a shared NFS pool: a stale definition left
   on a second host), is skipped and reported on every host. Adopting both
   copies would let deleting the stale one remove the running VM's disk.
   Because a duplicate on a host that could not be listed is invisible, **a
   discovery with any unreachable host starts no new adoption**: its
   candidates are reported as held ("not adopted while a host is unknown")
   and retried with the next discovery; bindings already in progress are
   still completed. *Remaining gaps:* disk paths are compared as the provider
   reports them (raw strings — two paths to one file through a symlink or a
   different mount are not matched), and the listing reports file-backed
   disks only, so block and network disks (a shared LUN, an RBD image) are
   not compared. (A clustered `Delete` checks every host of the Provider
   since A6.1, so deleting a VM whose shared disk a stale definition on
   another host uses is refused; see
   [the cluster-wide disk guard](#shared-storage-the-cluster-wide-disk-guard-a61).)
   **A Host whose endpoint another Host object, or a single-host Provider's
   `spec.endpoint`, names** (same scheme, host and port, whatever the SSH user;
   any Provider, any namespace) is not adopted from, and the manager logs a
   warning: two Providers fronting one hypervisor could each adopt the same
   unstamped domain. When the Hosts or Providers cannot be listed, the
   discovery adopts nothing. Keep one clustered Provider per host endpoint.
   *Residual risk:* one hypervisor reached under two names (a DNS alias, a
   host name and its IP address) is not recognized.
   **Do not front one hypervisor with both a single-host and a clustered
   Provider.** A single-host provider does not stamp the domains it manages,
   so a clustered adoption could take one of them. As a guard, an unstamped
   domain whose name is the `status.id` of a VirtualMachine bound through a
   non-clustered Provider is skipped and reported.
2. **The adopting VirtualMachine** is created in the Provider's namespace, as on
   a single host, but named after the domain **plus a digest of (host, id)**
   (for example `team-a-web-3f2a9c1b04`), so the same name on two hosts gives
   two VirtualMachines and a retry finds the same one. It carries the
   annotations `virtrigaud.io/adopted-host` and `virtrigaud.io/adopted-id`.
   Its status stays empty for now; the VirtualMachine controller waits for it
   (it never creates an adopted VM).
3. **Owner transfer.** Every routed call is owner-checked, so the domain is
   handed to the new VirtualMachine first: `TransferOwner` stamps it with the
   VirtualMachine's UID, namespace and name. It is a **serialized
   check-and-set with read-back**: the domain must still carry the UUID that
   was listed, and every stamp on it must be the new owner's (a retry) or one
   the caller lists as replaceable — adoption lists none, so it takes only an
   unstamped domain (A6.4's re-attach will list the previous incarnation's).
   A domain stamped for anyone else, with two stamps, or whose stamp cannot be
   read, is refused (`AlreadyExists`) and not touched; a replaced domain is
   `NotFound`. When the provider refuses the transfer itself for good
   (`AlreadyExists`, `NotFound`, `InvalidArgument`), the adopting
   VirtualMachine — whichever discovery created it — is removed again, after
   a fresh read shows it still waiting for that very domain, unbound: it has
   no `status.id`, so its deletion makes no provider call. One that cannot be
   removed is named in `status.adoption.message`. A retryable failure, or any
   failure after a successful transfer (the domain already carries its
   stamp), keeps it, and the next discovery completes the binding. Transfers are serialized per host in the provider process, so
   the check-and-set holds only while **one provider process** fronts a host:
   the provider controller runs a clustered Provider with **one replica and
   the `Recreate` strategy** (`spec.runtime.replicas` above 1 is ignored and
   logged), and each host endpoint must belong to **one clustered Provider**.
   `Recreate` stops the old provider pod before the new one starts, so a
   clustered provider's rollout (an image upgrade, a changed spec) is a short
   outage: its gRPC endpoint is down until the new pod is ready, and VM
   reconciles against it fail and are retried meanwhile.
   The provider refuses (`InvalidArgument`) an owner whose UID is not a
   canonical UUID or whose namespace/name are not DNS-1123, and a domain name
   virsh would read as a domain id or UUID (adoption skips those names); every
   `virsh` call addresses the domain with `--domain`, and the definition read
   must name the requested domain.
   The stamp is written with `virsh metadata`
   to the domain's persistent definition (and to the running domain when it is
   active), addressed by UUID, and read back before the call succeeds. A retry
   that finds the stamp already there succeeds without writing.
4. **Binding.** A routed, owner-checked `Describe` of the new VirtualMachine
   must then find the domain; only then is one status write made:
   `status.id`, `status.boundProvider`, `status.placement.host` (the listed
   host) and `.pool` (the Host's pool), and the domain's size from provider
   truth — `status.currentResources` (its effective size, the CPU raised to the
   vCPUs `Describe` reports online) and `status.placement.memoryCeilingMiB`
   (`Describe`'s memory maximum when it exceeds that, else 0). From then on the
   committed-capacity accounting counts it on its host, and the Host's in-use
   finalizer holds the Host.
5. If the binding write is lost after the owner transfer, the next discovery
   finds the domain stamped with a VirtualMachine that is still waiting for
   this very (host, id) and completes the binding — it never adopts it again.

As on a single host, an existing adopted-labelled VirtualMachine is bound only
when it references this Provider and every cross-namespace VMClass or VMImage it
references grants the Provider's namespace; without the grant it is not even
handed the domain. A listed host that is not a `Host` of this Provider, or is
being deleted, is never adopted from.

**Unknown is not empty.** Adoption only adds: it never unbinds, deletes or
re-adopts anything because a VM is missing from a list. The hosts in
`unreachable_host_ids` are named in `Provider.status.adoption.message` (by
`Host` name, never endpoint), and discovery is retried after 5 minutes instead
of an hour.

A domain adopted on a clustered provider is managed exactly like a created one:
deleting its VirtualMachine deletes the domain and its disks on its host (use
`virtrigaud.io/orphan-on-delete` to let go of it instead).

### Shared storage: the cluster-wide disk guard (A6.1)

The hosts of one clustered Provider may share a storage pool directory (an NFS
export mounted on every host, ADR-0007 D6). A VM's disk is then visible — and
writable — from every host, but each host's libvirtd knows only its own
domains. Before ADR-0007 A6.1, a create of `team-a/web` on host B overwrote
`<pool>/team-a.web-disk.qcow2` while a domain `team-a.web` — orphaned with
`orphan-on-delete`, left by a force-delete, or the original of a VM restored
with a new UID — was still running on host A from that file; and deleting a
VM on host B could remove a disk a domain on host A was using. The provider
now checks every host of the Provider:

- **Create and Clone.** The landing host probes **every name the VM's disk
  may have had** in the storage pool, in this incarnation or an earlier one,
  whatever kind of disk it was: `<namespace>.<name>-disk` (a blank volume —
  libvirt names the file after the volume, without an extension),
  `<namespace>.<name>-disk.qcow2` (a disk copied from an image, or a clone),
  `<namespace>.<name>-migrated.qcow2` (an imported disk attached in place), and
  the same three for the VM's legacy bare name. When any of them exists,
  **every host** in the provider's registry — the landing host too — is
  scanned once, for all of them, before anything is written: a previous
  incarnation found under any name holds the VM, and the file this create or
  clone writes is written only when no domain on any host uses it. (A foreign
  use of one of the *other* names refuses nothing: that file is not written.)
  The common case, with none of them there, costs two host commands on the
  landing host and contacts no other host. A clone's UEFI varstore is
  written next to its source's varstore: an existing one that resolves into
  the host-local NVRAM directory keeps the host-local check, and one that
  resolves anywhere else (a source whose varstore lives on shared storage) is
  checked on every host, like a disk.
- **The base image, too.** A clustered create from a host-path image
  (`VMImage.spec.source.libvirt.path`) copies that file into the VM's disk.
  The image confinement used to check only the landing host for a domain
  using it, so a tenant could name another host's live disk (an adopted
  domain's disk, say) and have it copied into their VM. Every other host is
  now scanned for the image as well, before anything is copied: a use refuses
  the create, and a host that cannot be checked fails it closed. **So every
  clustered create from a host-path image scans every host, and waits while
  any host of the Provider is unreachable** (creates from a URL or a template
  do not). On a clustered Provider the confinement also answers a missing
  file, a VirtRigaud-managed file name, a file that is not a regular file and
  a file in use with the **same** "not allowed" message, so a tenant's VMImage
  path cannot probe the hosts' storage (single-host answers are unchanged).
- **Delete.** After the host-local checks and **before** anything is
  destroyed, undefined or removed, every **other** host is scanned for a
  domain that uses one of the files the delete would remove. This runs on
  every clustered delete that has files to remove.

"Uses" is the host-local definition, applied per host: any domain defined
there — running or not, VirtRigaud's or anyone else's — that references the
file as a disk, anywhere in a disk's backing chain (read with `qemu-img info`,
as for the host-local check), or as another file or shared directory.

| What the scan finds | Answer | What the manager does |
|---|---|---|
| Create / Clone: a domain on any host stamped with the VM's namespace and name (any UID) | `AlreadyExists` + `VM_PREVIOUS_INCARNATION`; nothing written | Holds the VM on its pending host (`RestorePending`), see [Previous incarnations](#previous-incarnations-and-the-a6-runbook) |
| Create / Clone: another domain uses the file | `AlreadyExists`; nothing written | The slice 2 name-conflict rule: the host is excluded and the VM re-scheduled |
| Delete: a domain on another host uses one of the files | `FailedPrecondition` + `VM_DISK_IN_USE` + `VM_OPERATION_FAILED`; nothing changed | Keeps the finalizer (`Ready=False/DeleteBlocked`), re-checks |
| A host could not be reached (not leased, dropped, past its deadline, tombstoned) | `Unavailable` + `HOST_UNAVAILABLE`; nothing changed | Create: `Placed=False/HostUnavailable`, retried on the same pending host with the backoff (15 s doubling to 5 min, from when the hold began). Delete: finalizer kept, `DeleteBlocked=True/HostUnreachable`, retried with a backoff (15 s doubling to 5 min) |
| A host answered but could not be scanned (a definition or disk chain unreadable, more than 2000 domains), or the provider was too busy to scan | `Unavailable` + `VM_DISK_CHECK_FAILED` + `VM_OPERATION_FAILED`; nothing changed | Create: `Placed=False/CreatePending`, retried with the backoff. Delete: `DeleteBlocked=True/DiskCheckFailed`, retried with the backoff |
| Nothing uses the file | The existing file is a leftover of an earlier failed attempt and is replaced (create); the delete proceeds | — |

None of these answers counts toward the Provider's circuit breaker, and their
messages name no host, no path and no other domain (the provider logs the
details).

**Which hosts are scanned.** Every host of the Provider's inventory — and
also every `Host` the Provider fronts that the operator could **not** render
(its credentials are missing or refused, its endpoint is invalid, its id is
duplicated). Those are passed to the provider as id-only tombstones (no
endpoint, no credentials): it cannot connect to them, but it knows they exist,
so the guard fails closed on them rather than silently skipping them. A host
whose `Host` object is gone is no longer part of the Provider and is not
scanned.

**Fail closed, and what it costs.** A host that cannot be checked fails the
operation; nothing is ever written or removed on a partial answer. For Create
and Clone that happens only when a file is already where the disk goes. **A
clustered Delete, however, always has files to remove, so while any host of
the Provider cannot be reached (or is tombstoned), deleting a clustered VM is
held**: the finalizer stays, the VM shows `DeleteBlocked=True` (reason
`HostUnreachable` or `DiskCheckFailed`) and `Ready=False` (`DeleteBlocked`)
with a message that names no host, one `Warning` event is emitted per change,
and the delete is retried with a backoff per VM — 15 seconds, doubling up to
5 minutes. To get out of it: bring the host back (the next retry succeeds);
detach the VM with `virtrigaud.io/orphan-on-delete=true` (the domain and its
disks stay for manual removal; a VM in another namespace than the Provider
needs the Provider's permission, see above) or `virtrigaud.io/force-delete`
(the same, after the failed delete) — both are acted on at once, whatever the
backoff; or remove the dead host from the Provider (below).

**Fencing a host before you remove it.** Deleting the `Host` of a dead host
(possible only when no VM is bound or pending on it) removes it from the
Provider, and from then on the guard no longer looks at it. **Fence the host
first: power it off, or revoke its access to the shared export.** A host that
is merely unreachable from the provider may still run domains on the shared
pool; once it is out of the inventory, a delete or a re-create elsewhere can
remove or overwrite a disk it is using.

**One Provider per shared pool.** The guard sees the hosts of **one**
Provider. A shared pool (an NFS export) must be mounted only by the hosts of a
single clustered Provider — never by hosts of another Provider, a single-host
Provider, or anything VirtRigaud does not manage — or their domains are
invisible to it.

**Bounds.** The scan is slice 4's fan-out: at most 8 hosts at a time, each on
its own lease with a 60-second deadline, and the whole scan inside the
caller's deadline less 30 seconds, so the operation — a delete's teardown, a
create's disk write — and its answer still fit. It stops as soon as the
outcome is decided — a delete at its first use or failure, a create or clone
at its first previous incarnation — and cancels the hosts still running. A
host whose dial failed, whose connection dropped or that timed out less than
30 seconds ago is failed at once without being dialed again. A scan that any
failure decides — a delete's, and the base-image check — fails closed
**before** it starts when such a host, or a tombstoned one, is among the
hosts: no other host is read for an answer already known. A create's disk
scan still runs (a previous incarnation elsewhere is the more specific
answer) and checks those hosts first. At most 2 scans
run at once per provider process; one that gets no slot within its budget
fails closed as busy (`VM_DISK_CHECK_FAILED`, retried). A host with more than
2000 domains fails the scan closed instead of being read partially. The
landing host of a create or clone is scanned over the call's own connection.
A scan reads every domain's definition on each host, plus the disk chains of
shut-off domains, so a clustered delete costs roughly one SSH command per
domain of the Provider. *Follow-ups:* batch the per-domain reads (ADR-0008's
native list), and scope the scan to the hosts that share the pool once a pool
ownership marker exists.

**Check and act are serialized per domain, in the provider.** The scan and
the write, define or teardown it guards are not one atomic step. The
provider therefore holds a lock on the domain name (`<namespace>.<name>`) from
the check until the act completes, for every clustered Create, Clone and
Delete: a retry that arrives while an earlier attempt still runs (the manager
stopped waiting, the provider did not) waits for it, within its own budget,
instead of checking and writing next to it; one that gets no lock in time is
not performed (`Unavailable` + `VM_OPERATION_FAILED`, retried). A Delete
takes the lock **before** its owner check: the finalizer's Delete of a VM
whose Create is still running waits for it and removes the domain that
Create defined, instead of finding nothing yet and releasing the
finalizer. *Remaining
gap:* the lock is in-process, so an actor outside VirtRigaud — an
administrator's `virsh`, another tool, a second provider process fronting the
same hosts — can still change a disk between the check and the act.

**Paths are compared per host, canonically.** The candidate file — the path
the landing host uses and the path it resolves to there — is resolved again
with `realpath` on each scanned host and compared with that host's own
references, raw and resolved; a delete's cloud-init seed directory is resolved
on each host as well. So symbolic links on either side are followed. Files in
the libvirt NVRAM directory (`/var/lib/libvirt/qemu/nvram`) are host-local —
the same path on another host is another file — and are compared on the
operation's own host only; a clone's UEFI varstore that resolves there
therefore keeps the host-local check (one elsewhere is scanned on every
host). Each host is scanned through its `Host` endpoint's libvirt
instance (`qemu+ssh://…/system` or `…/session`): domains that another libvirt
instance on the same host runs (another user's session) are not seen.
**Mount a shared pool at the same path on every host of a Provider** (libvirt's
shared-storage migration needs that as well), and **enable NFS locking** on
it (NFSv4, or NFSv3 with `lockd`/`statd`): QEMU's own image locks are what stop
two running domains from opening one disk, and they need working NFS locks.
*Not matched (residuals):* a host that mounts the same export under a
**different** path, or reaches a file through a second mount, a bind mount or a
hard link (`realpath` resolves none of them); and disks without a host path —
protocol disks such as NBD, RBD or iSCSI. Comparing `(st_dev, st_ino)` does not
close that gap: `st_dev` is assigned by each NFS client, so it differs between
hosts for the same file, and `st_ino` alone is not unique across filesystems.
*The converse:* on **host-local** pools the same path on two hosts is two
files, so a domain on another host whose disk has the same path is reported
as a use — a false "in use" that fails safe (refuses) — until a pool ownership
marker tells the provider which hosts share a pool (a follow-up). Other
residuals: a host removed from the Provider's inventory is not scanned (see
*Fencing a host before you remove it*); and on a **host-local** pool a previous incarnation on
another host leaves no file where the new disk goes, so this guard does not
see it — the pre-schedule check (A6.2, R4) does.

### Previous incarnations and the A6 runbook

On a clustered Provider there is at most one domain VirtRigaud created per
`<namespace>.<name>` (ADR-0007 A6, decision 2): the domains VirtRigaud created
for a namespace and name carry them in their owner stamp. A domain stamped
with a VM's namespace and name under **another UID** is a *previous
incarnation* of that VM — left by `orphan-on-delete`, a force-delete, or a
backup restore that re-created the VirtualMachine with a new UID.

**Since A6.1 (R2).** A create or clone that meets a previous incarnation — on
its landing host by name, or on any host during
[the cluster-wide disk guard](#shared-storage-the-cluster-wide-disk-guard-a61)
— is refused with `AlreadyExists` + `VM_PREVIOUS_INCARNATION` and creates
nothing. The manager then **holds** the VM:

- it keeps `status.placement.pendingHost`: the host is **not** excluded and the
  VM is **not** re-scheduled (moving on is exactly how a second domain would be
  made). The VM keeps counting on that host at its pending size, and its
  `spec.providerRef` stays locked;
- `Placed=False` and `Provisioning=False` with reason `RestorePending`, one
  `Warning` event, and the create is retried on the same host with a backoff
  per VM (15 seconds, doubling up to 5 minutes, counted from when the hold
  began), so a held VM does not scan every host every few seconds;
- a clone's target VM is held the same way, and the `VMClone` stays `Pending`
  (`RestorePending`), never `Failed`;
- deleting a held VM never touches the previous incarnation: its owner-checked
  `Delete` carries the held VM's own UID and gets `NotFound`, and the
  VirtualMachine goes.

**The VM's own domain on another host.** A domain stamped with the VM's
namespace, name **and its own UID** on another host is not a previous
incarnation: it is this VM's own domain, and only its placement record was
lost. The provider counts it apart and says so (`VM_PREVIOUS_INCARNATION` with
ErrorInfo metadata `incarnation: own`): the VM is held the same way, but its
message asks for the pending host to be moved — **no re-stamp is needed**:
`kubectl patch virtualmachines.infra.virtrigaud.io <name> -n <namespace> --subresource=status --type=merge -p '{"status":{"placement":{"pendingHost":"<host>"}}}'`;
the next create retry binds the domain. Deleting such a VM is **held** too
(`DeleteBlocked=True/OwnDomainOnAnotherHost`): its `Delete` on the pending host
finds nothing, and releasing it would leave its own domain running. Move the
pending host (the delete then removes the domain), or set `force-delete` /
`orphan-on-delete` to release it and leave the domain for manual removal.

A foreign or unstamped domain that merely has the name keeps the slice 2
behaviour (the host is excluded and the VM placed elsewhere). The restore
marker and the pre-schedule check (A6.2) hold such VMs before they are ever
scheduled; until then R2 is the guard.

**The runbook.** Re-attaching is not automated in v0.4.0. As an administrator:

1. Find the previous incarnation. On each host of the Provider, run
   `virsh metadata <namespace>.<name> --uri https://virtrigaud.io/xmlns/libvirt/owner/v1`
   (and, for a domain named otherwise, check `virsh dumpxml`). Stop if more
   than one host has one, and check that no VirtualMachine with the stamp's
   UID still exists (`kubectl get virtualmachines -A -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}'`).
2. Either **re-attach** it: rewrite the stamp's `uid` to the held VM's UID,
   keeping the namespace and name —
   `virsh metadata --domain <uuid> --uri https://virtrigaud.io/xmlns/libvirt/owner/v1 --key virtrigaud --set "<owner uid='<new uid>' namespace='<namespace>' name='<name>'/>" --config`
   (add `--live` if the domain is running). If the held VM's
   `status.placement.pendingHost` is that domain's host, the next create retry
   binds it as an idempotent success. If it names another host (the domain was
   found there by the disk guard), set it to the domain's host first:
   `kubectl patch virtualmachines.infra.virtrigaud.io <name> -n <namespace> --subresource=status --type=merge -p '{"status":{"placement":{"pendingHost":"<host>"}}}'`.
3. Or **discard** it: remove the domain and its disk
   (`<pool>/<namespace>.<name>-disk.qcow2`) and cloud-init seed on its host;
   the next create retry creates the VM as new on its pending host.

Never run `kubectl replace --force` on a VirtualMachine (its delete half
destroys the domain), and prefer backups that include VirtualMachine status
(Velero `restoreStatus`). The full backup and restore guide (A6.3) and the
automated re-attach (`VMRestoreBinding`, A6.4) follow.

## What "clustered" does not mean (yet)

- **No automatic HA / failover.** v1 detects and surfaces host-down and supports
  operator-initiated evacuation, but does **not** auto-restart VMs elsewhere.
  Without fencing/STONITH that would risk split-brain disk corruption; automatic
  HA is a deferred future ADR (ADR-0007 D8/P5).
- **Experimental.** A clustered VM can be scheduled, created, described,
  powered, reconfigured, snapshotted, cloned (onto its own host), exported
  (s3 / nfs) and deleted on its host (ADR-0007 Addendum A slices 1–3), and
  VMs can be listed across hosts and adopted (slice 4). Disks on a shared
  pool are protected across hosts, and a previous incarnation holds its
  VirtualMachine (A6.1); the restore marker and pre-schedule check (A6.2) are
  not in yet. Nothing can be migrated **into** a clustered provider (P3).
  The `vprovider.kb.io` validating webhook enforces the topology×type rule at
  admission (ADR-0007 D2). Until Addendum A slice 5 validates a real clustered VM
  end to end, do not run workloads on `topology: cluster`.
- **No rescheduling or migration yet.** Moving an *already-created* VM is not wired:
  neither rescheduling a bound VM to a different host, nor host→host `Migrate` /
  `VMHostMigration`. These land in later ADR-0007 phases.
