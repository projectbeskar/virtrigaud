# ADR-0010: Provider in-use finalizer

## Status

**Accepted (maintainer decision, 2026-10-01).** Option A below was chosen by the
maintainer before this record was written; this ADR records the decision, the design
as implemented, and the limits Kubernetes puts on it.

**Author**: William Rizzo ([@wrkode](https://github.com/wrkode))

**Related**:
- [ADR-0007](./0007-clustered-orchestrator-provider.md) Addendum A, A1: the Host
  in-use finalizer (`host.infra.virtrigaud.io/in-use`). This ADR applies the same
  pattern one level up, to the `Provider`.
- [`docs/vm-provider-binding.md`](../vm-provider-binding.md#deleting-a-vm-while-its-provider-cannot-be-reached):
  a `VirtualMachine` delete is held while its `Provider` exists but cannot be reached
  (#370), and released when the `Provider` no longer exists in the VM's own namespace.
- [`docs/cross-namespace-references.md`](../cross-namespace-references.md): a
  `Provider` can serve `VirtualMachine`s in other namespaces
  (`spec.consumerNamespaceSelector`).

## Context

A `Provider` had no finalizer. The Provider controller's deletion handler only removed
the provider runtime it manages (the `Deployment`, `Service` and `ServiceAccount`);
the API server then removed the `Provider` at once.

The `VirtualMachine` finalizer deletes the hypervisor VM through the VM's `Provider`.
When that `Provider` no longer exists **in the VM's own namespace**, there is nothing
to delete through, and the finalizer is released without any provider `Delete`. This
is deliberate: without it, a VM whose `Provider` is gone could never be deleted.

Together, these two facts mean:

- **Deleting a `Provider` while VMs still use it** releases every one of those VMs on
  its next delete, with the hypervisor VM still running and no longer managed by
  anything.
- **Deleting a namespace** that holds a `Provider` and its VMs deletes both at the
  same time. Whichever VM delete runs after the `Provider` is gone takes the same
  release path, so most hypervisor VMs in the namespace are left running.
- **VMs in other namespaces** that use the `Provider` (cross-namespace consumers) are
  not released: a missing `Provider` in another namespace is refused like a
  `Provider` that does not grant the namespace, so their deletion is held until an
  administrator sets `orphan-on-delete` or `force-delete`. Their hypervisor VMs are
  stranded either way.

The orphaned VMs keep running and keep consuming hypervisor capacity, and nothing in
the cluster records that they exist. In the regulated environments VirtRigaud targets,
an unmanaged running VM is an audit finding.

## Decision

### D1: The Provider carries an in-use finalizer

The Provider controller keeps the finalizer `provider.infra.virtrigaud.io/in-use`
(`ProviderInUseFinalizer`) on every `Provider`. It is added on the first reconcile of a
`Provider` that is not being deleted, including every `Provider` that existed before
the upgrade.

### D2: What holds a Provider

A `Provider` being deleted is **held** while anything still needs it. The rules follow
the delete paths of each kind:

| Object | Holds the Provider while |
|---|---|
| `VirtualMachine` | its placement Provider (`status.boundProvider`, else `spec.providerRef`, an empty namespace meaning the VM's own) is this `Provider`, in **any** namespace, and it is not being deleted or still carries the `VirtualMachine` finalizer. A VM being deleted whose finalizer is gone (only another controller's finalizer keeps the object) does not hold it. This is the same key the Host in-use finalizer uses. |
| `VMSnapshot` | it carries its finalizer and its `VirtualMachine` is one of the VMs above (its delete calls `SnapshotDelete` through that VM's Provider). |
| `VMClone` | it carries its finalizer, is not being deleted and has not finished (phase not `Ready` or `Failed`), and its source VM is one of the VMs above. A clone's delete makes no provider call. |
| `VMMigration` | it carries its finalizer, and is in flight (phase not `Ready` or `Failed`) or being deleted (its cleanup can delete the source snapshot), and either its source VM is one of the VMs above or its `spec.target.providerRef` is this `Provider`. |

### D3: The runtime stays up while the Provider is held

While a `Provider` is held, the controller keeps reconciling its runtime exactly as for
a live `Provider` (Service, ServiceAccount, clustered host inventory, Deployment, and
`status.runtime`). Its runtime phase stays `Running` while its pods are ready, so the
resolver keeps serving it and the held objects can still be deleted through it. The
self-reported capabilities are not re-fetched and `status.lastHealthCheck` is not
refreshed while it is held, so a hold that has not changed writes nothing.

The Provider shows `DeleteBlocked=True` with reason `ProviderInUse`. The message gives
the number of each kind of object and the namespaces they are in, with a count per
namespace (at most 10 namespaces, then "and N more"). It names no object: the
`Provider` belongs to its administrator, but the objects may belong to tenants. A
`Warning` event `ProviderDeleteBlocked` with the same message is recorded when the hold
begins or its reason changes, not on every re-check.

The runtime is torn down, and the finalizer removed, only when nothing holds the
`Provider` any more. VirtualMachines in the `Provider`'s own namespace are not
special: a cross-namespace consumer holds it the same way.

### D4: Re-checks are event-driven

The Provider controller watches `VirtualMachine`, `VMSnapshot`, `VMClone` and
`VMMigration`. An event re-reconciles a `Provider` only if that `Provider` is being
deleted, and only for a delete or an update that can change whether the object holds
it (its finalizers, its deletion, its placement Provider, its source VM, its phase).
The lookups use cache field indexes: the existing `virtrigaud.io/placement-provider`
index for VMs, and a new `virtrigaud.io/provider-in-use` index on `VMSnapshot`,
`VMClone` and `VMMigration` (by the VM they act on, and for a migration by its target
Provider). Nothing lists every object in the cluster. A held `Provider` is also
re-checked every 2 minutes as a backstop.

### D5: Escape hatch: `virtrigaud.io/force-delete`

An administrator can release a held `Provider` at once by annotating it
`virtrigaud.io/force-delete: "true"`, the same annotation that releases a held
`VirtualMachine` or `VMSnapshot` delete. The controller tears the runtime down,
removes the finalizer and records a `Warning` event `ProviderForceDeleted` with the
counts and namespaces it released the `Provider` under. The objects are not touched:

- VMs in the `Provider`'s own namespace then take the existing "Provider not found in
  the VM's own namespace" path on their next delete and are released without a
  provider call. Their hypervisor VMs are left running, unmanaged.
- VMs in other namespaces stay held (a missing cross-namespace `Provider` is refused)
  until each is annotated `orphan-on-delete` or `force-delete`.

This is a deliberate, audited choice to leave hypervisor VMs behind, which is why it
is an explicit annotation and an event, not a timeout.

### D6: Namespace deletion, and the limit Kubernetes puts on D3

When a namespace is deleted, the namespace controller deletes every object in it.
That includes the provider runtime's `Deployment`, `ReplicaSet` and `Pod`s, which it
deletes directly. The `Provider`'s finalizer cannot stop that, and on clusters with
ordered namespace deletion the Pods go first. Nothing can be re-created in a namespace
that is being deleted.

So, in a namespace deletion:

1. The VMs, the `Provider` and the runtime are all deleted at the same moment. The
   `Provider` is held by the VMs (D2), so its VMs no longer find it missing and are
   not silently released.
2. VM deletes that reach the provider while its pod is still terminating (the provider
   container serves through a 15 s `preStop` and a 30 s grace period) succeed, and
   those VMs go.
3. A VM delete that comes later finds no usable provider client and is **held**
   (`ProviderUnavailable`, #370), not orphaned.
4. The `Provider` shows `DeleteBlocked=True` with reason `ProviderInUseRuntimeNotKept`.
   The controller makes no runtime writes in a namespace being deleted (they would be
   refused), and leaves `status.runtime` as it was, so VM deletes keep trying the
   provider for as long as its pod answers.
5. If VMs remain, the namespace stays `Terminating` until an administrator acts: set
   `virtrigaud.io/force-delete: "true"` on the `Provider` (D5) or on each remaining VM.
   Either way, the hypervisor VMs left are then unmanaged, and the administrator has
   chosen that.

The runbook is to delete the VMs, and wait for them to go, before deleting the
namespace. A `Provider` deleted with foreground cascading deletion
(`kubectl delete --cascade=foreground`) has the same limit, because the garbage
collector deletes its runtime before the `Provider`. The controller detects the
`foregroundDeletion` finalizer, reports the same reason, and does not re-create the
runtime against the garbage collector. Use the default (background) cascade.

VMs in **other** namespaces are not deleted with the namespace, so a `Provider` with
cross-namespace consumers waits for them (D2). The message says how many there are and
in which namespaces. The administrator deletes them, detaches them
(`orphan-on-delete`, which needs the Provider's
`infra.virtrigaud.io/allow-consumer-orphan-on-delete` permission for a clustered
`Provider`), or force-deletes the `Provider`.

### D7: Webhook: a metadata-only update is never refused

The Provider validating webhook re-checked the topology×type rule on every update. A
`Provider` that predates that rule and breaks it could then never get or lose a
finalizer, so it could never be deleted. An update that leaves `spec` unchanged
(metadata or status only) is now always admitted. The `spec.type` and `spec.topology`
immutability checks are unchanged.

### D8: Unchanged

- The VM delete path's "Provider not found in the VM's own namespace releases" branch
  stays. With the finalizer it is reached only after a `Provider` is force-deleted, or
  for a `Provider` deleted before the upgrade.
- A `Provider` being deleted without the in-use finalizer (deleted before the upgrade,
  kept by someone else's finalizer) is handled as before: its runtime is torn down.
- RBAC: the manager already has `update` on `providers` and `providers/finalizers`, and
  `get`/`list`/`watch` on the watched kinds and on namespaces. The kubebuilder markers
  on the Provider controller now state that, and the generated role does not change.

## Alternatives considered

**B: a validating webhook refusing the deletion of an in-use Provider.** Rejected. A
webhook sees only the `DELETE` of the `Provider` itself. It does not stop a namespace
deletion, which deletes the `Provider` with its VMs and is the most common way this
goes wrong. It also fails open whenever the webhook is not reachable or not installed,
and refusing a `DELETE` gives the administrator no visible "waiting for" state on the
object.

**C: document only ("delete VMs before their Provider").** Rejected. The failure is
silent, its cost (running, unmanaged VMs) is high, and the namespace case is easy to
hit by accident.

**D: run provider runtimes outside the tenant namespace** (for example in the
manager's namespace), so that deleting a namespace does not delete its runtime. This
is the only way to make D3 hold during a namespace deletion. It is a larger change
(credentials and TLS Secrets live in the tenant namespace, and so do the NetworkPolicy
and owner-reference assumptions), so it is left for a follow-up ADR. This ADR does not
preclude it.

**E: release automatically after a timeout.** Rejected. It turns "held" back into
"silently orphaned", only later.

## Consequences

### Positive

- Deleting a `Provider` no longer orphans the hypervisor VMs of the VMs that use it.
  The `Provider` waits, with its runtime up, until they are deleted through it.
- Cross-namespace consumers are protected the same way, and the administrator can see
  how many there are and in which namespaces.
- Every way to leave hypervisor VMs behind is an explicit annotation with a `Warning`
  event.

### Negative / trade-offs

- **Behaviour change:** deleting an in-use `Provider` now waits. Automation that
  deletes a `Provider` and waits for it to disappear needs to delete its VMs first, or
  a longer timeout.
- **Namespace deletion can now wait** (D6). A namespace whose VMs could not all be
  deleted while the provider pod was terminating stays `Terminating` until an
  administrator acts. The alternative was VMs orphaned without anyone being told.
- **Uninstalling the manager before deleting the Providers** leaves `Provider`s that
  cannot finish deleting, as for every other VirtRigaud finalizer: delete them (and
  their VMs) first, or remove the finalizer by hand.
- A VM created against a `Provider` that is already being deleted keeps it held.
  Refusing new VMs on a `Provider` being deleted is a follow-up for the VirtualMachine
  controller.

## Migration and upgrade

- No CRD schema change and no proto change. `ProviderInUseFinalizer` is a Go constant.
- The finalizer is added to every existing `Provider` on its first reconcile after the
  manager is rolled; no operator action. A `Provider` that cannot take it (an update
  refused by an older webhook) shows up as reconcile errors in the manager log.
- **Rollback:** a manager without this change ignores the finalizer, so a `Provider`
  deleted after a rollback stays `Terminating`. Remove it by hand:
  `kubectl patch provider <name> -n <ns> --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]'`
  (check that no VMs still use it first).

## Implementation

- `internal/controller/provider_inuse.go`: the hold, the release, the escape hatch,
  the index and the watches.
- `internal/controller/provider_controller.go`: the finalizer, and the split of the
  reconcile into a runtime part shared by live and held Providers.
- `api/infra.virtrigaud.io/v1beta1/provider_types.go`: `ProviderInUseFinalizer`.
- `api/infra.virtrigaud.io/v1beta1/provider_webhook.go`: D7.
