# Per-host agent enrolment: manager side (development builds only)

A clustered Provider that runs one agent per host (`spec.type: cloudhypervisor`,
`spec.topology: cluster`) must authenticate each host's agent. This page
describes how the manager enrols an agent: it authorises the agent's
certificate request, has it signed by a dedicated host-agent subordinate CA
(through cert-manager or an external PKI), validates the issued certificate
and pins the agent's key.

Everything here is used only by development builds: a manager built with the
`virtrigaud_dev` build tag and started with the `--dev-host-enrolment-*` flags.
A release manager refuses those flags at startup, renders no agent credentials
into any inventory, and leaves the new Host fields unused. The agent side
(generating the key and the CSR on the host, installing the certificate) is
in [`per-host-agent-development.md`](per-host-agent-development.md#enrolment).

## Host fields

| Field | Meaning |
|-------|---------|
| `spec.identity` | The host's identity anchor (TPM endorsement-key hash, hardware serial, administrator-assigned UUID). Required before the host can enrol. Set once: a CEL rule refuses changing or removing it. A replaced machine is a new identity (a new Host object), and never inherits the old host's pins. |
| `spec.revoked` | When `true`, the manager retires the host's enrolment id, removes its pinned keys and any pending or authorised record, never approves a request for it again, and renders it as unroutable. A CEL rule refuses clearing it: enrol a replacement as a new Host. Revoking the certificate at the CA is not done by this manager yet. |
| `status.agent` | A mirror of the pins Secret (see below): `mode`, `enrolmentID`, `pendingEnrolmentID`, `requestURIs`, `keyPins`, `certificateExpiry`. Public identifiers only. |
| condition `AgentEnrolled` | `True`/`Enrolled` once a key is pinned. `False` with `IdentityMissing`, `Revoked`, `CertificateExpired`, `AwaitingCertificateRequest`, `AwaitingCertificate`, `CertificateRequestDenied` or `CertificateRejected`. |

Libvirt hosts are unchanged: they get no `status.agent`, no condition and no
pins Secret.

## The enrolment record

When a host has an identity and is not revoked, the manager draws a 16-byte
enrolment id and stores a pending record in the host's pins Secret (see
below). The record
is the enrolment id, the Provider's UID, the manager's install id (from the
development operation signer) and the mode. `status.agent.requestURIs` lists
the exact URI subject alternative names the agent's CSR must carry, and
nothing else:

```
spiffe://<trust domain>/virtrigaud/host/<namespace>/<host>
urn:virtrigaud:enrolment:<32 hex>
urn:virtrigaud:provider:<Provider UID>
urn:virtrigaud:install:<32 hex>
urn:virtrigaud:mode:identity
```

The subordinate CA signs these URIs into the agent certificate, so whoever
holds the certificate can derive the enrolment record from it offline, and the
CA authenticates it. An enrolment id is never drawn twice for a host. When an
authorised request ends without a pinned key (its certificate is refused, or
the request is deleted or fails), its record is retired and a new one drawn,
so a CA-valid certificate that was never pinned never shares a record with the
one that is. A new request must then be built from the new
`status.agent.requestURIs`.

## Authorisation rules

The manager approves a `CertificateRequest` (cert-manager condition
`Approved`) only if every rule holds; otherwise it sets `Denied` with one of
the reasons below. Nothing else approves these requests: **cert-manager's
default approver must be disabled for the host-agent issuer** (run
cert-manager with `--controllers=*,-certificaterequests-approver` and approve
other issuers with approver-policy, or make sure the default approver has no
`approve` permission on this signer). Otherwise cert-manager approves before
the manager can check anything.

The requester is checked first, before the manager reads any Host, Provider
or Secret: a request from anyone else is denied with no trace on the Host (no
state write, no event, no hint whether the Host exists).

| Rule | Denial reason |
|------|---------------|
| The request names a Host (annotation `enrolment.virtrigaud.io/host: <namespace>/<name>`) | `HostNotNamed` |
| The requester (`spec.groups`, recorded by cert-manager's admission webhook) is in the enrolment administrator group | `RequesterNotAuthorised` |
| The requester may use the `enrol` verb (for a Host that is not enrolled) or the `rekey` verb (for an enrolled Host) on that Host (`hosts` in `infra.virtrigaud.io`, the Host's namespace and name), checked with a SubjectAccessReview built from the request's `spec.username`, `spec.uid`, `spec.groups` and `spec.extra`; one of the two is checked before the Host is read | `RequesterNotAuthorisedForHost` |
| The Host exists and is not being deleted | `HostNotFound` |
| The Host's Provider runs a per-host agent | `HostNotAgentManaged` |
| The Host has `spec.identity` | `HostIdentityMissing` |
| The Host is not revoked | `HostRevoked` |
| A record is pending, no other request is in flight, and the request's record was not retired | `NoPendingEnrolment`, `EnrolmentInFlight`, `EnrolmentRecordRetired` |
| For a re-key of an enrolled Host: the Host is enrolled with its own Provider, its start epochs are not exhausted, fewer than 8 predecessor enrolments are held, the requester may use the `rekey` verb on the Host, and a re-key window is open (see [Re-keying an enrolled Host](#re-keying-an-enrolled-host)) | `ProviderChanged`, `StartEpochExhausted`, `TooManyPredecessorEnrolments`, `RequesterNotAuthorisedToRekey`, `NoRekeyWindow` |
| The request's key is not the Host's pinned key nor one of its last 32 replaced keys | `KeyPreviouslyPinned` |
| `spec.request` is one PEM CSR | `RequestInvalid` |
| The CSR's signature verifies with its own key (proof of possession) | `ProofOfPossessionFailed` |
| The key is ECDSA P-256/P-384, Ed25519, or RSA of 2048 to 8192 bits | `KeyUnsupported` |
| The subject alternative names are exactly the host URI and the pending record (no DNS, IP or e-mail names) | `SubjectAltNamesRefused` |
| The subject is empty or a common name equal to the host name | `SubjectRefused` |
| The CSR asks for no extension other than the subject alternative names (and basic constraints with the CA bit clear) | `ExtensionRefused` |
| No CA certificate (`spec.isCA` false, no CA bit in the CSR) | `CARequested` |
| `spec.usages` include `server auth` and `digital signature`, and neither `client auth` (the per-host agent is only ever a TLS server) nor `cert sign` or `crl sign` (`key encipherment` is tolerated, nothing else) | `UsagesRefused` |
| `spec.duration` is set and at most 30 days (`720h`) | `LifetimeRefused` |

Before approving, the manager records the authorisation (request UID, record,
key pin) in the pins Secret. A denial never changes the pins.

## Re-keying an enrolled Host

An enrolled Host can be enrolled again with a new key, for example after its
key was lost or replaced, its disk reinstalled, or its state restored from a
snapshot. This is a re-key: a new enrolment of the same `spec.identity`. A
replaced machine is not re-keyed: it gets a new `spec.identity`, so a new Host.
A re-key always needs a new key: a request whose key is the current one, or
one of the last 32 replaced keys, is denied with `KeyPreviouslyPinned`. A
host restored from a snapshot that kept its old key generates a new one
(`vrtg-agent enroll csr`) before it is re-keyed.

A re-key is opened explicitly and needs its own permission:

1. Open a re-key window: `vrtg host rekey --host <name>`, which sets the
   Host's annotation `enrolment.virtrigaud.io/rekey-requested` to the current
   time (RFC 3339). The manager opens a window that closes one hour after
   that time and publishes a record for the Host's next enrolment in
   `status.agent.pendingEnrolmentID` and `status.agent.requestURIs`, while
   `status.agent.enrolmentID` and `AgentEnrolled=True` still show the current
   enrolment (its message says until when the window is open). A value is
   used once; a time more than an hour old, more than five minutes in the
   future, or not a time opens nothing. When the window closes without a
   re-key, its record is retired, and a certificate issued for it after the
   window closed is refused. Each of these is reported with an
   `AgentRekeyWindow` event naming the annotation value. Without an open
   window, a request for an enrolled Host is denied with `NoRekeyWindow`.
2. Submit the new key's request exactly as for a first enrolment (a CSR
   carrying those URIs, `vrtg host enroll`). It is authorised under the same
   rules (group, proof of possession, names, usages, lifetime), except that
   the requester needs the `rekey` verb on the Host instead of `enrol`
   (`RequesterNotAuthorisedToRekey` otherwise). The `enrol` verb alone cannot
   re-key an enrolled Host.

Opening a window needs only `update` or `patch` on the Host (the annotation);
the window itself grants nothing: the re-key request still needs the `rekey`
verb, the administrator group and every other rule. The API server's audit
log records who set the annotation.

Also:

- A re-key never changes the Provider. A Host enrolled with another Provider
  than the one it names now is reported with `AgentEnrolled=False`, reason
  `ProviderChanged`; no record is drawn for it and every request is denied
  with `ProviderChanged`. Its enrolment is not trusted by the new Provider. To
  move a host to another Provider, revoke the Host and create a new one (see
  [Start epochs](#start-epochs) for the re-created Host).
- At most 8 predecessor enrolments are held per Host. A re-key that would
  exceed this is denied with `TooManyPredecessorEnrolments`; none is dropped.
- Only one request at a time: another request while one is authorised is
  denied with `EnrolmentInFlight`.
- The Provider check and the predecessor bound are checked again when the
  certificate is issued; a certificate that fails them is refused.

When the re-key's certificate is validated, in one write of the pins Secret:

- the new key replaces the old one: the old key is no longer pinned (its pin
  is kept on the list of replaced keys), and the next inventory render
  carries only the new key, so the Provider closes the agent's old
  connection and accepts only the new certificate;
- the replaced enrolment is added to the Host's predecessor enrolments
  (oldest first). An operation the manager sent under it is still asked about
  by its identity, and its VM is held until it is resolved; it is never sent
  again under the new enrolment. Both enrolment ids stay on the never-reuse
  list;
- grants for the new enrolment are issued at its record's start epoch;
- the window closes;
- the store positions the manager keeps for the Host are kept too, so the
  continuity checks of signed answers run across the re-key.

The manager logs the re-key and emits an `AgentRekeyed` event on the Host
naming the requester (the request's username), the request, the new and the
replaced enrolment ids and the new start epoch, and, in the log, the new key's
pin. Neither carries key or certificate material.

Take the certificate to the host with
`vrtg host enroll --request <name> --certificate-out <file>`, which writes it
only once the manager has pinned its key for the Host, and install it with
`vrtg-agent enroll install --replace`; the agent re-enrols its store, freezing
the replaced enrolment's open operations
([details](per-host-agent-development.md#re-enrolment-freeze)).

### Confirming a predecessor's freeze

A predecessor stays listed until the host has discarded its records, in
three steps:

1. **Decision.** The manager confirms the freeze of a predecessor once it has
   been one for at least two minutes plus a 30-second margin for clock skew
   between manager replicas, and no attempt that could still need it is
   open. A predecessor recorded without a time (state written before times
   were kept) is stamped with the current time and waits the full period.
   The attempts that hold a predecessor are those of VirtualMachines of the
   Host's Provider (by `spec.providerRef` or `status.boundProvider`, in any
   namespace) that are open (not `Terminal`) on the Host, and the `Terminal`
   Delete attempts on the Host of VirtualMachines still being deleted. Each
   such attempt holds only the predecessor it was minted under, which the
   manager reads from the attempt's authenticated intent; an attempt whose
   intent cannot be read or authenticated holds every predecessor. The
   manager first looks at its cache: if the cache already shows every due
   predecessor held, it waits without reading the API server. Otherwise it
   confirms with a paginated read from the API server (500 objects a page);
   an intent authenticated in the cache pass is not read again.
   The decision is recorded in the pins Secret
   (`confirmedFrozenEnrolmentIDs`, with the time each predecessor became one
   in `predecessorSince`) and reported with an `AgentFreezeConfirmed` event
   naming the enrolment id. While a predecessor waits, the check is repeated
   after one minute, doubling up to ten minutes, and the decision survives a
   manager restart.
2. **Delivery.** The confirmed predecessors (at most 16) and the Host's
   current enrolment id are rendered into the Provider's host inventory
   (`freezeConfirmation`) when the inventory is next rendered (at least every
   60 seconds), which the Provider follows; it relays them to the agent with
   the next call to the host (`agent.v1 AgentStatus`), once per connection
   and again when they change. The agent discards the records of the
   predecessors it holds as frozen predecessors of that current enrolment
   and ignores everything else
   ([details](per-host-agent-development.md#freeze-confirmation),
   [Provider side](clustered-agent-provider-development.md#freeze-confirmations)).
3. **Removal.** The manager's heartbeat asks the host, with a signed request,
   about an operation id it never used under each confirmed predecessor. Only
   a verified signed answer from a ready agent, answering under another
   enrolment, that the predecessor is `UNKNOWN_TO_AGENT` (its records are
   gone) removes it from the pins Secret (written with its resourceVersion
   as precondition), with an `AgentPredecessorDiscarded` event and an audit
   log line carrying the signed answer. A `NO_RECORD` (still held), an
   unsigned answer, an answer from an agent that is not ready or that
   answers under the predecessor itself, or one about another operation or
   enrolment removes nothing. A probe that removes nothing is repeated after
   one minute, doubling up to 30 minutes, per predecessor. A removed
   predecessor is kept in a bounded list (`discardedEnrolmentIDs`, the last
   32): a VM whose attempt was minted under it is then reported as an attempt
   of a predecessor the host has discarded, and held as such, not as a
   tampered intent.

Once removed, a predecessor no longer counts towards the bound of 8, so a
Host that reached it can be re-keyed again. The relay is unsigned: a
Provider that confirms a predecessor the manager did not confirm, or relays
a stale confirmation, can only make the host discard records early. Their
operations are then answered `UNKNOWN_TO_AGENT` and the manager holds their
VMs; nothing runs twice. Such a predecessor is not removed from the pins
Secret, since the manager only probes those it confirmed. A Provider or an
agent that predates this never confirms anything: predecessors then stay
listed, as before.

An attempt minted just before a re-key is never first sent under the new
enrolment: after recording the attempt and before its first send, the
manager reads the Host's enrolment again and, if it changed, does not send
it. The VM is held with the reason `OperationHostRekeyed` for an operator
(`orphan-on-delete` or force-delete), as for any attempt of a predecessor.
If the enrolment cannot be read at that point (a transient API error), the
attempt is not sent either, and the VM is not held: the next reconcile asks
about the attempt and decides again.

**Liveness cost.** An attempt held this way, or any attempt of a
predecessor, is never concluded on its own. The host's `NO_RECORD` (or
`RETIRED`) answer about an attempt under a predecessor enrolment is not taken
as proof that it was never performed, so the attempt stays open (`Prepared`
or `Sent`), its VM stays held, and the attempt keeps that predecessor from
being confirmed. The predecessor then stays listed and counts towards the
bound of 8 until an operator settles the VM: set the annotation
`virtrigaud.io/orphan-on-delete=true` and delete the VirtualMachine (the
manager forgets it and leaves whatever the host has), or set
`virtrigaud.io/force-delete` on it.
Once the VM and its attempts are gone, the predecessor is confirmed at the
next check.

### Start epochs

The manager chooses the start epoch of each enrolment and the subordinate CA
signs it into the record (`urn:virtrigaud:epoch:<n>`, present from epoch 2;
a record without it is epoch 1, so a first enrolment's names are unchanged).
The agent starts the enrolment at exactly that epoch, and grants for it are
minted at it. A host refuses a first delivery minted below its enrolment's
start epoch, so both sides must agree; the CA signature makes them agree.

Each new record starts after every epoch the manager has approved a request
at (a CA-valid certificate may exist for it even if the manager later refused
it) and after the current enrolment's. The agent refuses a certificate whose
start epoch is not after the last one its store started: `vrtg-agent enroll
install` refuses it with an error naming the epochs, without touching the
store, and a running agent refuses to re-enrol at it.

A Host that is deleted and created again under the same name loses its
enrolment state, so its first record starts at epoch 1, which a host that was
enrolled before (whose store has started a later epoch) refuses. The install
error names the host's last start epoch `N`. Set the annotation
`enrolment.virtrigaud.io/start-epoch-floor: "<N+1>"` on the new Host: the
manager draws its first record at that epoch at least, and reports it with an
`AgentStartEpochFloor` event. The annotation applies only while the Host is
fresh (no enrolment and no approved request); once it is enrolled, or a
request for it was approved, the annotation is ignored, so editing it cannot
change a re-key's epoch. It is honoured from 1 to 65536; any other value is
ignored and reported, so a re-created Host whose agent store has started an
epoch above 65536 cannot be recovered with it (not reachable today: the
predecessor bound limits one Host object to about 9 epochs). Changing it on a
fresh Host retires a pending record
drawn at a lower epoch (a request built from that record is then denied, and
a new one is needed); it never lowers an epoch.

### Refused certificates

When the manager refuses to pin an issued certificate (it does not match the
authorisation, or the Provider changed or the predecessor bound was reached
before issuance), and when a request's authorisation was withdrawn (its
re-key window closed, or its record was retired) or its enrolment was
replaced, the manager sets the condition `VirtRigaudCertificateRefused=True`
on the `CertificateRequest`. cert-manager may still show `Ready=True`: the
certificate is CA-valid, but it must never be installed. `vrtg host enroll
--request` refuses such a request in every mode, and `--certificate-out`
writes a certificate only once its key is pinned on the Host.

## Certificate renewal

An enrolled agent renews its certificate for a new key before it expires.
A renewal keeps the enrolment: the renewed certificate carries exactly the
same URI names (host, enrolment, Provider, install, mode and start epoch). It
is not a re-key: no predecessor is recorded and no epoch changes. Renewal
needs the cert-manager issuer; with only the external-PKI path configured,
the manager reports `AgentRenewalNotSupported` on the Host and the Host must
be re-keyed before its certificate expires.

1. **Request.** At two thirds of its certificate's lifetime the agent has a
   new key made (it never leaves the host) and offers a renewal request: a
   PKCS #10 request for the new key, signed by the new key, wrapped in a
   `RenewalRequest` that the agent signs with its current, pinned key, under
   the host-signed domain separator with its own body type (never a raw
   signature over the request). The heartbeat asks for it
   (`GetHostInfoRequest.renewal_version`, sent only to an agent whose last
   verified answer shows API version 3 or later) and verifies it with the
   answer: signed with the answer's own certificate, the current enrolment's
   identities unchanged, a new P-256 key, proof of possession. A renewal that
   breaks any rule makes the whole answer "no answer". A verified request is
   recorded in the pins Secret (`renewal`) with an `AgentRenewalRequested`
   event; a key that is, or was, pinned for the Host is refused
   (`AgentRenewalKeyPreviouslyPinned`).
2. **Certificate request.** The manager records the name of its
   CertificateRequest in the pins Secret, then creates it in the enrolment
   namespace under its own identity (read at start with a
   `SelfSubjectReview`; without it renewal is off), with the labels
   `enrolment.virtrigaud.io/agent=true`, `enrolment.virtrigaud.io/renewal=true`
   and `enrolment.virtrigaud.io/host-hash`, the Host annotation, the host's
   request, the usages `digital signature` and `server auth`, and the current
   certificate's lifetime (at most 30 days).
3. **Approval.** The manager approves a request labelled as a renewal only
   when all of these hold, and denies it otherwise:
   - its recorded requester (`spec.username`, written by cert-manager's
     admission webhook) is the manager's own identity, checked before
     anything else is read (`RenewalNotRequestedByManager`);
   - it is the Host's renewal in progress, by name and UID
     (`NoPendingRenewal`);
   - the stored co-signed renewal request verifies again now: signed by a
     currently pinned key, the identities of the current enrolment
     unchanged, the current certificate valid (`RenewalNotVerified`);
   - the Host exists, runs an agent, has an identity and is not revoked;
   - the request carries exactly the host's request, agent usages, no CA and
     a lifetime of at most 30 days, and its key was never pinned.

   A request labelled as a renewal that another principal created (an
   administrator included) is never approved. Approvals are suspended, as for
   enrolment, while the admission check fails. Decisions read the pins Secret
   from the API server.
4. **Issuance.** The issued certificate is validated against the current
   enrolment and the new key (chain, profile, URI names, key) and published in
   the Provider's host inventory (`renewedCertificate`) with an
   `AgentRenewalIssued` event; a certificate that fails is marked
   `VirtRigaudCertificateRefused` on the request and the renewal starts
   again (`AgentRenewalRefused`). The Provider stages it on the host.
5. **New key pinned.** Only after a GetHostInfo the manager asked (its own
   nonce), answered by the current key, returns a verified answer whose
   `staged_certificate_sha256` is the digest of the certificate the manager
   issued, is the new key pinned beside the old one (`AgentRenewalPinned`).
   Both keys are now pinned (the overlap), and the Provider puts the renewed
   certificate in use on the host.
6. **Old key unpinned.** Only after such an answer is signed by the new key
   and names nothing staged is the old key unpinned (`AgentRenewed`): the new
   key is the only pin, the old one is kept as a previous key (a re-key to it
   is refused), and the certificate expiry becomes the renewed one's.

**Overlap.** The overlap is expected to end within a heartbeat or two. After
24 hours the manager reports `AgentRenewalOverlapExpired`, but removes no
pin: without evidence of which key the host uses, removing either could leave
it with no key the Provider accepts. Both pins stay until the host shows the
new key in use, or until the old certificate expires, when the old key can no
longer authenticate the host anyway and is unpinned
(`AgentRenewalCompletedAtExpiry`); the new key then stays pinned. An expired
overlap never leaves a host with zero valid pins.

A re-key or a revocation ends a renewal (a re-key records its new key as a
previous key). `Host.status.agent.renewal` mirrors the phase (`Requested`,
`Issued`, `Overlap`), the new key's pin, the request and the end of the
overlap; `status.agent.keyPins` shows both keys during the overlap. Events
and logs name keys by their pins and certificates by their digests; no
certificate, request or key material is logged, put in an event or in the
Host status. The agent side is described in
[`per-host-agent-development.md`](per-host-agent-development.md#certificate-renewal),
the Provider's relay in
[`clustered-agent-provider-development.md`](clustered-agent-provider-development.md#renewed-certificates).

## Issuance paths

Both paths carry the request as a cert-manager `CertificateRequest` in the
enrolment namespace, so the API server authenticates the requester.

- **cert-manager**: `spec.issuerRef` names the host-agent issuer
  (`--dev-host-enrolment-issuer`). cert-manager signs the approved request and
  sets `Ready=True`.
- **External PKI**: `spec.issuerRef` names the external-PKI reference
  (`--dev-host-enrolment-external-pki-issuer`, `group/kind/name` with a group
  other than `cert-manager.io`, so no cert-manager controller acts on it). After approval, an operator writes the authorised bundle (the CSR
  and the issuance policy: server authentication as the only extended key usage, the digital signature key usage, no certificate or CRL signing, no CA, the exact URI names and the maximum lifetime) with `vrtg host enroll --bundle-out`, has the
  enterprise CA sign it, and returns the certificate with
  `vrtg host enroll --certificate`, which writes it into
  `status.certificate`. The manager then sets `Ready=True` (pinned) or
  `Ready=False`/`Failed` (refused); a `Ready` condition with another reason is
  not the manager's and does not stop it. cert-manager's approval check must be able
  to resolve the external reference's kind and signer name; grant the manager
  `approve` on that signer too.

In both paths the manager pins the key only after the issued certificate
passes these checks against the authorisation: it chains to the configured
subordinate CA bundle (`--dev-host-enrolment-ca-bundle-file`) for server
authentication; its only extended key usage is server authentication (client
authentication is refused); it has the digital signature key usage and no
certificate or CRL signing key usage; it is not a CA; its lifetime is at most 30 days; its
subject is empty or a common name equal to the host name; its subject
alternative names are exactly the authorised URIs; and its key is the
authorised key. A certificate issued after the Host was revoked is refused.

## The pins Secret

Each Host has a manager-written Secret in the **enrolment namespace**
(`--dev-host-enrolment-namespace`), never in the Host's own namespace:
whoever can write Secrets in a tenant namespace must not be able to enrol a
host. Grant nobody but the manager Secret write access in the enrolment
namespace.

Use a cert-manager **ClusterIssuer** for the host-agent subordinate CA
(`--dev-host-enrolment-issuer=ClusterIssuer/<name>`, signer name
`clusterissuers.cert-manager.io/<name>`): its CA key Secret then lives in
cert-manager's cluster resource namespace, not in the enrolment namespace
where the manager can write Secrets. A namespaced `Issuer` keeps its key
Secret in its own namespace, which is the enrolment namespace; that layout
is not supported, because it puts the sub-CA key where the manager writes.

Residual: the manager's existing cluster-wide Secret permission (used for
provider credentials and the host inventories) still reaches every Secret,
including the sub-CA key wherever it lives. Narrowing that grant is separate
work.

The Secret is named `virtrigaud-agent-pins-<hash>` (a hash of the Host's
namespace and name, so the name is bounded), labelled
`app.kubernetes.io/component: host-agent-pins` and
`app.kubernetes.io/managed-by: virtrigaud`, and annotated
`enrolment.virtrigaud.io/host: <namespace>/<name>`. It holds one JSON
document: the Host UID and identity it is bound to, the pending record, the
authorised request, the current enrolment (record, key pins, expiry) and the
retired enrolment ids (up to 256). It holds no private key and no CA bundle.

Owner references cannot cross namespaces, so the manager deletes the Secret
itself: when the Host starts deleting (only a Secret bound to that Host's
UID), when it is gone, and, at start-up, for every pins Secret whose Host no
longer exists. Before deleting, it re-reads the Secret and the Host from the
API server (not its cache) and deletes with UID and resourceVersion
preconditions, so a live Host or a Secret rewritten in between is kept. A state bound to another Host
UID or identity (a replaced host) is discarded, never inherited. A Secret with
that name that is not a VirtRigaud pins Secret for the Host is never read as
pins, overwritten or deleted.

The Secret is authoritative; `Host.status.agent` only mirrors it. A lost or
edited status is rewritten from it, and the inventory render reads it, never
the status.

## CLI

```
vrtg -n <host namespace> host enroll --host <name> --csr <file> \
    --issuer-namespace <enrolment namespace> --issuer Issuer/<issuer name> \
    [--duration 720h] [--manifest-out <file>]
vrtg -n <host namespace> host enroll --host <name> --issuer-namespace <ns> \
    --request <CertificateRequest> --bundle-out <file>
vrtg -n <host namespace> host enroll --host <name> --issuer-namespace <ns> \
    --request <CertificateRequest> --certificate <file>
vrtg -n <host namespace> host enroll --host <name> --issuer-namespace <ns> \
    --request <CertificateRequest> --certificate-out <file>
vrtg -n <host namespace> host rekey --host <name>
vrtg -n <host namespace> host revoke --host <name>
```

`--certificate-out` writes the request's issued certificate only once the
manager has pinned its key for the Host (`status.agent`); every `--request`
mode refuses a request marked `VirtRigaudCertificateRefused`. `host rekey`
opens a re-key window (it needs `patch` on the Host).

`host enroll` checks the CSR file locally (a regular file of at most 64 KiB
holding one PEM CSR whose signature verifies; a FIFO or device is refused
before it is opened). Output files are written with
mode 0600 through a temporary file and a rename, never through a symbolic
link. With `--manifest-out` it writes the `CertificateRequest` to a file
and contacts no cluster, so the whole flow works by copying files: the agent's
CSR from the host to an administrator workstation, the manifest to wherever
`kubectl create -f` runs. Without it, it also checks the CSR carries exactly
the Host's `status.agent.requestURIs` before creating the request. The
requester fields are always left to cert-manager's webhook.

## Manager flags

| Flag | Meaning |
|------|---------|
| `--dev-host-enrolment-namespace` | Namespace of the enrolment requests (the issuer's). Setting it enables enrolment; the manager's CertificateRequest cache is restricted to it. |
| `--dev-host-enrolment-issuer` | cert-manager issuer of the subordinate CA: `kind/name` or `group/kind/name`. |
| `--dev-host-enrolment-external-pki-issuer` | Issuer reference of external-PKI requests: `group/kind/name`. |
| `--dev-host-enrolment-admin-group` | Group whose members may request an enrolment. |
| `--dev-host-enrolment-trust-domain` | Trust domain of host URI identities. |
| `--dev-host-enrolment-ca-bundle-file` | PEM file of the subordinate CA issued certificates must chain to. |

At least one issuer is required, and the development operation signer
(`--dev-operation-signer-key-file`) must be loaded: its install id goes into
every record.

cert-manager must be installed with its admission webhooks. The requester
fields of a `CertificateRequest` can be trusted only because cert-manager's
mutating webhook overwrites them on create and its validating webhook keeps
them immutable; without the webhooks, whoever creates a request chooses them.
At startup the manager therefore refuses to run enrolment unless it finds a
mutating webhook (on create) and a validating webhook (on create and update)
that cover `cert-manager.io/v1` `certificaterequests` in the enrolment
namespace with `failurePolicy: Fail` and no `matchConditions` (which could
skip a request). It repeats the check every three minutes; while it fails, the
manager decides no request (it retries each minute) and logs that approvals
are suspended. This is a registration check: it cannot prove the registered
webhook is cert-manager's. A registered webhook that is down makes creates
fail, which is closed.

## RBAC

The default RBAC grants nothing for enrolment. Apply
`config/rbac/host-enrolment/` (edit its placeholders) or set the Helm values
`experimental.hostEnrolment.enabled`, `experimental.hostEnrolment.developmentBuild`
(an explicit statement that the manager is a development build; rendering
fails without it), `experimental.hostEnrolment.namespace` and
`experimental.hostEnrolment.signerNames`. They grant exactly:

- `approve` on `signers.cert-manager.io` for the named signer names only
  (`resourceNames`, for example `clusterissuers.cert-manager.io/<name>` for
  the recommended ClusterIssuer; wildcards are refused);
- `create` on `subjectaccessreviews` (the per-Host `enrol` and `rekey` checks);
- `get`, `list` on mutating and validating webhook configurations (the
  startup admission check);
- in the enrolment namespace only (a `Role`): `create`, `get`, `list`,
  `watch` on `certificaterequests` (`create` for the manager's own renewal
  requests, see [Certificate renewal](#certificate-renewal)), `update` on
  `certificaterequests/status`, and `create`, `delete`, `get`, `list`,
  `update`, `watch` on Secrets (the pins Secrets).

The manager creates only renewal requests, under its own identity. Its
identity is read at start with a `SelfSubjectReview`, which every
authenticated user may create; no grant is added for it. Administrators who
submit requests need `create` on `certificaterequests` in the enrolment
namespace, the `enrol` verb on the Hosts they enrol and the `rekey` verb on
the Hosts they re-key (`hosts` in
`infra.virtrigaud.io`, in each Host's namespace), and, for the external-PKI
return, `update` on `certificaterequests/status`. Never give them `approve`
on the signer.

Before adding anyone to the enrolment administrator group, audit the roles
that grant wildcard verbs (`*`) on `hosts`, or on every resource: a wildcard
verb satisfies `enrol` and `rekey`, so whoever holds one can enrol and re-key
any Host in its scope. Grant `rekey` separately and sparingly: it replaces the
pinned key of a Host that is in service.

## Not done yet

Revoking at the CA is later work. Certificate renewal (rotation) is
described in [Certificate renewal](#certificate-renewal). A revoked Host's
enrolment is retired and its pins removed, the Host is left out of the
inventory, and a Provider that follows the inventory Secret through the API
(`--hosts-secret`) closes its connection and refuses it within seconds
([details](clustered-agent-provider-development.md#following-the-inventory-secret)).
The agent side (key generation, CSR, certificate installation, deriving the
enrolment record) is described in
[`per-host-agent-development.md`](per-host-agent-development.md#enrolment).
A predecessor's freeze is confirmed and the predecessor removed as described
in [Confirming a predecessor's freeze](#confirming-a-predecessors-freeze).
