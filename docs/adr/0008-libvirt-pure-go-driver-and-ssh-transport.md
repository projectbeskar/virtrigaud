# ADR-0008: Pure-Go libvirt driver and in-process SSH transport for the libvirt provider

## Status

**Accepted (2026-07-20; blocking decisions resolved 2026-09-21; accepted 2026-09-22).**
The two blocking decisions this ADR carried — the strangler flag's surface/lifetime
and the shadow-compare flip-gate — are settled (see `0007-0008-blocking-decisions.md`
and D6 below).

**Implementation status (2026-09-22):** the shadow phase is **shipped** — PR 0 (#302),
PR 2 (#305), PR 3 (#306), PR 4a (#307), PR 4b (#308), PR 4c (#310), plus the CI
build-images-on-PR gate (#311). `describe` and `list` reads run in **shadow** (virsh
authoritative and returned; go-libvirt compared and metered) at **0 divergences** on
the lab soak. **PR 5 — the native flip — remains gated on the D5 soak window** and is
not yet done. This ADR proposes replacing the libvirt provider's
`virsh`-subprocess-over-SSH control plane with a **pure-Go libvirt RPC client**
(`github.com/digitalocean/go-libvirt`) speaking over an **in-process SSH client**
we own, behind a **per-host connection seam** that ADR-0007's clustered provider
then generalizes from one host to N.

Three things make this ADR unusual and they should be read before the decisions:

1. **The scope is smaller than it looks.** Of ~152 `runVirshCommand` call sites,
   **74 are not virsh at all** — they are host shell commands (`sudo`, `qemu-img`,
   `bash`, `rm`, `genisoimage`, …) riding the `"!"` escape. A libvirt RPC client
   structurally **cannot** replace those. They are **permanent SSH tenants**, not
   technical debt, and the seam must treat them as first-class.
2. **The migration path does not move.** Live migration **stays on `virsh
   migrate` in v1** (D4). This is counter-intuitive for a "get off virsh" ADR and
   is the single most important decision here; it is argued at length.
3. **A contributor is already building this.** [#291](https://github.com/projectbeskar/virtrigaud/pull/291)
   by [@jing2uo](https://github.com/jing2uo) (Komh) implements the same thesis
   with the **cgo** binding, live-validated at **45 VMs**. This ADR **reconciles**
   with that PR rather than superseding it (D9).

No CRD change. No proto change until ADR-0007 P1. Nothing in this ADR is
user-visible except the image slimming, the removal of `sshpass`, and one
deliberate breaking change to SSH password auth (D8).

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Hypervisors in scope**: libvirt/KVM only. vSphere (govmomi) and Proxmox VE
(REST) are unaffected — they never had a subprocess control plane.

**Related**:
- [ADR-0003](./0003-mtls-and-provider-grpc-auth.md) — mTLS on the manager↔provider
  channel; unchanged. This ADR only changes what happens *below* the provider.
- [ADR-0004](./0004-libvirt-ssh-host-key-verification.md) — SSH host-key /
  `known_hosts` verification. **Preserved exactly**; D3 reimplements the same
  policy in-process instead of via `ssh`/`scp` command-line flags, and D8
  proposes retiring the password branch ADR-0004 had to special-case.
- [ADR-0006](./0006-storage-backend-agnostic-cross-hypervisor-migration.md) — the
  S3/NFS relay pipeline. Its host-side streaming (`qemu-img convert … /dev/stdout`)
  is one of the 74 permanent shell tenants and moves onto the new `Conn.Stream`.
- [ADR-0007](./0007-clustered-orchestrator-provider.md) — the clustered
  orchestrator provider. **This ADR's PR 2–4 are P1 prerequisites for it**; ADR-0007
  has been amended to say so.
- [#255](https://github.com/projectbeskar/virtrigaud/issues/255) /
  [#256](https://github.com/projectbeskar/virtrigaud/issues/256) — the persistent
  libvirt connection, named as the "strategic fix" in the `virsh.go:45` comment.
- [#257](https://github.com/projectbeskar/virtrigaud/issues/257) — go-libvirt
  adoption; `internal/providers/libvirt/domainxml.go:55-57` was already written
  anticipating it.
- [#288](https://github.com/projectbeskar/virtrigaud/issues/288) — the fork
  exhaustion that produced the exec semaphore. **A latent instance still exists**
  (see Context).
- [#291](https://github.com/projectbeskar/virtrigaud/pull/291) — the contributor
  PR this ADR reconciles with; review at
  `fieldTesting/pr-291-native-transport-review.md`.

---

## Context

### The current control plane, traced through the live code

The libvirt provider has **no libvirt client**. Every control operation is a
subprocess:

- `internal/providers/libvirt/virsh.go:459` `runVirshCommand` is the one
  chokepoint. It fans out to `runVirshCommandOnce` (`:503`), which forks either
  `virsh` locally against `LIBVIRT_DEFAULT_URI` (`:607`), or `ssh`/`sshpass` to
  run a command **on** the host (`:546`, `:557`, `:561`, `:566`, `:602`).
- `internal/providers/libvirt/provider.go:152` carries the tombstone of the
  removed cgo path: *"Removed old libvirt-go connection logic — now using virsh
  provider."*

**There is no `import "C"` anywhere in the package** — verified. It is pure
`os/exec`.

### Fact 1 — the scope is 74 shell sites smaller than the raw count

| Category | Count | Can a libvirt RPC client replace it? |
|---|---|---|
| `runVirshCommand` call sites (non-test) | **152** | — |
| …of which use the `"!"` **host shell** escape | **74** | **No — structurally impossible** |
| …of which are **real virsh** (~40 distinct ops) | **~78** | Yes |

The 74 are `sudo` (~20), `qemu-img` (~14), `bash` (~14), `rm` (~8),
`test`/`mkdir` (~6), plus `genisoimage`, `sha256sum`, `wget`, `curl`. They build
cloud-init ISOs, convert disks, stream bytes for ADR-0006, and stat files. A
libvirt connection cannot run `qemu-img`. **The SSH channel is permanent.** Any
design that treats "remove virsh" as "remove SSH" is wrong, and any seam that
does not make shell execution a first-class method will grow a second, worse
transport beside it.

The real-virsh side is ~40 distinct operations, heavily repeated: `dumpxml` ×6,
`pool-refresh` ×6, `list` ×3, `snapshot-list` ×2, `setvcpus` ×2. That
repetition is why the port is tractable — it is ~40 conversions, not 78.

`internal/providers/libvirt/storage.go` is the densest file at **44** call sites,
**24 of which are `"!"` shell**. Package totals: **17 non-test files, 8,402
non-test LOC**, 2,560 test LOC.

### Fact 2 — CGO is already vestigial, and that reframes the whole argument

- `cmd/provider-libvirt/Dockerfile:52` sets `CGO_ENABLED=1`, and `:8-9` install
  `libvirt-dev` + `pkg-config` + `gcc`.
- `Makefile:213` mirrors it: `CGO_ENABLED=1 go build … ./cmd/provider-libvirt`,
  with the help text `## Build libvirt provider binary (requires CGO)`.
- There is **no cgo code**. Every other provider already builds `CGO_ENABLED=0`
  (`Makefile:217` for vsphere).

So `CGO_ENABLED=0` + a static binary is available **today, before any refactor**
(PR 0). This matters for honesty: **the cgo argument in this ADR is not about a
build flag, it is about runtime packages.** Adopting the cgo binding
(`libvirt.org/go/libvirt`) would take a currently-vestigial dependency and make
it load-bearing — `libvirt0`, `libxml2`, and their transitive C surface would
become permanently required at runtime and permanently on the image.

### Fact 3 — a hard blocker sits in front of any driver swap

`internal/providers/libvirt/server.go` **type-asserts the concrete type at six
RPCs** — lines **269, 358, 400, 454, 501, 669** — then reaches through the
assertion into the *unexported* field `.virshProvider` and calls unexported
methods on it:

- `getDomainState` (`:290`, `:416`), `snapshotExists` (`:364`, `:406`),
  `runVirshCommand` (`:316`, `:384`, `:437`, `:730`, `:763`, `:772`)
- unexported **fields**: `.uri` (`:715`, `:863`), `.hostKey` (`:888-892`),
  `.credentials` (`:898-906`)

There is **no interface**. `VirshProvider` has 30 methods, 2 exported. Swapping
the driver underneath is impossible while the gRPC server layer is welded to the
concrete implementation. **Removing these assertions is a prerequisite, not
cleanup** — it is the entire content of PR 2.

### Fact 4 — the exec semaphore is not what ADR-0007 says it is, and it has 7 holes

**Correction to ADR-0007.** ADR-0007 (Context, and again in Security) describes
`virsh.go:45` as *"a single per-process semaphore"* and *"one global bucket"*,
and concludes the clustered executor needs a **per-host** budget it does not have.

That is wrong. `execSem` is already a **per-`VirshProvider` field**
(`internal/providers/libvirt/virsh.go:69`), allocated per instance in
`NewVirshProvider` (`:96`). It is "global" only in the trivial sense that exactly
**one instance exists today**. N connections ⇒ N semaphores, nearly free, no
redesign. ADR-0007 has been amended accordingly.

The **real** concurrency risks are different, and both are live today:

**(a) Seven call sites bypass the semaphore entirely.** `acquireExecSlot`
(`virsh.go:116`) is taken only in `runVirshCommandOnce` (`:506`). These fork SSH
without it:

```
internal/providers/libvirt/s3import.go:248   exec.CommandContext(ctx, "sshpass", …)
internal/providers/libvirt/s3import.go:258   exec.CommandContext(ctx, "ssh", …)
internal/providers/libvirt/s3export.go:230   exec.CommandContext(ctx, "sshpass", …)
internal/providers/libvirt/s3export.go:237   exec.CommandContext(ctx, "ssh", …)
internal/providers/libvirt/server.go:902     exec.CommandContext(ctx, "sshpass", …)  // scp
internal/providers/libvirt/server.go:909     exec.CommandContext(ctx, "scp", …)
internal/providers/libvirt/server.go:913     exec.CommandContext(ctx, "scp", …)
```

These are precisely the **disk-streaming and scp paths** — the ones most likely
to run concurrently during a migration. This is a **live latent instance of the
#288 fork exhaustion** that `c8fd36c` was supposed to have closed.

**(b) Streaming and control share one budget.** A 40 GB `qemu-img
convert … /dev/stdout` export holds one of **four** slots for the duration, so
concurrent exports can starve inventory probes and `Describe`. Long-lived
transfers and short control calls need **separate budgets**.

### Fact 5 — what go-libvirt does not give us

All verified against the pinned version:

- **No `context.Context` on any of ~488 generated RPC methods.** No per-call
  deadline, no cancellation. This is a **regression**: `exec.CommandContext`
  cancels correctly today, and the manager sets real per-RPC deadlines in
  `internal/transport/grpc/client.go` (30s / 2m / 5m). Mitigation in D5.
- **No keepalive implementation.** libvirt's RPC keepalive is not implemented;
  there is no `SetKeepAlive` and no close-callback. PR #291's finding **B3**
  (written against the cgo binding, which *has* both and simply didn't use them)
  therefore transfers to go-libvirt **and gets worse** — we must build the
  liveness machinery ourselves.
- **The bundled `libvirttest` mock is unusable.** It stamps its own serial
  counter instead of echoing the request's, and it hangs under concurrent load —
  i.e. it fails at exactly the tests that matter. See Testing strategy for the
  replacement.
- **Upstream has never cut a semver tag.** The dependency pins to pseudo-version
  `v0.0.0-20260609165003-6254771e63a8`. Recorded as a supply-chain caveat, not a
  blocker (the module is widely used, vendored by DigitalOcean, and the API
  surface we need is the generated RPC layer, which is stable by construction).

### Fact 6 — `libvirtxml` is pure Go and has exactly one bug we must fix

`libvirt.org/go/libvirtxml` was empirically verified: **zero dependencies in its
own `go.mod`, no `import "C"`, `CGO_ENABLED=0` build proven**. MIT, maintained by
Red Hat / libvirt.org.

Round-trip tested against real `virsh dumpxml` output from live domains with a
canonical-tree differ: **zero lost elements on `Domain` — except one.**
`<metadata>`'s own namespace declarations (`xmlns:libosinfo=`,
`xmlns:cockpit_machines=`) are **dropped**. Root cause: `DomainMetadata.XML` is
tagged `xml:",innerxml"`, which captures the children but not the parent tag's
own attributes. The result is **namespace-invalid XML**, confirmed with real
`xmllint` errors. A ~50-line hoist/restore fix was written and verified clean
against both `xmllint` and the RNG schema.

Separately, `Caps` silently drops `<externalSnapshot/>` — proof of the general
hazard: **an element the library does not model simply vanishes on serialize.**

**Rule this ADR establishes: never parse → mutate → serialize a domain
definition.** Either regenerate the XML from our own spec, or pass the exact
string through untouched. `domainxml.go:79` already embodies the right instinct
— it *errors* on a non-KiB memory unit rather than mis-scaling. Keep that guard.

### Fact 7 — the image and supply-chain delta, measured

| | Size | Packages | Base-image CVEs (C/H/M) |
|---|---|---|---|
| `debian:bookworm-slim` (current base) | 74.8 MB | 88 | **4 / 17 / 57** |
| …+ current runtime packages (as shipped) | **157 MB** | **144** | — |
| `distroless/static-debian12:nonroot` + comparable Go binary | **76.1 MB** | — | **0 / 0 / 0** |

The eliminated half is almost entirely third-party C. `libicu72` alone is **36
MB** — larger than `libvirt0` — and is pulled in *only* by `libxml2`. Also
eliminated: a second TLS stack (`libgnutls30`), Kerberos, PAM.
`cmd/provider-mock/Dockerfile:29` already ships `gcr.io/distroless/static:nonroot`
in this repo, so the pattern is proven here.

**Honest caveats.** CVE counts drift and many findings are unreachable in our
usage. **The stronger argument is the class argument**: `libxml2` and libvirt's C
RPC and XML parsers are memory-unsafe code parsing hypervisor-returned,
partially tenant-influenced input. Moving that to Go's `encoding/xml` plus a
pure-Go RPC client **removes a memory-corruption class outright**, which is a
categorical improvement that does not depend on any scanner's count. And because
D4 keeps `virsh`, **the image win is partial in v1** — `libvirt-clients` and its
C stack stay for one command.

Secondary wins available regardless: `curl` (`Dockerfile:65`) exists only for a
`HEALTHCHECK` (`:96-98`) that is **inert under Kubernetes**, which uses probes;
`bash`; and eventually `openssh-client` + `sshpass`.

### Fact 8 — the security debt in the current transport

- **`sshpass -e` passes a hypervisor-root-equivalent password through the
  `SSHPASS` environment variable** (`virsh.go:546`, `s3export.go:230`,
  `s3import.go:248`, `server.go:902`), readable from `/proc/<pid>/environ`.
  ADR-0004 also had to special-case this password path throughout.
- The SSH key is **written to disk** and an `~/.ssh/config` is generated
  (`virsh.go:276` `createSSHConfig`), which is what blocks a read-only root
  filesystem.
- **`verifyKnownHostsPresent` only checks the file is non-empty**, not that the
  *specific target host* is present. Seed host A, point a Provider at host B, and
  the gate passes — giving false confidence exactly where ADR-0004's first-contact
  trust window lives. (PR #291 review, **B6**.)
- **CI never scans the runtime images.** `.github/workflows/ci.yml:160-166` runs
  Trivy with `scan-type: 'fs'` only. The packages this ADR is about live in the
  *images*, which are not scanned at all.

`golang.org/x/crypto` is **already an indirect dependency** (`go.mod:102`, via
minio-go). Promoting `x/crypto/ssh` to a direct dependency adds **no new module
tree** — a real, not rhetorical, supply-chain argument.

### Fact 9 — a running contributor PR

[#291](https://github.com/projectbeskar/virtrigaud/pull/291) (@jing2uo / Komh),
+1374/−2, *"native control-plane transport over libvirt SDK — Describe first"*,
implements this exact thesis with the **cgo** binding and was **live-validated at
45 VMs: 865 sub-second `Describe` calls against a prior 30-second storm.** The
three-specialist review (`fieldTesting/pr-291-native-transport-review.md`) reads
*"Right architecture, not merge-ready"* and lists six blockers. Its findings are
about the **connection model**, not the binding, and therefore survive the change
of driver. See D9.

### Other live defects this ADR records as follow-ups

- `provider.go:141-145` — `New()` **logs and swallows** `Initialize` failure, so
  the process begins serving gRPC with a dead connection. PR #291's review calls
  the same class **B2** (does not fail closed).
- `server.go:520-545` hardcodes a `GetCapabilitiesResponse` struct literal and
  does **not** use `sdk/provider/capabilities` — unlike proxmox and mock. Three
  competing capability vocabularies now coexist. `HardwareUpgrade`
  (`proto/provider/v1/provider.proto:329`) is unimplemented and falls through
  silently.

---

## Decision

### D1 — Adopt the **pure-Go** `github.com/digitalocean/go-libvirt`; reject the cgo binding

**The libvirt control plane moves to a pure-Go implementation of libvirt's RPC
wire protocol. `libvirt.org/go/libvirt` (cgo) is rejected.**

Pin: `v0.0.0-20260609165003-6254771e63a8` (upstream has never tagged a release —
recorded as a supply-chain caveat; we pin the pseudo-version and treat bumps as
reviewed dependency changes).

*Rationale*: the decision is about **runtime packages, not the build flag**
(Fact 2). The cgo binding makes `libvirt0` + `libxml2` + `libicu72` +
`libgnutls30` permanently load-bearing — 157 MB / 144 packages against distroless'
76 MB / near-zero — and it makes multi-arch builds require a cross-compiling C
toolchain per architecture. For a project documented as deployable in **regulated
banking environments**, permanently adding a memory-unsafe XML and RPC parsing
surface that processes hypervisor-returned, partially tenant-influenced input is
the wrong trade, and the class argument holds regardless of any scanner's count
(Fact 7). Pure Go also gives us `CGO_ENABLED=0` static binaries on every provider
uniformly, which is what the rest of the repo already does.

*What we give up, honestly*: `libvirt.so`'s client-side helpers — most
consequentially the managed-migration orchestration, which is the entire subject
of D4.

### D2 — XML via `libvirt.org/go/libvirtxml`; **never** parse → mutate → serialize

**Use `libvirtxml` for typed domain/pool/volume XML. Treat every serialize as
lossy. Regenerate from spec or pass the exact string through — never round-trip a
definition you did not author.**

Two obligations attach:

1. **The `<metadata>` namespace-declaration bug must be fixed before any write
   path uses the library** (Fact 6). The ~50-line hoist/restore fix is verified;
   it lands with PR 4 and is upstreamed.
2. **Golden-file tests** capture real `virsh dumpxml` output from live domains and
   assert canonical-tree equality on parse, so an unmodelled element that starts
   vanishing is caught by CI rather than by a customer.

*Rationale*: `libvirtxml` is verifiably pure Go with zero transitive
dependencies, MIT, and maintained by the libvirt project itself — it is the only
option that preserves D1. But "unmodelled element silently vanishes" is a
structural property of `encoding/xml` struct mapping, not a bug we can fix, so
the *usage rule* is the actual mitigation. `<externalSnapshot/>` disappearing
from `Caps` is the cheap proof; a dropped `<hostdev>` on a re-defined domain would
be the expensive one.

### D3 — Transport stays **SSH**; implement go-libvirt's `socket.Dialer` over our own pooled `*ssh.Client`

**No libvirtd TCP or TLS listener is enabled on any host. go-libvirt reaches
libvirtd through a Unix socket forwarded over an SSH connection we own and
maintain: `sshClient.Dial("unix", "/var/run/libvirt/libvirt-sock")` behind
go-libvirt's one-method `socket.Dialer` interface. ADR-0004 host-key pinning is
preserved unchanged.**

go-libvirt ships its own SSH dialer (`socket/dialers/gossh.go`), and we
deliberately do not use it: it performs a **full SSH handshake on every
reconnect**, which turns a transient libvirtd blip into repeated key exchanges.
Our own persistent `*ssh.Client` reconnects the libvirt socket over an SSH
connection that is already up.

*Rationale*: this is the decision that preserves the most and costs the least.
The trust boundary does not move — same hosts, same keys, same `known_hosts`, same
ADR-0004 policy, **no new listener, no PKI**. It also collapses three problems at
once: the `sshpass` env-var exposure disappears (in-process `x/crypto/ssh` auth),
the on-disk key write and generated `~/.ssh/config` disappear (unblocking a
read-only root filesystem), and the 74 permanent shell tenants get a *better*
channel than they have now (one multiplexed connection instead of a fork per
call). And it costs no new module tree — `x/crypto` is already in `go.mod:102`.

### D4 — Live migration **stays on `virsh migrate` in v1**; peer-to-peer is **DEFERRED**

**This is the most important and most counter-intuitive decision in this ADR.**

**Managed (client-orchestrated) migration is not in the libvirt RPC wire
protocol.** It is implemented in `libvirt.so`, in
`virDomainMigrateVersion3Full` — the client library drives a five-phase
handshake between two daemons, shuttling opaque cookies. go-libvirt, being a wire
protocol client, exposes the **primitives** (`DomainMigrateBegin3Params`,
`Prepare3Params`, `Perform3Params`, `Finish3Params`, `Confirm3Params`, cookies as
plain `[]byte`) but **no orchestration**, and `DomainMigrateToURI*` /
`DomainMigrate*` are **absent entirely** — because they too live in the C client.

Three options were considered:

**(1) Reimplement the five-phase flow in Go.** *Rejected.* This is hand-rolling
libvirt's most safety-critical client path — cookie threading, error unwinding,
and the correct rollback at each phase — where the failure mode is a VM that
exists on both hosts or neither. And it does not even buy correctness: the flow
has an **inherent unrecoverable window at `PERFORM3_DONE`**, where the client has
lost the ability to cleanly undo, regardless of implementation quality. We would
take on libvirt's hardest client code and still inherit its worst window.

**(2) Adopt `VIR_MIGRATE_PEER2PEER`.** Technically the clean answer: one RPC to
the source daemon, which then drives the destination itself. It **survives pod
death**, and a restarted pod can reattach via `DomainGetJobStats`. Both OpenStack
Nova and oVirt/vdsm do exactly this. But it requires the **source host to reach
the destination daemon**, which means a listener, which means PKI — a **material
security cost** analyzed below.

**(3) Keep `virsh migrate`, run over the same in-process `*ssh.Client`.**
**CHOSEN for v1.**

*Rationale for (3)*:

- **ADR-0007 v1 explicitly defers automatic HA** (its D8, for fencing reasons).
  So every migration in v1 is **human-triggered and supervised**. That is exactly
  the regime in which p2p's survive-pod-death advantage matters **least**.
- It preserves ADR-0007's **pod-brokered managed model and trust boundary
  exactly** — control is pod→each-host, already established; the only host↔host
  requirement stays the QEMU data ports.
- It requires **no PKI and no new listener**.
- It keeps `virsh domjobinfo` / `domjobabort` progress and abort, which
  **ADR-0007 already specifies**, working unchanged.
- It removes the single hardest part of the port from scope.
- It lets migration be **lab-validated immediately** with the existing trust
  model, instead of blocking the entire ADR-0007 P2 slice on standing up a
  sub-CA and a migration VLAN.

*Honest costs, recorded*:

- **`libvirt-clients` (and its C stack) stays in the image for this one
  command.** The CVE/size win of D1 is therefore **partial in v1, not complete**.
  Say so in the release notes; do not claim distroless before P5.
- **Pod death mid-migration still leaves a VM paused** on one side, needing a
  manual `virsh resume`. Document it in the migration runbook.
- Migration is the one operation where **the entire motivation for this port does
  not apply**: it is rare, operator-initiated, and long-running, so a subprocess
  fork costs nothing measurable against a multi-minute RAM transfer.

**p2p is deferred to the automatic-evacuation / HA work — ADR-0007 P5, which
already gets its own fencing ADR. Bundle the migration PKI decision into that
ADR.** The pre-analysis below exists so that decision is *made*, not *researched*,
when P5 arrives.

#### Deferred-p2p pre-analysis (for ADR-0007 P5)

If p2p is adopted at P5, it requires **all** of the following. Several are traps
that fail silently:

1. **A dedicated migration sub-CA.** Never a general-issuance enterprise CA.
   libvirt's `tls_allowed_dn_list` **defaults to checking nothing**, so a general
   CA means *every certificate that CA has ever signed* is accepted as a migration
   client. The CA's issuance scope **is** the authorization boundary.
2. **An explicit per-host DN allowlist.** Note the asymmetry:
   **empty list = deny-all; unset = allow-any.** These are one character apart in
   a config file and opposite in effect.
3. **A TLS listener bound via a systemd `ListenStream=` drop-in.** **Trap:**
   under socket activation, libvirtd's `listen_addr` is **silently ignored** — set
   it and it appears configured while the socket binds wherever systemd says. The
   bind address must be set in the systemd unit, not in `libvirtd.conf`.
4. **A dedicated migration VLAN as a gating condition**, not a nice-to-have.
5. **CRL at libvirt's default paths.** **Trap:** hot reload via `virt-admin
   server-update-tls` re-reads **default paths only**. Any custom `crl_file`,
   `cert_file`, or `ca_file` override silently breaks CRL refresh — revocation
   stops working with no error. **OCSP is not supported at all.**
6. **The residual that cannot be engineered away:** a **migration client
   certificate is root-equivalent on the hypervisor and cannot be narrowed.**
   polkit does not apply to TLS clients. The DN allowlist gates *whether you
   connect*, never *what you may do* once connected. **One stolen certificate
   opens the entire HostPool — pool size IS blast-radius size.** This must be
   stated plainly in the P5 ADR and accepted explicitly, or p2p must not ship.

### D5 — A per-host connection **seam**, with shell as a first-class method

**Introduce `internal/providers/libvirt/hostconn/`: `HostID`, `Conn`, `Registry`.
Everything else in the package talks to the seam, never to a driver.**

```go
// HostID identifies one hypervisor host. Today exactly one exists; under
// ADR-0007 it is the Host CR name.
type HostID string

// Conn is one live connection to one host: a libvirt RPC client and a shell
// channel, both riding the same *ssh.Client.
type Conn interface {
    HostID() HostID
    Libvirt() *libvirt.Libvirt                        // typed control plane
    Run(ctx context.Context, argv ...string) (Result, error)  // the 74 permanent shell tenants
    Stream(ctx context.Context, argv ...string) (io.ReadCloser, error) // ADR-0006 export/import
}

// Registry owns the lifecycle of all Conns.
type Registry interface {
    ConnFor(ctx context.Context, id HostID) (Conn, error)
    Hosts() []HostID
    Evict(id HostID)
    Close() error
}
```

Two rules the seam imposes:

- **`Run` and `Stream` are not a compatibility shim — they are the permanent
  home of 74 call sites** (Fact 1). Designing them as an afterthought is how this
  package grows a second transport.
- **Never cache a `Conn`.** Always `ConnFor` at point of use. A watchdog can close
  and evict the underlying socket at any moment; a cached handle is a
  use-after-evict waiting to happen.

*Rationale*: the seam is what makes the driver swap possible at all (Fact 3), and
it is **the same seam ADR-0007 P1 needs**. Today `Registry` holds one entry
sourced from `PROVIDER_ENDPOINT`; under ADR-0007 it holds N sourced from projected
`Host` CRs. **Only the constructor changes.** Building it once, here, is why these
two ADRs must not be implemented independently.

### D6 — **Strangler-fig** migration: shadow-compare reads, per-family flag-gated writes

**Stage by operation family behind the D5 seam. Reads are shadowed in production
against the real VM population before any cutover; writes are lab-validated and
flipped per family. The flag is additive per family, never a global switch.**

- **Reads** (`dumpxml`, `list`, `domstate`, `dominfo`, `pool-info`, `vol-*`,
  `domblklist`, `domiflist`) are side-effect free. Run **both** drivers, **return
  virsh's answer**, meter the divergence. This buys parity evidence from the
  **real production 14–17 VM population at zero risk** — the single most valuable
  artifact in the plan, and it costs nothing but CPU.
- **Writes** cannot be shadowed (you cannot define a domain twice). Lab validation
  plus flag-gated cutover, one family at a time.
- The flag is **additive**: `VIRTRIGAUD_LIBVIRT_NATIVE=reads,lifecycle` (empty =
  all-virsh). **Not** a global `virsh|golibvirt` switch.

*Rationale for the additive flag specifically*: a global switch means the first
regression anywhere forces a **full revert**, and you learn nothing about *which
operation* broke. Per-family means a bad snapshot conversion rolls back snapshots
and leaves the read and lifecycle wins in production. The rollback granularity is
the whole point.

**Flag surface + lifetime — settled 2026-09-21 (blocking decision; resolves Open
question 1).** The flag is a **per-family env var** on the provider Deployment
(`VIRTRIGAUD_LIBVIRT_NATIVE=reads,lifecycle,…`), **not** a `Provider.spec` field: a
transient transition flag must not pollute the stable v1beta1 API and then need
deprecating once the refactor completes. Auditability — the part a spec field would
have bought — is provided instead by the operator **surfacing the active driver per
family as a `Provider.status` condition type and a metric** (e.g.
`virtrigaud_libvirt_active_driver{family}`), read-only and non-API-committing, so
the honesty need is met **without a schema change** (a new *condition type* on the
existing `conditions` array plus an `internal/obs/` metric — it keeps this ADR's
"no CRD change" invariant true). It also complements Risk 6's driver-aware
`GetCapabilities`. **Sunset:** the flag is removed and native made **unconditional
in the first minor release after the last operation family clears the flip-gate
below in production** — a transition flag that outlives its transition is not
allowed to become permanent supported config.

**Shadow-compare flip-gate — settled 2026-09-21 (blocking decision; resolves Open
question 2, and answers Open question 3).** Before PR 5 flips any read family to
native, the shadow harness must show **0 semantic divergences** — a
**canonicalizing comparator** excludes whitespace and field-order so only *meaning*
differences count — sustained **≥ 14 days in production**, spanning **≥ 2 libvirtd
restarts and ≥ 1 host reboot**, **plus** a completed **per-VM-shape coverage
checklist** (UEFI/NVRAM, snapshot-bearing, multi-disk, multi-NIC). The gate is
machine-measured by the `virtrigaud_libvirt_shadow_divergence_total{family,field}`
metric — which is also the answer to "where divergence is reported" (Open question
3): the metric *is* the evidence surface, read via a dashboard/alert on it. **PR 5
may not merge until that metric reads 0 across the full window and the checklist is
complete.** Strict, over a restart-and-reboot-spanning soak, is deliberate: it is
the difference between high confidence and letting "good enough" be defined by
whoever wants to ship.

### D7 — Drop the vestigial CGO **now**; distroless is the destination, **partial in v1**

**PR 0 sets `CGO_ENABLED=0` and drops `libvirt-dev` / `gcc` / `pkg-config` from
`cmd/provider-libvirt/Dockerfile` and `Makefile:213`. It ships before anything
else in this ADR.**

Because D4 keeps `virsh`, the runtime image **retains `libvirt-clients`** through
v1. Full distroless (matching `cmd/provider-mock/Dockerfile:29`) becomes available
only when migration moves off the subprocess — i.e. **at ADR-0007 P5**. State this
in the release notes; do not claim the 157 MB → 76 MB delta until it is real.

Independent of that, drop **`curl`** (its `HEALTHCHECK` at `Dockerfile:96-98` is
inert under Kubernetes, which uses probes) and **`bash`**.

### D8 — Deprecate SSH **password** authentication; key-based only

**Remove `sshpass` and the password branch in the same release as D3. Key-based
SSH authentication becomes the only supported mode.**

This is an **ADR-0004-class breaking change** and needs a release note, a
deprecation warning one release ahead, and a documented migration (generate a key,
update the credential Secret).

*Rationale*: `sshpass -e` puts a **hypervisor-root-equivalent password in the
`SSHPASS` environment variable**, readable from `/proc/<pid>/environ` by anything
sharing the namespace. ADR-0004 had to carry a special case for it throughout.
Going in-process with `x/crypto/ssh` removes the binary, the env exposure, and the
on-disk key write in one change — but only if we also remove the password path,
because keeping it means keeping `sshpass` and keeping the exposure. Half of this
change is worth very little.

### D9 — **Reconcile** PR #291; do not supersede it

**Retarget @jing2uo's work onto go-libvirt rather than closing it. Harvest the
seam design, the lifecycle findings, and the test cases; credit the work.**

Specifically:

- The **`controlTransport` seam** in #291 is the right shape and directly informs
  D5. It was independently arrived at.
- Review finding **B3** (stateful connection with no resilience) **transfers
  verbatim to go-libvirt — and is worse there**: the cgo binding at least *has*
  `SetKeepAlive` and `RegisterCloseCallback` and merely fails to use them;
  go-libvirt has **neither primitive at all** (Fact 5). B3 is therefore not a
  #291 bug we avoid by changing drivers — it is a requirement we inherit and must
  build.
- **B2** (production constructor does not fail closed) is the same defect as
  `provider.go:141-145`. One fix, both paths.
- **B6** (`verifyKnownHostsPresent` checks non-empty, not host-present) is a
  pre-existing gap the native path merely exposes. Fix in PR 3.
- **M2** (`DOMAIN_BLOCKED` folding to `Off`) is a benign example of exactly the
  **text→typed drift** class the D6 shadow harness exists to catch.
- The **45-VM validation** and its test cases are reusable evidence and should be
  the acceptance bar for PR 4/5.

*Rationale*: the contributor found the right root cause, built the right seam, and
validated it on real hardware at a scale we have not matched internally. The
disagreement is **one dependency choice**, and it is a choice their design is
already structured to accommodate. Closing that PR would discard a correct
architecture and an engaged contributor over a swap they can make in a
well-defined seam.

---

## Implementation plan — staging

Strategy: **strangler-fig behind a per-host connection seam, staged by operation
family, with production shadow-compare for reads and per-family flag-gated cutover
for writes.**

**The first four increments contain no go-libvirt at all.** That is deliberate:
they de-risk, they ship value independently, and they are the legitimate stopping
point if priorities change.

| PR | Content | Risk |
|---|---|---|
| **0** | Drop vestigial CGO: `Dockerfile:52` → `CGO_ENABLED=0`, drop `libvirt-dev`/`gcc`/`pkg-config`; `Makefile:213`. Drop `curl` + inert `HEALTHCHECK`. | Near-zero |
| **1** | Close the **7 exec-semaphore bypass sites**; add a **separate budget for streaming vs control**. | Low |
| **2** | **The seam** (D5), still virsh underneath: `HostID`/`Conn`/`Registry`; reshape `VirshProvider` into the per-host impl; **delete the six type assertions** (`server.go:269,358,400,454,501,669`); fix the swallowed `Initialize` error (`provider.go:141-145`). **Behaviour-preserving.** | Medium (wide, mechanical) |
| **3** | **In-process SSH** (D3): `x/crypto/ssh` + `knownhosts` `HostKeyCallback`; strengthen `verifyKnownHostsPresent` to check the *specific* host (B6); replace all **13** `exec.CommandContext` ssh/sshpass/scp sites; unify both virsh execution models onto "run virsh on the host over our own client". **virsh text parsing unchanged — zero semantic change.** Removes `sshpass`, `createSSHConfig`, the `/tmp` key write, the ControlMaster socket. Deprecate password auth (D8). | Medium — **highest value-to-risk ratio in the plan** |
| **4** | **Riskiest.** go-libvirt `socket.Dialer` + **full connection lifecycle** + reads in **shadow mode**. Lands the `libvirtxml` `<metadata>` fix (D2). | **High** |
| **5** | **Flip reads** — allowed only once the D6 flip-gate is met: `virtrigaud_libvirt_shadow_divergence_total` reads **0 semantic divergences ≥14 days in production**, spanning **≥2 libvirtd restarts + ≥1 host reboot**, with the per-VM-shape coverage checklist (UEFI/NVRAM, snapshot, multi-disk, multi-NIC) complete. | Medium |
| **6** | Lifecycle writes (create/define/start/stop/destroy/undefine), lab-first, flag-gated. | High |
| **7** | Reconfigure / snapshot / storage writes, lab-first, flag-gated. | High |
| **8** | **ADR-0007 P1**: registry gains N hosts, `ListHosts`, `target_host_id`. | — |
| **9** | **ADR-0007 P2** migration — **stays virsh per D4**, but still needs the additive proto RPC + `Unimplemented` stubs in vsphere/proxmox/mock. | — |

**PR 3 must precede any clustering work.** It is what deletes the
`LIBVIRT_DEFAULT_URI` process-environment hop (`virsh.go:273`) that hard-codes a
single host into the process. You cannot hold N connections while the URI lives in
`os.Environ()`.

**PR 4 detail — land the whole lifecycle before any flip.** Mutex-guarded
connection holder, redial, **`Close()` on the stale handle**, keepalive prober,
and per-call watchdog all land in PR 4, *before* a single read is flipped. The
failure mode is invisible and total (B3): dead-connection errors miss the
not-found classifier, so `Describe` fails for **every VM, every reconcile, until
pod restart**, while liveness reads a cached flag and reports healthy.

- Classify dead-connection errors as **evict-and-retry**, never operation-failed.
- **Per-call deadline mitigation (Fact 5):** run the RPC in a goroutine, select on
  `ctx.Done()`, and on expiry **close and evict the connection**. Document loudly
  that cancellation is **connection-scoped, not call-scoped** — concurrent
  siblings on that connection fail too. This is a real blast radius, not a
  footnote.
- **Separate connection for slow guest-agent sweeps**, so a `DescribeRich` sweep
  cannot head-of-line-block cheap `Describe` calls (B3's queue-instead-of-fork-storm
  failure).
- **Chaos test:** kill libvirtd, and separately kill the SSH transport, mid-flight;
  assert recovery **without pod restart**.

**Effort, honestly: roughly six to nine months part-time.** The port itself is not
the expensive part — **connection lifecycle, the shadow harness, and the migration
lab runbook are.**

**The legitimate de-scope: PR 0–3 alone.** They capture the fork-storm fix, the
`sshpass`/`openssh-client` removal, the read-only-rootfs unblock, and the seam —
**most of the operational win** — while leaving virsh in place. If this ADR stalls
after PR 3, it has still paid for itself.

---

## Testing strategy

Four tiers, because no single tier covers this.

**Tier 1 — pure unit (no libvirt).**
- XML **golden files** captured from real `virsh dumpxml` on live domains;
  canonical-tree equality on parse (D2's regression net).
- The existing **virsh text parsers must pass unchanged through the entire port**
  — they are the parity oracle for the shadow harness.
- Registry / watchdog / reconnect / error-classification tested against a **fake
  `socket.Dialer` returning a scripted `net.Conn`**. This is the right tool for
  lifecycle and error-path tests: it can produce a half-open socket, a mid-RPC
  EOF, and a hung read on demand, which no real daemon will do reliably.

**Tier 2 — real libvirtd + the `test://` driver, in CI, as a gate.**
libvirt's built-in test driver works and is a **first-class go-libvirt constant**
(`libvirt.TestDefault`). Proven twice: against a real libvirtd, and inside a
**fully isolated unprivileged Docker container** (`ubuntu:24.04` +
`libvirt-daemon-system`, **no qemu, no `--privileged`**), exit 0, covering domain
and storage-pool lifecycle. **No KVM and no nested virtualization needed** — so it
runs on stock GitHub runners.

It validates protocol framing, control flow, error classification, and **real
concurrency** — which the bundled `libvirttest` mock cannot (Fact 5). This closes
PR #291's **B4** (476 lines with no CI coverage).

*Limits, stated*: synthetic capabilities; no real qemu, storage, or guest agent;
**no migration**.

**Tier 3 — lab with real qemu.** Storage, snapshots, guest agent, S3/NFS relay —
plus the two cases most likely to bite on `undefine`: **a UEFI/NVRAM domain** and
**a snapshot-bearing domain** (see Risks).

**Tier 4 — production shadow-compare on reads** (D6). The real parity evidence; the
D6 flip-gate is measured here via
`virtrigaud_libvirt_shadow_divergence_total{family,field}`.

**Migration cannot be CI-tested. Say so plainly.** It is lab-only and needs a
written runbook covering: shared-storage happy path; block migration; abort and
rollback mid-flight; CPU-baseline mismatch rejection; destination failure at
switchover; and **pod death mid-migration → manual `virsh resume`** (D4's honest
cost).

---

## Security / compliance

VirtRigaud is documented as deployable in regulated banking environments. The
posture changes here are mostly improvements, with two honest qualifications.

**Improvements:**

- **`sshpass` and the `SSHPASS` environment variable are removed** (D8). A
  hypervisor-root-equivalent credential stops being readable from
  `/proc/<pid>/environ`.
- **The SSH private key stops being written to disk**, and the generated
  `~/.ssh/config` disappears — **unblocking a read-only root filesystem** for the
  provider pod.
- **`verifyKnownHostsPresent` starts verifying the actual host** (B6), closing the
  "seeded host A, pointed at host B" false-confidence gap in ADR-0004's
  first-contact window.
- **A memory-unsafe parsing class is removed** — `libxml2` and libvirt's C RPC/XML
  parsers process hypervisor-returned, partially tenant-influenced input; Go's
  `encoding/xml` plus a pure-Go RPC client removes memory corruption as a category,
  independent of any CVE count (Fact 7).
- **No new module tree**: `golang.org/x/crypto` is already indirect at
  `go.mod:102`; promoting `x/crypto/ssh` to direct adds nothing new to the
  supply chain.
- **No new listener, no new PKI, no change to the trust boundary** (D3/D4).

**Qualifications, stated plainly:**

- **The image win is partial in v1.** D4 keeps `virsh migrate`, so
  `libvirt-clients` and its C stack remain. Do not claim distroless or the
  157 MB → 76 MB delta until P5.
- **Cancellation gains blast radius.** Because go-libvirt has no per-call
  deadline, a context expiry evicts the whole connection (PR 4), failing
  concurrent siblings on that connection. This is a new failure mode that did not
  exist with `exec.CommandContext`.

**Supply chain:**

- `go-libvirt` pins to a **pseudo-version — upstream has never cut a semver tag**.
  Recorded as an accepted caveat; dependency bumps get explicit review.
- **CI does not scan the runtime images.** `.github/workflows/ci.yml:160-166`
  runs Trivy `scan-type: 'fs'` only, so the packages this entire ADR is about are
  never scanned. **Add `scan-type: 'image'` per built image, plus a scheduled
  re-scan of released tags** — a released image accumulates CVEs after it is
  built, and only a scheduled scan catches that.

**Deferred (P5):** the p2p migration PKI, with the residual that **a migration
client certificate is root-equivalent on the hypervisor and cannot be narrowed**
— see the D4 pre-analysis. That surface belongs in ADR-0007 P5's threat model,
with `security-architect` review.

---

## Consequences

**Positive.**

- The fork-storm class (#288) is closed at the root, including the **7 live
  bypass sites** it currently still leaks through.
- `Describe`/`ListVMs` latency collapses — PR #291 measured **865 sub-second
  `Describe` calls against a prior 30-second storm at 45 VMs**, on real hardware.
- A memory-unsafe parsing surface is removed from a banking-deployable component,
  and the provider joins every other provider at `CGO_ENABLED=0`.
- `sshpass`, the on-disk key, and `~/.ssh/config` are gone; read-only root
  filesystem becomes possible.
- **ADR-0007 P1 gets its seam for free** — the connection registry is built once
  and generalizes from 1 host to N by changing only the constructor.
- The plan de-risks itself: **PR 0–3 contain no go-libvirt** and deliver most of
  the operational win independently.

**Negative — the honest reality.**

- **Six to nine months part-time.** The port is not the expensive part;
  connection lifecycle, the shadow harness, and the migration runbook are.
- **We inherit an infrastructure obligation.** go-libvirt has no keepalive, no
  close callback, and **no per-call context** across ~488 methods. We build the
  liveness and deadline machinery ourselves, and cancellation becomes
  connection-scoped with real blast radius.
- **The image win is partial in v1** (D4 keeps `virsh`).
- **`virsh` never fully leaves.** 74 shell call sites are permanent, and migration
  keeps `libvirt-clients` until P5. "Remove virsh" was never the achievable goal;
  "remove the *control plane's* dependence on subprocess-per-call" is.
- **A silent-data-loss risk class is created and must be actively managed**:
  virsh's implicit flags, XML round-trip fidelity, and capability dishonesty
  during the transition (see Risks).
- **One deliberate breaking change** (D8, password auth) requires a deprecation
  cycle and release notes.

**Neutral but worth stating**: this ADR touches **no CRD and no proto** — the D6
driver-audit surfaces as a new *condition type* on the existing
`Provider.status.conditions` array plus an `internal/obs/` metric, neither of which
is a schema change. The gRPC contract changes only when ADR-0007 P1/P2 land
(PR 8/9).

### Top risks

1. **virsh implicit-flag drift — the silent-data-loss case.** `virsh undefine`
   applies NVRAM, snapshot, and managed-save handling flags that a naive
   `DomainUndefineFlags(dom, 0)` does **not**. **Audit every converted operation
   against the virsh source for the deployed version**, and lab-test the two
   domains that expose it: **a UEFI/NVRAM domain** and **a snapshot-bearing
   domain**.
2. **Text→typed drift.** The shadow harness (D6) exists for this. PR #291's **M2**
   (`DOMAIN_BLOCKED` folding to `Off`) is the benign archetype.
3. **XML round-trip fidelity.** Mitigated by rule, not by code: **never
   round-trip** (D2).
4. **Connection lifecycle.** B3, inherited and worsened. Mitigated by landing the
   full lifecycle in PR 4 before any flip, plus chaos tests.
5. **Loss of per-call deadlines.** Mitigated by the goroutine + evict watchdog,
   with the blast radius documented.
6. **Capability dishonesty mid-transition.** Make `GetCapabilities`
   **driver-aware**, so the reported surface reflects the **active** driver — and
   **capabilities must revert with the flag on rollback**. This is the easiest
   thing in the plan to get wrong and the hardest to notice: a stale capability
   after a rollback makes the manager attempt an operation the active driver does
   not implement, which is exactly the silent-no-op class the project has
   otherwise designed out.

---

## Open implementation questions

1. **Flag surface and lifetime. ✅ Resolved 2026-09-21 (see D6).** A **per-family
   env var** (`VIRTRIGAUD_LIBVIRT_NATIVE=…`), **not** a `Provider.spec` field (a
   transient flag must not pollute stable v1beta1); auditability comes from an
   operator-emitted status condition + metric. **Removed** — native made
   unconditional — in the first minor release after the last family clears the
   flip-gate in production.
2. **Shadow-compare divergence budget. ✅ Resolved 2026-09-21 (see D6).** **0
   semantic divergences** (canonicalizing comparator excludes whitespace/field
   order) **≥ 14 days in production**, spanning **≥ 2 libvirtd restarts + ≥ 1 host
   reboot**, plus a per-VM-shape coverage checklist (UEFI/NVRAM, snapshot-bearing,
   multi-disk, multi-NIC). PR 5 blocks until met.
3. **Where shadow divergence is reported. ✅ Answered by D6's flip-gate
   (2026-09-21).** Via the
   `virtrigaud_libvirt_shadow_divergence_total{family,field}` metric — the metric
   *is* the evidence surface. **The residual caution stands:** a metric nobody has a
   dashboard/alert for is not evidence, so the gate requires an actual dashboard on
   it before it can be read as met.
4. **Streaming vs control budget sizing.** PR 1 splits the budget — what are the
   two numbers, and are they env-tunable like `VIRTRIGAUD_LIBVIRT_MAX_CONCURRENT_VIRSH`?
5. **Guest-agent connection separation.** One extra connection per host, or a
   small pool? Interacts with the per-host fork/connection budget under ADR-0007's
   N hosts.
6. **`libvirtxml` `<metadata>` fix — vendor or upstream?** The ~50-line fix is
   verified. Upstreaming is correct but slow; carrying a patch is fast but is a
   supply-chain artifact we must track. Recommendation: upstream **and** carry
   until merged, with the carry documented.
7. **Capability vocabulary unification.** libvirt hardcodes a struct literal at
   `server.go:520-545` while proxmox and mock use `sdk/provider/capabilities`.
   Does driver-awareness (Risk 6) land on top of the literal, or do we unify
   first? Unifying first is cleaner and larger.
8. **Password-auth deprecation window.** One release with a warning, or two?
   Depends on known deployments — needs a maintainer call.
9. **PR #291 retarget mechanics.** Does @jing2uo retarget the existing branch, or
   do we land PR 2/3 first and have them rebase onto the seam? The latter is
   kinder to the contributor and lower-conflict, but slower.

---

## Follow-ups this ADR creates

- **`docs/adr/0007-clustered-orchestrator-provider.md`** — **amended by this ADR**
  (2026-07-20): the `execSem` "single global semaphore" claim corrected; migration
  confirmed virsh-based in v1 per D4; ADR-0008 PR 2–4 recorded as P1
  prerequisites; a new open question added for the P5 migration-mode/PKI decision.
- **New ADR at ADR-0007 P5** — automatic HA + fencing/STONITH **must now also
  decide migration mode (managed-virsh vs p2p) and, if p2p, the migration PKI**.
  The D4 pre-analysis above is its input.
- **`golang-engineer`** — PR 0 (CGO drop) and PR 1 (semaphore bypasses + split
  budget). Both are small, self-contained, and shippable immediately.
- **`staff-engineer`** — PR 2 (the seam + six type assertions + swallowed
  `Initialize`) and PR 3 (in-process SSH, 13 exec sites, `verifyKnownHostsPresent`).
  These cross `server.go`, `provider.go`, `virsh.go`, `storage.go`, `s3export.go`,
  `s3import.go`.
- **`virtualization-specialist`** — own the **virsh implicit-flag audit** (Risk 1),
  per operation, against the deployed virsh version; own the migration lab runbook;
  own the UEFI/NVRAM and snapshot-bearing `undefine` test cases.
- **`security-architect`** — review D8 (password-auth removal), the
  `verifyKnownHostsPresent` fix, the connection-scoped-cancellation blast radius,
  and the CI image-scanning gap. Separately, at P5, the p2p PKI residual.
- **CI** — (a) add `test://`-driver integration tests as a **gate** (closes #291
  B4); (b) add Trivy `scan-type: 'image'` per built image; (c) add a **scheduled**
  re-scan of released tags.
- **`tech-writer`** — release notes for D8 (breaking: password auth deprecated)
  and for the **partial** image slimming; the migration runbook's user-facing half;
  a note that read-only root filesystem becomes supported after PR 3.
- **Upstream** — the `libvirtxml` `<metadata>` namespace-declaration fix (D2),
  with the local carry documented until merged.
- **`internal/providers/libvirt/hostconn/`** — new package (D5), the artifact
  ADR-0007 P1 consumes.
- **Deferred defects recorded, not fixed here**: `HardwareUpgrade`
  (`proto/provider/v1/provider.proto:329`) unimplemented and silently falling
  through; the three competing capability vocabularies (Open question 7).
