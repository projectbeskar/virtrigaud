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

// Package mock provides a mock provider implementation for testing and demos.
package mock

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Provider implements a mock provider for testing and demos.
type Provider struct {
	providerv1.UnimplementedProviderServer
	mu           sync.RWMutex
	vms          map[string]*VirtualMachine
	tasks        map[string]*Task
	capabilities *capabilities.Manager
	failureMode  string
	slowMode     bool
}

// VirtualMachine represents a mock virtual machine.
type VirtualMachine struct {
	ID          string
	Name        string
	PowerState  string
	IPs         []string
	ConsoleURL  string
	Created     time.Time
	LastUpdated time.Time
	Snapshots   map[string]*Snapshot
}

// Snapshot represents a mock VM snapshot.
type Snapshot struct {
	ID           string
	Name         string
	CreatedTime  time.Time
	Description  string
	SizeBytes    int64
	HasMemory    bool
}

// Task represents an async operation.
type Task struct {
	ID        string
	Done      bool
	Error     string
	Created   time.Time
	Completed time.Time
}

// NewProvider creates a new mock provider.
func NewProvider() *Provider {
	// Build comprehensive capabilities for mock provider
	caps := capabilities.NewBuilder().
		Core().
		Mock().
		Snapshots().
		MemorySnapshots().
		LinkedClones().
		OnlineReconfigure().
		OnlineDiskExpansion().
		ImageImport().
		TaskStatus().
		DiskTypes("thin", "thick", "raw", "qcow2").
		NetworkTypes("bridge", "nat", "distributed").
		Build()

	provider := &Provider{
		vms:          make(map[string]*VirtualMachine),
		tasks:        make(map[string]*Task),
		capabilities: caps,
		failureMode:  os.Getenv("MOCK_FAILURE_MODE"),
		slowMode:     os.Getenv("MOCK_SLOW_MODE") == "true",
	}

	// Create some sample VMs for demos
	provider.createSampleVMs()

	return provider
}

// createSampleVMs creates some sample VMs for demonstration.
func (p *Provider) createSampleVMs() {
	sampleVMs := []struct {
		name       string
		powerState string
		ips        []string
	}{
		{"demo-vm-1", "On", []string{"192.168.1.10"}},
		{"demo-vm-2", "Off", []string{}},
		{"demo-vm-3", "On", []string{"192.168.1.12", "10.0.0.5"}},
	}

	for i, vm := range sampleVMs {
		id := fmt.Sprintf("vm-%d", i+1)
		p.vms[id] = &VirtualMachine{
			ID:          id,
			Name:        vm.name,
			PowerState:  vm.powerState,
			IPs:         vm.ips,
			ConsoleURL:  fmt.Sprintf("https://console.example.com/vm/%s", id),
			Created:     time.Now().Add(-time.Duration(i+1) * time.Hour),
			LastUpdated: time.Now(),
			Snapshots:   make(map[string]*Snapshot),
		}
	}
}

// Validate validates the mock provider configuration.
func (p *Provider) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	p.simulateDelay()

	if p.shouldFail("validate") {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: "Mock provider configured to fail validation",
		}, nil
	}

	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: "Mock provider is ready",
	}, nil
}

// Create creates a new virtual machine.
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	p.simulateDelay()

	if p.shouldFail("create") {
		return nil, errors.NewInternal("mock provider configured to fail create operations", nil)
	}

	// Generate VM ID
	id := p.generateID("vm")

	// Create VM
	vm := &VirtualMachine{
		ID:          id,
		Name:        req.Name,
		PowerState:  "Off", // Start powered off
		IPs:         []string{},
		ConsoleURL:  fmt.Sprintf("https://console.example.com/vm/%s", id),
		Created:     time.Now(),
		LastUpdated: time.Now(),
		Snapshots:   make(map[string]*Snapshot),
	}

	p.mu.Lock()
	p.vms[id] = vm
	p.mu.Unlock()

	// Create async task for the operation
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Complete the task after a delay
	go p.completeTaskAfterDelay(taskID, 2*time.Second)

	return &providerv1.CreateResponse{
		Id: id,
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// Delete deletes a virtual machine.
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	p.simulateDelay()

	if p.shouldFail("delete") {
		return nil, errors.NewInternal("mock provider configured to fail delete operations", nil)
	}

	p.mu.RLock()
	_, exists := p.vms[req.Id]
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.Id)
	}

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Delete VM after delay
	go func() {
		time.Sleep(1 * time.Second)
		p.mu.Lock()
		delete(p.vms, req.Id)
		task.Done = true
		task.Completed = time.Now()
		p.mu.Unlock()
	}()

	return &providerv1.TaskResponse{
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// Power performs power operations on a virtual machine.
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	p.simulateDelay()

	if p.shouldFail("power") {
		return nil, errors.NewInternal("mock provider configured to fail power operations", nil)
	}

	p.mu.RLock()
	vm, exists := p.vms[req.Id]
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.Id)
	}

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Update power state after delay
	go func() {
		time.Sleep(1 * time.Second)
		
		var newState string
		switch req.Op {
		case providerv1.PowerOp_POWER_OP_ON:
			newState = "On"
			// Assign IP when powering on
			if len(vm.IPs) == 0 {
				vm.IPs = []string{fmt.Sprintf("192.168.1.%d", 100+rand.Intn(50))}
			}
		case providerv1.PowerOp_POWER_OP_OFF:
			newState = "Off"
			// Clear IPs when powering off
			vm.IPs = []string{}
		case providerv1.PowerOp_POWER_OP_REBOOT:
			newState = "On"
		}

		p.mu.Lock()
		vm.PowerState = newState
		vm.LastUpdated = time.Now()
		task.Done = true
		task.Completed = time.Now()
		p.mu.Unlock()
	}()

	return &providerv1.TaskResponse{
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// Reconfigure reconfigures a virtual machine.
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	p.simulateDelay()

	if p.shouldFail("reconfigure") {
		return nil, errors.NewInternal("mock provider configured to fail reconfigure operations", nil)
	}

	p.mu.RLock()
	_, exists := p.vms[req.Id]
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.Id)
	}

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Complete reconfiguration after delay
	go p.completeTaskAfterDelay(taskID, 3*time.Second)

	return &providerv1.TaskResponse{
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// Describe describes a virtual machine's current state.
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	p.simulateDelay()

	if p.shouldFail("describe") {
		return nil, errors.NewInternal("mock provider configured to fail describe operations", nil)
	}

	p.mu.RLock()
	vm, exists := p.vms[req.Id]
	p.mu.RUnlock()

	if !exists {
		return &providerv1.DescribeResponse{
			Exists: false,
		}, nil
	}

	return &providerv1.DescribeResponse{
		Exists:     true,
		PowerState: vm.PowerState,
		Ips:        vm.IPs,
		ConsoleUrl: vm.ConsoleURL,
		ProviderRawJson: fmt.Sprintf(`{"id":"%s","name":"%s","created":"%s","lastUpdated":"%s"}`,
			vm.ID, vm.Name, vm.Created.Format(time.RFC3339), vm.LastUpdated.Format(time.RFC3339)),
	}, nil
}

// TaskStatus checks the status of an async task.
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	p.simulateDelay()

	if p.shouldFail("task_status") {
		return nil, errors.NewInternal("mock provider configured to fail task status operations", nil)
	}

	p.mu.RLock()
	task, exists := p.tasks[req.Task.Id]
	p.mu.RUnlock()

	if !exists {
		return &providerv1.TaskStatusResponse{
			Done:  true,
			Error: "task not found",
		}, nil
	}

	return &providerv1.TaskStatusResponse{
		Done:  task.Done,
		Error: task.Error,
	}, nil
}

// SnapshotCreate creates a VM snapshot.
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	p.simulateDelay()

	if p.shouldFail("snapshot_create") {
		return nil, errors.NewInternal("mock provider configured to fail snapshot operations", nil)
	}

	p.mu.RLock()
	vm, exists := p.vms[req.VmId]
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.VmId)
	}

	// Generate snapshot ID
	snapshotID := p.generateID("snap")

	// Create snapshot
	snapshot := &Snapshot{
		ID:          snapshotID,
		Name:        req.NameHint,
		CreatedTime: time.Now(),
		Description: req.Description,
		SizeBytes:   int64(rand.Intn(1000000000) + 100000000), // 100MB to 1GB
		HasMemory:   req.IncludeMemory,
	}

	p.mu.Lock()
	vm.Snapshots[snapshotID] = snapshot
	p.mu.Unlock()

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Complete snapshot creation after delay
	go p.completeTaskAfterDelay(taskID, 5*time.Second)

	return &providerv1.SnapshotCreateResponse{
		SnapshotId: snapshotID,
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// SnapshotDelete deletes a VM snapshot.
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	p.simulateDelay()

	if p.shouldFail("snapshot_delete") {
		return nil, errors.NewInternal("mock provider configured to fail snapshot operations", nil)
	}

	p.mu.RLock()
	vm, exists := p.vms[req.VmId]
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.VmId)
	}

	p.mu.RLock()
	_, snapshotExists := vm.Snapshots[req.SnapshotId]
	p.mu.RUnlock()

	if !snapshotExists {
		return nil, errors.NewNotFound("Snapshot", req.SnapshotId)
	}

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Delete snapshot after delay
	go func() {
		time.Sleep(2 * time.Second)
		p.mu.Lock()
		delete(vm.Snapshots, req.SnapshotId)
		task.Done = true
		task.Completed = time.Now()
		p.mu.Unlock()
	}()

	return &providerv1.TaskResponse{
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// SnapshotRevert reverts a VM to a snapshot.
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	p.simulateDelay()

	if p.shouldFail("snapshot_revert") {
		return nil, errors.NewInternal("mock provider configured to fail snapshot operations", nil)
	}

	p.mu.RLock()
	vm, exists := p.vms[req.VmId]
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.VmId)
	}

	p.mu.RLock()
	_, snapshotExists := vm.Snapshots[req.SnapshotId]
	p.mu.RUnlock()

	if !snapshotExists {
		return nil, errors.NewNotFound("Snapshot", req.SnapshotId)
	}

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Complete revert after delay
	go p.completeTaskAfterDelay(taskID, 4*time.Second)

	return &providerv1.TaskResponse{
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// Clone clones a virtual machine.
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	p.simulateDelay()

	if p.shouldFail("clone") {
		return nil, errors.NewInternal("mock provider configured to fail clone operations", nil)
	}

	p.mu.RLock()
	_, exists := p.vms[req.SourceVmId]
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.SourceVmId)
	}

	// Generate target VM ID
	targetID := p.generateID("vm")

	// Create cloned VM
	clonedVM := &VirtualMachine{
		ID:          targetID,
		Name:        req.TargetName,
		PowerState:  "Off",
		IPs:         []string{},
		ConsoleURL:  fmt.Sprintf("https://console.example.com/vm/%s", targetID),
		Created:     time.Now(),
		LastUpdated: time.Now(),
		Snapshots:   make(map[string]*Snapshot),
	}

	p.mu.Lock()
	p.vms[targetID] = clonedVM
	p.mu.Unlock()

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Complete clone after delay
	go p.completeTaskAfterDelay(taskID, 10*time.Second)

	return &providerv1.CloneResponse{
		TargetVmId: targetID,
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// ImagePrepare prepares an image for use.
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	p.simulateDelay()

	if p.shouldFail("image_prepare") {
		return nil, errors.NewInternal("mock provider configured to fail image operations", nil)
	}

	// Create async task
	taskID := p.generateID("task")
	task := &Task{
		ID:      taskID,
		Done:    false,
		Created: time.Now(),
	}

	p.mu.Lock()
	p.tasks[taskID] = task
	p.mu.Unlock()

	// Complete image preparation after delay
	go p.completeTaskAfterDelay(taskID, 15*time.Second)

	return &providerv1.TaskResponse{
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
	}, nil
}

// GetCapabilities returns the provider's capabilities.
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return p.capabilities.GetCapabilities(ctx, req)
}

// Helper methods

// generateID generates a unique ID with the given prefix.
func (p *Provider) generateID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().Unix(), rand.Intn(10000))
}

// completeTaskAfterDelay completes a task after the specified delay.
func (p *Provider) completeTaskAfterDelay(taskID string, delay time.Duration) {
	time.Sleep(delay)
	p.mu.Lock()
	if task, exists := p.tasks[taskID]; exists {
		task.Done = true
		task.Completed = time.Now()
	}
	p.mu.Unlock()
}

// shouldFail checks if the provider should fail for the given operation.
func (p *Provider) shouldFail(operation string) bool {
	if p.failureMode == "" {
		return false
	}

	// Support specific operation failures and "all" failures
	return p.failureMode == operation || p.failureMode == "all"
}

// simulateDelay simulates network/processing delay if slow mode is enabled.
func (p *Provider) simulateDelay() {
	if p.slowMode {
		delay := time.Duration(rand.Intn(500)+100) * time.Millisecond
		time.Sleep(delay)
	}
}
