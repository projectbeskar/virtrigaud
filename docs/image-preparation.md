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
`VMImage.status` (ADR-0005). VMs that already exist are not affected (they never prepare);
a VM re-created because it no longer exists on its hypervisor is a create, and is held too.

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
  another image or source — is not recorded: the entry and — while the image is available
  on no provider — the image get reason `ArtifactNotConfirmed` (nothing of the answer is
  shown), the create is held, and the prepare is sent again after 30 seconds, doubling to
  at most 5 minutes, however many VMs use the image.
- **Asynchronous prepares are confirmed when they end.** When the task of an asynchronous
  prepare completes, the manager sends the prepare again (it is idempotent) and records
  only that answer's confirmed echo; the end of a task is never proof that the artifact
  exists.
- **A failed asynchronous import backs off.** When the task fails, the entry records why
  (reason `ImportFailed`, the provider's detail sanitized) and the create is held. The
  prepare is sent again after one minute, doubling with each consecutive failure to at most
  30 minutes; the provider then finds the failed artifact abandoned and imports it again.
  A source that always fails (a 404, a checksum mismatch) is therefore re-downloaded at
  most once per window. Fix the source (a new `spec.source` starts afresh) or wait.
- **`status.providerStatus[].sourceDigest`** records the digest of the `spec.source` an
  entry was prepared for. A VM is created from an entry only when it is available, was
  recorded through the Provider's current UID, **and** its `sourceDigest` equals the digest
  of the current `spec.source` — for every source kind. So a changed `spec.source`, a
  `VMImage` switched from `ovaURL` to `templateName`, or an entry written by an earlier
  release (no digest) is never consumed: with `onMissing: Import` the image is prepared
  again for the current source before the create (a new artifact; the old one is left in
  place), and a reference-style source is used as written. VMs that already exist are not
  affected — except a VM that no longer exists on its hypervisor: re-creating it is a
  create, so its image is prepared (or held) first, like a new VM's.
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
  the create waits and is retried every 30 seconds; the entry records when the wait began.
  This answer does not count toward the Provider's circuit breaker. If the wait lasts
  longer than `max(2 × spec.prepare.timeout, 2h)` — the bound after which a provider
  treats an unfinished artifact as abandoned and imports it again — the entry says the
  other prepare may be stuck, the image gets reason `ArtifactPrepareStalled` (while it is
  available on no provider), and the `VMImage` gets one `Warning` event
  `ImageArtifactPrepareStalled`.
- **Provider messages.** The provider's detail in these messages (a rejected source, a
  conflict, a failed import) follows a message of the manager's own, with URL userinfo
  removed, control characters replaced and a 256-byte cap: a shared `VMImage`'s status and
  events are read in other namespaces too.
- **A re-created `VMImage`** (deleted and created again under the same name) has a new
  UID, so it gets a new artifact and never inherits its predecessor's. A reconcile that
  read the deleted object never records anything on its successor.
- **Metric.** The manager counts every identity prepare outcome in
  `virtrigaud_image_prepare_artifact_total{provider_type, outcome}` with `outcome` one of
  `created`, `reused`, `in_progress` and `conflict` (the confirmation of a Provider's own
  completed asynchronous import is not counted again). Alert on a rising `conflict`.

**Provider support.** The mock, the vSphere provider, the libvirt provider and the Proxmox
provider report `supportsImageArtifactIdentity`. vSphere names, stamps and verifies its
prepared templates by identity (see
[vSphere: identity-safe prepared templates](#vsphere-identity-safe-prepared-templates)).
libvirt names, stamps and publishes its prepared images by identity (single-host providers;
see [libvirt prepared images](#libvirt-prepared-images-sourcelibvirturl)). Proxmox reports
it because it prepares nothing by name: in this release it refuses every URL import with
`InvalidSpec`, so such an image gets reason `InvalidSource` rather than a hold (see
[Proxmox image sources](#proxmox-image-sources)). Until a provider reports it, import-style images (`ovaURL`, a
libvirt `url`, an `http` source) are held on it with `ProviderLacksArtifactIdentity`;
reference-style images and VMs that exist are unaffected. A VM re-created because it
vanished from its hypervisor counts as a create and is held too.

**After the upgrade**, the first create for each image and image location finds an entry
without a `sourceDigest` and prepares the image once more, under its new name — one
re-import per image and location. The artifact prepared by an earlier release (under the
bare name) is left untouched on the hypervisor.

**Multi-tenant setups.** Providers that resolve to the same image location (the same
vSphere import folder, libvirt pool directory, or Proxmox storage) share one artifact per
image and source; each re-checks the stamp through its own credentials before reusing it.
The trust boundary is write access to that location: a principal who can write there can
change an artifact's content, and — because a `VMImage`'s UID is readable by anyone who can
read the `VMImage`, and an image is prepared only at the first VM create — can plant a
matching artifact before the first prepare, which is then reused. **Only the Provider's own
hypervisor account may write the image location.** Give each tenant's Provider its own
import location, scope its hypervisor account to it, and grant nobody else write access to
it (see the vSphere section for the exact vCenter rights).

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
| `status.phase` | `Importing` while a prepare is in flight, `Ready` once prepared, `Failed`/`Pending` for `onMissing: Fail`/`Wait` holds, `Failed` for an artifact conflict, a rejected source, a failed import or an unconfirmed answer, `Pending` while a Provider lacks artifact identity or a stalled wait, `Importing` while waiting for an artifact another request prepares. |
| `status.ready` | `true` once the image is available on **at least one** provider (the OR across providers). A prepare in flight on one provider does not clear it while the image is available on another. |
| `status.availableOn` | The providers the image is prepared on, as `<namespace>/<name>` (the `Providers` print column). |
| `status.providerStatus["<namespace>/<name>"]` | Per-provider truth, keyed by the Provider's identity: `available`, `providerUID` (the Provider object it was recorded through), `taskRef` (an in-flight async prepare on that Provider), `sourceDigest` (the digest of the `spec.source` the entry was prepared for — or its in-flight task prepares — `sha256:<64 hex>`, as confirmed by the provider's artifact stamp; an entry is used only while it equals the current digest), plus the provider-specific `id`/`path`/`message`/`lastUpdated`. See [Prepare state is per Provider](#prepare-state-is-per-provider) and [Prepared-image artifact identity](#prepared-image-artifact-identity). |
| `status.prepareTaskRef` | Deprecated and no longer written; a value left by an earlier release is cleared. |
| `status.lastPrepareTime` | When the last prepare was triggered/completed. |
| `status.conditions` | `Ready` and `Importing` conditions with reasons (`Importing`, `Prepared`, `MissingOnProvider`, `WaitingForImage`, `InvalidSource`, `PrepareStateDropped`, `ProviderUIDMissing`, `SourceDigestMissing`, `ProviderLacksArtifactIdentity`, `ArtifactConflict`, `ArtifactNotConfirmed`, `ImportFailed`, `ArtifactPrepareStalled`). |

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

## Proxmox image sources

**Proxmox URL image import is not supported in this release.** Use a template that already
exists on Proxmox VE and reference it by VMID:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22
spec:
  source:
    proxmox:
      templateID: 9000   # an existing PVE template (template=1)
```

| Source | Behaviour |
|--------|-----------|
| `source.proxmox.templateID` | Unchanged. Nothing is prepared: the VM is cloned from that template (a full clone). |
| `source.proxmox.templateName` | Unchanged. `Create` clones only by VMID, so a name that is not a number fails at create time: use `templateID`. |
| `source.http.url` | **Refused.** `ImagePrepare` returns `InvalidSpec` ("Proxmox URL image import (source.http) is not supported in this release ..."), with or without an identity, before any PVE call. While the image is not Ready on another Provider, the `VMImage` gets `phase: Failed` and `Ready=False` with reason `InvalidSource`, and the VM is not created. |

Why: the earlier URL import only downloaded the image file into a PVE storage, named after
the `VMImage`. It never turned it into a template VM, and `Create` needs a template VMID, so
no VM could ever be created from it. It also reported any existing template with the same
name, whoever created it, as "already prepared" (the cross-tenant defect
[ADR-0009](adr/0009-prepared-image-artifact-identity.md) closes). Rather than keep a path
that pretends to work, the provider fails closed (ADR-0009 D10). ADR-0009 Slice 6 adds a
real import: a stamped template VM in the provider's own PVE pool, found only by its tag and
stamp and never by name.

To prepare an image yourself: import the cloud image into a VM disk on the node (for example
`qm importdisk`), convert the VM to a template (`qm template <vmid>`), and put that VMID in
`source.proxmox.templateID`. Give each tenant its own templates, or a PVE pool that only
its Provider's account can read, because a reference-style source reaches every template the
Provider's account can read.

The Proxmox provider advertises `supportsImageArtifactIdentity`: it never reuses a prepared
image by bare name, because it prepares none. Without that capability a new manager would
hold `source.http` VMImages on Proxmox with a misleading "upgrade the provider image"
reason instead of reporting `InvalidSource`.

**A manager older than this release** still sends Proxmox URL imports without an identity.
They get the same `InvalidSpec`. Each such request is also logged at `WARN` ("deprecated:
image prepare without identity from an older manager ...") and increments
`virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="proxmox"}`. The
Proxmox provider serves that counter at `/metrics` on its health port (as the libvirt
provider does). A non-zero value means a manager must be upgraded.

## vSphere: identity-safe prepared templates

> Design record: [ADR-0009](adr/0009-prepared-image-artifact-identity.md), Slice 3. The
> vSphere provider advertises `supportsImageArtifactIdentity`.

When the manager sends the `VMImage` identity (its UID, namespace and name) and the
digest of its `spec.source`, the vSphere provider prepares a `source.vsphere.ovaURL`
image as follows. Only an `ovaURL` is ever prepared: `templateName` and `contentLibrary`
reference existing objects and are used by reference. A source that sets `ovaURL`
together with `templateName` or `contentLibrary`, an `ovaURL` that is not `http(s)`, or an
`ovaURL` naming a bare `.ovf` (it cannot carry its disks: publish an `.ova`) gets
`InvalidSpec`.

**The package is read from the download only.** Every `<References><File ovf:href>` of the
OVF must be a plain file name (no path separator, no `.`/`..`, no URL scheme, no glob
character, at most 255 bytes) and a member at the root of the OVA; otherwise the image gets
`InvalidSpec` before vCenter sees the descriptor. The provider never opens a file on its own
filesystem named by an OVF. (Before this, a bare `.ovf` whose references named, for example,
the provider's mounted vCenter credentials made the provider upload that file into the
template. This applies to requests from an older manager too: a bare `.ovf` that references
any file is refused; one that references none still imports.)

**The download is restricted.** The provider refuses sources at, or redirecting to,
loopback, link-local (including `169.254.169.254` cloud metadata), unspecified and
multicast addresses — checked on every connection, after DNS — follows at most 3 redirects,
bounds the connect, TLS-handshake and response-header waits, and downloads at most
`VIRTRIGAUD_VSPHERE_IMAGE_MAX_DOWNLOAD_GIB` GiB (default 256, 1–16384; an invalid value logs
a warning and keeps the default). Set it on the provider pod through the Provider's
`spec.runtime.env`. Private (RFC 1918) addresses stay allowed. With an HTTP(S) proxy
configured for the provider, the proxy resolves named targets: restrict its egress too.
**Keep credentials and tokens out of `ovaURL`.** A vSphere OVA source has no separate
credential field, so host images where the provider can fetch them without a secret in the
URL; a presigned query is hashed into the source digest (a rotation re-imports), and the
provider shows a URL only as scheme, host and last path segment, never its user-info, the
rest of its path, or its query.

**Name.** The template is named `<namespace>.<name>_<16 hex>`, at most 80 characters; the
`<namespace>.<name>` part is cut when it is too long. The hex part is a hash of the
`VMImage` UID and the source digest, so a re-created `VMImage` or a changed `spec.source`
gets a new template instead of an old one. The name always contains `_`, which no
Kubernetes name can, so it never equals a bare `VMImage` name or a VM name.
Example: `team-a/ubuntu-22.04` → `team-a.ubuntu-22.04_3c9e1f0a7b2d4e61`.

**Location: the import folder.** The template is looked up, created and reused only in
the Provider's import folder: `spec.defaults.folder`, or the datacenter's VM folder when
that is empty. A configured folder that does not resolve (missing, ambiguous, outside the
default datacenter's VM folder, or a vCenter error) fails the prepare, which the manager
retries: the provider never falls back to another folder, so every retry uses the same
location. A same-named template in any other folder is neither reused nor a conflict.
`prepared_image_id` (and so
`status.providerStatus[...].id`) is the template's **absolute inventory path**, for
example `/DC0/vm/images/team-a.ubuntu-22.04_3c9e1f0a7b2d4e61`, and `Create` clones exactly
that template, never a same-named one elsewhere.

**Multi-tenant setups: give each tenant's Provider its own import folder**
(`spec.defaults.folder`) and scope that Provider's vCenter account to it. Providers that
resolve to the same folder share one template per image and source; that is intended for
Providers trusted with each other's images.

**Trust assumption: nobody but the Provider's vCenter account may create, move, rename,
reconfigure (`VirtualMachine.Config.AdvancedConfig`) or mark as template
(`VirtualMachine.Provisioning.MarkAsTemplate`) VMs in the import folder.** The stamp proves
provenance only against principals who cannot do that. A `VMImage`'s UID is readable by
anyone who can read the `VMImage`, and an image is prepared only at its first VM create, so
a principal holding those rights in the folder can plant a *complete, matching* template
first — and it is **reused**. The same principal can make the provider destroy a powered-off
VM they stamp as this image's abandoned import and move into the folder. vCenter folder
permissions propagate to child objects, and the import folder is `spec.defaults.folder`,
the folder the Provider's VMs are created in as well, so rights delegated to VM owners on
that folder (or a parent) reach the import folder too. Use a **dedicated import folder**
with a restrictive ACL: the Provider's account alone holds those rights on it, and no
propagating grant from a parent folder gives them to anyone else. (A separate
`defaults.imageFolder`, so VMs and templates need not share a folder, is a tracked
follow-up.)

**Stamp.** The template carries its provenance in ExtraConfig, written into the import
spec before `ImportVApp`, so the object carries it from the moment it exists:

| Key | Value |
|---|---|
| `virtrigaud.image.stampversion` | `1` |
| `virtrigaud.image.uid` | The `VMImage` UID. Authoritative. |
| `virtrigaud.image.sourcedigest` | `sha256:<64 hex>`, the source digest. Authoritative. |
| `virtrigaud.image.namespace`, `virtrigaud.image.name` | The `VMImage` namespace and name. Informational. |
| `virtrigaud.image.preparedby` | The Provider, as `<namespace>/<name>/<uid>`. Informational. |
| `virtrigaud.image.preparedat` | When the import started (RFC 3339). Used to age an unfinished import. |

The stamp holds no URL, header or secret. An OVF cannot forge it: every `virtrigaud.*`
ExtraConfig key an OVF carries is removed from the import spec first, and the real stamp is
added afterwards. `govc vm.info -e <template> | grep virtrigaud.image` shows it. A VM
created or cloned from a prepared template never carries the image stamp: `Create` and
`Clone` clear `virtrigaud.image.*` on the new VM.

**What an existing object at the name means.** The provider reuses an object only when it
is a template **and** its stamp carries the request's `VMImage` UID and source digest:

| Found at the name, in the import folder | Result |
|---|---|
| Nothing | Import: download, verify the checksum if the image pins one, `ImportVApp`, then mark as template |
| A template whose stamp matches | Reused (`artifact.reused=true`), no download |
| A matching **unfinished** import (not a template yet) that is live | In progress ("still being prepared"; see [In progress](#prepared-image-artifact-identity)) |
| A matching unfinished import that is **abandoned** | Removed, then imported again |
| Anything else: no stamp, an unreadable stamp, another UID or digest, a powered-on VM | `Conflict`, never used, replaced or deleted. The `VMImage` owner sees a uniform message; who owns the object is logged by the provider only |
| The lookup fails | Retryable error, never treated as "absent" |

An unfinished import counts as **live** while any of these holds: a task on it is queued
or running, or its state cannot be read (an import in flight shows as
`ResourcePool.ImportVAppLRO`, running, in the object's recent tasks); vCenter blocks its
`Destroy_Task` (a defensive extra — an import lease does not do that); or its
`preparedat` is younger than `max(2 × spec.prepare.timeout, 2h)` (a timeout above one year
counts as one year). Only a powered-off, non-template object of this same image that is
none of these is removed; its age alone never is.

**Concurrent prepares.** vCenter keeps VM names unique within a folder: a second import of
the same name fails with `DuplicateName`, reported through the import's NFC lease; the
provider then looks again and reuses, waits for, or refuses what is there. If the name is
held by something the provider does not look at — a vApp or folder with that name, or a VM
whose name differs only in case — the prepare is a `Conflict` at once. If two objects with
the name exist anyway (a vCenter or simulator without that uniqueness), the one with the
lowest managed object ID survives: every other prepare destroys only the object it created.
A template is handed out only while the name addresses it alone.

**Verified on vCenter 8.0.2 (2026-09-25):** `DuplicateName` is enforced within one folder
and arrives through the lease (`lease.Wait`), not from `ImportVApp` itself — for a
completed VM, a template, an entity still held by an active lease, and 4 concurrent imports
of one name (exactly one created its entity). While a lease is active the entity already
carries the stamp, is a powered-off non-template, and has `ResourcePool.ImportVAppLRO`
running in its recent tasks; `Destroy_Task` is not disabled. Aborting the lease deletes the
partial entity (the provider aborts on a context detached from the request, so a manager
timeout still cleans up). MOIDs increase with creation order, so the lowest-MOID
convergence is a fallback real vCenter should not need.

**An OVF must describe exactly one VM.** A vApp (`VirtualSystemCollection`) gets
`InvalidSpec`.

**Failures.** The import is synchronous. The provider tells the manager which failures
cannot heal on their own, so the manager holds the image instead of retrying:

| Failure | Result |
|---|---|
| The source answers a 4xx other than 408/429, is at (or redirects to) a refused address or scheme, redirects more than 3 times, or is larger than the download limit; checksum mismatch; unreadable archive; no or invalid OVF descriptor; a file reference outside the package; an OVF vCenter's parser rejects; a multi-VM OVF; a non-`http(s)` URL; a bare `.ovf` | `InvalidSpec` (`InvalidSource`; not retried until the source changes) |
| This image's template is still being imported by another request, or concurrent prepares are still settling on one template | In progress (retried every 30 seconds; not counted toward the Provider's circuit breaker) |
| The configured import folder is missing, ambiguous or outside the datacenter's VM folder | `FailedPrecondition`: retried, not counted toward the circuit breaker, so a wrong `defaults.folder` does not stop the Provider's other operations. Fix the Provider |
| The source answers 5xx, 408 or 429, is unreachable or breaks off; the download cannot be staged; vCenter refuses to create, upload or convert what the OVF describes | Retryable, tagged `IMAGE_SOURCE_UNAVAILABLE`: the image's own problem, so it is **not** counted toward the Provider's circuit breaker — one tenant's failing image cannot stop the Provider for every tenant. A partial import this call created is destroyed |
| vCenter itself is unreachable, the session expired, or the provider lacks rights | Retryable (`Unavailable`), counted toward the circuit breaker |

Messages are fixed text: HTTP status codes (beyond "an HTTP client error"), transport
errors, archive and XML parser output, vCenter fault text and the computed checksum go to
the provider log only, so a `VMImage`'s status tells nothing about what an arbitrary URL
serves.

**Requests from an older manager (deprecated).** A manager older than ADR-0009 sends a bare
target name and no identity. For this release only, the provider serves it the pre-ADR way
(a template named after the `VMImage`, reused by name anywhere in the datacenter, no
stamp), logs `deprecated: image prepare without identity from an older manager; upgrade
the manager; refused from the next release` at `WARN`, and increments
`virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="vsphere"}`, which
the vSphere provider serves at `/metrics` on its health port. Alert on that counter. The
next release refuses such requests. Legacy templates and identity
templates never share a name, so neither mode can touch the other's templates.

**Legacy templates are left alone.** Templates prepared before this change (named after
the `VMImage`, without `virtrigaud.image.uid`) are never reused, renamed or deleted; the
first create after the upgrade imports the image once more under the new name. To find
them: templates in the import folder without a `virtrigaud.image.uid` ExtraConfig key.

No new vCenter privilege is needed: importing, marking as template and destroying the
provider's own partial import were already part of the OVA prepare, and the stamp is an
ExtraConfig write (`VirtualMachine.Config.AdvancedConfig`, already required by
[VM ownership](vm-ownership.md#required-vcenter-privileges)).

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
`/var/lib/libvirt/images`) in the list: `ImagePrepare` refuses a pool whose directory is not
an allowed image directory (`InvalidSpec`), because no VM could be created from an image
prepared there. On multi-tenant hosts prefer a dedicated
directory that only administrators write to; every image in an allowed directory is
usable by anyone who can create a `VMImage` for that provider. Session-mode (`/session`)
providers, and images kept in subdirectories, need explicit configuration.

## libvirt prepared images (`source.libvirt.url`)

A libvirt `VMImage` with `source.libvirt.url` is **prepared**: the provider downloads the
image on the hypervisor host, checks it, converts it to qcow2 and publishes it in the storage
pool (`source.libvirt.storagePool`, default `default`), and every VM created from it gets its
own copy. This section describes the ADR-0009 behaviour of the libvirt provider (Slice 4),
which advertises `supportsImageArtifactIdentity`. A clustered libvirt provider (ADR-0007)
does not prepare images yet (`ImagePrepare` is `Unimplemented`, both capabilities off).

### Name and stamp

The prepared image of `VMImage` `<namespace>/<name>` (UID `u`), prepared from a
`spec.source` whose digest is `d`, is

```text
<pool dir>/<namespace>.<name>_<h16>.qcow2              the artifact (mode 0444)
<pool dir>/.<namespace>.<name>_<h16>.virtrigaud-image.json   its stamp (mode 0444)
```

where `h16` is the first 16 hex digits of `sha256("v1/" + u + "/" + d)` and
`<namespace>.<name>` is cut so the base name is at most 200 bytes. Example:
`team-a.ubuntu-22.04_3c9e1f0a7b2d4e61.qcow2`. The name contains `_`, which no Kubernetes
name can, so it never equals a pre-ADR bare name (`ubuntu-22.04.qcow2`) or a VM disk, and it
is never a #334 reserved name. A re-created `VMImage` (new UID) or a changed `spec.source`
(new digest) gets a new artifact; the old one is left in place.

The stamp is a small JSON file (at most 4 KiB) holding the image UID, namespace and name, the
source digest, the Provider that prepared it, the time, and the artifact file's **inode and
size**. It never holds the URL, a header or a secret:

```json
{"stampVersion":1,"image":{"uid":"5f0c…","namespace":"team-a","name":"ubuntu-22.04"},
 "sourceDigest":"sha256:9b1e…","preparedBy":{"uid":"…","namespace":"team-a","name":"libvirt"},
 "preparedAt":"2026-09-25T10:00:00Z","artifact":{"inode":1835021,"size":2361393152}}
```

A stamp that is oversized, not JSON, repeats a key, contains a `null`, an unknown field, a
wrong type, an unknown `stampVersion`, a malformed digest, a namespace or name that is not a
Kubernetes name, or a `preparedAt` that is not an RFC 3339 time is **untrusted** — treated
exactly like no stamp. So is a stamp or artifact **file** that the provider's SSH user does
not own, or that is writable by its group or by others: another principal could rewrite it
after it was checked. Providers that share one pool directory must therefore use the **same
SSH user**; a pool shared by different SSH users sees each other's artifacts as a Conflict.

### Reuse, and what is refused

Before downloading, the provider reads the artifact and its stamp (never following a
symlink):

| Found at the name | Result |
|---|---|
| Nothing | Download, convert and publish (below) |
| The artifact and a trusted stamp for this image's UID and digest, recording the artifact's inode and size | **Reused**; nothing is downloaded |
| A stamp for this image and digest, no artifact, last written less than the staleness bound ago | **In progress** (another prepare is publishing): retryable `Unavailable` |
| The same, older than the staleness bound | **Abandoned** by a crashed prepare of this image: that stamp alone is removed (only if unchanged since it was read), then the image is prepared again |
| Anything else: an artifact without a stamp, an untrusted stamp, another UID or digest, an inode or size that does not match, a file not owned by the SSH user or writable by group or others, a symlink or directory | **Conflict** (`AlreadyExists`, ADR-0009 D4): nothing is overwritten, deleted, re-stamped or adopted; an operator has to act |
| The check itself fails (SSH, `stat`) | Retryable error — never taken as "nothing there" |

The Conflict message names only the requester's own artifact; who else the stamp names is
written to the provider log only. The **staleness bound** is
`max(2 × spec.prepare.timeout, 2h)` (the timeout defaults to 30m), judged by file mtime on the
host's clock.

### How an image is published

1. Every prepare works in its **own** staging files in the pool directory, created by
   `mktemp` (exclusive, unpredictable name, mode `0600`):
   `.virtrigaud-imageprepare-XXXXXXXXXX.download` (the download), `….partial` (the
   `qemu-img convert` output) and `….stamp.partial` (the stamp). They are dotfiles with
   reserved suffixes, so a pool refresh does not list them and they can never be used as a
   base image. They are removed when the prepare ends; files of a crashed prepare that have
   not been written for the staleness bound are swept by the next prepare in that pool.
2. The download runs `curl` on the host with its configuration — the URL — on **stdin**
   (`curl -q -K -`): the URL never appears on a command line, in a log, in the host's
   process list or in a file, and the SSH user's `~/.curlrc` is not read. It is exactly
   **one** transfer (URL globbing is off, so `[1-9]` or `{a,b}` in a URL is literal), limited
   to `http`, `https` and `ftp` (redirects included), with a 30-second connect timeout, a
   total time limit of `spec.prepare.timeout` (default 30m), and a size limit of
   `VIRTRIGAUD_LIBVIRT_IMAGE_MAX_DOWNLOAD_GIB` GiB (provider pod environment, default `256`;
   curl enforces it on the announced size, and recent curl versions also stop a transfer
   that grows past it). Its checksum is verified when `checksum` is set, and its header must
   not reference other files.
3. The converted image is **finalized read-only**: `chmod 0444`, `restorecon` (only when
   `selinuxenabled` reports SELinux on; through non-interactive `sudo -n`, best-effort),
   `sync`. It is **not** chowned — it stays owned by the provider's SSH user, which is what
   lets that user hard-link it with `fs.protected_hardlinks=1`, and it only ever needs to be
   read (each VM gets a copy that is chowned and relabelled as before).
4. The stamp is linked into place with `ln -T` (never replacing a file, never linking into
   a directory). Of several concurrent prepares of the same image, exactly one creates the
   stamp; the others re-read the name and reuse, wait or refuse as in the table above.
5. Only the prepare that created the stamp links the artifact, again with `ln -T`. If the
   artifact name is taken at that moment — by any file, directory or symlink, even a symlink
   to this prepare's own file — the prepare first removes **its own** stamp (so it never
   stamps a file it did not publish) and returns a Conflict. If the link's answer is lost
   (the SSH connection drops), the provider checks whether the artifact is its file before
   withdrawing anything.

Nothing is ever downloaded, converted, written or removed at the final name. The pool
directory must be on a filesystem that supports hard links (ext4, xfs, NFS, …); on one that
does not, the prepare fails with an explicit `InvalidSpec`. The provider's SSH user must be
able to create files in the pool directory, as before.

### Rules for the source

- An import takes **exactly one input**, `source.libvirt.url`. A source that also sets
  `source.libvirt.path` is refused (`InvalidSpec`): converting the path would make a stamped
  copy of any file in the allowed image directories. A `source.libvirt.path` alone is a
  reference to an existing image and is used as written (confined, then copied), never
  prepared.
- The storage pool must exist on the host and its directory must be an allowed image
  directory (see above).

### Which failures are retried

A prepare runs synchronously. The provider reports failures that retrying cannot fix as
`InvalidSpec` (gRPC `InvalidArgument`): the manager records them on the `VMImage`
(`InvalidSource`) and holds instead of retrying every few seconds.

| Permanent (`InvalidSpec`) | Retried |
|---|---|
| The URL answers HTTP 4xx (except 408, 425, 429); the URL does not result in exactly one transfer; the image is larger than the download limit; curl reports an unsupported or disallowed protocol, a malformed URL, access or login denied, a missing remote file, or a TLS certificate the host cannot verify | HTTP 5xx, 408, 425, 429; DNS, connect, timeout, TLS-handshake and transfer errors |
| A checksum mismatch or an unsupported `checksumType` | The SSH transport or the host failing (a probe, `mktemp`, `chmod`/`sync`, `qemu-img convert`, `ln`, a checksum command that could not run) |
| An image `qemu-img` cannot read, an unsupported format, or a header that references other files | A matching stamp still being published (`Unavailable`) |
| A malformed request or source (see above), a pool that does not exist (libvirt's "Storage pool not found"), has no directory, is outside the allowed image directories or cannot hold hard links | Any other storage pool lookup failure (for example a libvirtd restart) |

Error messages never contain the source URL (it may embed credentials or a presigned
token), and they do not say which HTTP status or curl error a download ended with, nor
which checksum the host computed: the `VMImage`'s status is visible to other namespaces
when the image is shared, and a URL chosen by a tenant must not become a probe of what the
hypervisor host can reach. The provider log records the details, with the URL's user-info
and query removed.

### Deprecated: requests from an older manager

A manager older than ADR-0009 sends a bare `target_name` and no identity. For **this release
only**, the libvirt provider serves such a request the pre-ADR way — the artifact is
`<pool dir>/<VMImage name>.qcow2`, an existing file of that name is reused by name, no stamp
is written or echoed — but with the fixes above (retryable probe, private staging, read-only
image published with `ln`, never chowned). It can never reach an ADR-0009 artifact (the names
are disjoint). In legacy mode, two `VMImage`s with the **same name in different namespaces**
prepared through one Provider (or through Providers that share the pool directory) **share
one file**, `<pool dir>/<VMImage name>.qcow2`: whichever is prepared first is what both get.
This is the pre-ADR defect, and it is why legacy mode exists for one release only. Every such request logs
`WARN deprecated: image prepare without identity from an older manager; upgrade the manager; refused from the next release`
and increments `virtrigaud_provider_image_prepare_legacy_requests_total{provider_type="libvirt"}`.
Alert when that counter is non-zero, and do not run an older manager against a Provider
shared across tenants. The next release refuses these requests with `FailedPrecondition`.

Bare-name files prepared by earlier releases are never deleted, renamed, re-stamped or
adopted; after the upgrade, the first create for each image prepares a new artifact and the
old file becomes an orphan (a `<name>.qcow2` without a stamp).
