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

package vsphere

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectbeskar/virtrigaud/api/v1alpha1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Provider implements the contracts.Provider interface for vSphere
type Provider struct {
	// provider configuration
	config *v1alpha1.Provider

	// Kubernetes client for reading secrets
	k8sClient client.Client

	// vSphere connection
	client *govmomi.Client
	finder *find.Finder

	// cached credentials
	credentials *Credentials
}

// Credentials holds vSphere authentication information
type Credentials struct {
	Username string
	Password string
	Token    string
}

// NewProvider creates a new vSphere provider instance
func NewProvider(ctx context.Context, k8sClient client.Client, provider *v1alpha1.Provider) (contracts.Provider, error) {
	if provider.Spec.Type != "vsphere" {
		return nil, contracts.NewInvalidSpecError(fmt.Sprintf("invalid provider type: %s, expected vsphere", provider.Spec.Type), nil)
	}

	p := &Provider{
		config:    provider,
		k8sClient: k8sClient,
	}

	// Load credentials
	if err := p.loadCredentials(ctx); err != nil {
		return nil, contracts.NewUnauthorizedError("failed to load credentials", err)
	}

	// Initialize vSphere client
	if err := p.connect(ctx); err != nil {
		return nil, contracts.NewRetryableError("failed to connect to vSphere", err)
	}

	return p, nil
}

// Validate ensures the provider session/credentials are healthy
func (p *Provider) Validate(ctx context.Context) error {
	if p.client == nil {
		return contracts.NewRetryableError("vSphere client not initialized", nil)
	}

	// Test the connection by checking if the session is valid
	if !p.client.Valid() {
		// Try to reconnect
		if err := p.connect(ctx); err != nil {
			return contracts.NewRetryableError("failed to validate vSphere connection", err)
		}
	}

	return nil
}

// Create creates a new VM if it doesn't exist (idempotent)
func (p *Provider) Create(ctx context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	// Check if VM already exists
	vm, err := p.findVM(ctx, req.Name)
	if err == nil && vm != nil {
		// VM exists, return its ID
		return contracts.CreateResponse{
			ID: vm.Reference().Value,
		}, nil
	}

	// Create the VM
	taskRef, vmID, err := p.createVM(ctx, req)
	if err != nil {
		return contracts.CreateResponse{}, err
	}

	return contracts.CreateResponse{
		ID:      vmID,
		TaskRef: taskRef,
	}, nil
}

// Delete removes a VM (idempotent, succeeds even if VM doesn't exist)
func (p *Provider) Delete(ctx context.Context, id string) (taskRef string, err error) {
	vm, err := p.findVMByID(ctx, id)
	if err != nil {
		// VM not found, consider it already deleted
		return "", nil
	}

	// Power off the VM if it's running
	powerState, err := vm.PowerState(ctx)
	if err != nil {
		return "", contracts.NewRetryableError("failed to get VM power state", err)
	}

	if powerState == "poweredOn" {
		task, err := vm.PowerOff(ctx)
		if err != nil {
			return "", contracts.NewRetryableError("failed to power off VM", err)
		}
		// Wait for power off to complete
		if err := task.Wait(ctx); err != nil {
			return "", contracts.NewRetryableError("failed to wait for power off", err)
		}
	}

	// Delete the VM
	task, err := vm.Destroy(ctx)
	if err != nil {
		return "", contracts.NewRetryableError("failed to delete VM", err)
	}

	return task.Reference().Value, nil
}

// Power performs a power operation on the VM
func (p *Provider) Power(ctx context.Context, id string, op contracts.PowerOp) (taskRef string, err error) {
	vm, err := p.findVMByID(ctx, id)
	if err != nil {
		return "", contracts.NewNotFoundError("VM not found", err)
	}

	var task *object.Task
	switch op {
	case contracts.PowerOpOn:
		task, err = vm.PowerOn(ctx)
	case contracts.PowerOpOff:
		task, err = vm.PowerOff(ctx)
	case contracts.PowerOpReboot:
		// For reboot, we need guest tools
		err = vm.RebootGuest(ctx)
		if err != nil {
			// Fallback to hard reset
			task, err = vm.Reset(ctx)
		}
	default:
		return "", contracts.NewInvalidSpecError(fmt.Sprintf("unsupported power operation: %s", op), nil)
	}

	if err != nil {
		return "", contracts.NewRetryableError(fmt.Sprintf("failed to perform power operation %s", op), err)
	}

	if task != nil {
		return task.Reference().Value, nil
	}

	return "", nil
}

// Reconfigure modifies VM resources (CPU/RAM/Disks)
func (p *Provider) Reconfigure(ctx context.Context, id string, desired contracts.CreateRequest) (taskRef string, err error) {
	// For MVP, we'll implement basic CPU/RAM reconfiguration
	vm, err := p.findVMByID(ctx, id)
	if err != nil {
		return "", contracts.NewNotFoundError("VM not found", err)
	}

	spec := p.buildReconfigSpec(desired)
	if spec == nil {
		// No changes needed
		return "", nil
	}

	task, err := vm.Reconfigure(ctx, *spec)
	if err != nil {
		return "", contracts.NewRetryableError("failed to reconfigure VM", err)
	}

	return task.Reference().Value, nil
}

// Describe returns the current state of the VM
func (p *Provider) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	vm, err := p.findVMByID(ctx, id)
	if err != nil {
		return contracts.DescribeResponse{
			Exists: false,
		}, nil
	}

	return p.describeVM(ctx, vm)
}

// IsTaskComplete checks if an async task is complete
func (p *Provider) IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error) {
	task, err := p.getTask(ctx, taskRef)
	if err != nil {
		return false, contracts.NewRetryableError("failed to get task", err)
	}

	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		// Check if task is still running
		if soap.IsSoapFault(err) {
			// Task might still be running
			return false, nil
		}
		return false, contracts.NewRetryableError("task failed", err)
	}

	return info.State == "success" || info.State == "error", nil
}
