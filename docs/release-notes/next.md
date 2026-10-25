<!--
Release notes for VirtRigaud v0.4.1; title the release "v0.4.1" and, for a release candidate,
"v0.4.1-rc<N>" (mark it as a pre-release). v0.4.1 is a patch release of v0.4.0: fixes, honesty
about what the code does, and tooling. The notes are cumulative since v0.4.0 and are updated
for each candidate; this file starts with rc1. The v0.4.0 notes are kept in v0.4.0.md. See
docs/upgrading.md ("Upgrading from v0.4.0 to v0.4.1") for the upgrade guide these notes link to,
and CHANGELOG.md for the complete, dated changelog.
GitHub's limit for a release body is 125,000 characters; this file is far below it (about
43,000 characters), so it is pasted whole.
-->

This is **v0.4.1**, a patch release of v0.4.0. It changes how a Proxmox VM gets its cloud-init
user-data, closes the libvirt snapshot problems that v0.4.0 listed as known limitations, makes
`vcts` and `virtrigaud-loadgen` do what their documentation says, publishes the SDK and proto
Go modules, and corrects the published descriptions of the API and the gRPC contract:

- **Proxmox: the whole user-data reaches the guest.** The provider builds a NoCloud seed on the
  PVE node over SSH, so `users`, `write_files`, `packages`, `runcmd` and every other cloud-config
  key apply, as on libvirt and vSphere. **This needs SSH credentials in the Provider's Secret
  and is breaking for Proxmox users whose VMs carry user-data.**
- **Proxmox:** linked clones are no longer advertised (they were never made), a clone gets the
  class's memory, and a clone's boot order names only its boot disk.
- **libvirt snapshots:** a snapshot whose overlay file is already on the host is refused for
  good (it was a known limitation in v0.4.0), a failed revert is answered by what the provider
  can prove, and "not performed" answers are retried instead of counted as unknown outcomes or
  failed. Host commands no longer log staged-disk URLs.
- **Command-line tools:** `vcts run` runs the conformance tests, `virtrigaud-loadgen` performs
  real power, reconfigure, snapshot and clone operations, and `vrtg-provider init` can scaffold
  a Proxmox provider.
- **Provider authors:** each release now tags the `sdk` and `proto` Go modules.
- **Helm:** the `virtrigaud-provider-runtime` chart is deprecated and is removed in the next
  minor release.

**Several changes are breaking or change default behavior.** Read the upgrade guide before
upgrading: **[Upgrading from v0.4.0 to v0.4.1](../upgrading.md#upgrading-from-v040-to-v041)**.
The CRDs have no schema change (only their `description` texts differ) and the gRPC contract
has no field, number or RPC change.

The notes below are cumulative since v0.4.0. If you are upgrading from v0.3.11 or from a
v0.4.0 release candidate, follow [Upgrading from v0.3.11](../upgrading.md) or [Upgrading from a
v0.4.0 release candidate](../upgrading.md#upgrading-from-a-v040-release-candidate) as well; the
[v0.4.0 notes](v0.4.0.md) describe that release.

## Breaking changes

### Actions required

- **A Proxmox `Provider` whose VMs carry `spec.userData` needs the SSH data plane.** Add these
  keys to the Provider's credentials Secret (the one `spec.credentialSecretRef` names) **before
  you roll the Proxmox provider**:

  ```yaml
  stringData:
    token_id: "virtrigaud@pve!vrtg-token"      # unchanged: the API token
    token_secret: "<token secret>"
    ssh_user: "root"                           # must be root; "root" is the default
    ssh_privatekey: |                          # or ssh_password
      -----BEGIN OPENSSH PRIVATE KEY-----
      ...
    known_hosts: "pve.example.com ssh-ed25519 AAAA..."   # ssh-keyscan from a trusted host
  ```

  The key is `root`'s on the PVE node that the Provider's `endpoint` names, and `genisoimage`
  and `flock` must be installed there (both are on every stock PVE node: `genisoimage` is a
  dependency of `qemu-server`). Without the keys, a `Create` or `Clone` that needs a seed is
  **refused before anything is created** (`InvalidSpec`, with a message that names what is
  missing). A `VirtualMachine` shows `ValidationError` and is rechecked every 30 seconds; a
  `VMClone` ends `Failed` (`ProviderError`), so re-create it after you fix the Secret and roll
  the provider. In v0.4.0 such a `Create` made a VM that got only a user name and the
  `ssh_authorized_keys` items. A VM **without** `spec.userData` needs
  nothing new. Treat the Secret as root on the node: the seed's `qm importdisk` and `qm set` run
  as `root@pam` over SSH, outside the API token's permissions.
  → [Proxmox cloud-init](../proxmox-cloud-init-storage.md#prerequisites),
  [Upgrade guide](../upgrading.md#proxmox-user-data-needs-the-ssh-data-plane)
- **In a Proxmox cluster, the VM must be on the node SSH reaches.** The seed is built and
  attached on the node of the API endpoint's host, so set `PVE_NODE_SELECTOR` (in the
  Provider's `spec.runtime.env`) to that node. An `endpoint` that is a load balancer in front of
  several nodes does not work for VMs with user-data. A `Create` or `Clone` that would put the
  VM on another node is refused. The selector applies to every new VM of the Provider, with or
  without user-data: its templates (and clone sources) must be on that node, or a template
  `Create` is refused (`template ... not found on node ...`).
- **A Proxmox VM made without a template, with user-data, needs two more token privileges:**
  `VM.Audit` and `VM.Config.Options` on the VM. Such a VM is now created first and marked ready
  after its seed is attached (its config is read and its description written), as a clone
  already was.

### Behaviour changes

- **A new Proxmox VM with user-data on node-local storage cannot be moved to another node
  while its seed is attached.** `qm migrate` refuses a VM with a local CD-ROM image other than
  its own PVE cloud-init drive, online or offline, so HA and node evacuation cannot move it.
  Put the VM on a shared storage that has the `images` content type, or detach and free the seed
  first (`pvesm free <volume>`). Existing VMs keep their PVE cloud-init drive and are not
  converted.
  → [Known limitations](../proxmox-cloud-init-storage.md#known-limitations)
- **A Proxmox clone does not carry its source's user-data.** A clone of a VM that has a seed, or
  a `VMClone` with inline `spec.customization.userData.cloudInit.inline`, gets a seed of its own:
  `instance-id` `virtrigaud-<target VirtualMachine UID>`, its own hostname, the `VMClone`'s inline
  user-data (else an empty `#cloud-config`) and DHCP on its own MACs. This differs from libvirt
  and vSphere, whose clones re-run the source's user-data. The clone's first boot is a new
  cloud-init instance (new SSH host keys; modules that run once per instance run again with the
  distribution's defaults, for example `disable_root` on most cloud images). Give the `VMClone`
  inline user-data that sets what the clone must keep. A clone of a VM **without** a seed and
  without inline user-data is unchanged.
  → [Proxmox cloud-init](../proxmox-cloud-init-storage.md#lifecycle)
- **A Proxmox `VMClone` with `spec.options.type: LinkedClone` is refused.** It fails with reason
  `LinkedCloneUnsupported` before the provider is called. The provider never made linked clones
  (every clone was a full clone) and no longer advertises them; a vSphere `VMClone` is not
  affected.
- **A `VMSnapshot` create that a single-host provider did not perform is retried** (libvirt's
  answers are the common case; the controller change applies to every provider type, including a
  request that never reached the provider). The
  `VMSnapshot` shows `Ready=False/ProviderError` and the create is sent again after 1 minute,
  doubling up to 10 minutes, for as long as the object exists. In v0.4.0 it ended `Failed` and
  you deleted it and created a new one; a clustered Provider already retried. A cause that does
  not go away (an unreadable disk image, a domain libvirt keeps refusing) is retried every
  10 minutes until you fix it or delete the `VMSnapshot`.
  → [Provider call outcomes](../provider-call-outcomes.md)
- **A revert the provider did not perform is retried and no longer ends the request `Failed`.**
  The revert goes back to `Pending` with the new reason `RevertNotPerformed` and is sent again
  after 1 minute, doubling up to 10 minutes. In v0.4.0 it was re-sent as an unknown outcome and,
  after 5 attempts, ended `Failed` with "the VM may or may not have been reverted" although
  nothing had changed. The one exception is a "not performed" answer to the manager's own
  re-send of an attempt whose outcome is unknown: it proves nothing about that attempt and stays
  an unknown outcome. Remove `spec.snapshot.revertToRef` to give up.
  → [Snapshot revert](../vm-snapshot-revert.md#asynchronous-reverts-and-unknown-outcomes)
- **A libvirt snapshot whose name matches a leftover overlay file is refused.** The
  `VMSnapshot` ends `Failed`, nothing is created and the file is never removed. Use another
  snapshot name, or remove the file after you have checked that nothing references it. A
  `VMSnapshot` that v0.4.0 held in `SnapshotOutcomeUnknown` for this reason stays held until you
  delete it or confirm its attempt.
  → [libvirt snapshots](../libvirt-clones.md#a-snapshot-name-whose-overlay-file-is-still-on-the-host)
- **`vcts validate` and `vcts run` reject conformance specifications that were only loosely
  checked before.** Specifications are decoded strictly: an unknown key is an error; a step's
  resource must be an `infra.virtrigaud.io/v1beta1` kind named without a namespace; a created
  object's name must contain `${RUN}`; `wait` steps need a resource and a condition or
  `validate` entries; unknown capabilities, malformed paths, variables or regular expressions,
  duplicate test names and names over 63 characters are errors, and so is a step that creates,
  updates or deletes a `Provider`. A custom specification that
  `vcts validate` accepted in v0.4.0 can be rejected now.
- **`virtrigaud-loadgen` validates the ranges of `vmTemplate.resources`.** A configuration whose
  ranges are outside the `VirtualMachine` `spec.resources` bounds was accepted in v0.4.0 because
  the ranges were not used; it is rejected now. The configuration keys are unchanged.
  `--dry-run` makes no API call and needs no kubeconfig.
- **A `virtrigaud-loadgen` configuration with `power`, `reconfigure`, `snapshot` or `clone` weights
  now does that work on the hypervisor** (on VMs the run created). In v0.4.0 those operations were
  recorded as successful without any call. The snapshots and clones stay in place unless you pass
  `--cleanup`.
  → [CLI tools](../cli-tools.md)

## Highlights

### Security fixes

> **To be filled in by the release coordinator.** A security fix is still in review and will be
> listed here when it is merged. This subsection lists what is merged at the time of each
> candidate; the item below is merged.

- **libvirt host commands no longer log staged-disk URLs (R28).** The shared host-command log
  (the `ERROR Command failed` line and its stderr, the `DEBUG` command lines, the SSH stream
  errors, qemu-img stderr in copy errors and the migration `INFO` lines) printed full command
  lines, including a staged disk's `nfs://` URL with server, export, path and uid/gid. Every URL
  except the libvirt connection URI and `pvc://` is now replaced by `<redacted <scheme> URL>`;
  the S3 bucket name and the NFS server, export and file name are replaced in a staged disk's
  causes; and a single-host export or import whose outcome is unknown answers with a constant
  text instead of the raw cause. The legacy create-from-URL path logs the image URL without
  user-info and query. Logs written by v0.4.0 keep what they contain: treat them as sensitive.
- **The release workflow's `prepare` job runs with `contents: read` only.** It is the job that
  checks the tag and the module versions before anything is built. The tag reaches its shell
  through an environment variable instead of interpolation, and the module tags can only be
  created on the commit the run built.

### Proxmox

- **The whole user-data is applied (R35).** A `VirtualMachine` with `spec.userData` gets a
  NoCloud seed CD-ROM that holds the user-data byte for byte, a meta-data the provider renders
  (`instance-id` `virtrigaud-<VirtualMachine UID>`, `local-hostname` the VM name, the other keys
  of `spec.metaData` kept) and a version-2 network-config rendered from `spec.networks` (one
  entry per NIC, matched by the MAC PVE gave it and named `eth<N>`; DHCP, or the address, default
  route and DNS). The provider sends the three files in a tar archive on the standard input of
  one SSH session, never on a command line, and builds the ISO on the node with `genisoimage`
  in a root-only directory on a tmpfs under a per-VM lock; `qm importdisk --format raw` copies it
  into the VM's own storage and `qm set` attaches it as a CD-ROM. A VM without user-data is
  unchanged. Template clones, VMs made without a template and migration imports with user-data
  all use the seed.
  → [`docs/proxmox-cloud-init-storage.md`](../proxmox-cloud-init-storage.md)
- **Failures are definite and clean up after themselves.** No SSH data plane, no SSH credentials,
  no `known_hosts`, no `genisoimage`, a VM on another node than the one SSH reaches, user-data or
  meta-data over 1 MiB and meta-data that is not a YAML mapping are `InvalidSpec` before anything
  is created; the same refusals found after the VM was made remove it. A seed build whose SSH
  session broke keeps the VM (the outcome is unknown) and the re-sent `Create` finishes it; any
  other failure rolls the VM back. A re-sent `Create` never attaches a second seed. Delete
  removes the seed with the VM.
- **Linked clones are no longer advertised.** Proxmox reports no linked-clone support; the
  provider's `Clone` refuses a linked request, and the `VMClone` controller refuses a
  `LinkedClone` for it. The README feature matrix says so.
- **A clone gets the class's memory.** The `VMClone` controller sends the class as a
  `VMClassSpec` (`cpu` and a `memory` quantity); the provider read only a `memoryMiB` field, so a
  clone got the class's CPU and kept its source's memory.
- **A VM cloned from a template with user-data boots from its boot disk only (R38).** The
  provider set
  `boot: order=<disk>;<cloud-init slot>`, with `ide2` whatever slot the clone's drive was in and
  `scsi0` when the config showed no disk; PVE refuses a boot order that names a device the VM
  lacks, so such a `Create` failed and was rolled back. The order is now the template's own first
  boot disk (else its `bootdisk`, else the first disk), and the cloud-init drive is not in it.
  A `VMClone` and a template `Create` without user-data keep the template's or source's order.
  Prepare templates with `qm set <vmid> --boot order=<boot disk>` (for example `scsi0`).

### libvirt snapshots and reverts

- **A snapshot whose overlay file is already on the host is refused for good (V12).** v0.4.0
  listed this as a known limitation: a clustered Provider answered a generic error and the
  create was re-sent about once a minute until the `VMSnapshot` was held. The provider now probes
  the overlay paths before it changes anything and refuses as `AlreadyExists` (single-host and
  clustered); the message names no host path, and the log names the disk and the file. The
  leftover file is never removed or overwritten.
- **A snapshot libvirt cannot take for this VM fails for good.** A single-host snapshot that
  libvirt refuses before executing because the VM's configuration does not support it, with the
  snapshot list and disk sources unchanged, is now `InvalidArgument` instead of "not created",
  because sending it again cannot help. A busy domain or one in the wrong state stays "not
  performed".
- **A failed revert is answered by what the provider can prove.** "Not performed" for a failure
  before `virsh snapshot-revert` ran, and for a busy or wrong-state refusal with the disk sources
  unchanged; `FailedPrecondition` for a revert libvirt does not support for the VM; `NotFound`
  for a missing snapshot or domain; a terminal failure when libvirt failed during the revert;
  "outcome unknown: retry" when the revert may have run. For a memory snapshot only the
  job-lock refusal counts as "not performed". Single-host answers carry constant texts (they
  carried the raw `virsh` text), and a lost connection mid-revert is no longer reported as a
  terminal failure.
- **A VM delete refreshes the `default` pool (R37).** `virsh vol-list default` stops listing the
  deleted disk files. The refresh is best effort: a failure is logged and never fails the delete,
  and a VM whose disks are in another pool leaves that pool stale until its next refresh.

### Command-line tools and Go modules

- **`vcts run` executes the conformance tests.** It runs `create`, `update`, `delete`, `wait` and
  `validate` steps through the Kubernetes API in the run's namespace, with assertions, variables
  (`${RUN}`, `${PROVIDER}`, `--set NAME=value`) and capability checks (a test the Provider lacks a
  capability for is reported as skipped with the reason). It touches only what its run created,
  cleans up after each test even after a failure, `--timeout` or an interrupt, and never
  creates, changes or deletes a `Provider`. New flags: `--set`, `--keep`, `--cleanup-timeout`,
  `--spec-dir`. The default `--timeout` is 3 hours. Three new tests ship: `vm-reconfigure-offline`,
  `vm-snapshot-create-delete` and `vm-clone-full`; `basic-lifecycle` is rewritten for the v0.4.0
  API. Tests not started when a run is interrupted or reaches `--timeout` count as failed, so a
  partial run never exits 0.
- **`virtrigaud-loadgen` performs real operations.** `power`, `reconfigure`, `snapshot` and
  `clone` act on VMs the run created (v0.4.0 recorded them as successful without calling the
  API). `--observe` records the time until the object's status reflects the request, and
  `--cleanup` deletes what the run created and exits non-zero naming anything left.
  `test/performance/proxmox-loadtest.yaml` is rewritten in the tool's configuration format.
  → [CLI tools](../cli-tools.md#virtrigaud-loadgen)
- **`vrtg-provider init --type proxmox`** scaffolds a Proxmox provider: the core profile and the
  `raw`/`qcow2` disk and `bridge`/`vlan` network types, every operation still `Unimplemented`.
- **The SDK and proto Go modules are published with each release.** A release tags
  `sdk/vX.Y.Z` and `proto/vX.Y.Z` next to `vX.Y.Z`, including release candidates, and
  `vrtg-provider init` from a release binary writes a `go.mod` that requires the three modules at
  its own version, with no `replace` directives, and a `Dockerfile` that builds on its own.
  Releases up to v0.4.0 have no `sdk/` or `proto/` tags. `go install .../cmd/<tool>@vX.Y.Z` does
  not work (the root `go.mod` has `replace` directives); download the CLI binaries from the
  release.
  → [Go modules](../go-modules.md)
- **Build tooling.** The `gosec` findings in the command-line tools are annotated with their
  reason; `.pre-commit-config.yaml` pins the same golangci-lint as CI; CI runs `buf breaking`
  against the pull request's target branch (the check could not run before: it used a rule buf
  v1 does not have and an `except` list that switched off the rules that report a deleted or
  retyped field, a removed RPC or a removed enum value).
- **`providers/catalog.yaml` no longer claims conformance results.** Every conformance profile
  is `skip` (not run) with no report URL or date (the entries claimed passes and test dates
  nobody had run), and the tags are `v0.4.0` instead of 0.1.x. It still lists the `clone` profile
  for Proxmox, which no longer advertises linked clones, and names `v0.4.0` as the provider tag.
- **The `kubectl` image of the CRD upgrade hook is updated** to kubectl 1.35.9 on Alpine 3.24 (was
  1.32.0 on 3.20), which cuts the HIGH and CRITICAL image-scan findings that have a fix from 37 to 6.
  The remaining six are Go standard library and module fixes that no published kubectl release
  has picked up yet.

### API and gRPC contract

- **The published descriptions are corrected; nothing else changed.** The comments on the CRD
  types (`kubectl explain`, the CRD `description`s) and in `provider.proto` no longer carry
  internal tracking references or roadmap wording and say what each field does today. Fields the
  schema accepts but no code acts on say "Accepted but not acted on in this release." (for
  example `VirtualMachine.spec.lifecycle`, `spec.resources.gpu`, `spec.userData.ignition`,
  `Provider.spec.healthCheck`, most of `VMClone` `customization` and `options`, and every
  `VMSet` field: its controller is a stub); status fields no controller writes say "Not
  populated in this release." `ProviderSpec.insecureSkipVerify` (read by the vSphere and
  Proxmox provider pods, for the hypervisor endpoint) and
  `spec.runtime.service.tls.insecureSkipVerify` (read by the manager, for the provider's gRPC
  certificate) are described as the two separate switches they are. A `grpc://` `Host` endpoint
  passes validation but the libvirt provider connects with `qemu+ssh://` only.
- **A new condition reason constant, `RevertNotPerformed`** (`SnapshotReverted=False`, described
  above). The description of `status.snapshotRevert.attempts` now says it counts since the last
  "not performed" answer. There is no schema change.
- **The proto has no field, number, RPC or option change.** The generated Go differs only in
  comments. The disk export and import contract comment is attached to `ExportDiskRequest`, and
  corrects the old statement that `transfer_mode` `auto` is chosen from the providers'
  capabilities: the manager resolves it to `relay`, and no provider implements `direct`.

### Helm chart

- **The `virtrigaud-provider-runtime` chart is deprecated** (`deprecated: true`, version 0.1.2,
  a warning on install) and is **removed in the next minor release**. It was never published to
  the Helm repository or released with the `virtrigaud` chart. It runs a provider `Deployment`
  outside a `Provider` resource, which the manager never connects to, and bypasses the TLS and
  mTLS settings, the credential Secret mount, the dedicated ServiceAccount and the hardened
  security context the manager sets up for a provider. Create a `Provider` resource instead; the
  chart's README maps its values to `Provider` fields.
  → [Upgrade guide](../upgrading.md#deprecated-the-virtrigaud-provider-runtime-chart)
- **The `virtrigaud` chart has no template, RBAC or values change.** `helm upgrade` re-tags
  the images and the CRD hook uses the new `kubectl` image.

## Known limitations

These are known in this release. Each links to the page that documents it in full, where one
exists. The limitations of v0.4.0 that this release fixes (V12, the Proxmox user-data, the
"not performed" revert and snapshot answers, `vcts run` and `virtrigaud-loadgen`, the SDK and
proto module tags) are removed; the ones that still apply are carried forward and checked
against the code.

### New or changed in v0.4.1

- **Proxmox: a VM with user-data is tied to one node and its seed to its storage.** The VM must
  be on the node SSH reaches (the API endpoint's host); the seed is written once, at `Create`,
  so a later change to `spec.userData` does not rebuild it; NICs a template has but
  `spec.networks` does not name get no entry in the seed's network-config, so cloud-init does
  not configure them (a template's own `ipconfigN` is not used); user-data that a `VMClone` gives
  by Secret reference cannot be resolved by the provider and is not applied (logged); and the
  user-data is stored in plaintext in the seed volume for the VM's whole life, so keep
  long-lived secrets out of it
  (→ [`docs/proxmox-cloud-init-storage.md`](../proxmox-cloud-init-storage.md#known-limitations)).
- **Proxmox: the SSH credential is root on the node.** A Provider whose VMs carry user-data holds
  a root SSH credential to the PVE node; PVE attributes the seed's storage and config changes to
  root, and its permission checks do not apply to them. Restrict who can read the Provider's
  Secret and use a key only the provider uses
  (→ [security posture](../proxmox-cloud-init-storage.md#security-posture)).
- **libvirt: a leftover overlay file is never removed for you.** A snapshot whose name matches
  one is refused (above). The files come from additional disks' overlays, which `SnapshotDelete`
  never removes, from overlays that could not be proven safe to remove, and from snapshots
  deleted by a release before v0.4.0 or by hand. Remove one only after you have checked that no
  domain, snapshot or internal snapshot on any host that mounts the pool references it
  (→ [`docs/libvirt-clones.md`](../libvirt-clones.md#a-snapshot-name-whose-overlay-file-is-still-on-the-host)).
- **libvirt: snapshot and revert overlays are `0600`, and the disk check needs to read them.**
  libvirt creates `<domain>-disk.<snapshot>` and `<domain>-disk.<epoch>` mode `0600`, owned by
  root while the VM is off. A snapshot create, revert or delete, and a VM delete (on a clustered
  Provider also a create and a clone), reads the disk chain of every domain on the host first, and
  the provider's SSH user must be able to read those overlays: grant `VR_DISK_READ` over the pool
  directory (an NFS export must not squash root, and must then be dedicated to the Provider's
  hosts), or repair each new overlay by hand. Without either those operations are refused with
  "permission denied reading a disk image"; since v0.4.1 a `VMSnapshot` create and a revert are
  retried every 10 minutes at most until you fix it, and a VM delete and a snapshot delete are
  retried too
  (→ [`docs/libvirt-clones.md`](../libvirt-clones.md#unreadable-disk-images)).
- **Command-line tools:** the `vrtg snapshot`, `clone`, `diag`, `init`, `conformance` and
  `provider logs` subcommands are still not implemented and exit non-zero naming the
  alternative. The SDK and proto modules have no tag for v0.4.0 and earlier
  (→ [Go modules](../go-modules.md#before-v041)).
- **The `virtrigaud-provider-runtime` chart is deprecated** and is removed in the next minor
  release; see above.

### Carried forward from v0.4.0

- **Metrics are served over plain HTTP by default.** The manager's `--metrics-secure` is still
  off by default (a change is planned for a later release). `/metrics` includes
  administrator-sensitive gauges, so expose it only behind the chart's NetworkPolicy
  (`networkPolicy.enabled`, default off) or turn on `--metrics-secure=true` yourself
  (→ [`docs/clustered-provider-inventory.md`](../clustered-provider-inventory.md#committed-capacity)).
- **Clustered libvirt providers are experimental.** `topology: cluster` is opt-in and may still
  change. Host-to-host migration is not implemented. Migration into a clustered provider is
  experimental: `s3` and `nfs` only, the target VM in the migration's namespace; the disk-space
  check is a filter, not a reservation; the source disk's size is read once; a host that fails
  after placement is waited for, not replaced; two Providers whose hosts mount one pool are not
  serialised with each other; and a waiting target's `spec.importedDisk` can be edited by whoever
  may edit the VM (a swap is detected, not prevented)
  (→ [`docs/clustered-provider-inventory.md`](../clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)).
- **A clustered libvirt host's free-space figure is up to one Host sync (60 seconds) old**, and
  if the filesystem behind an active pool is not mounted, the figure is that of the filesystem
  underneath; a `stat` stuck on a dead hard-mounted NFS export cannot be killed and may
  accumulate until the mount recovers
  (→ [`docs/clustered-provider-inventory.md`](../clustered-provider-inventory.md#what-listhosts-reports-per-host-libvirt)).
- **A migrated guest can boot without network, or with a new identity (R36).** A Linux
  cloud-image guest whose netplan matches its NIC by MAC address (Ubuntu cloud images do) can
  boot without network on a vSphere or Proxmox target: the target NIC has a new MAC and
  cloud-init does not run again. On a libvirt target the network works only because the guest
  gets a new cloud-init identity, so its instance-id, hostname and SSH host keys change. The fix
  is accepted in [ADR-0014](../adr/0014-migrated-guest-network.md) and is not in this release.
  Until then, make the guest's netplan match its NIC by name or driver instead of `macaddress`
  before you migrate, and fix a guest that came up without network from the console.
- **Proxmox networks: part of an attachment is not applied.** `firewall`, `rateLimit` and `mtu`
  of the `proxmox` block, `VMNetworkAttachment.spec.ipAllocation` and `spec.networks[].macAddress`
  have no effect on a Proxmox VM; the CRD allows only `vmbrN` bridges and IPv4 addresses; and
  `Describe` reports no NICs. For a VM without user-data a static IP takes effect only when the
  VM has a PVE cloud-init drive (one its template has); a VM with user-data gets its addresses
  through the seed's network-config
  (→ [`docs/proxmox-networks.md`](../proxmox-networks.md)).
- **Deleting a namespace that holds an in-use `Provider`.** Kubernetes deletes the provider's
  runtime with the namespace, and nothing can stop that. VMs whose deletes miss the pod's
  termination window are held (`ProviderUnavailable`) and the namespace stays `Terminating`
  until an administrator acts (`virtrigaud.io/force-delete: "true"` on the `Provider` or on each
  VM, which leaves the hypervisor VMs unmanaged). Delete the VMs before the namespace
  (→ [ADR-0010](../adr/0010-provider-in-use-finalizer.md),
  [`docs/vm-provider-binding.md`](../vm-provider-binding.md#deleting-a-namespace-that-contains-a-provider)).
- **Proxmox: the legacy name rule can be claimed by whoever can create a VM.** An unstamped VM
  named like its `VirtualMachine` still passes the owner check, and the provider cannot tell
  whether the `VirtualMachine`'s own VM was ever stamped. So once a `VirtualMachine`'s VM is
  removed, a PVE user with `VM.Allocate` on a pool or path where VirtRigaud's VMs live can create
  an unstamped VM with that name at the freed VMID, and the `VirtualMachine` then reads, powers,
  resizes, snapshots and copies it as its own. A VM stamped for anyone else is refused. Grant
  `VM.Allocate` there as carefully as `VM.Config.Options`, and to no one but VirtRigaud's
  Provider tokens and your administrators.
  [ADR-0012](../adr/0012-proxmox-legacy-unstamped-name-rule.md) retires the rule; none of it is
  implemented in this release (→ [`docs/vm-ownership.md`](../vm-ownership.md#limits)).
- **Proxmox: the owner check and the call are two requests.** A VMID freed and reused between
  them is not caught. Do not delete VirtRigaud-managed VMs outside VirtRigaud: delete the
  `VirtualMachine`, or detach it with `orphan-on-delete`
  (→ [`docs/vm-ownership.md`](../vm-ownership.md#limits)).
- **The admission policy needs Kubernetes 1.30+.** On an older cluster nothing is installed, and
  anyone who can create or update `VirtualMachine`s in a namespace can forge the adoption markers
  and the `VMClone` marker. Restrict `create`/`update` on `virtualmachines` to trusted users
  there, or reserve the keys with Kyverno or Gatekeeper
  (→ [`docs/admission-policy.md`](../admission-policy.md)).
- **A kustomize overlay that changes the namespace or name prefix must repeat the
  `replacements` block of `config/default`.** Otherwise the admission policy keeps exempting the
  old manager name and denies the manager's own writes
  (→ [`docs/admission-policy.md`](../admission-policy.md#configuration)).
- **A leader-election overlap during a migration's target `Create` can still fail the
  migration.** It fails closed (`TargetVMNotOwned`); delete the target VM it created and
  re-create the migration
  (→ [`docs/cross-namespace-targets.md`](../cross-namespace-targets.md#the-target-vm-must-be-the-migrations-own)).
- **VMs adopted by v0.3.11 are not re-stamped, and clones made by it are unstamped.** Their
  deletes follow the unstamped rules. On Proxmox, a VM adopted earlier that still carries a
  previous owner's stamp is held `Ready=False/VMNotOwned` until you remove the stale
  `virtrigaud.owner` line from its Notes
  (→ [`docs/vm-ownership.md`](../vm-ownership.md#vms-adopted-before-this-release)).
- **libvirt: the `default` storage pool must be at `/var/lib/libvirt/images`.** Mount shared
  storage at that path, and keep `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` apart from the pool. A pool at
  another path is refused with `InvalidSpec` and never modified
  (→ [`docs/clustered-provider-inventory.md`](../clustered-provider-inventory.md#host-prerequisites)).
- **The disk-size limit does not cover an oversized image.** It checks class and spec sizes and
  the real disk sizes of clone and migration sources, but not an image or template whose own disk
  is larger than the class (→ [chart README](../../charts/virtrigaud/README.md#disk-size-limit)).
- **vSphere: a CPU or memory change of a running VM needs hot-add.** Without it the change waits
  for a power-off and is shown as `Reconfiguring=False/ResizePendingPowerOff`
  (→ [`docs/reconfigure-results.md`](../reconfigure-results.md#vsphere-changes-a-running-vm-cannot-take)).
- **A disk-size change alone does not trigger a `Reconfigure`, and `Reconfigure` resizes few
  disks.** The manager sends one when the effective CPU or memory changes. No provider's
  `Reconfigure` grows the root disk from a `spec.disks` entry or adds a disk; vSphere grows only
  an additional disk that `Create` recorded under the entry's name, and Proxmox resizes none
  (→ [`docs/reconfigure-results.md`](../reconfigure-results.md#when-a-reconfigure-is-sent)).
- **`spec.disks` is reported on libvirt and Proxmox, but only in some places.** The
  `DisksApplied=False` condition and the `Warning` event are always there; the admission warning
  only where the `VirtualMachine` validating webhook is rendered with `manager.maxDiskSize` set.
  The Proxmox provider attaches no `spec.disks` entry at `Create`. vSphere ignores an entry added
  after the VM was created, with no condition and no event
  (→ [`docs/reconfigure-results.md`](../reconfigure-results.md#which-disk-a-size-goes-to)).
- **Snapshot revert on libvirt needs a snapshot taken with the VM powered off, and libvirt 9.9 or
  later.** A snapshot of a VM that is off, or taken without memory, is an external disk-only
  snapshot; older libvirt cannot revert one and the provider reports `RevertFailed`. A disk-only
  snapshot that stores a CD-ROM or read-only disk with media is refused on revert, and so is one
  whose stored definition cannot be read. A snapshot of a powered-off VM ejects the seed CD-ROM for
  its duration; if the provider or the host fails in that window the CD-ROM stays empty until
  VirtRigaud's next snapshot create, revert, start or reboot of that VM inserts the seed again,
  and a VM someone starts by hand (`virsh start`) meanwhile boots without its seed. A medium
  other than the VM's own cloud-init seed (an installer ISO you attached) that an interrupted
  ejection left out is never inserted automatically: you insert it by hand.
  `SnapshotDelete` removes the primary disk's overlays and not those of additional data disks, so
  check the pool after a revert or a snapshot delete
  (→ [`docs/vm-snapshot-revert.md`](../vm-snapshot-revert.md#libvirt),
  [`docs/libvirt-clones.md`](../libvirt-clones.md#what-snapshotdelete-removes)).
- **libvirt: the check that a snapshot delete keeps the VM's other snapshots is not atomic with
  the delete,** and nothing serialises it against a `SnapshotCreate` of the same VM. A memory
  snapshot created while the delete of an earlier disk-only snapshot is running could still be
  orphaned: do not create a snapshot of a VM while you delete another of its snapshots
  (→ [`docs/libvirt-clones.md`](../libvirt-clones.md#deleting-memory-and-disk-only-snapshots-of-the-same-vm)).
- **libvirt: a memory snapshot orphaned by an earlier version is unusable, and the cleanup is
  limited to what can be proven.** The snapshot's metadata is deleted when every file it recorded
  is gone and no current disk image lists it; otherwise the provider asks you to power the VM off
  and delete it again, and the manual cleanup in the doc applies
  (→ [`docs/libvirt-clones.md`](../libvirt-clones.md#a-memory-snapshot-that-was-already-orphaned)).
- **A libvirt guest on a host bridge needs the QEMU guest agent to show `status.ips`.** libvirt
  holds no DHCP lease for a bridged guest; the host's ARP table is a last resort that depends on
  recent traffic
  (→ [`docs/vm-status-ips.md`](../vm-status-ips.md#libvirt)).
- **Held calls need an operator.** A `VMMigration` or `VMSnapshot` held in
  `SnapshotOutcomeUnknown`, `ExportOutcomeUnknown` or `ImportOutcomeUnknown` stays held until you
  check the provider and confirm the attempt with
  `virtrigaud.io/operation-outcome=not-performed@<attempt id>` (or delete the object), except an
  export or import on a re-send-safe backend, which the manager sends again after 35 minutes
  (→ [`docs/provider-call-outcomes.md`](../provider-call-outcomes.md#limitations)).
- **Remote host commands can outlive a cancelled call, and Proxmox `nfs` is not re-send safe.**
  Only `s3` is re-send safe on Proxmox. Single-host libvirt's flatten, `qemu-img convert` and `dd`
  run without `timeout(1)`
  (→ [`docs/provider-call-outcomes.md`](../provider-call-outcomes.md#why-each-provider-answers-the-way-it-does)).
- **A `pvc` migration on a filesystem that reports a link count other than 1 is refused**, as is
  any link or special file in the staging PVC. Use `s3` or `nfs`
  (→ [Staged-file hardening](../../examples/migration/README.md#staged-file-hardening-pvc-backend)).
- **Provider locks are per process: run one replica per Provider.** Do not set
  `spec.runtime.replicas` above 1 on libvirt or Proxmox Providers, and do not share the landing
  datastore or the `default` pool directory with hypervisors the Provider does not manage
  (→ [Upgrade guide](../upgrading.md#behavior-changes-that-are-visible-but-not-breaking)).
- **Landed disks are not deleted in some cases** (`cleanupPolicy: Never`, a disk the provider
  reports in use, a provider image older than v0.4.0, a Provider that is gone, a Proxmox Provider
  without a migration SSH data plane; `LandedDiskLeftBehind` says so), and disks left by
  migrations that failed under v0.3.11 are not removed either.
- **NFS staging cleanup does not remove the file on libvirt and vSphere targets** (a 0-byte file
  remains; Proxmox deletes it), and versioned or object-lock S3 buckets and NFS server snapshots
  keep the data (→ [ADR-0011](../adr/0011-migration-staging-cleanup.md)).
- **`verifyChecksums` does not checksum NFS migrations.** Integrity rests on the target's
  `qemu-img check`; use `s3` for an end-to-end SHA-256 comparison
  (→ [ADR-0006](../adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md)).
- **Leftovers from earlier releases are not cleaned up automatically:** staged migration objects,
  landed migration disks, vSphere import-VM files and `virtrigaud-export-*` files, libvirt
  snapshot overlay files and libvirt cloud-init seeds in `/tmp`. Find and remove them as described
  in [docs/upgrading.md](../upgrading.md#leftovers-from-earlier-releases).

## Validation

**To be filled after the rc1 lab validation.** Until then nothing in this release is claimed as
run on real hypervisors. The changes were tested with unit and envtest tests and with simulators:
the Proxmox seed against a fake PVE API and a stub node shell (the node script also in a real
`sh` and `bash` with stub `qm`, `genisoimage` and `pgrep`), the libvirt snapshot and revert
answers with scripted host commands, and `vcts run` and `virtrigaud-loadgen` against the API
server of an envtest cluster with a simulated provider. The v0.4.0 validation is in the
[v0.4.0 notes](v0.4.0.md#validation).

Please try the release on a non-production cluster first and report what you find (GitHub
issues; include the Provider type, the `kubectl describe` of the object involved and the provider
log, never a Secret).

## Upgrade

See **[Upgrading from v0.4.0 to v0.4.1](../upgrading.md#upgrading-from-v040-to-v041)** in
[docs/upgrading.md](../upgrading.md). The roll order is the one of v0.4.0: **CRDs, then the
manager, then the providers**. The CRDs have no schema change, so applying them is safe at any
point; the `virtrigaud` chart has no template, RBAC or values change, so `helm upgrade` only
re-tags the images and runs the CRD hook with the new `kubectl` image. Roll the libvirt providers
promptly after the manager: a v0.4.1 manager reads a v0.4.0 libvirt provider's answers as they
are (a single-host failed revert as a failure) and retries every "not performed" snapshot create,
so until the provider is rolled a single-host overlay collision or a refusal libvirt will never
accept is retried every 10 minutes instead of failing; only a v0.4.1 provider answers them for
good.

Before the Proxmox provider rolls, if any of its VMs carry `spec.userData` (or a `VMClone`
gives inline user-data): add `ssh_user`, `ssh_privatekey` or `ssh_password`, and `known_hosts`
to the Provider's Secret, check `genisoimage` and `flock` on the node, and in a cluster set
`PVE_NODE_SELECTOR` (and keep the templates on that node). Also:

1. If you installed the `virtrigaud-provider-runtime` chart, create a `Provider` resource for each
   provider it runs, wait until it is `Ready`, then `helm uninstall` the chart's release.
2. If you write providers outside this repository, switch your `go.mod` to the published modules
   at the same release (`github.com/projectbeskar/virtrigaud`, `.../sdk`, `.../proto`).
3. Check your custom `vcts` specifications with `vcts validate` and your `virtrigaud-loadgen`
   configurations with `--dry-run` (an invalid configuration fails at once; a valid one starts a
   simulated run that you can stop with Ctrl-C).
4. A `VMSnapshot` that v0.4.0 held in `SnapshotOutcomeUnknown` because of a leftover overlay file
   stays held: delete it and create one under another name, or confirm the attempt.

## Full changelog

The complete, dated list of changes is in [CHANGELOG.md](../../CHANGELOG.md). The pull requests
since v0.4.0 are #445 to #451. Once the tag exists, the commits are at
<https://github.com/projectbeskar/virtrigaud/compare/v0.4.0...v0.4.1-rc1>.
