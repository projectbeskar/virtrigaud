# Provider Developer Tutorial

This comprehensive tutorial walks you through creating a complete VirtRigaud provider from scratch. By the end, you'll have a fully functional provider that can create, manage, and delete virtual machines.

## Prerequisites

Before starting this tutorial, ensure you have:

- Go 1.23 or later installed
- Docker installed for containerization
- kubectl and a Kubernetes cluster (Kind/minikube for local development)
- Helm 3.x installed
- Basic understanding of gRPC and protobuf

## Tutorial Overview

We'll build a **File Provider** that manages "virtual machines" as JSON files on disk. While not practical for production, this provider demonstrates all the core concepts without requiring actual hypervisor access.

**What we'll build:**
- A complete provider implementation using the VirtRigaud SDK
- Conformance tests that pass VCTS core profile
- A Helm chart for deployment
- CI/CD integration
- Publication to the provider catalog

## Step 1: Initialize Your Provider Project

### 1.1 Create Project Structure

```bash
# Create project directory
mkdir virtrigaud-provider-file
cd virtrigaud-provider-file

# Initialize the provider project
vrtg-provider init file
```

The `vrtg-provider init` command creates the following structure:

```
virtrigaud-provider-file/
├── cmd/
│   └── provider-file/
│       ├── main.go
│       └── Dockerfile
├── internal/
│   └── provider/
│       ├── provider.go
│       ├── capabilities.go
│       └── provider_test.go
├── charts/
│   └── provider-file/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
├── .github/
│   └── workflows/
│       └── ci.yml
├── Makefile
├── go.mod
├── go.sum
├── .gitignore
└── README.md
```

### 1.2 Examine Generated Files

**main.go** - Entry point that sets up the gRPC server:
```go
package main

import (
    "log"
    
    "github.com/projectbeskar/virtrigaud/sdk/provider/server"
    "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
    "virtrigaud-provider-file/internal/provider"
)

func main() {
    // Create provider instance
    p, err := provider.New()
    if err != nil {
        log.Fatalf("Failed to create provider: %v", err)
    }
    
    // Configure server
    config := &server.Config{
        Port:        9443,
        HealthPort:  8080,
        EnableTLS:   false,
    }
    
    srv, err := server.New(config)
    if err != nil {
        log.Fatalf("Failed to create server: %v", err)
    }
    
    // Register provider service
    providerv1.RegisterProviderServiceServer(srv.GRPCServer(), p)
    
    // Start server
    log.Println("Starting file provider on port 9443...")
    if err := srv.Serve(); err != nil {
        log.Fatalf("Server failed: %v", err)
    }
}
```

**go.mod** - Module definition with SDK dependency:
```go
module virtrigaud-provider-file

go 1.23

require (
    github.com/projectbeskar/virtrigaud/sdk v0.1.0
    github.com/projectbeskar/virtrigaud/proto v0.1.0
)
```

## Step 2: Implement the Core Provider

### 2.1 Design the File Provider

Our file provider will:
- Store VM metadata as JSON files in `/var/lib/virtrigaud/vms/`
- Use filename as VM ID
- Simulate power operations with state files
- Support basic CRUD operations

### 2.2 Define the VM Model

Create `internal/provider/vm.go`:

```go
package provider

import (
    "encoding/json"
    "fmt"
    "io/ioutil"
    "os"
    "path/filepath"
    "time"
    
    "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

type VirtualMachine struct {
    ID          string                 `json:"id"`
    Name        string                 `json:"name"`
    Spec        *providerv1.VMSpec     `json:"spec"`
    Status      *providerv1.VMStatus   `json:"status"`
    CreatedAt   time.Time              `json:"created_at"`
    UpdatedAt   time.Time              `json:"updated_at"`
}

type FileStore struct {
    baseDir string
}

func NewFileStore(baseDir string) *FileStore {
    return &FileStore{baseDir: baseDir}
}

func (fs *FileStore) Save(vm *VirtualMachine) error {
    if err := os.MkdirAll(fs.baseDir, 0755); err != nil {
        return fmt.Errorf("failed to create directory: %w", err)
    }
    
    vm.UpdatedAt = time.Now()
    data, err := json.MarshalIndent(vm, "", "  ")
    if err != nil {
        return fmt.Errorf("failed to marshal VM: %w", err)
    }
    
    filename := filepath.Join(fs.baseDir, vm.ID+".json")
    return ioutil.WriteFile(filename, data, 0644)
}

func (fs *FileStore) Load(id string) (*VirtualMachine, error) {
    filename := filepath.Join(fs.baseDir, id+".json")
    data, err := ioutil.ReadFile(filename)
    if err != nil {
        if os.IsNotExist(err) {
            return nil, fmt.Errorf("VM not found: %s", id)
        }
        return nil, fmt.Errorf("failed to read VM file: %w", err)
    }
    
    var vm VirtualMachine
    if err := json.Unmarshal(data, &vm); err != nil {
        return nil, fmt.Errorf("failed to unmarshal VM: %w", err)
    }
    
    return &vm, nil
}

func (fs *FileStore) Delete(id string) error {
    filename := filepath.Join(fs.baseDir, id+".json")
    if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
        return fmt.Errorf("failed to delete VM file: %w", err)
    }
    return nil
}

func (fs *FileStore) List() ([]*VirtualMachine, error) {
    files, err := ioutil.ReadDir(fs.baseDir)
    if err != nil {
        if os.IsNotExist(err) {
            return []*VirtualMachine{}, nil
        }
        return nil, fmt.Errorf("failed to read directory: %w", err)
    }
    
    var vms []*VirtualMachine
    for _, file := range files {
        if !file.IsDir() && filepath.Ext(file.Name()) == ".json" {
            id := file.Name()[:len(file.Name())-5] // Remove .json extension
            vm, err := fs.Load(id)
            if err != nil {
                continue // Skip invalid files
            }
            vms = append(vms, vm)
        }
    }
    
    return vms, nil
}
```

### 2.3 Implement the Provider Interface

Update `internal/provider/provider.go`:

```go
package provider

import (
    "context"
    "fmt"
    "os"
    "path/filepath"
    "time"
    
    "github.com/google/uuid"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
    
    "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
    "github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
    "github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

type Provider struct {
    store *FileStore
    caps  *capabilities.ProviderCapabilities
}

func New() (*Provider, error) {
    // Get storage directory from environment or use default
    baseDir := os.Getenv("PROVIDER_STORAGE_DIR")
    if baseDir == "" {
        baseDir = "/var/lib/virtrigaud/vms"
    }
    
    // Create capabilities
    caps := &capabilities.ProviderCapabilities{
        ProviderInfo: &providerv1.ProviderInfo{
            Name:        "file",
            Version:     "0.1.0",
            Description: "File-based virtual machine provider for development and testing",
        },
        SupportedCapabilities: []capabilities.Capability{
            capabilities.CapabilityCore,
            capabilities.CapabilitySnapshot,
            capabilities.CapabilityClone,
        },
    }
    
    return &Provider{
        store: NewFileStore(baseDir),
        caps:  caps,
    }, nil
}

// GetCapabilities returns provider capabilities
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
    return &providerv1.GetCapabilitiesResponse{
        ProviderId: "file-provider",
        Capabilities: []*providerv1.Capability{
            {
                Name:        "vm.create",
                Supported:   true,
                Description: "Create virtual machines",
            },
            {
                Name:        "vm.read",
                Supported:   true,
                Description: "Read virtual machine information",
            },
            {
                Name:        "vm.update",
                Supported:   true,
                Description: "Update virtual machine configuration",
            },
            {
                Name:        "vm.delete",
                Supported:   true,
                Description: "Delete virtual machines",
            },
            {
                Name:        "vm.power",
                Supported:   true,
                Description: "Control virtual machine power state",
            },
            {
                Name:        "vm.snapshot",
                Supported:   true,
                Description: "Create and manage VM snapshots",
            },
            {
                Name:        "vm.clone",
                Supported:   true,
                Description: "Clone virtual machines",
            },
        },
    }, nil
}

// CreateVM creates a new virtual machine
func (p *Provider) CreateVM(ctx context.Context, req *providerv1.CreateVMRequest) (*providerv1.CreateVMResponse, error) {
    // Validate request
    if req.Name == "" {
        return nil, errors.NewInvalidSpec("VM name is required")
    }
    
    if req.Spec == nil {
        return nil, errors.NewInvalidSpec("VM spec is required")
    }
    
    // Generate unique ID
    vmID := uuid.New().String()
    
    // Create VM object
    vm := &VirtualMachine{
        ID:   vmID,
        Name: req.Name,
        Spec: req.Spec,
        Status: &providerv1.VMStatus{
            State:   "Creating",
            Message: "VM is being created",
        },
        CreatedAt: time.Now(),
        UpdatedAt: time.Now(),
    }
    
    // Save to store
    if err := p.store.Save(vm); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to save VM: %v", err)
    }
    
    // Simulate creation time
    go func() {
        time.Sleep(2 * time.Second)
        vm.Status.State = "Running"
        vm.Status.Message = "VM is running"
        p.store.Save(vm)
    }()
    
    return &providerv1.CreateVMResponse{
        VmId:   vmID,
        Status: vm.Status,
    }, nil
}

// GetVM retrieves virtual machine information
func (p *Provider) GetVM(ctx context.Context, req *providerv1.GetVMRequest) (*providerv1.GetVMResponse, error) {
    if req.VmId == "" {
        return nil, errors.NewInvalidSpec("VM ID is required")
    }
    
    vm, err := p.store.Load(req.VmId)
    if err != nil {
        return nil, errors.NewNotFound("VM not found: %s", req.VmId)
    }
    
    return &providerv1.GetVMResponse{
        VmId:   vm.ID,
        Name:   vm.Name,
        Spec:   vm.Spec,
        Status: vm.Status,
    }, nil
}

// UpdateVM updates virtual machine configuration
func (p *Provider) UpdateVM(ctx context.Context, req *providerv1.UpdateVMRequest) (*providerv1.UpdateVMResponse, error) {
    if req.VmId == "" {
        return nil, errors.NewInvalidSpec("VM ID is required")
    }
    
    vm, err := p.store.Load(req.VmId)
    if err != nil {
        return nil, errors.NewNotFound("VM not found: %s", req.VmId)
    }
    
    // Update spec if provided
    if req.Spec != nil {
        vm.Spec = req.Spec
        vm.Status.Message = "VM configuration updated"
        
        if err := p.store.Save(vm); err != nil {
            return nil, status.Errorf(codes.Internal, "failed to save VM: %v", err)
        }
    }
    
    return &providerv1.UpdateVMResponse{
        Status: vm.Status,
    }, nil
}

// DeleteVM deletes a virtual machine
func (p *Provider) DeleteVM(ctx context.Context, req *providerv1.DeleteVMRequest) (*providerv1.DeleteVMResponse, error) {
    if req.VmId == "" {
        return nil, errors.NewInvalidSpec("VM ID is required")
    }
    
    // Check if VM exists
    _, err := p.store.Load(req.VmId)
    if err != nil {
        return nil, errors.NewNotFound("VM not found: %s", req.VmId)
    }
    
    // Delete VM
    if err := p.store.Delete(req.VmId); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to delete VM: %v", err)
    }
    
    return &providerv1.DeleteVMResponse{
        Success: true,
        Message: "VM deleted successfully",
    }, nil
}

// PowerVM controls virtual machine power state
func (p *Provider) PowerVM(ctx context.Context, req *providerv1.PowerVMRequest) (*providerv1.PowerVMResponse, error) {
    if req.VmId == "" {
        return nil, errors.NewInvalidSpec("VM ID is required")
    }
    
    vm, err := p.store.Load(req.VmId)
    if err != nil {
        return nil, errors.NewNotFound("VM not found: %s", req.VmId)
    }
    
    // Update power state based on operation
    switch req.PowerOp {
    case providerv1.PowerOp_POWER_OP_ON:
        vm.Status.State = "Running"
        vm.Status.Message = "VM is running"
    case providerv1.PowerOp_POWER_OP_OFF:
        vm.Status.State = "Stopped"
        vm.Status.Message = "VM is stopped"
    case providerv1.PowerOp_POWER_OP_REBOOT:
        vm.Status.State = "Rebooting"
        vm.Status.Message = "VM is rebooting"
        // Simulate reboot
        go func() {
            time.Sleep(3 * time.Second)
            vm.Status.State = "Running"
            vm.Status.Message = "VM is running"
            p.store.Save(vm)
        }()
    default:
        return nil, errors.NewInvalidSpec("unsupported power operation: %v", req.PowerOp)
    }
    
    if err := p.store.Save(vm); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to save VM: %v", err)
    }
    
    return &providerv1.PowerVMResponse{
        Status: vm.Status,
    }, nil
}

// ListVMs lists all virtual machines
func (p *Provider) ListVMs(ctx context.Context, req *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
    vms, err := p.store.List()
    if err != nil {
        return nil, status.Errorf(codes.Internal, "failed to list VMs: %v", err)
    }
    
    var vmInfos []*providerv1.VMInfo
    for _, vm := range vms {
        vmInfos = append(vmInfos, &providerv1.VMInfo{
            VmId:   vm.ID,
            Name:   vm.Name,
            Status: vm.Status,
        })
    }
    
    return &providerv1.ListVMsResponse{
        Vms: vmInfos,
    }, nil
}

// CreateSnapshot creates a VM snapshot
func (p *Provider) CreateSnapshot(ctx context.Context, req *providerv1.CreateSnapshotRequest) (*providerv1.CreateSnapshotResponse, error) {
    if req.VmId == "" {
        return nil, errors.NewInvalidSpec("VM ID is required")
    }
    
    vm, err := p.store.Load(req.VmId)
    if err != nil {
        return nil, errors.NewNotFound("VM not found: %s", req.VmId)
    }
    
    // Create snapshot (simulate by copying VM file)
    snapshotID := uuid.New().String()
    snapshotPath := filepath.Join(filepath.Dir(p.store.baseDir), "snapshots")
    
    if err := os.MkdirAll(snapshotPath, 0755); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to create snapshot directory: %v", err)
    }
    
    // Copy VM data to snapshot
    snapshotVM := *vm
    snapshotVM.ID = snapshotID
    snapshotStore := NewFileStore(snapshotPath)
    
    if err := snapshotStore.Save(&snapshotVM); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to save snapshot: %v", err)
    }
    
    return &providerv1.CreateSnapshotResponse{
        SnapshotId: snapshotID,
        Status: &providerv1.TaskStatus{
            State:   "Completed",
            Message: "Snapshot created successfully",
        },
    }, nil
}

// CloneVM clones a virtual machine
func (p *Provider) CloneVM(ctx context.Context, req *providerv1.CloneVMRequest) (*providerv1.CloneVMResponse, error) {
    if req.SourceVmId == "" {
        return nil, errors.NewInvalidSpec("Source VM ID is required")
    }
    
    if req.CloneName == "" {
        return nil, errors.NewInvalidSpec("Clone name is required")
    }
    
    // Load source VM
    sourceVM, err := p.store.Load(req.SourceVmId)
    if err != nil {
        return nil, errors.NewNotFound("Source VM not found: %s", req.SourceVmId)
    }
    
    // Create clone
    cloneID := uuid.New().String()
    cloneVM := &VirtualMachine{
        ID:   cloneID,
        Name: req.CloneName,
        Spec: sourceVM.Spec, // Copy spec from source
        Status: &providerv1.VMStatus{
            State:   "Stopped",
            Message: "Clone created successfully",
        },
        CreatedAt: time.Now(),
        UpdatedAt: time.Now(),
    }
    
    if err := p.store.Save(cloneVM); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to save clone: %v", err)
    }
    
    return &providerv1.CloneVMResponse{
        CloneVmId: cloneID,
        Status: &providerv1.TaskStatus{
            State:   "Completed",
            Message: "VM cloned successfully",
        },
    }, nil
}
```

## Step 3: Add Tests and Validation

### 3.1 Create Unit Tests

Create `internal/provider/provider_test.go`:

```go
package provider

import (
    "context"
    "os"
    "path/filepath"
    "testing"
    "time"
    
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    
    "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

func TestProvider_CreateVM(t *testing.T) {
    // Create temporary directory for testing
    tmpDir, err := os.MkdirTemp("", "file-provider-test")
    require.NoError(t, err)
    defer os.RemoveAll(tmpDir)
    
    // Set storage directory
    os.Setenv("PROVIDER_STORAGE_DIR", tmpDir)
    defer os.Unsetenv("PROVIDER_STORAGE_DIR")
    
    // Create provider
    p, err := New()
    require.NoError(t, err)
    
    // Test VM creation
    req := &providerv1.CreateVMRequest{
        Name: "test-vm",
        Spec: &providerv1.VMSpec{
            Cpu:    2,
            Memory: 4096,
            Image:  "ubuntu:20.04",
        },
    }
    
    resp, err := p.CreateVM(context.Background(), req)
    require.NoError(t, err)
    assert.NotEmpty(t, resp.VmId)
    assert.Equal(t, "Creating", resp.Status.State)
    
    // Verify VM file was created
    vmFile := filepath.Join(tmpDir, resp.VmId+".json")
    assert.FileExists(t, vmFile)
}

func TestProvider_GetVM(t *testing.T) {
    tmpDir, err := os.MkdirTemp("", "file-provider-test")
    require.NoError(t, err)
    defer os.RemoveAll(tmpDir)
    
    os.Setenv("PROVIDER_STORAGE_DIR", tmpDir)
    defer os.Unsetenv("PROVIDER_STORAGE_DIR")
    
    p, err := New()
    require.NoError(t, err)
    
    // Create VM first
    createReq := &providerv1.CreateVMRequest{
        Name: "test-vm",
        Spec: &providerv1.VMSpec{
            Cpu:    2,
            Memory: 4096,
        },
    }
    
    createResp, err := p.CreateVM(context.Background(), createReq)
    require.NoError(t, err)
    
    // Get VM
    getReq := &providerv1.GetVMRequest{
        VmId: createResp.VmId,
    }
    
    getResp, err := p.GetVM(context.Background(), getReq)
    require.NoError(t, err)
    assert.Equal(t, createResp.VmId, getResp.VmId)
    assert.Equal(t, "test-vm", getResp.Name)
    assert.Equal(t, int32(2), getResp.Spec.Cpu)
}

func TestProvider_PowerVM(t *testing.T) {
    tmpDir, err := os.MkdirTemp("", "file-provider-test")
    require.NoError(t, err)
    defer os.RemoveAll(tmpDir)
    
    os.Setenv("PROVIDER_STORAGE_DIR", tmpDir)
    defer os.Unsetenv("PROVIDER_STORAGE_DIR")
    
    p, err := New()
    require.NoError(t, err)
    
    // Create VM
    createReq := &providerv1.CreateVMRequest{
        Name: "test-vm",
        Spec: &providerv1.VMSpec{Cpu: 1, Memory: 1024},
    }
    
    createResp, err := p.CreateVM(context.Background(), createReq)
    require.NoError(t, err)
    
    // Power off VM
    powerReq := &providerv1.PowerVMRequest{
        VmId:    createResp.VmId,
        PowerOp: providerv1.PowerOp_POWER_OP_OFF,
    }
    
    powerResp, err := p.PowerVM(context.Background(), powerReq)
    require.NoError(t, err)
    assert.Equal(t, "Stopped", powerResp.Status.State)
    
    // Power on VM
    powerReq.PowerOp = providerv1.PowerOp_POWER_OP_ON
    powerResp, err = p.PowerVM(context.Background(), powerReq)
    require.NoError(t, err)
    assert.Equal(t, "Running", powerResp.Status.State)
}

func TestProvider_GetCapabilities(t *testing.T) {
    p, err := New()
    require.NoError(t, err)
    
    req := &providerv1.GetCapabilitiesRequest{}
    resp, err := p.GetCapabilities(context.Background(), req)
    require.NoError(t, err)
    
    assert.Equal(t, "file-provider", resp.ProviderId)
    assert.NotEmpty(t, resp.Capabilities)
    
    // Check for core capabilities
    capNames := make(map[string]bool)
    for _, cap := range resp.Capabilities {
        capNames[cap.Name] = cap.Supported
    }
    
    assert.True(t, capNames["vm.create"])
    assert.True(t, capNames["vm.read"])
    assert.True(t, capNames["vm.delete"])
    assert.True(t, capNames["vm.power"])
}

func TestProvider_CloneVM(t *testing.T) {
    tmpDir, err := os.MkdirTemp("", "file-provider-test")
    require.NoError(t, err)
    defer os.RemoveAll(tmpDir)
    
    os.Setenv("PROVIDER_STORAGE_DIR", tmpDir)
    defer os.Unsetenv("PROVIDER_STORAGE_DIR")
    
    p, err := New()
    require.NoError(t, err)
    
    // Create source VM
    createReq := &providerv1.CreateVMRequest{
        Name: "source-vm",
        Spec: &providerv1.VMSpec{
            Cpu:    4,
            Memory: 8192,
            Image:  "centos:8",
        },
    }
    
    createResp, err := p.CreateVM(context.Background(), createReq)
    require.NoError(t, err)
    
    // Clone VM
    cloneReq := &providerv1.CloneVMRequest{
        SourceVmId: createResp.VmId,
        CloneName:  "cloned-vm",
    }
    
    cloneResp, err := p.CloneVM(context.Background(), cloneReq)
    require.NoError(t, err)
    assert.NotEmpty(t, cloneResp.CloneVmId)
    assert.NotEqual(t, createResp.VmId, cloneResp.CloneVmId)
    
    // Verify clone has same specs as source
    getReq := &providerv1.GetVMRequest{
        VmId: cloneResp.CloneVmId,
    }
    
    getResp, err := p.GetVM(context.Background(), getReq)
    require.NoError(t, err)
    assert.Equal(t, "cloned-vm", getResp.Name)
    assert.Equal(t, int32(4), getResp.Spec.Cpu)
    assert.Equal(t, int32(8192), getResp.Spec.Memory)
    assert.Equal(t, "centos:8", getResp.Spec.Image)
}
```

### 3.2 Add Build and Test Targets

Update the `Makefile`:

```makefile
# File Provider Makefile

.PHONY: help build test lint clean run docker-build docker-push

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the provider binary
	go build -o bin/provider-file ./cmd/provider-file

test: ## Run tests
	go test -v ./...

test-coverage: ## Run tests with coverage
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

lint: ## Run linters
	golangci-lint run ./...

clean: ## Clean build artifacts
	rm -rf bin/
	rm -f coverage.out coverage.html

run: build ## Run the provider locally
	PROVIDER_STORAGE_DIR=/tmp/virtrigaud-file ./bin/provider-file

docker-build: ## Build Docker image
	docker build -f cmd/provider-file/Dockerfile -t provider-file:latest .

docker-push: docker-build ## Build and push Docker image
	docker tag provider-file:latest ghcr.io/yourorg/provider-file:latest
	docker push ghcr.io/yourorg/provider-file:latest

# Development targets
dev-setup: ## Set up development environment
	go mod download
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

integration-test: build ## Run integration tests
	./scripts/integration-test.sh
```

## Step 4: Test with VCTS (VirtRigaud Conformance Test Suite)

### 4.1 Install VCTS

```bash
# Build VCTS from the main repository
go install github.com/projectbeskar/virtrigaud/cmd/vcts@latest
```

### 4.2 Create VCTS Configuration

Create `vcts-config.yaml`:

```yaml
provider:
  name: "file"
  endpoint: "localhost:9443"
  tls: false
  
profiles:
  core:
    enabled: true
    vm_specs:
      - name: "basic"
        cpu: 1
        memory: 1024
        image: "test:latest"
      - name: "medium"
        cpu: 2
        memory: 4096
        image: "ubuntu:20.04"
        
  snapshot:
    enabled: true
    
  clone:
    enabled: true

tests:
  timeout: "30s"
  parallel: false
  cleanup: true
```

### 4.3 Run Conformance Tests

```bash
# Start the provider
make run &
PROVIDER_PID=$!

# Wait for provider to start
sleep 3

# Run VCTS core profile
vcts run --config vcts-config.yaml --profile core

# Run all enabled profiles
vcts run --config vcts-config.yaml --profile all

# Stop the provider
kill $PROVIDER_PID
```

Expected output:
```
✅ Core Profile Tests
  ✅ Provider.GetCapabilities
  ✅ Provider.CreateVM
  ✅ Provider.GetVM
  ✅ Provider.UpdateVM
  ✅ Provider.DeleteVM
  ✅ Provider.PowerVM
  ✅ Provider.ListVMs

✅ Snapshot Profile Tests
  ✅ Provider.CreateSnapshot

✅ Clone Profile Tests
  ✅ Provider.CloneVM

🎉 All tests passed! Provider is conformant.
```

## Step 5: Create Helm Chart for Deployment

### 5.1 Chart Structure

The generated chart in `charts/provider-file/` includes:

```
charts/provider-file/
├── Chart.yaml
├── values.yaml
├── templates/
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── serviceaccount.yaml
│   ├── rbac.yaml
│   └── _helpers.tpl
└── examples/
    └── values-development.yaml
```

### 5.2 Customize Chart Values

Update `charts/provider-file/values.yaml`:

```yaml
# Default values for provider-file

replicaCount: 1

image:
  repository: ghcr.io/yourorg/provider-file
  pullPolicy: IfNotPresent
  tag: "0.1.0"

nameOverride: ""
fullnameOverride: ""

serviceAccount:
  create: true
  annotations: {}
  name: ""

podAnnotations: {}

podSecurityContext:
  fsGroup: 2000
  runAsNonRoot: true
  runAsUser: 1000

securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop:
    - ALL
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 1000

service:
  type: ClusterIP
  port: 9443
  healthPort: 8080

resources:
  limits:
    cpu: 500m
    memory: 512Mi
  requests:
    cpu: 100m
    memory: 128Mi

nodeSelector: {}

tolerations: []

affinity: {}

# Provider-specific configuration
provider:
  storageDir: "/var/lib/virtrigaud/vms"
  logLevel: "info"

# Persistent storage for VM data
persistence:
  enabled: true
  accessMode: ReadWriteOnce
  size: 10Gi
  storageClass: ""
```

### 5.3 Test Helm Chart

```bash
# Lint the chart
helm lint charts/provider-file/

# Template the chart
helm template provider-file charts/provider-file/ \
  --values charts/provider-file/values.yaml

# Install to local cluster
helm install provider-file charts/provider-file/ \
  --namespace provider-file \
  --create-namespace \
  --values charts/provider-file/examples/values-development.yaml
```

## Step 6: Set Up CI/CD

### 6.1 GitHub Actions Workflow

The generated `.github/workflows/ci.yml` includes:

```yaml
name: CI

on:
  push:
    branches: [ main, develop ]
  pull_request:
    branches: [ main, develop ]

env:
  GO_VERSION: '1.23'

jobs:
  test:
    name: Test
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v4
    
    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{ env.GO_VERSION }}
    
    - name: Run tests
      run: make test

    - name: Run linting
      run: make lint

  build:
    name: Build
    runs-on: ubuntu-latest
    needs: test
    steps:
    - uses: actions/checkout@v4
    
    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{ env.GO_VERSION }}
    
    - name: Build binary
      run: make build

    - name: Build Docker image
      run: make docker-build

  conformance:
    name: Conformance Tests
    runs-on: ubuntu-latest
    needs: build
    steps:
    - uses: actions/checkout@v4
    
    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{ env.GO_VERSION }}
    
    - name: Build provider
      run: make build

    - name: Install VCTS
      run: go install github.com/projectbeskar/virtrigaud/cmd/vcts@latest

    - name: Run conformance tests
      run: |
        # Start provider in background
        PROVIDER_STORAGE_DIR=/tmp/vcts-test ./bin/provider-file &
        PROVIDER_PID=$!
        
        # Wait for startup
        sleep 5
        
        # Run VCTS
        vcts run --config vcts-config.yaml --profile core
        
        # Clean up
        kill $PROVIDER_PID

  release:
    name: Release
    runs-on: ubuntu-latest
    needs: [test, build, conformance]
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    steps:
    - uses: actions/checkout@v4
    
    - name: Build and push Docker image
      run: |
        echo ${{ secrets.GITHUB_TOKEN }} | docker login ghcr.io -u ${{ github.actor }} --password-stdin
        make docker-push

    - name: Package Helm chart
      run: |
        helm package charts/provider-file/ -d dist/
        
    - name: Upload artifacts
      uses: actions/upload-artifact@v4
      with:
        name: release-artifacts
        path: |
          bin/
          dist/
```

## Step 7: Publish to Provider Catalog

### 7.1 Run Provider Verification

```bash
# Verify the provider meets all requirements
vrtg-provider verify --profile all
```

### 7.2 Publish to Catalog

```bash
# Publish to the VirtRigaud provider catalog
vrtg-provider publish \
  --name file \
  --image ghcr.io/yourorg/provider-file \
  --tag 0.1.0 \
  --repo https://github.com/yourorg/virtrigaud-provider-file \
  --maintainer your-email@example.com \
  --license Apache-2.0
```

This command will:
1. Run VCTS conformance tests
2. Generate a provider badge
3. Create a catalog entry
4. Open a pull request to the main VirtRigaud repository

### 7.3 Example Catalog Entry

The generated catalog entry will look like:

```yaml
- name: file
  displayName: "File Provider"
  description: "File-based virtual machine provider for development and testing"
  repo: "https://github.com/yourorg/virtrigaud-provider-file"
  image: "ghcr.io/yourorg/provider-file"
  tag: "0.1.0"
  capabilities:
    - core
    - snapshot
    - clone
  conformance:
    profiles:
      core: pass
      snapshot: pass
      clone: pass
      image-prepare: skip
      advanced: skip
    report_url: "https://github.com/yourorg/virtrigaud-provider-file/actions"
    badge_url: "https://img.shields.io/badge/conformance-pass-green"
    last_tested: "2025-08-26T15:00:00Z"
  maintainer: "your-email@example.com"
  license: "Apache-2.0"
  maturity: "beta"
  tags:
    - file
    - development
    - testing
  documentation: "https://github.com/yourorg/virtrigaud-provider-file/blob/main/README.md"
```

## Step 8: Production Considerations

### 8.1 Security Hardening

```yaml
# Production values.yaml
securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop:
    - ALL
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 65534

podSecurityContext:
  fsGroup: 65534
  runAsNonRoot: true
  runAsUser: 65534
  seccompProfile:
    type: RuntimeDefault

networkPolicy:
  enabled: true
  ingress:
    fromNamespaces:
      - virtrigaud-system
  egress:
    - to: []
      ports:
        - protocol: UDP
          port: 53
```

### 8.2 Observability

Add monitoring and logging:

```go
// Add to provider.go
import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    vmOperations = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "file_provider_vm_operations_total",
            Help: "Total number of VM operations",
        },
        []string{"operation", "status"},
    )
    
    vmOperationDuration = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Name: "file_provider_vm_operation_duration_seconds",
            Help: "Duration of VM operations",
        },
        []string{"operation"},
    )
)

func (p *Provider) CreateVM(ctx context.Context, req *providerv1.CreateVMRequest) (*providerv1.CreateVMResponse, error) {
    start := time.Now()
    defer func() {
        vmOperationDuration.WithLabelValues("create").Observe(time.Since(start).Seconds())
    }()
    
    // ... existing implementation ...
    
    vmOperations.WithLabelValues("create", "success").Inc()
    return resp, nil
}
```

### 8.3 Performance Optimization

- Add connection pooling for gRPC clients
- Implement caching for frequently accessed VMs
- Use background workers for long-running operations
- Add rate limiting and request validation

### 8.4 Error Handling and Resilience

- Implement circuit breakers for external dependencies
- Add retry logic with exponential backoff
- Use structured logging with correlation IDs
- Implement graceful shutdown handling

## Conclusion

You've successfully created a complete VirtRigaud provider! This tutorial covered:

✅ **Provider Implementation** - Full gRPC service with all core operations  
✅ **SDK Integration** - Using VirtRigaud SDK for server setup and utilities  
✅ **Testing** - Unit tests and VCTS conformance validation  
✅ **Containerization** - Docker images and Helm charts  
✅ **CI/CD** - Automated testing and publishing  
✅ **Catalog Integration** - Publishing to the provider ecosystem  

## Next Steps

1. **Explore Advanced Features**:
   - Add image management capabilities
   - Implement networking configuration
   - Add storage volume management

2. **Integration Examples**:
   - Connect to real hypervisors (libvirt, vSphere, etc.)
   - Add authentication and authorization
   - Implement backup and disaster recovery

3. **Community Contribution**:
   - Submit your provider to the catalog
   - Contribute improvements to the SDK
   - Help other developers with provider development

4. **Production Deployment**:
   - Set up monitoring and alerting
   - Implement proper security measures
   - Plan for scaling and high availability

For more information, visit the [VirtRigaud documentation](https://projectbeskar.github.io/virtrigaud/) or join our community discussions.

