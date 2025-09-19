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
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/sdk/provider/errors"
)

const (
	// CredentialsPath is where the controller mounts the credentials secret
	CredentialsPath = "/etc/virtrigaud/credentials"
)

// Provider implements the vSphere provider using the SDK pattern
type Provider struct {
	providerv1.UnimplementedProviderServer
	client *govmomi.Client
	finder *find.Finder
	logger *slog.Logger
	config *Config
}

// Config holds the vSphere provider configuration
type Config struct {
	Endpoint           string
	Username           string
	Password           string
	InsecureSkipVerify bool
	// Provider defaults from CRD
	DefaultDatastore string
	DefaultCluster   string
	DefaultFolder    string
}

// New creates a new vSphere provider that reads configuration from environment and mounted secrets
func New() *Provider {
	// Load configuration from environment (set by provider controller)
	config := &Config{
		Endpoint:           os.Getenv("PROVIDER_ENDPOINT"),
		InsecureSkipVerify: os.Getenv("TLS_INSECURE_SKIP_VERIFY") == "true", // Allow skipping TLS verification
		// Provider defaults - these should be set by the provider controller from CRD spec.defaults
		DefaultDatastore: getEnvOrDefault("PROVIDER_DEFAULT_DATASTORE", "datastore1"),
		DefaultCluster:   getEnvOrDefault("PROVIDER_DEFAULT_CLUSTER", "cluster01"),
		DefaultFolder:    getEnvOrDefault("PROVIDER_DEFAULT_FOLDER", "research-vms"),
	}

	// Load credentials from mounted secret files
	if err := loadCredentialsFromFiles(config); err != nil {
		slog.Error("Failed to load credentials from mounted secret", "error", err)
	}

	// Create vSphere client
	client, finder, err := createVSphereClient(config)
	if err != nil {
		// Log error but continue - validation will catch connection issues
		slog.Error("Failed to create vSphere client", "error", err)
	}

	return &Provider{
		config: config,
		client: client,
		finder: finder,
		logger: slog.Default(),
	}
}

// getEnvOrDefault returns environment variable value or default if not set
func getEnvOrDefault(envVar, defaultValue string) string {
	if value := os.Getenv(envVar); value != "" {
		return value
	}
	return defaultValue
}

// loadCredentialsFromFiles reads credentials from mounted secret files
func loadCredentialsFromFiles(config *Config) error {
	// Read username from mounted secret
	if data, err := os.ReadFile(CredentialsPath + "/username"); err == nil {
		config.Username = strings.TrimSpace(string(data))
	} else {
		return fmt.Errorf("failed to read username from %s/username: %w", CredentialsPath, err)
	}

	// Read password from mounted secret
	if data, err := os.ReadFile(CredentialsPath + "/password"); err == nil {
		config.Password = strings.TrimSpace(string(data))
	} else {
		return fmt.Errorf("failed to read password from %s/password: %w", CredentialsPath, err)
	}

	return nil
}

// createVSphereClient creates a govmomi client and finder from the configuration
func createVSphereClient(config *Config) (*govmomi.Client, *find.Finder, error) {
	if config.Endpoint == "" {
		return nil, nil, fmt.Errorf("PROVIDER_ENDPOINT environment variable is required")
	}

	if config.Username == "" || config.Password == "" {
		return nil, nil, fmt.Errorf("username and password are required in mounted credentials secret")
	}

	// Parse the endpoint URL (without embedding credentials to avoid special character issues)
	u, err := url.Parse(config.Endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid vSphere endpoint URL: %w", err)
	}

	// Create SOAP client without credentials in URL
	soapClient := soap.NewClient(u, config.InsecureSkipVerify)

	// Configure TLS if needed
	if !config.InsecureSkipVerify {
		soapClient.DefaultTransport().TLSClientConfig = &tls.Config{
			ServerName: u.Hostname(),
		}
	}

	// Create vSphere client
	vimClient, err := vim25.NewClient(context.Background(), soapClient)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create vSphere VIM client: %w", err)
	}

	client := &govmomi.Client{
		Client:         vimClient,
		SessionManager: session.NewManager(vimClient),
	}

	// Login to vSphere with explicit credentials (proper govmomi authentication method)
	userInfo := url.UserPassword(config.Username, config.Password)
	if err := client.Login(context.Background(), userInfo); err != nil {
		return nil, nil, fmt.Errorf("failed to login to vSphere: %w", err)
	}

	// Create finder for inventory navigation
	finder := find.NewFinder(client.Client, true)

	return client, finder, nil
}

// Validate validates the provider configuration and connectivity
func (p *Provider) Validate(ctx context.Context, req *providerv1.ValidateRequest) (*providerv1.ValidateResponse, error) {
	if p.client == nil {
		return &providerv1.ValidateResponse{
			Ok:      false,
			Message: "vSphere client not configured",
		}, nil
	}

	// Test the connection by checking if the session is valid
	if !p.client.Valid() {
		// Try to reconnect
		client, finder, err := createVSphereClient(p.config)
		if err != nil {
			return &providerv1.ValidateResponse{
				Ok:      false,
				Message: fmt.Sprintf("Failed to connect to vSphere: %v", err),
			}, nil
		}
		p.client = client
		p.finder = finder
	}

	return &providerv1.ValidateResponse{
		Ok:      true,
		Message: "vSphere provider is ready",
	}, nil
}

// GetCapabilities returns the provider's capabilities
func (p *Provider) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{
		SupportsReconfigureOnline:   true,
		SupportsDiskExpansionOnline: true,
		SupportsSnapshots:           true,
		SupportsMemorySnapshots:     false, // vSphere snapshots don't include memory by default
		SupportsLinkedClones:        true,
		SupportsImageImport:         true,
		SupportedDiskTypes:          []string{"thin", "thick", "eager-zeroed"},
		SupportedNetworkTypes:       []string{"standard", "distributed"},
	}, nil
}

// Create creates a new virtual machine
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	// Parse the JSON specifications to understand what to create
	vmSpec, err := p.parseCreateRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to parse create request: %w", err)
	}

	// Create the VM using govmomi
	vmID, err := p.createVirtualMachine(ctx, vmSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual machine: %w", err)
	}

	return &providerv1.CreateResponse{
		Id: vmID,
		// No task reference for now - synchronous operation
	}, nil
}

// Delete deletes a virtual machine
func (p *Provider) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Deleting virtual machine", "vm_id", req.Id)

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Create VM reference from ID
	vmRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.Id,
	}

	vm := object.NewVirtualMachine(p.client.Client, vmRef)

	// First, check if the VM exists by getting its power state
	powerState, err := vm.PowerState(ctx)
	if err != nil {
		// Check if this is a "not found" error
		if soap.IsSoapFault(err) {
			soapFault := soap.ToSoapFault(err)
			if soapFault.VimFault() != nil {
				// VM doesn't exist - this is not an error for deletion
				p.logger.Info("VM does not exist, deletion complete", "vm_id", req.Id)
				return &providerv1.TaskResponse{}, nil
			}
		}
		return nil, fmt.Errorf("failed to check VM power state: %w", err)
	}

	p.logger.Info("VM found, proceeding with deletion", "vm_id", req.Id, "power_state", powerState)

	// If VM is powered on, power it off first
	if powerState == types.VirtualMachinePowerStatePoweredOn {
		p.logger.Info("Powering off VM before deletion", "vm_id", req.Id)

		powerOffTask, err := vm.PowerOff(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to start power off operation: %w", err)
		}

		// Wait for power off to complete
		_, err = powerOffTask.WaitForResult(ctx, nil)
		if err != nil {
			p.logger.Warn("Power off failed, continuing with deletion", "vm_id", req.Id, "error", err)
			// Continue with deletion even if power off fails
		} else {
			p.logger.Info("VM powered off successfully", "vm_id", req.Id)
		}
	}

	// Delete the VM from disk (this removes it from inventory and deletes files)
	p.logger.Info("Deleting VM from disk", "vm_id", req.Id)

	deleteTask, err := vm.Destroy(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start VM deletion: %w", err)
	}

	// Wait for deletion to complete
	_, err = deleteTask.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("VM deletion failed: %w", err)
	}

	p.logger.Info("Virtual machine deleted successfully", "vm_id", req.Id)

	// Return empty task response since we completed synchronously
	return &providerv1.TaskResponse{}, nil
}

// Power performs power operations on a virtual machine
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Performing power operation", "vm_id", req.Id, "operation", req.Op.String())

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Create VM reference from ID
	vmRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.Id,
	}

	vm := object.NewVirtualMachine(p.client.Client, vmRef)

	// Perform the power operation
	var task *object.Task
	switch req.Op {
	case providerv1.PowerOp_POWER_OP_ON:
		task, err = vm.PowerOn(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to start power on task: %w", err)
		}
	case providerv1.PowerOp_POWER_OP_OFF:
		task, err = vm.PowerOff(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to start power off task: %w", err)
		}
	case providerv1.PowerOp_POWER_OP_REBOOT:
		// For reboot, we need to restart the guest OS
		err = vm.RebootGuest(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to reboot guest: %w", err)
		}
		// RebootGuest is synchronous, so we don't need to wait for a task
		p.logger.Info("Power operation completed successfully", "vm_id", req.Id, "operation", req.Op.String())
		return &providerv1.TaskResponse{}, nil
	case providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
		// Graceful shutdown using guest tools
		return p.performGracefulShutdown(ctx, vm, req)
	default:
		return nil, fmt.Errorf("unsupported power operation: %s", req.Op.String())
	}

	// For now, wait for the task to complete (synchronous operation)
	// In a real implementation, you might want to return the task reference for async tracking
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("power operation failed: %w", err)
	}

	p.logger.Info("Power operation completed successfully", "vm_id", req.Id, "operation", req.Op.String())

	// Return empty task response since we completed synchronously
	return &providerv1.TaskResponse{}, nil
}

// performGracefulShutdown performs a graceful shutdown using VMware guest tools
func (p *Provider) performGracefulShutdown(ctx context.Context, vm *object.VirtualMachine, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	// Default timeout if not specified
	gracefulTimeout := 60 * time.Second
	if req.GracefulTimeoutSeconds > 0 {
		gracefulTimeout = time.Duration(req.GracefulTimeoutSeconds) * time.Second
	}

	p.logger.Info("Attempting graceful shutdown", "vm_id", req.Id, "timeout_seconds", int(gracefulTimeout.Seconds()))

	// Check if VMware Tools is running
	toolsStatus, err := p.getVMwareToolsStatus(ctx, vm)
	if err != nil {
		p.logger.Warn("Failed to check VMware Tools status, falling back to power off", "vm_id", req.Id, "error", err)
		return p.fallbackToPowerOff(ctx, vm, req.Id)
	}

	if toolsStatus != "toolsOk" && toolsStatus != "toolsOld" {
		p.logger.Warn("VMware Tools not available for graceful shutdown, falling back to power off",
			"vm_id", req.Id, "tools_status", toolsStatus)
		return p.fallbackToPowerOff(ctx, vm, req.Id)
	}

	// Create a context with timeout for the graceful shutdown attempt
	shutdownCtx, cancel := context.WithTimeout(ctx, gracefulTimeout)
	defer cancel()

	// Attempt graceful shutdown using guest tools
	p.logger.Info("Initiating graceful shutdown using VMware Tools", "vm_id", req.Id)
	err = vm.ShutdownGuest(shutdownCtx)
	if err != nil {
		p.logger.Warn("Graceful shutdown failed, falling back to power off", "vm_id", req.Id, "error", err)
		return p.fallbackToPowerOff(ctx, vm, req.Id)
	}

	// Monitor shutdown progress
	return p.waitForGracefulShutdown(ctx, vm, req.Id, gracefulTimeout)
}

// getVMwareToolsStatus checks the status of VMware Tools on the VM
func (p *Provider) getVMwareToolsStatus(ctx context.Context, vm *object.VirtualMachine) (string, error) {
	var vmObj mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"guest.toolsStatus"}, &vmObj)
	if err != nil {
		return "", fmt.Errorf("failed to get VM properties: %w", err)
	}

	if vmObj.Guest == nil {
		return "toolsNotInstalled", nil
	}

	return string(vmObj.Guest.ToolsStatus), nil
}

// waitForGracefulShutdown waits for the VM to shut down gracefully within the timeout
func (p *Provider) waitForGracefulShutdown(ctx context.Context, vm *object.VirtualMachine, vmID string, timeout time.Duration) (*providerv1.TaskResponse, error) {
	p.logger.Info("Waiting for graceful shutdown to complete", "vm_id", vmID, "timeout", timeout)

	// Create a context with timeout
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Poll the power state until shutdown or timeout
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			// Timeout reached, fall back to hard power off
			p.logger.Warn("Graceful shutdown timeout reached, falling back to power off", "vm_id", vmID)
			return p.fallbackToPowerOff(ctx, vm, vmID)

		case <-ticker.C:
			powerState, err := vm.PowerState(ctx)
			if err != nil {
				p.logger.Error("Failed to check power state during graceful shutdown", "vm_id", vmID, "error", err)
				continue
			}

			if powerState == types.VirtualMachinePowerStatePoweredOff {
				p.logger.Info("Graceful shutdown completed successfully", "vm_id", vmID)
				return &providerv1.TaskResponse{}, nil
			}

			p.logger.Debug("VM still shutting down", "vm_id", vmID, "power_state", powerState)
		}
	}
}

// fallbackToPowerOff performs a hard power off when graceful shutdown fails
func (p *Provider) fallbackToPowerOff(ctx context.Context, vm *object.VirtualMachine, vmID string) (*providerv1.TaskResponse, error) {
	p.logger.Info("Performing hard power off", "vm_id", vmID)

	task, err := vm.PowerOff(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start power off task: %w", err)
	}

	// Wait for power off to complete
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("power off operation failed: %w", err)
	}

	p.logger.Info("Hard power off completed successfully", "vm_id", vmID)
	return &providerv1.TaskResponse{}, nil
}

// Reconfigure reconfigures a virtual machine
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("Reconfigure operation not yet implemented for vSphere")
}

// Describe retrieves virtual machine information
func (p *Provider) Describe(ctx context.Context, req *providerv1.DescribeRequest) (*providerv1.DescribeResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Describing virtual machine", "vm_id", req.Id)

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Try to find the VM by managed object ID
	vmRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.Id,
	}

	// VM object will be used for property retrieval

	// Get VM properties - comprehensive list for detailed monitoring
	var vmMo mo.VirtualMachine
	pc := property.DefaultCollector(p.client.Client)
	err = pc.RetrieveOne(ctx, vmRef, []string{
		// Power and runtime state
		"runtime.powerState",
		"runtime.connectionState",
		"runtime.bootTime",
		"summary.runtime.powerState",
		"summary.runtime.connectionState",

		// Guest information
		"guest.ipAddress",
		"guest.net",
		"guest.guestState",
		"guest.toolsStatus",
		"guest.toolsVersion",
		"guest.guestFullName",
		"guest.hostName",

		// Configuration
		"summary.config.name",
		"summary.config.numCpu",
		"summary.config.memorySizeMB",
		"summary.config.vmPathName",
		"summary.config.guestFullName",
		"summary.config.annotation",

		// Hardware and performance
		"summary.quickStats.overallCpuUsage",
		"summary.quickStats.guestMemoryUsage",
		"summary.quickStats.hostMemoryUsage",
		"summary.quickStats.uptimeSeconds",

		// Network details
		"network",
		"summary.runtime.host",
	}, &vmMo)

	if err != nil {
		// VM might not exist or be accessible
		p.logger.Warn("Failed to retrieve VM properties", "vm_id", req.Id, "error", err)
		return &providerv1.DescribeResponse{
			Exists: false,
		}, nil
	}

	// VM exists, gather comprehensive information
	powerState := p.mapVSpherePowerState(string(vmMo.Runtime.PowerState))
	connectionState := string(vmMo.Runtime.ConnectionState)

	// Collect IP addresses with enhanced detection
	var ips []string
	var primaryIP string

	if vmMo.Guest != nil {
		// Primary IP address
		if vmMo.Guest.IpAddress != "" {
			primaryIP = vmMo.Guest.IpAddress
			ips = append(ips, vmMo.Guest.IpAddress)
		}

		// Additional IPs from guest networks - filter out link-local and loopback
		if vmMo.Guest.Net != nil {
			for _, netInfo := range vmMo.Guest.Net {
				if netInfo.IpConfig != nil {
					for _, ipConfig := range netInfo.IpConfig.IpAddress {
						ip := ipConfig.IpAddress
						if ip != "" && !contains(ips, ip) && p.isValidIPAddress(ip) {
							ips = append(ips, ip)
						}
					}
				}
			}
		}
	}

	// Get guest tools status
	toolsStatus := ""
	toolsVersion := ""
	guestOS := ""
	hostname := ""

	if vmMo.Guest != nil {
		if vmMo.Guest.ToolsStatus != "" {
			toolsStatus = string(vmMo.Guest.ToolsStatus)
		}
		if vmMo.Guest.ToolsVersion != "" {
			toolsVersion = vmMo.Guest.ToolsVersion
		}
		if vmMo.Guest.GuestFullName != "" {
			guestOS = vmMo.Guest.GuestFullName
		}
		if vmMo.Guest.HostName != "" {
			hostname = vmMo.Guest.HostName
		}
	}

	// Get resource information (handle potential nil values safely)
	cpuCount := int32(0)
	memoryMB := int32(0)
	cpuUsage := int32(0)
	memoryUsage := int32(0)
	uptimeSeconds := int64(0)

	// Summary.Config and Summary.QuickStats are structs, not pointers
	cpuCount = vmMo.Summary.Config.NumCpu
	memoryMB = vmMo.Summary.Config.MemorySizeMB

	cpuUsage = vmMo.Summary.QuickStats.OverallCpuUsage
	memoryUsage = vmMo.Summary.QuickStats.GuestMemoryUsage
	uptimeSeconds = int64(vmMo.Summary.QuickStats.UptimeSeconds)

	// Boot time
	bootTime := ""
	if vmMo.Runtime.BootTime != nil {
		bootTime = vmMo.Runtime.BootTime.Format("2006-01-02T15:04:05Z")
	}

	// Create comprehensive provider raw JSON with detailed VM info
	providerRawJson := fmt.Sprintf(`{
		"vm_id": "%s",
		"name": "%s",
		"power_state": "%s",
		"connection_state": "%s",
		"primary_ip": "%s",
		"hostname": "%s",
		"guest_os": "%s",
		"tools_status": "%s",
		"tools_version": "%s",
		"cpu_count": %d,
		"memory_mb": %d,
		"cpu_usage_mhz": %d,
		"memory_usage_mb": %d,
		"uptime_seconds": %d,
		"boot_time": "%s"
	}`, req.Id,
		vmMo.Summary.Config.Name,
		powerState,
		connectionState,
		primaryIP,
		hostname,
		guestOS,
		toolsStatus,
		toolsVersion,
		cpuCount,
		memoryMB,
		cpuUsage,
		memoryUsage,
		uptimeSeconds,
		bootTime)

	return &providerv1.DescribeResponse{
		Exists:          true,
		PowerState:      powerState,
		Ips:             ips,
		ConsoleUrl:      "", // TODO: Generate console URL if needed
		ProviderRawJson: providerRawJson,
	}, nil
}

// contains checks if a string slice contains a specific string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// isValidIPAddress filters out unwanted IP addresses (loopback, link-local, etc.)
func (p *Provider) isValidIPAddress(ip string) bool {
	// Filter out localhost and link-local addresses
	if ip == "127.0.0.1" || ip == "::1" ||
		strings.HasPrefix(ip, "169.254.") || // Link-local IPv4
		strings.HasPrefix(ip, "fe80:") { // Link-local IPv6
		return false
	}
	return true
}

// mapVSpherePowerState maps vSphere power states to VirtRigaud standard power states
func (p *Provider) mapVSpherePowerState(vspherePowerState string) string {
	switch vspherePowerState {
	case "poweredOn":
		return "On"
	case "poweredOff":
		return "Off"
	case "suspended":
		return "Off" // Treat suspended as Off for VirtRigaud
	default:
		return "Off" // Default to Off for unknown states
	}
}

// addCloudInitToConfigSpec adds cloud-init data to VM configuration via guestinfo properties
func (p *Provider) addCloudInitToConfigSpec(configSpec *types.VirtualMachineConfigSpec, cloudInitData string) error {
	// VMware cloud-init datasource reads from guestinfo properties
	// This is the standard way to pass cloud-init data to VMs in vSphere

	// Encode cloud-init data (base64 encoding is common but not required)
	// We'll pass it directly as it's already in YAML format

	// Create extra config options for cloud-init
	extraConfig := []types.BaseOptionValue{
		&types.OptionValue{
			Key:   "guestinfo.userdata",
			Value: cloudInitData,
		},
		&types.OptionValue{
			Key:   "guestinfo.userdata.encoding",
			Value: "yaml", // Indicate this is YAML format
		},
		// Some cloud-init datasources also look for metadata
		&types.OptionValue{
			Key:   "guestinfo.metadata",
			Value: `{"instance-id": "` + configSpec.Name + `"}`,
		},
		&types.OptionValue{
			Key:   "guestinfo.metadata.encoding",
			Value: "json",
		},
	}

	// Add to existing extra config or create new
	if configSpec.ExtraConfig != nil {
		configSpec.ExtraConfig = append(configSpec.ExtraConfig, extraConfig...)
	} else {
		configSpec.ExtraConfig = extraConfig
	}

	return nil
}

// TaskStatus checks the status of an async task
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	return nil, errors.NewUnimplemented("TaskStatus operation not yet implemented for vSphere")
}

// SnapshotCreate creates a snapshot of a virtual machine
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	return nil, errors.NewUnimplemented("SnapshotCreate operation not yet implemented for vSphere")
}

// SnapshotDelete deletes a snapshot
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("SnapshotDelete operation not yet implemented for vSphere")
}

// SnapshotRevert reverts to a snapshot
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("SnapshotRevert operation not yet implemented for vSphere")
}

// Clone clones a virtual machine
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	return nil, errors.NewUnimplemented("Clone operation not yet implemented for vSphere")
}

// ImagePrepare prepares an image/template
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("ImagePrepare operation not yet implemented for vSphere")
}

// VMSpec represents the parsed virtual machine specification
type VMSpec struct {
	Name         string
	CPU          int32
	MemoryMB     int64
	DiskSizeGB   int64
	DiskType     string
	TemplateName string
	NetworkName  string
	Firmware     string
	CloudInit    string // Cloud-init user data
}

// parseCreateRequest parses the JSON-encoded specifications from the gRPC request
func (p *Provider) parseCreateRequest(req *providerv1.CreateRequest) (*VMSpec, error) {
	spec := &VMSpec{
		Name: req.Name,
	}

	// Parse VMClass from JSON (contracts.VMClass structure)
	if req.ClassJson != "" {
		var vmClass struct {
			CPU          int32  `json:"CPU"`
			MemoryMiB    int32  `json:"MemoryMiB"`
			Firmware     string `json:"Firmware"`
			DiskDefaults *struct {
				Type    string `json:"Type"`
				SizeGiB int32  `json:"SizeGiB"`
			} `json:"DiskDefaults"`
		}

		if err := json.Unmarshal([]byte(req.ClassJson), &vmClass); err != nil {
			return nil, fmt.Errorf("failed to parse VMClass JSON: %w", err)
		}

		spec.CPU = vmClass.CPU
		spec.MemoryMB = int64(vmClass.MemoryMiB) // Convert MiB to MB (same value)
		spec.Firmware = vmClass.Firmware

		if vmClass.DiskDefaults != nil {
			spec.DiskType = vmClass.DiskDefaults.Type
			spec.DiskSizeGB = int64(vmClass.DiskDefaults.SizeGiB) // Convert GiB to GB (same value)
		}
	}

	// Parse VMImage from JSON (contracts.VMImage structure)
	if req.ImageJson != "" {
		var vmImage struct {
			TemplateName string `json:"TemplateName"`
		}

		if err := json.Unmarshal([]byte(req.ImageJson), &vmImage); err != nil {
			return nil, fmt.Errorf("failed to parse VMImage JSON: %w", err)
		}

		spec.TemplateName = vmImage.TemplateName
	}

	// Parse Networks from JSON ([]contracts.NetworkAttachment structure)
	if req.NetworksJson != "" {
		var networks []struct {
			NetworkName string `json:"NetworkName"`
		}

		if err := json.Unmarshal([]byte(req.NetworksJson), &networks); err != nil {
			return nil, fmt.Errorf("failed to parse Networks JSON: %w", err)
		}

		if len(networks) > 0 {
			spec.NetworkName = networks[0].NetworkName
		}
	}

	// Parse UserData (cloud-init)
	if len(req.UserData) > 0 {
		spec.CloudInit = string(req.UserData)
	}

	return spec, nil
}

// createVirtualMachine creates a VM in vSphere using the parsed specification
func (p *Provider) createVirtualMachine(ctx context.Context, spec *VMSpec) (string, error) {
	p.logger.Info("Creating virtual machine",
		"name", spec.Name,
		"cpu", spec.CPU,
		"memory_mb", spec.MemoryMB,
		"disk_gb", spec.DiskSizeGB,
		"template", spec.TemplateName,
		"network", spec.NetworkName,
		"firmware", spec.Firmware,
	)

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Find the template VM
	template, err := p.finder.VirtualMachine(ctx, spec.TemplateName)
	if err != nil {
		return "", fmt.Errorf("failed to find template VM '%s': %w", spec.TemplateName, err)
	}

	// Find the cluster and resource pool
	cluster, err := p.finder.ClusterComputeResource(ctx, p.config.DefaultCluster)
	if err != nil {
		return "", fmt.Errorf("failed to find cluster '%s': %w", p.config.DefaultCluster, err)
	}

	resourcePool, err := cluster.ResourcePool(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get resource pool from cluster: %w", err)
	}

	// Find the datastore
	datastore, err := p.finder.Datastore(ctx, p.config.DefaultDatastore)
	if err != nil {
		return "", fmt.Errorf("failed to find datastore '%s': %w", p.config.DefaultDatastore, err)
	}

	// Find the folder
	folder, err := p.finder.Folder(ctx, p.config.DefaultFolder)
	if err != nil {
		// If folder doesn't exist, use the datacenter's default VM folder
		p.logger.Warn("Failed to find folder, using datacenter default VM folder", "folder", p.config.DefaultFolder, "error", err)
		folder, err = p.finder.Folder(ctx, datacenter.Name()+"/vm")
		if err != nil {
			return "", fmt.Errorf("failed to find datacenter VM folder: %w", err)
		}
	}

	// Find the network/portgroup
	var network object.NetworkReference
	if spec.NetworkName != "" {
		net, err := p.finder.Network(ctx, spec.NetworkName)
		if err != nil {
			return "", fmt.Errorf("failed to find network '%s': %w", spec.NetworkName, err)
		}
		network = net
	}

	// Create the clone specification
	cloneSpec := &types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Datastore: types.NewReference(datastore.Reference()),
			Pool:      types.NewReference(resourcePool.Reference()),
		},
		PowerOn:  false, // We'll power on separately if needed
		Template: false,
	}

	// Configure the VM specification for customization
	configSpec := &types.VirtualMachineConfigSpec{
		NumCPUs:  spec.CPU,
		MemoryMB: spec.MemoryMB,
	}

	// Set firmware if specified
	if spec.Firmware != "" {
		if strings.ToUpper(spec.Firmware) == "UEFI" {
			configSpec.Firmware = string(types.GuestOsDescriptorFirmwareTypeEfi)
		} else {
			configSpec.Firmware = string(types.GuestOsDescriptorFirmwareTypeBios)
		}
	}

	// Configure network if specified
	if network != nil {
		// Get the network reference
		networkRef := network.Reference()

		// Create network device configuration
		networkDevice := &types.VirtualVmxnet3{
			VirtualVmxnet: types.VirtualVmxnet{
				VirtualEthernetCard: types.VirtualEthernetCard{
					VirtualDevice: types.VirtualDevice{
						Key: -1, // Negative key for new device
						DeviceInfo: &types.Description{
							Label:   "Network adapter 1",
							Summary: spec.NetworkName,
						},
						Backing: &types.VirtualEthernetCardNetworkBackingInfo{
							VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
								DeviceName: spec.NetworkName,
							},
							Network: &networkRef,
						},
						Connectable: &types.VirtualDeviceConnectInfo{
							StartConnected:    true,
							AllowGuestControl: true,
							Connected:         true,
						},
					},
				},
			},
		}

		// Add network device to configuration
		configSpec.DeviceChange = []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    networkDevice,
			},
		}
	}

	cloneSpec.Config = configSpec

	// Add cloud-init data via guestinfo properties if provided
	if spec.CloudInit != "" {
		if err := p.addCloudInitToConfigSpec(configSpec, spec.CloudInit); err != nil {
			p.logger.Warn("Failed to add cloud-init configuration", "error", err)
			// Continue without cloud-init rather than failing
		} else {
			p.logger.Info("Added cloud-init configuration to VM", "vm_name", spec.Name)
		}
	}

	// Perform the clone operation
	p.logger.Info("Cloning virtual machine from template", "template", spec.TemplateName, "target", spec.Name)

	task, err := template.Clone(ctx, folder, spec.Name, *cloneSpec)
	if err != nil {
		return "", fmt.Errorf("failed to start clone operation: %w", err)
	}

	// Wait for the clone task to complete
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("clone task failed: %w", err)
	}

	// Get the new VM reference
	vmRef, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return "", fmt.Errorf("unexpected result type from clone task: %T", info.Result)
	}

	// Get the VM's managed object ID for returning
	vmID := vmRef.Value

	p.logger.Info("Virtual machine created successfully", "vm_id", vmID, "name", spec.Name)

	// Power on the VM if requested (VirtualMachine spec.powerState: "On")
	// Note: This is a simple implementation - in production you might want to check the actual powerState from the request
	newVM := object.NewVirtualMachine(p.client.Client, vmRef)
	powerTask, err := newVM.PowerOn(ctx)
	if err != nil {
		p.logger.Warn("Failed to power on VM after creation", "vm_id", vmID, "error", err)
		// Don't fail the entire creation if power on fails
	} else {
		_, err = powerTask.WaitForResult(ctx, nil)
		if err != nil {
			p.logger.Warn("VM power on task failed", "vm_id", vmID, "error", err)
		} else {
			p.logger.Info("VM powered on successfully", "vm_id", vmID)
		}
	}

	return vmID, nil
}
