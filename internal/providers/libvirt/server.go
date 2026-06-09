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
	"log"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
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
	// If no provider is configured yet, return a basic healthy response
	// This allows the health checks to pass while the provider is being initialized
	if s.provider == nil {
		return &providerv1.ValidateResponse{
			Ok:      true,
			Message: "Provider server is running (provider not yet initialized)",
		}, nil
	}

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
	if s.provider == nil {
		return nil, fmt.Errorf("provider not initialized")
	}

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
	case providerv1.PowerOp_POWER_OP_SHUTDOWN_GRACEFUL:
		powerOp = contracts.PowerOpShutdownGraceful
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
	if s.provider == nil {
		return nil, fmt.Errorf("provider not initialized")
	}

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
	log.Printf("INFO Creating snapshot for VM: %s", req.VmId)

	// Get the provider instance and cast to libvirt Provider
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}

	// Generate snapshot name if not provided
	snapshotName := req.NameHint
	if snapshotName == "" {
		snapshotName = fmt.Sprintf("snapshot-%d", generateTimestamp())
	}

	// Clean snapshot name (virsh has strict naming requirements)
	snapshotName = sanitizeSnapshotName(snapshotName)

	// Prepare snapshot description
	description := req.Description
	if description == "" {
		description = fmt.Sprintf("Snapshot created by VirtRigaud at %s", time.Now().Format(time.RFC3339))
	}

	// Check if domain exists and get its state
	domainState, err := libvirtProvider.virshProvider.getDomainState(ctx, req.VmId)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain state: %w", err)
	}

	log.Printf("INFO Domain %s is in state: %s", req.VmId, domainState)

	// Build the virsh snapshot-create-as arguments. A memory-inclusive (full
	// system) snapshot omits --disk-only and is only possible for a RUNNING
	// domain (there is no RAM state to capture otherwise); any other case is a
	// disk-only snapshot.
	args, memorySnapshot := buildSnapshotCreateArgs(req.VmId, snapshotName, description, req.IncludeMemory, domainState == "running")
	switch {
	case memorySnapshot:
		log.Printf("INFO Creating memory snapshot (full system checkpoint including RAM) for domain %s", req.VmId)
	case req.IncludeMemory:
		// Honest downgrade: a stopped VM has no RAM state to capture. The
		// snapshot still succeeds as disk-only; the caller is told why rather
		// than silently advertising a memory snapshot that did not happen.
		log.Printf("WARN Memory snapshot requested for domain %s but it is not running (state=%s); "+
			"creating a disk-only snapshot — memory state cannot be captured for a stopped VM", req.VmId, domainState)
	default:
		log.Printf("INFO Creating disk-only snapshot for domain %s", req.VmId)
	}

	// Execute snapshot creation
	result, err := libvirtProvider.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	log.Printf("INFO Snapshot created successfully: %s\nOutput: %s", snapshotName, result.Stdout)

	// Return snapshot ID (synchronous operation for libvirt)
	return &providerv1.SnapshotCreateResponse{
		SnapshotId: snapshotName,
		// No task reference - libvirt snapshots are synchronous
	}, nil
}

// buildSnapshotCreateArgs builds the `virsh snapshot-create-as` arguments and
// reports whether the result is a memory-inclusive (full system) snapshot.
//
// A memory snapshot — a full system checkpoint that captures RAM together with
// the disk state — is created by OMITTING --disk-only, and is only meaningful
// for a RUNNING domain (a stopped domain has no RAM state). In every other case
// (includeMemory false, or the domain not running) a --disk-only snapshot is
// taken. The boolean lets the caller log/report honestly which kind was made.
func buildSnapshotCreateArgs(vmID, name, description string, includeMemory, running bool) (args []string, memorySnapshot bool) {
	args = []string{
		"snapshot-create-as",
		vmID,
		name,
		"--description", description,
		"--atomic", // fail cleanly rather than leaving a half-created snapshot
	}
	if includeMemory && running {
		// No --disk-only flag → full snapshot including memory.
		return args, true
	}
	return append(args, "--disk-only"), false
}

// SnapshotDelete deletes a VM snapshot
func (s *Server) SnapshotDelete(ctx context.Context, req *providerv1.SnapshotDeleteRequest) (*providerv1.TaskResponse, error) {
	log.Printf("INFO Deleting snapshot %s from VM: %s", req.SnapshotId, req.VmId)

	// Get the provider instance and cast to libvirt Provider
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}

	// Check if snapshot exists
	exists, err := libvirtProvider.virshProvider.snapshotExists(ctx, req.VmId, req.SnapshotId)
	if err != nil {
		return nil, fmt.Errorf("failed to check snapshot existence: %w", err)
	}

	if !exists {
		log.Printf("WARN Snapshot %s does not exist, considering deletion successful", req.SnapshotId)
		return &providerv1.TaskResponse{}, nil
	}

	// Delete the snapshot
	// Format: virsh snapshot-delete DOMAIN SNAPSHOT --metadata
	// Using --metadata keeps the data but removes snapshot metadata (safer for external snapshots)
	// For internal snapshots, this will delete both metadata and disk changes
	args := []string{
		"snapshot-delete",
		req.VmId,
		req.SnapshotId,
	}

	result, err := libvirtProvider.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to delete snapshot: %w", err)
	}

	log.Printf("INFO Snapshot deleted successfully: %s\nOutput: %s", req.SnapshotId, result.Stdout)

	// Return empty response (synchronous operation)
	return &providerv1.TaskResponse{}, nil
}

// SnapshotRevert reverts a VM to a snapshot
func (s *Server) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	log.Printf("INFO Reverting VM %s to snapshot: %s", req.VmId, req.SnapshotId)

	// Get the provider instance and cast to libvirt Provider
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}

	// Check if snapshot exists
	exists, err := libvirtProvider.virshProvider.snapshotExists(ctx, req.VmId, req.SnapshotId)
	if err != nil {
		return nil, fmt.Errorf("failed to check snapshot existence: %w", err)
	}

	if !exists {
		return nil, fmt.Errorf("snapshot %s does not exist", req.SnapshotId)
	}

	// Get current domain state
	domainState, err := libvirtProvider.virshProvider.getDomainState(ctx, req.VmId)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain state: %w", err)
	}

	log.Printf("INFO Domain %s current state: %s", req.VmId, domainState)

	// Revert to snapshot
	// Format: virsh snapshot-revert DOMAIN SNAPSHOT --running|--paused
	args := []string{
		"snapshot-revert",
		req.VmId,
		req.SnapshotId,
		"--force", // Force revert even if domain is running
	}

	// If domain was running, keep it running after revert
	if domainState == "running" {
		args = append(args, "--running")
	}

	result, err := libvirtProvider.virshProvider.runVirshCommand(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to revert to snapshot: %w", err)
	}

	log.Printf("INFO Successfully reverted to snapshot: %s\nOutput: %s", req.SnapshotId, result.Stdout)

	// Return empty response (synchronous operation)
	return &providerv1.TaskResponse{}, nil
}

// Clone creates a VM clone. It delegates to the libvirt Provider
// implementation (clone.go), translating between the gRPC and provider-contract
// types. A full clone copies the source disk into an independent volume; a
// linked clone (req.Linked) creates a qcow2 overlay backed by the source disk
// and is therefore lifecycle-bound to it (issue #153).
func (s *Server) Clone(ctx context.Context, req *providerv1.CloneRequest) (*providerv1.CloneResponse, error) {
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}

	resp, err := libvirtProvider.Clone(ctx, contracts.CloneRequest{
		SourceVmID:    req.SourceVmId,
		TargetName:    req.TargetName,
		Linked:        req.Linked,
		ClassJSON:     req.ClassJson,
		PlacementJSON: req.PlacementJson,
		CustomizeJSON: req.CustomizeJson,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone VM: %w", err)
	}

	result := &providerv1.CloneResponse{
		TargetVmId: resp.TargetVmID,
	}
	if resp.TaskRef != "" {
		result.Task = &providerv1.TaskRef{Id: resp.TaskRef}
	}

	return result, nil
}

// ImagePrepare prepares/imports a VM image into a libvirt storage pool, making it
// available as a named template (<target_name>.qcow2) for subsequent VM creation
// (issue #154).
//
// The request carries a JSON-encoded VMImage spec (req.ImageJson) describing the
// source, a target template name, and an optional storage hint. The source may be
// a path already present on the libvirt host or a URL to download on the host. The
// image is converted into the resolved pool as a standalone qcow2; an existing
// target is treated as an idempotent no-op (see Provider.imagePrepare).
//
// libvirt/qemu-img are synchronous, so this returns an ImagePrepareResponse with
// an empty Task (no TaskRef); the controller treats an empty TaskRef as
// "completed synchronously". The response also carries the prepared image's
// location — prepared_image_id is the target name and prepared_image_path is the
// absolute pool path (<poolPath>/<target>.qcow2) — so the manager can create VMs
// from the prepared template instead of re-resolving the source (issue #154,
// PR-6 / #214).
func (s *Server) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
	log.Printf("INFO ImagePrepare: target=%q storageHint=%q", req.TargetName, req.StorageHint)

	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}

	preparedID, preparedPath, err := libvirtProvider.imagePrepare(ctx, req.ImageJson, req.TargetName, req.StorageHint)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare image: %w", err)
	}

	// Synchronous: no task reference. An empty Task signals "completed". The
	// id/path tell the manager where the prepared template landed.
	return &providerv1.ImagePrepareResponse{
		PreparedImageId:   preparedID,
		PreparedImagePath: preparedPath,
	}, nil
}

// GetCapabilities returns the capabilities of the Libvirt provider
func (s *Server) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	return &providerv1.GetCapabilitiesResponse{
		SupportsReconfigureOnline:   true, // Online CPU/mem reconfigure via `setvcpus/setmem --live` for VMs created with CPU/MemoryHotAddEnabled (headroom provisioned at create); grows up to the ~4× ceiling, beyond which a power-cycle is required (#203)
		SupportsDiskExpansionOnline: true, // Online grow via `virsh blockresize` + best-effort in-guest FS grow (resize2fs/xfs_growfs) when the guest agent is present; grow-only (#201)
		SupportsSnapshots:           true, // Libvirt supports snapshots (storage-dependent)
		SupportsMemorySnapshots:     true, // Full system checkpoints incl. RAM via `snapshot-create-as` without --disk-only; requires the VM running (#202)
		SupportsLinkedClones:        true, // Clone RPC implemented: qcow2 overlay (linked) + vol-clone (full) (issue #153)
		SupportsImageImport:         true, // ImagePrepare RPC implemented: import/convert image into a storage pool (issue #154)
		SupportedDiskTypes:          []string{"qcow2", "raw", "vmdk"},
		SupportedNetworkTypes:       []string{"virtio", "e1000", "rtl8139"},
		SupportsDiskExport:          true, // ExportDisk wired to virsh impl (issue #177)
		SupportsDiskImport:          true, // ImportDisk wired (pvc:///file:// sources)
		SupportedExportFormats:      []string{"qcow2", "raw"},
		SupportedImportFormats:      []string{"qcow2", "raw", "vmdk"},
		SupportsExportCompression:   true, // ExportDisk honors req.Compress via qemu-img -c for qcow2 (#199); default (Compress=false) is uncompressed for speed
	}, nil
}

// ExportDisk exports a VM disk for migration. It delegates to the libvirt
// Provider implementation (provider_virsh.go), translating between the gRPC and
// provider-contract types. Previously this RPC was unreachable over gRPC and
// returned Unimplemented despite a working implementation (issue #177).
func (s *Server) ExportDisk(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("provider not initialized")
	}

	resp, err := s.provider.ExportDisk(ctx, contracts.ExportDiskRequest{
		VmId:           req.VmId,
		DiskId:         req.DiskId,
		SnapshotId:     req.SnapshotId,
		DestinationURL: req.DestinationUrl,
		Format:         req.Format,
		Compress:       req.Compress,
		Credentials:    req.Credentials,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to export disk: %w", err)
	}

	result := &providerv1.ExportDiskResponse{
		ExportId:           resp.ExportId,
		EstimatedSizeBytes: resp.EstimatedSizeBytes,
		Checksum:           resp.Checksum,
	}
	if resp.TaskRef != "" {
		result.Task = &providerv1.TaskRef{Id: resp.TaskRef}
	}

	return result, nil
}

// GetDiskInfo returns details about a VM disk for migration planning. It
// delegates to the libvirt Provider implementation (provider_virsh.go),
// translating between the gRPC and provider-contract types. Previously this RPC
// was unreachable over gRPC and returned Unimplemented (issue #177).
func (s *Server) GetDiskInfo(ctx context.Context, req *providerv1.GetDiskInfoRequest) (*providerv1.GetDiskInfoResponse, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("provider not initialized")
	}

	resp, err := s.provider.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{
		VmId:       req.VmId,
		DiskId:     req.DiskId,
		SnapshotId: req.SnapshotId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get disk info: %w", err)
	}

	return &providerv1.GetDiskInfoResponse{
		DiskId:           resp.DiskId,
		Format:           resp.Format,
		VirtualSizeBytes: resp.VirtualSizeBytes,
		ActualSizeBytes:  resp.ActualSizeBytes,
		Path:             resp.Path,
		IsBootable:       resp.IsBootable,
		Snapshots:        resp.Snapshots,
		BackingFile:      resp.BackingFile,
		Metadata:         resp.Metadata,
	}, nil
}

// ImportDisk imports a disk from an external source (for VM migration)
func (s *Server) ImportDisk(ctx context.Context, req *providerv1.ImportDiskRequest) (*providerv1.ImportDiskResponse, error) {
	log.Printf("INFO Starting disk import from %s", req.SourceUrl)

	// Get the provider instance and cast to libvirt Provider
	libvirtProvider, ok := s.provider.(*Provider)
	if !ok || libvirtProvider == nil || libvirtProvider.virshProvider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}

	// Parse source URL (expecting pvc:// or file:// URL)
	sourceURL := req.SourceUrl
	var sourcePath string

	if strings.HasPrefix(sourceURL, "pvc://") {
		// PVC URL format: pvc://<pvc-name>/<path>
		// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
		pvcURL := strings.TrimPrefix(sourceURL, "pvc://")
		sourcePath = fmt.Sprintf("/mnt/migration-storage/%s", pvcURL)
		log.Printf("INFO Converting PVC URL to file path: %s -> %s", sourceURL, sourcePath)
	} else if strings.HasPrefix(sourceURL, "file://") {
		// Direct file path
		sourcePath = strings.TrimPrefix(sourceURL, "file://")
		log.Printf("INFO Using direct file path: %s", sourcePath)
	} else {
		return nil, fmt.Errorf("unsupported source URL scheme (expected pvc:// or file://): %s", sourceURL)
	}

	log.Printf("INFO Importing disk from path: %s", sourcePath)

	// Validate source file exists locally
	if _, err := os.Stat(sourcePath); err != nil {
		return nil, fmt.Errorf("source disk file not found locally: %s (error: %w)", sourcePath, err)
	}

	// Generate target volume name from TargetName or source filename
	volumeName := req.TargetName
	if volumeName == "" {
		// Extract filename without extension
		parts := strings.Split(sourcePath, "/")
		fileName := parts[len(parts)-1]
		volumeName = strings.TrimSuffix(fileName, ".qcow2")
		volumeName = strings.TrimSuffix(volumeName, ".vmdk")
		volumeName = strings.TrimSuffix(volumeName, ".raw")
		volumeName = fmt.Sprintf("%s-imported", volumeName)
	}

	log.Printf("INFO Target volume name: %s", volumeName)

	// Copy disk file to remote libvirt host (if using SSH connection)
	var finalSourcePath string
	if strings.Contains(libvirtProvider.virshProvider.uri, "ssh://") {
		log.Printf("INFO Copying disk file to remote libvirt host...")
		remotePath, err := s.copyDiskToRemote(ctx, libvirtProvider.virshProvider, sourcePath, volumeName)
		if err != nil {
			return nil, fmt.Errorf("failed to copy disk to remote host: %w", err)
		}
		finalSourcePath = remotePath
		log.Printf("INFO Disk copied to remote host: %s", finalSourcePath)
	} else {
		// Local libvirt connection - use source path directly
		finalSourcePath = sourcePath
		log.Printf("INFO Using local libvirt connection with path: %s", finalSourcePath)
	}

	// Get source disk info using qemu-img
	infoResult, err := libvirtProvider.virshProvider.runVirshCommand(ctx, "!", "qemu-img", "info", "--output=json", sourcePath)
	if err != nil {
		log.Printf("WARN Failed to get source disk info: %v", err)
	} else {
		log.Printf("DEBUG Source disk info: %s", infoResult.Stdout)
	}

	// Determine target pool (use StorageHint or default to "default")
	poolName := "default"
	if req.StorageHint != "" {
		poolName = req.StorageHint
	}

	// Create storage provider
	storageProvider := NewStorageProvider(libvirtProvider.virshProvider)

	// Ensure target pool exists and is active
	if err := storageProvider.EnsureDefaultStoragePool(ctx); err != nil {
		return nil, fmt.Errorf("failed to ensure storage pool: %w", err)
	}

	// Import the disk using CreateVolumeFromImageFile
	// This will copy, convert to qcow2, and set proper permissions
	log.Printf("INFO Importing disk to pool %s as volume %s", poolName, volumeName)
	volume, err := storageProvider.CreateVolumeFromImageFile(ctx, finalSourcePath, volumeName, poolName, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to import disk: %w", err)
	}

	log.Printf("INFO Disk successfully imported to: %s", volume.Path)

	// Get actual size of imported disk
	var actualSizeBytes int64
	statResult, err := libvirtProvider.virshProvider.runVirshCommand(ctx, "!", "stat", "-c", "%s", volume.Path)
	if err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(statResult.Stdout), "%d", &actualSizeBytes)
	}

	// Calculate checksum if requested
	checksum := ""
	if req.VerifyChecksum {
		log.Printf("INFO Calculating SHA256 checksum of imported disk...")
		checksumResult, err := libvirtProvider.virshProvider.runVirshCommand(ctx, "!", "sha256sum", volume.Path)
		if err != nil {
			log.Printf("WARN Failed to calculate checksum: %v", err)
		} else {
			// sha256sum output format: "<checksum> <filename>"
			parts := strings.Fields(checksumResult.Stdout)
			if len(parts) > 0 {
				checksum = parts[0]
				log.Printf("INFO Calculated checksum: %s", checksum)

				// Verify against expected checksum if provided
				if req.ExpectedChecksum != "" && req.ExpectedChecksum != checksum {
					return nil, fmt.Errorf("checksum mismatch: expected %s, got %s", req.ExpectedChecksum, checksum)
				}
			}
		}
	}

	// Generate disk ID (volume name in libvirt)
	diskID := volumeName

	return &providerv1.ImportDiskResponse{
		DiskId:          diskID,
		Path:            volume.Path,
		ActualSizeBytes: actualSizeBytes,
		Checksum:        checksum,
		// No task reference - import is synchronous
	}, nil
}

// ListVMs returns all VMs managed by this provider
func (s *Server) ListVMs(ctx context.Context, req *providerv1.ListVMsRequest) (*providerv1.ListVMsResponse, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("provider not initialized")
	}

	vmInfos, err := s.provider.ListVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list VMs: %w", err)
	}

	// Convert contracts.VMInfo to providerv1.VMInfo
	var protoVMInfos []*providerv1.VMInfo
	for _, vmInfo := range vmInfos {
		// Convert disks
		var protoDisks []*providerv1.DiskInfo
		for _, disk := range vmInfo.Disks {
			protoDisks = append(protoDisks, &providerv1.DiskInfo{
				Id:      disk.ID,
				Path:    disk.Path,
				SizeGib: disk.SizeGiB,
				Format:  disk.Format,
			})
		}

		// Convert networks
		var protoNetworks []*providerv1.NetworkInfo
		for _, net := range vmInfo.Networks {
			protoNetworks = append(protoNetworks, &providerv1.NetworkInfo{
				Name:      net.Name,
				Mac:       net.MAC,
				IpAddress: net.IPAddress,
			})
		}

		protoVMInfos = append(protoVMInfos, &providerv1.VMInfo{
			Id:          vmInfo.ID,
			Name:        vmInfo.Name,
			PowerState:  vmInfo.PowerState,
			Ips:         vmInfo.IPs,
			Cpu:         vmInfo.CPU,
			MemoryMib:   vmInfo.MemoryMiB,
			Disks:       protoDisks,
			Networks:    protoNetworks,
			ProviderRaw: vmInfo.ProviderRaw,
		})
	}

	return &providerv1.ListVMsResponse{
		Vms: protoVMInfos,
	}, nil
}

// copyDiskToRemote copies a disk file from local pod storage to the remote libvirt host
func (s *Server) copyDiskToRemote(ctx context.Context, virshProvider *VirshProvider, localPath, volumeName string) (string, error) {
	// IMPORTANT: Copy directly to libvirt pool directory for efficient in-place usage
	// This allows CreateVolumeFromImageFile to detect and use the disk without copying
	remoteDir := "/var/lib/libvirt/images"
	remotePath := fmt.Sprintf("%s/%s.qcow2", remoteDir, volumeName)

	// Extract SSH target (user@host) from URI
	parsedURI, err := url.Parse(virshProvider.uri)
	if err != nil {
		return "", fmt.Errorf("failed to parse libvirt URI: %w", err)
	}

	user := parsedURI.User.Username()
	host := parsedURI.Host
	sshTarget := fmt.Sprintf("%s@%s", user, host)

	log.Printf("INFO Ensuring libvirt pool directory exists on %s", sshTarget)

	// Ensure pool directory exists (usually already exists, but safe to check)
	_, err = virshProvider.runVirshCommand(ctx, "!", "sudo", "mkdir", "-p", remoteDir)
	if err != nil {
		log.Printf("WARN Failed to ensure pool directory exists (may already exist): %v", err)
	}

	// Copy disk file using scp (run locally from the pod, not through SSH)
	log.Printf("INFO Copying disk file (%s) to remote host via scp...", localPath)

	// Host-key options come from the same centralized policy as the virsh
	// paths (#149/ADR-0004) so the disk-image transfer is verified against the
	// same trust material. Re-emit the verification-mode audit line for the scp
	// connection and hard-fail before transfer if verification is on but no
	// usable known_hosts is present (no TOFU).
	virshProvider.hostKey.logVerificationMode(virshProvider.logger, host)
	if err := virshProvider.hostKey.verifyKnownHostsPresent(host); err != nil {
		return "", fmt.Errorf("libvirt scp host-key verification pre-flight failed: %w", err)
	}
	hostKeyOpts := virshProvider.hostKey.sshHostKeyOptions()
	// Share the SSH connection with the virsh path via ControlMaster (#194).
	hostKeyOpts = append(hostKeyOpts, sshMultiplexOptions()...)

	// Run scp LOCALLY on the pod to copy to remote host
	var cmd *exec.Cmd
	if virshProvider.credentials.Password != "" {
		// Use sshpass with scp for password authentication
		scpArgs := append([]string{"-e", "scp"}, hostKeyOpts...)
		scpArgs = append(scpArgs, localPath, fmt.Sprintf("%s:%s", sshTarget, remotePath))
		cmd = exec.CommandContext(ctx, "sshpass", scpArgs...)
		// Set password via environment variable for sshpass
		cmd.Env = append(os.Environ(), fmt.Sprintf("SSHPASS=%s", virshProvider.credentials.Password))
	} else {
		// Fallback to scp without sshpass (for key-based auth)
		scpArgs := append([]string{}, hostKeyOpts...)
		scpArgs = append(scpArgs, localPath, fmt.Sprintf("%s:%s", sshTarget, remotePath))
		cmd = exec.CommandContext(ctx, "scp", scpArgs...)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("scp failed: %w, output: %s", err, string(output))
	}

	log.Printf("INFO Successfully copied disk file to remote host: %s", remotePath)
	return remotePath, nil
}

// Helper functions for generating IDs and timestamps (shared with vSphere)
func generateTimestamp() int64 {
	return time.Now().Unix()
}

// sanitizeSnapshotName ensures snapshot name is valid for virsh
// Virsh snapshot names must contain only alphanumeric, underscore, hyphen, and period
func sanitizeSnapshotName(name string) string {
	// Replace invalid characters with hyphens
	reg := regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
	sanitized := reg.ReplaceAllString(name, "-")

	// Ensure it starts with alphanumeric
	sanitized = strings.TrimLeft(sanitized, "-_.")

	// Limit length to 64 characters (virsh limit)
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}

	// If empty after sanitization, use default
	if sanitized == "" {
		sanitized = fmt.Sprintf("snapshot-%d", generateTimestamp())
	}

	return sanitized
}
