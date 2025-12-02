# Provider Configuration

Configure hypervisor providers for VirtRigaud.

## Overview

Providers represent connections to hypervisors (vSphere, Libvirt, Proxmox).

## Provider Types

- [vSphere Provider](../providers/vsphere.md) - VMware vCenter/ESXi
- [Libvirt Provider](../providers/libvirt.md) - KVM/QEMU
- [Proxmox Provider](../providers/proxmox.md) - Proxmox VE

## Creating a Provider

Example vSphere provider:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: my-vsphere
spec:
  type: vsphere
  endpoint: https://vcenter.example.com
  credentials:
    secretRef:
      name: vsphere-creds
  datacenter: DC1
  resourcePool: /DC1/host/Cluster1/Resources
```

## Security

See the [Security](../advanced/security.md) chapter for credential management best practices.

## What's Next?

- [Provider Tutorial](../providers/tutorial.md) - Step-by-step provider setup
- [Creating VMs](creating-vms.md) - Create VMs with your provider
