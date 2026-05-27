# VM Migration Examples

This directory contains examples for migrating VMs between hypervisor platforms using VirtRigaud.

## v0.3.6 migration constraints

Before using these examples, read the constraints:

- **Only PVC storage is supported.** `storage.type` is enum-constrained to `"pvc"` in the API. The s3, http, and nfs URI storage types documented in older resources do not exist in v0.3.6.
- **Only vSphere → Libvirt/KVM is tested.** Other migration directions are roadmap items and are marked with a WARNING in the respective example files.
- **Requires a ReadWriteMany StorageClass.** NFS, CephFS, AWS EFS, and Azure Files are common options.

## Examples

| File | Direction | Status |
|------|-----------|--------|
| [libvirt-to-vsphere.yaml](./libvirt-to-vsphere.yaml) | Libvirt/KVM → vSphere | Untested (roadmap) |
| [vsphere-to-proxmox.yaml](./vsphere-to-proxmox.yaml) | vSphere → Proxmox VE | Untested (roadmap) |
| [proxmox-to-libvirt.yaml](./proxmox-to-libvirt.yaml) | Proxmox VE → Libvirt/KVM | Untested (roadmap) |

For the tested vSphere → Libvirt/KVM path, see `examples/vmmigration-basic.yaml` in the parent directory.

## Quick start

1. Create a ReadWriteMany StorageClass (example using NFS CSI driver):

   ```yaml
   apiVersion: storage.k8s.io/v1
   kind: StorageClass
   metadata:
     name: nfs-migration-storage
   provisioner: nfs.csi.k8s.io
   parameters:
     server: nfs-server.example.com
     share: /exports/virtrigaud-migrations
   volumeBindingMode: Immediate
   reclaimPolicy: Delete
   ```

2. Customise the example YAML for your environment:

   ```yaml
   spec:
     source:
       vmRef:
         name: your-source-vm
     target:
       name: your-target-vm
       providerRef:
         name: your-target-provider
     storage:
       type: pvc
       pvc:
         storageClassName: nfs-migration-storage
         size: 200Gi
         accessMode: ReadWriteMany
   ```

3. Apply and monitor:

   ```bash
   kubectl apply -f your-migration.yaml
   kubectl get vmmigration your-migration-name -w
   kubectl describe vmmigration your-migration-name
   ```

## Reference

- [VMMigration CRD source](../../api/infra.virtrigaud.io/v1beta1/vmmigration_types.go)
- [Migration Guide](https://projectbeskar.github.io/virtrigaud/operations/vm-migration/)
- Open issues: [#147 mTLS](https://github.com/projectbeskar/virtrigaud/issues/147), [#148 provider auth](https://github.com/projectbeskar/virtrigaud/issues/148), [#153 Libvirt Clone stub](https://github.com/projectbeskar/virtrigaud/issues/153), [#154 Libvirt ImagePrepare stub](https://github.com/projectbeskar/virtrigaud/issues/154)
