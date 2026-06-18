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
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
	"github.com/projectbeskar/virtrigaud/internal/storage"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// Reason labels used in metrics.RecordError calls for the VMMigration
// reconciler. See virtualmachine_controller.go for the naming convention.
// G3 currently instruments only the top-level Reconcile entry sites;
// per-phase-handler instrumentation can be added in a follow-up PR.
const (
	errReasonGetMigration = "get-migration"
	// errReasonAddFinalizer is shared with the VirtualMachine reconciler
	// (declared in virtualmachine_controller.go); reuse that constant
	// for the same operational meaning rather than redeclaring it.
)

// VMMigrationReconciler reconciles a VMMigration object
type VMMigrationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RemoteResolver *remote.Resolver
	Recorder       record.EventRecorder
	metrics        *metrics.ReconcileMetrics

	// EnforceCapabilities, when true, gates the migration export/import
	// phases on the source/target providers' self-reported capabilities
	// (issue #176). When false (the default) the migration phases are
	// byte-for-byte unchanged. See gateDiskExport / gateDiskImport.
	EnforceCapabilities bool

	// longOpInFlight is an in-memory guard against re-issuing a long-running,
	// non-idempotent migration RPC (ExportDisk / ImportDisk) when a reconcile
	// re-enters before the prior status write has propagated to the informer
	// cache. It is keyed by longOpKey(migration, op) and holds a sentinel for
	// the duration of (and after) the RPC for a given object generation.
	//
	// This guards the cache-staleness race ONLY; controller-runtime already
	// serializes reconciles per object key. The guard lives in the manager
	// process and is intentionally NOT persisted: on a manager restart it is
	// lost, but by then the durable Status (ExportID/ImportID + the
	// Exporting/Importing condition) is internally consistent, so the
	// persisted-state guard below (condition == True) prevents a duplicate RPC.
	longOpInFlight sync.Map

	// providerInstanceFn, when non-nil, overrides provider-instance resolution.
	// Production leaves it nil and resolves a real gRPC client via
	// RemoteResolver; tests inject a fake/counting provider here to exercise
	// the export/import guards without standing up a gRPC server.
	providerInstanceFn func(ctx context.Context, provider *infrav1beta1.Provider) (contracts.Provider, error)
}

// migrationLongOp identifies a long-running, non-idempotent migration RPC for
// the purpose of the in-memory re-entrancy guard (longOpInFlight).
type migrationLongOp string

const (
	// longOpExport marks the synchronous ExportDisk RPC.
	longOpExport migrationLongOp = "export"
	// longOpImport marks the synchronous ImportDisk RPC.
	longOpImport migrationLongOp = "import"
)

// longOpKey builds the in-memory guard key for a migration's long-running RPC.
// It is scoped to the object UID and generation so that a legitimate retry of a
// *re-created* migration (new UID) or an intentionally re-driven spec (new
// generation) is not wrongly suppressed, while same-generation cache-stale
// re-entries are.
func longOpKey(migration *infrav1beta1.VMMigration, op migrationLongOp) string {
	return fmt.Sprintf("%s/%d/%s", migration.UID, migration.Generation, op)
}

// markLongOpStarted records that the given long-running RPC has been issued for
// this migration generation. Returns true if this call won the race (i.e. the
// op was not already marked), false if another reconcile already marked it.
func (r *VMMigrationReconciler) markLongOpStarted(migration *infrav1beta1.VMMigration, op migrationLongOp) bool {
	_, loaded := r.longOpInFlight.LoadOrStore(longOpKey(migration, op), struct{}{})
	return !loaded
}

// longOpAlreadyStarted reports whether the given long-running RPC has already
// been issued for this migration generation in this manager process.
func (r *VMMigrationReconciler) longOpAlreadyStarted(migration *infrav1beta1.VMMigration, op migrationLongOp) bool {
	_, loaded := r.longOpInFlight.Load(longOpKey(migration, op))
	return loaded
}

// NewVMMigrationReconciler creates a new VMMigration reconciler.
//
// enforceCapabilities mirrors the manager's --enforce-provider-capabilities
// flag (issue #176); when true, the migration export/import phases are gated
// on the source/target providers' self-reported capabilities. Pass false to
// preserve the pre-#176 behaviour exactly.
func NewVMMigrationReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	remoteResolver *remote.Resolver,
	recorder record.EventRecorder,
	enforceCapabilities bool,
) *VMMigrationReconciler {
	return &VMMigrationReconciler{
		Client:              client,
		Scheme:              scheme,
		RemoteResolver:      remoteResolver,
		Recorder:            recorder,
		metrics:             metrics.NewReconcileMetrics("VMMigration"),
		EnforceCapabilities: enforceCapabilities,
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

// Reconcile is part of the main kubernetes reconciliation loop.
//
// Observability: per-call timer + outcome inference via deferred block
// emits `virtrigaud_manager_reconcile_total{kind="VMMigration",outcome=...}`
// and `virtrigaud_manager_reconcile_duration_seconds{kind="VMMigration"}`.
// Specific error sites also record `virtrigaud_errors_total{reason=...,
// component="manager"}`. Reason taxonomy: see the `errReason*` constants
// at the top of this file (added in G3).
//
// Named return values (`result`, `retErr`) are required by the deferred
// outcome-inference block — do not change the signature without updating
// the defer.
//
// Fixes issue #105: prior implementation used `defer timer.Finish(
// metrics.OutcomeSuccess)` (argument captured immediately) plus explicit
// `timer.Finish(metrics.OutcomeError)` on error paths. Errored reconciles
// recorded TWO samples (one error + one success) because the deferred
// Finish ran AFTER the explicit one. New pattern records exactly one.
func (r *VMMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("VMMigration")
	defer func() {
		outcome := metrics.OutcomeSuccess
		switch {
		case retErr != nil:
			outcome = metrics.OutcomeError
		case result.Requeue || result.RequeueAfter > 0:
			outcome = metrics.OutcomeRequeue
		}
		timer.Finish(outcome)
	}()

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
		metrics.RecordError(errReasonGetMigration, metrics.ComponentManager)
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
			metrics.RecordError(errReasonAddFinalizer, metrics.ComponentManager)
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

	// Short-circuit for migrations in terminal failed state (max retries exceeded)
	// This prevents continuous reconciliation of permanently failed migrations
	if migration.Status.Phase == infrav1beta1.MigrationPhaseFailed {
		if migration.Spec.Options != nil && migration.Spec.Options.RetryPolicy != nil {
			maxRetries := int32(3) // Default
			if migration.Spec.Options.RetryPolicy.MaxRetries != nil {
				maxRetries = *migration.Spec.Options.RetryPolicy.MaxRetries
			}
			if migration.Status.RetryCount >= maxRetries {
				// Migration has permanently failed, no need to reconcile further
				logger.V(1).Info("Migration permanently failed, skipping reconciliation",
					"retries", migration.Status.RetryCount,
					"max_retries", maxRetries)
				return ctrl.Result{}, nil
			}
		} else {
			// No retry policy, migration is permanently failed
			logger.V(1).Info("Migration failed with no retry policy, skipping reconciliation")
			return ctrl.Result{}, nil
		}
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

	// Gate the requested storage backend and transfer mode against what both
	// providers honestly report (ADR-0006 Slice 0). Only the pvc backend has
	// transfer logic today; nfs/s3 — and any backend/mode the source's export
	// set or the target's import set does not advertise — fail fast here with an
	// actionable message instead of wedging in a later phase.
	if msg := r.gateMigrationStorageBackend(migration, sourceProvider, targetProvider); msg != "" {
		return r.transitionToFailed(ctx, migration, msg)
	}

	// Validate storage configuration
	if migration.Spec.Storage != nil {
		if err := r.validateStorageConfig(ctx, migration.Spec.Storage); err != nil {
			return r.transitionToFailed(ctx, migration, fmt.Sprintf("Invalid storage configuration: %v", err))
		}

		// The s3 backend (ADR-0006) stages bytes through external object storage,
		// not a shared PVC, so it has NEITHER the cross-namespace constraint NOR a
		// migration PVC to create. The provider pods are the S3 clients. Skip the
		// PVC path entirely for s3.
		if migrationBackendType(migration) == storagemigration.BackendS3 {
			// Validate credentials resolve before any side effect (fail fast at
			// Validating, never mid-export — ADR-0006 D6). Never log the values.
			if _, err := r.loadS3Credentials(ctx, migration); err != nil {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf("Invalid s3 storage configuration: %v", err))
			}
		} else {
			// Fail fast on a cross-namespace topology (#229). The migration transfer
			// PVC is mounted into both the source and target provider pods, and a
			// Kubernetes pod can only mount a PVC from its own namespace. If either
			// provider lives in a different namespace than the migration (and thus the
			// PVC), the mount can never happen — reject it up front with an actionable
			// message instead of letting the handshake time out opaquely.
			if sourceProvider.Namespace != migration.Namespace || targetProvider.Namespace != migration.Namespace {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf(
					"PVC-based migration requires the migration (%s), source provider (%s/%s) and target provider (%s/%s) "+
						"to share one namespace; a pod cannot mount a PVC across namespaces",
					migration.Namespace, sourceProvider.Namespace, sourceProvider.Name,
					targetProvider.Namespace, targetProvider.Name))
			}

			// Ensure PVC exists or create it
			pvcName, err := r.ensureMigrationPVC(ctx, migration)
			if err != nil {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to ensure migration PVC: %v", err))
			}

			// Record the PVC name so later phases can find it.
			if migration.Status.StoragePVCName == "" {
				migration.Status.StoragePVCName = pvcName
				if err := r.updateStatus(ctx, migration); err != nil {
					return ctrl.Result{}, err
				}
			}

			// Non-blocking wait for both providers to roll a pod with the PVC mounted.
			// The provider controller watches migration PVCs and re-rolls its
			// Deployment to attach them; rather than blocking a reconcile worker with
			// a time.Sleep poll, evaluate the mount state once and requeue until it
			// settles or the deadline (derived from the PVC's age) elapses.
			ready, reason, err := r.migrationProvidersMounted(ctx, sourceProvider, targetProvider, pvcName)
			if err != nil {
				return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to check migration storage mount: %v", err))
			}
			if !ready {
				exceeded, mErr := r.migrationMountDeadlineExceeded(ctx, migration, pvcName)
				if mErr != nil {
					return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to check migration storage mount: %v", mErr))
				}
				if exceeded {
					return r.transitionToFailed(ctx, migration, fmt.Sprintf(
						"timed out after %s waiting for providers to mount migration PVC %s (%s); "+
							"verify the provider Deployments rolled new pods and that those pods are Ready "+
							"(kubectl -n %s get deploy,pod -l app.kubernetes.io/name=virtrigaud-provider)",
						migrationMountTimeout, pvcName, reason, migration.Namespace))
				}

				k8s.SetCondition(&migration.Status.Conditions, infrav1beta1.VMMigrationConditionValidating,
					metav1.ConditionFalse, "WaitingForStorageMount", reason)
				migration.Status.Message = fmt.Sprintf("Waiting for providers to mount migration storage: %s", reason)
				if err := r.updateStatus(ctx, migration); err != nil {
					return ctrl.Result{}, err
				}
				logger.Info("Waiting for providers to mount migration PVC", "pvc", pvcName, "reason", reason)
				return ctrl.Result{RequeueAfter: migrationMountPollInterval}, nil
			}

			logger.Info("Migration storage PVC mounted on both providers", "pvc", pvcName)
		}
	}

	// Validation complete, transition to next phase
	logger.Info("Validation complete")

	// Power off the source before export when requested. This gates the
	// transition to snapshot/export until the source reports Off, so the disk is
	// captured from a stopped VM. It is REQUIRED for a vSphere source — a running
	// VM's disk is locked and cannot be cloned to streamOptimized (#236, Bug H).
	if migration.Spec.Source.PowerOffBeforeMigration {
		done, res, err := r.ensureSourcePoweredOff(ctx, migration)
		if err != nil {
			return r.transitionToFailed(ctx, migration,
				fmt.Sprintf("Failed to power off source VM before migration: %v", err))
		}
		if !done {
			return res, nil
		}
	}

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

// ensureSourcePoweredOff powers off the migration source when
// spec.source.powerOffBeforeMigration is set, returning done=true only once the
// source reports the Off power state. A stopped source yields a quiescent disk
// and, for a vSphere source, is REQUIRED — ESXi locks a running VM's disk so it
// cannot be cloned to streamOptimized. It first aligns the source VM's desired
// power state (Spec.PowerState) to Off so the VirtualMachine reconciler does not
// race the export by powering the source back on, then issues a hard power-off
// (reliable, no guest-agent dependency; crash-consistent like a forced shutdown);
// a graceful guest shutdown is a documented future refinement. While the VM is
// still powering down it returns done=false with a requeue.
func (r *VMMigrationReconciler) ensureSourcePoweredOff(ctx context.Context, migration *infrav1beta1.VMMigration) (bool, ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	sourceVM, err := r.getSourceVM(ctx, migration)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("get source VM: %w", err)
	}
	if sourceVM.Status.ID == "" {
		return false, ctrl.Result{}, fmt.Errorf("source VM %s has no provider ID yet", sourceVM.Name)
	}

	// Align the source VM's desired power state with the migration intent so the
	// VirtualMachine reconciler does not race this controller back to On while the
	// disk is being exported. Without this, the VM reconciler (whose desired state
	// is Spec.PowerState, defaulting to On) re-powers the source mid-export — which
	// can re-lock a vSphere disk and would, for a non-deleted source, leave the
	// origin VM running after the move (split-brain). This patch is idempotent.
	if sourceVM.Spec.PowerState != infrav1beta1.PowerStateOff {
		patch := client.MergeFrom(sourceVM.DeepCopy())
		sourceVM.Spec.PowerState = infrav1beta1.PowerStateOff
		if err := r.Patch(ctx, sourceVM, patch); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("set source VM %s desired power state to Off: %w", sourceVM.Name, err)
		}
		logger.Info("Set source VM desired power state to Off for migration", "vm", sourceVM.Name)
	}

	sourceProvider, err := r.getProvider(ctx, sourceVM.Spec.ProviderRef, migration.Namespace)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("get source provider: %w", err)
	}
	providerInstance, err := r.getProviderInstance(ctx, sourceProvider)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("get source provider instance: %w", err)
	}

	desc, err := providerInstance.Describe(ctx, sourceVM.Status.ID)
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("describe source VM: %w", err)
	}
	if desc.PowerState == string(contracts.PowerStateOff) {
		logger.Info("Source VM is powered off; proceeding with migration", "vm", sourceVM.Name)
		return true, ctrl.Result{}, nil
	}

	// Issue the power-off only while the VM is still On so a transitional state is
	// not spammed with redundant power-off tasks; otherwise just keep polling.
	if desc.PowerState == string(contracts.PowerStateOn) {
		logger.Info("Powering off source VM before migration", "vm", sourceVM.Name, "id", sourceVM.Status.ID)
		if _, err := providerInstance.Power(ctx, sourceVM.Status.ID, contracts.PowerOpOff); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("power off source VM: %w", err)
		}
		r.Recorder.Event(migration, "Normal", "SourcePowerOff", "Powering off source VM before migration")
	}

	migration.Status.Message = "Powering off source VM before migration"
	if err := r.updateStatus(ctx, migration); err != nil {
		return false, ctrl.Result{}, err
	}
	return false, ctrl.Result{RequeueAfter: migrationPowerOffPollInterval}, nil
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

	// Check if export is already in progress or already completed.
	//
	// The guard is intentionally broader than the cache-read ExportID: a
	// reconcile that re-enters immediately after a synchronous ExportDisk can
	// observe a stale informer cache (Phase still Exporting, ExportID still "")
	// even though the persisted object already advanced. Issuing ExportDisk
	// again would overwrite the staged object with non-deterministic bytes
	// whose checksum is then lost to a status-update conflict, corrupting the
	// transfer (see CHANGELOG / GRPC race fix). To prevent that we also treat:
	//   - the durable Exporting condition being True (persisted, survives
	//     manager restart), and
	//   - the in-memory in-flight marker (set just before the RPC, robust
	//     against a cache that has not yet caught up to either of the above)
	// as "export already issued for this generation → advance to Importing".
	exportAlreadyDone := migration.Status.ExportID != "" ||
		k8s.IsConditionTrue(migration.Status.Conditions, infrav1beta1.VMMigrationConditionExporting) ||
		r.longOpAlreadyStarted(migration, longOpExport)
	if exportAlreadyDone {
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

		// Do NOT immediate-requeue after a status write: re-reading the
		// informer cache before it has caught up would re-observe Phase
		// Exporting and could re-issue ExportDisk. The status update emits a
		// watch event (no status-filtering predicate in SetupWithManager) that
		// re-drives reconcile with a fresh, consistent cache.
		return ctrl.Result{}, nil
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

	// Resolve the s3 staging options and credentials (ADR-0006). For the pvc
	// backend these are empty/no-op, preserving the legacy behavior. Credentials
	// come from the referenced Secret and are NEVER logged or placed in Status.
	storageOptionsJSON, err := s3StorageOptionsJSON(migration)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to build storage options: %v", err))
	}
	creds, err := r.loadS3Credentials(ctx, migration)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to load storage credentials: %v", err))
	}

	// Build export request. backend_type carries the requested staging backend
	// (default pvc); transfer_mode is resolved auto→relay (Slice 1 implements
	// only relay). storage_options_json carries the non-secret s3 options.
	exportReq := contracts.ExportDiskRequest{
		VmId:               sourceVM.Status.ID,
		DiskId:             "", // Empty means export primary disk
		SnapshotId:         migration.Status.SnapshotID,
		DestinationURL:     destinationURL,
		Format:             diskFormat,
		Compress:           migration.Spec.Options != nil && migration.Spec.Options.Compress,
		Credentials:        creds,
		BackendType:        migrationBackendType(migration),
		TransferMode:       resolveTransferMode(migration),
		StorageOptionsJSON: storageOptionsJSON,
	}

	// Create extended context for export operation (disk exports can take a long time)
	// For large disks (80GB+), clone + download + convert + upload can take 30+ minutes
	exportCtx, exportCancel := context.WithTimeout(ctx, 1*time.Hour)
	defer exportCancel()

	// Capability gate (issue #176). When enforcement is enabled and the
	// source provider reports it cannot export disks, fail the migration
	// BEFORE issuing the export RPC. Fails open if the provider does not
	// implement CapabilityReporter or the capability query fails. No-op when
	// EnforceCapabilities is false.
	if blocked, res := r.gateProviderCapability(ctx, migration, providerInstance,
		func(caps contracts.Capabilities) bool { return caps.SupportsDiskExport },
		"Source provider does not support disk export"); blocked {
		return res, nil
	}

	// Claim the in-memory export guard for this object generation BEFORE
	// issuing the RPC. If another reconcile already claimed it, a duplicate
	// ExportDisk would overwrite the staged object with non-deterministic
	// bytes; skip the RPC and re-drive so the already-running/completed export
	// drives the transition instead. This complements the durable
	// ExportID/condition guard above against a still-stale cache.
	if !r.markLongOpStarted(migration, longOpExport) {
		logger.Info("Export already issued for this migration generation; skipping duplicate ExportDisk RPC")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("Starting disk export", "vm_id", sourceVM.Status.ID, "destination", destinationURL)
	exportResp, err := providerInstance.ExportDisk(exportCtx, exportReq)
	if err != nil {
		// The export RPC failed; release the in-memory guard so a future
		// reconcile (e.g. after retry/backoff) can re-attempt the export for
		// this generation. transitionToFailed records the failure durably.
		r.longOpInFlight.Delete(longOpKey(migration, longOpExport))
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

	// CRITICAL: do NOT immediate-requeue here. The synchronous ExportDisk above
	// can take ~16 minutes; an immediate requeue re-reads the informer cache
	// before this status write has propagated, re-observes Phase Exporting with
	// an empty ExportID, and issues a SECOND ExportDisk — which overwrites the
	// staged (non-deterministic) object while its checksum is lost to a
	// conflicting status update, corrupting the transfer. The status write
	// above emits a watch event (no status-filtering predicate in
	// SetupWithManager) that re-drives reconcile with a fresh cache.
	return ctrl.Result{}, nil
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

	// Check if import is already in progress or already completed.
	//
	// As with the export guard, this is intentionally broader than the
	// cache-read ImportID. ImportDisk is also synchronous and long-running and
	// writes the target qcow2; a duplicate driven by a stale cache would
	// overwrite it. Treat the durable Importing condition being True and the
	// in-memory in-flight marker as "import already issued for this generation
	// → advance to Creating".
	importAlreadyDone := migration.Status.ImportID != "" ||
		k8s.IsConditionTrue(migration.Status.Conditions, infrav1beta1.VMMigrationConditionImporting) ||
		r.longOpAlreadyStarted(migration, longOpImport)
	if importAlreadyDone {
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

		// Do NOT immediate-requeue after a status write (see export phase): the
		// status update re-drives reconcile with a fresh cache.
		return ctrl.Result{}, nil
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

	// Resolve the s3 staging options and credentials (ADR-0006). For the pvc
	// backend these are empty/no-op. Credentials come from the referenced Secret
	// and are NEVER logged or placed in Status.
	storageOptionsJSON, err := s3StorageOptionsJSON(migration)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to build storage options: %v", err))
	}
	creds, err := r.loadS3Credentials(ctx, migration)
	if err != nil {
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to load storage credentials: %v", err))
	}

	// For the s3 backend the staged object is the SOURCE provider's native
	// flattened format and the target converts on import (ADR-0006 D4). This is
	// DIRECTION-AWARE and derived from the source provider type — never a
	// hard-coded "vmdk": vSphere source -> vmdk (forward), libvirt source ->
	// qcow2 (reverse). For non-s3 backends the legacy/spec-derived format is
	// threaded unchanged.
	importFormat := diskFormat
	if migrationBackendType(migration) == storagemigration.BackendS3 {
		sourceProvider, srcErr := r.getSourceProvider(ctx, migration)
		if srcErr != nil {
			return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to get source provider: %v", srcErr))
		}
		importFormat = stagedImportFormat(sourceProvider)
		logger.Info("Derived s3 staged import format from source provider",
			"source_provider_type", sourceProvider.Spec.Type, "import_format", importFormat)
	}

	// Build import request. transfer_mode is resolved auto→relay (Slice 1).
	importReq := contracts.ImportDiskRequest{
		SourceURL:          sourceURL,
		StorageHint:        "", // Let provider choose
		Format:             importFormat,
		TargetName:         fmt.Sprintf("%s-migrated", migration.Spec.Target.Name),
		VerifyChecksum:     migration.Spec.Options == nil || migration.Spec.Options.VerifyChecksums,
		ExpectedChecksum:   "",
		Credentials:        creds,
		BackendType:        migrationBackendType(migration),
		TransferMode:       resolveTransferMode(migration),
		StorageOptionsJSON: storageOptionsJSON,
	}

	// Set expected checksum if available
	if migration.Status.DiskInfo != nil && migration.Status.DiskInfo.SourceChecksum != "" {
		importReq.ExpectedChecksum = migration.Status.DiskInfo.SourceChecksum
	}

	// Capability gate (issue #176). When enforcement is enabled and the
	// target provider reports it cannot import disks, fail the migration
	// BEFORE issuing the import RPC. Fails open if the provider does not
	// implement CapabilityReporter or the capability query fails. No-op when
	// EnforceCapabilities is false.
	if blocked, res := r.gateProviderCapability(ctx, migration, providerInstance,
		func(caps contracts.Capabilities) bool { return caps.SupportsDiskImport },
		"Target provider does not support disk import"); blocked {
		return res, nil
	}

	// Claim the in-memory import guard for this object generation BEFORE
	// issuing the RPC. ImportDisk writes the target qcow2; a duplicate driven
	// by a stale cache would overwrite it. If another reconcile already claimed
	// it, skip the RPC and re-drive.
	if !r.markLongOpStarted(migration, longOpImport) {
		logger.Info("Import already issued for this migration generation; skipping duplicate ImportDisk RPC")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("Starting disk import", "source", sourceURL, "target_name", importReq.TargetName)
	importResp, err := providerInstance.ImportDisk(ctx, importReq)
	if err != nil {
		// Release the in-memory guard so a future reconcile can re-attempt the
		// import for this generation after the failure is recorded.
		r.longOpInFlight.Delete(longOpKey(migration, longOpImport))
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to start disk import: %v", err))
	}

	// Store import info in status.
	//
	// Space precheck (ADR-0006 Slice 2): there is intentionally NO target-side
	// capacity gate here today, so nothing asserts that the imported disk's
	// *used* size fits the target. This matters for vSphere: Approach 2 inflates
	// the disk with CopyVirtualDisk, which materializes as eagerZeroedThick on
	// VMFS — the target disk consumes its FULL provisioned (virtual) size, not
	// the used size. importResp.ActualSizeBytes below is reported for the status
	// message only and is NOT compared against any datastore capacity. If a
	// target-side capacity gate is added later, it MUST budget the *virtual*
	// (provisioned) size for vSphere targets, never the used/actual size.
	migration.Status.ImportID = importResp.DiskId
	migration.Status.Message = fmt.Sprintf("Importing disk (size: %d bytes)", importResp.ActualSizeBytes)

	// Update disk info in status.
	//
	// TargetFormat is the format the disk landed in on the TARGET provider,
	// derived from the target provider type (vSphere target -> vmdk, libvirt
	// target -> qcow2) rather than hard-defaulted to qcow2. For the forward
	// vSphere -> libvirt path this still resolves to qcow2, so that path is
	// unchanged; for the reverse libvirt -> vSphere path it correctly labels the
	// landed disk as vmdk.
	//
	// TargetPath is the provider-native path returned by ImportDisk (e.g.
	// "[datastore1] <id>/<id>.vmdk" for vSphere). It is CRITICAL for the reverse
	// path: the VM controller must attach the disk at exactly this path rather
	// than synthesizing a bogus libvirt-style path. The created target VM copies
	// it into Spec.ImportedDisk.Path in the creating phase.
	if migration.Status.DiskInfo == nil {
		migration.Status.DiskInfo = &infrav1beta1.MigrationDiskInfo{}
	}
	migration.Status.DiskInfo.TargetDiskID = importResp.DiskId
	migration.Status.DiskInfo.TargetFormat = landedTargetFormat(targetProvider)
	migration.Status.DiskInfo.TargetPath = importResp.Path
	migration.Status.DiskInfo.TargetChecksum = importResp.Checksum
	logger.Info("Recorded imported disk info",
		"target_disk_id", importResp.DiskId,
		"target_format", migration.Status.DiskInfo.TargetFormat,
		"target_path", importResp.Path)

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

	// Do NOT immediate-requeue here. ImportDisk above is synchronous and
	// long-running and writes the target qcow2; an immediate requeue re-reads a
	// stale cache (Phase still Importing, ImportID still "") and could re-issue
	// ImportDisk, overwriting the just-written target disk. Requeue after a short
	// settle delay instead: long enough for the informer cache to observe the
	// Phase=Creating write, but guaranteed to re-drive the Creating phase even if
	// the self-watch update event is coalesced or missed (relying on the status
	// write alone to re-enqueue left a synchronous Proxmox import wedged in
	// Creating).
	return ctrl.Result{RequeueAfter: migrationImportSettleInterval}, nil
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
		// IMPORTANT: Check the Ready condition, NOT the Phase field
		// VirtualMachine does not have a "Ready" phase - it uses conditions
		readyCondition := meta.FindStatusCondition(existingVM.Status.Conditions, "Ready")
		isReady := existingVM.Status.ID != "" && readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

		if isReady {
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
		readyStatus := "not set"
		if readyCondition != nil {
			readyStatus = string(readyCondition.Status)
		}
		logger.Info("Target VM exists but not ready, waiting", "vm", targetVMName, "readyCondition", readyStatus, "phase", existingVM.Status.Phase)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if client.IgnoreNotFound(err) != nil {
		// Error other than not found
		return r.transitionToFailed(ctx, migration, fmt.Sprintf("Failed to check target VM: %v", err))
	}

	// VM doesn't exist, create it.
	//
	// The imported-disk info must have been recorded during the Importing phase;
	// the target VM references the landed disk through it. If it is nil the import
	// did not persist disk info, so the target cannot be created correctly — fail
	// loudly with a diagnosable message instead of dereferencing nil (which
	// previously panicked in an infinite recover/requeue loop, wedging the
	// migration in Creating forever).
	if migration.Status.DiskInfo == nil {
		return r.transitionToFailed(ctx, migration,
			"import did not record disk info (status.diskInfo is nil); cannot create target VM")
	}

	logger.Info("Creating target VM", "name", targetVMName)

	targetVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetVMName,
			Namespace: targetNamespace,
			Labels:    migration.Spec.Target.Labels,
			Annotations: map[string]string{
				"virtrigaud.io/migrated-from":    fmt.Sprintf("%s/%s", migration.Namespace, migration.Spec.Source.VMRef.Name),
				"virtrigaud.io/migration":        fmt.Sprintf("%s/%s", migration.Namespace, migration.Name),
				"virtrigaud.io/imported-disk-id": migration.Status.ImportID,
				"virtrigaud.io/disk-checksum":    migration.Status.DiskInfo.TargetChecksum,
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

	// Set imported disk reference.
	// This references the disk that was imported during the migration. Path is
	// the authoritative provider-native location returned by ImportDisk and
	// recorded in Status.DiskInfo.TargetPath. Propagating it is CRITICAL for a
	// vSphere target: without it the VM controller would synthesize a libvirt
	// "/var/lib/libvirt/images/<id>.<fmt>" path that does not exist on the
	// target. When TargetPath is empty (older imports, or a provider that does
	// not return one), Path stays empty and the VM controller falls back to its
	// provider-default synthesis.
	targetVM.Spec.ImportedDisk = &infrav1beta1.ImportedDiskRef{
		DiskID: migration.Status.ImportID,
		Path:   migration.Status.DiskInfo.TargetPath,
		Format: migration.Status.DiskInfo.TargetFormat,
		Source: "migration",
		MigrationRef: &infrav1beta1.LocalObjectReference{
			Name: migration.Name,
		},
	}

	logger.Info("Target VM configured with imported disk",
		"disk_id", migration.Status.ImportID,
		"path", migration.Status.DiskInfo.TargetPath,
		"format", migration.Status.DiskInfo.TargetFormat,
		"checksum", migration.Status.DiskInfo.TargetChecksum)

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

	// Verify VM is ready by checking the Ready condition
	readyCondition := meta.FindStatusCondition(targetVM.Status.Conditions, "Ready")
	isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

	if !isReady {
		readyStatus := "not set"
		if readyCondition != nil {
			readyStatus = string(readyCondition.Status)
		}
		logger.Info("Target VM not ready yet", "readyCondition", readyStatus, "phase", targetVM.Status.Phase)
		migration.Status.Message = fmt.Sprintf("Waiting for target VM to be ready (current Ready condition: %s)", readyStatus)

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

	// Mark the target VM as completed to prevent deletion when migration is removed
	// This annotation protects the VM from being deleted with the migration resource
	if targetVM.Annotations == nil {
		targetVM.Annotations = make(map[string]string)
	}
	targetVM.Annotations["virtrigaud.io/migration-completed"] = "true"
	targetVM.Annotations["virtrigaud.io/migration-completed-at"] = time.Now().Format(time.RFC3339)
	if err := r.Update(ctx, targetVM); err != nil {
		logger.Error(err, "Failed to mark VM as migration-completed")
		// Don't fail the migration for this - the VM is already created and working
		// The annotation is just a safety marker
	} else {
		logger.Info("Marked target VM as migration-completed", "vm", fmt.Sprintf("%s/%s", targetNamespace, targetVMName))
	}

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

	// CleanupPolicy=Never opts out of removing migration-created artifacts
	// (intermediate storage + source snapshot). DeleteAfterMigration is handled
	// independently below because it is an explicit user action, not policy.
	if cleanupAllowed(migration) {
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
			snapshotID := migration.Status.SnapshotID
			if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
				logger.Error(err, "Failed to delete source snapshot, will retry")
				return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
			}
			cleanupPerformed = true
			logger.Info("Source snapshot deleted", "snapshot_id", snapshotID)
		}
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
		r.cleanupSnapshotOnTerminalFailure(ctx, migration)
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
		r.cleanupSnapshotOnTerminalFailure(ctx, migration)
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

// cleanupSnapshotOnTerminalFailure best-effort removes the migration-created
// source snapshot when the migration has reached a terminal Failed state and is
// not going to be retried.
//
// Without this, a migration that fails before Ready (e.g. no retry policy, so it
// is immediately terminal) leaves its snapshot on the LIVE source VM until the
// CR is deleted — which, combined with the await fix in deleteSourceSnapshot, is
// the accumulation observed on real hardware. The deletion is best-effort: a
// cleanup error is logged and swallowed so a transient provider failure cannot
// wedge a migration that is already terminal. Because deleteSourceSnapshot now
// awaits the task, this usually succeeds on the first pass. Skipped when the
// user opted out (CleanupPolicy=Never), when there is no migration-created
// snapshot, or when the user supplied their own snapshot (SnapshotRef set).
func (r *VMMigrationReconciler) cleanupSnapshotOnTerminalFailure(ctx context.Context, migration *infrav1beta1.VMMigration) {
	if !cleanupAllowed(migration) || migration.Status.SnapshotID == "" || migration.Spec.Source.SnapshotRef != nil {
		return
	}

	logger := logging.FromContext(ctx)
	snapshotID := migration.Status.SnapshotID
	if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
		logger.Error(err, "Failed to delete source snapshot on terminal failure (best-effort, continuing)",
			"snapshot_id", snapshotID)
		return
	}
	logger.Info("Source snapshot deleted on terminal failure", "snapshot_id", snapshotID)
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

	// 2. Explicitly delete PVC if it exists (in addition to owner reference cleanup)
	if migration.Status.StoragePVCName != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		pvcKey := client.ObjectKey{
			Namespace: migration.Namespace,
			Name:      migration.Status.StoragePVCName,
		}
		if err := r.Get(ctx, pvcKey, pvc); err == nil {
			logger.Info("Explicitly deleting PVC", "pvc_name", migration.Status.StoragePVCName)
			if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "Failed to delete PVC during cleanup", "pvc_name", migration.Status.StoragePVCName)
				cleanupErrors = append(cleanupErrors, fmt.Errorf("PVC cleanup: %w", err))
			} else {
				logger.Info("PVC deleted successfully", "pvc_name", migration.Status.StoragePVCName)
			}
		} else if !errors.IsNotFound(err) {
			logger.Error(err, "Failed to get PVC during cleanup", "pvc_name", migration.Status.StoragePVCName)
			cleanupErrors = append(cleanupErrors, fmt.Errorf("PVC get: %w", err))
		} else {
			logger.Info("PVC already deleted", "pvc_name", migration.Status.StoragePVCName)
		}
	}

	// 3. Delete migration-created snapshot if exists (unless the user opted out
	// with CleanupPolicy=Never).
	if cleanupAllowed(migration) && migration.Status.SnapshotID != "" && migration.Spec.Source.SnapshotRef == nil {
		if err := r.deleteSourceSnapshot(ctx, migration); err != nil {
			logger.Error(err, "Failed to delete source snapshot during deletion")
			cleanupErrors = append(cleanupErrors, fmt.Errorf("snapshot cleanup: %w", err))
		} else {
			logger.Info("Source snapshot cleaned up during deletion")
		}
	}

	// 4. Delete partially created target VM if migration failed
	// IMPORTANT: Never delete VMs that have been marked as migration-completed
	// This ensures that successfully migrated VMs persist independently of the migration resource
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
			// Check if VM has migration-completed marker
			if targetVM.Annotations != nil && targetVM.Annotations["virtrigaud.io/migration-completed"] == "true" {
				logger.Info("Target VM has migration-completed marker, skipping deletion",
					"vm", targetVMName,
					"completed_at", targetVM.Annotations["virtrigaud.io/migration-completed-at"])
				// VM is a successfully migrated VM, never delete it
			} else if targetVM.Annotations != nil {
				// Only delete if it has our migration annotation AND no completion marker
				if migrationRef, ok := targetVM.Annotations["virtrigaud.io/migration"]; ok {
					expectedRef := fmt.Sprintf("%s/%s", migration.Namespace, migration.Name)
					if migrationRef == expectedRef {
						logger.Info("Deleting partially created target VM", "vm", targetVMName)
						if err := r.Delete(ctx, targetVM); err != nil {
							logger.Error(err, "Failed to delete target VM during cleanup")
							cleanupErrors = append(cleanupErrors, fmt.Errorf("target VM cleanup: %w", err))
						} else {
							logger.Info("Partially created target VM deleted successfully", "vm", targetVMName)
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

	// Don't requeue here - let handleFailedPhase manage retry timing
	// This prevents continuous reconciliation loops that can overwhelm the API server
	return ctrl.Result{Requeue: true}, nil
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

// getSourceProvider retrieves the source provider for a migration
func (r *VMMigrationReconciler) getSourceProvider(ctx context.Context, migration *infrav1beta1.VMMigration) (*infrav1beta1.Provider, error) {
	var sourceProviderRef infrav1beta1.ObjectRef
	if migration.Spec.Source.ProviderRef != nil {
		sourceProviderRef = *migration.Spec.Source.ProviderRef
	} else {
		// Auto-detect from source VM
		sourceVM, err := r.getSourceVM(ctx, migration)
		if err != nil {
			return nil, fmt.Errorf("failed to get source VM: %w", err)
		}
		sourceProviderRef = sourceVM.Spec.ProviderRef
	}
	return r.getProvider(ctx, sourceProviderRef, migration.Namespace)
}

// getTargetProvider retrieves the target provider for a migration
func (r *VMMigrationReconciler) getTargetProvider(ctx context.Context, migration *infrav1beta1.VMMigration) (*infrav1beta1.Provider, error) {
	return r.getProvider(ctx, migration.Spec.Target.ProviderRef, migration.Namespace)
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

	// Validate storage type (ADR-0006: pvc and s3 have transfer logic).
	switch storageConfig.Type {
	case "pvc", "":
		// validated below
	case storagemigration.BackendS3:
		// S3 staging: require a bucket and a credentials Secret reference. The
		// actual credential values are validated when the Secret is read; here we
		// only check the shape. No PVC is involved.
		if storageConfig.S3 == nil {
			return fmt.Errorf("s3 configuration is required when using s3 storage type")
		}
		if storageConfig.S3.Bucket == "" {
			return fmt.Errorf("s3.bucket is required for s3 storage type")
		}
		if storageConfig.S3.CredentialsSecretRef.Name == "" {
			return fmt.Errorf("s3.credentialsSecretRef is required for s3 storage type")
		}
		return nil
	default:
		return fmt.Errorf("unsupported storage type: %s (supported: 'pvc', 's3')", storageConfig.Type)
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

	// Trigger provider reconciliation to mount the new PVC
	// This will cause provider pods to restart with the new PVC mounted
	if err := r.triggerProviderReconciliation(ctx, migration); err != nil {
		logger.Error(err, "Failed to trigger provider reconciliation", "pvc", pvcName)
		// Don't fail - reconciliation will happen eventually
	}

	return pvcName, nil
}

// triggerProviderReconciliation triggers reconciliation of both source and target providers
// by annotating them, causing provider pods to restart with updated PVC mounts
func (r *VMMigrationReconciler) triggerProviderReconciliation(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	logger := logging.FromContext(ctx)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	// Trigger source provider
	sourceProvider, err := r.getSourceProvider(ctx, migration)
	if err != nil {
		return fmt.Errorf("failed to get source provider: %w", err)
	}

	if sourceProvider.Annotations == nil {
		sourceProvider.Annotations = make(map[string]string)
	}
	sourceProvider.Annotations["virtrigaud.io/reconcile-trigger"] = timestamp
	sourceProvider.Annotations["virtrigaud.io/migration-pvc"] = migration.Status.StoragePVCName

	if err := r.Update(ctx, sourceProvider); err != nil {
		return fmt.Errorf("failed to annotate source provider: %w", err)
	}
	logger.Info("Triggered source provider reconciliation", "provider", sourceProvider.Name)

	// Trigger target provider
	targetProvider, err := r.getTargetProvider(ctx, migration)
	if err != nil {
		return fmt.Errorf("failed to get target provider: %w", err)
	}

	if targetProvider.Annotations == nil {
		targetProvider.Annotations = make(map[string]string)
	}
	targetProvider.Annotations["virtrigaud.io/reconcile-trigger"] = timestamp
	targetProvider.Annotations["virtrigaud.io/migration-pvc"] = migration.Status.StoragePVCName

	if err := r.Update(ctx, targetProvider); err != nil {
		return fmt.Errorf("failed to annotate target provider: %w", err)
	}
	logger.Info("Triggered target provider reconciliation", "provider", targetProvider.Name)

	return nil
}

// migrationMountTimeout bounds how long the VMMigration controller waits for
// both providers to roll a pod with the migration PVC mounted before failing
// with a diagnosable error. It is measured from the PVC's creation time so the
// budget is stable across reconciles and controller restarts.
const migrationMountTimeout = 5 * time.Minute

// migrationPowerOffPollInterval is the requeue cadence while waiting for a
// source VM to reach the Off power state when source.powerOffBeforeMigration is
// set (the export is gated on it).
const migrationPowerOffPollInterval = 5 * time.Second

// migrationImportSettleInterval is the short requeue delay after a synchronous
// ImportDisk completes and the phase advances to Creating. It is long enough for
// the informer cache to observe the Phase=Creating write (avoiding a stale-cache
// re-import) yet guarantees the Creating phase is re-driven even if the
// self-watch update event is coalesced or missed.
const migrationImportSettleInterval = 5 * time.Second

// migrationMountPollInterval is the requeue cadence while waiting for the
// provider controller to apply and roll out the migration PVC mount.
const migrationMountPollInterval = 5 * time.Second

// migrationProvidersMounted reports whether BOTH the source and target provider
// pods have rolled with the migration PVC mounted and Ready. It is a single,
// non-blocking evaluation: callers requeue rather than sleep. When ready is
// false, reason describes (with a source/target prefix) what it is waiting on.
func (r *VMMigrationReconciler) migrationProvidersMounted(ctx context.Context, source, target *infrav1beta1.Provider, pvcName string) (bool, string, error) {
	ready, reason, err := r.providerMountReady(ctx, source, pvcName)
	if err != nil {
		return false, "", fmt.Errorf("source provider %s: %w", source.Name, err)
	}
	if !ready {
		return false, "source: " + reason, nil
	}

	ready, reason, err = r.providerMountReady(ctx, target, pvcName)
	if err != nil {
		return false, "", fmt.Errorf("target provider %s: %w", target.Name, err)
	}
	if !ready {
		return false, "target: " + reason, nil
	}

	return true, "", nil
}

// providerMountReady performs a single, non-blocking check that the given
// provider's Deployment has rolled a Running, Ready pod carrying the migration
// PVC. The provider controller is responsible for discovering migration PVCs
// (label virtrigaud.io/component=migration-storage) and attaching them to its
// Deployment; this only observes the result. The returned reason explains what
// is still pending when ready is false.
func (r *VMMigrationReconciler) providerMountReady(ctx context.Context, provider *infrav1beta1.Provider, pvcName string) (bool, string, error) {
	deploymentName := fmt.Sprintf("virtrigaud-provider-%s-%s", provider.Namespace, provider.Name)

	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: provider.Namespace}, deployment); err != nil {
		if errors.IsNotFound(err) {
			return false, fmt.Sprintf("provider deployment %s not found yet", deploymentName), nil
		}
		return false, "", fmt.Errorf("get provider deployment %s: %w", deploymentName, err)
	}

	// The provider controller must have applied the PVC to the Deployment template.
	if !volumesContainPVC(deployment.Spec.Template.Spec.Volumes, pvcName) {
		return false, "deployment not yet updated with the migration PVC volume", nil
	}

	// The rollout that attaches the PVC must be complete.
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	if deployment.Status.UpdatedReplicas < replicas || deployment.Status.ReadyReplicas < replicas {
		return false, fmt.Sprintf("provider rollout in progress (updated %d/%d, ready %d/%d)",
			deployment.Status.UpdatedReplicas, replicas, deployment.Status.ReadyReplicas, replicas), nil
	}

	// At least one non-terminating pod must be Running, Ready and actually carry
	// the PVC (the old pods without it must be draining/gone).
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(provider.Namespace), client.MatchingLabels{
		"app.kubernetes.io/name":     "virtrigaud-provider",
		"app.kubernetes.io/instance": provider.Name,
	}); err != nil {
		return false, "", fmt.Errorf("list provider %s pods: %w", provider.Name, err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil {
			continue // ignore pods that are terminating
		}
		if !volumesContainPVC(pod.Spec.Volumes, pvcName) {
			continue // an old pod without the PVC, still draining
		}
		if pod.Status.Phase == corev1.PodRunning && podConditionTrue(pod, corev1.PodReady) {
			return true, "", nil
		}
	}

	return false, "no Ready pod with the migration PVC mounted yet", nil
}

// migrationMountDeadlineExceeded reports whether the migration PVC has existed
// longer than migrationMountTimeout without both providers mounting it. The
// budget is anchored on the PVC's creation time so it survives requeues.
func (r *VMMigrationReconciler) migrationMountDeadlineExceeded(ctx context.Context, migration *infrav1beta1.VMMigration, pvcName string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: migration.Namespace}, pvc); err != nil {
		return false, fmt.Errorf("get migration PVC %s: %w", pvcName, err)
	}
	return time.Since(pvc.CreationTimestamp.Time) > migrationMountTimeout, nil
}

// volumesContainPVC reports whether any volume in the slice binds the named PVC.
func volumesContainPVC(volumes []corev1.Volume, pvcName string) bool {
	for i := range volumes {
		if src := volumes[i].PersistentVolumeClaim; src != nil && src.ClaimName == pvcName {
			return true
		}
	}
	return false
}

// podConditionTrue reports whether the pod has the given condition set to True.
func podConditionTrue(pod *corev1.Pod, condType corev1.PodConditionType) bool {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == condType {
			return pod.Status.Conditions[i].Status == corev1.ConditionTrue
		}
	}
	return false
}

// gateProviderCapability enforces a single provider capability for a
// migration phase (issue #176). It returns blocked=true (with the
// ctrl.Result the caller should return) only when ALL of the following hold:
//   - r.EnforceCapabilities is true, AND
//   - providerInstance implements contracts.CapabilityReporter and its
//     GetCapabilities call succeeds, AND
//   - supported(caps) is false (the provider reports it cannot do this).
//
// In all other cases it returns blocked=false and the migration proceeds:
//   - enforcement off → no-op (byte-for-byte unchanged phase),
//   - provider is not a CapabilityReporter → FAIL OPEN (never block),
//   - GetCapabilities errors → FAIL OPEN (transient; let the RPC speak),
//   - provider reports the capability → proceed.
//
// When it blocks, it transitions the migration to Failed with the supplied
// reason via transitionToFailed, so no provider RPC is issued.
func (r *VMMigrationReconciler) gateProviderCapability(
	ctx context.Context,
	migration *infrav1beta1.VMMigration,
	providerInstance contracts.Provider,
	supported func(caps contracts.Capabilities) bool,
	failureMessage string,
) (blocked bool, result ctrl.Result) {
	if !r.EnforceCapabilities {
		return false, ctrl.Result{}
	}

	logger := logging.FromContext(ctx)

	reporter, ok := providerInstance.(contracts.CapabilityReporter)
	if !ok {
		// Fail open: provider does not advertise capabilities.
		logger.V(1).Info("Capability enforcement on but provider does not report capabilities; allowing migration step")
		return false, ctrl.Result{}
	}

	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		// Fail open: do not block on a transient capability-query failure.
		logger.V(1).Info("Capability enforcement on but GetCapabilities failed; allowing migration step", "error", err.Error())
		return false, ctrl.Result{}
	}

	if supported(caps) {
		return false, ctrl.Result{}
	}

	res, _ := r.transitionToFailed(ctx, migration, failureMessage)
	return true, res
}

// migrationBackendType returns the requested staging backend for a migration,
// defaulting to pvc when storage is unset or its type is empty (ADR-0006).
func migrationBackendType(migration *infrav1beta1.VMMigration) string {
	if migration.Spec.Storage == nil || migration.Spec.Storage.Type == "" {
		return storagemigration.BackendPVC
	}
	return migration.Spec.Storage.Type
}

// migrationTransferMode returns the requested transfer mode for a migration,
// defaulting to auto when storage is unset or its transferMode is empty
// (ADR-0006).
func migrationTransferMode(migration *infrav1beta1.VMMigration) string {
	if migration.Spec.Storage == nil || migration.Spec.Storage.TransferMode == "" {
		return storagemigration.TransferModeAuto
	}
	return migration.Spec.Storage.TransferMode
}

// resolveTransferMode collapses the requested transfer mode to the concrete mode
// the providers actually run (ADR-0006 D2). Slice 1 implements only relay, so
// "auto" resolves to "relay" and "relay" passes through. "direct" is rejected at
// the gate before this is called, so it never reaches here.
func resolveTransferMode(migration *infrav1beta1.VMMigration) string {
	mode := migrationTransferMode(migration)
	if mode == storagemigration.TransferModeAuto {
		return storagemigration.TransferModeRelay
	}
	return mode
}

// s3StorageOptionsJSON builds the non-secret storage_options_json carried to a
// provider for the s3 backend (ADR-0006). Returns "" for non-s3 backends.
func s3StorageOptionsJSON(migration *infrav1beta1.VMMigration) (string, error) {
	if migrationBackendType(migration) != storagemigration.BackendS3 {
		return "", nil
	}
	s3 := migration.Spec.Storage.S3
	if s3 == nil {
		return "", fmt.Errorf("s3 storage configuration is required for s3 backend")
	}
	return storagemigration.MarshalStorageOptions(storagemigration.StorageOptions{
		Backend:      storagemigration.BackendS3,
		Bucket:       s3.Bucket,
		Endpoint:     s3.Endpoint,
		Region:       s3.Region,
		Prefix:       s3.Prefix,
		UsePathStyle: s3.UsePathStyle,
	})
}

// loadS3Credentials reads the S3 access credentials from the Secret referenced by
// MigrationStorage.S3.CredentialsSecretRef and returns them as the gRPC
// credentials map keyed per ADR-0006. Returns an empty map (no error) for
// non-s3 backends so callers can unconditionally merge the result. Credential
// VALUES are secret material: this function never logs them, and the caller must
// never place them in Status, events, or logs.
func (r *VMMigrationReconciler) loadS3Credentials(ctx context.Context, migration *infrav1beta1.VMMigration) (map[string]string, error) {
	if migrationBackendType(migration) != storagemigration.BackendS3 {
		return map[string]string{}, nil
	}
	s3 := migration.Spec.Storage.S3
	if s3 == nil || s3.CredentialsSecretRef.Name == "" {
		return nil, fmt.Errorf("s3 backend requires storage.s3.credentialsSecretRef")
	}

	// The Secret lives in the migration's namespace (namespaced get only; #152).
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: migration.Namespace, Name: s3.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("get s3 credentials secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	accessKey := string(secret.Data[storagemigration.CredKeyAccessKeyID])
	secretKey := string(secret.Data[storagemigration.CredKeySecretAccessKey])
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf(
			"s3 credentials secret %s/%s must contain keys %q and %q",
			key.Namespace, key.Name, storagemigration.CredKeyAccessKeyID, storagemigration.CredKeySecretAccessKey)
	}

	creds := map[string]string{
		storagemigration.CredKeyAccessKeyID:     accessKey,
		storagemigration.CredKeySecretAccessKey: secretKey,
	}
	if token := string(secret.Data[storagemigration.CredKeySessionToken]); token != "" {
		creds[storagemigration.CredKeySessionToken] = token
	}
	return creds, nil
}

// reportedExportBackends returns a provider's advertised export backends,
// treating an empty/absent set as the implicit pvc-only default of a provider
// that predates ADR-0006 (matching the ReportedCapabilities field contract).
func reportedExportBackends(p *infrav1beta1.Provider) []string {
	if p.Status.ReportedCapabilities == nil || len(p.Status.ReportedCapabilities.SupportedExportBackends) == 0 {
		return storagemigration.PVCOnlyExportBackends()
	}
	return p.Status.ReportedCapabilities.SupportedExportBackends
}

// reportedImportBackends returns a provider's advertised import backends,
// treating an empty/absent set as the implicit pvc-only default.
func reportedImportBackends(p *infrav1beta1.Provider) []string {
	if p.Status.ReportedCapabilities == nil || len(p.Status.ReportedCapabilities.SupportedImportBackends) == 0 {
		return storagemigration.PVCOnlyImportBackends()
	}
	return p.Status.ReportedCapabilities.SupportedImportBackends
}

// reportedTransferModes returns a provider's advertised transfer modes, treating
// an empty/absent set as the implicit relay-only default.
func reportedTransferModes(p *infrav1beta1.Provider) []string {
	if p.Status.ReportedCapabilities == nil || len(p.Status.ReportedCapabilities.SupportedTransferModes) == 0 {
		return storagemigration.RelayOnlyTransferModes()
	}
	return p.Status.ReportedCapabilities.SupportedTransferModes
}

// containsString reports whether want is present in set.
func containsString(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}

// Native staged disk formats per provider family. The s3-relay path stages a
// disk in the SOURCE provider's native flattened format and converts on import,
// so the import format is derived from the source provider type and the landed
// target disk's format is derived from the target provider type — never from a
// hard-coded direction (ADR-0006 Slice 2).
const (
	// diskFormatQcow2 is the native staged/landed format for libvirt and proxmox.
	diskFormatQcow2 = "qcow2"
	// diskFormatVMDK is the native staged/landed format for vSphere.
	diskFormatVMDK = "vmdk"
)

// nativeDiskFormat returns the provider family's native flattened disk format.
// vSphere stages/lands vmdk; libvirt and proxmox (and any QEMU-backed family)
// stage/land qcow2. Unknown/empty provider types fall back to qcow2, matching
// the pre-ADR-0006 default and the libvirt-source forward assumption. This is
// the single source of truth for both the import-format and target-format
// derivations below, so the two can never disagree for the same provider.
func nativeDiskFormat(p *infrav1beta1.Provider) string {
	if p != nil && p.Spec.Type == infrav1beta1.ProviderTypeVSphere {
		return diskFormatVMDK
	}
	return diskFormatQcow2
}

// stagedImportFormat returns the format of the staged object the TARGET provider
// must read on import for an s3-relay migration. The staged object is the SOURCE
// provider's native flattened format (vSphere source -> vmdk, libvirt/proxmox
// source -> qcow2), regardless of migration direction. For non-s3 backends the
// caller threads the legacy/spec-derived format instead; this is only consulted
// on the s3 path.
//
// Note: contracts.ExportDiskResponse does not carry a Format field today, so the
// staged format is derived from the source provider type rather than threaded
// from the export response. If the contract later grows an authoritative
// ExportDiskResponse.Format, prefer threading it and fall back to this.
func stagedImportFormat(sourceProvider *infrav1beta1.Provider) string {
	return nativeDiskFormat(sourceProvider)
}

// landedTargetFormat returns the format of the disk that the TARGET provider's
// ImportDisk materializes natively (vSphere target -> vmdk, libvirt/proxmox
// target -> qcow2). It labels Status.DiskInfo.TargetFormat and seeds the created
// VirtualMachine's Spec.ImportedDisk.Format so the VM controller attaches the
// disk with the correct on-disk format for the target hypervisor.
func landedTargetFormat(targetProvider *infrav1beta1.Provider) string {
	return nativeDiskFormat(targetProvider)
}

// gateMigrationStorageBackend validates the requested storage backend and
// transfer mode against what the source provider can export to and the target
// provider can import from (read from each Provider's
// status.reportedCapabilities). It returns an empty string when the migration
// may proceed, or an actionable failure message otherwise (ADR-0006).
//
// Per-direction honesty (ADR-0006 D6): the source's EXPORT set and the target's
// IMPORT set are compared independently, so vSphere(export=[pvc,s3]) →
// libvirt(import=[pvc,s3]) in relay is allowed (Slice 1) while the reverse
// direction — libvirt(export=[pvc]) → vSphere(import=[pvc]) over s3 — fails fast
// because neither end advertises s3 in that direction. nfs and direct are
// rejected up front because no provider implements them yet. The "auto" transfer
// mode is permitted here and resolved to a concrete mode at request-build time.
func (r *VMMigrationReconciler) gateMigrationStorageBackend(
	migration *infrav1beta1.VMMigration,
	sourceProvider *infrav1beta1.Provider,
	targetProvider *infrav1beta1.Provider,
) string {
	backend := migrationBackendType(migration)
	mode := migrationTransferMode(migration)

	// Only pvc and s3 have transfer logic. nfs has none yet (ADR-0006 Slice 4),
	// so reject it up front regardless of what a provider might (later) advertise.
	if backend != storagemigration.BackendPVC && backend != storagemigration.BackendS3 {
		return fmt.Sprintf(
			"storage backend %q is not yet implemented; supported today: %q, %q (ADR-0006)",
			backend, storagemigration.BackendPVC, storagemigration.BackendS3)
	}

	// Only relay is implemented (ADR-0006 Slice 1). An explicit "direct" fails
	// loudly here — never a silent downgrade — before any side effect.
	if mode == storagemigration.TransferModeDirect {
		return fmt.Sprintf(
			"transfer mode %q is not yet implemented; only %q (and %q) are supported today (ADR-0006 Slice 1)",
			storagemigration.TransferModeDirect, storagemigration.TransferModeRelay, storagemigration.TransferModeAuto)
	}

	exportBackends := reportedExportBackends(sourceProvider)
	if !containsString(exportBackends, backend) {
		return fmt.Sprintf(
			"storage backend %q not supported by source provider %q (supported: %v); see ADR-0006",
			backend, sourceProvider.Name, exportBackends)
	}

	importBackends := reportedImportBackends(targetProvider)
	if !containsString(importBackends, backend) {
		return fmt.Sprintf(
			"storage backend %q not supported by target provider %q (supported: %v); see ADR-0006",
			backend, targetProvider.Name, importBackends)
	}

	// "auto" is resolved later against both providers' modes; only an explicit
	// mode is gated here. Both providers must advertise the explicit mode.
	if mode != storagemigration.TransferModeAuto {
		sourceModes := reportedTransferModes(sourceProvider)
		if !containsString(sourceModes, mode) {
			return fmt.Sprintf(
				"transfer mode %q not supported by source provider %q (supported: %v); see ADR-0006",
				mode, sourceProvider.Name, sourceModes)
		}
		targetModes := reportedTransferModes(targetProvider)
		if !containsString(targetModes, mode) {
			return fmt.Sprintf(
				"transfer mode %q not supported by target provider %q (supported: %v); see ADR-0006",
				mode, targetProvider.Name, targetModes)
		}
	}

	return ""
}

// getProviderInstance retrieves a provider gRPC client
func (r *VMMigrationReconciler) getProviderInstance(ctx context.Context, provider *infrav1beta1.Provider) (contracts.Provider, error) {
	// Test hook: allow injecting a fake/counting provider without dialing gRPC.
	if r.providerInstanceFn != nil {
		return r.providerInstanceFn(ctx, provider)
	}

	// Resolve the provider client
	providerClient, err := r.RemoteResolver.GetProvider(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve provider: %w", err)
	}

	return providerClient, nil
}

// updateStatus updates the migration status with retry on conflicts
func (r *VMMigrationReconciler) updateStatus(ctx context.Context, migration *infrav1beta1.VMMigration) error {
	logger := logging.FromContext(ctx)

	// Retry with exponential backoff on conflicts
	maxRetries := 3
	baseDelay := 100 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		err := r.Status().Update(ctx, migration)
		if err == nil {
			return nil
		}

		// If it's a conflict error, retry after a delay
		if errors.IsConflict(err) {
			if attempt < maxRetries-1 {
				delay := baseDelay * time.Duration(1<<uint(attempt)) // Exponential backoff
				logger.V(1).Info("Status update conflict, retrying",
					"attempt", attempt+1,
					"maxRetries", maxRetries,
					"delay", delay)
				time.Sleep(delay)

				// Refresh the object before retrying
				fresh := &infrav1beta1.VMMigration{}
				if err := r.Get(ctx, types.NamespacedName{
					Name:      migration.Name,
					Namespace: migration.Namespace,
				}, fresh); err != nil {
					logger.Error(err, "Failed to refresh VMMigration")
					return err
				}

				// Copy status to fresh object
				fresh.Status = migration.Status
				*migration = *fresh
				continue
			}
		}

		logger.Error(err, "Failed to update VMMigration status")
		return err
	}

	return fmt.Errorf("failed to update status after %d attempts", maxRetries)
}

// generateStorageURL generates a storage URL for the migration
func (r *VMMigrationReconciler) generateStorageURL(ctx context.Context, migration *infrav1beta1.VMMigration, stage string) (string, error) {
	// If no storage is configured, return an error
	if migration.Spec.Storage == nil {
		return "", fmt.Errorf("storage configuration is required for migration")
	}

	storageConfig := migration.Spec.Storage

	// Build URL based on storage type.
	switch storageConfig.Type {
	case "pvc", "":
		// PVC stages a qcow2 (the legacy pod-side path).
		migrationPath := fmt.Sprintf("vmmigrations/%s/%s/%s.qcow2",
			migration.Namespace, migration.Name, stage)

		// Get the PVC name from status (set during validation phase)
		pvcName := migration.Status.StoragePVCName
		if pvcName == "" {
			return "", fmt.Errorf("storage PVC name not set in migration status")
		}

		// PVC URL format: pvc://<pvc-name>/<path>
		// Provider pods have PVCs mounted at /mnt/migration-storage/<pvc-name>
		return fmt.Sprintf("pvc://%s/%s", pvcName, migrationPath), nil

	case storagemigration.BackendS3:
		// S3 stages the SOURCE's native format (ADR-0006 D4). For the Slice 1
		// vSphere → S3 → libvirt path that is a vmdk; the target converts on
		// import. Object key: <prefix>vmmigrations/<ns>/<name>/<stage>.vmdk.
		if storageConfig.S3 == nil || storageConfig.S3.Bucket == "" {
			return "", fmt.Errorf("s3 storage configuration (bucket) is required")
		}
		key := fmt.Sprintf("vmmigrations/%s/%s/%s.vmdk",
			migration.Namespace, migration.Name, stage)
		prefix := strings.Trim(storageConfig.S3.Prefix, "/")
		if prefix != "" {
			key = prefix + "/" + key
		}
		// s3://bucket/<prefix>/<key>
		return fmt.Sprintf("s3://%s/%s", storageConfig.S3.Bucket, key), nil

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
	// Provider controller always mounts PVCs at /mnt/migration-storage/<pvc-name>
	// If user specified a custom base path, use it, but always append PVC name
	basePath := "/mnt/migration-storage"
	if migration.Spec.Storage.PVC != nil && migration.Spec.Storage.PVC.MountPath != "" {
		basePath = migration.Spec.Storage.PVC.MountPath
	}
	mountPath := fmt.Sprintf("%s/%s", basePath, pvcName)

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

// cleanupAllowed reports whether the controller may remove migration-created
// artifacts for the given migration.
//
// The source snapshot the migration creates is an internal artifact left on the
// user's LIVE source VM; it is not something the user asked to keep, so we
// remove it at any terminal state (Ready or Failed) by default. The only way to
// retain it is to opt out explicitly with CleanupPolicy=Never. Always and
// OnSuccess both permit cleanup here: at a terminal state OnSuccess and Always
// are equivalent for this internal snapshot (a snapshot left on the source VM
// has no value after the migration has terminated either way). The distinction
// between Always and OnSuccess is reserved for future artifact classes; PR-1
// only governs the snapshot.
func cleanupAllowed(m *infrav1beta1.VMMigration) bool {
	// Allowed unless the user explicitly opted out with CleanupPolicy=Never.
	return m.Spec.Options == nil || m.Spec.Options.CleanupPolicy != infrav1beta1.CleanupPolicyNever
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

	// If the provider returned a task, AWAIT it to completion before returning.
	// A fire-and-forget delete (the previous behavior) orphans the task on CR
	// deletion: handleDeletion calls this then removes the finalizer, so the
	// async snapshot delete never lands and the snapshot accumulates on the
	// source VM. We poll with a bounded wait, mirroring the IsTaskComplete /
	// TaskStatus await pattern in handleExportingPhase.
	if taskRef != "" {
		logger.Info("Snapshot deletion task started, waiting for completion", "task_id", taskRef)

		waitErr := wait.PollUntilContextTimeout(ctx, 3*time.Second, 2*time.Minute, true,
			func(pollCtx context.Context) (bool, error) {
				done, pollErr := providerInstance.IsTaskComplete(pollCtx, taskRef)
				if pollErr != nil {
					// Treat a transient poll error as "keep polling" rather than
					// aborting the wait; the bounded timeout still caps total time.
					logger.V(1).Info("Transient error polling snapshot delete task, retrying",
						"task_id", taskRef, "error", pollErr.Error())
					return false, nil
				}
				return done, nil
			})
		if waitErr != nil {
			return fmt.Errorf("await snapshot delete task %s: %w", taskRef, waitErr)
		}

		// Task reported complete: verify it did not complete with an error.
		taskStatus, statusErr := providerInstance.TaskStatus(ctx, taskRef)
		if statusErr != nil {
			return fmt.Errorf("get snapshot delete task %s status: %w", taskRef, statusErr)
		}
		if taskStatus.Error != "" {
			return fmt.Errorf("snapshot delete task %s failed: %s", taskRef, taskStatus.Error)
		}
	}

	// Idempotency latch: the snapshot is gone, so clear the recorded ID. A
	// re-reconcile (or handleDeletion) then skips deleting a now-absent snapshot
	// rather than issuing a second delete against a stale ID.
	migration.Status.SnapshotID = ""

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
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 3, // Limit concurrent reconciliations to prevent API server overload
		}).
		Complete(r)
}
