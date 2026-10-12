# ADR-0011: Migration staging cleanup through a provider RPC

## Status

**Accepted (maintainer decision, 2026-10-11).** The maintainer chose Option A below
(a new provider RPC) before this record was written. This ADR records the decision,
the design as implemented, and the limits that remain. **Amended 2026-10-12** after
the security review of the implementation: unambiguous NFS staging names (D5), a
staging digest (D5), bounded holds for older providers and DNS failures (D4, D7),
waiting for a running export task (D3), the post-migration cleanup guard (D9,
maintainer decision), and staged-image header checks on every import (D10).

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Tracking**: v0.4.0-rc1 backlog item #15.

**Related**:
- [ADR-0006](./0006-storage-backend-agnostic-cross-hypervisor-migration.md): the
  `nfs` and `s3` staging backends this ADR cleans up after. Its Slice 5 planned
  "cleanup wired to `MigrationOptions.CleanupPolicy`"; this ADR is that cleanup.
- [ADR-0005](./0005-image-preparation-trigger-model.md): the capability double gate
  (type assertion plus an advertised capability) reused here.
- [ADR-0010](./0010-provider-in-use-finalizer.md): a `Provider` is kept while a
  `VMMigration` still needs it. This ADR adds a pending staging cleanup to that rule.
- The source-snapshot cleanup hold merged in #378
  (`internal/controller/vmmigration_snapshot_cleanup.go`): the hold, backoff and
  force-delete behavior here copy it.

## Context

A `VMMigration` moves a disk from the source provider to the target provider through
a **staging object**: the source's `ExportDisk` writes it, the target's `ImportDisk`
reads it. The object's location comes from the migration's `spec.storage`:

| Backend | Staged object |
|---------|---------------|
| `s3` | `<prefix>/vmmigrations/<namespace>/<name>/export.vmdk` in `spec.storage.s3.bucket` |
| `nfs` | `vmmigrations-<namespace>-<name>-export.qcow2` in `spec.storage.nfs.export` (under `path` when set) |
| `pvc` | `vmmigrations/<namespace>/<name>/export.qcow2` on the migration's PVC |

Each object is a **full, unencrypted copy of the tenant's disk**.

Before this ADR nothing deleted an `nfs` or `s3` staged object: not after a
successful import, not when the migration failed, and not when the `VMMigration` was
deleted. `docs/clustered-provider-inventory.md` told operators to remove them by hand.

The manager did have a cleanup step (`cleanupIntermediateStorage`), but it could not
work for any backend:

- It always built a **PVC** storage client, rooted at `/mnt/migration-storage/<pvc>`
  **in the manager pod**. The manager never mounts migration PVCs; only provider pods
  do. Creating the client therefore failed on every call.
- For `nfs` and `s3` it passed an `nfs://` or `s3://` URL to that PVC client.
- In `handleReadyPhase` its failure returned early with a 5-minute requeue, before
  the source-snapshot delete and before `spec.source.deleteAfterMigration`. A
  successful migration with `spec.storage` set (every migration, because the export
  requires it) therefore never cleaned up its source snapshot and never deleted its
  source VM. This was a real bug, and this ADR fixes it.

The `pvc` backend is different: the PVC that the migration creates is owned by the
`VMMigration` and deleted with it, and `handleDeletion` also deletes it explicitly. A
user-supplied PVC (`spec.storage.pvc.name`) is the user's volume.

## Options considered

### Option A — a provider RPC that deletes the staged object (**chosen**)

The provider already reaches the staging backend: it is the S3 client, or it runs the
NFS client (libnfs on the hypervisor host or in the pod, or a kernel mount on the
Proxmox node). Deleting the object uses the same reach, the same identity, and the
same credentials as the export and import, so no component gains a new network path.

### Option B — the manager deletes the object directly

The manager would become an S3 client and an NFS client.

Rejected because:
- the manager is the most privileged component. Dialling tenant-controlled endpoints
  from it moves the SSRF surface (ADR-0006 C3) into the component that holds
  cluster-wide RBAC.
- the distroless manager image has no NFS client. libnfs is a cgo dependency, and
  a kernel mount needs privileges the manager must not have.
- the manager's NetworkPolicy would need egress to every staging endpoint.

### Option C — documentation only, or bucket lifecycle policies

Operators would clean up by hand, or with an S3 lifecycle rule on the `vmmigrations/`
prefix and a periodic job on the NFS export.

Rejected as the only mechanism. The copy stays readable until the rule runs. NFS has
no lifecycle rule. And the operator has to know that VirtRigaud leaves tenant disks
behind. We keep it as a **recommended backstop** for objects that are left behind
(see D7) and for objects staged before this release.

### Other questions considered

- **Ask the source provider (the writer)?** Rejected as the rule. The source
  `Provider` can be resolved only through the source `VirtualMachine`, which
  `spec.source.deleteAfterMigration` deletes. In a provider-change migration
  (`metadata.purpose: provider-change`), the source `Provider` is the one being
  decommissioned.
- **Ask any provider with access, with fallback?** Rejected. A fallback chain makes
  the outcome depend on the order of failures, and a transient error on one provider
  would send the delete to another. One fixed rule is easier to reason about and test.

## Decision

### D1 — The RPC

```proto
rpc DeleteStagedDisk(DeleteStagedDiskRequest) returns (DeleteStagedDiskResponse);

message DeleteStagedDiskRequest {
  ObjectIdentity migration = 1;          // the VMMigration: uid, namespace, name
  string stage = 2;                      // "export", the only stage staged today
  string backend_type = 3;               // "nfs" | "s3"
  string storage_options_json = 4;       // same JSON as ExportDisk / ImportDisk
  map<string, string> credentials = 5;   // s3 only; same keys as ExportDisk
  string layout = 6;                     // "v2", or "v1"/empty (legacy); see D5
  ObjectIdentity target_vm = 7;          // the import's target VM, when set (below)
}

message DeleteStagedDiskResponse {
  StagedDiskDeleteOutcome outcome = 1;   // DELETED | NOT_FOUND | EMPTIED
}
```

`target_vm` names the VirtualMachine the migration's import landed a disk for (the
same identity as `ImportDiskRequest.target_vm`). A provider whose `ImportDisk` keeps
a provider-side copy until `Create` removes that copy too. Only Proxmox keeps one:
the node-side import stage, `/var/lib/virtrigaud/import-stage/<ns>_<vm>-migrated.<format>`,
named after the target VM. Proxmox removes it with `rm -f --`, in every stage format,
only on paths `isImportStagePath` accepts, and BEFORE the staged object. If that
removal fails, the call is retryable and the staged object is not touched. The
other providers ignore `target_vm`: libvirt's landed disk IS the VM's disk.

The manager records the VM an `nfs` / `s3` import is sent for in
`status.storageInfo.importTarget`, and persists it before `ImportDisk` is sent.
`target_vm` is only ever that recorded VM, never the current `spec.target`: a tenant
can edit `spec.target` after the import, and the stage of the VM it would then name
(possibly in another namespace) is not this migration's.

`target_vm` is left unset in three cases:

- no import was recorded;
- the recorded VM is in another namespace that no longer grants this migration's
  namespace (`AllowedSourceNamespacesAnnotation`, read live);
- another VMMigration still in flight targets, or recorded an import for, the same
  VM. "In flight" means not being deleted, not `Ready`, and not failed for good: a
  `Failed` migration that still waits for a retry counts. Its import may have
  written the same stage, and its `Create` may be about to read it.

A `Create` of that VM that is still running is not disturbed: `qm importdisk` keeps
reading a stage it has already opened after the stage is unlinked. A `Create` that
has not reached `qm importdisk` fails its node-side stage check and removes its VM
shell. Without the SSH data plane nothing can have been staged on the node, so
nothing is removed there.

- **Idempotent.** An object that does not exist is a success with outcome
  `NOT_FOUND`. A gRPC `NotFound` status is also treated as success by the manager.
- **No key, path or URL on the wire.** The provider derives the object's location
  itself (see D5).
- `EMPTIED` means the provider could not unlink the file and truncated it to zero
  bytes instead: the tenant data is gone, and an empty file with the staging name
  remains (see D6).
- Errors: `InvalidArgument` for a request that can never succeed, `Unimplemented`
  for a backend the provider cannot delete from, and any other code for a failure
  that a retry can fix.

### D2 — The target provider deletes

The manager asks the **target** `Provider` (`spec.target.providerRef`), resolved
through `targetProviderFor`. That resolution enforces the same consumer grants as the
import.

- It can be resolved from the `VMMigration` spec alone, with no `VirtualMachine`.
- It read the object last, so it has proved it can reach the backend. The Validating
  phase checked that it supports importing from this backend.
- It remains after a provider-change migration. ADR-0010 also keeps it from being
  deleted while the migration needs it (D8).

There is no fallback to the source provider.

### D3 — When the manager calls it

Only for the `nfs` and `s3` backends, and only when a staged object **may exist**:

- The manager sets the `StagingCleanup` condition to `Unknown` (reason
  `StagedDiskMayExist`) **before** it builds the export URL and sends `ExportDisk`.
  In the same status write it records `status.storageInfo.stagingLayout` (D5) and
  `status.storageInfo.stagingDigest` (D5). An export that fails can still leave a
  partial object.
- A migration from before this release has no such condition. If its `Exporting`
  condition is `True`, a staged object is assumed to exist.

| Event | `cleanupPolicy: Always` | `OnSuccess` (default) | `Never` |
|-------|------------------------|-----------------------|---------|
| Migration `Ready` | delete | delete | keep |
| Terminal failure (no retries left) | delete | keep until the `VMMigration` is deleted | keep |
| Retry pending (non-terminal `Failed`) | keep (the retry overwrites it) | keep | keep |
| `VMMigration` deleted | delete | delete | keep |

This gives `Always` and `OnSuccess` their documented difference for the first time.
A failed migration's staged disk is kept for diagnosis or a manual import, and is
deleted when the `VMMigration` is deleted. `Never` keeps it in every case: the
condition becomes `StagingCleanup=True`, reason `StagedDiskRetained`, and no RPC is
sent.

All exports are synchronous RPCs today, so a deletion never races an export that the
manager is still waiting on (reconciles of one object are serialized). A deleting
`VMMigration` whose export returned a task (`status.taskRef` in phase `Exporting`)
is **held until the source provider reports the task finished**: the task would
otherwise write the full object after the delete. force-delete releases it.

### D4 — A failed delete is held

This copies the source-snapshot hold from #378:

- **Condition** `StagingCleanup=False`, reason `StagingCleanupPending`, with a
  **constant** message. It never contains a bucket, key, path, endpoint, provider
  answer or delay. The cause is written only to the manager log.
- **One Warning event** (`StagingCleanupPending`), sent when the hold begins.
- **Backoff**: the blocked-VM backoff (`blockedRetryBackoff`), 15 s doubling to 5 min,
  measured from the condition's `LastTransitionTime`. A reconcile caused by the hold's
  own status write does not send the RPC again before the backoff has passed.
- **Deletion**: the `VMMigration` finalizer is kept while the delete can still
  succeed. `virtrigaud.io/force-delete=true` on the `VMMigration` releases it, with a
  `StagedDiskLeftBehind` Warning that names no location.
- **Ready / terminal failure**: the hold retries in the background. It never blocks
  the source-snapshot cleanup or `deleteAfterMigration`.
- **Success**: `StagingCleanup=True`, reason `StagedDiskDeleted` (`DELETED` or
  `NOT_FOUND`) or `StagedDiskEmptied` (`EMPTIED`), and one Normal event.

The delete is **released without deleting**, with one `StagedDiskLeftBehind` Warning
(constant message) and `StagingCleanup=True`, reason `StagedDiskLeftBehind`, when it
can never succeed:

- the target `Provider` does not exist (in the migration's own namespace);
- the provider does not support it, and has not for 24 hours (D7);
- the provider refuses the request (`InvalidArgument`);
- the S3 credentials Secret no longer exists (for example during a namespace
  deletion);
- the staging host is denied by the SSRF host policy;
- `spec.storage` or `spec.target.providerRef` changed since the export (the
  recorded staging digest no longer matches, D5);
- a migration exported by an older manager (no digest recorded) is answered
  `NOT_FOUND` at its current spec: that proves nothing about the object, so it
  is never reported deleted.

A **hold** is used when the delete can still succeed: the provider cannot be
reached, the capability query fails, a cross-namespace `Provider` is not granted,
the Secret cannot be read, the staging host name cannot be resolved (DNS can be
transient), the export task still runs, or the provider returns any other error.

Every outcome on the deletion path is persisted (`StagingCleanup=True`) **before**
the finalizer is removed, so a finalizer update that conflicts never re-sends the
RPC or the event. Causes that can name a host, path or provider answer are logged at
V(1) (manager) or debug (providers); only the constant reason is logged at
Info/Warn.

### D5 — Confinement: the provider names the object (security-critical)

The request carries the **migration identity**, not a location. The provider derives
the object with the same function the manager uses to build the export and import
URLs (`internal/storage/migration.StagedObjectKey`, the single source of truth for
the layout):

- `namespace` must be a DNS-1123 label and `name` a DNS-1123 subdomain. `stage` must
  be in an allowlist (`export`). Anything else is `InvalidArgument`.
- **s3**: the key is `<prefix>/vmmigrations/<namespace>/<name>/export.vmdk`. Whatever
  prefix the tenant sets, the key always **ends** with the migration's own
  namespace and name. The delete runs with the tenant's own credentials, in the
  bucket those credentials can write.
- **nfs**: the file is `<export>/<path>/vmmigrations_<namespace>_<name>_export.qcow2`
  (layout `v2`). It is built through `NFSURL`, which rejects `..` segments, URL
  delimiters, whitespace and control characters (ADR-0006 C7'). Proxmox then
  resolves the path relative to its kernel mount and rejects traversal again
  (`nfsInMountRel`).
- Before every call the manager checks the S3 endpoint or NFS server against the SSRF
  host policy again (ADR-0006 C3), because `spec.storage` could have changed since
  Validating.

A tenant can therefore only cause the deletion of objects named like a VirtRigaud
staging object **of a migration in its own namespace**, on a backend it already
writes to with `ExportDisk`.

**Staging layouts.** The legacy NFS name (`v1`,
`vmmigrations-<namespace>-<name>-<stage>.qcow2`) was ambiguous across namespaces:
namespace `team-prod` with migration `db` and namespace `team` with migration
`prod-db` stage the same file, so one tenant's export, import or delete could reach
the other's object on a shared export. Layout `v2` uses `_`, which appears in no
namespace (DNS-1123 label) and no name (DNS-1123 subdomain), so the name is
unambiguous. The manager records the layout with the staging marker
(`status.storageInfo.stagingLayout`): a new migration gets `v2`; a migration that an
older manager may already have staged under the `v1` name keeps `v1` for its import
and its delete. The layout travels on `DeleteStagedDiskRequest.layout`. The S3 key
(`/`-separated) is the same in both layouts.

**Staging digest.** The delete is built from the migration's **current** spec, which
a tenant can edit after the export. The manager therefore records, with the marker,
`status.storageInfo.stagingDigest`: a SHA-256 over the backend, its non-secret
options and the target `Provider`'s namespace, name and UID (it names no location).
The delete is sent only while the current spec and target `Provider` yield the same
digest; otherwise the staged disk is recorded `StagedDiskLeftBehind` and no delete is
aimed elsewhere. A migration without a digest (exported by an older manager) is
deleted best effort, and a `NOT_FOUND` answer is recorded as left behind, never as
deleted.

**Residual risk.** The S3 delete removes the current version only: in a
**versioned** bucket it adds a delete marker and the data stays as a noncurrent
version, and an **object-lock** retention keeps it for the retention period. Enable
a lifecycle rule that expires noncurrent versions on the `vmmigrations/` prefix, or
use an unversioned staging bucket. On NFS, a server-side **snapshot** of the export
keeps the staged file after it is deleted or emptied; exclude the staging export
from snapshots, or expire them. libvirt and vSphere reach NFS through libnfs, which
resolves a **symbolic link** that the NFS server serves at the staging name, and
qemu-img offers no `lstat`: a link there (planted by whoever can write the export)
makes the emptying truncate its target instead — another file **on the same NFS
server** that the presented `uid`/`gid` can write, never a host or pod file. Use one
export per tenant, as ADR-0006 C6 already recommends. Proxmox mounts the export
`nosymfollow` and refuses a non-regular file.

### D6 — How each provider deletes

| Provider | `s3` | `nfs` |
|----------|------|-------|
| libvirt (single host) | stat, then remove the object (pod is the S3 client) | **empty**: `qemu-img create -f raw <nfs-url> 0` on the host, as the SSH user, with the migration's `uid`/`gid` in the URL, the same identity as the import |
| vSphere | stat, then remove the object | **empty**: the same `qemu-img` command in the provider pod |
| Proxmox | stat, then remove the object | **delete**: kernel mount on the node, then `rm -f` as the migration's `uid`/`gid` (`setpriv`) |
| libvirt (clustered) | `Unimplemented`: a clustered provider is never a migration target (its `ImportDisk` is not routed yet) | `Unimplemented` |
| mock | `Unimplemented`: the mock stages only to `pvc` | `Unimplemented` |

libvirt and vSphere reach NFS only through qemu-img's libnfs driver, and qemu-img
cannot unlink a file. They first run `qemu-img info` on the object. A missing object
is `NOT_FOUND`, an empty one is already `EMPTIED`, and any other object is truncated
to zero bytes. The data is gone, but a 0-byte file with the staging name stays on the
export until the next migration with the same name overwrites it, or an operator
removes it. The maintainer accepted this (`EMPTIED`) for v0.4.0-rc1 on 2026-10-12.
Using a new NFS client instead (a Go NFSv3 library, or `libnfs-utils` on
every host) would add a dependency and a network path from the libvirt provider pod
that the deployment does not have today.

### D7 — Capability negotiation

`GetCapabilitiesResponse.supported_staged_disk_delete_backends` (field 23, a repeated
string) lists the backends a provider can delete from. The manager sends the RPC only
when:

- the provider client implements `contracts.StagedDiskDeleter` (the type assertion),
  **and**
- the live `GetCapabilities` lists the migration's backend.

An older provider image reports nothing, or answers `Unimplemented`. During a rolling
upgrade the manager runs before the provider pods are rolled, so the manager **holds**
the delete (the normal hold and backoff) for up to **24 hours** from the start of the
hold. A provider rolled in that window deletes the object. After 24 hours the manager
records `StagedDiskLeftBehind` once (Warning event and condition) and releases. A
deleting `VMMigration` can be released earlier with `virtrigaud.io/force-delete=true`. The new
field is not added to `Provider.status.reportedCapabilities`, so there is no CRD
change. The manager reads it live, like `supports_list_owner_filter`.

### D8 — The Provider is kept while the cleanup is pending

ADR-0010's `migrationHoldsProvider` also returns true for a `Ready` or `Failed`
migration whose staged disk may still exist and whose policy will delete it (D3). The
target `Provider` cannot be deleted under a pending staging cleanup: deleting it would
turn a pending delete into a leftover object. With `OnSuccess`, a terminally failed
migration keeps its `Provider` until the migration is deleted.

### D9 — Post-migration source cleanup only for migrations this manager completed

Fixing the `Ready` cleanup (see Context) means a migration completed long ago would
delete its source snapshot and — with `spec.source.deleteAfterMigration` — its source
VM on the first reconcile after the upgrade. The maintainer decided on a code guard:

- The status write that sets `Phase=Ready` also sets the condition
  `PostMigrationCleanup=False` (reason `PostMigrationCleanupPending`). `handleReadyPhase`
  deletes the source snapshot and the source VM **only** when that marker is present,
  and sets it `True` (`PostMigrationCleanupComplete`) when done.
- A `Ready` migration **without** the marker (completed by an older release) deletes
  nothing. Its cleanup is closed for good (`status.storageInfo.cleanedUp`,
  `PostMigrationCleanup=True` with reason `SourceDeletionSkippedPreUpgrade`), with one
  `SourceDeletionSkippedPreUpgrade` Warning when something would have been deleted.
  Its staged disk is still cleaned up (D3, D5).
- The Validating phase records the source VirtualMachine's UID
  (`status.sourceVMUID`). `deleteAfterMigration` deletes the source VM with a UID
  precondition, after a live read: a VirtualMachine re-created under the same name
  is never deleted (one `SourceDeletionRefused` Warning). A migration validated
  before the field existed deletes the source VM only when it is older than the
  migration.
- The Ready cleanup acts only on the latest object (a live read of the migration):
  a cache that has not caught up with an earlier pass's status write never repeats an
  RPC, an event or a delete.

### D10 — Staged images are checked before they are imported

The staged object comes from a backend the tenant controls (its NFS server or
bucket), so its header is untrusted. A qcow2 backing file, an external data file or
a VMDK extent can name any path or protocol the importing process can open: a node or
pod file (`/etc/pve/priv/*`, `/root/.ssh/*`, a service-account token, a block
device), or a `json:`/`nbd:`/`http:` target that bypasses the SSRF policy. `qemu-img
convert` (or `qm importdisk`, which probes its source) would flatten its contents
into the tenant's imported disk. Every NFS and S3 import therefore:

1. reads the tenant-controlled object **once**, as a **bounded** byte copy to a local
   file the tenant cannot change (libvirt: `qemu-img dd -f raw -O raw` into the
   private write directory; vSphere: the same into the pod; Proxmox: `head -c`
   through the `nosymfollow,nosuid,nodev,noexec` kernel mount, refusing a
   non-regular file, into a root-owned temp in the root-only stage directory that
   is renamed onto the stage (the stage builder of the imported-disk confinement,
   #384); S3: the download). The bound: the object's size is read first
   (`qemu-img info -f raw`, or `stat` on the mount — neither opens anything else)
   and refused above the copy limit (`VIRTRIGAUD_MIGRATION_IMPORT_MAX_GIB`, default
   1024 GiB, lowered by the import's `max_virtual_size_bytes` (#382). That limit is
   a virtual size, so it gets room for the image's metadata: + 1/64 + 16 MiB.
   See `migration.StagedCopyLimit`.) or when less than twice its size is free where it
   is copied (the Proxmox stage directory `/var/lib/virtrigaud/import-stage`, the pool directory, the pod's `/tmp` — a full pod
   `/tmp` evicts the provider for every tenant; the vSphere pod measures `/tmp`
   against its emptyDir `sizeLimit`, passed as `VIRTRIGAUD_TMP_SIZE_LIMIT`, not
   the node's free space). The copy reads at most the **measured** size + 1 byte
   (Proxmox `head -c`) or + under 1 MiB (`qemu-img dd`), never up to the limit — a
   tenant NFS server can report a small size and serve more — and a copy longer
   than measured is refused. S3 downloads are bounded the same way by the
   object's reported size (`BoundedWriter`), with the same free-space pre-check.
   On Proxmox the migration's `uid`/`gid` (`setpriv`) only measures the object (a
   regular file, and its size). The bound and the free space are checked by root:
   the stage directory's ancestors are created under `umask 077`, so a
   dropped-privilege `df` could not reach it. The copy is in the pipe form
   (`setpriv … cat -- src | head -c $((SZ + 1)) > "$TMP"`): root opens the copy,
   and the dropped-privilege reader holds only a pipe.
   Too large is `InvalidArgument`, no space `ResourceExhausted`; both constant;
2. checks that copy's header **in Go first, without qemu**
   (`migration.CheckStagedImageBytes` on its first 64 KiB): qemu opens a qcow2
   external data file and VMDK extents while it reads a header, so `qemu-img info`
   on a crafted image could open a device node, a host file or an `nbd:`/`http:`
   target — as root on a PVE node — before any check refuses it. qcow2: magic,
   version 2 or 3, no backing file, not encrypted, no external data file (bit or
   header extension), only known incompatible features, extensions terminated in
   the bytes read. VMDK: only the single-file sparse variants (monolithicSparse,
   streamOptimized), a non-zero capacity (qemu reads a zero-capacity header with a
   descriptor offset as a descriptor FILE, opening every extent), no grain
   directory at end (qemu would replace the header with a footer the check cannot
   read; qemu-img's own streamOptimized output, which the vSphere export writes,
   does not use it), a descriptor NUL-terminated within its declared size, at most
   one SPARSE extent whose file name is a plain name (the file itself), no parent —
   also not in the fixed region qemu searches for one. Raw: must not start with
   another format's magic, nor read as a VMDK descriptor by qemu's text probe.
   The declared virtual size (qcow2 size, VMDK capacity) is returned and refused
   above the copy limit, which includes #382's limit. #382's exact check runs on
   the disk as it will be landed. Only then `qemu-img info -U` (on Proxmox under
   `unshare -n`) and `migration.CheckStagedImageHeader` / libvirt's
   `inspectHostImageAs` check it again: exactly the expected format, no backing
   file, no data file, no extent outside the file. A refusal is `InvalidArgument`;
3. converts or imports **that same copy**, with an explicit source format.

On Proxmox the checked copy IS the stage: it stays in the root-only stage directory
until `Create`, and a refused or failed check removes it. `Create` parses and
checks the stage's header again, in the format of the stage's extension, before it
builds anything: `qm importdisk` reads the stage as root and probes it. A refused
stage is removed (`InvalidSpec`, nothing is created). A stage that cannot be read
is a VM-scoped not-performed answer, and the stage is kept for the retry.

Libvirt already had the header check, but its convert read the NFS object a second
time; Proxmox and vSphere had none. These were pre-existing.

**VMDK grain directory at end.** VirtRigaud's own vSphere export stages one of two
VMDKs. The first is a `sparseMonolithic` clone (`CopyVirtualDisk`), uploaded as is.
The second is a multi-file VMDK flattened by `qemu-img convert -O vmdk -o
subformat=streamOptimized`. Both keep the grain directory offset in the header.
qemu-img 8.2 writes `gd_offset` = sector 30 for both subformats; a test checks this
against real qemu-img output. Only VMware NFC/OVF streamOptimized exports use "grain
directory at end" with a footer, and VirtRigaud does not use them. Such a file,
staged by hand, is refused.

### D11 — The controller's status fields are trusted: tenants never write them

The staging marker (`StagingCleanup`), `status.storageInfo.stagingLayout`,
`status.storageInfo.stagingDigest`, `status.sourceVMUID` and the
`PostMigrationCleanup` marker decide what the controller deletes and where. They are
written by the controller only, through the `vmmigrations/status` subresource.
**Never grant tenants (or any role but the manager's) `update`/`patch` on
`vmmigrations/status`**: with it, a tenant could stamp a migration completed by an
older release so its source VM is deleted, point `sourceVMUID` at another
VirtualMachine's UID in its namespace, or make the digest match an edited spec. The
shipped chart and kustomize RBAC grant `vmmigrations/status` to the manager only;
check any custom aggregated or tenant roles.

## Consequences

- An `nfs` or `s3` migration no longer leaves a copy of the tenant's disk behind by
  default. On libvirt and vSphere NFS targets, a 0-byte file with the staging name
  remains.
- `Ready` cleanup works for migrations completed by this release: the source
  snapshot is deleted and `deleteAfterMigration` runs. Before, both were blocked by
  the manager-side PVC client. Migrations completed before the upgrade delete
  nothing (D9).
- A terminally failed migration with the default `OnSuccess` keeps its staged disk
  until the `VMMigration` is deleted, and keeps its `Provider` until then. Use
  `cleanupPolicy: Always` to remove the disk at once.
- Deleting a `VMMigration` can now be held while its target provider is unreachable.
  `virtrigaud.io/force-delete=true` releases it.
- Staged objects written before this release are deleted, best effort, only for
  migrations whose `Exporting` condition is `True` and that still exist after the
  upgrade. Older objects must be removed by hand. A bucket lifecycle rule on the
  `vmmigrations/` prefix (including noncurrent-version expiry) stays the recommended
  backstop.
- New NFS migrations stage under the `v2` name; a `v1` file is used only by
  migrations staged before the upgrade.
- The `pvc` backend is unchanged: the migration's own PVC is deleted with the
  `VMMigration`, and a staged file on a user-supplied PVC is left there.
- Not validated on real NFS or S3 servers in this change. The provider tests use an
  in-process S3 fake, a recorded host command runner, and a fake `qemu-img`.
