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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// mountTestScheme builds a scheme with the apps + core + infra types the
// migration mount-handshake checks read.
func mountTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(s))
	return s
}

// mountPVCVolume builds the volume the provider controller attaches for a
// migration PVC (name scheme mirrors discoverMigrationPVCs).
func mountPVCVolume(pvcName string) corev1.Volume {
	return corev1.Volume{
		Name: "migration-" + pvcName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
		},
	}
}

// providerDeployment builds a provider Deployment named like the one the
// provider controller reconciles.
func providerDeployment(ns, name string, replicas, updated, ready int32, withPVC bool, pvcName string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("virtrigaud-provider-%s-%s", ns, name),
			Namespace: ns,
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			UpdatedReplicas: updated,
			ReadyReplicas:   ready,
		},
	}
	if withPVC {
		d.Spec.Template.Spec.Volumes = []corev1.Volume{mountPVCVolume(pvcName)}
	}
	return d
}

// providerPod builds a provider pod with the standard selector labels.
func providerPod(ns, providerName, podName string, phase corev1.PodPhase, ready, withPVC bool, pvcName string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "virtrigaud-provider",
				"app.kubernetes.io/instance": providerName,
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if withPVC {
		p.Spec.Volumes = []corev1.Volume{mountPVCVolume(pvcName)}
	}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return p
}

func readyProvider(ns, name string) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status: infravirtrigaudiov1beta1.ProviderStatus{
			Conditions: []metav1.Condition{{
				Type:               "ProviderAvailable",
				Status:             metav1.ConditionTrue,
				Reason:             "RemoteAvailable",
				Message:            "ready",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// TestProviderMountReady covers the single-shot mount evaluation that replaced
// the blocking time.Sleep poll.
func TestProviderMountReady(t *testing.T) {
	const ns, provName, pvc = "default", "vsphere-lab", "mig-1-storage"

	tests := []struct {
		name       string
		objs       []client.Object
		deleting   string // pod name to Delete (sets DeletionTimestamp via finalizer)
		wantReady  bool
		wantReason string
	}{
		{
			name:       "deployment missing",
			objs:       nil,
			wantReady:  false,
			wantReason: "not found yet",
		},
		{
			name:       "deployment without PVC volume",
			objs:       []client.Object{providerDeployment(ns, provName, 1, 1, 1, false, pvc)},
			wantReady:  false,
			wantReason: "not yet updated with the migration PVC volume",
		},
		{
			name:       "rollout in progress",
			objs:       []client.Object{providerDeployment(ns, provName, 1, 0, 1, true, pvc)},
			wantReady:  false,
			wantReason: "rollout in progress",
		},
		{
			name: "rolled out but only old pod without PVC",
			objs: []client.Object{
				providerDeployment(ns, provName, 1, 1, 1, true, pvc),
				providerPod(ns, provName, "old-pod", corev1.PodRunning, true, false, pvc),
			},
			wantReady:  false,
			wantReason: "no Ready pod with the migration PVC mounted yet",
		},
		{
			name: "pod with PVC not yet Ready",
			objs: []client.Object{
				providerDeployment(ns, provName, 1, 1, 1, true, pvc),
				providerPod(ns, provName, "new-pod", corev1.PodPending, false, true, pvc),
			},
			wantReady:  false,
			wantReason: "no Ready pod with the migration PVC mounted yet",
		},
		{
			name: "ready pod with PVC mounted",
			objs: []client.Object{
				providerDeployment(ns, provName, 1, 1, 1, true, pvc),
				providerPod(ns, provName, "new-pod", corev1.PodRunning, true, true, pvc),
			},
			wantReady: true,
		},
		{
			name: "ready PVC pod is terminating -> ignored",
			objs: []client.Object{
				providerDeployment(ns, provName, 1, 1, 1, true, pvc),
				providerPod(ns, provName, "dying-pod", corev1.PodRunning, true, true, pvc),
			},
			deleting:   "dying-pod",
			wantReady:  false,
			wantReason: "no Ready pod with the migration PVC mounted yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := mountTestScheme(t)
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if len(tc.objs) > 0 {
				builder = builder.WithObjects(tc.objs...)
			}
			c := builder.Build()

			if tc.deleting != "" {
				// Give the pod a finalizer then Delete it so the fake client
				// retains it with a DeletionTimestamp (Terminating).
				pod := &corev1.Pod{}
				require.NoError(t, c.Get(ctx, types.NamespacedName{Name: tc.deleting, Namespace: ns}, pod))
				pod.Finalizers = []string{"test/keep"}
				require.NoError(t, c.Update(ctx, pod))
				require.NoError(t, c.Delete(ctx, pod))
			}

			r := &VMMigrationReconciler{Client: c, Scheme: scheme}
			provider := &infravirtrigaudiov1beta1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: provName, Namespace: ns},
			}

			ready, reason, err := r.providerMountReady(ctx, provider, pvc)
			require.NoError(t, err)
			assert.Equal(t, tc.wantReady, ready)
			if tc.wantReason != "" {
				assert.Contains(t, reason, tc.wantReason)
			}
		})
	}
}

// TestMigrationProvidersMounted verifies both providers must be mounted and the
// reason is prefixed with which side is pending.
func TestMigrationProvidersMounted(t *testing.T) {
	const ns, pvc = "default", "mig-1-storage"
	ctx := context.Background()
	scheme := mountTestScheme(t)

	source := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: ns}}
	target := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "tgt", Namespace: ns}}

	t.Run("target still pending", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			// source fully mounted
			providerDeployment(ns, "src", 1, 1, 1, true, pvc),
			providerPod(ns, "src", "src-pod", corev1.PodRunning, true, true, pvc),
			// target deployment not yet updated
			providerDeployment(ns, "tgt", 1, 1, 1, false, pvc),
		).Build()
		r := &VMMigrationReconciler{Client: c, Scheme: scheme}

		ready, reason, err := r.migrationProvidersMounted(ctx, source, target, pvc)
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Contains(t, reason, "target:")
	})

	t.Run("both mounted", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			providerDeployment(ns, "src", 1, 1, 1, true, pvc),
			providerPod(ns, "src", "src-pod", corev1.PodRunning, true, true, pvc),
			providerDeployment(ns, "tgt", 1, 1, 1, true, pvc),
			providerPod(ns, "tgt", "tgt-pod", corev1.PodRunning, true, true, pvc),
		).Build()
		r := &VMMigrationReconciler{Client: c, Scheme: scheme}

		ready, reason, err := r.migrationProvidersMounted(ctx, source, target, pvc)
		require.NoError(t, err)
		assert.True(t, ready)
		assert.Empty(t, reason)
	})
}

// TestMigrationMountDeadlineExceeded verifies the wait is bounded by the PVC's age.
func TestMigrationMountDeadlineExceeded(t *testing.T) {
	const ns, pvcName = "default", "mig-1-storage"
	ctx := context.Background()
	scheme := mountTestScheme(t)

	migration := &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-1", Namespace: ns},
	}

	newPVC := func(age time.Duration) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:              pvcName,
				Namespace:         ns,
				CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			},
		}
	}

	t.Run("fresh PVC is within deadline", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newPVC(10 * time.Second)).Build()
		r := &VMMigrationReconciler{Client: c, Scheme: scheme}
		exceeded, err := r.migrationMountDeadlineExceeded(ctx, migration, pvcName)
		require.NoError(t, err)
		assert.False(t, exceeded)
	})

	t.Run("old PVC has exceeded the deadline", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(newPVC(migrationMountTimeout + time.Minute)).Build()
		r := &VMMigrationReconciler{Client: c, Scheme: scheme}
		exceeded, err := r.migrationMountDeadlineExceeded(ctx, migration, pvcName)
		require.NoError(t, err)
		assert.True(t, exceeded)
	})
}

// TestHandleValidatingPhase_CrossNamespaceFailsFast verifies a PVC-based
// migration whose providers are not co-located with the migration fails fast
// with an actionable message instead of waiting for a mount that can never
// happen (#229).
func TestHandleValidatingPhase_CrossNamespaceFailsFast(t *testing.T) {
	ctx := context.Background()
	scheme := mountTestScheme(t)

	migration := &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig-x", Namespace: "default"},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Source: infravirtrigaudiov1beta1.MigrationSource{
				VMRef:       infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"},
				ProviderRef: &infravirtrigaudiov1beta1.ObjectRef{Name: "src-prov", Namespace: "team-a"},
			},
			Target: infravirtrigaudiov1beta1.MigrationTarget{
				Name:        "tgt-vm",
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "tgt-prov"},
			},
			Storage: &infravirtrigaudiov1beta1.MigrationStorage{
				Type: "pvc",
				PVC: &infravirtrigaudiov1beta1.PVCStorageConfig{
					StorageClassName: "std",
					Size:             "10Gi",
				},
			},
		},
		Status: infravirtrigaudiov1beta1.VMMigrationStatus{Phase: infravirtrigaudiov1beta1.MigrationPhaseValidating},
	}

	srcVM := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: "default"},
		Status:     infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "vm-1"},
	}
	srcProv := readyProvider("team-a", "src-prov") // different namespace than the migration
	tgtProv := readyProvider("default", "tgt-prov")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(migration, srcVM, srcProv, tgtProv).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VMMigration{}).
		Build()

	r := &VMMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(16),
	}

	_, err := r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)

	updated := &infravirtrigaudiov1beta1.VMMigration{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "mig-x", Namespace: "default"}, updated))
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseFailed, updated.Status.Phase)
	assert.Contains(t, updated.Status.Message, "share one namespace")
}
