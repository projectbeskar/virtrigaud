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
`<pool directory>/<domain>-disk.qcow2`, and is left powered off: the produced
`VirtualMachine` gets `spec.powerState: Off` unless the `VMClone` sets
`spec.options.powerOn: true`, in which case it is `On` and the VirtualMachine
controller powers the clone on once it is bound. The copy
flattens the source's whole image chain, so the clone of a VM with external
snapshots is one standalone disk, with none of the source's snapshots. The
clone's disk is always **qcow2**, whatever the source's format, and the
clone's definition declares it so (`<driver type='qcow2'>`): a raw source's
clone is a qcow2 disk (its clone used to keep `type='raw'` over the qcow2
copy and did not boot).

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
  A re-check that is refused again writes nothing to the `VMClone`, and a
  waiting clone shows no `startTime` and never `Phase=Cloning`; those are set
  when the provider accepts the clone. On a clustered Provider the target
  `VirtualMachine` the clone created, and its pending host, are kept while it
  waits.
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
recorded as part of its image (never opened by the check itself; a QEMU
with the CVE-2024-4467 fix is a prerequisite, see
[What the copies run as root](#what-the-copies-run-as-root)). Each disk is
opened in the format its domain definition names (a raw disk, whose bytes are
the guest's, is never opened at all), and a backing file in the format its
parent's header names. A backing file whose format nothing names is probed by
the SSH user only, and so is everything below it. A
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
SSH user passwordless `sudo` for the exact `qemu-img info` reads the check
makes (`VR_DISK_READ` in [the sudoers rule](#what-the-copies-run-as-root):
`/usr/bin/qemu-img ^info -U -f (qcow2|raw) --output=json -- /var/lib/libvirt/images/[^/ ]+$`, sudo 1.9.10
or later — never `qemu-img *`, nor the wildcard `qemu-img info -U *` earlier
releases documented), or membership of the group the disks belong to (`kvm`
for the disks VirtRigaud creates on Debian/Ubuntu hosts), or run it as
`root`. Root only ever reads an image in a named format: an image whose
format nothing names (a backing file its parent's header gives no format for),
every image below it, and an image of another format than qcow2 or raw are
read as the SSH user. The clone and export copies have their own,
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
  check and `GetDiskInfo` (for the VM's own disk) also use passwordless `sudo -n qemu-img info -U` where the host
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
  other than its SSH user can write one that is not sticky. It also
  **refuses a clone or export copy** in that case, when the source chain has
  an image in such a directory or the copy would write below one (see
  [What the copies run as root](#what-the-copies-run-as-root)).
  **Check your pool directory before upgrading:**
  `stat -c '%a %U:%G' /var/lib/libvirt/images`. A group-writable directory
  (for example `0775`) without the sticky bit makes every full clone and
  every disk export from it fail with `FailedPrecondition` until it is fixed
  (`chmod g-w`, or `chmod +t`). **A `root` SSH user is not supported on a
  pool directory other accounts can write.**
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

- **Prerequisite: a QEMU with the fix for CVE-2024-4467** (QEMU 9.0.2,
  8.2.6 or 7.2.13 and later, or your distribution's backport). Before that
  fix, `qemu-img info` and `convert` could be made to open a file named in
  an image's external data file entry. That includes a `json:` pseudo-protocol
  name, which lets it reach any file root can read.
- **Every disk root reads is opened in the format its domain definition
  names**: the `<driver type=...>` of the disk, or raw when there is none, as
  libvirt itself opens it. This applies to the clone and export copies,
  `GetDiskInfo`, the disk in-use check and Delete's own-chain walk. The format
  is never probed. A guest owns every byte of a raw disk, so it can make the
  disk look like a qcow2 image that names another tenant's disk as its backing
  file. Because the format is never probed, that header is never read.
  - A raw disk has no chain and is never walked. It is only checked to be a
    regular file.
  - A copy (clone or export) of a disk in any other format (`vmdk`, `qed`,
    `luks`, ...) is refused before anything reads it.
  - An export of an explicit disk path must name one of the VM's own disks.
    So must `GetDiskInfo` (and the pvc export, which copies the disk it
    resolves), on a single-host Provider too: a path that is not one of the
    VM's disks is refused (`InvalidArgument`) before anything reads it.
- **Before a copy opens any image of the source chain** (the disk, then each
  backing file, one image at a time), that image must pass all of these
  checks. Any other image is not read at all:
  - it is a local regular file, not a device or a FIFO;
  - it is named with its format, qcow2 or raw;
  - it is **not a symbolic link**;
  - its directory is writable only by root and the SSH user, or is sticky.
  A chain image that has an **external data file** is refused as well, once
  its header has been read. A copy writes its output below a directory,
  either the clone's pool directory or, for an s3 export, the source's
  directory. That directory must be just as safe, or root does not write
  there. **Every such refusal ends the copy.** It is not retried as the SSH
  user, who reads every VM disk on the host through the `kvm` group: a chain
  another account could swap is no safer for that user than for root. A
  refused clone or export fails with `FailedPrecondition`, and a routed
  (clustered) one also gets the `VM_OPERATION_FAILED` reason. On either
  kind of Provider the message says what kind of problem it is (for example
  "an image of its chain is a symbolic link") but names no host path, file
  or other VM; the provider log has the details. The manager
  never counts either toward its circuit breaker.
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
- An nfs export that runs as root writes to the destination's server and
  path with **exactly** the SSH user's uid and gid as its libnfs query,
  `?uid=<uid>&gid=<gid>`. Every other libnfs option in the URL is dropped.
  This is the identity the export has always used. libnfs presents the
  process's own uid and gid by default: for root that is uid 0, which a
  `root_squash` export maps to its anonymous user and a `no_root_squash`
  export does not.
  - When the `VMMigration` names another identity
    (`spec.storage.nfs.uid`/`gid`, for example the export owner's that a
    cross-provider migration sets), the export **never runs as root**. The SSH
    user's own `qemu-img` presents that identity, as it always has, with the
    source format still pinned. A VM with an external snapshot therefore cannot
    be exported to NFS with such an identity. To export one, leave `uid`/`gid`
    unset or set them to the SSH user's.
  - `uid: 0` and `gid: 0` are refused at `Validating` for every `VMMigration`,
    whatever its providers (`NFSRootIdentityNotAllowed`). AUTH_SYS identities
    are whatever the client claims, so on an export without `root_squash` they
    would be root, able to read or overwrite every file there. Use a dedicated
    non-zero uid/gid that owns the export.
- **Only when `sudo` itself refuses** does the copy run as the SSH user. That
  means `sudo` answers with its own refusal (no passwordless rule for the
  command, a password or terminal required, or not in sudoers), or `sudo` is
  not installed. The provider recognises only `sudo`'s exit status and exact
  messages, never a line `qemu-img` printed. The fallback pins the same
  source format (`-f <qcow2|raw>`). A disk the SSH user can read is copied as
  it always was. The overlay of a snapshotted VM fails with
  "Permission denied", which the provider logs with a pointer to this page. A
  clustered clone reports this as `VM_OPERATION_FAILED`, which the manager
  never counts toward its circuit breaker. A refusal of the chain (above) is
  never followed by this fallback.

`sudo` sees these commands (the umask shell before `sudo` runs as the SSH
user; `<N>` is the call's remaining budget in seconds, `XXXXXXXXXX` the
random part `mktemp` picks):

| Copy | Command `sudo -n` runs |
|---|---|
| Chain read (in-use check and all copies), `GetDiskInfo`'s size read of a VM's own disk, an adopted imported disk's size | `qemu-img info -U -f <qcow2\|raw> --output=json -- <image>` |
| Full clone, single-host | `qemu-img convert -f <qcow2\|raw> -O qcow2 <source disk> <pool dir>/.virtrigaud-write-XXXXXXXXXX/<clone domain>-disk.qcow2` |
| Full clone, clustered | `timeout --kill-after=10s <N>s qemu-img convert -f <qcow2\|raw> -O qcow2 <source disk> <pool dir>/.virtrigaud-write-XXXXXXXXXX/<clone domain>-disk.qcow2` |
| s3 export | `[timeout --kill-after=10s <N>s] qemu-img convert -U -f qcow2 -O qcow2 <source disk> <source dir>/.virtrigaud-write-XXXXXXXXXX/.virtrigaud-export-<vm>.qcow2` |
| nfs export | `[timeout --kill-after=10s <N>s] qemu-img convert -U -f <qcow2\|raw> -O qcow2 <source disk> nfs://<server>/<path>?uid=<SSH user's uid>&gid=<SSH user's gid>` |

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
Cmnd_Alias VR_DISK_READ = /usr/bin/qemu-img ^info -U -f (qcow2|raw) --output=json -- /var/lib/libvirt/images/[^/ ]+$
Cmnd_Alias VR_CLONE_COPY = /usr/bin/qemu-img ^convert -f (qcow2|raw) -O qcow2 /var/lib/libvirt/images/[^/ ]+ /var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/[^/ ]+\.qcow2$, \
    /usr/bin/timeout ^--kill-after=10s [0-9]+s qemu-img convert -f (qcow2|raw) -O qcow2 /var/lib/libvirt/images/[^/ ]+ /var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/[^/ ]+\.qcow2$
Cmnd_Alias VR_EXPORT_COPY = /usr/bin/qemu-img ^convert -U -f (qcow2|raw) -O qcow2 /var/lib/libvirt/images/[^/ ]+ (/var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/\.virtrigaud-export-[^/ ]+\.qcow2|nfs://nfs\.example\.com/exports/virtrigaud/[^?& ]+\?uid=1001&gid=1001)$, \
    /usr/bin/timeout ^--kill-after=10s [0-9]+s qemu-img convert -U -f (qcow2|raw) -O qcow2 /var/lib/libvirt/images/[^/ ]+ (/var/lib/libvirt/images/\.virtrigaud-write-[A-Za-z0-9]{10}/\.virtrigaud-export-[^/ ]+\.qcow2|nfs://nfs\.example\.com/exports/virtrigaud/[^?& ]+\?uid=1001&gid=1001)$
virtrigaud ALL=(root) NOPASSWD: VR_DISK_READ, VR_CLONE_COPY, VR_EXPORT_COPY
```

Replace `uid=1001&gid=1001` with the SSH user's own `id -u` and `id -g`: the
nfs part then matches only the URL the provider builds for a root export.
That URL is the server and path with exactly those two parameters, so no other
identity and no other libnfs option ever reaches a root `qemu-img`. Leave out
`VR_EXPORT_COPY` if the host never exports disks (no `VMMigration` from it),
and leave out the `timeout` lines on a single-host Provider.

**What the nfs part changes on the NFS server.** A root `qemu-img` connects
through libnfs from a **reserved source port** (below 1024), which an
unprivileged process cannot bind. Linux NFS exports are `secure` by default
and accept requests only from reserved ports. That is often what kept the SSH
user's own, unprivileged `qemu-img` off an export. With this rule, the host
reaches such an export as root, from a reserved port, presenting the SSH
user's uid and gid. A `secure` export that used to turn the host away now
accepts it as that uid. So:

- **Grant the nfs part only for exports meant for VirtRigaud.** Name exactly
  that server and path in the expression, as the example does, never a
  wildcard server or a parent directory.
- Keep that export dedicated to migrations (one per trust domain), with
  `root_squash` on, so only the files the SSH user's uid may touch are
  reachable. Check the rule
with `visudo -c`, then as root with `sudo -l -U virtrigaud <the full command>`
for a real clone path — `sudo -l` prints the command when the rule allows it.
A VM disk outside the pool directory (another allowed image directory, a
subdirectory) needs its directory added to the source part of the expression;
until it is, sudo refuses and the copy runs as the SSH user.

With sudo older than 1.9.10 these commands cannot be confined, and we do not
recommend a wildcard rule. Without a rule, everything works as before except
cloning or exporting a VM whose active disk is such an overlay (a VM with an
external snapshot), which fails as it did before this release.

`VR_DISK_READ` replaces the `qemu-img info -U *` wildcard earlier releases
documented. That wildcard let the account add options and read any file's
header as root; replace it when you upgrade. With the rule above, a read of an
image outside the pool directory (another allowed image directory, a backing
file elsewhere) is refused by sudo and made as the SSH user. Add that
directory to the expression if the SSH user cannot read the images there.

**Other allowed image directories.** The disk in-use check (clone, export,
Delete) reads the image chain of *every* domain on the host, not only of the
VM it acts on, so it also reads backing files that live in your other allowed
image directories (`VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`) — for example a cloud
image another domain is layered on:
`sudo -n qemu-img info -U -f qcow2 --output=json -- /vm-pool01/noble-server-cloudimg-amd64.img`.
With the `VR_DISK_READ` rule above, sudo refuses that read and it is made as
the SSH user instead, which works only if that user can read the file; if it
cannot, the check fails closed (`VM_DISK_CHECK_FAILED`) and the clone, export
or delete waits. To let root read them, list every allowed image directory in
the rule, each with the same `[^/ ]+` confinement (one file directly inside
the directory, no subdirectory, no space), and escape any `.` in a path as
`\.`. For `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS=/var/lib/libvirt/images,/vm-pool01`:

```
Cmnd_Alias VR_DISK_READ = /usr/bin/qemu-img ^info -U -f (qcow2|raw) --output=json -- (/var/lib/libvirt/images|/vm-pool01)/[^/ ]+$
```

Never widen it to a parent directory (`/`), a subdirectory wildcard or `.*`.
The copy rules (`VR_CLONE_COPY`, `VR_EXPORT_COPY`) need a directory added only
when a VM's own disk lives there, as described above.

What these rules protect, and what they do not. With them, root runs only
the commands in the table:

- `qemu-img info` and `qemu-img convert` of images in the pool directory;
- each image opened in the format its domain definition names, or the header
  of a parent that was itself read in a named format — qcow2 or raw, never
  probed;
- output written into VirtRigaud's private `.virtrigaud-write-*` directories,
  or to the NFS export with the SSH user's own uid and gid and no other libnfs
  option.

A copy of a chain another account could swap (a symbolic link, a
group-writable pool directory) is refused before any image of it is opened.

A tenant's input chooses which of its own VMs is read and, for nfs, the name
of the staged object on the export. It does not choose the options, the
formats, the NFS identity root presents, or any other file.

These rules do not change two things:

- An account that manages VMs on `qemu:///system`, as the provider's SSH user
  does, can already obtain root on the host through libvirt (libvirt
  documents its system connection as root-equivalent). The rules keep what
  VirtRigaud itself runs as root narrow, exact and auditable. They do not
  contain an attacker who controls that account.
- The regular expressions confine the command line, not the files behind it.
  Anyone who can write the pool directory or the NFS export can still change
  what is copied: keep them writable only by root and the SSH user (see
  [Pool directory](#clone-files-on-the-host)).

## Troubleshooting

### A clone or export fails: "the source VM's disk cannot be copied safely"

**Symptom.** A `VMClone` or a `VMMigration` from a libvirt Provider fails with
`FailedPrecondition`, or `VM_OPERATION_FAILED` on a clustered Provider, and a
message such as:

```
the source VM's disk cannot be copied safely: an image of its chain is in a directory other accounts can write (details are in the provider log)
```

The message never names a host path. The provider log has the full reason,
on a line that starts with `WARN Refusing to copy <disk>:`.

| The message says | Cause | Fix |
|---|---|---|
| an image of its chain is in a directory other accounts can write | The pool directory, or the directory of a backing file, is group- or world-writable and not sticky | `chmod g-w,o-w <dir>`, or `chmod +t <dir>` |
| the directory it would be written to can be written by other accounts | The same, for the directory the copy writes below: the clone's pool directory, or the source's directory for an s3 export | As above |
| an image of its chain could not be checked / the directory it would be written to could not be checked | The SSH user could not `stat` the directory | Let the SSH user search the directory (`x`) |
| an image of its chain is a symbolic link | The disk or a backing file is a symbolic link | Replace the link with the file it points to, with the VM off |
| an image of its chain has an external data file | A qcow2 in the chain stores its data in a separate file | Flatten the image (`qemu-img convert`) with the VM off |
| its disk format "…" is not qcow2 or raw | The domain's `<driver type>` is another format | Convert the disk to qcow2 and update the definition |
| its image chain names a backing file without its format | A header names its backing file with no format | `qemu-img rebase -u -F <format> -b <backing> <image>`, with the VM off |
| its image chain could not be read and verified | The chain could not be read, for example a `0600` overlay without the sudo rule | Add `VR_DISK_READ` ([The sudoers rule](#what-the-copies-run-as-root)) |

**A directory others can write is named at startup.** Every refusal is
logged, and the directory is also named before any copy is attempted:

- A **single-host** Provider checks every directory VM disks live in (the
  allowed image directories and the default pool's directory) when it starts.
- A **clustered** Provider checks each host's pool directory on first use.

For each one that another account can write and that is not sticky, the
provider logs, once per directory:

```
WARN /var/lib/libvirt/images, where VM disks are created, is writable by its group and is not sticky: ... Full clones and disk exports of VMs whose disks are there, and copies written below it, are REFUSED (FailedPrecondition). ...
```

Check with `stat -c '%a %U:%G' <dir>`. A sticky directory (for example mode
`1777` or `3777`) is accepted.

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
  `sudo qemu-img info` for the exact reads the check makes (`VR_DISK_READ` in
  [the sudoers rule](#what-the-copies-run-as-root); a host that allowed
  `qemu-img info -U *` for an earlier release should replace that wildcard)
  before upgrading, or the dependency check fails (retryably) and Delete and
  snapshot operations do not proceed;
- Delete removes fewer files: nothing outside the pool / allowed image
  directories, no symbolic links or their targets, and no backing files other
  than the VM's own external-snapshot chain. A deleted VM whose domain is
  already gone only has `<name>-disk.qcow2` cleaned up. Check the provider log
  for `Not deleting disk` lines if disk usage grows.
