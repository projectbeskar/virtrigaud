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
a VM's `Create` and `Delete` as signed operations and records each attempt before it is
first sent. The other mutating calls of such a VM (power, reconfigure, snapshots, image
prepare, ...) are not yet sent as signed operations: the operation-identity client refuses
them locally (`contracts.ErrOperationIdentityRequired` or
`ErrOperationIdentityNotImplemented`) and nothing is sent unsigned.

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
and the Provider's `outcomeReason` and `taskRef`. `status.placement.bindingID` identifies
the VM's placement; it is drawn with the VM's first Create attempt on its host, written in
the same status write, and unchanged by re-sends. Neither field is ever written for any
other Provider, and neither carries request bytes, a salt or a digest.

| Phase | Meaning |
|---|---|
| `Prepared` | The attempt's identity and exact request are stored. It may or may not have been delivered. |
| `Sent` | The Provider accepted the operation and returned a task; its outcome is not known yet. |
| `Terminal` | The outcome is known: `Succeeded`, `FailedClean`, `FailedDirty`, `Retired`, `Cancelled`, or `Rejected` (the host refused the first delivery with a reason that proves it was not performed). |

Every marker write is a JSON Patch on the status subresource whose `test` operations
require the markers (and, where written, `status.placement` and `status.id`) to be exactly
as the reconcile read them. A write that fails sends nothing. The order of one attempt is:

1. Decide: a new attempt only when no attempt of any kind is in flight for the VM, and the
   latest attempt of the kind ended not performed (`Retired`, `FailedClean`, `Cancelled`,
   `Rejected`) and its backoff (15 s doubling per attempt, at most 5 min) is over, or there
   is none. The `op_id` and digest salt are drawn once.
2. Persist: the intent Secret (below), then the `Prepared` marker.
3. Transmit: the stored bytes under a freshly minted grant. A re-send (after a transport
   error, `VM_BUSY`, a lost reply or a manager restart) sends the same bytes with the same
   `op_id`; a host answers a known `op_id` from its record and never executes it twice.
4. Close: `Sent` with the task reference, then `Terminal` with the outcome, then the intent
   Secret is deleted. A successful Create writes `Terminal`, `status.id` and the confirmed
   placement in one status write.

A VM's finalizer is released only once a Delete attempt is `Terminal/Succeeded`, or when no
Create of the VM was ever performed (it has no Create attempt, or its latest one was not
performed, and no id is recorded). A Delete waits for a Create in flight. Orphan-on-delete
and force-delete work as for every Provider.

The markers are not signed: anyone allowed to patch `virtualmachines/status` can edit them.
An edit makes the controller hold the VM (below), or makes the host refuse a delivery; it
never makes the controller send an operation again under a new identity.

### Intent Secrets

The exact request bytes of each attempt in flight (they can carry cloud-init user data)
and its digest salt are kept in a Secret, never in the VM's status:

- name `vrtg-op-<opID>`, type `virtrigaud.io/operation-intent`, in the Provider's
  namespace, owned by the Provider (garbage-collected with it);
- labels `virtrigaud.io/operation-intent: "true"`, `virtrigaud.io/vm-uid` and
  `virtrigaud.io/operation-kind`; annotations naming the VM's namespace and name;
- data `request` (the exact serialised request), `salt`, `identity` (the attempt's grant
  identity) and, for an asynchronous Create, `answer-id` (the VM id the Provider answered).

It is written before the marker leaves `Prepared` and deleted once the marker is
`Terminal`. The bytes are never logged or put in an event or condition. Restrict who can
read Secrets in a Provider's namespace accordingly.

The Provider controller sweeps, at each reconcile of a Provider whose type requires
operation identity, the intent Secrets it owns that are older than 10 minutes and that no
non-terminal marker of their VM names (the VM is read from the API server): a manager that
stopped between writing a Secret and its marker, or between a `Terminal` marker and the
Secret delete, leaves one behind.

### RBAC

The manager's role gains `delete` on Secrets, granted in a rule of its own (no `patch`, no
`deletecollection`). RBAC cannot scope a verb by name prefix, label or Secret type, so the
rule covers every Secret the role covers; the manager deletes only Secrets of type
`virtrigaud.io/operation-intent` with the intent label, the `vrtg-op-` name prefix and the
expected VM's UID label, with the UID it read as a delete precondition. Provider pods have
no Secrets access.

### Holds and how to clear them

A held VM reports `Ready=False` with one of these reasons (phase `Failed`) and sends no new
attempt:

| Reason | Cause | What clears it |
|---|---|---|
| `OperationSignerUnavailable` | No operation signer (every release build). | Start a development manager with `--dev-operation-signer-key-file`. |
| `OperationIdentityMismatch` | The Provider's advertised gates do not match its type. | Fix the Provider so it advertises `supports_` and `requires_operation_identity` and no fencing. |
| `OperationEnrolmentUnavailable` | No enrolment for the VM's host. | Add the host to the enrolment file and restart the manager. |
| `OperationStateLost` | The VM records a binding or an id but no Create marker (its status was edited or restored). | Operator. |
| `OperationIntentLost` | A marker in flight names an intent Secret that does not exist (for example after an etcd restore that did not restore it). | Operator, or restore the Secret. |
| `OperationFailedDirty` | The Provider reported the operation failed after an irreversible step began. | Operator. |
| `OperationOutcomeUnknown` | The Provider cannot tell what the attempt did (an `Unknown` outcome, or a refusal of a re-delivery that proves nothing about an earlier one). The same attempt is asked about, or re-sent, every 5 minutes. | Resolves itself if the Provider later reports a terminal outcome; otherwise operator. |
| `OperationUnknownToAgent` | The host has no record of the attempt's task. | Operator. |
| `OperationProviderMismatch` | The host refused the operation as coming from another Provider. The same attempt is re-sent every 5 minutes. | Fix the host's enrolment, or operator. |

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
- Only `Create` and `Delete` are sent as signed operations; the other kinds of a VM whose
  Provider requires operation identity are refused locally (power state changes and
  resizes of such a VM fail with a provider error).
- A Provider's typed outcomes and refusal reasons are unsigned claims: a Provider that
  falsely answers `Retired` or `FailedClean` makes the controller mint a new attempt.
- Attempt markers are not signed.
- A Delete releases the finalizer only on `Succeeded`: a host that answers that the VM is
  not there holds the delete until an operator releases it.
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
