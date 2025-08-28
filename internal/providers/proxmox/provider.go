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

package proxmox

import (
	"context"
	"fmt"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
)

// Provider implements the Proxmox provider.
type Provider struct {
	providerv1.UnimplementedProviderServer
	capabilities *capabilities.Manager
}

// New creates a new Proxmox provider.
func New() *Provider {
	// Build capabilities for this provider
	caps := capabilities.NewBuilder().
		Core().
		Snapshots().
		MemorySnapshots().
		LinkedClones().
		OnlineReconfigure().
		OnlineDiskExpansion().
		ImageImport().
		DiskTypes("raw", "qcow2").
		NetworkTypes("bridge", "vlan").
		Build()

	return &Provider{
		capabilities: caps,
	}
}

// Validate validates the provider configuration.
func (p *Provider) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	// TODO: Implement Proxmox VE connection validation
	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: "Proxmox provider is ready",
	}, nil
}

// Create creates a new virtual machine.
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	// TODO: Implement VM creation for Proxmox VE
	return nil, errors.NewUnimplemented("Create")
}

// Delete deletes a virtual machine.
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement VM deletion for Proxmox VE
	return nil, errors.NewUnimplemented("Delete")
}

// Power performs power operations on a virtual machine.
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement power operations for Proxmox VE
	return nil, errors.NewUnimplemented("Power")
}

// Reconfigure reconfigures a virtual machine.
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement VM reconfiguration for Proxmox VE
	return nil, errors.NewUnimplemented("Reconfigure")
}

// Describe describes a virtual machine's current state.
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	// TODO: Implement VM description for Proxmox VE
	return nil, errors.NewUnimplemented("Describe")
}

// TaskStatus checks the status of an async task.
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	// TODO: Implement task status checking for Proxmox VE
	return nil, errors.NewUnimplemented("TaskStatus")
}

// SnapshotCreate creates a VM snapshot.
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	// TODO: Implement snapshot creation for Proxmox VE
	return nil, errors.NewUnimplemented("SnapshotCreate")
}

// SnapshotDelete deletes a VM snapshot.
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement snapshot deletion for Proxmox VE
	return nil, errors.NewUnimplemented("SnapshotDelete")
}

// SnapshotRevert reverts a VM to a snapshot.
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement snapshot revert for Proxmox VE
	return nil, errors.NewUnimplemented("SnapshotRevert")
}

// Clone clones a virtual machine.
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	// TODO: Implement VM cloning for Proxmox VE
	return nil, errors.NewUnimplemented("Clone")
}

// ImagePrepare prepares an image for use.
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement image preparation for Proxmox VE
	return nil, errors.NewUnimplemented("ImagePrepare")
}

// GetCapabilities returns the provider's capabilities.
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return p.capabilities.GetCapabilities(ctx, req)
}
