# Typed task outcomes and per-host agent error reasons

A provider answers some calls (`Delete`, `Power`, `Reconfigure`, snapshot and clone
operations, `ImagePrepare`, `ExportDisk`) with a task reference, and the manager polls
it with the `TaskStatus` RPC. Until now the answer was two fields: `done` and `error`.
That cannot say "this task never ran", so a provider that knows a task did not happen
had no safe way to say it: `done` without an error is read as a success, and a
success of a `Delete` releases the `VirtualMachine`'s finalizer.

`TaskStatusResponse` now also carries a typed outcome (ADR-0016 A7). The change is
additive: the manager and every provider still read and fill `done` and `error`.

## The outcomes

`TaskStatusResponse.outcome` (enum `TaskOutcome`) and `outcome_reason` (a short reason
such as `never_received`, for logs only) are fields 3 and 4. A provider that sets
`outcome` must also fill `done` and `error` as this table says, so that a manager
that predates the field reads the same answer:

| `outcome` | Means | `done` | `error` |
|---|---|---|---|
| `TASK_OUTCOME_UNSPECIFIED` (0) | The provider reports no typed outcome. The manager reads `done` and `error` as before. | as before | as before |
| `TASK_OUTCOME_RUNNING` | Accepted, not terminal. | `false` | `""` |
| `TASK_OUTCOME_SUCCEEDED` | Every step is done and its effect was observed or recorded. It does not prove that the effect still exists. | `true` | `""` |
| `TASK_OUTCOME_FAILED_CLEAN` | A definite failure before any irreversible step. | `true` | the failure message |
| `TASK_OUTCOME_FAILED_DIRTY` | A definite failure after an irreversible step began: something may have happened. | `true` | the failure message |
| `TASK_OUTCOME_UNKNOWN` | Too little evidence to tell what the task did. Not terminal: never read as done. | `false` | `""` |
| `TASK_OUTCOME_RETIRED` | Provably never started and never will. It proves nothing about any other attempt. | `true` | `not performed: RETIRED{<reason>}` |
| `TASK_OUTCOME_CANCELLED` | Cancelled before any irreversible step; its compensations are done. | `true` | `not performed: CANCELLED{<reason>}` |

A retired or cancelled task carries an error on purpose: an older manager reads
`done` with an error as a failed task, never as a success, so it never releases a
finalizer for a `Delete` that did not happen.

## How the manager reads an answer

The manager's gRPC client (`internal/transport/grpc`) reads the typed outcome first:

| Answer | `TaskStatus` (`contracts.TaskStatus`) | `IsTaskComplete` |
|---|---|---|
| `UNSPECIFIED`, or a value this manager does not know | `IsCompleted` and `Error` from `done` and `error`, unchanged | as before: an error is `true` and `task failed: <error>`, otherwise `done` |
| `RUNNING`, `UNKNOWN` | not completed, no error (whatever `done` says) | `false`, no error |
| `SUCCEEDED` with `done` set and no `error` | completed, no error | `true`, no error |
| `SUCCEEDED` that `done` or `error` contradicts | read as `UNSPECIFIED` (never more permissive) | as `UNSPECIFIED` |
| `FAILED_CLEAN`, `FAILED_DIRTY` | completed, with the provider's error (or `<OUTCOME>{<reason>}`) | `true`, `task failed: ...` |
| `RETIRED`, `CANCELLED` | completed, with a non-empty error (`not performed: ...`) | `true` and a `contracts.ErrorTypeNotPerformed` error (`contracts.IsNotPerformed`), not retryable |

`contracts.TaskStatus` also carries `Outcome` and `OutcomeReason`. Every caller that
reads `IsCompleted` and `Error` alone therefore sees a retired or cancelled task as
a failure, never as a success.

The asynchronous `Delete` of a `VirtualMachine` (see
[`vm-status-phase.md`](vm-status-phase.md)) handles them so:

- `RETIRED` or `CANCELLED`: the `Delete` did not happen. The finalizer is kept,
  `DeleteTask` becomes `False` with reason `DeleteTaskFailed`, and the `Delete` is
  sent again after the failed-task backoff.
- `UNKNOWN`: the task stays recorded (`DeleteTask=True`) and is polled again. No
  `Delete` is sent again and the finalizer is kept.
- The text match that recognises "the provider does not know this task" (`task not
  found`, Proxmox VE's `does not report this task`, vSphere's purged-task fault)
  applies only to answers without a typed outcome.

## What each bundled provider reports

No bundled provider reports `RETIRED`, `CANCELLED`, `UNKNOWN` or `FAILED_DIRTY`:
none of them can prove those today.

| Provider | Typed outcomes | Left `UNSPECIFIED` |
|---|---|---|
| vSphere | vCenter task `success` → `SUCCEEDED`, `error` → `FAILED_CLEAN`, `queued` / `running` → `RUNNING` | an error while waiting for the task (it mixes a failed task with a task vCenter no longer knows), and an unexpected task state |
| Proxmox VE | running → `RUNNING`; stopped with exit status `OK` → `SUCCEEDED`, any other exit status → `FAILED_CLEAN` | a malformed task id, and a task PVE does not report |
| mock | running → `RUNNING`, done → `SUCCEEDED`, failed → `FAILED_CLEAN` | a task it does not know (`task not found`) |
| libvirt, single host | none | every answer: libvirt issues no task, so it knows nothing about the reference it is asked about (its answer stays `done`, as before) |
| libvirt, clustered | done → `SUCCEEDED`, not done → `RUNNING` | none (see below) |

`FAILED_CLEAN` here is the hypervisor's own report of a failed task. The manager
handles `FAILED_CLEAN` exactly as it handled a failed task before.

## A task a clustered host never issued: `UNKNOWN_TO_AGENT`

A clustered provider refuses a task reference it could not have issued (see
[`clustered-provider-inventory.md`](clustered-provider-inventory.md)). A reference that
is well formed and names a host the provider fronts, but names a task that host never
issued, is answered `NotFound` with a `google.rpc.ErrorInfo` of reason
`UNKNOWN_TO_AGENT` in domain `provider.virtrigaud.io`. It is never done, and never "not
performed": the absence of a record proves nothing about the task.

Every routed libvirt call is synchronous and returns no task, so clustered libvirt
answers every host-encoded reference this way once its host is leased. A host that
cannot be reached is still the host-scoped `HOST_UNAVAILABLE` (`Unavailable`), whose
`ErrorInfo` metadata `host` names the host the reference routed to. The reference
itself is never echoed.

The manager maps `UNKNOWN_TO_AGENT` to a `NotFound` marked
`contracts.ErrUnknownToAgent` (`contracts.IsUnknownToAgent`). The asynchronous
`Delete` treats it as any other answer that says the provider does not know the task:
no second `Delete`, and only an owner-checked `Describe` that reports the VM absent
releases the finalizer. The clustered conformance case `tasks.forged-inner-ref` is
Required and checks the code, the reason and the manager mapping (see
[`test/conformance/README.md`](../test/conformance/README.md)).

## Providers that require operation identity

For a Provider whose type requires operation identity (development builds only; no
bundled Provider type does), the VirtualMachine controller reads these answers for its
signed `Create`, `Delete`, power and `Reconfigure` attempts with the same outcome table,
with one difference for the `Delete`: an answer that finds no VM never releases the
finalizer. A `Delete` answered `NotFound` (or `VM_NOT_OWNED`) is held as
`OperationDeleteUnconfirmed`, `UNKNOWN_TO_AGENT` is held, and no `Describe` is used to
decide a release; only a confirmed `Delete`, orphan-on-delete or force-delete releases the
VM. Every other Provider keeps the behaviour described above. See
[`operation-grants-manager.md`](operation-grants-manager.md#delete-an-absent-vm-never-releases-the-finalizer).

## Per-host agent error reasons

These `google.rpc.ErrorInfo` reasons (domain `provider.virtrigaud.io`) are defined for
the answers of a per-host agent (ADR-0016). No bundled provider sends any of them yet,
except `UNKNOWN_TO_AGENT` from clustered libvirt's `TaskStatus`.

Every one of them is **breaker-exempt**: the manager keeps a status that carries one
of them out of the per-Provider circuit breaker, whatever its gRPC code (also
`Unavailable`, `DeadlineExceeded`, `Internal` and `Unknown`), as it does for
`HOST_UNAVAILABLE`. The provider answered, and the fact is about one host, VM or
operation. The same reason in another domain is not exempt.

A **definite not-performed** reason proves that the refused request did nothing. A
reason that is not one proves nothing about what the operation did, and is never read
as "nothing was done".

| Reason | Means | Definite not-performed |
|---|---|---|
| `UNKNOWN_TO_AGENT` | (`NotFound`) The host has no record of the task or operation asked about. | No |
| `VM_BUSY` | The host runs another, non-terminal operation for the same VM. | No (the request may re-deliver an attempt already accepted) |
| `PROVIDER_MISMATCH` | The request came from a Provider other than the one the host is enrolled with. | Yes |
| `UNATTESTED` | The host's agent has not yet proved the continuity of its store and refuses mutations. | No |
| `AGENT_LOST` | The host's agent lost, or cannot trust, its store and refuses mutations until it is re-enrolled. | No |
| `DIGEST_MISMATCH` | The request bytes the host received do not match the authorised digest. | Yes |
| `CANCEL_REFUSED` | A cancellation was refused: the operation passed an irreversible step, or its outcome is unknown. The operation goes on. | Only of the cancellation, never of the operation it names |
| `DEADLINE_EXCEEDED_NOT_PERFORMED` | The host refused to accept the operation because its acceptance deadline had passed. A plain `DeadlineExceeded` is never one. | Yes |

In code: `internal/providers/contracts` (`UnknownToAgentReason` and the others,
`PartAReasons`, `IsDefiniteNotPerformedReason`, `HasPartAReason`) and the provider
SDK's `sdk/provider/errors` (`ReasonUnknownToAgent` and the others, and
`WithReason` to attach one to a status). The manager only maps them today: apart from
the breaker exemption and `UNKNOWN_TO_AGENT`'s `NotFound` marking, no controller acts
on them yet.

### Operation-grant error reasons

These reasons are defined for a per-host agent's refusal of an operation grant (see
[`docs/operation-grants-wire-shape.md`](operation-grants-wire-shape.md)). They are
**not emitted by any built-in provider**. Like the reasons above, every one of them is
breaker-exempt, and `PartAReasons` lists them after the eight above.

| Reason | Means | Definite not-performed |
|---|---|---|
| `GRANT_UNAUTHENTICATED` | The operation grant's signature, certificate chain, certificate validity or authority generation did not verify. | Yes |
| `GRANT_MISMATCH` | The grant is authentic but does not fit this call: installation, Provider, host, enrolment, grant kind, operation kind, RPC, request type, grant version, or a field the version does not define. | Yes |
| `NONCE_CONSUMED` | The grant's single-use nonce was already used. | Yes, of this delivery; says nothing about the earlier use of the nonce |
| `REQUEST_CONSUMED` | The approved request was already executed under this enrolment. | Yes, of this delivery; says nothing about the earlier execution |
| `IDENTITY_REQUIRED` | The Provider or host requires an operation grant and the call carried none. | No |
| `FENCING_REQUIRED` | The Provider or host requires a grant issued under fencing. | No |
| `STALE_EPOCH` | The grant's epoch is older than the epoch the host has activated. | No |
| `STALE_ISSUED` | The grant was issued below the floor that the host has recorded for the Provider. | No |
| `EPOCH_NOT_ACTIVATED` | The grant's epoch has not been activated on the host. | No |
| `JUMP_TOO_LARGE` | An activation would raise the host's epoch by more than its bound. | No |
| `QUARANTINED` | The host is quarantined for this Provider and refuses mutations. | No |
| `OP_ID_CONFLICT` | The host knows the grant's `op_id` with different identity fields. | No |
| `BAD_NONCE` | The nonce is not one the host issued, or it has expired. | No |
| `VM_BINDING_STALE` | The grant's binding is not the VM's current placement on the host. | No |
| `CLOCK_UNCORROBORATED` | The host refused an attestation because it could not corroborate its clock. | No |

In code: `GrantUnauthenticatedReason` and the others in `internal/providers/contracts`,
`ReasonGrantUnauthenticated` and the others in `sdk/provider/errors`.

## For provider authors

The SDK's `sdk/provider/tasks` package builds a `TaskStatusResponse` from an outcome,
with `done` and `error` filled from it:

```go
return tasks.Status(providerv1.TaskOutcome_TASK_OUTCOME_FAILED_CLEAN, "", "disk full"), nil
```

Set an outcome only when the provider can prove it. A provider that cannot type an
answer leaves `outcome` unset (`UNSPECIFIED`) and fills `done` and `error` as before.
`outcome_reason` and the error text must never contain a secret, an endpoint or a
credential. The SDK client's `WaitForTask` reads the typed outcome first: `RETIRED`
and `CANCELLED` end the wait with an error, `RUNNING` and `UNKNOWN` keep it polling.

## Compatibility and upgrade

- Additive on the wire. A provider that predates the fields sends `UNSPECIFIED`, and
  the manager reads its `done` and `error` exactly as before.
- A manager that predates the fields ignores them and reads `done` and `error`, which
  every bundled provider fills as the table above says.
- Behaviour change for clustered libvirt: a host-encoded task reference to a host it
  fronts used to be answered `done`; it is now `NotFound` with `UNKNOWN_TO_AGENT`. No
  routed libvirt call returns a task, so the manager never polls such a reference in
  normal operation.
- No CRD, Helm or configuration change.
