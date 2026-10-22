<!--
Draft release notes for the VirtRigaud release after v0.4.0-rc2. The version
label is not decided: it is v0.4.0-rc3 or v0.4.0 GA (rc3 or GA - maintainer's
call). rc1 and rc2 were published from earlier versions of this file; rc1
followed v0.3.11. Paste everything below this comment into the GitHub release
once the label is chosen: search this file for "maintainer's call" for the
sentences that depend on it. The notes are cumulative since v0.3.11: a user going
from v0.3.11 needs all of them, while "Changes since v0.4.0-rc2" is the short
list for someone already on rc2 and "Changes since v0.4.0-rc1" the one for
someone on rc1. See docs/upgrading.md for the full upgrade guide these notes
link to, and CHANGELOG.md for the complete, dated changelog. For the GA
release, drop the rc sections and re-title.
-->

This is the release after **v0.4.0-rc2** <!-- rc3 or GA - maintainer's call: name the
release here, and keep or drop the release-candidate wording in this paragraph and in
"About this release candidate / what to test" -->. It is a
security-hardening release: a full codebase security review found and fixed several
cross-tenant and privilege-escalation issues in the vSphere and libvirt providers,
cross-namespace clone/migration targets, and VM provider binding. rc2 added two waves
of fixes on top of rc1 (below): one about proving that the VM a migration, adoption,
clone or per-VM call acts on is the one VirtRigaud owns, and one about failures
half-way through an operation (a resize that hit the wrong disk, a provider call whose
result was lost, a failed migration's leftover disk) and about provider pods' access to
migration storage. This release adds a third, on top of rc2: libvirt cloud-init seeds
(private, and a clone's own identity), leftover files, and migration into a clustered
libvirt Provider (experimental). **Several fixes are breaking
or change default behavior.** Read the upgrade guide before upgrading:
**[Upgrading from v0.3.11](docs/upgrading.md)**.

The notes below are cumulative since v0.3.11. If you are already on v0.4.0-rc2, read
"Changes since v0.4.0-rc2" first; on v0.4.0-rc1, read it and then "Changes since
v0.4.0-rc1".

## Changes since v0.4.0-rc2

Nine pull requests, merged 2026-10-19 to 2026-10-22. Three themes. **Cloud-init
seeds** (#408, #413, #414): the user-data of a libvirt VM, which can carry
passwords and keys, was readable by any local user of the hypervisor host; a
clone booted with its source's SSH host keys; and a delete left seeds behind.
**Leftover disk files** (#409). **Migration into a clustered libvirt Provider**
(#407, #412, #415, #416), an experimental capability. One change is breaking
(#414: a directory must exist on every libvirt host before the providers roll).
Apply them in the usual order, with one addition at the front: **the seed
directory on every libvirt host, then CRDs, then the manager, then the
providers** (see [Upgrade order for this release](#upgrade-order-for-this-release)).

| PR | What changed | Breaking |
|---|---|---|
| #414 (R27) | libvirt cloud-init seeds are built private (`0600`) in a dedicated directory, `/var/lib/libvirt/virtrigaud-seeds`, which must exist on every libvirt host and is checked before every create and clone | **Yes** |
| #413 (R26) | A clone gets its own cloud-init seed and a new cloud-init identity, so it no longer shares its source's SSH host keys (libvirt single-host and clustered, vSphere; Proxmox already did). `create` on `VMClone` is read access to the source's data | Behaviour change |
| #408 (R24) | A single-host libvirt `Delete` removes the VM's cloud-init seed directory after the undefine, also when a VM and its clone are deleted together | Behaviour change |
| #409 (R23) | vSphere removes the import VM's history log (`<id>-<hex>.hlog`); a migration that reaches `Ready` closes `LandedDiskCleanup` (`LandedDiskInUseByTarget`) | Behaviour change |
| #415 (R11, B.1+B.2) | A clustered libvirt provider serves `ImportDisk`, `DeleteLandedDisk` and `DeleteStagedDisk` on a host the manager names (new proto fields and capability `supports_routed_import`). Nothing calls it until #416 | No |
| #416 (R11, B.3) | A `VMMigration` can target a clustered libvirt Provider: the target VM is created and placed first, the disk lands on its host. New status field and condition, a CRD relaxation (empty `diskID`), and `allocatableStorageBytes` is now the `default` pool only | Behaviour change |
| #407, #412 (R11) | ADR-0007 Addendum B: the design, proposed, then accepted with decisions on its open questions | Documentation only |
| #411 | Go 1.26.10 (GO-2026-6809, `crypto/x509`, reachable through `x509.Certificate.Verify`); builder images in step | No |

### libvirt cloud-init seeds are private and live in a dedicated directory (#414, breaking)

Before, a VM's seed ISO was built under the world-listable `/tmp`, with the
file at the SSH user's umask (usually `0644`) and `user-data` and `meta-data`
left next to it. Any local user of a shared hypervisor host could read the
user-data. `/tmp` is also emptied at boot or aged out (Ubuntu 24.04), after
which the VM no longer starts.

Seeds are now built in `VIRTRIGAUD_LIBVIRT_SEED_DIR` (default
`/var/lib/libvirt/virtrigaud-seeds`). The ISO is `0600`, `user-data` and
`meta-data` are deleted once it is built, and libvirt's `dynamic_ownership`
(the default) hands the ISO to the qemu user when the VM starts. The provider
**fails closed**: before it writes anything for a create or a clone it checks
the directory on the host, and refuses one that is missing and cannot be
created, not owned by the SSH user, readable or writable by its group or
others, or below a directory other users can write. Every create and clone on
that host then fails, retryably, with `refusing to stage cloud-init data`.

**Before rolling the libvirt providers**, on every libvirt host (single-host,
every host of a clustered Provider, and every migration target host), as root:

```sh
install -d -m 0711 -o <ssh user> -g <ssh user> /var/lib/libvirt/virtrigaud-seeds
```

- Keep the directory on a local filesystem, never NFS.
- On a host shared by Providers with different SSH users, give each Provider its
  own directory through `spec.runtime.env`.
- It holds every VM's user-data in plaintext: exclude it from backups or encrypt
  them.
- Seeds of existing VMs stay in `/tmp` and stay readable until you make them
  private; the commands, and how to move one into the new directory, are in
  [Leftovers from earlier releases](docs/upgrading.md#leftovers-from-earlier-releases).

Run in the lab on a libvirt 10.0 / Ubuntu 24.04 host with AppArmor: the
`dynamic_ownership` hand-over, no AppArmor change needed, the seed unreadable by
another local user, the refusal of an unusable directory, and the delete of a
legacy `/tmp` seed. Not run: an SELinux host.
→ [`docs/libvirt-domain-ownership.md`](docs/libvirt-domain-ownership.md#cloud-init-seed-directory),
[Upgrade guide](docs/upgrading.md#breaking-changes)

### Clones get their own seed and a new cloud-init identity (#413)

A libvirt or vSphere clone used to boot with its **source's** cloud-init
`instance-id` (the copied disk already records it). cloud-init then skipped its
per-instance modules, so the clone kept the source's SSH host keys and
hostname: two VMs, one host identity. On single-host libvirt the clone's CD-ROM
also pointed at the source's seed, which tied the two VMs' lifecycles together
(the cause of #408).

Now the clone's `instance-id` is `virtrigaud-<target VirtualMachine uid>`, its
hostname is the target's (or `spec.customization.hostname`), and:

- **libvirt** (single-host and clustered): the clone gets a seed of its own,
  built from the source's, in the new seed directory. The source's seed is read on
  the host with `isoinfo` (from `genisoimage`, which a create already requires).
  `spec.customization.userData.cloudInit.inline` is used as the clone's user-data
  when set (at most 1 MiB).
- **vSphere**: the clone's `guestinfo.metadata` and a `#cloud-config`
  `guestinfo.userdata` hostname are rewritten.
- **Proxmox** is unchanged: PVE already gives a clone a new instance-id.

A clone's first boot runs every per-instance module of its user-data again
(`runcmd`, `packages`, `users`, ...), and its SSH host keys are regenerated.
`/etc/machine-id` is **not** regenerated: seal the source if clones need their own.
Existing clones keep the identity they booted with.

> **Security: `create` on `VMClone` is read access to the source VM's data.**
> The clone's disk is a full copy of the source's, and on libvirt the
> `VMClone`'s inline user-data runs as root on its first boot. Before upgrading,
> check who can create `VMClone`s
> (`kubectl auth can-i create vmclones.infra.virtrigaud.io -n <namespace> --as <subject>`)
> and narrow the grant where that is more than intended. It holds within one
> namespace as much as across namespaces.

Tested with unit tests, a round trip through the real `genisoimage` and
`isoinfo`, and the vSphere simulator. Checking a real cloned guest is still open (#413).
→ [`docs/vm-ownership.md`](docs/vm-ownership.md#a-clones-host-identity),
[`docs/libvirt-clones.md`](docs/libvirt-clones.md#a-clones-cloud-init-identity),
[Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

### Leftover seeds and history logs (#408, #409)

- **#408.** A single-host libvirt `Delete` of a VM and its clone together left
  `<staging>/virtrigaud-cloudinit-<domain>.<random>/` on the host: each delete
  kept the seed because the other domain still referenced it. After the
  undefine, the delete now removes the seed directory its definition referenced
  and any directory named exactly after the domain once no defined domain
  references it. In a single-host provider, `Create` and `Delete` of one domain
  name now also run one at a time (`Clone` already did). Seeds leaked by earlier
  releases are not removed (see the Leftovers list).
- **#409.** After an S3/NFS import, vCenter left the import VM's history log
  (`<id>-<hex>.hlog`, about 300 bytes) in the landing folder; it is now
  removed. A migration that reaches `Ready` now records
  `LandedDiskCleanup=True/LandedDiskInUseByTarget` in the same status write, and
  one that reached `Ready` under rc2 is closed once; before, it stayed
  `Unknown` for good. Deleting a `Ready` migration never deletes the disk.
  Logs left by rc2 imports stay; they are harmless.

### Migration into a clustered libvirt Provider (#407, #412, #415, #416; experimental)

A `VMMigration` whose target Provider has `topology: cluster` used to fail at
`Validating`. It now works, for `s3` and `nfs`:

```yaml
spec:
  target:
    providerRef:
      name: libvirt-cluster   # a libvirt Provider with spec.topology: cluster
  storage:
    type: nfs                 # or s3; pvc (also what an unset storage means) is refused
```

For a clustered target the migration creates its **target VirtualMachine
first**, at `Validating`, before the source is powered off, snapshotted or
exported. The ordinary scheduler places it on a host whose `default` pool has
room for the source disk (`Host.status.allocatableStorageBytes`), and its
capacity is reserved from then on. The migration waits with a new condition,
`TargetPlaced`, until the target has a pending host. Then it exports and imports
the disk **on that host** (`ImportDisk` carries the new `target_host_id`), records the
landed disk on the target VM's `spec.importedDisk`, and the VM is created on that
host with the disk attached in place. The host is recorded in
`status.storageInfo.importTarget.host`, which the re-send and the landed-disk and
`nfs` staged-disk cleanup use. Single-host, vSphere and Proxmox targets keep
import-then-create, unchanged.

What you need, and what you see:

- **The target Provider must report `supportsRoutedImport`** (roll the clustered
  provider first). One that does not fails the migration at `Validating`
  (`ClusteredTargetUnsupported`) before any side effect; so does a target VM in
  another namespace than the VMMigration's.
- **Each host**: the `default` pool at `/var/lib/libvirt/images`, active, with
  room; an `ssh://` endpoint; `qemu-img`, `flock` and `timeout`; the seed
  directory above. The provider runs one replica.
- **Conditions and reasons** you can meet: `TargetPlaced`, `WaitingForImport`,
  `MigrationNotFound`, `ImportedDiskPinned`, `TargetHostReleased`,
  `TargetHostUnavailable`, `TargetNotPending`, `TargetHostMoved`,
  `TargetDiskMismatch`, `SourceDiskSizeUnknown`, `ClusteredTargetUnsupported`,
  `ImportTargetPreviousIncarnation`, `LandingDiskInUse`, `LandingHostRefused`,
  `ImportNotPerformed`. Each is
  explained, with what to do about it, in the guide linked below. A host that fails
  after placement is **waited for, not replaced**. Consecutive "not performed"
  answers for 5 minutes fail the attempt into `spec.options.retryPolicy`.
- **CRDs.** `VMMigration.status.storageInfo.importTarget.host` is new, and
  `spec.importedDisk.diskID` may now be empty while `migrationRef` is set
  (`VirtualMachine`, `VMSet`, `VMMigration`; an additive relaxation). Apply the CRDs
  before the manager: the readiness check does not verify these.
- **`Host.status.allocatableStorageBytes` is now the `default` pool's free space
  only** (it summed every active pool). It is the input of the disk-space filter,
  and it changes the figure of every libvirt host and what a
  `VMPlacementPolicy` `minDiskSpacePerHost` compares against. Review any such
  policy.

**Limits.** The disk-space check is a filter, not a reservation: two migrations
can be placed on a host that holds only one, and growth of the source after
placement is not re-checked. Two Providers whose hosts mount one pool are not
serialised with each other. A target VM's `spec.importedDisk` can be edited by
whoever may edit the VM; a swap is detected (`TargetDiskMismatch`) and never
completes the migration, but not prevented. `topology: cluster` stays experimental.

**Run in the lab** (two clustered libvirt hosts sharing one NFS pool, build
`main-73e08e8`, 2026-10-22): a single-host libvirt VM migrated into the cluster
over `s3` and over `nfs`, both to `Ready`; a failure after the import, whose
landed disk was deleted through the recorded host; a host made unreachable after
the target was placed (the migration waited, did not re-place, and went on when the
host returned). **Not run:** a vSphere or Proxmox source into a clustered target,
a host made unreachable after the disk landed, a manager restart in the middle of
an import. These are covered by unit and envtest tests only.

**Known issue (R31, being fixed).** libvirt reports a pool's `Available` as last
refreshed, so on an NFS pool shared by several hosts, hosts can report different,
stale free space to the disk-space filter. An administrator can refresh the figure
with `virsh pool-refresh default` on the hosts.
→ [Migration into a clustered provider](docs/clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)
(prerequisites, every condition and how to clear it, limits),
[ADR-0007 Addendum B](docs/adr/0007-clustered-orchestrator-provider.md#addendum-b-2026-10-18-migration-into-a-clustered-provider-p3-cold-path),
[`examples/vmmigration-into-clustered.yaml`](examples/vmmigration-into-clustered.yaml),
[Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

### Go 1.26.10 (#411)

The blocking supply-chain scan failed on GO-2026-6809, a standard-library
vulnerability in `crypto/x509` that is reachable through
`x509.Certificate.Verify` (every TLS path). Go moves from 1.26.9 to 1.26.10 in
`go.mod`, the proto and SDK modules, the Makefile builder image and the five
Dockerfiles. No dependency changed. Every image is rebuilt, so every pod rolls.

### Upgrade order for this release

Same as for rc2 (**CRDs, then the admission policy and any grants, then the manager,
then the providers promptly**), with these additions:

1. **First, on every libvirt host: create the seed directory** (command above). It
   depends on nothing else, so do it first; it must exist before any libvirt provider
   image from this release runs. Until it exists,
   every create and clone through that host fails with `refusing to stage
   cloud-init data`. VMs that already run are not affected.
2. **CRDs.** Three CRDs changed (`VirtualMachine`, `VMSet`, `VMMigration`). Apply
   them before the manager; the manager's readiness check does not verify the new
   schema, but a clustered-target migration does not work without it.
3. **The manager, then the providers.** Roll the **clustered libvirt provider
   before you run a migration into it**: an older one is safe but is refused as a
   target (`ClusteredTargetUnsupported`). Every provider pod rolls (Go 1.26.10).
4. **Review who can create `VMClone`s** (above) before clones start carrying
   their own user-data.
5. **Review `VMPlacementPolicy` `minDiskSpacePerHost`** on clustered Providers:
   it now compares against the `default` pool alone.
6. **Leftovers** from earlier releases are not cleaned up: seeds in `/tmp`, and
   seed directories of VMs deleted together with a clone
   ([Leftovers from earlier releases](docs/upgrading.md#leftovers-from-earlier-releases)).

**Rollback.** Rolling the manager back with a clustered-target migration in flight:
finish or delete those migrations first (an older manager does not know
`WaitingForImport`; the design expects the provider to refuse its `Create`, and
that path is not covered by a test). Rolling a clustered provider back makes it
unavailable as a target, and a landed disk of a migration that was importing is
recorded as left behind. Rolling a libvirt provider back to before #413 and #414
builds seeds in `/tmp` again and makes clones share their source's identity
again; seeds already in the new directory keep working.
→ [Rollback caveats](docs/upgrading.md#rollback-caveats)

## Changes since v0.4.0-rc1

Thirteen pull requests in two waves. Wave 1 (#392–#395) is about ownership: proving that the
VM a migration, adoption, clone or per-VM call acts on is the one VirtRigaud owns. Wave
2 (#397–#405) is about what happens when something goes wrong half-way: a resize that
hit the wrong disk, a provider call whose result was lost, a failed migration that left
its disk behind, storage and files a tenant could redirect, and an empty `PHASE`
column. Two changes are breaking (#394, #401); the rest change behaviour you can
observe. Apply them in the usual order: **CRDs, then the admission policy and any
grants, then the manager, then the providers promptly after it** (see
[Upgrade order for rc2](#upgrade-order-for-rc2)).

| PR | What changed | Breaking |
|---|---|---|
| #394 (R1) | A `VMMigration` accepts only the target VM it created; a new admission policy reserves the manager-owned `virtrigaud.io/*` keys | **Yes** |
| #393 (R3) | Every per-VM call on single-host Proxmox is owner-checked; the manager sends the owner on all single-host calls | Behaviour change |
| #392 (R2) | Single-host adoption stamps the adopted VM with its new owner | Behaviour change |
| #395 (R4) | Single-host `VMClone` targets are created first and stamped; a re-sent `Clone` re-binds | Behaviour change |
| #401 (R5) | `Reconfigure` resizes the right disk (vSphere: named additional disks only; Proxmox: no longer touches `scsi0`); vSphere reports a change a running VM cannot take as `ResizePendingPowerOff` | **Yes** |
| #402, #405 (R6, R21) | A `SnapshotCreate`, `ExportDisk` or `ImportDisk` whose outcome is unknown is held for an operator instead of re-sent blindly; single-host libvirt answers the failures it can prove as definite | Behaviour change |
| #403 (R7) | A failed or deleted `VMMigration` deletes the disk its import landed; vSphere imports leave no import-VM files | Behaviour change |
| #399 (R15) | Provider pods mount only the migration PVCs a live `VMMigration` references; the target mounts read-only; a user-named PVC is never deleted | Behaviour change |
| #404 (R17) | Staged migration files on the `pvc` backend are opened with no symbolic link followed; hard links and special files are refused | Behaviour change |
| #398 (R10, R16) | `VirtualMachine` `status.phase` is set on every status write; the libvirt keepalive drops a dead client | Behaviour change |
| #400 (R8, R9) | vSphere uses one finder per call (race fix); libvirt passes host XML over stdin instead of fixed `/tmp` files | No |
| #397 (R14) | ADR-0012 (Accepted): the Proxmox legacy name rule is retired in stages from v0.4.1; ACL guidance for v0.4.0 | Documentation only |

The sections below cover Wave 1 first, then Wave 2 (starting at [Reconfigure resizes the right disk](#reconfigure-resizes-the-right-disk-and-vsphere-says-when-it-needs-a-power-off-401-breaking)).

### A VMMigration accepts only the target VM it created (#394, breaking)

Before, a migration treated **any** `VirtualMachine` with the target's name as its own
target. Someone with create rights in the target namespace could plant a same-named VM;
the migration then attached the landed disk to it and reported against it. The
provenance annotation and `spec.importedDisk` were forgeable.

Now the migration records the UID its own `Create` returned (`status.targetVMUID`). Before
the `Create` it records the SHA-256 digest of a random claim token
(`status.targetVMClaim`; the token itself goes only on the VM it creates, as the
annotation `virtrigaud.io/migration-claim`), so a `Create` whose UID write was lost to a
conflict or restart is recognised by its claim and not mistaken for a stranger. Any other
VM under the target name fails the migration:

```text
VMMigration  Failed  reason: TargetVMNotOwned
```

- The check runs first at `Validating`, **before the source is powered off, snapshotted
  or exported**. The foreign VM is never read into the migration's status, annotated or
  deleted; deleting the migration deletes only the VM bound by UID.
- A VM is declared "not ours" only after a live re-read of both objects, so a lagging
  cache never fails a migration over its own VM.
- Reserved labels in `spec.target.labels` (`virtrigaud.io/adopted`) are no longer copied
  to the target VM (Normal event `TargetLabelsDropped`).
- **A migration in `Creating` or `Validating-Target` during the upgrade fails closed with
  `TargetVMNotOwned`.** Its target was created without the binding, and there is
  deliberately no fallback to the forgeable fields. Let such migrations finish before
  upgrading, or do not start new ones. Afterwards, for one that failed: check its target
  VM, delete the source yourself if you meant to (`deleteAfterMigration` does not run for
  a failed migration), and re-create the migration. If another VM holds the target name,
  delete or rename it, or pick another `spec.target.name`.

**New ValidatingAdmissionPolicy.** The chart (and `config/admission-policy/` for
kustomize) installs a `ValidatingAdmissionPolicy` and binding that lets only the
manager's ServiceAccount create a `VirtualMachine` with, or add or change on one, the
manager-owned keys: `virtrigaud.io/migration`, `migration-claim`, `migrated-from`,
`imported-disk-id`, `disk-checksum`, `migration-completed(-at)`, `clone`, `clone-uid`,
`cloned-from`, `adopted-host`, `adopted-id`, and the label `virtrigaud.io/adopted`.
Updates that leave those keys unchanged, removing a key, and the keys users set
(`orphan-on-delete`, `force-delete`, `create-outcome`, `placement-uid`) are not affected.

```yaml
# values.yaml
admissionPolicy:
  reservedKeys:
    enabled: true            # default
    exemptUsernames: []      # e.g. ["system:serviceaccount:velero:velero"]
    exemptGroups: []         # e.g. ["platform-break-glass"]
```

- **It is a security control for migration, adoption and clone, not a convenience.**
  Single-host and clustered adoption and `VMClone` still bind from these markers, so
  without the policy anyone who can create `VirtualMachine`s in those namespaces can
  forge them. Do not set `reservedKeys.enabled: false` unless another admission control
  (Kyverno, Gatekeeper) reserves the same keys.
- **Kubernetes 1.30+ only** (`admissionregistration.k8s.io/v1`). It is not a webhook, so
  it works with webhooks disabled and while the manager is down. Below 1.30 nothing is
  rendered; see Known limitations.
- **Restore tools and a second installation are rejected unless exempted.** A restore
  that re-creates `VirtualMachine`s with their annotations (Velero) fails for every VM
  carrying a reserved key: add the tool's identity to `exemptUsernames` or
  `exemptGroups` before you restore. Another VirtRigaud installation's manager in the
  same cluster needs the same. `system:masters` is not exempt.
- **The installer needs new rights:** create/update/delete on the cluster-scoped
  `validatingadmissionpolicies` and `validatingadmissionpolicybindings`.
- Check it: `kubectl get validatingadmissionpolicy,validatingadmissionpolicybinding | grep reserved-keys`.

→ [Reserved keys and the admission policy](docs/admission-policy.md),
[The target VM must be the migration's own](docs/cross-namespace-targets.md#the-target-vm-must-be-the-migrations-own),
[Upgrade guide](docs/upgrading.md#breaking-changes)

### Single-host Proxmox: every per-VM call is owner-checked (#393)

In rc1 only `Delete` was owner-checked on a single-host Proxmox Provider. `Describe`,
`Power`, `Reconfigure`, the snapshot calls, `GetDiskInfo`, `ExportDisk` and a clone's
source acted on whatever VM held the recorded VMID, so if PVE reused a VMID a
`VirtualMachine` could read, power, resize, snapshot, export or clone **another tenant's
VM**. The manager filled `owner` only for clustered refs, so a provider-only fix would
have done nothing; both halves changed.

- **The manager sends the owner (UID, namespace, name) on every single-host per-VM
  call.** vSphere, single-host libvirt and mock ignore it on these calls; their behaviour
  is unchanged.
- **The Proxmox provider checks it before it reads or changes anything.** The VM at the
  VMID must be stamped with the requester's UID, or be unstamped and named like the owner
  (the legacy name rule, see Known limitations). A VM stamped for someone else, or
  unstamped with another name, is answered `NotFound` / `VM_NOT_OWNED`; an unreadable or
  multiple stamp is `FailedPrecondition` / `OWNER_STAMP_UNREADABLE`.
- **A refused `VirtualMachine` is held**, not re-created: `Ready=False/VMNotOwned` (or
  `OwnerStampUnreadable`), a constant message, one Warning event, `status.id` kept, a
  backoff up to 5 minutes. Deleting the `VirtualMachine` releases it and leaves the other
  VM alone.
- **A `VMMigration` or `VMClone` whose source is refused fails before any side effect**
  (nothing exported, snapshotted or cloned; with `powerOffBeforeMigration` the source's
  `spec.powerState` is no longer set to `Off` first). A refused `VMSnapshot` delete is
  released as already gone; a snapshot create fails.

**Who is held after the upgrade, and what to do.** Look for these before upgrading:

| Held VM | Resolve |
|---|---|
| An unstamped Proxmox VM renamed in PVE after VirtRigaud created it | Rename it back to the `VirtualMachine`'s name |
| A VM an earlier release adopted while it carried a previous owner's stamp (compare the `VirtualMachine` UID with `qm config <vmid> \| grep virtrigaud.owner`) | Remove the stale `virtrigaud.owner` line from the VM's Notes, or delete the `VirtualMachine` (the VM is left in place) and let adoption take it again; adoption now stamps it (next section) |
| A `VirtualMachine` whose VMID was reused by another VM | Delete the `VirtualMachine` (the other VM is left alone) and create it again |

After the upgrade, search for `Ready` reason `VMNotOwned`, for failed `VMMigration`s and
`VMClone`s whose message says the source VM was refused, and for `Refusing the call` in
the Proxmox provider log. The check needs **both** the rc2 manager and the rc2 Proxmox
provider. It was tested against the Proxmox test fake only, not a real PVE cluster.

→ [Per-VM calls](docs/vm-ownership.md#per-vm-calls-describe-power-reconfigure-snapshots-disks-and-clone-sources),
[Upgrade guide](docs/upgrading.md#breaking-changes)

### Single-host adoption stamps the adopted VM (#392)

In rc1, adoption left the adopted VM's owner stamp as it was, so a Proxmox VM detached
with `orphan-on-delete` and adopted again kept the deleted `VirtualMachine`'s stamp and
was left in place (`VM_NOT_OWNED`) when its new `VirtualMachine` was deleted. Now:

1. The adopting `VirtualMachine` is created **without** `status.id` (annotated
   `virtrigaud.io/adopted-id`).
2. The VM is stamped with it through `TransferOwner`, a compare-and-swap on the
   hypervisor (vSphere `config.changeVersion`, Proxmox config `digest`, libvirt domain
   UUID under a per-host lock). It is stamped only over no stamp, or over a stale stamp
   whose UID an uncached read of the API server finds on no `VirtualMachine` (the latter
   only with `virtrigaud.io/adopt-stamped-vms`).
3. Only then is `status.id` written. An interrupted adoption is finished, bound or
   removed by the next discovery (every 5 minutes).

- A VM already bound by `status.id` to a `VirtualMachine` of **another** single-host
  Provider object with the same type and endpoint is no longer adopted.
- **The capability `supports_owner_transfer`** (GetCapabilities field 25) says a provider
  can stamp on adoption. The rc2 vSphere, libvirt, Proxmox and mock providers report it.
  A provider without it (older, or third party) adopts as before, **unstamped**, and the
  Provider's adoption status says so. If capabilities cannot be read, nothing is adopted
  that round.
- VMs adopted by earlier releases are **not** backfilled. Clustered adoption of
  stale-stamped domains is unchanged (the ADR-0007 A6 runbook).
- New privileges: vSphere `VirtualMachine.Config.AdvancedConfig` and Proxmox
  `VM.Config.Options` on the VMs adoption takes over (the stamp is written on them).

→ [Adoption stamps the adopted VM](docs/vm-ownership.md#adoption-stamps-the-adopted-vm),
[Upgrade guide](docs/upgrading.md#breaking-changes)

### Single-host VMClone targets are stamped; a re-sent Clone re-binds (#395)

In rc1 the single-host controller created the target `VirtualMachine` after the clone, so
the clone was never stamped and a `Clone` sent again after a manager restart or a lost
record made a **second clone on Proxmox**, or failed on vSphere and libvirt and left the
first clone untracked. Now:

- The target `VirtualMachine` is created first, unbound. Its UID is recorded in
  `status.targetUID` and sent as `CloneRequest.target_vm`; every provider stamps the clone
  with it (vSphere in the same `CloneVM_Task`). A re-sent `Clone` returns the clone
  stamped with that UID; a same-named VM the target does not own is refused with
  `AlreadyExists` (a first attempt fails with `Ready=False/ProviderConflict`).
- Before a `Clone`, the `VMClone` records `CloneRequested=True` with a resourceVersion
  precondition; if that write fails, nothing is sent.
- **A `Clone` whose outcome is unknown no longer fails the `VMClone`.** It waits
  (`Pending`, `Ready=False/CloneOutcomeUnknown`, constant message, one Warning event) and
  is sent again with a backoff of 15 s doubling to 5 min. A Proxmox `CREATE_NOT_PERFORMED`
  and untyped vSphere clone errors are held and retried the same way. A target deleted
  after a `Clone` was requested fails as `TargetConflict`; it is never re-sent for a new
  target.
- **The capability `supports_owner_rebind`** (GetCapabilities field 26) gates re-sends of
  **both `Create` and `Clone`**. Only a provider that reports it (the rc2 vSphere,
  libvirt, Proxmox and mock providers) is ever sent either again. To any other, an
  unknown outcome is never re-sent: a `Create` is held (`CreateOutcomeUnknown`), a
  `Clone` fails as in rc1. This replaces the rc1 rule that chose by provider type, under
  which an older Proxmox image, which does not stamp, could make a second VMID.
- A failed clone removes the unbound target it created, unless the provider reported a
  cloned VM or a `Clone` of unknown outcome is on record; then the target is kept for an
  administrator.
- To abandon a `VMClone` held in `CloneOutcomeUnknown`: remove or adopt any VM stamped
  with its `status.targetUID`, then delete the `VMClone` **and** its target
  `VirtualMachine` (deleting the `VMClone` never touches the target).
- Clones made before rc2 are unstamped and are not re-bound.
- Proxmox re-bind is tested against the test fake only.

→ [Clones: re-bind](docs/vm-ownership.md#clones-re-bind),
[Upgrade guide](docs/upgrading.md#breaking-changes)

### Reconfigure resizes the right disk, and vSphere says when it needs a power-off (#401, breaking)

Two problems in the same call. In rc1 and v0.3.11, vSphere `Reconfigure` grew the
**first** disk (the root disk) to `spec.disks[0].sizeGiB`, and Proxmox sent every
`spec.disks` size to `scsi0` (the root disk) and, after a disk grow, returned before it
sent the CPU/memory change while reporting success. And on vSphere, a CPU or memory
change of a running VM without hot-add was sent to vCenter, failed, and was retried
forever as a plain `ProviderError` (rc1 lab finding V1).

Now:

- **Neither provider's `Reconfigure` touches the root disk.** The VMClass
  `spec.diskDefaults.size` is not applied by `Reconfigure`, as before.
- **vSphere** grows a `spec.disks` entry's own additional disk only: the disk recorded
  with the entry's name when `Create` attached it. A smaller `spec.disks` size is
  refused (`InvalidSpec`, nothing changed, the CPU/memory change in the same request
  included). A delta disk (snapshots, linked clone), a physical-mode RDM and an
  ambiguous name are left alone and logged.
- **Proxmox** changes no disk from `spec.disks` (its `Create` attaches none), and always
  sends the CPU/memory change. Nothing replaces the old root-disk grow in this release:
  grow the disk in PVE.
- **vSphere checks first whether vCenter can apply the change now.** For a VM that is not
  powered off it reads the hot-add settings and the disk's controller; if the change
  cannot be applied (no CPU or memory hot-add, fewer vCPUs without hot-remove, less
  memory, memory above the hot-plug limit, a disk on a shared bus or with multi-writer
  sharing, any change of a suspended VM) it refuses the whole request before changing
  anything:

```text
VirtualMachine  Reconfiguring=False  reason: ResizePendingPowerOff
```

  The VM stays `Ready`, `status.currentResources` is unchanged, and the request is sent
  again on the per-VM backoff and at once after a spec change. To apply it, set
  `spec.powerState: Off`, then `On`. These refusals and the faults vCenter or PVE
  returns for a task they refuse (`VM_OPERATION_FAILED`) no longer count toward the
  Provider's circuit breaker.

**Who is affected.** Anyone who grew a Proxmox root disk through a `spec.disks` entry
(that stops working); any vSphere VM whose `spec.disks` size is below its disk's (its
`Reconfigure`, CPU and memory included, now fails until you raise it back); alerting on
`Reconfiguring` (new reason). The additional disks of vSphere VMs created by rc1 or
earlier carry no recorded name and are never resized by `Reconfigure`, as before. The
Proxmox behaviour was tested against the PVE test fake only.

→ [Which disk a size goes to](docs/reconfigure-results.md#which-disk-a-size-goes-to),
[vSphere: changes a running VM cannot take](docs/reconfigure-results.md#vsphere-changes-a-running-vm-cannot-take),
[Upgrade guide](docs/upgrading.md#breaking-changes)

### A provider call whose outcome is unknown is held, not re-sent blindly (#402, #405)

In rc1, a manager restart or leader change between a `SnapshotCreate`, `ExportDisk` or
`ImportDisk` and the write of its result sent the call again. On vSphere and the mock
that took a **second snapshot of the live source VM**; a call that timed out failed the
attempt and the retry policy sent it again while a copy on the hypervisor host could
still be writing. Now:

- **A durable marker is written first.** A `VMMigration` records `SnapshotRequested`,
  `ExportRequested` or `ImportRequested` before the call, a `VMSnapshot` records
  `SnapshotRequested` with its binding, each with a `resourceVersion` precondition: if
  the write fails, nothing is sent. The result, or a definite failure (nothing done, or
  a refusal), removes it.
- **A failure without a definite answer no longer fails the attempt.** A deadline, a
  connection dropped after the request was written, or an unclassified provider error
  keeps the marker. The object is held in its phase:

```text
Ready=False  reason: SnapshotOutcomeUnknown | ExportOutcomeUnknown | ImportOutcomeUnknown
```

  with a constant message and one Warning event naming the attempt.
- **The call is re-sent only where that cannot duplicate anything.** A clustered
  `SnapshotCreate` (adopted by request token once no job runs on the domain), and an
  `ExportDisk` / `ImportDisk` on a staging backend the provider lists in the new
  capabilities `resend_safe_export_backends` / `resend_safe_import_backends` (fields 27
  and 28), 35 minutes after the marker, at most 3 times (10 for the routed snapshot).
  Everything else is held, including every single-host `SnapshotCreate` and every call
  through a provider image older than rc2.
- **Releasing a held object is an operator action.** Check what the call may have
  made (the snapshot named in the event, the target's `-migrated` disk), remove it, then
  confirm the attempt:

```sh
ATTEMPT=$(kubectl get vmmigration <name> -n <ns> \
  -o jsonpath='{.status.conditions[?(@.type=="ExportRequested")].lastTransitionTime}')
kubectl annotate vmmigration <name> -n <ns> virtrigaud.io/operation-outcome=not-performed@$ATTEMPT
```

  Or delete the object. A deleting migration with an unknown export waits 35 minutes
  before it deletes the staged object.
- **Single-host libvirt answers the failures it can prove (#405).** It used to answer
  almost every failure of these three calls as an untyped `Unknown`, which the new
  rule would hold: a `qemu-img` error, a missing source domain, a taken snapshot name. It
  now answers those as "not performed" or a typed refusal (so the attempt fails), and
  keeps `Unknown` for what it cannot prove (a killed command, an upload that may have
  completed, a failure after a disk landed). A libvirt image older than rc2 holds all of
  them. A host that cannot be reached during these calls no longer counts toward the
  circuit breaker.
- **Proxmox:** an s3 stage is published only when exactly the object's size arrived (a
  cancelled call used to publish a truncated stage).

Alert on migrations or snapshots stuck in a phase, and on the three `...OutcomeUnknown`
reasons.

→ [Provider calls whose outcome was lost](docs/provider-call-outcomes.md),
[Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

### A failed or deleted VMMigration deletes the disk its import landed (#403)

Until rc1 the disk `ImportDisk` landed on the target (vSphere
`[<datastore>] <namespace>_<vm>-migrated/`, libvirt
`<default pool>/<namespace>.<vm>-migrated.qcow2`, the Proxmox import stage) stayed there
for good when the migration failed, or was deleted, before a target VM attached it. Every
vSphere import also left about 1.4 KB of import-VM files (`.vmx`, `.vmsd`, ...) in the
landing folder. Now:

- Before the import the migration records `LandedDiskCleanup=Unknown/LandedDiskMayExist`
  and the import target (VM and Provider UID). The manager then asks the target provider
  to delete the landed disk (new RPC `DeleteLandedDisk`, capability
  `supports_landed_disk_delete`), following the staged disk's `cleanupPolicy`:
  `OnSuccess` (default) when the `VMMigration` is deleted, `Always` also when it fails for
  good, `Never` keeps it (`LandedDiskRetained`).
- **Fail closed.** A successful migration's disk, a disk its target `VirtualMachine`
  has, a disk any VM uses (`LandedDiskInUse`), and a disk another in-flight migration
  lands under the same name (`LandedDiskHandedOver`) are kept. A delete that fails is
  retried and holds a deleting `VMMigration` (`LandedDiskCleanupPending`;
  `virtrigaud.io/force-delete=true` releases it). An older provider image, a gone
  Provider, or a refused request records `LandedDiskLeftBehind` with one Warning.
- The vSphere import removes its own transient import-VM files, never the landed `.vmdk`.
- **Disks left by earlier releases are not touched.** Find and delete them as described
  in [Leftovers from earlier releases](docs/upgrading.md#leftovers-from-earlier-releases).
- The in-use checks see only the VMs of this vCenter or libvirt host. Do not share the
  landing datastore or the libvirt `default` pool directory with hypervisors the Provider
  does not manage.
- Tested against vcsim, recorded host runners and the PVE test fake, not real hypervisors.

→ [ADR-0011 Addendum A](docs/adr/0011-migration-staging-cleanup.md#addendum-a--landed-disks-rc2),
[Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

### Provider pods mount only the migration PVCs a live VMMigration references (#399)

Provider pods hold the hypervisor credentials. Before, they mounted **every** PVC in
their namespace labelled `virtrigaud.io/component=migration-storage`, read-write, so
anyone who could create a PVC there could attach arbitrary storage to them, or stall
their rollout with a PVC that never binds. This applies to the `pvc` staging backend.
Now:

- A PVC is mounted only into the source and target Provider of a `VMMigration` in that
  namespace that records it in `status.storagePVCName` and is still in flight. The label
  is necessary but not sufficient.
- The PVC must be the one the migration created, identified by the UID its own `Create`
  returned (new `status.storagePVCUID`; a forged `ownerReference` no longer counts), or
  the one `spec.storage.pvc.name` names, if no other `VMMigration` has it.
- The target Provider mounts it **read-only**; the source read-write.
- A PVC that is `Lost`, being deleted or `Pending` is not mounted (the one exception is a
  `Pending` PVC on a `WaitForFirstConsumer` StorageClass while its migration validates),
  so one that never binds cannot stall a rollout.
- **Deleting a `VMMigration` deletes only the PVC it created.** A PVC named in
  `spec.storage.pvc.name` is unmounted and left for its owner (event
  `MigrationPVCRetained`). Before, the manager deleted whatever PVC the migration had
  recorded. If you relied on that, delete your PVC yourself.
- **Extra pod rolls.** A provider pod is now also rolled when a migration becomes `Ready`
  or fails for good, and once on the upgrade, which interrupts an export or import
  running at that moment. Upgrade between migrations if you can.
- The manager's ClusterRole gains `get` on `storageclasses`.

→ [PVC staging backend: what the provider pods mount](examples/migration/README.md#pvc-staging-backend-legacy-what-the-provider-pods-mount),
[Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

### Staged migration files never follow links (#404)

On the `pvc` staging backend the staging PVC is writable by the tenant and mounted into
the credentialed provider pods. The vSphere and libvirt providers opened the staged
file by joining a path and following links, so a symbolic link a tenant planted in the PVC
(as the file or any parent directory) made `ImportDisk` read the pod's credentials, TLS
key or ServiceAccount token into the tenant's disk, and made `ExportDisk` write wherever
the pod can write. Now every staged-file path is resolved beneath the PVC mount with no
symbolic link in any component (`openat2` with `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS`,
falling back to a per-component `O_NOFOLLOW` walk). Only a regular file with a link count
of 1 is read; FIFOs, devices, directories and hard-linked files are refused. The libvirt
pod-side `ImportDisk` accepts only `pvc://` sources (the `file://` scheme, which the
manager never sent, is removed).

- A `pvc` migration whose PVC holds a symbolic link in its `vmmigrations/...` tree, or a
  hard link to the staged disk, now fails with `refused the staged file` /
  `refused the staged disk`. Replace the link with a real directory.
- **A staging PVC on a filesystem that reports a link count other than 1 for a regular
  file is refused the same way** (some CSI and FUSE filesystems). Use one that reports
  link counts, or the `s3` or `nfs` backend.
- `s3` and `nfs` migrations are unaffected. The Proxmox provider rejects the `pvc`
  backend before any file access. Linux only; other platforms fail closed.

→ [Staged-file hardening](examples/migration/README.md#staged-file-hardening-pvc-backend),
[Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

### VirtualMachine status.phase is set on every VM (#398)

`kubectl get vm` showed an empty `PHASE` on most VMs, because only a few code paths wrote
the field. The manager now derives it on every status write from the VM's state:
`Pending`, `Provisioning`, `Running`, `Stopped`, `Reconfiguring`, `Deleting` or `Failed`.
A `Ready=False` hold such as `VMNotOwned`, `ProviderUnavailable` or
`CreateOutcomeUnknown` is `Failed`; a `Suspended` VM is `Running`; an `Unknown` power
state is `Failed`; a delete that is held is `Deleting`. The first reconcile after the
manager rolls fills in the phase of every existing VM (one status write each, then none
unless the state changes). No CRD schema change. Automation that read `status.phase`
must expect every value.

→ [`docs/vm-status-phase.md`](docs/vm-status-phase.md)

### Smaller fixes (#398, #400)

- **vSphere: one finder per call.** The provider shared one `*find.Finder` between
  concurrent RPCs and re-scoped it on about twenty paths; a govmomi `Finder` is not safe
  for concurrent use, so RPCs raced on it (`go test -race` reports it). Each RPC now has
  its own finder, scoped to the default datacenter; the datacenter reference is cached
  for 60 seconds per vCenter client. No setting changes.
- **libvirt: no fixed `/tmp` files on the host.** Pool definition wrote `/tmp/default-pool.xml`
  and the persistent-XML sync `/tmp/<domain>-sync.xml` (left behind by a failed define)
  on the hypervisor; two concurrent calls overwrote each other's file. The XML now goes
  to `virsh` over stdin.
- **libvirt keepalive self-heals.** After a failed probe the keepalive drops the client,
  so a client that reports connected on a dead transport is replaced on the next tick.
- **go-libvirt upstream race.** `(*Libvirt).ConnectToURI` in the pinned
  `github.com/digitalocean/go-libvirt` writes a field after starting a goroutine that reads
  it; it is unfixed upstream. The one test that triggers it is skipped under `-race`
  (`TestGolibvirtHolder_ConcurrentAccess_Race`); it still runs without `-race`. The
  production consequence found is bounded by the self-heal above.

### Proxmox legacy name rule: decision and what to do in v0.4.0 (#397)

The single-host Proxmox owner check still accepts an **unstamped** VM named like its
`VirtualMachine`, so a PVE user with `VM.Allocate` can plant a look-alike at a freed
VMID. [ADR-0012](docs/adr/0012-proxmox-legacy-unstamped-name-rule.md) (Accepted) decides
to retire the rule: the manager records each VM whose stamp the provider confirmed and
then requires it (`require_owner_stamp`), legacy VMs are stamped once through
`TransferOwner`, and the rule is dropped by default in v0.5.0. **Phase 1 is planned for
v0.4.1, gated on validation against a real PVE cluster; it is not in v0.4.0.**

For v0.4.0:

- **Do not grant `VM.Allocate` on the pools and paths where VirtRigaud's VMs live** to
  any principal other than VirtRigaud's own Provider tokens and your administrators.
  That includes inherited grants (on `/vms` or `/`) and shared pools.
- **Accepted risk: a stamp can be copied.** The stamp's checksum detects edits, not
  forgery. Someone who knows a `VirtualMachine`'s UID (readable through the Kubernetes
  API) and can set a VM's description (`VM.Config.Options`) can write that stamp on a VM
  they plant. A binding to a PVE creation attribute would close it and is scheduled as a
  spike.

→ [`docs/vm-ownership.md`](docs/vm-ownership.md#limits)

### Upgrade order for rc2

Same as the rc1 order, with the admission policy added:
**CRDs, then the admission policy and any cross-namespace grants, then the manager, then
the providers promptly.** The policy is installed by `helm upgrade` or `config/default`;
the installing identity needs the new cluster-scoped rights above. Exempt your restore
tool before the next restore.

Roll the providers **right after the manager**, especially a v0.3.x Proxmox provider.
Until it is upgraded, the single-host Proxmox owner checks do not apply (an older
provider ignores the owner), and it does not report `supports_owner_rebind`, so a
`Create` or `Clone` of unknown outcome through it is held or fails rather than re-sent.
Rolling a provider back stops adoption stamping; rolling the manager back can leave an
adoption annotated `virtrigaud.io/adopted-id` without `status.id`: delete that
`VirtualMachine` (no provider call is made) and let adoption take the VM again.

Wave 2 adds to this:

- **CRDs first, as always.** Two optional status fields are new: `status.storagePVCUID`
  on `VMMigration` and `status.storageInfo.importTarget.providerUID`. With the old CRD the
  API server prunes them, so the manager cannot record them (an import target recorded
  without the Provider's UID is never sent a landed-disk delete).
- **Roll all providers with the manager, not only Proxmox.** Three capability fields are
  new in `GetCapabilities`: `resend_safe_export_backends` and `resend_safe_import_backends`
  (27, 28) and `supports_landed_disk_delete` (29). A provider that predates rc2 reports
  none of them, and the manager then takes the conservative path: a lost or timed-out
  export or import is **held** for an operator instead of re-sent, a landed disk is
  recorded as `LandedDiskLeftBehind` instead of deleted, and every failure of a
  single-host libvirt `SnapshotCreate`, `ExportDisk` or `ImportDisk` is held (the old image
  answers them all untyped). An older vSphere image shows a change a running VM cannot
  take as `ProviderError` again, and an older manager shows the new
  `ResizePendingPowerOff` refusal as `ProviderError`.
- **The manager upgrade rolls each provider pod that mounts migration PVCs once** (#399:
  unreferenced PVCs are dropped and target mounts become read-only), which interrupts an
  export or import running at that moment. Upgrade between migrations; also let
  migrations in `Creating` or `Validating-Target` finish (#394).
- **Rolling a vSphere or Proxmox provider back brings back the wrong-disk resize**
  (the root disk grown to `spec.disks[0].sizeGiB`, or every `spec.disks` size sent to
  `scsi0`). Hold CPU and memory changes of VMs with `spec.disks` while an older image
  runs. The disk-name records vSphere writes on VMs created by rc2 are ignored by the old
  image and used again after you roll forward.

## About this release candidate / what to test

<!-- rc3 or GA - maintainer's call: if this release is GA, drop this section's candidate wording. -->
v0.4.0 GA follows after a soak of the release candidates (rc1 and rc2 were the first). Please try it on a non-production
cluster and report what you find (GitHub issues; include the Provider type, the
`kubectl describe` of the object involved and the provider log, never a Secret).
The most useful things to exercise:

- **Upgrading from v0.3.11**, following [docs/upgrading.md](docs/upgrading.md): the
  CRDs, then the manager, then the providers, with the grants and vCenter
  privileges it lists. Tell us where the guide was wrong or unclear.
- **Clustered libvirt** (`topology: cluster`, experimental): placement, deletes
  while a host is unreachable, restore and re-attach.
- **Migrations over NFS and S3**, in any direction you use, including the new
  staging cleanup (`StagingCleanup` condition, `DeleteStagedDisk`) and the
  import-time checks on the staged image.
- **The administrator disk-size limit** (`manager.maxDiskSize`,
  `infra.virtrigaud.io/max-disk-size`), including clones and migrations.
- **Deleting a `Provider`** that still has VMs (the in-use finalizer, ADR-0010),
  and VM deletes while a provider is rolling out.
- **New in rc2:** a `VMMigration` into a namespace that already has a same-named
  `VirtualMachine` (it must fail `TargetVMNotOwned` before touching the source);
  the admission policy on Kubernetes 1.30+ (your restore tool and any second
  VirtRigaud installation exempted); single-host adoption of a VM that carries a
  stale stamp; a `VMClone` through a manager restart; and, on Proxmox, a reused
  VMID under a managed `VirtualMachine`.
- **New since rc2:**
  - on every libvirt host, the seed directory before the provider rolls; then a create
    and a clone through each Provider (no `refusing to stage cloud-init data`), and that a
    local non-root user cannot list or read the seed directory;
  - a clone of a cloud-init VM: new SSH host keys and the target's hostname in the guest,
    and `/var/lib/cloud/data/instance-id` equal to `virtrigaud-<uid>` (libvirt and
    vSphere; the real-guest check is still open, #413);
  - deleting a VM and its clone at the same time on single-host libvirt: no seed
    directory left;
  - **a migration into a clustered libvirt Provider** over `s3` and `nfs`, including the
    holds you can provoke (a host made unreachable after placement, a landing host with
    a same-named domain, a pool without room), a retry after a failure, and deleting the
    migration while it waits for placement;
  - a vSphere S3/NFS import: only the `.vmdk` is left in the landing folder.
- **Also new in rc2 (Wave 2):**
  - a CPU or memory change of a running vSphere VM that has no hot-add (expect
    `Reconfiguring=False/ResizePendingPowerOff`, then power it off and on), and a
    `spec.disks` size on a vSphere VM created by rc2 (it must grow only its own
    additional disk);
  - killing the manager (or the provider pod) during a migration's snapshot, export or
    import: the migration should hold with `...OutcomeUnknown` or re-send only on a
    backend the provider lists as re-send safe, and release with the
    `virtrigaud.io/operation-outcome` annotation;
  - a migration that fails or is deleted after its import: the landed disk should be
    gone (`LandedDiskCleanup`), or recorded as left behind;
  - a `pvc` migration: provider pods should mount only its PVC (read-only on the
    target) and drop it when the migration finishes; a PVC named in
    `spec.storage.pvc.name` should survive the migration's deletion;
  - `kubectl get vm`: `PHASE` filled in on every VM after the manager rolls.

**Proxmox was not validated against a real PVE cluster.** The Proxmox changes in
this release (owner stamps, per-VM owner checks, adoption stamping, clone
re-binding, imported-disk confinement, NFS/S3 import hardening, idempotent power,
and in Wave 2 the `Reconfigure` change that no longer touches `scsi0`, the s3 stage
publish check and the import-stage delete of a failed migration) were tested only
against the test fake, not a real PVE cluster. If you run Proxmox, treat it as
untested and try these paths on a lab cluster first. ADR-0012's Proxmox name-rule
change (planned for v0.4.1) needs a real PVE cluster as its release gate.

**Validated in the lab on real vSphere and libvirt** (vCenter, single-host and
clustered libvirt 10.0, Kubernetes 1.34), on the rc2 candidate build:

- Wave 1: the admission policy (enforced even for `system:masters`; the manager and
  user keys allowed), a planted target VM failing a migration at `Validating` before
  any side effect, adoption re-stamping a detached VM so that deleting it works, and
  single-host clone stamping.
- Wave 2: the `PHASE` column; libvirt definition sync via `virsh define /dev/stdin`
  with nothing left in the host's `/tmp`; a vSphere CPU change on a running VM
  without hot-add held as `ResizePendingPowerOff` (still `Ready`) and applied after
  power-off, with the root disk left alone; libvirt → S3 → vSphere and a pvc-backend
  vSphere → libvirt migration end to end, with the request markers cleared, the
  source provider mounting the PVC read-write and the target read-only only while
  the migration ran, the migration-created PVC deleted with the migration, the
  landed disk kept while its target VM uses it, and the import VM's `.vmx`/`.vmsd`
  removed from the landing folder.

Paths that need a fault to trigger (held provider calls after a manager restart,
landed-disk deletion after a failed import, staged-file link refusal) are covered
by unit, envtest and simulator tests only.

**Validated in the lab since rc2** (build `main-73e08e8` for the clustered migration;
libvirt 10.0, Ubuntu 24.04, AppArmor for the seed directory):

- The seed directory (#414): the `dynamic_ownership` hand-over of the `0600` ISO to
  the qemu user, no AppArmor change needed, the seed unreadable by another local user,
  the refusal of an unusable directory, and the delete of a legacy `/tmp` seed. An
  SELinux host was not tried.
- Migration into a clustered libvirt Provider (#415, #416), a two-host Provider on a
  shared NFS pool: a single-host libvirt VM into the cluster over `s3` and over `nfs`,
  both to `Ready`; a failure after the import, whose landed disk was deleted through the
  recorded host; a host made unreachable after the target was placed (the migration
  waited, did not re-place, and recovered when the host returned).
- **Not run in the lab:** a vSphere or Proxmox source into a clustered target, a host
  made unreachable after the disk landed, and a manager restart in the middle of a
  clustered import. **Not covered by the lab results recorded for this release:** a
  clone's new identity in a real guest and the vSphere `guestinfo` rewrite on a real
  vCenter. All of these are covered by unit, envtest and simulator tests.

## Breaking changes

- **A `Provider`, `VMClass` or `VMImage` in another namespace can only be used if its new `spec.consumerNamespaceSelector` selects the referencing namespace** (unset = own namespace only, `{}` = all namespaces). Set it on shared objects **after applying the new CRDs and before rolling the manager** (the field does not exist until the CRDs are upgraded); otherwise VMs using them fail closed with `ConsumerNotAllowed`. The manager's readiness now also checks that the Provider, VMClass and VMImage CRDs have the field. → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **`VirtualMachine.spec.providerRef` is now immutable once a VM is bound.** The
  old "re-point to a missing Provider, then delete" un-adopt workaround no longer
  works — use the new `virtrigaud.io/orphan-on-delete: "true"` annotation instead.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **Cross-namespace `VMClone`/`VMMigration` targets now require a grant** on the
  target namespace (`infra.virtrigaud.io/allowed-source-namespaces`). Objects in
  flight during the upgrade need the annotation applied first.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **vSphere `Create`/`Clone` fail closed on VM ownership.** Requires the new
  vCenter privilege `VirtualMachine.Config.AdvancedConfig`; `vm-<digits>` names
  are refused; `VMImage.templateName` must name a real vSphere template.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **libvirt VMs created without `spec.userData` no longer get a default `ubuntu`
  user, SSH key, or passwordless sudo.** Anyone relying on that default login
  must supply their own `spec.userData`. Existing VMs still carry the old key
  in-guest until removed by hand.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **libvirt base images are now confined to allowed directories and always
  copied**, never attached in place. Set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` if
  images live outside `/var/lib/libvirt/images`. VMs from an earlier release may
  share one disk file — check `virsh domblklist --details` before deleting a
  pre-upgrade VM.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)

- **vSphere attaches an imported disk in place only when it is the VM's own
  verified migration disk, and migrations land per namespace.** A migration now
  lands at `[<datastore>] <namespace>_<vm name>-migrated/...`, so a same-named VM
  in another namespace can no longer reach, overwrite or delete it.
  `spec.importedDisk.path` naming any other datastore file (another VM's disk) is
  refused before anything is created; a landing disk another VM uses is refused
  too. Deleting a VM no longer deletes a disk it does not own. **A vSphere
  migration landed before the upgrade whose target VM was not yet created must be
  run again.** The vCenter account needs `System.Read` on the root folder,
  propagated.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)

- **Security (affects v0.3.11): Proxmox imports a migrated disk only from the
  VM's own verified migration stage.** With the migration SSH data plane
  configured, a Proxmox `Create` ran `qm importdisk` on any node path in
  `spec.importedDisk.path` and then deleted it, so a tenant could copy any file
  the SSH user can read on the PVE node (another VM's disk, `/etc/pve/priv/*`)
  into their own VM. Now the path must be verified by the manager as this VM's
  own migration disk and be exactly
  `/var/lib/virtrigaud/import-stage/<namespace>_<vm name>-migrated.<format>` (stages
  are now named per namespace); anything else is refused before anything is
  created. **Upgrade the manager before the Proxmox provider.** A Proxmox-target
  migration staged before the upgrade whose target VM was not yet created must
  be run again. A migration into another namespace through a Proxmox target
  fails at `Validating` (`ProxmoxCrossNamespaceTarget`) before any export.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)

- **libvirt linked clones are disabled.** A `VMClone` with
  `spec.options.type: LinkedClone` through a libvirt Provider fails with
  `LinkedCloneUnsupported`: a linked clone reads its source VM's live disk, and
  powering the source on corrupts the clone. Use `FullClone` (the default).
  Existing linked clones keep working, but their source cannot be deleted,
  reverted or snapshotted while the clones exist — keep it powered off.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **libvirt `Reconfigure` no longer reports success for changes it did not
  apply.** A change is applied live and persisted, persisted and reported as
  restart required (`Reconfiguring=True/RestartRequired`: power-cycle the VM),
  or it fails (`Reconfiguring=False/ProviderError`). A memory shrink of a
  running VM always needs a restart. `status.powerState` can now be
  `Suspended` or `Unknown`; such a VM is not resized or powered on (a
  suspended one is powered off if its spec says `Off`). On clustered
  Providers, resizes are held until the libvirt provider reports
  `supportsHonestReconfigure` — roll the providers promptly after the
  manager.
  → [Upgrade guide](docs/upgrading.md#breaking-changes),
  [`docs/reconfigure-results.md`](docs/reconfigure-results.md)
- **`powerOn` is honored: a clone or migrated VM stays powered off unless
  `powerOn: true`.** `VMClone` `spec.options.powerOn` and `VMMigration`
  `spec.target.powerOn` (default `false`) were never read, so every clone and
  migrated VM came up running. The produced VirtualMachine's `spec.powerState`
  is now `On` only for `powerOn: true`. A target with `powerOn: false` is never
  booted, on any provider.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)

- **Deleting a `Provider` that is still in use now waits instead of orphaning
  its VMs** ([ADR-0010](docs/adr/0010-provider-in-use-finalizer.md)). A new
  finalizer, `provider.infra.virtrigaud.io/in-use`, keeps a `Provider` being
  deleted — and its runtime — while any `VirtualMachine` in any namespace (or a
  `VMSnapshot`, in-flight `VMClone` or `VMMigration`) still needs it, so they are
  deleted through it; before, every one of them was released without a provider
  `Delete` and its hypervisor VM kept running, unmanaged. It shows
  `DeleteBlocked=True/ProviderInUse` with counts per kind and namespace.
  Deleting a namespace still deletes the provider runtime's pods with it, so a
  VM whose delete misses the pod's termination window is held, and the
  namespace stays `Terminating` until an administrator acts. Delete VMs before
  their `Provider` or namespace; `virtrigaud.io/force-delete: "true"` on the
  `Provider` releases it at once (`Warning` event `ProviderForceDeleted`).
  → [Upgrade guide](docs/upgrading.md#breaking-changes),
  [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#deleting-a-provider)

- **New in rc2: a `VMMigration` acts only on the target `VirtualMachine` it
  created** (UID binding plus a claim, `status.targetVMUID` / `targetVMClaim`). A
  same-named `VirtualMachine` that the migration did not create fails it with
  `TargetVMNotOwned` at `Validating`, before the source is powered off or exported;
  before, the migration took it as its own target. **A migration in `Creating` or
  `Validating-Target` while you upgrade fails closed with `TargetVMNotOwned`** (its
  target VM is left as it is); let migrations finish first.
  → [Changes since v0.4.0-rc1](#a-vmmigration-accepts-only-the-target-vm-it-created-394-breaking),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **New in rc2: only the manager may set the manager-owned `virtrigaud.io/*`
  annotations and the `virtrigaud.io/adopted` label on a `VirtualMachine`**, enforced
  by a new `ValidatingAdmissionPolicy` (Kubernetes 1.30+; chart value
  `admissionPolicy.reservedKeys`, default on). Backup and restore tools (Velero)
  and a second VirtRigaud installation in the cluster are rejected unless listed in
  `exemptUsernames` / `exemptGroups`. Installing it needs create rights on
  `validatingadmissionpolicies` and `validatingadmissionpolicybindings`. It is a
  security control for migration, adoption and clone: do not disable it without a
  replacement. → [`docs/admission-policy.md`](docs/admission-policy.md),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **New in rc2: vSphere and Proxmox `Reconfigure` no longer resize the root disk.**
  Proxmox: a `spec.disks` size no longer grows `scsi0` and nothing replaces it in this
  release (grow the disk in PVE). vSphere: a `spec.disks` entry grows only the
  additional disk recorded under its name (VMs created by rc2 or later), and a size
  below the disk's now fails the `Reconfigure`, CPU and memory included. A change a
  running vSphere VM cannot take (no hot-add, a memory decrease, and others) shows
  `Reconfiguring=False/ResizePendingPowerOff` until the VM is powered off.
  → [Changes since v0.4.0-rc1](#reconfigure-resizes-the-right-disk-and-vsphere-says-when-it-needs-a-power-off-401-breaking),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **New in rc2: a `SnapshotCreate`, `ExportDisk` or `ImportDisk` whose outcome is unknown
  holds the `VMMigration` or `VMSnapshot` for an operator** (`Ready=False`,
  `SnapshotOutcomeUnknown`, `ExportOutcomeUnknown` or `ImportOutcomeUnknown`) instead of
  failing the attempt and re-sending it. It is re-sent on its own only on a backend the
  provider lists as re-send safe, 35 minutes later; otherwise you release it with the
  annotation `virtrigaud.io/operation-outcome=not-performed@<attempt id>` after checking
  the provider. Alerting and automation that expected a timed-out call to fail must
  include these reasons. Roll the providers with the manager.
  → [Changes since v0.4.0-rc1](#a-provider-call-whose-outcome-is-unknown-is-held-not-re-sent-blindly-402-405),
  [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md)
- **New in rc2: provider pods mount only the migration PVCs a live `VMMigration`
  references, and deleting a `VMMigration` deletes only the PVC it created.** If you
  relied on a migration's deletion to remove the PVC named in `spec.storage.pvc.name`, or
  on labelled PVCs being mounted without a migration, change that. The upgrade rolls
  provider pods once.
  → [Changes since v0.4.0-rc1](#provider-pods-mount-only-the-migration-pvcs-a-live-vmmigration-references-399),
  [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)
- **New in rc2: a `pvc` migration fails if the staging PVC holds a symbolic link, a hard
  link or a special file at a staged path, or sits on a filesystem that reports a link
  count other than 1**; the libvirt pod-side import no longer accepts `file://` sources.
  → [Changes since v0.4.0-rc1](#staged-migration-files-never-follow-links-404)

- **New since rc2: libvirt cloud-init seeds are built in a dedicated directory that must
  exist on every libvirt host** (#414). Create `/var/lib/libvirt/virtrigaud-seeds` (owned
  by the provider's SSH user, mode `0711`) **before rolling the libvirt providers**;
  otherwise every create and clone through that host fails, retryably, with
  `refusing to stage cloud-init data`. Seeds of existing VMs stay in `/tmp` until you
  make them private. → [Changes since v0.4.0-rc2](#libvirt-cloud-init-seeds-are-private-and-live-in-a-dedicated-directory-414-breaking),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **New since rc2: a clone boots as a new cloud-init instance** (#413): its SSH host keys
  are regenerated and its user-data's per-instance modules run again (libvirt and
  vSphere), so its host keys differ from its source's from the first boot. `create` on
  `VMClone` is read access to the source's data; review who has it.
  → [Changes since v0.4.0-rc2](#clones-get-their-own-seed-and-a-new-cloud-init-identity-413)
- **New since rc2: a libvirt Host's `allocatableStorageBytes` is its `default` pool's
  free space only** (#416; it summed every active pool). A clustered Provider's
  `VMPlacementPolicy` `minDiskSpacePerHost` compares against it, so such a policy can
  now reject hosts it used to accept. → [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

See the full breaking-change table, required upgrade order (CRDs → admission
policy and grants → manager → providers), and rollback caveats in
**[docs/upgrading.md](docs/upgrading.md)**.

## Highlights

### Security

- Every pod's writable `/tmp` emptyDir now has a `sizeLimit`; a pod that writes
  past it is evicted. The limit bounds the pod's `/tmp`, but does not reserve
  node storage: a node with less free ephemeral storage than the limit can still
  fill up first, so size it to your nodes. Defaults:
  - vSphere providers: `512Gi`. vSphere stages OVA downloads and about twice a
    migrated disk there.
  - libvirt, Proxmox and the other types: `1Gi`.
  - the manager: `128Mi`.

  Change one Provider's limit with the annotation
  `infra.virtrigaud.io/tmp-size-limit`, or all of them with
  `manager.providerTmpSizeLimit`. A cluster administrator can cap what the
  annotation may ask for with `manager.providerTmpSizeLimitMax`. The manager
  upgrade rolls every provider pod once.
  → [Upgrade guide](docs/upgrading.md),
  [chart README](charts/virtrigaud/README.md#tmp-size-limits)
- **Opt-in administrator disk-size limit.** By default a tenant can ask for
  disks up to the CRD bound of 64Ti each and fill a shared datastore or pool.
  Set `manager.maxDiskSize` (the manager's `--max-disk-size`, e.g. `500Gi`) to
  cap every disk VirtRigaud creates or grows: VMs that would create a larger
  disk are held (`DiskSizeExceedsLimit`), a Reconfigure never grows a disk
  beyond it, and clones and migrations are checked by the real sizes of the
  source's disks as its provider reports them (an unknown size refuses them);
  the target provider also enforces the limit on the disk it is about to land
  at the import, so a replaced staged object cannot get past it.
  VMs that already have larger disks keep running and are never shrunk. An
  image or template larger than the class size is not covered. A Provider's
  owner can lower the limit for that Provider with the annotation
  `infra.virtrigaud.io/max-disk-size` (never raise it). With webhooks enabled,
  VirtualMachine and VMClass writes above the limit are also refused at
  admission. Nothing changes unless it is set; roll the provider images with
  the manager (`GetDiskInfo` now lists every disk's size).
  → [Upgrade guide](docs/upgrading.md),
  [chart README](charts/virtrigaud/README.md#disk-size-limit)
- Editing a waiting `VMClone`'s metadata (labels, annotations), or a
  `VMSnapshot` whose delete is held, no longer re-runs it at once. A tenant
  re-annotating one in a loop drove one provider call per edit (`Clone`, a
  multi-host scan, or `SnapshotDelete`) and bypassed the wait's backoff. The
  `VMSnapshot` force-delete annotation still releases a held delete at once.
- vSphere VM adoption (`virtrigaud.io/adopt-vms`) no longer takes templates.
  It could turn a prepared image (ADR-0009) or any other template into a
  tenant's `VirtualMachine`, whose delete or resize then destroyed or changed
  the shared image for every consumer. The vSphere `ListVMs` now leaves out
  templates, objects with a `virtrigaud.image.*` key and VMs whose owner stamp
  cannot be read. It also reports each VM's owner UID, so adoption skips a VM
  that a live `VirtualMachine` owns through another Provider object. A legacy
  OVA import is marked as an image from the moment it exists. The vSphere
  `Delete`, `Power`, `Reconfigure`, `HardwareUpgrade` and snapshot calls now
  refuse a template or image artifact (`VM_IS_IMAGE_ARTIFACT`) before changing
  anything, so a `VirtualMachine` adopted from a template before the upgrade
  can no longer destroy it; nor does a linked clone snapshot one. `Create` and
  `Clone` clear every `virtrigaud.image.*` key the source carries, so a VM
  made from a template is never refused as one. Check for such `VirtualMachine`s, and for adopted
  duplicates of a VM managed under another Provider object, before upgrading
  (→ [Upgrade guide](docs/upgrading.md),
  [`docs/vm-ownership.md`](docs/vm-ownership.md#adoption-what-listvms-offers)).
- **Behaviour change:** adoption no longer adopts a VM whose owner stamp names
  no `VirtualMachine` in this cluster. That UID may belong to a live
  `VirtualMachine` of another VirtRigaud installation (or of this cluster
  before a restore) managing the same vCenter or host. Such VMs are counted in
  the Provider's adoption status. Re-adopting a VM detached with
  `orphan-on-delete` (it keeps the deleted `VirtualMachine`'s stamp) now needs
  `virtrigaud.io/adopt-stamped-vms: "true"` on the single-host Provider
  (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#owner-stamps-are-scoped-to-one-cluster)).
- Single-host libvirt adoption no longer takes a domain whose owner stamp
  cannot be read or names two owners: the single-host `ListVMs` reports
  `owner_stamp_state` as the clustered one does. Adoption also skips any VM a
  provider marks with `provider_raw["artifact"]`, a backstop for third-party
  providers that list templates.
- vSphere and libvirt `Create` no longer silently bind to (and libvirt/vSphere
  `Clone` no longer silently clones) a pre-existing VM/domain/template they
  don't own — both now stamp and check ownership, and fail closed
  (`ProviderConflict`) on anything they can't prove is theirs.
- Every libvirt command sent to the hypervisor over SSH is now shell-quoted,
  closing a command-injection path through domain names, image paths, and
  snapshot descriptions — and, as a side effect, guest-agent calls (IP
  discovery, time sync, in-guest disk grow) now actually work over SSH for the
  first time.
- libvirt domain XML generation escapes every user-derived value, closing an
  XML-injection path that could splice a sibling `<disk>` element pointing at
  host block storage into a VM's definition.
- New libvirt VMs get a per-tenant, collision-proof domain name
  (`<namespace>.<name>`) instead of the bare VirtualMachine name, closing a
  same-host cross-namespace name-squatting and disk-overwrite race.
- Cross-namespace `VMClone`/`VMMigration` targets, and a migration's source
  Provider, are now scoped to what the requester actually owns.
- `VMImage` prepare state is recorded per Provider identity
  (`status.providerStatus["<namespace>/<name>"]`, with the Provider's UID and
  its own prepare task), so a shared `VMImage` prepared through one namespace's
  Provider never lets a same-named Provider in another namespace skip its own
  prepare or create from the other's template, and a prepare task is only ever
  polled through the Provider that started it. Existing state is migrated on
  first use (→ [`docs/image-preparation.md`](docs/image-preparation.md#prepare-state-is-per-provider)).
- libvirt: **deleting a running linked clone no longer deletes its source VM's
  disk.** Delete removes only the VM's own disk files inside the storage pool
  (and its own external-snapshot chain, never a symlink's target), and a source
  VM whose disk a linked clone still uses can no longer be deleted, reverted or
  snapshotted until the clone is gone (`DeleteBlocked`); powering it on sets a
  `LinkedClonesDependOnDisk` warning. A `VMSnapshot` whose provider delete fails
  keeps its finalizer and is retried (`force-delete` to drop it), also when no
  provider call can be made for its VM. VM disks are created
  `0640 libvirt-qemu:kvm` (never `chmod`'ed as root; `chown -h`), no longer
  world-writable — the provider's SSH user needs the `kvm` group, `root`, or
  passwordless `sudo` for the exact `qemu-img info` reads (a regex rule) — disks are written in a private
  directory and renamed into place (a symbolic link at a disk's name is
  refused), the s3 export's temporary copy is private and always removed, and a
  UEFI clone's varstore is never copied through a symlink
  (→ [`docs/libvirt-clones.md`](docs/libvirt-clones.md)).
- libvirt, clustered Providers: a `VMImage` whose host path is another VM's
  live disk on **any** host of the Provider is refused (it used to be checked
  on the landing host only), and fails closed while a host cannot be checked;
  a refused image path answers the same whether it does not exist, is not
  allowed, is in use or has a header it may not have, which narrows what a
  tenant learns about the hosts' files (an accepted path still shows an image
  is there). **With a shared pool, set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` to a
  catalog directory apart from the pool**: the default image directory is
  the default pool's, and any unused, non-reserved file in it is copyable by
  any tenant of the Provider. Clustered disk writes,
  domain defines and deletes of one domain name are serialized inside the
  provider (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#shared-storage-the-cluster-wide-disk-guard-a61)).
- The manager's webhook and metrics servers pin an explicit TLS 1.2 floor.
- Optional, opt-in `NetworkPolicy` templates for the manager and provider pods
  (`networkPolicy.enabled`, default off).
- Provider pods run under a dedicated, token-less ServiceAccount instead of the
  namespace `default` SA.
- **New in rc2:** provider pods no longer mount every labelled PVC in their namespace,
  only the migration PVCs a live `VMMigration` references (target read-only), and
  staged migration files on the `pvc` backend are opened with no symbolic link followed,
  so a link planted in the staging PVC can no longer make an import read the provider
  pod's credentials or an export write outside the PVC
  (→ [Changes since v0.4.0-rc1](#provider-pods-mount-only-the-migration-pvcs-a-live-vmmigration-references-399)).
- **New since rc2:** a libvirt VM's cloud-init seed, which carries its user-data (passwords,
  keys, tokens), is no longer readable by every local user of the hypervisor host: it is
  built `0600` in a dedicated directory that the provider checks and refuses to use if it
  is not private (#414). A clone no longer shares its source's SSH host keys (#413), and
  `create` on `VMClone` is documented as read access to the source's data. Go 1.26.10
  fixes GO-2026-6809 in `crypto/x509` (#411).
  (→ [Changes since v0.4.0-rc2](#changes-since-v040-rc2))

### Clustered providers (experimental, opt-in)

- `topology: cluster` libvirt providers can now be scheduled, created,
  described, deleted, powered, reconfigured, snapshotted, cloned (full clones,
  onto the source VM's own host) and exported (s3 / nfs, as a migration source) across a
  `Host`/`HostPool` inventory (ADR-0007). Every per-VM call is routed to the
  VM's host and checked against the VM's owner stamp. A clone is admitted
  against its source host's free capacity like any create (it waits,
  `Unschedulable`, when it does not fit, and never moves to another host), and
  a clone that fails for good removes the target VM it created. VMs are
  listed across every host (each tagged with its host; a host that cannot be
  listed is reported as unknown, never as empty) and unstamped domains can be
  adopted, keyed on (host, domain): the adopted VM's domain is stamped with its
  new owner (a serialized check-and-set with read-back) and the VM is bound to
  its host and counted in its capacity. A domain left by a deleted or restored
  VirtualMachine is not adopted; it is reported for the A6 re-attach runbook,
  and nothing new is adopted while a host cannot be listed. A clustered
  provider runs one replica with the `Recreate` strategy, so its rollout is a
  short outage. Still experimental — host-to-host migration is not implemented, and
  migration into a clustered provider is new in this release and experimental
  (below).
- The clustered scheduler subtracts what each host already holds: every VM
  bound to it, pending on it, or being deleted from it, in any namespace, at
  its admitted size. It also keeps concurrent creates and resizes from booking
  the same capacity or breaking hard anti-affinity. A VM no host can take
  reports `Placed=False/Unschedulable` with its own request (no other tenant's
  figures), and is retried with a backoff of up to 2 minutes. A resize-up is
  sent only if it fits on the VM's host (`Reconfiguring=False/
  InsufficientHostCapacity` otherwise); a pending VM's size is frozen until it
  is created; and a consumer in another namespace may orphan-on-delete a
  clustered VM only if the Provider allows it. New administrator gauges:
  `virtrigaud_host_committed_cpu` and `virtrigaud_host_committed_memory_mib`.
  There is no per-tenant quota on a shared clustered Provider yet
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#committed-capacity)).
- Clustered disks on a shared pool are protected across hosts (ADR-0007
  A6.1). A create or clone that finds a file where the VM's disk goes writes
  it only when no domain on **any** host of the Provider uses it (every name
  the disk may have had is checked), a host-path base image is checked on
  every host too, and a delete first checks every other host. A host that
  cannot be checked — including a `Host` that exists but cannot be routed —
  makes the operation fail closed (never counted by the circuit breaker) — so
  **clustered deletes wait while any host of the Provider is unreachable**:
  the VM shows `DeleteBlocked=True` (`HostUnreachable` or `DiskCheckFailed`)
  and `Ready=False/DeleteBlocked`, and is retried with a backoff from 15 s
  doubling to 5 min; `force-delete` and `orphan-on-delete` still release it at
  once. A VirtualMachine re-created under the name of a previous incarnation
  (after `orphan-on-delete`, a force-delete or a backup restore) is held as
  `Placed=False/RestorePending` on its pending host instead of making a second
  domain; an administrator re-attaches or removes the old domain with the A6
  runbook. A VM whose own domain turns up on another host (lost placement
  record) is held the same way, and deleting it is held until its pending host
  is moved. **Before deleting a `Host` to release held deletes, fence it**
  (power it off or revoke its access to the export); mount a shared pool only
  on the hosts of one clustered Provider, at the same path, with NFS locking
  enabled (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#shared-storage-the-cluster-wide-disk-guard-a61)).
- A restored or re-created clustered VirtualMachine is held **before** it is
  scheduled (ADR-0007 A6.2). The manager writes the restore marker
  `infra.virtrigaud.io/placement-uid` (the VM's own UID) on every clustered
  VM; a VM restored under a new UID, whose marker names another UID, waits as
  `Placed=False/RestorePending` and nothing is created. Before a clustered VM
  is first scheduled, the manager asks every host for a domain stamped with
  its namespace and name (a new owner filter on `ListVMs`, advertised as
  `supportsListOwnerFilter`; the VM's candidate domain names only, so an
  adopted domain is not seen): a previous incarnation holds it, and its own
  domain is re-bound where it runs, at the domain's own size. A clone's
  target is checked the same way. The marker only ever holds the VM that
  carries it; single-host Providers ignore it. **A clustered Provider holds at
  most one domain per namespace and name, so re-creating a VM after
  `orphan-on-delete` waits until the old domain is re-attached or removed.**
  **Roll the clustered libvirt provider right after the manager**: with an
  older one, new clustered VMs wait (`ProviderLacksListOwnerFilter`). Include
  VirtualMachine status in backups (Velero `restoreStatus`); the re-attach
  runbook needs only a `virsh metadata` re-stamp and, for a VM restored
  without status, the marker set to its new UID — no status edit. Keep the
  marker out of manifests you commit
  (→ [`docs/clustered-restore.md`](docs/clustered-restore.md)).
- On a clustered Provider, shrinking a **running** VM waits until the VM is
  powered off (`Reconfiguring=False/ShrinkPendingPowerOff`): a live shrink
  only deflates the balloon, which the guest can take back. VirtRigaud never
  powers the VM off for it; set `spec.powerState: Off` and back to `On`. A VM
  whose VMClass enables memory hot-add counts at its balloon ceiling (4× its
  memory), and a pending create whose VMClass has grown since it was scheduled
  is not retried (`PendingSizeGrew`).
- **New since rc2: a `VMMigration` can target a clustered libvirt Provider** (`s3` or
  `nfs`, target VM in the migration's namespace). The target VM is created and placed
  first, on a host whose `default` pool has room for the source disk, and the disk is
  imported onto that host; a host that fails after placement is waited for, not
  replaced. The disk-space check is a filter, not a reservation. Run in the lab from a
  single-host libvirt source; a vSphere or Proxmox source was not
  (→ [Changes since v0.4.0-rc2](#migration-into-a-clustered-libvirt-provider-407-412-415-416-experimental),
  [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)).

### Fixes

- **New since rc2** (→ [Changes since v0.4.0-rc2](#changes-since-v040-rc2)):
  - **A single-host libvirt delete of a VM and its clone together left the VM's seed
    directory (its user-data) on the host** (#408).
  - **Every clone shared its source's cloud-init identity**, and with it the SSH host
    keys (#413).
  - **vSphere imports left a `<id>-<hex>.hlog` in the landing folder, and every
    successful migration showed `LandedDiskCleanup=Unknown` forever** (#409).

- **New in rc2 (Wave 2):** the fixes below are described in
  [Changes since v0.4.0-rc1](#reconfigure-resizes-the-right-disk-and-vsphere-says-when-it-needs-a-power-off-401-breaking).
  - **Reconfigure grew the wrong disk** (affects v0.3.11 and rc1): vSphere grew the root
    disk with an additional disk's size, Proxmox sent every `spec.disks` size to `scsi0`
    and dropped the CPU/memory change after a disk grow while reporting success (#401,
    breaking).
  - **A CPU or memory change of a running vSphere VM without hot-add was retried
    forever** as `ProviderError`; it is now
    `Reconfiguring=False/ResizePendingPowerOff` and no longer counts toward the circuit
    breaker (#401).
  - **A manager restart could take a second snapshot or land a second disk**: a lost
    `SnapshotCreate`, `ExportDisk` or `ImportDisk` result is no longer re-sent blindly
    (#402, #405).
  - **A failed or deleted migration left its landed disk on the target for good**, and
    every vSphere import left about 1.4 KB of import-VM files (#403).
  - **A Proxmox s3 stage could be published truncated** after a cancelled import (#402).
  - **libvirt read a failed `snapshot-list` as the snapshot's absence**; it is now an
    error (#405).
  - **`VirtualMachine` `status.phase` was empty on healthy VMs** (#398).
  - **vSphere RPCs raced on a shared finder** (#400); **libvirt wrote fixed `/tmp` files
    on the hypervisor** (#400); **a libvirt client stuck connected on a dead transport
    was reused forever** (#398).
- **A `VMMigration` no longer fails with "import did not record disk info"
  (affects v0.3.11).** When two reconciles of a migration overlapped (the
  second started from a cache that did not yet show the first one's write),
  the second one's status write replaced the whole live status with its older
  copy. That could drop `status.diskInfo` (seen on a libvirt → S3 → vSphere
  migration, which then failed at `Creating`), or any other recorded field:
  the staging marker, the source VM's UID, the task reference, even the phase.
  A status write now never replaces a newer status: it either loses the
  conflict and the migration is reconciled again from the live state, or —
  for the result of a snapshot, export or import — only that result is
  written onto the live object; a result that cannot be written yet is held
  and written by the next reconcile, so the manager does not send the
  operation again. A manager restart or leader change between the operation
  and its write can still lose the result; from rc2 the call is then held or
  re-sent only where that is proven safe (see
  [Changes since v0.4.0-rc1](#a-provider-call-whose-outcome-is-unknown-is-held-not-re-sent-blindly-402-405)). A result
  dropped because the migration had moved on is reported with a `Warning`
  event `ProviderResultNotRecorded`. The same fix stops the `Provider` controller from
  dropping `status.adoption` written by VM adoption, and adoption from losing
  its status to the `Provider` controller's frequent writes.
- **A vSphere disk export no longer leaks a full copy of the disk on the
  source datastore.** `ExportDisk` clones the disk to a temporary
  `virtrigaud-export-…vmdk` next to it and deleted only the descriptor, so on
  VMFS the `-flat.vmdk` extent (the whole disk) stayed in the VM's folder and
  outlived the VM (found on vCenter 8 in the rc1 lab; also in v0.3.11). The
  clone is now removed with `VirtualDiskManager.DeleteVirtualDisk` on every
  exit, including a clone that failed or was cancelled part-way. Files left by
  earlier exports are not removed: look for `virtrigaud-export-*` files in the
  VM folders of datastores used as migration sources and delete them.
  → [Upgrade guide](docs/upgrading.md#leftover-virtrigaud-export--files-on-vsphere-datastores)

- **Deleting a VirtualMachine while its Provider cannot be reached no longer
  orphans the hypervisor VM.** When the manager had no client for the
  Provider — its runtime not `Running` (every provider rollout or restart),
  the provider pod down, a TLS or network misconfiguration — the finalizer was
  released without any provider `Delete`, leaving the VM running unmanaged.
  The delete now waits: `Ready=False/ProviderUnavailable` on a single-host VM,
  `DeleteBlocked=True/ProviderUnavailable` on a clustered one, retried with a
  backoff from 15 s doubling to 5 min until the Provider answers;
  `orphan-on-delete` and `force-delete` still release it at once. A Provider
  that no longer exists in the VM's own namespace still releases it as before.
  When a held delete's Provider answers but the `Delete` then fails for
  another reason, `Ready` becomes `False/ProviderError` instead of keeping the
  old hold (a clustered VM kept `Ready=False/DeleteBlocked` after any hold).
  Every `force-delete` that releases a VM without a confirmed provider delete
  now records a `Warning` event **`VMLeftOnHypervisor`** naming the VM and its
  provider id (never a host), so leftovers can be found
  (→ [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#deleting-a-vm-while-its-provider-cannot-be-reached)).
- **On a clustered Provider, `force-delete` of a consumer VM needs the
  Provider's permission, like `orphan-on-delete`.** A VM in another namespace
  than its clustered Provider could be released with `force-delete` while its
  delete was held or failing, leaving its domain running on the shared hosts
  outside the capacity accounting. Unless the Provider carries
  `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"`, the annotation
  is now ignored for such a VM (`ForceDeleteRefused=True/ForceDeleteNotAllowed`
  and a `Warning` event) and its delete keeps being retried as if it were not
  set. Same-namespace VMs and single-host Providers are unchanged
  (→ [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#detaching-a-vm-without-deleting-it-virtrigaudioorphan-on-delete)).
- **A single-host VM no longer loses its provider id after a Create, and is
  never created twice or released while its Create's outcome is unknown.**
  Editing or deleting a VM while its Create ran made the status write that
  records the id fail with a conflict: the next reconcile created a second VM
  (Proxmox) or a delete released the VM without deleting it on the
  hypervisor. Before a Create the VM now records
  `Provisioning=True/CreateRequested`, confirmed before the RPC, and re-reads
  itself live (the marker is written with an optimistic lock); the id is
  written with a UID- and id-checked status patch,
  retried on transient errors (a **`VMRecordNotSaved`** event names a VM it
  could not record). A Create whose outcome stays unknown is re-sent only to a
  provider that reports `supports_owner_rebind` (new in rc2: the vSphere, libvirt,
  Proxmox and mock providers of this release, which re-bind the VM they stamped;
  an older provider image, including an older Proxmox one, is never re-sent it);
  otherwise the VM waits
  with `Ready=False/CreateOutcomeUnknown` until an operator records its
  `status.id` or confirms that this attempt made no VM
  (`virtrigaud.io/create-outcome=not-created@<attempt id>`, audited by a
  `CreateOutcomeConfirmed` event), and its delete is held the same way
  (libvirt and Proxmox find and delete the VM by its owner stamp; libvirt
  releases it only when the provider confirms no such domain exists)
  (→ [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#a-create-whose-outcome-is-unknown-createrequested)).
- A Create refused with a non-retryable `FailedPrecondition` or
  `PermissionDenied` is held as `Ready=False/CreateRefused` (the event
  carries the provider's answer) and re-checked with a backoff instead of
  every 5 s.
- A Create answered with a retryable `VM_DISK_CHECK_FAILED` or
  `VM_OPERATION_FAILED` backs off from 15 s to 5 min
  (`Ready=False/CreateBackoff`) instead of retrying every 5 s.
- **`spec.powerState: OffGraceful` no longer loops on a powered-off VM.**
  Providers report such a VM as `Off`, so it was sent a graceful shutdown on
  every reconcile (on vSphere an `InvalidPowerState` fault and
  `Ready=False/ProviderError` every few seconds). `Off` now satisfies
  `OffGraceful`.
- A VirtualMachine is created only through a Provider that is not being
  deleted: on one that is, it shows `Ready=False/ProviderDeleting` and nothing
  is created — point it at another Provider, or delete it.
- A VirtualMachine's `Ready` condition no longer carries the provider
  resolver's error (which named the provider's TLS Secret, Service address and
  runtime phase) or a gRPC transport error: it says so with a constant message
  and the cause goes to the manager log. A VM being deleted is no longer
  reconciled on its own status writes, so a held delete is retried on its
  backoff only; `force-delete` and `orphan-on-delete` still take effect at once.
- libvirt `Reconfigure` is honest: a failed `setvcpus`, `setmem`, `setmaxmem`
  or offline disk resize is an error (it used to return success), a live change
  is also written to the domain's persistent definition (it used to be undone
  at the next power cycle), a stopped VM's CPU/memory grow beyond its maximum
  works, and a change the running VM cannot take is reported as
  restart-required (new `TaskResponse.restart_required`). While a change is
  pending a restart, `status.currentResources` counts the larger of the
  running and the next-boot size. A paused or suspended domain is reported as
  `Suspended` (never `Off`) and is not reconfigured. On clustered Providers, a
  VM's memory ceiling is recorded once from the provider
  (`DescribeResponse.max_memory_mib`) when missing, raised when the provider
  reports more and lowered after a confirmed shrink; its recorded CPU is raised
  to the vCPUs `Describe` reports; a failed Reconfigure is counted at the
  larger size and retried on a per-VM backoff (5 s to 5 min); a clustered answer
  without the new `honest_result` marker is not trusted; provider-reported sizes
  are bounded and committed sums saturate; the disk is grown
  before any CPU/memory change; and a single-host per-VM failure no longer
  counts toward the Provider's circuit breaker
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md)).
- vSphere `Describe` no longer treats a transient vCenter error as "the VM is
  gone" (which used to trigger a spurious re-create).
- vSphere `Create` no longer powers on every new VM. A VirtualMachine with
  `spec.powerState: Off` booted until the VirtualMachine controller
  hard-powered it off. Create now leaves the VM off, like libvirt and Proxmox,
  and the controller powers it on when `spec.powerState` is `On` or empty.
  Cloud-init is set at create time and still applies on the first boot. A VM
  created from an imported (migrated) disk is now also grown to its VMClass
  disk size and gets its additional disks; a bug skipped both. The grow is
  grow-only: a migrated VM smaller than the VMClass `diskDefaults.size`
  (default `40Gi`) is grown to it, using datastore capacity (all at once for a
  thick disk); a GPT guest sees a moved backup header, which `growpart` repairs.
  A failed grow or disk attach now fails the create (retried and completed)
  instead of being skipped silently
  (→ [`docs/upgrading.md`](docs/upgrading.md)).
- libvirt: hardware-accelerated `<domain type='kvm'>` is used again on hosts
  with a readable `/dev/kvm`, instead of falling back to software emulation.
- Chart: `networkPolicy` (previously `security.networkPolicies`) is never
  silently enabled by `helm upgrade --reuse-values`.
- Examples: every manifest under `examples/` applies against the current CRDs
  again (quoted `powerState` values, fields updated to the v1beta1 API), and
  CI now dry-runs them all.
- A `VMSnapshot` whose `Provider` could not be resolved on its first attempt is
  no longer marked `Ready` without a snapshot being taken. It stays pending and
  retries until the provider is reachable. Snapshots already affected are `Ready`
  with no `status.creationTime`; the
  [upgrade guide](docs/upgrading.md#post-upgrade-verification-checklist) has a
  query to list them.
- libvirt clones (single-host and clustered): a full clone needs a
  **powered-off source**. A `VMClone` of a running libvirt VM used to fail on
  qemu's image lock; it now waits (`Pending`, `Ready=False/SourceMustBePoweredOff`,
  never counted by the circuit breaker) and proceeds once the source is off.
  A full clone, or an s3/nfs export, of a VM with an external snapshot (whose
  active disk is libvirt's `0600` overlay) used to fail with "Permission
  denied". The copy now reads the source through `sudo -n` when the host
  allows it, with a regex-confined sudoers rule
  (→ [`docs/libvirt-clones.md`](docs/libvirt-clones.md#what-the-copies-run-as-root)).
  Root opens every disk in the format its domain definition names, never
  probed; a raw disk's chain is never walked. A copy is refused, not retried
  as the SSH user, when:
  - its chain has a symbolic link, an image with an external data file, or
    an image in a directory other accounts can write (unless sticky);
  - its disk is neither qcow2 nor raw.
  An nfs export runs as root only with the SSH user's own NFS identity.
  `nfs.uid`/`gid` 0 are refused for every nfs migration, whatever its
  providers (`NFSRootIdentityNotAllowed`): use a dedicated non-zero uid/gid
  that owns the export. Prerequisites: a QEMU with the
  CVE-2024-4467 fix, and a pool directory that is not group-writable. Replace
  any `qemu-img info -U *` sudoers wildcard with the documented regex rule.
  A clustered VM is placed only on a `Host` labelled
  `net.virtrigaud.io/<network>: "true"` for each libvirt network it uses
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#first-vm-checklist)).
- A `VMClone` refused by its provider while it waits — for its source to be
  powered off, an unreachable host, a previous incarnation of its target — no
  longer calls the provider in a tight loop. Each refusal rewrote the clone's
  status, which re-triggered the clone at once: a clone of a running libvirt
  VM called `Clone` about ten times a second instead of following its backoff
  (15 s doubling to 5 min). A repeated wait now writes nothing, and a waiting
  clone never shows `Phase=Cloning` or `Cloning=True`. A clone that does not
  fit on its host no longer jumps straight to its longest re-check interval,
  and a `VMSnapshot` whose provider delete fails no longer retries it in a
  tight loop for its first five minutes.
- A `VMClone` edited while its provider copied the disk (a libvirt clone is a
  synchronous copy of several minutes) no longer loses the clone it made: the
  edit used to make the controller's status write fail, the clone was sent
  again, refused as "already exists", and failed, leaving the copy untracked
  on the host. The record lands only on the `VMClone` it was made for: never on
  one deleted and re-created under the same name meanwhile, and never over a
  stored `Failed`.
- A `Clone` or `SnapshotCreate` is never sent from a stale read: the object is
  re-read from the API server right before the call, so a second, untracked
  snapshot or clone cannot be made from a cache that does not show the first
  one's result yet.
- **A `VMMigration` no longer leaves the source snapshot it created behind.**
  Deleting a migration whose snapshot `Delete` failed, or whose source
  `Provider` could not be reached, released it anyway and left the snapshot
  untracked on the live source VM (on libvirt it then blocked deleting that
  VM); a migration that failed for good never cleaned its snapshot up at all.
  A failed-for-good migration now deletes its snapshot, and a deletion waits
  for it: `SnapshotCleanup=False/SnapshotCleanupPending` (constant message,
  one `Warning` event), retried with a backoff from 15 s doubling to 5 min.
  A snapshot the provider no longer finds releases it (vSphere and Proxmox
  now answer a missing snapshot with `NotFound`); a source VM or own-namespace
  `Provider` that is gone releases it with a `SnapshotLeftOnHypervisor`
  Warning, never a "deleted".
  `virtrigaud.io/force-delete: "true"` on the `VMMigration` releases it anyway
  with a `SnapshotLeftOnHypervisor` Warning naming the snapshot
  (→ [Upgrade guide](docs/upgrading.md#breaking-changes)).
- A `VMSnapshot` being deleted while its VM's `Provider` cannot be read or
  reached is held with `Ready=False/ProviderUnavailable` and the same backoff
  (it was retried every 30 s with no status), and `force-delete` now releases
  it, with a `SnapshotLeftOnHypervisor` Warning. One whose `Provider` no
  longer exists in its own namespace is released with that Warning.
- **A migration no longer exports before its source snapshot is taken.** A
  task-backed snapshot is polled to completion; a failed one fails the
  migration, and a retry never exports an unconfirmed snapshot.
- The first retry of a failed migration waits `retryDelay` (it ran at once),
  and later retries back off from the failure they retry.
- A `VMSnapshot`'s `deleteOnVMDelete` and `maxAge` retention now apply once
  its VM is gone or being deleted (never while the VM is detached with
  `orphan-on-delete`).
- **A `VMSnapshot` is bound to its VM and Provider** (new status fields
  `vmUID`, `boundProvider`): a VM re-created under the same name, or moved to
  another Provider, is never sent the snapshot's delete; the VMSnapshot is
  released with a `SnapshotLeftOnHypervisor` Warning
  (→ [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)).
- Proxmox snapshot deletes resolve the VM's actual node (an HA move no longer
  reads as "snapshot gone"); vSphere re-reads the VM before answering a
  snapshot `NotFound`.
- A migration's source snapshot task is waited for at most
  `spec.options.timeout` (2 h by default), and provider task error text no
  longer reaches the VMMigration status.
- `VMClone`, `VMSnapshot` and `VMMigration` status and events no longer quote
  the provider resolver's error (which can name the provider's TLS Secret,
  Service address and runtime phase) or a gRPC connection error; the cause is
  in the manager log. Other provider answers in them are sanitized
  (→ [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)).
- A `VMMigration` status write that hits a conflict is retried on the live
  object without sleeping in the reconcile.
- A single-host `VMClone` whose `Clone` provably did nothing (the request
  never reached the provider, or the provider answered that it did not
  perform it — an SSH or libvirtd blip) is held as `CloneRetrying` and
  retried with a backoff (15 s doubling to 5 min) instead of failing for good.
  A clone whose outcome is unknown (the connection dropped mid-call, a
  timeout) used to fail so that a retry could never make a second VM; since
  rc2 it is held and re-sent to a provider that stamps and re-binds
  (`supports_owner_rebind`), and still fails on any other
  (→ [Changes since v0.4.0-rc1](#single-host-vmclone-targets-are-stamped-a-re-sent-clone-re-binds-395)).
- **A migration's `nfs` / `s3` staged disk is deleted.** The staged object (a
  full, unencrypted copy of the source disk) was never deleted. The manager now
  asks the migration's target `Provider` to delete it, through a new provider
  RPC, `DeleteStagedDisk`
  (→ [ADR-0011](docs/adr/0011-migration-staging-cleanup.md),
  → [Upgrade guide](docs/upgrading.md#breaking-changes)):
  - **When.** After the migration succeeds, and when the `VMMigration` is
    deleted. With `cleanupPolicy: Always`, also when the migration fails for
    good. With the default `OnSuccess`, a failed migration's staged disk is
    kept until the `VMMigration` is deleted. `Never` keeps it in every case.
  - **Confinement.** The request names the migration, never a bucket, key or
    path. The provider derives the object from the migration's namespace and
    name, so it can delete only that migration's own staging object. A spec
    edited after the export is never deleted at its new location.
  - **A failed delete is held.** The `StagingCleanup` condition is `False`
    (reason `StagingCleanupPending`), one Warning event is sent, and the delete
    is retried with a backoff from 15 s to 5 min. A deleted `VMMigration` is
    kept by its finalizer meanwhile (and while an export task still runs);
    `virtrigaud.io/force-delete=true` releases it.
  - **Older providers.** A provider image older than this release cannot
    delete. The delete is held for up to 24 hours, then recorded as left
    behind (`StagedDiskLeftBehind`).
  - **libvirt and vSphere NFS.** These targets empty an `nfs` object (truncate
    it to zero bytes) instead of deleting it, because qemu-img cannot delete a
    file over NFS. An empty file stays on the export. Proxmox deletes it.
  - **Proxmox import stage.** The Proxmox target also removes the node-side
    import stage of the migration's target VM, unless another migration in
    flight targets the same VM.
  - **Versioned S3 buckets and NFS snapshots** keep the data after the delete:
    expire noncurrent versions on `vmmigrations/`, and exclude the staging
    export from snapshots.
- **Unambiguous NFS staging name.** New migrations stage
  `vmmigrations_<namespace>_<name>_export.qcow2`. The old dash-separated name
  was shared by two migrations such as `team-prod/db` and `team/prod-db`.
  Migrations staged before the upgrade keep their old name.
- **Fixed: `Ready` cleanup, now guarded.** A migration that succeeds under this
  release deletes its source snapshot and runs `deleteAfterMigration`, which a
  broken manager-side PVC cleanup blocked before. A migration completed before
  the upgrade deletes nothing (one `SourceDeletionSkippedPreUpgrade` Warning).
  `deleteAfterMigration` deletes only the source VM the migration validated (by
  UID), never one re-created under the same name.
- **Security: staged images are checked before import.** NFS and S3 imports on
  Proxmox, vSphere and libvirt read the staged object once into a local copy,
  refuse an image that references another file (a backing file, data file or
  extent could have pulled a node or pod secret into the imported disk), and
  import that same copy. Headers are parsed in Go before qemu-img opens
  them. The NFS copy is bounded (`VIRTRIGAUD_MIGRATION_IMPORT_MAX_GIB`,
  default 1024, lowered by the target `Provider`'s disk-size limit, and a
  free-space check). Proxmox stages the copy in its root-only stage directory,
  checks the stage's header again at `Create`, and mounts migration exports
  `nosymfollow,nosuid,nodev,noexec`.
- **Proxmox VMs carry an owner stamp; a re-sent `Create` no longer makes a
  duplicate.** The Proxmox provider writes the owning `VirtualMachine`'s
  identity into each new VM's description, in the same call that creates or
  clones it. A `Create` sent again after an unknown outcome returns, and
  finishes if needed, the VM an earlier one made. While PVE is still cloning,
  the re-sent `Create` waits instead of starting a second clone.
  - **`ListVMs`** reports `owner_uid` / `owner_stamp_state`, so adoption skips
    VMs managed through another Provider object and a VM can be found by its
    owner. It also keeps listing a down node's VMs.
  - **`Delete`.** One that carries an owner never destroys a VM stamped for
    someone else (a reused VMID), a template, or an unstamped VM with another
    name.
  - **"Not found".** A VM that PVE reports missing from one node is treated as
    gone only when its VMID is free cluster-wide.
  - **Error classes.** `Create`/`Clone` failures are now classified: the new
    `CREATE_NOT_PERFORMED` reason marks a create that made nothing, and a
    clone timeout or 5xx is no longer reported as an invalid spec.

  VMs created before this release stay unstamped and behave as before.
  → [VM ownership: Proxmox](docs/vm-ownership.md#proxmox)
- **A vSphere VM bound to a template or an image artifact is held when its
  delete or power change is refused.** The provider refuses both
  (`VM_IS_IMAGE_ARTIFACT`); the manager used to retry them every 15 s / 5 s.
  Now: `Ready=False/VMIsImageArtifact` (a clustered delete:
  `DeleteBlocked=True/VMIsImageArtifact`), a constant message, one `Warning`
  event and a backoff up to 5 min. Detach the VM with
  `virtrigaud.io/orphan-on-delete: "true"`.
- **Single-host provider `Delete` requests carry the VM's owner**, so the
  Proxmox owner-checked delete never destroys an unrelated VM that reused the
  VMID; such an answer (`VM_NOT_OWNED`) releases the VirtualMachine with a
  `VMLeftOnHypervisor` Warning. Adopted VMs and clone targets send it too;
  Proxmox accepts an unstamped VM under the name adoption gave it (sanitized
  PVE name or `vm-<vmid>`). A Proxmox template is never deleted
  (`VM_IS_IMAGE_ARTIFACT`), and a delete refused for an unreadable owner stamp
  is held as `Ready=False/OwnerStampUnreadable`.
- **New in rc2: every other single-host per-VM call carries the owner too, and
  Proxmox checks it.** `Describe`, `Power`, `Reconfigure`, the snapshot calls,
  `GetDiskInfo`, `ExportDisk` and a clone's source are answered `VM_NOT_OWNED`
  (or `OWNER_STAMP_UNREADABLE`) for a reused VMID instead of acting on another
  tenant's VM. The `VirtualMachine` is held `Ready=False/VMNotOwned`; a
  migration or clone of a refused source fails before any side effect. Renamed
  unstamped VMs and stale-stamped adopted VMs are held after the upgrade
  (→ [Changes since v0.4.0-rc1](#single-host-proxmox-every-per-vm-call-is-owner-checked-393)).
- **New in rc2: single-host adoption stamps the adopted VM** with its new
  owner (`TransferOwner`, compare-and-swap; capability `supports_owner_transfer`),
  and never adopts a VM bound to a `VirtualMachine` of another Provider object
  for the same endpoint. This fixes the rc1 case where a re-adopted Proxmox VM
  kept a previous owner's stamp and was left behind on delete. VMs adopted by
  earlier releases are not backfilled
  (→ [Changes since v0.4.0-rc1](#single-host-adoption-stamps-the-adopted-vm-392)).
- **New in rc2: single-host `VMClone` targets are created first and stamped,
  and a re-sent `Clone` re-binds** instead of making a second clone (Proxmox) or
  failing and leaving the first clone untracked (vSphere, libvirt). New
  `VMClone` states: `Ready=False/CloneOutcomeUnknown` (held, re-sent with a
  backoff) and `Failed` / `ProviderConflict`
  (→ [Changes since v0.4.0-rc1](#single-host-vmclone-targets-are-stamped-a-re-sent-clone-re-binds-395)).
- **Nothing is created through a Provider being deleted, even before the
  manager's cache sees the deletion.** The Provider is re-read from the API
  server right before every `Create`, `Clone` and `ImportDisk`; a deleting,
  gone or replaced Provider holds the object with `Ready=False/ProviderDeleting`.
  The other side of the race is closed too: before a deleting Provider's
  in-use finalizer is removed, what still uses it is counted again from the
  API server, and a use the cache had not seen yet keeps it held.
- **`force-delete` of a `VMSnapshot` or `VMMigration` in another namespace than
  its clustered Provider needs the Provider's
  `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"`**, as for a
  `VirtualMachine`; otherwise `ForceDeleteRefused=True/ForceDeleteNotAllowed`
  and the delete keeps being retried.
- Conditions set by the VMClone, VMMigration, VMSnapshot, VMSet and Provider
  controllers (and the VirtualMachine conditions set through the shared
  helper) now record the object's `metadata.generation` as
  `observedGeneration` (it was `0`, or a per-change counter).
- The kustomize helper ClusterRoles in `config/rbac`
  (`*-admin-role`, `*-editor-role`, `*-viewer-role`) named the API group
  `infra.virtrigaud.io.infra.virtrigaud.io` and granted nothing; they now
  grant `infra.virtrigaud.io`, so **subjects already bound to them gain that
  access** — review the bindings first. The admin roles list their verbs
  instead of `*`, and the `provider-*` roles are labelled
  `virtrigaud.io/role-scope: platform-admin`.
- A `VMMigration` with `deleteAfterMigration: true` whose target stays off
  (`powerOn: false`) records a `SourceDeletedTargetNotStarted` Warning when it
  deletes the source.
- **A `VMMigration` with `powerOffBeforeMigration: true` on a running source no
  longer fails.** The migration set the source's `spec.powerState` to `Off`
  *and* powered it off itself; the VirtualMachine controller powered it off
  too, and on libvirt the second `virsh destroy` failed on the stopped domain,
  failing the migration terminally (`MigrationFailed: Failed to power off
  source VM before migration`). The migration now leaves the power-off to the
  VirtualMachine controller and waits (`Validating=False/WaitingForSourcePowerOff`,
  at most 10 minutes) until the source is observed off.
- **Power is idempotent on every provider.** A power-off of a VM that is
  already off, or a power-on of one that is already running, succeeds
  (libvirt: `domstate` re-read after a failed `start`/`destroy`/`shutdown`;
  vSphere: `InvalidPowerState`; Proxmox: no task when already in the
  requested state; mock).
- **libvirt no longer rewrites an operator's `default` storage pool.** Since
  v0.3.11, a create or disk import on a host whose `default` pool was not at
  `/var/lib/libvirt/images` destroyed, undefined and recreated it there. An
  existing pool is now never destroyed or redefined (only started when
  inactive); one at another path fails the operation closed with an
  `InvalidSpec` whose constant message says an administrator must fix the
  host (the host and the pool's path are in the provider log, never in a
  status). Mount shared storage
  **at** `/var/lib/libvirt/images` — see the host prerequisites in
  [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#host-prerequisites).
- **libvirt: deleting a snapshot removes its leftover overlay file.** libvirt
  10 commits an external snapshot into its base on delete but leaves the
  overlay `<domain>-disk.<snapshot>` in the pool; `SnapshotDelete` now removes
  it when it is provably unused (same rules as VM delete, cluster-wide on a
  clustered Provider). Overlays from snapshots deleted before the upgrade are
  not swept.
- libvirt `Describe` no longer logs `ERROR` lines on every poll for expected
  probe failures (no guest agent; statistics of a shut-off domain); they are
  `DEBUG`.
- Provider SDK: the `sdk/provider/client` RPC methods (`Create`, `Describe`,
  `TaskStatus` and the others) return a nil error on success again. Before, a
  successful call returned a non-nil error that printed as `<nil>`, and
  `WaitForTask` failed on its first poll.
- **vSphere: a disk import works on a freshly started provider.** A migration
  into vSphere (S3 or NFS) whose `ImportDisk` was the provider's first call
  after a restart failed with `datastore/StoragePod "<name>" not found`: the
  import looked up the datastore before selecting the datacenter.

## Known limitations

These are known in this release and are not regressions from v0.3.11 unless a bullet
says so. Each links to the page that documents it in full, where one exists.

- **Metrics are served over plain HTTP by default.** The manager's
  `--metrics-secure` is still off by default (planned for v0.5). `/metrics`
  includes administrator-sensitive gauges, so expose it only behind the chart's
  NetworkPolicy (`networkPolicy.enabled`, default off) or turn on
  `--metrics-secure=true` yourself
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#committed-capacity)).
- **Clustered libvirt providers are experimental.** `topology: cluster` is opt-in
  and may still change. Host-to-host migration is not implemented. Migration into a
  clustered provider is new and experimental: `s3` and `nfs` only, the target VM in the
  migration's namespace; run in the lab only from a single-host libvirt source; the
  disk-space check is a filter, not a reservation; the source disk's size is read once;
  a host that fails after placement is waited for, not replaced; two Providers whose
  hosts mount one pool are not serialised with each other; and a waiting target's
  `spec.importedDisk` can be edited by whoever may edit the VM (a swap is detected, not
  prevented)
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)).
- **Known issue (R31, being fixed): a clustered libvirt host's free-space figure can be
  stale.** libvirt reports a pool's `Available` as last refreshed, so on an NFS pool
  shared by several hosts the hosts can report different, stale free space to the
  disk-space filter that places a migration target. An administrator can refresh it with
  `virsh pool-refresh default` on the hosts.
- **Deleting a namespace that holds an in-use `Provider`.** Kubernetes deletes the
  provider's runtime with the namespace, and nothing can stop that. VMs whose
  deletes miss the pod's termination window are held (`ProviderUnavailable`) and
  the namespace stays `Terminating` until an administrator acts
  (`virtrigaud.io/force-delete: "true"` on the `Provider` or on each VM, which
  leaves the hypervisor VMs unmanaged). Delete the VMs before the namespace
  (→ [ADR-0010](docs/adr/0010-provider-in-use-finalizer.md),
  [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#deleting-a-namespace-that-contains-a-provider)).
- **Proxmox: the legacy name rule can be claimed by whoever can create a VM.**
  An unstamped VM named like its `VirtualMachine` still passes the owner check (VMs
  created before owner stamps exist have no other proof), and the provider cannot tell
  whether the `VirtualMachine`'s own VM was ever stamped. So once a `VirtualMachine`'s
  VM is removed, a PVE user with `VM.Allocate` on a pool or path where VirtRigaud's VMs
  live can create an unstamped VM with that name at the freed VMID, and the
  `VirtualMachine` then reads, powers, resizes, snapshots and copies it as its own.
  A VM stamped for anyone else is refused. Grant `VM.Allocate` there as carefully as
  `VM.Config.Options`, and to no one but VirtRigaud's Provider tokens and your
  administrators. [ADR-0012](docs/adr/0012-proxmox-legacy-unstamped-name-rule.md)
  retires the rule: phase 1 is planned for v0.4.1 behind validation on a real PVE
  cluster, the rule is dropped by default in v0.5.0. Even then a stamp can be copied by
  someone who knows the `VirtualMachine`'s UID and holds `VM.Config.Options`; that risk is
  accepted for v0.4.0 (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#limits)).
- **Proxmox: the owner check and the call are two requests.** A VMID freed and reused
  between them is not caught. Do not delete VirtRigaud-managed VMs outside VirtRigaud:
  delete the `VirtualMachine`, or detach it with `orphan-on-delete`
  (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#limits)).
- **The admission policy needs Kubernetes 1.30+.** On an older cluster nothing is
  installed, and anyone who can create or update `VirtualMachine`s in a namespace can
  forge the adoption markers (`virtrigaud.io/adopted`, `adopted-id`, `adopted-host`) and
  the `VMClone` marker (`clone-uid`). Restrict `create`/`update` on `virtualmachines` to
  trusted users there, or reserve the keys with Kyverno or Gatekeeper. (A `VMMigration`
  no longer depends on the markers.)
  → [`docs/admission-policy.md`](docs/admission-policy.md)
- **A kustomize overlay that changes the namespace or name prefix must repeat the
  `replacements` block of `config/default`.** Otherwise the admission policy keeps
  exempting the old manager name and denies the manager's own writes.
  → [`docs/admission-policy.md`](docs/admission-policy.md#configuration)
- **A leader-election overlap during a migration's target `Create` can still fail the
  migration.** If the new leader finds the target VM carrying a claim it cannot match
  (the old leader's), it re-checks, but if the claim never matches the migration fails
  `TargetVMNotOwned`. This fails closed; delete the target VM it created and re-create
  the migration (→ [`docs/cross-namespace-targets.md`](docs/cross-namespace-targets.md#the-target-vm-must-be-the-migrations-own)).
- **VMs adopted before rc2 are not re-stamped, and clones made before rc2 are
  unstamped.** Their deletes follow the unstamped rules. On Proxmox, a VM adopted earlier
  that still carries a previous owner's stamp is held `Ready=False/VMNotOwned` (and
  left behind on delete with `VMLeftOnHypervisor`) until you remove the stale
  `virtrigaud.owner` line from its Notes (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#vms-adopted-before-this-release)).
- **libvirt: the `default` storage pool must be at `/var/lib/libvirt/images`.**
  Mount shared storage at that path, and keep `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`
  (the image catalog) apart from the pool. A pool at another path is refused with
  `InvalidSpec` and never modified
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#host-prerequisites)).
- **The disk-size limit does not cover an oversized image.** It checks class and
  spec sizes and the real disk sizes of clone and migration sources, but not an
  image or template whose own disk is larger than the class
  (→ [chart README](charts/virtrigaud/README.md#disk-size-limit)).
- **vSphere: a CPU or memory change of a running VM needs hot-add.** Without it the
  change waits for a power-off and is shown as `Reconfiguring=False/ResizePendingPowerOff`
  instead of being retried against vCenter. The provider still reports online reconfigure
  as supported regardless of the VM
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md#vsphere-changes-a-running-vm-cannot-take)).
- **A disk-size change alone does not trigger a `Reconfigure`.** The manager sends one
  when the effective CPU or memory changes, and the disk sizes travel with it. To apply a
  `spec.disks` size change, or a growth skipped earlier (for example while the disk had
  snapshots), change CPU or memory too
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md#when-a-reconfigure-is-sent)).
- **`Reconfigure` resizes few disks.** No provider's `Reconfigure` grows the root disk
  from a `spec.disks` entry or adds a disk. vSphere grows only an additional disk that
  `Create` recorded under the entry's name, so the additional disks of VMs created by
  rc1 or earlier are not resized. Proxmox resizes none.
- **Held calls need an operator.** A `VMMigration` or `VMSnapshot` held in
  `SnapshotOutcomeUnknown`, `ExportOutcomeUnknown` or `ImportOutcomeUnknown` stays held
  until you check the provider and confirm the attempt with
  `virtrigaud.io/operation-outcome=not-performed@<attempt id>` (or delete the object),
  except an export or import on a re-send-safe backend, which the manager sends again
  after 35 minutes. A failure write that is itself lost holds the object too, and
  deleting a held object does not look up the snapshot the lost call may have made
  (→ [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md#limitations)).
- **Remote host commands can outlive a cancelled call, and Proxmox `nfs` is not re-send
  safe.** A command running on the hypervisor host keeps going after the call that
  started it is cancelled. The Proxmox `nfs` import copies to completion and publishes the
  stage at an arbitrary later time, and its `nfs` export writes in place, so both are
  held rather than re-sent (only `s3` is re-send safe on Proxmox).
  Single-host libvirt's flatten, `qemu-img convert` and `dd` run without `timeout(1)`, so
  an orphan can still be running after the 35-minute settle time
  (→ [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md#why-each-provider-answers-the-way-it-does)).
- **A `pvc` migration on a filesystem that reports a link count other than 1 is
  refused**, as is any link or special file in the staging PVC. Use `s3` or `nfs`, or a
  filesystem that reports link counts
  (→ [Staged-file hardening](examples/migration/README.md#staged-file-hardening-pvc-backend)).
- **Provider locks are per process: run one replica per Provider.** The landing,
  landed-disk delete and import serialisation live inside one provider process. vSphere
  runtimes are rendered with one replica already; do not set `spec.runtime.replicas`
  above 1 on libvirt or Proxmox Providers either. The landed-disk in-use checks see only
  the VMs of one vCenter or libvirt host, so do not share the landing datastore or the
  `default` pool directory with hypervisors the Provider does not manage
  (→ [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)).
- **Landed disks are not deleted in some cases.** A disk is kept with
  `cleanupPolicy: Never`, when the provider reports it in use, when the provider image
  is older than rc2, the Provider is gone, or a Proxmox Provider has no migration SSH
  data plane (`LandedDiskLeftBehind` says so). Disks left by migrations that failed
  before rc2 are not removed either.
- **NFS staging cleanup does not remove the file on libvirt and vSphere targets.**
  It empties the staged file (a 0-byte file remains); Proxmox deletes it.
  Versioned or object-lock S3 buckets and NFS server snapshots keep the data:
  expire noncurrent versions on `vmmigrations/` and exclude the staging export
  from snapshots (→ [ADR-0011](docs/adr/0011-migration-staging-cleanup.md)).
- **`verifyChecksums` does not checksum NFS migrations.** On the `nfs` backend
  the disk is written by `qemu-img` over NFS, which computes no byte checksum, so
  no source or target checksum is recorded. Integrity rests on the target's
  `qemu-img check` of the converted image. Use `s3` when you need an end-to-end
  SHA-256 comparison
  (→ [ADR-0006](docs/adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md)).
- **Leftovers from earlier releases are not cleaned up automatically:** staged
  migration objects, landed migration disks and vSphere import-VM files of migrations
  that ran before rc2 (and the `.hlog` that rc2 imports left), libvirt snapshot overlay
  files (`<domain>-disk.<snapshot>`), libvirt cloud-init seeds in `/tmp` (readable by
  other local users until you make them private, and seed directories of VMs deleted
  together with a clone) and vSphere `virtrigaud-export-*` files. Find and remove them as described in
  [docs/upgrading.md](docs/upgrading.md#leftovers-from-earlier-releases).

## Upgrade

Since rc2: **create the seed directory on every libvirt host first**, then CRDs, then
the manager, then the providers (the clustered libvirt provider before you migrate into
it); see [Upgrade order for this release](#upgrade-order-for-this-release).

See **[docs/upgrading.md](docs/upgrading.md)** for the full breaking-change
table, required upgrade order, new required privileges, and a post-upgrade
verification checklist. The roll order is **CRDs, then the cross-namespace grants
(`spec.consumerNamespaceSelector`) and the admission policy, then the manager, then
the providers**, and roll the providers promptly after the manager: until a
provider is upgraded, a Proxmox provider does not enforce the new owner checks, and
no provider that predates rc2 is ever sent a `Create` or `Clone` again after an
unknown outcome, a lost export or import is held for an operator instead of re-sent,
and a failed migration's landed disk is recorded as left behind instead of deleted. The
order was validated in the rc1 lab (see
[Required upgrade order](docs/upgrading.md#required-upgrade-order) and
[Changes since v0.4.0-rc1](#upgrade-order-for-rc2)).
