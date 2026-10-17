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
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

const (
	msNS        = "team-a"
	msSource    = "src-prov"
	msTarget    = "tgt-prov"
	msBystander = "other-prov"
	msSourceVM  = "src-vm"
	msMig       = "mig"
	msMigUID    = types.UID("mig-uid")
	msPVC       = "mig-storage"
	msWFFC      = "wffc"
	msImmediate = "immediate"
)

// migrationTestScheme builds a scheme with the core, storage and infra types
// used by these tests.
func migrationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, storagev1.AddToScheme(s))
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(s))
	return s
}

// msProvider is a Provider in ns.
func msProvider(ns, name string) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

// msSourceVMObj is the source VM on the source Provider.
func msSourceVMObj(ns string) *infravirtrigaudiov1beta1.VirtualMachine {
	return &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: msSourceVM, Namespace: ns},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: msSource}},
	}
}

// msMigration is an in-flight (Exporting) pvc migration from msSourceVM to
// msTarget whose controller recorded pvcName (auto-created when named is "").
func msMigration(ns, name string, uid types.UID, pvcName, named string) *infravirtrigaudiov1beta1.VMMigration {
	return &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: msSourceVM}},
			Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: msTarget}},
			Storage: &infravirtrigaudiov1beta1.MigrationStorage{
				Type: storagemigration.BackendPVC,
				PVC:  &infravirtrigaudiov1beta1.PVCStorageConfig{Name: named, StorageClassName: msWFFC, Size: "10Gi"},
			},
		},
		Status: infravirtrigaudiov1beta1.VMMigrationStatus{
			Phase:          infravirtrigaudiov1beta1.MigrationPhaseExporting,
			StoragePVCName: pvcName,
		},
	}
}

// msPVCObj is a Bound PVC; labelled marks it as migration storage and owner
// (when set) makes it controlled by that VMMigration.
func msPVCObj(ns, name string, labelled bool, owner *infravirtrigaudiov1beta1.VMMigration) *corev1.PersistentVolumeClaim {
	sc := msWFFC
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + ns + "-" + name)},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if labelled {
		pvc.Labels = map[string]string{migrationPVCLabelKey: migrationPVCLabelValue}
	}
	if owner != nil {
		isController := true
		pvc.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: infravirtrigaudiov1beta1.GroupVersion.String(), Kind: vmMigrationKind,
			Name: owner.Name, UID: owner.UID, Controller: &isController,
		}}
	}
	return pvc
}

// msStorageClasses are a WaitForFirstConsumer and an Immediate StorageClass.
func msStorageClasses() []client.Object {
	wffc := storagev1.VolumeBindingWaitForFirstConsumer
	immediate := storagev1.VolumeBindingImmediate
	return []client.Object{
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: msWFFC}, Provisioner: "x", VolumeBindingMode: &wffc},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: msImmediate}, Provisioner: "x", VolumeBindingMode: &immediate},
	}
}

// msOwnPVC is the Bound, labelled PVC m created, with its UID recorded in
// m's status as the migration controller records it.
func msOwnPVC(m *infravirtrigaudiov1beta1.VMMigration) *corev1.PersistentVolumeClaim {
	pvc := msPVCObj(m.Namespace, m.Status.StoragePVCName, true, m)
	m.Status.StoragePVCUID = string(pvc.UID)
	return pvc
}

// msReadyPod is a Running, Ready pod of Provider name carrying PVC pvcName.
func msReadyPod(ns, name, pvcName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: ns, Labels: map[string]string{
			providerPodNameLabel: providerPodNameValue, providerPodInstanceLabel: name,
		}},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

// msWorld is the default fixture: three Providers, the source VM, an
// in-flight migration and its own Bound, labelled PVC (UID recorded).
func msWorld() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim, []client.Object) {
	m := msMigration(msNS, msMig, msMigUID, msPVC, "")
	pvc := msOwnPVC(m)
	objs := []client.Object{
		msProvider(msNS, msSource), msProvider(msNS, msTarget), msProvider(msNS, msBystander),
		msSourceVMObj(msNS),
	}
	return m, pvc, append(objs, msStorageClasses()...)
}

func msReconciler(t *testing.T, objs ...client.Object) *ProviderReconciler {
	t.Helper()
	s := migrationTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VMMigration{}, &corev1.PersistentVolumeClaim{}).Build()
	return &ProviderReconciler{Client: c, Scheme: s}
}

// msMounts returns the discovered mounts of Provider name, keyed by PVC name.
func msMounts(t *testing.T, r *ProviderReconciler, name string) map[string]migrationStorageMount {
	t.Helper()
	mounts, err := r.discoverMigrationStorage(context.Background(), msProvider(msNS, name))
	require.NoError(t, err)
	out := map[string]migrationStorageMount{}
	for _, m := range mounts {
		out[m.pvcName] = m
	}
	return out
}

// TestDiscoverMigrationStorage_MountsOnlyForSourceAndTarget: the source
// Provider mounts the migration's PVC read-write, the target read-only, and a
// bystander Provider in the same namespace not at all.
func TestDiscoverMigrationStorage_MountsOnlyForSourceAndTarget(t *testing.T) {
	m, pvc, objs := msWorld()
	r := msReconciler(t, append(objs, m, pvc)...)

	src := msMounts(t, r, msSource)
	require.Contains(t, src, msPVC)
	assert.False(t, src[msPVC].readOnly, "the source exports into the PVC: read-write")

	tgt := msMounts(t, r, msTarget)
	require.Contains(t, tgt, msPVC)
	assert.True(t, tgt[msPVC].readOnly, "the target only reads the staged disk: read-only")

	assert.Empty(t, msMounts(t, r, msBystander), "a Provider the migration does not use mounts nothing")
}

// TestDiscoverMigrationStorage_SameProviderBothEnds: a Provider that is both
// the source and the target mounts the PVC read-write.
func TestDiscoverMigrationStorage_SameProviderBothEnds(t *testing.T) {
	m, pvc, objs := msWorld()
	m.Spec.Target.ProviderRef.Name = msSource
	r := msReconciler(t, append(objs, m, pvc)...)
	src := msMounts(t, r, msSource)
	require.Contains(t, src, msPVC)
	assert.False(t, src[msPVC].readOnly)
}

// TestDiscoverMigrationStorage_LabelledButUnreferencedPVC: the label alone no
// longer attaches a PVC to the credentialed provider pod.
func TestDiscoverMigrationStorage_LabelledButUnreferencedPVC(t *testing.T) {
	_, _, objs := msWorld()
	rogue := msPVCObj(msNS, "rogue", true, nil)
	r := msReconciler(t, append(objs, rogue)...)
	for _, p := range []string{msSource, msTarget, msBystander} {
		assert.Empty(t, msMounts(t, r, p), "provider %s must not mount an unreferenced labelled PVC", p)
	}
}

// TestDiscoverMigrationStorage_DeletedMigration: the PVC of a migration that
// is gone, or being deleted, is not mounted.
func TestDiscoverMigrationStorage_DeletedMigration(t *testing.T) {
	t.Run("migration gone", func(t *testing.T) {
		_, pvc, objs := msWorld()
		r := msReconciler(t, append(objs, pvc)...) // the PVC outlived its migration
		assert.Empty(t, msMounts(t, r, msSource))
		assert.Empty(t, msMounts(t, r, msTarget))
	})
	t.Run("migration being deleted", func(t *testing.T) {
		m, pvc, objs := msWorld()
		m.Finalizers = []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}
		r := msReconciler(t, append(objs, m, pvc)...)
		require.NoError(t, r.Delete(context.Background(), m))
		assert.Empty(t, msMounts(t, r, msSource))
		assert.Empty(t, msMounts(t, r, msTarget))
	})
}

// TestDiscoverMigrationStorage_Phases: Ready and terminally Failed migrations
// release the mount; a Failed migration that will be retried keeps it.
func TestDiscoverMigrationStorage_Phases(t *testing.T) {
	retries := int32(2)
	retryPending := func(m *infravirtrigaudiov1beta1.VMMigration) {
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseFailed
		m.Spec.Options = &infravirtrigaudiov1beta1.MigrationOptions{RetryPolicy: &infravirtrigaudiov1beta1.MigrationRetryPolicy{MaxRetries: &retries}}
		m.Status.RetryCount = 1
	}
	cases := []struct {
		name    string
		mutate  func(*infravirtrigaudiov1beta1.VMMigration)
		extra   []client.Object
		mounted bool
	}{
		{"exporting", func(*infravirtrigaudiov1beta1.VMMigration) {}, nil, true},
		{"importing", func(m *infravirtrigaudiov1beta1.VMMigration) {
			m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseImporting
		}, nil, true},
		{"ready", func(m *infravirtrigaudiov1beta1.VMMigration) {
			m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseReady
		}, nil, false},
		{"failed, no retry policy", func(m *infravirtrigaudiov1beta1.VMMigration) {
			m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseFailed
		}, nil, false},
		{"failed, retry pending, the mount worked (a Ready pod carries the PVC)", retryPending,
			[]client.Object{msReadyPod(msNS, msSource, msPVC)}, true},
		{"failed, retry pending, the mount never worked (no Ready pod with it)", retryPending, nil, false},
		{"failed, retry pending, only a not-Ready pod carries it", retryPending, []client.Object{func() client.Object {
			p := msReadyPod(msNS, msSource, msPVC)
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = nil
			return p
		}()}, false},
		{"failed, retries used up", func(m *infravirtrigaudiov1beta1.VMMigration) {
			m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseFailed
			m.Spec.Options = &infravirtrigaudiov1beta1.MigrationOptions{RetryPolicy: &infravirtrigaudiov1beta1.MigrationRetryPolicy{MaxRetries: &retries}}
			m.Status.RetryCount = 2
		}, []client.Object{msReadyPod(msNS, msSource, msPVC)}, false},
		{"no PVC recorded yet", func(m *infravirtrigaudiov1beta1.VMMigration) { m.Status.StoragePVCName = "" }, nil, false},
		{"s3 backend", func(m *infravirtrigaudiov1beta1.VMMigration) { m.Spec.Storage.Type = storagemigration.BackendS3 }, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, pvc, objs := msWorld()
			tc.mutate(m)
			r := msReconciler(t, append(append(objs, m, pvc), tc.extra...)...)
			if tc.mounted {
				assert.Contains(t, msMounts(t, r, msSource), msPVC)
			} else {
				assert.Empty(t, msMounts(t, r, msSource))
			}
		})
	}
}

// TestDiscoverMigrationStorage_Reference: the recorded PVC must be the
// migration's own (auto-created, controlled by its UID) or the labelled PVC
// spec.storage.pvc.name names, not another migration's.
func TestDiscoverMigrationStorage_Reference(t *testing.T) {
	other := msMigration(msNS, "other", "other-uid", "other-storage", "")
	cases := []struct {
		name    string
		build   func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim)
		mounted bool
	}{
		{"auto-created, owned", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			m, pvc, _ := msWorld()
			return m, pvc
		}, true},
		{"auto name taken by a PVC the migration did not create", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			m := msMigration(msNS, msMig, msMigUID, msPVC, "")
			m.Status.StoragePVCUID = "uid-of-the-pvc-it-created"
			return m, msPVCObj(msNS, msPVC, true, nil)
		}, false},
		{"squat with a forged controller ownerReference to the migration", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			m := msMigration(msNS, msMig, msMigUID, msPVC, "")
			m.Status.StoragePVCUID = "uid-of-the-pvc-it-created"
			return m, msPVCObj(msNS, msPVC, true, m) // ownerRef names m's UID; the PVC's own UID differs
		}, false},
		{"auto name controlled by a migration of the same name with another UID", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			m := msMigration(msNS, msMig, msMigUID, msPVC, "")
			stale := msMigration(msNS, msMig, "stale-uid", msPVC, "")
			return m, msPVCObj(msNS, msPVC, true, stale)
		}, false},
		{"owned but label removed", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			m := msMigration(msNS, msMig, msMigUID, msPVC, "")
			pvc := msOwnPVC(m)
			pvc.Labels = nil
			return m, pvc
		}, false},
		{"legacy: created by an older manager (no UID recorded), controlled by the migration", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			m := msMigration(msNS, msMig, msMigUID, msPVC, "")
			return m, msPVCObj(msNS, msPVC, true, m)
		}, true},
		{"legacy fallback never applies to a name other than <migration>-storage", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			m := msMigration(msNS, msMig, msMigUID, "elsewhere", "")
			return m, msPVCObj(msNS, "elsewhere", true, m)
		}, false},
		{"named, labelled", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			return msMigration(msNS, msMig, msMigUID, "shared", "shared"), msPVCObj(msNS, "shared", true, nil)
		}, true},
		{"named, not labelled", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			return msMigration(msNS, msMig, msMigUID, "db-data", "db-data"), msPVCObj(msNS, "db-data", false, nil)
		}, false},
		{"named, another migration's PVC", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			return msMigration(msNS, msMig, msMigUID, "other-storage", "other-storage"), msPVCObj(msNS, "other-storage", true, other)
		}, false},
		{"status names a PVC other than the named one", func() (*infravirtrigaudiov1beta1.VMMigration, *corev1.PersistentVolumeClaim) {
			return msMigration(msNS, msMig, msMigUID, "elsewhere", "shared"), msPVCObj(msNS, "elsewhere", true, nil)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, objs := msWorld()
			m, pvc := tc.build()
			r := msReconciler(t, append(objs, m, pvc)...)
			if tc.mounted {
				assert.Contains(t, msMounts(t, r, msSource), pvc.Name)
			} else {
				assert.Empty(t, msMounts(t, r, msSource))
			}
		})
	}
}

// TestDiscoverMigrationStorage_CrossNamespace: a Provider mounts nothing for
// a migration in another namespace (a pod mounts PVCs of its own namespace
// only), even one that targets it; and a PVC of the same name in another
// namespace is never picked up.
func TestDiscoverMigrationStorage_CrossNamespace(t *testing.T) {
	_, _, objs := msWorld()
	foreign := msMigration("team-b", msMig, msMigUID, msPVC, "")
	foreign.Spec.Target.ProviderRef = infravirtrigaudiov1beta1.ObjectRef{Name: msTarget, Namespace: msNS}
	foreignPVC := msPVCObj("team-b", msPVC, true, foreign)
	r := msReconciler(t, append(objs, foreign, foreignPVC, msSourceVMObj("team-b"), msProvider("team-b", msSource))...)
	assert.Empty(t, msMounts(t, r, msTarget), "the target Provider must not mount a PVC for a migration in another namespace")

	// A same-namespace migration whose PVC exists only in another namespace.
	local := msMigration(msNS, msMig, msMigUID, msPVC, "")
	r = msReconciler(t, append(objs, local, msPVCObj("team-b", msPVC, true, local))...)
	assert.Empty(t, msMounts(t, r, msSource))
}

// TestDiscoverMigrationStorage_Binding: a deleting PVC is never mounted; a
// Pending PVC is mounted only while the migration waits for the mount and
// only on a WaitForFirstConsumer StorageClass, so an unbindable PVC cannot
// wedge (or, with Recreate, take down) the provider.
func TestDiscoverMigrationStorage_Binding(t *testing.T) {
	sc := func(name string) *string { return &name }
	cases := []struct {
		name    string
		phase   infravirtrigaudiov1beta1.MigrationPhase
		mutate  func(*corev1.PersistentVolumeClaim)
		mounted bool
	}{
		{"bound", infravirtrigaudiov1beta1.MigrationPhaseExporting, func(*corev1.PersistentVolumeClaim) {}, true},
		{"pending WFFC while validating", infravirtrigaudiov1beta1.MigrationPhaseValidating, func(p *corev1.PersistentVolumeClaim) {
			p.Status.Phase = corev1.ClaimPending
		}, true},
		{"pending WFFC after validating", infravirtrigaudiov1beta1.MigrationPhaseExporting, func(p *corev1.PersistentVolumeClaim) {
			p.Status.Phase = corev1.ClaimPending
		}, false},
		{"pending Immediate while validating", infravirtrigaudiov1beta1.MigrationPhaseValidating, func(p *corev1.PersistentVolumeClaim) {
			p.Status.Phase = corev1.ClaimPending
			p.Spec.StorageClassName = sc(msImmediate)
		}, false},
		{"pending, StorageClass missing", infravirtrigaudiov1beta1.MigrationPhaseValidating, func(p *corev1.PersistentVolumeClaim) {
			p.Status.Phase = corev1.ClaimPending
			p.Spec.StorageClassName = sc("gone")
		}, false},
		{"pending, no StorageClass (static binding)", infravirtrigaudiov1beta1.MigrationPhaseValidating, func(p *corev1.PersistentVolumeClaim) {
			p.Status.Phase = corev1.ClaimPending
			p.Spec.StorageClassName = sc("")
		}, false},
		{"lost", infravirtrigaudiov1beta1.MigrationPhaseExporting, func(p *corev1.PersistentVolumeClaim) {
			p.Status.Phase = corev1.ClaimLost
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, pvc, objs := msWorld()
			m.Status.Phase = tc.phase
			tc.mutate(pvc)
			r := msReconciler(t, append(objs, m, pvc)...)
			if tc.mounted {
				assert.Contains(t, msMounts(t, r, msSource), msPVC)
			} else {
				assert.Empty(t, msMounts(t, r, msSource))
			}
		})
	}

	t.Run("deleting", func(t *testing.T) {
		m, pvc, objs := msWorld()
		pvc.Finalizers = []string{"kubernetes.io/pvc-protection"}
		r := msReconciler(t, append(objs, m, pvc)...)
		require.NoError(t, r.Delete(context.Background(), pvc))
		assert.Empty(t, msMounts(t, r, msSource), "a Terminating PVC is never mounted (issue #184)")
	})

	t.Run("pending, StorageClass unreadable while validating", func(t *testing.T) {
		m, pvc, objs := msWorld()
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
		pvc.Status.Phase = corev1.ClaimPending
		r := msReconciler(t, append(objs, m, pvc)...)
		r.APIReader = forbiddenStorageClassReader{Reader: r.Client}
		assert.Contains(t, msMounts(t, r, msSource), msPVC, "without StorageClass access the pre-R15 behaviour applies during the bounded mount wait")
	})
}

// forbiddenStorageClassReader refuses StorageClass reads like a
// namespace-scoped install's Role does.
type forbiddenStorageClassReader struct{ client.Reader }

func (f forbiddenStorageClassReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*storagev1.StorageClass); ok {
		return apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, key.Name, stderrors.New("denied"))
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// TestDiscoverMigrationStorage_SortedAndMerged: several migrations render in
// PVC-name order (a stable pod template), a shared named PVC is mounted once,
// read-write when any migration exports into it through the Provider.
func TestDiscoverMigrationStorage_SortedAndMerged(t *testing.T) {
	_, _, objs := msWorld()
	b := msMigration(msNS, "b", "b-uid", "b-storage", "")
	a := msMigration(msNS, "a", "a-uid", "a-storage", "")
	shared1 := msMigration(msNS, "s1", "s1-uid", "shared", "shared")
	shared2 := msMigration(msNS, "s2", "s2-uid", "shared", "shared")
	shared2.Spec.Target.ProviderRef.Name = msSource // s2 imports into the source Provider
	r := msReconciler(t, append(objs, b, a, shared1, shared2,
		msPVCObj(msNS, "b-storage", true, b), msPVCObj(msNS, "a-storage", true, a), msPVCObj(msNS, "shared", true, nil))...)

	mounts, err := r.discoverMigrationStorage(context.Background(), msProvider(msNS, msSource))
	require.NoError(t, err)
	var names []string
	for _, m := range mounts {
		names = append(names, m.pvcName)
		assert.False(t, m.readOnly, "%s: the source Provider exports into every one", m.pvcName)
	}
	assert.Equal(t, []string{"a-storage", "b-storage", "shared"}, names)
}

// TestDiscoverMigrationStorage_ReadErrorIsReturned: a failed read is
// returned, so the caller keeps the Deployment instead of unmounting the
// storage of migrations in flight.
func TestDiscoverMigrationStorage_ReadErrorIsReturned(t *testing.T) {
	m, pvc, objs := msWorld()
	r := msReconciler(t, append(objs, m, pvc)...)
	r.Client = failingPVCGetClient{Client: r.Client}
	_, err := r.discoverMigrationStorage(context.Background(), msProvider(msNS, msSource))
	require.ErrorIs(t, err, errMigrationStorageDiscovery)
}

// TestMigrationStorageDiscoveryFailed: a discovery read error does not
// downgrade a Provider whose Deployment exists; without a Deployment it is
// reported with its own reason and a constant message. Neither returns an
// error (the render is retried after a fixed delay).
func TestMigrationStorageDiscoveryFailed(t *testing.T) {
	ctx := context.Background()
	s := migrationTestScheme(t)
	require.NoError(t, appsv1.AddToScheme(s))
	readErr := fmt.Errorf("%w: list VMMigrations: boom", errMigrationStorageDiscovery)

	t.Run("deployment exists", func(t *testing.T) {
		p := msProvider(msNS, msSource)
		p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning}
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: msNS}}
		r := &ProviderReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(dep).Build(), Scheme: s}
		res, err := r.migrationStorageDiscoveryFailed(ctx, p, "dep", readErr)
		require.NoError(t, err)
		assert.Equal(t, migrationStorageDiscoveryRetry, res.RequeueAfter)
		assert.Equal(t, infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning, p.Status.Runtime.Phase, "not downgraded")
		assert.Empty(t, p.Status.Conditions)
	})
	t.Run("no deployment yet", func(t *testing.T) {
		p := msProvider(msNS, msSource)
		r := &ProviderReconciler{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
		_, err := r.migrationStorageDiscoveryFailed(ctx, p, "dep", readErr)
		require.NoError(t, err)
		require.Len(t, p.Status.Conditions, 1)
		assert.Equal(t, reasonMigrationStorageDiscoveryFailed, p.Status.Conditions[0].Reason)
		assert.Equal(t, migrationStorageDiscoveryFailedMessage, p.Status.Conditions[0].Message)
	})
}

// failingPVCGetClient fails every PVC Get with a non-NotFound error.
type failingPVCGetClient struct{ client.Client }

func (f failingPVCGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		return apierrors.NewServiceUnavailable("cache not synced")
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

// TestMigrationStorageVolumes renders read-only mounts for the target and
// names volumes "migration-<pvc>" when that is a valid DNS label.
func TestMigrationStorageVolumes(t *testing.T) {
	mounts := assignMigrationVolumeNames(context.Background(),
		[]migrationStorageMount{{pvcName: "a-storage"}, {pvcName: "b-storage", readOnly: true}})
	vols := migrationStorageVolumes(mounts)
	vms := migrationStorageVolumeMounts(mounts)
	require.Len(t, vols, 2)
	require.Len(t, vms, 2)
	assert.Equal(t, "migration-a-storage", vols[0].Name)
	assert.Equal(t, "a-storage", vols[0].PersistentVolumeClaim.ClaimName)
	assert.Equal(t, "/mnt/migration-storage/a-storage", vms[0].MountPath)
	assert.False(t, vms[0].ReadOnly)
	assert.True(t, vms[1].ReadOnly)
	assert.Equal(t, vols[1].Name, vms[1].Name)
}

// TestMigrationStorageVolumeName: a PVC name that does not make a valid
// volume name (too long, dots) gets a valid, distinct, stable one.
func TestMigrationStorageVolumeName(t *testing.T) {
	long := strings.Repeat("a", 60) + "-storage"
	for _, name := range []string{long, long + "x", "my.pvc.name", "short"} {
		v := migrationStorageVolumeName(name)
		assert.Empty(t, validation.IsDNS1123Label(v), "%q -> %q must be a DNS-1123 label", name, v)
		assert.Equal(t, v, migrationStorageVolumeName(name), "stable")
	}
	assert.Equal(t, "migration-short", migrationStorageVolumeName("short"))
	assert.NotEqual(t, migrationStorageVolumeName(long), migrationStorageVolumeName(long+"x"))
}

// TestAssignMigrationVolumeNames_Collision: a PVC literally named like
// another PVC's hashed volume name never yields two volumes of one name (an
// invalid Deployment): the second one gets the long hash-only name.
func TestAssignMigrationVolumeNames_Collision(t *testing.T) {
	dotted := "x.y"
	hashed := migrationStorageVolumeName(dotted) // migration-x-y-<16 hex>
	literal := strings.TrimPrefix(hashed, migrationStorageVolumePrefix)
	require.Equal(t, hashed, migrationStorageVolumeName(literal), "the literal PVC name maps to the same volume name")

	mounts := assignMigrationVolumeNames(context.Background(), []migrationStorageMount{{pvcName: literal}, {pvcName: dotted}})
	require.Len(t, mounts, 2)
	seen := map[string]bool{}
	for _, m := range mounts {
		assert.Empty(t, validation.IsDNS1123Label(m.volumeName), m.volumeName)
		assert.False(t, seen[m.volumeName], "duplicate volume name %s", m.volumeName)
		seen[m.volumeName] = true
	}
	vols := migrationStorageVolumes(mounts)
	vms := migrationStorageVolumeMounts(mounts)
	for i := range vols {
		assert.Equal(t, vols[i].Name, vms[i].Name)
	}

	// A mount whose fallback name is taken too is left out rather than
	// rendered: here a PVC is literally named like x.y's hash-only fallback.
	sum := sha256.Sum256([]byte(dotted))
	fallbackLiteral := hex.EncodeToString(sum[:])[:validation.DNS1123LabelMaxLength-len(migrationStorageVolumePrefix)]
	all := []migrationStorageMount{{pvcName: fallbackLiteral}, {pvcName: literal}, {pvcName: dotted}}
	got := assignMigrationVolumeNames(context.Background(), all)
	require.Len(t, got, 2)
	for _, m := range got {
		assert.NotEqual(t, dotted, m.pvcName, "the PVC with no free volume name is not mounted")
	}
}

// TestMigrationPVCReferenceError covers the reasons the VMMigration
// controller reports when it validates the storage.
func TestMigrationPVCReferenceError(t *testing.T) {
	m := msMigration(msNS, msMig, msMigUID, msPVC, "")
	own := msOwnPVC(m)
	assert.NoError(t, migrationPVCReferenceError(own, m))
	assert.ErrorIs(t, migrationPVCReferenceError(msPVCObj(msNS, msPVC, false, m), m), errMigrationPVCNotLabelled)
	unowned := msPVCObj(msNS, msPVC, true, nil)
	unowned.UID = "uid-someone-else"
	assert.ErrorIs(t, migrationPVCReferenceError(unowned, m), errMigrationPVCNotOwned)
	forged := msPVCObj(msNS, msPVC, true, m)
	forged.UID = "uid-squatter"
	assert.ErrorIs(t, migrationPVCReferenceError(forged, m), errMigrationPVCNotOwned, "a forged ownerReference proves nothing")

	legacy := msMigration(msNS, msMig, msMigUID, msPVC, "") // older manager: no UID recorded
	assert.NoError(t, migrationPVCReferenceError(msPVCObj(msNS, msPVC, true, legacy), legacy))
	fresh := msMigration(msNS, msMig, msMigUID, "", "") // nothing recorded yet: nothing is legacy
	assert.ErrorIs(t, migrationPVCReferenceError(msPVCObj(msNS, msPVC, true, fresh), fresh), errMigrationPVCNotOwned)

	named := msMigration(msNS, msMig, msMigUID, "shared", "shared")
	assert.NoError(t, migrationPVCReferenceError(msPVCObj(msNS, "shared", true, nil), named))
	assert.ErrorIs(t, migrationPVCReferenceError(msPVCObj(msNS, "x", true, nil), named), errMigrationPVCNameMismatch)
	other := msMigration(msNS, "other", "other-uid", "", "")
	assert.ErrorIs(t, migrationPVCReferenceError(msPVCObj(msNS, "shared", true, other), named), errMigrationPVCOtherMigration)
}

// msRequestNames returns the Provider names of reqs (all in msNS).
func msRequestNames(t *testing.T, reqs []reconcile.Request) []string {
	t.Helper()
	var names []string
	for _, req := range reqs {
		assert.Equal(t, msNS, req.Namespace)
		names = append(names, req.Name)
	}
	return names
}

// TestProvidersForMigrationPVC: an event of a labelled or referenced PVC
// re-renders every Provider in its namespace (one that mounted it earlier may
// no longer be traceable through its migration); any other PVC maps to
// nothing.
func TestProvidersForMigrationPVC(t *testing.T) {
	m, pvc, objs := msWorld()
	unlabelledRef := msMigration(msNS, "named", "named-uid", "plain-ref", "plain-ref")
	r := msReconciler(t, append(objs, m, pvc, unlabelledRef)...)
	ctx := context.Background()
	all := []string{msSource, msTarget, msBystander}

	assert.ElementsMatch(t, all, msRequestNames(t, r.providersForMigrationPVC(ctx, pvc)))
	assert.ElementsMatch(t, all, msRequestNames(t, r.providersForMigrationPVC(ctx, msPVCObj(msNS, "rogue", true, nil))),
		"a labelled PVC nothing references may still be mounted from before")
	assert.ElementsMatch(t, all, msRequestNames(t, r.providersForMigrationPVC(ctx, msPVCObj(msNS, "plain-ref", false, nil))),
		"an unlabelled PVC a migration records")
	assert.Empty(t, r.providersForMigrationPVC(ctx, msPVCObj(msNS, "plain", false, nil)))
	assert.Empty(t, r.providersForMigrationPVC(ctx, msPVCObj("team-b", msPVC, true, nil)), "no Provider in that namespace")
}

// TestProvidersForMigration: an in-flight migration maps to its
// same-namespace source and target Providers, once each; a deleted or
// finished migration, or one whose source VM is gone, re-renders every
// Provider in its namespace, so the source never keeps a stale mount.
func TestProvidersForMigration(t *testing.T) {
	ctx := context.Background()
	all := []string{msSource, msTarget, msBystander}

	m, _, objs := msWorld()
	r := msReconciler(t, append(objs, m)...)
	assert.ElementsMatch(t, []string{msSource, msTarget}, msRequestNames(t, r.providersForMigration(ctx, m)))

	same := m.DeepCopy()
	same.Spec.Target.ProviderRef.Name = msSource
	assert.ElementsMatch(t, []string{msSource}, msRequestNames(t, r.providersForMigration(ctx, same)))

	ready := m.DeepCopy()
	ready.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseReady
	assert.ElementsMatch(t, all, msRequestNames(t, r.providersForMigration(ctx, ready)), "no longer in flight")

	t.Run("source VM gone", func(t *testing.T) {
		m, _, objs := msWorld()
		var noVM []client.Object
		for _, o := range objs {
			if _, isVM := o.(*infravirtrigaudiov1beta1.VirtualMachine); !isVM {
				noVM = append(noVM, o)
			}
		}
		r := msReconciler(t, append(noVM, m)...)
		assert.ElementsMatch(t, all, msRequestNames(t, r.providersForMigration(ctx, m)),
			"the source Provider can no longer be found through the VM, so every Provider re-renders")
	})
	t.Run("migration deleted", func(t *testing.T) {
		m, _, objs := msWorld()
		r := msReconciler(t, objs...) // not in the cache any more
		assert.ElementsMatch(t, all, msRequestNames(t, r.providersForMigration(ctx, m)))
	})
}

// TestMigrationStorageChanged: progress writes do not re-drive Providers;
// recording the PVC, finishing and deleting do.
func TestMigrationStorageChanged(t *testing.T) {
	p := migrationStorageChanged()
	m, _, _ := msWorld()

	progressed := m.DeepCopy()
	progressed.Status.Message = "50%"
	progressed.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseImporting
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: m, ObjectNew: progressed}))

	ready := m.DeepCopy()
	ready.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseReady
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: m, ObjectNew: ready}))

	fresh := m.DeepCopy()
	fresh.Status.StoragePVCName = ""
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: fresh, ObjectNew: m}))
	assert.False(t, p.Create(event.CreateEvent{Object: fresh}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: m}))

	pp := migrationPVCChanged()
	_, pvc, _ := msWorld()
	resized := pvc.DeepCopy()
	resized.Annotations = map[string]string{"x": "y"}
	assert.False(t, pp.Update(event.UpdateEvent{ObjectOld: pvc, ObjectNew: resized}))
	pending := pvc.DeepCopy()
	pending.Status.Phase = corev1.ClaimPending
	assert.True(t, pp.Update(event.UpdateEvent{ObjectOld: pending, ObjectNew: pvc}))
}

// TestMigrationMountWaitStart: the mount budget starts at the latest of the
// PVC's creation, the migration's start and its last retry, so an older PVC
// named in spec.storage.pvc.name is not timed out at once.
func TestMigrationMountWaitStart(t *testing.T) {
	now := time.Now()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))}}
	m := &infravirtrigaudiov1beta1.VMMigration{}
	assert.Equal(t, pvc.CreationTimestamp.Time, migrationMountWaitStart(m, pvc))

	m.Status.StartTime = &metav1.Time{Time: now.Add(-time.Minute)}
	assert.Equal(t, m.Status.StartTime.Time, migrationMountWaitStart(m, pvc))

	m.Status.LastRetryTime = &metav1.Time{Time: now}
	assert.Equal(t, now, migrationMountWaitStart(m, pvc))

	young := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now.Add(time.Second))}}
	assert.Equal(t, young.CreationTimestamp.Time, migrationMountWaitStart(m, young))
}

// TestEnsureMigrationPVC_RefusesUnusablePVCs: the VMMigration controller
// fails fast on a PVC the provider pods would never mount.
func TestEnsureMigrationPVC_RefusesUnusablePVCs(t *testing.T) {
	ctx := context.Background()
	s := migrationTestScheme(t)

	t.Run("named PVC without the label", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "", "db-data")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(m, msPVCObj(msNS, "db-data", false, nil)).Build()
		r := NewVMMigrationReconciler(c, c, s, nil, record.NewFakeRecorder(10), false)
		_, err := r.ensureMigrationPVC(ctx, m)
		require.ErrorIs(t, err, errMigrationPVCNotLabelled)
	})
	t.Run("named PVC, labelled", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "", "shared")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(m, msPVCObj(msNS, "shared", true, nil)).Build()
		r := NewVMMigrationReconciler(c, c, s, nil, record.NewFakeRecorder(10), false)
		name, err := r.ensureMigrationPVC(ctx, m)
		require.NoError(t, err)
		assert.Equal(t, "shared", name)
	})
	t.Run("auto name taken by a foreign PVC", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "", "")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(m, msPVCObj(msNS, msPVC, true, nil)).Build()
		r := NewVMMigrationReconciler(c, c, s, nil, record.NewFakeRecorder(10), false)
		_, err := r.ensureMigrationPVC(ctx, m)
		require.ErrorIs(t, err, errMigrationPVCNotOwned)
	})
	t.Run("auto name squatted with a forged controller ownerReference", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "", "")
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(m, msPVCObj(msNS, msPVC, true, m)).
			WithStatusSubresource(&infravirtrigaudiov1beta1.VMMigration{}).Build()
		r := NewVMMigrationReconciler(c, c, s, nil, record.NewFakeRecorder(10), false)
		_, err := r.ensureMigrationPVC(ctx, m)
		require.ErrorIs(t, err, errMigrationPVCNotOwned)
		assert.Empty(t, m.Status.StoragePVCUID, "nothing is recorded for a PVC the migration did not create")
	})
	t.Run("creates the PVC and records the UID the API server answered", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "", "")
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(m).
			WithStatusSubresource(&infravirtrigaudiov1beta1.VMMigration{}).
			WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetUID("uid-created-by-apiserver")
				return cl.Create(ctx, obj, opts...)
			}}).Build()
		r := NewVMMigrationReconciler(c, c, s, nil, record.NewFakeRecorder(10), false)
		name, err := r.ensureMigrationPVC(ctx, m)
		require.NoError(t, err)
		assert.Equal(t, msPVC, name)
		got := &infravirtrigaudiov1beta1.VMMigration{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(m), got))
		assert.Equal(t, msPVC, got.Status.StoragePVCName)
		assert.Equal(t, "uid-created-by-apiserver", got.Status.StoragePVCUID)

		_, err = r.ensureMigrationPVC(ctx, got)
		require.NoError(t, err, "a later reconcile accepts that PVC")
	})
	t.Run("spec.storage.pvc.name changed after the record", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "shared", "elsewhere")
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(m, msPVCObj(msNS, "elsewhere", true, nil)).Build()
		r := NewVMMigrationReconciler(c, c, s, nil, record.NewFakeRecorder(10), false)
		_, err := r.ensureMigrationPVC(ctx, m)
		require.ErrorIs(t, err, errMigrationPVCSpecChanged)
		_, err = r.generateStorageURL(ctx, m, "export")
		require.ErrorIs(t, err, errMigrationPVCSpecChanged, "every later phase fails on it too")
	})
}

// TestHandleDeletion_DeletesOnlyTheMigrationsOwnPVC: deleting a VMMigration
// deletes the PVC it created, never a user's PVC named in
// spec.storage.pvc.name, which is left with a Normal event.
func TestHandleDeletion_DeletesOnlyTheMigrationsOwnPVC(t *testing.T) {
	ctx := context.Background()
	s := migrationTestScheme(t)
	run := func(t *testing.T, m *infravirtrigaudiov1beta1.VMMigration, pvc *corev1.PersistentVolumeClaim) (bool, *record.FakeRecorder) {
		t.Helper()
		m.Finalizers = []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseReady
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(m, pvc).
			WithStatusSubresource(&infravirtrigaudiov1beta1.VMMigration{}).Build()
		require.NoError(t, c.Delete(ctx, m))
		got := &infravirtrigaudiov1beta1.VMMigration{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(m), got))
		rec := record.NewFakeRecorder(10)
		r := NewVMMigrationReconciler(c, c, s, nil, rec, false)
		_, err := r.handleDeletion(ctx, got)
		require.NoError(t, err)
		err = c.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{})
		if err != nil {
			require.True(t, apierrors.IsNotFound(err))
			return false, rec
		}
		return true, rec
	}

	t.Run("controller-created PVC is deleted", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, msPVC, "")
		exists, rec := run(t, m, msOwnPVC(m))
		assert.False(t, exists)
		assert.Empty(t, rec.Events)
	})
	t.Run("legacy controller-created PVC (no UID recorded) is deleted", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, msPVC, "")
		exists, _ := run(t, m, msPVCObj(msNS, msPVC, true, m))
		assert.False(t, exists)
	})
	t.Run("squat with a forged ownerReference survives", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, msPVC, "")
		m.Status.StoragePVCUID = "uid-of-the-pvc-it-created"
		exists, rec := run(t, m, msPVCObj(msNS, msPVC, true, m))
		assert.True(t, exists, "only the PVC with the recorded UID is deleted")
		require.Len(t, rec.Events, 1)
		assert.Contains(t, <-rec.Events, eventReasonMigrationPVCRetained)
	})
	t.Run("user-named PVC survives with a Normal event", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "shared", "shared")
		exists, rec := run(t, m, msPVCObj(msNS, "shared", true, nil))
		assert.True(t, exists, "VirtRigaud never deletes a PVC it did not create")
		require.Len(t, rec.Events, 1)
		ev := <-rec.Events
		assert.Contains(t, ev, corev1.EventTypeNormal+" "+eventReasonMigrationPVCRetained)
		assert.Contains(t, ev, "shared")
	})
	t.Run("PVC controlled by another migration survives", func(t *testing.T) {
		m := msMigration(msNS, msMig, msMigUID, "other-storage", "other-storage")
		other := msMigration(msNS, "other", "other-uid", "", "")
		exists, _ := run(t, m, msPVCObj(msNS, "other-storage", true, other))
		assert.True(t, exists)
	})
}
