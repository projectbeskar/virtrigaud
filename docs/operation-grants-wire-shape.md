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

## Signed host answers

A grant lets a manager authorise a mutating call without trusting the Provider that
relays it. A **signed host answer** is the other direction: the per-host agent behind a
Provider signs what it answers about one operation, one VM placement or itself, so a
manager can act on the answer without trusting the Provider either. The manager puts a
fresh random nonce in a read request; the agent signs an answer body that echoes the
nonce and names its enrolment and the position of its operation store; the Provider
relays the signed bytes unchanged.

**This too is shape only.** No bundled provider reads the new request fields or sets a
`host_answer`, and the manager sets none of the request fields and ignores every
`host_answer`. The messages are defined in their own file,
[`proto/provider/v1/host_answer.proto`](../proto/provider/v1/host_answer.proto) (package
`provider.v1`, which `provider.proto` imports); its comments are the normative
description of every field.

### Messages and enums

| Name | Kind | Purpose |
|---|---|---|
| `SignedBlob` | message | A signed message as the exact bytes that were signed: `body` (1), `signature` (2), `signer_certificate` (3, the DER certificate of the signing key), `body_type` (4, the full name of the message in `body`). |
| `AnswerContext` | message | What binds an answer to one request and to the host's state: `nonce` (1), `provider_uid`, `host_id`, `install_id`, `enrolment_id` (2–5), `chain_position` (6, store identifier, sequence number and chain head, 56 bytes), `agent_state` and `state_reason` (7, 8), `agent_time` (9, informational), and the continuity fields `continuity_head`, `checkpoint_seq`, `checkpoint_head` (10–12). Every answer body carries one. |
| `OperationAnswer` | message | The answer about one operation: `context` (1), the `op_id` and `op_enrolment_id` asked about (2, 3), `state` (4), `tombstoned` (5), the recorded identity `kind`, `attempt`, `vm_uid`, `binding_id`, `intent_digest` (6–10) and `outcome_reason` (11). `kind` is the number of the recorded `OperationKind`, carried as an `int32` because `provider.proto` imports this file. |
| `VMAnswer` | message | The answer about one VM placement: `context` (1), the `vm_uid` and `binding_id` asked about (2, 3), `presence` (4), and for a completed Delete `delete_op_id`, `delete_enrolment_id`, `delete_seq` (5–7). |
| `HostAnswer` | message | The answer about the host itself: `context` (1), `agent_version` (2, informational), `agent_api_version` (3), `grant_versions` (4, ascending), `mode` (5), `journal_schema_version` (6), `clock_floor` (7, informational), `staged_certificate_sha256` (8). |
| `AgentState` | enum | `UNATTESTED`, `READY`, `QUARANTINED` (values 0–3, 0 is `UNSPECIFIED`). `READY` means the store's continuity is established and the recovery of interrupted operations has run. |
| `OperationRecordState` | enum | `ACCEPTED`, `EXECUTING`, `COMPLETED`, `FAILED_CLEAN`, `FAILED_DIRTY`, `OUTCOME_UNKNOWN`, `RETIRED` (1–7: what the store records for an operation it holds a record or tombstone of), `NO_RECORD` and `UNKNOWN_TO_AGENT` (8, 9: it holds neither; neither proves anything about the operation). |
| `VMPresence` | enum | `CREATING`, `PRESENT`, `DELETED`, `OTHER_BINDING`, `NO_RECORD` (values 0–5, 0 is `UNSPECIFIED`). `NO_RECORD` never means that the VM was deleted. |
| `AgentMode` | enum | `IDENTITY` (values 0–1, 0 is `UNSPECIFIED`). |

### Where each answer travels

| `body_type` | Carried in | Answers |
|---|---|---|
| `provider.v1.OperationAnswer` | `TaskStatusResponse.host_answer` (5) | `TaskStatus` |
| `provider.v1.VMAnswer` | `DescribeResponse.host_answer` (8) | `Describe` |
| `provider.v1.HostAnswer` | `HostInfo.host_answer` (12); the three responses of the per-host agent service | `GetHostInfo`, the agent service |

`body_type` is a closed set of these three strings; a verifier refuses any other value,
and any member other than the one expected for the RPC that carried it. A `HostInfo`
in a `ListHosts` answer never carries a `host_answer`: `ListHostsRequest` has no nonce.
When an agent answers a read that carried a nonce with an error status (for example
`NotFound` with the `UNKNOWN_TO_AGENT` reason), the `SignedBlob` travels as exactly one
status detail of type `type.googleapis.com/provider.v1.SignedBlob`, beside the
`ErrorInfo`, encoded in at most 16 KiB.

### Request and response fields

Thirteen additive fields on six existing messages:

| Message | Field | No. | Type |
|---|---|---|---|
| `TaskStatusRequest` | `answer_nonce` | 2 | bytes |
| | `op_id` | 3 | bytes |
| | `op_enrolment_id` | 4 | bytes |
| | `target_host_id` | 5 | string |
| | `continuity_seq` | 6 | uint64 |
| `TaskStatusResponse` | `host_answer` | 5 | `SignedBlob` |
| `DescribeRequest` | `answer_nonce` | 4 | bytes |
| | `binding_id` | 5 | bytes |
| | `continuity_seq` | 6 | uint64 |
| `DescribeResponse` | `host_answer` | 8 | `SignedBlob` |
| `GetHostInfoRequest` | `answer_nonce` | 2 | bytes |
| | `continuity_seq` | 3 | uint64 |
| `HostInfo` | `host_answer` | 12 | `SignedBlob` |

- `answer_nonce` is exactly 16 random bytes, fresh for each request. An agent signs an
  answer only for a request that carries one; mutating requests carry nothing beside
  the operation envelope, so they never get a signed answer.
- A signed `TaskStatus` is addressed by operation, not by task reference: it sets
  `op_id`, `op_enrolment_id` (the enrolment the operation's grants named) and
  `target_host_id` (the host that ran, or was sent, the operation), with `task` unset.
  A Provider routes such a request by `target_host_id` and refuses one that also sets
  `task`. A plain `TaskStatus` is routed by its task reference, as before.
- A signed `Describe` names the placement it asks about in `binding_id`, together with
  the VM's `owner`.
- `continuity_seq` is a sequence number of the host's operation store at which the
  manager has already seen a signed chain head; the agent answers with its own head at
  that sequence number (`AnswerContext.continuity_head`), so a manager can tell a store
  that was rolled back or forked from one that moved forward. 0 asks for nothing.
- The request fields are set only for a Provider whose type requires operation
  identity. They carry no meaning for other Providers: a `Describe` or `GetHostInfo`
  is answered as without them, and a `TaskStatus` is answered from `task` alone, so
  one without `task` is refused or fails there as it does today.

### The signature input

A verifier checks `signature` over the signature input built from `body_type` and
`body`, and only then decodes `body`. It never re-encodes `body`. The signature input
is:

```
"virtrigaud.host-signed.v1"  (25 ASCII bytes)
0x00                          (1 byte)
len(body_type)                (2-byte big-endian unsigned integer)
body_type
len(body)                     (4-byte big-endian unsigned integer)
body
```

The signature is ECDSA P-256 over the SHA-256 digest of that input, ASN.1 DER encoded,
with `s` at most half the curve order. There is no algorithm field: the algorithm is
fixed by the type of the key in `signer_certificate`, and the only type accepted is
ECDSA P-256. Limits: `body_type` at most 64 bytes, `body` at most 64 KiB, `signature`
at most 160 bytes, `signer_certificate` at most 8 KiB.

Test vector (it pins the framing only; its body is not a complete answer, so a verifier
refuses it):

| | Value |
|---|---|
| `body_type` | `provider.v1.HostAnswer` (22 bytes) |
| `body` | `12 06 76 30 2e 30 2e 31` (`agent_version` = `"v0.0.1"`, 8 bytes) |
| signature input (62 bytes, hex) | `766972747269676175642e686f73742d7369676e65642e763100` `0016` `70726f76696465722e76312e486f7374416e73776572` `00000008` `120676302e302e31` |
| SHA-256 of the input | `92b2ed124ec68b2661cef2f6188a1f937121db3f0d25bb0ae8b98319bd696cea` |

The input starts with the byte `0x76`, which is not a valid protobuf tag, so it never
parses as a protobuf message (a `GrantBody` in particular, whose signature covers the
plain serialised body).

### The signing key

These are requirements on a per-host agent; no agent implements them yet.

- The signing key must be the host's enrolled identity key, generated on the host and
  never leaving it (after a renewal is put in use, the renewed key of the same
  enrolment), and `signer_certificate` must be the host's current identity certificate.
  The certificate's issuer, a dedicated host-agent CA, is a trust anchor the verifier
  is configured with and is never sent.
- The same key is the agent's TLS server key. The agent's TLS server must accept
  TLS 1.3 only, and the agent must use the key for nothing but TLS 1.3 handshakes,
  certificate requests and host-signed messages.
- The host-agent CA hierarchy and the grant-signing hierarchy must be separate: no
  certificate and no ancestor in common, so that whoever can issue host certificates
  can never issue a grant signer, and the reverse.

Accepted risk: because one key serves both TLS and signed answers, a compromise of the
TLS-serving process of an agent can produce answers that verify for that host's VMs;
keeping the key in a separate signer process (see
[`per-host-agent-development.md`](per-host-agent-development.md#host-key-signer)) limits
key exfiltration, not this online forgery. A separate answer key, held by a process that
never terminates TLS, is required before a regulated deployment relies on signed host
answers.

### What a manager will verify

When a manager uses these answers, for a Provider whose type requires operation
identity only, it will treat an answer that fails any check, and a missing answer, as
**no answer**: never as evidence that something happened or did not happen. In order,
before acting on an answer it will check:

1. the sizes above, and that the `SignedBlob` has no unknown fields;
2. that `body_type` is the one expected for the RPC;
3. the certificate: one ECDSA P-256 leaf for TLS server use, chaining to the host-agent
   CA (never to a grant anchor), naming the host the manager chose for this request,
   and whose key is pinned for that host;
4. the signature over the signature input;
5. that `body` decodes with no unknown fields or enum values and its fields are well
   formed;
6. that the nonce is the one it drew for this request, and that the Provider,
   host, installation and enrolment in the answer are the expected ones;
7. that the store position does not move backwards, fork or change store under the
   same enrolment;
8. that the answer is about the operation or placement it asked about, as the
   manager's own authenticated record of that operation names it.

Only answers from an agent in `READY` state can be definite. `NO_RECORD`,
`UNKNOWN_TO_AGENT`, `OTHER_BINDING`, `CREATING` and `PRESENT` never release a Delete
finalizer.

### The per-host agent service

[`proto/agent/v1/agent.proto`](../proto/agent/v1/agent.proto) (package `agent.v1`, Go
package `github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1`) defines
`AgentService`, which a per-host agent serves to the Provider that routes to it:
`Hello` (the first call on each connection, an API version check),
`InstallRenewedCertificate` (two-phase: stage a renewed certificate, then put the
byte-identical certificate in use) and `AgentStatus`. Every response carries a
`HostAnswer` that echoes the caller's nonce. The service is internal to VirtRigaud: it
is not part of the provider contract, external Provider implementations neither serve
nor call it, and the manager never calls it. Nothing serves or calls it yet.

## Compatibility

- Additive on the wire: new messages, enums, one new field per mutating request and
  four new capability fields. Nothing existing is renamed, renumbered or retyped.
- The signed host answer messages, their thirteen request and response fields and the
  per-host agent service are additive in the same way; the answer messages are in
  their own file from the start, so the breaking-change check never sees them move.
- A provider built against an older contract ignores the new fields; a manager built
  against an older contract never sends them.
- The grant messages live in `provider.proto`. The contract's breaking-change check
  (`make proto-breaking`, buf category `FILE`) treats moving a message to another file
  as breaking, so they stay there.
- `contracts.Capabilities` carries the four gates; the SDK's `sdk/provider/capabilities`
  has no constant for them. A provider author leaves the four fields `false`: no
  release build of the manager implements the protocol they advertise.
