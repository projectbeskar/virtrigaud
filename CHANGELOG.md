# Changelog

All notable changes to VirtRigaud will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2026-05-29 18:40] - v0.3.7: Fix kubectl image arm64 build (hardcoded amd64 download)
**Author:** @wrkode (William Rizzo)

### Fixed
- `build/Dockerfile.kubectl`: the kubectl binary download URL hardcoded `linux/amd64`, so the new arm64 image (added in #165 PR-B) installed an amd64 binary and the `RUN kubectl version --client` verify failed with a syntax/exec error on the arm64 leg — failing the `v0.3.7-rc1` release run (`Build and Push Images (kubectl, arm64)`; all 4 real components built arm64 cleanly). Added `ARG TARGETARCH` (auto-populated by BuildKit per target platform) and switched both download URLs to `linux/${TARGETARCH}/kubectl`, matching the k8s release-URL convention.

### Why
The multi-arch release build (#165) added arm64 to all image components including the `kubectl` CRD-upgrade utility image, but that image downloads a prebuilt kubectl binary rather than cross-compiling Go — so it needed to be made arch-aware. Validated locally with `docker buildx build --platform linux/arm64`: the arm64 binary downloads and `kubectl version --client` reports `Client Version: v1.32.0`. Unblocks recutting `v0.3.7-rc2`.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only — release build infra
- [ ] Documentation only

## [2026-05-29 16:20] - v0.3.7: Ship arm64 release images via native per-arch build + manifest merge, with full attestation parity (PR-B of #165; closes #169)
**Author:** @wrkode (William Rizzo)

### Added
- **`linux/arm64` release images** — release-notes headline for v0.3.7. All five release components (`manager`, `provider-libvirt`, `provider-vsphere`, `provider-proxmox`, `kubectl`) now publish a **multi-arch (`linux/amd64,linux/arm64`)** manifest under the existing release tags, so arm64 clusters can run VirtRigaud in production. This is a new user-facing capability and must be called out in the v0.3.7 release notes + the provider image-arch matrix in docs.
- **SBOM-as-attestation on the signed index** — `.github/workflows/release.yml` (`generate-sbom` job): after Syft produces the SPDX SBOM, a keyless `cosign attest --yes --recursive --type spdxjson --predicate sbom-<component>.spdx.json <repo>@<index-digest>` attaches it to the **same multi-arch index digest that `merge-and-sign` signs** (and, via `--recursive`, to both per-arch children). The index digest is resolved with `docker buildx imagetools inspect …:<tag> --format '{{json .Manifest.Digest}}' | tr -d '"'`, identical to `merge-and-sign`. This restores the supply-chain parity the old single-arch release provided through the buildx `sbom: true` setting — which **cannot** be re-added because push-by-digest orphans an `unknown/unknown` manifest that breaks `imagetools create`. The job pins `permissions: id-token: write` (+ `packages: write`, `contents: read`) for keyless OIDC, the same trust model as the existing signing.
- **SLSA provenance attestation on the signed index** — `.github/workflows/release.yml` (new `collect-index-digests` fan-in job + new `provenance` job): restores the SLSA provenance the old single-arch release carried via buildx `provenance: mode=max`, which **cannot** be re-added to the build step (same `imagetools create` orphan-manifest constraint as the SBOM). Instead the **`slsa-framework/slsa-github-generator` container generator** (`generator_container_slsa3.yml@v2.1.0`, reusable workflow) generates a SLSA Build L3, Rekor-logged provenance attestation and pushes it to GHCR. It runs against the **byte-identical index digest that `merge-and-sign` already signed** (and `generate-sbom` already attested): `merge-and-sign` now records the signed digest to an `index-digest-<component>` artifact, the `collect-index-digests` fan-in job folds those into a `component → {image,digest}` JSON map output, and the matrix `provenance` job reads its digest from that map (matrix-job step outputs do not fan out per-value to a reusable-workflow `uses:` call, so the fan-in map is required). The generator workflow is **tag-pinned `@v2.1.0`, NOT SHA-pinned** — the one sanctioned exception to the repo's SHA-pin rule, because the generator self-verifies its source against the referenced tag to emit non-forgeable provenance (a SHA pin breaks that); an inline comment documents this so it is not "fixed" to a SHA. The caller job grants `id-token: write` + `packages: write` + `actions: read`; registry creds are passed keyless as `registry-username: ${{ github.actor }}` / `registry-password: ${{ secrets.GITHUB_TOKEN }}`. With this, v0.3.7 ships **full attestation parity** — recursive cosign **signing** + SBOM **attestation** + SLSA **provenance** — all over the same multi-arch index, matching/exceeding the pre-#165 single-arch release and **closing #169**. `create-release` now also depends on `provenance` so it is a release-blocker.

### Changed
- `.github/workflows/release.yml`: restructure the release image build into the proven two-stage native multi-arch buildx pattern already validated in `ci.yml` (PR-A, #166/#167). **Stage 1** (`build-and-push`) now fans out over `component × arch` (10 legs): `amd64` legs on `ubuntu-22.04`, `arm64` legs on a **native `ubuntu-24.04-arm` runner** (QEMU removed), each building a single native arch and pushing **by digest** (`outputs: type=image,name=…,push-by-digest=true,name-canonical=true,push=true` — the `name=` form fixed in #167), uploading per-`component`/`arch` digest artifacts. The load-bearing `build-args` (VERSION/GIT_SHA ldflags → `virtrigaud_build_info`) are preserved on every leg. **Stage 2** (`merge-and-sign`, per component) downloads the digests, runs `docker buildx imagetools create` to assemble the multi-arch manifest under the **unchanged release tag scheme** (`type=semver,{{version}}` + `{{major}}.{{minor}}` + `{{major}}` + `type=ref,event=tag`), inspects it, resolves the manifest **index digest**, and `cosign sign --yes --recursive`'s that index — signing the index **and both per-arch child images**. `security-scan` (Trivy), `generate-sbom` (Syft), and `create-release` now depend on `merge-and-sign` (the release tag only exists after the merge); they scan/SBOM the multi-arch manifest by tag exactly as before (default platform = amd64, preserving prior coverage).

### Why
`release.yml` previously built `linux/amd64` only, so VirtRigaud shipped no arm64 release images. PR-B adds arm64 using the same native-per-arch + manifest-merge pattern proven in PR-A — native arm64 avoids the QEMU-emulation timeout that cancelled the #164 `main` run. The supply-chain controls (cosign signing, SBOM, Trivy) are preserved, with signing now covering **both arches** via `--recursive`. The per-arch digest merge had to drop the two buildx attestations (`provenance: mode=max`, `sbom: true`) the old single-arch release carried, because they orphan as `unknown/unknown` manifests that break `imagetools create`; **both halves of that parity are now restored in this PR** — the SBOM as a keyless `cosign attest` on the signed index, and SLSA provenance via the `slsa-github-generator` container workflow against that same index digest (closing #169). The maintainer (2026-05-29) made provenance a **v0.3.7 release-blocker** — it must land before any `v0.3.7-rc1` tag — so v0.3.7 does not regress vs v0.3.6, which had buildx `provenance: mode=max`. Validation caveat: `release.yml` is **tag-gated**, so PR CI does not exercise it — this is verified by a **`v0.3.7-rc1` tag** before v0.3.7 final (per the rc→smoke→final discipline), where `cosign verify-attestation --type spdxjson <image>@<digest>` confirms the SBOM attestation and `slsa-verifier verify-image <image>@<index-digest> --source-uri github.com/projectbeskar/virtrigaud` (or `cosign verify-attestation --type slsaprovenance`) confirms the provenance — and the provenance subject digest must equal the signed index digest. `actionlint` on `release.yml` is clean (note: actionlint does not validate the reusable-workflow's input/secret contract — that was reasoned against the generator's `v2.1.0` `workflow_call` definition).

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only — release CI infrastructure; adds the new arm64 release-image capability, no Go/Dockerfile/toolchain change
- [ ] Documentation only

## [2026-05-29 15:05] - v0.3.7: Fix push-by-digest "tag is needed" in CI image build (fix-forward for #166)
**Author:** @wrkode (William Rizzo)

### Fixed
- `.github/workflows/ci.yml`: the per-arch `build-images` step used `outputs: type=image,push-by-digest=true,...` **without a `name=`**, so every leg (all 4 components × amd64+arm64) failed at buildx validation with `ERROR: tag is needed when pushing to registry` — the first post-merge `main` run after #166 failed all 8 legs in <20s. push-by-digest still needs the repository **name** so buildx knows where to push the digest (the tag is applied later in `merge-images`). Added `name=${{ env.REGISTRY }}/${{ env.IMAGE_NAME_PREFIX }}/${{ matrix.component }}` to the `outputs:`.

### Why
PR #166 (PR-A of #165) restructured dev-image builds to native per-arch runners + manifest merge, but omitted the `name=` token required by push-by-digest. `actionlint` can't catch it (it's a runtime buildx requirement). The fix was **validated locally end-to-end** against a `docker-container` buildx builder + a throwaway registry: the broken form reproduces the exact error; the fixed form builds + pushes both arches by digest and `imagetools create` assembles a valid multi-arch manifest. Unblocks the #165 PR-A validation on `main`.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only — CI build infra
- [ ] Documentation only

## [2026-05-29 14:10] - v0.3.7: CI dev images build arm64 on native runners (PR-A of #165)
**Author:** @wrkode (William Rizzo)

### Changed
- `.github/workflows/ci.yml`: restructure the `build-images` job into the canonical two-stage multi-arch buildx pattern. **Stage 1** (`build-images`) fans out over `component × arch` (8 legs): the `arm64` legs now run on a **native `ubuntu-24.04-arm` GitHub-hosted runner** instead of QEMU emulation, the `amd64` legs on `ubuntu-22.04`. QEMU setup is dropped. Each leg builds a single native arch and pushes **by digest** (`outputs: type=image,push-by-digest=true,name-canonical=true,push=true`), exporting the digest as a per-`component`/`arch` artifact. **Stage 2** (`merge-images`, `needs: build-images`, per `component`) downloads the digests and runs `docker buildx imagetools create` to assemble the multi-arch manifest under the unchanged dev tags, then `imagetools inspect` to confirm. The published tag scheme (`:main`, `:main-<sha>`, `:latest`-on-default-branch), the image-name pattern (`${REGISTRY}/${IMAGE_NAME_PREFIX}/<component>`), and the per-component Dockerfile paths (manager → `build/Dockerfile.manager`, providers → `cmd/<component>/Dockerfile`) are preserved byte-for-byte. The build stays gated to `push` on `main` only, so PR CI does not exercise it.

### Why
The post-merge `main` run for #164 ended `cancelled`: the emulated `arm64` leg of the libvirt image (CGO + libvirt headers) hit the `timeout-minutes: 30` ceiling (libvirt 30m20s → cancelled → whole run cancelled). Building each architecture on its own native runner eliminates QEMU emulation entirely — native arm64 builds in minutes rather than ~25m emulated — and removes the timeout class of failure. This is **PR-A of #165** and covers the CI dev-image workflow only; the release workflow (`release.yml`, which adds `linux/arm64` to shipped images) is **PR-B** and is untouched here. Validation note: because `build-images` runs only on push to `main`, this is verified by the **post-merge `main` run**, not by this PR's own CI (where it shows skipped).

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only — CI build infrastructure; no runtime, Go, Dockerfile, or shipped-artifact change
- [ ] Documentation only

## [2026-05-29 12:30] - v0.3.7: Fix `make test-e2e` to actually run the e2e suite (closes #146)
**Author:** @wrkode (William Rizzo)

### Fixed
- `Makefile`: the `test-e2e` target's `go test ./test/e2e/ -v -ginkgo.v` invocation now passes **`-tags=e2e`**. PR #133 added `//go:build e2e` constraints to `test/e2e/e2e_test.go` and `e2e_suite_test.go`, but the target was never updated to set the tag — so `go test` saw "build constraints exclude all Go files" and ran **zero** e2e tests. The failure was masked because the target's preamble exits early when no Kind cluster is present, so the broken `go test` line was rarely reached. Verified: `go test -tags=e2e -list '.*' ./test/e2e/` now discovers `TestE2E`; without the tag the package fails to build. The flag is additive (it only includes the `e2e`-tagged files; no untagged files are dropped).

### Why
A test target that silently runs nothing gives false confidence — the e2e suite has been effectively dark since PR #133. Restoring the tag makes `make test-e2e` exercise the Ginkgo e2e suite as intended on a Kind cluster.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only — build/test tooling; no runtime or shipped-artifact change
- [ ] Documentation only

## [2026-05-29 06:49] - v0.3.7: Remediate reachable CVEs + add blocking govulncheck CI gate (closes #151)
**Author:** @wrkode (William Rizzo)

### Security
- `go.mod`: bump `golang.org/x/net` v0.52.0 → **v0.55.0**. Clears all reachable `x/net` vulnerabilities (GO-2024-3218 / CVE-2024-45338 HTTP/2 HPACK exhaustion; GO-2025-3563 / CVE-2025-22872 `idna` tokenizer OOB; GO-2024-3112 GOAWAY flood) reachable via `internal/providers/proxmox/pveapi/client.go` (`http.Client.Do` → HTTP/2 transport + `idna.ToASCII`). Transitive co-bumps from `go mod tidy`: `x/sys` v0.42.0 → v0.45.0, `x/term` v0.41.0 → v0.43.0, `x/text` v0.35.0 → v0.37.0, `x/tools` v0.42.0 → v0.44.0.
- `go.mod`, `sdk/go.mod`, `proto/go.mod`: raise `go` directive **1.26.0 → 1.26.3** across all three modules. `govulncheck` keys stdlib analysis off the module's `go` directive, not the running toolchain — modules declaring `go 1.26.0` continued to surface 14 stdlib CVEs (fixed in go1.26.1–1.26.3: `os`, `net/url`, `crypto/x509`, `net/mail`, `net/http`, etc.) even when built with a 1.26.3 toolchain. Container release images already build with `golang:1.26.3`; this aligns the directive with the actual build floor and stops source-build consumers on 1.26.0–1.26.2 from being exposed.
- `.github/workflows/ci.yml`: add **`govulncheck` job** — runs `golang/govulncheck-action@b625fbe` (v1.0.4, SHA-pinned) on both the root module and `sdk/` module on every PR and push to `main`. Wired into the `ci` summary job's `needs` list so a reachable vulnerability finding **blocks merge**. Uses `go-version-file: go.mod` / `sdk/go.mod` so the scanner's stdlib analysis always matches the declared build floor.

### Changed
- `Makefile`: update `GOLANGCI_LINT_VERSION` v1.64.8 → **v2.12.2** and the install path from `github.com/golangci/golangci-lint/cmd/golangci-lint` to `github.com/golangci/golangci-lint/v2/cmd/golangci-lint`. The v1 binary was built with go1.24.6 and refused to process modules declaring `go 1.26.3` (hard error: "the Go language version used to build golangci-lint is lower than the targeted Go version"). v2.12.2 is the version CI's `golangci/golangci-lint-action@v9` already uses, so local and CI lint are now in sync.

### Why
`govulncheck ./...` on `main` prior to this PR produced 17 reachable findings split across two root causes: (1) three `x/net` CVEs reachable through the Proxmox PVE HTTP client; and (2) fourteen stdlib CVEs surfaced because the `go` directive was pinned to 1.26.0 while the fixes shipped in 1.26.1–1.26.3. The maintainer chose "remediate first, then gate" — adding a failing govulncheck CI job without first clearing the findings would have immediately broken CI. The new gate enforces zero-reachable-vulnerability hygiene going forward, closing a supply-chain gap that Trivy+gosec alone did not cover (those tools scan containers/source patterns; govulncheck traces the actual call graph).

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

> **Source-build floor raised to Go 1.26.3.** Consumers who build VirtRigaud from source (not from the published container images) and are running go1.26.0–go1.26.2 must upgrade their toolchain. Binary/image consumers are unaffected — release images already use `golang:1.26.3`.

---

## [2026-05-29 05:10] - v0.3.7: Tighten manager ServiceAccount RBAC to least-privilege (closes #152)
**Author:** @wrkode (William Rizzo)

### Security
- `charts/virtrigaud/templates/manager-rbac.yaml`: **narrowed the manager ClusterRole/Role** (the RBAC that actually ships and binds to the manager SA, since this chart is hand-maintained — not generated from `config/rbac/`). Both the cluster-scoped and namespace-scoped branches were tightened identically: **`secrets` dropped from full CRUD (`create;delete;get;list;patch;update;watch`) to `get;list;watch`** — the manager only reads credential/cloud-init/TLS Secrets (`internal/controller/cloudinit.go`, `internal/runtime/remote/resolver.go`), it never writes them; **`configmaps` removed entirely** (no controller references ConfigMaps); the **phantom CRDs `vmclones`, `vmsets`, `vmplacementpolicies` (+/status) removed** (CRDs exist but no controller reconciles them); **`metrics.k8s.io` `pods`/`nodes` removed** (unused); **`deployments/status` removed** (the manager reads Deployment status off the object but never writes the status subresource); **`events` narrowed to `create;patch`**; and the CRD mega-rule split so **`vmimages`/`vmnetworkattachments` are read-only (`get;list;watch`)** and `vmclasses` keeps `create` without `delete`.
- `internal/controller/vmclass_controller.go`, `internal/controller/vmimage_controller.go`, `internal/controller/vmnetworkattachment_controller.go`: fixed the `+kubebuilder:rbac` markers, which named a **doubled, non-existent apiGroup `infra.virtrigaud.io.infra.virtrigaud.io`** and granted `create;update;patch;delete` + status + finalizers on it. These three reconcilers are watch-only no-op stubs (informer cache via `For()` only), so the markers were corrected to `groups=infra.virtrigaud.io` and narrowed to `get;list;watch` — the minimum the controller-runtime cache requires. This eliminated an entire phantom rule block from the generated `config/rbac/role.yaml`.
- `internal/controller/vmadoption_controller.go`: narrowed the `vmimages` marker from `get;list;watch;create;update;patch` to `get;list;watch` — the adoption controller references existing disks rather than minting VMImages (it does still create/update VirtualMachine and VMClass, which retain their write verbs).

### Changed
- `config/rbac/role.yaml`: regenerated via `make manifests` (controller-gen). Net effect of the marker fixes: the phantom `infra.virtrigaud.io.infra.virtrigaud.io` group (3 rules) is gone and `vmimages`/`vmnetworkattachments` collapse into a single read-only rule.

### Why
#152 is a MEDIUM-severity finding from the v0.3.6 security audit: the manager SA held broader Secret access (and other excess grants) than minimal-functional requires, so a compromised manager could create/mutate Secrets cluster-wide as a data-exfiltration or credential-tampering vehicle. Least-privilege RBAC shrinks that blast radius, which matters for the regulated-banking deployment posture. The marker fixes also remove dead grants (a doubled apiGroup) that no Kubernetes API server would ever evaluate.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

> **Requires cluster rollout** (`helm upgrade`) to apply the tightened ClusterRole/Role. The change is purely a verb/resource reduction matched against verified manager code paths; no controller logic, CRD, or provider RBAC was touched. Operators who extended the manager with custom controllers can re-add grants via `rbac.additionalRules` in values. **Residual risk:** envtest does not enforce RBAC, so unit tests cannot validate this — the real validation is the e2e suite running under the actual ServiceAccount. Recommend an e2e run before/after merge.

## [2026-05-29 04:40] - v0.3.7: Libvirt SSH host-key verification on by default (closes #149)
**Author:** @wrkode (William Rizzo)

### Security
- `internal/providers/libvirt/sshhostkey.go`: **new centralized host-key policy** — the single source of truth for the libvirt provider's SSH/scp host-key verification. `resolveHostKeyPolicy()` reads the new escape-hatch env var `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION` once (honoured only for the literal word `"true"`, case-insensitive/trimmed — mirrors ADR-0003's `isInsecureOptedIn`); `sshHostKeyOptions()`/`sshConfigStanza()`/`applyURIHostKeyOptions()` render the `-o StrictHostKeyChecking=…`/`UserKnownHostsFile=…` flags and `~/.ssh/config`/URI options; `verifyKnownHostsPresent()` is the loud hard-fail gate; `logVerificationMode()` emits the structured `slog` audit line. Trust material is read from `known_hosts` in the existing credentials Secret mount (`/etc/virtrigaud/credentials/known_hosts`) — zero controller/CRD change.
- `internal/providers/libvirt/virsh.go`: closed three of the five bypass paths — removed unconditional `no_verify=1` on the `qemu+ssh://` URI (now gated by the policy), and replaced `StrictHostKeyChecking=accept-new` + ephemeral `UserKnownHostsFile=/tmp/known_hosts` on both the `sshpass`-direct (`!`) and the standard virsh-over-ssh argv builders with the centralized policy options. `createSSHConfig` now writes a verifying `~/.ssh/config` instead of `accept-new`. `setupConnection` resolves the policy once into `VirshProvider.hostKey`, emits the one-line verification-mode audit log, and hard-fails (no TOFU) when verification is on but no usable `known_hosts` is present.
- `internal/providers/libvirt/server.go`: closed the remaining two bypass paths — the `scp` disk-image copy (`copyDiskToRemote`, both `sshpass`-password and key-based fallback) now consumes the same centralized host-key options, re-emits the verification-mode audit line, and re-runs the hard-fail gate before transfer.

### Fixed
- Libvirt SSH/scp no longer disables host-key verification on any code path. Pre-#149 every path used `no_verify=1` / `accept-new` against an emptyDir-backed `/tmp/known_hosts` that re-TOFU'd on every pod restart — strictly worse than classic TOFU and the exact MITM window #149 calls out. Verification is now on by default with a single, named, audit-visible escape hatch.

### Added
- `internal/providers/libvirt/sshhostkey_test.go`: table-driven coverage of the env-var parsing (`unset`/`""`/`false`/`1`/`yes`/`true`/`TRUE`/`  true  `), the secure and insecure option/config/URI output across all three transports (URI/key, password, scp), the actionable hard-fail error (asserts it names the host, the path, the `ssh-keyscan` recipe, and the env var), and the WARN/INFO `slog` audit lines via a captured handler.
- `docs/adr/0004-libvirt-ssh-host-key-verification.md`: promoted ADR-0004 from the gitignored `fieldTesting/` draft into the tracked ADR directory; flipped Status to **Accepted (2026-05-27)**, converted Open Questions into a "Decisions resolved" table, and added an "Implementation status — as shipped" note (centralized helper, scp re-emit, `slog` audit, empty-file hard-fail, I1 interaction).
- `docs/adr/README.md`: added the ADR-0004 index row (Accepted, 2026-05-27).

### Why
#149 is the last HIGH-severity item from the v0.3.6 security audit: the libvirt provider (a virsh-CLI-over-SSH provider) connected to remote hypervisors with SSH host-key verification disabled on every path, exposing the manager→hypervisor channel to MITM (SSH credential leak + injected `virsh`/disk-image tampering) — below the project's regulated-banking compliance posture. This makes verification secure-by-default and coherent with ADR-0003's manager→provider mTLS, closing both transport-trust boundaries in v0.3.7.

### Impact
- [x] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

> **Breaking change for v0.3.7 release notes.** Existing v0.3.6 libvirt Providers that connect over SSH and relied on the implicit `no_verify=1` will **stop connecting** after upgrade until the operator either (a) adds a `known_hosts` key to the credentials Secret referenced by `credentialSecretRef` (seed it from a trusted bastion: `ssh-keyscan -H <host> >> known_hosts`), OR (b) sets `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true` in the Provider's `spec.runtime.env` (audit-flagged WARN on every connection). This is intentional and the same breaking-change callout class as ADR-0003's nil-TLS-block loud failure.

### Usage
```yaml
# Secure (default): seed the host key in the existing credentials Secret.
apiVersion: v1
kind: Secret
metadata:
  name: libvirt-credentials
  namespace: virtrigaud-system
stringData:
  ssh-privatekey: |
    -----BEGIN OPENSSH PRIVATE KEY-----
    ...
  known_hosts: |
    # output of: ssh-keyscan -H <libvirt-host> >> known_hosts
    |1|...= ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
---
# Escape hatch (lab/migration only — audit-flagged): per-Provider env var.
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
spec:
  runtime:
    env:
      - name: LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION
        value: "true"
```

---

## [2026-05-28 11:05] - v0.3.7 PR-4: Promote ADR-0003 (mTLS + provider gRPC auth) to docs/adr/
**Author:** @wrkode (William Rizzo)

### Added
- `docs/adr/0003-mtls-and-provider-grpc-auth.md`: promoted the mTLS + provider-gRPC-auth design record from the gitignored `fieldTesting/` draft into the tracked ADR directory (mirrors the ADR-0001/0002 promotion pattern). Added an **"Implementation status — as shipped on `main`"** section that is authoritative where it conflicts with the original per-PR plan, documenting the three deviations between plan and reality: (1) provider config shipped as the `VIRTRIGAUD_PROVIDER_ALLOWED_SANS` / `VIRTRIGAUD_PROVIDER_INSECURE` env-var contract + on-disk cert detection (not the planned `TLS_CERT_PATH`/`AUTH_ALLOWED_SANS`); (2) the `--insecure-no-tls-providers` global flag was NOT built — per-Provider `tls.enabled=false` is the only escape hatch; (3) the operator runbook + example YAMLs + website `mtls.md` flip are deferred to the v0.3.7 release doc-sync. Marked PR-1 (#157), PR-2 (#158), PR-3 (#159) as Landed inline and fixed the companion-ADR relative links.
- `docs/adr/README.md`: added the ADR-0003 index row (Accepted, 2026-05-27).

### Why
ADR-0003's implementation is code-complete on `main` (PRs #157/#158/#159). Promoting the design record out of the gitignored scratch area makes the mTLS/auth decision auditable and discoverable for operators and contributors — required by the regulated-deployment posture. Scoped to ADR-promotion-only per maintainer decision (2026-05-27): the website security pages document *released* reality and v0.3.7 is unreleased, so the operator-facing runbook + website updates wait for the v0.3.7 release doc-sync. PR-4 of 4 in the v0.3.7 security track (umbrella #156).

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

---

## [2026-05-28 09:42] - v0.3.7 PR-3: Provider cert hot-reload (certwatcher) + Helm TLS surface + tls.enabled=false crash-loop fix
**Author:** @wrkode (William Rizzo)

### Security
- `sdk/provider/server/server.go`: implemented the long-declared-but-unwired `TLSConfig.AutoReload`. When `AutoReload=true` the SDK server now wires `tls.Config.GetCertificate` to a `sigs.k8s.io/controller-runtime/pkg/certwatcher.CertWatcher` (the same primitive the canonical manager already uses from ADR-0002 PR-1) instead of a static `Certificates` slice, so a rotated leaf cert/key is picked up on the next TLS handshake without a pod restart. The watcher's `Start` loop runs under `Serve`'s context (cancelled on shutdown — a proper cancellation story, no detached goroutine). When `AutoReload=false` the v0.3.7 PR-2 static-load path (`tls.LoadX509KeyPair` → `Certificates`) is preserved unchanged. `buildServerTLSConfig`/`buildTLSCredentials` now return the watcher alongside the `*tls.Config` so the caller can run it.
- `sdk/provider/server/tlsconfig.go`: `ResolveTLSAndAuth` now sets `AutoReload: true` on the TLS-present branch, making hot-reload the default production posture for any provider that finds cert material on disk (ADR-0003 rotation section). Documented the v0.3.7 limitation honestly: only the LEAF cert/key hot-reload; the `ClientCAs` pool is loaded once and rotating the CA bundle still requires a provider restart (certwatcher has no CA-pool reload primitive).

### Fixed
- `internal/controller/provider_controller.go`: **fixed a latent crash-loop on the controller-managed provider path for `tls.enabled=false` Providers.** PR-1 mounts the TLS Secret only when `tls.enabled=true`; PR-2 made the provider hard-exit when it finds no cert files AND no explicit opt-out. The two together meant a controller-managed Provider with `tls.enabled=false` would have produced a pod with neither a TLS mount nor the opt-out env-var, so `ResolveTLSAndAuth` took its hard-error branch and the provider crash-looped on upgrade to v0.3.7. The controller now sets `VIRTRIGAUD_PROVIDER_INSECURE=true` on the provider container whenever `tls.enabled=false`, so the provider opts into audit-flagged plaintext instead of failing to start. Guarded behind `evaluateTLSPosture` (the nil-TLS loud-failure case short-circuits earlier) so the opt-out can never silently downgrade an undecided Provider. New package constant `envProviderInsecure` mirrors the SDK env-var name.

### Added
- `charts/virtrigaud/values.yaml`: new `providerTLS` block for the chart-templated (static) provider Deployments — `secretName` (externally-provisioned `kubernetes.io/tls` Secret with `tls.crt`/`tls.key`/`ca.crt`), `allowedSANs` (list, comma-joined into `VIRTRIGAUD_PROVIDER_ALLOWED_SANS`), and `insecure` (the plaintext escape hatch → `VIRTRIGAUD_PROVIDER_INSECURE`). **No cert-manager `Certificate`/issuer scaffolding** — operators provision the Secret themselves (ADR-0003 maintainer decision #1, 2026-05-27). This block governs only the chart-templated providers; controller-managed providers read their posture from the Provider CR's `spec.runtime.service.tls`.
- `charts/virtrigaud/templates/_helpers.tpl`: three helpers — `virtrigaud.providerTLSEnv` (renders `VIRTRIGAUD_PROVIDER_ALLOWED_SANS` when a Secret is set, or `VIRTRIGAUD_PROVIDER_INSECURE=true` when `insecure=true` with no Secret, or nothing on the secure default so the provider hard-exits), `virtrigaud.providerTLSVolumeMount` (read-only mount at `/etc/virtrigaud/tls`), and `virtrigaud.providerTLSVolume`.
- `charts/virtrigaud/templates/provider-{vsphere,proxmox,libvirt}-deployment.yaml`: wired the three helpers in, including correct merge with each template's pre-existing `env:` shape (proxmox merges with `providers.proxmox.env`; vsphere/libvirt append to their own env).
- `sdk/provider/server/autoreload_test.go`: unit tests proving `AutoReload=true` installs the `GetCertificate` callback (and leaves `Certificates` empty), that the watcher picks up a rotated cert on disk (via `ReadCertificate`, avoiding flaky fsnotify timing), and that `AutoReload=false` keeps the static `Certificates` path with no watcher and nil `GetCertificate`.
- `sdk/provider/server/buildtls_test.go`: extended the existing PR-2 tests for the new `(config, watcher, err)` signature and assert `watcher == nil` on the static path.
- `internal/controller/provider_controller_tls_test.go`: extended the PR-1 tests to assert the `tls.enabled=false` pod carries `VIRTRIGAUD_PROVIDER_INSECURE=true` and the `tls.enabled=true` pod does NOT.
- `sdk/go.mod`, `sdk/go.sum`: added `sigs.k8s.io/controller-runtime` as a direct dependency of the SDK module (for `certwatcher`).

### Why
The SDK `TLSConfig.AutoReload` field shipped declared-but-unconsumed; this PR makes it real so cert rotation is transparent (ADR-0003 rotation section). The Helm chart needed a TLS surface for operators who deploy providers statically rather than via Provider CRs. Most importantly, the end-to-end verification of the controller-managed path surfaced a real integration bug between PR-1 (mount only when enabled) and PR-2 (hard-exit when no certs) that would have crash-looped `tls.enabled=false` Providers on upgrade — fixed here. PR-3 of 4 in the v0.3.7 security track; see `fieldTesting/ADR-0003-mtls-and-provider-grpc-auth.md`.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout — provider pods gain hot-reload behaviour; the `tls.enabled=false` crash-loop fix changes the rendered provider Deployment (adds `VIRTRIGAUD_PROVIDER_INSECURE=true` on that path).
- [x] Config change only — the Helm `providerTLS` block is new and opt-in (defaults to secure-by-default: no Secret, `insecure=false`).
- [ ] Documentation only

---

## [2026-05-28 05:23] - v0.3.7 PR-2: Enforce provider-side mTLS auth + migrate libvirt onto the SDK server
**Author:** @wrkode (William Rizzo)

### Security
- `sdk/provider/middleware/middleware.go`: implemented `validateTLSPeer` (previously a TODO stub that accepted any TLS caller). It extracts the verified client cert chain from the gRPC `peer.Peer` → `credentials.TLSInfo` and enforces the ADR-0003 SAN allow-list. No peer / no TLS / no verified chain → `codes.Unauthenticated`. Empty `AllowedSANs` → permissive (any cert the configured CA signed is accepted, matching kube-apiserver client-cert behaviour). Non-empty `AllowedSANs` → leaf cert must match by DNS SAN, URI SAN, or CN (CN as last-resort fallback); mismatch → `codes.PermissionDenied` with a structured log line naming the presented identity vs. the allow-list (SAN strings only, never the full cert).
- `sdk/provider/middleware/middleware.go`: `authenticateRequest` now passes `validateTLSPeer`'s gRPC status error through unchanged instead of re-wrapping every TLS failure as `PermissionDenied`, so callers can distinguish missing-cert (Unauthenticated) from rejected-cert (PermissionDenied).
- `sdk/provider/server/server.go`: completed the `RequireClientCert` path in `buildTLSCredentials` — the CA bundle at `CAFile` is now loaded into `tls.Config.ClientCAs` (was a `// TODO: Load CA cert` leaving mTLS verification non-functional) and `ClientAuth=RequireAndVerifyClientCert`. Pinned `MinVersion=tls.VersionTLS13` (ADR-0003 floor, matches the manager-side dialer). `#nosec G304` justifications added for the operator-supplied cert/CA file reads.
- `cmd/provider-vsphere/main.go`, `cmd/provider-proxmox/main.go`, `cmd/provider-mock/main.go`: wired `config.TLS` + `config.Middleware.Auth` from `server.ResolveTLSAndAuth()`, so these providers serve mTLS and run the `validateTLSPeer` interceptor when TLS material is mounted. Plaintext-with-loud-WARN when the operator explicitly opts out; hard-fail-to-start when material is missing and no opt-out is set.
- `cmd/provider-libvirt/main.go`: migrated off raw `grpc.NewServer()` onto the SDK `server.New(config)` so the libvirt provider inherits the same TLS + Auth interceptor chain (it previously bypassed auth entirely). Framework swap only — gRPC health protocol, HTTP `/healthz` + `/readyz`, and SIGINT/SIGTERM graceful shutdown are preserved; the SDK additionally applies the keepalive tuning the raw server lacked.

### Added
- `sdk/provider/server/tlsconfig.go`: new `ResolveTLSAndAuth` helper translating the ADR-0003 PR-2 contract (canonical `/etc/virtrigaud/tls` mount + `VIRTRIGAUD_PROVIDER_ALLOWED_SANS` / `VIRTRIGAUD_PROVIDER_INSECURE` env-vars) into a `TLSConfig`+`AuthConfig` pair. Three outcomes: files-present → mTLS-mandatory; files-absent + `VIRTRIGAUD_PROVIDER_INSECURE=true` → plaintext sentinel `ErrInsecureModeOptedIn`; files-absent + no opt-in → hard error. Mount-path constants (`ProviderTLSMountPath` etc.) cross-reference the PR-1 controller-side constants.
- `sdk/provider/server/server.go`: extracted `buildServerTLSConfig` (returns the assertable `*tls.Config`) out of `buildTLSCredentials`, and added a `GetServiceInfo()` pass-through for registration assertions.
- `sdk/provider/middleware/middleware_test.go`: unit tests for `validateTLSPeer` — no-peer / plaintext / no-verified-chain → Unauthenticated; empty allow-list → accept; DNS/URI/CN match → accept; allow-list miss → PermissionDenied; plus propagation tests through `authenticateRequest`.
- `sdk/provider/server/tlsconfig_test.go`: unit tests for `parseAllowedSANs`, `isInsecureOptedIn`, and the `ResolveTLSAndAuth` branch table.
- `sdk/provider/server/buildtls_test.go`: unit tests asserting the provider startup `*tls.Config` carries `MinVersion=TLS13` + `RequireAndVerifyClientCert` + a populated `ClientCAs` when certs are present, plus the no-client-cert and missing-CA/missing-cert error paths.
- `cmd/provider-libvirt/main_test.go`: tests proving the libvirt `Server` still satisfies `providerv1.ProviderServer` and that the SDK-based server registers the `provider.v1.Provider` service the raw server did.

### Why
PR-1 (#157) encrypted the manager→provider channel but the provider gRPC servers still accepted any caller on the pod network and the libvirt provider bypassed the SDK auth chain entirely. PR-2 closes that gap: providers now refuse unauthenticated callers when TLS is configured (closes #148, finishes #147), and libvirt is unified onto the SDK so it can never silently regress to no-auth. Empty-SAN permissive default is the documented single-CA trust model (ADR-0003 decision #5). PR-2 of 4 in the v0.3.7 security track; see `fieldTesting/ADR-0003-mtls-and-provider-grpc-auth.md`.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout — provider pods gain new TLS-serving + auth-enforcing behaviour. With TLS material mounted (PR-1 path), providers now require a verified client cert. The explicit escape hatch is `VIRTRIGAUD_PROVIDER_INSECURE=true` (via `spec.runtime.env`) with no TLS material, which starts plaintext with a loud audit-flagged WARN.
- [ ] Config change only
- [ ] Documentation only

---

## [2026-05-27 15:11] - v0.3.7 PR-1: Wire mTLS through Resolver.buildTLSConfig + fix provider_controller breakage sites
**Author:** @wrkode (William Rizzo)

### Security
- `internal/runtime/remote/resolver.go`: replaced the `(nil, nil)` short-circuit in `buildTLSConfig` with a real implementation that loads `tls.crt` / `tls.key` / `ca.crt` from the Secret referenced by `spec.runtime.service.tls.secretRef`. Builds a `*tls.Config` with `MinVersion=tls.VersionTLS13`, `RootCAs` from `ca.crt`, `Certificates` from `tls.crt`+`tls.key`, and `ServerName` anchored to the provider Service FQDN. Supports both `kubernetes.io/tls`-typed Secrets and plain `Opaque` Secrets carrying the same three keys. Honours the existing `tls.insecureSkipVerify` field as the dev-only escape hatch.
- `internal/controller/provider_controller.go`: replaced hardcoded `tlsEnabled := false` (line 509) with a real read of `spec.runtime.service.tls.enabled` via the new `providerTLSEnabled` helper. Replaced the `if false { ... }` TLS volume guard (line 704) with `if providerTLSEnabled(provider)`; volume now points at the operator-supplied `secretRef.Name` and mounts at the canonical `/etc/virtrigaud/tls`.
- `internal/controller/provider_controller.go`: added the loud-failure `TLSConfigured` Condition (`evaluateTLSPosture`). A Provider whose `spec.runtime.service.tls` is nil sets `TLSConfigured=False, Reason=TLSBlockMissing` with an operator-action message and refuses to provision a Deployment. The `ExplicitlyDisabled`, `SecretRefMissing`, and `Enabled` reasons cover the other branches. Banking auditors can grep `kubectl get providers -o yaml` for posture.

### Added
- `internal/runtime/remote/resolver.go`: exported sentinel errors `ErrTLSBlockMissing` and `ErrTLSSecretRefMissing` so callers can disambiguate the loud-failure branches via `errors.Is`.
- `internal/transport/grpc/client.go`: new `TLSConfig.PrebuiltConfig *tls.Config` field — when non-nil it is used verbatim for gRPC transport credentials, bypassing the existing on-disk file-loading path. Resolver.buildTLSConfig uses this to hand the gRPC client a fully assembled `*tls.Config` built in-memory from the Secret bytes.
- `internal/runtime/remote/resolver_test.go`: 11 new unit tests covering nil-TLS, nil-Service, enabled=false, missing-secretRef, missing-Secret, three missing-key cases, both `Opaque` and `kubernetes.io/tls` happy paths, and `InsecureSkipVerify` propagation.
- `internal/controller/provider_controller_tls_test.go`: 4 new unit tests covering the four Condition branches plus Deployment-volume/mount construction for `tls.enabled=true`.

### Fixed
- `internal/runtime/remote/resolver.go`: removed the stale breadcrumb comment claiming "TLS configuration removed in v1beta1" — the CRD schema for `ProviderTLSSpec` has been present and validated all along; the runtime simply never read it.
- `internal/runtime/remote/resolver.go`: replaced `//nolint:gosec` (golangci-lint-only directive) on the `InsecureSkipVerify` assignment with `// #nosec G402 -- <reason>` so the standalone `gosec` binary invoked by GitHub Advanced Security honours the suppression. Same justification (operator-controlled escape hatch per ADR-0003), now portable across both lint paths. Also emit a per-reconcile WARNING log line via `ctrl.LoggerFrom(ctx)` whenever a Provider has `tls.insecureSkipVerify=true`, naming the Provider + namespace so operators searching for "WARNING:" in the manager log catch the unsafe config; ADR-0003 mandated this signal and it was missing. Added a log-assertion test (`internal/runtime/remote/resolver_test.go`) using `funcr` log capture to keep the WARN line from regressing.

### Why
The v0.3.6 documentation audit (close commit `7b539ae`) confirmed manager↔provider gRPC traffic ships plaintext despite the website's `operations/security.md` claiming TLS support, and despite the CRD field being fully present. PR-1 wires the existing CRD into the existing transport-credential machinery so the documented banking-deployability posture is no longer a compensating-controls-only story. Tracks umbrella #156 and progresses #147; PR-2 (provider-side `Auth.RequireTLS`) is the next sequential PR. See `fieldTesting/ADR-0003-mtls-and-provider-grpc-auth.md` (Accepted 2026-05-27) for the full design.

### Impact
- [x] Breaking change — existing v0.3.6 Provider CRs without a `spec.runtime.service.tls` block will fail to reconcile after upgrade (loud-failure `TLSConfigured=False, Reason=TLSBlockMissing` Condition; no Deployment created). The operator must either (a) provision a Secret and set `tls.enabled=true` with `secretRef`, or (b) explicitly set `tls.enabled=false` to keep plaintext. Release-note callout required for v0.3.7.
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

---

## [0.3.6] - 2026-05-25

Headline observability + supply-chain release. v0.3.6-rc1 was deployed on `vr1.lab.k8`, smoke-tested, then tagged as v0.3.6 from the same commit.

### Added (operator-visible)
- **G6 CircuitBreaker on the provider gRPC RPC path** (#112). One CB per Provider CR allocated lazily from a shared `resilience.Registry`. The `virtrigaud_circuit_breaker_state{provider_type, provider}` gauge and `_failures_total` counter activate after this deploy and immediately surfaced the existing libvirt-SSH issue (#I1) on the v0.3.6-rc1 smoke — exactly the value the CB was supposed to provide.
- **G7.1 `virtrigaud_vm_operations_total`** (#126). Per-Provider VM-operation counter; deferred-record on Create/Delete/Power/Describe/Reconfigure gRPC client methods.
- **G7.2 `virtrigaud_ip_discovery_duration_seconds`** (#128). Per-Provider histogram measuring "kubectl apply → first IP visible in Status". Pure helper `recordIPDiscoveryIfFirstSeen` in `VirtualMachineReconciler`; idempotent across manager restarts via etcd-persisted vm.Status.IPs.
- **G7.3 `virtrigaud_provider_tasks_inflight`** (#130). Per-Provider async-task tracker. Mutex-guarded inflight map; trackTaskStart in 9 task-creating RPCs; trackTaskDone in 2 task-polling methods. Race-free under controller-runtime's 10 concurrent reconciles. Gauge seeded to 0 at boot so the family appears on /metrics from the first scrape.
- **H1 build-path consolidation** (#92 umbrella; PRs #115/#117/#119/#121). One manager entrypoint, one manager Dockerfile. Ported `--version` flag + `certwatcher` + `metrics/filters.WithAuthenticationAndAuthorization` to the canonical entrypoint; parametrised `build/Dockerfile.manager` with `BUILDER_IMAGE`/`BASE_IMAGE`/`GOPROXY`/CA-cert handling for corporate / banking deployments. Closed latent bug #113 (`make docker-build` was producing a manager binary missing metrics + VMSnapshot + VMMigration controllers).

### Changed
- **Go toolchain floor: 1.24.0 → 1.26.0** (#125). Pinned 5 builder Dockerfiles to `golang:1.26.3(-bookworm)`. Dropped stale `GO_VERSION: '1.23'` env var from CI workflows. **Source builders need Go 1.26+ installed locally**; binary consumers via released images unaffected.
- **GitHub Actions Node.js 20 backlog fully cleared** (#78/#80/#138/#139/#141/#142): `actions/upload-artifact` → v7, `actions/download-artifact` → v8, `actions/setup-go` → v6.4.0, `docker/login-action` → v4.2.0, `docker/metadata-action` → v6.1.0, `docker/setup-buildx-action` → v4.1.0. Clears the 2026-09-16 hard removal deadline (#134, ongoing).
- **Dependabot policy made explicit** (#135): added `allow: dependency-type: all` and `groups: ci-actions-non-major` for minor/patch batching while keeping major bumps individual. Top-of-file comments document the policy + Node 20 deadline + SHA-pin caveat.
- **e2e suite gated behind `//go:build e2e`** (#133): default `go test ./...` no longer fails on TestE2E (which needs a kind cluster). Run e2e explicitly with `go test -tags=e2e ./test/e2e/...`.

### Deprecated
- **`virtrigaud_queue_depth` gauge family** and `(*ReconcileMetrics).SetQueueDepth` helper (#132). Redundant with controller-runtime's already-emitted `workqueue_depth{name}` (present on /metrics since v0.3.0). Help string carries `[DEPRECATED v0.3.6 — use workqueue_depth{name} instead]`; helper has `Deprecated:` GoDoc. Removal scheduled for v0.4.0 or later. Migration recipe: see #131.

### Security
- **`go.opentelemetry.io/otel` + `otel/sdk` + `otel/trace` + `otlptracegrpc` v1.39.0/v1.37.0 → v1.43.0** (#144 / closes #143). Closes 3 HIGH-severity CVEs (CVE-2026-29181, CVE-2026-24051, CVE-2026-39883 — last two are PATH hijacking primitives). First attempt to cut v0.3.6-rc1 was blocked by the release workflow's Trivy scan on the manager image catching these; bump unblocks the release.

### Fixed
- **#113 (latent)**: `make docker-build` and `make run` now produce a complete manager binary (emits `virtrigaud_build_info`, registers VMSnapshot + VMMigration controllers). Fix-by-construction in H1 PR-3 (#119).

### Metric coverage after v0.3.6
**11 of 12 `virtrigaud_*` families wired in code; the 12th explicitly deprecated.** Live on `vr1.lab.k8` post-deploy: 9 of those 11 emit immediately (G6 CB pair + G7.3 inflight + the 6 from v0.3.5); G7.1 (`vm_operations_total`) and G7.2 (`ip_discovery_duration_seconds`) start emitting on the first VM RPC and the first no-IPs → has-IPs transition respectively.

### Process
- **Release process followed `rc1 → smoke → final`** for the third consecutive release (v0.3.3 took 4 RCs; v0.3.5 and v0.3.6 each took 1). Trivy on the manager image caught a real CVE before promotion (otel CVEs above); the v0.3.6-rc1 attempt from commit `d1e08d0` was deleted and re-cut from `d707c02` after the security fix landed.

### Author
**Author:** @wrkode (William Rizzo) — all PRs in this release.

---

## [0.3.3] - 2026-03-17

### Changed
- Changelog organization with versioned release headers

---

## [2026-05-25 16:55] - chore(test): gate e2e suite behind //go:build e2e (closes #133)
**Author:** @wrkode (William Rizzo)

### Audit finding
`test/e2e/TestE2E` fails on `go test ./...` because its `BeforeSuite` requires a running kind cluster + the manager image loaded (verified during G7.3 PR #130 pre-merge verify). The `make test` target works around this with an explicit `grep -v /test/e2e` exclude — fine for the make target, but raw `go test ./...` and IDE 'run all tests' buttons both hit the failure. Idiomatic Go solution: a build tag.

### Added
- `test/e2e/e2e_suite_test.go`, `test/e2e/e2e_test.go`: `//go:build e2e` on the first line. Excludes the suite from default `go test ./...` / `go vet ./...` runs.

### Run the suite explicitly
```bash
go test -tags=e2e ./test/e2e/...
```

### Why
- Default `go test ./...` runs cleanly across 13 packages instead of failing on TestE2E
- IDE 'run all tests' integration works
- Matches how other controller-runtime projects (cert-manager, capi, …) gate their e2e suites

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only — operator-visible behaviour unchanged; CI `make test` already excluded test/e2e

### Notes
- Supersedes the 5-day-old stale draft #71 (which had drifted ~60 files from main and would have hit huge rebase conflicts).
- CI `Test` job uses `make test` (which already excludes test/e2e), so no CI behaviour change.

---

## [2026-05-25 12:08] - chore(ci): make Dependabot policy explicit + group non-major actions bumps (#135 / closes #134 in part)
**Author:** @wrkode (William Rizzo)

### Audit finding
The May 22 Dependabot batch (#74–#82) cleared 5 of the 9 then-outdated GitHub Actions in our workflows. 4 remained on Node 20 with newer Node 24 majors available (`actions/setup-go`, `docker/login-action`, `docker/metadata-action`, `docker/setup-buildx-action`). Dependabot had not surfaced PRs for them, likely because of SHA-pinning + `# vX` version-comment hints making it conservative about major bumps. Hard deadline 2026-09-16.

### Changed
- `.github/dependabot.yml`: added top-of-file comment block (~25 lines) documenting the policy, the Node 20 deadline, the 4 outstanding actions, and the SHA-pinning caveat.
- `.github/dependabot.yml`: explicit `allow: dependency-type: all` block (functionally equivalent to omitting the block; loud-and-clear intent for future maintainers).
- `.github/dependabot.yml`: `groups: ci-actions-non-major` (`update-types: [minor, patch]`) so minor/patch bumps batch into one weekly PR per ecosystem while major bumps stay individual (matches the Tier A/B/C convention).

### Why
Make the major-bump-permitted policy loud; improve the per-week review experience by batching minor/patch noise; surface the Node 20 plan to anyone reading the config.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only
- [ ] Documentation only

### Notes
- Worked exactly as intended: the next Monday Dependabot run surfaced the 4 outstanding Node 20 actions plus 3 others. Tracking issue #134 documents the rollout. The 4 backlog clears merged the same day (#138/#139/#141/#142); #137 (actions/checkout 4→6) intentionally deferred to preserve the K4 mitigation pin from PR #104; #140 (codecov-action 5→6) deferred to v0.3.7 (not Node 20 backlog).

---

## [2026-05-25 15:28] - security: bump go.opentelemetry.io/otel + sdk to v1.43.0 (closes #143; unblocks v0.3.6-rc1)
**Author:** @wrkode (William Rizzo)

### Audit finding
First attempt to cut `v0.3.6-rc1` from main (commit `d1e08d0`, tag `f537176`, deleted) **failed the release workflow's Trivy scan on the manager image** with 3 HIGH-severity CVEs in the OpenTelemetry dependencies. The downstream `Create GitHub Release` + `Update Helm Repository` jobs were correctly skipped; the deleted rc1 tag never promoted.

| CVE | Package | Installed | Fixed in |
|---|---|---|---|
| CVE-2026-29181 | `go.opentelemetry.io/otel` | v1.39.0 | 1.41.0 |
| CVE-2026-24051 | `go.opentelemetry.io/otel/sdk` | v1.39.0 | 1.40.0 (Arbitrary Code Execution via PATH Hijacking) |
| CVE-2026-39883 | `go.opentelemetry.io/otel/sdk` | v1.39.0 | 1.43.0 (BSD kenv command not using absolute path enables PATH hijacking) |

### Security
- `go.mod`: bump `go.opentelemetry.io/otel` v1.39.0 → **v1.43.0**
- `go.mod`: bump `go.opentelemetry.io/otel/sdk` v1.39.0 → **v1.43.0**
- `go.mod`: bump `go.opentelemetry.io/otel/trace` v1.39.0 → **v1.43.0**
- `go.mod`: bump `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` v1.37.0 → **v1.43.0** (consistency across the otel module surface)
- Indirect bumps from `go mod tidy`: `go.opentelemetry.io/otel/metric` v1.39.0 → v1.43.0, `go.opentelemetry.io/proto/otlp` v1.7.0 → v1.10.0, plus minor refresh of `golang.org/x/{oauth2,sync,sys,term,text,tools}` and `google.golang.org/{grpc,protobuf,genproto/googleapis/*}` to satisfy otel v1.43.0's module graph.

### Why
1. **Hard release blocker.** Trivy on the release workflow's manager-image scan exits 1 on any HIGH or CRITICAL CVE — by design. v0.3.6-rc1 could not be promoted with the old otel pin.
2. **Banking-compliance posture** (per `CLAUDE.md`): PATH hijacking primitives are exactly the class of finding regulated environments will not accept. CVE-2026-24051 + CVE-2026-39883 both describe PATH hijacking vectors in the OpenTelemetry Go SDK.
3. **Smallest possible upgrade surface.** Only `internal/obs/tracing/tracing.go` imports otel; no code changes are required by the bump itself. `go mod tidy` is the entire diff after the version pin.

### Impact
- [ ] Breaking change (no public API or CRD surface changed; otel API surface is stable across the v1.39 → v1.43 range we span)
- [x] Requires cluster rollout (the manager image needs to be rebuilt with the new otel deps — that's what v0.3.6-rc1 will be)
- [ ] Config change only
- [ ] Documentation only

### Notes
- Verified locally pre-PR: `go vet ./...` clean; `go build ./...` clean; `make test` 12/12 packages with 0 FAIL; `docker build -f build/Dockerfile.manager` clean; `docker run virtrigaud-manager:otelfix --version` returns the expected banner.
- The v0.3.6-rc1 attempt that surfaced this is run [26406778809](https://github.com/projectbeskar/virtrigaud/actions/runs/26406778809). After this PR merges, `v0.3.6-rc1` will be re-cut from the new HEAD.
- The release workflow's Trivy scan is now the second post-Go-bump security-net catching real issues — this is the safety mechanism working as intended.

---

## [2026-05-25 08:06] - chore(obs): deprecate virtrigaud_queue_depth in favor of controller-runtime's workqueue_depth (G7.4 / closes #131 + G7 umbrella #123)
**Author:** @wrkode (William Rizzo)

### Audit finding
Fourth and final sub-issue of the G7 umbrella (#123). Smoke test on `vr1.lab.k8` post-G7.3 (PR #130) confirmed that controller-runtime's standard workqueue metric family is **already exposed on `/metrics`** and has been since v0.3.0:

```
workqueue_adds_total
workqueue_depth                              ← functionally identical to virtrigaud_queue_depth
workqueue_longest_running_processor_seconds
workqueue_queue_duration_seconds_*
workqueue_retries_total
workqueue_unfinished_work_seconds
workqueue_work_duration_seconds_*
```

`virtrigaud_queue_depth{kind}` is **redundant by construction** with `workqueue_depth{name}`. The originally-imagined wiring (custom `workqueue.MetricsProvider`) would have **silently replaced** the controller-runtime default and broken those 9 standard metrics for operators already scraping them — a breaking change disguised as a feature add.

### Deprecated
- `virtrigaud_queue_depth{kind}` gauge family — replaced by controller-runtime's `workqueue_depth{name=<controller-name>}`. The `Help` string emitted by Prometheus now begins with `[DEPRECATED v0.3.6 — use controller-runtime's workqueue_depth{name} instead. See CHANGELOG.]` so operators see the deprecation in their scrapes' `# HELP` lines.
- `(*ReconcileMetrics).SetQueueDepth(depth float64)` helper — annotated with a Go `Deprecated:` paragraph. `go doc` and IDE tooling will flag any new callers. Production code does not call this helper (and never has).
- Both will be removed in v0.4.0 or later. Out-of-tree code that imported the helper continues to compile + run; the call is just a no-op (gauge family still registered, just deprecated).

### Operator migration recipe
Replace your dashboards / alerts:
```
virtrigaud_queue_depth{kind="<X>"}    →    workqueue_depth{name="<controller-name>"}
```
Controller-name mapping (verified in `internal/controller/*.go`):

| Reconciler | controller-runtime `name` label |
|---|---|
| `VirtualMachineReconciler` | `virtualmachine` |
| `ProviderReconciler` | `provider` |
| `VMClassReconciler` | `vmclass` |
| `VMImageReconciler` | `vmimage` |
| `VMNetworkAttachmentReconciler` | `vmnetworkattachment` |
| `VMAdoptionReconciler` | `vmadoption` |
| `VMSnapshotReconciler` | `vmsnapshot` (default = lower-cased Kind; no `Named()` override) |
| `VMMigrationReconciler` | `vmmigration` (default = lower-cased Kind; no `Named()` override) |

You also get 8 sibling metrics for free with the migration: `workqueue_adds_total`, `_queue_duration_seconds`, `_work_duration_seconds`, `_retries_total`, `_unfinished_work_seconds`, `_longest_running_processor_seconds`, etc.

### Why
1. **Don't reinvent existing controller-runtime metrics.** The original G7.4 issue framed this as "wire `virtrigaud_queue_depth` via custom `workqueue.MetricsProvider`," but that approach would either silently break 9 standard metrics (Option A) or require reconstructing controller-runtime's internal MetricsProvider (Option D — fragile across versions). Option C (deprecate) is the honest call.
2. **Closes the G7 umbrella #123 cleanly.** All 4 deferred-from-G-track families are now addressed:
   - G7.1 / PR #126 — `vm_operations_total` wired ✅
   - G7.2 / PR #128 — `ip_discovery_duration_seconds` wired ✅
   - G7.3 / PR #130 — `provider_tasks_inflight` wired ✅
   - G7.4 / **this PR** — `queue_depth` deprecated in favor of `workqueue_depth`
3. **Zero operator-visible breakage.** Existing scrapes of `virtrigaud_queue_depth` continue to work (gauge family still registered; helper still callable; values still empty since no production caller). The deprecation banner is visible in `# HELP` lines so operators see it on their next reload.

### Impact
- [ ] Breaking change (deprecation only; metric family still registered, helper still callable)
- [x] Requires cluster rollout (only because the deprecation banner in `# HELP` shows up in the new image)
- [ ] Config change only
- [x] Documentation only (this is the bulk of the change)

### Notes
- After this lands, **11 of 12 `virtrigaud_*` families are wired** (G7.1 + G7.2 + G7.3 added 3, leaving only the 2 `virtrigaud_circuit_breaker_*` gaps that activate on `vr1.lab.k8` only after a v0.3.6-rc1 deploy — code already in main since G6 / PR #112). The 12th (`virtrigaud_queue_depth`) is explicitly deprecated in favor of the canonical controller-runtime equivalent.
- v0.3.6 candidate work is now **complete**: G6 #112 + C2-B #94 + H1 #92 + Go-bump #122 + G7 umbrella #123 all closed. Next checkpoint: **cut v0.3.6-rc1**, deploy to `vr1.lab.k8`, run smoke (CB lifecycle + 3 new G7 families on `/metrics`).
- Concurrent CHANGELOG hygiene change: previous entries' author-line GitHub handle updated from `@williamrizzo` to `@wrkode` to match the maintainer's actual handle (21 occurrences). Per-instruction: no separate commit ceremony.

---

## [2026-05-25 07:12] - feat(obs): wire virtrigaud_provider_tasks_inflight per-Provider async-task tracker (G7.3 / closes #129)
**Author:** @wrkode (William Rizzo)

### Audit finding
Third sub-PR of the G7 umbrella (#123). `virtrigaud_provider_tasks_inflight{provider_type, provider}` was registered in `internal/obs/metrics/metrics.go` with helper `(*TaskMetrics).SetInflightTasks(count)` — same dead-code-paradox shape that G7.1 / G7.2 / G6 each fixed: zero production callsites.

Pre-PR: operators dashboarding "how many tasks is the manager currently tracking per Provider?" got an empty family.

### Added
- `internal/transport/grpc/client.go`: `Client` struct gains three fields — `tasks *metrics.TaskMetrics`, `inflightTasksMu sync.Mutex`, and `inflightTasks map[string]struct{}`. The map-based set is the source of truth; the gauge value is set to `len(map)` after each change.
- `internal/transport/grpc/client.go`: `(*Client).trackTaskStart(taskID string)` — adds taskID to the set and pushes the new gauge value. Nil-safe on `c.tasks`. Idempotent on duplicate IDs (defends against pathological provider-server behaviour returning the same TaskRef twice).
- `internal/transport/grpc/client.go`: `(*Client).trackTaskDone(taskID string)` — removes taskID from the set and pushes the new gauge value. Idempotent on unknown IDs (handles two real-world cases: the reconciler crashes between observing Done=true and clearing `vm.Status.LastTaskRef`, and a new manager instance polls a TaskRef recorded by the previous instance — both must NOT push the gauge negative).
- `internal/transport/grpc/client.go`: 9 task-creating method call sites now invoke `c.trackTaskStart(resp.Task.Id)` after observing a non-nil Task field: **Create, Delete, Power, Reconfigure, SnapshotCreate, SnapshotDelete, SnapshotRevert, ExportDisk, ImportDisk**. Each call site annotated with `// G7.3 (#129)`.
- `internal/transport/grpc/client.go`: 2 task-polling methods now invoke `c.trackTaskDone(taskRef)` on terminal completion: **IsTaskComplete** (when `resp.Done` or `resp.Error != ""`) and **TaskStatus** (same condition).
- `internal/transport/grpc/client.go`: `NewClient` seeds the gauge to 0 via `taskMetrics.SetInflightTasks(0)` so the family appears on `/metrics` from boot — operators get a stable label set to dashboard against even before the first async task fires.
- `internal/transport/grpc/client_tasks_inflight_test.go` (new): 7 tests covering:
  - `TestTrackTask_NilTasksIsSafe` — pins nil-safety on both helpers
  - `TestTrackTask_StartAndDoneCycle` — pins the success path Start("a") → 1 → Start("b") → 2 → Done("a") → 1 → Done("b") → 0
  - `TestTrackTask_DoubleDoneIsIdempotent` — pins the double-poll contract (reconciler retry between observing Done and clearing status ref)
  - `TestTrackTask_UnknownDoneIsNoop` — pins the post-restart contract (new manager instance's set is empty; unknown Done must not push gauge negative)
  - `TestTrackTask_StartIdempotentOnSameID` — pins the duplicate-Start defensive path
  - `TestTrackTask_ConcurrentStartDoneIsRaceFree` — 200 goroutines (100 Start + 100 Done), verified clean under `go test -race`
  - `TestClient_TaskLifecycle_EndToEnd` — integration via bufconn server: Create-returning-Task → gauge=1, IsTaskComplete-returning-Done → gauge=0
- New `fakeTasksServer` in the test file: minimal `providerv1.ProviderServer` with `CreateTaskID` and `DoneOnPoll` toggles for the integration test.
- Local `gaugeSample` helper in the test file (mirrors the existing `counterSampleByLabels` pattern; kept package-local).

### Why
1. **Closes G7.3 of the G7 umbrella (#123).** Third of 4 deferred metric families to wire.
2. **Operator visibility into provider load.** "Is the libvirt provider drowning in concurrent migration tasks?" — pre-PR, no metric. Post-PR: `virtrigaud_provider_tasks_inflight{provider_type="libvirt"} > 5` is a sensible alert threshold.
3. **Correct semantic across manager restarts.** The gauge measures "tasks **this manager instance** is tracking," not "tasks the provider believes are in-flight." This is intentional and documented — the per-instance count self-corrects within seconds as tasks complete and new ones start. Documented in helper GoDoc.
4. **Race-free under concurrent reconciler load.** The mutex-guarded map handles the realistic case of multiple VirtualMachineReconciler workers (the controller-runtime workqueue defaults to 10 concurrent reconciles per controller, per `MaxConcurrentReconciles` in `SetupWithManager`) racing on the same Client. Verified with `go test -race`.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager binary gains new gauge emission)
- [ ] Config change only
- [ ] Documentation only

### Notes
- 9 task-creating sites is the full set verified in the audit on `main` post-#128 (Create / Delete / Power / Reconfigure / SnapshotCreate / SnapshotDelete / SnapshotRevert / ExportDisk / ImportDisk). All return `*providerv1.TaskResponse` (or have a Task field on a richer response struct). Any future RPC that returns a TaskRef must add a `trackTaskStart` call — would surface in code review since the pattern is uniform.
- Both `IsTaskComplete` and `TaskStatus` instrument `trackTaskDone`. They wrap the same underlying gRPC RPC; the per-Client mutex serialises updates so calling both for the same task only decrements the gauge once (because the second call sees the ID already removed from the set and no-ops).
- Pre-merge verification: `make fmt` clean; `go vet ./...` clean; `make test` 12/12 packages with 0 FAIL; new test file 7/7 sub-cases green; `go test -race ./internal/transport/grpc/...` clean; `make build` produces working `bin/manager` with `--version` exit 0.
- After this lands, **10 of 12 expected `virtrigaud_*` families are wired in code**. The remaining gap is G7.4 (`virtrigaud_queue_depth`) plus the two `virtrigaud_circuit_breaker_*` families that emit on `vr1.lab.k8` only after a v0.3.6-rc1 deploy (code already in main since G6 / PR #112).
- Next in G7: G7.4 (file sub-issue when starting) — wire `virtrigaud_queue_depth` (per-reconciler workqueue depth). Closes G7 umbrella #123.

---

## [2026-05-25 06:24] - feat(obs): wire virtrigaud_ip_discovery_duration_seconds in VirtualMachineReconciler (G7.2 / closes #127)
**Author:** @wrkode (William Rizzo)

### Audit finding
Second sub-PR of the G7 umbrella (#123). `virtrigaud_ip_discovery_duration_seconds{provider_type}` was registered in `internal/obs/metrics/metrics.go` with helper `metrics.RecordIPDiscovery(providerType, duration)` — same dead-code-paradox shape that G7.1 / G6 / etc. fixed: zero production callsites. The histogram has buckets 100ms-~51s which fits "VM CR creation → first IP visible" durations.

### Added
- `internal/controller/virtualmachine_controller.go`: new pure helper `recordIPDiscoveryIfFirstSeen(currentIPs, descIPs []string, creationTime metav1.Time, providerType string)`. Gates on three conditions (currentIPs empty, descIPs non-empty, creationTime non-zero) and records `metrics.RecordIPDiscovery(providerType, time.Since(creationTime.Time))` when all three hold. Pure function (no reconciler state) so it is unit-testable without standing up the envtest harness.
- `internal/controller/virtualmachine_ip_discovery_test.go` (new): 4 tests pinning all 4 gate paths:
  - `TestRecordIPDiscoveryIfFirstSeen_NoIPsToNoIPs` — common "still waiting for DHCP" case; must NOT record.
  - `TestRecordIPDiscoveryIfFirstSeen_FirstIPDiscovered` — load-bearing success path; records exactly 1 sample with correct `provider_type` label, sample-sum delta ≈ time-since-creationTime (4s-60s window absorbs CI scheduling jitter without flakiness).
  - `TestRecordIPDiscoveryIfFirstSeen_AlreadyHadIPs` — idempotency-after-restart contract; must NOT re-record because `vm.Status.IPs` persists in etcd and subsequent reconciles see currentIPs non-empty.
  - `TestRecordIPDiscoveryIfFirstSeen_ZeroCreationTimeIsSkipped` — defensive; must NOT record when CreationTimestamp is zero (would emit nonsensical durations like `time.Since(epoch)`).
- `internal/controller/virtualmachine_ip_discovery_test.go`: also introduces two local histogram-introspection helpers (`histogramSampleCount`, `histogramSampleSum`) that mirror the existing `counterSample` pattern but for `*dto.Histogram` samples. Kept package-local so the helpers don't leak into production code.

### Changed
- `internal/controller/virtualmachine_controller.go`: in `Reconcile`, call `recordIPDiscoveryIfFirstSeen(vm.Status.IPs, desc.IPs, vm.CreationTimestamp, string(provider.Spec.Type))` **immediately before** `vm.Status.IPs = desc.IPs`. The ordering is load-bearing — the gate inspects the **pre-update** value of `vm.Status.IPs`. Inline comment block points at #127 and explains the ordering.

### Why
1. **Closes G7.2 of the G7 umbrella (#123).** Second of 4 deferred metric families to wire.
2. **Operator-facing SLO answer.** "How long from `kubectl apply` to the VM having an IP?" was previously unanswerable from metrics; required scraping events and timestamps off `kubectl describe vm`. Post-PR: `histogram_quantile(0.95, sum(rate(virtrigaud_ip_discovery_duration_seconds_bucket{provider_type="libvirt"}[5m])) by (le))` gives the p95 IP-discovery latency for libvirt VMs over the last 5 minutes.
3. **Adoption case handled**: an adopted VM (CR created against an existing running VM) will have `CreationTimestamp` = adoption time and IPs already populated by Describe on the first reconcile. The gate fires once on first observation with a small positive duration (adoption → reconcile latency), which is the correct semantic — operators learn how quickly post-adoption their metrics surfaced the VM state. Documented in the helper's GoDoc.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager binary gains new metric emission)
- [ ] Config change only
- [ ] Documentation only

### Notes
- The helper is **package-private** intentionally: it has no callers outside the reconciler today. If a different reconciler ever needs the same semantics it can be promoted to a shared helper at that point.
- The histogram buckets (100ms-51.2s exponential) fit the expected operator-VM SLO range. Outliers above 51.2s land in the `+Inf` bucket — visible but not bucketed in detail. Acceptable for v0.3.6; can revisit if operators ask for finer resolution above 1 minute.
- This is now **9 of 12** `virtrigaud_*` families wired (with G6 + G7.1 + G7.2 stacked). Remaining gaps: `virtrigaud_provider_tasks_inflight` (G7.3), `virtrigaud_queue_depth` (G7.4), plus the two `virtrigaud_circuit_breaker_*` families that emit on `vr1.lab.k8` only after a v0.3.6-rc1 deploy (the CB code is in main already since G6 / PR #112).
- Pre-merge verification: `make fmt` clean; `go vet ./...` clean; `make test` 12/12 packages pass with 0 FAIL; new test file 4/4 sub-cases green; `make build` produces a working `bin/manager` with `--version` exit 0.
- Next in G7: G7.3 (file sub-issue when starting) — wire `virtrigaud_provider_tasks_inflight` (per-Provider async task tracker).

---

## [2026-05-25 05:35] - feat(obs): wire virtrigaud_vm_operations_total in gRPC client VM-operation methods (G7.1 / closes #124)
**Author:** @wrkode (William Rizzo)

### Audit finding
First implementation chunk of the G7 umbrella (#123). The `virtrigaud_vm_operations_total{operation,provider_type,provider,outcome}` metric family was registered in `internal/obs/metrics/metrics.go` and had a helper `(*VMOperationMetrics).RecordOperation(op, outcome)` — but **zero production callsites called the helper**. Same shape of dead-code-paradox bug that G6 (#111) closed for CircuitBreaker metrics.

The G4 metrics interceptor (PR #107) already records the lower-level `virtrigaud_provider_rpc_requests_total{provider_type, method, code}` — the per-gRPC-method counter. G7.1 adds the **higher-level** per-VM-operation counter, which is what operators dashboard for questions like "how often does Create fail on the vsphere-prod Provider?" without having to know the gRPC method shape.

### Added
- `internal/transport/grpc/client.go`: `Client.vmOps *metrics.VMOperationMetrics` field, initialised by `NewClient` from the new `providerName` parameter (paired with the existing `providerType`).
- `internal/transport/grpc/client.go`: `(*Client).recordVMOp(op string, retErr *error)` helper. Designed to be called via `defer c.recordVMOp(metrics.OpCreate, &retErr)` from each VM-operation method, with a named `retErr` return value. The pointer-to-error indirection is the load-bearing piece: it lets the deferred call evaluate the FINAL return value rather than the value at defer-time (always nil). Nil-safe on `c.vmOps` so tests that construct `&Client{...}` directly don't panic.
- `internal/transport/grpc/client_vm_operations_test.go` (new): 4 tests covering:
  - `TestRecordVMOp_NilVMOpsIsSafe` — pins the nil-safety contract on the helper.
  - `TestRecordVMOp_OutcomeDerivation` — pins success vs error outcome derivation, including the defensive nil-`*error` path.
  - `TestClient_VMOperations_RecordOnSuccess` — table-driven canary, all 5 VM-op methods × success path, asserts per-operation counter increments by exactly 1.
  - `TestClient_VMOperations_RecordOnError` — same table × error path, asserts the `{outcome="error"}` sample increments. Catches the regression class where outcome derivation inverts.
- `internal/transport/grpc/client_vm_operations_test.go`: new `fakeVMOpsServer` that implements all 5 VM-operation server handlers with a `fail` toggle. Single bufconn server can produce both metric outcomes without re-instantiating.

### Changed
- `internal/transport/grpc/client.go`: `NewClient` signature gains a `providerName string` parameter between `providerType` and `cb`. Empty string is permitted (label will be empty) but discouraged in production — `provider` is the second half of the `{provider_type, provider}` label pair that lets operators distinguish multiple Providers of the same type.
- `internal/transport/grpc/client.go`: 5 VM-operation methods refactored to use named return values + `defer c.recordVMOp(metrics.Op<X>, &retErr)`:
  - `Create` → `(result contracts.CreateResponse, retErr error)`
  - `Delete` → `(taskRef string, retErr error)`
  - `Power` → `(taskRef string, retErr error)`
  - `Describe` → `(result contracts.DescribeResponse, retErr error)`
  - `Reconfigure` → `(taskRef string, retErr error)`
  Same shape as G1's named-return reconcile-timer pattern (PR #101). All other method bodies are unchanged.
- `internal/runtime/remote/resolver.go`: passes `provider.Name` as the new `providerName` parameter to `NewClient`. Doc comment updated to call out which label `provider.Name` populates.
- `internal/transport/grpc/client_metrics_test.go`: `newTestClient` initialises the new `vmOps` field so deferred `recordVMOp` calls in production methods remain safe under this test path.
- `internal/transport/grpc/client_circuitbreaker_test.go`: same change to `newTestClientWithCB`; added `metrics` import.

### Why
1. **Closes the G7.1 part of the G7 umbrella (#123).** First of 4 deferred-from-G-track metric families to wire up.
2. **Operator-facing signal that's been missing.** "How often is Create failing on this Provider?" is a routine question for an operator on call. Today (pre-merge) the answer requires knowing the gRPC method names and joining `virtrigaud_provider_rpc_requests_total` on labels. After this PR the answer is a single PromQL query on `virtrigaud_vm_operations_total{operation="Create", outcome="error"}`.
3. **Sets the wiring pattern for the rest of G7.** The named-return + defer + nil-safe helper combo is the canonical pattern this codebase has converged on (G1 reconcile timer, G3 double-count fix, G6 circuit breaker). G7.2/G7.3/G7.4 should reuse it.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager binary gains new metric emission; new metric series appear on `/metrics` after rollout)
- [ ] Config change only
- [ ] Documentation only

### Notes
- `NewClient` signature change ripples to 3 call sites: `internal/runtime/remote/resolver.go` (production — passes `provider.Name`), and the two test helpers in `internal/transport/grpc/` (test — pass synthetic provider names). No other production callers exist.
- The `Validate` RPC and the snapshot/task/disk-related methods are intentionally NOT instrumented with `vm_operations_total` — they are not VM operations in the operator-facing sense. The G4 interceptor still records every RPC at the lower level via `virtrigaud_provider_rpc_requests_total`.
- Pre-merge verification: `make fmt` clean; `go vet ./...` clean; `make test` 12/12 packages pass with 0 FAIL; `make build` produces a working `bin/manager` with `--version` exit 0. The new test file adds 12 sub-test cases (4 tests × multiple operations).
- Next in G7: G7.2 (file sub-issue when starting) — wire `virtrigaud_ip_discovery_duration_seconds` in the VirtualMachineReconciler IP-discovery path.

---

## [2026-05-24 21:27] - chore: bump Go toolchain floor to 1.26.0 and pin Dockerfiles to golang:1.26.3 (closes #122)
**Author:** @wrkode (William Rizzo)

### Audit finding
Drift between the project's three Go layers:
- `go.mod` floor: `go 1.24.0`
- `sdk/go.mod` floor: `go 1.24.0`
- `proto/go.mod` floor: `go 1.23` (most stale)
- Dockerfiles: `golang:1.25(-bookworm)` (all 5 builder bases)
- CI: `GO_VERSION: '1.23'` env var in `.github/workflows/{ci,runtime-chart}.yml` — unused because every job uses `go-version-file: go.mod` which trumps the env var

This PR consolidates all three layers on the same target (Go 1.26) and clears the stale env var.

### Changed
- `go.mod`: `go 1.24.0` → `go 1.26.0`. Module-graph cleanup from `go mod tidy` reclassifies `prometheus/client_model` from indirect → direct (test code uses it directly; this was a long-standing miscategorization the new tidy detected).
- `sdk/go.mod`: `go 1.24.0` → `go 1.26.0`. Module-graph cleanup prunes `cel.dev/expr v0.24.0` and `rogpeppe/go-internal v1.13.1` (already superseded by newer transitive versions; not removed manually, `go mod tidy` did it).
- `proto/go.mod`: `go 1.23` → `go 1.26.0`. New `proto/go.sum` (14 lines) tracked; previously untracked because the module graph was simple enough that go.sum wasn't needed before the newer Go version.
- `build/Dockerfile.manager`: `ARG BUILDER_IMAGE=docker.io/golang:1.25` → `ARG BUILDER_IMAGE=docker.io/golang:1.26.3`.
- `cmd/provider-libvirt/Dockerfile`: `ARG BUILDER_IMAGE=golang:1.25-bookworm` → `ARG BUILDER_IMAGE=golang:1.26.3-bookworm`.
- `cmd/provider-vsphere/Dockerfile`: same change.
- `cmd/provider-mock/Dockerfile`: hardcoded `FROM golang:1.25-bookworm` → `FROM golang:1.26.3-bookworm` (this Dockerfile predates the BUILDER_IMAGE ARG pattern; left as-is for this PR).
- `cmd/provider-proxmox/Dockerfile`: `ARG BUILDER_IMAGE=golang:1.25` → `ARG BUILDER_IMAGE=golang:1.26.3`.
- `.github/workflows/ci.yml`: removed stale `GO_VERSION: '1.23'` env var (was unused — jobs use `go-version-file: go.mod`). Updated one straggler job (line ~535, the `Verify single-version CRDs` step in the CRD-verification workflow) from `go-version: ${{ env.GO_VERSION }}` to `go-version-file: go.mod` for consistency with the rest of the file.
- `.github/workflows/runtime-chart.yml`: removed the same stale `GO_VERSION: '1.23'` env var.

### Why
1. **Closes the drift** between the three Go layers. Going forward, the source of truth is `go.mod`'s `go` directive plus the explicit Dockerfile pins.
2. **Picks up 1.26 toolchain improvements** (runtime, stdlib, build tooling) without paying for them under release pressure.
3. **Clears the stale CI env var** that pinned a version we no longer support and that misled contributors reading the workflow.
4. **Prepares the tree for G7 work** (#123 umbrella) — wiring 4 more `virtrigaud_*` metric families lands next, and we don't want \"is this metric quirk because of an old Go version?\" being a confounder.

### Safety evidence (hands-on, all collected before this PR was filed — see #122 issue body)
- `go vet ./...` clean against bumped directive
- `go build ./...` clean against bumped directive
- `make test` 12/12 packages pass (the unrelated `test/e2e` failure requires a kind cluster and is excluded from `make test`)
- `go mod tidy` in all 3 modules — zero source changes required; only go.sum cleanup
- `docker.io/golang:1.26.3` AND `golang:1.26.3-bookworm` images pullable (manifest inspected)
- Full Docker build chain: `golang:1.26.3` builder → `go 1.26.0` directive → manager binary → `./manager --version` works (exit 0). Built locally as `virtrigaud-manager:gobump-pr` from commit `b7bf3f9`, banner returned `virtrigaud-manager v0.3.6-gobump-pr (b7bf3f937017a331ff99fad356cc565a549afc74)`.

### Impact
- [ ] Breaking change (no public API or CRD surface changed)
- [x] Requires cluster rollout — only for source-building consumers: anyone running `make build` or `make docker-build` (or any non-released artefact) needs **Go 1.26+** installed locally. Operators pulling the released images (`ghcr.io/projectbeskar/virtrigaud/manager:<tag>`) are unaffected because the release image embeds its own Go toolchain in the builder stage.
- [ ] Config change only
- [ ] Documentation only

### Notes
- Corporate forks that override `BUILDER_IMAGE` via `--build-arg` (enabled by PR #117 / H1 PR-2) can pin their own `golang:1.26.x` image from an internal mirror without patching the Dockerfile.
- This is a standalone PR per the v0.3.6 plan agreed with William: Go bump first → G7 PRs second → v0.3.6-rc1 cut. A bisect surface stays clean if any downstream regression emerges.
- Pre-existing `make lint` tooling drift (`.golangci.yml` v2 syntax vs `golangci-lint v1.64.8` Makefile pin) is unchanged — out of scope here; CI's runner-installed version passes.

---

## [2026-05-24 12:47] - chore: delete cmd/main.go + root Dockerfile (H1 PR-4 / closes #92 H1 umbrella + #120)
**Author:** @wrkode (William Rizzo)

### Audit finding
Final v0.3.6 chunk of the H1 build-path consolidation roadmap (`fieldTesting/ADR-0002-build-path-consolidation.md`). With PR-1 (#114 / PR #115), PR-2 (#116 / PR #117), and PR-3 (#118 / PR #119) all merged, the orphan local-dev path (`cmd/main.go` + root `Dockerfile`) is reachable from nowhere — no Makefile target, no CI workflow, no hack script, no release-engineering tooling, no fork-engineering tooling references either file. Delete them.

### Removed
- `cmd/main.go` (~303 LOC). The local-dev orphan manager entrypoint. Every feature it had that operators relied on was ported to the canonical `cmd/manager/main.go` in PR #115 (`--version` flag, certwatcher integration for webhook + metrics, `metrics/filters.WithAuthenticationAndAuthorization` RBAC filter). The two missing things that made it strictly inferior to the canonical (no `metrics.SetupMetrics`, no VMSnapshot controller, no VMMigration controller — tracked as the closed-by-PR-3 bug #113) are gone with it.
- `Dockerfile` (repo root, ~58 LOC). The local-dev orphan Dockerfile that PR-3 already routed every Makefile target away from. The CA-cert handling pattern it carried was reimplemented correctly in `build/Dockerfile.manager` by PR #117 (using `/usr/local/share/ca-certificates/` instead of the orphan's broken `/etc/ssl/certs/` target, with a dedicated `ca-certs/` subdirectory rather than the orphan's repo-root glob).

### Why
1. **The H1 umbrella #92 closes here.** The original 2026-05-22 instinct (`rm cmd/main.go Dockerfile`) was wrong because the orphan path had real features the canonical lacked. After 4 PRs of careful porting + redirect + verify, this PR is the safe execution of that original instinct.
2. **Stops the dual-path tax** that PR #112 (G6) had to pay (wiring the CircuitBreaker registry into both entrypoints). Future cross-cutting work touches one entrypoint only.
3. **Removes the next-contributor footgun.** "Which Dockerfile builds the released image?" no longer has two plausible answers — the 30 minutes of confusion during the v0.3.3-rc4 hotfix cannot recur.

### Impact
- [ ] Breaking change (no public API or CRD surface changed)
- [x] Requires cluster rollout — only for corporate **forks** that imported the deleted files. Fork-migration story: replace `-f Dockerfile` with `-f build/Dockerfile.manager`; replace `cmd/main.go` with `cmd/manager/main.go`. ADR-0002 has the full feature-parity table. Upstream operators see no change.
- [ ] Config change only
- [ ] Documentation only

### Notes
- **Verified deletion-safe before filing** (full grep matrix in #120's body): `.github/workflows/{ci,release}.yml`, `hack/{dev-deploy,test-release-locally}.sh`, and all 5 affected Makefile targets explicitly route the manager to `build/Dockerfile.manager` / `cmd/manager/main.go`. The remaining string-level references to `cmd/main.go` / `Dockerfile` in the tree after this PR are: historical CHANGELOG entries (do not edit past records), explanatory comments in `Makefile` + `cmd/manager/main.go` that point at H1 PR numbers (kept as project history), and `internal/scaffold/scaffold.go` which uses `"Dockerfile"` as the output filename of the scaffolding template (unrelated).
- Manual smoke pre-merge (after deletion):
  - `make build` → `./bin/manager --version` → `virtrigaud-manager v0.3.5-7-gd29fffd-dirty (d29fffd0...)`, exit 0.
  - `make docker-build CONTROLLER_IMG=virtrigaud-manager:h1pr4-test VERSION=v0.3.6-h1pr4-test` → image built; `docker run --rm virtrigaud-manager:h1pr4-test --version` returned `virtrigaud-manager v0.3.6-h1pr4-test (d29fffd0...)`, exit 0.
  - `go vet ./...` clean; `make test` 12/12 packages pass.
  - The pre-existing `test/e2e` failure (kind-cluster-required, excluded from `make test`) is not affected by this PR.
- **H1 roadmap status after this merges**:
  - PR-1 / #114 (PR #115) ✅ MERGED
  - PR-2 / #116 (PR #117) ✅ MERGED
  - PR-3 / #118 (PR #119) ✅ MERGED (closes #113)
  - PR-4 / #120 (this PR) ✅ closes #92 (H1 umbrella)
  - PR-5 (v0.4.0 only — breaking `--metrics-secure` default flip) — deferred per ADR-0002.

---

## [2026-05-24 11:38] - fix(make): redirect build/run/docker-build/docker-buildx to canonical path; closes latent #113 (H1 PR-3 / closes #118)
**Author:** @wrkode (William Rizzo)

### Audit finding
Third implementation chunk of the H1 build-path consolidation roadmap (`fieldTesting/ADR-0002-build-path-consolidation.md`). The Makefile's local-dev build targets (`make build`, `make run`, `make docker-build`, `make docker-build-multiplatform`, `make docker-buildx`) all invoked the orphan path — `cmd/main.go` and the root `Dockerfile`. That orphan path produces an **incomplete manager binary** (this is #113, the latent bug discovered during the H1 audit):

- **No `metrics.SetupMetrics()` call** — local-dev manager never emits `virtrigaud_build_info`.
- **VMSnapshot controller NOT registered** — local-dev manager could not reconcile `VMSnapshot` CRs at all.
- **VMMigration controller NOT registered** — local-dev manager could not reconcile `VMMigration` CRs at all.

Anyone using `make docker-build`, `make build`, `make run` for local testing has been running a half-working binary ever since these paths diverged.

### Changed
- `Makefile:build` target — now `go build ... ./cmd/manager` (was `cmd/main.go`).
- `Makefile:run` target — now `go run ./cmd/manager` (was `./cmd/main.go`).
- `Makefile:docker-build` target — now `$(CONTAINER_TOOL) build -f build/Dockerfile.manager ...` (was implicit `-f Dockerfile` pointing at the orphan root Dockerfile). All 10 `--build-arg` flags are preserved verbatim.
- `Makefile:docker-build-multiplatform` target — same `-f build/Dockerfile.manager` redirect.
- `Makefile:docker-buildx` target — `sed` source is now `build/Dockerfile.manager` (was root `Dockerfile`). The transformed `Dockerfile.cross` output continues to be written to and cleaned up from repo root.

All five edits carry an inline `# H1 PR-3 (#118): ...` comment block explaining the redirect and pointing at #113.

### Fixed
- **#113** — `make docker-build`, `make build`, and `make run` now produce a manager binary functionally equivalent to the released image: emits `virtrigaud_build_info`, registers all 8 controllers (including VMSnapshot + VMMigration), exposes the `--version` handler from PR #115 (H1 PR-1).

### Why
1. **Fixes the silent #113 bug** at its root cause: the Makefile targets pointed at the wrong files. Every local-dev manager build before this commit was missing real functionality.
2. **Cleans up the H1 surface area** before PR-4 (orphan deletion): after this PR lands, `cmd/main.go` and root `Dockerfile` are truly orphaned — no Makefile target, no CI workflow, no hack script references them anymore. (Verified — `hack/dev-deploy.sh` already used `-f build/Dockerfile.manager`; the CI `Build (manager)` matrix job already used `go build ./cmd/manager` directly.)
3. **Brings local-dev parity with CI and release**: CI's `Build (manager)` job and the release workflow both target `cmd/manager/main.go` + `build/Dockerfile.manager`. Now `make build` matches.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout — only for local-dev workflows: anyone doing `make dev-deploy` after running `make docker-build` will pick up a manager binary with NEW controllers (VMSnapshot, VMMigration) and NEW metrics emission they were previously missing. This is the desired behaviour, but if local CRs of those kinds were left in a stale state, they will start being reconciled. Call out to local-dev users: review your VMSnapshot / VMMigration CRs after upgrading. CI and release builds are unaffected (already on canonical).
- [ ] Config change only
- [ ] Documentation only

### Notes
- **CI is unaffected.** `.github/workflows/ci.yml` line 256 uses `go build -o bin/manager ./cmd/manager` directly (not `make build`). Line 343's container-image matrix uses `./build/Dockerfile.manager` for manager (not `make docker-build`). The CI `Build (manager)` job was already on canonical; only local-dev workflows shift with this PR.
- **`make dev-deploy` is unaffected.** `hack/dev-deploy.sh` line 148 already used `-f build/Dockerfile.manager`. The bug only bit users who reached for raw `make docker-build` or `make build`.
- Manual smoke pre-merge:
  - `make build` → `./bin/manager --version` returned `virtrigaud-manager v0.3.5-6-g4c1746d-dirty (4c1746da3...)`, exit 0.
  - `strings ./bin/manager | grep -E 'vmsnapshot-controller|vmmigration-controller'` → both symbols present.
  - `make docker-build CONTROLLER_IMG=virtrigaud-manager:h1pr3-test VERSION=v0.3.6-h1pr3-test` → image built; `docker run --rm virtrigaud-manager:h1pr3-test --version` returned `virtrigaud-manager v0.3.6-h1pr3-test (4c1746d...)`, exit 0.
- This is the third PR in the H1 roadmap; PR-4 (delete `cmd/main.go` + root `Dockerfile`) is now unblocked and can land next.

---

## [2026-05-24 10:50] - feat(build): add BUILDER_IMAGE/BASE_IMAGE/GOPROXY ARGs + CA-cert handling to build/Dockerfile.manager (H1 PR-2 / closes #116)
**Author:** @wrkode (William Rizzo)

### Audit finding
Second implementation chunk of the H1 build-path consolidation roadmap (`fieldTesting/ADR-0002-build-path-consolidation.md`). Audit on 2026-05-24 found that:
- `Makefile:docker-build` already passes 10 build-args (`VERSION`, `GIT_SHA`, `TARGETOS`, `TARGETARCH`, `BUILDER_IMAGE`, `BASE_IMAGE`, `GOPROXY`, `GOINSECURE`, `GOPRIVATE`, `GOSUMDB`).
- All 3 provider Dockerfiles (`cmd/provider-{libvirt,vsphere,proxmox}/Dockerfile`) already declare all 10 ARGs.
- `build/Dockerfile.manager` declared only 4 of them — the other 6 were silently ignored. The manager Dockerfile was the project's only outlier.
- The orphan root `Dockerfile` did declare all 10, **but** its CA-cert handling used `COPY *.crt /etc/ssl/certs/` which is broken in two ways: (1) the glob matches zero files on a fresh checkout (verified `ls *.crt` errors), and (2) `update-ca-certificates` reads inputs from `/usr/local/share/ca-certificates/`, not `/etc/ssl/certs/` — copying to the latter directly does nothing.

### Added
- `build/Dockerfile.manager`: `ARG BUILDER_IMAGE=docker.io/golang:1.25` and `ARG BASE_IMAGE=gcr.io/distroless/static:nonroot`, used in the corresponding `FROM` lines. Defaults match the previously-hardcoded images so upstream release builds produce byte-equivalent images.
- `build/Dockerfile.manager`: `ARG GOPROXY=""`, `ARG GOINSECURE=""`, `ARG GOPRIVATE=""`, `ARG GOSUMDB="sum.golang.org"` with corresponding `ENV` lines. Matches the pattern used by all 3 provider Dockerfiles. Defaults preserve current upstream behaviour.
- `build/Dockerfile.manager`: `COPY ca-certs/ /usr/local/share/ca-certificates/` + `RUN update-ca-certificates` **before** `RUN go mod download`. Required ordering so corporate TLS-intercepting proxies do not break the Go module fetch. Uses the correct Debian/Ubuntu directory (`/usr/local/share/ca-certificates/`), unlike the orphan's broken `/etc/ssl/certs/` path.
- `ca-certs/.gitkeep`: new placeholder so the directory exists in the upstream tree. Without `.crt` files, the `COPY` + `update-ca-certificates` pair is a no-op (the script skips non-`.crt` files).
- `ca-certs/README.md`: 50+ line operator guide. Covers when to populate this directory (corporate TLS proxies, internal module mirrors), how to use it (drop `.crt` files, rebuild), the builder-stage-only scope (NOT runtime CAs — those go via Kubernetes secrets/configmaps), and security warnings (one cert per file, never private keys, do not commit org-internal CAs to public forks).

### Why
1. **Brings the manager Dockerfile in line with the rest of the project.** All 3 provider Dockerfiles already declared these ARGs; the manager being the outlier was tech-debt that operators have to discover via failed builds in regulated environments.
2. **Unblocks corporate / banking deployments without a Dockerfile fork.** Banking customers categorically cannot pull from `docker.io` or `gcr.io` in their production build pipelines. Without these ARGs they forked; forks drift. With them, an internal mirror override is one `--build-arg` away.
3. **CA-cert handling fixes a latent bug from the orphan Dockerfile.** The orphan's `COPY *.crt /etc/ssl/certs/` was broken on fresh checkouts AND used the wrong directory. The corrected pattern in this PR (`ca-certs/` → `/usr/local/share/ca-certificates/`) works upstream as a no-op AND works for corporate forks that drop their certs in.
4. **Subdirectory pattern limits the security blast radius.** ADR-0002 flagged the orphan's repo-root glob as a risk: any stray `.crt` file in the build context would have been shipped into the image. The dedicated `ca-certs/` subdirectory makes the intent explicit and confines what gets bundled.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image embeds the additional CA-cert handling layer — content-identical at defaults but cache key changes)
- [ ] Config change only
- [ ] Documentation only

### Notes
- Default behaviour is preserved at every level: `docker build -f build/Dockerfile.manager .` with no `--build-arg` overrides produces a functionally equivalent image to today's release. Verified pre-merge by building locally with default ARGs, then again with `--build-arg BUILDER_IMAGE=docker.io/golang:1.25 --build-arg GOSUMDB=off` — both produced runnable `--version`-correct images.
- Manual smoke pre-merge: \
  `docker build -f build/Dockerfile.manager --build-arg VERSION=v0.3.6-h1pr2-test --build-arg GIT_SHA=$(git rev-parse HEAD) -t virtrigaud-manager:h1pr2-test .` succeeded. \
  `docker run --rm virtrigaud-manager:h1pr2-test --version` returned `virtrigaud-manager v0.3.6-h1pr2-test (23457f932...)` with exit 0. This proves PR-1 (`--version` handler) + PR-2 (Dockerfile parametrization) stack correctly.
- The Makefile already passes all 10 build-args; the release workflow passes only `VERSION` + `GIT_SHA` (the new ARGs fall back to defaults — release behaviour unchanged). No Makefile or release.yml changes were needed in this PR.
- Pre-existing `make lint` tooling drift remains as noted in #114.

---

## [2026-05-24 10:09] - feat(cmd/manager): port --version flag, certwatcher, and metrics RBAC filter (H1 PR-1 / closes #114)
**Author:** @wrkode (William Rizzo)

### Audit finding
First implementation chunk of the H1 build-path consolidation roadmap (see `fieldTesting/ADR-0002-build-path-consolidation.md`). Three features the local-dev orphan `cmd/main.go` had but the canonical `cmd/manager/main.go` lacked are now ported into the canonical entrypoint, with defaults preserved so existing deployments see zero behaviour change.

### Added
- `cmd/manager/main.go`: `versionString()` helper — single-line banner emitted by `--version`. Extracted from main() so it is unit-testable without subprocess execution. Format is `virtrigaud-manager <version.String()>`, pinned by `TestVersionString` so release-verification grep patterns stay stable.
- `cmd/manager/main.go`: `--version` flag handler at the top of main(). Mirrors the cmd/main.go behaviour. Uses `os.Args[1]` lookup (not `flag.BoolVar` + `flag.Parse`) so `--version` works even when invalid flags follow and avoids the long help text on error.
- `cmd/manager/main.go`: certificate-rotation flag set — `--webhook-cert-path`, `--webhook-cert-name`, `--webhook-cert-key`, `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key`. All default to empty/`tls.crt`/`tls.key`, matching cmd/main.go.
- `cmd/manager/main.go`: `sigs.k8s.io/controller-runtime/pkg/certwatcher` integration for both webhook and metrics endpoints. Hot cert rotation works once `--*-cert-path` is set. Watchers are registered as Runnables on the manager after controllers and are nil-guarded so zero-overhead at defaults.
- `cmd/manager/main.go`: `sigs.k8s.io/controller-runtime/pkg/metrics/filters.WithAuthenticationAndAuthorization` wiring on the metrics endpoint. **Activates ONLY when `--metrics-secure=true`**, which currently defaults to `false`. When enabled, `/metrics` becomes an RBAC-checked resource (only ServiceAccounts with `get` on the `/metrics` nonResourceURL can scrape).
- `cmd/manager/main.go`: imports `fmt`, `path/filepath`, `sigs.k8s.io/controller-runtime/pkg/certwatcher`, `sigs.k8s.io/controller-runtime/pkg/metrics/filters`. Renamed metrics-server alias from `server` to `metricsserver` for clarity (the prior unqualified name collided readability-wise with the controller-runtime `webhook` package).
- `cmd/manager/main_test.go`: new file. `TestVersionString` pins the banner format and the contract that it delegates to `internal/version.String()` (the same source of truth as the `virtrigaud_build_info` metric label).

### Changed
- `cmd/manager/main.go`: webhook server construction now uses `webhookTLSOpts` (base `tlsOpts` plus the webhook certwatcher's `GetCertificate` callback when `--webhook-cert-path` is set). When the flag is unset, `webhookTLSOpts == tlsOpts` — no change vs. pre-PR.
- `cmd/manager/main.go`: metrics server options are now built as a `metricsServerOptions` variable (rather than constructed inline inside `ctrl.NewManager`) so the `FilterProvider` and the metrics certwatcher's `GetCertificate` callback can be layered on conditionally. At defaults (`--metrics-secure=false`, `--metrics-cert-path=""`), the resulting `metricsserver.Options{...}` is byte-for-byte equivalent to the prior inline struct.

### Why
Per ADR-0002, the H1 consolidation needs four PRs in sequence. PR-1 is the prerequisite for retiring the local-dev orphan (PR-4): the canonical path must first absorb every feature the orphan had that operators may rely on. The three features ported here are exactly the ones flagged by the 2026-05-23 audit comment on #92 — `--version` (operators script against it), certwatcher (cert-manager renewals don't require pod restarts), and the metrics RBAC filter (banking-compliance posture, anonymous /metrics exposure is a routine audit finding).

By keeping `--metrics-secure=false` as the default in this PR, every existing deployment sees identical behaviour. The default flip is a separate, breaking change held for v0.4.0 (PR-5).

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout (the manager binary gains features, but their activation requires explicit flag settings)
- [ ] Config change only
- [ ] Documentation only

### Notes
- `make build` still builds `cmd/main.go` (the orphan) — this PR does not touch Makefile targets. That redirection is PR-3 of the H1 roadmap, which also closes the latent bug #113 (orphan binary missing `virtrigaud_build_info` emission + VMSnapshot + VMMigration controllers).
- `make lint` continues to fail locally on the pre-existing `.golangci.yml` v2 vs `golangci-lint v1.64.8` tooling drift; CI's runner-installed version is newer and passes. Tracked separately as future tooling work.
- Manual smoke pre-merge: built with `-ldflags '-X .../version.Version=v0.3.6-h1pr1-dev -X .../version.GitSHA=$(git rev-parse HEAD)'`, ran `./bin/manager --version`, got `virtrigaud-manager v0.3.6-h1pr1-dev (66d0fbc9cef23bd6561bb1f86239660a3601433c)` with exit 0.

---

## [2026-05-24 08:02] - feat(obs): wire CircuitBreaker into provider gRPC RPC path (closes #111)
**Author:** @wrkode (William Rizzo)

### Audit finding
v0.3.5 release smoke on `vr1.lab.k8` confirmed that `virtrigaud_circuit_breaker_state` and `virtrigaud_circuit_breaker_failures_total` emit zero samples in production despite G5 (PR #108) wiring all metric emission paths correctly. Root cause: **the CircuitBreaker code in `internal/resilience/` was never instantiated by any production caller**. The gRPC client in `internal/transport/grpc/client.go` had a G4 metrics interceptor but no resilience layer. Net effect: no real circuit-breaker protection on the manager→provider RPC path, AND no breaker metrics ever emitted. The package was correct but dead.

### Added
- `internal/transport/grpc/client.go`: New `providerCircuitBreakerInterceptor` — a `grpc.UnaryClientInterceptor` that wraps every outbound RPC with `CircuitBreaker.Call`. Chained AFTER the existing G4 metrics interceptor via `grpc.WithChainUnaryInterceptor`, so circuit-breaker rejections still show up in `virtrigaud_provider_rpc_requests_total{code="Unavailable"}` (no silent drops).
- `internal/transport/grpc/client.go`: New `isInfraFailure` classifier — pins the gRPC-code policy that decides what counts as a "provider health" failure (tripping the breaker) vs a business error (passing through). Tripping codes: `Unavailable`, `DeadlineExceeded`, `Internal`, `Unknown`. Pass-through codes: `NotFound`, `InvalidArgument`, `AlreadyExists`, `FailedPrecondition`, `PermissionDenied`, `Unauthenticated`, `Canceled`, `ResourceExhausted`, `Aborted`, `OutOfRange`, `Unimplemented`. Documented inline with rationale per code.
- `internal/transport/grpc/client_circuitbreaker_test.go`: New file. 5 tests covering: gRPC-code classification table (15 cases), infra errors trip the breaker after `FailureThreshold`, business errors never trip regardless of count, successful calls leave the breaker Closed, and the full `Closed → Open → HalfOpen → Closed` lifecycle through the interceptor (not just the underlying breaker).
- `cmd/manager/main.go`: Constructs a `resilience.NewRegistry(resilience.DefaultConfig())` once at startup and threads it to `remote.NewResolver`. One CircuitBreaker per Provider CR is allocated lazily by the Resolver via `Registry.GetOrCreate("rpc", providerType, providerName)`.
- `cmd/main.go` (parallel build path, see #92 H1): same wiring so this build path retains parity.

### Changed
- `internal/transport/grpc/client.go`: `NewClient` signature now accepts `cb *resilience.CircuitBreaker` (4th parameter, before `tlsConfig`). Passing `nil` disables circuit-breaker wiring — supported for unit tests that exercise real gRPC failure semantics without the breaker interposing.
- `internal/runtime/remote/resolver.go`: `NewResolver` signature now accepts `cbRegistry *resilience.Registry` (2nd parameter). `getRemoteProvider` calls `cbRegistry.GetOrCreate(...)` per-Provider before passing the resulting breaker to `NewClient`. `CleanupClient` also calls `cbRegistry.Remove(...)` so deleted Providers stop emitting `virtrigaud_circuit_breaker_*` series and don't leak CB instances.
- `internal/controller/vmmigration_controller_test.go`: Test now passes `nil` registry to `remote.NewResolver` (it uses a fake k8s client; no real gRPC dialing happens, so the wiring would be inert).

### Why
1. **Real circuit-breaker protection on the RPC path.** Before this change, a flapping hypervisor (e.g. the `kex_exchange_identification` libvirt SSH issue tracked as #I1) caused the manager to keep hammering the provider with retries — v0.3.5 smoke showed 22 `DeadlineExceeded` + 9 `Canceled` Validate RPCs against libvirt in a few minutes. With G6, after `FailureThreshold=10` infra failures, the breaker opens and short-circuits subsequent RPCs for `ResetTimeout=60s`, giving the downstream hypervisor room to recover and the manager room to free up reconcile slots.
2. **`virtrigaud_circuit_breaker_*` metric families now emit.** Closes the last empty G-track family from the v0.3.5 smoke gap analysis (6/8 → 8/8 families with samples). Operators can dashboard `virtrigaud_circuit_breaker_state{provider_type, provider}` and alert on `state > 0` to catch any provider whose breaker is non-closed.
3. **Opinionated classification matters.** A 1000-VM reconcile loop that gets `NotFound` for one missing VM shouldn't trip the breaker — the provider is healthy; the request was bad. Conversely, a single `Unavailable` from a dead provider pod IS a health signal. The classifier encodes this distinction at one place, with documented rationale per code, so future RPCs added to the proto inherit it automatically.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager binary; new metrics emit after rollout)
- [ ] Config change only
- [ ] Documentation only

### Notes
- Config knobs (`FailureThreshold`, `ResetTimeout`, `HalfOpenMaxCalls`) use `resilience.DefaultConfig()` globally — no per-Provider CRD field. If operators ask for per-Provider tuning later, the wiring is straightforward (add `Spec.Runtime.CircuitBreaker` to the Provider CRD and pass through the Resolver).
- Pre-existing tooling drift discovered during verify: `.golangci.yml` is v2 syntax (commit `8d3be28`), but Makefile pins `golangci-lint v1.64.8`. `make lint` fails. Independent of this PR; should be tracked as a separate tooling issue.
- v0.3.6-rc1 smoke checklist should include: scale a provider deployment to 0, watch the breaker's state gauge flip `0 → 2` after `FailureThreshold` infra failures within `ResetTimeout`; scale back up and watch it transition `2 → 1 → 0` through HalfOpen. This proves the wiring end-to-end on the cluster.

---

## [2026-05-23 14:02] - feat(obs): document + test CircuitBreakerMetrics lifecycle (closes #91)
**Author:** @wrkode (William Rizzo)

### Audit finding
G5 began as "audit + add missing CircuitBreakerMetrics hooks." The audit revealed the instrumentation was **already complete**:
- `NewCircuitBreaker` emits `SetState(Closed)` on construction (initial state)
- `transitionToClosed` / `transitionToOpen` / `transitionToHalfOpen` each emit the corresponding `SetState` sample
- `recordFailure` increments `RecordFailure` on every counted failure

No new instrumentation calls were needed. The issue body's "not all transitions are recorded" was based on incomplete information; closing #91 with documentation + tests as the deliverable.

### Added
- `internal/resilience/circuitbreaker_metrics_test.go`: New file. 4 lifecycle tests asserting the metric contract end-to-end:
  - `TestCircuitBreakerMetrics_InitialStateIsClosed` — construction emits `SetState(Closed)`
  - `TestCircuitBreakerMetrics_FullLifecycle` — exercises closed → open → half-open → closed with `FailureThreshold=2`, asserting the gauge value matches the expected state at every step AND the failures counter increments on every counted failure
  - `TestCircuitBreakerMetrics_FailureInHalfOpenReopens` — a failure during HalfOpen must re-set the gauge to Open (not leave it at HalfOpen)
  - `TestCircuitBreakerMetrics_ResetEmitsClosedGauge` — explicit `Reset()` emits `SetState(Closed)`

### Changed
- `internal/resilience/circuitbreaker.go`: Doc comments at the metric call sites (`recordFailure`, `transitionToClosed`, `transitionToOpen`, `transitionToHalfOpen`) now explicitly state what metric is emitted, with what value, and what operators dashboard against. Makes the existing instrumentation contract visible without reading both this file and `internal/obs/metrics/metrics.go`.

### Why
Fifth and final in the G-track (#86). With this PR, **all 11 previously-empty `virtrigaud_*` metric families have at least one emission path**:
- `virtrigaud_build_info` (v0.3.4 / PRs #83, #84, #85)
- `virtrigaud_manager_reconcile_total` + `_duration_seconds` (G1-G3 / PRs #101, #103, #106)
- `virtrigaud_errors_total` (G1-G3 across all reconcilers)
- `virtrigaud_provider_rpc_requests_total` + `_latency_seconds` (G4 / PR #107)
- `virtrigaud_circuit_breaker_state` + `_failures_total` (G5 / this PR — instrumentation already wired; this PR adds the test contract)

Remaining unwired families (`virtrigaud_queue_depth`, `virtrigaud_vm_operations_total`, `virtrigaud_provider_tasks_inflight`, `virtrigaud_ip_discovery_duration_seconds`) are not in the G-track scope — they require their own design decisions about WHERE to record (controller queue depths require workqueue introspection; VM operations need a per-RPC mapping; etc.). Tracked as v0.3.6+ work.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout (no behavior change — only doc comments + new tests; the existing instrumentation was already wired)
- [ ] Config change only
- [ ] Documentation only

Technically test-only + comments. Targeted for **v0.3.5**.

### Verification
```bash
go test -v -count=1 -run TestCircuitBreakerMetrics ./internal/resilience/
# PASS: 4 lifecycle tests
make test  # resilience pkg coverage now 32.7%
make test-integration  # still green
```

### References
- Closes #91
- Umbrella: #86 (G-track COMPLETE — all 5 sub-tasks closed)
- Pattern: PRs #101 (G1), #103 (G2), #106 (G3 + K5), #107 (G4)
- Coordinates with PR #100 (circuit-breaker correctness fix) — half-open semantics now both correct AND tested

### After this merges: cut v0.3.5-rc1
Per the release process established for v0.3.5:
1. Tag `v0.3.5-rc1` from the merge commit
2. Release workflow produces images + chart
3. `helm upgrade --version v0.3.5-rc1 --reset-values` on `vr1.lab.k8`
4. Smoke: `curl /metrics | grep '^virtrigaud_'` should now show 12+ families with samples (vs the 1 family that v0.3.4 ships)
5. If smoke passes, tag `v0.3.5` final from the same commit

---

## [2026-05-23 13:31] - feat(obs): wire ProviderRPCMetrics into gRPC client middleware (closes #90)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/transport/grpc/client.go`: New `providerRPCMetricsInterceptor(providerType)` returns a `grpc.UnaryClientInterceptor` that wraps every outbound provider RPC. On each call it records `virtrigaud_provider_rpc_requests_total{provider_type,method,code}` and `virtrigaud_provider_rpc_latency_seconds{provider_type,method}` via the existing `metrics.NewRPCTimer` API.
- `internal/transport/grpc/client.go`: New `shortRPCMethod(fullMethod)` helper that extracts the RPC name from gRPC's full path (`/provider.v1.Provider/Validate` → `Validate`).
- `internal/transport/grpc/client.go`: New `grpcCodeString(err)` helper that returns the canonical gRPC status code string (`"OK"`, `"Unavailable"`, `"DeadlineExceeded"`, etc.) for the metric `code` label.
- `internal/transport/grpc/client_metrics_test.go`: New file. 5 tests / 15 sub-cases covering: `shortRPCMethod` parsing (6 edge cases), `grpcCodeString` mapping (5 codes incl. nil and non-gRPC errors), and 4 bufconn-based integration tests exercising the interceptor against an in-process gRPC server (successful call, error propagation, latency histogram, deadline-exceeded). No real network.

### Changed
- `internal/transport/grpc/client.go`: `NewClient` signature now takes a `providerType string` parameter inserted between `endpoint` and `tlsConfig`. The new parameter populates the `provider_type` metric label on every RPC made through this client. Empty string is permitted (label will be empty) but production callers should pass `provider.Spec.Type`.
- `internal/runtime/remote/resolver.go`: Updated the sole caller of `NewClient` to pass `string(provider.Spec.Type)` (the only callsite repo-wide; verified by `grep -rn "transport/grpc\".NewClient" --include="*.go"`).

### Why
Fourth in the G-track. Per-RPC latency + error code is the most operationally valuable single signal for the remote-provider architecture: it answers "which RPC method is slow?", "which provider type is flapping?", "are gRPC errors concentrated on specific endpoints?", and "what's our SLO baseline?" — questions that per-Kind reconcile counters (G1-G3) can't.

The interceptor is the natural insertion point — every gRPC call goes through it without per-callsite changes. The 17+ `c.client.XXX(...)` callsites in `client.go` need no edits.

Coordinates with ADR-0001 follow-up F3 (interceptor coverage audit) — this PR establishes the metrics interceptor; future PRs can layer logging/tracing interceptors using the same pattern.

### Impact
- [ ] Breaking change to public API (the `NewClient` signature change is internal-only; only `internal/runtime/remote/resolver.go` uses it; verified)
- [x] Requires cluster rollout (new manager image needed to emit the additional families)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5**.

### Verification
```bash
go test -v -count=1 -run "TestShortRPCMethod|TestGrpcCodeString|TestProviderRPCMetricsInterceptor" ./internal/transport/grpc/
# PASS: 5 tests / 15 sub-cases
make test  # transport/grpc pkg coverage 0% -> 9.2% (first tests in this package)
make test-integration  # still green
```

Post-deploy (after v0.3.5-rc1):
```bash
curl /metrics | grep '^virtrigaud_provider_rpc_requests_total'
# expect samples with provider_type=libvirt|vsphere|proxmox, method=Validate|Create|Describe|..., code=OK|Unavailable|...
curl /metrics | grep '^virtrigaud_provider_rpc_latency_seconds_bucket'
# expect populated histogram per provider_type x method combination
```

### References
- Closes #90
- Umbrella: #86
- Pattern: PRs #101 (G1), #103 (G2), #106 (G3 + K5)
- Coordinates with ADR-0001 F3

---

## [2026-05-23 12:46] - feat(obs): instrument remaining 6 reconcilers + fix double-count bug (closes #89, #105)
**Author:** @wrkode (William Rizzo)

Two related changes in one PR (both touch the same files / pattern):

### Fixed
- `internal/controller/vmmigration_controller.go` and `internal/controller/vmsnapshot_controller.go`: **Double-count bug** (#105). Prior `Reconcile` used `defer timer.Finish(metrics.OutcomeSuccess)` — the argument was evaluated and captured at defer-time — alongside explicit `timer.Finish(metrics.OutcomeError)` on error paths. Errored reconciles recorded TWO samples (one error from the explicit call, one success from the deferred call) because the deferred Finish always ran. `RecordReconcile` is not idempotent. Inflated success counters and made error-rate alerts under-report. Refactored both files to the G1 pattern (named return values + single deferred outcome-inference block that uses the actual return state).

### Added
Six reconcilers now instrumented (closes #89):
- `internal/controller/vmmigration_controller.go`: reconcile timer + `errReasonGetMigration` constant + `RecordError` on get/add-finalizer paths
- `internal/controller/vmsnapshot_controller.go`: reconcile timer + `errReasonGetSnapshot` constant + `RecordError` on get/add-finalizer/get-VM paths
- `internal/controller/vmadoption_controller.go`: reconcile timer + 3 new constants (`adoption-discover-failed`, `adoption-status-update`, `adoption-invalid-filter`) + `RecordError` at 4 sites
- `internal/controller/vmclass_controller.go`: reconcile timer only (stub Reconciler, no error paths)
- `internal/controller/vmimage_controller.go`: same
- `internal/controller/vmnetworkattachment_controller.go`: same

After this PR, all 8 reconcilers emit `virtrigaud_manager_reconcile_total{kind=...,outcome=...}` and `virtrigaud_manager_reconcile_duration_seconds{kind=...}`:
- `VirtualMachine` (G1, #87, PR #101)
- `Provider` (G2, #88, PR #103)
- `VMMigration`, `VMSnapshot`, `VMAdoption`, `VMClass`, `VMImage`, `VMNetworkAttachment` (G3, this PR)

### Added — tests
- `internal/controller/g3_metrics_test.go`: 4 tests covering:
  - `TestVMMigrationReconcile_NoDoubleCountOnSuccessfulNotFound` — regression canary for #105
  - `TestVMSnapshotReconcile_NoDoubleCountOnSuccessfulNotFound` — same
  - `TestG3_StubReconcilersEmitTimer` — table-driven across VMClass, VMImage, VMNetworkAttachment stubs; asserts outcome=success emits on no-op reconcile
  - `TestVMAdoptionReconcile_NotFoundIsSuccessOutcome` — IsNotFound contract for VMAdoption

### Why
G3 was the bulk pass over the remaining reconcilers per the G-track umbrella (#86) — meant to be mostly mechanical after G1 and G2 established the pattern. The audit step (recommended in #89's body) turned up the double-count bug in the 2 reconcilers that already had partial instrumentation. Filed as #105 and bundled into the same PR because both fixes touch the same files and the refactor was the same.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed to emit the additional families AND to fix the double-count)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5**.

### Verification
```bash
go test -v -count=1 -run "TestG3|TestVMMigrationReconcile_NoDouble|TestVMSnapshotReconcile_NoDouble|TestVMAdoptionReconcile" ./internal/controller/
# PASS: 6 sub-tests (4 top-level)
make test  # controller pkg coverage 22.4% -> 24.0%
make test-integration  # still green
```

Post-deploy (after v0.3.5-rc1 deploys):
```bash
curl /metrics | awk '/^virtrigaud_manager_reconcile_total/{print $1}' | sort -u
# expect samples across all 8 reconcilers' kinds
```

### References
- Closes #89 (G3)
- Closes #105 (K5 double-count fix)
- Umbrella: #86
- Pattern: PRs #101 (G1), #103 (G2)
- Pre-#105 bug shipping in: every release with `vmmigration` and `vmsnapshot` reconcilers that called the metrics package (at least v0.3.0+)

---

## [2026-05-23 12:24] - fix(ci): pin Build matrix checkout to v4 to mitigate K4 intermittent failure (refs #102)
**Author:** @wrkode (William Rizzo)

### Fixed
- `.github/workflows/ci.yml`: Pinned the `build` matrix job's `actions/checkout` from v6.0.2 (`de0fac2e...`) back to v4 (`34e114876b...`). Added a `DO NOT bump without resolving #102` comment so future contributors don't silently re-introduce the issue.

### Why
The `Build (...)` matrix in `ci.yml` (4 components: manager + 3 providers) has been intermittently failing during the `actions/checkout@v6.0.2` step with `fatal: could not read Username for 'https://github.com': terminal prompts disabled` (3 observed occurrences in 24 hours; tracked as #102). The same `v6.0.2` works fine on every other job in the same workflow (test, lint, security, build-tools, build-images). The asymmetry suggests something specific to this matrix's checkout invocation — possibly the parallel-fetch of the `pull/<N>/merge` ref racing against GitHub-side ref availability, or a regression introduced when PR #74 bumped checkout from v4 → v6 (PR #74 only touched ci.yml, not release.yml).

This is a **partial fix** targeted at the most common K4 occurrence shape:
- ✅ Addresses 2 of 3 observed occurrences (both in `ci.yml`'s `build` matrix)
- ⚠️ Does NOT address release-time K4 (`release.yml`'s `build-and-push` matrix was already on v4 when occurrence 1 hit during the failed v0.3.3.1 release attempt; root cause likely different)
- ⚠️ Does NOT pin `ci.yml`'s `build-images` matrix (no observed failures there; pinning preventively is over-fitting)

If K4 continues to fire after this pin lands, the next move is to wrap the affected checkout in a retry action (e.g., `nick-fields/retry`) as a belt-and-suspenders measure regardless of root cause.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only (CI workflow)
- [ ] Documentation only

CI-only change.

### Verification
- [x] YAML syntax is valid (will be confirmed by CI's own self-load on this PR)
- [ ] **CI on this PR runs cleanly** — if green on first try, the pin works AND we've already broken K4's per-PR pattern.
- [ ] **Next ~5 PRs in the G-track also run cleanly on first try** — that's the real evidence the pin solved the per-cycle friction.

If K4 still hits a future PR's `build` matrix despite this pin, that disproves the v6-regression hypothesis and we widen the search (retry wrapper, GitHub Actions infra investigation, etc.).

### References
- Refs #102 (not Closes — see "partial fix" caveat above; close #102 after we see ~10 consecutive K4-free CI runs across the next few PRs)
- Original v4→v6 bump: PR #74
- Affected runs: PR #101 CI (run 26331518215, succeeded on rerun), post-#101 main CI (run 26331986052, succeeded on rerun), failed v0.3.3.1 release attempt (run 26329718832, never re-attempted; re-cut as v0.3.4)

---

## [2026-05-23 11:58] - feat(obs): instrument ProviderReconciler with reconcile timer + error counter (closes #88)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/controller/provider_controller.go`: `Reconcile` now records `virtrigaud_manager_reconcile_total{kind="Provider",outcome=...}` and `virtrigaud_manager_reconcile_duration_seconds{kind="Provider"}` via a single deferred block that infers outcome from named return values. Mirrors the G1 pattern shipped for `VirtualMachineReconciler` in PR #101.
- `internal/controller/provider_controller.go`: 6 `metrics.RecordError(reason, ComponentManager)` calls at the documented error sites in `Reconcile`, `handleDeletion`, and `reconcileRemoteRuntime`. New `errReason*` constants at the top of the file: `get-provider`, `runtime-spec-invalid`, `service-reconcile-failed`, `deployment-reconcile-failed`, `cleanup-failed`. No collisions with the VirtualMachine reconciler's taxonomy.
- `internal/controller/provider_metrics_test.go`: New file. 3 focused unit tests using `client/fake` (no envtest needed):
  - `TestProviderReconcile_Metrics_NotFoundIsSuccessOutcome` — IsNotFound is outcome=success
  - `TestProviderReconcile_Metrics_MissingRuntimeIsErrorOutcome` — missing `spec.runtime` is outcome=error + `errors_total{reason="runtime-spec-invalid"}`
  - `TestProviderReconcile_Metrics_DurationHistogramFires` — smoke that the histogram observes at least one sample under `kind="Provider"`

### Why
Second in the G-track sequence (after G1 / PR #101). `ProviderReconciler` reconciles Provider CRs into per-Provider Deployments + Services + Secrets, and its failure modes (missing runtime spec, Service/Deployment reconcile errors) are operationally distinct from VM reconcile failures. Per-Kind reconcile counters and a dedicated reason taxonomy let operators dashboard "Provider lifecycle health" separately from "VM lifecycle health."

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed to emit)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5** alongside G1 (already merged), G3, G4, G5.

### Verification
Locally:
```bash
go test -v -count=1 -run TestProviderReconcile_Metrics ./internal/controller/
# PASS: 3 tests
make test  # controller coverage 21.6% -> 22.4%
make test-integration  # still green
```

Post-deploy (after the full G-track lands + v0.3.5-rc1 deploys):
```bash
curl /metrics | grep '^virtrigaud_manager_reconcile_total{kind="Provider"'
# expect non-zero samples for outcome=success|requeue|error
```

### References
- Closes #88
- Umbrella: #86
- Pattern established by: #101 (G1)
- PROJECT_CONTEXT.md G-track item G2

---

## [2026-05-23 11:26] - feat(obs): instrument VirtualMachineReconciler with reconcile timer + structured error counter (closes #87)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/controller/virtualmachine_controller.go`: Reconcile entry now records a per-call timer to `virtrigaud_manager_reconcile_total{kind="VirtualMachine",outcome=...}` + `virtrigaud_manager_reconcile_duration_seconds{kind="VirtualMachine"}` via a single deferred block that infers outcome from named return values (`success` / `error` / `requeue`).
- `internal/controller/virtualmachine_controller.go`: 11 `metrics.RecordError(reason, ComponentManager)` calls at the documented error sites in Reconcile + reconcileVM + handleDeletion, emitting `virtrigaud_errors_total{reason=...,component="manager"}`. Structured reason taxonomy declared as `errReason*` constants at the top of the file: `get-vm`, `add-finalizer`, `remove-finalizer`, `deps-not-found`, `deps-error`, `provider-resolve`, `provider-validate`, `provider-describe`, `provider-task-status`, `provider-delete`.
- `internal/controller/virtualmachine_metrics_test.go`: New file. 3 focused unit tests using `client/fake` (no envtest needed):
  - `TestReconcile_Metrics_NotFoundIsSuccessOutcome` — reconciling a non-existent VM (IsNotFound) records outcome=success
  - `TestReconcile_Metrics_MissingDepsIsRequeueOutcome` — reconciling a VM whose Provider is missing records outcome=requeue + `errors_total{reason="deps-not-found"}` (Provider-not-found is recoverable transient state, NOT outcome=error noise)
  - `TestReconcile_Metrics_DurationHistogramFires` — smoke that the duration histogram observes at least one sample per reconcile

### Why
v0.3.3 wired up the metrics infrastructure (registry binding PR #83, SetupMetrics PR #84, accurate labels PR #85, integration-test CI gate PR #98) but only `virtrigaud_build_info` emitted samples. This change starts populating the 11 other registered metric families by instrumenting the primary work loop — `VirtualMachineReconciler` — which is expected to be the loudest emitter under normal operation. After this PR deploys, operators scraping `/metrics` will see non-zero `virtrigaud_manager_reconcile_total{kind="VirtualMachine",...}` samples and a populated `virtrigaud_errors_total` histogram from real reconcile activity.

The dual-channel design (outcome timer + structured error counter) was deliberate: the outcome label answers "was this reconcile a success/error/requeue from controller-runtime's perspective", and the error reason answers "WHY". A Provider-not-found situation is `outcome=requeue` (we asked to come back in 30s, not an error) AND `errors_total{reason="deps-not-found"}` (we want to dashboard the volume of missing-dep blocking states). These don't always overlap — a `deps-error` from a k8s API issue produces `outcome=requeue + reason=deps-error`, while a finalizer add failure produces `outcome=error + reason=add-finalizer`.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed for the metrics to start emitting)
- [ ] Config change only
- [ ] Documentation only

Targeted for **v0.3.5** alongside G2-G5 (other reconcilers + gRPC middleware + circuit breaker).

### Verification
Locally:
```bash
go test -v -count=1 -run TestReconcile_Metrics ./internal/controller/
# PASS: 3 tests
make test  # full unit suite still green; controller coverage 20.8% -> 21.6%
make test-integration  # still green; no integration regressions
```

Post-deploy (after merging the G-track sequence and cutting v0.3.5-rc1):
```bash
kubectl -n virtrigaud-system port-forward deploy/virtrigaud-manager 8080:8080 &
curl -s :8080/metrics | grep '^virtrigaud_manager_reconcile_total{kind="VirtualMachine"'
# expect one or more samples with outcome=success|error|requeue
curl -s :8080/metrics | grep '^virtrigaud_errors_total{component="manager"'
# expect samples for whichever error reasons actually fired during cluster operation
```

### References
- Closes #87
- Umbrella: #86
- PROJECT_CONTEXT.md G-track item G1
- Companion PRs (G2-G5) coming next

---

## [2026-05-23 11:05] - Fix circuit-breaker half-open transition count + harden timing-fragile test (closes #96, #97)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/resilience/circuitbreaker.go`: `allowCall()` now counts the Open→HalfOpen transition itself as the first half-open call. Previously the transition reset `halfOpenCalls=0` and returned `true` without incrementing, so a circuit configured with `HalfOpenMaxCalls=N` actually required `N+1` successful calls to transition back to Closed instead of `N`. Implementation: `fallthrough` from the StateOpen case into the StateHalfOpen case so the same `halfOpenCalls++` logic runs uniformly. Closes #96.
- `test/integration/observability_test.go`: `TestCombinedResiliencePolicy` now uses `ResetTimeout: 30 * time.Second` (was `50 * time.Millisecond`). The old value was shorter than the test's own retry-loop runtime, so by the time the "Next call should be fast-failed" assertion ran, the reset timeout had already elapsed and the circuit had correctly transitioned to half-open — invalidating the test's premise. The fix decouples the test from wall-clock timing; it was a test bug, not a behavioral bug in the circuit breaker. Closes #97.

### Added
- `internal/resilience/circuitbreaker_test.go`: New file. 4 focused unit tests covering the half-open contract:
  - `TestHalfOpenTransitionCountsAsCall` — the regression canary for #96
  - `TestOpenStateRejectsCallsBeforeResetTimeout` — pins "fast-fail when open" contract, asserts the returned error is a retryable ProviderError
  - `TestHalfOpenFailureReOpensCircuit` — a single failure in half-open re-opens the circuit, regardless of HalfOpenMaxCalls
  - `TestHalfOpenRejectsAfterMaxCalls` — once HalfOpenMaxCalls is reached, additional calls are rejected until recordSuccess transitions to Closed
- `test/integration/observability_test.go`: Removed `t.Skip("blocked by #96")` on `TestCircuitBreakerIntegration` and `t.Skip("blocked by #97")` on `TestCombinedResiliencePolicy`. Both now run in `make test-integration` (which is gated in CI as of PR #98).

### Why
Both issues were discovered when `test/integration/observability_test.go` was wired into CI in PR #98 (which closed #93). #96 is a real code bug — minor production impact (circuits stay half-open one call longer than configured before transitioning to closed) but worth fixing for correctness and to make the codebase match its documented behavior. #97 is a pure test bug with zero production impact — the circuit breaker correctly enforces open-state for the configured `ResetTimeout` window; the test was just timing-fragile.

Bundled into a single PR because both touch the same subsystem (`internal/resilience/circuitbreaker.go` + its integration tests), the fixes are small, and the new unit-test suite covers behavior shared by both.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (next manager image will include the corrected half-open transition behavior — circuits recover one call faster than before, otherwise unchanged)
- [ ] Config change only
- [ ] Documentation only

Not a security fix. Targeted for **v0.3.5** alongside the G-track metrics instrumentation.

### Verification
```bash
go test -v -run "TestHalfOpen|TestOpenState" ./internal/resilience/
# PASS: 4 tests
go test -v -run "TestCircuitBreakerIntegration|TestCombinedResiliencePolicy" ./test/integration/
# PASS: 2 tests (formerly skipped)
make test-integration
# ok ... all tests pass, no skips remaining for these two
```

### References
- Closes #96, Closes #97
- Discovered by PR #98 / commit `3f49026` (CI wiring)
- PROJECT_CONTEXT.md tracks K2 + K3

---

## [2026-05-23 09:23] - SECURITY: fix logging.Redact to mask values, not field names (closes #95)
**Author:** @wrkode (William Rizzo)

### Security
- `internal/obs/logging/logging.go`: `Redactor.Redact` (and the convenience wrappers `RedactString`, `RedactMap`) previously replaced the FIRST capture group of every matched pattern. For two-group kv-pair patterns like `(password|token|...)\s*[:=]\s*([^\s]+)`, this meant the field NAME (group 1) was masked while the SECRET VALUE (group 2) survived in cleartext. Input `"password=hunter2"` produced `"[REDACTED]=hunter2"`.
- Fix: replace the LAST capture group, by documented convention "the value is always the last group". Works correctly for both 1-group patterns (URL passwords) and 2-group patterns (kv-pairs); 0-group patterns continue to replace the entire match.

### Added
- `internal/obs/logging/logging_test.go`: New file. 6 tests with 22+ sub-cases covering:
  - `TestRedactStringValuesNotKeys`: 13 sub-cases (equals/colon separators, `api_key` underscore + hyphen variants, case-insensitivity, all `password|passwd|pwd|token|secret` aliases, quoted values, multiple kv-pairs in one string, URL-embedded passwords incl. `qemu+ssh://`).
  - `TestRedactStringNonSensitiveInputUntouched`: confirms harmless content passes through unchanged.
  - `TestRedactMap`: confirms `RedactMap` masks whole values for sensitive keys, recursively masks values for non-sensitive keys, leaves harmless map entries alone.
  - `TestRedactStringSSHPublicKey`, `TestIsSensitiveKey`, `TestRedactStringIdempotent`.

### Changed
- `test/integration/observability_test.go`: Removed the `t.Skip("blocked by #95 ...")` on `TestObservabilityIntegration`. The integration assertion `assert.NotContains(t, redacted, "secret123")` now passes against the fixed implementation.

### Why
Discovered during PR #98 (wiring `test/integration/` into CI) on 2026-05-23. `TestObservabilityIntegration` had been failing silently in the disabled CI job since the function shipped. The bug is real but has **zero production callsites today** — no code in `internal/controller/`, `internal/providers/`, or `internal/transport/` calls any redaction function. So no operator's v0.3.3 logs contain leaked secrets via this path. Still shipping as a security fix because (a) the function name implies behavior the implementation didn't deliver, giving any future caller a false sense of security; (b) banking-compliance posture demands rapid response to security findings even when current exposure is dormant; (c) the fix is small and isolated.

Cut as v0.3.3.1 patch release (not rolled into v0.3.4 alongside the G-track) for clean per-incident release auditability.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image for the redaction behavior to take effect — only matters once a code path actually calls Redact)
- [ ] Config change only
- [ ] Documentation only

### Verification
Locally:
```bash
go test -v -run TestRedact ./internal/obs/logging/
# PASS: 13 sub-tests
go test -v -run TestObservabilityIntegration ./test/integration/
# PASS: now un-skipped; log output proves the fix:
#   Original: password=secret123 and api_key=abcdef
#   Redacted: password=[REDACTED] and api_key=[REDACTED]
```

### References
- Closes #95
- Discovered by PR #98 (wiring observability_test into CI)
- PROJECT_CONTEXT.md track K1 (renamed from previous "Active TODO" entry)

---

## [2026-05-23 09:07] - Wire observability integration test into CI (closes #93)
**Author:** @wrkode (William Rizzo)

### Added
- `Makefile`: New `test-integration` target — `go test -race -coverprofile=cover-integration.out ./test/integration/...`. Distinct from `make test` (unit, excludes integration) and `make test-e2e` (kind cluster + ginkgo).
- `.github/workflows/ci.yml`: The existing `integration` job now actually runs `make test-integration` instead of `echo "Skipping..."`. Removed the stale Kind cluster + CRD install steps (the only active test under `test/integration/` is `observability_test.go`, which has no Kubernetes API dependency). Added codecov upload tagged `integration`.

### Fixed
- `.github/workflows/ci.yml`: Replaced the no-op `echo "Skipping integration tests in CI due to libvirt dependencies"` step. The "libvirt dependencies" claim was stale — `test/integration/observability_test.go` only imports `internal/obs/*` + `internal/providers/contracts` + `internal/resilience` (no cgo, no libvirt). The disabled file (`vm_lifecycle_test.go.disabled`) was the libvirt-dependent one but Go ignores files not ending in `.go`.

### Security
- This PR surfaces, but does not fix, a critical pre-existing security bug in `internal/obs/logging.RedactString`: the function redacts field NAMES rather than VALUES, so `"password=secret123"` becomes `"[REDACTED]=secret123"` with the secret in cleartext. Tracked as **#95 (P0)**. `TestObservabilityIntegration` is skipped in this PR pending #95's fix.

### Test infrastructure
- `test/integration/observability_test.go`: Marked 3 pre-existing failing tests with `t.Skip(...)` and explicit issue references:
  - `TestObservabilityIntegration` — blocked by #95 (security: RedactString redacts keys not values)
  - `TestCircuitBreakerIntegration` — blocked by #96 (state assertion mismatch)
  - `TestCombinedResiliencePolicy` — blocked by #97 (circuit-open not enforced — possible behavioral bug)
- 4 tests now run in CI on every PR: `TestMetricsIntegration`, `TestHealthSystem`, `TestRetryIntegration`, `TestObservabilityEndToEnd`. Each follow-up PR (#95, #96, #97) un-skips its test as it lands.

### Why
The v0.3.0 → v0.3.2 → v0.3.3-rc1 metrics regression went undetected for three minor releases because the test that catches it (`TestMetricsIntegration`) lives under `test/integration/` and `make test` explicitly excludes that path. CI ran a no-op for the integration job. This PR closes that gap so future metric-binding (and broader observability) regressions are caught at PR time.

Discovering 3 pre-existing failures the moment we wired the gate up — including a security-relevant one — validates the choice to land H3 before the G-track instrumentation work begins.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

Affects CI only. Future PRs gain integration-test gating; the 3 skipped tests are tracked by their own issues and will un-skip in their fix PRs.

### References
- Closes #93
- Discovered: #95 (security, P0), #96, #97
- PROJECT_CONTEXT.md track H3

---

## [2026-05-22 18:27] - Inject VERSION and GIT_SHA into manager binary via ldflags
**Author:** @wrkode (William Rizzo)

### Fixed
- `build/Dockerfile.manager`: Added `ARG VERSION=dev` and `ARG GIT_SHA=unknown` plus `-ldflags="-s -w -X 'github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION}' -X 'github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}'"` on the `go build` step. The container image now reports accurate version metadata at runtime.
- `.github/workflows/release.yml`: Added `build-args: VERSION / GIT_SHA` to the `docker/build-push-action` step so the release tag and commit SHA are passed into the container build. Dockerfiles that don't declare these ARGs (kubectl image, current provider images) silently ignore them — no impact.

### Why
v0.3.3-rc3 smoke test on `vr1.lab.k8` showed `virtrigaud_build_info{component="manager",git_sha="unknown",go_version="go1.25.10",version="dev"} 1` on `/metrics` instead of `version="v0.3.3-rc3"` and the real SHA. The metrics pipeline (registry binding from PR #83 + SetupMetrics call from PR #84) was working — the binary itself had never been built with ldflags to populate `internal/version`. Latent bug since the project's inception, but only became visible once `virtrigaud_build_info` was actually exposed. `virtrigaud_build_info` is the canonical way for operators to know which manager version is running; reporting `version="dev"` to Prometheus dashboards is actively misleading during incident response, which matters for the project's banking-grade operational posture.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new manager image needed for labels to update)
- [ ] Config change only
- [ ] Documentation only

### Verification
After deploying the new image:
```bash
kubectl -n virtrigaud-system port-forward deploy/virtrigaud-manager 8080:8080 &
curl -s http://localhost:8080/metrics | grep '^virtrigaud_build_info'
# expect:
# virtrigaud_build_info{component="manager",git_sha="<rc4-commit-sha>",go_version="...",version="v0.3.3-rc4"} 1
```

Local sanity check:
```bash
go build -ldflags="-s -w \
  -X 'github.com/projectbeskar/virtrigaud/internal/version.Version=v0.3.3-rc4-sanity' \
  -X 'github.com/projectbeskar/virtrigaud/internal/version.GitSHA=abc1234'" \
  -o /tmp/manager ./cmd/manager/
strings /tmp/manager | grep -E 'v0\.3\.3-rc4-sanity|abc1234'
# both literals present in the resulting binary
```

---

## [2026-05-22 15:29] - Emit virtrigaud_build_info on manager startup
**Author:** @wrkode (William Rizzo)

### Fixed
- `cmd/manager/main.go`: Call `metrics.SetupMetrics(version.Version, version.GitSHA, metrics.ComponentManager)` immediately after logger setup. Without this call the build_info `GaugeVec` from `internal/obs/metrics` stays empty after package init, and the `virtrigaud_build_info` family does NOT appear in the manager's `/metrics` output even with the PR #83 registry-binding fix correctly applied.

### Why
v0.3.3-rc2 deploy + smoke test on `vr1.lab.k8` exposed that PR #83 was a necessary-but-not-sufficient fix. The registry binding is structurally correct (verified by `internal/obs/metrics/metrics_test.go`) but Prometheus Vec metrics with zero observations do not appear in `/metrics` output. The only metric that wouldn't depend on reconcile activity is `virtrigaud_build_info`, which is populated by `SetupMetrics`. Nothing in production called `SetupMetrics`, so the family stayed empty and the smoke test continued to report `0 virtrigaud_* metric families` after upgrading to rc2 — same as rc1.

This change makes the manager emit a `virtrigaud_build_info{version,git_sha,go_version,component}` sample on startup, providing the canary metric that release smoke tests can rely on, and serving as proof end-to-end that the registry binding works in production. Per-reconciler instrumentation (VirtualMachineReconciler, ProviderReconciler) for full operational visibility is deferred to v0.3.4.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager pod restart picks up the new SetupMetrics call)
- [ ] Config change only
- [ ] Documentation only

### Verification
After deploying the fix:
```bash
curl -s http://<manager>:8080/metrics | grep '^virtrigaud_build_info'
# expect: virtrigaud_build_info{component="manager",git_sha="<sha>",go_version="go1.25.x",version="v0.3.3-rc3"} 1
```

---

## [2026-05-22 06:38] - Fix virtrigaud_* metrics not exposed on /metrics endpoint
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/obs/metrics/metrics.go`: Bind all 12 virtrigaud Prometheus metric vectors to controller-runtime's `metrics.Registry` (the registry served by the manager on `/metrics`) instead of promauto's default global registry. Previously the metrics were created in-process but never exposed because the manager's `/metrics` endpoint serves controller-runtime's registry, not promauto's default. Use `promauto.With(ctrlmetrics.Registry).New*` for: `virtrigaud_build_info`, `virtrigaud_manager_reconcile_total`, `virtrigaud_manager_reconcile_duration_seconds`, `virtrigaud_queue_depth`, `virtrigaud_vm_operations_total`, `virtrigaud_provider_rpc_requests_total`, `virtrigaud_provider_rpc_latency_seconds`, `virtrigaud_provider_tasks_inflight`, `virtrigaud_errors_total`, `virtrigaud_ip_discovery_duration_seconds`, `virtrigaud_circuit_breaker_state`, `virtrigaud_circuit_breaker_failures_total`.

### Removed
- `internal/obs/metrics/metrics.go`: Removed dead `Init()` function which had a misleading comment claiming promauto registered metrics with the controller-runtime registry; it had zero callers anywhere in the codebase.

### Added
- `internal/obs/metrics/metrics_test.go`: New unit tests `TestMetricsRegisteredInControllerRuntimeRegistry` and `TestSetupMetricsEmitsBuildInfo` asserting all 12 metric families appear in `GetRegistry().Gather()` after one observation each, and that `virtrigaud_build_info` carries the correct label values. These run under `make test` and serve as a regression canary.
- `test/integration/observability_test.go`: Added missing `metrics.SetupMetrics(...)` call at the top of `TestMetricsIntegration` so the test is self-contained rather than relying on side effects from other tests. The integration test now passes when run directly (`go test ./test/integration/...`), though `make test` continues to exclude that path.

### Why
v0.3.3-rc1 smoke test on `vr1.lab.k8` confirmed that scraping `/metrics:8080` returned 41 metric families — all controller-runtime/workqueue/certwatcher/go_/process_ — with zero `virtrigaud_*` series. Root cause: `promauto.NewGaugeVec(...)` etc. bind to `prometheus.DefaultRegisterer`, which the manager does not serve. The bug shipped in v0.3.0 through v0.3.2 (originally introduced in commit `a849271 implement observability extensions`) and would have shipped in v0.3.3 without this fix. Observability is load-bearing for the project's banking-compliance posture; releasing without exposed metrics is a regression on every downstream operator that relies on Prometheus scraping. This change blocks v0.3.3 release until merged (cut v0.3.3-rc2 from this commit).

### Impact
- [ ] Breaking change (no public API change; vector variables are unexported and callers use the helper structs)
- [x] Requires cluster rollout (`virtrigaud_*` metrics begin appearing once the new manager image is deployed)
- [ ] Config change only
- [ ] Documentation only

### Verification
After deploying the fix:
```bash
kubectl -n virtrigaud-system port-forward deploy/virtrigaud-controller-manager 8080:8080 &
curl -s http://localhost:8080/metrics | grep -c '^virtrigaud_'
# expect: 12 or more (one line per metric family + samples)
curl -s http://localhost:8080/metrics | grep '^virtrigaud_build_info'
# expect: virtrigaud_build_info{component="manager",git_sha="...",go_version="go...",version="v0.3.3-rc2"} 1
```

---

## [2026-03-17 22:10] - Add SCSI Controller Configuration for Additional Disks
**Author:** @firestoned (Erick Bourgeois)

### Added
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: New `SCSIControllerSpec` type for SCSI controller configuration
  - `controller`: Specify SCSI bus number (0-3) for the disk
  - `sharedBus`: Configure bus sharing mode (noSharing, virtualSharing, physicalSharing)
  - `controllerType`: Select controller type (lsilogic, buslogic, lsilogic-sas, pvscsi)
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: Added optional `SCSI` field to `DiskSpec`
- `internal/providers/vsphere/server.go`: New `createSCSIController()` function to create SCSI controllers dynamically
- `internal/providers/vsphere/server.go`: Enhanced `attachAdditionalDisk()` to support multiple SCSI controllers
  - Automatically creates new SCSI controllers if specified controller doesn't exist
  - Tracks controllers by bus number for precise disk placement
  - Supports up to 4 SCSI controllers (bus 0-3) with 15 disks each (60 disks total)

### Changed
- `internal/providers/vsphere/server.go`: `AdditionalDiskSpec` now includes SCSI configuration fields
- `internal/providers/vsphere/server.go`: `parseCreateRequest()` parses SCSI configuration from DisksJson
- `internal/providers/vsphere/server.go`: Disk attachment logic refactored to support controller selection

### Why
Users need to attach disks to specific SCSI controllers with custom configurations
(e.g., pvscsi with virtualSharing for RDM disks, or separate controllers for different
disk types). Previously, all disks were attached to the first available SCSI controller
(bus 0), limiting flexibility for advanced storage configurations.

### Usage
```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: multi-controller-vm
spec:
  classRef:
    name: large
  imageRef:
    name: rhel9-template
  providerRef:
    name: vsphere
  disks:
    # Disks on default controller (bus 0)
    - name: data-disk-1
      sizeGiB: 100
      type: thin

    # Disks on dedicated pvscsi controller (bus 1)
    - name: db-disk-1
      sizeGiB: 500
      type: eagerzeroedthick
      scsi:
        controller: 1
        controllerType: pvscsi
        sharedBus: noSharing

    - name: db-disk-2
      sizeGiB: 500
      type: eagerzeroedthick
      scsi:
        controller: 1  # Same controller as db-disk-1
```

This automatically creates SCSI controller 1 (pvscsi, noSharing) if it doesn't exist,
then attaches both db disks to it.

### Impact
- [ ] Breaking change
- [x] New feature - SCSI controller configuration
- [ ] Requires cluster rollout
- [x] CRD update required - includes new `scsi` field in DiskSpec

## [2026-03-17 21:55] - Add 'vm' ShortName Alias for VirtualMachine CRD
**Author:** @firestoned (Erick Bourgeois)

### Added
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: Added `+kubebuilder:resource:shortName=vm` marker
- `config/crd/bases/infra.virtrigaud.io_virtualmachines.yaml`: Generated CRD now includes `vm` as a shortName

### Changed
- VirtualMachine CRD now supports `kubectl get vm` as an alias for `kubectl get virtualmachines`

### Why
Users frequently need to query VirtualMachines and typing `virtualmachines` is verbose. Following
Kubernetes conventions (like `po` for pods, `svc` for services), adding a `vm` shortName improves
user experience and command-line efficiency.

### Usage
After applying the updated CRD:
```bash
kubectl get vm              # Instead of kubectl get virtualmachines
kubectl get vm -A           # List all VMs across namespaces
kubectl describe vm foo     # Describe a specific VM
kubectl delete vm bar       # Delete a VM
```

### Impact
- [ ] Breaking change
- [x] CRD update required - run `kubectl apply -f config/crd/bases/infra.virtrigaud.io_virtualmachines.yaml`
- [ ] Requires cluster rollout
- [ ] Config change only

## [2026-03-17 21:30] - Fix Datastore Name Resolution for Additional Disks
**Author:** @firestoned (Erick Bourgeois)

### Fixed
- `internal/providers/vsphere/server.go`: Additional disk attachment failing with "Invalid configuration for device '0'"
  - Issue: `resolveDatastoreFromStoragePod()` returned `object.NewDatastore()` which doesn't populate the Name property
  - When `datastore.Name()` was called in `attachAdditionalDisk()`, it returned empty string
  - Resulted in invalid disk path: `[] vm-name/disk.vmdk` instead of `[datastore-name] vm-name/disk.vmdk`
  - Fixed by using `p.finder.Datastore(ctx, best.Name)` which properly fetches datastore with all properties

### Why
Additional disks specified in VirtualMachine `spec.disks` were being parsed and attachment was attempted,
but all attachments failed with "Invalid configuration for device '0'" because the datastore path was malformed.
The datastore name was empty in the path string, causing vSphere to reject the disk configuration.

### Impact
- [x] Bug fix - additional disks can now attach successfully
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only

## [2026-03-17 19:00] - Add Comprehensive Disk Debugging and VM Existence Check
**Author:** @firestoned (Erick Bourgeois)

### Added
- `internal/controller/virtualmachine_controller.go`: Verbose-level logging for disk configuration
  - Logs count and details of all additional disks being sent to provider using `log.V(1).Info()`
  - Logs image source resolution details (Libvirt, vSphere, Proxmox) at verbose level
  - Helps identify if disk specs are properly reaching the create request
- `internal/transport/grpc/client.go`: Debug output for DisksJson transmission
  - Prints DisksJson being sent over gRPC to provider
  - Useful for troubleshooting marshaling issues
- `internal/providers/vsphere/server.go`: VM existence check before creation
  - Checks if VM already exists before attempting creation
  - Returns existing VM ID if found, avoiding "name already exists" errors
  - Useful for recovering from failed creation attempts

### Changed
- `internal/providers/vsphere/server.go`: Enhanced debug-level logging throughout disk operations
  - Logs DisksJson receipt in Create method using `p.logger.Debug()`
  - Logs parsed VMSpec with disk count using Debug log level
  - Detailed per-disk logging during attachment with index, name, size, and type
  - Separate logging for success (Debug) vs failure (Error) of each disk attachment
  - Uses proper slog Debug/Error methods
- `internal/controller/virtualmachine_controller.go`: Uses logr verbose logging pattern
  - Converted debug logging from `log.Info("DEBUG ...")` to proper `log.V(1).Info()`
  - Follows controller-runtime's logging conventions for verbose/debug output
  - Verbose logs only appear when log level is set to 1 or higher

### Why
Users were experiencing "name already exists" errors when VM creation failed mid-process,
leaving a partially-created VM that blocked subsequent reconciliation attempts. Additionally,
there was insufficient visibility into whether disk specifications were being properly
transmitted and processed through the controller → gRPC → provider pipeline.

Debug logging was using Info level with "DEBUG" prefix strings, which made logs noisy and
couldn't be controlled by log level settings. Now uses proper debug/verbose logging that
can be enabled/disabled via log level configuration.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Debugging improvements - better error recovery and visibility
- [ ] Config change only

**To enable verbose logging:**
- Controller: Set `--zap-log-level=1` or higher
- Provider: Uses slog Debug level (enabled via LOG_LEVEL=debug environment variable)

---

## [2026-03-17 16:00] - Implement Additional Disks Support in vSphere Provider
**Author:** @firestoned (Erick Bourgeois)

### Added
- `internal/providers/vsphere/server.go`: Complete support for additional disks beyond root disk
  - Added `AdditionalDisks` field to `VMSpec` struct to store additional disk specifications
  - Added `AdditionalDiskSpec` type to define disk name, size, and provisioning type
  - Implemented `attachAdditionalDisk()` helper function to attach disks to existing VMs
  - Added parsing of `DisksJson` from gRPC CreateRequest in `parseCreateRequest()`
  - Integrated disk attachment logic into `createVirtualMachine()` workflow

### Changed
- `internal/providers/vsphere/server.go`: Additional disks now attached after root disk resize, before power-on
  - Automatically finds available SCSI controller slots (unit numbers 0-15, excluding 7)
  - Supports thin, thick, and eager-zeroed-thick provisioning types
  - Creates disk files with naming convention: `vm-name_N.vmdk` (N = disk index)
  - Logs detailed information about each disk attachment operation

### Fixed
- **CRITICAL BUG**: `spec.disks` in VirtualMachine CRD was completely ignored by vSphere provider
  - DisksJson was sent via gRPC but never parsed by the provider
  - Users could specify additional disks in YAML but they were silently not created
  - Now all disks specified in `spec.disks` are properly created and attached

### Why
The VirtualMachine CRD has always supported additional disks via `spec.disks` (up to 20 disks),
but the vSphere provider implementation was incomplete. DisksJson was marshaled and sent by the
controller, transmitted via gRPC, but never parsed or acted upon by the provider. This meant:
- VMs were created with only the root disk (from VMClass.DiskDefaults)
- Additional storage requirements were silently ignored
- No error or warning was generated, making it appear the feature worked

This fix completes the implementation by parsing DisksJson and attaching each specified disk
to the VM using vSphere's Reconfigure API after VM creation.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Feature completion - additional disks now work as documented
- [ ] Config change only

**Example Usage:**
```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: database-server
spec:
  classRef:
    name: standard  # Root disk from class defaults (e.g., 20GB)
  disks:
    - name: data-disk
      sizeGiB: 100
      type: thin
    - name: logs-disk
      sizeGiB: 50
      type: thick
```

Result: VM will have 3 disks (root + 2 additional) attached to SCSI controller.

---

## [2026-01-21 18:00] - Align MetaData Structure with UserData Pattern
**Author:** @firestoned (Erick Bourgeois)

### Changed
- `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go`: Restructured MetaData type to match UserData pattern
  - Changed from `spec.metaData.inline` to `spec.metaData.cloudInit.inline`
  - Added new `CloudInitMetaData` type with `inline` and `secretRef` fields
  - Updated `MetaData` type to nest `CloudInit` configuration
- `internal/controller/virtualmachine_controller.go`: Updated metaData access to use nested structure
- `docs/CLOUD_INIT.md`: Updated all examples to use `metaData.cloudInit.inline` syntax
- `CHANGELOG.md`: Updated example YAML to reflect new structure

### Why
Provides consistent API pattern between userData and metaData fields. Both now follow the same structure:
- `spec.userData.cloudInit.inline`
- `spec.metaData.cloudInit.inline`

This makes the API more intuitive and easier to learn.

### Impact
- [x] Breaking change (requires updating existing VirtualMachine manifests)
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

**Migration:** Users must update their VirtualMachine manifests from:
```yaml
metaData:
  inline: |
    instance-id: vm-001
```
To:
```yaml
metaData:
  cloudInit:
    inline: |
      instance-id: vm-001
```

---

## [2026-01-21 17:30] - Fix Duplicate ProviderReconciler Registration
**Author:** @firestoned (Erick Bourgeois)

### Fixed
- `cmd/main.go`: Removed duplicate ProviderReconciler registration (lines 258-263)
  - ProviderReconciler was registered twice, causing "controller with name provider already exists" error
  - First registration at line 232 is correct and retained
  - Duplicate registration removed to allow manager to start successfully

### Why
The manager was failing to start with error: "controller with name provider already exists. Controller names must be unique to avoid multiple controllers reporting the same metric."

### Impact
- [x] Breaking change (fixes manager startup failure)
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

---

## [2026-01-21] - Add MetaData Support for Cloud-Init Metadata
**Author:** @firestoned (Erick Bourgeois)

### Added
- `VirtualMachine.spec.metaData`: New field for cloud-init metadata configuration
  - Supports inline YAML format (similar to userData)
  - Sent to vSphere via `guestinfo.metadata`
  - Falls back to default `{"instance-id": "<vm-name>"}` if not provided
- `MetaData` type in CRD API with `inline` and `secretRef` support
- `MetaData` support in contracts and controller
- vSphere provider now accepts custom cloud-init metadata
- **Documentation**:
  - New comprehensive guide: [docs/CLOUD_INIT.md](docs/CLOUD_INIT.md)
  - Updated CRD reference: [docs/CRDs.md](docs/CRDs.md)
  - New example: [docs/examples/cloud-init-with-metadata.yaml](docs/examples/cloud-init-with-metadata.yaml)
  - Updated main README with Cloud-Init Configuration link

### Changed
- vSphere `addCloudInitToConfigSpec` now accepts metadata parameter
- Default metadata encoding is JSON, custom metadata uses YAML encoding
- `VMCreateSpec` includes `CloudInitMetaData` field
- CRD documentation now includes `metaData` field specification

### Why
Allows users to customize cloud-init metadata beyond the default instance-id. This is useful for:
- Custom instance configuration
- Network configuration via metadata
- Vendor data and other cloud-init metadata features
- Organizing VMs with custom metadata fields (region, environment, project, etc.)

### Documentation
Complete guide now available at [docs/CLOUD_INIT.md](docs/CLOUD_INIT.md) with:
- Explanation of UserData vs MetaData
- Basic and advanced examples
- Kubernetes Secret integration
- VMware vSphere implementation details
- Best practices and troubleshooting
- Network configuration examples

### Example
```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  metaData:
    cloudInit:
      inline: |
        instance-id: custom-id
        local-hostname: my-custom-hostname
        network:
          version: 2
          ethernets:
            eth0:
              dhcp4: true
  userData:
    cloudInit:
      inline: |
        #cloud-config
        users:
          - name: admin
```

## [0.3.0] [2026-01-10 19:00] - VM Adoption Feature and Infrastructure Improvements
**Author:** @wrkode (William Rizzo)

### Added

#### VM Adoption
- VM adoption controller for discovering and managing existing VMs not created by VirtRigaud
- Annotation-driven adoption via `virtrigaud.io/adopt-vms: "true"`
- Flexible filtering system via `virtrigaud.io/adopt-filter` annotation with support for:
  - Name pattern matching (regex)
  - Power state filtering
  - CPU and memory range filtering
- Automatic VirtualMachine CR creation for discovered unmanaged VMs
- Automatic VMClass generation based on discovered VM properties
- Adoption status tracking in Provider status with discovery time, counts, and error messages
- Provider interface extension with `ListVMs()` method implemented across all providers (vSphere, Libvirt, Proxmox, Mock)
- gRPC API extension with `ListVMs` RPC and VMInfo message types

#### Provider Status Enhancements
- `ConnectedVMs` count field in Provider status showing number of managed VMs
- `Healthy` status field indicating provider availability
- Status fields now properly populate in `kubectl get providers` output

### Fixed

#### GitHub Pages Conflict
- Resolved conflict between documentation and Helm chart hosting
- Documentation now served from `/docs` subdirectory
- Helm charts remain at root for backward compatibility
- Both coexist in `gh-pages` branch without conflicts

### Changed

#### Documentation
- Updated docs workflow to publish to `gh-pages/docs/` directory
- Updated release workflow to preserve docs directory when publishing charts
- Added comprehensive VM Adoption guide (`docs/src/VM_ADOPTION.md`)
- Added VM adoption examples (`docs/src/examples/vm-adoption-example.yaml`)
- Updated installation guides with correct GitHub Pages URLs

#### API Changes
- Added `AdoptionStatus` field to `ProviderStatus` with tracking fields
- New Provider annotations: `virtrigaud.io/adopt-vms` and `virtrigaud.io/adopt-filter`

### Why
- Enables onboarding of existing VMs into VirtRigaud management without manual CR creation
- Provides flexible filtering to adopt only specific VMs based on criteria
- Resolves GitHub Pages hosting conflict allowing both docs and charts to coexist
- Improves Provider status visibility for operational monitoring

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] New feature (opt-in via annotations)

---

## [2025-12-16 22:30] - Add Documentation Workflow and CRD API Reference Generation
**Author:** @ebourgeois (Erick Bourgeois)

### Added
- `.github/workflows/docs.yaml`: GitHub Actions workflow for building and deploying documentation
  - Automatically builds documentation on pushes to `main` and PRs
  - Generates CRD API reference from Go types
  - Installs mdBook and mdbook-mermaid preprocessor
  - Deploys to GitHub Pages on main branch
- `docs/crd-ref-docs-config.yaml`: Configuration for CRD API reference generation
- `Makefile`: Enhanced `docs` target to generate CRD API reference and build mdBook
  - Now generates API reference from `api/infra.virtrigaud.io/v1beta1` types
  - Uses `crd-ref-docs` tool for automatic API documentation
  - `docs-build` is now an alias for the comprehensive `docs` target

### Changed
- Documentation workflow now mirrors bindy's approach with mdBook + API reference generation
- `docs` target is now the primary documentation build command (replaces basic `docs-build`)

### Why
- Automates documentation deployment to GitHub Pages for easier access
- Generates CRD API reference directly from Go types (single source of truth)
- Ensures documentation stays in sync with code changes
- Provides CI/CD validation for documentation builds
- Improves developer experience with automatic API documentation

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

---

## [2025-12-05 15:00] - Standardize Project Name Capitalization
**Author:** @eribourg (Erick Bourgeois)

### Changed
- `docs/`: All occurrences of "Virtrigaud" changed to "VirtRigaud" (correct capitalization)
- Applied to all Markdown and TOML files in documentation

### Why
The project name is "VirtRigaud" with capital V and R. Inconsistent capitalization was causing confusion and looked unprofessional in documentation.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

---

## [2025-12-04 14:50] - Add Makefile Targets for Documentation
**Author:** @ebourgeois (Erick Bourgeois)

### Added
- `Makefile`: Documentation build targets
  - `make docs-build`: Build documentation using mdbook
  - `make docs-serve`: Serve documentation with live reload at http://localhost:3000
  - `make docs-clean`: Clean documentation build artifacts
  - `make docs-watch`: Alias for docs-serve

### Changed
- Makefile now includes `##@ Documentation` section with mdbook integration

### Why
Simplifies documentation workflow by providing standard make targets. Users can easily build, serve, and clean documentation without remembering mdbook commands.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

---

## [2025-12-03 14:45] - Add mdbook Configuration
**Author:** @ebourgeois (Erick Bourgeois)

### Added
- `docs/book.toml`: mdbook configuration file for documentation site
- `docs/src/SUMMARY.md`: Table of contents generated from README.md structure
- `docs/.gitignore`: Ignore mdbook build output directory

### Changed
- Documentation structure now supports mdbook for better browsing and navigation
- All documentation files symlinked into `docs/src/` for mdbook compatibility

### Why
mdbook provides a better reading experience for documentation with navigation, search, and a clean interface. This allows users to browse documentation locally or deploy it as a website.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

## [0.3.0] - 2025-11-16

### Major Release: Cross-Provider VM Migration and Advanced Lifecycle Management

This release introduces VM migration capabilities, multi-VM management, and advanced placement policies. VirtRigaud v0.3.0 enables VM migrations between different hypervisor platforms (currently tested: vSphere to Libvirt/KVM) and provides enterprise-grade VM lifecycle management features.

### Added

#### VM Migration (VMMigration CRD)
- **Cross-Provider Migration**: VM migration support between different hypervisor providers (currently tested: vSphere to Libvirt/KVM)
- **PVC-Based Storage**: Kubernetes PersistentVolumeClaims (PVCs) as intermediate storage for migration transfers
- **Automatic Storage Management**: Auto-creation and cleanup of migration PVCs with ReadWriteMany access mode
- **Format Conversion**: Automatic disk format conversion (qcow2, VMDK, raw) during migration
- **Progress Tracking**: Real-time migration progress with phase tracking and percentage completion
- **Checksum Validation**: SHA256 checksum verification for data integrity
- **Migration Phases**: Comprehensive phase tracking (Pending, Validating, Snapshotting, Exporting, Transferring, Converting, Importing, Creating, Validating-Target, Ready, Failed)
- **Retry Policies**: Configurable retry behavior with exponential backoff
- **Validation Checks**: Optional validation checks for disk size, checksum, boot success, and connectivity
- **Snapshot Integration**: Automatic snapshot creation before migration with optional snapshot references
- **Provider Restart Management**: Automatic provider pod restart to mount migration PVCs with graceful shutdown
- **Storage URL Format**: PVC-based storage URLs for provider communication
- **Migration Metadata**: Purpose tracking, project identification, and environment tagging
- **Cleanup Policies**: Configurable cleanup behavior (Always, OnSuccess, Never)
- **Documentation**: Complete migration guide with examples and troubleshooting

#### Multi-VM Management (VMSet CRD)
- **Replica Management**: Declarative management of multiple VM instances with replica count
- **Rolling Updates**: Rolling update strategy for VM sets with configurable max surge and max unavailable
- **OnDelete Strategy**: OnDelete update strategy for manual control
- **Recreate Strategy**: Complete replacement strategy for major updates
- **MinReadySeconds**: Configurable minimum ready time before considering VM ready
- **Revision History**: Configurable retention of old VMSet revisions
- **PVC Retention**: PersistentVolumeClaim retention policies for VM disks
- **Ordinal Management**: Sequential ordering of VM indices with configurable start offset
- **Service Integration**: Service name reference for VM set management
- **Volume Claim Templates**: Template-based PVC creation for VM sets
- **Label Selectors**: Label-based VM selection and matching
- **Status Tracking**: Comprehensive status tracking with ready replicas, updated replicas, and conditions

#### Advanced Placement Policies (VMPlacementPolicy CRD)
- **Hard Constraints**: Mandatory placement constraints for clusters, datastores, hosts, folders, resource pools, networks, zones, and regions
- **Soft Constraints**: Preferred placement constraints with weight-based scoring
- **Anti-Affinity Rules**: VM anti-affinity rules to prevent co-location
- **Affinity Rules**: VM affinity rules to encourage co-location
- **Resource Constraints**: Resource-based placement constraints (CPU, memory, storage)
- **Security Constraints**: Security-based placement constraints for compliance
- **Priority and Weight**: Configurable priority and weight for policy evaluation
- **Label Selectors**: Label-based policy matching for VMs
- **Topology Spread**: Topology spread constraints for distribution across zones
- **Placement Scoring**: Weighted scoring system for placement decisions
- **Policy References**: VM-level placement policy references via PlacementRef

#### ImportedDisk Support
- **ImportedDisk Field**: New VirtualMachine spec field for referencing pre-imported disks from migrations
- **Migration References**: Automatic migration reference tracking in imported disks
- **Disk Metadata**: Format, source, and size metadata for imported disks
- **Type Safety**: Type-safe validation for imported disk references
- **Separation of Concerns**: Clear separation between template-based and disk-based VM creation

#### Provider Enhancements

**vSphere Provider:**
- **Migration Export**: VMDK export support for migration operations
- **Migration Import**: VMDK import support with format conversion
- **Disk Path Fixes**: Corrected disk path handling for migration storage
- **Enhanced Error Handling**: Improved error messages for migration operations

**Libvirt Provider:**
- **Migration Export**: qcow2 export support for migration operations
- **Migration Import**: qcow2 import support with in-place detection
- **Disk Path Fixes**: Fixed disk copy path to use pool directory instead of /tmp
- **In-Place Detection**: Intelligent detection of disks already in pool directory
- **SCP Transfer**: Secure copy protocol support for disk transfers
- **Format Conversion**: qemu-img-based format conversion support

**Proxmox Provider:**
- **Migration Export**: Disk export support for migration operations
- **Migration Import**: Disk import support with format conversion
- **Storage Integration**: Enhanced storage pool integration for migrations

#### Controller Enhancements
- **Migration Controller**: Complete VMMigration controller implementation
- **VMSet Controller**: VMSet controller for multi-VM management
- **Placement Policy Controller**: VMPlacementPolicy controller for advanced placement
- **Provider Restart Coordination**: Automatic provider pod restart coordination for PVC mounting
- **PVC Management**: Automatic PVC creation, mounting, and cleanup
- **Status Reconciliation**: Enhanced status reconciliation for all CRDs
- **Event Recording**: Comprehensive event recording for all operations

#### Storage Layer
- **PVC Storage Backend**: PVC-based storage backend for migrations
- **Storage URL Parsing**: PVC URL parsing and path resolution
- **Mount Path Management**: Automatic mount path management for provider pods
- **Storage Discovery**: Automatic discovery of migration PVCs
- **Volume Mount Management**: Dynamic volume mount management for providers

### Fixed

#### Migration Fixes
- **Disk Path Issue**: Fixed disk copy path in Libvirt provider to use pool directory instead of /tmp
- **In-Place Detection**: Fixed in-place disk detection logic to correctly identify migrated disks
- **VM Creation**: Fixed VM creation to use imported disks instead of creating fresh template copies
- **Data Preservation**: Ensured migrated VM data is preserved during migration
- **PVC Mount Path**: Fixed PVC mount path resolution for provider pods
- **Provider Restart**: Fixed provider restart timing and coordination
- **Storage URL Format**: Corrected storage URL format to include PVC name

#### Provider Fixes
- **Libvirt Disk Import**: Fixed disk import to correctly handle in-place detection
- **vSphere Export**: Fixed VMDK export path handling
- **Proxmox Import**: Fixed disk import path resolution
- **Connection Management**: Improved gRPC connection management during provider restarts
- **Error Handling**: Enhanced error handling for migration operations

#### Controller Fixes
- **PVC Creation**: Fixed PVC creation timing and error handling
- **Provider Reconciliation**: Fixed provider reconciliation trigger mechanism
- **Status Updates**: Fixed status update timing and consistency
- **Event Recording**: Fixed event recording for migration operations

### Enhanced

#### Documentation
- **Migration Guide**: Complete VM migration guide with examples and troubleshooting
- **Migration Architecture**: Detailed migration storage architecture documentation
- **VMSet Documentation**: VMSet usage and examples documentation
- **Placement Policy Guide**: VMPlacementPolicy configuration guide
- **Advanced Lifecycle**: Enhanced advanced lifecycle management documentation
- **API Reference**: Updated API reference with new CRDs

#### Examples
- **Migration Examples**: Complete migration examples for all provider combinations
- **VMSet Examples**: VMSet examples with rolling updates
- **Placement Policy Examples**: VMPlacementPolicy configuration examples
- **Multi-Provider Examples**: Enhanced multi-provider examples

### Technical Details

#### New CRDs
- **VMMigration**: Cross-provider VM migration resource
- **VMSet**: Multi-VM management resource
- **VMPlacementPolicy**: Advanced placement policy resource

#### API Changes
- **VirtualMachine**: Added ImportedDisk field and PlacementRef field
- **Backward Compatibility**: Maintains full backward compatibility with v0.2.x
- **CRD Schemas**: Enhanced CRD schemas with comprehensive validation

#### Storage Architecture
- **PVC-Based Storage**: Per-migration PVC approach with automatic provider restart
- **ReadWriteMany Access**: Requires StorageClass with ReadWriteMany access mode
- **Automatic Cleanup**: Automatic PVC cleanup on migration completion or deletion
- **Provider Restart**: Brief (5-15 second) provider pod restart for PVC mounting

### Deployment Notes

#### Container Images
Updated provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.3.0`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.3.0`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.3.0`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.3.0`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.3.0`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.3.0`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.3.0`

#### Prerequisites
- **StorageClass**: Requires StorageClass with ReadWriteMany access mode for migrations
- **Kubernetes**: Kubernetes 1.24+ recommended
- **Provider Versions**: All providers updated to v0.3.0

#### Upgrade Path
- Direct upgrade from v0.2.x with no manual intervention required
- Existing deployments will automatically benefit from new features
- Migration features require StorageClass with ReadWriteMany access mode
- No configuration changes needed for standard deployments

### Known Limitations

#### Migration
- **Testing Status**: Currently only tested from vSphere to Libvirt/KVM. Other provider combinations (Libvirt to vSphere, Proxmox migrations, etc.) are not yet fully tested
- **Provider Restart**: Brief (5-15 second) provider pod restart required for PVC mounting
- **Concurrent Migrations**: Multiple migrations may cause multiple provider restarts
- **Storage Requirements**: Requires StorageClass with ReadWriteMany access mode
- **Network Bandwidth**: Migration speed depends on network bandwidth and storage performance

#### VMSet
- **Rolling Updates**: Rolling updates may cause brief VM unavailability during updates
- **Replica Limits**: Maximum 1000 replicas per VMSet

#### Placement Policies
- **Provider Support**: Placement policies require provider-specific implementation
- **Policy Evaluation**: Policy evaluation occurs during VM creation only


---

## [0.2.3] - 2025-10-13

### Added

#### vSphere Provider
- **VM Reconfiguration Support**: Complete implementation of dynamic VM reconfiguration
  - Online CPU adjustment for running VMs (hot-add when supported by guest OS)
  - Online memory adjustment for running VMs (hot-add when supported)
  - Disk resizing with safety checks to prevent data loss (shrinking prevented)
  - Intelligent change detection to avoid unnecessary reconfigurations
  - Memory parsing support for multiple units (Mi, Gi, MiB, GiB)
  - Automatic fallback to offline changes when online modification is not supported
- **Asynchronous Task Tracking**: Full TaskStatus RPC implementation
  - Real-time monitoring of vSphere async operations via govmomi
  - Task state reporting (queued, running, success, error)
  - Error information extraction from failed tasks
  - Progress tracking with percentage completion
  - Integration with vSphere task manager for reliable operation status
- **VM Cloning Operations**: Complete clone functionality
  - Full clone support for independent VM copies
  - Linked clone support for space-efficient template-based deployments
  - Automatic snapshot creation for linked clones when no snapshot exists
  - Proper disk relocation and storage configuration
  - Clone naming and folder placement control
- **Console URL Generation**: Web-based VM console access
  - Automatic vSphere web client console URL generation
  - Direct browser-based VM console access via vCenter
  - URL includes VM instance UUID for reliable identification
  - Integration with vCenter endpoint for proper routing

#### Libvirt Provider
- **VM Reconfiguration Support**: Complete virsh-based reconfiguration
  - Online CPU adjustment via `virsh setvcpus --live` for running VMs
  - Online memory adjustment via `virsh setmem --live` for running VMs
  - Offline configuration updates for stopped VMs via `virsh setvcpus/setmem --config`
  - Disk volume resizing via storage provider integration
  - Automatic VM info parsing to extract current CPU and memory settings
  - Graceful handling of operations requiring VM restart
  - Memory unit conversion and validation (bytes, KiB, MiB, GiB)
- **VNC Console URL Generation**: Remote console access support
  - Automatic VNC port extraction from domain XML configuration
  - VNC console URL generation for direct viewer connections
  - Support for standard VNC clients and web-based VNC viewers
  - Integration with libvirt graphics configuration

#### Proxmox Provider
- **Guest Agent IP Detection**: Enhanced network information retrieval
  - QEMU guest agent integration for accurate IP address detection
  - Extraction of all network interfaces from running VMs
  - Automatic filtering of loopback and link-local addresses
  - Support for both IPv4 and IPv6 address reporting
  - Real-time IP information when guest agent is installed and running
- **Template Cloning Support**: Full VM cloning from Proxmox templates
  - Automatic template detection from VMImage CRD `templateID` or `templateName`
  - Full clone and linked clone support via `fullClone` parameter
  - Proper storage pool selection during clone operation
  - Clone task monitoring with async operation support
- **Intelligent Boot Order Configuration**:
  - Auto-detection of primary boot disk from cloned template
  - Support for multiple disk types: scsi, virtio, sata, ide
  - Automatic boot order generation (e.g., `boot=order=scsi0;ide2`)
  - Exclusion of CD-ROM devices from boot order
- **Cloud-Init Integration**:
  - Complete cloud-init configuration via `ide2:cloudinit` device
  - SSH key injection with proper URL encoding
  - User creation and authentication setup
  - Network configuration (static IP and DHCP)
  - Package installation and custom commands via runcmd
- **Complete CRD Support**:
  - ProxmoxImageSource integration for template-based deployments
  - ProxmoxNetworkConfig for bridge and VLAN configuration
  - Controller parsing of Proxmox-specific fields
  - Provider RPC implementation for all VM operations
- **Production Features**:
  - VM creation, power management, deletion
  - Status reporting with IP address detection
  - Task-based async operations with progress tracking
  - Comprehensive error handling and logging

### Fixed

#### vSphere Provider

**Reconfigure Type Mismatch**:
- **Root Issue**: Memory comparison in Reconfigure function caused compilation error due to type mismatch between int64 and int32
- **Cause**: govmomi's `VirtualMachineConfigInfo.Hardware.MemoryMB` field is int32, but comparison was using int64
- **Fix**: Added explicit type casting `int64(vmMo.Config.Hardware.MemoryMB)` to ensure type compatibility
- **Impact**: Reconfigure operations now compile and execute correctly without type errors

#### Libvirt Provider

**Missing Standard Library Imports**:
- **Root Issue**: Reconfigure and Describe functions referenced undefined packages causing compilation failures
- **Cause**: Implementation added `strconv` usage for integer conversion and `net/url` for URL parsing without importing packages
- **Fix**: Added missing imports:
  - `import "strconv"` for string-to-integer conversions in CPU/memory parsing
  - `import "net/url"` for VNC console URL construction
- **Impact**: Provider now compiles successfully and all reconfiguration and console URL features work correctly

#### Proxmox Provider

**SSH Keys Encoding**:
- **Root Issue**: Proxmox API's `sshkeys` parameter requires **double URL encoding** due to its internal decoding behavior
**Template Cloning and Boot Order**:
- **Root Issue**: Provider was creating new VMs instead of cloning from templates, causing boot failures
- **Fix**: Implemented proper template detection and cloning workflow:
  1. Parse `TemplateName` from VMImage CRD (via controller)
  2. Call CloneVM API with template ID and storage configuration
  3. Wait for clone task completion
  4. Detect primary boot disk from cloned VM config
  5. Reconfigure VM with cloud-init and correct boot order
- **Boot Order Detection**: Added intelligent disk detection that:
  - Scans VM config for disk attachments (virtio, scsi, sata, ide)
  - Filters out CD-ROM devices
  - Prioritizes disk types (virtio > scsi > sata > ide)
  - Constructs proper boot parameter (e.g., `boot=order=scsi0;ide2`)


**Controller Image Source Parsing**:
- **Root Issue**: Controller was sending empty `TemplateName` to provider, causing fallback to VM creation
- **Cause**: JSON field name mismatch - controller uses `TemplateName` (capital T) but provider expected `template_name` (snake_case)
- **Fix**: Updated provider to parse `TemplateName` field correctly from contracts.VMImage JSON
- **Debug Process**: Added comprehensive debug logging in both controller and provider to trace data flow
- **Impact**: Template ID from VMImage CRD is now correctly transmitted to Proxmox provider

#### Release Workflow

**Helm Chart Image Tag Updates**:
- **Root Issue**: Manager and provider images were not being updated during Helm releases, staying pinned to v0.2.0
- **Cause**: sed patterns in release workflow used incorrect range expressions that stopped before reaching the `tag:` lines
  - Manager pattern: `/^manager:/,/^  image:/` stopped at line 16, but `tag:` is on line 18
  - Provider patterns: Nested ranges didn't reach `tag:` lines which appear after provider names
- **Fix**: Replaced fragile regex ranges with precise line-number-based patterns:
  - Manager: `MANAGER_START` to `MANAGER_START+10`
  - LibVirt: `LIBVIRT_START` to `LIBVIRT_START+15`
  - vSphere: `VSPHERE_START` to `VSPHERE_START+15`
  - Proxmox: `PROXMOX_START` to `PROXMOX_START+15`
- **Impact**: All component images (manager, providers, kubectl) now update correctly to match release version


## [0.2.2] - 2025-10-13

### Added (Continued)

#### Nested Virtualization Support
- **VMClass PerformanceProfile**: Added `nestedVirtualization` field to enable nested virtualization capabilities in VMs, allowing VMs to run their own hypervisors and nested virtual machines
- **vSphere Provider Implementation**:
  - Automatically configures `vhv.enable=TRUE` for hardware-assisted virtualization
  - Enables `vhv.allowNestedPageTables=TRUE` for improved nested VM performance
  - Compatible with VM hardware version 9+ (version 14+ recommended)
- **LibVirt Provider Implementation**:
  - Configures CPU mode with required virtualization extensions (vmx for Intel VT-x, svm for AMD-V)
  - Automatically passes through host CPU virtualization features to guest VMs
  - Compatible with QEMU/KVM hypervisors with nested virtualization enabled
- **VT-d/AMD-Vi Support**: Added `vtdEnabled` field in SecurityProfile for Intel VT-d or AMD IOMMU support, improving I/O performance for nested environments
- **CPU/Memory Hot-Add**: Added `cpuHotAddEnabled` and `memoryHotAddEnabled` in PerformanceProfile for dynamic resource scaling without VM restart
- **Virtualization Based Security**: Added `virtualizationBasedSecurity` field in PerformanceProfile for Windows VBS features

#### Security Features
- **TPM (Trusted Platform Module) Support**:
  - Added `tpmEnabled` and `tpmVersion` fields in VMClass SecurityProfile
  - vSphere Provider: Full TPM 2.0 device support (requires vSphere 6.7+ and VM hardware version 14+)
  - LibVirt Provider: TPM emulator support with tpm-tis model and version 2.0
  - Automatically enforces UEFI firmware requirement when TPM is enabled
  - Enables Windows 11 support and BitLocker encryption capabilities
- **Secure Boot Support**:
  - Added `secureBoot` field in SecurityProfile for UEFI Secure Boot functionality
  - vSphere Provider: Configures EFI Secure Boot through VM boot options
  - LibVirt Provider: Uses OVMF firmware with Secure Boot capabilities
  - Automatically forces UEFI firmware when enabled
  - Protects against rootkits and bootkits at firmware level
- **Comprehensive Documentation**:
  - Added `docs/NESTED_VIRTUALIZATION.md` with detailed configuration guide
  - Added `docs/examples/nested-virtualization.yaml` with complete working examples
  - Includes verification steps, troubleshooting guidance, and performance recommendations

#### Use Cases Enabled
- Development and testing of virtualization platforms (e.g., Proxmox, OpenStack, vSphere)
- Running Kubernetes clusters with nested container runtimes
- Creating isolated lab environments for security testing
- Educational scenarios for learning virtualization technologies

#### VM Snapshot Management
- **Complete VMSnapshot CRD**: Full-featured API for VM snapshot lifecycle management
  - Snapshot creation with memory state and filesystem quiescing options
  - Snapshot deletion with proper cleanup
  - Snapshot revert for rollback scenarios
  - Retention policies (maxAge, deleteOnVMDelete, maxCount)
  - Automated scheduling support via cron expressions
  - Snapshot metadata and tagging
- **vSphere Provider Implementation**:
  - Full govmomi-based snapshot operations (Create, Delete, Revert)
  - Memory snapshot support for powered-on VMs
  - Filesystem quiescing with VMware Tools integration
  - Automatic power state handling during revert
  - Hierarchical snapshot tree navigation
  - Synchronous operations for immediate completion
- **LibVirt Provider Implementation**:
  - Full virsh-based snapshot operations (Create, Delete, Revert)
  - Memory snapshot support for running VMs with qcow2 storage
  - Disk-only snapshots for VMs with incompatible storage backends
  - Atomic snapshot creation with --atomic flag
  - Automatic power state preservation during revert
  - Snapshot existence validation before operations
  - Synchronous operations with immediate feedback
  - Snapshot name sanitization for virsh compatibility
  - Helper methods for snapshot listing and querying
- **Proxmox Provider Implementation**:
  - Complete snapshot lifecycle support
  - Memory state inclusion (vmstate)
  - Async task handling with status tracking
  - Full VM creation from templates with cloud-init
  - Intelligent boot order configuration
  - SSH key injection with proper encoding
  - Network configuration with bridge support
  - Storage pool management
- **Controller Integration**:
  - Real provider RPC calls (no more simulation)
  - Proper task status polling for async operations
  - Comprehensive error handling and reporting
  - Event recording for observability
  - Finalizer-based cleanup
- **Transport Layer**:
  - Added snapshot methods to gRPC client
  - TaskStatus RPC for async operation tracking
  - Proper request/response type mapping
- **Use Cases**:
  - Pre-maintenance backups with quick rollback
  - CI/CD testing with snapshot-based environments
  - Disaster recovery and point-in-time restore
  - Development environment versioning

### Fixed

#### vSphere Provider
- **Placement Override Bug**: Fixed critical bug where VirtualMachine `spec.placement.folder`, `spec.placement.datastore`, and `spec.placement.cluster` overrides were not being respected by the vSphere provider. The provider was always using the default values from the Provider CRD instead of honoring the per-VM placement overrides specified in the VirtualMachine manifest. VMs are now correctly created in the specified folder, datastore, and cluster when placement overrides are provided.

## [0.2.1] - 2025-09-29

### Patch Release: Critical Fixes and Documentation Updates

This patch release addresses several critical issues discovered in v0.2.0, including linter compliance fixes, documentation improvements, and enhanced provider capabilities. VirtRigaud v0.2.1 ensures improved stability and usability for production deployments.

### Fixed

#### Code Quality and Compliance
- **Error Handling**: Fixed unchecked error return values in vSphere provider fmt.Sscanf calls
- **Linting Compliance**: Resolved golangci-lint errcheck violations that were causing CI failures
- **CRD Validation**: Fixed CRD validation conflicts for OffGraceful powerState transitions

#### Documentation and Examples
- **Broken Links**: Corrected broken documentation links in README.md
- **Example Updates**: Consolidated and enhanced examples with v0.2.1 features
- **CLI Documentation**: Added comprehensive CLI documentation and reference guides

#### Provider Enhancements
- **VMClass Disk Settings**: Fixed VMClass disk size settings to be properly respected across all providers
- **CRD Schema Sync**: Synchronized Helm chart CRDs with latest schema fixes for consistency

### Added

#### Infrastructure Improvements
- **Build Artifacts**: Enhanced .gitignore to properly exclude dist/ and build artifacts
- **Automated CRD Sync**: Implemented automated CRD synchronization workflow for improved consistency
- **Field Test Exclusions**: Added fieldTest exclusions to .gitignore for cleaner repository

#### vSphere Provider Features
- **Hardware Version Management**: Added VM hardware version management support with version comparison logic
- **Graceful Shutdown**: Implemented graceful shutdown capabilities for virtual machines
- **Enhanced Power States**: Improved power state management with better error handling

### Enhanced

#### Documentation
- **README Updates**: Comprehensive updates to project README with corrected examples and links
- **CLI Reference**: Complete CLI documentation covering all available commands and options
- **Provider Guides**: Enhanced provider-specific documentation with updated examples

#### Development Workflow
- **Release Preparation**: Streamlined release preparation process with automated documentation sync
- **CI/CD Pipeline**: Improved continuous integration with better linting and validation checks

### Technical Details

#### API Stability
- Maintains full backward compatibility with v0.2.0
- No breaking changes to existing APIs or configurations
- CRD schemas remain stable with validation improvements

#### Provider Compatibility
- All existing provider configurations continue to work without modification
- Enhanced error handling improves provider reliability
- VMClass configurations now properly enforce disk size settings

### Deployment Notes

#### Container Images
Updated provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.2.1`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.1`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.2.1`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.1`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.2.1`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.2.1`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.2.1`

#### Upgrade Path
- Direct upgrade from v0.2.0 with no manual intervention required
- Existing deployments will automatically benefit from the fixes
- No configuration changes needed for standard deployments

### Acknowledgments

This release includes important fixes identified by the community and addresses issues reported in production environments. Thanks to all contributors who helped identify and resolve these issues.

---

## [0.2.0] - 2025-09-15

### Major Release: Production-Ready Provider Architecture

This release marks a significant milestone for VirtRigaud with production-ready vSphere and LibVirt providers, comprehensive documentation, and a complete CLI toolset. VirtRigaud v0.2.0 delivers enterprise-grade virtual machine management across multiple hypervisor platforms.

### Added

#### Core Features
- **Remote Provider Architecture**: Complete implementation of the remote provider model with gRPC communication
- **Production-Ready vSphere Provider**: Full VMware vSphere integration with enterprise features
- **Production-Ready LibVirt Provider**: Comprehensive KVM/QEMU support via virsh-based implementation
- **Advanced Storage Management**: Storage pools, volume operations, and cloud image handling
- **Enhanced Cloud-Init Support**: NoCloud datasource implementation with ISO generation
- **QEMU Guest Agent Integration**: Enhanced guest OS monitoring and communication

#### CLI Tools Suite
- **vrtg**: Complete virtual machine management CLI with resource operations
- **vcts**: Conformance testing suite for provider validation
- **vrtg-provider**: Provider development toolkit for scaffolding and code generation
- **virtrigaud-loadgen**: Load testing and performance benchmarking tool

#### Provider Capabilities

**vSphere Provider:**
- VM creation from templates, OVA/OVF files, and content libraries
- Power management with suspend/resume support
- Advanced networking with distributed switches and port groups
- Snapshot management with memory state preservation
- Template and content library integration
- High availability and DRS configuration support
- Storage policy management and vSAN integration
- Comprehensive error handling and async task monitoring

**LibVirt Provider:**
- VM creation from cloud images with automatic download
- Storage pool and volume management with multiple backends
- Network configuration with bridges and virtual networks
- Cloud-init integration via NoCloud ISO generation
- QEMU Guest Agent support for enhanced monitoring
- Snapshot operations with storage-dependent features
- Resource configuration and management
- Performance optimization with virtio drivers

#### Documentation
- **Comprehensive Provider Documentation**: Detailed guides for each supported provider
- **CLI Reference Manual**: Complete documentation for all command-line tools
- **Provider Capabilities Matrix**: Feature comparison and implementation status
- **Architecture Documentation**: Remote provider design and configuration flows
- **Examples and Tutorials**: Real-world configuration examples and best practices

### Enhanced

#### Core Improvements
- **Provider Registry**: Centralized provider discovery and capability reporting
- **Error Handling**: Improved error classification and retry logic
- **Resource Management**: Enhanced VM lifecycle management with proper cleanup
- **Network Configuration**: Advanced networking features across all providers
- **Monitoring Integration**: Comprehensive metrics and observability features

#### CI/CD Pipeline
- **Automated Testing**: Enhanced test coverage with conformance testing
- **Release Automation**: Streamlined build and release processes
- **Documentation Generation**: Automated API reference and capability documentation
- **Quality Assurance**: Comprehensive linting and static analysis

### Fixed

#### Stability and Reliability
- **Connection Management**: Robust connection handling with automatic retry
- **Resource Cleanup**: Proper cleanup of VM resources and associated storage
- **Memory Management**: Improved memory usage in provider implementations
- **Concurrent Operations**: Thread-safe operations and proper synchronization
- **Error Recovery**: Enhanced error recovery and graceful degradation

#### Provider-Specific Fixes
- **vSphere**: Resolved template deployment and network configuration issues
- **LibVirt**: Fixed storage pool management and cloud-init generation
- **Cross-Platform**: Improved compatibility across different hypervisor versions

### Technical Details

#### API Changes
- **Stable v1beta1 API**: Production-ready API with comprehensive resource definitions
- **Provider Contract**: Standardized provider interface with capability discovery
- **Resource Schemas**: Enhanced CRD schemas with validation and defaults
- **Backward Compatibility**: Seamless upgrade path from previous versions

#### Performance Improvements
- **Async Operations**: Non-blocking VM operations with progress tracking
- **Connection Pooling**: Efficient resource utilization in provider connections
- **Caching**: Intelligent caching of templates, images, and metadata
- **Batch Operations**: Support for bulk VM operations where applicable

#### Security Enhancements
- **Credential Management**: Secure handling of hypervisor credentials
- **Network Isolation**: Provider network isolation with configurable policies
- **RBAC Integration**: Fine-grained role-based access control
- **Audit Logging**: Comprehensive audit trail for all operations

### Provider Feature Matrix

| Feature | vSphere | LibVirt | Status |
|---------|---------|---------|---------|
| VM Lifecycle | Complete | Complete | Production |
| Power Management | Complete | Complete | Production |
| Storage Management | Complete | Complete | Production |
| Network Configuration | Complete | Complete | Production |
| Snapshot Operations | Complete | Storage-dependent | Production |
| Template Management | Complete | Cloud Images | Production |
| Guest Integration | VMware Tools | QEMU Guest Agent | Production |
| High Availability | Complete | Planned | vSphere Only |
| Live Migration | Complete | Planned | vSphere Only |
| Hot Reconfiguration | Complete | Restart Required | Mixed |

### Deployment and Operations

#### Container Images
All provider images are available from GitHub Container Registry:
- **Manager**: `ghcr.io/projectbeskar/virtrigaud/manager:v0.2.0`
- **vSphere Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-vsphere:v0.2.0`
- **LibVirt Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-libvirt:v0.2.0`
- **Proxmox Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-proxmox:v0.2.0`
- **Mock Provider**: `ghcr.io/projectbeskar/virtrigaud/provider-mock:v0.2.0`

#### Helm Charts
- **Main Chart**: `virtrigaud/virtrigaud:0.2.0`
- **Provider Runtime Chart**: `virtrigaud/virtrigaud-provider-runtime:0.2.0`

#### Installation Methods
- Helm charts with comprehensive configuration options
- Kustomize overlays for different deployment scenarios
- Direct YAML manifests for custom deployments
- CLI-based installation and management

### Upgrade Notes

#### Breaking Changes
- None. This release maintains full backward compatibility with v0.1.x deployments.

#### Migration Guide
- Existing v0.1.x deployments can be upgraded in-place
- Provider configurations are automatically migrated
- No manual intervention required for standard deployments

#### Deprecations
- None in this release. All APIs remain stable and supported.

### Known Issues

#### Current Limitations
- **LibVirt Hot Reconfiguration**: CPU and memory changes require VM restart
- **LibVirt Memory Snapshots**: Not supported on all storage backends
- **Cross-Provider Migration**: Not yet implemented between different provider types

#### Workarounds
- Detailed workarounds are documented in the provider-specific guides
- Community support available for deployment-specific issues

### Acknowledgments

This release includes contributions from the VirtRigaud development team and community feedback from early adopters. Special thanks to all contributors who helped shape this production-ready release.

### What's Next

#### Roadmap for v0.3.0
- **Enhanced LibVirt Features**: Live migration and hot reconfiguration support
- **Proxmox VE Provider**: Production-ready Proxmox integration
- **Multi-Cloud Providers**: AWS EC2, Azure, and GCP provider implementations
- **Advanced Networking**: Service mesh integration and network policies
- **Backup and Recovery**: Integrated backup solutions and disaster recovery

#### Community
- Join our community discussions on GitHub
- Contribute to provider development and documentation
- Report issues and feature requests through GitHub Issues

For detailed upgrade instructions and deployment guides, see the [Installation Documentation](docs/install-helm-only.md).

For provider-specific configuration and capabilities, see the [Provider Documentation](docs/providers/).

---

**Full Changelog**: https://github.com/projectbeskar/virtrigaud/compare/v0.2.0...v0.2.1
#### Proxmox Provider CRD Integration (Completed)

The Proxmox provider now has full CRD integration for template-based VM deployment:

**VMImage CRD - ProxmoxImageSource**:
- Template ID or template name references
- Storage pool selection for cloned VMs
- Node specification for template location
- Full clone vs linked clone selection
- Disk format configuration (qcow2, raw, vmdk)

**VMNetworkAttachment CRD - ProxmoxNetworkConfig**:
- Linux bridge selection (vmbr0, vmbr1, etc.)
- Network card model selection (virtio, e1000, rtl8139, vmxnet3)
- VLAN tagging support
- Proxmox firewall integration
- Bandwidth rate limiting
- MTU configuration

**Controller Integration**:
- Full parsing of Proxmox-specific fields from CRDs
- Conversion to provider contracts and gRPC messages
- Proper JSON field mapping (TemplateName, Bridge, Model, etc.)

**Documentation**:
- Complete provider documentation in `docs/providers/PROXMOX.md`
- Working examples in `examples/proxmox/`
- Troubleshooting guides for common issues

**Impact**: The Proxmox provider now has feature parity with vSphere and LibVirt providers, enabling production-ready VM management on Proxmox VE clusters via Kubernetes CRDs.
