# Proxmox: the storage of a VM's cloud-init drive

A `VirtualMachine` that carries user-data (`spec.userData`) gets a cloud-init
drive from the Proxmox provider: the `ide2` device of the PVE VM, written as
`<storage>:cloudinit`. PVE creates that drive as a volume on the storage, so the
storage must allow the `images` content type. On a stock PVE install the
storage `local` is a directory storage that does **not** (an LVM-thin
storage such as `local-lvm` does), and PVE then refuses to start the VM with
`storage 'local' does not support content-type 'images'`.

Before this change the provider put the drive on `local` for every VM cloned
from a template, so every Proxmox VM with user-data failed to start on such a
node (rc4 validation finding R33). The provider now chooses the storage as below.

## How the storage is chosen

For a VM cloned from a template (the `VMImage` names a Proxmox template),
the first of these that is set wins:

1. **A storage the request names.** A `storage` key in the image JSON the
   provider receives, or the `source.proxmox.storage` of a `VMImageSpec` in it
   (read even when the template is named). The manager's own `Create` request
   does not carry either today, so for a `VirtualMachine` this step has no effect
   yet. It also sets the target storage of the full clone (PVE's `storage`
   clone parameter).
2. **The provider's default storage**: the environment variable
   `PROVIDER_DEFAULT_STORAGE`, else `PVE_DEFAULT_STORAGE`, on the Proxmox provider
   pod. Set it with `spec.runtime.env` of the `Provider`:

   ```yaml
   spec:
     type: proxmox
     runtime:
       env:
         - name: PVE_DEFAULT_STORAGE
           value: local-lvm   # a storage with the "images" content type
   ```

   The same variable is the target of a migration's `qm importdisk`
   ([ADR-0006](adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md)).
3. **The storage the cloned VM's boot disk lives on**, read from the clone's
   config after the clone task ends (the storage in the `scsi0`, `virtio0`,
   `sata0` or `ide0` value, e.g. `local-lvm` in
   `local-lvm:vm-101-disk-0,size=32G`, whichever the provider detects as the boot
   disk). A storage that holds the VM's disk holds its cloud-init drive.
4. **`local`**, only when the config shows no such disk (the boot disk is a
   passthrough device, or the config cannot be read). The provider logs a
   warning when it falls back.

A VM that is not cloned from a template (created without a disk) has no boot
disk to follow: steps 1, 2 and 4 apply. A VM created from a migration's imported
disk keeps its own rule: the cloud-init drive goes to the storage the disk was
imported to (the request's storage, else the provider's default storage, else
`local-lvm`).

The provider does not ask PVE whether the chosen storage allows `images`. A
storage named by step 1 or 2 is the operator's explicit choice, and if PVE
rejects it the VM's start fails with PVE's own message, which names the storage
and the content type. Step 3 needs no check: the storage already holds the VM's
disk image.

## Upgrading

A VM already created with a cloud-init drive on `local` keeps it (the provider
sets the drive only while it creates, or finishes creating, the VM). Move it with `qm set <vmid>
--ide2 <storage>:cloudinit` on the node, or delete the VM and let the
`VirtualMachine` create it again.
