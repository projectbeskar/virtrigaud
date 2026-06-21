# Changelog

All notable changes to VirtRigaud will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2026-06-21 14:30] - fix(proxmox): NFS migration via kernel mount (pve-qemu lacks libnfs) (ADR-0006 Slice 4, #236)
**Author:** @wrkode (William Rizzo)

### Changed
- `internal/providers/proxmox/nfs.go`: replace the qemu-img `nfs://` (libnfs) transport — shipped in #273 but **non-functional on a stock PVE node** (`pve-qemu-kvm` is built without the libnfs block driver; the node's qemu-img rejects `nfs://` with `Unknown protocol 'nfs'`) — with a **kernel NFS mount**, exactly how PVE's first-class NFS storage mounts. The export is two-stage: qemu-img as root flattens the (root-owned) source disk to a local temp, then the temp is copied onto the mounted export as the migration's `nfs.uid/gid` via `setpriv` (a single process cannot be both root, to read the source, and the share's uid, to write the export — only libnfs can decouple those, which is why libvirt/vSphere keep the libnfs path). The import mounts the export and converts the staged qcow2 to the node-local stage file as `nfs.uid/gid`, then `qm importdisk` consumes it. New helpers `buildNFSKernelMountScript`, `buildNFSKernelExportScript`, `nfsSetprivPrefix`, `nfsInMountRel`; the mount runs `vers=3` (numeric-uid AUTH_SYS) with a trap that unmounts + removes the temp on exit.

### Why
Lab validation against the OpenMediaVault server proved the libnfs approach cannot work on Proxmox (unlike libvirt/vSphere, whose images/hosts have libnfs). The kernel-mount transport is Proxmox-idiomatic, needs no extra packages, and honors `nfs.uid/gid` so the staged objects interoperate with the libvirt/vSphere providers (which present the same uid via the libnfs URL). Validated GREEN end-to-end: **libvirt→Proxmox import** and **Proxmox→libvirt export** (a freshly-imported VM round-tripped back to libvirt), both directions over NFS against the lab OMV.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (proxmox provider image)
- [ ] Config change only

## [2026-06-21 12:00] - fix(migration): NFS stages a FLAT object key (nested key fails libnfs mount) (ADR-0006 Slice 4, #236)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/vmmigration_controller.go`: the NFS staging key is now a single flat filename `vmmigrations-<ns>-<name>-<stage>.qcow2` in the export root (or the operator's optional `path`), instead of the nested `vmmigrations/<ns>/<name>/<stage>.qcow2`. Unlike S3 — where a slashed key creates the prefix implicitly — NFS is a real filesystem: every parent directory must already exist, and qemu-img/libnfs (the only NFS client in the data plane) cannot `mkdir`. A nested key therefore failed the libnfs mount with `MNT3ERR_NOENT`. Surfaced by the first live libvirt↔libvirt NFS migration against the lab OpenMediaVault server.

### Why
Lab validation of the ADR-0006 Slice 4 NFS backend: a real qemu-img `nfs://` write cannot create intermediate directories, so the staged object must live where a directory already exists. ns/name/stage are encoded into the single filename so the object stays unique and traceable. The PVC and S3 backends are unchanged (PVC mkdirs on a mounted volume; S3 keys are flat prefixes).

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager)
- [ ] Config change only

## [2026-06-21 10:15] - feat(vsphere): NFS migration via pod-side qemu-img nfs:// + qemu-block-extra image (ADR-0006 Slice 4, #236)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/vsphere/nfs.go` (new): `exportLocalVMDKToNFS` / `importDiskFromNFS`. Unlike libvirt/Proxmox (host/node-side), vSphere has no shell on the hypervisor, so it runs `qemu-img` **in the provider pod** — the same place it already converts for the S3 import. Export: the existing pipeline downloads + flattens the disk to a self-contained local vmdk, then the pod's qemu-img writes it **as qcow2 straight to the `nfs://` export over libnfs** (no S3 client, no second upload). Import: the pod's qemu-img reads the staged qcow2 **directly from `nfs://`** and converts it — in one pass — to the streamOptimized vmdk the vCenter NFC `HttpNfcLease` requires, then imports it via the **same `nfcImportStreamOptimized` path the S3 import uses** (the genuinely-subtle NFC logic stays shared; only the source leg differs).
- `cmd/provider-vsphere/Dockerfile`: install `qemu-block-extra` — qemu's `block-nfs.so` (libnfs) driver. Without it the pod's qemu-img cannot open an `nfs://` URL.

### Changed
- `internal/providers/vsphere/server.go`: `ExportDisk`/`ImportDisk` gate on `migration.EnsurePVCS3OrNFSBackend` (accepts `nfs`, keeps pvc/s3); `nfs` is **exempt from the relay-mode gate** (it uses qemu-img's native transport, pod-side). `GetCapabilities` now advertises `nfs` in the export/import backends (`migration.PVCS3AndNFS*`). The export skips storage-client creation for `nfs` (validates the `nfs://` URL instead) and returns early after the qemu-img write; the import dispatches `nfs` to `importDiskFromNFS` before the s3 branch.

### Why
Third and final provider of the NFS backend (ADR-0006 Slice 4). With libvirt, Proxmox, and vSphere all advertising and serving `nfs`, every cross-hypervisor NFS migration path is now wireable end-to-end. vSphere's NFS legs reuse the lab-validated S3 download/flatten (export) and NFC lease import (import) machinery — only the byte-transport leg swaps to qemu-img's `nfs://` driver. NFS integrity is the post-import uuid query (qemu-img emits no in-stream byte checksum). Lab validation against the OMS server is the next step.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (vSphere provider image — adds `qemu-block-extra`)
- [ ] Config change only

## [2026-06-21 09:30] - feat(proxmox): NFS migration via node-side qemu-img nfs:// (ADR-0006 Slice 4, #236)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/proxmox/nfs.go` (new): `exportDiskToNFS` / `importDiskFromNFS`. The PVE NODE's `qemu-img` writes/reads the staged qcow2 **directly to/from the `nfs://` export over libnfs** (qemu's `block-nfs` driver) — no provider-pod hop, no S3 client. Export resolves the on-node disk path (`pvesm path`) and flattens straight to `nfs://` (reusing the tested `buildExportFlattenCommand` argv builder); import converts `nfs://` → a node-local stage file (new `buildNFSImportConvertCommand`) + `qemu-img check`, which the Create path then feeds to `qm importdisk` (Proxmox cannot `importdisk` an `nfs://` URL directly).
- `internal/providers/proxmox/capabilities.go`: `GetCapabilities` now advertises `nfs` alongside `s3` in the export/import backends (`migration.S3AndNFS*`).

### Changed
- `internal/providers/proxmox/server.go`: `ExportDisk`/`ImportDisk` now gate on `migration.EnsureS3OrNFSBackend` (was `EnsurePVCOrS3Backend`) — this **accepts `nfs`, keeps `s3`, and now rejects `pvc`**, aligning the gate with Proxmox's long-standing honest advertisement (its disks live on the node, so the pod-mounted pvc path can never work). `nfs` is **exempt from the relay-mode gate** (it runs node-side, not relay); the `s3` path keeps its relay-only enforcement.

### Why
Second provider of the NFS backend (ADR-0006 Slice 4). Proxmox now advertises and serves `nfs` as both source and target over the node-side qemu-img transport, mirroring the libvirt host-side model. With libvirt + Proxmox both serving `nfs`, the first end-to-end NFS migration paths are now wireable. NFS integrity for this transport is the node-side `qemu-img check` (qemu-img emits no in-stream byte checksum). vSphere (pod-side) follows next; lab validation against the OMS server comes after.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (proxmox provider image)
- [ ] Config change only

## [2026-06-19 14:15] - feat(libvirt): NFS migration via host-side qemu-img nfs:// (ADR-0006 Slice 4, #236)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/libvirt/nfs.go` (new): `exportDiskToNFS` / `importDiskFromNFS`. The libvirt HOST's `qemu-img` writes/reads the staged qcow2 **directly to/from the `nfs://` export over libnfs** (qemu's `block-nfs` driver) — no provider-pod hop, no S3 client, no host temp. Export = `qemu-img convert -U -f qcow2 -O qcow2 <vol> <nfs://…>`; import = `qemu-img convert <nfs://…> <pool-vol>` + `qemu-img check`. Reuses the existing SSH/disk-resolution machinery.
- `internal/providers/libvirt/server.go`: `ExportDisk`/`ImportDisk` accept `nfs` (new `migration.EnsurePVCS3OrNFSBackend`) and **exempt it from the relay-mode gate** (NFS uses qemu-img's native transport, not relay); `GetCapabilities` now advertises `nfs` in the export/import backends.
- `internal/storage/migration/backend.go`: NFS-inclusive gate + advertisement helpers (`EnsurePVCS3OrNFSBackend`, `EnsureS3OrNFSBackend`, `PVCS3AndNFS*`, `S3AndNFS*`).

### Why
First provider of the NFS backend (ADR-0006 Slice 4). libvirt now advertises and serves `nfs` as both source and target over the qemu-img transport. NFS integrity for this transport is the target-side `qemu-img check` (qemu-img emits no in-stream byte checksum). vSphere and Proxmox follow in the next slices; lab validation against the OMS server comes after.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider image)
- [ ] Config change only

## [2026-06-19 13:45] - feat(migration): controller routes the nfs backend (qemu-img native transport) (ADR-0006 Slice 4, #236)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/controller/vmmigration_controller.go`: the VMMigration controller now routes `storage.type: nfs` on its side:
  - `gateMigrationStorageBackend` accepts `nfs` and exempts it from relay/direct transfer-mode gating — NFS uses qemu-img's native `nfs://` transport (host-side for libvirt/Proxmox, pod-side for vSphere), not the stream-through-the-pod relay model.
  - `validateStorageConfig` validates `nfs.server`/`nfs.export` and runs the server through the **same SSRF host gate** as the S3 endpoint (ADR-0006 C3); NFS carries no credentials Secret.
  - `storageOptionsJSON` (renamed from `s3StorageOptionsJSON`) ships the NFS coordinates (server/export/path/uid/gid) in `storage_options_json`.
  - `generateStorageURL` builds the hardened `nfs://…/<stage>.qcow2` URL via `NFSURL` (C7'); the staged object is qcow2 and the import format is derived as qcow2.
  - The Validating-phase pre-flight skips the PVC cross-namespace check for nfs (it stages over the network, like s3).

### Why
Wires the controller for the NFS backend. NFS migrations now pass the controller's routing/validation; they still require the providers to advertise `nfs` and run the `qemu-img nfs://` transport (the next, per-provider slice), so end-to-end NFS is not yet active.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager)
- [ ] Config change only

## [2026-06-19 13:15] - feat(migration): NFS staging coordinates + hardened nfs:// URL builder (ADR-0006 Slice 4, #236)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/storage/migration/nfs.go` (new): `NFSURL(opts, key)` — the single, hardened construction site for the libnfs/qemu-img `nfs://<server><export>/<path>/<key>[?uid=&gid=]` URL (ADR-0006 condition C7'). It rejects URL query/fragment delimiters (`?`/`&`/`#`), whitespace and control characters, and `..` traversal in the server/export/path/key, and range-checks uid/gid — so a tenant-controlled NFS coordinate cannot smuggle libnfs URL options or escape the export when the string is handed to `qemu-img` on argv.
- `internal/storage/migration/options.go`: NFS fields on `StorageOptions` (`Server`, `Export`, `Path`, `UID`, `GID`) carried in the gRPC `storage_options_json`.
- `api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`: optional `uid`/`gid` on `NFSStorageConfig` (ADR-0006 C5), bounded to the AUTH_SYS uint32 range. CRDs + deepcopy regenerated.

### Why
Foundational, fully-unit-tested primitives for the qemu-img/libnfs NFS migration backend (the mechanism chosen at the security re-bless). No runtime behavior change yet — NFS stays gated off in the controller and providers; the wiring lands in the following slices.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (CRD update for the new `nfs.uid`/`nfs.gid` fields)
- [ ] Config change only

## [2026-06-19 12:30] - fix(security): migration storage host allowlist closes live S3-endpoint SSRF; gates NFS (ADR-0006 C3, #236)
**Author:** @wrkode (William Rizzo)

### Security
- `internal/storage/migration/addrpolicy.go` (new) + `internal/controller/vmmigration_controller.go`: a `VMMigration`'s `storage.s3.endpoint` is tenant-controlled and was dialed by the provider pod **with no host validation** — a live SSRF. A migration could point the pod at `169.254.169.254` (cloud-metadata), loopback, or an internal service, and the S3 SDK would present the configured credentials there. The controller now enforces a `HostPolicy` at the `Validating` phase: it **always** rejects loopback / link-local (including the metadata address) / unspecified / multicast targets, and — when the new allowlist is configured — anything outside it. Hostnames are resolved and **every** resolved address is checked (a validate-time DNS-rebind guard). RFC1918/private ranges stay permitted by default so on-prem storage works; regulated deployments should set the allowlist. The same gate is the hard prerequisite (ADR-0006 Slice 4, condition C3) for the NFS `direct`-mode backend, where the hypervisor host would dial the address — so it lands first.

### Added
- `cmd/manager/main.go`: `--migration-storage-allowed-hosts` flag (comma-separated IP/CIDR) wiring the allowlist into the VMMigration reconciler. Empty = permissive except the always-denied set.
- `api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`: `MaxLength` bounds on `S3StorageConfig.Endpoint` and `NFSStorageConfig.{Server,Export,Path}` (apiserver-side payload cap; CRDs regenerated).

### Why
The NFS migration security review (ADR-0006 Slice 4, C3) found the shipped S3 path dials an unvalidated, tenant-controlled endpoint — exploitable today — and that the NFS backend would extend the same exposure to the hypervisor host. This closes the live S3 SSRF and provides the reusable host gate the NFS slices build on.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager)
- [ ] Config change only

## [2026-06-19 11:30] - fix(vsphere): Reconfigure honors VMClass CPU/memory (wrong JSON keys) (#266)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/vsphere/server.go`: `Reconfigure` parsed `req.DesiredJson` (a marshaled `contracts.CreateRequest`) with the wrong keys — `class`/`cpus`/`memory` and `disks`/`size`. `VMClass`/`DiskSpec` carry **no** json tags, so the real keys are `Class.CPU`, `Class.MemoryMiB`, and `Disks[].SizeGiB`. Every key missed, so a VMClass CPU/memory change **silently no-opped while the RPC returned success** — and because the controller then treats the empty response as "completed synchronously" and updates status, the VM **reported the new size while the hardware stayed at the old one**, and drift detection never re-fired. Now parses the correct typed keys (mirroring the Create path and the libvirt/Proxmox providers), so `SupportsReconfigureOnline: true` is honest. Surfaced by the #261 cross-provider parity audit — the same wrong-key class that #261 P1-1 fixed for Proxmox.
- `internal/providers/proxmox/server.go`: fixed the identical wrong-key bug in the Proxmox `Reconfigure` **disk-resize** branch (`desired["disks"]["size"]` → `Disks[].SizeGiB`). CPU/memory were already fixed in #261 P1-1; the disk branch was latent dead code (the controller only triggers Reconfigure on CPU/mem drift today, but it would silently drop a disk resize once that is wired).
- Removed the now-unused `parseMemory` helpers from both providers — their only callers were the wrong-key paths; CPU/memory/size now arrive as numbers in the typed contract.

### Why
The Proxmox parity epic (#261) surfaced, via the cross-provider lens, that vSphere `Reconfigure` had the same wrong-JSON-key bug Proxmox just fixed — and worse, it corrupts status (reports a size the hardware never got). P0: a silently-wrong VM size in a regulated posture.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (vSphere + Proxmox provider images)
- [ ] Config change only

Closes #266.

## [2026-06-19 10:30] - docs(proxmox): align migration docs/examples with shipped parity; ExportCompression caveat (#261 P2-4)
**Author:** @wrkode (William Rizzo)

### Changed
- `README.md`: rewrote the **VM Migration** section — the S3/relay path is the validated model across **all three providers in any direction** (vSphere ↔ libvirt ↔ Proxmox); the example now uses `storage.type: s3` and PVC is demoted to compat-only. The old text claimed "only vSphere → Libvirt tested" and "pvc only — s3/http/nfs not implemented", both false post-#236/#261.
- `examples/migration/proxmox-to-libvirt.yaml`, `examples/migration/vsphere-to-proxmox.yaml`: rewrote from the stale, broken **PVC** shape to **S3/relay** (mirroring `vmmigration-proxmox-s3.yaml`). The Proxmox provider is S3-only, so the old `storage.type: pvc` examples failed capability validation against Proxmox.
- `examples/migration/README.md`: marked both Proxmox directions **Tested (ADR-0006 Slice 3)** over S3; corrected the model description (all-direction S3, Proxmox S3-only); documented that the Proxmox Provider Secret needs both API-token and SSH creds.
- `examples/vmmigration-basic.yaml`, `examples/vmmigration-advanced.yaml`: corrected headers — these are legacy PVC compat examples; S3 is the recommended/validated path; dropped the "only vSphere→libvirt tested / s3 not implemented" claims.

### Added
- `docs/adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md`: a **parity-hardening (#261)** note in Slice 3 — relay-mode is now enforced at the RPC (`EnsureRelayMode`, `InvalidArgument` on `direct`), and **P2-4**: `ExportCompression` is qcow2-only (`compress: true` has no effect on raw/vmdk exports). The qcow2-only caveat is also surfaced in the rewritten example headers.

### Why
Closes the last open items on the Proxmox parity epic (#261): the P2-4 ExportCompression caveat was undocumented, and several in-repo migration docs/examples still described the pre-parity world (pvc-only, "untested" Proxmox), actively contradicting the shipped behavior.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

## [2026-06-19 09:30] - fix(proxmox): collision-free VMIDs, real disk-info, drop key logging (#261 P2)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/proxmox/server.go` + `pveapi/client.go` (P2-2): VMIDs are now allocated from PVE's `GET /cluster/nextid` (new cached `GetNextVMID`, surfaced through a `nextVMID` helper used by all four allocation sites) instead of `time.Now().Unix() % 999999`. The timestamp scheme could collide on rapid creates and is not cluster-aware; `/cluster/nextid` returns a guaranteed-free id. The helper falls back to the timestamp (with a warning) only if the cluster API is unreachable.
- `internal/providers/proxmox/server.go` + `pveapi/client.go` (P2-1): `GetDiskInfo` now reports the disk's real size/used/format read from `GET /nodes/{node}/storage/{storage}/content` (new `GetStorageContent`) instead of fabricating them — the old code reported `actualSize == virtualSize` (no thin usage) and guessed the format as `qcow2`/`raw` from a substring of the config string. Falls back to the config-derived guesses if the storage API is unreachable, so it never hard-fails.

### Changed
- `internal/providers/proxmox/server.go` (P2-3): removed the leftover `DEBUG: …` INFO log lines in the `Create` image-parse path (raw `ImageJson` dump, parsed-map dump, "TemplateName not found") — request-payload noise that violated the structured-logging convention.

### Security
- `internal/providers/proxmox/server.go` + `pveapi/client.go` (P2-3): removed the `DEBUG SSH ...` log lines in the cloud-init SSH-key encoding path that dumped public-key material and its base64/URL-encoding into the provider log. Compliance posture forbids logging credential-adjacent material; the noisy `log/slog` import is gone with them.

### Why
Final slice of the Proxmox parity epic (#261), after P0 (#262), P1-correctness (#263) and P1-features (#264). These are the P2 polish items from the audit: collision-free cluster-aware VMID allocation, honest disk metadata for migration sizing, and removing credential-adjacent debug logging. Closes the epic.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (proxmox provider image)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-19 08:45] - fix(proxmox): Clone overrides, graceful-shutdown timeout, node discovery (#261 P1)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/proxmox/server.go` (P1-2): `Clone` now applies the requested VMClass sizing and placement instead of dropping them. A PVE clone inherits the source's cores/memory, so the `ClassJson` override is applied in a post-clone reconfigure (shared `applyVMClassSizing` helper, also used by the template-create path), and `PlacementJson` is honored — the target node (`Host`) is threaded into the clone and the post-clone sizing, and the target storage (`Datastore`) into the clone.
- `internal/providers/proxmox/server.go` + `pveapi/client.go` (P1-4): a `SHUTDOWN_GRACEFUL` power op now forwards `graceful_timeout_seconds` to the PVE shutdown endpoint and sets `forceStop=1` so it escalates to a hard stop when the deadline elapses, instead of ignoring the timeout. (New `PowerOperationWithParams`; the pveapi request builder already form-encodes params.)
- `internal/providers/proxmox/pveapi/client.go` + `server.go` (P1-4): node discovery. `FindNode` and `ListVMs` no longer assume a node literally named `pve` — when no `NodeSelector` is configured they discover the cluster's nodes via `GET /nodes` (new cached `ListNodes`). A wrong node name otherwise 500s every API call on a cluster whose node isn't named `pve`.

### Why
Continues the Proxmox parity epic (#261) after the P0 delete fix (#262) and the P1 sizing fix (#263): closes the remaining P1 feature gaps where Clone silently ignored overrides, graceful shutdown ignored the caller's timeout, and node addressing was hardcoded.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (proxmox provider image)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-19 08:05] - fix(proxmox): honor VMClass CPU/memory; enforce relay-only transfer (#261 P1)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/proxmox/server.go` (P1-1): `Create` and `Reconfigure` now read the VMClass CPU/memory from the correct keys. `ClassJson` is a marshaled `contracts.VMClass` (no JSON tags), so the keys are `CPU`/`MemoryMiB` — the old code read `cpus`/`memory` (and, in `Reconfigure`, the outer key `class` instead of `Class`), which never matched, so cores/memory stayed unset and **every Proxmox VM came up at the PVE default of 1 core / 512 MB regardless of its VMClass**. The fix is applied on both create paths: the diskless create (via `configToValues`) and the template clone (a PVE clone inherits the template's hardware, so `cores`/`memory` are now also set in the post-clone reconfigure). Mirrors the vSphere/libvirt parse of the same contract.
- `internal/providers/proxmox/server.go` (P1-3): `ExportDisk`/`ImportDisk` now call `migration.EnsureRelayMode(req.TransferMode)`, so a `direct` transfer request fails with `InvalidArgument` instead of silently running as `relay` — Proxmox advertises relay-only (ADR-0006 D2). Matches libvirt's enforcement.

### Why
The first P0 fix (#262) addressed the delete-orphan incident; this P1 slice fixes the next-worst Proxmox parity gap surfaced by the audit (#261): silently undersized VMs (confirmed on real hardware — migrated VMs showed `cores=None, memory=None` in PVE) and a silently-downgraded transfer mode.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (proxmox provider image)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-19 07:20] - fix(proxmox): delete no longer orphans VMs; finalizer retained on delete failure (#261 P0)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/virtualmachine_controller.go` (P0-2, cross-cutting): `handleDeletion` no longer removes the VirtualMachine finalizer when the provider `Delete` fails. The old code logged the error and continued (`// Continue with cleanup even if deletion fails`), so a failed delete deleted the CR while leaving the hypervisor VM running — orphaning it on **any** provider (a real compliance problem for the regulated posture). It now keeps the finalizer and requeues on a real failure, treats a provider `NotFound` as already-deleted (idempotent), and honors a new `virtrigaud.io/force-delete: "true"` annotation as an operator escape hatch for a permanently-unreachable provider. Adds a `contracts.IsNotFound` helper and unit tests.
- `internal/providers/proxmox/{server.go,pveapi/client.go}` (P0-1): the Proxmox `Delete` now stops a running VM first (Proxmox refuses to destroy a running VM — "VM <id> is running - destroy failed") and then destroys it with `purge=1&destroy-unreferenced-disks=1`, waiting for both tasks so a successful Delete means the VM is actually gone. Mirrors vSphere (power-off → Destroy) and libvirt (destroy → undefine). Also fixes the pveapi request builder to honor a query string instead of percent-encoding the `?` into the path. Fake PVE server now rejects destroying a running VM so the stop-first path is exercised by tests.
- `internal/providers/proxmox/capabilities.go` (P0-3): drop the dishonest `pvc` disk export/import backend advertisement — the legacy pvc path `os.Open`s node-local image paths the provider pod can never reach, so only `s3` is advertised. Adds `migration.S3Only{Export,Import}Backends`.

### Why
A `kubectl delete virtualmachine` of Proxmox VMs removed the Kubernetes resources but left the VMs running on the hypervisor. Root cause was two bugs (a Proxmox `Delete` that can't destroy a running VM, and a controller that drops the finalizer even when the provider delete fails). This is the P0 slice of the Proxmox feature-parity epic (#261), which audits Proxmox against vSphere/libvirt.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager + proxmox provider images)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-18 20:40] - migration: wire powerOffBeforeMigration + Proxmox matrix validated (ADR-0006)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/controller/vmmigration_controller.go`: implemented `source.powerOffBeforeMigration` (Bug H) — previously a no-op. When set, the Validating phase now (1) aligns the source VM's desired `spec.powerState` to `Off` so the VirtualMachine reconciler does not race the export by powering the source back on (which would re-lock a vSphere disk mid-export and leave a non-deleted source running after the move), then (2) issues a hard, reliable power-off and gates the export on the source reaching `Off`. This is **required** for a vSphere source (ESXi locks a running VM's disk, so the streamOptimized clone fails with "…vmdk … is locked") and yields a quiescent disk for libvirt/Proxmox. A graceful guest shutdown is a documented future refinement. Unit tests in `vmmigration_poweroff_test.go`.

### Fixed
- `internal/providers/libvirt/s3import.go` (Bug P): honor `req.Format` for the staged source format instead of hardcoding vmdk — a libvirt/Proxmox source stages qcow2, which the import previously rejected as an "invalid VMDK image descriptor".
- `internal/providers/proxmox/{server.go,s3import.go,pveapi/client.go}` (Bug Q): the migration `qm importdisk` target storage is now operator-configurable (`PROVIDER_DEFAULT_STORAGE`/`PVE_DEFAULT_STORAGE`) instead of a hardcoded `local-lvm`, so a dir-storage PVE node can be a migration target.
- `internal/providers/proxmox/s3import.go` (Bug R): resolve the imported disk's volid from `qm config` rather than assuming the LVM naming — a directory store names the volume `<storage>:<vmid>/vm-<vmid>-disk-0.<ext>`, so `qm set --scsi0` previously failed with "unable to parse directory volume name".
- `internal/providers/proxmox/server.go` (Bug U): `Describe` now returns the CRD power-state enum (`On`/`Off`) instead of lowercase `on`/`off`, which the manager rejected on the status write — stalling a migration into Proxmox even though the VM was created and running.
- `internal/providers/vsphere/server.go` (Bug W): guard a nil parent backing in `GetDiskInfo` — exporting a freshly-imported/migrated VM (a base disk with no backing chain) panicked the provider with a nil-pointer dereference.
- `internal/controller/vmmigration_controller.go` (Bug X): after a synchronous `ImportDisk`, the Importing→Creating transition returned with no requeue and relied solely on the status-write self-watch event to re-drive reconciliation — which can be coalesced or missed, wedging a migration in `Creating` forever (observed on the synchronous Proxmox import path). It now requeues after a short settle delay, which still lets the informer cache observe `Phase=Creating` (avoiding a stale-cache re-import) while guaranteeing the Creating phase runs.
- `internal/controller/vmmigration_controller.go` (Bug Y): `handleCreatingPhase` dereferenced `status.diskInfo` with no nil check. A migration that reached `Creating` without recorded disk info panicked on the nil pointer — and because controller-runtime recovers and requeues, it crash-looped on that object forever. It now fails the migration cleanly with a diagnosable message (`status.diskInfo is nil; cannot create target VM`) instead of panicking. Unit test in `vmmigration_creating_test.go`.

### Why
Validated the full any-direction S3 migration matrix for Proxmox end-to-end on real hardware — vSphere ⇄ Proxmox and libvirt ⇄ Proxmox all reach Ready — completing the three-hypervisor S3 phase of #236 (NFS remains). The power-off wiring is the v1 of source quiescing; the minimal-downtime variant (snapshot + change-block delta-sync + cutover) is tracked separately.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager + provider images)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-18 15:25] - feat(proxmox): S3 cross-hypervisor migration data path (ADR-0006)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/proxmox/ssh.go`: SSH DATA-plane transport to the PVE node (host derived from the SAME `PROVIDER_ENDPOINT` the API token uses; `root@pam` → node root). Ported from the libvirt provider: host-key verification policy (`PROXMOX_INSECURE_SKIP_HOST_KEY_VERIFICATION`, known_hosts hard-fail, #149), private-key materialization to `/tmp/virtrigaud-proxmox` at 0600/0700 (#249), sshpass (password) vs `ssh -i` (key) branches, ControlMaster multiplexing (`PROXMOX_SSH_DISABLE_MULTIPLEXING`, #194), and `runSSH`/`runSSHStdout`/`runSSHStdin` helpers. SSH creds loaded from `/etc/virtrigaud/credentials/{ssh_user,ssh_password,ssh_privatekey}` with `PROVIDER_*`/`PVE_*` env fallbacks. No secret is ever logged (paths only, `sshpass -e`, key via SSHPASS env).
- `internal/providers/proxmox/s3export.go`: SOURCE path — resolve the disk's on-node path via `pvesm path <storage:volid>`, flatten with `qemu-img convert -U -f <fmt> -O qcow2` (honoring `req.Compress` via `-c`), then stream `cat <hostTmp>` → S3 over an io.Pipe (SHA256 in-stream, ContentLength:-1). Mirrors libvirt's error-precedence surfacing when both the cat stream and the S3 upload fail.
- `internal/providers/proxmox/s3import.go`: TARGET path — stream the S3 qcow2 down to a node temp via `cat > <hostTmp>` (SHA256-verified), `qemu-img check` it, and return the node path. The Proxmox Create path (`createFromImportedDisk`) then builds a diskless VM shell and attaches the staged disk with `qm importdisk` + `qm set --scsi0 … --boot order=scsi0`; cloud-init (ide2/ciuser/sshkeys) is wired via the API `ReconfigureVMRaw` so multi-line SSH keys are url-encoded correctly (no `qm set --sshkeys` command-line pitfall).
- `internal/providers/proxmox/{ssh_test.go,s3_test.go}`: unit coverage for SSH arg construction (password vs key branch, host-key + multiplex options, SSHPASS-not-in-argv, key file perms), host derivation, `pvesm path` parsing, export/import command construction, the imported-disk Create handoff, and backend rejection/no-SSH guards.

### Changed
- `internal/providers/proxmox/server.go`: `ExportDisk`/`ImportDisk` now accept the S3 backend (`EnsurePVCOrS3Backend`) and dispatch to the S3 paths; `parseCreateRequest` captures a migration import's node `Path` from the image JSON into the new `VMConfig.ImportedDiskPath`; `Create` routes to `createFromImportedDisk` when that path is set; the `Provider` gains a nil-safe `ssh *sshTransport`.
- `internal/providers/proxmox/capabilities.go`: advertise pvc+s3 export/import backends (`PVCAndS3ExportBackends`/`PVCAndS3ImportBackends`) instead of pvc-only.
- `internal/providers/proxmox/pveapi/client.go`: add out-of-band `VMConfig.ImportedDiskPath`/`ImportedDiskFormat` (json:"-") for the importdisk Create path.
- `internal/providers/proxmox/provider_test.go`: capability assertions now expect the pvc+s3 backend set.
- `cmd/provider-proxmox/Dockerfile`: runtime base switched from static-distroless to `debian:bookworm-slim` with `openssh-client` + `sshpass` (+ ca-certificates/curl) and a non-root `app` user — the SSH data plane shells out to `ssh`/`sshpass`, which distroless cannot carry (the Proxmox analog of the vSphere qemu-img Dockerfile gap).

### Why
ADR-0006 cross-hypervisor migration over S3 was DONE for vSphere↔libvirt but Proxmox had no working remote disk transport — its export/import were PVC-only and used a local-filesystem stub that only worked if the pod ran on the PVE node. This adds the SSH-to-node data plane so Proxmox can be a migration source and target, with the migration controller already treating it as qcow2-native (no controller change).

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (provider image bump; S3 migration also needs SSH creds in the Provider's credentials Secret)
- [ ] Config change only
- [ ] Documentation only

---

## [2026-06-17 15:30] - test: regression coverage for the migration-PVC mount lifecycle (#230)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/controller/provider_controller_migration_test.go`: `TestDiscoverMigration_MergesAllActive_AndDropsRemoved` reproduces both #230 bugs and asserts the discovery-rebuild model fixes them — (1) two concurrent migration PVCs both yield a volume+mount (no last-writer-wins clobber), and (2) once a PVC is removed, the next discovery drops its volume/mount (no stale leftover). Locks in the behavior delivered by #232/#184.

### Why
#230 was structurally addressed by the discovery-rebuild model but lacked an explicit regression test for the parallel-merge and stale-removal scenarios; this verifies and guards them. Closes #230.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-17 14:55] - chart: render the observability templates (ServiceMonitor, PrometheusRule, Grafana dashboard)
**Author:** @wrkode (William Rizzo)

### Added
- `charts/virtrigaud/templates/servicemonitor.yaml`: a Prometheus-Operator `ServiceMonitor` selecting the manager Service's `metrics` port (`/metrics`), gated on `observability.prometheus.{enabled,serviceMonitor.enabled}` **and** the `monitoring.coreos.com/v1` CRD being present (so an install without the operator skips it cleanly rather than failing the apply). `interval`/`scrapeTimeout`/extra labels come from values.
- `charts/virtrigaud/templates/prometheusrule.yaml`: a `PrometheusRule` with four starter alerts that reference only metrics the manager actually exports — `VirtRigaudManagerDown` (`absent(virtrigaud_build_info)`), `VirtRigaudProviderCircuitBreakerOpen` (`virtrigaud_circuit_breaker_state == 2`), `VirtRigaudHighReconcileErrorRate` (error-outcome reconcile ratio > 20%), `VirtRigaudProviderRPCErrors` (non-OK RPC ratio > 10%). Same value + CRD gate.
- `charts/virtrigaud/templates/grafana-dashboard-configmap.yaml` + `charts/virtrigaud/dashboards/virtrigaud-dashboard.json`: a Grafana dashboard ConfigMap (9 panels) labelled `grafana_dashboard: "1"` for the Grafana sidecar to auto-import, gated on `observability.grafana.{enabled,dashboards.enabled}`.

### Why
The `observability` values block (and its `serviceMonitorLabels`/`prometheusRuleLabels`/`grafanaDashboardLabels` helpers) already existed, but **no template rendered any of it** — setting `observability.prometheus.serviceMonitor.enabled=true` produced nothing, so operators wiring kube-prometheus-stack had to hand-roll a ServiceMonitor. Closes #228.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only
- [ ] Documentation only
## [2026-06-17 13:03] - libvirt: drop hard-coded qemu emulator path
**Author:** @jing2uo (Komh)

### Fixed
- `internal/providers/libvirt/provider_virsh.go`: `generateDomainXMLWithStorage` no longer hard-codes `<emulator>/usr/bin/qemu-system-x86_64</emulator>` in the generated domain XML. That path only exists on Debian/Ubuntu; on RHEL/CentOS the QEMU binary lives at `/usr/libexec/qemu-kvm`, so `Provider/Create` failed on those hosts. The `<emulator>` element is now omitted, and libvirt's qemu driver fills in the host's default emulator for the domain's `(arch, machine, type)` during define-time post-parse — the same resolution `virt-install` relies on. Because the provider talks to a possibly-remote libvirtd over SSH, the remote daemon resolves its own emulator, which is both simpler and more correct than reimplementing the selection in the provider.

### Why
The provider must run unchanged against heterogeneous libvirt hosts. Hard-coding the Debian emulator path broke VM creation on RHEL-family hosts. Verified against a CentOS host (libvirt 4.5.0) and an Ubuntu host (libvirt 8.0.0): with the element omitted, each host's `dumpxml` shows its own emulator (`/usr/libexec/qemu-kvm` and `/usr/bin/qemu-system-x86_64` respectively) and `Provider/Create` succeeds.

## [2026-06-17 11:00] - libvirt: lighten connection validation to avoid gRPC deadline
**Author:** @jing2uo (Komh)

### Fixed
- `internal/providers/libvirt/provider.go` + `internal/providers/libvirt/virsh.go`: `Provider.Validate` and `VirshProvider.testConnection` no longer call `listDomains`, which issued one `virsh domstate` per domain on top of the initial `virsh list`. Over the `qemu+ssh://` transport every virsh call is a separate SSH round-trip, so that N+1 grows linearly with the number of domains on the host. `Validate` runs under the manager's 30s gRPC deadline and is invoked on every VM reconcile (and by the runtime resolver as a health check before reusing a provider client), so on a host with only ~40 domains the cold-SSH N+1 (~37s measured) exceeded the deadline and returned `DeadlineExceeded` — leaving VMs stuck `NotReady` in a 5s requeue loop and forcing the resolver to evict and reconnect. Both paths now run a single `virsh list --all --name` and count non-empty lines; the per-domain state the old code fetched was only used for a log count, never consumed.

### Why
Validation/readiness only need a reachability check, not per-domain state. Collapsing the N+1 to one command makes the cost O(1) and independent of how many VMs run on the host. Measured on two hosts (CentOS 7 / libvirt 4.5.0 with 44 domains; Ubuntu 22.04 / libvirt 8.0.0 with 38 domains): the old path took ~36–37s of cold-SSH round-trips against a 30s deadline, while the single `virsh list` is one round-trip.

## [2026-06-17 10:05] - Keep the libvirt SSH private key off node disk (memory-backed volume)
**Author:** @wrkode (William Rizzo)

### Security
- `internal/controller/provider_controller.go`: libvirt provider pods now get a dedicated **memory-backed (tmpfs) emptyDir** mounted at `/tmp/virtrigaud-libvirt` (the directory the libvirt provider materialises its SSH private key into, added in #249). Previously that path lived under the provider's main `/tmp`, a plain disk-backed `EmptyDir{}`, so a key written from the `credentialsSecretRef` Secret (itself tmpfs) would also persist at rest on the node's disk. The new volume is `Medium: Memory` with a small `4Mi` sizeLimit and is **scoped to libvirt providers** so the main `/tmp` stays disk-backed for multi-GB migration disk staging (a memory-backed `/tmp` would OOM vSphere's qcow2→vmdk conversion).

### Why
Hardening for the libvirt key-based SSH auth path: key material should never touch the node's disk. Surfaced during the security review of the libvirt key-auth fix (#249/#248). The mount path matches the libvirt provider's `sshPrivateKeyDir`.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (provider pods re-templated; libvirt provider pods gain one volume)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-17 08:57] - libvirt SSH private-key authentication works end-to-end
**Author:** @jing2uo (Komh)

### Fixed
- `internal/providers/libvirt/virsh.go`: `setupConnection` now materialises the SSH private key to `/tmp/virtrigaud-libvirt/ssh-privatekey` (0600 in a 0700 dir) and pins `keyfile=&sshauth=privkey` on the `qemu+ssh://` URI, so libvirt's own transport authenticates with the key. The `~/.ssh/config` is written for **every** ssh URI (previously only when a password was set), and a write failure is a hard error on the key path. The `!` direct-exec handler and the standard virsh wrapper now branch password→`sshpass` / key→`ssh -i <keyfile>` / else local through a shared `sshKeyAuthOptions()` + `resolveSSHKeyFile()`; previously the key case fell through to **local** execution inside the pod, so every host command silently ran in the container.
- `internal/providers/libvirt/sshhostkey.go`: `sshConfigStanza` emits `PubkeyAuthentication yes` (was `no`), which had disabled key auth for the config-reading transport.
- `internal/providers/libvirt/server.go`: `copyDiskToRemote`'s key branch passes `-i <keyfile>` instead of a bare `scp` that could never authenticate; `ImportDisk` runs `qemu-img info` against the remote copy (`finalSourcePath`) rather than the pod-local `sourcePath`.
- `internal/providers/libvirt/s3import.go`: `runSSHStdin`'s key branch passes `-i <keyfile>`.
- `internal/providers/libvirt/storage.go` + `internal/providers/libvirt/provider_virsh.go`: `pool-define` and `define` run on the remote host via `runRemoteVirshCommand` (so `virsh` reads the host-local XML the provider wrote); they failed client-side under key auth before.
- `internal/providers/libvirt/cloudinit.go`: `copyISOToRemote` uses host-side `! cp`/`! mkdir`/`! chmod` instead of hand-rolled `! ssh`/`! scp`; removed the now-unused `getSSHTarget()`.

### Added
- `internal/providers/libvirt/virsh.go`: `remoteVirshConnectURI` derives `qemu:///{system,session}` from the connection URI and `runRemoteVirshCommand` pins it with `-c`, so a remote `virsh` targets the same libvirtd whether the ssh user is root (system default) or non-root (session default) — also fixes the pre-existing password + non-root case.
- `internal/providers/libvirt/sshkeyauth_test.go`: unit tests for key materialisation (content + 0600/0700 perms), `setupConnection` pinning `keyfile=`/`sshauth=privkey` on the key path and NOT on the password path, the `sshKeyAuthOptions` arg list, `resolveSSHKeyFile`, the `PubkeyAuthentication` flip, and `remoteVirshConnectURI` across the root/non-root × system/session quadrants.

### Why
The libvirt provider's private-key auth path was broken end-to-end (key never written, URI missing `keyfile=`, `PubkeyAuthentication no`, direct ssh/scp missing `-i`, and `define`/`pool-define` reading host files client-side). Only the password path worked, so a key-only Provider could not connect, import disks, or define VMs. This is the authentication counterpart to ADR-0004's host-key verification work on the same transport. Closes #248.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (provider image — the fix is in the libvirt provider)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-16 20:15] - Remove dead parseStorageSize (orphaned by the Bug G GetDiskInfo rewrite)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/libvirt/provider_virsh.go`: removed the now-unused `parseStorageSize` helper. The Bug G rewrite of `GetDiskInfo` switched to `qemu-img info -U --output=json` (which returns the virtual size directly), removing the only caller; `golangci-lint`'s `unused` linter flagged it (`go vet` does not catch unused functions). No behavior change.

### Why
CI Lint failed on the Slice 2 branch with `func parseStorageSize is unused`; the function was orphaned when its caller was replaced. Refs #236.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-16 19:50] - Docs: ADR-0006 Slice 2 validated; reverse migration example + README
**Author:** @wrkode (William Rizzo)

### Changed
- `docs/adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md`: added an *Implementation log — validated slices* section recording the as-built slicing (Slice 1 vSphere→libvirt and Slice 2 libvirt→vSphere over S3/`relay`, both end-to-end validated) and the Slice 2 findings: NFC streamOptimized import (not `CopyVirtualDisk`), `qemu-img` in the provider image, lease device-URL host rewrite to vCenter, `Unregister`-not-`Destroy` + folder idempotency, the cross-hypervisor `target.networks` requirement, and streaming robustness. Plus deferred follow-ups.
- `examples/migration/libvirt-to-vsphere.yaml`: rewritten from the obsolete "untested / PVC-only / S3-not-supported" placeholder into the **validated S3 reverse-migration example**, including a `VMNetworkAttachment` + `target.networks` so the migrated vSphere VM gets a NIC.
- `examples/migration/README.md`: replaced the stale "PVC-only / only vSphere→libvirt tested" constraints with the storage-agnostic S3 model; both vSphere↔libvirt S3 directions marked tested; S3 quick start.

### Why
ADR-0006 Slice 2 (libvirt→vSphere over S3) is implemented and validated on real hardware; the docs and example must reflect the working path and steer users away from the no-NIC trap. Refs #236.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

## [2026-06-16 19:35] - Remove dead, misleading build/Dockerfile.provider-{vsphere,libvirt}
**Author:** @wrkode (William Rizzo)

### Removed
- `build/Dockerfile.provider-vsphere`, `build/Dockerfile.provider-libvirt`: deleted. Neither is referenced by the Makefile, the release workflow, or any build script — the canonical provider images are built from `cmd/provider-<name>/Dockerfile` (and `build/Dockerfile.manager`/`Dockerfile.kubectl` remain the canonical manager/kubectl builds). The stale `build/Dockerfile.provider-vsphere` was actively wrong: it used a plain `gcr.io/distroless/static` runtime with **no `qemu-img`**, so anyone who built from it would get a vSphere provider that fails every ADR-0006 S3 import with `qemu-img is not available in the provider image`. The canonical `cmd/provider-vsphere/Dockerfile` already ships `qemu-utils` on a `debian:bookworm-slim` base, so the released image is correct; this just removes the trap.

### Why
While validating the Slice 2 reverse migration, a hand-built dev image based on the stale Dockerfile lacked `qemu-img` and broke the import. Removing the dead files leaves one source of truth per provider image and prevents the same mistake. Refs #236.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [x] Documentation only

## [2026-06-16 19:20] - vSphere S3 import: make the NFC import idempotent against a leftover folder
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/vsphere/s3import.go` (`nfcImportStreamOptimized`): before importing, best-effort delete the entire `[<ds>] <id>` import folder (Force-overwrite intent), not just the target `<id>.vmdk` file. The import id is deterministic per migration (the controller uses `<target>-migrated`), so a retry after a prior attempt — or after a deleted target VM that left its disk folder behind — found the folder occupied; `ImportVApp` then placed the disk in a collision-suffixed folder (`<id>_1`), and the post-import disk-uuid query on the expected `[<ds>] <id>/<id>.vmdk` path failed with `File … was not found` ("likely corrupt import"). Deleting only the `.vmdk` was insufficient when the folder still held a prior `.vmx`/`.nvram`.

### Why
Surfaced re-running the ADR-0006 Slice 2 migration to the same target after an earlier validation run: the second import failed not because the disk was corrupt but because a stale datastore folder forced a name collision. Retries to a fixed target name must start from a clean folder. Refs #236.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-16 18:10] - Surface the real S3 error on a libvirt export stream failure (un-mask)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/libvirt/s3export.go` (`exportDiskToS3`): when BOTH the host-side `cat` stream and the S3 upload reported an error, the code surfaced only the `cat` error — but that is almost always a downstream symptom (a failed S3 upload closes the pipe's read end, which breaks `cat` with SIGPIPE, and ssh then reports a bare `exit status 255` with no stderr). It now reports both, leading with the upload error, so the actual root cause (e.g. an S3 endpoint returning `503 service unavailable` or `erasure write quorum failed` when its disk is full) is no longer hidden behind `exit status 255`.

### Why
Found while validating the ADR-0006 Slice 2 reverse migration: a libvirt→vSphere export kept failing with an opaque `host-side stream (cat …) failed: exit status 255 (stderr: )`. The masking turned a one-line storage diagnosis into a multi-step investigation. Refs #236.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-16 18:00] - vSphere S3 import: drive the NFC lease by hand, rewrite device URL to vCenter
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/vsphere/s3import.go` (`ImportDisk`): replaced the `vmdk.Import` call with a hand-driven `HttpNfcLease` import (`nfcImportStreamOptimized`). `vmdk.Import` uploads to the lease's device URL, which vCenter populates with the **ESXi host FQDN** (e.g. `esxi.lab.k8`); a remote provider pod reaches vCenter by IP (`PROVIDER_ENDPOINT`) and typically cannot resolve that ESXi name via cluster DNS, so the streamOptimized upload failed with `lookup esxi.lab.k8 … no such host`. The new path rewrites each lease device URL host to the vCenter host (which proxies NFC and is always reachable from the pod), and `Unregister`s — rather than `Destroy`s — the transient import VM (vCenter 8 faults `Destroy` of a freshly imported VM with `file … is attached to vm`). Net result is identical: `[<ds>] <id>/<id>.vmdk` left as a native thin disk.

### Why
ADR-0006 Slice 2's vSphere import target could acquire the NFC lease but never complete the upload from an in-cluster provider pod, because the lease's device URL is unresolvable there. This is the final data-path blocker for the reverse (libvirt→vSphere) migration. Refs #236.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-16 17:20] - Fix `createSnapshot: false` being impossible to set (defaulted-bool footgun)
**Author:** @wrkode (William Rizzo)

### Fixed
- `api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`: removed `omitempty` from `MigrationSource.CreateSnapshot` (kept `+kubebuilder:default=true`). The non-pointer bool with `omitempty` + a `true` default silently flipped an explicit `createSnapshot: false` back to `true` on the controller's status round-trip (same footgun class as the `tls.enabled` fix in #235), so a snapshot-free migration (e.g. of a powered-off source, or a multi-disk cloud-image VM whose CDROM breaks `--disk-only` snapshots) could not be requested.

### Why
Found while validating the ADR-0006 Slice 2 reverse migration: a `powerOffBeforeMigration: true` + `createSnapshot: false` run still attempted (and failed) a snapshot.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-16 16:05] - ADR-0006 Slice 2 — reverse-direction (libvirt→vSphere) controller wiring
**Author:** @wrkode (William Rizzo)

### Added
- `api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`: additive `MigrationDiskInfo.TargetPath` status field — the provider-native path the target provider's `ImportDisk` returns (e.g. `[datastore1] <id>/<id>.vmdk` for vSphere). Regenerated `config/crd/bases/infra.virtrigaud.io_vmmigrations.yaml` (the only schema delta; `zz_generated.deepcopy.go` unchanged — plain `string`).
- `internal/controller/vmmigration_controller.go`: direction-agnostic disk-format derivation keyed off **provider type** — `nativeDiskFormat`/`stagedImportFormat`/`landedTargetFormat` (vSphere→`vmdk`, libvirt/proxmox/unknown→`qcow2`), with `diskFormatQcow2`/`diskFormatVMDK` constants. One source of truth so the import-format and target-format derivations can never disagree.
- `internal/controller/vmmigration_direction_test.go`: unit tests for (a) reverse libvirt→vSphere threading `Format=qcow2` to import + propagating `importResp.Path` to `TargetPath` + labeling `TargetFormat=vmdk`; (b) forward vSphere→libvirt unchanged (vmdk + libvirt pool path); (c) creating phase copying `TargetPath`→`ImportedDisk.Path`; (d) the provider-type→format table. Uses the `providerInstanceFn` seam.

### Fixed
- `internal/controller/vmmigration_controller.go` (`handleImportingPhase`): the s3 import format was hard-coded to `vmdk` (forward-path only). It is now derived from the **source** provider type, so the reverse path correctly threads `qcow2`. The source-checksum→`ImportDiskRequest.ExpectedChecksum` threading from Slice 1 is preserved.
- `internal/controller/vmmigration_controller.go` (`handleImportingPhase`): the controller dropped `importResp.Path`; it now records it in `Status.DiskInfo.TargetPath`. `TargetFormat` is derived from the **target** provider type (was hard-defaulted to `qcow2`; resolves to `qcow2` for a libvirt target so the forward path is byte-identical, `vmdk` for a vSphere target).
- `internal/controller/vmmigration_controller.go` (`handleCreatingPhase`): the created target VM's `Spec.ImportedDisk.Path` is now set from `Status.DiskInfo.TargetPath`.
- `internal/controller/virtualmachine_controller.go`: the imported-disk path resolution now treats the synthesized `/var/lib/libvirt/images/<id>.<fmt>` form as an explicit libvirt-only **last resort** (only when `ImportedDisk.Path` is empty) and documents that vSphere targets always carry an explicit path. No behavior change when a path is set.

### Why
ADR-0006 Slice 2's provider data-paths (libvirt→S3 qcow2 export, vSphere←S3 import) were committed but the controller still assumed the forward (vSphere→libvirt) staged format and dropped the target disk path — so every reverse libvirt→vSphere migration would have imported with the wrong format and then attached a bogus libvirt path on a vSphere target. This wires the controller to be direction-agnostic by deriving format/path from provider type. Refs #236.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager image — CRD additive field + controller logic)
- [ ] Config change only
- [ ] Documentation only

### Notes
- vSphere space precheck: there is no target-side capacity gate today; nothing asserts the imported disk's used size fits. A code comment in `handleImportingPhase` documents that any future gate must budget the **virtual** (provisioned) size for vSphere targets, because Approach 2's `CopyVirtualDisk` materializes as eagerZeroedThick on VMFS.
- The generic `gateMigrationStorageBackend` already permits libvirt→vSphere now that the provider capability sets advertise `s3` in both directions; it was verified, not changed.

---

## [2026-06-16 14:30] - ADR-0006 Slice 2 — libvirt→S3 export & vSphere←S3 import (reverse relay) provider data-paths
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/libvirt/s3export.go`: new libvirt SOURCE data-path `exportDiskToS3` for the reverse relay (libvirt→S3→vSphere). Requires an `ssh://` transport; resolves the source disk via `GetDiskInfo`; **flattens** the (possibly migration-snapshot-overlay) backing chain into one standalone qcow2 on the host with `qemu-img convert -f qcow2 -O qcow2 <src> <hostTmp>` (reads the full chain — not just the overlay); then streams `cat <hostTmp.qcow2>` host→pod→S3 via an `io.Pipe` coupling the new `runSSHStdout` to `storage.UploadStream` (SHA256 in-stream). Always best-effort `rm -f <hostTmp>` (deferred). Reports `Format = qcow2` (the staged object). New helpers `runSSHStdout` (the symmetric sibling of `runSSHStdin` — `cmd.Stdout = w`, same host-key/ControlMaster/sshpass handling) and `hostExportStagePath`.
- `internal/providers/vsphere/s3import.go`: new vSphere TARGET data-path `importDiskFromS3` (Approach 2, de-risked GREEN on lab vCenter 8.0.2). Downloads the staged qcow2 to `/tmp/<id>.qcow2` (SHA256 verified vs `ExpectedChecksum`); converts qcow2→**monolithicSparse** vmdk (`monolithicSparse` is **mandatory** — streamOptimized is rejected by this ESXi at both the NFC and VirtualDiskManager layers); uploads the staged vmdk via the vCenter **datastore-HTTP** endpoint (`DatastoreFileManager.UploadFile`, never a direct-ESXi URL); inflates it to a native thin disk with `VirtualDiskManager.CopyVirtualDisk`; verifies via `QueryVirtualDiskUuid` (empty/error = corrupt → fail); deletes the staging vmdk and, on any post-upload failure, best-effort deletes both staging and final. New helper `resolveImportDatastore` (datastore name first, StoragePod/SDRS fallback).
- `internal/diskutil/qemu_img.go`: new `ConvertOptions.Subformat` field — when set, `Convert()` emits `-o subformat=<Subformat>` (takes precedence over the `Compression` convenience mapping). The vSphere import passes `Subformat: "monolithicSparse"` explicitly rather than relying on the qemu-img default. Refactored the convert argument assembly into a pure, unit-testable `buildConvertArgs`.
- Tests: `internal/providers/libvirt/s3export_test.go` (nil-provider, ssh-required, stage-path containment/quoting), `internal/providers/vsphere/capabilities_test.go` (s3-import passes-gate→`Unavailable` not `Unimplemented`; nfs stays `Unimplemented`), `internal/diskutil/qemu_img_test.go` `TestBuildConvertArgs` (Subformat emission + precedence).

### Changed
- `internal/providers/libvirt/server.go`: `ExportDisk` gate flipped from `EnsurePVCBackend` to `EnsurePVCOrS3Backend` + `EnsureRelayMode`; `s3` now dispatches to `exportDiskToS3`. `GetCapabilities.SupportedExportBackends` flipped to `PVCAndS3ExportBackends()`.
- `internal/providers/vsphere/server.go`: `ImportDisk` gate flipped from `EnsurePVCBackend` to `EnsurePVCOrS3Backend`; `s3` now dispatches to `importDiskFromS3`. `GetCapabilities.SupportedImportBackends` flipped to `PVCAndS3ImportBackends()`.
- `internal/providers/libvirt/server_test.go`: inverted `TestServer_ExportDisk_S3BackendUnimplemented` → `TestServer_ExportDisk_S3BackendPassesGate` (s3 now passes the gate, fails later at nil-provider, not `Unimplemented`); added `TestServer_ExportDisk_S3DirectModeRejected`; updated the storage-backends capability assertion.

### Why
ADR-0006 Slice 2 makes the cross-hypervisor migration bidirectional: libvirt becomes a SOURCE and vSphere a TARGET over the S3 relay. Owning BOTH ends in one change keeps the format contract consistent — libvirt stages **qcow2**, vSphere converts qcow2→monolithicSparse-vmdk locally and inflates via CopyVirtualDisk. This is the provider-boundary half; the controller wiring lands as a separate step. Refs #236.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (provider images — libvirt and vSphere providers)
- [ ] Config change only
- [ ] Documentation only

---

## [2026-06-16 11:14] - Honor CleanupPolicy + reliably remove the migration source snapshot
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/vmmigration_controller.go`: `deleteSourceSnapshot` now **awaits** the provider snapshot-delete task instead of fire-and-forget. After `SnapshotDelete` returns a `taskRef`, it polls `IsTaskComplete` with a bounded `wait.PollUntilContextTimeout(ctx, 3s, 2m, immediate)` (no `time.Sleep`), then checks `TaskStatus` and returns an error if `.Error != ""` — mirroring the existing `handleExportingPhase` await. The previous code logged "best effort — don't wait" and returned `nil`, so on CR deletion (`handleDeletion` calls it then removes the finalizer) the async delete was orphaned and the snapshot survived. This was the primary cause of the **4 accumulated `*-migration-*` snapshots** observed on the source VM after a live vSphere→libvirt run.
- `internal/controller/vmmigration_controller.go`: `CleanupPolicy` is now consulted. New helper `cleanupAllowed(m)` returns `false` only when `Spec.Options.CleanupPolicy == "Never"`. `handleReadyPhase` gates both the intermediate-storage cleanup and the source-snapshot deletion behind `cleanupAllowed`, and `handleDeletion` gates the snapshot deletion the same way. Previously all three ran unconditionally, so `Never` was silently ignored. `DeleteAfterMigration` stays independent of policy (it is an explicit user action).
- `internal/controller/vmmigration_controller.go`: `handleFailedPhase` now performs **best-effort** source-snapshot cleanup at both terminal exits (no-retry-policy and max-retries-exceeded) via new `cleanupSnapshotOnTerminalFailure`, gated on `cleanupAllowed && Status.SnapshotID != "" && Spec.Source.SnapshotRef == nil`. A migration that fails before `Ready` (the lab had no retry policy → immediately terminal) previously left its snapshot until the CR was deleted (and even then was orphaned by the bug above). A cleanup failure here is logged and swallowed so it cannot wedge an already-terminal migration; the await makes it usually succeed on the first pass.
- `internal/controller/vmmigration_controller.go`: on a successful delete, `deleteSourceSnapshot` clears `Status.SnapshotID = ""` as an idempotency latch, so re-reconciles and `handleDeletion` skip a now-absent snapshot instead of re-issuing a delete against a stale ID.

### Added
- `api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`: exported Go constants `CleanupPolicyAlways`, `CleanupPolicyOnSuccess`, `CleanupPolicyNever` mirroring the existing `+kubebuilder:validation:Enum` on `MigrationOptions.CleanupPolicy`. Purely additive — **no CRD schema change** (regen produces zero diff); they remove bare string literals from the controller per the CLAUDE.md global-constants rule.
- `internal/controller/vmmigration_snapshot_cleanup_test.go`: unit tests (counting fake provider, `providerInstanceFn` seam) proving `deleteSourceSnapshot` polls the task to completion and surfaces a task error, clears `SnapshotID` on success, `Never` skips snapshot deletion on `Ready`, a terminally-failed migration deletes its snapshot under `OnSuccess`/`Always` but not `Never`, and terminal cleanup is best-effort (a delete failure does not wedge the migration). Plus a table test for `cleanupAllowed`.

### Why
A live vSphere→libvirt migration accumulated 4 source snapshots on real hardware, traced to three defects in the migration controller's snapshot cleanup: an orphaned async delete on CR deletion, an ignored `CleanupPolicy`, and no snapshot cleanup at terminal failure. The migration-created snapshot is an internal artifact left on the user's LIVE source VM, so it must be removed at any terminal state (Ready or Failed) unless the user opted out with `Never`. This is PR-1 of ADR-0006 Slice 2 prep and also fixes the FORWARD path; scope is the snapshot only (intermediate-storage-on-failure policy is out of scope). Refs #236.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager image — the reconciler runs in the manager)
- [ ] Config change only
- [ ] Documentation only

---

## [2026-06-16 06:55] - Fix VMMigration reconcile race issuing a duplicate ExportDisk/ImportDisk
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/vmmigration_controller.go`: `handleExportingPhase` no longer returns `ctrl.Result{Requeue: true}` after the synchronous `ExportDisk` completes. The vSphere export is synchronous and ~16 minutes on real hardware; the immediate requeue re-entered `Reconcile` before the status write propagated to the informer cache, so the early guard read a stale `Phase=Exporting`/`ExportID==""` and issued a **second** `ExportDisk` (observed twice in the lab, ~1s apart). The handler now returns `ctrl.Result{}, nil` and relies on the status-update watch event (no status-filtering predicate in `SetupWithManager`) to re-drive reconcile with a fresh, consistent cache. The synchronous `handleImportingPhase` (`ImportDisk`, which writes the target qcow2) got the identical fix.
- `internal/controller/vmmigration_controller.go`: hardened both long-op guards so a still-stale cache cannot re-issue the RPC. The export guard now treats `ExportID != ""` **OR** the durable `Exporting` condition being `True` **OR** an in-memory in-flight marker as "already exported → advance to Importing" (import guard mirrors this with the `Importing` condition). Before issuing `ExportDisk`/`ImportDisk` the reconciler claims a process-local `sync.Map` marker keyed by `UID/generation/op`; a duplicate reconcile that finds the marker already set skips the RPC and re-drives. The marker is released if the RPC errors so a legitimate retry can proceed. The marker is intentionally not persisted: on a manager restart it is lost, but the durable `ExportID`/condition is then consistent, so the persisted-state guard prevents a duplicate.

### Added
- `internal/controller/vmmigration_export_race_test.go`: regression tests proving a single migration drives **exactly one** `ExportDisk`/`ImportDisk` across an immediate stale-cache re-reconcile (counting fake provider returning a per-call-distinct checksum). Covers: no-immediate-requeue return, in-memory-guard-only suppression, durable-condition-guard suppression with an empty in-memory map (manager-restart model), the import-side counterpart, and a full-`Reconcile`-driven no-immediate-requeue assertion.

### Why
The exported streamOptimized VMDK is not byte-deterministic (same size, different SHA256). A duplicate export overwrites the staged S3 object with new bytes, while its `updateStatus` conflicts and drops — leaving `Status.DiskInfo.SourceChecksum` from export #1 but the object from export #2. The libvirt import then fails checksum verification (`expected != actual`), failing the whole migration. The invariant restored: the staged object and the recorded checksum always come from the same `ExportDisk` call.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager image — the reconciler runs in the manager; a manager dev image rebuild is required to validate in the lab)
- [ ] Config change only
- [ ] Documentation only

---

## [2026-06-16 06:10] - Fix libvirt S3 disk import: stage vmdk to a seekable host file before convert
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/libvirt/s3import.go`: `importDiskFromS3` no longer streams the S3 vmdk straight into `qemu-img convert -f vmdk /dev/stdin`. qemu-img's vmdk+file driver requires a seekable **regular file** (it seeks to read the streamOptimized footer/grain directory) and rejects a non-seekable pipe with `'file' driver requires '/dev/stdin' to be a regular file`. The import now uses a two-step host flow: **stage** the object via `cat > <hostTmp>` (sequential, pipe-friendly), then **convert** `qemu-img convert -f vmdk -O qcow2 <hostTmp> <target>.qcow2` against the seekable file. Bytes still flow S3 → pod → SSH → host (no CSI PVC), and the transfer SHA256 is still verified in-stream during the stage (ADR D5).
- `internal/providers/libvirt/s3import.go`: the previous single-pipe design masked the real failure — qemu-img died in <100ms, closed the pipe read end, and the code reported the resulting `io: read/write on closed pipe` download error instead of the qemu-img message. Stage and convert are now separated so the **real** cause is surfaced: a stage download/checksum failure is reported as such; a convert/check failure surfaces qemu-img's **stderr** directly (new `qemuImgStderr` helper).
- `internal/providers/libvirt/s3import.go`: the staged temp file is removed **unconditionally** (deferred `rm -f`, WARN on failure) so a failed import never leaks a multi-GB vmdk on the host. The temp lives in the pool directory (`<poolPath>/.virtrigaud-import-<vol>-<unixts>.vmdk`) so the convert stays intra-device (new `hostStagePath` helper).

### Added
- `internal/providers/libvirt/s3import_test.go`: host-independent tests for the staged-import flow — nil-provider and `ssh://`-transport guards, `hostStagePath` (pool co-location, dot-prefix, sanitized-name containment, distinct-from-target, trailing-slash normalization), `cat >` path quoting, and `qemuImgStderr` surfacing.

### Why
In the live ADR-0006 Slice 1 lab run (vSphere→S3→libvirt), the libvirt TARGET import failed instantly with a masked "closed pipe" error. Direct testing with qemu-img 8.2.2 confirmed qemu-img cannot read a vmdk from a non-seekable pipe; staging to a seekable host file before converting fixes the import and lets the real error through. The full vmdk lands transiently on the host (host disk = vmdk + qcow2 during convert); true streaming/`direct` mode is the ADR-0006 follow-up.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider image; relies on `qemu-img` already shipped)
- [ ] Config change only
- [ ] Documentation only

---

## [2026-06-16 05:26] - Fix vSphere ExportDisk uploading descriptor-only VMDK (multi-file flatten)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/vsphere/server.go`: `ExportDisk` now flattens a multi-file VMDK (tiny text descriptor + separate data extent(s), e.g. a `-sesparse.vmdk` delta produced when exporting a snapshot of a running VM) into a single self-contained streamOptimized VMDK with `qemu-img` and uploads THAT, instead of uploading only the ~586-byte descriptor with no disk data. The reported `Checksum`/`EstimatedSizeBytes` are now computed over exactly the uploaded flattened object.
- `internal/providers/vsphere/server.go`: new `flattenVMDK` helper runs `qemu-img convert -f vmdk -O vmdk -o subformat=streamOptimized <descriptor> <flattened>`; it verifies `qemu-img` is present and fails loudly with the `qemu-img` stderr on failure. Single-file (no separate extents) VMDKs are uploaded as-is; qcow2/raw exports continue to convert directly (qemu-img already resolves the extents).

### Added
- `internal/providers/vsphere/export_disk_test.go`: regression tests for the flatten path — multi-file (descriptor + extent) flatten with byte-for-byte disk-content integrity, single-file no-op flatten, and loud-fail when `qemu-img` is absent. The qemu-img-dependent cases skip when the binary is unavailable.

### Why
In a live lab migration (ADR-0006 Slice 1, vSphere→S3→libvirt), vCenter presented the running firewall's snapshot disk as a descriptor + a `-sesparse.vmdk` extent. The export downloaded both but uploaded only the 586-byte descriptor, so the target's `qemu-img convert -f vmdk` produced garbage. Flattening before upload guarantees a single importable object with the actual disk data.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (vSphere provider image; relies on `qemu-img` from qemu-utils, already shipped)
- [ ] Config change only
- [ ] Documentation only

---

## [2026-06-15 19:49] - ADR-0006 Slice 1: vSphere → S3 → libvirt cross-hypervisor transfer (relay)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/storage/s3.go`: new minio-go-backed S3 backend implementing the `Storage` interface against any S3-compatible store (rustfs/MinIO/Ceph RGW/AWS). Streaming `UploadStream` (auto-multipart `PutObject`) and `DownloadStream` (`GetObject`) compute SHA256 in-stream via `io.TeeReader`/`io.MultiWriter`. `http://` endpoint ⇒ plaintext; path-style via `usePathStyle`; credentials come from the request, never config/logs.
- `internal/storage/storage.go`: `Storage` interface gains `UploadStream`/`DownloadStream` (+ `StreamUploadRequest`/`StreamDownloadRequest`); `StorageConfig` gains an `S3` block; `NewStorage` now constructs the `s3` backend.
- `internal/storage/pvc.go`: PVC backend implements the new streaming methods via file I/O (keeps `pvc` working).
- `internal/storage/migration/options.go`: shared `StorageOptions` (storage_options_json) marshal/parse and S3 credential-map key constants (`accessKeyID`/`secretAccessKey`/`sessionToken`).
- `internal/storage/migration/storageconfig.go`: `S3StorageConfigFromRequest` builds a provider-side `storage.StorageConfig` from the gRPC options+credentials (shared by vSphere and libvirt).
- `internal/storage/migration/backend.go`: `PVCAndS3Export/ImportBackends`, `RelayOnlyTransferModes`, `EnsurePVCOrS3Backend`, `EnsureRelayMode` (explicit `direct` ⇒ `InvalidArgument`, never a silent downgrade).
- `internal/providers/libvirt/s3import.go`: libvirt TARGET S3 import — download from S3 and stream over SSH **stdin** into host-side `qemu-img convert -f vmdk -O qcow2 /dev/stdin <pool>/<vol>.qcow2` (target-owned conversion, ADR D4), then `qemu-img check` (ADR D5) and `pool-refresh`. New `runSSHStdin` streams stdin (pipe), never buffering the disk.
- Tests: S3/PVC streaming + checksum round-trip, options/creds/gate/url controller tests, vSphere/libvirt capability flips, and an S3-credential redaction no-leak test (`internal/obs/logging`).

### Changed
- `internal/providers/vsphere/server.go`: `ExportDisk` accepts `backend_type=s3` (SOURCE) — stages the native vmdk and streams it to S3 with in-stream SHA256; `GetCapabilities` now advertises export `[pvc,s3]` (import stays `[pvc]`).
- `internal/providers/libvirt/server.go`: `ImportDisk` accepts `backend_type=s3` (TARGET) via `importDiskFromS3`; `GetCapabilities` advertises import `[pvc,s3]` (export stays `[pvc]`).
- `internal/controller/vmmigration_controller.go`: resolves `transferMode auto→relay` (rejects explicit `direct`); builds the `s3://bucket/prefix/<key>` URL; loads creds from `S3StorageConfig.credentialsSecretRef` into the gRPC `credentials` map; passes `storage_options_json`; per-direction gate now ALLOWS vSphere(export=s3)→libvirt(import=s3) in relay while the reverse direction still fails fast; skips PVC creation + the cross-namespace PVC check for the s3 backend.
- `internal/obs/logging/logging.go`: redactor explicitly lists the S3 credential keys.
- `examples/vmmigration-s3.yaml`: rewritten into a real vSphere→libvirt rustfs example (`http://rustfs.lab.k8:9000`, `usePathStyle: true`, `credentialsSecretRef`, sample Secret with placeholder keys).
- `go.mod`/`go.sum`: add `github.com/minio/minio-go/v7`.

### Why
ADR-0006 Slice 1 delivers the first real cross-hypervisor disk transfer, proving the storage-backend-agnostic, pod-as-S3-client architecture end-to-end without a shared filesystem or a CSI PVC the hypervisor host cannot see. Scope is intentionally one direction (vSphere SOURCE → libvirt TARGET); capabilities are advertised honestly per direction so unsupported combinations fail fast at Validating.

### Scope / follow-ups
- vSphere→libvirt only. Reverse direction, Proxmox, NFS, and the `direct` transfer mode are later slices and are rejected with an ADR-referencing message.
- vSphere export stages the vmdk to a temp file in the pod before the S3 upload (govmomi datastore download), then streams that file to S3; a fully streaming vCenter→S3 export is a follow-up.
- minio auto-multipart satisfies "multipart now"; full crash-resume (UploadId/part state in Status) is OUT of scope — a failed transfer retries whole.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (new provider behavior; namespaced `get` on the S3 credentials Secret)
- [ ] Config change only
- [ ] Documentation only

### Usage
```yaml
apiVersion: v1
kind: Secret
metadata: { name: s3-migration-credentials, namespace: default }
stringData:
  accessKeyID: "..."
  secretAccessKey: "..."
---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMMigration
spec:
  source: { vmRef: { name: prod-app-server } }         # vSphere-backed VM
  target: { name: prod-app-server-migrated, providerRef: { name: libvirt-provider } }
  storage:
    type: s3
    transferMode: relay
    s3:
      bucket: virtrigaud
      endpoint: http://rustfs.lab.k8:9000
      usePathStyle: true
      credentialsSecretRef: { name: s3-migration-credentials }
```

---

## [2026-06-11 14:05] - ADR-0006 Slice 0: storage-backend-agnostic migration surface (additive)
**Author:** @wrkode (William Rizzo)

### Added
- `api/infra.virtrigaud.io/v1beta1/vmmigration_types.go`: `MigrationStorage` gains `Type` enum `pvc;nfs;s3` (default `pvc`), `TransferMode` enum `auto;relay;direct` (default `auto`), and `NFS`/`S3` sub-configs. New `S3StorageConfig` (bucket/endpoint/region/prefix/`credentialsSecretRef`/`usePathStyle`) and `NFSStorageConfig` (server/export/path/`readOnly`). `usePathStyle`/`readOnly` are defaulted bools without `omitempty` (PR #235 footgun). Credentials are referenced via Secret, never inline.
- `api/infra.virtrigaud.io/v1beta1/provider_types.go`: `ReportedCapabilities` gains `SupportedExportBackends`, `SupportedImportBackends`, `SupportedTransferModes` (empty == pvc-only / relay-only).
- `proto/provider/v1/provider.proto`: additive fields — `ExportDiskRequest`/`ImportDiskRequest` `backend_type=8`, `transfer_mode=9`, `storage_options_json=10`; `GetCapabilitiesResponse` `supported_export_backends=14`, `supported_import_backends=15`, `supported_transfer_modes=16`. Added the host-vs-pod execution-contract comment block (bytes never traverse gRPC; relay = host->pod->backend, direct = host->backend).
- `internal/storage/migration/backend.go`: new shared package with backend/transfer-mode constants, honest pvc-only/relay-only advertisement helpers, and `EnsurePVCBackend` (returns `codes.Unimplemented` for non-pvc) — the single source of truth used by every provider and the controller.
- `sdk/provider/capabilities/capabilities.go`: `ExportBackends`/`ImportBackends`/`TransferModes` builder methods + Manager fields, mirroring the existing `SetSupportedExportFormats` pattern.

### Changed
- `internal/providers/{vsphere,libvirt,proxmox,mock}`: `GetCapabilities` now advertises the honest status quo — export/import backends `["pvc"]`, transfer modes `["relay"]`. `ExportDisk`/`ImportDisk` on vsphere/libvirt/proxmox reject any non-pvc `backend_type` with `Unimplemented`; empty `backend_type` keeps today's behavior.
- `internal/controller/vmmigration_controller.go`: the Validating phase fails fast (via `transitionToFailed`) when `storage.type` is `nfs`/`s3`, or when the requested backend/transfer mode is not in both the source provider's export set and the target provider's import set, with an actionable, ADR-referencing message. Export/Import requests now carry `backend_type` (default `pvc`) and `transfer_mode` (default `auto`).
- `internal/providers/contracts/` and `internal/transport/grpc/client.go`: the transport-agnostic `Capabilities`/`ExportDiskRequest`/`ImportDiskRequest` types and the manager-side client mapping carry the new fields end-to-end.

### Why
First slice of ADR-0006: establish the CRD/proto/capability surface for storage-backend-agnostic (NFS/S3) any-direction cross-hypervisor migration, and make every provider report honestly what it supports. This slice adds NO transfer logic — every non-pvc path returns `Unimplemented` and is rejected at validation.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

> Additive and backward-compatible: existing `pvc` migrations are unchanged. New CRD fields are optional with safe defaults; new proto fields are additive (next free numbers). A not-yet-rolled provider reporting an empty capability set is treated as the implicit pvc-only/relay-only default, so pvc migrations are never wrongly blocked.

## [2026-06-11 06:10] - Keep `tls.enabled: false` durable on Provider (defaulted-bool footgun)
**Author:** @wrkode (William Rizzo)

### Fixed
- `api/infra.virtrigaud.io/v1beta1/provider_types.go`: Removed `omitempty` from `ProviderTLSSpec.Enabled` and `ProviderHealthCheck.Enabled` (both keep `+optional` and `+kubebuilder:default=true`). A non-pointer bool with `omitempty` **and** a default of `true` silently flips an explicit `false` back to `true` on any controller `Update`: `omitempty` drops the `false` on serialization, then the apiserver re-applies the default. For `tls.enabled=false` this meant an explicitly-plaintext provider would, after the next reconcile that re-writes its spec, fail the TLS posture gate (`enabled=true` + no `secretRef`) and wedge to `runtime.phase=Failed` with a healthy pod still running.
- `api/infra.virtrigaud.io/v1beta1/provider_tls_marshal_test.go`: New round-trip tests asserting `Enabled=false` survives JSON marshal for both specs.

### Why
An operator who explicitly opts a provider out of TLS could see it silently flip back on and stop reconciling. The CRD OpenAPI schema is unchanged (the fields stay optional and default to `true`); only the Go serialization is corrected, so existing objects are unaffected.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-11 04:34] - Don't hold VM create for already-present image sources (#227)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/virtualmachine_image_prepare.go`: A VM referencing a **reference-style** `VMImage` source — a libvirt pool-file `path`, an existing vSphere `templateName`/`contentLibrary`, or an existing Proxmox `templateID`/`templateName` — with `prepare.onMissing: Fail` was held in `WaitingForDependencies` forever, waiting for an import that has nothing to import. `EnsureImageOnProvider` now classifies the source via a new `imageSourceNeedsPrepare` helper and proceeds straight to create for already-present references (the by-reference create path consumes them directly). Import-style sources (libvirt/vSphere URLs, HTTP/registry/DataVolume pulls) are unchanged — they still prepare, and `onMissing: Fail` still holds them. Ambiguous sources (e.g. a libvirt source with both a path and a URL) prefer running the idempotent prepare, so no real import is ever silently skipped.
- `internal/controller/virtualmachine_image_prepare_test.go`: Added a source-classification table test plus #227 regression tests (libvirt path + vSphere template with `onMissing: Fail` now skip the hold).

### Why
Discovered during the v0.3.9 lab E2E: a libvirt path-based image (the qcow2 already sits on the host) was held even though there is nothing to prepare. A wrong path/template now fails honestly at create time — where a missing backing artifact belongs — instead of being pre-held by the prepare gate.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-11 04:07] - Fix stale Makefile builder image (Go version drift)
**Author:** @wrkode (William Rizzo)

### Fixed
- `Makefile`: Bumped `BUILDER_IMAGE ?= golang:1.25` → `golang:1.26.4` to match `go.mod` (`go 1.26.4`). The Makefile passes `BUILDER_IMAGE` as a `--build-arg` to the Dockerfiles (overriding their already-correct `golang:1.26.4` default), so the stale value made a plain `make docker-build` fail with `go: go.mod requires go >= 1.26.4 (running go 1.25.11; GOTOOLCHAIN=local)`. Added a comment to keep it in lockstep with `go.mod`.

### Why
Local container builds via `make docker-build`/`make docker-buildx` were broken unless the caller manually passed `BUILDER_IMAGE=`. CI and the release pipeline were unaffected — they build with `docker/build-push-action` against the Dockerfile (whose default is already `golang:1.26.4`) and resolve their Go toolchain from `go.mod` via `go-version-file`.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-10 16:36] - Fix migration PVC-mount handshake: non-blocking, diagnosable, co-location-safe
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/vmmigration_controller.go`: Replaced the blocking 5-minute `time.Sleep` poll (`waitForProvidersReady`/`waitForProviderReady`/`waitForProviderBasicReady`) that waited for both providers to mount the migration storage PVC with a single-shot, non-blocking check (`migrationProvidersMounted`/`providerMountReady`) plus `RequeueAfter`. The wait deadline is now derived from the PVC's creation timestamp so it survives requeues, and a timeout fails with an actionable message (the exact `kubectl` command to inspect) instead of an opaque "timeout waiting for provider pods" error. Removes a reconcile-worker-blocking anti-pattern that could wedge the migration.
- `internal/controller/vmmigration_controller.go`: Added a fail-fast co-location guard for PVC-based migrations (#229). A migration whose source provider, target provider, and the migration itself are not all in one namespace now fails immediately with a clear message — a Kubernetes pod cannot mount a PVC across namespaces, so the transfer could never succeed and previously hung for the full timeout.
- `internal/controller/provider_controller.go`: `discoverMigrationPVCs`/`discoverMigrationVolumeMounts` no longer swallow the PVC `List` error silently; a denied List (missing RBAC) or an unsynced cache is now logged at error level with the namespace and label selector, so an un-mountable migration PVC is diagnosable instead of silently stranding the VMMigration controller (#231 hardening).
- `internal/controller/vmmigration_mount_test.go`: New table-driven unit tests for the single-shot mount evaluation, the source/target aggregate, the PVC-age deadline, and the cross-namespace fail-fast path.

### Why
Live cross-provider migration could hang for five minutes and then fail with an opaque timeout. The mount handshake blocked a reconcile worker with `time.Sleep` (violating the project's no-`time.Sleep`-for-synchronization rule), the cross-namespace topology was structurally impossible yet un-guarded, and a failed PVC `List` in the provider controller was invisible. Manager PVC RBAC is already present at HEAD (chart commits `40a6b26`/`ed852c6`), so this is the handshake reliability + diagnosability fix layered on top of it.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-09] - v0.3.9: libvirt feature parity + end-to-end image preparation
**Author:** @wrkode (William Rizzo)

Release roll-up. This release closes the libvirt capability gaps and makes image
preparation reachable end-to-end through CRs across all providers. Full provider
capability parity is achieved (umbrella #204 closed) with a single intentional
exception — Proxmox export compression, tracked as a low-priority follow-up (#219).

### Headline features
- **libvirt Clone** (#153) — qcow2 overlay (linked) + full-copy clones, same-provider; resolves the real backing-disk path (fix #207).
- **End-to-end image preparation** (#154) — `VMImage.spec.prepare` drives lazy, VM-create-time image import across libvirt / vSphere (real OVA/OVF URL import as template) / Proxmox; the controller writes `VMImage.status` (single-writer) and Create consumes the prepared location. See `docs/image-preparation.md` + ADR-0005.
- **libvirt online disk expansion** (#201) — live `virsh blockresize` (grow-only) + best-effort in-guest filesystem grow via the guest agent.
- **libvirt memory-inclusive snapshots** (#202) — RAM-inclusive checkpoints for a running VM; stopped VMs honestly downgrade to disk-only.
- **libvirt online CPU/memory reconfigure** (#203) — live `setvcpus/setmem` for VMs created with `cpuHotAddEnabled`/`memoryHotAddEnabled` (headroom provisioned at create, up to a ~4× ceiling). See the [Libvirt provider guide](https://projectbeskar.github.io/virtrigaud/providers/libvirt/).
- **Capability accuracy** (#198/#199/#200) — Proxmox advertises disk export/import; libvirt honors export compression; vSphere advertises memory snapshots.

### Upgrade notes
- **Requires cluster rollout.** Rebuild/redeploy the manager and provider images (providers are CR-managed via `spec.runtime.image`).
- **Online CPU/memory reconfigure on libvirt requires the hot-add flags set on the `VMClass` at create time.** VMs created without them (including all pre-v0.3.9 VMs) still require a power-cycle to change CPU/memory. The emitted domain XML is byte-identical to v0.3.8 when the flags are off.
- **No CRD or proto breaking changes.** `VMImage.status` gained additive image-prepare fields; `Provider.status.reportedCapabilities` reflects the new flags.

The detailed per-change entries follow below.

## [2026-06-09 13:10] - proxmox export compression (#219)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/proxmox/server.go`: `exportNeedsConversion` helper — a pure, host-independent decision function that determines when the export path must run a `qemu-img` convert pass. It forces a pass when the target format differs from the source, or when `Compress=true` and the target is `qcow2` (qemu-img applies `-c` only during a convert pass).
- `internal/providers/proxmox/provider_test.go`: `TestExportNeedsConversion` table test covering qcow2/raw/vmdk × compress combinations (no live Proxmox/qemu required).

### Changed
- `internal/providers/proxmox/server.go`: `ExportDisk` now honors `ExportDiskRequest.Compress`. Previously it hardcoded `Compression: false` and only converted when the format changed, so qcow2→qcow2 exports never compressed. It now forces a `qemu-img convert -c` pass for qcow2 targets when `Compress=true` and passes `req.Compress` through to `diskutil`.
- `internal/providers/proxmox/capabilities.go`: advertise `ExportCompression()` and update the stale "does not compress today" comment to describe what is actually compressed (qcow2 only).
- `internal/providers/proxmox/provider_test.go`: `TestProxmoxProvider_GetCapabilities` now asserts `SupportsExportCompression` is `True`.

### Why
The Proxmox provider implemented `ExportDisk` but silently ignored the `Compress` request flag, and the capability was deliberately withheld to stay honest (#153/#154/#204 posture: a `true` capability means the provider actually performs the operation). This makes export compression real for the common qcow2 case and only then advertises it. Compression is genuine for qcow2 only; raw has no container to compress into and vmdk stream-optimized output is not produced on this path, so those targets stay uncompressed even when `Compress=true` — documented in both the capability comment and the helper.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

## [2026-06-09 08:15] - libvirt clone hardening: UEFI nvram re-point + hot-add headroom preservation (#208, #221)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/libvirt/clone.go`: `rewriteNVRAMPath` derives a fresh per-clone UEFI varstore path from the SOURCE varstore's directory plus a `<targetName>_VARS.fd` basename, and `copyClonedNVRAM` copies the actual varstore file on the libvirt host (`sudo cp -f -- <src> <dst>` via the `!` direct-exec convention) so the clone gets an independent set of UEFI variables. `rewriteDomainXMLForClone` now also returns the source/target nvram paths it rewrote (empty for a BIOS source).
- `internal/providers/libvirt/clone.go`: `cloneClassOverride`/`cloneClassPerfProfile` local structs that mirror the **v1beta1 `VMClassSpec`** shape the clone controller actually marshals — `cpu` (int), `memory` (a `resource.Quantity` such as `"8Gi"`, converted to MiB via `Memory.Value()/1MiB` to match the manager), and `performanceProfile.{cpuHotAddEnabled,memoryHotAddEnabled}` — so `applyClassOverrides` reads the real class JSON.
- `internal/providers/libvirt/clone_test.go`: host-independent tests — UEFI nvram re-point (non-default dirs, `template=` attribute preserved, bare `<nvram/>` and missing-element no-ops), BIOS no-op, and hot-add headroom (hot-add class emits `<vcpu current=...>` ceiling + `<memory>`-ceiling > `<currentMemory>`; no flags = plain form unchanged; CPU-only leaves memory plain).

### Changed
- `internal/providers/libvirt/clone.go`: for a UEFI source the clone's per-VM `<os>...<nvram>` varstore is re-pointed to the fresh path and the source varstore is copied to it on the host; a failed copy logs a loud WARN rather than silently leaving the clone pointing at the source's varstore. A BIOS source (no `<nvram>`) is unchanged.
- `internal/providers/libvirt/clone.go`: `applyClassOverrides` now renders `<vcpu>`/`<memory>`/`<currentMemory>` via the create-path `buildCPUMemoryXML` helper (#203), so a clone created with a hot-add-capable class keeps online-reconfigure headroom. When the flags are absent/false the emitted XML is byte-identical to before (no regression).

### Fixed
- `internal/providers/libvirt/clone.go`: a clone with a class override now actually honors the **memory** override. The previous `applyClassOverrides` unmarshalled into a struct whose memory field expected an int `memoryMiB`, but the clone controller marshals the v1beta1 `VMClassSpec` where memory is `"memory": "<quantity>"` — so the value never bound and the memory override (and, with #221, the memory headroom) silently did nothing. The override is now parsed as a `resource.Quantity`. The previous unit tests masked this by feeding a synthetic `memoryMiB` JSON that never occurs in production; they now use the real `"memory": "8Gi"` shape.

### Why
The clone XML rewrite was incomplete in three ways: (1) it kept the source's absolute UEFI `<nvram>` varstore path, so a UEFI clone shared the source's varstore — a define-time conflict or silent corruption of boot order / Secure Boot state — and never created its own; (2) `applyClassOverrides` emitted the plain no-headroom resource form, silently stripping a hot-add-capable clone of the live CPU/memory grow capability provisioned for non-clone VMs (#203); (3) the memory override never bound because the parse struct didn't match the v1beta1 memory-quantity shape the controller sends. This closes the clone-headroom follow-up called out in the #203 entry and makes the memory side of #221 actually functional end-to-end.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

Rebuild/redeploy the libvirt provider image. BIOS clones are unaffected. UEFI clones now get an independent varstore copy. Clones created with a hot-add-capable class now retain online-reconfigure headroom (the emitted XML differs only when the class sets the hot-add flags). No CRD or proto change.

### Notes
- The nvram path-derivation and resource-XML rewrite stay pure/string-only (fully unit-testable without a host); the side-effecting varstore `cp` happens in `Clone()` next to the disk copy, gated on the non-empty nvram paths returned by the rewrite.
- Headroom preservation reuses `buildCPUMemoryXML`/the ceiling helpers from `reconfigure_online.go` (#203) — no duplicated policy.

## [2026-06-09 07:52] - libvirt online CPU/memory reconfigure: hotplug headroom + live setvcpus/setmem (#203)
**Author:** @wrkode (William Rizzo)

### Changed
- `internal/providers/libvirt/provider_virsh.go`: the create-path now provisions hotplug headroom in the domain XML when the VMClass opts into `CPUHotAddEnabled`/`MemoryHotAddEnabled`. CPU becomes `<vcpu placement='static' current='<initial>'>=ceiling</vcpu>` (extra vCPUs start offline, brought online by `setvcpus --live`); memory becomes `<memory>=ceiling` (balloon maximum) with `<currentMemory>=initial` (so `setmem --live` can inflate up to the ceiling). When hot-add is disabled the emitted XML is byte-identical to before (`<vcpu placement='static'>N</vcpu>`, `<memory>==<currentMemory>==initial`) — no regression for existing/most VMs.
- `internal/providers/libvirt/provider_virsh.go`: the `Reconfigure` online CPU/memory `--live` failure logs are now honest — they state the desired increase exceeds the provisioned hotplug headroom (the `<vcpu>` max / `<memory>` balloon maximum) or the VM was created without the hot-add flag, and that a power cycle is required. The CPU/memory comparison and `requiresRestart` fallback logic are unchanged.
- `internal/providers/libvirt/server.go`: `GetCapabilities` now advertises `SupportsReconfigureOnline: true` — online CPU/mem reconfigure via `setvcpus/setmem --live` for VMs created with the hot-add flags (headroom provisioned at create), up to the ~4× ceiling, beyond which a power cycle is required.

### Added
- `internal/providers/libvirt/reconfigure_online.go`: `buildCPUMemoryXML` helper plus `computeHotplugCeilingVCPUs`/`computeHotplugCeilingMemoryMiB` with named, tunable constants — `hotplugResourceMultiplier` (4×) and `maxHotplugVCPUs` (64). The ceiling is floored to be strictly greater than the initial (so headroom always exists) and the vCPU ceiling is hard-capped; memory has no hard cap (the guest only allocates `currentMemory`).
- `internal/providers/libvirt/reconfigure_online_test.go`: host-independent table tests for the CPU/memory XML helper (hot-add off = unchanged layout/no `current=`; hot-add on = `<vcpu current='<initial>'>` with ceiling max and memory ceiling > currentMemory) and the ceiling computation (4× multiplier, floor, CPU cap, no-headroom-at-cap).
- `internal/providers/libvirt/server_test.go`: assert `SupportsReconfigureOnline` is now advertised.

### Why
KVM/QEMU supports live CPU and memory hotplug, but the prior libvirt create-path provisioned zero headroom (`<memory>==<currentMemory>`, `<vcpu>` with no `current<max`), so the existing `setvcpus/setmem --live` path could never grow a running VM past its boot size and the capability flag was understated as `false`. Provisioning the headroom at create (opt-in via the existing hot-add flags, no CRD/proto change) lets the live path actually grow the VM and lets the provider honestly advertise online reconfigure.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout
- [ ] Config change only
- [ ] Documentation only

Rebuild/redeploy the libvirt provider image. This **changes the domain XML for VMs created with `CPUHotAddEnabled`/`MemoryHotAddEnabled`** (headroom is provisioned at create time); VMs created without those flags are unaffected (byte-identical XML). No CRD or proto change.

### Notes
- Online grow is bounded by the ~4× ceiling provisioned at create; growing beyond the ceiling still requires a power cycle, and the `--live` failure log says so.
- Headroom only exists for VMs created with the hot-add flags **after** this change — existing VMs (and VMs created without the flags) have no headroom and still need a power-cycle to grow CPU/memory.
- Memory live grow inflates the balloon up to `<memory>`; it is balloon-based (`currentMemory`), not DIMM hotplug. The guest sees the new memory as the balloon deflates.
- Follow-up: the clone path (`clone.go` `applyClassOverrides`) rewrites `<vcpu>`/`<memory>`/`<currentMemory>` and would drop the `current<max` headroom on a clone; preserving headroom across clone is a separate change and out of scope here.
## [2026-06-09 07:48] - libvirt memory-inclusive snapshots: advertise SupportsMemorySnapshots (#202)
**Author:** @wrkode (William Rizzo)

### Changed
- `internal/providers/libvirt/server.go`: advertise `SupportsMemorySnapshots=true`. `SnapshotCreate` already captures RAM for a running VM (full system checkpoint via `virsh snapshot-create-as` *without* `--disk-only`); the flag was understated. Extracted a testable `buildSnapshotCreateArgs` helper, and when a memory snapshot is requested for a **non-running** VM it now logs a clear WARN and creates a disk-only snapshot (honest downgrade — a stopped VM has no RAM state to capture) instead of silently behaving as if memory were captured.

### Added
- `internal/providers/libvirt/snapshot_test.go`: unit tests for the args helper (memory vs disk-only across running/stopped) + the `SupportsMemorySnapshots=true` capability.

### Why
KVM supports full-system checkpoints; the libvirt provider already created them when `IncludeMemory` was set on a running VM, but advertised the capability as `false`. This makes the flag honest (mirrors the vSphere memory-snapshot honesty fix #200).

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider image; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

### Notes
- Memory snapshots require the VM running; the captured RAM state lives inside the qcow2 (≈ RAM size, so larger/slower than a disk-only snapshot). A memory snapshot requested for a stopped VM is downgraded to disk-only with a WARN.

## [2026-06-09 07:41] - libvirt online disk expansion: live blockresize + best-effort guest FS grow (#201)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/libvirt/disk_expand.go`: online disk-grow path for the libvirt provider. Resolves the primary disk target and current capacity from the live domain (`domblklist` + `domblkinfo`, NOT the `<vmid>-disk` volume-name guess), applies a grow-only/idempotency guard, persists the larger size to the backing volume, runs `virsh blockresize <dom> <target> <n>G` so live QEMU exposes the new size immediately, then best-effort extends the in-guest partition/filesystem via the guest agent (`growpart` → `resize2fs`/`xfs_growfs`).
- `internal/providers/libvirt/disk_expand_test.go`: host-independent unit tests for target-device parsing, capacity parsing, the grow-only guard, the blockresize size-arg, and the FS-grow command sequence.

### Changed
- `internal/providers/libvirt/provider_virsh.go`: `Reconfigure` disk block now branches on domain state — running → `growDiskOnline` (live block-device resize is fatal on failure; in-guest FS grow is non-fatal); stopped → existing offline volume resize for next boot. CPU/memory paths unchanged.
- `internal/providers/libvirt/server.go`: `GetCapabilities` now advertises `SupportsDiskExpansionOnline: true`.
- `internal/providers/libvirt/server_test.go`: assert the online-disk-expansion capability is advertised.

### Why
KVM/QEMU supports growing a running VM's block device live via `blockresize`; the provider already resized the qcow2 volume but never told live QEMU or the guest, so the capability flag was honestly understated as `false`.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider image)
- [ ] Config change only
- [ ] Documentation only

No CRD or proto change.

### Notes
Grow-only: shrinks are rejected and resizing to the current size is a no-op (libvirt/qcow2 cannot shrink a live device). The in-guest filesystem grow is best-effort and gated on the QEMU guest agent — without it (or for non-standard partition layouts) the block device is still grown but the operator must finish the FS grow via cloud-init or manually.

## [2026-06-09 07:35] - ImagePrepare returns the prepared location; Create consumes it (#154, PR-6 / #214)
**Author:** @wrkode (William Rizzo)

### Changed
- `proto/provider/v1/provider.proto`: new `ImagePrepareResponse{task=1, prepared_image_id=2, prepared_image_path=3}`; `rpc ImagePrepare` now returns it (was `TaskResponse`). **Wire-compatible / non-breaking** — `task` stays field 1, so the bytes are identical to the `TaskResponse` it replaces (verified: both `Task` are `bytes,1`); mirrors the `CloneResponse` precedent. Regenerated `proto/rpc/provider/v1/*.pb.go` via `make proto` (do not hand-edit).
- `internal/providers/contracts/image.go`: `ImagePrepareResponse` gains `PreparedImageID` + `PreparedImagePath`.
- `internal/providers/libvirt/{image.go,server.go}`: internal `imagePrepare` now returns `(preparedID, preparedPath)`; the pool path (`<poolPath>/<target>.qcow2`) is threaded out and returned as `prepared_image_path` (id = target name), known even on the idempotent no-op and sync paths.
- `internal/providers/vsphere/image.go`: returns `prepared_image_id` = the (verified or imported) template name; path empty (vSphere clones templates by name).
- `internal/providers/proxmox/image.go`: returns `prepared_image_id` = the template name/VMID; on the **async** import path the deterministic id is returned in the SAME response as the `TaskRef` (location-at-trigger), not after the task.
- `internal/providers/mock/provider.go`: returns deterministic id/path so conformance/tests can assert.
- `internal/transport/grpc/client.go`: `PrepareImage` maps `prepared_image_id/path` → `contracts.ImagePrepareResponse.{PreparedImageID,PreparedImagePath}` (TaskRef + `trackTaskStart` unchanged).
- `internal/controller/virtualmachine_image_prepare.go`: stamps the prepared location onto `VMImage.status.ProviderStatus[provider]{ID,Path}` — alongside `PrepareTaskRef` on the async path (Available stays false until the task completes), or with `Available=true` via the extended `markImagePrepared(id,path)` on the sync/completion path.
- `internal/controller/virtualmachine_controller.go`: **consume** the prepared image at create — when `ProviderStatus[provider].Available`, `buildCreateRequest` overrides the source via `overrideImageWithPreparedLocation` (libvirt → `image.Path` + clear `URL`; vSphere → `image.TemplateName` + clear OVA `URL`; Proxmox → `image.TemplateName`). Falls back to the original source resolution when not prepared (no regression). `createVM`/`reconfigureVM`/`buildCreateRequest` thread the provider name.
- `sdk/provider/client/client.go`: passthrough `ImagePrepare` return type updated to `*ImagePrepareResponse`.

### Why
Closes the image-prepare loop (#154): the manager prepared an image on a provider but `Create` still re-resolved the original source (re-downloading a URL per VM). The provider now reports WHERE it put the prepared image, the controller records it, and `Create` clones the prepared template / uses the local prepared pool file — so a second VM from the same prepared image skips the re-download. Closes #214.

### Impact
- [ ] Breaking change (gRPC change is wire-compatible: `task` stays field 1)
- [x] Requires cluster rollout (coordinated manager + all-provider image rollout — proto change)
- [ ] Config change only
- [ ] Documentation only

### Tests
- `internal/transport/grpc/client_imageprepare_test.go`: assert `prepared_image_id/path` round-trip on both async and sync paths.
- `internal/controller/virtualmachine_image_prepare_test.go`: assert `ProviderStatus[provider]{ID,Path}` is stamped (sync), stamped-but-not-Available at trigger then preserved on completion (async), and `TestOverrideImageWithPreparedLocation_Consume` (libvirt/vSphere/Proxmox override + not-Available + wrong-provider no-regression cases).
- `internal/providers/{vsphere,proxmox}/*_test.go`: assert the prepared id (and async location-at-trigger for Proxmox import).

## [2026-06-08 11:32] - Wire image-prepare into the controllers: lazy VM-create-driven prepare (#154)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/controller/virtualmachine_image_prepare.go` (new): `EnsureImageOnProvider`, called from `reconcileVM` after provider validation and before create. Lazy, VM-create-driven image preparation — the VirtualMachine controller is the trigger (it holds the `(image, provider)` pair) and the **single writer** of the prepare-related `VMImage` status fields. Capability-gated (no-op unless the provider instance implements `contracts.ImagePreparer` AND the Provider CR advertises `Status.ReportedCapabilities.SupportsImageImport` — #176), so providers that can't prepare fall through to the unchanged by-reference create path. Idempotent (skips when `ProviderStatus[provider].Available`); honors `Prepare.OnMissing` (Import default / Fail / Wait); triggers `PrepareImage`, persisting an async `PrepareTaskRef`+`Phase=Importing` and requeueing to poll via `IsTaskComplete`, or stamping completion immediately for a synchronous provider. On completion it records `ProviderStatus[provider]{Available}`, appends to `AvailableOn` (deduped), and sets `Ready`/`Phase=Ready`.
- `internal/controller/virtualmachine_image_prepare_test.go` (new): sync + async (poll→complete) prepare, idempotency, no-regression (non-`ImagePreparer` and flag-false), `OnMissing=Fail`/`Wait`, prepare error, and a concurrent two-provider no-clobber case (clean under `-race`).
- `docs/adr/0005-image-preparation-trigger-model.md`: records the lazy/VM-driven trigger, single-writer status strategy (avoids the #189-class race), `Ready`=OR-of-providers vs per-provider `ProviderStatus`/`AvailableOn`, and the deferred items (eager `prepareOn`; "Create consumes the prepared template").
- `docs/image-preparation.md` + `examples/vmimage-prepare-on-create.yaml`: lifecycle + a `VMImage{source.libvirt.url}` + VM example.

### Changed
- `internal/controller/virtualmachine_controller.go`: call `EnsureImageOnProvider` in `reconcileVM`; hold create (requeue, not error) while a prepare is in flight or `OnMissing` forbids preparing. Added a `vmimages/status` get;update;patch RBAC marker (regenerated `config/rbac/role.yaml` + synced `charts/virtrigaud/templates/manager-rbac.yaml`); `vmimages` objects stay read-only.
- `internal/controller/vmimage_controller.go`: documented as a deliberate no-op backstop — the VirtualMachine controller is the sole status writer to avoid a two-writer race.

### Why
PR-5 (the keystone) of the image-prepare vertical slice (#154): makes `ImagePrepare` reachable end-to-end through CRs — applying a `VMImage` + `VirtualMachine` now drives a provider image prepare and reflects it in `VMImage.status` (`phase`/`availableOn`/`providerStatus`).

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager image + RBAC; no CRD-spec/proto change)
- [ ] Config change only
- [ ] Documentation only

### Notes
- **No CRD-spec or proto change.** Follow-up (PR-6): make `Create` *consume* the prepared template (return the prepared image path/id from the provider). Until then the VM still creates from its own source resolution (libvirt Create already handles `url`/`path`/template), so a green VM does not by itself prove the prepared template was used — verify prepare via `VMImage.status`.

## [2026-06-08 11:25] - Manager transport: contracts.ImagePreparer + gRPC client PrepareImage (#154)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/contracts/image.go` (new): optional `ImagePreparer` capability mirroring `Cloner` — `ImagePrepareRequest{ImageJSON, TargetName, StorageHint}` → `ImagePrepareResponse{TaskRef}`, with a `PrepareImage` method. Callers type-assert a `Provider` to `ImagePreparer` to invoke a prepare without widening the core `Provider` interface, so in-process providers and fakes that don't support it are unaffected.
- `internal/transport/grpc/client.go`: `*Client.PrepareImage` implements `ImagePreparer` — calls the provider `ImagePrepare` RPC (5-minute timeout like Create/Clone, `mapGRPCError`, `trackTaskStart` on a returned TaskRef) — plus a compile-time `_ contracts.ImagePreparer = (*Client)(nil)` assertion.
- `internal/transport/grpc/client_imageprepare_test.go` (new): bufconn tests for request-field forwarding + async TaskRef surfacing, synchronous empty-task, and error mapping.

### Why
PR-4 of the image-prepare vertical slice (#154). Pure manager-side plumbing so the manager can *call* `ImagePrepare`; no reconcile behavior change yet. The VM/VMImage controller wiring that drives it (the CR-driven E2E) lands in PR-5.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager image; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-08 11:18] - Audit + tighten Proxmox ImagePrepare to honor the contract (#154)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/proxmox/image.go` (new): real `parseProxmoxImageSource` replaces the untyped `map[string]interface{}` walk in `server.go`. It parses, in precedence: the rich `v1beta1` `source.proxmox.{templateID,templateName,storage,node,format}` shape and the generic `source.http.url` import shape, falling back to the flat `contracts.VMImage` (`TemplateName`/`URL`/`Format`) that `Create` round-trips. An empty/unparseable/source-less spec yields an empty source → the caller returns `InvalidSpec`, never a fabricated success — mirroring the libvirt (PR-1) and vSphere (PR-2) parsers.
- `internal/providers/proxmox/image.go`: `ImagePrepare` now honors `req.TargetName`. Existing-template references (`source.proxmox.templateID`/`templateName`) are verify-only: the template must exist on the node and be `template=1` (`GetVM`/`ListVMs`), returning success if found and an honest `NotFound` if missing — no import. The `source.http.url` path imports into a template named `TargetName` and requires a non-empty `TargetName` (no name is ever fabricated from the URL basename).
- `internal/providers/proxmox/image.go`: idempotency gate — when importing, if a template named `TargetName` already exists on the node, log and return success without downloading (consistent with libvirt/vSphere).

### Changed
- `internal/providers/proxmox/image.go`: storage precedence is `req.StorageHint` → `source.proxmox.storage` → `local-lvm` (documented last resort); node precedence is `source.proxmox.node` → `FindNode` default.
- `internal/providers/proxmox/server.go`: removed the old loose `ImagePrepare` (ignored `target_name`, parsed the non-CRD `source.template` field, hardcoded storage, no idempotency); the method now lives in `image.go`. `SupportsImageImport` stays `true` — now contract-correct.
- `internal/providers/proxmox/pveapi/client.go`: `PrepareImage` now takes `targetName` and `format`, POSTs the storage `download-url` with `content=import` (a cloud image becomes a VM disk → template, not an ISO) and a `filename` of `<targetName>.<format>` instead of the hardcoded `content=iso` + `imported-image.qcow2`. The verify-only "does the template exist" decode-and-discard probe was dropped (that check is now done provider-side).
- `internal/providers/proxmox/pvefake/server.go`: added `GET /nodes/{node}/qemu` (list VMs, incl. templates) and `POST /nodes/{node}/storage/{storage}/download-url` handlers; the latter records the request via `LastDownloadRequest()` so tests can assert node/storage/content/filename/url propagation.
- `internal/providers/proxmox/provider_test.go` + `image_test.go` (new): rewrote `TestProxmoxProvider_ImagePrepare` into focused cases using the real serialized shapes — existing template by name/ID (no-op), missing template (`NotFound`), URL import (asserts the PVE download-url call), storage precedence, missing `target_name` on import (`InvalidSpec`), empty/source-less spec (`InvalidSpec`), and idempotency — plus host-independent unit tests for `parseProxmoxImageSource` and storage precedence.

### Why
PR-3 of the image-prepare vertical slice (#154). The Proxmox ImagePrepare was implemented but loose — it ignored `target_name`, parsed non-CRD JSON shapes, and had no idempotency. This aligns it with the libvirt (PR-1) and vSphere (PR-2) contracts so the upcoming controller wiring (PR-5) can drive all three consistently.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (Proxmox provider image; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

### Notes
- No real Proxmox lab available; validated via unit tests + the in-repo fake PVE server. The `content=import` semantics and the download-url → attach-to-VM → mark-as-template flow are asserted at the call level (the fake server records the request) but not against a live PVE — see the caveats in the PR description.

## [2026-06-08 11:14] - Implement vSphere ImagePrepare: OVA/OVF URL import as template (#154)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/vsphere/image.go`: real `ImagePrepare` for the vSphere provider. Parses the image source from `ImageJson` (rich `v1beta1` `source.vsphere` shape — the only one able to carry `ovaURL`/`contentLibrary` — falling back to the flat `contracts.VMImage` `TemplateName`). Handles three source kinds in precedence order behind an idempotency gate: (1) if a template/VM named `target_name` already exists, return success without importing; (2) `source.vsphere.templateName` → verify the template exists (`finder.VirtualMachine`), no import, honest `NotFound` if missing; (3) `source.vsphere.contentLibrary` → verify the library item exists via vAPI (`vapi/rest` + `vapi/library`), then return a clear `InvalidSpec` (deploy-to-template is out of scope for this PR); (4) `source.vsphere.ovaURL` → the real import: resolve placement (resource pool from `DefaultCluster`, datastore from `storage_hint`→`DefaultDatastore`→`DefaultStoragePod`, folder from `DefaultFolder`→datacenter VM folder), download the OVA/OVF to a temp file, optionally verify its checksum (md5/sha1/sha256/sha512; sha256 default), import via govmomi `ovf/importer` (descriptor → `CreateImportSpec` → `ImportVApp` NFC lease → upload → complete) with thin disk provisioning, then `MarkAsTemplate`. Mid-import failures best-effort `Destroy` the partial VM so a retry starts clean.
- `internal/providers/vsphere/image.go`: `findOVADescriptorName` resolves the OVF descriptor's **exact** entry name inside the OVA tar, skipping macOS **AppleDouble** sidecars (`._*`) and `__MACOSX/` entries, instead of relying on govmomi's `"*.ovf"` glob. OVAs repackaged on macOS pack a binary `._foo.ovf` sidecar *before* the real descriptor; the glob matched it first and the import failed with `XML syntax error: illegal character code U+0000`. Found during live validation against a real macOS-packaged Ubuntu 24.04 OVA.
- `internal/providers/vsphere/image_test.go`: host-independent unit tests (source-JSON parsing, checksum-hasher selection, file-checksum match/mismatch/unknown, nil-client guard, AppleDouble descriptor resolution + no-descriptor error) plus vcsim integration tests that import a trivial OVA and assert a template named `target_name` results, that a re-run is a no-op, and that the templateName verify-only branch returns success when present and `NotFound` when absent.

### Changed
- `internal/providers/vsphere/server.go`: removed the `ImagePrepare` `Unimplemented` stub (the method now lives in `image.go`); updated the `SupportsImageImport: true` comment to reference the now-real implementation (#154). The capability stays `true` — it is now honest.

### Why
PR-2 of the image-prepare vertical slice (#154). Makes vSphere's existing
SupportsImageImport=true honest by importing OVA/OVF from a URL into vCenter
as a template via govmomi, instead of returning Unimplemented.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (vSphere provider image; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

### Notes
- Synchronous import (empty TaskRef); very large OVAs may need async polling (future).
- templateName/contentLibrary sources are verify-only; ovaURL does the real import.
- **Deployment requirement:** the OVA disk upload uses an NFC lease that streams **directly from the provider to the ESXi host** (not via vCenter). The ESXi host(s) must therefore be DNS-resolvable and network-reachable from wherever the vSphere provider runs. This is inherent to vSphere NFC (the same applies to disk export/migration). Surfaced during live validation: vCenter returned a lease URL to `esxi.<domain>` which the provider pod could not resolve.
- Live-validated against a real macOS-packaged Ubuntu 24.04 OVA on vCenter through descriptor parse → CreateImportSpec → ImportVApp → NFC lease; the byte upload requires ESXi reachability per the note above.

## [2026-06-08 11:08] - Implement libvirt ImagePrepare RPC: import image into storage pool (#154)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/libvirt/image.go`: real `ImagePrepare` logic for the libvirt provider. Resolves the target pool (request `StorageHint` → `source.libvirt.storagePool` → `default`), places the prepared template at `<poolPath>/<targetName>.qcow2`, and is idempotent — a re-run with the target already present is a cheap no-op (host `stat` probe). Source resolution: `source.libvirt.path` (host-local image) is converted into the pool directly; `source.libvirt.url` is downloaded **on the libvirt host** via `curl` (not streamed through the provider pod), converted with `qemu-img convert -O qcow2`, then the temp file is removed. Optional checksum verification (md5/sha1/sha256/sha512) runs on the host before commit; a mismatch removes the target and fails. Neither path nor url returns an `InvalidSpec` error rather than a fabricated success. Ownership/permissions/SELinux/pool-refresh reuse the existing `finalizeClonedDisk` helper.
- `internal/providers/libvirt/image_test.go`: host-independent unit tests for image-JSON parsing (rich `v1beta1` `source.libvirt` shape vs flat `contracts.VMImage` shape vs empty/unparseable), target-pool precedence, target-path construction, checksum-tool selection, and the nil-provider / missing-target-name / no-source guard paths.

### Changed
- `internal/providers/libvirt/server.go`: replaced the `ImagePrepare` `Unimplemented` stub with a thin RPC wrapper that casts to `*Provider`, guards `virshProvider`, delegates to `Provider.imagePrepare`, and returns a `TaskResponse` with an empty `Task` (libvirt is synchronous). Flipped `GetCapabilities.SupportsImageImport` from `false` to `true`. Dropped the now-unused `sdk/provider/errors` import.
- `internal/providers/libvirt/server_test.go`: replaced `TestServer_ImagePrepare_ReturnsUnimplemented` with a nil-provider guard test; flipped the `SupportsImageImport` assertion in `TestServer_GetCapabilities_HonestFlags` to expect `true`.

### Why
PR-1 of the image-prepare vertical slice (#154); libvirt was the last true provider-RPC gap among the production providers. Provider-only here — the manager-side transport client and VM/VMImage controller wiring land in later PRs (PR-4/PR-5).

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider image; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

### Notes
- `ImagePrepare` is not yet invoked by any controller (transport client + VM controller wiring are PR-4/PR-5). This PR is validatable by calling the RPC directly via gRPC. Synchronous (empty `TaskRef`).

## [2026-06-08 11:05] - Fix libvirt full clone: copy the resolved disk path, not a guessed pool-volume name (#153)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/libvirt/clone.go`: full clone (`FullClone`) no longer assumes the source disk is a libvirt storage volume named `<vmid>-disk` in the `default` pool. That assumption failed in the field with `error: Storage volume not found: no storage vol with matching path '<vmid>-disk'`, because the provider's create path does not name disks that way. Full clone now copies the **real disk path** resolved from the live domain (`domblklist`) via `qemu-img convert -O qcow2` on the libvirt host — the same naming-agnostic, path-based approach the linked-clone branch already used. `qemu-img convert` also flattens any backing chain, so the full copy is genuinely independent of the source (and of the base image the source may overlay).

### Changed
- `internal/providers/libvirt/clone.go`: extracted the post-create ownership/permission/SELinux/pool-refresh steps into a shared `finalizeClonedDisk` helper used by both full and linked clone; added `createFullCopy`; removed the `virsh vol-clone` path.

### Why
libvirt clone E2E validation on a real host (isolated test provider, fresh source VM) showed **linked clone succeeds** but **full clone fails** for exactly this reason. Full clone is the default clone type, so this made the common case unusable; the path-based copy mirrors the proven linked-clone path.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider image; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

### Notes
- UEFI sources are still not fully handled: the domain-XML rewrite does not yet re-point the per-VM `<nvram>` varstore, so cloning a UEFI domain can collide on the NVRAM path. Use BIOS sources until that follow-up lands; tracked for #153 follow-up.

## [2026-06-08 10:10] - Implement libvirt Clone RPC: qcow2 full + linked clones (#153)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/libvirt/clone.go`: real `Provider.Clone` replacing the `Unimplemented` stub. Full clone copies the source disk via `virsh vol-clone`; linked clone creates a qcow2 overlay backed read-only by the source disk. Defines a new domain by rewriting the source's `dumpxml` with a fresh name, v4 UUID, and per-NIC locally-administered MAC, re-pointing the primary disk source (cloud-init CD-ROM left untouched). Best-effort CPU/memory `ClassJSON` overrides applied.
- `internal/diskutil/qemu_img.go`: `QemuImg.CreateWithBacking` helper (`qemu-img create -f qcow2 -b <src> -F <fmt> <overlay>`) for copy-on-write overlays, with backing-format required.
- `internal/providers/libvirt/clone_test.go`, `internal/diskutil/qemu_img_test.go`: unit tests for XML rewrite (fresh identity, multi-NIC, error paths), class overrides, UUID/MAC generation, backing-file arg validation, and provider/server guards.

### Changed
- `internal/providers/libvirt/server.go`: `Server.Clone` now delegates to `Provider.Clone` (proto↔contracts translation) instead of returning `Unimplemented`; `GetCapabilities` advertises `SupportsLinkedClones=true`. `SupportsImageImport` left false (#154).
- `internal/providers/libvirt/server_test.go`: capability assertion flipped to `SupportsLinkedClones=true`; former Unimplemented test replaced by nil-provider guard.

### Why
The manager-side VMClone controller (#179) shipped gated on `SupportsLinkedClones`, but libvirt returned `Unimplemented`, so clone requests failed honestly but unusably. This implements same-provider qcow2 clones so LinkedClone works end-to-end on libvirt.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider image; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

### Notes
- Linked-clone children are lifecycle-bound to the parent: the source disk must not be modified or deleted while overlays exist, or the clones corrupt.
- `CustomizeJSON` (hostname/cloud-init) is logged-and-deferred in this MVP — the clone inherits the source's cloud-init; faithful customization (nocloud ISO regen) is a follow-up.

## [2026-06-08 07:30] - Capability parity quick wins: vSphere memory snapshots, libvirt export compression, Proxmox disk export/import (#198, #199, #200)
**Author:** @wrkode (William Rizzo)

### Changed
- `internal/providers/vsphere/server.go`: advertise `SupportsMemorySnapshots=true` (#200). The snapshot path already honors `req.IncludeMemory` (passes `memory` to `CreateSnapshot`); only the capability flag was understated. Memory snapshots require the VM to be powered on.
- `internal/providers/libvirt/provider_virsh.go` + `server.go`: honor `req.Compress` in `ExportDisk` via `qemu-img -c` for qcow2 targets (forcing a convert pass when compression is requested for an unchanged format), and advertise `SupportsExportCompression=true` (#199). Default (`Compress=false`) export is byte-for-byte unchanged; raw targets are unaffected (qemu-img ignores `-c` for raw).
- `sdk/provider/capabilities/capabilities.go`: the capability `Manager`/`Builder` could not express disk migration — `Manager.GetCapabilities` omitted the disk-export/import/compression fields entirely. Added `DiskExport(formats…)`, `DiskImport(formats…)`, `ExportCompression()` builder methods + the `Manager` mapping (#198). This is the reusable fix so any builder-based provider can advertise disk migration.
- `internal/providers/proxmox/capabilities.go`: advertise `SupportsDiskExport`/`SupportsDiskImport` (+ formats `qcow2`/`raw`/`vmdk`) to match the implemented `ExportDisk`/`ImportDisk`/`GetDiskInfo` RPCs (#198). `SupportsExportCompression` left false (the Proxmox export path does not compress today).

### Added
- Tests: `sdk/provider/capabilities/capabilities_test.go` (disk-migration builder + mapping); updated vSphere/libvirt/Proxmox capability tests.

### Why
Part of the provider-capability-parity roadmap (umbrella #204). Each was a VirtRigaud reporting/wiring gap, not a hypervisor limitation. These corrections are load-bearing once `--enforce-provider-capabilities` (#176) is enabled — without them, capability gating would wrongly block Proxmox disk migration and under-report vSphere/libvirt support.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (provider images; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-08 06:10] - CI/release: unify supply-chain verification format + close Node-20 action backlog (#172, #134)
**Author:** @wrkode (William Rizzo)

### Changed
- `.github/workflows/release.yml`: pin both `Install Cosign` steps to `cosign-release: v2.6.3` (#172). cosign v3.0 flipped signing output to the new Sigstore-bundle (OCI 1.1 referrer) format, while `slsa-github-generator` still writes SLSA provenance to the legacy `.att` tag — so no single cosign version verified signature + SBOM + SLSA with default flags. v2.6.3 emits legacy `.sig`/`.att` tags that match the SLSA generator, restoring **single-cosign-version (any >= 2.2) verification of all three artifacts**. Verification-ergonomics only — all artifacts were already individually present and cryptographically valid.
- `.github/dependabot.yml`: documented #134 resolution — the four originally-outstanding Node-20 actions (`actions/setup-go`, `docker/login-action`, `docker/metadata-action`, `docker/setup-buildx-action`) are now on their Node-24 majors across all workflows; remaining holdouts (`actions/checkout` v4 #137/K4, `codecov-action` v5 #140) are tracked + deferred.

### Why
Closes #172 (verification ergonomics) and #134 (Node-20 GitHub Actions deprecation, hard-deadlined 2026-09-16). The cosign change is the prerequisite for documenting a single, universally-verifiable supply-chain check.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] CI / release-workflow change only (takes effect on the next release; v0.3.8 shipped as-is)
- [ ] Documentation only

## [2026-06-08 05:45] - libvirt provider: SSH ControlMaster connection multiplexing (#194)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/libvirt/sshhostkey.go`: SSH **ControlMaster** connection multiplexing for the libvirt provider. A burst of `virsh`/`scp` invocations now reuses a single SSH connection (`ControlMaster=auto`, `ControlPath=/tmp/virtrigaud-ssh-%C`, `ControlPersist=60s`) instead of opening a fresh handshake per command — eliminating at the source the connection churn that can trip the libvirt host's sshd `MaxStartups`/fail2ban (the symptom #191 retries client-side). Wired into both `virsh` SSH branches (`virsh.go`), the `scp` disk-copy path (`server.go`), and the `~/.ssh/config` stanza used by libvirt's own `qemu+ssh://` transport. ON by default; escape hatch `LIBVIRT_SSH_DISABLE_MULTIPLEXING=true` reverts to one connection per command. The control socket lives under `/tmp` (writable in the provider container, wiped on restart, so no stale sockets survive), and `%C` keeps the path under the AF_UNIX limit.
- `internal/providers/libvirt/sshmultiplex_test.go`: tests for the env escape hatch, the option builder (enabled/disabled), and the config-stanza multiplex lines.

### Why
Follow-up to #191 (which added client-side retry/backoff). Multiplexing removes the churn rather than just tolerating its symptom: steady-state, many `virsh` calls share one connection, so the host's per-source connection-rate limits are far less likely to trip.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider only; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-07 17:05] - libvirt provider: retry transient SSH connection failures (#191)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/libvirt/virsh.go`: the libvirt provider runs every `virsh` command as a fresh `sshpass -e ssh` process, so a burst of operations opens a burst of SSH handshakes. When the libvirt host throttles connections (sshd `MaxStartups`, fail2ban) it closes the connection during key exchange, surfacing as `kex_exchange_identification: Connection closed by remote host` and tripping the provider circuit breaker. `runVirshCommand` now retries **only** transient SSH *connection* failures (classified from stderr: `kex_exchange_identification`, connection refused/reset/timed-out, no-route-to-host, transient DNS) with bounded exponential backoff (3 attempts), honoring context cancellation. Real virsh errors (e.g. "domain not found") and the success path return on the first attempt — behavior is unchanged except under host-side SSH throttling.

### Added
- `internal/providers/libvirt/virsh_retry_test.go`: tests for the transient-error classifier (transient connect errors vs. real virsh errors) and the retry loop (retries-then-succeeds, real-error-no-retry, attempt exhaustion, context-cancel).

### Why
Recurring on the lab (`I1`): host `172.16.56.8` repeatedly closed SSH connections during migration/clone E2E windows, making managed VMs appear unreachable. This makes a momentary host-side refusal recoverable instead of fatal. **The root cause is partly host-side** — operators must still tune `MaxStartups`/fail2ban on the libvirt host; connection multiplexing (ControlMaster) is tracked as a follow-up.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (libvirt provider only; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-07 17:00] - vSphere provider: keep vCenter session alive + real-probe reconnect (#190)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/vsphere/server.go`: the vSphere provider established a govmomi session once at startup and never kept it warm, so after a long idle period (no managed vSphere VMs ⇒ no API traffic) the server-side vCenter session expired and the next operation failed with `NotAuthenticated`. Worse, `Validate` gated its reconnect on the cached `client.Valid()` (no round-trip), so it reported OK while operations failed. Two-part fix:
  - **Keepalive handler** (`session/keepalive.NewHandlerSOAP`) installed in `createVSphereClient`, probing every `vSphereKeepAliveInterval` (5m, well under vCenter's 30m default) and re-logging-in on a failed probe — so the session never idles out.
  - **`Validate` now probes the live session** (`methods.GetCurrentTime`) instead of trusting `client.Valid()`, and reconnects with a fresh login on failure. Since the manager calls `Validate` before operations, this is the safety net.

### Added
- `internal/providers/vsphere/session_test.go`: hermetic govmomi-`simulator` tests — keepalive handler is installed on the round-tripper, and `Validate` reconnects + reports OK after the session is dropped.

### Why
Observed on the lab: the `vsphere-prod` provider ran ~8 days idle (no managed vSphere VMs), then a clone `Create` failed `NotAuthenticated` while `Validate` returned OK; a pod restart was the only recovery. This makes the provider self-heal.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (vSphere provider only; no CRD/proto change)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-07 10:00] - Fix VMClone target-VM bind race (Status.ID seed) (#179 follow-up)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/vmclone_controller.go`: make the clone target-VM binding conflict-tolerant and idempotent. A live vSphere clone E2E found that the target VM's `Status.ID` seed (`Status().Update`) loses a race with the VirtualMachine controller — which reconciles the freshly-created adopted target VM immediately and bumps its resourceVersion — so the update failed with a conflict and the recovery path (`Target VM already exists → finalizeReady`) marked the clone `Ready` **without** re-seeding `Status.ID`, leaving the cloned VM orphaned (unmanaged) forever. The fix: persist `Status.TargetVMID` before binding; seed `Status.ID` via `RetryOnConflict` re-Getting the latest object; only finalize `Ready` once `Status.ID` is confirmed; resume binding off the persisted `TargetVMID` (never re-clone); poll the async task before binding; and refuse a pre-existing foreign VM with the target name instead of finalizing over it.

### Why
The MVP clone controller (#188) passed unit tests but the fake client did not reproduce the concurrent write from the VirtualMachine controller. The real vSphere clone succeeded (one VM, no double-create — the guard held) but the target VM CR was left with an empty `Status.ID`, so the cloned VM was never managed. This makes the bind step robust against the inherent controller-vs-controller race.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (manager only; no CRD/RBAC change)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-07 09:00] - VMClone controller (MVP), VMSet stub, VMPlacementPolicy reference-only, VM double-create guard (#179)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/providers/contracts/clone.go`: `CloneRequest`/`CloneResponse` structs and a narrow `Cloner` optional capability interface (mirrors `CapabilityReporter`; does NOT widen the core `Provider` interface).
- `internal/transport/grpc/client.go`: `(*Client).Clone` implementing `contracts.Cloner` over the existing proto `Clone` RPC; compile-time assertions that `*Client` satisfies `Provider`, `CapabilityReporter`, and `Cloner`.
- `internal/controller/vmclone_controller.go`: new VMClone reconciler (MVP) — `source.vmRef` only, same-provider, full & linked clones. Resolves source VM + provider, intrinsic linked-clone capability pre-check (fail-open), idempotent clone, task polling, and binds a target VirtualMachine CR (seeds `Status.ID`, `virtrigaud.io/adopted=true`, clone provenance annotations). Deleting a VMClone never deletes the produced VM.
- `internal/controller/vmset_controller.go`: not-yet-active VMSet stub reconciler that sets `Ready=False / ControllerNotImplemented` and nothing else.
- `api/infra.virtrigaud.io/v1beta1/vmclone_types.go`: additive `Status.TargetVMID` field to persist the provider's cloned VM ID across reconciles.
- `cmd/manager/main.go`: register the VMClone and VMSet reconcilers.
- `internal/controller/{vmclone,vmset,virtualmachine_controller_adopted}_controller_test.go`: unit tests — VMClone happy path / idempotency / linked-blocked / linked-fail-open / non-vmRef source / non-Cloner provider / source-missing / deletion-preserves-target; VMSet stub condition; Part B adopted-guard (no provider Create) + non-adopted control.
- `examples/vmclone-basic.yaml`: vmRef-source full-clone example.

### Changed
- `internal/controller/virtualmachine_controller.go`: the create decision now skips create for a VM labeled `virtrigaud.io/adopted=true` while `Status.ID` is empty (requeues instead), preventing a double-create while the adoption/clone controller sets `Status.ID`. Behavior for normal VMs is unchanged. Added `vmIsAdopted` helper.
- `internal/controller/vmadoption_controller.go`: promoted the `virtrigaud.io/adopted` label key/value to shared `AdoptedLabel`/`AdoptedLabelValue` constants and reuse them in place of literals.
- `config/crd/bases/`, `charts/virtrigaud/crds/`: regenerated VMClone CRD with the new `targetVMID` status field.
- `config/rbac/role.yaml`, `charts/virtrigaud/templates/manager-rbac.yaml`: added RBAC for `vmclones` (get/list/watch/update/patch + status + finalizers) and `vmsets` (get/list/watch + status); VirtualMachine create was already granted.
- `README.md`: CRD table now lists controller status — VMClone active (MVP), VMSet not yet active (stub), VMPlacementPolicy reference-only.

### Why
VMClone, VMSet, and VMPlacementPolicy shipped as CRDs without controllers. This wires VMClone end-to-end (MVP), marks VMSet explicitly not-yet-active rather than silently inert, and documents VMPlacementPolicy as a reference-only policy object. The VirtualMachine double-create guard closes a real correctness gap: an adopted/cloned VM CR briefly has an empty `Status.ID`, during which the VM controller would otherwise create a second VM on the provider. libvirt's `Clone` is `Unimplemented`, so a libvirt clone surfaces a clear `Phase=Failed/ProviderError` rather than a silent no-op.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout (CRD `targetVMID` field, new RBAC, two new controllers)
- [ ] Config change only
- [ ] Documentation only

### Usage
```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClone
metadata:
  name: basic-clone
  namespace: default
spec:
  source:
    vmRef:
      name: my-source-vm
  target:
    name: my-cloned-vm
    classRef:
      name: standard-vm
  options:
    type: FullClone   # or LinkedClone (requires provider support)
```

---

## [2026-06-07 12:05] - vSphere: advertise disk export/import capabilities accurately (#178)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/vsphere/server.go`: `GetCapabilities` now reports `SupportsDiskExport=true`, `SupportsDiskImport=true`, `SupportedExportFormats=[vmdk, qcow2, raw]`, `SupportedImportFormats=[vmdk, qcow2, raw]`, and `SupportsExportCompression=true`. These were previously left at the zero value (`false`/empty), understating capabilities that vSphere actually implements (`ExportDisk` clones to a compressed streamOptimized VMDK and converts to the target format; `ImportDisk` accepts and converts those formats).

### Added
- `internal/providers/vsphere/capabilities_test.go`: asserts the corrected disk-migration flags + formats, and that existing flags are unchanged.

### Why
The understated flags are harmless today (the manager surfaces but does not yet enforce capabilities) but become load-bearing once `--enforce-provider-capabilities` (#176) is enabled: gating on `SupportsDiskExport`/`SupportsDiskImport=false` would wrongly refuse vSphere migrations it can actually perform. Confirmed live on the lab during #176 validation — vSphere's `status.reportedCapabilities` showed disk export/import absent (false). This unblocks safely enabling capability gating. Fixes #178.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout — only to ship the rebuilt vSphere provider image; no behavior change (capabilities are advisory until gating is enabled)
- [ ] Config change only
- [ ] Documentation only

## [2026-06-07 12:00] - Capability negotiation: surface provider capabilities + opt-in gating (#176)
**Author:** @wrkode (William Rizzo)

### Added
- `internal/controller/provider_controller.go`: the Provider reconciler now best-effort queries the running provider's `GetCapabilities` RPC (once `ProviderAvailable` and runtime `Running`) and surfaces the result on `Provider.Status.ReportedCapabilities`, plus a `CapabilitiesReported` Condition (True `CapabilitiesFetched` / False `CapabilitiesUnavailable`). Consumed via the narrow `contracts.CapabilityReporter` extension interface (type-asserted from the resolved provider), so the core `contracts.Provider` interface is unchanged. Strictly best-effort: a nil resolver, resolve failure, non-reporter provider, or failing RPC logs at V(1) and never fails the reconcile or flips `Healthy`.
- `cmd/manager/main.go`: new `--enforce-provider-capabilities` bool flag (**default false**). When off, snapshot/migration behavior is byte-for-byte unchanged. Threaded as `EnforceCapabilities` into the VMSnapshot and VMMigration reconcilers.
- `internal/controller/vmsnapshot_controller.go`: when enforcement is on, the snapshot CREATE path gates on the provider's reported capabilities before calling `SnapshotCreate` — refusing with a Warning event + Failed/`UnsupportedByProvider` condition when `!SupportsSnapshots`, or when a memory-inclusive snapshot is requested and `!SupportsMemorySnapshots`. Fails open if the provider is not a `CapabilityReporter` or the query fails.
- `internal/controller/vmmigration_controller.go`: when enforcement is on, the exporting phase gates on source `SupportsDiskExport` before `ExportDisk`, and the importing phase gates on target `SupportsDiskImport` before `ImportDisk`, failing the migration with a clear reason. Fails open if the provider is not a `CapabilityReporter` or the query fails.
- `internal/controller/capability_gating_test.go`, `internal/controller/provider_controller_capabilities_test.go`: table-style unit tests with a fake `contracts.CapabilityReporter` provider asserting gating blocks when the flag is on and the capability is false, does not block when the flag is off or the capability is true, and fails open when the provider is not a `CapabilityReporter` or the RPC errors; plus the capabilities→status mapping and the provider-controller best-effort condition behavior.

### Why
Builds on the #176 foundation (capabilities contract, gRPC client method, CRD status field). Surfacing capabilities makes provider feature support observable to operators; gating prevents issuing operations a provider declares it cannot perform. Gating is opt-in because a provider that under-reports a capability (e.g. vSphere currently understates disk export/import) would otherwise block operations it can actually perform — operators must confirm capability flags are accurate before enabling.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout — manager image must be updated
- [ ] Config change only
- [ ] Documentation only

## [2026-06-06 13:57] - Fix: migration PVCs being deleted no longer wedge the provider rollout (#184)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/controller/provider_controller.go`: `discoverMigrationPVCs` and `discoverMigrationVolumeMounts` now **skip migration PVCs that are being deleted** (`DeletionTimestamp != nil`). Mounting a `Terminating` PVC into a freshly-rolled provider pod made that pod unschedulable (`persistentvolumeclaim "<pvc>" is being deleted`); combined with the running pod holding the PVC, this deadlocked both the PVC deletion and the provider rollout.
- `internal/controller/provider_controller.go`: the provider controller now **watches migration-storage PVCs** and re-reconciles the namespace's `Provider`s when one appears or starts deleting, so provider Deployments mount/unmount migration storage promptly instead of waiting for the next resync.
- Extracted the migration-PVC label into `migrationPVCLabelKey`/`migrationPVCLabelValue` constants (was a repeated literal).

### Added
- `internal/controller/provider_controller_migration_test.go`: unit tests (fake client) asserting deleting PVCs are skipped by both discovery functions and that the PVC→Provider watch map function enqueues same-namespace providers (and ignores non-migration PVCs).

### Why
A `VMMigration` that fails or is deleted mid-flight leaves its scratch PVC being deleted while the long-running provider pod still mounts it. The provider controller previously re-mounted every `migration-storage` PVC unconditionally, so the deleting PVC could never be unmounted — wedging the source provider's next rollout and leaving the PVC stuck `Terminating`. Observed on the lab during #177 validation; recovery required manual finalizer/annotation surgery. Fixes #184.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout — manager image must be updated to get the fix
- [ ] Config change only
- [ ] Documentation only

## [2026-06-06 08:04] - Libvirt: wire ExportDisk/GetDiskInfo into the gRPC server (#177)
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/libvirt/server.go`: added `Server.ExportDisk` and `Server.GetDiskInfo`, which delegate to the existing libvirt `Provider` implementations in `provider_virsh.go` (translating between the gRPC and provider-contract types). These RPCs were previously unreachable over gRPC — the embedded `UnimplementedProviderServer` answered them — so libvirt could not act as a disk-export source or report disk info for migration despite having working implementations.
- `internal/providers/libvirt/server.go`: `GetCapabilities` now advertises `SupportsDiskExport=true`, `SupportsDiskImport=true`, `SupportedExportFormats=[qcow2, raw]`, `SupportedImportFormats=[qcow2, raw, vmdk]`, and `SupportsExportCompression=false`, matching the now-reachable behavior.

### Added
- `internal/providers/libvirt/server_test.go`: tests covering request/response type conversion for both RPCs (including the `DestinationUrl`→`DestinationURL` and `TaskRef`→`Task` mappings), the nil-provider error path, and the new capability flags. Uses an embedded-interface fake provider so only the two methods under test are implemented.

### Why
The libvirt provider already implemented `ExportDisk`/`GetDiskInfo` (in `provider_virsh.go`), but the gRPC `Server` never delegated to them, so the manager's migration controller received `Unimplemented`. Wiring them restores libvirt as a migration source and makes its capability advertisement honest. Fixes #177.

### Impact
- [ ] Breaking change — additive: an RPC that returned `Unimplemented` now returns real results
- [ ] Requires cluster rollout — effect applies after the libvirt provider image is updated
- [ ] Config change only
- [ ] Documentation only

## [2026-06-06 07:50] - Chart: disable templated standalone providers by default (#173)
**Author:** @wrkode (William Rizzo)

### Fixed
- `charts/virtrigaud/values.yaml`: `providers.libvirt.enabled` and `providers.vsphere.enabled` now default to `false` (proxmox already was). Under the v0.3.7 secure-by-default provider auth (ADR-0003 / #148), a default `helm install` rendered standalone provider Deployments with neither TLS material nor the plaintext opt-out, so they fail-closed and crash-looped. Providers are normally deployed by the manager's ProviderController from `Provider` CRs; the chart-templated Deployments are an opt-in.
- `charts/virtrigaud/README.md`: removed `--set providers.*.enabled=true` from the casual install example, updated the Provider Configuration section to the new defaults, and documented that opting in also requires the `providerTLS` block (`secretName` for mTLS, or `insecure=true` for audit-flagged plaintext) — otherwise the provider crash-loops.
- `charts/virtrigaud/values.yaml`: added an explanatory comment on the `providers:` block describing the opt-in contract.

### Why
The out-of-box experience of the published chart was a crash-loop (the manager and CR-deployed providers were unaffected). Defaulting the templated providers to disabled matches the documented architecture — providers come from `Provider` CRs — and the opt-in path (verified via `helm template`) still wires `providerTLS` correctly. Fixes #173.

### Impact
- [ ] Breaking change — only affects operators who relied on the chart auto-templating standalone libvirt/vsphere providers (which were crash-looping anyway); they now set `providers.<name>.enabled=true` explicitly plus `providerTLS`.
- [ ] Requires cluster rollout
- [x] Chart + docs change only
- [ ] Documentation only

## [2026-06-06 06:55] - Security: bump Go toolchain 1.26.3 → 1.26.4 (clears 3 stdlib CVEs)
**Author:** @wrkode (William Rizzo)

### Security
- `go.mod`, `sdk/go.mod`, `proto/go.mod`: bump the `go` directive 1.26.3 → 1.26.4.
- `build/Dockerfile.manager`, `cmd/provider-{libvirt,mock,proxmox,vsphere}/Dockerfile`: bump the golang builder image 1.26.3 → 1.26.4.
- Clears three Go standard-library vulnerabilities flagged by `govulncheck` (all fixed in go1.26.4): **GO-2026-5037** (`crypto/x509`), **GO-2026-5038** (`mime`), **GO-2026-5039** (`net/textproto`).

### Why
The stdlib CVEs were disclosed after the last `main` CI run, so the required `govulncheck` job began failing repo-wide on every branch (including `main` HEAD), independent of any code change. Bumping the toolchain to the fixed release restores a green supply-chain scan and removes the `crypto/x509` exposure relevant to the project's regulated-deployment posture. Verified locally under go1.26.4: `govulncheck ./...` and `cd sdk && govulncheck ./...` both report "No vulnerabilities found"; `make lint`/`make build`/`make test` pass.

### Impact
- [ ] Breaking change
- [x] Requires cluster rollout — only to ship rebuilt images carrying the patched stdlib; running clusters are unaffected until upgraded
- [ ] Config change only
- [ ] Documentation only

## [2026-06-06 06:40] - Libvirt: honest Clone/ImagePrepare (return Unimplemented) + accurate capabilities
**Author:** @wrkode (William Rizzo)

### Fixed
- `internal/providers/libvirt/server.go`: `Clone` and `ImagePrepare` no longer fabricate a `TaskRef` for work they do not perform. They now return gRPC `Unimplemented` (via `sdk/provider/errors.NewUnimplemented`), so callers get an honest, actionable error instead of a fake success that never produces a VM/image. Removed the now-unused `generateTaskID`/`generateVMID` helpers.
- `internal/providers/libvirt/server.go`: `GetCapabilities` now reports `SupportsLinkedClones=false` and `SupportsImageImport=false`, matching actual behavior (previously advertised `true`).
- `cmd/provider-libvirt/main.go`: removed `linked-clones` from the provider's startup capabilities log banner so it no longer advertises a capability the provider does not implement.
- `README.md`: corrected the provider capability matrix — Libvirt full clone / linked clones / Clone RPC / ImagePrepare RPC / Image Import are marked unsupported with issue references.

### Added
- `internal/providers/libvirt/server_test.go`: tests asserting `Clone`/`ImagePrepare` return `Unimplemented` and that `GetCapabilities` reports honest clone/image-import flags (snapshots remain supported).

### Why
The libvirt Clone and ImagePrepare RPCs were stubs that fabricated task IDs and reported success while doing nothing, and the provider advertised both as supported. A fabricated `TaskRef` is worse than an honest error — the manager would poll a task that never completes. These RPCs are not reachable from any manager controller today (no `Clone`/`ImagePrepare` in the manager gRPC client or `contracts.Provider`; `VMClone` has no reconciler), so this change is libvirt-provider-local and does not alter any manager-driven flow. Addresses #153 and #154.

### Impact
- [ ] Breaking change — provider-internal behavior correction only; no CRD/operator API change and no manager flow reaches these RPCs
- [ ] Requires cluster rollout — optional; effect only applies after the libvirt provider image is updated
- [ ] Config change only
- [ ] Documentation only

## [2026-05-29 19:25] - v0.3.7: Fix SBOM attestation (cosign attest has no --recursive)
**Author:** @wrkode (William Rizzo)

### Fixed
- `.github/workflows/release.yml`: the `Generate SBOMs` job's SBOM attestation used `cosign attest --yes --recursive`, but `cosign attest` does **not** support `--recursive` (only `cosign sign` does; current cosign removed it from `attest`). The `v0.3.7-rc2` release run failed with `Error: unknown flag: --recursive` on the manager SBOM leg (fail-fast cancelled the rest), skipping `create-release`. Removed `--recursive`; the SBOM is now attested to the multi-arch **index** digest, which is the correct, verifiable target (`cosign verify-attestation <image>:<tag>` resolves tag → index → attestation).

### Why
rc2 proved the whole pipeline EXCEPT this: all 10 build legs, 5 manifest merges, `cosign sign --recursive` (index + both arch children), Trivy, and the full SLSA provenance generator chain all succeeded. Only the SBOM attestation flag was wrong. Index-level SBOM attestation is not a regression vs v0.3.6 (which was amd64-only with a single SBOM attestation) — the SBOM predicate is essentially arch-independent (Go module set). No supply-chain control weakened: signing stays recursive over index+children, SLSA provenance is per-component, SBOM covers the released artifact. Unblocks recutting `v0.3.7-rc3`.

### Impact
- [ ] Breaking change
- [ ] Requires cluster rollout
- [x] Config change only — release build infra
- [ ] Documentation only

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
