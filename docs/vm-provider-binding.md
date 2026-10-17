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

The same holds for `vmmigrations/status`: the disk-size limit trusts
`status.diskInfo.sourceSize` (the exported disk's size, recorded by the
manager) when it checks a migration before its import, so write access to
`vmmigrations/status` must stay with the manager — never grant it to tenants.
(The target provider also enforces the limit on the disk it lands, so a forged
record cannot land a larger disk through an up-to-date provider.)

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
  because there is nothing to delete through. A `Provider` that exists but
  cannot be reached holds the deletion instead (`ProviderUnavailable`, see
  [below](#deleting-a-vm-while-its-provider-cannot-be-reached)).

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
  VM's binding untouched, and names the cloned VM left on the provider. The
  target is created before the `Clone`, on every topology, and the `VMClone`
  records its UID (`status.targetUID`): only that object is ever bound or
  stamped onto the clone (see
  [VM ownership: Clones: re-bind](vm-ownership.md#clones-re-bind)).
- `spec.target.annotations` of a `VMClone` or `VMMigration` are copied to the
  target without keys in the reserved `virtrigaud.io` domain
  (`virtrigaud.io/...` and `*.virtrigaud.io/...`), such as
  `virtrigaud.io/orphan-on-delete`, `virtrigaud.io/force-delete` and the
  provenance annotations. The controller writes its own provenance after the
  user annotations, so it cannot be overridden.
- Adoption binds a pre-existing `virtrigaud.io/adopted` VM only if its
  `spec.providerRef` names the adopting `Provider`, and never adopts a
  hypervisor VM a `VirtualMachine` is bound to through that `Provider`. On a
  clustered `Provider` the hypervisor VM is identified by (host, id) and the
  adoption also records `status.placement.host`
  ([`clustered-provider-inventory.md`](clustered-provider-inventory.md#listing-and-adoption-slice-4)).

### A `Provider` in another namespace

A `spec.providerRef` that names another namespace is used only if that
`Provider`'s `spec.consumerNamespaceSelector` selects the VM's namespace (the
same applies to `spec.classRef` and `spec.imageRef`, checked where they are
used). Otherwise the VM reports `Ready=False` / `ConsumerNotAllowed` and no
provider call is made — for a bound VM too. Like a `ProviderRefMismatch`,
deleting such a VM keeps the finalizer (also when that cross-namespace
`Provider` no longer exists) until access is restored or the VM carries
`virtrigaud.io/orphan-on-delete` or `virtrigaud.io/force-delete` (on a
clustered `Provider`, both need that `Provider`'s permission, see
[Detaching a VM without deleting it](#detaching-a-vm-without-deleting-it-virtrigaudioorphan-on-delete)). See
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
refused. It also reads the `VMSnapshot` CRD and requires `status.vmUID` and
`status.boundProvider` (a snapshot's binding to its VM and Provider; an older
CRD would prune them). A missing feature in any of the five CRDs is
`missing`; an unreadable one is `unknown`:

| State | Meaning | Readiness |
|---|---|---|
| `verified` | Every feature is present. | Ready |
| `missing` | The CRD is older than the manager, or absent. An error is logged and `virtrigaud_errors_total{reason="vm-crd-security-features-missing"}` counts each check. | **Not ready** until the CRD is upgraded |
| `unknown` | The CRD cannot be read (with `rbac.scope: namespace` the manager has no cluster-scoped read). A warning is logged. | Ready |

The state is exported as
`virtrigaud_manager_vm_crd_security_features{state="verified|missing|unknown"}`
(1 for the current state). The manager needs `get` on those five CRDs
(`resourceNames: [virtualmachines, providers, vmclasses, vmimages, vmsnapshots
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
- **Clustered `Provider`s (`topology: cluster`): consumers need the Provider's
  permission.** A detached VM keeps running on its host, but it no longer counts
  toward that host's committed capacity, so the scheduler would place other VMs
  into capacity it still uses. So a VM in **another namespace** than its
  clustered `Provider` is detached only when the `Provider`'s administrator has
  set `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"` on the
  `Provider` (only the value `"true"` counts). Otherwise the deletion is held:
  the finalizer stays, the VM gets `Ready=False` with reason
  `OrphanOnDeleteNotAllowed` and a `Warning` event, and it is re-checked every
  2 minutes. Ask the administrator to allow it, or remove the orphan annotation
  to delete the VM (and its hypervisor VM) normally. A VM in the `Provider`'s
  own namespace, a VM that is not bound yet, and every VM of a single-host
  `Provider` are detached as before.

This replaces the previous workaround — pointing `spec.providerRef` at a
`Provider` that doesn't exist and then deleting the `VirtualMachine` — which the
CRD rule now rejects for a bound VM.

`virtrigaud.io/force-delete: "true"` releases the finalizer
when the provider `Delete` keeps failing, when no delete can be routed (an
unbound clustered VM, a placement/topology mismatch, a `ProviderRefMismatch`,
a cross-namespace `Provider` the VM's namespace may not use
(`ConsumerNotAllowed`)), or when the manager cannot reach the `Provider`
(`ProviderUnavailable`, below).
It is an escape hatch; to detach a VM on purpose, use `orphan-on-delete`.
Every force-delete release that did not (confirmably) delete the hypervisor VM
records a `Warning` event **`VMLeftOnHypervisor`** on the `VirtualMachine`,
naming it, its provider id, its `Provider` and why nothing was deleted (the
Provider could not be reached, its `Delete` failed, no delete could be routed,
or its own domain is on another host) — never a host or a provider error; the
manager log has those (`force-delete: removing the finalizer without a confirmed
provider delete`). Use it to find hypervisor VMs left behind.
On a clustered `Provider`, a force-delete that releases a VM whose `Delete`
failed or was held (`HostUnreachable`, `DiskCheckFailed`, `DiskInUse`,
`OwnDomainOnAnotherHost`, `ProviderUnavailable`) leaves its domain running on
the host outside the committed-capacity accounting; check the host (and cordon
it or lower its overcommit ratio) until the leftover domain is removed.

So, as for `orphan-on-delete`, a VM in **another namespace** than its clustered
`Provider` (a consumer) is force-deleted only when the `Provider`'s
administrator has set `infra.virtrigaud.io/allow-consumer-orphan-on-delete:
"true"` on it — the same permission covers both, since both leave the domain
running outside the accounting. Otherwise the annotation is **ignored**: the
finalizer stays, the VM gets `ForceDeleteRefused=True` with reason
`ForceDeleteNotAllowed` (a constant message naming the `Provider` and the
permission) and one `Warning` event `ForceDeleteNotAllowed`, and the delete
keeps being retried exactly as if the annotation were not set — `Ready` and,
on a clustered VM, `DeleteBlocked` still say why it waits. It completes as soon
as the provider deletes the VM (or answers that it is gone, for example after
the administrator removed it by hand). Removing the annotation, or the
`Provider` allowing it, removes the condition. Each refusal counts under
`virtrigaud_errors_total{reason="force-delete-not-allowed"}`. This applies to a
consumer VM that is bound, or whose own domain was found on another host; a VM
in the `Provider`'s own namespace and every VM of a single-host `Provider` are
force-deleted as before. It also applies when the VM's namespace may no longer
use the `Provider` (`ConsumerNotAllowed`) or its reference no longer matches
(`ProviderRefMismatch`): the `Provider`'s administrator then decides.

The same gate applies to `virtrigaud.io/force-delete` on a `VMSnapshot` (whose
provider snapshot delete failed or could not be sent) and on a `VMMigration`
(whose source snapshot delete failed) when its VM is in another namespace than
its clustered `Provider`: without the `Provider`'s permission the annotation is
ignored, the object gets `ForceDeleteRefused=True/ForceDeleteNotAllowed` and
one `Warning` event, and its delete keeps being retried.

### Deleting a VM while its `Provider` cannot be reached

When the `Provider` exists but the manager has no usable client for it — its
provider runtime is not `Running` or has no endpoint (during every provider
rollout or restart), the provider pod is down, or the TLS setup, connection or
`Validate` call fails — no `Delete` can be sent, and the hypervisor VM may still
exist. The deletion is **held** instead of releasing the finalizer (earlier
releases released it, leaving the hypervisor VM running unmanaged):

- A single-host VM shows `Ready=False` with reason **`ProviderUnavailable`**. A
  clustered VM shows `DeleteBlocked=True/ProviderUnavailable` and
  `Ready=False/DeleteBlocked`, like its other held deletes
  ([`clustered-provider-inventory.md`](clustered-provider-inventory.md#shared-storage-the-cluster-wide-disk-guard-a61)).
  The message is constant and names no endpoint or host; one `Warning` event
  `DeleteBlocked` is recorded when the hold begins.
- The delete is retried with a backoff — 15 seconds, doubling up to 5 minutes,
  counted from the deletion request (single-host) or from when `DeleteBlocked`
  became `True` (clustered) — and completes on the first retry after the
  `Provider` answers again.
- The cause is in the manager log: search for `Cannot reach the provider to
  delete the VM` — the same line for single-host and clustered VMs, with the
  error and, for a clustered VM, its host. Each retry counts under
  `virtrigaud_errors_total{reason="provider-resolve"}`.
- If the `Provider` answers but its `Delete` then fails for another reason,
  `DeleteBlocked` is removed and `Ready` becomes `False/ProviderError` (a
  constant message; the provider's answer is in the manager log).
- `orphan-on-delete` (no `Provider` is resolved) and `force-delete` release it
  at once; the hypervisor VM is then left for manual removal. On a clustered
  `Provider` in **another namespace**, both need the Provider's permission
  (above); without it the delete stays held. A force-deleted clustered VM's
  domain keeps running on its host, no longer counted toward that host's
  committed capacity, and re-creating a VM with the same namespace and name is
  then held at `Placed=False/RestorePending` until that domain is re-attached
  or removed ([`clustered-restore.md`](clustered-restore.md)).
- A held delete is retried on its backoff only. The `VirtualMachine`
  controller reconciles a VM being deleted when its deletion is requested and
  then only when its annotations or finalizers change — never on its own
  status writes — so `orphan-on-delete` or `force-delete` set while it is held
  is acted on at once, and a hold does not re-run itself.

A `Provider` that no longer exists in the VM's own namespace is different:
there is nothing to delete through, and the finalizer is released as before.
Since the `Provider` in-use finalizer (below), that happens only after an
administrator force-deletes the `Provider`, or for a `Provider` deleted before
the upgrade.

## Deleting a `Provider`

A `Provider` carries the finalizer `provider.infra.virtrigaud.io/in-use`
([ADR-0010](adr/0010-provider-in-use-finalizer.md)); the manager adds it to
every `Provider`, including those created before the upgrade. Deleting a
`Provider` that is still needed does not remove it:

- **What holds it:**
  - Every `VirtualMachine` whose placement Provider it is (`status.boundProvider`,
    else `spec.providerRef`), until its `VirtualMachine` finalizer is gone, if:
    - it is bound, or may have something on the hypervisor (`status.id`,
      `status.placement.pendingHost`, or the `CreateRequested` provisioning
      marker); a VM never created holds nothing;
    - and it is in the `Provider`'s namespace, bound through it, or in a namespace
      the `Provider`'s `spec.consumerNamespaceSelector` allows. A namespace that
      may not use the `Provider` cannot hold it.
  - A `VMSnapshot` of one of those VMs with a delete still to run.
  - An in-flight `VMClone` of one of them.
  - A `VMMigration` in flight (or being deleted) out of one of them, or into this
    `Provider` from its namespace or an allowed one.
- **Its runtime is kept:** while it is held, its Deployment, Service and
  ServiceAccount are reconciled as before and its runtime phase stays `Running`,
  so the VMs can still be deleted through it.
- **What you see:** `DeleteBlocked=True` with reason `ProviderInUse`; the
  message gives the number of objects of each kind and the namespaces they are
  in, with a count per namespace (at most 10 namespaces are named, and never an
  object). One `Warning` event `ProviderDeleteBlocked` is recorded when the hold
  begins. A hold that has not changed writes nothing.
- **Release:** as soon as the last object goes (the manager watches them), the
  runtime is removed and the finalizer released.

### Deleting a namespace that contains a `Provider`

Kubernetes deletes every object in a namespace being deleted, **including the
provider runtime's Deployment and pods** — a finalizer cannot stop that, and
nothing can be re-created in that namespace. So:

1. The `Provider` is held by its VMs, so they are not released without a
   provider `Delete`.
2. A VM whose delete reaches the provider while its pod is terminating (a 15 s
   `preStop` within a 30 s grace period) is deleted normally.
3. A VM whose delete comes later is held with `ProviderUnavailable` (above), not
   orphaned. The `Provider` shows `DeleteBlocked=True` with reason
   `ProviderInUseRuntimeNotKept`, and the namespace stays `Terminating`.
4. To finish, set `virtrigaud.io/force-delete: "true"` on the `Provider` or on
   each remaining VM. The hypervisor VMs left are then unmanaged.

**Delete the VMs, and wait for them to go, before deleting their namespace.**
A `Provider` deleted with `kubectl delete --cascade=foreground` has the same
limit (the garbage collector deletes its runtime first); use the default
cascade.

VMs in **other** namespaces are not deleted with the namespace, so a `Provider`
with cross-namespace consumers waits for them. Delete them, detach them with
`virtrigaud.io/orphan-on-delete: "true"` (a clustered `Provider` must allow
that for consumers, above), or force-delete the `Provider`.

### Releasing a held `Provider`: `virtrigaud.io/force-delete`

Annotating a `Provider` being deleted `virtrigaud.io/force-delete: "true"`
releases it at once: its runtime is removed, the finalizer is released, and a
`Warning` event `ProviderForceDeleted` records how many objects (per kind and
namespace) still used it. The objects are not touched. VMs in the `Provider`'s
namespace are released on their next delete without any provider call; VMs in
other namespaces stay held until they are annotated `orphan-on-delete` or
`force-delete`. Either way their hypervisor VMs are left running, unmanaged.

### Uninstalling and rolling back

Delete VMs and `Provider`s **before** removing the manager: without it, nothing
releases the finalizer and they stay `Terminating`. After a rollback to a manager
without this finalizer, a `Provider` deleted later also stays `Terminating`;
once no VM uses it, remove the finalizer by hand:

```sh
kubectl patch provider <name> -n <namespace> --type=json \
  -p '[{"op":"remove","path":"/metadata/finalizers"}]'
```

## A Create whose outcome is unknown (`CreateRequested`)

A single-host `VirtualMachine` has no provider id until its Create is
recorded. So that a VM made by a Create is never lost track of, the manager
re-reads the `VirtualMachine` live, then records `Provisioning=True` with
reason **`CreateRequested`** (and the `Provider` it is sent through,
`status.boundProvider`) over that read, with an optimistic lock on its
`resourceVersion`; only once that write is confirmed is the Create sent. Each
such write is one **attempt**, identified by the condition's
`lastTransitionTime` (RFC 3339). The Create's outcome — `status.id` and its
`Provisioning` condition — replaces the marker. The marker is kept when the
outcome is unknown: a transport failure or deadline, an error the provider did
not classify as a refusal, a `VM_OPERATION_FAILED`, `HOST_UNAVAILABLE` or
`VM_DISK_CHECK_FAILED` answer (a provider also sends these for an operation
that may still complete on its host), or **any** failure answering a re-send
of the Create: that answer is about the re-send only and says nothing about
what the earlier attempt made (vSphere, for example, answers `Conflict` for
its own VM while the clone is still running). A re-send is retried with a
backoff from its last attempt (15 seconds, then about as long as the failures
have lasted, up to 5 minutes), and is not sent while the VM is being deleted
or its spec changed since the reconcile read it. On a first Create, a
`Conflict`, `InvalidSpec`, `NotFound`, `NotSupported`,
`Unauthorized`, `QuotaExceeded`, an answer carrying the reason
`CREATE_NOT_PERFORMED` (the provider made nothing), and a non-retryable
`FailedPrecondition` or `PermissionDenied` replace it. The last two hold the VM
with `Ready=False/CreateRefused` (one `Warning` event carrying the provider's
answer) and are re-checked with a backoff of 15 seconds doubling to 5
minutes. In any owner-stamp lookup, a listed VM whose stamp the provider
reports it cannot read (`owner_stamp_state`) makes the answer ambiguous, and
the VM is held. While the manager
keeps running it also remembers a created id whose record failed, and writes
it on the next reconcile.

A VM with the marker and no `status.id` is sent the Create again only when its
provider reports `supports_owner_rebind` (GetCapabilities field 26, rc2 R4):
the rc2 vSphere, libvirt, Proxmox and mock providers do. A provider that does
not — every image older than rc2 — is never sent it again: the VM is held
(`Ready=False/CreateOutcomeUnknown`, one `Warning` event, re-checked every
minute), whatever its type. While the capability can't be read, nothing is
sent. With the capability:

| Provider | Create | Delete |
|---|---|---|
| vSphere | The Create is sent again: the provider re-binds the VM stamped with this VirtualMachine's UID. | Held (`CreateOutcomeUnknown`): vSphere's `ListVMs` does not report owner stamps. |
| libvirt | The Create is sent again (re-bind, as above). | The VM stamped with this VirtualMachine's UID is found (`ListVMs`) and deleted by id. If none is listed, the finalizer is released only on a positive confirmation: once no Create can still be running (10 minutes after it was requested), each domain name the Create could have used (`<namespace>.<name>`, and the bare name) must answer a definite not-found — the libvirt provider answers `NotFound` when virsh reports no such domain — and a Normal `CreateConfirmedAbsent` event is recorded. Any other answer holds the delete. |
| Proxmox | The Create is sent again: the provider re-binds the VM stamped with this VirtualMachine's UID. A VM created by a Proxmox provider image that does not stamp (before the owner stamp, #380) cannot be re-bound; such an image does not report the capability, so it is never sent the Create again. | The VM stamped with this VirtualMachine's UID is found (`ListVMs`) and deleted by id. If none is listed, held: a VMID cannot be confirmed absent by name (so is a legacy, unstamped VM). |
| The mock provider | The Create is sent again: the mock re-binds the VM it recorded for this VirtualMachine's UID. | Held (`CreateOutcomeUnknown`). |

To resolve a held VM, check the hypervisor for it:

- **It exists:** record its id, and the VM is managed (or, if being deleted,
  deleted) as usual:
  `kubectl patch virtualmachine <name> -n <ns> --subresource=status --type=merge -p '{"status":{"id":"<provider id>"}}'`
  (a Proxmox VMID, a vSphere managed object id such as `vm-1234`).
- **It does not exist:** confirm it for this attempt:
  `kubectl annotate virtualmachine <name> -n <ns> virtrigaud.io/create-outcome=not-created@<attempt id>`,
  where the attempt id is the `lastTransitionTime` of the `Provisioning`
  condition:
  `kubectl get virtualmachine <name> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="Provisioning")].lastTransitionTime}'`.
  The manager checks it against the live object, records a `Warning` event
  **`CreateOutcomeConfirmed`** (audit), removes the annotation and creates
  the VM (or, while it is being deleted, releases it). A value naming another
  attempt, or any other value, is ignored and removed; the annotation is also
  removed before every new attempt, so a confirmation never carries over.
- A held delete is also released by `virtrigaud.io/force-delete: "true"`
  (with a `VMLeftOnHypervisor` event) or detached by
  `virtrigaud.io/orphan-on-delete: "true"`.

A VM with the marker whose `spec.providerRef` no longer names the `Provider`
the Create was sent through is refused like a bound one
(`ProviderRefMismatch`). The marker counts as using the `Provider` (with
`status.id` and `status.placement.pendingHost`); a VM held because its
`Provider` is being deleted (`ProviderDeleting`) never gets one.

The manager's cache can show a `Provider` as not being deleted after its
deletion began. So right before every `Create` (after the marker or the
pending host is recorded), `Clone` and `ImportDisk`, the `Provider` is re-read
from the API server, and nothing is sent when it is being deleted, gone or
replaced: the `VirtualMachine`, `VMClone` or `VMMigration` gets
`Ready=False/ProviderDeleting` (a create marker written for this attempt alone is
replaced by `Provisioning=False/ProviderDeleting`, since no Create was sent; the
marker of an earlier attempt whose outcome is unknown is kept), with
a constant message and one `Warning` event, re-checked every minute (ADR-0010).

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
