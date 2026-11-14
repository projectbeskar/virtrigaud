# Manager Secret access

This page lists every Kubernetes Secret the VirtRigaud manager reads or writes,
the RBAC each access needs, and the two ways the Helm chart can grant it:

- **`rbac.secrets.scope: cluster`** (the default). The manager's ClusterRole
  grants Secret `get`, `list`, `watch`, `create`, `update` and `delete` in every
  namespace, and its Secret informer caches every Secret in the cluster.
- **`rbac.secrets.scope: namespaced`** (opt-in). No ClusterRole grants Secrets.
  The manager gets a Role in each namespace you list, and its Secret informer
  covers only the Provider namespaces.

Provider pods have no Secret RBAC in either mode. They read only the files the
kubelet mounts for them.

## Every Secret the manager touches

| Secret | Namespace | Name | Verbs | Read through | Code |
|---|---|---|---|---|---|
| Provider TLS Secret (`spec.runtime.service.tls.secretRef`) | Provider's | chosen by the user | `get` | informer cache, so `list` and `watch` too | `internal/runtime/remote/resolver.go` (`validationKey`, `buildTLSConfig`); read on every Provider resolve |
| Host credential Secrets of a clustered Provider (`Host.spec.credentialSecretRef`, otherwise the Provider's `spec.credentialSecretRef`) | Provider's (a reference to another namespace is not read) | chosen by the user | `get` | informer cache | `internal/controller/provider_hostinventory.go` (`resolveHostCredentials`) |
| Host-inventory Secret of a clustered Provider | Provider's | `<provider>-hosts` | `get`, `create`, `update` (no `delete`: owned by the Provider and garbage-collected with it) | informer cache | `internal/controller/provider_hostinventory.go` (`reconcileHostInventorySecret`) |
| Operation-intent Secrets (only for a Provider whose type requires operation identity; development builds only) | Provider's | `vrtg-op-<op_id>`, and `vrtg-opr-<op_id>` (redacted) | `create`, `get`, `list`, `delete` | `get`: API server (`APIReader`). `list` (the sweep): informer cache, by label, in the Provider's namespace | `internal/controller/operation_intent.go` |
| Cloud-init user data and metadata (`spec.userData.cloudInit.secretRef`, `spec.metaData.cloudInit.secretRef`) | VirtualMachine's | chosen by the user | `get` | informer cache when it covers the namespace, otherwise API server (`APIReader`) | `internal/controller/cloudinit.go`, `secret_access.go` (`getWorkloadSecret`) |
| S3 staging credentials (`spec.storage.s3.credentialsSecretRef`) | VMMigration's | chosen by the user | `get` | informer cache when it covers the namespace, otherwise API server (`APIReader`) | `internal/controller/vmmigration_controller.go` (`loadS3Credentials`) |
| Per-host agent pins Secrets (the experimental per-host agent enrolment, development builds only, where present) | the enrolment namespace | derived from the Host's namespace and name | `get`, `list`, `watch`, `create`, `update`, `delete` | informer cache | granted by that feature's own Role, never by the rules on this page (the chart only adds the namespace to the informer) |

The manager does **not** read the Provider's `spec.credentialSecretRef` for a
single-host Provider, nor the chart's `providerTLS.secretName`. It mounts them
into the provider pod as volumes, and the kubelet reads them. That needs no
manager RBAC.

The manager deletes only Secrets that it can tell are its own: operation-intent
Secrets (by type, label, name prefix and the UID it read) and, where present,
pins Secrets (by owner labels, with UID and resourceVersion preconditions).
RBAC cannot express that limit, because it cannot scope a verb by label, type or
name prefix. In both modes the `delete` verb therefore covers every Secret in
the namespaces where it is granted.

## Why nothing narrower

- **`resourceNames`** would fit only fixed names. Every Secret above is named by
  a user (TLS, credentials, cloud-init, S3) or derived from an object
  (`<provider>-hosts`, `vrtg-op-<op_id>`, `vrtg-opr-<op_id>`, the pins hash). A `create` rule cannot
  use `resourceNames` at all.
- **A label-selected informer** (`cache.Options.ByObject` with a label
  selector) caches less, but still needs `list` and `watch` on all Secrets in
  the namespace, because RBAC does not scope by label. The user-named Secrets
  are not labelled either.
- **Workload namespaces need `get`.** A cloud-init or S3 Secret lives next to
  the VirtualMachine or VMMigration that names it, so the manager must be able
  to read Secrets wherever tenants create those objects. In namespaced mode
  this is `get` only, without `list`, `watch` or write.

## The default: cluster scope

The default keeps the grant that earlier installations have:

```yaml
rbac:
  secrets:
    scope: cluster
```

It works for any namespace layout, including tenants that create
VirtualMachines in new namespaces at any time. The cost is that a compromised
manager can read every Secret in the cluster, including Secrets that have
nothing to do with VirtRigaud, such as a CA key. The manager also keeps a copy
of every Secret in the cluster in memory.

The default stays cluster scope because the chart cannot know where your
Providers, VirtualMachines and VMMigrations live. If the default were narrowed,
an existing installation would lose access to Secrets in namespaces it has never
been told about, and its VMs, migrations and Providers there would start failing
on upgrade.

## Opt-in: namespaced scope

```yaml
rbac:
  secrets:
    scope: namespaced
    # Namespaces of your Provider objects. Required.
    providerNamespaces:
      - virtrigaud-providers
    # Namespaces of VirtualMachines with a cloud-init secretRef and of
    # VMMigrations with S3 storage.
    workloadNamespaces:
      - team-a
      - team-b
```

The chart then renders:

| Where | Object | Secret verbs |
|---|---|---|
| Manager ClusterRole (or the release-namespace Role with `rbac.scope: namespace`) | no Secret rule | none |
| Each `providerNamespaces` entry | Role and RoleBinding `<release>-virtrigaud-manager-secrets` | `get`, `list`, `watch`, `create`, `update`, `delete` |
| Each `workloadNamespaces` entry that is not also a Provider namespace | Role and RoleBinding `<release>-virtrigaud-manager-secrets-read` | `get` |
| Manager Deployment | `--secret-cache-namespaces=<providerNamespaces>` (plus the enrolment namespace, see below) | (informer limited to those namespaces) |

Each binding names only the manager's ServiceAccount. When the per-host agent
enrolment is enabled, the chart adds its namespace to
`--secret-cache-namespaces`, because pins Secrets are watched through the cache.
The manager also adds the enrolment namespace itself when it is given a
restricted list and enrolment is enabled, so a Kustomize or hand-written flag
cannot leave the pins Secret watch blind.
This chart renders no Secret Role there: the Secret verbs in that namespace come
from the enrolment feature's own Role. With `rbac.create: false` the chart
renders no RBAC but still sets `--secret-cache-namespaces`. You must then
create the equivalent Roles yourself.

At startup, a manager with `--secret-cache-namespaces` checks, with
SelfSubjectAccessReviews, that it may `list` and `watch` Secrets in each of
those namespaces. If it may not, it logs the namespaces and verbs it lacks and
exits. Without the check, a cached Secret read in such a namespace would wait
for the informer to sync and never return an error, so the Provider and its
VMs would hang without one. The check needs `create` on
`selfsubjectaccessreviews`, which Kubernetes grants every authenticated user
through the default `system:basic-user` ClusterRole.

`workloadNamespaces` must list every namespace where a VirtualMachine with a
cloud-init Secret, or a VMMigration with S3 storage, can appear. That includes
the target namespaces where VMClone, VMSet and VMMigration create
VirtualMachines: the created VM reads its cloud-init Secret in its own
namespace.

Rendering fails if `providerNamespaces` is empty, if a name is not a valid
namespace name, or if `scope` is anything other than `cluster` or `namespaced`.

### What happens outside the listed namespaces

A Secret in a namespace you did not list is refused:

- **VirtualMachine with a cloud-init Secret.** Its create request cannot be
  built. The VM reports reason `ProviderError` with
  `Failed to build create request: resolving cloud-init user data: fetching cloud-init secret "<name>": ... forbidden`.
  The create is retried every few seconds, and succeeds once the namespace is
  added. A reconfigure of an existing VM fails the same way.
- **VMMigration with S3 storage.** The migration fails at validation with
  `Invalid s3 storage configuration: get s3 credentials secret ...: ... forbidden`.
  This failure is final for that migration. Add the namespace, then create the
  migration again.
- **Provider in an unlisted namespace.** The cached reads fail with
  `unable to get: <namespace>/<name> because of unknown namespace for the cache`.
  The Provider's TLS Secret cannot be loaded, and for a clustered Provider no
  host-inventory Secret can be written. The Provider cannot be used until its
  namespace is added to `providerNamespaces`.

To add a namespace, add it to the list and run `helm upgrade`. The changed
`--secret-cache-namespaces` argument rolls the manager.

### What remains

Namespaced mode removes the manager's direct Secret access outside the listed
namespaces: no API read or list there, and no in-memory copy of those Secrets.
It does **not** make those Secrets unreachable to a compromised manager. The
manager keeps its cluster-wide grant on Deployments, Services and
ServiceAccounts, which it needs to run provider pods. With that grant it could
create a Deployment in any namespace that mounts a Secret of that namespace as
a volume, or runs under an existing ServiceAccount there. The kubelet mounts
the Secret, so no Secret RBAC is involved. To close that path, use an admission
policy (a ValidatingAdmissionPolicy, Kyverno or Gatekeeper) that allows the
manager's ServiceAccount to create Deployments only in the Provider namespaces,
with only the provider ServiceAccounts the manager creates. Narrowing the
workload grants themselves is separate work.

Within the listed namespaces, the manager can read, create, update and delete
any Secret in each Provider namespace, and read any Secret in each workload
namespace if it knows the name. So:

- Keep Provider namespaces for Provider objects and their own Secrets. Do not
  put unrelated high-value Secrets, such as CA keys or other applications'
  credentials, in a Provider namespace or a workload namespace.
- Operation-intent Secrets of consumer VMs copy their cloud-init user data into
  the Provider namespace (see [operation-grants-manager.md](operation-grants-manager.md#intent-secrets)).
  Restrict Secret read in Provider namespaces to the manager and the
  administrators.
- The per-host agent enrolment feature's own Role grants the manager full
  Secret access in its namespace. Keep the cert-manager signing CA's key out of that namespace (for
  example, a ClusterIssuer whose key lives in cert-manager's cluster resource
  namespace).
- Enable encryption at rest for Secrets in etcd.

### Performance

In the default mode nothing changes: every Secret read is a cache read, as
before. In namespaced mode the informer holds only the Provider namespaces'
Secrets, instead of every Secret in the cluster. A cloud-init or S3 credential
Secret in a workload namespace is then read from the API server: each time a
VM's create or reconfigure request is built, and on each S3 migration step that
needs the credentials. That adds a few API requests per create or migration
step, counted against the manager's client rate limit (`manager.clientQPS`).
A read the API server cannot answer for the moment (throttled, timed out,
unavailable, internal error) is retried. A missing or forbidden Secret fails
the migration as before.

## Kustomize

`config/rbac/role.yaml` is generated from the kubebuilder markers in
`internal/controller/` and holds the cluster-scope grant. To run namespaced with
kustomize:

1. Remove the `secrets` rule from the ClusterRole with a kustomize patch, not a
   hand edit: `make manifests` regenerates `config/rbac/role.yaml`.
2. Create the Roles and RoleBindings from the table above.
3. Add `--secret-cache-namespaces=<provider namespaces>` to the manager's
   arguments.

## Checking the grant

```bash
SA=system:serviceaccount:virtrigaud-system:virtrigaud
kubectl auth can-i list secrets --all-namespaces --as "$SA"                # namespaced: no
kubectl auth can-i create secrets -n virtrigaud-providers --as "$SA"       # yes
kubectl auth can-i get secrets -n team-a --as "$SA"                        # yes
kubectl auth can-i list secrets -n team-a --as "$SA"                       # namespaced: no
kubectl auth can-i get secrets -n kube-system --as "$SA"                   # namespaced: no
```

Replace the namespace and ServiceAccount name with your release's. The
ServiceAccount is `manager.serviceAccount.name` when set. Otherwise it is
`<release>-virtrigaud`, or just `<release>` when the release name already
contains `virtrigaud`.

## Tests

- `internal/controller/rbac_secrets_test.go` pins the Secret markers, the
  generated `config/rbac/role.yaml`, and the chart output in both modes,
  including the refusals.
- `internal/controller/secret_access_envtest_test.go` applies the chart's
  namespaced RBAC to a real API server. It then runs each access above as the
  manager's ServiceAccount through a manager with the restricted informer, and
  checks that Secrets elsewhere are refused.
