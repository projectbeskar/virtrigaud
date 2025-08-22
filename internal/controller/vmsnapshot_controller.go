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

	infrav1alpha1 "github.com/projectbeskar/virtrigaud/api/v1alpha1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/registry"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// VMSnapshotReconciler reconciles a VMSnapshot object
type VMSnapshotReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	ProviderRegistry *registry.Registry
	RemoteResolver   *remote.Resolver
	Recorder         record.EventRecorder
	metrics          *metrics.ReconcileMetrics
}

// NewVMSnapshotReconciler creates a new VMSnapshot reconciler
func NewVMSnapshotReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	providerRegistry *registry.Registry,
	remoteResolver *remote.Resolver,
	recorder record.EventRecorder,
) *VMSnapshotReconciler {
	return &VMSnapshotReconciler{
		Client:           client,
		Scheme:           scheme,
		ProviderRegistry: providerRegistry,
		RemoteResolver:   remoteResolver,
		Recorder:         recorder,
		metrics:          metrics.NewReconcileMetrics("VMSnapshot"),
	}
}

//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsnapshots,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsnapshots/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsnapshots/finalizers,verbs=update
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop
func (r *VMSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	timer := metrics.NewReconcileTimer("VMSnapshot")
	defer timer.Finish(metrics.OutcomeSuccess)

	// Add correlation context
	ctx = logging.WithCorrelationID(ctx, fmt.Sprintf("vmsnapshot-%s", req.NamespacedName.Name))
	logger := logging.FromContext(ctx)

	logger.Info("Reconciling VMSnapshot", "snapshot", req.NamespacedName)

	// Fetch the VMSnapshot instance
	snapshot := &infrav1alpha1.VMSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("VMSnapshot not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get VMSnapshot")
		timer.Finish(metrics.OutcomeError)
		return ctrl.Result{}, err
	}

	// Add snapshot context
	ctx = logging.WithCorrelationID(ctx, fmt.Sprintf("vmsnapshot-%s/%s", snapshot.Namespace, snapshot.Name))
	logger = logging.FromContext(ctx)

	// Handle deletion
	if !snapshot.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, snapshot)
	}

	// Add finalizer if needed
	if !controllerutil.ContainsFinalizer(snapshot, "snapshot.infra.virtrigaud.io/finalizer") {
		controllerutil.AddFinalizer(snapshot, "snapshot.infra.virtrigaud.io/finalizer")
		if err := r.Update(ctx, snapshot); err != nil {
			logger.Error(err, "Failed to add finalizer")
			timer.Finish(metrics.OutcomeError)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Get the referenced VM
	vm := &infrav1alpha1.VirtualMachine{}
	vmKey := client.ObjectKey{
		Namespace: snapshot.Namespace,
		Name:      snapshot.Spec.VMRef.Name,
	}
	if err := r.Get(ctx, vmKey, vm); err != nil {
		logger.Error(err, "Failed to get referenced VM", "vm", snapshot.Spec.VMRef.Name)
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1alpha1.VMSnapshotConditionReady,
			metav1.ConditionFalse, infrav1alpha1.VMSnapshotReasonProviderError,
			fmt.Sprintf("Referenced VM not found: %v", err))
		r.updateStatus(ctx, snapshot)
		timer.Finish(metrics.OutcomeError)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Add VM context
	ctx = logging.WithVM(ctx, vm.Namespace, vm.Name)
	logger = logging.FromContext(ctx)

	// Check VM status
	if vm.Status.ID == "" {
		logger.Info("VM not yet provisioned, waiting")
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1alpha1.VMSnapshotConditionCreating,
			metav1.ConditionTrue, infrav1alpha1.VMSnapshotReasonCreating,
			"Waiting for VM to be provisioned")
		r.updateStatus(ctx, snapshot)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Handle snapshot lifecycle based on phase
	switch snapshot.Status.Phase {
	case "":
		// Initialize snapshot creation
		return r.createSnapshot(ctx, snapshot, vm)
	case infrav1alpha1.SnapshotPhaseCreating:
		// Check if snapshot creation is complete
		return r.checkSnapshotCreation(ctx, snapshot, vm)
	case infrav1alpha1.SnapshotPhaseReady:
		// Snapshot is ready, check for retention policy
		return r.handleRetention(ctx, snapshot)
	case infrav1alpha1.SnapshotPhaseFailed:
		// Handle failed snapshots
		logger.Info("Snapshot is in failed state", "message", snapshot.Status.Message)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	default:
		// Unknown phase
		logger.Info("Unknown snapshot phase", "phase", snapshot.Status.Phase)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// createSnapshot initiates snapshot creation
func (r *VMSnapshotReconciler) createSnapshot(ctx context.Context, snapshot *infrav1alpha1.VMSnapshot, vm *infrav1alpha1.VirtualMachine) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	logger.Info("Creating VM snapshot")

	// Update phase to creating
	snapshot.Status.Phase = infrav1alpha1.SnapshotPhaseCreating
	snapshot.Status.Message = "Creating snapshot"
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1alpha1.VMSnapshotConditionCreating,
		metav1.ConditionTrue, infrav1alpha1.VMSnapshotReasonCreating,
		"Snapshot creation initiated")

	// For demonstration purposes, simulate snapshot creation
	// In a real implementation, this would call the provider's SnapshotCreate RPC
	snapshotID := fmt.Sprintf("snapshot-%s-%d", snapshot.Spec.NameHint, time.Now().Unix())
	snapshot.Status.SnapshotID = snapshotID
	snapshot.Status.TaskRef = fmt.Sprintf("task-snapshot-%s", snapshotID)
	snapshot.Status.CreationTime = &metav1.Time{Time: time.Now()}

	r.Recorder.Event(snapshot, "Normal", "SnapshotCreating", "Started snapshot creation")

	if err := r.updateStatus(ctx, snapshot); err != nil {
		return ctrl.Result{}, err
	}

	// Simulate async operation - in real implementation, this would poll the task
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// checkSnapshotCreation checks if snapshot creation is complete
func (r *VMSnapshotReconciler) checkSnapshotCreation(ctx context.Context, snapshot *infrav1alpha1.VMSnapshot, vm *infrav1alpha1.VirtualMachine) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	logger.Info("Checking snapshot creation progress")

	// Simulate completion check
	// In real implementation, this would call TaskStatus RPC
	if time.Since(snapshot.Status.CreationTime.Time) > 30*time.Second {
		// Mark as ready
		snapshot.Status.Phase = infrav1alpha1.SnapshotPhaseReady
		snapshot.Status.Message = "Snapshot created successfully"
		snapshot.Status.SizeBytes = func() *int64 { size := int64(1024 * 1024 * 1024); return &size }() // 1GB
		snapshot.Status.TaskRef = ""

		k8s.SetCondition(&snapshot.Status.Conditions, infrav1alpha1.VMSnapshotConditionReady,
			metav1.ConditionTrue, infrav1alpha1.VMSnapshotReasonCreated,
			"Snapshot created successfully")
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1alpha1.VMSnapshotConditionCreating,
			metav1.ConditionFalse, infrav1alpha1.VMSnapshotReasonCreated,
			"Snapshot creation completed")

		r.Recorder.Event(snapshot, "Normal", "SnapshotReady", "Snapshot created successfully")

		if err := r.updateStatus(ctx, snapshot); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Still creating
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// handleRetention handles snapshot retention policies
func (r *VMSnapshotReconciler) handleRetention(ctx context.Context, snapshot *infrav1alpha1.VMSnapshot) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Check retention policy
	if snapshot.Spec.RetentionPolicy != nil && snapshot.Spec.RetentionPolicy.MaxAge != nil {
		maxAge := snapshot.Spec.RetentionPolicy.MaxAge.Duration
		if time.Since(snapshot.Status.CreationTime.Time) > maxAge {
			logger.Info("Snapshot has exceeded retention period, deleting")

			// Delete the snapshot
			if err := r.Delete(ctx, snapshot); err != nil {
				logger.Error(err, "Failed to delete expired snapshot")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
	}

	// Check retention based on VM deletion
	if snapshot.Spec.RetentionPolicy != nil && snapshot.Spec.RetentionPolicy.DeleteOnVMDelete {
		vm := &infrav1alpha1.VirtualMachine{}
		vmKey := client.ObjectKey{
			Namespace: snapshot.Namespace,
			Name:      snapshot.Spec.VMRef.Name,
		}
		if err := r.Get(ctx, vmKey, vm); err != nil {
			if client.IgnoreNotFound(err) == nil {
				logger.Info("Referenced VM deleted, deleting snapshot")
				if err := r.Delete(ctx, snapshot); err != nil {
					logger.Error(err, "Failed to delete snapshot after VM deletion")
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}
	}

	// Check again in an hour
	return ctrl.Result{RequeueAfter: time.Hour}, nil
}

// handleDeletion handles snapshot deletion
func (r *VMSnapshotReconciler) handleDeletion(ctx context.Context, snapshot *infrav1alpha1.VMSnapshot) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	logger.Info("Deleting VM snapshot")

	// Update phase
	snapshot.Status.Phase = infrav1alpha1.SnapshotPhaseDeleting
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1alpha1.VMSnapshotConditionDeleting,
		metav1.ConditionTrue, infrav1alpha1.VMSnapshotReasonDeleting,
		"Snapshot deletion initiated")

	r.updateStatus(ctx, snapshot)

	// In real implementation, this would call the provider's SnapshotDelete RPC
	r.Recorder.Event(snapshot, "Normal", "SnapshotDeleting", "Started snapshot deletion")

	// Simulate deletion time
	time.Sleep(2 * time.Second)

	// Remove finalizer
	controllerutil.RemoveFinalizer(snapshot, "snapshot.infra.virtrigaud.io/finalizer")
	if err := r.Update(ctx, snapshot); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	r.Recorder.Event(snapshot, "Normal", "SnapshotDeleted", "Snapshot deleted successfully")
	logger.Info("VM snapshot deleted successfully")

	return ctrl.Result{}, nil
}

// updateStatus updates the snapshot status
func (r *VMSnapshotReconciler) updateStatus(ctx context.Context, snapshot *infrav1alpha1.VMSnapshot) error {
	if err := r.Status().Update(ctx, snapshot); err != nil {
		logger := logging.FromContext(ctx)
		logger.Error(err, "Failed to update VMSnapshot status")
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *VMSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.VMSnapshot{}).
		Complete(r)
}
