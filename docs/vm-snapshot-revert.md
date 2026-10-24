# Reverting a VirtualMachine to a VMSnapshot (`spec.snapshot.revertToRef`)

`spec.snapshot.revertToRef` on a `VirtualMachine` names a `VMSnapshot` to revert the
VM to. Up to v0.4.0-rc5 the field was accepted and **ignored**: no controller sent the
provider's `SnapshotRevert`, the VM stayed `Ready`, and no condition or event said that
nothing happened. Since v0.4.0-rc6 the `VirtualMachine` controller acts on it,
under the rules below.

> This flow is covered by the controller's unit and envtest tests, which drive a
> scripted provider fake (not the mock provider binary, nor the gRPC transport). It has
> not been validated on a real vSphere, libvirt or Proxmox host yet.

## How to revert

1. Power the VM off. VirtRigaud never powers a VM off for a revert:

   ```bash
   kubectl patch vm web-server-01 --type merge -p '{"spec":{"powerState":"Off"}}'
   ```

2. Name the `VMSnapshot`. It must be `Ready` and taken of this VM:

   ```bash
   kubectl patch vm web-server-01 --type merge \
     -p '{"spec":{"snapshot":{"revertToRef":{"name":"web-server-01-pre-update"}}}}'
   ```

3. Watch the `SnapshotReverted` condition and `status.snapshotRevert`:

   ```bash
   kubectl get vm web-server-01 \
     -o jsonpath='{.status.snapshotRevert.phase}{"\n"}{.status.conditions[?(@.type=="SnapshotReverted")].message}{"\n"}'
   ```

   It ends `Succeeded` (`SnapshotReverted=True`, reason `Reverted`) or `Failed`.

4. Power the VM on again, if you want it running. The revert does not do that:

   ```bash
   kubectl patch vm web-server-01 --type merge -p '{"spec":{"powerState":"On"}}'
   ```

To revert to the same snapshot **again**, remove the field, wait for
`status.snapshotRevert` to disappear, then set it again:

```bash
kubectl patch vm web-server-01 --type json -p '[{"op":"remove","path":"/spec/snapshot/revertToRef"}]'
```

You can leave the field set after the revert. It does nothing more while it names the
same `VMSnapshot` (see [GitOps](#gitops)).

## The rules

### The VM must be powered off

The revert is sent only when the provider reports the VM `Off` **and**
`spec.powerState` is `Off` or `OffGraceful`. Otherwise nothing is sent, and the
`SnapshotReverted` condition is `False` with reason `WaitingForPowerOff` and a message
asking for `spec.powerState: Off`. This holds when `spec.powerState` is `On` and the VM
happens to be off, too: the controller would power it on.

- The controller never powers a VM off for a revert. A VM whose spec says `Off` but is
  still running is powered off because its spec says so, as for any VM, and is reverted
  on a later reconcile, once a fresh `Describe` reports it `Off`.
- After the revert, the VM's power follows `spec.powerState` as usual. Reverting to a
  snapshot that includes memory can leave the VM running: the vSphere provider asks
  vCenter for that explicitly (see [Providers](#providers)), and other hypervisors can
  restore a memory snapshot's running state too. A VM whose spec still says `Off` is
  then powered off again.
- A VM the provider reports `Suspended` or `Unknown` is not `Off`: it waits.
- A revert requested while the VM runs stays requested. It is sent the next time the
  VM is powered off with `spec.powerState: Off`, however much later that is. To cancel
  it, remove `spec.snapshot.revertToRef`.

Why: a revert discards the VM's current disk state. Requiring it to be off keeps the
rule that VirtRigaud never powers off a VM nobody asked to power off, and it makes a
revert that is sent again (after a lost answer) safe: nothing changes the disks of a VM
that stays off between the two attempts. A VM seen running, or whose spec asks for it to
run, after an attempt whose outcome is unknown is therefore never reverted again for
that request (see [Asynchronous reverts and unknown outcomes](#asynchronous-reverts-and-unknown-outcomes)).

### The VMSnapshot is resolved fail-closed

`revertToRef.name` names a `VMSnapshot` in the VM's namespace. The revert is sent only
when all of these hold; otherwise nothing is sent and the condition says why:

| Check | Reason when it fails |
|---|---|
| The `VMSnapshot` exists | `SnapshotNotFound` |
| Its `spec.vmRef.name` is this VM | `SnapshotForAnotherVM` |
| It is not being deleted | `SnapshotNotReady` |
| Its `status.phase` is `Ready` | `SnapshotNotReady` |
| It carries the provider's snapshot id (`status.snapshotID`, the id the provider's `SnapshotCreate` answered) | `SnapshotNotReady` |
| It was taken of this VM: its `status.vmUID` is this VM's UID. A `VMSnapshot` taken by an older manager (no `status.vmUID`) counts only when its timestamps prove it, as for its delete | `SnapshotForAnotherVM` |
| It was taken through this VM's Provider (`status.boundProvider`: namespace and name, and UID when both are recorded). A `VMSnapshot` that records no Provider (some taken by an older manager) is not refused on this check; the VM UID check above still applies | `SnapshotForAnotherVM` |
| The Provider does not report `supportsSnapshots: false` (`status.reportedCapabilities`); a Provider that has not reported capabilities is not refused here | `Unsupported` |

These are re-checked every 30 seconds (the `VirtualMachine` controller does not watch
`VMSnapshot`s), and at once on any change to the VM's spec. A `VirtualMachine` deleted
and re-created under the same name is another VM (another UID): the old VM's snapshots
are refused with `SnapshotForAnotherVM`.

### At most once per request

`status.snapshotRevert` records the request: the `VMSnapshot`'s name and UID, the
phase, and the outcome.

- While it records `Succeeded` or `Failed` for the `VMSnapshot` the field names (same
  UID), nothing is sent again: not on later reconciles, not when another spec field
  changes, not when the same manifest is applied again. Deleting that `VMSnapshot`
  afterwards does not undo or repeat anything.
- Removing `spec.snapshot.revertToRef` clears the record and the condition. Setting it
  again is a new request.
- Naming another `VMSnapshot`, or a `VMSnapshot` deleted and re-created under the same
  name (another UID), is a new request. The record keeps only the latest request: naming
  snapshot A, then B, then A again reverts to A twice.
- A `Failed` revert is not retried on its own. Remove the field and set it again —
  unless the provider refused it for good (`FailedPrecondition`, for example libvirt's
  refusal of a snapshot it cannot revert safely): the same revert is refused again, so
  the message ends with the provider's guidance instead of the retry hint.

### Recorded before it is sent

Before the call, the controller writes `status.snapshotRevert` with phase `Reverting`,
`attempts` and `attemptTime`, with the optimistic lock of the `resourceVersion` it read.
If that write fails (a conflict: the cached VM was not the stored one), nothing is sent,
so a stale cache never sends a second revert. If the installed `VirtualMachine` CRD
does not store `status.snapshotRevert` (CRDs older than the manager), nothing is sent
and the condition is `Unsupported` with a message asking for the CRD upgrade.

The provider's answer is written the same way. If that write loses a `resourceVersion`
race (the VM's spec or metadata changed while the call ran, for example
`spec.powerState: On` patched right after `revertToRef`), the controller re-reads the VM
and writes the record and the condition over the stored status, as long as the stored
record is still the same attempt. Only if that also fails does the answer stay
unrecorded, and the outcome is then treated as unknown (below).

### Asynchronous reverts and unknown outcomes

- **A task.** A provider that answers with a task (Proxmox, the mock provider) has it
  recorded in `status.snapshotRevert.taskRef` and polled every 10 seconds. While it
  runs, nothing else is done to the VM: it is not powered on, resized or reverted again,
  even if its spec changes, and removing `revertToRef` clears the record only once the
  task has ended. A task that ends in failure fails the revert. So does a task the
  provider reports, in its task-status answer, that it does not know (Proxmox and the
  mock provider answer that way): fail-closed, never re-sent. A task-status call that
  fails with `NotFound` forgets the task and treats its outcome as unknown. A task that
  has not completed 2 hours after the attempt, or whose status cannot be read for that
  long, is `Failed` with its outcome unknown, so the VM is managed again.
- **An unknown outcome.** A call that fails without a definite answer (the provider
  not reached or not answering in time, a retryable answer, a clustered host that
  stopped answering after the command was sent — `HOST_CONNECTION_LOST` —, or a manager
  restart between the call and the write of its answer) leaves the record `Reverting`
  with no task. The revert is then sent again, because a VM that stayed off reaches the
  same state with a second revert:
  - no sooner than 6 minutes after the last attempt, doubling with each attempt up to 30
    minutes (the manager's own deadline on `SnapshotRevert` is 5 minutes, so the earlier
    call has ended, on the manager's side at least);
  - only if a fresh `Describe` reports the VM `Off` and `spec.powerState` is still `Off`
    or `OffGraceful`. If the VM is seen in any other state, or its spec asks for it to
    run, it may have run since the attempt: the revert is `Failed` (`RevertFailed`, the
    VM may or may not have been reverted) and is never sent again for that request;
  - only to the same `VMSnapshot` (same UID), and only while the Provider still reports
    snapshot support. If it was deleted meanwhile, or the Provider now reports
    `supportsSnapshots: false`, the revert is `Failed` with the same warning;
  - at most 5 calls for one request; after that the revert is `Failed`, with the same
    warning.
- **A definite failure** (a refusal, an error the provider reported, an unimplemented
  RPC) is terminal: phase `Failed`. Its message is the provider's answer, made safe for
  status (one line, URL credentials removed, bounded).

The `VirtualMachine` revert has no `virtrigaud.io/operation-outcome` confirmation,
unlike the calls in [`provider-call-outcomes.md`](provider-call-outcomes.md): a re-sent
revert of a powered-off VM cannot duplicate anything.

## Status, condition and events

### `status.snapshotRevert`

| Field | Meaning |
|---|---|
| `snapshotName` | The `VMSnapshot` the request is for |
| `snapshotUID` | Its UID; empty while it was not found |
| `phase` | `Pending` (waiting; the condition says for what), `Reverting` (sent, outcome not known yet), `Succeeded`, `Failed` |
| `taskRef` | The provider task of an asynchronous revert, while it runs |
| `attempts` | `SnapshotRevert` calls sent for this request (more than 1 only after unknown outcomes) |
| `requestedAt` | When the request was first recorded |
| `attemptTime` | When the last call was recorded, just before it was sent |
| `completionTime` | When the revert succeeded or failed |
| `message` | What it waits for, or how it ended |

Only the operator writes it. It is absent while `spec.snapshot.revertToRef` is unset.

### The `SnapshotReverted` condition

Absent while `spec.snapshot.revertToRef` is unset. Its `observedGeneration` is the VM's
generation.

| Status | Reason | Meaning |
|---|---|---|
| `False` | `WaitingForPowerOff` | The VM is not reported `Off`, or `spec.powerState` is `On`. Set `spec.powerState: Off` |
| `False` | `SnapshotNotFound` | No `VMSnapshot` of that name in the namespace |
| `False` | `SnapshotNotReady` | The `VMSnapshot` is not `Ready`, has no `status.snapshotID`, or is being deleted |
| `False` | `SnapshotForAnotherVM` | The `VMSnapshot` was not taken of this VM (another `spec.vmRef`, VM UID or Provider), or that cannot be proven |
| `False` | `Unsupported` | The Provider reports no snapshot support (before the revert is sent; after an attempt whose outcome is unknown this ends the request as `Failed`), answers that it does not implement `SnapshotRevert`, or the installed CRD cannot record the request |
| `False` | `Reverting` | Sent; the outcome is not known yet (a task runs, or the call is sent again after an unknown outcome) |
| `True` | `Reverted` | The provider confirmed the revert |
| `False` | `RevertFailed` | The provider failed or refused it, or its outcome stayed unknown. Remove the field and set it again to retry — except after a refusal the provider gives every time (`FailedPrecondition`), whose message says what to do instead (for libvirt: take a new snapshot with the VM powered off) |

While `SnapshotReverted` is `False`, **`Ready` is `False`** with the same reason and the
message prefixed `Snapshot revert: `, unless `Ready` is already `False` for another
reason, which it keeps. A VM whose revert failed therefore stays `Ready=False` until the
field is removed. `status.phase` is not changed by a revert: it follows the power state
(`Stopped` while the VM is off, as it is while a revert runs; `Running` while a revert
waits for a running VM to be powered off, or after a failed revert of a VM that runs;
see [`vm-status-phase.md`](vm-status-phase.md)).

### Events

| Type | Reason | When |
|---|---|---|
| `Normal` | `SnapshotRevertStarted` | A `SnapshotRevert` call is sent (with the attempt number) |
| `Normal` | `SnapshotRevertSucceeded` | The provider confirmed the revert |
| `Warning` | `SnapshotRevertFailed` | The revert failed, or was given up with its outcome unknown |

## GitOps

A manifest that keeps `spec.snapshot.revertToRef` set is safe to apply again and again:
once the record says `Succeeded` or `Failed` for that `VMSnapshot`, nothing is sent.
Two things to know:

- To revert again to the same snapshot from Git, remove the field in one commit, let it
  sync (the record is cleared), then add it back in another.
- A `VirtualMachine` re-created from the same manifest (deleted, restored, moved to a new
  cluster) is a new VM: the field then names a `VMSnapshot` of the old VM, which is
  refused (`SnapshotForAnotherVM`), and the VM stays `Ready=False` until the field is
  removed. Do not keep the field in Git after the revert if VMs are re-created from it.

## Providers

Every provider implements `SnapshotRevert`; the manager sends the VM's id (and, on a
clustered Provider, its bound host and owner) and the `VMSnapshot`'s
`status.snapshotID`.

| Provider | What the revert does (as implemented) |
|---|---|
| vSphere | Finds the snapshot in the VM's snapshot tree by managed-object id (what `SnapshotCreate` answers) or name, refuses templates and image artifacts, and runs `RevertToSnapshot_Task` with `suppressPowerOn=false`, waiting for it in the call (no task is returned). A snapshot taken with memory powers the VM on; the controller then applies `spec.powerState` |
| libvirt (single host) | Checks that the snapshot exists, refuses while another domain depends on the VM's disk (a linked clone: `VM_DISK_IN_USE`, a `RevertFailed`; a check that cannot run is retried as an unknown outcome), refuses a snapshot libvirt cannot revert without breaking the VM's cloud-init CD-ROM — one that stored the CD-ROM with its seed ([below](#libvirt)) — then runs `virsh snapshot-revert <domain> <snapshot> --force` (no `--running`, as the VM is off), inserts the seed again into the CD-ROM the revert left empty, and checks that no read-only disk was moved to a new file. Synchronous |
| libvirt (clustered) | The same, routed to the VM's bound host and owner-checked: a domain this VM does not own is `NotFound` and is never reverted. A snapshot id the provider could not have created is refused before any host is touched ([`clustered-provider-inventory.md`](clustered-provider-inventory.md)) |
| Proxmox | Owner-checks the VM, then `POST .../qemu/<vmid>/snapshot/<name>/rollback`; the answer is a task, which the manager polls |
| mock | Answers a task |

### libvirt

A snapshot the libvirt provider takes of a VM that is off, or without memory, is an
external disk-only snapshot (`snapshot-create-as --disk-only`); a memory snapshot of a
running VM is an internal one. libvirt reverts external snapshots from 9.9 on; an older
libvirt refuses, and the provider's error is reported as `RevertFailed`.

libvirt reverts an external snapshot by rebuilding the VM's disks from the domain
definition the snapshot **stored**, and creating a **new** qcow2 overlay for every disk
of that definition that has media — named after the disk's file with its extension
replaced by the revert's time in Unix seconds (`<domain>-disk.<epoch>` for the VM's
disk) — and it removes the overlay it replaces. `SnapshotDelete` and VM delete recognise
the `<domain>-disk.<epoch>` overlay as the VM's own
([`libvirt-clones.md`](libvirt-clones.md#what-snapshotdelete-removes)).

The stored definition includes the VM's read-only cloud-init seed CD-ROM. A revert of a
snapshot that stored the CD-ROM with its seed rewrites the CD-ROM to a qcow2 overlay of
the seed ISO inside the VM's private seed directory, owned by root, which the host's
AppArmor profile for the VM does not let QEMU open: the VM no longer starts (`paused
(starting up)` or `shut off (failed)`, the power-on fails: `Ready=False/ProviderError`). libvirt does this
whatever the CD-ROM's mode in the snapshot's disk list — it records a read-only CD-ROM
`snapshot='no'` by itself, and excluding it with `--diskspec <target>,snapshot=no` does
not help — and it then rewrites the snapshot's metadata to record the overlay as an
external snapshot disk. Every snapshot v0.4.0-rc6 and earlier took of a VM with
cloud-init stored its seed. Since v0.4.0-rc7:

- **`SnapshotCreate` of a powered-off VM takes the snapshot with the VM's CD-ROMs and
  floppies ejected**, so the stored definition's drives are empty. For each drive that
  holds a file, in this order: the provider records the drives and their files in the
  domain's metadata (namespace `https://virtrigaud.io/xmlns/libvirt/media/v1`), ejects
  each one (`virsh change-media <domain> <target> --eject --config`), takes the snapshot,
  inserts each file again (`virsh change-media <domain> <target> <file> --insert
  --config`, also when the snapshot failed), checks that the domain shows each file
  again with its driver type, and removes the record. Only the VM's persistent
  definition changes; it is off.
- **A running VM is never ejected** (the guest would see it). A disk-only snapshot of a
  running VM excludes the CD-ROM with `--diskspec <target>,snapshot=no` but stores its
  seed, so it **cannot be reverted** (see the next point): on libvirt, take the snapshot
  with the VM powered off to be able to revert to it. A memory snapshot of a running VM
  is internal: libvirt reverts it in place, without new overlays, and it is not refused.
  Read-only disks that are not removable media, and media that are not plain files (a
  block device, a network source), are never ejected either; they are excluded with a
  diskspec, and a disk-only snapshot that stores them cannot be reverted.
- **`SnapshotRevert` refuses**, before the revert changes anything (only the repair of
  an interrupted ejection, below, may run first), an external snapshot whose stored
  definition has a CD-ROM, floppy or read-only disk with a source, and a snapshot whose
  disk lists record one as an external snapshot disk. That is a disk-only snapshot of a
  VM with a cloud-init CD-ROM (or another read-only disk) taken while the VM was
  running, and every such snapshot v0.4.0-rc6 or earlier took — as created (the stored
  definition has the seed) and after libvirt rewrote its metadata on a revert (the
  CD-ROM recorded external). The VM gets `SnapshotReverted=False/RevertFailed`
  (terminal, not retried); the provider's part of the message (on a clustered Provider
  after `failed to revert to snapshot: `) reads `revert refused, nothing changed: take a new snapshot with the VM
  powered off. Snapshot "s1" was taken while it ran or by an earlier release; libvirt
  cannot revert it safely (disk hda)`. The manager keeps the first 256 bytes of a
  provider's answer, so a long snapshot name may cut the end; the guidance comes first.
  The manager does not append its "remove `spec.snapshot.revertToRef` and set it again
  to retry" hint to this answer, or to any other `FailedPrecondition` answer to a revert
  (it keeps it for a disk another VM depends on, `VM_DISK_IN_USE`): the same snapshot is
  refused again. A snapshot or domain definition that cannot be read means no revert
  either.
- **After a revert**, the revert leaves the CD-ROM empty (the stored definition had it
  empty). The provider reads the domain again and, for each CD-ROM or floppy that held a
  file before the revert and is now empty, records it, inserts the same file again
  (with `--live` too when the revert kept the VM running), checks it, and removes the
  record. A file that cannot be inserted again is reported as `RevertFailed`
  (`reverted, but CD-ROM hda could not be re-inserted; ...`): the VM was reverted, and
  its seed stays recorded for the repair below. The provider also checks (when the domain can be read after the revert) that no
  read-only disk was moved to a new file; one that was is reported as `RevertFailed`
  (`reverted, but the VM may not start: libvirt moved read-only disk hda to a new file;
  ...`) instead of `Succeeded`. Repair it as below before you request another revert.

**Overlay permissions.** libvirt creates the overlay of a disk-only snapshot, and the
new `<domain>-disk.<epoch>` overlay of a revert, with mode `0600` (owned by root while
the VM is off). The disk in-use check that every other VM's snapshot, revert or delete
runs reads every domain's disk chain on the host. It fails on such a file ("permission
denied reading a disk image") when the provider's SSH user cannot read it and its
`VR_DISK_READ` sudo rule does not cover the file's directory. Grant `VR_DISK_READ` over
the pool directory: see [Unreadable disk images](libvirt-clones.md#unreadable-disk-images),
which also describes the manual repair.

**Interrupted ejections are repaired.** If the provider, its connection or the host
fails between the eject and the re-insert, the VM is left with an empty CD-ROM and the
record in its metadata. A disk-only snapshot that was made but whose seed could not be
inserted again is reported as made; the record stays. The next `SnapshotCreate`,
`SnapshotRevert` or start (`spec.powerState: On`, or a reboot) of that VM inserts the
recorded file again before it changes anything else (after its read-only checks; not
`SnapshotDelete`) — **only when it is provably the VM's own cloud-init seed**
(`cloud-init.iso` in `<seed root>/virtrigaud-cloudinit-<domain>.<random>/`, or an
earlier release's `<seed root>/virtrigaud-cloudinit/<domain>/`, in the provider's seed or
staging directory) — and removes the record. A recorded file that is anything else (an
installer ISO you attached) is never inserted from the record: the provider logs it,
keeps the record, and leaves it to you (`virsh -c "$CONN" change-media <domain> <target>
<file> --insert --config`, then `virsh -c "$CONN" metadata <domain> --uri
https://virtrigaud.io/xmlns/libvirt/media/v1 --remove --config`). A start whose seed
cannot be inserted again is not attempted (the guest would boot without its cloud-init
datasource). Until one of those calls, the VM's CD-ROM stays empty; a VM that is powered
on by hand (`virsh start`) in that window boots without its seed.

To replace a snapshot taken by v0.4.0-rc6 or earlier, or while the VM was running:
delete its `VMSnapshot` (deleting a snapshot that was never reverted merges it on
libvirt 10), power the VM off, and create a new one. The same applies on single-host
and clustered Providers.

#### Repairing a VM broken by a v0.4.0-rc6 revert

A VM reverted by v0.4.0-rc6 shows its CD-ROM on a `cloud-init.<digits>` file in its
seed directory and does not start. The snapshot itself recorded the CD-ROM
`snapshot='no'` when it was taken; the broken state comes from libvirt's revert, which
also rewrote the snapshot's metadata to record that overlay as an external snapshot
disk. The VM's disk is reverted correctly; only the CD-ROM needs repair. The steps need
the provider's SSH user (or root) on the VM's host. Name the connection explicitly, as in
the [re-attach runbook](clustered-restore.md#re-attach-runbook): `export
CONN=qemu:///system` on the host itself, or the Provider's (single host) or Host's
(clustered) endpoint, e.g. `qemu+ssh://virtrigaud@host-a.example.com/system`. Do the
steps in this order, and do **not** delete the `VMSnapshot` before step 4: its metadata
still names the CD-ROM's overlay.

1. Find the domain and the overlay. The CD-ROM's source ends in `cloud-init.<digits>`:

   ```sh
   virsh -c "$CONN" domblklist --details <domain>
   # e.g.  file  cdrom  hda  /var/lib/libvirt/virtrigaud-seeds/virtrigaud-cloudinit-<domain>.<random>/cloud-init.1792831471
   sudo qemu-img info -U <that file>   # optional: backing file: .../cloud-init.iso, same directory
   ```

2. Stop the domain, if it is not shut off: `virsh -c "$CONN" destroy <domain>`.

3. Point the CD-ROM back at the seed ISO. Dump the inactive definition, edit the CD-ROM's
   `<disk device='cdrom'>`: `<driver name='qemu' type='raw'/>`, `<source
   file='<seed directory>/cloud-init.iso'/>`, and remove any `<backingStore>` inside it;
   then define it again (the owner stamp in `<metadata>` is kept):

   ```sh
   virsh -c "$CONN" dumpxml --inactive <domain> > <domain>.xml
   # edit the cdrom <disk> as above
   virsh -c "$CONN" define <domain>.xml
   ```

4. Drop the reverted snapshot's metadata only — not its files; the VM's disk
   (`<domain>-disk.<digits>`) is its live disk now:
   `virsh -c "$CONN" snapshot-delete <domain> <snapshot> --metadata`.

5. Remove the CD-ROM's overlay, and only it (never `cloud-init.iso`):
   `rm -- <seed directory>/cloud-init.<digits>` (it is owned by root; the seed directory
   is the SSH user's, so the SSH user can remove it).

6. Start the VM: set its `spec.powerState: On` (the controller starts it), or
   `virsh -c "$CONN" start <domain>`. Remove `spec.snapshot.revertToRef`, delete the
   `VMSnapshot` (the provider finds no snapshot and reports it gone), and take a new
   snapshot with the VM powered off.

Deleting the VM afterwards removes the domain, its whole disk chain (the
`<domain>-disk.<digits>` overlay included) and its seed directory. Other snapshots of the
VM taken by v0.4.0-rc6 or earlier are refused on revert as above; delete and re-take
them.

## Limitations

- `status.currentResources` is not changed by a revert. If the hypervisor restores a
  different CPU or memory size with the snapshot, the manager does not notice it.
- A `VMSet` whose template sets `revertToRef` gives every member the same `VMSnapshot`
  name; each member accepts only a snapshot of itself.
- There is no operator confirmation for an unknown outcome. A revert whose outcome stays
  unknown — after 5 attempts, a VM seen running in between, or a task not completed
  within 2 hours — is `Failed` with a message saying the VM may or may not have been
  reverted: check the VM, and request the revert again if it was not.
- While a revert task runs (up to 2 hours), the VM is not powered on or resized, even if
  `revertToRef` is removed.

## Upgrade notes

- Additive API change: `status.snapshotRevert` and the `SnapshotReverted` condition. No
  existing field changes meaning. **Upgrade the CRDs before the manager**: a manager
  whose CRD prunes `status.snapshotRevert` sends no revert (condition
  `SnapshotReverted=False/Unsupported`).
- A `VirtualMachine` that already carries `spec.snapshot.revertToRef` (set before this
  release, when it did nothing) is acted on by the first reconcile after the upgrade:
  reverted if it is off with `spec.powerState: Off` and the snapshot resolves, otherwise
  `Ready=False` with the reason. Remove the field from VMs you do not want reverted
  before you upgrade the manager.
- The manager needs `get`, `list` and `watch` on `vmsnapshots`, which its role already
  grants.
- libvirt: a disk-only `VMSnapshot` of a VM with a cloud-init CD-ROM (or any read-only
  disk) taken by v0.4.0-rc6 or earlier, or taken while the VM was running, is refused on
  revert (`RevertFailed`, nothing changed); delete it and take a new one with the VM
  powered off after upgrading the libvirt provider. A VM already reverted by v0.4.0-rc6
  to such a snapshot needs the [repair](#repairing-a-vm-broken-by-a-v040-rc6-revert)
  above. Snapshots of powered-off VMs now briefly eject the CD-ROM from the VM's
  persistent definition; a VM started by hand (`virsh start`) while an interrupted
  snapshot left it ejected boots without its seed ([Interrupted ejections are
  repaired](#libvirt)).
