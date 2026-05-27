# VirtRigaud Examples

Verified against v0.3.6 on 2026-05-27.

All examples use `apiVersion: infra.virtrigaud.io/v1beta1` and the 10 CRDs that exist in v0.3.6.

## Known limitations (v0.3.6)

Before applying any example, read these constraints:

- **mTLS not default-on**: gRPC between manager and provider pods is TLS-capable but mTLS is not wired end-to-end. See [#147](https://github.com/projectbeskar/virtrigaud/issues/147), [#148](https://github.com/projectbeskar/virtrigaud/issues/148).
- **Libvirt Clone RPC is a stub**: the Clone gRPC is declared but not implemented for Libvirt in v0.3.6. See [#153](https://github.com/projectbeskar/virtrigaud/issues/153).
- **Libvirt ImagePrepare RPC is a stub**: see [#154](https://github.com/projectbeskar/virtrigaud/issues/154).
- **Only vSphere → Libvirt/KVM migration is tested**: other migration directions are roadmap. See `migration/README.md`.
- **Migration storage**: only `type: pvc` is supported. `s3`, `http`, and `nfs` URI types do not exist in the API.
- **Proxmox credentials**: read from files at `/etc/virtrigaud/credentials/`; do NOT use `envFrom: secretRef`.

For full documentation see [https://projectbeskar.github.io/virtrigaud](https://projectbeskar.github.io/virtrigaud).

---

## Quick Start Examples

| File | Description |
|------|-------------|
| [complete-example.yaml](complete-example.yaml) | Complete end-to-end: Provider + VMClass + VMImage + VMNetworkAttachment + VirtualMachine |
| [vm-ubuntu-small.yaml](vm-ubuntu-small.yaml) | Simple Ubuntu VM |
| [vmclass-small.yaml](vmclass-small.yaml) | VMClass resource profile |
| [vmimage-ubuntu.yaml](vmimage-ubuntu.yaml) | VMImage configuration |
| [vmnetwork-app.yaml](vmnetwork-app.yaml) | VMNetworkAttachment configuration |

## Provider Examples

| File | Description |
|------|-------------|
| [provider-vsphere.yaml](provider-vsphere.yaml) | vSphere provider configuration |
| [provider-libvirt.yaml](provider-libvirt.yaml) | Libvirt/KVM provider configuration |
| [vm-adoption-example.yaml](vm-adoption-example.yaml) | Provider CRs with VMAdoption annotation |

## Provider-Specific Examples

| File | Description |
|------|-------------|
| [vsphere-advanced-example.yaml](vsphere-advanced-example.yaml) | Advanced vSphere VM configuration |
| [vsphere-hardware-versions.yaml](vsphere-hardware-versions.yaml) | vSphere hardware version management |
| [libvirt-complete-example.yaml](libvirt-complete-example.yaml) | Complete Libvirt/KVM deployment |
| [libvirt-advanced-example.yaml](libvirt-advanced-example.yaml) | Advanced Libvirt configuration |
| [proxmox-complete-example.yaml](proxmox-complete-example.yaml) | Complete Proxmox VE setup |
| [multi-provider-example.yaml](multi-provider-example.yaml) | vSphere + Libvirt + Proxmox in one cluster |

## Feature-Specific Examples

| File | Description |
|------|-------------|
| [graceful-shutdown-examples.yaml](graceful-shutdown-examples.yaml) | OffGraceful power state |
| [disk-sizing-examples.yaml](disk-sizing-examples.yaml) | Disk size configuration |
| [nested-virtualization.yaml](nested-virtualization.yaml) | Nested virtualisation (vSphere) |
| [cloud-init-with-metadata.yaml](cloud-init-with-metadata.yaml) | Cloud-init with metadata |
| [vm-scsi-controllers.yaml](vm-scsi-controllers.yaml) | SCSI controller configuration (vSphere only) |

## v0.2.x Showcase (historical reference)

| File | Description |
|------|-------------|
| [v021-feature-showcase.yaml](v021-feature-showcase.yaml) | v0.2.1 features comprehensive demo (historical; some fields may be superseded by v0.3.x schema changes) |

## Migration Examples

See [`migration/`](migration/) for cross-provider migration examples. All migration examples use `storage.type: pvc`.

## Secrets Examples

See [`secrets/`](secrets/) for credential Secret examples per provider:
- `vsphere-creds.yaml` — `username` + `password`
- `libvirt-creds.yaml` — `username` + `ssh-privatekey` (or `password`)
- `proxmox-creds.yaml` — `token_id` + `token_secret` (or `username` + `password`)

## Advanced Examples

See [`advanced/`](advanced/) for snapshot lifecycle, console access, task tracking, and VM cloning scenarios.

## Security Examples

See [`security/`](security/) for NetworkPolicy, RBAC, and ExternalSecrets patterns.

## File Organization

```
examples/
├── README.md                    # This file
├── complete-example.yaml        # Full end-to-end
├── vm-ubuntu-small.yaml
├── vmclass-small.yaml
├── vmimage-ubuntu.yaml
├── vmnetwork-app.yaml
├── provider-vsphere.yaml
├── provider-libvirt.yaml
├── vm-adoption-example.yaml
├── vsphere-advanced-example.yaml
├── vsphere-hardware-versions.yaml
├── libvirt-complete-example.yaml
├── libvirt-advanced-example.yaml
├── proxmox-complete-example.yaml
├── multi-provider-example.yaml
├── vmmigration-basic.yaml       # PVC-only storage
├── vmmigration-advanced.yaml    # PVC-only, all options
├── vmmigration-nfs.yaml         # NFS-backed StorageClass
├── vmmigration-s3.yaml          # Renamed from s3 — now PVC-based
├── disk-sizing-examples.yaml
├── graceful-shutdown-examples.yaml
├── cloud-init-with-metadata.yaml
├── nested-virtualization.yaml
├── vm-scsi-controllers.yaml
├── v021-feature-showcase.yaml
├── advanced/                    # Snapshots, consoles, cloning, task tracking
├── migration/                   # Cross-provider migration examples
├── secrets/                     # Per-provider Secret templates
└── security/                    # NetworkPolicy, RBAC, ExternalSecrets
```

## Version compatibility

These examples target **v0.3.6**. For earlier versions, check git history.
