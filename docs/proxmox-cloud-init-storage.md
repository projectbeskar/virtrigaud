# Proxmox: the storage of a VM's cloud-init drive

A `VirtualMachine` that carries user-data (`spec.userData`) gets a cloud-init
drive from the Proxmox provider: a device of the PVE VM (`ide2` unless the
template already has one elsewhere), written as `<storage>:cloudinit`. PVE
creates that drive as a volume on the storage, so the storage must allow the
`images` content type. On a stock PVE install the storage `local` is a directory
storage that does **not** (an LVM-thin storage such as `local-lvm` does), and PVE
then refuses to start the VM with `storage 'local' does not support content-type
'images'`.

Before this change the provider put the drive on `local` for every VM cloned
from a template, so every Proxmox VM with user-data failed to start on such a
node (rc4 validation finding R33). The provider now chooses the storage as below.

## How the storage is chosen

For a VM cloned from a template (the `VMImage` names a Proxmox template), the
provider reads the config of the new VM once the clone task has ended. If it
cannot read it, the Create fails and is retried; the provider does not guess a
storage without it. Then the first of these
that applies wins:

1. **A storage the request names.** A `storage` key in the image JSON the
   provider receives, or the `source.proxmox.storage` of a `VMImageSpec` in it
   (read even when the template is named). The provider honours both, but the
   manager's own `Create` request does not carry either today, so for a
   `VirtualMachine` this step has no effect yet. It also sets the target storage
   of the full clone (PVE's `storage` clone parameter), which takes the template's
   own volumes, its cloud-init volume included, to that storage.
2. **The storage the clone's primary disk lives on**: the storage in the
   `virtio`, `scsi`, `sata` or `ide` value, e.g. `local-lvm` in
   `local-lvm:vm-101-disk-0,size=32G`. The provider takes the first drive that
   is not a cdrom, scanning virtio0-15, then scsi0-15, sata0-15, ide0-15. That is
   the first such drive, not necessarily the boot disk: a data disk on an earlier
   bus wins (`PrimaryDiskFromConfig`, whose disk also starts the boot order). A
   storage that holds the VM's disk holds its cloud-init drive.
3. **The provider's default storage**: the environment variable
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

   It is used for a clone whose config shows no storage-backed disk (a
   passthrough device), for a VM made without a template, and as the target of a
   migration's `qm importdisk`
   ([ADR-0006](adr/0006-storage-backend-agnostic-cross-hypervisor-migration.md)).
   It does not move the drive of a clone whose primary disk is on another storage.
4. **`local`**, only when none of the above applies. The provider logs a warning
   when a clone falls back.

A VM that is not cloned from a template (created without a disk) has no disk to
follow: steps 1, 3 and 4 apply. A VM created from a migration's imported disk
keeps its own rule: the cloud-init drive goes to the storage the disk was
imported to (the request's storage, else the provider's default storage, else
`local-lvm`).

### A template that already has a cloud-init drive

A full clone of a template that has a cloud-init drive owns a cloud-init volume
of its own (`vm-<vmid>-cloudinit`, on the clone's storage). PVE cannot create it
a second time on that storage: asking for `<storage>:cloudinit` there fails with
`Logical Volume "vm-<vmid>-cloudinit" already exists` and leaves the drive as it
is. So, once the storage is chosen as above:

- The clone's drive is already on it: the provider sets no drive. PVE builds the
  drive's contents again at every start.
- The clone's drive is on another storage: the provider sets the drive to a new
  one on the chosen storage, in the slot it is in (PVE removes the old volume).
- The clone has no cloud-init drive: the provider sets `ide2`.

A cloud-init drive is recognised by PVE's volume name, `vm-<vmid>-cloudinit`; on
a directory or file storage it is a file `<vmid>/vm-<vmid>-cloudinit.raw` (made
by a full clone), `.qcow2` (made by a config update that replaces the drive) or
`.vmdk`. A volume that only has "cloudinit" in its name, such as an ISO, is not a
cloud-init drive.

The template's drive can be in any `ide`, `sata` or `scsi` slot; the boot order
(`order=<primary disk>;<that slot>`) follows it. A re-sent Create that finds its
half-made VM runs the same step again and changes nothing.

### Checks

A storage named for the VM (the request's or the image's) and the provider's
default storage must have the syntax of a PVE storage ID (letters, digits, `-`,
`_`, `.`; starting with a letter, ending with a letter or digit). Otherwise the
Create is refused as an invalid spec before anything is created, with a message
that does not repeat the value: it ends up in a config value (`<storage>:cloudinit`)
and must not be able to add parameters to it. The default storage is also checked
when the provider starts (an error is logged, without the value) and by
`Validate`, which then fails with a message naming `PVE_DEFAULT_STORAGE` /
`PROVIDER_DEFAULT_STORAGE`, so the problem shows when the provider is checked,
not at the first Create that uses the value.

The provider does not ask PVE whether the chosen storage allows `images`. A
storage named by step 1 or 3 is the operator's explicit choice, and if PVE
rejects it the VM's start fails with PVE's own message, which names the storage
and the content type. Step 2 needs no check: the storage already holds the VM's
disk image.

## Upgrading

A VM already created with a cloud-init drive on `local` keeps it (the provider
sets the drive only while it creates, or finishes creating, the VM). Move it with `qm set <vmid>
--ide2 <storage>:cloudinit` on the node, or delete the VM and let the
`VirtualMachine` create it again.
