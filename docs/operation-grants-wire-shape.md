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
No manager verifies them yet.

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

**Partly implemented, development builds only, not wired to the manager.** The
development-build agent implements the attestation part (see
[Agent attestation](#agent-attestation-development-builds) below); receipts, ledger
marks and retire-if-unknown are not implemented by anyone yet. The manager sends no
`ledger_version`, signs no grant at version 2, calls no `HostAuthority` method and
ignores `operation_receipt`; its host answer verifier still refuses an answer that
sets a ledger field of `AnswerContext`, `OperationAnswer` or `HostAnswer`, or carries
one of the three ledger body types, and the grant verifier refuses a version 1
`GrantBody` that sets field 32, 33 or 34. The comments in the `.proto` files are the
normative description of every field.

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

Reserved `body_type` values (outside the closed set above; a verifier refuses them):
`provider.v1.OperationReceipt` (only in `operation_receipt`),
`provider.v1.ChallengeAnswer` (only in `ChallengeResponse.host_answer`) and
`provider.v1.AuthorityAnswer` (only in `AuthorityResponse.host_answer`).

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
agent serves `Challenge` and `Attest` (below).

The Kubernetes API reserves the matching status fields, all optional and none populated
yet: the `Accepted` phase and the `receipt` and `ledgerGeneration` fields of
`VirtualMachine.status.operationAttempts[]` (see
[Attempt markers](operation-grants-manager.md#attempt-markers-statusoperationattempts)),
and `Host.status.agentLedger` and `Host.status.agentLastStore` (see
[Host fields](host-agent-enrolment.md#host-fields)).

### Agent attestation (development builds)

A development-build agent reports agent API version 5 and grant versions `[2]`. It is
not wired to any manager: the current manager signs only grant version 1, so it signs
nothing for such an agent, and the agent refuses every operation until it is attested.
A release-build agent reports the same versions, serves no `HostAuthority` method,
runs no operation, and reports itself awaiting attestation in every ledger state.

- **Grant version 2.** The agent verifies operation grants at grant version 2 only
  (version 1 is refused, rule `grant_version`), with the version 1 field rules; fields
  32 to 34 stay unset in an operation grant. An operation grant at version 2 is not
  yet receipt-bound: the agent returns no `operation_receipt` and does not yet hold an
  accepted operation back until its acceptance is recorded by a manager.
- **Awaiting attestation.** After every start the agent serves reads and refuses every
  mutation `UNATTESTED` (cause `awaiting_attestation`) and commits nothing: continuity,
  the clock floor commit and recovery wait for the first attestation. The only
  exception is an enrolment change (a fresh store, or a certificate that names a new
  enrolment): without it the agent could sign no answer, so it is committed before the
  attestation; recovery still waits.
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
  session and mark live in memory only. Every outcome is a signed `AuthorityAnswer`
  whose context names the session after the grant and whose `grant_digest` is
  SHA-256(be32(len(body)) ‖ body ‖ signature) of the delivered grant. `LedgerMark` and
  `RetireIfUnknown` are `Unimplemented`.
- **Ledger fields in reads.** `GetHostInfo`, `TaskStatus` and `Describe` with
  `answer_nonce` and `ledger_version` ≥ 1 echo the version in
  `AnswerContext.ledger_version_echo` and name the session in
  `AnswerContext.attestation_id` while the agent is attested (never while
  quarantined); `GetHostInfo` also carries `HostAnswer.ledger` (version 1, session
  state, session id and mark when attested, origin evidence; `pending_receipts` and the
  parked request bytes are 0 until receipts are implemented). `ledger_version` without
  `answer_nonce` is `InvalidArgument`. `OperationAnswer` 12–14 are never set yet.
- **Ledger loss reasons.** A quarantine an attestation recorded keeps its reason
  (`ledger_store_mismatch`, `ledger_ahead`, `ledger_mismatch`, `ledger_unverifiable`)
  only in an answer to a request with `ledger_version` ≥ 1 and in host authority
  answers; every other answer says `rolled_back`, which every manager knows.
- **Origin evidence.** The store records the enrolment under which it was first
  enrolled and how many enrolments were activated in it since, in the activation
  transaction itself. A store enrolled before this record existed reports none and
  never gets one inferred.
- **Retention.** No checkpoint prunes a chain entry at or above the attested mark, so
  the head at every position since the last attestation stays on the store. Until
  ledger marks are implemented, the mark does not move between attestations.

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
