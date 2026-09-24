# VM provider binding: `spec.providerRef` is locked once a VM is bound

A `VirtualMachine`'s `status.id` is the hypervisor's identifier for the VM: a
Proxmox VMID, a vSphere managed object ID (MOID), a libvirt domain name. It is
meaningful only on the hypervisor of the `Provider` that assigned it. These
identifiers repeat across hypervisors: VMID `100` exists on most Proxmox
clusters, `vm-42` on many vCenters.

Every per-VM operation — `Describe`, `Power`, `Reconfigure`, `Delete`,
snapshots, clones and migration exports — addresses the VM by `status.id`
through the `Provider` that `spec.providerRef` resolves to. Before this change
`spec.providerRef` could be edited after the VM was bound. Anyone allowed to
edit a `VirtualMachine` could point it at another `Provider`, and the operator
would then describe, power, reconfigure, snapshot, export — or, on deletion,
destroy — whatever VM holds that id on the other hypervisor, with the other
`Provider`'s credentials. That bypassed the rule that a `VMMigration` exports
only through its source VM's own `Provider` (see
[`cross-namespace-targets.md`](cross-namespace-targets.md)). The owner stamps
described in [`vm-ownership.md`](vm-ownership.md) protect `Create` and the
clustered libvirt paths, not these by-id operations on single-host libvirt,
vSphere and Proxmox.

Two controls close this. Each works on its own.

## 1. The CRD locks `spec.providerRef` (admission)

The `VirtualMachine` CRD carries a CEL transition rule on its root. It is
enforced by the Kubernetes API server itself, so it needs no webhook:

```cel
!has(oldSelf.status)
  || ((!has(oldSelf.status.id) || size(oldSelf.status.id) == 0)
      && (!has(oldSelf.status.placement) || !has(oldSelf.status.placement.pendingHost)
          || size(oldSelf.status.placement.pendingHost) == 0))
  || (has(self.spec) && has(oldSelf.spec) && self.spec.providerRef == oldSelf.spec.providerRef)
```

- **A VM is bound** when the stored object has a `status.id`, or a clustered
  create in flight (`status.placement.pendingHost`). From then on any change to
  `spec.providerRef` is rejected with `422 Invalid`:
  `spec.providerRef is immutable once the VirtualMachine is bound ...`.
- **Before binding it stays editable.** You can fix a typo after a failed
  create, while `status.id` is still empty.
- **The reference is compared as written.** An unset `namespace` equals only an
  unset `namespace`. Changing an unset namespace to the VM's own namespace
  spelled out is therefore also rejected, although it names the same `Provider`
  (the rule cannot read `metadata.namespace`).
- **The rule reads the stored status.** With the status subresource, an update
  of the main resource is validated against the stored `status`, so a request
  body that clears `status.id` doesn't unlock the reference. A status update
  ignores the `spec` in its body, so it can't change `spec.providerRef` either.
- **Every other update is unaffected**: other spec fields, labels,
  annotations, and the operator's status writes.

Only a writer of the `virtualmachines/status` subresource (the operator, or an
administrator) can unbind a VM. Keep that permission away from tenants: it
controls `status.id`, which decides what the operator acts on.

## 2. The operator checks the binding (defense in depth)

When the operator binds a VM to a hypervisor VM, it records the `Provider` it
bound through, in the **same status write** as the binding:

```yaml
status:
  id: "100"
  boundProvider:
    namespace: infra
    name: proxmox-prod
    uid: 3f0c9c1e-...   # the Provider object's UID at bind time
```

It is written by every bind path:

| Bind path | Where |
|---|---|
| `VirtualMachine` create | with `status.id` after `Create` succeeds |
| Clustered create in flight | with `status.placement.pendingHost`, before `Create` |
| `VMClone` target | with the target's `status.id` (the `Provider` the clone ran on) |
| Adoption | with the adopted VM's `status.id` |

While a VM is bound, the operator makes no provider call for it unless
`spec.providerRef` still resolves to that `Provider`: same namespace and name
and, when recorded, the same object UID. The check runs in the
`VirtualMachine` controller, in the helper every per-VM call goes through, for
a `VMClone` source (and its clone task and target bind), for a `VMMigration`
source (power-off, snapshot, export, snapshot cleanup), and for a `VMSnapshot`
(create, task poll, delete). On a mismatch:

- The `VirtualMachine` reports `Ready=False` with reason
  **`ProviderRefMismatch`**, with `observedGeneration` set, and a `Warning`
  event with the same reason. No `Provider` client is resolved and no RPC is
  sent. It is re-checked every **2 minutes**.
- `VMClone`, `VMMigration` and `VMSnapshot` record the same reason on their own
  conditions and make no call for the VM. A `VMMigration` whose source
  reference points at another `Provider` fails with a message naming the bound
  one.
- The metric `virtrigaud_errors_total{reason="provider-ref-mismatch"}` counts
  the `VirtualMachine` controller's refusals (a refused delete counts under
  `provider-delete`).
- **Deletion never goes through the mismatched `Provider`.** The finalizer is
  kept, with the same condition and an event. It is released only by
  `virtrigaud.io/orphan-on-delete: "true"` (below) or
  `virtrigaud.io/force-delete: "true"`. Neither calls a provider. A `Provider`
  that is simply **gone** — the VM still references the `Provider` it is bound
  through, but that object no longer exists — releases the finalizer as before,
  because there is nothing to delete through.

A `VMClone` also refuses to bind its cloned id to a target `VirtualMachine`
that references a `Provider` other than the one the clone ran on (for example
one created under the target name while the clone ran), and adoption only
binds a pre-existing `virtrigaud.io/adopted` VM whose `spec.providerRef` names
the adopting `Provider`.

### A re-created `Provider`

A `Provider` deleted and re-created under the same name has a new UID. Its VMs
then report `ProviderRefMismatch` ("deleted and re-created"), because the new
object may point at a different hypervisor. If it manages the same hypervisor,
an administrator re-accepts it by clearing the record through the status
subresource; the operator records the current `Provider` again on its next
reconcile (within 2 minutes):

```sh
kubectl patch virtualmachine <vm> -n <ns> --subresource=status --type=merge \
  -p '{"status":{"boundProvider":null}}'
```

## Detaching a VM without deleting it: `virtrigaud.io/orphan-on-delete`

To stop managing a VM but leave the hypervisor VM running (un-adopt), annotate
the `VirtualMachine` and delete it:

```sh
kubectl annotate virtualmachine <vm> -n <ns> virtrigaud.io/orphan-on-delete=true
kubectl delete virtualmachine <vm> -n <ns>
```

- The finalizer is removed **without** resolving or calling any `Provider`.
  The hypervisor VM, its disks and any clustered owner stamp are left
  untouched. The operator logs it and records a `Normal` event with reason
  `Orphaned`, naming the id and `Provider` it left behind.
- Only the value `"true"` counts.
- The annotation is read when the deletion is processed, so you can also add it
  to a `VirtualMachine` that is already being deleted (for example one kept by a
  `ProviderRefMismatch`).
- A detached VM stamped with the deleted `VirtualMachine`'s UID can be adopted
  again later: adoption ignores stamps of `VirtualMachine`s that no longer
  exist.

This replaces the previous workaround — pointing `spec.providerRef` at a
`Provider` that doesn't exist and then deleting the `VirtualMachine` — which the
CRD rule now rejects for a bound VM.

`virtrigaud.io/force-delete: "true"` is unchanged. It releases the finalizer
when the provider `Delete` keeps failing, or when no delete can be routed (an
unbound clustered VM, a placement/topology mismatch, a `ProviderRefMismatch`).
It is an escape hatch; to detach a VM on purpose, use `orphan-on-delete`.

## Upgrade notes

- **Breaking: `spec.providerRef` of a bound VM can no longer be changed.** Any
  tooling that edits it after creation (including the re-point-and-delete
  un-adopt workaround) now gets `422 Invalid`. Use `orphan-on-delete` to
  detach, or delete and re-create the `VirtualMachine`. A `kubectl apply` that
  leaves `providerRef` as it is keeps working.
- **The CRD must be upgraded for the admission rule to apply.** It ships in
  the chart's CRDs; confirm the upgrade replaced the `VirtualMachine` CRD
  (`kubectl get crd virtualmachines.infra.virtrigaud.io -o yaml | grep -A3 x-kubernetes-validations`).
  The operator-side check works either way.
- **Trust on first reconcile.** A VM bound before this version has no
  `status.boundProvider`. The `VirtualMachine` controller records it on the
  VM's first reconcile after the upgrade, from its **current**
  `spec.providerRef`, and enforces it from then on. A reference that was
  re-pointed *before* the upgrade is therefore accepted as the binding. Before
  upgrading in a multi-tenant cluster, you can list bound VMs and check that
  each references the `Provider` whose hypervisor holds its id:
  `kubectl get vm -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,ID:.status.id,PROVIDER_NS:.spec.providerRef.namespace,PROVIDER:.spec.providerRef.name`.
  Between the upgrade and that first reconcile, `VMClone`, `VMMigration` and
  `VMSnapshot` treat a VM with no record the same way (they trust its current
  reference).
- The field is additive and optional; older managers ignore it.
