# libvirt clones and the linked-clone dependency

This page describes how the libvirt provider clones a VM (`VMClone`), why
**linked** clones are disabled in this release, how an existing linked clone
depends on its source VM, and what the provider refuses to do while that
dependency exists. It applies to single-host libvirt Providers; a clustered
(`topology: cluster`) libvirt Provider does not clone yet.

## Full and linked clones

| | Full clone (`spec.options.type: FullClone`, the default) | Linked clone (`spec.options.type: LinkedClone`) |
|---|---|---|
| Status on libvirt | **Supported** | **Disabled in this release** (see below); linked clones made by an earlier release keep working |
| Disk | An independent copy of the source's primary disk (`qemu-img convert`, flattening any backing chain) | A thin qcow2 overlay whose **backing file is the source VM's disk** (`qemu-img create -f qcow2 -b <source disk>`) |
| Speed and space | Slow, full size | Fast, only the clone's own writes |
| Dependency | None: deleting either VM never touches the other's disk | The clone reads every block it has not written from the source's disk, for as long as it exists |

A clone is named `<target namespace>.<target name>` on the host, with the disk
`<pool directory>/<domain>-disk.qcow2`, and is left powered off.

## Linked clones are disabled

A linked clone's backing file is the source VM's **live** disk, and nothing
freezes it. If the source is powered on while a linked clone of it exists —
typically while the clone is shut off — the source's guest writes to the very
file the clone reads its unwritten blocks from, and the clone's data is
silently corrupted. No provider-side check can catch this — no delete or
snapshot is involved, just a power-on — so libvirt linked clones are
**disabled** until the base is frozen at clone time.

- The libvirt provider reports `supportsLinkedClones: false`, so a `VMClone`
  with `spec.options.type: LinkedClone` through it fails before any provider call:
  `Phase=Failed`, `Ready=False` / `Failed=True` with reason
  `LinkedCloneUnsupported`, a `Warning` event, and a message naming
  `spec.options.type: FullClone`. Recreate the `VMClone` as a full clone.
- A linked `Clone` request that still reaches the provider (a manager that could
  not read the provider's capabilities) is refused with `InvalidArgument`
  ("linked clones are disabled on libvirt in this release: the source disk is
  not frozen, so the source's writes would corrupt the clone; use FullClone")
  before any command runs on the host. It is not counted toward the Provider's
  circuit breaker.
- Full clones are unaffected. vSphere linked clones are unaffected.
- **Existing linked clones** (made by an earlier release) keep working and are
  not touched. The guards below still protect them. Keep their source VMs
  powered off.

**Follow-up:** re-enable libvirt linked clones once the base is frozen at clone
time — either by taking an external snapshot of the source at clone time (the
clone is backed by the frozen snapshot base while the source continues in a new
overlay) or by cloning only from an immutable template image.

## The linked-clone dependency

While a linked clone exists, its source VM's disk must be neither removed nor
rewritten, or the clone loses its data. The provider therefore refuses the
operations below on any VM whose disk file another domain on the same host uses
— as its backing file anywhere in its image chain (a linked clone, or a clone
of a clone), or as a disk of its own:

| Operation on the source VM | Why it is refused |
|---|---|
| Delete | It would remove the clone's backing file. |
| Snapshot revert | Reverting to an internal snapshot rewrites the image the clone reads. |
| Snapshot delete | Deleting an external snapshot commits it into its base image — the file the clone reads. |
| Snapshot create (every kind) | An internal or memory snapshot writes its snapshot table and saved RAM into the backing file itself. A disk-only (external) snapshot does not, but it moves the source's disk under a new overlay: the dependency would no longer be visible at the source's own disk, and a later snapshot delete would commit into the file the clone reads. |

The clone's own operations are not restricted: it can be deleted, snapshotted
and reverted as usual. Once every linked clone of the source is deleted, the
source's operations are allowed again — nothing has to be reset.

**To delete a source VM, delete its linked clones first.**

### What the requester sees

The provider refuses **before anything is changed**: the domain is not stopped,
undefined or snapshotted, and no file is removed.

- **Delete**: the `VirtualMachine` keeps its finalizer and gets
  `Ready=False` with reason `DeleteBlocked`, a message saying how many other
  domains use its disk, and a `Warning` event `DeleteBlocked`. The delete is
  re-checked every minute and completes on its own once the clones are gone.
  To remove the `VirtualMachine` without deleting the hypervisor VM, set
  `virtrigaud.io/orphan-on-delete: "true"`; `virtrigaud.io/force-delete: "true"`
  also removes the finalizer (the domain is left on the host in both cases).
  A linked clone in **another namespace** holds the source's delete the same way,
  until that clone is removed or the source is detached with orphan-on-delete.
- **Snapshots**: a refused create shows in the `VMSnapshot`'s status. A refused
  delete keeps the `VMSnapshot` and its finalizer — the snapshot is still on the
  host — with `Ready=False` and `Deleting=False`, reason `DeleteBlocked`, and a
  `Warning` event `DeleteBlocked`; it is re-checked every minute and completes
  once the clones are gone. `virtrigaud.io/force-delete: "true"` on the
  `VMSnapshot` removes it anyway, leaving the snapshot on the host (a `Warning`
  event `SnapshotLeftOnHypervisor` names the snapshot).
- **Migration**: a `VMMigration` of the source VM fails at its snapshot step
  (`Failed to create snapshot: …`).
- **Power on**: never refused. After the source of an existing linked clone is
  started, the provider counts the VMs that depend on its disk and the
  `VirtualMachine` gets the condition `LinkedClonesDependOnDisk=True` (and a
  `Warning` event): "powering this VM on while its linked clones are shut off
  corrupts them". `Ready` is unchanged. The condition is removed once a start
  finds no dependents.

The message names only the requesting VM's own domain and the number of
dependent domains — never another domain (it may belong to another tenant) or
a host path. On the wire the refusal is `FailedPrecondition` with a
`google.rpc.ErrorInfo` reason `VM_DISK_IN_USE` (domain
`provider.virtrigaud.io`); a clustered Provider's routed `Delete` adds
`VM_OPERATION_FAILED`. The manager maps it to a `Conflict` (only this reason
makes a delete `DeleteBlocked`), and it never counts toward the Provider's
circuit breaker.

A `VMSnapshot` whose provider delete fails for any other reason (an unreachable
host, an open circuit breaker, a dependency check that could not run) also
keeps its finalizer, with `Ready=False` / `Deleting=False` reason
`ProviderError` and a `Warning` event, and is retried with a growing delay (15
seconds up to 5 minutes). Only a snapshot the provider reports as already gone,
or `force-delete`, releases it.

### How the dependency is found

Before the operation, the provider reads every domain defined on the host —
running or shut off, VirtRigaud's or anyone else's — and each disk's full image
chain. A running domain's definition lists its chain in `<backingStore>`, which
is used as is; a shut-off domain's chain is read with
`qemu-img info -U --backing-chain`, through passwordless `sudo -n` when the host
allows it (so a disk the SSH user cannot read — a `0600 libvirt-qemu` image, a
root-squashed NFS pool — does not fail the check), and as the SSH user
otherwise. A VM "has dependents" when any other domain references one of its
disk files as a disk, a backing file, or any other file. Delete only runs the
check when it has files to remove.

The check fails closed: if a definition or a disk's image chain cannot be read,
the operation is not performed and returns a retryable error
(`Unavailable` with `google.rpc.ErrorInfo` reason `VM_DISK_CHECK_FAILED`, which
the manager retries and never counts toward the Provider's circuit breaker),
with the details in the provider log. Give the provider's SSH user passwordless
`sudo` for `qemu-img`, or membership of the group the disks belong to (`kvm` for
the disks VirtRigaud creates on Debian/Ubuntu hosts), or run it as `root`.

## What Delete removes

Delete reads the domain's definition once, structurally — if it cannot be read,
the delete fails with a retryable error and the domain is left intact — and
removes only:

- the domain's **own top-level disk files** (`<disk type='file'
  device='disk'>`) that are, as the definition names them, **regular files**
  (not symbolic links) lying **directly inside** the default storage pool's
  directory or an allowed image directory (`VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`,
  default `/var/lib/libvirt/images`), resolved on the host. A symlinked disk is
  left in place, link and target alike; each file is re-checked right before
  it is removed;
- the files **below them in their backing chains that are its own**: named
  after its disk (`<domain>-disk.<anything>` — the disk it was created with,
  under the overlays its external snapshots added), meeting the same rules,
  and used by no other domain. A base image or another VM's disk is never
  removed, and neither is a pre-snapshot disk a linked clone still reads;
- its **VirtRigaud cloud-init seed directory** (`/tmp/virtrigaud-cloudinit-<domain>.<random>/`,
  or an earlier release's `/tmp/virtrigaud-cloudinit/<domain>/`) — unless another
  domain still references a file in it (a clone's CD-ROM keeps pointing at its
  source's seed ISO, and a domain whose CD-ROM file is missing no longer
  starts).

It never removes any other backing file, cdrom or floppy media, or any file
outside those directories: such a file is left in place and logged by the
provider.

A single-host Provider deleting a VM whose domain no longer exists still cleans
up the disk VirtRigaud named after it, `<name>-disk.qcow2` in the default pool's
directory, but only if it exists and no remaining domain uses it — a linked
clone of the vanished VM keeps its backing file. Nothing else is looked for.

## Clone files on the host

- **Disk mode.** Every VM disk VirtRigaud creates or adopts — a new VM's disk,
  an image copied or downloaded for it, an imported disk adopted in place, and
  a clone's disk — is `chown libvirt-qemu:kvm` and `chmod 0660`, no longer
  world-writable (`0777`). The provider's SSH user reads VM disks (disk in-use
  checks, `GetDiskInfo`, s3/nfs disk export, a full clone's copy) as a member of
  the `kvm` group, or as `root`; the in-use check also uses passwordless
  `sudo -n qemu-img` where the host allows it. **Disks created by an earlier
  release keep their mode** — to close one, shut its VM off and
  `sudo chown libvirt-qemu:kvm` and `sudo chmod 0660` it. Least privilege (`0600
  libvirt-qemu`, with every read through `sudo -n`) is a tracked follow-up.
- **UEFI varstore.** For a UEFI source, the clone gets its own copy of the
  source's `<nvram>` varstore, `<nvram directory>/<clone domain>_VARS.fd`. The
  clone is refused (`Conflict`) before any of its files is written when that
  path is a symbolic link or the varstore (or any other file) of an existing
  domain. An unused file left by an earlier, failed clone is first removed, and
  the copy is then created fresh: `sudo dd iflag=nofollow oflag=nofollow
  conv=excl`, so it never follows a symlink at either end and never writes into
  a file that appeared in between (the clone fails instead). The copy is
  `chmod 0600`.

## Known limitations

- Powering on the **source** VM of an existing linked clone lets its guest
  write to the clone's backing file, which corrupts the clone. The provider does
  not prevent it — this is why new linked clones are disabled — but it warns:
  the source's `VirtualMachine` gets `LinkedClonesDependOnDisk=True` after it is
  started (see [What the requester sees](#what-the-requester-sees)). Keep the
  source of an existing linked clone powered off.
- The dependency check reads the whole host on every guarded operation: every
  domain's definition, and one `qemu-img info` per disk of every shut-off
  domain (a running domain's chain comes from its definition). Delete skips it
  when it has no files to remove, and nothing is cached, so on a host with many
  shut-off domains Delete and snapshot operations take longer.
- On a clustered Provider, snapshots are not routed to hosts yet; when they
  are, the same dependency check applies on the VM's host.

## Upgrade notes

This behavior ships in the release that follows v0.3.11 and fixes a data-loss
bug: **deleting a running linked clone deleted its source VM's disk**, because
Delete removed every file its definition listed, including the backing file.
After upgrading:

- **libvirt `LinkedClone` is refused** (`LinkedCloneUnsupported`): use
  `FullClone`. Existing linked clones keep working, but their source VMs cannot
  be deleted, reverted or snapshotted while the clones exist, and warn when they
  are powered on;
- a `VMSnapshot` whose provider delete fails or is refused now **keeps its
  finalizer** and is retried, instead of being removed with the snapshot left
  on the host; use `virtrigaud.io/force-delete: "true"` to remove one anyway;
- new VM disks are `0660 libvirt-qemu:kvm` — make sure the provider's SSH
  user is `root`, in the `kvm` group, or allowed passwordless `sudo qemu-img`
  before upgrading, or the dependency check fails (retryably) and Delete and
  snapshot operations do not proceed;
- Delete removes fewer files: nothing outside the pool / allowed image
  directories, no symbolic links or their targets, and no backing files other
  than the VM's own external-snapshot chain. A deleted VM whose domain is
  already gone only has `<name>-disk.qcow2` cleaned up. Check the provider log
  for `Not deleting disk` lines if disk usage grows.
