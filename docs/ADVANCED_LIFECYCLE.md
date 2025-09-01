# Advanced VM Lifecycle Management

This document describes the advanced VM lifecycle features in Virtrigaud, including reconfiguration, snapshots, cloning, multi-VM sets, and placement policies.

## Overview

Virtrigaud Stage E introduces comprehensive VM lifecycle management capabilities that go beyond basic create/delete operations:

- **VM Reconfiguration**: Modify CPU, memory, and disk resources of running VMs
- **Snapshot Management**: Create, delete, and revert VM snapshots
- **VM Cloning**: Create new VMs from existing ones with linked clone support
- **Multi-VM Sets**: Manage groups of VMs with rolling updates
- **Placement Policies**: Advanced placement rules and anti-affinity constraints
- **Image Preparation**: Automated image import and preparation workflows

## VM Reconfiguration

### Online vs Offline Reconfiguration

Virtrigaud supports both online (hot) and offline reconfiguration depending on provider capabilities:

**vSphere**: Supports online CPU/memory changes and hot disk expansion
**Libvirt**: Typically requires power cycle for resource changes

### Example: CPU/Memory Upgrade

```yaml
# Original VM with 2 CPU, 4GB RAM
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: web-server
spec:
  resources:
    cpu: 2
    memoryMiB: 4096

# Patch to upgrade resources
# kubectl patch vm web-server --type merge -p '{"spec":{"resources":{"cpu":4,"memoryMiB":8192}}}'
```

The controller will:
1. Detect resource changes in VM spec
2. Attempt online reconfiguration if supported
3. If offline required, orchestrate graceful power cycle:
   - Set condition `ReconfigurePendingPowerCycle=True`
   - Power off VM gracefully
   - Apply reconfiguration
   - Power on VM
   - Update `status.lastReconfigureTime`

### Disk Expansion

```yaml
spec:
  disks:
    - name: data
      sizeGiB: 100  # Expanded from 50GB
      expandPolicy: "Online"  # Try online first
```

## Snapshot Management

### Creating Snapshots

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSnapshot
metadata:
  name: pre-maintenance-backup
spec:
  vmRef:
    name: web-server
  nameHint: "maintenance-backup"
  memory: true  # Include memory state
  description: "Backup before maintenance"
  retentionPolicy:
    maxAge: "7d"
    deleteOnVMDelete: true
```

### Snapshot Lifecycle

1. **Creating**: Snapshot creation in progress
2. **Ready**: Snapshot available for use
3. **Deleting**: Snapshot being removed
4. **Failed**: Snapshot operation failed

### Reverting to Snapshots

```yaml
# Patch VM to revert to snapshot
spec:
  snapshot:
    revertToRef:
      name: pre-maintenance-backup
```

The controller will:
1. Power off VM if running
2. Call provider's SnapshotRevert RPC
3. Power on VM
4. Clear `revertToRef` when complete

## VM Cloning

### Basic Cloning

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMClone
metadata:
  name: web-server-clone
spec:
  sourceRef:
    name: web-server
  target:
    name: web-server-test
    classRef:
      name: test-class
  linked: true  # Faster, space-efficient
  powerOn: true
```

### Clone Customization

```yaml
spec:
  customization:
    hostname: web-server-test
    networks:
      - name: primary
        ipAddress: "192.168.1.100"
        gateway: "192.168.1.1"
        dns: ["8.8.8.8"]
    userData:
      cloudInit:
        inline: |
          #cloud-config
          runcmd:
            - echo "Test environment" > /etc/motd
```

## Multi-VM Sets (VMSet)

VMSets provide declarative management of multiple VMs with rolling updates.

### Basic VMSet

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMSet
metadata:
  name: web-tier
spec:
  replicas: 3
  selector:
    matchLabels:
      app: web-server
  template:
    metadata:
      labels:
        app: web-server
    spec:
      providerRef:
        name: vsphere-prod
      classRef:
        name: web-class
      imageRef:
        name: nginx-image
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 1
```

### Rolling Updates

When you update the template spec, VMSet will:
1. Create new VMs with updated configuration
2. Wait for new VMs to be ready
3. Delete old VMs respecting `maxUnavailable`
4. Continue until all replicas are updated

## Placement Policies

### Advanced Placement Rules

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMPlacementPolicy
metadata:
  name: production-policy
spec:
  hard:
    clusters: ["prod-cluster-1", "prod-cluster-2"]
    datastores: ["ssd-datastore-1", "ssd-datastore-2"]
    hosts: ["esxi-01", "esxi-02", "esxi-03"]
  soft:
    folders: ["/Production/WebServers"]
    zones: ["zone-a", "zone-b"]
  antiAffinity:
    hostAntiAffinity: true      # Spread across hosts
    clusterAntiAffinity: false
    datastoreAntiAffinity: true # Spread across datastores
```

### Using Placement Policies

```yaml
spec:
  placementRef:
    name: production-policy
```

The provider will attempt to satisfy:
1. **Hard constraints**: Must be satisfied
2. **Soft constraints**: Best effort
3. **Anti-affinity rules**: Avoid co-location

## Image Preparation

### Automated Image Import

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VMImage
metadata:
  name: ubuntu-22-04
spec:
  vsphere:
    ovaURL: "https://releases.ubuntu.com/22.04/ubuntu-22.04-server.ova"
    checksum: "sha256:abcd1234..."
  libvirt:
    url: "https://cloud-images.ubuntu.com/22.04/ubuntu-22.04-server.img"
    format: "qcow2"
  prepare:
    onMissing: "Import"  # Auto-import if missing
    validateChecksum: true
    timeout: "30m"
    retries: 3
    storage:
      vsphere:
        datastore: "images-datastore"
        folder: "/Templates"
        thinProvisioned: true
```

### Image Preparation Phases

1. **Pending**: Waiting to start preparation
2. **Importing**: Downloading/importing image
3. **Preparing**: Processing image (conversion, etc.)
4. **Ready**: Image ready for use
5. **Failed**: Preparation failed

## Provider Capabilities

Different providers support different features. Query capabilities:

```yaml
# Example capabilities response
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
status:
  capabilities:
    supportsReconfigureOnline: true      # vSphere: true, Libvirt: false
    supportsDiskExpansionOnline: true    # vSphere: true, Libvirt: false
    supportsSnapshots: true              # Both: true
    supportsMemorySnapshots: true        # vSphere: true, Libvirt: varies
    supportsLinkedClones: true           # Both: true
    supportsImageImport: true            # Both: true
    supportedDiskTypes: ["thin", "thick"]
    supportedNetworkTypes: ["VMXNET3", "E1000"]
```

## Observability

### Metrics

New metrics for advanced lifecycle operations:

```
virtrigaud_vm_reconfigure_total{provider_type,outcome}
virtrigaud_vm_snapshot_total{action,provider_type,outcome}
virtrigaud_vm_clone_total{linked,provider_type,outcome}
virtrigaud_vm_image_prepare_total{provider_type,outcome}
```

### Events

Detailed events for lifecycle operations:

```
Normal   SnapshotCreating    Started snapshot creation
Normal   SnapshotReady       Snapshot created successfully
Normal   ReconfigureStarted  Started VM reconfiguration
Warning  ReconfigurePowerCycle  Reconfiguration requires power cycle
Normal   CloneCompleted      VM clone created successfully
```

### Conditions

Comprehensive condition reporting:

**VM Conditions**:
- `Ready`: VM is ready for use
- `Provisioning`: VM is being created
- `Reconfiguring`: VM is being reconfigured
- `ReconfigurePendingPowerCycle`: Needs power cycle for changes

**Snapshot Conditions**:
- `Ready`: Snapshot is ready
- `Creating`: Snapshot being created
- `Deleting`: Snapshot being deleted

**Clone Conditions**:
- `Ready`: Clone completed successfully
- `Cloning`: Clone operation in progress
- `Customizing`: Applying customizations

## Best Practices

### Snapshot Management

1. **Retention Policies**: Always set appropriate retention policies
2. **Memory Snapshots**: Use sparingly due to storage overhead
3. **Cleanup**: Implement automated cleanup for old snapshots
4. **Testing**: Test snapshot revert procedures regularly

### VM Reconfiguration

1. **Gradual Changes**: Make incremental resource changes
2. **Monitoring**: Monitor VM performance after changes
3. **Rollback Plan**: Have snapshots before major changes
4. **Capacity Planning**: Ensure host resources before scaling up

### Placement Policies

1. **Start Simple**: Begin with basic constraints
2. **Test Anti-Affinity**: Verify rules work as expected
3. **Monitor Placement**: Check actual VM placement matches policy
4. **Balance Performance**: Don't over-constrain placement

### Multi-VM Operations

1. **Rolling Updates**: Use appropriate `maxUnavailable` settings
2. **Health Checks**: Implement proper readiness checks
3. **Monitoring**: Monitor rollout progress
4. **Rollback Strategy**: Plan for rollback scenarios

## Troubleshooting

### Common Issues

**Reconfiguration Fails**:
- Check provider capabilities
- Verify resource availability on host
- Check for VM tools/agent issues

**Snapshot Operations Fail**:
- Verify storage backend supports snapshots
- Check available storage space
- Ensure VM is not in transitional state

**Clone Customization Issues**:
- Verify network configuration
- Check cloud-init/guest tools
- Validate IP address availability

**Placement Policy Violations**:
- Check resource availability in target locations
- Verify anti-affinity rules aren't too restrictive
- Review cluster resource distribution

### Debugging

```bash
# Check VM reconfiguration status
kubectl describe vm web-server

# Monitor snapshot progress
kubectl get vmsnapshots -w

# Check clone status
kubectl describe vmclone web-server-clone

# Review placement policy usage
kubectl describe vmplacementpolicy production-policy

# Check VMSet rollout
kubectl describe vmset web-tier
```

## Migration from Basic VMs

Existing VMs can be enhanced with advanced features:

1. **Add Placement Policy**: Update VM spec with `placementRef`
2. **Enable Reconfiguration**: Add resource overrides
3. **Create Snapshots**: Deploy VMSnapshot resources
4. **Scale with VMSets**: Migrate to VMSet for multi-instance workloads

The controller maintains backward compatibility with existing VM definitions.
