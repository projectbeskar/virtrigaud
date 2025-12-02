# Prerequisites

Before installing VirtRigaud, ensure your environment meets these requirements.

## Kubernetes Cluster

- Kubernetes 1.24 or later
- `kubectl` configured to access your cluster
- Cluster admin permissions

## Hypervisor Access

At least one of the following hypervisors:
- VMware vSphere 7.0 or later
- Libvirt/KVM
- Proxmox VE 7.0 or later

## Tools

- Helm 3.8+ (for Helm installation method)
- `kubectl` 1.24+

## Network Requirements

- Cluster can reach hypervisor management APIs
- (Optional) LoadBalancer service support for remote providers

## What's Next?

Proceed to the [Quick Start Guide](../getting-started/quickstart.md) to begin installation.
