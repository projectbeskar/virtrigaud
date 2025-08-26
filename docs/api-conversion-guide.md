# API Conversion Guide

This guide explains how virtrigaud handles API version conversion between v1alpha1 and v1beta1.

## Overview

Virtrigaud supports automatic conversion between API versions using Kubernetes conversion webhooks. This allows:

- **Backward compatibility**: Existing v1alpha1 resources continue to work
- **Forward migration**: New features are available in v1beta1  
- **Seamless transition**: No manual migration required
- **Storage consolidation**: All data is stored as v1beta1 regardless of input version

## API Version Support

| Version | Status | Storage | Served | Features |
|---------|--------|---------|--------|----------|
| v1alpha1 | Deprecated | ❌ | ✅ | Basic VM lifecycle |
| v1beta1 | Stable | ✅ | ✅ | Enhanced validation, new features |

### Deprecation Timeline

- **v0.1.x**: Both v1alpha1 and v1beta1 served, v1beta1 storage
- **v0.2.x**: v1alpha1 served with deprecation warnings  
- **v0.3.x**: v1alpha1 removed (planned)

## Field Mapping

### VirtualMachine

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|-------|
| `spec.networks[].staticIP` | `spec.networks[].ipAddress` | Renamed for clarity |
| `spec.networks[].ipPolicy` | `spec.networks[].ipAddress=""` | DHCP when empty |
| `spec.disks[].sizeGiB` | `spec.storage.disks[].size` | Uses resource.Quantity |
| `spec.tags[]` | `spec.metadata.labels{}` | Array to map conversion |
| `spec.powerState` (string) | `spec.powerState` (enum) | Validated enum values |

### Provider

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|-------|
| `spec.type` (string) | `spec.type` (enum) | Validated enum values |
| `spec.endpoint` | `spec.endpoint` | No change |
| `spec.credentialSecretRef` | `spec.credentialSecretRef` | No change |
| `spec.insecure` | Deprecated | Moved to connection.tls.insecure |

### VMClass

| v1alpha1 Field | v1beta1 Field | Notes |
|----------------|---------------|-------|
| `spec.memoryMiB` | `spec.memory` | Uses resource.Quantity (e.g., "4Gi") |
| `spec.diskDefaults.sizeGiB` | `spec.diskDefaults.size` | Uses resource.Quantity |
| `spec.guestToolsPolicy` | `spec.guestTools.policy` | Moved to nested structure |

## Conversion Examples

### Creating Resources

You can create resources using either API version:

```yaml
# v1alpha1 format
apiVersion: infra.virtrigaud.io/v1alpha1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  providerRef: {name: "vsphere-prod"}
  classRef: {name: "medium"}
  imageRef: {name: "ubuntu-22"}
  networks:
  - name: app-net
    ipPolicy: dhcp
  disks:
  - name: data
    sizeGiB: 100
    type: thin
  powerState: "On"
  tags:
  - "environment:prod"
  - "app:web"
```

This automatically converts to v1beta1 format internally:

```yaml
# v1beta1 format (internal storage)
apiVersion: infra.virtrigaud.io/v1beta1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  providerRef: {name: "vsphere-prod"}
  classRef: {name: "medium"}
  imageRef: {name: "ubuntu-22"}
  networks:
  - name: app-net
    networkRef: {name: "app-net"}
    ipAddress: ""  # Empty = DHCP
  storage:
    disks:
    - name: data
      size: 100Gi
      type: thin
  powerState: On
  metadata:
    labels:
      environment: "prod"
      app: "web"
```

### Reading Resources

Regardless of how you created the resource, you can read it in either format:

```bash
# Read as v1alpha1
kubectl get vm my-vm -o yaml --show-managed-fields=false | grep apiVersion
# Output: apiVersion: infra.virtrigaud.io/v1alpha1

# Read as v1beta1  
kubectl get vms.v1beta1.infra.virtrigaud.io my-vm -o yaml | grep apiVersion
# Output: apiVersion: infra.virtrigaud.io/v1beta1
```

## Validation During Conversion

### Enhanced Validation in v1beta1

v1beta1 includes stricter validation that may reject invalid v1alpha1 resources:

```yaml
# This v1alpha1 resource would be rejected
apiVersion: infra.virtrigaud.io/v1alpha1
kind: VirtualMachine
spec:
  powerState: "InvalidState"  # Only "On"/"Off" allowed in v1beta1
```

### Conversion Errors

If conversion fails, you'll see clear error messages:

```
error validating data: ValidationError(VirtualMachine.spec.powerState): 
invalid value: "InvalidState", supported values: "On", "Off"
```

## Testing Conversion

### Automated Tests

Run the conversion test suite:

```bash
# Unit tests
go test ./api/... -v

# Integration tests with envtest
go test ./test/conversione2e -v -timeout 10m
```

### Manual Testing

```bash
# Create test resources in both versions
kubectl apply -f examples/upgrade/alpha/
kubectl apply -f examples/upgrade/beta/

# Verify they can be read in both formats
kubectl get vm --show-managed-fields=false
kubectl get vms.v1beta1.infra.virtrigaud.io --show-managed-fields=false
```

## Best Practices

### For New Deployments

- **Use v1beta1**: Start with the stable API version
- **Enable validation**: Use `kubectl apply --validate=true` 
- **Test conversions**: Verify examples work in your environment

### For Existing Deployments

- **Gradual migration**: Update examples and docs to v1beta1
- **Test compatibility**: Ensure existing resources still work
- **Monitor deprecation**: Watch for v1alpha1 removal timeline

### For CI/CD Pipelines

```yaml
# Validate resources against current schema
- name: Validate resources
  run: |
    kubectl apply --dry-run=server --validate=true -f manifests/
    
# Test conversion works
- name: Test conversion
  run: |
    kubectl apply -f examples/upgrade/alpha/
    kubectl get vms.v1beta1.infra.virtrigaud.io
```

## Troubleshooting

### Common Issues

**Issue**: `conversion webhook call failed`
**Solution**: Check webhook service and certificates
```bash
kubectl get svc virtrigaud-webhook -n virtrigaud
kubectl logs -l app.kubernetes.io/name=virtrigaud -n virtrigaud
```

**Issue**: `field is immutable`  
**Solution**: Some fields can't be changed after creation
```bash
# Check resource status for error details
kubectl describe vm my-vm
```

**Issue**: `unsupported conversion path`
**Solution**: Ensure all CRDs have conversion webhook configured
```bash
./hack/verify-crd-conversion.sh
```

### Debug Conversion

Enable debug logging in the manager:

```yaml
# In manager deployment
env:
- name: LOG_LEVEL
  value: "debug"
```

View conversion logs:
```bash
kubectl logs -l app.kubernetes.io/name=virtrigaud -n virtrigaud | grep conversion
```

## Migration Tools

### Convert Examples Script

```bash
#!/bin/bash
# convert-examples.sh - Convert v1alpha1 examples to v1beta1

for file in examples/*.yaml; do
  if grep -q "v1alpha1" "$file"; then
    echo "Converting $file..."
    # Apply as alpha, read back as beta
    kubectl apply --dry-run=client -f "$file"
    kubectl get $(yq '.kind' "$file") $(yq '.metadata.name' "$file") \
      -o yaml --dry-run=client > "${file%.yaml}-v1beta1.yaml"
  fi
done
```

### Validation Script

```bash
#!/bin/bash
# validate-conversion.sh - Test all resources convert properly

kubectl apply --dry-run=server -f examples/upgrade/alpha/
echo "✅ All v1alpha1 resources valid"

kubectl apply --dry-run=server -f examples/upgrade/beta/  
echo "✅ All v1beta1 resources valid"
```
