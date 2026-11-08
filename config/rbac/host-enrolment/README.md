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
- `issuers.cert-manager.io/host-agent-pki.host-agent-ca`: the cert-manager
  signer name of the host-agent issuer: `issuers.cert-manager.io/<namespace>.<name>`
  for an `Issuer`, `clusterissuers.cert-manager.io/<name>` for a `ClusterIssuer`.
  With an external-PKI issuer reference, add its signer name too
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
- in the enrolment namespace only (a `Role`): `get`, `list`, `watch` on
  `certificaterequests`, `update` on `certificaterequests/status` (to set
  `Approved`/`Denied`, and `Ready` on the external-PKI path), and the pins
  Secrets (`create`, `delete`, `get`, `list`, `update`, `watch`).

The manager does not get `create` on `certificaterequests`: it only decides
requests administrators submitted.

Administrators who submit requests need, besides `create` on
`certificaterequests` in the enrolment namespace, the `enrol` verb on the
Hosts they enrol, for example:

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
```

cert-manager's default approver must not approve requests for the host-agent
issuer: run cert-manager with `--controllers=*,-certificaterequests-approver`
and approve other issuers with approver-policy, or otherwise make sure the
default approver has no `approve` permission on this signer.
