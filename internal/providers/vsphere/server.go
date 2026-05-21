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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/projectbeskar/virtrigaud/internal/diskutil"
	"github.com/projectbeskar/virtrigaud/internal/storage"
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
	DefaultDatastore  string
	DefaultStoragePod string // Datastore Cluster for automatic datastore selection
	DefaultCluster    string
	DefaultFolder     string
}

// New creates and returns a new vSphere Provider instance. It reads all configuration
// from environment variables and mounted Kubernetes secret files:
//
//   - PROVIDER_ENDPOINT: vCenter SOAP API URL (required), e.g. https://vcenter.example.com/sdk
//   - TLS_INSECURE_SKIP_VERIFY: set to "true" to disable TLS certificate verification
//   - PROVIDER_DEFAULT_DATASTORE: datastore name to use when none is specified (default: "datastore1")
//   - PROVIDER_DEFAULT_STORAGE_POD: datastore cluster name for automatic placement
//   - PROVIDER_DEFAULT_CLUSTER: compute cluster name (default: "cluster01")
//   - PROVIDER_DEFAULT_FOLDER: VM folder path (default: "research-vms")
//
// Credentials (username and password) are read from files mounted at CredentialsPath
// by the provider controller. If credentials or endpoint are missing the govmomi
// client will not be created; the error is logged but New still returns a Provider —
// Validate will subsequently report the connection failure.
func New() *Provider {
	// Load configuration from environment (set by provider controller)
	config := &Config{
		Endpoint:           os.Getenv("PROVIDER_ENDPOINT"),
		InsecureSkipVerify: os.Getenv("TLS_INSECURE_SKIP_VERIFY") == "true", // Allow skipping TLS verification
		// Provider defaults - these should be set by the provider controller from CRD spec.defaults
		DefaultDatastore:  os.Getenv("PROVIDER_DEFAULT_DATASTORE"),
		DefaultStoragePod: os.Getenv("PROVIDER_DEFAULT_STORAGE_POD"),
		DefaultCluster:    os.Getenv("PROVIDER_DEFAULT_CLUSTER"),
		DefaultFolder:     os.Getenv("PROVIDER_DEFAULT_FOLDER"),
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

// loadCredentialsFromFiles reads the vCenter username and password from plain-text
// files mounted by the provider controller at CredentialsPath. The files are
// expected to be named "username" and "password". Surrounding whitespace (including
// newlines added by base64-encoded secrets) is trimmed before storing the values in
// config. An error is returned if either file cannot be read.
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

// createVSphereClient establishes an authenticated govmomi session against the vCenter
// SOAP API described by config and returns a connected govmomi.Client together with a
// Finder configured to search the default datacenter.
//
// Credentials are passed via url.UserPassword so that special characters in passwords
// are handled correctly and are never embedded in the URL string. TLS verification is
// controlled by config.InsecureSkipVerify; when verification is enabled the expected
// server name is set to the host component of the endpoint URL.
//
// An error is returned if the endpoint URL is missing, credentials are empty, the TCP
// connection cannot be established, or the Login call is rejected by vCenter.
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

// resolveDatastoreFromStoragePod picks the datastore with the most free space from the
// vSphere Datastore Cluster (StoragePod) identified by storagePodName. This implements
// a lightweight alternative to Storage DRS: the caller does not need Storage DRS enabled
// on the cluster.
//
// The method enumerates all StoragePod objects via a container view, locates the one
// matching storagePodName, then retrieves summary information for all member datastores
// using the property collector. It delegates the selection to selectBestDatastoreByFreeSpace
// and returns an object.Datastore wrapping the winning managed-object reference.
//
// Returns an error if the StoragePod is not found, contains no accessible datastores, or
// if any vSphere API call fails.
func (p *Provider) resolveDatastoreFromStoragePod(ctx context.Context, storagePodName string) (*object.Datastore, error) {
	viewMgr := view.NewManager(p.client.Client)
	v, err := viewMgr.CreateContainerView(ctx, p.client.ServiceContent.RootFolder, []string{"StoragePod"}, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create container view for StoragePods: %w", err)
	}
	defer func() { _ = v.Destroy(ctx) }()

	var pods []mo.StoragePod
	if err := v.Retrieve(ctx, []string{"StoragePod"}, []string{"name", "childEntity"}, &pods); err != nil {
		return nil, fmt.Errorf("failed to retrieve StoragePods: %w", err)
	}

	var childRefs []types.ManagedObjectReference
	for _, pod := range pods {
		if pod.Name == storagePodName {
			childRefs = pod.ChildEntity
			break
		}
	}
	if childRefs == nil {
		return nil, fmt.Errorf("StoragePod '%s' not found", storagePodName)
	}
	if len(childRefs) == 0 {
		return nil, fmt.Errorf("StoragePod '%s' contains no datastores", storagePodName)
	}

	pc := property.DefaultCollector(p.client.Client)
	var datastores []mo.Datastore
	if err := pc.Retrieve(ctx, childRefs, []string{"name", "summary"}, &datastores); err != nil {
		return nil, fmt.Errorf("failed to retrieve datastores from StoragePod '%s': %w", storagePodName, err)
	}
	if len(datastores) == 0 {
		return nil, fmt.Errorf("StoragePod '%s' has no accessible datastores", storagePodName)
	}

	best := selectBestDatastoreByFreeSpace(datastores)
	p.logger.Info("Selected datastore from StoragePod",
		"storagePod", storagePodName,
		"datastore", best.Name,
		"freeSpaceGiB", best.Summary.FreeSpace/1024/1024/1024,
	)

	// Use finder to get the datastore by name so that properties like Name() are populated
	return p.finder.Datastore(ctx, best.Name)
}

// selectBestDatastoreByFreeSpace performs a linear scan over datastores and returns the
// one whose Summary.FreeSpace field is largest. The caller must ensure that the slice is
// non-empty; passing an empty slice will panic on the initial index access.
func selectBestDatastoreByFreeSpace(datastores []mo.Datastore) mo.Datastore {
	best := datastores[0]
	for _, ds := range datastores[1:] {
		if ds.Summary.FreeSpace > best.Summary.FreeSpace {
			best = ds
		}
	}
	return best
}

// cloneDiskToStreamOptimized copies the VMDK at sourcePath to destPath using the
// VirtualDiskManager API, converting the on-disk format to sparseMonolithic in the
// process. sparseMonolithic produces a single, self-contained, compressed VMDK file
// that is universally compatible with OVF/OVA tooling and can be downloaded in one
// HTTP request.
//
// sourcePath and destPath must be fully-qualified vSphere datastore paths of the form
// "[datastoreName] path/to/file.vmdk". Both paths are resolved against the default
// datacenter. This approach handles all input VMDK subtypes (sesparse, flat, thick,
// thin) without requiring the caller to know the source format.
//
// The call blocks until VirtualDiskManager.CopyVirtualDisk task completes.
func (p *Provider) cloneDiskToStreamOptimized(ctx context.Context, sourcePath, destPath string) error {
	if p.client == nil {
		return fmt.Errorf("vSphere client not initialized")
	}

	// Get datacenter
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return fmt.Errorf("failed to get datacenter: %w", err)
	}

	// Create VirtualDiskManager
	virtualDiskManager := object.NewVirtualDiskManager(p.client.Client)

	// Clone disk to sparseMonolithic format
	// SparseMonolithic is a single-file compressed format ideal for export/migration
	// It's the format typically used in OVF/OVA exports and is universally compatible
	spec := &types.VirtualDiskSpec{
		DiskType:    string(types.VirtualDiskTypeSparseMonolithic),
		AdapterType: string(types.VirtualDiskAdapterTypeLsiLogic),
	}

	p.logger.Info("Starting disk clone operation", "source", sourcePath, "dest", destPath, "format", "sparseMonolithic")

	task, err := virtualDiskManager.CopyVirtualDisk(ctx, sourcePath, datacenter, destPath, datacenter, spec, false)
	if err != nil {
		return fmt.Errorf("failed to start disk clone: %w", err)
	}

	// Wait for task completion
	err = task.Wait(ctx)
	if err != nil {
		return fmt.Errorf("disk clone failed: %w", err)
	}

	p.logger.Info("Disk clone completed successfully", "destination", destPath)
	return nil
}

// Validate implements the ProviderServer interface. It verifies that the vSphere client
// is initialized and that the current session is still active (govmomi.Client.Valid).
// If the session has expired, Validate attempts to create a fresh client using the
// stored configuration. On success it returns ValidateResponse{Ok: true}; on any failure
// it returns ValidateResponse{Ok: false, Message: <reason>} without propagating a gRPC
// error, so the controller can surface the message to the user.
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

// GetCapabilities implements the ProviderServer interface and returns a static description
// of the features supported by this vSphere provider:
//
//   - Online reconfiguration of CPU and memory (hot-add must be enabled on the VM)
//   - Online disk expansion
//   - Snapshots (disk-only; memory snapshots are not captured by default)
//   - Linked clones (delta-disk backed clones sharing a parent disk)
//   - Image import from external sources
//   - Disk types: thin, thick, eager-zeroed
//   - Network types: standard vSwitch portgroups and distributed virtual switch portgroups
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

// Create implements the ProviderServer interface. It provisions a new virtual machine
// based on the JSON-encoded specifications contained in req (VMClass, VMImage, networks,
// cloud-init user-data, cloud-init metadata, and placement overrides).
//
// Two creation paths are supported:
//   - Template clone: when req.ImageJson contains a TemplateName the VM is created by
//     cloning an existing vSphere VM or template with CloneVM_Task.
//   - Imported disk: when req.ImageJson contains a Path the VM is created from scratch
//     (CreateVM_Task) with the pre-uploaded VMDK attached as a persistent disk.
//
// In both cases the VM is powered on immediately after creation. The returned
// CreateResponse.Id contains the vSphere ManagedObjectReference value (e.g. "vm-42")
// which is used as the stable VM identifier in all subsequent API calls.
func (p *Provider) Create(ctx context.Context, req *providerv1.CreateRequest) (*providerv1.CreateResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Debug("Create called",
		"vm_name", req.Name,
		"has_disks_json", req.DisksJson != "",
		"disks_json_length", len(req.DisksJson))

	if req.DisksJson != "" {
		p.logger.Debug("DisksJson received", "disks_json", req.DisksJson)
	}

	// Parse the JSON specifications to understand what to create
	vmSpec, err := p.parseCreateRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to parse create request: %w", err)
	}

	p.logger.Debug("Parsed VMSpec",
		"vm_name", vmSpec.Name,
		"additional_disks_count", len(vmSpec.AdditionalDisks))

	if len(vmSpec.AdditionalDisks) > 0 {
		for i, disk := range vmSpec.AdditionalDisks {
			p.logger.Debug("Additional disk",
				"index", i,
				"name", disk.Name,
				"size_gib", disk.SizeGiB,
				"type", disk.Type)
		}
	}

	// Check if VM already exists
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err == nil {
		p.finder.SetDatacenter(datacenter)
		existingVM, _ := p.finder.VirtualMachine(ctx, vmSpec.Name)
		if existingVM != nil {
			p.logger.Warn("VM already exists, will attempt to use existing VM",
				"vm_name", vmSpec.Name,
				"vm_ref", existingVM.Reference().Value)
			return &providerv1.CreateResponse{
				Id: existingVM.Reference().Value,
			}, nil
		}
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

// Delete implements the ProviderServer interface. It removes the virtual machine
// identified by req.Id (vSphere ManagedObjectReference value) from the vCenter
// inventory and deletes all associated files from the datastore.
//
// The operation follows this sequence:
//  1. Look up the VM power state via its ManagedObjectReference.
//  2. If the VM is not found (SOAP fault), treat the deletion as already complete
//     and return success (idempotent behaviour).
//  3. If the VM is powered on, issue a PowerOff task and wait for it to finish.
//     A power-off failure is logged but does not abort the deletion.
//  4. Issue a Destroy task (equivalent to "Delete from Disk" in the vSphere UI)
//     and wait for it to complete.
//
// The operation blocks until the Destroy task finishes.
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

// Power implements the ProviderServer interface. It performs one of the following power
// operations on the virtual machine identified by req.Id:
//
//   - POWER_OP_ON: issues PowerOn_Task and waits for completion.
//   - POWER_OP_OFF: issues PowerOff_Task (hard power-off) and waits for completion.
//   - POWER_OP_REBOOT: calls RebootGuest via VMware Tools; requires Tools to be running.
//     This call is synchronous — no task is polled.
//   - POWER_OP_SHUTDOWN_GRACEFUL: delegates to performGracefulShutdown, which attempts
//     ShutdownGuest via VMware Tools and falls back to a hard PowerOff if Tools are
//     unavailable or the graceful timeout (req.GracefulTimeoutSeconds, default 60 s)
//     elapses.
//
// All operations except REBOOT block until the underlying vSphere task completes.
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

// performGracefulShutdown sends a guest OS shutdown request via VMware Tools and waits
// for the VM to reach the powered-off state within the configured timeout.
//
// The graceful timeout is taken from req.GracefulTimeoutSeconds; if zero or negative,
// a default of 60 seconds is used. Before issuing ShutdownGuest the method checks the
// VMware Tools status via getVMwareToolsStatus. If Tools are not installed or are in a
// non-operational state ("toolsOk" or "toolsOld" are the only accepted values), the
// method falls back immediately to a hard PowerOff via fallbackToPowerOff. The same
// fallback occurs if ShutdownGuest returns an error or if the VM has not reached
// powered-off state before the timeout expires.
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

// getVMwareToolsStatus retrieves the guest.toolsStatus property of vm via the property
// collector. The returned string corresponds to the VirtualMachineToolsStatus enum
// values reported by vSphere, such as "toolsOk", "toolsOld", "toolsNotInstalled", or
// "toolsNotRunning". If the Guest information object is nil (e.g. the VM has never been
// powered on) "toolsNotInstalled" is returned as a safe default.
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

// waitForGracefulShutdown polls the VM power state every 2 seconds until it reaches
// VirtualMachinePowerStatePoweredOff or until timeout expires. A child context derived
// from ctx with the given timeout controls the polling loop, so the function also
// respects cancellation of the parent context. If the timeout is reached, the method
// falls back to a hard power-off via fallbackToPowerOff.
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

// fallbackToPowerOff issues a hard PowerOff_Task against vm and waits for it to
// complete. It is called by performGracefulShutdown and waitForGracefulShutdown when
// graceful shutdown via VMware Tools is not possible or has timed out.
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

// Reconfigure implements the ProviderServer interface. It applies the desired
// configuration described by req.DesiredJson to the virtual machine identified by
// req.Id. The JSON payload is expected to contain a subset of the following top-level
// keys:
//
//   - "class": object with "cpus" (number) and/or "memory" (string, e.g. "4Gi") fields
//     for CPU and memory adjustments.
//   - "disks": array where the first element may contain a "size" (string, e.g. "100Gi")
//     field to expand the primary disk.
//
// Current values are retrieved from vCenter before building the change-spec so that
// only actual differences trigger a ReconfigVM_Task. If no changes are detected the
// method returns success immediately without contacting vSphere. Disk shrinks are not
// permitted; a resize is only applied when the desired size is larger than the current
// allocated size.
func (p *Provider) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Reconfiguring virtual machine", "vm_id", req.Id)

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

	// Get current VM properties
	var vmMo mo.VirtualMachine
	err = vm.Properties(ctx, vm.Reference(), []string{
		"config.hardware.numCPU",
		"config.hardware.memoryMB",
		"config.hardware.device",
		"runtime.powerState",
	}, &vmMo)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM properties: %w", err)
	}

	// Parse the desired configuration from JSON
	var desired map[string]interface{}
	if err := json.Unmarshal([]byte(req.DesiredJson), &desired); err != nil {
		return nil, fmt.Errorf("failed to parse desired configuration: %w", err)
	}

	// Build the reconfiguration spec
	configSpec := &types.VirtualMachineConfigSpec{}
	hasChanges := false

	// Handle CPU changes
	if classData, ok := desired["class"].(map[string]interface{}); ok {
		if cpus, ok := classData["cpus"].(float64); ok {
			newCPUs := int32(cpus)
			if newCPUs != vmMo.Config.Hardware.NumCPU {
				p.logger.Info("CPU change requested", "vm_id", req.Id, "old", vmMo.Config.Hardware.NumCPU, "new", newCPUs)
				configSpec.NumCPUs = newCPUs
				hasChanges = true
			}
		}

		// Handle memory changes (memory is in MiB in the request)
		if memory, ok := classData["memory"].(string); ok {
			memMiB, err := p.parseMemory(memory)
			if err == nil {
				newMemoryMB := memMiB
				currentMemoryMB := int64(vmMo.Config.Hardware.MemoryMB)
				if newMemoryMB != currentMemoryMB {
					p.logger.Info("Memory change requested", "vm_id", req.Id, "old_mb", currentMemoryMB, "new_mb", newMemoryMB)
					configSpec.MemoryMB = newMemoryMB
					hasChanges = true
				}
			} else {
				p.logger.Warn("Failed to parse memory value", "memory", memory, "error", err)
			}
		}
	}

	// Handle disk changes
	if disksData, ok := desired["disks"].([]interface{}); ok && len(disksData) > 0 {
		// Get the first disk (primary disk) for resizing
		if diskData, ok := disksData[0].(map[string]interface{}); ok {
			if sizeStr, ok := diskData["size"].(string); ok {
				sizeGiB, err := p.parseMemory(sizeStr)
				if err == nil {
					// Convert MiB to GiB (if parseMemory returns MiB)
					sizeGB := sizeGiB / 1024
					if sizeGB > 0 {
						// Find the primary disk
						var primaryDisk *types.VirtualDisk
						for _, device := range vmMo.Config.Hardware.Device {
							if disk, ok := device.(*types.VirtualDisk); ok {
								primaryDisk = disk
								break
							}
						}

						if primaryDisk != nil {
							currentSizeGB := primaryDisk.CapacityInKB / (1024 * 1024)
							if sizeGB > currentSizeGB {
								p.logger.Info("Disk resize requested", "vm_id", req.Id, "old_gb", currentSizeGB, "new_gb", sizeGB)

								// Create a new disk with updated size
								newDisk := *primaryDisk
								newDisk.CapacityInKB = sizeGB * 1024 * 1024 // Convert GB to KB

								deviceSpec := &types.VirtualDeviceConfigSpec{
									Operation: types.VirtualDeviceConfigSpecOperationEdit,
									Device:    &newDisk,
								}

								configSpec.DeviceChange = append(configSpec.DeviceChange, deviceSpec)
								hasChanges = true
							}
						}
					}
				}
			}
		}
	}

	// If no changes, return success immediately
	if !hasChanges {
		p.logger.Info("No configuration changes needed", "vm_id", req.Id)
		return &providerv1.TaskResponse{}, nil
	}

	// Perform the reconfiguration
	p.logger.Info("Applying VM reconfiguration", "vm_id", req.Id)
	task, err := vm.Reconfigure(ctx, *configSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to start reconfiguration: %w", err)
	}

	// Wait for the reconfiguration to complete
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reconfiguration failed: %w", err)
	}

	p.logger.Info("VM reconfigured successfully", "vm_id", req.Id)

	// Return empty task response since we completed synchronously
	return &providerv1.TaskResponse{}, nil
}

// parseMemory converts a Kubernetes-style quantity string to mebibytes (MiB).
// Supported suffixes and their conversions:
//
//   - "Gi" — gibibytes, multiplied by 1024 to yield MiB.
//   - "Mi" — mebibytes, returned as-is.
//   - "Ki" — kibibytes, divided by 1024 to yield MiB (fractional KiB is truncated).
//   - no suffix — assumed to already be MiB; parsed as a decimal integer.
//
// An error is returned for unrecognised suffixes or non-numeric values.
func (p *Provider) parseMemory(memStr string) (int64, error) {
	memStr = strings.TrimSpace(memStr)

	if strings.HasSuffix(memStr, "Gi") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(memStr, "Gi"), 64)
		if err != nil {
			return 0, err
		}
		return int64(val * 1024), nil // Convert GiB to MiB
	}

	if strings.HasSuffix(memStr, "Mi") {
		val, err := strconv.ParseInt(strings.TrimSuffix(memStr, "Mi"), 10, 64)
		if err != nil {
			return 0, err
		}
		return val, nil
	}

	if strings.HasSuffix(memStr, "Ki") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(memStr, "Ki"), 64)
		if err != nil {
			return 0, err
		}
		return int64(val / 1024), nil // Convert KiB to MiB
	}

	// Try parsing as raw number (assume MiB)
	val, err := strconv.ParseInt(memStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory format: %s", memStr)
	}
	return val, nil
}

// HardwareUpgrade implements the ProviderServer interface. It upgrades the virtual
// hardware compatibility level of the VM identified by req.Id to the version specified
// by req.TargetVersion (integer, e.g. 21 maps to "vmx-21").
//
// Prerequisites enforced by this method:
//   - The VM must be powered off; an error is returned otherwise.
//   - The target version must be strictly newer than the current version as determined
//     by isNewerHardwareVersion; downgrading is not supported.
//
// If the VM is already at the requested version the method returns success without
// issuing an UpgradeVM_Task. The upgrade task blocks until completion.
func (p *Provider) HardwareUpgrade(ctx context.Context, req *providerv1.HardwareUpgradeRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Upgrading VM hardware version", "vm_id", req.Id, "target_version", req.TargetVersion)

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

	// Check current power state - VM must be powered off for hardware upgrade
	powerState, err := vm.PowerState(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check VM power state: %w", err)
	}

	if powerState != types.VirtualMachinePowerStatePoweredOff {
		return nil, fmt.Errorf("VM must be powered off for hardware upgrade, current state: %s", powerState)
	}

	// Get current VM configuration to check current hardware version
	var vmMo mo.VirtualMachine
	err = vm.Properties(ctx, vm.Reference(), []string{"config.version", "config.name"}, &vmMo)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM properties: %w", err)
	}

	currentVersion := vmMo.Config.Version
	targetVersion := fmt.Sprintf("vmx-%d", req.TargetVersion)

	p.logger.Info("Hardware version comparison",
		"vm_id", req.Id,
		"current_version", currentVersion,
		"target_version", targetVersion)

	// Check if upgrade is needed
	if currentVersion == targetVersion {
		p.logger.Info("VM already at target hardware version", "vm_id", req.Id, "version", targetVersion)
		return &providerv1.TaskResponse{}, nil
	}

	// Validate target version is newer than current
	if !p.isNewerHardwareVersion(currentVersion, targetVersion) {
		return nil, fmt.Errorf("target version %s is not newer than current version %s", targetVersion, currentVersion)
	}

	// Perform the hardware upgrade
	upgradeTask, err := vm.UpgradeVM(ctx, targetVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to start hardware upgrade: %w", err)
	}

	// Wait for upgrade to complete
	_, err = upgradeTask.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("hardware upgrade failed: %w", err)
	}

	p.logger.Info("Hardware upgrade completed successfully",
		"vm_id", req.Id,
		"from_version", currentVersion,
		"to_version", targetVersion)

	return &providerv1.TaskResponse{}, nil
}

// isNewerHardwareVersion reports whether target represents a higher hardware version than
// current. Both strings are expected to be in the vSphere "vmx-N" format (e.g. "vmx-19",
// "vmx-21"). The numeric suffix is extracted with fmt.Sscanf; if parsing fails for either
// argument that argument's version number defaults to 0.
func (p *Provider) isNewerHardwareVersion(current, target string) bool {
	// Extract version numbers from vmx-XX format
	var currentNum, targetNum int

	// Parse current version, default to 0 if parsing fails
	if _, err := fmt.Sscanf(current, "vmx-%d", &currentNum); err != nil {
		currentNum = 0
	}

	// Parse target version, default to 0 if parsing fails
	if _, err := fmt.Sscanf(target, "vmx-%d", &targetNum); err != nil {
		targetNum = 0
	}

	return targetNum > currentNum
}

// Describe implements the ProviderServer interface. It retrieves a comprehensive
// snapshot of the virtual machine identified by req.Id (vSphere ManagedObjectReference
// value) and returns it as a DescribeResponse.
//
// The response includes:
//   - Exists: false if the VM cannot be found or is inaccessible.
//   - PowerState: "On", "Off" (suspended VMs are reported as "Off").
//   - Ips: all non-loopback, non-link-local IPv4/IPv6 addresses reported by VMware Tools.
//   - ConsoleUrl: a vSphere web client URL for direct browser access to the VM console.
//   - ProviderRawJson: a JSON object with extended fields (cpu_count, memory_mb,
//     cpu_usage_mhz, memory_usage_mb, uptime_seconds, boot_time, guest OS, hostname,
//     VMware Tools status and version).
//
// If the property collector call fails (e.g. VM was deleted), the method returns
// Exists: false rather than propagating a gRPC error.
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

	// Generate console URL for vSphere web client
	consoleURL := ""
	if p.config != nil && p.config.Endpoint != "" {
		// vSphere web client URL format: https://{vcenter}/ui/app/vm;nav=v/urn:vmomi:VirtualMachine:{vm-id}:{instance-uuid}
		// Simpler format that works for most vSphere versions
		consoleURL = fmt.Sprintf("%s/ui/#?extensionId=vsphere.core.vm.summary&objectId=urn:vmomi:VirtualMachine:%s:%s",
			strings.TrimSuffix(p.config.Endpoint, "/sdk"),
			req.Id,
			vmMo.Summary.Config.InstanceUuid)
	}

	return &providerv1.DescribeResponse{
		Exists:          true,
		PowerState:      powerState,
		Ips:             ips,
		ConsoleUrl:      consoleURL,
		ProviderRawJson: providerRawJson,
	}, nil
}

// contains reports whether item is present in slice using a linear search.
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// isValidIPAddress reports whether ip is a routable address suitable for inclusion in
// the Describe/ListVMs response. The following addresses are rejected:
//   - IPv4 loopback: 127.0.0.1
//   - IPv6 loopback: ::1
//   - IPv4 link-local: 169.254.0.0/16
//   - IPv6 link-local: fe80::/10 (matched by "fe80:" prefix)
func (p *Provider) isValidIPAddress(ip string) bool {
	// Filter out localhost and link-local addresses
	if ip == "127.0.0.1" || ip == "::1" ||
		strings.HasPrefix(ip, "169.254.") || // Link-local IPv4
		strings.HasPrefix(ip, "fe80:") { // Link-local IPv6
		return false
	}
	return true
}

// mapVSpherePowerState translates a vSphere VirtualMachinePowerState string to the
// VirtRigaud canonical power state strings used in API responses:
//
//   - "poweredOn"  → "On"
//   - "poweredOff" → "Off"
//   - "suspended"  → "Off" (suspended VMs are treated as effectively off)
//   - any other    → "Off" (safe default for unknown states)
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

// addCloudInitToConfigSpec injects cloud-init configuration into configSpec using the
// VMware guestinfo mechanism, which is the standard way to pass cloud-init data to VMs
// without a CD-ROM or network datasource. Both values are base64-encoded and stored in
// the VM's ExtraConfig alongside their encoding hint keys so that cloud-init inside the
// guest can locate and decode them at boot:
//
//   - guestinfo.userdata        → base64(cloudInitData)
//   - guestinfo.userdata.encoding → "base64"
//   - guestinfo.metadata        → base64(cloudInitMetaData or fallback)
//   - guestinfo.metadata.encoding → "base64"
//
// If cloudInitMetaData is empty, a minimal metadata document containing only
// "instance-id: <vmName>" is used as a fallback. If both cloudInitData and
// cloudInitMetaData are empty the method returns nil immediately without modifying
// configSpec. New ExtraConfig entries are appended to any existing entries.
func (p *Provider) addCloudInitToConfigSpec(configSpec *types.VirtualMachineConfigSpec, cloudInitData string, cloudInitMetaData string) error {
	// Defensive: return early if both userdata and metadata are empty
	if cloudInitData == "" && cloudInitMetaData == "" {
		p.logger.Warn("No cloudInitData or cloudInitMetaData provided", "vm_id", configSpec.Name)
		return nil
	}

	// Defensive: if only userdata is empty, skip adding it
	var encodedUserData string
	if cloudInitData != "" {
		p.logger.Debug("Adding cloudInitData to VM config", "vm_id", configSpec.Name)
		encodedUserData = base64.StdEncoding.EncodeToString([]byte(cloudInitData))
	}

	// Defensive: if only metadata is empty, skip adding it
	var metadataValue string
	if cloudInitMetaData != "" {
		metadataValue = cloudInitMetaData
	} else {
		metadataValue = `instance-id: "` + configSpec.Name + `"`
	}
	var encodedMetaData string
	if metadataValue != "" {
		p.logger.Debug("Adding metadata to VM config", "vm_id", configSpec.Name)
		encodedMetaData = base64.StdEncoding.EncodeToString([]byte(metadataValue))
	}

	var extraConfig []types.BaseOptionValue
	if encodedUserData != "" {
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "guestinfo.userdata",
			Value: encodedUserData,
		})
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "guestinfo.userdata.encoding",
			Value: "base64",
		})
	}
	if encodedMetaData != "" {
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "guestinfo.metadata",
			Value: encodedMetaData,
		})
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "guestinfo.metadata.encoding",
			Value: "base64",
		})
	}

	if len(extraConfig) == 0 {
		return nil
	}

	// Add to existing extra config or create new
	if configSpec.ExtraConfig != nil {
		configSpec.ExtraConfig = append(configSpec.ExtraConfig, extraConfig...)
	} else {
		configSpec.ExtraConfig = extraConfig
	}

	return nil
}

// TaskStatus implements the ProviderServer interface. It polls the vSphere task
// identified by req.Task.Id (a Task ManagedObjectReference value) and returns its
// current state.
//
// Because govmomi's Task.WaitForResult blocks until the task reaches a terminal state,
// this method effectively converts an already-running vSphere task into a synchronous
// call from the controller's perspective. Terminal states and their mapping:
//
//   - TaskInfoStateSuccess → Done: true, no error
//   - TaskInfoStateError   → Done: true, Error: localised error message from vCenter
//   - TaskInfoStateQueued  → Done: false
//   - TaskInfoStateRunning → Done: false
//   - any other state      → Done: true, Error: "unexpected task state: <state>"
//
// If WaitForResult itself returns an error (e.g. session expired, task not found) the
// method returns Done: true with the error message rather than propagating a gRPC error,
// so the controller can surface it cleanly.
func (p *Provider) TaskStatus(ctx context.Context, req *providerv1.TaskStatusRequest) (*providerv1.TaskStatusResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	if req.Task == nil || req.Task.Id == "" {
		return nil, fmt.Errorf("task reference is required")
	}

	p.logger.Debug("Checking task status", "task_id", req.Task.Id)

	// Create task reference from ID
	// vSphere task IDs are ManagedObjectReference values
	taskRef := types.ManagedObjectReference{
		Type:  "Task",
		Value: req.Task.Id,
	}

	task := object.NewTask(p.client.Client, taskRef)

	// Get task info
	taskInfo, err := task.WaitForResult(ctx, nil)
	if err != nil {
		// Task failed or error getting status
		p.logger.Error("Task failed or error getting status", "task_id", req.Task.Id, "error", err)
		return &providerv1.TaskStatusResponse{
			Done:  true,
			Error: fmt.Sprintf("task failed: %v", err),
		}, nil
	}

	// Check task state
	isDone := false
	errorMsg := ""

	switch taskInfo.State {
	case types.TaskInfoStateSuccess:
		isDone = true
		p.logger.Debug("Task completed successfully", "task_id", req.Task.Id)
	case types.TaskInfoStateError:
		isDone = true
		if taskInfo.Error != nil {
			errorMsg = taskInfo.Error.LocalizedMessage
		} else {
			errorMsg = "task failed with unknown error"
		}
		p.logger.Error("Task completed with error", "task_id", req.Task.Id, "error", errorMsg)
	case types.TaskInfoStateQueued:
		isDone = false
		p.logger.Debug("Task is queued", "task_id", req.Task.Id)
	case types.TaskInfoStateRunning:
		isDone = false
		p.logger.Debug("Task is running", "task_id", req.Task.Id, "progress", taskInfo.Progress)
	default:
		// Unknown state, consider it done to avoid hanging
		isDone = true
		errorMsg = fmt.Sprintf("unexpected task state: %s", taskInfo.State)
		p.logger.Warn("Task in unexpected state", "task_id", req.Task.Id, "state", taskInfo.State)
	}

	return &providerv1.TaskStatusResponse{
		Done:  isDone,
		Error: errorMsg,
	}, nil
}

// SnapshotCreate implements the ProviderServer interface. It creates a disk-only
// snapshot of the virtual machine identified by req.VmId.
//
// Snapshot name: req.NameHint is used if non-empty; otherwise a name of the form
// "snapshot-<unix-timestamp>" is generated automatically.
//
// Memory: req.IncludeMemory controls whether the VM's in-memory state is captured.
// Note that GetCapabilities reports SupportsMemorySnapshots: false, so callers should
// generally pass false.
//
// Quiesce: filesystem quiescing (which requires VMware Tools and guest coordination) is
// always disabled in this implementation. The SnapshotCreateResponse.SnapshotId contains
// the ManagedObjectReference value of the newly created VirtualMachineSnapshot object,
// which is used in subsequent SnapshotDelete and SnapshotRevert calls.
func (p *Provider) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Creating VM snapshot",
		"vm_id", req.VmId,
		"name_hint", req.NameHint,
		"include_memory", req.IncludeMemory)

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Create VM reference from ID
	vmRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.VmId,
	}

	vm := object.NewVirtualMachine(p.client.Client, vmRef)

	// Generate snapshot name if not provided
	snapshotName := req.NameHint
	if snapshotName == "" {
		snapshotName = fmt.Sprintf("snapshot-%d", time.Now().Unix())
	}

	// Description defaults to empty string if not provided
	description := req.Description

	// Quiesce filesystem (false by default, requires VMware Tools)
	// TODO: Make this configurable via API when proto is updated
	quiesce := false

	// Create the snapshot
	// Parameters: name, description, includeMemory, quiesce
	p.logger.Info("Initiating snapshot creation",
		"vm_id", req.VmId,
		"snapshot_name", snapshotName,
		"memory", req.IncludeMemory,
		"quiesce", quiesce)

	task, err := vm.CreateSnapshot(ctx, snapshotName, description, req.IncludeMemory, quiesce)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot task: %w", err)
	}

	// Wait for snapshot creation to complete
	taskInfo, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot creation failed: %w", err)
	}

	// Extract snapshot reference from task result
	if taskInfo.Result == nil {
		return nil, fmt.Errorf("snapshot creation completed but no snapshot reference returned")
	}

	snapshotRef, ok := taskInfo.Result.(types.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf("unexpected task result type: %T", taskInfo.Result)
	}

	p.logger.Info("Snapshot created successfully",
		"vm_id", req.VmId,
		"snapshot_id", snapshotRef.Value,
		"snapshot_name", snapshotName)

	return &providerv1.SnapshotCreateResponse{
		SnapshotId: snapshotRef.Value,
	}, nil
}

// SnapshotDelete implements the ProviderServer interface. It removes the snapshot
// identified by req.SnapshotId from the VM identified by req.VmId.
//
// The snapshot tree is traversed with findSnapshotByID which matches first by
// ManagedObjectReference value and falls back to snapshot name. RemoveSnapshot_Task is
// called with removeChildren=false (orphan child snapshots are re-parented rather than
// deleted) and consolidate=true (delta disks are merged after removal to reclaim space).
// The call blocks until the task completes.
func (p *Provider) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Deleting VM snapshot", "vm_id", req.VmId, "snapshot_id", req.SnapshotId)

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Create VM reference from ID
	vmRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.VmId,
	}

	vm := object.NewVirtualMachine(p.client.Client, vmRef)

	// Get the VM's snapshot tree to find the specific snapshot
	var vmObj mo.VirtualMachine
	err = vm.Properties(ctx, vm.Reference(), []string{"snapshot"}, &vmObj)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM snapshot properties: %w", err)
	}

	if vmObj.Snapshot == nil {
		return nil, fmt.Errorf("VM has no snapshots")
	}

	// Find the snapshot by ID in the snapshot tree
	snapshot := p.findSnapshotByID(vmObj.Snapshot.RootSnapshotList, req.SnapshotId)
	if snapshot == nil {
		return nil, fmt.Errorf("snapshot not found: %s", req.SnapshotId)
	}

	// Remove the snapshot using RemoveSnapshot_Task method
	// removeChildren: false = consolidate child snapshots into parent
	// consolidate: true = merge snapshot disks after removal
	removeChildren := false
	consolidate := true

	task, err := vm.RemoveSnapshot(ctx, snapshot.Snapshot.Value, removeChildren, &consolidate)
	if err != nil {
		return nil, fmt.Errorf("failed to start snapshot removal: %w", err)
	}

	// Wait for snapshot deletion to complete
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot deletion failed: %w", err)
	}

	p.logger.Info("Snapshot deleted successfully", "vm_id", req.VmId, "snapshot_id", req.SnapshotId)

	return &providerv1.TaskResponse{}, nil
}

// SnapshotRevert implements the ProviderServer interface. It reverts the virtual machine
// identified by req.VmId to the state captured in the snapshot identified by
// req.SnapshotId.
//
// The snapshot is located by traversing the VM's snapshot tree with findSnapshotByID
// (matching by ManagedObjectReference value or name). RevertToSnapshot_Task is called
// with suppressPowerOn=false, meaning the VM will be automatically powered on if the
// snapshot was taken while the VM was running. The call blocks until the task completes.
func (p *Provider) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Reverting to VM snapshot", "vm_id", req.VmId, "snapshot_id", req.SnapshotId)

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Create VM reference from ID
	vmRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.VmId,
	}

	vm := object.NewVirtualMachine(p.client.Client, vmRef)

	// Get the VM's snapshot tree to find the specific snapshot
	var vmObj mo.VirtualMachine
	err = vm.Properties(ctx, vm.Reference(), []string{"snapshot", "runtime.powerState"}, &vmObj)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM properties: %w", err)
	}

	if vmObj.Snapshot == nil {
		return nil, fmt.Errorf("VM has no snapshots")
	}

	// Find the snapshot by ID in the snapshot tree
	snapshot := p.findSnapshotByID(vmObj.Snapshot.RootSnapshotList, req.SnapshotId)
	if snapshot == nil {
		return nil, fmt.Errorf("snapshot not found: %s", req.SnapshotId)
	}

	// Store original power state for restoration if needed
	originalPowerState := vmObj.Runtime.PowerState
	p.logger.Info("VM current power state", "vm_id", req.VmId, "power_state", originalPowerState)

	// Revert to the snapshot
	// suppressPowerOn: false = power on if snapshot contains memory state
	// TODO: Make this configurable via API when proto is updated
	suppressPowerOn := false

	task, err := vm.RevertToSnapshot(ctx, snapshot.Snapshot.Value, suppressPowerOn)
	if err != nil {
		return nil, fmt.Errorf("failed to start snapshot revert: %w", err)
	}

	// Wait for snapshot revert to complete
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot revert failed: %w", err)
	}

	p.logger.Info("Snapshot revert completed successfully",
		"vm_id", req.VmId,
		"snapshot_id", req.SnapshotId)

	return &providerv1.TaskResponse{}, nil
}

// findSnapshotByID performs a depth-first recursive search of the VM snapshot tree.
// It returns a pointer to the first VirtualMachineSnapshotTree entry whose
// Snapshot.Value (ManagedObjectReference) equals snapshotID. As a secondary fallback,
// an entry whose Name field equals snapshotID is also accepted. Returns nil if no
// matching snapshot is found at any depth.
func (p *Provider) findSnapshotByID(snapshots []types.VirtualMachineSnapshotTree, snapshotID string) *types.VirtualMachineSnapshotTree {
	for i := range snapshots {
		if snapshots[i].Snapshot.Value == snapshotID {
			return &snapshots[i]
		}

		// Also check by snapshot name as a fallback
		if snapshots[i].Name == snapshotID {
			return &snapshots[i]
		}

		// Recursively search child snapshots
		if len(snapshots[i].ChildSnapshotList) > 0 {
			if found := p.findSnapshotByID(snapshots[i].ChildSnapshotList, snapshotID); found != nil {
				return found
			}
		}
	}

	return nil
}

// Clone implements the ProviderServer interface. It creates a new VM named
// req.TargetName from the source VM identified by req.SourceVmId.
//
// Two clone modes are supported based on req.Linked:
//
//   - Full clone (req.Linked == false): all disk backings are copied to the target
//     datastore (DiskMoveType: MoveAllDiskBackingsAndAllowSharing). The clone is
//     completely independent of the source.
//   - Linked clone (req.Linked == true): a delta disk is created on top of a snapshot
//     of the source VM (DiskMoveType: CreateNewChildDiskBacking). If the source has
//     existing snapshots the most recent root-level snapshot is used; otherwise a new
//     snapshot named "clone-base-<timestamp>" is created automatically.
//
// Placement uses the provider defaults (cluster, datastore, folder); the folder falls
// back to the datacenter's default VM folder if the configured folder path is not found.
// The cloned VM is left powered off. The returned CloneResponse.TargetVmId contains
// the ManagedObjectReference value of the new VM.
func (p *Provider) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Cloning virtual machine", "source_vm_id", req.SourceVmId, "target_name", req.TargetName, "linked", req.Linked)

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Create source VM reference from ID
	sourceVMRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.SourceVmId,
	}

	sourceVM := object.NewVirtualMachine(p.client.Client, sourceVMRef)

	// Determine which cluster to use (provider default)
	clusterName := p.config.DefaultCluster
	cluster, err := p.finder.ClusterComputeResource(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("failed to find cluster '%s': %w", clusterName, err)
	}

	resourcePool, err := cluster.ResourcePool(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get resource pool from cluster: %w", err)
	}

	// Determine which datastore to use (provider default)
	datastoreName := p.config.DefaultDatastore
	datastore, err := p.finder.Datastore(ctx, datastoreName)
	if err != nil {
		return nil, fmt.Errorf("failed to find datastore '%s': %w", datastoreName, err)
	}

	// Determine which folder to use (provider default)
	folderName := p.config.DefaultFolder
	folder, err := p.finder.Folder(ctx, folderName)
	if err != nil {
		// If folder doesn't exist, use the datacenter's default VM folder
		p.logger.Warn("Failed to find folder, using datacenter default VM folder", "folder", folderName, "error", err)
		folder, err = p.finder.Folder(ctx, datacenter.Name()+"/vm")
		if err != nil {
			return nil, fmt.Errorf("failed to find datacenter VM folder: %w", err)
		}
	}

	// Create the clone specification
	cloneSpec := &types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Datastore: types.NewReference(datastore.Reference()),
			Pool:      types.NewReference(resourcePool.Reference()),
		},
		PowerOn:  false, // Don't power on automatically
		Template: false,
	}

	// Handle linked clone if requested
	if req.Linked {
		p.logger.Info("Creating linked clone", "source_vm_id", req.SourceVmId)

		// For linked clone, we need to specify a snapshot
		// Get the current snapshot or create one
		var sourceVMObj mo.VirtualMachine
		err = sourceVM.Properties(ctx, sourceVM.Reference(), []string{"snapshot"}, &sourceVMObj)
		if err != nil {
			return nil, fmt.Errorf("failed to get source VM properties: %w", err)
		}

		var snapshotRef *types.ManagedObjectReference
		if sourceVMObj.Snapshot != nil && len(sourceVMObj.Snapshot.RootSnapshotList) > 0 {
			// Use the most recent snapshot
			snapshots := sourceVMObj.Snapshot.RootSnapshotList
			latestSnapshot := &snapshots[len(snapshots)-1]
			snapshotRef = &latestSnapshot.Snapshot
			p.logger.Info("Using existing snapshot for linked clone", "snapshot", latestSnapshot.Name)
		} else {
			// Create a snapshot for the linked clone
			p.logger.Info("Creating snapshot for linked clone", "source_vm_id", req.SourceVmId)
			snapshotName := fmt.Sprintf("clone-base-%d", time.Now().Unix())
			snapshotTask, err := sourceVM.CreateSnapshot(ctx, snapshotName, "Snapshot for linked clone", false, false)
			if err != nil {
				return nil, fmt.Errorf("failed to create snapshot for linked clone: %w", err)
			}

			taskInfo, err := snapshotTask.WaitForResult(ctx, nil)
			if err != nil {
				return nil, fmt.Errorf("snapshot creation failed: %w", err)
			}

			if taskInfo.Result == nil {
				return nil, fmt.Errorf("snapshot creation completed but no snapshot reference returned")
			}

			snapshotRefResult, ok := taskInfo.Result.(types.ManagedObjectReference)
			if !ok {
				return nil, fmt.Errorf("unexpected snapshot task result type: %T", taskInfo.Result)
			}
			snapshotRef = &snapshotRefResult
		}

		// Set up linked clone with snapshot
		cloneSpec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsCreateNewChildDiskBacking)
		cloneSpec.Snapshot = snapshotRef
	} else {
		// Full clone
		p.logger.Info("Creating full clone", "source_vm_id", req.SourceVmId)
		cloneSpec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndAllowSharing)
	}

	// Perform the clone operation
	p.logger.Info("Cloning virtual machine", "source_vm_id", req.SourceVmId, "target_name", req.TargetName)

	cloneTask, err := sourceVM.Clone(ctx, folder, req.TargetName, *cloneSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to start clone operation: %w", err)
	}

	// Wait for the clone task to complete
	taskInfo, err := cloneTask.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("clone task failed: %w", err)
	}

	// Get the new VM reference
	targetVMRef, ok := taskInfo.Result.(types.ManagedObjectReference)
	if !ok {
		return nil, fmt.Errorf("unexpected result type from clone task: %T", taskInfo.Result)
	}

	// Get the target VM's managed object ID
	targetVMID := targetVMRef.Value

	p.logger.Info("Virtual machine cloned successfully", "source_vm_id", req.SourceVmId, "target_vm_id", targetVMID, "target_name", req.TargetName)

	return &providerv1.CloneResponse{
		TargetVmId: targetVMID,
		// No task reference since we completed synchronously
	}, nil
}

// ImagePrepare implements the ProviderServer interface. Image preparation (converting
// an external image into a vSphere template) is not yet implemented for this provider;
// the method always returns an Unimplemented error.
func (p *Provider) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.TaskResponse, error) {
	return nil, errors.NewUnimplemented("ImagePrepare operation not yet implemented for vSphere")
}

// VMSpec represents the parsed virtual machine specification
type VMSpec struct {
	Name                        string
	CPU                         int32
	MemoryMB                    int64
	DiskSizeGB                  int64
	DiskType                    string
	TemplateName                string
	DiskPath                    string // Path to existing disk (for imported disks)
	DiskFormat                  string // Format of existing disk (for imported disks)
	NetworkName                 string
	Firmware                    string
	HardwareVersion             *int32 // VM hardware compatibility version
	CloudInit                   string // Cloud-init user data
	CloudInitMetaData           string // Cloud-init metadata
	NestedVirtualization        bool   // Enable nested virtualization
	VirtualizationBasedSecurity bool   // Enable VBS features
	CPUHotAddEnabled            bool   // Enable CPU hot-add
	MemoryHotAddEnabled         bool   // Enable memory hot-add
	SecureBoot                  bool   // Enable secure boot
	TPMEnabled                  bool   // Enable TPM
	VTDEnabled                  bool   // Enable Intel VT-d or AMD-Vi
	// Additional disks beyond the root disk
	AdditionalDisks []AdditionalDiskSpec
	// Placement overrides
	Cluster    string // Cluster override (empty = use provider default)
	Datastore  string // Datastore override (empty = use provider default)
	StoragePod string // Datastore Cluster override (empty = use provider default; ignored when Datastore is set)
	Folder     string // Folder override (empty = use provider default)
	Host       string // Host override (empty = use provider default)
}

// AdditionalDiskSpec defines an additional disk to attach to a VM
type AdditionalDiskSpec struct {
	Name               string
	SizeGiB            int32
	Type               string
	SCSIController     *int32 // SCSI controller bus number (0-3), nil = auto-select
	SCSISharedBus      string // SCSI bus sharing: noSharing, virtualSharing, physicalSharing
	SCSIControllerType string // SCSI controller type: lsilogic, buslogic, lsilogic-sas, pvscsi
}

// parseCreateRequest deserialises the JSON-encoded fields of a CreateRequest into a
// VMSpec. The following fields are parsed:
//
//   - req.ClassJson  — contracts.VMClass: CPU, MemoryMiB, Firmware, ExtraConfig
//     (vsphere.hardwareVersion key), DiskDefaults, PerformanceProfile (nested virt,
//     VBS, CPU/memory hot-add), SecurityProfile (SecureBoot, TPM, VT-d).
//   - req.ImageJson  — contracts.VMImage: TemplateName (for template clones) or Path +
//     Format (for imported-disk VMs). When Path is non-empty, disk-based creation is
//     used and TemplateName is ignored.
//   - req.NetworksJson — []contracts.NetworkAttachment: only the first element's
//     NetworkName is used to attach a single network adapter.
//   - req.UserData    — raw cloud-init user-data bytes.
//   - req.MetaData    — raw cloud-init metadata bytes.
//   - req.PlacementJson — contracts.Placement: optional per-VM overrides for Cluster,
//     Datastore, StoragePod, Folder, and Host.
//   - req.DisksJson — []contracts.DiskSpec: additional disks to attach beyond the root disk.
//
// Returns an error if any JSON field is present but cannot be unmarshalled.
func (p *Provider) parseCreateRequest(req *providerv1.CreateRequest) (*VMSpec, error) {
	p.logger.Info("Starting parseCreateRequest",
		"name", req.Name,
		"hasClassJson", req.ClassJson != "",
		"hasImageJson", req.ImageJson != "",
		"hasPlacementJson", req.PlacementJson != "",
		"placementJsonLength", len(req.PlacementJson))

	spec := &VMSpec{
		Name: req.Name,
	}

	// Parse VMClass from JSON (contracts.VMClass structure)
	if req.ClassJson != "" {
		var vmClass struct {
			CPU          int32             `json:"CPU"`
			MemoryMiB    int32             `json:"MemoryMiB"`
			Firmware     string            `json:"Firmware"`
			ExtraConfig  map[string]string `json:"ExtraConfig"`
			DiskDefaults *struct {
				Type    string `json:"Type"`
				SizeGiB int32  `json:"SizeGiB"`
			} `json:"DiskDefaults"`
			PerformanceProfile *struct {
				NestedVirtualization        bool `json:"NestedVirtualization"`
				VirtualizationBasedSecurity bool `json:"VirtualizationBasedSecurity"`
				CPUHotAddEnabled            bool `json:"CPUHotAddEnabled"`
				MemoryHotAddEnabled         bool `json:"MemoryHotAddEnabled"`
			} `json:"PerformanceProfile"`
			SecurityProfile *struct {
				SecureBoot bool `json:"SecureBoot"`
				TPMEnabled bool `json:"TPMEnabled"`
				VTDEnabled bool `json:"VTDEnabled"`
			} `json:"SecurityProfile"`
		}

		if err := json.Unmarshal([]byte(req.ClassJson), &vmClass); err != nil {
			return nil, fmt.Errorf("failed to parse VMClass JSON: %w", err)
		}

		spec.CPU = vmClass.CPU
		spec.MemoryMB = int64(vmClass.MemoryMiB) // Convert MiB to MB (same value)
		spec.Firmware = vmClass.Firmware

		// Parse hardware version from ExtraConfig (vSphere-specific)
		if vmClass.ExtraConfig != nil {
			if hwVersionStr, exists := vmClass.ExtraConfig["vsphere.hardwareVersion"]; exists && hwVersionStr != "" {
				if hwVersion, err := strconv.ParseInt(hwVersionStr, 10, 32); err == nil {
					hwVersionInt32 := int32(hwVersion)
					spec.HardwareVersion = &hwVersionInt32
					p.logger.Info("Parsed hardware version from ExtraConfig", "version", hwVersion, "vm_name", spec.Name)
				} else {
					p.logger.Warn("Invalid hardware version in ExtraConfig", "value", hwVersionStr, "vm_name", spec.Name)
				}
			}
		}

		if vmClass.DiskDefaults != nil {
			spec.DiskType = vmClass.DiskDefaults.Type
			spec.DiskSizeGB = int64(vmClass.DiskDefaults.SizeGiB) // Convert GiB to GB (same value)
		}

		// Parse PerformanceProfile
		if vmClass.PerformanceProfile != nil {
			spec.NestedVirtualization = vmClass.PerformanceProfile.NestedVirtualization
			spec.VirtualizationBasedSecurity = vmClass.PerformanceProfile.VirtualizationBasedSecurity
			spec.CPUHotAddEnabled = vmClass.PerformanceProfile.CPUHotAddEnabled
			spec.MemoryHotAddEnabled = vmClass.PerformanceProfile.MemoryHotAddEnabled
		}

		// Parse SecurityProfile
		if vmClass.SecurityProfile != nil {
			spec.SecureBoot = vmClass.SecurityProfile.SecureBoot
			spec.TPMEnabled = vmClass.SecurityProfile.TPMEnabled
			spec.VTDEnabled = vmClass.SecurityProfile.VTDEnabled
		}
	}

	// Parse VMImage from JSON (contracts.VMImage structure)
	if req.ImageJson != "" {
		var vmImage struct {
			TemplateName string `json:"TemplateName"`
			Path         string `json:"Path"`
			Format       string `json:"Format"`
		}

		if err := json.Unmarshal([]byte(req.ImageJson), &vmImage); err != nil {
			return nil, fmt.Errorf("failed to parse VMImage JSON: %w", err)
		}

		// If Path is set, this is an imported disk (not a template)
		if vmImage.Path != "" {
			spec.DiskPath = vmImage.Path
			spec.DiskFormat = vmImage.Format
			if spec.DiskFormat == "" {
				spec.DiskFormat = "vmdk" // Default for vSphere
			}
		} else {
			// Otherwise, it's a template-based VM
			spec.TemplateName = vmImage.TemplateName
		}
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

	// Parse MetaData (cloud-init metadata)
	if len(req.MetaData) > 0 {
		spec.CloudInitMetaData = string(req.MetaData)
	}

	// Parse Placement from JSON (contracts.Placement structure)
	if req.PlacementJson != "" {
		p.logger.Info("Parsing placement JSON", "json", req.PlacementJson, "vm_name", spec.Name)

		var placement struct {
			Cluster    string `json:"Cluster"`
			Datastore  string `json:"Datastore"`
			StoragePod string `json:"StoragePod"`
			Folder     string `json:"Folder"`
			Host       string `json:"Host"`
		}

		if err := json.Unmarshal([]byte(req.PlacementJson), &placement); err != nil {
			return nil, fmt.Errorf("failed to parse Placement JSON: %w", err)
		}

		p.logger.Info("Parsed placement data",
			"cluster", placement.Cluster,
			"datastore", placement.Datastore,
			"storagePod", placement.StoragePod,
			"folder", placement.Folder,
			"host", placement.Host,
			"vm_name", spec.Name)

		// Set placement overrides if specified
		spec.Cluster = placement.Cluster
		spec.Datastore = placement.Datastore
		spec.StoragePod = placement.StoragePod
		spec.Folder = placement.Folder
		spec.Host = placement.Host
	}

	// Parse Disks from JSON ([]contracts.DiskSpec structure)
	if req.DisksJson != "" {
		var disks []struct {
			Name    string `json:"Name"`
			SizeGiB int32  `json:"SizeGiB"`
			Type    string `json:"Type"`
			SCSI    *struct {
				Controller     *int32 `json:"controller"`
				SharedBus      string `json:"sharedBus"`
				ControllerType string `json:"controllerType"`
			} `json:"SCSI"`
		}

		if err := json.Unmarshal([]byte(req.DisksJson), &disks); err != nil {
			return nil, fmt.Errorf("failed to parse Disks JSON: %w", err)
		}

		p.logger.Info("Parsed additional disks", "count", len(disks), "vm_name", spec.Name)

		for _, disk := range disks {
			diskSpec := AdditionalDiskSpec{
				Name:    disk.Name,
				SizeGiB: disk.SizeGiB,
				Type:    disk.Type,
			}

			// Parse SCSI controller configuration if provided
			if disk.SCSI != nil {
				diskSpec.SCSIController = disk.SCSI.Controller
				diskSpec.SCSISharedBus = disk.SCSI.SharedBus
				diskSpec.SCSIControllerType = disk.SCSI.ControllerType
			}

			spec.AdditionalDisks = append(spec.AdditionalDisks, diskSpec)
		}
	}

	p.logger.Info("Finished parseCreateRequest",
		"name", spec.Name,
		"cluster", spec.Cluster,
		"datastore", spec.Datastore,
		"storagePod", spec.StoragePod,
		"folder", spec.Folder,
		"host", spec.Host,
		"additional_disks", len(spec.AdditionalDisks))

	return spec, nil
}

// createVirtualMachine provisions a new VM in vSphere according to spec and returns
// the ManagedObjectReference value ("vm-N") of the created VM.
//
// Resource placement follows a priority chain for each dimension:
//   - Cluster:   spec.Cluster → p.config.DefaultCluster
//   - Datastore: spec.Datastore → StoragePod (spec.StoragePod or p.config.DefaultStoragePod) → p.config.DefaultDatastore
//   - Folder:    spec.Folder   → p.config.DefaultFolder → datacenter default VM folder
//
// VM creation path:
//   - If spec.DiskPath is set: CreateVM_Task with an attached existing VMDK and an LSI
//     Logic SCSI controller added to DeviceChange.
//   - Otherwise: CloneVM_Task from the template named spec.TemplateName.
//
// In both cases, cloud-init data (if provided) is embedded via addCloudInitToConfigSpec
// before the task is submitted so that guestinfo properties are set at creation time.
// After the task completes, the primary disk is optionally grown via resizeVMDisk and
// the VM is powered on.
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

	// Find the template VM (only if not using an imported disk)
	var template *object.VirtualMachine
	if spec.DiskPath == "" {
		if spec.TemplateName == "" {
			return "", fmt.Errorf("either templateName or diskPath must be specified")
		}
		template, err = p.finder.VirtualMachine(ctx, spec.TemplateName)
		if err != nil {
			return "", fmt.Errorf("failed to find template VM '%s': %w", spec.TemplateName, err)
		}
	} else {
		// Using imported disk - skip template lookup
		p.logger.Info("Using imported disk, skipping template lookup", "disk_path", spec.DiskPath, "disk_format", spec.DiskFormat)
	}

	// Determine which cluster to use (spec override or provider default)
	clusterName := p.config.DefaultCluster
	if spec.Cluster != "" {
		clusterName = spec.Cluster
		p.logger.Info("Using placement override for cluster", "cluster", clusterName)
	}

	// Find the cluster and resource pool
	cluster, err := p.finder.ClusterComputeResource(ctx, clusterName)
	if err != nil {
		return "", fmt.Errorf("failed to find cluster '%s': %w", clusterName, err)
	}

	resourcePool, err := cluster.ResourcePool(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get resource pool from cluster: %w", err)
	}

	// Determine which datastore to use (spec override, StoragePod, or provider default)
	var datastore *object.Datastore
	p.logger.Info("Determining datastore placement",
		"spec.Datastore", spec.Datastore,
		"spec.StoragePod", spec.StoragePod,
		"config.DefaultStoragePod", p.config.DefaultStoragePod,
		"config.DefaultDatastore", p.config.DefaultDatastore,
		"vm_name", spec.Name)

	if spec.Datastore != "" {
		p.logger.Info("Using placement override for datastore", "datastore", spec.Datastore)
		datastore, err = p.finder.Datastore(ctx, spec.Datastore)
		if err != nil {
			return "", fmt.Errorf("failed to find datastore '%s': %w", spec.Datastore, err)
		}
	} else {
		storagePodName := p.config.DefaultStoragePod
		if spec.StoragePod != "" {
			storagePodName = spec.StoragePod
			p.logger.Info("Using placement override for StoragePod", "storagePod", storagePodName, "vm_name", spec.Name)
		}
		if storagePodName != "" {
			p.logger.Info("Resolving datastore from StoragePod", "storagePod", storagePodName)
			datastore, err = p.resolveDatastoreFromStoragePod(ctx, storagePodName)
			if err != nil {
				return "", fmt.Errorf("failed to resolve datastore from StoragePod '%s': %w", storagePodName, err)
			}
			p.logger.Info("Successfully resolved datastore from StoragePod",
				"storagePod", storagePodName,
				"datastore", datastore.Name(),
				"vm_name", spec.Name)
		} else {
			p.logger.Info("No StoragePod specified, using default datastore",
				"datastore", p.config.DefaultDatastore,
				"vm_name", spec.Name)
			datastore, err = p.finder.Datastore(ctx, p.config.DefaultDatastore)
			if err != nil {
				return "", fmt.Errorf("failed to find datastore '%s': %w", p.config.DefaultDatastore, err)
			}
		}
	}

	// Determine which folder to use (spec override or provider default)
	folderName := p.config.DefaultFolder
	if spec.Folder != "" {
		folderName = spec.Folder
		p.logger.Info("Using placement override for folder", "folder", folderName)
	}

	// Find the folder
	folder, err := p.finder.Folder(ctx, folderName)
	if err != nil {
		// If folder doesn't exist, use the datacenter's default VM folder
		p.logger.Warn("Failed to find folder, using datacenter default VM folder", "folder", folderName, "error", err)
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

	// Set hardware version if specified
	if spec.HardwareVersion != nil {
		// Convert hardware version to VMX format (e.g., 21 -> "vmx-21")
		configSpec.Version = fmt.Sprintf("vmx-%d", *spec.HardwareVersion)
		p.logger.Info("Setting VM hardware version", "version", configSpec.Version, "vm_name", spec.Name)
	}

	// Configure performance and security features
	var extraConfig []types.BaseOptionValue

	// Enable nested virtualization if requested
	if spec.NestedVirtualization {
		p.logger.Info("Enabling nested virtualization", "vm_name", spec.Name)
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "vhv.enable",
			Value: "TRUE",
		})
		// Also enable nested page tables for better performance
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "vhv.allowNestedPageTables",
			Value: "TRUE",
		})
	}

	// Enable VBS (Virtualization Based Security) if requested
	if spec.VirtualizationBasedSecurity {
		p.logger.Info("Enabling Virtualization Based Security", "vm_name", spec.Name)
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "vbs.enable",
			Value: "TRUE",
		})
	}

	// Enable Intel VT-d or AMD-Vi if requested
	if spec.VTDEnabled {
		p.logger.Info("Enabling VT-d/AMD-Vi", "vm_name", spec.Name)
		extraConfig = append(extraConfig, &types.OptionValue{
			Key:   "vvtd.enable",
			Value: "TRUE",
		})
	}

	// Configure CPU and memory hot-add
	// Note: CPU hot-add is incompatible with nested virtualization
	if spec.CPUHotAddEnabled {
		if spec.NestedVirtualization {
			p.logger.Warn("CPU hot-add is incompatible with nested virtualization, skipping CPU hot-add", "vm_name", spec.Name)
		} else {
			p.logger.Info("Enabling CPU hot-add", "vm_name", spec.Name)
			configSpec.CpuHotAddEnabled = &spec.CPUHotAddEnabled
		}
	}

	if spec.MemoryHotAddEnabled {
		p.logger.Info("Enabling memory hot-add", "vm_name", spec.Name)
		configSpec.MemoryHotAddEnabled = &spec.MemoryHotAddEnabled
	}

	// Configure secure boot and TPM
	if spec.SecureBoot || spec.TPMEnabled {
		// These features require UEFI firmware
		if spec.Firmware == "" || strings.ToUpper(spec.Firmware) != "UEFI" {
			p.logger.Warn("Secure Boot and TPM require UEFI firmware, forcing UEFI", "vm_name", spec.Name)
			configSpec.Firmware = string(types.GuestOsDescriptorFirmwareTypeEfi)
		}

		if spec.SecureBoot {
			p.logger.Info("Enabling Secure Boot", "vm_name", spec.Name)
			configSpec.BootOptions = &types.VirtualMachineBootOptions{
				EfiSecureBootEnabled: &spec.SecureBoot,
			}
		}

		if spec.TPMEnabled {
			p.logger.Info("Enabling TPM", "vm_name", spec.Name)
			// Add TPM device - this requires vSphere 6.7+ and hardware version 14+
			tpmDevice := &types.VirtualTPM{
				VirtualDevice: types.VirtualDevice{
					Key: -1, // Auto-assign key
					DeviceInfo: &types.Description{
						Label:   "TPM",
						Summary: "Trusted Platform Module",
					},
				},
			}

			deviceChange := &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    tpmDevice,
			}

			configSpec.DeviceChange = append(configSpec.DeviceChange, deviceChange)
		}
	}

	// Apply extra configuration if any
	if len(extraConfig) > 0 {
		configSpec.ExtraConfig = extraConfig
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

	var vmRef types.ManagedObjectReference
	var vmID string

	if spec.DiskPath != "" {
		// Using imported disk - create VM from scratch and attach existing disk
		p.logger.Info("Creating VM with imported disk", "disk_path", spec.DiskPath, "target", spec.Name)

		// Set VM name in config spec FIRST (required for CreateVM and cloud-init)
		configSpec.Name = spec.Name

		// Add cloud-init data via guestinfo properties if provided
		// Note: Must be called AFTER setting Name for imported disk VMs
		if spec.CloudInit != "" {
			if err := p.addCloudInitToConfigSpec(configSpec, spec.CloudInit, spec.CloudInitMetaData); err != nil {
				p.logger.Warn("Failed to add cloud-init configuration", "error", err)
				// Continue without cloud-init rather than failing
			} else {
				p.logger.Info("Added cloud-init configuration to VM", "vm_name", spec.Name)
			}
		}

		// Create VM config spec with disk attachment
		// Parse datastore path: [datastore] path/file.vmdk
		diskBacking := &types.VirtualDiskFlatVer2BackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
				FileName: spec.DiskPath,
			},
			DiskMode: string(types.VirtualDiskModePersistent),
		}

		// Add disk device
		diskDevice := &types.VirtualDisk{
			VirtualDevice: types.VirtualDevice{
				Key:     -1,
				Backing: diskBacking,
			},
		}

		// Add SCSI controller
		scsiController := &types.VirtualLsiLogicController{
			VirtualSCSIController: types.VirtualSCSIController{
				SharedBus: types.VirtualSCSISharingNoSharing, // Required: set sharing mode for SCSI controller
				VirtualController: types.VirtualController{
					VirtualDevice: types.VirtualDevice{
						Key: -1,
					},
					BusNumber: 0,
				},
			},
		}

		configSpec.DeviceChange = append(configSpec.DeviceChange,
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    scsiController,
			},
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    diskDevice,
			},
		)

		// Create VM using CreateVM_Task
		vmFolder := folder
		task, err := vmFolder.CreateVM(ctx, *configSpec, resourcePool, nil)
		if err != nil {
			return "", fmt.Errorf("failed to create VM: %w", err)
		}

		info, err := task.WaitForResult(ctx, nil)
		if err != nil {
			return "", fmt.Errorf("VM creation task failed: %w", err)
		}

		vmRef, ok := info.Result.(types.ManagedObjectReference)
		if !ok {
			return "", fmt.Errorf("unexpected result type from create task: %T", info.Result)
		}

		vmID = vmRef.Value
		p.logger.Info("Virtual machine created successfully with imported disk", "vm_id", vmID, "name", spec.Name)
	} else {
		// Using template - clone from template
		// Set VM name for template-based VMs (needed for cloud-init)
		configSpec.Name = spec.Name

		// Add cloud-init data via guestinfo properties if provided
		if spec.CloudInit != "" {
			if err := p.addCloudInitToConfigSpec(configSpec, spec.CloudInit, spec.CloudInitMetaData); err != nil {
				p.logger.Warn("Failed to add cloud-init configuration", "error", err)
				// Continue without cloud-init rather than failing
			} else {
				p.logger.Info("Added cloud-init configuration to VM", "vm_name", spec.Name)
			}
		}

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
		var ok bool
		vmRef, ok = info.Result.(types.ManagedObjectReference)
		if !ok {
			return "", fmt.Errorf("unexpected result type from clone task: %T", info.Result)
		}

		// Get the VM's managed object ID for returning
		vmID = vmRef.Value

		p.logger.Info("Virtual machine created successfully", "vm_id", vmID, "name", spec.Name)
	}

	// Get the new VM object for further operations
	newVM := object.NewVirtualMachine(p.client.Client, vmRef)

	// NOTE: extraConfig and cloud-init are already applied during CloneVM_Task above
	// No post-clone reconfiguration needed - rely on clone-time settings

	// Resize disk if specified in VMClass
	if spec.DiskSizeGB > 0 {
		if err := p.resizeVMDisk(ctx, newVM, spec.DiskSizeGB, vmID); err != nil {
			p.logger.Warn("Failed to resize VM disk", "vm_id", vmID, "target_size_gb", spec.DiskSizeGB, "error", err)
			// Don't fail the entire creation if disk resize fails
		}
	}

	// Attach additional disks if specified
	if len(spec.AdditionalDisks) > 0 {
		p.logger.Debug("Starting additional disk attachment",
			"vm_id", vmID,
			"count", len(spec.AdditionalDisks))

		for i, diskSpec := range spec.AdditionalDisks {
			p.logger.Debug("Attaching disk",
				"vm_id", vmID,
				"disk_index", i+1,
				"disk_name", diskSpec.Name,
				"size_gib", diskSpec.SizeGiB,
				"type", diskSpec.Type)

			if err := p.attachAdditionalDisk(ctx, newVM, diskSpec, datacenter, datastore, i+1); err != nil {
				p.logger.Error("Failed to attach additional disk",
					"vm_id", vmID,
					"disk_name", diskSpec.Name,
					"disk_index", i+1,
					"error", err)
				// Continue with remaining disks rather than failing entire creation
			} else {
				p.logger.Debug("Successfully attached additional disk",
					"vm_id", vmID,
					"disk_name", diskSpec.Name,
					"disk_index", i+1,
					"size_gb", diskSpec.SizeGiB)
			}
		}
	} else {
		p.logger.Debug("No additional disks to attach", "vm_id", vmID)
	}

	// Power on the VM if requested (VirtualMachine spec.powerState: "On")
	// Note: This is a simple implementation - in production you might want to check the actual powerState from the request
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

// attachAdditionalDisk attaches a new disk to an existing VM
func (p *Provider) attachAdditionalDisk(
	ctx context.Context,
	vm *object.VirtualMachine,
	diskSpec AdditionalDiskSpec,
	datacenter *object.Datacenter,
	datastore *object.Datastore,
	diskIndex int,
) error {
	p.logger.Info("Attaching additional disk",
		"disk_name", diskSpec.Name,
		"size_gb", diskSpec.SizeGiB,
		"type", diskSpec.Type,
		"disk_index", diskIndex,
		"scsi_controller", diskSpec.SCSIController,
		"scsi_type", diskSpec.SCSIControllerType,
		"scsi_shared_bus", diskSpec.SCSISharedBus)

	// Get current VM configuration to find available controller and unit number
	var vmMo mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"config.hardware.device", "config.name"}, &vmMo)
	if err != nil {
		return fmt.Errorf("failed to get VM properties: %w", err)
	}

	// Find SCSI controllers indexed by bus number
	scsiControllers := make(map[int32]types.BaseVirtualSCSIController)
	controllerKeys := make(map[int32]int32)           // busNumber -> controllerKey
	usedUnitNumbers := make(map[int32]map[int32]bool) // controllerKey -> unitNumber -> used

	for _, device := range vmMo.Config.Hardware.Device {
		if scsiCtrl, ok := device.(types.BaseVirtualSCSIController); ok {
			ctrl := scsiCtrl.GetVirtualSCSIController()
			busNumber := ctrl.BusNumber
			scsiControllers[busNumber] = scsiCtrl
			controllerKeys[busNumber] = ctrl.Key
			usedUnitNumbers[ctrl.Key] = make(map[int32]bool)
		}
	}

	// Track used unit numbers for each controller
	for _, device := range vmMo.Config.Hardware.Device {
		if disk, ok := device.(*types.VirtualDisk); ok {
			if disk.UnitNumber != nil {
				if units, exists := usedUnitNumbers[disk.ControllerKey]; exists {
					units[*disk.UnitNumber] = true
				}
			}
		}
	}

	// Determine which controller to use
	var targetControllerKey int32
	var targetBusNumber int32
	var needsNewController bool

	if diskSpec.SCSIController != nil {
		// Specific controller requested
		targetBusNumber = *diskSpec.SCSIController
		if key, exists := controllerKeys[targetBusNumber]; exists {
			// Controller exists
			targetControllerKey = key
			p.logger.Debug("Using existing SCSI controller",
				"bus_number", targetBusNumber,
				"controller_key", targetControllerKey)
		} else {
			// Need to create new controller
			needsNewController = true
			p.logger.Info("Need to create new SCSI controller",
				"bus_number", targetBusNumber,
				"type", diskSpec.SCSIControllerType,
				"shared_bus", diskSpec.SCSISharedBus)
		}
	} else {
		// Auto-select: Use first available SCSI controller
		if len(scsiControllers) == 0 {
			return fmt.Errorf("no SCSI controller found in VM")
		}
		// Get first controller (bus 0)
		if key, exists := controllerKeys[0]; exists {
			targetControllerKey = key
			targetBusNumber = 0
		} else {
			return fmt.Errorf("no SCSI controller found on bus 0")
		}
	}

	// Create new SCSI controller if needed
	if needsNewController {
		newKey, err := p.createSCSIController(ctx, vm, targetBusNumber, diskSpec.SCSIControllerType, diskSpec.SCSISharedBus)
		if err != nil {
			return fmt.Errorf("failed to create SCSI controller: %w", err)
		}
		targetControllerKey = newKey
		usedUnitNumbers[newKey] = make(map[int32]bool)
		p.logger.Info("Created new SCSI controller",
			"bus_number", targetBusNumber,
			"controller_key", newKey)
	}

	// Find next available unit number on target controller (0-15, but 7 is reserved)
	var unitNumber int32 = -1
	units := usedUnitNumbers[targetControllerKey]
	for i := int32(0); i < 16; i++ {
		if i == 7 {
			continue // Reserved for SCSI controller itself
		}
		if !units[i] {
			unitNumber = i
			break
		}
	}

	if unitNumber == -1 {
		return fmt.Errorf("no available unit numbers on SCSI controller (all 15 slots used)")
	}

	// Construct datastore path for new disk
	// Format: [datastore] vm-name/vm-name_N.vmdk
	vmName := vmMo.Config.Name
	diskFileName := fmt.Sprintf("%s_%d.vmdk", vmName, diskIndex)
	diskPath := fmt.Sprintf("[%s] %s/%s", datastore.Name(), vmName, diskFileName)

	p.logger.Info("Creating disk file",
		"path", diskPath,
		"unit_number", unitNumber,
		"controller_key", targetControllerKey)

	// Determine disk backing type from spec
	var diskMode string = string(types.VirtualDiskModePersistent)
	var thinProvisioned *bool

	switch strings.ToLower(diskSpec.Type) {
	case "thin":
		thin := true
		thinProvisioned = &thin
	case "thick", "eagerzeroedthick":
		thick := false
		thinProvisioned = &thick
	default:
		// Default to thin provisioning
		thin := true
		thinProvisioned = &thin
	}

	// Create disk backing
	diskBacking := &types.VirtualDiskFlatVer2BackingInfo{
		VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
			FileName:  diskPath,
			Datastore: types.NewReference(datastore.Reference()),
		},
		DiskMode:        diskMode,
		ThinProvisioned: thinProvisioned,
	}

	// For eager zeroed thick, set the flag
	if strings.ToLower(diskSpec.Type) == "eagerzeroedthick" {
		eagerScrub := true
		diskBacking.EagerlyScrub = &eagerScrub
	}

	// Create the disk device
	diskDevice := &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Key:           -1, // Auto-assign
			ControllerKey: targetControllerKey,
			UnitNumber:    &unitNumber,
			Backing:       diskBacking,
			DeviceInfo: &types.Description{
				Label:   diskSpec.Name,
				Summary: fmt.Sprintf("%d GB disk", diskSpec.SizeGiB),
			},
		},
		CapacityInKB: int64(diskSpec.SizeGiB) * 1024 * 1024, // Convert GiB to KB
	}

	// Create device change spec
	deviceSpec := &types.VirtualDeviceConfigSpec{
		Operation:     types.VirtualDeviceConfigSpecOperationAdd,
		FileOperation: types.VirtualDeviceConfigSpecFileOperationCreate,
		Device:        diskDevice,
	}

	// Create reconfigure spec
	configSpec := &types.VirtualMachineConfigSpec{
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{deviceSpec},
	}

	// Perform the reconfiguration to add the disk
	task, err := vm.Reconfigure(ctx, *configSpec)
	if err != nil {
		return fmt.Errorf("failed to start disk attachment task: %w", err)
	}

	// Wait for reconfiguration to complete
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return fmt.Errorf("disk attachment task failed: %w", err)
	}

	p.logger.Info("Disk attached successfully",
		"disk_name", diskSpec.Name,
		"path", diskPath,
		"size_gb", diskSpec.SizeGiB)

	return nil
}

// createSCSIController creates a new SCSI controller on the VM
func (p *Provider) createSCSIController(
	ctx context.Context,
	vm *object.VirtualMachine,
	busNumber int32,
	controllerType string,
	sharedBus string,
) (int32, error) {
	// Set defaults
	if controllerType == "" {
		controllerType = "pvscsi"
	}
	if sharedBus == "" {
		sharedBus = "noSharing"
	}

	// Create appropriate controller type
	var controller types.BaseVirtualDevice

	switch strings.ToLower(controllerType) {
	case "pvscsi":
		ctrl := &types.ParaVirtualSCSIController{
			VirtualSCSIController: types.VirtualSCSIController{
				SharedBus: types.VirtualSCSISharing(sharedBus),
				VirtualController: types.VirtualController{
					BusNumber: busNumber,
					VirtualDevice: types.VirtualDevice{
						Key: -1, // Auto-assign key
					},
				},
			},
		}
		controller = ctrl
	case "lsilogic":
		ctrl := &types.VirtualLsiLogicController{
			VirtualSCSIController: types.VirtualSCSIController{
				SharedBus: types.VirtualSCSISharing(sharedBus),
				VirtualController: types.VirtualController{
					BusNumber: busNumber,
					VirtualDevice: types.VirtualDevice{
						Key: -1,
					},
				},
			},
		}
		controller = ctrl
	case "lsilogic-sas":
		ctrl := &types.VirtualLsiLogicSASController{
			VirtualSCSIController: types.VirtualSCSIController{
				SharedBus: types.VirtualSCSISharing(sharedBus),
				VirtualController: types.VirtualController{
					BusNumber: busNumber,
					VirtualDevice: types.VirtualDevice{
						Key: -1,
					},
				},
			},
		}
		controller = ctrl
	case "buslogic":
		ctrl := &types.VirtualBusLogicController{
			VirtualSCSIController: types.VirtualSCSIController{
				SharedBus: types.VirtualSCSISharing(sharedBus),
				VirtualController: types.VirtualController{
					BusNumber: busNumber,
					VirtualDevice: types.VirtualDevice{
						Key: -1,
					},
				},
			},
		}
		controller = ctrl
	default:
		return 0, fmt.Errorf("unsupported SCSI controller type: %s", controllerType)
	}

	// Create device change spec
	deviceSpec := &types.VirtualDeviceConfigSpec{
		Operation: types.VirtualDeviceConfigSpecOperationAdd,
		Device:    controller,
	}

	// Create reconfigure spec
	configSpec := &types.VirtualMachineConfigSpec{
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{deviceSpec},
	}

	p.logger.Info("Creating SCSI controller",
		"bus_number", busNumber,
		"type", controllerType,
		"shared_bus", sharedBus)

	// Perform the reconfiguration
	task, err := vm.Reconfigure(ctx, *configSpec)
	if err != nil {
		return 0, fmt.Errorf("failed to start controller creation task: %w", err)
	}

	// Wait for reconfiguration to complete
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("controller creation task failed: %w", err)
	}

	// Extract the new controller key from the result
	// The key should be in the result, but we need to query the VM to get it
	var vmMo mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config.hardware.device"}, &vmMo); err != nil {
		return 0, fmt.Errorf("failed to get VM properties after controller creation: %w", err)
	}

	// Find the newly created controller by bus number
	for _, device := range vmMo.Config.Hardware.Device {
		if scsiCtrl, ok := device.(types.BaseVirtualSCSIController); ok {
			ctrl := scsiCtrl.GetVirtualSCSIController()
			if ctrl.BusNumber == busNumber {
				p.logger.Info("SCSI controller created successfully",
					"bus_number", busNumber,
					"controller_key", ctrl.Key,
					"type", controllerType)
				return ctrl.Key, nil
			}
		}
	}

	return 0, fmt.Errorf("failed to find newly created SCSI controller (bus %d)", busNumber)
}

// resizeVMDisk expands the primary disk (first VirtualDisk device) of vm to targetSizeGB
// gibibytes by issuing a ReconfigVM_Task with an edit device-change spec. The operation
// is grow-only: if the current allocated size is already equal to or greater than
// targetSizeGB the method returns nil without modifying the VM. Disk shrinks are not
// supported by the vSphere API and are silently skipped.
//
// vmID is used only for structured log messages and has no effect on the operation.
func (p *Provider) resizeVMDisk(ctx context.Context, vm *object.VirtualMachine, targetSizeGB int64, vmID string) error {
	p.logger.Info("Resizing VM disk", "vm_id", vmID, "target_size_gb", targetSizeGB)

	// Get current VM configuration to find the disk
	var vmMo mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"config.hardware.device"}, &vmMo)
	if err != nil {
		return fmt.Errorf("failed to get VM properties: %w", err)
	}

	// Find the primary disk (usually the first disk device)
	var primaryDisk *types.VirtualDisk
	for _, device := range vmMo.Config.Hardware.Device {
		if disk, ok := device.(*types.VirtualDisk); ok {
			primaryDisk = disk
			break
		}
	}

	if primaryDisk == nil {
		return fmt.Errorf("no disk found in VM")
	}

	// Get current disk size in bytes
	currentSizeKB := primaryDisk.CapacityInKB
	currentSizeGB := currentSizeKB / (1024 * 1024) // Convert KB to GB
	targetSizeKB := targetSizeGB * 1024 * 1024     // Convert GB to KB

	p.logger.Info("Disk size comparison",
		"vm_id", vmID,
		"current_size_gb", currentSizeGB,
		"target_size_gb", targetSizeGB)

	// Only resize if target is larger than current (don't shrink)
	if targetSizeGB <= currentSizeGB {
		p.logger.Info("Disk already at or larger than target size", "vm_id", vmID, "current_gb", currentSizeGB, "target_gb", targetSizeGB)
		return nil
	}

	// Create a new virtual disk device spec with updated size
	newDisk := *primaryDisk
	newDisk.CapacityInKB = targetSizeKB

	// Create device change spec for disk resize
	deviceSpec := &types.VirtualDeviceConfigSpec{
		Operation: types.VirtualDeviceConfigSpecOperationEdit,
		Device:    &newDisk,
	}

	// Create reconfigure spec
	configSpec := &types.VirtualMachineConfigSpec{
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{deviceSpec},
	}

	// Perform the reconfiguration
	task, err := vm.Reconfigure(ctx, *configSpec)
	if err != nil {
		return fmt.Errorf("failed to start disk resize task: %w", err)
	}

	// Wait for reconfiguration to complete
	_, err = task.WaitForResult(ctx, nil)
	if err != nil {
		return fmt.Errorf("disk resize task failed: %w", err)
	}

	p.logger.Info("Disk resized successfully", "vm_id", vmID, "from_gb", currentSizeGB, "to_gb", targetSizeGB)
	return nil
}

// GetDiskInfo implements the ProviderServer interface. It retrieves metadata about a
// specific virtual disk attached to the VM identified by req.VmId.
//
// Disk selection: req.DiskId is parsed as "disk-N" where N is the zero-based device
// index among all VirtualDisk devices in hardware order. An empty DiskId defaults to
// the primary disk (index 0).
//
// The response includes:
//   - DiskId: the device label from DeviceInfo.
//   - Format: always "vmdk" for vSphere.
//   - VirtualSizeBytes / ActualSizeBytes: derived from CapacityInKB (actual size is
//     approximated as equal to virtual size; querying the datastore for real allocation
//     is not implemented).
//   - Path: the datastore-qualified VMDK path from FlatVer2 backing.
//   - BackingFile: parent-disk path when the VM has snapshots (delta-chain base).
//   - IsBootable: true only for the first disk (index 0).
//   - Snapshots: flat list of snapshot names from the VM snapshot tree.
//   - Metadata: device key and SCSI unit number as string map entries.
func (p *Provider) GetDiskInfo(ctx context.Context, req *providerv1.GetDiskInfoRequest) (*providerv1.GetDiskInfoResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("vSphere client not configured", nil)
	}

	p.logger.Info("Getting disk info", "vm_id", req.VmId, "disk_id", req.DiskId)

	// Get VM reference
	vmRef := types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: req.VmId,
	}

	// Get VM configuration
	var vmMo mo.VirtualMachine
	pc := property.DefaultCollector(p.client.Client)
	err := pc.RetrieveOne(ctx, vmRef, []string{
		"config.hardware.device",
		"config.name",
		"snapshot",
	}, &vmMo)
	if err != nil {
		return nil, errors.NewNotFound("VM", req.VmId)
	}

	// Find the disk device (primary disk or specified disk)
	var targetDisk *types.VirtualDisk
	diskIndex := 0
	if req.DiskId != "" {
		// Try to find disk by label or index
		var idx int
		if _, err := fmt.Sscanf(req.DiskId, "disk-%d", &idx); err == nil {
			diskIndex = idx
		}
	}

	currentIndex := 0
	for _, device := range vmMo.Config.Hardware.Device {
		if disk, ok := device.(*types.VirtualDisk); ok {
			if currentIndex == diskIndex {
				targetDisk = disk
				break
			}
			currentIndex++
		}
	}

	if targetDisk == nil {
		return nil, errors.NewNotFound("disk not found in VM %s", req.VmId)
	}

	// Extract disk information
	diskLabel := targetDisk.DeviceInfo.GetDescription().Label
	virtualSizeBytes := targetDisk.CapacityInKB * 1024 // Convert KB to bytes

	// Determine disk path and backing
	var diskPath string
	var backingFile string
	format := "vmdk" // vSphere uses VMDK format

	if backing, ok := targetDisk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
		diskPath = backing.FileName
		backingFile = backing.Parent.GetVirtualDeviceFileBackingInfo().FileName
	} else if backing, ok := targetDisk.Backing.(*types.VirtualDiskSparseVer2BackingInfo); ok {
		diskPath = backing.FileName
	}

	// Get snapshots
	var snapshots []string
	if vmMo.Snapshot != nil {
		snapshots = p.extractSnapshotNames(vmMo.Snapshot.RootSnapshotList)
	}

	// For vSphere, actual size would require querying the datastore
	// For now, use virtual size as approximation
	actualSizeBytes := virtualSizeBytes

	response := &providerv1.GetDiskInfoResponse{
		DiskId:           diskLabel,
		Format:           format,
		VirtualSizeBytes: virtualSizeBytes,
		ActualSizeBytes:  actualSizeBytes,
		Path:             diskPath,
		IsBootable:       (diskIndex == 0), // First disk is bootable
		Snapshots:        snapshots,
		BackingFile:      backingFile,
		Metadata: map[string]string{
			"device_key":  fmt.Sprintf("%d", targetDisk.Key),
			"unit_number": fmt.Sprintf("%d", targetDisk.UnitNumber),
		},
	}

	p.logger.Info("Disk info retrieved", "disk_id", diskLabel, "path", diskPath, "size_bytes", virtualSizeBytes)
	return response, nil
}

// extractSnapshotNames performs a depth-first traversal of snapshotTree and returns a
// flat slice of all snapshot Name fields, including those of nested child snapshots.
func (p *Provider) extractSnapshotNames(snapshotTree []types.VirtualMachineSnapshotTree) []string {
	var names []string
	for _, snapshot := range snapshotTree {
		names = append(names, snapshot.Name)
		if len(snapshot.ChildSnapshotList) > 0 {
			names = append(names, p.extractSnapshotNames(snapshot.ChildSnapshotList)...)
		}
	}
	return names
}

// ExportDisk implements the ProviderServer interface. It exports a virtual disk from
// vSphere storage and writes the result to the PVC-backed storage URL specified in
// req.DestinationUrl (format: "pvc://<pvc-name>/<file-path>").
//
// The export pipeline is:
//  1. GetDiskInfo to resolve the source disk path. If the VM has snapshots, the base
//     (parent) backing disk is used instead of the delta disk.
//  2. VirtualDiskManager.CopyVirtualDisk clones the source VMDK to a temporary
//     sparseMonolithic VMDK on the same datastore. This normalises the format and
//     produces a single downloadable file regardless of the original VMDK subtype.
//  3. DatastoreFileManager.DownloadFile transfers the temporary VMDK to a local temp
//     directory. If the descriptor references extent files they are downloaded as well.
//  4. If req.Format is not "vmdk", diskutil/qemu-img converts the downloaded VMDK to
//     the requested format ("qcow2" or "raw"). Supported formats: "vmdk", "qcow2", "raw".
//  5. The final file is uploaded to the destination PVC storage via the storage client.
//  6. Temporary local files and the temporary datastore VMDK are cleaned up via deferred
//     calls regardless of success or failure.
//
// The operation runs synchronously; Task in the response is nil.
func (p *Provider) ExportDisk(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("vSphere client not configured", nil)
	}

	p.logger.Info("Exporting disk", "vm_id", req.VmId, "destination", req.DestinationUrl)

	// Get disk information first
	diskInfo, err := p.GetDiskInfo(ctx, &providerv1.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return nil, errors.NewInternal("failed to get disk info", err)
	}

	// Validate format - for vSphere export, we'll convert VMDK to target format
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = "vmdk" // Keep VMDK by default
	}
	if targetFormat != "vmdk" && targetFormat != "qcow2" && targetFormat != "raw" {
		return nil, errors.NewInvalidSpec("unsupported export format: %s", targetFormat)
	}

	exportID := fmt.Sprintf("export-vsphere-%s-%d", req.VmId, time.Now().Unix())

	// vSphere disk export strategy:
	// 1. Use OVF export to get the VMDK files
	// 2. Convert VMDK to target format if needed (using qemu-img)
	// 3. Upload to destination storage
	// 4. Track progress via task API

	p.logger.Info("Preparing disk export using OVF export", "vm_id", req.VmId)
	p.logger.Warn("vSphere disk export requires OVF export API - simplified implementation")
	p.logger.Info("Note: Full implementation would use govmomi OVF export and datastore file access")

	// Configure storage client
	// URL format: pvc://<pvc-name>/<file-path>
	// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
	// Extract PVC name from URL to construct the correct mount path
	pvcName, err := extractPVCNameFromURL(req.DestinationUrl)
	if err != nil {
		return nil, errors.NewInternal("failed to extract PVC name from URL", err)
	}

	// Mount path matches where the controller mounts PVCs: /mnt/migration-storage/<pvc-name>
	mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvcName)

	storageConfig := storage.StorageConfig{
		Type:      "pvc",
		MountPath: mountPath,
	}

	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create storage client", err)
	}
	defer storageClient.Close()

	// Create datastore file manager
	dsManager := NewDatastoreFileManager(p)

	// Create a temporary directory for VMDK export
	tempDir := fmt.Sprintf("/tmp/%s", exportID)
	err = os.MkdirAll(tempDir, 0755)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp directory", err)
	}
	defer func() {
		// Clean up temp directory and temporary cloned disk
		_ = os.RemoveAll(tempDir)
	}()

	// Strategy: Use VirtualDiskManager to clone disk to sparseMonolithic format
	// This handles all VMDK types (sesparse, flat, thick, thin) and produces a single downloadable file
	// IMPORTANT: For VMs with snapshots, we must use the BASE disk, not the snapshot delta disk
	// VirtualDiskManager cannot clone snapshot delta disks directly

	sourceDiskPath := diskInfo.Path

	// If we have a backing file (parent disk), use it instead of the current disk
	// This happens when the VM has snapshots - Path is the delta disk, BackingFile is the base
	if diskInfo.BackingFile != "" {
		p.logger.Info("VM has snapshots, using base disk for export",
			"delta_disk", diskInfo.Path,
			"base_disk", diskInfo.BackingFile)
		sourceDiskPath = diskInfo.BackingFile
	}

	p.logger.Info("Cloning disk to sparseMonolithic format for export", "source_disk", sourceDiskPath)

	// Parse source datastore path
	srcDsName, srcFilePath, err := parseDatastorePath(sourceDiskPath)
	if err != nil {
		return nil, errors.NewInternal("failed to parse source datastore path", err)
	}

	// Create temporary destination path for sparseMonolithic clone
	// Use a unique name to avoid conflicts
	tempDiskName := fmt.Sprintf("virtrigaud-export-%s-%d.vmdk", req.VmId, time.Now().Unix())
	destPath := path.Join(path.Dir(srcFilePath), tempDiskName)
	destDatastorePath := fmt.Sprintf("[%s] %s", srcDsName, destPath)

	p.logger.Info("Creating sparseMonolithic clone", "source", sourceDiskPath, "destination", destDatastorePath)

	// Get VirtualDiskManager
	err = p.cloneDiskToStreamOptimized(ctx, sourceDiskPath, destDatastorePath)
	if err != nil {
		return nil, errors.NewInternal("failed to clone disk to streamOptimized format", err)
	}

	// Ensure cleanup of temporary disk on datastore
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := dsManager.DeleteFile(cleanupCtx, destDatastorePath); err != nil {
			p.logger.Warn("Failed to cleanup temporary disk on datastore", "path", destDatastorePath, "error", err)
		} else {
			p.logger.Info("Cleaned up temporary disk from datastore", "path", destDatastorePath)
		}
	}()

	// Now download the streamOptimized VMDK (single file, no extent files)
	tempFile := fmt.Sprintf("%s/disk.vmdk", tempDir)
	file, err := os.Create(tempFile)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp file", err)
	}

	// Download with progress tracking
	downloadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			if int(progress)%10 == 0 { // Log every 10%
				p.logger.Info("Download progress", "percent", fmt.Sprintf("%.1f%%", progress))
			}
		}
	}

	p.logger.Info("Downloading streamOptimized VMDK", "source", destDatastorePath)
	err = dsManager.DownloadFile(ctx, destDatastorePath, file, downloadProgress)
	if err != nil {
		file.Close()
		return nil, errors.NewInternal("failed to download streamOptimized VMDK", err)
	}

	// Close file to flush writes
	file.Close()
	p.logger.Info("Download complete", "file", tempFile)

	// Parse VMDK descriptor to find extent files (for multi-file VMDKs like sesparse)
	// Even though we requested sparseMonolithic, vSphere may still create multi-file VMDKs
	descriptor, err := parseVMDKDescriptor(tempFile)
	if err != nil {
		p.logger.Warn("Failed to parse VMDK descriptor, assuming single-file VMDK", "error", err)
		descriptor = &VMDKDescriptor{
			DescriptorPath: tempFile,
			ExtentFiles:    []string{},
		}
	}

	// Download extent files if any
	if len(descriptor.ExtentFiles) > 0 {
		p.logger.Info("VMDK has extent files, downloading them", "count", len(descriptor.ExtentFiles), "files", descriptor.ExtentFiles)
		basePath := extractDatastoreBasePath(destDatastorePath)

		for _, extentFile := range descriptor.ExtentFiles {
			// Construct full datastore path for extent file
			extentPath := constructDatastorePath(basePath, extentFile)
			localPath := fmt.Sprintf("%s/%s", tempDir, extentFile)

			p.logger.Info("Downloading extent file", "datastore_path", extentPath, "local_path", localPath)

			extentFileHandle, err := os.Create(localPath)
			if err != nil {
				return nil, errors.NewInternal("failed to create extent file", err)
			}

			err = dsManager.DownloadFile(ctx, extentPath, extentFileHandle, nil)
			extentFileHandle.Close()
			if err != nil {
				p.logger.Warn("Failed to download extent file", "extent", extentFile, "error", err)
				// Continue anyway - qemu-img might work without all extents
			} else {
				p.logger.Info("Extent file downloaded successfully", "file", extentFile)
			}
		}
		p.logger.Info("All available extent files downloaded")
	}

	// Convert format if needed
	var uploadPath string
	if targetFormat != "vmdk" {
		p.logger.Info("Converting VMDK to target format", "target_format", targetFormat)
		convertedPath := fmt.Sprintf("%s/converted.%s", tempDir, targetFormat)

		// Use diskutil for conversion
		qemuImg := diskutil.NewQemuImg()
		err = qemuImg.Convert(ctx, diskutil.ConvertOptions{
			SourcePath:        tempFile,
			DestinationPath:   convertedPath,
			SourceFormat:      diskutil.FormatVMDK,
			DestinationFormat: diskutil.SupportedFormat(targetFormat),
			Compression:       false, // No compression for migration (faster)
		})
		if err != nil {
			return nil, errors.NewInternal("failed to convert VMDK format", err)
		}

		uploadPath = convertedPath
		// No need for explicit cleanup - tempDir cleanup will handle it
	} else {
		uploadPath = tempFile
	}

	// Upload to destination storage
	p.logger.Info("Uploading disk to storage", "destination", req.DestinationUrl)

	// Re-open file for reading
	uploadFile, err := os.Open(uploadPath)
	if err != nil {
		return nil, errors.NewInternal("failed to open file for upload", err)
	}
	defer uploadFile.Close()

	// Get file size
	stat, err := uploadFile.Stat()
	if err != nil {
		return nil, errors.NewInternal("failed to stat file", err)
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
		return nil, errors.NewInternal("failed to upload disk", err)
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

// ImportDisk implements the ProviderServer interface. It downloads a virtual disk from
// PVC-backed storage (req.SourceUrl, format: "pvc://<pvc-name>/<file-path>") and
// uploads it to a vSphere datastore so that it can be attached to a VM via Create.
//
// The import pipeline is:
//  1. Determine the target datastore: req.StorageHint if set, otherwise
//     p.config.DefaultDatastore.
//  2. Download the disk file from PVC storage to a local temp file, optionally
//     verifying the checksum (req.VerifyChecksum / req.ExpectedChecksum).
//  3. If req.Format is not "vmdk", diskutil/qemu-img converts the downloaded file to
//     VMDK format (thin, stream-optimised). Supported source formats: "qcow2", "raw".
//  4. DatastoreFileManager.UploadFile transfers the VMDK to the datastore at the path
//     "[<datastore>] <diskID>/<diskID>.vmdk".
//  5. Temporary local files are cleaned up via deferred os.Remove calls.
//
// The ImportDiskResponse.Path contains the fully-qualified datastore VMDK path to be
// passed as VMImage.Path in a subsequent Create request. The operation runs
// synchronously; Task in the response is nil.
func (p *Provider) ImportDisk(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	if p.client == nil {
		return nil, errors.NewUnavailable("vSphere client not configured", nil)
	}

	p.logger.Info("Importing disk", "source", req.SourceUrl, "storage", req.StorageHint)

	// Determine target datastore
	datastore := p.config.DefaultDatastore
	if req.StorageHint != "" {
		datastore = req.StorageHint
	}

	// Determine format
	targetFormat := req.Format
	if targetFormat == "" {
		targetFormat = "vmdk" // Default for vSphere
	}
	if targetFormat != "vmdk" && targetFormat != "qcow2" && targetFormat != "raw" {
		return nil, errors.NewInvalidSpec("unsupported import format: %s", targetFormat)
	}

	// Generate disk name
	diskID := req.TargetName
	if diskID == "" {
		diskID = fmt.Sprintf("imported-disk-%d", time.Now().Unix())
	}

	p.logger.Info("Preparing disk import", "disk_id", diskID, "datastore", datastore, "format", targetFormat)

	// vSphere disk import strategy:
	// 1. Download disk from SourceURL to temporary location
	// 2. Convert to VMDK if needed (using qemu-img or vmware-vdiskmanager)
	// 3. Upload VMDK to datastore using datastore file manager
	// 4. Create disk descriptor
	// 5. Return disk reference

	// Configure storage client
	// URL format: pvc://<pvc-name>/<file-path>
	// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
	// Extract PVC name from URL to construct the correct mount path
	pvcName, err := extractPVCNameFromURL(req.SourceUrl)
	if err != nil {
		return nil, errors.NewInternal("failed to extract PVC name from URL", err)
	}

	// Mount path matches where the controller mounts PVCs: /mnt/migration-storage/<pvc-name>
	mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvcName)

	storageConfig := storage.StorageConfig{
		Type:      "pvc",
		MountPath: mountPath,
	}

	storageClient, err := storage.NewStorage(storageConfig)
	if err != nil {
		return nil, errors.NewInternal("failed to create storage client", err)
	}
	defer storageClient.Close()

	// Create datastore file manager
	dsManager := NewDatastoreFileManager(p)

	// Create temporary file for download
	tempFile := fmt.Sprintf("/tmp/%s-import", diskID)
	defer func() {
		_ = os.Remove(tempFile)
	}()

	// Download from storage
	p.logger.Info("Downloading disk from storage", "source", req.SourceUrl)

	file, err := os.Create(tempFile)
	if err != nil {
		return nil, errors.NewInternal("failed to create temp file", err)
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
		return nil, errors.NewInternal("failed to download disk", err)
	}

	// Close file to flush writes
	file.Close()

	p.logger.Info("Download completed", "bytes", downloadResp.BytesTransferred, "checksum", downloadResp.Checksum)

	// Convert to VMDK if needed
	var vmdkPath string
	if targetFormat != "vmdk" {
		p.logger.Info("Converting to VMDK format", "source_format", targetFormat)
		vmdkPath = fmt.Sprintf("/tmp/%s.vmdk", diskID)

		// Use diskutil for conversion
		qemuImg := diskutil.NewQemuImg()
		err = qemuImg.Convert(ctx, diskutil.ConvertOptions{
			SourcePath:        tempFile,
			DestinationPath:   vmdkPath,
			DestinationFormat: diskutil.FormatVMDK,
			Compression:       false, // No compression for migration (faster)
		})
		if err != nil {
			return nil, errors.NewInternal("failed to convert to VMDK format", err)
		}

		defer os.Remove(vmdkPath)
	} else {
		vmdkPath = tempFile
	}

	// Upload to datastore
	p.logger.Info("Uploading VMDK to datastore", "datastore", datastore, "disk_id", diskID)

	// Re-open file for reading
	uploadFile, err := os.Open(vmdkPath)
	if err != nil {
		return nil, errors.NewInternal("failed to open file for datastore upload", err)
	}
	defer uploadFile.Close()

	// Get file size
	stat, err := uploadFile.Stat()
	if err != nil {
		return nil, errors.NewInternal("failed to stat file", err)
	}

	// Generate datastore path
	diskPath := fmt.Sprintf("[%s] %s/%s.vmdk", datastore, diskID, diskID)

	// Upload to datastore with progress tracking
	uploadProgress := func(transferred, total int64) {
		if total > 0 {
			progress := float64(transferred) / float64(total) * 100
			p.logger.Debug("Datastore upload progress", "percent", progress, "transferred", transferred, "total", total)
		}
	}

	err = dsManager.UploadFile(ctx, uploadFile, diskPath, stat.Size(), uploadProgress)
	if err != nil {
		return nil, errors.NewInternal("failed to upload to datastore", err)
	}

	p.logger.Info("Disk import completed", "disk_id", diskID, "path", diskPath)

	response := &providerv1.ImportDiskResponse{
		DiskId:          diskID,
		Path:            diskPath,
		Task:            nil, // Synchronous operation
		ActualSizeBytes: downloadResp.ContentLength,
		Checksum:        downloadResp.Checksum,
	}

	return response, nil
}

// ListVMs implements the ProviderServer interface. It enumerates every virtual machine
// in the default datacenter using the govmomi Finder and returns summary information
// for each one.
//
// For each VM the following information is collected via the property collector:
//   - Name, CPU count, memory size (MB), power state.
//   - IP addresses (filtered by isValidIPAddress; duplicates removed).
//   - Disk devices: ID (device key), datastore path, size in GiB, format ("vmdk").
//   - Network adapters: MAC address and portgroup/network name.
//   - ProviderRaw map: vm_id (same as Name), power_state, guest_os.
//
// VMs for which property retrieval fails are skipped with a warning log rather than
// aborting the entire list operation.
func (p *Provider) ListVMs(ctx context.Context, req *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("vSphere client not configured")
	}

	p.logger.Info("Listing all virtual machines")

	// Set datacenter context for finder
	datacenter, err := p.finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default datacenter: %w", err)
	}
	p.finder.SetDatacenter(datacenter)

	// Find all VMs in the datacenter
	vms, err := p.finder.VirtualMachineList(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("failed to list virtual machines: %w", err)
	}

	p.logger.Info("Found VMs", "count", len(vms))

	// Collect VM information
	var vmInfos []*providerv1.VMInfo
	pc := property.DefaultCollector(p.client.Client)

	for _, vm := range vms {
		// Get VM properties
		var vmMo mo.VirtualMachine
		err := pc.RetrieveOne(ctx, vm.Reference(), []string{
			"summary.config.name",
			"summary.config.numCpu",
			"summary.config.memorySizeMB",
			"summary.runtime.powerState",
			"guest.ipAddress",
			"guest.net",
			"config.hardware.device",
		}, &vmMo)

		if err != nil {
			p.logger.Warn("Failed to retrieve VM properties, skipping", "vm", vm.Name(), "error", err)
			continue
		}

		// Extract power state
		powerState := p.mapVSpherePowerState(string(vmMo.Summary.Runtime.PowerState))

		// Extract IP addresses
		var ips []string
		if vmMo.Guest != nil {
			if vmMo.Guest.IpAddress != "" {
				ips = append(ips, vmMo.Guest.IpAddress)
			}
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

		// Extract disk information
		var disks []*providerv1.DiskInfo
		if vmMo.Config != nil {
			for _, device := range vmMo.Config.Hardware.Device {
				if disk, ok := device.(*types.VirtualDisk); ok {
					// Get disk size in GiB
					sizeGiB := int32(disk.CapacityInBytes / (1024 * 1024 * 1024))
					if sizeGiB == 0 && disk.CapacityInBytes > 0 {
						sizeGiB = 1 // Round up to at least 1 GiB
					}

					diskID := fmt.Sprintf("%d", disk.Key)
					diskPath := ""
					if disk.Backing != nil {
						if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
							diskPath = backing.FileName
						}
					}

					diskInfo := &providerv1.DiskInfo{
						Id:      diskID,
						Path:    diskPath,
						SizeGib: sizeGiB,
						Format:  "vmdk",
					}
					disks = append(disks, diskInfo)
				}
			}
		}

		// Extract network information
		var networks []*providerv1.NetworkInfo
		if vmMo.Config != nil {
			for _, device := range vmMo.Config.Hardware.Device {
				if nic, ok := device.(*types.VirtualEthernetCard); ok {
					networkInfo := &providerv1.NetworkInfo{
						Mac: nic.MacAddress,
					}
					if nic.Backing != nil {
						if networkBacking, ok := nic.Backing.(*types.VirtualEthernetCardNetworkBackingInfo); ok {
							networkInfo.Name = networkBacking.DeviceName
						}
					}
					networks = append(networks, networkInfo)
				}
			}
		}

		// Build provider raw metadata
		providerRaw := make(map[string]string)
		providerRaw["vm_id"] = vmMo.Summary.Config.Name
		providerRaw["power_state"] = powerState
		if vmMo.Summary.Config.GuestFullName != "" {
			providerRaw["guest_os"] = vmMo.Summary.Config.GuestFullName
		}

		vmInfo := &providerv1.VMInfo{
			Id:          vm.Reference().Value, // Use ManagedObjectReference value as ID
			Name:        vmMo.Summary.Config.Name,
			PowerState:  powerState,
			Ips:         ips,
			Cpu:         vmMo.Summary.Config.NumCpu,
			MemoryMib:   int64(vmMo.Summary.Config.MemorySizeMB),
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
