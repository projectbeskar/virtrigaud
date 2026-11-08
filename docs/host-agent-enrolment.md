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
(generating the key and the CSR on the host) is not part of this page.

## Host fields

| Field | Meaning |
|-------|---------|
| `spec.identity` | The host's identity anchor (TPM endorsement-key hash, hardware serial, administrator-assigned UUID). Required before the host can enrol. Set once: a CEL rule refuses changing or removing it. A replaced machine is a new identity (a new Host object), and never inherits the old host's pins. |
| `spec.revoked` | When `true`, no certificate request for the host is approved, no issued certificate is pinned, and the host is rendered into the inventory as unroutable. Pin removal and certificate revocation at the CA are not done by this manager yet. |
| `status.agent` | A mirror of the pins Secret (see below): `mode`, `enrolmentID`, `pendingEnrolmentID`, `requestURIs`, `keyPins`, `certificateExpiry`. Public identifiers only. |
| condition `AgentEnrolled` | `True`/`Enrolled` once a key is pinned. `False` with `IdentityMissing`, `Revoked`, `AwaitingCertificateRequest`, `AwaitingCertificate`, `CertificateRequestDenied` or `CertificateRejected`. |

Libvirt hosts are unchanged: they get no `status.agent`, no condition and no
pins Secret.

## The enrolment record

When a host has an identity and is not revoked, the manager draws a 16-byte
enrolment id and stores a pending record in the host's pins Secret. The record
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
CA authenticates it. An enrolment id is never drawn twice for a host.

## Authorisation rules

The manager approves a `CertificateRequest` (cert-manager condition
`Approved`) only if every rule holds; otherwise it sets `Denied` with one of
the reasons below. Nothing else approves these requests: **cert-manager's
default approver must be disabled for the host-agent issuer** (run
cert-manager with `--controllers=*,-certificaterequests-approver` and approve
other issuers with approver-policy, or make sure the default approver has no
`approve` permission on this signer). Otherwise cert-manager approves before
the manager can check anything.

| Rule | Denial reason |
|------|---------------|
| The request names an existing Host (annotation `enrolment.virtrigaud.io/host: <namespace>/<name>`) | `HostNotNamed`, `HostNotFound` |
| The Host's Provider runs a per-host agent | `HostNotAgentManaged` |
| The Host has `spec.identity` | `HostIdentityMissing` |
| The Host is not revoked | `HostRevoked` |
| A record is pending, the Host is not already enrolled, and no other request is in flight | `NoPendingEnrolment`, `AlreadyEnrolled`, `EnrolmentInFlight` |
| The requester (`spec.groups`, recorded by cert-manager's admission webhook) is in the enrolment administrator group | `RequesterNotAuthorised` |
| `spec.request` is one PEM CSR | `RequestInvalid` |
| The CSR's signature verifies with its own key (proof of possession) | `ProofOfPossessionFailed` |
| The key is ECDSA P-256/P-384, Ed25519, or RSA of 2048 to 8192 bits | `KeyUnsupported` |
| The subject alternative names are exactly the host URI and the pending record (no DNS, IP or e-mail names) | `SubjectAltNamesRefused` |
| The subject is empty or a common name equal to the host name | `SubjectRefused` |
| The CSR asks for no extension other than the subject alternative names (and basic constraints with the CA bit clear) | `ExtensionRefused` |
| No CA certificate (`spec.isCA` false, no CA bit in the CSR) | `CARequested` |
| `spec.usages` include `client auth` and `server auth` (`digital signature` and `key encipherment` are tolerated, nothing else) | `UsagesRefused` |
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
  (`--dev-host-enrolment-external-pki-issuer`); no cert-manager controller
  signs it. After approval, an operator writes the authorised bundle (the CSR
  and the issuance policy) with `vrtg host enroll --bundle-out`, has the
  enterprise CA sign it, and returns the certificate with
  `vrtg host enroll --certificate`, which writes it into
  `status.certificate`. The manager then sets `Ready=True` (pinned) or
  `Ready=False`/`Failed` (refused). cert-manager's approval check must be able
  to resolve the external reference's kind and signer name; grant the manager
  `approve` on that signer too.

In both paths the manager pins the key only after the issued certificate
passes these checks against the authorisation: it chains to the configured
subordinate CA bundle (`--dev-host-enrolment-ca-bundle-file`) for client and
server authentication; it is not a CA; its lifetime is at most 30 days; its
subject alternative names are exactly the authorised URIs; and its key is the
authorised key. A certificate issued after the Host was revoked is refused.

## The pins Secret

Each enrolled Host has a manager-owned Secret `virtrigaud-agent-pins-<host>` in
its namespace, controlled by the Host (garbage-collected with it), labelled
`app.kubernetes.io/component: host-agent-pins` and
`app.kubernetes.io/managed-by: virtrigaud`. It holds one JSON document: the
Host UID and identity it is bound to, the pending record, the authorised
request, the current enrolment (record, key pins, CA bundle, expected SAN,
expiry) and the retired enrolment ids. It holds no private key.

The Secret is authoritative; `Host.status.agent` only mirrors it. A lost or
edited status is rewritten from the Secret, and the inventory render reads the
Secret, never the status. A state bound to another Host UID or identity (a
replaced host) is discarded, never inherited. A Secret with that name that the
manager does not manage is never read as pins or overwritten.

## Inventory credentials

For a Provider that runs per-host agents, the host-inventory Secret renders
each enrolled, unrevoked host with `credentials.agentTLS`:

```json
"credentials": {
  "agentTLS": {
    "caBundle": "<base64 PEM of the subordinate CA>",
    "expectedSAN": "spiffe://<trust domain>/virtrigaud/host/<namespace>/<host>",
    "keyPins": ["<64 hex>"]
  }
}
```

The Provider's credential Secret is not read for these hosts, and validation
refuses a host that mixes SSH material with `agentTLS`. Hosts that are not
enrolled, are revoked or have no identity are listed in `unroutableHostIds`.

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

`host enroll` checks the CSR file locally (one PEM CSR whose signature
verifies). With `--manifest-out` it writes the `CertificateRequest` to a file
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
every record. cert-manager must be installed (its `CertificateRequest` CRD).

## RBAC

The default RBAC grants nothing for enrolment. Apply
`config/rbac/host-enrolment/` (edit its placeholders) or set the Helm values
`experimental.hostEnrolment.enabled`, `experimental.hostEnrolment.namespace`
and `experimental.hostEnrolment.signerNames`. They grant exactly:

- `approve` on `signers.cert-manager.io` for the named signer names only
  (`resourceNames`, for example
  `issuers.cert-manager.io/<namespace>.<issuer>`; wildcards are refused);
- `create`, `get`, `list`, `watch` on `certificaterequests` and `update` on
  `certificaterequests/status`, in the enrolment namespace only (a `Role`).

The pins Secrets are covered by the manager's existing Secret permissions;
nothing is added for them. Administrators who submit requests need `create`
on `certificaterequests` in the enrolment namespace, and, for the external-PKI
return, `update` on `certificaterequests/status` there.

## Not done yet

Rotation, re-enrolment, removing pins on revocation, revoking at the CA, and
the agent side (key generation, CSR, certificate installation) are later work.
