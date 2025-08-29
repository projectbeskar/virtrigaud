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
	"math/rand"
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
	// Get capabilities for Proxmox VE
	caps := GetProviderCapabilities()

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

	// Check if VM already exists (idempotency)
	if existing, err := p.client.GetVM(ctx, node, vmConfig.VMID); err == nil && existing != nil {
		// VM exists, check if it matches our requirements
		if existing.Name == req.Name {
			p.logger.Info("VM already exists with same name, skipping creation", 
				"vmid", vmConfig.VMID, "name", req.Name)
			return &providerv1.CreateResponse{
				Id: fmt.Sprintf("%d", vmConfig.VMID),
			}, nil
		} else {
			// VM exists but with different name, generate new VMID
			p.logger.Warn("VM exists with different name, generating new VMID", 
				"existing_vmid", vmConfig.VMID, "existing_name", existing.Name, "requested_name", req.Name)
			vmConfig.VMID = int(time.Now().Unix()%999999) + 100000
		}
	}

	// Create the VM with better error handling
	taskID, err := p.client.CreateVM(ctx, node, vmConfig)
	if err != nil {
		// Map specific PVE errors to appropriate SDK errors
		errMsg := err.Error()
		if strings.Contains(errMsg, "already exists") {
			return nil, errors.NewAlreadyExists("VM", fmt.Sprintf("%d", vmConfig.VMID))
		}
		if strings.Contains(errMsg, "insufficient") || strings.Contains(errMsg, "no space") {
			return nil, errors.NewResourceExhausted("insufficient resources for VM creation")
		}
		if strings.Contains(errMsg, "permission") || strings.Contains(errMsg, "access denied") {
			return nil, errors.NewPermissionDenied("create VM")
		}
		if strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "bad parameter") {
			return nil, errors.NewInvalidSpec("invalid VM configuration: %v", err)
		}
		if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "connection") {
			return nil, errors.NewUnavailable("Proxmox VE API unavailable: %v", err)
		}
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
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	vmid, node, err := p.parseVMReference(req.Id)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// Get current VM configuration to check what can be changed online
	currentConfig, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return nil, errors.NewInternal("failed to get current VM config: %v", err)
	}

	// Parse the desired configuration
	var desired map[string]interface{}
	if err := json.Unmarshal([]byte(req.DesiredJson), &desired); err != nil {
		return nil, errors.NewInvalidSpec("failed to parse desired configuration: %v", err)
	}

	// Extract reconfiguration parameters
	config := &pveapi.ReconfigureConfig{}
	requiresPowerCycle := false

	// Handle CPU changes
	if classData, ok := desired["class"].(map[string]interface{}); ok {
		if cpus, ok := classData["cpus"].(float64); ok {
			cpuCount := int(cpus)
			config.CPUs = &cpuCount
			
			// Check if VM is running - CPU changes may require power cycle
			vm, err := p.client.GetVM(ctx, node, vmid)
			if err == nil && vm.Status == "running" {
				// PVE supports online CPU changes in most cases, but check current config
				if currentCPUs, exists := currentConfig["cores"]; exists {
					if currentCPUCount, ok := currentCPUs.(float64); ok && int(currentCPUCount) != cpuCount {
						// Online CPU change is supported in PVE for most guest OSes
						p.logger.Info("CPU change will be applied online", "vmid", vmid, "old", int(currentCPUCount), "new", cpuCount)
					}
				}
			}
		}
		
		// Handle memory changes
		if memory, ok := classData["memory"].(string); ok {
			if memBytes, err := parseMemory(memory); err == nil {
				memMB := memBytes / (1024 * 1024)
				config.Memory = &memMB
				
				// Check if VM is running - memory changes may require power cycle for some guest OSes
				vm, err := p.client.GetVM(ctx, node, vmid)
				if err == nil && vm.Status == "running" {
					// PVE supports online memory changes with balloon driver
					if currentMem, exists := currentConfig["memory"]; exists {
						if currentMemMB, ok := currentMem.(float64); ok && int64(currentMemMB) != memMB {
							p.logger.Info("Memory change will be applied online (requires balloon driver)", "vmid", vmid, "old", int64(currentMemMB), "new", memMB)
						}
					}
				}
			}
		}
	}

	// Handle disk changes
	if disksData, ok := desired["disks"].([]interface{}); ok {
		for _, diskData := range disksData {
			if disk, ok := diskData.(map[string]interface{}); ok {
				if diskName, ok := disk["name"].(string); ok {
					if sizeStr, ok := disk["size"].(string); ok {
						if sizeBytes, err := parseMemory(sizeStr); err == nil {
							sizeGB := sizeBytes / (1024 * 1024 * 1024)
							
							// Find corresponding disk in current config
							diskKey := fmt.Sprintf("scsi0") // Default to scsi0, could be smarter
							if diskName == "root" {
								diskKey = "scsi0"
							}
							
							// Check current disk size to prevent shrinking
							if currentDisk, exists := currentConfig[diskKey]; exists {
								if currentDiskStr, ok := currentDisk.(string); ok {
									// Parse current disk size from config string (e.g., "local:vm-100-disk-0,size=32G")
									if strings.Contains(currentDiskStr, "size=") {
										parts := strings.Split(currentDiskStr, ",")
										for _, part := range parts {
											if strings.HasPrefix(part, "size=") {
												sizeStr := strings.TrimPrefix(part, "size=")
												sizeStr = strings.TrimSuffix(sizeStr, "G")
												if currentSize, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
													if sizeGB < currentSize {
														return nil, errors.NewInvalidSpec("disk shrinking not allowed: current=%dG, requested=%dG", currentSize, sizeGB)
													}
													if sizeGB > currentSize {
														// Use ResizeDisk for disk expansion
														taskID, err := p.client.ResizeDisk(ctx, node, vmid, diskKey, sizeGB)
														if err != nil {
															return nil, errors.NewInternal("failed to resize disk: %v", err)
														}
														
														result := &providerv1.TaskResponse{}
														if taskID != "" {
															result.Task = &providerv1.TaskRef{Id: taskID}
														}
														return result, nil
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Apply CPU/Memory changes if any
	if config.CPUs != nil || config.Memory != nil {
		taskID, err := p.client.ReconfigureVM(ctx, node, vmid, config)
		if err != nil {
			return nil, errors.NewInternal("failed to reconfigure VM: %v", err)
		}

		result := &providerv1.TaskResponse{}
		if taskID != "" {
			result.Task = &providerv1.TaskRef{Id: taskID}
		}
		
		// If power cycle is required, include in response metadata
		if requiresPowerCycle {
			result.RequiresPowerCycle = true
		}

		return result, nil
	}

	// No changes needed
	return &providerv1.TaskResponse{}, nil
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
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// Generate snapshot name if not provided
	snapName := req.NameHint
	if snapName == "" {
		snapName = fmt.Sprintf("snapshot-%d", time.Now().Unix())
	}

	// Create snapshot
	taskID, err := p.client.CreateSnapshot(ctx, node, vmid, snapName, req.Description, req.IncludeMemory)
	if err != nil {
		return nil, errors.NewInternal("failed to create snapshot: %v", err)
	}

	result := &providerv1.SnapshotCreateResponse{
		SnapshotId: snapName,
	}

	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// SnapshotDelete deletes a VM snapshot
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	taskID, err := p.client.DeleteSnapshot(ctx, node, vmid, req.SnapshotId)
	if err != nil {
		return nil, errors.NewInternal("failed to delete snapshot: %v", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
}

// SnapshotRevert reverts a VM to a snapshot
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	taskID, err := p.client.RevertSnapshot(ctx, node, vmid, req.SnapshotId)
	if err != nil {
		return nil, errors.NewInternal("failed to revert snapshot: %v", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
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
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured")
	}

	// Find appropriate node and storage
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return nil, errors.NewInternal("failed to find node: %v", err)
	}

	// Default storage - could be made configurable
	storage := "local-lvm"
	if req.StorageClass != "" {
		storage = req.StorageClass
	}

	// Determine if we need to import or just ensure template exists
	templateName := ""
	imageURL := ""

	if req.ImageRef != "" {
		// Parse image reference to determine if it's a template name or URL
		if strings.HasPrefix(req.ImageRef, "http://") || strings.HasPrefix(req.ImageRef, "https://") {
			imageURL = req.ImageRef
			// Generate template name from URL
			parts := strings.Split(req.ImageRef, "/")
			if len(parts) > 0 {
				templateName = strings.TrimSuffix(parts[len(parts)-1], ".qcow2")
				templateName = strings.TrimSuffix(templateName, ".ova")
			}
		} else {
			// Assume it's a template name
			templateName = req.ImageRef
		}
	}

	// Prepare the image/template
	taskID, err := p.client.PrepareImage(ctx, node, storage, imageURL, templateName)
	if err != nil {
		return nil, errors.NewInternal("failed to prepare image: %v", err)
	}

	result := &providerv1.TaskResponse{}
	if taskID != "" {
		result.Task = &providerv1.TaskRef{Id: taskID}
	}

	return result, nil
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

	// Parse Networks configuration
	if req.NetworksJson != "" {
		var networksData []interface{}
		if err := json.Unmarshal([]byte(req.NetworksJson), &networksData); err == nil {
			config.Networks = make([]pveapi.NetworkConfig, 0, len(networksData))
			config.IPConfigs = make([]pveapi.IPConfig, 0, len(networksData))
			
			for i, netData := range networksData {
				if network, ok := netData.(map[string]interface{}); ok {
					netConfig := pveapi.NetworkConfig{
						Index:  i,
						Model:  "virtio", // Default model
						Bridge: "vmbr0",  // Default bridge
					}
					
					ipConfig := pveapi.IPConfig{
						Index: i,
						DHCP:  true, // Default to DHCP
					}

					// Extract network name and map to bridge
					if name, ok := network["name"].(string); ok {
						// Map network names to bridges
						switch name {
						case "lan", "default":
							netConfig.Bridge = "vmbr0"
						case "dmz":
							netConfig.Bridge = "vmbr1"
						case "management", "mgmt":
							netConfig.Bridge = "vmbr2"
						default:
							// Use the name as bridge if it looks like a bridge name
							if strings.HasPrefix(name, "vmbr") {
								netConfig.Bridge = name
							}
						}
					}

					// Check for VLAN configuration
					if vlan, ok := network["vlan"].(float64); ok {
						netConfig.VLAN = int(vlan)
					}

					// Check for static IP configuration
					if staticIP, ok := network["static_ip"].(map[string]interface{}); ok {
						if ip, ok := staticIP["address"].(string); ok {
							ipConfig.IP = ip
							ipConfig.DHCP = false
						}
						if gw, ok := staticIP["gateway"].(string); ok {
							ipConfig.Gateway = gw
						}
						if dns, ok := staticIP["dns"].([]interface{}); ok {
							var dnsServers []string
							for _, d := range dns {
								if dnsStr, ok := d.(string); ok {
									dnsServers = append(dnsServers, dnsStr)
								}
							}
							if len(dnsServers) > 0 {
								ipConfig.DNS = strings.Join(dnsServers, ",")
							}
						}
					}

					// Check for MAC address
					if mac, ok := network["mac"].(string); ok {
						netConfig.MAC = mac
					}

					config.Networks = append(config.Networks, netConfig)
					config.IPConfigs = append(config.IPConfigs, ipConfig)
				}
			}
		}
	}

	// Default network if none specified
	if len(config.Networks) == 0 {
		config.Networks = []pveapi.NetworkConfig{{
			Index:  0,
			Model:  "virtio",
			Bridge: "vmbr0",
		}}
		config.IPConfigs = []pveapi.IPConfig{{
			Index: 0,
			DHCP:  true,
		}}
	}

	// Configure cloud-init if user data provided
	if len(req.UserData) > 0 {
		config.IDE2 = "cloudinit"
		
		// Extract SSH keys and user from cloud-init data if possible
		userData := string(req.UserData)
		if strings.Contains(userData, "ssh_authorized_keys:") {
			// Try to extract SSH keys from YAML
			lines := strings.Split(userData, "\n")
			var sshKeys []string
			inKeys := false
			for _, line := range lines {
				if strings.Contains(line, "ssh_authorized_keys:") {
					inKeys = true
					continue
				}
				if inKeys && strings.HasPrefix(strings.TrimSpace(line), "- ") {
					key := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
					key = strings.Trim(key, "\"'")
					if key != "" {
						sshKeys = append(sshKeys, key)
					}
				} else if inKeys && !strings.HasPrefix(strings.TrimSpace(line), " ") {
					inKeys = false
				}
			}
			if len(sshKeys) > 0 {
				config.SSHKeys = strings.Join(sshKeys, "\n")
			}
		}
		
		// Extract username
		if strings.Contains(userData, "name:") {
			lines := strings.Split(userData, "\n")
			for _, line := range lines {
				if strings.Contains(line, "name:") && !strings.Contains(line, "hostname:") {
					parts := strings.Split(line, ":")
					if len(parts) >= 2 {
						username := strings.TrimSpace(parts[1])
						username = strings.Trim(username, "\"' ")
						if username != "" {
							config.CIUser = username
						}
					}
					break
				}
			}
		}
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
