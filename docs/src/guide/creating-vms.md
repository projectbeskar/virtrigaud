# Creating VMs

Step-by-step guide to creating your first virtual machine with VirtRigaud.

## Prerequisites

- VirtRigaud controller installed
- Provider configured
- VMClass and VMImage resources created

## Step 1: Create a VMClass

Define VM size and resource allocation:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: medium
spec:
  cpus: 4
  memory: 8Gi
```

## Step 2: Create a VirtualMachine

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-vm
  namespace: default
spec:
  providerRef:
    name: my-provider
  vmClassName: medium
  powerState: "on"
  guestInfo:
    hostname: my-vm.example.com
```

## Step 3: Apply and Verify

```bash
kubectl apply -f vm.yaml
kubectl get vm my-vm
kubectl describe vm my-vm
```

## What's Next?

- [VM Configuration](vm-configuration.md) - Advanced configuration options
- [VM Lifecycle](../advanced-lifecycle.md) - Manage VM power state and lifecycle
