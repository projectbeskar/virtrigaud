# Examples

This document provides practical examples for using virtrigaud.

## Basic VM Creation

This example demonstrates creating a simple VM on vSphere.

### 1. Create Credentials Secret

```bash
kubectl create secret generic vsphere-creds \
  --from-literal=username=administrator@vsphere.local \
  --from-literal=password=your-password
```

### 2. Create Provider

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
spec:
  type: vsphere
  endpoint: https://vcenter.example.com
  credentialSecretRef:
    name: vsphere-creds
  defaults:
    datastore: datastore1
    cluster: compute-cluster
    folder: virtrigaud-vms
```

### 3. Create VM Class

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: small
spec:
  cpu: 2
  memoryMiB: 4096
  firmware: UEFI
  diskDefaults:
    type: thin
    sizeGiB: 40
```

### 4. Create VM Image

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22
spec:
  vsphere:
    templateName: ubuntu-22.04-cloudimg
```

### 5. Create Network Attachment

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: vm-network
spec:
  vsphere:
    portgroup: VM Network
  ipPolicy: dhcp
```

### 6. Create Virtual Machine

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server-01
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  networks:
    - name: vm-network
  powerState: On
  userData:
    cloudInit:
      inline: |
        #cloud-config
        packages:
          - nginx
        runcmd:
          - systemctl enable nginx
          - systemctl start nginx
```

## Multi-Disk VM

Example with additional data disks:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: database-server
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: large
  imageRef:
    name: ubuntu-22
  networks:
    - name: db-network
  disks:
    - name: data-disk-1
      sizeGiB: 100
      type: thick
    - name: data-disk-2
      sizeGiB: 200
      type: thin
  powerState: On
```

## Static IP Configuration

Example with static IP assignment:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: static-network
spec:
  vsphere:
    portgroup: Static-Network
  ipPolicy: static

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: static-vm
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  networks:
    - name: static-network
      ipPolicy: static
      staticIP: 192.168.1.100
  powerState: On
```

## Cloud-Init with Secret

Using a secret for cloud-init configuration:

### 1. Create Cloud-Init Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: web-server-config
type: Opaque
stringData:
  cloud-init: |
    #cloud-config
    users:
      - name: admin
        sudo: ALL=(ALL) NOPASSWD:ALL
        shell: /bin/bash
        ssh_authorized_keys:
          - ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAB...
    packages:
      - nginx
      - certbot
    runcmd:
      - systemctl enable nginx
      - systemctl start nginx
      - ufw allow 'Nginx Full'
      - ufw enable
```

### 2. Reference Secret in VM

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server-02
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  networks:
    - name: vm-network
  userData:
    cloudInit:
      secretRef:
        name: web-server-config
        key: cloud-init
  powerState: On
```

## High Memory VM Class

Custom VM class for memory-intensive workloads:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: high-memory
spec:
  cpu: 8
  memoryMiB: 32768  # 32 GB
  firmware: UEFI
  diskDefaults:
    type: thick
    sizeGiB: 100
  extraConfig:
    mem.hotadd: "true"
    vcpu.hotadd: "true"
```

## Libvirt Provider Example

Configuration for Libvirt/KVM:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-local
spec:
  type: libvirt
  endpoint: qemu+tcp://kvm-host.local/system
  credentialSecretRef:
    name: libvirt-creds

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22-kvm
spec:
  libvirt:
    url: https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img
    format: qcow2
    checksum: b8e9f8f8e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9
    checksumType: sha256

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: bridge-network
spec:
  libvirt:
    bridge: br0
    model: virtio
  ipPolicy: dhcp
```

## VM with Custom Placement

Specify exact placement for the VM:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: placed-vm
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  networks:
    - name: vm-network
  placement:
    datastore: ssd-datastore
    cluster: production-cluster
    folder: web-tier
  tags:
    - environment:production
    - tier:web
    - owner:team-alpha
  powerState: On
```

## Multiple Network Interfaces

VM with multiple network connections:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: frontend-network
spec:
  vsphere:
    portgroup: Frontend-Network
  ipPolicy: dhcp

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: backend-network
spec:
  vsphere:
    portgroup: Backend-Network
  ipPolicy: dhcp

---
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: multi-nic-vm
spec:
  providerRef:
    name: vsphere-prod
  classRef:
    name: small
  imageRef:
    name: ubuntu-22
  networks:
    - name: frontend-network
    - name: backend-network
  powerState: On
```

## VM Lifecycle Management

### Power Off VM

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server-01
spec:
  # ... other fields remain the same
  powerState: Off
```

### Delete VM

```bash
kubectl delete virtualmachine web-server-01
```

The controller will:
1. Power off the VM if it's running
2. Delete the VM from the hypervisor
3. Remove the Kubernetes resource

## Monitoring VM Status

Check VM status:

```bash
# List all VMs
kubectl get virtualmachine

# Get detailed status
kubectl describe virtualmachine web-server-01

# Watch for changes
kubectl get virtualmachine -w
```

Example output:

```
NAME            PROVIDER       CLASS   IMAGE      POWER   IPS             READY   AGE
web-server-01   vsphere-prod   small   ubuntu-22  On      192.168.1.50    True    5m
```

## Troubleshooting

### Check Provider Health

```bash
kubectl get provider vsphere-prod -o yaml
```

Look for conditions in the status section.

### Check VM Conditions

```bash
kubectl describe virtualmachine web-server-01
```

Look for events and conditions to understand any issues.

### Controller Logs

```bash
kubectl logs -n virtrigaud-system controller-manager-xxx
```
