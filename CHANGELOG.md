# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v0.2.0-rc.1] - 2025-01-11

### BREAKING CHANGES

**⚠️ v1alpha1 API Removed**

- **Removed v1alpha1 CRDs and all conversion webhooks**
- v1beta1 is now the only served and storage version
- All conversion webhooks have been removed from the system
- If you somehow have v1alpha1 objects, they must be migrated before upgrading

**🔒 Lint-Zero Enforcement**

- **Strict lint-zero implementation** with CI gates that fail builds on any issues
- **Modern API compliance** with updated gRPC, OpenTelemetry, and string handling
- **Enhanced error handling** with comprehensive errcheck compliance

### Changed

- **API Consolidation**: Simplified to single v1beta1 API version only
- **CRDs**: All CustomResourceDefinitions now serve only v1beta1 with `served: true` and `storage: true`
- **Documentation**: Updated all documentation to reflect v1beta1-only usage
- **Examples**: All examples now use v1beta1 API version
- **Charts**: Removed conversion webhook configurations from Helm charts

### Removed

- v1alpha1 API types and all related code
- ConvertTo/ConvertFrom implementations
- Conversion webhook patches and configurations
- API conversion documentation and upgrade guides
- v1alpha1 sample manifests

### Added

- `hack/check-alpha-crs.sh`: Preflight script to detect any remaining v1alpha1 custom resources
- `hack/verify-single-version.sh`: Script to verify CRDs have only v1beta1 version

### Migration Guide

**Before upgrading to v0.2.0:**

1. Run the preflight check: `./hack/check-alpha-crs.sh`
2. If any v1alpha1 resources exist, they were from a very early development version
3. Back up your resources and recreate them using v1beta1 API

**After upgrading:**

- All new resources must use `apiVersion: infra.virtrigaud.io/v1beta1`
- No conversion webhooks are needed or available
- The system is simplified with a single API version

## [0.1.0] - 2025-01-XX

### Added

- Initial release with v1alpha1 and v1beta1 API support
- Multi-hypervisor VM management
- Provider-based architecture
- Conversion webhooks between API versions
