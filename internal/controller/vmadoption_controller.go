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
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/runtime/remote"
)

const (
	// AdoptionAnnotation is the annotation that triggers VM adoption
	AdoptionAnnotation = "virtrigaud.io/adopt-vms"
	// AdoptionFilterAnnotation is an optional annotation to filter VMs to adopt
	// Format: JSON object with optional fields: namePattern, powerState, minCPU, minMemoryMiB, maxCPU, maxMemoryMiB
	// Example: {"namePattern": "prod-.*", "powerState": "on", "minCPU": 2}
	AdoptionFilterAnnotation = "virtrigaud.io/adopt-filter"
)

// VMAdoptionFilter defines the filter criteria for VM adoption
type VMAdoptionFilter struct {
	// NamePattern is a regex pattern to match VM names
	NamePattern string `json:"namePattern,omitempty"`
	// PowerState filters by power state (e.g., "on", "off", "suspended")
	PowerState string `json:"powerState,omitempty"`
	// MinCPU is the minimum number of CPUs required
	MinCPU int32 `json:"minCPU,omitempty"`
	// MaxCPU is the maximum number of CPUs allowed
	MaxCPU int32 `json:"maxCPU,omitempty"`
	// MinMemoryMiB is the minimum memory in MiB required
	MinMemoryMiB int64 `json:"minMemoryMiB,omitempty"`
	// MaxMemoryMiB is the maximum memory in MiB allowed
	MaxMemoryMiB int64 `json:"maxMemoryMiB,omitempty"`
}

// VMAdoptionReconciler reconciles Provider resources for VM adoption
type VMAdoptionReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RemoteResolver *remote.Resolver
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmclasses,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=vmimages,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles adoption requests
func (r *VMAdoptionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("VMAdoption controller reconciling Provider", "provider", req.NamespacedName)

	// Fetch the Provider
	var provider infravirtrigaudiov1beta1.Provider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Provider not found, may have been deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Provider")
		return ctrl.Result{}, err
	}

	// Check if adoption is requested
	adoptVMs := provider.Annotations[AdoptionAnnotation]
	logger.Info("Checking adoption annotation", "provider", provider.Name, "adoptVMs", adoptVMs)
	if adoptVMs != "true" {
		// Clear adoption status if annotation is removed
		if provider.Status.Adoption != nil {
			provider.Status.Adoption = nil
			if err := r.Status().Update(ctx, &provider); err != nil {
				logger.Error(err, "Failed to clear adoption status")
			}
		}
		return ctrl.Result{}, nil
	}

	// Initialize adoption status
	if provider.Status.Adoption == nil {
		provider.Status.Adoption = &infravirtrigaudiov1beta1.ProviderAdoptionStatus{}
	}

	// Check if provider is ready
	if !r.isProviderReady(&provider) {
		logger.Info("Provider not ready, waiting", "provider", provider.Name)
		provider.Status.Adoption.Message = "Provider not ready"
		if err := r.Status().Update(ctx, &provider); err != nil {
			logger.Error(err, "Failed to update adoption status")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Parse filter annotation if present
	var filter *VMAdoptionFilter
	if filterStr := provider.Annotations[AdoptionFilterAnnotation]; filterStr != "" {
		parsedFilter, err := parseFilterAnnotation(filterStr)
		if err != nil {
			logger.Error(err, "Failed to parse filter annotation", "filter", filterStr)
			provider.Status.Adoption.Message = fmt.Sprintf("Invalid filter: %v", err)
			if updateErr := r.Status().Update(ctx, &provider); updateErr != nil {
				logger.Error(updateErr, "Failed to update adoption status")
			}
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		filter = parsedFilter
		logger.Info("Using VM adoption filter", "filter", filterStr)
	}

	// Discover unmanaged VMs
	unmanagedVMs, err := r.discoverUnmanagedVMs(ctx, &provider, filter)
	if err != nil {
		logger.Error(err, "Failed to discover unmanaged VMs")
		provider.Status.Adoption.Message = fmt.Sprintf("Discovery failed: %v", err)
		if updateErr := r.Status().Update(ctx, &provider); updateErr != nil {
			logger.Error(updateErr, "Failed to update adoption status")
		}
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Update discovery status
	now := metav1.Now()
	provider.Status.Adoption.LastDiscoveryTime = &now
	// #nosec G115 -- len() returns int, conversion to int32 is safe for VM counts (max 2^31-1 VMs is unrealistic)
	provider.Status.Adoption.DiscoveredVMs = int32(len(unmanagedVMs))

	// Adopt VMs
	adoptedCount := int32(0)
	failedCount := int32(0)
	for _, vmInfo := range unmanagedVMs {
		if err := r.adoptVM(ctx, &provider, vmInfo); err != nil {
			logger.Error(err, "Failed to adopt VM", "vm_id", vmInfo.ID, "vm_name", vmInfo.Name)
			failedCount++
		} else {
			adoptedCount++
		}
	}

	// Update adoption status
	provider.Status.Adoption.AdoptedVMs = adoptedCount
	provider.Status.Adoption.FailedAdoptions = failedCount
	if failedCount > 0 {
		provider.Status.Adoption.Message = fmt.Sprintf("Adopted %d VMs, %d failed", adoptedCount, failedCount)
	} else {
		provider.Status.Adoption.Message = fmt.Sprintf("Successfully adopted %d VMs", adoptedCount)
	}

	if err := r.Status().Update(ctx, &provider); err != nil {
		logger.Error(err, "Failed to update adoption status")
		return ctrl.Result{}, err
	}

	// Requeue periodically to check for new VMs
	return ctrl.Result{RequeueAfter: 1 * time.Hour}, nil
}

// discoverUnmanagedVMs finds VMs not yet managed by VirtRigaud
func (r *VMAdoptionReconciler) discoverUnmanagedVMs(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, filter *VMAdoptionFilter) ([]contracts.VMInfo, error) {
	logger := log.FromContext(ctx)

	// Get provider instance
	providerInstance, err := r.getProviderInstance(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider instance: %w", err)
	}

	// List all VMs from provider
	allVMs, err := providerInstance.ListVMs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list VMs: %w", err)
	}

	logger.Info("Discovered VMs from provider", "count", len(allVMs))

	// Get all existing VirtualMachine CRs
	vmList := &infravirtrigaudiov1beta1.VirtualMachineList{}
	if err := r.List(ctx, vmList); err != nil {
		return nil, fmt.Errorf("failed to list VirtualMachine CRs: %w", err)
	}

	// Build map of managed VM IDs (by provider)
	managedVMIDs := make(map[string]bool)
	for _, vm := range vmList.Items {
		// Check if VM is managed by this provider
		providerNamespace := vm.Namespace
		if vm.Spec.ProviderRef.Namespace != "" {
			providerNamespace = vm.Spec.ProviderRef.Namespace
		}

		if vm.Spec.ProviderRef.Name == provider.Name && providerNamespace == provider.Namespace {
			// Use status.ID if available, otherwise use name
			vmID := vm.Status.ID
			if vmID == "" {
				vmID = vm.Name
			}
			managedVMIDs[vmID] = true
		}
	}

	// Filter out managed VMs and apply adoption filter
	var unmanagedVMs []contracts.VMInfo
	for _, vmInfo := range allVMs {
		// Skip if already managed
		if managedVMIDs[vmInfo.ID] {
			continue
		}

		// Apply adoption filter if specified
		if filter != nil && !r.matchesFilter(vmInfo, filter) {
			logger.V(1).Info("VM filtered out by adoption filter", "vm_id", vmInfo.ID, "vm_name", vmInfo.Name)
			continue
		}

		unmanagedVMs = append(unmanagedVMs, vmInfo)
	}

	logger.Info("Found unmanaged VMs", "count", len(unmanagedVMs), "filtered", filter != nil)
	return unmanagedVMs, nil
}

// adoptVM creates VirtualMachine CR for an existing VM
func (r *VMAdoptionReconciler) adoptVM(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, vmInfo contracts.VMInfo) error {
	logger := log.FromContext(ctx).WithValues("vm_id", vmInfo.ID, "vm_name", vmInfo.Name)

	// Generate VM name from VM name (sanitize for Kubernetes)
	vmName := sanitizeVMName(vmInfo.Name)
	if vmName == "" {
		vmName = fmt.Sprintf("vm-%s", vmInfo.ID)
	}

	// Check if VM CR already exists
	existingVM := &infravirtrigaudiov1beta1.VirtualMachine{}
	vmKey := types.NamespacedName{
		Name:      vmName,
		Namespace: provider.Namespace,
	}
	if err := r.Get(ctx, vmKey, existingVM); err == nil {
		logger.Info("VirtualMachine CR already exists, skipping adoption")
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing VM: %w", err)
	}

	// Ensure VMClass exists
	vmClass, err := r.ensureVMClass(ctx, vmInfo, provider.Namespace)
	if err != nil {
		return fmt.Errorf("failed to ensure VMClass: %w", err)
	}

	// Note: For adopted VMs, we use ImportedDisk directly in the VM spec
	// VMImage is not required since we're referencing an existing disk

	// Create VirtualMachine CR
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmName,
			Namespace: provider.Namespace,
			Labels: map[string]string{
				"virtrigaud.io/adopted": "true",
			},
		},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{
				Name:      provider.Name,
				Namespace: provider.Namespace,
			},
			ClassRef: infravirtrigaudiov1beta1.ObjectRef{
				Name:      vmClass.Name,
				Namespace: vmClass.Namespace,
			},
			ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{
				DiskID: vmInfo.ID,
				Format: "qcow2", // Default, will be updated from disk info if available
				Source: "manual",
			},
			PowerState: infravirtrigaudiov1beta1.PowerState(vmInfo.PowerState),
		},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{
			ID:        vmInfo.ID,
			PowerState: infravirtrigaudiov1beta1.PowerState(vmInfo.PowerState),
			IPs:       vmInfo.IPs,
			Provider:  vmInfo.ProviderRaw,
		},
	}

	// Set disk format if available
	if len(vmInfo.Disks) > 0 {
		vm.Spec.ImportedDisk.Format = vmInfo.Disks[0].Format
		if vmInfo.Disks[0].Path != "" {
			vm.Spec.ImportedDisk.Path = vmInfo.Disks[0].Path
		}
	}

	// Set resources from VM info
	if vmInfo.CPU > 0 || vmInfo.MemoryMiB > 0 {
		vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{}
		if vmInfo.CPU > 0 {
			vm.Spec.Resources.CPU = &vmInfo.CPU
		}
		if vmInfo.MemoryMiB > 0 {
			vm.Spec.Resources.MemoryMiB = &vmInfo.MemoryMiB
		}
	}

	if err := r.Create(ctx, vm); err != nil {
		return fmt.Errorf("failed to create VirtualMachine CR: %w", err)
	}

	logger.Info("Successfully adopted VM", "vm_name", vmName)
	return nil
}

// ensureVMClass creates or finds appropriate VMClass
func (r *VMAdoptionReconciler) ensureVMClass(ctx context.Context, vmInfo contracts.VMInfo, namespace string) (*infravirtrigaudiov1beta1.VMClass, error) {
	logger := log.FromContext(ctx)

	// Use defaults if CPU/memory not available
	cpu := vmInfo.CPU
	if cpu == 0 {
		cpu = 2 // Default
	}
	memoryMiB := vmInfo.MemoryMiB
	if memoryMiB == 0 {
		memoryMiB = 4096 // Default 4 GiB
	}

	// Generate VMClass name
	className := fmt.Sprintf("adopted-%dcpu-%dmb", cpu, memoryMiB)

	// Check if VMClass already exists
	vmClass := &infravirtrigaudiov1beta1.VMClass{}
	classKey := types.NamespacedName{
		Name:      className,
		Namespace: namespace,
	}
	if err := r.Get(ctx, classKey, vmClass); err == nil {
		return vmClass, nil
	} else if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to check for existing VMClass: %w", err)
	}

	// Create new VMClass
	vmClass = &infravirtrigaudiov1beta1.VMClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      className,
			Namespace: namespace,
			Labels: map[string]string{
				"virtrigaud.io/adopted": "true",
			},
		},
		Spec: infravirtrigaudiov1beta1.VMClassSpec{
			CPU:     cpu,
			Memory:  resource.MustParse(fmt.Sprintf("%dMi", memoryMiB)),
			Firmware: infravirtrigaudiov1beta1.FirmwareTypeBIOS, // Default, can be enhanced to detect from VM
		},
	}

	if err := r.Create(ctx, vmClass); err != nil {
		return nil, fmt.Errorf("failed to create VMClass: %w", err)
	}

	logger.Info("Created VMClass for adopted VM", "class_name", className)
	return vmClass, nil
}

// Note: For adopted VMs, we use ImportedDisk directly in VirtualMachine spec
// instead of creating a VMImage. This is simpler and more direct for existing VMs.

// getProviderInstance gets the provider instance using RemoteResolver
func (r *VMAdoptionReconciler) getProviderInstance(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	if r.RemoteResolver == nil {
		return nil, fmt.Errorf("remote resolver not configured")
	}

	providerInstance, err := r.RemoteResolver.GetProvider(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to get provider: %w", err)
	}

	return providerInstance, nil
}

// isProviderReady checks if provider is ready for adoption
func (r *VMAdoptionReconciler) isProviderReady(provider *infravirtrigaudiov1beta1.Provider) bool {
	// Check ProviderAvailable condition
	providerAvailable := k8s.GetCondition(provider.Status.Conditions, "ProviderAvailable")
	return providerAvailable != nil && providerAvailable.Status == metav1.ConditionTrue
}

// sanitizeVMName sanitizes VM name for Kubernetes resource name
func sanitizeVMName(name string) string {
	// Convert to lowercase
	name = strings.ToLower(name)
	// Replace invalid characters with hyphens
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, " ", "-")
	// Remove invalid characters
	var result strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			result.WriteRune(r)
		}
	}
	name = result.String()
	// Ensure it starts and ends with alphanumeric
	name = strings.Trim(name, "-")
	// Limit length to 63 characters (Kubernetes limit)
	if len(name) > 63 {
		name = name[:63]
		name = strings.Trim(name, "-")
	}
	return name
}

// parseFilterAnnotation parses the filter annotation JSON string
func parseFilterAnnotation(filterStr string) (*VMAdoptionFilter, error) {
	var filter VMAdoptionFilter
	if err := json.Unmarshal([]byte(filterStr), &filter); err != nil {
		return nil, fmt.Errorf("failed to parse filter JSON: %w", err)
	}

	// Validate regex pattern if provided
	if filter.NamePattern != "" {
		if _, err := regexp.Compile(filter.NamePattern); err != nil {
			return nil, fmt.Errorf("invalid namePattern regex: %w", err)
		}
	}

	return &filter, nil
}

// matchesFilter checks if a VM matches the filter criteria
func (r *VMAdoptionReconciler) matchesFilter(vmInfo contracts.VMInfo, filter *VMAdoptionFilter) bool {
	// Check name pattern
	if filter.NamePattern != "" {
		matched, err := regexp.MatchString(filter.NamePattern, vmInfo.Name)
		if err != nil || !matched {
			return false
		}
	}

	// Check power state
	if filter.PowerState != "" {
		// Normalize power state for comparison (case-insensitive)
		vmPowerState := strings.ToLower(strings.TrimSpace(vmInfo.PowerState))
		filterPowerState := strings.ToLower(strings.TrimSpace(filter.PowerState))
		if vmPowerState != filterPowerState {
			return false
		}
	}

	// Check CPU constraints
	if filter.MinCPU > 0 && vmInfo.CPU < filter.MinCPU {
		return false
	}
	if filter.MaxCPU > 0 && vmInfo.CPU > filter.MaxCPU {
		return false
	}

	// Check memory constraints
	if filter.MinMemoryMiB > 0 && vmInfo.MemoryMiB < filter.MinMemoryMiB {
		return false
	}
	if filter.MaxMemoryMiB > 0 && vmInfo.MemoryMiB > filter.MaxMemoryMiB {
		return false
	}

	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *VMAdoptionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.Provider{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1, // Process one provider at a time
		}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				// Reconcile if adoption annotation is set
				adoptVal := e.Object.GetAnnotations()[AdoptionAnnotation]
				return adoptVal == "true"
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Reconcile if adoption annotation changed
				oldAdoptVal := e.ObjectOld.GetAnnotations()[AdoptionAnnotation]
				newAdoptVal := e.ObjectNew.GetAnnotations()[AdoptionAnnotation]
				if oldAdoptVal != newAdoptVal {
					return true
				}

				// Reconcile if filter annotation changed
				oldFilterVal := e.ObjectOld.GetAnnotations()[AdoptionFilterAnnotation]
				newFilterVal := e.ObjectNew.GetAnnotations()[AdoptionFilterAnnotation]
				return oldFilterVal != newFilterVal
			},
		}).
		Complete(r)
}

