# Cross-namespace clone and migration targets

A `VMClone` or `VMMigration` creates a `VirtualMachine`, by default in its own
namespace. `spec.target.namespace` can name a different one. The manager has
cluster-wide RBAC, so without a check, anyone allowed to create a `VMClone` or
`VMMigration` in namespace `team-a` could make the manager create a
`VirtualMachine` in `team-b`, with labels and annotations they chose. A
migration could also put a disk named for a `team-b` VM on the hypervisor
(libvirt: `team-b.<name>-migrated.qcow2`), and that VM could later attach the
disk in place.

**A target in another namespace is refused unless that namespace grants it.**

## The rule

- **Own namespace: no change.** An empty `spec.target.namespace`, or one equal
  to the object's own namespace, works as before. No grant is needed and the
  manager reads no `Namespace` object.
- **Another namespace: allowed only with a grant.** The target `Namespace`
  must have the annotation `infra.virtrigaud.io/allowed-source-namespaces`,
  and the annotation's value must list the source namespace:

  ```yaml
  apiVersion: v1
  kind: Namespace
  metadata:
    name: team-b
    annotations:
      # VMClones / VMMigrations in team-a and team-c may create VMs here.
      infra.virtrigaud.io/allowed-source-namespaces: "team-a, team-c"
  ```

  ```sh
  kubectl annotate namespace team-b \
    infra.virtrigaud.io/allowed-source-namespaces=team-a
  ```

  The value is a comma-separated list of exact namespace names. Spaces around
  an entry are ignored. Nothing else is interpreted: `*`, `team-*`, a prefix
  such as `team`, and a different case all fail to match.
- **Whoever can update the target `Namespace` can grant.** `Namespace` is
  cluster-scoped, so on a plain cluster that's a cluster administrator: a
  tenant who can only write objects inside their own namespace can't annotate
  another namespace, and so can't grant themselves access. Keep
  `update`/`patch` on `namespaces` limited to the people who should decide.
- **Self-service platforms can change who that is.** Tools like Capsule, HNC
  and Rancher projects may let a namespace owner update their own `Namespace`,
  including its annotations. There, a tenant can open its own namespace to
  other namespaces. Where that matters, block or restrict
  `infra.virtrigaud.io/allowed-source-namespaces` with a tenant policy, for
  example a Kyverno or Gatekeeper rule that only lets platform administrators
  set it.
- **Grants match namespace names, not identities.** If a namespace listed in a
  grant is deleted and a new namespace is later created with the same name,
  the new namespace inherits the grant. Remove a deleted namespace's name from
  every `allowed-source-namespaces` value, and review the grants before you
  reuse a namespace name.

## When a target is refused

The manager does nothing in the target namespace. It doesn't create, read,
bind, annotate or delete anything there, and it makes no provider call for a
clone target or a migration import there. The object reports:

- `Ready=False`, reason `TargetNamespaceNotAllowed`, with a message naming the
  two namespaces and the annotation to set. A `VMMigration` refused while
  validating also sets `Validating=False` with the same reason.
- One `Warning` event with reason `TargetNamespaceNotAllowed`, when the
  refusal starts.
- `status.observedGeneration` set to the current generation.

The refusal isn't a failure. The object isn't moved to `Failed`, and a
migration's retry count doesn't change. The message is the same whether the
target namespace is missing or just has no grant, so a refusal doesn't tell a
tenant whether the namespace exists.

### Recovering

Either of these lets the object continue from where it stopped:

- Someone who can update the target `Namespace` adds the grant. The manager
  watches that annotation on `Namespace` objects and re-runs the affected
  objects right away.
- The owner changes `spec.target.namespace` to the object's own namespace, or
  empties it.

A refused object is also re-checked every 5 minutes. The manager doesn't retry
it faster, because only someone who can update the target `Namespace` can lift
the refusal.

## Revoking a grant

The grant is checked again before every step that touches the target
namespace, not only when the object is first reconciled:

| Object | Checked before |
|---|---|
| `VMClone` | every reconcile: the provider `Clone` call, the task poll and the check for an existing VM; again right before the target VM is created and bound |
| `VMMigration` | `Validating` (before the source is powered off or snapshotted, and before the staging PVC or the export); `Importing` (before `ImportDisk` lands the disk); `Creating` (before the target VM is read or created); `Validating-Target` (before the target VM is annotated); deletion (before the finalizer removes a partly created target VM) |

These checks read the manager's cache, which can briefly lag a change. So the
steps that create something in the target namespace re-read the grant straight
from the API server just before they run: the provider `Clone` call, the
migration's `ImportDisk` call, and the `Create` of the target VM for a clone or
a migration. If the API server shows the grant revoked, the step isn't taken,
even if the cache still shows the grant.

If a grant is removed while a clone or migration is running, the next of these
steps is refused and the object waits as described above. Nothing that already
exists is changed:

- A `Ready` clone or migration stays `Ready`, and its VM stays.
- An in-flight clone keeps its provider task and target ID. If the grant comes
  back, it binds the VM it already cloned. It doesn't clone again.
- A `VMMigration` deleted while its target namespace doesn't grant it leaves
  any partly created target VM there for that namespace's owners.

## References in the created VM

When a target in another namespace is allowed, the created `VirtualMachine`
names the objects the clone or migration actually used. A reference without a
namespace would resolve in the target namespace instead:

- **VMClone:** `spec.providerRef` and `spec.classRef` get the clone's namespace
  when they had none. The source VM, and so the Provider it runs on, lives in
  the clone's namespace.
- **VMMigration:** `spec.providerRef` gets the migration's namespace when
  `spec.target.providerRef.namespace` is empty. `spec.classRef` already did.

For a target in the object's own namespace, these references stay as they
were.

**The pinned `Provider` and `VMClass` must also share themselves with the
target namespace.** They live in the source namespace, so the target VM
references them from another namespace, which needs their
`spec.consumerNamespaceSelector` to select the target namespace. The clone or
migration checks this before its provider call and before it creates the target
VM, and waits with `Ready=False` / `ConsumerNotAllowed` otherwise. See
[`cross-namespace-references.md`](cross-namespace-references.md).

### A granted migration's disk is not attached in place

A migration's landing disk is attached in place only when the manager can
prove it belongs to that VM: the VM's `spec.importedDisk.migrationRef` has to
name a `VMMigration` in the **VM's own namespace**. A migration into another
namespace lives in the source namespace, so the lookup finds nothing, and the
target VM's imported disk is handled as a base image. On libvirt it's confined,
then copied into the VM's own disk or refused; on vSphere, whose base images
are templates, the create is refused (`ValidationError`). It's never attached
in place, so this fails closed. For a granted cross-namespace migration, expect
the disk to be copied (libvirt), or the target VM's create to be refused (vSphere).
A migration into another namespace through a **Proxmox** target Provider is
refused up front: it fails at `Validating` (`Phase=Failed`, `Validating=False`,
reason `ProxmoxCrossNamespaceTarget`) before anything is exported or imported,
because its target VM's create would be refused. So is a migration into
another namespace through a **clustered** (`topology: cluster`) target Provider
(reason `ClusteredTargetUnsupported`): its target VM is created first and waits
for its import, which it could never prove from another namespace
([Migration into a clustered provider](clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)).
A migration into its own namespace is unaffected. See
[`image-preparation.md`](image-preparation.md) for the base-image rules.

### No libvirt linked clone into another namespace

A grant allows a **copy** of the source VM into the target namespace. A libvirt
linked clone is not a copy: its disk is an overlay that keeps reading the
source VM's disk for as long as the clone exists. A `VMClone` with
`options.type: LinkedClone` on a libvirt Provider whose target is in another
namespace is therefore refused before any provider call (`Failed`,
`Ready=False` / `LinkedCloneCrossNamespace`); use a full clone. vSphere linked
clones are not affected.

### A clone carries the source's cloud-init seed

A clone is a copy of the source VM's disk and definition. On libvirt (both
topologies) the clone gets a cloud-init seed of its own. It carries a new
identity, and the source's user-data unless the `VMClone` gives its own
(single-host clones made before R26 still reference the source's seed). On
vSphere the clone keeps the source's `guestinfo.userdata`. Either way the
target VM receives the source's user-data — any provisioning secrets in it
(passwords, keys, tokens) included. Grant a namespace only if its users may see
the source VMs' user-data, or keep secrets out of user-data.

**Creating a `VMClone` is read access to the source VM's data.** The clone's
disk is a full copy of the source's disk. On libvirt, the `VMClone`'s
`spec.customization.userData.cloudInit.inline` runs as **root** on the clone's
first boot, so whoever can create the `VMClone` can read everything on that
copy. This holds within one namespace as much as across namespaces. Grant
`create` on `vmclones` only to subjects trusted with read access to the source
VMs' data. Grant a target namespace only if the subjects there may have that
access too. See
[VM ownership: a clone's host identity](vm-ownership.md#a-clones-host-identity).

## The target VM must be the migration's own

The target's name (`spec.target.name`) is chosen by the migration's owner, but
in a granted namespace other people can create `VirtualMachine`s too. One of
them could create a VM under that name first and copy every field that looks
like provenance onto it: the `virtrigaud.io/migration` annotation, and a
`spec.importedDisk` that names the migration and its `status.importID`. Before
this release, a migration took any VM with the target's name as its own. It
then reported that VM's status as the migration's result, marked it
`migration-completed`, and could delete the source after it.

**A migration now acts only on the `VirtualMachine` it created:**

- When its own `Create` of the target VM succeeds, it records the UID the API
  server returned in `status.targetVMUID`. From then on, only the VM with
  that UID is its target: waiting for it, validating it, marking it completed,
  and deleting a partly created one when the migration is deleted (with a UID
  precondition).
- Just before the `Create`, it writes a **claim**: a fresh random token goes
  into the VM's `virtrigaud.io/migration-claim` annotation, and only its
  SHA-256 digest into `status.targetVMClaim`. The digest is stored first, and
  no `Create` is sent unless that write succeeds. If the `Create` succeeded
  but the UID was never recorded (a lost status write, a manager restart),
  the next reconcile finds the VM whose token matches the digest and records
  its UID. Reading the `VMMigration` gives you only the digest, which can't
  be turned back into the token.
- **Any other `VirtualMachine` under the target's name fails the
  migration**: `Phase=Failed`, `Ready=False` and `Failed=True` with reason
  `TargetVMNotOwned`, and a fixed message that names nothing about the other
  VM. That VM is never read into the migration's status, annotated or
  deleted. The check runs at `Validating`, before the source is powered off,
  snapshotted or exported, so a name that's already taken costs nothing.
  `Creating` and `Validating-Target` check again against the API server, in
  case the VM was created during the export. With a retry policy, the
  migration retries and fails the same way until the name is free.
- A migration whose own target VM disappears (or is being deleted) before it
  completes fails with reason `TargetVMGone`. It isn't re-created under a new
  identity, because the landing disk most likely went with it.
  `Validating-Target`, the step that commits the migration, reads the VM from
  the API server rather than the cache. A retry releases the binding to a VM
  that no longer exists, so the retried migration creates and binds a new
  one. A bound VM that still exists keeps its binding and is reused.
- If the target name is taken at the moment of the `Create` by a VM that
  carries another migration claim, the migration checks again instead of
  failing at once. With the reserved-keys admission policy on, only a manager
  can write a claim, so this is most likely an overlapping leader.
- Reserved labels in `spec.target.labels` (`virtrigaud.io/adopted`) aren't
  copied onto the target VM. A Normal `TargetLabelsDropped` event lists them.
- The landing disk follows the same rule. It's attached in place only for the
  VM the migration created (its UID or its claim). A same-named VM with a
  copied `spec.importedDisk` gets it handled as a base image, like any other
  unverified path.

To fix a `TargetVMNotOwned` failure, delete or rename the other
`VirtualMachine`, or pick another `spec.target.name`, then run the migration
again.

The chart's [reserved-keys admission policy](admission-policy.md) also stops
anyone but the manager from setting `virtrigaud.io/migration`,
`virtrigaud.io/migration-claim` and the other provenance keys on a
`VirtualMachine`.

## Source references

Every source reference is local to the object's namespace.
`VMClone.spec.source.vmRef` and `VMMigration.spec.source.vmRef` are
`LocalObjectReference`s. The other `VMClone` source kinds (`snapshotRef`,
`templateRef` and `imageRef`) are rejected as unsupported. A clone or
migration can't read a VM in another namespace.

`VMMigration.spec.source.providerRef` can name a Provider in any namespace.
**It must now name the Provider the source VM runs on**, which is the VM's own
`spec.providerRef`, where an empty namespace means the VM's namespace. The
migration exports the source VM through that Provider by the VM's provider ID.
A different Provider would export whatever unrelated VM has that ID on its
hypervisor, and that VM could belong to another tenant. A mismatch fails the
migration with a message naming both Providers. If you leave the field empty,
the migration uses the source VM's Provider, as before.

## RBAC

The manager's role gains read-only access to `namespaces`
(`get`, `list`, `watch`) so it can read and watch the grant, and re-read it
from the API server before a create step. This applies to
the kubebuilder markers, `config/rbac/role.yaml`, and both chart RBAC templates
(`rbac.scope: cluster` and `namespace`). The manager never writes `Namespace`
objects.

## Upgrade notes

- **Breaking for cross-namespace targets.** A `VMClone` or `VMMigration` whose
  `spec.target.namespace` names another namespace used to work with no grant.
  After the upgrade, it waits with `TargetNamespaceNotAllowed` until someone
  who can update the target `Namespace` annotates it. This includes objects
  that are in flight during the upgrade. Before upgrading, list the objects
  affected:

  ```sh
  kubectl get vmclones,vmmigrations -A -o json | jq -r '
    .items[] | select(.spec.target.namespace != null
      and .spec.target.namespace != "" and .spec.target.namespace != .metadata.namespace)
    | "\(.kind) \(.metadata.namespace)/\(.metadata.name) -> \(.spec.target.namespace)"'
  ```

  Then annotate each target namespace you want to keep allowing.
- **Breaking for a mismatched `spec.source.providerRef`.** A `VMMigration`
  whose `spec.source.providerRef` names a Provider other than the source VM's
  now fails. To fix it, remove the field or set it to the source VM's
  Provider.
- The same-namespace behavior is unchanged, and so is every object that
  doesn't set these fields.
- **A later release binds a migration to the target VM it created**
  ([above](#the-target-vm-must-be-the-migrations-own)), in every namespace. A
  migration that's in `Creating` or `Validating-Target` when the manager is
  upgraded created its target VM before that binding existed, so it has no
  recorded UID or claim. It fails with `TargetVMNotOwned`, and its target VM
  is left as it is. Let migrations finish, or don't start new ones, before
  upgrading. For a migration that fails this way, check the target VM, then
  delete the source yourself if you meant to (`deleteAfterMigration` doesn't
  run for a failed migration).
- **A later release also requires the consumer grant** on the `Provider` and
  `VMClass` a cross-namespace target references. See the upgrade notes in
  [`cross-namespace-references.md`](cross-namespace-references.md#upgrade-notes).
