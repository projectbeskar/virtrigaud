# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.2.0-rc.1] - 2025-09-03

### BREAKING CHANGES

**🏗️ Remote-Only Provider Architecture**

- **Removed InProcess provider mode** - All providers now run as independent pods
- **Provider CRD requires `runtime` field** - All Provider resources must include runtime configuration
- **Remote provider images** - Provider containers run as separate deployments with gRPC communication

**⚠️ v1alpha1 API Removed (from previous RC)**

- **Removed v1alpha1 CRDs and all conversion webhooks**
- v1beta1 is now the only served and storage version
- All conversion webhooks have been removed from the system

**🔒 Lint-Zero Enforcement (from previous RC)**

- **Strict lint-zero implementation** with CI gates that fail builds on any issues
- **Modern API compliance** with updated gRPC, OpenTelemetry, and string handling
- **Enhanced error handling** with comprehensive errcheck compliance

### Changed

- **Provider Architecture**: Unified Remote-only provider pattern for scalability and reliability
- **Provider CRD**: `spec.runtime` field is now required with `mode: Remote`, `image`, and `service` configuration
- **Provider Controller**: Simplified to only handle Remote provider deployments
- **Provider Resolver**: Streamlined to pure remote provider resolution
- **Documentation**: Completely updated to reflect Remote-only architecture
- **Examples**: All examples now include required `runtime` configuration
- **Test Cases**: Updated GitHub Actions and conformance tests for Remote providers

### Removed

- **InProcess provider mode** and all related code
- **Provider Registry** for InProcess provider registration
- **Provider Factories** for InProcess provider instantiation
- **Dual-mode complexity** from provider controller and resolver
- **Mixed deployment patterns** documentation

### Added

- **Comprehensive Remote Provider Documentation**: Detailed configuration flow from Provider CRD to pod arguments
- **Provider Configuration Mapping**: Clear documentation of how Provider specs become command-line args and env vars
- **Advanced Provider Examples**: High-availability and production-ready provider configurations
- **Updated GitHub Actions**: Fixed kubectl version issues and updated test cases for Remote providers

### Fixed

- **GitHub Actions CI**: Updated kubectl version from v1.31.2 to v1.31.0 (non-existent version fix)
- **CRD Installation Tests**: Added required `runtime` field to test Provider resources
- **Conformance Tests**: Updated test manifests to include Remote provider configuration

### Migration Guide

**Before upgrading to v0.2.0:**

1. **Backup all Provider resources** - The Provider CRD schema has breaking changes
2. **Note current provider configurations** - InProcess mode is no longer supported
3. **Ensure provider images are available** - All providers now require container images

**After upgrading:**

1. **Update all Provider resources** to include the required `runtime` field:
   ```yaml
   apiVersion: infra.virtrigaud.io/v1beta1
   kind: Provider
   spec:
     type: vsphere  # or libvirt, proxmox
     endpoint: "your-endpoint"
     credentialSecretRef:
       name: your-credentials
     runtime:
       mode: Remote
       image: "virtrigaud/provider-vsphere:latest"
       service:
         port: 9090
   ```

2. **Provider images will be automatically deployed** as separate pods
3. **All providers now run as independent services** with gRPC communication
4. **Improved scalability and reliability** with isolated provider pods

## [0.1.0] - 2025-01-XX

### Added

- Initial release with v1alpha1 and v1beta1 API support
- Multi-hypervisor VM management
- Provider-based architecture
- Conversion webhooks between API versions
