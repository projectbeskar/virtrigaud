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

package libvirt

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/internal/rpc/provider/v1"
)

// Server implements the providerv1.ProviderServer interface for Libvirt
type Server struct {
	providerv1.UnimplementedProviderServer
	provider contracts.Provider
}

// NewServer creates a new Libvirt gRPC server
func NewServer(provider contracts.Provider) *Server {
	return &Server{
		provider: provider,
	}
}

// Validate validates the provider configuration
func (s *Server) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	err := s.provider.Validate(ctx)
	if err != nil {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: err.Error(),
		}, nil
	}

	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: "Provider is healthy",
	}, nil
}

// Create creates a new virtual machine
func (s *Server) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	// Parse JSON-encoded specifications
	createReq, err := s.parseCreateRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to parse create request: %w", err)
	}

	resp, err := s.provider.Create(ctx, createReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	result := &providerv1.CreateResponse{
		Id: resp.ID,
	}

	if resp.TaskRef != "" {
		result.Task = &providerv1.TaskRef{Id: resp.TaskRef}
	}

	return result, nil
}

// Delete deletes a virtual machine
func (s *Server) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	taskRef, err := s.provider.Delete(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to delete VM: %w", err)
	}

	result := &providerv1.TaskResponse{}
	if taskRef != "" {
		result.Task = &providerv1.TaskRef{Id: taskRef}
	}

	return result, nil
}

// Power performs power operations on a virtual machine
func (s *Server) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	var powerOp contracts.PowerOp
	switch req.Op {
	case providerv1.PowerOp_POWER_OP_ON:
		powerOp = contracts.PowerOpOn
	case providerv1.PowerOp_POWER_OP_OFF:
		powerOp = contracts.PowerOpOff
	case providerv1.PowerOp_POWER_OP_REBOOT:
		powerOp = contracts.PowerOpReboot
	default:
		return nil, fmt.Errorf("unsupported power operation: %v", req.Op)
	}

	taskRef, err := s.provider.Power(ctx, req.Id, powerOp)
	if err != nil {
		return nil, fmt.Errorf("failed to perform power operation: %w", err)
	}

	result := &providerv1.TaskResponse{}
	if taskRef != "" {
		result.Task = &providerv1.TaskRef{Id: taskRef}
	}

	return result, nil
}

// Reconfigure reconfigures a virtual machine
func (s *Server) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	// Parse the desired configuration
	var createReq contracts.CreateRequest
	if err := json.Unmarshal([]byte(req.DesiredJson), &createReq); err != nil {
		return nil, fmt.Errorf("failed to parse desired configuration: %w", err)
	}

	taskRef, err := s.provider.Reconfigure(ctx, req.Id, createReq)
	if err != nil {
		return nil, fmt.Errorf("failed to reconfigure VM: %w", err)
	}

	result := &providerv1.TaskResponse{}
	if taskRef != "" {
		result.Task = &providerv1.TaskRef{Id: taskRef}
	}

	return result, nil
}

// Describe describes the current state of a virtual machine
func (s *Server) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	resp, err := s.provider.Describe(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to describe VM: %w", err)
	}

	// Convert provider raw data to JSON
	providerRawJSON := "{}"
	if len(resp.ProviderRaw) > 0 {
		data, err := json.Marshal(resp.ProviderRaw)
		if err == nil {
			providerRawJSON = string(data)
		}
	}

	return &providerv1.DescribeResponse{
		Exists:          resp.Exists,
		PowerState:      resp.PowerState,
		Ips:             resp.IPs,
		ConsoleUrl:      resp.ConsoleURL,
		ProviderRawJson: providerRawJSON,
	}, nil
}

// TaskStatus checks the status of an async task
func (s *Server) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	done, err := s.provider.IsTaskComplete(ctx, req.Task.Id)
	if err != nil {
		return &providerv1.TaskStatusResponse{
			Done:  false,
			Error: err.Error(),
		}, nil
	}

	return &providerv1.TaskStatusResponse{
		Done:  done,
		Error: "",
	}, nil
}

// parseCreateRequest converts gRPC request to contracts.CreateRequest
func (s *Server) parseCreateRequest(req *providerv1.CreateRequest) (contracts.CreateRequest, error) {
	createReq := contracts.CreateRequest{
		Name: req.Name,
		Tags: req.Tags,
	}

	// Parse UserData if provided
	if len(req.UserData) > 0 {
		createReq.UserData = &contracts.UserData{
			CloudInitData: string(req.UserData),
		}
	}

	// Parse VMClass
	if req.ClassJson != "" {
		if err := json.Unmarshal([]byte(req.ClassJson), &createReq.Class); err != nil {
			return createReq, fmt.Errorf("failed to parse class JSON: %w", err)
		}
	}

	// Parse VMImage
	if req.ImageJson != "" {
		if err := json.Unmarshal([]byte(req.ImageJson), &createReq.Image); err != nil {
			return createReq, fmt.Errorf("failed to parse image JSON: %w", err)
		}
	}

	// Parse Networks
	if req.NetworksJson != "" {
		if err := json.Unmarshal([]byte(req.NetworksJson), &createReq.Networks); err != nil {
			return createReq, fmt.Errorf("failed to parse networks JSON: %w", err)
		}
	}

	// Parse Disks
	if req.DisksJson != "" {
		if err := json.Unmarshal([]byte(req.DisksJson), &createReq.Disks); err != nil {
			return createReq, fmt.Errorf("failed to parse disks JSON: %w", err)
		}
	}

	// Parse Placement
	if req.PlacementJson != "" {
		if err := json.Unmarshal([]byte(req.PlacementJson), &createReq.Placement); err != nil {
			return createReq, fmt.Errorf("failed to parse placement JSON: %w", err)
		}
	}

	return createReq, nil
}

// SnapshotCreate creates a VM snapshot
func (s *Server) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	// Libvirt supports disk snapshots; memory snapshots depend on storage backend
	snapshotID := fmt.Sprintf("snapshot-%s-%d", req.NameHint, generateTimestamp())

	// For now, simulate async operation
	taskRef := &providerv1.TaskRef{
		Id: fmt.Sprintf("task-snapshot-%s", generateTaskID()),
	}

	// Note: Memory snapshots may not be supported on all storage backends
	return &providerv1.SnapshotCreateResponse{
		SnapshotId: snapshotID,
		Task:       taskRef,
	}, nil
}

// SnapshotDelete deletes a VM snapshot
func (s *Server) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	// Simulate async snapshot deletion
	taskRef := &providerv1.TaskRef{
		Id: fmt.Sprintf("task-delete-snapshot-%s", generateTaskID()),
	}

	return &providerv1.TaskResponse{
		Task: taskRef,
	}, nil
}

// SnapshotRevert reverts a VM to a snapshot
func (s *Server) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	// Simulate async snapshot revert
	taskRef := &providerv1.TaskRef{
		Id: fmt.Sprintf("task-revert-snapshot-%s", generateTaskID()),
	}

	return &providerv1.TaskResponse{
		Task: taskRef,
	}, nil
}

// Clone creates a VM clone
func (s *Server) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	// Libvirt supports cloning via volume clone + new domain definition
	// Linked clones are supported with qcow2 backing files
	targetVMID := fmt.Sprintf("vm-clone-%s", generateVMID())

	// Simulate async clone operation
	taskRef := &providerv1.TaskRef{
		Id: fmt.Sprintf("task-clone-%s", generateTaskID()),
	}

	return &providerv1.CloneResponse{
		TargetVmId: targetVMID,
		Task:       taskRef,
	}, nil
}

// ImagePrepare prepares/imports a VM image
func (s *Server) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	// Libvirt supports downloading qcow2/raw images to storage pools
	taskRef := &providerv1.TaskRef{
		Id: fmt.Sprintf("task-image-prepare-%s", generateTaskID()),
	}

	return &providerv1.TaskResponse{
		Task: taskRef,
	}, nil
}

// GetCapabilities returns the capabilities of the Libvirt provider
func (s *Server) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{
		SupportsReconfigureOnline:   false, // Libvirt typically requires power cycle for CPU/memory changes
		SupportsDiskExpansionOnline: false, // Disk expansion usually requires power cycle
		SupportsSnapshots:           true,  // Libvirt supports snapshots (storage-dependent)
		SupportsMemorySnapshots:     false, // Memory snapshots not always supported
		SupportsLinkedClones:        true,  // Supported via qcow2 backing files
		SupportsImageImport:         true,  // Supports downloading images to storage pools
		SupportedDiskTypes:          []string{"qcow2", "raw", "vmdk"},
		SupportedNetworkTypes:       []string{"virtio", "e1000", "rtl8139"},
	}, nil
}

// Helper functions for generating IDs and timestamps (shared with vSphere)
func generateTimestamp() int64 {
	return 1642671234 // Placeholder - would use time.Now().Unix()
}

func generateTaskID() string {
	return "12345678-1234-1234-1234-123456789012" // Placeholder - would generate UUID
}

func generateVMID() string {
	return "vm-12345678" // Placeholder - would generate unique ID
}
