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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
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
	errReasonProviderValidate = "provider-validate"
	errReasonProviderDescribe = "provider-describe"
	errReasonProviderTask     = "provider-task-status"
	errReasonProviderDelete   = "provider-delete"
	errReasonImagePrepare     = "image-prepare"
)

// ProviderResolver resolves Provider resources to provider implementations.
// Implemented by *remote.Resolver in production; can be mocked in tests.
type ProviderResolver interface {
	GetProvider(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error)
}

type VirtualMachineReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RemoteResolver ProviderResolver
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmimages,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmimages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmnetworkattachments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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

	// Update observed generation
	vm.Status.ObservedGeneration = vm.Generation

	// Get dependencies
	imageRefName := ""
	if vm.Spec.ImageRef != nil {
		imageRefName = vm.Spec.ImageRef.Name
	} else if vm.Spec.ImportedDisk != nil {
		imageRefName = fmt.Sprintf("imported:%s", vm.Spec.ImportedDisk.DiskID)
	}
	logger.V(1).Info("Resolving VM dependencies", "provider", vm.Spec.ProviderRef.Name, "class", vm.Spec.ClassRef.Name, "image", imageRefName)
	provider, vmClass, vmImage, networks, err := r.getDependencies(ctx, vm)
	if err != nil {
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

	// Validate provider
	logger.V(1).Info("Validating provider connectivity")
	if err := providerInstance.Validate(ctx); err != nil {
		logger.Error(err, "Provider validation failed - will retry in 5s", "provider", provider.Name)
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Provider validation failed: %v", err))
		metrics.RecordError(errReasonProviderValidate, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	logger.V(1).Info("Provider validation successful", "provider", provider.Name)

	// Ensure the referenced image is prepared on this provider before creating
	// the VM (lazy, VM-create-driven prepare — issue #154). This is a no-op for
	// providers that do not advertise/implement image import, for ImportedDisk
	// VMs, and for images already prepared on this provider; in those cases it
	// returns (false, nil) and we fall through to the unchanged create path.
	if requeue, err := r.EnsureImageOnProvider(ctx, vm, vmImage, provider, providerInstance); err != nil {
		if stderrors.Is(err, errImagePrepareHold) {
			// OnMissing forbids preparing (Fail/Wait); the condition is recorded
			// on the VMImage. Reflect a waiting condition on the VM and requeue
			// without treating it as a reconcile error.
			logger.Info("Holding VM create: referenced image is not prepared and may not be imported",
				"image", vmImage.Name, "provider", provider.Name)
			k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonWaitingForDependencies,
				fmt.Sprintf("Image %s not prepared on provider %s", vmImage.Name, provider.Name))
			r.updateStatus(ctx, vm)
			return imageEnsureResultToReconcile(), nil
		}
		logger.Error(err, "Failed to ensure image on provider - will retry in 5s",
			"image", imageRefName, "provider", provider.Name)
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError,
			fmt.Sprintf("Image prepare failed: %v", err))
		metrics.RecordError(errReasonImagePrepare, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	} else if requeue {
		// A prepare is in flight; surface a provisioning condition and requeue to
		// poll it. We do NOT create the VM until the image is Ready on the provider.
		logger.Info("Waiting for image prepare to complete before creating VM",
			"image", vmImage.Name, "provider", provider.Name)
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonTaskInProgress,
			fmt.Sprintf("Preparing image %s on provider %s", vmImage.Name, provider.Name))
		r.updateStatus(ctx, vm)
		return imageEnsureResultToReconcile(), nil
	}

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

		// Reconfigure task completed, update current resources and clear task ref
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
		logger.Info("Creating VM")
		return r.createVM(ctx, vm, providerInstance, provider.Name, vmClass, vmImage, networks)
	}

	// VM exists, check current state
	desc, err := providerInstance.Describe(ctx, vm.Status.ID)
	if err != nil {
		logger.Error(err, "Failed to describe VM")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to describe VM: %v", err))
		metrics.RecordError(errReasonProviderDescribe, metrics.ComponentManager)
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if !desc.Exists {
		logger.Info("VM no longer exists, recreating")
		vm.Status.ID = ""
		return r.createVM(ctx, vm, providerInstance, provider.Name, vmClass, vmImage, networks)
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
		return r.adjustPowerState(ctx, vm, providerInstance, string(desiredPowerState))
	}

	// Check if VMClass resources have changed and need reconfiguration
	if r.needsReconfigure(vm, vmClass) {
		logger.Info("VMClass resources changed, reconfiguring VM",
			"currentCPU", r.getCurrentCPU(vm),
			"desiredCPU", vmClass.Spec.CPU,
			"currentMemoryMiB", r.getCurrentMemoryMiB(vm),
			"desiredMemoryMiB", vmClass.Spec.Memory.Value()/(1024*1024))
		return r.reconfigureVM(ctx, vm, providerInstance, provider.Name, vmClass, vmImage, networks)
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

	// Get provider if we have a provider ref and VM ID
	if vm.Status.ID != "" && vm.Spec.ProviderRef.Name != "" {
		provider := &infravirtrigaudiov1beta1.Provider{}
		providerKey := types.NamespacedName{
			Name:      vm.Spec.ProviderRef.Name,
			Namespace: vm.Namespace,
		}
		if vm.Spec.ProviderRef.Namespace != "" {
			providerKey.Namespace = vm.Spec.ProviderRef.Namespace
		}

		if err := r.Get(ctx, providerKey, provider); err != nil {
			if !errors.IsNotFound(err) {
				logger.Error(err, "Failed to get provider for deletion")
				metrics.RecordError(errReasonDepsError, metrics.ComponentManager)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// Provider not found, continue with cleanup
		} else {
			// Delete VM from provider
			providerInstance, err := r.getProviderInstance(ctx, provider)
			if err != nil {
				logger.Error(err, "Failed to get provider instance for deletion")
				metrics.RecordError(errReasonProviderResolve, metrics.ComponentManager)
			} else {
				logger.Info("Deleting VM from provider", "id", vm.Status.ID)
				taskRef, err := providerInstance.Delete(ctx, vm.Status.ID)
				if err != nil {
					logger.Error(err, "Failed to delete VM from provider")
					metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)
					// Continue with cleanup even if deletion fails
				} else if taskRef != "" {
					logger.Info("VM deletion initiated", "taskRef", taskRef)
					// TODO: Wait for task completion in future iterations
				}
			}
		}
	}

	// Remove finalizer
	if err := k8s.RemoveFinalizer(ctx, r.Client, vm, infravirtrigaudiov1beta1.VirtualMachineFinalizer); err != nil {
		logger.Error(err, "Failed to remove finalizer")
		metrics.RecordError(errReasonRemoveFinalizer, metrics.ComponentManager)
		return ctrl.Result{}, err
	}

	logger.Info("VirtualMachine deleted successfully")
	return ctrl.Result{}, nil
}

// getDependencies fetches all required dependencies for the VM
func (r *VirtualMachineReconciler) getDependencies(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (
	*infravirtrigaudiov1beta1.Provider,
	*infravirtrigaudiov1beta1.VMClass,
	*infravirtrigaudiov1beta1.VMImage,
	[]*infravirtrigaudiov1beta1.VMNetworkAttachment,
	error,
) {
	// Get Provider
	provider := &infravirtrigaudiov1beta1.Provider{}
	providerKey := types.NamespacedName{
		Name:      vm.Spec.ProviderRef.Name,
		Namespace: vm.Namespace,
	}
	if vm.Spec.ProviderRef.Namespace != "" {
		providerKey.Namespace = vm.Spec.ProviderRef.Namespace
	}
	if err := r.Get(ctx, providerKey, provider); err != nil {
		if errors.IsNotFound(err) {
			// Provider doesn't exist yet - preserve the NotFound error for proper handling upstream
			return nil, nil, nil, nil, fmt.Errorf("provider %s not found (namespace: %s): %w", vm.Spec.ProviderRef.Name, providerKey.Namespace, err)
		}
		return nil, nil, nil, nil, fmt.Errorf("failed to get provider %s: %w", vm.Spec.ProviderRef.Name, err)
	}

	// Get VMClass
	vmClass := &infravirtrigaudiov1beta1.VMClass{}
	classKey := types.NamespacedName{
		Name:      vm.Spec.ClassRef.Name,
		Namespace: vm.Namespace,
	}
	if vm.Spec.ClassRef.Namespace != "" {
		classKey.Namespace = vm.Spec.ClassRef.Namespace
	}
	if err := r.Get(ctx, classKey, vmClass); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get vmclass %s: %w", vm.Spec.ClassRef.Name, err)
	}

	// Get VMImage (only if ImageRef is specified, not ImportedDisk)
	var vmImage *infravirtrigaudiov1beta1.VMImage
	if vm.Spec.ImageRef != nil {
		vmImage = &infravirtrigaudiov1beta1.VMImage{}
		imageKey := types.NamespacedName{
			Name:      vm.Spec.ImageRef.Name,
			Namespace: vm.Namespace,
		}
		if vm.Spec.ImageRef.Namespace != "" {
			imageKey.Namespace = vm.Spec.ImageRef.Namespace
		}
		if err := r.Get(ctx, imageKey, vmImage); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to get vmimage %s: %w", vm.Spec.ImageRef.Name, err)
		}
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
				return nil, nil, nil, nil, fmt.Errorf("failed to get vmnetworkattachment %s: %w", netRef.NetworkRef.Name, err)
			}
			networks = append(networks, network)
		} else {
			// No networkRef - append nil to maintain index alignment with vm.Spec.Networks
			networks = append(networks, nil)
		}
	}

	return provider, vmClass, vmImage, networks, nil
}

// createVM creates a new VM using the provider
func (r *VirtualMachineReconciler) createVM(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider contracts.Provider,
	providerName string,
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
	req, err := r.buildCreateRequest(ctx, vm, providerName, vmClass, vmImage, networks)
	if err != nil {
		logger.Error(err, "Failed to build create request")
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to build create request: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Create VM
	resp, err := provider.Create(ctx, req)
	if err != nil {
		logger.Error(err, "Failed to create VM")
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to create VM: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Update status
	vm.Status.ID = resp.ID
	// Initialize current resources to track for future resize detection
	r.updateCurrentResources(vm, vmClass)

	if resp.TaskRef != "" {
		vm.Status.LastTaskRef = resp.TaskRef
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonCreating, "VM creation initiated")
	} else {
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM created")
	}

	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// adjustPowerState adjusts the VM power state
func (r *VirtualMachineReconciler) adjustPowerState(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider contracts.Provider,
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

	taskRef, err := provider.Power(ctx, vm.Status.ID, powerOp)
	if err != nil {
		logger.Error(err, "Failed to adjust power state")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to adjust power state: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if taskRef != "" {
		vm.Status.LastTaskRef = taskRef
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, "Adjusting power state")
	}

	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// buildCreateRequest builds a provider create request from VM spec.
// It resolves cloud-init user data and metadata from both inline content and Secret references.
func (r *VirtualMachineReconciler) buildCreateRequest(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerName string,
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

	// Convert VMClass
	class := contracts.VMClass{
		CPU:              vmClass.Spec.CPU,
		MemoryMiB:        int32(vmClass.Spec.Memory.Value() / (1024 * 1024)), // Convert bytes to MiB
		Firmware:         string(vmClass.Spec.Firmware),
		GuestToolsPolicy: string(vmClass.Spec.GuestToolsPolicy),
		ExtraConfig:      vmClass.Spec.ExtraConfig,
	}

	if vmClass.Spec.DiskDefaults != nil {
		class.DiskDefaults = &contracts.DiskDefaults{
			Type:    string(vmClass.Spec.DiskDefaults.Type),
			SizeGiB: int32(vmClass.Spec.DiskDefaults.Size.Value() / (1024 * 1024 * 1024)), // Convert bytes to GiB
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

		image = contracts.VMImage{
			Path:         diskPath,
			Format:       format,
			ChecksumType: "sha256",
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
		if overrode, detail := overrideImageWithPreparedLocation(&image, vmImage, providerName); overrode {
			log.Info("Consuming prepared image at create (skipping source re-resolution)",
				"vm", vm.Name, "image", vmImage.Name, "provider", providerName, "override", detail)
		} else {
			log.V(1).Info("Image not prepared on provider; using original source",
				"vm", vm.Name, "image", vmImage.Name, "provider", providerName, "reason", detail)
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
	}, nil
}

// overrideImageWithPreparedLocation rewrites the create-time image source to the
// prepared location recorded on vmImage.status for providerName, closing the
// image-prepare loop (issue #154, PR-6 / #214). The provider prepared the image
// (downloaded/converted/imported it into a template or pool) and reported WHERE
// it landed; this makes Create consume that prepared location instead of
// re-resolving — and possibly re-downloading — the original source.
//
// It returns (true, detail) when an override was applied, or (false, reason) when
// the original source is kept (image not prepared / not Available on this
// provider, or no usable prepared location recorded). The fallback path is the
// unchanged by-reference behavior, so unprepared images and non-importing
// providers see no regression.
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
	providerName string,
) (overrode bool, detail string) {
	ps, found := vmImage.Status.ProviderStatus[providerName]
	if !found || !ps.Available {
		return false, "not prepared/available on provider"
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
	providerName string,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Build the desired configuration
	req, err := r.buildCreateRequest(ctx, vm, providerName, vmClass, vmImage, networks)
	if err != nil {
		logger.Error(err, "Failed to build create request")
		return ctrl.Result{}, err
	}

	// Call provider reconfigure
	taskRef, err := provider.Reconfigure(ctx, vm.Status.ID, req)
	if err != nil {
		logger.Error(err, "Failed to reconfigure VM")
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to reconfigure VM: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.VirtualMachine{}).
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
