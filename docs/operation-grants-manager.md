# Operation grants: manager side (development builds only)

[`operation-grants-wire-shape.md`](operation-grants-wire-shape.md) describes the operation
grant messages in the provider contract. This page describes the manager-side code that
mints, sends and verifies them, following the operation-identity design of ADR-0016
(status: Proposed).

**It is development-only, and no controller uses it.** A release build has no grant signer,
refuses the development signer flag, and refuses to send a signed operation. No bundled
provider reports operation identity, and no controller builds an operation-identity client,
so a running manager, release or development, sends exactly the calls it sent before.

## How release builds are kept unchanged

| Lock | Where | Release build (no `virtrigaud_dev` tag) |
|---|---|---|
| No signer | `internal/operation/signer_release.go` | `operation.SignerCompiled` is `false`; the only signer, `DevSigner`, is not compiled in. `operation.LoadSigner` refuses any key file with `operation.ErrNoSigner` and never reads it. |
| Startup refusal | `cmd/manager/main.go` | `--dev-operation-signer-key-file` set: the manager logs `invalid --dev-operation-signer-key-file` and exits 1 before it connects to the cluster, like `--enable-dev-provider-types`. |
| No sending | `internal/transport/grpc/client_operation.go` | `NewOperationIdentityClient` and `Client.SendOperation` return `operation.ErrNoSigner` and send nothing. |
| No new path | every controller | Nothing calls `NewOperationIdentityClient`, `BuildOperation` or `SendOperation`. Every existing client keeps its behaviour: no `operation_envelope` is set on any call. |

The tagged and untagged builds are both tested: `go test ./internal/operation/
./internal/transport/grpc/ ./cmd/manager/` and the same with `-tags virtrigaud_dev`.

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
`PreparedOperation` formats with the bytes redacted.

## Verifying a grant

`operation.Verifier` is the verifier that a test fake and a later per-host agent share. It
pins trust anchors and the installation, Provider, host and enrolment it verifies for, and
refuses a call (the call is not performed) with a `*operation.Refusal` naming the reason and
the rule:

| Refused | Reason |
|---|---|
| No `operation_envelope` on the call | `IDENTITY_REQUIRED` |
| Another field of the outer request set beside the envelope, or unknown fields on it | `GRANT_MISMATCH`, answered as `INVALID_ARGUMENT` |
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

A development manager started with the flag loads the file, refuses a malformed one at
startup (without quoting it), and logs a warning that it is not for production use. It does
not use the signer for anything else yet.

## Capability gates

The transport client reads `GetCapabilitiesResponse` fields 31–34 into
`contracts.Capabilities` (`SupportsOperationIdentity`, `RequiresOperationIdentity`,
`SupportsFencing`, `RequiresFencing`). They are a Provider's unauthenticated claims: they
are hints that must match the protection a Provider's type requires, and they never lower
it. No controller reads them yet, and every bundled provider reports `false`.

## Known limits

- No receipts, ledger marks, release, retire-if-unknown or cancel grants: only
  `GRANT_KIND_OPERATION` is minted or accepted.
- Every Provider answer, including a typed task outcome or a grant refusal reason, is the
  Provider's unsigned claim.
- No fencing: the epoch fields carry identity only, with no ordering.
- No durable attempt state: nothing records an attempt before it is sent.
