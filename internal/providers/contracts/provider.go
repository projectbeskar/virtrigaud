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

package contracts

import (
	"context"
)

// PowerOp represents a power operation type
type PowerOp string

const (
	// PowerOpOn powers on the VM
	PowerOpOn PowerOp = "On"
	// PowerOpOff powers off the VM
	PowerOpOff PowerOp = "Off"
	// PowerOpReboot reboots the VM
	PowerOpReboot PowerOp = "Reboot"
)

// CreateRequest contains all information needed to create a VM
type CreateRequest struct {
	// Name of the VM to create
	Name string
	// Class defines the VM resource allocation
	Class VMClass
	// Image defines the base template/image
	Image VMImage
	// Networks defines network attachments
	Networks []NetworkAttachment
	// Disks defines additional disks
	Disks []DiskSpec
	// UserData contains cloud-init/ignition configuration
	UserData *UserData
	// Placement provides placement hints
	Placement *Placement
	// Tags are applied to the VM
	Tags []string
}

// CreateResponse contains the result of a create operation
type CreateResponse struct {
	// ID is the provider-specific identifier
	ID string
	// TaskRef references an async operation if applicable
	TaskRef string
}

// DescribeResponse contains the current state of a VM
type DescribeResponse struct {
	// Exists indicates if the VM exists
	Exists bool
	// PowerState is the current power state
	PowerState string
	// IPs contains assigned IP addresses
	IPs []string
	// ConsoleURL provides console access
	ConsoleURL string
	// ProviderRaw contains provider-specific details
	ProviderRaw map[string]string
}

// Provider defines the interface that all providers must implement
type Provider interface {
	// Validate ensures the provider session/credentials are healthy
	Validate(ctx context.Context) error

	// Create creates a new VM if it doesn't exist (idempotent)
	// Returns TaskRef if the operation is asynchronous
	Create(ctx context.Context, req CreateRequest) (CreateResponse, error)

	// Delete removes a VM (idempotent, succeeds even if VM doesn't exist)
	// Returns TaskRef if the operation is asynchronous
	Delete(ctx context.Context, id string) (taskRef string, err error)

	// Power performs a power operation on the VM
	// Returns TaskRef if the operation is asynchronous
	Power(ctx context.Context, id string, op PowerOp) (taskRef string, err error)

	// Reconfigure modifies VM resources (CPU/RAM/Disks)
	// May be no-op for unsupported fields
	// Returns TaskRef if the operation is asynchronous
	Reconfigure(ctx context.Context, id string, desired CreateRequest) (taskRef string, err error)

	// Describe returns the current state of the VM
	// Should be cheap and resilient to call frequently
	Describe(ctx context.Context, id string) (DescribeResponse, error)

	// IsTaskComplete checks if an async task is complete
	IsTaskComplete(ctx context.Context, taskRef string) (done bool, err error)
}
