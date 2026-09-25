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
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
)

// Reason labels used in metrics.RecordError calls for the VirtualMachine
// reconciler. Keep this taxonomy small and operationally meaningful — the
// `reason` label is cardinality-sensitive and drives operator alerting.
//
// Naming convention: kebab-case, prefix with the subsystem (`get-`, `deps-`,
// `provider-`, `remove-`). A new reason should describe WHAT went wrong, not
// WHICH return statement fired.
const (
	errReasonGetVM            = "get-vm"
	errReasonAddFinalizer     = "add-finalizer"
	errReasonRemoveFinalizer  = "remove-finalizer"
	errReasonDepsNotFound     = "deps-not-found"
	errReasonDepsError        = "deps-error"
	errReasonProviderResolve  = "provider-resolve"
	errReasonProviderDescribe = "provider-describe"
	errReasonProviderTask     = "provider-task-status"
	errReasonProviderDelete   = "provider-delete"
	errReasonImagePrepare     = "image-prepare"
	errReasonPlacement        = "placement"
	// errReasonProviderCreateRejected counts provider Create calls refused with
	// a NON-retryable error (Conflict / InvalidSpec) — notably a libvirt domain
	// name already taken by a domain this VirtualMachine does not own. A rising
	// count is a tenant-collision (or probing) signal worth alerting on.
	errReasonProviderCreateRejected = "provider-create-rejected"

	// errReasonProviderPower and errReasonProviderReconfigure count a
	// clustered VM's Power / Reconfigure that found its bound host unavailable
	// or the VM missing on it (ADR-0007 Addendum A, slice 2).
	errReasonProviderPower       = "provider-power"
	errReasonProviderReconfigure = "provider-reconfigure"
)

// vmCreateConflictRetryInterval is the requeue cadence after the provider
// refused Create with a Conflict (the provider-side name is taken by a VM this
// VirtualMachine does not own). Retrying cannot fix it — an operator must adopt
// or remove the colliding hypervisor VM, or rename this VirtualMachine — so it
// re-checks slowly instead of hammering the provider. Still bounded, so a
// collision resolved out-of-band is picked up without a manual nudge.
const vmCreateConflictRetryInterval = 2 * time.Minute

// vmCreateInvalidSpecRetryInterval is the requeue cadence after the provider
// rejected Create as InvalidSpec. A spec edit re-triggers reconcile on its own;
// the moderate cadence avoids a tight loop while staying tolerant of providers
// that classify some transient failures as InvalidSpec.
const vmCreateInvalidSpecRetryInterval = 30 * time.Second

// Placement requeue cadences for the clustered-provider create path (ADR-0007 P1,
// D4). The VirtualMachine controller does NOT watch Host / HostPool /
// VMPlacementPolicy (that would amplify every host heartbeat into a reconcile of
// every VM), so a RequeueAfter is how a blocked VM retries once the admin fixes
// the pool or capacity frees up. Both are deliberately unhurried — not artificial
// tight loops — so a namespace of unschedulable VMs cannot storm the scheduler.
const (
	// placementConfigRetryInterval requeues a VM blocked by a misconfiguration
	// that only an admin edit resolves (no/multiple HostPools, a dangling
	// VMPlacementPolicy, or a malformed scheduler input).
	placementConfigRetryInterval = 30 * time.Second
	// placementUnschedulableRetryInterval requeues a VM the scheduler could not
	// place (no feasible host), which may become schedulable when capacity frees
	// up or a cordoned host returns.
	placementUnschedulableRetryInterval = 30 * time.Second
)

// clusterPlacement is the successful outcome of scheduling a VirtualMachine onto a
// clustered provider's HostPool (ADR-0007 P1, D4): the chosen host, the pool it
// was scheduled into, and the scheduler's decision trace. It is threaded from the
// scheduler call to the post-Create binding write so status.placement is recorded
// ONLY after the provider confirms the VM (honesty-first, D3).
type clusterPlacement struct {
	hostID   string
	poolName string
	reason   string
}

// Clustered-VM lifecycle cadences (ADR-0007 Addendum A). Each is deliberately
// unhurried: the state it waits on changes on the order of minutes and needs a
// host to come back or an administrator to act, so a tight loop would only load
// the provider (no reconcile storms).
const (
	// placementUnboundRetryInterval re-checks a clustered VM that has a provider
	// id but no confirmed host binding (Placed=False/Unbound). No per-VM call is
	// sent while it waits.
	placementUnboundRetryInterval = 30 * time.Second
	// pendingHostUnavailableRetryInterval re-tries a Create whose pending host is
	// unreachable (Placed=False/HostUnavailable). The VM is never re-scheduled.
	pendingHostUnavailableRetryInterval = 30 * time.Second
	// vmMissingOnHostRetryInterval re-describes a clustered VM whose bound host
	// reports it missing (A4). It is never re-created; the re-check only notices
	// an administrator restoring the domain.
	vmMissingOnHostRetryInterval = 2 * time.Minute
	// routedOpNotSupportedRetryInterval re-checks a clustered VM whose provider
	// answers a routed per-VM call (Describe, Power, Reconfigure) with
	// Unimplemented — e.g. a manager on this version talking to an older
	// clustered provider image that does not route the RPC yet — so the
	// unsupported call is not hammered every few seconds (routedCallRetryAfter).
	routedOpNotSupportedRetryInterval = 2 * time.Minute
	// boundHostUnavailableRetryInterval re-checks a clustered VM whose bound
	// host is unknown, draining or unreachable (a host-scoped Unavailable on
	// Describe, Power or Reconfigure). A dead host must not turn every VM on it
	// into a 5s poll.
	boundHostUnavailableRetryInterval = 30 * time.Second
	// createConflictRescheduleInterval re-schedules a clustered VM whose Create
	// was refused on its pending host with a name conflict (the host is then
	// excluded for this VM). Each conflict removes one candidate host, so the
	// retries are bounded by the pool size; when every candidate is excluded the
	// VM waits on vmCreateConflictRetryInterval instead.
	createConflictRescheduleInterval = 5 * time.Second
)

// forceDeleteAnnotation, when set to "true" on a VirtualMachine, lets the
// finalizer be removed even if the provider Delete keeps failing. It is the
// operator escape hatch for a permanently-unreachable provider; by default a
// failed Delete retains the finalizer and retries so the hypervisor VM is never
// silently orphaned.
const forceDeleteAnnotation = "virtrigaud.io/force-delete"

// eventReasonOrphaned is the event reason recorded when a VirtualMachine is
// deleted with the orphan-on-delete annotation (the hypervisor VM is detached,
// not destroyed).
const eventReasonOrphaned = "Orphaned"

// vmDeleteRetryInterval is the requeue cadence while a provider Delete keeps
// failing (finalizer retained until it succeeds or force-delete is set).
const vmDeleteRetryInterval = 15 * time.Second

// providerErrorRetryInterval is the requeue cadence after a provider Create /
// image-prepare failure that may be transient (host unreachable, task error).
const providerErrorRetryInterval = 5 * time.Second

// providerFailureOutcome classifies a provider Create / image-prepare error into
// the VM condition reason and requeue cadence: a non-retryable InvalidSpec
// rejection (e.g. a libvirt image path outside the allowed image directories)
// surfaces as ValidationError on the vmCreateInvalidSpecRetryInterval recheck,
// which also picks up a corrected VMImage (the VM controller does not watch
// VMImage); anything else stays a ProviderError retried on the normal cadence.
func providerFailureOutcome(err error) (reason string, requeueAfter time.Duration) {
	if contracts.IsInvalidSpec(err) {
		return k8s.ReasonValidationError, vmCreateInvalidSpecRetryInterval
	}
	return k8s.ReasonProviderError, providerErrorRetryInterval
}

// providerErrorMessage renders err for a status condition. For a categorized
// provider error it uses the message alone, dropping the "(caused by: rpc
// error: ...)" suffix that repeats the same text; anything else renders as-is.
// Provider messages carry only request-derived values (never credentials).
func providerErrorMessage(err error) string {
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) && pe.Message != "" {
		return pe.Message
	}
	return err.Error()
}

// hasForceDeleteAnnotation reports whether the VM carries the force-delete
// escape-hatch annotation set to "true".
func hasForceDeleteAnnotation(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vm.Annotations[forceDeleteAnnotation] == "true"
}

// ProviderResolver resolves Provider resources to provider implementations.
// Implemented by *remote.Resolver in production; can be mocked in tests.
type ProviderResolver interface {
	GetProvider(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error)
}

// VirtualMachineReconciler reconciles VirtualMachine objects against the
// Provider each one references.
type VirtualMachineReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RemoteResolver ProviderResolver
	// Recorder emits Kubernetes events on the VirtualMachine (e.g. an
	// orphan-on-delete detach, a provider reference mismatch). Optional: nil
	// disables events.
	Recorder record.EventRecorder
	// ImageCRDFeatures reports whether the installed VMImage CRD can record
	// per-Provider image prepare state; while it verifiably cannot, VM creates
	// that need an image prepare are held without calling the provider (see
	// EnsureImageOnProvider). *VMCRDFeatureChecker implements it. Optional:
	// nil skips the check.
	ImageCRDFeatures VMImageCRDFeatureReporter

	// imagePrepares de-duplicates concurrent image prepares of one VMImage
	// through one Provider object within this manager (prepareImageOnce).
	imagePrepares singleflight.Group
}

// recordEvent emits an event on vm when a Recorder is configured.
func (r *VirtualMachineReconciler) recordEvent(vm *infravirtrigaudiov1beta1.VirtualMachine, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(vm, eventType, reason, msg)
	}
}

// reconcileBoundProvider reconciles a bound VM's status.boundProvider with the
// Provider its spec.providerRef resolved to (in memory; the caller's status
// write persists it):
//
//   - no record (bound before the field existed): record provider — trust on
//     first reconcile — with a Normal BoundProviderRecorded event;
//   - a different namespace/name: a *ProviderRefMismatchError (no provider
//     call may be made for the VM);
//   - the same namespace/name but a new UID (the Provider was deleted and
//     re-created): accepted, with a Warning BoundProviderRecreated event, and
//     the new UID recorded for audit.
func (r *VirtualMachineReconciler) reconcileBoundProvider(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider *infravirtrigaudiov1beta1.Provider,
) error {
	logger := log.FromContext(ctx)
	providerKey := types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}
	switch {
	case vm.Status.BoundProvider == nil:
		logger.Info("Recording the Provider this pre-existing bound VM is bound through (backfill from spec.providerRef)",
			"provider", providerKey.String(), "id", vm.Status.ID)
		recordBoundProvider(vm, provider)
		r.recordEvent(vm, corev1.EventTypeNormal, eventReasonBoundProviderRecorded, fmt.Sprintf(
			"recorded Provider %s (uid %s) as the Provider this VM is bound through, from its current spec.providerRef",
			providerKey, provider.UID))
	case checkVMProvider(vm, provider) != nil:
		return checkVMProvider(vm, provider)
	case boundProviderRecreated(vm, provider):
		oldUID := vm.Status.BoundProvider.UID
		logger.Info("The Provider this VM is bound through was re-created under the same name; accepting it and recording its new UID",
			"provider", providerKey.String(), "oldUID", oldUID, "newUID", string(provider.UID))
		r.recordEvent(vm, corev1.EventTypeWarning, eventReasonBoundProviderRecreated, fmt.Sprintf(
			"Provider %s this VM is bound through was deleted and re-created (uid %s -> %s); operating through the new object, "+
				"which is trusted to front the same hypervisor", providerKey, oldUID, provider.UID))
		vm.Status.BoundProvider.UID = string(provider.UID)
	}
	return nil
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmimages,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmimages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmnetworkattachments,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmmigrations,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hostpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=hosts,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmplacementpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile handles VirtualMachine reconciliation.
//
// Observability: every reconcile run records its duration and outcome to
// `virtrigaud_manager_reconcile_total{kind="VirtualMachine",outcome=...}`
// and `virtrigaud_manager_reconcile_duration_seconds{kind="VirtualMachine"}`
// via a deferred timer that infers outcome from the named return values.
// Specific error sites also record `virtrigaud_errors_total{reason=...,
// component="manager"}` so operators can dashboard the WHY behind requeues
// and error returns. See the `errReason*` constants above for the taxonomy.
//
// Named return values (`result`, `retErr`) are required by the deferred
// outcome-inference block — do not change the signature without updating
// the defer.
func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	timer := metrics.NewReconcileTimer("VirtualMachine")
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

	logger := log.FromContext(ctx)
	logger.Info("Reconciling VirtualMachine", "name", req.Name, "namespace", req.Namespace)

	// Fetch the VirtualMachine instance
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("VirtualMachine not found, assuming deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to fetch VirtualMachine")
		metrics.RecordError(errReasonGetVM, metrics.ComponentManager)
		return ctrl.Result{}, err
	}

	// Handle deletion
	if k8s.IsBeingDeleted(vm) {
		return r.handleDeletion(ctx, vm)
	}

	// Add finalizer if not present
	if !k8s.HasFinalizer(vm, infravirtrigaudiov1beta1.VirtualMachineFinalizer) {
		if err := k8s.AddFinalizer(ctx, r.Client, vm, infravirtrigaudiov1beta1.VirtualMachineFinalizer); err != nil {
			logger.Error(err, "Failed to add finalizer")
			metrics.RecordError(errReasonAddFinalizer, metrics.ComponentManager)
			return ctrl.Result{}, err
		}
		// Requeue to continue reconciliation
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile the VM
	return r.reconcileVM(ctx, vm)
}

// reconcileVM handles the main reconciliation logic
func (r *VirtualMachineReconciler) reconcileVM(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// The status as read, so a refusal that changes nothing is not re-written.
	persisted := vm.Status.DeepCopy()

	// Update observed generation
	vm.Status.ObservedGeneration = vm.Generation

	// A bound VM whose spec.providerRef no longer names the Provider it is
	// bound through is refused before anything is resolved: its status.id is
	// meaningful only on that Provider.
	if vmIsBound(vm) {
		if err := checkBoundProvider(vm, vmProviderKey(vm)); err != nil {
			return r.handleNotRoutable(ctx, vm, err)
		}
	}

	// Get dependencies
	imageRefName := ""
	if vm.Spec.ImageRef != nil {
		imageRefName = vm.Spec.ImageRef.Name
	} else if vm.Spec.ImportedDisk != nil {
		imageRefName = fmt.Sprintf("imported:%s", vm.Spec.ImportedDisk.DiskID)
	}
	logger.V(1).Info("Resolving VM dependencies", "provider", vm.Spec.ProviderRef.Name, "class", vm.Spec.ClassRef.Name, "image", imageRefName)
	deps, err := r.getDependencies(ctx, vm)
	if err != nil {
		// A Provider in another namespace that does not select this one
		// (spec.consumerNamespaceSelector) is refused before any provider is
		// resolved or called — also for a VM that is already bound (fail closed
		// after an upgrade that introduced the grant). So are the VMClass and
		// VMImage of a VM that is not bound yet.
		if isConsumerNotAllowed(err) {
			return r.refuseConsumer(ctx, vm, persisted, err)
		}
		// Check if Provider is missing - log at INFO level and skip reconciliation
		// Check both wrapped errors and error message for "not found"
		if errors.IsNotFound(err) || strings.Contains(err.Error(), "not found") {
			logger.Info("Provider not found, skipping reconciliation until Provider exists", "provider", vm.Spec.ProviderRef.Name, "error", err.Error())
			k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonWaitingForDependencies, fmt.Sprintf("Provider %s not found", vm.Spec.ProviderRef.Name))
			metrics.RecordError(errReasonDepsNotFound, metrics.ComponentManager)
			r.updateStatus(ctx, vm)
			// Requeue with longer interval when Provider is missing to reduce log noise
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		logger.Error(err, "Failed to get dependencies - will retry in 5s", "provider", vm.Spec.ProviderRef.Name, "class", vm.Spec.ClassRef.Name, "image", imageRefName)
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonWaitingForDependencies, err.Error())
		metrics.RecordError(errReasonDepsError, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	logger.V(1).Info("Dependencies resolved successfully")
	provider, vmClass, vmImage, networks := deps.provider, deps.class, deps.image, deps.networks

	// Bind the VM to the Provider it resolved to, or refuse it. A VM bound
	// before status.boundProvider existed is backfilled from its current
	// spec.providerRef (trust on first reconcile; the reference is immutable
	// once bound, so only a re-point made before the upgrade is trusted). A
	// Provider re-created under the same namespace and name is accepted, with a
	// Warning event, and its new UID recorded. Both records are persisted with
	// the status write that ends this reconcile.
	if vmIsBound(vm) {
		if err := r.reconcileBoundProvider(ctx, vm, provider); err != nil {
			return r.handleNotRoutable(ctx, vm, err)
		}
	}

	// A VM that records a clustered placement on a Provider that is not (or no
	// longer) clustered is failed CLOSED before any provider call — image
	// prepare, create or describe (ADR-0007 Addendum A).
	if err := placementTopologyError(vm, provider); err != nil {
		return r.handleNotRoutable(ctx, vm, err)
	}

	// Get provider instance (remote or in-process)
	logger.V(1).Info("Getting provider instance", "provider", provider.Name, "runtime_phase", provider.Status.Runtime.Phase, "endpoint", provider.Status.Runtime.Endpoint)
	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		logger.Error(err, "Failed to get provider instance - will retry in 5s", "provider", provider.Name, "runtime_phase", provider.Status.Runtime.Phase)
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, err.Error())
		metrics.RecordError(errReasonProviderResolve, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	logger.V(1).Info("Provider instance obtained successfully", "provider", provider.Name)

	// Provider liveness is already verified by getProviderInstance →
	// Resolver.GetProvider (it validates the cached/new client before returning).
	// Re-validating here doubled the real virsh-over-ssh Validate calls on every
	// reconcile, which — under 10-way concurrency post-adoption — became a fork
	// storm on the libvirt host (#288). The image-prepare / create RPCs below are
	// themselves the liveness test: a dead provider fails them and the reconcile
	// requeues.
	//
	// The referenced image is prepared on the provider only right before a
	// create (prepareImageForCreate, below): a VM that exists already never
	// sends an image prepare, so nothing about its image can stop describe,
	// power or reconfigure (no provider's Reconfigure reads the image).

	// Check if we have an active task
	if vm.Status.LastTaskRef != "" {
		done, err := providerInstance.IsTaskComplete(ctx, vm.Status.LastTaskRef)
		if err != nil {
			logger.Error(err, "Failed to check task status")
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to check task: %v", err))
			metrics.RecordError(errReasonProviderTask, metrics.ComponentManager)
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		if !done {
			logger.Info("Task still in progress", "taskRef", vm.Status.LastTaskRef)
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonTaskInProgress, "Task in progress")
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Task completed, clear it
		vm.Status.LastTaskRef = ""
	}

	// Check if we have an active reconfigure task
	if vm.Status.ReconfigureTaskRef != "" {
		done, err := providerInstance.IsTaskComplete(ctx, vm.Status.ReconfigureTaskRef)
		if err != nil {
			logger.Error(err, "Failed to check reconfigure task status")
			k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to check reconfigure task: %v", err))
			metrics.RecordError(errReasonProviderTask, metrics.ComponentManager)
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		if !done {
			logger.Info("Reconfigure task still in progress", "taskRef", vm.Status.ReconfigureTaskRef)
			k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonTaskInProgress, "Reconfiguration in progress")
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Reconfigure task completed, update current resources and clear task ref.
		// That reads the class: a class the VM's namespace may no longer use
		// keeps the task recorded until access is restored.
		if deps.classRefusal != nil {
			return r.refuseConsumer(ctx, vm, persisted, deps.classRefusal)
		}
		logger.Info("Reconfigure task completed", "taskRef", vm.Status.ReconfigureTaskRef)
		r.updateCurrentResources(vm, vmClass)
		vm.Status.ReconfigureTaskRef = ""
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM reconfigured successfully")
	}

	// Ensure VM exists.
	//
	// An adopted VM (labeled virtrigaud.io/adopted=true) has its underlying
	// hypervisor VM created by the adoption/clone controller, which then sets
	// Status.ID out-of-band. Between the CR appearing and that Status.ID write
	// landing, Status.ID is briefly empty. Creating here in that window would
	// produce a SECOND VM on the provider — the exact double-create the clone
	// controller's Status.ID write is meant to prevent. So we only enter the
	// create path for non-adopted VMs; an adopted VM with an empty Status.ID
	// waits for its ID to be set (issue #179).
	if vm.Status.ID == "" {
		if vmIsAdopted(vm) {
			logger.Info("Adopted VM has no Status.ID yet; waiting for adoption/clone controller to set it (not creating)",
				"name", vm.Name, "namespace", vm.Namespace)
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue,
				k8s.ReasonWaitingForDependencies, "Waiting for adoption/clone controller to set Status.ID")
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// A create uses the class and the image (a clustered create in flight
		// is bound, so its refusals were deferred to here).
		if err := deps.createRefusal(); err != nil {
			return r.refuseConsumer(ctx, vm, persisted, err)
		}
		// Prepare the image first — unless a clustered create is already in
		// flight (status.placement.pendingHost): that create is re-sent as it
		// was, and its image was prepared before the first attempt.
		if pendingHost(vm) == "" {
			if done, res, err := r.prepareImageForCreate(ctx, vm, persisted, vmImage, provider, providerInstance); done {
				return res, err
			}
		}
		logger.Info("Creating VM")
		return r.createVM(ctx, vm, providerInstance, provider, vmClass, vmImage, networks)
	}

	// Address the VM for every per-VM call below (ADR-0007 Addendum A, A1). A
	// single-host / thin-client provider gets the bare id, exactly as before; a
	// clustered VM gets its confirmed host, and one with no binding is never sent
	// a per-VM call — the provider must never pick a host.
	ref, err := vmRefFor(vm, provider)
	if err != nil {
		return r.handleNotRoutable(ctx, vm, err)
	}
	if ref.Routed() {
		setPlacedCondition(vm, metav1.ConditionTrue, k8s.ReasonBound,
			fmt.Sprintf("VM is bound to host %s", ref.HostID))
	}

	// VM exists, check current state
	desc, err := providerInstance.Describe(ctx, ref)
	if err != nil {
		if res, handled := r.handleRoutedOpError(ctx, vm, ref, err, errReasonProviderDescribe); handled {
			return res, nil
		}
		logger.Error(err, "Failed to describe VM")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to describe VM: %v", err))
		metrics.RecordError(errReasonProviderDescribe, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: routedCallRetryAfter(ref, err)}, nil
	}

	if !desc.Exists {
		if ref.Routed() {
			// ADR-0007 Addendum A, A4: a clustered VM is NEVER re-created — not on
			// the bound host, not elsewhere. Without fencing, a restart could leave
			// two running copies of one disk (D8).
			return r.handleMissingOnBoundHost(ctx, vm, ref, errReasonProviderDescribe)
		}
		// Re-creating uses the class and the image again: both must still be
		// usable from the VM's namespace.
		if err := deps.createRefusal(); err != nil {
			return r.refuseConsumer(ctx, vm, persisted, err)
		}
		logger.Info("VM no longer exists, recreating")
		vm.Status.ID = ""
		// A re-create is a create: the image is prepared (or re-validated)
		// first, exactly as for a new VM.
		if done, res, err := r.prepareImageForCreate(ctx, vm, persisted, vmImage, provider, providerInstance); done {
			return res, err
		}
		return r.createVM(ctx, vm, providerInstance, provider, vmClass, vmImage, networks)
	}

	// G7.2 (#127): record virtrigaud_ip_discovery_duration_seconds on
	// the first reconcile that observes the no-IPs → has-IPs transition
	// for this VM. Must be called BEFORE the vm.Status.IPs = desc.IPs
	// assignment below so the gate sees the pre-update value of
	// vm.Status.IPs. Idempotent across manager restarts because
	// vm.Status.IPs is persisted in etcd.
	recordIPDiscoveryIfFirstSeen(vm.Status.IPs, desc.IPs, vm.CreationTimestamp, string(provider.Spec.Type))

	// Update status with current state
	vm.Status.PowerState = infravirtrigaudiov1beta1.PowerState(desc.PowerState)
	vm.Status.IPs = desc.IPs
	vm.Status.ConsoleURL = desc.ConsoleURL
	vm.Status.Provider = desc.ProviderRaw

	// Check desired power state
	desiredPowerState := vm.Spec.PowerState
	if desiredPowerState == "" {
		desiredPowerState = infravirtrigaudiov1beta1.PowerStateOn
	}

	if desc.PowerState != string(desiredPowerState) {
		logger.Info("Power state mismatch, adjusting", "current", desc.PowerState, "desired", desiredPowerState)
		return r.adjustPowerState(ctx, vm, providerInstance, ref, string(desiredPowerState))
	}

	// Reconciling the VM to its class reads the class: a class the VM's
	// namespace may no longer use is refused here (describe and power above
	// are unaffected). A revoked image never stops a bound VM; it is simply not
	// sent with a reconfigure (vmImage is nil).
	if deps.classRefusal != nil {
		return r.refuseConsumer(ctx, vm, persisted, deps.classRefusal)
	}

	// Check if VMClass resources have changed and need reconfiguration
	if r.needsReconfigure(vm, vmClass) {
		logger.Info("VMClass resources changed, reconfiguring VM",
			"currentCPU", r.getCurrentCPU(vm),
			"desiredCPU", vmClass.Spec.CPU,
			"currentMemoryMiB", r.getCurrentMemoryMiB(vm),
			"desiredMemoryMiB", vmClass.Spec.Memory.Value()/(1024*1024))
		return r.reconfigureVM(ctx, vm, providerInstance, ref, provider, vmClass, vmImage, networks)
	}

	// VM is ready
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonReconcileSuccess, "VM is ready")
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM provisioned")

	r.updateStatus(ctx, vm)

	// Optimize polling frequency based on VM state
	return ctrl.Result{RequeueAfter: r.getRequeueInterval(vm, desc)}, nil
}

// handleDeletion handles VM deletion
func (r *VirtualMachineReconciler) handleDeletion(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !k8s.HasFinalizer(vm, infravirtrigaudiov1beta1.VirtualMachineFinalizer) {
		return ctrl.Result{}, nil
	}

	// Orphan-on-delete: detach the hypervisor VM instead of destroying it. No
	// provider is resolved or called; the finalizer is simply removed.
	if hasOrphanOnDeleteAnnotation(vm) {
		return r.orphanOnDelete(ctx, vm)
	}

	// Get provider if we have a provider ref and either a VM ID or a clustered
	// create in flight (status.placement.pendingHost, ADR-0007 Addendum A, A2).
	if vmIsBound(vm) && vm.Spec.ProviderRef.Name != "" {
		providerKey := vmProviderKey(vm)

		// A VM whose spec.providerRef no longer names the Provider it is bound
		// through is never deleted through the mismatched one (and, unlike a
		// Provider that is gone, the mismatch does not release the finalizer):
		// removing the CR takes orphan-on-delete or force-delete.
		if err := checkBoundProvider(vm, providerKey); err != nil {
			if res, retain := r.retainForUnroutableDelete(ctx, vm, err); retain {
				return res, nil
			}
			return r.removeFinalizer(ctx, vm)
		}

		// A Provider in another namespace that does not select this one — or
		// that does not exist, which is refused the same way — is never used to
		// delete the hypervisor VM, and, as for a provider reference mismatch,
		// the refusal does not release the finalizer: removing the CR takes
		// orphan-on-delete or force-delete (or the grant). A Provider in the
		// VM's own namespace that is gone releases it, as before.
		provider := &infravirtrigaudiov1beta1.Provider{}
		if err := getForConsumer(ctx, r.Client, providerKey, provider, vm.Namespace); err != nil {
			switch {
			case isConsumerNotAllowed(err):
				if res, retain := r.retainForUnroutableDelete(ctx, vm, err); retain {
					return res, nil
				}
				// force-delete: the finalizer is removed below without any provider call.
			case errors.IsNotFound(err):
				// Provider not found, continue with cleanup
			default:
				logger.Error(err, "Failed to get provider for deletion")
				metrics.RecordError(errReasonDepsError, metrics.ComponentManager)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		} else if ref, ok, res := r.deletionTarget(ctx, vm, provider); !ok {
			// deletionTarget decided (an unbound clustered VM, or one bound
			// through another Provider object, without the force-delete escape
			// hatch): retain the finalizer.
			return res, nil
		} else if ref.ID != "" {
			// Delete VM from provider
			providerInstance, err := r.getProviderInstance(ctx, provider)
			if err != nil {
				logger.Error(err, "Failed to get provider instance for deletion")
				metrics.RecordError(errReasonProviderResolve, metrics.ComponentManager)
			} else {
				logger.Info("Deleting VM from provider", "id", ref.ID, "host", ref.HostID)
				// A routed (clustered) ref carries the VM's owner: the provider
				// destroys the VM only when its owner stamp matches (A2). A
				// single-host ref is the bare id, unchanged.
				taskRef, err := providerInstance.Delete(ctx, ref)
				switch {
				case err == nil:
					if taskRef != "" {
						logger.Info("VM deletion initiated", "taskRef", taskRef)
						// TODO: Wait for task completion in future iterations
					}
				case contracts.IsNotFound(err):
					// The hypervisor VM is already gone — nothing to orphan, so
					// proceed to finalizer removal (idempotent delete). A clustered
					// provider also answers not-found for a domain this VM does not
					// own, which it never destroys.
					logger.Info("VM already absent from provider; proceeding with cleanup", "id", ref.ID)
				case hasForceDeleteAnnotation(vm):
					// Operator opted out of the safety gate: drop the finalizer even
					// though the provider VM may be left behind. Logged loudly.
					logger.Error(err, "Provider VM delete failed but force-delete annotation is set; removing finalizer (the provider VM may be orphaned)",
						"id", ref.ID, "annotation", forceDeleteAnnotation)
					metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)
				default:
					// Real failure (e.g. PVE "VM is running - destroy failed"). Do
					// NOT remove the finalizer — that would orphan the hypervisor VM.
					// Retain it and requeue so the delete is retried; an operator can
					// set the force-delete annotation to break out if needed.
					logger.Error(err, "Failed to delete VM from provider; retaining finalizer and retrying",
						"id", ref.ID)
					metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)
					return ctrl.Result{RequeueAfter: vmDeleteRetryInterval}, nil
				}
			}
		}
	}

	return r.removeFinalizer(ctx, vm)
}

// removeFinalizer removes the VirtualMachine finalizer, completing deletion.
func (r *VirtualMachineReconciler) removeFinalizer(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if err := k8s.RemoveFinalizer(ctx, r.Client, vm, infravirtrigaudiov1beta1.VirtualMachineFinalizer); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		metrics.RecordError(errReasonRemoveFinalizer, metrics.ComponentManager)
		return ctrl.Result{}, err
	}

	logger.Info("VirtualMachine deleted successfully")
	return ctrl.Result{}, nil
}

// orphanOnDelete completes the deletion of a VirtualMachine that carries the
// orphan-on-delete annotation: the finalizer is removed WITHOUT resolving or
// calling any Provider, so the hypervisor VM (if any) is left in place,
// untouched and no longer managed. This is the supported way to un-adopt /
// detach a VM. It is logged and recorded as an event on the VM.
func (r *VirtualMachineReconciler) orphanOnDelete(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, error) {
	provider := vmProviderKey(vm)
	if b := vm.Status.BoundProvider; b != nil {
		provider = types.NamespacedName{Namespace: b.Namespace, Name: b.Name}
	}
	msg := fmt.Sprintf("%s=true: detaching without deleting the hypervisor VM (id %q, provider %s); it is left in place and no longer managed",
		infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, vm.Status.ID, provider)
	log.FromContext(ctx).Info("Orphan-on-delete: removing the finalizer without a provider Delete",
		"id", vm.Status.ID, "provider", provider.String(), "annotation", infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
	r.recordEvent(vm, corev1.EventTypeNormal, eventReasonOrphaned, msg)
	return r.removeFinalizer(ctx, vm)
}

// vmDependencies are the objects a VirtualMachine references, resolved for
// one reconcile.
type vmDependencies struct {
	provider *infravirtrigaudiov1beta1.Provider
	class    *infravirtrigaudiov1beta1.VMClass
	image    *infravirtrigaudiov1beta1.VMImage
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment

	// classRefusal and imageRefusal hold, for a BOUND VM, the
	// *ConsumerNotAllowedError of a VMClass / VMImage in another namespace
	// that does not (or no longer) select the VM's; class / image is then nil.
	// Their grant is enforced where their content is used — the class at a
	// reconfigure, both at a (re-)create — so revoking a share does not stop
	// describe and power of a VM created from it. See getDependencies.
	classRefusal, imageRefusal error
}

// createRefusal is the refusal that keeps deps from being used to create (or
// re-create) the VM: the class's, then the image's; nil when both may be used.
func (d vmDependencies) createRefusal() error {
	if d.classRefusal != nil {
		return d.classRefusal
	}
	return d.imageRefusal
}

// getDependencies fetches all required dependencies for the VM.
//
// The Provider, VMClass and VMImage may be in another namespace only when that
// object's spec.consumerNamespaceSelector selects the VM's namespace
// (getForConsumer); a cross-namespace object that does not exist is refused
// the same way. When to refuse:
//
//   - the Provider: always — every provider call goes through it, so an
//     ungranted Provider returns a *ConsumerNotAllowedError and the caller
//     makes no provider call, bound VM or not;
//   - the VMClass and VMImage of an UNBOUND VM: here, before anything is
//     created from them;
//   - the VMClass and VMImage of a BOUND VM: only where their content is used
//     (vmDependencies.classRefusal / imageRefusal). The VM exists already, so
//     describing and powering it uses neither.
//
// Networks are always in the VM's own namespace.
func (r *VirtualMachineReconciler) getDependencies(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (vmDependencies, error) {
	var deps vmDependencies
	bound := vmIsBound(vm)

	// Get Provider
	provider := &infravirtrigaudiov1beta1.Provider{}
	providerKey := vmProviderKey(vm)
	if err := getForConsumer(ctx, r.Client, providerKey, provider, vm.Namespace); err != nil {
		if isConsumerNotAllowed(err) {
			return deps, err
		}
		if errors.IsNotFound(err) {
			// Provider doesn't exist yet - preserve the NotFound error for proper handling upstream
			return deps, fmt.Errorf("provider %s not found (namespace: %s): %w", vm.Spec.ProviderRef.Name, providerKey.Namespace, err)
		}
		return deps, fmt.Errorf("failed to get provider %s: %w", vm.Spec.ProviderRef.Name, err)
	}
	deps.provider = provider

	// Get VMClass
	vmClass := &infravirtrigaudiov1beta1.VMClass{}
	classKey := types.NamespacedName{Namespace: vm.Namespace, Name: vm.Spec.ClassRef.Name}
	if key, ok := vmClassKey(vm); ok {
		classKey = key
	}
	if err := getForConsumer(ctx, r.Client, classKey, vmClass, vm.Namespace); err != nil {
		switch {
		case isConsumerNotAllowed(err) && bound:
			deps.classRefusal, vmClass = err, nil
		case isConsumerNotAllowed(err):
			return deps, err
		default:
			return deps, fmt.Errorf("failed to get vmclass %s: %w", vm.Spec.ClassRef.Name, err)
		}
	}
	deps.class = vmClass

	// Get VMImage (only if ImageRef is specified, not ImportedDisk)
	if imageKey, ok := vmImageKey(vm); ok {
		vmImage := &infravirtrigaudiov1beta1.VMImage{}
		if err := getForConsumer(ctx, r.Client, imageKey, vmImage, vm.Namespace); err != nil {
			switch {
			case isConsumerNotAllowed(err) && bound:
				deps.imageRefusal, vmImage = err, nil
			case isConsumerNotAllowed(err):
				return deps, err
			default:
				return deps, fmt.Errorf("failed to get vmimage %s: %w", vm.Spec.ImageRef.Name, err)
			}
		}
		deps.image = vmImage
	} else if vm.Spec.ImageRef != nil {
		// An image reference with an empty name names nothing: report it the
		// way the lookup always has.
		return deps, fmt.Errorf("failed to get vmimage %q: empty name", vm.Spec.ImageRef.Name)
	}

	// Get VMNetworkAttachments (only for networks that have networkRef specified)
	var networks []*infravirtrigaudiov1beta1.VMNetworkAttachment
	for _, netRef := range vm.Spec.Networks {
		if netRef.NetworkRef != nil {
			network := &infravirtrigaudiov1beta1.VMNetworkAttachment{}
			netKey := types.NamespacedName{
				Name:      netRef.NetworkRef.Name,
				Namespace: vm.Namespace,
			}
			if err := r.Get(ctx, netKey, network); err != nil {
				return deps, fmt.Errorf("failed to get vmnetworkattachment %s: %w", netRef.NetworkRef.Name, err)
			}
			networks = append(networks, network)
		} else {
			// No networkRef - append nil to maintain index alignment with vm.Spec.Networks
			networks = append(networks, nil)
		}
	}

	deps.networks = networks
	return deps, nil
}

// createVM creates a new VM using the provider.
//
// For a clustered ("brain-in-operator") provider (providerCR.Spec.Topology ==
// cluster, ADR-0007 P1) it first schedules the VM onto a host in the provider's
// HostPool and threads the chosen host to the wire as CreateRequest.TargetHostID
// (D4), then records the binding on status.placement ONLY after Create confirms
// the VM (honesty-first, D3). For a single-host / thin-client provider (the
// default topology) this is byte-for-byte the original path: no scheduling, an
// empty TargetHostID, and no status.placement write.
func (r *VirtualMachineReconciler) createVM(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider contracts.Provider,
	providerCR *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Validate that either ImageRef or ImportedDisk is specified
	if vm.Spec.ImageRef == nil && vm.Spec.ImportedDisk == nil {
		err := fmt.Errorf("either imageRef or importedDisk must be specified")
		logger.Error(err, "Invalid VM specification")
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, err.Error())
		r.updateStatus(ctx, vm)
		return ctrl.Result{}, err
	}

	// Validate mutual exclusivity
	if vm.Spec.ImageRef != nil && vm.Spec.ImportedDisk != nil {
		err := fmt.Errorf("imageRef and importedDisk are mutually exclusive")
		logger.Error(err, "Invalid VM specification")
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, err.Error())
		r.updateStatus(ctx, vm)
		return ctrl.Result{}, err
	}

	// Build create request
	req, err := r.buildCreateRequest(ctx, vm, providerCR, vmClass, vmImage, networks)
	if err != nil {
		logger.Error(err, "Failed to build create request")
		if contracts.IsInvalidSpec(err) {
			// A VMClass value out of range (e.g. memory at or above its
			// maximum) only changes with an edit: surface it as a validation
			// error and re-check on the slower spec cadence.
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonValidationError,
				fmt.Sprintf("Failed to build create request: %s", providerErrorMessage(err)))
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: vmCreateInvalidSpecRetryInterval}, nil
		}
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to build create request: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// ADR-0007 P1 (D4): a clustered provider's VMs are scheduled onto a specific
	// host by the operator BEFORE Create. Resolve the host and set
	// req.TargetHostID here; a single-host / thin-client provider skips this
	// entirely (topology defaults to single), leaving TargetHostID empty and
	// status.placement untouched — today's path, unchanged.
	clustered := isClusterTopology(providerCR)
	if clustered {
		host := pendingHost(vm)
		if host == "" {
			p, res, perr := r.resolveClusterPlacement(ctx, vm, providerCR, req, networks)
			if perr != nil {
				// An unexpected infrastructure error (e.g. a List failed). Bubble it
				// so the reconcile records an error outcome and backs off; do NOT
				// create.
				return ctrl.Result{}, perr
			}
			if p == nil {
				// Not schedulable or misconfigured: resolveClusterPlacement has
				// already set the Provisioning=False condition and persisted
				// status. Requeue WITHOUT creating — never bind a host the
				// scheduler did not choose.
				return res, nil
			}
			// ADR-0007 Addendum A, A2: durably record the attempted host BEFORE
			// Create, so a retry after a lost status write (or a Create that ran
			// past its deadline) lands on this same host instead of a second one.
			if res, recorded, rerr := r.recordPendingHost(ctx, vm, providerCR, p); !recorded {
				return res, rerr
			}
			host = p.hostID
		} else {
			// A create is already in flight on host: reuse it as-is. The
			// scheduler is NOT re-run for such a VM (A2).
			logger.Info("Retrying clustered create on its pending host (not re-scheduling)", "host", host)
		}
		req.TargetHostID = host
	}

	// Create VM
	resp, err := provider.Create(ctx, req)
	if err != nil && clustered {
		return r.handleClusteredCreateError(ctx, vm, req.TargetHostID, err)
	}
	if err != nil {
		// Honesty-first (ADR-0007 D3): Create failed, so we do NOT write
		// status.placement — it stays whatever it was (unset on a first attempt),
		// never claiming a host the provider has not accepted the VM on. Nor do
		// we write Status.ID: a VM whose create was refused is bound to nothing,
		// so deleting it never reaches provider.Delete (handleDeletion gates on
		// Status.ID) and cannot touch the colliding hypervisor VM.
		if res, handled := r.handleRejectedCreate(ctx, vm, err); handled {
			return res, nil
		}
		logger.Error(err, "Failed to create VM")
		reason, requeueAfter := providerFailureOutcome(err)
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, reason,
			fmt.Sprintf("Failed to create VM: %s", providerErrorMessage(err)))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Update status. The Provider the VM is bound through is recorded in the
	// same status write as its id.
	vm.Status.ID = resp.ID
	recordBoundProvider(vm, providerCR)
	// Initialize current resources to track for future resize detection
	r.updateCurrentResources(vm, vmClass)

	// Record the placement binding now that the provider has confirmed the VM is
	// on the chosen host (ADR-0007 D3, honesty-first): promote pendingHost into
	// host and clear pendingHost (A2). Set only on the clustered path;
	// single-host VMs leave status.placement nil.
	if clustered {
		promotePendingHost(vm, req.TargetHostID)
	}

	if resp.TaskRef != "" {
		vm.Status.LastTaskRef = resp.TaskRef
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonCreating, "VM creation initiated")
	} else {
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM created")
	}

	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleRejectedCreate handles a provider Create that failed with a
// NON-retryable error and reports whether it did. Two classes qualify:
//
//   - Conflict (gRPC AlreadyExists): the provider-side name is already taken by
//     a VM this VirtualMachine does not own — e.g. a libvirt domain of the same
//     name created by another namespace/tenant, or never created by VirtRigaud.
//     The provider deliberately refused to bind to it.
//   - InvalidSpec (gRPC InvalidArgument): the request can never succeed as-is —
//     e.g. a VirtualMachine name libvirt would resolve as a domain ID or UUID.
//
// Both set Ready=False and Provisioning=False with a specific reason and the
// provider's (non-secret) message, stamped with ObservedGeneration, and requeue
// on a slower cadence than the 5s transient-error path
// (vmCreateConflictRetryInterval / vmCreateInvalidSpecRetryInterval), so an
// unresolvable create does not hot-loop against the provider.
// Status.ID is left empty. Any other error returns handled=false and keeps the
// existing transient-retry path.
func (r *VirtualMachineReconciler) handleRejectedCreate(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	err error,
) (ctrl.Result, bool) {
	var (
		reason     string
		retryAfter time.Duration
	)
	switch {
	case contracts.IsConflict(err):
		reason, retryAfter = k8s.ReasonProviderConflict, vmCreateConflictRetryInterval
	case contracts.IsInvalidSpec(err):
		reason, retryAfter = k8s.ReasonValidationError, vmCreateInvalidSpecRetryInterval
	default:
		return ctrl.Result{}, false
	}

	// Surface the provider's categorized message, not err.Error(): the latter
	// also embeds the raw gRPC status chain, which is noise in a condition.
	msg := err.Error()
	var pe *contracts.ProviderError
	if stderrors.As(err, &pe) && pe.Message != "" {
		msg = pe.Message
	}
	msg = fmt.Sprintf("Provider rejected VM create (non-retryable; re-checking every %s): %s", retryAfter, msg)

	log.FromContext(ctx).Info("Provider rejected VM create with a non-retryable error; not binding and backing off",
		"reason", reason, "retryAfter", retryAfter.String(), "error", err.Error())
	metrics.RecordError(errReasonProviderCreateRejected, metrics.ComponentManager)

	for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: vm.Generation,
		})
	}
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: retryAfter}, true
}

// resolveClusterPlacement schedules vm onto a host in providerCR's HostPool for a
// clustered provider (ADR-0007 P1, D4) and returns the chosen binding.
//
// Contract:
//   - success → (*clusterPlacement, ctrl.Result{}, nil); the caller sends the host
//     as CreateRequest.TargetHostID and writes status.placement after Create.
//   - not schedulable / misconfigured → (nil, requeueResult, nil); this function
//     has ALREADY set the Provisioning=False condition and persisted status, and
//     the caller must requeue WITHOUT calling Create (never bind a host the
//     scheduler did not choose).
//   - unexpected infrastructure error (a List/Get failed) → (nil, ctrl.Result{},
//     err); the caller bubbles it.
//
// It never mutates any Host / HostPool / VMPlacementPolicy (read-only inputs) and
// never writes status.placement (that is the caller's post-Create job).
func (r *VirtualMachineReconciler) resolveClusterPlacement(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	req contracts.CreateRequest,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) (*clusterPlacement, ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Clustered objects (HostPool, Host, their credential Secrets) live in the
	// clustered Provider's namespace — the same-namespace model enforced by the
	// Host/HostPool schema (LocalObjectReference providerRef). providerCR was
	// resolved from vm.spec.providerRef (namespace defaulting to the VM's), so
	// its namespace — NOT the VM's — is where the pool and hosts are looked up.
	// Looking in the VM's namespace instead would (a) fail to find the pool for
	// a VM that references a Provider in another namespace, and (b) let a
	// tenant's own namespace-local HostPool/Hosts that merely share the
	// Provider's NAME steer placement for someone else's Provider.
	clusterNS := providerCR.Namespace

	// (a) Resolve the provider's HostPool. v1 assumes EXACTLY ONE HostPool per
	// clustered provider; zero or many is a misconfiguration the operator refuses
	// to guess through (multi-pool selection is a deferred follow-up).
	var poolList infravirtrigaudiov1beta1.HostPoolList
	if err := r.List(ctx, &poolList, client.InNamespace(clusterNS)); err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("list HostPools in namespace %s: %w", clusterNS, err)
	}
	var pools []*infravirtrigaudiov1beta1.HostPool
	for i := range poolList.Items {
		p := &poolList.Items[i]
		// Defense in depth: the List is namespace-scoped, but a pool belongs to
		// this Provider only if it is in the Provider's namespace AND names it.
		if p.Namespace == clusterNS && p.Spec.ProviderRef.Name == providerCR.Name {
			pools = append(pools, p)
		}
	}
	switch {
	case len(pools) == 0:
		msg := fmt.Sprintf("clustered provider %q has no HostPool in namespace %s", providerCR.Name, clusterNS)
		logger.Info("Cannot schedule VM: " + msg)
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonNoHostPool, msg)
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return nil, ctrl.Result{RequeueAfter: placementConfigRetryInterval}, nil
	case len(pools) > 1:
		msg := fmt.Sprintf("clustered provider %q has %d HostPools in namespace %s; v1 supports exactly one pool per clustered provider (multi-pool is deferred)", providerCR.Name, len(pools), clusterNS)
		logger.Info("Cannot schedule VM: " + msg)
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonMultipleHostPools, msg)
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return nil, ctrl.Result{RequeueAfter: placementConfigRetryInterval}, nil
	}
	pool := pools[0]

	// (b) List the pool's candidate Hosts (with their live status). The scheduler
	// filters/scores them; it does not fetch them.
	// Hosts are looked up in the Provider's namespace, and a candidate must be
	// in the chosen pool AND fronted by this Provider: only such a host is in
	// the Provider's rendered inventory, so binding any other would send the
	// provider a TargetHostID it cannot route.
	var hostList infravirtrigaudiov1beta1.HostList
	if err := r.List(ctx, &hostList, client.InNamespace(clusterNS)); err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("list Hosts in namespace %s: %w", clusterNS, err)
	}
	var candidates []infravirtrigaudiov1beta1.Host
	for i := range hostList.Items {
		h := &hostList.Items[i]
		if h.Namespace == clusterNS && h.Spec.PoolRef.Name == pool.Name && h.Spec.ProviderRef.Name == providerCR.Name {
			c := *h
			if !c.DeletionTimestamp.IsZero() {
				// A Host being deleted is treated as cordoned: new VMs must not be
				// placed on it, or they would keep re-arming its in-use finalizer
				// and block the deletion indefinitely (ADR-0007 Addendum A, A1).
				c.Spec.Schedulable = false
			}
			candidates = append(candidates, c)
		}
	}

	// (c) Resolve the optional VMPlacementPolicy (spec.placementRef). A dangling
	// ref is a misconfiguration, not "no policy".
	var policy *infravirtrigaudiov1beta1.VMPlacementPolicy
	if vm.Spec.PlacementRef != nil {
		policy = &infravirtrigaudiov1beta1.VMPlacementPolicy{}
		key := types.NamespacedName{Name: vm.Spec.PlacementRef.Name, Namespace: vm.Namespace}
		if err := r.Get(ctx, key, policy); err != nil {
			if errors.IsNotFound(err) {
				msg := fmt.Sprintf("referenced VMPlacementPolicy %q not found in namespace %s", vm.Spec.PlacementRef.Name, vm.Namespace)
				logger.Info("Cannot schedule VM: " + msg)
				k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonPlacementPolicyNotFound, msg)
				metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
				r.updateStatus(ctx, vm)
				return nil, ctrl.Result{RequeueAfter: placementConfigRetryInterval}, nil
			}
			return nil, ctrl.Result{}, fmt.Errorf("get VMPlacementPolicy %s: %w", vm.Spec.PlacementRef.Name, err)
		}
	}

	// (d) Build the pool's already-placed VM set (name -> host + labels) for VM
	// (anti-)affinity and per-host bound counts, EXCLUDING this VM. A VM counts
	// only once it carries a confirmed binding into this same pool.
	var vmList infravirtrigaudiov1beta1.VirtualMachineList
	if err := r.List(ctx, &vmList, client.InNamespace(vm.Namespace)); err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("list VirtualMachines in namespace %s: %w", vm.Namespace, err)
	}
	var placedVMs []scheduler.PlacedVM
	for i := range vmList.Items {
		other := &vmList.Items[i]
		if other.Name == vm.Name {
			continue
		}
		pl := other.Status.Placement
		if pl == nil || pl.Pool != pool.Name || pl.Host == "" {
			continue
		}
		placedVMs = append(placedVMs, scheduler.PlacedVM{
			Name:   other.Name,
			HostID: pl.Host,
			Labels: other.Labels,
		})
	}

	// CurrentBinding drives the scheduler's D4 idempotent re-selection: a VM
	// already bound to a still-feasible host re-selects it without churn.
	// ExcludedHosts are the hosts where a Create of this VM was refused with a
	// name conflict (ADR-0007 Addendum A, A2 amendment); they are never chosen.
	currentBinding := ""
	var excludedHosts []string
	if vm.Status.Placement != nil {
		currentBinding = vm.Status.Placement.Host
		excludedHosts = vm.Status.Placement.ExcludedHosts
	}

	// (e) Schedule. Resources reuse the CPU/memory buildCreateRequest already
	// resolved (no duplicate parse). RequiredNetworks are the VM's resolved
	// network identities as D6 host-visibility constraints. RequiredStoragePools
	// and RequiredMachineType are deliberately left empty — see the TODO below.
	result, err := scheduler.Schedule(scheduler.Request{
		Resources: scheduler.ResourceRequest{
			CPU:       req.Class.CPU,
			MemoryMiB: int64(req.Class.MemoryMiB),
		},
		Policy:           policy,
		Pool:             pool.Spec,
		Candidates:       candidates,
		CurrentBinding:   currentBinding,
		PlacedVMs:        placedVMs,
		ExcludedHosts:    excludedHosts,
		RequiredNetworks: requiredNetworksForScheduling(networks),
		// TODO(ADR-0007 D6): wire RequiredStoragePools and RequiredMachineType once
		// there is an unambiguous mapping. A VM's disks carry no storage-pool name
		// (DiskSpec.StorageClass is a k8s StorageClass, not a libvirt/NFS pool) and
		// VMClass carries no machine type (only Firmware), so inventing either
		// mapping would be a bug. The scheduler treats empty as "no constraint",
		// which is the honest, correct behavior until those inputs exist.
	})
	if err != nil {
		if scheduler.AllExcluded(err) {
			// Every candidate is a host where a Create of this VM was refused with
			// a name conflict. Nothing the operator can do on its own will change
			// that, so report it on Placed and re-check slowly — no hot loop.
			msg := fmt.Sprintf("every candidate host in pool %q is excluded for this VM (a create there was refused: "+
				"a same-named domain this VirtualMachine does not own exists on it): %s. Resolve the name conflicts "+
				"(or rename the VirtualMachine) and clear status.placement.excludedHosts",
				pool.Name, strings.Join(excludedHosts, ", "))
			logger.Info("Cannot schedule VM: " + msg)
			setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonAllHostsExcluded, msg)
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonUnschedulable, msg)
			metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
			r.updateStatus(ctx, vm)
			return nil, ctrl.Result{RequeueAfter: vmCreateConflictRetryInterval}, nil
		}
		if stderrors.Is(err, scheduler.ErrNoFeasibleHost) {
			// Capacity/visibility/affinity eliminated every host. The error's
			// message carries the per-host breakdown; surface it and requeue.
			msg := fmt.Sprintf("no feasible host in pool %q: %v", pool.Name, err)
			logger.Info("Cannot schedule VM: " + msg)
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonUnschedulable, msg)
			metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
			r.updateStatus(ctx, vm)
			return nil, ctrl.Result{RequeueAfter: placementUnschedulableRetryInterval}, nil
		}
		// Malformed input the admin must fix (bad overcommit ratio / affinity
		// selector). Requeueing will not help until the policy/pool is corrected,
		// but surface it and retry slowly rather than hot-looping.
		msg := fmt.Sprintf("placement error in pool %q: %v", pool.Name, err)
		logger.Error(err, "Placement error scheduling VM", "pool", pool.Name)
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonPlacementError, msg)
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return nil, ctrl.Result{RequeueAfter: placementConfigRetryInterval}, nil
	}

	logger.Info("Scheduled VM onto clustered host",
		"vm", vm.Name, "pool", pool.Name, "host", result.HostID, "reason", result.Reason)
	return &clusterPlacement{hostID: result.HostID, poolName: pool.Name, reason: result.Reason}, ctrl.Result{}, nil
}

// requiredNetworksForScheduling derives the ADR-0007 D6 network-visibility
// constraints for the scheduler from the VM's resolved network attachments. Each
// attachment's libvirt network identity — the libvirt network name, or the bridge
// name when no network name is set — becomes a required Host label
// (scheduler.LabelNetworkPrefix + name): only a host that carries that network is
// a feasible placement. Attachments with no networkRef (a template's pre-wired
// NIC) or no libvirt identity contribute no constraint (an honest empty), and
// duplicates are collapsed so one required network is emitted per distinct
// identity.
//
// Only the libvirt attachment shape is mapped, because topology: cluster is valid
// only on libvirt (and, later, cloud-hypervisor) providers (ADR-0007 D2/D9); the
// vSphere/Proxmox network shapes never reach a clustered create path.
func requiredNetworksForScheduling(networks []*infravirtrigaudiov1beta1.VMNetworkAttachment) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, net := range networks {
		if net == nil || net.Spec.Network.Libvirt == nil {
			continue
		}
		name := net.Spec.Network.Libvirt.NetworkName
		if name == "" && net.Spec.Network.Libvirt.Bridge != nil {
			name = net.Spec.Network.Libvirt.Bridge.Name
		}
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// adjustPowerState adjusts the power state of the VM ref addresses.
func (r *VirtualMachineReconciler) adjustPowerState(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider contracts.Provider,
	ref contracts.VMRef,
	desiredState string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var powerOp contracts.PowerOp
	switch desiredState {
	case "On":
		powerOp = contracts.PowerOpOn
	case "Off":
		powerOp = contracts.PowerOpOff
	case "OffGraceful":
		powerOp = contracts.PowerOpShutdownGraceful
	default:
		logger.Error(nil, "Unsupported power state", "state", desiredState)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	taskRef, err := provider.Power(ctx, ref, powerOp)
	if err != nil {
		// A clustered VM's host-scoped unavailability or not-found (including a
		// domain whose owner stamp is not this VM's) is handled like the same
		// answer to Describe (ADR-0007 Addendum A, slice 2).
		if res, handled := r.handleRoutedOpError(ctx, vm, ref, err, errReasonProviderPower); handled {
			return res, nil
		}
		logger.Error(err, "Failed to adjust power state")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to adjust power state: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: routedCallRetryAfter(ref, err)}, nil
	}

	if taskRef != "" {
		vm.Status.LastTaskRef = taskRef
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, "Adjusting power state")
	}

	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// isOwnMigrationDisk reports whether diskPath is verifiably the landing disk a
// VMMigration imported FOR THIS VM, which is the only case in which the
// provider may attach a disk in place (contracts.VMImage.ImportedDisk).
//
// vm.spec.importedDisk (path and migrationRef) is tenant-writable and VM names
// repeat across namespaces, so the spec alone proves nothing. The decision
// rests on the referenced VMMigration, looked up in the VM's OWN namespace:
// its spec must target this VM (name, and namespace — Target.Namespace or the
// migration's own), and its status — which tenants cannot write — must record
// diskPath as the imported disk (status.diskInfo.targetPath). A missing ref, a
// migration that does not exist, or any mismatch yields false: the provider
// then treats the path as a base image (confined, then copied or rejected).
// Only an API error other than NotFound is returned, so a transient failure
// retries instead of silently downgrading an honest migration.
func (r *VirtualMachineReconciler) isOwnMigrationDisk(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	diskPath string,
) (bool, error) {
	disk := vm.Spec.ImportedDisk
	if disk == nil || disk.MigrationRef == nil || disk.MigrationRef.Name == "" || diskPath == "" {
		return false, nil
	}
	migration := &infravirtrigaudiov1beta1.VMMigration{}
	key := types.NamespacedName{Name: disk.MigrationRef.Name, Namespace: vm.Namespace}
	if err := r.Get(ctx, key, migration); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get VMMigration %s referenced by importedDisk: %w", key, err)
	}
	targetNamespace := migration.Spec.Target.Namespace
	if targetNamespace == "" {
		targetNamespace = migration.Namespace
	}
	di := migration.Status.DiskInfo
	return migration.Spec.Target.Name == vm.Name &&
		targetNamespace == vm.Namespace &&
		di != nil && di.TargetPath != "" && di.TargetPath == diskPath, nil
}

// buildCreateRequest builds a provider create request from VM spec.
// It resolves cloud-init user data and metadata from both inline content and Secret references.
// providerCR is the Provider the request is for: an image prepared through it
// is consumed at its prepared location (overrideImageWithPreparedLocation). A
// nil providerCR keeps the image's original source.
func (r *VirtualMachineReconciler) buildCreateRequest(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) (contracts.CreateRequest, error) {
	log := ctrl.Log.WithName("buildCreateRequest")

	// Check if using imported disk or template
	usingImportedDisk := vm.Spec.ImportedDisk != nil

	if usingImportedDisk {
		log.V(1).Info("buildCreateRequest called with imported disk",
			"vm", vm.Name,
			"diskID", vm.Spec.ImportedDisk.DiskID,
			"format", vm.Spec.ImportedDisk.Format,
			"source", vm.Spec.ImportedDisk.Source)
	} else if vmImage != nil {
		log.V(1).Info("buildCreateRequest called with vmImage template",
			"vm", vm.Name,
			"vmImage", vmImage.Name,
			"hasLibvirtSource", vmImage.Spec.Source.Libvirt != nil,
			"hasVSphereSource", vmImage.Spec.Source.VSphere != nil,
			"hasProxmoxSource", vmImage.Spec.Source.Proxmox != nil)
	}

	// Convert VMClass. Memory and the default disk size are range-checked
	// before the int32 conversion: an out-of-range value is an InvalidSpec
	// error, never a wrapped-around size.
	memoryMiB, err := vmClassQuantityUnits("memory", vmClass.Spec.Memory, bytesPerMiB, maxVMClassMemoryBytes)
	if err != nil {
		return contracts.CreateRequest{}, err
	}
	class := contracts.VMClass{
		CPU:              vmClass.Spec.CPU,
		MemoryMiB:        memoryMiB, // bytes to MiB
		Firmware:         string(vmClass.Spec.Firmware),
		GuestToolsPolicy: string(vmClass.Spec.GuestToolsPolicy),
		ExtraConfig:      vmClass.Spec.ExtraConfig,
	}

	if vmClass.Spec.DiskDefaults != nil {
		sizeGiB, err := vmClassQuantityUnits("diskDefaults.size", vmClass.Spec.DiskDefaults.Size, bytesPerGiB, maxVMClassDiskBytes)
		if err != nil {
			return contracts.CreateRequest{}, err
		}
		class.DiskDefaults = &contracts.DiskDefaults{
			Type:    string(vmClass.Spec.DiskDefaults.Type),
			SizeGiB: sizeGiB, // bytes to GiB
		}
	}

	// Convert PerformanceProfile
	if vmClass.Spec.PerformanceProfile != nil {
		class.PerformanceProfile = &contracts.PerformanceProfile{
			LatencySensitivity:          vmClass.Spec.PerformanceProfile.LatencySensitivity,
			CPUHotAddEnabled:            vmClass.Spec.PerformanceProfile.CPUHotAddEnabled,
			MemoryHotAddEnabled:         vmClass.Spec.PerformanceProfile.MemoryHotAddEnabled,
			VirtualizationBasedSecurity: vmClass.Spec.PerformanceProfile.VirtualizationBasedSecurity,
			NestedVirtualization:        vmClass.Spec.PerformanceProfile.NestedVirtualization,
			HyperThreadingPolicy:        vmClass.Spec.PerformanceProfile.HyperThreadingPolicy,
		}
	}

	// Convert SecurityProfile
	if vmClass.Spec.SecurityProfile != nil {
		class.SecurityProfile = &contracts.SecurityProfile{
			SecureBoot:        vmClass.Spec.SecurityProfile.SecureBoot,
			TPMEnabled:        vmClass.Spec.SecurityProfile.TPMEnabled,
			TPMVersion:        vmClass.Spec.SecurityProfile.TPMVersion,
			VTDEnabled:        vmClass.Spec.SecurityProfile.VTDEnabled,
			EncryptionEnabled: vmClass.Spec.SecurityProfile.EncryptionPolicy != nil && vmClass.Spec.SecurityProfile.EncryptionPolicy.Enabled,
			RequireEncryption: vmClass.Spec.SecurityProfile.EncryptionPolicy != nil && vmClass.Spec.SecurityProfile.EncryptionPolicy.RequireEncryption,
		}
		if vmClass.Spec.SecurityProfile.EncryptionPolicy != nil {
			class.SecurityProfile.KeyProvider = vmClass.Spec.SecurityProfile.EncryptionPolicy.KeyProvider
		}
	}

	// Convert ResourceLimits
	if vmClass.Spec.ResourceLimits != nil {
		class.ResourceLimits = &contracts.ResourceLimits{
			CPULimit:       vmClass.Spec.ResourceLimits.CPULimit,
			CPUReservation: vmClass.Spec.ResourceLimits.CPUReservation,
			CPUShares:      vmClass.Spec.ResourceLimits.CPUShares,
		}
		if vmClass.Spec.ResourceLimits.MemoryLimit != nil {
			memLimitBytes := vmClass.Spec.ResourceLimits.MemoryLimit.Value()
			memLimitMiB := memLimitBytes / (1024 * 1024)
			// Check for int32 overflow (max int32 = 2,147,483,647 MiB ~= 2048 TiB)
			// This is extremely unlikely in practice, but we handle it defensively
			const maxInt32 = int64(^uint32(0) >> 1)
			if memLimitMiB > maxInt32 {
				ctrl.LoggerFrom(context.Background()).Info(
					"Memory limit exceeds int32 max, clamping to maximum",
					"original", memLimitMiB, "clamped", maxInt32,
				)
				memLimitMiB = maxInt32
			}
			memLimitMiB32 := int32(memLimitMiB) // #nosec G115 -- overflow checked above
			class.ResourceLimits.MemoryLimitMiB = &memLimitMiB32
		}
		if vmClass.Spec.ResourceLimits.MemoryReservation != nil {
			memResBytes := vmClass.Spec.ResourceLimits.MemoryReservation.Value()
			memResMiB := memResBytes / (1024 * 1024)
			// Check for int32 overflow
			const maxInt32 = int64(^uint32(0) >> 1)
			if memResMiB > maxInt32 {
				ctrl.LoggerFrom(context.Background()).Info(
					"Memory reservation exceeds int32 max, clamping to maximum",
					"original", memResMiB, "clamped", maxInt32,
				)
				memResMiB = maxInt32
			}
			memResMiB32 := int32(memResMiB) // #nosec G115 -- overflow checked above
			class.ResourceLimits.MemoryReservationMiB = &memResMiB32
		}
	}

	// Convert VMImage - handle both imported disk and template cases
	var image contracts.VMImage

	if usingImportedDisk {
		// VM uses an imported disk (e.g., from migration)
		disk := vm.Spec.ImportedDisk

		// Set format (default to qcow2 if not specified)
		format := disk.Format
		if format == "" {
			format = "qcow2"
		}

		// Determine the path of the imported disk on the target provider.
		//
		// PREFER the explicit Spec.ImportedDisk.Path: for a migration this is
		// the authoritative provider-native path returned by the target
		// provider's ImportDisk (propagated via VMMigration
		// Status.DiskInfo.TargetPath). It is correct for ANY target hypervisor —
		// e.g. "[datastore1] <id>/<id>.vmdk" for vSphere — and must not be
		// overwritten with a synthesized one.
		//
		// Only when no path is set do we synthesize a default. The synthesized
		// form is libvirt-specific (/var/lib/libvirt/images/<id>.<fmt>) and is a
		// LAST RESORT for libvirt targets that imported a disk without reporting
		// a path; it is NOT valid for a vSphere target, which is precisely why
		// the migration controller now always propagates TargetPath for vSphere.
		diskPath := disk.Path
		if diskPath == "" {
			diskPath = fmt.Sprintf("/var/lib/libvirt/images/%s.%s", disk.DiskID, format)
			log.Info("No imported-disk path set; synthesizing libvirt default path (last resort)",
				"diskID", disk.DiskID,
				"format", format,
				"path", diskPath)
		}

		// spec.importedDisk is tenant-writable and VM names repeat across
		// namespaces, so only a disk VERIFIED as this VM's own migration landing
		// disk may be attached in place (ImportedDisk). Anything else is treated
		// as a base image by the provider: confined, then copied or rejected.
		ownImport, err := r.isOwnMigrationDisk(ctx, vm, diskPath)
		if err != nil {
			return contracts.CreateRequest{}, err
		}

		image = contracts.VMImage{
			Path:         diskPath,
			Format:       format,
			ChecksumType: "sha256",
			ImportedDisk: ownImport,
		}

		log.Info("Built image reference from imported disk",
			"diskID", disk.DiskID,
			"path", image.Path,
			"format", image.Format,
			"source", disk.Source)
	} else if vmImage != nil {
		// VM uses a template image
		image = contracts.VMImage{
			Format:       "template", // Default for vSphere
			ChecksumType: "sha256",
		}

		if vmImage.Spec.Source.VSphere != nil {
			image.TemplateName = vmImage.Spec.Source.VSphere.TemplateName
			image.URL = vmImage.Spec.Source.VSphere.OVAURL
			if vmImage.Spec.Source.VSphere.Checksum != "" {
				image.Checksum = vmImage.Spec.Source.VSphere.Checksum
			}
			if vmImage.Spec.Source.VSphere.ChecksumType != "" {
				image.ChecksumType = string(vmImage.Spec.Source.VSphere.ChecksumType)
			}
		}

		if vmImage.Spec.Source.Libvirt != nil {
			log.V(1).Info("Libvirt image source found",
				"path", vmImage.Spec.Source.Libvirt.Path,
				"url", vmImage.Spec.Source.Libvirt.URL,
				"format", vmImage.Spec.Source.Libvirt.Format)
			image.Path = vmImage.Spec.Source.Libvirt.Path
			image.URL = vmImage.Spec.Source.Libvirt.URL
			image.Format = string(vmImage.Spec.Source.Libvirt.Format)
			if vmImage.Spec.Source.Libvirt.Checksum != "" {
				image.Checksum = vmImage.Spec.Source.Libvirt.Checksum
			}
			if vmImage.Spec.Source.Libvirt.ChecksumType != "" {
				image.ChecksumType = string(vmImage.Spec.Source.Libvirt.ChecksumType)
			}
			log.V(1).Info("Set image from Libvirt source",
				"image.Path", image.Path,
				"image.URL", image.URL,
				"image.Format", image.Format)
		} else {
			log.V(1).Info("Libvirt image source is nil")
		}

		if vmImage.Spec.Source.Proxmox != nil {
			log.V(1).Info("Proxmox image source found",
				"templateID", vmImage.Spec.Source.Proxmox.TemplateID,
				"templateName", vmImage.Spec.Source.Proxmox.TemplateName)

			if vmImage.Spec.Source.Proxmox.TemplateID != nil {
				image.TemplateName = fmt.Sprintf("%d", *vmImage.Spec.Source.Proxmox.TemplateID)
				log.V(1).Info("Set TemplateName from TemplateID",
					"templateID", *vmImage.Spec.Source.Proxmox.TemplateID,
					"image.TemplateName", image.TemplateName)
			} else if vmImage.Spec.Source.Proxmox.TemplateName != "" {
				image.TemplateName = vmImage.Spec.Source.Proxmox.TemplateName
				log.V(1).Info("Set TemplateName from TemplateName",
					"image.TemplateName", image.TemplateName)
			}
		} else {
			log.V(1).Info("Proxmox image source is nil")
		}

		// Consume the prepared image (issue #154, PR-6 / #214). When the image has
		// been prepared and is Available on THIS provider, override the source with
		// the prepared location recorded on VMImage.status — so Create clones the
		// prepared template / uses the local prepared pool file instead of
		// re-resolving (and re-downloading) the original source. Falls through to
		// the by-reference source resolved above when the image is not prepared, so
		// there is no regression for unprepared images or non-importing providers.
		if overrode, detail := overrideImageWithPreparedLocation(&image, vmImage, providerCR); overrode {
			log.Info("Consuming prepared image at create (skipping source re-resolution)",
				"vm", vm.Name, "image", vmImage.Name, "provider", client.ObjectKeyFromObject(providerCR).String(), "override", detail)
		} else {
			log.V(1).Info("Image not prepared on provider; using original source",
				"vm", vm.Name, "image", vmImage.Name, "reason", detail)
		}
	}

	// Convert Networks
	// NetworkRef is now optional - if not specified, use template's pre-configured NIC
	// but still pass IP/prefix/gateway/DNS for guestinfo configuration
	var networkAttachments []contracts.NetworkAttachment
	for i, netRef := range vm.Spec.Networks {
		attachment := contracts.NetworkAttachment{
			Name:     netRef.Name,
			StaticIP: netRef.IPAddress,
			Prefix:   netRef.Prefix,
			Gateway:  netRef.Gateway,
			DNS:      netRef.DNS,
		}

		// Only look up VMNetworkAttachment if networkRef is specified
		if netRef.NetworkRef != nil && i < len(networks) && networks[i] != nil {
			net := networks[i]

			if net.Spec.Network.VSphere != nil {
				attachment.NetworkName = net.Spec.Network.VSphere.Portgroup
				if net.Spec.Network.VSphere.VLAN != nil && net.Spec.Network.VSphere.VLAN.VlanID != nil {
					attachment.VLAN = *net.Spec.Network.VSphere.VLAN.VlanID
				}
				// Pass PCI slot number for predictable interface naming (e.g., ens192)
				if net.Spec.Network.VSphere.PCISlotNumber != nil {
					attachment.PCISlotNumber = net.Spec.Network.VSphere.PCISlotNumber
				}
			}

			if net.Spec.Network.Libvirt != nil {
				attachment.NetworkName = net.Spec.Network.Libvirt.NetworkName
				if net.Spec.Network.Libvirt.Bridge != nil {
					attachment.Bridge = net.Spec.Network.Libvirt.Bridge.Name
				}
				attachment.Model = net.Spec.Network.Libvirt.Model
			}

			if net.Spec.Network.Proxmox != nil {
				attachment.Bridge = net.Spec.Network.Proxmox.Bridge
				attachment.Model = net.Spec.Network.Proxmox.Model
				if net.Spec.Network.Proxmox.VLANTag != nil {
					attachment.VLAN = *net.Spec.Network.Proxmox.VLANTag
				}
			}
		} else if netRef.NetworkRef == nil {
			log.V(1).Info("NetworkRef not specified, using template's pre-configured NIC with guestinfo for IP config",
				"network", netRef.Name,
				"ip", netRef.IPAddress)
		}

		networkAttachments = append(networkAttachments, attachment)
	}

	// Convert Disks
	var disks []contracts.DiskSpec
	for _, diskSpec := range vm.Spec.Disks {
		disks = append(disks, contracts.DiskSpec{
			SizeGiB: diskSpec.SizeGiB,
			Type:    diskSpec.Type,
			Name:    diskSpec.Name,
		})
	}

	if len(disks) > 0 {
		log.V(1).Info("Additional disks configured",
			"vm", vm.Name,
			"disk_count", len(disks),
			"disks", disks)
	} else {
		log.V(1).Info("No additional disks configured", "vm", vm.Name)
	}

	// Convert UserData — resolve inline and/or SecretRef, merging if both present
	var userData *contracts.UserData
	if vm.Spec.UserData != nil && vm.Spec.UserData.CloudInit != nil {
		cloudInitData, err := r.resolveCloudInitUserData(ctx, vm.Namespace, vm.Spec.UserData.CloudInit)
		if err != nil {
			return contracts.CreateRequest{}, fmt.Errorf("resolving cloud-init user data: %w", err)
		}
		if cloudInitData != "" {
			userData = &contracts.UserData{
				Type:          "cloud-init",
				CloudInitData: cloudInitData,
			}
		}
	}

	// Convert MetaData — resolve inline and/or SecretRef, merging if both present
	var metaData *contracts.MetaData
	if vm.Spec.MetaData != nil && vm.Spec.MetaData.CloudInit != nil {
		metaDataStr, err := r.resolveCloudInitMetaData(ctx, vm.Namespace, vm.Spec.MetaData.CloudInit)
		if err != nil {
			return contracts.CreateRequest{}, fmt.Errorf("resolving cloud-init metadata: %w", err)
		}
		if metaDataStr != "" {
			metaData = &contracts.MetaData{
				MetaDataYAML: metaDataStr,
			}
		}
	}

	// Convert Placement
	var placement *contracts.Placement
	if vm.Spec.Placement != nil {
		log.Info("Building placement from VM spec",
			"vm", vm.Name,
			"cluster", vm.Spec.Placement.Cluster,
			"datastore", vm.Spec.Placement.Datastore,
			"storagePod", vm.Spec.Placement.StoragePod,
			"folder", vm.Spec.Placement.Folder)
		placement = &contracts.Placement{
			Datastore:  vm.Spec.Placement.Datastore,
			StoragePod: vm.Spec.Placement.StoragePod,
			Cluster:    vm.Spec.Placement.Cluster,
			Folder:     vm.Spec.Placement.Folder,
		}
	} else {
		log.Info("No placement specified in VM spec", "vm", vm.Name)
	}

	return contracts.CreateRequest{
		Name:      vm.Name,
		Class:     class,
		Image:     image,
		Networks:  networkAttachments,
		Disks:     disks,
		UserData:  userData,
		MetaData:  metaData,
		Placement: placement,
		Tags:      vm.Spec.Tags,
		// Owner lets a provider that keys VMs by a tenant-shared name (libvirt, vSphere)
		// stamp the VM it creates and bind to an existing same-named VM ONLY when
		// that VM records this VirtualMachine's UID — never another tenant's.
		Owner: contracts.ObjectIdentity{
			UID:       string(vm.UID),
			Namespace: vm.Namespace,
			Name:      vm.Name,
		},
	}, nil
}

// overrideImageWithPreparedLocation rewrites the create-time image source to the
// prepared location recorded on vmImage.status for providerCR, closing the
// image-prepare loop (issue #154, PR-6 / #214). The provider prepared the image
// (downloaded/converted/imported it into a template or pool) and reported WHERE
// it landed; this makes Create consume that prepared location instead of
// re-resolving — and possibly re-downloading — the original source.
//
// Only providerCR's own entry is consulted — the one keyed by its identity
// "<namespace>/<name>" (imageProviderKey) — and only when it was recorded
// through providerCR's current object (imageEntryRecordedThrough). An entry of
// a same-named Provider in another namespace, a bare-name entry from an earlier
// release, or an entry recorded through a since re-created Provider is never
// used here: a VM must not be created from an artifact its Provider did not
// prepare or confirm. Nor is an entry prepared for another spec.source — or
// for none, recorded by an earlier release (ADR-0009 D8): its sourceDigest
// must equal the digest of the current spec.source, for every source kind.
//
// It returns (true, detail) when an override was applied, or (false, reason) when
// the original source is kept (no Provider, image not prepared / not Available on
// this provider, not recorded through it or not for the current spec.source, or
// no usable prepared location recorded). The fallback path is the unchanged
// by-reference behavior, so unprepared images and non-importing providers see
// no regression.
//
// The override is dispatched by the VMImage source kind:
//   - libvirt: set image.Path to the prepared pool file and clear image.URL, so
//     libvirt Create resolves /path → CreateVolumeFromImageFile instead of
//     re-downloading the URL.
//   - vSphere: set image.TemplateName to the prepared template name and clear
//     image.URL (the OVA URL), so Create clones the prepared template.
//   - Proxmox: set image.TemplateName to the prepared template name/VMID.
func overrideImageWithPreparedLocation(
	image *contracts.VMImage,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	providerCR *infravirtrigaudiov1beta1.Provider,
) (overrode bool, detail string) {
	if providerCR == nil {
		return false, "no provider"
	}
	ps, found := vmImage.Status.ProviderStatus[imageProviderKey(providerCR)]
	if !found || !ps.Available {
		return false, "not prepared/available on provider"
	}
	if !imageEntryRecordedThrough(ps, providerCR) {
		return false, "prepare state not recorded through this Provider object"
	}
	// ADR-0009 D8: the entry must have been prepared for the CURRENT
	// spec.source, whatever its kind. Otherwise a VMImage switched from, say,
	// an ovaURL to a templateName would keep cloning the artifact prepared for
	// the OVA (a reference-style source never prepares, so nothing would
	// replace the old entry).
	digest, err := imageSourceDigest(vmImage)
	if err != nil {
		return false, "cannot compute the source digest: " + err.Error()
	}
	if !imageEntryForSource(ps, digest) {
		if ps.SourceDigest == "" {
			return false, "prepare state records no source digest (recorded by an earlier release)"
		}
		return false, "prepare state is for a different spec.source (source digest mismatch)"
	}

	switch {
	case vmImage.Spec.Source.Libvirt != nil:
		if ps.Path == "" {
			return false, "prepared but no pool path recorded"
		}
		image.Path = ps.Path
		image.URL = "" // prefer the local prepared template over re-downloading
		return true, fmt.Sprintf("libvirt path=%s", ps.Path)
	case vmImage.Spec.Source.VSphere != nil:
		if ps.ID == "" {
			return false, "prepared but no template id recorded"
		}
		image.TemplateName = ps.ID
		image.URL = "" // clear the OVA URL so Create clones the prepared template
		return true, fmt.Sprintf("vsphere templateName=%s", ps.ID)
	case vmImage.Spec.Source.Proxmox != nil:
		if ps.ID == "" {
			return false, "prepared but no template id recorded"
		}
		image.TemplateName = ps.ID
		return true, fmt.Sprintf("proxmox templateName=%s", ps.ID)
	default:
		return false, "no recognized image source kind"
	}
}

// updateStatus updates the VM status
func (r *VirtualMachineReconciler) updateStatus(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) {
	if err := r.Status().Update(ctx, vm); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update VirtualMachine status")
	}
}

// needsReconfigure checks if the VM needs to be reconfigured based on VMClass changes
func (r *VirtualMachineReconciler) needsReconfigure(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) bool {
	// Get desired resources from VMClass (with possible overrides from VM spec)
	desiredCPU := vmClass.Spec.CPU
	desiredMemoryMiB := vmClass.Spec.Memory.Value() / (1024 * 1024)

	// Check for VM-level resource overrides
	if vm.Spec.Resources != nil {
		if vm.Spec.Resources.CPU != nil {
			desiredCPU = *vm.Spec.Resources.CPU
		}
		if vm.Spec.Resources.MemoryMiB != nil {
			desiredMemoryMiB = *vm.Spec.Resources.MemoryMiB
		}
	}

	// Get current resources from status
	currentCPU := r.getCurrentCPU(vm)
	currentMemoryMiB := r.getCurrentMemoryMiB(vm)

	// If no current resources tracked, assume first reconcile after creation
	// and update status without triggering reconfigure
	if currentCPU == 0 && currentMemoryMiB == 0 {
		return false
	}

	// Check if CPU or memory changed
	return currentCPU != desiredCPU || currentMemoryMiB != desiredMemoryMiB
}

// getCurrentCPU returns the current CPU count from VM status
func (r *VirtualMachineReconciler) getCurrentCPU(vm *infravirtrigaudiov1beta1.VirtualMachine) int32 {
	if vm.Status.CurrentResources != nil && vm.Status.CurrentResources.CPU != nil {
		return *vm.Status.CurrentResources.CPU
	}
	return 0
}

// getCurrentMemoryMiB returns the current memory in MiB from VM status
func (r *VirtualMachineReconciler) getCurrentMemoryMiB(vm *infravirtrigaudiov1beta1.VirtualMachine) int64 {
	if vm.Status.CurrentResources != nil && vm.Status.CurrentResources.MemoryMiB != nil {
		return *vm.Status.CurrentResources.MemoryMiB
	}
	return 0
}

// reconfigureVM reconfigures the VM with new VMClass resources
func (r *VirtualMachineReconciler) reconfigureVM(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider contracts.Provider,
	ref contracts.VMRef,
	providerCR *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Build the desired configuration
	req, err := r.buildCreateRequest(ctx, vm, providerCR, vmClass, vmImage, networks)
	if err != nil {
		logger.Error(err, "Failed to build create request")
		return ctrl.Result{}, err
	}

	// Call provider reconfigure
	taskRef, err := provider.Reconfigure(ctx, ref, req)
	if err != nil {
		// As for Power: a clustered VM's host-scoped unavailability or
		// not-found is a host-level fact, not a reconfigure failure to retry
		// every few seconds (ADR-0007 Addendum A, slice 2).
		if res, handled := r.handleRoutedOpError(ctx, vm, ref, err, errReasonProviderReconfigure); handled {
			return res, nil
		}
		logger.Error(err, "Failed to reconfigure VM")
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to reconfigure VM: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: routedCallRetryAfter(ref, err)}, nil
	}

	// Update status with reconfiguration info
	vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseReconfiguring
	now := metav1.Now()
	vm.Status.LastReconfigureTime = &now

	if taskRef != "" {
		vm.Status.ReconfigureTaskRef = taskRef
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, "VM reconfiguration in progress")
	} else {
		// Reconfigure completed synchronously, update current resources
		r.updateCurrentResources(vm, vmClass)
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM reconfigured successfully")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonReconcileSuccess, "VM is ready")
	}

	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// updateCurrentResources updates the VM status with current resource allocation
func (r *VirtualMachineReconciler) updateCurrentResources(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) {
	cpu := vmClass.Spec.CPU
	memoryMiB := vmClass.Spec.Memory.Value() / (1024 * 1024)

	// Check for VM-level resource overrides
	if vm.Spec.Resources != nil {
		if vm.Spec.Resources.CPU != nil {
			cpu = *vm.Spec.Resources.CPU
		}
		if vm.Spec.Resources.MemoryMiB != nil {
			memoryMiB = *vm.Spec.Resources.MemoryMiB
		}
	}

	if vm.Status.CurrentResources == nil {
		vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{}
	}
	vm.Status.CurrentResources.CPU = &cpu
	vm.Status.CurrentResources.MemoryMiB = &memoryMiB
}

func (r *VirtualMachineReconciler) getRequeueInterval(vm *infravirtrigaudiov1beta1.VirtualMachine, desc contracts.DescribeResponse) time.Duration {
	// Polling intervals for various states
	const (
		fastPoll     = 10 * time.Second // For transitional states
		waitingForIP = 10 * time.Second // Waiting for IP address (VMware Tools)
		normalPoll   = 2 * time.Minute  // For stable running VMs
		slowPoll     = 5 * time.Minute  // For stable powered-off VMs
	)

	// Check if VM has no IP addresses yet (waiting for DHCP/network or VMware Tools)
	if desc.PowerState == "poweredOn" && len(desc.IPs) == 0 {
		return waitingForIP // Poll less frequently while waiting for IP
	}

	// Check VM power state for different polling frequencies
	switch desc.PowerState {
	case "poweredOn":
		// VM is running and has IP - normal monitoring frequency
		return normalPoll
	case "poweredOff":
		// VM is off - slower polling
		return slowPoll
	case "suspended":
		// VM is suspended - normal polling
		return normalPoll
	default:
		// Unknown or transitional state - fast polling
		return fastPoll
	}
}

// getProviderInstance resolves a provider to a remote implementation
func (r *VirtualMachineReconciler) getProviderInstance(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	// All providers are now remote
	if r.RemoteResolver == nil {
		return nil, fmt.Errorf("no remote resolver available")
	}

	return r.RemoteResolver.GetProvider(ctx, provider) //nolint:wrapcheck
}

// recordIPDiscoveryIfFirstSeen emits a single
// virtrigaud_ip_discovery_duration_seconds sample on the no-IPs →
// has-IPs transition for this VM. Pure function (no reconciler state)
// so it is unit-testable without standing up the full envtest harness.
//
// Inputs:
//   - currentIPs: the value of vm.Status.IPs BEFORE the reconciler
//     overwrites it with descIPs. The pre-update value is what tells us
//     whether the VM has previously been observed with IPs.
//   - descIPs:    the IPs the provider just reported via Describe.
//   - creationTime: vm.CreationTimestamp. Used as the baseline for the
//     duration measurement, so the metric reads as "kubectl apply →
//     first IP visible". Zero-valued (defensive) → skip.
//   - providerType: the value of provider.Spec.Type, used as the
//     provider_type metric label.
//
// Gate semantics (all three must hold; otherwise skip silently):
//  1. currentIPs is empty (we have not previously observed any IP)
//  2. descIPs is non-empty (the provider just gave us an IP)
//  3. creationTime is non-zero (CreationTimestamp is present)
//
// Idempotency across manager restarts is achieved at the persistence
// layer: vm.Status.IPs is committed to etcd as soon as the gate fires
// once (the very next line in Reconcile does
// `vm.Status.IPs = desc.IPs`), so subsequent reconciles after the
// transition see currentIPs non-empty and the gate short-circuits.
//
// G7.2 / #127.
func recordIPDiscoveryIfFirstSeen(currentIPs, descIPs []string, creationTime metav1.Time, providerType string) {
	if len(currentIPs) != 0 {
		return // already observed IPs in a previous reconcile
	}
	if len(descIPs) == 0 {
		return // VM still doesn't have an IP this round
	}
	if creationTime.IsZero() {
		return // defensive — CRs fetched via the API server always have this
	}
	metrics.RecordIPDiscovery(providerType, time.Since(creationTime.Time))
}

// vmIsAdopted reports whether the VirtualMachine is marked adopted, i.e. its
// underlying hypervisor VM is created by the adoption/clone controller (which
// sets Status.ID out-of-band) rather than by the VirtualMachine controller.
// The VirtualMachine controller consults this before its create decision to
// avoid double-creating a VM whose Status.ID has not yet been written (issue
// #179).
func vmIsAdopted(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vm.Labels[AdoptedLabel] == AdoptedLabelValue
}

// vmsForGrantChange returns the VirtualMachines a consumer-grant change may
// affect (consumerGrantIndex): for a Namespace label change, the VMs there that
// reference another namespace's objects or are refused; for a selector change,
// the VMs that reference that object from another namespace — so a grant, and
// a revocation, takes effect promptly.
func (r *VirtualMachineReconciler) vmsForGrantChange(ctx context.Context, indexValue string) []reconcile.Request {
	return requestsForGrantChange(ctx, r.Client, &infravirtrigaudiov1beta1.VirtualMachineList{}, indexValue, nil)
}

// SetupWithManager sets up the controller with the Manager. Besides its own
// VirtualMachines it watches Namespace label changes and the
// spec.consumerNamespaceSelector of Providers, VMClasses and VMImages, so a
// cross-namespace grant (or revocation) takes effect without waiting for the
// slow recheck.
func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := indexConsumerGrants(mgr, &infravirtrigaudiov1beta1.VirtualMachine{}, vmConsumerGrantIndexValues); err != nil {
		return err
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.VirtualMachine{})
	return withConsumerGrantWatches(b, r.vmsForGrantChange).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Only reconcile if spec changed (ignore status-only updates)
				// This prevents tight reconcile loops from status updates
				oldVM, ok1 := e.ObjectOld.(*infravirtrigaudiov1beta1.VirtualMachine)
				newVM, ok2 := e.ObjectNew.(*infravirtrigaudiov1beta1.VirtualMachine)
				if ok1 && ok2 {
					// Reconcile if generation changed (spec changed) or if being deleted
					return oldVM.Generation != newVM.Generation || !newVM.DeletionTimestamp.IsZero()
				}
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// Handle deletion in Reconcile through finalizers
				return false
			},
		}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 10, // Process up to 10 VMs in parallel
		}).
		Named("virtualmachine").
		Complete(r)
}
