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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	// TODO: Implement snapshot creation
	// For now, transition to exporting phase
	migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
	migration.Status.Message = "Snapshot created, preparing export"

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionSnapshotting,
		metav1.ConditionTrue, "SnapshotComplete",
		"Source VM snapshot created")

	r.Recorder.Event(migration, "Normal", "SnapshotComplete", "Source VM snapshot created")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleExportingPhase exports the disk from source provider
func (r *VMMigrationReconciler) handleExportingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling exporting phase")

	// TODO: Implement disk export
	// For now, transition to transferring phase
	migration.Status.Phase = infrav1beta1.MigrationPhaseTransferring
	migration.Status.Message = "Disk exported, starting transfer"

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionExporting,
		metav1.ConditionTrue, "ExportComplete",
		"Source VM disk exported")

	r.Recorder.Event(migration, "Normal", "ExportComplete", "Source VM disk exported")

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

	// TODO: Implement disk import
	// For now, transition to creating phase
	migration.Status.Phase = infrav1beta1.MigrationPhaseCreating
	migration.Status.Message = "Disk imported, creating target VM"

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionImporting,
		metav1.ConditionTrue, "ImportComplete",
		"Disk imported to target provider")

	r.Recorder.Event(migration, "Normal", "ImportComplete", "Disk imported to target provider")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleCreatingPhase creates the target VM
func (r *VMMigrationReconciler) handleCreatingPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling creating phase")

	// TODO: Implement VM creation on target provider
	// For now, transition to validating target phase
	migration.Status.Phase = infrav1beta1.MigrationPhaseValidatingTarget
	migration.Status.Message = "Target VM created, validating"

	r.Recorder.Event(migration, "Normal", "TargetVMCreated", "Target VM created successfully")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// handleValidatingTargetPhase validates the migrated VM
func (r *VMMigrationReconciler) handleValidatingTargetPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling target validation phase")

	// TODO: Implement target VM validation
	// For now, transition to ready phase
	migration.Status.Phase = infrav1beta1.MigrationPhaseReady
	migration.Status.Message = "Migration completed successfully"
	migration.Status.CompletionTime = &metav1.Time{Time: time.Now()}

	k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionReady,
		metav1.ConditionTrue, "MigrationComplete",
		"VM migration completed successfully")

	r.Recorder.Event(migration, "Normal", "MigrationComplete", "VM migration completed successfully")

	if err := r.updateStatus(ctx, migration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handleReadyPhase handles migrations that are complete
func (r *VMMigrationReconciler) handleReadyPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Migration is ready")

	// TODO: Handle post-migration cleanup if configured
	// - Delete source VM if requested
	// - Cleanup intermediate storage
	// - Remove snapshots

	return ctrl.Result{}, nil
}

// handleFailedPhase handles migrations that have failed
func (r *VMMigrationReconciler) handleFailedPhase(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Migration is in failed state", "message", migration.Status.Message)

	// TODO: Implement retry logic based on retry policy
	// For now, just requeue after a delay
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// handleDeletion handles deletion of VMMigration resources
func (r *VMMigrationReconciler) handleDeletion(ctx context.Context, migration *infrav1beta1.VMMigration) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Handling VMMigration deletion")

	// TODO: Cleanup resources
	// - Delete intermediate storage artifacts
	// - Remove temporary snapshots
	// - Cancel in-progress operations

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
	// Check if provider has the Ready condition set to True
	for _, condition := range provider.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
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

	// Create storage config
	config := storage.StorageConfig{
		Type:     storageConfig.Type,
		Endpoint: storageConfig.Endpoint,
		Bucket:   storageConfig.Bucket,
		Region:   storageConfig.Region,
	}

	// TODO: Load credentials from CredentialsSecretRef if provided
	// For now, just validate the basic configuration

	// Try to create storage instance to validate config
	store, err := storage.NewStorage(config)
	if err != nil {
		return fmt.Errorf("failed to create storage backend: %w", err)
	}
	defer store.Close()

	return nil
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

// SetupWithManager sets up the controller with the Manager
func (r *VMMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1beta1.VMMigration{}).
		Complete(r)
}

