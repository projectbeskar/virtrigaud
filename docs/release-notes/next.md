<!--
Draft release notes for the release that follows v0.3.11. Paste the body below
(from "## Breaking changes" onward) into the GitHub release. Fill in the version
number in the title before publishing; see docs/upgrading.md for the full upgrade
guide these notes link to, and CHANGELOG.md for the complete, dated changelog.
-->

This release is a security-hardening release: a full codebase security review
found and fixed several cross-tenant and privilege-escalation issues in the
vSphere and libvirt providers, cross-namespace clone/migration targets, and VM
provider binding. **Several fixes are breaking or change default behavior.**
Read the upgrade guide before upgrading:
**[Upgrading from v0.3.11](docs/upgrading.md)**.

## Breaking changes

- **A `Provider`, `VMClass` or `VMImage` in another namespace can only be used if its new `spec.consumerNamespaceSelector` selects the referencing namespace** (unset = own namespace only, `{}` = all namespaces). Set it on shared objects **after applying the new CRDs and before rolling the manager** (the field does not exist until the CRDs are upgraded); otherwise VMs using them fail closed with `ConsumerNotAllowed`. The manager's readiness now also checks that the Provider, VMClass and VMImage CRDs have the field. → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **`VirtualMachine.spec.providerRef` is now immutable once a VM is bound.** The
  old "re-point to a missing Provider, then delete" un-adopt workaround no longer
  works — use the new `virtrigaud.io/orphan-on-delete: "true"` annotation instead.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **Cross-namespace `VMClone`/`VMMigration` targets now require a grant** on the
  target namespace (`infra.virtrigaud.io/allowed-source-namespaces`). Objects in
  flight during the upgrade need the annotation applied first.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **vSphere `Create`/`Clone` fail closed on VM ownership.** Requires the new
  vCenter privilege `VirtualMachine.Config.AdvancedConfig`; `vm-<digits>` names
  are refused; `VMImage.templateName` must name a real vSphere template.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **libvirt VMs created without `spec.userData` no longer get a default `ubuntu`
  user, SSH key, or passwordless sudo.** Anyone relying on that default login
  must supply their own `spec.userData`. Existing VMs still carry the old key
  in-guest until removed by hand.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **libvirt base images are now confined to allowed directories and always
  copied**, never attached in place. Set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` if
  images live outside `/var/lib/libvirt/images`. VMs from an earlier release may
  share one disk file — check `virsh domblklist --details` before deleting a
  pre-upgrade VM.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)

- **vSphere attaches an imported disk in place only when it is the VM's own
  verified migration disk, and migrations land per namespace.** A migration now
  lands at `[<datastore>] <namespace>_<vm name>-migrated/...`, so a same-named VM
  in another namespace can no longer reach, overwrite or delete it.
  `spec.importedDisk.path` naming any other datastore file (another VM's disk) is
  refused before anything is created; a landing disk another VM uses is refused
  too. Deleting a VM no longer deletes a disk it does not own. **A vSphere
  migration landed before the upgrade whose target VM was not yet created must be
  run again.** The vCenter account needs `System.Read` on the root folder,
  propagated.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)

- **libvirt linked clones are disabled.** A `VMClone` with
  `spec.options.type: LinkedClone` through a libvirt Provider fails with
  `LinkedCloneUnsupported`: a linked clone reads its source VM's live disk, and
  powering the source on corrupts the clone. Use `FullClone` (the default).
  Existing linked clones keep working, but their source cannot be deleted,
  reverted or snapshotted while the clones exist — keep it powered off.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)
- **libvirt `Reconfigure` no longer reports success for changes it did not
  apply.** A change is applied live and persisted, persisted and reported as
  restart required (`Reconfiguring=True/RestartRequired`: power-cycle the VM),
  or it fails (`Reconfiguring=False/ProviderError`). A memory shrink of a
  running VM always needs a restart. `status.powerState` can now be
  `Suspended` or `Unknown`; such a VM is not resized or powered on (a
  suspended one is powered off if its spec says `Off`). On clustered
  Providers, resizes are held until the libvirt provider reports
  `supportsHonestReconfigure` — roll the providers promptly after the
  manager.
  → [Upgrade guide](docs/upgrading.md#breaking-changes),
  [`docs/reconfigure-results.md`](docs/reconfigure-results.md)
- **`powerOn` is honored: a clone or migrated VM stays powered off unless
  `powerOn: true`.** `VMClone` `spec.options.powerOn` and `VMMigration`
  `spec.target.powerOn` (default `false`) were never read, so every clone and
  migrated VM came up running. The produced VirtualMachine's `spec.powerState`
  is now `On` only for `powerOn: true`. A target with `powerOn: false` is never
  booted, on any provider.
  → [Upgrade guide](docs/upgrading.md#breaking-changes)

- **Deleting a `Provider` that is still in use now waits instead of orphaning
  its VMs** ([ADR-0010](docs/adr/0010-provider-in-use-finalizer.md)). A new
  finalizer, `provider.infra.virtrigaud.io/in-use`, keeps a `Provider` being
  deleted — and its runtime — while any `VirtualMachine` in any namespace (or a
  `VMSnapshot`, in-flight `VMClone` or `VMMigration`) still needs it, so they are
  deleted through it; before, every one of them was released without a provider
  `Delete` and its hypervisor VM kept running, unmanaged. It shows
  `DeleteBlocked=True/ProviderInUse` with counts per kind and namespace.
  Deleting a namespace still deletes the provider runtime's pods with it, so a
  VM whose delete misses the pod's termination window is held, and the
  namespace stays `Terminating` until an administrator acts. Delete VMs before
  their `Provider` or namespace; `virtrigaud.io/force-delete: "true"` on the
  `Provider` releases it at once (`Warning` event `ProviderForceDeleted`).
  → [Upgrade guide](docs/upgrading.md#breaking-changes),
  [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#deleting-a-provider)

See the full breaking-change table, required upgrade order (CRDs → manager →
providers), and rollback caveats in
**[docs/upgrading.md](docs/upgrading.md)**.

## Highlights

### Security

- Every pod's writable `/tmp` emptyDir now has a `sizeLimit`; a pod that writes
  past it is evicted. The limit bounds the pod's `/tmp`, but does not reserve
  node storage: a node with less free ephemeral storage than the limit can still
  fill up first, so size it to your nodes. Defaults:
  - vSphere providers: `512Gi`. vSphere stages OVA downloads and about twice a
    migrated disk there.
  - libvirt, Proxmox and the other types: `1Gi`.
  - the manager: `128Mi`.

  Change one Provider's limit with the annotation
  `infra.virtrigaud.io/tmp-size-limit`, or all of them with
  `manager.providerTmpSizeLimit`. A cluster administrator can cap what the
  annotation may ask for with `manager.providerTmpSizeLimitMax`. The manager
  upgrade rolls every provider pod once.
  → [Upgrade guide](docs/upgrading.md),
  [chart README](charts/virtrigaud/README.md#tmp-size-limits)
- **Opt-in administrator disk-size limit.** By default a tenant can ask for
  disks up to the CRD bound of 64Ti each and fill a shared datastore or pool.
  Set `manager.maxDiskSize` (the manager's `--max-disk-size`, e.g. `500Gi`) to
  cap every disk VirtRigaud creates or grows: VMs that would create a larger
  disk are held (`DiskSizeExceedsLimit`), a Reconfigure never grows a disk
  beyond it, and clones and migrations are checked by the real sizes of the
  source's disks as its provider reports them (an unknown size refuses them);
  the target provider also enforces the limit on the disk it is about to land
  at the import, so a replaced staged object cannot get past it.
  VMs that already have larger disks keep running and are never shrunk. An
  image or template larger than the class size is not covered. A Provider's
  owner can lower the limit for that Provider with the annotation
  `infra.virtrigaud.io/max-disk-size` (never raise it). With webhooks enabled,
  VirtualMachine and VMClass writes above the limit are also refused at
  admission. Nothing changes unless it is set; roll the provider images with
  the manager (`GetDiskInfo` now lists every disk's size).
  → [Upgrade guide](docs/upgrading.md),
  [chart README](charts/virtrigaud/README.md#disk-size-limit)
- Editing a waiting `VMClone`'s metadata (labels, annotations), or a
  `VMSnapshot` whose delete is held, no longer re-runs it at once. A tenant
  re-annotating one in a loop drove one provider call per edit (`Clone`, a
  multi-host scan, or `SnapshotDelete`) and bypassed the wait's backoff. The
  `VMSnapshot` force-delete annotation still releases a held delete at once.
- vSphere VM adoption (`virtrigaud.io/adopt-vms`) no longer takes templates.
  It could turn a prepared image (ADR-0009) or any other template into a
  tenant's `VirtualMachine`, whose delete or resize then destroyed or changed
  the shared image for every consumer. The vSphere `ListVMs` now leaves out
  templates, objects with a `virtrigaud.image.*` key and VMs whose owner stamp
  cannot be read. It also reports each VM's owner UID, so adoption skips a VM
  that a live `VirtualMachine` owns through another Provider object. A legacy
  OVA import is marked as an image from the moment it exists. The vSphere
  `Delete`, `Power`, `Reconfigure`, `HardwareUpgrade` and snapshot calls now
  refuse a template or image artifact (`VM_IS_IMAGE_ARTIFACT`) before changing
  anything, so a `VirtualMachine` adopted from a template before the upgrade
  can no longer destroy it; nor does a linked clone snapshot one. `Create` and
  `Clone` clear every `virtrigaud.image.*` key the source carries, so a VM
  made from a template is never refused as one. Check for such `VirtualMachine`s, and for adopted
  duplicates of a VM managed under another Provider object, before upgrading
  (→ [Upgrade guide](docs/upgrading.md),
  [`docs/vm-ownership.md`](docs/vm-ownership.md#adoption-what-listvms-offers)).
- **Behaviour change:** adoption no longer adopts a VM whose owner stamp names
  no `VirtualMachine` in this cluster. That UID may belong to a live
  `VirtualMachine` of another VirtRigaud installation (or of this cluster
  before a restore) managing the same vCenter or host. Such VMs are counted in
  the Provider's adoption status. Re-adopting a VM detached with
  `orphan-on-delete` (it keeps the deleted `VirtualMachine`'s stamp) now needs
  `virtrigaud.io/adopt-stamped-vms: "true"` on the single-host Provider
  (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#owner-stamps-are-scoped-to-one-cluster)).
- Single-host libvirt adoption no longer takes a domain whose owner stamp
  cannot be read or names two owners: the single-host `ListVMs` reports
  `owner_stamp_state` as the clustered one does. Adoption also skips any VM a
  provider marks with `provider_raw["artifact"]`, a backstop for third-party
  providers that list templates.
- vSphere and libvirt `Create` no longer silently bind to (and libvirt/vSphere
  `Clone` no longer silently clones) a pre-existing VM/domain/template they
  don't own — both now stamp and check ownership, and fail closed
  (`ProviderConflict`) on anything they can't prove is theirs.
- Every libvirt command sent to the hypervisor over SSH is now shell-quoted,
  closing a command-injection path through domain names, image paths, and
  snapshot descriptions — and, as a side effect, guest-agent calls (IP
  discovery, time sync, in-guest disk grow) now actually work over SSH for the
  first time.
- libvirt domain XML generation escapes every user-derived value, closing an
  XML-injection path that could splice a sibling `<disk>` element pointing at
  host block storage into a VM's definition.
- New libvirt VMs get a per-tenant, collision-proof domain name
  (`<namespace>.<name>`) instead of the bare VirtualMachine name, closing a
  same-host cross-namespace name-squatting and disk-overwrite race.
- Cross-namespace `VMClone`/`VMMigration` targets, and a migration's source
  Provider, are now scoped to what the requester actually owns.
- `VMImage` prepare state is recorded per Provider identity
  (`status.providerStatus["<namespace>/<name>"]`, with the Provider's UID and
  its own prepare task), so a shared `VMImage` prepared through one namespace's
  Provider never lets a same-named Provider in another namespace skip its own
  prepare or create from the other's template, and a prepare task is only ever
  polled through the Provider that started it. Existing state is migrated on
  first use (→ [`docs/image-preparation.md`](docs/image-preparation.md#prepare-state-is-per-provider)).
- libvirt: **deleting a running linked clone no longer deletes its source VM's
  disk.** Delete removes only the VM's own disk files inside the storage pool
  (and its own external-snapshot chain, never a symlink's target), and a source
  VM whose disk a linked clone still uses can no longer be deleted, reverted or
  snapshotted until the clone is gone (`DeleteBlocked`); powering it on sets a
  `LinkedClonesDependOnDisk` warning. A `VMSnapshot` whose provider delete fails
  keeps its finalizer and is retried (`force-delete` to drop it), also when no
  provider call can be made for its VM. VM disks are created
  `0640 libvirt-qemu:kvm` (never `chmod`'ed as root; `chown -h`), no longer
  world-writable — the provider's SSH user needs the `kvm` group, `root`, or
  passwordless `sudo` for the exact `qemu-img info` reads (a regex rule) — disks are written in a private
  directory and renamed into place (a symbolic link at a disk's name is
  refused), the s3 export's temporary copy is private and always removed, and a
  UEFI clone's varstore is never copied through a symlink
  (→ [`docs/libvirt-clones.md`](docs/libvirt-clones.md)).
- libvirt, clustered Providers: a `VMImage` whose host path is another VM's
  live disk on **any** host of the Provider is refused (it used to be checked
  on the landing host only), and fails closed while a host cannot be checked;
  a refused image path answers the same whether it does not exist, is not
  allowed, is in use or has a header it may not have, which narrows what a
  tenant learns about the hosts' files (an accepted path still shows an image
  is there). **With a shared pool, set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` to a
  catalog directory apart from the pool**: the default image directory is
  the default pool's, and any unused, non-reserved file in it is copyable by
  any tenant of the Provider. Clustered disk writes,
  domain defines and deletes of one domain name are serialized inside the
  provider (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#shared-storage-the-cluster-wide-disk-guard-a61)).
- The manager's webhook and metrics servers pin an explicit TLS 1.2 floor.
- Optional, opt-in `NetworkPolicy` templates for the manager and provider pods
  (`networkPolicy.enabled`, default off).
- Provider pods run under a dedicated, token-less ServiceAccount instead of the
  namespace `default` SA.

### Clustered providers (experimental, opt-in)

- `topology: cluster` libvirt providers can now be scheduled, created,
  described, deleted, powered, reconfigured, snapshotted, cloned (full clones,
  onto the source VM's own host) and exported (s3 / nfs, as a migration source) across a
  `Host`/`HostPool` inventory (ADR-0007). Every per-VM call is routed to the
  VM's host and checked against the VM's owner stamp. A clone is admitted
  against its source host's free capacity like any create (it waits,
  `Unschedulable`, when it does not fit, and never moves to another host), and
  a clone that fails for good removes the target VM it created. VMs are
  listed across every host (each tagged with its host; a host that cannot be
  listed is reported as unknown, never as empty) and unstamped domains can be
  adopted, keyed on (host, domain): the adopted VM's domain is stamped with its
  new owner (a serialized check-and-set with read-back) and the VM is bound to
  its host and counted in its capacity. A domain left by a deleted or restored
  VirtualMachine is not adopted; it is reported for the A6 re-attach runbook,
  and nothing new is adopted while a host cannot be listed. A clustered
  provider runs one replica with the `Recreate` strategy, so its rollout is a
  short outage. Still experimental — migration into a clustered provider and
  host-to-host migration are not implemented yet.
- The clustered scheduler subtracts what each host already holds: every VM
  bound to it, pending on it, or being deleted from it, in any namespace, at
  its admitted size. It also keeps concurrent creates and resizes from booking
  the same capacity or breaking hard anti-affinity. A VM no host can take
  reports `Placed=False/Unschedulable` with its own request (no other tenant's
  figures), and is retried with a backoff of up to 2 minutes. A resize-up is
  sent only if it fits on the VM's host (`Reconfiguring=False/
  InsufficientHostCapacity` otherwise); a pending VM's size is frozen until it
  is created; and a consumer in another namespace may orphan-on-delete a
  clustered VM only if the Provider allows it. New administrator gauges:
  `virtrigaud_host_committed_cpu` and `virtrigaud_host_committed_memory_mib`.
  There is no per-tenant quota on a shared clustered Provider yet
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#committed-capacity)).
- Clustered disks on a shared pool are protected across hosts (ADR-0007
  A6.1). A create or clone that finds a file where the VM's disk goes writes
  it only when no domain on **any** host of the Provider uses it (every name
  the disk may have had is checked), a host-path base image is checked on
  every host too, and a delete first checks every other host. A host that
  cannot be checked — including a `Host` that exists but cannot be routed —
  makes the operation fail closed (never counted by the circuit breaker) — so
  **clustered deletes wait while any host of the Provider is unreachable**:
  the VM shows `DeleteBlocked=True` (`HostUnreachable` or `DiskCheckFailed`)
  and `Ready=False/DeleteBlocked`, and is retried with a backoff from 15 s
  doubling to 5 min; `force-delete` and `orphan-on-delete` still release it at
  once. A VirtualMachine re-created under the name of a previous incarnation
  (after `orphan-on-delete`, a force-delete or a backup restore) is held as
  `Placed=False/RestorePending` on its pending host instead of making a second
  domain; an administrator re-attaches or removes the old domain with the A6
  runbook. A VM whose own domain turns up on another host (lost placement
  record) is held the same way, and deleting it is held until its pending host
  is moved. **Before deleting a `Host` to release held deletes, fence it**
  (power it off or revoke its access to the export); mount a shared pool only
  on the hosts of one clustered Provider, at the same path, with NFS locking
  enabled (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#shared-storage-the-cluster-wide-disk-guard-a61)).
- A restored or re-created clustered VirtualMachine is held **before** it is
  scheduled (ADR-0007 A6.2). The manager writes the restore marker
  `infra.virtrigaud.io/placement-uid` (the VM's own UID) on every clustered
  VM; a VM restored under a new UID, whose marker names another UID, waits as
  `Placed=False/RestorePending` and nothing is created. Before a clustered VM
  is first scheduled, the manager asks every host for a domain stamped with
  its namespace and name (a new owner filter on `ListVMs`, advertised as
  `supportsListOwnerFilter`; the VM's candidate domain names only, so an
  adopted domain is not seen): a previous incarnation holds it, and its own
  domain is re-bound where it runs, at the domain's own size. A clone's
  target is checked the same way. The marker only ever holds the VM that
  carries it; single-host Providers ignore it. **A clustered Provider holds at
  most one domain per namespace and name, so re-creating a VM after
  `orphan-on-delete` waits until the old domain is re-attached or removed.**
  **Roll the clustered libvirt provider right after the manager**: with an
  older one, new clustered VMs wait (`ProviderLacksListOwnerFilter`). Include
  VirtualMachine status in backups (Velero `restoreStatus`); the re-attach
  runbook needs only a `virsh metadata` re-stamp and, for a VM restored
  without status, the marker set to its new UID — no status edit. Keep the
  marker out of manifests you commit
  (→ [`docs/clustered-restore.md`](docs/clustered-restore.md)).
- On a clustered Provider, shrinking a **running** VM waits until the VM is
  powered off (`Reconfiguring=False/ShrinkPendingPowerOff`): a live shrink
  only deflates the balloon, which the guest can take back. VirtRigaud never
  powers the VM off for it; set `spec.powerState: Off` and back to `On`. A VM
  whose VMClass enables memory hot-add counts at its balloon ceiling (4× its
  memory), and a pending create whose VMClass has grown since it was scheduled
  is not retried (`PendingSizeGrew`).

### Fixes

- **Deleting a VirtualMachine while its Provider cannot be reached no longer
  orphans the hypervisor VM.** When the manager had no client for the
  Provider — its runtime not `Running` (every provider rollout or restart),
  the provider pod down, a TLS or network misconfiguration — the finalizer was
  released without any provider `Delete`, leaving the VM running unmanaged.
  The delete now waits: `Ready=False/ProviderUnavailable` on a single-host VM,
  `DeleteBlocked=True/ProviderUnavailable` on a clustered one, retried with a
  backoff from 15 s doubling to 5 min until the Provider answers;
  `orphan-on-delete` and `force-delete` still release it at once. A Provider
  that no longer exists in the VM's own namespace still releases it as before.
  When a held delete's Provider answers but the `Delete` then fails for
  another reason, `Ready` becomes `False/ProviderError` instead of keeping the
  old hold (a clustered VM kept `Ready=False/DeleteBlocked` after any hold).
  Every `force-delete` that releases a VM without a confirmed provider delete
  now records a `Warning` event **`VMLeftOnHypervisor`** naming the VM and its
  provider id (never a host), so leftovers can be found
  (→ [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#deleting-a-vm-while-its-provider-cannot-be-reached)).
- **On a clustered Provider, `force-delete` of a consumer VM needs the
  Provider's permission, like `orphan-on-delete`.** A VM in another namespace
  than its clustered Provider could be released with `force-delete` while its
  delete was held or failing, leaving its domain running on the shared hosts
  outside the capacity accounting. Unless the Provider carries
  `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"`, the annotation
  is now ignored for such a VM (`ForceDeleteRefused=True/ForceDeleteNotAllowed`
  and a `Warning` event) and its delete keeps being retried as if it were not
  set. Same-namespace VMs and single-host Providers are unchanged
  (→ [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#detaching-a-vm-without-deleting-it-virtrigaudioorphan-on-delete)).
- **A single-host VM no longer loses its provider id after a Create, and is
  never created twice or released while its Create's outcome is unknown.**
  Editing or deleting a VM while its Create ran made the status write that
  records the id fail with a conflict: the next reconcile created a second VM
  (Proxmox) or a delete released the VM without deleting it on the
  hypervisor. Before a Create the VM now records
  `Provisioning=True/CreateRequested`, confirmed before the RPC, and re-reads
  itself live (the marker is written with an optimistic lock); the id is
  written with a UID- and id-checked status patch,
  retried on transient errors (a **`VMRecordNotSaved`** event names a VM it
  could not record). A Create whose outcome stays unknown is re-sent on
  vSphere, libvirt and Proxmox (they re-bind the VM they stamped; roll the
  Proxmox provider with the manager); otherwise the VM waits
  with `Ready=False/CreateOutcomeUnknown` until an operator records its
  `status.id` or confirms that this attempt made no VM
  (`virtrigaud.io/create-outcome=not-created@<attempt id>`, audited by a
  `CreateOutcomeConfirmed` event), and its delete is held the same way
  (libvirt and Proxmox find and delete the VM by its owner stamp; libvirt
  releases it only when the provider confirms no such domain exists)
  (→ [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#a-create-whose-outcome-is-unknown-createrequested)).
- A Create refused with a non-retryable `FailedPrecondition` or
  `PermissionDenied` is held as `Ready=False/CreateRefused` (the event
  carries the provider's answer) and re-checked with a backoff instead of
  every 5 s.
- A Create answered with a retryable `VM_DISK_CHECK_FAILED` or
  `VM_OPERATION_FAILED` backs off from 15 s to 5 min
  (`Ready=False/CreateBackoff`) instead of retrying every 5 s.
- **`spec.powerState: OffGraceful` no longer loops on a powered-off VM.**
  Providers report such a VM as `Off`, so it was sent a graceful shutdown on
  every reconcile (on vSphere an `InvalidPowerState` fault and
  `Ready=False/ProviderError` every few seconds). `Off` now satisfies
  `OffGraceful`.
- A VirtualMachine is created only through a Provider that is not being
  deleted: on one that is, it shows `Ready=False/ProviderDeleting` and nothing
  is created — point it at another Provider, or delete it.
- A VirtualMachine's `Ready` condition no longer carries the provider
  resolver's error (which named the provider's TLS Secret, Service address and
  runtime phase) or a gRPC transport error: it says so with a constant message
  and the cause goes to the manager log. A VM being deleted is no longer
  reconciled on its own status writes, so a held delete is retried on its
  backoff only; `force-delete` and `orphan-on-delete` still take effect at once.
- libvirt `Reconfigure` is honest: a failed `setvcpus`, `setmem`, `setmaxmem`
  or offline disk resize is an error (it used to return success), a live change
  is also written to the domain's persistent definition (it used to be undone
  at the next power cycle), a stopped VM's CPU/memory grow beyond its maximum
  works, and a change the running VM cannot take is reported as
  restart-required (new `TaskResponse.restart_required`). While a change is
  pending a restart, `status.currentResources` counts the larger of the
  running and the next-boot size. A paused or suspended domain is reported as
  `Suspended` (never `Off`) and is not reconfigured. On clustered Providers, a
  VM's memory ceiling is recorded once from the provider
  (`DescribeResponse.max_memory_mib`) when missing, raised when the provider
  reports more and lowered after a confirmed shrink; its recorded CPU is raised
  to the vCPUs `Describe` reports; a failed Reconfigure is counted at the
  larger size and retried on a per-VM backoff (5 s to 5 min); a clustered answer
  without the new `honest_result` marker is not trusted; provider-reported sizes
  are bounded and committed sums saturate; the disk is grown
  before any CPU/memory change; and a single-host per-VM failure no longer
  counts toward the Provider's circuit breaker
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md)).
- vSphere `Describe` no longer treats a transient vCenter error as "the VM is
  gone" (which used to trigger a spurious re-create).
- vSphere `Create` no longer powers on every new VM. A VirtualMachine with
  `spec.powerState: Off` booted until the VirtualMachine controller
  hard-powered it off. Create now leaves the VM off, like libvirt and Proxmox,
  and the controller powers it on when `spec.powerState` is `On` or empty.
  Cloud-init is set at create time and still applies on the first boot. A VM
  created from an imported (migrated) disk is now also grown to its VMClass
  disk size and gets its additional disks; a bug skipped both. The grow is
  grow-only: a migrated VM smaller than the VMClass `diskDefaults.size`
  (default `40Gi`) is grown to it, using datastore capacity (all at once for a
  thick disk); a GPT guest sees a moved backup header, which `growpart` repairs.
  A failed grow or disk attach now fails the create (retried and completed)
  instead of being skipped silently
  (→ [`docs/upgrading.md`](docs/upgrading.md)).
- libvirt: hardware-accelerated `<domain type='kvm'>` is used again on hosts
  with a readable `/dev/kvm`, instead of falling back to software emulation.
- Chart: `networkPolicy` (previously `security.networkPolicies`) is never
  silently enabled by `helm upgrade --reuse-values`.
- Examples: every manifest under `examples/` applies against the current CRDs
  again (quoted `powerState` values, fields updated to the v1beta1 API), and
  CI now dry-runs them all.
- A `VMSnapshot` whose `Provider` could not be resolved on its first attempt is
  no longer marked `Ready` without a snapshot being taken. It stays pending and
  retries until the provider is reachable. Snapshots already affected are `Ready`
  with no `status.creationTime`; the
  [upgrade guide](docs/upgrading.md#post-upgrade-verification-checklist) has a
  query to list them.
- libvirt clones (single-host and clustered): a full clone needs a
  **powered-off source**. A `VMClone` of a running libvirt VM used to fail on
  qemu's image lock; it now waits (`Pending`, `Ready=False/SourceMustBePoweredOff`,
  never counted by the circuit breaker) and proceeds once the source is off.
  A full clone, or an s3/nfs export, of a VM with an external snapshot (whose
  active disk is libvirt's `0600` overlay) used to fail with "Permission
  denied". The copy now reads the source through `sudo -n` when the host
  allows it, with a regex-confined sudoers rule
  (→ [`docs/libvirt-clones.md`](docs/libvirt-clones.md#what-the-copies-run-as-root)).
  Root opens every disk in the format its domain definition names, never
  probed; a raw disk's chain is never walked. A copy is refused, not retried
  as the SSH user, when:
  - its chain has a symbolic link, an image with an external data file, or
    an image in a directory other accounts can write (unless sticky);
  - its disk is neither qcow2 nor raw.
  An nfs export runs as root only with the SSH user's own NFS identity.
  `nfs.uid`/`gid` 0 are refused for every nfs migration, whatever its
  providers (`NFSRootIdentityNotAllowed`): use a dedicated non-zero uid/gid
  that owns the export. Prerequisites: a QEMU with the
  CVE-2024-4467 fix, and a pool directory that is not group-writable. Replace
  any `qemu-img info -U *` sudoers wildcard with the documented regex rule.
  A clustered VM is placed only on a `Host` labelled
  `net.virtrigaud.io/<network>: "true"` for each libvirt network it uses
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#first-vm-checklist)).
- A `VMClone` refused by its provider while it waits — for its source to be
  powered off, an unreachable host, a previous incarnation of its target — no
  longer calls the provider in a tight loop. Each refusal rewrote the clone's
  status, which re-triggered the clone at once: a clone of a running libvirt
  VM called `Clone` about ten times a second instead of following its backoff
  (15 s doubling to 5 min). A repeated wait now writes nothing, and a waiting
  clone never shows `Phase=Cloning` or `Cloning=True`. A clone that does not
  fit on its host no longer jumps straight to its longest re-check interval,
  and a `VMSnapshot` whose provider delete fails no longer retries it in a
  tight loop for its first five minutes.
- A `VMClone` edited while its provider copied the disk (a libvirt clone is a
  synchronous copy of several minutes) no longer loses the clone it made: the
  edit used to make the controller's status write fail, the clone was sent
  again, refused as "already exists", and failed, leaving the copy untracked
  on the host. The record lands only on the `VMClone` it was made for: never on
  one deleted and re-created under the same name meanwhile, and never over a
  stored `Failed`.
- A `Clone` or `SnapshotCreate` is never sent from a stale read: the object is
  re-read from the API server right before the call, so a second, untracked
  snapshot or clone cannot be made from a cache that does not show the first
  one's result yet.
- **A `VMMigration` no longer leaves the source snapshot it created behind.**
  Deleting a migration whose snapshot `Delete` failed, or whose source
  `Provider` could not be reached, released it anyway and left the snapshot
  untracked on the live source VM (on libvirt it then blocked deleting that
  VM); a migration that failed for good never cleaned its snapshot up at all.
  A failed-for-good migration now deletes its snapshot, and a deletion waits
  for it: `SnapshotCleanup=False/SnapshotCleanupPending` (constant message,
  one `Warning` event), retried with a backoff from 15 s doubling to 5 min.
  A snapshot the provider no longer finds releases it (vSphere and Proxmox
  now answer a missing snapshot with `NotFound`); a source VM or own-namespace
  `Provider` that is gone releases it with a `SnapshotLeftOnHypervisor`
  Warning, never a "deleted".
  `virtrigaud.io/force-delete: "true"` on the `VMMigration` releases it anyway
  with a `SnapshotLeftOnHypervisor` Warning naming the snapshot
  (→ [Upgrade guide](docs/upgrading.md#breaking-changes)).
- A `VMSnapshot` being deleted while its VM's `Provider` cannot be read or
  reached is held with `Ready=False/ProviderUnavailable` and the same backoff
  (it was retried every 30 s with no status), and `force-delete` now releases
  it, with a `SnapshotLeftOnHypervisor` Warning. One whose `Provider` no
  longer exists in its own namespace is released with that Warning.
- **A migration no longer exports before its source snapshot is taken.** A
  task-backed snapshot is polled to completion; a failed one fails the
  migration, and a retry never exports an unconfirmed snapshot.
- The first retry of a failed migration waits `retryDelay` (it ran at once),
  and later retries back off from the failure they retry.
- A `VMSnapshot`'s `deleteOnVMDelete` and `maxAge` retention now apply once
  its VM is gone or being deleted (never while the VM is detached with
  `orphan-on-delete`).
- **A `VMSnapshot` is bound to its VM and Provider** (new status fields
  `vmUID`, `boundProvider`): a VM re-created under the same name, or moved to
  another Provider, is never sent the snapshot's delete; the VMSnapshot is
  released with a `SnapshotLeftOnHypervisor` Warning
  (→ [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)).
- Proxmox snapshot deletes resolve the VM's actual node (an HA move no longer
  reads as "snapshot gone"); vSphere re-reads the VM before answering a
  snapshot `NotFound`.
- A migration's source snapshot task is waited for at most
  `spec.options.timeout` (2 h by default), and provider task error text no
  longer reaches the VMMigration status.
- `VMClone`, `VMSnapshot` and `VMMigration` status and events no longer quote
  the provider resolver's error (which can name the provider's TLS Secret,
  Service address and runtime phase) or a gRPC connection error; the cause is
  in the manager log. Other provider answers in them are sanitized
  (→ [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)).
- A `VMMigration` status write that hits a conflict is retried on the live
  object without sleeping in the reconcile.
- A single-host `VMClone` whose `Clone` provably did nothing (the request
  never reached the provider, or the provider answered that it did not
  perform it — an SSH or libvirtd blip) is held as `CloneRetrying` and
  retried with a backoff (15 s doubling to 5 min) instead of failing for good.
  A clone whose outcome is unknown (the connection dropped mid-call, a
  timeout) still fails, so a retry can never make a second VM.
- Known limitation: a migration's `nfs` or `s3` staging object (a full copy of
  the source disk) is never deleted by VirtRigaud; clean it up yourself
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md)).
- **Proxmox VMs carry an owner stamp; a re-sent `Create` no longer makes a
  duplicate.** The Proxmox provider writes the owning `VirtualMachine`'s
  identity into each new VM's description, in the same call that creates or
  clones it. A `Create` sent again after an unknown outcome returns, and
  finishes if needed, the VM an earlier one made. While PVE is still cloning,
  the re-sent `Create` waits instead of starting a second clone.
  - **`ListVMs`** reports `owner_uid` / `owner_stamp_state`, so adoption skips
    VMs managed through another Provider object and a VM can be found by its
    owner. It also keeps listing a down node's VMs.
  - **`Delete`.** One that carries an owner never destroys a VM stamped for
    someone else (a reused VMID), a template, or an unstamped VM with another
    name.
  - **"Not found".** A VM that PVE reports missing from one node is treated as
    gone only when its VMID is free cluster-wide.
  - **Error classes.** `Create`/`Clone` failures are now classified: the new
    `CREATE_NOT_PERFORMED` reason marks a create that made nothing, and a
    clone timeout or 5xx is no longer reported as an invalid spec.

  VMs created before this release stay unstamped and behave as before.
  → [VM ownership: Proxmox](docs/vm-ownership.md#proxmox)
- **A vSphere VM bound to a template or an image artifact is held when its
  delete or power change is refused.** The provider refuses both
  (`VM_IS_IMAGE_ARTIFACT`); the manager used to retry them every 15 s / 5 s.
  Now: `Ready=False/VMIsImageArtifact` (a clustered delete:
  `DeleteBlocked=True/VMIsImageArtifact`), a constant message, one `Warning`
  event and a backoff up to 5 min. Detach the VM with
  `virtrigaud.io/orphan-on-delete: "true"`.
- **Single-host provider `Delete` requests carry the VM's owner**, so the
  Proxmox owner-checked delete never destroys an unrelated VM that reused the
  VMID; such an answer (`VM_NOT_OWNED`) releases the VirtualMachine with a
  `VMLeftOnHypervisor` Warning. Adopted VMs and clone targets send it too;
  Proxmox accepts an unstamped VM under the name adoption gave it (sanitized
  PVE name or `vm-<vmid>`). A Proxmox template is never deleted
  (`VM_IS_IMAGE_ARTIFACT`), and a delete refused for an unreadable owner stamp
  is held as `Ready=False/OwnerStampUnreadable`.
- **Nothing is created through a Provider being deleted, even before the
  manager's cache sees the deletion.** The Provider is re-read from the API
  server right before every `Create`, `Clone` and `ImportDisk`; a deleting,
  gone or replaced Provider holds the object with `Ready=False/ProviderDeleting`.
  The other side of the race is closed too: before a deleting Provider's
  in-use finalizer is removed, what still uses it is counted again from the
  API server, and a use the cache had not seen yet keeps it held.
- **`force-delete` of a `VMSnapshot` or `VMMigration` in another namespace than
  its clustered Provider needs the Provider's
  `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"`**, as for a
  `VirtualMachine`; otherwise `ForceDeleteRefused=True/ForceDeleteNotAllowed`
  and the delete keeps being retried.
- Conditions set by the VMClone, VMMigration, VMSnapshot, VMSet and Provider
  controllers (and the VirtualMachine conditions set through the shared
  helper) now record the object's `metadata.generation` as
  `observedGeneration` (it was `0`, or a per-change counter).
- The kustomize helper ClusterRoles in `config/rbac`
  (`*-admin-role`, `*-editor-role`, `*-viewer-role`) named the API group
  `infra.virtrigaud.io.infra.virtrigaud.io` and granted nothing; they now
  grant `infra.virtrigaud.io`.
- A `VMMigration` with `deleteAfterMigration: true` whose target stays off
  (`powerOn: false`) records a `SourceDeletedTargetNotStarted` Warning when it
  deletes the source.
- Provider SDK: the `sdk/provider/client` RPC methods (`Create`, `Describe`,
  `TaskStatus` and the others) return a nil error on success again. Before, a
  successful call returned a non-nil error that printed as `<nil>`, and
  `WaitForTask` failed on its first poll.

## Upgrade

See **[docs/upgrading.md](docs/upgrading.md)** for the full breaking-change
table, required upgrade order, new required privileges, and a post-upgrade
verification checklist.
