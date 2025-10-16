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
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	v1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/proxmox/pveapi"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/capabilities"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

// Provider implements the Proxmox VE provider
type Provider struct {
	providerv1.UnimplementedProviderServer
	client       *pveapi.Client
	capabilities *capabilities.Manager
	logger       *slog.Logger
}

// readCredentialFile reads a credential from a mounted secret file
func readCredentialFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// New creates a new Proxmox provider
func New() *Provider {
	// Get capabilities for Proxmox VE
	caps := GetProviderCapabilities()

	// Create PVE client from environment
	// Support both PVE_* (legacy) and PROVIDER_* (new standard) env vars
	endpoint := os.Getenv("PROVIDER_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("PVE_ENDPOINT")
	}

	// Read credentials from mounted secret files (primary method)
	tokenID := readCredentialFile("/etc/virtrigaud/credentials/token_id")
	tokenSecret := readCredentialFile("/etc/virtrigaud/credentials/token_secret")
	username := readCredentialFile("/etc/virtrigaud/credentials/username")
	password := readCredentialFile("/etc/virtrigaud/credentials/password")

	// Fallback to environment variables if files don't exist
	if tokenID == "" {
		tokenID = os.Getenv("PROVIDER_TOKEN_ID")
		if tokenID == "" {
			tokenID = os.Getenv("PVE_TOKEN_ID")
		}
	}

	if tokenSecret == "" {
		tokenSecret = os.Getenv("PROVIDER_TOKEN_SECRET")
		if tokenSecret == "" {
			tokenSecret = os.Getenv("PVE_TOKEN_SECRET")
		}
	}

	if username == "" {
		username = os.Getenv("PROVIDER_USERNAME")
		if username == "" {
			username = os.Getenv("PVE_USERNAME")
		}
	}

	if password == "" {
		password = os.Getenv("PROVIDER_PASSWORD")
		if password == "" {
			password = os.Getenv("PVE_PASSWORD")
		}
	}

	insecureSkipVerify := os.Getenv("TLS_INSECURE_SKIP_VERIFY") == "true" || os.Getenv("PVE_INSECURE_SKIP_VERIFY") == "true"

	config := &pveapi.Config{
		Endpoint:           endpoint,
		TokenID:            tokenID,
		TokenSecret:        tokenSecret,
		Username:           username,
		Password:           password,
		InsecureSkipVerify: insecureSkipVerify,
	}

	// Parse node selector
	nodeSelector := os.Getenv("PROVIDER_NODE_SELECTOR")
	if nodeSelector == "" {
		nodeSelector = os.Getenv("PVE_NODE_SELECTOR")
	}
	if nodeSelector != "" {
		config.NodeSelector = strings.Split(nodeSelector, ",")
		for i := range config.NodeSelector {
			config.NodeSelector[i] = strings.TrimSpace(config.NodeSelector[i])
		}
	}

	// Parse CA bundle
	caBundle := os.Getenv("PROVIDER_CA_BUNDLE")
	if caBundle == "" {
		caBundle = os.Getenv("PVE_CA_BUNDLE")
	}
	if caBundle != "" {
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	// Parse the request
	vmConfig, node, err := p.parseCreateRequest(req)
	if err != nil {
		return nil, errors.NewInvalidSpec("failed to parse create request: %v", err)
	}

	// Check if VM already exists (idempotency)
	if existing, existErr := p.client.GetVM(ctx, node, vmConfig.VMID); existErr == nil && existing != nil {
		// VM exists, check if it matches our requirements
		if existing.Name == req.Name {
			p.logger.Info("VM already exists with same name, skipping creation",
				"vmid", vmConfig.VMID, "name", req.Name)
			return &providerv1.CreateResponse{
				Id: fmt.Sprintf("%d", vmConfig.VMID),
			}, nil
		}
		// VM exists but with different name, generate new VMID
		p.logger.Warn("VM exists with different name, generating new VMID",
			"existing_vmid", vmConfig.VMID, "existing_name", existing.Name, "requested_name", req.Name)
		vmConfig.VMID = int(time.Now().Unix()%999999) + 100000
	}

	var taskID string

	// Determine if we need to clone from a template or create a new VM
	if vmConfig.Template != "" {
		// Parse template ID from the Template field
		templateID, parseErr := strconv.Atoi(vmConfig.Template)
		if parseErr != nil {
			return nil, errors.NewInvalidSpec("invalid template ID '%s': %v", vmConfig.Template, parseErr)
		}

		// Clone from template
		p.logger.Info("Cloning VM from template", "template_id", templateID, "new_vmid", vmConfig.VMID)

		// Set full clone flag
		if vmConfig.Custom == nil {
			vmConfig.Custom = make(map[string]string)
		}
		vmConfig.Custom["full"] = "1" // Full clone by default

		taskID, err = p.client.CloneVM(ctx, node, templateID, vmConfig)
		if err != nil {
			return nil, errors.NewInvalidSpec("failed to clone template: %v", err)
		}

		// Wait for clone to complete
		if taskID != "" {
			if err = p.client.WaitForTask(ctx, node, taskID); err != nil {
				return nil, errors.NewInternal("clone task failed: %v", err)
			}
		}

		// After cloning, we need to reconfigure the VM with cloud-init settings
		if len(req.UserData) > 0 || vmConfig.SSHKeys != "" {
			p.logger.Info("Reconfiguring cloned VM with cloud-init", "vmid", vmConfig.VMID)

			// Auto-detect primary boot disk from cloned VM
			primaryDisk, err := p.client.DetectPrimaryDisk(ctx, node, vmConfig.VMID)
			if err != nil {
				p.logger.Warn("Failed to detect primary disk, using scsi0 as fallback", "error", err)
				primaryDisk = "scsi0"
			}
			p.logger.Info("Detected primary boot disk", "vmid", vmConfig.VMID, "disk", primaryDisk)

			// Build reconfiguration values for cloud-init
			reconfigValues := url.Values{}
			if vmConfig.IDE2 != "" {
				reconfigValues.Set("ide2", vmConfig.IDE2)
			}
			if vmConfig.SSHKeys != "" {
				reconfigValues.Set("sshkeys", vmConfig.SSHKeys)
			}
			if vmConfig.CIUser != "" {
				reconfigValues.Set("ciuser", vmConfig.CIUser)
			}
			// Set boot order: detected primary disk first, then cloud-init drive
			bootOrder := fmt.Sprintf("order=%s;ide2", primaryDisk)
			reconfigValues.Set("boot", bootOrder)
			p.logger.Info("Setting boot order", "vmid", vmConfig.VMID, "boot", bootOrder)

			// Add network config (these should already be in the cloned VM, but just to be safe)
			for _, netConfig := range vmConfig.Networks {
				netString := p.buildNetworkString(netConfig)
				if netString != "" {
					reconfigValues.Set(fmt.Sprintf("net%d", netConfig.Index), netString)
				}
			}
			// Add IP config
			for _, ipConfig := range vmConfig.IPConfigs {
				ipString := p.buildIPConfigString(ipConfig)
				if ipString != "" {
					reconfigValues.Set(fmt.Sprintf("ipconfig%d", ipConfig.Index), ipString)
				}
			}

			// Reconfigure the cloned VM with cloud-init settings
			taskID, err = p.client.ReconfigureVMRaw(ctx, node, vmConfig.VMID, reconfigValues)
			if err != nil {
				return nil, errors.NewInternal("failed to reconfigure VM: %v", err)
			}

			// Wait for reconfiguration if async
			if taskID != "" {
				if err = p.client.WaitForTask(ctx, node, taskID); err != nil {
					return nil, errors.NewInternal("reconfigure task failed: %v", err)
				}
			}
		}
	} else {
		// Create a new VM (not from template)
		p.logger.Info("Creating new VM", "vmid", vmConfig.VMID)
		taskID, err = p.client.CreateVM(ctx, node, vmConfig)
	}

	// Handle errors
	if err != nil {
		// Map specific PVE errors to appropriate SDK errors
		errMsg := err.Error()
		if strings.Contains(errMsg, "already exists") {
			return nil, errors.NewAlreadyExists("VM", fmt.Sprintf("%d", vmConfig.VMID))
		}
		if strings.Contains(errMsg, "insufficient") || strings.Contains(errMsg, "no space") {
			return nil, errors.NewRateLimit(time.Minute * 5) // Retry after 5 minutes for resource exhaustion
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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
	case providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
		operation = "shutdown" // Proxmox supports graceful shutdown
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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
							diskKey := "scsi0" // Default to scsi0, could be smarter
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

		// Note: Power cycle requirement should be handled at a higher level
		// The TaskResponse only contains task reference for async operations

		return result, nil
	}

	// No changes needed
	return &providerv1.TaskResponse{}, nil
}

// Describe describes a virtual machine's current state
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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

	// Extract IP addresses from guest agent
	var ips []string
	if vm.Status == "running" {
		// Try to get IP addresses from QEMU guest agent
		interfaces, err := p.client.GetGuestNetworkInterfaces(ctx, node, vmid)
		if err != nil {
			p.logger.Debug("Failed to get guest network interfaces (guest agent may not be available)", "error", err)
		} else {
			// Extract IP addresses from interfaces
			for _, iface := range interfaces {
				// Skip loopback interface
				if iface.Name == "lo" {
					continue
				}

				for _, ipAddr := range iface.IPAddresses {
					// Filter out link-local addresses
					ip := ipAddr.IPAddress
					if ip != "" && !strings.HasPrefix(ip, "127.") &&
						!strings.HasPrefix(ip, "169.254.") &&
						!strings.HasPrefix(ip, "fe80:") {
						ips = append(ips, ip)
					}
				}
			}
		}
	}

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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	sourceVMID, sourceNode, err := p.parseVMReference(req.SourceVmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid source VM reference: %v", err)
	}

	// Generate new VMID for clone
	targetVMID := int(time.Now().Unix()%999999) + 100000 // Ensure 6-digit VMID

	config := &pveapi.VMConfig{
		VMID: targetVMID,
		Name: req.TargetName,
	}

	if req.Linked {
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
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	// Find appropriate node and storage
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return nil, errors.NewInternal("failed to find node: %v", err)
	}

	// Default storage - could be made configurable
	storage := "local-lvm"
	if req.StorageHint != "" {
		storage = req.StorageHint
	}

	// Determine if we need to import or just ensure template exists
	templateName := ""
	imageURL := ""

	if req.ImageJson != "" {
		// Parse JSON-encoded VMImage spec to determine template name or URL
		var imageSpec map[string]interface{}
		if err := json.Unmarshal([]byte(req.ImageJson), &imageSpec); err != nil {
			return nil, errors.NewInvalidSpec("failed to parse image JSON: %v", err)
		}

		// Extract image source information
		if source, ok := imageSpec["source"].(map[string]interface{}); ok {
			if httpSource, ok := source["http"].(map[string]interface{}); ok {
				if url, ok := httpSource["url"].(string); ok {
					imageURL = url
					// Generate template name from URL
					parts := strings.Split(url, "/")
					if len(parts) > 0 {
						templateName = strings.TrimSuffix(parts[len(parts)-1], ".qcow2")
						templateName = strings.TrimSuffix(templateName, ".ova")
					}
				}
			} else if templateRef, ok := source["template"].(string); ok {
				templateName = templateRef
			}
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
	vmid := int(time.Now().Unix()%999999) + 100000

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
	// The controller sends contracts.VMImage which has the template in TemplateName field
	if req.ImageJson != "" {
		// DEBUG: Log the raw ImageJson to see what we're actually receiving
		p.logger.Info("DEBUG: Received ImageJson", "json", req.ImageJson)

		// First try parsing as contracts.VMImage (sent by controller)
		var contractsImage map[string]interface{}
		if err := json.Unmarshal([]byte(req.ImageJson), &contractsImage); err == nil {
			p.logger.Info("DEBUG: Parsed ImageJson as map", "keys", fmt.Sprintf("%v", contractsImage))

			// Check for TemplateName field (from contracts.VMImage - note capital T)
			if templateName, ok := contractsImage["TemplateName"].(string); ok && templateName != "" {
				config.Template = templateName
				p.logger.Info("Parsed template from contracts.VMImage", "template", templateName)
			} else {
				p.logger.Info("DEBUG: TemplateName not found or empty", "TemplateName", contractsImage["TemplateName"])
			}

			// Check for storage hint
			if storage, ok := contractsImage["storage"].(string); ok && storage != "" {
				config.Storage = storage
			}
		}

		// Fallback: try to parse as VMImageSpec for backwards compatibility
		if config.Template == "" {
			var imageSpec v1beta1.VMImageSpec
			if err := json.Unmarshal([]byte(req.ImageJson), &imageSpec); err == nil {
				// Check for Proxmox-specific image source
				if imageSpec.Source.Proxmox != nil {
					proxmoxSource := imageSpec.Source.Proxmox

					// Set template ID or name
					if proxmoxSource.TemplateID != nil {
						config.Template = fmt.Sprintf("%d", *proxmoxSource.TemplateID)
					} else if proxmoxSource.TemplateName != "" {
						config.Template = proxmoxSource.TemplateName
					}

					// Set storage if specified
					if proxmoxSource.Storage != "" {
						config.Storage = proxmoxSource.Storage
					}

					// Set node if specified (for template location)
					if proxmoxSource.Node != "" {
						// Store node hint for later use
						if config.Custom == nil {
							config.Custom = make(map[string]string)
						}
						config.Custom["template_node"] = proxmoxSource.Node
					}

					// Set clone type (full vs linked)
					if proxmoxSource.FullClone != nil && !*proxmoxSource.FullClone {
						if config.Custom == nil {
							config.Custom = make(map[string]string)
						}
						config.Custom["full_clone"] = "0" // Linked clone
					}
				}
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
		// IDE2 needs storage pool prefix, use the configured storage or default to 'local'
		storage := config.Storage
		if storage == "" {
			storage = "local"
		}
		config.IDE2 = fmt.Sprintf("%s:cloudinit", storage)

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
					// Extra safety: ensure no trailing/leading whitespace including newlines
					key = strings.TrimSpace(key)
					if key != "" {
						// DEBUG: Log extracted SSH key with length and escaped representation
						slog.Info("DEBUG SSH extraction", "location", "server.go", "len", len(key), "repr", key)
						sshKeys = append(sshKeys, key)
					}
				} else if inKeys && !strings.HasPrefix(strings.TrimSpace(line), " ") {
					inKeys = false
				}
			}
			if len(sshKeys) > 0 {
				// Join multiple keys with newline separator (no trailing newline)
				// Then trim again to be absolutely sure
				config.SSHKeys = strings.TrimSpace(strings.Join(sshKeys, "\n"))
				// DEBUG: Log final SSH keys value
				slog.Info("DEBUG SSH after join", "location", "server.go", "len", len(config.SSHKeys), "repr", config.SSHKeys)
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

// buildNetworkString constructs network configuration string for Proxmox
func (p *Provider) buildNetworkString(netConfig pveapi.NetworkConfig) string {
	// Format: virtio,bridge=vmbr0[,tag=100][,firewall=1]
	result := fmt.Sprintf("%s,bridge=%s", netConfig.Model, netConfig.Bridge)
	if netConfig.VLAN > 0 {
		result += fmt.Sprintf(",tag=%d", netConfig.VLAN)
	}
	if netConfig.MAC != "" {
		result += fmt.Sprintf(",mac=%s", netConfig.MAC)
	}
	if netConfig.Firewall {
		result += ",firewall=1"
	}
	return result
}

// buildIPConfigString constructs IP configuration string for Proxmox
func (p *Provider) buildIPConfigString(ipConfig pveapi.IPConfig) string {
	if ipConfig.DHCP {
		return "ip=dhcp"
	}
	result := fmt.Sprintf("ip=%s", ipConfig.IP)
	if ipConfig.Gateway != "" {
		result += fmt.Sprintf(",gw=%s", ipConfig.Gateway)
	}
	return result
}

// GetDiskInfo retrieves detailed information about a VM's disk
func (p *Provider) GetDiskInfo(ctx context.Context, req *providerv1.GetDiskInfoRequest) (*providerv1.GetDiskInfoResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	p.logger.Info("Getting disk info", "vm_id", req.VmId)

	// Parse VM reference
	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// Get VM configuration
	config, err := p.client.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return nil, errors.NewInternal("failed to get VM config: %v", err)
	}

	// Get VM info for additional details
	vmInfo, err := p.client.GetVM(ctx, node, vmid)
	if err != nil {
		p.logger.Warn("Failed to get VM info", "error", err)
	}

	// Find primary disk (scsi0, virtio0, sata0, or ide0)
	diskID := "scsi0" // default
	if req.DiskId != "" {
		diskID = req.DiskId
	}

	// Get disk config
	var diskInfo string
	var found bool
	if val, ok := config[diskID]; ok {
		if strVal, ok := val.(string); ok {
			diskInfo = strVal
			found = true
		}
	}

	if !found {
		return nil, errors.NewNotFound("disk %s not found", diskID)
	}

	// Parse disk info (format: "local-lvm:vm-100-disk-0,size=32G")
	parts := strings.Split(diskInfo, ",")
	var storagePath, size string
	if len(parts) > 0 {
		storagePath = parts[0]
	}
	for _, part := range parts {
		if strings.HasPrefix(part, "size=") {
			size = strings.TrimPrefix(part, "size=")
		}
	}

	// Parse size to bytes
	virtualSizeBytes, err := p.parseDiskSize(size)
	if err != nil {
		p.logger.Warn("Failed to parse disk size", "size", size, "error", err)
		virtualSizeBytes = 0
	}

	// Actual size is same as virtual size for Proxmox (thin provisioning)
	// Can be refined later by querying actual disk usage from storage API
	actualSizeBytes := virtualSizeBytes
	_ = vmInfo // Use vmInfo to avoid unused variable warning

	// Determine format from storage type
	format := "qcow2" // default for Proxmox
	if strings.Contains(storagePath, "raw") || strings.Contains(diskInfo, "raw") {
		format = "raw"
	}

	// Get snapshots
	snapshotList, err := p.client.ListSnapshots(ctx, node, vmid)
	if err != nil {
		p.logger.Warn("Failed to list snapshots", "error", err)
		snapshotList = []*pveapi.Snapshot{}
	}

	// Convert snapshots to string list
	snapshots := make([]string, 0, len(snapshotList))
	for _, snap := range snapshotList {
		snapshots = append(snapshots, snap.Name)
	}

	response := &providerv1.GetDiskInfoResponse{
		DiskId:           diskID,
		Format:           format,
		VirtualSizeBytes: virtualSizeBytes,
		ActualSizeBytes:  actualSizeBytes,
		Path:             storagePath,
		IsBootable:       true, // Primary disk is always bootable
		Snapshots:        snapshots,
		BackingFile:      "",
		Metadata: map[string]string{
			"node":    node,
			"vmid":    fmt.Sprintf("%d", vmid),
			"storage": storagePath,
		},
	}

	p.logger.Info("Disk info retrieved", "disk_id", diskID, "format", format, "size_bytes", virtualSizeBytes)
	return response, nil
}

// ExportDisk exports a VM disk for migration
func (p *Provider) ExportDisk(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	p.logger.Info("Exporting disk", "vm_id", req.VmId, "destination", req.DestinationUrl)

	// Parse VM reference
	vmid, node, err := p.parseVMReference(req.VmId)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid VM reference: %v", err)
	}

	// Get disk information first
	diskInfo, err := p.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return nil, errors.NewInternal("failed to get disk info: %v", err)
	}

	// Validate format compatibility
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = diskInfo.Format
	}
	if targetFormat != "qcow2" && targetFormat != "raw" && targetFormat != "vmdk" {
		return nil, errors.NewInvalidSpec("unsupported export format: %s", targetFormat)
	}

	exportID := fmt.Sprintf("export-%d-%d", vmid, time.Now().Unix())

	// Proxmox disk export strategy:
	// 1. Use vzdump to create a backup (VMA format)
	// 2. Extract the disk from VMA archive
	// 3. Convert if necessary
	// 4. Upload to destination

	p.logger.Info("Creating backup for disk export", "vmid", vmid, "node", node)

	// For now, we'll use a simplified approach:
	// - Stop the VM if required
	// - Use qemu-img to convert/copy the disk
	// - Calculate checksum
	// Note: Full implementation would use vzdump and handle running VMs

	p.logger.Warn("Using simplified disk export (not using vzdump)")
	p.logger.Info("Note: For production, implement vzdump-based export for running VMs")

	// Parse storage path (format: "storage:vm-100-disk-0")
	storageParts := strings.Split(diskInfo.Path, ":")
	if len(storageParts) != 2 {
		return nil, errors.NewInvalidSpec("invalid disk path format: %s", diskInfo.Path)
	}

	// Parse storage URL and create storage client
	parsedURL, err := storage.ParseStorageURL(req.DestinationUrl)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid destination URL: %v", err)
	}

	// Build storage config
	storageConfig := storage.StorageConfig{
		Type:    parsedURL.Type,
		Timeout: 300,
		UseSSL:  true,
	}

	// Apply credentials
	if req.Credentials != nil {
		if accessKey, ok := req.Credentials["accessKey"]; ok {
			storageConfig.AccessKey = accessKey
		}
		if secretKey, ok := req.Credentials["secretKey"]; ok {
			storageConfig.SecretKey = secretKey
		}
		if token, ok := req.Credentials["token"]; ok {
			storageConfig.Token = token
		}
		if endpoint, ok := req.Credentials["endpoint"]; ok {
			storageConfig.Endpoint = endpoint
		}
		if region, ok := req.Credentials["region"]; ok {
			storageConfig.Region = region
		}
	}

	// Configure based on storage type
	switch parsedURL.Type {
	case "s3":
		storageConfig.Bucket = parsedURL.Bucket
		if storageConfig.Region == "" {
			storageConfig.Region = "us-east-1"
		}
	case "http", "https":
		if storageConfig.Endpoint == "" {
			storageConfig.Endpoint = parsedURL.Endpoint
		}
	case "nfs":
		if storageConfig.Endpoint == "" {
			storageConfig.Endpoint = parsedURL.Path
		}
	}

	// Create storage client
	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create storage client: %v", err)
	}
	defer storageClient.Close()

	// Create storage manager for Proxmox disk access
	storageManager := NewStorageManager(p.client, node)

	// Create temporary file for download
	pveStorage := storageParts[0]
	volid := storageParts[1]
	tempFile := fmt.Sprintf("/tmp/%s", exportID)
	defer func() {
		_ = os.Remove(tempFile)
	}()

	// Download from Proxmox storage
	p.logger.Info("Downloading disk from Proxmox storage", "storage", pveStorage, "volid", volid)
	
	file, err := os.Create(tempFile)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp file: %v", err)
	}
	defer file.Close()

	// Download with progress tracking
	downloadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Download progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	err = storageManager.DownloadVolume(ctx, pveStorage, volid, file, downloadProgress)
	if err != nil {
		return nil, errors.NewInternal("failed to download disk from Proxmox storage: %v", err)
	}

	// Close file to flush writes
	file.Close()

	// Convert format if needed
	var uploadPath string
	if targetFormat != diskInfo.Format {
		p.logger.Info("Converting disk format", "from", diskInfo.Format, "to", targetFormat)
		convertedPath := fmt.Sprintf("/tmp/%s-converted.%s", exportID, targetFormat)
		
		// TODO: Add qemu-img conversion
		p.logger.Warn("Format conversion not yet implemented, uploading original format")
		uploadPath = tempFile
		defer os.Remove(convertedPath)
	} else {
		uploadPath = tempFile
	}

	// Upload to destination storage
	p.logger.Info("Uploading disk to storage", "destination", req.DestinationUrl)
	
	// Re-open file for reading
	uploadFile, err := os.Open(uploadPath)
	if err != nil {
		return nil, errors.NewInternal("failed to open file for upload: %v", err)
	}
	defer uploadFile.Close()

	// Get file size
	stat, err := uploadFile.Stat()
	if err != nil {
		return nil, errors.NewInternal("failed to stat file: %v", err)
	}

	// Upload with progress tracking
	uploadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Upload progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	uploadReq := storage.UploadRequest{
		Reader:           uploadFile,
		DestinationURL:   req.DestinationUrl,
		ContentLength:    stat.Size(),
		ProgressCallback: uploadProgress,
	}

	uploadResp, err := storageClient.Upload(ctx, uploadReq)
	if err != nil {
		return nil, errors.NewInternal("failed to upload disk: %v", err)
	}

	p.logger.Info("Disk export completed", "export_id", exportID, "checksum", uploadResp.Checksum, "bytes", uploadResp.BytesTransferred)

	response := &providerv1.ExportDiskResponse{
		ExportId:           exportID,
		Task:               nil, // Synchronous operation
		EstimatedSizeBytes: uploadResp.BytesTransferred,
		Checksum:           uploadResp.Checksum,
	}

	return response, nil
}

// ImportDisk imports a disk from an external source
func (p *Provider) ImportDisk(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("PVE client not configured", nil)
	}

	p.logger.Info("Importing disk", "source", req.SourceUrl, "storage", req.StorageHint)

	// Find appropriate node
	node, err := p.client.FindNode(ctx)
	if err != nil {
		return nil, errors.NewInternal("failed to find node: %v", err)
	}

	// Determine storage
	pveStorage := "local-lvm"
	if req.StorageHint != "" {
		pveStorage = req.StorageHint
	}

	// Determine target format
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = "qcow2" // Default
	}
	if targetFormat != "qcow2" && targetFormat != "raw" && targetFormat != "vmdk" {
		return nil, errors.NewInvalidSpec("unsupported import format: %s", targetFormat)
	}

	// Generate disk ID and VMID for import
	diskID := req.TargetName
	if diskID == "" {
		diskID = fmt.Sprintf("imported-%d", time.Now().Unix())
	}

	// Generate a temporary VMID for qm importdisk
	tempVMID := int(time.Now().Unix()%999999) + 100000

	p.logger.Info("Preparing disk import", "disk_id", diskID, "node", node, "storage", pveStorage, "format", targetFormat)

	// Proxmox disk import strategy:
	// 1. Download disk from SourceURL to temp location
	// 2. Verify checksum if requested
	// 3. Use `qm importdisk` to import into Proxmox storage
	// 4. Return the imported disk reference

	// Parse storage URL and create storage client
	parsedURL, err := storage.ParseStorageURL(req.SourceUrl)
	if err != nil {
		return nil, errors.NewInvalidSpec("invalid source URL: %v", err)
	}

	// Build storage config
	storageConfig := storage.StorageConfig{
		Type:    parsedURL.Type,
		Timeout: 300,
		UseSSL:  true,
	}

	// Apply credentials
	if req.Credentials != nil {
		if accessKey, ok := req.Credentials["accessKey"]; ok {
			storageConfig.AccessKey = accessKey
		}
		if secretKey, ok := req.Credentials["secretKey"]; ok {
			storageConfig.SecretKey = secretKey
		}
		if token, ok := req.Credentials["token"]; ok {
			storageConfig.Token = token
		}
		if endpoint, ok := req.Credentials["endpoint"]; ok {
			storageConfig.Endpoint = endpoint
		}
		if region, ok := req.Credentials["region"]; ok {
			storageConfig.Region = region
		}
	}

	// Configure based on storage type
	switch parsedURL.Type {
	case "s3":
		storageConfig.Bucket = parsedURL.Bucket
		if storageConfig.Region == "" {
			storageConfig.Region = "us-east-1"
		}
	case "http", "https":
		if storageConfig.Endpoint == "" {
			storageConfig.Endpoint = parsedURL.Endpoint
		}
	case "nfs":
		if storageConfig.Endpoint == "" {
			storageConfig.Endpoint = parsedURL.Path
		}
	}

	// Create storage client
	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create storage client: %v", err)
	}
	defer storageClient.Close()

	// Create storage manager for Proxmox disk upload
	storageManager := NewStorageManager(p.client, node)

	// Create temporary file for download
	tempFile := fmt.Sprintf("/tmp/%s-import", diskID)
	defer func() {
		_ = os.Remove(tempFile)
	}()

	// Download from storage
	p.logger.Info("Downloading disk from storage", "source", req.SourceUrl)
	
	file, err := os.Create(tempFile)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp file: %v", err)
	}
	defer file.Close()

	// Download with progress tracking
	downloadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Download progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	downloadReq := storage.DownloadRequest{
		SourceURL:        req.SourceUrl,
		Writer:           file,
		VerifyChecksum:   req.VerifyChecksum,
		ExpectedChecksum: req.ExpectedChecksum,
		ProgressCallback: downloadProgress,
	}

	downloadResp, err := storageClient.Download(ctx, downloadReq)
	if err != nil {
		return nil, errors.NewInternal("failed to download disk: %v", err)
	}

	// Close file to flush writes
	file.Close()

	p.logger.Info("Download completed", "bytes", downloadResp.BytesTransferred, "checksum", downloadResp.Checksum)

	// Convert to target format if needed
	var importPath string
	if targetFormat != "qcow2" {
		p.logger.Info("Converting to target format", "target_format", targetFormat)
		convertedPath := fmt.Sprintf("/tmp/%s-converted.%s", diskID, targetFormat)
		
		// TODO: Add qemu-img conversion
		p.logger.Warn("Format conversion not yet implemented, using downloaded format")
		importPath = tempFile
		defer os.Remove(convertedPath)
	} else {
		importPath = tempFile
	}

	// Upload to Proxmox storage
	p.logger.Info("Uploading to Proxmox storage", "storage", pveStorage, "disk_id", diskID)
	
	// Re-open file for reading
	uploadFile, err := os.Open(importPath)
	if err != nil {
		return nil, errors.NewInternal("failed to open file for Proxmox upload: %v", err)
	}
	defer uploadFile.Close()

	// Get file size
	stat, err := uploadFile.Stat()
	if err != nil {
		return nil, errors.NewInternal("failed to stat file: %v", err)
	}

	// Generate filename for Proxmox storage
	filename := fmt.Sprintf("%d/vm-%d-disk-0.%s", tempVMID, tempVMID, targetFormat)
	
	// Upload to Proxmox storage with progress tracking
	uploadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Proxmox upload progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	volid, err := storageManager.UploadVolume(ctx, uploadFile, pveStorage, filename, stat.Size(), uploadProgress)
	if err != nil {
		return nil, errors.NewInternal("failed to upload to Proxmox storage: %v", err)
	}

	p.logger.Info("Disk import completed", "disk_id", diskID, "volid", volid)

	response := &providerv1.ImportDiskResponse{
		DiskId:          diskID,
		Path:            volid,
		Task:            nil, // Synchronous operation
		ActualSizeBytes: downloadResp.ContentLength,
		Checksum:        downloadResp.Checksum,
	}

	return response, nil
}

// parseDiskSize converts Proxmox disk size string (e.g., "32G", "1024M") to bytes
func (p *Provider) parseDiskSize(sizeStr string) (int64, error) {
	if sizeStr == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// Remove trailing spaces
	sizeStr = strings.TrimSpace(sizeStr)

	// Extract number and unit
	var value float64
	var unit string

	// Try to parse with scanf
	n, err := fmt.Sscanf(sizeStr, "%f%s", &value, &unit)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid size format: %s", sizeStr)
	}

	// Default to bytes if no unit
	if n == 1 {
		return int64(value), nil
	}

	// Convert to bytes based on unit
	switch strings.ToUpper(unit) {
	case "B":
		return int64(value), nil
	case "K", "KB", "KIB":
		return int64(value * 1024), nil
	case "M", "MB", "MIB":
		return int64(value * 1024 * 1024), nil
	case "G", "GB", "GIB":
		return int64(value * 1024 * 1024 * 1024), nil
	case "T", "TB", "TIB":
		return int64(value * 1024 * 1024 * 1024 * 1024), nil
	default:
		return 0, fmt.Errorf("unknown unit: %s", unit)
	}
}
