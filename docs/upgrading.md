# Upgrading from v0.3.11

This guide covers upgrading a VirtRigaud installation from **v0.3.11** (2026-06-21) to
**v0.4.0**. That release contains an unusually large number of security fixes, several of which are
**breaking** or change operator-visible behavior. Read the "Breaking changes" table
below before you upgrade, then follow "Required upgrade order". If you are on a v0.4.0
release candidate, see [Upgrading from a v0.4.0 release candidate](#upgrading-from-a-v040-release-candidate).
For v0.4.1, see [Upgrading from v0.4.0 to v0.4.1](#upgrading-from-v040-to-v041).

Each change below links to the in-tree page that documents it in full — this guide
is the index and the sequencing, not a duplicate of that detail.

> If you are upgrading through several intermediate `main` snapshots (for example a
> CI/CD pipeline tracking `main`), read the caveats for **#339** (libvirt domain
> naming) and **#336** (chart NetworkPolicy key) — both describe a broken
> intermediate state that only matters if you deployed a `main` build from inside
> that window.

## Upgrading from v0.4.0 to v0.4.1

The rest of this guide covers the upgrade from v0.3.11 to v0.4.0. v0.4.1 is a patch release of
v0.4.0; from v0.4.0 (GA) the roll order is the same, **CRDs, then the manager, then the
providers**, and:

- **The CRDs have no schema change.** Only their `description` texts differ (`kubectl explain`
  shows the corrected text), and one new condition reason, `RevertNotPerformed`, is a constant in
  the Go API with no schema effect. Apply them as usual; the `virtrigaud` chart's CRD hook does.
- **The `virtrigaud` chart has no RBAC or values change** (only comments in `values.yaml`). Its
  webhook template gains the warning-only `vvmclone-customization.kb.io` entry, rendered only with
  `webhooks.enabled=true` (see [Security fixes](#security-fixes)). `helm upgrade` re-tags the
  images, and the CRD hook uses a newer `kubectl` image (1.35.9, was 1.32.0).
- **The gRPC contract has no field, number or RPC change** (comments only), so providers of the
  two versions interoperate. Roll the libvirt providers promptly after the manager: a v0.4.1
  manager reads a v0.4.0 libvirt provider's answers as they are (a single-host failed revert as
  a failure) and retries every "not performed" snapshot create, so until the provider is rolled a
  single-host overlay collision (V12) or a refusal libvirt will never accept is retried every 10
  minutes instead of failing; only a v0.4.1 provider answers them for good, as described below.

If you are upgrading from v0.3.11 or from a v0.4.0 release candidate, do the steps of the
sections below it as well, in the order the rest of this guide gives.

### Security fixes

- **Security: a `VMClone`'s unused customization secrets are no longer sent to the provider.**
  Before, the manager sent all of `spec.customization` in the `Clone` request, including
  `sysprep.productKey`, `sysprep.adminPassword.value`, `sysprep.joinDomain.password.value`,
  `sysprep.customCommands`, `certificates[].data`, `guestCommands[].command`/`.arguments` and
  `userData.ignition.inline`, which no provider acts on. From v0.4.1 the request carries only the
  fields a provider applies or reports; the fields above are never sent. Nothing changes in what a
  clone gets: `hostname`, `domain` and (libvirt) `userData.cloudInit.inline` are sent and
  applied as before, and the libvirt and vSphere providers still log the other fields that are
  set as not applied (except a `sysprep` that holds only `productKey` or `customCommands`, and
  an `ignition` that holds only `inline`, which the provider no longer sees).
  **If you set any of these fields on v0.4.0 or earlier, treat the values as disclosed to the
  provider (and, without gRPC TLS, to the network between manager and provider) and rotate
  them.** They are also still **stored in plaintext** in the `VMClone` — the v1beta1 API keeps
  the fields — so do not put a real password, key or other credential in them. Clearing a
  field does not remove copies elsewhere: etcd backups, API-server audit logs (at `Request`
  level or above), the `kubectl.kubernetes.io/last-applied-configuration` annotation and your
  GitOps sources. When one of these fields is set, the manager records a `Warning` event
  (reason `CustomizationSecretNotSent`) once the provider accepts the clone; it names the
  fields, never their values. With
  `webhooks.enabled=true` the chart also renders a new validating webhook,
  `vvmclone-customization.kb.io`, which only returns one admission **warning** naming such
  fields (`kubectl` prints it) on a create, and on an update that changes
  `spec.customization`. It never refuses a write, and its `failurePolicy` is always `Ignore`
  (timeout 5s), whatever `webhooks.validating.failurePolicy` says, so `VMClone` writes gain no
  admission dependency. It is advisory: to forbid these fields, add your own admission policy.
  With webhooks off (the chart default) and a clone the provider refuses, neither the warning
  nor the event appears; the field descriptions (`kubectl explain`) still say so.
  Kustomize users who enable `config/webhook` get the same entry from
  `config/webhook/manifests.yaml`.
- **Security: libvirt create-from-URL no longer puts the image URL on the host's command line.**
  When a create carries an image URL instead of a prepared image (the legacy create-from-URL
  path), the libvirt provider ran `wget <URL>` on the libvirt host, so the URL's user-info or
  presigned query string was visible in the host's process list and audit log, and the
  provider logged it in full. The download now goes the way an `ImagePrepare` download does
  ([`docs/image-preparation.md`](image-preparation.md#how-an-image-is-published)): `curl -q -K -`
  with the URL on **stdin**, exactly one transfer, `http`/`https`/`ftp` only (redirects
  included), a 30-second connect timeout and the `VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB`
  size limit. Its total time is 30 minutes or, when the `Create` call's deadline comes sooner,
  the time left until it less a tenth of that time (at most a minute) kept for the convert; the
  manager gives a `Create` 5 minutes, so a download is cut off after at most about 4.5 minutes (`wget` had no
  limit, but the call failed after 5 minutes anyway while `wget` ran on). The same cap now
  applies to `ImagePrepare` downloads, which are therefore cut off after about 4.5 minutes,
  whatever `spec.prepare.timeout` says (the 30-minute default included). The provider log shows the URL without its user-info and query,
  and errors never contain it. Every image URL takes this path, whatever its scheme: an
  `ftp://` URL is now downloaded (before, it was looked up as a template name, which logged
  it and returned it in the error), and any other scheme is refused (`InvalidSpec`). A URL
  with spaces or control characters is refused too, before anything runs. **Failures are
  classified now:** an HTTP 4xx answer (except 408, 425 and 429) and the permanent curl
  failures listed in [Which failures are retried](image-preparation.md#which-failures-are-retried)
  are reported as `InvalidArgument` — the `VirtualMachine` gets `Ready=False` with reason
  `ValidationError` and is re-checked every 30 seconds — instead of the generic provider
  error retried before. **The libvirt host needs `curl`** for this path (as for
  `ImagePrepare`; `wget` is no longer used); if it is missing, the provider log says so. The
  manager's debug log (`V(1)`) of a create request also shows the image URL redacted now.
- **Provider authors (SDK / third-party providers):** a `CloneRequest`'s `customize_json` is
  now an allow-listed subset of `spec.customization`, still in the shape of the v1beta1
  `VMCustomization` type: the fields that hold or commonly hold secret values (listed above)
  are never included, and a `sysprep` or `userData.ignition` that set only such fields
  arrives as an empty object. Do not rely on receiving them.

### Proxmox: user-data needs the SSH data plane

**Who is affected.** A Proxmox `Provider` whose `VirtualMachine`s carry `spec.userData`, or whose
`VMClone`s carry `spec.customization.userData.cloudInit.inline` (or clone a VM that has a seed).
A `VirtualMachine` without `spec.userData` needs nothing new. To list the VMs on Proxmox Providers
that carry user-data:

```bash
kubectl get virtualmachines -A -o json | jq -r '.items[]
  | select(.spec.userData != null)
  | "\(.metadata.namespace)/\(.metadata.name) provider=\(.spec.providerRef.name)"'
```

**What changed.** Up to v0.4.0 the provider read two things out of the user-data with a line scan
(the first `name:` as the user, and the `ssh_authorized_keys:` block-list items as that user's
keys) and dropped the rest silently, so `runcmd`, `write_files`, `packages` and a second user
never reached the guest. From v0.4.1 the provider builds a NoCloud seed CD-ROM on the PVE node
over SSH, and the **whole** user-data reaches cloud-init, as on libvirt and vSphere. The seed is
used for a new VM (from a template or without one), for a clone that needs one and for a
migration's imported disk, and not for a VM that already exists: **existing VMs keep their PVE
cloud-init drive and are not converted.**
→ [`docs/proxmox-cloud-init-storage.md`](proxmox-cloud-init-storage.md)

**What you must do before you roll the Proxmox provider.**

1. **Add the SSH keys to the Provider's credentials Secret** (the one `spec.credentialSecretRef`
   names, which already holds `token_id` and `token_secret`, or `username` and `password`):

   | Key | Value |
   |---|---|
   | `ssh_user` | `root` (also the default). The seed script runs `qm importdisk` and `qm set` and owns a root-only build directory, so a non-root user does not work. |
   | `ssh_privatekey` | The PEM private key of `root` on the PVE node. Use a key only the provider uses. |
   | `ssh_password` | Instead of a key. If both are set, the password is used. |
   | `known_hosts` | The node's host key line(s): `ssh-keyscan -H <node host> > known_hosts` from a trusted machine, and compare the fingerprint with the node's. Host-key verification is on by default and has no trust-on-first-use. |

   The SSH host is the host of the Provider's `spec.endpoint`. The same keys are what a
   `VMMigration` to or from this Provider already needs; see
   [`examples/migration/vsphere-to-proxmox.yaml`](../examples/migration/vsphere-to-proxmox.yaml).
   Setting `PROXMOX_INSECURE_SKIP_HOST_KEY_VERIFICATION=true` in the Provider's
   `spec.runtime.env` skips host-key verification instead of `known_hosts`; it is flagged in the
   provider log and is not recommended.
2. **Check the node.** `genisoimage` and `flock` must be installed on the PVE node SSH reaches
   (`command -v genisoimage flock`; both are on every stock PVE node), and the storage the seed
   goes to must have the `images` content type
   ([how the storage is chosen](proxmox-cloud-init-storage.md#how-the-storage-is-chosen)).
3. **In a PVE cluster, set `PVE_NODE_SELECTOR`** in the Provider's `spec.runtime.env` to the node
   of the API endpoint's host. The VM must be on the node SSH reaches (the node check compares it
   with `uname -n` there); an `endpoint` that is a load balancer in front of several nodes does not
   work for VMs with user-data. The selector applies to every new VM of the Provider, with or
   without user-data: its templates (and clone sources) must be on that node, or a template
   `Create` is refused (`template ... not found on node ...`).
4. **Token privileges.** The seed itself is imported and attached by `root` over SSH. A VM made
   **without a template** with user-data is now created first and marked ready afterwards, so the
   token also needs `VM.Audit` and `VM.Config.Options` on it, as it already does for clones.

**What happens without them.** A `Create` or `Clone` that needs a seed and cannot get one is
refused **before anything is created** (`InvalidSpec`), with a message that names what is
missing (the SSH data plane, `ssh_password` or `ssh_privatekey`, `known_hosts`, `genisoimage`, or
`PVE_NODE_SELECTOR`). The user-data is never dropped in silence any more. A `VirtualMachine`
shows `ValidationError` and is rechecked every 30 seconds; it needs no cleanup, and is created
on a recheck once the keys are there. A `VMClone` ends `Failed` (`ProviderError`, "clone
failed: ...") and is not retried: re-create it. The provider reads the SSH keys when it starts,
so roll or restart it after you change the Secret. A node without `flock` is not refused: the
seed script cannot take its lock, the VM is kept with an unknown outcome and every re-send
repeats it, so check `flock` on the node first.

**Security.** The Provider now holds a root SSH credential to the PVE node. Its changes to the
node's config and storage are attributed to `root@pam`, outside the API token's permissions.
Restrict who can read the Secret. The user-data is stored in plaintext in the seed volume for
the VM's whole life; keep long-lived secrets out of it.
→ [Security posture](proxmox-cloud-init-storage.md#security-posture)

**Behaviour you will see on the new VMs.**

- **A seed on node-local storage blocks `qm migrate`.** qemu-server refuses to migrate a VM with a
  local CD-ROM image other than its own PVE cloud-init drive, online or offline: HA and node
  evacuation cannot move such a VM. Put the VM on a shared storage with `images`, or detach and
  free the seed before moving it (`pvesm free <volume>`; removing only the CD-ROM leaves the
  volume on the storage).
  → [Known limitations](proxmox-cloud-init-storage.md#known-limitations)
- **A clone does not carry its source's user-data.** A clone of a VM that has a seed, and any
  `VMClone` that gives inline user-data, gets a seed of its own: `instance-id`
  `virtrigaud-<UID of the target VirtualMachine>`, its own hostname, the `VMClone`'s inline
  user-data (else an empty `#cloud-config`) and DHCP on its own MACs. libvirt and vSphere clones
  re-run the source's user-data; a Proxmox clone does not, because its disk already holds what
  the user-data applied. Its first boot is a new cloud-init instance: new SSH host keys, and
  per-instance modules run again with the distribution's defaults where the user-data is empty
  (on most cloud images `disable_root: true`, `ssh_pwauth` back to the image default, and the
  default user created). Give the `VMClone` inline user-data that sets what the clone must keep.
  A clone of a VM without a seed and without inline user-data is unchanged.
  → [Lifecycle](proxmox-cloud-init-storage.md#lifecycle)
- **Linked clones.** A `VMClone` with `spec.options.type: LinkedClone` for a Proxmox Provider is
  refused with reason `LinkedCloneUnsupported`, before the provider is called. The provider never
  made linked clones and no longer advertises them. Use `FullClone`.
- **A clone gets the class's memory** (it kept the source's memory before, whatever the class
  said), so a clone's memory can differ from v0.4.0's for the same `VMClone`.
- **The boot order of a VM cloned from a template with user-data** names only its boot disk (the
  template's first boot disk, else its `bootdisk`, else the first disk) and no longer the
  cloud-init slot. A `VMClone` and a template `Create` without user-data keep the template's or
  source's order. Prepare templates with `qm set <vmid> --boot order=<boot disk>` (for example
  `scsi0`).
  → [Boot order](proxmox-cloud-init-storage.md#boot-order)
- **A cloud-init drive left on `local` by v0.4.0** can be moved with
  `qm set <vmid> --ide2 <storage>:cloudinit` on the node.

**Rolling back.** The v0.4.0 Proxmox provider knows nothing about a seed. A VM with a seed keeps
working, but do not clone a VM that has a seed with a v0.4.0 provider: PVE's clone copies the
source's CD-ROM line, so the clone would point at its source's seed volume (same identity, and a
volume it does not own).

### libvirt: snapshot and revert answers

These change what an operator sees; no action is required before the upgrade.

- **A new disk-only snapshot is refused when its overlay file is already on the host (V12).**
  The overlay of a disk-only snapshot is `<disk file without its extension>.<snapshot name>`
  (for the VM's own disk `<domain>-disk.<snapshot name>`). In v0.4.0 a clustered Provider
  answered libvirt's refusal with a generic error, so the create was re-sent about once a
  minute and the `VMSnapshot` was held in `SnapshotOutcomeUnknown` after ten re-sends; a
  single-host Provider failed it with no hint. Now the provider probes the paths before it
  changes anything and refuses: the `VMSnapshot` is `Failed` (`Ready=False/ProviderError`, a
  `SnapshotFailed` event), on single-host and clustered Providers, and the leftover file is
  never removed or overwritten. Use another snapshot name, or remove the file after you have
  checked that no domain, snapshot or internal snapshot on any host that mounts the pool
  references it. A `VMSnapshot` that v0.4.0 held for this reason **stays held** after the
  upgrade: delete it and create one under another name, or confirm the attempt with the
  `virtrigaud.io/operation-outcome` annotation its message names (the confirmed create is then
  refused as above, or taken if you removed the file first).
  → [`docs/libvirt-clones.md`](libvirt-clones.md#a-snapshot-name-whose-overlay-file-is-still-on-the-host)
- **A `VMSnapshot` create the provider did not perform is retried on a single-host Provider too
  (behaviour change; the controller change applies to every provider type).** v0.4.0 ended it `Failed` and you deleted it and created a new one; a
  clustered Provider already retried it. Now the `VMSnapshot` shows `Ready=False/ProviderError`
  with the provider's answer in a `SnapshotCreateRetrying` event, and the create is sent again
  after 1 minute, doubling to 10 minutes, for as long as the object exists. This includes a
  request that never reached the provider and an unreachable host. A cause that does not go
  away (for example an unreadable disk image: grant `VR_DISK_READ`) is retried every 10 minutes
  until you fix it or delete the `VMSnapshot`. The source snapshot of a `VMMigration` is
  unchanged: "not performed" fails the attempt and the retry policy decides.
  → [`docs/provider-call-outcomes.md`](provider-call-outcomes.md)
- **A revert the provider did not perform is retried, not counted as an unknown outcome.** The
  `VirtualMachine`'s `status.snapshotRevert` goes back to `Pending` with
  `SnapshotReverted=False/RevertNotPerformed` and a `SnapshotRevertNotPerformed` Warning event,
  and the revert is sent again as a new attempt after 1 minute, doubling to 10 minutes. These
  answers do not count toward the 5-attempt limit and no longer end `Failed` with "the VM may or
  may not have been reverted" (the exception is a "not performed" answer to the manager's own
  re-send of an attempt whose outcome is unknown: it stays an unknown outcome and is counted). If you keep a script or alert on that message or on
  `status.snapshotRevert.attempts`, note that `attempts` counts since the last "not performed"
  answer. A revert that keeps being refused is sent again until you fix the cause or remove
  `spec.snapshot.revertToRef`.
  → [`docs/vm-snapshot-revert.md`](vm-snapshot-revert.md#asynchronous-reverts-and-unknown-outcomes)
- **A failed libvirt revert is answered by what the provider proves**: not performed, refused
  for good (`FailedPrecondition` for a revert libvirt does not support for the VM), `NotFound`,
  a terminal failure, or "outcome unknown: retry". Single-host messages are constant texts
  that name no host path or command; the details are in the provider log. A single-host lost
  connection mid-revert is no longer reported as a terminal failure.
- **On a single-host Provider, a snapshot libvirt cannot take for this VM fails for good**
  (`InvalidArgument`) when libvirt refuses it before executing because the VM's configuration does not support it; a busy domain
  or one in the wrong state is still "not performed".
- **Host-command logs are redacted (R28).** URLs (a staged disk's `nfs://` server, export and
  path, an S3 endpoint) are replaced by `<redacted <scheme> URL>` in the libvirt provider's log
  and errors, except the libvirt connection URI and `pvc://`. If you collected libvirt provider
  logs from v0.4.0 for support or audit, they still hold the URLs; treat them as sensitive.
- **A VM delete refreshes the `default` pool (R37)** so `virsh vol-list default` stops listing the
  deleted disk files (best effort; a VM whose disks are in another pool leaves that pool stale
  until its next refresh).

### Command-line tools

- **`vcts` is stricter.** A custom conformance specification that `vcts validate` accepted before
  may be rejected: unknown keys, a step resource that is not an `infra.virtrigaud.io/v1beta1`
  kind or names a namespace, a created object's name without `${RUN}`, a `wait` step without a
  resource and a condition or `validate` entries, unknown capabilities, malformed paths,
  variables or regular expressions, duplicate test names, test names over 63 characters, and a step that creates,
  updates or deletes a `Provider`. Run
  `vcts validate <dir>` on your specifications before you rely on them in CI.
  `vcts run` now executes the steps (it reported every step as "not implemented" before) and
  creates real objects in the namespace you pass with `-n` (`default` if omitted): run it against a non-production
  cluster. The default `--timeout` is 3 hours (was 30 minutes).
- **`virtrigaud-loadgen` validates ranges and does real work.** A configuration whose
  `vmTemplate.resources` ranges are outside the `VirtualMachine` `spec.resources` bounds, accepted
  before because the ranges were unused, is rejected. `power`, `reconfigure`, `snapshot` and
  `clone` now act on VMs the run created. `--dry-run` makes no API call and needs no kubeconfig.
  Without `--cleanup` the command prints how to remove what the run left.
  `test/performance/proxmox-loadtest.yaml` is rewritten in the tool's configuration format.
  → [`docs/cli-tools.md`](cli-tools.md)
- **`vrtg-provider init --type proxmox`** is new.

### Provider authors: the SDK and proto Go modules are published

From v0.4.1 on, each release tags `sdk/vX.Y.Z` and `proto/vX.Y.Z` next to `vX.Y.Z` (release
candidates too), and `vrtg-provider init` from a release writes a `go.mod` that requires them at
its own version instead of `replace` directives to a checkout. An existing project can drop its
`replace` directives and require all three modules (`github.com/projectbeskar/virtrigaud`,
`.../sdk`, `.../proto`) at the same release. Releases up to v0.4.0 have no `sdk/` or `proto/`
tags. `go install github.com/projectbeskar/virtrigaud/cmd/<tool>@vX.Y.Z` does not work (the root
`go.mod` has `replace` directives, which `go install <package>@<version>` refuses): download the
CLI binaries from the GitHub release. See [`docs/go-modules.md`](go-modules.md).

### Deprecated: the `virtrigaud-provider-runtime` chart

`charts/virtrigaud-provider-runtime/` is removed in the next minor release. It was never
published to the Helm repository or released with the `virtrigaud` chart. It runs a provider
`Deployment` outside a `Provider` resource, which the manager never connects to (the manager talks
only to the provider it runs for a `Provider`, at `status.runtime.endpoint`), and it bypasses what
the manager sets up for that provider: the gRPC TLS/mTLS settings, the credential `Secret` mount,
a dedicated ServiceAccount and the hardened security context. If you installed it, create a
`Provider` resource for each provider instead, wait until it is `Ready`, then `helm uninstall`
the chart's release. The chart's [README](../charts/virtrigaud-provider-runtime/README.md) maps
its values to `Provider` fields.

### Other changes

- **API and proto comments.** The descriptions in the CRDs and `provider.proto` are corrected
  (fields that nothing acts on say so; the two `insecureSkipVerify` switches are described as
  separate). No field, number, RPC or option changed.
- **`providers/catalog.yaml`** now lists `v0.4.0` tags and `skip` (not run) for every conformance
  profile, instead of 0.1.x tags and passes nobody had run. It still lists the `clone` profile for
  Proxmox, which no longer advertises linked clones.
- **CI** checks proto compatibility with `buf breaking` on pull requests.

### Verifying the upgrade

- Proxmox, a VM with user-data created after the upgrade: `qm config <vmid>` shows a CD-ROM
  (`ide2: <storage>:vm-<vmid>-disk-<N>,media=cdrom`, on a directory storage
  `<storage>:<vmid>/vm-<vmid>-disk-<N>.raw`, and another slot if the VM's drive was in one),
  and in the guest `cloud-init status --long` reports `done` with the `runcmd` and `write_files`
  of your user-data applied.
- libvirt: create a `VMSnapshot` whose name matches an existing overlay (for a test VM) and check
  it ends `Failed` with the "overlay file ... already exists" message and the file is untouched.
- A `VMClone` with `LinkedClone` for a Proxmox Provider fails with `LinkedCloneUnsupported`.
- With webhooks enabled, `kubectl apply` of a test `VMClone` with `sysprep.productKey` set prints
  one admission warning naming the field, and once the clone starts the `VMClone` has one
  `CustomizationSecretNotSent` event; neither shows the value.

## Breaking changes

| Change | Who is affected | Action required |
|---|---|---|
| **Cross-namespace `Provider` / `VMClass` / `VMImage` references need a grant** ([`docs/cross-namespace-references.md`](cross-namespace-references.md)) | Anyone whose VirtualMachines (or VMClone/VMMigration targets) reference a Provider, VMClass or VMImage in **another** namespace — e.g. a shared Provider in `virtrigaud-system`. Such VMs **fail closed** after upgrade (`Ready=False`, reason `ConsumerNotAllowed`, no provider calls) until access is granted. | **After applying the new CRDs and before rolling the manager**, set `spec.consumerNamespaceSelector` on every shared Provider, VMClass and VMImage (`{}` = shared with all namespaces; or a label selector on the consumer namespaces). The field does not exist until the CRDs are upgraded: an older CRD rejects it (strict field validation) or prunes it. See step 2 of "Required upgrade order". Deleting a VM whose Provider is refused needs `virtrigaud.io/orphan-on-delete: "true"` or `force-delete`. See the upgrade notes in the linked doc for queries that list what needs a selector. |
| **`VirtualMachine.spec.providerRef` is immutable once bound** ([#341](https://github.com/projectbeskar/virtrigaud/pull/341), [`docs/vm-provider-binding.md`](vm-provider-binding.md)) | Anyone whose tooling edits `providerRef` after creation, and anyone using the old "re-point to a missing Provider, then delete" un-adopt trick | Detach with `virtrigaud.io/orphan-on-delete: "true"` instead. Upgrade CRDs with or before the manager — the manager refuses readiness on an old CRD. |
| **Cross-namespace VMClone/VMMigration targets need a grant** ([#340](https://github.com/projectbeskar/virtrigaud/pull/340), [`docs/cross-namespace-targets.md`](cross-namespace-targets.md)) | Anyone whose `spec.target.namespace` differs from the object's own namespace | Annotate the target namespace: `kubectl annotate namespace <target> infra.virtrigaud.io/allowed-source-namespaces=<source-ns>[,<source-ns2>...]`. Do this **before** upgrading if you have objects in flight — see the query in the linked doc. |
| **Linked clones fail closed; no libvirt linked clone into another namespace** ([`docs/cross-namespace-targets.md`](cross-namespace-targets.md#no-libvirt-linked-clone-into-another-namespace)) | Anyone creating a `VMClone` with `options.type: LinkedClone`. It is now issued only when the Provider reports `supportsLinkedClones` (a Provider whose capabilities cannot be read makes the clone wait instead of proceeding), and a libvirt linked clone whose target is in another namespace is refused (`LinkedCloneCrossNamespace`). vSphere linked clones in any namespace are unchanged. | Use a full clone for a libvirt target in another namespace. |
| **`Provider.spec.type` is immutable** (`api/infra.virtrigaud.io/v1beta1/provider_types.go`, CRD CEL rule plus the Provider validating webhook) | Anyone whose tooling edits `spec.type` on an existing Provider. The controllers decide type-specific safety rules from it (for example, a libvirt linked clone into another namespace is refused), and a Provider's VMs were created by the provider implementation of its type, so an in-place re-type is now rejected: `spec.type is immutable; create a new Provider to change it`. Updates that leave the type alone are unaffected. | Create a new Provider of the other type instead of editing `spec.type`. Upgrade CRDs with or before the manager. |
| **vSphere Create/Clone fail closed on VM ownership** ([#335](https://github.com/projectbeskar/virtrigaud/pull/335), [`docs/vm-ownership.md`](vm-ownership.md)) | All vSphere users | Grant the vCenter account **Virtual machine > Change Configuration > Advanced configuration** (`VirtualMachine.Config.AdvancedConfig`). Rename any VirtualMachine, VMImage prepare target, or bare template name matching `vm-<digits>`. Make sure every `VMImage.templateName` names a real vSphere template, not a regular VM. |
| **libvirt default cloud-init no longer provisions any credentials** ([#328](https://github.com/projectbeskar/virtrigaud/pull/328)) | Anyone relying on the default `ubuntu` user / SSH key / passwordless sudo on a libvirt VM created **without** `spec.userData` | Supply your own `spec.userData` with a user and key going forward. On VMs already created without `spec.userData`, the old hardcoded key and sudo grant are still present *inside the guest* — remove `/home/ubuntu/.ssh/authorized_keys`'s VirtRigaud entry and the `/etc/sudoers.d/90-cloud-init-users` drop-in by hand if you want them gone; rolling the provider image does not touch running VMs. |
| **libvirt: base images are confined and always copied, not attached in place** ([#334](https://github.com/projectbeskar/virtrigaud/pull/334), [`docs/image-preparation.md`](image-preparation.md#libvirt-image-paths-sourcelibvirtpath)) | libvirt users with images outside `/var/lib/libvirt/images`, in a non-default pool, or attached in place by an earlier release | Set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` (via `Provider.spec.runtime.env`) if images live elsewhere. Images an earlier release attached in place are now refused as a base — create a `VMImage` under a **new name** to get a fresh copy. **Before deleting any pre-upgrade libvirt VM**, run `virsh domblklist --details <domain>` on every domain on the host and check for a shared disk file — deleting one VM can delete a disk that other VMs still use (with the libvirt delete-safety fix below, the provider refuses such a delete with `DeleteBlocked` instead). Expect more disk usage and slower creates from the copy. |
| **vSphere: an imported disk is attached in place only when it is the VM's own verified migration disk; landing names carry the namespace** (security fix, `internal/providers/vsphere/disk_guard.go`) | Anyone who sets `VirtualMachine.spec.importedDisk` on a vSphere Provider by hand, and anyone with a vSphere `VMMigration` in flight during the upgrade. The vSphere provider attached any datastore path in `spec.importedDisk.path` to the new VM as its disk, in place, so a tenant could name another VM's disk and read and write it, grow it, and delete it with its own VM; and migrations landed every disk at `[<datastore>] <vm name>-migrated`, so a same-named VM in another namespace could reach, overwrite or delete another tenant's landed disk. Migrations now land at `[<datastore>] <namespace>_<vm name>-migrated/<namespace>_<vm name>-migrated.vmdk` (`_` appears in no Kubernetes name, so no VM can be named like a landing folder; a name over vCenter's 80-character limit is shortened with a hash). Create refuses, before anything is created, unless the manager verified the path as the disk a `VMMigration` in the VM's own namespace imported for this VM, the path is exactly that VM's namespaced landing path, and no VM or template with files on that datastore (or on the same storage under another datastore name) uses the disk, compared by path and by disk UUID. A wrong path is `Ready=False/ValidationError` (`InvalidArgument`, not retried); a disk another VM uses is a `Conflict` (`FailedPrecondition`, `VM_DISK_IN_USE`); when the check cannot run — the scan fails, a VM on that datastore is inaccessible or has a disk with neither a readable path nor a UUID, or the account lacks `System.Read` — nothing is attached and the create is retried (`Unavailable`, `VM_DISK_CHECK_FAILED`, not counted toward the circuit breaker). A landing disk that does not exist is `FailedPrecondition` ("imported disk not found"): run the migration again. A `VMImage` that carries both a vSphere template and another provider's `path` now clones the template on vSphere. A migration import refuses to reuse a landing folder whose files a VM still uses, and a failed import deletes its folder only if it is still unused. Deleting a vSphere VM first detaches, keeping the file, any disk outside its own directory that is not its own landing disk or that another VM uses; with snapshots referencing such a disk the delete is refused (`DeleteBlocked`, `VM_DISK_IN_USE`). | **Finish or redo in-flight vSphere migrations around the upgrade.** A migration whose disk was landed by the old provider but whose target VM was not yet created is refused after the upgrade (`ValidationError`: "landed by a release before v0.4.0"); delete that target VirtualMachine and its `VMMigration`, run the migration again, and remove the old `[<datastore>] <vm name>-migrated` folder by hand once nothing uses it. Migrations that complete before the upgrade, and VMs already created from a migrated disk, are unaffected. Grant the provider's vCenter account `System.Read` on the vCenter root folder, propagated (see "New required privileges and config"). Run one replica of each vSphere provider. **Before deleting a pre-upgrade vSphere VM that has a `spec.importedDisk` or disks attached by hand**, check in vCenter that its disks are its own. |
| **Proxmox: an imported disk is imported only when it is the VM's own verified migration stage; stages are named per namespace** (security fix, affects v0.3.11 and later, `internal/providers/proxmox/import_stage.go`) | Anyone who sets `VirtualMachine.spec.importedDisk` on a Proxmox Provider by hand, anyone with a cross-namespace `VMMigration` into Proxmox, and anyone with a Proxmox-target `VMMigration` in flight during the upgrade. When the migration SSH data plane was configured, the Proxmox provider ran `qm importdisk` on **any** node path in `spec.importedDisk.path` and then deleted it with `rm -f`, so a tenant could copy any file or block device the SSH user can read on the PVE node (another VM's disk, `/etc/pve/priv/*`) into their own VM and have the provider delete the original. Create now takes the import path only when the manager verified the path as the disk a `VMMigration` in the VM's own namespace imported for this VM, and the path is exactly `/var/lib/virtrigaud/import-stage/<namespace>_<vm name>-migrated.<qcow2\|raw\|vmdk>` — the stage `ImportDisk` now creates for that VirtualMachine (a name over 200 bytes is shortened with a hash). `qm importdisk` runs only if the stage is a regular file, not a symbolic link, with one hard link, and the provider removes no node file that is not an import stage. Anything else is refused before anything is created (`Ready=False/ValidationError`, `InvalidArgument`, not retried; the message never contains the path). `ImportDisk` refuses a request without the target VM's namespace and name (a manager older than this release). A cross-namespace migration into Proxmox is not verified as the VM's own (its `VMMigration` lives in the source namespace), so its target VM's create would be refused: the manager now fails such a migration at `Validating` (`Phase=Failed`, reason `ProxmoxCrossNamespaceTarget`, constant message) before anything is exported or imported, and one that passed `Validating` before the upgrade fails before its import. A stage an earlier release created under the bare `/var/tmp/.virtrigaud-import-<vm name>-migrated.<ext>` name is refused even for the VM's own migration (`ValidationError`: "staged by an earlier release"), because same-named VMs in different namespaces shared that file; the provider never removes such a file. | **Upgrade the manager before the Proxmox provider** (an older manager sends neither the verification flag nor `target_vm`, so every Proxmox-target migration would be refused). **Finish or redo in-flight Proxmox-target migrations around the upgrade.** A migration whose disk was staged by the old provider but whose target VM was not yet created is refused after the upgrade; delete that target VirtualMachine and its `VMMigration` and run the migration again. Migrations that complete before the upgrade, and VMs already created from a migrated disk, are unaffected. Stages now live in `/var/lib/virtrigaud/import-stage/` (created `0700` by the provider; keep it root-only, an import fails otherwise). Remove leftover `/var/tmp/.virtrigaud-import-*` files on the PVE nodes by hand once no migration is in flight: the provider never stages there any more and never removes them. Re-create a granted cross-namespace Proxmox migration as one in the target VM's own namespace. Sweep stale stages periodically, and run one VirtRigaud installation per PVE node (see [Stale Proxmox import stages](#stale-proxmox-import-stages)). |
| **`spec.resources` overrides are now actually sent to the provider — a running VM with one set may resize on upgrade** (`internal/controller/virtualmachine_controller.go`) | Any VirtualMachine with `spec.resources.cpu` and/or `spec.resources.memoryMiB` set. A bug meant the override was only ever compared against and recorded in `status.currentResources` — never sent in the Create/Reconfigure request the VMClass's own CPU/memory went out in — so a VM could report a size (`status.currentResources`) it never actually had. Fixed: both the request and `status.currentResources` now use the same effective (VMClass ⊕ override) values. On the **first reconcile after upgrade**, a VM whose override was never applied now differs from its (corrected) desired size and goes through the normal reconfigure path — exactly as any other legitimate VMClass resize does today. | Before upgrading, find affected VMs: `kubectl get virtualmachine -A -o json \| jq -r '.items[] \| select(.spec.resources != null) \| "\(.metadata.namespace)/\(.metadata.name): \(.spec.resources)"'`. Confirm the override is the size you actually want — the VM **will be resized to it**. Reconfigure follows the existing online/offline rules unchanged by this fix: it applies live only when the VMClass has CPU/memory hot-add enabled and enough headroom was provisioned at create; otherwise the new size is written to the VM's offline config and takes effect only after a restart (the provider logs a WARN that a restart is required — the operator does not restart the VM for you, and `status.currentResources` is still updated to the requested size once the config write succeeds, ahead of the guest actually seeing it). To keep a VM at its current size, remove `spec.resources` before upgrading — an override removed is treated the same as one changed, and the VM reconciles back to its plain VMClass size. An out-of-range override (outside the VMClass's own CPU/memory bounds) is refused with `Reconfiguring=False/ValidationError` instead of being sent to the provider. |
| **`powerOn` is honored: clones and migrated VMs that do not set `powerOn: true` stay powered off** (`VMClone` `spec.options.powerOn`, `VMMigration` `spec.target.powerOn`; [`examples/vmclone-basic.yaml`](../examples/vmclone-basic.yaml)) | Anyone creating a `VMClone` or `VMMigration` without `powerOn: true`. Both fields are documented with a default of `false`, but neither was ever read: the produced VirtualMachine got an empty `spec.powerState`, which the VirtualMachine controller treats as `On`, so every clone and every migrated VM came up running — even with an explicit `powerOn: false`, as every shipped migration example sets. The produced VirtualMachine's `spec.powerState` is now `On` for `powerOn: true` and `Off` otherwise. Every provider's Clone leaves the clone powered off, so a clone is never powered on and then off. VirtualMachines that clones or migrations produced before the upgrade keep the power state they have. | Set `spec.options.powerOn: true` (VMClone) or `spec.target.powerOn: true` (VMMigration) where the new VM must run, or set `spec.powerState: On` on the produced VirtualMachine afterwards. Check automation that expects a clone or migrated VM to be running right after it completes. **`deleteAfterMigration: true` with `powerOn: false` (or omitted):** the migration completes and deletes the source VM even though the target was never started, so the workload then runs nowhere until you power the target on. The VMMigration gets a `SourceDeletedTargetNotStarted` Warning event when that happens. Set `spec.target.powerOn: true` on every migration that also sets `spec.source.deleteAfterMigration: true`, unless a powered-off target is what you want. |
| **libvirt `LinkedClone` is refused; use `FullClone`** ([`docs/libvirt-clones.md`](libvirt-clones.md#linked-clones-are-disabled)) | Anyone creating `VMClone`s with `spec.options.type: LinkedClone` through a libvirt Provider. A libvirt linked clone reads the source VM's live, unfrozen disk: powering the source on while a linked clone of it exists corrupts the clone, and no provider check can catch that. The libvirt provider now reports `supportsLinkedClones: false`, so such a `VMClone` fails before any provider call (`Phase=Failed`, reason `LinkedCloneUnsupported`); a linked `Clone` that still reaches the provider is refused with `InvalidArgument` before any host command. Full clones and vSphere linked clones are unchanged. | Use `spec.options.type: FullClone` (the default) for libvirt, and recreate any failed `VMClone` as a full clone. Existing libvirt linked clones keep working and are not touched, but their source VM cannot be deleted, reverted or snapshotted while the clones exist (see the linked-clone row below) — keep those sources powered off. Linked clones return once the base is frozen at clone time. |
| **NFS migrations refuse `nfs.uid: 0` / `nfs.gid: 0`, for every provider type** ([`examples/migration/README.md`](../examples/migration/README.md)) | Anyone whose nfs `VMMigration` sets `spec.storage.nfs.uid` or `gid` to `0`, whatever its source and target Providers (libvirt, vSphere, Proxmox). The provider presents that identity to the NFS server as its AUTH_SYS credential, which the client chooses: on an export without `root_squash` uid 0 is root, able to read or overwrite every staged disk there. Such a migration now fails at `Validating` before anything is read or done (`Phase=Failed`, `Validating=False`, reason `NFSRootIdentityNotAllowed`). The providers also refuse uid or gid 0 in the storage options they receive (`InvalidArgument`, constant message), whatever sent them. | Use a dedicated non-zero uid/gid that owns the export (and set the export's files to it), or leave `uid`/`gid` unset. Re-create the failed migration. |
| **A `VMMigration` acts only on the target `VirtualMachine` it created** (security fix, [`docs/cross-namespace-targets.md`](cross-namespace-targets.md#the-target-vm-must-be-the-migrations-own); new `status.targetVMUID` / `status.targetVMClaim`) | Anyone whose `VMMigration` target name (`spec.target.name`, in its own or a granted namespace) is already taken by a `VirtualMachine` the migration didn't create. Before, the migration took that VM as its own target: it reported the VM's status as the result, marked it `migration-completed`, and on deletion could delete it if it carried a copied `virtrigaud.io/migration` annotation. Now the migration fails at `Validating` (before the source is powered off or exported) with reason `TargetVMNotOwned` and a fixed message, and never touches that VM. Also affected: a migration that's in `Creating` or `Validating-Target` **during the upgrade**. Its target VM was created without the new UID/claim binding, so it fails with `TargetVMNotOwned` and its VM is left as it is. Deleting a migration that failed before the upgrade no longer deletes its partly created target VM. | Before upgrading, let migrations in `Creating` / `Validating-Target` finish, or don't start new ones. After the upgrade, for a migration that failed with `TargetVMNotOwned`: free the name (delete or rename the other VM) or pick another `spec.target.name`, then re-create the migration. For one caught mid-upgrade, check its target VM, then delete the source yourself if you meant to (`deleteAfterMigration` doesn't run for a failed migration). Delete partly created target VMs of migrations that failed before the upgrade yourself. |
| **Only the manager may set the manager-owned `virtrigaud.io` keys on a `VirtualMachine`** (security fix, ValidatingAdmissionPolicy, [`docs/admission-policy.md`](admission-policy.md); chart value `admissionPolicy.reservedKeys`, kustomize `config/admission-policy`) | Anyone who creates a `VirtualMachine` carrying, or adds or changes on one, a migration, clone or adoption marker (`virtrigaud.io/migration`, `virtrigaud.io/migration-claim`, `virtrigaud.io/migrated-from`, `virtrigaud.io/imported-disk-id`, `virtrigaud.io/disk-checksum`, `virtrigaud.io/migration-completed(-at)`, `virtrigaud.io/clone`, `virtrigaud.io/clone-uid`, `virtrigaud.io/cloned-from`, `virtrigaud.io/adopted-host`, `virtrigaud.io/adopted-id`, label `virtrigaud.io/adopted`) under any identity other than the manager's ServiceAccount. That includes backup tools that restore `VirtualMachine`s with their annotations (Velero), and a second VirtRigaud installation's manager in the same cluster. Updates that leave those keys unchanged, removing one, and the keys users set (`orphan-on-delete`, `force-delete`, `create-outcome`, `placement-uid`) aren't affected. The policy is installed on Kubernetes 1.30+ only (nothing is rendered below). **Residual risk below 1.30:** single-host adoption (`virtrigaud.io/adopted` + `virtrigaud.io/adopted-id` + name), clustered adoption (`adopted-host` / `adopted-id`) and `VMClone` (`clone-uid`) still decide bindings from those markers, so there anyone who can create `VirtualMachine`s in those namespaces can forge them. | On a cluster older than 1.30, restrict `create`/`update` on `virtualmachines` to trusted users, or reserve the keys with Kyverno or Gatekeeper. Don't disable the policy on 1.30+ without such a replacement. Installing it needs rights to create `validatingadmissionpolicies` and `validatingadmissionpolicybindings`. Add restore tools and other managers to `admissionPolicy.reservedKeys.exemptUsernames` / `exemptGroups` before you rely on them. To opt out, set `admissionPolicy.reservedKeys.enabled: false` (kustomize: drop `../admission-policy` from `config/default`). |
| **vSphere and Proxmox `Reconfigure` no longer resize the root disk** ([`docs/reconfigure-results.md`](reconfigure-results.md#which-disk-a-size-goes-to)) | vSphere and Proxmox VMs with `spec.disks` entries whose CPU or memory is changed (the disk sizes travel with that `Reconfigure`). Before, vSphere grew the **first** disk — the root disk — to `spec.disks[0].sizeGiB`; Proxmox sent every `spec.disks` size to `scsi0` (the root disk), refused one smaller than `scsi0` as a shrink, and after a disk grow dropped the CPU/memory change while reporting success. Now `Reconfigure` never touches the root disk on either (the VMClass `spec.diskDefaults.size` is not applied by `Reconfigure`, as before). On Proxmox no `spec.disks` size changes any disk (the Proxmox `Create` attaches none) and the CPU/memory change is always sent. On vSphere a `spec.disks` entry grows the additional disk `Create` attached for it — only on VMs created by this release, which record the entry's name with the disk; a smaller `spec.disks` size is refused (`InvalidSpec`, nothing changed); a delta (snapshot/linked-clone) or physical-mode RDM disk, an ambiguous name, and an entry no disk is recorded under are left alone. Additional disks of VMs created by v0.3.11 (recorded without a name) are never resized by `Reconfigure` — as before. | Anyone who grew a Proxmox root disk through a `spec.disks` entry (e.g. one named `root`): that no longer happens, and nothing replaces it in this release — grow the disk in PVE (applying the VMClass size at Proxmox `Create` and a disk-resize flow are follow-ups). On vSphere, a `spec.disks` size below the disk's current size now fails the Reconfigure (`Reconfiguring=False/ProviderError`), CPU/memory change included, until it is raised back. A `ReconfigVM_Task` vCenter fails (and a Proxmox config update PVE fails) is answered `VM_OPERATION_FAILED` and no longer counts toward the Provider's circuit breaker. The Proxmox behaviour is validated against the in-repo PVE fake, not a real PVE. |
| **vSphere: a change a VM cannot take while it is not powered off waits for a power-off** ([`docs/reconfigure-results.md`](reconfigure-results.md#vsphere-changes-a-running-vm-cannot-take)) | Running vSphere VMs whose CPU count or memory is changed without CPU/memory hot-add (CPU hot-remove for fewer vCPUs) enabled on them, any memory decrease of a running VM, a memory grow above the VM's hot-plug limit, the growth of a disk on a shared SCSI bus or with multi-writer sharing, and any change of a suspended VM. Before, vCenter refused the `ReconfigVM_Task`, the VM showed `Reconfiguring=False/ProviderError` and the manager retried it indefinitely on its backoff, failing a vCenter task each time. Now the provider refuses it before changing anything (no vCenter task) with `RECONFIGURE_NEEDS_POWER_OFF`, shown as the new reason `Reconfiguring=False/ResizePendingPowerOff` with a constant message; the VM keeps its recorded size and stays `Ready`. Everything else in the same request waits too. A CPU count that is not a multiple of the VM's cores per socket is refused up front (`InvalidSpec`). | To apply the change, set `spec.powerState: Off` (the manager powers the VM off and the next Reconfigure applies it), then `On`; or enable hot-add on the VM while it is off. Alerting on `Reconfiguring` should treat `ResizePendingPowerOff` as action-needed. Roll the vSphere provider with the manager: an older provider image keeps the old refusal (`ProviderError`), an older manager shows the new one as `ProviderError`. |

The following are **not** flagged `Breaking change` in the CHANGELOG (no API/CRD
schema break, and existing bound VMs are unaffected), but change what a *new*
`Create` does and are easy to trip over mid-upgrade:

| Change | What changes | Operator action |
|---|---|---|
| **Every `/tmp` emptyDir now has a `sizeLimit`** (`internal/controller/provider_controller.go`, [`charts/virtrigaud/README.md`](../charts/virtrigaud/README.md#tmp-size-limits)) | The writable `/tmp` of every pod (the root filesystem is read-only) was an unbounded emptyDir. Now:<br>• provider pods run for `Provider` CRs: vSphere `512Gi`, libvirt, Proxmox and the other types `1Gi`;<br>• chart-templated providers: the same values;<br>• the manager: `128Mi`.<br>A pod that writes past its limit is **evicted** by the kubelet. The limit bounds the pod's `/tmp`; it does **not** reserve node storage, so a node with less free ephemeral storage than the limit (the vSphere default is `512Gi`) can still fill up first. Of the providers, only vSphere stages whole disks in its own `/tmp`: an OVA download (capped by `VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB`, default 256 GiB) and about **twice the disk** for a migration import or a converted/multi-extent export. libvirt and Proxmox stream disk data over SSH. The new pod template **rolls every manager-run provider pod once** when the manager is upgraded (a clustered libvirt provider, `Recreate`, is briefly down). | Before migrating a vSphere disk larger than about 256 GiB, raise its limit: annotate the `Provider` `infra.virtrigaud.io/tmp-size-limit: "<quantity>"` (or `"none"`), or set `manager.providerTmpSizeLimit` (the manager flag `--provider-tmp-size-limit`) for all of them. Size every limit to the free ephemeral storage of the nodes the pod can run on, or schedule vSphere providers on nodes that have it. To stop `Provider` authors from raising their own limit, set `manager.providerTmpSizeLimitMax` (`--provider-tmp-size-limit-max`): a larger annotation, or `"none"`, is then clamped to it with a `TmpSizeLimitClamped` Warning event. The webhook refuses an invalid annotation, and warns when `virtrigaud.io/force-delete` is set on a Provider that is not being deleted. Chart-templated providers use `providers.<name>.tmpSizeLimit`; the manager uses `manager.tmpSizeLimit`. An empty chart value (or `"none"`) removes the limit. Schedule the manager upgrade outside provider-heavy windows. |
| **Opt-in administrator disk-size limit** (`internal/controller/disk_size_limit.go`, `disk_size_limit_copies.go`, [`charts/virtrigaud/README.md`](../charts/virtrigaud/README.md#disk-size-limit)) | **Nothing changes unless it is set.** With `manager.maxDiskSize` (the manager's `--max-disk-size`) set, or a Provider annotated `infra.virtrigaud.io/max-disk-size` (which can only lower the manager's limit):<br>• a VM whose VMClass `spec.diskDefaults.size` (on libvirt, 20Gi when the class sets none) or any `spec.disks[].sizeGiB` is above the limit is not created (`Ready=False`, reason `DiskSizeExceedsLimit`, one Warning event), nor re-created if it vanished;<br>• a Reconfigure never grows a disk beyond the limit: the VM's other changes are applied and the disk is left as it is (`DiskSizeWithinLimit=False`, `DiskSizeExceedsLimit`);<br>• a `VMClone` or `VMMigration` is checked by the **real** sizes of the source's disks, read from its provider (`GetDiskInfo`), not by its VMClass or `spec.disks`: a clone for every disk before it is issued (it stays `Pending`); a migration in `Validating`, again right before the export (the size is recorded in `status.diskInfo.sourceSize`) and that record right before the import (it stays in its phase). **The target provider also enforces the limit itself** on the disk it is about to land (`ImportDiskRequest.max_virtual_size_bytes`), so a staged object replaced after the export, or a source provider that reports a wrong size, cannot land a larger disk; its refusal holds the migration in `Importing`. A size that cannot be read refuses it (`DiskSizeLimitUnverifiable`), as does a migration that reaches the import with no recorded size;<br>• VMs that already have larger disks keep running, stay `Ready` and are never shrunk.<br>**Not covered:** a primary disk larger than asked because its image or template is larger than the class size: vSphere and libvirt only grow an image's disk to the class size, Proxmox does not apply the class size at all, and no image size is known before the Create (VMImage `status.providerStatus[].size` is never filled).<br>An invalid Provider annotation fails closed (`DiskSizeLimitUnverifiable`): nothing is created, cloned, migrated or grown through that Provider. With `webhooks.enabled=true` the chart also renders the `vvirtualmachine-disksize.kb.io` and `vvmclass-disksize.kb.io` validating webhooks (only when `manager.maxDiskSize` is set), so VM and VMClass writes then depend on the webhook (`webhooks.validating.failurePolicy`). The kustomize `config/webhook/manifests.yaml` always lists them with `failurePolicy: Fail`. | Before setting it, list VMs and VMClasses above the intended limit: they keep running, but a clone or migration of them, a re-create, or growing their disks is refused. **Roll the provider images with the manager**: a provider image from before this release does not report every disk's size, so clones through it are refused while a limit applies, and it does not enforce the import-time limit (it does not report `supports_import_size_limit`), so while a limit applies a migration into it is held (`DiskSizeLimitUnverifiable`, "upgrade the provider image"). Keep `vmmigrations/status` (like `virtualmachines/status`) writable by the manager only: the pre-import check trusts `status.diskInfo.sourceSize`. A migration exported before the limit was set has no recorded size and is held before its import: delete it and migrate again. Use binary units (`500Gi`, not `500G`, which is about 465.7Gi); a value below `1Gi` or invalid stops the manager at startup and fails the chart render. Kustomize users who enable `config/webhook` without `--max-disk-size` should drop the two disk-size entries (or set them to `Ignore`). |
| **vSphere provider runtimes run one replica with the `Recreate` strategy** (`internal/controller/provider_controller.go`) | The vSphere provider serializes migration landing work (the import folder of a migrated disk) with a per-process lock. A second replica, or an old and a new pod overlapping in a rolling update, could race on the same landing folder. A vSphere `Provider`'s Deployment now always has one replica and the `Recreate` strategy, as a clustered libvirt `Provider` already does: `spec.runtime.replicas` above 1 is ignored (logged by the manager). The upgrade itself replaces each vSphere provider pod this way. **The vSphere provider is briefly unavailable during every rollout** (image change, spec change, manager upgrade): operations against it are retried by the controllers. The chart's opt-in vSphere Deployment also runs one replica with `Recreate`, and `providers.vsphere.replicaCount` above 1 fails the render. | Drop `spec.runtime.replicas` above 1 from vSphere `Provider`s (it has no effect). Expect a short vSphere provider outage on each rollout and schedule rollouts accordingly. |
| **New libvirt domains are named `<namespace>.<name>`** ([#339](https://github.com/projectbeskar/virtrigaud/pull/339), [`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md)) | Only *new* domains get the namespaced host name; existing VMs keep their name and are addressed by `status.id` as always. | **Upgrade the manager before the libvirt provider** (see "Required upgrade order"). External tooling that assumes "domain name == VirtualMachine name" must read `status.id` instead. Remove leftover `/tmp/virtrigaud-cloudinit/<name>/` seed directories after upgrading (a pre-#331 failed create never cleaned these up on the host). |
| **libvirt Create fails closed on a same-named domain it doesn't own** ([#333](https://github.com/projectbeskar/virtrigaud/pull/333), [`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md)) | A `VirtualMachine` create that collides with a domain that predates this change (no owner stamp) now fails with `Ready=False/ProviderConflict` instead of silently taking the domain over. | Adopt the domain (`virtrigaud.io/adopt-vms`) or rename the VirtualMachine to resolve a conflict. |
| **libvirt SSH commands are shell-quoted; guest-agent calls now actually work** ([#331](https://github.com/projectbeskar/virtrigaud/pull/331)) | Guest-agent-backed fields (Describe IPs, `SyncGuestTime`, best-effort in-guest filesystem grow after an online disk expand) were silently broken over SSH before this fix and now run for real, within new time/size bounds. | Confirm the SSH login shell for the provider's hypervisor account is POSIX-compatible (`sh`, `bash`, `dash`, `zsh`, `ash`) — the quoting relies on POSIX single-quote semantics. |
| **`VMImage` prepare state is keyed by Provider `<namespace>/<name>`** ([`docs/image-preparation.md`](image-preparation.md#prepare-state-is-per-provider)) | `status.providerStatus` and `status.availableOn` use `<namespace>/<name>` instead of the bare Provider name; each entry records the Provider's UID and its own `taskRef`; the image-wide `status.prepareTaskRef` is no longer written. On the first VM create that uses each image, a bare-name entry is moved to the image's own namespace when a Provider of that name exists there (and re-validated once through the idempotent prepare), otherwise dropped; a legacy `prepareTaskRef` is cleared. An image is now prepared only before a create or re-create, never for a VM that exists. | Upgrade the `VMImage` CRD **before** the manager: until it has `providerStatus[].providerUID`/`taskRef`, readiness fails and VM creates that need an image prepare are held. Update scripts that read `providerStatus`/`availableOn`. Under `onMissing: Fail`/`Wait`, an entry without `providerUID` holds creates (reason `ProviderUIDMissing`): switch `onMissing` to `Import` to re-validate it. Never write `vmimages/status` by hand; the VirtualMachine controller is its single writer (ADR-0005). Expect one `ImagePrepare` call per image and Provider on the first create after the upgrade (a no-op when the prepared image exists). |
| **Image prepares carry the `VMImage`'s identity; providers without artifact identity are held** ([ADR-0009](adr/0009-prepared-image-artifact-identity.md), [`docs/image-preparation.md`](image-preparation.md#prepared-image-artifact-identity)) | The manager sends every `ImagePrepare` with the `VMImage`'s UID, a digest of its `spec.source` and an empty target name, and records a prepare only when the provider's answer confirms that identity. A Provider that imports images but does not report `status.reportedCapabilities.supportsImageArtifactIdentity` gets no prepare: creates of import-style images (`ovaURL`, libvirt `url`, `http`) through it are held (`ProviderLacksArtifactIdentity`) until the provider is upgraded. `status.providerStatus[].sourceDigest` is recorded, and an entry is used only for the `spec.source` it was prepared for, so the first create for each image and location after the upgrade prepares the image once more, under its new artifact name. An artifact at that name that the provider cannot prove was prepared for the image holds creates (`ArtifactConflict`, `Warning` event `ImageArtifactConflict`, metric `virtrigaud_image_prepare_artifact_total{outcome="conflict"}`). | Roll the providers right after the manager (step 4): until they report the capability, new VMs from import-style images wait. Expect one re-import per image and image location; the bare-named artifacts of the earlier release are left on the hypervisor (remove them yourself once nothing references them). Under `onMissing: Fail`/`Wait`, an entry without a digest holds creates (`SourceDigestMissing`): switch `onMissing` to `Import`. Alert on `virtrigaud_image_prepare_artifact_total{outcome="conflict"}`. Reference-style images and VMs that exist are unaffected, but a VM re-created because it vanished from its hypervisor is a create and waits too. |
| **libvirt: prepared images are named, stamped and published by identity** (ADR-0009 Slice 4, [`docs/image-preparation.md`](image-preparation.md#libvirt-prepared-images-sourcelibvirturl)) | The libvirt provider reports `supportsImageArtifactIdentity` (single-host Providers; a clustered Provider still does not prepare images). A URL-sourced libvirt `VMImage` is prepared as `<pool dir>/<namespace>.<name>_<16 hex>.qcow2` with a stamp file `.<same>.virtrigaud-image.json`. The prepared file is **read-only (`0444`) and owned by the provider's SSH user** — no longer `chown libvirt-qemu:kvm` / `chmod 777`; each VM's own copy is unchanged. The pool directory must be an allowed image directory (`VIRTRIGAUD_LIBVIRT_IMAGE_DIRS`) on a filesystem with hard links, or the prepare fails with `InvalidSource`. Downloads are one transfer (no URL globbing), limited in time (`spec.prepare.timeout`) and size (`VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB`, default 256). A source that fails for good (HTTP 4xx, checksum mismatch, unreadable image) now shows `InvalidSource` instead of being retried every few seconds. | Roll the libvirt providers with the other providers (step 4). Providers that share a pool directory must use the same SSH user (another user's artifact is a Conflict). If `restorecon` must run (SELinux hosts), the SSH user needs passwordless `sudo` for it. Raise `VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB` if an image is larger than 256 GiB. Alert on `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="libvirt"}`: non-zero means a manager older than this release is still preparing images by bare name. |
| **Proxmox: URL image import (`source.http`) is refused** (ADR-0009 D10, [`docs/image-preparation.md`](image-preparation.md#proxmox-image-sources)) | It never worked: the import only downloaded the file and never created a template VM that `Create` could clone, and it treated any same-named template as "already prepared". `ImagePrepare` now returns `InvalidSpec` for it, with or without an identity and before any PVE call, so the `VMImage` shows `Ready=False` / `InvalidSource` and new VMs from it are not created. VMs that already exist are unaffected. `source.proxmox.templateID`/`templateName` are unchanged. The Proxmox provider now advertises `supportsImageArtifactIdentity` and serves `/metrics` on its health port. | Prepare a template on Proxmox VE (`qm importdisk` + `qm template`) and point the `VMImage` at it with `source.proxmox.templateID`. Real URL import comes with ADR-0009 Slice 6. After the upgrade, `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="proxmox"}` must stay at 0: a non-zero value means a manager older than this release is still sending requests (they are refused and counted). |
| **vSphere: prepared templates are named, stamped and kept in the import folder** (ADR-0009 Slice 3, [`docs/image-preparation.md`](image-preparation.md#vsphere-identity-safe-prepared-templates)) | An `ovaURL` image is imported as a template named `<namespace>.<name>_<16 hex>`, stamped with `virtrigaud.image.*` ExtraConfig, into the Provider's import folder (`spec.defaults.folder`, or the datacenter VM folder when unset), and reused only when its stamp matches; `status.providerStatus[].id` becomes the template's absolute inventory path. A configured `defaults.folder` that does not exist or is ambiguous now fails image prepares (retried) instead of falling back to the datacenter VM folder. Permanent import failures (a 404 source, a checksum mismatch, an invalid or multi-VM OVF) are `InvalidSpec` and hold the image. The provider now serves `/metrics` on its health port. | Check that each vSphere Provider's `defaults.folder` resolves to exactly one VM folder; for multi-tenant setups give each tenant's Provider its own import folder. Make sure nobody but the Provider's vCenter account can create, move, rename, reconfigure (`VirtualMachine.Config.AdvancedConfig`) or mark as template VMs in that folder — folder permissions propagate, and such a principal can plant a matching template before the first prepare. Expect one re-import per image and folder; templates named after the bare `VMImage` are left in place (they have no `virtrigaud.image.uid`). Alert on `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="vsphere"}` > 0. |
| **vSphere: OVF packages are read from the download only; bare `.ovf` sources refused** (security fix, [`docs/image-preparation.md`](image-preparation.md#vsphere-identity-safe-prepared-templates)) | Every OVF `<File ovf:href>` must be a plain file name that is a root-level member of the OVA; anything else (a path, `..`, a URL, a glob) is `InvalidSpec` before vCenter sees the descriptor. This closes a pre-existing local file read: a bare `.ovf` could name any file the provider pod can read (its vCenter credentials, its TLS key) and have it uploaded into the template. A bare `.ovf` `ovaURL` is refused in identity mode; from an older manager (legacy mode) a bare `.ovf` that references any file is refused, one that references none still imports. OVA members inside a subdirectory are no longer found. | Republish images served as a bare `.ovf` (with disks next to it) as an `.ova`. Check that your OVAs keep their descriptor and disks at the archive root (a leading `./` is fine). |
| **vSphere: image downloads are restricted and bounded** ([`docs/image-preparation.md`](image-preparation.md#vsphere-identity-safe-prepared-templates)) | The provider refuses image sources at, or redirecting to, loopback, link-local (including `169.254.169.254`), unspecified and multicast addresses (RFC 1918 stays allowed), follows at most 3 redirects, bounds connect/TLS/response-header waits, and downloads at most `VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB` GiB (new env var, default 256, 1–16384; invalid values log a warning and keep the default). Such refusals are `InvalidSpec`. Image-source failures (the source unreachable or failing, a download that cannot be staged, vCenter refusing the image's content) are tagged `IMAGE_SOURCE_UNAVAILABLE` and no longer count toward the Provider's circuit breaker (needs this manager too). Status messages no longer quote HTTP codes, transport errors or parser output. | Move images served from loopback or link-local addresses to a reachable image server. For images above 256 GiB, set `VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB` through the Provider's `spec.runtime.env`. Image downloads ignore `HTTP_PROXY`/`HTTPS_PROXY` (see the proxy row below). Look in the provider log for the details a status no longer shows. |
| **vSphere: more image-source addresses refused; no `https` → `http` redirect** ([`docs/image-preparation.md`](image-preparation.md#vsphere-identity-safe-prepared-templates)) | Also refused, on every connection after DNS: `fd00:ec2::254`, `100.100.100.200`, `168.63.129.16`, and IPv6 addresses embedding a refused IPv4 address (NAT64 `64:ff9b::/96`, IPv4-compatible `::/96`). A redirect from `https` to `http` is refused. All are `InvalidSpec`. | Serve images over `https` end to end, or over `http` from the start. |
| **vSphere: image downloads ignore `HTTP_PROXY`/`HTTPS_PROXY`; new opt-in `VIRTRIGAUD_VSPHERE_IMAGE_PROXY`** ([`docs/image-preparation.md`](image-preparation.md#vsphere-identity-safe-prepared-templates)) | OVA downloads connect directly even when the provider pod has `HTTP_PROXY`/`HTTPS_PROXY` set (the vCenter connection still honours them): a proxy resolves targets itself, which bypassed the address checks. `VIRTRIGAUD_VSPHERE_IMAGE_PROXY` (an `http://` or `https://` URL with a host) sends image downloads through that proxy; an invalid value logs a warning and downloads connect directly. The proxy's own address must not be loopback or link-local. | If image servers are reachable only through a proxy, set `VIRTRIGAUD_VSPHERE_IMAGE_PROXY` through the Provider's `spec.runtime.env` **before** rolling the provider, and restrict that proxy's egress to your image servers. |
| **vSphere: OVF descriptor at most 16 MiB and at most 256 file references** ([`docs/image-preparation.md`](image-preparation.md#vsphere-identity-safe-prepared-templates)) | A larger descriptor is refused from its tar header, before it is read; a descriptor with more `<File>` references is refused before vCenter sees it. Both are `InvalidSpec`. Real OVFs are a few KiB with a handful of references. | None expected. An image refused for this is not a valid single-VM OVA. |
| **vSphere: image downloads must keep moving; prepares end before the manager's deadline** ([`docs/image-preparation.md`](image-preparation.md#vsphere-identity-safe-prepared-templates)) | A download that delivers less than 64 KiB in any 60 seconds is abandoned, and a prepare gives up 15 seconds before the manager's deadline for the call. Both are `IMAGE_SOURCE_UNAVAILABLE` (retried, not counted toward the circuit breaker). The manager no longer counts its own deadline or cancellation of an `ImagePrepare` toward the breaker, and an NFC upload network error counts only when vCenter also fails to answer. | None. A very slow image server now shows a retried prepare instead of a hung one; serve large images from a faster server. |
| **vSphere: adoption never takes templates, prepared images or VMs another `VirtualMachine` owns** (security fix, [`docs/vm-ownership.md`](vm-ownership.md#adoption-what-listvms-offers)) | The vSphere `ListVMs`, which `virtrigaud.io/adopt-vms` adopts from, listed every object in the datacenter, templates included. Adoption could turn a prepared image (ADR-0009) or any other template into a tenant's `VirtualMachine`, and deleting or resizing that `VirtualMachine` destroyed or changed the shared image for every consumer. `ListVMs` now leaves out every template, every object carrying a `virtrigaud.image.*` key (a legacy OVA import now carries one, `virtrigaud.image.preparedat`, from the moment it exists), and every VM whose owner stamp or configuration cannot be read. It also reports each VM's owner stamp UID, so adoption skips a VM that a live `VirtualMachine` owns under another Provider object for the same vCenter. Existing `VirtualMachine`s are not changed. | Before upgrading, run two checks. **1. A `VirtualMachine` on a template.** List every `VirtualMachine`'s `status.id`, adopted or not: `kubectl get virtualmachine -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PROVIDER:.spec.providerRef.name,ID:.status.id`. List the templates in vCenter without the `VirtualMachine:` prefix govc prints: `govc find -i / -type m -config.template true \| sed 's/^VirtualMachine://'`. Any `status.id` in that list is a `VirtualMachine` on a template. **2. A duplicate adoption.** A `VirtualMachine` adopted before the upgrade can duplicate one under another Provider object for the same vCenter (both have the same `status.id`): `kubectl get virtualmachine -A -o jsonpath='{range .items[*]}{.status.id}{"\n"}{end}' \| sort \| uniq -d` prints each such ID; the duplicate is the one labelled `virtrigaud.io/adopted=true`. For every match of either check, set `virtrigaud.io/orphan-on-delete: "true"` on the `VirtualMachine` (for a duplicate, the adopted one) and delete it. Never delete it plainly: that destroys the template, or the VM the other `VirtualMachine` manages. After the upgrade a plain delete of a `VirtualMachine` on a template is refused (next row). |
| **vSphere: per-VM operations refuse templates and image artifacts** (security fix, [`docs/vm-ownership.md`](vm-ownership.md#per-vm-operations-refuse-templates-and-image-artifacts)) | `Delete`, `Power`, `Reconfigure`, `HardwareUpgrade`, `SnapshotCreate`, `SnapshotDelete` and `SnapshotRevert` read `config.template` and `config.extraConfig` first, and refuse a template or an object with a `virtrigaud.image.*` key before changing anything (`FailedPrecondition` + `VM_IS_IMAGE_ARTIFACT`, a constant message; also refused, with a plain `FailedPrecondition`, when the configuration cannot be read). The manager maps the refusal to a non-retryable `Conflict` and never counts it toward the circuit breaker. A refused `Delete` keeps the finalizer: the `VirtualMachine` stays `Terminating` and is retried every 15 seconds (manager log: `Failed to delete VM from provider`). Before, `Delete` destroyed the template. One extra property read per guarded call. A full `Clone` is not guarded; a linked `Clone` that would first snapshot a template or image artifact (`clone-base-*`) is refused the same way. `Create` and `Clone` now also read the source's `config.extraConfig` and clear every `virtrigaud.image.*` key it carries (any case, including keys this release does not write), so a VM made from a template is never itself refused; a failed read fails the create or clone before anything is made. | Roll the vSphere provider. Detach any `VirtualMachine` on a template with `virtrigaud.io/orphan-on-delete: "true"` before deleting it (previous row). |
| **Adoption never adopts a VM whose owner stamp names no `VirtualMachine` in this cluster; new opt-in `virtrigaud.io/adopt-stamped-vms`** (security fix, behaviour change, [`docs/vm-ownership.md`](vm-ownership.md#owner-stamps-are-scoped-to-one-cluster)) | Every provider that reports `owner_uid` (libvirt, vSphere). An owner stamp only means something in the cluster that wrote it. Single-host adoption used to adopt a VM whose stamped UID belonged to no `VirtualMachine` in this cluster, treating it as stale; that UID may be a live `VirtualMachine` of **another VirtRigaud installation**, or of this cluster before a restore, managing the same vCenter or host. Such a VM is now logged (`Not adopting a VM whose owner stamp names no VirtualMachine in this cluster`), counted in the Provider's adoption status message (`N not adopted — owner stamp names no VirtualMachine in this cluster`), and left alone. Clustered adoption already never adopted a stamped VM and is unchanged. **This includes re-adopting a VM you detached with `orphan-on-delete`:** the VM keeps the stamp of the deleted `VirtualMachine`, so deleting a `VirtualMachine` with `virtrigaud.io/orphan-on-delete: "true"` and letting adoption pick the VM up again now needs `virtrigaud.io/adopt-stamped-vms: "true"` on the single-host Provider. An adopted VM's `status.provider` no longer carries `owner_uid` or `owner_stamp_state` (single-host now strips them as clustered adoption does). | If you relied on adopting VMs left behind by `orphan-on-delete`, annotate the single-host Provider with `virtrigaud.io/adopt-stamped-vms: "true"` (only `"true"` counts) while no other VirtRigaud installation manages the same vCenter or host, and remove it afterwards. Takes effect at the next discovery after the manager is rolled. |
| **Adoption never adopts a VM whose owner stamp cannot be read, or that is marked as an artifact** ([`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md#adoption), [`docs/vm-ownership.md`](vm-ownership.md#adoption-what-listvms-offers)) | The single-host libvirt `ListVMs` (virsh and go-libvirt) now reports `provider_raw["owner_stamp_state"]` (`unreadable`, `multiple`) as the clustered one does, instead of only the readable UIDs; single-host adoption skips any VM with that key, as clustered adoption already did. Both flows also skip any VM carrying `provider_raw["artifact"]` (any value), which a third-party provider must set on a template or image artifact it lists, and never copy that key into status. The go-libvirt list shadow comparison does not compare either key, so the ADR-0008 D5 soak is unaffected. | Roll the libvirt provider and the manager. A hand-edited domain with two owner stamps, or a stamp without a UID, is no longer adopted: fix its `<metadata>` or remove the domain. |
| **vSphere: Create leaves the new VM powered off; the VirtualMachine controller powers it on** (`internal/providers/vsphere/server.go`) | The vSphere provider's Create used to power on every VM it cloned from a template, whatever its `spec.powerState`, so a VirtualMachine created with `powerState: Off` booted until the controller hard-powered it off. Create now leaves the VM off, like the libvirt and Proxmox providers. The controller powers it on in its next reconcile (a few seconds later) when `spec.powerState` is `On` or empty. Cloud-init and `guestinfo.network.*` keys are set at create time, so the guest still reads them on its first boot. A migrated VM (created from an imported disk) is now also grown to its VMClass disk size and gets its additional disks: a bug skipped both steps on that path. The grow is grow-only: a migrated VM **smaller than the VMClass `diskDefaults.size` (default `40Gi`)** is grown to it, and one already at or above it is left alone. Growing uses datastore capacity (all of it at once for a thick disk). A GPT guest sees its backup partition header no longer at the end of the disk; `growpart`/cloud-init repairs it on boot (or run `sgdisk -e`). A failed grow or additional-disk attach now fails the create (retried; the VM is kept and the next attempt completes the missing step without repeating the others) instead of being logged and skipped. Additional disks are named `<vm>-additional-<N>.vmdk` in the VM's own directory. | Check the VMClass `diskDefaults.size` of migration targets: set it to the source disk size (or less) to keep a migrated disk at its size. Make sure the datastore has room for the growth. Expect a VM to show `status.powerState: Off` for one reconcile after it is created. A `VMMigration` or `VMClone` target with `powerOn: false` on vSphere is never booted. |
| **libvirt: image-source download failures no longer count toward the circuit breaker** ([`docs/image-preparation.md`](image-preparation.md#which-failures-are-retried)) | A retried download failure the source caused (HTTP 5xx/408/425/429; DNS, connect, timeout, TLS-handshake, send/receive, truncated body) is answered `Unavailable` with `IMAGE_SOURCE_UNAVAILABLE` instead of a plain error, with the same text. The SSH transport or the host failing still counts. | Roll the libvirt provider with this manager. |
| **libvirt: source VMs with linked clones can no longer be deleted, reverted or snapshotted until their clones are gone; `VMSnapshot` deletes are retried instead of dropped** (security fix, data loss; [`docs/libvirt-clones.md`](libvirt-clones.md)) | Deleting a running linked clone deleted its **source VM's disk** (Delete removed every file the definition listed, including the clone's backing file). Delete now reads the definition once (an unreadable one fails the delete, retryably, with the domain intact) and removes only: the domain's own top-level disk files that are regular files directly inside the default pool directory or an allowed image directory — **never a symbolic link or its target**; the files of its **own external-snapshot chain** below them (its snapshot overlays `<domain>-disk.<snapshot>` down to the disk it was created with, same rules, used by no other domain); and its cloud-init seed directory (kept while a clone's CD-ROM still references it). Backing files it does not own, cdrom/floppy media and files elsewhere are left in place and logged. A deleted VM whose domain is already gone only has `<pool dir>/<name>-disk.qcow2` cleaned up (no more name-pattern matching). While another domain on the host uses a VM's disk — its linked clone (in any namespace), or a shared disk — the provider refuses, before changing anything, to delete the VM (`Ready=False/DeleteBlocked`, finalizer kept, re-checked every minute, `Warning` event `DeleteBlocked`) and to create, revert or delete its snapshots (`FailedPrecondition` + `VM_DISK_IN_USE`, not counted toward the circuit breaker). The check reads every domain on the host and its disk chain (a running domain's from its definition, a shut-off one's one image at a time with `sudo -n qemu-img info -U` when allowed, following only local regular backing files); when it cannot, the operation is not performed and returns `Unavailable` + `VM_DISK_CHECK_FAILED` (retried, not counted toward the circuit breaker) — or, when the host cannot be reached at all, a host failure (`HOST_UNAVAILABLE` on a clustered Provider, a counted `Unavailable` on a single-host one). A `VMSnapshot` whose provider delete fails, is refused, or cannot be sent while its VM exists (Provider no longer granted, VM unbound, `providerRef` mismatch) now **keeps its finalizer** (`Ready=False`/`Deleting=False`, reason `DeleteBlocked`, `ProviderError` or the reason no call can be made, retried with backoff) instead of being removed with the snapshot left on the host; only a snapshot the provider reports gone, a `VirtualMachine` that no longer exists, or `virtrigaud.io/force-delete: "true"` (`Warning` event `SnapshotLeftOnHypervisor` naming the snapshot), releases it. Powering on the source of an existing linked clone is not refused, but sets `LinkedClonesDependOnDisk=True` and a `Warning` event on its `VirtualMachine`. Every VM disk is now created `0640` and `chown -h`'ed to `libvirt-qemu:kvm` — no root `chmod` that could follow a swapped-in symlink (was world-writable `0777`; see "New required privileges and config"); a UEFI clone's varstore is refused when its path is a symlink or in use, and is created fresh (`conv=excl`, `0600`) without following symlinks. | Delete a source VM's linked clones before the source (or detach it with `virtrigaud.io/orphan-on-delete: "true"`); a linked clone in another namespace holds the source's delete too. Keep those sources powered off. Make sure the provider's SSH user can read every VM disk on the host — `root`, a member of the `kvm` group, or allowed passwordless `sudo qemu-img info -U` (only that) — or the checks fail (retryably) and Delete and snapshot operations do not proceed. Roll the manager with the libvirt provider, so a refused delete shows `DeleteBlocked` and a failed check is kept out of the circuit breaker. Expect `VMSnapshot`s stuck deleting where the provider cannot delete the snapshot: fix the cause, or set `force-delete`. If disk usage grows, look for `Not deleting disk` in the provider log. |
| **Deleting a `VirtualMachine` while its `Provider` cannot be reached now waits instead of orphaning the hypervisor VM** ([`docs/vm-provider-binding.md`](vm-provider-binding.md#deleting-a-vm-while-its-provider-cannot-be-reached)) | Every `VirtualMachine` with a provider id (or a clustered create pending), on every provider type. Before, when the manager had no usable client for the VM's `Provider` — its provider runtime not `Running` or without an endpoint (during every provider rollout or restart; a clustered libvirt provider runs one replica with the `Recreate` strategy), the provider pod down, a TLS or NetworkPolicy misconfiguration, a failed `Validate` — deleting the VM logged an error and **released the finalizer without any provider `Delete`**: the `VirtualMachine` disappeared while its hypervisor VM kept running, unmanaged (on a clustered Provider it later holds a same-named VM as a previous incarnation). Now the finalizer is kept and the delete is retried with a backoff (15 s doubling to 5 min — from the deletion request for a single-host VM, from when the hold began for a clustered one): a single-host VM shows `Ready=False/ProviderUnavailable`, a clustered VM `DeleteBlocked=True/ProviderUnavailable` and `Ready=False/DeleteBlocked`, both with a constant message and one `Warning` event `DeleteBlocked` when the hold begins. The cause is in the manager log — search for `Cannot reach the provider to delete the VM`, the same line for single-host and clustered VMs — and counted under `virtrigaud_errors_total{reason="provider-resolve"}`. If the Provider then answers but its `Delete` fails for another reason, a held VM's `Ready` becomes `False/ProviderError` (and a clustered VM's `DeleteBlocked` is removed) instead of keeping the old hold's reason. The delete completes on the first retry after the Provider is reachable again. A `Provider` that no longer exists in the VM's own namespace still releases the finalizer, as before. | Expect `VirtualMachine`s to stay `Terminating` while their `Provider` is down or rolling out; check the Provider (`kubectl get provider <name> -o jsonpath='{.status.runtime}'`) and the manager log, and fix it. To release a VM anyway, set `virtrigaud.io/orphan-on-delete: "true"` (detach on purpose; no provider call; on a clustered Provider in another namespace it needs the Provider's permission) or `virtrigaud.io/force-delete: "true"` (drops the finalizer, logged as possibly orphaning, with a `Warning` event `VMLeftOnHypervisor` on the VM — recorded for every force-delete that leaves a hypervisor VM behind) — either way the hypervisor VM and its disks are left for manual removal. Automation that deletes VMs during a provider outage and waits for them to disappear needs a longer timeout. Takes effect when the manager is rolled. |
| **Deleting a `Provider` that is still in use now waits; its runtime is kept until nothing uses it** ([ADR-0010](adr/0010-provider-in-use-finalizer.md), [`docs/vm-provider-binding.md`](vm-provider-binding.md#deleting-a-provider)) | Every `Provider`. The manager adds the finalizer `provider.infra.virtrigaud.io/in-use` to every `Provider` when it is rolled. Before, deleting a `Provider` — directly, or by deleting its namespace — removed it at once, and every VM using it was then released on its next delete **without any provider `Delete`**: its hypervisor VM kept running, unmanaged. Now a `Provider` being deleted stays `Terminating` while any `VirtualMachine` (in any namespace, including cross-namespace consumers), `VMSnapshot`, in-flight `VMClone` or `VMMigration` still needs it, and its runtime (Deployment, Service, ServiceAccount) is kept running so they can still be deleted through it. It shows `DeleteBlocked=True/ProviderInUse`, with the number of objects per kind and per namespace (no object names), and one `Warning` event `ProviderDeleteBlocked` when the hold begins. It is released, and its runtime removed, as soon as the last one goes. **Namespace deletion:** Kubernetes deletes the provider runtime's pods with the namespace, so VMs whose delete does not reach the provider while its pod is terminating are held (`ProviderUnavailable`), not orphaned, and the `Provider` shows `DeleteBlocked=True/ProviderInUseRuntimeNotKept`; the namespace then stays `Terminating` until an administrator acts. The same holds for a `Provider` deleted with `--cascade=foreground`. | Delete VMs (and wait for them to go) **before** deleting their `Provider` or its namespace. Automation that deletes a `Provider` and waits for it to disappear needs to delete its VMs first, or a longer timeout. VMs in **other** namespaces are not deleted with the `Provider`'s namespace: delete them or detach them (`virtrigaud.io/orphan-on-delete: "true"`). To release a held `Provider` at once, annotate it `virtrigaud.io/force-delete: "true"`: its runtime is removed and a `Warning` event `ProviderForceDeleted` records how many objects were left; VMs in its namespace are then released on their next delete without a provider call, and VMs in other namespaces stay held until annotated `orphan-on-delete` or `force-delete` — their hypervisor VMs are left unmanaged. Uninstall order: delete VMs and `Provider`s before removing the manager, or they stay `Terminating`. Takes effect when the manager is rolled; no CRD change. |
| **Deleting a `VMMigration` now waits until the source snapshot it created is deleted; a `VMSnapshot` delete waits while its VM's `Provider` cannot be reached** | Every `VMMigration` that created a source snapshot (`spec.source.createSnapshot: true`, no `spec.source.snapshotRef`, `cleanupPolicy` not `Never`), on every provider type. Before, deleting such a migration released its finalizer even when the snapshot `Delete` failed or the source `Provider` could not be resolved, leaving the snapshot untracked on the **live source VM** (on libvirt its metadata then blocks deleting the source VM). Now the finalizer is kept while the snapshot may still exist and a retry can still delete it: `SnapshotCleanup=False/SnapshotCleanupPending` with a constant message, one `Warning` event `SnapshotCleanupPending` when the hold begins, retried with a backoff (15 s doubling to 5 min). A snapshot the provider no longer finds releases it (the vSphere and Proxmox providers now answer a provably missing snapshot with gRPC `NotFound` — Proxmox on the VM's actual node, so a VM moved by HA keeps its snapshot held, and a VM hidden from the token by its ACL is never taken for absent; roll them with the manager). When nothing can address the snapshot any more — the source VM, or its `Provider` in the migration's own namespace, is gone — a deletion is released with a `Warning` **`SnapshotLeftOnHypervisor`** naming it (never reported deleted). A migration that **failed for good** (no `retryPolicy`, or its retries used up) now deletes its source snapshot right away, held and retried the same way; before, that cleanup never ran. Separately, a `VMSnapshot` whose VM's `Provider` exists but cannot be read or reached is now held with `Ready=False` / `Deleting=False` reason `ProviderUnavailable` (constant message) and the same backoff, instead of being retried every 30 s with no status, and `force-delete` now releases it; one whose `Provider` no longer exists in its own namespace is released with a `SnapshotLeftOnHypervisor` Warning (it used to be held). | Nothing to change. A `VMMigration` or `VMSnapshot` that stays in deletion is waiting for its provider: the cause is in the manager log (search for `Cannot delete the migration's source snapshot yet` or `Cannot reach the VM's provider to delete the snapshot`). To remove it anyway, leaving the snapshot on the hypervisor, set `virtrigaud.io/force-delete: "true"` on the `VMMigration` (or `VMSnapshot`); a `Warning` event **`SnapshotLeftOnHypervisor`** names the snapshot left behind. |
| **`nfs` / `s3` migrations delete their staged disk; deleting such a `VMMigration` waits until it is deleted; new NFS staging name; `Ready` cleanup only for migrations this release completes** ([ADR-0011](adr/0011-migration-staging-cleanup.md)) | Every `VMMigration` with `spec.storage.type: nfs` or `s3`, and every `Ready` `VMMigration`. The staged object (a full, unencrypted copy of the source disk) was never deleted. The manager now asks the migration's **target** `Provider` to delete it (`DeleteStagedDisk`) when the migration is `Ready`, when the `VMMigration` is deleted, and when the migration fails for good with `cleanupPolicy: Always`. With the default `OnSuccess`, a failed migration's staged disk is kept until the `VMMigration` is deleted. `Never` keeps it in every case (`StagingCleanup=True`, reason `StagedDiskRetained`). A delete that fails is held: `StagingCleanup=False`, reason `StagingCleanupPending`, one Warning event, retried from 15 s doubling to 5 min. A deleted `VMMigration` keeps its finalizer meanwhile (also while an export task still runs), and a `Ready` or `Failed` migration with a staged disk still to delete keeps its target `Provider` from being deleted (ADR-0010). A target provider image older than this release cannot delete: the delete is **held for up to 24 hours** (roll the providers), then recorded as `StagedDiskLeftBehind` (Warning). A spec whose `spec.storage` or `spec.target.providerRef` was changed after the export is never deleted at the new location (`StagedDiskLeftBehind`). libvirt and vSphere targets empty an `nfs` object (0 bytes; an empty file remains), and Proxmox deletes it. **New NFS staging name:** new migrations stage `vmmigrations_<namespace>_<name>_export.qcow2` (`_` separators; the old `vmmigrations-<namespace>-<name>-export.qcow2` was shared by, e.g., `team-prod/db` and `team/prod-db`); migrations staged before the upgrade keep the old name for import and delete (`status.storageInfo.stagingLayout`). **`Ready` cleanup was broken for every migration with `spec.storage`** (a manager-side PVC cleanup that could not work failed first, so the source snapshot was never deleted and `deleteAfterMigration` never ran). It now runs **only for migrations that become `Ready` under this release** (condition `PostMigrationCleanup`). A migration completed before the upgrade deletes nothing: it is closed with one `SourceDeletionSkippedPreUpgrade` Warning (when a snapshot or source VM would have been deleted). `deleteAfterMigration` deletes only the source VirtualMachine the migration validated (UID recorded in `status.sourceVMUID`, UID precondition); a re-created one is kept (`SourceDeletionRefused` Warning). **Imports check the staged image** (security fix): NFS and S3 imports on Proxmox, vSphere and libvirt copy the staged object once to a local file, parse its header without qemu and then with qemu-img, refuse an image that references any other file (backing file, data file, extent) or is not the expected format (`InvalidArgument`), and import that same copy. NFS copies and S3 downloads are bounded: an object (or a declared virtual size) above `VIRTRIGAUD_MIGRATION_IMPORT_MAX_GIB` (provider env, default 1024) is refused (`InvalidArgument`); one that does not fit twice in the free space of the Proxmox stage directory (`/var/lib/virtrigaud/import-stage`), the libvirt pool directory or the vSphere pod `/tmp` (measured against its emptyDir `sizeLimit`, passed as `VIRTRIGAUD_TMP_SIZE_LIMIT`) is refused (`ResourceExhausted`, retried); a copy longer than the size measured before it is refused. Libvirt hosts and vSphere pods need `qemu-img dd`. A VMDK whose grain directory is at the end of the file (a VMware-native streamOptimized export) is refused; VirtRigaud's own vSphere export is not affected. The manager-run vSphere provider Deployment gains the `VIRTRIGAUD_TMP_SIZE_LIMIT` variable (one more rollout of its pod). Proxmox mounts the export `nosymfollow,nosuid,nodev,noexec` (PVE 7 or later). On Proxmox, `Create` checks the stage's header again before `qm importdisk` (a refused stage is removed, nothing is created). The staged-disk delete also removes the node-side import stage in `/var/lib/virtrigaud/import-stage`, but only for the VM the import was sent for (`status.storageInfo.importTarget`, never an edited `spec.target`), and not while another in-flight migration targets that VM. | Roll the providers with the manager (step 4) so the targets support `DeleteStagedDisk` within the 24-hour window. The S3 credentials need `s3:GetObject` (for the stat) and `s3:DeleteObject` on the `vmmigrations/` prefix. On NFS, the migration's `uid`/`gid` needs write access to the staged file (libvirt, vSphere) or to the export directory (Proxmox). **Versioned or object-lock buckets** keep the data after the delete (a delete marker): add a lifecycle rule that expires noncurrent versions on `vmmigrations/`, or use an unversioned staging bucket. Exclude the NFS staging export from **server-side snapshots**. Set `cleanupPolicy: Never` on migrations whose staged disk must be kept. If a target provider can no longer be reached, `virtrigaud.io/force-delete=true` on the `VMMigration` releases it, and the staged disk is left behind. For `Ready` migrations completed before the upgrade, remove their source snapshots and (with `deleteAfterMigration`) their source VMs yourself if still wanted. **Never grant tenants `update`/`patch` on `vmmigrations/status`** (check custom or aggregated roles): the controller trusts the markers it writes there (`PostMigrationCleanup`, `StagingCleanup`, `sourceVMUID`, `storageInfo.stagingLayout`/`stagingDigest`) to decide what to delete. Remove yourself the objects staged before the upgrade by migrations that no longer exist, the objects recorded as `StagedDiskLeftBehind`, and the empty NFS files. A staged image an import now refuses must be re-exported (self-contained qcow2). Set a disk-size limit (`--max-disk-size` or the Provider annotation) on target Providers whose hosts or pools are shared: the import's free-space check covers twice the staged file size, and a compressed staged image can expand up to its declared virtual size when it is converted. |
| **libvirt cloud-init seeds are built private in a dedicated seed directory, which must exist on every host** (R27, `internal/providers/libvirt/seed_dir.go`, [`docs/libvirt-domain-ownership.md`](libvirt-domain-ownership.md#cloud-init-seed-directory)) | Every libvirt Provider, single-host and clustered. In v0.3.11 a VM's seed (its user-data, which may carry passwords, keys and tokens) was built under the world-listable `/tmp` with the ISO at the SSH user's umask (typically `0644`), so any local user of the hypervisor host could find and read it. New seeds are built in `VIRTRIGAUD_LIBVIRT_SEED_DIR` (default `/var/lib/libvirt/virtrigaud-seeds`): the ISO is `0600` (libvirt's `dynamic_ownership`, the default, gives it to the qemu user at start), `user-data`/`meta-data` are removed once the ISO is built, and the SSH user keeps a private `cloud-init.private.iso` that clones copy from. The provider refuses (fails closed) a seed directory that is missing and cannot be created, not owned by the SSH user, or readable or writable by its group or others: **every create on that host fails**, and so does every clone (single-host or clustered) of a VM that has a seed, with a retryable `refusing to stage cloud-init data` until it is fixed; a create without `spec.userData` is refused too. Clones (R26, single-host and clustered) build their own seed there too, read the source's seed through its private copy (an unusable seed directory refuses the clone before anything is read or copied). Seeds of existing VMs stay where they are; `Delete` finds and removes them there. After a successful create, older seed directories of the same domain name that no domain references are removed. The ancestors of the seed directory must be owned by root or the SSH user and not writable by group or others unless sticky. | **Before rolling the libvirt providers**, on every libvirt host, as root: `install -d -m 0711 -o <ssh user> -g <ssh user> /var/lib/libvirt/virtrigaud-seeds` (the provider creates it itself only when its SSH user may create it, normally root). Keep `dynamic_ownership = 1` in `/etc/libvirt/qemu.conf` (the default). No AppArmor change is needed. To use another directory, set `VIRTRIGAUD_LIBVIRT_SEED_DIR` through `Provider.spec.runtime.env` (not in `/tmp`, `/var/tmp`, `/run`: refused at startup). Keep it on a local filesystem, never NFS (libvirt's `chown` to the qemu user fails on a root-squashed export, and the cleanup only sees its own host's domains). On a host shared by Providers with **different SSH users**, give each Provider its own directory through its `spec.runtime.env` (the default can be owned by only one of them; the others' creates fail closed). Include **migration target hosts** (a migrated VM's create builds its seed there) and every host of a clustered Provider. The directory holds every VM's user-data in plaintext: exclude it from host backups or encrypt them, and prefer short-lived bootstrap credentials. Then make the seeds earlier releases left in `/tmp` private (see [Leftovers from earlier releases](#leftovers-from-earlier-releases)). |
| **A failed or deleted `VMMigration` deletes the disk its import landed on the target; vSphere imports leave no import-VM files** (`internal/controller/vmmigration_landed_disk_cleanup.go`, new provider RPC `DeleteLandedDisk`, [ADR-0011 Addendum A](adr/0011-migration-staging-cleanup.md#addendum-a--landed-disks-rc2)) | Every `VMMigration` whose import ran under this release, on every backend (`pvc`, `nfs`, `s3`). In v0.3.11 the disk `ImportDisk` landed on the target — vSphere `[<datastore>] <namespace>_<vm>-migrated/`, libvirt `<default pool>/<namespace>.<vm>-migrated.qcow2`, the Proxmox import stage — stayed there for good when the migration failed, or was deleted, before a target VM attached it. The manager now records `LandedDiskCleanup=Unknown` (and the target VM and Provider UID in `status.storageInfo.importTarget`) before it sends the import, and asks the target provider to delete the landed disk with the staged disk's `cleanupPolicy` semantics: `OnSuccess` (the default) when the `VMMigration` is deleted, `Always` also when it fails for good, `Never` never (`LandedDiskRetained`). A successful migration's disk is its target VM's disk: the status write that marks the migration `Ready` also closes the condition (`LandedDiskCleanup=True`, `LandedDiskInUseByTarget`; a migration that reached `Ready` under a v0.4.0 candidate before rc4 with the condition still `Unknown` is closed once on its next reconcile), and deleting a `Ready` migration never deletes it. A disk the migration's own target `VirtualMachine` (or any `VirtualMachine` there naming an imported disk) has, a disk the target provider reports in use by any VM (`LandedDiskInUse`), and a disk another in-flight `VMMigration` lands under the same name (`LandedDiskHandedOver`) are kept. The provider deletes only that VM's landing name and refuses one any VM uses (vSphere: no file of the landing folder referenced, checked under the landing lock; libvirt: no domain uses the file; fails closed). A delete that fails is retried with a backoff and holds a deleting `VMMigration` (`LandedDiskCleanupPending`, one `Warning`; `virtrigaud.io/force-delete=true` releases it). An older provider image, a gone or replaced target Provider, an import target recorded without the Provider's UID, a target namespace that no longer grants the migration's namespace, or a refused request (including a Proxmox provider without its migration SSH data plane, and anything but a regular file at the libvirt landing name) records `LandedDiskLeftBehind` with one `Warning` and releases it. A target `Provider` that no longer selects the migration's namespace (`consumerNamespaceSelector`) holds the cleanup, retryably, exactly as it holds the staged-disk delete, until the grant is restored or `force-delete` is set. Within one provider process the delete is serialised with an import and an in-place create of the same landing name (vSphere and libvirt: a lock; Proxmox: an import in flight makes the delete retry). The in-use checks see only the VMs of this vCenter / this libvirt host: do not share the landing datastore or the libvirt default pool directory with hypervisors the Provider does not manage, and run one replica of each provider. A `Failed` migration whose landed disk will still be deleted keeps its target `Provider` from being deleted. The vSphere import also removes the transient import VM's own files (`.vmx`, `.vmsd`, `.hlog`, ..., and the `<landing name>-<hex>.hlog` history log vCenter names with a random suffix) from the landing folder after it unregisters it, never the landed `.vmdk` (best effort, logged at `WARN`). | **Roll the providers with the manager** so `DeleteLandedDisk` is reported; until then the manager records the landed disk as left behind. Apply the CRDs first (new optional `status.storageInfo.importTarget.providerUID`). Disks landed by migrations that failed or were deleted under v0.3.11 are not touched: remove them by hand (see [Leftovers from earlier releases](#leftovers-from-earlier-releases)). Use `cleanupPolicy: Never` to keep a failed migration's landed disk for diagnosis. |
| **Chart: NetworkPolicy switch moved to `networkPolicy.*`** ([#336](https://github.com/projectbeskar/virtrigaud/pull/336)) | The legacy `security.networkPolicies` key is now ignored. Only relevant if you deployed a `main` build from the #327 window and set `security.networkPolicies.enabled=true`. | Set `networkPolicy.enabled=true` instead. For any Helm upgrade that carries custom values across chart versions, prefer `helm upgrade --reset-then-reuse-values` over `--reuse-values` (see "Required upgrade order"). |
| **Clustered providers: VMs are placed against committed capacity; a resize-up is checked too** (ADR-0007 A5 scheduler-accuracy amendment, [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md#committed-capacity)) | Only `topology: cluster` Providers. The scheduler subtracts what each host already holds (bound, pending and deleting VMs, in every namespace, at their admitted size), so a host that used to take any number of VMs now refuses the ones that do not fit (`Placed=False/Unschedulable`). A `Reconfigure` that grows a VM's CPU or memory is sent only if it fits in its host's free capacity; otherwise the VM keeps its size and gets `Reconfiguring=False/InsufficientHostCapacity`, retried with a backoff (30 s to 2 min). A shrink is never refused, but a **running** VM's shrink waits until the VM is powered off (`Reconfiguring=False/ShrinkPendingPowerOff`); VirtRigaud never powers it off itself, and a change that shrinks anything waits as a whole. A VM whose VMClass enables **memory hot-add** counts at its balloon ceiling, 4× its memory, existing VMs included (recorded from now on in `status.placement.memoryCeilingMiB`). | To shrink a running clustered VM, set `spec.powerState: Off`, wait for `status.currentResources` to show the new size, then set it back to `On`. After the upgrade, compare the committed gauges with each host's capacity: existing memory hot-add VMs now count at 4× their memory. Check that each clustered Host's `allocatableCPU`/`allocatableMemoryMiB` times the pool's overcommit ratio covers the VMs you expect on it; raise the ratio, add Hosts, or move VMs if not. Keep room for unmanaged domains with a ratio below 1. The new gauges `virtrigaud_host_committed_cpu`/`_memory_mib` show each host's committed sum; they are administrator-sensitive, so enable the chart's NetworkPolicy or `--metrics-secure` before exposing `/metrics`. |
| **libvirt `Reconfigure` reports failures and restart-required instead of silently succeeding** ([`docs/reconfigure-results.md`](reconfigure-results.md)) | All libvirt VMs whose size is changed. Before, a CPU or memory change the domain did not take (a failed `setvcpus`/`setmem`/`setmaxmem`, live or offline, or a failed offline disk resize) returned success, `status.currentResources` recorded a size the VM did not have, and a live change was never written to the persistent definition (it was undone at the next power cycle). Now each change is applied live **and** persisted, or persisted only and reported as restart required (`Reconfiguring=True/RestartRequired`; a grow beyond a VM's running maximum, a vCPU unplug that is not hotpluggable, and **every memory shrink of a running VM** — which also lowers the VM's memory maximum, removing any memory hot-add headroom), or the call fails (`Reconfiguring=False/ProviderError`, retried). A paused or suspended domain is refused instead of changed. A stopped non-hot-add VM's CPU/memory grow, which used to fail silently, now works. | Expect `RestartRequired` on running VMs that were resized beyond their hot-add headroom or shrunk: power-cycle them (`spec.powerState: Off`, then `On`) to take the new size. Expect `ProviderError` where a resize used to "succeed" without effect — read the condition message. Alerting on `Reconfiguring` should treat `RestartRequired` as action-needed, not as a failure. On single-host libvirt Providers this changes the pinned Reconfigure command sequences (the ADR-0008 soak window restarts). The disk is now grown before any CPU/memory change, so a disk grow the host refuses fails the call before anything else changes. A failed Reconfigure is retried on a per-VM backoff (5 s doubling to 5 min, at once after a spec change) instead of every few seconds; on a clustered Provider it is counted at the larger of the old and the requested size until a retry succeeds, and a single-host failure on the VM no longer counts toward the Provider's circuit breaker (an unreachable host still does). |
| **Clustered resizes are held on a Provider that does not report `supportsHonestReconfigure`** ([`docs/reconfigure-results.md`](reconfigure-results.md#providers-without-the-honest-result)) | Only `topology: cluster` Providers. A resize (grow or shrink) of a VM is not sent until its Provider reports `status.reportedCapabilities.supportsHonestReconfigure` — the current libvirt provider does; an older provider image does not, and its Reconfigure could report a change applied that was not, which the committed-capacity accounting would trust. The VM keeps its size (`Reconfiguring=False/ProviderLacksHonestReconfigure`, re-checked every 30 s). Single-host Providers are resized as before (the manager logs a warning). | Roll the libvirt provider images promptly after the manager (step 4). Readiness requires the field on the Provider CRD (step 1). Even with the capability, a clustered Reconfigure answer without `TaskResponse.honest_result` (a provider rolled back behind a stale capability) is not trusted: the VM is counted at the larger size and the resize re-checked every 2 minutes. |
| **`status.powerState` can be `Suspended` or `Unknown` (libvirt)** ([`docs/reconfigure-results.md`](reconfigure-results.md#power-states-off-means-powered-off)) | libvirt VMs that are paused, suspended to RAM by their guest, or in a state the provider cannot classify — reported as `Off` before. `idle` (running, vCPUs blocked) domains are now `On`. While a VM is `Suspended`/`Unknown` the manager neither powers it on nor resizes it (`Ready=False/PowerStateUnmanaged`); a `Suspended` VM whose `spec.powerState` is `Off` is powered off (destroyed), one whose `spec.powerState` is `OffGraceful` is held with a message that a hard `Off` is required (a suspended guest cannot shut down gracefully), and an `Unknown` VM is left alone entirely — including one a third-party (SDK) provider reports in a state outside `On`/`Off`/`Suspended`, which is recorded as `Unknown`. Existing clustered domains created before this release can still suspend to RAM (new ones cannot); that is safe — they are reported `Suspended` and left alone — and adding `<pm>` to them is a follow-up. New clustered domains are created with guest suspend disabled. | Upgrade the CRDs first (step 1): an older VirtualMachine CRD rejects the new values (the manager's readiness check reports it). Tooling that reads `status.powerState` must accept the two new values. Go code importing the API package: `VirtualMachineStatus.PowerState` is now of type `ObservedPowerState` (wire-compatible, not source-compatible — assign `ObservedPowerStateOn`/`Off` or convert). Resume a paused VM on the hypervisor (`virsh resume`, `virsh dompmwakeup`) to hand it back to VirtRigaud. |
| **Clustered providers: a pending VM's size is frozen** (CRD rule) | Only `topology: cluster` Providers. While `status.placement.pendingHost` is set and `status.id` is empty, the API server rejects any change to `spec.classRef` or `spec.resources` (`422 Invalid`). It lifts as soon as the VM is created. The size the VM was scheduled at is recorded in `status.placement.pendingResources`; if its VMClass is edited so that the retried Create would be larger (or gain memory hot-add), the retry is not sent (`Placed=False/PendingSizeGrew`). The manager's readiness check reports the rule, `status.placement.pendingResources` or `status.placement.memoryCeilingMiB` missing on an older CRD. | Upgrade the CRDs before the manager (step 1). Tooling that edits a VM's size right after creating it must wait until `status.id` is set. |
| **Clustered providers: cross-host `ListVMs` and adoption** (ADR-0007 Addendum A slice 4, [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md#listing-and-adoption-slice-4)) | Only `topology: cluster` Providers. `ListVMs` now runs on every host (each VM tagged with its `host_id`; hosts it could not list are reported in `unreachable_host_ids`, not as empty), and `virtrigaud.io/adopt-vms: "true"` on a clustered Provider — refused before — now **adopts**: every **unstamped** domain on its hosts that no VirtualMachine manages becomes a VirtualMachine in the Provider's namespace, named `<domain>-<10 hex digits>`, bound to its host and counted in the host's committed capacity. The domain is stamped with the new VirtualMachine (`virsh metadata`). A domain stamped by a VirtualMachine that no longer exists (after `orphan-on-delete` or a restore) is **not** adopted on a clustered Provider — unlike single-host — and is named in `status.adoption.message`: re-attach it per the ADR-0007 A6 runbook, or remove it. Adoption needs a provider that reports `supportsRoutedAdoption` (new libvirt provider image); with an older one the Provider's `status.adoption.message` says so and nothing is adopted. Single-host `ListVMs` and adoption are unchanged. | **Before upgrading, remove `virtrigaud.io/adopt-vms` from any clustered Provider whose hosts carry domains you do not want managed** — an adopted VM's deletion deletes its domain and disks. Roll the libvirt provider with the manager (step 4). A clustered Provider's Deployment now always runs **one replica with the `Recreate` strategy** (`spec.runtime.replicas` above 1 is ignored): its owner transfers are serialized inside one process. **Every rollout of a clustered provider is therefore a short outage** — the old pod stops before the new one is ready, and VM reconciles against it are retried meanwhile — so roll it in a maintenance window. Keep each host endpoint under one clustered Provider. Watch `status.adoption.message` for hosts that could not be listed (their VMs are unknown and are retried every 5 minutes). |
| **Clustered providers: a VM whose name a previous incarnation holds now waits; Create, Clone and Delete check every host and fail closed while one cannot be checked** (ADR-0007 A6.1, [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md#shared-storage-the-cluster-wide-disk-guard-a61)) | Only `topology: cluster` Providers. **(1)** A create or clone that meets a domain stamped with the VM's namespace and name under another UID — left by `orphan-on-delete`, a force-delete or a backup restore — on its host, or on any host using its disk file, is now **held**: `Placed=False/RestorePending` (and `Provisioning`), `pendingHost` kept, and nothing is created. Before, the host was excluded and another host created a second domain — on a shared pool overwriting the original's disk. A foreign or unstamped same-named domain still excludes the host. A domain with the VM's **own** UID on another host (its placement record was lost) is held the same way, as `Placed=False/OwnDomainOnAnotherHost` (a clone's target too), with its own message: move `pendingHost` to that host, no re-stamp; deleting such a VM is held too (`DeleteBlocked=True/OwnDomainOnAnotherHost`) until `pendingHost` is moved or it is force-deleted / orphaned. **(2)** When a file already exists where a clustered VM's disk goes — under any name it may have had (`<vol>`, `<vol>.qcow2`, the legacy `<name>-disk`, a migrated disk) — it is written only if no domain on **any** host of the Provider uses it; a **host-path base image** is likewise refused while a domain on any host uses it, and a refused image path answers the same whether it does not exist, is not allowed, is in use or has a header it may not have (a backing file, say; the detail is in the provider log). While a host cannot be checked the create or clone **fails closed** (`HOST_UNAVAILABLE` or `VM_DISK_CHECK_FAILED`); so every clustered create from a host-path image now needs every host reachable. With no file there and no host-path image, nothing changes. **(3)** Every clustered **delete** first checks every other host: it is refused (`Ready=False/DeleteBlocked`) while a domain on another host uses the VM's disk, and **held while any host of the Provider cannot be checked**: `DeleteBlocked=True` (`HostUnreachable` or `DiskCheckFailed`) and `Ready=False/DeleteBlocked`, a message naming no host, one `Warning` event per transition. **(4)** A `Host` that exists but cannot be routed (no usable endpoint or credentials) is passed to the provider as an id-only tombstone and counts as unreachable: it holds deletes and fails creates closed instead of being silently skipped. **(5)** Held deletes, `RestorePending` VMs and creates answered `HOST_UNAVAILABLE` or `VM_DISK_CHECK_FAILED` are retried with a per-VM backoff from 15 s doubling to 5 min (from when the hold began; a create on an unreachable pending host was retried every 30 s before); adding `force-delete` or `orphan-on-delete` is acted on at once. The provider runs at most 2 cluster scans at a time, skips hosts that failed within 30 s without dialing, stops a scan once its answer is known, and fails a delete or base-image check at once, without scanning, while a tombstoned or recently unreachable host is among the Provider's hosts. None of these counts toward the circuit breaker. | Re-attach or remove a previous incarnation with the runbook ([Backup and restore](clustered-restore.md#re-attach-runbook)). Mount a shared pool at the **same path** on every host of a Provider, with **NFS locking enabled** (QEMU's image locks rely on it), and **only on the hosts of one clustered Provider** — the guard cannot see another Provider's domains. With a shared pool, **set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` to a catalog directory separate from the pool**: the default image directory is the default pool's, and any file in the pool that no domain uses and that is not a VirtRigaud-managed name can otherwise be named as a base image, and copied, by any tenant of the Provider. To delete VMs while a host is down: bring it back; or **first fence it (power it off, or revoke its access to the export), and only then** delete its `Host` (only when no VM is bound or pending on it) — a deleted `Host` is no longer checked, so an unfenced one could still be writing the disks you delete; or detach with `orphan-on-delete` / `force-delete` and clean up by hand. Fix (or fence, then delete) any `Host` its Provider reports as skipped (`HostCredentialsReady=False`, or a `HostInventoryEntrySkipped` event): it now blocks that Provider's clustered deletes. Roll the manager and the clustered libvirt provider together: an older manager reads `VM_PREVIOUS_INCARNATION` as a plain name conflict and excludes the host, and an older provider ignores the tombstones. |
| **Clustered providers: a restored or re-created VM is held before it is scheduled** (ADR-0007 A6.2, [`docs/clustered-restore.md`](clustered-restore.md)) | Only `topology: cluster` Providers. **(1)** The manager now writes the annotation `infra.virtrigaud.io/placement-uid` (the VM's own UID) on every clustered VirtualMachine: before its first `pendingHost`, on a clone's target, when adoption binds it, and on existing bound VMs after their next successful `Describe` — a one-time metadata patch per VM. A VM re-created under a new UID (a Velero restore, an exported manifest) whose marker names another UID and that has no `status.id` or `pendingHost` is **held**: `Placed=False/RestorePending` and `Provisioning=False/RestorePending`, no image prepare, no scheduling, no `Create`. **(2)** Before a clustered VM is first scheduled, the manager asks every host (one owner-filtered `ListVMs`) for domains stamped with its namespace and name: another UID, more than one, or an unreadable stamp holds it (`RestorePending`); its own domain has its host recorded as `pendingHost`, at the domain's own size, and the create binds it; a resize then converges it to the spec. Hosts that cannot be checked do not hold it. The check looks at the VM's candidate domain names only (`<namespace>.<name>`, the stamped legacy bare name): an **adopted** domain, which keeps its original name, is not seen. A clone's target is checked the same way before the clone is admitted. **(3)** It needs a provider that reports `supportsListOwnerFilter`: with an older provider image every **new** clustered VM is held (`Placed=False/ProviderLacksListOwnerFilter`); a failed check (or a provider busy with more than 2 such listings at once) holds it with `UniquenessCheckFailed`. All three back off from 15 s to 5 min; a clean answer is reused for 3 minutes per VM. A VM whose own domain is on a host the Provider does not front keeps its finalizer when deleted (`DeleteBlocked=True/OwnDomainOnAnotherHost`) until `force-delete` or `orphan-on-delete`. **(4)** A bound VM whose marker names another UID and whose domain is missing shows `Ready=False/RestorePending` instead of `VMMissingOnHost`. **(5)** A pending create answered with the VM's own domain on another host moves `pendingHost` there by itself when the lookup finds exactly that domain. **(6)** A clustered clone answered `HOST_UNAVAILABLE` or `VM_DISK_CHECK_FAILED` backs off (15 s to 5 min) instead of retrying every 30 s. Single-host Providers ignore the marker and never run the check. | **Roll the clustered libvirt provider right after the manager** (step 4): until it reports `supportsListOwnerFilter`, no new clustered VM is created. **After re-creating a VM deleted with `orphan-on-delete`, expect it to wait**: re-attach or remove the old domain ([runbook](clustered-restore.md#re-attach-runbook)). Include VirtualMachine status in backups (Velero `restoreStatus`). Tooling that copies VirtualMachine annotations between objects should drop `infra.virtrigaud.io/placement-uid`; a copy carrying another VM's UID is held until the marker is removed. **Keep the marker out of committed manifests** (strip it on export; Argo CD `ignoreDifferences`, see [GitOps re-apply](clustered-restore.md#gitops-re-apply)). Before re-creating an adopted VM after `orphan-on-delete`, find its old domain by stamp and re-attach or remove it. |
| **Clustered providers: orphan-on-delete needs the Provider's permission for consumers** ([`docs/vm-provider-binding.md`](vm-provider-binding.md#detaching-a-vm-without-deleting-it-virtrigaudioorphan-on-delete)) | Only VMs in **another namespace** than their `topology: cluster` Provider. Deleting such a bound VM with `virtrigaud.io/orphan-on-delete=true` is held (`Ready=False/OrphanOnDeleteNotAllowed`, finalizer kept) unless the Provider carries `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"`, because a detached VM would keep running outside the capacity accounting. VMs in the Provider's own namespace and single-host Providers are unchanged. | Provider administrators who want consumers to detach VMs annotate the Provider. A consumer whose deletion is held removes the orphan annotation to delete the VM normally, or asks the administrator. |
| **Clustered providers: force-delete needs the Provider's permission for consumers too** ([`docs/vm-provider-binding.md`](vm-provider-binding.md#detaching-a-vm-without-deleting-it-virtrigaudioorphan-on-delete)) | Only VMs in **another namespace** than their `topology: cluster` Provider. `virtrigaud.io/force-delete=true` used to release such a VM whose delete was held or failed (`ProviderUnavailable`, `HostUnreachable`, `DiskCheckFailed`, `DiskInUse`, `OwnDomainOnAnotherHost`, a failed `Delete`, `ConsumerNotAllowed`, `ProviderRefMismatch`) without a provider `Delete`, leaving its domain running on the shared hosts outside the capacity accounting. It is now **ignored** for such a VM — `ForceDeleteRefused=True/ForceDeleteNotAllowed` plus one `Warning` event, finalizer kept, the delete retried as if the annotation were not set — unless the Provider carries `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"` (the same permission as orphan-on-delete). VMs in the Provider's own namespace and single-host Providers are unchanged. | Provider administrators who want consumers to be able to force-delete annotate the Provider (it also allows orphan-on-delete). Otherwise a consumer whose delete is stuck asks the administrator, who can remove the leftover domain by hand: the next retry's `Delete` then answers not-found and the VM is released. |
| **Single-host VMs: a Create whose outcome was never recorded is no longer sent again blindly, and such a VM is no longer released on delete** ([`docs/vm-provider-binding.md`](vm-provider-binding.md#a-create-whose-outcome-is-unknown-createrequested)) | Every single-host `VirtualMachine`. Before a provider Create, the VM now records `Provisioning=True/CreateRequested` (and its Provider), confirmed before the RPC; the Create's outcome replaces it. If the outcome is never recorded (a lost status write followed by a manager restart, a Create that timed out, or an error the provider did not classify as a refusal): a provider that reports `supports_owner_rebind` (the vSphere, libvirt, Proxmox and mock providers of this release) is sent the Create again (it re-binds the VM it stamped with the VM's UID); any other — every older provider image — is never sent it again, and the VM waits with `Ready=False/CreateOutcomeUnknown` and a `Warning` event (this used to be decided by provider type, so an older Proxmox image, which does not stamp, could make a second VM). Deleting such a VM is never released as if nothing had been created: libvirt and Proxmox find and delete the VM stamped with its UID; libvirt releases it only when the provider confirms that no domain of its names exists (the libvirt provider now answers a Describe of an absent domain with `NotFound` — roll it with the manager); otherwise, and on vSphere, the delete is held (`CreateOutcomeUnknown`). A Create refused with a non-retryable `FailedPrecondition` or `PermissionDenied` is held as `CreateRefused` with a backoff instead of retried every 5 seconds. | For a held VM, check the hypervisor. If the VM exists, record its id: `kubectl patch virtualmachine <name> -n <ns> --subresource=status --type=merge -p '{"status":{"id":"<id>"}}'`. If it does not, `kubectl annotate virtualmachine <name> -n <ns> virtrigaud.io/create-outcome=not-created@<attempt id>`, where the attempt id is the `lastTransitionTime` of the `Provisioning` condition, in RFC 3339: `kubectl get vm <name> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="Provisioning")].lastTransitionTime}'` (any other value is ignored and removed). A held delete is also released by `virtrigaud.io/force-delete=true` (with a `VMLeftOnHypervisor` event). |
| **Single-host `VMClone`: the target `VirtualMachine` is created before the clone, the clone is stamped with it, and a re-sent `Clone` re-binds** ([`docs/vm-ownership.md`](vm-ownership.md#clones-re-bind)) | Every `VMClone` through a single-host vSphere, libvirt or Proxmox Provider. The target `VirtualMachine` used to be created only after the clone, so the `Clone` carried no owner, the clone was unstamped, and a `Clone` sent again after a manager restart or a lost record made a second clone on Proxmox (vSphere and libvirt refused it as a name conflict and the `VMClone` failed, leaving the first clone untracked). Now the target exists, unbound, while the clone runs; the `Clone` carries its UID (`CloneRequest.target_vm.uid`) and every provider stamps the clone with it and returns that clone to a re-sent `Clone`. A same-named VM the target doesn't own is refused with `AlreadyExists` (on libvirt this was a non-retryable `codes.Unknown`): a first attempt fails with `Ready=False/ProviderConflict` and a constant message. A `Clone` whose outcome is unknown (a transport failure after the request was sent, a deadline, an unclassified provider error), and any failure answering a re-send, no longer fails the `VMClone`: it waits (`Pending`, `Ready=False/CloneOutcomeUnknown`, constant message, one Warning event) and is sent again with a backoff of 15 s doubling to 5 min. A Proxmox `CREATE_NOT_PERFORMED` answer is retried the same way as a transient failure. Only a provider that reports the new capability `supports_owner_rebind` (GetCapabilities field 26; the vSphere, libvirt, Proxmox and mock providers of this release) is ever sent a `Clone` (or a `Create`) again; to any other, an unknown outcome fails the `VMClone` as before. Held attempts are paced by their backoff whatever reconciles the `VMClone`. A failed single-host clone removes the empty target it created unless the provider reported a cloned VM or a `Clone` of unknown outcome is on record; a target that is gone is never re-created at the bind. | **Roll the providers promptly after the manager** (the usual order): until a provider reports `supports_owner_rebind`, a `VMClone` whose `Clone` had an unknown outcome fails, as before, and an older vSphere or libvirt provider leaves clones unstamped. Don't change a vSphere Provider's default folder while clones or creates are held (a re-send would look in the new folder). Check automation that expected such a `VMClone` to go `Failed`. A `VMClone` held in `CloneOutcomeUnknown` that you want to abandon: remove or adopt any VM stamped with its `status.targetUID`, then delete the `VMClone` **and** its target `VirtualMachine` (deleting the `VMClone` never touches the target). |
| **A `VMMigration` snapshot/export/import or a `VMSnapshot` create whose result was lost to a manager restart, or that failed without a definite answer, is no longer sent again blindly** ([`docs/provider-call-outcomes.md`](provider-call-outcomes.md)) | Every `VMMigration` and `VMSnapshot`. Before `SnapshotCreate`, `ExportDisk` or `ImportDisk` the object now records `SnapshotRequested` / `ExportRequested` / `ImportRequested` (a condition written with the read `resourceVersion` as precondition; nothing is sent if that write fails), removed with the call's result or a definite failure (nothing done, or a refusal). A marker without a result — a restart or leader change between the call and its result, **or a failure without a definite answer (a deadline, a connection dropped after the request was written, an unclassified provider error)**, which no longer fails the attempt — is re-sent only where that cannot duplicate anything: a `SnapshotCreate` through a clustered Provider (adopted by request token once no job runs on the domain), an `ExportDisk` / `ImportDisk` on a staging backend the provider reports in the new capabilities `resend_safe_export_backends` / `resend_safe_import_backends` (fields 27/28; vSphere s3+nfs both ways, libvirt single-host export s3 / import pvc+s3+nfs, clustered libvirt export s3+nfs, Proxmox s3 both ways), and then only 35 minutes after the earlier call (past its 30-minute deadline). Automatic re-sends are capped (10 routed snapshots, 3 exports/imports). Anything else — every single-host `SnapshotCreate`, every export or import on another backend or through a provider image older than this release — is held with `Ready=False/SnapshotOutcomeUnknown`, `ExportOutcomeUnknown` or `ImportOutcomeUnknown` (constant message, one `Warning` event naming the attempt and, for a snapshot, its name). Before, the call was sent again (by the new leader, or by the retry policy after a timeout), which on vSphere and the mock took a second snapshot of the live source VM and could overwrite a staged disk still being written. A deleting migration with an unknown export waits 35 minutes before it deletes the staged object. A single-host libvirt provider answers the failures its code can prove (nothing published, a refusal before any side effect, a command that ended with an error) as definite, so those still fail the attempt; only the failures it cannot prove stay held (a lost connection or a killed command while a copy may still run, an upload that may have completed, a failure after a disk landed; see [`docs/provider-call-outcomes.md`](provider-call-outcomes.md#single-host-libvirt-which-failures-are-definite)). A libvirt provider image older than that answers every failure untyped, so all of them are held; and a host that cannot be reached during these three calls no longer counts toward the Provider's circuit breaker. | **Roll the providers promptly after the manager** (until a provider reports the new fields, a lost export or import is held instead of re-sent). For a held object: check the provider for what the call may have made (the snapshot named in the event, the target's `-migrated` disk) and remove it, then `kubectl annotate <vmmigration\|vmsnapshot> <name> -n <ns> virtrigaud.io/operation-outcome=not-performed@<attempt id>`, the attempt id being the marker condition's `lastTransitionTime` (the API server's audit log records who set it); or delete the object. Alerting on migrations stuck in a phase, or that expected a timed-out call to fail, should include these reasons. |
| **vSphere: a VM bound to a template or an image artifact is held, not retried, when its delete or power change is refused** | `VirtualMachine`s on a vSphere Provider whose hypervisor object is a template or carries a VirtRigaud prepared-image stamp (for example a VM adopted from a template). The provider refuses `Delete` and `Power` on such an object (`FailedPrecondition` + `VM_IS_IMAGE_ARTIFACT`, nothing changed). Before, the manager retried the delete every 15 s and the power change every 5 s with `Ready=False/ProviderError`. Now a refused delete keeps the finalizer with `Ready=False/VMIsImageArtifact` (a clustered VM: `DeleteBlocked=True/VMIsImageArtifact` and `Ready=False/DeleteBlocked`), and a refused power change sets `Ready=False/VMIsImageArtifact`. Both use a constant message, one `Warning` event `VMIsImageArtifact` when the hold begins, and the blocked-VM backoff (15 s doubling to 5 min); a repeated refusal writes nothing. Metric reason `vm-is-image-artifact`. | Detach such a VM without touching the object: set `virtrigaud.io/orphan-on-delete: "true"` and delete the `VirtualMachine`. `force-delete` still releases it too. |
| **Single-host provider `Delete` requests now carry the VM's owner** | Every single-host `VirtualMachine`, adopted ones and clone targets included. `DeleteRequest.owner` (the VM's UID, namespace and name) used to be sent only for clustered VMs. A Proxmox provider with the owner-checked delete (#380) now never destroys a VM stamped with another owner — an unrelated VM that reused the VMID — and answers it not-found with the reason `VM_NOT_OWNED`: the `VirtualMachine` is released, that VM is left alone, and a `Warning` event `VMLeftOnHypervisor` naming the provider VM id (and an error-level manager log line) records it. An unstamped Proxmox VM is deleted as before when its PVE name is the `VirtualMachine`'s name, or the name adoption gave it (the sanitized PVE name, or `vm-<vmid>`). A Proxmox template at the VMID is never deleted, with or without an owner: `FailedPrecondition` + `VM_IS_IMAGE_ARTIFACT`, held as `Ready=False/VMIsImageArtifact`. vSphere, single-host libvirt and mock ignore the owner on a delete. | Nothing to change. A Proxmox VM renamed on the hypervisor after VirtRigaud created it, and never stamped (created before #380), is no longer deleted with its `VirtualMachine`; neither is a Proxmox VM adopted **before this release** that still carries a previous owner's stamp (detached with `orphan-on-delete`, then adopted again; adoption now stamps the VMs it takes over, next row). Search events for `VMLeftOnHypervisor` and the Proxmox provider log for `Refusing to delete`, and remove such VMs by hand. A `VirtualMachine` re-created under a new UID (for example by a backup restore) is released the same way while its VM keeps running: re-adopt or remove it. |
| **Single-host adoption stamps the adopted VM with its new owner** ([`docs/vm-ownership.md`](vm-ownership.md#adoption-stamps-the-adopted-vm)) | Every single-host Provider with `virtrigaud.io/adopt-vms`, through an upgraded vSphere, libvirt or Proxmox provider (they report the new `supportsOwnerTransfer` capability; `TransferOwner` is now implemented by every provider). Adoption used to leave the adopted VM's owner stamp as it was: an unstamped VM stayed unstamped, and a VM detached with `orphan-on-delete` and adopted again (`adopt-stamped-vms`) kept the deleted `VirtualMachine`'s stamp, so its new `VirtualMachine`'s Proxmox `Delete` answered `VM_NOT_OWNED` and left it. Now the adopting `VirtualMachine` is created first **without** `status.id` (annotated `virtrigaud.io/adopted-id`), the VM is stamped with it — compare-and-swap on the hypervisor (vSphere `changeVersion`, Proxmox config `digest`, libvirt domain UUID under a per-host lock), only over no stamp or a stale stamp whose UID an uncached read of the API server finds on no `VirtualMachine` — and only then is `status.id` written. A VM bound by `status.id` to a `VirtualMachine` of **another** single-host Provider object of the same type and endpoint is no longer adopted (reported `bound to a VirtualMachine of another Provider object for the same endpoint`). A refused transfer removes the waiting `VirtualMachine` again (no provider call); an interrupted adoption is finished by the next discovery, retried every 5 minutes. A provider that cannot stamp (older, or third-party) adopts as before, unstamped, and the adoption message says so; one whose capabilities cannot be read adopts nothing until they can. VMs adopted **before** this release are not re-stamped. | Roll the providers, then the manager (either order is safe). The vSphere account needs `VirtualMachine.Config.AdvancedConfig` on the VMs adoption takes over; the Proxmox token `VM.Config.Options` on them. For a Proxmox VM adopted before this release that still carries a previous owner's stamp, remove the `virtrigaud.owner` line from its Notes, or detach and re-adopt it with `adopt-stamped-vms` (see the linked doc). |
| **Single-host per-VM requests now carry the VM's owner; Proxmox checks it** ([`docs/vm-ownership.md`](vm-ownership.md#per-vm-calls-describe-power-reconfigure-snapshots-disks-and-clone-sources)) | Every single-host `VirtualMachine`, and every `VMMigration` and `VMClone` of one. The owner (the VM's UID, namespace and name) used to be sent only for clustered VMs, except on `Delete`; it is now sent on `Describe`, `Power`, `Reconfigure`, the snapshot calls, `GetDiskInfo`, `ExportDisk` and as a clone's `source_owner`. A Proxmox provider of this release checks it against the owner stamp of the VM at the VMID before it reads, copies or changes anything: a VM stamped with another owner, or unstamped and not named like the `VirtualMachine` (its VMID was reused), is answered `NotFound` + `VM_NOT_OWNED`; a stamp that cannot be read, `FailedPrecondition` + `OWNER_STAMP_UNREADABLE`. Before, such a `VirtualMachine` read, powered, resized and snapshotted whatever VM held its VMID, and a migration or clone of it copied that VM's disks. The manager holds the `VirtualMachine` as `Ready=False/VMNotOwned` (or `OwnerStampUnreadable`) with one `Warning` event and a backoff of up to 5 minutes, keeps `status.id`, and never creates a VM in its place. A `VMMigration` or `VMClone` of it fails with a constant message before anything is exported or cloned; with `powerOffBeforeMigration` the source's `spec.powerState` is no longer set to `Off` first. A `VMSnapshot` delete answered `VM_NOT_OWNED` is released as already gone; a snapshot create fails. vSphere, single-host libvirt and mock ignore the owner. The check needs both the manager and the Proxmox provider of this release. | Before upgrading, look for Proxmox VMs that would be held: unstamped VMs renamed in PVE after VirtRigaud created them (rename them back), and VMs an earlier release adopted while they carried a previous owner's stamp (compare each adopted `VirtualMachine`'s UID with `qm config <vmid> | grep virtrigaud.owner`; remove the stale `virtrigaud.owner` line from the VM's Notes). Alternatively delete such a `VirtualMachine` (the VM is left in place) and let adoption take the VM again: it is stamped with its new `VirtualMachine` when the provider reports `supportsOwnerTransfer` (this release), the Provider has `virtrigaud.io/adopt-vms` and, for a VM that still carries a stamp, `virtrigaud.io/adopt-stamped-vms`, and no `VirtualMachine` has the stamped UID. After the upgrade, look for `Ready` reason `VMNotOwned`, for failed `VMMigration`s and `VMClone`s whose message says the source VM was refused, and for `Refusing the call` in the Proxmox provider log. A `VirtualMachine` held because its VMID was reused by another VM: delete it (that VM is left alone) and create it again. |
| **Nothing is created through a `Provider` being deleted, even when the manager's cache has not seen the deletion yet** ([ADR-0010](adr/0010-provider-in-use-finalizer.md)) | Every provider type. Right before a `Create`, `Clone` or `ImportDisk` RPC — after the object recorded its use of the Provider — the manager re-reads the `Provider` from the API server. When it is being deleted, gone, or replaced by another object of the same name, nothing is sent: a `VirtualMachine` gets `Ready=False/ProviderDeleting` (a create marker written for this attempt alone is replaced by `Provisioning=False/ProviderDeleting`, as no Create was sent; an earlier attempt's marker is kept), a `VMClone` `Ready=False/ProviderDeleting` (phase `Pending`), and a `VMMigration` `Ready=False/ProviderDeleting` in its current phase (not failed). Constant messages, one `Warning` event `ProviderDeleting`, re-checked every minute. A clustered VM's in-flight create on its pending host is held the same way. On the Provider side, before a deleting `Provider`'s in-use finalizer is removed, what still uses it is counted again from the API server (one List per kind across all namespaces, in pages of 500, once per release); a use the cache had not seen yet keeps it held, and a failed read keeps it held and retries. | Nothing to change. Delete or re-point the objects, or re-create the Provider. |
| **`force-delete` of a `VMSnapshot` or `VMMigration` needs the Provider's permission for cross-namespace consumers of a clustered Provider** ([`docs/vm-provider-binding.md`](vm-provider-binding.md#detaching-a-vm-without-deleting-it-virtrigaudioorphan-on-delete)) | Only `VMSnapshot`s and `VMMigration`s whose VM is in **another namespace** than its `topology: cluster` Provider. `virtrigaud.io/force-delete=true` released a `VMSnapshot` whose provider snapshot delete failed or could not be sent, and a `VMMigration` whose source snapshot delete failed, leaving the snapshot on the shared hosts outside the capacity accounting. It is now **ignored** for such objects — `ForceDeleteRefused=True/ForceDeleteNotAllowed`, a constant message and one `Warning` event, finalizer kept, the delete retried as if it were not set — unless the Provider carries `infra.virtrigaud.io/allow-consumer-orphan-on-delete: "true"`, as for a `VirtualMachine` (#381). Same-namespace objects and single-host Providers are unchanged. | Provider administrators who want consumers to be able to force-delete annotate the Provider. Otherwise the administrator removes the leftover snapshot by hand; the next retry then completes the delete. |
| **The kustomize helper ClusterRoles in `config/rbac` now grant what their names say** | Anyone who bound the `*-admin-role`, `*-editor-role` or `*-viewer-role` ClusterRoles from `config/rbac` (Provider, VirtualMachine, VMClass, VMImage, VMNetworkAttachment). They named the API group `infra.virtrigaud.io.infra.virtrigaud.io`, which matches nothing, so binding them granted nothing; they now grant `infra.virtrigaud.io`. **Subjects bound to these roles gain that access when the new manifests are applied.** The admin roles list their verbs explicitly instead of `*`. The `provider-*` roles carry the label `virtrigaud.io/role-scope: platform-admin`: a Provider holds hypervisor credentials and controls who may use it (`spec.consumerNamespaceSelector`) and whether consumers may leave VMs behind (`allow-consumer-orphan-on-delete`). The chart does not ship these roles. | Before applying, review every binding to these ClusterRoles (`kubectl get clusterrolebindings,rolebindings -A -o wide \| grep -E '(provider\|virtualmachine\|vmclass\|vmimage\|vmnetworkattachment)-(admin\|editor\|viewer)-role'`). Bind `provider-*` roles to platform administrators only. |
| **libvirt: an existing `default` storage pool is never rewritten; one at another path fails closed** (`internal/providers/libvirt/storage.go`, [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md#host-prerequisites)) | Since v0.3.11, a libvirt `Create`, `ImportDisk` or NFS/S3 import on a host whose `default` pool was **not** at `/var/lib/libvirt/images` ran `pool-destroy` + `pool-undefine` on it and recreated it there — silently rewriting, for example, an NFS-backed `default` pool. It now never destroys, undefines or redefines an existing pool: a `default` pool at `/var/lib/libvirt/images` is started if inactive, a missing one is created there as before, and one at any other directory is left untouched and the operation fails with `InvalidSpec` / `InvalidArgument` and a constant message, `the libvirt storage pool "default" on the VM's host is not at /var/lib/libvirt/images; VirtRigaud requires it there and did not change it — an administrator must fix the host (details in the provider log)` (the `VirtualMachine` shows it on `Ready`, reason `ValidationError`, re-checked on the slow non-retryable cadence; the host and the pool's path are only in the provider log). | Before upgrading, run `virsh pool-dumpxml default` on every libvirt host. If its `<target><path>` is not `/var/lib/libvirt/images`, mount that storage **at** `/var/lib/libvirt/images` and redefine the pool there yourself (or point the pool back at the local directory). Shared storage for a clustered Provider goes at that path too; keep `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` for image *sources* only. A host whose pool VirtRigaud already rewrote keeps the rewritten pool — check that the operator's original pool definition is restored if it mattered. |
| **`powerOffBeforeMigration`: the VirtualMachine controller powers the source off; the migration only waits** (`internal/controller/vmmigration_source_power.go`) | A `VMMigration` with `spec.source.powerOffBeforeMigration: true` sets the source `VirtualMachine`'s `spec.powerState` to `Off` (as before) and then **waits** — `Validating=False` and a new `SourcePowerOff=False` condition, reason `WaitingForSourcePowerOff`, status written only on change — until a read-only `Describe` reports the source `Off`. It no longer calls the provider's `Power` itself: that second, concurrent power-off failed on libvirt (`virsh destroy` of an already-stopped domain) and failed the migration terminally although the source was off. A source not observed off within **10 minutes** (measured from `SourcePowerOff`'s transition, so other Validating checks do not restart it; a retry starts a fresh budget) fails the migration with a constant message; a failed `Describe` is waited out, but a source its provider does not find fails the migration at once (`the source VM was not found on its provider`). The source is still not powered back on by the migration. | None. If a migration waits, look at the source `VirtualMachine`'s `Ready` condition and events: it is that controller's power-off that is pending. |
| **Power is idempotent on every provider** (`internal/providers/{libvirt,vsphere,proxmox,mock}`) | Powering off a VM that is already off, or on a VM that is already running, now succeeds instead of returning a retryable error — also when another actor won a race to the same change. libvirt re-reads `domstate` after a failed `start` / `destroy` / `shutdown`; vSphere re-reads the power state after `InvalidPowerState`; Proxmox skips the PVE task when the VM is already in the requested state and re-reads it after a failed request. `Reboot` is unchanged. | None. |
| **libvirt: deleting an external snapshot removes the overlay file libvirt leaves behind** (`internal/providers/libvirt/snapshot_overlay_cleanup.go`) | On libvirt 10 (Ubuntu 24.04), `virsh snapshot-delete` of VirtRigaud's disk-only snapshots commits the overlay into its base but leaves `<domain>-disk.<snapshot>` in the pool, and a later VM delete did not remove it. `SnapshotDelete` now removes that file after a successful delete — only when it is the domain's own overlay, out of the domain's current chain, referenced by no other snapshot, a regular file in the pool / allowed image directories, and used by no other domain (on any host of a clustered Provider). Anything it cannot establish keeps the file; a failed removal is logged by the provider and never fails the delete. | Overlays left by snapshots deleted **before** the upgrade are not swept: on each libvirt host, list `<pool>/*-disk.*` files that are not `.qcow2`, check with `virsh domblklist --all`/`qemu-img info --backing-chain` that no domain uses them, and remove them by hand. |
| **Provider pods mount only the migration PVCs a live `VMMigration` references** (security fix, `internal/controller/provider_migration_storage.go`, [`examples/migration/README.md`](../examples/migration/README.md#pvc-staging-backend-legacy-what-the-provider-pods-mount)) | `storage.type: pvc` migrations, and anyone who labels PVCs `virtrigaud.io/component=migration-storage`. **Before:** every PVC in a Provider's namespace with that label was mounted read-write into **every** provider pod in the namespace, and those pods hold the hypervisor credentials. So anyone who could create a PVC there could attach any storage to them, or stall their rollout with a PVC that never binds. **Now:** a PVC is mounted only into the source and target Provider of a `VMMigration` in that namespace that records it in `status.storagePVCName` and is still in flight (not `Ready`, not deleting, not `Failed` with no retry left). A `Failed` migration that will be retried keeps the mount only if a Ready provider pod carries the PVC. The PVC must carry the label, must be `Bound`, and must be one of two: the PVC the migration created, identified by the UID it records in the new `status.storagePVCUID` from its own create (an `ownerReference` can be forged and no longer counts), or the PVC `spec.storage.pvc.name` names. A `Pending` PVC is mounted only during `Validating`, and only on a `WaitForFirstConsumer` StorageClass. The target mounts it **read-only**. A migration fails at `Validating` instead of waiting out the mount budget when its named PVC lacks the label or belongs to another `VMMigration`, or when a `<name>-storage` PVC it didn't create is already there. Changing `spec.storage.pvc` after the PVC is recorded fails the migration. The 5-minute mount budget now starts at the latest of the PVC's creation, the migration's start and its last retry, so an older named PVC isn't timed out at once. The manager's ClusterRole gains `get` on `storage.k8s.io/storageclasses`; the kustomize `config/rbac` role also gains read access to `pods`, which the chart already granted. Deleting a `VMMigration` now deletes only the PVC it created (the recorded UID). Before, it also deleted the PVC named in `spec.storage.pvc.name`. That PVC is now unmounted and left for its owner, with a Normal `MigrationPVCRetained` event. **New pod rolls:** before, a migration PVC stayed mounted until the migration was deleted. Now the pod is also rolled when the migration becomes `Ready` or fails for good. A named PVC shared by two migrations flips from read-write to read-only (another roll) when the one exporting through that Provider finishes. | None for migrations in flight: their PVCs are already labelled and recorded in status, so they stay mounted. A migration whose PVC an older manager created has no `status.storagePVCUID`; for it, the controller `ownerReference` with its UID is still accepted. On upgrade, each provider pod is rolled once to drop unreferenced PVCs and switch target mounts to read-only, which interrupts an export or import running at that moment, as any provider rollout does. Upgrade between migrations if you can. For a future migration with `spec.storage.pvc.name`, label that PVC first: `kubectl label pvc <name> virtrigaud.io/component=migration-storage`. Labelled PVCs that only existed to be mounted are no longer mounted; reference them from a `VMMigration` instead. If you relied on migration deletion to remove your own named PVC, delete it yourself, or remove the staged disk under `vmmigrations/<namespace>/<migration-name>/` on it. |
| **`spec.snapshot.revertToRef` now reverts the VM** (fix of a silent no-op, `internal/controller/virtualmachine_snapshot_revert.go`, [`docs/vm-snapshot-revert.md`](vm-snapshot-revert.md)) | Every `VirtualMachine` that sets `spec.snapshot.revertToRef`. **Before:** the field was accepted and ignored; the VM stayed `Ready` with no condition or event. **Now:** the `VirtualMachine` controller sends `SnapshotRevert` once per request, only while the provider reports the VM `Off` and `spec.powerState` is `Off` or `OffGraceful` (it never powers a VM off for it), only to a `Ready` `VMSnapshot` taken of this VM through its Provider, and records the outcome in the new `status.snapshotRevert` and the `SnapshotReverted` condition. While the revert waits, runs or has failed, `Ready` is `False` with the condition's reason. A VM that already carries the field is acted on by the first reconcile after the upgrade. | Upgrade the CRDs before the manager (a CRD that prunes `status.snapshotRevert` makes the manager send nothing, `SnapshotReverted=False/Unsupported`). Before upgrading the manager, remove `spec.snapshot.revertToRef` from every VM you do not want reverted: `kubectl get virtualmachines.infra.virtrigaud.io -A -o jsonpath='{range .items[?(@.spec.snapshot.revertToRef)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'` lists them (use the full resource name: `vm` can resolve to another project's `VirtualMachine`). Check `VMSet` templates too (`spec.template.spec.snapshot.revertToRef`): VMs a `VMSet` creates carry the field. |
| **libvirt: snapshots of a powered-off VM are taken with the cloud-init CD-ROM ejected, and a revert to a snapshot that stored the CD-ROM is refused** (`internal/providers/libvirt/snapshot_media.go`, `snapshot_readonly_disks.go`, [`docs/vm-snapshot-revert.md`](vm-snapshot-revert.md#libvirt)) | Every libvirt VM with a cloud-init seed (or another CD-ROM or read-only disk with media) that has, or will have, a `VMSnapshot`. **Before:** `spec.snapshot.revertToRef` did nothing (see the row above), and a revert of a disk-only snapshot of such a VM through the provider, on libvirt 9.9 or later with AppArmor, made libvirt turn the seed CD-ROM into an overlay QEMU cannot open; the revert was reported `Succeeded` and the VM no longer started. **Now:** `SnapshotCreate` of a shut-off VM ejects its CD-ROMs and floppies from the persistent definition for the duration of the snapshot (recorded in the domain's metadata first, inserted again afterwards, also when the snapshot failed), so the stored definition has empty drives; `SnapshotRevert` refuses, before changing anything, a snapshot that stored a CD-ROM or read-only disk with media (`SnapshotReverted=False/RevertFailed`), inserts the VM's CD-ROMs and floppies (the seed included) again after a revert, and reports a medium it cannot insert as a failure. An interrupted ejection is repaired by the next snapshot create, revert, start or reboot of the VM, and a power-on or reboot of a libvirt VM issues one extra `dumpxml`. `SnapshotDelete` also removes the `<domain>-disk.<epoch>` overlay a revert makes. | Roll the libvirt providers (single-host and clustered). Disk-only snapshots taken by v0.3.11 or by a v0.4.0 candidate before rc7 of a VM with a cloud-init seed, and disk-only snapshots of a running VM that has one, can no longer be reverted: delete their `VMSnapshot`s and take new ones with the VM powered off. A VM that a v0.4.0-rc6 revert already broke (its CD-ROM is on a `cloud-init.<digits>` file and it does not start) needs the manual [repair](vm-snapshot-revert.md#repairing-a-vm-broken-by-a-v040-rc6-revert) first. |
| **`status.ips` no longer lists loopback, link-local and other unreachable addresses** (behaviour change for anything that parses it, `internal/providers/contracts/routable_ips.go`, [`docs/vm-status-ips.md`](vm-status-ips.md)) | Every `VirtualMachine`, and anything that reads `status.ips` (scripts, `kubectl get vm -o jsonpath`, `vrtg`). **Before (seen on libvirt in the v0.4.0 candidates):** a libvirt VM with a running QEMU guest agent listed every address of every guest interface, so `status.ips` could read `["127.0.0.1","::1","192.0.2.31","fe80::5054:ff:fe88:d0c1"]`. **Now:** the manager filters every provider's answer before it writes the field: loopback, unspecified, IPv4 and IPv6 link-local and multicast addresses and anything that is not an IP address are dropped, duplicates removed, the provider's order kept; private, IPv6 unique-local and global addresses stay. The libvirt provider applies the same rule to the agent's answer, so the `primary_ip` in `status.provider` is no longer `127.0.0.1`. An address that is dropped no longer counts as "has an IP" for `virtrigaud_ip_discovery_duration_seconds` either. No CRD schema change (the field's description changed). A libvirt guest on libvirt's NAT `default` network now also shows its DHCP lease without the guest agent (V10: those rows were all dropped before); a guest on a host bridge, or with no network attachment, has no lease and shows an address only if the QEMU guest agent runs in the guest (or, as a last resort, the host has an ARP entry for its MAC). | If a script took `status.ips[0]` or matched a loopback or link-local entry, read the first reachable address instead. Nothing to roll back or migrate: the next reconcile of each VM rewrites the field. |
| **libvirt: the disk in-use check must be able to read libvirt's `0600` snapshot overlays; a routed snapshot create that was not performed is retried with a backoff** (`internal/providers/libvirt/imagepath.go`, `internal/controller/vmsnapshot_controller.go`, [`docs/libvirt-clones.md`](libvirt-clones.md#unreadable-disk-images)) | Every libvirt host where a `VMSnapshot` is created or reverted with the VM powered off, and every clustered Provider that creates `VMSnapshot`s. The disk in-use check is new since v0.3.11. **Before (v0.4.0 candidates up to rc7):** after an offline snapshot or a revert, the check that guards a snapshot create/revert/delete and a delete (clustered: also create and clone) re-read a shut-off VM's nested backing stores as the SSH user, which cannot open the `0600` overlay libvirt makes, so those operations of every other VM on the host were refused ("could not verify that no other domain uses its disks"), and a clustered `VMSnapshot` create so refused was held `SnapshotOutcomeUnknown` after ten re-sends. **Now:** a backing store that a disk's chain already read is not read again; an image nobody can read gives "permission denied reading a disk image" with a pointer to the fix, still fail-closed (a VM delete, a snapshot delete and a clustered snapshot create are retried; a single-host snapshot create fails and a revert is re-sent up to 5 times; from v0.4.1 both are retried, see [Upgrading from v0.4.0 to v0.4.1](#libvirt-snapshot-and-revert-answers)); and a routed create the provider says it did not perform is sent again as a new attempt after 1 minute, doubling up to 10 minutes, with no operator hold. | On each libvirt host where the provider's SSH user cannot already read the overlays, grant `VR_DISK_READ` over the pool directory ([Unreadable disk images](libvirt-clones.md#unreadable-disk-images); on NFS the export must not squash root) or repair each new overlay by hand (`chown -h libvirt-qemu:kvm`, `chmod 0640`, VM off). Roll the libvirt providers and the manager. A `VMSnapshot` held `SnapshotOutcomeUnknown` by v0.4.0-rc7 for this cause stays held until you release it with the `virtrigaud.io/operation-outcome` annotation. |
| **libvirt: a disk-only snapshot is not deleted while a memory snapshot is stored in its overlay; a refused snapshot delete shows the provider's answer** (`internal/providers/libvirt/snapshot_delete_guard.go`, `internal/controller/vmsnapshot_controller.go`, [`docs/libvirt-clones.md`](libvirt-clones.md#deleting-memory-and-disk-only-snapshots-of-the-same-vm)) | Every libvirt VM with both a disk-only `VMSnapshot` and a memory `VMSnapshot` (`includeMemory: true`) taken after it, and anything that watches `VMSnapshot` conditions. **Before (v0.3.11 and v0.4.0-rc8 and earlier):** deleting the disk-only snapshot orphaned the memory snapshot stored in its overlay (its data stayed in the leftover overlay file, unusable, because the VM no longer runs on it) and reported success; the memory snapshot's `VMSnapshot` then stayed `Deleting`. A snapshot delete a provider refused with `FailedPrecondition` was `Ready=False/ProviderError` with a constant message. **Now:** that delete is refused, nothing changed, naming the snapshots to delete first; a check that cannot run refuses it retryably; a memory snapshot an earlier release already orphaned has only its metadata deleted when its data is provably gone. A snapshot delete refused with `FailedPrecondition` (other than a disk another VM depends on) is `Ready=False/DeleteBlocked` with the provider's answer, re-checked with a backoff of up to 5 minutes. | Roll the libvirt providers, then the manager. Delete memory snapshots before the disk-only snapshot they were taken on. For a VM still on v0.3.11 or v0.4.0-rc8 or earlier, or a `VMSnapshot` already force-deleted, see the [manual cleanup](libvirt-clones.md#a-memory-snapshot-that-was-already-orphaned). |

**Experimental clustered providers** (`Provider.spec.topology: cluster`, the `Host`/
`HostPool` CRDs) were added entirely after v0.3.11, so nothing on v0.3.11 is affected.
If you are already running a clustered provider from `main`: `Provider.spec.topology`
is now immutable (recreate the Provider to change it), `Host`/`HostPool`
`providerRef`/`credentialSecretRef` must be in the same namespace as the Provider
(drop any explicit `namespace`), and `Host.spec.endpoint` is now pattern-validated.
See [`docs/clustered-provider-inventory.md`](clustered-provider-inventory.md) and
[ADR-0007](adr/0007-clustered-orchestrator-provider.md).

### Stale Proxmox import stages

A Proxmox-target migration stages its disk on the PVE node in
`/var/lib/virtrigaud/import-stage/<namespace>_<vm name>-migrated.<format>`, and the target
VM's create removes the stage after `qm importdisk`. A migration that fails, or is deleted,
between its import and its create leaves the stage (often many GB) on the node. From this
release the staged-disk delete also removes the stage of the migration's target VM (see the
`nfs` / `s3` staging row above), and the landed-disk cleanup (`DeleteLandedDisk`)
removes it as well, but only for migrations that run under this release and only while a
target provider that supports one of the two RPCs is reachable. Sweep the stage
directory on every PVE node periodically for the rest, for example daily from root's crontab:

```sh
# List stages older than 2 days; delete only those whose VMMigration is no longer in flight
# (no VMMigration for <namespace>/<vm name> in phase Importing, Creating or ValidatingTarget).
find /var/lib/virtrigaud/import-stage -maxdepth 1 -type f -name '*_*-migrated.*' -mtime +2 -print
# Interrupted writes (and their NFS copy status files) are always safe to remove when old:
find /var/lib/virtrigaud/import-stage -maxdepth 1 -type f \( -name '.stage-*' -o -name '.rc-*' \) -mmin +720 -delete
```

**One VirtRigaud installation per PVE node.** Stage names are unique within one
Kubernetes cluster only (`<namespace>_<vm name>`), and every installation uses the same
stage directory. Two installations (two Kubernetes clusters, or two VirtRigaud releases in
one cluster) that migrate a same-named VM in a same-named namespace onto the same PVE node
would write, import and remove the same stage. Point each PVE node (cluster) at exactly
one VirtRigaud installation.

### Leftover `virtrigaud-export-*` files on vSphere datastores

Before this release, every vSphere disk export (a `VMMigration` from a vSphere source,
including the S3 and NFS staging paths) leaked a **full copy of the exported disk** on the
source datastore. The export clones the disk to a temporary
`virtrigaud-export-<vm id>-<unix time>.vmdk` next to the source disk and used to delete only
that descriptor; on VMFS the data lives in a separate `virtrigaud-export-…-flat.vmdk`
extent, which stayed behind in the source VM's folder (observed: 12 GiB for a 12 GiB disk).
The leftover also kept the folder alive after the VM was destroyed. This affected v0.3.11
as well. The fix removes the clone completely on every exit of the export, but it does not
remove files that earlier exports already left.

On every datastore that has been a vSphere **migration source**, look for leftovers in the
VM folders (datastore browser, or `ls` on an ESXi host) and delete them:

```sh
# On an ESXi host: list leftovers (descriptor and -flat extent) on all datastores.
find /vmfs/volumes/*/ -maxdepth 2 -name 'virtrigaud-export-*.vmdk'
```

Delete a file only if its name starts with `virtrigaud-export-` and no export is running
(no `VMMigration` from that vSphere source in phase `Exporting`). Delete the descriptor and
its `-flat.vmdk` together, from the vSphere client (Datastore browser, Delete) or with
`vmkfstools -U <descriptor>.vmdk` on the host. A VM folder that still holds only such files
after its VM was destroyed can be removed once they are gone. The provider logs a leftover it
could not delete at `ERROR` ("Failed to delete temporary export disk on the datastore").

### Leftovers from earlier releases

VirtRigaud does not clean up files that releases before this one left behind. Look for
the following once after upgrading, and delete only what no `VMMigration` in flight uses:

- **Staged migration objects** (full, unencrypted copies of source disks) of migrations
  that no longer exist, and the empty files NFS cleanup leaves on libvirt and vSphere
  targets. On S3 they are under `<prefix>/vmmigrations/<namespace>/<name>/export.vmdk` in
  the migration's bucket; on NFS they are `vmmigrations_<namespace>_<name>_export.qcow2`
  (new name) or `vmmigrations-<namespace>-<name>-export.qcow2` (name used before this
  release) on the export. Objects recorded as `StagedDiskLeftBehind` are never retried.
  Versioned buckets also need a lifecycle rule for noncurrent versions (see the staging row
  in the breaking-change table).
- **libvirt snapshot overlay files** `<domain>-disk.<snapshot>` in the storage pool, left
  by snapshots deleted before this release. List the pool's `*-disk.*` files that are not
  `.qcow2`, check with `virsh domblklist --all` and `qemu-img info --backing-chain` that no
  domain uses them, then remove them. Until then, a new disk-only snapshot with the same
  name as one of them cannot be taken; from v0.4.1 it is refused and its `VMSnapshot` is
  `Failed` (see [A snapshot name whose overlay file is still on the
  host](libvirt-clones.md#a-snapshot-name-whose-overlay-file-is-still-on-the-host), which
  also covers a `VMSnapshot` v0.4.0 held for this reason).
- **vSphere `virtrigaud-export-*` files** in the VM folders of migration-source datastores:
  see [Leftover `virtrigaud-export-*` files on vSphere
  datastores](#leftover-virtrigaud-export--files-on-vsphere-datastores).
- **Proxmox import stages** and `/var/tmp/.virtrigaud-import-*` files on the PVE nodes: see
  [Stale Proxmox import stages](#stale-proxmox-import-stages).
- **Landed migration disks** of migrations that failed, or were deleted, under v0.3.11 without
  a target VM that uses them: vSphere landing folders `[<datastore>] <namespace>_<vm>-migrated/`
  (and the pre-v0.4.0 `[<datastore>] <vm>-migrated/`), libvirt
  `<default pool>/<namespace>.<vm>-migrated.qcow2` (and `<vm>-migrated.qcow2`). Check that no
  VM uses them (vSphere: the datastore browser / the VM's disks; libvirt:
  `virsh domblklist --all`), then delete them. From this release on these no longer accumulate: the
  manager deletes a migration's unused landed disk (`LandedDiskCleanup`, see the
  breaking-change table); only disks recorded as `LandedDiskLeftBehind` or kept by
  `cleanupPolicy: Never` stay, and the condition says so.
- **libvirt cloud-init seed directories** `<staging>/virtrigaud-cloudinit-<domain>.<random>`
  (default `/tmp`) of single-host VMs deleted before this release together with a clone of them, or
  whose delete was retried after the domain was undefined. They hold the VM's user-data,
  which may carry secrets. List the domains with `virsh list --all`, check with
  `virsh dumpxml --inactive <domain>` for each that no domain's CD-ROM names the
  directory (for example `virsh dumpxml --inactive <domain> | grep virtrigaud-cloudinit`),
  then remove the directory. From this release on a delete removes them itself.
- **libvirt cloud-init seeds of existing VMs in `/tmp`** (R27). Seeds built before this
  release stay where their domains' CD-ROMs reference them,
  `/tmp/virtrigaud-cloudinit-<domain>.<random>/` or, before that, the per-name
  `/tmp/virtrigaud-cloudinit/<name>/`: the directories are listable by every local user of
  the host, the ISO is usually `0644`, and `user-data`/`meta-data` (plaintext) sit next to
  it. On every libvirt host, as root:
  1. Remove the seed directories no domain references (as in the bullet above).
  2. In every one a domain still references, remove `user-data` and `meta-data` (the ISO
     carries their content), give the provider's SSH user its private copy of the ISO
     **before** making the ISO private, then close the directories. Without the private
     copy, once the VM has started (libvirt then gives the `0600` ISO to the qemu user) the
     provider can no longer read the seed, and a clustered clone of that VM fails:

     ```bash
     SSH_USER=virtrigaud   # the provider's SSH user
     for d in /tmp/virtrigaud-cloudinit-*/ /tmp/virtrigaud-cloudinit/*/; do
       [ -f "$d/cloud-init.iso" ] || continue
       rm -f -- "$d/user-data" "$d/meta-data"
       cp -- "$d/cloud-init.iso" "$d/cloud-init.private.iso"
       chown "$SSH_USER": "$d/cloud-init.private.iso"
       chmod 600 "$d/cloud-init.private.iso" "$d/cloud-init.iso"
       chmod 711 "$d"
     done
     [ -d /tmp/virtrigaud-cloudinit ] && chmod 711 /tmp/virtrigaud-cloudinit
     ```

     A started domain's ISO already belongs to the qemu user, which keeps reading it.
  3. A host that empties `/tmp` at boot, or cleans old files in it (Ubuntu:
     `systemd-tmpfiles`, 30 days), removes these seeds, and the domain then no longer
     starts. Move a VM's seed into the seed directory while the VM is shut off. The new
     directory must be named exactly `virtrigaud-cloudinit-<domain>.<10 letters or digits>`
     (what `mktemp` makes from ten `X`), be owned by the SSH user with mode `0711`, and hold
     the private copy — otherwise `Delete` does not recognise it and leaves it behind, and
     a clone cannot read it:

     ```bash
     SSH_USER=virtrigaud; SEEDS=/var/lib/libvirt/virtrigaud-seeds; DOM=<libvirt domain name>
     OLD=$(virsh domblklist --details "$DOM" | awk '$2=="cdrom" && $4 ~ /cloud-init\.iso$/ {print $4}')
     TGT=$(virsh domblklist --details "$DOM" | awk '$2=="cdrom" && $4 ~ /cloud-init\.iso$/ {print $3}')
     NEW=$(mktemp -d "$SEEDS/virtrigaud-cloudinit-$DOM.XXXXXXXXXX")
     cp -- "$OLD" "$NEW/cloud-init.iso"
     cp -- "$OLD" "$NEW/cloud-init.private.iso"
     chmod 600 "$NEW/cloud-init.iso" "$NEW/cloud-init.private.iso"
     chown -R "$SSH_USER": "$NEW"
     chmod 711 "$NEW"
     virsh change-media "$DOM" "$TGT" "$NEW/cloud-init.iso" --config
     rm -rf -- "$(dirname -- "$OLD")"
     ```

  VMs created after the upgrade are not affected.
- **vSphere transient import-VM files** (`<landing name>.vmx`, `.vmsd`, `.hlog`, `.nvram`,
  about 1.4 KB) in the landing folders of migrations imported under v0.3.11. They are harmless;
  remove them with the datastore browser, never the `.vmdk` files next to them. From this
  release on the import removes them itself, the history log `<landing name>-<hex>.hlog`
  (about 300 bytes) included; imports under v0.4.0-rc2 left that log (removed from rc4 on).
  It is equally harmless and can be removed the same way.

## Required upgrade order

1. **CRDs first.**
   - Helm (default): the chart's pre-upgrade hook applies the CRDs baked into the
     chart with `kubectl apply --server-side --force-conflicts` before the manager
     rolls. This is automatic when `crdUpgrade.enabled: true` (the default) — just
     make sure the chart you're upgrading with was packaged from a current checkout
     (`make gen-helm-crds` if you package it yourself).
   - Manual / GitOps / `crdUpgrade.enabled: false`: apply the CRDs yourself before
     the manager rolls out:
     ```sh
     kubectl apply --server-side --force-conflicts -f config/crd/bases
     ```
   - **Why this matters more than usual this release:** the new manager reads the
     installed `VirtualMachine`, `Provider`, `VMClass` and `VMImage` CRDs at startup
     and on every readiness probe. If the `VirtualMachine` CRD is missing
     `status.boundProvider` or the `spec.providerRef` immutability rule (#341), or
     (for clustered Providers, ADR-0007) the pending-size rule,
     `status.placement.pendingResources` or `status.placement.memoryCeilingMiB`, or
     the `Suspended`/`Unknown` values of `status.powerState`, if the `Provider` CRD
     lacks `status.reportedCapabilities.supportsHonestReconfigure`, if
     the other three lack `spec.consumerNamespaceSelector`, or if the `VMImage` CRD
     lacks `status.providerStatus[].providerUID`, `taskRef` or `sourceDigest` or the
     `Provider` CRD lacks `status.reportedCapabilities.supportsImageArtifactIdentity`
     (ADR-0009), the manager **fails
     readiness** (it does not crash, and it does not stop managing existing VMs —
     but `helm upgrade --wait` will time out, and a rolling update will not proceed
     past the old pod).
   - **Migration into a clustered Provider needs two more schema changes, which the
     readiness check does not verify:** the `VMMigration` status field
     `storageInfo.importTarget.host` and the relaxed `diskID` rule of
     `spec.importedDisk` (an empty `diskID` while `migrationRef` is set). Apply the
     CRDs before the manager, as above; with an older CRD such a migration does not
     work (see the clustered-migration bullet of "Behavior changes that are visible but
     not breaking").
   - **Snapshot revert needs one more schema change, which the readiness check does not
     verify:** `VirtualMachine` `status.snapshotRevert`. With an older CRD the manager
     sends no revert (`SnapshotReverted=False/Unsupported`). The `HostPool` CRD also
     gains printer columns, so `kubectl get hostpools` shows the member-host counts
     once it is applied.
2. **Then grant cross-namespace consumers**, before the new manager runs. If any
   VirtualMachine, VMClone or VMMigration references a `Provider`, `VMClass` or
   `VMImage` in another namespace, set `spec.consumerNamespaceSelector` on those
   objects now. The field exists only once step 1 has applied the new CRDs (an
   older CRD rejects or prunes it), and the old manager ignores it, so setting it
   here changes nothing until the new manager starts — which then finds the
   grants in place. Skipping this step makes the affected VMs stop being managed
   (`ConsumerNotAllowed`, nothing is deleted) until you set it.
   - **Helm:** the chart's pre-upgrade hook applies the CRDs in the same
     `helm upgrade` that rolls the manager, so there is no pause between steps 1
     and 3. Apply the new chart's CRDs yourself first (`kubectl apply
     --server-side --force-conflicts -f <new chart>/crds/`, or `config/crd/bases`
     from the release checkout), set the selectors, then run `helm upgrade` (the
     hook's re-apply is a no-op). If you run `helm upgrade` directly instead,
     set the selectors right after it: the affected VMs are refused only until
     then, and the manager re-drives them within seconds of each grant.
   - See the queries in
     [`docs/cross-namespace-references.md`](cross-namespace-references.md#upgrade-notes).
3. **Then the manager.** Wait for the manager Deployment to report Ready before
   touching provider images — see "Post-upgrade verification" below for the exact
   check.
   - The manager's RBAC changed: `get` on the `virtualmachines`, `providers`,
     `vmclasses` and `vmimages` `.infra.virtrigaud.io` CRDs (#341, consumer grant),
     `get;list;watch` on `namespaces` (#340), and `get;patch;update` on
     `hostpools/status` (the HostPool controller). The Helm chart applies these with the
     manager; a manual/kustomize install must reapply `config/rbac/role.yaml`.
4. **Then the providers**, in any order across hypervisor types, and promptly: until a
   provider reports `supportsImageArtifactIdentity` (ADR-0009), the new manager sends it
   no image prepare, so creates of import-style images through it wait
   (`ProviderLacksArtifactIdentity`; VMs that exist and reference-style images are
   unaffected). Also:
   - **libvirt: do not roll the provider ahead of the manager.** A libvirt provider
     built after #339 expects `CreateRequest.owner` and `ImportDiskRequest.target_vm`
     from the manager. A manager older than #333 talking to a new provider still
     works (bare names throughout); a manager that already sends `owner` (#333+) but
     not yet `target_vm` (#339) makes the new provider name new domains
     `<namespace>.<name>` while migration landing disks still use the legacy name —
     every libvirt-target `VMMigration` fails in that window. Don't start a
     libvirt-target migration while the provider is ahead of the manager; if one is
     between its import and its create when you upgrade, re-run it afterwards.
   - **libvirt: prepare the cloud-init seed directory on every host before rolling any
     libvirt provider** (R27). From this release every create on a host whose
     `/var/lib/libvirt/virtrigaud-seeds` is missing (and cannot be created by the SSH
     user) or not private fails, retryably, with `refusing to stage cloud-init data`; so
     does a clone (single-host or clustered) of a VM that has a seed.
     The command is in the R27 row of the breaking-change table.
   - **Clustered libvirt: roll the provider before you run a migration into it.** The
     manager refuses a clustered target whose provider does not report
     `supports_routed_import` (`ClusteredTargetUnsupported`, before any side effect), so
     an older provider is safe but cannot be a migration target.
   - **libvirt: roll the provider after the manager (honest `Reconfigure`).** A new
     libvirt provider reports `restart_required` and the `Suspended`/`Unknown`
     power states. An older manager ignores `restart_required` — it records a
     change pending a restart as applied, the old behaviour — and writes
     `Suspended` into a `status.powerState` its CRD may not allow. A new manager
     with an older provider is safe but holds every resize of a VM on a
     clustered Provider (`ProviderLacksHonestReconfigure`) until the provider
     reports `supportsHonestReconfigure`, so roll the libvirt providers promptly.
   - **Clustered libvirt: roll the provider after the manager for a host that stops
     answering.** A new provider ends its waits on a host that does not answer before the
     call's deadline and names the host in its `HOST_UNAVAILABLE` answers. An older
     provider behind the new manager is safe: a delete held because its host was
     unreachable before anything ran then names no host in its message (a host lost
     during the delete is still named), and a `Describe` to such a host can still end with `ProviderError` after 30 seconds.
   - **libvirt: grant `VR_DISK_READ` over the pool directory where the provider's SSH user
     cannot read libvirt's `0600` overlays, then roll the libvirt providers (single-host
     and clustered) and the manager.** The providers carry the revert and CD-ROM handling,
     the disk check over nested backing stores, the bounded wait for another call's dial, the
     refusal to delete a disk-only snapshot while a memory snapshot is stored in its overlay
     and the `status.ips` filter of the guest agent's answer; the manager carries the
     backoff of a clustered snapshot create the provider did not perform, the
     `DeleteBlocked` view of a refused snapshot delete and the `status.ips` filter. Roll the
     libvirt providers first: until one is rolled, a delete of a disk-only snapshot still
     orphans a memory snapshot stored in its overlay, so do not delete such a snapshot. An
     older libvirt provider behind the new manager still works with its old behaviour (a
     revert of a disk-only snapshot of a VM with a cloud-init seed bricks the VM; other
     VMs' snapshot, clone and delete are refused after an offline snapshot or a revert; a
     NAT guest without the guest agent shows no address); a newer provider behind an older
     manager is safe, but a refused snapshot delete shows as `ProviderError` with the
     provider's answer only in the event. See
     [Unreadable disk images](libvirt-clones.md#unreadable-disk-images) and
     [Deleting memory and disk-only snapshots of the same
     VM](libvirt-clones.md#deleting-memory-and-disk-only-snapshots-of-the-same-vm).
   - **Clustered libvirt: roll the provider right after the manager (ADR-0007
     A6.2).** The new manager creates a clustered VM only after an
     owner-filtered `ListVMs` through a provider that reports
     `supportsListOwnerFilter`; until the provider is upgraded, every new
     clustered VM waits (`ProviderLacksListOwnerFilter`). Existing VMs are
     unaffected. An older manager with a new provider is safe: it never sends
     the filter.
   - **Proxmox: do not roll the provider ahead of the manager.** The new Proxmox
     provider imports a migration's disk only when the manager marks it as the
     VM's own verified migration disk, and stages imports by
     `ImportDiskRequest.target_vm`; an older manager sends neither, so every
     Proxmox-target `VMMigration` fails (`InvalidArgument`) while the provider is
     ahead.
   - **vSphere: grant the new vCenter privilege before rolling the provider.**
     Without `VirtualMachine.Config.AdvancedConfig`, every `Create` and `Clone`
     starts failing the moment the new provider image is live.
5. **Helm value carry-over:** if you keep custom `values.yaml` overrides across the
   upgrade, use `helm upgrade --reset-then-reuse-values` (Helm 3.14+), not
   `--reuse-values`. `--reuse-values` reuses the **old** chart's defaults, which is
   exactly the bug #336 fixed for the NetworkPolicy key — settings added in the new
   chart are silently missing and old defaults linger.

## Upgrading from a v0.4.0 release candidate

v0.4.0 ships the code of v0.4.0-rc9: from rc9 there is no code, CRD, RBAC or proto
difference, so a `helm upgrade` to the new chart version only re-tags the images. Do not
use `v0.4.0-rc3` images: that tag was never released (its release run was stopped by the
image scan, CVE-2026-63209). Everything else in this guide is written for an upgrade from
v0.3.11; the table below lists only what a release candidate still needs. A row applies if
you are on that candidate or an earlier one: do every row that applies to you, oldest
first. Where a row says **first** or **before**, do that before anything is rolled; then
apply the CRDs, roll the manager and the providers in the usual order. If you are on rc8 or earlier, the libvirt providers
(single-host and clustered) roll **before** the manager (the seed directory of the rc2 row
still comes first), and "promptly after the manager" in the other rows then means "as part of
that roll".

| If you are on | What is still needed |
|---|---|
| rc1 | CRDs first (two optional status fields are new: `VMMigration` `status.storagePVCUID` and `status.storageInfo.importTarget.providerUID`; an old CRD prunes them), then the admission policy and any cross-namespace grants (exempt your restore tool in `admissionPolicy.reservedKeys.exemptUsernames` before the next restore), then the manager, then **all** providers promptly (three capability fields are new, so an older provider gets the conservative path: lost exports and imports are held, landed disks recorded as left behind). Let migrations in `Creating` or `Validating-Target` finish before the manager rolls (`TargetVMNotOwned`). The manager upgrade rolls each provider pod that mounts migration PVCs once, which interrupts an export or import running at that moment. Rolling a vSphere or Proxmox provider back brings back the wrong-disk resize; hold CPU and memory changes of VMs with `spec.disks` while an older image runs. A rolled-back manager can leave an adoption annotated `virtrigaud.io/adopted-id` without `status.id`: delete that `VirtualMachine` (no provider call is made) and let adoption take the VM again. |
| rc2 or earlier | **First, on every libvirt host, create the seed directory.** Apply the CRDs (`VirtualMachine`, `VMSet` and `VMMigration` changed, and `Host` in a description) before the manager. Roll the clustered libvirt provider before you run a migration into it (an older one is refused as a target with `ClusteredTargetUnsupported`). Every provider pod rolls (Go 1.26.10). Review who can create `VMClone`s, and `VMPlacementPolicy` `minDiskSpacePerHost` on clustered Providers. A migration that reached `Ready` with `LandedDiskCleanup` still `Unknown` is closed once on its next reconcile. Seeds in `/tmp` and seed directories of VMs deleted together with a clone are not cleaned up ([Leftovers from earlier releases](#leftovers-from-earlier-releases)). |
| rc4 or earlier | Grant the PVE token `VM.Config.Network`, `VM.Config.Cloudinit` and `SDN.Use` on the bridges your VMs use (skip for the root user or the `Administrator` role) **before** you roll the Proxmox provider, then roll the manager and the Proxmox provider together. Optionally set `PVE_DEFAULT_STORAGE` on Proxmox Providers whose clones can have no storage-backed disk. Review each `VMNetworkAttachment` a Proxmox VM uses, because the bridge and VLAN they name are now applied. |
| rc5 or earlier | **Before the manager rolls,** remove `spec.snapshot.revertToRef` from every `VirtualMachine` and `VMSet` template you do not want reverted (list them with `kubectl get virtualmachines.infra.virtrigaud.io -A -o jsonpath='{range .items[?(@.spec.snapshot.revertToRef)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`). Apply the CRDs first: `VirtualMachine` gains `status.snapshotRevert`, `HostPool` gains printer columns, `VMSet` a description; a manager whose `VirtualMachine` CRD prunes `status.snapshotRevert` sends no revert (`SnapshotReverted=False/Unsupported`), and the readiness check does not verify the field. The manager's role gains `hostpools/status` (the chart and `config/rbac/role.yaml` carry it; a kustomize install must reapply the role). Roll the clustered libvirt provider promptly after the manager. |
| rc6 or earlier | If you reverted a libvirt VM to a snapshot on rc6, **repair it first**: its CD-ROM is on a `cloud-init.<digits>` file in its seed directory and it does not start ([Repairing a VM broken by a v0.4.0-rc6 revert](vm-snapshot-revert.md#repairing-a-vm-broken-by-a-v040-rc6-revert); do not delete its `VMSnapshot` before step 4 of the runbook). The VMs that carry a finished request are listed by `kubectl get virtualmachines.infra.virtrigaud.io -A -o jsonpath='{range .items[?(@.status.snapshotRevert.phase=="Succeeded")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'`. Roll the libvirt providers (single-host and clustered), then take new snapshots, with the VM powered off, of the VMs you want to be able to revert, and delete the old ones: until then a revert to an old snapshot of a VM with a seed is refused with `RevertFailed` and changes nothing. |
| rc7 or earlier | Apply the CRDs (only the description of `status.ips` changed since rc7). On every libvirt host whose provider SSH user cannot read libvirt's `0600` snapshot overlays, grant `VR_DISK_READ` over the pool directory ([Unreadable disk images](libvirt-clones.md#unreadable-disk-images)) or repair the overlays by hand, then roll the libvirt providers, then the manager. Check the grant with `sudo -l -U <ssh user> /usr/bin/qemu-img info -U -f qcow2 --output=json -- <file>`. A `VMSnapshot` that rc7 held in `SnapshotOutcomeUnknown` for this cause stays held until you release it with the `virtrigaud.io/operation-outcome` annotation. |
| rc8 or earlier | No CRD change. Roll the libvirt providers (single-host and clustered) first, then the manager. Until a libvirt provider is rolled, do not delete a disk-only snapshot of a VM that has a memory snapshot. The new delete check reads more overlay files than rc8's checks did: where the provider's SSH user lacks `VR_DISK_READ` over the pool directory (see the rc7 row), deleting a disk-only snapshot is refused, retryably, until it is granted. Rolling a libvirt provider back to rc8 brings back the orphaned memory snapshot. |

## New required privileges and config

- **vSphere:** `VirtualMachine.Config.AdvancedConfig` on the target VM folder and
  resource pool for the provider's vCenter account (#335), and on the VMs
  single-host adoption takes over (their owner stamp is written on adoption).
- **vSphere inventory read:** the provider's vCenter account needs `System.Read`
  (read-only) on the vCenter **root folder, propagated**, with no narrower
  permission below it that removes access. Before attaching a migrated disk in
  place, before a migration import reuses (or a failed one deletes) its landing
  folder, and before deleting a VM with a disk outside its own directory, the
  provider checks that no other VM uses the disk. vCenter silently hides VMs the
  account may not see, so the provider first checks (`FetchUserPrivilegeOnEntities`)
  that it holds `System.Read` on the root folder and the disk's datastore, and
  refuses (`VM_DISK_CHECK_FAILED`, retried) otherwise. It cannot see a "No access"
  set further down the tree: don't set one for this account. The check reads only
  the VMs and templates with files on the disk's datastore (and on the same storage
  under another datastore name), as vCenter lists them in `Datastore.vm` — which
  includes a VM homed on another datastore that has a disk there (verify this on
  your vCenter version in a lab: the simulator the tests use does not model it); an
  inaccessible VM there (no readable configuration) fails the check closed for
  operations on that datastore only.
- **Installer (Helm or kustomize) privileges:** create/update/delete on the
  cluster-scoped `validatingadmissionpolicies` and
  `validatingadmissionpolicybindings` (`admissionregistration.k8s.io`), for the
  reserved-keys policy ([`docs/admission-policy.md`](admission-policy.md);
  Kubernetes 1.30+). The manager itself needs no new permission for it.
- **Manager RBAC:** `get` on the `virtualmachines.infra.virtrigaud.io` CRD (#341) and
  on the `providers`, `vmclasses` and `vmimages` `.infra.virtrigaud.io` CRDs (the
  consumer-grant check);
  `get;list;watch` on `namespaces`, cluster-scoped (#340); `serviceaccounts`
  create/get/list/watch/update/patch/delete, already shipped since v0.3.11-era #297
  for per-provider ServiceAccounts — listed here because a manual/kustomize install
  that has drifted from `config/rbac/role.yaml` should reconcile it now. All of this
  is applied automatically by the Helm chart alongside the manager.
- **libvirt image directories:** `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` (provider pod env
  via `Provider.spec.runtime.env`), a comma- or colon-separated list of allowed host
  directories for `source.libvirt.path` and prepared images. Default:
  `/var/lib/libvirt/images`. Required if your images live elsewhere, in a
  subdirectory, under a session-mode path, or in a non-default storage pool (#334).
- **libvirt SSH login shell:** must be POSIX-compatible (`sh`, `bash`, `dash`, `zsh`,
  `ash`) for the SSH-quoting fix to work correctly (#331).
- **libvirt SSH user can read VM disks:** every VM disk VirtRigaud creates — Create
  (a blank disk too), image copy or download, an imported disk, Clone — is now
  **created** `0640` and given to `libvirt-qemu:kvm` with `chown -h` (never
  `chmod`'ed as root afterwards), instead of world-writable `0777`. **Existing
  disks keep their old mode** (to close one, shut its VM off and run
  `sudo chown -h libvirt-qemu:kvm -- <disk>` and `sudo chmod 0640 -- <disk>`). The
  provider's SSH user must be **a member of the `kvm` group** (or `root`) for the
  reads it does as itself: `GetDiskInfo`, s3/nfs disk export and a full clone's copy.
  The disk in-use checks behind Create, Clone, Delete and snapshots read every
  domain's disk chain on the host, one image at a time, with
  **`sudo -n qemu-img info -U`** when passwordless sudo allows it, and as the SSH
  user otherwise; a disk neither can read fails the check closed (retried, not
  counted toward the circuit breaker) ([`docs/libvirt-clones.md`](libvirt-clones.md)).
  Allow that for the in-use check with the exact regular-expression rule
  `VR_DISK_READ` from [`docs/libvirt-clones.md`](libvirt-clones.md#what-the-copies-run-as-root)
  (`/usr/bin/qemu-img ^info -U -f (qcow2|raw) --output=json -- /var/lib/libvirt/images/[^/ ]+$`, sudo 1.9.10
  or later; adjust the user, the pool directory and the path of `qemu-img`).
  **Replace the `qemu-img info -U *` wildcard an earlier release documented:**
  it let the account add options and read any file's header as root. Never
  allow `qemu-img *`, which would let the account convert or write any file
  as root.
  **To clone or export a VM that has an external snapshot** (its active disk is
  libvirt's `0600 libvirt-qemu` overlay, which even a `kvm` member cannot read),
  the full clone's copy and the s3/nfs export's flatten also need passwordless
  `sudo -n` for the exact `qemu-img convert` (and, on a clustered Provider,
  `timeout ... qemu-img convert`) commands they run. Those can only be
  confined with sudo's regular-expression rules (sudo 1.9.10 or later): the
  command table and a ready-to-adapt `/etc/sudoers.d` snippet are in
  [`docs/libvirt-clones.md`](libvirt-clones.md#what-the-copies-run-as-root) — never
  `qemu-img convert *` or `timeout *`. Without them, those copies run as the
  SSH user as before, and a VM with an external snapshot cannot be cloned or
  exported.
  Keep the pool directory writable only by `root` and the SSH user (or sticky);
  the provider logs a `WARN` otherwise, and **a `root` SSH user is not supported
  on a pool directory other accounts can write**. Disks are written in a private
  `.virtrigaud-write-*` directory next to their final name and renamed into
  place; a symbolic link at a disk's name is refused (`Conflict`). The s3
  export's flattened copy is a private (`0600`), per-export
  `.virtrigaud-export-<vm>.qcow2` inside a private `.virtrigaud-write-*`
  directory next to the source disk, removed with it even when the export
  fails or is cancelled — remove any `.virtrigaud-export-*` or
  `.virtrigaud-import-*` file an earlier release left in a pool directory by
  hand. Follow-up (tracked): least privilege — VM disks `0600 libvirt-qemu`,
  every read through `sudo -n`.
- **libvirt host prerequisite: QEMU with the CVE-2024-4467 fix.** The provider
  runs `qemu-img info` and `qemu-img convert` as root on VM disks. Upgrade
  every libvirt host to a QEMU that fixes CVE-2024-4467 (QEMU 9.0.2, 8.2.6 or
  7.2.13 and later, or your distribution's backport) **before** you allow
  those commands in sudoers. Unfixed versions can be made to open a file named
  in an image's external data file entry, including a `json:` pseudo-protocol
  name. Check with `qemu-img --version` and your distribution's advisory.
  VirtRigaud also refuses a copy whose chain has a data file, and opens every
  disk in the format its domain definition names (never probed). Those checks
  are defence in depth; they do not replace the fix.
- **libvirt pool directory must not be writable by other accounts** (unless
  sticky). A full clone, or an s3/nfs disk export, is now **refused**
  (`FailedPrecondition`; on a clustered Provider, `VM_OPERATION_FAILED`,
  outside the circuit breaker) when:
  - the source's image chain has a symbolic link;
  - an image of the chain lies in a directory that an account other than
    root and the SSH user can write and that is not sticky;
  - the copy would write below such a directory.
  Before, the provider only logged a `WARN`. Check with
  `stat -c '%a %U:%G' /var/lib/libvirt/images` and fix a group- or
  world-writable one with `chmod g-w,o-w`, or set the sticky bit
  ([`docs/libvirt-clones.md`](libvirt-clones.md#clone-files-on-the-host)).
- **libvirt image download limit:** `VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB` (provider
  pod env via `Provider.spec.runtime.env`), the largest image `ImagePrepare` downloads, in
  GiB. Default `256`; an invalid value falls back to the default (logged). A larger source
  is refused as `InvalidSource` (ADR-0009 Slice 4).
- **vSphere image download proxy:** `VIRTRIGAUD_VSPHERE_IMAGE_PROXY` (provider pod env via
  `Provider.spec.runtime.env`), optional. Image downloads ignore `HTTP_PROXY`/`HTTPS_PROXY`
  and connect directly unless this names an `http://` or `https://` proxy. Required only if
  your image servers are reachable only through a proxy.

## Behavior changes that are visible but not breaking

- **TLS 1.2 floor on the webhook and metrics servers** (#327). Transparent for any
  client that already negotiates TLS 1.2+ (the Kubernetes API server and Prometheus
  both do); only matters if something talks to those endpoints with a hard TLS 1.0/1.1
  requirement.
- **NetworkPolicy templates exist now but are opt-in and default-off**
  (`networkPolicy.enabled: false`) (#327, #336). No behavior change unless you
  explicitly enable them, in which case read the chart README's "Network Policies"
  section first — it needs a NetworkPolicy-enforcing CNI and deployment-specific
  tuning (monitoring namespace, DNS, provider namespace, hypervisor egress CIDRs).
- **`VMClass.spec.memory` / `spec.diskDefaults.size` now have CRD upper bounds**
  (below 100Ti and 1Pi respectively) (#338). No plausible existing VMClass is
  affected; this closes an int32 wraparound bug, not a realistic value someone set.
- **Guest-agent-derived VM status fields may start appearing/changing** for libvirt
  VMs (#331) — they were silently no-ops before this release due to the same
  shell-quoting bug.
- **A vSphere `Describe` that hits a transient vCenter error no longer triggers a
  re-create** (#335) — it now retries instead of clearing `status.id`. This is a
  fix, but it means a VM that used to "self-heal" through a spurious re-create during
  vCenter blips now just waits and retries.
- **A libvirt full clone needs a powered-off source** (ADR-0007 Slice 5 lab,
  [`docs/libvirt-clones.md`](libvirt-clones.md#a-full-clone-needs-a-powered-off-source)).
  A `VMClone` of a running (or paused) libvirt VM used to fail on qemu's image
  lock. It now **waits** instead: `Pending`, `Ready=False` reason
  `SourceMustBePoweredOff`, re-checked with a backoff and whenever the source's
  power state changes, and it proceeds once the source is off. The provider's
  refusal (`FailedPrecondition` + `VM_SOURCE_RUNNING`) never counts toward the
  circuit breaker. vSphere clones of running VMs are unchanged. Roll the
  manager with the libvirt provider: an older manager fails such a clone.
- **libvirt disks are opened in the format their domain definition names.**
  A libvirt disk export now pins that format (`-f raw` for a raw disk) instead
  of reading every disk as qcow2. The same applies to `GetDiskInfo`, the disk
  in-use check and Delete. An export or full clone of a disk defined in
  another format (`vmdk`, ...) is refused (`FailedPrecondition`), and so is
  one whose chain names a backing file without its format. `GetDiskInfo`
  reports the definition's format, and refuses an explicit disk path that is
  not one of the VM's own disks on a single-host Provider too (as a clustered
  one already did), instead of reading that file as the SSH user.
- **An nfs VMMigration with `nfs.uid: 0` or `nfs.gid: 0` fails at
  Validating**, whatever its providers (`NFSRootIdentityNotAllowed`; see the
  breaking-change table). A libvirt nfs export runs as root only with the SSH
  user's own identity. One that names another uid or gid is written by the
  SSH user's `qemu-img`, so a VM with an external snapshot cannot be exported
  to NFS that way
  ([`docs/libvirt-clones.md`](libvirt-clones.md#what-the-copies-run-as-root)).
- **libvirt clone and export copies read the source through `sudo -n` when
  allowed** ([`docs/libvirt-clones.md`](libvirt-clones.md#what-the-copies-run-as-root)).
  A full clone, or an s3/nfs disk export, of a VM with an external snapshot
  (whose active disk is libvirt's `0600 libvirt-qemu` overlay) now works when
  the host allows those copies in sudoers; otherwise it fails as before. The
  s3 export's staging file moved into a private `.virtrigaud-write-*`
  directory next to the source disk. `GetDiskInfo` reads the sizes of a VM's
  own disk through the same `sudo -n qemu-img info -U` rule as the in-use
  check, so an export of a snapshotted VM no longer reports 0 sizes.
- **pvc migrations refuse links and special files in the staging PVC**
  ([`examples/migration/README.md`](../examples/migration/README.md#staged-file-hardening-pvc-backend)).
  The vSphere and libvirt providers now resolve every staged-file path beneath
  the PVC mount with no symbolic link in any component, and import only a
  regular single-link file. A `pvc` migration whose PVC holds a symbolic link
  in the `vmmigrations/…` tree (for example `vmmigrations` linked to another
  directory of the same PVC), or a hard link to the staged disk, now fails
  with `refused the staged file` / `refused the staged disk`; replace the link
  with a real directory. A staging PVC on an exotic CSI or FUSE filesystem
  that reports a link count other than 1 for a regular file is refused the
  same way; use a filesystem that reports link counts (or the s3 / nfs
  backend). The libvirt pod-side import no longer accepts `file://` source
  URLs (the manager never sent them). s3 and nfs migrations are unaffected.
- **A libvirt clone's disk is declared qcow2** (`<driver type='qcow2'>`), the
  format it is always written in: the clone of a raw-typed source used to
  keep `type='raw'` and did not boot. Clones made earlier from raw sources
  keep their definition; fix one with `virsh edit` (set the disk's
  `<driver type='qcow2'/>`).
- **`VMClone`, `VMSnapshot` and `VMMigration` status no longer quotes resolver
  or transport errors.** When the manager cannot resolve a `Provider` to a
  client, the status says so with a constant text (`the manager cannot reach the
  provider ...`); the resolver's own error — which can name the provider's TLS
  Secret, its Service address and its runtime phase — is in the manager log only.
  A provider call that failed in the transport (the gRPC connection itself) is
  reported as `the provider could not be reached`; every other provider answer
  copied into these statuses and events is sanitized (credentials in URLs
  removed, one line, at most 256 bytes). A waiting clustered clone
  (`HostUnavailable`, `CloneRetrying`) keeps a constant condition message and
  carries the provider's answer in a `Warning` event of the same reason, recorded
  when the wait begins; a routed `VMSnapshot` create that is retried does the
  same with a `SnapshotCreateRetrying` event. Tooling that parsed provider text
  out of these conditions should read the events or the manager log instead.
- **A single-host clone that provably did nothing is retried instead of
  failed.** A `VMClone` on a single-host Provider whose `Clone` failed without
  doing anything — the request never reached the provider (no connection, or
  the circuit breaker's fast-fail), or the provider answered that it did not
  perform it (its libvirt host unreachable, libvirtd restarting, a disk check
  that could not run) — now waits (`Pending`, `Ready=False` reason
  `CloneRetrying`, one `Warning` event with the provider's answer) and is
  retried with a backoff from 15 s doubling to 5 min; it used to fail for good.
  A clone whose outcome is unknown — the connection dropped after the request
  was sent (a provider pod restarted mid-call), or the call timed out — still
  fails: a single-host clone carries no request token, and sending it again
  could make a second VM. Roll the libvirt provider with the manager: an
  older libvirt provider reports its transient clone errors as terminal.
- **A migration's source snapshot is polled before the export.** A
  task-backed snapshot (vSphere, Proxmox) was assumed complete on the next
  reconcile, so the export could start while it was still being taken. The
  migration now stays in `Snapshotting` (`Snapshotting=True` reason
  `SnapshotInProgress`) until the task finishes; a failed task fails the
  migration (`SnapshotFailed`), and a retry takes a new snapshot instead of
  exporting an unconfirmed one.
- **The first retry of a failed migration waits `retryDelay`.** It used to
  run at once; retry n now waits `retryDelay × backoffMultiplier^(n-1)` after
  the failure it retries (capped at 30 min).
- **A `VMSnapshot`'s retention applies once its VM is gone.**
  `deleteOnVMDelete` and `maxAge` used to be ignored when the referenced VM was
  missing (or had no provider id). Now `maxAge` expires the snapshot whatever
  the VM's state, and `deleteOnVMDelete` deletes it when the VM is gone or
  being deleted — while the VM still exists, through the provider, which also
  lets a libvirt VM whose delete waits for its snapshot metadata go (never
  while the VM is detached with `orphan-on-delete`). A snapshot whose VM is
  missing shows `Ready=False` reason `VMNotFound`; deleting it then records a
  `SnapshotLeftOnHypervisor` Warning instead of claiming there was nothing to
  delete.
- **New `VMSnapshot` status fields: `status.vmUID` and
  `status.boundProvider`** (additive; apply the new CRDs with the manager).
  The operator records the VM's UID and the Provider (namespace, name, UID)
  when it issues a snapshot's create. A VM deleted and re-created under the
  same name, or one that now goes through another Provider, is never sent this
  snapshot's delete — on libvirt and Proxmox a snapshot id is a name, so it
  could have deleted another VMSnapshot's snapshot. Such a VMSnapshot is
  released with a `SnapshotLeftOnHypervisor` Warning, and for retention its VM
  counts as gone. **Existing snapshots** (no `vmUID`) are bound on their first
  reconcile only when it is provable: the VM was created before the
  VMSnapshot object, or more than 30 s before the snapshot's
  `status.creationTime` (the manager's clock); a VM created later — or within
  that margin — is treated as another VM; a snapshot without a creation time
  keeps today's behaviour. These fields are written only by the operator, and
  the manager's readiness requires them in the installed VMSnapshot CRD
  (apply the new CRDs before or with the manager; the manager now also needs
  `get` on `vmsnapshots.infra.virtrigaud.io`, which the chart grants).
- **A migration's source snapshot wait is bounded, and its task errors stay in
  the log.** The source snapshot task is waited for at most
  `spec.options.timeout` (2 h when unset); a retry's snapshot is named after
  its attempt (`<vm>-migration-<uid8>-r<n>`, the VM part cut to fit 40
  characters); a failed snapshot, export or
  import task records "the provider's task failed" in the VMMigration status,
  with the task's own text in the manager log.
- **A clustered clone whose host fails a read before copying anything is
  retried** (`Pending`, `CloneRetrying`, backoff 15 s to 5 min, never counted
  toward the circuit breaker) instead of failing and removing its target; a
  retryable clustered clone answer now backs off instead of retrying every
  30 s.
- **A single-host VM's provider id is no longer lost after a Create.** The
  status write that records it is now a status patch (checked against the VM's
  UID and its recorded id, without a resourceVersion precondition) retried on
  transient errors, so editing the VM — or deleting it — while a long Create
  runs no longer loses the id, and an id recorded meanwhile is never
  overwritten. Before, the next reconcile created a second VM (Proxmox, mock)
  and deleting the VM released it without deleting the hypervisor VM. If the
  record still cannot be written, the manager keeps it in memory — it is not
  created again, and a delete meanwhile deletes it through the provider — and
  records a `Warning` event `VMRecordNotSaved` naming the provider's VM id;
  after a manager restart, the `CreateRequested` marker (breaking changes,
  above) keeps the VM from being created twice or released.
- **A Create that fails on its host backs off.** A Create answered with a
  retryable `VM_DISK_CHECK_FAILED` or `VM_OPERATION_FAILED` is retried with a
  backoff from 15 seconds doubling to 5 minutes (`Ready=False/CreateBackoff`,
  a constant message, one `Warning` event) instead of every 5 seconds.
- **`Ready` no longer carries resolver or transport errors.** When the manager
  cannot get a client for a VM's Provider, `Ready=False/ProviderError` has a
  constant message; when a provider call fails in transport (gRPC
  `Unavailable`, `DeadlineExceeded`, `Canceled`), the describe, power,
  reconfigure, task-check and create conditions say "the provider could not be
  reached or did not answer in time". Both used to embed the error, which can
  name the provider's TLS Secret, Service address and runtime phase — visible
  to consumers of a Provider in another namespace. The cause is in the manager
  log. Provider answers (not found, invalid spec, conflict, ...) are still
  shown, sanitized.
- **A `VirtualMachine` being deleted is reconciled less often.** After its
  deletion is requested it is reconciled only when its generation, annotations
  or finalizers change, or when its hold's backoff expires — no longer on every
  update, including its own status writes (which re-ran a held delete, and its
  provider calls, at once). `force-delete` and `orphan-on-delete` set while the
  delete is held still take effect immediately.
- **`spec.powerState: OffGraceful` no longer loops on a powered-off VM.**
  Providers report a powered-off VM as `Off`, which never matched
  `OffGraceful`, so the VM was sent a graceful shutdown on every reconcile (on
  vSphere an `InvalidPowerState` fault and `Ready=False/ProviderError` every few
  seconds). `Off` now satisfies `OffGraceful` and the VM becomes `Ready`.
- **No VM is created through a `Provider` that is being deleted.** A VM is
  created only through a `Provider` that is not. A `VirtualMachine` not
  created yet (or one whose hypervisor VM is gone and would be re-created) on
  a `Provider` with a `deletionTimestamp` shows `Ready=False` /
  `Provisioning=False`, reason `ProviderDeleting`, and nothing is created:
  point its `spec.providerRef` at another `Provider`, or delete it. It does
  not keep the `Provider` in use. Created VMs are managed as before.

- **Proxmox VMs carry an owner stamp, and a re-sent `Create` re-binds**
  ([`docs/vm-ownership.md`](vm-ownership.md#proxmox)). Every VM the Proxmox
  provider creates, and every clone made for a target `VirtualMachine` with a
  UID, gets a `virtrigaud.owner v1 uid=… namespace=… name=… phase=… sum=…`
  line in its description (Notes). The line is reserved: the provider replaces
  any line that mentions `virtrigaud.owner`, and a hand-edited one makes the
  stamp unreadable, which fails closed. Setting the description needs
  `VM.Config.Options` on the new VM (and on a VM adoption takes over, which
  is stamped too); `PVEVMAdmin` includes it. Restrict
  `VM.Config.Options` on these VMs to the provider's token and trusted admins.
  **Give each Proxmox Provider its own PVE API token.** Providers that share a
  token hold each other's creates while a clone runs, because the in-progress
  guard identifies the provider's own PVE tasks by their user.
  - **Re-bind.** A `Create` sent again after an unknown outcome returns the VM
    stamped with the `VirtualMachine`'s UID, anywhere in the cluster, instead
    of making a second one. While PVE is still cloning or creating a VM for
    that owner, the re-sent `Create` waits (outcome unknown) and starts
    nothing, after a provider restart too.
  - **`ListVMs`** reports `owner_uid` / `owner_stamp_state`. It now lists from
    `/cluster/resources`, so a down node's VMs are listed as `unreadable`
    rather than skipped.
  - **"Gone" means gone everywhere.** A VM is gone only when its VMID is free
    cluster-wide. PVE answers HTTP 500 "does not exist" both for a deleted VM
    and for one on another node. So `Describe` and `Delete` of a VM that
    moved to another node now return a retryable error instead of
    `Exists=false` / success.
  - **Owner-checked `Delete`.** A `Delete` that carries an owner never
    destroys a VM stamped for another owner, a template, or an unstamped VM
    whose PVE name isn't the owner's name. The latter is a reused VMID, or an
    adopted VM with a different name; remove those by hand.
  - **Error classes.** `Create`/`Clone` failures are classified:
    - `InvalidSpec` only for request errors;
    - the new ErrorInfo reason `CREATE_NOT_PERFORMED` when nothing was made.
      Alone for provider-wide failures, which count toward the circuit
      breaker; with `VM_OPERATION_FAILED` for one VM's, which don't;
    - outcome-unknown otherwise.

    A clone-call timeout or 5xx is no longer `InvalidSpec`. A VMID taken
    since `/cluster/nextid` is retried instead of being a `Conflict`. A failed
    `/cluster/nextid` fails the create instead of using a time-derived VMID.
  - **VMs created before the upgrade are unstamped.** They stay bound and are
    deleted as before (with an owner on the request: when their PVE name is
    the `VirtualMachine`'s name, or the name adoption gave it — the sanitized
    PVE name, or `vm-<vmid>`). They are never re-bound by a `Create` and
    can't be looked up by owner.
- **`VirtualMachine` `status.phase` is now set on every VM** (
  [`docs/vm-status-phase.md`](vm-status-phase.md)). Before, the `PHASE` column
  (`kubectl get vm`) was empty on most VMs, because only a few code paths wrote
  it. The manager now derives it on every status write from the VM's state:
  `Pending`, `Provisioning`, `Running`, `Stopped`, `Reconfiguring`, `Deleting` or
  `Failed`. The first reconcile after the manager is rolled fills it in on every
  existing VM (one status write each, then none unless the state changes). No CRD
  schema change (the enum is unchanged; the field description is), so no CRD
  ordering applies. Automation that read `status.phase` must expect every value, and
  can drop workarounds that read `status.powerState` or the `Ready` condition
  instead. A VM that is `Suspended` reports `Running`; one whose power state the
  provider cannot classify (`Unknown`) reports `Failed`; a delete that is held
  reports `Deleting` (the hold is on `Ready`).
- **A single-host libvirt `Delete` removes the VM's own cloud-init seed directory
  reliably** (R24, `internal/providers/libvirt/seed_reclaim.go`). The CD-ROM of a
  clone made before R26 references its source VM's seed ISO, so the delete of either keeps the seed
  while the other is defined; when a VM and its clone were deleted at the same time,
  each kept it and the directory
  `<staging>/virtrigaud-cloudinit-<domain>.<random>` (default `/tmp`) stayed on the
  host. After undefining the domain, the delete now re-checks: it removes the seed
  directory its definition referenced that the delete kept, and any seed directory
  named after exactly that domain that is at least five minutes old (an exact name
  match: `t2s3` and `t2s3c` are distinct), once no defined domain's persistent
  definition references a file in it. A delete retried for a domain that is already
  gone does the same. When a clone is the last domain to reference its source VM's seed,
  deleting the clone therefore removes the SOURCE's seed directory; that is intended.
  The directories it considers are real directories directly inside the staging
  directory (a symlink is never followed or removed); references are compared as
  canonical paths, and anything that cannot be checked is kept. A failure is logged
  at `WARN` and does not fail the delete. In a single-host provider, `Create` and
  `Delete` of one domain name now also run one at a time (`Clone` already did): a
  retried `Create` waits for the delete of the same name, or is answered `domain busy`
  and retried. Clustered providers are unchanged.
- **Clones now boot as a new cloud-init instance: their SSH host keys are
  regenerated** (R26, [`docs/vm-ownership.md`](vm-ownership.md#a-clones-host-identity),
  [`docs/libvirt-clones.md`](libvirt-clones.md#a-clones-cloud-init-identity)). Before,
  a libvirt or vSphere clone presented its source's cloud-init `instance-id`, so
  cloud-init skipped its per-instance modules and the clone kept the source's SSH host
  keys and hostname. Two VMs shared one host key.
  - A **libvirt** clone (single-host and clustered) of a VM with a NoCloud seed now
    gets its own seed directory, named after the clone's domain and removed with the
    clone. A single-host clone's CD-ROM no longer points at its source's seed. With
    R27 that seed directory is in the seed directory (`VIRTRIGAUD_LIBVIRT_SEED_DIR`,
    default `/var/lib/libvirt/virtrigaud-seeds`, which must exist on the host: see the
    R27 row), sealed private, and the source's seed is read through the SSH user's
    private copy `cloud-init.private.iso`. A source created before R27 whose seed was
    made private by hand needs that copy too (see
    [Leftovers from earlier releases](#leftovers-from-earlier-releases)), or its clones
    fail to read the seed once the source has started.
  - A **vSphere** clone's `guestinfo.metadata` is rewritten.
  - In both, the `instance-id` is `virtrigaud-<target VirtualMachine uid>` and the
    hostname is the target's (`VMClone` `spec.customization.hostname`/`domain` when
    set). A `#cloud-config` user-data's top-level `hostname:`/`fqdn:` keys are
    rewritten to the target's.
  - libvirt also applies `spec.customization.userData.cloudInit.inline` as the clone's
    user-data (at most 1 MiB). Before, all customization was ignored with a warning.
  - **Review who can create `VMClone`s.** On libvirt that user-data runs as root on
    the clone's first boot, and the clone's disk is a full copy of the source's disk.
    `create` on `vmclones` in a namespace is therefore **read access to every VM's
    data in that namespace** (files, credentials, keys). It is also access to the
    source's user-data, which every provider copies to a clone. Before upgrading,
    check which subjects can create `VMClone`s (for example
    `kubectl auth can-i create vmclones.infra.virtrigaud.io -n <namespace> --as <subject>`,
    or your RBAC review tooling) and narrow the grant where that is more than intended.
  - User-data keys that keep the source's identity (`ssh_keys`, `ssh_deletekeys: false`,
    `preserve_hostname: true`, `manual_cache_clean: true`) are logged by name.
  - On its first boot a new clone runs every per-instance module of its user-data again
    (`runcmd`, `packages`, `users`, ...), as any new instance does.
  - `/etc/machine-id` is **not** regenerated by cloud-init. Seal the source if clones
    need their own.
  - **Proxmox** is unchanged: PVE already gives a clone a new instance-id and hostname.
  - **Existing clones are unaffected**: they keep the identity they booted with. Single-host
    libvirt clones made by an earlier release keep sharing their source's seed (the
    R24 reclaim still handles them). Clones of VMs without cloud-init are unaffected.
  - A libvirt clone now reads the source's seed ISO on the host with `isoinfo`
    (shipped with `genisoimage`, already required) before copying anything. A host
    without it fails clones of seeded VMs (retried) until it is installed.
  - A vSphere source whose `guestinfo.metadata` cannot be read (unknown encoding, not
    a YAML/JSON mapping) is refused (`InvalidArgument`). The rewritten guestinfo keys
    keep the source's key spelling and encoding.
- **A clustered libvirt provider serves `ImportDisk`, `DeleteLandedDisk` and
  `DeleteStagedDisk`** (R11, [ADR-0007 Addendum B](adr/0007-clustered-orchestrator-provider.md#addendum-b-2026-10-18-migration-into-a-clustered-provider-p3-cold-path)
  slices 1 and 2, `internal/providers/libvirt/routed_import.go`,
  `routed_migration_cleanup.go`). They are routed to the new `target_host_id`
  fields of the requests (`s3` and `nfs` only; `pvc` is refused), and the
  provider now reports `supports_routed_import`, disk import, resend-safe `s3` /
  `nfs` imports, the landed-disk delete and `s3` / `nfs` staged-disk deletes in its
  `GetCapabilities` answer. `supports_routed_import` is not a field of
  `Provider.status.reportedCapabilities`: the manager reads it from the provider when
  a migration needs it. The manager calls them since slice 3 (next row).
  A clustered `Create` that attaches an imported disk in place now takes the
  provider-wide landing lock of its landing name, serialised with the routed
  import and delete of that disk through any host. Do not share one pool
  directory between two Providers (they are not serialised with each other).
  See [Migration into a clustered provider](clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)
  for the host prerequisites (the `default` pool at `/var/lib/libvirt/images`,
  `flock` and `timeout`). Single-host, vSphere and Proxmox providers are unchanged
  (they ignore the new fields and do not report the capability).
- **A `VMMigration` can target a clustered Provider** (R11, ADR-0007 Addendum B
  slice 3; [Migration into a clustered provider](clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b)).
  The target Provider must report `supports_routed_import` (upgrade the clustered
  provider first; an older one is refused at `Validating` with reason
  `ClusteredTargetUnsupported`, before any side effect), the backend must be `s3` or
  `nfs`, and the target VM must be in the VMMigration's namespace. The target
  VirtualMachine is created at `Validating`, with an **empty
  `spec.importedDisk.diskID`** naming the migration (the CRD now accepts an empty
  `diskID` only while `migrationRef` is set — an additive relaxation; **apply the new
  CRDs before the manager**: its readiness check does not verify these changes, an
  older `VirtualMachine` CRD rejects the empty `diskID` (the migration then fails at
  `Validating`, when its target VM cannot be created) and an older `VMMigration` CRD
  prunes `importTarget.host`, which the routed re-send and the landed-disk cleanup
  depend on), and is placed before the source is touched, on a
  host whose `allocatableStorageBytes` holds the source disk; its capacity counts from
  then on. New status: `VMMigration.status.storageInfo.importTarget.host`, the
  `TargetPlaced` condition, and the VirtualMachine reasons `WaitingForImport`,
  `MigrationNotFound` and `ImportedDiskPinned`. Single-host, vSphere and Proxmox
  targets are unchanged (import, then create). **A libvirt Host's
  `allocatableStorageBytes` now reports its `default` pool's free space only** (it
  summed every active pool): it is the input of the disk-space filter, and it changes
  what `Host.status` shows for every libvirt host. Placement filters on it for a
  migration target and, for any clustered VM, through a `VMPlacementPolicy`'s
  `minDiskSpacePerHost`: such a policy now compares against the `default` pool.
  **Experimental**, like all of `topology: cluster`; run in a two-host lab from a
  single-host libvirt source over `s3` and `nfs`. The filter is not a reservation: two migrations can be placed on one host whose
  space holds only one. **Rolling the manager back** with a
  clustered-target migration in flight: finish or delete those migrations first; an
  older manager does not know `WaitingForImport` and would send a `Create` for the
  waiting target. The design expects the clustered provider to refuse that `Create`
  (the disk is not the VM's own verified migration disk), so that nothing is
  destroyed ([ADR-0007 B5](adr/0007-clustered-orchestrator-provider.md#b5-upgrade-and-compatibility));
  this rollback path is not covered by a test.
- **A libvirt Host's `allocatableStorageBytes` is measured fresh** (R31): it is
  the free space available to non-root users in the `default` pool's directory
  (statvfs `f_bavail × f_frsize`, what `df` shows as *Available*), read on every
  inventory sync with `stat -f` under `timeout` (both GNU coreutils), instead of
  libvirt's `pool-info` value, which went stale until a `virsh pool-refresh`.
  The new value excludes root-reserved blocks, so a host may report less than
  before; it follows the filesystem and is the same on hosts that share one pool
  export. A host now reports `0` (the provider log says why), and placement that
  needs disk space avoids it, when its `default` pool is not of type `dir`, `fs`
  or `netfs`, or is not at `/var/lib/libvirt/images` once symbolic links are
  resolved (where creates and imports already refuse it); also when the pool is
  inactive or cannot be measured within its budget (15 s at most). See
  [What `ListHosts` reports per host](clustered-provider-inventory.md#what-listhosts-reports-per-host-libvirt).
- **A clustered libvirt host that stops answering is not always "nothing done"** (R25,
  #422): a routed call whose host is lost mid-call is `HOST_UNAVAILABLE` only when
  nothing ran on the host; after reads only it is not performed, and after a command
  that may change the host was sent it is outcome unknown (for `Clone`, a named
  `SnapshotCreate` with a request token, and a landed-disk removal: the retryable
  reason `VM_OUTCOME_UNKNOWN_RETRY`). A clustered `Delete`, `Power`, `Reconfigure` or
  `Create` whose host drops mid-call therefore shows a retryable provider error
  instead of `HostUnavailable`, and `Power`/`Reconfigure` answered this way are
  re-checked every 30 seconds. A clustered `Delete` whose `destroy` loses the host
  stops and is retried instead of undefining a domain that may still be running.
  Roll the manager with the libvirt providers: an older manager reads the new answers
  as before (retryable). Single-host libvirt is unchanged. See
  [Clustered libvirt: a host that stops answering](provider-call-outcomes.md#clustered-libvirt-a-host-that-stops-answering).
- **Proxmox VMs now land on the bridge, VLAN and static IP their spec names** (R36,
  ADR-0014 Slice 0a, [`docs/proxmox-networks.md`](proxmox-networks.md)). The provider
  used to read none of a VM's networks: every Proxmox VM got `virtio` on `vmbr0` with
  no static IP, and a VM cloned from a template with user-data had its template's
  NICs overwritten with that default. Now the bridge, VLAN tag and model of each
  `VMNetworkAttachment` and the `ipAddress`/`prefix`/`gateway`/`dns` of each
  `spec.networks` entry are applied, a clone keeps its template's NICs (bridge, VLAN,
  options and the MAC PVE gave it) where the spec names none, and an invalid network is
  refused as `InvalidSpec` before a VM is made. **VMs that relied on the accidental
  `vmbr0` default change on their next create or clone; VMs already created are not
  modified.** Check each `VMNetworkAttachment` before you create or recreate a Proxmox
  VM: an attachment's `bridge` is now used (it was ignored), and its `model` (the CRD
  defaults it to `virtio`) replaces the model of a cloned template's NIC.
  - **Newly refused, terminal on a new or re-sent create** (both were silently ignored
    on Proxmox before): an `ipAddress` **without `prefix`** (the CRD allows it and
    vSphere accepts it; PVE's `ip=` needs a prefix), and a **`dns` entry that is not an
    IP address** (a hostname, or a space-separated list; the list is comma-separated).
    Also refused: a bridge, model, VLAN or gateway PVE cannot take.
  - **The manager now sends only the target `Provider`'s block of a
    `VMNetworkAttachment`.** A Proxmox VM reads the attachment's `proxmox` block only: a
    vSphere VLAN or a libvirt bridge in the same attachment no longer reaches the NIC,
    and an attachment with no `proxmox` block sends no bridge, model or VLAN (the VM gets the
    provider's default, `vmbr0` for a new VM). vSphere and libvirt VMs read their own block
    too; before, the last block present won a field several blocks share, such as the
    network name. Roll the manager with the Proxmox provider.
  - **PVE token privileges.** Writing a VM's `netN`, `ipconfigN` and `nameserver` needs
    `VM.Config.Network` and `VM.Config.Cloudinit`, and, for any token that is not root,
    `SDN.Use` on `/sdn/zones/<zone>/<bridge>[/<tag>]` of every bridge a VM is put on. A
    narrowly scoped token that worked before may now get a 403 while the provider
    configures a clone, and the clone is rolled back. Grant them before you roll the
    provider, and use `SDN.Use` ACLs to decide which bridges and VLANs tenants can reach
    ([`docs/proxmox-networks.md`](proxmox-networks.md#permissions-of-the-pve-token)).
  - `firewall`, `rateLimit`, `mtu`, `ipAllocation` and `spec.networks[].macAddress` are
    still not applied to a Proxmox VM, and IPv6 is not reachable through the CRD yet.
    `Describe` still reports no NICs (`ListVMs` reports bridge and MAC).

## Post-upgrade verification checklist

- [ ] `kubectl get pods -n virtrigaud-system` — manager and provider pods Ready.
- [ ] Manager CRD-features check verified:
  ```sh
  kubectl get --raw /metrics -n virtrigaud-system 2>/dev/null | grep virtrigaud_manager_vm_crd_security_features
  # or, without scraping metrics:
  kubectl get crd virtualmachines.infra.virtrigaud.io \
    -o jsonpath='{.spec.versions[?(@.name=="v1beta1")].schema.openAPIV3Schema.properties.status.properties.boundProvider.type}'
  # should print: object
  kubectl get crd providers.infra.virtrigaud.io \
    -o jsonpath='{.spec.versions[?(@.name=="v1beta1")].schema.openAPIV3Schema.properties.spec.properties.consumerNamespaceSelector.type}'
  # should print: object (likewise for vmclasses and vmimages)
  ```
- [ ] No VM is unexpectedly refused for a cross-namespace reference:
  ```sh
  kubectl get virtualmachines -A -o json | jq -r '.items[]
    | select(any(.status.conditions[]?; .type=="Ready" and .reason=="ConsumerNotAllowed"))
    | "\(.metadata.namespace)/\(.metadata.name)"'
  ```
  Each line needs `spec.consumerNamespaceSelector` on the object its `Ready` message
  names — see [`docs/cross-namespace-references.md`](cross-namespace-references.md).
- [ ] No `VMSnapshot` is `Ready` without a snapshot on the hypervisor. Older managers
      could mark a snapshot `Ready` without calling the provider when its `Provider`
      could not be resolved on the first attempt. Such snapshots have no
      `status.creationTime`:
  ```sh
  kubectl get vmsnapshots -A -o json | jq -r '.items[]
    | select(.status.phase=="Ready" and .status.creationTime==null)
    | "\(.metadata.namespace)/\(.metadata.name)"'
  ```
  Delete and re-create each one listed; the new manager does not rewrite them.
- [ ] Audit bound VMs' `providerRef` before the CRD's admission rule locks them in —
      use the query in [`docs/vm-provider-binding.md`](vm-provider-binding.md#upgrade-notes).
- [ ] Audit cross-namespace clone/migration targets and confirm each target namespace
      carries the grant — the query is in
      [`docs/cross-namespace-targets.md`](cross-namespace-targets.md#upgrade-notes).
- [ ] vSphere: create (or reconcile) a test VM and confirm it succeeds — this exercises
      the new `AdvancedConfig` privilege end to end.
- [ ] libvirt: if images live outside `/var/lib/libvirt/images`, confirm
      `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` is set on the affected Providers and that a
      `VMImage`/`VirtualMachine` create against them still succeeds.
- [ ] libvirt: on every host, the seed directory exists, belongs to the provider's SSH
      user and is private: `stat -c '%a %U' /var/lib/libvirt/virtrigaud-seeds` prints `711`
      and the SSH user (`700` only works when qemu runs as the SSH user or root, R27). Create a test VM through each Provider and confirm
      it does not fail with `refusing to stage cloud-init data`.
- [ ] libvirt: check for and clean up orphaned `/tmp/virtrigaud-cloudinit/` seed
      directories on each host, and make the seeds of existing VMs private (see
      [Leftovers from earlier releases](#leftovers-from-earlier-releases)).
- [ ] Clustered libvirt: `kubectl get hosts -A -o custom-columns=NAME:.metadata.name,STORAGE:.status.allocatableStorageBytes`
      shows the free space of each host's `default` pool (it used to sum every active pool).
- [ ] libvirt: before deleting any **pre-upgrade** VM, run
      `virsh domblklist --details <domain>` and compare disk sources across domains on
      that host to rule out a shared legacy disk.
- [ ] Proxmox: list `VMImage`s that import from a URL; any that VMs on a Proxmox
      Provider use must be switched to `source.proxmox.templateID` (URL import is
      refused in this release):
      ```sh
      kubectl get vmimages -A -o json | jq -r '.items[]
        | select(.spec.source.http != null and .spec.source.proxmox == null)
        | "\(.metadata.namespace)/\(.metadata.name)"'
      ```
- [ ] If you enabled `networkPolicy.enabled=true`, render and check the objects before
      relying on them: `helm template virtrigaud charts/virtrigaud --set
      networkPolicy.enabled=true | grep -A2 'kind: NetworkPolicy'`.

## Rollback caveats

- **Rolling the manager back leaves the `Provider` in-use finalizer in place (ADR-0010).** An older manager neither adds nor removes `provider.infra.virtrigaud.io/in-use`, so a `Provider` deleted after the rollback stays `Terminating`. Check that no VM still uses it, then remove the finalizer by hand: `kubectl patch provider <name> -n <namespace> --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]'`.
- **Do not downgrade the `VirtualMachine` CRD while any VM's `status.powerState` is
  `Suspended` or `Unknown`.** An older CRD's enum lacks them: every status write of
  such a VM (conditions, recorded size) is rejected until its power state changes.
  Resume or power off those VMs first, or keep the new CRD.
- **Rolling the libvirt provider back holds clustered resizes.** An older provider
  image does not report `supportsHonestReconfigure`, so the manager stops resizing
  VMs on clustered Providers (`ProviderLacksHonestReconfigure`) until it is upgraded
  again. Single-host resizes go on, with the old (dishonest) results.
- **Rolling the vSphere or Proxmox provider back brings back the wrong-disk
  resize.** An older image grows the root disk to `spec.disks[0].sizeGiB`
  (vSphere) or sends every `spec.disks` size to `scsi0` (Proxmox) on the next CPU
  or memory change, and an older vSphere image sends a change a running VM cannot
  take to vCenter again (`ProviderError`, retried, counted toward the circuit
  breaker). Hold CPU and memory changes of VMs with `spec.disks` while it runs.
  The disk-name records (`virtrigaud.disk.additional.<N>.name`) of vSphere VMs
  created by this release are ignored by an older image and used again after a
  roll forward.
- **Rolling a provider back stops stamping single-host adoptions; rolling the manager
  back can leave an unfinished adoption unbound.** An older provider does not report
  `supportsOwnerTransfer`, so the VMs adopted through it are not stamped (the
  adoption message says so). An older manager does not finish an adoption the new
  one started: a `VirtualMachine` annotated `virtrigaud.io/adopted-id` without
  `status.id` may stay unbound. Delete it (no provider call is made) and let
  adoption take the VM again.
- **Rolling the libvirt provider back stops clustered adoption.** An older provider
  does not report `supportsRoutedAdoption`, so nothing more is adopted from a
  clustered Provider. VMs already adopted stay bound and managed: their domains
  carry their owner stamp.
- **Rolling the libvirt provider back removes the clustered cross-host disk
  guard (ADR-0007 A6.1).** An older clustered provider checks only the create
  or delete host for other domains using a disk, and never answers
  `VM_PREVIOUS_INCARNATION`, so on a shared pool a re-created VM can again
  overwrite a previous incarnation's disk. VMs held as `RestorePending` are
  then retried and treated as ordinary name conflicts. Do not roll back while
  previous incarnations are left on a shared pool.
- **Rolling the clustered libvirt provider back holds new clustered VMs; rolling
  the manager back removes the restore guards (ADR-0007 A6.2).** An older
  provider does not report `supportsListOwnerFilter`, so every clustered VM
  that has not been placed yet waits (`ProviderLacksListOwnerFilter`); bound
  VMs are unaffected. An older manager ignores the
  `infra.virtrigaud.io/placement-uid` marker and runs no pre-schedule check:
  a restored clustered VM is scheduled again, and only A6.1's R2 and R3 stand
  in the way of a second domain (on a host-local pool, not at all). Do not
  restore clustered VirtualMachines while the manager is rolled back. The
  marker annotations it wrote stay on the objects and are harmless.
- **Rolling the manager back with a clustered-target migration in flight.** An older
  manager does not know `WaitingForImport` and would send a `Create` for the waiting
  target; finish or delete those migrations first (the clustered-migration bullet of
  "Behavior changes that are visible but not breaking").
- **Rolling a clustered libvirt provider back stops it being a migration target.** An
  older provider does not report `supports_routed_import`, so new migrations into it fail
  at `Validating` (`ClusteredTargetUnsupported`). A migration whose `ImportDisk` was in
  flight when the provider was replaced has an unknown outcome, and the older provider
  reports no `resend_safe_import_backends`, so the migration is held
  `Ready=False/ImportOutcomeUnknown` for an operator, not failed. Its landed disk is
  recorded as left behind (`LandedDiskLeftBehind`, with a `Warning`) because the older
  provider reports no `supports_landed_disk_delete`: remove a landed disk by hand if one
  is there
  ([ADR-0007 B5](adr/0007-clustered-orchestrator-provider.md#b5-upgrade-and-compatibility)).
- **Rolling a libvirt provider back to before R26 and R27.** The older provider builds
  new seeds in its staging directory (default `/tmp`) again, with the file modes of
  that release, and a clone of a libvirt VM again boots with its source's
  cloud-init identity. Seeds already in `/var/lib/libvirt/virtrigaud-seeds` stay where
  their domains reference them and keep working; remove one by hand after its VM is
  gone, because the older provider is not known to look there.
- **CRD-level changes are one-way in practice.** The `spec.providerRef` immutability
  rule (#341) is a CRD-level CEL admission rule, not manager code — rolling the
  *manager* back to a pre-#341 image does **not** restore the ability to re-point a
  bound VM's `providerRef`, because the apiserver still enforces the rule from the
  installed CRD. Only reapplying an older CRD schema removes it, which also drops
  `status.boundProvider` and the other additive status fields already recorded (not
  recommended — you'd lose the binding record along with the protection).
- **An old manager against the new CRDs silently drops the new protections.** A
  manager older than #341 doesn't know about `status.boundProvider` and won't
  enforce the bound-provider check on VM/VMClone/VMMigration/VMSnapshot calls, even
  though the CRD's admission rule still blocks `spec.providerRef` edits. If you must
  roll the manager back temporarily, treat that window as running without the
  defense-in-depth check, not just "no schema difference."
- **New-format libvirt domains outlive a provider rollback.** A domain created under
  `<namespace>.<name>` (#339) keeps that host name even if you roll the libvirt
  provider image back to an older build — every operation after `Create` addresses it
  by `status.id`, which an older provider still resolves correctly.
- **An old manager ignores `spec.consumerNamespaceSelector`.** Rolling the manager
  back re-opens unrestricted cross-namespace `Provider`, `VMClass` and `VMImage`
  references for the duration. The selectors stay on the objects and take effect
  again when the new manager returns.
- **Rolling a provider back holds image prepares.** A provider image older than
  ADR-0009 does not report `supportsImageArtifactIdentity`, so the manager stops
  sending it image prepares (`ProviderLacksArtifactIdentity`); if the Provider's
  status still shows the capability, the old provider refuses the empty target name
  and nothing is recorded as prepared. Either way it fails closed. Images already
  prepared under the new scheme stay usable. In the stale-capability case the refusal
  is an invalid-argument answer, so the `VMImage` shows `InvalidSource` ("image source
  rejected by provider …: … target name …") — misleadingly blaming the image's owner,
  who may be in another namespace for a shared image. The image is fine: roll the
  provider forward again. Once the Provider controller re-reads the old provider's
  capabilities, the hold becomes `ProviderLacksArtifactIdentity`.
- **Cross-namespace grants and orphan-on-delete have no equivalent on old code.**
  Anything you did with `infra.virtrigaud.io/allowed-source-namespaces` (#340) or
  `virtrigaud.io/orphan-on-delete` (#341) after upgrading is inert if you roll the
  manager back — the old manager doesn't read either.
- **An old manager against a new libvirt provider.** Its prepares carry no identity:
  for this release they are served in deprecated legacy mode — the bare-name file
  `<pool dir>/<VMImage name>.qcow2`, reused by name, so same-named `VMImage`s of different
  namespaces share one file again — logged at `WARN` and counted in
  `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="libvirt"}`.
  Artifacts prepared by identity are left alone and reused when the manager rolls
  forward.
- **An old manager against a new Proxmox provider.** Its Proxmox URL imports carry no
  identity: they are refused like any other, logged at `WARN` and counted in
  `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="proxmox"}`.
  Reference-style Proxmox sources keep working. Rolling the Proxmox provider back does
  not bring back a working URL import: the old one never produced a usable template.
- **An old manager against a new vSphere provider.** Its image prepares carry no
  identity. For this release they are served in deprecated legacy mode — the pre-ADR
  bare-name template, reused by name anywhere in the datacenter — so the cross-tenant
  limitation ADR-0009 closes is back while the manager is rolled back. Each such request
  is logged at `WARN` and counted in
  `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="vsphere"}`.
  Templates prepared under the new scheme are never touched by legacy mode and are
  reused when the manager rolls forward.
- **Rolling the Proxmox provider back stops owner stamping and re-binding.**
  An older provider creates unstamped VMs, never re-binds a re-sent `Create`
  (so it makes a duplicate), lists no `owner_uid`, and ignores the owner on
  `Delete`. Stamps already written stay in the VMs' descriptions and are used
  again when the provider rolls forward. Don't edit or copy those lines in the
  meantime.
- **Rolling the Proxmox provider back stops applying a VM's networks.** An older provider
  goes back to `virtio` on `vmbr0` with no static IP for every NIC, and to overwriting a
  clone's NICs when the request has user-data. VMs created while the newer provider
  ran keep the NICs and IP settings they were given.
- **Rolling the manager back (to a v0.4.0 candidate or v0.3.11) undoes what only it does.**
  `HostPool.status` stops being updated and keeps its last values; `spec.snapshot.revertToRef`
  is a no-op again; the `DisksApplied` and `SnapshotReverted` conditions are neither set nor
  removed, so they go stale on the VMs that carry them (a VM keeps its
  `status.snapshotRevert`); a Proxmox VM stays in phase `Reconfiguring` after a power change
  and every network block of an attachment is sent again; a clustered `VMSnapshot` create the
  provider did not perform ends in the operator hold again (ten re-sends, then
  `SnapshotOutcomeUnknown`); a revert refused for good says "set it again to retry"; a refused
  snapshot delete shows as `ProviderError` with a constant message; and `status.ips` is
  unfiltered for vSphere, Proxmox and the mock provider.
- **Rolling a libvirt provider back brings back what only it fixes.** A revert of a disk-only
  snapshot of a VM with a cloud-init seed bricks the VM again on libvirt 9.9 or later with
  AppArmor (a snapshot taken by this release has an empty CD-ROM in its stored definition, and
  an older provider does not insert the seed again after a revert to it, so the guest would
  boot without its cloud-init datasource); other VMs' snapshot, revert and delete are refused
  after an offline snapshot or a revert; a NAT guest without the guest agent shows no address
  and `primary_ip` can read `127.0.0.1`; deleting a disk-only snapshot orphans a memory snapshot
  stored in its overlay again (a `VMSnapshot` held `DeleteBlocked` by the new provider is then
  retried against the old one, which sends the delete). Rolling the clustered libvirt provider
  back also brings back the unbounded waits on a dead host and the second answer (`Unknown`).
  Nothing needs converting: these releases write no new record or file format, and a memory
  snapshot's metadata that this release deleted is not restored.
- If you do need a full rollback during a maintenance window, go in reverse order
  (providers → manager → CRDs) and expect the caveats above; a partial rollback
  (providers only, or manager only) is safer and sufficient for most of the issues
  this release fixes.

## Also since v0.3.11

Lower-impact items with no required operator action, listed for completeness — see
`CHANGELOG.md` for full detail:

- Dependency/CVE fixes: grpc, x/text, Go toolchain bumps (#298, #303, #299).
- Provider pods run under a dedicated, token-less, binding-less ServiceAccount
  instead of the namespace `default` SA (#297) — automatic on rollout.
- libvirt: capped concurrent virsh forks, `ListVMs` reads domain XML once instead of
  per-domain (#289, #285) — performance only.
- libvirt: hardware-accelerated `<domain type='kvm'>` is used for **new** VMs when
  the host exposes a readable `/dev/kvm`, instead of always emulating with `qemu`
  (#283, contributed by @jing2uo) — existing domains are unaffected.
- ADR-0007 (clustered orchestrator provider) and ADR-0008 (pure-Go libvirt driver +
  in-process SSH transport) landed their early slices behind the experimental
  `topology: cluster` gate and a shadow-compare soak that never serves production
  traffic — no behavior change for single-host providers.
