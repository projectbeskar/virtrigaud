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
	stderrors "errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/storage/migration"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Server implements the providerv1.ProviderServer interface for Libvirt.
//
// It holds the provider through the providerBackend interface (satisfied by
// *Provider), not the concrete type. This is the ADR-0008 PR 2 de-weld: the six
// snapshot/clone/image/import RPCs that used to type-assert s.provider.(*Provider)
// and reach into unexported fields now go through providerBackend and the
// per-host connection seam, so an alternative transport (PR 3/PR 4) can be
// swapped underneath without touching this layer.
type Server struct {
	providerv1.UnimplementedProviderServer
	provider providerBackend
}

// NewServer creates a new Libvirt gRPC server. provider is the libvirt provider
// implementation (a *Provider); a nil provider yields a server whose RPCs report
// "not initialized" until one is wired, which the health path and tests rely on.
func NewServer(provider providerBackend) *Server {
	return &Server{
		provider: provider,
	}
}

// clusteredProvider reports whether the backend runs in CLUSTERED topology
// (ADR-0007 D3). Per-VM RPCs then need a routed host (ADR-0007 Addendum A):
// Describe, Delete, Power and Reconfigure are routed; the rest are refused
// until their slice lands.
func (s *Server) clusteredProvider() bool {
	return s.provider != nil && s.provider.clustered()
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
		return nil, createRPCError(err)
	}

	result := &providerv1.CreateResponse{
		Id: resp.ID,
	}

	if resp.TaskRef != "" {
		result.Task = &providerv1.TaskRef{Id: resp.TaskRef}
	}

	return result, nil
}

// createRPCError converts a Create failure into the error returned on the wire.
// The two NON-retryable classes carry a real gRPC status so the manager can tell
// them apart from a transient failure (and so they do not count as infra errors
// toward its circuit breaker):
//
//   - Conflict -> codes.AlreadyExists: the domain name is taken by a domain this
//     VirtualMachine does not own; the provider refused to bind to it.
//   - InvalidSpec -> codes.InvalidArgument: the request can never succeed as-is
//     (e.g. a name virsh would resolve as a domain ID/UUID).
//
// A clustered create whose target host is unknown or unreachable is
// HostUnavailable -> codes.Unavailable with a HOST_UNAVAILABLE ErrorInfo
// (retryable, and not counted by the manager's circuit breaker), so the
// operator can report the pending host as unavailable instead of re-scheduling
// (ADR-0007 Addendum A, A2). Only the clustered routing produces that class.
//
// Any other failure of a clustered create on its target host (hostOpError) is
// host-scoped too: a host that could not be reached is HOST_UNAVAILABLE, and
// anything else keeps the historical code and message plus a
// VM_OPERATION_FAILED ErrorInfo, so neither counts toward the manager's
// per-Provider circuit breaker (ADR-0007 Addendum A, slice 2; hostOpRPCError).
//
// Only the categorized message crosses the wire (it is written to be safe for
// the requesting VirtualMachine's status). Every other error — every
// single-host error among them — keeps the historical wrapped form.
func createRPCError(err error) error {
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) {
		switch pe.Type {
		case contracts.ErrorTypeConflict:
			return status.Error(codes.AlreadyExists, pe.Message)
		case contracts.ErrorTypeInvalidSpec:
			return status.Error(codes.InvalidArgument, pe.Message)
		case contracts.ErrorTypeHostUnavailable:
			return hostUnavailableStatus(pe)
		}
	}
	var ho *hostOpError
	if stderrors.As(err, &ho) {
		return hostOpRPCError("create VM", ho, err)
	}
	return fmt.Errorf("failed to create VM: %w", err)
}

// Delete deletes a virtual machine. On a clustered provider the delete is
// routed to target_host_id and owner-checked (ADR-0007 Addendum A); a domain
// this VM does not own is answered NotFound and left untouched.
func (s *Server) Delete(ctx context.Context, req *providerv1.DeleteRequest) (*providerv1.TaskResponse, error) {
	taskRef, err := s.provider.Delete(ctx, contracts.VMRef{ID: req.Id, HostID: req.TargetHostId, Owner: ownerFromProto(req.GetOwner())})
	if err != nil {
		if s.clusteredProvider() {
			return nil, routedRPCError("delete VM", err)
		}
		return nil, fmt.Errorf("failed to delete VM: %w", err)
	}

	result := &providerv1.TaskResponse{}
	if taskRef != "" {
		result.Task = &providerv1.TaskRef{Id: taskRef}
	}

	return result, nil
}

// Power performs power operations on a virtual machine. On a clustered
// provider the operation is routed to target_host_id and owner-checked
// (ADR-0007 Addendum A, slice 2); a domain this VM does not own is answered
// NotFound and left untouched.
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
		if s.clusteredProvider() {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported power operation: %v", req.Op)
		}
		return nil, fmt.Errorf("unsupported power operation: %v", req.Op)
	}

	taskRef, err := s.provider.Power(ctx, contracts.VMRef{ID: req.Id, HostID: req.TargetHostId, Owner: ownerFromProto(req.GetOwner())}, powerOp)
	if err != nil {
		if s.clusteredProvider() {
			return nil, routedRPCError("perform power operation", err)
		}
		return nil, fmt.Errorf("failed to perform power operation: %w", err)
	}

	result := &providerv1.TaskResponse{}
	if taskRef != "" {
		result.Task = &providerv1.TaskRef{Id: taskRef}
	}

	return result, nil
}

// Reconfigure reconfigures a virtual machine. On a clustered provider the
// reconfigure is routed to target_host_id and owner-checked (ADR-0007 Addendum
// A, slice 2); a domain this VM does not own is answered NotFound and none of
// its CPU, memory or disks is changed.
func (s *Server) Reconfigure(ctx context.Context, req *providerv1.ReconfigureRequest) (*providerv1.TaskResponse, error) {
	// Parse the desired configuration
	var createReq contracts.CreateRequest
	if err := json.Unmarshal([]byte(req.DesiredJson), &createReq); err != nil {
		if s.clusteredProvider() {
			return nil, status.Errorf(codes.InvalidArgument, "failed to parse desired configuration: %v", err)
		}
		return nil, fmt.Errorf("failed to parse desired configuration: %w", err)
	}

	taskRef, err := s.provider.Reconfigure(ctx, contracts.VMRef{ID: req.Id, HostID: req.TargetHostId, Owner: ownerFromProto(req.GetOwner())}, createReq)
	if err != nil {
		if s.clusteredProvider() {
			return nil, routedRPCError("reconfigure VM", err)
		}
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

	resp, err := s.provider.Describe(ctx, contracts.VMRef{ID: req.Id, HostID: req.TargetHostId, Owner: ownerFromProto(req.GetOwner())})
	if err != nil {
		if s.clusteredProvider() {
			return nil, routedRPCError("describe VM", err)
		}
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
		// TargetHostID is the clustered create binding (ADR-0007 P1, D4). It is
		// empty for single-host callers; the clustered Create path requires it and
		// routes the create onto that host's connection (provider_virsh.go).
		TargetHostID: req.TargetHostId,
	}

	// Owner is the requesting VirtualMachine's identity: stamped onto the domain
	// on create, and the ONLY thing that authorizes treating an existing domain of
	// the same name as this VM's (see bindExistingDomain). Absent from an older
	// manager, in which case an existing domain is never bound.
	createReq.Owner = ownerFromProto(req.GetOwner())

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

// ownerFromProto converts the wire ObjectIdentity (CreateRequest.owner and the
// owner of the routed Describe, Delete, Power and Reconfigure requests) to the
// provider-contract form. A nil identity (an older manager that sends none)
// yields the zero ObjectIdentity, which never authorizes anything.
func ownerFromProto(o *providerv1.ObjectIdentity) contracts.ObjectIdentity {
	if o == nil {
		return contracts.ObjectIdentity{}
	}
	return contracts.ObjectIdentity{
		UID:       o.GetUid(),
		Namespace: o.GetNamespace(),
		Name:      o.GetName(),
	}
}

// importVolumeName is the volume name an ImportDisk request lands its disk
// under (importedDiskVolumeName over the request's target_vm and target_name):
// "<namespace>.<name>-migrated" for a request naming its target VirtualMachine,
// the legacy target_name otherwise ("" lets the caller generate one). A request
// whose target_name and target_vm disagree is InvalidArgument.
func importVolumeName(req *providerv1.ImportDiskRequest) (string, error) {
	name, err := importedDiskVolumeName(ownerFromProto(req.GetTargetVm()), req.GetTargetName())
	if err != nil {
		var pe *contracts.ProviderError
		if stderrors.As(err, &pe) {
			return "", status.Error(codes.InvalidArgument, pe.Message)
		}
		return "", err
	}
	return name, nil
}

// SnapshotCreate creates a VM snapshot
func (s *Server) SnapshotCreate(ctx context.Context, req *providerv1.SnapshotCreateRequest) (*providerv1.SnapshotCreateResponse, error) {
	if s.clusteredProvider() {
		return nil, notRoutedYet("SnapshotCreate", sliceRoutedSnapshotCloneDisk)
	}
	log.Printf("INFO Creating snapshot for VM: %s", req.VmId)

	// Obtain the per-host connection through the seam (ADR-0008 PR 2) instead of
	// type-asserting the concrete *Provider.
	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, err
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
	domainState, err := conn.getDomainState(ctx, req.VmId)
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

	// Execute snapshot creation (control-plane exec through the seam)
	result, err := conn.Virsh(ctx, args...)
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
	if s.clusteredProvider() {
		return nil, notRoutedYet("SnapshotDelete", sliceRoutedSnapshotCloneDisk)
	}
	log.Printf("INFO Deleting snapshot %s from VM: %s", req.SnapshotId, req.VmId)

	// Obtain the per-host connection through the seam (ADR-0008 PR 2).
	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, err
	}

	// Check if snapshot exists
	exists, err := conn.snapshotExists(ctx, req.VmId, req.SnapshotId)
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

	result, err := conn.Virsh(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to delete snapshot: %w", err)
	}

	log.Printf("INFO Snapshot deleted successfully: %s\nOutput: %s", req.SnapshotId, result.Stdout)

	// Return empty response (synchronous operation)
	return &providerv1.TaskResponse{}, nil
}

// SnapshotRevert reverts a VM to a snapshot
func (s *Server) SnapshotRevert(ctx context.Context, req *providerv1.SnapshotRevertRequest) (*providerv1.TaskResponse, error) {
	if s.clusteredProvider() {
		return nil, notRoutedYet("SnapshotRevert", sliceRoutedSnapshotCloneDisk)
	}
	log.Printf("INFO Reverting VM %s to snapshot: %s", req.VmId, req.SnapshotId)

	// Obtain the per-host connection through the seam (ADR-0008 PR 2).
	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, err
	}

	// Check if snapshot exists
	exists, err := conn.snapshotExists(ctx, req.VmId, req.SnapshotId)
	if err != nil {
		return nil, fmt.Errorf("failed to check snapshot existence: %w", err)
	}

	if !exists {
		return nil, fmt.Errorf("snapshot %s does not exist", req.SnapshotId)
	}

	// Get current domain state
	domainState, err := conn.getDomainState(ctx, req.VmId)
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

	result, err := conn.Virsh(ctx, args...)
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
	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	if s.clusteredProvider() {
		return nil, notRoutedYet("Clone", sliceRoutedSnapshotCloneDisk)
	}

	resp, err := s.provider.Clone(ctx, contracts.CloneRequest{
		Source:        contracts.VMRef{ID: req.SourceVmId, HostID: req.SourceHostId},
		TargetName:    req.TargetName,
		TargetVM:      ownerFromProto(req.GetTargetVm()),
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

// ImagePrepare prepares/imports a VM image into a libvirt storage pool
// (issue #154; ADR-0009 prepared-image artifact identity, Slice 4).
//
// The request is decoded by imageartifact.ParseRequest (a malformed request is
// InvalidArgument) and served in one of two modes (see image.go):
//
//   - identity mode (image + source_digest, empty target_name): the artifact
//     is named from the image identity and source digest, stamped, reused only
//     on a matching stamp, and published atomically (image_publish.go). The
//     response echoes the stamp in ImagePrepareResponse.artifact;
//   - deprecated legacy mode (a bare target_name from a manager older than
//     ADR-0009, this release only): the pre-ADR bare-name artifact with the
//     ADR-0009 provider-internal fixes and no artifact echo. Every such request
//     emits the deprecation signal (a WARN log and
//     virtrigaud_provider_image_prepare_legacy_requests_total{provider_type}).
//
// libvirt/qemu-img are synchronous, so the response carries no task; the
// controller treats that as "completed synchronously". prepared_image_id is the
// artifact base name and prepared_image_path its absolute pool path, so the
// manager creates VMs from the prepared image instead of re-resolving the
// source (issue #154, PR-6 / #214). Errors are classified by
// imagePrepareRPCError.
func (s *Server) ImagePrepare(ctx context.Context, req *providerv1.ImagePrepareRequest) (*providerv1.ImagePrepareResponse, error) {
	log.Printf("INFO ImagePrepare: target=%q image=%s/%s uid=%q storageHint=%q", req.GetTargetName(),
		req.GetImage().GetNamespace(), req.GetImage().GetName(), req.GetImage().GetUid(), req.GetStorageHint())

	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	if s.clusteredProvider() {
		// Host-scoped, with no target_host_id yet: a clustered provider reports
		// supports_image_import=false and refuses here (ADR-0007 Addendum A, A1).
		return nil, status.Error(codes.Unimplemented,
			"ImagePrepare is host-scoped and not routed to a host on a clustered libvirt provider yet "+
				"(supports_image_import=false; ADR-0007 Addendum A)")
	}

	parsed, err := imageartifact.ParseRequest(req)
	if err != nil {
		return nil, err
	}
	if parsed.Mode == imageartifact.ModeLegacy {
		imageartifact.SignalLegacyRequest(ctx, nil, libvirtProviderType, parsed.LegacyTargetName)
	}

	res, err := s.provider.imagePrepare(ctx, parsed, req.GetImageJson(), req.GetStorageHint())
	if err != nil {
		return nil, imagePrepareRPCError(err)
	}

	// Synchronous: no task reference. An empty Task signals "completed". The
	// id/path tell the manager where the prepared image landed; in identity
	// mode the artifact echo reports the stamp the provider verified or wrote.
	resp := &providerv1.ImagePrepareResponse{
		PreparedImageId:   res.ID,
		PreparedImagePath: res.Path,
	}
	if parsed.Mode == imageartifact.ModeIdentity && res.Stamp != nil {
		resp.Artifact = res.Stamp.PreparedArtifact(res.ID, res.Reused)
	}
	return resp, nil
}

// imagePrepareRPCError converts an ImagePrepare failure into the error returned
// on the wire, so the manager can tell a request that can never succeed from a
// transient failure (see the classification in image.go):
//
//   - InvalidSpec -> codes.InvalidArgument: the manager records the rejection
//     on the VMImage and holds instead of retrying every few seconds (a source
//     404, a checksum mismatch, an unreadable or unsafe image, a confinement
//     rejection, a pool that cannot hold the image);
//   - Conflict -> codes.AlreadyExists (ADR-0009 D4);
//   - a download failure the image source caused (imageSourceFailure) ->
//     imageartifact.SourceUnavailableError: codes.Unavailable with an
//     IMAGE_SOURCE_UNAVAILABLE ErrorInfo, retried by the manager but kept out
//     of its circuit breaker;
//   - an error that already carries a gRPC status keeps it (the sdk
//     InvalidSpec of the #334 confinement, imageartifact's Conflict and
//     in-progress Unavailable);
//   - anything else — the SSH transport or the host failing — keeps the
//     historical wrapped form, which the manager retries.
func imagePrepareRPCError(err error) error {
	var src *imageSourceFailure
	if stderrors.As(err, &src) {
		// The text is the historical wire text, byte for byte (only the code
		// and the ErrorInfo change).
		return imageartifact.SourceUnavailableError("failed to prepare image: " + src.pe.Error())
	}
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) {
		switch pe.Type {
		case contracts.ErrorTypeInvalidSpec:
			return status.Error(codes.InvalidArgument, "failed to prepare image: "+pe.Message)
		case contracts.ErrorTypeConflict:
			return status.Error(codes.AlreadyExists, "failed to prepare image: "+pe.Message)
		}
	}
	return fmt.Errorf("failed to prepare image: %w", err)
}

// GetCapabilities returns the capabilities of the Libvirt provider. A
// clustered provider hides every per-VM capability whose RPC is not routed to a
// host yet, and reports supports_image_import=false (ADR-0007 Addendum A, D7
// honesty-first); see clusteredCapabilities.
func (s *Server) GetCapabilities(ctx context.Context, req *providerv1.GetCapabilitiesRequest) (*providerv1.GetCapabilitiesResponse, error) {
	if s.clusteredProvider() {
		return clusteredCapabilities(), nil
	}
	return &providerv1.GetCapabilitiesResponse{
		SupportsReconfigureOnline:   true, // Online CPU/mem reconfigure via `setvcpus/setmem --live` for VMs created with CPU/MemoryHotAddEnabled (headroom provisioned at create); grows up to the ~4× ceiling, beyond which a power-cycle is required (#203)
		SupportsDiskExpansionOnline: true, // Online grow via `virsh blockresize` + best-effort in-guest FS grow (resize2fs/xfs_growfs) when the guest agent is present; grow-only (#201)
		SupportsSnapshots:           true, // Libvirt supports snapshots (storage-dependent)
		SupportsMemorySnapshots:     true, // Full system checkpoints incl. RAM via `snapshot-create-as` without --disk-only; requires the VM running (#202)
		SupportsLinkedClones:        true, // Clone RPC implemented: qcow2 overlay (linked) + vol-clone (full) (issue #153)
		SupportsImageImport:         true, // ImagePrepare RPC implemented: import/convert image into a storage pool (issue #154)
		// ADR-0009 Slice 4: prepared images are named from the VMImage identity
		// and source digest, stamped (sidecar), reused only on a matching stamp
		// and published with link(2) — never reused by a bare name. Hidden on a
		// clustered provider, which does not serve ImagePrepare yet.
		SupportsImageArtifactIdentity: true,
		SupportedDiskTypes:            []string{"qcow2", "raw", "vmdk"},
		SupportedNetworkTypes:         []string{"virtio", "e1000", "rtl8139"},
		SupportsDiskExport:            true, // ExportDisk wired to virsh impl (issue #177)
		SupportsDiskImport:            true, // ImportDisk wired (pvc:///file:// sources)
		SupportedExportFormats:        []string{"qcow2", "raw"},
		SupportedImportFormats:        []string{"qcow2", "raw", "vmdk"},
		SupportsExportCompression:     true, // ExportDisk honors req.Compress via qemu-img -c for qcow2 (#199); default (Compress=false) is uncompressed for speed
		// ADR-0006: libvirt is the TARGET of the vSphere → S3 → libvirt relay
		// (Slice 1) AND, as of Slice 2, the SOURCE of the libvirt → S3 → vSphere
		// reverse relay. It therefore both IMPORTS (download + host-side
		// vmdk→qcow2 convert) and EXPORTS (host-side flatten to standalone qcow2 +
		// stream) over pvc AND s3. Only the relay transfer mode is implemented;
		// direct is not.
		SupportedExportBackends: migration.PVCS3AndNFSExportBackends(),
		SupportedImportBackends: migration.PVCS3AndNFSImportBackends(),
		SupportedTransferModes:  migration.RelayOnlyTransferModes(),
		// ADR-0007 P1 / D7 (honesty-first): advertise clustering only when the
		// backend is actually in CLUSTERED topology, i.e. it fronts N host-keyed
		// connections and answers ListHosts/GetHostInfo for real. Single-host mode
		// (and an uninitialized server) reports false, keeping the host-inventory
		// surface Unimplemented there (D9). This flips true only now that the real
		// libvirt host-inventory implementation ships.
		SupportsClustering: s.provider != nil && s.provider.clustered(),
	}, nil
}

// clusteredCapabilities is the GetCapabilities answer of a CLUSTERED provider
// (ADR-0007 Addendum A, A1 + D7). Only what is routed to a host is advertised:
// Create (target_host_id) and the routed Describe/Delete/Power need no flag;
// the routed Reconfigure (slice 2) runs the same core as single-host, so its
// online CPU/memory reconfigure and online disk expansion are advertised as on
// a single-host provider; supports_clustering is true. Every per-VM capability
// whose RPC is still refused until its slice lands — snapshots, linked clones,
// disk export (slice 3) — is hidden, as are disk import (no target host until
// P3) and image import (host-scoped, no target_host_id yet). The format /
// backend / transfer lists are left empty with their capability off.
func clusteredCapabilities() *providerv1.GetCapabilitiesResponse {
	return &providerv1.GetCapabilitiesResponse{
		SupportsReconfigureOnline:   true, // routed + owner-checked since Addendum A slice 2; same setvcpus/setmem --live core as single-host (#203)
		SupportsDiskExpansionOnline: true, // routed + owner-checked since Addendum A slice 2; blockresize + guest-agent FS grow on the bound host (#201)
		SupportedDiskTypes:          []string{"qcow2", "raw", "vmdk"},
		SupportedNetworkTypes:       []string{"virtio", "e1000", "rtl8139"},
		SupportsClustering:          true,
	}
}

// ExportDisk exports a VM disk for migration. It delegates to the libvirt
// Provider implementation (provider_virsh.go), translating between the gRPC and
// provider-contract types. Previously this RPC was unreachable over gRPC and
// returned Unimplemented despite a working implementation (issue #177).
func (s *Server) ExportDisk(ctx context.Context, req *providerv1.ExportDiskRequest) (*providerv1.ExportDiskResponse, error) {
	if s.clusteredProvider() {
		return nil, notRoutedYet("ExportDisk", sliceRoutedSnapshotCloneDisk)
	}
	// ADR-0006: libvirt is a SOURCE for the S3 relay export (Slice 2, the reverse
	// of Slice 1's vSphere→S3→libvirt). Accept pvc and s3; reject nfs/unknown
	// honestly. Only the relay transfer mode is implemented; an explicit "direct"
	// fails loudly (never a silent downgrade, ADR-0006 D2).
	if err := migration.EnsurePVCS3OrNFSBackend(req.BackendType); err != nil {
		return nil, err
	}
	// Relay-mode enforcement applies to the s3 streaming path; the nfs transport
	// is qemu-img-native (the host writes nfs:// directly via libnfs), so it is
	// exempt from the relay/direct distinction (ADR-0006 Slice 4).
	if req.BackendType != migration.BackendNFS {
		if err := migration.EnsureRelayMode(req.TransferMode); err != nil {
			return nil, err
		}
	}

	// NFS export: the host's qemu-img writes the flattened qcow2 straight to the
	// nfs:// export (no pod hop, no S3 client).
	if req.BackendType == migration.BackendNFS {
		return s.exportDiskToNFS(ctx, req)
	}

	// S3 relay export: flatten the disk on the host and stream the standalone
	// qcow2 up to S3. Never logs the credentials map.
	if req.BackendType == migration.BackendS3 {
		return s.exportDiskToS3(ctx, req)
	}

	if s.provider == nil {
		return nil, fmt.Errorf("provider not initialized")
	}

	resp, err := s.provider.ExportDisk(ctx, contracts.ExportDiskRequest{
		VM:             contracts.VMRef{ID: req.VmId, HostID: req.TargetHostId},
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
	if s.clusteredProvider() {
		return nil, notRoutedYet("GetDiskInfo", sliceRoutedSnapshotCloneDisk)
	}

	resp, err := s.provider.GetDiskInfo(ctx, contracts.GetDiskInfoRequest{
		VM:         contracts.VMRef{ID: req.VmId, HostID: req.TargetHostId},
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
	if s.clusteredProvider() {
		return nil, notRoutedYet("ImportDisk", sliceRoutedImport)
	}
	// ADR-0006: libvirt is a TARGET for the S3 relay import (Slice 1). Accept pvc
	// and s3; reject nfs/unknown honestly. Only the relay transfer mode is
	// implemented; an explicit "direct" fails loudly.
	if err := migration.EnsurePVCS3OrNFSBackend(req.BackendType); err != nil {
		return nil, err
	}
	if req.BackendType != migration.BackendNFS {
		if err := migration.EnsureRelayMode(req.TransferMode); err != nil {
			return nil, err
		}
	}

	// NFS import: the host's qemu-img reads the staged qcow2 straight from the
	// nfs:// export and writes it into the target pool (ADR-0006 Slice 4).
	if req.BackendType == migration.BackendNFS {
		return s.importDiskFromNFS(ctx, req)
	}

	// S3 relay import: download from S3 and stream into host-side qemu-img
	// convert (vmdk→qcow2). Never logs the credentials map.
	if req.BackendType == migration.BackendS3 {
		return s.importDiskFromS3(ctx, req)
	}

	log.Printf("INFO Starting disk import from %s", req.SourceUrl)

	// Obtain the per-host connection through the seam (ADR-0008 PR 2) instead of
	// type-asserting the concrete *Provider.
	if s.provider == nil {
		return nil, fmt.Errorf("libvirt provider not initialized")
	}
	conn, err := s.provider.conn(ctx)
	if err != nil {
		return nil, err
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
	volumeName, err := importVolumeName(req)
	if err != nil {
		return nil, err
	}
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
	if strings.Contains(conn.uri(), "ssh://") {
		// The copy lands directly at <DefaultImageDir>/<volume>.qcow2: never
		// over a disk another domain uses.
		if err := ensureDiskTargetFree(ctx, hostConnRunner{conn: conn}, importedDiskSubject(volumeName),
			filepath.Join(DefaultImageDir, volumeName+qcow2Ext)); err != nil {
			return nil, err
		}
		log.Printf("INFO Copying disk file to remote libvirt host...")
		remotePath, err := conn.copyDiskToRemote(ctx, sourcePath, volumeName)
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

	// Get source disk info using qemu-img (shell exec through the seam)
	infoResult, err := conn.RunHost(ctx, "qemu-img", "info", "--output=json", finalSourcePath)
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

	// Create storage provider (bound to this host's connection)
	storageProvider := conn.storageProvider()

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
	statResult, err := conn.RunHost(ctx, "stat", "-c", "%s", volume.Path)
	if err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(statResult.Stdout), "%d", &actualSizeBytes)
	}

	// Calculate checksum if requested
	checksum := ""
	if req.VerifyChecksum {
		log.Printf("INFO Calculating SHA256 checksum of imported disk...")
		checksumResult, err := conn.RunHost(ctx, "sha256sum", volume.Path)
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
	if s.clusteredProvider() {
		// Not per-VM: a clustered ListVMs runs across every host (A3).
		return nil, notRoutedYet("ListVMs", sliceRoutedListVMs)
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
