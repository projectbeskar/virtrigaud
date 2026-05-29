# ADR-0004: Libvirt SSH host-key verification

## Status

**Accepted (2026-05-27)** — maintainer sign-off received; targeted at v0.3.7.

**Implementation**: code-complete on `v0.3.7/149-libvirt-ssh-hostkey`. The single
PR closes [#149](https://github.com/projectbeskar/virtrigaud/issues/149), lands the
fix + tests + CHANGELOG, and promotes this document to `docs/adr/0004-...`. The
operator-facing runbook and website security-page updates are intentionally
deferred to the **v0.3.7 release doc-sync** (the website documents released reality,
and v0.3.7 is unreleased at promotion time), mirroring ADR-0003.

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Related issues**:
- [#149](https://github.com/projectbeskar/virtrigaud/issues/149) — libvirt SSH skips host-key verification (`no_verify=1`)
- `I1` (PROJECT_CONTEXT, no GitHub issue) — libvirt SSH connectivity to `172.16.56.8` on `vr1.lab.k8` fails with `kex_exchange_identification: Connection closed by remote host`. **Related but distinct** — I1 is the cluster-specific data-plane symptom; #149 is the codebase-level host-key-verification fix. Their interaction is covered in "Interaction with I1" below.

**Companion ADRs**: [ADR-0001](./0001-transport-grpc-and-capi-integration.md) (gRPC transport), [ADR-0003](./0003-mtls-and-provider-grpc-auth.md) (mTLS + provider gRPC auth). ADR-0004 is the **provider→hypervisor** transport-trust sibling of ADR-0003's **manager→provider** transport-trust decision; it deliberately reuses ADR-0003's patterns (secure-by-default + named escape hatch, trust material co-located in the provider's existing Secret, loud-failure-on-missing-material).

### Decisions resolved 2026-05-27

The maintainer accepted every recommendation in this ADR. The Open Questions
section (preserved at the end) is superseded by this table.

| # | Question | Decision | Reflected in |
|---|---|---|---|
| (a) | Backwards-compat option | **Option C** — verify-by-default + named env escape hatch. Existing libvirt SSH Providers break on upgrade unless they supply `known_hosts` or set the escape hatch (intentional; v0.3.7 breaking-change callout). | "Decision" / "Backwards-compat — Option C" |
| (b) | Escape-hatch mechanism + name | **Env var `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION`** (value `"true"`), settable per-Provider via the existing `spec.runtime.env`. **No CRD change.** | "CRD surface — no schema change" |
| (c) | `known_hosts` Secret-key name + format | **Key `known_hosts`** (standard OpenSSH line format) in the **existing `credentialSecretRef` Secret**; mounts read-only at `/etc/virtrigaud/credentials/known_hosts` with zero controller change. | "Trust material source" |
| (d) | First-connect policy | **Hard-fail** with an actionable error on missing/unknown/mismatched host key. **No TOFU.** | "`known_hosts` absent + verification on = loud hard failure" |
| (e) | Release-note treatment | **Breaking-change callout** in the v0.3.7 release notes, same class as ADR-0003's nil-TLS-block loud failure. | "Consequences (Negative)" |

### Implementation status — as shipped (2026-05-27)

The "Implementation plan" section below preserves the **original design intent**.
A few details were sharpened during coding; this table is authoritative where it
conflicts with the per-section "Files touched" notes:

| Area | As planned (below) | As shipped |
|---|---|---|
| Where the policy lives | "centralize the on/off decision in `setupConnection`" | A dedicated file `internal/providers/libvirt/sshhostkey.go` holds a `hostKeyPolicy` value (`resolveHostKeyPolicy()`), with `sshHostKeyOptions()` / `sshConfigStanza()` / `applyURIHostKeyOptions()` / `verifyKnownHostsPresent()` / `logVerificationMode()`. `setupConnection` resolves it once into `VirshProvider.hostKey`; **all five** SSH/scp call sites consume that one value. This is stronger than the plan's "one branch in setupConnection" — there is exactly one host-key literal of each kind in the tree, in this file. |
| Verification-mode log + hard-fail re-emission | logged/checked once at startup in `setupConnection` | `setupConnection` logs + hard-fails for the virsh paths; the scp disk-copy path (`server.go copyDiskToRemote`) **re-emits** the verification-mode audit line and **re-runs** the hard-fail gate before transfer, because scp can be reached independently of the initial connection. |
| Logging backend | "the libvirt provider uses `slog` — check and match" | The host-key audit log uses `slog` (structured `provider`/`host`/`env_var`/`known_hosts` fields) on an injectable `VirshProvider.logger` (defaults to `slog.Default()`), so the WARN can be asserted via a captured handler in tests. The surrounding legacy `virsh.go`/`server.go` lines still use stdlib `log`; only the audit line was moved to `slog`, to keep the diff focused (no broad logging refactor — out of scope). |
| Empty `known_hosts` | hard-fail on absent | hard-fail on absent **or empty** (`os.Stat` size == 0): an empty file is no trust material and would silently fall through to ssh's default behaviour otherwise. |

**Interaction with I1 (decision (e), as-shipped):** the hard-fail error and a code
comment in `sshhostkey.go` (`verifyKnownHostsPresent`) both record that, post-#149,
an operator hitting I1 with a stale/missing `known_hosts` will now see
`Host key verification failed` (or the pre-flight error naming the path) instead of
the old `kex_exchange_identification: Connection closed`. The two failure modes stay
distinguishable by error string; this is the control working as designed, not a
regression.

No divergence from the accepted decisions themselves — no CRD change, env-var name
and Secret-key name as specified, hard-fail (no TOFU), Option C.

---

## Context

VirtRigaud is documented as **deployable in regulated banking environments**. The
libvirt provider reaches remote libvirt hosts over **SSH** (it is a virsh-CLI-over-SSH
provider, not a libvirt-go binding — confirmed below). Today that SSH connection is
made with **host-key verification disabled on every code path**. An attacker who can
intercept the manager→libvirt-host network (compromised intermediate hop, ARP
poisoning on the management VLAN, BGP hijack) can present their own SSH host key, and
the provider connects without warning — leaking the SSH credential and accepting
injected `virsh` commands against the hypervisor. This is below the bar the project's
compliance posture claims (NIST 800-53 SC-8 / IA-3 routinely require SSH host-key
verification in the secure-remote-management baseline).

Unlike ADR-0003 (a *wiring* gap — the machinery existed and just needed connecting),
#149 is a **default-posture** gap: the code actively and unconditionally opts out of a
security control. The fix is to flip the default and supply the trust material.

### Transport mechanism — virsh-CLI-over-SSH (this determines everything)

The libvirt provider is **not** libvirt-go with an SSH transport. It shells out to
`virsh` and `scp`. There are **two distinct execution paths**, and *both* disable
host-key verification:

1. **Key-based path** — `virsh` is invoked with `LIBVIRT_DEFAULT_URI` set to a
   `qemu+ssh://...` URI. libvirt's own ssh transport then execs the system `ssh`
   binary; the `no_verify=1` query parameter in the URI tells it to skip host-key
   checking. Set at `internal/providers/libvirt/virsh.go:176-177` (URI exported as
   env) and `:167` (the `no_verify=1`).

2. **Password path** — when a password credential is present, the URI is bypassed
   entirely. The provider builds explicit `sshpass -e ssh -o ...` argv (for `virsh`)
   and `sshpass -e scp -o ...` argv (for disk copy), pinning
   `StrictHostKeyChecking=accept-new` and `UserKnownHostsFile=/tmp/known_hosts` — a
   trust-on-first-use against an ephemeral, never-persisted file. See
   `internal/providers/libvirt/virsh.go:255-258, 287-290`,
   `internal/providers/libvirt/virsh.go:488-492` (the SSH config file), and
   `internal/providers/libvirt/server.go:694-695, 703-704` (scp).

A complete fix for #149 must therefore close **both** paths. The issue body cites only
`virsh.go:167`; the password path's `accept-new` + `/tmp/known_hosts` is an equivalent
gap that the issue does not mention.

### What's broken — evidence table (file:line)

| # | Location | What it does | Why it's a gap |
|---|---|---|---|
| 1 | `internal/providers/libvirt/virsh.go:167` | `query.Set("no_verify", "1")` appended unconditionally to every `qemu+ssh://` URI | Key-based path: libvirt's ssh transport skips host-key verification entirely. No `known_hosts` consulted. |
| 2 | `internal/providers/libvirt/virsh.go:255-258` | direct-command (`!`-prefixed) ssh: `-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/tmp/known_hosts` | Password path: TOFU against an ephemeral file inside the (read-only-root, `tmp`-emptyDir) pod — first connection always trusts whatever key is presented; file is lost on restart so it is TOFU forever. |
| 3 | `internal/providers/libvirt/virsh.go:287-290` | virsh-over-ssh: same `accept-new` + `/tmp/known_hosts` | Same as #2, for the primary virsh command path. |
| 4 | `internal/providers/libvirt/virsh.go:488-492` | `createSSHConfig` writes `StrictHostKeyChecking accept-new` + `UserKnownHostsFile /tmp/known_hosts` into `~/.ssh/config` | Belt-and-suspenders TOFU for any ssh invocation that reads the config. |
| 5 | `internal/providers/libvirt/server.go:694-695, 703-704` | `scp` for disk-to-remote copy: `accept-new` + `/tmp/known_hosts` | Disk image transfer to the hypervisor over an unverified channel — same MITM exposure, plus tampering of the transferred image. |

### How the SSH key is consumed today (so we know where to add trust material)

- The libvirt credentials Secret is referenced by `Provider.spec.credentialSecretRef`
  (`api/infra.virtrigaud.io/v1beta1/provider_types.go:221-222`) and mounted by the
  controller as a read-only volume at **`/etc/virtrigaud/credentials`**
  (`internal/controller/provider_controller.go:751-755`, volume defined at `:869-875`).
- The provider reads the SSH private key from the env var
  `LIBVIRT_SSH_PRIVATE_KEY`, falling back to the mounted file
  **`/etc/virtrigaud/credentials/ssh-privatekey`** (`internal/providers/libvirt/virsh.go:128-133`).
  The Secret-key name `ssh-privatekey` was confirmed during the v0.3.6 doc audit and
  matches Kubernetes' `kubernetes.io/ssh-auth` convention.
- The same mount also carries `username` and `password`
  (`internal/providers/libvirt/virsh.go:115-126`).

So a `known_hosts` entry placed in the **same credentials Secret** would mount,
read-only, at `/etc/virtrigaud/credentials/known_hosts` with **zero controller
changes** — the whole Secret is already projected into that directory. This is the
ADR-0003 "trust material co-located in the provider's Secret" pattern applied to SSH.

### Current CRD surface — there is *no* SSH/host-key field today

`Provider.spec` (`provider_types.go`) has `endpoint`, `credentialSecretRef`,
`runtime` (image/service/env/etc.), `defaults`, `healthCheck`. The only TLS-ish field
is `runtime.service.tls` (`ProviderTLSSpec`, `:64-78`) — that governs the
**manager→provider gRPC** channel (ADR-0003), *not* the provider→hypervisor SSH
channel. There is **no** libvirt-specific block, no `Spec.Libvirt`, no SSH struct.
Everything libvirt-connection-related is credential-Secret-driven or env-driven today.

This matters for the CRD-surface decision below: adding `Spec.Libvirt.SSH.*` (as the
issue body sketches) would be a **net-new CRD subtree**, triggering the `crd-update`
skill and a v1beta1 schema commitment. The env/Secret-driven alternative needs none.

### The escape-hatch precedent (ADR-0003 as-shipped)

ADR-0003 shipped its plaintext escape hatch as an **env var**, not a CRD field:
`VIRTRIGAUD_PROVIDER_INSECURE` (`sdk/provider/server/tlsconfig.go:61`, mirrored by the
controller constant `envProviderInsecure` at `provider_controller.go:105`). The
controller sets it only after `evaluateTLSPosture` confirms an *explicit* opt-out, so
an undecided Provider can never be silently downgraded
(`provider_controller.go:670-687`). #149's escape hatch should follow the same shape.

---

## Decision

**Make SSH host-key verification on by default for the libvirt provider, with an
explicit, audit-flagged, env-driven escape hatch — and no CRD schema change.** Trust
material (`known_hosts`) is supplied as a new key in the **existing libvirt
credentials Secret**. Missing trust material with verification on is a **loud,
actionable hard failure** at provider startup — not TOFU.

This ships in v0.3.7, alongside ADR-0003, because #149 is a documented HIGH-severity
compliance failure and we should not ship another release with a security control
actively disabled.

### Backwards-compat — **Option C: verify-by-default with an explicit escape hatch**

Existing v0.3.6 libvirt Providers connect with `no_verify=1` / `accept-new` today.
Turning verification on is a breaking change for them. The three options (mirroring
ADR-0003):

| Option | Behaviour for an existing libvirt Provider on upgrade | Trade-off |
|---|---|---|
| **(A)** Hard breaking — always verify, no escape | Provider refuses to connect until operator supplies `known_hosts`; no fallback | Loud and correct, but every libvirt upgrade requires operator action *before* the provider can do anything, including mid-migration |
| **(B)** Opt-in — verify only when `known_hosts` supplied; default stays `no_verify=1` | Silent upgrade, existing Providers keep working unchanged | The compliance gap simply persists; an operator who never supplies `known_hosts` is silently insecure forever — the exact "silent failure mode" #149 calls out |
| **(C)** Verify-by-default + named escape hatch | Provider refuses to connect unless `known_hosts` is present **or** the operator sets an explicit `*_INSECURE_SKIP_HOST_KEY_VERIFICATION` env opt-out (audit-flagged WARN) | Loud but reversible — operators *can* fall back for lab/migration, but only by typing the word "insecure", leaving an audit trail in logs |

**Option C wins**, and the reasoning is stronger here than the generic banking
argument:

1. **Consistency with ADR-0003.** Manager→provider (ADR-0003) and provider→hypervisor
   (#149) are the two transport-trust boundaries in the system. Having one default-on
   and the other default-off would be incoherent for an auditor.
2. **The I1 reality argues *for* the escape hatch, not against secure-default.** On
   `vr1.lab.k8` the libvirt data plane is currently broken (I1). An operator
   mid-migration, or debugging I1, genuinely needs a way to connect without first
   nailing down `known_hosts` — Option A would make I1 strictly harder to diagnose by
   adding a second failure mode on top of the connectivity failure. The escape hatch
   gives them that, *loudly*, instead of the current *silent* `no_verify=1`.
3. **Option B keeps the gap.** It is the status quo with extra steps; rejected.

### `known_hosts` absent + verification on = loud hard failure (not TOFU)

When the libvirt provider starts with verification on (the new default) and finds **no**
`known_hosts` material and **no** explicit insecure opt-out, it **refuses to operate**
and emits an actionable error — mirroring ADR-0003's `TLSNotConfigured` loud failure:

> `libvirt SSH host-key verification is on (default) but no known_hosts was found at /etc/virtrigaud/credentials/known_hosts. Add a 'known_hosts' key to the credentials Secret (see runbook: ssh-keyscan), or set LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true to connect without verification (audit-flagged, NOT recommended for production).`

**TOFU is rejected.** Trust-on-first-use accepts whatever key the host presents on the
first connection — which is *exactly* the MITM window #149 is about, just narrowed to
the first connect. In a hostile network the first connection is precisely when the
attacker strikes. TOFU also can't be audited (no operator decision is recorded). The
current `/tmp/known_hosts` behaviour is in fact *worse* than classic TOFU: because
`/tmp` is an emptyDir that is wiped on pod restart (`provider_controller.go:773-776`,
`server.go` mounts `tmp`), every restart re-TOFUs. We are removing this.

### Verification-mode startup log line (audit signal)

On startup the provider logs exactly one of:

- `libvirt SSH host-key verification: enabled (known_hosts: /etc/virtrigaud/credentials/known_hosts)`
- `libvirt SSH host-key verification: DISABLED via LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION` — at WARN level, mirroring ADR-0003's `InsecureSkipVerify` WARN.

Banking auditors grep for this line, same as they grep `TLSConfigured` for ADR-0003.

### How the SSH client is pointed at `known_hosts` (both paths)

Tied to the two transport paths found in the code:

- **Key-based path (URI):** stop appending `no_verify=1`. libvirt's ssh transport
  reads the standard ssh config / `known_hosts`. Point it explicitly by adding
  `-o UserKnownHostsFile=/etc/virtrigaud/credentials/known_hosts -o StrictHostKeyChecking=yes`
  via the ssh-config the provider already writes (`createSSHConfig`,
  `virsh.go:469-503`) — repurpose that function to write *verifying* config instead of
  `accept-new`.
- **Password path (explicit argv):** replace `StrictHostKeyChecking=accept-new` with
  `StrictHostKeyChecking=yes` and `UserKnownHostsFile=/tmp/known_hosts` with
  `UserKnownHostsFile=/etc/virtrigaud/credentials/known_hosts` in all four argv
  builders (`virsh.go:255-260`, `:287-292`, `server.go:693-697`, `:702-707`).
- **Escape hatch:** when `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true`, restore the
  *old* behaviour (`no_verify=1` on the URI; `StrictHostKeyChecking=accept-new` on the
  argv) plus the WARN log. One branch, set once in `setupConnection`.

### Trust material source — new key in the existing credentials Secret

| Option | Verdict | Reason |
|---|---|---|
| **New key `known_hosts` in the existing `credentialSecretRef` Secret** | **Chosen** | Already mounted read-only at `/etc/virtrigaud/credentials`; zero controller change; co-located with the SSH key it secures (ADR-0003 pattern). Standard OpenSSH `known_hosts` line format. |
| Separate ConfigMap (`KnownHostsConfigMapRef`) | Rejected for v0.3.7 | A host *public* key is not secret, so a ConfigMap is defensible — but it needs a *new CRD field* to reference, a second volume/mount in the controller, and splits libvirt trust material across two objects. Not worth the surface for v0.3.7. Can be a follow-up if operators ask. |
| `Provider` CRD field carrying the host key inline | Rejected | Puts operational trust material in the CR spec (drifts from the Secret-driven credential model), and inline-string host keys are a poor UX vs. `ssh-keyscan` output. Also a v1beta1 schema commitment. |

**Secret-key name:** `known_hosts`. **Format:** standard OpenSSH `known_hosts` lines,
one host per line:

```
172.16.56.8 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
vr1.lab.k8,172.16.56.8 ecdsa-sha2-nistp256 AAAAE2VjZHNh...
```

Multiple lines / multiple key types per host are fine (it is just the standard file).
The Secret may be `kubernetes.io/ssh-auth` (already used for `ssh-privatekey`) or
`Opaque` — both project the key into the mount identically.

### CRD surface — **no schema change** (strong preference honoured)

The issue body sketches `Spec.Libvirt.SSH.{SkipHostKeyVerification,KnownHostsSecretRef,KnownHostsConfigMapRef}`.
This ADR **rejects the new CRD subtree** for v0.3.7, because the same outcome is
reachable with mechanisms that already exist:

- **Trust material** → new `known_hosts` key in the Secret that
  `credentialSecretRef` *already* points at. No new ref field needed.
- **Escape hatch** → env var `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION`, settable
  per-Provider via the **existing** `spec.runtime.env` field
  (`provider_types.go:148-151`), exactly the way ADR-0003 surfaces
  `VIRTRIGAUD_PROVIDER_INSECURE`. The operator writes
  `runtime.env: [{name: LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION, value: "true"}]`.

This mirrors how ADR-0003 mostly avoided CRD changes. If maintainer review concludes a
typed CRD field is genuinely required (e.g. to surface a Condition or to validate the
value), **that is a bigger commitment** — it triggers the `crd-update` skill, a
`zz_generated.deepcopy.go` regen, Helm CRD sync, and a v1beta1 schema-stability review.
Flagged loudly as Open Question (b). The ADR's recommendation is to stay
env/Secret-driven.

> Note: the manager could *optionally* set the env var itself when it detects the
> known_hosts key is absent and the operator has annotated the Provider — but that
> reintroduces controller logic. The simplest v0.3.7 shape is: operator sets the env
> var explicitly via `runtime.env`, provider honours it. No controller change at all.

---

## Bootstrap / first-connect UX

There is no automatic bootstrap. The operator obtains the host key out-of-band and
seeds the Secret. The runbook shape (pointer-quality, like ADR-0003's openssl recipe):

**1. Scan the libvirt host's key from a trusted bastion / jump host** (NOT from the
pod — the whole point is an out-of-band trust anchor):

```bash
ssh-keyscan -t ed25519,ecdsa,rsa 172.16.56.8 > known_hosts
# Verify the fingerprint against what the libvirt host's admin reports:
ssh-keygen -lf known_hosts
```

**2. Put it in the existing credentials Secret** (alongside `ssh-privatekey`):

```bash
kubectl create secret generic libvirt-creds \
  --namespace virtrigaud-system \
  --from-file=ssh-privatekey=./id_ed25519 \
  --from-file=known_hosts=./known_hosts \
  --from-literal=username=virtrigaud \
  --dry-run=client -o yaml | kubectl apply -f -
```

(Or `kubectl patch`/`kubectl edit` an existing Secret to add the `known_hosts` key.)

**3. Verify** the provider picked it up:

```bash
kubectl logs deploy/provider-libvirt-... | grep 'host-key verification'
# expect: libvirt SSH host-key verification: enabled (known_hosts: /etc/virtrigaud/credentials/known_hosts)
```

Secret-to-pod sync (~60s) re-projects the file without a pod restart, so rotating a
host key (e.g. after a hypervisor rebuild) is `kubectl apply` + wait — no rollout.

---

## Interaction with I1

This is a **diagnostic note for whoever debugs I1 after #149 lands**, not a fix for I1.

I1 today surfaces as `kex_exchange_identification: Connection closed by remote host` —
a failure *before* the SSH key exchange completes (host-side sshd config, fail2ban
rate-limiting, firewall). Host-key verification happens *after* key exchange, so #149
**does not change the I1 signature** in the I1-as-currently-observed case: a host that
closes the connection at `kex` never gets far enough to present a host key.

What *will* change: once I1's connectivity is fixed and the host responds normally, an
operator with a **stale or missing** `known_hosts` entry will now see
`Host key verification failed` (a clean, post-kex error) instead of the old silent
`no_verify=1` success. Document this in the I1 ticket so the debugger isn't confused by
a *new* error appearing after the connectivity fix — it is the security control working
as designed, not a regression. The two failure modes are orthogonal and distinguishable
by the error string (`kex_exchange_identification` = connectivity/host-side;
`Host key verification failed` = trust material mismatch).

This also retroactively validates #149's "operational observation": with verification
on, the I1-class failure would have been *more* diagnosable (a trust error names the
problem) rather than masked behind `no_verify=1`.

---

## Implementation plan — single PR

#149 is narrower than ADR-0003 (which needed 4 sequential PRs to wire two ends, an
auth interceptor, and hot-reload). The host-key change is confined to the libvirt
provider package plus its docs. **One focused PR.**

**Files touched:**
- `internal/providers/libvirt/virsh.go` — the crux:
  - `:167` — gate `no_verify=1` behind the insecure opt-out (default: omit it).
  - `:255-260`, `:287-292` — `StrictHostKeyChecking=yes` +
    `UserKnownHostsFile=/etc/virtrigaud/credentials/known_hosts` (insecure path keeps
    `accept-new` + `/tmp/known_hosts`).
  - `:469-503` (`createSSHConfig`) — write verifying config; point at the mounted
    `known_hosts`.
  - `setupConnection` (`:142-197`) — read the opt-out env var once; emit the
    verification-mode log line; loud-fail if verification on and `known_hosts` absent.
- `internal/providers/libvirt/server.go` — `:693-697`, `:702-707` (scp): same argv
  swap as the virsh path.
- **No controller change** (the Secret is already mounted; the escape-hatch env var
  rides the existing `runtime.env` path). No CRD change. No Helm change.

**Tests required** (mirroring ADR-0003's test shape):
- verification-on + matching `known_hosts` present → URI/argv contain
  `StrictHostKeyChecking=yes`, point at the mounted file, **no** `no_verify`/`accept-new`.
- verification-on + `known_hosts` absent + no opt-out → `setupConnection` returns the
  loud actionable error (assert the message names the path and the env var).
- escape-hatch env set → URI keeps `no_verify=1` / argv keeps `accept-new`, and a WARN
  log is emitted (mirror the mTLS `InsecureSkipVerify` WARN test).
- argv builders for both the `virsh` path and the `scp` path are covered (the issue
  body misses scp — the test must not).
- table-test the `setupConnection` env-var parsing (`unset`/`"false"`/`"true"`/`"TRUE"`).

**CHANGELOG shape** (per the strict project format): a `### Security` entry under a
`## [YYYY-MM-DD HH:MM] - Libvirt SSH host-key verification on by default` header,
`**Author:** @williamrizzo (William Rizzo)`, `### Why` citing #149 / MITM / banking
posture, and `### Impact` checking **Breaking change** and **Requires cluster rollout**
(operators relying on implicit `no_verify=1` must add `known_hosts` or the opt-out env
before the libvirt provider will connect).

**Rollback:** revert the single PR; libvirt SSH goes back to `no_verify=1` /
`accept-new` everywhere (v0.3.6 behaviour). No state migration, no CRD rollback.

**Why not >1 PR:** the change is one provider package + tests + docs. There is no
cross-package contract to sequence (unlike ADR-0003's resolver↔controller↔SDK chain).
If review decides a typed CRD field *is* required after all, that splits into (1) the
CRD-field PR (with `crd-update`) and (2) the wiring PR — but the recommendation is to
avoid that and stay single-PR.

---

## Out of scope

- **The I1 data-plane investigation itself.** I1 is host-side sshd/fail2ban/firewall on
  `172.16.56.8`, not VirtRigaud code. Tracked separately in PROJECT_CONTEXT. #149 only
  notes how its failure signature changes post-fix (above).
- **mTLS / provider-gRPC-auth** (#147/#148, ADR-0003, done) — a different transport
  boundary (manager→provider, not provider→hypervisor).
- **Any non-libvirt provider.** vSphere and Proxmox use their own API transports
  (already TLS-bearing); they have no SSH host-key surface.
- **A typed CRD `Spec.Libvirt.SSH` block** — explicitly deferred unless maintainer
  review reverses Open Question (b). The env/Secret mechanism covers v0.3.7.
- **ConfigMap-based `known_hosts` distribution** — defensible (host public keys aren't
  secret) but needs a new ref field; deferred to a follow-up if demanded.
- **Migrating libvirt off virsh-CLI-over-SSH onto a native libvirt-go transport** — a
  much larger refactor; orthogonal to host-key trust.

---

## Consequences

### Positive
- **v0.3.7 closes the last disclosed transport-trust gap.** Both the manager→provider
  (ADR-0003) and provider→hypervisor (#149) channels are secure-by-default, coherent
  for an auditor.
- **No CRD schema change.** v1beta1 stays put; no `crd-update` churn; trust material
  rides the existing credential Secret.
- **No controller change.** The Secret is already mounted; the escape hatch rides
  `runtime.env`. The blast radius is one provider package.
- **Loud, auditable posture.** Startup log line + hard-fail-on-missing-material; an
  auditor greps one log line, exactly like ADR-0003's `TLSConfigured`.
- **Removes the worse-than-TOFU `/tmp/known_hosts` re-trust-on-every-restart behaviour.**

### Negative — release-note callout required for v0.3.7
- **Existing v0.3.6 libvirt Providers that rely on the implicit `no_verify=1` will stop
  connecting after upgrade until the operator either (a) adds a `known_hosts` key to the
  credentials Secret OR (b) sets `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true` in
  `runtime.env`.** This is intentional. **The v0.3.7 release notes must call this out in
  the same breaking-change callout class as ADR-0003's nil-TLS-block loud failure.**
- **Two failure modes now exist on the libvirt SSH path** (connectivity vs. host-key
  mismatch). Mitigated by distinct, greppable error strings (documented under
  "Interaction with I1").
- **Operator friction:** seeding `known_hosts` is a manual `ssh-keyscan` + Secret edit.
  Conscious trade-off; mitigated by the runbook recipe. Same friction class as
  ADR-0003's manual cert provisioning.

## Security implications summary

| Item | Status after v0.3.7 |
|---|---|
| Libvirt SSH host-key verification | **On by default.** Per-Provider opt-out via `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true` (audit-flagged WARN). |
| Trust material source | New `known_hosts` key in the existing `credentialSecretRef` Secret, mounted read-only at `/etc/virtrigaud/credentials/known_hosts`. |
| First-connect behaviour | **Hard fail** on missing/mismatched host key. No TOFU. |
| MITM on the manager→libvirt SSH path | Blocked when verification on (both key-based and password/scp paths). |
| Disk-image transfer (scp) | Verified host key on the same trust material — closes the gap the issue body omits. |
| Audit visibility | One startup log line: `libvirt SSH host-key verification: enabled\|DISABLED via ...`. |
| CRD impact | **None.** No v1beta1 schema change. |
| Ephemeral `/tmp/known_hosts` re-TOFU on restart | Removed. |

---

## Open Questions (resolved 2026-05-27 — see "Decisions resolved" table at top)

> **Superseded.** All five questions below were resolved by the maintainer on
> 2026-05-27; the recommendations were accepted verbatim. This section is retained
> for the design-record trail. See the "Decisions resolved 2026-05-27" table at the
> top for the binding outcomes.

1. **(a) Backwards-compat option — confirm C.** The ADR recommends Option C
   (verify-by-default + named env escape hatch), for consistency with ADR-0003 and
   because the I1 reality argues for a reversible hatch rather than a hard break (A).
   Confirm, or pick A/B with rationale.

2. **(b) Escape-hatch mechanism + exact name.** The ADR recommends an env var,
   `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION`, set per-Provider via the existing
   `spec.runtime.env` — **no CRD change**, mirroring ADR-0003's
   `VIRTRIGAUD_PROVIDER_INSECURE`. Confirm the name, and confirm we are NOT adding a
   typed `Spec.Libvirt.SSH.SkipHostKeyVerification` CRD field (which would trigger
   `crd-update` and a v1beta1 commitment). If you want the typed field, say so — it
   re-shapes the implementation into two PRs.

3. **(c) `known_hosts` Secret-key name + format.** The ADR recommends key name
   `known_hosts`, standard OpenSSH `known_hosts` line format, in the existing
   `credentialSecretRef` Secret (mounts at `/etc/virtrigaud/credentials/known_hosts`).
   Confirm the key name and that we are reusing the existing Secret (vs. the issue
   body's `KnownHostsSecretRef`/`KnownHostsConfigMapRef` separate-object approach).

4. **(d) First-connect policy — confirm hard-fail over TOFU.** The ADR rejects TOFU
   (it preserves the MITM window #149 is about and can't be audited) in favour of a
   loud hard failure with an actionable message. Confirm, or accept TOFU with rationale.

5. **(e) Release-note treatment — breaking change requiring operator action?** The ADR
   treats this as the same callout class as ADR-0003's nil-TLS-block loud failure:
   existing libvirt operators must add `known_hosts` or the opt-out env before upgrade,
   or the provider stops connecting. Confirm this lands in the v0.3.7 release notes as a
   loud breaking-change callout (and whether it warrants a pre-upgrade check in the
   release runbook).
