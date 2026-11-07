# Operation grants: manager side (development builds only)

[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md) describes the operation
grant messages in the provider contract. This page describes the manager-side code that
mints, sends and verifies them, following the operation-identity design of ADR-0016
(status: Proposed).

**It is development-only.** A release build has no grant signer, refuses the development
signer and enrolment flags, and refuses to send a signed operation. Only a Provider whose
`spec.type` requires operation identity (the development-only `cloudhypervisor` type, which
only a development build admits) gets signed operations; no bundled Provider type does, so
for every other Provider a manager, release or development, sends exactly the calls it sent
before, and writes none of the status fields or Secrets described below.

## How release builds are kept unchanged

| Lock | Where | Release build (no `virtrigaud_dev` tag) |
|---|---|---|
| No signer | `internal/operation/signer_release.go` | `operation.SignerCompiled` is `false`; the only signer, `DevSigner`, is not compiled in. `operation.LoadSigner` refuses any key file with `operation.ErrNoSigner` and never reads it. |
| Startup refusal | `cmd/manager/main.go` | `--dev-operation-signer-key-file` set: the manager logs `invalid --dev-operation-signer-key-file` and exits 1 before it connects to the cluster, like `--enable-dev-provider-types`. |
| No sending | `internal/transport/grpc/client_operation.go` | `NewOperationIdentityClient` and `Client.SendOperation` return `operation.ErrNoSigner` and send nothing. |
| Startup refusal | `cmd/manager/main.go` | `--dev-operation-enrolment-file` set: the manager logs `invalid --dev-operation-enrolment-file` and exits 1. |
| Type-gated path | `internal/runtime/remote/resolver.go`, `internal/controller/virtualmachine_operation_identity.go` | Only a Provider whose `spec.type` requires operation identity gets an operation-identity client and the attempt-marker code path. Without a signer (every release build) such a Provider is not resolved at all: its VMs report `Ready=False/OperationSignerUnavailable` and nothing is sent. Every other Provider keeps its behaviour: no `operation_envelope`, no marker, no intent Secret. |

The tagged and untagged builds are both tested: `go test ./internal/operation/
./internal/transport/grpc/ ./internal/runtime/remote/ ./internal/controller/ ./cmd/manager/`
and the same with `-tags virtrigaud_dev` (the end-to-end controller specs against the
instrumented fake run only in the tagged build).

## Operation kinds

`internal/operation/kinds.go` holds the `OperationKind` table: each kind names one RPC,
its request message type and, for the four Power kinds, the fixed `PowerRequest.op`. A
test pins it to the proto descriptors and to the table in
[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md#messages-and-enums).

The manager can send seven of the eighteen kinds as signed operations:
`Create`, `Delete`, `PowerOn`, `PowerOff`, `Reboot`, `ShutdownGraceful` and `Reconfigure`
(`contracts.OperationKind`). The other kinds (snapshots, clone, image prepare, export,
import, staged and landed disk deletes, owner transfer, hardware upgrade) have no signed
path: on a client in operation-identity mode they are refused with
`contracts.ErrOperationIdentityNotImplemented` and nothing is sent, signed or unsigned.

## Minting a grant

`operation.MintOperationGrant` builds a version 1 `GRANT_KIND_OPERATION` `GrantBody`:

- fields 1–8 and 10–17 from the attempt identity, the signer's `install_id` and
  `authority_generation`, and `intent_digest = SHA-256(digest_salt || request bytes)`;
- `expect_known` (18) false; fields 20–29 unset;
- `issued_at` (31) the manager's clock and `not_after` (30) five minutes later by default,
  at most one hour (defence in depth only).

Every counter (`authority_generation`, `Epoch.counter`, `attempt`) counts from 1;
`install_id`, `enrolment_id`, `binding_id`, `op_id` and `Epoch.holder` are 16 random
bytes; the digest salt is 32 random bytes. The minter refuses, before signing, anything the
verifier would refuse on its own, including request bytes that do not decode as the kind's
request type. The body is serialised once and the signature covers those exact bytes:
ECDSA P-256 over SHA-256, ASN.1 DER, with `signer_cert_chain` holding the leaf and its
path length 0 issuer. The trust anchor is never sent.

## Sending: two phases

A client in operation-identity mode (`grpc.NewOperationIdentityClient`, development builds
only) implements `contracts.OperationSender`. It requires TLS with a verified server
certificate: a missing TLS configuration (plaintext), `Insecure`, or a prebuilt
configuration with `InsecureSkipVerify` is refused, because request bytes can carry secrets
and a grant seen on the wire stays valid until its `not_after`.

1. `BuildOperation` builds the request message of the kind (the same fields the unsigned
   method sends, with `operation_envelope` unset), serialises it once, draws the digest
   salt and checks that a grant could be minted. It sends nothing. A caller can persist the
   returned `PreparedOperation` before the first send. A routed request whose host is not
   the grant's host is refused.
2. `SendOperation` mints a fresh grant over the prepared bytes and sends the envelope on
   the kind's RPC, with **no other field of the outer request set**. Before signing it
   checks again that the bytes decode as the kind's request and that a routed request
   targets the grant's host, because a prepared operation may have been stored and
   reloaded since step 1. Whoever can write a stored prepared operation can therefore have
   the manager sign that kind of request, so such a store is part of the signing boundary. A re-send of the same
   `PreparedOperation` sends the same bytes and `op_id` under a new grant.

In that mode the unsigned mutating methods of `contracts.Provider` (`Create`, `Delete`,
`Power`, `Reconfigure`) refuse with `contracts.ErrOperationIdentityRequired` and send
nothing. Request bytes can carry cloud-init user data: they are never logged, and
`PreparedOperation` renders without the bytes or the salt through `fmt` (`String`,
`GoString`), `encoding/json` (`MarshalJSON`) and `log/slog` (`LogValue`).

## Verifying a grant

`operation.Verifier` is the verifier that a test fake and a later per-host agent share. It
pins trust anchors and the installation, Provider, host and enrolment it verifies for, and
refuses a call (the call is not performed) with a `*operation.Refusal` naming the reason and
the rule:

| Refused | Reason |
|---|---|
| No `operation_envelope` on the call | `IDENTITY_REQUIRED` |
| Another field of the outer request set beside the envelope, or unknown fields on it | `GRANT_MISMATCH`, answered as `INVALID_ARGUMENT` |
| A grant body over 64 KiB, a signature over 160 bytes or a certificate over 8 KiB (checked before anything is parsed) | `GRANT_UNAUTHENTICATED` |
| A request over 4 MiB | `GRANT_MISMATCH` |
| No grant; a chain that is not exactly a leaf and a path length 0 issuer chaining to a pinned anchor; a leaf without the grant-signing profile (below); an issuer without `keyCertSign`; a certificate that is not valid now; a key that is not ECDSA P-256; a signature that does not verify | `GRANT_UNAUTHENTICATED` |
| `authority_generation` below the verifier's minimum; an expired grant, one issued in the future, or one valid for more than one hour | `GRANT_UNAUTHENTICATED` |
| Unknown fields in the envelope, the grant, the body, any message nested in it, or the decoded request | `GRANT_MISMATCH` |
| An `UNSPECIFIED` or unknown `grant_kind` or `kind`; an unknown enum value anywhere in the body or the request; a grant kind other than `OPERATION` | `GRANT_MISMATCH` |
| `grant_version` 0 or not 1; field 26, 28 or 29 set; an administrative field (20–25) set | `GRANT_MISMATCH` |
| A counter that is 0; an identifier of the wrong length; an empty Provider, host or VM uid; a missing timestamp | `GRANT_MISMATCH` |
| Another installation, Provider, host or enrolment | `GRANT_MISMATCH` |
| A kind that names another RPC or request type than the call; a request that does not decode as that type, carries an envelope of its own, or disagrees with the kind's fixed `op`; an `intent_digest` that does not match the request bytes | `GRANT_MISMATCH` |

The signature is checked before the body is decoded, and the body is never re-encoded.

**Replay and single execution.** The verifier is stateless: a verified envelope verifies
again until its `not_after`. Whoever executes operations must enforce single execution
itself, keyed on the grant's (`op_id`, `attempt`), never on the signature or the grant
bytes (a re-send of the same attempt carries a new grant). Signatures are accepted in
low-S form only (`s` at most half the curve order), so a signature has one encoding; the
development signer always emits that form. The caller acts only on the decoded request the
verifier returns.

**Grant-signing leaf profile.** The leaf must not be a CA, its key usage must be exactly
`digitalSignature` (no `keyEncipherment`, no `keyCertSign`), and it must carry **no
extended key usage at all**: a leaf with `serverAuth`, `clientAuth`, `anyExtendedKeyUsage`
or any other EKU OID is refused. A TLS certificate issued under the same anchor therefore
cannot sign grants. The issuer must be a CA with path length 0 and `keyCertSign`. A
dedicated grant-signing EKU OID may replace the "no EKU" rule later. The anchor should
still be dedicated to grant signing.

## The development signer

`operation.DevSigner` (compiled only with `-tags virtrigaud_dev`) is a stand-in for real
custody of the grant-signing key. It loads an ECDSA P-256 anchor key and a 16-byte
installation id from the file named by `--dev-operation-signer-key-file`, makes a
self-signed anchor certificate from the key (stable for a key, so a verifier that pinned it
keeps verifying after a manager restart), and mints in-process ECDSA P-256 keys for a path
length 0 issuer and a one-hour grant-signing leaf, which it re-mints before they expire.
`authority_generation` is always 1. The key is exportable by construction: never use it
outside development.

The key file is PEM with exactly two blocks, a PKCS#8 `PRIVATE KEY` (P-256) and a
`VIRTRIGAUD DEV INSTALL ID` holding 16 bytes:

```bash
umask 077
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 > signer.pem
{ echo '-----BEGIN VIRTRIGAUD DEV INSTALL ID-----'
  head -c 16 /dev/urandom | base64
  echo '-----END VIRTRIGAUD DEV INSTALL ID-----'; } >> signer.pem
```

The file must not be accessible to group or others (mode `0600` or `0400`); a Kubernetes
Secret holding it is mounted with `defaultMode: 0400`. A development manager started with
the flag loads the file, refuses a malformed one at
startup (without quoting it), and logs a warning that it is not for production use. The
signer signs the operations sent to Providers whose type requires operation identity.

## Capability gates

The transport client reads `GetCapabilitiesResponse` fields 31–34 into
`contracts.Capabilities` (`SupportsOperationIdentity`, `RequiresOperationIdentity`,
`SupportsFencing`, `RequiresFencing`). They are a Provider's unauthenticated claims: hints
that must match the protection the Provider's type requires, never a way to lower it.

- The required protection comes only from the Provider's immutable `spec.type`
  (`operation.RequiredProtection`): `cloudhypervisor` requires operation identity, every
  other type requires none. The client mode is chosen from it, never from the capabilities,
  and is part of the resolver's client cache key.
- A Provider whose type requires operation identity must advertise both
  `supports_operation_identity` and `requires_operation_identity`, and no fencing gate.
  Otherwise the Provider reports `Ready=False/OperationIdentityMismatch`, its VMs report the
  same, and nothing is sent to it (signed or unsigned) until it advertises a match. With a
  match and a signer loaded the Provider reports `Ready=True/OperationIdentityMatched`.
- Any other Provider that advertises any of the four gates reports
  `Ready=False/OperationIdentityMismatch` on the Provider only; its VMs are unchanged (they
  are never sent signed operations). Every bundled provider advertises none, so no other
  Provider gets a `Ready` condition from this.
- Fencing is not implemented: advertising either fencing gate is always a mismatch.

## Operation identity on VirtualMachines

For a Provider whose type requires operation identity, the VirtualMachine controller sends
a VM's `Create`, `Delete`, power operations (`PowerOn`, `PowerOff`, `Reboot`,
`ShutdownGraceful`) and `Reconfigure` as signed operations, and records each attempt before
it is first sent. Every other mutating operation on such a VM or Provider (snapshots,
clones, image prepares, migrations, adoption) has no signed path and is refused locally
before any RPC (see
[Operations with no signed path](#operations-with-no-signed-path)). The
operation-identity client also refuses every unsigned mutating call
(`contracts.ErrOperationIdentityRequired` or `ErrOperationIdentityNotImplemented`), so
nothing is ever sent unsigned.

### Kinds and their requests

| Kind | Asked for by | Request | Recorded once `Succeeded` |
|---|---|---|---|
| `Create` | a VM with no id, once placed | `CreateRequest` for the pending host, owner = the VM's UID | `status.id`, the confirmed placement, `Bound` |
| `Delete` | the VM's deletion | `DeleteRequest` for the VM's id, host and owner | the finalizer is released |
| `PowerOn` | `spec.powerState: On` while the VM is reported off | `PowerRequest`, `op` = `POWER_OP_ON` | nothing: the next Describe reports the state |
| `PowerOff` | `spec.powerState: Off` while the VM is reported on (or suspended) | `PowerRequest`, `op` = `POWER_OP_OFF` | as above |
| `ShutdownGraceful` | `spec.powerState: OffGraceful` while the VM is reported on | `PowerRequest`, `op` = `POWER_OP_SHUTDOWN_GRACEFUL` | as above |
| `Reboot` | no VirtualMachine field asks for it yet; the controller can send it | `PowerRequest`, `op` = `POWER_OP_REBOOT` | as above |
| `Reconfigure` | a CPU or memory change (class or `spec.resources`), after the same capacity and honest-result checks as for every clustered Provider | `ReconfigureRequest` for the VM's id, host and owner, with the desired configuration without cloud-init user data or metadata | `status.currentResources` = the CPU and memory the stored request asked for, written with `Terminal` |

Each power kind fixes its `PowerRequest.op` (the `OperationKind` table), and the grant names
the kind, so a host refuses a request whose `op` does not match its grant with
`GRANT_MISMATCH` (for example a `REBOOT` grant over a `SHUTDOWN_GRACEFUL` request; the
conformance case `identity.wrong-kind-refused`). When the controller loads a stored
intent, it also checks the request against the attempt: a power request must carry its
kind's `op` and name the VM's recorded id, its host and the VM's UID as owner; a
Reconfigure request must name the same and a CPU and memory size, and carry no user data
or metadata. A request that fails these checks is held as `OperationIntentTampered` and
not sent.

A signed `Reconfigure` leaves the VM's cloud-init user data and metadata out of its
desired configuration: no Provider's Reconfigure reads them (each reads the class's CPU,
memory and disk defaults, and the disks), so they are not copied into the intent Secret or
covered by the grant. The unsigned Reconfigure of every other Provider is unchanged.

A `Reconfigure` is bound to the exact resources it asks for: the grant covers the exact
request bytes, and once the attempt `Succeeded` the controller records the size from the
stored request, not from the spec as it is then. A synchronous answer is recorded as for
every clustered Provider: applied, applied to the persistent definition only
(`Reconfiguring=True/RestartRequired`, counted at the larger of the old and new size), or,
without the honest-result marker, not trusted (counted at the larger size). An answer with
a task records its honest-result and restart-required flags on the `Sent` marker
(`honest`, `restartRequired`, covered by the task tag), and they are applied the same way
when the task `Succeeded`. The size is written in the same status write as `Terminal`, so a
restart in between cannot lose it. A spec that changes while the attempt is in flight is a
new attempt once this one is over.

A power operation or `Reconfigure` can be asked for again after one `Succeeded` (the VM's
state or spec changed since); the next attempt of the same kind gets a new `op_id` and the
next attempt number, after a wait measured from the end of the previous one. The wait is
15 seconds, and doubles (at most 5 minutes) with each consecutive success of the kind that a
new attempt follows within 10 minutes; a request more than 10 minutes after the last success
waits not at all and starts the doubling again. The kinds are counted apart, so asking for
another power state starts its own count. So a guest that powers itself off again, or a
Provider whose Describe lags its task, cannot drive a fast loop of attempts. The count is
kept in the manager's memory, like its other pacing records: a manager restart only
shortens a wait. A `lastTransitionTime` in the future counts as now. A `Create` or a `Delete` that
`Succeeded` is never sent again.

A power or Reconfigure send that meets an unavailable host, a VM missing on its host or not
this VM's, or an owner that cannot be told is held as for every clustered Provider
(`HostUnavailable`, `VMMissingOnHost`, the owner-refusal holds), at their pace; the attempt
stays and is re-sent with the same `op_id`.

### One attempt at a time

At most one attempt of any kind is in flight (`Prepared` or `Sent`) per VM. Each reconcile
of a VM whose Provider requires operation identity first drives the attempt in flight, if
there is one, to its end (re-sending it with the same `op_id` and bytes, or polling its
task), and decides nothing else until it is `Terminal`. Then:

1. a VM being deleted gets its `Delete` first; nothing else is started for it (a power
   operation or Reconfigure in flight when the deletion began is finished first, or
   superseded if it is held, below);
2. otherwise the reconcile decides as it does for every Provider: `Create` for a VM with no
   id, then the power state, then the size.

So a resize requested while a power-off runs waits until the power-off is over and is then
decided from the VM's state at that time; a deletion requested while a Reconfigure runs
waits for it and is then the next attempt. Not-performed outcomes and their backoff apply
per kind: while a power attempt waits out its backoff, the reconcile does not move on to
the size.

**A deadline on running power and Reconfigure attempts.** A `Sent` power or Reconfigure
attempt that its host keeps reporting running for more than one hour, measured from its
`Sent` transition (a `lastTransitionTime` in the future counts as now), is held as
`OperationOutcomeUnknown`: it is still asked about, and a Delete of the VM supersedes it
(below). A `Create` or `Delete` attempt has no such deadline.

**A Delete supersedes a held power or Reconfigure attempt.** An attempt in flight whose
outcome the controller cannot learn would otherwise block the VM's Delete for ever, and the
only ways out (orphan-on-delete, force-delete) leave the VM on its host. So, for a VM being
deleted, a power or Reconfigure attempt in flight that is held as `OperationUnknownToAgent`,
`OperationOutcomeUnknown`, `OperationIntentLost`, `OperationIntentTampered` or
`OperationStateLost` is written `Terminal` with outcome `Superseded`, a Warning event
`OperationSuperseded` names its kind, number and op_id, its intent Secret is kept in the
Provider's namespace as evidence while the VM exists (the sweeper removes it only once the
VM is gone), and the signed Delete follows. The VM is released only once that Delete is
confirmed. Whatever the superseded attempt did or still does, a confirmed Delete removes the
VM. Nothing else supersedes a held attempt:

- a held or in-flight `Create` is never superseded: the Delete needs the VM's id, and a
  Create that may still run could create the VM after the Delete;
- a `Delete` is never superseded;
- a `PowerOff` or `ShutdownGraceful` never supersedes a held `PowerOn` or `Reboot`: the
  held operation could still run on the host after the power-off and leave the VM on while
  the operator reports it off. The held attempt keeps blocking power changes; deleting the
  VM, or the acknowledgement below for a `FailedDirty` outcome, are the ways forward.

### The development enrolment file

A grant names the host's enrolment identifier and an epoch. Until hosts prove their
enrolment with a signed answer, a development manager reads them from the JSON file named
by `--dev-operation-enrolment-file` (compiled only with `-tags virtrigaud_dev`; a release
build refuses the flag):

```json
{
  "epoch": {"counter": 2, "holder": "<32 hexadecimal characters>"},
  "providers": {
    "<provider namespace>/<provider name>": {
      "<host name>": "<32 hexadecimal characters: the host's enrolment id>"
    }
  }
}
```

Unknown fields are refused, the epoch counter counts from 1, and every identifier is 16
bytes. The epoch carries identity only: no ordering or fencing meaning. A VM whose host has
no enrolment reports `Ready=False/OperationEnrolmentUnavailable` and nothing is sent.

### Attempt markers: `status.operationAttempts`

`VirtualMachine.status.operationAttempts` holds at most one entry per operation kind (a
list keyed by `kind`, at most 18 entries): the latest attempt of that kind. Each entry has
the attempt number (counting from 1), its random `opID` (32 hexadecimal characters), the
`bindingID` and `hostID` it was issued for, its `phase`, and, once Terminal, its `outcome`
and the Provider's `outcomeReason` and `taskRef`; a Create answered with a task also
records the VM `id` the Provider answered, with `Sent`. `status.placement.bindingID` identifies
the VM's placement; it is drawn with the VM's first Create attempt on its host, written in
the same status write, and unchanged by re-sends. Neither field is ever written for any
other Provider, and neither carries request bytes, a salt or a digest.

| Phase | Meaning |
|---|---|
| `Prepared` | The attempt's identity and exact request are stored. It may or may not have been delivered. |
| `Sent` | The Provider accepted the operation and returned a task; its outcome is not known yet. |
| `Terminal` | The outcome is known: `Succeeded`, `FailedClean`, `FailedDirty`, `Retired`, `Cancelled`, or `Rejected` (the host refused the first delivery with a reason that proves it was not performed); or, for a held power or Reconfigure attempt of a VM being deleted, `Superseded` (its outcome is unknown; see "One attempt at a time"). |

Every marker write is a JSON Patch on the status subresource whose `test` operations
require the markers (and, where written, `status.placement` and `status.id`) to be exactly
as the reconcile read them. A write that fails sends nothing. The order of one attempt is:

1. Decide: a new attempt only when no attempt of any kind is in flight for the VM, and the
   latest attempt of the kind ended not performed (`Retired`, `FailedClean`, `Cancelled`,
   `Rejected`) and its backoff (15 s doubling per attempt, at most 5 min) is over, or, for
   a power kind or `Reconfigure`, `Succeeded` and the same wait is over, or there is none. The
   `op_id` and digest salt are drawn once.
2. Persist: the intent Secret (below), then the `Prepared` marker.
3. Transmit: the stored bytes under a freshly minted grant. A re-send (after a transport
   error, `VM_BUSY`, a lost reply or a manager restart) sends the same bytes with the same
   `op_id`; a host answers a known `op_id` from its record and never executes it twice.
4. Close: `Sent` with the task reference and its `taskTag`, then `Terminal` with the outcome, then the intent
   Secret is deleted (a Delete's is kept until the finalizer is released). A successful
   Create writes `Terminal`, `status.id` and the confirmed placement in one status write.

A VM's finalizer is released once a Delete attempt is `Terminal/Succeeded`, or when no
Create of the VM was ever performed (it has no Create attempt, or its latest one was not
performed, and no id is recorded). A Delete recorded `Succeeded` that the reconcile which
recorded it did not release (a restart in between, or an edited status) releases the
finalizer only when its intent Secret authenticates and matches it and the Provider
confirms it: its task reports `Succeeded`, or, for a synchronous Delete, a re-send of the
same attempt succeeds. A Delete waits for any attempt in flight. Orphan-on-delete and
force-delete work as for every Provider.

### Delete: an absent VM never releases the finalizer

For every other Provider, a Delete answered "not found", or an owner-checked Describe that
reports the VM absent after a Delete task's outcome is unknown, releases the finalizer
([`task-status-outcomes.md`](task-status-outcomes.md)). For a Provider that requires
operation identity it never does: "no such VM" is an unsigned claim that a misrouted,
restored or misbehaving host can make about a VM that still runs. The finalizer is
released only by:

- a confirmed Delete: a task bound to this Delete attempt (its `taskTag`) reports
  `Succeeded`, or a re-send of the same `op_id` is answered with success;
- no Create of the VM ever performed (no Create attempt, or only not-performed ones, and no
  id);
- orphan-on-delete or force-delete.

So:

| The host or Provider answers | Result |
|---|---|
| the Delete with "not found" or "not this VM's owner" (`NotFound`, `VM_NOT_OWNED`) | held as `OperationDeleteUnconfirmed`; the same attempt is re-sent every 5 minutes |
| the Delete's task with `UNKNOWN_TO_AGENT` | held as `OperationUnknownToAgent`; no new Delete attempt |
| the Delete's task with `Retired`, `FailedClean` or `Cancelled` | a new Delete attempt after the backoff (never a release) |
| nothing, because the VM's Provider is gone, and a Create of the VM was or may have been performed | held as `OperationDeleteUnconfirmed` |

No Describe is used to decide a release. An authenticated host answer that the VM is
deleted (bound to the VM's UID and binding, signed by or bound to the host's enrolment)
could release such a VM later; no such answer exists in the provider contract yet, and the
controller's extension point for it (`authenticatedHostDeleted`) always answers no.

### Operations with no signed path

For a Provider that requires operation identity, these are refused locally, before any RPC:
nothing is sent, signed or unsigned. The VMSnapshot, VMClone, VMMigration and VMAdoption
controllers refuse before they resolve a Provider client at all; the VirtualMachine
controller (image prepare, snapshot revert) has already resolved the VM's
operation-identity client, which it needs for the signed lifecycle, and sends nothing on
it for these.

| Operation | Refused by | Where the refusal shows |
|---|---|---|
| VM snapshot create, delete | VMSnapshot controller | the VMSnapshot's `Ready=False/OperationIdentityUnsupported` |
| Snapshot revert (`spec.snapshot.revertToRef`) | VirtualMachine controller | the VM's `SnapshotReverted=False/OperationIdentityUnsupported` (and `Ready=False`); power and size are still reconciled |
| Image prepare (a VM with `spec.imageRef` on a Provider that advertises image import) | VirtualMachine controller | the VM's `Ready=False/OperationIdentityUnsupported`; the VM is not created |
| Clone | VMClone controller | the VMClone's `Ready=False/OperationIdentityUnsupported`, phase `Pending`; no target is created |
| Migration (snapshot, disk export and import, staged and landed disk deletion), when its source or target Provider requires operation identity | VMMigration controller | the VMMigration's `Ready=False/OperationIdentityUnsupported`; its phase does not advance |
| Adoption (owner transfer) | VMAdoption controller | the Provider's `status.adoption.message` |

Each refusal has a fixed message and records one Warning event with reason
`OperationIdentityUnsupported` when it begins. The hardware upgrade kind has no manager
call at all. The Provider client refuses the same calls again
(`contracts.ErrOperationIdentityNotImplemented`) as a second lock.

**Task binding.** When an attempt is recorded `Sent`, the controller also records
`taskTag`: an HMAC-SHA256 under the intent key, with its own domain-separation prefix
(`virtrigaud.io/operation-task/v2` and a NUL byte), over the attempt's op_id, kind and
number, the VM's UID, the task reference and a Reconfigure answer's honest-result and
restart-required flags. (Version 1 covered the first five fields; a version 1 tag does not
verify.) A task reference is polled, or used to
confirm a Delete, only when its tag verifies and no other marker of the VM names the same
reference; otherwise the VM is held as `OperationStateLost` and nothing is polled or
sent. So copying the Create's task reference into the Delete marker, editing a task
reference, stripping its tag or reusing another attempt's tag cannot release a VM.

The markers are not signed or authenticated: anyone allowed to write
`virtualmachines/status` can edit them (the bundled editor and admin roles grant only
`get` on it). The controller holds the VM on the edits it can detect: a removed Create
marker; an id, a confirmed host or a `Bound` placement without a successful Create
marker; a Delete recorded `Succeeded` that its intent and the Provider do not confirm
(`OperationStateLost`). It cannot detect every edit: a principal with status write access
that also rewrites the placement and conditions consistently can still cause a duplicate
Create, or, by removing the Create marker and the binding, id and confirmed host, a
release without a Delete. The task reference itself is bound (above), but `TaskStatus`
is not yet bound to an op_id on the wire: the host answers for whatever task reference it
is asked about. The host's own de-duplication of a VM's operations, and a host-side
binding of tasks to operations (a follow-up for the agent contract), are the backstop.

### Intent Secrets

The exact request bytes of each attempt in flight (they can carry cloud-init user data)
and its digest salt are kept in a Secret, never in the VM's status:

- name `vrtg-op-<opID>`, type `virtrigaud.io/operation-intent`, in the Provider's
  namespace, owned by the Provider (garbage-collected with it);
- labels `virtrigaud.io/operation-intent: "true"`, `virtrigaud.io/vm-uid`,
  `virtrigaud.io/operation-kind` and `app.kubernetes.io/managed-by: virtrigaud`;
  annotations naming the VM's namespace and name;
- data `request` (the exact serialised request), `salt`, `identity` (the attempt's grant
  identity) and `tag`;
- immutable (written once).

It is written before the marker leaves `Prepared` and deleted once the marker is
`Terminal`. The bytes are never logged or put in an event or condition. A request larger
than 768 KiB is not stored: the VM is held as `OperationRequestTooLarge` and nothing is
sent.

**Authentication.** `tag` is an HMAC-SHA256 over a domain-separated encoding (prefix
`virtrigaud.io/operation-intent/v1` and a NUL byte, then each field length-prefixed) of
the Secret's namespace and name, the VM's namespace, name and UID, the operation kind,
and the exact identity, salt and request bytes. The key never leaves the manager: the
development signer derives it with HKDF-SHA256 from its anchor key under that label, so
it survives a manager restart and is independent of every grant-signing key. This is a
development arrangement only: the MAC key and the anchor signing key then come from one
secret (one key material, two purposes). A production key custody must supply its own,
separate MAC key. Rotating the anchor key changes the MAC key, so every attempt in flight
then fails authentication and is held as `OperationIntentTampered` (it fails safe; finish
or release in-flight attempts before rotating). The grant
signer itself is not used, because grants are signed over the `GrantBody` bytes with no
domain-separation prefix, by a short-lived leaf key that a restart replaces: an intent
signature with it would be neither separated from grant signatures nor verifiable after
the restart it must survive. An intent tag (32 bytes) is never a DER ECDSA grant
signature, and a grant signature never opens as an intent tag; a test pins both. Before
an attempt is re-sent or a Delete's release is confirmed, the Secret must authenticate,
and its identity must name the VM's UID, the Provider's UID, the marker's op_id, kind,
attempt, host and binding, and the host's current enrolment; a Delete's request must name
the VM the Delete is routed to now, and a Create's request the marker's host and the VM as
owner. On any failure nothing is sent, the Secret is kept as evidence, the VM is held as
`OperationIntentTampered`, and a Warning event (with no Secret content) is recorded when
the hold begins. Without the key (a release build, or no signer) nothing is stored or
sent. A principal that can write Secrets in the Provider's namespace can therefore delete
an intent (the VM is held as `OperationIntentLost`) or replace it (held as
`OperationIntentTampered`), but cannot make the manager sign other bytes.

**Confidentiality.** For a VM in another namespace than its Provider (a consumer
VirtualMachine), the request, including cloud-init user data that came from a Secret in
the VM's own namespace, is copied into the Provider's namespace for as long as the attempt
is in flight (and, for an abandoned attempt, until the sweeper removes it). Anyone allowed
to read Secrets in the Provider's namespace can read it. Restrict Secret `get`, `list` and
`watch` in Provider namespaces to the manager and the administrators who already may see
every consumer's user data, and enable encryption at rest for Secrets in etcd.

The Provider controller sweeps, at each reconcile of a Provider whose type requires
operation identity, the intent Secrets it owns that are older than 10 minutes and that its
VM no longer needs (no non-terminal marker names it, and it is not a Delete awaiting its
finalizer's release). It finds the VM by the Secret's VM-UID label and reads it from the
API server; a Secret whose VM annotations disagree with that VM is skipped and logged,
never deleted. A manager that stopped between writing a Secret and its marker, or between
a `Terminal` marker and the Secret delete, leaves one behind.

### RBAC

The manager's role gains `delete` on Secrets, granted in a rule of its own (no `patch`, no
`deletecollection`). RBAC cannot scope a verb by name prefix, label or Secret type, so the
rule covers every Secret the role covers. With the chart's cluster scope
(`rbac.scope=cluster`, a ClusterRole) that is every Secret in the cluster; with the
namespace scope it is every Secret in the namespaces the Role is bound in. RBAC rules are
additive: together with the existing `create`, `get`, `list`, `update` and `watch`, the
manager's effective permission on Secrets is their union, which is now every verb but
`patch` and `deletecollection`. The restriction to intent Secrets is in the code only: the
manager deletes only Secrets of type `virtrigaud.io/operation-intent` with the intent
label, the `vrtg-op-` name prefix and the expected VM's UID label, with the UID it read as
a delete precondition. Provider pods have no Secrets access.

### Holds and how to clear them

A held VM reports `Ready=False` with one of these reasons (phase `Failed`) and sends no new
attempt:

| Reason | Cause | What clears it |
|---|---|---|
| `OperationSignerUnavailable` | No operation signer (every release build). | Start a development manager with `--dev-operation-signer-key-file`. |
| `OperationIdentityMismatch` | The Provider's advertised gates do not match its type. | Fix the Provider so it advertises `supports_` and `requires_operation_identity` and no fencing. |
| `OperationEnrolmentUnavailable` | No enrolment for the VM's host. | Add the host to the enrolment file and restart the manager. |
| `OperationStateLost` | The VM records a binding, an id, a confirmed host or a `Bound` placement without a successful Create marker, or a Delete recorded `Succeeded` that its intent and the Provider do not confirm (its status was edited or restored). | Operator. |
| `OperationIntentLost` | A marker in flight names an intent Secret that does not exist (for example after an etcd restore that did not restore it). | Operator, or restore the Secret. |
| `OperationIntentTampered` | An intent Secret does not authenticate, or names another VM, Provider, host, binding, attempt or target than its marker (also after the anchor key was rotated). The Secret is kept. | Operator: investigate who wrote Secrets in the Provider's namespace (the Secret is the evidence), check the host by hand, release the VM as below, then delete the Secret by hand: `kubectl -n <provider namespace> delete secret vrtg-op-<opID>`. The sweeper also keeps, and announces once with an `OperationIntentKept` Warning event on the Provider, an intent Secret whose VM annotations disagree with its VM-UID label; delete it by hand the same way once investigated. |
| `OperationRequestTooLarge` | The request (with its user data) is over 768 KiB. | Make the user data smaller. |
| `OperationFailedDirty` | The Provider reported the operation failed after an irreversible step began. | For a power or Reconfigure attempt: check the VM on its host, then acknowledge that exact attempt with `kubectl annotate vm <name> virtrigaud.io/acknowledge-operation=<its opID>` (from `status.operationAttempts`). The manager removes the annotation, records an `OperationAcknowledged` event, and, after the next Describe, a new attempt of that kind may follow. The annotation does nothing for another op_id or for a Create or Delete. For a Create or Delete: operator. |
| `OperationOutcomeUnknown` | The Provider cannot tell what the attempt did (an `Unknown` outcome, or a refusal of a re-delivery that proves nothing about an earlier one). The same attempt is asked about, or re-sent, every 5 minutes. | Resolves itself if the Provider later reports a terminal outcome; otherwise operator. |
| `OperationUnknownToAgent` | The host has no record of the attempt's task. | Operator. |
| `OperationProviderMismatch` | The host refused the operation as coming from another Provider. The same attempt is re-sent every 5 minutes. | Fix the host's enrolment, or operator. |
| `OperationDeleteUnconfirmed` | The VM is being deleted and its host answers that there is no such VM, or its Provider is gone (see above). The same attempt is re-sent every 5 minutes. | Operator. |
| `OperationIdentityUnsupported` | An operation with no signed path was asked of the VM (an image prepare; a snapshot revert shows on `SnapshotReverted`). Nothing is sent. | Remove the request (for example use an imported disk instead of an image), or use another Provider. |

**Who can acknowledge.** The `virtrigaud.io/acknowledge-operation` annotation is part of
the VirtualMachine's metadata: anyone allowed to update the VirtualMachine can set it,
including a tenant whose VM runs on a Provider in another namespace (a consumer of a shared
Provider). It names one attempt's exact op_id and only ends a `FailedDirty` hold of a power
or Reconfigure attempt, but it is an operator decision, so restrict it. A
ValidatingAdmissionPolicy can allow only a group of operators to set or change it (removing
it, as the manager does, stays allowed):

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: virtrigaud-acknowledge-operation
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["infra.virtrigaud.io"]
        apiVersions: ["v1beta1"]
        operations: ["CREATE", "UPDATE"]
        resources: ["virtualmachines"]
  variables:
    - name: key
      expression: "'virtrigaud.io/acknowledge-operation'"
    - name: newValue
      expression: >-
        has(object.metadata.annotations) && variables.key in object.metadata.annotations
        ? object.metadata.annotations[variables.key] : ''
    - name: oldValue
      expression: >-
        oldObject != null && has(oldObject.metadata.annotations) && variables.key in oldObject.metadata.annotations
        ? oldObject.metadata.annotations[variables.key] : ''
  validations:
    - expression: >-
        variables.newValue == '' || variables.newValue == variables.oldValue ||
        'virtrigaud:operation-acknowledgers' in request.userInfo.groups
      message: "only members of virtrigaud:operation-acknowledgers may set virtrigaud.io/acknowledge-operation"
      reason: Forbidden
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: virtrigaud-acknowledge-operation
spec:
  policyName: virtrigaud-acknowledge-operation
  validationActions: ["Deny"]
```

Adapt the group to your operators and bind the policy to the namespaces that run VMs of
Providers that require operation identity. The policy was checked with
`kubectl apply --dry-run=client` and `--dry-run=server` against an ephemeral local test API
server, which also denied the annotation to a user outside the group and allowed it to one
inside it and its removal.

"Operator" means: there is no retire, cancel or resolve path yet, so the controller cannot
learn what happened on the host. Check the host by hand, then release the VirtualMachine
with `virtrigaud.io/orphan-on-delete=true` (leave whatever is on the host) or
`virtrigaud.io/force-delete=true` (the controller stops waiting; remove anything left by
hand) and delete it. Do not remove or edit `status.operationAttempts` to retry: that is
itself held as `OperationStateLost`.

A not-performed outcome is not a hold: the VM reports `Ready=False/OperationRetryBackoff`
until the next attempt is sent, and `OperationInProgress` while an attempt is in flight.

## Known limits

- No receipts, ledger marks, release, retire-if-unknown or cancel grants: only
  `GRANT_KIND_OPERATION` is minted or accepted.
- Every Provider answer, including a typed task outcome or a grant refusal reason, is the
  Provider's unsigned claim.
- No fencing: the epoch fields carry identity only, with no ordering.
- `authority_generation` is not yet bound to the generation the issuer certificate names;
  it is only checked to be at least 1, and a verifier refuses a configured minimum above 1
  until that binding exists.
- Only the seven VM lifecycle kinds are sent as signed operations; snapshots, clones,
  image prepares, migrations and adoption are refused locally for a Provider that
  requires operation identity. No VirtualMachine field asks for a `Reboot` yet.
- A held power attempt blocks later power changes of the VM (only a Delete supersedes it).
  A task that the host keeps reporting as running blocks the VM, the Delete included, for
  up to one hour for a power or Reconfigure attempt; a Create's has no deadline.
- The acknowledgement annotation is restricted by RBAC on VirtualMachines only; restrict it
  further with an admission policy (above).
- A superseded attempt's outcome stays unknown: if it still runs on the host after the
  Delete, only the host's own de-duplication by VM and binding stops it.
- A Provider's typed outcomes and refusal reasons are unsigned claims: a Provider that
  falsely answers `Retired` or `FailedClean` makes the controller mint a new attempt.
- Attempt markers are not signed or authenticated: detected edits hold the VM, but a
  principal with `virtualmachines/status` write access can still cause a duplicate or a
  skipped operation (see above).
- Intent Secrets of consumer VMs copy their user data into the Provider's namespace.
- The `delete` verb on Secrets is not scoped to intent Secrets by RBAC.
- `TaskStatus` is not bound to an op_id on the wire; the controller binds the task
  reference on its side only.
- In development builds the intent MAC key is derived from the anchor signing key.
- A Delete releases the finalizer only when it is confirmed: a host that answers that the
  VM is not there, or a Provider that is gone, holds the delete until an operator releases
  it. There is no authenticated "deleted" answer yet.
- A VM whose Create attempts were not performed stays on its pending host; it is never
  re-scheduled automatically.
- A restore of the VM's status to before its first Create attempt, together with the loss
  of its placement, is indistinguishable from a new VM: a new attempt is minted, and only
  the host's own state of the VM protects against a second create.
- The outer-request rule is about decoded presence: an outer field encoded on the wire with
  its default value (for example an empty string) is not seen. This is harmless because a
  verifier's caller acts only on the decoded request the verifier returns.
- The development signer's anchor certificate gets a fresh validity on each load and always
  serial 1. Pinning still works (the subject and key are stable), but the certificate
  bytes are not.
