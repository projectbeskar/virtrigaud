# VirtRigaud Examples

All examples use `apiVersion: infra.virtrigaud.io/v1beta1` and the 12 CRDs of VirtRigaud v0.4.2. `make test` checks every `infra.virtrigaud.io` object here against the CRDs (see [Validation](#validation)). Pin images to the release you run: the examples use `v0.4.2`.

## Read before applying an example

- **A Provider needs a TLS decision.** The manager talks to provider pods over TLS by default. A `Provider` with no `spec.runtime.service.tls` block is not deployed (`TLSConfigured=False`, reason `TLSBlockMissing`). The Provider examples set `tls.enabled: true` with a `secretRef` to a Secret holding `tls.crt`, `tls.key` and `ca.crt`; create it first, or set `enabled: false` for a plaintext lab.
- **libvirt connects over SSH.** Use a `qemu+ssh://` endpoint. The credentials Secret needs `username`, `ssh-privatekey` (or `password`) and `known_hosts`; the provider does not trust a host key on first use. See [secrets/libvirt-creds.yaml](secrets/libvirt-creds.yaml).
- **libvirt linked clones are refused** in v0.4.2; use `FullClone` ([docs/libvirt-clones.md](../docs/libvirt-clones.md)).
- **Proxmox** reads its credentials from files in the Provider's Secret (`token_id` and `token_secret`, or `username` and `password`); it does not use `envFrom: secretRef`. It does not import images from a URL (`source.http`): use an existing template. It applies the whole user-data through a NoCloud seed it builds on the PVE node over SSH, so a Proxmox Provider whose VMs carry user-data needs `ssh_user`, `ssh_privatekey` (or `ssh_password`) and `known_hosts` in its Secret ([docs/proxmox-cloud-init-storage.md](../docs/proxmox-cloud-init-storage.md)). It makes full clones only (no `LinkedClone`).
- **`spec.lifecycle`** (`preStop`, `postStart`, `gracefulShutdownTimeout`) is accepted by the API, but no controller runs it. For a graceful shutdown use `spec.powerState: OffGraceful` ([graceful-shutdown-examples.yaml](graceful-shutdown-examples.yaml)).
- **Migration** is validated over S3 and NFS staging between vSphere, libvirt and Proxmox in any direction. `storage.type: pvc` is a legacy path that does not work for Proxmox. Migration into a clustered libvirt Provider is experimental. See [migration/README.md](migration/README.md).
- **Clustered libvirt** (`topology: cluster`) is experimental in v0.4.2 ([docs/clustered-provider-inventory.md](../docs/clustered-provider-inventory.md)).

For full documentation see the [documentation index](../docs/README.md).

---

## Quick Start Examples

| File | Description |
|------|-------------|
| [complete-example.yaml](complete-example.yaml) | Complete end-to-end: Provider + VMClass + VMImage + VMNetworkAttachment + VirtualMachine |
| [vm-ubuntu-small.yaml](vm-ubuntu-small.yaml) | Simple Ubuntu VM |
| [vmclass-small.yaml](vmclass-small.yaml) | VMClass resource profile |
| [vmimage-ubuntu.yaml](vmimage-ubuntu.yaml) | VMImage configuration |
| [vmimage-prepare-on-create.yaml](vmimage-prepare-on-create.yaml) | VMImage (libvirt URL source) + VirtualMachine showing the prepare-on-create flow (ADR-0005) |
| [vmclone-basic.yaml](vmclone-basic.yaml) | `VMClone` of an existing VirtualMachine on the same provider |
| [vmnetwork-app.yaml](vmnetwork-app.yaml) | VMNetworkAttachment configuration |

## Provider Examples

| File | Description |
|------|-------------|
| [provider-vsphere.yaml](provider-vsphere.yaml) | vSphere provider configuration |
| [provider-libvirt.yaml](provider-libvirt.yaml) | Libvirt/KVM provider configuration |
| [shared-provider-consumer-namespaces.yaml](shared-provider-consumer-namespaces.yaml) | A Provider, VMClass and VMImage shared with other namespaces via `spec.consumerNamespaceSelector`, and a tenant VM using them (see `docs/cross-namespace-references.md`) |
| [vm-adoption-example.yaml](vm-adoption-example.yaml) | Provider CRs with the `virtrigaud.io/adopt-vms` annotation, and how to detach a VM |

## Clustered Provider Examples (ADR-0007 P1)

| File | Description |
|------|-------------|
| [provider-libvirt-clustered.yaml](provider-libvirt-clustered.yaml) | A `topology: cluster` libvirt `Provider` — the operator renders its `Host` CRs into a `<provider>-hosts` Secret mounted at `/etc/virtrigaud/hosts/` (ADR-0007 D3). The Hosts and their SSH credentials must be in the Provider's namespace. |
| [hostpool-clustered.yaml](hostpool-clustered.yaml) | `HostPool` (Spread) + two `Host`s — the inventory a clustered libvirt Provider schedules onto. Additive; single-host providers unchanged. See [docs/clustered-provider-inventory.md](../docs/clustered-provider-inventory.md). |
| [host-libvirt-clustered-first-vm.yaml](host-libvirt-clustered-first-vm.yaml) | One `Host` labelled `net.virtrigaud.io/default: "true"` for the `VMNetworkAttachment` of libvirt's `default` network. A clustered VM is placed only on a Host labelled for every libvirt network it uses; without the label it stays `Unschedulable` ("host does not have a required network"). |
| [vmmigration-into-clustered.yaml](vmmigration-into-clustered.yaml) | Experimental. A `VMMigration` into a clustered Provider (ADR-0007 Addendum B): the target VM is created and placed first (on a host with room for the disk), the disk is imported to that host and attached in place. `s3` / `nfs` only; the provider must report `supports_routed_import`; the target VM is in the migration's namespace. See [Migration into a clustered provider](../docs/clustered-provider-inventory.md#migration-into-a-clustered-provider-addendum-b). |

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
| [v021-feature-showcase.yaml](v021-feature-showcase.yaml) | v0.2.1 features comprehensive demo (historical; it still passes the CRD dry-run) |

## Migration Examples

| File | Description |
|------|-------------|
| [vmmigration-s3.yaml](vmmigration-s3.yaml) | vSphere to libvirt over S3 |
| [vmmigration-proxmox-s3.yaml](vmmigration-proxmox-s3.yaml) | vSphere to Proxmox VE over S3 |
| [vmmigration-nfs.yaml](vmmigration-nfs.yaml) | Libvirt to vSphere over an NFS export |
| [vmmigration-basic.yaml](vmmigration-basic.yaml), [vmmigration-advanced.yaml](vmmigration-advanced.yaml) | Legacy PVC-backed staging (not for Proxmox or host-resident libvirt disks) |
| [vmmigration-into-clustered.yaml](vmmigration-into-clustered.yaml) | Into a clustered libvirt Provider (experimental) |

See [`migration/`](migration/) for the migration model, the NFS and PVC backends, and the per-direction examples (libvirt to vSphere, vSphere to Proxmox, Proxmox to libvirt).

## Secrets Examples

See [`secrets/`](secrets/) for credential Secret examples per provider:
- `vsphere-creds.yaml` — `username` + `password`
- `libvirt-creds.yaml` — `username` + `ssh-privatekey` (or `password`) + `known_hosts`
- `proxmox-creds.yaml` — `token_id` + `token_secret` (or `username` + `password`); `ssh_*` keys and `known_hosts` for migrations

## Advanced Examples

See [`advanced/`](advanced/) for snapshot lifecycle, console access, task tracking, and VM cloning scenarios.

## Security Examples

See [`security/`](security/) for NetworkPolicy, Secret and ExternalSecrets patterns. Provider pods run under a ServiceAccount the Provider controller creates and need no Kubernetes RBAC.

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
├── vmmigration-basic.yaml       # Legacy PVC staging
├── vmmigration-advanced.yaml    # Legacy PVC staging, all options
├── vmmigration-nfs.yaml         # NFS staging
├── vmmigration-s3.yaml          # S3 staging, vSphere to libvirt
├── vmmigration-proxmox-s3.yaml  # S3 staging, vSphere to Proxmox
├── vmmigration-into-clustered.yaml # Into a topology: cluster Provider (Addendum B)
├── vmclone-basic.yaml
├── vmimage-prepare-on-create.yaml
├── provider-libvirt-clustered.yaml, hostpool-clustered.yaml, host-libvirt-clustered-first-vm.yaml
├── shared-provider-consumer-namespaces.yaml
├── disk-sizing-examples.yaml
├── graceful-shutdown-examples.yaml
├── cloud-init-with-metadata.yaml
├── nested-virtualization.yaml
├── vm-scsi-controllers.yaml
├── v021-feature-showcase.yaml
├── advanced/                    # Snapshots, consoles, cloning, task tracking
├── migration/                   # Cross-provider migration examples
├── secrets/                     # Per-provider Secret templates
└── security/                    # NetworkPolicy, Secret, ExternalSecrets
```

## Validation

`make test` checks every `infra.virtrigaud.io` object in this directory
(`internal/controller/examples_envtest_test.go`): it decodes each file the way
`kubectl` does, runs a strict server-side dry-run against the CRDs in
`config/crd/bases/`, and decodes the result into the Go types. A file that does
not parse, an unknown field, a wrong type or an invalid enum value fails CI.

Quote `powerState` values (`"On"`, `"Off"`, `"OffGraceful"`): YAML 1.1, which
`kubectl` uses, reads bare `On` and `Off` as booleans.

## Version compatibility

These examples target **v0.4.2**. For earlier versions, check git history.
