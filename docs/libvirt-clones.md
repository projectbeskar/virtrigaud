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

The clone is **stamped with its target VirtualMachine's identity** (uid
included), which the manager creates before the clone and sends in
`CloneRequest.target_vm`. A `Clone` sent again — the manager restarted, or its
answer or record was lost — is answered with the domain an earlier attempt
defined when that domain carries the target's stamp, before the source's state
is read; nothing is copied again. A domain of the clone's name that does not
carry it is refused with `AlreadyExists` and never redefined. See
[VM ownership: Clones: re-bind](vm-ownership.md#clones-re-bind) and
[`libvirt-domain-ownership.md`](libvirt-domain-ownership.md) (rule 6).

## A clone's cloud-init identity

A full clone copies its source's disk, and with it cloud-init's record of the
instance it last booted as. If the clone's cloud-init seed still presented the
source's `instance-id`, cloud-init would treat the clone's first boot as a
reboot of the **same** instance and skip its per-instance modules: the clone
would keep the source's **SSH host keys** and hostname — two VMs answering with
one host key, which defeats SSH host-key verification.

When the source boots from a cloud-init NoCloud seed ISO, the clone therefore
gets a **seed of its own**, on single-host and clustered Providers alike:

| | What the clone's seed holds |
|---|---|
| `instance-id` | `virtrigaud-<target VirtualMachine uid>` — a new instance, and the same value whenever a `Clone` for that VirtualMachine is sent again, so a retried clone writes an identical seed. (A manager older than rc2 sends no uid; the clone's domain name is used instead.) |
| `local-hostname` | `spec.customization.hostname` of the `VMClone` when set, else the target VirtualMachine's name; with `spec.customization.domain`, the FQDN `<hostname>.<domain>` |
| user-data | `spec.customization.userData.cloudInit.inline` when set (used as given); otherwise the **source's** user-data, with its top-level `hostname:` and `fqdn:` keys (block style, one-line values, in a `#cloud-config` document) rewritten to the clone's — cloud-init prefers those keys over `local-hostname`. Any other format (a shell script, MIME multi-part, a Jinja template) is kept unchanged. |
| other meta-data | the source's, unchanged (network configuration included) |
| `network-config`, `vendor-data` | the source seed's, unchanged, when it has them |

On the clone's first boot cloud-init sees a new `instance-id` and runs its
per-instance modules: the `ssh` module deletes and regenerates the host keys
(`ssh_deletekeys` defaults to `true`), the hostname modules set the clone's
name, and every other per-instance module of the user-data — `runcmd`,
`packages`, `users`, `write_files` — **runs again** on the clone, as it would
on any new instance.

What a new instance does **not** change:

- **`/etc/machine-id`.** cloud-init does not regenerate it. A clone of a VM
  whose machine-id is populated keeps it — and with it anything derived from
  it, such as systemd-networkd's DHCP client identifier. Seal the source (empty
  `/etc/machine-id`, for example with `cloud-init clean --machine-id` before
  the source is shut down for cloning) if clones must have their own.
- **Static network configuration** in the source's meta-data or
  `network-config` is carried as it is: a clone of a VM with a static address
  claims the same address.
- **Customization fields other than** `hostname`, `domain` and
  `userData.cloudInit.inline` (`networks`, `timeZone`, `guestCommands`,
  `sysprep`, `certificates`, `tags`, `userData.ignition`) are not applied by
  the libvirt provider and are logged as such. User-data given by
  `userData.cloudInit.secretRef` cannot be resolved by the provider: the clone
  keeps the source's user-data, and the provider logs it.
- **User-data that pins the identity.** Some `#cloud-config` keys in the
  clone's user-data (the source's, or the customization's) undo the new
  instance, and the provider logs a `WARN` naming them (never their values):
  - `ssh_keys` writes fixed SSH host keys on every new instance, so the
    clone gets the source's;
  - `ssh_deletekeys: false` keeps the host keys already on the disk;
  - `preserve_hostname: true` never sets the hostname;
  - `manual_cache_clean: true` makes cloud-init skip the instance-id check,
    so no per-instance module runs again.

  Remove them from the source's user-data, or give the clone its own with
  `spec.customization.userData.cloudInit.inline`, if clones must have their
  own host identity.

> **Security: creating a `VMClone` is read access to the source VM's data.**
> On libvirt, `spec.customization.userData.cloudInit.inline` becomes the
> clone's user-data, and cloud-init runs it as **root** on the clone's first
> boot. The clone's disk is a full copy of the source's disk. Anyone who can
> `create` a `VMClone` of a VM can therefore run any command as root on a copy
> of that VM's disk, and read everything on it: files, credentials, keys. The
> source's own user-data, which may carry secrets, is copied into the clone's
> seed as well. Grant `create` on `vmclones` in a namespace only to subjects
> trusted with read access to every VM's data in that namespace. The same holds
> for any clone of a cloud-init VM, on every provider: the clone boots the
> source's user-data again.

The source's seed is read on the host with `isoinfo` (shipped with
`genisoimage`, which the provider already needs) **before anything is
copied**, as the provider's SSH user. Once the source has started, libvirt has
given its `cloud-init.iso` (`0600`) to the qemu user, so the read goes to the SSH
user's private copy next to it, `cloud-init.private.iso` (R27; see
[Cloud-init seed directory](libvirt-domain-ownership.md#cloud-init-seed-directory));
the provider never reads a seed as root. The host's seed directory is checked
before that read: an unusable one fails the clone, retryably, with
`refusing to stage cloud-init data`, before anything is read or copied. The
clone's own seed is built in that seed directory and sealed like a create's
(ISO and private copy `0600`, no `user-data`, `meta-data`, `network-config` or
`vendor-data` left next to them, directory `0711`). A seed that cannot be read fails the clone with nothing written —
retried when the read failed (an unreachable host, a missing `isoinfo`), and
refused (`InvalidArgument`) when the ISO holds no NoCloud `user-data` or
`meta-data`, holds a file over 1 MiB, or has meta-data that is not a YAML or
JSON mapping. A failed read never puts what was read before the failure in an
error or a log line. The customization's inline user-data is bounded to 1 MiB
too (`InvalidArgument`). If the clone's own seed cannot be made after the
copy, nothing was defined and the clone is retried (`Unavailable`). A source
without a seed ISO gives a clone without one, as before (its hostname and
user-data customization are then not applied, and logged).

The `#cloud-config` hostname rewrite only changes a line that the YAML parser
confirms is the top-level `hostname:` or `fqdn:` key, with its whole value on
that line. A look-alike line inside a multi-line quoted value or flow
collection is never touched, and user-data that does not parse is left as it
is (logged). Meta-data whose `instance-id` or hostname value carries a YAML
anchor is refused (`InvalidArgument`): replacing it would break an alias that
names it.

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
the SSH user only, and so is everything below it. A shut-off domain's
definition can list its chain in nested `<backingStore>` elements (libvirt
keeps them there after an offline external snapshot or a revert): such a
backing store is not opened again when its disk's chain already reached it,
and only one that no chain reached is probed, by the SSH user only. A
VM "has dependents" when any other domain references one of its disk files as
a disk, a backing file, a data file, or any other file. Delete only runs the
check when it has files to remove.

The check fails closed: if a definition or a disk's image chain cannot be read
— including a chain that names a non-local backing or data file (a protocol,
`json:` or relative name), a non-regular file, or is deeper than 32 images — the operation
is not performed and returns a retryable error (`Unavailable` with
`google.rpc.ErrorInfo` reason `VM_DISK_CHECK_FAILED`, which the manager retries
and never counts toward the Provider's circuit breaker), with the details in
the provider log. When it failed because an image cannot be read at all
(`Permission denied`), the answer says so instead of "transient host error",
and the provider log names the image: see
[Unreadable disk images](#unreadable-disk-images). A host that cannot be reached at all during the check (the
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
- its **VirtRigaud cloud-init seed directory**
  (`<seed dir>/virtrigaud-cloudinit-<domain>.<random>/`, default seed directory
  `/var/lib/libvirt/virtrigaud-seeds`; or an earlier release's
  `/tmp/virtrigaud-cloudinit-<domain>.<random>/` or
  `/tmp/virtrigaud-cloudinit/<domain>/`) — unless another domain still
  references a file in it (a domain whose CD-ROM file is missing no longer
  starts). A clone has its own seed directory, named after the clone's domain,
  so deleting a source never touches its clones' seeds; only a **single-host
  clone made by an earlier release** still points its CD-ROM at its source's
  seed ISO, which is then kept until the last domain referencing it is deleted.

It never removes any other backing file, cdrom or floppy media, or any file
outside those directories: such a file is left in place and logged by the
provider.

### What SnapshotDelete removes

`virsh snapshot-delete` of VirtRigaud's external (disk-only) snapshots
commits the overlay back into its base and pivots the domain onto the base,
but libvirt (10, Ubuntu 24.04) **leaves the overlay file** —
`<domain>-disk.<snapshot name>` — in the pool, and Delete no longer finds it
(it is not in the domain's chain any more). `SnapshotDelete` therefore reads
the snapshot's overlay files (`virsh snapshot-dumpxml`) before deleting it and,
once the delete succeeded, removes each one only when **all** of these hold:

- it is the domain's own primary-disk overlay, `<domain>-disk.<snapshot name>`,
  exactly as the snapshot names it — or, after a revert to that snapshot,
  `<domain>-disk.<epoch>`: libvirt (9.9 and later) reverts an external
  snapshot by creating a new overlay named after the disk with its extension
  replaced by the revert's time in Unix seconds, and records that overlay as
  the snapshot's from then on (a snapshot with child snapshots records it in
  `<revertDisks>`, which is read too);
- it is in no backing chain of the domain's current disks (read with
  `qemu-img info`; a chain that cannot be read keeps the file);
- no other snapshot of the domain references it, in its disk list or in the
  domain definition it recorded;
- it is a regular file (not a symbolic link) directly inside the default
  storage pool's directory or an allowed image directory — the same rules as
  Delete;
- no other domain on the host uses it, and on a clustered Provider no domain
  on **any other host** of the Provider does (the cluster-wide disk guard).

Anything that cannot be established keeps the file. A failure to remove it is
logged by the provider (no path reaches a status) and never fails the
`SnapshotDelete`: the snapshot itself is gone. Overlays of additional data
disks (`<domain>-disk-<n>.<snapshot name>`) and overlays left by snapshots
deleted before this release are not removed. `SnapshotRevert` does not remove
files itself; libvirt's revert removes the overlay it replaces. A revert's
`<domain>-disk.<epoch>` overlay that is still the VM's disk (or in its chain)
is removed with the VM, as any other own overlay
([Reverting a VirtualMachine to a VMSnapshot](vm-snapshot-revert.md#libvirt)).

A single-host Provider deleting a VM whose domain no longer exists still cleans
up the disk VirtRigaud named after it, `<name>-disk.qcow2` in the default pool's
directory, but only if it exists and no remaining domain uses it — a linked
clone of the vanished VM keeps its backing file. Nothing else is looked for.

### Deleting memory and disk-only snapshots of the same VM

A `VMSnapshot` with `includeMemory: true` of a running VM is an **internal** libvirt
snapshot: libvirt stores the guest's memory and the disks' state inside the qcow2 file
each disk is using at that moment. A VM that already has a disk-only snapshot runs on
that snapshot's overlay (`<domain>-disk.<snapshot name>`), so a memory snapshot taken
after it is stored **inside that overlay**. Deleting the disk-only snapshot makes libvirt
commit the overlay into its base and move the VM off it. In the lab where this was found
(libvirt 10, a running VM), libvirt also removed the overlay file, and the memory
snapshot's data went with it. libvirt (checked against 10.0) does not refuse this
delete: it refuses an internal snapshot that is the disk-only snapshot's *parent*, but
not one stored in the file the delete merges away, and it keeps the memory snapshot's
metadata, which still names the removed file. v0.4.0-rc8 and earlier sent that delete
and reported success.

**The delete is refused before anything changes.** Since v0.4.0-rc9, `SnapshotDelete`
of a disk-only snapshot is refused while another snapshot of the VM is stored in a file
the delete may merge away or remove. For each of the snapshot's disks these files are
the snapshot's overlay (and, after a revert to it, the revert's overlay), the disk file
each child snapshot recorded, and the disk file the snapshot itself recorded. A snapshot
is stored in such a file when either:

- libvirt's metadata says so: another snapshot of the domain recorded one of its
  internal disks on that file; or
- the file's own header says so: `qemu-img info -U -f qcow2 --output=json` of the file
  lists an internal snapshot. This is the same read, through the `VR_DISK_READ` sudo
  rule, as the disk in-use check ([What the copies run as root](#what-the-copies-run-as-root)).
  A file that does not exist holds nothing.

The `VMSnapshot` keeps its finalizer and stays `Deleting`, with
`Ready=False/DeleteBlocked` and a `SnapshotDeleteFailed` Warning event. The message
carries the provider's answer, which names the snapshots to delete first by name, for
example:

```text
Provider refused to delete the snapshot: snapshotDelete failed: failed to delete snapshot: delete refused, nothing changed: delete snapshot "mem" first; it is stored in a disk file that deleting snapshot "live" would merge away, and would be lost. The VMSnapshot is kept and the delete is re-checked with a backoff of up to 5m0s; ...
```

The manager keeps the first 256 bytes of the provider's answer, so the snapshot names
come first; it names up to three and counts the rest. The delete is re-checked with a
backoff from 15 seconds up to 5 minutes. A re-check of the same refusal writes nothing.

**Delete in this order:** first the memory snapshots taken after the disk-only one, then
the disk-only snapshot. libvirt deletes a memory snapshot of a running VM only while the
VM still runs on the file the snapshot is stored in, as it does in this case. A snapshot
whose delete does not touch a file holding another snapshot is deleted as before. For
example, a disk-only snapshot followed by a second disk-only snapshot and then a memory
snapshot: the first disk-only snapshot can be deleted, because the memory snapshot is
in the second one's overlay. The second disk-only snapshot is refused until the memory
snapshot is deleted.

When the check cannot be completed (a snapshot, the snapshot list or a file's header
cannot be read, or a disk-only snapshot's overlay is not a file), nothing is deleted.
The answer is retryable (`VM_DISK_CHECK_FAILED`): `snapshot delete of libvirt domain
"<domain>" not performed: could not verify that deleting the snapshot keeps the VM's
other snapshots`. A header the provider may not read gives "permission denied reading a
disk image": see [Unreadable disk images](#unreadable-disk-images).

`virtrigaud.io/force-delete=true` on the `VMSnapshot` still removes it. Nothing is
deleted on the host; the snapshot stays on the hypervisor.

#### A memory snapshot whose data was already lost

On a VM where v0.4.0-rc8 or earlier deleted the disk-only snapshot first, the memory
snapshot's metadata remains and records a file that no longer exists. While the VM
runs, libvirt refuses to delete the snapshot (`disk image 'vda' for internal snapshot
'mem' is not the same as disk image currently used by VM`), and its `VMSnapshot` stayed
`Deleting`.

Since v0.4.0-rc9, when libvirt refuses a delete with that error, the provider deletes
only the snapshot's metadata (`virsh snapshot-delete <domain> <snapshot> --metadata`,
which touches no disk file), but only when it can prove that the data is gone. Both of
these must hold:

- every file the snapshot recorded for its internal disks no longer exists (`qemu-img`
  answers "No such file or directory"); and
- no image in the backing chains of the VM's current disks lists a snapshot of that
  name.

Otherwise it refuses, and nothing changes:

```text
delete refused, nothing changed: power the VM off and delete it again; libvirt cannot delete snapshot "mem" while the VM runs on a disk file other than the one it is stored in
```

With the VM powered off, libvirt deletes an internal snapshot from the files it
recorded (`qemu-img snapshot -d`), without comparing them with the VM's current disk.

**Manual cleanup.** If a VM is still on v0.4.0-rc8 or earlier, or its `VMSnapshot` was
already removed with force-delete, clear the leftover metadata by hand. You need the
provider's SSH user (or root) on the VM's host. Set the connection as in the
[re-attach runbook](clustered-restore.md#re-attach-runbook). Delete only the metadata,
and only after you have checked that the data is gone:

1. Read the snapshot. It must show `<memory snapshot='internal'/>`. Note the
   `<source file=...>` of each disk in its recorded `<domain>` whose entry in `<disks>`
   is `snapshot='internal'`:

   ```sh
   virsh -c "$CONN" snapshot-dumpxml <domain> <snapshot>
   ```

2. Check each of those files. The data is gone only if `qemu-img` reports that the file
   does not exist:

   ```sh
   sudo qemu-img snapshot -l -U <recorded file>
   # qemu-img: Could not open '<recorded file>': ... No such file or directory
   ```

   If it lists the snapshot, stop here: the data is still there. Power the VM off and
   delete the `VMSnapshot` again.

3. Check that none of the VM's current disk images lists the snapshot. The output must
   not show `<snapshot>` under any image's "Snapshot list":

   ```sh
   virsh -c "$CONN" domblklist --details <domain>
   sudo qemu-img info -U --backing-chain <each disk file>
   ```

4. Only then delete the metadata:

   ```sh
   virsh -c "$CONN" snapshot-delete <domain> <snapshot> --metadata
   ```

5. If the `VMSnapshot` still exists, its next re-check finds no snapshot on the host
   and removes the finalizer.

## Clone files on the host

- **Cloud-init seed.** A clone of a VM with a seed ISO gets its own seed
  directory, `<staging>/virtrigaud-cloudinit-<clone domain>.<random>/`, made
  exactly like a created VM's (see
  [A clone's cloud-init identity](#a-clones-cloud-init-identity)), and its
  CD-ROM points there. It is removed with the clone, or when the clone's
  define fails and the domain provably does not exist.
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
- **Snapshot overlays.** The overlays libvirt itself creates for an external
  snapshot (`<domain>-disk.<snapshot>`) and for a revert
  (`<domain>-disk.<epoch>`) are `0600`, owned by root while their VM is off.
  The provider does not change them; the disk in-use checks read them as
  root through `VR_DISK_READ` (see
  [Unreadable disk images](#unreadable-disk-images)).
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
the directory, no subdirectory, no space). The rule is a regular expression:
escape **every** regular-expression metacharacter in a directory path with a
backslash — `.` `+` `*` `?` `(` `)` `[` `]` `{` `}` `|` `^` `$` and `\`
itself (for example `/srv/vm.pool+1` becomes `/srv/vm\.pool\+1`), or the rule
matches more paths than you meant. For
`VIRTRIGAUD_LIBVIRT_IMAGE_DIRS=/var/lib/libvirt/images,/vm-pool01`:

```
Cmnd_Alias VR_DISK_READ = /usr/bin/qemu-img ^info -U -f (qcow2|raw) --output=json -- (/var/lib/libvirt/images|/vm-pool01)/[^/ ]+$
```

Never widen it to a parent directory (`/`), a subdirectory wildcard or `.*`.
The copy rules (`VR_CLONE_COPY`, `VR_EXPORT_COPY`) need a directory added only
when a VM's own disk lives there, as described above.

sudo matches the path as written, and `qemu-img info` follows a **symbolic
link** inside an allowed directory to wherever it points. Such a read never
returns the target's contents: a root `qemu-img info` in a pinned format
reveals only the target's image metadata — its size and format and, for a
qcow2 image, header fields such as its backing file name and internal
snapshot names. (The copies refuse symbolic links outright, see above.) Keep
every allowed image directory writable only by root and the SSH user, so no
other account can place a link there.

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
| its image chain could not be read and verified | The chain could not be read, for example a `0600` overlay without the sudo rule | Add `VR_DISK_READ` ([The sudoers rule](#what-the-copies-run-as-root)); see also [Unreadable disk images](#unreadable-disk-images) |

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

### Unreadable disk images

**Symptom.** A snapshot create, revert or delete, or a delete of a VM (on a
clustered Provider also a create or clone), is not performed, and its answer
reads:

```
snapshot create of libvirt domain "<namespace>.<name>" not performed: permission denied reading a disk image on its host (see docs/libvirt-clones.md#unreadable-disk-images; the provider log names it), so no other domain's use of its disks can be ruled out; retried
```

(`on a host of this Provider` when the cluster-wide check failed on another
host.) It is `Unavailable` with the `VM_DISK_CHECK_FAILED` reason, which the
manager never counts toward the Provider's circuit breaker and handles as
for any check that could not run ([How the dependency is found](#how-the-dependency-is-found)):
a VM delete is retried, and a `VMSnapshot` on a clustered Provider shows
`Ready=False/ProviderError` with the answer in its `SnapshotCreateRetrying`
event and is sent again (after 1 minute, doubling up to 10 minutes) until
the image is fixed. The answer
never names the image: it can be another tenant's VM's. The provider log
does:

```
ERROR libvirt image confinement: read backing chain: permission denied reading disk image <path> (see docs/libvirt-clones.md#unreadable-disk-images): ...
```

**Cause.** The disk in-use check reads the disk chain of every domain on the
host, the VM's own and every other one, one image at a time. It reads an
image as root only through `sudo -n qemu-img info -U -f <qcow2|raw>
--output=json -- <image>`, the shape the `VR_DISK_READ` rule
([the sudoers rule](#what-the-copies-run-as-root)) allows, and as the SSH
user when sudo refuses. Neither could read this one. That is usually an
overlay libvirt created for an external snapshot
(`<pool dir>/<domain>-disk.<snapshot>`) or for a revert
(`<pool dir>/<domain>-disk.<epoch>`). libvirt creates these `0600`, owned by
root while their VM is off and by `libvirt-qemu` while it runs, so the SSH
user cannot read them even as a member of `kvm`. Every snapshot and every
revert on libvirt leaves one. The provider does not change their owner or
mode.

**Fix: grant `VR_DISK_READ` over the pool directory.** This is the
recommended fix. The rule lets root read only the metadata of an image in a
named format, one file directly inside the directories it lists:

```
Cmnd_Alias VR_DISK_READ = /usr/bin/qemu-img ^info -U -f (qcow2|raw) --output=json -- /var/lib/libvirt/images/[^/ ]+$
virtrigaud ALL=(root) NOPASSWD: VR_DISK_READ
```

List every directory VM disks live in, as described in
[Other allowed image directories](#what-the-copies-run-as-root) (sudo 1.9.10
or later; escape regular-expression metacharacters). Check it with
`sudo -l -U <ssh user> /usr/bin/qemu-img info -U -f qcow2 --output=json -- <file>`.

**A pool on NFS.** Root reads through `VR_DISK_READ` reach a file another
account owns only when the export does not squash root (`no_root_squash`).
With `root_squash`, root is mapped to the anonymous user and the read fails
with `Permission denied` like the SSH user's. An export that does not squash
root gives root on every client root on the export: dedicate it to the
Provider's hosts, and do not mount it anywhere else.

**Alternative: repair the files by hand.** Where the rule cannot be granted,
give each overlay the owner and mode of a VM disk. Find the files the
provider log names, or list the VM overlays without group read, as root on
each host:

```
find /var/lib/libvirt/images -maxdepth 1 -type f -name '*-disk.*' ! -name '*.qcow2' ! -perm -g=r -printf '%m %u:%g %p\n'
```

Then, for each file whose VM is **off**, check that it is a regular file
(`stat -c %F -- <file>` prints `regular file`, not `symbolic link`), and run:

```
sudo chown -h libvirt-qemu:kvm -- <file>
sudo chmod 0640 -- <file>
stat -c '%a %U:%G %n' -- <file>    # 640 libvirt-qemu:kvm <file>
```

Run the commands on the host where the file is: on shared storage, run them
once. Every later snapshot or revert creates a new `0600` overlay, which needs
the same repair, and libvirt may give an overlay back to root when its VM
stops; the `VR_DISK_READ` rule avoids both.

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
- On a clustered Provider, snapshots are routed to the VM's host (ADR-0007
  Addendum A, slice 3), and the routed calls keep the same dependency check on
  that host.

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
