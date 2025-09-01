/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package scaffold provides provider project scaffolding functionality.
package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Config holds scaffolding configuration.
type Config struct {
	ProviderName string
	ProviderType string
	TargetDir    string
	Remote       bool
	Force        bool
}

// Scaffolder generates provider project structure.
type Scaffolder struct {
	config Config
}

// New creates a new scaffolder with the given configuration.
func New(config Config) *Scaffolder {
	return &Scaffolder{config: config}
}

// Generate creates the provider project structure.
func (s *Scaffolder) Generate() error {
	// Create template context
	ctx := s.createTemplateContext()

	// Generate all files
	files := s.getFileTemplates()
	for relativePath, tmplContent := range files {
		if err := s.generateFile(relativePath, tmplContent, ctx); err != nil {
			return fmt.Errorf("failed to generate %s: %w", relativePath, err)
		}
	}

	return nil
}

// createTemplateContext creates the template rendering context.
func (s *Scaffolder) createTemplateContext() map[string]interface{} {
	// Convert provider name to different cases
	providerName := s.config.ProviderName
	providerNameCamel := toCamelCase(providerName)
	providerNameUpper := strings.ToUpper(strings.ReplaceAll(providerName, "-", "_"))
	moduleName := fmt.Sprintf("provider-%s", providerName)

	return map[string]interface{}{
		"ProviderName":      providerName,
		"ProviderNameCamel": providerNameCamel,
		"ProviderNameUpper": providerNameUpper,
		"ProviderType":      s.config.ProviderType,
		"ModuleName":        moduleName,
		"Remote":            s.config.Remote,
		"IsVSphere":         s.config.ProviderType == "vsphere",
		"IsLibvirt":         s.config.ProviderType == "libvirt",
		"IsFirecracker":     s.config.ProviderType == "firecracker",
		"IsQEMU":            s.config.ProviderType == "qemu",
		"IsGeneric":         s.config.ProviderType == "generic",
	}
}

// generateFile generates a single file from a template.
func (s *Scaffolder) generateFile(relativePath, tmplContent string, ctx map[string]interface{}) error {
	// Process the file path through template (for dynamic names)
	pathTmpl, err := template.New("path").Parse(relativePath)
	if err != nil {
		return fmt.Errorf("failed to parse path template: %w", err)
	}

	var pathBuf strings.Builder
	if err := pathTmpl.Execute(&pathBuf, ctx); err != nil {
		return fmt.Errorf("failed to render path template: %w", err)
	}

	targetPath := filepath.Join(s.config.TargetDir, pathBuf.String())

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Check if file exists and force is not set
	if !s.config.Force {
		if _, err := os.Stat(targetPath); err == nil {
			return fmt.Errorf("file already exists: %s", targetPath)
		}
	}

	// Parse and execute template
	tmpl, err := template.New("file").Parse(tmplContent)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	file, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close() //nolint:errcheck // File close in defer is not critical

	if err := tmpl.Execute(file, ctx); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	return nil
}

// getFileTemplates returns a map of file paths to template content.
func (s *Scaffolder) getFileTemplates() map[string]string {
	return map[string]string{
		"main.go":                            mainGoTemplate,
		"go.mod":                             goModTemplate,
		"Makefile":                           makefileTemplate,
		"Dockerfile":                         dockerfileTemplate,
		"README.md":                          readmeTemplate,
		".gitignore":                         gitignoreTemplate,
		".github/workflows/ci.yml":           ciWorkflowTemplate,
		"config/deployment.yaml":             deploymentTemplate,
		"config/service.yaml":                serviceTemplate,
		"internal/provider/provider.go":      providerTemplate,
		"internal/provider/capabilities.go":  capabilitiesTemplate,
		"internal/provider/provider_test.go": providerTestTemplate,
	}
}

// toCamelCase converts a kebab-case string to CamelCase.
func toCamelCase(s string) string {
	parts := strings.Split(s, "-")
	for i, part := range parts {
		if len(part) > 0 {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "")
}

// Template constants (defined in separate files for clarity)
const mainGoTemplate = `/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/projectbeskar/virtrigaud/sdk/provider/server"
	"github.com/projectbeskar/virtrigaud/sdk/provider/middleware"
	"github.com/projectbeskar/virtrigaud/internal/version"
	"{{.ModuleName}}/internal/provider"
)

func main() {
	// Handle --version flag
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		println("{{.ProviderName}}-provider", version.String())
		os.Exit(0)
	}

	// Create logger
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Create server configuration
	config := server.DefaultConfig()
	config.Logger = logger
	config.Middleware = &middleware.Config{
		Logging: &middleware.LoggingConfig{
			Enabled: true,
			Logger:  logger,
		},
		Recovery: &middleware.RecoveryConfig{
			Enabled: true,
			Logger:  logger,
		},
	}

	// Create server
	srv, err := server.New(config)
	if err != nil {
		logger.Error("Failed to create server", "error", err)
		os.Exit(1)
	}

	// Create and register provider
	providerImpl := provider.New()
	srv.RegisterProvider(providerImpl)

	// Start server
	logger.Info("Starting {{.ProviderNameCamel}} provider server", "version", version.String())
	if err := srv.Serve(context.Background()); err != nil {
		logger.Error("Server failed", "error", err)
		os.Exit(1)
	}
}`

const goModTemplate = `module {{.ModuleName}}

go 1.23

require (
	github.com/projectbeskar/virtrigaud v0.1.0
)

// For local development, uncomment and adjust path:
// replace github.com/projectbeskar/virtrigaud => ../../
`

const makefileTemplate = `# Makefile for {{.ProviderNameCamel}} Provider

VERSION ?= $(shell git describe --tags --always --dirty)
GIT_SHA ?= $(shell git rev-parse HEAD)
LDFLAGS := -X github.com/projectbeskar/virtrigaud/internal/version.Version=$(VERSION) -X github.com/projectbeskar/virtrigaud/internal/version.GitSHA=$(GIT_SHA)

.PHONY: all
all: build

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o bin/provider-{{.ProviderName}} .

.PHONY: test
test:
	go test -v ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: docker-build
docker-build:
	docker build -t provider-{{.ProviderName}}:$(VERSION) .

.PHONY: clean
clean:
	rm -rf bin/

.PHONY: deps
deps:
	go mod download
	go mod tidy

.PHONY: verify
verify: fmt vet lint test
	@echo "✅ All checks passed"

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build        - Build the provider binary"
	@echo "  test         - Run tests"
	@echo "  lint         - Run linter"
	@echo "  fmt          - Format code"
	@echo "  vet          - Run go vet"
	@echo "  docker-build - Build Docker image"
	@echo "  clean        - Clean build artifacts"
	@echo "  deps         - Download dependencies"
	@echo "  verify       - Run all checks"
`

const dockerfileTemplate = `# Build stage
FROM golang:1.23-bookworm AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.mod
COPY go.sum go.sum

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the provider binary
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_SHA=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/projectbeskar/virtrigaud/internal/version.Version=${VERSION} -X github.com/projectbeskar/virtrigaud/internal/version.GitSHA=${GIT_SHA}" \
    -a -o provider-{{.ProviderName}} .

# Runtime stage
FROM gcr.io/distroless/static:nonroot

# Copy the binary from builder stage
COPY --from=builder /workspace/provider-{{.ProviderName}} /usr/local/bin/provider-{{.ProviderName}}

USER 65532:65532

# Expose gRPC and health ports
EXPOSE 9443 8080

ENTRYPOINT ["/usr/local/bin/provider-{{.ProviderName}}"]
`

const readmeTemplate = `# {{.ProviderNameCamel}} Provider

A VirtRigaud provider for {{.ProviderType}} virtualization platform.

## Overview

This provider implements the VirtRigaud provider interface to manage virtual machines on {{.ProviderType}}.

## Features

- ✅ VM lifecycle management (create, delete, power operations)
- ✅ VM description and status monitoring
{{if .IsVSphere}}- ✅ vSphere API integration
- ✅ Distributed virtual switches support
- ✅ Resource pool management{{end}}
{{if .IsLibvirt}}- ✅ Libvirt API integration
- ✅ KVM/QEMU virtualization
- ✅ Storage pool management{{end}}
{{if .Remote}}- ✅ Remote runtime deployment
- ✅ Kubernetes integration{{end}}

## Quick Start

### Building

'''bash
make build
'''

### Testing

'''bash
make test
make verify
'''

### Running Locally

'''bash
./bin/provider-{{.ProviderName}} --port 9443
'''

### Docker

'''bash
make docker-build
docker run -p 9443:9443 provider-{{.ProviderName}}:latest
'''

{{if .Remote}}
### Deployment

Deploy to Kubernetes:

'''bash
kubectl apply -f config/
'''
{{end}}

## Configuration

The provider accepts the following configuration:

| Parameter | Description | Default |
|-----------|-------------|---------|
| --port | gRPC server port | 9443 |
| --health-port | Health check port | 8080 |

{{if .IsVSphere}}
### vSphere Configuration

Configure vSphere credentials via environment variables:

'''bash
export VSPHERE_SERVER=vcenter.example.com
export VSPHERE_USERNAME=admin@vsphere.local
export VSPHERE_PASSWORD=password
'''
{{end}}

{{if .IsLibvirt}}
### Libvirt Configuration

Configure libvirt connection:

'''bash
export LIBVIRT_URI=qemu:///system
'''
{{end}}

## Development

### Prerequisites

- Go 1.23+
- Docker
- kubectl (for Kubernetes deployment)
{{if .IsVSphere}}- vSphere environment access{{end}}
{{if .IsLibvirt}}- Libvirt/KVM environment{{end}}

### Project Structure

'''
├── main.go                    # Entry point
├── internal/
│   └── provider/             # Provider implementation
├── config/                   # Kubernetes manifests
├── Dockerfile               # Container build
└── Makefile                # Build targets
'''

### Testing

Run the full test suite:

'''bash
make verify
'''

Run conformance tests:

'''bash
vrtg-provider verify --profile core
'''

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Run 'make verify'
6. Submit a pull request

## License

Licensed under the Apache License, Version 2.0.
`

const gitignoreTemplate = `# Binaries
bin/
*.exe
*.exe~
*.dll
*.so
*.dylib

# Go
*.test
*.out
vendor/

# IDE
.vscode/
.idea/
*.swp
*.swo
*~

# OS
.DS_Store
Thumbs.db

# Logs
*.log

# Coverage
coverage.out
cover.out

# Temporary files
tmp/
temp/
`

const ciWorkflowTemplate = `name: CI

on:
  push:
    branches: [ main, develop ]
  pull_request:
    branches: [ main, develop ]

{{ "env:" }}
  GO_VERSION: '1.23'

jobs:
  test:
    name: Test
    runs-on: ubuntu-latest
    steps:
    - name: Checkout
      uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{"{{"}} env.GO_VERSION {{"}}"}}

    - name: Cache Go modules
      uses: actions/cache@v3
      with:
        path: ~/go/pkg/mod
        key: ${{"{{"}} runner.os {{"}}"}}-go-${{"{{"}} hashFiles('**/go.sum') {{"}}"}}
        restore-keys: |
          ${{"{{"}} runner.os {{"}}"}}-go-

    - name: Download dependencies
      run: go mod download

    - name: Verify dependencies
      run: go mod verify

    - name: Run go vet
      run: go vet ./...

    - name: Run tests
      run: go test -v -race -coverprofile=coverage.out ./...

    - name: Upload coverage
      uses: codecov/codecov-action@v3
      with:
        file: ./coverage.out

  lint:
    name: Lint
    runs-on: ubuntu-latest
    steps:
    - name: Checkout
      uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{"{{"}} env.GO_VERSION {{"}}"}}

    - name: Run golangci-lint
      uses: golangci/golangci-lint-action@v3
      with:
        version: latest

  build:
    name: Build
    runs-on: ubuntu-latest
    needs: [test, lint]
    steps:
    - name: Checkout
      uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{"{{"}} env.GO_VERSION {{"}}"}}

    - name: Build binary
      run: make build

    - name: Build Docker image
      run: make docker-build
`

const deploymentTemplate = `{{if .Remote}}apiVersion: apps/v1
kind: Deployment
metadata:
  name: provider-{{.ProviderName}}
  labels:
    app: provider-{{.ProviderName}}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: provider-{{.ProviderName}}
  template:
    metadata:
      labels:
        app: provider-{{.ProviderName}}
    spec:
      containers:
      - name: provider
        image: provider-{{.ProviderName}}:latest
        ports:
        - containerPort: 9443
          name: grpc
        - containerPort: 8080
          name: health
        {{ "env:" }}
        - name: LOG_LEVEL
          value: "info"
        {{if .IsVSphere}}- name: VSPHERE_SERVER
          valueFrom:
            secretKeyRef:
              name: {{.ProviderName}}-credentials
              key: server
        - name: VSPHERE_USERNAME
          valueFrom:
            secretKeyRef:
              name: {{.ProviderName}}-credentials
              key: username
        - name: VSPHERE_PASSWORD
          valueFrom:
            secretKeyRef:
              name: {{.ProviderName}}-credentials
              key: password{{end}}
        {{if .IsLibvirt}}- name: LIBVIRT_URI
          value: "qemu:///system"{{end}}
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /ready
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
        resources:
          requests:
            memory: "64Mi"
            cpu: "50m"
          limits:
            memory: "128Mi"
            cpu: "100m"
{{else}}# Provider is configured for in-process mode
# No deployment needed - provider runs as part of manager{{end}}`

const serviceTemplate = `{{if .Remote}}apiVersion: v1
kind: Service
metadata:
  name: provider-{{.ProviderName}}
  labels:
    app: provider-{{.ProviderName}}
spec:
  selector:
    app: provider-{{.ProviderName}}
  ports:
  - name: grpc
    port: 9443
    targetPort: 9443
    protocol: TCP
  - name: health
    port: 8080
    targetPort: 8080
    protocol: TCP
  type: ClusterIP
{{else}}# Provider is configured for in-process mode
# No service needed{{end}}`

const providerTemplate = `/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"context"
	"fmt"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
)

// Provider implements the {{.ProviderNameCamel}} provider.
type Provider struct {
	providerv1.UnimplementedProviderServer
	capabilities *capabilities.Manager
}

// New creates a new {{.ProviderNameCamel}} provider.
func New() *Provider {
	// Build capabilities for this provider
	caps := capabilities.NewBuilder().
		Core().
		{{if eq .ProviderType "vsphere"}}VSphere().
		Snapshots().
		LinkedClones().
		OnlineReconfigure().
		DiskTypes("thin", "thick", "eager_zero").
		NetworkTypes("distributed", "standard").{{end}}
		{{if eq .ProviderType "libvirt"}}Libvirt().
		Snapshots().
		LinkedClones().
		DiskTypes("qcow2", "raw").
		NetworkTypes("bridge", "nat").{{end}}
		{{if eq .ProviderType "firecracker"}}Firecracker().
		DiskTypes("raw").
		NetworkTypes("tap").{{end}}
		{{if eq .ProviderType "qemu"}}QEMU().
		Snapshots().
		DiskTypes("qcow2", "raw", "vmdk").
		NetworkTypes("bridge", "user").{{end}}
		{{if eq .ProviderType "generic"}}DiskTypes("raw").
		NetworkTypes("bridge").{{end}}
		Build()

	return &Provider{
		capabilities: caps,
	}
}

// Validate validates the provider configuration.
func (p *Provider) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	// TODO: Implement {{.ProviderType}} connection validation
	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: "{{.ProviderNameCamel}} provider is ready",
	}, nil
}

// Create creates a new virtual machine.
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	// TODO: Implement VM creation for {{.ProviderType}}
	return nil, errors.NewUnimplemented("Create")
}

// Delete deletes a virtual machine.
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement VM deletion for {{.ProviderType}}
	return nil, errors.NewUnimplemented("Delete")
}

// Power performs power operations on a virtual machine.
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement power operations for {{.ProviderType}}
	return nil, errors.NewUnimplemented("Power")
}

// Reconfigure reconfigures a virtual machine.
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement VM reconfiguration for {{.ProviderType}}
	return nil, errors.NewUnimplemented("Reconfigure")
}

// Describe describes a virtual machine's current state.
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	// TODO: Implement VM description for {{.ProviderType}}
	return nil, errors.NewUnimplemented("Describe")
}

// TaskStatus checks the status of an async task.
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	// TODO: Implement task status checking for {{.ProviderType}}
	return nil, errors.NewUnimplemented("TaskStatus")
}

// SnapshotCreate creates a VM snapshot.
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	// TODO: Implement snapshot creation for {{.ProviderType}}
	return nil, errors.NewUnimplemented("SnapshotCreate")
}

// SnapshotDelete deletes a VM snapshot.
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement snapshot deletion for {{.ProviderType}}
	return nil, errors.NewUnimplemented("SnapshotDelete")
}

// SnapshotRevert reverts a VM to a snapshot.
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement snapshot revert for {{.ProviderType}}
	return nil, errors.NewUnimplemented("SnapshotRevert")
}

// Clone clones a virtual machine.
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	// TODO: Implement VM cloning for {{.ProviderType}}
	return nil, errors.NewUnimplemented("Clone")
}

// ImagePrepare prepares an image for use.
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement image preparation for {{.ProviderType}}
	return nil, errors.NewUnimplemented("ImagePrepare")
}

// GetCapabilities returns the provider's capabilities.
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return p.capabilities.GetCapabilities(ctx, req)
}
`

const capabilitiesTemplate = `/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
)

// GetProviderCapabilities returns the capabilities for this provider type.
func GetProviderCapabilities() *capabilities.Manager {
	builder := capabilities.NewBuilder().Core()

	{{if eq .ProviderType "vsphere"}}// vSphere-specific capabilities
	builder = builder.
		VSphere().
		Snapshots().
		MemorySnapshots().
		LinkedClones().
		OnlineReconfigure().
		OnlineDiskExpansion().
		ImageImport().
		DiskTypes("thin", "thick", "eager_zero").
		NetworkTypes("distributed", "standard", "vlan"){{end}}

	{{if eq .ProviderType "libvirt"}}// Libvirt-specific capabilities
	builder = builder.
		Libvirt().
		Snapshots().
		LinkedClones().
		OnlineReconfigure().
		DiskTypes("qcow2", "raw", "vmdk").
		NetworkTypes("bridge", "nat", "ovs"){{end}}

	{{if eq .ProviderType "firecracker"}}// Firecracker-specific capabilities
	builder = builder.
		Firecracker().
		DiskTypes("raw").
		NetworkTypes("tap"){{end}}

	{{if eq .ProviderType "qemu"}}// QEMU-specific capabilities
	builder = builder.
		QEMU().
		Snapshots().
		LinkedClones().
		DiskTypes("qcow2", "raw", "vmdk", "vdi").
		NetworkTypes("bridge", "user", "tap"){{end}}

	{{if eq .ProviderType "generic"}}// Generic provider capabilities
	builder = builder.
		DiskTypes("raw").
		NetworkTypes("bridge"){{end}}

	return builder.Build()
}
`

const providerTestTemplate = `/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"context"
	"testing"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestProvider_Validate(t *testing.T) {
	provider := New()

	req := &providerv1.ValidateRequest{}
	resp, err := provider.Validate(context.Background(), req)

	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	if !resp.Ok {
		t.Errorf("Expected validation to succeed, got: %s", resp.Message)
	}
}

func TestProvider_GetCapabilities(t *testing.T) {
	provider := New()

	req := &providerv1.GetCapabilitiesRequest{}
	resp, err := provider.GetCapabilities(context.Background(), req)

	if err != nil {
		t.Fatalf("GetCapabilities failed: %v", err)
	}

	if resp == nil {
		t.Fatal("GetCapabilities returned nil response")
	}

	// Verify basic capabilities are present
	{{if eq .ProviderType "vsphere"}}if !resp.SupportsSnapshots {
		t.Error("Expected vSphere provider to support snapshots")
	}

	if !resp.SupportsLinkedClones {
		t.Error("Expected vSphere provider to support linked clones")
	}{{end}}

	{{if eq .ProviderType "libvirt"}}if !resp.SupportsSnapshots {
		t.Error("Expected libvirt provider to support snapshots")
	}{{end}}

	if len(resp.SupportedDiskTypes) == 0 {
		t.Error("Expected provider to support at least one disk type")
	}

	if len(resp.SupportedNetworkTypes) == 0 {
		t.Error("Expected provider to support at least one network type")
	}
}

func TestProvider_UnimplementedOperations(t *testing.T) {
	provider := New()
	ctx := context.Background()

	// Test that unimplemented operations return appropriate errors
	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "Create",
			fn: func() error {
				_, err := provider.Create(ctx, &providerv1.CreateRequest{})
				return err
			},
		},
		{
			name: "Delete",
			fn: func() error {
				_, err := provider.Delete(ctx, &providerv1.DeleteRequest{})
				return err
			},
		},
		{
			name: "Power",
			fn: func() error {
				_, err := provider.Power(ctx, &providerv1.PowerRequest{})
				return err
			},
		},
		{
			name: "Describe",
			fn: func() error {
				_, err := provider.Describe(ctx, &providerv1.DescribeRequest{})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if err == nil {
				t.Errorf("Expected %s to return unimplemented error", tt.name)
			}
		})
	}
}
`
