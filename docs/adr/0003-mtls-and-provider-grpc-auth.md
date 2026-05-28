# ADR-0003: Wire mTLS and provider gRPC authentication

## Status

**Accepted (2026-05-27)** — maintainer sign-off received; targeted at v0.3.7.

**Implementation**: code-complete on `main`. PR-1 [#157](https://github.com/projectbeskar/virtrigaud/pull/157), PR-2 [#158](https://github.com/projectbeskar/virtrigaud/pull/158), and PR-3 [#159](https://github.com/projectbeskar/virtrigaud/pull/159) landed the wiring; this document is the PR-4 promotion of the design record. The operator-facing provisioning runbook and the website security-page updates are intentionally deferred to the **v0.3.7 release doc-sync** — the website documents released reality, and v0.3.7 is unreleased at promotion time. Tracked under umbrella [#156](https://github.com/projectbeskar/virtrigaud/issues/156).

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Related issues**:
- [#147](https://github.com/projectbeskar/virtrigaud/issues/147) — wire mTLS through `Resolver.buildTLSConfig`
- [#148](https://github.com/projectbeskar/virtrigaud/issues/148) — provider gRPC servers don't enable `Auth.RequireTLS` / `BearerTokenAuth`

**Companion ADRs**: [ADR-0001](./0001-transport-grpc-and-capi-integration.md) (chose gRPC), [ADR-0002](./0002-build-path-consolidation.md) (`certwatcher` already in canonical manager).

### Implementation status — as shipped on `main` (2026-05-27)

The "Implementation Plan" section below preserves the **original design intent**. Three details shifted during implementation; this table is authoritative where it conflicts with the per-PR "Files touched" notes:

| Area | As planned (below) | As shipped |
|---|---|---|
| Provider-side config contract | `TLS_CERT_PATH` / `TLS_KEY_PATH` / `TLS_CA_PATH` / `AUTH_ALLOWED_SANS` env vars | On-disk cert detection at `/etc/virtrigaud/tls` + `VIRTRIGAUD_PROVIDER_ALLOWED_SANS` + `VIRTRIGAUD_PROVIDER_INSECURE`, resolved by `sdk/provider/server.ResolveTLSAndAuth` (constants `EnvAllowedSANs` / `EnvInsecure` in `sdk/provider/server/tlsconfig.go`). |
| Global escape hatch | `--insecure-no-tls-providers` manager flag (PR-3) | **Not implemented.** The only escape hatch is per-Provider `tls.enabled=false`, which the controller translates into `VIRTRIGAUD_PROVIDER_INSECURE=true` on the provider pod. A nil `tls` block is still a loud failure (decision #3). |
| Example YAML + operator runbook | `examples/providers/provider-with-mtls.yaml`, `tls-secret-template.yaml`, `docs/operations/security.md` (PR-1/PR-4) | **Deferred to the v0.3.7 release doc-sync.** The canonical openssl recipe lives in this ADR ("Manual cert provisioning recipe") in the meantime. |

Everything else (mTLS wiring, loud-failure Condition, `validateTLSPeer` SAN allow-list with permissive empty default, libvirt SDK migration, `AutoReload` certwatcher hot-reload) shipped as designed. Known limitation: only the **leaf** cert hot-reloads; rotating the **CA bundle** still requires a provider pod restart.

### Decisions resolved 2026-05-27

| # | Question | Decision | Reflected in |
|---|---|---|---|
| 1 | Cert provisioning posture | **Manual provisioning only** — no Helm-gated cert-manager `Certificate` template; chart exposes an externally-provisioned `tls.secretName` reference only. | "Cert provisioning", "Out of scope", "Manual cert provisioning recipe", PR-3/PR-4 plans |
| 2 | Backwards-compat option | **Option C** — TLS on by default in v0.3.7. | "Decision" / "Backwards-compat option chosen" |
| 3 | `Spec.Runtime.Service.TLS == nil` semantic | **Loud failure** — manager refuses to reconcile, surfaces a Condition. | "Decision", PR-1 plan, "Consequences" |
| 4 | Secret-key convention | **Split** — TLS uses `tls.crt`/`tls.key`/`ca.crt`; credentials stay per-provider. | "Secret layout" |
| 5 | `AllowedSANs` empty | **Permissive** — empty list trusts any client cert signed by the configured CA. | "Auth layer", "Consequences (Negative)" |

## Context

VirtRigaud is documented as **deployable in regulated banking environments**. The honest v0.3.6 reality, captured on the website's `src/providers/security/mtls.md` and `bearer-token.md` pages during the 2026-05-27 doc audit, is that **manager↔provider gRPC traffic is plaintext** and **provider servers accept any caller on the pod network**. This is below the bar the project's compliance posture claims.

The mismatch is not a missing-feature gap — it is a wiring gap. The transport-credential machinery exists at both ends and is correctly implemented. The CRD field exists and is validated. Only the four pieces of glue connecting them are absent.

### What is already correct in v0.3.6

| Component | Evidence |
|---|---|
| Manager-side gRPC client builds TLS credentials from `(CertFile, KeyFile, CAFile)` | `internal/transport/grpc/client.go:951-991` |
| `NewClient` consumes a `*TLSConfig` and falls back to `insecure.NewCredentials()` only when nil | `internal/transport/grpc/client.go:94-108` |
| SDK gRPC server builds TLS credentials with optional `RequireClientCert` | `sdk/provider/server/server.go:340-358` |
| SDK middleware has an auth interceptor gated on `Auth.RequireTLS` / `Auth.BearerTokenAuth` | `sdk/provider/middleware/middleware.go:141-145, 222-259` |
| CRD field for declarative TLS configuration | `api/infra.virtrigaud.io/v1beta1/provider_types.go:50-78` |
| Manager already runs `certwatcher` for webhook + metrics certs | `cmd/manager/main.go:161-228` (ADR-0002 PR-1) |

### What is broken or stubbed

| Component | Evidence |
|---|---|
| `Resolver.buildTLSConfig` returns `(nil, nil)` unconditionally | `internal/runtime/remote/resolver.go:143-148` — guarded by `if true { return nil, nil }` |
| `ProviderController` hardcodes `tlsEnabled := false` and ignores `Spec.Runtime.Service.TLS` | `internal/controller/provider_controller.go:509` and `:704` |
| vSphere/Proxmox/Mock providers wire `middleware.Config` but omit the `Auth` field | `cmd/provider-{vsphere,proxmox,mock}/main.go` |
| Libvirt provider bypasses the SDK server entirely and uses raw `grpc.NewServer()` | `cmd/provider-libvirt/main.go:71` |
| `middleware.validateTLSPeer` is a TODO stub — peer SAN allow-listing is unenforced | `sdk/provider/middleware/middleware.go:262-273` |
| `server.TLSConfig.AutoReload` declared but not implemented | `sdk/provider/server/server.go:86-87` |

### Current CRD surface — `Provider.spec.runtime.service.tls`

Defined in `api/infra.virtrigaud.io/v1beta1/provider_types.go:50-78`:

```go
type ProviderServiceSpec struct {
    Port int32             `json:"port,omitempty"`             // default 9443
    TLS  *ProviderTLSSpec  `json:"tls,omitempty"`
}

type ProviderTLSSpec struct {
    Enabled             bool                          `json:"enabled,omitempty"`             // default true
    SecretRef           *corev1.LocalObjectReference  `json:"secretRef,omitempty"`           // tls.crt + tls.key + ca.crt
    InsecureSkipVerify  bool                          `json:"insecureSkipVerify,omitempty"`  // default false
}
```

Validation markers and JSON tags are stable. No schema change is required.

### Trust model in cluster today (compensating control)

In a default v0.3.6 deployment, the only thing keeping manager↔provider traffic from being trivially sniffed or hijacked is operator-supplied NetworkPolicy plus an encrypted CNI overlay. Neither is enforced by VirtRigaud. This ADR replaces those compensating controls with in-process authn/authz — they remain available as defence-in-depth.

## Decision

**Wire mTLS end-to-end against the existing CRD surface and turn it on by default with a documented escape hatch.** v0.3.7 ships **Option C** below for backwards compatibility, with no schema-breaking changes to `ProviderTLSSpec`. The default-flip happens in v0.3.7 itself, not v0.4.0, because the gap is a documented compliance failure and we should not ship another release in this state.

### Backwards-compat — **Option C: TLS on by default, with explicit escape hatch**

| Option | Behaviour for unset `runtime.service.tls` | Trade-off |
|---|---|---|
| **(A)** Hard breaking — no escape | Manager refuses to dial; `Degraded` until certs supplied | Loud and correct, but every upgrade requires operator action *first* |
| **(B)** TLS opt-in via existing field | Manager dials plaintext as today, only encrypts when `tls.enabled=true` | Silent upgrade, but compliance posture unchanged |
| **(C)** TLS-on-by-default + escape hatch | Manager refuses unless `tls.enabled=false` is set explicitly OR `--insecure-no-tls-providers` is passed | Loud but reversible — operators *can* fall back, but they have to type the word "insecure" |

Option C wins because banking posture demands secure-by-default; the per-Provider escape hatch keeps the upgrade non-fatal; discoverability is preserved via a Provider-CR Condition (`TLSConfigured=False, Reason=ExplicitlyDisabled`) that banking auditors can grep.

### `Spec.Runtime.Service.TLS == nil` — loud failure

When an operator upgrades a v0.3.6 Provider CR to v0.3.7 and the `tls` block is unset (nil), the manager **refuses to reconcile** and surfaces a Condition on the Provider with this exact framing:

> `type=Ready, status=False, reason=TLSNotConfigured`. Message: "TLS is default-on in v0.3.7. To proceed, either provision a TLS Secret and set `runtime.service.tls.enabled=true` with `secretRef`, or explicitly set `runtime.service.tls.enabled=false` to keep plaintext (audit-flagged)."

The Condition message is part of the v0.3.7 release UX. The implementation owner is **`internal/controller/provider_controller.go`** — the reconcile path that today hardcodes `tlsEnabled := false` at line 509. PR-1 owns this Condition.

This is the secure-by-default posture the banking-compliance buyer expects. The escape hatch (`tls.enabled=false`) is trivially applicable and per-Provider, so the upgrade is recoverable without a full rollback.

### Trust model — single CA per VirtRigaud install

- **One CA per install.** Manager holds a client cert + key + CA. Each provider has a server cert + key + the same CA. All provider pods trust the same CA; all share the same trust domain. Matches the operator's expectation that one VirtRigaud install is one administrative boundary.
- **Cert provisioning is the operator's responsibility.** The controller reads a Kubernetes Secret containing `tls.crt` / `tls.key` / `ca.crt`. How those bytes get there — manual openssl, an external PKI pipeline, Vault, External Secrets, or cert-manager layered on top by the operator — is out of scope for VirtRigaud. The chart ships **no** cert-manager scaffolding. (See "Out of scope" and "Manual cert provisioning recipe".)
- **Per-provider trust roots are deferred** to a follow-up ADR. The CRD field is per-Provider so the data path can already carry a per-Provider CA bundle; only the *manager* side is currently single-CA.
- **SPIFFE / SPIRE identity is out of scope.**

### Cert rotation — file watch via `certwatcher`

The canonical manager already runs `sigs.k8s.io/controller-runtime/pkg/certwatcher` for webhook + metrics certs. Reuse the same mechanism for the gRPC client cert and the provider server cert:

- **Manager-side**: gRPC client cert loaded via `certwatcher.New(...)` and `tls.Config.GetClientCertificate` rather than `tls.LoadX509KeyPair` once at boot. `Resolver.buildTLSConfig` returns a `*tls.Config` whose `GetClientCertificate` callback pulls from the watcher on every dial.
- **Provider-side**: implement the `AutoReload` field that already exists on `sdk/provider/server/server.go:86-87`. When `AutoReload=true` (default), the SDK server wraps cert/key loading in a `certwatcher.CertWatcher`.
- **Per-Provider Secret rotation** is handled by Kubernetes' standard Secret-to-Pod sync (~60 s). The file watcher on the mounted path picks up the new bytes without a restart.

No pod restarts for cert rotation, regardless of how the certs were minted.

### Secret layout — split convention (TLS vs. credentials)

The mounted TLS Secret on each pod (manager + each provider) contains:

| Key | Required | Source |
|---|---|---|
| `tls.crt` | yes | server certificate (or client cert on manager side), PEM |
| `tls.key` | yes | private key, PEM |
| `ca.crt` | yes | CA bundle used to verify the *peer's* cert |

This matches kube-apiserver / cert-manager convention, so operators who layer cert-manager themselves can point the chart at a Secret that cert-manager produces with zero translation.

**TLS Secrets and credential Secrets stay distinct, at different mount paths, with different key conventions:**

- `runtime.service.tls.secretRef` → `tls.crt` / `tls.key` / `ca.crt` (mounted at `/etc/virtrigaud/tls/`)
- `credentialSecretRef` → per-provider keys (`ssh-privatekey` for libvirt, `token_id`/`token_secret` for proxmox, `username`/`password` for vsphere; mounted at `/etc/virtrigaud/credentials/`)

Two distinct Secrets per Provider CR. No collision.

### Auth layer on top of TLS — client cert identity is sufficient

Once mTLS is wired:

- Provider gRPC server verifies the manager's client cert against the configured CA (`tls.RequireAndVerifyClientCert`, present at `sdk/provider/server/server.go:353`).
- Cert SAN/CN identifies the manager. The SDK's `Auth.AllowedSANs` allow-list is the authorization gate. **Implementing `validateTLSPeer` is required** — it is currently a TODO at `sdk/provider/middleware/middleware.go:262-273` and accepts any TLS-authenticated caller.
- **Empty `AllowedSANs` = trust any client cert signed by the configured CA.** This matches kube-apiserver client-cert auth behaviour. The security trade-off is explicit: empty SAN list assumes the CA is trustworthy (the operator isn't sharing the CA with workloads they don't trust). Operators who want SAN-level allow-listing populate the list.
- No bearer token is needed in v0.3.7 — the manager is the only legitimate caller. `Auth.BearerTokenAuth` and the corresponding manager-side bearer-token injection remain available but unused.

Plan B if a deployment categorically cannot use mTLS yet: the per-Provider `tls.enabled=false` escape hatch. Compensating controls (NetworkPolicy + encrypted CNI) remain valid for those Providers. The manager logs WARNINGs and the Provider CR Status condition reads `TLSConfigured=False, Reason=ExplicitlyDisabled` — visible to compliance auditors.

## Implementation Plan — 4 sequential PRs

### PR-1: Implement `Resolver.buildTLSConfig` + wire `ProviderController` + loud-failure Condition (v0.3.7)

**Status**: ✅ **Landed** in [#157](https://github.com/projectbeskar/virtrigaud/pull/157). The loud-failure Condition shipped as `TLSConfigured=False, Reason=TLSBlockMissing` on the Provider; the manager-side client gained a `PrebuiltConfig *tls.Config` field so the resolver hands over an in-memory config built from the Secret bytes (no scratch files).

**Goal**: the manager actually reads the existing CRD field, dials providers over TLS when configured, and surfaces the `TLSNotConfigured` Condition when the operator hasn't decided.

**Files touched**:
- `internal/runtime/remote/resolver.go` — remove the `if true { return nil, nil }` short-circuit; load cert/key/ca from the mounted Secret path; wire `certwatcher.CertWatcher`.
- `internal/transport/grpc/client.go` — accept a `*tls.Config` directly (or add a `GetCertificate` callback field) so the cert watcher refreshes per-dial.
- `internal/controller/provider_controller.go` — replace hardcoded `tlsEnabled := false` (line 509) with `provider.Spec.Runtime.Service.TLS.Enabled`; replace the `if false { ... }` block (line 704) with a real volume that mounts `provider.Spec.Runtime.Service.TLS.SecretRef.Name`; pass `TLS_CERT_PATH`, `TLS_KEY_PATH`, `TLS_CA_PATH` env vars; **own the `TLSNotConfigured` Condition message** for the nil-TLS case (decision #3).
- `cmd/manager/main.go` — wire the manager-side gRPC client cert. Flags `--provider-grpc-cert-path` / `--provider-grpc-cert-name` / `--provider-grpc-cert-key` (same shape as the existing webhook-cert flags from ADR-0002 PR-1).
- `examples/providers/` — add `provider-with-mtls.yaml` showing a Provider CR plus a `kubectl create secret tls` example (no cert-manager `Certificate` resource).

**Tests**: unit on `Resolver.buildTLSConfig` (enabled/disabled, missing-secret, missing-key); unit on `ProviderController` reconcile (nil-TLS → `Ready=False, Reason=TLSNotConfigured`; `tls.enabled=true` + secretRef → TLS volume present; `tls.enabled=false` → `TLSConfigured=False, Reason=ExplicitlyDisabled`); integration: Provider CR with `tls.enabled=true` and a valid Secret + mock provider with matching cert → manager dials successfully.

**Rollback**: revert; nil-TLS goes back to silent plaintext.

---

### PR-2: Wire `Auth.RequireTLS` + implement `validateTLSPeer` on all four in-tree providers (v0.3.7)

**Status**: ✅ **Landed** in [#158](https://github.com/projectbeskar/virtrigaud/pull/158). `validateTLSPeer` requires `VerifiedChains` (not raw `PeerCertificates`); returns `Unauthenticated` for a missing/unverified cert and `PermissionDenied` for a non-matching SAN. The four mains consume `sdk/provider/server.ResolveTLSAndAuth` (env-var contract per the as-shipped table above). The libvirt provider was migrated off raw `grpc.NewServer()` onto the SDK server; `provider_virsh.go` was untouched.

**Goal**: the provider gRPC server refuses unauthenticated callers when TLS is configured. Closes #148.

**Files touched**:
- `cmd/provider-vsphere/main.go`, `cmd/provider-proxmox/main.go`, `cmd/provider-mock/main.go` — set `config.TLS = &server.TLSConfig{...}` and `config.Middleware.Auth = &middleware.AuthConfig{RequireTLS: true, AllowedSANs: ...}` from env (`TLS_CERT_PATH`, `TLS_KEY_PATH`, `TLS_CA_PATH`, `TLS_REQUIRE_CLIENT_CERT`, `AUTH_ALLOWED_SANS` as comma-separated list).
- `cmd/provider-libvirt/main.go` — **replace the raw `grpc.NewServer()` at line 71 with `server.New(config)`**. Existing libvirt-specific health/HTTP setup migrates into the SDK's hooks. Largest single change in the plan.
- `sdk/provider/middleware/middleware.go:262-273` — **implement `validateTLSPeer`**. Read the peer cert chain from `peer.FromContext`, walk it for SAN matches against `config.AllowedSANs`. Empty `AllowedSANs` means "any valid client cert from the trusted CA is accepted" (decision #5).
- `internal/controller/provider_controller.go` — pass `TLS_CERT_PATH=/etc/virtrigaud/tls/tls.crt`, etc., plus `AUTH_ALLOWED_SANS` derived from the manager's client cert SAN.

**Tests**: unit on `validateTLSPeer` (no peer info, no TLS info, empty allow-list, matching/non-matching SAN); integration: unauthenticated direct-dial → `PermissionDenied`; libvirt-specific: SDK migration didn't break `LIBVIRT_*` env-var contract or HTTP health endpoints.

**Rollback**: revert; provider pods go back to accepting unauthenticated calls; NetworkPolicy + encrypted CNI compensating controls remain.

---

### PR-3: Implement `AutoReload` hot-reload + global escape-hatch flag + ship chart `tls.secretName` (v0.3.7)

**Status**: ✅ **Landed** in [#159](https://github.com/projectbeskar/virtrigaud/pull/159), with two deviations from this heading: (a) the `--insecure-no-tls-providers` global flag was **not** built — per-Provider `tls.enabled=false` is the only escape hatch (see as-shipped table); (b) #159 also fixed a latent PR-1↔PR-2 integration bug where a `tls.enabled=false` Provider crash-looped (controller now sets `VIRTRIGAUD_PROVIDER_INSECURE=true` on the plaintext path, reachable only after `evaluateTLSPosture` confirms an explicit opt-out — never for a nil block). `AutoReload` hot-reloads the leaf cert only; CA-bundle rotation needs a restart.

**Goal**: cert rotation is transparent. Operators have a documented, named global escape hatch. The Helm chart's TLS surface is finalized — operator-provided Secret only, **no cert-manager scaffolding** (decision #1).

**Files touched**:
- `sdk/provider/server/server.go` — implement `TLSConfig.AutoReload`. When `true` (default), wrap cert loading in `certwatcher.CertWatcher`; register watcher as a goroutine with the gRPC server's lifecycle.
- `cmd/manager/main.go` — add `--insecure-no-tls-providers` flag (default `false`). When `true`, the manager skips TLS dial for *all* Providers and logs a startup ERROR `manager started with --insecure-no-tls-providers; gRPC traffic to ALL providers is plaintext`. Global escape hatch separate from per-Provider `tls.enabled=false`.
- `charts/virtrigaud/values.yaml` — expose **only** an existing-Secret reference: `providerTLS.secretName` (default empty). No `tls.certManager.enabled`, no `tls.issuerRef`, no `Certificate` template. The chart is agnostic about how the Secret got there.
- `charts/virtrigaud/templates/manager-deployment.yaml` — wire the mount path / env vars to the operator-named Secret.

**Tests**: manual rotation smoke (`grpcurl` continues without pod restart); unit: startup ERROR fires when `--insecure-no-tls-providers=true`; chart-render test: empty `providerTLS.secretName` produces a deployment with no TLS volume (chart stays valid for the explicit-disabled path).

**Rollback**: revert; `AutoReload` becomes a no-op (v0.3.6 behaviour — rotation requires pod restart); chart loses the `providerTLS.secretName` value.

---

### PR-4: Docs PR — operator runbook for manual cert provisioning + ADR promotion + example updates (v0.3.7)

**Status**: 🔄 **Split.** The maintainer scoped PR-4 down (2026-05-27) to **ADR promotion only** (this document). The operator runbook, the `examples/providers/*.yaml` additions, and the website `mtls.md` flip are **deferred to the v0.3.7 release doc-sync** — applying them now would document an unreleased feature on a website that tracks released reality. The original full scope is preserved below as the spec for that future doc-sync.

**Goal**: operator-facing docs cover the manual-provisioning happy path end-to-end. Promote ADR-0003 from `fieldTesting/` to `docs/adr/`. Flip the website's `mtls.md` admonition from "Not wired" to "Wired in v0.3.7 — here's how to provision certs".

**Files touched**:
- `docs/operations/security.md` — new operator doc covering: (a) the openssl recipe to mint CA + manager client cert + N provider server certs (canonical recipe lives in this ADR, below); (b) the Secret YAML template to apply on the cluster; (c) the manual rotation procedure (re-mint, kubectl apply -f, verify via `kubectl get providers -o yaml | grep TLSConfigured`); (d) the escape-hatch matrix (per-Provider `tls.enabled=false` vs. global `--insecure-no-tls-providers`).
- `src/providers/security/mtls.md` (website) — replace the `!!! danger "Not wired"` admonition with a `!!! info "Wired in v0.3.7"` configuration guide pointing at `docs/operations/security.md`.
- `examples/providers/*.yaml` — update existing examples to include `runtime.service.tls.secretRef`. Add a companion `examples/providers/tls-secret-template.yaml` showing the operator-applied Secret shape.
- `docs/adr/0003-mtls-and-provider-grpc-auth.md` — promoted from `fieldTesting/`.

**Tests**: docs-only PR — the validation is "follow the runbook on a fresh kind cluster, end up with a green Provider CR". Manual smoke test as part of the PR sign-off.

**Rollback**: revert; docs go back to disclosing the gap.

---

## Manual cert provisioning recipe (canonical)

This is the minimum recipe the docs PR (PR-4) expands into the operator-facing runbook. Pointer-quality, not full tutorial — but enough that the staff-engineer can implement against it without ambiguity.

**1. Mint a self-signed CA (one-time per install):**

```bash
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -subj "/CN=virtrigaud-ca" -out ca.crt
```

**2. Mint the manager's client cert:**

```bash
openssl genrsa -out manager.key 4096
openssl req -new -key manager.key -subj "/CN=virtrigaud-manager" -out manager.csr
openssl x509 -req -in manager.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out manager.crt -days 365 -sha256 \
  -extfile <(printf "subjectAltName=DNS:virtrigaud-manager")
```

**3. Mint a provider server cert (repeat per Provider):**

```bash
PROVIDER=provider-vsphere-prod
openssl genrsa -out ${PROVIDER}.key 4096
openssl req -new -key ${PROVIDER}.key -subj "/CN=${PROVIDER}" -out ${PROVIDER}.csr
openssl x509 -req -in ${PROVIDER}.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out ${PROVIDER}.crt -days 365 -sha256 \
  -extfile <(printf "subjectAltName=DNS:${PROVIDER},DNS:${PROVIDER}.virtrigaud-system.svc")
```

**4. Apply Secrets — TLS Secret template (operator-applied, one per Provider):**

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: provider-vsphere-prod-tls
  namespace: virtrigaud-system
type: kubernetes.io/tls
stringData:
  tls.crt: |  # contents of provider-vsphere-prod.crt
    -----BEGIN CERTIFICATE-----
    ...
  tls.key: |  # contents of provider-vsphere-prod.key
    -----BEGIN PRIVATE KEY-----
    ...
data:
  ca.crt: |   # base64 of ca.crt — kubernetes.io/tls accepts the extra key
```

Manager-side Secret uses the same shape with `manager.crt`/`manager.key` as `tls.crt`/`tls.key`.

**5. Reference from Provider CR:**

```yaml
spec:
  runtime:
    service:
      tls:
        enabled: true
        secretRef:
          name: provider-vsphere-prod-tls
```

**Rotation procedure (manual):** re-run steps 2/3 with a fresh serial, `kubectl apply` the updated Secret, watch `kubectl get providers -o yaml | grep -A2 TLSConfigured` go green again. No pod restart needed — `certwatcher` (PR-3) picks up the new bytes.

## Out of scope

- **Helm-gated cert-manager `Certificate` template scaffolding** (maintainer decision 2026-05-27 — operators provision certs externally; v0.3.7 ships the consuming-side wiring only; the chart's TLS surface is an existing-Secret reference and nothing more).
- **#149 libvirt SSH `no_verify=1`** (provider→hypervisor layer, separate ADR if warranted).
- **External-secrets / Vault integration for TLS material** — ADR-0003 only cares about the *keys* the Secret contains; how the bytes arrive is the operator's choice.
- **SPIFFE / SPIRE identity** — could be a follow-up ADR if demand emerges.
- **Per-RPC authorization** (e.g., "this caller can call Describe but not Delete") — needs policy-engine integration, not justified by current threat model.
- **Streaming-RPC authentication** — `authStreamInterceptor` exists at `middleware.go:233-240`; shares `authenticateRequest` so it picks up `validateTLSPeer` automatically. Listed here only so PR-2 testing doesn't forget it.

## Consequences

### Positive

- **v0.3.7 honours the project's banking-deployability claim.** No more compensating-controls-only story for manager↔provider transport.
- **No CRD schema break.** `ProviderTLSSpec` shipped in v1beta1; only the controller's *interpretation* changes.
- **Cert rotation works without pod restarts** (PR-3 `AutoReload` + Kubernetes Secret-to-Pod sync).
- **Loud failure mode** — banking auditors verify TLS posture via `kubectl get providers` rather than packet capture.
- **Chart stays small.** No cert-manager dependency creep; operators with their own PKI pipelines are first-class.
- **Defence-in-depth preserved**: NetworkPolicy + encrypted CNI guidance stays in `network-policies.md`; mTLS is the new primary control.

### Negative — release-note callout required for v0.3.7

- **Existing v0.3.6 Provider CRs that don't have a `tls` block in their spec will fail to reconcile after upgrade until the operator either (a) sets `tls.enabled=true` with a provisioned Secret OR (b) explicitly sets `tls.enabled=false` to keep plaintext.** This is intentional, not a bug. **The v0.3.7 release notes must call this out loudly.** Mitigated by the per-Provider escape hatch and the global `--insecure-no-tls-providers` flag.
- **PR-2 unifies libvirt onto the SDK server** — behavioural change for libvirt operators (different process layout, different shutdown semantics). Risk of regression on the existing libvirt HTTP health endpoints — load-bearing integration test required in PR-2.
- **Manual cert provisioning is more friction than a one-line cert-manager value flip.** Conscious trade-off (decision #1): keeps the chart surface small and respects banking-deployment buyers who run their own PKI. Mitigated by the canonical openssl recipe shipped in the operator runbook (PR-4).
- **`validateTLSPeer` empty-SAN default trusts any cert from the configured CA.** Matches kube-apiserver client-cert auth and the single-administrative-domain VirtRigaud model, but would be incorrect for a multi-tenant cluster where multiple distinct managers share a CA. Documented limitation; operators in that posture populate `AllowedSANs`.

## Security implications summary

| Item | Status after v0.3.7 |
|---|---|
| Manager→Provider transport | **Encrypted via mTLS by default** (Option C). Per-Provider opt-out via `tls.enabled=false`; global opt-out via `--insecure-no-tls-providers`. |
| Provider→Manager authentication | **Client cert SAN allow-list** (single-CA trust domain, permissive empty list). Provider rejects unauthenticated callers when TLS is configured. |
| Cert rotation | **Hot-reload via `certwatcher`** on both ends. No pod restart required. |
| Cert provisioning | **Operator-owned.** Manual recipe documented; chart consumes an externally-provisioned Secret. |
| Credential leakage on the wire | Eliminated — credentials only travel inside mTLS-protected gRPC streams. |
| Direct-dial bypass of the manager | Blocked at the provider's gRPC server (`Auth.RequireTLS` interceptor). |
| Cert key material storage | Kubernetes Secret at `/etc/virtrigaud/tls/`. |
| Audit visibility | Provider CR `Status.Conditions` reports `TLSConfigured` (True / False / Reason). Manager logs WARNING for Providers with TLS disabled. |

## Roadmap-position note

| ADR | Decision |
|---|---|
| ADR-0001 | gRPC as the manager↔provider transport (settled). |
| ADR-0002 | One canonical manager build path, certwatcher in canonical (settled, shipped v0.3.6). |
| **ADR-0003** | **mTLS + provider gRPC auth on top of the canonical transport (this document, target v0.3.7).** |

ADR-0003 stands on ADR-0001 (transport is gRPC) and ADR-0002 (manager already has certwatcher wired). The PRs in this ADR do not require any architectural change beyond those — only the wiring that was deferred to "future ADR" in v0.3.6.

## References

- Issue [#147](https://github.com/projectbeskar/virtrigaud/issues/147) — wire mTLS through Resolver.buildTLSConfig
- Issue [#148](https://github.com/projectbeskar/virtrigaud/issues/148) — provider gRPC servers don't enable Auth.RequireTLS / BearerTokenAuth
- Website doc disclosing the gap: `projectbeskar/virtrigaud-website#10` → `src/providers/security/mtls.md`
- File: `internal/runtime/remote/resolver.go:142-161` — the short-circuit
- File: `internal/transport/grpc/client.go:94-108, 951-991` — the existing TLS plumbing
- File: `sdk/provider/server/server.go:72-88, 340-358` — server-side TLS support
- File: `sdk/provider/middleware/middleware.go:81-94, 222-273` — auth middleware (with the TODO stub)
- File: `api/infra.virtrigaud.io/v1beta1/provider_types.go:50-78` — `ProviderTLSSpec`
- File: `internal/controller/provider_controller.go:508-519, 583-590, 702-713` — hardcoded `tlsEnabled := false`
- File: `cmd/provider-libvirt/main.go:71` — raw `grpc.NewServer()`, bypasses SDK
- Companion ADRs: [ADR-0001](./0001-transport-grpc-and-capi-integration.md), [ADR-0002](./0002-build-path-consolidation.md)
