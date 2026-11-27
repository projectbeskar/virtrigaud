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
| `GrantBody` | message | The signed content of a grant: version, grant kind, installation, Provider, host, enrolment, authority generation and epoch (1–8); operation fields (10–18); administrative fields (20–25); fields 26, 28, 29 set aside for the authority-history protocol (defined, unset at grant version 1; field 27 is a proto `reserved` number); validity `not_after` (30) and `issued_at` (31); fields 32–34 reserved for the host ledger protocol (see [Host ledger protocol (reserved)](#host-ledger-protocol-reserved); unset at grant version 1, and a verifier refuses a version 1 body that sets any). |
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
| `HostAnswer` | message | The answer about the host itself: `context` (1), `agent_version` (2, informational), `agent_api_version` (3), `grant_versions` (4, ascending), `mode` (5), `journal_schema_version` (6), `clock_floor` (7, informational), `staged_certificate_sha256` (8), `inventory` (9, see [Host inventory](#host-inventory)), `pending_renewal` (10, see [Certificate renewal](#certificate-renewal)), `store_capacity` (11, see [Store capacity](#store-capacity)). |
| `RenewalRequest` | message | A host's request to renew its identity certificate for a new key, carried as a host-signed `SignedBlob` in `HostAnswer.pending_renewal`: `renewal_version` (1), `provider_uid`, `host_id`, `install_id`, `enrolment_id` (2–5), `current_certificate_sha256` (6), `certificate_request` (7, DER PKCS #10 for the new key). |
| `StoreCapacity` | message | How full the host's operation store is, in `HostAnswer.store_capacity`: `live_bytes` (1), `threshold_bytes` (2), `level` (3), `requested_version` (4, the request's `store_capacity_version`, echoed), `version` (5, the content version the agent used). |
| `AgentState` | enum | `UNATTESTED`, `READY`, `QUARANTINED` (values 0–3, 0 is `UNSPECIFIED`). `READY` means the store's continuity is established and the recovery of interrupted operations has run. |
| `OperationRecordState` | enum | `ACCEPTED`, `EXECUTING`, `COMPLETED`, `FAILED_CLEAN`, `FAILED_DIRTY`, `OUTCOME_UNKNOWN`, `RETIRED` (1–7: what the store records for an operation it holds a record or tombstone of), `NO_RECORD` and `UNKNOWN_TO_AGENT` (8, 9: it holds neither; neither proves anything about the operation). |
| `VMPresence` | enum | `CREATING`, `PRESENT`, `DELETED`, `OTHER_BINDING`, `NO_RECORD` (values 0–5, 0 is `UNSPECIFIED`). `NO_RECORD` never means that the VM was deleted. |
| `AgentMode` | enum | `IDENTITY` (values 0–1, 0 is `UNSPECIFIED`). |
| `StoreLevel` | enum | `OK`, `WARNING`, `FULL`, `NOT_MEASURED` (values 0–4, 0 is `UNSPECIFIED`): the level of a `StoreCapacity`. |

### Where each answer travels

| `body_type` | Carried in | Answers |
|---|---|---|
| `provider.v1.OperationAnswer` | `TaskStatusResponse.host_answer` (5) | `TaskStatus` |
| `provider.v1.VMAnswer` | `DescribeResponse.host_answer` (8) | `Describe` |
| `provider.v1.HostAnswer` | `HostInfo.host_answer` (12); the three responses of the per-host agent service | `GetHostInfo`, the agent service |
| `provider.v1.RenewalRequest` | `HostAnswer.pending_renewal` (10) only | the pending renewal of a `GetHostInfo` that asked for it |

`body_type` is a closed set of these four strings; a verifier refuses any other value,
and any member other than the one expected for the field that carried it. A `HostInfo`
in a `ListHosts` answer never carries a `host_answer`: `ListHostsRequest` has no nonce.
When an agent answers a read that carried a nonce with an error status (for example
`NotFound` with the `UNKNOWN_TO_AGENT` reason), the `SignedBlob` travels as exactly one
status detail of type `type.googleapis.com/provider.v1.SignedBlob`, beside the
`ErrorInfo`, encoded in at most 16 KiB.

### Request and response fields

Sixteen additive fields on six existing messages:

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
| | `inventory_version` | 4 | uint32 |
| | `renewal_version` | 5 | uint32 |
| | `store_capacity_version` | 6 | uint32 |
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
- `inventory_version` asks the agent to put its host inventory in the signed answer;
  see [Host inventory](#host-inventory). It is set only together with `answer_nonce`.
- `renewal_version` asks the agent to put its pending certificate renewal request in
  the signed answer; see [Certificate renewal](#certificate-renewal). It is set only
  together with `answer_nonce`.
- `store_capacity_version` asks the agent to put how full its operation store is in
  the signed answer; see [Store capacity](#store-capacity). It is set only together
  with `answer_nonce`.
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

These are requirements on a per-host agent. The development-build agent
([`per-host-agent-development.md`](per-host-agent-development.md)) follows them,
with the separate signer process described there.

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

### How the development-build agent answers

The development-build per-host agent makes `OperationAnswer` and `VMAnswer` as
follows (see [`per-host-agent-development.md`](per-host-agent-development.md#operations)).
A development-build manager verifies them before it uses them.

- It signs only for a request with a 16-byte `answer_nonce`, and only while its
  store's active enrolment is the one its certificate names; otherwise the read is
  answered without `host_answer`. Refusals of mutating calls carry no signed answer.
- `TaskStatus` with `answer_nonce` names the operation by `op_id`, `op_enrolment_id` and
  `target_host_id` (the agent's own host) and carries no `task`; anything else is
  `InvalidArgument`. The body, the chain position and the checkpoint come from one read
  of the store. A held operation is answered with its state, number for number, and its
  identity fields, `tombstoned` once it is over; an operation the store has no record
  of is `NO_RECORD` when the store holds the enrolment named and `UNKNOWN_TO_AGENT`
  when it does not, both as `NotFound` with the `UNKNOWN_TO_AGENT` reason and the signed
  answer as a status detail.
- `Describe` with `answer_nonce` needs `binding_id` and the owner's UID. `DELETED`, with
  the Delete's `op_id`, enrolment and sequence number, comes only from the VM tombstone
  the Delete's terminal transaction wrote for exactly that placement; the absence of a
  record is `NO_RECORD`. `exists` is true for `CREATING`, `PRESENT` and `OTHER_BINDING`.

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

### Host inventory

`HostAnswer.inventory` (9) is a host's report of the components it runs virtual
machines with, in the same signed body as the nonce, the enrolment and the chain
position, so one signature covers them all. Nothing in an agent or a manager uses it
yet: this is the wire shape only.

| Message or enum | Fields and values |
|---|---|
| `HostInventory` | `inventory_version` (1), `state` (2), `snapshot_age_seconds` (3), `components` (4), `features` (5), `process_identity_tier` (6), `unexpected_extensions` (7), `state_reason` (8), `requested_version` (9), `supported_version` (10) |
| `HostComponent` | `name` (1), `state` (2), `state_reason` (3), `observed_version` (4), `declared_version` (5), `required_range` (6), `source` (7), `manifest_origin` (8), `artifact_sha256` (9), `files` (10) |
| `ComponentFile` | `name` (1), `sha256` (2) |
| `HostFeature` | `name` (1), `result` (2), `reason` (3) |
| `HostExtension` | `name` (1), `kind` (2), `image_sha256` (3) |
| `InventoryState` | `COMPLETE`, `PENDING`, `OVER_LIMIT`, `UNREADABLE` (values 1-4) |
| `ComponentState` | `READY`, `MISSING`, `INCOMPATIBLE`, `UNHEALTHY`, `UNMANIFESTED`, `MANIFEST_INVALID` (1-6) |
| `ManifestOrigin` | `NONE`, `READ_ONLY`, `WRITABLE` (1-3) |
| `FeatureResult` | `PRESENT`, `ABSENT`, `ERROR` (1-3) |
| `ProcessIdentityTier` | `PIDFD_STORE`, `PIDFS_HANDLE`, `PIDFS_INODE`, `START_TIME` (1-4) |
| `ExtensionKind` | `DIRECTORY`, `UNDECLARED_IMAGE`, `OVERLAYS_COMPONENT` (1-3) |

Every enum has `UNSPECIFIED` as value 0, which is never valid in an answer except
`process_identity_tier`, where it means "not reported".

**Asking and echo.** A caller asks for an inventory with
`GetHostInfoRequest.inventory_version`: the highest version of `HostInventory` it
implements, or 0 for none. It is set only together with `answer_nonce`. The answer
carries `inventory` if and only if the request's `inventory_version` is at least 1. The
agent echoes the request's value byte for byte in `requested_version`, reports the
highest version it implements in `supported_version`, and uses the lower of the two as
`inventory_version`. A verifier requires `requested_version` to equal what it sent, so a
Provider that lowers the requested version in transit is caught, and requires
`inventory_version` to be the lower of the two. A field, enum value or reason added to
the inventory later is used only when `inventory_version` is at least the version that
introduced it. An answer without `inventory` to a request that asked for one is still a
valid answer for every other purpose; it only means that no inventory is known. A manager asks for
the inventory on every signed `GetHostInfo` to a host of a Provider whose type runs a
per-host agent, whatever agent version the host last reported, except during a short
backoff after the agent signed an inventory that breaks a field rule
([`host-inventory-manager.md`](host-inventory-manager.md#asking)).

**Bounds.** At most 32 components, 8 files per component and 64 files in all, 32
features and 16 unexpected extensions; the encoded `HostInventory` is at most 32 KiB,
and the largest allowed content encodes to 25 532 bytes. An
agent never truncates: when what it observed does not fit it reports `OVER_LIMIT` and
lists nothing. Lists are sorted by `name` in byte order, each name once. Digests are raw
SHA-256, exactly 32 bytes where set. Names, versions, ranges and reasons are short and
restricted to a small character set; a reason is a value from a closed set and never an
error message or a path. A `COMPLETE` inventory has no `state_reason` and a reported
`process_identity_tier`; `PENDING`, `OVER_LIMIT` and `UNREADABLE` list nothing.

**What it is.** The inventory is signed so that the Provider cannot alter, replay or
downgrade it. A host whose agent is compromised can still report anything, so it is a
detective control and not a proof.

Test vectors (reproduced by the tests in the `proto` module):

- `GetHostInfoRequest{answer_nonce = 00 01 ... 0f, inventory_version = 1}` encodes to
  `1210000102030405060708090a0b0c0d0e0f` `2001`.
- A `HostAnswer` body (not a complete answer: it has no `context`) with
  `agent_version` `"v0.0.1"` and an `inventory` of version 1, state `COMPLETE`, a
  snapshot age of 30, one component (`cloud-hypervisor`, `READY`, observed and
  declared version `1.0.0`, origin `READ_ONLY`, the SHA-256 of the empty string as
  artifact digest), identity tier `PIDFS_HANDLE` and requested and supported version
  1 encodes to these 94 bytes:
  `120676302e302e31` `4a54` `08011001181e` `2246`
  `0a10636c6f75642d68797065727669736f72` `1001` `2205312e302e30` `2a05312e302e30`
  `4002` `4a20e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
  `3002` `4801` `5001`.
  Its signature input is the 148 bytes `766972747269676175642e686f73742d7369676e65642e763100`
  `0016` `70726f76696465722e76312e486f7374416e73776572` `0000005e` followed by that
  body, and the SHA-256 of the input is
  `43b3a3f66dc30637eea72a7b65bde3ee4f93c9844186838322a88971569be858`. The framing
  vector above is unchanged.

### Certificate renewal

A per-host agent renews its identity certificate for a new key it generates on the
host. It offers the renewal in the signed answer of a `GetHostInfo` that asked for it:

- **Asking.** `GetHostInfoRequest.renewal_version` is the highest version of
  `RenewalRequest` the caller implements (1), or 0 for none, set only together with
  `answer_nonce`. `HostAnswer.pending_renewal` is set only in the answer to such a
  request, and only while the host has a renewal pending; a verifier refuses an answer
  that carries one the request did not ask for. A manager asks only an agent whose
  last verified answer showed `agent_api_version` 3 or later.
- **Signature.** `pending_renewal` is a `SignedBlob` whose `body_type` is
  `provider.v1.RenewalRequest`, signed over the same signature input as the answers
  (domain separator `virtrigaud.host-signed.v1`), by the key of the host's current
  identity certificate. Its `signer_certificate` is exactly the answer's. This is the
  proof that the holder of the pinned key asks for the renewal; the
  `certificate_request` inside is signed by the new key, which is the proof of
  possession of the new key. The current key never signs the PKCS #10 structure
  itself.
- **Field rules.** `renewal_version` is the lower of the requested and the implemented
  version. `provider_uid`, `host_id`, `install_id` and `enrolment_id` are those of the
  answer and of the signer certificate's enrolment record; `current_certificate_sha256`
  is the SHA-256 of the signer certificate (32 bytes); `certificate_request` is one DER
  PKCS #10 request of at most 4096 bytes with nothing after it, whose signature
  verifies with its own key, for an ECDSA P-256 key other than the signer's, asking for
  no extension but the subject alternative names, with an empty subject or the host's
  name, no DNS, address or e-mail name, and URI names that are exactly the signer
  certificate's (the same host, the same enrolment record and start epoch). The encoded
  body is at most 8 KiB, the `SignedBlob` at most 18 KiB; the body is canonically
  encoded and carries no unknown field.
- **Delivery.** The renewed certificate goes back to the host through the per-host
  agent service (`InstallRenewedCertificate`): staged first, put in use only with the
  byte-identical certificate. `HostAnswer.staged_certificate_sha256` names the staged
  certificate. The manager pins the new key only after the answer to its own
  `GetHostInfo` names, as staged, the certificate it had issued
  ([`host-agent-enrolment.md`](host-agent-enrolment.md#certificate-renewal)).

### Store capacity

A per-host agent refuses new operations while its operation store is above a size
threshold. It reports how full the store is in the signed answer of a `GetHostInfo`
that asks for it, so a manager can warn before that happens:

- **Asking.** `GetHostInfoRequest.store_capacity_version` is the highest version of
  `StoreCapacity` the caller implements (1), or 0 for none, set only together with
  `answer_nonce`. `HostAnswer.store_capacity` is set only in the answer to such a
  request; a verifier refuses an answer that carries one the request did not ask for.
  A manager refuses unknown fields in a signed answer, so an agent never sends the
  field to a manager that did not ask for it. An agent older than the field ignores
  the request field and answers without it. A per-host agent refuses a request that
  sets `store_capacity_version` without `answer_nonce` (`InvalidArgument`), as it
  does for `inventory_version` and `renewal_version`; release builds included.
- **Echo, version and presence.** `requested_version` is the request's
  `store_capacity_version`, echoed; a verifier refuses a capacity whose echo is not the
  version it sent. `version` is the content version the agent used, the lower of the
  request's and the highest it implements (as `HostInventory.inventory_version`); a
  verifier refuses 0, a version above the request or above the highest it implements, so
  a manager that asks for a later version still accepts an older agent's answer. An
  agent whose `agent_api_version` is 4 or later answers a capacity in every answer to
  a request that asks for one (`NOT_MEASURED` when it cannot measure its store, and in
  a release build), so a verifier that asked refuses such an agent's answer without
  one: the request lost its `store_capacity_version` in transit. Both checks are over
  signed content, so a relay that strips or changes the unsigned request field makes
  the answer fail verification instead of reading as "not reported".
- **Fields.** `live_bytes` is the bytes the store's live pages use (its size less its
  free pages) as of its last committed transaction: the measure the agent's admission
  compares with the threshold. `threshold_bytes` is the size above which the agent
  refuses new operations (`RESOURCE_EXHAUSTED`, reason `STORE_FULL`); never 0. Both
  are 0 when the level is `NOT_MEASURED`.
- **Level.** With `live = live_bytes` and `threshold = threshold_bytes`, in unsigned
  integer arithmetic: `NOT_MEASURED` exactly when both are 0; otherwise `FULL` when
  `live > threshold`, otherwise `WARNING` when `live >= threshold / 4 * 3`, otherwise
  `OK`. `FULL` is exactly when admission refuses (a store at the threshold still
  accepts). A verifier refuses an answer whose level breaks this rule. A manager may
  apply its own hysteresis when it reports the level (VirtRigaud's keeps a warning
  until the store is below 70% of the threshold); the signed rule does not change.
- **Signature.** `store_capacity` is a field of the `HostAnswer` body, so the answer's
  signature covers it like every other field: altering, adding or stripping it after
  signing fails verification. It adds no `body_type` and changes no framing.

### The per-host agent service

[`proto/agent/v1/agent.proto`](../proto/agent/v1/agent.proto) (package `agent.v1`, Go
package `github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1`) defines
`AgentService`, which a per-host agent serves to the Provider that routes to it:
`Hello` (the first call on each connection, an API version check),
`InstallRenewedCertificate` (two-phase: stage a renewed certificate, then put the
byte-identical certificate in use) and `AgentStatus`. Every response carries a
`HostAnswer` that echoes the caller's nonce. `AgentStatusResponse.freeze_results`
reports, outside the signed answer, what the agent did with each enrolment id
of a relayed freeze confirmation
([details](per-host-agent-development.md#freeze-confirmation)). The service is internal to VirtRigaud: it
is not part of the provider contract, external Provider implementations neither serve
nor call it, and the manager never calls it. A development build of the per-host agent
serves it and the Cloud Hypervisor Provider calls it.

## Host ledger protocol (reserved)

The contract reserves the wire shape of a later protocol in which the manager durably
records every operation a host's agent accepted, from a host-signed receipt, before
the agent starts it, and attests the agent's operation store against that record after
each agent start.

**Implemented in development builds only.** The development-build agent implements
attestation, receipts, ledger marks and retire-if-unknown (see
[Agent attestation](#agent-attestation-development-builds) below), and a
development-build manager with host ledger keys drives them (see
[Manager driver](#manager-driver-development-builds) below). The `.proto` comments
of `HostAuthority` and `AuthorityAnswer` still say "reserved; not yet implemented":
the shape is frozen, and this page describes what development builds do with it. A
manager without host ledger keys, and every release
build, behaves as before: it sends no `ledger_version`, signs no grant at version 2,
calls no `HostAuthority` method, ignores `operation_receipt`, and its host answer
verifier refuses an answer to a request without a `ledger_version` that sets a ledger
field of `AnswerContext`, `OperationAnswer` or `HostAnswer`. The grant verifier refuses
a version 1 `GrantBody` that sets field 32, 33 or 34. The comments in the `.proto`
files are the normative description of every field.

Fields added to existing messages:

| Message | Field | No. | Type |
|---|---|---|---|
| `CreateResponse` | `operation_receipt` | 3 | `SignedBlob` |
| `TaskResponse` | `operation_receipt` | 4 | `SignedBlob` |
| `TaskStatusRequest`, `DescribeRequest`, `GetHostInfoRequest` | `ledger_version` | 7 | uint32 |
| `GrantBody` | `ledger` | 32 | `LedgerPosition` |
| | `ledger_checkpoint` | 33 | `LedgerPosition` |
| | `attestation_id` | 34 | bytes |
| `AnswerContext` | `attestation_id` | 13 | bytes |
| | `ledger_version_echo` | 14 | uint32 |
| `OperationAnswer` | `accepted_seq` | 12 | uint64 |
| | `accepted_head` | 13 | bytes |
| | `awaiting_request` | 14 | bool |
| `HostAnswer` | `ledger` | 12 | `HostLedgerState` |

New messages and enums, in
[`host_answer.proto`](../proto/provider/v1/host_answer.proto):

| Name | Kind | Purpose |
|---|---|---|
| `LedgerPosition` | message | A position in a host's operation store: `store_id` (1), `seq` (2), `head` (3). |
| `LedgerSessionState` | enum | `AWAITING_ATTESTATION`, `ATTESTED` (values 0–2, 0 is `UNSPECIFIED`). |
| `HostLedgerState` | message | A host's ledger state (fields 1–10): the requested and used versions, session state and id, the confirmed mark, pending receipts, the store's origin enrolment and enrolment count, and the parked request bytes and budget. |
| `OperationReceipt` | message | A host's receipt for an accepted operation (fields 1–16), bound to the delivered grant by `grant_digest`. |
| `ChallengePurpose` | enum | `ATTEST`, `REATTEST` (values 0–2, 0 is `UNSPECIFIED`). |
| `ChallengeAnswer` | message | A host's answer to a challenge request (fields 1–6). |
| `AuthorityOutcome` | enum | `APPLIED`, `ALREADY_APPLIED`, `NOT_APPLICABLE`, `REFUSED`, `QUARANTINED` (values 0–5, 0 is `UNSPECIFIED`). |
| `AuthorityAnswer` | message | A host's answer to an authority grant (fields 1–8). |

Ledger `body_type` values: `provider.v1.ChallengeAnswer` (only in
`ChallengeResponse.host_answer`) and `provider.v1.AuthorityAnswer` (only in
`AuthorityResponse.host_answer`) are verified as answers to the request that asked for
them, and only then; `provider.v1.OperationReceipt` (only in `operation_receipt`) is
never an answer to a request (it carries no nonce) and is verified only as a receipt,
against the attempt and the grant of the call it came back from.

A new service, `HostAuthority`, in its own file
[`authority.proto`](../proto/provider/v1/authority.proto): `Challenge`
(`ChallengeRequest` 1–3 → `ChallengeResponse` 1), and `Attest`, `LedgerMark` and
`RetireIfUnknown` (`AttestRequest`, `LedgerMarkRequest`, `RetireIfUnknownRequest`, each
1–3, carrying an `OperationGrant` → `AuthorityResponse` 1). It is a separate service,
not part of the `Provider` service, and nothing in `GetCapabilitiesResponse` changes.
The clustered Cloud Hypervisor Provider of a development build routes it to the
addressed host's agent, relaying requests and answers unchanged (see
[Host authority service](clustered-agent-provider-development.md#host-authority-service));
no other bundled provider registers it, so gRPC answers each of its methods
`Unimplemented` there. A provider author does not implement it. The development-build
agent serves `Challenge`, `Attest`, `LedgerMark` and `RetireIfUnknown` (below).

The Kubernetes API reserves the matching status fields, all optional: the `Accepted`
phase and the `receipt` and `ledgerGeneration` fields of
`VirtualMachine.status.operationAttempts[]` (see
[Attempt markers](operation-grants-manager.md#attempt-markers-statusoperationattempts)),
which a development-build manager with a host ledger writes, and the display mirrors
`Host.status.agentLedger` and `Host.status.agentLastStore` (see
[Host fields](host-agent-enrolment.md#host-fields)), which development builds write.
The manager's durable ledger state is described in
[Host ledger state](host-agent-enrolment.md#host-ledger-state).

### Agent attestation (development builds)

A development-build agent reports agent API version 6, grant versions `[2]` and host
ledger version 2 (version 2 adds the store's attestation record to `HostLedgerState`;
the agent API version is unchanged because the field is additive and versioned by
`HostLedgerState.version`). Only a development-build manager with host ledger keys
signs for it (see [Manager driver](#manager-driver-development-builds)); any other
manager signs only grant version 1, so it signs nothing for such an agent. The agent
refuses every operation until it is attested and runs none until a ledger mark covers
its acceptance. A release-build agent reports
the same versions, serves no `HostAuthority` method, runs no operation, and reports
itself awaiting attestation in every ledger state. Agent API version 5 was the agent
with attestation but without receipts, ledger marks or the ledger gate.

- **Grant version 2.** The agent verifies operation grants at grant version 2 only
  (version 1 is refused, rule `grant_version`), with the version 1 field rules; fields
  32 to 34 stay unset in an operation grant, and `expect_known` (18) may be set.
- **Awaiting attestation.** After every start the agent serves reads and refuses every
  mutation `UNATTESTED` (cause `awaiting_attestation`) and commits nothing: continuity,
  the clock floor commit and recovery wait for the first attestation. The only
  exception is an enrolment change (a fresh store, or a certificate that names a new
  enrolment): without it the agent could sign no answer, so it is committed before the
  attestation; recovery still waits. The first attestation a store ever receives
  commits too, after its position passed the checks (see the attestation record
  below).
- **`Challenge`.** Request rules: no unknown field or enum value, `target_host_id` is
  the agent's host, a 16-byte `answer_nonce`, purpose `ATTEST` or `REATTEST`; anything
  else is `InvalidArgument`. `ATTEST` is issued only while the agent awaits its first
  attestation, `REATTEST` only while it is attested, neither while the store is
  quarantined; otherwise `not_issued_reason` is `already_attested`, `not_attested` or
  `quarantined`. A challenge is 16 random bytes, valid for 300 seconds of
  `CLOCK_MONOTONIC_RAW`, single-use, held in memory only (a restart forgets it), at most
  four per purpose (a fifth evicts the oldest). The answer always carries a
  `HostLedgerState` with the store's origin evidence. An agent whose store's active
  enrolment is not its certificate's signs no answer at all, so it never says
  `not_enrolled`.
- **`Attest`.** After the request rules, the grant must verify at grant version 2 as a
  `GRANT_KIND_ATTEST` grant (the kind is checked against the method before any other
  field) for the agent's host, Provider, installation and certificate enrolment, at
  the clock floor, with the challenge nonce in `single_use_nonce`, `ledger` set and
  `ledger_checkpoint`, when set, a multiple of 1024 at or below it in the same store;
  every other field unset (else `REFUSED` `grant_invalid`). Then: a quarantined store
  is `QUARANTINED`; the nonce must name an outstanding challenge (else
  `challenge_unknown` or `challenge_expired`) whose purpose the agent's state allows
  (else `already_attested` or `not_attested`); only then is the challenge consumed.
  The store compares the ledger position with its chain: another store quarantines it
  (`ledger_store_mismatch`), a position beyond its chain (`ledger_ahead`), another head
  at the position, or at a held checkpoint, (`ledger_mismatch`), and no head and no
  held checkpoint (`ledger_unverifiable`). When it passes, the challenge nonce becomes
  the attestation session (`APPLIED`, `session_replaced` on a re-attestation) and the
  mark becomes the ledger position (the higher of the two on a re-attestation). The
  session and mark live in memory only. Before the first attestation a store ever
  receives is applied, the store commits its attestation record (below); when that
  commit fails, the outcome is `QUARANTINED` (a failed commit quarantines the store
  `io_error`) or `REFUSED` `store_busy`, and no session exists. Every outcome is a signed `AuthorityAnswer`
  whose context names the session after the grant and whose `grant_digest` is
  SHA-256(be32(len(body)) ‖ body ‖ signature) of the delivered grant.
- **`RetireIfUnknown`.** After the request rules, the grant must verify at grant
  version 2 as a `GRANT_KIND_RETIRE_IF_UNKNOWN` grant (the kind first) for the agent's
  host, Provider, installation and certificate enrolment, naming the operation's
  identity (VM, binding, op_id, kind, attempt, intent digest and mint epoch, fields 10
  to 15 and 17) and a 16-byte `attestation_id`, with every other field unset, the
  digest salt and `expect_known` included (else `REFUSED` `grant_invalid`). Then: a
  quarantined store is `QUARANTINED`; an agent that is not `READY` or awaits its first
  attestation is `REFUSED` `not_attested`; another session is `REFUSED`
  `stale_attestation`, an enrolment other than the store's active one `REFUSED`
  `enrolment_not_active` (neither changes anything). The store first scans the
  objects its executor owns for a trace of the operation, without the store lock: an
  object stamped with the op_id, or, for a Create, a VM of the operation's placement.
  A trace that no record of the op_id (and, for a placement, no VM record of the
  placement) explains quarantines the store (`unknown_op_trace`) and is `REFUSED`
  `op_trace_found`; an executor that cannot scan, or whose scan fails, retires
  nothing (`REFUSED` `store_busy`). Then, under the store lock (which serialises every
  admission) and in one transaction: a record or tombstone of the op_id with the same
  identity is `NOT_APPLICABLE` with its state (`ALREADY_APPLIED` with `RETIRED` for a
  tombstone an earlier retirement committed), with other identity fields `REFUSED`
  `op_known`; a witness file of the operation, or a VM record naming it busy, without
  its record quarantines the store (`inconsistent_store`, `QUARANTINED`); otherwise a
  `RETIRED` tombstone with reason `never_received` is committed and anchored, the
  attempt mark of the operation's VM and kind is raised to its attempt, and the answer
  is `APPLIED` with `op_state` `RETIRED`. A later delivery of the op_id meets the
  tombstone and is answered `RETIRED`, never run; a delivery with other identity
  fields is `OP_ID_CONFLICT`. A delivery racing the retirement is decided by the
  store lock: either its record exists first (the retirement is not applicable) or it
  meets the tombstone. The answer echoes the op_id once the grant verified and carries
  no mark.
- **Receipts.** Every operation the agent accepts while attested is answered with an
  `operation_receipt` in its `CreateResponse` or `TaskResponse`: a host-signed
  `provider.v1.OperationReceipt` (receipt version 1) naming the Provider, host,
  installation and enrolment, the attestation session the store held when it decided,
  the store, the operation's identity (op_id, kind, attempt, VM, binding, intent
  digest), the chain sequence number and head at which it was accepted, and the grant
  digest of that delivery. A re-delivery of an operation that is still accepted gets a
  receipt for the same acceptance, bound to its own grant. No receipt is signed once
  the operation has started, for an operation of another enrolment or Provider than
  the certificate's, or above a per-Provider receipt limit (20 per second, burst 40,
  separate from the answer limit); the manager then asks for the operation's state.
- **The ledger gate.** An accepted operation commits no step, and so has no effect,
  until the ledger mark of the current attestation session is at or above the chain
  sequence number at which it was accepted. Until then it is parked: its request bytes
  are held in memory only, and its run waits for the mark. An operation the mark
  covers but whose request bytes the agent does not hold (a restart lost them, or they
  did not fit the budget) waits for a delivery of the same op_id, unless the executor
  runs its kind without them (the development fake executor does). An operation whose
  first step was already committed is never held back. A restart forgets the mark, so
  every operation accepted above the next attestation's ledger position is held back
  again.
- **`LEDGER_BEHIND`.** A new operation is refused `RESOURCE_EXHAUSTED` with reason
  `LEDGER_BEHIND` (definitely not performed) while 4096 accepted operations sit above
  the mark, or when its request would take the parked request bytes above their budget
  (256 MiB). An op_id the agent already knows is answered as usual.
- **`expect_known`.** A grant that sets `expect_known` for an op_id the agent holds
  neither a record nor a tombstone of is refused `NOT_FOUND` `UNKNOWN_TO_AGENT` (not
  definite), nothing is accepted, and the decision is audited as
  `expect_known_unknown`.
- **`LedgerMark`.** After the request rules, the grant must verify at grant version 2
  as a `GRANT_KIND_LEDGER_MARK` grant (the kind first) for the agent's host, Provider,
  installation and certificate enrolment, with `ledger` and a 16-byte
  `attestation_id` set and every other field unset (else `REFUSED` `grant_invalid`).
  Then: a quarantined store is `QUARANTINED`; an agent that awaits its first
  attestation is `REFUSED` `not_attested`; a session other than the current one is
  `REFUSED` `stale_attestation` and changes nothing. The position is checked against
  the chain: another store, a position beyond the chain, another head, or a head above
  the mark the store no longer holds quarantine the store with the ledger loss
  reason. A position above the mark raises it (`APPLIED`) and wakes the operations it
  now covers; one at or below it is `ALREADY_APPLIED`. The answer names the session
  and the mark after the grant.
- **Ledger fields in reads.** `GetHostInfo`, `TaskStatus` and `Describe` with
  `answer_nonce` and `ledger_version` ≥ 1 echo the version in
  `AnswerContext.ledger_version_echo` and name the session in
  `AnswerContext.attestation_id` while the agent is attested (never while
  quarantined); `GetHostInfo` also carries `HostAnswer.ledger` (version 1, session
  state, session id and mark when attested, origin evidence, the accepted operations
  above the mark in `pending_receipts`, capped at 65535, and the parked request bytes
  and their budget). `ledger_version` without `answer_nonce` is `InvalidArgument`. In
  such an answer `OperationAnswer.accepted_seq` is the sequence number at which the
  operation was accepted (from its record); `accepted_head`, the chain head there, is
  set while the operation is accepted and the store holds that head, and
  `awaiting_request` while it is accepted, covered by the mark and waiting for its
  request bytes. An answer to a request without `ledger_version` sets none of them.
- **Ledger loss reasons.** A quarantine the ledger protocol recorded keeps its reason
  (`ledger_store_mismatch`, `ledger_ahead`, `ledger_mismatch`, `ledger_unverifiable`,
  and `unknown_op_trace` from a retirement) only in an answer to a request with
  `ledger_version` ≥ 1 and in host authority answers; every other answer says
  `rolled_back`, which every manager knows. The manager's verifier accepts those
  reasons only in such answers.
- **Origin evidence.** The store records the enrolment under which it was first
  enrolled and how many enrolments were activated in it since, in the activation
  transaction itself. A store enrolled before this record existed reports none and
  never gets one inferred.
- **Attestation record.** The first attestation a store ever receives commits, in
  its own chain transaction and before the attestation is applied, a record that the
  store has been attested: the ledger position's sequence number, the enrolment
  active then, and the sequence number of that transaction (one above the store's
  position at the attestation, so above the attested position). The checks are
  repeated in that transaction. A store that has not established continuity yet (an
  enrolled store restarted before its first attestation ever) first commits what
  must precede any other commit (continuity, the clock floor, the enrolment from the
  certificate). Exactly one such commit happens in a store's life; re-attestations
  and attestations after a restart commit nothing, and the record is never cleared
  or rewritten. `HostLedgerState` version 2 reports it in every ledger state
  (`ChallengeAnswer.ledger`, and `HostAnswer.ledger` for a request with
  `ledger_version` ≥ 2): `store_ever_attested` (11), `store_first_attested_seq`
  (12), `store_first_attested_enrolment_id` (13), `store_attested_record_seq` (14),
  all unset at version 1, so `false` means "never attested" only at version 2 or
  higher. The manager-side decoder (`hostidentity.StoreAttestationOf`) refuses a
  state that sets them at version 1, sets facts without the flag, or sets the flag
  without a first position, with a record sequence number not above it, or with an
  enrolment that is neither empty nor 16 bytes; the agent applies the same rules
  before signing. Erasing the record needs a rollback of the store below that
  transaction: the anchor log quarantines a store file restored alone
  (`rolled_back`), and a manager ledger at or above `store_attested_record_seq`
  catches a joint restore of the store and its anchor log. A store attested by an
  agent from before the record reports `false` until its next attestation. A
  development-build manager starts a host ledger only for a store whose record is
  reported and says it was never attested; a state of version 1 (an agent that does
  not report the record) is treated as unknown, never as "never attested", and holds
  the host until the agent reports it.
- **Retention.** No checkpoint prunes a chain entry at or above the ledger mark, so
  the head at every position since the mark stays on the store; a ledger mark raises
  the floor with the mark.

### Manager driver (development builds)

A development-build manager started with host ledger keys drives the protocol for
every host whose last verified host answer came from an agent of API version 6 or
later. Nothing changes for other hosts, for a manager without host ledger keys, or for
a release build.

- **Ledger version.** Its `GetHostInfo` and `TaskStatus` requests to such a host carry
  `ledger_version` 2. The answer must echo it in `AnswerContext.ledger_version_echo`
  (a request stripped or altered in transit is "no answer"), a host answer must carry
  `HostAnswer.ledger`, and every ledger field must follow the rules the agent applies
  before signing. `ChallengeAnswer` and `AuthorityAnswer` are verified against the
  purpose, grant kind and grant digest of the request that asked for them.
- **Starting a ledger.** A host ledger is started only from ledger evidence: a value
  only the answer verifier builds, from a verified host answer with its ledger state
  (or a challenge answer). It carries the store, the request nonce, the Provider,
  host and enrolment the answer was bound to, and when it verified. The ledger accepts
  only the latest evidence it observed for the host, of the store being started and
  the host's current enrolment, younger than the host answer lifetime; evidence of
  another store, of an earlier request or of another enrolment is refused before
  anything is read or written. Before such a request the manager persists the host's
  latest verified store position, so the continuity position the request names is the
  one a ledger may start from once the host confirms it.
- **Binding.** Every step reads the store's ledger bound to the Host object the host
  id names now, with its identity, and not archived. A ledger that serves an earlier
  Host object (a Host replaced by a new object of the same name or machine) moves to
  the new one only under the rebind conditions (the earlier object deleted or revoked,
  quiet for the host answer lifetime, and no other live Host carrying the identity);
  it is then Pending and the agent is attested again. An archived ledger is never used.
- **Attestation.** An agent awaiting attestation is asked for an `ATTEST` challenge;
  the verified challenge's nonce is begun as a pending session in the ledger (which
  becomes Pending: the agent runs nothing until attested anyway), and an
  attestation grant is signed only from the ledger record that write returned, after
  the ledger object is read again and found unchanged: no ledger object for the store,
  no attestation grant. An agent attested in a session the ledger does not hold (a
  delayed attestation, an agent restart, a restored ledger) is asked for a `REATTEST`
  challenge at most once a minute; the ledger's current session stays current until
  the new one is adopted. Any verified, nonce-bound answer (host, operation, challenge
  or authority answer, never a receipt) that names a pending session adopts it. A
  challenge the agent does not issue is asked again later; an agent that reports its
  store quarantined holds the host.
- **Grant version 2.** Such a host gets operation grants at version 2 only, and only
  while the manager saw its last verified host answer `READY` in the ledger's adopted
  session of the store that answer named, with nothing holding the host; otherwise
  nothing is sent to it. That readiness is decided by a fresh read of the bound,
  unarchived ledger, never by whether a raise was taken or throttled. For such a host,
  an operation answer from any session other than the adopted one is no answer: the
  agent is attested again first. Version 1 is never signed for an agent that implements version
  2. Older agents keep version 1, as before.
- **Receipts.** A receipt in the reply to a version 2 send is verified against the
  host's trust, the attempt's authenticated intent (op_id, kind, attempt, VM, binding,
  intent digest) and the digest of the grant of that very call, then against the
  ledger: its store must be the ledger's and its session the ledger's adopted one. A
  verified receipt moves the attempt to `Accepted` with the receipt recorded on the
  marker, bound to the attempt by a tag under the intent key, raises the ledger, and
  then raises the agent's ledger mark. A receipt counts only for the host's current
  store, bound to the Host object and not archived. An operation answer bound to the
  attempt that shows the operation accepted with its acceptance position, in the
  adopted session, stands for its receipt (a lost reply): it is recorded on the marker
  the same way, before the ledger and the mark are raised over it. A receipt that does
  not verify is "no receipt": the attempt stays `Sent`, and the next raise from a host
  answer covers the operation. The marker records the store, position, head and
  session: the tag covers them, the store, position and session decide whether the
  receipt counts as ledgered, and the head is required by the schema (a public chain
  digest). The receipt tag is an HMAC under the intent key with its own context prefix,
  so it never verifies as an intent or task tag. The marker's `ledgerGeneration`
  mirrors the generation the attempt's intent records, and stays unset when it has
  none.
- **Ledger marks.** A ledger mark grant is signed only from the record of the
  manager's own last successful write of the store's ledger, while nobody wrote it
  since, in an adopted session. The ledger is raised from the agent's verified chain
  position at most once every 30 seconds per host, and the mark follows it whenever
  the agent's mark is behind. The store's attestation record the agent signs
  (`store_attested_record_seq`) is in the agent's chain above the position its first
  attestation named, so it reaches the manager's ledger only with the first raise
  after that attestation.
- **Re-sends.** A re-send of an attempt whose recorded receipt counts as ledgered sets
  `expect_known`, so an agent that holds no record of the operation refuses it instead
  of running it as new. A receipt counts as ledgered when its session is the ledger's
  current or a pending one and its position is at or below the ledger's, or its session
  is an earlier one and its position is at or below where that session was replaced,
  or its session or store is one the ledger does not know (the cautious direction); a
  receipt whose tag fails, or of an attempt not continuous with the ledger, counts too.
- **Retire-if-unknown.** For such a host, a verified answer from its `READY` agent in
  the ledger's adopted session that it holds no record of an attempt's operation
  under its active enrolment (`NO_RECORD`) leads to a retirement when every
  precondition holds: the attempt is `Prepared`, `Sent` or `Accepted`; its marker
  holds no receipt that counts as ledgered (a recorded receipt whose tag does not
  verify holds the attempt as `OperationStateLost`); the store's ledger is bound,
  current, not archived and `Attested` in that session; and the attempt is
  continuous with the ledger (the store and generation its authenticated intent
  records are the ledger's, and its enrolment is one of the generation's). A
  retire-if-unknown grant naming the attempt's identity and that session is then
  signed in exactly one place and sent through the Provider. `APPLIED`, or
  `ALREADY_APPLIED` with `RETIRED`, for this op_id in this session, closes the
  attempt `Retired` with reason `never_received`, after the ledger is raised to the
  answer's position (so a later rollback of the store below the tombstone fails its
  next attestation); a new attempt follows after the usual backoff of a not-performed
  attempt. `NOT_APPLICABLE` (the host holds a record now) leaves the attempt in
  flight and asks again; `REFUSED` `stale_attestation` attests the agent again and
  asks again; `QUARANTINED`, or `REFUSED` `op_trace_found`, holds the host and the VM
  (`OperationOutcomeUnknown`); any other refusal, or no verified answer, asks again.
  An attempt prepared without a ledger (an intent of the first version, generation
  0), or under another store or generation (for example an intent recreated after a
  new ledger generation started), is never retired.
- **Re-sends and retirement.** One `NO_RECORD` never leads to both: an attempt whose
  receipt counts as ledgered is re-sent with `expect_known` and never retired; an
  attempt with no ledgered receipt that is continuous with the ledger is retired and
  never re-sent; an attempt that is not continuous with the ledger is never retired
  and keeps the re-send rules it had before.
- **Frozen enrolment closure.** A verified `NO_RECORD`, in the adopted session from a
  `READY` agent, about an attempt prepared under an earlier enrolment of the host
  whose records the store still holds (a frozen enrolment: an agent answers
  `UNKNOWN_TO_AGENT` for one it does not hold) closes the attempt `Retired` with
  reason `unknown_to_frozen_enrolment`, without any grant, when its marker holds no
  ledgered receipt, the earlier enrolment is one of the ledger generation's
  enrolments, and the attempt's authenticated store and generation are the ledger's.
  A frozen enrolment admits no new operation, and a record whose acceptance the
  ledger covered could not have been lost without failing an attestation. A
  `RETIRED` about such an attempt stays non-definite (a freeze can retire a record
  whose step was lost).
- **Store replaced.** When a new store under a new enrolment archives a host's earlier
  stores, every attempt still in flight whose authenticated intent names an archived
  store is held as `OperationOutcomeUnknown` with reason `store_lost` when it is next
  reconciled: what it did in the lost store can never be learned, so it is neither
  re-sent nor retired.
- **Quarantined answers.** A verified answer from such a host's agent that says its
  store is quarantined (with any reason, a ledger loss reason included) holds the host
  and reaches the attempt's quarantine hold with the agent's reason, rather than being
  read as "no answer".
- **VM answers.** A `Describe` to such a host carries `ledger_version`, and its signed
  answer that a placement is deleted counts only from a `READY` agent in the ledger's
  adopted session, as every operation answer does. A deleted placement is read from
  its VM tombstone, which no rollback can create, so this costs only liveness (a Delete
  waits for the agent's attestation to be adopted); it is applied so that nothing such
  a host answers counts outside the adopted session.
- **Intent version 2.** An attempt prepared for such a host records, in its
  operation-intent Secret, the ledger store and generation it was prepared under, and
  its tag covers them under a second intent message version. Continuity with the
  current ledger is decided on those authenticated values only (store, generation, and
  the attempt's enrolment among the generation's enrolments), never on a marker field
  or a timestamp; an intent of the first version counts as generation 0 and is never
  continuous.
- **Traffic.** Attestation (20 per second, burst 40), heartbeat raises and their marks
  (50 per second, burst 100) and receipt raises and their marks (200 per second, burst
  400) draw from separate token buckets, so none can starve another. The per-host
  heartbeat throttle is checked before a heartbeat token is taken.
- **Upgrade pre-flight.** A stopping manager that held the leader lease persists every
  host's latest verified store position (bounded to 10 seconds), so the next manager
  starts each host ledger from a fresher position and the agent's first attestation
  needs a checkpoint less often. A replica that never led writes nothing; a stopping
  leader may already have lost its lease, which is safe because a position is only
  ever raised, from answers that replica verified.

## Compatibility

- Additive on the wire: new messages, enums, one new field per mutating request and
  four new capability fields. Nothing existing is renamed, renumbered or retyped.
- The signed host answer messages, their fourteen request and response fields and the
  per-host agent service are additive in the same way; the answer messages are in
  their own file from the start, so the breaking-change check never sees them move.
- The host inventory (`HostAnswer.inventory` and the messages and enums it uses) is
  additive in the same way and lives in the same file as `HostAnswer`. It adds no
  `body_type`, changes no framing and no existing field.
- The certificate renewal request (`GetHostInfoRequest.renewal_version`,
  `HostAnswer.pending_renewal`, `RenewalRequest`) is additive in the same way. It adds
  one `body_type`, signed under the same domain separator; the framing and every
  existing field are unchanged.
- The store capacity (`GetHostInfoRequest.store_capacity_version`,
  `HostAnswer.store_capacity`, `StoreCapacity`, `StoreLevel`) is additive in the same
  way. It adds no `body_type` and changes no framing or existing field. Agent API
  version 4 marks an agent that always answers it when asked.
- The host ledger reservation is additive in the same way: new messages, enums and a
  new file and service, and fields at unused numbers only. `GrantBody` 19 stays
  unassigned and 27 reserved. Because generated code now knows the reserved fields,
  the verifiers refuse them explicitly when set, instead of through the unknown-field
  rule.
- Agent API version 5 (development builds) is not compatible with the current
  manager on purpose: it reports grant versions `[2]`, and the manager, which signs
  version 1 only, signs nothing for it, so the agent's VMs keep running and control
  waits. The Cloud Hypervisor Provider speaks agent API version 5 (it accepts agents
  of version 4 and 5); update Providers before agents.
- A provider built against an older contract ignores the new fields; a manager built
  against an older contract never sends them.
- The grant messages live in `provider.proto`. The contract's breaking-change check
  (`make proto-breaking`, buf category `FILE`) treats moving a message to another file
  as breaking, so they stay there.
- `contracts.Capabilities` carries the four gates; the SDK's `sdk/provider/capabilities`
  has no constant for them. A provider author leaves the four fields `false`: no
  release build of the manager implements the protocol they advertise.
