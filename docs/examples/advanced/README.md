# Advanced VirtRigaud Examples

This directory contains advanced examples demonstrating complex scenarios and v0.2.3+ features.

## Overview

These examples showcase production-ready patterns, advanced operations, and best practices for managing VMs at scale with VirtRigaud.

## v0.2.3+ Features

### vSphere Clone Operations
**File**: [vsphere-clone-example.yaml](vsphere-clone-example.yaml)

Comprehensive cloning examples demonstrating:
- **Full Clones**: Independent VMs for production workloads
- **Linked Clones**: Space-efficient copies for development/testing
- **Automatic Snapshot Handling**: Seamless snapshot creation for linked clones
- **Clone from VMs or Templates**: Flexible source options
- **Rapid Provisioning**: Multiple parallel clones for test environments

**Use Cases**:
- Production workload deployment
- Development environment provisioning
- Test environment creation
- VM backup and disaster recovery
- Rapid scaling for temporary workloads

**Key Features**:
- Full clone: ~5-15 minutes, independent storage
- Linked clone: ~1-3 minutes, shared storage with parent
- Automatic snapshot management
- Complete isolation or shared storage options

### vSphere Task Tracking
**File**: [vsphere-task-tracking.yaml](vsphere-task-tracking.yaml)

Real-time monitoring of long-running vSphere operations:
- **Task Progress Monitoring**: Real-time progress percentages
- **State Reporting**: Queued, running, success, error states
- **Error Information**: Detailed error messages from vSphere
- **vCenter Integration**: Direct correlation with vSphere task manager
- **Parallel Operations**: Independent tracking for multiple VMs

**Monitored Operations**:
- VM creation and cloning
- Resource reconfiguration
- Snapshot operations
- VM deletion
- Disk expansion

**Benefits**:
- Visibility into operation progress
- Debugging failed operations
- Performance optimization insights
- Capacity planning data
- Integration with vCenter task console

### Console Access
**File**: [console-access-example.yaml](console-access-example.yaml)

Remote VM console access for both vSphere and Libvirt:

**vSphere Web Console**:
- Automatic URL generation
- Browser-based access via vCenter
- Full keyboard/mouse support
- Copy/paste functionality
- Multi-monitor support
- Power controls integration

**Libvirt VNC Console**:
- VNC URL generation
- Multiple VNC client support (TigerVNC, RealVNC, noVNC)
- SSH tunnel support for security
- Web-based access via noVNC
- Multiple simultaneous viewers

**Use Cases**:
- Initial VM setup before SSH
- Boot troubleshooting
- OS installation
- GUI application access
- Rescue and recovery operations
- Training and demonstrations

## Existing Advanced Examples

### VM Reconfiguration and Snapshots
**File**: [vm-reconfigure-and-snapshot.yaml](vm-reconfigure-and-snapshot.yaml)

Demonstrates VM lifecycle management:
- Resource reconfiguration
- Snapshot creation before changes
- Rollback capabilities
- Placement policies

### Snapshot Lifecycle
**File**: [snapshot-lifecycle.yaml](snapshot-lifecycle.yaml)

Complete snapshot management:
- Snapshot creation with memory state
- Retention policies
- Scheduled snapshots
- Snapshot deletion and cleanup

### VM Reconfiguration Patch
**File**: [vm-reconfigure-patch.yaml](vm-reconfigure-patch.yaml)

Demonstrates dynamic reconfiguration:
- Live resource updates
- Kubectl patch operations
- Zero-downtime changes (where supported)

## Getting Started

### Prerequisites

1. **VirtRigaud Installed**:
   ```bash
   helm install virtrigaud virtrigaud/virtrigaud -n virtrigaud-system
   ```

2. **Provider Configured**:
   - vSphere: vCenter credentials and access
   - Libvirt: Libvirt daemon connection
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
   kubectl logs -f deployment/virtrigaud-provider-vsphere
   
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

### Batch Operations

Deploy multiple VMs simultaneously:
```bash
# Deploy all examples
kubectl apply -f advanced/

# Monitor all VMs
watch -n 2 'kubectl get vm'
```

### Resource Management

Update VM resources dynamically:
```bash
# Trigger reconfiguration
kubectl patch vm <vm-name> --type='merge' \
  -p='{"spec":{"vmClassRef":"larger-class"}}'
```

### Automation Scripts

Use provided scripts for common tasks:
```bash
# Console access automation
kubectl create configmap console-scripts \
  --from-file=console-access-example.yaml

# Execute script
kubectl exec <pod> -- bash /scripts/automation.sh <vm-name>
```

## Best Practices

### 1. Resource Planning
- Use linked clones for dev/test environments
- Reserve full clones for production workloads
- Monitor task durations for capacity planning

### 2. Security
- Use SSH tunnels for VNC connections
- Implement network policies
- Rotate console access credentials
- Enable audit logging

### 3. Operations
- Monitor provider logs during complex operations
- Use task tracking for troubleshooting
- Implement proper cleanup procedures
- Document custom configurations

### 4. Performance
- Distribute VMs across datastores
- Use linked clones to reduce storage load
- Monitor vCenter task queue depth
- Implement rate limiting for bulk operations

## Troubleshooting

### Common Issues

#### Clone Operations Fail
```bash
# Check source template
kubectl describe vmimage <image-name>

# Verify storage capacity
kubectl logs deployment/virtrigaud-provider-vsphere | grep -i storage

# Check vCenter for detailed errors
```

#### Console URL Not Generated
```bash
# Verify VM is running
kubectl get vm <vm-name> -o jsonpath='{.status.phase}'

# Check provider logs
kubectl logs deployment/virtrigaud-provider-vsphere | grep -i console

# Restart provider if needed
kubectl rollout restart deployment/virtrigaud-provider-vsphere
```

#### Task Tracking Shows Errors
```bash
# Get task details from provider logs
kubectl logs deployment/virtrigaud-provider-vsphere | grep task-<id>

# Check vCenter task manager
# Navigate to vCenter > Tasks & Events

# Review VM events
kubectl get events --field-selector involvedObject.name=<vm-name>
```

## Example Combinations

### Complete Development Environment
```bash
# 1. Create base template VM
kubectl apply -f vsphere-clone-example.yaml  # Base template

# 2. Create multiple dev VMs via linked clones
kubectl apply -f - <<EOF
<linked-clone-definitions>
EOF

# 3. Access via console for initial setup
kubectl get vm dev-vm-01 -o jsonpath='{.status.consoleURL}'

# 4. Monitor all operations
kubectl get vm -w
```

### Production Deployment Pipeline
```bash
# 1. Full clone from approved template
# 2. Task tracking for provisioning
# 3. Automated testing via console
# 4. Snapshot before production deployment
# 5. Deploy to production
```

## Further Reading

- [Main Examples README](../README.md)
- [Provider Documentation](../../providers/)
- [vSphere Provider Guide](../../providers/vsphere.md)
- [Libvirt Provider Guide](../../providers/libvirt.md)
- [Provider Capabilities Matrix](../../PROVIDERS_CAPABILITIES.md)

## Contributing

Have an advanced example to share? Contributions welcome!

1. Create your example YAML
2. Document use cases and features
3. Include troubleshooting guidance
4. Submit a pull request

---

**Note**: These examples use v0.2.3+ features. Ensure you're running the latest VirtRigaud version.

