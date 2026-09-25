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
| Virtual machine > Change Configuration > Advanced configuration | `VirtualMachine.Config.AdvancedConfig` | **Every** `Create` and `Clone`. The owner stamp, and the clearing of an inherited stamp, are ExtraConfig writes. Grant it on the target VM folder and resource pool. |

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
