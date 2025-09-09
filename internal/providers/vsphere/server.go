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
	"strconv"
	"strings"

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
	return nil, errors.NewUnimplemented("Delete operation not yet implemented for vSphere")
}

// Power performs power operations on a virtual machine
func (p *Provider) Power(ctx context.Context, req *providerv1.PowerRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("Power operation not yet implemented for vSphere")
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

	// Get VM properties
	var vmMo mo.VirtualMachine
	pc := property.DefaultCollector(p.client.Client)
	err = pc.RetrieveOne(ctx, vmRef, []string{
		"runtime.powerState",
		"guest.ipAddress",
		"guest.net",
		"summary.config.name",
		"summary.runtime.powerState",
	}, &vmMo)

	if err != nil {
		// VM might not exist or be accessible
		p.logger.Warn("Failed to retrieve VM properties", "vm_id", req.Id, "error", err)
		return &providerv1.DescribeResponse{
			Exists: false,
		}, nil
	}

	// VM exists, gather information
	powerState := string(vmMo.Runtime.PowerState)

	// Collect IP addresses
	var ips []string
	if vmMo.Guest != nil {
		// Primary IP address
		if vmMo.Guest.IpAddress != "" {
			ips = append(ips, vmMo.Guest.IpAddress)
		}

		// Additional IPs from guest networks
		if vmMo.Guest.Net != nil {
			for _, netInfo := range vmMo.Guest.Net {
				if netInfo.IpConfig != nil {
					for _, ipConfig := range netInfo.IpConfig.IpAddress {
						if ipConfig.IpAddress != "" && !contains(ips, ipConfig.IpAddress) {
							ips = append(ips, ipConfig.IpAddress)
						}
					}
				}
			}
		}
	}

	// Create provider raw JSON with detailed VM info
	providerRawJson := fmt.Sprintf(`{
		"vm_id": "%s",
		"name": "%s",
		"power_state": "%s",
		"guest_ip": "%s"
	}`, req.Id, vmMo.Summary.Config.Name, powerState, vmMo.Guest.IpAddress)

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
}

// parseCreateRequest parses the JSON-encoded specifications from the gRPC request
func (p *Provider) parseCreateRequest(req *providerv1.CreateRequest) (*VMSpec, error) {
	spec := &VMSpec{
		Name: req.Name,
	}

	// Parse VMClass from JSON
	if req.ClassJson != "" {
		var vmClass struct {
			CPU          int32  `json:"cpu"`
			Memory       string `json:"memory"`
			DiskDefaults struct {
				Size string `json:"size"`
				Type string `json:"type"`
			} `json:"diskDefaults"`
			Firmware string `json:"firmware"`
		}

		if err := json.Unmarshal([]byte(req.ClassJson), &vmClass); err != nil {
			return nil, fmt.Errorf("failed to parse VMClass JSON: %w", err)
		}

		spec.CPU = vmClass.CPU
		spec.Firmware = vmClass.Firmware
		spec.DiskType = vmClass.DiskDefaults.Type

		// Parse memory (e.g., "4Gi" -> 4096 MB)
		if memMB, err := p.parseMemoryToMB(vmClass.Memory); err == nil {
			spec.MemoryMB = memMB
		}

		// Parse disk size (e.g., "40Gi" -> 40 GB)
		if diskGB, err := p.parseSizeToGB(vmClass.DiskDefaults.Size); err == nil {
			spec.DiskSizeGB = diskGB
		}
	}

	// Parse VMImage from JSON
	if req.ImageJson != "" {
		var vmImage struct {
			Source struct {
				VSphere struct {
					TemplateName string `json:"templateName"`
				} `json:"vsphere"`
			} `json:"source"`
		}

		if err := json.Unmarshal([]byte(req.ImageJson), &vmImage); err != nil {
			return nil, fmt.Errorf("failed to parse VMImage JSON: %w", err)
		}

		spec.TemplateName = vmImage.Source.VSphere.TemplateName
	}

	// Parse Networks from JSON
	if req.NetworksJson != "" {
		var networks []struct {
			Network struct {
				VSphere struct {
					Portgroup string `json:"portgroup"`
				} `json:"vsphere"`
			} `json:"network"`
		}

		if err := json.Unmarshal([]byte(req.NetworksJson), &networks); err != nil {
			return nil, fmt.Errorf("failed to parse Networks JSON: %w", err)
		}

		if len(networks) > 0 {
			spec.NetworkName = networks[0].Network.VSphere.Portgroup
		}
	}

	return spec, nil
}

// parseMemoryToMB converts memory strings like "4Gi" to megabytes
func (p *Provider) parseMemoryToMB(memory string) (int64, error) {
	if memory == "" {
		return 0, fmt.Errorf("empty memory specification")
	}

	// Simple parsing for common units
	if strings.HasSuffix(memory, "Gi") {
		gb := strings.TrimSuffix(memory, "Gi")
		if val, err := strconv.ParseInt(gb, 10, 64); err == nil {
			return val * 1024, nil // GB to MB
		}
	}
	if strings.HasSuffix(memory, "Mi") {
		mb := strings.TrimSuffix(memory, "Mi")
		if val, err := strconv.ParseInt(mb, 10, 64); err == nil {
			return val, nil
		}
	}

	return 0, fmt.Errorf("unsupported memory format: %s", memory)
}

// parseSizeToGB converts size strings like "40Gi" to gigabytes
func (p *Provider) parseSizeToGB(size string) (int64, error) {
	if size == "" {
		return 0, fmt.Errorf("empty size specification")
	}

	if strings.HasSuffix(size, "Gi") {
		gb := strings.TrimSuffix(size, "Gi")
		return strconv.ParseInt(gb, 10, 64)
	}
	if strings.HasSuffix(size, "Ti") {
		tb := strings.TrimSuffix(size, "Ti")
		if val, err := strconv.ParseInt(tb, 10, 64); err == nil {
			return val * 1024, nil // TB to GB
		}
	}

	return 0, fmt.Errorf("unsupported size format: %s", size)
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
	vmRef := info.Result.(types.ManagedObjectReference)

	// Get the VM's managed object ID for returning
	vmID := vmRef.Value

	p.logger.Info("Virtual machine created successfully", "vm_id", vmID, "name", spec.Name)

	// TODO: Power on the VM if powerState is requested
	// This would involve checking the original request and calling newVM.PowerOn(ctx)

	return vmID, nil
}
