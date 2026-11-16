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
  pins Secrets (`create`, `delete`, `get`, `list`, `update`, `watch`).

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
  checks the names (spiffe host URIs and `urn:virtrigaud:` URIs only, the
  host's name as common name, which every agent request carries). It is an
  approver, not a filter: approver-policy approves the requests it allows
  itself, which replaces the manager's approval decision for its renewal
  requests and approves allowed-shape requests the manager never would. See
  `docs/host-agent-enrolment.md` (RBAC) before using it.

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
