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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These envtest specs run the VMMigration controller in a real manager
// against a real apiserver (rc2 R7): the LandedDiskCleanup outcomes are real
// status writes on the vmmigrations/status subresource, written once (no
// reconcile storm), and the finalizer keeps a deleted VMMigration while the
// target provider cannot delete the landed disk.
var _ = Describe("VMMigration landed-disk cleanup (envtest)", func() {
	startLandedManager := func(ns string, prov *landedProvider) *record.FakeRecorder {
		rec := record.NewFakeRecorder(200)
		startHoldTestManager(func(mgr ctrl.Manager) {
			r := NewVMMigrationReconciler(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme(), nil, rec, false)
			r.providerInstanceFn = onlyProviderIn(ns, prov)
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
		return rec
	}

	// createImportedMigration creates, in a fresh namespace, the target
	// Provider and a finalized pvc VMMigration in phase whose import was sent
	// (LandedDiskCleanup=Unknown) and returned landedTestPath, with
	// cleanupPolicy policy and no retry policy (a Failed one failed for good).
	createImportedMigration := func(phase infravirtrigaudiov1beta1.MigrationPhase, policy string) *infravirtrigaudiov1beta1.VMMigration {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "landed-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		target := holdTestProvider(ns)
		Expect(k8sClient.Create(ctx, target)).To(Succeed())

		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns, Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source:  infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target:  infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"}},
				Options: &infravirtrigaudiov1beta1.MigrationOptions{CleanupPolicy: policy},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		m.Status.Phase = phase
		markLandedDiskMayExist(m, migrationTargetVMKey(m), target)
		m.Status.DiskInfo = &infravirtrigaudiov1beta1.MigrationDiskInfo{TargetPath: landedTestPath}
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		return m
	}

	landedConditionOf := func(g Gomega, m *infravirtrigaudiov1beta1.VMMigration) *metav1.Condition {
		got := &infravirtrigaudiov1beta1.VMMigration{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got)).To(Succeed())
		return meta.FindStatusCondition(got.Status.Conditions, migrationConditionLandedDiskCleanup)
	}

	gone := func(m *infravirtrigaudiov1beta1.VMMigration) func() bool {
		return func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), &infravirtrigaudiov1beta1.VMMigration{}))
		}
	}

	countEvents := func(rec *record.FakeRecorder, reason string) int {
		n := 0
		for _, e := range drainRecordedEvents(rec) {
			Expect(e).NotTo(ContainSubstring(landedTestPath))
			if strings.Contains(e, " "+reason+" ") {
				n++
			}
		}
		return n
	}

	It("deletes the landed disk of a migration that failed for good with cleanupPolicy Always, once", func() {
		prov := newLandedProvider()
		m := createImportedMigration(infravirtrigaudiov1beta1.MigrationPhaseFailed, infravirtrigaudiov1beta1.CleanupPolicyAlways)
		rec := startLandedManager(m.Namespace, prov)

		Eventually(func(g Gomega) {
			c := landedConditionOf(g, m)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(c.Reason).To(Equal(migrationReasonLandedDiskDeleted))
			g.Expect(c.Message).To(Equal(migrationLandedDiskDeletedMessage))
		}, "20s", "200ms").Should(Succeed())
		Consistently(func() int32 { return prov.calls.Load() }, "2s", "250ms").Should(Equal(int32(1)), "no second RPC")
		Expect(prov.lastRequest().TargetVM).To(Equal(contracts.ObjectIdentity{Namespace: m.Namespace, Name: "web"}))
		Expect(prov.lastRequest().Path).To(Equal(landedTestPath))
		Expect(countEvents(rec, migrationReasonLandedDiskDeleted)).To(Equal(1))
	})

	It("keeps the landed disk while the migration's target VirtualMachine exists", func() {
		prov := newLandedProvider()
		m := createImportedMigration(infravirtrigaudiov1beta1.MigrationPhaseFailed, infravirtrigaudiov1beta1.CleanupPolicyAlways)
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: m.Namespace},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Path: landedTestPath, Source: "migration",
					MigrationRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: m.Name}},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		stored := &infravirtrigaudiov1beta1.VMMigration{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), stored)).To(Succeed())
		stored.Status.TargetVMUID = string(vm.UID)
		Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())
		startLandedManager(m.Namespace, prov)

		Eventually(func(g Gomega) {
			c := landedConditionOf(g, m)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(c.Reason).To(Equal(migrationReasonLandedDiskInUse))
		}, "20s", "200ms").Should(Succeed())
		Expect(prov.calls.Load()).To(BeZero(), "the target VM's disk is never sent for deletion")
	})

	It("records a provider that predates the RPC as left behind, with one Warning", func() {
		prov := newLandedProvider()
		prov.supported = false
		m := createImportedMigration(infravirtrigaudiov1beta1.MigrationPhaseFailed, infravirtrigaudiov1beta1.CleanupPolicyAlways)
		rec := startLandedManager(m.Namespace, prov)

		Eventually(func(g Gomega) {
			c := landedConditionOf(g, m)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(migrationReasonLandedDiskLeftBehind))
			g.Expect(c.Message).To(Equal(landedDiskLeftUnsupported + migrationLandedDiskLeftBehindSuffix))
		}, "20s", "200ms").Should(Succeed())
		Expect(prov.calls.Load()).To(BeZero())
		Consistently(func() int { return countEvents(rec, migrationReasonLandedDiskLeftBehind) }, "1s", "250ms").
			Should(BeNumerically("<=", 1))
	})

	It("holds a deleted VMMigration while its landed disk cannot be deleted, and releases it once it is", func() {
		prov := newLandedProvider()
		prov.answer("", contracts.NewRetryableError("deleteLandedDisk: unavailable", nil))
		m := createImportedMigration(infravirtrigaudiov1beta1.MigrationPhaseFailed, infravirtrigaudiov1beta1.CleanupPolicyOnSuccess)
		rec := startLandedManager(m.Namespace, prov)

		By("OnSuccess keeps it while the failed VMMigration exists")
		Consistently(func(g Gomega) {
			c := landedConditionOf(g, m)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionUnknown))
		}, "2s", "250ms").Should(Succeed())
		Expect(prov.calls.Load()).To(BeZero())

		By("a deleted VMMigration is held by its finalizer")
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		Eventually(func(g Gomega) {
			c := landedConditionOf(g, m)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal(migrationReasonLandedDiskCleanupPending))
			g.Expect(c.Message).To(Equal(migrationLandedDiskCleanupPendingMessage))
		}, "20s", "200ms").Should(Succeed())
		Consistently(func() bool { return gone(m)() }, "2s", "250ms").Should(BeFalse())

		By("the provider recovers: the landed disk is deleted and the VMMigration released")
		prov.answer(contracts.LandedDiskDeleted, nil)
		Eventually(gone(m), "45s", "250ms").Should(BeTrue())
		Expect(countEvents(rec, migrationReasonLandedDiskCleanupPending)).To(Equal(1), "one Warning for the whole hold")
	})
})
