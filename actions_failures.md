# GitHub Actions Failures Analysis

Based on examination of workflow files in `.github/workflows/`, here are the identified issues and root causes:

## Common Issues Across All Workflows

### 1. **Outdated Action Versions**
- Using `actions/setup-go@v4` instead of `@v5`
- Using `actions/cache@v3` instead of `@v4` 
- Using `docker/build-push-action@v5` instead of `@v6`
- Using older versions of various actions

### 2. **Missing Security Hardening**
- No `timeout-minutes` set on jobs (can hang indefinitely)
- Missing concurrency groups (multiple runs can interfere)
- Some workflows lack proper permissions restrictions
- No `shell: bash -euxo pipefail` for safety

### 3. **Runner Version Issues**
- Using generic `ubuntu-latest` instead of pinned versions
- Should use `ubuntu-24.04` or `ubuntu-22.04` for consistency

## Workflow-Specific Issues

### CI Workflow (`ci.yml`)
**Root Causes:**
- **Missing provider-proxmox**: Build matrix excludes the new Proxmox provider
- **Libvirt dependencies**: CGO builds may fail without proper libvirt setup
- **Multi-arch build missing**: Only building `linux/amd64` in development
- **Action version mismatch**: Using older setup-go and cache actions

**Likely Failures:**
- Build jobs don't include `provider-proxmox` in matrix
- Multi-arch builds fail on ARM64
- Cache misses due to outdated cache action

### Release Workflow (`release.yml`)
**Root Causes:**
- **Missing provider-proxmox**: Build matrix missing the new provider
- **Multi-arch platform string**: Format might be incorrect
- **SBOM/signing timing**: Race conditions with image availability
- **Helm repo update**: May fail if gh-pages branch doesn't exist

**Likely Failures:**
- `provider-proxmox` not built or pushed
- ARM64 builds fail due to missing QEMU setup
- SBOM generation fails if images not yet propagated

### Provider SDK Workflow (`provider-sdk.yml`)
**Root Causes:**
- **Buf tool version**: May be outdated or incompatible
- **Proto generation**: Missing newer provider types in scaffolding
- **Integration test gaps**: Mock provider tests incomplete

**Likely Failures:**
- Proto breaking change detection false positives
- Scaffolding tests missing `proxmox` provider type
- Integration tests not actually validating gRPC

### Helm CRD Installation (`helm-crds.yml`)
**Root Causes:**
- **CRD path mismatches**: References to non-existent CRDs
- **Chart values outdated**: Test values may reference old image names
- **Conversion webhook config**: May not match actual webhook service names

**Likely Failures:**
- Chart installation fails due to missing CRDs
- Webhook verification fails if service names changed
- Server-side dry-run fails on invalid manifests

### Runtime Chart (`runtime-chart.yml`)
**Root Causes:**
- **Missing example values**: No Proxmox example values file
- **Kind version outdated**: Using older kind-action version
- **Health check assumptions**: Assuming HTTP health endpoints exist

**Likely Failures:**
- Template tests fail for missing `values-proxmox.yaml`
- gRPC health checks fail without proper grpcurl setup
- Port-forward commands may hang

### Conversion Tests (`conversion.yml`)
**Root Causes:**
- **Script path dependencies**: References to hack scripts that may not exist
- **Test path assumptions**: Conversion E2E test paths may be incorrect
- **Missing test files**: Referenced test files may not exist

**Likely Failures:**
- `./hack/test-conversion-no-skips.sh` not found
- `./test/conversione2e` path incorrect
- Conversion webhook tests fail on missing CRDs

## Critical Missing Elements

1. **Proxmox Provider Integration**: New provider not included in build matrices
2. **Multi-arch Support**: Missing QEMU setup for ARM64 builds
3. **Proper Error Handling**: No `continue-on-error` for optional jobs
4. **Integration Test Gating**: Tests don't check for required secrets
5. **Chart Testing**: Missing chart testing (ct) validation
6. **Status Badges**: No CI status badges in README

## Recommended Fixes Priority

### High Priority (Blocking CI)
1. Add `provider-proxmox` to all build matrices
2. Fix action versions and add proper permissions
3. Add missing hack scripts or update paths
4. Fix chart template references

### Medium Priority (Performance/Security)
1. Add multi-arch build support with QEMU
2. Add timeout and concurrency controls
3. Gate integration tests on secret availability
4. Add proper error handling

### Low Priority (Nice-to-have)
1. Add chart testing with ct
2. Add status badges
3. Improve documentation generation
4. Add performance testing integration

## Links to Investigate
- Check if `hack/test-conversion-no-skips.sh` exists
- Verify `test/conversione2e` directory structure
- Confirm chart values files in `charts/virtrigaud-provider-runtime/examples/`
- Review actual CRD names in `config/crd/`
