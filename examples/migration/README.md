# VM Migration Examples

This directory contains examples for migrating VMs between hypervisor platforms using VirtRigaud.

## Migration model

VirtRigaud's validated cross-hypervisor migration is **storage-backend-agnostic**
(ADR-0006): the disk is staged through an object store (S3-compatible) and the
**provider pod is the S3 client**, so the bytes flow host → pod → S3 → pod → host
and **never traverse a CSI PVC**. The source exports its native disk format and
the target converts on import. This is the recommended path for the
vSphere ↔ libvirt directions.

A PVC-backed model also exists and is the basis for the (untested) Proxmox
roadmap examples below; it requires a ReadWriteMany StorageClass.

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
| [vsphere-to-proxmox.yaml](./vsphere-to-proxmox.yaml) | vSphere → Proxmox VE | PVC | Untested (roadmap) |
| [proxmox-to-libvirt.yaml](./proxmox-to-libvirt.yaml) | Proxmox VE → Libvirt/KVM | PVC | Untested (roadmap) |

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

For the PVC-backed roadmap examples (Proxmox), supply a ReadWriteMany
StorageClass and use `storage.type: pvc` as shown in those files.

## Reference

- [VMMigration CRD source](../../api/infra.virtrigaud.io/v1beta1/vmmigration_types.go)
- [Migration Guide](https://projectbeskar.github.io/virtrigaud/operations/vm-migration/)
- Open issues: [#147 mTLS](https://github.com/projectbeskar/virtrigaud/issues/147), [#148 provider auth](https://github.com/projectbeskar/virtrigaud/issues/148), [#153 Libvirt Clone stub](https://github.com/projectbeskar/virtrigaud/issues/153), [#154 Libvirt ImagePrepare stub](https://github.com/projectbeskar/virtrigaud/issues/154)
