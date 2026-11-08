# Per-host agent enrolment RBAC (development builds only)

These manifests grant the manager what per-host agent enrolment needs. They are
**not** part of the default kustomization, and only a manager built with the
`virtrigaud_dev` build tag and started with the `--dev-host-enrolment-*` flags
uses them.

Edit the placeholders before applying:

- `host-agent-pki`: the namespace of the host-agent issuer and of the enrolment
  `CertificateRequest`s (`--dev-host-enrolment-namespace`).
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
- `get`, `list`, `watch` and `create` on `certificaterequests`, and `update` on
  `certificaterequests/status` (to set `Approved`/`Denied`, and `Ready` on the
  external-PKI path), in the issuer namespace only (a `Role`).

The per-Host pins Secrets live in each Host's namespace and are covered by the
manager's existing Secret permissions; nothing here adds Secret access.

cert-manager's default approver must not approve requests for the host-agent
issuer: run cert-manager with `--controllers=*,-certificaterequests-approver`
and approve other issuers with approver-policy, or otherwise make sure the
default approver has no `approve` permission on this signer.
