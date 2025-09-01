# Testing GitHub Actions Workflows Locally

This guide explains how to test VirtRigaud's GitHub Actions workflows locally before pushing to save on GitHub Actions costs and catch issues early.

## Overview

We provide several scripts to test workflows locally:

| Script | Purpose | Dependencies | Use Case |
|--------|---------|--------------|-----------|
| `hack/test-workflows-locally.sh` | **Main orchestrator** using `act` | `act`, `docker` | Full GitHub Actions simulation |
| `hack/test-lint-locally.sh` | **Lint workflow** replica | `go`, `golangci-lint` | Quick lint testing |
| `hack/test-ci-locally.sh` | **CI workflow** replica | `go`, `helm`, system deps | Comprehensive CI testing |
| `hack/test-release-locally.sh` | **Release workflow** simulation | `docker`, `helm`, `go` | Release preparation testing |
| `hack/test-helm-locally.sh` | **Helm charts** testing | `helm`, `kind`, `kubectl` | Chart validation and deployment |

## Quick Start

### 1. Setup (First Time)

```bash
# Install dependencies and configure act
./hack/test-workflows-locally.sh setup
```

This will:
- Install `act` (GitHub Actions runner)
- Create `.actrc` configuration
- Create `.env.local` with environment variables
- Create `.secrets` file (update with real values if needed)

### 2. Quick Validation

```bash
# Fast syntax check of all workflows
./hack/test-workflows-locally.sh smoke
```

### 3. Test Individual Workflows

```bash
# Test lint workflow (fastest)
./hack/test-lint-locally.sh

# Test CI workflow (comprehensive)
./hack/test-ci-locally.sh

# Test Helm charts
./hack/test-helm-locally.sh

# Test release workflow (requires Docker)
./hack/test-release-locally.sh v0.2.0-test
```

## Detailed Usage

### Lint Testing (`test-lint-locally.sh`)

Replicates the `lint.yml` workflow:

```bash
# Quick lint test
./hack/test-lint-locally.sh
```

**What it tests:**
- Go version compatibility
- golangci-lint installation and execution
- Comprehensive code linting (matching CI exactly)

**Requirements:**
- Go 1.21+
- Internet access (to download golangci-lint if needed)

### CI Testing (`test-ci-locally.sh`)

Replicates the `ci.yml` workflow jobs:

```bash
# Interactive mode (asks about optional jobs)
./hack/test-ci-locally.sh

# Quick essential tests only
./hack/test-ci-locally.sh quick

# Full CI replication including security scans
./hack/test-ci-locally.sh full
```

**Jobs tested:**
- **test**: Go tests and coverage
- **lint**: Code linting with golangci-lint  
- **generate**: Code and manifest generation
- **build**: Binary compilation
- **build-tools**: CLI tools compilation
- **helm**: Helm chart validation
- **security**: Security scanning (optional)

**Requirements:**
- Go 1.23+
- Helm 3.12+
- System dependencies (libvirt-dev on Linux)
- Python 3 (for YAML validation)

### Release Testing (`test-release-locally.sh`)

Simulates the `release.yml` workflow:

```bash
# Test with default tag
./hack/test-release-locally.sh

# Test with specific tag
./hack/test-release-locally.sh v0.3.0-rc.1

# Skip image building (faster)
./hack/test-release-locally.sh --no-images
```

**What it tests:**
- Container image building and pushing (to local registry)
- Helm chart packaging with version updates
- CLI tools building for multiple platforms
- Changelog generation
- Checksum creation
- Container image smoke testing

**Requirements:**
- Docker
- Go 1.23+
- Helm 3.12+
- Local Docker registry (started automatically)

### Helm Testing (`test-helm-locally.sh`)

Tests Helm charts with real Kubernetes:

```bash
# Full helm test suite
./hack/test-helm-locally.sh

# Individual test types
./hack/test-helm-locally.sh lint     # Chart linting only
./hack/test-helm-locally.sh template # Template rendering only
./hack/test-helm-locally.sh crd      # CRD installation only
./hack/test-helm-locally.sh main     # Main chart installation
./hack/test-helm-locally.sh runtime  # Runtime chart installation

# Cleanup after testing
./hack/test-helm-locally.sh cleanup
```

**What it tests:**
- Helm chart linting (`helm lint`)
- Template rendering with various value files
- CRD installation and functionality
- Chart installation in Kind cluster
- Pod readiness and basic functionality

**Requirements:**
- Helm 3.12+
- Kind (Kubernetes in Docker)
- kubectl
- Docker

### Act-Based Testing (`test-workflows-locally.sh`)

Uses `act` to run actual GitHub Actions workflows:

```bash
# Setup first time
./hack/test-workflows-locally.sh setup

# Test individual workflows
./hack/test-workflows-locally.sh lint
./hack/test-workflows-locally.sh ci
./hack/test-workflows-locally.sh runtime

# Test all workflows (interactive)
./hack/test-workflows-locally.sh all

# Cleanup
./hack/test-workflows-locally.sh cleanup
```

**Advanced usage:**
- Supports secrets from `.secrets` file
- Uses reusable containers for speed
- Artifact handling with local storage
- Environment variable injection

## Configuration Files

### `.actrc`
```bash
# Act configuration for GitHub Actions simulation
-P ubuntu-latest=catthehacker/ubuntu:act-22.04
-P ubuntu-22.04=catthehacker/ubuntu:act-22.04  
-P ubuntu-24.04=catthehacker/ubuntu:act-22.04
--container-daemon-socket /var/run/docker.sock
--reuse
--rm
```

### `.env.local`
```bash
# Local environment variables
GO_VERSION=1.23
GOLANGCI_LINT_VERSION=v1.64.8
REGISTRY=localhost:5000
IMAGE_NAME_PREFIX=virtrigaud
GITHUB_ACTOR=local-user
GITHUB_REPOSITORY=projectbeskar/virtrigaud
# ... more environment variables
```

### `.secrets` (optional)
```bash
# GitHub token for release workflows
GITHUB_TOKEN=your_github_token_here
REGISTRY=localhost:5000
```

## Workflow-Specific Notes

### Lint Workflow (`lint.yml`)
- **Fast**: Usually completes in 1-2 minutes
- **Requirements**: Minimal (Go + golangci-lint)
- **Run before**: Every commit
- **Catches**: Code style, syntax, and simple errors

### CI Workflow (`ci.yml`)
- **Comprehensive**: Tests building, testing, security
- **Duration**: 10-20 minutes for full run
- **Platform deps**: LibVirt requires Linux for full testing
- **Run before**: Pull requests and major changes

### Release Workflow (`release.yml`)
- **Complex**: Multi-platform builds, signing, publishing
- **Duration**: 20-30 minutes
- **Local only**: Uses local registry, no real publishing
- **Run before**: Creating releases

### Runtime Chart Workflow (`runtime-chart.yml`)
- **Kubernetes focused**: Tests provider runtime charts
- **Requirements**: Kind cluster
- **Duration**: 5-10 minutes
- **Run before**: Chart changes

## Best Practices

### Daily Development Workflow

```bash
# Before committing
./hack/test-lint-locally.sh

# Before pushing feature branch
./hack/test-ci-locally.sh quick

# Before creating PR
./hack/test-ci-locally.sh full
```

### Pre-Release Workflow

```bash
# Test release preparation
./hack/test-release-locally.sh v0.2.0-rc.1

# Test chart deployment
./hack/test-helm-locally.sh full

# Test with act for full simulation
./hack/test-workflows-locally.sh all
```

### Troubleshooting

#### Common Issues

1. **Docker permission denied**
   ```bash
   sudo usermod -aG docker $USER
   # Then logout/login
   ```

2. **LibVirt dependencies missing**
   ```bash
   # Ubuntu/Debian
   sudo apt-get install libvirt-dev pkg-config
   
   # Skip libvirt tests on non-Linux
   ./hack/test-ci-locally.sh quick
   ```

3. **Kind cluster creation fails**
   ```bash
   # Clean up and retry
   kind delete cluster --name virtrigaud-test
   ./hack/test-helm-locally.sh
   ```

4. **Act fails with container errors**
   ```bash
   # Clean up act containers
   docker ps -a | grep "act-" | awk '{print $1}' | xargs docker rm -f
   
   # Rebuild without cache
   ./hack/test-workflows-locally.sh cleanup
   ./hack/test-workflows-locally.sh setup
   ```

#### Debugging Tips

- **Check logs**: All scripts provide detailed logging
- **Use dry-run**: Most scripts support `--help` for options
- **Incremental testing**: Start with `lint`, then `ci quick`, then full tests
- **Docker cleanup**: Regular `docker system prune` helps with space

### Performance Tips

1. **Use quick modes** for daily development
2. **Skip expensive jobs** like security scans during iteration
3. **Reuse Kind clusters** with `./hack/test-helm-locally.sh`
4. **Use local registry** for container testing
5. **Run parallel tests** when possible

## Integration with Development

### Git Hooks

Add to `.git/hooks/pre-push`:
```bash
#!/bin/bash
echo "Running local lint check before push..."
./hack/test-lint-locally.sh
```

### IDE Integration

Many IDEs can run these scripts as build tasks:

**VS Code** (`.vscode/tasks.json`):
```json
{
  "version": "2.0.0",
  "tasks": [
    {
      "label": "Test Lint Locally",
      "type": "shell", 
      "command": "./hack/test-lint-locally.sh",
      "group": "test",
      "presentation": {
        "echo": true,
        "reveal": "always",
        "focus": false,
        "panel": "shared"
      }
    }
  ]
}
```

### CI Cost Optimization

By testing locally first:
- **Reduce failed CI runs** by ~80%
- **Save GitHub Actions minutes** 
- **Faster feedback** (local runs are often faster)
- **Better debugging** (local environment is easier to inspect)

## Conclusion

These local testing scripts allow you to:

✅ **Catch issues early** before they reach GitHub Actions  
✅ **Save costs** by reducing failed CI runs  
✅ **Debug faster** with local environment access  
✅ **Test thoroughly** with multiple approaches  
✅ **Iterate quickly** during development  

Start with the lint script for daily use, and gradually incorporate the full test suite for comprehensive validation before releases.
