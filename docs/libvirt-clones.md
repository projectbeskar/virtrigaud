# libvirt clones and the linked-clone dependency

This page describes how the libvirt provider clones a VM (`VMClone`), how a
**linked** clone depends on its source VM, and what the provider refuses to do
while that dependency exists. It applies to single-host libvirt Providers; a
clustered (`topology: cluster`) libvirt Provider does not clone yet.

## Full and linked clones

| | Full clone (`spec.linked: false`) | Linked clone (`spec.linked: true`) |
|---|---|---|
| Disk | An independent copy of the source's primary disk (`qemu-img convert`, flattening any backing chain) | A thin qcow2 overlay whose **backing file is the source VM's disk** (`qemu-img create -f qcow2 -b <source disk>`) |
| Speed and space | Slow, full size | Fast, only the clone's own writes |
| Dependency | None: deleting either VM never touches the other's disk | The clone reads every block it has not written from the source's disk, for as long as it exists |

Both are named `<target namespace>.<target name>` on the host, with the disk
`<pool directory>/<domain>-disk.qcow2`, and are left powered off.

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
- **Snapshots**: the `VMSnapshot` shows the refusal in its status (create), or in
  a `SnapshotDeleteFailed` event (delete — the `VMSnapshot` is still removed,
  and the libvirt snapshot is left on the host).
- **Migration**: a `VMMigration` of the source VM fails at its snapshot step
  (`Failed to create snapshot: …`).

The message names only the requesting VM's own domain and the number of
dependent domains — never another domain (it may belong to another tenant) or
a host path. On the wire the refusal is `FailedPrecondition` with a
`google.rpc.ErrorInfo` reason `VM_DISK_IN_USE` (domain
`provider.virtrigaud.io`); a clustered Provider's routed `Delete` adds
`VM_OPERATION_FAILED`. The manager maps it to a `Conflict`, and it never counts
toward the Provider's circuit breaker.

### How the dependency is found

Before the operation, the provider reads every domain defined on the host —
running or shut off, VirtRigaud's or anyone else's — and each disk's full image
chain with `qemu-img info -U --backing-chain` (a shut-off domain's definition
does not list its backing files). A VM "has dependents" when any other domain
references one of its disk files as a disk, a backing file, or any other file.

The check fails closed: if a definition or a disk's image chain cannot be read,
the operation is not performed and returns a retryable error, with the details
in the provider log. The provider's SSH user must therefore be able to read
every domain's disk on the host (be `root`, or a member of the group the disks
belong to — `kvm` for the disks VirtRigaud creates on Debian/Ubuntu hosts); the
same already holds for image confinement at create time.

## What Delete removes

Delete reads the domain's definition once, structurally, and removes only:

- the domain's **own top-level disk files** (`<disk type='file'
  device='disk'>`) that lie **directly inside** the default storage pool's
  directory or an allowed image directory (`VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`,
  default `/var/lib/libvirt/images`), resolved on the host — a symlink out of
  those directories does not count;
- its **VirtRigaud cloud-init seed directory** (`/tmp/virtrigaud-cloudinit-<domain>.<random>/`,
  or an earlier release's `/tmp/virtrigaud-cloudinit/<domain>/`) — unless another
  domain still references a file in it (a clone's CD-ROM keeps pointing at its
  source's seed ISO, and a domain whose CD-ROM file is missing no longer
  starts).

It never removes a backing file (`<backingStore>`), cdrom or floppy media, or
any file outside those directories: such a file is left in place and logged by
the provider. For a VM that took external (disk-only) snapshots, the disk
underneath the snapshot overlays is part of its backing chain and is therefore
also left in place; remove it by hand once nothing references it.

A single-host Provider deleting a VM whose domain no longer exists still cleans
up files named after it (`<name>-disk.qcow2`, `<name>.qcow2`, `<name>-disk`
under `/var/lib/libvirt/images`), but only files that exist, lie in those
directories, and that no remaining domain uses — a linked clone of the vanished
VM keeps its backing file.

## Clone files on the host

- **Disk mode.** A clone's disk is `chown libvirt-qemu:kvm` and `chmod 0660` —
  no longer world-writable (`0777`). The provider's SSH user reads it (disk
  in-use checks, `GetDiskInfo`, s3/nfs disk export, a clone of the clone) through
  membership of the `kvm` group, or as `root`.
- **UEFI varstore.** For a UEFI source, the clone gets its own copy of the
  source's `<nvram>` varstore, `<nvram directory>/<clone domain>_VARS.fd`. The
  clone is refused (`Conflict`) before any of its files is written when that
  path is a symbolic link or the varstore (or any other file) of an existing
  domain; an unused file left by an earlier, failed clone is overwritten. The
  copy is made with `sudo dd iflag=nofollow oflag=nofollow`, so it never follows
  a symlink at either end, and the copy is `chmod 0600`.

## Known limitations

- Powering on the **source** VM of a linked clone lets its guest write to the
  clone's backing file, which corrupts the clone. The provider does not prevent
  it; keep a linked clone's source powered off (a template).
- A linked clone created **while** its source is being deleted can be left with
  a missing backing file: the dependency check runs before the source's domain
  is removed, not under a lock shared with `Clone`.
- On a clustered Provider, snapshots are not routed to hosts yet; when they
  are, the same dependency check applies on the VM's host.

## Upgrade notes

This behavior ships in the release that follows v0.3.11 and fixes a data-loss
bug: **deleting a running linked clone deleted its source VM's disk**, because
Delete removed every file its definition listed, including the backing file.
After upgrading:

- a source VM with linked clones can no longer be deleted, reverted, or
  snapshotted until its clones are deleted (see above);
- new clone disks are `0660 libvirt-qemu:kvm` — make sure the provider's SSH
  user is `root` or in the `kvm` group before creating clones;
- Delete removes fewer files: nothing outside the pool / allowed image
  directories and no backing files. Check the provider log for
  `Not deleting disk` lines if disk usage grows.
