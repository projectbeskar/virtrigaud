# ADR-0007: Migrate the libvirt control plane to a native libvirt SDK transport

## Status

**Proposed (2026-06-29).** The control plane stays on `virsh` by default; the native path is
opt-in. The binding and transport choice this migration depends on is decided separately in
[ADR-0008](./0008-libvirt-go-binding-and-transport-selection.md).

**Author**: Komh ([@jing2uo](https://github.com/jing2uo))

**Tracking**: the motivating problem is the libvirt `Describe` storm (companion issue).

**Related**:

- [ADR-0008](./0008-libvirt-go-binding-and-transport-selection.md) — *which* binding/transport
  (official CGO binding + `qemu+libssh2`); this ADR is *how* the migration is structured.
- [ADR-0004](./0004-libvirt-ssh-host-key-verification.md) — trust contract preserved across the
  migration.
- [ADR-0001](./0001-transport-grpc-and-capi-integration.md) — gRPC contract unchanged; this is
  entirely below the provider gRPC surface.
- `internal/providers/libvirt/{transport.go,transport_native.go,provider.go,virsh.go,provider_virsh.go,guest_agent.go}`

---

## Context

### The control-plane tax

The libvirt provider is a `virsh`-CLI-over-SSH client: every control-plane operation resolves to
one or more of `virsh …`, `ssh … virsh …`, or `ssh … <host command>`, through
`VirshProvider.runVirshCommand()`. A bounded exec semaphore was already added to stop
subprocess/fork storms — a strong signal that the **transport model itself** is now the
bottleneck. The same architectural tax surfaced three times, each patched tactically while the
root cause (subprocess-per-call over SSH) remained:

- `ListVMs` slowness (N+1 `virsh` fan-out) → tactical: one `dumpxml` per VM;
- a post-adoption `Validate` fork storm → tactical: drop duplicate validate + exec semaphore;
- a `Describe` 30s-deadline storm → the companion issue, still open in spirit.

### Why a native SDK is the structural fix

A persistent native libvirt client turns each control call into a typed RPC on one connection: no
per-call `virsh` fork, no per-call SSH handshake, no semaphore queueing. It is feasible here
because the provider is **already a CGO build** (`Makefile` builds `provider-libvirt` with
`CGO_ENABLED=1`; CI installs `libvirt-dev`; the Dockerfile installs `libvirt-dev` in the build
stage and `libvirt0` at runtime). Moving from "CGO provider that shells out to `virsh`" to "CGO
provider that links a native client" is **not** a build-system paradigm shift.

### The transport trap (non-code risk)

Current security/compat behaviour is coupled to literally launching `ssh`: explicit argv, host-key
policy via a written `~/.ssh/config`, mounted `known_hosts`, `sshpass` for passwords, per-command
retry. A native transport changes connection establishment, so transport/auth/host-key is a
first-class design item — decided in ADR-0008. "It still connects" is not a sufficient bar.

---

## Decision

### 1. Migrate the control plane; keep the data plane on SSH

Move **only the control-plane hot paths** onto the native client; **keep the SSH host-command
runner** for data-plane work the SDK cannot replace.

- **Migrate (control plane):** `Validate`, `Describe`, `ListVMs`, `Reconfigure`, `Power`,
  `Snapshot*`, and the domain-control parts of `Create`/`Delete`.
- **Keep on SSH (data plane):** `ImagePrepare`, `ImportDisk`, `ExportDisk`, clone disk
  copy/overlay/NVRAM, cloud-init ISO, file ownership/permission/SELinux relabel, all `qemu-img`.

The SDK solves the libvirt RPC/control plane; it does **not** solve remote host filesystem
orchestration. These primitives remain necessary regardless of transport and stay on the host
runner: `qemu-img info|convert|resize|check`, `curl`/`wget`, `genisoimage`, `cp`, `rm`, `chmod`,
`chown`, `restorecon`. What changes is that control-plane hot paths stop using that channel; only
storage/image/file paths still do.

### 2. Target architecture: split transport concerns

Introduce two abstractions inside `internal/providers/libvirt`, and have `Provider` compose both:

- a **control transport** for domain/libvirt-RPC operations (the native client; today
  `controlTransport` in `transport.go`), and
- a **host-command runner** for remote command/file/data-plane work (extracted from the
  `runVirshCommand("!", …)` paths).

```go
// control plane (native): lookup/state/info/XML/ifaddr/stats/agent, power, resize, snapshots
type controlTransport interface { Validate(...); Describe(...); /* … */ }
// data plane (SSH): remote exec + file copy for qemu-img/genisoimage/chown/restorecon/...
type HostCommandRunner interface { Run(...); CopyToHost(...) }
```

The exact names are not important; the separation is. It makes obvious which methods can move
immediately and which still depend on the host command channel.

### 3. Opt-in gate, build-tag seam, fail-closed, default virsh

- Runtime gate `LIBVIRT_CONTROL_TRANSPORT=virsh|native` (default `virsh`) → live A/B in kind or
  shared clusters without a flag day; rollback by env change, not rebuild.
- Build-tag seam `//go:build libvirt_native`: the native CGO code is isolated so the default
  `go build`/`go test` on machines without `libvirt-dev` is unaffected; the provider image is
  built with `-tags libvirt_native`. The non-tagged shim registers the native dialer via an
  `init()` so `Provider` references only an interface, never the CGO types.
- **No silent downgrade**: native-requested-but-uninitialisable fails closed (ADR-0004 posture).
- Startup logging states which transport is active and the host-key verification posture.

### 4. Split `Describe` (do not port the sweep)

The current `Describe` is a per-VM "comprehensive monitoring" sweep (`getDomainInfo` →
`enrichDomainInfo` + a guest-agent layer): ~15 `virsh`/`ssh` subprocesses, many optional
guest-agent calls that fail on agent-less fleets but still cost a round-trip each. Porting it 1:1
would preserve the conceptual mistake. Split into:

1. **cheap `Describe`** (reconcile-safe): existence, power state, best-effort IPs, console URL,
   minimal provider-raw — the fields the controller actually consumes;
2. **opt-in `DescribeRich`**: memory/block stats + guest-agent OS/hostname/fs/users — off the hot
   path, every guest-agent call time-bounded and best-effort (an absent/slow agent must never gate
   reconcile; it degrades to host-side stats only).

This enforces the contract — `Describe` *"should be cheap and resilient to call frequently"* — in
code structure, not just intent.

---

## Payoff by surface (why this order)

| Surface | Today | Native | Payoff |
|---|---|---|---|
| `Validate` | `virsh list --all --name` (fork+ssh per call) | `Connect.IsAlive` | **very high** |
| `Describe` | monitoring sweep → 30s deadline | typed lookup + split rich path | **very high** |
| `Reconfigure` | drags full `getDomainInfo` to compare state | `GetState`+`GetInfo`, disk only on change | **high** |
| `ListVMs` | `list --all` + `dumpxml`/VM over ssh | native enumerate + XML on the connection | **moderate–high** |
| `Power`/`Snapshot*` | one `virsh` per op | typed domain/snapshot ops | **moderate** |
| `ImagePrepare`/`Import`/`Export`/clone | qemu-img + transfer + SELinux | only trims incidental `virsh` | **low** |

The native SDK does **not** make `Describe` cheap by itself if the requirement stays "collect
comprehensive monitoring every reconcile" — that is why §4 splits it. The SDK removes the
transport overhead; the split removes the over-fetch.

---

## Phased rollout

- **Phase 0 — groundwork & safety rails**: `controlTransport` seam (`transport.go`), native impl
  (`transport_native.go`), `LIBVIRT_CONTROL_TRANSPORT` gate, build-tag isolation, fail-closed,
  startup transport/verification logging.
- **Phase 1 — `Validate`**: cheap native liveness probe; no subprocess/fork; no manager/provider
  contract change.
- **Phase 2 — `Describe` (split first)**: cheap `Describe` + opt-in `DescribeRich`; native
  lookup/state/info/ifaddr/XML + bounded guest-agent. No semantic regression for existence /
  power-state correction / IP discovery / reconfigure gating.
- **Phase 3 — `Reconfigure`**: native state/info + live vCPU/memory setters; keep guest-fs
  extension / disk-grow helpers on the host path; stop dragging the `Describe`-class chain in.
- **Phase 4 — `ListVMs`**: native enumeration + XML, same `contracts.VMInfo` for adoption. This
  also removes the need for the insecure opt-out on the discovery path in a read-only-rootfs pod,
  where the system `ssh` client cannot receive the host-key config (ADR-0008 §transport) — itself
  an argument for the migration.
- **Phase 5 — lifecycle**: `Power`, `Snapshot{Create,Delete,Revert}`, domain-control parts of
  `Create`/`Delete` — mostly straightforward SDK substitutions once the transport exists.
- **Phase 6 — optional cleanup**: re-evaluate whether `virsh` is needed at all; extract the SSH
  host-command runner (`hostrunner_ssh.go`) from `runVirshCommand("!", …)`; shrink runtime deps;
  revisit `ImagePrepare`/`Import`/`Export`/clone. Optional — the operational pain is gone by then.

---

## Consequences

- The control-plane storms (`Validate` fork storm, `Describe` 30s deadline) are removed at the
  root, not patched.
- A clean control/data-plane split. **Files likely to shrink** over phases: `virsh.go`,
  `provider_virsh.go`, `guest_agent.go`, parts of `clone.go`. **Files likely to stay substantial**
  (real host/file/data work): `storage.go`, `image.go`, `cloudinit.go`, `s3export.go`,
  `s3import.go`, `nfs.go`.
- New build/runtime dependency (decided in ADR-0008): the native binding + `libssh2-1` in the
  runtime image.
- The single-host, single-connection native client is the natural **host-agent** primitive for a
  future per-host-pod cluster (the libvirt multi-host cluster RFC / #257, Option 2) — keep it
  single-connection; do not evolve it into an in-process multi-host pool (that is Option 1's
  SPOF/debt path).
- Backward-compatible: default stays `virsh`; native is opt-in and env-reversible.

---

## Appendix A — virsh → SDK call mapping

| Concern | virsh (today) | native SDK |
|---|---|---|
| liveness | `list --all --name` | `Connect.IsAlive` |
| lookup/state/info | `list --all` / `domstate` / `dominfo` | `LookupDomainByName` / `GetState` / `GetInfo` |
| XML | `dumpxml` | `GetXMLDesc` |
| IPs | `domiflist` / `domifaddr` / `guestinfo` | `ListAllInterfaceAddresses` (lease+agent sources) |
| mem/cpu | `dommemstat` / `cpu-stats` | `MemoryStats` / typed CPU+state info |
| disk stats | `domblklist` / `domblkinfo` / `domblkstat` | XML + `BlockStats` |
| power | `start` / `destroy` / `shutdown` | `Create` / `Destroy` / `Shutdown` / `Reboot` |
| reconfigure | `setvcpus` / `setmem` / `setmaxmem` / `blockresize` | typed setters + `BlockResize` |
| snapshots | `snapshot-create-as` / `-delete` / `-revert` | `DomainSnapshotCreateXML` + revert/delete |
| guest agent | `qemu-agent-command …` | `QemuAgentCommand` (bounded timeout) |

## Appendix B — file plan

- **Add**: `transport.go` (seam), `transport_native.go` (native client); later
  `hostrunner.go` / `hostrunner_ssh.go` (extract the `! …` host-command path), and
  `describe_basic.go` if the cheap path outgrows `transport_native.go`.
- **Refactor/shrink over phases**: `provider.go` (construction chooses transport),
  `virsh.go` (toward virsh-transport-only), `provider_virsh.go` (split by behaviour, not the de
  facto monolith), `guest_agent.go` (stop assuming agent calls are shell-invoked).
- **Unchanged (data plane)**: `storage.go`, `image.go`, `cloudinit.go`, `s3export.go`,
  `s3import.go`, `nfs.go`.

## Appendix C — testing & rollout

- **Unit**: transport init `virsh` vs `native`; cheap-`Describe` semantics; `Validate` behaviour;
  host-key/auth config resolution.
- **Integration (kind / shared cluster)**: A/B `virsh` baseline vs `native` — startup, `Validate`,
  adoption/`ListVMs`, steady-state reconcile, `Describe` latency under manager restart, snapshot
  create/delete, power on/off.
- **Perf**: before/after `Validate` and `Describe` latency, manager `Failed to describe VM` count,
  provider subprocess count / absence of fork storms.
- **Rollout**: merge with default `virsh`; enable `native` in test envs; flip default only after
  auth/host-key parity (ADR-0008), `Describe` storm materially improved, and rollback confirmed.
- **Avoid**: porting `Describe` mechanically; making data-plane work a prerequisite; removing
  `virsh` before native proves remote-auth parity; assuming ADR-0004 "comes for free."
