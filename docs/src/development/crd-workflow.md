# CRD Development Workflow

This guide explains how to work with Custom Resource Definitions (CRDs) in VirtRigaud.

## Overview

VirtRigaud uses **Go types as the source of truth** for CRDs. The CRD YAML files in `config/crd/bases/` are **automatically generated** from Go type definitions in `api/infra.virtrigaud.io/v1beta1/` using [controller-gen](https://book.kubebuilder.io/reference/controller-gen.html).

This approach ensures:
- **Type safety** - Go compiler catches errors
- **Single source of truth** - No drift between code and deployed CRDs
- **Schema validation** - Kubernetes validates based on generated CRDs
- **Automatic synchronization** - Generated files stay in sync with code

## Quick Start

### 1. One-Time Setup (Recommended)

Install the git pre-commit hook to automatically regenerate CRDs:

```bash
make setup-git-hooks
```

This installs a hook that automatically:
- Detects changes to `*_types.go` files
- Regenerates CRD YAMLs and DeepCopy methods
- Syncs CRDs to the Helm chart
- Adds generated files to your commit
- Runs formatting and linting

### 2. Modify CRD Types

Edit your CRD type definitions:

```bash
vim api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go
```

### 3. Commit Changes

With hooks installed, just commit normally:

```bash
git add api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go
git commit -m "feat: add new field to VirtualMachine spec"
```

The hook will automatically:
1. Generate DeepCopy methods
2. Generate CRD YAML manifests
3. Sync CRDs to Helm chart
4. Add all generated files to your commit

## Manual Workflow

If you don't have git hooks installed, use this workflow:

### 1. Edit Go Types

Modify files in `api/infra.virtrigaud.io/v1beta1/`:

```go
// VirtualMachineSpec defines the desired state of VirtualMachine
type VirtualMachineSpec struct {
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`

    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Minimum=1
    CPUs int32 `json:"cpus"`

    // NEW FIELD: Add kubebuilder markers for validation
    // +kubebuilder:validation:Optional
    // +kubebuilder:default=false
    EnableGPU bool `json:"enableGPU,omitempty"`
}
```

### 2. Regenerate Everything

Use the convenient make target:

```bash
make update-crds
```

This runs:
- `make generate` - Generates DeepCopy methods
- `make manifests` - Generates CRD YAMLs
- `make sync-helm-crds` - Syncs to Helm chart

Or run individually:

```bash
# Generate DeepCopy methods
make generate

# Generate CRD manifests
make manifests

# Sync to Helm chart
make sync-helm-crds
```

### 3. Update Documentation and Examples

**CRITICAL:** After ANY CRD change, you MUST update:

```bash
# Update example YAML files
vim docs/examples/virtualmachine.yaml

# Update documentation
vim docs/src/crds.md
vim docs/src/getting-started/quickstart.md

# Validate all examples
kubectl apply --dry-run=client -f docs/examples/*.yaml
```

### 4. Verify Changes

```bash
# Verify CRDs are in sync
make verify-crd-sync

# Run tests
make test

# Run linter
make lint
```

### 5. Commit All Changes

```bash
git add api/ config/crd/ charts/virtrigaud/crds/ docs/
git commit -m "feat: add GPU support to VirtualMachine CRD"
```

## Kubebuilder Markers

Use kubebuilder markers to control CRD generation:

### Validation Markers

```go
type VirtualMachineSpec struct {
    // Required field
    // +kubebuilder:validation:Required
    Name string `json:"name"`

    // Minimum/Maximum values
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=128
    CPUs int32 `json:"cpus"`

    // String length
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    Hostname string `json:"hostname"`

    // Pattern matching
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    DNSName string `json:"dnsName"`

    // Enum values
    // +kubebuilder:validation:Enum=Running;Stopped;Paused
    PowerState string `json:"powerState"`

    // Default values
    // +kubebuilder:default=false
    AutoStart bool `json:"autoStart,omitempty"`
}
```

### Display Columns

```go
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="IP",type="string",JSONPath=".status.ipAddress"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type VirtualMachine struct {
    // ...
}
```

### Subresources

```go
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
type VMSet struct {
    // ...
}
```

### Resource Metadata

```go
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=vm;vms
// +kubebuilder:storageversion
type VirtualMachine struct {
    // ...
}
```

## Verification Commands

```bash
# Verify CRDs are in sync with Go types
make verify-crd-sync

# Verify Helm chart CRDs match generated CRDs
make verify-helm-crds

# Regenerate everything
make update-crds

# Install git hooks
make setup-git-hooks
```

## CI/CD Integration

The CI pipeline includes multiple layers of protection:

### 1. Verify CRDs Workflow

`.github/workflows/verify-crds.yml` runs on every PR that touches:
- `api/infra.virtrigaud.io/v1beta1/*_types.go`
- `config/crd/bases/*.yaml`
- `charts/virtrigaud/crds/*.yaml`

### 2. Generate Job

The `generate` job in `.github/workflows/ci.yml`:
- Regenerates all code and manifests
- Verifies no files changed (fails if out of sync)

### 3. Automatic PR Comments

If CRDs are out of sync, a bot comments with fix instructions:

```bash
make update-crds
git add api/ config/crd/ charts/virtrigaud/crds/
git commit --amend --no-edit
git push --force-with-lease
```

## Troubleshooting

### "CRD YAMLs are out of sync"

**Problem:** CI fails with "CRD YAML files are out of sync with Go types"

**Solution:**
```bash
make update-crds
git add config/crd/ charts/virtrigaud/crds/
git commit --amend --no-edit
```

### "DeepCopy methods are out of sync"

**Problem:** Build fails with missing DeepCopy methods

**Solution:**
```bash
make generate
git add api/**/zz_generated.deepcopy.go
git commit --amend --no-edit
```

### "Helm chart CRDs are out of sync"

**Problem:** `make verify-helm-crds` fails

**Solution:**
```bash
make sync-helm-crds
git add charts/virtrigaud/crds/
git commit --amend --no-edit
```

### Pre-commit Hook Not Running

**Problem:** Hook doesn't execute on commit

**Solution:**
```bash
# Reinstall hooks
make setup-git-hooks

# Verify hook is installed
ls -la .git/hooks/pre-commit

# Make hook executable
chmod +x .git/hooks/pre-commit
```

## Best Practices

1. **Always install git hooks** - Run `make setup-git-hooks` on first checkout
2. **Never edit generated files** - Only modify `*_types.go` files
3. **Update docs with CRDs** - Keep examples and documentation in sync
4. **Validate examples** - Run `kubectl apply --dry-run=client -f docs/examples/`
5. **Use kubebuilder markers** - Add validation, defaults, and documentation
6. **Test thoroughly** - Run `make test` after CRD changes
7. **Commit generated files** - Always include CRD YAMLs in commits

## File Structure

```
api/infra.virtrigaud.io/v1beta1/
├── virtualmachine_types.go        # Source of truth
├── provider_types.go               # Source of truth
├── vmclass_types.go                # Source of truth
└── zz_generated.deepcopy.go        # Auto-generated (DO NOT EDIT)

config/crd/bases/
├── infra.virtrigaud.io_virtualmachines.yaml  # Auto-generated (DO NOT EDIT)
├── infra.virtrigaud.io_providers.yaml        # Auto-generated (DO NOT EDIT)
└── infra.virtrigaud.io_vmclasses.yaml        # Auto-generated (DO NOT EDIT)

charts/virtrigaud/crds/
├── infra.virtrigaud.io_virtualmachines.yaml  # Auto-synced (DO NOT EDIT)
├── infra.virtrigaud.io_providers.yaml        # Auto-synced (DO NOT EDIT)
└── infra.virtrigaud.io_vmclasses.yaml        # Auto-synced (DO NOT EDIT)
```

## Further Reading

- [Kubebuilder Book](https://book.kubebuilder.io/)
- [Controller-Gen Documentation](https://book.kubebuilder.io/reference/controller-gen.html)
- [Kubebuilder Markers](https://book.kubebuilder.io/reference/markers.html)
- [CRD Versioning](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/)
