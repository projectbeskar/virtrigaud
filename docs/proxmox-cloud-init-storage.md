# Proxmox: cloud-init user-data (the NoCloud seed) and its storage

A `VirtualMachine` that carries user-data (`spec.userData`) on a Proxmox
`Provider` gets the **whole** user-data document in the guest, as on libvirt
(a NoCloud seed ISO) and vSphere (`guestinfo.userdata`): `users` with their own
keys and `sudo`, `write_files`, `packages`, `runcmd`, `chpasswd`, `hostname`
and every other cloud-config key reach cloud-init unchanged.

The provider does this with a **NoCloud seed volume** it builds on the PVE node:
an ISO9660 image with the volume label `cidata` that holds the request's
user-data byte for byte, a meta-data and a network-config the provider renders.
It is a VM-owned volume of the VM's own storage, attached as a CD-ROM (in `ide2`
unless the VM's PVE cloud-init drive, or another slot, says otherwise).

In v0.4.0 and earlier the provider read only two things out of the user-data
with a line scan: the first `name:` it found (as PVE's `ciuser`) and the items
of every `ssh_authorized_keys:` block list, all merged into one key list for
that one user (a second user's keys went to the first user; a flow list
`[key1, key2]` was dropped). Everything else was dropped silently.

A VM **without** user-data is unchanged: no seed, no SSH command, PVE's own
cloud-init handling as before.

## What the seed holds

| File | Content |
|---|---|
| `user-data` | `spec.userData`, byte for byte. The provider does not parse it. |
| `meta-data` | Rendered by the provider (see [Meta-data](#meta-data)). |
| `network-config` | Rendered by the provider from `spec.networks` (see [Network-config](#network-config)); absent when no NIC can be matched. |

PVE's own cloud-init settings are never combined with a seed (two `cidata`
sources would be undefined behaviour): the provider sets no `ciuser`,
`sshkeys`, `ipconfigN` or `nameserver` on a VM with a seed, and a PVE
cloud-init drive the template gave the clone is **replaced** by the seed in its
slot (PVE frees the drive's volume). Keys a template already carries
(`ciuser`, `ipconfig0`, ...) stay in the config but are inert: PVE uses them
only to build its own cloud-init drive, which the VM no longer has.

### Meta-data

- `instance-id`: `virtrigaud-<UID of the VirtualMachine>`. It is the same for a
  re-sent `Create` of the same `VirtualMachine` and different for every other
  `VirtualMachine`, including one with the same namespace and name created
  again. A request with no owner UID (a caller other than the manager) gets
  `virtrigaud-pve-<VMID>`.
- `local-hostname`: the VM name. A `hostname:` or `fqdn:` in the user-data takes
  precedence inside the guest, as cloud-init always prefers them.
- `spec.metaData`, when set, is the base: every other key it has is kept, and
  its `instance-id` and `local-hostname` are replaced by the two above (so a
  VMClone always gets a new identity). A `spec.metaData` that is not a YAML
  mapping is refused (`InvalidSpec`, nothing created). `spec.metaData` without
  `spec.userData` is not applied (no seed), as on libvirt.

All values are written through the YAML encoder, never interpolated.

### Network-config

A NoCloud `network-config`, version 2, with one entry per `spec.networks` entry
(one for NIC 0 when the VM names no network):

```yaml
version: 2
ethernets:
  eth0:
    match:
      macaddress: bc:24:11:aa:bb:01   # the MAC PVE gave net0
    set-name: eth0
    addresses: [192.0.2.10/24]
    routes:
      - to: 0.0.0.0/0
        via: 192.0.2.1
    nameservers:
      addresses: [192.0.2.53]
  eth1:
    match:
      macaddress: bc:24:11:aa:bb:02
    set-name: eth1
    dhcp4: true
    nameservers:
      addresses: [192.0.2.53]
```

- Each entry matches the MAC of NIC `netN` as the VM's config shows it after
  the VM was made and its networks were applied, and names it `eth<N>`, as PVE's
  own cloud-init network data does (`name: eth<N>` with the MAC).
- An entry with a static IP gets the address with its prefix and, with a
  gateway, a default route through it (`0.0.0.0/0`, or `::/0` for IPv6); one
  without gets `dhcp4: true`.
- The DNS servers of all entries (PVE's `nameserver` is VM-wide) are given to
  every NIC. No search domain is set (the CRD has none).
- An entry whose NIC the VM does not have, or whose NIC has no MAC, is left out
  (logged). With no entry left, the seed has no `network-config` and cloud-init
  falls back to DHCP on the first NIC.

## Prerequisites

The seed is built over the provider's **SSH data plane** to the PVE node (the
one migrations use). A `Create` with user-data that the provider cannot honour
is refused **before anything is created**, as `InvalidSpec` (`ValidationError`
on the `VirtualMachine`, rechecked with a backoff of 30 seconds doubling to 10 minutes), with a message that names
what is missing. The user-data is never dropped in silence:

| Missing | Message names |
|---|---|
| No SSH data plane (no endpoint host) | `ssh_user` and `ssh_password` or `ssh_privatekey` (and `known_hosts`) in the Provider's credentials Secret |
| No SSH credentials | `ssh_password` or `ssh_privatekey` |
| Host-key verification on and no `known_hosts` | `known_hosts` (details, with the host, in the provider log) |
| `genisoimage` not installed on the node | the `genisoimage` package |
| The VM would be on another node than the one SSH reaches | `PVE_NODE_SELECTOR` (or the clone's placement host) |

The node check compares the node the VM is created on (or a clone's placement
host) with `uname -n` of the node SSH reaches (without its domain, as PVE names
nodes); the seed script checks it again (the VM's config must be in the node's
own `/etc/pve/qemu-server`). The same refusals found only after the VM was made
(e.g. no free CD-ROM slot) remove the VM and are answered as `InvalidSpec` too,
so the manager does not make and remove a VM at every retry.

What the node and the configuration need:

- **SSH credentials** in the Provider's credentials Secret: `ssh_user`
  (default `root`), `ssh_privatekey` or `ssh_password`, and `known_hosts` (or
  the audit-flagged `PROXMOX_INSECURE_SKIP_HOST_KEY_VERIFICATION=true`). See
  [`examples/migration/vsphere-to-proxmox.yaml`](../examples/migration/vsphere-to-proxmox.yaml).
  The SSH user must be `root` (it runs `qm importdisk` and `qm set`, and owns
  the root-only build directory).
- **The SSH host is the API endpoint's host.** The seed is built and attached
  on that node, so the VM must be created there: in a cluster, point
  `PVE_NODE_SELECTOR` at that node (as for migration imports). An endpoint that
  is a load balancer in front of several nodes does not work for VMs with
  user-data.
- **`genisoimage`** on the node. It is a dependency of `qemu-server`, so every
  PVE node has it; the provider checks it before creating the VM.
- **`flock`** (util-linux) on the node, present on every PVE node.
- **A storage with the `images` content type** for the seed, chosen as
  [below](#how-the-storage-is-chosen).
- **PVE token privileges.** The seed is imported and attached by `root` over
  SSH, not by the token. A VM made **without a template** with user-data is now
  created first and stamped "ready" afterwards (its config is read and its
  description written), so the token needs `VM.Audit` and `VM.Config.Options`
  on it, as it already does for clones.

### Security posture

Before this release, VM lifecycle needed only the PVE API token; SSH was used
only by migrations. A Proxmox Provider whose VMs carry user-data now holds a
**root SSH credential** to the PVE node. The seed's `qm importdisk` and `qm set`
run as `root@pam` over SSH: PVE attributes those config and storage changes to
root, not to the provider's token, and its permission checks do not apply to
them. The provider logs one line for every seed build (VM, node, storage, slot,
the size of the user-data, never its content). Treat the provider pod's
credentials Secret as root on the node: restrict who can read it, and prefer an
SSH key that only the provider uses.

The user-data is stored in plaintext in the seed volume, on the VM's storage,
for the VM's whole life (any principal who can read that storage, and root in
the guest, can read it), as in PVE's own cloud-init drive and libvirt's seed.
Keep long-lived secrets out of user-data; fetch them at first boot instead.

## How the seed is built

One SSH session per seed, its input on the session's standard input:

1. The three files travel as a tar archive on stdin: never on a command line,
   never in a log, never in an event or a status.
2. On the node, `/run/virtrigaud/seed-stage` (on `/run`, a tmpfs) is refused
   when it is a symbolic link, (re)created `0700`, and checked to be owned by
   the SSH user (root). The script holds the VM's lock
   (`/run/virtrigaud/seed-stage/.lock-<vmid>`, `flock`) for its whole run: a
   seed build of the same VM still running (an interrupted earlier attempt), or
   a running `qm importdisk` of the VM, makes it stop without changing anything.
   Once it holds the lock it checks that the file it locked is still the one at
   that path (the lock file of a destroyed VM is removed, see
   [Lifecycle](#lifecycle)); if not, it stops the same way and the request is
   retried.
   The files are extracted into a fresh `mktemp -d` directory there (no owner
   or mode taken from the archive), and `genisoimage -volid cidata -joliet
   -rock` builds the ISO.
3. `qm importdisk <vmid> <iso> <storage> --format raw` copies it into the VM's
   storage as `vm-<vmid>-disk-N` (`<vmid>/vm-<vmid>-disk-N.raw` on a directory
   storage). It is never named `vm-<vmid>-cloudinit`, which PVE rebuilds.
   `--format raw` keeps a file storage from turning the ISO into qcow2, which
   the guest could not read. The seed is the one `unusedN` entry the import
   added, and its volume must be the VM's own disk volume on that storage;
   anything else stops the script without deleting anything (a volume left
   unused is freed with the VM).
4. `qm set <vmid> --<slot> <volume>,media=cdrom` attaches it (and deletes a PVE
   cloud-init drive in another slot). PVE appends a newly attached CD-ROM to the
   VM's boot order; the provider then takes the seed out of it again in a config
   update of its own (see [Boot order](#boot-order)).
5. On every exit the build directory is removed; the identified seed volume,
   imported but not attached, is deleted from the VM again (`qm set --delete
   unusedN`, which frees it). A build directory a killed session left behind is
   removed by the next build after an hour, and by a reboot.

Each `qm` call that fails stops the script with an exit status of its own, so a
failed `qm` call is a definite failure (the VM is removed), never mistaken for a
broken SSH session.

## Lifecycle

- **Delete** removes the seed with the VM. `qm destroy` frees a VM's disks,
  unused disks and PVE cloud-init drive, but skips CD-ROMs; the provider's
  destroy passes `destroy-unreferenced-disks=1`, which frees every volume of the
  VMID, the seed included (and an unused seed volume an interrupted build left).
  Once a VM whose config showed its own seed is gone, the provider also
  removes the VM's seed build lock file (`/run/virtrigaud/seed-stage/.lock-<vmid>`,
  an empty file on a tmpfs) over SSH; a VM without a seed opens no SSH
  session. A failed Create that removed its VM removes the file too, seed or
  not (its build may have failed before attaching). This is best effort, at
  most 10 seconds (less when the call has less time left): it never fails the
  call, it leaves a lock a running build holds alone, and a file it could not
  remove (or one of a VM destroyed by hand) is gone at the next reboot.
- **A failed Create** removes the VM it made (seed included). When the seed's
  SSH session broke, or another build or `qm importdisk` of the VM was still
  running, the build may still be going on: the VM is kept and the answer is an
  unknown outcome; the re-sent `Create` finishes it. A VM created from a
  migration's imported disk is removed and built again from the staged disk
  instead, once no build or `qm importdisk` of it runs.
- **A re-sent Create** (the manager's retry after an unknown outcome) never
  attaches a second seed: a VM that already has a seed of its own is left as it
  is.
- **VMClone.** PVE's clone copies its source's CD-ROM lines as they are, so a
  clone would point at its **source's** seed volume (same identity, and a volume
  it does not own). The provider gives such a clone a seed of its own in that
  slot, and so it does for any clone whose VMClone gives inline user-data
  (replacing the clone's PVE cloud-init drive, if it has one), made like a
  `Create`'s:
  - `instance-id` `virtrigaud-<UID of the target VirtualMachine>`;
  - `local-hostname` the customization's `hostname` (with its `domain`), else
    the target VirtualMachine's name;
  - user-data: the VMClone's `spec.customization.userData.cloudInit.inline`,
    else a `#cloud-config` whose body is an empty mapping (`{}`), which
    configures nothing. (In v0.4.1-rc1 it was a bare `#cloud-config` line,
    which cloud-init reports as "empty cloud config": a warning, so
    `cloud-init status` ended "degraded done" with exit code 2.) The source's
    user-data is **not** carried (unlike libvirt and vSphere): the clone's disk
    already holds what it applied (users and their keys, files, packages).
    User-data given by a Secret reference cannot be resolved by the provider
    and is not applied (logged);
  - network-config: DHCP on every NIC, matched by the clone's own MACs.

  The clone's first boot is therefore a new cloud-init instance: new SSH host
  keys, its own hostname, and cloud-init's per-instance modules run again
  **with the distribution's defaults** where the user-data is empty. On most
  cloud images that means `disable_root: true` (root's `authorized_keys` is
  rewritten to refuse logins), `ssh_pwauth` back to the image default, and the
  distribution's default user created. Do not rely on the source's
  user-data being applied again: the clone's cloud-init applies only the
  clone's user-data. What the source's first boot wrote to its disk
  stays in the clone. In the v0.4.1-rc2 lab, a clone without inline user-data
  of a fully booted Ubuntu 24.04 source (cloud-init 26.1) kept the rules of
  the source's users: `/etc/sudoers.d/90-cloud-init-users` still listed the
  source user's `NOPASSWD:ALL` rule, and that user still had passwordless sudo.
  cloud-init had appended a rule for the distribution's default user, which
  it created in the clone with **passwordless sudo**, a locked password and no
  SSH keys. A clone of a source that had not finished its first boot was not
  tested. So a clone ends with the source's users **and** a default user
  that has sudo. Give the VMClone inline user-data that sets the users, keys
  and sudo rules the clone must have (the source's own settings are not
  re-applied), for example:

  ```yaml
  spec:
    customization:
      userData:
        cloudInit:
          inline: |
            #cloud-config
            users:
              - default
              - name: ops
                sudo: ALL=(ALL) NOPASSWD:ALL
                shell: /bin/bash
                ssh_authorized_keys:
                  - ssh-ed25519 AAAA... ops@example.com
  ```

  The `sudo` rule above is only an illustration: grant the narrowest rule
  your policy allows.

  A clone that needs a seed is refused before anything is cloned when the
  provider cannot build one (the prerequisites above). A clone of a VM
  **without** a seed and without inline user-data is unchanged: PVE regenerates
  its own cloud-init drive.
- **A VMClone deleted while the clone runs.** If a `VMClone` and its target
  `VirtualMachine` are deleted while the provider is still cloning, the clone
  PVE finishes is bound to nothing and is **left on PVE on purpose**: the
  manager never removes a VM it can no longer attribute to a
  `VirtualMachine`. When the reconcile that sent the `Clone` sees it finish
  after the target was deleted, the manager logs "... is not bound to a new
  one and is left on the provider: adopt or remove it", naming the clone's
  VMID; otherwise (for example after a manager restart) nothing records the
  clone's VMID. Find the VM by the target name and the owner stamp in its
  notes, then adopt it or destroy it (`qm destroy <vmid> --purge 1
  --destroy-unreferenced-disks 1`, which also frees its seed; its seed build
  lock file on `/run` stays until the next reboot). `vcts` and `virtrigaud-loadgen` work only with Kubernetes
  objects: they wait for a `VMClone` whose clone may still be in flight and
  leave an unsettled one in place, but they cannot see a cloned VM that no
  Kubernetes object records any more.
- **A template made from a VM with a seed.** Its clones would reference the
  template's seed volume. A `Create` with user-data replaces it with the VM's
  own seed; a `Create` without user-data detaches it (the VM gets no
  cloud-init source from the template). The template's volume is never freed.
- **Migration import.** A `VirtualMachine` created from a migration's imported
  disk that carries user-data gets a seed on the storage the disk was imported
  to, after the disk is attached. One without user-data gets no cloud-init
  source, as before.

## Known limitations

- The VM must be on the node the SSH data plane reaches (the API endpoint's
  host); a Create or Clone that would put it elsewhere is refused.
- **A seed on node-local storage blocks moving the VM to another node.**
  qemu-server refuses to migrate a VM with a local CD-ROM image other than its
  own `vm-<vmid>-cloudinit` drive, online or offline, so HA and node evacuation
  cannot move such a VM (a VM with PVE's cloud-init drive could be moved
  offline). Put the VM (and so its seed) on a shared storage with `images`, or
  detach and free the seed before moving it (below). Without its seed, a guest
  whose cloud-init then finds no datasource may fall back to `DataSourceNone`:
  a new instance with regenerated host keys and fallback DHCP networking.
- The seed is written once, at `Create`: a later change to `spec.userData` does
  not rebuild it.
- To drop the seed by hand, do not just remove the CD-ROM (`qm set <vmid>
  --delete ide2` leaves the volume on the storage, unreferenced, until the VM
  is destroyed). Detach it and free it: `pvesm free <volume>`.
- NICs a template has but the request does not name get no entry in the
  seed's network-config, so cloud-init does not configure them (a template's
  own `ipconfigN` is not used): name every NIC that needs an address in
  `spec.networks`.

## How the storage is chosen

The seed goes to the storage a PVE cloud-init drive would use. On a stock PVE
install the storage `local` is a directory storage **without** the `images`
content type (an LVM-thin storage such as `local-lvm` has it), and PVE refuses
an image volume there (in v0.4.0 every clone's cloud-init drive went to `local`,
rc4 finding R33).

For a VM cloned from a template (the `VMImage` names a Proxmox template), the
provider reads the config of the new VM once the clone task has ended. If it
cannot read it, the Create fails and is retried; the provider does not guess a
storage without it. Then the first of these that applies wins:

1. **A storage the request names.** A `storage` key in the image JSON the
   provider receives, or the `source.proxmox.storage` of a `VMImageSpec` in it
   (read even when the template is named). The provider honours both, but the
   manager's own `Create` request does not carry either today, so for a
   `VirtualMachine` this step has no effect yet. It also sets the target storage
   of the full clone (PVE's `storage` clone parameter).
2. **The storage the clone's primary disk lives on**: the storage in the
   `virtio`, `scsi`, `sata` or `ide` value, e.g. `local-lvm` in
   `local-lvm:vm-101-disk-0,size=32G`. The provider takes the first drive that
   is not a CD-ROM, scanning virtio0-15, then scsi0-15, sata0-15, ide0-15. That
   is the first such drive, not necessarily the boot disk: a data disk on an
   earlier bus wins (`PrimaryDiskFromConfig`; the boot order follows the
   template's own boot disk first, see [Boot order](#boot-order)). A storage
   that holds the VM's disk holds image volumes.
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
   It does not move the seed of a clone whose primary disk is on another storage.
4. **`local`**, only when none of the above applies. The provider logs a warning
   when a clone falls back.

A VM that is not cloned from a template (created without a disk) has no disk to
follow: steps 1, 3 and 4 apply. A VM created from a migration's imported disk
keeps its own rule: the seed goes to the storage the disk was imported to (the
request's storage, else the provider's default storage, else `local-lvm`).

### The slot

The seed CD-ROM goes, in this order, to the slot of another VM's seed the
clone's config references (it is replaced), to the slot of the VM's PVE
cloud-init drive (it is replaced; PVE recognises one by its volume name
`vm-<vmid>-cloudinit`, on a file storage `<vmid>/vm-<vmid>-cloudinit.raw`,
`.qcow2` or `.vmdk`), else to the first free slot of `ide2`, `ide0`, `ide1`,
`ide3`, `sata0` to `sata5` (a PVE cloud-init drive in another slot than the
seed's is deleted). When none is free the VM is removed and the Create is
refused (`InvalidSpec`).

### PVE's own cloud-init drive (SSH keys without user-data)

PVE's own cloud-init drive is used only when SSH keys are set for a clone
without user-data, which no request from the manager does since the seed
replaced the line scan. Its storage is chosen as above; a template's drive
already on that storage is kept (PVE cannot allocate `vm-<vmid>-cloudinit`
twice), one on another storage is replaced in its slot, and a clone without one
gets one in `ide2`.

### Boot order

A clone with user-data gets its boot order set to one disk:
`boot: order=<boot disk>` (e.g. `order=scsi0`). The boot disk is, in this order:

1. the first disk (a `virtio`, `scsi`, `sata` or `ide` drive the config has,
   not a CD-ROM) in the `order=` list the clone inherited from the template;
2. the legacy `bootdisk` the template names, when it is such a disk;
3. the primary disk (`PrimaryDiskFromConfig`, the first disk of the bus scan;
   step 2 of [How the storage is chosen](#how-the-storage-is-chosen)).

When the clone's config shows no disk at all, the provider sets no boot order
and the clone keeps the template's. PVE refuses a boot order that names a
device the VM does not have (`invalid bootorder: device '<dev>' does not
exist`), so the provider only names a disk the config has.

The seed is not in the order: cloud-init in the guest finds it by its
filesystem label (`cidata`) and reads it as a data source; nothing boots from
it (the image has no boot record). PVE's Cloud-Init guide does the same for its
own drive (`qm set <vmid> --boot order=scsi0`), and so does a VM created from a
migration's imported disk. Network boot (`net0`) is not kept either.

PVE appends a CD-ROM that a config update attaches to the VM's boot order when
the order is a non-empty `order=` list that does not name the slot yet ("append
new CD drives to bootorder to mark them bootable"; a PVE cloud-init drive is
exempt). The seed is attached by such an update (`qm set`), so after attaching
it the provider sets the order again, in a config update of its own, with the
seed's slot taken out. It does not set the order in the same update that
attaches the seed: PVE applies an update's parameters in no defined sequence,
so the result would depend on it. Every VM that gets a seed ends with the seed
out of its order:

| How the VM is made | Final boot order |
|--------------------|------------------|
| `Create` from a template, with user-data | `order=<boot disk>` (above) |
| `Create` without a template, with user-data | PVE's default for the new VM without the seed: `order=net0` for a VM without a disk |
| `Create` from a migration's imported disk, with user-data | `order=scsi0` |
| `VMClone` that gets a seed (a seeded source, or inline user-data) | the source's order, without the seed's slot |

A slot the order already named before the seed took it (for example a
template's `ide2`, once its install ISO, now its cloud-init drive) is taken
out too. A device the config does not have is left out of the new order (PVE
refuses an order that names one). An order that would be left empty names the
boot disk, else the first NIC. A config with no `order=` list (no `boot` value,
or a legacy one such as `cdn`) is left as it is; PVE appends nothing to it,
but its legacy order includes every CD-ROM, the seed too (which is not
bootable). The update carries the config's digest, so a concurrent change of
the config is never overwritten (the provider reads the config and tries once
more). If the update still fails, a request with an owner keeps the VM, which
is complete but for its boot order, and answers with an unknown outcome; the
re-sent request finds the VM with the seed attached but still in the order and
finishes the step. (A request without an owner, which cannot find its VM
again, removes it.)

In v0.4.1-rc1 the order was set before the seed was attached, so PVE appended
the seed's slot to it (`order=scsi0;ide2`, `order=net0;ide2`). The guest still
booted from its disk (or tried the network first, then the seed, which is not
bootable). To clean up such a VM, set its order without the slot, for example
`qm set <vmid> --boot order=scsi0`.

A clone without user-data gets no boot order from the provider (only its sizing
and networks are set) and keeps the template's.

In v0.4.0 and earlier the order was `order=<primary disk>;<slot>`, with the
cloud-init drive's slot, and named `scsi0` when the clone's config showed no
disk. When a device it named was missing, PVE refused the update, so such a
Create failed and was rolled back. It also ignored the template's own boot
order, so a template with a data disk on an earlier bus booted the clone from
that data disk.

### Checks

A storage named for the VM (the request's or the image's) and the provider's
default storage must have the syntax of a PVE storage ID (letters, digits, `-`,
`_`, `.`; starting with a letter, ending with a letter or digit). Otherwise the
Create is refused as an invalid spec before anything is created, with a message
that does not repeat the value: it ends up in a command and a config value and
must not be able to add parameters to them. The default storage is also checked
when the provider starts (an error is logged, without the value) and by
`Validate`, which then fails with a message naming `PVE_DEFAULT_STORAGE` /
`PROVIDER_DEFAULT_STORAGE`, so the problem shows when the provider is checked,
not at the first Create that uses the value.

The provider does not ask PVE whether the chosen storage allows `images`. A
storage named by step 1 or 3 is the operator's explicit choice, and if PVE
rejects it the seed's `qm importdisk` fails with PVE's own message (in the
provider log), and the Create is rolled back. Step 2 needs no check: the
storage already holds the VM's disk image.

## Upgrading

- A Proxmox `Provider` whose VMs carry user-data now needs the SSH data plane
  (the [prerequisites](#prerequisites)). Without it, such a `Create` is refused
  (`InvalidSpec`) instead of creating a VM with only the user name and keys.
  Before rolling the provider: add `ssh_user`, `ssh_privatekey` (or
  `ssh_password`) and `known_hosts` to the Provider's Secret, and in a cluster
  set `PVE_NODE_SELECTOR` to the API endpoint's node. VMs without user-data
  need nothing new. Read the [security posture](#security-posture) first.
- New VMs with user-data on node-local storage can no longer be moved to
  another node while their seed is attached
  ([Known limitations](#known-limitations)).
- A Proxmox `VMClone` with `spec.options.type: LinkedClone` is now refused
  (`LinkedCloneUnsupported`): the provider never made linked clones.
- VMs created before keep their PVE cloud-init drive and settings; the provider
  does not convert them. A cloud-init drive left on `local` by v0.4.0 can be
  moved with `qm set <vmid> --ide2 <storage>:cloudinit` on the node.
