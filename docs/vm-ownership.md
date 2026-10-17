# VM ownership: how `Create` treats an existing VM

A vSphere VM created by VirtRigaud is named after the `VirtualMachine` object,
without its namespace. So when several namespaces (tenants) share a Provider,
two `VirtualMachine`s called `web` ask for the same hypervisor name. A VM with
that name may also already exist for reasons unrelated to VirtRigaud. (The
libvirt provider now names a new domain `<namespace>.<name>`, so two namespaces
no longer share a name there; the rule below still applies to libvirt, for
legacy bare-named domains and for the rare remaining collision. See
[`libvirt-domain-ownership.md`](libvirt-domain-ownership.md#domain-names).)

The **libvirt** and **vSphere** providers follow one rule:

**`Create` never binds a `VirtualMachine` to an existing VM unless it can prove
that this `VirtualMachine` created the VM.**

This page covers the parts both providers share and the vSphere details. For
the libvirt details, see
[`libvirt-domain-ownership.md`](libvirt-domain-ownership.md).

The **Proxmox** provider stamps its VMs too, but Proxmox doesn't key VMs by
name: every create gets a new VMID. Its stamp re-binds a re-sent `Create` to
the VM an earlier one made, instead of refusing a name collision. See
[Proxmox](#proxmox).

## Shared model

1. **The owner goes on the wire.** Every `Create` carries
   `CreateRequest.owner` (`ObjectIdentity{uid, namespace, name}`, proto field
   11). The manager fills it from `vm.UID`, `vm.Namespace` and `vm.Name`. Only
   the UID authorizes a bind. The namespace and name are there for audit and
   diagnostics.
2. **The owner is stamped on create.** The provider records the owner on the VM
   it creates. libvirt stores it in the domain `<metadata>`. vSphere stores it
   in the VM's ExtraConfig.
3. **Creates fail closed.** If a VM with the requested name already exists
   where the provider would create it, the provider binds to it (idempotent
   success) **only** if its stamp records the requester's UID. That case covers
   a create that is retried because the manager lost its `status.id` write. In
   every other case the create is refused with a non-retryable `Conflict`,
   which is `codes.AlreadyExists` on the wire.
4. **Unsafe names are rejected.** The provider rejects a name that its tooling
   would resolve as something other than a name. The error is `InvalidSpec`
   (`codes.InvalidArgument`), and no hypervisor lookup runs first.
   `VirtualMachine` CRD validation doesn't change, because every provider shares
   it.

### What operators see

A refused create leaves `status.id` empty. The `VirtualMachine` is therefore
bound to nothing, and deleting it never calls the provider's `Delete`, so the
other VM is untouched. The `VirtualMachine` shows:

- `Ready=False` and `Provisioning=False` with reason **`ProviderConflict`** for
  an ownership refusal, or **`ValidationError`** for a rejected name.
  `observedGeneration` is set.
- A message that names only the requested VM. It never says which namespace or
  `VirtualMachine` owns the other VM, and it never gives an inventory path. The
  provider log records those details for operators.
- A re-check every **2 minutes**, or every **30 seconds** for a rejected name,
  instead of the 5-second transient retry.
- The manager metric `virtrigaud_errors_total{reason="provider-create-rejected"}`.

To resolve a refused create, do one of the following:

- **Adopt the VM** if it should be managed. Set the `virtrigaud.io/adopt-vms`
  annotation on the Provider (see `examples/vm-adoption-example.yaml`). Adopted
  VMs get `status.id` directly and never go through `Create`.
- **Remove or rename the other VM** if it is stale. The next re-check creates
  the VM.
- **Rename the `VirtualMachine`.** Names are immutable, so recreate it under a
  different name.

## Adoption stamps the adopted VM

Adoption (`virtrigaud.io/adopt-vms` on a Provider) creates a `VirtualMachine`
for each VM no `VirtualMachine` manages. The VM is then **stamped with that
`VirtualMachine`'s identity** (its UID, namespace and name), exactly as if
VirtRigaud had created it, before the `VirtualMachine` is bound to it
(`status.id`). This applies to every provider that stamps: vSphere, libvirt
(single-host and clustered) and Proxmox. Owner-checked calls then accept the
adopted VM, the Proxmox `Delete` removes it, and every other Provider object's
adoption sees it as owned.

Before this change, single-host adoption left the VM's stamp as it was. An
adopted VM that still carried a previous owner's stamp — a `VirtualMachine`
detached with `virtrigaud.io/orphan-on-delete`, then adopted again — was
answered `VM_NOT_OWNED` by its new `VirtualMachine`'s Proxmox `Delete` and left
on the hypervisor. An unstamped adopted VM stayed unstamped, so another Provider
object for the same vCenter or host could adopt it a second time.

The stamp is written by the `TransferOwner` RPC. A single-host provider that
implements it reports `supportsOwnerTransfer`; a clustered libvirt provider
already used it for its own adoption (`supportsRoutedAdoption`, see
[`clustered-provider-inventory.md`](clustered-provider-inventory.md#listing-and-adoption-slice-4)).

### Which VMs are stamped

Adoption only stamps a VM it would adopt anyway. It never adopts, and never
stamps:

- a VM bound to a `VirtualMachine` of this Provider (its `status.id`);
- a VM whose stamp names the UID of a `VirtualMachine` that exists, in any
  namespace and under any Provider object;
- a VM whose stamp can't be read or names more than one owner, a template, or
  an image artifact;
- a VM whose id is the `status.id` of a `VirtualMachine` bound through **another**
  single-host Provider object of the same type and endpoint. Such a VM may be
  unstamped (created before owner stamps, or adopted before this release), and
  stamping it would take it away from that `VirtualMachine`. It is reported as
  `bound to a VirtualMachine of another Provider object for the same endpoint`.
  The same endpoint reached under two names (a DNS alias, a host name and its
  IP) is not recognized.

Of the rest, it stamps:

- **an unstamped VM**;
- **a VM stamped only with UIDs of no `VirtualMachine` in this cluster**, only
  with the Provider's `virtrigaud.io/adopt-stamped-vms: "true"` opt-in (see
  [Owner stamps are scoped to one cluster](#owner-stamps-are-scoped-to-one-cluster)),
  and only after an **uncached** list of every `VirtualMachine` from the API
  server confirms that none has one of those UIDs. A UID is never reused, so a
  UID absent from the API server never comes back. Those UIDs are the only
  stamps the provider may replace. A stamp the check finds alive is reported
  (`its owner stamp names a VirtualMachine that exists`), and when the check
  can't be made the VM is reported and retried at the next discovery.

The adopted `VirtualMachine` and its `VMClass` are created in the Provider's
own namespace, as before. Consumer grants (`spec.consumerNamespaceSelector`)
don't widen this. An existing adopting `VirtualMachine` whose class or image is
in another namespace that doesn't grant it is neither stamped onto the VM nor
bound.

### Compare-and-swap on the hypervisor

`TransferOwner` never overwrites a stamp it wasn't told it may replace. The
provider reads the VM, refuses unless every stamp on it is absent, the new
owner's (an idempotent retry), or one of the replaceable UIDs, and then writes
the stamp so that a concurrent change is detected:

| Provider | The VM must still be the VM listed | The write is conditional on |
|---|---|---|
| vSphere | Its MOID (never reused in a vCenter), and its instance UUID when one is sent | The VM's `config.changeVersion` at the read. vCenter refuses the reconfigure (`ConcurrentAccess`) if the configuration changed since. |
| libvirt | Its domain name **and** domain UUID (`ListVMs` `provider_raw["uuid"]`) | A per-host lock in the provider process. libvirt has no conditional metadata write, so two provider processes fronting one host aren't serialized: run one Provider per host endpoint. |
| Proxmox | Its PVE name, which must be the adopting `VirtualMachine`'s name (the rule an unstamped VM's `Delete` uses), so a reused VMID is never stamped | The config `digest` at the read. PVE refuses the update (`detected modified configuration`) if the configuration changed since. |

The new stamp is read back before the call succeeds. A refusal is
`AlreadyExists` (owned by someone else, or a stamp, template or artifact that
can't be taken over) or `NotFound` (gone, or not the VM listed). Its message
names only the VM, never another owner. A concurrent change, a locked Proxmox
VM (`lock: backup`, `clone`, ...) or a hypervisor error is retryable and is
marked `VM_OPERATION_FAILED`, so it doesn't count toward the Provider's circuit
breaker. The rest of the VM is never changed: the Proxmox notes other than the
stamp line are kept, and no other ExtraConfig key or metadata element is
touched.

The write needs the privilege the provider already needs to stamp a VM at
create time: vSphere `VirtualMachine.Config.AdvancedConfig`, Proxmox
`VM.Config.Options`.

### Order, and what deleting the `VirtualMachine` does meanwhile

The VM can only be stamped with a `VirtualMachine` that exists (it needs a
UID), and the `VirtualMachine` must never be bound to a VM that doesn't carry its
stamp. So one discovery:

1. creates the `VirtualMachine`, labelled `virtrigaud.io/adopted=true` and
   annotated `virtrigaud.io/adopted-id: <VM id>`, **without** `status.id`. The
   VirtualMachine controller waits for its `status.id` and never creates a VM
   for it;
2. re-reads it from the API server, and stamps the VM with it only while it
   still exists, is not being deleted and still waits for that VM;
3. writes `status.id` and `status.boundProvider` only after the stamp was read
   back, and only while the `VirtualMachine` still waits for that VM.

A failure or a manager restart between these steps is finished by the next
discovery (every 5 minutes while one is pending):

| State left behind | Next discovery | Deleting the `VirtualMachine` in this state |
|---|---|---|
| Created, VM not stamped (the transfer failed retryably) | Stamps the VM with the waiting `VirtualMachine` (found by its annotation) and binds it | No provider call: the VM is untouched |
| VM stamped, not bound (the status write was lost) | Sees the VM stamped with the waiting `VirtualMachine`'s UID and writes the binding, without a second transfer | No provider call. The VM stays on the hypervisor, stamped with a UID that no longer exists; adopting it again needs `adopt-stamped-vms` |
| The provider refused the transfer (the VM is someone else's now, gone, or replaced), or the listing shows the VM can no longer be adopted by the waiting `VirtualMachine` | Deletes the waiting `VirtualMachine` again: re-read first, preconditioned on its UID and resourceVersion, only while it still waits. It has no `status.id`, so its deletion makes no provider call | — |

A waiting `VirtualMachine` created by an older manager (no `adopted-id`
annotation) is bound the same way when its VM can be stamped, but is never
deleted by adoption.

Only a `VirtualMachine` named as adoption names it (the sanitized VM name, or
`vm-<id>`) is ever handed a VM or bound to it, and only for a VM that passes
the Provider's `virtrigaud.io/adopt-filter`. A `VirtualMachine` someone creates
in the Provider's namespace with the adopted label and an `adopted-id`
annotation under another name gets nothing; a waiting one whose VM the filter
now excludes is withdrawn. A libvirt domain listed without a UUID is skipped
(the provider needs it for the compare-and-swap), and a Proxmox VM found on a
node outside the Provider's node selector is refused.

### What operators see

`Provider.status.adoption.message` counts the VMs adopted, failed and deferred,
the unfinished adoptions withdrawn, and — by reason, naming a few VM ids — the
VMs not adopted. The reasons are constant text and never name another owner.
The manager log has the details, and refusals count in
`virtrigaud_errors_total{reason="adoption-owner-transfer"}`.

### Older providers, older managers

- **A provider that doesn't report `supportsOwnerTransfer`** (older than this
  release, or a third-party provider): adoption works as before. The
  `VirtualMachine` is created and bound in one pass and the VM keeps whatever
  stamp it had. The adoption message ends with `adopted VMs are not stamped
  with their new owner ... upgrade the provider`.
- **A provider whose capabilities can't be read**: nothing is adopted that
  discovery, because the provider might be able to stamp
  (`Waiting for the provider's capabilities before a discovery ...`). Retried
  every 5 minutes.
- **A manager older than this release** never calls `TransferOwner` for a
  single-host Provider, so a newer provider changes nothing for it.

### VMs adopted before this release

Adoption stamps a VM only when it adopts it: **a VM adopted by an earlier
release keeps the stamp it had.** On vSphere and single-host libvirt nothing
checks that stamp on a single-host per-VM call. On Proxmox, an adopted VM that
still carries a previous owner's stamp is answered `VM_NOT_OWNED` by its
`VirtualMachine`'s `Delete` and left on the cluster, and by every other per-VM
call too: its `VirtualMachine` is held as `Ready=False/VMNotOwned` (see
[Per-VM calls](#per-vm-calls-describe-power-reconfigure-snapshots-disks-and-clone-sources)). To find them, compare each
adopted Proxmox `VirtualMachine`'s UID with the stamp on its VM
(`qm config <vmid> | grep virtrigaud.owner`). To fix one, either remove the
`virtrigaud.owner` line from the VM's Notes (the VM is then unstamped and is
deleted with its `VirtualMachine` while its PVE name matches), or detach the
`VirtualMachine` with `virtrigaud.io/orphan-on-delete: "true"`, delete it, and
let adoption take the VM again with `adopt-stamped-vms` (it is then stamped).

## vSphere

### What was wrong

Before this change, the vSphere `Create` searched the **whole default
datacenter** for a VM with the requested name (`finder.VirtualMachine(name)`).
If it found one, it returned that VM's managed object ID (MOID) as the new
`VirtualMachine`'s ID. That caused three problems:

- **Any same-named VM was bound.** It could belong to another tenant, or be an
  unrelated production VM. Deleting the `VirtualMachine` then powered off and
  destroyed that VM, including its disks.
- **Lookup errors were discarded.** An ambiguous name ("multiple found") or a
  transient failure fell through to creating a new VM.
- **MOID-shaped names resolved as references.** govmomi's `find.Finder`
  resolves an argument such as `vm-1234` as a managed object reference before
  it tries it as a name (`object.ReferenceFromString` in `find/finder.go`,
  govmomi v0.52.0). So a `VirtualMachine` named `vm-1234` bound to the VM with
  MOID `vm-1234`, in any datacenter. MOIDs are sequential, so they are easy to
  guess.

### The owner stamp

A new VM gets three ExtraConfig (advanced configuration) keys:

| Key | Value |
|---|---|
| `virtrigaud.owner.uid` | The `VirtualMachine`'s UID. This is the only key that authorizes a bind. |
| `virtrigaud.owner.namespace` | The `VirtualMachine`'s namespace. Informational. |
| `virtrigaud.owner.name` | The `VirtualMachine`'s name. Informational. |

- The keys **don't** use the `guestinfo.` prefix, so software inside the guest
  can't read them through VMware Tools. They are visible only through the
  vSphere API, to principals that can read the VM's configuration. To inspect
  them, run `govc vm.info -e <vm> | grep virtrigaud.owner`.
- The provider writes them on **every** create path, in the same vCenter task
  that creates the VM:
  - `CloneVM_Task` from a template (`VMImage` `templateName`).
  - `CreateVM_Task` with an imported disk (the migration import path).
- A clone copies the source's ExtraConfig. When a create carries no owner (a
  manager older than the provider), the keys are set to `""`, which removes
  them. The new VM therefore never inherits a stamp from its template. The
  prepared-image stamp a template carries (`virtrigaud.image.*`, ADR-0009; see
  [image preparation](image-preparation.md#vsphere-identity-safe-prepared-templates))
  is cleared the same way on every create: a VM is never an image artifact.
- Writing ExtraConfig needs the vCenter privilege
  **Virtual machine > Change Configuration > Advanced configuration**
  (`VirtualMachine.Config.AdvancedConfig`). Cloud-init injection through
  `guestinfo` already needed it. Every create now needs it. See
  [Required vCenter privileges](#required-vcenter-privileges).

### Where `Create` looks

The provider resolves the **target folder** exactly as it will create the VM:

1. `spec.placement.folder`, else the Provider's default folder.
2. If that folder doesn't exist, or its name is ambiguous, the datacenter's
   default VM folder. This fallback is unchanged. Any other folder-lookup error,
   such as a transient vCenter failure, fails the create so it's retried; the
   provider no longer falls back.

The provider then lists the `VirtualMachine` objects that are **direct
children** of that folder and compares their names to the requested name,
byte for byte. It doesn't use a finder search or MOID resolution. vSphere
allows VMs with the same name in different folders. A same-named VM in any
other folder, vApp or datacenter is neither bound nor considered, and the
create proceeds in the target folder.

| VM with the requested name in the target folder | Result |
|---|---|
| None | The VM is created and stamped. This happens even when the request has no owner. |
| One, stamped with the requester's UID | Idempotent success: its MOID is returned. |
| One, stamped with a different UID | **Refused** (`Conflict`) |
| One, with no stamp: not created by VirtRigaud, or created before this change | **Refused** |
| One, with an unreadable stamp (repeated key or non-string value) | **Refused** |
| One, but it's a template | **Refused** |
| One, but the request carries no owner (older manager) | **Refused** |
| More than one: vCenter normally forbids this | **Refused** |
| Lookup failed transiently | Error, retried. The provider never creates or binds on an unknown answer. |

If a same-named VM appears in the folder after the check, the create fails with
vCenter's `DuplicateName` fault and is retried. The retry makes the decision
again.

### Rejected names

`Create`, the `Clone` target, the `ImagePrepare` target and a bare template name
all reject these names before any vCenter call:

- A name made of `vm-` followed only by digits, such as `vm-42`. That is the
  shape of a vCenter VirtualMachine MOID. Names such as `vm-web` or `vm-1a` are
  accepted.
- A name that contains `/`, `\`, `%` or `:`. These are inventory-path
  separators, characters that vSphere escapes in names, or the MOID `Type:value`
  separator. None of them can appear in a Kubernetes name. A template can still
  be referenced by an inventory path; see
  [Clone sources](#clone-sources-only-real-templates).

### Clone sources: only real templates

Before this change, a `VMImage`'s `templateName` was resolved with the same
datacenter-wide finder search. It matched **any** VM with that name, whether
running or powered off, and a MOID-shaped name matched any VM in any
datacenter. A tenant could point a `VMImage` at another tenant's VM, or a
production VM, and full-clone it, which exposed that VM's disks. `ImagePrepare`
had the same problem: its "already prepared" check accepted any VM with the
target name, and every VM created from that `VMImage` then cloned it.

Now a template reference is resolved as follows:

- **Validated first.** A bare name follows the same rules as a VM name (see
  [Rejected names](#rejected-names)). An inventory path may contain `%`, which
  vSphere uses to escape special characters within a path element. It must not
  contain `\` or `:`, and it must not have an empty, `.` or `..` element. Its
  last element must not have the form `vm-<digits>`. An invalid reference gets
  `InvalidSpec` before any vCenter call.
- **Resolved exactly, without the finder.**
  - A bare name is compared byte for byte with the names of the VMs in the
    Provider's default datacenter, including nested folders. No MOID
    resolution or globbing is applied.
  - An inventory path is resolved with `SearchIndex.FindByInventoryPath`, the
    documented alternative. The path is either absolute, such as
    `/DC/vm/templates/ubuntu`, or relative to the datacenter's VM folder, such
    as `templates/ubuntu`. An absolute path must be under the default
    datacenter's VM folder. The prefix comparison is exact, so a path in any
    other datacenter the vCenter account can see gets `InvalidSpec` before any
    lookup.
- **Only a real template is used.** Only an object with `config.template=true`
  is used as a clone source or counted as a prepared image:

  | VMs matching the reference | `Create` | `ImagePrepare` (target / `templateName`) |
  |---|---|---|
  | Exactly one template, plus any number of regular VMs with the same name | Clones the template. The regular VMs are ignored and can't block a shared template name. | Already prepared / verified |
  | Two or more templates | **Refused**: `InvalidSpec`, "ambiguous". Use an inventory path. | **Refused** |
  | Only regular VMs, running or powered off | **Refused**: `InvalidSpec`, "does not name a vSphere template" | **Refused**. The VM is never treated as the prepared image, overwritten or adopted. |
  | Nothing | Existing not-found path: `Create` retries, `templateName` gets `NotFound` | Target: the OVA is imported. `templateName`: `NotFound`. |
  | Lookup failed transiently | Error, retried | Error, retried. The provider never starts an import on an unknown answer. |

  Messages name only the requested reference. They never name another VM or its
  location.

  A vCenter failure during a lookup can involve other objects' MOIDs or vCenter
  fault text. The provider writes those details to the provider log only. The
  message returned to the caller is generic and retryable: "a vCenter error
  occurred (details in the provider log)". The existing-VM check and the
  owner-stamp read in `Create` work the same way.

  Some information still leaks. Because not-found, "not a template" and
  "ambiguous" get different results, a tenant can learn whether any VM or
  template with a given name exists in the datacenter, but not what it is or who
  owns it.

The `ImagePrepare` column describes a request from a manager older than
ADR-0009 (deprecated legacy mode, this release only), whose target name is the
`VMImage` name. That name is also the name of the template an OVA import
creates, so it follows the VM-name rules. A request from a current manager
carries the `VMImage` identity instead: the template is named after it, looked
up only in the Provider's import folder and reused only when its image stamp
matches. See
[vSphere: identity-safe prepared templates](image-preparation.md#vsphere-identity-safe-prepared-templates).

**OVA imports never carry a stamp from the OVF.** An OVF can include arbitrary
`<vmw:ExtraConfig>` entries, and vCenter maps them into the import spec. A
tenant's OVA could therefore set `virtrigaud.owner.uid` to another
`VirtualMachine`'s UID, or `virtrigaud.image.uid` to another `VMImage`'s. For
as long as the import runs, or if `MarkAsTemplate` and the cleanup both fail,
the imported object is a regular VM, so a create with that name in the same
folder would accept it as its own. To prevent this, `ImagePrepare` removes every
`virtrigaud.*` ExtraConfig key from the import spec before `ImportVApp`, and
logs the removal on the provider side. The real image stamp is added after the
removal. Other OVF ExtraConfig keys are imported unchanged.

Known limitation: a **reference** (`templateName`, including an inventory path)
reaches any template the Provider's vCenter account can read, including a
template prepared for another tenant's `VMImage`. Prepared templates are no
longer shared by name between images, but refusing references to another
image's stamped template is ADR-0009 Slice 7.

### Clones

The `Clone` RPC never binds to an existing VM. If the target name is taken in
the folder, `CloneVM_Task` fails with `DuplicateName`. `CloneRequest` carries no
owner, because the target `VirtualMachine` is created after the clone and given
the returned ID directly. The clone is therefore left **unstamped**, and the
owner stamp it would inherit from the source VM is cleared, as is any
prepared-image stamp (`virtrigaud.image.*`). A clone never claims the source
`VirtualMachine`'s owner, and is never an image artifact.

### Adoption: what `ListVMs` offers

`ListVMs` is the adoption discovery feed: every VM it returns can become a
`VirtualMachine` through `virtrigaud.io/adopt-vms`, and deleting that
`VirtualMachine` destroys the vSphere object. The vSphere `ListVMs` therefore
lists ordinary VMs only. It leaves out:

- **every vSphere template**, stamped or not. That covers every prepared image
  (ADR-0009 marks each one as a template) and any template made by hand or by an
  earlier release;
- **any object with a prepared-image stamp key** (`virtrigaud.image.*`), valid
  or not, template or not. An image import in flight is a plain VM until it is
  marked as a template, and a failed import can leave one behind. Every OVA
  import carries such a key from the moment it exists: a stamped import its
  full stamp, a legacy (identity-less) import one marker key,
  `virtrigaud.image.preparedat`. `Create` and `Clone` clear these keys, so an
  ordinary VM never carries them;
- **any VM whose owner stamp cannot be read** (a key repeated, in any case, or a
  non-string value): who owns it cannot be told;
- **any VM whose configuration cannot be read.**

Leaving these objects out of `ListVMs` is the provider-side control, and the
primary one. The manager has a backstop for third-party and SDK providers: a
listed VM carrying `provider_raw["artifact"]` (`template` or `image`; any value
counts) is never adopted, single-host or clustered, and the key is never copied
into a `VirtualMachine`'s status. A provider that lists a template or image
artifact must set it. The vSphere and Proxmox providers keep leaving these
objects out instead of listing them tagged, because a manager older than the
key would adopt a tagged template, and providers can be rolled before the
manager.

A VM with a readable owner stamp is listed with its owner UID (`ListVMs`
`provider_raw["owner_uid"]`). The adoption controller skips it while the
`VirtualMachine` with that UID exists, in any namespace and under any Provider
object. Before this change the vSphere `ListVMs` reported no owner UID. Adoption
through one Provider object could therefore adopt a VM that a `VirtualMachine`
under another Provider object for the same vCenter owned, or a VM whose create
had not yet recorded its `status.id`. A VM adoption takes over is stamped with
its adopting `VirtualMachine` (see
[Adoption stamps the adopted VM](#adoption-stamps-the-adopted-vm)), so it is
listed as owned from then on.

#### Owner stamps are scoped to one cluster

An owner stamp records the UID of a `VirtualMachine` in the cluster that wrote
it, and means nothing anywhere else. When no `VirtualMachine` in this cluster
has a stamped UID, the controller cannot tell a deleted `VirtualMachine` (a
stale stamp) from a live `VirtualMachine` of **another VirtRigaud
installation**, or of this cluster before a restore, that manages the same
vCenter or host. Adoption therefore **never adopts a VM whose owner stamp names
only UIDs that are not in this cluster**. It logs each one and counts it in the
Provider's adoption status message ("N not adopted — owner stamp names no
VirtualMachine in this cluster").

To adopt stale-stamped VMs on purpose, for example after deleting
`VirtualMachine`s with `virtrigaud.io/orphan-on-delete`, annotate the
single-host Provider:

```yaml
metadata:
  annotations:
    virtrigaud.io/adopt-vms: "true"
    virtrigaud.io/adopt-stamped-vms: "true"
```

Only `"true"` opts in. Set it only when no other VirtRigaud installation
manages the same vCenter or host, and remove it afterwards. Through a provider
that reports `supportsOwnerTransfer`, the stale stamp is **replaced** by the
adopting `VirtualMachine`'s, after an uncached check that no `VirtualMachine`
in this cluster has the stale UID (see
[Adoption stamps the adopted VM](#adoption-stamps-the-adopted-vm)). That check
can't see another installation: if one does manage the VM, it loses it (its
owner-checked calls then refuse the VM), which is why the opt-in must stay off
whenever an endpoint is shared between installations.
A clustered Provider never adopts a stamped VM, with or without the
annotation: it reports it as a previous incarnation, which you re-attach per
the ADR-0007 A6 runbook.

### Per-VM operations refuse templates and image artifacts

A `VirtualMachine` can still address a template, for example one that adoption
made from a template before this release. The vSphere `Delete`, `Power`,
`Reconfigure`, `HardwareUpgrade`, `SnapshotCreate`, `SnapshotDelete` and
`SnapshotRevert` therefore read `config.template` and `config.extraConfig`
first. They refuse a template, or an object carrying a `virtrigaud.image.*` key,
before changing anything. The refusal is `FailedPrecondition` with the reason
`VM_IS_IMAGE_ARTIFACT` and a constant message. The manager maps it to a
non-retryable `Conflict` and never counts it toward the circuit breaker. A
refused `Delete` keeps the `VirtualMachine`'s finalizer with
`Ready=False/VMIsImageArtifact` (clustered: `DeleteBlocked=True/VMIsImageArtifact`),
and a refused `Power` sets `Ready=False/VMIsImageArtifact`. Both carry a
constant message, one `Warning` event and a backoff of up to 5 minutes; detach
such a `VirtualMachine` with `virtrigaud.io/orphan-on-delete: "true"`. An
object whose configuration cannot be read is refused too, with a plain
`FailedPrecondition`. A full `Clone` is not guarded: cloning from a template is
what templates are for, and a full clone never changes its source. A linked
`Clone` of a source without a snapshot first creates a `clone-base-*` snapshot
on the source, so that step is guarded: a linked clone of a template or image
artifact is refused unless the source already has a snapshot.

`Create` and `Clone` clear every `virtrigaud.image.*` key the source carries on
the new VM, in any case and including keys this release does not write (a newer
release's after a rollback, or an admin tool's). The same test decides what is
an image artifact and what is cleared, so a VM made from a template is never
refused as one.

A refused `Delete` keeps the `VirtualMachine`'s finalizer, and the template is
never destroyed. The `VirtualMachine` stays `Terminating` and the delete is
retried every 15 seconds; the manager log shows
`Failed to delete VM from provider` with the refusal. To release it, detach
the `VirtualMachine` with `virtrigaud.io/orphan-on-delete: "true"`. The
template stays in vCenter.

### `Describe` no longer reports a transient error as "gone"

When `Describe` returns `Exists=false`, the manager clears `status.id` and calls
`Create` again. Before this change, the vSphere `Describe` returned
`Exists=false` for **any** error, including session loss, network errors,
vCenter restarts and permission changes. Now it returns `Exists=false` only for
a definite `ManagedObjectNotFound`. Other errors are returned as errors, so the
manager retries and the `VirtualMachine` keeps its `status.id`. Without this
fix, a vCenter outage would unbind every VM created before the upgrade: those
VMs have no stamp, so the re-run `Create` would be refused.

### Required vCenter privileges

VirtRigaud doesn't publish a full vCenter role yet. This change adds one
requirement to the account in the Provider's credential Secret:

| Privilege | ID | Needed for |
|---|---|---|
| Virtual machine > Change Configuration > Advanced configuration | `VirtualMachine.Config.AdvancedConfig` | **Every** `Create` and `Clone`, and adoption (`TransferOwner`). The owner stamp, and the clearing of an inherited stamp, are ExtraConfig writes. Grant it on the target VM folder and resource pool, and on the VMs adoption may take over. |

Without this privilege, creates fail with a vCenter permission error. They
don't create an unstamped VM. Deployments that already inject cloud-init
through `guestinfo` have this privilege.

Identity-safe image preparation (ADR-0009) adds no privilege: its image stamp
is an ExtraConfig entry of the import spec (this same privilege, on the import
folder), and importing an OVA, marking it as a template and destroying the
provider's own unfinished import were already part of `ImagePrepare`.

### Upgrade notes (vSphere)

- **The vCenter account needs `VirtualMachine.Config.AdvancedConfig`.** See
  [Required vCenter privileges](#required-vcenter-privileges).
- **Templates must be real vSphere templates.** A `VMImage` whose
  `templateName`, or whose prepared name, points at a regular VM now gets
  `ValidationError` instead of cloning that VM. Convert the source to a template
  (`govc vm.markastemplate`), or point the `VMImage` at a template. If two
  templates share a name, reference the right one by inventory path.
- **`VirtualMachine` names of the form `vm-<digits>` are refused.** The same
  applies to `VMImage` names used as `ImagePrepare` targets and to bare template
  names. They get `ValidationError`; rename them.
- VMs that are already bound (`status.id` set) are **unaffected**. After a VM is
  bound, every operation uses its MOID and never calls `Create` again. The only
  exception is a VM whose MOID really disappears, for example a VM that was
  unregistered and registered again. The manager then re-runs `Create`. An
  unstamped VM, created before this change, is refused; adopt it to recover. A
  stamped VM is bound again if it is still in the target folder.
- VMs that existed before this change have no owner stamp. A create that
  collides with one of them in the target folder now fails with
  `ProviderConflict` instead of taking it over. Pre-existing VMs are never
  bound automatically.
- One edge case: the provider created a VM before the upgrade, and the manager
  lost the `status.id` write for it. After the upgrade, that VM's retried create
  is refused. Adopt the VM to recover.
- The manager and the provider can be upgraded in either order, but **the
  protection needs the upgraded vSphere provider**. An older provider ignores
  `owner` and still binds by name across the whole datacenter. A newer provider
  behind an older manager never binds an existing VM, but still creates new
  ones, which are left unstamped.

## libvirt

The libvirt provider stamps the owner into the domain `<metadata>` and binds a
same-named domain only when that stamp matches the requester's UID. It rejects
names that virsh would resolve as a domain ID or UUID. For the full rules and
upgrade notes, see [`libvirt-domain-ownership.md`](libvirt-domain-ownership.md).

## Proxmox

### Why Proxmox needs a stamp

Proxmox VE gives every new VM a new VMID from `/cluster/nextid` and doesn't
keep VM names unique. Before owner stamps, that caused three problems:

- **A re-sent `Create` made a duplicate.** When the manager never learned the
  outcome of a `Create` (a timeout, a lost answer, a manager restart), sending
  it again made a second VM under a new VMID. The VM controller therefore held
  such a Proxmox VM (`Ready=False/CreateOutcomeUnknown`) until an operator
  acted.
- **A VM couldn't be found by the `VirtualMachine` it was made for.** A delete
  of a `VirtualMachine` whose `Create` outcome was unknown couldn't find the VM
  to delete.
- **Adoption couldn't tell managed VMs apart.** Adoption through one Provider
  object could take a VM that a `VirtualMachine` behind another Provider object
  already manages.

### The owner stamp

Every VM that `Create` makes, every clone that `Clone` makes for a target
`VirtualMachine` with a UID, and every VM adoption takes over (`TransferOwner`,
`phase=ready`), gets one line in its **description** (the "Notes" field in the
PVE GUI):

```
virtrigaud.owner v1 uid=<uid> namespace=<namespace> name=<name> phase=<creating|ready> sum=<16 hex digits>
```

| Field | Meaning |
|---|---|
| `uid` | The `VirtualMachine`'s UID. This is the only field that authorizes a re-bind. |
| `namespace`, `name` | The `VirtualMachine`'s namespace and name. A re-bind also requires them to match, as a consistency check. |
| `phase` | `creating` from the call that makes the VM, and `ready` once every step of the `Create` or `Clone` has succeeded. |
| `sum` | The first 16 hex digits of the SHA-256 of the rest of the line. |

To read it: `qm config <vmid> | grep virtrigaud.owner`, or open the VM's
Notes in the GUI.

**Why the description and not a tag.**

- PVE accepts `description` both in `POST /nodes/{node}/qemu` (create) and in
  `POST .../qemu/{vmid}/clone`. The stamp is therefore written by the same
  call that makes the VM. The clone endpoint takes no `tags`, so a tag would
  need a second call, and a crash in between would leave an unstamped VM.
- The UID, namespace and name go in verbatim. Tags allow only
  `[a-z0-9_][a-z0-9_+.-]*` and would need an encoding.
- A tag can be blocked by the datacenter's `user-tag-access` and
  `registered-tags` options. These options can forbid an API token to create a
  new tag, which would make every create fail. The description has no such
  policy.
- Neither field is visible to the guest. Both are copied by a PVE clone and by
  backup/restore, which is why the clone and re-bind rules below exist.

**Tamper evidence, not tamper proofing.** The line is parsed strictly: fixed
fields, in order, single spaces, a known phase, identity values in
`[A-Za-z0-9][A-Za-z0-9._-]*`, and a matching `sum`. Any line that mentions
`virtrigaud.owner` in any letter case but isn't a valid stamp makes the stamp
**unreadable**. So does a valid line that appears twice. Two valid lines with
different UIDs make it **multiple**. An edit that changes the UID therefore
never points the VM at another owner: the stamp becomes unreadable, and every
check below fails closed. The checksum catches accidental edits only. Anyone
with `VM.Config.Options` on the VM can rewrite the whole line, just as they
could edit vSphere ExtraConfig or a libvirt domain's metadata. PVE ACLs are
the trust boundary:

- Grant `VM.Config.Options` on VirtRigaud's VMs only to the provider's API
  token and to administrators you trust with every tenant's VMs. Anyone who
  can set it can claim or disown a VM.
- `VM.Audit` lets a user read the stamps, which name each VM's namespace and
  `VirtualMachine`. Grant it as you would read access to those names.

A keyed `sum` (an HMAC with a provider-held secret, binding the VMID too) was
considered and **deferred**. Deriving the key from the provider's credential
Secret would make every existing stamp unreadable at the first credential
rotation: deletes would be held and re-binds refused across the whole
Provider. A keyed stamp needs key versioning first. It doesn't change the
trust boundary above.

**Reserved.** The provider replaces every description line that mentions
`virtrigaud.owner`. No spec field reaches the description: the Proxmox
provider doesn't apply `CreateRequest.tags`, and cloud-init data goes to
`ciuser`/`sshkeys`/`ipconfigN`. An owner whose UID, namespace or name falls
outside the charset above is refused with `InvalidSpec` before any PVE call.
Kubernetes UIDs and names always fit it.

**Writes.**

- A plain create sends the stamp with `phase=ready`. The create request
  carries the whole configuration, so nothing follows it.
- A template clone, and a migration's imported-disk create, send
  `phase=creating`. When sizing, cloud-init or the disk attach has succeeded,
  the provider rewrites the stamp with `phase=ready` and reads it back.
- A clone keeps the description of its template or source VM and drops any
  owner stamp from it. PVE ignores an empty description in a clone request
  and copies the source's, so when nothing is left to send (no notes, no
  stamp) but the source has a description, the request carries the note
  `Cloned by VirtRigaud.` The provider still reads the clone's description
  back, and rewrites it if it carries a stamp it shouldn't. A clone made for
  a `VirtualMachine` without a UID (`VMClone` today, see below) is left
  unstamped.
- PVE limits a description to 8 KiB. When inherited notes and the stamp
  don't fit together, the notes are cut at a character boundary and end with
  `[notes truncated by VirtRigaud to fit the owner stamp]`. The provider logs
  a warning. The stamp is always kept.

### While PVE is still making the VM

While PVE clones or creates a VM, the VM's configuration is a temporary one:
`lock: clone` (or `lock: create`), **no name and no description**. The stamp,
sent with the request, is written only when the task ends. A lookup by name
and stamp can't see such a VM. So, for a request that carries an owner:

- **One call per owner.** The provider runs one `Create` or `Clone` per owner
  UID at a time, from start to end including any rollback. A second one
  arriving meanwhile is answered outcome-unknown and makes no PVE call.
- **The provider's own record.** Each create or clone task the provider
  starts for an owner is recorded (node, VMID and task UPID; only the VMID
  when the request's answer was lost) until the provider sees the task end. A
  `Create` or `Clone` for that owner is answered outcome-unknown while the
  task runs, or while its state or the VM at that VMID can't be read. Nothing
  is started. A record is dropped after twice the provider's task timeout (10
  minutes by default). The owner lookup, with the guard below, then decides
  alone.
- **After a provider restart.** The record is gone, and a request whose
  answer was lost never had a UPID. The owner lookup therefore also looks at
  every nameless QEMU guest locked for clone or create that isn't one of the
  provider's recorded tasks. `/cluster/resources` may list such a guest with
  an empty name or, as PVE's status daemon does, as `VM <vmid>`; both count,
  and the guest's config confirms it (a clone/create lock and no name).
  If the provider's PVE user (its API token) has a clone or create task
  running on that guest's node that isn't recorded, or the node's tasks can't
  be listed, the guest may be this owner's VM. The answer is outcome-unknown,
  never a new clone. Two kinds of guest don't hold creates:
  - a guest whose clone was started by another PVE user, for example from the
    GUI;
  - a guest on a node that is down (`/cluster/resources` status `unknown`):
    no task can run there.

What this guarantees: a re-sent `Create` or `Clone` never starts a second VM
for an owner while PVE may still be making the first one. Once that task ends,
the re-sent request finds the VM by its name and stamp (and finishes it if
needed). If the task failed, PVE removed the VM and a new one is made.

### `Create` and `Clone`: re-bind

When the request carries an owner, the provider first applies the in-flight
checks above. It then looks for a VM that is already stamped with that
owner's UID. It searches the **whole cluster** (`GET /cluster/resources?type=vm`),
because VMIDs are cluster-wide and a VM may have been migrated. It reads the
stamp of every QEMU guest with the requested name: one config read each.

| What the cluster has | Result |
|---|---|
| No VM with that name stamped with this UID | A new VM is created and stamped. Same-named VMs that are unstamped or stamped for others are left alone: PVE names aren't unique. |
| No such VM, but an unattributed clone or create is in progress (above) | Retryable, outcome unknown. Nothing is started. |
| One, `phase=ready` | Idempotent success: its id is returned and nothing is changed. The id is the bare VMID on the provider's default node, and `<node>:<vmid>` on any other node. |
| One, `phase=creating`, locked by a PVE task | Retryable, outcome unknown. The VM isn't touched. |
| One, `phase=creating`, template clone | The clone is finished: sizing and cloud-init are applied again (idempotent config updates), the stamp is set to `ready`, and its id is returned. |
| One, `phase=creating`, imported-disk create | It's left alone (outcome unknown) unless all of these hold: it's stopped, the SSH data plane is configured, the staged disk is still on the node (its path is an import stage, and the file is a regular file, not a symbolic link, with one hard link, owned by the SSH user, not group- or other-writable), and no `qm importdisk <vmid>` process of the interrupted create still runs (checked with an exact `pgrep -f` over SSH; a check that can't run counts as running). Then the half-built VM is removed, as the in-process rollback would, and built again from the staged disk. |
| One whose stamp records this UID with another namespace or name | **Refused** (`Conflict`): the stamp was edited. |
| More than one | **Refused** |
| A template stamped with this UID | **Refused** |
| A same-named VM (not a template) with an unreadable or multiple stamp | **Refused**: it might be this `VirtualMachine`'s VM with a damaged stamp. |
| The cluster list, or a same-named VM's config, can't be read | Retryable (`CREATE_NOT_PERFORMED`). The provider never creates on an unknown answer. |

A request without an owner (a manager older than this provider) creates an
unstamped VM and never re-binds, as before.

A VM renamed in PVE since its create isn't found. The next create makes a new
one.

### `ListVMs`

`ListVMs` lists every QEMU guest from `/cluster/resources?type=vm`, within the
Provider's node selector (every node when none is set). That list comes from
the cluster's replicated configuration, so it includes the VMs of a node that
is down, with status `unknown`. Each listed VM reports its stamp in
`provider_raw`:

- `owner_uid`: the stamped UID. For a stamp with several owners, the UIDs,
  comma-separated.
- `owner_stamp_state`: `unreadable` or `multiple` when the stamp can't be
  relied on. This includes a VM whose config couldn't be read, such as one on
  a down node: it's listed with `unreadable` rather than skipped.

The typed `owner_namespace` and `owner_name` stay empty: they are set by
clustered providers only. Templates are skipped, as before. Adoption skips VMs
stamped with the UID of a live `VirtualMachine`, and VMs with an
`owner_stamp_state`.

One node being down no longer fails or shortens the listing. `ListVMs` fails
as a whole only when the cluster list itself can't be read. A caller that
looks a VM up by owner must never mistake a partial list for "no such VM".

The provider's cached list of cluster nodes now expires after a minute, and
is sorted. The default node (the first, when no node selector is set) is the
same whatever order `/nodes` answers in.

### When is a VM gone?

Real PVE answers a call for a VM that isn't on the node asked with HTTP 500
`Configuration file 'nodes/<node>/qemu-server/<vmid>.conf' does not exist`,
not 404. It gives the same answer for a VM that was deleted and for one that
moved to another node. PVE puts the message in the reason phrase of the HTTP
status line, with a body of `{"data":null}`; some answers carry it in the
body. The provider reads both, here and wherever it looks at a PVE error's
text, such as "already exists". It maps that answer, and a 404, to "not on
this node". It treats the VM as **gone** only when the VMID is also absent from
`/cluster/resources` **and** free according to `GET /cluster/nextid?vmid=<id>`.
That second check doesn't depend on the token's permissions. This applies to:

- `Describe`: `Exists=false` only for a gone VM. A VM on another node is a
  retryable error. `Exists=false` would make the manager create the VM again.
  A VM at the VMID that isn't the requester's is never `Exists=false` either:
  it's answered `VM_NOT_OWNED` (see
  [per-VM calls](#per-vm-calls-describe-power-reconfigure-snapshots-disks-and-clone-sources)).
- `Delete`: success only for a gone VM. A VM on another node is a retryable
  error, and the finalizer is kept.
- The `Create`/`Clone` rollback: "nothing remains" only for a gone VM.
- The owner lookup and `ListVMs`: a VM is skipped only when it's gone.
- The `Clone` source: `NotFound` only when it's gone.

### `Delete`

When `DeleteRequest.owner` carries a UID, the provider reads the VM's stamp
before it stops or destroys anything:

| The VM at that VMID | Result |
|---|---|
| Stamped with the request's UID (`creating` or `ready`) | Deleted |
| Unstamped (created before this release, adopted, or a clone without a target UID) **and** named like the owner (`DeleteRequest.owner.name`, the name `Create` gives a VM) | Deleted, as before |
| Unstamped with any other name: its VMID was reused by an unrelated VM | `NotFound` + `VM_NOT_OWNED`. The VM is **never** stopped or destroyed. |
| Stamped with another UID: this `VirtualMachine`'s VM is gone and its VMID was reused | `NotFound` + `VM_NOT_OWNED`, never stopped or destroyed |
| A template (with or without an owner on the request) | `FailedPrecondition` + `VM_IS_IMAGE_ARTIFACT`, never destroyed |
| Unreadable or multiple stamp | `FailedPrecondition` + `OWNER_STAMP_UNREADABLE`. Nothing is destroyed, and the delete is held until the stamp is fixed or the VM is removed by hand. |
| Its config can't be read | Retryable, nothing destroyed |
| Not on the node asked | Deleted as gone only when the VMID is free cluster-wide; otherwise retryable |

The name rule applies **only to a `Delete` that carries an owner**. An
unstamped VM is named like the owner when its PVE name is the owner's name, or
when the adoption flow would name its `VirtualMachine` after it
(`contracts.AdoptedVMName`, shared with the adoption controller): the PVE name
lower-cased, with `_`, `.` and spaces turned into `-`, other characters
dropped, cut to 63 characters — or `vm-<vmid>` when nothing is left. For
example PVE `Web_01` matches the owner `web-01`. Any other name is `NotFound`.

Without an owner on the request, `Delete` is unchanged, except for the gone
check above. The manager sends `DeleteRequest.owner` on every delete of a
VirtualMachine with a UID — clustered and single-host, adopted VMs and clone
targets included; nothing a tenant can edit turns the check off. Adoption
stamps the VM it adopts with the adopting `VirtualMachine` (see
[Adoption stamps the adopted VM](#adoption-stamps-the-adopted-vm)), so its
`Delete` removes it. A VM adopted by an earlier release that still carries a
previous owner's stamp (a `VirtualMachine` detached with
`virtrigaud.io/orphan-on-delete`, then adopted again), or one adopted through a
provider that doesn't report `supportsOwnerTransfer`, is answered `NotFound` +
`VM_NOT_OWNED`: it is left in place, and the manager releases the
`VirtualMachine` with a `VMLeftOnHypervisor` Warning event naming the VM's
provider id (and an error-level manager log line with the id, the Provider and
the VirtualMachine; metric reason `vm-not-owned`). That is the fail-safe
direction: without the owner, a reused VMID could make the delete destroy an
unrelated VM. See [VMs adopted before this release](#vms-adopted-before-this-release)
to fix such a VM.

A template at the VMID is never deleted, with or without an owner on the
request: `FailedPrecondition` + `VM_IS_IMAGE_ARTIFACT`, which the manager holds
as `Ready=False/VMIsImageArtifact` until the VirtualMachine is detached. A stamp
that cannot be read or records more than one owner is `FailedPrecondition` +
`OWNER_STAMP_UNREADABLE`, held as `Ready=False/OwnerStampUnreadable` until an
administrator fixes the stamp or removes the VM.

`status.id` and `status.boundProvider` are what bind a VirtualMachine to a
hypervisor VM, so write access to `virtualmachines/status` must stay with the
platform administrators and the manager: the tenant-facing roles (including
the `virtualmachine-editor-role` helper) grant only `get` on it.

### Per-VM calls: `Describe`, `Power`, `Reconfigure`, snapshots, disks and clone sources

A VM deleted outside VirtRigaud frees its VMID, and PVE may hand that VMID to
the next VM anyone creates. Before this check, a `VirtualMachine` whose
`status.id` still recorded the VMID read that new VM's state, and could power
it off, resize it, snapshot it or revert it. A `VMMigration` or `VMClone` of
that `VirtualMachine` read and copied the other VM's disks.

The manager now sends the `VirtualMachine`'s identity (UID, namespace, name) as
`owner` on every per-VM request, single-host included: `Describe`, `Power`,
`Reconfigure`, `SnapshotCreate`, `SnapshotDelete`, `SnapshotRevert`,
`GetDiskInfo`, `ExportDisk`, and the source of a `Clone`
(`CloneRequest.source_owner`). The Proxmox provider reads the VM's config and
checks its stamp **before** it reads anything else of the VM, copies its disks
or changes it. It applies the same rule as `Delete`:

| The VM at that VMID | Result |
|---|---|
| Stamped with the request's UID (`creating` or `ready`) | The call goes on |
| Unstamped (created before owner stamps, adopted, or a clone without a target UID) **and** named like the owner (the owner's name, or the name adoption gave it; see `Delete`) | The call goes on, as before |
| Unstamped with any other name: its VMID was reused by an unrelated VM | `NotFound` + `VM_NOT_OWNED`. Nothing of the VM is read or changed. |
| Stamped with another UID: its VMID was reused by another `VirtualMachine`'s VM | `NotFound` + `VM_NOT_OWNED`. Nothing of the VM is read or changed. |
| Unreadable or multiple stamp | `FailedPrecondition` + `OWNER_STAMP_UNREADABLE`. Nothing is done. |
| Its config can't be read | Retryable, nothing done. `Reconfigure` and `GetDiskInfo` keep their historical `Internal` answer for this; a `Clone` answers `Unavailable` + `CREATE_NOT_PERFORMED` + `VM_OPERATION_FAILED` (nothing was cloned, see the error classes below). |
| Not on the node asked | Handled by the call as before: `Describe` reports it gone only when the VMID is free cluster-wide (see [When is a VM gone?](#when-is-a-vm-gone)) |

- **A template is checked like any VM.** Only `Delete` refuses templates
  outright (`VM_IS_IMAGE_ARTIFACT`); these calls keep their handling, plus the
  owner check.
- **`SnapshotDelete` checks the VM on its actual node.** It resolves the node
  from `/cluster/resources` (an HA move changes it), as before, and checks the
  stamp of the VM there.
- **API load: every checked call except `Reconfigure` and `GetDiskInfo` costs
  one extra `GET /nodes/{node}/qemu/{vmid}/config`.** `Reconfigure` and
  `GetDiskInfo` already read the VM's config and check the stamp in it. The
  extra read is made only when the request carries an owner. Size the PVE API
  rate limits and the token's request budget with it: the VM controller sends
  a `Describe` on every reconcile of every bound VM.
- **`ExportDisk` checks before either backend path, and again on the config it
  copies from.** The S3 and NFS exports run on the node over SSH; a refused
  export reaches neither, so no disk is read or copied. Each path's own disk
  read (the config that yields the volume it copies) carries the owner too, so
  a VM that took the VMID over after the first check is not copied either.
- **A `Clone` checks its source before anything is cloned:** after the
  re-bind lookup, which never reads the source, and before the VMID allocation
  and the clone task. A retried `Clone` whose target already exists is
  therefore answered with it even if the source's VMID was reused since. The
  clone *target* (`target_vm`) is stamped as before.
- **`Describe` doesn't answer `Exists=false` for a VM that isn't the
  requester's.** On a single-host Provider, `Exists=false` makes the manager
  create the VM again. The provider can't prove the requester's own VM is gone:
  an adopted VM can still carry an earlier owner's stamp, and an unstamped VM
  may have been renamed in PVE. So it answers `VM_NOT_OWNED` and returns none of
  the VM's state.

What the manager does with each answer:

| Call | `VM_NOT_OWNED` | `OWNER_STAMP_UNREADABLE` |
|---|---|---|
| `Describe`, `Power`, `Reconfigure` (VM controller) | `Ready=False/VMNotOwned`. `status.id` is kept, and **no VM is created in its place**. | `Ready=False/OwnerStampUnreadable` |
| `SnapshotDelete` (`VMSnapshot` finalizer, migration cleanup) | Released, since it can't be deleted through a VM that isn't the `VirtualMachine`'s. It is **not** reported as already gone: a `SnapshotLeftOnHypervisor` Warning says the snapshot may be left on the VM at that id (if that VM is the `VirtualMachine`'s after all: renamed in PVE, or the `VirtualMachine` re-created under a new UID), and the manager logs the VM and snapshot ids. | Held and retried, like any failed snapshot delete |
| `SnapshotCreate` (`VMSnapshot`) | The `VMSnapshot` fails | The same |
| `VMMigration` source: the `powerOffBeforeMigration` `Describe`, the source snapshot, the disk-size checks' `GetDiskInfo`, `ExportDisk` | The migration **fails** with a constant message, before anything is exported. The power-off `Describe` now runs before the source `VirtualMachine`'s `spec.powerState` is set to `Off`, so a refused source's spec isn't changed either. | The same |
| `VMClone` source: the disk-size check's `GetDiskInfo`, `Clone` | The clone **fails** (`Failed=True`, reason `VMNotOwned`) with a constant message and one `Warning` event. Nothing was cloned. | The same, reason `OwnerStampUnreadable` |

The VM controller's two holds use a constant message, record one `Warning`
event when they begin, write the status only when it changes, and re-check
with a backoff of up to 5 minutes. The metric reasons are `vm-not-owned` and
`owner-stamp-unreadable`. A migration or clone is failed rather than held: it
is a one-shot copy, and the refusal doesn't change on a retry. Its message
names neither the VM at the VMID nor its owner; the provider's answer is in
the manager log.

To resolve `VMNotOwned`:

- **The VM at the VMID really is another VM** (the common case): delete the
  `VirtualMachine`. Its `Delete` is answered `VM_NOT_OWNED`, so it is released
  and the other VM is left alone, with a `VMLeftOnHypervisor` event. Create
  the `VirtualMachine` again to get a new VM.
- **The VM is this `VirtualMachine`'s, renamed in PVE** (it is unstamped):
  rename it back; the next `Describe` accepts it. Or delete the
  `VirtualMachine` (the VM is left in place) and let adoption take the VM
  again: through a Proxmox provider of this release (it reports
  `supportsOwnerTransfer`), adoption stamps it with its new `VirtualMachine`
  (see [Adoption stamps the adopted VM](#adoption-stamps-the-adopted-vm)).
  An unstamped VM needs only `virtrigaud.io/adopt-vms` on the Provider.
- **The VM is this `VirtualMachine`'s, adopted by an earlier release while it
  carried an earlier owner's stamp** (see
  [VMs adopted before this release](#vms-adopted-before-this-release)): remove
  the `virtrigaud.owner` line from the VM's Notes in PVE; the VM is then
  unstamped and accepted while its PVE name matches. Or delete (or detach with
  `virtrigaud.io/orphan-on-delete: "true"`) the `VirtualMachine` and let
  adoption take the VM again. That re-stamps it only when all of these hold:
  the provider reports `supportsOwnerTransfer`; the Provider has
  `virtrigaud.io/adopt-vms` **and** `virtrigaud.io/adopt-stamped-vms: "true"`;
  no `VirtualMachine` in the cluster has the stamped UID (adoption checks this
  uncached; the deleted `VirtualMachine`'s own UID doesn't count once it is
  gone); and the VM's PVE name is the one adoption gives the new
  `VirtualMachine`. Otherwise remove the stamp line by hand.
- **`OwnerStampUnreadable`**: adoption never takes such a VM. An administrator
  fixes or removes the `virtrigaud.owner` line(s) in the VM's Notes, or
  removes the VM.

A VM adopted by this release is stamped with its `VirtualMachine` before it
is bound, so adoption no longer leads to this hold.

The provider log names the VMID, the requester and, for a stamped VM, the
owner recorded on it: search for `Refusing the call`.

A request without an owner, from a manager older than this check, is acted on
as before. So is any request through an older Proxmox provider, which ignores
the owner. **The protection needs both the manager and the Proxmox provider of
this release.** vSphere, single-host libvirt and the mock provider ignore the
owner on all of these calls, including `source_owner` (single-host libvirt
reads it only on its clustered path).

### `Create` and `Clone` error classes

The VM controller decides from a failed `Create` whether a VM may exist. The
Proxmox provider answers every `Create` and `Clone` failure with one of these.
`CREATE_NOT_PERFORMED` is a `google.rpc.ErrorInfo` reason in the
`provider.virtrigaud.io` domain (`contracts.CreateNotPerformedReason`). The
manager's transport marks an answer that carries it
(`contracts.IsCreateNotPerformed`), whatever its code.

| Answer | Meaning | When |
|---|---|---|
| `InvalidSpec` (`InvalidArgument`) | Nothing was done; the request is wrong | An owner that can't be stamped, a template id that isn't a number, a template that isn't on the node, a malformed clone source reference, or PVE's own JSON answer of HTTP 400 to the create or clone request. Its message quotes PVE's error text, cut to 512 bytes. |
| `NotFound` | Nothing was done | `Clone` only: the source VM is gone (see above) |
| `PermissionDenied` | Nothing was done | PVE's own JSON answer of HTTP 401 or 403 to the create or clone request |
| `AlreadyExists` (`Conflict`) | Nothing was done | The re-bind refusals above |
| `Unavailable` + `CREATE_NOT_PERFORMED` only | **Nothing was made**, provider-wide failure; retry is safe. Counts toward the manager's circuit breaker, so a PVE outage opens it. | Client not configured, node discovery, `/cluster/nextid` (no longer replaced by a time-derived VMID), the cluster list of the owner lookup, the migration SSH data plane not configured |
| `Unavailable` + `CREATE_NOT_PERFORMED` + `VM_OPERATION_FAILED` | **Nothing was made**, failure scoped to one VM; retry is safe. Kept out of the circuit breaker. | Reading the template, the clone source or a same-named VM's stamp. A 5xx "already exists" for a VMID that another VM has certainly taken since `/cluster/nextid`: that VM is readable, carries no stamp of this owner, and isn't still being made. A step after the VM was made failed, and that VM, verifiably this request's, was destroyed again (see below). |
| `Unavailable` **without** `CREATE_NOT_PERFORMED` | **Outcome unknown**: a VM may exist | A transport error. A 5xx (other than the "already exists" case above). Any 4xx other than 400/401/403, or any 4xx whose body isn't PVE's JSON (a proxy's page). A task that couldn't be followed to its end. A failed step whose VM couldn't be removed. An SSH command of the imported-disk create whose connection failed or broke (ssh exit 255) or that was cut off: the shell is never removed under it. The in-flight cases above. |

A rollback destroys a VM only when it is verifiably this request's. Its owner
stamp records this request's owner (or, for a request without an owner, it's
unstamped and has the requested name). It isn't locked by a PVE task, and it's
stopped. A VM stamped `ready` is destroyed only by the call that wrote that
stamp. After the destroy, the VMID must be free cluster-wide.

Wire messages carry the category of the failure, not PVE's error detail. The
detail can include the PVE address or the node's command output, so the
provider log records it. The one exception is the text of an HTTP 400.

Before this change, any error from PVE's clone call was `InvalidSpec`,
including a timeout or a 5xx after PVE had accepted the clone. A VMID collision
was a non-retryable `AlreadyExists`.

### Existing VMs (created before this release)

Proxmox VMs created before this release carry no stamp. They:

- **stay bound and managed as before** while their PVE name is the
  `VirtualMachine`'s name or the name adoption gave it. Every operation after
  `Create` uses `status.id`. One renamed in PVE is answered `VM_NOT_OWNED` by
  `Describe`, `Power`, `Reconfigure` and the snapshot calls, and is held as
  `Ready=False/VMNotOwned` (see
  [per-VM calls](#per-vm-calls-describe-power-reconfigure-snapshots-disks-and-clone-sources));
- **are never re-bound.** A `Create` for their `VirtualMachine` doesn't find
  them by owner. This only matters for a `VirtualMachine` that never recorded
  its `status.id`, which a re-sent `Create` treats as having no VM;
- **are reported by `ListVMs` without `owner_uid`.** The adoption rules for
  unstamped VMs are unchanged, and the manager can't look them up by owner;
- **are deleted as before** when the request carries no owner, or when their
  PVE name is the `VirtualMachine`'s name or the name adoption gave it (see
  `Delete` above).

The provider doesn't stamp existing VMs by itself. A VM adoption takes over
is stamped through `TransferOwner` (compare-and-swap on the config `digest`,
see [Adoption stamps the adopted VM](#adoption-stamps-the-adopted-vm)); a VM
that adoption bound before this release keeps the stamp it had.

### Limits

- `VMClone` sends no target UID today (the target `VirtualMachine` is created
  after the clone), so `VMClone` clones are unstamped. They aren't re-bound by
  a re-sent `Clone`, and a re-sent `Clone` can make a second clone.
- The owner check and the call are two PVE requests. A VMID freed and reused
  between them isn't caught. PVE's config `digest` parameter could close this
  for `Reconfigure` (its config `PUT` and disk `resize`), as `TransferOwner`
  already uses it; `Reconfigure` doesn't use it yet. `Power` and the snapshot
  calls have no such parameter.
- **The legacy name rule can be claimed by anyone who can create a VM.** An
  unstamped VM named like the owner is treated as the owner's (see the table
  above), and the provider can't tell whether the `VirtualMachine`'s own VM
  was stamped. So once a `VirtualMachine`'s VM is removed, anyone who can
  create a VM at the freed VMID — `VM.Allocate` on a pool or path it can be
  created in, such as a shared pool — can create an unstamped VM with the
  `VirtualMachine`'s name there, and the `VirtualMachine` then reads, powers,
  resizes, snapshots and copies it as its own. A VM at that VMID stamped for
  anyone else is refused. Grant `VM.Allocate` on the pools and paths where
  VirtRigaud's VMs live only as you would `VM.Config.Options` (see
  [The owner stamp](#the-owner-stamp-1)).
- **Give each Provider its own PVE API token.** The restart guard tells this
  provider's clone and create tasks from other users' by the PVE user that
  ran them. Two Provider objects, or two provider replicas, that share a token
  don't share their in-flight records. Each treats the other's unrecorded
  tasks as possibly its own, so they hold each other's creates (never
  duplicate) until those tasks end. A token per Provider also keeps
  `VM.Config.Options` and audit trails per Provider.
