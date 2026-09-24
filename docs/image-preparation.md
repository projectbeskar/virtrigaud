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
| `status.ready` | `true` once the image is available on **at least one** provider (the OR across providers). |
| `status.availableOn` | The list of providers the image is prepared on (the `Providers` print column). |
| `status.providerStatus[<provider>]` | Per-provider truth: `available`, plus the provider-specific `id`/`path`/`message`/`lastUpdated`. |
| `status.prepareTaskRef` | The in-flight async prepare task ref, if any; cleared on completion. |
| `status.lastPrepareTime` | When the last prepare was triggered/completed. |
| `status.conditions` | `Ready` and `Importing` conditions with reasons (`Importing`, `Prepared`, `MissingOnProvider`, `WaitingForImage`, `InvalidSource`). |

Status never contains secrets — only provider ids/paths/messages.

## What is and isn't done

PR-5 wired preparation to **run** through CRs and reflect it in status (`Importing` →
`Ready`). **PR-6 (#214) closes the loop**: the provider now returns *where* it placed the
prepared image (`prepared_image_id` / `prepared_image_path`), the controller stamps that
onto `status.providerStatus[provider].{id,path}`, and `Create` **consumes** it — cloning the
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
   **any** domain defined on the host (VirtRigaud-managed or not).
7. **Self-contained**: `qemu-img info` must show no backing file, no external data file,
   and no VMDK extent outside the file; accepted formats are qcow2, raw, vmdk, vpc, vhdx
   and vdi. Downloaded (`url`) images get the same header check before conversion.

A base image is always **copied** into the VM's own `<vm>-disk.qcow2`; it is never
attached in place, so VMs never share a disk and deleting a VM never deletes the image.
The only disk attached in place is a migration's landing disk: `spec.importedDisk` whose
path resolves to `<vm name>-migrated.qcow2` directly in the `default` pool directory and
is not used by any domain.

A rejected path is a non-retryable `InvalidArgument`: the VM gets
`Provisioning=False` with reason `ValidationError` (rechecked every 2 minutes rather than
retried every 5 seconds), and an image rejected during preparation gets
`status.providerStatus[<provider>].message` plus, while it is not Ready on any provider,
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
