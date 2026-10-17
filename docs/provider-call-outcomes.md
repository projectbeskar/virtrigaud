# Provider calls whose outcome was lost (`...Requested`, `...OutcomeUnknown`)

A few provider calls make something that a second call could duplicate: the source
snapshot and the disk export and import of a `VMMigration`, and the snapshot of a
`VMSnapshot`. If the manager restarts, or leadership moves to another manager
replica, after such a call was sent but before its result was written to the
object's status, the result is lost. In v0.4.0-rc1 the new leader then sent the call
again. Depending on the provider, that could take a second snapshot of the live
source VM or land a second disk.

The same holds for a call that **fails without a definite answer**: it ran past its
deadline, or the connection dropped after the request was written (a provider pod
restarted mid-call). The provider may still be completing it — a command running on
the hypervisor host outlives a cancelled call. In rc1 such a failure failed the
migration attempt, and the retry policy sent the call again a few minutes later,
possibly while the first one was still writing.

Since rc2 (R6), the manager writes a durable record before each of these calls and
uses it to decide what to do when a result is missing or the outcome is unknown.

## The marker

Right before the call, the object's status records a condition of its own type,
with `status: "True"`:

| Object | Call | Condition |
|---|---|---|
| `VMMigration` | `SnapshotCreate` of the source VM | `SnapshotRequested` |
| `VMMigration` | `ExportDisk` | `ExportRequested` |
| `VMMigration` | `ImportDisk` | `ImportRequested` |
| `VMSnapshot` | `SnapshotCreate` | `SnapshotRequested` |

- **Written before the call, with a precondition.** The status is written with the
  `resourceVersion` the reconcile read. If that write fails for any reason,
  including a conflict with another writer, **nothing is sent**.
- **Removed with the outcome.** The call's result, or a definite failure from the
  provider, is written with the marker removed, in the same status write. A
  definite failure is one that proves the outcome:
  - nothing was done: the request never reached the provider, its host was
    unreachable, or it answered that it did not perform the call;
  - the provider refused the call before doing anything: an invalid request, a VM
    not found or not owned, a name conflict, a disk-size refusal.

  Every other failure — a deadline, `Unavailable` after the request was written, an
  error the provider did not classify — **keeps the marker** and is handled like a
  lost result, below. The object is never failed for it. Through a clustered
  Provider, a `SnapshotCreate` answered "retry" is handled the same way. A
  single-host libvirt provider classifies the failures it can prove (see
  [Single-host libvirt: which failures are definite](#single-host-libvirt-which-failures-are-definite)).
- **Attempt id.** The marker's `lastTransitionTime` (RFC 3339) identifies the
  attempt. Every new send writes a new, later one.

A marker with no recorded result therefore means the call may have run, but its
outcome is unknown. The manager never treats a stale informer cache as that case:
it first re-reads the object from the API server.

- **Automatic re-sends are capped.** Each one is counted in the marker's message
  (`(automatic re-send <n>)`):
  - a routed `SnapshotCreate` is re-sent at most 10 times (about ten minutes);
  - an `ExportDisk` or `ImportDisk` at most 3 times.

  After that, the object is held for an operator with a constant message. An
  operator's confirmation starts the count again.
- **Undecided cases are re-checked quietly.** When the manager cannot tell yet
  whether a call may be sent again — the source VM, a Provider or its capabilities
  cannot be read — it re-checks every minute. It writes no hold and records no
  event.

## What happens to a call whose outcome is unknown

| Call | Sent again when | Otherwise |
|---|---|---|
| `SnapshotCreate` (`VMMigration` and `VMSnapshot`) | The VM is on a **clustered** Provider (`topology: cluster`). The routed create records the request token (the `VMSnapshot` UID, or the migration UID and attempt) with the snapshot. A re-send returns that snapshot, but only once no job runs on the domain. libvirt lists a snapshot as soon as its job starts, before memory and disks are written, so until then the answer is "retry". For a `VMMigration`, the marker must belong to the current attempt. Re-sends are at least one minute apart. | Held: `Ready=False`, reason `SnapshotOutcomeUnknown` |
| `ExportDisk` | The source Provider lists the migration's staging backend in the capability `resend_safe_export_backends`, **and** 35 minutes have passed since the marker. The manager's deadline on an export is 30 minutes, so the earlier call has ended by then. Until then, the migration waits with `Ready=False/ExportOutcomeUnknown` and a message saying it will be re-sent. | Held: `Ready=False/ExportOutcomeUnknown` |
| `ImportDisk` | The target Provider lists the backend in `resend_safe_import_backends`, and 35 minutes have passed. | Held: `Ready=False/ImportOutcomeUnknown` |

### Why each provider answers the way it does

**`SnapshotCreate` on a single host.** Only the clustered libvirt provider records
the request token and returns the earlier snapshot. On a single host:

| Provider | What a second `SnapshotCreate` of the same name does |
|---|---|
| vSphere | Creates a second snapshot with the same name. |
| libvirt (single host) | Refuses the name and does not return the first snapshot. |
| Proxmox | Starts a task that fails because the name is taken. |
| mock | Creates a second snapshot. |

**`ExportDisk` and `ImportDisk`.** The export destination is chosen by the manager
(`destination_url`), and the landed disk's name is derived from the target VM. A
re-send therefore replaces the earlier result rather than adding a second one. That
holds only where no earlier call can still be writing, so each provider lists only
the backends where this is proven:

| Provider | `resend_safe_export_backends` | `resend_safe_import_backends` |
|---|---|---|
| vSphere | `s3`, `nfs` | `s3`, `nfs` |
| libvirt, single host | `s3` | `pvc`, `s3`, `nfs` |
| libvirt, clustered | `s3`, `nfs` | — (import is not served) |
| Proxmox | `s3` | `s3` |
| mock | — | — |

The reasons rest on one fact: **a command on the hypervisor host outlives a
cancelled call**. It runs without a pty, so closing the SSH session sends it no
hang-up.

- **vSphere:** every step of an export honours the call's cancellation, and the
  temporary datastore clone has a unique name and is always removed. The `pvc` copy
  does not stop when the call is cancelled.
- **libvirt, single host:**
  - Exports: an `s3` object is replaced only when its upload completes. The `nfs`
    and `pvc` exports write in place, and a cancelled call's `qemu-img` or copy can
    keep writing.
  - Imports: every import writes into a private directory and renames the result
    onto the landing name.
- **libvirt, clustered:** an export runs under the per-domain export lock and the
  routed time budget.
- **Proxmox:**
  - Exports: the `nfs` export copies onto the staged file in place on the node.
  - `s3` imports: the bytes are streamed from the provider pod into a private
    temporary file, and `mv -T` publishes it onto the stage only when exactly the
    object's size arrived. A cancelled call ends the node's input, and the short copy
    is discarded.
  - `nfs` imports: the whole node-side stage (mount, copy, `mv -T` onto the stage)
    runs with no input from the pod, no time limit and no lock. After a cancelled
    call it copies to completion and publishes the stage at an arbitrary later time,
    so it is **not** re-send safe.

A provider image older than rc2 reports neither field. An export or import whose
outcome it lost is held.

## Single-host libvirt: which failures are definite

A single-host libvirt provider used to answer almost every failure as an unclassified
`codes.Unknown`. Since rc2 (R21) it answers the failures its code can prove, with a
constant status text (the detail is in the provider log, never in a status or event):

- **Not performed** (`Unavailable` with `VM_OPERATION_FAILED`, which the manager reads as
  "the provider did not perform the operation" and keeps out of the Provider's circuit
  breaker). The failing step only reads or validates, or writes into a private area that
  the call removes, and the call's one publishing step never ran.
- **A typed refusal** (`InvalidArgument`, `NotFound`, `AlreadyExists`,
  `FailedPrecondition`, `PermissionDenied`) for a request the provider refused before
  doing anything.
- **Unknown** (an untyped `Unknown`): the call may have changed something, or may still
  be running. The marker is kept, as above.

| Call | Definite (the attempt fails, the marker is cleared) | Unknown (the marker is kept) |
|---|---|---|
| `SnapshotCreate` | The domain's state, disk dependencies or snapshot names cannot be read (not performed); the domain does not exist (`NotFound`); the snapshot name is already taken (`AlreadyExists`, nothing is created); the disk is a linked clone's backing file (`FailedPrecondition`); `virsh snapshot-create-as --atomic` ended with an exit status of its own **and** the snapshot, listed again, is absent (not performed). | The create's connection dropped or `virsh` was killed (exit status 128 or above); the request's deadline passed; the snapshot exists after a failed create; the list after the failed create failed. |
| `ExportDisk` `s3` | The storage configuration is unusable (`InvalidArgument`); the source domain or disk is missing or refused; **any** failure of the flatten (it writes only into a private directory that is removed, and the upload is the only publishing step); the S3 service answered the upload with an HTTP 4xx, or the host stream broke before the upload completed (it is never completed, so nothing is published). | The upload may have completed: an HTTP 5xx, a network error, or a deadline while the upload ran. |
| `ExportDisk` `nfs` | An invalid destination (`InvalidArgument`); the source domain or disk is missing or refused; any check before the convert; the in-place `qemu-img convert` **ended with an exit status of its own** (it no longer writes; any partial staged file is replaced by the next export). | The convert was killed or its connection dropped: it may still be writing the staged file. |
| `ExportDisk` `pvc` | Every failure. The export runs in the provider's own process: the local `qemu-img` is killed with the call, the copy is an in-process write that has finished when the call returns, and a partial file is replaced by the next export. | None. |
| `ImportDisk` `s3` and `nfs` | The request is unusable (`InvalidArgument`, `FailedPrecondition`); **any** failure before the rename onto the landing name (the staged object and the converted disk are in a private directory that is removed, and the rename runs only in the same call); the rename ended with an exit status of its own. | The rename's connection dropped or `mv` was killed: the disk may have landed. |
| `ImportDisk` `pvc` | The request is unusable; the staged disk is missing (`NotFound`); any failure up to and including the copy to the host's pool directory, as above. | A failure after that copy (the pool or volume step): the disk is already on the host. |

A failure that already carries a typed refusal keeps it (for example the disk-size limit
refusals, which hold the migration instead of failing it).

## Releasing a held object

The hold's `Ready` message is constant and names the action. The `Warning` event of
the same reason gives the attempt id and, for a snapshot, the name of the snapshot
that was asked for.

1. **Check the provider for what the call may have made, and remove it.**
   - **Snapshot:** look on the source VM for a snapshot with the name from the
     event.
   - **Import:** look on the target provider for the disk landed for the target
     `VirtualMachine`. Its name ends in `-migrated`.
   - **Export:** make sure no export of this migration is still running. The staged
     object itself is replaced by the next export.
2. **Confirm the attempt.** This sends the call again:

   ```sh
   ATTEMPT=$(kubectl get vmmigration <name> -n <ns> \
     -o jsonpath='{.status.conditions[?(@.type=="ExportRequested")].lastTransitionTime}')
   kubectl annotate vmmigration <name> -n <ns> virtrigaud.io/operation-outcome=not-performed@$ATTEMPT
   ```

   - For a `VMSnapshot`, use `vmsnapshot` and the `SnapshotRequested` condition.
   - For a migration snapshot or import, use the `SnapshotRequested` or
     `ImportRequested` condition.

   How the annotation is handled:
   - The manager logs the confirmation and records a `Warning` event
     `OperationOutcomeConfirmed`. The log line names the annotation's field
     managers (`annotationManagers`, for example `kubectl-annotate/Update`). The
     API server's audit log is the authoritative record of **who** set the
     annotation.
   - It removes the annotation before it sends the call, so a confirmation never
     carries over to a later attempt.
   - A value that names another attempt is ignored.
3. **Or delete the object.** The `VMMigration` or `VMSnapshot` cleanup removes only
   what its status records. Anything the lost call made is yours to remove.

A held object writes its status once and is re-checked every minute. A confirmation
re-drives it at once.

## Limitations

- **Some single-host libvirt failures are still held.** The provider classifies the
  failures its code can prove (see
  [the table above](#single-host-libvirt-which-failures-are-definite)). The rest keep
  the marker instead of failing the attempt:
  - a snapshot create that lost its connection, was killed, or left the snapshot behind
    is held for an operator;
  - an `s3` export whose upload may have completed is re-sent after the settle time;
  - an `nfs` export whose convert was killed, and an import whose rename was lost or
    that failed after the disk landed, are held.
  A provider image older than rc2 still answers every libvirt failure untyped, and the
  manager holds each of them.
- **Host-unreachable failures of these three calls no longer count toward the circuit
  breaker.** A single-host libvirt provider whose host cannot be reached while a
  `SnapshotCreate`, `ExportDisk` or `ImportDisk` runs now answers "not performed", which
  is out of the breaker like every `VM_OPERATION_FAILED`. Other calls to the same
  provider (`Describe`, `Power`, ...) still count the failure.
- **A lost failure write is held.** A known failure whose status write itself is
  lost (a conflict on the failure write, a restart before it) leaves the marker. The
  object is then held for an operator, although the provider answered definitely.
  Confirming the attempt sends the call again.
- **A deleting migration waits out an unknown export.** If an `ExportRequested`
  marker has no result, the staged object of an `nfs` or `s3` migration is not
  deleted until 35 minutes after the marker. An export still running on the host
  could otherwise re-create the object after the delete, and nothing would own it.
  `virtrigaud.io/force-delete` releases the migration at once, with a
  `StagedDiskLeftBehind` warning.
- **Deleting a held object skips the lost call's snapshot.** A `VMSnapshot` or
  `VMMigration` deleted while it is held releases its finalizer as before. A
  snapshot the lost call may have made is not looked up.
- **In-process results.** Within one manager process, a result whose write failed
  is still held in memory and written by the next reconcile (#388). If a
  `VMMigration` is released while such a result is held, the manager logs the
  provider's ids at `Error`: `unrecorded provider result dropped with the released
  VMMigration`.

## Upgrade notes

- **No CRD change.** The markers are conditions.
- **Capability fields.** The new fields are `GetCapabilitiesResponse` fields 27
  (`resend_safe_export_backends`) and 28 (`resend_safe_import_backends`). They are
  additive.
- **Roll the providers promptly after the manager.** Until a provider reports the
  fields, an export or import whose outcome was lost is held for an operator instead
  of being re-sent. Until a single-host libvirt provider is rolled to the rc2 image
  that carries R21, every libvirt failure of these calls is held.
- **Objects in flight across the upgrade carry no marker.** A result lost in the
  upgrade itself behaves as in rc1.
