<!--
Release notes for VirtRigaud v0.4.0 (GA); title the release "v0.4.0". v0.4.0 ships the code of
v0.4.0-rc9 (nine release candidates came before it: rc1, rc2, rc4 to rc9 were published from
earlier versions of this file; rc3 was tagged, but its release run was stopped by the image
scan, CVE-2026-63209, and no rc3 release was published; rc1 followed v0.3.11). The notes are
cumulative since v0.3.11. Someone already on a release candidate follows "Upgrading from a
v0.4.0 release candidate" under "Upgrade", which keeps, per candidate, the actions that are
still needed. See docs/upgrading.md for the full upgrade guide these notes link to, and
CHANGELOG.md for the complete, dated changelog.
GitHub's limit for a release body is 125,000 characters; this file is about 143,000
characters, so the published body is a trimmed version of it: everything below this comment
except the "Fixes" subsection of "Highlights" (the preamble, "Breaking changes", the other
"Highlights" subsections, "Known limitations", "Validation" and "Upgrade", about
102,000 characters). The first four "Fixes" bullets point back at those
subsections; add a line to the published body linking to this file's "Fixes" for the rest.
-->

This is **v0.4.0**, the first release since v0.3.11. It is a security-hardening release: a
full codebase security review found and fixed several cross-tenant and privilege-escalation
issues in the vSphere and libvirt providers, cross-namespace clone/migration targets, and VM
provider binding. It also fixes what goes wrong half-way through an operation, and finishes
work on libvirt, Proxmox and clustered libvirt that the lab found:

- **Ownership is proven before a call acts on a VM.** Migrations, adoption, clones and
  every per-VM call act only on the VM VirtRigaud owns, and a reserved-keys admission policy
  protects the markers they rely on.
- **A failure half-way through an operation no longer loses data or repeats it.** A resize
  that hit the wrong disk, a provider call whose result was lost, a failed migration's
  leftover disk and staged copy, and provider pods' access to migration storage.
- **libvirt:** private cloud-init seeds and a clone's own identity, snapshot revert
  (`spec.snapshot.revertToRef`), safer snapshot deletes, `status.ips` that lists only
  reachable addresses, and migration into a clustered libvirt Provider (experimental).
- **Proxmox:** the cloud-init drive goes where the VM's disk is, the phase after a power
  change, and the bridge, VLAN, model and static IP a VM's networks name.
- **Clustered libvirt (experimental):** `HostPool.status`, answers that say how far a call
  got when its host stops answering, and a delete held for an unreachable host that says so.

v0.4.0 went through nine release candidates (rc3 was tagged but not released) and was
validated in the lab on real vSphere, libvirt (single-host and clustered) and Proxmox VE;
see [Validation](#validation). It ships the code of v0.4.0-rc9. **Several fixes are
breaking or change default behavior.** Read the upgrade guide before upgrading: **[Upgrading
from v0.3.11](docs/upgrading.md)**.

The notes below are cumulative since v0.3.11. If you are already on a v0.4.0 release
candidate, follow [Upgrading from a v0.4.0 release
candidate](#upgrading-from-a-v040-release-candidate): it lists, per candidate, what is still
needed.

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

- **Security (affects v0.3.11): Proxmox imports a migrated disk only from the
  VM's own verified migration stage.** With the migration SSH data plane
  configured, a Proxmox `Create` ran `qm importdisk` on any node path in
  `spec.importedDisk.path` and then deleted it, so a tenant could copy any file
  the SSH user can read on the PVE node (another VM's disk, `/etc/pve/priv/*`)
  into their own VM. Now the path must be verified by the manager as this VM's
  own migration disk and be exactly
  `/var/lib/virtrigaud/import-stage/<namespace>_<vm name>-migrated.<format>` (stages
  are now named per namespace); anything else is refused before anything is
  created. **Upgrade the manager before the Proxmox provider.** A Proxmox-target
  migration staged before the upgrade whose target VM was not yet created must
  be run again. A migration into another namespace through a Proxmox target
  fails at `Validating` (`ProxmoxCrossNamespaceTarget`) before any export.
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

- **A `VMMigration` acts only on the target `VirtualMachine` it
  created** (UID binding plus a claim, `status.targetVMUID` / `targetVMClaim`). A
  same-named `VirtualMachine` that the migration did not create fails it with
  `TargetVMNotOwned` at `Validating`, before the source is powered off or exported;
  before, the migration took it as its own target. **A migration in `Creating` or
  `Validating-Target` while you upgrade fails closed with `TargetVMNotOwned`** (its
  target VM is left as it is); let migrations finish first.
  → [The target VM must be the migration's own](docs/cross-namespace-targets.md#the-target-vm-must-be-the-migrations-own),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **Only the manager may set the manager-owned `virtrigaud.io/*`
  annotations and the `virtrigaud.io/adopted` label on a `VirtualMachine`**, enforced
  by a new `ValidatingAdmissionPolicy` (Kubernetes 1.30+; chart value
  `admissionPolicy.reservedKeys`, default on). Backup and restore tools (Velero)
  and a second VirtRigaud installation in the cluster are rejected unless listed in
  `exemptUsernames` / `exemptGroups`. Installing it needs create rights on
  `validatingadmissionpolicies` and `validatingadmissionpolicybindings`. It is a
  security control for migration, adoption and clone: do not disable it without a
  replacement. → [`docs/admission-policy.md`](docs/admission-policy.md),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **vSphere and Proxmox `Reconfigure` no longer resize the root disk.**
  Proxmox: a `spec.disks` size no longer grows `scsi0` and nothing replaces it in this
  release (grow the disk in PVE). vSphere: a `spec.disks` entry grows only the
  additional disk recorded under its name (VMs created by v0.4.0 or later), and a size
  below the disk's now fails the `Reconfigure`, CPU and memory included. A change a
  running vSphere VM cannot take (no hot-add, a memory decrease, and others) shows
  `Reconfiguring=False/ResizePendingPowerOff` until the VM is powered off.
  → [Which disk a size goes to](docs/reconfigure-results.md#which-disk-a-size-goes-to),
  [vSphere: changes a running VM cannot take](docs/reconfigure-results.md#vsphere-changes-a-running-vm-cannot-take),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **A `SnapshotCreate`, `ExportDisk` or `ImportDisk` whose outcome is unknown
  holds the `VMMigration` or `VMSnapshot` for an operator** (`Ready=False`,
  `SnapshotOutcomeUnknown`, `ExportOutcomeUnknown` or `ImportOutcomeUnknown`) instead of
  failing the attempt and re-sending it. It is re-sent on its own only on a backend the
  provider lists as re-send safe, 35 minutes later; otherwise you release it with the
  annotation `virtrigaud.io/operation-outcome=not-performed@<attempt id>` after checking
  the provider. Alerting and automation that expected a timed-out call to fail must
  include these reasons. Roll the providers with the manager.
  → [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md)
- **Provider pods mount only the migration PVCs a live `VMMigration`
  references, and deleting a `VMMigration` deletes only the PVC it created.** If you
  relied on a migration's deletion to remove the PVC named in `spec.storage.pvc.name`, or
  on labelled PVCs being mounted without a migration, change that. The upgrade rolls
  provider pods once.
  → [What the provider pods mount](examples/migration/README.md#pvc-staging-backend-legacy-what-the-provider-pods-mount),
  [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)
- **A `pvc` migration fails if the staging PVC holds a symbolic link, a hard
  link or a special file at a staged path, or sits on a filesystem that reports a link
  count other than 1**; the libvirt pod-side import no longer accepts `file://` sources.
  → [Staged-file hardening](examples/migration/README.md#staged-file-hardening-pvc-backend)
- **libvirt cloud-init seeds are built in a dedicated directory that must
  exist on every libvirt host.** Create it (owned by the provider's SSH user, mode
  `0711`) **before rolling the libvirt providers**, as root on every single-host libvirt
  host, every host of a clustered Provider and every migration target host:

  ```sh
  install -d -m 0711 -o <ssh user> -g <ssh user> /var/lib/libvirt/virtrigaud-seeds
  ```

  Otherwise every create through that host, and every clone of a VM that has a seed,
  fails, retryably, with `refusing to stage cloud-init data`. VMs that already run are
  not affected, and their seeds stay in `/tmp` until you make them private. The directory
  is `VIRTRIGAUD_LIBVIRT_SEED_DIR` (default `/var/lib/libvirt/virtrigaud-seeds`); keep it
  on a local filesystem, never NFS, and exclude it from backups or encrypt them: it holds
  every VM's user-data in plaintext.
  → [`docs/libvirt-domain-ownership.md`](docs/libvirt-domain-ownership.md#cloud-init-seed-directory),
  [Upgrade guide](docs/upgrading.md#breaking-changes)
- **`spec.snapshot.revertToRef` now reverts the VM; it did nothing before.** The field
  was accepted and ignored. A `VirtualMachine` that carries it is acted on by the first
  reconcile of the new manager, and one that is powered off is reverted at once,
  discarding what was written to its disks since the snapshot. **Before the manager
  rolls, remove the field from every `VirtualMachine` and `VMSet` template you do not
  want reverted.** List the VMs that carry it with:

  ```sh
  kubectl get virtualmachines.infra.virtrigaud.io -A -o jsonpath='{range .items[?(@.spec.snapshot.revertToRef)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
  ```

  The revert runs only while the VM is off and at most once per request; see
  [Snapshots and revert](#snapshots-and-revert).
  → [Upgrade guide](docs/upgrading.md#breaking-changes),
  [`docs/vm-snapshot-revert.md`](docs/vm-snapshot-revert.md)
- **libvirt: grant the provider's SSH user `VR_DISK_READ` over the pool directory, where
  it cannot already read libvirt's `0600` snapshot and revert overlays.** The disk
  in-use check that guards a snapshot create, revert or delete, and a VM delete (on a
  clustered Provider also a create and a clone), reads the disk chain of every domain on
  the host. libvirt creates `<domain>-disk.<snapshot>` and `<domain>-disk.<epoch>` mode
  `0600`, owned by root while the VM is off, and VirtRigaud does not change them. Without
  the grant (or a manual `chown -h libvirt-qemu:kvm` and `chmod 0640` of each new
  overlay, with the VM off) those operations are refused with "permission denied reading
  a disk image". On an NFS pool, root reads only when the export does not squash root;
  such an export must be dedicated to the Provider's hosts. The sudoers rule needs sudo
  1.9.10 or later.
  → [Unreadable disk images](docs/libvirt-clones.md#unreadable-disk-images),
  [What the copies run as root](docs/libvirt-clones.md#what-the-copies-run-as-root)

**Behaviour changes to check.** No operator action beyond the rollout unless stated:

- **A libvirt snapshot delete that would leave another snapshot unusable is refused.**
  Deleting a disk-only snapshot while a memory snapshot of the same VM is stored in its
  overlay (or in another file the delete would merge away or remove) answers a definite
  `FailedPrecondition` naming the snapshot to delete first, and changes nothing.
  **Delete the memory snapshot first.** A check that cannot be completed, and a snapshot
  whose definition cannot be read, refuse the delete too, retryably (a retried
  `ProviderError`, not `DeleteBlocked`). See
  [Snapshots and revert](#snapshots-and-revert).
- **A refused `VMSnapshot` delete shows `Ready=False/DeleteBlocked` with the provider's
  answer** (sanitised, first 256 bytes), for every provider's `FailedPrecondition` other
  than a disk another VM depends on: the libvirt refusals above, vSphere's refusal to
  delete a snapshot of a template or image artifact, and a Proxmox VM whose owner stamp
  cannot be read. A re-check is paced by the failed-delete backoff (15 seconds up to 5
  minutes). Automation must match the reason `DeleteBlocked`, not a message.
- **A revert of a libvirt snapshot is refused (`SnapshotReverted=False/RevertFailed`,
  terminal, nothing changed but a repair of an interrupted ejection that may run first)
  when the snapshot stored a CD-ROM or read-only disk with media:** any disk-only snapshot
  of a VM with a cloud-init seed or another read-only disk that was taken while the VM
  ran, and those taken before this release (by v0.3.11 or a v0.4.0 candidate before rc7)
  of a VM with a seed. Take a new snapshot with the VM powered off.
- **A snapshot of a powered-off libvirt VM briefly ejects its CD-ROMs and floppies from the
  VM's persistent definition** and inserts them again; the domain's metadata carries a
  record of them meanwhile. A snapshot interrupted in that window is repaired by the next
  snapshot create, revert, start or reboot of the VM.
- **A power-on or reboot of a libvirt VM issues one extra read of the domain definition,
  and is not attempted while a cloud-init seed an interrupted snapshot ejected cannot be
  inserted again.** A definition that cannot be read is logged and the start proceeds.
- **A clustered `VMSnapshot` create the provider answers it did not perform no longer ends
  in the operator hold.** It shows `Ready=False/ProviderError`, a `SnapshotCreateRetrying`
  event (once, not on each retry), and is sent again after 1, 2, 4, 8, then every 10
  minutes. Alerting that watched for `SnapshotOutcomeUnknown` after such an answer no
  longer fires; a create whose outcome is really unknown is still held.
- **The `RevertFailed` message of a revert the provider refuses for good** (libvirt: a
  snapshot it cannot revert safely) carries the provider's answer only, with no "set it
  again to retry". Match on the reason, not the message.
- **`status.ips` has fewer entries on every provider:** loopback, unspecified, link-local
  and multicast addresses, entries that are not an IP address (an entry with a prefix
  length too) and duplicates are gone, and kept addresses are written in canonical form
  (IPv4-mapped IPv6 as IPv4, IPv6 lower-case and compressed). A script that read
  `status.ips[0]`, or looked for such an entry, must change.
- **A libvirt NAT guest without the guest agent shows its DHCP lease, and a guest
  interface that does not start with `e` is no longer dropped by name.** →
  [`docs/vm-status-ips.md`](docs/vm-status-ips.md)
- **A VM held by a clustered restore guard** (`RestorePending`, `OwnDomainOnAnotherHost`,
  `UniquenessCheckFailed`, `ProviderLacksListOwnerFilter`) **is `status.phase: Pending`,
  not `Failed`;** alerts that count `Failed` VMs no longer see them.
- **A clustered delete whose host stopped answering is held** with
  `DeleteBlocked=True/HostUnreachable` (a `Warning` event `DeleteBlocked`) and retried
  with the 15 s to 5 min backoff; its message can contain the VM's `Host` name. The
  sentence of the `DeleteBlocked` message that tells how to release the VM starts with a
  capital letter: match on the reason.
- **A `Describe` to a clustered host that stopped answering, also one queued behind
  another call's dial, is `HOST_UNAVAILABLE` (`Ready=False/HostUnavailable`) within its
  deadline** instead of `Unknown` (`Ready=False/ProviderError`) after the other call's
  deadline.
- **A clustered `Delete`, `Power`, `Reconfigure` or `Create` whose host drops mid-call
  shows a retryable provider error, not `HostUnavailable`,** when a command that may
  change the host was sent; `HOST_UNAVAILABLE` is kept for "nothing was done".
  → [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md#clustered-libvirt-a-host-that-stops-answering)
- **A VM with `spec.disks` on a libvirt or Proxmox Provider gets the condition
  `DisksApplied=False/NotSupportedByProvider` and a `Warning` event;** `Ready` is not
  affected.
- **A clone of a libvirt or vSphere VM gets its own cloud-init identity** (a new
  `instance-id`, the target's hostname, new SSH host keys on first boot); see
  [Security](#security) for who may create `VMClone`s.
- **A libvirt Host's `allocatableStorageBytes` is the `default` pool's free space only,
  measured fresh,** which changes what a `VMPlacementPolicy` `minDiskSpacePerHost`
  compares against; review such policies on clustered Providers.
- **Proxmox: the cloud-init drive of a VM cloned from a template moves from `local` to the
  storage of the VM's disk.** A drive already on `local` stays there.
- **Proxmox: a failed power task** shows `Ready=False/ProviderError`, settles the
  `Reconfiguring` marker as `PowerTaskFailed` (a reason alerting that matches on reasons
  should know) and is re-sent on a 5 s to 5 min backoff instead of being polled for ever.
- **Proxmox: the PVE token needs `VM.Config.Network`, `VM.Config.Cloudinit` and, if it is
  not root, `SDN.Use` on the bridges in use,** or a clone that sets a NIC fails with a 403
  and is rolled back. Grant them before you roll the Proxmox provider.
- **Proxmox: a VM lands on the bridge, VLAN, model and static IP its networks name,** so a
  VM that relied on the old `vmbr0` default changes on its next create or clone, and an
  `ipAddress` without `prefix` or a `dns` entry that is not an IP is refused.
- **The manager sends only the network block that matches the target Provider's type,**
  which also changes which shared field a vSphere or libvirt VM reads when its attachment
  has several blocks.
  → [Proxmox networks](docs/proxmox-networks.md),
  [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)

See the full breaking-change table, required upgrade order (CRDs → admission
policy and grants → manager → providers), and rollback caveats in
**[docs/upgrading.md](docs/upgrading.md)**.

## Highlights

### Security

- `github.com/klauspost/compress` 1.18.7 fixes CVE-2026-63209 (HIGH, denial of
  service), reached through the S3 client used by migration storage (#423).
- The Proxmox provider applies the bridge and VLAN tag a `VirtualMachine`'s network
  names, so the PVE token's `SDN.Use` ACLs are what decide which bridges and VLANs the
  authors of `VirtualMachine`s in a namespace can reach; the provider has no allow-list
  of its own yet (a slice 0a follow-up in ADR-0014)
  (→ [Proxmox](#proxmox)).
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
- Provider pods no longer mount every labelled PVC in their namespace,
  only the migration PVCs a live `VMMigration` references (target read-only; the PVC
  must be the one the migration created, identified by its UID, or the one
  `spec.storage.pvc.name` names), and staged migration files on the `pvc` backend are
  opened with no symbolic link followed (`openat2` with `RESOLVE_BENEATH |
  RESOLVE_NO_SYMLINKS`), so a link planted in the staging PVC can no longer make an
  import read the provider pod's credentials or an export write outside the PVC
  (→ [PVC staging backend](examples/migration/README.md#pvc-staging-backend-legacy-what-the-provider-pods-mount)).
- A libvirt VM's cloud-init seed, which carries its user-data (passwords, keys, tokens),
  is no longer readable by every local user of the hypervisor host: it is built `0600` in
  a dedicated directory that the provider checks and refuses to use if it is not private
  (missing and not creatable, not owned by the SSH user, readable or writable by its
  group or others, or below a directory that is not owned by root or the SSH user or that
  other users can write unless it is sticky). Seeds of existing VMs stay in `/tmp` and
  readable until you make them private
  ([Leftovers from earlier releases](docs/upgrading.md#leftovers-from-earlier-releases)).
- **Treat `create` on `VMClone` as read access to the source VM's data.** A clone's disk
  is a full copy of its source's, and on libvirt the `VMClone`'s inline user-data
  (`spec.customization.userData.cloudInit.inline`, at most 1 MiB) runs as root on its first
  boot. It holds within one namespace as much as across namespaces. Check who can create
  `VMClone`s (`kubectl auth can-i create vmclones.infra.virtrigaud.io -n <namespace> --as
  <subject>`) and narrow the grant where that is more than intended. A clone no longer
  shares its source's cloud-init identity: its `instance-id` is `virtrigaud-<target
  VirtualMachine uid>`, its hostname the target's (or `spec.customization.hostname`), it
  gets its own seed (libvirt, built from the source's with `isoinfo`) or rewritten
  `guestinfo` (vSphere; Proxmox already did), so its first boot runs every per-instance
  module of its user-data again (`runcmd`, `packages`, `users`, ...) and regenerates the
  SSH host keys. `/etc/machine-id` is **not** regenerated: seal the source if clones need
  their own. Existing clones keep the identity they booted with
  (→ [VM ownership: a clone's host identity](docs/vm-ownership.md#a-clones-host-identity),
  [libvirt clones](docs/libvirt-clones.md#a-clones-cloud-init-identity)).
- Go 1.26.10 fixes GO-2026-6809 in `crypto/x509`, reachable through
  `x509.Certificate.Verify` (every TLS path); every image is rebuilt (#411).

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
  short outage. Still experimental — host-to-host migration is not implemented, and
  migration into a clustered provider is new in this release and experimental
  (below).
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
- **A `VMMigration` can target a clustered libvirt Provider** (`s3` or `nfs`; `pvc`,
  also what an unset storage means, is refused; the target VM must be in the migration's
  namespace):

  ```yaml
  spec:
    target:
      providerRef:
        name: libvirt-cluster   # a libvirt Provider with spec.topology: cluster
    storage:
      type: nfs                 # or s3
  ```

  The migration creates its **target VirtualMachine first**, at `Validating`, before the
  source is powered off, snapshotted or exported. The ordinary scheduler places it on a
  host whose `default` pool has room for the source disk
  (`Host.status.allocatableStorageBytes`), the migration waits with the condition
  `TargetPlaced`, and then the disk is exported and imported **on that host**
  (`ImportDisk` carries `target_host_id`) and attached in place. A host that fails after
  placement is waited for, not replaced. The disk-space check is a filter, not a
  reservation. Single-host, vSphere and Proxmox targets keep import-then-create. What you
  need: a clustered provider that reports `supports_routed_import` (roll it first; a
  target that does not fails the migration at `Validating` with
  `ClusteredTargetUnsupported`, before any side effect), the CRDs applied before the
  manager (`status.storageInfo.importTarget.host` is new, and `spec.importedDisk.diskID`
  may be empty while `migrationRef` is set), and on each host the `default` pool at
  `/var/lib/libvirt/images` with room, an `ssh://` endpoint, `qemu-img`, `flock` and
  `timeout`, and the seed directory. Run in the lab from single-host libvirt, vSphere and
  Proxmox sources and from a clustered libvirt source to Proxmox.
  **Behaviour change:** a libvirt Host's `allocatableStorageBytes` is now the `default`
  pool's free space only, measured fresh with `stat -f` on every Host sync (it summed
  every active pool from libvirt's cached `pool-info` before), which changes what a
  `VMPlacementPolicy` `minDiskSpacePerHost` compares against
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b),
  [`examples/vmmigration-into-clustered.yaml`](examples/vmmigration-into-clustered.yaml)).
- **A clustered host that stops answering is reported by how far the call got.** Every
  routed call records what it ran on its host. If nothing ran, the answer is
  `HOST_UNAVAILABLE` ("nothing was done"); if only reads ran, "not performed"; if a
  command that may change the host was sent, the outcome is unknown, and for `Clone`,
  `SnapshotCreate` (with a name and request token) and a landed-disk removal the answer is
  "unknown, safe to retry" (`VM_OUTCOME_UNKNOWN_RETRY`), which the manager retries without
  treating it as "nothing done". A `Describe` is always `HOST_UNAVAILABLE`, within its
  deadline: the waits before a command runs on a host (a command slot, the SSH dial and
  handshake, an SSH session, another call's dial) end 5 seconds before the call's deadline
  (a quarter of the time left, when that is shorter), and the TCP connect of a dial stops
  after 30 seconds at the latest. A `HOST_UNAVAILABLE` about the host the call was routed
  to names it. A clustered `Delete` whose `destroy` loses the host stops and is retried,
  instead of going on to undefine a domain that may still be running. A delete held for an
  unreachable host shows `DeleteBlocked=True/HostUnreachable`, naming the VM's `Host`
  (`status.placement.host`, never an endpoint), and completes once the host answers
  (→ [A host that stops answering](docs/provider-call-outcomes.md#clustered-libvirt-a-host-that-stops-answering),
  [A delete held for an unreachable host](docs/provider-call-outcomes.md#a-delete-held-for-an-unreachable-host)).
- **`HostPool.status` is written.** A `HostPool` controller in the manager counts each
  pool's member `Host`s (same namespace, `spec.poolRef` naming the pool) into
  `totalHosts`, `readyHosts` and `schedulableHosts` and sets a `Ready` condition
  (`HostsAvailable`, or `NoMemberHosts` / `NoSchedulableHosts`); `kubectl get hostpools`
  shows `TOTAL`, `READY` and `SCHEDULABLE`. Before, every pool showed `status: null`. The
  manager's role gains `hostpools/status`
  (→ [`HostPool.status`](docs/clustered-provider-inventory.md#hostpoolstatus)).

### Snapshots and revert

- **`spec.snapshot.revertToRef` reverts the VM.** The `VirtualMachine` controller now acts
  on the field; every provider already implemented `SnapshotRevert`.
  - *Only while the VM is off:* the revert is sent only when the provider reports the VM
    `Off` and `spec.powerState` is `Off` or `OffGraceful`. VirtRigaud never powers a VM off
    for a revert and does not power it on afterwards; otherwise
    `SnapshotReverted=False/WaitingForPowerOff` asks for `spec.powerState: Off`.
  - *The snapshot is resolved fail-closed:* the `VMSnapshot` must exist in the VM's
    namespace, be `Ready` with a provider snapshot id, not be deleting, and be taken of this
    VM (`spec.vmRef`, `status.vmUID`) through its Provider; otherwise `SnapshotNotFound`,
    `SnapshotNotReady` or `SnapshotForAnotherVM`, and nothing is sent. A Provider that
    reports `supportsSnapshots: false` gets `Unsupported`.
  - *At most once per request:* the new `status.snapshotRevert` records the `VMSnapshot`
    (name and UID), the phase (`Pending`, `Reverting`, `Succeeded`, `Failed`), the attempts
    and the outcome. While it records `Succeeded` or `Failed` for the `VMSnapshot` the field
    names, nothing is sent again, so a GitOps re-apply is safe. To revert again, remove the
    field and set it again; a ref to another `VMSnapshot` is a new request. A failed revert
    is not retried on its own.
  - *Recorded before it is sent,* with the optimistic lock of the `resourceVersion` the
    reconcile read. A provider task (Proxmox) is recorded and polled, for at most 2 hours.
    A call whose outcome is unknown is sent again only while a fresh `Describe` shows the VM
    still off with `spec.powerState` still off: at least 6 minutes apart, at most 5 calls.
  - *Conditions and events:* the condition `SnapshotReverted` (reasons
    `WaitingForPowerOff`, `SnapshotNotFound`, `SnapshotNotReady`, `SnapshotForAnotherVM`,
    `Unsupported`, `Reverting`, `Reverted`, `RevertFailed`); while it is `False`, `Ready` is
    `False` with the same reason. Events `SnapshotRevertStarted`, `SnapshotRevertSucceeded`,
    `SnapshotRevertFailed`.
  - *CRDs first:* `status.snapshotRevert` is a new field the manager's readiness check does
    not verify; with an older `VirtualMachine` CRD the manager sends no revert
    (`SnapshotReverted=False/Unsupported`).

  → [`docs/vm-snapshot-revert.md`](docs/vm-snapshot-revert.md)
- **libvirt: a snapshot of a powered-off VM can be reverted, and a revert no longer breaks
  the VM's cloud-init CD-ROM.** libvirt (9.9 and later) reverts an external (disk-only)
  snapshot by rebuilding the VM's disks from the domain definition the snapshot stored, and
  makes a new qcow2 overlay of every disk there that has media, including the read-only
  cloud-init seed CD-ROM. AppArmor does not let QEMU open that overlay in the private seed
  directory, so such a revert left the VM unable to start. Now a snapshot of a powered-off
  VM is taken with its CD-ROMs and floppies ejected from the persistent definition (recorded
  in the domain's metadata first, inserted again afterwards, also when the snapshot failed);
  a running VM is never ejected. A revert is refused before it changes anything when the
  snapshot stored a CD-ROM or read-only disk with media, inserts the VM's CD-ROMs and
  floppies (the seed included) again after a revert, and reports a medium it cannot insert
  as `RevertFailed`. An interrupted ejection is repaired by the next snapshot create,
  revert, start or reboot, only when the file is provably the VM's own cloud-init seed; any
  other medium (an installer ISO you attached) is never inserted from the record. A memory
  snapshot (internal) is not affected. `SnapshotDelete` also removes the
  `<domain>-disk.<epoch>` overlay a revert makes
  (→ [libvirt](docs/vm-snapshot-revert.md#libvirt)).
- **libvirt: other VMs' disk checks no longer fail on a snapshot overlay.** After an offline
  snapshot or a revert, a shut-off domain's definition keeps its chain in nested
  `<backingStore>` elements, and the disk in-use check opened each of them a second time as
  the SSH user only, which cannot read libvirt's `0600` overlay: every guarded operation of
  every other VM on the host was answered "could not verify that no other domain uses its
  disks". The check now reads such a chain once. A check that still cannot read an image
  answers "permission denied reading a disk image on its host" (`Unavailable` +
  `VM_DISK_CHECK_FAILED`: fail-closed, retried for a VM delete, a snapshot delete and a
  clustered snapshot create, never counted by the circuit breaker) and points to the fix
  (the `VR_DISK_READ` grant above; → [Unreadable disk
  images](docs/libvirt-clones.md#unreadable-disk-images)).
- **A clustered `VMSnapshot` create the provider did not perform is retried with a
  backoff, not held.** An answer such as `VM_DISK_CHECK_FAILED`, `HOST_UNAVAILABLE` or
  `VM_OPERATION_FAILED` "not performed" closes the attempt (the `SnapshotRequested` marker is
  removed), shows `Ready=False/ProviderError` ("The provider did not perform the snapshot
  creation ..."), and the create is sent again as a new attempt with the same request token
  after 1 minute, doubling per consecutive such answer up to 10 minutes (per `VMSnapshot`,
  in the manager's memory). An answer that leaves the outcome unknown keeps the marker, the
  re-send count and the hold
  (→ [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md)).
- **A revert refused for good no longer says "set it again to retry".** A revert the
  provider answers with `FailedPrecondition` (on libvirt: a snapshot taken while the VM ran
  or by an earlier release, which libvirt cannot revert safely) carries only the provider's
  own answer, which for libvirt starts with the guidance to take a new snapshot with the VM
  powered off. A revert refused because another VM depends on the disk (`VM_DISK_IN_USE`)
  and any other failure keep the retry hint.
- **libvirt: a disk-only snapshot is not deleted while a memory snapshot is stored in its
  overlay.** A `VMSnapshot` with `includeMemory: true` of a running VM is an internal
  libvirt snapshot, stored inside the qcow2 file the VM's disk is using. When the VM already
  has a disk-only snapshot, that file is the disk-only snapshot's overlay. On libvirt 10.0,
  deleting an external snapshot block-commits its overlay into its base and moves the VM
  off it, but leaves the overlay file; libvirt does not check for an internal snapshot
  stored in that file. The memory snapshot was therefore orphaned: its data stayed in the
  leftover file, unusable, because the VM no longer runs on it; libvirt kept its metadata,
  and every later delete of it failed while the VM ran (`disk image 'vda' for internal
  snapshot 'mem' is not the same as disk image currently used by VM`). Its `VMSnapshot`
  kept its finalizer and was retried for ever (`Ready=False/ProviderError`). VirtRigaud's
  overlay cleanup kept the leftover file, because the memory snapshot's metadata still
  referenced it. Now:
  - *The delete is refused, and nothing changes,* while another snapshot of the VM is
    stored in a file the delete may merge away or remove. The provider checks both libvirt's
    snapshot metadata and the files' own headers (`qemu-img info -U -f qcow2
    --output=json`, the same root read as the disk in-use check). The answer is a definite
    `FailedPrecondition` that names the snapshots to delete first. **Delete the memory
    snapshot first, then the disk-only snapshot;** deleting a memory snapshot is never
    refused by this check. When nothing references the merged overlay, `SnapshotDelete`
    removes the leftover file, so both snapshots are gone and no overlay file is left.
  - *A check that cannot be completed refuses the delete too, retryably* (`VM_DISK_CHECK_FAILED`,
    or the host-unreachable answer), and so does a snapshot whose definition cannot be
    read.
  - *A memory snapshot an earlier version orphaned* has only its metadata deleted
    (`virsh snapshot-delete <domain> <snapshot> --metadata`) when libvirt refuses its delete
    with the error above and the provider proves the data is gone: every file it was taken
    on no longer exists (for example because someone removed the leftover overlay), and no
    image in the VM's current disk chains lists it. Otherwise the answer is `delete refused,
    nothing changed: power the VM off and delete it again; ...` (with the VM off, libvirt
    deletes an internal snapshot from the files it recorded).
  - *The manager shows the refusal:* the `VMSnapshot` keeps its finalizer, gets
    `Ready=False/DeleteBlocked` with the provider's answer (sanitised, first 256 bytes) and
    a `SnapshotDeleteFailed` Warning event, and is re-checked with a backoff from 15 seconds
    up to 5 minutes. A re-check of the same refusal writes nothing.
    `virtrigaud.io/force-delete=true` still removes it, leaving the snapshot on the
    hypervisor. This is the manager's rule for any provider's definite refusal of a snapshot
    delete (a refusal about a disk another VM depends on, `VM_DISK_IN_USE`, keeps its own
    handling; a failure that is not a definite refusal stays `ProviderError` with the answer
    only in the event).

  Single-host and clustered Providers behave the same; a clustered refusal also carries
  `VM_OPERATION_FAILED`. → [Deleting memory and disk-only snapshots of the same
  VM](docs/libvirt-clones.md#deleting-memory-and-disk-only-snapshots-of-the-same-vm)

### Proxmox

- **The cloud-init drive of a VM cloned from a template goes where the VM's disk is.** For a
  `VirtualMachine` with `spec.userData`, the drive (`ide2`) was put on storage `local`,
  which a stock PVE has no `images` content type for, so the VM did not start. It now goes
  on the first of these that applies: a storage named for the VM in the request or the image
  (no effect yet for a `VirtualMachine`); the storage of the clone's first non-cdrom disk
  (the step that usually decides; a passthrough device skips to the next); then
  `PROVIDER_DEFAULT_STORAGE` or `PVE_DEFAULT_STORAGE` on the Proxmox provider pod; `local`
  only when none applies, with a warning in the provider log. A template that has a
  cloud-init drive of its own keeps it when it is already on the chosen storage. The
  provider fails closed (it guesses no storage when the clone's config cannot be read), and a
  storage ID that is not valid is refused as `InvalidSpec`. Existing VMs keep a drive that is
  already on `local`; move it with `qm set <vmid> --ide2 <storage>:cloudinit`
  (→ [`docs/proxmox-cloud-init-storage.md`](docs/proxmox-cloud-init-storage.md)).
- **A Proxmox VM no longer stays in phase `Reconfiguring` after a power change.** The power
  operation returns an asynchronous task; the manager now settles the `Reconfiguring` marker
  (`Reconfiguring=False/ReconcileSuccess`) when the task ends, so the phase follows the power
  state again. A failed power task is shown on `Ready=False/ProviderError`, settles the
  marker as `PowerTaskFailed`, and is sent again after 5 seconds, then 10, doubling to 5
  minutes, per VM. A task-status query PVE rejects as a request ends the task; a transient
  failure (a transport error, a timeout, a 5xx, 401/403, 429) does not
  (→ [`docs/vm-status-phase.md`](docs/vm-status-phase.md#a-power-change-that-returns-a-task)).
- **A Proxmox VM gets the bridge, VLAN tag, model and static IP its networks name.** The
  manager sends a VM's networks under the Go field names of the provider contract, which the
  Proxmox provider did not read, so every Proxmox VM got `virtio` on `vmbr0` with no static
  IP and a clone with user-data had its template's NICs overwritten. A new VM gets one NIC per
  entry of `spec.networks` (`net0`, `net1`, ...), `ipconfigN` and the VM-wide `nameserver`; a
  clone keeps its template's bridge, VLAN, options and MAC for every NIC the spec does not
  name (the CRD defaults `model` to `virtio`: set `model` to the template's if it is
  another). A bridge, model, VLAN tag, static IP, prefix, gateway or DNS server PVE cannot
  take, more than 32 networks, an `ipAddress` without `prefix` and a `dns` entry that is not
  an IP address are refused as `InvalidSpec` before a VM is made. Existing VMs are not
  modified
  (→ [`docs/proxmox-networks.md`](docs/proxmox-networks.md),
  [`examples/proxmox-complete-example.yaml`](examples/proxmox-complete-example.yaml)).
- **ADR-0014** records the accepted design for the rest of the migrated-guest network
  problem (carry each source NIC's MAC to the target and present the source's cloud-init
  instance-id, switchable with a planned `VMMigration` `spec.options.guestIdentity:
  Preserve|Fresh`, with a default-deny Provider policy for tenant-set MACs). Only the
  Proxmox network plumbing is implemented; see Known limitations
  (→ [ADR-0014](docs/adr/0014-migrated-guest-network.md)).

### Addresses, phase and conditions

- **`status.ips` lists only reachable addresses, on every provider.** The manager filters
  every provider's answer before it writes the field: loopback (`127.0.0.0/8`, `::1`),
  unspecified, IPv4 link-local (`169.254.0.0/16`), IPv6 link-local (`fe80::/10`) and
  multicast addresses, and anything that is not an IP address, are dropped; duplicates are
  removed; the provider's order is kept; private, IPv6 unique-local and global addresses
  stay. The libvirt provider applies the same rule to the guest agent's answer (so
  `primary_ip` in `status.provider` is no longer `127.0.0.1`, and an agent that reports only
  unreachable addresses falls back to the `virsh domifaddr` list), and so does adoption: an
  address that is dropped no longer counts as "has an IP" for the IP-discovery metric or the
  "waiting for an IP" poll interval.
- **A libvirt NAT guest without the guest agent shows its DHCP lease.** The provider kept
  only the rows of `virsh domifaddr` whose interface name starts with `e`, but the `lease`
  and `arp` sources name their rows after the NIC's host-side device (`vnet194`), so every
  lease and ARP row was dropped. Now the address, not the name, decides what is kept (the
  prefix length, `/24` from `lease` and `/0` from `arp`, is removed); the sources are still
  tried in order, the agent, the lease, then the ARP table, and the first with an address
  wins. A guest with several NICs shows all their addresses, including a NIC's second
  address. A libvirt guest on a host bridge has no lease: its address shows only if the QEMU
  guest agent runs, or, as a last resort, if the host has an ARP entry for the guest's MAC
  (→ [`docs/vm-status-ips.md`](docs/vm-status-ips.md)).
- **`VirtualMachine` `status.phase` is set on every VM** (`kubectl get vm` showed an empty
  `PHASE` on most VMs). The manager derives it on every status write: `Pending`,
  `Provisioning`, `Running`, `Stopped`, `Reconfiguring`, `Deleting` or `Failed`. A
  `Ready=False` hold such as `VMNotOwned`, `ProviderUnavailable` or `CreateOutcomeUnknown` is
  `Failed`; a `Suspended` VM is `Running`; an `Unknown` power state is `Failed`; a delete that
  is held is `Deleting`; a VM held by a clustered restore guard is `Pending`. The first
  reconcile after the manager rolls fills in every existing VM. Automation that reads
  `status.phase` must expect every value
  (→ [`docs/vm-status-phase.md`](docs/vm-status-phase.md#the-rule)).
- **`spec.disks` on libvirt and Proxmox is reported.** These providers attach no
  `spec.disks` (unchanged), but adding one used to change nothing visible: `Reconfiguring`
  even said "VM reconfigured successfully". A VM with `spec.disks` on such a Provider now
  gets `DisksApplied=False/NotSupportedByProvider` (removed when `spec.disks` is emptied)
  and one `Warning` event per generation; nothing is refused
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md#which-disk-a-size-goes-to)).

### Fixes

- **libvirt snapshot delete, revert and disk checks** (details under
  [Snapshots and revert](#snapshots-and-revert)):
  - Deleting a libvirt disk-only snapshot orphaned a memory snapshot stored in its overlay
    without any sign, and the memory snapshot's `VMSnapshot` could then never be deleted
    while the VM ran.
  - A libvirt snapshot revert was reported `Succeeded` and left the VM unable to start when
    the VM had a cloud-init seed: libvirt 9.9 or later made an overlay of the seed CD-ROM
    that AppArmor does not let QEMU open.
  - After an offline snapshot or a revert on libvirt, the snapshot, revert and delete (on a
    clustered Provider also create and clone) of every other VM on the host were refused
    ("could not verify that no other domain uses its disks"), because the disk check
    re-read a nested backing store as the SSH user.
  - A clustered `VMSnapshot` create the provider had not performed ended in an operator hold
    after ten re-sends.
  - A revert refused for good said "set it again to retry".
- **Addresses and conditions** (details under
  [Addresses, phase and conditions](#addresses-phase-and-conditions)): `status.ips` listed
  loopback and link-local addresses; a libvirt NAT guest without the guest agent never
  showed its DHCP lease; `spec.disks` on libvirt and Proxmox was dropped without any sign;
  `VirtualMachine` `status.phase` was empty on healthy VMs; a VM held by a clustered restore
  guard showed `Failed`.
- **Clustered libvirt** (details under
  [Clustered providers](#clustered-providers-experimental-opt-in)): `HostPool.status` was
  never written; a clustered host that stopped answering was reported as `HOST_UNAVAILABLE`
  or, after 30 seconds (or after the other call's deadline, for a call queued behind another
  call's SSH dial, 1m28s in the lab with a 30-second deadline), as `Unknown`, and a delete
  held for it said nothing about why.
- **Proxmox** (details under [Proxmox](#proxmox)): a VM with user-data failed to start on a
  stock PVE because the cloud-init drive was put on `local`; a VM stayed in phase
  `Reconfiguring` after a power change and a failed power task was polled for ever; every VM
  got `virtio` on `vmbr0` with no static IP, whatever its networks named, and a clone with
  user-data lost its template's NICs.
- **libvirt cloud-init seeds and clones:** a single-host libvirt delete of a VM and its clone
  together left the VM's seed directory (its user-data) on the host: after the undefine, the
  delete now removes the seed directory its definition referenced, and any directory named
  exactly after the domain that is at least five minutes old, once no defined domain
  references it, and `Create` and `Delete` of one domain name run one at a time (seeds
  leaked by earlier releases are not removed); every clone shared its source's cloud-init
  identity, and with it the SSH host keys (see [Security](#security)).
- **vSphere imports left a `<id>-<hex>.hlog` (about 300 bytes) in the landing folder,** and
  every successful migration showed `LandedDiskCleanup=Unknown` forever: the history log is
  removed, and a migration that reaches `Ready` records
  `LandedDiskCleanup=True/LandedDiskInUseByTarget` in the same status write. Deleting a
  `Ready` migration never deletes the disk.
- **Reconfigure grew the wrong disk** (affects v0.3.11): vSphere grew the root disk with an
  additional disk's size, Proxmox sent every `spec.disks` size to `scsi0` and dropped the
  CPU/memory change after a disk grow while reporting success. Neither provider's
  `Reconfigure` touches the root disk now (breaking, see above).
- **A CPU or memory change of a running vSphere VM without hot-add was retried
  forever** as `ProviderError`; it is now `Reconfiguring=False/ResizePendingPowerOff` and no
  longer counts toward the circuit breaker. vSphere checks first whether vCenter can apply
  the change now (hot-add settings, fewer vCPUs without hot-remove, less memory, memory above
  the hot-plug limit, a disk on a shared bus or with multi-writer sharing, any change of a
  suspended VM) and refuses the whole request before changing anything. The VM stays
  `Ready`; set `spec.powerState: Off`, then `On`, to apply it.
- **A manager restart could take a second snapshot or land a second disk:** a lost
  `SnapshotCreate`, `ExportDisk` or `ImportDisk` result is no longer re-sent blindly. A
  durable marker (`SnapshotRequested`, `ExportRequested`, `ImportRequested`) is written
  before the call, with a `resourceVersion` precondition (if the write fails, nothing is
  sent); a failure without a definite answer keeps it and holds the object; the call is
  re-sent only where that cannot duplicate anything: a clustered `SnapshotCreate` (adopted by
  request token) and an `ExportDisk` / `ImportDisk` on a staging backend the provider lists
  in `resend_safe_export_backends` / `resend_safe_import_backends`, 35 minutes after the
  marker, at most 3 times (10 for the routed snapshot). To release a held object, check what
  the call may have made (the snapshot named in the event, the target's `-migrated` disk),
  remove it, then confirm the attempt:

  ```sh
  ATTEMPT=$(kubectl get vmmigration <name> -n <ns> \
    -o jsonpath='{.status.conditions[?(@.type=="ExportRequested")].lastTransitionTime}')
  kubectl annotate vmmigration <name> -n <ns> virtrigaud.io/operation-outcome=not-performed@$ATTEMPT
  ```

  Or delete the object (a deleting migration with an unknown export waits 35 minutes before
  it deletes the staged object). Single-host libvirt answers the failures it can prove (a
  `qemu-img` error, a missing source domain, a taken snapshot name) as "not performed" or a
  typed refusal, and keeps `Unknown` for what it cannot prove. Alert on migrations or
  snapshots stuck in a phase, and on the three `...OutcomeUnknown` reasons
  (→ [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md)).
- **A failed or deleted migration left its landed disk on the target for good,** and every
  vSphere import left about 1.4 KB of import-VM files (`.vmx`, `.vmsd`, ...) in the landing
  folder. Before the import the migration records `LandedDiskCleanup=Unknown/LandedDiskMayExist`
  and the import target (VM and Provider UID); the manager then asks the target provider to
  delete the landed disk (RPC `DeleteLandedDisk`, capability `supports_landed_disk_delete`),
  following the staged disk's `cleanupPolicy`: `OnSuccess` (default) when the `VMMigration`
  is deleted, `Always` also when it fails for good, `Never` keeps it (`LandedDiskRetained`).
  It fails closed: a successful migration's disk, a disk its target `VirtualMachine` has, a
  disk any VM uses (`LandedDiskInUse`) and a disk another in-flight migration lands under the
  same name (`LandedDiskHandedOver`) are kept; a delete that fails is retried and holds a
  deleting `VMMigration` (`LandedDiskCleanupPending`; `virtrigaud.io/force-delete=true`
  releases it); an older provider image, a gone Provider or a refused request records
  `LandedDiskLeftBehind` with one Warning. Disks left by earlier releases are not touched
  (→ [ADR-0011 Addendum A](docs/adr/0011-migration-staging-cleanup.md#addendum-a--landed-disks-rc2)).
- **A Proxmox s3 stage could be published truncated** after a cancelled import: it is
  published only when exactly the object's size arrived.
- **libvirt read a failed `snapshot-list` as the snapshot's absence;** it is now an error.
- **vSphere RPCs raced on a shared finder** (each RPC now has its own finder, scoped to the
  default datacenter; the datacenter reference is cached for 60 seconds per vCenter
  client); **libvirt wrote fixed `/tmp` files on the hypervisor** (pool definition and the
  persistent-XML sync; the XML now goes to `virsh` over stdin); **a libvirt client stuck
  connected on a dead transport was reused forever** (the keepalive drops the client after a
  failed probe).
- **A `VMMigration` no longer fails with "import did not record disk info"
  (affects v0.3.11).** When two reconciles of a migration overlapped (the
  second started from a cache that did not yet show the first one's write),
  the second one's status write replaced the whole live status with its older
  copy. That could drop `status.diskInfo` (seen on a libvirt → S3 → vSphere
  migration, which then failed at `Creating`), or any other recorded field:
  the staging marker, the source VM's UID, the task reference, even the phase.
  A status write now never replaces a newer status: it either loses the
  conflict and the migration is reconciled again from the live state, or —
  for the result of a snapshot, export or import — only that result is
  written onto the live object; a result that cannot be written yet is held
  and written by the next reconcile, so the manager does not send the
  operation again. A manager restart or leader change between the operation
  and its write can still lose the result; the call is then held or
  re-sent only where that is proven safe (see the lost-result fix above). A result
  dropped because the migration had moved on is reported with a `Warning`
  event `ProviderResultNotRecorded`. The same fix stops the `Provider` controller from
  dropping `status.adoption` written by VM adoption, and adoption from losing
  its status to the `Provider` controller's frequent writes.
- **A vSphere disk export no longer leaks a full copy of the disk on the
  source datastore.** `ExportDisk` clones the disk to a temporary
  `virtrigaud-export-…vmdk` next to it and deleted only the descriptor, so on
  VMFS the `-flat.vmdk` extent (the whole disk) stayed in the VM's folder and
  outlived the VM (found on vCenter 8; also in v0.3.11). The
  clone is now removed with `VirtualDiskManager.DeleteVirtualDisk` on every
  exit, including a clone that failed or was cancelled part-way. Files left by
  earlier exports are not removed: look for `virtrigaud-export-*` files in the
  VM folders of datastores used as migration sources and delete them.
  → [Upgrade guide](docs/upgrading.md#leftover-virtrigaud-export--files-on-vsphere-datastores)

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
  could not record). A Create whose outcome stays unknown is re-sent only to a
  provider that reports `supports_owner_rebind` (the vSphere, libvirt,
  Proxmox and mock providers of this release, which re-bind the VM they stamped;
  an older provider image, including an older Proxmox one, is never re-sent it);
  otherwise the VM waits
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
  timeout) used to fail so that a retry could never make a second VM; it is now
  held and re-sent to a provider that stamps and re-binds
  (`supports_owner_rebind`), and still fails on any other
  (→ [Clones: re-bind](docs/vm-ownership.md#clones-re-bind)).
- **A migration's `nfs` / `s3` staged disk is deleted.** The staged object (a
  full, unencrypted copy of the source disk) was never deleted. The manager now
  asks the migration's target `Provider` to delete it, through a new provider
  RPC, `DeleteStagedDisk`
  (→ [ADR-0011](docs/adr/0011-migration-staging-cleanup.md),
  → [Upgrade guide](docs/upgrading.md#breaking-changes)):
  - **When.** After the migration succeeds, and when the `VMMigration` is
    deleted. With `cleanupPolicy: Always`, also when the migration fails for
    good. With the default `OnSuccess`, a failed migration's staged disk is
    kept until the `VMMigration` is deleted. `Never` keeps it in every case.
  - **Confinement.** The request names the migration, never a bucket, key or
    path. The provider derives the object from the migration's namespace and
    name, so it can delete only that migration's own staging object. A spec
    edited after the export is never deleted at its new location.
  - **A failed delete is held.** The `StagingCleanup` condition is `False`
    (reason `StagingCleanupPending`), one Warning event is sent, and the delete
    is retried with a backoff from 15 s to 5 min. A deleted `VMMigration` is
    kept by its finalizer meanwhile (and while an export task still runs);
    `virtrigaud.io/force-delete=true` releases it.
  - **Older providers.** A provider image older than this release cannot
    delete. The delete is held for up to 24 hours, then recorded as left
    behind (`StagedDiskLeftBehind`).
  - **libvirt and vSphere NFS.** These targets empty an `nfs` object (truncate
    it to zero bytes) instead of deleting it, because qemu-img cannot delete a
    file over NFS. An empty file stays on the export. Proxmox deletes it.
  - **Proxmox import stage.** The Proxmox target also removes the node-side
    import stage of the migration's target VM, unless another migration in
    flight targets the same VM.
  - **Versioned S3 buckets and NFS snapshots** keep the data after the delete:
    expire noncurrent versions on `vmmigrations/`, and exclude the staging
    export from snapshots.
- **Unambiguous NFS staging name.** New migrations stage
  `vmmigrations_<namespace>_<name>_export.qcow2`. The old dash-separated name
  was shared by two migrations such as `team-prod/db` and `team/prod-db`.
  Migrations staged before the upgrade keep their old name.
- **Fixed: `Ready` cleanup, now guarded.** A migration that succeeds under this
  release deletes its source snapshot and runs `deleteAfterMigration`, which a
  broken manager-side PVC cleanup blocked before. A migration completed before
  the upgrade deletes nothing (one `SourceDeletionSkippedPreUpgrade` Warning).
  `deleteAfterMigration` deletes only the source VM the migration validated (by
  UID), never one re-created under the same name.
- **Security: staged images are checked before import.** NFS and S3 imports on
  Proxmox, vSphere and libvirt read the staged object once into a local copy,
  refuse an image that references another file (a backing file, data file or
  extent could have pulled a node or pod secret into the imported disk), and
  import that same copy. Headers are parsed in Go before qemu-img opens
  them. The NFS copy is bounded (`VIRTRIGAUD_MIGRATION_IMPORT_MAX_GIB`,
  default 1024, lowered by the target `Provider`'s disk-size limit, and a
  free-space check). Proxmox stages the copy in its root-only stage directory,
  checks the stage's header again at `Create`, and mounts migration exports
  `nosymfollow,nosuid,nodev,noexec`.
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
- **Every other single-host per-VM call carries the owner too, and Proxmox checks it.**
  Before, only `Delete` was owner-checked on a single-host Proxmox Provider: `Describe`,
  `Power`, `Reconfigure`, the snapshot calls, `GetDiskInfo`, `ExportDisk` and a clone's
  source acted on whatever VM held the recorded VMID, so if PVE reused a VMID a
  `VirtualMachine` could read, power, resize, snapshot, export or clone **another tenant's
  VM**. The manager now sends the owner (UID, namespace, name) on every single-host per-VM
  call (vSphere, single-host libvirt and mock ignore it on these calls), and the Proxmox
  provider checks it before it reads or changes anything: the VM at the VMID must be
  stamped with the requester's UID, or be unstamped and named like the owner (the legacy
  name rule, see Known limitations). Otherwise the answer is `VM_NOT_OWNED` (or
  `OWNER_STAMP_UNREADABLE` for an unreadable or multiple stamp). A refused
  `VirtualMachine` is held, not re-created: `Ready=False/VMNotOwned` (or
  `OwnerStampUnreadable`), a constant message, one Warning event, `status.id` kept, a
  backoff up to 5 minutes; deleting the `VirtualMachine` releases it and leaves the other VM
  alone. A `VMMigration` or `VMClone` whose source is refused fails before any side
  effect; a refused `VMSnapshot` delete is released as already gone; a snapshot create
  fails. **Look for these before upgrading,** they are held afterwards: an unstamped
  Proxmox VM renamed in PVE after VirtRigaud created it (rename it back), a VM adopted
  earlier while it carried a previous owner's stamp (remove the stale `virtrigaud.owner`
  line from the VM's Notes, or delete the `VirtualMachine`, which leaves the VM, and let
  adoption take it again), and a `VirtualMachine` whose VMID was reused by another VM
  (delete the `VirtualMachine`, which leaves the other VM alone, and create it again).
  After the upgrade, search for `Ready` reason `VMNotOwned`, for failed `VMMigration`s and
  `VMClone`s whose message says the source VM was refused, and for `Refusing the call` in
  the Proxmox provider log. The check needs both the new manager and the new Proxmox
  provider
  (→ [Per-VM calls](docs/vm-ownership.md#per-vm-calls-describe-power-reconfigure-snapshots-disks-and-clone-sources)).
- **Single-host adoption stamps the adopted VM** with its new owner (`TransferOwner`, a
  compare-and-swap on the hypervisor: vSphere `config.changeVersion`, Proxmox config
  `digest`, libvirt domain UUID under a per-host lock; capability
  `supports_owner_transfer`), and never adopts a VM bound to a `VirtualMachine` of another
  Provider object with the same type and endpoint. Before, a re-adopted Proxmox VM kept a
  previous owner's stamp and was left behind on delete. The adopting `VirtualMachine` is
  created without `status.id` (annotated `virtrigaud.io/adopted-id`), the VM is stamped
  only over no stamp (or over a stale stamp whose UID no `VirtualMachine` has, only with
  `virtrigaud.io/adopt-stamped-vms`), and only then is `status.id` written; an interrupted
  adoption is finished, bound or removed by the next discovery (every 5 minutes). A
  provider without the capability adopts as before, unstamped, and the Provider's adoption
  status says so. VMs adopted by earlier releases are not backfilled. New privileges:
  vSphere `VirtualMachine.Config.AdvancedConfig` and Proxmox `VM.Config.Options` on the VMs
  adoption takes over
  (→ [Adoption stamps the adopted VM](docs/vm-ownership.md#adoption-stamps-the-adopted-vm)).
- **Single-host `VMClone` targets are created first and stamped, and a re-sent `Clone`
  re-binds** instead of making a second clone (Proxmox) or failing and leaving the first
  clone untracked (vSphere, libvirt). The target `VirtualMachine` is created first,
  unbound; its UID is recorded in `status.targetUID` and sent as `CloneRequest.target_vm`;
  every provider stamps the clone with it. Before a `Clone`, the `VMClone` records
  `CloneRequested=True` with a `resourceVersion` precondition (if that write fails, nothing
  is sent). New `VMClone` states: `Ready=False/CloneOutcomeUnknown` (held, re-sent with a
  15 s to 5 min backoff, only to a provider that reports `supports_owner_rebind`) and
  `Failed` / `ProviderConflict` for a same-named VM the target does not own. A failed clone
  removes the unbound target it created, unless the provider reported a cloned VM or a
  `Clone` of unknown outcome is on record. To abandon a `VMClone` held in
  `CloneOutcomeUnknown`, remove or adopt any VM stamped with its `status.targetUID`, then
  delete the `VMClone` and its target `VirtualMachine` (deleting the `VMClone` never
  touches the target). Clones made before are unstamped and are not re-bound
  (→ [Clones: re-bind](docs/vm-ownership.md#clones-re-bind)).
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
  grant `infra.virtrigaud.io`, so **subjects already bound to them gain that
  access** — review the bindings first. The admin roles list their verbs
  instead of `*`, and the `provider-*` roles are labelled
  `virtrigaud.io/role-scope: platform-admin`.
- A `VMMigration` with `deleteAfterMigration: true` whose target stays off
  (`powerOn: false`) records a `SourceDeletedTargetNotStarted` Warning when it
  deletes the source.
- **A `VMMigration` with `powerOffBeforeMigration: true` on a running source no
  longer fails.** The migration set the source's `spec.powerState` to `Off`
  *and* powered it off itself; the VirtualMachine controller powered it off
  too, and on libvirt the second `virsh destroy` failed on the stopped domain,
  failing the migration terminally (`MigrationFailed: Failed to power off
  source VM before migration`). The migration now leaves the power-off to the
  VirtualMachine controller and waits (`Validating=False/WaitingForSourcePowerOff`,
  at most 10 minutes) until the source is observed off.
- **Power is idempotent on every provider.** A power-off of a VM that is
  already off, or a power-on of one that is already running, succeeds
  (libvirt: `domstate` re-read after a failed `start`/`destroy`/`shutdown`;
  vSphere: `InvalidPowerState`; Proxmox: no task when already in the
  requested state; mock).
- **libvirt no longer rewrites an operator's `default` storage pool.** Since
  v0.3.11, a create or disk import on a host whose `default` pool was not at
  `/var/lib/libvirt/images` destroyed, undefined and recreated it there. An
  existing pool is now never destroyed or redefined (only started when
  inactive); one at another path fails the operation closed with an
  `InvalidSpec` whose constant message says an administrator must fix the
  host (the host and the pool's path are in the provider log, never in a
  status). Mount shared storage
  **at** `/var/lib/libvirt/images` — see the host prerequisites in
  [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#host-prerequisites).
- **libvirt: deleting a snapshot removes its leftover overlay file.** libvirt
  10 commits an external snapshot into its base on delete but leaves the
  overlay `<domain>-disk.<snapshot>` in the pool; `SnapshotDelete` now removes
  it when it is provably unused (same rules as VM delete, cluster-wide on a
  clustered Provider). Overlays from snapshots deleted before the upgrade are
  not swept.
- libvirt `Describe` no longer logs `ERROR` lines on every poll for expected
  probe failures (no guest agent; statistics of a shut-off domain); they are
  `DEBUG`.
- Provider SDK: the `sdk/provider/client` RPC methods (`Create`, `Describe`,
  `TaskStatus` and the others) return a nil error on success again. Before, a
  successful call returned a non-nil error that printed as `<nil>`, and
  `WaitForTask` failed on its first poll.
- **vSphere: a disk import works on a freshly started provider.** A migration
  into vSphere (S3 or NFS) whose `ImportDisk` was the provider's first call
  after a restart failed with `datastore/StoragePod "<name>" not found`: the
  import looked up the datastore before selecting the datacenter.

## Known limitations

These are known in this release and are not regressions from v0.3.11 unless a bullet
says so. Each links to the page that documents it in full, where one exists.

- **Metrics are served over plain HTTP by default.** The manager's
  `--metrics-secure` is still off by default (a change is planned for a later release). `/metrics`
  includes administrator-sensitive gauges, so expose it only behind the chart's
  NetworkPolicy (`networkPolicy.enabled`, default off) or turn on
  `--metrics-secure=true` yourself
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#committed-capacity)).
- **Clustered libvirt providers are experimental.** `topology: cluster` is opt-in
  and may still change. Host-to-host migration is not implemented. Migration into a
  clustered provider is new and experimental: `s3` and `nfs` only, the target VM in the
  migration's namespace; run in the lab from a single-host libvirt source, a vSphere
  source (over S3) and a Proxmox source; the
  disk-space check is a filter, not a reservation; the source disk's size is read once;
  a host that fails after placement is waited for, not replaced; two Providers whose
  hosts mount one pool are not serialised with each other; and a waiting target's
  `spec.importedDisk` can be edited by whoever may edit the VM (a swap is detected, not
  prevented)
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)).
- **A clustered libvirt host's free-space figure is up to one Host sync (60 seconds)
  old** (#417), so the disk-space filter that places a migration target can act on a
  figure that has changed since. If the filesystem behind an active pool is not
  mounted, the figure is that of the filesystem underneath, and a `stat` stuck on a
  dead hard-mounted NFS export cannot be killed and may accumulate until the mount
  recovers
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#what-listhosts-reports-per-host-libvirt)).
- **A migrated guest can boot without network, or with a new identity (R36).** The fix
  is designed, not built. A Linux cloud-image guest whose netplan matches its NIC by
  MAC address (Ubuntu cloud images do) can boot **without network** on a vSphere or
  Proxmox target: the target NIC has a new MAC and cloud-init does not run again. On a
  libvirt target the network works only because the guest gets a new cloud-init
  identity, so its instance-id, hostname and SSH host keys change. The fix, which
  carries the source NICs' MACs to the target and presents the source's instance-id, is
  accepted in [ADR-0014](docs/adr/0014-migrated-guest-network.md) and planned for a later release;
  of its slices only the Proxmox network plumbing is in this release. Windows
  guests are outside the design beyond carrying the MAC. Until then: make the guest's
  netplan match its NIC by name or driver instead of `macaddress` before you migrate,
  or use a guest image that already does, and after a migrated guest comes up without
  network, fix its network configuration from the console
  (→ [ADR-0014](docs/adr/0014-migrated-guest-network.md)).
- **Proxmox applies only two things from `spec.userData` (R35).** The provider does not
  pass the user-data to the guest: PVE builds the cloud-init drive from settings the
  provider sets. From the user-data it reads the `ssh_authorized_keys:` entries written
  as a block list of `- key` lines (a flow list such as `[a, b]` is not read), which
  become the VM's `sshkeys`, and the value on the first line that contains `name:` but
  not `hostname:`, which becomes `ciuser`. It sets no password and no custom user-data
  snippet (`cicustom`), so `packages`, `runcmd`, `write_files` and the rest of the
  user-data do not run on a Proxmox guest
  (`internal/providers/proxmox/server.go`, `parseCreateRequest`).
- **Proxmox networks: part of an attachment is not applied.** `firewall`, `rateLimit` and
  `mtu` of the `proxmox` block, `VMNetworkAttachment.spec.ipAllocation` and
  `spec.networks[].macAddress` have no effect on a Proxmox VM (the manager drops the MAC
  before every provider until ADR-0014 slice 0b); the CRD allows only `vmbrN` bridges
  and IPv4 addresses; and `Describe` reports no NICs. A static IP takes effect only
  when the VM has a cloud-init drive
  (→ [`docs/proxmox-networks.md`](docs/proxmox-networks.md)).
- **Deleting a namespace that holds an in-use `Provider`.** Kubernetes deletes the
  provider's runtime with the namespace, and nothing can stop that. VMs whose
  deletes miss the pod's termination window are held (`ProviderUnavailable`) and
  the namespace stays `Terminating` until an administrator acts
  (`virtrigaud.io/force-delete: "true"` on the `Provider` or on each VM, which
  leaves the hypervisor VMs unmanaged). Delete the VMs before the namespace
  (→ [ADR-0010](docs/adr/0010-provider-in-use-finalizer.md),
  [`docs/vm-provider-binding.md`](docs/vm-provider-binding.md#deleting-a-namespace-that-contains-a-provider)).
- **Proxmox: the legacy name rule can be claimed by whoever can create a VM.**
  An unstamped VM named like its `VirtualMachine` still passes the owner check (VMs
  created before owner stamps exist have no other proof), and the provider cannot tell
  whether the `VirtualMachine`'s own VM was ever stamped. So once a `VirtualMachine`'s
  VM is removed, a PVE user with `VM.Allocate` on a pool or path where VirtRigaud's VMs
  live can create an unstamped VM with that name at the freed VMID, and the
  `VirtualMachine` then reads, powers, resizes, snapshots and copies it as its own.
  A VM stamped for anyone else is refused. Grant `VM.Allocate` there as carefully as
  `VM.Config.Options`, and to no one but VirtRigaud's Provider tokens and your
  administrators. [ADR-0012](docs/adr/0012-proxmox-legacy-unstamped-name-rule.md)
  retires the rule: phase 1 is planned for v0.4.1 behind validation on a real PVE
  cluster, the rule is dropped by default in v0.5.0. Even then a stamp can be copied by
  someone who knows the `VirtualMachine`'s UID and holds `VM.Config.Options`; that risk is
  accepted for v0.4.0 (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#limits)).
- **Proxmox: the owner check and the call are two requests.** A VMID freed and reused
  between them is not caught. Do not delete VirtRigaud-managed VMs outside VirtRigaud:
  delete the `VirtualMachine`, or detach it with `orphan-on-delete`
  (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#limits)).
- **The admission policy needs Kubernetes 1.30+.** On an older cluster nothing is
  installed, and anyone who can create or update `VirtualMachine`s in a namespace can
  forge the adoption markers (`virtrigaud.io/adopted`, `adopted-id`, `adopted-host`) and
  the `VMClone` marker (`clone-uid`). Restrict `create`/`update` on `virtualmachines` to
  trusted users there, or reserve the keys with Kyverno or Gatekeeper. (A `VMMigration`
  no longer depends on the markers.)
  → [`docs/admission-policy.md`](docs/admission-policy.md)
- **A kustomize overlay that changes the namespace or name prefix must repeat the
  `replacements` block of `config/default`.** Otherwise the admission policy keeps
  exempting the old manager name and denies the manager's own writes.
  → [`docs/admission-policy.md`](docs/admission-policy.md#configuration)
- **A leader-election overlap during a migration's target `Create` can still fail the
  migration.** If the new leader finds the target VM carrying a claim it cannot match
  (the old leader's), it re-checks, but if the claim never matches the migration fails
  `TargetVMNotOwned`. This fails closed; delete the target VM it created and re-create
  the migration (→ [`docs/cross-namespace-targets.md`](docs/cross-namespace-targets.md#the-target-vm-must-be-the-migrations-own)).
- **VMs adopted by v0.3.11 are not re-stamped, and clones made by it are
  unstamped.** Their deletes follow the unstamped rules. On Proxmox, a VM adopted earlier
  that still carries a previous owner's stamp is held `Ready=False/VMNotOwned` (and
  left behind on delete with `VMLeftOnHypervisor`) until you remove the stale
  `virtrigaud.owner` line from its Notes (→ [`docs/vm-ownership.md`](docs/vm-ownership.md#vms-adopted-before-this-release)).
- **libvirt: the `default` storage pool must be at `/var/lib/libvirt/images`.**
  Mount shared storage at that path, and keep `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`
  (the image catalog) apart from the pool. A pool at another path is refused with
  `InvalidSpec` and never modified
  (→ [`docs/clustered-provider-inventory.md`](docs/clustered-provider-inventory.md#host-prerequisites)).
- **The disk-size limit does not cover an oversized image.** It checks class and
  spec sizes and the real disk sizes of clone and migration sources, but not an
  image or template whose own disk is larger than the class
  (→ [chart README](charts/virtrigaud/README.md#disk-size-limit)).
- **vSphere: a CPU or memory change of a running VM needs hot-add.** Without it the
  change waits for a power-off and is shown as `Reconfiguring=False/ResizePendingPowerOff`
  instead of being retried against vCenter. The provider still reports online reconfigure
  as supported regardless of the VM
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md#vsphere-changes-a-running-vm-cannot-take)).
- **A disk-size change alone does not trigger a `Reconfigure`.** The manager sends one
  when the effective CPU or memory changes, and the disk sizes travel with it. To apply a
  `spec.disks` size change, or a growth skipped earlier (for example while the disk had
  snapshots), change CPU or memory too
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md#when-a-reconfigure-is-sent)).
- **`Reconfigure` resizes few disks.** No provider's `Reconfigure` grows the root disk
  from a `spec.disks` entry or adds a disk. vSphere grows only an additional disk that
  `Create` recorded under the entry's name, so the additional disks of VMs created by
  v0.3.11 are not resized. Proxmox resizes none.
- **`spec.disks` is reported on libvirt and Proxmox, but only in some places.** The
  `DisksApplied=False` condition and the `Warning` event are always there. The
  admission warning is given only where the `VirtualMachine` validating webhook is
  rendered, which the chart does only with `webhooks.enabled=true` (and
  `webhooks.validating.enabled`, on by default) **and** `manager.maxDiskSize` set;
  without a disk-size limit there is no admission warning.
  vSphere attaches `spec.disks` at create but ignores an entry added after the VM was
  created, with no condition and no event (the provider logs it at `INFO`); nothing
  refuses either case
  (→ [`docs/reconfigure-results.md`](docs/reconfigure-results.md#which-disk-a-size-goes-to)).
- **Snapshot revert on libvirt needs a snapshot taken with the VM powered off, and
  libvirt 9.9 or later.** A snapshot of a VM that is off, or taken without memory, is an
  external disk-only snapshot; older libvirt cannot revert one and the provider reports
  `RevertFailed`. A disk-only snapshot that stores a CD-ROM or read-only disk with media
  (one taken while the VM ran, or before v0.4.0-rc7) is refused on revert, and so is one
  whose stored definition cannot be read. A snapshot of a powered-off VM ejects the seed
  CD-ROM for its duration; if the provider or the host fails in that window the CD-ROM
  stays empty until VirtRigaud's next snapshot create, revert, start or reboot of that VM
  inserts the seed again, and a VM someone starts by hand (`virsh start`) meanwhile boots
  without its seed. A medium other than the VM's own cloud-init seed (an installer ISO you
  attached) that an interrupted ejection left out is never inserted automatically: the
  provider logs it and you insert it by hand. `SnapshotDelete` removes the primary
  disk's overlays, `<domain>-disk.<snapshot>` and the `<domain>-disk.<epoch>` a revert
  makes, and not those of additional data disks, so check the pool
  (`<pool>/*-disk*` files that are not `.qcow2`) after a revert or a snapshot delete
  (→ [`docs/vm-snapshot-revert.md`](docs/vm-snapshot-revert.md#libvirt),
  [`docs/libvirt-clones.md`](docs/libvirt-clones.md#what-snapshotdelete-removes)).
  The libvirt revert was validated in the lab (single-host and clustered, libvirt 10.0,
  AppArmor, an NFS pool); see [Validation](#validation).
- **libvirt: the check that a snapshot delete keeps the VM's other snapshots is not
  atomic with the delete.** `SnapshotDelete` reads the VM's snapshots and the files'
  headers, then sends `virsh snapshot-delete`, with no lock between the two and nothing
  that serialises it against a `SnapshotCreate` of the same VM
  (`internal/providers/libvirt/server.go`, `snapshot_delete_guard.go`). A memory snapshot
  created for the same VM while the delete of an earlier disk-only snapshot is running
  is not seen and could still be orphaned. Do not create a snapshot of a VM while you delete
  another of its snapshots. The check also covers only the files the delete may merge
  away or remove; a snapshot libvirt itself refuses to delete (an internal parent of an
  external snapshot) is libvirt's answer, not this check's
  (→ [`docs/libvirt-clones.md`](docs/libvirt-clones.md#deleting-memory-and-disk-only-snapshots-of-the-same-vm)).
- **libvirt: a memory snapshot orphaned by an earlier version is unusable, and the cleanup is
  limited to what can be proven.** A memory snapshot whose disk-only snapshot was deleted
  first (by v0.3.11 or by v0.4.0-rc8 or earlier) keeps its data in the leftover overlay
  file, but the VM no longer runs on that file, so the snapshot cannot be used or reverted
  to. This release deletes the snapshot's metadata when every file the snapshot recorded is
  gone and no current disk image lists it; otherwise it asks you to power the VM off and
  delete it again, and the manual cleanup in the doc applies
  (→ [`docs/libvirt-clones.md`](docs/libvirt-clones.md#a-memory-snapshot-that-was-already-orphaned)).
- **libvirt: a new disk-only snapshot can collide with an earlier snapshot's leftover
  overlay file (V12).** The overlay of a disk-only snapshot is
  `<pool>/<domain>-disk.<snapshot name>`. When an earlier snapshot of that name left its
  overlay in the pool because the overlay could not be proven safe to remove (for example a
  memory snapshot lost to the delete problem above on rc8 or earlier still referenced it),
  a new disk-only snapshot with the same name fails in libvirt (`external snapshot file for
  disk vda already exists`). On a clustered Provider the provider answers this with a
  generic error (`failed to create snapshot on host "<host>"`, `Unknown` with
  `VM_OPERATION_FAILED`), which the manager cannot read as "nothing was done": the
  `VMSnapshot` never gets a phase: it stays `Ready=False/ProviderError` and the create is
  sent again about once a minute (from the code: after ten re-sends it is held
  `SnapshotOutcomeUnknown`). A single-host Provider recognises libvirt's refusal as "not
  created" and the `VMSnapshot` ends `Failed`; that is from the code and was not run in the
  lab. **Workaround:** use a new snapshot name. Or, after you have verified that no domain
  definition and no snapshot references the file (`virsh dumpxml --inactive` of every domain
  on the host, and `virsh snapshot-dumpxml` of every snapshot), remove the leftover file; a
  `VMSnapshot` that is still being retried then completes on its next attempt (one already
  held `SnapshotOutcomeUnknown` is deleted and created again). A definite refusal is
  planned for a later release.
- **libvirt: snapshot and revert overlays are `0600`, and the disk check needs to read
  them.** libvirt creates `<domain>-disk.<snapshot>` and `<domain>-disk.<epoch>` mode
  `0600`, owned by root while the VM is off, and VirtRigaud does not change them. A
  snapshot create, revert or delete, and a VM delete (on a clustered Provider also a
  create and a clone), of any VM on the host reads the disk chain of every domain on the
  host first. The provider's SSH user must be able to read those overlays: grant
  `VR_DISK_READ` over the pool directory (an NFS export must not squash root, and must
  then be dedicated to the Provider's hosts), or repair each new overlay by hand (`chown
  -h libvirt-qemu:kvm`, `chmod 0640`). Without either, those operations are refused with
  "permission denied reading a disk image" (fail-closed: a VM delete, a snapshot delete
  and a clustered snapshot create are retried; a single-host snapshot create fails and a
  revert is re-sent up to 5 times, see the next two limitations) (→
  [`docs/libvirt-clones.md`](docs/libvirt-clones.md#unreadable-disk-images)).
- **A libvirt guest on a host bridge needs the QEMU guest agent to show `status.ips`.**
  libvirt holds no DHCP lease for a bridged guest (the LAN's DHCP server answers); the
  host's ARP table is a last resort that depends on recent traffic, and a guest on a
  libvirt-managed network such as `default` shows its lease without the agent
  (→ [`docs/vm-status-ips.md`](docs/vm-status-ips.md#libvirt)).
- **A revert the provider definitely did not perform is still counted as an unknown
  outcome.** An answer such as "not performed: could not verify that no other domain uses
  its disks" is retryable, so the revert is sent again (6 minutes after the last attempt,
  doubling up to 30 minutes) and, after 5 attempts, ends `Failed` with "the VM may or may
  not have been reverted" although nothing was changed. Check the VM, then remove
  `spec.snapshot.revertToRef` and set it again (→
  [`docs/vm-snapshot-revert.md`](docs/vm-snapshot-revert.md#asynchronous-reverts-and-unknown-outcomes)).
- **A single-host `VMSnapshot` create the provider did not perform fails instead of
  being retried.** On a single-host Provider the same "not performed" answer that a
  clustered Provider retries with a backoff (V8) ends the `VMSnapshot` in `Failed`
  (`ProviderError`); delete it and create a new one once the cause (for example the
  unreadable overlay above) is fixed.
- **Held calls need an operator.** A `VMMigration` or `VMSnapshot` held in
  `SnapshotOutcomeUnknown`, `ExportOutcomeUnknown` or `ImportOutcomeUnknown` stays held
  until you check the provider and confirm the attempt with
  `virtrigaud.io/operation-outcome=not-performed@<attempt id>` (or delete the object),
  except an export or import on a re-send-safe backend, which the manager sends again
  after 35 minutes. A failure write that is itself lost holds the object too, and
  deleting a held object does not look up the snapshot the lost call may have made
  (→ [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md#limitations)).
- **Remote host commands can outlive a cancelled call, and Proxmox `nfs` is not re-send
  safe.** A command running on the hypervisor host keeps going after the call that
  started it is cancelled. The Proxmox `nfs` import copies to completion and publishes the
  stage at an arbitrary later time, and its `nfs` export writes in place, so both are
  held rather than re-sent (only `s3` is re-send safe on Proxmox).
  Single-host libvirt's flatten, `qemu-img convert` and `dd` run without `timeout(1)`, so
  an orphan can still be running after the 35-minute settle time
  (→ [`docs/provider-call-outcomes.md`](docs/provider-call-outcomes.md#why-each-provider-answers-the-way-it-does)).
- **A `pvc` migration on a filesystem that reports a link count other than 1 is
  refused**, as is any link or special file in the staging PVC. Use `s3` or `nfs`, or a
  filesystem that reports link counts
  (→ [Staged-file hardening](examples/migration/README.md#staged-file-hardening-pvc-backend)).
- **Provider locks are per process: run one replica per Provider.** The landing,
  landed-disk delete and import serialisation live inside one provider process. vSphere
  runtimes are rendered with one replica already; do not set `spec.runtime.replicas`
  above 1 on libvirt or Proxmox Providers either. The landed-disk in-use checks see only
  the VMs of one vCenter or libvirt host, so do not share the landing datastore or the
  `default` pool directory with hypervisors the Provider does not manage
  (→ [Upgrade guide](docs/upgrading.md#behavior-changes-that-are-visible-but-not-breaking)).
- **Landed disks are not deleted in some cases.** A disk is kept with
  `cleanupPolicy: Never`, when the provider reports it in use, when the provider image
  predates this release, the Provider is gone, or a Proxmox Provider has no migration SSH
  data plane (`LandedDiskLeftBehind` says so). Disks left by migrations that failed
  under v0.3.11 are not removed either.
- **NFS staging cleanup does not remove the file on libvirt and vSphere targets.**
  It empties the staged file (a 0-byte file remains); Proxmox deletes it.
  Versioned or object-lock S3 buckets and NFS server snapshots keep the data:
  expire noncurrent versions on `vmmigrations/` and exclude the staging export
  from snapshots (→ [ADR-0011](docs/adr/0011-migration-staging-cleanup.md)).
- **`verifyChecksums` does not checksum NFS migrations.** On the `nfs` backend
  the disk is written by `qemu-img` over NFS, which computes no byte checksum, so
  no source or target checksum is recorded. Integrity rests on the target's
  `qemu-img check` of the converted image. Use `s3` when you need an end-to-end
  SHA-256 comparison
  (→ [ADR-0006](docs/adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md)).
- **Leftovers from earlier releases are not cleaned up automatically:** staged
  migration objects, landed migration disks and vSphere import-VM files of migrations
  that ran under v0.3.11 or a v0.4.0 candidate before rc4 (including the `.hlog` history
  logs of vSphere imports), libvirt snapshot overlay
  files (`<domain>-disk.<snapshot>`), libvirt cloud-init seeds in `/tmp` (readable by
  other local users until you make them private, and seed directories of VMs deleted
  together with a clone) and vSphere `virtrigaud-export-*` files. Find and remove them as described in
  [docs/upgrading.md](docs/upgrading.md#leftovers-from-earlier-releases).

## Validation

v0.4.0 went through nine release candidates, and each was run in the lab on real
hypervisors: Kubernetes 1.34, a vCenter, libvirt 10.0 (Ubuntu 24.04, AppArmor) as a single
host and as a two-host clustered Provider on a shared NFS pool, and a single Proxmox VE 9.2
node (not a PVE cluster). Code that changed in a candidate was re-run on the candidate that
changed it; the results below name that candidate in brackets, and an area that no later
candidate touched was not re-run on rc9, which is the build v0.4.0 ships. Please try the
release on a non-production cluster first and report what you find (GitHub issues; include
the Provider type, the `kubectl describe` of the object involved and the provider log, never
a Secret).

### Validated in the lab

- **Ownership and admission (rc2).** The admission policy is enforced even for
  `system:masters`, and the manager's and the users' keys pass; a planted same-named target
  VM fails a migration at `Validating` before any side effect; adoption re-stamps a detached
  VM so that deleting it works; single-host clone targets are stamped.
- **Reconfigure, phase, host files (rc2).** The `PHASE` column is filled in; libvirt
  definition sync goes through `virsh define /dev/stdin` and leaves nothing in the host's
  `/tmp`; a vSphere CPU change on a running VM without hot-add is held as
  `ResizePendingPowerOff` (still `Ready`) and applied after a power-off, with the root disk
  left alone.
- **Migrations through S3, NFS and a PVC (rc2).** libvirt → S3 → vSphere, and a
  `pvc`-backend vSphere → libvirt, end to end: request markers cleared, the source provider
  mounting the PVC read-write and the target read-only only while the migration ran, the
  migration-created PVC deleted with the migration, the landed disk kept while its target VM
  uses it, and the import VM's `.vmx` and `.vmsd` removed from the landing folder.
- **libvirt cloud-init seeds and clone identity (rc2, rc4, rc5).** A seed built as the
  provider builds it is handed to `libvirt-qemu:kvm` at start (`dynamic_ownership`), the
  guest's cloud-init reads it (status `done`, hostname and instance-id applied), AppArmor
  denies nothing, and other local users are denied; a create, a single-host clone of a
  started source (reading only the private copy), the refusal of a `0755` seed directory with
  nothing written and the recovery after `0711`, and a delete that removed the new and a
  legacy `/tmp` seed. A clone got its own sealed seed with `instance-id`
  `virtrigaud-<target UID>` and its own hostname, on single-host libvirt and, for a clustered
  clone of an Ubuntu cloud-image VM (source powered off; a started source is held with
  `SourceMustBePoweredOff`), with a new SSH host key too. On a real vSphere a clone showed a
  new `instance-id`, hostname and SSH host key in the guest (the `guestinfo` rewrite), and a
  vSphere import left no `.hlog` in the landing folder.
- **Migration into a clustered libvirt Provider (rc2, rc4, rc5).** A single-host libvirt VM
  into the cluster over `s3` and over `nfs`, both to `Ready`; the target placed before the
  source was powered off and exported; the landed disk attached in place on the chosen host;
  a failure after the import, with a routed `DeleteLandedDisk` to the recorded host; a host
  made unreachable after placement (the migration waited, did not re-place, and recovered).
  vSphere → clustered over S3 and over NFS, Proxmox → clustered over S3 and over NFS, and
  clustered → Proxmox over S3 (the first export out of a clustered Provider). A manager
  restart during an import held two migrations as `ImportOutcomeUnknown` and re-sent them 35
  minutes after the request, as documented; no duplicate disk was left. libvirt's cached
  pool figure could differ between hosts sharing an NFS pool, which is why a Host's
  `allocatableStorageBytes` is measured fresh.
- **Proxmox on one PVE 9.2 node (rc4, rc5, rc6).** A create from a template with user-data
  (the cloud-init drive stays on the template's storage, with no `local` workaround), a
  power off and on (the phase settles to `Stopped` and `Running`), a `VMSnapshot` create and
  delete, a `VMClone` with a new identity, and a delete. A VM with two NICs, one on an
  isolated VLAN-aware bridge with a VLAN tag and a static IP, applied in the guest. Migrations
  over S3 between vSphere and Proxmox (both directions; the guests had no network, see Known
  limitations) and between single-host libvirt and Proxmox (both directions; the guests were
  reachable); over NFS, an import into Proxmox from single-host libvirt and an export to
  it; a single-host libvirt → Proxmox migration landed on the bridge and VLAN its target
  names.
- **Clustered libvirt on two hosts (rc5, rc6).** `Spread` placement across both hosts; the
  committed CPU and memory metrics counted each VM once; power off and on; an offline CPU or
  memory change applied, and an online change beyond the domain's live maximum reported
  `Reconfiguring=True/RestartRequired` and applied after a power cycle; create and delete of
  a disk-only snapshot (the delete merged the overlay back and no file was left); a cordoned
  host skipped, `Placed=False/Unschedulable` with the filter breakdown when both were
  cordoned, and placement within about 40 seconds of the uncordon. A host made unreachable
  from the provider: its `Host` went `Ready=False`, a `Describe`, a power change and a delete
  of its VMs were held (the delete kept its finalizer and the domain, with
  `DeleteBlocked=True/HostUnreachable` naming the Host and one `Warning` event) and all
  converged within seconds once the host answered, with no domain, disk or seed left;
  `kubectl get hostpools` showed `TOTAL`, `READY` and `SCHEDULABLE` as 2/2/2, dropped to 1
  while a host was unreachable, and went back within seconds. After `orphan-on-delete`, a
  re-created VM was held with `RestorePending` (phase `Pending`), and the [re-attach
  runbook](docs/clustered-restore.md#re-attach-runbook) bound the running domain without a
  restart. Deleting a Provider that still had a VM showed `DeleteBlocked=True/ProviderInUse`,
  kept the runtime, and released after the VM was deleted. A `spec.disks` entry on a
  clustered libvirt VM showed `DisksApplied=False/NotSupportedByProvider` and one `Warning`
  event, `Ready` unaffected.
- **A clustered host that stops answering, under load (rc7).** With a host made unreachable
  while VMs on it were reconciled, every failed call (12 `Describe`s) was answered
  `Unavailable` within its deadline, none `Unknown` or slow.
- **Snapshot revert (rc6, rc7, rc8, rc9).** On vSphere and on Proxmox (an asynchronous PVE task) (rc6) and on libvirt,
  single-host and clustered (rc7, again on rc8 and rc9): a guest file written after the
  snapshot was gone and the earlier one kept, the record stayed at one attempt (no second
  revert), and on libvirt the seed CD-ROM was back as the read-only raw ISO and no
  `<file>.<epoch>` overlay or stray file was left in the seed directory. A revert to a
  snapshot an older provider took, and to a disk-only snapshot taken while the VM ran, was
  refused with nothing changed on the host. A memory snapshot of a running seeded VM was
  created and reverted: the VM booted with cloud-init `done` and its seed present. The
  manager rolled with three VMs still carrying a completed `spec.snapshot.revertToRef` and
  none was reverted again (the once-per-request record held across the upgrade).
- **The disk check over snapshot overlays (rc8).** An offline snapshot and a revert of one
  VM left a fresh `0600` root-owned revert overlay and a three-level nested backing store in
  its definition; another VM's live snapshot, its snapshot delete, a clustered full clone and
  the clone's delete all succeeded.
- **A bridged lab network and `status.ips` (rc7, rc8).** A VM attached with
  `spec.network.libvirt.bridge` was placed only on the host labelled
  `net.virtrigaud.io/<bridge>`, and on either once both were labelled; the guests got
  addresses from the lab's DHCP and were reachable directly. A vSphere → clustered libvirt
  migration over S3 landed on the bridge, and the guest was reachable with its data. NAT
  guests without the guest agent showed their DHCP lease; bridged guests with the agent
  showed only their lab-network address (no loopback, no link-local); a bridged guest
  without the agent showed its address through the host's ARP entry; Proxmox guests without
  the agent showed none, as documented.
- **Deleting snapshots of one VM in either order (rc9).** On v0.4.0-rc9 (Kubernetes 1.34,
  libvirt 10.0 single-host and two-host clustered on a shared NFS pool): a disk-only
  snapshot of a running VM, then a memory snapshot, then deleting the disk-only snapshot was
  refused `DeleteBlocked` naming the memory snapshot, with nothing changed on the host.
  Deleting the memory snapshot first and then the disk-only one succeeded for both, and no
  snapshot or overlay file was left. The libvirt snapshot revert, clustered and single-host,
  passed again.

### Not validated

- **Proxmox beyond the list above.** Adoption of a Proxmox VM, a reused VMID under a managed
  `VirtualMachine` and the ADR-0012 name rule (which needs a PVE cluster, not one node), a
  `Reconfigure` (resize), the import-stage delete of a failed migration, and a PVE cluster of
  more than one node. The owner stamps, per-VM owner checks, clone re-binding, imported-disk
  confinement and NFS/S3 import hardening on Proxmox ran against the test fake before they
  ran on the node above; try a path that is not on the list on a lab node first.
- **The clustered `VMSnapshot` create backoff.** No provider answered "not performed" during
  a run; it is covered by unit tests only.
- **A clustered migration target in three cases:** a host made unreachable after the disk
  landed, a target VM held by the restore guard, and a retry after an import.
- **The V12 case,** a snapshot name that collides with a leftover overlay: the clustered
  answer is from the lab, the single-host answer is from the code only.
- **The admission warning for `spec.disks`.** The lab's VM webhook is not rendered, so only
  the condition and the event were seen.
- **An SELinux libvirt host,** and any Kubernetes version other than 1.34.
- **Paths that need a fault to trigger:** held provider calls after a manager restart (apart
  from the import above), landed-disk deletion after a failed import on every backend, and
  the staged-file link refusal. They are covered by unit, envtest and simulator tests only.

## Upgrade

See **[docs/upgrading.md](docs/upgrading.md)** for the full breaking-change
table, required upgrade order, new required privileges, and a post-upgrade
verification checklist. The roll order is **CRDs, then the cross-namespace grants
(`spec.consumerNamespaceSelector`) and the admission policy, then the manager, then
the providers**, and roll the providers promptly after the manager: until a
provider is upgraded, a Proxmox provider does not enforce the new owner checks, and
no provider image older than this release is ever sent a `Create` or `Clone` again
after an unknown outcome, a lost export or import is held for an operator instead of
re-sent, and a failed migration's landed disk is recorded as left behind instead of
deleted. The order was validated in the lab on the release candidates (see
[Required upgrade order](docs/upgrading.md#required-upgrade-order)).

From v0.3.11, three things to do before the new manager and providers run, on top of that
order:

1. **On every libvirt host, create the cloud-init seed directory** (the command is under
   [Breaking changes](#breaking-changes)), before any libvirt provider of this release runs.
2. **Remove `spec.snapshot.revertToRef`** from every `VirtualMachine` and `VMSet` template
   you do not want reverted, before the manager rolls (it did nothing before; it reverts
   now).
3. **Grant the new privileges:** the vCenter privilege `VirtualMachine.Config.AdvancedConfig`
   (and `System.Read` on the root folder, propagated, for migrations), the PVE token's
   `VM.Config.Network`, `VM.Config.Cloudinit`, `SDN.Use` on its bridges and `VM.Config.Options`
   on adopted VMs, and on libvirt hosts `VR_DISK_READ` over the pool directory.

### Upgrading from a v0.4.0 release candidate

v0.4.0 ships the code of v0.4.0-rc9: from rc9 there is no code, CRD, RBAC or proto
difference, so a `helm upgrade` to the new chart version only re-tags the images. Do not
use `v0.4.0-rc3` images: that tag was never released (its release run was stopped by the
image scan, CVE-2026-63209). A row applies if you are on that candidate or an earlier one:
do every row that applies to you, oldest first. Where a row says **first** or **before**,
do that before anything is rolled; then apply the CRDs, roll the manager and the providers
in the usual order.

| If you are on | What is still needed |
|---|---|
| rc1 | CRDs first (two optional status fields are new: `VMMigration` `status.storagePVCUID` and `status.storageInfo.importTarget.providerUID`; an old CRD prunes them), then the admission policy and any cross-namespace grants (exempt your restore tool in `admissionPolicy.reservedKeys.exemptUsernames` before the next restore), then the manager, then **all** providers promptly (three capability fields are new, so an older provider gets the conservative path: lost exports and imports are held, landed disks recorded as left behind). Let migrations in `Creating` or `Validating-Target` finish before the manager rolls (`TargetVMNotOwned`). The manager upgrade rolls each provider pod that mounts migration PVCs once, which interrupts an export or import running at that moment. Rolling a vSphere or Proxmox provider back brings back the wrong-disk resize; hold CPU and memory changes of VMs with `spec.disks` while an older image runs. A rolled-back manager can leave an adoption annotated `virtrigaud.io/adopted-id` without `status.id`: delete that `VirtualMachine` (no provider call is made) and let adoption take the VM again. |
| rc2 or earlier | **First, on every libvirt host, create the seed directory.** Apply the CRDs (`VirtualMachine`, `VMSet` and `VMMigration` changed) before the manager. Roll the clustered libvirt provider before you run a migration into it (an older one is refused as a target with `ClusteredTargetUnsupported`). Every provider pod rolls (Go 1.26.10). Review who can create `VMClone`s, and `VMPlacementPolicy` `minDiskSpacePerHost` on clustered Providers. Seeds in `/tmp` and seed directories of VMs deleted together with a clone are not cleaned up ([Leftovers from earlier releases](docs/upgrading.md#leftovers-from-earlier-releases)). |
| rc4 or earlier | Grant the PVE token `VM.Config.Network`, `VM.Config.Cloudinit` and `SDN.Use` on the bridges your VMs use (skip for the root user or the `Administrator` role) **before** you roll the Proxmox provider, then roll the manager and the Proxmox provider together. Optionally set `PVE_DEFAULT_STORAGE` on Proxmox Providers whose clones can have no storage-backed disk. Review each `VMNetworkAttachment` a Proxmox VM uses, because the bridge and VLAN they name are now applied. |
| rc5 or earlier | **Before the manager rolls,** remove `spec.snapshot.revertToRef` from every `VirtualMachine` and `VMSet` template you do not want reverted (list them with the query under [Breaking changes](#breaking-changes)). Apply the CRDs first: `VirtualMachine` gains `status.snapshotRevert`, `HostPool` gains printer columns, `VMSet` a description; a manager whose `VirtualMachine` CRD prunes `status.snapshotRevert` sends no revert (`SnapshotReverted=False/Unsupported`), and the readiness check does not verify the field. The manager's role gains `hostpools/status` (the chart and `config/rbac/role.yaml` carry it; a kustomize install must reapply the role). Roll the clustered libvirt provider promptly after the manager. |
| rc6 or earlier | If you reverted a libvirt VM to a snapshot on rc6, **repair it first**: its CD-ROM is on a `cloud-init.<digits>` file in its seed directory and it does not start ([Repairing a VM broken by a v0.4.0-rc6 revert](docs/vm-snapshot-revert.md#repairing-a-vm-broken-by-a-v040-rc6-revert); do not delete its `VMSnapshot` before step 4 of the runbook). The VMs that carry a finished request are listed by `kubectl get virtualmachines.infra.virtrigaud.io -A -o jsonpath='{range .items[?(@.status.snapshotRevert.phase=="Succeeded")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`. Roll the libvirt providers (single-host and clustered), then take new snapshots, with the VM powered off, of the VMs you want to be able to revert, and delete the old ones: until then a revert to an old snapshot of a VM with a seed is refused with `RevertFailed` and changes nothing. |
| rc7 or earlier | Apply the CRDs (only the description of `status.ips` changed since rc7). On every libvirt host whose provider SSH user cannot read libvirt's `0600` snapshot overlays, grant `VR_DISK_READ` over the pool directory ([Unreadable disk images](docs/libvirt-clones.md#unreadable-disk-images)) or repair the overlays by hand, then roll the manager and the libvirt providers. Check the grant with `sudo -l -U <ssh user> /usr/bin/qemu-img info -U -f qcow2 --output=json -- <file>`. A `VMSnapshot` that rc7 held in `SnapshotOutcomeUnknown` for this cause stays held until you release it with the `virtrigaud.io/operation-outcome` annotation. |
| rc8 or earlier | No CRD change. Roll the libvirt providers (single-host and clustered) first, then the manager. Until a libvirt provider is rolled, do not delete a disk-only snapshot of a VM that has a memory snapshot. The new delete check reads more overlay files than rc8's checks did: where the provider's SSH user lacks `VR_DISK_READ` over the pool directory (see the rc7 row), deleting a disk-only snapshot is refused, retryably, until it is granted. Rolling a libvirt provider back to rc8 brings back the orphaned memory snapshot. |

Rollback caveats for each of these steps are in
[Rollback caveats](docs/upgrading.md#rollback-caveats).
