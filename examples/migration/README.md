# VM Migration Examples

This directory contains examples for migrating VMs between hypervisor platforms using VirtRigaud.

## Migration model

VirtRigaud's validated cross-hypervisor migration is **storage-backend-agnostic**
(ADR-0006): the disk is staged through an object store (S3-compatible) and the
**provider pod is the S3 client**, so the bytes flow host → pod → S3 → pod → host
and **never traverse a CSI PVC**. The source exports its native disk format and
the target converts on import. This is the validated path across **all three
providers in any direction** — vSphere, libvirt, and Proxmox (ADR-0006 Slices
1–3). An **NFS** staging backend is the validated second option across all three
providers (ADR-0006 Slice 4 — see the [NFS section](#nfs-staging-backend-adr-0006-slice-4)
below). The Proxmox provider advertises **s3 and nfs** (not PVC): its disks live on
the PVE node, so the pod-mounted PVC path can never reach them, and a
`storage.type: pvc` migration with a Proxmox source or target fails capability
validation.

A legacy PVC-backed model also exists for the vSphere/libvirt directions (a
ReadWriteMany StorageClass), but it is compat-only and does not work for Proxmox
or for host-resident libvirt disks the provider pod cannot reach.

Key points before using these examples:

- **S3 staging needs a credentials Secret** (`accessKeyID` / `secretAccessKey`,
  optional `sessionToken`) in the same namespace as the VMMigration — never inline.
- **Attach a network on the target.** A raw-disk migration carries the guest's
  disk, not its NIC. For a vSphere target you MUST reference a `VMNetworkAttachment`
  (mapping to a portgroup) via `target.networks`, or the migrated VM boots with no
  NIC. See the header note in `libvirt-to-vsphere.yaml`.
- **`relay` transfer mode** is the universal floor (credentials stay in the pod);
  `auto` resolves to `relay`. `direct` is a roadmap item.

## Examples

| File | Direction | Storage | Status |
|------|-----------|---------|--------|
| [`../vmmigration-s3.yaml`](../vmmigration-s3.yaml) | vSphere → Libvirt/KVM | S3 | **Tested** (ADR-0006 Slice 1) |
| [libvirt-to-vsphere.yaml](./libvirt-to-vsphere.yaml) | Libvirt/KVM → vSphere | S3 | **Tested** (ADR-0006 Slice 2) |
| [vsphere-to-proxmox.yaml](./vsphere-to-proxmox.yaml) | vSphere → Proxmox VE | S3 | **Tested** (ADR-0006 Slice 3) |
| [proxmox-to-libvirt.yaml](./proxmox-to-libvirt.yaml) | Proxmox VE → Libvirt/KVM | S3 | **Tested** (ADR-0006 Slice 3) |
| [`../vmmigration-nfs.yaml`](../vmmigration-nfs.yaml) | Any ↔ Any | NFS | **Tested** (ADR-0006 Slice 4) |

## Quick start (S3, vSphere ↔ libvirt)

1. Create the S3 credentials Secret in the migration's namespace:

   ```bash
   kubectl create secret generic s3-migration-credentials \
     --from-literal=accessKeyID=YOUR_ACCESS_KEY \
     --from-literal=secretAccessKey=YOUR_SECRET_KEY
   ```

2. For a vSphere **target**, define the destination network so the migrated VM
   gets a NIC (skip for a libvirt target):

   ```yaml
   apiVersion: infra.virtrigaud.io/v1beta1
   kind: VMNetworkAttachment
   metadata:
     name: vm-network
   spec:
     network:
       type: bridged
       vsphere:
         portgroup: "VM Network"
         adapterType: vmxnet3
     ipAllocation:
       type: DHCP
   ```

3. Customise the example YAML (`libvirt-to-vsphere.yaml` or `../vmmigration-s3.yaml`)
   — source/target VM names, providerRefs, the S3 `endpoint`/`bucket`, and
   `target.networks` — then apply and monitor:

   ```bash
   kubectl apply -f your-migration.yaml
   kubectl get vmmigration your-migration-name -w   # Validating→Exporting→Importing→Creating→Ready
   kubectl describe vmmigration your-migration-name
   ```

The Proxmox directions (`vsphere-to-proxmox.yaml`, `proxmox-to-libvirt.yaml`) use
the same S3/relay shape. Note that the Proxmox **Provider's** Secret must carry
both an API token (`token_id`/`token_secret`) and SSH credentials (`ssh_user` +
`ssh_password` and/or `ssh_privatekey`) plus a pinned `known_hosts`, because the
Proxmox disk data plane runs `qemu-img`/`qm` on the PVE node over SSH.

## NFS staging backend (ADR-0006 Slice 4)

NFS is the validated **second** staging backend. Instead of an object store, the
disk is staged on an NFS export and moved with **qemu-img's native transport** —
no PVC, no provider-pod relay, no S3 credentials. It is validated across **all
three providers in both directions** (libvirt ↔ vSphere ↔ Proxmox). Set
`storage.type: nfs` with `storage.nfs.{server,export,uid,gid}` — see
[`../vmmigration-nfs.yaml`](../vmmigration-nfs.yaml).

**Per-provider transport (an implementation detail, but it drives the
requirements below):**

| Provider | Who runs qemu-img | NFS client |
|----------|-------------------|------------|
| libvirt  | the libvirt **host** (over SSH) | qemu-img `nfs://` (libnfs) |
| vSphere  | the provider **pod** | qemu-img `nfs://` (libnfs) |
| Proxmox  | the PVE **node** | **kernel NFS mount** — `pve-qemu-kvm` ships no libnfs, so the node mounts the export (as its native NFS storage does) and qemu-img works against the mount |

**Operational requirements:**

- **`nfs.uid` / `nfs.gid` are effectively mandatory for cross-provider
  migrations.** AUTH_SYS authorizes by the numeric uid/gid the client presents,
  and each provider's qemu-img runs as a different identity (the libvirt SSH user,
  the vSphere pod's `uid 65532`, the Proxmox node's root). Set them to the uid/gid
  that **owns the export's files** so every leg can read *and* write the same
  staged object. (Proxmox presents them via `setpriv`; libvirt/vSphere via the
  libnfs URL.) Omitting them works only when every leg already presents a trusted
  uid.
- **Reachability + ACL.** The server must be reachable from each provider's data
  plane — the hypervisor host/node for libvirt/Proxmox, the provider **pod** for
  vSphere (its egress is a cluster node IP) — and the export ACL must allow those
  client IPs.
- **Host allowlist (C3).** The server host must pass the operator's
  `--migration-storage-allowed-hosts`; loopback/link-local/metadata targets are
  always rejected.
- **Flat staging object.** The staged disk is a single flat file in the export
  root, `vmmigrations-<ns>-<name>-<stage>.qcow2` (NFS, unlike S3 keys, cannot
  auto-create directories). One export per tenant keeps these from colliding.

**Hardening (ADR-0006 C6).** AUTH_SYS over NFSv3 is **cleartext with no Kerberos**
— the uid/gid is an assertion, not an authenticated identity. Run the migration
export only on a **trusted network**, keep **`root_squash` on**, give each tenant
its **own export**, and prefer narrow per-client ACLs over broad subnets. The disk
bytes themselves are not encrypted in transit.

## Reference

- [VMMigration CRD source](../../api/infra.virtrigaud.io/v1beta1/vmmigration_types.go)
- [Migration Guide](https://projectbeskar.github.io/virtrigaud/operations/vm-migration/)
- Open issues: [#147 mTLS](https://github.com/projectbeskar/virtrigaud/issues/147), [#148 provider auth](https://github.com/projectbeskar/virtrigaud/issues/148), [#153 Libvirt Clone stub](https://github.com/projectbeskar/virtrigaud/issues/153), [#154 Libvirt ImagePrepare stub](https://github.com/projectbeskar/virtrigaud/issues/154)
