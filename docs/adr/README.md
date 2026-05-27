# Architecture Decision Records

This directory contains Architecture Decision Records (ADRs) for VirtRigaud. ADRs record significant design decisions that are binding on the codebase and should not be re-litigated without reading the relevant ADR first.

## Format

```
# ADR-NNNN: Title
## Status: Proposed | Accepted | Deprecated | Superseded by ADR-XXXX
## Context
## Decision
## Consequences
```

## Index

| # | Title | Status | Date | Summary |
|---|-------|--------|------|---------|
| [0001](./0001-transport-grpc-and-capi-integration.md) | Transport choice (gRPC) and CAPI integration layer | Accepted | 2026-05-22 | Settles gRPC as the manager↔provider transport and establishes that a future CAPI provider sits on top of VirtRigaud's CRDs (not gRPC). |
| [0002](./0002-build-path-consolidation.md) | Consolidate two parallel manager build paths | Accepted | 2026-05-24 | H1 decision: retire the local-dev `cmd/main.go` + root `Dockerfile`; port missing features to the canonical path; shipped PRs 1–4 in v0.3.6; PR-5 (HTTPS-by-default metrics) deferred to v0.4.0. |

## Contributing an ADR

1. Copy the format from an existing ADR.
2. Number it sequentially (NNNN).
3. Set Status to **Proposed** until accepted.
4. Open a PR; move to **Accepted** when merged to `main`.
5. For superseded ADRs, update the old ADR's Status line and link to the new one.
