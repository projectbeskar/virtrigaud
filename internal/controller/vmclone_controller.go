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
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

const (
	// vmCloneFinalizer guards a VMClone so the controller runs its (minimal)
	// cleanup before the object disappears. Deleting a VMClone never cascades
	// to the produced target VM — only the finalizer is removed.
	vmCloneFinalizer = "clone.infra.virtrigaud.io/finalizer"

	// errReasonGetClone is the metrics.RecordError reason for a failed VMClone
	// Get in the reconcile entry path.
	errReasonGetClone = "get-clone"

	// CloneAnnotationClonedFrom records the source VM the target was cloned
	// from (provenance) on the produced VirtualMachine CR.
	CloneAnnotationClonedFrom = "virtrigaud.io/cloned-from"
	// CloneAnnotationClone records the VMClone resource that produced the
	// target VirtualMachine CR (provenance).
	CloneAnnotationClone = "virtrigaud.io/clone"

	// cloneReasonUnsupportedSource is the condition reason used when the
	// requested clone source type is not implemented in this MVP.
	cloneReasonUnsupportedSource = "UnsupportedSource"
	// cloneReasonLinkedUnsupported is the condition reason used when the
	// provider reports it cannot perform linked clones.
	cloneReasonLinkedUnsupported = "LinkedCloneUnsupported"
)

// VMCloneReconciler reconciles a VMClone object. It supports the MVP source
// type (spec.source.vmRef), same-provider clones only: the produced VM lives
// on the source VM's provider (issue #179).
type VMCloneReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RemoteResolver resolves a Provider CR to a provider implementation. It
	// is the ProviderResolver interface (satisfied by *remote.Resolver in
	// production) so unit tests can inject a fake provider — mirroring the
	// VirtualMachine controller.
	RemoteResolver ProviderResolver
	Recorder       record.EventRecorder
}

// NewVMCloneReconciler creates a new VMClone reconciler.
func NewVMCloneReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	remoteResolver ProviderResolver,
	recorder record.EventRecorder,
) *VMCloneReconciler {
	return &VMCloneReconciler{
		Client:         c,
		Scheme:         scheme,
		RemoteResolver: remoteResolver,
		Recorder:       recorder,
	}
}

//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclones,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclones/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclones/finalizers,verbs=update
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a VMClone through its phases: validate the source, resolve
// the provider, pre-check linked-clone capability, clone (idempotently), poll
// the task, then bind a target VirtualMachine CR to the produced VM.
//
// Observability: per-call timer + outcome inference via deferred block emits
// `virtrigaud_manager_reconcile_total{kind="VMClone",outcome=...}` and the
// duration histogram. Named return values (`result`, `retErr`) are required by
// the deferred block — do not change the signature without updating the defer.
func (r *VMCloneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("VMClone")
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

	ctx = logging.WithCorrelationID(ctx, fmt.Sprintf("vmclone-%s/%s", req.Namespace, req.Name))
	logger := logging.FromContext(ctx)
	logger.Info("Reconciling VMClone", "clone", req.NamespacedName)

	clone := &infrav1beta1.VMClone{}
	if err := r.Get(ctx, req.NamespacedName, clone); err != nil {
		if client.IgnoreNotFound(err) == nil {
			logger.Info("VMClone not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get VMClone")
		metrics.RecordError(errReasonGetClone, metrics.ComponentManager)
		return ctrl.Result{}, err
	}

	// Handle deletion: removing a VMClone must NOT delete the target VM. Just
	// drop the finalizer.
	if !clone.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, clone)
	}

	// Add finalizer if needed.
	if !controllerutil.ContainsFinalizer(clone, vmCloneFinalizer) {
		controllerutil.AddFinalizer(clone, vmCloneFinalizer)
		if err := r.Update(ctx, clone); err != nil {
			logger.Error(err, "Failed to add finalizer")
			metrics.RecordError(errReasonAddFinalizer, metrics.ComponentManager)
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Terminal states: nothing further to do.
	switch clone.Status.Phase {
	case infrav1beta1.ClonePhaseReady, infrav1beta1.ClonePhaseFailed:
		return ctrl.Result{}, nil
	}

	clone.Status.ObservedGeneration = clone.Generation

	// Validate source type: MVP supports only spec.source.vmRef.
	if clone.Spec.Source.VMRef == nil || clone.Spec.Source.VMRef.Name == "" {
		return r.markFailed(ctx, clone, cloneReasonUnsupportedSource,
			"clone source type not yet supported; use source.vmRef"), nil
	}

	// Resolve the source VirtualMachine CR (same namespace as the VMClone).
	sourceVM := &infrav1beta1.VirtualMachine{}
	sourceKey := client.ObjectKey{Namespace: clone.Namespace, Name: clone.Spec.Source.VMRef.Name}
	if err := r.Get(ctx, sourceKey, sourceVM); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Source VM not found yet, waiting", "vm", sourceKey.Name)
			return r.markPending(ctx, clone, infrav1beta1.VMCloneReasonSourceNotFound,
				fmt.Sprintf("source VM %q not found", sourceKey.Name)), nil
		}
		logger.Error(err, "Failed to get source VM", "vm", sourceKey.Name)
		metrics.RecordError(errReasonGetVM, metrics.ComponentManager)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if sourceVM.Status.ID == "" {
		logger.Info("Source VM not yet provisioned (empty Status.ID), waiting", "vm", sourceKey.Name)
		return r.markPending(ctx, clone, infrav1beta1.VMCloneReasonCloning,
			"waiting for source VM to be provisioned"), nil
	}

	// Resolve the source provider — the produced VM lives on this provider
	// (same-provider clone only in this MVP).
	provider := &infrav1beta1.Provider{}
	providerKey := client.ObjectKey{
		Name:      sourceVM.Spec.ProviderRef.Name,
		Namespace: sourceVM.Namespace,
	}
	if sourceVM.Spec.ProviderRef.Namespace != "" {
		providerKey.Namespace = sourceVM.Spec.ProviderRef.Namespace
	}
	if err := r.Get(ctx, providerKey, provider); err != nil {
		logger.Error(err, "Failed to get provider", "provider", providerKey.Name)
		return r.markPending(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("provider %q not found", providerKey.Name)), nil
	}

	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		logger.Error(err, "Failed to get provider instance", "provider", providerKey.Name)
		return r.markPending(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("failed to resolve provider %q: %v", providerKey.Name, err)), nil
	}

	// Determine target namespace (defaults to the VMClone namespace).
	targetNamespace := clone.Spec.Target.Namespace
	if targetNamespace == "" {
		targetNamespace = clone.Namespace
	}

	linked := r.requestedCloneType(clone) == infrav1beta1.CloneTypeLinkedClone

	// Linked-clone capability pre-check (intrinsic correctness, independent of
	// the #176 enforcement flag). Fails OPEN if the provider is not a
	// CapabilityReporter or the query errors.
	if linked {
		if blocked, res := r.gateLinkedClone(ctx, clone, providerInstance); blocked {
			return res, nil
		}
	}

	// A clone task is still in flight (async clone): wait for it, then bind.
	// Checked before TargetVMID so async clones don't bind before the provider
	// has finished cloning.
	if clone.Status.TaskRef != "" {
		return r.pollCloneTask(ctx, clone, sourceVM, providerInstance, targetNamespace, linked)
	}

	// Idempotency. Once the Clone RPC has returned a target VM ID (persisted in
	// status by startClone before it binds, and after any task completes), the
	// clone itself is done — resume binding rather than re-cloning. bindTargetVM
	// is idempotent and conflict-tolerant, so re-entering here after a partial
	// bind (e.g. a Status.ID write that lost a race with the VirtualMachine
	// controller) completes the binding instead of leaving the clone orphaned.
	if clone.Status.TargetVMID != "" {
		return r.bindTargetVM(ctx, clone, sourceVM, targetNamespace, clone.Status.TargetVMID, linked)
	}

	// No clone issued yet. Refuse if a VM with the target name already exists
	// and was NOT produced by this clone (no recorded TargetVMID) — cloning
	// over a foreign VM would be destructive.
	existing := &infrav1beta1.VirtualMachine{}
	existingKey := client.ObjectKey{Namespace: targetNamespace, Name: clone.Spec.Target.Name}
	if err := r.Get(ctx, existingKey, existing); err == nil {
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("target VM %q already exists and was not created by this clone", existingKey.Name)), nil
	} else if !errors.IsNotFound(err) {
		logger.Error(err, "Failed to check for existing target VM", "vm", existingKey.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Issue the clone.
	return r.startClone(ctx, clone, sourceVM, provider, providerInstance, targetNamespace, linked)
}

// startClone issues the Clone RPC and records the resulting task / target ID.
func (r *VMCloneReconciler) startClone(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	provider *infrav1beta1.Provider,
	providerInstance contracts.Provider,
	targetNamespace string,
	linked bool,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// The provider must support cloning (optional Cloner capability).
	cloner, ok := providerInstance.(contracts.Cloner)
	if !ok {
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonUnsupported,
			"provider does not support clone"), nil
	}

	req := contracts.CloneRequest{
		SourceVmID:    sourceVM.Status.ID,
		TargetName:    clone.Spec.Target.Name,
		Linked:        linked,
		ClassJSON:     r.classJSON(ctx, clone),
		PlacementJSON: r.placementJSON(ctx, clone),
		CustomizeJSON: r.customizeJSON(ctx, clone),
	}

	now := metav1.Now()
	clone.Status.Phase = infrav1beta1.ClonePhaseCloning
	clone.Status.StartTime = &now
	if linked {
		clone.Status.ActualCloneType = infrav1beta1.CloneTypeLinkedClone
	} else {
		clone.Status.ActualCloneType = infrav1beta1.CloneTypeFullClone
	}
	k8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionCloning,
		metav1.ConditionTrue, infrav1beta1.VMCloneReasonCloning, "Clone operation initiated")

	resp, err := cloner.Clone(ctx, req)
	if err != nil {
		logger.Error(err, "Clone RPC failed")
		r.Recorder.Event(clone, "Warning", infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("Clone failed: %v", err))
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("clone failed: %v", err)), nil
	}

	clone.Status.TargetVMID = resp.TargetVmID
	clone.Status.TaskRef = resp.TaskRef

	// Persist the target VM ID BEFORE attempting to bind. The bind step writes
	// the target VM's Status.ID and can lose a race with the VirtualMachine
	// controller, which reconciles the freshly-created adopted target VM
	// immediately. If that happens and we requeue, this persisted TargetVMID is
	// what lets the next reconcile resume binding (via the idempotency check)
	// instead of issuing a second clone.
	if err := r.updateStatus(ctx, clone); err != nil {
		return ctrl.Result{}, err
	}

	// Synchronous clone (no task): bind the target VM immediately.
	if resp.TaskRef == "" {
		logger.Info("Clone completed synchronously", "target_vm_id", resp.TargetVmID)
		return r.bindTargetVM(ctx, clone, sourceVM, targetNamespace, resp.TargetVmID, linked)
	}

	logger.Info("Clone task started", "task_ref", resp.TaskRef)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// pollCloneTask checks an in-flight clone task and, on completion, binds the
// target VM.
func (r *VMCloneReconciler) pollCloneTask(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	providerInstance contracts.Provider,
	targetNamespace string,
	linked bool,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	done, err := providerInstance.IsTaskComplete(ctx, clone.Status.TaskRef)
	if err != nil {
		logger.Error(err, "Clone task failed", "task_ref", clone.Status.TaskRef)
		r.Recorder.Event(clone, "Warning", infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("Clone task failed: %v", err))
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("clone task failed: %v", err)), nil
	}
	if !done {
		logger.Info("Clone task still in progress", "task_ref", clone.Status.TaskRef)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Persist the cleared task ref before binding so a bind retry resumes via
	// the TargetVMID idempotency path rather than re-polling a finished task.
	clone.Status.TaskRef = ""
	if err := r.updateStatus(ctx, clone); err != nil {
		return ctrl.Result{}, err
	}
	return r.bindTargetVM(ctx, clone, sourceVM, targetNamespace, clone.Status.TargetVMID, linked)
}

// bindTargetVM ensures the target VirtualMachine CR exists for a completed
// clone and that its Status.ID is seeded with the provider-reported target VM
// ID. The adopted label plus the seeded Status.ID together stop the
// VirtualMachine controller from creating a second VM (issue #179).
//
// It is idempotent and conflict-tolerant: it may run multiple times for the
// same clone (the VirtualMachine controller reconciles the freshly-created,
// adopted target VM immediately and bumps its resourceVersion, which would race
// a naive Status().Update). The Status.ID seed therefore re-Gets the latest
// object and retries on conflict, and the clone is only finalized Ready once
// Status.ID is confirmed set — so a lost race resumes and completes the binding
// instead of leaving the cloned VM orphaned.
func (r *VMCloneReconciler) bindTargetVM(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	targetNamespace string,
	targetVMID string,
	linked bool,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	if targetVMID == "" {
		// Defensive: a completed clone with no target ID is a provider bug.
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
			"clone completed but provider returned no target VM ID"), nil
	}

	vmKey := client.ObjectKey{Namespace: targetNamespace, Name: clone.Spec.Target.Name}

	// Ensure the target VM CR exists (idempotent across requeues).
	targetVM := &infrav1beta1.VirtualMachine{}
	switch err := r.Get(ctx, vmKey, targetVM); {
	case errors.IsNotFound(err):
		targetVM = r.buildTargetVM(clone, sourceVM, targetNamespace)
		if createErr := r.Create(ctx, targetVM); createErr != nil && !errors.IsAlreadyExists(createErr) {
			logger.Error(createErr, "Failed to create target VM CR", "vm", vmKey.Name)
			return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
				fmt.Sprintf("failed to create target VM: %v", createErr)), nil
		}
		r.Recorder.Event(clone, "Normal", infrav1beta1.VMCloneReasonCompleted,
			fmt.Sprintf("Created target VM %q", vmKey.Name))
	case err != nil:
		logger.Error(err, "Failed to get target VM CR", "vm", vmKey.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Seed Status.ID out-of-band (the status subresource is not persisted on
	// Create) so the VirtualMachine controller adopts the already-cloned VM
	// rather than creating a second one. Re-Get + retry on conflict because the
	// VirtualMachine controller writes this same object concurrently.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &infrav1beta1.VirtualMachine{}
		if getErr := r.Get(ctx, vmKey, latest); getErr != nil {
			return getErr
		}
		if latest.Status.ID == targetVMID {
			return nil
		}
		latest.Status.ID = targetVMID
		return r.Status().Update(ctx, latest)
	}); err != nil {
		logger.Error(err, "Failed to seed Status.ID on target VM CR; will retry", "vm", vmKey.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	logger.Info("Target VM bound to cloned VM", "vm", vmKey.Name, "vm_id", targetVMID)
	return r.finalizeReady(ctx, clone, targetVM)
}

// buildTargetVM constructs the target VirtualMachine CR for a clone: it carries
// the adopted label and clone provenance annotations, inherits the source VM's
// provider and class (unless the clone overrides the class), and copies the
// requested networks/placement. Status.ID is seeded separately by bindTargetVM.
func (r *VMCloneReconciler) buildTargetVM(
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	targetNamespace string,
) *infrav1beta1.VirtualMachine {
	labels := map[string]string{}
	for k, v := range clone.Spec.Target.Labels {
		labels[k] = v
	}
	labels[AdoptedLabel] = AdoptedLabelValue

	annotations := map[string]string{
		CloneAnnotationClonedFrom: sourceVM.Name,
		CloneAnnotationClone:      clone.Name,
	}
	for k, v := range clone.Spec.Target.Annotations {
		annotations[k] = v
	}

	classRef := sourceVM.Spec.ClassRef
	if clone.Spec.Target.ClassRef != nil && clone.Spec.Target.ClassRef.Name != "" {
		classRef = infrav1beta1.ObjectRef{Name: clone.Spec.Target.ClassRef.Name}
	}

	targetVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        clone.Spec.Target.Name,
			Namespace:   targetNamespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: sourceVM.Spec.ProviderRef,
			ClassRef:    classRef,
		},
	}
	if len(clone.Spec.Target.Networks) > 0 {
		targetVM.Spec.Networks = clone.Spec.Target.Networks
	}
	if clone.Spec.Target.PlacementRef != nil && clone.Spec.Target.PlacementRef.Name != "" {
		targetVM.Spec.PlacementRef = &infrav1beta1.LocalObjectReference{Name: clone.Spec.Target.PlacementRef.Name}
	}
	return targetVM
}

// finalizeReady marks the VMClone Ready and records the target reference.
func (r *VMCloneReconciler) finalizeReady(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	targetVM *infrav1beta1.VirtualMachine,
) (ctrl.Result, error) {
	now := metav1.Now()
	clone.Status.Phase = infrav1beta1.ClonePhaseReady
	clone.Status.Message = "Clone completed successfully"
	clone.Status.TaskRef = ""
	clone.Status.CompletionTime = &now
	clone.Status.TargetRef = &infrav1beta1.LocalObjectReference{Name: targetVM.Name}
	k8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionReady,
		metav1.ConditionTrue, infrav1beta1.VMCloneReasonCompleted, "Clone completed successfully")
	k8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionCloning,
		metav1.ConditionFalse, infrav1beta1.VMCloneReasonCompleted, "Clone completed")

	if err := r.updateStatus(ctx, clone); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// gateLinkedClone refuses a linked-clone request when the resolved provider
// reports it cannot perform linked clones. It is INTRINSIC clone correctness:
// it always runs (independent of the #176 enforcement flag) but fails OPEN if
// the provider does not implement contracts.CapabilityReporter or the
// capability query errors. Returns blocked=true (with the ctrl.Result the
// caller should return) only when the provider explicitly reports
// !SupportsLinkedClones.
func (r *VMCloneReconciler) gateLinkedClone(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	providerInstance contracts.Provider,
) (blocked bool, result ctrl.Result) {
	logger := logging.FromContext(ctx)

	reporter, ok := providerInstance.(contracts.CapabilityReporter)
	if !ok {
		logger.V(1).Info("Provider does not report capabilities; allowing linked clone (fail open)")
		return false, ctrl.Result{}
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		logger.V(1).Info("GetCapabilities failed; allowing linked clone (fail open)", "error", err.Error())
		return false, ctrl.Result{}
	}
	if !caps.SupportsLinkedClones {
		return true, r.markFailed(ctx, clone, cloneReasonLinkedUnsupported,
			"provider does not support linked clones")
	}
	return false, ctrl.Result{}
}

// handleDeletion drops the finalizer without touching the produced target VM.
func (r *VMCloneReconciler) handleDeletion(ctx context.Context, clone *infrav1beta1.VMClone) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	logger.Info("Deleting VMClone (target VM is intentionally preserved)")

	controllerutil.RemoveFinalizer(clone, vmCloneFinalizer)
	if err := r.Update(ctx, clone); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		metrics.RecordError(errReasonRemoveFinalizer, metrics.ComponentManager)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// markFailed sets the VMClone to the Failed phase with a Ready=False / Failed
// condition and persists status. It does not requeue, and Failed is terminal:
// the Reconcile entry short-circuits on a Failed phase, so a failed clone is
// not retried in place — recreate the VMClone to retry. This is deliberate:
// a clone is a one-shot job and a partial provider-side clone makes blind
// auto-retry unsafe (it could leave or create a duplicate provider VM).
func (r *VMCloneReconciler) markFailed(ctx context.Context, clone *infrav1beta1.VMClone, reason, message string) ctrl.Result {
	logger := logging.FromContext(ctx)
	logger.Info("VMClone failed", "reason", reason, "message", message)

	clone.Status.Phase = infrav1beta1.ClonePhaseFailed
	clone.Status.Message = message
	clone.Status.TaskRef = ""
	k8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionReady,
		metav1.ConditionFalse, reason, message)
	k8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionFailed,
		metav1.ConditionTrue, reason, message)
	r.Recorder.Event(clone, "Warning", reason, message)

	_ = r.updateStatus(ctx, clone) //nolint:errcheck // status errors retried next reconcile
	return ctrl.Result{}
}

// markPending sets the VMClone to the Pending phase (still waiting on a
// prerequisite) and requeues.
func (r *VMCloneReconciler) markPending(ctx context.Context, clone *infrav1beta1.VMClone, reason, message string) ctrl.Result {
	clone.Status.Phase = infrav1beta1.ClonePhasePending
	clone.Status.Message = message
	k8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionReady,
		metav1.ConditionFalse, reason, message)
	_ = r.updateStatus(ctx, clone) //nolint:errcheck // status errors retried next reconcile
	return ctrl.Result{RequeueAfter: 30 * time.Second}
}

// requestedCloneType returns the clone type from spec.options, defaulting to
// FullClone when unset.
func (r *VMCloneReconciler) requestedCloneType(clone *infrav1beta1.VMClone) infrav1beta1.CloneType {
	if clone.Spec.Options != nil && clone.Spec.Options.Type != "" {
		return clone.Spec.Options.Type
	}
	return infrav1beta1.CloneTypeFullClone
}

// classJSON best-effort marshals the referenced VMClass override to JSON, or
// returns "" when no class override is referenced or the lookup/marshal fails.
func (r *VMCloneReconciler) classJSON(ctx context.Context, clone *infrav1beta1.VMClone) string {
	if clone.Spec.Target.ClassRef == nil || clone.Spec.Target.ClassRef.Name == "" {
		return ""
	}
	vmClass := &infrav1beta1.VMClass{}
	key := client.ObjectKey{Namespace: clone.Namespace, Name: clone.Spec.Target.ClassRef.Name}
	if err := r.Get(ctx, key, vmClass); err != nil {
		return ""
	}
	data, err := json.Marshal(vmClass.Spec)
	if err != nil {
		return ""
	}
	return string(data)
}

// placementJSON best-effort marshals the target placement reference to JSON,
// or returns "" when no placement is referenced or marshal fails.
func (r *VMCloneReconciler) placementJSON(_ context.Context, clone *infrav1beta1.VMClone) string {
	if clone.Spec.Target.PlacementRef == nil || clone.Spec.Target.PlacementRef.Name == "" {
		return ""
	}
	data, err := json.Marshal(clone.Spec.Target.PlacementRef)
	if err != nil {
		return ""
	}
	return string(data)
}

// customizeJSON best-effort marshals the spec.customization to JSON, or
// returns "" when no customization is present or marshal fails.
func (r *VMCloneReconciler) customizeJSON(_ context.Context, clone *infrav1beta1.VMClone) string {
	if clone.Spec.Customization == nil {
		return ""
	}
	data, err := json.Marshal(clone.Spec.Customization)
	if err != nil {
		return ""
	}
	return string(data)
}

// getProviderInstance resolves a Provider CR to a remote provider implementation.
func (r *VMCloneReconciler) getProviderInstance(ctx context.Context, provider *infrav1beta1.Provider) (contracts.Provider, error) {
	if r.RemoteResolver == nil {
		return nil, fmt.Errorf("no remote resolver available")
	}
	return r.RemoteResolver.GetProvider(ctx, provider)
}

// updateStatus persists the VMClone status subresource.
func (r *VMCloneReconciler) updateStatus(ctx context.Context, clone *infrav1beta1.VMClone) error {
	if err := r.Status().Update(ctx, clone); err != nil {
		logging.FromContext(ctx).Error(err, "Failed to update VMClone status")
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VMCloneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1beta1.VMClone{}).
		Complete(r)
}
