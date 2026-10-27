# Advanced VirtRigaud Examples

This directory contains advanced examples: cloning, task tracking, console access, snapshots and reconfiguration.

## Overview

Each file is a set of manifests with comments on what the controllers and providers do with them. Replace the placeholder names (providers, templates, networks, credentials) before applying.

## Examples

### vSphere Clone Operations
**File**: [vsphere-clone-example.yaml](vsphere-clone-example.yaml)

Creating VMs from a vSphere template, and cloning an existing VM:
- **VMs from a template image**: several `VirtualMachine`s whose `imageRef` names the same vSphere template; each is created by cloning that template.
- **`VMClone`**: clones an existing `VirtualMachine` (`source.vmRef`) into a new one on the same provider. `FullClone` is the default; `LinkedClone` needs a provider that reports linked-clone support (vSphere does; libvirt refuses it in v0.4.1, see [../../docs/libvirt-clones.md](../../docs/libvirt-clones.md)).
- A `VMClone` from a template or a `VMImage` is declared in the schema but not implemented; see [../vmclone-basic.yaml](../vmclone-basic.yaml) for the supported scope.

### vSphere Task Tracking
**File**: [vsphere-task-tracking.yaml](vsphere-task-tracking.yaml)

How the manager follows the long-running vSphere operations a provider starts (create, clone, reconfigure, snapshot, delete): the provider returns a task reference, and the manager polls the provider's `TaskStatus` RPC until the task ends. The file also holds example `kubectl` commands for watching a VM, its events and the provider log, and for finding the task in vCenter's task console.

### Console Access
**File**: [console-access-example.yaml](console-access-example.yaml)

Remote VM console access for both vSphere and Libvirt:

**vSphere Web Console**:
- A vSphere web client URL in `status.consoleURL`; the console itself opens in the vCenter UI, with your vCenter login

**Libvirt VNC Console**:
- A `vnc://<libvirt host>:<port>` URL in `status.consoleURL` while the VM is on
- Connecting with a VNC client, through an SSH tunnel or a noVNC proxy

### VM Reconfiguration and Snapshots
**File**: [vm-reconfigure-and-snapshot.yaml](vm-reconfigure-and-snapshot.yaml)

Demonstrates VM lifecycle management:
- Resource reconfiguration, and what each provider does with it (see [../../docs/reconfigure-results.md](../../docs/reconfigure-results.md))
- A snapshot before the change
- A `VMClone` of the VM, a `VMSet`
- A placement policy

### Snapshot Lifecycle
**File**: [snapshot-lifecycle.yaml](snapshot-lifecycle.yaml)

Snapshot management:
- `VMSnapshot` creation, with and without memory state
- Retention policies (`maxAge`, `deleteOnVMDelete`)
- Reverting a VM to a snapshot with `spec.snapshot.revertToRef` ([../../docs/vm-snapshot-revert.md](../../docs/vm-snapshot-revert.md))

### VM Reconfiguration Patch
**File**: [vm-reconfigure-patch.yaml](vm-reconfigure-patch.yaml)

Demonstrates changing a VM's CPU, memory and disks with `kubectl patch`. Whether the change applies live or at the next power cycle depends on the provider ([../../docs/reconfigure-results.md](../../docs/reconfigure-results.md)).

## Getting Started

### Prerequisites

1. **VirtRigaud Installed**:
   ```bash
   helm repo add virtrigaud https://projectbeskar.github.io/virtrigaud
   helm install virtrigaud virtrigaud/virtrigaud --version 0.4.1 \
     -n virtrigaud-system --create-namespace
   ```

2. **Provider Configured**:
   - vSphere: vCenter credentials and access
   - Libvirt: SSH access to the libvirt host (`qemu+ssh://`)
   - Proxmox: Proxmox VE API access

3. **Base Resources**:
   - VMClass definitions
   - VMImage configurations
   - Network attachments

### Usage Pattern

1. **Review Example**:
   ```bash
   # Read the example file
   cat vsphere-clone-example.yaml
   ```

2. **Customize for Your Environment**:
   - Update provider endpoints
   - Adjust resource specifications
   - Modify network configurations
   - Update credentials references

3. **Deploy**:
   ```bash
   kubectl apply -f vsphere-clone-example.yaml
   ```

4. **Monitor**:
   ```bash
   # Watch VMs
   kubectl get vm -w
   
   # Check provider logs
   kubectl logs -f deployment/virtrigaud-provider-<namespace>-<provider name>
   
   # View VM details
   kubectl describe vm <vm-name>
   ```

5. **Access and Verify**:
   ```bash
   # Get console URL
   kubectl get vm <vm-name> -o jsonpath='{.status.consoleURL}'
   
   # SSH to VM
   ssh user@<vm-ip>
   ```

## Advanced Patterns

### Resource Management

Change a VM's size by patching `spec.resources` (the VMClass is the baseline):
```bash
kubectl patch vm <vm-name> --type='merge' \
  -p='{"spec":{"resources":{"cpu":4,"memoryMiB":8192}}}'
```
or switch to another class with `spec.classRef.name`. See [vm-reconfigure-patch.yaml](vm-reconfigure-patch.yaml).

## Best Practices

### 1. Resource Planning
- Use linked clones for dev/test environments only where the provider supports them; a linked clone depends on its source's disk
- Reserve full clones for production workloads

### 2. Security
- Use SSH tunnels for VNC connections
- Implement network policies
- Keep credentials in Secrets, not in manifests

### 3. Operations
- Monitor provider logs during complex operations
- Use the VM's events and conditions, and the provider log, for troubleshooting

## Troubleshooting

### Common Issues

#### Clone Operations Fail
```bash
# Check source template
kubectl describe vmimage <image-name>

# Verify storage capacity
kubectl logs deployment/virtrigaud-provider-<namespace>-<provider name> | grep -i storage

# Check vCenter for detailed errors
```

#### Console URL Not Generated
```bash
# Verify VM is running
kubectl get vm <vm-name> -o jsonpath='{.status.phase}'

# Check provider logs
kubectl logs deployment/virtrigaud-provider-<namespace>-<provider name> | grep -i console

# Restart provider if needed
kubectl rollout restart deployment/virtrigaud-provider-<namespace>-<provider name>
```

#### Task Tracking Shows Errors
```bash
# Get task details from provider logs
kubectl logs deployment/virtrigaud-provider-<namespace>-<provider name> | grep task-<id>

# Check vCenter task manager
# Navigate to vCenter > Tasks & Events

# Review VM events
kubectl get events --field-selector involvedObject.name=<vm-name>
```

## Further Reading

- [Main Examples README](../README.md)
- [Documentation index](../../docs/README.md)

## Contributing

Have an advanced example to share? Contributions welcome!

1. Create your example YAML
2. Document use cases and features
3. Include troubleshooting guidance
4. Submit a pull request

---

**Note**: These examples target VirtRigaud v0.4.1.

