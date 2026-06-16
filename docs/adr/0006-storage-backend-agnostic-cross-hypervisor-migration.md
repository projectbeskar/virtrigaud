# ADR-0006: Storage-backend-agnostic, any-direction cross-hypervisor VM migration

## Status

**Accepted (2026-06-11)** — reviewed and accepted by the maintainer; the open questions
below are **resolved** (see *Resolved decisions*). Implementation proceeds in phased
slices (see *Phasing*); **Slice 0** (additive CRD/proto + honest capability surface) is
the first to land. This ADR supersedes the implicit "PVC-only" transfer model that shipped
through v0.3.9 and establishes the architecture for the project's now-primary Key Value:

> **Regardless of the storage backend (NFS / S3), VirtRigaud must be able to migrate VMs
> across hypervisors in *any* direction.**

**Implementation status (2026-06-16)**: the S3 / `relay` paths for the
**vSphere ↔ libvirt** pair — **Slice 1** (vSphere → libvirt) and **Slice 2**
(libvirt → vSphere) — are implemented and **end-to-end validated** on real
hardware. See *Implementation log — validated slices* below for the as-built
slicing (which diverged from the original phasing) and the key findings.

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Hypervisors in scope**: vSphere, Libvirt/KVM, Proxmox VE (a Proxmox lab host is being
added for end-to-end validation).

**Tracking**: [#236](https://github.com/projectbeskar/virtrigaud/issues/236).

**Related**:
- [ADR-0001](./0001-transport-grpc-and-capi-integration.md) — gRPC is the
  manager↔provider transport; `ExportDisk`/`ImportDisk` ride it.
- [ADR-0003](./0003-mtls-and-provider-grpc-auth.md) — mTLS protects the manager↔provider
  channel that carries transfer credentials.
- [ADR-0005](./0005-image-preparation-trigger-model.md) — the capability double-gate
  (`contracts.X` type-assert + `Status.ReportedCapabilities` flag) is reused here for
  per-backend / per-mode negotiation.
- The defaulted-bool `omitempty` footgun fix (PR #235): every new defaulted bool field in
  this ADR omits `omitempty` and documents why.

---

## Context

### The current model, traced through the live code

Migration today is **PVC-only and structurally broken for libvirt and Proxmox**:

1. `VMMigration` reconcile creates an RWX PVC, labels it
   `virtrigaud.io/component=migration-storage`, and builds a `pvc://<pvc>/<path>`
   destination URL (`internal/controller/vmmigration_controller.go`).
2. The Provider controller watches those PVCs and mounts them into **both** provider pods
   at `/mnt/migration-storage/<pvc>` (`internal/controller/provider_controller.go` →
   `discoverMigrationPVCs`).
3. `ExportDisk` reads the source disk, optionally converts it, and uploads to the `pvc://`
   URL; `ImportDisk` reads it back. The only backend is PVC — `internal/storage/storage.go`
   `NewStorage` rejects anything but `pvc`. The CRD enforces this:
   `MigrationStorage.Type` is `+kubebuilder:validation:Enum=pvc`.

### The fatal flaw: disk *resolution* and disk *I/O* run on different machines

| Provider | Disk lives | I/O realistically runs | Can reach a K8s CSI PVC? |
|----------|-----------|------------------------|--------------------------|
| libvirt/KVM | host storage pool (`/vm-pool01/…`) | **host-side** (over `qemu+ssh://`) | **No** |
| vSphere | vCenter datastore | vCenter API (streams to the pod) | **No** |
| Proxmox VE | PVE node storage | **node-side** (REST API / SSH) | **No** |

For libvirt, `GetDiskInfo` runs `virsh vol-info … --pool default` **on the host** and
returns a host path; then `ExportDisk` feeds that host path to `qemu-img` and the uploader
**inside the pod** — which has no access to the host's filesystem. Proxmox is identical
(and its export is explicitly stubbed: `"Using simplified disk export (not using
vzdump)"`). A CSI PVC (e.g. Longhorn) is the mirror problem: visible to pods, never to
hypervisor hosts. The only configuration that ever worked end-to-end was vSphere→libvirt
over an **NFS export visible to both the datastore and the pod at a consistent path** —
the shared-filesystem assumption happening to hold.

**Verdict:** every cell in the last column is "No". **A Kubernetes PVC is the wrong
primitive for a hypervisor-to-hypervisor transfer, because hypervisor hosts are not
Kubernetes nodes.**

---

## Decision

### D1 — Transfer medium is a pluggable backend; **S3 is primary**, NFS second, PVC compat-only

A transfer-backend abstraction keyed by URL scheme:

- **`s3://bucket/prefix/key` — primary.** An **external, generic S3-compatible** object
  store (AWS S3, MinIO, Ceph RGW, …) that is **outside the providers and the cluster** and
  reachable from both ends. Object storage removes the shared-filesystem requirement
  entirely: the source streams bytes up, the target streams bytes down, nothing needs a
  shared mount. This is the configuration that makes the any-direction matrix achievable.
- **`nfs://server/export/key` — second.** For environments without object storage.
- **`pvc://name/key` — retained, compat-only.** Honestly constrained: valid only when both
  endpoints' I/O is pod-side and the PVC is RWX (in practice vSphere↔vSphere / shared-FS).
  Documented as **not valid** for libvirt/Proxmox host-resident disks.

Both `s3` and `nfs` are addressed by **logical coordinates + a relative object key**
(`endpoint`/`bucket`/`key`; `server`/`export`/`key`). No absolute mount path is baked into
the CRD, so there is no per-host path-consistency requirement.

### D2 — Two transfer **modes**: `relay` (universal) and `direct` (scale)

The transfer carries *where it runs*, not just a URL:

- **`relay` — host → pod → backend.** The host does only native disk I/O; the **provider
  pod is the backend client**. For libvirt/Proxmox, the host streams disk bytes over the
  existing SSH channel (`qemu-img convert -O <fmt> <vol> /dev/stdout` /
  `virsh vol-download … /dev/stdout`) into the pod, which runs the backend upload. For
  vSphere, the pod already receives the disk as a vCenter API stream. The pod receives a
  **byte stream**, never a file lookup — the precise thing the old model got wrong.
  Relay is the **universal-compatibility floor**: it needs no host backend reachability,
  no host tooling beyond what's already there, and the **backend credentials never leave
  the pod**.
- **`direct` — host → backend.** The host writes straight to the backend
  (`aws s3 cp`/`rclone` on the host, or the host's own NFS mount). No pod bottleneck;
  parallel migrations scale across hosts. Requires the host to have backend reachability,
  a transfer tool, and (for S3) credentials delivered ephemerally for the migration.

Selection: `spec.storage.transferMode ∈ {auto, relay, direct}`, default **`auto`**.
`auto` picks `direct` only when **both** endpoints report the host can do it (backend
reachable + tooling present, via a capability/pre-flight probe), else `relay`. An explicit
`direct` that an endpoint cannot satisfy **fails loudly** — never a silent downgrade.

> **Why keep both:** relay works where hosts are isolated / can't reach the backend or each
> other; direct is needed so large disks and many concurrent migrations scale without
> funnelling every byte through the provider pods.

### D3 — Disk I/O executes where the disk lives

Disk read/write is always native to the hypervisor (host-side for libvirt/Proxmox over
SSH; vCenter API for vSphere). Only the **backend write/read** moves between pod (relay)
and host (direct). Data never traverses a CSI PVC a host cannot see.

### D4 — The **target** owns format conversion (where possible)

The source always exports its **native** format (no source-side conversion). The target
converts on import (`qemu-img` for libvirt/Proxmox; `qemu-img`+HttpNfc for vSphere).
"Where possible" is capability-gated: if a target cannot ingest/convert a given source
format, that direction is advertised unsupported — not faked.

### D5 — **Dual checksum**

1. **Transfer integrity** — SHA256 of the *transferred object* (source computes
   pre-upload, target verifies post-download). Proves the backend round-trip is bit-exact.
2. **Post-conversion validation** — `qemu-img check` on the converted disk (structural
   integrity; no reference checksum needed), plus the existing optional `CheckBoot`.

A byte-equality check after conversion is intentionally **not** attempted — conversion is
not byte-deterministic, so there is no reference to compare against.

### D6 — Capability negotiation per `(provider, backend, direction, mode)`

Reuse the ADR-0005 double-gate. `GetCapabilities` advertises supported export backends,
import backends, and transfer modes. The controller migrates only when the requested
backend+mode is in the intersection of source-export and target-import capabilities; an
empty intersection fails at **Validate**, before any snapshot/export side effect.

### D7 — Phase machine unchanged

The existing `MigrationPhase` enum already models the flow
(`Pending→Validating→Snapshotting→Exporting→Transferring→Converting→Importing→Creating→
ValidatingTarget→Ready/Failed`). What changes is *what* Export/Transfer/Import do per
backend+mode — not the states.

---

## CRD / API changes (additive, v1beta1-safe)

`api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`:

```go
// MigrationStorage
// +kubebuilder:validation:Enum=pvc;nfs;s3
// +kubebuilder:default=pvc
Type string `json:"type"`

// +optional
TransferMode string `json:"transferMode,omitempty"` // +kubebuilder:validation:Enum=auto;relay;direct  +kubebuilder:default=auto

// +optional
NFS *NFSStorageConfig `json:"nfs,omitempty"`
// +optional
S3  *S3StorageConfig  `json:"s3,omitempty"`

type S3StorageConfig struct {
    Bucket               string    `json:"bucket"`
    Endpoint             string    `json:"endpoint,omitempty"` // empty ⇒ AWS default; set for MinIO/Ceph RGW
    Region               string    `json:"region,omitempty"`
    Prefix               string    `json:"prefix,omitempty"`
    CredentialsSecretRef ObjectRef `json:"credentialsSecretRef"` // Secret (access key/secret[/token]); NEVER inline
    UsePathStyle         bool      `json:"usePathStyle"`         // NO omitempty (defaulted-bool footgun, PR #235)
}

type NFSStorageConfig struct {
    Server   string `json:"server"`
    Export   string `json:"export"`
    Path     string `json:"path,omitempty"`     // subpath under the export
    ReadOnly bool   `json:"readOnly"`            // NO omitempty (defaulted-bool footgun, PR #235)
}
```

`type` keeps defaulting to `pvc` for manifest compatibility; docs steer new users to `s3`.
A validating webhook enforces *exactly one* of `pvc|nfs|s3` config blocks, matching `type`.
Credentials live only in the referenced Secret; `MigrationStorageInfo.URL` in status may
carry an `s3://bucket/prefix/...` URL (no secret material). Regenerate with the CRD source
of truth — never hand-edit `config/crd/bases/` or `charts/virtrigaud/crds/`.

---

## gRPC contract changes (`proto/provider/v1/provider.proto`, additive — no major bump)

- **`ExportDiskRequest` / `ImportDiskRequest`**: the existing `destination_url`/`source_url`
  + `map<string,string> credentials` stay; add `string backend_type`, `string transfer_mode`,
  and `string storage_options_json` (endpoint/prefix/path-style/region without further proto
  churn). Credential map keys are formalized per scheme (`s3`: `access_key_id`,
  `secret_access_key`, `session_token`, `region`, `endpoint`).
- **Keep the async task-ref model** (`ExportDiskResponse.task` / `ImportDiskResponse.task`).
  The bytes never traverse the manager↔provider channel, so no gRPC byte-streaming — the
  provider returns a `TaskRef`, the controller polls `TaskStatus`. Multipart upload state
  (UploadId + completed parts) is tracked in `VMMigration.status` so an interrupted transfer
  **resumes**, not restarts.
- **`GetCapabilitiesResponse`**: add `repeated string supported_export_backends`,
  `repeated string supported_import_backends`, `repeated string supported_transfer_modes`.
  Surface through `sdk/provider/capabilities/` and onto `Provider.status.reportedCapabilities`.

---

## Per-provider implementation sketch

"Where it runs" is load-bearing.

**libvirt/KVM** — disk on host; relay streams `qemu-img convert -O <fmt> <vol> /dev/stdout`
over SSH into the pod's S3/NFS client; direct runs `aws s3 cp -`/NFS-write on the host.
Native qcow2/raw; vmdk via conversion. Replaces the pod-side upload that is the broken path
today.

**vSphere** — disk in datastore; export via vCenter API (OVF/HttpNfc) streamed through the
pod (inherently relay for S3); can be direct for an NFS datastore. vmdk native; qcow2 via
pod-side `qemu-img`. The only provider that can legitimately use `pvc`.

**Proxmox VE** — disk on node; relay streams a node-side export over SSH into the pod;
direct runs the node-side `aws s3 cp`/NFS-backed PVE storage. Replaces the `vzdump` stub.
qcow2/raw native; vmdk via conversion.

Honest gaps (vmdk native consumption on libvirt/Proxmox; a host with no S3 egress in
`direct`) are **declared via capabilities**, never faked.

---

## Security / compliance

- **Credentials** live only in the `S3StorageConfig.credentialsSecretRef` Secret. The
  manager reads it at reconcile and passes it over the mTLS gRPC channel.
  - **relay**: the credential stays in the **pod**; the hypervisor host never sees it.
  - **direct**: the credential is injected **ephemerally** as env on the host-side command
    for the duration of the transfer, never persisted to host disk/Status/logs.
- The `direct` path's host credential exposure (brief process-env) requires a
  **`security-architect` review before it merges**.
- NFS carries no CRD secret. Document `root_squash`/permissions expectations.
- No secret material in Status, events, or logs; the `internal/obs/logging` redactor must
  cover the new credential keys. Add a test asserting it.
- New RBAC: namespaced `get` on the credentials Secret only (mirrors #152 scoping).
- New outbound flow (host/pod → external S3) for the banking review; may need an egress
  allow-list / VPC endpoint.

---

## Phasing

- **Slice 0 — make it honest (small, additive).** CRD enum + config blocks + `transferMode`
  + `GetCapabilities` backend/mode fields. All non-`pvc` paths return `Unimplemented`;
  providers advertise `export=[pvc]` only. Stops the silent-failure footgun; gives the
  controller a real negotiation surface. No behavior change for existing users.
- **Slice 1 — S3 / relay / libvirt→libvirt (cross-host).** The minimal end-to-end proof
  that decouples from shared-FS: SSH-stream export on host A → pod S3 multipart upload →
  pod download → SSH-stream import on host B. Exercises capability negotiation,
  Secret→credentials propagation, relay execution, multipart+resume, dual checksum — on one
  hypervisor type, removing format conversion as a variable.
- **Slice 2 — S3 / direct mode** (libvirt, then Proxmox once the lab host lands), incl.
  the host-side credential path and its security review. **libvirt → Proxmox over S3** is
  the natural cross-type proof.
- **Slice 3 — S3 / vSphere** (export + import): API/pod-side execution + vmdk↔qcow2
  conversion. Unlocks the full any-direction matrix on S3.
- **Slice 4 — NFS backend** across all three (pod-mount for relay; host/datastore mount for
  direct).
- **Slice 5 — resumability hardening, end-to-end checksum/verification, cleanup** wired to
  `MigrationOptions.CleanupPolicy`.

Recommendation: **S3 before NFS, relay before direct, one provider pair at a time,
libvirt-first.**

---

## Implementation log — validated slices (2026-06-16)

The **as-built** slicing diverged from the planned phasing above: implementation
prioritized the **vSphere ↔ libvirt cross-type pair over S3 / `relay`** (the
highest-value proof — it exercises format conversion in both directions) ahead of
the planned libvirt→libvirt and `direct`-mode slices. As shipped:

- **Slice 1 — vSphere → S3 → libvirt (`relay`), validated.** The vSphere source
  exports its native vmdk (flattened to a single monolithic file) to S3; the
  libvirt target converts vmdk → qcow2 on the host during import. (#236.)
- **Slice 2 — libvirt → S3 → vSphere (`relay`), validated 2026-06-16.** The
  libvirt source flattens its disk to a standalone qcow2 and streams it to S3; the
  vSphere target converts qcow2 → streamOptimized vmdk **in-pod** and imports it,
  then creates the VM. E2E proof: a live Ubuntu 24.04 libvirt VM was migrated to
  vCenter and powered on to a login prompt with its original hostname and a
  connected vmxnet3 NIC.

### Slice 2 decisions & findings (refine D3/D4 for the vSphere target)

1. **vSphere import = qcow2 → streamOptimized vmdk → NFC `HttpNfcLease`, not
   `CopyVirtualDisk`.** `CopyVirtualDisk` cannot ingest a foreign (qemu-img-built)
   vmdk — every subformat fails with `parameter not correct: fileType`. The
   provider instead runs `qemu-img convert -O vmdk -o subformat=streamOptimized`
   and uploads through an `ImportVApp` lease. D4 conversion is therefore
   **target-owned, executed in the provider pod**.
2. **The vSphere provider image must carry `qemu-img`.** The runtime is
   `debian:bookworm-slim` + `qemu-utils` (canonical `cmd/provider-vsphere/Dockerfile`).
   The stale, unused `build/Dockerfile.provider-vsphere` (distroless, no qemu-img)
   was removed so nobody ships an import-broken provider.
3. **The NFC lease device URL is rewritten to the vCenter host.** The lease returns
   an upload URL pointing at the ESXi host by FQDN, which an in-cluster provider pod
   typically cannot resolve; the provider rewrites the host to the vCenter host
   (the reachable `PROVIDER_ENDPOINT`, which proxies NFC).
4. **The transient import VM is `Unregister`ed, not `Destroy`ed** (vCenter 8 faults
   `Destroy` of a freshly imported VM), and the `[<ds>] <id>` import folder is
   force-deleted before import so retries to a fixed target name stay idempotent
   (otherwise `ImportVApp` lands the disk in a collision-suffixed `<id>_1` folder).
5. **Cross-hypervisor networks must be re-specified on the target.** A raw-disk
   migration carries the disk, not the NIC: `target.networks` must reference a
   `VMNetworkAttachment` mapping to a vSphere portgroup, or the migrated VM boots
   with no adapter. The guest's in-OS config (netplan) still targets the source's
   virtio NIC — a guest-OS concern, out of scope for the raw-disk transfer.
6. **Streaming robustness (both directions):** unknown-size S3 multipart part size
   is bounded to avoid ~525 MiB part buffers (OOM); the libvirt export reads a
   running source with `qemu-img convert -U`; and an export-stream failure surfaces
   the real S3 error (e.g. a full backing store) instead of the downstream
   `cat: exit status 255`.

**Deferred follow-ups** (tracked): honor `source.powerOffBeforeMigration` and
`target.powerOn:false` in the controller for both directions; snapshot of a
multi-disk source carrying a cloud-init CDROM (`--diskspec`); Proxmox over S3; the
NFS backend; and the `direct` transfer mode.

---

## Resolved decisions (from maintainer review, 2026-06-11)

1. **Host-side creds** → from a Kubernetes **Secret** (`credentialsSecretRef`); per-migration,
   ephemeral on the host only in `direct` mode; never in `relay`.
2. **Conversion** → **target**-owned where possible (capability-gated).
3. **Resumability** → **S3 multipart from day one**, state tracked in `status`.
4. **Checksum** → **dual**: transferred-object integrity **and** post-conversion
   `qemu-img check` (no after-conversion byte-equality check).
5. **S3** → **external, generic, S3-compatible**, reachable from both ends; **not** a
   PVE-native S3 storage type; one uniform S3 backend for all providers.
6. **NFS addressing** → **logical coordinates + relative key** (`server`/`export`/`key`);
   no per-host mount-path consistency required (pod mounts it in `relay`).
7. **Transfer modes** → **both** `relay` (universal floor) **and** `direct` (scale);
   `transferMode: auto|relay|direct`, default `auto`, loud-fail on impossible `direct`.

---

## Consequences

**Positive** — any-direction migration becomes achievable without hypervisor hosts mounting
Kubernetes storage; S3 removes the shared-FS assumption; relay keeps creds off hosts and
works in isolated topologies; direct scales for bulk/large transfers; additive CRD/proto
keep v1beta1 stable; honest capability negotiation kills the silent-no-op class.

**Negative / trade-offs** — real per-provider implementation surface (Export/Import rewritten
to run in the right place); relay routes all bytes through the provider pod (throughput
chokepoint — mitigated by `direct`); `direct` adds host egress + a credential-exposure path
needing security review; vmdk parity is conversion-based on libvirt/Proxmox (declared);
PVC is demoted (release-note it clearly).

---

## Open implementation questions (non-blocking)

- `auto` selection signal: a live reachability+tooling probe vs. a static provider-declared
  capability (proposal: capability-declared, refined by an optional pre-flight probe).
- Per-endpoint vs. shared NFS path when a provider's native mount differs (vSphere datastore
  vs. libvirt host mount) — addressed by carrying the logical `server`/`export`/`key` and
  letting each provider resolve its own mount.
- Multipart part size / parallelism defaults and where they live (provider config vs. fixed).

## Follow-ups this ADR creates

- `crd-update` (vmmigration_types.go), `proto-update` (provider.proto),
  `add-changelog-entry`, `verify-go-changes` per slice.
- `internal/storage/`: `s3.go` + `nfs.go` behind `NewStorage` (pod-side path); host-side
  `direct` transfers shell the tool on the host and may bypass `internal/storage`.
- Webhook: exactly-one-of storage block matching `type`.
- `security-architect` review of the `direct` credential path before Slice 2 merges.
- `tech-writer` / website: rewrite migration docs (PVC compat-only; S3 recommended; host
  egress requirement) — coordinated with implementation, not ahead of it.
