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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	conditions "github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// Reason label specific to the VMSnapshot reconciler. Shared reasons
// (errReasonAddFinalizer, errReasonGetVM) are declared in
// virtualmachine_controller.go and reused here for the same operational
// meaning.
const errReasonGetSnapshot = "get-snapshot"

// snapshotReasonUnsupportedByProvider is the condition reason and Warning
// event reason used when capability enforcement (issue #176) blocks a
// snapshot because the resolved provider reports it cannot perform the
// requested operation (no snapshot support, or no memory-snapshot support
// for a memory-inclusive request). Only emitted when
// EnforceCapabilities is true and the provider implements
// contracts.CapabilityReporter.
const snapshotReasonUnsupportedByProvider = "UnsupportedByProvider"

// snapshotCreateNotIssuedMessage is the status message and Creating condition
// message of a snapshot found in the Creating phase with no SnapshotCreate
// result recorded (no task, snapshot id or creation time), which is returned
// to the initial phase so its create is issued.
const snapshotCreateNotIssuedMessage = "Snapshot create was not issued to the provider; retrying"

// snapshotCreateRetryingMessage is the status message and Ready condition
// message of a routed (clustered) snapshot create that did not reach a
// definite outcome and is retried. It is constant; the provider's answer is in
// the snapshotReasonCreateRetrying Warning event and the manager log.
const snapshotCreateRetryingMessage = "Snapshot creation did not reach a definite outcome on the provider and will be retried; " +
	"the provider's answer is in the SnapshotCreateRetrying event"

// snapshotReasonCreateRetrying is the Warning event reason recorded when a
// routed snapshot create starts being retried.
const snapshotReasonCreateRetrying = "SnapshotCreateRetrying"

// VMSnapshotReconciler reconciles a VMSnapshot object
type VMSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RemoteResolver *remote.Resolver
	Recorder       record.EventRecorder
	metrics        *metrics.ReconcileMetrics

	// APIReader reads straight from the API server, bypassing the informer
	// cache (the manager's GetAPIReader). The VMSnapshot is re-read through it
	// right before its SnapshotCreate (confirmSnapshotNotCreated), so a create
	// is never sent from a cache that does not show an earlier create's result
	// yet. Nil falls back to Client, which only unit tests rely on.
	APIReader client.Reader

	// EnforceCapabilities, when true, gates the snapshot CREATE path on the
	// provider's self-reported capabilities (issue #176). When false (the
	// default) the create path is byte-for-byte unchanged. See the gate in
	// createSnapshot.
	EnforceCapabilities bool

	// providerInstanceFn, when non-nil, overrides provider-instance resolution.
	// Production leaves it nil and resolves a real gRPC client via
	// RemoteResolver; tests inject a fake provider here to exercise the create
	// path without standing up a gRPC server.
	providerInstanceFn func(ctx context.Context, provider *infrav1beta1.Provider) (contracts.Provider, error)
}

// NewVMSnapshotReconciler creates a new VMSnapshot reconciler.
//
// enforceCapabilities mirrors the manager's --enforce-provider-capabilities
// flag (issue #176); when true, the snapshot CREATE path is gated on the
// provider's self-reported capabilities. Pass false to preserve the
// pre-#176 behaviour exactly.
func NewVMSnapshotReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	remoteResolver *remote.Resolver,
	recorder record.EventRecorder,
	enforceCapabilities bool,
) *VMSnapshotReconciler {
	return &VMSnapshotReconciler{
		Client: client,
		Scheme: scheme,

		RemoteResolver:      remoteResolver,
		Recorder:            recorder,
		metrics:             metrics.NewReconcileMetrics("VMSnapshot"),
		EnforceCapabilities: enforceCapabilities,
	}
}

//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsnapshots,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsnapshots/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmsnapshots/finalizers,verbs=update
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers;vmclasses;vmimages,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop.
//
// Observability: per-call timer + outcome inference via deferred block
// emits `virtrigaud_manager_reconcile_total{kind="VMSnapshot",outcome=...}`
// and `virtrigaud_manager_reconcile_duration_seconds{kind="VMSnapshot"}`.
//
// Named return values (`result`, `retErr`) are required by the deferred
// outcome-inference block — do not change the signature without updating
// the defer.
//
// Fixes issue #105: prior implementation used `defer timer.Finish(
// metrics.OutcomeSuccess)` (argument captured immediately) plus explicit
// `timer.Finish(metrics.OutcomeError)` on error paths. Errored reconciles
// recorded TWO samples per reconcile. New pattern records exactly one.
func (r *VMSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("VMSnapshot")
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
	ctx = logging.WithCorrelationID(ctx, fmt.Sprintf("vmsnapshot-%s", req.Name))
	logger := logging.FromContext(ctx)

	logger.Info("Reconciling VMSnapshot", "snapshot", req.NamespacedName)

	// Fetch the VMSnapshot instance
	snapshot := &infrav1beta1.VMSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, snapshot); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("VMSnapshot not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get VMSnapshot")
		metrics.RecordError(errReasonGetSnapshot, metrics.ComponentManager)
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
			metrics.RecordError(errReasonAddFinalizer, metrics.ComponentManager)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Get the referenced VM
	vm := &infrav1beta1.VirtualMachine{}
	vmKey := client.ObjectKey{
		Namespace: snapshot.Namespace,
		Name:      snapshot.Spec.VMRef.Name,
	}
	vmErr := r.Get(ctx, vmKey, vm)
	vmGone := apierrors.IsNotFound(vmErr)
	if vmErr != nil && !vmGone {
		// A failed read says nothing about the VM: nothing is recorded, and
		// the read is retried.
		logger.Error(vmErr, "Failed to get referenced VM", "vm", snapshot.Spec.VMRef.Name)
		metrics.RecordError(errReasonGetVM, metrics.ComponentManager)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// A Ready snapshot's retention policy applies whether or not its VM still
	// exists or has a provider id: deleteOnVMDelete fires once the VM is gone
	// (or being deleted), and maxAge whatever the VM's state.
	if snapshot.Status.Phase == infrav1beta1.SnapshotPhaseReady {
		switch {
		case vmGone:
			vm = nil
		case snapshotOfOtherVM(snapshot, vm):
			// A VM re-created under the snapshot's VM name is not its VM:
			// for retention, the snapshot's VM is gone.
			logger.Info("The VM named by spec.vmRef is not the one this snapshot was taken of",
				"vm_uid", vm.UID, "snapshot_vm_uid", snapshot.Status.VMUID)
			vm = nil
		case backfillSnapshotBinding(snapshot, vm):
			// Status update errors are intentionally ignored; the binding is
			// recorded again on the next reconcile.
			_ = r.updateStatus(ctx, snapshot)
		}
		return r.handleRetention(ctx, snapshot, vm)
	}
	if vmGone {
		return r.recordSnapshotVMNotFound(ctx, snapshot), nil
	}

	// Add VM context
	ctx = logging.WithVM(ctx, vm.Namespace, vm.Name)
	logger = logging.FromContext(ctx)

	// Check VM status
	if vm.Status.ID == "" {
		logger.Info("VM not yet provisioned, waiting")
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
			metav1.ConditionTrue, infrav1beta1.VMSnapshotReasonCreating,
			"Waiting for VM to be provisioned")
		// Status update errors are intentionally ignored to avoid blocking reconciliation
		_ = r.updateStatus(ctx, snapshot)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Handle snapshot lifecycle based on phase
	switch snapshot.Status.Phase {
	case "":
		// Initialize snapshot creation
		return r.createSnapshot(ctx, snapshot, vm)
	case infrav1beta1.SnapshotPhaseCreating:
		// Check if snapshot creation is complete
		return r.checkSnapshotCreation(ctx, snapshot, vm)
	case infrav1beta1.SnapshotPhaseReady:
		// Snapshot is ready, check for retention policy
		return r.handleRetention(ctx, snapshot, vm)
	case infrav1beta1.SnapshotPhaseFailed:
		// Handle failed snapshots
		logger.Info("Snapshot is in failed state", "message", snapshot.Status.Message)
		return ctrl.Result{RequeueAfter: snapshotFailedRecheckInterval}, nil
	default:
		// Unknown phase
		logger.Info("Unknown snapshot phase", "phase", snapshot.Status.Phase)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// createSnapshot initiates snapshot creation
func (r *VMSnapshotReconciler) createSnapshot(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	logger.Info("Creating VM snapshot")

	// Every failure before the SnapshotCreate RPC below (provider lookup,
	// binding, provider client) leaves the snapshot in the initial phase, so
	// the create is retried unchanged; the capability gate fails it. The
	// Creating phase is persisted only with the RPC's outcome:
	// checkSnapshotCreation reads Creating with no task as a finished
	// synchronous create.

	// Get the provider for this VM. A Provider in another namespace must
	// select this one (spec.consumerNamespaceSelector); the refusal is
	// recorded without moving the phase, so the create is retried unchanged
	// once the grant exists.
	provider, err := getVMProvider(ctx, r.Client, vm)
	if isConsumerNotAllowed(err) {
		return r.refuseSnapshotConsumer(ctx, snapshot, err), nil
	}
	if err != nil {
		logger.Error(err, "Failed to get provider", "provider", vm.Spec.ProviderRef.Name)
		return r.retrySnapshotCreate(ctx, snapshot, fmt.Sprintf("Failed to get provider: %v", err)), nil
	}

	// Address the VM (ADR-0007 Addendum A, A1) before resolving a client for
	// the Provider: a clustered VM with no confirmed host binding — or a VM
	// whose spec.providerRef no longer names the Provider it is bound through —
	// is never sent a per-VM call: wait.
	ref, err := vmRefFor(vm, provider)
	if err != nil {
		return r.waitForVMBinding(ctx, snapshot, err), nil
	}

	// Get provider instance
	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		// The resolver's error stays in the log (it can name a TLS Secret or
		// a Service address).
		logger.Error(err, "Failed to get provider instance")
		return r.retrySnapshotCreate(ctx, snapshot, "Failed to get provider instance: "+providerResolveFailedDetail), nil
	}

	// Build snapshot create request
	req := r.buildSnapshotCreateRequest(snapshot, ref)

	// Capability gate (issue #176). When enforcement is enabled and the
	// provider reports capabilities, refuse a snapshot the provider declares
	// it cannot perform BEFORE issuing the RPC. Fails open: a provider that
	// does not implement CapabilityReporter is never blocked. No-op when
	// EnforceCapabilities is false.
	if blocked, res := r.gateSnapshotCreate(ctx, snapshot, providerInstance, req); blocked {
		return res, nil
	}

	// A SnapshotCreate is never sent from a stale read: a single-host provider
	// honors no request token, so a second create made from a cache that does
	// not show the first one's result yet would leave an untracked snapshot.
	if fresh, res, err := r.confirmSnapshotNotCreated(ctx, snapshot); !fresh {
		return res, err
	}

	// The snapshot is bound to this VM (by UID) and this Provider: it is
	// never deleted through another VM of the same name (see
	// recordSnapshotBinding).
	recordSnapshotBinding(snapshot, vm, provider)

	// The create is issued now: only from here on is the snapshot Creating.
	// The phase is persisted below together with the RPC's outcome (a task to
	// poll, Ready, or Failed), never on its own.
	snapshot.Status.Phase = infrav1beta1.SnapshotPhaseCreating
	snapshot.Status.Message = "Creating snapshot"
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
		metav1.ConditionTrue, infrav1beta1.VMSnapshotReasonCreating,
		"Snapshot creation initiated")

	// Call provider to create snapshot
	resp, err := providerInstance.SnapshotCreate(ctx, req)
	if err != nil && req.VM.HostID != "" && contracts.IsRetryable(err) {
		// A ROUTED (clustered) create without a definite outcome — its host
		// unavailable, its time budget run out while libvirtd may still be
		// writing the snapshot, or another job on the VM still running — is
		// retried, never failed: a failed VMSnapshot would leave a snapshot
		// that completes afterwards untracked on the host. The provider's
		// routed create is idempotent per request (request_token, the
		// VMSnapshot's uid), so the retry finds that snapshot instead of making
		// a second one. A disk-dependents check that could not run
		// (VM_DISK_CHECK_FAILED, #358) is retried here too; a refusal because
		// another VM depends on the disk (VM_DISK_IN_USE, a Conflict) fails the
		// create, as on a single host. Single-host failures are unchanged.
		logger.Info("Snapshot creation did not reach a definite outcome; retrying", "error", err.Error())
		if c := meta.FindStatusCondition(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady); c == nil ||
			c.Message != snapshotCreateRetryingMessage {
			r.Recorder.Event(snapshot, corev1.EventTypeWarning, snapshotReasonCreateRetrying,
				"Snapshot creation will be retried: "+rpcFailureDetail(err))
		}
		return r.retrySnapshotCreate(ctx, snapshot, snapshotCreateRetryingMessage), nil
	}
	if err != nil {
		logger.Error(err, "Failed to create snapshot")
		detail := rpcFailureDetail(err)
		snapshot.Status.Phase = infrav1beta1.SnapshotPhaseFailed
		snapshot.Status.Message = "Failed to create snapshot: " + detail
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
			metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonProviderError,
			"Snapshot creation failed: "+detail)
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
			metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonProviderError,
			"Snapshot creation failed: "+detail)
		r.Recorder.Event(snapshot, "Warning", "SnapshotFailed", "Failed to create snapshot: "+detail)
		// Status update errors are intentionally ignored to avoid blocking reconciliation
		_ = r.updateStatus(ctx, snapshot)
		return ctrl.Result{RequeueAfter: snapshotFailedRecheckInterval}, nil
	}

	// Update status with snapshot information
	snapshot.Status.SnapshotID = resp.SnapshotId
	snapshot.Status.CreationTime = &metav1.Time{Time: time.Now()}

	// Check if there's a task to monitor
	if resp.Task != nil && resp.Task.ID != "" {
		snapshot.Status.TaskRef = resp.Task.ID
		logger.Info("Snapshot creation task started", "task_id", resp.Task.ID)
	} else {
		// Snapshot was created synchronously
		snapshot.Status.Phase = infrav1beta1.SnapshotPhaseReady
		snapshot.Status.Message = "Snapshot created successfully"
		snapshot.Status.TaskRef = ""

		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
			metav1.ConditionTrue, infrav1beta1.VMSnapshotReasonCreated,
			"Snapshot created successfully")
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
			metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonCreated,
			"Snapshot creation completed")

		r.Recorder.Event(snapshot, "Normal", "SnapshotReady", "Snapshot created successfully")
		logger.Info("Snapshot created successfully", "snapshot_id", resp.SnapshotId)
	}

	if err := r.updateStatus(ctx, snapshot); err != nil {
		return ctrl.Result{}, err
	}

	// If there's a task, requeue to check status
	if snapshot.Status.TaskRef != "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Snapshot is ready: its retention is checked next after the retention
	// interval. Never requeue at once after the write that ends a create: the
	// informer cache may not show it yet, and a reconcile that read the
	// snapshot as not created would create it again.
	return ctrl.Result{RequeueAfter: snapshotRetentionCheckInterval}, nil
}

// snapshotFailedRecheckInterval is how often a Failed snapshot is re-checked.
const snapshotFailedRecheckInterval = 5 * time.Minute

// snapshotStaleReadRetryInterval is the retry of a create skipped because the
// cached VMSnapshot was older than the API server's (confirmSnapshotNotCreated).
const snapshotStaleReadRetryInterval = 2 * time.Second

// confirmSnapshotNotCreated re-reads the VMSnapshot from the API server (the
// uncached APIReader) right before its SnapshotCreate, and allows the create
// only if the live object is the one this reconcile read — same UID and
// resourceVersion — and still records no create (initial phase, no snapshot id
// or task). Otherwise nothing is sent: a VMSnapshot that is gone ends the
// reconcile, and a cache that is behind is retried shortly, from a fresh read.
// A read error is returned (retried with backoff, nothing sent).
func (r *VMSnapshotReconciler) confirmSnapshotNotCreated(ctx context.Context, snapshot *infrav1beta1.VMSnapshot) (bool, ctrl.Result, error) {
	live := &infrav1beta1.VMSnapshot{}
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(snapshot), live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, fmt.Errorf("re-read VMSnapshot %s/%s before its create: %w", snapshot.Namespace, snapshot.Name, err)
	}
	if live.UID != snapshot.UID || live.ResourceVersion != snapshot.ResourceVersion ||
		live.Status.Phase != "" || live.Status.SnapshotID != "" || live.Status.TaskRef != "" {
		logging.FromContext(ctx).Info("The cached VMSnapshot is not the live one; not creating the snapshot from it",
			"cachedResourceVersion", snapshot.ResourceVersion, "liveResourceVersion", live.ResourceVersion, "livePhase", live.Status.Phase)
		return false, ctrl.Result{RequeueAfter: snapshotStaleReadRetryInterval}, nil
	}
	return true, ctrl.Result{}, nil
}

// liveReader is the uncached reader for the pre-create re-read: APIReader, or
// Client when none was set (unit tests built as struct literals).
func (r *VMSnapshotReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// checkSnapshotCreation checks if snapshot creation is complete
func (r *VMSnapshotReconciler) checkSnapshotCreation(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	logger.Info("Checking snapshot creation progress", "task_ref", snapshot.Status.TaskRef)

	// Creating with no task, no snapshot id and no creation time records no
	// SnapshotCreate result at all: the phase was persisted before any RPC
	// was issued (older managers did so when the provider could not be
	// resolved). Never report that as Ready: return the snapshot to the
	// initial phase so the create is issued.
	if snapshot.Status.TaskRef == "" && snapshot.Status.SnapshotID == "" && snapshot.Status.CreationTime == nil {
		logger.Info("Snapshot is Creating but no create was recorded; retrying the create")
		snapshot.Status.Phase = ""
		snapshot.Status.Message = snapshotCreateNotIssuedMessage
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
			metav1.ConditionTrue, infrav1beta1.VMSnapshotReasonCreating, snapshotCreateNotIssuedMessage)
		// Status update errors are intentionally ignored to avoid blocking reconciliation
		_ = r.updateStatus(ctx, snapshot)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// For vSphere and other synchronous providers, if there's no task ref,
	// the snapshot is already complete
	if snapshot.Status.TaskRef == "" {
		snapshot.Status.Phase = infrav1beta1.SnapshotPhaseReady
		snapshot.Status.Message = "Snapshot created successfully"

		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
			metav1.ConditionTrue, infrav1beta1.VMSnapshotReasonCreated,
			"Snapshot created successfully")
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
			metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonCreated,
			"Snapshot creation completed")

		r.Recorder.Event(snapshot, "Normal", "SnapshotReady", "Snapshot created successfully")

		if err := r.updateStatus(ctx, snapshot); err != nil {
			return ctrl.Result{}, err
		}

		// Ready: the retention is checked next after the retention interval
		// (never an immediate requeue after the write that ends a create).
		return ctrl.Result{RequeueAfter: snapshotRetentionCheckInterval}, nil
	}

	// Get the provider to check task status (it must still be usable from
	// this namespace: spec.consumerNamespaceSelector).
	provider, err := getVMProvider(ctx, r.Client, vm)
	if isConsumerNotAllowed(err) {
		return r.refuseSnapshotConsumer(ctx, snapshot, err), nil
	}
	if err != nil {
		logger.Error(err, "Failed to get provider")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// The task belongs to the Provider the VM is bound through; never poll it
	// on another one.
	if err := checkVMProvider(vm, provider); err != nil {
		logger.Info("VM is not bound through the Provider its spec.providerRef names; not polling the snapshot task", "error", err.Error())
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
			metav1.ConditionTrue, vmRefErrorReason(err), err.Error())
		// Status update errors are intentionally ignored to avoid blocking reconciliation
		_ = r.updateStatus(ctx, snapshot)
		return ctrl.Result{RequeueAfter: providerRefMismatchRetryInterval}, nil
	}

	// Get provider instance
	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		logger.Error(err, "Failed to get provider instance")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check task status
	taskStatus, err := providerInstance.TaskStatus(ctx, snapshot.Status.TaskRef)
	if err != nil {
		logger.Error(err, "Failed to check task status")
		// Don't fail immediately, retry
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if taskStatus.IsCompleted {
		if taskStatus.Error != "" {
			// Task failed. The task's error is the provider's text: made
			// safe before it reaches status and events.
			logger.Info("Snapshot creation task failed", "task_ref", snapshot.Status.TaskRef, "error", taskStatus.Error)
			detail := sanitizeProviderDetail(errors.New(taskStatus.Error))
			snapshot.Status.Phase = infrav1beta1.SnapshotPhaseFailed
			snapshot.Status.Message = "Snapshot creation failed: " + detail
			snapshot.Status.TaskRef = ""

			k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
				metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonProviderError,
				"Snapshot creation failed: "+detail)
			k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
				metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonProviderError,
				"Snapshot creation failed")

			r.Recorder.Event(snapshot, "Warning", "SnapshotFailed", "Snapshot creation failed: "+detail)
		} else {
			// Task succeeded
			snapshot.Status.Phase = infrav1beta1.SnapshotPhaseReady
			snapshot.Status.Message = "Snapshot created successfully"
			snapshot.Status.TaskRef = ""

			k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
				metav1.ConditionTrue, infrav1beta1.VMSnapshotReasonCreated,
				"Snapshot created successfully")
			k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
				metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonCreated,
				"Snapshot creation completed")

			r.Recorder.Event(snapshot, "Normal", "SnapshotReady", "Snapshot created successfully")
		}

		if err := r.updateStatus(ctx, snapshot); err != nil {
			return ctrl.Result{}, err
		}

		// Ready or Failed: taken up again from the new phase after that
		// phase's own interval (the retention check, or the failed-snapshot
		// recheck) — never at once after the write that ends a create.
		if snapshot.Status.Phase == infrav1beta1.SnapshotPhaseFailed {
			return ctrl.Result{RequeueAfter: snapshotFailedRecheckInterval}, nil
		}
		return ctrl.Result{RequeueAfter: snapshotRetentionCheckInterval}, nil
	}

	// Task still in progress
	logger.Info("Snapshot creation task still in progress")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// snapshotReasonVMNotFound is the Ready condition reason of a VMSnapshot whose
// referenced VirtualMachine does not exist.
const snapshotReasonVMNotFound = "VMNotFound"

// snapshotVMNotFoundMessage is the status message and Ready condition message
// of a VMSnapshot (not Ready) whose referenced VirtualMachine does not exist.
// It is constant, so a re-check writes nothing.
const snapshotVMNotFoundMessage = "Referenced VM not found; no snapshot is taken until it exists"

// recordSnapshotVMNotFound records that the VMSnapshot's referenced VM does not
// exist (Ready=False/VMNotFound, constant message; written only when that
// changes the status) and re-checks after 30 s.
func (r *VMSnapshotReconciler) recordSnapshotVMNotFound(ctx context.Context, snapshot *infrav1beta1.VMSnapshot) ctrl.Result {
	logging.FromContext(ctx).Info("Referenced VM not found", "vm", snapshot.Spec.VMRef.Name)
	before := snapshot.Status.DeepCopy()
	snapshot.Status.Message = snapshotVMNotFoundMessage
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
		metav1.ConditionFalse, snapshotReasonVMNotFound, snapshotVMNotFoundMessage)
	if !equality.Semantic.DeepEqual(before, &snapshot.Status) {
		// Status update errors are intentionally ignored to avoid blocking reconciliation
		_ = r.updateStatus(ctx, snapshot)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}
}

// handleRetention applies a Ready snapshot's retention policy. vm is the
// referenced VirtualMachine, nil when it no longer exists:
//
//   - maxAge: a snapshot older than maxAge (by its creation time) is deleted,
//     whatever its VM's state;
//   - deleteOnVMDelete: the snapshot is deleted once its VM is gone or being
//     deleted. A VM being deleted still exists, so the VMSnapshot's finalizer
//     deletes the snapshot through the provider — which also lets a libvirt
//     VM, whose delete is refused while snapshot metadata remains, be
//     deleted; a VM that is already gone took its snapshots with it, and the
//     finalizer makes no provider call.
func (r *VMSnapshotReconciler) handleRetention(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, vm *infrav1beta1.VirtualMachine) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// Check retention policy. A snapshot with no creation time has no age: an
	// older manager could mark one Ready without any create (see
	// checkSnapshotCreation); it is left for the operator, never expired.
	if snapshot.Spec.RetentionPolicy != nil && snapshot.Spec.RetentionPolicy.MaxAge != nil &&
		snapshot.Status.CreationTime != nil {
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
	// A VM being detached (orphan-on-delete) keeps running, snapshots and
	// all, on the hypervisor: its snapshots are not deleted with it.
	if snapshot.Spec.RetentionPolicy != nil && snapshot.Spec.RetentionPolicy.DeleteOnVMDelete &&
		(vm == nil || (!vm.DeletionTimestamp.IsZero() && !hasOrphanOnDeleteAnnotation(vm))) {
		logger.Info("Referenced VM is gone or being deleted, deleting snapshot (retentionPolicy.deleteOnVMDelete)")
		if err := r.Delete(ctx, snapshot); err != nil {
			logger.Error(err, "Failed to delete snapshot after VM deletion")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Check again later
	return ctrl.Result{RequeueAfter: snapshotRetentionCheckInterval}, nil
}

// snapshotRetentionCheckInterval is how often a Ready snapshot's retention
// policy is re-checked.
const snapshotRetentionCheckInterval = time.Hour

// handleDeletion handles snapshot deletion
func (r *VMSnapshotReconciler) handleDeletion(ctx context.Context, snapshot *infrav1beta1.VMSnapshot) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	logger.Info("Deleting VM snapshot")

	// Update phase — once: a delete the provider refused or failed is
	// retried without announcing a new deletion each time.
	if !snapshotDeleteRetrying(snapshot) {
		snapshot.Status.Phase = infrav1beta1.SnapshotPhaseDeleting
		k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionDeleting,
			metav1.ConditionTrue, infrav1beta1.VMSnapshotReasonDeleting,
			"Snapshot deletion initiated")

		_ = r.updateStatus(ctx, snapshot) //nolint:errcheck // Status update errors logged elsewhere

		r.Recorder.Event(snapshot, "Normal", "SnapshotDeleting", "Started snapshot deletion")
	}

	// Get the VM to find the provider
	vm := &infrav1beta1.VirtualMachine{}
	vmKey := client.ObjectKey{
		Namespace: snapshot.Namespace,
		Name:      snapshot.Spec.VMRef.Name,
	}

	// Get the VM
	vmNotFound := false
	if err := r.Get(ctx, vmKey, vm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// VM was already deleted, skip provider call
			logger.Info("VM not found, skipping provider snapshot deletion", "vm", snapshot.Spec.VMRef.Name)
			vmNotFound = true
		} else {
			logger.Error(err, "Failed to get VM for snapshot deletion")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	// removed is the Normal event recorded once the finalizer is released
	// ("" = none: nothing was confirmed deleted).
	removed := "VMSnapshot removed; there was no provider snapshot to delete"

	// A VM re-created under the snapshot's VM name is another VM: a delete
	// sent through it could reach another snapshot of the same name. No
	// provider call; released with a Warning.
	if !vmNotFound && snapshot.Status.SnapshotID != "" && snapshotOfOtherVM(snapshot, vm) {
		msg := fmt.Sprintf("Removing the VMSnapshot without deleting snapshot %q: the VM %q is not the one it was taken of "+
			"(it was re-created under the same name), so no provider call is made; if the original VM still exists on the "+
			"hypervisor, the snapshot is left there", snapshot.Status.SnapshotID, snapshot.Spec.VMRef.Name)
		logger.Info("The VM named by spec.vmRef is not the snapshot's VM; removing finalizer without a provider call",
			"snapshot_id", snapshot.Status.SnapshotID, "vm_uid", vm.UID, "snapshot_vm_uid", snapshot.Status.VMUID)
		r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
		return r.releaseSnapshotFinalizer(ctx, snapshot, "")
	}

	// A recorded snapshot whose VM no longer exists (or has no provider id)
	// cannot be deleted by anyone: the VMSnapshot is released, but never
	// with a claim that there was nothing to delete — the VM may still run
	// on the hypervisor (e.g. detached with orphan-on-delete), snapshot and
	// all.
	if snapshot.Status.SnapshotID != "" && (vmNotFound || vm.Status.ID == "") {
		msg := fmt.Sprintf("Removing the VMSnapshot although snapshot %q was not deleted: its VM %q no longer exists "+
			"(or has no provider id), so nothing can delete it; if the VM still exists on the hypervisor (e.g. it was "+
			"detached with orphan-on-delete), the snapshot is left there", snapshot.Status.SnapshotID, snapshot.Spec.VMRef.Name)
		logger.Info("The snapshot's VM is gone; removing finalizer, the snapshot may be left on the hypervisor",
			"snapshot_id", snapshot.Status.SnapshotID, "vm", snapshot.Spec.VMRef.Name)
		r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
		removed = ""
	}

	// Only call provider if VM still exists and we have a snapshot ID
	if !vmNotFound && snapshot.Status.SnapshotID != "" && vm.Status.ID != "" {
		// Get the provider. A Provider in another namespace that does not select
		// this one (spec.consumerNamespaceSelector) is never used.
		provider, err := getVMProvider(ctx, r.Client, vm)

		// Address the VM (ADR-0007 Addendum A, A1) before resolving a client for
		// the Provider. An ungranted (or revoked) cross-namespace Provider, a
		// clustered VM with no confirmed host binding, or a VM whose
		// spec.providerRef no longer names the Provider it is bound through, is
		// never sent a per-VM call. The snapshot is then still on the
		// hypervisor: the finalizer stays and the delete is retried with
		// backoff, unless force-delete says to leave the snapshot behind.
		// A Provider that cannot be read, or that has no usable client (its
		// runtime not Running, TLS or dial failing — every provider rollout),
		// was sent nothing either and is held the same way (unavailableErr).
		var refErr, unavailableErr error
		var ref contracts.VMRef
		var providerInstance contracts.Provider
		providerGone := false
		switch {
		case isConsumerNotAllowed(err):
			refErr = err
		case apierrors.IsNotFound(err):
			// The VM's Provider in its own namespace no longer exists
			// (one in another namespace that is missing is refused as
			// ConsumerNotAllowed, above): nothing can delete the
			// snapshot, and holding would wait forever. Released, as the
			// VirtualMachine and VMMigration controllers do, with a
			// Warning naming what may be left.
			providerGone = true
		case err != nil:
			unavailableErr = err
		case snapshotProviderMismatch(snapshot, provider):
			// The VM now goes through another Provider than the one the
			// snapshot was taken through: never sent there.
			msg := fmt.Sprintf("Removing the VMSnapshot without deleting snapshot %q: it was taken through Provider %s/%s, and "+
				"its VM now references Provider %s/%s, so no provider call is made; the snapshot may be left on the hypervisor",
				snapshot.Status.SnapshotID, snapshot.Status.BoundProvider.Namespace, snapshot.Status.BoundProvider.Name,
				provider.Namespace, provider.Name)
			logger.Info("The VM's Provider is not the one the snapshot was taken through; removing finalizer without a provider call",
				"snapshot_id", snapshot.Status.SnapshotID)
			r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
			return r.releaseSnapshotFinalizer(ctx, snapshot, "")
		default:
			ref, refErr = vmRefFor(vm, provider)
			if refErr == nil {
				providerInstance, unavailableErr = r.getProviderInstance(ctx, provider)
			}
		}
		if providerGone {
			msg := fmt.Sprintf("Removing the VMSnapshot although snapshot %q of VM %q was not deleted: the VM's Provider %q no "+
				"longer exists, so nothing can delete it; if the VM still exists on the hypervisor, the snapshot is left there",
				snapshot.Status.SnapshotID, vm.Status.ID, vm.Spec.ProviderRef.Name)
			logger.Info("The VM's Provider no longer exists; removing finalizer, the snapshot may be left on the hypervisor",
				"snapshot_id", snapshot.Status.SnapshotID, "vm_id", vm.Status.ID, "error", err.Error())
			r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
			removed = ""
		} else if unavailableErr != nil {
			if !hasSnapshotForceDeleteAnnotation(snapshot) {
				return r.retainForUnavailableSnapshotProvider(ctx, snapshot, unavailableErr), nil
			}
			// The cause stays in the log: resolver text can name a TLS
			// Secret or a Service address.
			msg := fmt.Sprintf("%s=true: removing the VMSnapshot although the manager could not reach the VM's Provider %q, "+
				"so snapshot %q of VM %q was not deleted; it is left on the hypervisor", forceDeleteAnnotation,
				vm.Spec.ProviderRef.Name, snapshot.Status.SnapshotID, vm.Status.ID)
			logger.Error(unavailableErr, "Cannot reach the provider to delete the snapshot but force-delete annotation is set; removing finalizer, the snapshot is left on the hypervisor",
				"snapshot_id", snapshot.Status.SnapshotID, "vm_id", vm.Status.ID, "annotation", forceDeleteAnnotation)
			r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
			removed = ""
		} else if refErr != nil {
			if !hasSnapshotForceDeleteAnnotation(snapshot) {
				return r.retainForUnaddressableSnapshotDelete(ctx, snapshot, refErr), nil
			}
			providerName := ""
			if provider != nil {
				providerName = provider.Name
			}
			msg := fmt.Sprintf("%s=true: removing the VMSnapshot although no provider call can be made for VM %q (%s), so "+
				"snapshot %q was not deleted (provider %s); it is left on the hypervisor. Detail: %v", forceDeleteAnnotation,
				vm.Status.ID, vmRefErrorReason(refErr), snapshot.Status.SnapshotID, providerName, refErr)
			logger.Info("No provider call can be made for the VM but force-delete annotation is set; removing finalizer, the snapshot is left on the hypervisor",
				"snapshot_id", snapshot.Status.SnapshotID, "vm_id", vm.Status.ID, "reason", vmRefErrorReason(refErr), "annotation", forceDeleteAnnotation)
			r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
			removed = ""
		} else {
			// Delete the snapshot via provider
			logger.Info("Calling provider to delete snapshot", "snapshot_id", snapshot.Status.SnapshotID, "vm_id", vm.Status.ID)
			_, err := providerInstance.SnapshotDelete(ctx, ref, snapshot.Status.SnapshotID)
			switch {
			case err == nil:
				logger.Info("Snapshot deleted successfully via provider")
				removed = "Snapshot deleted from the provider"
			case contracts.IsNotFound(err):
				logger.Info("The provider snapshot is already gone", "snapshot_id", snapshot.Status.SnapshotID)
				removed = "The provider snapshot was already gone"
			case hasSnapshotForceDeleteAnnotation(snapshot):
				// The operator opted out: release the finalizer although the
				// snapshot is still on the hypervisor, and say which one.
				msg := fmt.Sprintf("%s=true: removing the VMSnapshot although the provider did not delete snapshot %q of VM %q "+
					"(provider %s); it is left on the hypervisor. Provider detail: %s", forceDeleteAnnotation,
					snapshot.Status.SnapshotID, vm.Status.ID, provider.Name, rpcFailureDetail(err))
				logger.Error(err, "Provider snapshot delete failed but force-delete annotation is set; removing finalizer, the snapshot is left on the hypervisor",
					"snapshot_id", snapshot.Status.SnapshotID, "vm_id", vm.Status.ID, "provider", provider.Name, "annotation", forceDeleteAnnotation)
				r.Recorder.Event(snapshot, corev1.EventTypeWarning, eventReasonSnapshotLeftOnHypervisor, msg)
				removed = ""
			case contracts.IsVMDiskInUse(err):
				// The provider refused BEFORE changing anything: other VMs
				// on the hypervisor depend on the VM's disk (e.g. a libvirt
				// linked clone), and deleting the snapshot would rewrite
				// it. Keep the finalizer — releasing it would leave the
				// snapshot on the hypervisor with nothing tracking it —
				// say why, and re-check.
				return r.retainForBlockedSnapshotDelete(ctx, snapshot, err), nil
			default:
				// Any other failure (a host or provider error, an open
				// breaker, a check that could not run): the snapshot may
				// still be on the hypervisor, so the finalizer stays and the
				// delete is retried with backoff.
				return r.retainForFailedSnapshotDelete(ctx, snapshot, err), nil
			}
		}
	}

	return r.releaseSnapshotFinalizer(ctx, snapshot, removed)
}

// releaseSnapshotFinalizer removes the VMSnapshot finalizer and, when removed
// is not empty (the provider snapshot was confirmed deleted or absent),
// records it as a Normal SnapshotDeleted event.
func (r *VMSnapshotReconciler) releaseSnapshotFinalizer(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, removed string) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	controllerutil.RemoveFinalizer(snapshot, "snapshot.infra.virtrigaud.io/finalizer")
	if err := r.Update(ctx, snapshot); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		return ctrl.Result{}, err
	}

	if removed != "" {
		r.Recorder.Event(snapshot, "Normal", "SnapshotDeleted", removed)
	}
	logger.Info("VMSnapshot finalizer removed", "outcome", removed)

	return ctrl.Result{}, nil
}

// hasSnapshotForceDeleteAnnotation reports whether the VMSnapshot carries the
// force-delete escape hatch (the VirtualMachine's annotation,
// forceDeleteAnnotation, set to "true"): a snapshot delete that fails, is
// refused, or cannot be made then releases the finalizer, leaving the
// snapshot on the hypervisor (a SnapshotLeftOnHypervisor Warning names it).
func hasSnapshotForceDeleteAnnotation(snapshot *infrav1beta1.VMSnapshot) bool {
	return snapshot.Annotations[forceDeleteAnnotation] == "true"
}

// snapshotDeleteBlocked reports whether snapshot's delete was last refused by
// the provider (Ready=False/DeleteBlocked, retainForBlockedSnapshotDelete).
func snapshotDeleteBlocked(snapshot *infrav1beta1.VMSnapshot) bool {
	c := meta.FindStatusCondition(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == conditions.ReasonDeleteBlocked
}

// snapshotDeleteRetrying reports whether snapshot's delete already started and
// is being retried: refused (DeleteBlocked), failed (ProviderError) or not
// possible yet (a vmRefErrorReason) — Deleting=False, which only the retain
// paths set.
func snapshotDeleteRetrying(snapshot *infrav1beta1.VMSnapshot) bool {
	c := meta.FindStatusCondition(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionDeleting)
	return c != nil && c.Status == metav1.ConditionFalse
}

// eventReasonSnapshotLeftOnHypervisor is the Warning event reason recorded
// when force-delete removes a VMSnapshot whose provider snapshot could not be
// deleted.
const eventReasonSnapshotLeftOnHypervisor = "SnapshotLeftOnHypervisor"

// Backoff of a provider SnapshotDelete that failed or could not be made
// (retainForFailedSnapshotDelete, retainForUnaddressableSnapshotDelete).
const (
	// snapshotDeleteRetryMin is the first retry delay.
	snapshotDeleteRetryMin = 15 * time.Second
	// snapshotDeleteRetryMax caps the delay.
	snapshotDeleteRetryMax = 5 * time.Minute
)

// snapshotDeleteRetryAfter is the delay before the next attempt of a failing
// snapshot delete: the time since the first failure (the Deleting condition's
// last transition to False), so it roughly doubles per attempt, within
// [snapshotDeleteRetryMin, snapshotDeleteRetryMax].
func snapshotDeleteRetryAfter(snapshot *infrav1beta1.VMSnapshot, now time.Time) time.Duration {
	if !snapshotDeleteRetrying(snapshot) {
		return snapshotDeleteRetryMin
	}
	c := meta.FindStatusCondition(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionDeleting)
	d := now.Sub(c.LastTransitionTime.Time)
	switch {
	case d < snapshotDeleteRetryMin:
		return snapshotDeleteRetryMin
	case d > snapshotDeleteRetryMax:
		return snapshotDeleteRetryMax
	}
	return d
}

// retainForFailedSnapshotDelete keeps the finalizer of a VMSnapshot whose
// provider SnapshotDelete failed for any reason other than "already gone" or
// a VM_DISK_IN_USE refusal — an unreachable host, an open circuit breaker, a
// disk check that could not run: the snapshot may still be on the hypervisor
// (with the guest's RAM, for a memory snapshot), and a VM whose snapshot
// metadata remains cannot be undefined. It records Ready=False and
// Deleting=False with reason ProviderError and the way out, a Warning event
// when the failure is new or different, and retries with backoff
// (snapshotDeleteRetryAfter) until the delete succeeds or the force-delete
// annotation is set (holdSnapshotDelete).
func (r *VMSnapshotReconciler) retainForFailedSnapshotDelete(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, err error) ctrl.Result {
	retryAfter := snapshotDeleteRetryAfter(snapshot, time.Now())
	logging.FromContext(ctx).Error(err, "Provider snapshot delete failed; retaining finalizer and retrying",
		"snapshot_id", snapshot.Status.SnapshotID, "retryAfter", retryAfter.String())
	metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)

	// The provider's answer goes to the Warning event and the log, never into
	// the status: its text can differ on every attempt (a single-host error
	// can carry a connection's ephemeral port), and a status that changed on
	// every retry would make each retry write it.
	r.holdSnapshotDelete(ctx, snapshot, infrav1beta1.VMSnapshotReasonProviderError, snapshotDeleteFailedMessage,
		fmt.Sprintf("%s Provider detail: %s", snapshotDeleteFailedMessage, rpcFailureDetail(err)))
	return ctrl.Result{RequeueAfter: retryAfter}
}

// snapshotDeleteFailedMessage is the status message (and Ready / Deleting
// condition message) of a VMSnapshot whose provider SnapshotDelete failed.
// It is constant; the provider's answer is in the Warning event and the log.
var snapshotDeleteFailedMessage = fmt.Sprintf("Provider could not delete the snapshot; the VMSnapshot is kept (the snapshot may "+
	"still be on the hypervisor) and the delete is retried with a backoff of up to %s. Set %s=true to remove it anyway; the "+
	"provider's answer is in the SnapshotDeleteFailed event.", snapshotDeleteRetryMax, forceDeleteAnnotation)

// holdSnapshotDelete records a held delete of snapshot — Ready=False and
// Deleting=False with reason and msg — and, only when that changes the stored
// status, a SnapshotDeleteFailed Warning event with eventMsg and the status
// write. A retry of the same hold therefore writes nothing: msg must be the
// same on every retry of the same failure (no retry delay, timestamp or
// provider text in it), so the hold's backoff, not its own writes, paces the
// retries.
func (r *VMSnapshotReconciler) holdSnapshotDelete(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, reason, msg, eventMsg string) {
	before := snapshot.Status.DeepCopy()
	snapshot.Status.Message = msg
	for _, condType := range []string{infrav1beta1.VMSnapshotConditionReady, infrav1beta1.VMSnapshotConditionDeleting} {
		meta.SetStatusCondition(&snapshot.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: snapshot.Generation,
		})
	}
	if equality.Semantic.DeepEqual(before, &snapshot.Status) {
		return
	}
	r.Recorder.Event(snapshot, corev1.EventTypeWarning, "SnapshotDeleteFailed", eventMsg)
	// Status update errors are intentionally ignored to avoid blocking reconciliation.
	_ = r.updateStatus(ctx, snapshot)
}

// retainForUnaddressableSnapshotDelete keeps the finalizer of a VMSnapshot
// whose provider SnapshotDelete cannot be made at all: the VM still exists,
// but no provider call can be made for it (vmRefFor) — a cross-namespace
// Provider that does not, or no longer, allow this namespace; a clustered VM
// with no confirmed host binding (e.g. while it is re-placed); a
// spec.providerRef that no longer names the Provider it is bound through. The
// snapshot is then still on the hypervisor, and releasing the finalizer would
// leave it untracked. It records Ready=False and Deleting=False with
// vmRefErrorReason(err) and the way out, a Warning event, and retries with
// backoff (snapshotDeleteRetryAfter) until a call can be made, the VM is gone,
// or the force-delete annotation is set.
func (r *VMSnapshotReconciler) retainForUnaddressableSnapshotDelete(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, err error) ctrl.Result {
	retryAfter := snapshotDeleteRetryAfter(snapshot, time.Now())
	reason := vmRefErrorReason(err)
	logging.FromContext(ctx).Info("No provider call can be made for the VM; retaining the VMSnapshot finalizer and retrying",
		"snapshot_id", snapshot.Status.SnapshotID, "reason", reason, "retryAfter", retryAfter.String(), "error", err.Error())

	// err is the operator's own refusal (vmRefFor): no provider text, only
	// object names, a host and a reason, which change only when the objects
	// do. It is kept in the status, so a retry of the same refusal writes
	// nothing.
	msg := fmt.Sprintf("%s. The VMSnapshot is kept (the snapshot may still be on the hypervisor) and its delete is retried "+
		"with a backoff of up to %s. Set %s=true to remove it anyway, leaving the snapshot on the hypervisor. Detail: %v",
		vmRefWaitMessage(err), snapshotDeleteRetryMax, forceDeleteAnnotation, err)
	r.holdSnapshotDelete(ctx, snapshot, reason, msg, msg)
	return ctrl.Result{RequeueAfter: retryAfter}
}

// snapshotDeleteProviderUnavailableMessage is the status message (and Ready /
// Deleting condition message) of a VMSnapshot whose delete was not sent
// because the manager could not reach the VM's Provider
// (retainForUnavailableSnapshotProvider). It is constant and carries no
// resolver text.
var snapshotDeleteProviderUnavailableMessage = fmt.Sprintf("The manager cannot reach the VM's Provider, so the snapshot delete "+
	"was not sent; the VMSnapshot is kept (the snapshot may still be on the hypervisor) and the delete is retried with a backoff "+
	"of up to %s. Set %s=true to remove it anyway, leaving the snapshot on the hypervisor. The cause is in the manager log.",
	blockedRetryMax, forceDeleteAnnotation)

// retainForUnavailableSnapshotProvider keeps the finalizer of a VMSnapshot
// whose provider SnapshotDelete was not sent because the VM's Provider could
// not be read or resolved to a usable client (its runtime not Running or
// without an endpoint, TLS or gRPC setup failing — as during every provider
// rollout): the snapshot may still be on the hypervisor. It records Ready=False
// and Deleting=False with reason ProviderUnavailable and the constant
// snapshotDeleteProviderUnavailableMessage (the cause, which can name a TLS
// Secret or a Service address, goes to the manager log only), and retries with
// the blocked-VM backoff (blockedRetryBackoff: 15 s doubling to 5 min, from when
// the delete was first held) until the Provider answers, the VM is gone, or the
// force-delete annotation is set.
func (r *VMSnapshotReconciler) retainForUnavailableSnapshotProvider(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, err error) ctrl.Result {
	var since time.Time
	if snapshotDeleteRetrying(snapshot) {
		since = conditionSince(snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionDeleting)
	}
	retry := blockedRetryBackoff(since)
	logging.FromContext(ctx).Info("Cannot reach the VM's provider to delete the snapshot; retaining the VMSnapshot finalizer and retrying",
		"snapshot_id", snapshot.Status.SnapshotID, "retryAfter", retry.String(), "error", err.Error())
	metrics.RecordError(errReasonProviderResolve, metrics.ComponentManager)
	r.holdSnapshotDelete(ctx, snapshot, conditions.ReasonProviderUnavailable, snapshotDeleteProviderUnavailableMessage,
		snapshotDeleteProviderUnavailableMessage)
	return ctrl.Result{RequeueAfter: retry}
}

// retainForBlockedSnapshotDelete keeps the finalizer of a VMSnapshot whose
// provider SnapshotDelete was refused with VM_DISK_IN_USE (contracts.IsVMDiskInUse) — other VMs on the
// hypervisor depend on the VM's disk (e.g. linked clones backed by it), and
// deleting the snapshot would rewrite that disk underneath them. The provider
// changed nothing, so the snapshot stays on the hypervisor and tracked. It
// records Ready=False/DeleteBlocked (and Deleting=False) with the way out, a
// Warning event when the refusal is new, and re-checks every
// vmDeleteBlockedRetryInterval — the VirtualMachine's cadence — until the
// dependent VMs are gone or the force-delete annotation is set.
func (r *VMSnapshotReconciler) retainForBlockedSnapshotDelete(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, err error) ctrl.Result {
	logging.FromContext(ctx).Info("Provider refused to delete the snapshot because other VMs depend on the VM's disk; retaining finalizer",
		"snapshot_id", snapshot.Status.SnapshotID, "retryAfter", vmDeleteBlockedRetryInterval.String(), "error", err.Error())
	metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)

	msg := fmt.Sprintf("Provider refused to delete the snapshot: other VMs on its hypervisor depend on the VM's disk (e.g. linked clones of the VM), "+
		"and deleting the snapshot would rewrite it. Delete them first, or set %s=true to remove this VMSnapshot and leave the snapshot "+
		"on the hypervisor (re-checking every %s). Provider detail: %s",
		forceDeleteAnnotation, vmDeleteBlockedRetryInterval, rpcFailureDetail(err))
	if !snapshotDeleteBlocked(snapshot) {
		r.Recorder.Event(snapshot, corev1.EventTypeWarning, conditions.ReasonDeleteBlocked, msg)
	}
	before := snapshot.Status.DeepCopy()
	snapshot.Status.Message = msg
	for _, condType := range []string{infrav1beta1.VMSnapshotConditionReady, infrav1beta1.VMSnapshotConditionDeleting} {
		meta.SetStatusCondition(&snapshot.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonDeleteBlocked,
			Message:            msg,
			ObservedGeneration: snapshot.Generation,
		})
	}
	// A recheck of the same refusal changes nothing: don't write it again.
	// Status update errors are intentionally ignored to avoid blocking reconciliation.
	if !equality.Semantic.DeepEqual(before, &snapshot.Status) {
		_ = r.updateStatus(ctx, snapshot)
	}
	return ctrl.Result{RequeueAfter: vmDeleteBlockedRetryInterval}
}

// updateStatus updates the snapshot status
func (r *VMSnapshotReconciler) updateStatus(ctx context.Context, snapshot *infrav1beta1.VMSnapshot) error {
	if err := r.Status().Update(ctx, snapshot); err != nil {
		logger := logging.FromContext(ctx)
		logger.Error(err, "Failed to update VMSnapshot status")
		return err
	}
	return nil
}

// gateSnapshotCreate enforces provider capabilities for the snapshot CREATE
// path (issue #176). It returns blocked=true (with the ctrl.Result the
// caller should return) only when ALL of the following hold:
//   - r.EnforceCapabilities is true, AND
//   - providerInstance implements contracts.CapabilityReporter and its
//     GetCapabilities call succeeds, AND
//   - the provider reports it cannot satisfy the request: snapshots are
//     unsupported, or the request includes memory and memory snapshots are
//     unsupported.
//
// In all other cases it returns blocked=false and the snapshot proceeds:
//   - enforcement off → no-op (byte-for-byte unchanged path),
//   - provider is not a CapabilityReporter → FAIL OPEN (never block),
//   - GetCapabilities errors → FAIL OPEN (transient; let the RPC speak),
//   - provider reports the capability → proceed.
//
// When it blocks it sets the snapshot Phase to Failed, a Ready=False
// condition with reason UnsupportedByProvider, and records a Warning event,
// then persists status — so no provider RPC is issued.
func (r *VMSnapshotReconciler) gateSnapshotCreate(
	ctx context.Context,
	snapshot *infrav1beta1.VMSnapshot,
	providerInstance contracts.Provider,
	req contracts.SnapshotCreateRequest,
) (blocked bool, result ctrl.Result) {
	if !r.EnforceCapabilities {
		return false, ctrl.Result{}
	}

	logger := logging.FromContext(ctx)

	reporter, ok := providerInstance.(contracts.CapabilityReporter)
	if !ok {
		// Fail open: provider does not advertise capabilities.
		logger.V(1).Info("Capability enforcement on but provider does not report capabilities; allowing snapshot")
		return false, ctrl.Result{}
	}

	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		// Fail open: do not block on a transient capability-query failure.
		logger.V(1).Info("Capability enforcement on but GetCapabilities failed; allowing snapshot", "error", err.Error())
		return false, ctrl.Result{}
	}

	switch {
	case !caps.SupportsSnapshots:
		return true, r.blockSnapshot(ctx, snapshot,
			"Provider does not support snapshots")
	case req.IncludeMemory && !caps.SupportsMemorySnapshots:
		return true, r.blockSnapshot(ctx, snapshot,
			"Provider does not support memory-inclusive snapshots")
	default:
		return false, ctrl.Result{}
	}
}

// blockSnapshot marks the snapshot as failed because capability enforcement
// refused the operation (issue #176), records a Warning event, and persists
// status. The returned ctrl.Result intentionally does not requeue: the
// provider's capabilities are a property of the provider, so retrying the
// same request on the same provider would fail identically. A spec change or
// provider change re-triggers reconciliation via the watch.
func (r *VMSnapshotReconciler) blockSnapshot(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, message string) ctrl.Result {
	logger := logging.FromContext(ctx)
	logger.Info("Capability enforcement blocked snapshot", "message", message)

	snapshot.Status.Phase = infrav1beta1.SnapshotPhaseFailed
	snapshot.Status.Message = message
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
		metav1.ConditionFalse, snapshotReasonUnsupportedByProvider, message)
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
		metav1.ConditionFalse, snapshotReasonUnsupportedByProvider, message)
	r.Recorder.Event(snapshot, "Warning", snapshotReasonUnsupportedByProvider, message)

	// Status update errors are intentionally ignored to avoid blocking
	// reconciliation, matching the surrounding create-path error handling.
	_ = r.updateStatus(ctx, snapshot)
	return ctrl.Result{}
}

// getProviderInstance resolves a provider to a remote implementation
func (r *VMSnapshotReconciler) getProviderInstance(ctx context.Context, provider *infrav1beta1.Provider) (contracts.Provider, error) {
	// Test hook: allow injecting a fake provider without dialing gRPC.
	if r.providerInstanceFn != nil {
		return r.providerInstanceFn(ctx, provider)
	}

	// All providers are now remote
	if r.RemoteResolver == nil {
		return nil, fmt.Errorf("no remote resolver available")
	}

	return r.RemoteResolver.GetProvider(ctx, provider)
}

// waitForVMBinding records that a snapshot of a clustered VM with no confirmed
// host binding is waiting for it (ADR-0007 Addendum A, A1) and requeues. The
// snapshot stays in the initial phase, so the create is retried unchanged once
// the VM is bound; no provider call is made.
func (r *VMSnapshotReconciler) waitForVMBinding(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, cause error) ctrl.Result {
	logging.FromContext(ctx).Info("No provider call can be made for the VM; waiting before snapshotting", "error", cause.Error())
	snapshot.Status.Phase = ""
	snapshot.Status.Message = vmRefWaitMessage(cause)
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionCreating,
		metav1.ConditionTrue, vmRefErrorReason(cause), cause.Error())
	// Status update errors are intentionally ignored to avoid blocking reconciliation
	_ = r.updateStatus(ctx, snapshot)
	return ctrl.Result{RequeueAfter: 30 * time.Second}
}

// retrySnapshotCreate records that the snapshot create could not be issued
// because the VM's Provider could not be read or resolved to a client
// (Ready=False, reason ProviderError, with message) and requeues. The snapshot stays in the
// initial phase, so the create is retried unchanged; no provider call was made.
func (r *VMSnapshotReconciler) retrySnapshotCreate(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, message string) ctrl.Result {
	snapshot.Status.Phase = ""
	snapshot.Status.Message = message
	k8s.SetCondition(&snapshot.Status.Conditions, infrav1beta1.VMSnapshotConditionReady,
		metav1.ConditionFalse, infrav1beta1.VMSnapshotReasonProviderError, message)
	// Status update errors are intentionally ignored to avoid blocking reconciliation
	_ = r.updateStatus(ctx, snapshot)
	return ctrl.Result{RequeueAfter: 30 * time.Second}
}

// buildSnapshotCreateRequest builds a snapshot create request from the snapshot
// spec for the VM vm addresses (see vmRefFor).
func (r *VMSnapshotReconciler) buildSnapshotCreateRequest(snapshot *infrav1beta1.VMSnapshot, vm contracts.VMRef) contracts.SnapshotCreateRequest {
	req := contracts.SnapshotCreateRequest{
		VM: vm,
		// The VMSnapshot's uid: a provider that retries a create by name adopts
		// an existing snapshot only when it was made for this VMSnapshot, never
		// one an earlier VMSnapshot of the same name left behind.
		RequestToken: string(snapshot.UID),
	}

	// Set snapshot configuration if provided
	if snapshot.Spec.SnapshotConfig != nil {
		req.NameHint = snapshot.Spec.SnapshotConfig.Name
		if req.NameHint == "" {
			// Use the VMSnapshot resource name as a hint
			req.NameHint = snapshot.Name
		}

		req.Description = snapshot.Spec.SnapshotConfig.Description
		req.IncludeMemory = snapshot.Spec.SnapshotConfig.IncludeMemory
		req.Quiesce = snapshot.Spec.SnapshotConfig.Quiesce
	} else {
		// Use the VMSnapshot resource name as a hint
		req.NameHint = snapshot.Name
		req.Quiesce = true // Default to quiesce for consistency
	}

	return req
}

// refuseSnapshotConsumer records that the snapshot's VM references a
// Provider in another namespace that does not select this one
// (spec.consumerNamespaceSelector): Ready=False with reason ConsumerNotAllowed
// and a Warning event on the transition. The phase is left alone (a snapshot
// not yet started stays in the initial phase, so its create is retried
// unchanged once access is granted) and no provider is resolved or called. The
// recheck is slow; the grant watches re-drive it promptly.
func (r *VMSnapshotReconciler) refuseSnapshotConsumer(ctx context.Context, snapshot *infrav1beta1.VMSnapshot, cause error) ctrl.Result {
	cause = consumerRefusalCause(cause)
	before := snapshot.Status.DeepCopy()
	if consumerRefusalIsNew(snapshot.Status.Conditions, cause) {
		logging.FromContext(ctx).Info("The VM's Provider may not be used from this namespace; not calling the provider", "error", cause.Error())
		r.Recorder.Event(snapshot, corev1.EventTypeWarning, conditions.ReasonConsumerNotAllowed, cause.Error())
	}
	snapshot.Status.Message = cause.Error()
	snapshot.Status.ObservedGeneration = snapshot.Generation
	meta.SetStatusCondition(&snapshot.Status.Conditions, metav1.Condition{
		Type:               infrav1beta1.VMSnapshotConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonConsumerNotAllowed,
		Message:            cause.Error(),
		ObservedGeneration: snapshot.Generation,
	})
	metrics.RecordError(errReasonConsumerNotAllowed, metrics.ComponentManager)
	// A recheck of the same refusal changes nothing: don't write it again.
	// Status update errors are intentionally ignored to avoid blocking reconciliation.
	if !equality.Semantic.DeepEqual(before, &snapshot.Status) {
		_ = r.updateStatus(ctx, snapshot)
	}
	return ctrl.Result{RequeueAfter: consumerNotAllowedRetryInterval}
}

// snapshotsForGrantChange returns the VMSnapshots refused with
// ConsumerNotAllowed that a consumer-grant change may lift (consumerGrantIndex),
// so a grant takes effect without waiting for the slow recheck.
func (r *VMSnapshotReconciler) snapshotsForGrantChange(ctx context.Context, indexValue string) []reconcile.Request {
	return requestsForGrantChange(ctx, r.Client, &infrav1beta1.VMSnapshotList{}, indexValue, nil)
}

// snapshotUpdateNeedsReconcile reports whether an update of a VMSnapshot is
// reconciled: a spec change or the start of its deletion (both change its
// generation), or a change of the force-delete annotation — the only metadata
// the controller reads, which releases a held delete at once. Any other
// status- or metadata-only update is not: the controller's own status writes,
// or a tenant re-annotating a snapshot whose delete is held, would otherwise
// re-run the provider's SnapshotDelete ahead of its backoff. Every step that
// must follow one of the controller's own writes asks for it with an explicit
// requeue.
func snapshotUpdateNeedsReconcile(oldSnap, newSnap *infrav1beta1.VMSnapshot) bool {
	return oldSnap.Generation != newSnap.Generation ||
		oldSnap.Annotations[forceDeleteAnnotation] != newSnap.Annotations[forceDeleteAnnotation]
}

// snapshotUpdatePredicate applies snapshotUpdateNeedsReconcile to update
// events; create, delete and generic events always pass.
func snapshotUpdatePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSnap, ok := e.ObjectOld.(*infrav1beta1.VMSnapshot)
			if !ok {
				return true
			}
			newSnap, ok := e.ObjectNew.(*infrav1beta1.VMSnapshot)
			if !ok {
				return true
			}
			return snapshotUpdateNeedsReconcile(oldSnap, newSnap)
		},
	}
}

// SetupWithManager sets up the controller with the Manager. Its own
// VMSnapshots are reconciled on create, delete, spec changes, the start of
// their deletion and changes of the force-delete annotation
// (snapshotUpdatePredicate). Besides them it watches consumer-grant changes
// (Namespace labels, Provider selectors) to re-drive snapshots refused with
// ConsumerNotAllowed.
func (r *VMSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := indexConsumerGrants(mgr, &infrav1beta1.VMSnapshot{}, snapshotConsumerGrantIndexValues); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1beta1.VMSnapshot{}, builder.WithPredicates(snapshotUpdatePredicate()))
	return withConsumerGrantWatches(b, r.snapshotsForGrantChange).
		Complete(r)
}
