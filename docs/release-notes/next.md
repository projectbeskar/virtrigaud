<!--
Release notes for VirtRigaud v0.4.1; title the release "v0.4.1" and, for a release candidate,
"v0.4.1-rc<N>" (mark it as a pre-release). v0.4.1 is a patch release of v0.4.0: fixes, honesty
about what the code does, and tooling. The notes are cumulative since v0.4.0 and are updated
for each candidate; this file now covers rc1, rc2 and rc3. The v0.4.0 notes are kept in v0.4.0.md. See
docs/upgrading.md ("Upgrading from v0.4.0 to v0.4.1") for the upgrade guide these notes link to,
and CHANGELOG.md for the complete, dated changelog.
GitHub's limit for a release body is 125,000 characters; this file is below it (about
98,000 characters), so it is pasted whole. Tighten the older validation tables before it nears
100,000.
-->

This is **v0.4.1**, a patch release of v0.4.0. It changes how a Proxmox VM gets its cloud-init
user-data, closes the libvirt snapshot problems that v0.4.0 listed as known limitations, makes
libvirt secure boot, UEFI, VT-d, nested virtualization and multi-NIC VMs work, makes `vcts` and
`virtrigaud-loadgen` do what their documentation says, publishes the SDK and proto Go modules,
and corrects the published descriptions of the API and the gRPC contract. The second release
candidate (rc2) added the fixes for what the rc1 lab validation found, and the third (rc3) adds
the fixes for what the rc2 lab validation found (see [Validation](#validation)):

- **Proxmox: the whole user-data reaches the guest.** The provider builds a NoCloud seed on the
  PVE node over SSH, so `users`, `write_files`, `packages`, `runcmd` and every other cloud-config
  key apply, as on libvirt and vSphere. **This needs SSH credentials in the Provider's Secret
  and is breaking for Proxmox users whose VMs carry user-data.**
- **Proxmox:** linked clones are no longer advertised (they were never made), a clone gets the
  class's memory, and the user-data's NoCloud seed CD-ROM is kept out of every VM's boot order.
- **libvirt platform (rc2):** a VMClass with `securityProfile.secureBoot`, `vtdEnabled` or
  `nestedVirtualization`, and a VM with more than one network interface, now define and start
  on libvirt 10 (the secure boot, VT-d and multi-NIC domains were refused, nested virtualization
  never started). **`firmware: UEFI` is honoured now** (it was ignored): check your VMClasses
  before you roll the libvirt provider.
- **Controller (rc2):** a `VirtualMachine` create the provider refuses is retried with a backoff
  (30 seconds doubling to 10 minutes) and at once when the VM, its VMClass, VMImage or Provider
  changes, instead of every 30 or every 5 seconds; `Provider.status.reportedCapabilities` is
  read again after a provider roll; a `Provider` without a ready pod is no longer reported
  `ProviderAvailable=True`/`healthy=true`.
- **Controller (rc3):** the provider's definite answer to a `VirtualMachine` `Create`, a
  single-host `VMClone`'s `Clone` or a `VMSnapshot`'s `SnapshotCreate` is no longer lost when the
  object is edited while the call runs. A `VirtualMachine` deleted after a first `Create` the provider
  said it did not perform is no longer held as `CreateOutcomeUnknown`, and a `VMSnapshot` that was
  made is no longer held as `SnapshotOutcomeUnknown`. The `CreateBackoff` message no longer names a
  cause the provider may not have had.
- **Proxmox (rc3):** the loser of a VMID race is answered "not performed" also when PVE refuses its
  request outright while the winner's VM is still being made, if the provider can show that VM is
  another request's (rc2 did it for a clone task that ended on a lock).
- **libvirt snapshots:** a snapshot whose overlay file is already on the host is refused for
  good (it was a known limitation in v0.4.0), a failed revert is answered by what the provider
  can prove, and "not performed" answers are retried instead of counted as unknown outcomes or
  failed. Staging locations (NFS server, export, path; S3 bucket, key, endpoint) are no longer
  logged by any provider or by the manager, and a clustered libvirt export that the NFS server
  or the S3 service refuses fails at once instead of being held for 35 minutes (rc2).
- **Command-line tools:** `vcts run` runs the conformance tests, `virtrigaud-loadgen` performs
  real power, reconfigure, snapshot and clone operations (and, from rc2, keeps at most `vmCount`
  VMs alive and starts nothing after its duration), and `vrtg-provider init` can scaffold a
  Proxmox provider.
- **Security:** a `VMClone`'s unused customization secrets are no longer sent to the provider
  (rotate any real value you set), and libvirt create-from-URL downloads with `curl` with the
  URL off the host's command line.
- **Provider authors:** each release now tags the `sdk` and `proto` Go modules.
- **Helm:** the `virtrigaud-provider-runtime` chart is deprecated and is removed in the next
  minor release.

**Several changes are breaking or change default behavior** (from rc2, among others,
`firmware: UEFI` on libvirt). Read the upgrade guide before
upgrading: **[Upgrading from v0.4.0 to v0.4.1](../upgrading.md#upgrading-from-v040-to-v041)**.
The CRDs have no schema change (only their `description` texts differ) and the gRPC contract
has no field, number or RPC change. If you run v0.4.1-rc1 or
v0.4.1-rc2, read [If you run v0.4.1-rc1](../upgrading.md#if-you-run-v041-rc1) or
[If you run v0.4.1-rc2](../upgrading.md#if-you-run-v041-rc2) too.

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
  missing). A `VirtualMachine` shows `ValidationError` and is rechecked with a backoff (30
  seconds doubling to 10 minutes, or at once when the VM, its VMClass, VMImage or Provider spec
  changes; a changed Secret is not such a change, so the recheck that picks it up can be up to
  10 minutes later); a `VMClone` ends `Failed` (`ProviderError`), so re-create it after you fix
  the Secret and roll the provider. In v0.4.0 such a `Create` made a VM that got only a user name and the
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
- **If you set a secret in a `VMClone`'s `spec.customization` on v0.4.0 or earlier, rotate it.**
  The values of `sysprep.productKey`, `sysprep.adminPassword.value`,
  `sysprep.joinDomain.password.value`, `sysprep.customCommands`, `certificates[].data`,
  `guestCommands[].command`/`.arguments` and `userData.ignition.inline` were sent to the provider (and, without gRPC
  TLS, over the network between the manager and the provider), and they stay in plaintext in the
  object, in etcd backups, API-server audit logs (at `Request` level or above), the `last-applied-configuration` annotation and
  your GitOps sources.
  → [Upgrade guide](../upgrading.md#security-fixes)
- **A libvirt VMClass with `firmware: UEFI` now makes UEFI VMs: check it before you roll the
  libvirt provider (rc2).** The provider ignored the field up to rc1. New VMs of such a VMClass
  boot with OVMF (without Secure Boot) on the q35 machine type, so their image must boot with UEFI
  and their libvirt host needs an OVMF package (`ovmf`, `edk2-ovmf`, ...) that installs firmware
  descriptors in `/usr/share/qemu/firmware/`. On a host that reports no UEFI firmware the create is
  refused (`Ready=False/ValidationError`) where v0.4.0 made a BIOS VM. On a clustered Provider every
  host that can receive such a VM needs the package: the scheduler does not consider firmware, and a
  VM placed on a host without it is refused and is re-checked on that host (it keeps its pending
  host). Existing domains are not
  redefined. To keep BIOS VMs set `firmware: BIOS` or leave it unset. List the affected VMClasses with
  `kubectl get vmclass -A -o jsonpath='{range .items[?(@.spec.firmware=="UEFI")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`.
  → [libvirt firmware](../libvirt-firmware.md),
  [Upgrade guide](../upgrading.md#libvirt-firmware-secure-boot-and-platform-features)
- **libvirt hosts need `curl` for create-from-URL.** A create that carries an image URL instead
  of a prepared image (or names one of the provider's predefined templates) now downloads with `curl` on the libvirt host (as `ImagePrepare` already
  did); `wget` is no longer used. If `curl` is missing, the provider log says so.

### Behaviour changes

- **A libvirt create-from-URL download is classified and time-limited.** An HTTP 4xx answer
  (except 408, 425 and 429), a permanent `curl` failure, and a URL with a scheme other than
  `http`, `https` or `ftp` are reported as `InvalidArgument`:
  the `VirtualMachine` shows `Ready=False/ValidationError` and is rechecked with the backoff
  described below, instead of the generic provider error retried before. The download is cut off after at most
  about 4.5 minutes (the manager gives a `Create` 5 minutes, less a reserve for the conversion);
  in v0.4.0 the call failed after 5 minutes while `wget` kept running on the host.
- **A refused `VirtualMachine` create is retried with a backoff, and at once when its inputs
  change (rc2).** A `Create` the provider refuses as an invalid specification (`Ready=False` and
  `Provisioning=False` with reason `ValidationError`) was sent again every 30 seconds for as long
  as the VM existed, and a libvirt `Create` copies the base image before libvirt refuses the
  definition. It is now sent again after 30 seconds, then 1, 2, 4 and 8 minutes, then every
  10 minutes, and nothing is sent to the provider while it waits. The refusals `NotFound`,
  `NotSupported` (gRPC `Unimplemented`) and a typed `Unauthorized` or `QuotaExceeded` (single-host
  and clustered), which were sent again every 5 seconds, follow the same backoff and keep reason
  `ProviderError` (the typed ones come only from the manager's own code: from a remote provider only
  `NotFound` and `Unimplemented` are paced; none of the in-tree providers answers a `Create` with
  either, so that pacing is covered by unit tests and has not been run against a hypervisor). An edit of the `VirtualMachine`'s spec or of the spec of its `VMClass`,
  `VMImage` or `Provider` sends the create again within seconds and starts the backoff again; a
  `Secret`, a `VMNetworkAttachment` or a fix on the host is picked up at the next re-check, at most
  10 minutes later. The message names the next re-check: `Provider rejected VM create
  (non-retryable; re-checking in 4m0s, at <time>, or at once when the VirtualMachine, its VMClass,
  VMImage or Provider changes): <detail>` (it read `re-checking every 30s`), and `Failed to create
  VM (non-retryable; re-checking in ...): <detail>` for the other refusals: a script or alert that
  matches the old text needs the new one. An `ImagePrepare` whose source the provider refuses is
  paced the same way (a `VMImage` `spec.source` or `Provider` spec edit retries at once). Unchanged:
  a `Conflict` (2 minutes), an untyped `FailedPrecondition` or `PermissionDenied`
  (`CreateRefused`), and every failure that may pass or whose outcome is unknown (5 seconds or their own
  backoff).
  → [Upgrade guide](../upgrading.md#a-refused-vm-create-is-retried-with-a-backoff),
  [Provider call outcomes](../provider-call-outcomes.md)
- **A provider's answer is not lost to an edit made during the call (rc3).** A `Create`, a
  single-host `Clone` or a `SnapshotCreate` can take minutes. If the object is edited meanwhile (a
  label, an annotation, a spec change, another tool's update), the status write that records the
  answer conflicts. In v0.4.0 and up to rc2 the answer was then lost, the call's marker stayed, and a
  `VirtualMachine` deleted right after a `Create` that the provider had answered it did not
  perform was held as `CreateOutcomeUnknown` until an operator confirmed it, with nothing on the
  hypervisor; a `VMSnapshot` that was made could be held as `SnapshotOutcomeUnknown`, and a first
  `Clone` attempt's refusal as `CloneOutcomeUnknown`. The manager now reads the object again after
  the conflict and writes the answer over it, only while the object still waits for that answer;
  anything newer is never overwritten. **An object that an rc2 manager already holds stays held**:
  the lost answer is not in its status (see
  [If you run v0.4.1-rc2](../upgrading.md#if-you-run-v041-rc2)). Still lost: an answer whose
  write fails for another reason, or that a manager restart cuts short; and a `VMMigration`'s
  failed attempt whose write conflicts (see [Known limitations](#known-limitations)).
  → [Provider call outcomes](../provider-call-outcomes.md#an-answer-is-not-lost-to-an-edit-made-during-the-call),
  [Upgrade guide](../upgrading.md#a-providers-answer-is-no-longer-lost-to-an-edit-made-during-the-call)
- **The `CreateBackoff` messages changed (rc3).** The condition message of a `Create` the provider
  answered it did not perform now reads `The provider answered that it did not perform the last
  Create sent for this VM, so that request made nothing, and that it can be retried. It is retried
  with a backoff of up to 5m0s; ...`, and that of any other `CreateBackoff` reads `The provider
  could not complete this VM's Create: the operation failed on the provider's host, or a check it
  needs could not run there. ...`. Both used to name a cause the provider may not have had ("it
  could not verify that no other VM uses its disk, or the operation failed on its host"). The
  `CreateBackoff` `Warning` event now ends with `Provider answer: <the provider's answer>`
  (sanitized), as the `CreateRefused` event does, and a provider-wide `CREATE_NOT_PERFORMED` (for
  example a VMID that could not be allocated from `/cluster/nextid`) shows that answer in the `Provisioning` message instead of "could
  not be reached or did not answer in time". The reason, the backoff and the phase are unchanged.
  An alert or script that matches the old text needs the new one.
  → [Upgrade guide](../upgrading.md#a-providers-answer-is-no-longer-lost-to-an-edit-made-during-the-call)
- **A libvirt definition that libvirt refuses is a validation error and leaves no disk (rc2).** A
  `virsh define` that libvirt refuses because of the definition itself (an XML error, an
  unsupported configuration, no matching OVMF build) was a retryable error: the `VirtualMachine`
  re-copied the disk on each retry, its delete was held as `CreateOutcomeUnknown` and the attempt's
  disk stayed on the pool. It is now `Ready=False/ValidationError` on single-host and clustered
  Providers, the disk the attempt wrote is removed when the domain is known not to be defined, and
  the delete is not held. A disk left by an earlier release's attempt stays on the pool: remove it
  by hand
  (→ [libvirt firmware](../libvirt-firmware.md#when-libvirt-refuses-the-definition)).
- **A `Provider` without a ready pod is `ProviderAvailable=False` and `status.healthy=false`
  (rc2).** Both stayed `True` once set. (`healthy: false` is omitted from the object, so the
  `HEALTHY` column of `kubectl get provider` is empty, not `false`.) The reason is `DeploymentNotReady`. A `VMMigration` waits
  in validation and a `VMAdoption` waits until the provider is available again, as for a `Provider`
  that was never ready; a vSphere or clustered `Provider` (one replica, `Recreate` strategy) is
  briefly unavailable during each of its rollouts, so an alert on these can fire during a roll.
  A reconcile that fails before it reaches the Deployment (a TLS-posture refusal) leaves them as
  last observed.
- **`virtrigaud_manager_reconcile_total{kind="Provider"}` counts a healthy `Provider`'s reconciles
  as `outcome="requeue"` instead of `"success"` (rc2).** Each schedules the next capability read.
  A dashboard that charts the outcome changes shape; an alert on `outcome="error"` does not.
- **A `Provider`'s `reportedCapabilities` is read again after a provider roll (rc2).** The manager
  read it over its cached connection, which a terminating pod of the old ReplicaSet could still
  answer, and nothing read it again, so the gates that read it (image import, clustered resizes,
  snapshot reverts, migration backends) acted on the old image's answer. It is now read once the
  rollout is complete, again 45 seconds later, every 10 minutes while the `Provider` is healthy
  and one minute after a failed read: expect the new answer within about a minute of the rollout.
  → [Upgrade guide](../upgrading.md#provider-status-after-a-provider-roll)
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
  user-data (else a cloud-config that configures nothing) and DHCP on its own MACs. This differs from libvirt
  and vSphere, whose clones re-run the source's user-data. The clone's first boot is a new
  cloud-init instance (new SSH host keys; modules that run once per instance run again with the
  distribution's defaults, for example `disable_root` on most cloud images, and the default user
  is created, with passwordless sudo, a locked password and no keys; what the source's first boot
  wrote to its disk, such as its users' sudo rules, stays). Give the `VMClone` inline user-data
  that sets what the clone must have (users, keys, sudo rules). A clone of a VM **without** a seed and
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
- **`virtrigaud-loadgen` enforces `vmCount` (rc2).** It is the most VMs a run keeps alive at once
  (the VMs it created, the targets of the clones it requested and the VMs whose delete is not yet
  confirmed). The default is 10, so a configuration that omits it is now capped at 10 VMs, and a
  value below 1 is rejected. A create or clone beyond the cap is skipped and recorded as skipped
  (a new last `Skipped` column in `results.csv`, a count in `summary.md`). No operation starts
  after the configured `duration` (an operation or `--observe` wait in flight finishes first), and
  an interrupted run exits non-zero after its cleanup. **Size `vmCount` to your datastore: a run
  can need `vmCount` times the disk size, plus snapshots, which `vmCount` does not limit.**
  → [CLI tools](../cli-tools.md)
- **The SDK's request log has new attributes (rc2).** `gRPC request completed` and `gRPC stream
  completed` (`sdk/provider/middleware`, used by every provider built on the SDK) log `code` and
  the status message the caller receives, and the `error` attribute is that message as a string
  (or nil) instead of an `error` value. A `slog` handler or alert that keyed on the error value
  or on its full text must use the new attributes.

## Highlights

### Security fixes

- **A `VMClone`'s unused customization secrets are no longer sent to the provider.** The manager
  sent all of `spec.customization` in the `Clone` request, including plaintext values no provider
  acts on: `sysprep.productKey`, `sysprep.adminPassword.value`, `sysprep.joinDomain.password.value`,
  `sysprep.customCommands`, `certificates[].data`, `guestCommands[].command`/`.arguments` and
  `userData.ignition.inline`. The request now carries only an allow-list of what providers apply
  (`hostname`, `domain`, `userData.cloudInit.inline`) or report as not applied; nothing changes in
  what a clone gets. When one of these fields is set, the manager records a `Warning` event
  (`CustomizationSecretNotSent`) once the provider accepts the clone, and with
  `webhooks.enabled=true` a new warning-only webhook (`vvmclone-customization.kb.io`, failure
  policy always `Ignore`) returns one admission warning; both name the fields, never their values.
  The values are still **stored in plaintext in the `VMClone`** (the v1beta1 API keeps the fields):
  do not put a real credential in them, and **rotate any value you set on v0.4.0 or earlier**.
  → [Upgrade guide](../upgrading.md#security-fixes)
- **libvirt create-from-URL no longer puts the image URL on the host's command line.** The
  legacy create-from-URL path ran `wget <URL>` on the libvirt host, so a URL's user-info or
  presigned query string was visible in the host's process list and audit log, and in the
  provider log. It now downloads like `ImagePrepare`: `curl -q -K -` with the URL on standard
  input, `http`/`https`/`ftp` only, the size limit and a time limit bounded by the call's
  deadline. Every image URL takes this path (an `ftp://` URL used to fall into the template
  lookup, which logged it and returned it in the error), the URL is redacted in the provider and
  manager logs, and errors never contain it.
  → [Upgrade guide](../upgrading.md#security-fixes)
- **libvirt host commands no longer log staged-disk URLs (R28).** The shared host-command log
  (the `ERROR Command failed` line and its stderr, the `DEBUG` command lines, the SSH stream
  errors, qemu-img stderr in copy errors and the migration `INFO` lines) printed full command
  lines, including a staged disk's `nfs://` URL with server, export, path and uid/gid. Every URL
  except the libvirt connection URI and `pvc://` is now replaced by `<redacted <scheme> URL>`;
  the S3 bucket name and the NFS server, export and file name are replaced in a staged disk's
  causes; and a single-host export or import whose outcome is unknown answers with a constant
  text instead of the raw cause. Logs written by v0.4.0 keep what they contain: treat them as sensitive.
- **Staging locations are redacted from every provider and manager log (R28, rc2).** The rc1 lab
  found the rest of what R28 was meant to cover. A migration's staging location (an NFS server,
  export, path and `uid`/`gid`; an S3 bucket, key and endpoint) no longer reaches: the clustered
  (routed) libvirt export, import and staged-disk cleanup logs and errors; the S3 client's errors
  of every provider; the causes of a `DeleteStagedDisk` failure; the vSphere and Proxmox export
  and import log lines (shown as `<scheme>://<redacted>`) and the errors their `ExportDisk` and
  `ImportDisk` answer (their gRPC code and details are kept); the SDK's request log (it logs the
  message the caller receives); and the manager's `Starting disk export` and `Starting disk import`
  lines, with the provider answer a `VMMigration` failure quotes in its status, conditions and
  Warning event. The cause stays readable (`MNT3ERR_ACCES`, `SignatureDoesNotMatch`). Logs written
  by earlier releases keep what they contain.
  → [Upgrade guide](../upgrading.md#staging-locations-are-no-longer-logged-r28)
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
- **The seed stays out of the boot order of every VM that gets one (rc2).** PVE appends a CD-ROM
  that a config update attaches to a non-empty `order=` list, so rc1 VMs ended with
  `order=scsi0;ide2` or `order=net0;ide2`. The provider now takes the seed's slot out of the order
  in a config update of its own after it attaches the seed, on every path that attaches one (a
  template clone `Create`, a `Create` without a template, an imported-disk `Create` and a `VMClone`),
  also when a re-sent request finds the seed already attached. The update carries the config's
  digest, so a concurrent change is never overwritten; if it still fails, the VM is kept and the
  re-sent request finishes the step. A config without an `order=` list is left alone.
  → [Boot order](../proxmox-cloud-init-storage.md#boot-order)
- **A clone without user-data configures nothing (rc2).** Its seed carried a bare `#cloud-config`
  line, which cloud-init reports as "empty cloud config": `cloud-init status` said `degraded done`
  and exited 2. It is now a cloud-config with an empty mapping (`{}`).
- **The seed's lock file is removed with its VM (rc2).** `/run/virtrigaud/seed-stage/.lock-<vmid>`
  stayed on the node after the VM was deleted. A `Delete` of a VM that has a seed and a create
  rollback that destroyed its VM now remove it over SSH (best effort, at most 10 seconds, never
  failing the call). A VM without a seed opens no SSH session.
- **A clone task that lost a VMID race is answered "not performed" (rc2).** Two `Create`s that
  got the same VMID from the cluster ended one of them with a config lock timeout, which was
  answered as an unknown outcome although nothing had been made. A task that ended because it
  could not lock the clone source's or the new VMID's config, or found the new VMID's config
  already there, is now definite and the other request's VM is not touched.
- **A VMID-race refusal while the winner's VM is still being made is answered "not performed"
  (rc3).** PVE can also refuse the loser's request outright with HTTP 5xx `unable to create VM
  <vmid>: config file already exists`. It answers that before it writes anything or starts a task,
  so the request made nothing, but the provider answered "outcome unknown" whenever the VM at that
  VMID was still being made (always the case while the winner's clone runs), and a `VirtualMachine`
  deleted before its `Create` was sent again was held as `CreateOutcomeUnknown`. It is now "not
  performed" (retried with a new VMID) when the provider can show the VM is another request's: a
  task it recorded for another owner is making that VMID, and every other clone or create task
  of the provider's PVE user on the node is one it recorded for another owner too. It stays unknown
  when it cannot: a temporary config with no known maker (another PVE user's clone, or an earlier
  attempt of the same owner), a task of the provider's PVE user that it did not record for another
  owner (an earlier attempt whose answer was lost, a task started before a provider restart,
  another Provider that shares the API token, an expired record, a concurrent request of the same
  Provider whose answer has not arrived), or a task list or config it cannot read. Every create path and the `Clone` RPC use the same check. The provider also asks
  PVE for up to 100000 active tasks (PVE lists 50 by default), so a busy node no longer hides one
  of its own tasks from the restart guard.
  → [`Create` and `Clone` error classes](../vm-ownership.md#create-and-clone-error-classes)
- **The provider's startup log lists the capabilities it advertises (rc2)**, without
  `linked-clones`.

### libvirt platform: secure boot, UEFI, VT-d, nested virtualization, network interfaces (rc2)

- **Secure boot works.** A VMClass with `securityProfile.secureBoot: true` produced a q35 domain
  with the i440fx device layout, which libvirt 10 refuses. UEFI and VT-d domains now use q35 with
  its own device layout (libvirt assigns the PCI addresses, the cloud-init CD-ROM is on SATA, USB is
  `qemu-xhci`, video is VGA). The firmware is no longer a hard-coded path: the domain asks libvirt for
  `<os firmware='efi'>` and libvirt selects an OVMF build from the host's firmware descriptors, one
  with Secure Boot keys enrolled for `secureBoot`, so Secure Boot is enforced from the first boot.
  On a libvirt that selects the firmware at define (such as 10.0), a new VM never starts from a
  variable store that an earlier domain of the same name left, and a store that is a symbolic link
  or another domain's refuses the create (`Conflict`).
- **`firmware: UEFI` is honoured** (see [Actions required](#actions-required)). `BIOS`, `EFI` and an
  unset value keep BIOS, as on vSphere.
- **`securityProfile.vtdEnabled` works** (an emulated Intel IOMMU with interrupt remapping on q35;
  libvirt refused the old XML) and **`performanceProfile.nestedVirtualization` works** (it uses the
  host CPU, `host-passthrough`; it required `vmx` and `svm` together, so it never started).
- **VMs with more than one network interface can be created.** On i440fx the second interface
  took the sound card's PCI slot and libvirt refused the domain.
- **A UEFI create is refused before the image is copied** when `virsh domcapabilities` lists no
  `efi` firmware (or, for secure boot, no Secure Boot capable loader). A host that cannot answer is
  left to the define.
- **Delete removes a UEFI VM's own variable store** (`virsh undefine --nvram`; libvirt refuses to
  undefine a UEFI domain otherwise). A store at another path is kept and logged.
  → [`docs/libvirt-firmware.md`](../libvirt-firmware.md),
  [Upgrade guide](../upgrading.md#libvirt-firmware-secure-boot-and-platform-features)

### Controller and Provider status (rc2)

- **A refused create is retried on a backoff** (see [Behaviour changes](#behaviour-changes)). The
  manager now watches the spec of a `Provider`, `VMClass` and `VMImage` for a VM whose create was
  refused, so an edit retries within seconds. The wait is kept in the manager's memory and recovered
  after a restart from the condition message, which names the next re-check.
- **No panic for a Provider without `status.runtime`.** Reconciling a `VirtualMachine` whose
  `Provider` the Provider controller had not reconciled yet dereferenced a nil pointer in a log
  line and panicked (recovered, retried with rate limiting). The VM now gets the usual "cannot reach
  this VirtualMachine's Provider" condition.
- **`Provider.status.reportedCapabilities`, `status.healthy` and `ProviderAvailable` follow the
  provider's runtime** (see [Behaviour changes](#behaviour-changes)). The read goes over a connection
  opened for it (bounded to 10 seconds), once the Deployment's rollout is complete.

### Controller: a provider's answer survives an edit (rc3)

- **The answer to a `Create`, a single-host `Clone` or a `SnapshotCreate` is written again after
  a conflict** (see [Behaviour changes](#behaviour-changes)). Only the status the call's own
  controller wrote is re-applied, and only while the object still carries the call's marker (no
  `status.id` or `status.snapshotID`, the same `CreateRequested`, `CloneRequested` or
  `SnapshotRequested` marker, and for a clustered `Create` the same pending host). The conditions
  keep the `observedGeneration` of the spec the call was built from, so a spec change made during
  the call is still seen as not yet acted on. A delete reconciled from a cache that still shows a
  marker the stored VM no longer carries re-reads the VM before it holds, so it is neither held
  nor announced as `CreateOutcomeUnknown`.
- **A `VMClone`'s or a `VMSnapshot`'s whole status is written again, a `VirtualMachine`'s
  conditions only.** A clustered `VMClone`'s answer recorded on its target, a clustered `Create`
  whose host was excluded after a name conflict, and the clustered restore and own-domain holds
  (`RestorePending`, `OwnDomainOnAnotherHost`, an imported disk pinned to its host) are not written
  again: the pending host stays, so the next reconcile sends the call to the same host and gets the
  same answer.
  → [Provider call outcomes](../provider-call-outcomes.md#an-answer-is-not-lost-to-an-edit-made-during-the-call)

### Clustered libvirt export answers (rc2)

- **A clustered `ExportDisk` failure the provider can prove is definite.** An NFS export the server
  refused (`MNT3ERR_ACCES`), an S3 upload the service refused or failed before the flattened disk
  was read to its end (`SignatureDoesNotMatch`, `AccessDenied`, a 5xx) and an unusable S3
  configuration were all answered "unknown", so the `VMMigration` was held with
  `ExportOutcomeUnknown` and re-sent after 35 minutes. They are now answered as a single-host
  Provider answers them: the migration fails at once and its retry policy applies. A flatten or
  convert whose connection dropped or was killed, a host that stopped answering and an upload whose
  completing call may have taken effect stay unknown.
  → [Provider call outcomes](../provider-call-outcomes.md#clustered-libvirt-definite-export-failures)

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
  From rc2, `vmCount` is enforced (the most VMs alive at once; creates and clones beyond it are
  skipped and counted in a `Skipped` column), no operation starts after the duration, and an
  interrupted run exits non-zero after its cleanup.
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
  capabilities: the manager resolves it to `relay`, and no provider implements `direct`. The
  `customize_json` comment now says it is an allow-listed subset of `spec.customization`:
  third-party providers no longer receive the secret-capable customization fields.

### Helm chart

- **The `virtrigaud-provider-runtime` chart is deprecated** (`deprecated: true`, version 0.1.2,
  a warning on install) and is **removed in the next minor release**. It was never published to
  the Helm repository or released with the `virtrigaud` chart. It runs a provider `Deployment`
  outside a `Provider` resource, which the manager never connects to, and bypasses the TLS and
  mTLS settings, the credential Secret mount, the dedicated ServiceAccount and the hardened
  security context the manager sets up for a provider. Create a `Provider` resource instead; the
  chart's README maps its values to `Provider` fields.
  → [Upgrade guide](../upgrading.md#deprecated-the-virtrigaud-provider-runtime-chart)
- **The `virtrigaud` chart gains one webhook and has no RBAC or values change.** With
  `webhooks.enabled=true` it renders the warning-only `vvmclone-customization.kb.io` entry (failure
  policy always `Ignore`, timeout 5s); `values.yaml` changes only in comments. `helm upgrade`
  re-tags the images and the CRD hook uses the new `kubectl` image.

## Known limitations

These are known in this release. Each links to the page that documents it in full, where one
exists. The limitations of v0.4.0 that this release fixes (V12, the Proxmox user-data, the
"not performed" revert and snapshot answers, `vcts run` and `virtrigaud-loadgen`, the SDK and
proto module tags) are removed; the ones that still apply are carried forward and checked
against the code.

### New or changed in v0.4.1

- **A libvirt `ImagePrepare` download is cut off after about 4.5 minutes, whatever
  `spec.prepare.timeout` says (the 30-minute default included).** The manager gives the `ImagePrepare` call 5 minutes, and the download is now cut off
  within that call (before, `curl` kept running on the host after the call had failed). Prepare
  large images from a source close to the libvirt host.
- **The `VMClone` customization secret fields are only warned about.** The admission warning is
  advisory and appears only with webhooks enabled; to forbid these fields, add your own admission
  policy.
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
- **A refused create is re-sent on a backoff, not never.** The wait is in the manager's memory
  (a restart recovers it from the condition message). A change that is not a spec change of the
  `VirtualMachine`, its `VMClass`, `VMImage` or `Provider` (a `Secret` the provider reads, a
  `VMNetworkAttachment`, a fix on the host) is picked up at the next re-check, at most 10 minutes
  later. A libvirt refusal that comes from the define, other than a missing UEFI firmware, still
  copies the image on each re-check and removes the copy: at most every 10 minutes after the first
  few. That includes a secure boot VM on a host that has a Secure Boot build of OVMF but not the
  one with keys enrolled: the check before the copy looks for the former only
  (→ [`docs/libvirt-firmware.md`](../libvirt-firmware.md#how-the-firmware-is-chosen)). A remote provider's `Unauthenticated` and `ResourceExhausted` answers are not paced and
  are still sent again every 5 seconds
  (→ [Upgrade guide](../upgrading.md#a-refused-vm-create-is-retried-with-a-backoff)).
- **libvirt UEFI: no memory snapshot, and no revert of UEFI variables.** libvirt 10.0 refuses an
  internal snapshot of a VM with pflash firmware, so a `VMSnapshot` with `includeMemory: true` of a
  running UEFI VM is not taken (disk-only snapshots work, and do not restore UEFI variables such as
  the boot order). On a clustered Provider whose hosts have different OVMF packages, VMs of one
  VMClass can get different firmware builds. A change to a VMClass's `firmware` or
  `securityProfile` does not change existing VMs. Which Secure Boot keys a VM trusts depends on the
  host's OVMF package
  (→ [`docs/libvirt-firmware.md`](../libvirt-firmware.md#known-limitations),
  [trust model](../libvirt-firmware.md#secure-boot-trust-model)).
- **libvirt: files left by an earlier release's refused create stay.** A disk
  (`<domain>-disk.qcow2`) that a create of v0.4.0 or rc1 left on the pool is removed only if a
  later create of the same VM removes it; remove one yourself after you have checked that no domain
  uses it. The `CreateOutcomeUnknown` hold of such a VM is released after 10 minutes, or at once by
  the annotation its message names
  (→ [`docs/libvirt-firmware.md`](../libvirt-firmware.md#when-libvirt-refuses-the-definition)).
- **libvirt: a create from a `source.libvirt.path` image goes through the disk check too.** The rc1
  validation saw a single-host create from such an image refused, and retried, with "could not
  verify the image on the libvirt host (transient host error; details are in the provider log)"
  where a snapshot gets the "permission denied reading a disk image" text; the provider log names
  the image. Fix it as for the snapshots
  (→ [`docs/libvirt-clones.md`](../libvirt-clones.md#unreadable-disk-images)).
- **libvirt: the pool refresh of a VM delete covers the host that ran it.** On a clustered Provider
  whose hosts share the `default` pool directory, another host's `virsh vol-list default` keeps
  listing the deleted files until that host's pool is refreshed (the files are gone; run
  `virsh pool-refresh default` on that host).
- **Proxmox: a `VMClone` and its target deleted while the clone runs leave the cloned VM on PVE.**
  The manager keeps it on purpose (it logs that the cloned VM is not bound), and neither `vcts` nor
  `virtrigaud-loadgen` can see such a VM, so their cleanup reports success. Do not delete a
  `VMClone` or its target while it is still cloning
  (→ [`docs/proxmox-cloud-init-storage.md`](../proxmox-cloud-init-storage.md#lifecycle)).
- **`virtrigaud-loadgen` can fill a datastore.** `create` and `clone` make real VMs: a run can
  need `vmCount` times the disk size, plus snapshots, which `vmCount` does not limit. `vmCount` is the cap, not a reservation, and deleting a
  `VMClone` or its target while the provider is cloning can leave a hypervisor VM the tool cannot
  see (→ [CLI tools](../cli-tools.md#virtrigaud-loadgen)).
- **A resize of a single-host `VMClone` target is ignored (also in v0.4.0).** A `VirtualMachine`
  bound by a single-host `VMClone` has no `status.currentResources` (only a clustered clone records
  the size it was made at), and the manager sends a `Reconfigure` only when the effective size
  differs from a recorded one. An edit of the target's `spec.resources`, or of its `VMClass`, sends
  nothing: no `Reconfigure` call, no `Reconfiguring` condition, and the hypervisor VM keeps the
  size it was cloned at (the rc2 lab reproduced it on Proxmox; the controller code is the same for
  every single-host Provider, and by the code a single-host `VMAdoption` target has no recorded size
  either; that case was not run). There is no way to resize such a VM through the `VirtualMachine`:
  create the `VMClone` with the target `VMClass` already at the size you want, or resize the VM on
  the hypervisor. The source VM and every VM made by `Create` resize as documented
  (→ [`docs/reconfigure-results.md`](../reconfigure-results.md#when-a-reconfigure-is-sent)).
- **A `VMMigration`'s failed attempt whose status write conflicts is still held.** The fix for a
  lost answer covers a `VirtualMachine`'s `Create`, a single-host `VMClone`'s `Clone` and a
  `VMSnapshot`'s `SnapshotCreate`. A `VMMigration`'s failure write that conflicts (the migration was
  edited while the call ran), a write that fails for another reason, and a manager restart before
  the write still leave the marker: the migration is held (`SnapshotOutcomeUnknown`,
  `ExportOutcomeUnknown`, `ImportOutcomeUnknown`) although the provider answered, until you confirm
  the attempt
  (→ [`docs/provider-call-outcomes.md`](../provider-call-outcomes.md#limitations)).
- **Proxmox: a VMID race can still end "outcome unknown".** The provider answers "not performed" only
  when it can show the other VM is another request's (see Highlights). For example while another
  request of the same Provider still waits for PVE's answer, while the other VM is still being made
  from the PVE GUI or by another PVE user, or when two Providers share an API token (each treats the
  other's tasks as possibly its own), the loser is held by the unknown outcome and a
  `VirtualMachine` deleted at that point needs the
  `virtrigaud.io/create-outcome` annotation its message names
  (→ [`docs/vm-ownership.md`](../vm-ownership.md#create-and-clone-error-classes)).
- **Proxmox: a `VMSnapshot` create that PVE fails is not retried.** The provider answers a failed
  snapshot task, for example a configuration lock timeout because another snapshot of the same VM
  was running, as a plain failure, and the `VMSnapshot` ends `Failed` (the rc2 lab saw it). libvirt
  answers a busy domain "not performed", which is retried; Proxmox does not. Create a new
  `VMSnapshot`.
- **A provider image re-pulled under the same tag is noticed within 10 minutes.** The capability
  read follows a change of the provider Deployment; a re-pull that changes nothing in it is caught
  by the 10-minute read. While a rollout is stuck, the capabilities of the old pods are kept.

- **Minor, known and not fixed.** libvirt hosts keep the empty per-VM lock files
  `/tmp/virtrigaud-locks/{export,import,clone}-<domain>.lock` after the VM is gone. The redaction of
  staging locations also replaces every parent directory of an NFS export path, so an unrelated path
  in a clustered provider's error can read `<staged object>/libvirt/images/...`. Two libvirt
  provider log lines mislead: the single-host "exists but no domain uses it; overwriting it" (the
  disk is then removed) and the clustered "VM has a create in flight; sending an owner-checked
  delete to its pending host" (by design for a VM without an id). Every create logs
  `ERROR Command failed: sudo restorecon ...` on a host without SELinux, followed by a warning.

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

v0.4.1-rc1 and v0.4.1-rc2 were each validated on real hypervisors (in rc2 vSphere only as a provider roll); v0.4.1-rc3 carries the fixes
for what the rc2 run found and **has not been validated on real hypervisors yet** (see the end of
this section). The changes were also tested with unit and envtest tests and with simulators: the Proxmox seed
against a fake PVE API that models qemu-server's boot order and a stub node shell (the node script
also in a real `sh` and `bash` with stub `qm`, `genisoimage` and `pgrep`), the libvirt snapshot,
revert, platform and refusal answers with scripted host commands (every libvirt platform variant
was also defined and started paused, without a guest boot, on a libvirt 10.0 / QEMU 8.2 host with
OVMF), and `vcts run` and
`virtrigaud-loadgen` against the API server of an envtest cluster with a simulated provider. The
v0.4.0 validation is in the [v0.4.0 notes](v0.4.0.md#validation).

### rc1 lab validation

The environment was a single Proxmox VE 9 node (no cluster), a clustered libvirt Provider with two
hosts and a single-host libvirt Provider (libvirt 10), with the manager and the providers on
v0.4.1-rc1 and the release binaries of `vcts` and `virtrigaud-loadgen`.

| Area | What was exercised | Result |
|---|---|---|
| Proxmox user-data | A template VM with users, `write_files`, `runcmd`, packages and a static IP; delete; a VM without user-data; a Provider without SSH credentials | Passed: every key applied, the seed equal to the spec byte for byte, no user-data in any log, no volume left by a delete, no SSH login for a VM without user-data, the credential-less Provider refused before anything was created. |
| Proxmox clones | A clone with and without inline user-data; `LinkedClone` | Passed (own instance-id, host keys and hostname, the class's memory, the source untouched; `LinkedCloneUnsupported` before any provider call). Findings below. |
| Proxmox boot order | Four template shapes (a cloud-init drive, a drive on another bus, no disk, a data disk before the boot disk) | The boot disk stayed first and the guests booted, but the seed's slot was in every VM's order (fixed in rc2). |
| Proxmox seed blocks `qm migrate` | | Not run: the lab was a single node. |
| libvirt snapshots (single-host and clustered) | An existing overlay file (V12), also one the SSH user could not see; a revert and a create made "not performed" by an unreadable disk image; a memory snapshot of a UEFI VM | Passed: one request, `Failed` with the documented message, the file untouched; the revert and the create were retried at 1, 2 and 4 minutes, never `Failed`, and succeeded once the permission was fixed; the memory snapshot was refused for good. The UEFI domain was built by hand (fixed in rc2). |
| libvirt revert with a stopped libvirtd (clustered) | The VM's own host stopped, then another host | Partial: the revert waited and then succeeded; the answer for a host lost mid-revert was never produced this way. |
| libvirt create-from-URL | A clustered Provider with user-info and a query string in the URL | Passed: only `curl` ran, and no URL, query or user-info was in the host's process list or any log. Not run on the single-host Provider (it imports images, so the URL went through `ImagePrepare`), and `auditd` was not installed. |
| libvirt pool refresh (R37); `VMClone` secrets | A single-host VM delete; the admission warning, the event and a search of every log and status | Passed (one warning and one event naming the fields, no value anywhere). |
| R28 log redaction | Failing NFS and S3 migrations from a single-host and a clustered source | Single-host passed. The clustered Provider, the manager's start lines and the staged-disk cleanup showed staging locations (fixed in rc2). |
| `vcts`; `virtrigaud-loadgen` | `vcts run` of the five built-in tests on three Providers, an interrupt; a dry run, every operation, `--observe`, `--cleanup` and an interrupt | Passed: 5 of 5 on each, nothing left behind, the interrupted run exited 1 and cleaned up; `virtrigaud-loadgen` passed, with the findings below. |

Bugs found in rc1 (in the lab run, or in review where marked), and the pull request that fixed each in rc2:

| Finding | Fixed in |
|---|---|
| The seed's slot was in the boot order of every Proxmox VM that has one; the startup log listed linked clones among the capabilities; a clone without user-data made `cloud-init status` report degraded (and the docs did not say what a clone keeps of its source's users and sudo rules, or that a `VMClone` and its target deleted mid-clone leave the VM on PVE); two creates that got the same VMID were answered "outcome unknown" for the loser; the seed's lock file stayed after its VM was deleted. | #455 |
| `Provider.status.reportedCapabilities` kept the previous image's answer after a provider roll (a rolled Proxmox Provider still reported linked clones); `status.healthy` stayed `true` without a ready pod (found in review). | #456 |
| A clustered libvirt export logged an S3 bucket and key; a failed staged-disk cleanup logged the NFS server, export and `uid`/`gid` (through the SDK's request log); the manager's `Starting disk export` line named the staging destination; a clustered export that NFS or S3 refused was held as `ExportOutcomeUnknown` for 35 minutes. | #457 |
| A libvirt VMClass with `securityProfile.secureBoot: true` could not define its domain (the retry re-copied the disk every 30 seconds, the delete was held and the disk stayed); `firmware: UEFI` was ignored, and VT-d, nested virtualization and multi-NIC VMs were broken (found in review). | #458 |
| Refused creates were re-sent every 30 seconds (every 5 seconds for some answers); a Provider without `status.runtime` made every VM reconcile panic (found in review). | #459 |
| `virtrigaud-loadgen` did not cap the VMs it kept alive, started operations after its duration and exited 0 after an interrupt. | #454 |
| A delete refreshed the pool only on the host that ran it; a single-host create from a libvirt path image that the disk check cannot read is retried with a "transient host error" text. | Not fixed: documented as limitations. |

### rc2 lab validation

The environment was a single-node Proxmox VE 9 (LVM-thin storage, no cluster), a clustered libvirt
Provider with two hosts and a single-host libvirt Provider (libvirt 10.0, QEMU 8.2, Ubuntu 24.04
OVMF 2024.02), and a vSphere Provider (rolled, not used for VMs), with the manager and the
providers on v0.4.1-rc2 and the release binaries of `vcts` and `virtrigaud-loadgen`. The guests
were Ubuntu 24.04 cloud images (CirrOS for the migrations).

**Proxmox, `vcts` and `virtrigaud-loadgen`**

| Area | What was exercised | Result |
|---|---|---|
| Boot order | A template `Create` with user-data; a `VMClone` of that VM after it had fully booted; three concurrent template `Create`s; a `Create` without a template (through a direct gRPC client: a `VirtualMachine` cannot make one) | Passed. The final order was `order=scsi0` (`order=net0` for the VM without a disk), the seed's slot was in none of them, the provider logged taking it out, and every guest with a disk booted from it and finished cloud-init `done` with exit 0 (the seed was its data source where checked). |
| Boot order, imported-disk `Create` | | Not run: it needs a real `VMMigration` that lands a disk for the VM. |
| Clone without user-data | A clone of a fully booted seeded source | Passed: cloud-init `done`, exit 0, no recoverable errors, its own instance-id, hostname and SSH host keys, the source untouched. The docs' statement about the clone's sudo rules was wrong (finding D1, corrected). |
| Seed lock file | A delete of each kind of seeded VM | Passed: `.lock-<vmid>` was gone every time, and the three files rc1 had left went when new VMs reused their VMIDs. |
| Startup capabilities | The provider's start line and `status.reportedCapabilities` | Passed: no linked clones in either. |
| rc1 results, re-checked | User-data in the guest (two users, each with only its own key and one with passwordless sudo, files, a package, `runcmd`, a static IP); the seed's content against the spec; a search of the provider and manager logs for the user-data; a VM without user-data | Passed. The delete left no volume, and the VM without user-data opened no SSH session on the node. |
| VMID race | Three concurrent seeded `Create`s | Passed: the two losers were answered "not performed" and re-sent (one lost twice), all three became Ready, and the winner's VM was untouched. |
| VMID race under load | `virtrigaud-loadgen` with concurrency 3 and `vmCount: 3` | Failed in two forms (B1 and B2 below): a deleted `VirtualMachine` was held as `CreateOutcomeUnknown` with nothing on PVE. |
| `virtrigaud-loadgen` | A run with every operation and `--cleanup`; a run with `--observe`; an interrupt with a clone and snapshots in flight | The cap held: never more than 3 VMs alive (counting terminating VMs and clone targets), the surplus recorded as skipped, no operation started after the duration, and the interrupted run exited non-zero after its cleanup. The cleanup of two runs was blocked by B1 and B2 until the held VM was released by hand. Other VMs, clones and snapshots in the namespace were unchanged. |
| `vcts` | `vcts run` of the five built-in tests with the release binary | Passed: 5 of 5, nothing left behind. |
| Seed and `qm migrate` | | Not run: the lab was a single node. |

**libvirt, single-host and clustered**

| Area | What was exercised | Result |
|---|---|---|
| Secure boot | A VMClass with `securityProfile.secureBoot: true` | Passed: a q35 domain whose firmware libvirt selected from the host's descriptors (the build with Microsoft keys enrolled), SMM on, the loader `secure='yes'`; the guest reported `SecureBoot enabled`, with kernel lockdown. |
| `firmware: UEFI`, and BIOS | A `firmware: UEFI` VMClass; a `firmware: BIOS` VMClass | Passed: UEFI booted with Secure Boot off; BIOS stayed on i440fx with SeaBIOS. |
| VT-d, nested virtualization, two NICs | One VM of each | Passed: the guest saw the emulated IOMMU (`DMAR`, interrupt remapping); the nested VM used `host-passthrough`; two-NIC VMs started on i440fx and on q35 with no PCI collision. A default domain on these hosts also shows `vmx` in the guest, so `vmx` alone does not prove the nested setting. |
| Delete | A UEFI, a secure boot and a BIOS VM | Passed: `undefine --nvram` for the UEFI ones; no variable store, disk or pool volume left on either host. |
| Refused definition | A VMClass with `securityProfile.tpmEnabled: true` on hosts without `swtpm`; a secure boot VM while the enrolled-keys firmware descriptor was hidden on one host | Passed: `Ready=False`, `Provisioning=False/ValidationError` with libvirt's reason; no disk between attempts (sampled every second); the delete finished within a second and was never held as `CreateOutcomeUnknown`. With the descriptor hidden the check before the copy still passed, because the Secure Boot build without keys remained, so the image was copied and then removed (documented). |
| Backoff of a refused create | The refused single-host VM for about 40 minutes | Passed: re-sent at 30 seconds, 1, 2, 4 and 8 minutes, then every 10 minutes (8 attempts, 8 "Creating VM" lines in the provider log); the message named each next re-check; nothing was sent in between, also across a manager restart. |
| What retries at once | Edits of the VMClass, the Provider and the VM; status-only, label and annotation changes | Passed: a spec edit re-sent within a second and restarted the backoff at 30 seconds; a Provider status update, a label and an annotation did not. |
| `VMImage` with a wrong checksum | A single-host Provider | Passed: prepared again on the backoff (30 seconds, 1, 2, 4 minutes), not every 30 seconds; prepared at once after the checksum was corrected, and the VM was created. |
| A `Create` answered `NotFound` is paced | | Not run: no in-tree provider answers a `Create` with a typed `NotFound` (libvirt maps only `Conflict`, `InvalidSpec` and host-unavailable answers; a missing Proxmox template is `InvalidSpec`; a missing vSphere template is untyped), so the check could not be made. Pacing of `NotFound` and `NotSupported` answers is in the manager for providers that send them, covered by unit tests, not lab-validated. |

**Redaction, clustered exports, Provider status, manager restart**

| Area | What was exercised | Result |
|---|---|---|
| Clustered export answers | A clustered source exporting to an NFS export the host may not mount, and to S3 with a wrong secret key; a single-host control | Passed: both failed about a second after the export started, with no `ExportOutcomeUnknown`, were re-sent once as the configured retry policy (one retry) says, and then failed for good; the single-host control gave the same answer and timing. |
| Redaction | Failing and successful NFS and S3 migrations; a failing staged-disk cleanup on a single-host and a clustered target | Passed. A search of the manager log, every provider log, every event and the status and conditions of every migration for the staging server, export path, `uid`/`gid`, bucket, key prefix and endpoint found nothing (the locations remain in your own spec and its `last-applied-configuration`). The manager's `Starting disk export` and `Starting disk import` lines show `nfs://<redacted>` or `s3://<redacted>`, and the provider logs `<redacted nfs URL>`. |
| Successful migrations | One NFS and one S3 migration from the clustered source | Passed: both completed in about two minutes, the staging cleanup ran, and the target VMs booted and answered ping. |
| Provider status after a roll | A vSphere Provider (`Recreate`) rolled to another image and back | Passed: `ProviderAvailable=False/DeploymentNotReady` during each roll; `GetCapabilities` ran when the new pod became ready and 45 seconds later; no reconcile loop. |
| `healthy` and `ProviderAvailable` | The provider Deployment scaled to 0 | Passed: the manager restored the replica at once; the not-ready window lasted about 13 seconds. |
| No panic without `status.runtime` | A `Provider` and a `VirtualMachine` created in one apply | Passed: the VM showed "cannot reach this VirtualMachine's Provider" until the runtime was Running, then was created; no panic in either manager log. |
| Manager restart | Refused VMs in their backoff; 20 Ready VMs | Passed: the refused single-host VMs were re-sent at the time their message named; every Provider's capabilities were read again within a second; all Ready VMs stayed Ready. |

Bugs and other findings, and what was done about each:

| Finding | Result |
|---|---|
| **B1.** A VMID race refused outright by PVE (HTTP 5xx "config file already exists") while the winner's clone was still running was answered "outcome unknown"; a `VirtualMachine` deleted before its `Create` was re-sent was held as `CreateOutcomeUnknown` with nothing on PVE. | Fixed in rc3 (#463). |
| **B2.** A `Create` answered "not performed" was lost when the status write conflicted with an edit; the deleted VM was held as `CreateOutcomeUnknown`. The same loss applied to a single-host `Clone` and a `SnapshotCreate` (found in review of the fix). | Fixed in rc3 (#464). |
| **O1.** The `CreateBackoff` message and event for a VMID-race loser named a disk check that the Proxmox provider had not made (the phase shows `Failed` during the backoff, as documented). | Fixed in rc3 (#464). |
| **D1.** The Proxmox docs said a clone's source users have no sudo rule. | Fixed in rc3 (docs): they keep their rules, and the default user is created with passwordless sudo. |
| **B3.** A resize of a single-host `VMClone` target is ignored (also in v0.4.0). | Not fixed: [known limitation](#known-limitations). |
| **O2.** A Proxmox `VMSnapshot` whose task hit a configuration lock timeout ended `Failed` without a retry. | Not fixed: [known limitation](#known-limitations). |
| **O3.** The boot order of a `Create` without a template cannot be reached through the manager. | Documented ([boot order](../proxmox-cloud-init-storage.md#boot-order)). |
| The secure boot check before the copy does not look for enrolled keys. | Documented ([libvirt firmware](../libvirt-firmware.md#how-the-firmware-is-chosen)). |
| The notes listed a `Create` answered `NotFound` as a lab check that no in-tree provider can produce. | Corrected in rc3 (above and in the upgrade guide). |
| `status.healthy` is omitted, not `false`, while a Provider is not ready (empty `HEALTHY` column). | Documented (rc3). |
| An NFS cleanup leaves a 0-byte staged file on a libvirt target; a clustered host keeps listing deleted volumes until its pool is refreshed. | Not fixed: already documented ([Known limitations](#known-limitations)). |
| Lock files kept on libvirt hosts, the redaction of parent directories of an NFS export path, three misleading log lines. | Not fixed: [listed](#known-limitations). |
| A clustered VM refused by the libvirt define was re-sent before its named time after the manager restart. | Not a defect: the lab's own edits of the Provider and of the VM spec, each of which re-sends at once, explain every early send. |

### rc3 lab validation

**To be filled after the rc3 lab validation.** Until then nothing in rc3 is claimed as run on real
hypervisors. The run will re-check the rc2 failures and the rc3 changes:

- Proxmox: B1 and B2 again under `virtrigaud-loadgen` (concurrency 3, `vmCount: 3`, a high create
  weight and a delete weight of 1 on a single node), including an interrupted run with `--cleanup`: no
  `VirtualMachine` is held as `CreateOutcomeUnknown` except in one of the forms listed under
  [Known limitations](#known-limitations) (each hold is traced to its form), the loser of a race
  with a known winner is answered "not performed" instead of "the PVE clone template request
  failed; a VM may have been created", and every loadgen cleanup completes without an annotation
  except for such a hold.
- A VMID race with several `VirtualMachine`s created at once, the losers deleted while they wait
  in `CreateBackoff`: the deletes finish at once and the winner's VM is untouched. The new
  `CreateBackoff` texts and the event's `Provider answer:`.
- A `VMSnapshot` and a single-host `VMClone` edited (a label, then a spec change) while the provider
  call runs: the `VMSnapshot` is Ready with its snapshot id, the `VMClone` finishes, and neither is
  held.
- A regression smoke on each provider (Proxmox, single-host and clustered libvirt, vSphere): create,
  power, reconfigure, snapshot and revert, clone, delete, and `vcts run`.

Not planned for rc3: the Proxmox imported-disk `Create` after a real migration, the effect of the
seed on `qm migrate` (it needs a multi-node Proxmox cluster), a single-host libvirt
create-from-URL, and the pacing of a `Create` answered `NotFound` or `NotSupported` (no in-tree
provider sends one). They were not run in rc1 or rc2 either.

Please try the release on a non-production cluster first and report what you find (GitHub
issues; include the Provider type, the `kubectl describe` of the object involved and the provider
log, never a Secret).

## Upgrade

See **[Upgrading from v0.4.0 to v0.4.1](../upgrading.md#upgrading-from-v040-to-v041)** in
[docs/upgrading.md](../upgrading.md). The roll order is the one of v0.4.0: **CRDs, then the
manager, then the providers**. The CRDs have no schema change, so applying them is safe at any
point; the `virtrigaud` chart has no RBAC or values change (its only template change is the
warning-only `vvmclone-customization.kb.io` webhook entry, rendered with `webhooks.enabled=true`),
so `helm upgrade` re-tags the images and runs the CRD hook with the new `kubectl` image. Roll the libvirt providers
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
   simulated run that you can stop with Ctrl-C, which exits non-zero by design). Set `vmCount` in
   each configuration (it caps the run at 10 VMs when omitted).
4. A `VMSnapshot` that v0.4.0 held in `SnapshotOutcomeUnknown` because of a leftover overlay file
   stays held: delete it and create one under another name, or confirm the attempt.
5. If a `VMClone` set `sysprep.productKey`, `sysprep.adminPassword.value`,
   `sysprep.joinDomain.password.value`, `sysprep.customCommands`, `certificates[].data`,
   `guestCommands` or `userData.ignition.inline` on v0.4.0 or earlier, rotate those values
   ([Security fixes](../upgrading.md#security-fixes)).
6. Make sure `curl` is installed on every libvirt host that creates VMs from an image URL or a
   predefined template.
7. Before you roll a libvirt provider, list the VMClasses with `firmware: UEFI` and decide for each
   (they now make UEFI VMs; see [Actions required](#actions-required)). Every host that can receive
   a UEFI or secure boot VM needs an OVMF package.
8. If you run v0.4.1-rc1, read
   [If you run v0.4.1-rc1](../upgrading.md#if-you-run-v041-rc1): VMs rc1 made keep what rc1 gave
   them.
9. If you run v0.4.1-rc2, read
   [If you run v0.4.1-rc2](../upgrading.md#if-you-run-v041-rc2): a `VirtualMachine`, `VMClone` or
   `VMSnapshot` that an rc2 manager already holds stays held until you confirm it, and the
   `CreateBackoff` message has new texts.

## Full changelog

The complete, dated list of changes is in [CHANGELOG.md](../../CHANGELOG.md). The pull requests
since v0.4.0 are #445 to #453 (rc1), #454 to #461 (rc2) and #462 to #464 (rc3; #462 only updates CI actions). Once the tag
exists, the commits are at
<https://github.com/projectbeskar/virtrigaud/compare/v0.4.0...v0.4.1-rc3>.
