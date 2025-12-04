# Versioning & Breaking Changes

This document outlines VirtRigaud's approach to versioning, compatibility, and managing breaking changes across the provider ecosystem.

## Overview

VirtRigaud follows semantic versioning (SemVer) principles and maintains backward compatibility through careful API design and migration strategies. The system has multiple versioning dimensions:

- **VirtRigaud Core** - The main platform (API server, manager, CRDs)
- **Provider SDK** - Go SDK for building providers  
- **Proto Contracts** - gRPC/protobuf API definitions
- **Individual Providers** - Each provider has independent versioning

## Semantic Versioning

All VirtRigaud components follow [Semantic Versioning 2.0.0](https://semver.org/):

### Version Format: MAJOR.MINOR.PATCH

- **MAJOR** (X.0.0): Breaking changes that require user action
- **MINOR** (0.X.0): New features that are backward compatible  
- **PATCH** (0.0.X): Bug fixes and security updates

### Examples

```
1.0.0 → 1.0.1  # Patch: Bug fixes only
1.0.1 → 1.1.0  # Minor: New features, backward compatible
1.1.0 → 2.0.0  # Major: Breaking changes
```

## Component Versioning Strategy

### VirtRigaud Core APIs

Kubernetes-style API versioning with multiple supported versions:

```yaml
# Supported API versions
apiVersion: infra.virtrigaud.io/v1beta1  # Development/preview
apiVersion: infra.virtrigaud.io/v1beta1   # Pre-release/testing
apiVersion: infra.virtrigaud.io/v1        # Stable/production
```

**Stability Levels:**
- **Alpha (v1beta1)**: Experimental, may change or be removed
- **Beta (v1beta1)**: Well-tested, minimal changes expected
- **Stable (v1)**: Production-ready, strong backward compatibility

**Support Windows:**
- Alpha: Best effort, no guarantees
- Beta: Supported for 2 minor releases after stable equivalent
- Stable: Supported for 12 months after deprecation

### Provider SDK Versioning

SDK versions are independent of core VirtRigaud versions:

```go
// Go module versioning
module github.com/projectbeskar/virtrigaud/sdk

// Version tags
sdk/v0.1.0    # Initial release
sdk/v0.2.0    # New features
sdk/v1.0.0    # First stable release
sdk/v2.0.0    # Breaking changes (new module path: sdk/v2)
```

**SDK Compatibility Matrix:**

| SDK Version | VirtRigaud Core | Go Version | Status |
|-------------|-----------------|------------|---------|
| v0.1.x | 0.1.0 - 0.2.x | 1.23+ | Beta |
| v1.0.x | 0.2.0 - 1.0.x | 1.23+ | Stable |
| v1.1.x | 0.3.0 - 1.1.x | 1.23+ | Stable |
| v2.0.x | 1.0.0+ | 1.24+ | Future |

### Proto Contract Versioning

Protobuf APIs use both module versions and service versions:

```protobuf
// Service versioning in proto files
package provider.v1;
service ProviderService {
  // API methods
}

// Module versioning
module github.com/projectbeskar/virtrigaud/proto
```

**Proto Evolution Rules:**
- ✅ Add new fields (with proper defaults)
- ✅ Add new RPC methods
- ✅ Add new enum values
- ❌ Remove fields or methods
- ❌ Change field types or semantics
- ❌ Remove enum values

### Provider Versioning

Each provider maintains independent versioning:

```yaml
# Provider catalog entry
name: vsphere
tag: "1.2.3"      # Provider version
sdk_version: "v1.0.0"  # SDK dependency
proto_version: "v0.1.0"  # Proto dependency
```

## Breaking Change Policy

### What Constitutes a Breaking Change

**API Breaking Changes:**
- Removing or renaming API fields
- Changing field types or semantics
- Removing API endpoints or methods
- Changing required vs optional fields
- Modifying default behaviors
- Changing error codes or messages that clients depend on

**SDK Breaking Changes:**
- Removing public functions, types, or methods
- Changing function signatures
- Modifying struct fields (without proper backward compatibility)
- Changing package import paths
- Removing or renaming configuration options

**Proto Breaking Changes:**
- Removing fields or RPC methods
- Changing field numbers or types
- Removing enum values
- Modifying service or method names

### Breaking Change Process

#### 1. Proposal Phase
```markdown
# Breaking Change Proposal: [Title]

## Summary
Brief description of the change and motivation.

## Motivation  
Why is this change necessary? What problems does it solve?

## Proposed Changes
Detailed description of the changes.

## Migration Path
How will users migrate from old to new behavior?

## Timeline
- Deprecation announcement: v1.1.0
- Breaking change implementation: v2.0.0
- Legacy support removal: v3.0.0

## Alternatives Considered
What other approaches were considered?
```

#### 2. Deprecation Phase
```go
// Deprecated functions include clear migration guidance
// Deprecated: Use NewCreateVMRequest instead. Will be removed in v2.0.0.
func CreateVM(name string) *VMRequest {
    return &VMRequest{Name: name}
}

// New recommended approach
func NewCreateVMRequest(spec *VMSpec) *CreateVMRequest {
    return &CreateVMRequest{Spec: spec}
}
```

#### 3. Migration Tools
```bash
# Migration command examples
vrtg-provider migrate --from v1 --to v2
vrtg-provider check-compatibility --target-version v2.0.0
```

#### 4. Communication
- Release notes with migration guide
- Blog posts for major changes
- Community discussions and Q&A
- Updated documentation

## Compatibility Testing

### Automated Compatibility Checks

```yaml
# .github/workflows/compatibility.yml
name: Compatibility Check

jobs:
  compatibility-matrix:
    strategy:
      matrix:
        sdk_version: [v1.0.0, v1.1.0, current]
        provider_version: [v1.0.0, v1.1.0, current]
    
    steps:
    - name: Test SDK ${{ matrix.sdk_version }} with Provider ${{ matrix.provider_version }}
      run: |
        # Build provider with specific SDK version
        # Run conformance tests
        # Report compatibility results
```

### Buf Proto Compatibility

```yaml
# proto/buf.yaml
version: v1
breaking:
  use:
    # Prevent breaking changes
    - FILE_NO_DELETE
    - FIELD_NO_DELETE
    - FIELD_SAME_TYPE
    - ENUM_VALUE_NO_DELETE
    - RPC_NO_DELETE
    - SERVICE_NO_DELETE
  ignore:
    # Allowed changes during alpha/beta
    - "provider/v1beta1"
```

```bash
# Check for breaking changes
buf breaking --against 'https://github.com/projectbeskar/virtrigaud.git#branch=main'
```

### Provider Compatibility Testing

```bash
# Test provider against multiple VirtRigaud versions
vcts run --provider ./provider --virtrigaud-version 0.1.0
vcts run --provider ./provider --virtrigaud-version 0.2.0
vcts run --provider ./provider --virtrigaud-version 1.0.0
```

## Migration Strategies

### API Version Migration

#### Example: VirtualMachine v1beta1 → v1beta1

```go
// Conversion webhook approach
func (src *v1beta1.VirtualMachine) ConvertTo(dst *v1beta1.VirtualMachine) error {
    // Convert common fields
    dst.ObjectMeta = src.ObjectMeta
    
    // Handle field migrations
    if src.Spec.PowerState == "On" {
        dst.Spec.PowerState = v1beta1.PowerStateOn
    }
    
    // Set new fields with appropriate defaults
    if dst.Spec.Phase == "" {
        dst.Spec.Phase = v1beta1.PhaseUnknown
    }
    
    return nil
}
```

#### Gradual Migration Process

```bash
# Phase 1: Dual support (both versions work)
kubectl apply -f vm-v1beta1.yaml  # Still works
kubectl apply -f vm-v1beta1.yaml   # Also works

# Phase 2: Deprecation warning
kubectl apply -f vm-v1beta1.yaml
# Warning: v1beta1 is deprecated, use v1beta1

# Phase 3: Conversion only (internal storage uses v1beta1)
kubectl apply -f vm-v1beta1.yaml  # Automatically converted

# Phase 4: Removal (after support window)
kubectl apply -f vm-v1beta1.yaml  # Error: version not supported
```

### Provider SDK Migration

#### Example: SDK v1 → v2

**SDK v1 (deprecated):**
```go
// Old SDK pattern
func NewProvider(config Config) *Provider {
    return &Provider{config: config}
}

func (p *Provider) CreateVM(name string, cpu int, memory int) error {
    // Implementation
}
```

**SDK v2 (new):**
```go
// New SDK pattern with better types
func NewProvider(config *Config) (*Provider, error) {
    if err := config.Validate(); err != nil {
        return nil, err
    }
    return &Provider{config: config}, nil
}

func (p *Provider) CreateVM(ctx context.Context, req *CreateVMRequest) (*CreateVMResponse, error) {
    // Implementation with proper context and structured types
}
```

**Migration Bridge:**
```go
// sdk/v2/compat/v1.go - Compatibility layer
package compat

import (
    v1 "github.com/projectbeskar/virtrigaud/sdk/provider"
    v2 "github.com/projectbeskar/virtrigaud/sdk/v2/provider"
)

// Bridge for gradual migration
func AdaptV1Provider(v1Provider v1.Provider) v2.Provider {
    return &v1ProviderAdapter{old: v1Provider}
}

type v1ProviderAdapter struct {
    old v1.Provider
}

func (a *v1ProviderAdapter) CreateVM(ctx context.Context, req *v2.CreateVMRequest) (*v2.CreateVMResponse, error) {
    // Convert v2 request to v1 format
    err := a.old.CreateVM(req.Name, int(req.Spec.CPU), int(req.Spec.Memory))
    
    // Convert v1 response to v2 format
    if err != nil {
        return nil, err
    }
    
    return &v2.CreateVMResponse{
        Status: "Created",
    }, nil
}
```

### Configuration Migration

#### Example: Configuration Schema Changes

**v1 Configuration:**
```yaml
# provider-config-v1.yaml
provider:
  type: "vsphere"
  server: "vcenter.example.com"
  username: "admin"
  password: "secret"
```

**v2 Configuration:**
```yaml
# provider-config-v2.yaml
apiVersion: config.virtrigaud.io/v2
kind: ProviderConfig
metadata:
  name: vsphere-config
spec:
  type: "vsphere"
  connection:
    endpoint: "vcenter.example.com"
    authentication:
      method: "basic"
      secretRef:
        name: "vsphere-credentials"
  features:
    snapshots: true
    cloning: true
```

**Migration Command:**
```bash
# Automatic migration tool
vrtg-provider config migrate \
  --from provider-config-v1.yaml \
  --to provider-config-v2.yaml \
  --create-secret vsphere-credentials
```

## Release Planning

### Release Cadence

- **Patch releases**: As needed for critical bugs/security
- **Minor releases**: Every 2-3 months  
- **Major releases**: Every 12-18 months

### Feature Lifecycle

```
Experimental → Alpha → Beta → Stable → Deprecated → Removed
     |          |       |       |         |          |
     |          |       |       |         |          +-- After support window
     |          |       |       |         +-- 2 releases notice
     |          |       |       +-- Production ready
     |          |       +-- Pre-release testing
     |          +-- Public preview
     +-- Internal/development only
```

### Release Branch Strategy

```
main                    # Current development
├── release-0.1        # Patch releases for v0.1.x
├── release-0.2        # Patch releases for v0.2.x
└── release-1.0        # Patch releases for v1.0.x
```

### Support Matrix

| Version | Status | Support Level | End of Life |
|---------|--------|---------------|-------------|
| 1.0.x | Stable | Full support | 2026-01-01 |
| 0.2.x | Stable | Security only | 2025-06-01 |
| 0.1.x | Deprecated | None | 2025-01-01 |

## Best Practices

### For Provider Developers

1. **Version Dependencies Carefully**
   ```go
   // Use specific versions, not floating
   require github.com/projectbeskar/virtrigaud/sdk v1.2.3
   ```

2. **Test Compatibility Early**
   ```bash
   # Test against multiple SDK versions
   go mod edit -require=github.com/projectbeskar/virtrigaud/sdk@v1.1.0
   go test ./...
   go mod edit -require=github.com/projectbeskar/virtrigaud/sdk@v1.2.0
   go test ./...
   ```

3. **Handle Deprecations Gracefully**
   ```go
   // Check for deprecated features
   if provider.IsDeprecated("vm.legacy-create") {
       log.Warn("Using deprecated API, migrate to vm.create")
   }
   ```

4. **Document Breaking Changes**
   ```markdown
   # CHANGELOG.md
   ## [2.0.0] - 2025-01-15
   ### BREAKING CHANGES
   - Removed deprecated `CreateVM` method, use `CreateVMRequest` instead
   - Changed configuration format, see migration guide
   
   ### Migration Guide
   Old: `provider.CreateVM("vm1", 2, 4096)`
   New: `provider.CreateVM(ctx, &CreateVMRequest{...})`
   ```

### For Users

1. **Pin Versions in Production**
   ```yaml
   # Helm values
   image:
     tag: "1.2.3"  # Not "latest"
   ```

2. **Test Upgrades in Staging**
   ```bash
   # Upgrade strategy
   helm upgrade provider-test virtrigaud/provider \
     --version 1.3.0 \
     --namespace staging
   ```

3. **Monitor Deprecation Warnings**
   ```bash
   # Check for deprecation warnings
   kubectl logs -l app=provider | grep -i deprecat
   ```

4. **Plan Migration Windows**
   ```yaml
   # Schedule upgrades during maintenance windows
   # Have rollback plans ready
   # Test compatibility thoroughly
   ```

## Future Considerations

### Long-term Compatibility

- **10-year Support Goal**: Core APIs should remain usable for 10 years
- **Gradual Evolution**: Prefer gradual evolution over revolutionary changes
- **Ecosystem Stability**: Consider impact on the entire provider ecosystem

### Emerging Standards

- **OCI Compliance**: Align with OCI runtime and image standards
- **CNCF Integration**: Follow CNCF project graduation requirements
- **Industry Standards**: Adopt relevant industry standards as they emerge

### Technology Evolution

- **Go Version Support**: Support 2-3 latest Go versions
- **Kubernetes Compatibility**: Support 3-4 latest Kubernetes versions
- **gRPC Evolution**: Adapt to gRPC and protobuf improvements

This versioning strategy ensures VirtRigaud can evolve while maintaining stability and compatibility for the provider ecosystem.

