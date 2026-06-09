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
| `status.conditions` | `Ready` and `Importing` conditions with reasons (`Importing`, `Prepared`, `MissingOnProvider`, `WaitingForImage`). |

Status never contains secrets — only provider ids/paths/messages.

## What is and isn't done

PR-5 wired preparation to **run** through CRs and reflect it in status (`Importing` →
`Ready`). **PR-6 (#214) closes the loop**: the provider now returns *where* it placed the
prepared image (`prepared_image_id` / `prepared_image_path`), the controller stamps that
onto `status.providerStatus[provider].{id,path}`, and `Create` **consumes** it — cloning the
prepared template (vSphere/Proxmox) or using the local prepared pool file (libvirt) instead
of re-resolving (and re-downloading) the original source. A second VM from the same prepared
image therefore skips the re-download. When an image is not yet prepared/available on the
target provider, `Create` falls back to the original by-reference source resolution
unchanged (no regression). The `ImagePrepare` RPC change is wire-compatible (the `task` ref
stays at proto field 1), so manager and providers must roll together but no CRD spec field
changed. See ADR-0005 "Out of scope" for the original PR-6 framing.
