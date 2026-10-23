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
| `Pending` | Not created. The VM waits for its `Provider`, class, image, a placement or (adoption) the controller that binds it. |
| `Provisioning` | Being created: a `Create` is requested (the create marker is on `Provisioning`), a task is recorded, a clustered host was chosen for it, or it was created and has not been described yet. |
| `Running` | Created, and the provider reports it `On`. A `Suspended` VM (paused, or suspended to RAM) is still active and reports `Running`; `Ready=False/PowerStateUnmanaged` says it is not managed until resumed. |
| `Stopped` | Created, and the provider reports it `Off`. |
| `Reconfiguring` | A resize is in flight: `Reconfiguring=True` (other than `RestartRequired`) or `status.reconfigureTaskRef` is set. A resize applied to the persistent definition only (`RestartRequired`) leaves the phase to the power state. Also while an asynchronous power task runs (see [A power change that returns a task](#a-power-change-that-returns-a-task)). |
| `Deleting` | The VM has a deletion timestamp, including while the delete is held (`Ready`/`DeleteBlocked` say why). |
| `Failed` | A hold that needs an operator, a create that was refused, or a power state the provider cannot classify. See below. |

## The rule

First match wins:

1. The VM is being deleted: `Deleting`.
2. `Ready=False` with a hold reason: `Failed`. The reasons are `VMNotOwned`,
   `OwnerStampUnreadable`, `ProviderUnavailable`, `VMIsImageArtifact`,
   `CreateOutcomeUnknown`, `DeleteBlocked`, `ProviderRefMismatch`,
   `PlacementTopologyMismatch`, `ConsumerNotAllowed`, `VMMissingOnHost`,
   `RestorePending` and `OwnDomainOnAnotherHost`. They hold whether or not the VM
   has an id.
3. No `status.id` (not created) and `Ready=False` with a failed-create reason
   (`CreateBackoff`, `CreateRefused`, `ProviderConflict`, `ValidationError`):
   `Failed`. On a VM that exists the same reasons describe a failed change to it,
   not the VM, and do not change its phase.
4. No `status.id` and a clustered restore hold on `Placed` or `Provisioning`
   (`RestorePending`, `OwnDomainOnAnotherHost`, `UniquenessCheckFailed`,
   `ProviderLacksListOwnerFilter`, `ImportedDiskPinned`; these are written to those
   two conditions, not to `Ready`, for a VM that was never placed): `Failed`.
5. No `status.id`: `Provisioning` if a create is requested, a clustered
   `pendingHost` is set, `status.lastTaskRef` is set or `Provisioning=True` (not
   `WaitingForDependencies`); otherwise `Pending`.
6. A resize in flight: `Reconfiguring`.
7. By `status.powerState`: `On` and `Suspended` are `Running`; `Off` is
   `Stopped`; `Unknown` is `Failed` (the current observation: it clears on the
   next reconcile that sees a state the provider can classify, so a VM that passes
   through an odd libvirt state shows `Failed` only for that long). With no power
   state observed yet the VM is
   `Running` if `Ready=True`, else `Provisioning`.

A transient provider error (`Ready=False/ProviderError`) on a VM that exists does
not change the phase: it keeps the last observed power state. An unschedulable
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

- A `Reconfiguring` condition of a real resize is never settled by a power task: only
  the exact reason and message above are, and only when no `status.reconfigureTaskRef`
  is recorded. `RestartRequired`, `ResizePendingPowerOff`, `ShrinkPendingPowerOff`,
  `InsufficientHostCapacity` and the reconfigure's own `Updating` and
  `TaskInProgress` conditions are left as they are.
- A power task that ends in failure is forgotten (`status.lastTaskRef` is cleared),
  and the failure is shown on `Ready=False/ProviderError` and
  `Reconfiguring=False/ProviderError` with the message `Power state change failed: ...`. The
  phase follows the power state. The next reconcile compares the power state with
  `spec.powerState` again and sends the power operation again if they still differ.
- An earlier manager (v0.4.0-rc4 and before) never settled the condition, so every
  Proxmox VM whose power was changed stayed in phase `Reconfiguring` with `Ready=True`.
  The first reconcile after the upgrade settles such a VM; no manual step is needed.
