# Provider calls whose outcome was lost (`...Requested`, `...OutcomeUnknown`)

A few provider calls make something that a second call could duplicate: the source
snapshot and the disk export and import of a `VMMigration`, and the snapshot of a
`VMSnapshot`. If the manager restarts, or leadership moves to another manager
replica, after such a call was sent but before its result was written to the
object's status, the result is lost. In v0.4.0-rc1 the new leader then sent the call
again. Depending on the provider, that could take a second snapshot of the live
source VM or land a second disk.

Since rc2 (R6), the manager writes a durable record before each of these calls and
uses it to decide what to do when a result is missing.

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
  provider, is written with the marker removed, in the same status write.
- **Attempt id.** The marker's `lastTransitionTime` (RFC 3339) identifies the
  attempt. Every new send writes a new, later one.

A marker with no recorded result therefore means the call may have run, but its
outcome is unknown. The manager never treats a stale informer cache as that case:
it first re-reads the object from the API server.

## What happens to a call whose outcome is unknown

| Call | Sent again when | Otherwise |
|---|---|---|
| `SnapshotCreate` (`VMMigration` and `VMSnapshot`) | The VM is on a **clustered** Provider (`topology: cluster`). The routed create records the request token (the `VMSnapshot` UID, or the migration UID and attempt) with the snapshot, and a re-send returns that snapshot. For a `VMMigration`, the marker must belong to the current attempt. Re-sends are at least one minute apart. | Held: `Ready=False`, reason `SnapshotOutcomeUnknown` |
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
| Proxmox | `s3` | `s3`, `nfs` |
| mock | — | — |

The reasons:

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
  - Imports: imports are staged through a private temporary file and `mv -T`.

A provider image older than rc2 reports neither field. An export or import whose
outcome it lost is held.

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
     `OperationOutcomeConfirmed` for audit.
   - It removes the annotation before it sends the call, so a confirmation never
     carries over to a later attempt.
   - A value that names another attempt is ignored.
3. **Or delete the object.** The `VMMigration` or `VMSnapshot` cleanup removes only
   what its status records. Anything the lost call made is yours to remove.

A held object writes its status once and is re-checked every minute. A confirmation
re-drives it at once.

## Limitations

- **A failure answer is still final.** A call the provider answered with a failure
  is recorded as failed, as before. For example, an export that ran past its
  deadline fails the migration attempt, and the retry policy then sends it again.
  For a migration snapshot, a retry takes a new snapshot under the next attempt's
  name (`-r<n>`).
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
  of being re-sent.
- **Objects in flight across the upgrade carry no marker.** A result lost in the
  upgrade itself behaves as in rc1.
