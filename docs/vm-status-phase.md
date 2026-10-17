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
| `Reconfiguring` | A resize is in flight: `Reconfiguring=True` (other than `RestartRequired`) or `status.reconfigureTaskRef` is set. A resize applied to the persistent definition only (`RestartRequired`) leaves the phase to the power state. |
| `Deleting` | The VM has a deletion timestamp, including while the delete is held (`Ready`/`DeleteBlocked` say why). |
| `Failed` | A hold that needs an operator, a create that was refused, or a power state the provider cannot classify. See below. |

## The rule

First match wins:

1. The VM is being deleted: `Deleting`.
2. `Ready=False` with a hold reason: `Failed`. The reasons are `VMNotOwned`,
   `OwnerStampUnreadable`, `ProviderUnavailable`, `VMIsImageArtifact`,
   `CreateOutcomeUnknown`, `DeleteBlocked`, `ProviderRefMismatch`,
   `PlacementTopologyMismatch`, `ConsumerNotAllowed`, `VMMissingOnHost`,
   `OwnDomainOnAnotherHost`, `UniquenessCheckFailed` and
   `ProviderLacksListOwnerFilter`. They hold whether or not the VM has an id.
3. No `status.id` (not created) and `Ready=False` with a failed-create reason
   (`CreateBackoff`, `CreateRefused`, `ProviderConflict`, `ValidationError`):
   `Failed`. On a VM that exists the same reasons describe a failed change to it,
   not the VM, and do not change its phase.
4. No `status.id`: `Provisioning` if a create is requested, a clustered
   `pendingHost` is set, `status.lastTaskRef` is set or `Provisioning=True` (not
   `WaitingForDependencies`); otherwise `Pending`.
5. A resize in flight: `Reconfiguring`.
6. By `status.powerState`: `On` and `Suspended` are `Running`; `Off` is
   `Stopped`; `Unknown` is `Failed`. With no power state observed yet the VM is
   `Running` if `Ready=True`, else `Provisioning`.

A transient provider error (`Ready=False/ProviderError`) on a VM that exists does
not change the phase: it keeps the last observed power state. An unschedulable
clustered VM stays `Pending`.

## Why it is derived on every write

Earlier releases assigned the phase on a few code paths only (a reconfigure), so
most VMs had none. Now every status write of the VirtualMachine controller (and
the pre-create marker, the post-create record and the clustered pending-host
records) recomputes it from the status being written, so it changes in the same
write as the fact it summarizes and never causes a write by itself: an unchanged
reconcile writes nothing. When a delete begins, the controller writes `Deleting`
once, with a status merge patch that changes nothing else.

## Upgrade notes

The first reconcile after the manager is rolled fills in the phase of every
existing VM (one status write per VM that changes). There is no CRD schema change
(the `phase` enum is unchanged), so no CRD ordering applies. Tooling that read
`status.phase` must expect every value above.
