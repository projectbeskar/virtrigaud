# Per-host agent enrolment RBAC (development builds only)

These manifests grant the manager what per-host agent enrolment needs. They are
**not** part of the default kustomization, and only a manager built with the
`virtrigaud_dev` build tag and started with the `--dev-host-enrolment-*` flags
uses them.

Edit the placeholders before applying:

- `host-agent-pki`: the enrolment namespace: the namespace of the host-agent
  issuer, of the enrolment `CertificateRequest`s and of the per-Host pins
  Secrets (`--dev-host-enrolment-namespace`). Grant nobody but the manager
  Secret write access in it.
- `clusterissuers.cert-manager.io/host-agent-ca`: the cert-manager signer
  name of the host-agent issuer, a `ClusterIssuer`
  (`clusterissuers.cert-manager.io/<name>`). Use a ClusterIssuer, so the
  sub-CA key Secret lives in cert-manager's cluster resource namespace and not
  in the enrolment namespace, where the manager can write Secrets. A
  namespaced `Issuer` whose key Secret sits in the enrolment namespace is not
  supported. With an external-PKI issuer reference, add its signer name too
  (`<resource plural>.<group>/<namespace>.<name>`).
- `virtrigaud-system` / `virtrigaud-controller-manager`: the manager's
  ServiceAccount.

The manager must not be able to read Secrets in all namespaces or the sub-CA
key Secret, enter pods in the key Secret's namespace, or grant itself that
access (impersonation, token minting, role binding), even through a rule
limited to named objects. It refuses to run enrolment otherwise, and suspends
approvals if a re-check fails later (see `docs/host-agent-enrolment.md`, "The
manager must not read the sub-CA key"). The checks cover grants cluster-wide
and in the key Secret's namespace only, and are defence in depth, not proof. A
cluster with a webhook authorizer in its chain, which commonly includes managed
Kubernetes, always reports incomplete rules, so enrolment refuses there. `config/rbac/role.yaml`
grants Secret access in all namespaces, so replace that grant with
per-namespace Roles first (`docs/manager-secret-access.md`, "Kustomize"). Pass
`--dev-host-enrolment-ca-key-secret=<namespace>/<name>`: it is required with a
cert-manager issuer, and with an external signer that keeps its key in this
cluster it is your responsibility to set it.

What is granted, and nothing more:

- `approve` on the `signers` resource of `cert-manager.io`, for the named
  signer(s) only (`resourceNames`), so the manager cannot approve requests for
  any other issuer;
- `create` on `subjectaccessreviews`, to check that a request's requester may
  use the `enrol` verb on the Host it names;
- `get` and `list` on mutating and validating webhook configurations, to check
  at startup that cert-manager's admission webhooks cover `CertificateRequest`s
  (enrolment refuses to run otherwise);
- in the enrolment namespace only (a `Role`): `create`, `delete`, `get`,
  `list`, `watch` on `certificaterequests` (`create` and `delete` for the
  manager's own certificate renewal requests), `update` on `certificaterequests/status` (to
  set `Approved`/`Denied`, and `Ready` on the external-PKI path), and the
  pins Secrets and the host ledger Secrets (`create`, `delete`, `get`,
  `list`, `update`, `watch`).

Host ledger keys (optional, development builds only; the manager does not yet
sign grant version 2 or drive the ledger, see `docs/host-agent-enrolment.md`,
"Host ledger state"): create a Secret in the manager's namespace with data
keys `current` and `identity` (each `openssl rand -hex 32`), mount it into the
manager pod alone, and pass `--dev-host-ledger-key-dir=<mount path>` and
`--dev-host-ledger-key-secret=<namespace>/<name>`. No role here grants access
to it, and none may: the ledger objects' tag defends only while no principal
other than the manager may write Secrets in the enrolment namespace, and no
principal may read the key Secret through the API or reach it through a
workload in its namespace. The manager refuses enrolment while it may read the
key Secret through the API (replace the all-namespaces Secret grant of
`config/rbac/role.yaml` first, as above). Its pre-existing right to create
Deployments in every namespace remains a path to the key, as to the sub-CA
key: a custody limit, not a configuration to fix here.
`internal/controller/rbac_host_ledger_test.go` proves both over these
manifests and `config/rbac`; keep it passing for every role you add.

The manager creates only renewal requests, under its own identity, for a
renewal the host signed with its pinned key; it approves a renewal request
only when the recorded requester is its own identity. Every other request is
one an administrator submitted.

Residual: with `create` on `certificaterequests`, `update` on their status
and `approve` on the signer, the manager can obtain a host-agent certificate
by itself. Its renewal approval rules (co-signed by the currently pinned key,
identities unchanged) live in its own code; whoever controls the manager's
ServiceAccount can have the issuer sign any Host's names (the certificate is
pinned for no Host, but it is CA-valid). `admission/` holds two optional
controls, not referenced by `kustomization.yaml`; edit their placeholders
before applying them:

- `admission/manager-certificaterequests.yaml` (recommended): a
  `ValidatingAdmissionPolicy` and binding (Kubernetes 1.30 or later) that
  only denies. A `CertificateRequest` the manager's ServiceAccount creates is
  refused unless it is a renewal request of the manager's shape (enrolment
  namespace, renewal label and Host annotation, the host-agent ClusterIssuer,
  no CA, exactly the usages `digital signature` and `server auth`, at most 30
  days). It approves nothing. It cannot see the names inside the PKCS #10
  request.
- `admission/approver-policy.yaml`: a cert-manager approver-policy
  `CertificateRequestPolicy` bound to the manager's ServiceAccount, which
  checks the names (spiffe host and record URIs under the trust domain only, the
  host's name as common name, which every agent request carries). It is an
  approver, not a filter: approver-policy approves the requests it allows
  itself, which replaces the manager's approval decision for its renewal
  requests and approves allowed-shape requests the manager never would. See
  `docs/host-agent-enrolment.md` (RBAC) before using it.
- `admission/provider-identity-approver-policy.yaml`: an approver-policy
  `CertificateRequestPolicy` for a clustered Provider's identity certificate
  (the Provider identity issuer, the Provider's namespace, its own URI only),
  bound to cert-manager's ServiceAccount; with the default approver disabled
  a Certificate for it needs an approver. See
  `docs/clustered-agent-provider-development.md`.

Residual: a principal with `update` or `delete` on `certificaterequests` in
the enrolment namespace can delete or relabel each new renewal request, so
each counts as a failure and a key is given up after 5 (renewal then waits
for the agent's next key). Grant those verbs there to no one but cert-manager
and the manager.

Residual: the manager's existing cluster-wide Secret permission (from the
default RBAC) still reaches every Secret, including the sub-CA key. Narrowing
it is separate work.

A wildcard verb (`*`) on `hosts` satisfies `enrol`: audit such roles before
adding anyone to the enrolment administrator group.

Administrators who submit requests need, besides `create` on
`certificaterequests` in the enrolment namespace, the `enrol` verb on the
Hosts they enrol, and the separate `rekey` verb on the enrolled Hosts they
re-key (grant it only to those who may replace the key of a Host in service),
for example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: host-enrolers
  namespace: <host namespace>
rules:
- apiGroups: ["infra.virtrigaud.io"]
  resources: ["hosts"]
  verbs: ["enrol"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: host-rekeyers
  namespace: <host namespace>
rules:
- apiGroups: ["infra.virtrigaud.io"]
  resources: ["hosts"]
  verbs: ["rekey", "patch"]   # patch: vrtg host rekey sets the request annotation
```

Anyone with `update` or `patch` on a Host can open its re-key window (the
`enrolment.virtrigaud.io/rekey-requested` annotation); the window grants
nothing by itself, since the request still needs the `rekey` verb and the
administrator group. The API server's audit log attributes the annotation.

cert-manager's default approver must not approve requests for the host-agent
issuer: run cert-manager with `--controllers=*,-certificaterequests-approver`
and approve other issuers with approver-policy, or otherwise make sure the
default approver has no `approve` permission on this signer.
