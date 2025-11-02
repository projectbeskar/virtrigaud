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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/util"
)

// ProviderReconciler reconciles a Provider object
type ProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.virtrigaud.io,resources=providers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile manages Provider resources and their runtime deployments
func (r *ProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Provider
	var provider infravirtrigaudiov1beta1.Provider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Provider not found, may have been deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Provider")
		return ctrl.Result{}, err
	}

	// Handle deletion (cleanup deployments and services)
	if !provider.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &provider)
	}

	// Initialize runtime status if needed
	if provider.Status.Runtime == nil {
		provider.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{}
	}

	// Validate that runtime is configured (now required)
	if provider.Spec.Runtime == nil {
		err := fmt.Errorf("runtime configuration is required")
		k8s.SetCondition(&provider.Status.Conditions, "ProviderRuntimeReady", metav1.ConditionFalse, "MissingRuntime", err.Error())
		provider.Status.ObservedGeneration = provider.Generation
		if updateErr := r.Status().Update(ctx, &provider); updateErr != nil {
			logger.Error(updateErr, "Failed to update Provider status")
		}
		return ctrl.Result{}, err
	}

	// Set runtime mode (always Remote)
	provider.Status.Runtime.Mode = infravirtrigaudiov1beta1.RuntimeModeRemote

	// Handle remote runtime reconciliation
	result, err := r.reconcileRemoteRuntime(ctx, &provider)

	// Update provider status
	provider.Status.ObservedGeneration = provider.Generation
	if updateErr := r.Status().Update(ctx, &provider); updateErr != nil {
		logger.Error(updateErr, "Failed to update Provider status")
		if err == nil {
			err = updateErr
		}
	}

	return result, err
}

// handleDeletion cleans up remote runtime resources when Provider is deleted
func (r *ProviderReconciler) handleDeletion(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Always clean up remote runtime resources (all providers are remote now)
	if err := r.cleanupRemoteRuntime(ctx, provider); err != nil {
		logger.Error(err, "Failed to cleanup remote runtime resources")
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	return ctrl.Result{}, nil
}

// reconcileRemoteRuntime manages remote provider deployments and services
func (r *ProviderReconciler) reconcileRemoteRuntime(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Validate remote runtime configuration
	if err := r.validateRemoteRuntimeSpec(provider); err != nil {
		k8s.SetCondition(&provider.Status.Conditions, "ProviderRuntimeReady", metav1.ConditionFalse, "InvalidConfiguration", err.Error())
		provider.Status.Runtime.Phase = infravirtrigaudiov1beta1.ProviderRuntimePhaseFailed
		provider.Status.Runtime.Message = err.Error()
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Generate names for deployment and service
	deploymentName := r.getDeploymentName(provider)
	serviceName := r.getServiceName(provider)

	// Reconcile Service first (needed for endpoint)
	service, err := r.reconcileService(ctx, provider, serviceName)
	if err != nil {
		logger.Error(err, "Failed to reconcile service")
		k8s.SetCondition(&provider.Status.Conditions, "ProviderRuntimeReady", metav1.ConditionFalse, "ServiceError", fmt.Sprintf("Failed to create service: %v", err))
		provider.Status.Runtime.Phase = infravirtrigaudiov1beta1.ProviderRuntimePhaseFailed
		provider.Status.Runtime.Message = err.Error()
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Reconcile Deployment
	deployment, err := r.reconcileDeployment(ctx, provider, deploymentName)
	if err != nil {
		logger.Error(err, "Failed to reconcile deployment")
		k8s.SetCondition(&provider.Status.Conditions, "ProviderRuntimeReady", metav1.ConditionFalse, "DeploymentError", fmt.Sprintf("Failed to create deployment: %v", err))
		provider.Status.Runtime.Phase = infravirtrigaudiov1beta1.ProviderRuntimePhaseFailed
		provider.Status.Runtime.Message = err.Error()
		return ctrl.Result{RequeueAfter: time.Minute}, err
	}

	// Update runtime status
	port := int32(9443)
	if provider.Spec.Runtime.Service != nil && provider.Spec.Runtime.Service.Port != 0 {
		port = provider.Spec.Runtime.Service.Port
	}

	provider.Status.Runtime.Endpoint = fmt.Sprintf("%s.%s.svc.cluster.local:%d", service.Name, provider.Namespace, port)
	provider.Status.Runtime.ServiceRef = &corev1.LocalObjectReference{Name: service.Name}

	// Check deployment readiness
	if deployment.Status.ReadyReplicas > 0 {
		provider.Status.Runtime.Phase = infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning
		provider.Status.Runtime.Message = "Remote provider runtime is ready"

		k8s.SetCondition(&provider.Status.Conditions, "ProviderRuntimeReady", metav1.ConditionTrue, "DeploymentReady", fmt.Sprintf("Deployment has %d ready replicas", deployment.Status.ReadyReplicas))

		k8s.SetCondition(&provider.Status.Conditions, "ProviderAvailable", metav1.ConditionTrue, "RemoteAvailable", "Remote provider is available")
	} else {
		provider.Status.Runtime.Phase = infravirtrigaudiov1beta1.ProviderRuntimePhasePending
		provider.Status.Runtime.Message = "Waiting for deployment to be ready"

		k8s.SetCondition(&provider.Status.Conditions, "ProviderRuntimeReady", metav1.ConditionFalse, "DeploymentNotReady", "Deployment pods are not ready yet")

		// Requeue to check readiness again
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// validateRemoteRuntimeSpec validates the remote runtime configuration
func (r *ProviderReconciler) validateRemoteRuntimeSpec(provider *infravirtrigaudiov1beta1.Provider) error {
	if provider.Spec.Runtime == nil {
		return fmt.Errorf("runtime configuration is required for remote mode")
	}

	if provider.Spec.Runtime.Image == "" {
		return fmt.Errorf("image is required for remote runtime")
	}

	return nil
}

// getDeploymentName generates a unique deployment name for the provider
func (r *ProviderReconciler) getDeploymentName(provider *infravirtrigaudiov1beta1.Provider) string {
	return fmt.Sprintf("virtrigaud-provider-%s-%s", provider.Namespace, provider.Name)
}

// getServiceName generates a unique service name for the provider
func (r *ProviderReconciler) getServiceName(provider *infravirtrigaudiov1beta1.Provider) string {
	return fmt.Sprintf("virtrigaud-provider-%s-%s", provider.Namespace, provider.Name)
}

// reconcileService creates or updates the service for remote provider
func (r *ProviderReconciler) reconcileService(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, serviceName string) (*corev1.Service, error) {
	port := int32(9443)
	if provider.Spec.Runtime.Service != nil && provider.Spec.Runtime.Service.Port != 0 {
		port = provider.Spec.Runtime.Service.Port
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: provider.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "virtrigaud-provider",
				"app.kubernetes.io/instance":   provider.Name,
				"app.kubernetes.io/component":  "provider",
				"app.kubernetes.io/managed-by": "virtrigaud",
				"virtrigaud.io/provider-type":  string(provider.Spec.Type),
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"app.kubernetes.io/name":     "virtrigaud-provider",
				"app.kubernetes.io/instance": provider.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "grpc",
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "metrics",
					Port:       8080,
					TargetPort: intstr.FromInt32(8080),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(provider, desired, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Check if service exists
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: provider.Namespace}, existing)

	if apierrors.IsNotFound(err) {
		// Create new service
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("failed to create service: %w", err)
		}
		return desired, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get service: %w", err)
	}

	// Update existing service if needed
	existing.Spec.Ports = desired.Spec.Ports
	existing.Labels = desired.Labels
	if err := r.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("failed to update service: %w", err)
	}

	return existing, nil
}

// reconcileDeployment creates or updates the deployment for remote provider
func (r *ProviderReconciler) reconcileDeployment(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider, deploymentName string) (*appsv1.Deployment, error) {
	// Default values
	replicas := int32(1)
	if provider.Spec.Runtime.Replicas != nil {
		replicas = *provider.Spec.Runtime.Replicas
	}

	// Build container spec
	container, err := r.buildProviderContainer(provider)
	if err != nil {
		return nil, fmt.Errorf("failed to build container spec: %w", err)
	}

	// Build deployment spec
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: provider.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "virtrigaud-provider",
				"app.kubernetes.io/instance":   provider.Name,
				"app.kubernetes.io/component":  "provider",
				"app.kubernetes.io/managed-by": "virtrigaud",
				"virtrigaud.io/provider-type":  string(provider.Spec.Type),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "virtrigaud-provider",
					"app.kubernetes.io/instance": provider.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":       "virtrigaud-provider",
						"app.kubernetes.io/instance":   provider.Name,
						"app.kubernetes.io/component":  "provider",
						"app.kubernetes.io/managed-by": "virtrigaud",
						"virtrigaud.io/provider-type":  string(provider.Spec.Type),
					},
				},
				Spec: corev1.PodSpec{
					Containers:                    []corev1.Container{*container},
					Volumes:                       r.buildPodVolumes(provider),
					NodeSelector:                  provider.Spec.Runtime.NodeSelector,
					Tolerations:                   provider.Spec.Runtime.Tolerations,
					Affinity:                      provider.Spec.Runtime.Affinity,
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: util.Int64Ptr(30), // Allow time for graceful shutdown
				},
			},
		},
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(provider, desired, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Check if deployment exists
	existing := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: provider.Namespace}, existing)

	if apierrors.IsNotFound(err) {
		// Create new deployment
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("failed to create deployment: %w", err)
		}
		return desired, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get deployment: %w", err)
	}

	// Update existing deployment if needed
	existing.Spec.Replicas = &replicas
	existing.Spec.Template = desired.Spec.Template
	existing.Labels = desired.Labels
	if err := r.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("failed to update deployment: %w", err)
	}

	return existing, nil
}

// buildProviderContainer builds the container spec for the provider
func (r *ProviderReconciler) buildProviderContainer(provider *infravirtrigaudiov1beta1.Provider) (*corev1.Container, error) {
	// Use the image as-is since Runtime.Version field was removed
	image := provider.Spec.Runtime.Image

	// Default resource requirements
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	if provider.Spec.Runtime.Resources != nil {
		resources = *provider.Spec.Runtime.Resources
	}

	// Build environment variables
	env := []corev1.EnvVar{
		{
			Name:  "PROVIDER_TYPE",
			Value: string(provider.Spec.Type),
		},
		{
			Name:  "PROVIDER_ENDPOINT",
			Value: provider.Spec.Endpoint,
		},
		{
			Name: "PROVIDER_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{
			Name: "PROVIDER_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.labels['app.kubernetes.io/instance']",
				},
			},
		},
	}

	// Add TLS environment variable
	tlsEnabled := false // TLS configuration removed in v1beta1
	env = append(env, corev1.EnvVar{
		Name:  "TLS_ENABLED",
		Value: fmt.Sprintf("%t", tlsEnabled),
	})

	// Add TLS insecure skip verify configuration
	env = append(env, corev1.EnvVar{
		Name:  "TLS_INSECURE_SKIP_VERIFY",
		Value: fmt.Sprintf("%t", provider.Spec.InsecureSkipVerify),
	})

	// Add custom environment variables
	if provider.Spec.Runtime.Env != nil {
		env = append(env, provider.Spec.Runtime.Env...)
	}

	// Build volume mounts
	var volumeMounts []corev1.VolumeMount

	// Mount credentials secret
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "provider-credentials",
		MountPath: "/etc/virtrigaud/credentials",
		ReadOnly:  true,
	})

	// Mount TLS certificates if enabled
	if tlsEnabled {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "provider-tls",
			MountPath: "/etc/virtrigaud/tls",
			ReadOnly:  true,
		})
	}

	// Auto-discover and mount migration PVCs
	migrationMounts := r.discoverMigrationVolumeMounts(context.Background(), provider.Namespace)
	volumeMounts = append(volumeMounts, migrationMounts...)

	// Mount temporary directory (needed for read-only root filesystem)
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "tmp",
		MountPath: "/tmp",
	})

	// Default security context
	securityContext := &corev1.SecurityContext{
		RunAsNonRoot:             util.BoolPtr(true),
		RunAsUser:                util.Int64Ptr(65532),
		ReadOnlyRootFilesystem:   util.BoolPtr(true),
		AllowPrivilegeEscalation: util.BoolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}

	if provider.Spec.Runtime.SecurityContext != nil {
		securityContext = provider.Spec.Runtime.SecurityContext
	}

	// Determine gRPC port
	grpcPort := int32(9443)
	if provider.Spec.Runtime.Service != nil && provider.Spec.Runtime.Service.Port != 0 {
		grpcPort = provider.Spec.Runtime.Service.Port
	}

	container := &corev1.Container{
		Name:            "provider",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			fmt.Sprintf("--port=%d", grpcPort),
			"--health-port=8080",
		},
		Env:             env,
		Resources:       resources,
		VolumeMounts:    volumeMounts,
		SecurityContext: securityContext,
		Ports: []corev1.ContainerPort{
			{
				Name:          "grpc",
				ContainerPort: grpcPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "metrics",
				ContainerPort: 8080,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt32(8080),
				},
			},
			InitialDelaySeconds: 15,
			PeriodSeconds:       20,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt32(8080),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					// Sleep to allow in-flight gRPC requests to complete
					// The gRPC server should handle graceful shutdown internally
					Command: []string{"/bin/sh", "-c", "sleep 15"},
				},
			},
		},
	}

	// Add volumes to pod spec (we'll need to modify the caller to handle this)
	return container, nil
}

// buildPodVolumes builds the volumes for the provider pod
func (r *ProviderReconciler) buildPodVolumes(provider *infravirtrigaudiov1beta1.Provider) []corev1.Volume {
	var volumes []corev1.Volume

	// Add credentials volume
	volumes = append(volumes, corev1.Volume{
		Name: "provider-credentials",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: provider.Spec.CredentialSecretRef.Name,
			},
		},
	})

	// Add TLS volume if enabled
	// TLS configuration removed in v1beta1
	if false {
		volumes = append(volumes, corev1.Volume{
			Name: "provider-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "tls-secret",
				},
			},
		})
	}

	// Auto-discover and mount migration PVCs
	// This allows providers to access migration storage without manual configuration
	migrationVolumes := r.discoverMigrationPVCs(context.Background(), provider.Namespace)
	volumes = append(volumes, migrationVolumes...)

	// Add temporary directory volume (needed for read-only root filesystem)
	volumes = append(volumes, corev1.Volume{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	return volumes
}

// discoverMigrationPVCs finds all migration PVCs in the namespace and returns volume definitions for them
func (r *ProviderReconciler) discoverMigrationPVCs(ctx context.Context, namespace string) []corev1.Volume {
	var volumes []corev1.Volume

	// List all PVCs in the namespace with migration labels
	pvcList := &corev1.PersistentVolumeClaimList{}
	err := r.List(ctx, pvcList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"virtrigaud.io/component": "migration-storage",
		},
	)

	if err != nil {
		// Log error but don't fail - migrations might not be active
		return volumes
	}

	// Create a volume for each migration PVC
	for _, pvc := range pvcList.Items {
		// Generate a safe volume name from PVC name (K8s volume names must be DNS label)
		volumeName := fmt.Sprintf("migration-%s", pvc.Name)

		volumes = append(volumes, corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.Name,
				},
			},
		})
	}

	return volumes
}

// discoverMigrationVolumeMounts finds all migration PVCs and returns volume mounts for them
func (r *ProviderReconciler) discoverMigrationVolumeMounts(ctx context.Context, namespace string) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount

	// List all PVCs in the namespace with migration labels
	pvcList := &corev1.PersistentVolumeClaimList{}
	err := r.List(ctx, pvcList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"virtrigaud.io/component": "migration-storage",
		},
	)

	if err != nil {
		// Log error but don't fail - migrations might not be active
		return mounts
	}

	// Create a volume mount for each migration PVC
	for _, pvc := range pvcList.Items {
		// Generate a safe volume name (must match the volume name in buildPodVolumes)
		volumeName := fmt.Sprintf("migration-%s", pvc.Name)

		// Mount at /mnt/migration-storage/<pvc-name>
		// This allows multiple migration PVCs to be mounted if needed
		mountPath := fmt.Sprintf("/mnt/migration-storage/%s", pvc.Name)

		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: mountPath,
			ReadOnly:  false, // Migrations need read-write access
		})
	}

	return mounts
}

// cleanupRemoteRuntime cleans up deployment and service for remote providers
func (r *ProviderReconciler) cleanupRemoteRuntime(ctx context.Context, provider *infravirtrigaudiov1beta1.Provider) error {
	deploymentName := r.getDeploymentName(provider)
	serviceName := r.getServiceName(provider)

	// Delete deployment
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: provider.Namespace,
		},
	}
	if err := r.Delete(ctx, deployment); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete deployment: %w", err)
	}

	// Delete service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: provider.Namespace,
		},
	}
	if err := r.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete service: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infravirtrigaudiov1beta1.Provider{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 5, // Process up to 5 providers in parallel
		}).
		Named("provider").
		Complete(r)
}
