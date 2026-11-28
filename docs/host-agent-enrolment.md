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

Both agent-side steps refuse to run where the result would be lost at the next
reboot, checking before they write anything: `vrtg-agent enroll csr` that the
directory of the host key is on persistent storage, and `enroll install` that
the agent's state directory and its `pki/` directory are (not tmpfs, ramfs,
overlay, a loop device or a network file system). The signer refuses to start
(exit 1; systemd retries it within the unit's start limit, then the unit stays
failed) with the key directory on non-persistent storage. On an immutable host
that falls back to an in-memory `/var`, provision a persistent volume for
`/var/lib/virtrigaud-agent` and `/var/lib/virtrigaud-signer` first.

## Host fields

| Field | Meaning |
|-------|---------|
| `spec.identity` | The host's identity anchor (TPM endorsement-key hash, hardware serial, administrator-assigned UUID). Required before the host can enrol. Set once: a CEL rule refuses changing or removing it. A replaced machine is a new identity (a new Host object), and never inherits the old host's pins. |
| `spec.revoked` | When `true`, the manager retires the host's enrolment id, removes its pinned keys and any pending or authorised record, never approves a request for it again, and renders it as unroutable. A CEL rule refuses clearing it: enrol a replacement as a new Host. Revoking the certificate at the CA is not done by this manager yet. |
| `status.agent` | A mirror of the pins Secret (see below): `mode`, `enrolmentID`, `pendingEnrolmentID`, `requestURIs`, `keyPins`, `certificateExpiry`. Public identifiers only. |
| `status.agentLedger` | Development builds with host ledger keys only (see [Host ledger state](#host-ledger-state)); empty until the manager holds a ledger for the host's store (it starts one when it first attests the host's agent). A display mirror of the manager's ledger of the host's agent operation store: `storeIDPrefix` (the first 8 hexadecimal characters of the store id only), `seq`, `checkpointSeq`, `attestationState` (`Pending` or `Attested`), `attestedAt`, `ledgerGeneration` and `updatedAt`. It never carries the full store id, a chain head or a session id, is outside `status.agent` (so writing it does not re-render the Provider's inventory), and nothing reads it back to decide. Refreshed at most every 30 seconds. |
| `status.agentLastStore` | A display mirror of the last store position the manager verified in a signed answer of the host's agent: `storeIDPrefix`, `seq`, `checkpointSeq` and `updatedAt`, refreshed at most every 30 seconds by the heartbeat after a verified answer. The authoritative position stays in the pins Secret (`store.json`); nothing reads this mirror back to decide. A position above the field's 64-bit signed range is not mirrored. |
| condition `AgentEnrolled` | `True`/`Enrolled` once a key is pinned. `False` with `IdentityMissing`, `Revoked`, `CertificateExpired`, `AwaitingCertificateRequest`, `AwaitingCertificate`, `CertificateRequestDenied`, `CertificateRejected` or `EnrolmentRefused` (another live Host claims the same `spec.identity`; the reason says no more: see [One live Host per identity](#one-live-host-per-identity)). |
| condition `AgentIdentityDuplicated` | On an enrolled Host only: `True`/`DuplicateIdentityClaimed` while another live Host claims its identity (that Host is not enrolled), `False`/`Unique` once none does. It names neither the other Host nor the identity. |
| condition `AgentLedgerHeld` | Development builds with host ledger keys only. `True` with the hold's reason while the host's ledger holds the host, `False`/`NotHeld` once the ledger verified with no hold. See [Holds](#holds). |
| condition `AgentStorePressure` | Set by the heartbeat from each verified signed host answer that reports the agent's operation store capacity: `False`/`StoreCapacityOK` below three quarters of the store's size threshold, `True`/`StoreNearlyFull` from there, `True`/`StoreFull` above it (the agent refuses new operations); once `True`, `False` again only below 70% of the threshold (an `Unknown` or absent condition resets this). `Unknown` with `StoreCapacityNotMeasured` (the agent cannot measure its store), `StoreCapacityNotReported` (an older agent stopped reporting) or `StoreCapacityStale` (a heartbeat got no verified answer while it was `False`; a `True` is kept). Absent until an agent reports it. Removed when the Host is no longer polled for, or cannot be checked with, signed answers: its Provider no longer requires operation identity, is gone, is not clustered or does not implement `GetHostInfo`; the manager has no host answer verification configured, or the Provider's client cannot ask for signed answers; the manager holds no trust state for the Host (no Host or no current enrolment; a trust state that cannot be read for a moment counts as a failed heartbeat); the Host's store regressed; or the Host is revoked or its agent certificate expired. See [Store size](per-host-agent-development.md#store-size). |

Libvirt hosts are unchanged: they get no `status.agent`, none of these
conditions and no pins Secret.

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
spiffe://<trust domain>/virtrigaud/record/enrolment/<32 hex>
spiffe://<trust domain>/virtrigaud/record/provider/<Provider UID>
spiffe://<trust domain>/virtrigaud/record/install/<32 hex>
spiffe://<trust domain>/virtrigaud/record/mode/identity
```

Every URI has the manager's trust domain as its host, so the subordinate CA
can carry a URI name constraint for the trust domain (see
[Constrain the sub-CA to the trust domain](#constrain-the-sub-ca-to-the-trust-domain)).
A record URI under another trust domain, or in the former
`urn:virtrigaud:…` form, is refused: a host enrolled by an earlier
development build must enrol again.

The subordinate CA signs these URIs into the agent certificate, so whoever
holds the certificate can derive the enrolment record from it offline, and the
CA authenticates it. An enrolment id is never drawn twice for a host. When an
authorised request ends without a pinned key (its certificate is refused, or
the request is deleted or fails), its record is retired and a new one drawn,
so a CA-valid certificate that was never pinned never shares a record with the
one that is. A new request must then be built from the new
`status.agent.requestURIs`.

### One live Host per identity

`spec.identity` names one machine. Identities are compared without regard to
case, among Hosts in every namespace that are neither revoked nor being
deleted and whose Provider runs per-host agents. The first holder wins: a Host
that already holds a current enrolment is never blocked by a later claimant,
and shows the duplicate in its `AgentIdentityDuplicated` condition. A Host
that holds no enrolment is not enrolled while another live Host claims its
identity: no record is drawn, its `AgentEnrolled` condition is
`False`/`EnrolmentRefused`, an `EnrolmentRefused` Warning event is emitted and
a certificate request for it is denied with the same reason. None of these
names the other Host or echoes the identity, and the claimant's reason does
not say why, so a Host's author does not learn which identities are in use.
Revoke or delete one of the Hosts to resolve it. A record drawn before the
duplicate appeared is not withdrawn, but its request is denied. The decision
to draw a record or approve a request lists every Host live; the cache index
only pre-filters. Admission does not check this yet (a validating webhook is
later work).

Accepted limit: the first holder wins, so a Host created on a per-host agent
Provider can claim an identity before the real machine's Host enrols, and the
real Host is then refused until the claimant is revoked or deleted. Creating
Hosts is a privileged, audited action; the admission check that would refuse
the claimant at creation is a follow-up.
The host ledger relies on this: it follows a machine from one Host object to
its replacement only while no two live Hosts claim the machine.

## Authorisation rules

The manager approves a `CertificateRequest` (cert-manager condition
`Approved`) only if every rule holds; otherwise it sets `Denied` with one of
the reasons below. Nothing else approves these requests: **cert-manager's
default approver must be disabled for the host-agent issuer** (run
cert-manager with `--controllers=*,-certificaterequests-approver` and approve
other issuers with approver-policy, or make sure the default approver has no
`approve` permission on this signer). Otherwise cert-manager approves before
the manager can check anything. The same holds for approver-policy: a
`CertificateRequestPolicy` that selects the host-agent issuer and is bound to
a requester approves that requester's requests itself (see [RBAC](#rbac)).

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
| `spec.duration` is set, at least 15 minutes (`15m`) and at most 30 days (`720h`) | `LifetimeRefused` |

The 15-minute floor keeps a certificate renewable: the agent asks to renew
at two thirds of the lifetime and draws a new key when its request is not
answered within half the lifetime then left (a sixth of the lifetime). The
manager collects the request on its next heartbeat poll, up to a minute
later, and the issued certificate is staged seconds after; at 15 minutes a
sixth is 150 seconds. A shorter certificate could have its request
discarded before it is answered and end pinned to a key the host no longer
holds. `vrtg host enroll --duration` refuses a shorter lifetime locally, and
the approver-policy sample sets the same `minDuration`.

The floor also applies to the certificate the issuer returns, not only to the
lifetime requested: a sub-CA near its own expiry or an external PKI that
shortens lifetimes can issue a certificate valid for less than 15 minutes
(`notAfter` minus `notBefore`; the check is exact, with no tolerance). The
manager refuses to pin the key of such a certificate, at first enrolment and
at renewal: nothing is pinned or published, the request is marked refused,
and an `AgentCertificateLifetimeTooShort` warning event on the Host carries
the reason `IssuedLifetimeTooShort` and the lifetime found. At first
enrolment the `AgentEnrolled` condition says `CertificateRejected` with the
same reason in its message; on a re-key the condition stays `Enrolled`, so
the event is the only report; a renewal restarts under the backoff below. A
renewed certificate of that kind stored by an earlier manager is refused the
same way (no pin, renewal restarted) instead of being pinned. The agent applies
the same floor before it installs an enrolment certificate or stages a renewed
one, so it never runs with a certificate it cannot renew. A certificate that
is already pinned is not judged against the floor again: it stays in use until
it expires.

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
   predecessors it holds as frozen predecessors of that current enrolment,
   ignores everything else, and reports an outcome for each id. A
   confirmation the agent did not apply (refused because 32 discards are
   still pending, or deferred, for example while the agent is not ready)
   is not counted as delivered: the Provider relays it again with a later
   call, after a backoff of 10 seconds doubling up to 10 minutes, until the
   agent reports it applied, already applied or not applicable
   ([details](per-host-agent-development.md#freeze-confirmation),
   [Provider side](clustered-agent-provider-development.md#freeze-confirmations)).
3. **Removal.** The manager's heartbeat asks the host, with a signed request,
   about an operation id it never used under each confirmed predecessor. Only
   a verified signed answer from a ready agent, answering under another
   enrolment, that the predecessor is `UNKNOWN_TO_AGENT` (it is discarded:
   its records are gone, or the agent is still removing them) removes it from the pins Secret (written with its resourceVersion
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
signs it into the record
(`spiffe://<trust domain>/virtrigaud/record/epoch/<n>`, present from epoch 2;
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
   breaks any rule makes the whole answer "no answer", deliberately: the
   renewal request is part of the signed answer, so a broken one is the
   agent's fault, and the host is held (no new grant) until the agent stops
   offering it (it draws a new key after a bound, see below). A verified request is
   recorded in the pins Secret (`renewal`) with an `AgentRenewalRequested`
   event; a key that is, or was, pinned for the Host is refused
   (`AgentRenewalKeyPreviouslyPinned`).
2. **Certificate request.** The manager draws a name for its
   CertificateRequest (`virtrigaud-renewal-` and 12 random bytes) and records
   it in the pins Secret, then creates it in the enrolment namespace under its
   own identity (read at start with a `SelfSubjectReview`, and again every
   minute while it cannot be; until then a waiting renewal is reported once
   with `AgentRenewalNoManagerIdentity`), owned by the Host's pins Secret, with the labels
   `enrolment.virtrigaud.io/agent=true`, `enrolment.virtrigaud.io/renewal=true`
   and `enrolment.virtrigaud.io/host-hash`, the Host annotation, the host's
   request, the usages `digital signature` and `server auth`, and the current
   certificate's lifetime in whole hours, rounded down, at least 1 hour and
   at most 30 days (a 30-minute certificate is renewed for 1 hour, a
   90-minute one for 1 hour). An object of that name that the
   manager did not create (another requester, Host or request) is left alone
   and another name is drawn. The request's name appears in
   `status.agent.renewal` once it exists.
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
     a lifetime of at least 15 minutes and at most 30 days, and its key was
     never pinned.

   A request labelled as a renewal that another principal created (an
   administrator included) is never approved. Approvals are suspended, as for
   enrolment, while the admission check fails. Decisions read the pins Secret
   from the API server.
4. **Issuance.** The issued certificate is validated against the current
   enrolment and the new key (chain, profile, URI names, key) and published in
   the Provider's host inventory (`renewedCertificate`) with an
   `AgentRenewalIssued` event; a certificate that fails is marked
   `VirtRigaudCertificateRefused` on the request (`AgentRenewalRefused`) and
   the renewal restarts under the backoff below. The Provider stages it on
   the host.
5. **New key pinned.** Only after a GetHostInfo the manager asked (its own
   nonce), answered by the current key, returns a verified answer whose
   `staged_certificate_sha256` is the digest of the certificate the manager
   issued, is the new key pinned beside the old one (`AgentRenewalPinned`).
   Both keys are now pinned (the overlap), and the Provider puts the renewed
   certificate in use on the host.
6. **Old key unpinned.** Only after such an answer is signed by the new key
   and names nothing staged is the old key unpinned (`AgentRenewed`): the new
   key is the only pin, the old one is kept as a previous key (a re-key or
   renewal to it is refused; the last 128 are kept), and the certificate
   expiry becomes the renewed one's.

**Activated early.** A Provider that puts the renewed certificate in use
before step 5 would otherwise lock the host out. A host answer of the
manager's own `GetHostInfo` whose signer certificate is exactly, byte for
byte, the certificate the manager issued and validated for this renewal is
verified with that certificate's key pinned as well (every other check is
unchanged) and completes the renewal (`AgentRenewalActivatedEarly`). No other
certificate qualifies: another certificate for the same key, or any other
signer, is no answer.

**Retries.** A renewal restarts when its issued certificate is refused, or
when its request is denied, fails, is deleted or replaced, or loses its
renewal label (`AgentRenewalRestarted`). The same key is recorded again only
after a backoff that starts at an eighth of the current certificate's
remaining lifetime (between 1 and 10 minutes) and doubles up to 6 hours, so
a short-lived certificate is retried well before the agent draws a new key;
after 5 failures the
key is given up (`AgentRenewalGivenUp`, which names the issuer as the likely
fault: its CA must be the configured sub-CA and it must sign the request's
names unchanged). The agent draws a new key when its request has gone
unanswered for half the lifetime its certificate had left when it was made,
and a new key starts afresh. The manager keeps the newest 3 finished renewal
requests of each Host (denied, failed, refused or issued) as cert-manager's
record of the issuance, and deletes older finished ones (recorded under its
identity, labelled as this Host's renewals, at least 5 minutes old, never
the request of the renewal in progress, decided on a read of the pins Secret
from the API server after the reconcile's own save). The pins Secret owns
them, so they also go with the Host. Events and logs keep the key pins and
certificate digests of every renewal.

**Dark window at expiry.** If the Provider put the renewed certificate in use
before step 5 and no answer reached the manager afterwards (the connection
dropped), the Provider still trusts only the old key and cannot reconnect to
the host. When the old certificate expires, the manager pins the issued key
as the only pin (`AgentRenewalPinnedAtExpiry`), after checking on the live
state that the Host is not revoked or deleting, its Provider is the
enrolment's, the pins Secret still holds the same enrolment and the same
unpinned renewal, and the issued certificate still validates. The host is
then reachable again. The window is bounded by the old certificate's expiry
(at most a third of its lifetime after the renewal started). A re-key, a
revocation or a renewal replaced by a new key before then prevents it. The
pinned key may be one the host no longer holds (its agent discarded an
unanswered request and drew a new key that never reached the manager): the
host then stays unreachable, as it would have with only the expired pin, and
re-keying the Host is the recovery.

**Overlap.** The overlap is expected to end within a heartbeat or two. After
24 hours the manager reports `AgentRenewalOverlapExpired`, but removes no
pin: without evidence of which key the host uses, removing either could leave
it with no key the Provider accepts. Both pins stay until the host shows the
new key in use, or until the old certificate expires, when the old key can no
longer authenticate the host anyway and is unpinned
(`AgentRenewalCompletedAtExpiry`); the new key then stays pinned. An expired
overlap never leaves a host with zero valid pins.

**Overdue.** The agent asks to renew at two thirds of its certificate's
lifetime. When the certificate passes five sixths of its lifetime and no
renewal is in progress on the manager's side, nothing from the host has
reached the manager (the agent's signer could not write the pending key, for
example, or the agent is down): `AgentEnrolled` stays `True` with reason
`RenewalOverdue` and a message naming the expiry, and an
`AgentRenewalOverdue` Warning event is emitted when the reason appears. Look
at the agent's and the signer's logs on the host, or re-key the Host before
the certificate expires. A pinned certificate recorded before this check
existed has no recorded start and is not checked until its next renewal or
re-key.

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

### Constrain the sub-CA to the trust domain

Give the host-agent sub-CA certificate a critical URI name constraint whose
only permitted URI domain is the trust domain
(`--dev-host-enrolment-trust-domain`). Every URI in an agent certificate has
the trust domain as its host, so the constraint costs nothing, and it is the
one control that still binds a **stolen** sub-CA key: the admission policy
and approver-policy below act only at the API, while a verifier (the
manager, the Provider and the agent all use Go's `crypto/x509`) refuses any
certificate the sub-CA signs with a URI outside the trust domain. Inside the
trust domain a stolen key can still mint any host's names; only key pinning
stops those from being used.

With cert-manager, set it on the `Certificate` that issues the sub-CA (the
`NameConstraints` feature is beta and enabled by default from cert-manager
v1.17; from v1.14 it is alpha and needs `--feature-gates=NameConstraints=true`
on the controller and the webhook):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: host-agent-ca
  namespace: cert-manager
spec:
  isCA: true
  commonName: host-agent sub-CA
  secretName: host-agent-ca
  privateKey:
    algorithm: ECDSA
    size: 256
  nameConstraints:
    critical: true
    permitted:
      uriDomains:
      - cluster.example   # the trust domain, exactly
  issuerRef:
    kind: ClusterIssuer
    name: your-root-or-intermediate
```

cert-manager's CA issuer does not check the names it signs against the CA's
own constraints, so a mismatched request is still issued and only refused
when verified. The manager does not check that the constraint is present.

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

### State written by a newer manager

A newer manager may store more per-Host state than this one knows about.
That state can be new members in the `enrolment.json` and `store.json`
documents, new data keys in the pins Secret, or Secrets of its own in the
enrolment namespace. If you roll the manager back, the older manager must not
destroy that state. Two rules make sure of this.

**Preservation.** A manager keeps what it does not understand:

- Every JSON member of `enrolment.json` and `store.json` that it does not
  know is written back unchanged, at every nesting level. A member of a
  record stays with that record when records move. For example, an
  earlier store's record keeps its members when it moves from `current` to
  `previous`.
- Every data key of the pins Secret other than `enrolment.json` and
  `store.json` is kept.
- Labels and annotations it does not set are kept. It still sets its own
  `app.kubernetes.io/component`, `app.kubernetes.io/managed-by` and
  `enrolment.virtrigaud.io/host`.

A document with no unknown members is written exactly as before. A document
with unknown members is written with its members in sorted order, so the
output is deterministic. The one exception is a state bound to another Host
UID or identity. It is discarded with everything in it, as described above,
because a replaced host inherits nothing.

**Downgrade floor.** A newer manager can stop older ones from acting on a
Host. To do so, it writes a top-level `minManagerVersion` member, a semantic
version such as `"0.6.0"` (a leading `v` is accepted), in one of these places:

- `enrolment.json`;
- `store.json`;
- any data value that is a JSON object, in a Secret of the enrolment
  namespace labelled `virtrigaud.io/host-ledger` (any value). Such a Secret
  applies to the Host named by its `enrolment.virtrigaud.io/host` annotation.
  Without that annotation, it applies to every Host. No current manager
  creates one.

A manager refuses the Host when any of these is true:

- the floor is above its own version;
- the floor is not a string holding a semantic version;
- its own version is not a semantic version. That is the case for a build
  without release version information, such as `dev` or a bare commit hash.

Versions compare by semantic-version precedence. So a pre-release (`0.6.0-rc.1`) and a
build described from a tag (`v0.6.0-3-gabcdef0`) are both below the release
`0.6.0`, and are refused by a floor of `0.6.0`. An unversioned build is not
affected while no floor is present.

A refused Host's state is left exactly as it is. In particular, the manager:

- does not write or delete the pins Secret. A Host that is deleted or gone
  keeps its pins Secret, and a newer manager removes it;
- approves no certificate request for the Host;
- verifies no signed answer of the Host, so it signs no new operation for it;
- renders no agent credentials for it.

It sets the `AgentEnrolled` condition to `False` with reason `ManagerTooOld`,
emits a `ManagerTooOld` warning event when the condition was not already so,
and logs the reason. The reason names the document and both versions; it
never names key material. Run a manager at least as new as the floor to
resume. The Host is checked again when its pins Secret, a host ledger Secret
naming it, or the Host changes, and when the manager restarts.

### The manager must not read the sub-CA key

Whoever reads the sub-CA key can sign CA-valid host-agent certificates with
no approval at all. Enrolment therefore requires the manager's namespaced
Secret grant ([manager-secret-access.md](manager-secret-access.md#opt-in-namespaced-scope)),
and the chart and the manager check for it.

**What the checks guarantee.** These checks are defence in depth against
misconfigured RBAC. They do not prove that the manager cannot reach the key.
They refuse Secret access and the escalation paths listed below when those are
granted cluster-wide or in the declared key namespace. They do not review any
other namespace: a Role in a third namespace is not seen, for example in the
namespace where the signer controller runs, if that differs from the key
namespace. Keep the sub-CA key in the namespace where cert-manager itself runs.
cert-manager's Helm chart sets its cluster resource namespace to that namespace,
but the controller's own default is `kube-system`, so confirm the value of
`--cluster-resource-namespace`. The key namespace and the controller's namespace
are then the same one, and it is reviewed.

The two checks are:

- **Helm.** With `experimental.hostEnrolment.enabled`, rendering fails:
  - unless `rbac.secrets.scope` is `namespaced`;
  - when an `rbac.additionalRules` entry covers `secrets` (or `*`) in the
    core API group (`""` or `*`);
  - when an `rbac.additionalRules` entry is not a rule, or its `apiGroups` or
    `resources` are not lists of strings, so that it cannot be checked;
  - when a `signerNames` entry does not start with a lowercase
    `<resource>.<group>` without a trailing dot, or names `cert-manager.io`
    with no resource;
  - when `experimental.hostEnrolment.caKeySecret` is empty and
    `signerNames` lists a signer in the `cert-manager.io` group itself, such as
    `issuers.cert-manager.io/...` or `clusterissuers.cert-manager.io/...`. It
    names the sub-CA key Secret as `<namespace>/<name>`. A sub-group, such as
    `awspcaclusterissuers.awspca.cert-manager.io/...`, is an external issuer.
    The chart cannot tell where an external signer keeps its key, so declare
    it there too if that key is in this cluster;
  - when the enrolment namespace is listed in
    `rbac.secrets.providerNamespaces` or `workloadNamespaces`: only the
    manager may write Secrets there, and listing it would declare it a
    namespace for Provider objects or workloads;
  - when the sub-CA key Secret is in the enrolment namespace, or its
    namespace is listed in either list.

  The chart does not start the enrolment manager, so pass the same value to
  it as `--dev-host-enrolment-ca-key-secret`.
- **Manager.** `--dev-host-enrolment-ca-key-secret=<namespace>/<name>` is
  required with `--dev-host-enrolment-issuer`, and refused in the enrolment
  namespace. With an external PKI alone the flag is optional, because the
  manager cannot tell where that signer keeps its key. **If an external signer
  keeps its key in this cluster, declaring the flag is your responsibility:**
  without it, the checks on the key Secret and its namespace do not run.

  Before it starts enrolment, the development manager asks the API server,
  with SelfSubjectAccessReviews, whether it may:
  - `get`, `list`, `watch`, `update`, `patch`, `delete` or
    `deletecollection` Secrets in all namespaces;
  - `get`, `list`, `watch`, `update`, `patch` or `delete` the sub-CA key
    Secret by name (a `resourceNames` rule allows a list or watch with a
    name field selector);
  - `list`, `watch`, `update`, `patch`, `delete` or `deletecollection`
    Secrets in the key Secret's namespace;
  - `get` or `create` `pods/exec` or `pods/attach`, or `update` or `patch`
    `pods/ephemeralcontainers` or `pods`, in the key Secret's namespace (`get`
    authorizes a WebSocket connection on API servers before Kubernetes 1.31);
  - `get` or `create` `nodes/proxy`;
  - `impersonate` users or groups, or ServiceAccounts in all namespaces or in
    the key namespace;
  - `create` ServiceAccount tokens (`serviceaccounts/token`), or `create`,
    `update` or `patch` RoleBindings, in all namespaces or in the key
    namespace; `create`, `update` or `patch` ClusterRoleBindings;
  - `bind` or `escalate` Roles or ClusterRoles in all namespaces or in the
    key namespace.

  All of these verbs except a plain `delete` return Secret data or lead to it.
  A plain `delete` returns no Secret data, but deleting the key is destructive.

  An access review with no object name does not match a rule limited by
  `resourceNames`. The manager therefore also lists its rules with a
  SelfSubjectRulesReview, in the key Secret's namespace (or, with no key
  declared, in the enrolment namespace; cluster-wide rules appear in either).
  It refuses on any rule, whatever its `resourceNames` and including wildcards,
  that grants:
  - `impersonate` on users, groups, ServiceAccounts, uids or user extras;
  - `create` on `serviceaccounts/token`;
  - `create`, `update` or `patch` on RoleBindings or ClusterRoleBindings;
  - `bind` or `escalate` on Roles or ClusterRoles;
  - `get` or `create` on `nodes/proxy`;
  - with a declared key: any read or write verb on Secrets, `get` or `create`
    on `pods/exec` or `pods/attach`, or `update` or `patch` on `pods` or
    `pods/ephemeralcontainers`.

  If any access is allowed, the manager logs the allowed accesses and exits.
  It also exits if a review fails, if the authorizer reports an evaluation
  error, or if the rules cannot be listed completely. **A cluster with a
  webhook authorizer in its authorization chain always reports incomplete
  rules, so enrolment refuses to run there. This commonly includes managed
  Kubernetes services.** There is no override. A dangling role binding also
  refuses: one that refers to a missing role, is cluster-wide or in the reviewed
  namespace, and applies to the manager directly or through any group it belongs
  to, such as `system:authenticated`.
  The reviews ask the API server's authorizers, so
  they cover the manager's RBAC whatever renders it (the chart, Kustomize or
  hand-written roles). They do not see admission policies. They need `create`
  on `selfsubjectaccessreviews` and `selfsubjectrulesreviews`, which Kubernetes
  grants every authenticated user through the default `system:basic-user`
  ClusterRole, so no grant is added for them. The chart's manager and enrolment
  RBAC grant none of the reviewed accesses, with `rbac.scope: cluster` and with
  `rbac.scope: namespace`.

The manager repeats the reviews with the cert-manager admission check: once
when its controllers start, and then every three minutes. While either fails, it decides
no enrolment or renewal request and logs that approvals are suspended, as
described in [Manager flags](#manager-flags). A Secret grant added after
startup therefore suspends approvals within the interval plus the time one
check takes.

At startup the manager also asks whether it may `create`, `update` or `patch`
Deployments, DaemonSets, StatefulSets, ReplicaSets or CronJobs, or `create`
Jobs or Pods, in the key Secret's namespace, and logs a `WARNING` naming them
if it may. It does not refuse, because an admission
policy, which the reviews cannot see, may forbid them. With the chart's default
`rbac.scope: cluster`, the manager's ClusterRole may create, update and patch
Deployments in every namespace, so the warning fires.

The checks do not cover these paths:

- **Other namespaces.** Grants in a namespace other than the key namespace are
  not reviewed (see [What the checks guarantee](#the-manager-must-not-read-the-sub-ca-key)).
- **Workloads.** A compromised manager with a cluster-wide Deployment grant
  can create a Deployment in the key Secret's namespace that mounts the key
  Secret, or that runs under an existing ServiceAccount there, such as
  cert-manager's own. It can also patch a Deployment that already runs there:
  changing cert-manager's own Deployment is a path to the key. The kubelet
  mounts the Secret, and the pod gets that ServiceAccount's token, with no
  Secret RBAC of the manager involved (see also
  [What remains](manager-secret-access.md#what-remains)). To close this path,
  use an admission policy (a ValidatingAdmissionPolicy, Kyverno or Gatekeeper)
  on the manager's ServiceAccount that covers both `CREATE` and `UPDATE`, for
  every workload kind the manager may write. It
  must let the manager write workloads only in the Provider namespaces, allow
  their pods only the ServiceAccounts the manager creates for providers, and
  so block any change to existing workloads in the key Secret's namespace.
- **Between re-checks.** A grant added after the last review works until the
  next one finishes: at most the interval plus the time one check takes. Whoever
  can create Roles and RoleBindings can grant the manager Secret access at any
  time, within what Kubernetes RBAC lets them grant.
- **An undeclared key.** With an external PKI alone and no declared key
  Secret, nothing checks the namespace where that signer keeps its key.
- **Paths RBAC review cannot list.** The reviews cover the listed accesses
  only. Other routes to the key, such as access to a node, to etcd or to a
  backup, are outside them.
- **The API approval path.** Whoever holds the manager's ServiceAccount, or
  can create Roles that grant it more, can still use the approval path
  described in [RBAC](#rbac): `create` on `certificaterequests`, `update` on
  their status and `approve` on the signer. A certificate obtained that way
  is pinned for no Host, so the Provider and the manager do not accept it.
  The deny-only admission policy and approver-policy narrow what such a
  holder can request.

## Host ledger state

> Development builds only, and only with host ledger keys
> (`--dev-host-ledger-key-dir`). The manager attests the host's agent, verifies
> operation receipts, sends ledger marks and signs grant version 2 from this
> state (see
> [Manager driver](operation-grants-wire-shape.md#manager-driver-development-builds)).
> This section describes the durable state, its checks, and the status mirror.

The host ledger is what the manager knows durably about one host agent
operation store: the highest position of the store's chain the manager has
acknowledged (with the checkpoint used at attestation), the agent's
attestation sessions, and the store's generation. A development-build agent
runs an accepted operation only once a ledger mark covers it (see
[Agent attestation](operation-grants-wire-shape.md#agent-attestation-development-builds));
the ledger is where the manager will keep the marks it has given.

### Ledger objects

One Secret per store in the enrolment namespace, named
`virtrigaud-ledger-<store id, 32 hexadecimal characters>`, with data key
`ledger.json`. It is keyed by the store, not by the Host: it has no owner
reference, deleting or replacing a Host or resetting its pins Secret does not
touch it, no controller ever deletes it, and no older manager reads or writes
it. Labels (lookup hints only; every decision reads the tagged body):
`app.kubernetes.io/component: host-ledger`,
`app.kubernetes.io/managed-by: virtrigaud`, `virtrigaud.io/host-ledger: v1`,
`virtrigaud.io/enrolment: <current enrolment id>` and
`virtrigaud.io/host-identity-mac: <first 32 hexadecimal characters of the
identity MAC>`; the annotation `enrolment.virtrigaud.io/host` names the Host
it serves.

`ledger.json` holds: the store id; the host identity MAC (an HMAC of
`spec.identity` under the identity key, so a low-entropy serial number cannot
be recovered from a Secret listing; the label carries a prefix of it, which
lets whoever can list Secrets in the enrolment namespace link the ledgers of
one machine across Host objects and stores, without learning the identity); the Host UID, `namespace/name` and
Provider UID it serves; the ledger position (`seq`, `head`) and checkpoint;
the current attestation session and its state (`Pending` or `Attested`); up
to four pending sessions with when each was begun; up to eight earlier
sessions with the ledger position at which each was replaced; the generation,
when it started, and up to sixteen enrolments under which it was continuous;
a write counter (+1 per write); `minManagerVersion` (the downgrade floor, see
[State written by a newer manager](#state-written-by-a-newer-manager); it is
`0.4.2-0` while the format is in development builds only, and is raised to
the first release that ships the format);
`updatedAt`; for an archived ledger, the successor store and when; a hold of
the Host it serves with the enrolment it was decided under (see
[Holds](#holds)); the tag key id; and the tag: an HMAC-SHA256 under the ledger tag key over a
length-prefixed encoding of every member except `updatedAt` and the tag key
id, including members a newer manager added (a rewrite keeps them).

The pins Secret's `store.json` gains a member `ledgerIndex`: the stores that
have (or are getting) a ledger object, up to eight, each with the highest
write counter of its ledger seen; the highest generation any of the Host's
ledgers had; a hold of the Host; and a tag under the same key bound to the
Host UID (an index copied from another Host's pins Secret does not verify).
None of it is ever lowered. Writing the index also sets the format's
downgrade floor as `store.json`'s `minManagerVersion`. Every other write of
`store.json` keeps both as they are. A store is reserved in the index before
its ledger object is created and its counter recorded after; every
session-changing write and every raise from receipts records its counter at
once, a raise from heartbeats at most every 30 seconds.

The manager refuses a ledger that serves a Host whose index does not name it,
an index that disappeared after the manager saw it, and a store the index
names whose object is gone or no longer carries the Host's labels.

### Writes

Every read is uncached, and every change is one update that carries the
`resourceVersion` it read: a concurrent writer makes it fail, and it is
retried from a fresh read, never overwritten. The manager uses a ledger only
after its tag verified, or as the object the API server returned for its own
write.

| Write | When | Change |
|---|---|---|
| Initialise | The store has no ledger object and its agent signs, in the ledger state of a verified answer (a challenge answer, or a host answer to a request with ledger version 2), that the store was never attested. From the host's last verified position in `store.json`, only once the host has confirmed it: a verified answer to a request that named that position carrying the same head. Otherwise only on first contact: the agent shows, signed, that the store was created under the current enrolment and held no other enrolment, and no other store has a ledger under that enrolment. | A new object; the store is added to the index. |
| Archive | First contact with a new store under a new enrolment while the machine's previous store has a ledger. | The old ledger is marked archived with its successor and never written again; the new ledger's generation is above it. |
| Rebind | A new Host object for the same machine (same identity MAC), once the Host the ledger served is deleted or revoked, has had no verified answer for twice the heartbeat interval (answers are recorded per Host object, so a replacement of the same name does not count), has been seen released by this manager for that long (so a restart waits again), and no other Host that is not revoked carries the identity. | The ledger serves the new Host; position, generation and sessions are kept (never a fresh start), and it becomes `Pending`: the new Host's agent is attested again. |
| Begin session | An attestation challenge for the agent. | The session is added to the pending sessions (oldest of more than four evicted). For an agent awaiting attestation the ledger becomes `Pending`; a re-attestation of an attested agent keeps the working session current until the new one is adopted. |
| Raise | Verified positions of the current session. | The position and checkpoint rise to the highest; positions of any other session are dropped; positions from heartbeats raise the ledger at most once per 30 seconds per host. |
| Adopt | A verified nonce-bound answer (never an operation receipt) names a pending session. | That session becomes current and `Attested`; the previous one moves to the history with the position at which it was replaced. |

Every start of a ledger needs the agent's signed record of whether its store
was ever attested against a host ledger (the agent keeps it in the store,
from its first attestation on). A store that was attested ran, or could have
run, operations under a ledger the manager no longer holds, so a fresh ledger
for it could hide them: whatever the Secrets in the enrolment namespace say
(a deleted ledger object, a removed or replayed index), the manager holds the
host (`ledger_missing`, persistent) and never starts one; the exit is the
fresh-store procedure. An agent that does not report the record (an older
agent) holds the host too, re-evaluated on every attempt, until it is
upgraded. A store that was never attested starts its ledger however long its
chain grew before (clock-floor commits of restarts, say).

For an existing ledger, a store whose signed first attestation position is
above the ledger's position shows that the ledger is behind the store's own
history (an older ledger restored, or another put in its place): the host is
held (`LedgerRegressed`, persistent). The record's own sequence number is not
compared with the ledger: the ledger legitimately stays below it until it is
raised.

A generation is a number never handed out twice for a store: a fresh ledger
starts at the largest of one above its predecessor's, one above the highest
generation the Host's index recorded, and the current Unix time in seconds,
so a ledger restarted after its object was lost never matches an earlier
generation.

### Holds

A check that fails holds the host: an `AgentLedgerHeld` Warning event names
the reason and the first 8 hexadecimal characters of the store id (no key,
store id, chain head or session id), and the `AgentLedgerHeld` condition shows
it. The reasons:

| Reason | Meaning | Ends |
|---|---|---|
| `LedgerTampered` | A ledger object or the index fails its tag or form or names an unknown tag key; the index is missing or does not name a ledger that serves the Host; an indexed store lost its object or its labels; a ledger rewritten under the same write counter; or a ledger's sessions changed in a later write this manager did not make while it ran. | The object is fixed. A rewrite under the same counter, and an index that disappeared after it was seen, are persistent (below). A session change in a later write holds the host for that read only: another manager makes exactly such a write during a leader change. The manager goes on from the new ledger and records the write counter it observed in the index at once, so an older copy replayed later is refused even after a restart. Its Warning event is emitted at most once per host every five minutes, and `virtrigaud_manager_host_ledger_foreign_session_changes_total` counts these changes. |
| `LedgerRegressed` | A ledger went back: a lower position or write counter than this manager wrote, a write counter below the index's (an older copy restored), or the ledger object this manager held is gone. | Persistent. |
| `ledger_missing` | The store has no ledger object and may not get one: the index names it (its object was lost); the agent signs that the store was attested (persistent); the agent does not report whether it was (re-evaluated on every attempt); or no evidence allows first contact. | The fresh-store procedure: take the VMs off the host, reinstall the agent's state and re-key the Host, which gives a first contact. A lost ledger is never restarted from an operator's word. |
| `ledger_unconfirmed` | The host has not confirmed the last position the ledger would start from. | A confirming answer. |
| `ledger_ahead` | The ledger is above the host's chain tip. | Investigation; nothing is signed. |
| `ledger_fork` | Two verified positions of one sequence number carry different heads. | Persistent. |
| `ledger_store_mismatch` | The host shows another store under an enrolment that already has a ledger. | Persistent. |
| `ledger_store_foreign` | The ledger is bound to another machine. | None automatic. |
| `ledger_store_bound` | The ledger serves another Host that is live, not revoked, answering, recently released, or whose identity another live Host carries. | When the rebind conditions hold. |

A persistent hold ends only with a new enrolment of the host. It is written
into the tagged ledger index and as the store-regression hold of the pins
Secret (condition `AgentStoreRegressed`), and, for a fork or a store mismatch,
into the tagged ledger object too, so clearing the pins Secret's hold does not
lift it. A ledger object found regressed or changed behind the manager's back
is left untouched, as evidence.
A persistent hold that cannot be written is returned as an error with the
hold, and the manager keeps holding the host in memory; it is never lifted by
a later call. The `AgentLedgerHeld` condition returns to `False`/`NotHeld`
when a transient hold clears.

### Host ledger keys

The tag and identity keys are random keys of their own, not derived from the
operation signer or the intent key. Create a Secret in the manager's
namespace with three data keys, each 64 lowercase hexadecimal characters of
random bytes:

```bash
kubectl -n virtrigaud-system create secret generic host-ledger-key \
  --from-literal=current="$(openssl rand -hex 32)" \
  --from-literal=identity="$(openssl rand -hex 32)"
```

Mount it into the manager pod alone (the chart does this with
`experimental.hostEnrolment.ledgerKeySecret`, at
`/etc/virtrigaud/host-ledger-key` with mode 0400; the manager, which does not
run as root, reads it through the pod's `fsGroup`, which the kubelet applies
to Secret volumes) and pass `--dev-host-ledger-key-dir=<mount path>` and
`--dev-host-ledger-key-secret=<namespace>/<name>`. The manager logs only the
key ids (the first 8 bytes of a SHA-256 of each key, as 16 hexadecimal
characters) and refuses to start with a malformed, all-zero or repeated key,
or a key Secret in the enrolment namespace. A release manager refuses both
flags; they need `--dev-host-enrolment-namespace`. The manager never reads the
key Secret through the API: per-host agent enrolment refuses to run (and
suspends approvals on a later re-check) while the manager may get, list or
watch it or any Secret of its namespace, through a review or a rule limited to
named objects, as for the sub-CA key. The chart refuses to render the key
Secret when the enrolment namespace is the release namespace.

To rotate the tag key, move `current` to `previous`, put a new key in
`current`, and restart the manager. Tags made with the previous key still
verify. With a previous key configured, the leader runs a re-tag sweep at
start: every ledger object (archived ones included) and every ledger index in
the enrolment namespace tagged under the previous key is verified under it and
written again under the current key (with its `resourceVersion`, a few writes
per second). A ledger object is first checked as a read of it would be: when
the Host it serves resolves, against that Host's index, write counter, stored
hold and the manager's memory, holding the Host on a regression, and its new
counter is recorded in the index; otherwise against the index left in that
Host's pins Secret. A restored older copy is never re-tagged and counts as
remaining. It logs the outcome and publishes
`virtrigaud_manager_host_ledger_retag_remaining`: once that is 0 (the log says
the previous key may be removed), drop `previous` and restart. A tag under a
key the manager no longer holds is `LedgerTampered`. Never change `identity`:
the identity MACs stored in the ledgers are computed with it.

The tag defends against a principal that can write Secrets in the enrolment
namespace but cannot read the key Secret, and nothing more. The chart grants
no principal Secret access in the manager's namespace for it, and refuses
`ledgerKeySecret` when that namespace is listed in
`rbac.secrets.providerNamespaces` or `workloadNamespaces`. A test enumerates
every role and binding the chart renders and `config/rbac` ships and proves
that only the manager's ServiceAccount may write Secrets in the enrolment
namespace; that no principal, the manager included, may read the key Secret
through the API; and that no principal may reach it through a workload in its
namespace (creating or changing pods, deployments, statefulsets, daemonsets,
replicasets, jobs or cronjobs, `pods/exec`, `pods/attach`,
`serviceaccounts/token`), impersonation, or `bind` or `escalate` on roles,
except one named exception: the manager's own pre-existing right to create
and change Deployments in every namespace (the provider controller needs it).
The paths it covers also include replicationcontrollers, ephemeral containers
and `nodes/proxy` (the kubelet API).

Custody limit, stated plainly: a stolen manager ServiceAccount can create a
Deployment in the manager's namespace that mounts the key Secret, and so read
the key, exactly as it could mount the sub-CA key Secret. This is accepted and
is not checked at runtime (the manager's startup check covers API reads of the
key only); the RBAC proof accepts this one right by name. Until the keys are
held by a custody service, the tag protects the ledger against principals
other than the manager, not against the manager's own identity. Keep every
role you add within the proof.

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
| `--dev-host-enrolment-ca-key-secret` | `<namespace>/<name>` of the Secret that holds the subordinate CA's key. Required with `--dev-host-enrolment-issuer`. With an external PKI alone it is optional, and you must set it whenever that signer keeps its key in this cluster. Enrolment refuses to run while the manager may read that Secret ([details](#the-manager-must-not-read-the-sub-ca-key)). It must not be in the enrolment namespace. Only a development build defines this flag. A release build stops at flag parsing when it is set. |
| `--dev-host-ledger-key-dir` | Directory of the host ledger keys (files `current`, optional `previous`, `identity`), normally the key Secret mounted into the manager pod alone ([details](#host-ledger-keys)). Optional; needs `--dev-host-enrolment-namespace` and `--dev-host-ledger-key-secret`. A release build refuses to start when it is set. |
| `--dev-host-ledger-key-secret` | `<namespace>/<name>` of the Secret mounted at `--dev-host-ledger-key-dir`, outside the enrolment namespace. Enrolment refuses to run while the manager may get, list or watch Secrets in its namespace through the API. A release build refuses to start when it is set. |

At least one issuer is required, and the development operation signer
(`--dev-operation-signer-key-file`) must be loaded: its install id goes into
every record. The manager must not be able to read Secrets in all namespaces
or the sub-CA key Secret, or grant itself that access; it refuses to run
enrolment if it can, and suspends approvals if that changes later
([details](#the-manager-must-not-read-the-sub-ca-key)).

cert-manager must be installed with its admission webhooks. The requester
fields of a `CertificateRequest` can be trusted only because cert-manager's
mutating webhook overwrites them on create and its validating webhook keeps
them immutable; without the webhooks, whoever creates a request chooses them.
At startup the manager therefore refuses to run enrolment unless it finds a
mutating webhook (on create) and a validating webhook (on create and update)
that cover `cert-manager.io/v1` `certificaterequests` in the enrolment
namespace with `failurePolicy: Fail` and no `matchConditions` (which could
skip a request). It repeats the check once when its controllers start and then every three minutes, together with the
Secret access reviews ([details](#the-manager-must-not-read-the-sub-ca-key));
while either fails, the manager decides no request (it retries each minute)
and logs that approvals are suspended. This is a registration check: it cannot prove the registered
webhook is cert-manager's. A registered webhook that is down makes creates
fail, which is closed.

## RBAC

The default RBAC grants nothing for enrolment. Apply
`config/rbac/host-enrolment/` (edit its placeholders) or set the Helm values
`experimental.hostEnrolment.enabled`, `experimental.hostEnrolment.developmentBuild`
(an explicit statement that the manager is a development build; rendering
fails without it), `experimental.hostEnrolment.namespace` and
`experimental.hostEnrolment.signerNames`, with `rbac.secrets.scope: namespaced`
(rendering fails without it, see
[The manager must not read the sub-CA key](#the-manager-must-not-read-the-sub-ca-key)).
`experimental.hostEnrolment.caKeySecret` is required with a cert-manager
signer and is only used for render checks.
They grant exactly:

- `approve` on `signers.cert-manager.io` for the named signer names only
  (`resourceNames`, for example `clusterissuers.cert-manager.io/<name>` for
  the recommended ClusterIssuer; wildcards are refused);
- `create` on `subjectaccessreviews` (the per-Host `enrol` and `rekey` checks);
- `get`, `list` on mutating and validating webhook configurations (the
  startup admission check);
- in the enrolment namespace only (a `Role`): `create`, `delete`, `get`,
  `list`, `watch` on `certificaterequests` (`create` and `delete` for the
  manager's own renewal requests, see [Certificate renewal](#certificate-renewal)), `update` on
  `certificaterequests/status`, and `create`, `delete`, `get`, `list`,
  `update`, `watch` on Secrets (the pins Secrets and the host ledger Secrets, see [Host ledger state](#host-ledger-state)).

`experimental.hostEnrolment.ledgerKeySecret` grants nothing: it mounts the
host ledger key Secret into the manager pod alone (see
[Host ledger keys](#host-ledger-keys)). The tag on the ledger objects is only
as strong as two properties, which a test proves over every role and binding
the chart renders and `config/rbac` ships: only the manager's ServiceAccount
may create, update, patch or delete Secrets in the enrolment namespace, and no
other principal may read the key Secret. Roles you add must keep both.

The manager creates (and deletes) only renewal requests, under its own
identity. Its identity is read with a `SelfSubjectReview`, which every
authenticated user may create; no grant is added for it.

With `create` on `certificaterequests`, `update` on their status and
`approve` on the signer, the manager can obtain a host-agent certificate by
itself: its approval rules (a renewal co-signed by the currently pinned key,
unchanged identities) are enforced by its own code, not by the API server.
Whoever controls the manager's ServiceAccount can therefore have the issuer
sign a certificate for any Host's URI names. Such a certificate is pinned for
no Host (the Provider and the manager accept only pinned keys), but it is
CA-valid. Two optional controls narrow what that ServiceAccount can request;
they do different things:

- **Deny-only admission (recommended).**
  `config/rbac/host-enrolment/admission/manager-certificaterequests.yaml` is
  a `ValidatingAdmissionPolicy` and its binding (Kubernetes 1.30 or later)
  that refuses, at admission, every `CertificateRequest` created by the
  manager's ServiceAccount unless it is shaped like the manager's renewal
  requests: in the enrolment namespace, labelled as an agent renewal and
  naming a Host, for the host-agent ClusterIssuer, not a CA, with exactly the
  usages `digital signature` and `server auth`, and a lifetime of at most 30
  days. It approves nothing, so the manager's approval rules stay the only
  approval decision. It cannot check the names inside the PKCS #10 request
  (`spec.request` is DER, which admission expressions cannot parse); the
  manager checks those itself, and the sub-CA's
  [URI name constraint](#constrain-the-sub-ca-to-the-trust-domain) bounds
  them to the trust domain. An envtest spec applies the sample to a real API
  server.
- **approver-policy (only if you accept what it does).**
  `config/rbac/host-enrolment/admission/approver-policy.yaml` is a
  cert-manager approver-policy `CertificateRequestPolicy` bound (`use`) to the
  manager's ServiceAccount. approver-policy is an approver, not a filter: for
  requests from a requester bound to a policy, approver-policy itself
  approves those the policy allows and denies the others, in practice before
  the manager evaluates them. Installed this way, it replaces the manager's
  approval decision for the manager's own renewal requests (the manager still
  validates the issued certificate before it publishes or pins anything), and
  it approves requests of the allowed shape that the manager would never
  approve, including a request for a Host that does not exist. What it adds
  is a check of the names inside the request: only
  `spiffe://<trust domain>/virtrigaud/host/...` and
  `spiffe://<trust domain>/virtrigaud/record/...` URIs, no DNS, IP or e-mail
  names; it cannot require exactly one host URI. The policy
  must allow a subject common name: every agent request, enrolment and
  renewal, carries the host's name as its common name, and a policy without
  `allowed.commonName` denies every renewal (the manager gives a key up after
  5 denials). A test checks the sample against a request built by the
  agent's own code. Do not bind approver-policy to administrators'
  requests: the manager decides those.

Neither control changes the residual for a holder of the ServiceAccount,
which also holds `approve` on the signer; the admission policy narrows the
shape of what it can create, approver-policy the names.

A principal with `update` or `delete` on `certificaterequests` in the
enrolment namespace can delete or relabel each new renewal request; each
counts as a failure, so it can make the manager give a key up after 5 and
delay renewal until the agent draws a new key (and, repeated, until the
certificate expires). Grant those verbs there to no one but cert-manager
and the manager. Administrators who
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

## Deployment

The Helm chart cannot run enrolment yet. It renders the enrolment RBAC
(`experimental.hostEnrolment`), but it cannot pass the manager's
`--dev-host-enrolment-*` and `--dev-operation-signer-key-file` flags or mount
the sub-CA bundle and the operation signer key, and it ships no development
build of the manager. Run a development build of the manager with those
flags and files by other means (with its own ServiceAccount, so that the API
server authorises it by `config/rbac/` and `config/rbac/host-enrolment/`).
`config/rbac/role.yaml` grants Secret access in all namespaces, so the
manager refuses to start enrolment with it as generated. Remove that grant
and grant Secrets per namespace instead, as described in
[manager-secret-access.md](manager-secret-access.md#kustomize). Pass
`--dev-host-enrolment-ca-key-secret` too; it is required with a cert-manager
issuer.

The manager's connection to a clustered Provider for per-host agents must be
TLS: the manager sends that Provider's mutating calls as signed operations,
and refuses plaintext or unverified TLS to it. Set
`spec.runtime.service.tls.enabled: true` with a `secretRef` on the Provider
([details](clustered-agent-provider-development.md#flags)). The Provider's own
identity certificate, which it presents to the agents, comes from its
credentials Secret
([details](clustered-agent-provider-development.md#provider-identity-certificate)).

A revoked Host (`spec.revoked`) is no longer polled by the heartbeat: its
`Ready` condition is `False` with reason `Revoked`. Nor is a Host whose
pinned agent certificate has expired (read from its pins Secret, which
`status.agent.certificateExpiry` mirrors): `Ready=False` with reason `CertificateExpired`, rechecked every
minute without a call, and polled again once a re-key pins a new
certificate. `Ready` turns `CertificateExpired` only once the expiry has
passed, and no later than a second after it: while the certificate is
valid, the next check is brought forward to its expiry when that comes
before the next heartbeat. A change of the Host's pins Secret polls the
Host at once when the poll must follow it: a re-key that pins a valid
certificate in place of an expired one, the first certificate pinned, and
every step of a certificate renewal (recorded, request created, approved,
issued, new key pinned, renewal ended, key pins changed while one is in
progress). While a renewed certificate is issued and not yet pinned, the
Host is polled every 5 seconds: the Provider stages the certificate on its
next call to the host, and the manager pins the new key on the host's next
answer. No other change of the pins Secret polls the Host: the writes of an
enrolment before its pin or of a revocation would only poll a host its
Provider does not front, and the heartbeat's own writes of the verified
store position would add a poll per heartbeat. A Host its Provider does not
front (an agent Host waiting for its first certificate) is polled with a
backoff from 15 seconds, doubled per poll up to the heartbeat's minute, and
its `GetHostInfo failed` error is logged on the first poll and then at most
once a minute (the other polls log at verbosity 1); once it has a pinned
certificate the Provider has not applied yet, it is retried every 15
seconds. A
condition's `lastTransitionTime` moves only when its status changes, not
when only its reason or message does. While a revoked or expired Host is not polled,
`AgentInventoryVerified` is `False` with reason `NoSignedAnswer` and
`AgentAnswerVerified` (when the Host has it) is `False` with reason
`NoVerifiedAnswer`; their messages say that the host is not polled and why
(`Revoked` or `CertificateExpired`), rather than keeping a verdict from
before. A status update that loses an optimistic-lock race with another
writer is retried at once without an error log (a verbosity 1 log line
records it).

With approver-policy bound to the manager, a renewal request is often
approved by approver-policy before the manager decides it. The manager then
applies its own rules to the approved request (it writes no condition): if
they hold, it records its approval and publishes the issued certificate (an
`AgentRenewalApproved` event and a log line report the approval once, when
it is recorded, also when another approver's approval lands while the
manager is writing its own); if
they do not, it marks the request refused at once (`VirtRigaudCertificateRefused`;
an approved request cannot also be denied) and an `AgentRenewalDenied`
Warning says so. The refusal is final: the request is not judged again even
if the rule that failed passes later, its certificate is never published,
and the renewal starts again with a new request. A request already issued
before the manager judged it is refused the same way. A renewal-shaped
request that names an existing Host but never was its renewal (not one the
manager created for it) is refused once it is issued, and an
`AgentRenewalDenied` Warning on that Host says that it was not the Host's
renewal (it names the request, never its contents). While approvals are
suspended (the admission check fails), such requests are not judged. An
optimistic-lock conflict (the request or the pins Secret written by someone
else since it was read) is retried at once without an error log; every
write carries the version it read, and the retry decides again from a
fresh read.

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

A manager with host ledger keys attests a development-build agent and signs
its operations at grant version 2 (see
[Host ledger state](#host-ledger-state)); any other manager signs nothing for
it, so the agent runs no operation. Removing a ledger
object of a store that no longer exists needs an operator tool that is not
written yet; until then ledger Secrets accumulate (about 1.5 KB per store
ever seen).
