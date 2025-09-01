# Quick Start Guide

This guide will help you get started with virtrigaud and create your first virtual machine using either the vSphere or Libvirt/KVM provider.

## Prerequisites

- Kubernetes cluster (1.24+)
- kubectl configured to access your cluster
- **For vSphere**: Access to a vSphere environment with:
  - vCenter Server
  - At least one datacenter, cluster, and datastore
  - VM template or base image for cloning
  - Network (portgroup) for VM connectivity
  - User account with VM management permissions
- **For Libvirt**: Access to a Libvirt/KVM environment with:
  - Libvirt daemon running (local or remote)
  - At least one storage pool configured
  - Base qcow2 images for VM creation
  - Network configuration (default network or custom bridges)
  - Appropriate user permissions for VM management

## Installation

1. **Install the CRDs**:
   ```bash
   make install
   ```

2. **Deploy the operator**:
   ```bash
   make deploy
   ```

   Or run locally for development:
   ```bash
   make run
   ```

## Create Your First VM

Choose either the vSphere or Libvirt path below based on your infrastructure.

## Option A: vSphere Provider

### Step 1: Create Credentials Secret

Create a secret with your vSphere credentials:

```bash
kubectl create secret generic vsphere-creds \
  --from-literal=username=administrator@vsphere.local \
  --from-literal=password=your-password-here
```

### Step 2: Configure the Provider

Create a Provider resource pointing to your vCenter:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: my-vsphere
spec:
  type: vsphere
  endpoint: https://vcenter.example.com
  credentialSecretRef:
    name: vsphere-creds
  insecureSkipVerify: true  # For dev/test only
  defaults:
    datastore: datastore1
    cluster: my-cluster
```

Apply it:
```bash
kubectl apply -f provider.yaml
```

### Step 3: Define VM Class

Create a VMClass defining the resource allocation:

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

### Step 4: Define VM Image

Create a VMImage referencing your vSphere template:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-template
spec:
  vsphere:
    templateName: "ubuntu-20.04-template"
```

### Step 5: Define Network

Create a VMNetworkAttachment for VM networking:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: vm-network
spec:
  vsphere:
    portgroup: "VM Network"
  ipPolicy: dhcp
```

### Step 6: Create the Virtual Machine

Finally, create your VM:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-first-vm
spec:
  providerRef:
    name: my-vsphere
  classRef:
    name: small
  imageRef:
    name: ubuntu-template
  networks:
    - name: vm-network
  powerState: On
  userData:
    cloudInit:
      inline: |
        #cloud-config
        hostname: my-first-vm
        users:
          - name: ubuntu
            sudo: ALL=(ALL) NOPASSWD:ALL
            shell: /bin/bash
            ssh_authorized_keys:
              - ssh-rsa AAAAB... # Your SSH public key
```

Apply it:
```bash
kubectl apply -f vm.yaml
```

### Step 7: Monitor the VM

Watch the VM creation progress:

```bash
# Check VM status
kubectl get virtualmachine my-first-vm

# Watch for changes
kubectl get virtualmachine my-first-vm -w

# Get detailed information
kubectl describe virtualmachine my-first-vm
```

Expected output:
```
NAME          PROVIDER     CLASS   IMAGE            POWER   IPS           READY   AGE
my-first-vm   my-vsphere   small   ubuntu-template  On      192.168.1.50  True    2m
```

## Option B: Libvirt/KVM Provider

### Step 1: Create Credentials Secret

For local connections, create a minimal secret (credentials may not be needed):

```bash
kubectl create secret generic libvirt-creds \
  --from-literal=username=virtrigaud \
  --from-literal=password=not-used-for-local
```

For remote connections, provide actual credentials:
```bash
kubectl create secret generic libvirt-creds \
  --from-literal=username=your-username \
  --from-literal=password=your-password
```

### Step 2: Configure the Provider

Create a Provider resource pointing to your Libvirt daemon:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: my-libvirt
spec:
  type: libvirt
  endpoint: qemu:///system  # Local connection
  # For remote: qemu+tcp://kvm-host.example.com/system
  credentialSecretRef:
    name: libvirt-creds
  defaults:
    cluster: default
```

### Step 3: Define VM Class

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: kvm-small
spec:
  cpu: 2
  memoryMiB: 2048
  firmware: BIOS
  diskDefaults:
    type: qcow2
    sizeGiB: 20
```

### Step 4: Define VM Image

Create a VMImage referencing your qcow2 image:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-kvm
spec:
  libvirt:
    path: "/var/lib/libvirt/images/ubuntu-20.04.qcow2"
    format: qcow2
```

### Step 5: Define Network

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMNetworkAttachment
metadata:
  name: kvm-network
spec:
  libvirt:
    networkName: "default"
    model: virtio
  ipPolicy: dhcp
```

### Step 6: Create the Virtual Machine

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-kvm-vm
spec:
  providerRef:
    name: my-libvirt
  classRef:
    name: kvm-small
  imageRef:
    name: ubuntu-kvm
  networks:
    - name: kvm-network
  powerState: On
  userData:
    cloudInit:
      inline: |
        #cloud-config
        hostname: my-kvm-vm
        users:
          - name: ubuntu
            sudo: ALL=(ALL) NOPASSWD:ALL
            shell: /bin/bash
            ssh_authorized_keys:
              - ssh-rsa AAAAB... # Your SSH public key
```

### Step 7: Monitor the VM

```bash
kubectl get virtualmachine my-kvm-vm -w
```

## Troubleshooting

### Check Provider Health

```bash
kubectl describe provider my-vsphere
```

Look for conditions that indicate connectivity issues.

### Check Controller Logs

```bash
kubectl logs -n virtrigaud-system deployment/virtrigaud-controller-manager
```

### Common Issues

1. **Authentication Failed**: Verify credentials in the secret
2. **Template Not Found**: Ensure the template exists in vSphere
3. **Network Issues**: Check that the portgroup name is correct
4. **Resource Not Available**: Verify datastore and cluster names

## Next Steps

- Explore more examples in the `examples/` directory
- Read the [CRD documentation](docs/CRDs.md) for all available options
- Check out [advanced examples](docs/EXAMPLES.md) for complex scenarios
- Learn about [provider development](docs/PROVIDERS.md) to add new hypervisors

## Complete Examples

For complete working examples with all components, see:

**vSphere:**
```bash
kubectl apply -f examples/complete-example.yaml
```

**Libvirt/KVM:**
```bash
kubectl apply -f examples/libvirt-complete-example.yaml
```

**Multi-provider (both vSphere and Libvirt):**
```bash
kubectl apply -f examples/multi-provider-example.yaml
```

Each creates a full stack with provider, credentials, classes, and VM in one file.
