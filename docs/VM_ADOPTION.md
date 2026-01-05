# VM Adoption Guide

VM Adoption allows VirtRigaud to discover and manage existing virtual machines in a provider that were not originally created by VirtRigaud. This feature enables you to onboard legacy VMs into VirtRigaud's management system.

## Overview

When VM adoption is enabled on a Provider resource, VirtRigaud will:

1. **Discover** all VMs managed by the provider
2. **Filter** VMs based on optional criteria (if specified)
3. **Exclude** VMs already managed by VirtRigaud
4. **Create** VirtualMachine Custom Resources (CRs) for unmanaged VMs
5. **Generate** default VMClass resources based on discovered VM properties

## Enabling VM Adoption

To enable VM adoption, add the `virtrigaud.io/adopt-vms: "true"` annotation to your Provider resource:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  annotations:
    virtrigaud.io/adopt-vms: "true"
spec:
  type: vsphere
  # ... provider configuration
```

Once enabled, the VM adoption controller will:

- Periodically scan the provider for unmanaged VMs (every hour by default)
- Create VirtualMachine CRs for discovered VMs
- Track adoption progress in the Provider status

## Filtering VMs

You can optionally filter which VMs should be adopted using the `virtrigaud.io/adopt-filter` annotation. This is useful when you want to adopt only specific VMs based on criteria like name patterns, power state, or resource requirements.

### Filter Format

The filter annotation accepts a JSON object with the following optional fields:

| Field | Type | Description |
|-------|------|-------------|
| `namePattern` | `string` | Regular expression pattern to match VM names |
| `powerState` | `string` | Filter by power state (e.g., "on", "off", "suspended") |
| `minCPU` | `int32` | Minimum number of CPUs required |
| `maxCPU` | `int32` | Maximum number of CPUs allowed |
| `minMemoryMiB` | `int64` | Minimum memory in MiB required |
| `maxMemoryMiB` | `int64` | Maximum memory in MiB allowed |

### Filter Examples

#### Example 1: Adopt Only Production VMs

Adopt only VMs with names starting with "prod-":

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  annotations:
    virtrigaud.io/adopt-vms: "true"
    virtrigaud.io/adopt-filter: '{"namePattern": "^prod-.*"}'
spec:
  type: vsphere
```

#### Example 2: Adopt Only Powered-On VMs

Adopt only VMs that are currently powered on:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  annotations:
    virtrigaud.io/adopt-vms: "true"
    virtrigaud.io/adopt-filter: '{"powerState": "on"}'
spec:
  type: vsphere
```

#### Example 3: Adopt VMs with Specific Resource Requirements

Adopt only VMs with at least 4 CPUs and 8 GiB of memory:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  annotations:
    virtrigaud.io/adopt-vms: "true"
    virtrigaud.io/adopt-filter: '{"minCPU": 4, "minMemoryMiB": 8192}'
spec:
  type: vsphere
```

#### Example 4: Complex Filter

Combine multiple criteria - adopt production VMs that are powered on with 2-8 CPUs:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  annotations:
    virtrigaud.io/adopt-vms: "true"
    virtrigaud.io/adopt-filter: '{"namePattern": "^prod-.*", "powerState": "on", "minCPU": 2, "maxCPU": 8}'
spec:
  type: vsphere
```

#### Example 5: Exclude Development VMs

Adopt all VMs except those with "dev" or "test" in their names:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  annotations:
    virtrigaud.io/adopt-vms: "true"
    virtrigaud.io/adopt-filter: '{"namePattern": "^(?!.*(dev|test)).*$"}'
spec:
  type: vsphere
```

### Filter Validation

- **Name Pattern**: Must be a valid regular expression. Invalid patterns will cause adoption to fail with an error message in the Provider status.
- **Power State**: Case-insensitive matching. Common values: "on", "off", "suspended", "poweredOn", "poweredOff".
- **Resource Constraints**: All constraints are inclusive (>= for min, <= for max).

## Adoption Status

The Provider status includes adoption tracking information:

```yaml
status:
  adoption:
    lastDiscoveryTime: "2025-01-15T10:30:00Z"
    discoveredVMs: 25
    adoptedVMs: 20
    failedAdoptions: 2
    message: "Successfully adopted 20 VMs"
```

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `lastDiscoveryTime` | `timestamp` | Time of last VM discovery scan |
| `discoveredVMs` | `int32` | Total number of unmanaged VMs discovered |
| `adoptedVMs` | `int32` | Number of VMs successfully adopted |
| `failedAdoptions` | `int32` | Number of VMs that failed to adopt |
| `message` | `string` | Human-readable status message |

## How Adoption Works

### Discovery Process

1. The VM adoption controller watches Provider resources with the adoption annotation
2. When adoption is enabled, the controller:
   - Calls the provider's `ListVMs()` method to discover all VMs
   - Filters out VMs already managed by VirtRigaud (by checking existing VirtualMachine CRs)
   - Applies the adoption filter (if specified)
   - Creates VirtualMachine CRs for matching unmanaged VMs

### VMClass Generation

For each adopted VM, VirtRigaud automatically creates a VMClass resource based on the VM's discovered properties:

- **CPU**: Uses the VM's current CPU count (defaults to 2 if not available)
- **Memory**: Uses the VM's current memory in MiB (defaults to 4096 MiB if not available)
- **Firmware**: Defaults to BIOS (can be enhanced to detect from VM)

VMClass names follow the pattern: `adopted-{cpu}cpu-{memory}mb`

Example: A VM with 4 CPUs and 8192 MiB memory will generate `adopted-4cpu-8192mb`

### VirtualMachine CR Creation

Adopted VMs are created with:

- **Labels**: `virtrigaud.io/adopted: "true"` to identify adopted VMs
- **ImportedDisk**: References the existing VM disk using the VM's ID
- **Status**: Pre-populated with VM ID, power state, IPs, and provider metadata

### Example Adopted VirtualMachine

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: prod-web-server-01
  labels:
    virtrigaud.io/adopted: "true"
spec:
  providerRef:
    name: vsphere-prod
    namespace: default
  classRef:
    name: adopted-4cpu-8192mb
    namespace: default
  importedDisk:
    diskID: vm-12345
    format: vmdk
    source: manual
  powerState: On
status:
  id: vm-12345
  powerState: On
  ips:
    - 192.168.1.100
```

## Disabling Adoption

To disable VM adoption, simply remove or set the annotation to `"false"`:

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: Provider
metadata:
  name: vsphere-prod
  annotations:
    virtrigaud.io/adopt-vms: "false"  # or remove the annotation
spec:
  type: vsphere
```

When disabled, the adoption status is cleared from the Provider status.

## Best Practices

1. **Start with Filters**: Use filters to adopt VMs incrementally, starting with a small subset
2. **Review Before Adoption**: Check the Provider status to see how many VMs will be adopted before enabling
3. **Use Name Patterns**: Leverage consistent VM naming conventions to filter effectively
4. **Monitor Status**: Regularly check the Provider adoption status for failed adoptions
5. **Test First**: Test adoption on a non-production provider first
6. **Backup**: Ensure you have backups before adopting critical VMs

## Troubleshooting

### Adoption Not Working

1. **Check Provider Status**: Ensure the Provider is healthy (`ProviderAvailable` condition is `True`)
2. **Check Logs**: Review controller logs for errors:
   ```bash
   kubectl logs -n virtrigaud-system deployment/virtrigaud-manager | grep adoption
   ```
3. **Verify Annotation**: Ensure the annotation is correctly set:
   ```bash
   kubectl get provider <provider-name> -o jsonpath='{.metadata.annotations.virtrigaud\.io/adopt-vms}'
   ```

### Filter Not Working

1. **Check Filter Syntax**: Validate JSON syntax and regex patterns
2. **Check Provider Status**: Invalid filters will show an error message in `status.adoption.message`
3. **Test Regex**: Test your regex pattern separately before using it

### VMs Not Being Adopted

1. **Already Managed**: Check if VMs already have VirtualMachine CRs
2. **Filter Too Restrictive**: Verify your filter criteria aren't excluding the VMs
3. **Provider Not Ready**: Ensure the Provider is healthy and ready

### Failed Adoptions

Check the Provider status for `failedAdoptions` count and review controller logs for specific error messages.

## Limitations

- **Periodic Scanning**: VM discovery runs periodically (every hour), not in real-time
- **VMClass Defaults**: Auto-generated VMClasses use default firmware (BIOS) and may need adjustment
- **Network Configuration**: Adopted VMs may need network attachments configured manually
- **Disk Format Detection**: Disk format detection depends on provider capabilities

## Related Documentation

- [Provider Documentation](PROVIDERS.md) - Provider configuration guide
- [CRDs Reference](CRDs.md) - Complete API reference
- [Advanced Lifecycle Management](ADVANCED_LIFECYCLE.md) - VM management features

