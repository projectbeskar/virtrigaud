# ADR-0001: Transport choice (gRPC) and future CAPI integration layer

> **Location**: `docs/adr/0001-transport-grpc-and-capi-integration.md`
> Promoted from `fieldTesting/` on 2026-05-27. The gRPC transport decision has shipped through v0.3.6; the CAPI layering principle is settled.

## Status

**Accepted** — 2026-05-22 (promoted 2026-05-27)

## Context

Two questions get raised periodically about VirtRigaud's architecture:

1. **Is gRPC the right manager↔provider transport on performance grounds?** A simpler-looking alternative (REST/JSON, or CRD-as-RPC) keeps being mentioned in design discussions, and "could we just use REST?" is a fair question for any synchronous RPC system.

2. **Does the gRPC choice affect the future plan to build a Cluster API (CAPI) Infrastructure Provider for VirtRigaud?** There's a perception that the transport choice constrains what CAPI integration can look like.

The point of this ADR is to close both questions definitively so they don't re-open every time a new contributor wonders.

### Relevant facts

- **RPC surface** is in `proto/provider/v1/provider.proto`: 17 coarse-grained verbs (Validate, Create, Delete, Power, Describe, Reconfigure, HardwareUpgrade, SnapshotCreate/Delete/Revert, Clone, ImagePrepare, ExportDisk, ImportDisk, TaskStatus, GetCapabilities, plus health/version).
- **RPC profile**: 10–100 RPC/sec at peak per cluster, kilobyte payloads, long-running ops use the `TaskRef` + `TaskStatus` polling pattern (not streaming).
- **Disk transfer** uses RWX PVCs as an out-of-band sidechannel — the actual disk bytes do NOT flow through the gRPC channel. `ExportDisk`/`ImportDisk` carry references to PVC paths, not contents.
- **Provider impl scale**: vSphere (~3842 LOC), Libvirt (~2057 LOC), Proxmox (~1852 LOC), Mock (~744 LOC). Adding REST equivalents on the provider side would mean rewriting four servers + their conformance harness.
- **Existing transport code**: `internal/transport/grpc/` (manager-side client), `sdk/provider/` (provider SDK with server, middleware, capabilities, errors). All wired; observability fully collected as of v0.3.6 (11 of 12 `virtrigaud_*` metric families, G6 circuit breaker per Provider CR).
- **CAPI integration model**: Cluster API infrastructure providers (CAPV, CAPZ, CAPI-libvirt, CAPI-openstack) reconcile CAPI's `Machine`/`Cluster` CRDs and emit their own `InfrastructureMachine`/`InfrastructureCluster` CRDs that CAPI watches via OwnerReferences. **CAPI's contract with infra providers is CRDs, not wire RPC.**

## Decision

### Part 1: Keep gRPC as the manager↔provider transport.

The performance argument is a non-question — VirtRigaud is nowhere near the scale where transport throughput matters. The real and load-bearing value of gRPC for this project is:

1. **Proto3 + `buf` as a contract enforcement mechanism** across heterogeneous provider implementations. This is what keeps four provider impls (and any future ones — Firecracker, Cloud-Hypervisor, QEMU direct) honest with each other.
2. **The `Unimplemented` + `GetCapabilities` pattern** for clean feature negotiation without silent no-ops. Only works cleanly with a typed IDL.
3. **First-class deadlines, cancellation, mTLS readiness, and interceptor middleware** for cross-cutting concerns. The wiring exists; mTLS is not yet default-on (see open issues #147, #148).
4. **Bidirectional streaming available** if disk transfer ever moves in-band. Not in the plan today, but it's a free option.

### Part 2: A future CAPI Infrastructure Provider will sit on top of VirtRigaud's CRDs, not its gRPC contract.

When `cluster-api-provider-virtrigaud` is built, its architecture will be:

```
┌─ Mgmt cluster ──────────────────────────────────────────────────┐
│                                                                  │
│  CAPI controllers                                                │
│         │                                                        │
│         │  watches Machine/Cluster CRDs via OwnerReferences      │
│         ▼                                                        │
│  cluster-api-provider-virtrigaud (NEW REPO)                     │
│    reconciles: VirtRigaudMachine / VirtRigaudCluster CRDs       │
│         │                                                        │
│         │  emits: VirtualMachine + Provider CRDs                │
│         ▼                                                        │
│  VirtRigaud manager (EXISTING)                                  │
│         │                                                        │
│         │  gRPC/TLS (INTERNAL to VirtRigaud — invisible above)  │
│         ▼                                                        │
│  Provider pods (vSphere / Libvirt / Proxmox / ...)              │
│         │                                                        │
└─────────┼────────────────────────────────────────────────────────┘
          ▼
       Hypervisor
```

The gRPC layer is two levels below CAPI. **It is not in the CAPI provider's path, and CAPI never sees it.** This is the same layering used by every other CAPI provider.

## Alternatives considered

### Transport alternatives (rejected)

| Option | Why not |
|--------|---------|
| **REST/JSON over HTTP** | No measurable perf change; loses codegen discipline; providers will drift on schema unless we reintroduce OpenAPI codegen. Cost of switching ≈ rewrite four provider servers + manager client + conformance harness. Gain: marginally easier debugging via `curl`. Not worth it. |
| **CRD-as-RPC** (manager writes a `VMOperation` CR, provider watches it) | Every `Describe` becomes an etcd write. Providers need cluster credentials and watch quotas. Operational latency tied to apiserver QPS. Hard to do streaming. Trades complexity for a different kind of complexity, with worse scaling characteristics. |
| **Async messaging (NATS, Redis Streams, Kafka)** | Strictly worse version of CRD-as-RPC for this workload. Adds infrastructure dependency without solving anything we have. |

### CAPI integration alternatives (rejected)

| Option | Why not |
|--------|---------|
| **CAPI provider talks gRPC directly to provider pods, bypassing VirtRigaud's CRDs** | Makes the gRPC contract a *public* API surface. Doubles blast radius on every proto change (CAPI users break, not just internal providers). Bypasses the per-Provider-CR multi-tenant boundary. |
| **CAPI provider as an in-tree feature of VirtRigaud (no separate repo)** | Couples release cadences. Forces VirtRigaud users to take CAPI dependency they may not want. Not the established CAPI pattern. |
| **CAPI provider links VirtRigaud's controllers as a library** | Rare in CAPI ecosystem. Inherits lifecycle pain from both projects. No clear advantage over the CRD-translation pattern. |

## Consequences

### Positive

- The transport question is settled. Future contributors won't relitigate gRPC vs REST without first reading this ADR.
- The CAPI integration shape is settled. When the CAPI provider is built, the team knows it's a CRD translator and doesn't go down the gRPC-exposure path.
- The proto contract (`proto/provider/v1/provider.proto`) can continue to evolve as the internal contract it is, without worrying about external CAPI consumers.

### Negative

- **Provider authors face a steeper onboarding curve** than a REST API would impose: they need to consume protobuf-generated bindings, understand the `Unimplemented` + capabilities pattern, and use `buf` for proto codegen. Mitigated by `sdk/provider/` which abstracts most of it, and by the conformance harness in `internal/conformance/`.
- **Debugging is harder** for operators looking at the wire (no `curl -X POST` on `/Validate`). Mitigated by gRPC reflection (if enabled) and `grpcurl`.

### Neutral

- mTLS posture between manager and providers is not yet default-on (issues [#147](https://github.com/projectbeskar/virtrigaud/issues/147), [#148](https://github.com/projectbeskar/virtrigaud/issues/148)). gRPC makes mTLS easy when we decide to enforce it; this ADR doesn't change that.

## Follow-ups

| ID | Item | Owner | Priority | Status |
|----|------|-------|----------|--------|
| F1 | ~~Promote ADR from `fieldTesting/`~~ | tech-writer | low | Done (v0.3.6) |
| F2 | **Add a comment block to `proto/provider/v1/provider.proto`** near `ExportDisk`/`ImportDisk` explicitly declaring disk transfer as out-of-band (PVC-mediated). | golang-engineer | low | Open |
| F3 | **Interceptor coverage audit**: verify `sdk/provider/middleware/` interceptors emit per-RPC latency, error counters, and tracing spans. v0.3.6 wired 11 of 12 metric families; confirm the interceptor loop is closed end-to-end. | staff-engineer | medium | Partially done (v0.3.6) |
| F4 | **ADR-0002 "CAPI provider sits on top of VirtRigaud CRDs"** — when CAPI work actually starts. Captures the layering decision more formally with `VirtRigaudMachine`/`VirtRigaudCluster` schema sketch. | staff-architect | deferred | Open |
| F5 | **Provider-author onboarding guide** covering `sdk/provider/`, the `buf` codegen flow, capability declaration, and conformance harness. | tech-writer | low | Open |

## Open questions

1. **Should mTLS be default-on in v0.4?** Separate ADR needed; the answer affects how strongly we lean on "gRPC has good security ergonomics" as a justification here. See #147.
2. **Is `VirtRigaudCluster` going to need a control-plane endpoint (LB / kube-vip / VIP)?** This is the biggest design call in a future CAPI provider. Decision affects timing of F4.
3. **Does `VMSet` overlap awkwardly with CAPI's `MachineDeployment`/`MachineSet`?** Worth deciding whether the CAPI provider creates individual `VirtualMachine` CRDs or composes via `VMSet`. Decision belongs in F4.

## References

- Related PR: [#83](https://github.com/projectbeskar/virtrigaud/issues/83) (metrics-registry fix)
- Related PR: [#112](https://github.com/projectbeskar/virtrigaud/issues/112) (G6 CircuitBreaker per Provider CR)
- Security issues: [#147](https://github.com/projectbeskar/virtrigaud/issues/147) mTLS, [#148](https://github.com/projectbeskar/virtrigaud/issues/148) provider auth
- Code touchpoints:
  - `proto/provider/v1/provider.proto`
  - `internal/transport/grpc/`
  - `sdk/provider/` (server, client, middleware, capabilities, errors)
  - `internal/providers/{vsphere,libvirt,proxmox,mock}/server.go`
