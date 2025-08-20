# Quick Start Guide

This guide will help you get started with virtrigaud and create your first virtual machine using the vSphere provider.

## Prerequisites

- Kubernetes cluster (1.24+)
- kubectl configured to access your cluster
- Access to a vSphere environment with:
  - vCenter Server
  - At least one datacenter, cluster, and datastore
  - VM template or base image for cloning
  - Network (portgroup) for VM connectivity
  - User account with VM management permissions

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
apiVersion: infra.virtrigaud.io/v1alpha1
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
apiVersion: infra.virtrigaud.io/v1alpha1
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
apiVersion: infra.virtrigaud.io/v1alpha1
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
apiVersion: infra.virtrigaud.io/v1alpha1
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
apiVersion: infra.virtrigaud.io/v1alpha1
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

## Complete Example

For a complete working example with all components, see:
```bash
kubectl apply -f examples/complete-example.yaml
```

This creates a full stack with provider, credentials, classes, and VM in one file.
