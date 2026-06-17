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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// migrationTestScheme builds a scheme with the core + infra types used by these tests.
func migrationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(s))
	return s
}

func migrationPVC(name, ns string, withFinalizer bool) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{migrationPVCLabelKey: migrationPVCLabelValue},
		},
	}
	if withFinalizer {
		// A finalizer lets the fake client retain the object (with a
		// DeletionTimestamp) after Delete, simulating a stuck Terminating PVC.
		pvc.Finalizers = []string{"kubernetes.io/pvc-protection"}
	}
	return pvc
}

// TestDiscoverMigrationPVCs_SkipsDeletingPVCs verifies that a migration PVC that
// is being deleted is NOT turned into a provider volume (issue #184): mounting a
// Terminating PVC would wedge the provider rollout.
func TestDiscoverMigrationPVCs_SkipsDeletingPVCs(t *testing.T) {
	ctx := context.Background()
	scheme := migrationTestScheme(t)
	ns := "default"

	normal := migrationPVC("mig-normal", ns, false)
	deleting := migrationPVC("mig-deleting", ns, true)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(normal, deleting).Build()
	// Delete the second PVC: its finalizer keeps it present with a DeletionTimestamp.
	require.NoError(t, c.Delete(ctx, deleting))

	r := &ProviderReconciler{Client: c, Scheme: scheme}

	volumes := r.discoverMigrationPVCs(ctx, ns)

	require.Len(t, volumes, 1, "only the non-deleting migration PVC should be mounted")
	assert.Equal(t, "migration-mig-normal", volumes[0].Name)
	require.NotNil(t, volumes[0].PersistentVolumeClaim)
	assert.Equal(t, "mig-normal", volumes[0].PersistentVolumeClaim.ClaimName)
}

// TestDiscoverMigrationVolumeMounts_SkipsDeletingPVCs verifies the mount side
// mirrors the volume side (issue #184).
func TestDiscoverMigrationVolumeMounts_SkipsDeletingPVCs(t *testing.T) {
	ctx := context.Background()
	scheme := migrationTestScheme(t)
	ns := "default"

	normal := migrationPVC("mig-normal", ns, false)
	deleting := migrationPVC("mig-deleting", ns, true)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(normal, deleting).Build()
	require.NoError(t, c.Delete(ctx, deleting))

	r := &ProviderReconciler{Client: c, Scheme: scheme}

	mounts := r.discoverMigrationVolumeMounts(ctx, ns)

	require.Len(t, mounts, 1, "only the non-deleting migration PVC should be mounted")
	assert.Equal(t, "migration-mig-normal", mounts[0].Name)
	assert.Equal(t, "/mnt/migration-storage/mig-normal", mounts[0].MountPath)
}

// TestDiscoverMigration_MergesAllActive_AndDropsRemoved reproduces the two #230
// bugs and asserts the discovery-rebuild model fixes both:
//
//  1. Parallel migrations no longer clobber each other — with TWO active migration
//     PVCs in the namespace, discovery returns a volume+mount for BOTH (the desired
//     set is rebuilt from the full PVC list each reconcile, not from one migration's
//     context, so it is not last-writer-wins).
//  2. Stale mounts are removed — once a PVC is gone, the next discovery no longer
//     returns its volume/mount, so a provider re-reconcile drops it.
func TestDiscoverMigration_MergesAllActive_AndDropsRemoved(t *testing.T) {
	ctx := context.Background()
	scheme := migrationTestScheme(t)
	ns := "default"

	a := migrationPVC("mig-a", ns, false)
	b := migrationPVC("mig-b", ns, false)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b).Build()
	r := &ProviderReconciler{Client: c, Scheme: scheme}

	volNames := func() map[string]string {
		out := map[string]string{}
		for _, v := range r.discoverMigrationPVCs(ctx, ns) {
			require.NotNil(t, v.PersistentVolumeClaim)
			out[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
		return out
	}
	mountNames := func() map[string]string {
		out := map[string]string{}
		for _, m := range r.discoverMigrationVolumeMounts(ctx, ns) {
			out[m.Name] = m.MountPath
		}
		return out
	}

	// (1) Parallel merge: both active PVCs are present in the desired volume/mount set.
	vols := volNames()
	require.Len(t, vols, 2, "both concurrent migration PVCs must be mounted (no last-writer-wins)")
	assert.Equal(t, "mig-a", vols["migration-mig-a"])
	assert.Equal(t, "mig-b", vols["migration-mig-b"])

	mounts := mountNames()
	require.Len(t, mounts, 2)
	assert.Equal(t, "/mnt/migration-storage/mig-a", mounts["migration-mig-a"])
	assert.Equal(t, "/mnt/migration-storage/mig-b", mounts["migration-mig-b"])

	// (2) Stale removal: delete mig-a entirely (no finalizer → actually removed). The
	// next discovery rebuilds from the current PVC list and drops mig-a's volume/mount.
	require.NoError(t, c.Delete(ctx, a))
	vols = volNames()
	require.Len(t, vols, 1, "removed PVC's volume must not survive the rebuild")
	assert.Equal(t, "mig-b", vols["migration-mig-b"])
	assert.NotContains(t, mountNames(), "migration-mig-a")

	// Remove the last one too → the set is empty (no stale leftovers).
	require.NoError(t, c.Delete(ctx, b))
	assert.Empty(t, r.discoverMigrationPVCs(ctx, ns))
	assert.Empty(t, r.discoverMigrationVolumeMounts(ctx, ns))
}

// TestProvidersForMigrationPVC verifies the watch map function enqueues every
// Provider in the PVC's namespace for a migration PVC, and ignores non-migration
// PVCs (issue #184).
func TestProvidersForMigrationPVC(t *testing.T) {
	ctx := context.Background()
	scheme := migrationTestScheme(t)
	ns := "default"

	p1 := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: ns}}
	p2 := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: ns}}
	other := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "other-ns"}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p1, p2, other).Build()
	r := &ProviderReconciler{Client: c, Scheme: scheme}

	t.Run("migration PVC enqueues same-namespace providers", func(t *testing.T) {
		reqs := r.providersForMigrationPVC(ctx, migrationPVC("mig-x", ns, false))
		names := map[string]bool{}
		for _, req := range reqs {
			assert.Equal(t, ns, req.Namespace)
			names[req.Name] = true
		}
		assert.Len(t, reqs, 2)
		assert.True(t, names["p1"] && names["p2"], "both same-namespace providers enqueued")
		assert.False(t, names["p3"], "providers in other namespaces are not enqueued")
	})

	t.Run("non-migration PVC is ignored", func(t *testing.T) {
		plain := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: ns}}
		assert.Nil(t, r.providersForMigrationPVC(ctx, plain))
	})
}
