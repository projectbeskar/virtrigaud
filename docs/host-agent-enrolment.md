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
| The requester may use the `enrol` verb on that Host (`hosts` in `infra.virtrigaud.io`, the Host's namespace and name), checked with a SubjectAccessReview built from the request's `spec.username`, `spec.uid`, `spec.groups` and `spec.extra` | `RequesterNotAuthorisedForHost` |
| The Host exists and is not being deleted | `HostNotFound` |
| The Host's Provider runs a per-host agent | `HostNotAgentManaged` |
| The Host has `spec.identity` | `HostIdentityMissing` |
| The Host is not revoked | `HostRevoked` |
| A record is pending, the Host is not already enrolled, no other request is in flight, and the request's record was not retired | `NoPendingEnrolment`, `AlreadyEnrolled`, `EnrolmentInFlight`, `EnrolmentRecordRetired` |
| `spec.request` is one PEM CSR | `RequestInvalid` |
| The CSR's signature verifies with its own key (proof of possession) | `ProofOfPossessionFailed` |
| The key is ECDSA P-256/P-384, Ed25519, or RSA of 2048 to 8192 bits | `KeyUnsupported` |
| The subject alternative names are exactly the host URI and the pending record (no DNS, IP or e-mail names) | `SubjectAltNamesRefused` |
| The subject is empty or a common name equal to the host name | `SubjectRefused` |
| The CSR asks for no extension other than the subject alternative names (and basic constraints with the CA bit clear) | `ExtensionRefused` |
| No CA certificate (`spec.isCA` false, no CA bit in the CSR) | `CARequested` |
| `spec.usages` include `server auth` and not `client auth`: the per-host agent is only ever a TLS server (`digital signature` and `key encipherment` are tolerated, nothing else) | `UsagesRefused` |
| `spec.duration` is set and at most 30 days (`720h`) | `LifetimeRefused` |

Before approving, the manager records the authorisation (request UID, record,
key pin) in the pins Secret. A denial never changes the pins.

## Issuance paths

Both paths carry the request as a cert-manager `CertificateRequest` in the
enrolment namespace, so the API server authenticates the requester.

- **cert-manager**: `spec.issuerRef` names the host-agent issuer
  (`--dev-host-enrolment-issuer`). cert-manager signs the approved request and
  sets `Ready=True`.
- **External PKI**: `spec.issuerRef` names the external-PKI reference
  (`--dev-host-enrolment-external-pki-issuer`, `group/kind/name` with a group
  other than `cert-manager.io`, so no cert-manager controller acts on it). After approval, an operator writes the authorised bundle (the CSR
  and the issuance policy) with `vrtg host enroll --bundle-out`, has the
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
authentication is refused); it is not a CA; its lifetime is at most 30 days; its
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
vrtg -n <host namespace> host revoke --host <name>
```

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
- `create` on `subjectaccessreviews` (the per-Host `enrol` check);
- `get`, `list` on mutating and validating webhook configurations (the
  startup admission check);
- in the enrolment namespace only (a `Role`): `get`, `list`, `watch` on
  `certificaterequests`, `update` on `certificaterequests/status`, and
  `create`, `delete`, `get`, `list`, `update`, `watch` on Secrets (the pins
  Secrets).

The manager gets no `create` on `certificaterequests`. Administrators who
submit requests need `create` on `certificaterequests` in the enrolment
namespace, the `enrol` verb on the Hosts they enrol (`hosts` in
`infra.virtrigaud.io`, in each Host's namespace), and, for the external-PKI
return, `update` on `certificaterequests/status`. Never give them `approve`
on the signer.

Before adding anyone to the enrolment administrator group, audit the roles
that grant wildcard verbs (`*`) on `hosts`, or on every resource: a wildcard
verb satisfies `enrol`, so whoever holds one can enrol any Host in its scope.

## Not done yet

Rotation, removing pins on revocation, and revoking at the CA are later work.
The agent side (key generation, CSR, certificate installation, deriving the
enrolment record) is described in
[`per-host-agent-development.md`](per-host-agent-development.md#enrolment); the
manager does not yet support enrolling a Host that is already enrolled (a
re-enrolment is refused with `AlreadyEnrolled`).
