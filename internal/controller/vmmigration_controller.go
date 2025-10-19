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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// VMMigrationReconciler reconciles a VMMigration object
type VMMigrationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RemoteResolver *remote.Resolver
	Recorder       record.EventRecorder
	metrics        *metrics.ReconcileMetrics
}

// NewVMMigrationReconciler creates a new VMMigration reconciler
func NewVMMigrationReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	remoteResolver *remote.Resolver,
	recorder record.EventRecorder,
) *VMMigrationReconciler {
	return &VMMigrationReconciler{
		Client:         client,
		Scheme:         scheme,
		RemoteResolver: remoteResolver,
		Recorder:       recorder,
		metrics:        metrics.NewReconcileMetrics("VMMigration"),
	}
}

//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmmigrations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmmigrations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmmigrations/finalizers,verbs=update
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsnapshots,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop
func (r *VMMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	timer := metrics.NewReconcileTimer("VMMigration")
	defer timer.Finish(metrics.OutcomeSuccess)

	// Add correlation context
	ctx = logging.WithCorrelationID(ctx, fmt.Sprintf("vmmigration-%s", req.Name))
	logger := logging.FromContext(ctx)

	logger.Info("Reconciling VMMigration", "migration", req.NamespacedName)

	// Fetch the VMMigration instance
	migration := &infrav1beta1.VMMigration{}
	if err := r.Get(ctx, req.NamespacedName, migration); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("VMMigration not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get VMMigration")
		timer.Finish(metrics.OutcomeError)
		return ctrl.Result{}, err
	}

	// Add migration context
	ctx = logging.WithCorrelationID(ctx, fmt.Sprintf("vmmigration-%s/%s", migration.Namespace, migration.Name))
	logger = logging.FromContext(ctx)

	// Handle deletion
	if !migration.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, migration)
	}

	// Add finalizer if needed
	const finalizerName = "vmmigration.infra.virtrigaud.io/finalizer"
	if !controllerutil.ContainsFinalizer(migration, finalizerName) {
		controllerutil.AddFinalizer(migration, finalizerName)
		if err := r.Update(ctx, migration); err != nil {
			logger.Error(err, "Failed to add finalizer")
			timer.Finish(metrics.OutcomeError)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize status if needed
	if migration.Status.Phase == "" {
		migration.Status.Phase = infrav1beta1.MigrationPhasePending
		migration.Status.StartTime = &metav1.Time{Time: time.Now()}
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Handle migration lifecycle based on phase
	switch migration.Status.Phase {
	case infrav1beta1.MigrationPhasePending:
		return r.handlePendingPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseValidating:
		return r.handleValidatingPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseSnapshotting:
		return r.handleSnapshottingPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseExporting:
		return r.handleExportingPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseTransferring:
		return r.handleTransferringPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseConverting:
		return r.handleConvertingPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseImporting:
		return r.handleImportingPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseCreating:
		return r.handleCreatingPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseValidatingTarget:
		return r.handleValidatingTargetPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseReady:
		return r.handleReadyPhase(ctx, migration)
	case infrav1beta1.MigrationPhaseFailed:
		return r.handleFailedPhase(ctx, migration)
	default:
		logger.Info("Unknown migration phase", "phase", migration.Status.Phase)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// handlePendingPhase transitions from Pending to Validating
func (r *VMMigrationReconciler) handlePendingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling pending phase")

	// Transition to validating
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
	migration.Status.Message = "Starting validation"

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
		metav1.ConditionTrue, "ValidationStarted",
		"Migration validation started")

	r.Recorder.Event(migration, "Normal", "ValidationStarted", "Starting migration validation")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleValidatingPhase performs validation checks
func (r *VMMigrationReconciler) handleValidatingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Validating migration requirements")

	// Validate source VM exists
	sourceVM, err := r.getSourceVM(ctx, migration)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get source VM: %v", err))
	}

	// Validate source VM is provisioned
	if sourceVM.Status.ID == "" {
		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
			metav1.ConditionFalse, "SourceVMNotReady",
			"Source VM is not yet provisioned")
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Validate source provider
	var sourceProviderRef infrav1beta1.ObjectRef
	if migration.Spec.Source.ProviderRef != nil {
		sourceProviderRef = *migration.Spec.Source.ProviderRef
	} else {
		// Auto-detect from source VM
		sourceProviderRef = sourceVM.Spec.ProviderRef
	}
	sourceProvider, err := r.getProvider(ctx, sourceProviderRef, migration.Namespace)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get source provider: %v", err))
	}

	// Validate target provider
	targetProvider, err := r.getProvider(ctx, migration.Spec.Target.ProviderRef, migration.Namespace)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get target provider: %v", err))
	}

	// Validate providers are ready
	if !r.isProviderReady(sourceProvider) {
		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
			metav1.ConditionFalse, "SourceProviderNotReady",
			"Source provider is not ready")
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if !r.isProviderReady(targetProvider) {
		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
			metav1.ConditionFalse, "TargetProviderNotReady",
			"Target provider is not ready")
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Validate storage configuration
	if migration.Spec.Storage != nil {
		if err := r.validateStorageConfig(ctx, migration.Spec.Storage); err != nil {
			return r.transitionToFailed(ctx, migration, fmt.Sprintf("Invalid storage configuration: %v", err))
		}

		// Ensure PVC exists or create it
		pvcName, err := r.ensureMigrationPVC(ctx, migration)
		if err != nil {
			return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to ensure migration PVC: %v", err))
		}

		// Store PVC name in migration status for later use
		if migration.Status.StoragePVCName == "" {
			migration.Status.StoragePVCName = pvcName
			if err := r.updateStatus(ctx, migration); err != nil {
				return ctrl.Result{}, err
			}
		}

		logger.Info("Migration storage PVC ready", "pvc", pvcName)
	}

	// Validation complete, transition to next phase
	logger.Info("Validation complete")

	// Check if we need to create a snapshot
	if migration.Spec.Source.CreateSnapshot {
		migration.Status.Phase = infrav1beta1.MigrationPhaseSnapshotting
		migration.Status.Message = "Preparing to snapshot source VM"
	} else {
		// Skip snapshotting, go directly to export
		migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
		migration.Status.Message = "Preparing to export source VM"
	}

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
		metav1.ConditionTrue, "ValidationComplete",
		"Migration validation completed successfully")

	r.Recorder.Event(migration, "Normal", "ValidationComplete", "Migration validation completed")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleSnapshottingPhase creates a snapshot of the source VM
func (r *VMMigrationReconciler) handleSnapshottingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling snapshotting phase")

	// Get source VM
	sourceVM, err := r.getSourceVM(ctx, migration)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get source VM: %v", err))
	}

	// Check if user specified a snapshot to use
	if migration.Spec.Source.SnapshotRef != nil {
		// Use existing snapshot
		migration.Status.SnapshotID = migration.Spec.Source.SnapshotRef.Name
		migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
		migration.Status.Message = fmt.Sprintf("Using existing snapshot %s", migration.Status.SnapshotID)

		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
			metav1.ConditionTrue, "SnapshotSelected",
			"Using existing snapshot")

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// Get source provider
	var sourceProviderRef infrav1beta1.ObjectRef
	if migration.Spec.Source.ProviderRef != nil {
		sourceProviderRef = *migration.Spec.Source.ProviderRef
	} else {
		sourceProviderRef = sourceVM.Spec.ProviderRef
	}
	sourceProvider, err := r.getProvider(ctx, sourceProviderRef, migration.Namespace)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get source provider: %v", err))
	}

	// Get provider instance
	providerInstance, err := r.getProviderInstance(ctx, sourceProvider)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get provider instance: %v", err))
	}

	// Check if we already created a snapshot
	if migration.Status.SnapshotID != "" {
		// Check if snapshot is complete
		// For now, assume it's complete and transition to export
		// TODO: Check snapshot status via provider
		migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
		migration.Status.Message = "Snapshot ready, starting export"

		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
			metav1.ConditionTrue, "SnapshotComplete",
			"Source VM snapshot created")

		r.Recorder.Event(migration, "Normal", "SnapshotComplete", "Source VM snapshot created")

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// Create snapshot
	snapshotName := fmt.Sprintf("%s-migration-%s", migration.Spec.Source.VMRef.Name, migration.UID[:8])
	snapshotReq := contracts.SnapshotCreateRequest{
		VmId:          sourceVM.Status.ID,
		NameHint:      snapshotName,
		Description:   fmt.Sprintf("Migration snapshot for %s", migration.Name),
		IncludeMemory: false, // Disk-only snapshot for migration
		Quiesce:       false,
	}

	logger.Info("Creating migration snapshot", "snapshot_name", snapshotName)
	snapshotResp, err := providerInstance.SnapshotCreate(ctx, snapshotReq)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to create snapshot: %v", err))
	}

	// Store snapshot ID in status
	migration.Status.SnapshotID = snapshotResp.SnapshotId
	migration.Status.Message = fmt.Sprintf("Snapshot %s created", snapshotResp.SnapshotId)

	// If there's a task, we need to wait for it to complete
	if snapshotResp.Task != nil && snapshotResp.Task.ID != "" {
		migration.Status.TaskRef = snapshotResp.Task.ID
		logger.Info("Snapshot creation task started, waiting for completion", "task_id", snapshotResp.Task.ID)

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Snapshot created synchronously, move to export
	migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
	migration.Status.Message = "Snapshot created, starting export"
	migration.Status.TaskRef = ""

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
		metav1.ConditionTrue, "SnapshotComplete",
		"Source VM snapshot created")

	r.Recorder.Event(migration, "Normal", "SnapshotComplete", fmt.Sprintf("Snapshot %s created", snapshotResp.SnapshotId))

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleExportingPhase exports the disk from source provider
func (r *VMMigrationReconciler) handleExportingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling exporting phase")

	// Get source VM
	sourceVM, err := r.getSourceVM(ctx, migration)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get source VM: %v", err))
	}

	// Get source provider
	var sourceProviderRef infrav1beta1.ObjectRef
	if migration.Spec.Source.ProviderRef != nil {
		sourceProviderRef = *migration.Spec.Source.ProviderRef
	} else {
		sourceProviderRef = sourceVM.Spec.ProviderRef
	}
	sourceProvider, err := r.getProvider(ctx, sourceProviderRef, migration.Namespace)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get source provider: %v", err))
	}

	// Get provider instance
	providerInstance, err := r.getProviderInstance(ctx, sourceProvider)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get provider instance: %v", err))
	}

	// Check if export is already in progress
	if migration.Status.ExportID != "" {
		// Check export task status if there is one
		if migration.Status.TaskRef != "" {
			done, err := providerInstance.IsTaskComplete(ctx, migration.Status.TaskRef)
			if err != nil {
				logger.Error(err, "Failed to check export task status")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			if !done {
				logger.Info("Export task still running", "task_id", migration.Status.TaskRef)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			// Task complete, check status
			taskStatus, err := providerInstance.TaskStatus(ctx, migration.Status.TaskRef)
			if err != nil {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get export task status: %v", err))
			}

			if taskStatus.Error != "" {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf("Export failed: %s", taskStatus.Error))
			}

			logger.Info("Export task completed successfully")
		}

		// Export complete, transition to importing (skip transfer phase for direct export)
		migration.Status.Phase = infrav1beta1.MigrationPhaseImporting
		migration.Status.Message = "Disk exported successfully"
		migration.Status.TaskRef = ""

		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionExporting,
			metav1.ConditionTrue, "ExportComplete",
			"Source VM disk exported")

		r.Recorder.Event(migration, "Normal", "ExportComplete", "Disk exported successfully")

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// Generate destination URL for export
	destinationURL, err := r.generateStorageURL(ctx, migration, "export")
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to generate storage URL: %v", err))
	}

	// Determine disk format
	diskFormat := "qcow2" // Default for Libvirt/Proxmox
	if migration.Spec.Options != nil && migration.Spec.Options.DiskFormat != "" {
		diskFormat = migration.Spec.Options.DiskFormat
	}

	// Build export request
	exportReq := contracts.ExportDiskRequest{
		VmId:           sourceVM.Status.ID,
		DiskId:         "", // Empty means export primary disk
		SnapshotId:     migration.Status.SnapshotID,
		DestinationURL: destinationURL,
		Format:         diskFormat,
		Compress:       migration.Spec.Options != nil && migration.Spec.Options.Compress,
		Credentials:    make(map[string]string),
	}

	// TODO: Load credentials from storage secret if configured

	logger.Info("Starting disk export", "vm_id", sourceVM.Status.ID, "destination", destinationURL)
	exportResp, err := providerInstance.ExportDisk(ctx, exportReq)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to start disk export: %v", err))
	}

	// Store export info in status
	migration.Status.ExportID = exportResp.ExportId
	migration.Status.Message = fmt.Sprintf("Exporting disk (estimated size: %d bytes)", exportResp.EstimatedSizeBytes)

	// Initialize disk info in status
	if migration.Status.DiskInfo == nil {
		migration.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{}
	}
	migration.Status.DiskInfo.SourceDiskID = exportReq.DiskId
	migration.Status.DiskInfo.SourceFormat = diskFormat
	migration.Status.DiskInfo.SourceChecksum = exportResp.Checksum

	// If there's a task, we need to wait for it
	if exportResp.TaskRef != "" {
		migration.Status.TaskRef = exportResp.TaskRef
		logger.Info("Export task started", "task_id", exportResp.TaskRef, "export_id", exportResp.ExportId)

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Export completed synchronously, transition to importing
	migration.Status.Phase = infrav1beta1.MigrationPhaseImporting
	migration.Status.Message = "Disk exported successfully"
	migration.Status.TaskRef = ""

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionExporting,
		metav1.ConditionTrue, "ExportComplete",
		"Source VM disk exported")

	r.Recorder.Event(migration, "Normal", "ExportComplete", fmt.Sprintf("Disk exported to %s", destinationURL))

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleTransferringPhase transfers the disk to intermediate storage
func (r *VMMigrationReconciler) handleTransferringPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling transferring phase")

	// TODO: Implement disk transfer
	// For now, transition to converting/importing phase
	// Skip conversion for MVP (qcow2 -> qcow2)
	migration.Status.Phase = infrav1beta1.MigrationPhaseImporting
	migration.Status.Message = "Transfer complete, starting import"

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionTransferring,
		metav1.ConditionTrue, "TransferComplete",
		"Disk transfer completed")

	r.Recorder.Event(migration, "Normal", "TransferComplete", "Disk transfer completed")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleConvertingPhase converts disk format if needed
func (r *VMMigrationReconciler) handleConvertingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling converting phase")

	// TODO: Implement disk format conversion
	// For MVP, we skip conversion (qcow2 -> qcow2 only)
	migration.Status.Phase = infrav1beta1.MigrationPhaseImporting
	migration.Status.Message = "Conversion complete, starting import"

	r.Recorder.Event(migration, "Normal", "ConversionComplete", "Disk format conversion completed")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleImportingPhase imports the disk to target provider
func (r *VMMigrationReconciler) handleImportingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling importing phase")

	// Get target provider
	targetProvider, err := r.getProvider(ctx, migration.Spec.Target.ProviderRef, migration.Namespace)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get target provider: %v", err))
	}

	// Get provider instance
	providerInstance, err := r.getProviderInstance(ctx, targetProvider)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get target provider instance: %v", err))
	}

	// Check if import is already in progress
	if migration.Status.ImportID != "" {
		// Check import task status if there is one
		if migration.Status.TaskRef != "" {
			done, err := providerInstance.IsTaskComplete(ctx, migration.Status.TaskRef)
			if err != nil {
				logger.Error(err, "Failed to check import task status")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			if !done {
				logger.Info("Import task still running", "task_id", migration.Status.TaskRef)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			// Task complete, check status
			taskStatus, err := providerInstance.TaskStatus(ctx, migration.Status.TaskRef)
			if err != nil {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get import task status: %v", err))
			}

			if taskStatus.Error != "" {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf("Import failed: %s", taskStatus.Error))
			}

			logger.Info("Import task completed successfully")
		}

		// Import complete, transition to creating
		migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
		migration.Status.Message = "Disk imported, creating target VM"
		migration.Status.TaskRef = ""

		k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionImporting,
			metav1.ConditionTrue, "ImportComplete",
			"Disk imported to target provider")

		r.Recorder.Event(migration, "Normal", "ImportComplete", "Disk imported successfully")

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// Generate source URL (same as export destination)
	sourceURL, err := r.generateStorageURL(ctx, migration, "export")
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to generate source URL: %v", err))
	}

	// Determine disk format
	diskFormat := "qcow2" // Default
	if migration.Spec.Options != nil && migration.Spec.Options.DiskFormat != "" {
		diskFormat = migration.Spec.Options.DiskFormat
	}

	// Build import request
	importReq := contracts.ImportDiskRequest{
		SourceURL:        sourceURL,
		StorageHint:      "", // Let provider choose
		Format:           diskFormat,
		TargetName:       fmt.Sprintf("%s-migrated", migration.Spec.Target.Name),
		VerifyChecksum:   migration.Spec.Options == nil || migration.Spec.Options.VerifyChecksums,
		ExpectedChecksum: "",
		Credentials:      make(map[string]string),
	}

	// Set expected checksum if available
	if migration.Status.DiskInfo != nil && migration.Status.DiskInfo.SourceChecksum != "" {
		importReq.ExpectedChecksum = migration.Status.DiskInfo.SourceChecksum
	}

	// TODO: Load credentials from storage secret if configured

	logger.Info("Starting disk import", "source", sourceURL, "target_name", importReq.TargetName)
	importResp, err := providerInstance.ImportDisk(ctx, importReq)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to start disk import: %v", err))
	}

	// Store import info in status
	migration.Status.ImportID = importResp.DiskId
	migration.Status.Message = fmt.Sprintf("Importing disk (size: %d bytes)", importResp.ActualSizeBytes)

	// Update disk info in status
	if migration.Status.DiskInfo == nil {
		migration.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{}
	}
	migration.Status.DiskInfo.TargetDiskID = importResp.DiskId
	migration.Status.DiskInfo.TargetFormat = diskFormat
	migration.Status.DiskInfo.TargetChecksum = importResp.Checksum

	// If there's a task, we need to wait for it
	if importResp.TaskRef != "" {
		migration.Status.TaskRef = importResp.TaskRef
		logger.Info("Import task started", "task_id", importResp.TaskRef, "import_id", importResp.DiskId)

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Import completed synchronously, transition to creating
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.Message = "Disk imported, creating target VM"
	migration.Status.TaskRef = ""

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionImporting,
		metav1.ConditionTrue, "ImportComplete",
		"Disk imported to target provider")

	r.Recorder.Event(migration, "Normal", "ImportComplete", fmt.Sprintf("Disk imported as %s", importResp.DiskId))

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleCreatingPhase creates the target VM
func (r *VMMigrationReconciler) handleCreatingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling creating phase")

	// Check if target VM already exists
	targetVMName := migration.Spec.Target.Name
	if targetVMName == "" {
		targetVMName = fmt.Sprintf("%s-migrated", migration.Spec.Source.VMRef.Name)
	}

	targetNamespace := migration.Spec.Target.Namespace
	if targetNamespace == "" {
		targetNamespace = migration.Namespace
	}

	// Check if VM CR already exists
	existingVM := &infrav1beta1.VirtualMachine{}
	vmKey := client.ObjectKey{
		Namespace: targetNamespace,
		Name:      targetVMName,
	}

	err := r.Get(ctx, vmKey, existingVM)
	if err == nil {
		// VM already exists, check if it's ready
		if existingVM.Status.ID != "" && existingVM.Status.Phase == "Ready" {
			// VM is ready, store reference and transition to validation
			migration.Status.TargetVMID = existingVM.Status.ID
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidatingTarget
			migration.Status.Message = "Target VM ready, validating"

			if err := r.updateStatus(ctx, migration); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{Requeue: true}, nil
		}

		// VM exists but not ready yet, wait
		logger.Info("Target VM exists but not ready, waiting", "vm", targetVMName, "phase", existingVM.Status.Phase)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if client.IgnoreNotFound(err) != nil {
		// Error other than not found
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to check target VM: %v", err))
	}

	// VM doesn't exist, create it
	logger.Info("Creating target VM", "name", targetVMName)

	targetVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetVMName,
			Namespace: targetNamespace,
			Labels:    migration.Spec.Target.Labels,
			Annotations: map[string]string{
				"virtrigaud.io/migrated-from": fmt.Sprintf("%s/%s", migration.Namespace, migration.Spec.Source.VMRef.Name),
				"virtrigaud.io/migration":     fmt.Sprintf("%s/%s", migration.Namespace, migration.Name),
			},
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: migration.Spec.Target.ProviderRef,
		},
	}

	// Merge user-provided annotations
	if migration.Spec.Target.Annotations != nil {
		for k, v := range migration.Spec.Target.Annotations {
			targetVM.Annotations[k] = v
		}
	}

	// Set class ref if provided
	if migration.Spec.Target.ClassRef != nil {
		targetVM.Spec.ClassRef = infrav1beta1.ObjectRef{
			Name:      migration.Spec.Target.ClassRef.Name,
			Namespace: migration.Namespace,
		}
	}

	// Set networks if provided
	if len(migration.Spec.Target.Networks) > 0 {
		targetVM.Spec.Networks = migration.Spec.Target.Networks
	}

	// TODO: Set disks - need to reference the imported disk
	// For now, we'll let the provider handle disk attachment based on imported disk ID

	// Set placement if provided
	if migration.Spec.Target.PlacementRef != nil {
		targetVM.Spec.PlacementRef = migration.Spec.Target.PlacementRef
	}

	// Create the VM resource
	if err := r.Create(ctx, targetVM); err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to create target VM: %v", err))
	}

	logger.Info("Target VM created", "name", targetVMName)
	r.Recorder.Event(migration, "Normal", "TargetVMCreated", fmt.Sprintf("Created target VM %s", targetVMName))

	migration.Status.Message = fmt.Sprintf("Target VM %s created, waiting for provisioning", targetVMName)

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to check VM status
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// handleValidatingTargetPhase validates the migrated VM
func (r *VMMigrationReconciler) handleValidatingTargetPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling target validation phase")

	// Get target VM name
	targetVMName := migration.Spec.Target.Name
	if targetVMName == "" {
		targetVMName = fmt.Sprintf("%s-migrated", migration.Spec.Source.VMRef.Name)
	}

	targetNamespace := migration.Spec.Target.Namespace
	if targetNamespace == "" {
		targetNamespace = migration.Namespace
	}

	// Get target VM
	targetVM := &infrav1beta1.VirtualMachine{}
	vmKey := client.ObjectKey{
		Namespace: targetNamespace,
		Name:      targetVMName,
	}

	if err := r.Get(ctx, vmKey, targetVM); err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get target VM: %v", err))
	}

	// Verify VM is ready
	if targetVM.Status.Phase != "Ready" {
		logger.Info("Target VM not ready yet", "phase", targetVM.Status.Phase)
		migration.Status.Message = fmt.Sprintf("Waiting for target VM to be ready (current: %s)", targetVM.Status.Phase)

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// VM is ready, mark migration as complete
	migration.Status.TargetVMID = targetVM.Status.ID
	migration.Status.Phase = infrav1beta1.MigrationPhaseReady
	migration.Status.Message = "Migration completed successfully"
	migration.Status.CompletionTime = &metav1.Time{Time: time.Now()}

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady,
		metav1.ConditionTrue, "MigrationComplete",
		"VM migration completed successfully")

	r.Recorder.Event(migration, "Normal", "MigrationComplete", fmt.Sprintf("VM migrated successfully to %s/%s", targetNamespace, targetVMName))

	logger.Info("Migration completed successfully", "target_vm", fmt.Sprintf("%s/%s", targetNamespace, targetVMName))

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleReadyPhase handles migrations that are complete
func (r *VMMigrationReconciler) handleReadyPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Migration is ready")

	// Check if cleanup has already been performed
	if migration.Status.StorageInfo != nil && migration.Status.StorageInfo.CleanedUp {
		return ctrl.Result{}, nil
	}

	// Perform post-migration cleanup
	cleanupPerformed := false

	// 1. Cleanup intermediate storage
	if migration.Spec.Storage != nil {
		if err := r.cleanupIntermediateStorage(ctx, migration); err != nil {
			logger.Error(err, "Failed to cleanup intermediate storage, will retry")
			// Don't fail the migration, just log and retry
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		cleanupPerformed = true
		logger.Info("Intermediate storage cleaned up")
	}

	// 2. Delete source snapshot if it was created for migration
	if migration.Status.SnapshotID != "" && migration.Spec.Source.SnapshotRef == nil {
		// Only delete if we created the snapshot (not if user provided one)
		if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
			logger.Error(err, "Failed to delete source snapshot, will retry")
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		cleanupPerformed = true
		logger.Info("Source snapshot deleted", "snapshot_id", migration.Status.SnapshotID)
	}

	// 3. Delete source VM if requested
	if migration.Spec.Source.DeleteAfterMigration {
		if err := r.deleteSourceVM(ctx, migration); err != nil {
			logger.Error(err, "Failed to delete source VM, will retry")
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		cleanupPerformed = true
		logger.Info("Source VM deleted")
	}

	// Mark cleanup as complete
	if cleanupPerformed {
		if migration.Status.StorageInfo == nil {
			migration.Status.StorageInfo = &infrav1beta1.MigrationStorageInfo{}
		}
		migration.Status.StorageInfo.CleanedUp = true
		migration.Status.Message = "Migration completed, cleanup finished"

		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}

		r.Recorder.Event(migration, "Normal", "CleanupComplete", "Post-migration cleanup completed")
	}

	return ctrl.Result{}, nil
}

// handleFailedPhase handles migrations that have failed
func (r *VMMigrationReconciler) handleFailedPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Migration is in failed state", "message", migration.Status.Message)

	// Check if retry is configured and allowed
	if migration.Spec.Options == nil || migration.Spec.Options.RetryPolicy == nil {
		logger.Info("No retry policy configured, migration remains failed")
		return ctrl.Result{}, nil
	}

	retryPolicy := migration.Spec.Options.RetryPolicy
	maxRetries := int32(3) // Default
	if retryPolicy.MaxRetries != nil {
		maxRetries = *retryPolicy.MaxRetries
	}

	// Check if we've exceeded max retries
	if migration.Status.RetryCount >= maxRetries {
		logger.Info("Max retries exceeded, migration remains failed",
			"retries", migration.Status.RetryCount,
			"max_retries", maxRetries)
		migration.Status.Message = fmt.Sprintf("Migration failed after %d retries: %s",
			migration.Status.RetryCount, migration.Status.Message)
		if err := r.updateStatus(ctx, migration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Calculate backoff delay
	baseDelay := 5 * time.Minute // Default delay
	if retryPolicy.RetryDelay != nil {
		baseDelay = retryPolicy.RetryDelay.Duration
	}

	backoffMultiplier := int32(2) // Default multiplier
	if retryPolicy.BackoffMultiplier != nil {
		backoffMultiplier = *retryPolicy.BackoffMultiplier
	}

	// Exponential backoff: delay = baseDelay * multiplier^retryCount (capped at 30 minutes)
	retryDelay := baseDelay * time.Duration(1)
	for i := int32(0); i < migration.Status.RetryCount; i++ {
		retryDelay *= time.Duration(backoffMultiplier)
	}

	maxDelay := 30 * time.Minute
	if retryDelay > maxDelay {
		retryDelay = maxDelay
	}

	// Check if enough time has passed since last retry
	now := time.Now()
	if migration.Status.LastRetryTime != nil {
		timeSinceLastRetry := now.Sub(migration.Status.LastRetryTime.Time)
		if timeSinceLastRetry < retryDelay {
			// Not enough time has passed, requeue
			remainingWait := retryDelay - timeSinceLastRetry
			logger.Info("Waiting before retry", "remaining_wait", remainingWait)
			return ctrl.Result{RequeueAfter: remainingWait}, nil
		}
	}

	// Perform retry
	logger.Info("Retrying migration",
		"retry_count", migration.Status.RetryCount+1,
		"max_retries", maxRetries,
		"retry_delay", retryDelay)

	// Increment retry counter
	migration.Status.RetryCount++
	migration.Status.LastRetryTime = &metav1.Time{Time: now}

	// Reset to appropriate phase based on where we failed
	// For simplicity, restart from validation phase
	migration.Status.Phase = infrav1beta1.MigrationPhasePending
	migration.Status.Message = fmt.Sprintf("Retrying migration (attempt %d/%d)",
		migration.Status.RetryCount, maxRetries)

	// Clear task references
	migration.Status.TaskRef = ""

	// Update conditions
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionFailed,
		metav1.ConditionFalse, "Retrying",
		fmt.Sprintf("Retrying migration (attempt %d/%d)", migration.Status.RetryCount, maxRetries))

	r.Recorder.Event(migration, "Normal", "RetryingMigration",
		fmt.Sprintf("Retrying migration (attempt %d/%d)", migration.Status.RetryCount, maxRetries))

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleDeletion handles deletion of VMMigration resources
func (r *VMMigrationReconciler) handleDeletion(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling VMMigration deletion")

	// Perform cleanup operations
	cleanupErrors := []error{}

	// 1. Cleanup intermediate storage
	if migration.Spec.Storage != nil && (migration.Status.StorageInfo == nil || !migration.Status.StorageInfo.CleanedUp) {
		if err := r.cleanupIntermediateStorage(ctx, migration); err != nil {
			logger.Error(err, "Failed to cleanup intermediate storage during deletion")
			cleanupErrors = append(cleanupErrors, fmt.Errorf("storage cleanup: %w", err))
		} else {
			logger.Info("Intermediate storage cleaned up during deletion")
		}
	}

	// 2. Delete migration-created snapshot if exists
	if migration.Status.SnapshotID != "" && migration.Spec.Source.SnapshotRef == nil {
		if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
			logger.Error(err, "Failed to delete source snapshot during deletion")
			cleanupErrors = append(cleanupErrors, fmt.Errorf("snapshot cleanup: %w", err))
		} else {
			logger.Info("Source snapshot cleaned up during deletion")
		}
	}

	// 3. Delete partially created target VM if migration failed
	if migration.Status.Phase == infrav1beta1.MigrationPhaseFailed || migration.Status.Phase == infrav1beta1.MigrationPhaseCreating {
		targetVMName := migration.Spec.Target.Name
		if targetVMName == "" {
			targetVMName = fmt.Sprintf("%s-migrated", migration.Spec.Source.VMRef.Name)
		}
		targetNamespace := migration.Spec.Target.Namespace
		if targetNamespace == "" {
			targetNamespace = migration.Namespace
		}

		targetVM := &infrav1beta1.VirtualMachine{}
		vmKey := client.ObjectKey{
			Namespace: targetNamespace,
			Name:      targetVMName,
		}

		// Check if target VM exists
		if err := r.Get(ctx, vmKey, targetVM); err == nil {
			// Only delete if it has our migration annotation
			if targetVM.Annotations != nil {
				if migrationRef, ok := targetVM.Annotations["virtrigaud.io/migration"]; ok {
					expectedRef := fmt.Sprintf("%s/%s", migration.Namespace, migration.Name)
					if migrationRef == expectedRef {
						logger.Info("Deleting partially created target VM", "vm", targetVMName)
						if err := r.Delete(ctx, targetVM); err != nil {
							logger.Error(err, "Failed to delete target VM during cleanup")
							cleanupErrors = append(cleanupErrors, fmt.Errorf("target VM cleanup: %w", err))
						}
					}
				}
			}
		}
	}

	// If there were errors, log them but continue with finalizer removal
	if len(cleanupErrors) > 0 {
		logger.Info("Cleanup completed with errors", "error_count", len(cleanupErrors))
		for i, err := range cleanupErrors {
			logger.Error(err, "Cleanup error", "index", i)
		}
		r.Recorder.Event(migration, "Warning", "CleanupErrors",
			fmt.Sprintf("Cleanup completed with %d errors", len(cleanupErrors)))
	}

	// Remove finalizer
	const finalizerName = "vmmigration.infra.virtrigaud.io/finalizer"
	controllerutil.RemoveFinalizer(migration, finalizerName)
	if err := r.Update(ctx, migration); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	logger.Info("VMMigration deleted successfully")
	return ctrl.Result{}, nil
}

// transitionToFailed transitions migration to failed state
func (r *VMMigrationReconciler) transitionToFailed(ctx context.Context, migration *infrav1beta1.VMMigration, message string) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Error(nil, "Migration failed", "message", message)

	migration.Status.Phase = infrav1beta1.MigrationPhaseFailed
	migration.Status.Message = message
	migration.Status.CompletionTime = &metav1.Time{Time: time.Now()}

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionFailed,
		metav1.ConditionTrue, "MigrationFailed",
		message)
	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady,
		metav1.ConditionFalse, "MigrationFailed",
		message)

	r.Recorder.Event(migration, "Warning", "MigrationFailed", message)

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// Helper functions

// getSourceVM retrieves the source VM
func (r *VMMigrationReconciler) getSourceVM(ctx context.Context, migration *infrav1beta1.VMMigration) (*infrav1beta1.VirtualMachine, error) {
	vm := &infrav1beta1.VirtualMachine{}
	key := client.ObjectKey{
		Namespace: migration.Namespace,
		Name:      migration.Spec.Source.VMRef.Name,
	}

	if err := r.Get(ctx, key, vm); err != nil {
		return nil, err
	}

	return vm, nil
}

// getProvider retrieves a provider
func (r *VMMigrationReconciler) getProvider(ctx context.Context, providerRef infrav1beta1.ObjectRef, defaultNamespace string) (*infrav1beta1.Provider, error) {
	provider := &infrav1beta1.Provider{}
	namespace := providerRef.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	key := client.ObjectKey{
		Namespace: namespace,
		Name:      providerRef.Name,
	}

	if err := r.Get(ctx, key, provider); err != nil {
		return nil, err
	}

	return provider, nil
}

// isProviderReady checks if a provider is ready
func (r *VMMigrationReconciler) isProviderReady(provider *infrav1beta1.Provider) bool {
	// Check if provider has the ProviderAvailable condition set to True
	// The Provider CRD uses "ProviderAvailable" and "ProviderRuntimeReady" conditions
	for _, condition := range provider.Status.Conditions {
		if condition.Type == "ProviderAvailable" && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// validateStorageConfig validates the storage configuration
func (r *VMMigrationReconciler) validateStorageConfig(ctx context.Context, storageConfig *infrav1beta1.MigrationStorage) error {
	if storageConfig == nil {
		return fmt.Errorf("storage configuration is required")
	}

	// Validate storage type
	if storageConfig.Type != "pvc" && storageConfig.Type != "" {
		return fmt.Errorf("unsupported storage type: %s (only 'pvc' is supported)", storageConfig.Type)
	}

	// Validate PVC configuration
	if storageConfig.PVC == nil {
		return fmt.Errorf("pvc configuration is required when using pvc storage type")
	}

	pvcConfig := storageConfig.PVC

	// If using existing PVC, validate it exists
	if pvcConfig.Name != "" {
		// PVC name specified - it must exist
		// We'll verify this in the actual migration phases
		return nil
	}

	// Auto-create PVC validation
	if pvcConfig.StorageClassName == "" {
		return fmt.Errorf("storageClassName is required when PVC name is not specified (auto-create mode)")
	}

	if pvcConfig.Size == "" {
		return fmt.Errorf("size is required when PVC name is not specified (auto-create mode)")
	}

	// Validate access mode
	if pvcConfig.AccessMode != "" {
		validModes := map[string]bool{
			"ReadWriteOnce": true,
			"ReadWriteMany": true,
			"ReadOnlyMany":  true,
		}
		if !validModes[pvcConfig.AccessMode] {
			return fmt.Errorf("invalid access mode: %s", pvcConfig.AccessMode)
		}
	}

	return nil
}

// ensureMigrationPVC ensures the PVC for migration storage exists
// Returns the PVC name and any error
func (r *VMMigrationReconciler) ensureMigrationPVC(ctx context.Context, migration *infrav1beta1.VMMigration) (string, error) {
	logger := logging.FromContext(ctx)

	if migration.Spec.Storage == nil || migration.Spec.Storage.PVC == nil {
		return "", fmt.Errorf("storage PVC configuration is missing")
	}

	pvcConfig := migration.Spec.Storage.PVC

	// If PVC name specified, verify it exists
	if pvcConfig.Name != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      pvcConfig.Name,
			Namespace: migration.Namespace,
		}, pvc)
		if err != nil {
			if errors.IsNotFound(err) {
				return "", fmt.Errorf("specified PVC %s not found in namespace %s", pvcConfig.Name, migration.Namespace)
			}
			return "", fmt.Errorf("failed to get PVC %s: %w", pvcConfig.Name, err)
		}
		logger.Info("Using existing PVC for migration", "pvc", pvcConfig.Name)
		return pvcConfig.Name, nil
	}

	// Auto-create PVC
	pvcName := fmt.Sprintf("%s-storage", migration.Name)

	// Check if PVC already exists
	existingPVC := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      pvcName,
		Namespace: migration.Namespace,
	}, existingPVC)

	if err == nil {
		// PVC already exists
		logger.Info("Migration PVC already exists", "pvc", pvcName)
		return pvcName, nil
	}

	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("failed to check for existing PVC: %w", err)
	}

	// Create new PVC
	logger.Info("Creating migration PVC", "pvc", pvcName, "storageClass", pvcConfig.StorageClassName, "size", pvcConfig.Size)

	// Set default access mode if not specified
	accessMode := corev1.ReadWriteMany
	if pvcConfig.AccessMode != "" {
		accessMode = corev1.PersistentVolumeAccessMode(pvcConfig.AccessMode)
	}

	// Parse size
	quantity, err := resource.ParseQuantity(pvcConfig.Size)
	if err != nil {
		return "", fmt.Errorf("invalid PVC size %s: %w", pvcConfig.Size, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: migration.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "virtrigaud",
				"virtrigaud.io/migration":      migration.Name,
				"virtrigaud.io/component":      "migration-storage",
			},
			Annotations: map[string]string{
				"virtrigaud.io/created-by": "vmmigration-controller",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &pvcConfig.StorageClassName,
			AccessModes: []corev1.PersistentVolumeAccessMode{
				accessMode,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: quantity,
				},
			},
		},
	}

	// Set owner reference so PVC is cleaned up when migration is deleted
	if err := controllerutil.SetControllerReference(migration, pvc, r.Scheme); err != nil {
		return "", fmt.Errorf("failed to set owner reference on PVC: %w", err)
	}

	if err := r.Create(ctx, pvc); err != nil {
		return "", fmt.Errorf("failed to create PVC: %w", err)
	}

	logger.Info("Successfully created migration PVC", "pvc", pvcName)
	r.Recorder.Event(migration, "Normal", "PVCCreated", fmt.Sprintf("Created migration storage PVC: %s", pvcName))

	return pvcName, nil
}

// getProviderInstance retrieves a provider gRPC client
func (r *VMMigrationReconciler) getProviderInstance(ctx context.Context, provider *infrav1beta1.Provider) (contracts.Provider, error) {
	// Resolve the provider client
	providerClient, err := r.RemoteResolver.GetProvider(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve provider: %w", err)
	}

	return providerClient, nil
}

// updateStatus updates the migration status
func (r *VMMigrationReconciler) updateStatus(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	if err := r.Status().Update(ctx, migration); err != nil {
		logger := logging.FromContext(ctx)
		logger.Error(err, "Failed to update VMMigration status")
		return err
	}
	return nil
}

// generateStorageURL generates a storage URL for the migration
func (r *VMMigrationReconciler) generateStorageURL(ctx context.Context, migration *infrav1beta1.VMMigration, stage string) (string, error) {
	// If no storage is configured, return an error
	if migration.Spec.Storage == nil {
		return "", fmt.Errorf("storage configuration is required for migration")
	}

	storageConfig := migration.Spec.Storage

	// Generate a unique path for this migration
	migrationPath := fmt.Sprintf("vmmigrations/%s/%s/%s.qcow2",
		migration.Namespace,
		migration.Name,
		stage)

	// Build URL based on storage type
	switch storageConfig.Type {
	case "pvc", "":
		// Get the PVC name from status (set during validation phase)
		pvcName := migration.Status.StoragePVCName
		if pvcName == "" {
			return "", fmt.Errorf("storage PVC name not set in migration status")
		}

		// PVC URL format: pvc://<pvc-name>/<path>
		// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
		return fmt.Sprintf("pvc://%s/%s", pvcName, migrationPath), nil

	default:
		return "", fmt.Errorf("unsupported storage type: %s", storageConfig.Type)
	}
}

// cleanupIntermediateStorage removes temporary disk files from intermediate storage
func (r *VMMigrationReconciler) cleanupIntermediateStorage(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	logger := logging.FromContext(ctx)

	if migration.Spec.Storage == nil {
		return nil
	}

	// Create storage client
	// Determine PVC mount path based on the PVC name
	pvcName := migration.Status.StoragePVCName
	if pvcName == "" && migration.Spec.Storage.PVC != nil && migration.Spec.Storage.PVC.Name != "" {
		pvcName = migration.Spec.Storage.PVC.Name
	}

	// Set mount path to match provider controller's mount location
	mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvcName)
	if migration.Spec.Storage.PVC != nil && migration.Spec.Storage.PVC.MountPath != "" {
		mountPath = migration.Spec.Storage.PVC.MountPath
	}

	storageConfig := storage.StorageConfig{
		Type:         "pvc",
		PVCName:      pvcName,
		PVCNamespace: migration.Namespace,
		MountPath:    mountPath,
	}

	store, err := storage.NewStorage(storageConfig)
	if err != nil {
		return fmt.Errorf("failed to create storage client: %w", err)
	}
	defer store.Close()

	// Delete export file
	exportURL, err := r.generateStorageURL(ctx, migration, "export")
	if err != nil {
		logger.Error(err, "Failed to generate export URL for cleanup")
	} else {
		if err := store.Delete(ctx, exportURL); err != nil {
			logger.Error(err, "Failed to delete export file", "url", exportURL)
			// Continue with other cleanup even if this fails
		} else {
			logger.Info("Deleted export file", "url", exportURL)
		}
	}

	return nil
}

// deleteSourceSnapshot deletes the migration-created snapshot
func (r *VMMigrationReconciler) deleteSourceSnapshot(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	logger := logging.FromContext(ctx)

	// Get source VM
	sourceVM, err := r.getSourceVM(ctx, migration)
	if err != nil {
		return fmt.Errorf("failed to get source VM: %w", err)
	}

	// Get source provider
	var sourceProviderRef infrav1beta1.ObjectRef
	if migration.Spec.Source.ProviderRef != nil {
		sourceProviderRef = *migration.Spec.Source.ProviderRef
	} else {
		sourceProviderRef = sourceVM.Spec.ProviderRef
	}

	sourceProvider, err := r.getProvider(ctx, sourceProviderRef, migration.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get source provider: %w", err)
	}

	// Get provider instance
	providerInstance, err := r.getProviderInstance(ctx, sourceProvider)
	if err != nil {
		return fmt.Errorf("failed to get provider instance: %w", err)
	}

	// Delete snapshot
	logger.Info("Deleting source snapshot", "snapshot_id", migration.Status.SnapshotID)
	taskRef, err := providerInstance.SnapshotDelete(ctx, sourceVM.Status.ID, migration.Status.SnapshotID)
	if err != nil {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}

	// If there's a task, wait for it to complete
	if taskRef != "" {
		logger.Info("Snapshot deletion task started, waiting for completion", "task_id", taskRef)
		// For cleanup, we'll do best effort - don't wait indefinitely
		// The task will complete asynchronously
	}

	return nil
}

// deleteSourceVM deletes the source VM after successful migration
func (r *VMMigrationReconciler) deleteSourceVM(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	logger := logging.FromContext(ctx)

	// Get source VM
	sourceVM, err := r.getSourceVM(ctx, migration)
	if err != nil {
		return fmt.Errorf("failed to get source VM: %w", err)
	}

	logger.Info("Deleting source VM", "vm", fmt.Sprintf("%s/%s", sourceVM.Namespace, sourceVM.Name))

	// Delete the VM resource
	if err := r.Delete(ctx, sourceVM); err != nil {
		return fmt.Errorf("failed to delete source VM: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *VMMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1beta1.VMMigration{}).
		Complete(r)
}
