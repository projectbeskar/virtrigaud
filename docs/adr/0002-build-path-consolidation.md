# ADR-0002: Consolidate the two parallel manager build paths

> **Location**: `docs/adr/0002-build-path-consolidation.md`
> Promoted from `fieldTesting/` on 2026-05-27.
> **H1 PRs 1–4 shipped in v0.3.6. PR-5 (HTTPS-by-default metrics) is deferred to v0.4.0.**

## Status

**Accepted** — 2026-05-24 (promoted 2026-05-27)

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Related issue**: [#92](https://github.com/projectbeskar/virtrigaud/issues/92)

## Context

VirtRigaud's manager binary had **two coexisting build paths** that compiled slightly different programs from slightly different Dockerfiles. Both paths were exercised by current Makefile targets; neither path was dead.

### The two paths (pre-v0.3.6)

| Aspect | Canonical (release) | Local-dev |
|---|---|---|
| Entrypoint | `cmd/manager/main.go` | `cmd/main.go` |
| Dockerfile | `build/Dockerfile.manager` | `Dockerfile` (repo root) |
| Invoked by | Release workflow | `make docker-build`, `make build`, `make run` |
| Built into | `ghcr.io/projectbeskar/virtrigaud/manager:<tag>` | Local dev images |

### What actually broke (v0.3.3-rc4 incident)

During the v0.3.3-rc4 hotfix, `virtrigaud_build_info{version="dev"}` appeared on `/metrics`. Diagnosis cost ~30 minutes because two Dockerfiles were visible in the tree. The first instinct was `rm cmd/main.go Dockerfile` — which would have **broken `make docker-build` and `make docker-buildx`**, two daily-driver developer commands.

### Functional gaps (audit as of 2026-05-24)

**Local-dev path lacked** (vs. canonical):
- No `metrics.SetupMetrics()` call — local-dev binary did not emit `virtrigaud_build_info`.
- VMSnapshot controller not registered.
- VMMigration controller not registered.
- No client-side QPS/burst tuning.

**Canonical path lacked** (vs. local-dev):
- `--version` CLI flag.
- `certwatcher` integration for hot cert rotation.
- `metrics/filters.WithAuthenticationAndAuthorization`.
- `ARG BUILDER_IMAGE` / `ARG BASE_IMAGE` / `GOPROXY` / CA-cert passthrough in Dockerfile (required for corporate/banking environments that cannot pull from `docker.io` directly).

## Decision

**Option A — Consolidate to the canonical path, port the missing features, retire the local-dev orphan.**

The release flow was already established on `cmd/manager/main.go` + `build/Dockerfile.manager`. The local-dev path was the one to retire — after the small handful of genuinely useful features it carried had been ported across.

## Roadmap (PR-sized chunks)

### PR-1: Port `cmd/manager/main.go` parity gains (v0.3.6) ✅ Done

Added `--version` flag, `certwatcher` integration, and `metrics/filters.WithAuthenticationAndAuthorization` to the canonical entrypoint. All additions are gated on flags that default to off/empty; existing operators see no behaviour change.

**Shipped**: v0.3.6 (#115).

### PR-2: Add Dockerfile parametrization to `build/Dockerfile.manager` (v0.3.6) ✅ Done

Added `ARG BUILDER_IMAGE`, `ARG BASE_IMAGE`, `ARG GOPROXY` / `GOINSECURE` / `GOPRIVATE` / `GOSUMDB`, and CA-cert bundle injection. Enables corporate forks to point at internal mirrors without patching Dockerfiles.

**Shipped**: v0.3.6 (#117).

### PR-3: Point `make docker-build` / `make docker-buildx` at the canonical Dockerfile (v0.3.6) ✅ Done

Changed `make docker-build`, `make docker-buildx`, `make build`, and `make run` to use `cmd/manager/main.go` + `build/Dockerfile.manager`. Silent fix for latent bug #113: `make docker-build` now produces a complete manager binary (emits `virtrigaud_build_info`, registers VMSnapshot + VMMigration controllers).

**Shipped**: v0.3.6 (#119).

### PR-4: Delete the orphaned local-dev path (v0.3.6) ✅ Done

Removed `cmd/main.go` and root `Dockerfile`. All Makefile targets that previously used these files were rewired in PR-3.

**Shipped**: v0.3.6 (#121).

### PR-5: Flip metrics-secure default to `true` (v0.4.0) — deferred

Changes `--metrics-secure` default from `false` to `true`. Breaking: operators with HTTP-scrape Prometheus configurations will stop receiving metrics on upgrade unless they update their scrape config or explicitly set `--metrics-secure=false`. Held for v0.4.0 with loud release-note callout.

**Target**: v0.4.0. Must NOT ship in a v0.3.x point release.

## Security implications

| Item | Status |
|---|---|
| **HTTPS-by-default metrics (PR-5)** | Deferred to v0.4.0. Removes a plaintext channel that exposes operator internals to anonymous scrapers. Paired with cert-manager guidance and a Helm values escape hatch. |
| **`certwatcher` integration (PR-1)** | Shipped v0.3.6. Enables cert rotation without pod restart — critical for cert-manager 90-day renewals. |
| **`metrics/filters.WithAuthenticationAndAuthorization` (PR-1)** | Shipped v0.3.6. Activates when `--metrics-secure=true`. Requires Prometheus ServiceAccount to have RBAC on `/metrics`. |
| **`BUILDER_IMAGE`/`BASE_IMAGE` parametrization (PR-2)** | Shipped v0.3.6. Banking customers categorically cannot pull from `docker.io` or `gcr.io` in production build pipelines; this unblocks them without requiring Dockerfile forks. |

## Consequences

### Positive

- **One build path, one entrypoint.** Future contributors don't have to ask "which Dockerfile?" The v0.3.3-rc4 diagnosis cost doesn't recur.
- **VMSnapshot and VMMigration controllers are no longer silently missing from local-dev builds.** Anyone who ran `make docker-build` for local testing and then said "why isn't snapshot working?" was tripping over this.
- **Banking-deployment posture strengthens**: BUILDER/BASE parametrization, CA cert handling, certwatcher, and metrics RBAC are now in the canonical path.

### Negative

- **PR-5 is breaking and operator-visible.** Even with release notes, some operators will discover the HTTP→HTTPS change the hard way when their Prometheus stops scraping.
- **PR-3 silently activated VMSnapshot/VMMigration controllers for local-dev users.** Correct behaviour, but a behaviour change. Called out explicitly in the v0.3.6 CHANGELOG.

## Follow-ups

| ID | Item | Owner | Priority | Status |
|----|------|-------|----------|--------|
| F1 | ~~Promote ADR from `fieldTesting/`~~ | tech-writer | low | Done (v0.3.6) |
| F2 | ~~Implement PR-1 through PR-4~~ | staff-engineer + golang-engineer | medium | Done (v0.3.6) |
| F3 | **Implement PR-5** with explicit release-note callout and Helm values escape hatch | staff-engineer + security-architect | low | Open (v0.4.0) |
| F4 | **Operator-facing doc** in `docs/operators/build-customization.md` covering `BUILDER_IMAGE` / `BASE_IMAGE` / `GOPROXY` for corporate environments | tech-writer | low | Open |
| F5 | ~~Audit `hack/dev-deploy.sh` for Dockerfile path assumptions~~ | golang-engineer | low | Done (PR-3) |
| F6 | **Consider renaming `build/Dockerfile.manager` → `cmd/manager/Dockerfile`** for layout consistency with provider Dockerfiles | staff-architect | low | Open (optional, post-v0.3.6) |

## References

- Issue: [#92](https://github.com/projectbeskar/virtrigaud/issues/92) — chore: remove orphan cmd/main.go and root Dockerfile
- PR [#115](https://github.com/projectbeskar/virtrigaud/pull/115) — PR-1 (cmd/manager parity)
- PR [#117](https://github.com/projectbeskar/virtrigaud/pull/117) — PR-2 (Dockerfile parametrization)
- PR [#119](https://github.com/projectbeskar/virtrigaud/pull/119) — PR-3 (Makefile redirect; fixes #113)
- PR [#121](https://github.com/projectbeskar/virtrigaud/pull/121) — PR-4 (remove orphans)
- PR [#85](https://github.com/projectbeskar/virtrigaud/pull/85) — `internal/version` populated; prerequisite for `--version` flag port
- PR [#112](https://github.com/projectbeskar/virtrigaud/pull/112) — G6 per-Provider CircuitBreaker; exposed the dual-path porting tax
- Companion: [ADR-0001](./0001-transport-grpc-and-capi-integration.md) — gRPC transport choice
