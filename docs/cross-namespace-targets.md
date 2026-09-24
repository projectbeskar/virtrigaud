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
- **The grant is an administrator's decision.** `Namespace` is cluster-scoped.
  A tenant who can only write objects inside their own namespace can't
  annotate another namespace, and so can't grant themselves access. Keep
  `update`/`patch` on `namespaces` limited to cluster administrators.

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

- An administrator adds the grant. The manager watches that annotation on
  `Namespace` objects and re-runs the affected objects right away.
- The owner changes `spec.target.namespace` to the object's own namespace, or
  empties it.

A refused object is also re-checked every 5 minutes. The manager doesn't retry
it faster, because only an administrator can lift the refusal.

## Revoking a grant

The grant is checked again before every step that touches the target
namespace, not only when the object is first reconciled:

| Object | Checked before |
|---|---|
| `VMClone` | every reconcile: the provider `Clone` call, the task poll and the check for an existing VM; again right before the target VM is created and bound |
| `VMMigration` | `Validating` (before the source is powered off or snapshotted, and before the staging PVC or the export); `Importing` (before `ImportDisk` lands the disk); `Creating` (before the target VM is read or created); `Validating-Target` (before the target VM is annotated); deletion (before the finalizer removes a partly created target VM) |

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
(`get`, `list`, `watch`) so it can read and watch the grant. This applies to
the kubebuilder markers, `config/rbac/role.yaml`, and both chart RBAC templates
(`rbac.scope: cluster` and `namespace`). The manager never writes `Namespace`
objects.

## Upgrade notes

- **Breaking for cross-namespace targets.** A `VMClone` or `VMMigration` whose
  `spec.target.namespace` names another namespace used to work with no grant.
  After the upgrade, it waits with `TargetNamespaceNotAllowed` until an
  administrator annotates the target namespace. This includes objects that
  are in flight during the upgrade. Before upgrading, list the objects
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
