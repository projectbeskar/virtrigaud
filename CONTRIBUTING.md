# Contributing to VirtRigaud

Thank you for your interest in contributing to VirtRigaud! This document provides guidelines and information for contributors.

## Development Setup

### Prerequisites

- Go 1.23+
- Docker
- Kubernetes cluster (kind, k3s, or remote)
- kubectl
- Helm 3.x
- make

### Clone and Setup

```bash
git clone https://github.com/projectbeskar/virtrigaud.git
cd virtrigaud

# Install development dependencies
make dev-setup

# Install pre-commit hooks (optional but recommended)
pip install pre-commit
pre-commit install
```

## Development Workflow

### 1. Making Changes

#### API Changes
When modifying API types:

```bash
# Edit API types
vim api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go

# Generate code and CRDs
make generate
make gen-crds
```

#### Code Changes
For other code changes:

```bash
# Run tests
make test

# Lint code
make lint

# Format code
make fmt
```

### 2. CRD Management

**Important**: CRDs are generated from the Go types in `api/infra.virtrigaud.io/v1beta1/*_types.go` (the single source of truth) and are not duplicated in git.

- `config/crd/bases/` - CRDs for local development and releases (**checked into git**)
- `charts/virtrigaud/crds/` - CRDs for the Helm chart (**gitignored**, generated at package time)

```bash
# After ANY change to api/infra.virtrigaud.io/v1beta1/*_types.go:
make gen-crds        # regenerates config/crd/bases/ (committed)
make gen-helm-crds   # regenerates charts/virtrigaud/crds/ (gitignored)
```

**Why the chart CRDs are gitignored (and what that means for you):** storing a second
copy of every CRD in the chart caused constant drift, so they are generated on demand
instead. The official chart is always correct because `release.yml` runs
`make gen-helm-crds` immediately before `helm package`.

> **Footgun — installing the chart from a checkout.** A fresh checkout has an *empty*
> `charts/virtrigaud/crds/` (only `.gitkeep`). If you `helm install`/`helm upgrade`
> straight from `charts/virtrigaud` without regenerating first, the chart ships **no**
> (or stale) CRDs. Worse, on upgrade the `crd-upgrade-job` hook does
> `kubectl apply --server-side --force-conflicts` of whatever CRDs were baked into the
> package, so a **stale** set silently **prunes** fields from the live CRDs (this is how
> `spec.topology` disappeared and Host/HostPool went missing on the lab). Always run
> `make gen-helm-crds` first, or use `make helm-template` / `make helm-package` /
> `make helm-lint`, which regenerate for you.

**CI guard:** the *Verify Generated Files* job runs `make verify-helm-crds`, which
regenerates both trees from the Go types and asserts `charts/virtrigaud/crds/` is
byte-identical to `config/crd/bases/` (and covers the full set). A PR that adds/changes
a CRD but breaks `gen-helm-crds` — or a type the chart generator no longer emits — fails
CI. **Upgrading CRDs on an existing release** requires a fresh chart release, or
`kubectl apply -f config/crd/bases` (the chart's native `crds/` dir is install-only).

### 3. Testing

```bash
# Unit tests
make test

# Integration tests (requires cluster)
make test-integration

# End-to-end tests
make test-e2e

# Test specific provider
make test-provider-vsphere
```

### 4. Local Development

```bash
# Deploy to local cluster
make dev-deploy

# Watch for changes and auto-reload
make dev-watch

# Clean up
make dev-clean
```

## Contribution Guidelines

### Pull Request Process

1. **Fork and branch**: Create a feature branch from `main`
2. **Make changes**: Follow the development workflow above
3. **Test thoroughly**: Run all relevant tests
4. **Update docs**: Update documentation if needed
5. **CRD sync**: Ensure CRDs are synchronized (CI will verify)
6. **Submit PR**: Create a pull request with clear description

### PR Requirements

- [ ] All tests pass
- [ ] CRDs are in sync (verified by CI)
- [ ] Code is formatted (`make fmt`)
- [ ] Code is linted (`make lint`)
- [ ] Documentation updated if needed
- [ ] Changelog entry added (for user-facing changes)

### Commit Message Format

Use conventional commit format:

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

Types:
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `style`: Code style changes
- `refactor`: Code refactoring
- `test`: Test changes
- `chore`: Maintenance tasks

Examples:
```
feat(vsphere): add graceful shutdown support
fix(crd): resolve powerState validation conflict
docs(upgrade): add CRD synchronization guide
```

## Code Style

### Go Code

- Follow standard Go conventions
- Use `gofmt` and `golangci-lint`
- Add meaningful comments for exported functions
- Include unit tests for new functionality

### YAML/Kubernetes

- Use 2-space indentation
- Follow Kubernetes API conventions
- Add descriptions to CRD fields
- Include examples in documentation

### Documentation

- Use clear, concise language
- Include code examples
- Update both API docs and user guides
- Test documentation examples

## Testing

### Unit Tests

```bash
# Run all unit tests
make test

# Run tests for specific package
go test ./internal/controller/...

# Run with coverage
make test-coverage
```

### Integration Tests

```bash
# Requires running Kubernetes cluster
export KUBECONFIG=~/.kube/config
make test-integration
```

### Provider Tests

```bash
# Test specific provider (requires infrastructure)
make test-provider-vsphere
make test-provider-libvirt
make test-provider-proxmox
```

## Release Process

### For Maintainers

1. **Prepare release**:
   ```bash
   # Generate CRDs for config directory (will be in release artifacts)
   make gen-crds

   # Update version in charts
   vim charts/virtrigaud/Chart.yaml

   # Update changelog
   vim CHANGELOG.md
   ```

2. **Create release**:
   ```bash
   git tag v0.2.1
   git push origin v0.2.1
   ```

3. **CI handles**:
   - Building and pushing images
   - Creating GitHub release
   - Publishing Helm charts
   - Generating CLI binaries

## Common Issues

### CRD Generation Issues

If you need to regenerate CRDs:

```bash
# For local development and config directory
make gen-crds

# For Helm chart packaging
make gen-helm-crds
```

Note: CRDs in `charts/virtrigaud/crds/` are generated during packaging and should not be committed to git.

### Test Failures

```bash
# Clean and retry
make clean
make test

# For libvirt-related failures
export SKIP_LIBVIRT_TESTS=true
make test
```

### Development Environment

```bash
# Reset development environment
make dev-clean
make dev-deploy

# Check logs
kubectl logs -n virtrigaud-system deployment/virtrigaud-manager
```

## Getting Help

- **GitHub Issues**: Bug reports and feature requests
- **GitHub Discussions**: Questions and community support
- **Documentation**: Check docs/ directory
- **Code Review**: Maintainers will provide feedback on PRs

## Recognition

Contributors are recognized in:
- CHANGELOG.md for significant contributions
- README.md contributors section
- GitHub contributor graphs

Thank you for contributing to VirtRigaud! 🚀
