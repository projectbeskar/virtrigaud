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

A `VirtualMachine`'s `SnapshotRevert` (`spec.snapshot.revertToRef`) is recorded the same
way — before the call, with the optimistic lock, nothing sent if the write fails — but
in a status field instead of a condition: `status.snapshotRevert` with `phase:
Reverting`, `attempts` and `attemptTime`. See
[`vm-snapshot-revert.md`](vm-snapshot-revert.md#recorded-before-it-is-sent).

- **Written before the call, with a precondition.** The status is written with the
  `resourceVersion` the reconcile read. If that write fails for any reason,
  including a conflict with another writer, **nothing is sent**.
- **Removed with the outcome.** The call's result, or a definite failure from the
  provider, is written with the marker removed, in the same status write. That
  write is not lost when the object was edited while the call ran (a label, an
  annotation, a spec change: the write conflicts): since v0.4.1 the outcome of a
  `VMSnapshot`'s `SnapshotCreate` is then written again over the object as it is
  stored, as long as it still carries the same marker and records no snapshot id.
  See [An answer is not lost to an edit made during the call](#an-answer-is-not-lost-to-an-edit-made-during-the-call).
  A definite failure is one that proves the outcome:
  - nothing was done: the request never reached the provider, its host was
    unreachable, or it answered that it did not perform the call;
  - the provider refused the call before doing anything: an invalid request, a VM
    not found or not owned, a name conflict, a disk-size refusal.

  Every other failure — a deadline, `Unavailable` after the request was written, an
  error the provider did not classify — **keeps the marker** and is handled like a
  lost result, below. The object is never failed for it. Through a clustered
  Provider, a `SnapshotCreate` answered "retry" is handled the same way. A
  `VMSnapshot`'s `SnapshotCreate` that the provider answers it did not
  perform (`HOST_UNAVAILABLE`, `VM_DISK_CHECK_FAILED`, `VM_OPERATION_FAILED`
  "not performed", or a request that never reached it; a clustered Provider since
  v0.4.0, a single-host one too since v0.4.1) is not failed either: its marker is removed (the attempt is
  closed, so it never counts toward the re-send cap below), the `VMSnapshot`
  shows `Ready=False/ProviderError`, and the create is sent again as a new
  attempt (with the same request token), after 1 minute, doubling for each consecutive such answer up to 10 minutes
  (in the manager's memory per `VMSnapshot`; any other answer, or a manager
  restart, starts it again at 1 minute). A reconcile before that wait has
  passed sends nothing. A
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
| `SnapshotRevert` (`VirtualMachine`) | On any provider, because a VM that stayed off reaches the same state with a second revert: a fresh `Describe` reports the VM `Off`, `spec.powerState` is still `Off` or `OffGraceful`, the `VMSnapshot` is the same one (same UID), the Provider still reports snapshot support, and 6 minutes have passed since the last attempt, doubling to 30 (the manager's deadline on `SnapshotRevert` is 5 minutes). At most 5 calls. An unknown outcome includes a clustered `HOST_CONNECTION_LOST`. An answer that the provider did not perform the revert (`contracts.IsNothingDone`) is not an unknown outcome: from v0.4.1 the request goes back to `Pending` (`RevertNotPerformed`) and is sent again after 1 minute, doubling to 10, without counting toward the 5 calls ([`vm-snapshot-revert.md`](vm-snapshot-revert.md#asynchronous-reverts-and-unknown-outcomes)). No operator confirmation is needed or read. | `status.snapshotRevert.phase: Failed` (`RevertFailed`, or `Unsupported` when the Provider stopped reporting snapshot support), never sent again for the request: when the VM is seen in any other power state or its spec asks for it to run (it may have run since the attempt), when the `VMSnapshot` was deleted, or after 5 calls |

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
| libvirt, clustered | `s3`, `nfs` | `s3`, `nfs` (routed import, ADR-0007 Addendum B) |
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
  routed time budget. A routed import runs every host command under `flock` on the
  import's host lock and `timeout(1)` ending before the routed budget, writes only
  into a private directory, and publishes with one rename onto the landing name
  derived from the target VM (see
  [Clustered libvirt: the routed import](#clustered-libvirt-the-routed-import)).
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
| `SnapshotCreate` | The domain's state, disk dependencies, snapshot names or disk sources cannot be read (not performed); the domain does not exist (`NotFound`); the snapshot name is already taken (`AlreadyExists`, nothing is created); a disk-only snapshot's overlay file is already on the host (`AlreadyExists`, v0.4.1, [V12](libvirt-clones.md#a-snapshot-name-whose-overlay-file-is-still-on-the-host)); the disk is a linked clone's backing file (`FailedPrecondition`); `virsh snapshot-create-as --atomic` ended with an exit status of its own, its first `error:` line is one of libvirt's **refusals before it executes anything**, **and** the snapshot names and `domblklist --details` sources, read again, are as before. A busy domain or one in the wrong state (`cannot acquire state change lock`, `Requested operation is not valid`, `already exists`) is not performed. An existing overlay file is `AlreadyExists` (V12). A snapshot the VM's configuration does not support (an error of class `unsupported configuration`, `Operation not supported`, `argument unsupported` or `invalid argument`, v0.4.1) is `InvalidArgument`: sending it again cannot help. | The create's connection dropped, `virsh` was killed (exit status 128 or above), reported that it lost libvirtd or timed out its keepalive; the request's deadline passed; **any other error text** (libvirt saves a snapshot's metadata last and drops the snapshot from its list when that fails, after the disk sources were switched); the snapshot exists, the disk sources changed, or either could not be read after the failed create. |
| `ExportDisk` `s3` | The storage configuration is unusable (`InvalidArgument`); the source domain or disk is missing or refused; a check before the flatten, or the flatten **ended with an exit status of its own** (it writes only into a private directory that is removed, and the upload is the only publishing step); **any upload error before the flattened disk was read to its end** - a credentials or bucket refusal, a 5xx, an unreachable service, a broken host stream - because the S3 client sends an upload's completing call only after it has read its source to the end. | The flatten's session dropped or the command was killed (it may still run, and a retry would start a second one); **any upload error once the source was read to its end**, whatever the error: the completing call may have been applied, and the client retries it when its answer is lost (the retry of an applied upload is answered `NoSuchUpload`, or even `AccessDenied`). |
| `ExportDisk` `nfs` | An invalid destination (`InvalidArgument`); the source domain or disk is missing or refused; any check before the convert; the in-place `qemu-img convert` **ended with an exit status of its own** (it no longer writes; any partial staged file is replaced by the next export). | The convert was killed or its connection dropped: it may still be writing the staged file. |
| `ExportDisk` `pvc` | Every failure. The export runs in the provider's own process: the local `qemu-img` is killed with the call, the copy is an in-process write that has finished when the call returns, and a partial file is replaced by the next export. | None. |
| `ImportDisk` `s3` and `nfs` | The request is unusable (`InvalidArgument`, `FailedPrecondition`); a read, a check, the download or the stage into the private directory fails (the directory is removed, and the rename runs only in the same call); the `qemu-img convert` / `dd` **ended with an exit status of its own**; the rename ended with an exit status of its own. | The convert, `dd` or rename session dropped or was killed (a convert may still run; the rename may have happened). |
| `ImportDisk` `pvc` | The request is unusable; the staged disk is missing (`NotFound`); any failure up to and including the copy to the host's pool directory, as above. | A failure after that copy (the pool or volume step): the disk is already on the host. |

**A failure answering the manager's own re-send is never definite.** When the object
already carries the marker of an earlier call whose outcome was not recorded and the
manager sends the call again, the answer says nothing about what the earlier call made
(a `SnapshotCreate` answered `AlreadyExists` may be the snapshot the first call took).
The marker is kept and the object is held or settled, whatever the answer. A routed
(clustered) `SnapshotCreate` is exempt: its provider adopts the earlier snapshot by
request token, so its definite answers stay definite.

**A call sent on the operator's confirmation is a first send**, as a confirmed VM
`Create` is: a definite failure fails the attempt. The exception is an answer that
contradicts the confirmation - a plain `Conflict` (a snapshot of that name exists, the
landing name is in use by another domain) - which holds the object again. A disk-size,
disk-in-use or template `Conflict` says nothing about the earlier call and stays definite.

A failure that already carries a typed refusal keeps it (for example the disk-size limit
refusals, which hold the migration instead of failing it), with the refusal's own text.

**A `VMSnapshot` create that was not performed is sent again (v0.4.1).** On a single host,
as on a clustered Provider since v0.4.0 (V8), a `VMSnapshot` create answered "not
performed" (`contracts.IsNothingDone` and not "outcome unknown: retry") is no longer
`Failed`. The attempt is closed (its marker removed, so it never counts toward a hold),
the `VMSnapshot` shows `Ready=False/ProviderError` ("The provider did not perform the
snapshot creation (nothing was changed) and it will be retried; ..."), the provider's
answer is in the `SnapshotCreateRetrying` event, and the create is sent again after 1
minute, doubling to 10 minutes (in the manager's memory). This includes a request that
never reached the provider and an unreachable host. An answer to the manager's own re-send
still proves nothing and keeps the hold. A cause that does not go away (libvirt refusing a
domain in the wrong state for good, an image that stays unreadable) is retried every 10
minutes for as long as the `VMSnapshot` exists, with the same condition and the
`SnapshotCreateRetrying` event: fix the cause or delete the `VMSnapshot`. The source snapshot of a single-host
`VMMigration` is unchanged: "not performed" fails the attempt, and the retry policy
decides.

**A `VirtualMachine` create the provider refuses is sent again with a backoff (v0.4.1).** A
`Create` answered `InvalidArgument` (`InvalidSpec`) is definite: the marker is cleared and the
VM shows `Ready=False` and `Provisioning=False` with reason `ValidationError` and the provider's
detail. It is sent again after 30 seconds, doubling to 10 minutes, and nothing is sent to the
provider meanwhile (a libvirt `Create` copies the base image before libvirt refuses the
definition, so each re-send copied the image again). The condition message names the next
re-check (`re-checking in 4m0s, at <time>`). An edit of the VM's spec or of the spec of its
`VMClass`, `VMImage` or `Provider` sends it again within seconds and starts the backoff again,
as does a refusal with another message. The backoff is kept in the manager's memory and
recovered from the condition after a manager restart. An answer to the manager's own re-send
(the marker kept, above) is recorded on `Ready` only and is paced the same way. The other
definite refusals a `Create` can get - `NotFound`, `Unimplemented` (`NotSupported`), and a typed
`Unauthorized` or `QuotaExceeded` - were re-sent every 5 seconds and follow the same backoff
now, on a single host and on a clustered Provider; they keep their reason (`ProviderError`)
and their condition (`Provisioning`, or `Ready` for an answer to a re-send), and their message
reads `Failed to create VM (non-retryable; re-checking in ...): <detail>`. A `Conflict`
(`AlreadyExists`) keeps its 2-minute re-check, an untyped `FailedPrecondition` or
`PermissionDenied` keeps the `CreateRefused` backoff (15 seconds doubling to 5 minutes), and a
"not performed" answer and every failure whose outcome is unknown keep their cadence. (A typed
`Unauthorized` or `QuotaExceeded` comes only from the manager's own code; a remote provider's
`Unauthenticated` or `ResourceExhausted` is untyped and keeps the 5-second cadence. None of
the in-tree providers answers a `Create` with `NotFound` or `Unimplemented` today, so the
pacing of those two is for providers that send them; unit tests cover it, and it has not been
run against a hypervisor.) After a
manager restart the wait is read back from the condition that holds the answer to the latest
`Create`: `Provisioning` when it is False, `Ready` while `Provisioning` is the create marker. See
[A refused VM create is retried with a backoff](upgrading.md#a-refused-vm-create-is-retried-with-a-backoff).

### An answer is not lost to an edit made during the call

A provider call can take minutes. If the object is edited meanwhile (a label or an
annotation, a spec change, another controller's metadata write), the status write that
records the provider's answer conflicts with the edit. Up to v0.4.1-rc2 (v0.4.0 included) that answer was then
lost for a `VirtualMachine`'s `Create`, a single-host `VMClone`'s `Clone` and a `VMSnapshot`'s
`SnapshotCreate`: the marker stayed although the provider had answered. A `VirtualMachine`
deleted right after a `Create` the provider answered it did not perform was then held as
`CreateOutcomeUnknown` for an operator, with nothing on the hypervisor.

Since v0.4.1 the manager reads the object again after such a conflict and writes the answer
over it as stored, but only while the object still waits for that answer. Anything newer is
never overwritten; the next reconcile works from it.

| Object and call | Waits for the answer while | What is written again |
|---|---|---|
| `VirtualMachine` `Create`, single host (a refusal, "not performed", a failure on the host, an outcome that stays unknown) | no `status.id`, and the `CreateRequested` marker of the attempt the `Create` was sent under | the `Ready` and `Provisioning` conditions as the answer set them |
| `VirtualMachine` `Create`, clustered | no `status.id`, and the pending host the `Create` was sent to | the `Ready`, `Provisioning` and `Placed` conditions as the answer set them |
| `VMClone` `Clone`, single host, a first attempt's answer that proves nothing was made | the same `CloneRequested` condition | the `VMClone`'s status as the answer set it |
| `VMSnapshot` `SnapshotCreate` (the snapshot made, a definite failure, "not performed") | no `status.snapshotID`, and the same `SnapshotRequested` marker | the `VMSnapshot`'s status as the answer set it |

The answer's conditions keep the `observedGeneration` of the spec the call was built from,
and a `VirtualMachine`'s `status.observedGeneration` keeps its stored value, so a spec change
made during the call is still seen as not yet acted on. A `VMClone`'s or a `VMSnapshot`'s
whole status is written again: only its own controller writes it, and an object is never
reconciled twice at once, so the stored status is the one written with the marker.

Already covered before v0.4.1: a `VirtualMachine`'s created id (`status.id`, a status patch
without a `resourceVersion` precondition), a `SnapshotRevert`'s answer, the result of a
`VMMigration`'s `SnapshotCreate`, `ExportDisk` or `ImportDisk` while the migration is still in
the phase that sent it, a `VMClone`'s accepted clone, and an image prepare's answer on the
`VMImage`. A clustered `VMClone`'s answer recorded on its target, a clustered `Create` whose
host was excluded after a name conflict, and the clustered restore and own-domain holds
(`RestorePending`, `OwnDomainOnAnotherHost`, an imported disk pinned to its host) are not written
again: the pending host stays, so the next reconcile sends the call again to the same host and
gets the same answer.

What is still lost: an answer whose write fails for another reason (the API server
unavailable) or is cut short by a manager restart. The marker then stands, as for a lost
result.

**`SnapshotRevert`** carries no marker, but the libvirt provider answers it the same way
from v0.4.1 (single-host and clustered): "not performed" before the revert ran, a terminal
answer when libvirt refused or failed it, and "outcome unknown: retry" when it may have
run. See [`vm-snapshot-revert.md`](vm-snapshot-revert.md#libvirt).

Three things the table does not show:

- **A single-host libvirt command can outlive the settle time.** The flatten,
  `qemu-img convert` and `dd` run on the host without `timeout(1)` (tracked as R20), and a
  command whose session dropped keeps running. The manager's 35-minute settle assumes the
  earlier call has ended by then; an orphan can still be running past it.
- **A host that cannot be reached fails the attempt.** The reads and checks before a
  call's first change are "not performed" whatever state the connection is in, so a
  `VMMigration` whose libvirt host is down fails the attempt (as in rc1) and its retry
  policy decides; it is not held. A `VMSnapshot` create of an unreachable host is sent
  again after a backoff (below).
- **The s3 and nfs imports may start the default storage pool.** Before the private
  directory is made, the import ensures the `default` pool exists and is active. That is
  idempotent and leaves no disk, so a failure after it is still "nothing landed".

## Clustered libvirt: definite export failures

From v0.4.1 a clustered (routed) `ExportDisk` answers the failures of the `ExportDisk` `s3` and
`nfs` rows above as a single host does: the same constant texts, `InvalidArgument` for an
unusable S3 configuration, and "not performed" (`Unavailable` with `VM_OPERATION_FAILED`) for an
upload the S3 service refused or failed before the flattened disk was read to its end (a
`SignatureDoesNotMatch` or `AccessDenied` refusal, a 5xx, an unreachable service) and for a
`qemu-img convert` that ended with an exit status of its own (an NFS server that refused the
mount, `MNT3ERR_ACCES`). The `VMMigration` fails at once instead of being held with
`ExportOutcomeUnknown` and re-sent after 35 minutes. Before v0.4.1 the routed export answered
all of them as an unknown outcome ("failed to export disk on host ...").

The routed guards keep their own answers first: a flatten or convert that `timeout(1)` stopped,
that was killed, or that outran the routed budget is unknown; the same export still running on
the host is "retry". A host that stops answering is answered by how far the call got
([below](#clustered-libvirt-a-host-that-stops-answering)), never as a definite failure, and the
unknown cases of the rows above (a command whose session dropped, an upload whose completing
call may have taken effect) stay unknown.

No log line or answer of the export names the staged object's location (the NFS server, export,
path and `uid`/`gid`, the S3 bucket, key and endpoint: R28,
[upgrading](upgrading.md#staging-locations-are-no-longer-logged-r28)).

## Clustered libvirt: the routed import

A clustered libvirt provider routes `ImportDisk` to the target VM's pending host
(ADR-0007 Addendum B). Its failures are classified like the single-host import's,
with two differences: a host command stopped by `timeout(1)` is unknown, and
`HOST_UNAVAILABLE` (which the manager reads as "nothing done") is answered only when
the host never answered a command of the call. How the manager acts on each answer
(release the target's host, hold, wait, re-send) is in
[Migration into a clustered provider](clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b).

| Failure | Answer |
|---|---|
| The request is unusable: `pvc` or an empty backend, no `target_host_id`, a `target_vm` without its uid, another storage pool, a non-`nfs://` source, a staged format other than `qcow2`/`raw`/`vmdk` (`nfs`: `qcow2`), an `s3` configuration without its bucket or credentials | `InvalidArgument`, before any host is leased |
| The host is unknown, removed or draining, or cannot be dialed; or the call's first command finds the connection dead | `Unavailable` + `HOST_UNAVAILABLE` (nothing done) |
| A domain named after the target VM is on the landing host | `AlreadyExists`: with `VM_PREVIOUS_INCARNATION` for a previous incarnation, with `VM_PREVIOUS_INCARNATION` and `incarnation: own` for the VM's own domain, plain for a foreign or unstamped one. Nothing is landed |
| The landing file exists and a domain on any host of the Provider uses it | `FailedPrecondition` + `VM_DISK_IN_USE` + `VM_OPERATION_FAILED`. Nothing is landed |
| The landing name is a symbolic link on the landing host | `FailedPrecondition` + `VM_OPERATION_FAILED` (a host-guard refusal, counted by `virtrigaud_provider_host_guard_refusals_total`). Nothing is written |
| A host of the Provider cannot be scanned for that file | `Unavailable` + `HOST_UNAVAILABLE` or `VM_DISK_CHECK_FAILED` (fails closed; nothing done) |
| The default pool is not at `/var/lib/libvirt/images` | `FailedPrecondition` + `HOST_STORAGE_MISCONFIGURED` + `VM_OPERATION_FAILED`, scoped to that host |
| The host is not reached over `qemu+ssh://` | `FailedPrecondition` |
| Any other failure while only reads ran on the landing host (the host answered at least one command, then a read failed or its connection dropped) | Not performed (`Unavailable` + `VM_OPERATION_FAILED`) |
| Another import, delete or in-place create of the same landing name (through any host) runs in the provider for 2 minutes | Not performed (`Unavailable` + `VM_OPERATION_FAILED`) |
| Less than one minute of the time budget is left when the write phase would start | Not performed (`Unavailable` + `VM_OPERATION_FAILED`) |
| The host lock is held: a command of an earlier, cancelled call still runs | `Unavailable` + `VM_OPERATION_FAILED`, "the same import is still running" |
| The host guard refuses the lock directory or lock file; `flock`, `timeout` or `qemu-img` is missing | `FailedPrecondition` + `VM_OPERATION_FAILED` (nothing ran) |
| A staged-image or disk-size refusal | `InvalidArgument` / the disk-size reasons, as on a single host |
| A write-phase command (private directory, stage, copy, checks, convert, rename) **ended with an exit status of its own** (1-127, not 124) | Not performed: nothing is still running and nothing landed (`Unavailable` + `VM_OPERATION_FAILED`) |
| A write-phase command was stopped by `timeout(1)` (124) or killed (137), its session dropped, the routed budget ran out, or the rename's result was lost | **Unknown** (untyped `Unknown`, with `VM_OPERATION_FAILED` so the breaker does not count it): the marker is kept. Because of the time budget the earlier call has ended within the 35-minute settle time, so the manager may re-send |

The removals of the call's own private files run under `timeout(1)` only, never
under the import's `flock`, and are not counted: their failure leaves a private
directory behind (logged, and reaped by a later import once it is 65 minutes
old), never a landed disk.

The routed `DeleteLandedDisk` answers `InvalidArgument` for a request without
`target_host_id` or target VM, a recorded path that is not the landing file, or a
symbolic link or non-file at the landing name; `HOST_UNAVAILABLE` for a landing
host that cannot be leased, or that stops answering before it answered a command (see
[a host that stops answering](#clustered-libvirt-a-host-that-stops-answering)); `FailedPrecondition` + `VM_DISK_IN_USE` +
`VM_OPERATION_FAILED` when a domain on any host uses the disk; `HOST_UNAVAILABLE` /
`VM_DISK_CHECK_FAILED` when a host cannot be scanned; `Unavailable` +
`VM_OPERATION_FAILED` while an import of the name runs in the provider or holds the
host lock, or when the removal fails, is stopped by `timeout(1)` or loses its session
(the last says that it is not known whether the disk was removed); and
`FailedPrecondition` when the host guard refuses the removal. The routed
`DeleteStagedDisk` (`nfs`) answers `InvalidArgument` without `target_host_id`,
`HOST_UNAVAILABLE` for a host that cannot be leased, and `Unavailable` when the
host's `qemu-img` fails, is stopped, or its session drops (never
`HOST_UNAVAILABLE` once a command ran).

## Clustered libvirt: a host that stops answering

The manager reads `HOST_UNAVAILABLE` as "nothing was done": it re-places, re-sends
and clears markers on it. A clustered libvirt provider runs every routed call on one
leased host, and a host that stops answering part-way through a call is not always
"nothing done" (R25). Before R25 the provider answered `HOST_UNAVAILABLE` for **any**
such failure, so an `ExportDisk` whose SSH session dropped while `qemu-img convert`
ran was reported as definite although the copy may have completed or still be
running. The routed `ImportDisk` already used the model below (see
[the routed import](#clustered-libvirt-the-routed-import)); it now applies to every
routed call.

A host has stopped answering when a command of the call ended with no exit status
of its own (the SSH dial, the handshake or the session failed, or the connection
dropped mid-command), or `virsh` could not connect to the host's libvirtd. The
provider records, for each routed call, whether the host answered a command of it
and whether it sent a command that may change the host, and answers by how far the
call got:

| How far the call got | Answer | The manager reads |
|---|---|---|
| The host is unknown, removed or draining, or cannot be dialed; or no command of the call was answered and none that may change the host was sent (the first command finds the connection dead) | `Unavailable` + `HOST_UNAVAILABLE` | Nothing done |
| The host answered at least one command, and no command that may change it was sent (only reads ran) | `Unavailable` + `VM_OPERATION_FAILED` + `HOST_CONNECTION_LOST`, "the host stopped answering before the operation changed anything; nothing was done" | Not performed |
| A command that may change the host was sent. It counts as sent once its SSH session is open (or, for a local connection, once its process starts), and before its result: when the session drops it may have completed, or still be running. A dial or session-open failure ran nothing and does not count | `Unknown` + `VM_OPERATION_FAILED` + `HOST_CONNECTION_LOST`, "the host stopped answering while the operation ran; its outcome is unknown" | Unknown |
| The same, for an RPC that is idempotent per request and that the manager retries on the same host (`SnapshotCreate` with a name and a request token, `Clone`; see below) | `Unavailable` + `VM_OUTCOME_UNKNOWN_RETRY` + `VM_OPERATION_FAILED` + `HOST_CONNECTION_LOST`, "its outcome is unknown and it may still complete on the host: retry" | Retry, outcome unknown: **not** nothing done |

**A host that does not answer in time.** Before a command of a routed call runs, the
call waits for the host: for one of the host's command slots, for the SSH dial and
handshake when there is no connection, and for an SSH session on the connection. A
host that stops answering without closing the connection made these waits last
until the call's own deadline: the session open waited on the dead connection, the
calls stuck there held the host's command slots, and every later call waited for a
slot. Such a call then ended with its context error, which is not a host failure,
and was answered `Unknown` + `VM_OPERATION_FAILED`, "failed to describe VM on host
...", just as the manager's 30-second `Describe` deadline ran out; a call that found
the dead connection at once was answered `HOST_UNAVAILABLE` (v0.4.0-rc5 and
earlier). Now these three waits end 5 seconds before the call's deadline (or a quarter of the time the
call has left, when that is shorter), and a wait that ends there counts as a host
that stopped answering: it ran nothing, so the table above applies (for `Describe`,
always `HOST_UNAVAILABLE`), and the answer reaches the manager before its deadline.
A call whose own context ends first keeps its context error, as before. A command
that was already running when the host stopped answering is not cut short by this;
it ends at the call's deadline, as before. The single-host provider is unchanged.

The wait for the SSH dial also covers a dial that **another call** has in flight:
v0.4.0-rc6 bounded only the dial a call ran itself. The host's one SSH connection
was dialed under a lock, and a call that arrived meanwhile waited for that lock
without regard to its own deadline. A `Describe` (30 seconds) that arrived while a
call with a longer deadline (a `Delete`, or a cleanup that runs detached from its
request) was dialing a host that did not answer therefore waited for that dial, up
to the other call's deadline, and ended with its context error, `Unknown` + "failed
to describe VM on host ..." (in the lab, durations of 1m28s, with a 30-second
deadline). Now one dial runs at a time and holds no lock: a call that needs the
connection waits for that dial's result or for its own bound (5 seconds before its
deadline), whichever comes first, and a wait that ends on its bound is a host that
stopped answering, as above. A dial that failed for its own reason (refused, a
failed handshake, a host key or authentication failure) is the answer of the calls
that waited for it; a dial that ended on its own caller's time says nothing about
the time the others have left, so they dial again. The TCP connect of a dial
stops after 30 seconds at the latest (before, a call without a deadline waited for the
TCP stack to give up, minutes), and a call cancelled during the SSH handshake ends it
at once.

**Which host.** A `HOST_UNAVAILABLE` answer about the host the call was routed to
(its `target_host_id`: the lease failed, or that host stopped answering) carries
that host's id in its `ErrorInfo` metadata, under the key `host`
(`contracts.HostUnavailableHostKey`). The manager already sent that id; the key
only tells it that the host that could not be reached is the VM's own. The
cluster-wide disk guard's `HOST_UNAVAILABLE` (a host that could not be checked
may be another one) carries no `host` key, and neither does an answer from an
older provider. The manager names a host in a `VirtualMachine` condition only
when the key is the VM's own host (below); it never names an endpoint, a URI or
an address.

A command **may change the host** unless it is on a short, fixed list of reads: the
`virsh` subcommands that only read (`list`, `dumpxml`, `domstate`, `snapshot-list`,
`pool-dumpxml`, ...; not `snapshot-current`, which can set the current snapshot, and
`dommemstat` only without `--period`, `--config`, `--live` or `--current`),
`qemu-img info`, `test`, `stat`, and the provider's own path and disk probes. Every
other command counts, and so does any data streamed to or from the host. A command
missing from the list therefore makes an answer less specific (unknown instead of
"not performed"), never wrong. (The `isoinfo` read of a clone's source seed is one such
command.)

**The "retry" form.** `VM_OUTCOME_UNKNOWN_RETRY` is for a call that may have changed the
host, whose outcome the provider cannot tell, and that is safe to send again. The
manager's transport client maps it to a retryable error that is **not** "nothing done"
(`IsNothingDone` is false), so no marker is cleared, no host is re-placed and the answer
is never a definite outcome; it is also kept out of the circuit breaker. The provider
sends `VM_OPERATION_FAILED` with it. A manager that predates the new reason therefore
maps the answer as it did before R25: retryable, out of the breaker, and read as "not
performed" by `IsNothingDone`; the controllers of these calls check "retryable" first
(`VMSnapshot` and `VMMigration` `SnapshotCreate`, the clustered `VMClone`, the landed-disk
cleanup), so an older manager retries it and never fails on it. The same form is used by:
the time-budget answer of a `SnapshotCreate` (the abandoned `virsh` client leaves
libvirtd's job running), the answer "the snapshot made for this request is still being
taken" (the request's snapshot exists, so the call is not "nothing done"), and the
`DeleteLandedDisk` answer after the `rm` was sent. The answers "another operation on this
VM is still running" and "the same copy is still running" stay "not performed": this
call did nothing. `HOST_CONNECTION_LOST` is added to the three host-loss answers above,
and changes nothing else about them: a `VirtualMachine` whose `Power` or `Reconfigure`
is answered with it is re-checked every 30 seconds, as for an unavailable host, instead
of every 5 seconds.

**Shared setup writes.** The cluster-wide disk guard's `HOST_UNAVAILABLE` concerns
**another** host of the Provider, and it can follow idempotent setup writes on the leased
host (the seed directory and the default storage pool of a `Create`, the varstore check of
a `Clone`). Those leave nothing a retry does not redo, and the guard says that nothing was
created, so the answer stays `HOST_UNAVAILABLE`.

Each routed RPC applies the rule, with these differences:

| RPC | When the host stops answering | What the manager does with it |
|---|---|---|
| `Describe`, `GetDiskInfo`, `TaskStatus` | Always `HOST_UNAVAILABLE`. These calls only read, so a lost connection cannot leave anything half done. | `Describe`: `Ready=False/HostUnavailable`, re-checked. |
| `ListVMs` | Unchanged: the host is named in `unreachable_host_ids`; the call does not fail. | |
| `ExportDisk` (`s3`, `nfs`) | The table above. A flatten or convert that is sent and whose session drops, that `timeout(1)` stops (124), that is killed (137), or that runs past the routed budget is unknown: the `ExportRequested` marker is kept, and the export is re-sent only as described [above](#what-happens-to-a-call-whose-outcome-is-unknown). A failure the host answered (a command that ended with an exit status of its own, an upload the S3 service refused before the disk was read to its end) is definite from v0.4.1, as on a single host ([Clustered libvirt: definite export failures](#clustered-libvirt-definite-export-failures)). | |
| `SnapshotCreate` | Before the `snapshot-create-as` is sent: `HOST_UNAVAILABLE` or not performed. After: the **retry form** ("its outcome is unknown and it may still complete on the host: retry"), not `Unknown`, **when the request names its snapshot and carries a request token** (the manager always sends both). The routed create is then idempotent: the retry finds the snapshot by name and token, or is refused by libvirt's job lock while the job runs. A request without both (a direct caller) gets `Unknown`, for its time-budget answer too, because its retry could take a second snapshot. | The retry form: retried, never failed, outcome unknown; the `VMSnapshot` or `VMMigration` marker rules above apply (the marker is kept). `HOST_UNAVAILABLE` or not performed, for a `VMSnapshot`: the attempt is closed (the marker is removed) and the create is sent again as a new attempt, after 1 minute, doubling for each consecutive such answer up to 10 minutes; never failed, never held. |
| `Clone` | Before the copy is sent, only reads: `HOST_UNAVAILABLE` or not performed. Any command not on the read list sent earlier (the `isoinfo` read of the source's seed, the varstore check, the copy's private write directory) already gives the retry form. After the copy is sent: the retry form, not `Unknown`: the clone is stamped with its target, so the retry finds a clone that completed, or is told that the earlier copy still runs (`flock`). An `Unknown` would fail the `VMClone` and remove its target. | Retried on the same host with the backoff (`CloneRetrying`). |
| `Power` | The table above. `on`, `off` and `shutdown` can be repeated: a second attempt finds the domain already in the state asked for. `reboot` cannot: an unknown `reboot` that is retried may reboot the VM twice. | A provider error, retried every 30 s (`HOST_CONNECTION_LOST`). |
| `Create`, `Delete`, `Reconfigure`, `SnapshotDelete`, `SnapshotRevert`, `TransferOwner` | The table above. A clustered `Delete` stops at a `destroy` whose session dropped, instead of going on to `undefine` a domain that may still run; it is retried. Any other failed `destroy` is ignored, as before. | `Delete`: finalizer kept, `DeleteBlocked=True/HostUnreachable` and `Ready=False/DeleteBlocked`, retried with a backoff (15 s doubling to 5 min) until the host answers; see [A delete held for an unreachable host](#a-delete-held-for-an-unreachable-host). `SnapshotDelete` of a `VMSnapshot`: finalizer kept, retried with a backoff of 15 s doubling to 5 min. `Reconfigure` and `Create`: a provider error, retried (a `Reconfigure` every 30 s); a failed `Reconfigure` counts the larger size on a clustered Provider. `TransferOwner`: the adoption fails and is retried. `SnapshotRevert` of a `VirtualMachine` (`spec.snapshot.revertToRef`): `status.snapshotRevert` stays `Reverting`, and the revert is sent again only while the VM is still powered off ([above](#what-happens-to-a-call-whose-outcome-is-unknown)). Each call can be repeated: a second attempt finds the domain, snapshot or stamp already in its final state (a `Create` binds the domain the first attempt made, which carries the VM's owner stamp). |
| `DeleteLandedDisk` | `HOST_UNAVAILABLE` only before the host answered a command. After reads were answered: `Unavailable` + `VM_OPERATION_FAILED`, "could not be checked or removed". After the `rm` was sent: the retry form, "it is not known whether it was removed". | Retried. A retry that finds the disk gone is `NOT_FOUND`. |
| `DeleteStagedDisk` (`nfs`) | `HOST_UNAVAILABLE` only for a host that cannot be leased. Every `qemu-img` failure, a dropped session included, is a plain `Unavailable` with a constant message (never `HOST_UNAVAILABLE`); this path is unchanged by R25. | The cleanup is retried. |

Three things stay as they were:

- A failure of the **operation** (the host answered, and the command failed) is
  `Unknown` + `VM_OPERATION_FAILED` with the historical message, whether or not it
  came after a read. Only a lost connection is classified by how far the call got.
- The disk-dependency guard runs before the VM's disks or domain are changed (it can
  follow the idempotent setup writes described under **Shared setup writes**). When it
  cannot reach the host it is `HOST_UNAVAILABLE` (fails closed), even after the owner
  check read the host.
- A single-host libvirt provider answers as before: it keeps no such record.

None of these answers counts toward the Provider's circuit breaker, except the plain
`Unavailable` of `DeleteStagedDisk`.

### A delete held for an unreachable host

A clustered `Delete` answered `HOST_UNAVAILABLE` whose `host` key is the VM's own
host (`status.placement.host`, or the pending host of a create in flight), or
answered with `HOST_CONNECTION_LOST`, is held: the finalizer is kept and the VM
shows `DeleteBlocked=True` with reason `HostUnreachable`, the same message on
`Ready=False/DeleteBlocked`, and one `Warning` event `DeleteBlocked`. The message
names the host by its `Host` name and says whether anything may have been
deleted:

- `HOST_UNAVAILABLE`, or `HOST_CONNECTION_LOST` that is "not performed": "Delete held:
  this VirtualMachine's host host-a could not be reached, so nothing was deleted. The
  finalizer is kept; the delete is retried with a backoff of up to 5m0s and completes
  once the host answers. ..."
- `HOST_CONNECTION_LOST` with an unknown outcome: "Delete held: this VirtualMachine's
  host host-a stopped answering during the delete, so it is not known how far the
  delete got. ..."

Both end with how to release the VM without the provider's delete
(`virtrigaud.io/orphan-on-delete` or `virtrigaud.io/force-delete`). A
`HOST_UNAVAILABLE` without the VM's own host in its `host` key (the cluster-wide
disk guard, an older provider) keeps the constant message that names no host. The
delete is retried with the blocked-VM backoff (15 s doubling to 5 min); when the
host answers again the delete completes and the finalizer is removed, and a later
attempt that fails for another reason removes `DeleteBlocked` (`Ready` then says
`ProviderError`). Up to v0.4.0-rc5 a `HOST_CONNECTION_LOST` delete set no condition
(`Ready` kept its last message), and the hold's message never named the host.

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
  - a snapshot create that lost its connection, was killed, failed with an error that is
    not a refusal before execution, or left the snapshot (or changed disk sources)
    behind is held for an operator;
  - an `s3` export whose flatten was killed, or that failed after its source was read to
    the end, is re-sent after the settle time;
  - an `nfs` export whose convert was killed, an import whose convert, `dd` or rename
    was killed or lost, and an import that failed after the disk landed, are held.
  A provider image older than rc2 still answers every libvirt failure untyped, and the
  manager holds each of them.
- **Host-unreachable failures of these three calls no longer count toward the circuit
  breaker.** A single-host libvirt provider whose host cannot be reached while a
  `SnapshotCreate`, `ExportDisk` or `ImportDisk` runs now answers "not performed", which
  is out of the breaker like every `VM_OPERATION_FAILED`. Other calls to the same
  provider (`Describe`, `Power`, ...) still count the failure.
- **A lost failure write is held.** A known failure whose status write itself is
  lost leaves the marker: a restart before the write, a write that fails for another
  reason than a conflict, and - for a `VMMigration` - a conflict on the write that
  fails the attempt. The object is then held for an operator, although the provider
  answered definitely. Confirming the attempt sends the call again. A conflict on the
  write of a `VMSnapshot`'s, a `VirtualMachine`'s or a single-host `VMClone`'s answer no
  longer loses it (v0.4.1, [above](#an-answer-is-not-lost-to-an-edit-made-during-the-call)).
- **A Proxmox clone refused because its source is locked is held as unknown until it is sent
  again.** When PVE refuses a `Clone` at once because the source VM is locked (for example while
  a snapshot of the source runs: `HTTP 500 VM is locked (snapshot)`), nothing was started, but the
  Proxmox provider answers "outcome unknown": it treats only a 400, a 401 or 403 and the VMID-taken
  refusal as definite, and any other answer may have started a task. The `VMClone`
  stays `Pending` with `Ready=False/CloneOutcomeUnknown` and the manager sends the `Clone` again
  after its backoff (15 seconds, doubling to 5 minutes); it completes on a re-send that finds the
  lock gone. This is not one of the VMID-race forms, which are answered "not performed"
  ([`Create` and `Clone` error classes](vm-ownership.md#create-and-clone-error-classes)).
- **A generic status write that conflicts with an edit is logged at `ERROR`.** The
  `VirtualMachine` controller's own status write (`Failed to update VirtualMachine status`) is
  not retried in place: if the object was edited during the reconcile, it fails with "the object
  has been modified", the log line is at `ERROR` level, and the next reconcile converges. A
  provider's answer is not affected (see
  [above](#an-answer-is-not-lost-to-an-edit-made-during-the-call)).
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
