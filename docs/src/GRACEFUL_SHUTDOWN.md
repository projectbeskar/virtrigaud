# Graceful Shutdown Feature

The virtrigaud VM management platform now supports graceful shutdown of virtual machines to prevent data corruption and ensure proper cleanup of running processes.

## Overview

Graceful shutdown uses VM guest tools (VMware Tools, QEMU Guest Agent, etc.) to properly shut down the operating system before powering off the virtual machine. This prevents data corruption and allows applications to save their state properly.

## Power States

virtrigaud supports three power states:

- `On`: Power on the VM
- `Off`: Hard power off (immediate shutdown without guest OS notification)
- `OffGraceful`: Graceful shutdown using guest tools with automatic fallback to hard power off

## Configuration

### Basic Usage

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  powerState: OffGraceful  # Use graceful shutdown
  # ... other configuration
```

### Advanced Configuration with Lifecycle Hooks

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  powerState: OffGraceful
  
  lifecycle:
    # Timeout for graceful shutdown (default: 60s)
    gracefulShutdownTimeout: "120s"
    
    # Pre-stop hook runs before shutdown
    preStop:
      exec:
        command:
          - "/bin/bash"
          - "-c"
          - |
            # Save application state
            systemctl stop my-application
            # Sync filesystem
            sync
```

## How It Works

### vSphere Provider

1. **Guest Tools Check**: Verifies VMware Tools is installed and running
2. **Graceful Shutdown**: Calls `vm.ShutdownGuest()` to initiate OS shutdown
3. **Monitoring**: Polls VM power state every 2 seconds
4. **Timeout Handling**: Falls back to hard power off if timeout is reached
5. **Fallback**: Uses `vm.PowerOff()` if graceful shutdown fails

### Libvirt Provider

1. **Graceful Attempt**: Uses `virsh shutdown` command 
2. **Fallback**: Falls back to `virsh destroy` if shutdown fails
3. **Guest Agent**: Requires QEMU Guest Agent for best results

### Proxmox Provider

1. **API Call**: Uses Proxmox `shutdown` API endpoint
2. **Built-in Timeout**: Proxmox handles timeout and fallback internally

## Default Timeouts

- **vSphere**: 60 seconds (configurable via gRPC request)
- **Libvirt**: Immediate fallback if `virsh shutdown` fails
- **Proxmox**: Managed by Proxmox server configuration

## Requirements

### VMware vSphere
- VMware Tools must be installed and running in the guest OS
- Guest OS must support ACPI shutdown signals

### Libvirt/KVM
- QEMU Guest Agent recommended for reliable graceful shutdown
- Guest OS must support ACPI shutdown signals

### Proxmox
- QEMU Guest Agent recommended
- Guest OS must support ACPI shutdown signals

## Best Practices

1. **Always Install Guest Tools**: Ensure VMware Tools or QEMU Guest Agent is installed
2. **Test Graceful Shutdown**: Verify your VMs respond properly to shutdown signals
3. **Set Appropriate Timeouts**: Allow enough time for applications to shut down gracefully
4. **Use Lifecycle Hooks**: Implement pre-stop hooks for critical applications
5. **Monitor Logs**: Check provider logs to verify graceful shutdown is working

## Troubleshooting

### Graceful Shutdown Not Working

1. **Check Guest Tools Status**:
   ```bash
   # For VMware
   vmware-toolbox-cmd stat running
   
   # For QEMU/KVM
   systemctl status qemu-guest-agent
   ```

2. **Verify ACPI Support**:
   ```bash
   # Check if ACPI shutdown is supported
   cat /proc/acpi/button/power/*/info
   ```

3. **Test Manual Shutdown**:
   ```bash
   # Test graceful shutdown manually
   sudo shutdown -h now
   ```

### Timeout Issues

If VMs consistently hit the graceful shutdown timeout:

1. **Increase Timeout**: Set a longer `gracefulShutdownTimeout`
2. **Optimize Applications**: Ensure applications shut down quickly
3. **Check System Resources**: Verify the system isn't under heavy load

### Fallback to Hard Power Off

The provider will automatically fall back to hard power off if:
- Guest tools are not available
- Graceful shutdown times out
- Guest tools command fails

This ensures VMs are always powered off even if graceful shutdown isn't possible.

## Examples

See `examples/graceful-shutdown-vm.yaml` for complete examples of using graceful shutdown with various configurations.
