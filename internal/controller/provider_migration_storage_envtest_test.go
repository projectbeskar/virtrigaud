/*
Copyright 2026.

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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// These envtest specs pin the migration-storage mount scope (R15) against a
// real apiserver: a provider pod mounts only the PVC a live VMMigration
// references, read-write on the source and read-only on the target; a
// labelled PVC nothing references, a finished migration's PVC, a foreign
// namespace's PVC and an unbindable PVC are never mounted.

const msEnvTarget = "prov-2"

// msEnvProviders creates the source Provider prov-1 and the target Provider
// prov-2 in ns, plus the source VM src-vm on prov-1.
func msEnvProviders(ns string) {
	inUseEnvProvider(ns)
	tgt := holdTestProvider(ns)
	tgt.Name = msEnvTarget
	tgt.Spec.Runtime.Service = &infravirtrigaudiov1beta1.ProviderServiceSpec{
		Port: 9443, TLS: &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false},
	}
	Expect(k8sClient.Create(ctx, tgt)).To(Succeed())
	inUseEnvVM(ns, "src-vm", ns)
}

// msEnvMigration creates a pvc VMMigration name in ns from src-vm to prov-2
// (named: the PVC spec.storage.pvc.name names, or "" for an auto-created one)
// and records pvcName and phase in its status.
func msEnvMigration(ns, name, named, pvcName string, phase infravirtrigaudiov1beta1.MigrationPhase) *infravirtrigaudiov1beta1.VMMigration {
	m := &infravirtrigaudiov1beta1.VMMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
			Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: msEnvTarget}},
			Storage: &infravirtrigaudiov1beta1.MigrationStorage{
				Type: storagemigration.BackendPVC,
				PVC:  &infravirtrigaudiov1beta1.PVCStorageConfig{Name: named, StorageClassName: "any", Size: "1Gi"},
			},
		},
	}
	Expect(k8sClient.Create(ctx, m)).To(Succeed())
	msEnvSetStatus(m, pvcName, phase)
	return m
}

// msEnvSetStatus writes pvcName and phase to m's status.
func msEnvSetStatus(m *infravirtrigaudiov1beta1.VMMigration, pvcName string, phase infravirtrigaudiov1beta1.MigrationPhase) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
			return err
		}
		m.Status.StoragePVCName = pvcName
		m.Status.Phase = phase
		return k8sClient.Status().Update(ctx, m)
	})).To(Succeed())
}

// msEnvPVC creates the PVC name in ns on storageClass, labelled as migration
// storage when labelled, controlled by owner when set, in the given phase.
func msEnvPVC(ns, name, storageClass string, labelled bool, owner *infravirtrigaudiov1beta1.VMMigration, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClass,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
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
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	pvc.Status.Phase = phase
	Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
	return pvc
}

// msEnvDeploymentMounts returns the migration PVC mounts of Provider name's
// Deployment in ns: claim name -> read-only.
func msEnvDeploymentMounts(g Gomega, ns, name string) map[string]bool {
	dep := &appsv1.Deployment{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: fmt.Sprintf("virtrigaud-provider-%s-%s", ns, name)}, dep)).To(Succeed())
	claimOf := map[string]string{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claimOf[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}
	out := map[string]bool{}
	for _, m := range dep.Spec.Template.Spec.Containers[0].VolumeMounts {
		if claim, ok := claimOf[m.Name]; ok {
			g.Expect(m.MountPath).To(Equal(migrationStorageMountRoot + "/" + claim))
			out[claim] = m.ReadOnly
		}
	}
	g.Expect(out).To(HaveLen(len(claimOf)), "every PVC volume is mounted")
	return out
}

var _ = Describe("Provider migration-storage mount scope (envtest)", func() {
	It("mounts only the PVC a live VMMigration references, on its source (rw) and target (ro), and drops it when the migration is Ready", func() {
		ns := inUseTestNamespace("ms-scope-")
		msEnvProviders(ns)
		msEnvPVC(ns, "rogue", "any", true, nil, corev1.ClaimBound)
		startProviderInUseManager(ns)

		By("a labelled PVC nothing references is not mounted")
		Eventually(func(g Gomega) {
			g.Expect(msEnvDeploymentMounts(g, ns, "prov-1")).To(BeEmpty())
			g.Expect(msEnvDeploymentMounts(g, ns, msEnvTarget)).To(BeEmpty())
		}, "20s", "100ms").Should(Succeed())

		By("the PVC an in-flight migration created and recorded is mounted")
		m := msEnvMigration(ns, "mig", "", "", infravirtrigaudiov1beta1.MigrationPhaseValidating)
		msEnvPVC(ns, "mig-storage", "any", true, m, corev1.ClaimBound)
		msEnvSetStatus(m, "mig-storage", infravirtrigaudiov1beta1.MigrationPhaseValidating)
		Eventually(func(g Gomega) {
			g.Expect(msEnvDeploymentMounts(g, ns, "prov-1")).To(Equal(map[string]bool{"mig-storage": false}))
			g.Expect(msEnvDeploymentMounts(g, ns, msEnvTarget)).To(Equal(map[string]bool{"mig-storage": true}))
		}, "20s", "100ms").Should(Succeed())

		By("a Ready migration's PVC is unmounted")
		msEnvSetStatus(m, "mig-storage", infravirtrigaudiov1beta1.MigrationPhaseReady)
		Eventually(func(g Gomega) {
			g.Expect(msEnvDeploymentMounts(g, ns, "prov-1")).To(BeEmpty())
			g.Expect(msEnvDeploymentMounts(g, ns, msEnvTarget)).To(BeEmpty())
		}, "20s", "100ms").Should(Succeed())
	})

	It("never mounts a foreign namespace's, a deleted migration's, an unowned or an unlabelled named PVC", func() {
		ns := inUseTestNamespace("ms-refuse-")
		other := inUseTestNamespace("ms-foreign-")
		msEnvProviders(ns)
		r := &ProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		mountsOf := func(name string) []migrationStorageMount {
			p := &infravirtrigaudiov1beta1.Provider{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, p)).To(Succeed())
			mounts, err := r.discoverMigrationStorage(ctx, p)
			Expect(err).NotTo(HaveOccurred())
			return mounts
		}

		By("a migration in another namespace targeting prov-2 mounts nothing into it")
		foreign := msEnvMigration(other, "mig", "", "", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
				return err
			}
			foreign.Spec.Target.ProviderRef.Namespace = ns
			return k8sClient.Update(ctx, foreign)
		})).To(Succeed())
		msEnvPVC(other, "mig-storage", "any", true, foreign, corev1.ClaimBound)
		msEnvSetStatus(foreign, "mig-storage", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		Expect(mountsOf(msEnvTarget)).To(BeEmpty())

		By("a PVC the migration did not create, under its auto name, is not mounted")
		squat := msEnvMigration(ns, "squat", "", "", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		msEnvPVC(ns, "squat-storage", "any", true, nil, corev1.ClaimBound)
		msEnvSetStatus(squat, "squat-storage", infravirtrigaudiov1beta1.MigrationPhaseExporting)

		By("a named PVC its owner did not label is not mounted")
		named := msEnvMigration(ns, "named", "db-data", "", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		msEnvPVC(ns, "db-data", "any", false, nil, corev1.ClaimBound)
		msEnvSetStatus(named, "db-data", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		Expect(mountsOf("prov-1")).To(BeEmpty())
		Expect(mountsOf(msEnvTarget)).To(BeEmpty())

		By("a deleted migration's PVC is not mounted")
		live := msEnvMigration(ns, "live", "", "", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		msEnvPVC(ns, "live-storage", "any", true, live, corev1.ClaimBound)
		msEnvSetStatus(live, "live-storage", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		Expect(mountsOf("prov-1")).To(Equal([]migrationStorageMount{{pvcName: "live-storage"}}))
		Expect(k8sClient.Delete(ctx, live)).To(Succeed())
		Expect(mountsOf("prov-1")).To(BeEmpty(), "the PVC outlives its migration until garbage collection")
	})

	It("mounts a Pending PVC only on a WaitForFirstConsumer StorageClass while the migration waits for it", func() {
		ns := inUseTestNamespace("ms-bind-")
		msEnvProviders(ns)
		wffc := storagev1.VolumeBindingWaitForFirstConsumer
		immediate := storagev1.VolumeBindingImmediate
		wffcSC := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{GenerateName: "ms-wffc-"}, Provisioner: "example.com/x", VolumeBindingMode: &wffc}
		immSC := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{GenerateName: "ms-imm-"}, Provisioner: "example.com/x", VolumeBindingMode: &immediate}
		Expect(k8sClient.Create(ctx, wffcSC)).To(Succeed())
		Expect(k8sClient.Create(ctx, immSC)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, wffcSC))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, immSC))).To(Succeed())
		})
		r := &ProviderReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme()}
		src := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, src)).To(Succeed())
		names := func() []string {
			mounts, err := r.discoverMigrationStorage(ctx, src)
			Expect(err).NotTo(HaveOccurred())
			var out []string
			for _, m := range mounts {
				out = append(out, m.pvcName)
			}
			return out
		}

		w := msEnvMigration(ns, "w", "", "", infravirtrigaudiov1beta1.MigrationPhaseValidating)
		msEnvPVC(ns, "w-storage", wffcSC.Name, true, w, corev1.ClaimPending)
		msEnvSetStatus(w, "w-storage", infravirtrigaudiov1beta1.MigrationPhaseValidating)
		i := msEnvMigration(ns, "i", "", "", infravirtrigaudiov1beta1.MigrationPhaseValidating)
		msEnvPVC(ns, "i-storage", immSC.Name, true, i, corev1.ClaimPending)
		msEnvSetStatus(i, "i-storage", infravirtrigaudiov1beta1.MigrationPhaseValidating)
		Expect(names()).To(Equal([]string{"w-storage"}), "only the WaitForFirstConsumer PVC, which a pod must bind")

		By("outside the mount wait an unbound PVC is not mounted, so it cannot wedge the rollout")
		msEnvSetStatus(w, "w-storage", infravirtrigaudiov1beta1.MigrationPhaseExporting)
		Expect(names()).To(BeEmpty())
	})
})
