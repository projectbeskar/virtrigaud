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

# Generate code and sync CRDs
make generate
make sync-helm-crds

# Verify everything is in sync
make verify-helm-crds
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

**Important**: Always keep CRDs synchronized between `config/crd/bases/` and `charts/virtrigaud/crds/`.

```bash
# After API changes, sync CRDs to Helm chart
make sync-helm-crds

# Verify sync before committing
make verify-helm-crds
```

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
   # Ensure CRDs are synced
   make sync-helm-crds
   
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

### CRD Sync Issues

If you see "Helm chart CRDs are out of sync":

```bash
# Fix with
make sync-helm-crds

# Verify
make verify-helm-crds
```

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
