# Operation grants: the reserved wire shape

The provider contract ([`proto/provider/v1/provider.proto`](../proto/provider/v1/provider.proto))
defines how a manager can authorise one mutating call that a per-host agent behind a
Provider executes: an **operation grant**. This page describes that shape.

**It is not enforced.** No bundled provider reads, checks, forwards or refuses an
operation grant, the manager never sends one, and every capability gate below defaults
to `false` and is `false` on every bundled provider (vSphere, libvirt single-host and
clustered, Proxmox, mock). The messages, field numbers and reason strings are reserved
so that later work can use them without a breaking change to the contract. The
manager-side code that mints, sends and verifies grants exists in development builds
only and is used by no controller; see
[`operation-grants-manager.md`](operation-grants-manager.md).

## Messages and enums

| Name | Kind | Purpose |
|---|---|---|
| `OperationEnvelope` | message | One authorised mutating request: `request` (1, the exact serialised request message) and `grant` (2). |
| `OperationGrant` | message | A signed `GrantBody`: `body` (1, the serialised `GrantBody`), `signature` (2) over those exact bytes, `signer_cert_chain` (3, DER certificates, leaf first). |
| `GrantBody` | message | The signed content of a grant: version, grant kind, installation, Provider, host, enrolment, authority generation and epoch (1–8); operation fields (10–18); administrative fields (20–25); fields 26, 28, 29 set aside for the authority-history protocol (defined, unset at grant version 1; field 27 is a proto `reserved` number); validity `not_after` (30) and `issued_at` (31). |
| `Epoch` | message | A manager epoch: `counter` (1) and the random `holder` (2) of the term that holds it. |
| `GrantPrecondition` | message | Host state a grant requires: `store_id`, `seq`, `head`, `quarantine_id`, `clock_floor` (1–5). |
| `GrantKind` | enum | What a grant authorises: `GRANT_KIND_OPERATION` and eleven other kinds (values 0–11, 0 is `UNSPECIFIED`). |
| `OperationKind` | enum | The mutating call a `GRANT_KIND_OPERATION` grant authorises (values 0–18, 0 is `UNSPECIFIED`). |

`GrantBody` field 27 and the name `ceremony_index` are `reserved`; numbers 9 and 19 are
unassigned. The comments in `provider.proto` are the normative description of every
field.

Each `OperationKind` names exactly one RPC and its request message, and the four Power
kinds also fix `PowerRequest.op`:

| `OperationKind` | RPC | Fixed request field |
|---|---|---|
| `CREATE` | `Create` | |
| `DELETE` | `Delete` | |
| `POWER_ON` / `POWER_OFF` / `REBOOT` / `SHUTDOWN_GRACEFUL` | `Power` | `op` = `POWER_OP_ON` / `POWER_OP_OFF` / `POWER_OP_REBOOT` / `POWER_OP_SHUTDOWN_GRACEFUL` |
| `RECONFIGURE` | `Reconfigure` | |
| `HARDWARE_UPGRADE` | `HardwareUpgrade` | |
| `SNAPSHOT_CREATE` / `SNAPSHOT_DELETE` / `SNAPSHOT_REVERT` | `SnapshotCreate` / `SnapshotDelete` / `SnapshotRevert` | |
| `CLONE` | `Clone` | |
| `IMAGE_PREPARE` | `ImagePrepare` | |
| `EXPORT` / `IMPORT` | `ExportDisk` / `ImportDisk` | |
| `DELETE_STAGED_DISK` / `DELETE_LANDED_DISK` | `DeleteStagedDisk` / `DeleteLandedDisk` | |
| `ADOPT` | `TransferOwner` | |

The read RPCs (`Validate`, `Describe`, `TaskStatus`, `GetCapabilities`, `GetDiskInfo`,
`ListVMs`, `ListHosts`, `GetHostInfo`) take no grant.

## Where the envelope attaches

Each of the 15 mutating request messages has an `OperationEnvelope operation_envelope`
field, at these field numbers:

| Request message | Field | Request message | Field |
|---|---|---|---|
| `CreateRequest` | 12 | `CloneRequest` | 11 |
| `DeleteRequest` | 4 | `ImagePrepareRequest` | 7 |
| `PowerRequest` | 6 | `ExportDiskRequest` | 13 |
| `ReconfigureRequest` | 5 | `ImportDiskRequest` | 14 |
| `HardwareUpgradeRequest` | 4 | `DeleteStagedDiskRequest` | 9 |
| `SnapshotCreateRequest` | 8 | `DeleteLandedDiskRequest` | 5 |
| `SnapshotDeleteRequest` | 5 | `TransferOwnerRequest` | 6 |
| `SnapshotRevertRequest` | 5 | | |

When the field is set, no other field of the request is set: the request travels as
the exact bytes inside the envelope, and the grant binds those bytes. A Provider
forwards `request` and `grant` byte for byte.

## Capability gates

`GetCapabilitiesResponse` has four gates. All default to `false`, and no bundled
provider reports any of them. The manager's transport client reads them into
`contracts.Capabilities` (`SupportsOperationIdentity`, `RequiresOperationIdentity`,
`SupportsFencing`, `RequiresFencing`), but no controller acts on them yet. They are
unauthenticated claims and never lower the protection a Provider's type requires:

| Field | No. | Means when `true` |
|---|---|---|
| `supports_operation_identity` | 31 | The Provider accepts `operation_envelope` and forwards it to a per-host agent that verifies the grant. |
| `requires_operation_identity` | 32 | The Provider refuses a mutating call that carries no valid grant (`IDENTITY_REQUIRED`). Only with `supports_operation_identity`. |
| `supports_fencing` | 33 | The agents also enforce host-scoped fencing: a grant is accepted only under an epoch the host has activated. Only with `supports_operation_identity`. |
| `requires_fencing` | 34 | The Provider refuses a grant not issued under fencing (`FENCING_REQUIRED`). Implies the two above. |

## Error reasons

Fifteen `google.rpc.ErrorInfo` reasons (domain `provider.virtrigaud.io`) are defined for
an agent's refusal of a grant. They are listed in
[`docs/task-status-outcomes.md`](task-status-outcomes.md#operation-grant-error-reasons).
No bundled provider emits them.

In code: `internal/providers/contracts` (`GrantMismatchReason` and the others, appended
to `PartAReasons`) and the SDK's `sdk/provider/errors` (`ReasonGrantMismatch` and the
others).

## Compatibility

- Additive on the wire: new messages, enums, one new field per mutating request and
  four new capability fields. Nothing existing is renamed, renumbered or retyped.
- A provider built against an older contract ignores the new fields; a manager built
  against an older contract never sends them.
- The messages live in `provider.proto`. The contract's breaking-change check
  (`make proto-breaking`, buf category `FILE`) treats moving a message to another file
  as breaking, so they stay there.
- `contracts.Capabilities` carries the four gates; the SDK's `sdk/provider/capabilities`
  has no constant for them. A provider author leaves the four fields `false`: no
  release build of the manager implements the protocol they advertise.
