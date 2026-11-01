# VirtualMachine `status.phase`

`kubectl get vm` shows `status.phase` in its `PHASE` column. The manager sets it
on **every** status write of a `VirtualMachine`, as a function of the rest of the
status, so it is never empty on a VM the manager reconciles and it never disagrees
with the conditions. Do not set it by hand: the next reconcile overwrites it.

The phase is a summary. The `Ready`, `Provisioning`, `Reconfiguring` and
`Placed` conditions carry the reason; `status.powerState` carries the power state
as the provider reports it.

## Values

| Phase | Meaning |
|---|---|
| `Pending` | Not created. The VM waits for its `Provider`, class, image, a placement or (adoption) the controller that binds it. Also a clustered VM held by a restore guard (R1/R4: `RestorePending`, `OwnDomainOnAnotherHost`, `UniquenessCheckFailed`, `ProviderLacksListOwnerFilter`), created or not: it waits and proceeds once the cause is gone. |
| `Provisioning` | Being created: a `Create` is requested (the create marker is on `Provisioning`), a task is recorded, a clustered host was chosen for it, or it was created and has not been described yet. |
| `Running` | Created, and the provider reports it `On`. A `Suspended` VM (paused, or suspended to RAM) is still active and reports `Running`; `Ready=False/PowerStateUnmanaged` says it is not managed until resumed. |
| `Stopped` | Created, and the provider reports it `Off`. |
| `Reconfiguring` | A resize is in flight: `Reconfiguring=True` (other than `RestartRequired`) or `status.reconfigureTaskRef` is set. A resize applied to the persistent definition only (`RestartRequired`) leaves the phase to the power state. Also while an asynchronous power task runs (see [A power change that returns a task](#a-power-change-that-returns-a-task)). |
| `Deleting` | The VM has a deletion timestamp, including while the delete is held (`Ready`/`DeleteBlocked` say why). |
| `Failed` | A hold that needs an operator (other than a restore guard's), a create that was refused or is retried with a backoff (`CreateBackoff`, also when the provider did not perform it), or a power state the provider cannot classify. See below. |

## The rule

First match wins:

1. The VM is being deleted: `Deleting`.
2. `Ready=False` with a hold reason: `Failed`. The reasons are `VMNotOwned`,
   `OwnerStampUnreadable`, `ProviderUnavailable`, `VMIsImageArtifact`,
   `CreateOutcomeUnknown`, `DeleteBlocked`, `ProviderRefMismatch`,
   `PlacementTopologyMismatch`, `ConsumerNotAllowed` and `VMMissingOnHost`. They
   hold whether or not the VM has an id.
3. No `status.id` (not created) and `Ready=False` with a failed-create reason
   (`CreateBackoff`, `CreateRefused`, `ProviderConflict`, `ValidationError`):
   `Failed`. On a VM that exists the same reasons describe a failed change to it,
   not the VM, and do not change its phase.
4. A clustered restore guard's hold (ADR-0007 A6, R1/R4): `Pending`. It is
   `Ready=False/RestorePending` (a bound VM whose restore marker names another
   UID), or, with no `status.id`, `Placed=False` or `Provisioning=False` with
   `RestorePending`, `OwnDomainOnAnotherHost`, `UniquenessCheckFailed` or
   `ProviderLacksListOwnerFilter` (written to those two conditions, not to
   `Ready`, for a VM that was never placed). The VM is held, not failed: it
   proceeds once the cause is gone ([Backup and restore of clustered
   VirtualMachines](clustered-restore.md#conditions)). Up to v0.4.0-rc5 these holds
   showed `Failed`.
5. No `status.id` and `Placed=False` or `Provisioning=False` with
   `ImportedDiskPinned` (a migration target whose create on the host its disk
   landed on met a name conflict an operator must resolve): `Failed`.
6. No `status.id`: `Provisioning` if a create is requested, a clustered
   `pendingHost` is set, `status.lastTaskRef` is set or `Provisioning=True` (not
   `WaitingForDependencies`); otherwise `Pending`.
7. A resize in flight: `Reconfiguring`.
8. By `status.powerState`: `On` and `Suspended` are `Running`; `Off` is
   `Stopped`; `Unknown` is `Failed` (the current observation: it clears on the
   next reconcile that sees a state the provider can classify, so a VM that passes
   through an odd libvirt state shows `Failed` only for that long). With no power
   state observed yet the VM is
   `Running` if `Ready=True`, else `Provisioning`.

A transient provider error (`Ready=False/ProviderError`) on a VM that exists does
not change the phase: it keeps the last observed power state. Neither does a
snapshot revert that is pending, running or failed (`Ready=False` with a reason of
the `SnapshotReverted` condition, such as `WaitingForPowerOff`, `Reverting` or
`RevertFailed`; see [`vm-snapshot-revert.md`](vm-snapshot-revert.md)): the phase
follows the power state, so a VM whose revert runs (it is off) shows `Stopped`, and one
that waits to be powered off shows `Running`. An unschedulable
clustered VM stays `Pending`. The target VM of a migration into a clustered Provider
that waits for its import (`Provisioning=False/WaitingForImport`) has a pending host,
so it shows `Provisioning`; one held as `MigrationNotFound` is not (re)placed, so it
shows `Pending` unless it was placed earlier, in which case it keeps its placement and
reservation and shows `Provisioning` ([Migration into a clustered provider](clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)).

## Why it is derived on every write

Earlier releases assigned the phase on a few code paths only (a reconfigure), so
most VMs had none. Now every status write of the VirtualMachine controller (and
the pre-create marker, the post-create record and the clustered pending-host
records) recomputes it from the status being written, so it changes in the same
write as the fact it summarizes and never causes a write by itself: an unchanged
reconcile writes nothing. When a delete begins, the controller writes `Deleting`
once, with a status merge patch that changes nothing else and is refused (and
retried a second later on a fresh read) if the object it holds is not the stored
one any more.

Paths that write only when a hold changed the status compare the status after the
phase is derived, so a VM that was already held when the manager was upgraded gets
its phase on its first reconcile. A VM written by the adoption or clone
controllers (which set `status.id` out of band) gets its phase on the next
regular VirtualMachine reconcile, not from those writes.

## Upgrade notes

The first reconcile after the manager is rolled fills in the phase of every
existing VM (one status write per VM that changes). There is no CRD schema change
(the `phase` enum is unchanged), so no CRD ordering applies. Tooling that read
`status.phase` must expect every value above.

## A power change that returns a task

A provider whose power operations are asynchronous (Proxmox answers a task id) makes
the manager record the task in `status.lastTaskRef` and set `Reconfiguring=True`
with reason `Updating` and the message `Adjusting power state`, so the VM shows
`Reconfiguring` while the power task runs. When the task has ended, the manager
sets that condition `False` (reason `ReconcileSuccess`, message `Power state
adjusted`) and the phase follows the power state again: `Running` or `Stopped`.
Providers whose power operations are synchronous (vSphere, libvirt) never set it.

- **A real reconfigure's condition is kept.** The marker is set only when
  `Reconfiguring` is absent or settled (`False` with `ReconcileSuccess` or
  `PowerTaskFailed`) and no `status.reconfigureTaskRef` is recorded. A power
  operation sent while `Reconfiguring` is `True` (a reconfigure in flight,
  `ResizeReserved`, `RestartRequired`) or `False` with any reason other than `ReconcileSuccess` and
  `PowerTaskFailed` (`ProviderError`, `ResizePendingPowerOff`,
  `ProviderLacksHonestReconfigure`, `ValidationError`, `ShrinkPendingPowerOff`,
  `InsufficientHostCapacity`, ...) leaves that condition as it is, and the
  completion of the power task does not settle it: only the exact reason and
  message above are settled, and only when no `status.reconfigureTaskRef` is
  recorded.
- **A power task that ends in failure** is forgotten (`status.lastTaskRef` is
  cleared) and shown on `Ready=False/ProviderError` with the message `Power state
  change failed: ...`. The marker, if it is on the VM, is settled as
  `Reconfiguring=False/PowerTaskFailed`, not `ProviderError`: a failed power task
  is not a failed Reconfigure, and `ProviderError` on `Reconfiguring` makes the
  manager send a Reconfigure again. The phase follows the power state.
  `Reconfiguring=False/PowerTaskFailed` is settled to `ReconcileSuccess` once the
  VM is observed in the power state its spec asks for.
- **A task the provider cannot report on.** A task-status query that fails for a
  transient reason (a transport error, a timeout, a 5xx, the token, rate limiting)
  keeps the task recorded and is asked again. For Proxmox, a query PVE rejects as a
  request (a malformed or unknown UPID: any 4xx other than 401/403/429, or a 5xx
  that says there is no such task) ends the task with an error, so the manager
  stops asking; a `VMMigration` whose snapshot task cannot be polled fails the
  snapshot once the task's wait limit has passed.
- **The power operation is sent again on a backoff**, per VM: after 5 seconds, then
  10, doubling to at most 5 minutes while its task keeps failing. A power
  operation that completes resets the backoff, and so does a change to `spec`
  since the failure (the operation then goes at once). While the VM waits, nothing is
  sent and the status does not change. The backoff is held by the manager process:
  after a manager restart the first retry goes at once.
- A task that ends in failure is recognised as a power task by the marker, or, when
  the marker was not set, by the manager process remembering the task its last
  power operation returned. After a restart of the manager, a failed power task
  without the marker is handled like any other failed task: `Provisioning=False/
  ProviderError` ("Failed to check task: ...") with the task still recorded.
- An earlier manager (v0.4.0-rc4 and before) never settled the condition, so every
  Proxmox VM whose power was changed stayed in phase `Reconfiguring` with `Ready=True`.
  The first reconcile after the upgrade settles such a VM; no manual step is needed.

## A delete that returns a task

The VirtualMachine finalizer is removed when the provider confirms that the
hypervisor VM is deleted. A provider whose `Delete` completes synchronously (an
empty task reference) confirms that when the call returns, and the finalizer is
removed at once, with no condition and no extra status write. None of the bundled
production providers (vSphere, libvirt single-host and clustered, Proxmox VE)
returns a `Delete` task today. A provider that does (the mock provider, or a
third-party provider) gets the following behaviour. Before this change the manager
released the finalizer as soon as the task reference came back, before the task
had run.

The state is the `DeleteTask` condition together with `status.lastTaskRef`, so a
new manager instance (after a restart or a leader change) carries on from where the
previous one stopped.

- **The task is recorded first.** The reference goes into `status.lastTaskRef`,
  with `DeleteTask=True` (reason `TaskInProgress`) and `Ready=False/Deleting`. The
  status write needs no resourceVersion and is retried, so an edit to the VM
  during the `Delete` cannot make it conflict. The manager also keeps the task in
  memory. If the write does not land, or the cache does not show it yet, the next
  reconcile writes it again before it polls the task, and no second `Delete` is
  sent (Warning event `DeleteTaskNotRecorded`).
- **The task is polled, not sent again.** While `DeleteTask=True` the manager
  calls `TaskStatus` for the recorded reference. A poll writes no status unless
  something changed. The poll interval is 5 seconds and grows with the task's age
  to at most 1 minute.
- **Completed without an error:** the finalizer is removed. The task's success is
  the provider's confirmation, exactly as a successful synchronous `Delete` is.
- **Status unknown:** a `TaskStatus` call that fails for a transient reason (a
  transport error or a timeout) is never taken for a deleted VM. `DeleteTask` stays
  `True` with reason `DeleteTaskStatusUnknown`, the task stays recorded, no
  `Delete` is sent, and the status is asked again after 15 seconds, doubling to
  5 minutes (Warning event `DeleteTaskStatusUnknown` once). This backoff is held
  in memory and starts again from 15 seconds after a manager restart.
- **The provider does not know the task.** The provider may answer the reference
  not-found, invalid or unsupported, or end it with an error that says it does
  not know the task: the mock provider's `task not found`, or Proxmox VE's
  `does not report this task` and `invalid task ID format`. That is typically a
  provider that restarted and lost its task table, and the destroy may still be
  running. The answer is ambiguous, so `DeleteTask` becomes `Unknown` with reason
  `DeleteTaskOutcomeUnknown` (Warning event of the same name) and **no `Delete`
  is sent again**. The manager asks the provider instead, with the same
  owner-checked `Describe` it uses for a running VM:
  - Reported absent (`exists: false`, or not-found): the finalizer is removed,
    with a `DeleteConfirmedAbsent` event.
  - Reported as another owner's VM (`VM_NOT_OWNED`): the finalizer is removed, as
    for a synchronous `Delete` answered `VM_NOT_OWNED` (`VMLeftOnHypervisor` event).
  - Still present, or no answer: the finalizer is kept, and the VM is checked
    again with a backoff from when the outcome became unknown: 15 seconds,
    growing to 5 minutes. An operator decides with `orphan-on-delete` or
    `force-delete`.
- **Failed:** a task the provider explicitly ends with any other error is cleared
  from `status.lastTaskRef`, and `DeleteTask=False/DeleteTaskFailed` is set
  (Warning event `DeleteTaskFailed`). The `Delete` is sent again once a wait has
  passed, counted from when the task failed. The wait equals the time the
  deletion had already taken then, between 15 seconds and 5 minutes, so it
  roughly doubles with each failure. It is anchored on the condition's
  `lastTransitionTime`, so a manager restart does not shorten it. Sending the
  `Delete` again is the manager's choice, the same retry a failed synchronous
  `Delete` gets. It is not a guarantee of the provider contract. It relies on the
  provider answering not-found for a VM that is already gone and refusing a VM
  at the id that belongs to someone else (`VM_NOT_OWNED`), as the bundled
  providers do. When the new attempt fails or is refused, its own condition
  replaces `DeleteTaskFailed`.
- **Provider object deleted:** a VM with no `Delete` task whose Provider object
  is gone is released as before. A VM with an unresolved task is kept instead,
  with `DeleteTask` reason `DeleteTaskProviderNotFound` (Warning event of the
  same name), because only the provider could confirm the task. When the Provider
  object is re-created, the task is observed again.
- **Recovery.** Every message names the two escape hatches.
  `virtrigaud.io/orphan-on-delete=true` detaches the VirtualMachine without any
  provider call, as before. `virtrigaud.io/force-delete=true` releases it once the
  delete is not confirmed: running, status unknown, outcome unknown, failed, or
  Provider gone. The usual `VMLeftOnHypervisor` Warning event is recorded. A
  provider that cannot be reached is handled as it always was
  (`ProviderUnavailable`), and the recorded task is kept.

A second `Delete` for the same VM can be sent in these cases:

1. The manager stops after the provider accepted the `Delete` and before the task
   record was stored. A new manager instance finds no record and sends the
   `Delete` again.
2. The provider explicitly ended the task with an error (`DeleteTaskFailed`). The
   `Delete` is sent again after the wait above.
3. A `Delete` that fails synchronously is retried, as before this change (every
   15 seconds, or held for the reasons in
   [provider-call-outcomes.md](provider-call-outcomes.md)).

No other path sends a second `Delete`. That includes a transient `TaskStatus`
error, a task the provider does not know, a VM still present after that, and a
Provider object that is gone.

Two dormant provider details matter only if a provider starts returning
`Delete` tasks:

- vSphere's `TaskStatus` reports any error waiting for a task, transient ones
  included, as the task ended with an error. A vSphere `Delete` task would
  therefore be taken as failed, and its `Delete` sent again, after a transient
  error.
- A single-host libvirt provider's `TaskStatus` reports every reference as
  completed, whatever it is. A libvirt `Delete` task would release the finalizer
  on the first poll.

Neither provider returns a `Delete` task today.
