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
	stderrors "errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	conditions "github.com/projectbeskar/virtrigaud/internal/k8s"
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
	// CloneAnnotationCloneUID records the UID of the VMClone that created the
	// target VirtualMachine CR. bindTargetVM binds the cloned VM's id only to a
	// target carrying its own clone's UID, so it never binds onto a
	// VirtualMachine it did not create.
	CloneAnnotationCloneUID = "virtrigaud.io/clone-uid"

	// cloneReasonTargetConflict is the condition reason used when the clone's
	// target VirtualMachine cannot be bound to the cloned VM: it was not created
	// by this clone, is already bound to another VM, or references another
	// Provider.
	cloneReasonTargetConflict = "TargetConflict"

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

	// APIReader reads straight from the API server, bypassing the informer
	// cache (the manager's GetAPIReader). The cross-namespace grant is re-read
	// through it immediately before each side effect in the target namespace —
	// the Clone RPC and the target VirtualMachine Create — so a revoked grant
	// is honoured even while the cache still shows it. Nil falls back to
	// Client, which only unit tests built as struct literals rely on.
	APIReader client.Reader
}

// NewVMCloneReconciler creates a new VMClone reconciler. apiReader must be an
// uncached reader (mgr.GetAPIReader()); see VMCloneReconciler.APIReader.
func NewVMCloneReconciler(
	c client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	remoteResolver ProviderResolver,
	recorder record.EventRecorder,
) *VMCloneReconciler {
	return &VMCloneReconciler{
		Client:         c,
		APIReader:      apiReader,
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
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

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

	// Target namespace (defaults to the VMClone's). A different namespace must
	// grant this one (AllowedSourceNamespacesAnnotation) before anything else
	// happens: no provider call, and no read or write in the target namespace.
	// It is checked on EVERY reconcile — clone start, task poll and bind all
	// follow — so revoking the grant stops a clone that is already in flight.
	targetNamespace := cloneTargetNamespace(clone)
	if allowed, res, err := r.gateTargetNamespace(ctx, clone, targetNamespace); !allowed {
		return res, err
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
	// (same-provider clone only in this MVP). The source VM must still be bound
	// through it (status.boundProvider): its status.id — cloned here, and the
	// clone task and bind that follow — is meaningful only on that Provider.
	providerKey := vmProviderKey(sourceVM)
	if err := checkBoundProvider(sourceVM, providerKey); err != nil {
		logger.Info("Source VM is not bound through the Provider its spec.providerRef names; not cloning", "vm", sourceKey.Name, "error", err.Error())
		return r.markPending(ctx, clone, vmRefErrorReason(err), err.Error()), nil
	}

	// Every cross-namespace Provider / VMClass the clone uses or pins onto its
	// target must select the namespace that uses it
	// (spec.consumerNamespaceSelector): the source VM's Provider for this
	// namespace, and the target VM's references for the target namespace. A
	// refusal makes no provider call and creates nothing. It is checked on
	// EVERY reconcile (clone start, task poll and bind all follow), so a
	// revoked grant stops a clone in flight.
	if allowed, res, err := r.gateConsumers(ctx, r.Client, clone, sourceVM, targetNamespace); !allowed {
		return res, err
	}

	provider := &infrav1beta1.Provider{}
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
		return r.pollCloneTask(ctx, clone, sourceVM, provider, providerInstance, targetNamespace, linked)
	}

	// Idempotency. Once the Clone RPC has returned a target VM ID (persisted in
	// status by startClone before it binds, and after any task completes), the
	// clone itself is done — resume binding rather than re-cloning. bindTargetVM
	// is idempotent and conflict-tolerant, so re-entering here after a partial
	// bind (e.g. a Status.ID write that lost a race with the VirtualMachine
	// controller) completes the binding instead of leaving the clone orphaned.
	if clone.Status.TargetVMID != "" {
		return r.bindTargetVM(ctx, clone, sourceVM, provider, targetNamespace, clone.Status.TargetVMID, linked)
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

	// Address the source VM (ADR-0007 Addendum A, A1): on a clustered provider
	// the clone is routed to the source's bound host. A clustered source with no
	// confirmed binding is never sent a per-VM call — wait for it.
	sourceRef, err := vmRefFor(sourceVM, provider)
	if err != nil {
		logger.Info("Source VM has no host binding; waiting before cloning", "vm", sourceKey.Name, "error", err.Error())
		return r.markPending(ctx, clone, vmRefErrorReason(err), err.Error()), nil
	}

	// Issue the clone.
	return r.startClone(ctx, clone, sourceRef, provider, providerInstance, targetNamespace, sourceVM, linked)
}

// startClone issues the Clone RPC for the source VM source addresses and
// records the resulting task / target ID.
func (r *VMCloneReconciler) startClone(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	source contracts.VMRef,
	provider *infrav1beta1.Provider,
	providerInstance contracts.Provider,
	targetNamespace string,
	sourceVM *infrav1beta1.VirtualMachine,
	linked bool,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	// The provider must support cloning (optional Cloner capability).
	cloner, ok := providerInstance.(contracts.Cloner)
	if !ok {
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonUnsupported,
			"provider does not support clone"), nil
	}

	// TargetVM names the VirtualMachine bindTargetVM will create for the clone.
	// A provider whose VM names are host-global (libvirt) names the clone from
	// it with its Create naming rule ("<namespace>.<name>") and returns that
	// name as TargetVmID, which is recorded as the VM's status.id — the
	// operator never re-derives it.
	req := contracts.CloneRequest{
		Source:        source,
		TargetName:    clone.Spec.Target.Name,
		TargetVM:      contracts.ObjectIdentity{Namespace: targetNamespace, Name: clone.Spec.Target.Name},
		Linked:        linked,
		ClassJSON:     r.classJSON(ctx, clone),
		PlacementJSON: r.placementJSON(ctx, clone),
		CustomizeJSON: r.customizeJSON(ctx, clone),
	}

	// The Clone RPC creates a VM named for the target namespace. Re-read the
	// grants from the API server (not the cache) right before it — the target
	// namespace's, and the consumer grants of the Provider and VMClass the
	// clone uses and pins; nothing that does I/O runs between these checks and
	// the RPC.
	if allowed, res, err := r.confirmTargetNamespaceLive(ctx, clone, targetNamespace); !allowed {
		return res, err
	}
	if allowed, res, err := r.gateConsumers(ctx, r.liveReader(), clone, sourceVM, targetNamespace); !allowed {
		return res, err
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
		return r.bindTargetVM(ctx, clone, sourceVM, provider, targetNamespace, resp.TargetVmID, linked)
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
	provider *infrav1beta1.Provider,
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
	return r.bindTargetVM(ctx, clone, sourceVM, provider, targetNamespace, clone.Status.TargetVMID, linked)
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
//
// provider is the Provider the clone ran on. The target is bound through it:
// status.boundProvider is written in the same status write as Status.ID, and a
// target VM whose spec.providerRef resolves to any other Provider (e.g. one a
// tenant created under the target name while the clone ran) is never bound —
// the cloned VM's id is meaningful only on the Provider that made it.
func (r *VMCloneReconciler) bindTargetVM(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	provider *infrav1beta1.Provider,
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

	// Re-check the cross-namespace grant right before the first read or write
	// in the target namespace: a synchronous Clone RPC or a task poll ran since
	// the check in Reconcile, and the grant may have been revoked meanwhile.
	if allowed, res, err := r.gateTargetNamespace(ctx, clone, targetNamespace); !allowed {
		return res, err
	}

	vmKey := client.ObjectKey{Namespace: targetNamespace, Name: clone.Spec.Target.Name}

	// Ensure the target VM CR exists (idempotent across requeues).
	targetVM := &infrav1beta1.VirtualMachine{}
	switch err := r.Get(ctx, vmKey, targetVM); {
	case errors.IsNotFound(err):
		// Re-read the grants from the API server (not the cache) right before
		// the Create in the target namespace.
		if allowed, res, liveErr := r.confirmTargetNamespaceLive(ctx, clone, targetNamespace); !allowed {
			return res, liveErr
		}
		if allowed, res, liveErr := r.gateConsumers(ctx, r.liveReader(), clone, sourceVM, targetNamespace); !allowed {
			return res, liveErr
		}
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
	//
	// On a clustered provider the clone lands on the source's host (v1 requires
	// source and landing host to be equal: disks are host-local), so the target's
	// binding (status.placement.host) is written in the SAME write as Status.ID
	// (ADR-0007 Addendum A, A1) — the target is never observable with an id but
	// no host. A single-host source has no binding, so nothing is written.
	landing := clonedPlacement(sourceVM)
	bound := boundProviderRefFor(provider)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &infrav1beta1.VirtualMachine{}
		if getErr := r.Get(ctx, vmKey, latest); getErr != nil {
			return getErr
		}
		if err := cloneTargetBindable(clone, latest, provider, targetVMID); err != nil {
			return err
		}
		if latest.Status.ID == targetVMID && (landing == nil || boundHost(latest) == landing.Host) &&
			latest.Status.BoundProvider != nil && *latest.Status.BoundProvider == *bound {
			return nil
		}
		latest.Status.ID = targetVMID
		latest.Status.BoundProvider = bound
		if landing != nil {
			latest.Status.Placement = landing.DeepCopy()
		}
		return r.Status().Update(ctx, latest)
	}); err != nil {
		var conflict *cloneTargetConflictError
		if stderrors.As(err, &conflict) {
			logger.Info("Refusing to bind the cloned VM to the target VirtualMachine", "vm", vmKey.Name, "error", err.Error())
			return r.markFailed(ctx, clone, cloneReasonTargetConflict, err.Error()), nil
		}
		logger.Error(err, "Failed to seed Status.ID on target VM CR; will retry", "vm", vmKey.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	logger.Info("Target VM bound to cloned VM", "vm", vmKey.Name, "vm_id", targetVMID)
	return r.finalizeReady(ctx, clone, targetVM)
}

// cloneTargetConflictError reports why a clone's target VirtualMachine may
// not be bound to the cloned VM. The cloned VM is left on the provider (its id
// is in the message) for an administrator to adopt or remove.
type cloneTargetConflictError struct {
	target  client.ObjectKey
	id      string
	problem string
}

// Error implements error.
func (e *cloneTargetConflictError) Error() string {
	return fmt.Sprintf("target VM %s %s; the cloned VM %q is not bound to it and is left on the provider",
		e.target, e.problem, e.id)
}

// cloneTargetBindable reports whether target — the VirtualMachine under the
// clone's target name — may be bound to the cloned VM targetVMID, which the
// clone made on provider. It may only if all of these hold, so the clone never
// overwrites or takes over a VirtualMachine it did not create:
//
//   - it carries this clone's UID marker (CloneAnnotationCloneUID), which
//     buildTargetVM sets (and user-supplied target annotations cannot);
//   - its status.id is empty or already the cloned VM's (a resumed bind);
//   - its spec.providerRef names the Provider the clone ran on.
func cloneTargetBindable(clone *infrav1beta1.VMClone, target *infrav1beta1.VirtualMachine,
	provider *infrav1beta1.Provider, targetVMID string) error {
	key := client.ObjectKeyFromObject(target)
	conflict := func(format string, args ...any) error {
		return &cloneTargetConflictError{target: key, id: targetVMID, problem: fmt.Sprintf(format, args...)}
	}
	if clone.UID == "" || target.Annotations[CloneAnnotationCloneUID] != string(clone.UID) {
		return conflict("was not created by this VMClone (no %s=%s marker)", CloneAnnotationCloneUID, clone.UID)
	}
	if target.Status.ID != "" && target.Status.ID != targetVMID {
		return conflict("is already bound to VM %q", target.Status.ID)
	}
	if providerKey, cloneProvider := vmProviderKey(target), client.ObjectKeyFromObject(provider); providerKey != cloneProvider {
		return conflict("references Provider %s, not the Provider the clone ran on (%s)", providerKey, cloneProvider)
	}
	return nil
}

// clonedPlacement returns the binding a clone of sourceVM lands with: the
// source's confirmed host and pool on a clustered provider (ADR-0007 Addendum
// A, A1), or nil for a source without a binding (single-host / thin-client).
func clonedPlacement(sourceVM *infrav1beta1.VirtualMachine) *infrav1beta1.PlacementStatus {
	host := boundHost(sourceVM)
	if host == "" {
		return nil
	}
	now := metav1.Now()
	return &infrav1beta1.PlacementStatus{
		Host:              host,
		Pool:              sourceVM.Status.Placement.Pool,
		LastScheduledTime: &now,
		Reason:            fmt.Sprintf("cloned from %s on its host", sourceVM.Name),
	}
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

	// User annotations first, without the operator's reserved virtrigaud.io
	// keys (control annotations such as orphan-on-delete / force-delete, and
	// provenance); the controller's provenance is written last so it cannot be
	// overridden.
	annotations := userTargetAnnotations(clone.Spec.Target.Annotations)
	annotations[CloneAnnotationClonedFrom] = sourceVM.Name
	annotations[CloneAnnotationClone] = clone.Name
	annotations[CloneAnnotationCloneUID] = string(clone.UID)

	providerRef := sourceVM.Spec.ProviderRef
	classRef := sourceVM.Spec.ClassRef
	if clone.Spec.Target.ClassRef != nil && clone.Spec.Target.ClassRef.Name != "" {
		classRef = infrav1beta1.ObjectRef{Name: clone.Spec.Target.ClassRef.Name}
	}
	if targetNamespace != clone.Namespace {
		// A reference without a namespace resolves in the namespace of the VM
		// that holds it. A target in another namespace would therefore resolve
		// a same-named Provider (and class) THERE and bind the cloned VM's ID on
		// the wrong hypervisor — possibly to an unrelated VM with that ID. Pin
		// the namespace the clone itself resolved: the source VM's, which is
		// the VMClone's (spec.source.vmRef is namespace-local).
		if providerRef.Namespace == "" {
			providerRef.Namespace = clone.Namespace
		}
		if classRef.Namespace == "" {
			classRef.Namespace = clone.Namespace
		}
	}

	targetVM := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        clone.Spec.Target.Name,
			Namespace:   targetNamespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: providerRef,
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

// cloneTargetNamespace is the namespace the clone's target VirtualMachine is
// created in: spec.target.namespace, or the VMClone's own namespace.
func cloneTargetNamespace(clone *infrav1beta1.VMClone) string {
	if clone.Spec.Target.Namespace != "" {
		return clone.Spec.Target.Namespace
	}
	return clone.Namespace
}

// gateTargetNamespace enforces the cross-namespace target rule
// (AllowedSourceNamespacesAnnotation). allowed=true means the clone may read,
// create and bind in targetNamespace; any refusal left on the clone by an
// earlier reconcile is then cleared. Otherwise the clone is marked refused and
// (result, err) is what the caller returns: a slow recheck after a refusal
// (the Namespace watch re-drives it on a grant), or the read error, so the
// controller retries with backoff while failing closed.
func (r *VMCloneReconciler) gateTargetNamespace(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	targetNamespace string,
) (allowed bool, result ctrl.Result, err error) {
	allowed, err = targetNamespaceAllowed(ctx, r.Client, clone.Namespace, targetNamespace)
	if err != nil {
		logging.FromContext(ctx).Error(err, "Failed to check the target namespace grant; not proceeding")
		return false, ctrl.Result{}, err
	}
	if !allowed {
		return false, r.markTargetNamespaceNotAllowed(ctx, clone, targetNamespace), nil
	}
	if err := r.clearTargetNamespaceRefusal(ctx, clone); err != nil {
		return false, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}

// confirmTargetNamespaceLive re-checks the cross-namespace grant through the
// uncached APIReader immediately before a side effect in the target namespace
// (the Clone RPC, the target VirtualMachine Create). gateTargetNamespace reads
// the informer cache, which can lag a revocation; this closes that window for
// the calls that create something. The own namespace needs no read. A refusal
// is recorded exactly like the cached one; a read error is returned so the
// controller retries with backoff and nothing is issued.
func (r *VMCloneReconciler) confirmTargetNamespaceLive(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	targetNamespace string,
) (allowed bool, result ctrl.Result, err error) {
	allowed, err = targetNamespaceAllowed(ctx, r.liveReader(), clone.Namespace, targetNamespace)
	if err != nil {
		logging.FromContext(ctx).Error(err, "Failed to re-read the target namespace grant; not proceeding")
		return false, ctrl.Result{}, err
	}
	if !allowed {
		return false, r.markTargetNamespaceNotAllowed(ctx, clone, targetNamespace), nil
	}
	return true, ctrl.Result{}, nil
}

// liveReader is the uncached reader for grant re-checks: APIReader, or Client
// when none was injected.
func (r *VMCloneReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// markTargetNamespaceNotAllowed records that the clone's target namespace does
// not grant the clone's namespace: Ready=False with reason
// TargetNamespaceNotAllowed. It is NOT terminal — a clone that has issued
// nothing yet waits in Pending, and one already in flight keeps its phase,
// task and target ID so it resumes if the grant returns. Nothing already
// created is touched. The Warning event is emitted only on the transition, and
// the recheck is slow: only whoever can update the target Namespace can lift
// the refusal.
func (r *VMCloneReconciler) markTargetNamespaceNotAllowed(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	targetNamespace string,
) ctrl.Result {
	msg := targetNamespaceNotAllowedMessage(clone.Namespace, targetNamespace)
	prev := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	transition := prev == nil || prev.Reason != ReasonTargetNamespaceNotAllowed || prev.Message != msg

	if clone.Status.TaskRef == "" && clone.Status.TargetVMID == "" {
		clone.Status.Phase = infrav1beta1.ClonePhasePending
	}
	clone.Status.Message = msg
	clone.Status.ObservedGeneration = clone.Generation
	meta.SetStatusCondition(&clone.Status.Conditions, metav1.Condition{
		Type:               infrav1beta1.VMCloneConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonTargetNamespaceNotAllowed,
		Message:            msg,
		ObservedGeneration: clone.Generation,
	})
	if transition {
		logging.FromContext(ctx).Info("VMClone target namespace does not grant the clone's namespace; nothing is created there",
			"targetNamespace", targetNamespace)
		r.Recorder.Event(clone, corev1.EventTypeWarning, ReasonTargetNamespaceNotAllowed, msg)
	}
	_ = r.updateStatus(ctx, clone) //nolint:errcheck // status errors retried next reconcile
	return ctrl.Result{RequeueAfter: crossNamespaceRecheckInterval}
}

// clearTargetNamespaceRefusal removes a TargetNamespaceNotAllowed Ready
// condition once the grant is present (or the target was changed to the own
// namespace), so the clone does not keep reporting a refusal while it
// proceeds. It persists immediately; it is a no-op when there is nothing to
// clear.
func (r *VMCloneReconciler) clearTargetNamespaceRefusal(ctx context.Context, clone *infrav1beta1.VMClone) error {
	ready := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	if ready == nil || ready.Reason != ReasonTargetNamespaceNotAllowed {
		return nil
	}
	meta.RemoveStatusCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	clone.Status.Message = "Target namespace access granted; resuming"
	clone.Status.ObservedGeneration = clone.Generation
	r.Recorder.Event(clone, corev1.EventTypeNormal, "TargetNamespaceAllowed", clone.Status.Message)
	return r.updateStatus(ctx, clone)
}

// clonesTargetingNamespace maps a Namespace whose grant annotation changed to
// the VMClones in OTHER namespaces that target it and have not finished, so a
// grant (or revocation) takes effect without waiting for the slow recheck.
func (r *VMCloneReconciler) clonesTargetingNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	clones := &infrav1beta1.VMCloneList{}
	if err := r.List(ctx, clones); err != nil {
		logging.FromContext(ctx).Error(err, "Failed to list VMClones for a namespace grant change", "namespace", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range clones.Items {
		c := &clones.Items[i]
		if c.Namespace == obj.GetName() || cloneTargetNamespace(c) != obj.GetName() {
			continue
		}
		if c.Status.Phase == infrav1beta1.ClonePhaseReady || c.Status.Phase == infrav1beta1.ClonePhaseFailed {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(c)})
	}
	return reqs
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
	// spec.consumerNamespaceSelector is operator-side policy, not class data:
	// it is never sent to a provider.
	spec := vmClass.Spec
	spec.ConsumerNamespaceSelector = nil
	data, err := json.Marshal(spec)
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

// gateConsumers enforces spec.consumerNamespaceSelector for everything the
// clone uses from another namespace, reading through reader (the cache, or the
// live APIReader right before a side effect):
//
//   - the source VM's Provider, used from this namespace (the Clone RPC, the
//     task poll and the bind all go through it);
//   - the references buildTargetVM pins onto the target VirtualMachine — its
//     Provider and VMClass — used from the target namespace, so a clone never
//     produces a VirtualMachine that the VirtualMachine controller would
//     refuse to manage.
//
// allowed=true means every grant holds; any ConsumerNotAllowed refusal left by
// an earlier reconcile is then cleared. Otherwise the clone is marked refused
// and (result, err) is what the caller returns: a slow recheck (the grant
// watches re-drive it), or the read error so the controller retries with
// backoff while failing closed.
func (r *VMCloneReconciler) gateConsumers(
	ctx context.Context,
	reader client.Reader,
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	targetNamespace string,
) (allowed bool, result ctrl.Result, err error) {
	// The own namespace's Provider needs no grant (and a missing one is
	// reported by the caller's lookup).
	if key := vmProviderKey(sourceVM); key.Namespace != clone.Namespace {
		err = getForConsumer(ctx, reader, key, &infrav1beta1.Provider{}, clone.Namespace)
	}
	if err == nil {
		err = checkVMConsumerRefs(ctx, reader, r.buildTargetVM(clone, sourceVM, targetNamespace))
	}
	switch {
	case isConsumerNotAllowed(err):
		return false, r.markConsumerNotAllowed(ctx, clone, err), nil
	case err != nil:
		logging.FromContext(ctx).Error(err, "Failed to check the consumer grants; not proceeding")
		return false, ctrl.Result{}, err
	}
	if err := r.clearConsumerRefusal(ctx, clone); err != nil {
		return false, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}

// markConsumerNotAllowed records that the clone uses (or would pin onto its
// target) a Provider or VMClass in another namespace that does not select the
// namespace using it: Ready=False with reason ConsumerNotAllowed. Like the
// target-namespace refusal it is NOT terminal — a clone that has issued
// nothing yet waits in Pending, one in flight keeps its phase, task and target
// ID — and nothing already created is touched. The Warning event is emitted on
// the transition only, and the recheck is slow.
func (r *VMCloneReconciler) markConsumerNotAllowed(ctx context.Context, clone *infrav1beta1.VMClone, cause error) ctrl.Result {
	if consumerRefusalIsNew(clone.Status.Conditions, cause) {
		logging.FromContext(ctx).Info("VMClone uses a Provider or VMClass its namespace may not use; not cloning", "error", cause.Error())
		r.Recorder.Event(clone, corev1.EventTypeWarning, conditions.ReasonConsumerNotAllowed, cause.Error())
	}
	if clone.Status.TaskRef == "" && clone.Status.TargetVMID == "" {
		clone.Status.Phase = infrav1beta1.ClonePhasePending
	}
	clone.Status.Message = cause.Error()
	clone.Status.ObservedGeneration = clone.Generation
	meta.SetStatusCondition(&clone.Status.Conditions, metav1.Condition{
		Type:               infrav1beta1.VMCloneConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonConsumerNotAllowed,
		Message:            cause.Error(),
		ObservedGeneration: clone.Generation,
	})
	metrics.RecordError(errReasonConsumerNotAllowed, metrics.ComponentManager)
	_ = r.updateStatus(ctx, clone) //nolint:errcheck // status errors retried next reconcile
	return ctrl.Result{RequeueAfter: consumerNotAllowedRetryInterval}
}

// clearConsumerRefusal removes a ConsumerNotAllowed Ready condition once every
// grant holds, so the clone does not keep reporting a refusal while it
// proceeds. It persists immediately and is a no-op when there is nothing to
// clear.
func (r *VMCloneReconciler) clearConsumerRefusal(ctx context.Context, clone *infrav1beta1.VMClone) error {
	if !consumerRefused(clone.Status.Conditions) {
		return nil
	}
	meta.RemoveStatusCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	clone.Status.Message = "Cross-namespace access granted; resuming"
	clone.Status.ObservedGeneration = clone.Generation
	r.Recorder.Event(clone, corev1.EventTypeNormal, "ConsumerAllowed", clone.Status.Message)
	return r.updateStatus(ctx, clone)
}

// clonesRefusedAsConsumers maps a consumer-grant change to the unfinished
// VMClones refused with ConsumerNotAllowed. The namespace is not used to
// filter: a clone's refusal may concern its target namespace, not its own.
func (r *VMCloneReconciler) clonesRefusedAsConsumers(ctx context.Context, _ string) []reconcile.Request {
	clones := &infrav1beta1.VMCloneList{}
	if err := r.List(ctx, clones); err != nil {
		logging.FromContext(ctx).Error(err, "Failed to list VMClones for a consumer grant change")
		return nil
	}
	var reqs []reconcile.Request
	for i := range clones.Items {
		c := &clones.Items[i]
		if c.Status.Phase == infrav1beta1.ClonePhaseReady || c.Status.Phase == infrav1beta1.ClonePhaseFailed {
			continue
		}
		if consumerRefused(c.Status.Conditions) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager. Besides its own
// VMClones it watches Namespaces, but only for changes to the cross-namespace
// grant annotation, to re-drive clones whose target namespace just granted or
// revoked access; and consumer-grant changes (Namespace labels, the
// spec.consumerNamespaceSelector of Providers, VMClasses and VMImages) to
// re-drive clones refused with ConsumerNotAllowed.
func (r *VMCloneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1beta1.VMClone{}).
		Watches(&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.clonesTargetingNamespace),
			builder.WithPredicates(allowedSourceNamespacesChanged()))
	return withConsumerGrantWatches(b, r.clonesRefusedAsConsumers).
		Complete(r)
}
