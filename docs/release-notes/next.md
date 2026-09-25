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
- The manager's webhook and metrics servers pin an explicit TLS 1.2 floor.
- Optional, opt-in `NetworkPolicy` templates for the manager and provider pods
  (`networkPolicy.enabled`, default off).
- Provider pods run under a dedicated, token-less ServiceAccount instead of the
  namespace `default` SA.

### Clustered providers (experimental, opt-in)

- `topology: cluster` libvirt providers can now be scheduled, created,
  described, deleted, powered, and reconfigured across a `Host`/`HostPool`
  inventory (ADR-0007). Still experimental — snapshots, clones, and disk
  export/import on a clustered provider are not implemented yet.

### Fixes

- vSphere `Describe` no longer treats a transient vCenter error as "the VM is
  gone" (which used to trigger a spurious re-create).
- libvirt: hardware-accelerated `<domain type='kvm'>` is used again on hosts
  with a readable `/dev/kvm`, instead of falling back to software emulation.
- Chart: `networkPolicy` (previously `security.networkPolicies`) is never
  silently enabled by `helm upgrade --reuse-values`.
- Examples: every manifest under `examples/` applies against the current CRDs
  again (quoted `powerState` values, fields updated to the v1beta1 API), and
  CI now dry-runs them all.

## Upgrade

See **[docs/upgrading.md](docs/upgrading.md)** for the full breaking-change
table, required upgrade order, new required privileges, and a post-upgrade
verification checklist.
