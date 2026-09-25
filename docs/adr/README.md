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
| [0003](./0003-mtls-and-provider-grpc-auth.md) | Wire mTLS and provider gRPC authentication | Accepted | 2026-05-27 | Wire mTLS manager↔provider on the existing CRD surface, TLS-on-by-default (Option C) with per-Provider `tls.enabled=false` escape hatch; `validateTLSPeer` SAN allow-list (permissive empty); manual cert provisioning (no cert-manager template). Code-complete on `main` (PRs #157/#158/#159); targeted at v0.3.7. |
| [0004](./0004-libvirt-ssh-host-key-verification.md) | Libvirt SSH host-key verification | Accepted | 2026-05-27 | Provider→hypervisor sibling of ADR-0003: libvirt SSH host-key verification on-by-default (Option C), env escape hatch `LIBVIRT_INSECURE_SKIP_HOST_KEY_VERIFICATION=true`; `known_hosts` co-located in the existing credentials Secret; hard-fail (no TOFU) on missing trust material; no CRD change. Closes #149; targeted at v0.3.7. |
| [0005](./0005-image-preparation-trigger-model.md) | Image-preparation trigger model | Accepted | 2026-06-08 | PR-5 of #154: lazy, VM-create-driven image prepare (not eager); `ImagePreparer` optional capability gated by both type-assert AND `Provider.status.reportedCapabilities.supportsImageImport` (#176); VirtualMachine controller is the single writer of `vmimages/status` (re-GET under `RetryOnConflict`) to avoid the #189-class race; `Ready` = OR-of-providers, `ProviderStatus`/`AvailableOn` per-provider truth. No proto/CRD-spec change. "Create consumes the prepared template" deferred to PR-6 (additive proto). |
| [0006](./0006-storage-backend-agnostic-cross-hypervisor-migration.md) | Storage-backend-agnostic, any-direction cross-hypervisor VM migration | Accepted | 2026-06-11 | Replaces the PVC-only transfer model. S3 is the primary staging backend, NFS second, PVC for compatibility only. Two transfer modes (`relay`, `direct`); the target owns format conversion; dual checksum; capabilities gated per (provider, backend, direction, mode); phased slices. Tracking #236. |
| [0007](./0007-clustered-orchestrator-provider.md) | Clustered orchestrator provider | Accepted | 2026-09-22 | VirtRigaud as the cluster manager for hypervisors without one (libvirt first): `Host`/`HostPool` inventory, a filter-and-score scheduler, `target_host_id` binding. Addendum A covers post-create lifecycle routing. Blocking decisions in `0007-0008-blocking-decisions.md`. |
| [0008](./0008-libvirt-pure-go-driver-and-ssh-transport.md) | Pure-Go libvirt driver and in-process SSH transport | Accepted | 2026-09-22 | Replaces the libvirt provider's `virsh`-over-SSH control plane with `go-libvirt` over an in-process SSH client, behind a per-host connection seam. Strangler rollout: reads are shadow-compared first, and the native flip is gated on the soak. Live migration stays on `virsh migrate` in v1. |
| [0009](./0009-prepared-image-artifact-identity.md) | Prepared-image artifact identity, provenance stamps, and fail-closed reuse | Proposed | 2026-09-25 | Release blocker for cross-namespace VMImage sharing (#343): prepared artifacts named by provider-derived `(VMImage UID, source digest)` names instead of the bare VMImage name; provenance stamp per hypervisor (vSphere ExtraConfig, libvirt sidecar, Proxmox description/tags); reuse only on a matching stamp, otherwise fail-closed Conflict; one artifact per image and hypervisor location; additive proto + capability, old providers fail closed; legacy artifacts left in place. |

## Contributing an ADR

1. Copy the format from an existing ADR.
2. Number it sequentially (NNNN).
3. Set Status to **Proposed** until accepted.
4. Open a PR; move to **Accepted** when merged to `main`.
5. For superseded ADRs, update the old ADR's Status line and link to the new one.
