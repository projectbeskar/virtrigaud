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
- A `Failed` revert is not retried on its own. Remove the field and set it again.

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
| `False` | `RevertFailed` | The provider failed or refused it, or its outcome stayed unknown. Remove the field and set it again to retry |

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
| libvirt (single host) | Checks that the snapshot exists, refuses while another domain depends on the VM's disk (a linked clone: `VM_DISK_IN_USE`, a `RevertFailed`; a check that cannot run is retried as an unknown outcome), refuses a snapshot that records the VM's read-only cloud-init CD-ROM ([below](#libvirt)), then runs `virsh snapshot-revert <domain> <snapshot> --force` (no `--running`, as the VM is off) and checks that the VM's read-only disks (its CD-ROM) were not moved to new files. Synchronous |
| libvirt (clustered) | The same, routed to the VM's bound host and owner-checked: a domain this VM does not own is `NotFound` and is never reverted. A snapshot id the provider could not have created is refused before any host is touched ([`clustered-provider-inventory.md`](clustered-provider-inventory.md)) |
| Proxmox | Owner-checks the VM, then `POST .../qemu/<vmid>/snapshot/<name>/rollback`; the answer is a task, which the manager polls |
| mock | Answers a task |

### libvirt

A snapshot the libvirt provider takes of a VM that is off, or without memory, is an
external disk-only snapshot (`snapshot-create-as --disk-only`). libvirt reverts external
snapshots from 9.9 on; an older libvirt refuses, and the provider's error is reported as
`RevertFailed`. libvirt reverts one by creating a **new** overlay for every disk the
snapshot recorded as external, named after the disk's file with its extension replaced
by the revert's time in Unix seconds (`<domain>-disk.<epoch>` for the VM's disk), and
removing the overlay it replaces. `SnapshotDelete` and VM delete recognise that overlay
as the VM's own ([`libvirt-clones.md`](libvirt-clones.md#what-snapshotdelete-removes)).

**Read-only disks are never part of a snapshot (since v0.4.0-rc7).** v0.4.0-rc6 and
earlier took disk-only snapshots without a disk list, and libvirt then records **every**
disk that is not empty as an external snapshot disk, including the VM's read-only
cloud-init seed CD-ROM. Reverting such a snapshot on libvirt 10 rewrote the CD-ROM to a
new qcow2 overlay of the seed ISO, inside the VM's private seed directory; the host's
AppArmor profile for the VM does not let QEMU open that file, so the VM no longer started
(`paused (starting up)` or `shut off (failed)`, `Ready=False` "power failed") while the
revert was reported `Succeeded`. Now:

- `SnapshotCreate` reads the domain and passes `--diskspec <target>,snapshot=no` for
  every CD-ROM, floppy and disk marked `<readonly/>` — for disk-only and memory
  snapshots alike. In a disk-only snapshot the VM's writable disks keep libvirt's
  defaults (external, with the overlay named `<domain>-disk.<snapshot>`); in a memory
  snapshot they stay internal. A read-only disk without a target, or whose target is not
  a plain libvirt target name (a lower-case letter, then lower-case letters and digits,
  at most 32), means no snapshot is taken.
- `SnapshotRevert` reads the snapshot (`virsh snapshot-dumpxml`) and the domain first,
  and **refuses** a snapshot that records a read-only disk or CD-ROM as an external
  snapshot disk — one taken by v0.4.0-rc6 or earlier — before anything changes on the
  host. The VM gets `SnapshotReverted=False/RevertFailed` (terminal, not retried); the
  provider's part of the message reads `revert refused, nothing changed: take a new
  snapshot and delete this VMSnapshot. Snapshot "s1" is from an earlier release and
  includes read-only disk hda (the VM would not start)`. The manager keeps the first 256
  bytes of a provider's answer, so a long snapshot name may cut the end; the guidance
  comes first. The manager's generic "remove `spec.snapshot.revertToRef` and set it again
  to retry" hint does not help here: the same snapshot is refused again. A snapshot or
  domain definition that cannot be read means no revert either.
- After a revert, the provider reads the domain again and checks that each read-only
  disk, CD-ROM and floppy (empty media aside) still has the source it had before the
  revert or the one the snapshot recorded. A revert that moved one anyway is reported
  as `RevertFailed` (`reverted, but the VM may not start: libvirt moved read-only disk
  hda to a new file; ...`) instead of `Succeeded`: the VM was reverted, but may not
  start. Repair it as below before you request another revert. If the domain cannot be
  read after the revert, the provider logs it and reports the revert as `Succeeded`
  (the snapshot was checked before the revert).

To replace a snapshot taken by v0.4.0-rc6 or earlier: delete its `VMSnapshot` (deleting
one that was never reverted merges it cleanly on libvirt 10), then create a new one. The
same applies on single-host and clustered Providers.

#### Repairing a VM broken by a v0.4.0-rc6 revert

A VM reverted by v0.4.0-rc6 to a snapshot that recorded its seed CD-ROM shows its CD-ROM
on a `cloud-init.<digits>` file in its seed directory and does not start. Its disk is
reverted correctly; only the CD-ROM needs repair. The steps need the provider's SSH user
(or root) on the VM's host. Name the connection explicitly, as in the
[re-attach runbook](clustered-restore.md#re-attach-runbook): `export CONN=qemu:///system`
on the host itself, or the Provider's (single host) or Host's (clustered) endpoint, e.g.
`qemu+ssh://virtrigaud@host-a.example.com/system`. Do the steps in this order, and do
**not** delete the `VMSnapshot` before step 4: its metadata still names the CD-ROM's
overlay.

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
   `rm -- <seed directory>/cloud-init.<digits>`.

6. Start the VM: set its `spec.powerState: On` (the controller starts it), or
   `virsh -c "$CONN" start <domain>`. Remove `spec.snapshot.revertToRef`, delete the
   `VMSnapshot` (the provider finds no snapshot and reports it gone), and take a new
   snapshot.

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
- libvirt: a `VMSnapshot` of a VM with a cloud-init CD-ROM (or any read-only disk) taken
  by v0.4.0-rc6 or earlier is refused on revert (`RevertFailed`, nothing changed); delete
  it and take a new one after upgrading the libvirt provider. A VM already reverted by
  v0.4.0-rc6 to such a snapshot needs the [repair](#repairing-a-vm-broken-by-a-v040-rc6-revert)
  above.
