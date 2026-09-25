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
   as before — preparation is skipped, not failed.
4. The prepare may be **synchronous** (the provider imports during the call) or
   **asynchronous** (the provider returns a task ref the controller polls). Either way the
   VM is **not** created until the image is `Ready` on that provider.
5. Once prepared, the result is recorded on the `VMImage` status and subsequent VMs
   referencing the same image on the same provider skip straight to create (idempotent).

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
polled. With `onMissing: Fail` or `Wait`, which forbid a prepare, an available entry is
accepted as is: the new UID is recorded and the VM gets a `Warning` event
`ImagePrepareStateAccepted`. Holding instead would stop the VMs that already run from the
image, and the same namespace owns the old and the new Provider.

**Upgrading from a release that keyed entries by the bare Provider name.** On the first
prepare reconcile of each `VMImage`, the controller migrates that state in one status
write:

| Earlier state | After migration |
|---------------|-----------------|
| `providerStatus[<name>]`, and a Provider `<name>` exists in the **VMImage's own** namespace | moved to `providerStatus[<vmimage-namespace>/<name>]` with no `providerUID`, so it is re-validated (the idempotent prepare is issued once through that Provider) before a VM is created from it |
| `providerStatus[<name>]`, and no Provider `<name>` in the VMImage's namespace | dropped; it never satisfies a same-named Provider in another namespace, and that Provider prepares the image itself on first use |
| `availableOn` element `<name>` | rewritten like its entry, kept only while that entry is available |
| `prepareTaskRef` | cleared and never polled — it cannot be attributed to a Provider; the prepare is issued again |

Before cross-namespace sharing required a grant, a VM could reference a `VMImage` in
another namespace with a Provider of its own namespace, so a bare-name entry is not
guaranteed to come from the VMImage's own namespace. That is why a migrated entry is
re-validated rather than trusted. Until a `VMImage` is migrated, its bare-name entries are
simply ignored. If the migration drops every available entry, `status.ready` becomes
`false` with reason `PrepareStateDropped` until the next prepare. An out-of-band preparer
(used with `onMissing: Wait`) must now write `providerStatus["<namespace>/<name>"]` with
`available: true` and `providerUID` set to the Provider's UID.

The VMImage CRD must be upgraded before the manager (as for the other CRD changes of this
release): an older CRD prunes `providerUID` and `taskRef`, so no prepared image would be
trusted and no asynchronous prepare tracked. The manager's readiness check fails until the
CRD has both fields.

## `spec.prepare.onMissing`

`VMImageSpec.prepare.onMissing` gates the behaviour when the image is not yet prepared on a
provider:

| Value | Behaviour |
|-------|-----------|
| `Import` (default) | Prepare the image on the provider, then create the VM. |
| `Fail` | Do **not** prepare; record `Ready=False` / `Phase=Failed` on the `VMImage` and hold the VM (it will not be created until the image is prepared out of band). |
| `Wait` | Do **not** prepare; record `Ready=False` / `Phase=Pending` and hold, waiting for an out-of-band preparer. |

## `VMImage.status` fields you will see

```bash
kubectl get vmimage <name> -o wide
kubectl get vmimage <name> -o yaml | yq '.status'
```

| Field | Meaning |
|-------|---------|
| `status.phase` | `Importing` while a prepare is in flight, `Ready` once prepared, `Failed`/`Pending` for `onMissing: Fail`/`Wait` holds. |
| `status.ready` | `true` once the image is available on **at least one** provider (the OR across providers). A prepare in flight on one provider does not clear it while the image is available on another. |
| `status.availableOn` | The providers the image is prepared on, as `<namespace>/<name>` (the `Providers` print column). |
| `status.providerStatus["<namespace>/<name>"]` | Per-provider truth, keyed by the Provider's identity: `available`, `providerUID` (the Provider object it was recorded through), `taskRef` (an in-flight async prepare on that Provider), plus the provider-specific `id`/`path`/`message`/`lastUpdated`. See [Prepare state is per Provider](#prepare-state-is-per-provider). |
| `status.prepareTaskRef` | Deprecated and no longer written; a value left by an earlier release is cleared. |
| `status.lastPrepareTime` | When the last prepare was triggered/completed. |
| `status.conditions` | `Ready` and `Importing` conditions with reasons (`Importing`, `Prepared`, `MissingOnProvider`, `WaitingForImage`, `InvalidSource`, `PrepareStateDropped`). |

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
