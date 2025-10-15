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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"

	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
)

// VirtualMachineReconciler reconciles a VirtualMachine object
type VirtualMachineReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RemoteResolver *remote.Resolver
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmimages,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmnetworkattachments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles VirtualMachine reconciliation
func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	provider, vmClass, vmImage, networks, err := r.getDependencies(ctx, vm)
	if err != nil {
		logger.Error(err, "Failed to get dependencies")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonWaitingForDependencies, err.Error())
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Get provider instance (remote or in-process)
	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		logger.Error(err, "Failed to get provider instance")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, err.Error())
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Validate provider
	if err := providerInstance.Validate(ctx); err != nil {
		logger.Error(err, "Provider validation failed")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Provider validation failed: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check if we have an active task
	if vm.Status.LastTaskRef != "" {
		done, err := providerInstance.IsTaskComplete(ctx, vm.Status.LastTaskRef)
		if err != nil {
			logger.Error(err, "Failed to check task status")
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to check task: %v", err))
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		if !done {
			logger.Info("Task still in progress", "taskRef", vm.Status.LastTaskRef)
			k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonTaskInProgress, "Task in progress")
			r.updateStatus(ctx, vm)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Task completed, clear it
		vm.Status.LastTaskRef = ""
	}

	// Ensure VM exists
	if vm.Status.ID == "" {
		logger.Info("Creating VM")
		return r.createVM(ctx, vm, providerInstance, vmClass, vmImage, networks)
	}

	// VM exists, check current state
	desc, err := providerInstance.Describe(ctx, vm.Status.ID)
	if err != nil {
		logger.Error(err, "Failed to describe VM")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to describe VM: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if !desc.Exists {
		logger.Info("VM no longer exists, recreating")
		vm.Status.ID = ""
		return r.createVM(ctx, vm, providerInstance, vmClass, vmImage, networks)
	}

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
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			// Provider not found, continue with cleanup
		} else {
			// Delete VM from provider
			providerInstance, err := r.getProviderInstance(ctx, provider)
			if err != nil {
				logger.Error(err, "Failed to get provider instance for deletion")
			} else {
				logger.Info("Deleting VM from provider", "id", vm.Status.ID)
				taskRef, err := providerInstance.Delete(ctx, vm.Status.ID)
				if err != nil {
					logger.Error(err, "Failed to delete VM from provider")
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

	// Get VMImage
	vmImage := &infravirtrigaudiov1beta1.VMImage{}
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

	// Get VMNetworkAttachments
	var networks []*infravirtrigaudiov1beta1.VMNetworkAttachment
	for _, netRef := range vm.Spec.Networks {
		network := &infravirtrigaudiov1beta1.VMNetworkAttachment{}
		netKey := types.NamespacedName{
			Name:      netRef.Name,
			Namespace: vm.Namespace,
		}
		if err := r.Get(ctx, netKey, network); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to get vmnetworkattachment %s: %w", netRef.Name, err)
		}
		networks = append(networks, network)
	}

	return provider, vmClass, vmImage, networks, nil
}

// createVM creates a new VM using the provider
func (r *VirtualMachineReconciler) createVM(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider contracts.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Build create request
	req := r.buildCreateRequest(vm, vmClass, vmImage, networks)

	// Create VM
	resp, err := provider.Create(ctx, req)
	if err != nil {
		logger.Error(err, "Failed to create VM")
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to create VM: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Update status
	vm.Status.ID = resp.ID
	if resp.TaskRef != "" {
		vm.Status.LastTaskRef = resp.TaskRef
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonCreating, "VM creation initiated")
	} else {
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonReconcileSuccess, "VM created")
	}

	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	taskRef, err := provider.Power(ctx, vm.Status.ID, powerOp)
	if err != nil {
		logger.Error(err, "Failed to adjust power state")
		k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError, fmt.Sprintf("Failed to adjust power state: %v", err))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if taskRef != "" {
		vm.Status.LastTaskRef = taskRef
		k8s.SetReconfiguringCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonUpdating, "Adjusting power state")
	}

	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// buildCreateRequest builds a provider create request from VM spec
func (r *VirtualMachineReconciler) buildCreateRequest(
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) contracts.CreateRequest {
	log := ctrl.Log.WithName("buildCreateRequest")

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

	// Convert VMImage
	image := contracts.VMImage{
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

	if vmImage.Spec.Source.Proxmox != nil {
		log.Info("DEBUG controller: Proxmox image source found",
			"templateID", vmImage.Spec.Source.Proxmox.TemplateID,
			"templateName", vmImage.Spec.Source.Proxmox.TemplateName)

		if vmImage.Spec.Source.Proxmox.TemplateID != nil {
			image.TemplateName = fmt.Sprintf("%d", *vmImage.Spec.Source.Proxmox.TemplateID)
			log.Info("DEBUG controller: Set TemplateName from TemplateID",
				"templateID", *vmImage.Spec.Source.Proxmox.TemplateID,
				"image.TemplateName", image.TemplateName)
		} else if vmImage.Spec.Source.Proxmox.TemplateName != "" {
			image.TemplateName = vmImage.Spec.Source.Proxmox.TemplateName
			log.Info("DEBUG controller: Set TemplateName from TemplateName",
				"image.TemplateName", image.TemplateName)
		}
	} else {
		log.Info("DEBUG controller: Proxmox image source is nil")
	}

	// Convert Networks
	var networkAttachments []contracts.NetworkAttachment
	for i, netRef := range vm.Spec.Networks {
		if i < len(networks) {
			net := networks[i]
			attachment := contracts.NetworkAttachment{
				Name:     netRef.Name,
				StaticIP: netRef.IPAddress,
			}

			if net.Spec.Network.VSphere != nil {
				attachment.NetworkName = net.Spec.Network.VSphere.Portgroup
				if net.Spec.Network.VSphere.VLAN != nil && net.Spec.Network.VSphere.VLAN.VlanID != nil {
					attachment.VLAN = *net.Spec.Network.VSphere.VLAN.VlanID
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

			// MacAddress is not part of NetworkConfig in v1beta1, skip for now

			networkAttachments = append(networkAttachments, attachment)
		}
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

	// Convert UserData
	var userData *contracts.UserData
	if vm.Spec.UserData != nil && vm.Spec.UserData.CloudInit != nil {
		userData = &contracts.UserData{
			Type: "cloud-init",
		}
		if vm.Spec.UserData.CloudInit.Inline != "" {
			userData.CloudInitData = vm.Spec.UserData.CloudInit.Inline
		}
		// TODO: Handle SecretRef
	}

	// Convert Placement
	var placement *contracts.Placement
	if vm.Spec.Placement != nil {
		placement = &contracts.Placement{
			Datastore: vm.Spec.Placement.Datastore,
			Cluster:   vm.Spec.Placement.Cluster,
			Folder:    vm.Spec.Placement.Folder,
		}
	}

	return contracts.CreateRequest{
		Name:      vm.Name,
		Class:     class,
		Image:     image,
		Networks:  networkAttachments,
		Disks:     disks,
		UserData:  userData,
		Placement: placement,
		Tags:      vm.Spec.Tags,
	}
}

// updateStatus updates the VM status
func (r *VirtualMachineReconciler) updateStatus(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) {
	if err := r.Status().Update(ctx, vm); err != nil {
		log.FromContext(ctx).Error(err, "Failed to update VirtualMachine status")
	}
}

// getRequeueInterval returns an intelligent requeue interval based on VM state
func (r *VirtualMachineReconciler) getRequeueInterval(vm *infravirtrigaudiov1beta1.VirtualMachine, desc contracts.DescribeResponse) time.Duration {
	// Fast polling intervals for various states
	const (
		fastPoll   = 30 * time.Second // For transitional states
		normalPoll = 2 * time.Minute  // For stable running VMs
		slowPoll   = 5 * time.Minute  // For stable powered-off VMs
		errorPoll  = 30 * time.Second // For error conditions
	)

	// Check if VM has no IP addresses yet (waiting for DHCP/network)
	if desc.PowerState == "poweredOn" && len(desc.IPs) == 0 {
		return fastPoll // Poll frequently until VM gets IP
	}

	// Check VM power state for different polling frequencies
	switch desc.PowerState {
	case "poweredOn":
		// VM is running - normal monitoring frequency
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

	return r.RemoteResolver.GetProvider(ctx, provider)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.VirtualMachine{}).
		WithEventFilter(predicate.Funcs{
			DeleteFunc: func(e event.DeleteEvent) bool {
				// Handle deletion in Reconcile through finalizers
				return false
			},
		}).
		Named("virtualmachine").
		Complete(r)
}
