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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
)

// Provider implements the Proxmox VE provider
type Provider struct {
	providerv1.UnimplementedProviderServer
	client       *pveapi.Client
	capabilities *capabilities.Manager
	logger       *slog.Logger
}

// New creates a new Proxmox provider
func New() *Provider {
	// Build capabilities for Proxmox VE
	caps := capabilities.NewBuilder().
		Core().
		Snapshots().
		LinkedClones().
		ImageImport().
		DiskTypes("raw", "qcow2").
		NetworkTypes("bridge", "vlan").
		Build()

	// Create PVE client from environment
	config := &pveapi.Config{
		Endpoint:           os.Getenv("PVE_ENDPOINT"),
		TokenID:            os.Getenv("PVE_TOKEN_ID"),
		TokenSecret:        os.Getenv("PVE_TOKEN_SECRET"),
		Username:           os.Getenv("PVE_USERNAME"),
		Password:           os.Getenv("PVE_PASSWORD"),
		InsecureSkipVerify: os.Getenv("PVE_INSECURE_SKIP_VERIFY") == "true",
	}

	// Parse node selector
	if nodeSelector := os.Getenv("PVE_NODE_SELECTOR"); nodeSelector != "" {
		config.NodeSelector = strings.Split(nodeSelector, ",")
		for i := range config.NodeSelector {
			config.NodeSelector[i] = strings.TrimSpace(config.NodeSelector[i])
		}
	}

	// Parse CA bundle
	if caBundle := os.Getenv("PVE_CA_BUNDLE"); caBundle != "" {
		config.CABundle = []byte(caBundle)
	}

	client, err := pveapi.NewClient(config)
	if err != nil {
		// Log error but continue - validation will catch connection issues
		slog.Error("Failed to create PVE client", "error", err)
	}

	return &Provider{
		client:       client,
		capabilities: caps,
		logger:       slog.Default(),
	}
}

// Validate validates the provider configuration and connectivity
func (p *Provider) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	if p.client == nil {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: "PVE client not configured",
		}, nil
	}

	// Test connectivity by trying to find a node
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: fmt.Sprintf("Failed to connect to Proxmox VE: %v", err),
		}, nil
	}

	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: fmt.Sprintf("Proxmox VE provider is ready (node: %s)", node),
	}, nil
}

// Create creates a new virtual machine
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	// Parse the request
	vmConfig, node, err := p.parseCreateRequest(req)
	if err != nil {
		return nil, errors.NewInvalidSpec("failed to parse create request: %v", err)
	}

	// Check if VM already exists
	if existing, err := p.client.GetVM(ctx, node, vmConfig.VMID); err == nil && existing != nil {
		// VM exists, return its ID
		return &providerv1.CreateResponse{
			Id: fmt.Sprintf("%d", vmConfig.VMID),
		}, nil
	}

	// Create the VM
	taskID, err := p.client.CreateVM(ctx, node, vmConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create VM: %v", err)
	}

	result := &providerv1.CreateResponse{
		Id: fmt.Sprintf("%d", vmConfig.VMID),
	}

	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// Delete deletes a virtual machine
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	taskID, err := p.client.DeleteVM(ctx, node, vmid)
	if err != nil {
		return nil, errors.NewInternal("failed to delete VM: %v", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// Power performs power operations on a virtual machine
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	var operation string
	switch req.Op {
	case providerv1.PowerOp_POWER_OP_ON:
		operation = "start"
	case providerv1.PowerOp_POWER_OP_OFF:
		operation = "stop"
	case providerv1.PowerOp_POWER_OP_REBOOT:
		operation = "reboot"
	default:
		return nil, errors.NewInvalidSpec("unsupported power operation: %v", req.Op)
	}

	taskID, err := p.client.PowerOperation(ctx, node, vmid, operation)
	if err != nil {
		return nil, errors.NewInternal("failed to perform power operation: %v", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// Reconfigure reconfigures a virtual machine
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement VM reconfiguration for Proxmox VE
	return nil, errors.NewUnimplemented("Reconfigure")
}

// Describe describes a virtual machine's current state
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	vm, err := p.client.GetVM(ctx, node, vmid)
	if err != nil {
		if err == pveapi.ErrVMNotFound {
			return &providerv1.DescribeResponse{
				Exists:     false,
				PowerState: "notfound",
			}, nil
		}
		return nil, errors.NewInternal("failed to describe VM: %v", err)
	}

	// Convert PVE status to standard power state
	powerState := "unknown"
	switch vm.Status {
	case "running":
		powerState = "on"
	case "stopped":
		powerState = "off"
	case "suspended", "paused":
		powerState = "suspended"
	}

	// Generate console URL
	endpoint := ""
	if p.client != nil && p.client.Config() != nil {
		endpoint = p.client.Config().Endpoint
	}
	consoleURL := fmt.Sprintf("%s/#v1:0:=qemu/%d:4:5:=console", 
		strings.TrimSuffix(endpoint, "/api2"), vmid)

	// TODO: Extract IP addresses from guest agent or network config
	var ips []string

	// Provider-specific details
	providerRaw := map[string]string{
		"node":      vm.Node,
		"vmid":      strconv.Itoa(vm.VMID),
		"status":    vm.Status,
		"qmpstatus": vm.QMPStatus,
	}
	if vm.PID != 0 {
		providerRaw["pid"] = strconv.Itoa(vm.PID)
	}
	if vm.ConfigLock != "" {
		providerRaw["lock"] = vm.ConfigLock
	}

	providerRawJSON, _ := json.Marshal(providerRaw)

	return &providerv1.DescribeResponse{
		Exists:          true,
		PowerState:      powerState,
		Ips:             ips,
		ConsoleUrl:      consoleURL,
		ProviderRawJson: string(providerRawJSON),
	}, nil
}

// TaskStatus checks the status of an async task
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	// Parse task ID to extract node
	taskID := req.Task.Id
	parts := strings.Split(taskID, ":")
	if len(parts) < 3 {
		return &providerv1.TaskStatusResponse{
			Done:  true,
			Error: fmt.Sprintf("invalid task ID format: %s", taskID),
		}, nil
	}

	node := parts[1]
	task, err := p.client.GetTaskStatus(ctx, node, taskID)
	if err != nil {
		return &providerv1.TaskStatusResponse{
			Done:  true,
			Error: fmt.Sprintf("failed to get task status: %v", err),
		}, nil
	}

	done := task.Status == "stopped"
	errorMsg := ""
	if done && task.ExitCode != nil && *task.ExitCode != "OK" {
		errorMsg = fmt.Sprintf("task failed with exit code: %s", *task.ExitCode)
	}

	return &providerv1.TaskStatusResponse{
		Done:  done,
		Error: errorMsg,
	}, nil
}

// SnapshotCreate creates a VM snapshot
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	// TODO: Implement snapshot creation for Proxmox VE
	return nil, errors.NewUnimplemented("SnapshotCreate")
}

// SnapshotDelete deletes a VM snapshot
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement snapshot deletion for Proxmox VE
	return nil, errors.NewUnimplemented("SnapshotDelete")
}

// SnapshotRevert reverts a VM to a snapshot
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement snapshot revert for Proxmox VE
	return nil, errors.NewUnimplemented("SnapshotRevert")
}

// Clone clones a virtual machine
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	sourceVMID, sourceNode, err := p.parseVMReference(req.SourceVmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid source VM reference: %v", err)
	}

	// Generate new VMID for clone
	targetVMID := int(time.Now().Unix() % 999999) + 100000 // Ensure 6-digit VMID
	targetNode := sourceNode // Clone to same node by default

	config := &pveapi.VMConfig{
		VMID: targetVMID,
		Name: req.TargetName,
	}

	if req.LinkedClone {
		config.Custom = map[string]string{"full": "0"}
	} else {
		config.Custom = map[string]string{"full": "1"}
	}

	taskID, err := p.client.CloneVM(ctx, sourceNode, sourceVMID, config)
	if err != nil {
		return nil, errors.NewInternal("failed to clone VM: %v", err)
	}

	result := &providerv1.CloneResponse{
		TargetVmId: fmt.Sprintf("%d", targetVMID),
	}

	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// ImagePrepare prepares an image for use
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	// TODO: Implement image preparation for Proxmox VE
	return nil, errors.NewUnimplemented("ImagePrepare")
}

// GetCapabilities returns the provider's capabilities
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return p.capabilities.GetCapabilities(ctx, req)
}

// Helper methods

// parseCreateRequest parses the gRPC create request into PVE API format
func (p *Provider) parseCreateRequest(req *providerv1.CreateRequest) (*pveapi.VMConfig, string, error) {
	// Generate VMID from name hash or use timestamp
	vmid := int(time.Now().Unix() % 999999) + 100000

	config := &pveapi.VMConfig{
		VMID: vmid,
		Name: req.Name,
	}

	// Parse VMClass for CPU/memory
	if req.ClassJson != "" {
		var class map[string]interface{}
		if err := json.Unmarshal([]byte(req.ClassJson), &class); err == nil {
			if cpus, ok := class["cpus"].(float64); ok {
				config.CPUs = int(cpus)
			}
			if memory, ok := class["memory"].(string); ok {
				if memBytes, err := parseMemory(memory); err == nil {
					config.Memory = memBytes / (1024 * 1024) // Convert to MB
				}
			}
		}
	}

	// Parse VMImage for template
	if req.ImageJson != "" {
		var image map[string]interface{}
		if err := json.Unmarshal([]byte(req.ImageJson), &image); err == nil {
			if template, ok := image["source"].(string); ok {
				config.Template = template
			}
		}
	}

	// Configure cloud-init if user data provided
	if len(req.UserData) > 0 {
		config.IDE2 = "cloudinit"
		// TODO: Process cloud-init data and extract user/ssh keys
	}

	// Find appropriate node
	node, err := p.client.FindNode(context.Background())
	if err != nil {
		return nil, "", fmt.Errorf("failed to find node: %w", err)
	}

	return config, node, nil
}

// parseVMReference parses a VM reference (ID) into VMID and node
func (p *Provider) parseVMReference(ref string) (int, string, error) {
	// Try to parse as simple VMID first
	if vmid, err := strconv.Atoi(ref); err == nil {
		// Find node for this VMID
		node, err := p.client.FindNode(context.Background())
		if err != nil {
			return 0, "", fmt.Errorf("failed to find node: %w", err)
		}
		return vmid, node, nil
	}

	// Try to parse as node:vmid format
	parts := strings.Split(ref, ":")
	if len(parts) == 2 {
		vmid, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, "", fmt.Errorf("invalid VMID in reference: %s", parts[1])
		}
		return vmid, parts[0], nil
	}

	return 0, "", fmt.Errorf("invalid VM reference format: %s", ref)
}

// parseMemory converts memory string (e.g., "2Gi", "1024Mi") to bytes
func parseMemory(memory string) (int64, error) {
	memory = strings.TrimSpace(memory)
	if strings.HasSuffix(memory, "Gi") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(memory, "Gi"), 64)
		if err != nil {
			return 0, err
		}
		return int64(val * 1024 * 1024 * 1024), nil
	}
	if strings.HasSuffix(memory, "Mi") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(memory, "Mi"), 64)
		if err != nil {
			return 0, err
		}
		return int64(val * 1024 * 1024), nil
	}
	if strings.HasSuffix(memory, "Ki") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(memory, "Ki"), 64)
		if err != nil {
			return 0, err
		}
		return int64(val * 1024), nil
	}
	// Assume bytes
	return strconv.ParseInt(memory, 10, 64)
}
