# 15-Minute Quickstart

This guide will get you up and running with Virtrigaud in 15 minutes using both vSphere and Libvirt providers.

## Prerequisites

- Kubernetes cluster (1.24+)
- kubectl configured
- Helm 3.x
- Access to a vSphere environment (optional)
- Access to a Libvirt/KVM host (optional)

## API Support

**Default API**: v1beta1 - The recommended stable API for all new deployments.

**Legacy API**: v1beta1 - Served for compatibility but deprecated. See the [upgrade guide](../upgrade/) for migration instructions.

All resources support seamless conversion between API versions via webhooks.

## Step 1: Install Virtrigaud

### Using Helm (Recommended)

```bash
# Add the Virtrigaud Helm repository
helm repo add virtrigaud https://projectbeskar.github.io/virtrigaud
helm repo update

# Install with default settings (CRDs included automatically)
helm install virtrigaud virtrigaud/virtrigaud \
  --namespace virtrigaud-system \
  --create-namespace

# Or install with specific providers enabled
helm install virtrigaud virtrigaud/virtrigaud \
  --namespace virtrigaud-system \
  --create-namespace \
  --set providers.vsphere.enabled=true \
  --set providers.libvirt.enabled=true

# To skip CRDs if already installed separately
helm install virtrigaud virtrigaud/virtrigaud \
  --namespace virtrigaud-system \
  --create-namespace \
  --skip-crds
```

### Using Kustomize

```bash
# Clone the repository
git clone https://github.com/projectbeskar/virtrigaud.git
cd virtrigaud

# Apply base installation
kubectl apply -k deploy/kustomize/base

# Or apply with overlays
kubectl apply -k deploy/kustomize/overlays/standard
```

## Step 2: Verify Installation

```bash
# Check that the manager is running
kubectl get pods -n virtrigaud-system

# Check CRDs are installed
kubectl get crds | grep virtrigaud

# Verify API conversion is working (v1beta1 <-> v1beta1)
kubectl get crd virtualmachines.infra.virtrigaud.io -o yaml | yq '.spec.conversion'

# Check manager logs
kubectl logs -n virtrigaud-system deployment/virtrigaud-manager
```

## Step 3: Configure a Provider

### Option A: vSphere Provider

Create a secret with vSphere credentials:

```bash
kubectl create secret generic vsphere-credentials \
  --namespace default \
  --from-literal=endpoint=https://vcenter.example.com \
  --from-literal=username=administrator@vsphere.local \
  --from-literal=password=your-password \
  --from-literal=insecure=false
```

Create a vSphere provider:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  namespace: default
spec:
  type: vsphere
  endpoint: https://vcenter.example.com
  credentialSecretRef:
    name: vsphere-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-vsphere:latest"
    service:
      port: 9090
  defaults:
    datastore: "datastore1"
    cluster: "cluster1"
    folder: "virtrigaud-vms"
```

### Option B: Libvirt Provider

Create a secret with Libvirt connection details:

```bash
kubectl create secret generic libvirt-credentials \
  --namespace default \
  --from-literal=uri=qemu+ssh://root@libvirt-host.example.com/system \
  --from-literal=username=root \
  --from-literal=privateKey="$(cat ~/.ssh/id_rsa)"
```

Create a Libvirt provider:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: libvirt-lab
  namespace: default
spec:
  type: libvirt
  endpoint: qemu+ssh://root@libvirt-host.example.com/system
  credentialSecretRef:
    name: libvirt-credentials
  runtime:
    mode: Remote
    image: "virtrigaud/provider-libvirt:latest"
    service:
      port: 9090
  defaults:
    defaultStoragePool: "default"
    defaultNetwork: "default"
```

Apply the provider configuration:

```bash
kubectl apply -f provider.yaml
```

## Step 4: Create a VM Class

Define resource templates for your VMs:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClass
metadata:
  name: small
  namespace: default
spec:
  cpu: 2
  memoryMiB: 2048
  disks:
  - name: root
    sizeGiB: 20
    type: thin
  networks:
  - name: default
    type: "VM Network"  # vSphere network name
```

```bash
kubectl apply -f vmclass.yaml
```

## Step 5: Create a VM Image

Define the base image for your VMs:

### vSphere Image (OVA)

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-20-04
  namespace: virtrigaud-system
spec:
  source:
    vsphere:
      ovaURL: "https://cloud-images.ubuntu.com/releases/20.04/ubuntu-20.04-server-cloudimg-amd64.ova"
      checksum: "sha256:abc123..."
      datastore: "datastore1"
      folder: "vm-templates"
  prepare:
    onMissing: Import
    timeout: "30m"
```

### Libvirt Image (qcow2)

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-20-04
  namespace: virtrigaud-system
spec:
  source:
    libvirt:
      qcow2URL: "https://cloud-images.ubuntu.com/releases/20.04/ubuntu-20.04-server-cloudimg-amd64.img"
      checksum: "sha256:def456..."
      storagePool: "default"
  prepare:
    onMissing: Import
    timeout: "30m"
```

```bash
kubectl apply -f vmimage.yaml
```

## Step 6: Create Your First VM

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-first-vm
  namespace: default
spec:
  providerRef:
    name: vsphere-prod  # or libvirt-lab
    namespace: default
  classRef:
    name: small
    namespace: default
  imageRef:
    name: ubuntu-20-04
    namespace: default
  powerState: "On"
  userData:
    cloudInit:
      inline: |
        #cloud-config
        users:
          - name: ubuntu
            sudo: ALL=(ALL) NOPASSWD:ALL
            ssh_authorized_keys:
              - ssh-rsa AAAAB3... your-public-key
        packages:
          - curl
          - vim
  networks:
  - name: default
    networkRef:
      name: default-network
      namespace: default
```

```bash
kubectl apply -f vm.yaml
```

## Step 7: Monitor VM Creation

```bash
# Watch VM status
kubectl get vm my-first-vm -w

# Check detailed status
kubectl describe vm my-first-vm

# View events
kubectl get events --field-selector involvedObject.name=my-first-vm

# Check provider logs
kubectl logs -n virtrigaud-system deployment/virtrigaud-provider-vsphere
```

## Step 8: Access Your VM

```bash
# Get VM IP address
kubectl get vm my-first-vm -o jsonpath='{.status.ips[0]}'

# Get console URL (if supported)
kubectl get vm my-first-vm -o jsonpath='{.status.consoleURL}'

# SSH to the VM (once it has an IP)
ssh ubuntu@<vm-ip>
```

## Step 9: Try Advanced Operations

### Create a Snapshot

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSnapshot
metadata:
  name: my-vm-snapshot
  namespace: default
spec:
  vmRef:
    name: my-first-vm
  nameHint: "pre-update-snapshot"
  memory: true
```

### Clone the VM

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClone
metadata:
  name: my-vm-clone
  namespace: default
spec:
  sourceRef:
    name: my-first-vm
  target:
    name: cloned-vm
    classRef:
      name: small
      namespace: default
  linked: true
```

### Scale with VMSet

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSet
metadata:
  name: web-servers
  namespace: default
spec:
  replicas: 3
  template:
    spec:
      providerRef:
        name: vsphere-prod
        namespace: default
      classRef:
        name: small
        namespace: default
      imageRef:
        name: ubuntu-20-04
        namespace: default
      powerState: "On"
```

## Step 10: Clean Up

```bash
# Delete VM
kubectl delete vm my-first-vm

# Delete snapshots and clones
kubectl delete vmsnapshot my-vm-snapshot
kubectl delete vmclone my-vm-clone
kubectl delete vmset web-servers

# Uninstall Virtrigaud (optional)
helm uninstall virtrigaud -n virtrigaud-system
kubectl delete namespace virtrigaud-system
```

## Next Steps

- Explore the [VM Lifecycle Guide](../user-guides/virtual-machines.md)
- Learn about [Advanced Networking](../user-guides/networking.md)
- Set up [Monitoring and Observability](../admin-guides/monitoring.md)
- Configure [Security and RBAC](../admin-guides/security.md)
- Read the [Provider Development Guide](../developer/provider-guide.md)

## Troubleshooting

If you encounter issues:

1. Check the [Troubleshooting Guide](../admin-guides/troubleshooting.md)
2. Verify your provider credentials and connectivity
3. Check the manager and provider logs
4. Ensure your Kubernetes cluster meets the requirements
5. File an issue on [GitHub](https://github.com/projectbeskar/virtrigaud/issues)
