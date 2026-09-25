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

Two controls close this. Both live in the `VirtualMachine` CRD, so both need
the upgraded CRD, which the manager checks at startup and on readiness
([section 3](#3-the-manager-checks-the-installed-crd)).

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
    uid: 3f0c9c1e-...   # the Provider object's UID, for audit
```

It is written by every bind path:

| Bind path | Where |
|---|---|
| `VirtualMachine` create | with `status.id` after `Create` succeeds |
| Clustered create in flight | with `status.placement.pendingHost`, before `Create` |
| `VMClone` target | with the target's `status.id` (the `Provider` the clone ran on) |
| Adoption | with the adopted VM's `status.id` |

While a VM is bound, the operator makes no provider call for it unless
`spec.providerRef` still names that `Provider` (same namespace and name). The
check runs in the `VirtualMachine` controller, in the helper every per-VM call
goes through, for a `VMClone` source (and its clone task and target bind), for
a `VMMigration` source (validation, power-off, snapshot, export and its task
poll, snapshot cleanup), and for a `VMSnapshot` (create, task poll, delete). On
a mismatch:

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

### A re-created `Provider`

A `Provider` deleted and re-created under the same namespace and name is
accepted in its place: the binding is enforced on namespace and name only. The
`VirtualMachine` controller records a `Warning` event with reason
**`BoundProviderRecreated`** (old and new UID), records the new UID in
`status.boundProvider`, and keeps operating through the new object. No action is
needed.

The operator therefore trusts that a `Provider` re-created under the same name
fronts the same hypervisor. Proving that needs a hypervisor identity bound to
each VM (for example a vCenter instance UUID or a Proxmox cluster name), which
is tracked as a follow-up ADR. Until then, treat the right to delete and create
`Provider` objects as the right to redirect every VM bound through them.

### `VMClone` and `VMMigration` targets

- A `VMClone` marks the target `VirtualMachine` it creates with
  `virtrigaud.io/clone-uid: <VMClone UID>`. It binds the cloned id only to a
  target that carries its own marker, has an empty `status.id` (or already the
  cloned id, when a bind is resumed) and references the `Provider` the clone ran
  on. Anything else — a `VirtualMachine` created under the target name by
  someone else, one already bound to another VM, one that references another
  `Provider` — fails the clone with reason `TargetConflict`, leaves that
  VM's binding untouched, and names the cloned VM left on the provider.
- `spec.target.annotations` of a `VMClone` or `VMMigration` are copied to the
  target without keys in the reserved `virtrigaud.io` domain
  (`virtrigaud.io/...` and `*.virtrigaud.io/...`), such as
  `virtrigaud.io/orphan-on-delete`, `virtrigaud.io/force-delete` and the
  provenance annotations. The controller writes its own provenance after the
  user annotations, so it cannot be overridden.
- Adoption binds a pre-existing `virtrigaud.io/adopted` VM only if its
  `spec.providerRef` names the adopting `Provider`, and never adopts a
  hypervisor VM a `VirtualMachine` is bound to through that `Provider`.

### A `Provider` in another namespace

A `spec.providerRef` that names another namespace is used only if that
`Provider`'s `spec.consumerNamespaceSelector` selects the VM's namespace (the
same applies to `spec.classRef` and `spec.imageRef`, checked where they are
used). Otherwise the VM reports `Ready=False` / `ConsumerNotAllowed` and no
provider call is made — for a bound VM too. Like a `ProviderRefMismatch`,
deleting such a VM keeps the finalizer (also when that cross-namespace
`Provider` no longer exists) until access is restored or the VM carries
`virtrigaud.io/orphan-on-delete` or `virtrigaud.io/force-delete`. See
[`cross-namespace-references.md`](cross-namespace-references.md).

## 3. The manager checks the installed CRD

Both controls live in the `VirtualMachine` CRD, which is upgraded separately
from the manager. **With an older CRD neither works.** The admission rule is
absent, so a bound VM can be re-pointed. `status.boundProvider` is not in the
schema, so the API server prunes it on every write: the record never persists,
the backfill re-runs on every reconcile from the still-mutable reference, and
the operator-side check never fires. This happens when the chart's CRD upgrade
hook is disabled (`crdUpgrade.enabled: false`), or when a GitOps tool applies
the CRDs after the manager.

The manager therefore reads the installed `VirtualMachine` CRD at startup and
on every readiness probe (results reused for one minute) and checks that its
`v1beta1` schema has `status.boundProvider` and the `spec.providerRef` rule.
The same check reads the `Provider`, `VMClass` and `VMImage` CRDs and requires
`spec.consumerNamespaceSelector` in each (the cross-namespace consumer grant,
see [`cross-namespace-references.md`](cross-namespace-references.md)): with an
older CRD no grant can be set, so every cross-namespace reference would be
refused. A missing feature in any of the four CRDs is `missing`; an unreadable
one is `unknown`:

| State | Meaning | Readiness |
|---|---|---|
| `verified` | Every feature is present. | Ready |
| `missing` | The CRD is older than the manager, or absent. An error is logged and `virtrigaud_errors_total{reason="vm-crd-security-features-missing"}` counts each check. | **Not ready** until the CRD is upgraded |
| `unknown` | The CRD cannot be read (with `rbac.scope: namespace` the manager has no cluster-scoped read). A warning is logged. | Ready |

The state is exported as
`virtrigaud_manager_vm_crd_security_features{state="verified|missing|unknown"}`
(1 for the current state). The manager needs `get` on those four CRDs
(`resourceNames: [virtualmachines, providers, vmclasses, vmimages
.infra.virtrigaud.io]`), which the chart's ClusterRole grants.

Failing readiness is deliberate. A manager running against an old CRD looks
healthy while the protection is off. A failing readiness check makes that
visible: the pod is not Ready, `helm upgrade --wait` fails, and a rolling
update keeps the previous manager, which has no weaker protection against the
same CRD, until the CRD is upgraded. VM management is not stopped: controllers
keep running under leader election. An unreadable CRD doesn't fail readiness,
because nothing was proven missing.

Each backfill of a pre-existing VM also records a `Normal`
`BoundProviderRecorded` event on the VM. The same VM getting that event on
every reconcile is the per-VM symptom of an old CRD.

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
- Whoever can annotate and delete a `VirtualMachine` can detach it. Restricting
  the annotation to administrators is a tracked follow-up; until then, use a
  policy engine (Kyverno, Gatekeeper) if tenants must not detach their VMs.

This replaces the previous workaround — pointing `spec.providerRef` at a
`Provider` that doesn't exist and then deleting the `VirtualMachine` — which the
CRD rule now rejects for a bound VM.

`virtrigaud.io/force-delete: "true"` is unchanged. It releases the finalizer
when the provider `Delete` keeps failing, or when no delete can be routed (an
unbound clustered VM, a placement/topology mismatch, a `ProviderRefMismatch`,
a cross-namespace `Provider` the VM's namespace may not use
(`ConsumerNotAllowed`)).
It is an escape hatch; to detach a VM on purpose, use `orphan-on-delete`.

## Upgrade notes

- **Breaking: `spec.providerRef` of a bound VM can no longer be changed.** Any
  tooling that edits it after creation (including the re-point-and-delete
  un-adopt workaround) now gets `422 Invalid`. Use `orphan-on-delete` to
  detach, or delete and re-create the `VirtualMachine`. A `kubectl apply` that
  leaves `providerRef` as it is keeps working.
- **Upgrade the CRDs with (or before) the manager.** Both controls need the new
  `VirtualMachine` CRD (see [section 3](#3-the-manager-checks-the-installed-crd)).
  The chart's CRD upgrade hook does this by default. With
  `crdUpgrade.enabled: false` or a GitOps flow, apply
  `charts/virtrigaud/crds/` (or `config/crd/bases/`) first; until then the new
  manager is not Ready. To confirm, check that the manager pod is Ready and
  `virtrigaud_manager_vm_crd_security_features{state="verified"}` is `1`, or
  that
  `kubectl get crd virtualmachines.infra.virtrigaud.io -o jsonpath='{.spec.versions[?(@.name=="v1beta1")].schema.openAPIV3Schema.properties.status.properties.boundProvider.type}'`
  prints `object`.
- **The manager role gains `get` on the `virtualmachines.infra.virtrigaud.io`
  CRD** (chart ClusterRole and `config/rbac`). Apply the updated RBAC with the
  new manager.
- **Trust on first reconcile.** A VM bound before this version has no
  `status.boundProvider`. The `VirtualMachine` controller records it on the
  VM's first reconcile after the upgrade, from its **current**
  `spec.providerRef`, with a `BoundProviderRecorded` event, and enforces it
  from then on. A reference that was re-pointed *before* the upgrade is
  therefore accepted as the binding. Before upgrading in a multi-tenant cluster,
  you can list bound VMs and check that each references the `Provider` whose
  hypervisor holds its id:
  `kubectl get vm -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,ID:.status.id,PROVIDER_NS:.spec.providerRef.namespace,PROVIDER:.spec.providerRef.name`.
  Between the upgrade and that first reconcile, `VMClone`, `VMMigration` and
  `VMSnapshot` treat a VM with no record the same way (they trust its current
  reference).
- **Clones in flight.** A `VMClone` whose target `VirtualMachine` was created by
  an older manager (no `virtrigaud.io/clone-uid` marker) but not yet bound fails
  with `TargetConflict` after the upgrade. The cloned VM is left on the
  provider; adopt it, or delete it and the target, and clone again.
- The field is additive and optional; older managers ignore it.
