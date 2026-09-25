# Cross-namespace references: `spec.consumerNamespaceSelector`

A `VirtualMachine` names its `Provider`, `VMClass` and `VMImage` with
`spec.providerRef`, `spec.classRef` and `spec.imageRef`. Each reference can set
a `namespace`. The manager has cluster-wide RBAC, so without a check anyone
allowed to create a `VirtualMachine` in `team-a` could:

- drive a `Provider` in `team-b`, and with it `team-b`'s hypervisor
  credentials, to create, power and delete VMs;
- create VMs from `team-b`'s `VMImage`s, which reads the data in their
  templates or disk images;
- size VMs with `team-b`'s `VMClass`es.

`VMClone`, `VMMigration` and `VMSnapshot` act through the same references.

**A reference to another namespace's `Provider`, `VMClass` or `VMImage` is
refused unless that object shares itself with the referencing namespace.**

## The rule

`Provider`, `VMClass` and `VMImage` have an optional field,
`spec.consumerNamespaceSelector` (a standard Kubernetes label selector):

| `spec.consumerNamespaceSelector` | Who may reference the object |
|---|---|
| unset (the default) | only its own namespace |
| `{}` (empty selector) | every namespace |
| `matchLabels` / `matchExpressions` | its own namespace, plus every namespace whose labels match |

- **The own namespace is always allowed.** A reference with no `namespace`, or
  one that names the referencing object's own namespace, works as before. No
  selector is needed and the manager reads no `Namespace` object.
- **The selector is matched against the `Namespace` object's labels.** Every
  namespace carries the label `kubernetes.io/metadata.name: <name>` (set by the
  API server), so you can name namespaces explicitly.
- **Any selector with no requirements selects every namespace.** That is `{}`,
  and also `matchLabels: {}` or `matchExpressions: []` on their own. Only an
  unset field means "own namespace only".
- **Negative selectors also match namespaces created later.** `NotIn` and
  `DoesNotExist` (for example "every namespace except `team-x`") select every
  new namespace that doesn't carry the excluded label. Prefer positive
  selectors (`In`, `Exists`, `matchLabels`) that name who may use the object.
- **Each object grants for itself.** Sharing a `Provider` doesn't share its
  `VMClass`es or `VMImage`s, and the other way round.
- **Sharing a `Provider` shares everything its credentials can reach.** A
  tenant allowed to use a `Provider` can create its own `VMImage` in its own
  namespace (no grant needed) that names any vSphere template or
  content-library item, Proxmox template, or libvirt image file the Provider's
  account can read, and create VMs from it. The `VMImage` grant protects the
  `VMImage` objects of another namespace, not the data behind a shared
  `Provider`. Share a `Provider` only with namespaces trusted with everything
  its hypervisor account can see, and scope that account accordingly.

A `Provider` shared with two namespaces by name:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  namespace: virtrigaud-system
spec:
  type: vsphere
  # ...
  consumerNamespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: In
        values: ["team-a", "team-b"]
```

A `VMImage` shared with every namespace labelled `virtrigaud.io/tenant=gold`:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22-04
  namespace: virtrigaud-system
spec:
  source:
    # ...
  consumerNamespaceSelector:
    matchLabels:
      virtrigaud.io/tenant: gold
```

A complete example (a shared `Provider`, `VMClass` and `VMImage`, and a VM in
another namespace that uses them) is in
[`examples/shared-provider-consumer-namespaces.yaml`](../examples/shared-provider-consumer-namespaces.yaml).

## Who can grant

Two parties decide together:

- **Whoever can edit the `Provider`, `VMClass` or `VMImage`** sets its
  selector. Keep `update`/`patch` on those objects limited to the owners of the
  namespace they live in.
- **Whoever can label a `Namespace`** decides whether that namespace matches a
  label selector. `Namespace` is cluster-scoped, so on a plain cluster that's a
  cluster administrator: a tenant who can only write objects inside its own
  namespace can't label its namespace, and so can't give itself access.

**Self-service platforms can change who labels namespaces.** Tools like
Capsule, HNC and Rancher projects may let a namespace owner edit its own
`Namespace`, including its labels. There, a tenant can add whatever label a
shared object's selector asks for. Where that matters:

- prefer selectors on `kubernetes.io/metadata.name` (the API server sets it and
  nobody can change it), or `{}` for objects meant for everyone; or
- restrict the labels your selectors use with a tenant policy, for example a
  Kyverno or Gatekeeper rule that only lets platform administrators set them.

The same caveat applies to the target-namespace grant for clones and
migrations; see [`cross-namespace-targets.md`](cross-namespace-targets.md).

**Selectors match names and labels, not identities.** A namespace deleted and
re-created under the same name, or re-labelled, matches again.

## When a reference is refused

The manager makes no provider call through, or for, the refused reference. It
doesn't resolve the `Provider` to a client, doesn't prepare the image, and
doesn't create, describe, power, reconfigure or delete the VM. The object
reports:

- `Ready=False`, reason `ConsumerNotAllowed`, with a message that names the
  referenced kind, namespace and name and the field that would grant access,
  for example:
  `Provider virtrigaud-system/vsphere-prod may not be used from this namespace:
  a reference from another namespace is allowed only when the Provider's
  spec.consumerNamespaceSelector selects the referencing namespace; no provider
  call is made`. A `VMMigration` refused while validating also sets
  `Validating=False` with the same reason.
- One `Warning` event with reason `ConsumerNotAllowed`, when the refusal
  starts.
- `observedGeneration` set to the current generation on the condition.

The refusal isn't a failure: nothing moves to `Failed`, a migration's retry
count doesn't change, and a clone or migration in flight keeps its phase and
state. The message is the same whether the referenced object doesn't exist or
exists but doesn't select the namespace, and it never shows the selector, so a
refusal tells a tenant nothing about another namespace.

### What each controller checks

| Object | Checked | Consumer namespace |
|---|---|---|
| `VirtualMachine` | the `Provider` on every reconcile, before the provider is resolved, and before the finalizer deletes. The `VMClass` and `VMImage` before the VM is created (and in the image-prepare path, before any prepare call or `VMImage` status write); once the VM exists, only where their content is used — see below | the VM's |
| `VMSnapshot` | the VM's `Provider`, before create, the task poll and the delete | the snapshot's |
| `VMClone` | the source VM's `Provider`; the `Provider` and `VMClass` the target VM will reference. Every reconcile, and re-read from the API server right before the provider `Clone` call and the target VM `Create` | the clone's for the source; the target namespace for the target's references |
| `VMMigration` | the source VM's `Provider`; the target `Provider`; the target `VMClass`. Before every phase except `Ready` and `Failed`, and re-read from the API server right before `SnapshotCreate`, `ExportDisk`, `ImportDisk` and the target VM `Create` | the migration's; the target namespace too for the target `Provider` and `VMClass` |
| Adoption | never binds an existing adopted-labelled VM whose `VMClass` or `VMImage` is refused. Adopted VMs are created in the `Provider`'s own namespace, so they need no grant | the `Provider`'s |

#### A VM that already exists

Every provider call goes through the `Provider`, so a `Provider` the VM's
namespace may no longer use stops everything: describe, power, reconfigure and
delete. The `VMClass` and `VMImage` are content the VM was created from, so
their grants are checked only where that content is used again:

| Operation on a bound VM | `VMClass` grant needed | `VMImage` grant needed |
|---|---|---|
| describe, power on/off | no | no |
| reconfigure to the class (and applying a finished reconfigure) | yes | no (the image is not sent) |
| re-create a VM missing on its hypervisor, or a clustered create still in flight | yes | yes |

So revoking an image share never stops VMs created from the image, and revoking
a class share stops only reconfiguring to it: the VM keeps running and is
powered as before, but reports `Ready=False` / `ConsumerNotAllowed` naming the
class. A VM that is not bound yet needs all three grants before anything is
created.

A clone or migration into **another namespace** needs both grants: the target
namespace must allow the source namespace
(`infra.virtrigaud.io/allowed-source-namespaces`, see
[`cross-namespace-targets.md`](cross-namespace-targets.md)), and the `Provider`
and `VMClass` pinned onto the target VM must select the target namespace. The
target-namespace grant is checked first.

### Granting, and how fast it takes effect

Either of these lets the object continue from where it stopped:

- Set (or widen) the referenced object's `spec.consumerNamespaceSelector`.
- Label the referencing namespace so an existing selector matches it.

The manager watches `Namespace` label changes and the
`spec.consumerNamespaceSelector` of `Provider`s, `VMClass`es and `VMImage`s,
and re-runs the affected objects right away; status-only updates don't trigger
anything. It finds them through an index, not by listing everything: a
selector change re-runs only the VMs that reference that object from another
namespace and the clones, migrations and snapshots refused on it; a namespace
label change re-runs only the objects whose grants depend on that namespace.
A refused object is also re-checked every 5 minutes as a safety net; a recheck
that finds the same refusal writes nothing. It isn't retried faster, because
only whoever can set the selector or the namespace labels can lift the
refusal.

Or point the reference at an object in the referencing namespace, if the VM
isn't bound yet (`spec.providerRef` is immutable once bound, see
[`vm-provider-binding.md`](vm-provider-binding.md)).

### Revoking

Removing a namespace from a selector takes effect on the next check. For a
`Provider`, a bound VM then fails closed: it keeps its `status.id` and its
hypervisor VM, but the manager stops describing, powering and reconfiguring it
until access is restored. For a `VMClass` or `VMImage`, see
[A VM that already exists](#a-vm-that-already-exists). Nothing is deleted or
unbound. A `VirtualMachine` is re-run when the selector of an object it
references from another namespace changes, or when its namespace's labels
change, so a revocation is noticed within seconds.

## Deleting a VM whose `Provider` is refused

The finalizer never deletes a hypervisor VM through a `Provider` the VM's
namespace may not use. Like a `ProviderRefMismatch`, the refusal **keeps the
finalizer**: the `VirtualMachine` stays in `Terminating` with
`Ready=False`/`ConsumerNotAllowed` and a `Warning` event explaining the options:

- restore access (the selector or the namespace labels), and the normal delete
  runs; or
- set `virtrigaud.io/orphan-on-delete: "true"` to detach: the finalizer is
  removed without any provider call and the hypervisor VM is left in place; or
- set `virtrigaud.io/force-delete: "true"` to drop the finalizer (also without
  a provider call).

A refused `VMClass` or `VMImage` doesn't block deletion: the delete only uses
the `Provider`. A cross-namespace `Provider` that no longer exists is refused
like one that doesn't select the namespace (the finalizer is kept), so the
outcome never reveals whether it exists. If a `Provider` in the VM's own
namespace no longer exists, the finalizer is removed as before.

## Shared `VMImage`s: prepare state is per Provider

A shared `VMImage` is prepared separately on each `Provider` that uses it
([`image-preparation.md`](image-preparation.md#prepare-state-is-per-provider)).
Its `status.providerStatus` is keyed by the Provider's `<namespace>/<name>`, and
each entry records the UID of the Provider object it was recorded through and
its own prepare task. So when `team-a` and `team-b` each have a Provider named
`vsphere` and both use `virtrigaud-system/ubuntu`:

- `team-b` preparing first records `team-b/vsphere` only. A VM on
  `team-a/vsphere` still prepares through its own Provider and is created from
  what that Provider reported, never from `team-b`'s entry.
- Each prepare task is polled only through the Provider that started it, so a
  task on one Provider can never mark the image ready on another.
- A Provider deleted and re-created under the same name re-validates what its
  predecessor prepared before a VM is created from it.
- The image is prepared only before a VM is created (or re-created). Running
  VMs never prepare, so a change to a shared image's state never stops them.

Prepare state an earlier release recorded under a bare Provider name is
migrated to the `VMImage`'s own namespace only when a Provider of that name
exists there, and re-validated; otherwise it is dropped. It never satisfies a
Provider in another namespace.

**Known limitation: prepared artifacts on the hypervisor are not per tenant.**
This separates the operator's records, not the hypervisor. A prepared template
or image file is named after the `VMImage` (the bare name), and each provider's
prepare accepts an existing artifact of that name as already prepared, without
checking who created it or from what. Two Providers whose accounts reach the
same inventory (the same vCenter datacenter, Proxmox node, or libvirt pool
directory) therefore share one artifact, whichever prepared it first: a tenant
allowed to use a shared `Provider` can pre-create, or later change, the template
another tenant's VMs are created from. This needs a design change (tracked
separately). Until then, as with a shared `Provider`, give separate tenants
separate hypervisor accounts scoped to what each may see, and don't share a
`Provider` between tenants that must not influence each other's images.

## What is never sent to a provider

The selector is operator-side policy. The manager strips
`consumerNamespaceSelector` from the `VMImage` spec it sends with an image
prepare and from the `VMClass` spec it sends with a clone.

## Admission

There is no admission webhook for this check; the controllers are the
enforcement point. A `VirtualMachine` that references an object it may not use
is accepted by the API server and then refused as described above. Webhooks are
optional in VirtRigaud, and a grant can change after admission, so the
controllers check on every reconcile either way.

## The manager checks the installed CRDs

With a `Provider`, `VMClass` or `VMImage` CRD older than the manager, the field
doesn't exist: the API server rejects it (strict field validation) or prunes it,
so no grant can be set and every cross-namespace reference is refused. The
manager's existing CRD check (see
[`vm-provider-binding.md`](vm-provider-binding.md#3-the-manager-checks-the-installed-crd))
therefore also requires `spec.consumerNamespaceSelector` in those three CRDs,
and `status.providerStatus[].providerUID` and `taskRef` in the `VMImage` CRD
(an older one prunes them, so no prepared image is trusted and no asynchronous
prepare is tracked): while one lacks a field, the state is `missing` and
readiness fails. The state is on
`virtrigaud_manager_vm_crd_security_features`.

## RBAC

- `namespaces`: nothing new. The manager already reads and watches them
  (`get`, `list`, `watch`), added for the clone and migration target grant. The
  `VirtualMachine` and `VMSnapshot` controllers now use it too. The manager
  never writes `Namespace` objects.
- `customresourcedefinitions`: `get` on `providers.infra.virtrigaud.io`,
  `vmclasses.infra.virtrigaud.io` and `vmimages.infra.virtrigaud.io`, next to
  the existing `virtualmachines.infra.virtrigaud.io`, for the CRD check. Only
  those four objects (`resourceNames`), in the chart's ClusterRole.

## Upgrade notes

- **Breaking: cross-namespace references need a grant.** A `VirtualMachine`
  that references a `Provider`, `VMClass` or `VMImage` in another namespace
  used to work with no grant. After the upgrade a VM whose `Provider` is not
  shared with it fails closed with `ConsumerNotAllowed`, including VMs that are
  already running: no provider calls, no deletes, until the `Provider` selects
  its namespace. (An unshared `VMClass` or `VMImage` blocks only what uses it —
  see [A VM that already exists](#a-vm-that-already-exists).)
- **Order: CRDs, then the selectors, then the manager.** The field exists only
  once the new CRDs are applied (an older CRD rejects or prunes it), and older
  managers ignore it:
  1. Apply the new CRDs. With Helm, the chart's pre-upgrade hook applies them
     in the same `helm upgrade` that rolls the manager; to get a pause between
     the two, apply the new chart's `crds/` (or `config/crd/bases/` from the
     release checkout) with `kubectl apply --server-side --force-conflicts`
     first — the hook's later re-apply is a no-op.
  2. Set `spec.consumerNamespaceSelector` on every shared `Provider`, `VMClass`
     and `VMImage` (queries below). The old manager is unaffected.
  3. Roll the manager (and then the providers).

  If you let `helm upgrade` do steps 1 and 3 together, set the selectors right
  after it: affected VMs are refused only until then, nothing is deleted, and
  the manager re-drives them within seconds of each grant.
- **Clones and migrations into another namespace** now also need the pinned
  `Provider` and `VMClass` to select the target namespace, in addition to the
  target-namespace annotation.
- **The manager's readiness also checks the three CRDs** for the field, and
  needs `get` on them (see [RBAC](#rbac)).
- **`VMImage` prepare state is re-keyed.** `status.providerStatus` and
  `status.availableOn` use `<namespace>/<name>` instead of the bare Provider
  name, and `status.prepareTaskRef` is no longer written. Existing state is
  migrated on the first VM create that uses each image, which re-issues the
  idempotent prepare once per image and Provider; running VMs are not affected
  (see [Shared `VMImage`s](#shared-vmimages-prepare-state-is-per-provider)).
  Update scripts that read these fields.

List what needs a selector (run it before step 3):

```sh
# VirtualMachines referencing another namespace's Provider, VMClass or VMImage.
kubectl get virtualmachines -A -o json | jq -r '
  .items[] | . as $vm
  | [["Provider", .spec.providerRef], ["VMClass", .spec.classRef], ["VMImage", .spec.imageRef]][]
  | select(.[1] != null and (.[1].namespace // "") != "" and .[1].namespace != $vm.metadata.namespace)
  | "\(.[0]) \(.[1].namespace)/\(.[1].name) <- \($vm.metadata.namespace)"' | sort -u

# VMMigrations whose target Provider is in another namespace.
kubectl get vmmigrations -A -o json | jq -r '
  .items[] | select((.spec.target.providerRef.namespace // "") != ""
    and .spec.target.providerRef.namespace != .metadata.namespace)
  | "Provider \(.spec.target.providerRef.namespace)/\(.spec.target.providerRef.name) <- \(.metadata.namespace)"' | sort -u
```

Each line is an object and a namespace that must be selected. Clones and
migrations into another namespace also need their pinned `Provider` and
`VMClass` to select the target namespace; list them with the query in
[`cross-namespace-targets.md`](cross-namespace-targets.md#upgrade-notes).
