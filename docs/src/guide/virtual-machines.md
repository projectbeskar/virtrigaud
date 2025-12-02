# Managing Virtual Machines

Learn how to create, configure, and manage virtual machines with VirtRigaud.

## Overview

VirtRigaud provides Kubernetes-native VM management through the `VirtualMachine` custom resource.

## Topics

- [Creating VMs](creating-vms.md) - Step-by-step VM creation
- [VM Configuration](vm-configuration.md) - Configure CPU, memory, storage, networking
- [VM Lifecycle](../advanced-lifecycle.md) - Power operations and lifecycle management
- [Graceful Shutdown](../graceful-shutdown.md) - Handle VM shutdown gracefully

## Quick Example

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server
  namespace: default
spec:
  providerRef:
    name: my-vsphere-provider
  vmClassName: medium
  powerState: "on"
```

Apply with:

```bash
kubectl apply -f vm.yaml
```
