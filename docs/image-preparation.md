# Image preparation lifecycle

> Design record: [ADR-0005](adr/0005-image-preparation-trigger-model.md). Example:
> [`examples/vmimage-prepare-on-create.yaml`](../examples/vmimage-prepare-on-create.yaml).
> Tracked by [#154](https://github.com/projectbeskar/virtrigaud/issues/154).

A `VMImage` describes *where* a VM image comes from (a vSphere template/OVA, a libvirt
path/URL, a Proxmox template, an HTTP/registry source). For some providers the image must
be **prepared** — downloaded and imported into the provider's storage (a template, a
storage pool, a datastore) — before a VM can be created from it. VirtRigaud does this
**lazily, on first VM create**.

## How it works

1. You apply a `VMImage` and a `VirtualMachine` whose `spec.imageRef` points at it.
2. When the VirtualMachine controller reconciles the VM, **before creating it**, it checks
   whether the referenced image needs preparing on the VM's provider.
3. Preparation runs **only** when the provider both implements image import **and**
   advertises it via `Provider.status.reportedCapabilities.supportsImageImport` (see
   [capability negotiation, #176](https://github.com/projectbeskar/virtrigaud/issues/176)).
   If the provider does not advertise image import, the VM is created by reference exactly
   as before — preparation is skipped, not failed. A provider that imports images but does
   not also advertise `supportsImageArtifactIdentity` is **held** instead: nothing is sent
   to it (see [Prepared-image artifact identity](#prepared-image-artifact-identity)).
4. The prepare may be **synchronous** (the provider imports during the call) or
   **asynchronous** (the provider returns a task ref the controller polls). Either way the
   VM is **not** created until the image is `Ready` on that provider.
5. Once prepared, the result is recorded on the `VMImage` status and subsequent VMs
   referencing the same image on the same provider skip straight to create (idempotent),
   as long as `spec.source` has not changed since.

The image is prepared **only right before a create**: for a VM that has not been created
yet, and before re-creating a VM that no longer exists on its hypervisor. A VM that exists
never sends an image prepare, so nothing about its image — a source the provider now
refuses, a template deleted out of band, `onMissing: Fail`/`Wait` — stops its describe,
power or reconfigure (no provider's `Reconfigure` reads the image). A clustered create
already in flight is re-sent as it was, without a new prepare.

Concurrent reconciles that prepare the same `VMImage` (for the same `spec.source`) through
the same Provider share **one** `ImagePrepare` call within the manager, and a reconcile
that finds a prepare already recorded for that Provider and source (available, or with a
task in flight) issues none.

The VirtualMachine controller is the **single writer** of the prepare-related `VMImage`
status fields; writes are conflict-safe (`RetryOnConflict`) so multiple VMs preparing the
same image on different providers never clobber each other.

## Prepare state is per Provider

A `VMImage` can be shared with other namespaces
([`cross-namespace-references.md`](cross-namespace-references.md)), and two namespaces can
each have a `Provider` with the same name — possibly fronting different hypervisors with
different credentials. Prepare state is therefore recorded per **Provider identity**:

- `status.providerStatus` is keyed by the Provider's `<namespace>/<name>` (for example
  `team-a/vsphere`), and `status.availableOn` lists the same keys. A VM consults **only**
  the entry of the Provider it uses: `team-b/vsphere` having prepared the image never lets
  a VM on `team-a/vsphere` skip its own prepare or be created from `team-b`'s template.
- Each entry records `providerUID`, the UID of the Provider object it was recorded
  through, and is trusted only while that is the Provider's current UID.
- Each entry carries its own `taskRef` for an asynchronous prepare, polled only through
  that Provider. The image-wide `status.prepareTaskRef` is no longer written.

**A re-created Provider** (deleted and created again under the same namespace and name, so
a new UID) is accepted, as for a VM's bound Provider
([`vm-provider-binding.md`](vm-provider-binding.md)), but what its predecessor prepared is
not assumed. With `onMissing: Import` (the default) the prepare is issued again through the
new Provider object: `ImagePrepare` is idempotent on every provider, so an existing
prepared image is confirmed (and its location re-recorded) without a new import, and a
missing one is imported again. A prepare task recorded through the old object is never
polled. With `onMissing: Fail` or `Wait`, which forbid a prepare, an available entry
recorded through the previous Provider object is accepted as is: the new UID is recorded
and the VM gets a `Warning` event `ImagePrepareStateAccepted` (the same namespace owns the
old and the new Provider).

**An entry with no `providerUID`** (migrated from an earlier release, or written out of
band without one) is never trusted: it cannot be tied to any Provider object. With
`onMissing: Import` it is re-validated by the prepare, as above. With `Fail` or `Wait` the
create is held: the `VMImage` gets `Ready=False` with reason `ProviderUIDMissing`, and the
VM `Ready=False` / `WaitingForDependencies`, until the image's owner switches the image to
`onMissing: Import`, so that the controller re-validates the entry through the Provider.
Never write the entry by hand: the VirtualMachine controller is the single writer of
`VMImage.status` (ADR-0005). VMs that already exist are not affected (they never prepare).

**Upgrading from a release that keyed entries by the bare Provider name.** On the first
prepare of each `VMImage` — that is, the first VM **create** that uses it after the
upgrade — the controller migrates that state in one status write:

| Earlier state | After migration |
|---------------|-----------------|
| `providerStatus[<name>]`, and a Provider `<name>` exists in the **VMImage's own** namespace | moved to `providerStatus[<vmimage-namespace>/<name>]` with no `providerUID`, so it is re-validated (the idempotent prepare is issued once through that Provider) before a VM is created from it; under `onMissing: Fail`/`Wait` see "An entry with no `providerUID`" above |
| `providerStatus[<name>]`, and no Provider `<name>` in the VMImage's namespace | dropped; it never satisfies a same-named Provider in another namespace, and that Provider prepares the image itself on first use |
| `availableOn` element `<name>` | rewritten like its entry, kept only while that entry is available |
| `prepareTaskRef` | cleared and never polled — it cannot be attributed to a Provider; the prepare is issued again |

Before cross-namespace sharing required a grant, a VM could reference a `VMImage` in
another namespace with a Provider of its own namespace, so a bare-name entry is not
guaranteed to come from the VMImage's own namespace. That is why a migrated entry is
re-validated rather than trusted. Until a `VMImage` is migrated, its bare-name entries are
simply ignored. If the migration drops every available entry, `status.ready` becomes
`false` with reason `PrepareStateDropped` until the next prepare. An entry without
`providerUID` (for example one written by an older manager) is never trusted: under
`onMissing: Fail` or `Wait` it holds creates (reason `ProviderUIDMissing`) until the image's
owner switches `onMissing` to `Import`, so the controller re-validates it through the
Provider. `VMImage.status` has a single writer, the VirtualMachine controller (ADR-0005): do
not write it by hand, and do not grant `vmimages/status` to anyone else.

The VMImage and Provider CRDs must be upgraded before the manager (as for the other CRD
changes of this release): an older VMImage CRD prunes `providerUID`, `taskRef` and
`sourceDigest`, so no prepared image would be trusted and no asynchronous prepare tracked,
and an older Provider CRD prunes `supportsImageArtifactIdentity`, so every Provider would
look like one without artifact identity. Until the CRDs have these fields, the manager's
readiness check fails and every VM create that needs an image prepare is held without any
provider call (`Ready=False` / `WaitingForDependencies`, "upgrade the CRDs"); VMs that
exist, and images whose source is already present on the provider, are not affected.

## Prepared-image artifact identity

> Design record: [ADR-0009](adr/0009-prepared-image-artifact-identity.md).

A prepared image is an artifact on the hypervisor — a vSphere template, a libvirt pool
file, a Proxmox template. It used to be named after the `VMImage` alone, and every
provider accepted an existing artifact of that name as "already prepared", so two
`VMImage`s with the same name in different namespaces collided, and a tenant allowed to use
a shared Provider could pre-create the artifact another tenant's VMs are cloned from.
Artifacts are now identified by the `VMImage`'s **UID** and a **digest of its
`spec.source`**:

- **The manager sends the identity.** Every `ImagePrepare` carries the `VMImage`'s
  namespace, name and UID, the source digest (below) and the Provider's identity, and an
  **empty** target name. The provider derives the artifact name from the identity (for
  example `team-a.ubuntu-22.04_3c9e1f0a7b2d4e61`), stamps the artifact with it, and reuses
  an existing artifact only when its stamp carries the same UID and digest. A provider
  older than ADR-0009 refuses an empty target name, so it can never import under a bare
  name for this manager.
- **The gate, in order.** (1) A provider that does not import images at all (for example a
  clustered libvirt Provider) is skipped as before: the VM is created by reference. (2) A
  provider that imports images but does not report
  `status.reportedCapabilities.supportsImageArtifactIdentity` is **held**: nothing is sent,
  the `VMImage` entry and — while the image is available on no provider — the image get
  `Ready=False` with reason `ProviderLacksArtifactIdentity`, and the VM
  `Ready=False` / `WaitingForDependencies`. The create is retried every 30 seconds and
  resumes once the Provider reports the capability (upgrade the provider image). (3)
  Otherwise the prepare is sent. Images whose source is already present on the provider
  (`templateName`, a libvirt `path`, …) never need a prepare and are never held.
- **The answer must confirm the identity.** A prepare is recorded only when the provider's
  answer echoes the artifact's stamp with the requested UID and digest. Anything else — no
  echo (an older provider, or one whose reported capability is stale), or an echo for
  another image or source — is not recorded: the create fails with "the provider did not
  confirm the prepared image's identity" and is retried.
- **Asynchronous prepares are confirmed when they end.** When the task of an asynchronous
  prepare ends, successfully or not, the manager sends the prepare again (it is
  idempotent) and records only that answer's confirmed echo; the end of a task is never
  proof that the artifact exists. A failed import is found abandoned by the provider and
  imported again.
- **`status.providerStatus[].sourceDigest`** records the digest of the `spec.source` an
  entry was prepared for. A VM is created from an entry only when it is available, was
  recorded through the Provider's current UID, **and** its `sourceDigest` equals the digest
  of the current `spec.source` — for every source kind. So a changed `spec.source`, a
  `VMImage` switched from `ovaURL` to `templateName`, or an entry written by an earlier
  release (no digest) is never consumed: with `onMissing: Import` the image is prepared
  again for the current source before the create (a new artifact; the old one is left in
  place), and a reference-style source is used as written. VMs that already exist are not
  affected.
- **`onMissing: Fail` or `Wait` and an entry without a digest.** Such an entry cannot be
  matched to `spec.source`, and these settings forbid the prepare that would re-validate
  it: creates are held with reason `SourceDigestMissing` until the image's owner switches
  `onMissing` to `Import`. An entry prepared for another source is simply "not prepared"
  (`MissingOnProvider` / `WaitingForImage`). Never write `VMImage.status` by hand.
- **Conflict.** When an artifact already exists at the derived name but its stamp does not
  match (it was placed out of band, edited, or — with probability 2^-64 — collides), the
  provider refuses to use or replace it. The entry and — while the image is available on
  no provider — the image get reason `ArtifactConflict` (phase `Failed`), the `VMImage`
  gets one `Warning` event `ImageArtifactConflict` when the state changes, the VM is held
  (`WaitingForDependencies`) and retried every 5 minutes. VirtRigaud never adopts,
  overwrites or deletes that artifact: an operator must investigate and remove it. The
  message names only the requesting image's own artifact; who else prepared it is in the
  provider's log only.
- **In progress.** When the artifact is still being imported for the same `VMImage` by
  another request (for example through another Provider that shares the image location),
  the create waits and is retried every 30 seconds. This answer does not count toward the
  Provider's circuit breaker.
- **A re-created `VMImage`** (deleted and created again under the same name) has a new
  UID, so it gets a new artifact and never inherits its predecessor's. A reconcile that
  read the deleted object never records anything on its successor.
- **Metric.** The manager counts every identity prepare outcome in
  `virtrigaud_image_prepare_artifact_total{provider_type, outcome}` with `outcome` one of
  `created`, `reused`, `in_progress` and `conflict` (the confirmation of a Provider's own
  completed asynchronous import is not counted again). Alert on a rising `conflict`.

**Provider support.** Of the providers, only the mock reports
`supportsImageArtifactIdentity` so far. vSphere, libvirt and Proxmox report it once their
ADR-0009 slices land; ADR-0009 makes those slices part of the same release. Until
a provider reports it, import-style images (`ovaURL`, a libvirt `url`, an `http` source)
are held on it with `ProviderLacksArtifactIdentity`; reference-style images and VMs that
exist are unaffected.

**After the upgrade**, the first create for each image and image location finds an entry
without a `sourceDigest` and prepares the image once more, under its new name — one
re-import per image and location. The artifact prepared by an earlier release (under the
bare name) is left untouched on the hypervisor.

**Multi-tenant setups.** Providers that resolve to the same image location (the same
vSphere import folder, libvirt pool directory, or Proxmox storage) share one artifact per
image and source; each re-checks the stamp through its own credentials before reusing it.
The trust boundary is write access to that location, so give each tenant's Provider its own
import location and scope its hypervisor account to it.

The source digest follows ADR-0009's Q7 rule, "location yes, transport no". It covers
every `spec.source` field that says what the image is or where it is placed: URLs and
paths, the expected checksum and algorithm, formats, and the location fields (libvirt
`storagePool`, Proxmox `storage` and `node`). It excludes the fields that only change how
the bytes are fetched, or by whom: `http.timeout`, `http.headers`,
`http.authentication`, `registry.pullSecretRef` (a credential reference) and
`vsphere.providerRef` (which Provider imports the image), and the `user:password@` part
of every URL (`http.url`, `libvirt.url`, `vsphere.ovaURL`). Changing an excluded field
never causes a re-import. `spec.metadata`, `spec.distribution`, `spec.prepare` and
`spec.consumerNamespaceSelector` are outside the digest.

A URL's query string is part of the digest, because it may select the content. A
presigned or token-bearing query string therefore gets a new artifact (one re-import)
each time it is rotated. Keep credentials out of URLs: use `http.authentication` (a Secret
reference) instead.

## `spec.prepare.onMissing`

`VMImageSpec.prepare.onMissing` gates the behaviour when the image is not yet prepared on a
provider:

| Value | Behaviour |
|-------|-----------|
| `Import` (default) | Prepare the image on the provider, then create the VM. |
| `Fail` | Do **not** prepare; record `Ready=False` / `Phase=Failed` on the `VMImage` and hold the VM (it will not be created until the image is prepared out of band). |
| `Wait` | Do **not** prepare; record `Ready=False` / `Phase=Pending` and hold, waiting for an out-of-band preparer. |

Under `Fail` and `Wait`, only an entry prepared for the current `spec.source` counts as
prepared, and one recorded without a `sourceDigest` holds creates with reason
`SourceDigestMissing` (see [Prepared-image artifact identity](#prepared-image-artifact-identity)).

## `VMImage.status` fields you will see

```bash
kubectl get vmimage <name> -o wide
kubectl get vmimage <name> -o yaml | yq '.status'
```

| Field | Meaning |
|-------|---------|
| `status.phase` | `Importing` while a prepare is in flight, `Ready` once prepared, `Failed`/`Pending` for `onMissing: Fail`/`Wait` holds, `Failed` for an artifact conflict or a rejected source, `Pending` while a Provider lacks artifact identity. |
| `status.ready` | `true` once the image is available on **at least one** provider (the OR across providers). A prepare in flight on one provider does not clear it while the image is available on another. |
| `status.availableOn` | The providers the image is prepared on, as `<namespace>/<name>` (the `Providers` print column). |
| `status.providerStatus["<namespace>/<name>"]` | Per-provider truth, keyed by the Provider's identity: `available`, `providerUID` (the Provider object it was recorded through), `taskRef` (an in-flight async prepare on that Provider), `sourceDigest` (the digest of the `spec.source` the entry was prepared for — or its in-flight task prepares — `sha256:<64 hex>`, as confirmed by the provider's artifact stamp; an entry is used only while it equals the current digest), plus the provider-specific `id`/`path`/`message`/`lastUpdated`. See [Prepare state is per Provider](#prepare-state-is-per-provider) and [Prepared-image artifact identity](#prepared-image-artifact-identity). |
| `status.prepareTaskRef` | Deprecated and no longer written; a value left by an earlier release is cleared. |
| `status.lastPrepareTime` | When the last prepare was triggered/completed. |
| `status.conditions` | `Ready` and `Importing` conditions with reasons (`Importing`, `Prepared`, `MissingOnProvider`, `WaitingForImage`, `InvalidSource`, `PrepareStateDropped`, `ProviderUIDMissing`, `SourceDigestMissing`, `ProviderLacksArtifactIdentity`, `ArtifactConflict`). |

`status.ready` and `status.availableOn` record where the image was prepared; after a
`spec.source` change they keep listing the Providers it was prepared on for the previous
source until the next create re-prepares it there, and a VM is never created from such an
entry.

Status never contains secrets — only provider ids/paths/messages.

## What is and isn't done

PR-5 wired preparation to **run** through CRs and reflect it in status (`Importing` →
`Ready`). **PR-6 (#214) closes the loop**: the provider now returns *where* it placed the
prepared image (`prepared_image_id` / `prepared_image_path`), the controller stamps that
onto `status.providerStatus["<namespace>/<name>"].{id,path}`, and `Create` **consumes** it — cloning the
prepared template (vSphere/Proxmox) or copying the local prepared pool file into the VM's own
disk (libvirt) instead of re-resolving (and re-downloading) the original source. A second VM from the same prepared
image therefore skips the re-download. When an image is not yet prepared/available on the
target provider, `Create` falls back to the original by-reference source resolution
unchanged (no regression). The `ImagePrepare` RPC change is wire-compatible (the `task` ref
stays at proto field 1), so manager and providers must roll together but no CRD spec field
changed. See ADR-0005 "Out of scope" for the original PR-6 framing.

## libvirt image paths (`source.libvirt.path`)

A libvirt `VMImage` can name an image file that already exists on the hypervisor host.
Because `VMImage` (and `VirtualMachine.spec.importedDisk`) are namespaced objects that
tenants may create, the libvirt provider **confines** every such path on the host that
will use it (in clustered mode, the VM's scheduled target host) before touching it:

1. **Shape** (also enforced by the CRD): absolute, no `..` segment, no segment starting
   with `-`, no control characters, at most 4096 bytes.
2. **Canonical path**: the path is resolved on the host with `realpath -e`, so symlinks and
   `..` cannot escape. Only the resolved path is used afterwards.
3. **Allowed directory**: the resolved file must sit **directly** inside one of the
   provider's allowed image directories (subdirectories are not included).
4. **Not a VirtRigaud artifact**: names ending in `-disk.qcow2`/`-disk` (VM disks),
   `-migrated.qcow2` (migration landing disks), cloud-init seed ISOs, staging files, and
   dotfiles are refused.
5. **Regular, non-empty file** — never a device, directory, FIFO or socket.
6. **Not in use**: the file must not be a disk, backing file, or shared directory of
   **any** domain defined on the host (VirtRigaud-managed or not). Each disk's backing
   chain is read from the images themselves (`qemu-img info -U --backing-chain`), so the
   base images of shut-off VMs count too. If the check cannot complete (a domain
   definition, a volume path or a backing chain cannot be read), the create fails
   closed with a generic, retryable error; the detail is only in the provider log.
7. **Self-contained**: `qemu-img info` must show no backing file, no external data file,
   and no VMDK extent outside the file; accepted formats are qcow2, raw, vmdk, vpc, vhdx
   and vdi. Downloaded (`url`) images get the same header check before conversion.

A base image is always **copied** into the VM's own `<domain>-disk.qcow2` (for a new VM
the domain is `<namespace>.<name>`, see
[`libvirt-domain-ownership.md`](libvirt-domain-ownership.md#domain-names)); it is never
attached in place, so VMs never share a disk and deleting a VM never deletes the image.
The copy never replaces a file at that path that another domain uses.
The only disk attached in place is a migration's landing disk, and only when the manager
can prove it: `spec.importedDisk.migrationRef` must name a `VMMigration` in the VM's own
namespace that targets this VM (name and namespace) and whose `status.diskInfo.targetPath`
equals the disk path. The provider additionally requires the file to be
`<domain>-migrated.qcow2` — the name the migration import derived for this VM with the
same naming rule, e.g. `team-a.web-migrated.qcow2` — directly in the `default` pool
directory and unused by any domain. Any other `spec.importedDisk` is treated as a base
image (confined, then copied or refused). Migrated disks landed by ImportDisk get the
same header check before conversion.

> **Upgrade warning — legacy shared disks.** Before this change a path or prepared image
> in the pool directory was attached in place, so several VMs created from the same image
> may **share one disk file**, and deleting any of them deletes that file for all of
> them. Before deleting a VM created by an earlier release, compare
> `virsh domblklist --details <domain>` across domains and look for duplicate sources.
> A prepared image that was attached this way is refused as a template (it is a live VM
> disk); create a `VMImage` under a new name to prepare a fresh copy.

A rejected path is a non-retryable `InvalidArgument`: the VM gets
`Provisioning=False` with reason `ValidationError` (rechecked every 30 seconds rather than
retried every 5 seconds), and an image rejected during preparation gets
`status.providerStatus["<namespace>/<name>"].message` plus, while it is not Ready on any provider,
`phase: Failed` and a `Ready=False` condition with reason `InvalidSource`. Messages never
reveal the target of a symlink, the allowed directories, or other VMs; "does not exist" is
only reported for paths inside an allowed directory.

### Configuring the allowed directories

Set `VIRTRIGAUD_LIBVIRT_IMAGE_DIRS` on the provider pod through the Provider's
`spec.runtime.env`. It is a comma- or colon-separated list of absolute directories; when
unset or blank it defaults to `/var/lib/libvirt/images` (the `default` storage pool, where
`ImagePrepare` also writes prepared images). System locations (`/`, `/etc`, `/dev`,
`/proc`, `/sys`, `/boot`, `/root`, `/run`, `/tmp`, `/var/tmp`, `/usr`, `/var/log`,
`/var/lib/libvirt/qemu`, `/var/lib/libvirt/images/cloud-init`, kubelet/container runtime
state, ...) are refused, and the provider does not start with a malformed value.

```yaml
spec:
  runtime:
    env:
      - name: VIRTRIGAUD_LIBVIRT_IMAGE_DIRS
        value: "/srv/golden-images,/var/lib/libvirt/images"
```

Setting the variable **replaces** the default. If you use URL-sourced (prepared) images,
keep the directory of the pool `ImagePrepare` writes into (normally
`/var/lib/libvirt/images`) in the list. On multi-tenant hosts prefer a dedicated
directory that only administrators write to; every image in an allowed directory is
usable by anyone who can create a `VMImage` for that provider. Session-mode (`/session`)
providers, and images kept in subdirectories, need explicit configuration.
