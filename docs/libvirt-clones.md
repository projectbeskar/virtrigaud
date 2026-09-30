# libvirt clones and the linked-clone dependency

This page describes how the libvirt provider clones a VM (`VMClone`), why
**linked** clones are disabled in this release, how an existing linked clone
depends on its source VM, and what the provider refuses to do while that
dependency exists. It applies to single-host libvirt Providers and to
clustered (`topology: cluster`) ones, which clone onto the source VM's own
host and serve full clones only (see
[`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md)).

## Full and linked clones

| | Full clone (`spec.options.type: FullClone`, the default) | Linked clone (`spec.options.type: LinkedClone`) |
|---|---|---|
| Status on libvirt | **Supported** | **Disabled in this release** (see below); linked clones made by an earlier release keep working |
| Disk | An independent copy of the source's primary disk (`qemu-img convert`, flattening any backing chain) | A thin qcow2 overlay whose **backing file is the source VM's disk** (`qemu-img create -f qcow2 -b <source disk>`) |
| Speed and space | Slow, full size | Fast, only the clone's own writes |
| Dependency | None: deleting either VM never touches the other's disk | The clone reads every block it has not written from the source's disk, for as long as it exists |

A clone is named `<target namespace>.<target name>` on the host, with the disk
`<pool directory>/<domain>-disk.qcow2`, and is left powered off. The copy
flattens the source's whole image chain, so the clone of a VM with external
snapshots is one standalone disk, with none of the source's snapshots.

## A full clone needs a powered-off source

A libvirt full clone copies the source VM's disk chain with `qemu-img
convert`, which cannot open an image a running QEMU holds (`Failed to get
shared "write" lock`) — and a copy of a disk a running guest is writing would
not be a consistent clone anyway. **The source VM must be powered off** (`virsh
domstate` "shut off"). vSphere is different: it clones running VMs.

- The provider reads the source's state before it copies anything, and refuses
  any other state (running, paused, suspended, in shutdown, ...) with
  `FailedPrecondition` and the ErrorInfo reason `VM_SOURCE_RUNNING` ("the
  clone's source VM is "running": power off the source VM to clone it").
  Nothing is copied or written, and the refusal is never counted toward the
  Provider's circuit breaker. A clone an earlier attempt already made (its
  answer lost) is still reported as done, whatever the source's state now.
- The `VMClone` is **not failed**: it stays `Pending`, with `Ready=False` and
  `Cloning=False`, reason `SourceMustBePoweredOff`, and one `Warning` event
  when it starts waiting. It is re-checked with a backoff (15 s, doubling to 5
  minutes) and at once whenever the source VM's observed power state
  (`status.powerState`) changes, so it proceeds as soon as the source is off.
  On a clustered Provider the target `VirtualMachine` the clone created, and
  its pending host, are kept while it waits.
- To clone a running VM, set the source's `spec.powerState: Off`, wait for the
  clone to become `Ready`, then power the source back on.

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
  finds no dependents. The provider keeps the count by the domain's UUID
  (a VM re-created under the same name never inherits it) and forgets it when
  the domain is deleted or the provider restarts.

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
seconds up to 5 minutes). So does one whose delete cannot be sent at all while
its VM still exists — the VM's `Provider` in another namespace does not (or no
longer) allow the snapshot's namespace, a clustered VM has no confirmed host
binding, or the VM's `spec.providerRef` no longer names the `Provider` it is
bound through: the reason is `ConsumerNotAllowed`, `Unbound`,
`ProviderRefMismatch` or `PlacementTopologyMismatch`, with the same backoff.
Only a snapshot the provider reports as already gone, a `VirtualMachine` that no
longer exists, or `force-delete` (a `Warning` event `SnapshotLeftOnHypervisor`
names the snapshot left behind) releases it.

### How the dependency is found

Before the operation, the provider reads every domain defined on the host —
running or shut off, VirtRigaud's or anyone else's — and each disk's full image
chain. A running domain's definition lists its chain in `<backingStore>`, which
is used as is. A shut-off domain's chain is read one image at a time with
`qemu-img info -U` — through passwordless `sudo -n` when the host allows it (so
a disk the SSH user cannot read — a `0600 libvirt-qemu` image, a root-squashed
NFS pool — does not fail the check), and as the SSH user otherwise. Every
image — the disk itself too — must be a regular file before `qemu-img` opens
it, and a backing file is followed only when its header names an absolute
local path (opened in the format the header names); `qemu-img` is never asked
to follow a whole chain itself, so it never opens an `nbd:`, `http:` or
`json:` backing, a device or a FIFO as root. A qcow2 external data file is
recorded as part of its image (never opened by the check itself; run a QEMU
with the CVE-2024-4467 fix, whose `qemu-img info` does not open it either). A
VM "has dependents" when any other domain references one of its disk files as
a disk, a backing file, a data file, or any other file. Delete only runs the
check when it has files to remove.

The check fails closed: if a definition or a disk's image chain cannot be read
— including a chain that names a non-local backing or data file (a protocol,
`json:` or relative name), a non-regular file, or is deeper than 32 images — the operation
is not performed and returns a retryable error (`Unavailable` with
`google.rpc.ErrorInfo` reason `VM_DISK_CHECK_FAILED`, which the manager retries
and never counts toward the Provider's circuit breaker), with the details in
the provider log. A host that cannot be reached at all during the check (the
SSH connection or its libvirtd fails) is a host failure instead: `Unavailable`
with `HOST_UNAVAILABLE` on a clustered Provider, and a plain `Unavailable` —
which the circuit breaker counts — on a single-host one. Give the provider's
SSH user passwordless `sudo` for `qemu-img info -U`
(`virtrigaud ALL=(root) NOPASSWD: /usr/bin/qemu-img info -U *` — never `qemu-img *`), or membership of the group the
disks belong to (`kvm` for the disks VirtRigaud creates on Debian/Ubuntu
hosts), or run it as `root`. The clone and export copies have their own,
exact rules: see [What the copies run as root](#what-the-copies-run-as-root).

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
- the files **below them in their backing chains that are its own**: the
  overlays its external snapshots added (`<domain>-disk.<snapshot name>`),
  down to and including the disk it was created with (`<domain>-disk.qcow2`,
  or `<domain>-disk` for a blank disk) — nothing below that disk, even a file
  named like `<domain>-disk.<x>…` (another VM's disk) — meeting the same rules
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

- **Disk mode.** Every VM disk VirtRigaud creates — a new VM's disk (a blank
  one too), an image copied or downloaded for it, an imported disk, and a
  clone's disk — is **created** `0640` (owner read-write, group read-only, no
  access for others; `qemu-img` and the copy run under `umask 0137`, and a
  blank disk is made by libvirt with that mode), then given to
  `libvirt-qemu:kvm` with `sudo chown -h` — never `chmod`'ed afterwards, and a
  symbolic link swapped in for the file has only its own ownership changed,
  never its target's. It used to be world-writable (`0777`). The provider's SSH
  user reads VM disks (disk in-use checks, `GetDiskInfo`, s3/nfs disk export, a
  full clone's copy) as a member of the `kvm` group, or as `root`; the in-use
  check also uses passwordless `sudo -n qemu-img info -U` where the host
  allows it, and a full clone's copy and the s3/nfs export's flatten run
  `qemu-img convert` through passwordless `sudo -n` where the host allows it
  (see [What the copies run as root](#what-the-copies-run-as-root)) — which
  is what reads the `0600 libvirt-qemu` overlay libvirt creates for an
  external snapshot. Nothing writes a VM disk through the group. **Disks created or
  imported by an earlier release keep their mode** (an imported disk adopted in
  place is only chowned) — to close one, shut its VM off and run
  `sudo chown -h libvirt-qemu:kvm -- <disk>` and `sudo chmod 0640 -- <disk>`.
  Least privilege (`0600 libvirt-qemu`, with every read through `sudo -n`) is
  a tracked follow-up.
- **How a disk is written.** A disk (and, for an s3 import, the staged object)
  is written inside a private directory made next to its final name
  (`mktemp -d`: an unpredictable `.virtrigaud-write-*` name, mode `0700`,
  owned by the SSH user), then renamed onto the name (`mv -T`, rename(2)),
  which replaces whatever is there — a stale file, or a symbolic link planted
  at the name — and never follows it; the directory is removed whatever
  happens. Before that, a symbolic link at the disk's name — dangling or not —
  refuses the operation (`Conflict`, "is a symbolic link"), like the UEFI
  varstore below. A blank disk is created by libvirt itself (`vol-create`,
  which refuses an existing name).
- **Pool directory.** Keep the directories VM files are created in writable
  only by `root` and the provider's SSH user (for example `root:root 0755`, or
  owned by the SSH user `0755`), or sticky (`chmod +t`). Anyone else who can
  write there can still replace a finished disk, or a file libvirt later
  opens. The provider logs a `WARN` once per directory when a non-root account
  other than its SSH user can write one that is not sticky; it never refuses.
  **A `root` SSH user is not supported on a pool directory other accounts can
  write.**
- **UEFI varstore.** For a UEFI source, the clone gets its own copy of the
  source's `<nvram>` varstore, `<nvram directory>/<clone domain>_VARS.fd`. The
  clone is refused (`Conflict`) before any of its files is written when that
  path is a symbolic link or the varstore (or any other file) of an existing
  domain. An unused file left by an earlier, failed clone is first removed, and
  the copy is then created fresh: `sudo dd iflag=nofollow oflag=nofollow
  conv=excl` under `umask 0177`, so it never follows a symlink at either end,
  never writes into a file that appeared in between (the clone fails instead),
  and is `0600` from the start; it is then `chown -h`'ed to the qemu user.

## What the copies run as root

After an external (disk-only) snapshot — a `VMSnapshot`, or the snapshot a
`VMMigration` takes before it exports — a VM runs on libvirt's overlay
`<pool directory>/<domain>-disk.<snapshot>`, which libvirt creates `0600` and
owned by `libvirt-qemu`. The provider's SSH user cannot read it, even as a
member of `kvm`, so a full clone or a disk export of such a VM needs root to
read the source. The three copies that read a VM's whole disk chain — a full
clone's copy (single-host and clustered), and the flatten of an s3 or nfs
disk export — therefore run `qemu-img convert` through passwordless
`sudo -n`:

- The source is opened in the format its domain definition names (`-f qcow2`
  or `-f raw`; the exports keep their `-f qcow2`), so root never probes a
  disk's format — a guest cannot make a raw disk it wrote read as a qcow2
  image that names a host file as its backing file.
- Before root opens it, the source's image chain is read one image at a time
  with `qemu-img info -U` (through `sudo -n` too, the rule the in-use check
  already uses): every image must be a local regular file and every backing
  file must be named with its format. A chain that fails this is copied as
  the SSH user, as before.
- The copy's local output is created by the SSH user, under the copy's umask
  (`0137`: `0640` for a clone's disk; `0177`: `0600` for an export's staging
  file), inside a private `mktemp -d` directory (`.virtrigaud-write-*`, `0700`)
  next to it, **before** root writes into it: root never creates the file or
  follows a link at its name. The clone's disk is then renamed into place
  with `mv -f -T` and given to `libvirt-qemu:kvm` with `chown -h`, as before;
  an s3 export's staging file is removed with its directory.
- On a clustered Provider the copy keeps its guard: the SSH user takes the
  `flock` on the clone's (or export's) lock file **outside** `sudo`, and
  `timeout(1)` runs **inside** `sudo`, so it can stop — and after 10 s kill —
  the root `qemu-img` when the call's time budget runs out.
- An nfs export reaches the NFS server with the SSH user's uid and gid (added
  to the `nfs://` URL as libnfs `uid=`/`gid=` unless the URL names them), the
  identity it has always used — never uid 0, which a `root_squash` export
  would map to its anonymous user.
- **When `sudo` refuses** (no passwordless rule for the command, or no `sudo`
  at all), the copy runs as the SSH user, exactly as before: a disk the SSH
  user can read is copied as it always was; the overlay of a snapshotted VM
  fails with "Permission denied" (logged by the provider with a pointer to
  this page; a clustered clone reports `VM_OPERATION_FAILED`, which the
  manager never counts toward its circuit breaker).

`sudo` sees these commands (the umask shell before `sudo` runs as the SSH
user; `<N>` is the call's remaining budget in seconds, `XXXXXXXXXX` the
random part `mktemp` picks):

| Copy | Command `sudo -n` runs |
|---|---|
| Chain read (all copies; unchanged rule) | `qemu-img info -U [-f <format>] --output=json -- <image>` |
| Full clone, single-host | `qemu-img convert -f <qcow2\|raw> -O qcow2 <source disk> <pool dir>/.virtrigaud-write-XXXXXXXXXX/<clone domain>-disk.qcow2` |
| Full clone, clustered | `timeout --kill-after=10s <N>s qemu-img convert -f <qcow2\|raw> -O qcow2 <source disk> <pool dir>/.virtrigaud-write-XXXXXXXXXX/<clone domain>-disk.qcow2` |
| s3 export | `[timeout --kill-after=10s <N>s] qemu-img convert -U -f qcow2 -O qcow2 <source disk> <source dir>/.virtrigaud-write-XXXXXXXXXX/.virtrigaud-export-<vm>.qcow2` |
| nfs export | `[timeout --kill-after=10s <N>s] qemu-img convert -U -f qcow2 -O qcow2 <source disk> nfs://<server>/<path>?uid=<uid>&gid=<gid>` |

(`timeout` appears on a clustered Provider only.)

**The sudoers rule.** Allow exactly these shapes, confined to your pool
directory and your NFS export, and nothing more of `qemu-img` or `timeout` —
**never `qemu-img *`, `qemu-img convert *` or `timeout *`**: sudoers
wildcards (`*`) match across spaces, so any rule with a wildcard in these
commands lets the account add options and files of its choosing, and have
root write anywhere. Only sudo's regular-expression rules (`^...$`, **sudo
1.9.10 or later**) can confine them. For the default pool directory
`/var/lib/libvirt/images`, an SSH user `virtrigaud`, and an NFS migration
export `nfs.example.com:/exports/virtrigaud` (adjust all three, and the paths
of `qemu-img` and `timeout`):

```
# /etc/sudoers.d/virtrigaud — edit with: visudo -f /etc/sudoers.d/virtrigaud
Cmnd_Alias VR_DISK_READ = /usr/bin/qemu-img info -U *
Cmnd_Alias VR_CLONE_COPY = /usr/bin/qemu-img ^convert -f (qcow2|raw) -O qcow2 /var/lib/libvirt/images/[^/ ]+ /var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/[^/ ]+\.qcow2$, \
    /usr/bin/timeout ^--kill-after=10s [0-9]+s qemu-img convert -f (qcow2|raw) -O qcow2 /var/lib/libvirt/images/[^/ ]+ /var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/[^/ ]+\.qcow2$
Cmnd_Alias VR_EXPORT_COPY = /usr/bin/qemu-img ^convert -U -f qcow2 -O qcow2 /var/lib/libvirt/images/[^/ ]+ (/var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/\.virtrigaud-export-[^/ ]+\.qcow2|nfs://nfs\.example\.com/exports/virtrigaud/[^ ]+)$, \
    /usr/bin/timeout ^--kill-after=10s [0-9]+s qemu-img convert -U -f qcow2 -O qcow2 /var/lib/libvirt/images/[^/ ]+ (/var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/\.virtrigaud-export-[^/ ]+\.qcow2|nfs://nfs\.example\.com/exports/virtrigaud/[^ ]+)$
virtrigaud ALL=(root) NOPASSWD: VR_DISK_READ, VR_CLONE_COPY, VR_EXPORT_COPY
```

Leave out `VR_EXPORT_COPY` if the host never exports disks (no `VMMigration`
from it), and the `timeout` lines on a single-host Provider. Check the rule
with `visudo -c`, then as root with `sudo -l -U virtrigaud <the full command>`
for a real clone path — `sudo -l` prints the command when the rule allows it.
A VM disk outside the pool directory (another allowed image directory, a
subdirectory) needs its directory added to the source part of the expression;
until it is, sudo refuses and the copy runs as the SSH user.

With sudo older than 1.9.10 these commands cannot be confined, and we do not
recommend a wildcard rule. Without a rule, everything works as before except
cloning or exporting a VM whose active disk is such an overlay (a VM with an
external snapshot), which fails as it did before this release.

A note on what these rules protect: an account that manages VMs on
`qemu:///system` — which the provider's SSH user does — can already obtain
root on the host through libvirt (libvirt documents its system connection as
root-equivalent). The rules above do not change that; they keep what
VirtRigaud itself runs as root narrow, exact and auditable, so a tenant's
input can never widen it.

## Known limitations

- Powering on the **source** VM of an existing linked clone lets its guest
  write to the clone's backing file, which corrupts the clone. The provider does
  not prevent it — this is why new linked clones are disabled — but it warns:
  the source's `VirtualMachine` gets `LinkedClonesDependOnDisk=True` after it is
  started (see [What the requester sees](#what-the-requester-sees)). Keep the
  source of an existing linked clone powered off.
- The dependency check reads the whole host on every guarded operation: every
  domain's definition, and one `qemu-img info` per image in the disk chains of
  every shut-off domain, plus a file-type check per backing file (a running
  domain's chain comes from its definition). Delete skips it
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
- new VM disks are `0640 libvirt-qemu:kvm` — make sure the provider's SSH
  user is `root`, in the `kvm` group, or allowed passwordless
  `sudo qemu-img info -U` (`virtrigaud ALL=(root) NOPASSWD: /usr/bin/qemu-img info -U *`) before upgrading, or the
  dependency check fails (retryably) and Delete and snapshot operations do not
  proceed;
- Delete removes fewer files: nothing outside the pool / allowed image
  directories, no symbolic links or their targets, and no backing files other
  than the VM's own external-snapshot chain. A deleted VM whose domain is
  already gone only has `<name>-disk.qcow2` cleaned up. Check the provider log
  for `Not deleting disk` lines if disk usage grows.
