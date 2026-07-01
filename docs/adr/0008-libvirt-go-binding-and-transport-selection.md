# ADR-0008: Libvirt Go binding & transport selection — official CGO binding + `qemu+libssh2`

## Status

**Proposed (2026-06-29).** Decides *which* native binding and *which* transport the
[ADR-0007](./0007-libvirt-native-control-transport.md) control-plane migration uses.

**Author**: Komh ([@jing2uo](https://github.com/jing2uo))

**Related**:

- [ADR-0007](./0007-libvirt-native-control-transport.md) — the migration this binding/transport
  choice serves (the *how*; this ADR is the *which*).
- [ADR-0004](./0004-libvirt-ssh-host-key-verification.md) — the trust contract preserved:
  same credentials Secret, same `known_hosts`, verification on by default, single insecure escape
  hatch, hard-fail on missing trust material, no TOFU.

---

## Decision

1. **Binding: the official CGO binding `libvirt.org/go/libvirt`** — not
   `digitalocean/go-libvirt`.
2. **Transport: `qemu+libssh2://`** with the host-key policy carried on the URI
   (`known_hosts` / `known_hosts_verify`) — not `qemu+ssh://`.

These are one coherent thesis: a control transport that is **complete**, **institutionally
maintained**, and whose **host-key verification is deterministic and auditable** without depending
on the system `ssh` client's config discovery.

---

## Context

The native migration needs a Go libvirt binding and a way to reach a remote host over SSH while
preserving ADR-0004 trust semantics. Two bindings are realistic candidates, and within the chosen
binding, libvirt offers more than one SSH transport. Both choices are security-significant: the
provider runs in a read-only-rootfs Kubernetes pod, and the host-key story behaves differently
there than on a developer laptop.

### The two candidate bindings

| | `libvirt.org/go/libvirt` (official) | `digitalocean/go-libvirt` |
|---|---|---|
| Mechanism | CGO over libvirt's C client | pure-Go, libvirt RPC wire protocol |
| CGO / `libvirt-dev` | **required** | not needed |
| Connection | libvirt URI (`NewConnect(uri)`), libvirt's own remote driver | caller supplies the socket (you dial SSH yourself) |
| API coverage | **complete** C API | generated 1:1 from RPC; gaps below |
| `ConnectIsAlive` (cheap liveness) | **yes** | **no** (hand-roll via a cheap RPC) |
| Typed params (memstat/blockstat) | typed helpers | **raw `nparams`**, hand-decode |
| Events / streams | yes | yes (streams hand-wired) |
| Reconnect | caller's job | caller's job (no helper) |
| Releases | **monthly semver**, latest v1.12003.0 (2026-04-28) | **0 tags ever**; pseudo-versions; README: *"API not stable… vendor it"* |
| Maintainers | **libvirt core devs** (Berrangé, Krempa, Privoznik, Hrdina @ Red Hat) | **bus-factor ≈ 1** (Geoff Hickey); both founders left DO |
| Backlog (verified) | 2 open issues, 0 open MRs | 13 issues, a 3-year-stale human PR |
| Sibling health | n/a | `go-qemu` ~13 months without a human commit |

Sources: gitlab.com/libvirt/libvirt-go-module, libvirt.org/uri.html, libvirt.org/golang.html;
github.com/digitalocean/go-libvirt (+ GitHub API for tags/commits/PRs), DigitalOcean's 2016
launch blog.

---

## Why the official binding wins for this project

Three reasons compound; none alone is decisive, together they are.

### 1. Institutional health (the biggest non-technical factor)

The official binding is maintained by the **libvirt project itself** — the same Red Hat engineers
who write libvirt — with **monthly semver releases tracking libvirt versions** and a
**near-empty backlog**. `go-libvirt` is a **single-maintainer** project under the DigitalOcean
banner where **both original authors have left the company**, ships **no tagged releases** (you
pin a SHA and vendor it), self-describes its API as *"not stable,"* and its sibling `go-qemu` has
gone ~13 months without a human commit. For a security-sensitive transport we expect to depend on
for years, "maintained by the libvirt team, monthly releases" beats "vendor-and-pin a
bus-factor-1 dependency."

### 2. Completeness — the gaps land on us with `go-libvirt`

The control surface we need is present in **both**. But `go-libvirt` specifically lacks things a
long-lived provider wants:

- **No `ConnectIsAlive`** — our cheap `Validate` probe (proven at ~30µs in the deployed provider)
  would need a hand-rolled substitute.
- **Raw typed-params** — memory/block stats come back as raw `nparams` to decode by hand, per call
  site, where the official binding gives typed helpers.
- **No reconnect helper** — both make us write reconnect, but go-libvirt gives less to build on.

None are blockers, but each is work the official binding hands us for free.

### 3. CGO is already paid, and the binding reuses existing trust material

The libvirt provider is **already a CGO build** (Makefile `CGO_ENABLED=1`; Dockerfile installs
`libvirt-dev`/`libvirt0`). So `go-libvirt`'s headline advantage — "no CGO" — is a benefit the
project **does not need to buy**; it already pays CGO for the current provider. Meanwhile the
official binding talks libvirt URIs, so it reuses the **exact same** credentials Secret,
`known_hosts`, and key/password auth the provider already mounts (ADR-0004) — no new trust surface.

> The one genuine virtue of `go-libvirt` is that it owns the SSH dial in Go, so host-key
> verification can be an explicit `ssh.HostKeyCallback`. That is a real strength — but the official
> binding reaches the **same** "explicit, deterministic, config-independent host-key verification"
> through `qemu+libssh2` URI params (below), **without** taking on go-libvirt's governance and
> completeness costs. So this virtue does not flip the decision.

---

## Why `qemu+libssh2://` over `qemu+ssh://`

This is the sharper half of the decision, grounded in a problem **reproduced** both on a laptop
and inside the deployed provider pod.

### How each transport enforces host keys

- **`qemu+ssh://`** shells out to the system `ssh` binary. Host-key policy
  (`StrictHostKeyChecking`, `UserKnownHostsFile`) is **not** expressible on the libvirt URI — it
  comes from the **ssh client config**. The provider therefore writes a `~/.ssh/config`
  (`createSSHConfig`) and hopes `ssh` reads it.
- **`qemu+libssh2://`** uses libssh2 **in-process**; host-key policy is carried **on the URI**:
  `known_hosts=<path>` + `known_hosts_verify=normal|auto|ignore`. No ssh client, no config file.

### The `qemu+ssh` fragility, reproduced

Modern OpenSSH resolves `~/.ssh/config` from the **password-database home directory**, and
**ignores `$HOME`**. Confirmed two ways:

- **Laptop (OpenSSH 10.3):** with `HOME` pointed at a temp dir holding a strict config, `ssh -G`
  still reported `stricthostkeychecking ask` and the *personal* `known_hosts`; `ssh -v` showed it
  read `/home/<user>/.ssh/config`, never the temp one.
- **Deployed provider pod (OpenSSH 9.2, read-only rootfs):** the passwd home `/home/app` is
  **read-only**, so `createSSHConfig` falls back to `/tmp/.ssh/config` and sets `HOME=/tmp`. We
  verified in-pod that **even with `HOME=/tmp`**, `ssh -G` resolves `stricthostkeychecking ask` +
  `/home/app/.ssh/known_hosts` and reads only `/etc/ssh/ssh_config` — the provider's policy
  **never reaches `ssh`**. `virsh` then fails: `Cannot recv data: Host key verification failed`.

So under `qemu+ssh`, in exactly the environment we deploy into, the host-key policy is **silently
undeliverable**. The verifying path can't connect, and the only way to make `virsh` connect is the
insecure opt-out (`no_verify=1`, which libvirt passes as a command-line `-o
StrictHostKeyChecking=no` — the one place ssh *does* honor it). That is a forced choice between
"broken" and "insecure" — the worst kind of fragility for a trust boundary, and exactly the
*"secure by accident"* drift ADR-0004 exists to prevent.

### `qemu+libssh2` makes it deterministic

Because libssh2 reads the policy off the URI, none of the above applies — the policy is enforced
in-process:

- **Correct key** in `known_hosts` → connects.
- **Wrong host key** → connection **rejected** with `!!! SSH HOST KEY VERIFICATION FAILED !!!:
  Identity of host … differs … man in the middle attack`, and `known_hosts` is **not** rewritten
  (no accept-new / no TOFU).
- **Missing/empty `known_hosts`** → the provider's pre-flight gate hard-fails with an actionable
  error (no TOFU), before any connection.
- **Explicit insecure opt-out** (`LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true`) → the single,
  audit-flagged bypass still works (maps to `known_hosts_verify=ignore`).

This is the **same ADR-0004 trust contract** — same `known_hosts` file, same hard-fail default,
same single opt-out — but enforced **in-process and config-independent**, so it holds in the
read-only pod where `qemu+ssh` cannot.

---

## Putting it together

- `go-libvirt`'s sole real advantage (explicit, Go-owned host-key verification) is **also
  achievable with the official binding** — via `qemu+libssh2` URI params — proven deterministic
  and config-independent.
- Choosing `qemu+libssh2` therefore **neutralizes the only reason to consider `go-libvirt`**,
  while the official binding keeps everything else it is better at: institutional maintenance, a
  complete API (incl. `IsAlive`/typed params), CGO already paid, and reuse of the existing
  ADR-0004 trust material.

→ **Official binding + `qemu+libssh2` = the explicit-verification virtue of go-libvirt, without its
governance and completeness costs.**

---

## Consequences

- New dependency `libvirt.org/go/libvirt` (CGO) + `libssh2-1` in the runtime image.
- Pinned to `qemu+libssh2`; `qemu+ssh` remains a usable fallback only where the deployment can
  guarantee the ssh client reads the provider config (e.g. a writable passwd home) — but
  `qemu+libssh2` is the default because it does not depend on that guarantee.
- The trust contract (ADR-0004) is preserved exactly; no new libvirt TLS/PKI surface is introduced.

---

## Operational caveats for `qemu+libssh2`

Seeding/usage details for the runbook — **not** binding deficiencies (libssh2 1.11.1 supports
OpenSSH-format keys, RSA-SHA2, and ed25519):

1. **Seed `known_hosts` with all host-key types.** libssh2 verifies against the algorithm it
   negotiates; a `known_hosts` holding only (say) ed25519 yields a spurious "identity differs" when
   another type is negotiated. Seed with an unrestricted `ssh-keyscan <host>` (not `-t ed25519`).
2. **The private key must be authorized on the host.** libssh2 offers only the `keyfile=` you pin,
   with no ssh-agent multi-key fallback, so an unauthorized key yields `Username/PublicKey
   combination invalid` — a *wrong-key* error, **not** a libssh2 limitation.
3. **Password auth** (`sshauth=password`) is the one path not yet exercised; verify before relying
   on it. Key auth is the supported path.
