# CLI Tools Reference

VirtRigaud provides a comprehensive set of command-line tools for managing virtual machines, developing providers, running conformance tests, and performing load testing. This guide covers all available CLI tools and their usage.

## Overview

| Tool | Purpose | Target Users |
|------|---------|--------------|
| [`vrtg`](#vrtg) | Main CLI for VM management and operations | End users, DevOps teams, System administrators |
| [`vcts`](#vcts) | Conformance testing suite | Provider developers, QA teams, CI/CD pipelines |
| [`vrtg-provider`](#vrtg-provider) | Provider development toolkit | Provider developers, Contributors |
| [`virtrigaud-loadgen`](#virtrigaud-loadgen) | Load testing and benchmarking | Performance engineers, SREs |

## Installation

### From GitHub Releases

```bash
# Download the latest release
export VIRTRIGAUD_VERSION="v0.2.1"
export PLATFORM="linux-amd64"  # or darwin-amd64, windows-amd64

# Install main CLI tool
curl -L "https://github.com/projectbeskar/virtrigaud/releases/download/${VIRTRIGAUD_VERSION}/vrtg-${PLATFORM}" -o vrtg
chmod +x vrtg
sudo mv vrtg /usr/local/bin/

# Install all CLI tools
curl -L "https://github.com/projectbeskar/virtrigaud/releases/download/${VIRTRIGAUD_VERSION}/virtrigaud-cli-${PLATFORM}.tar.gz" | tar xz
sudo mv vrtg vcts vrtg-provider virtrigaud-loadgen /usr/local/bin/
```

### From Source

```bash
git clone https://github.com/projectbeskar/virtrigaud.git
cd virtrigaud

# Build all CLI tools
make build-cli

# Install to /usr/local/bin
sudo make install-cli

# Or install to custom location
make install-cli PREFIX=/usr/local
```

### Using Go

```bash
# Install specific version
go install github.com/projectbeskar/virtrigaud/cmd/vrtg@v0.2.1
go install github.com/projectbeskar/virtrigaud/cmd/vcts@v0.2.1
go install github.com/projectbeskar/virtrigaud/cmd/vrtg-provider@v0.2.1
go install github.com/projectbeskar/virtrigaud/cmd/virtrigaud-loadgen@v0.2.1

# Install latest
go install github.com/projectbeskar/virtrigaud/cmd/vrtg@latest
```

### Completion

Enable shell completion for enhanced productivity:

```bash
# Bash
vrtg completion bash > /etc/bash_completion.d/vrtg
source /etc/bash_completion.d/vrtg

# Zsh
vrtg completion zsh > "${fpath[1]}/_vrtg"

# Fish
vrtg completion fish > ~/.config/fish/completions/vrtg.fish

# PowerShell
vrtg completion powershell | Out-String | Invoke-Expression
```

---

## vrtg

The main CLI tool for managing VirtRigaud resources and virtual machines.

### Global Flags

```bash
--kubeconfig string   Path to kubeconfig file (default: $KUBECONFIG or ~/.kube/config)
--namespace string    Kubernetes namespace (default: "default")
--output string       Output format: table, json, yaml (default: "table")
--timeout duration    Operation timeout (default: 30s)
--verbose             Enable verbose output
-h, --help           Help for vrtg
```

### Commands

#### vm - Virtual Machine Management

Manage virtual machines with comprehensive lifecycle operations.

```bash
# List virtual machines
vrtg vm list [flags]

# Describe a virtual machine
vrtg vm describe <name> [flags]

# Show VM events
vrtg vm events <name> [flags]

# Get VM console URL
vrtg vm console-url <name> [flags]
```

**Flags:**
- `--all-namespaces`: List VMs across all namespaces
- `--label-selector`: Filter by labels (e.g., `app=web,env=prod`)
- `--field-selector`: Filter by fields (e.g., `spec.powerState=On`)
- `--sort-by`: Sort output by column (name, namespace, powerState, provider)
- `--watch`: Watch for changes

**Examples:**

```bash
# List all VMs in table format
vrtg vm list

# List VMs with custom output format
vrtg vm list --output json --namespace production

# List VMs across all namespaces
vrtg vm list --all-namespaces

# Filter VMs by labels
vrtg vm list --label-selector environment=production,tier=web

# Watch VM status changes
vrtg vm list --watch

# Get detailed VM information
vrtg vm describe my-vm --output yaml

# Get VM console URL
vrtg vm console-url my-vm

# Show recent VM events
vrtg vm events my-vm
```

#### provider - Provider Management

Manage provider configurations and monitor their health.

```bash
# List providers
vrtg provider list [flags]

# Show provider status
vrtg provider status <name> [flags]

# Show provider logs
vrtg provider logs <name> [flags]
```

**Flags:**
- `--follow`: Follow log output (for logs command)
- `--tail`: Number of lines to show from end of logs (default: 100)
- `--since`: Show logs since timestamp (e.g., 1h, 30m)

**Examples:**

```bash
# List all providers
vrtg provider list

# Check provider status
vrtg provider status vsphere-provider

# View provider logs
vrtg provider logs vsphere-provider --tail 50

# Follow provider logs in real-time
vrtg provider logs vsphere-provider --follow

# Show logs from last hour
vrtg provider logs vsphere-provider --since 1h
```

#### snapshot - Snapshot Management

Manage VM snapshots for backup and recovery.

```bash
# Create a VM snapshot
vrtg snapshot create <vm-name> <snapshot-name> [flags]

# List snapshots
vrtg snapshot list [vm-name] [flags]

# Revert VM to snapshot
vrtg snapshot revert <vm-name> <snapshot-name> [flags]
```

**Flags for create:**
- `--description`: Snapshot description
- `--include-memory`: Include memory state in snapshot

**Examples:**

```bash
# Create a simple snapshot
vrtg snapshot create my-vm pre-upgrade

# Create snapshot with description and memory
vrtg snapshot create my-vm pre-maintenance \
  --description "Before maintenance window" \
  --include-memory

# List all snapshots
vrtg snapshot list

# List snapshots for specific VM
vrtg snapshot list my-vm

# Revert to a snapshot
vrtg snapshot revert my-vm pre-upgrade
```

#### clone - VM Cloning

Clone virtual machines for rapid provisioning.

```bash
# Clone a virtual machine
vrtg clone run <source-vm> <target-vm> [flags]

# List clone operations
vrtg clone list [flags]
```

**Flags for run:**
- `--linked`: Create linked clone (faster, space-efficient)
- `--target-namespace`: Namespace for target VM
- `--customize`: Apply customization during clone

**Examples:**

```bash
# Simple VM clone
vrtg clone run template-vm new-vm

# Linked clone for development
vrtg clone run production-vm dev-vm --linked

# Clone to different namespace
vrtg clone run template-vm test-vm --target-namespace testing

# List clone operations
vrtg clone list
```

#### conformance - Provider Testing

Run conformance tests against providers.

```bash
# Run conformance tests
vrtg conformance run <provider> [flags]
```

**Flags:**
- `--output-dir`: Directory for test results
- `--skip-tests`: Comma-separated list of tests to skip
- `--timeout`: Test timeout (default: 30m)

**Examples:**

```bash
# Run conformance tests
vrtg conformance run vsphere-provider

# Run tests with custom timeout
vrtg conformance run vsphere-provider --timeout 1h

# Skip specific tests
vrtg conformance run vsphere-provider --skip-tests "test-large-vms,test-network"
```

#### diag - Diagnostics

Diagnostic tools for troubleshooting.

```bash
# Create diagnostic bundle
vrtg diag bundle [flags]
```

**Flags:**
- `--output`: Output file path (default: virtrigaud-diag-<timestamp>.tar.gz)
- `--include-logs`: Include provider logs in bundle
- `--since`: Collect logs since timestamp

**Examples:**

```bash
# Create diagnostic bundle
vrtg diag bundle

# Create bundle with logs from last 2 hours
vrtg diag bundle --include-logs --since 2h

# Custom output location
vrtg diag bundle --output /tmp/debug-bundle.tar.gz
```

#### init - Installation

Initialize VirtRigaud in a Kubernetes cluster.

```bash
# Initialize virtrigaud
vrtg init [flags]
```

**Flags:**
- `--chart-version`: Helm chart version to install
- `--namespace`: Installation namespace (default: virtrigaud-system)
- `--values`: Values file for Helm chart
- `--dry-run`: Show what would be installed

**Examples:**

```bash
# Basic installation
vrtg init

# Install specific version
vrtg init --chart-version v0.2.1

# Install with custom values
vrtg init --values custom-values.yaml

# Dry run to see what would be installed
vrtg init --dry-run
```

---

## vcts

VirtRigaud Conformance Test Suite - runs standardized tests against providers.

### Usage

```bash
vcts [command] [flags]
```

### Global Flags

```bash
--kubeconfig string   Path to kubeconfig file
--namespace string    Kubernetes namespace (default: "virtrigaud-system")
--provider string     Provider name to test
--output-dir string   Output directory for test results (default: "./conformance-results")
--skip-tests strings  Comma-separated list of tests to skip
--timeout duration    Test timeout (default: 30m)
--parallel int        Number of parallel test executions (default: 1)
--verbose             Enable verbose output
```

### Commands

#### run - Execute Tests

```bash
# Run all conformance tests
vcts run --provider vsphere-provider

# Run with custom settings
vcts run --provider vsphere-provider \
  --timeout 1h \
  --parallel 3 \
  --output-dir /tmp/test-results

# Skip specific tests
vcts run --provider libvirt-provider \
  --skip-tests "test-snapshots,test-linked-clones"

# Verbose output for debugging
vcts run --provider proxmox-provider --verbose
```

#### list - List Available Tests

```bash
# List all available tests
vcts list

# List tests for specific capability
vcts list --capability snapshots
```

#### validate - Validate Provider

```bash
# Validate provider configuration
vcts validate --provider vsphere-provider

# Check provider connectivity
vcts validate --provider vsphere-provider --check-connectivity
```

### Test Categories

1. **Basic Operations**: VM creation, deletion, power operations
2. **Lifecycle Management**: Start, stop, restart, suspend operations  
3. **Resource Management**: CPU, memory, disk operations
4. **Networking**: Network configuration and connectivity
5. **Storage**: Disk operations, resizing, multiple disks
6. **Snapshots**: Create, list, revert, delete snapshots
7. **Cloning**: VM cloning and linked clones
8. **Error Handling**: Provider error scenarios
9. **Performance**: Basic performance benchmarks

### Output Formats

Test results are available in multiple formats:

- **JUnit XML**: For CI/CD integration
- **JSON**: Machine-readable format  
- **HTML**: Human-readable report
- **TAP**: Test Anything Protocol

---

## vrtg-provider

Provider development toolkit for creating and managing VirtRigaud providers.

### Usage

```bash
vrtg-provider [command] [flags]
```

### Global Flags

```bash
--verbose     Enable verbose output
--help        Help for vrtg-provider
```

### Commands

#### init - Initialize Provider

Bootstrap a new provider project with scaffolding.

```bash
vrtg-provider init <provider-name> [flags]
```

**Flags:**
- `--template`: Template to use (grpc, rest, hybrid)
- `--output-dir`: Output directory (default: current directory)
- `--module`: Go module name
- `--author`: Author name for generated files

**Examples:**

```bash
# Create basic gRPC provider
vrtg-provider init my-provider --template grpc

# Create with custom module
vrtg-provider init my-provider \
  --template grpc \
  --module github.com/myorg/my-provider \
  --author "John Doe <john@example.com>"

# Create in specific directory
vrtg-provider init my-provider \
  --output-dir /path/to/providers \
  --template grpc
```

#### generate - Code Generation

Generate boilerplate code for provider implementation.

```bash
vrtg-provider generate [type] [flags]
```

**Types:**
- `client`: Generate client code
- `server`: Generate server implementation
- `tests`: Generate test scaffolding
- `docs`: Generate documentation templates

**Examples:**

```bash
# Generate client code
vrtg-provider generate client --provider my-provider

# Generate test scaffolding
vrtg-provider generate tests --provider my-provider

# Generate documentation
vrtg-provider generate docs --provider my-provider
```

#### verify - Verification

Verify provider implementation and compliance.

```bash
vrtg-provider verify [flags]
```

**Flags:**
- `--provider-dir`: Provider directory to verify
- `--check-interface`: Verify interface compliance
- `--check-docs`: Verify documentation completeness
- `--check-tests`: Verify test coverage

**Examples:**

```bash
# Basic verification
vrtg-provider verify --provider-dir ./my-provider

# Comprehensive check
vrtg-provider verify \
  --provider-dir ./my-provider \
  --check-interface \
  --check-docs \
  --check-tests
```

#### publish - Publishing

Prepare provider for publishing and distribution.

```bash
vrtg-provider publish [flags]
```

**Flags:**
- `--provider-dir`: Provider directory
- `--version`: Version to publish
- `--registry`: Container registry
- `--chart-repo`: Helm chart repository

**Examples:**

```bash
# Publish provider
vrtg-provider publish \
  --provider-dir ./my-provider \
  --version v1.0.0 \
  --registry ghcr.io/myorg

# Publish with Helm chart
vrtg-provider publish \
  --provider-dir ./my-provider \
  --version v1.0.0 \
  --registry ghcr.io/myorg \
  --chart-repo https://charts.myorg.com
```

### Provider Template Structure

```
my-provider/
├── cmd/
│   └── provider/
│       └── main.go              # Provider entry point
├── internal/
│   ├── provider/
│   │   ├── server.go           # gRPC server implementation
│   │   ├── client.go           # Provider client
│   │   └── types.go            # Provider-specific types
│   └── config/
│       └── config.go           # Configuration management
├── pkg/
│   └── api/                    # Public API interfaces
├── test/
│   ├── conformance/            # Conformance tests
│   └── integration/            # Integration tests
├── deploy/
│   ├── helm/                   # Helm charts
│   └── k8s/                    # Kubernetes manifests
├── docs/                       # Documentation
├── Dockerfile                  # Container image
├── Makefile                    # Build automation
└── README.md                   # Provider documentation
```

---

## virtrigaud-loadgen

Load testing and benchmarking tool for VirtRigaud deployments.

### Usage

```bash
virtrigaud-loadgen [command] [flags]
```

### Global Flags

```bash
--kubeconfig string   Path to kubeconfig file
--namespace string    Kubernetes namespace (default: "default")  
--output-dir string   Output directory for results (default: "./loadgen-results")
--config-file string  Load generation configuration file
--dry-run            Show what would be executed without running
--verbose            Enable verbose output
```

### Commands

#### run - Execute Load Test

```bash
virtrigaud-loadgen run [flags]
```

**Flags:**
- `--vms`: Number of VMs to create (default: 10)
- `--duration`: Test duration (default: 10m)
- `--ramp-up`: Ramp-up time (default: 2m)
- `--workers`: Number of concurrent workers (default: 5)
- `--provider`: Provider to test against
- `--vm-class`: VMClass to use for test VMs
- `--vm-image`: VMImage to use for test VMs

**Examples:**

```bash
# Basic load test
virtrigaud-loadgen run --vms 50 --duration 15m

# Comprehensive load test
virtrigaud-loadgen run \
  --vms 100 \
  --duration 30m \
  --ramp-up 5m \
  --workers 10 \
  --provider vsphere-provider

# Test with specific configuration
virtrigaud-loadgen run --config-file loadtest-config.yaml
```

#### config - Configuration Management

```bash
# Generate sample configuration
virtrigaud-loadgen config generate --output sample-config.yaml

# Validate configuration
virtrigaud-loadgen config validate --config-file my-config.yaml
```

### Configuration File

```yaml
# loadtest-config.yaml
metadata:
  name: "production-load-test"
  description: "Load test for production environment"

spec:
  # Test parameters
  vms: 100
  duration: "30m"
  rampUp: "5m"
  workers: 10
  
  # Target configuration
  provider: "vsphere-provider"
  namespace: "loadtest"
  
  # VM configuration
  vmClass: "standard-vm"
  vmImage: "ubuntu-22-04"
  
  # Test scenarios
  scenarios:
    - name: "vm-lifecycle"
      weight: 70
      operations:
        - create
        - start
        - stop
        - delete
    
    - name: "vm-operations"
      weight: 20
      operations:
        - snapshot
        - clone
        - reconfigure
    
    - name: "provider-stress"
      weight: 10
      operations:
        - rapid-create-delete
        - concurrent-operations

  # Reporting
  reporting:
    formats: ["json", "html", "csv"]
    metrics:
      - response-time
      - throughput
      - error-rate
      - resource-usage
```

### Metrics and Reporting

Load test results include:

- **Performance Metrics**: Response times, throughput, latency percentiles
- **Error Analysis**: Error rates, failure patterns, error categorization
- **Resource Usage**: CPU, memory, network utilization
- **Provider Metrics**: Provider-specific performance indicators
- **Trend Analysis**: Performance over time, bottleneck identification

### Output Formats

- **JSON**: Machine-readable results for automation
- **HTML**: Interactive dashboard with charts and graphs
- **CSV**: Raw data for further analysis
- **Prometheus**: Metrics export for monitoring systems

---

## Advanced Usage

### Automation and Scripting

#### Bash Integration

```bash
#!/bin/bash
# VM management script

# Function to check VM status
check_vm_status() {
  local vm_name=$1
  vrtg vm describe "$vm_name" --output json | jq -r '.status.powerState'
}

# Wait for VM to be ready
wait_for_vm() {
  local vm_name=$1
  local timeout=300
  local count=0
  
  while [ $count -lt $timeout ]; do
    status=$(check_vm_status "$vm_name")
    if [ "$status" = "On" ]; then
      echo "VM $vm_name is ready"
      return 0
    fi
    sleep 5
    count=$((count + 5))
  done
  
  echo "Timeout waiting for VM $vm_name"
  return 1
}

# Create and wait for VM
vrtg vm create --file vm-config.yaml
wait_for_vm "my-vm"
```

#### CI/CD Integration

```yaml
# .github/workflows/vm-test.yml
name: VM Integration Test

on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      
      - name: Install vrtg CLI
        run: |
          curl -L "https://github.com/projectbeskar/virtrigaud/releases/latest/download/vrtg-linux-amd64" -o vrtg
          chmod +x vrtg
          sudo mv vrtg /usr/local/bin/
      
      - name: Setup kubeconfig
        run: echo "${{ secrets.KUBECONFIG }}" | base64 -d > ~/.kube/config
      
      - name: Run conformance tests
        run: vcts run --provider test-provider --output-dir test-results
      
      - name: Upload test results
        uses: actions/upload-artifact@v3
        with:
          name: conformance-results
          path: test-results/
```

### Configuration Management

#### Environment-specific Configurations

```bash
# Development environment
export VRTG_KUBECONFIG=~/.kube/dev-config
export VRTG_NAMESPACE=development
export VRTG_OUTPUT=yaml

# Production environment  
export VRTG_KUBECONFIG=~/.kube/prod-config
export VRTG_NAMESPACE=production
export VRTG_OUTPUT=json

# Use environment-specific settings
vrtg vm list  # Uses environment variables
```

#### Configuration Files

Create `~/.vrtg/config.yaml`:

```yaml
contexts:
  development:
    kubeconfig: ~/.kube/dev-config
    namespace: development
    output: yaml
    timeout: 30s
  
  production:
    kubeconfig: ~/.kube/prod-config
    namespace: production
    output: json
    timeout: 60s

current-context: development

aliases:
  ls: vm list
  get: vm describe
  logs: provider logs
```

### Troubleshooting

#### Common Issues

1. **Connection Issues**
```bash
# Check cluster connectivity
vrtg provider list

# Validate kubeconfig
kubectl cluster-info

# Check provider logs
vrtg provider logs <provider-name> --tail 100
```

2. **Permission Issues**
```bash
# Check RBAC permissions
kubectl auth can-i create virtualmachines

# Get current user context
kubectl auth whoami
```

3. **Provider Issues**
```bash
# Check provider status
vrtg provider status <provider-name>

# Run diagnostics
vrtg diag bundle --include-logs
```

#### Debug Mode

Enable debug output:

```bash
# Global debug flag
vrtg --verbose vm list

# Provider-specific debugging
vrtg provider logs <provider-name> --follow --verbose

# Conformance test debugging
vcts run --provider <provider-name> --verbose
```

## See Also

- [Getting Started Guide](getting-started/quickstart.md)
- [Provider Development](providers/development.md)
- [API Reference](api-reference/)
- [Examples](examples/)
- [Troubleshooting](resilience.md)
