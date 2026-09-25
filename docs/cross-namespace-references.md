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
- **Each object grants for itself.** Sharing a `Provider` doesn't share its
  `VMClass`es or `VMImage`s, and the other way round.

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
| `VirtualMachine` | `Provider`, `VMClass`, `VMImage` on every reconcile, before the provider is resolved; again in the image-prepare path before any prepare call or `VMImage` status write; the `Provider` again before the finalizer deletes | the VM's |
| `VMSnapshot` | the VM's `Provider`, before create, the task poll and the delete | the snapshot's |
| `VMClone` | the source VM's `Provider`; the `Provider` and `VMClass` the target VM will reference. Every reconcile, and re-read from the API server right before the provider `Clone` call and the target VM `Create` | the clone's for the source; the target namespace for the target's references |
| `VMMigration` | the source VM's `Provider`; the target `Provider`; the target `VMClass`. Before every phase except `Ready` and `Failed`, and re-read from the API server right before `ImportDisk` and the target VM `Create` | the migration's; the target namespace too for the target `Provider` and `VMClass` |
| Adoption | never binds an existing adopted-labelled VM whose `VMClass` or `VMImage` is refused. Adopted VMs are created in the `Provider`'s own namespace, so they need no grant | the `Provider`'s |

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
anything. A refused object is also re-checked every 5 minutes as a safety net.
It isn't retried faster, because only whoever can set the selector or the
namespace labels can lift the refusal.

Or point the reference at an object in the referencing namespace, if the VM
isn't bound yet (`spec.providerRef` is immutable once bound, see
[`vm-provider-binding.md`](vm-provider-binding.md)).

### Revoking

Removing a namespace from a selector takes effect on the next check. A bound VM
then fails closed: it keeps its `status.id` and its hypervisor VM, but the
manager stops describing, powering and reconfiguring it until access is
restored. Nothing is deleted or unbound. `VirtualMachine`s that reference an
object in another namespace are re-run when a namespace's labels or an object's
selector change, so a revocation is noticed within seconds.

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
the `Provider`. If the `Provider` no longer exists, the finalizer is removed as
before.

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

## RBAC

Nothing new. The manager already reads and watches `namespaces`
(`get`, `list`, `watch`), added for the clone and migration target grant. The
`VirtualMachine` and `VMSnapshot` controllers now use it too. The manager never
writes `Namespace` objects.

## Upgrade notes

- **Breaking: cross-namespace references need a grant.** A `VirtualMachine`
  that references a `Provider`, `VMClass` or `VMImage` in another namespace
  used to work with no grant. After the upgrade it fails closed with
  `ConsumerNotAllowed`, including VMs that are already running: no provider
  calls, no deletes, until the referenced object selects its namespace.
  **Set `spec.consumerNamespaceSelector` on every shared `Provider`, `VMClass`
  and `VMImage` before you upgrade the manager.** The field is ignored by older
  managers, so you can set it first.
- **Clones and migrations into another namespace** now also need the pinned
  `Provider` and `VMClass` to select the target namespace, in addition to the
  target-namespace annotation.
- **Upgrade the CRDs with (or before) the manager.** The chart's CRD upgrade
  hook does this by default. With `crdUpgrade.enabled: false` or a GitOps flow,
  apply `charts/virtrigaud/crds/` (or `config/crd/bases/`) first. An older CRD
  prunes the field, and every cross-namespace reference is then refused.

List what needs a selector before upgrading:

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
