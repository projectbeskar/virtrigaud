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
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
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

	// images is the mock's single in-memory image location: the prepared-image
	// artifacts by name (ADR-0009). Guarded by mu.
	images map[string]*preparedImage
	// imagePrepareDelay is how long an image import takes; 0 completes it
	// synchronously (no task).
	imagePrepareDelay time.Duration
	// logger receives the provider's log lines (slog.Default() when unset).
	logger *slog.Logger
	// idSeq makes generated IDs unique within the process.
	idSeq atomic.Uint64
}

// Option configures a Provider built by NewProvider.
type Option func(*Provider)

// WithImagePrepareDelay sets how long an image import takes (default 15s). A
// positive delay makes ImagePrepare asynchronous (it returns a task, like the
// vSphere provider); 0 makes it complete within the call (no task, like the
// libvirt provider).
func WithImagePrepareDelay(d time.Duration) Option {
	return func(p *Provider) { p.imagePrepareDelay = d }
}

// WithLogger sets the provider's logger (default slog.Default()).
func WithLogger(logger *slog.Logger) Option {
	return func(p *Provider) { p.logger = logger }
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
	// Owner is the VM's owner stamp: the VirtualMachine it was made for
	// (CreateRequest.owner, CloneRequest.target_vm, when they carry a UID;
	// rc2 R4) or set by TransferOwner (zero when unstamped); ListVMs reports
	// its UID as provider_raw "owner_uid".
	Owner contracts.ObjectIdentity
}

// Snapshot represents a mock VM snapshot.
type Snapshot struct {
	ID          string
	Name        string
	CreatedTime time.Time
	Description string
	SizeBytes   int64
	HasMemory   bool
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
func NewProvider(opts ...Option) *Provider {
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
		// ADR-0009 D7: ImagePrepare names, stamps and verifies artifacts by
		// the VMImage identity and source digest (image.go).
		ImageArtifactIdentity().
		// TransferOwner stamps a VM adoption takes over (transfer_owner.go).
		OwnerTransfer().
		// Create and Clone stamp the VM with its owner uid and return the VM
		// an earlier attempt made for it (rc2 R4).
		OwnerRebind().
		TaskStatus().
		// ADR-0006 Slice 0: advertise the status quo honestly. The mock's
		// migration path (like the production providers) is pod-side only —
		// pvc staging, relay-shaped; nfs/s3 and direct transfer are not
		// implemented.
		ExportBackends(migration.PVCOnlyExportBackends()...).
		ImportBackends(migration.PVCOnlyImportBackends()...).
		TransferModes(migration.RelayOnlyTransferModes()...).
		DiskTypes("thin", "thick", "raw", "qcow2").
		NetworkTypes("bridge", "nat", "distributed").
		Build()

	provider := &Provider{
		vms:               make(map[string]*VirtualMachine),
		tasks:             make(map[string]*Task),
		capabilities:      caps,
		failureMode:       os.Getenv("MOCK_FAILURE_MODE"),
		slowMode:          os.Getenv("MOCK_SLOW_MODE") == "true",
		images:            make(map[string]*preparedImage),
		imagePrepareDelay: defaultImagePrepareDelay,
		logger:            slog.Default(),
	}
	for _, opt := range opts {
		opt(provider)
	}
	if provider.logger == nil {
		provider.logger = slog.Default()
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

	// As on the real providers (rc2 R4, supports_owner_rebind), a Create sent
	// again for the same owner uid returns the VM an earlier attempt made
	// (synchronously, no task) instead of a second one; the check and the new
	// VM happen under one lock.
	owner := stampOf(req.GetOwner())
	p.mu.Lock()
	if owner.UID != "" {
		for vmID, existing := range p.vms {
			if existing.Name == req.Name && existing.Owner.UID == owner.UID {
				p.mu.Unlock()
				return &providerv1.CreateResponse{Id: vmID}, nil
			}
		}
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
		Owner:       owner,
	}
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

// Mock VM power states (VirtualMachine.PowerState).
const (
	// mockPowerStateOn is a running mock VM.
	mockPowerStateOn = "On"
	// mockPowerStateOff is a stopped mock VM.
	mockPowerStateOff = "Off"
)

// mockRequestedPowerState is the power state op leaves a VM in — On for
// POWER_OP_ON, Off for POWER_OP_OFF and POWER_OP_SHUTDOWN_GRACEFUL — or ""
// for an op (reboot) that no state satisfies.
func mockRequestedPowerState(op providerv1.PowerOp) string {
	switch op {
	case providerv1.PowerOp_POWER_OP_ON:
		return mockPowerStateOn
	case providerv1.PowerOp_POWER_OP_OFF, providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
		return mockPowerStateOff
	default:
		return ""
	}
}

// Power performs power operations on a virtual machine.
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	p.simulateDelay()

	if p.shouldFail("power") {
		return nil, errors.NewInternal("mock provider configured to fail power operations", nil)
	}

	p.mu.RLock()
	vm, exists := p.vms[req.Id]
	var current string
	if exists {
		current = vm.PowerState
	}
	p.mu.RUnlock()

	if !exists {
		return nil, errors.NewNotFound("VirtualMachine", req.Id)
	}

	// Power is idempotent, as on the real providers: a VM already in the
	// requested state is success, with no task.
	if want := mockRequestedPowerState(req.Op); want != "" && current == want {
		return &providerv1.TaskResponse{}, nil
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
			newState = mockPowerStateOn
			// Assign IP when powering on
			if len(vm.IPs) == 0 {
				vm.IPs = []string{fmt.Sprintf("192.168.1.%d", 100+rand.Intn(50))}
			}
		case providerv1.PowerOp_POWER_OP_OFF:
			newState = mockPowerStateOff
			// Clear IPs when powering off
			vm.IPs = []string{}
		case providerv1.PowerOp_POWER_OP_REBOOT:
			newState = mockPowerStateOn
		case providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
			// Mock graceful shutdown - same as regular Off but with a slight delay
			newState = mockPowerStateOff
			// Clear IPs when shutting down
			vm.IPs = []string{}
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

	// The mock applies every change in the task and never leaves one pending a
	// restart; a provider that answers with a task reports RestartRequired false.
	return &providerv1.TaskResponse{
		Task: &providerv1.TaskRef{
			Id: taskID,
		},
		RestartRequired: false,
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

// ListVMs returns all VMs managed by this provider. The owner filter
// (req.owner_namespace / owner_name, ADR-0007 A6.2) is not implemented: the
// mock provider is not clustered, does not advertise
// supports_list_owner_filter, ignores the filter and never sets
// owner_filter_applied, so a caller never mistakes its answer for a filtered
// one (D7, honesty-first).
func (p *Provider) ListVMs(ctx context.Context, req *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
	p.simulateDelay()

	p.mu.RLock()
	defer p.mu.RUnlock()

	var vmInfos []*providerv1.VMInfo

	for _, vm := range p.vms {
		// Create sample disk info
		disks := []*providerv1.DiskInfo{
			{
				Id:      fmt.Sprintf("%s-disk-0", vm.ID),
				Path:    fmt.Sprintf("/var/lib/libvirt/images/%s.qcow2", vm.ID),
				SizeGib: 20,
				Format:  "qcow2",
			},
		}

		// Create sample network info
		networks := []*providerv1.NetworkInfo{
			{
				Name:      "default",
				Mac:       fmt.Sprintf("52:54:00:%02x:%02x:%02x", rand.Intn(256), rand.Intn(256), rand.Intn(256)),
				IpAddress: "",
			},
		}
		if len(vm.IPs) > 0 {
			networks[0].IpAddress = vm.IPs[0]
		}

		// Build provider raw metadata
		providerRaw := make(map[string]string)
		providerRaw["vm_id"] = vm.ID
		providerRaw["power_state"] = vm.PowerState
		providerRaw["console_url"] = vm.ConsoleURL
		if vm.Owner.UID != "" {
			providerRaw[contracts.VMInfoOwnerUIDKey] = vm.Owner.UID
		}

		vmInfo := &providerv1.VMInfo{
			Id:          vm.ID,
			Name:        vm.Name,
			PowerState:  vm.PowerState,
			Ips:         vm.IPs,
			Cpu:         2,    // Default CPU
			MemoryMib:   4096, // Default 4 GiB
			Disks:       disks,
			Networks:    networks,
			ProviderRaw: providerRaw,
		}

		vmInfos = append(vmInfos, vmInfo)
	}

	return &providerv1.ListVMsResponse{
		Vms: vmInfos,
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

// Clone clones a virtual machine. As on the real providers (rc2 R4), a request
// that names its target VirtualMachine's UID (target_vm.uid) stamps the clone
// with it, and a re-sent Clone returns the clone stamped with that UID under
// the target name (synchronously, no task) instead of making a second one; any
// other VM of the target name is refused with AlreadyExists. A request
// without a UID clones as before.
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	p.simulateDelay()

	if p.shouldFail("clone") {
		return nil, errors.NewInternal("mock provider configured to fail clone operations", nil)
	}

	owner := stampOf(req.GetTargetVm())

	// The check, the re-bind and the new clone happen under one lock, so two
	// attempts of one Clone never both clone.
	p.mu.Lock()
	if _, exists := p.vms[req.SourceVmId]; !exists {
		p.mu.Unlock()
		return nil, errors.NewNotFound("VirtualMachine", req.SourceVmId)
	}
	if owner.UID != "" {
		if existing, err := p.cloneTargetLocked(req.TargetName, owner.UID); err != nil || existing != "" {
			p.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return &providerv1.CloneResponse{TargetVmId: existing}, nil
		}
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
		Owner:       owner,
	}
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

// stampOf is the owner stamp a Create or Clone request asks for: o with a UID,
// or the zero identity (no stamp) for a request without one.
func stampOf(o *providerv1.ObjectIdentity) contracts.ObjectIdentity {
	if o.GetUid() == "" {
		return contracts.ObjectIdentity{}
	}
	return contracts.ObjectIdentity{UID: o.GetUid(), Namespace: o.GetNamespace(), Name: o.GetName()}
}

// cloneTargetLocked decides a stamped Clone's target name; p.mu is held. It
// returns the id of the one VM named name stamped with ownerUID (a re-sent
// Clone), "" when no VM has that name, or AlreadyExists when a VM of that
// name is not (only) the target's.
func (p *Provider) cloneTargetLocked(name, ownerUID string) (string, error) {
	var owned string
	for id, vm := range p.vms {
		if vm.Name != name {
			continue
		}
		if vm.Owner.UID != ownerUID || owned != "" {
			return "", errors.NewAlreadyExists("VirtualMachine", name)
		}
		owned = id
	}
	return owned, nil
}

// GetCapabilities returns the provider's capabilities.
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return p.capabilities.GetCapabilities(ctx, req)
}

// DeleteStagedDisk answers Unimplemented (ADR-0011): the mock stages migration
// disks only to pvc (it advertises no nfs or s3 export or import backend), so it
// never writes or reads an nfs / s3 staging object. It validates the request
// first, with the same confinement as the real providers, so a malformed
// request is InvalidArgument here too. It advertises no
// supported_staged_disk_delete_backends, so the manager never asks it.
func (p *Provider) DeleteStagedDisk(_ context.Context, req *providerv1.DeleteStagedDiskRequest) (*providerv1.DeleteStagedDiskResponse, error) {
	if _, err := migration.ResolveStagedDisk(req); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unimplemented, "the mock provider stages no nfs or s3 objects (pvc only)")
}

// Helper methods

// generateID generates a unique ID with the given prefix. A process-wide
// sequence number (not a random suffix) keeps two IDs generated in the same
// second distinct.
func (p *Provider) generateID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().Unix(), p.idSeq.Add(1))
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

// FinishTask marks the asynchronous task taskID done now — failed with errMsg
// when it is non-empty — as if its operation had just finished, and reports
// whether the task exists. It exists for tests and demos that drive an
// asynchronous operation (for example an image import, WithImagePrepareDelay)
// to completion deterministically instead of waiting for its delay.
func (p *Provider) FinishTask(taskID, errMsg string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	task, exists := p.tasks[taskID]
	if !exists {
		return false
	}
	task.Done = true
	task.Error = errMsg
	task.Completed = time.Now()
	return true
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
