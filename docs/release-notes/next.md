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

See the full breaking-change table, required upgrade order (CRDs → manager →
providers), and rollback caveats in
**[docs/upgrading.md](docs/upgrading.md)**.

## Highlights

### Security

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
- Provider SDK: the `sdk/provider/client` RPC methods (`Create`, `Describe`,
  `TaskStatus` and the others) return a nil error on success again. Before, a
  successful call returned a non-nil error that printed as `<nil>`, and
  `WaitForTask` failed on its first poll.

## Upgrade

See **[docs/upgrading.md](docs/upgrading.md)** for the full breaking-change
table, required upgrade order, new required privileges, and a post-upgrade
verification checklist.
