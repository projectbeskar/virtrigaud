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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// These envtest specs run the VMMigration controller in a real manager
// against a real apiserver (ADR-0011 D4): the staged-disk hold is a real
// status write on the vmmigrations/status subresource, the finalizer keeps a
// deleted VMMigration while the target provider cannot delete the staged
// disk, a recovered provider releases it, and force-delete releases it at
// once.
var _ = Describe("VMMigration staged-disk cleanup (envtest)", func() {
	// startStagingManager runs the VMMigration controller, resolving only ns's
	// Providers to prov.
	startStagingManager := func(ns string, prov *stagingProvider) *record.FakeRecorder {
		rec := record.NewFakeRecorder(200)
		startHoldTestManager(func(mgr ctrl.Manager) {
			r := NewVMMigrationReconciler(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme(), nil, rec, false)
			r.providerInstanceFn = onlyProviderIn(ns, prov)
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
		return rec
	}

	// createReadyS3Migration creates, in a fresh namespace, the target
	// Provider, the S3 credentials Secret and a finalized s3 VMMigration in
	// phase Ready whose export was sent (StagingCleanup=Unknown).
	createReadyS3Migration := func() *infravirtrigaudiov1beta1.VMMigration {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "staging-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns := nsObj.Name
		target := holdTestProvider(ns)
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: stagingTestSecret, Namespace: ns},
			Data: map[string][]byte{
				storagemigration.CredKeyAccessKeyID:     []byte("AKID"),
				storagemigration.CredKeySecretAccessKey: []byte(stagingTestKeyValue),
			},
		})).To(Succeed())

		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns, Finalizers: []string{vmMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"}},
				Storage: &infravirtrigaudiov1beta1.MigrationStorage{
					Type: storagemigration.BackendS3,
					S3: &infravirtrigaudiov1beta1.S3StorageConfig{
						Bucket:               stagingTestBucket,
						Endpoint:             stagingTestEndpoint,
						CredentialsSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: stagingTestSecret},
						UsePathStyle:         true,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseReady
		digest, err := stagingDigest(m, target)
		Expect(err).NotTo(HaveOccurred())
		markStagedDiskMayExist(m, digest)
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		return m
	}

	stagingConditionOf := func(g Gomega, m *infravirtrigaudiov1beta1.VMMigration) *metav1.Condition {
		got := &infravirtrigaudiov1beta1.VMMigration{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got)).To(Succeed())
		return meta.FindStatusCondition(got.Status.Conditions, migrationConditionStagingCleanup)
	}

	gone := func(m *infravirtrigaudiov1beta1.VMMigration) func() bool {
		return func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), &infravirtrigaudiov1beta1.VMMigration{}))
		}
	}

	It("holds a deleted VMMigration while its staged disk cannot be deleted, and releases it once it is", func() {
		prov := newStagingProvider()
		prov.answer("", contracts.NewRetryableError("deleteStagedDisk: unavailable", nil))
		m := createReadyS3Migration()
		rec := startStagingManager(m.Namespace, prov)

		By("the Ready cleanup records a held delete")
		Eventually(func(g Gomega) {
			c := stagingConditionOf(g, m)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal(migrationReasonStagingCleanupPending))
			g.Expect(c.Message).To(Equal(migrationStagingCleanupPendingMessage))
			g.Expect(c.Message).NotTo(ContainSubstring(stagingTestBucket))
		}, "20s", "200ms").Should(Succeed())
		Expect(prov.calls.Load()).To(BeNumerically(">=", 1))

		By("a deleted VMMigration is kept by its finalizer")
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		Consistently(func(g Gomega) {
			got := &infravirtrigaudiov1beta1.VMMigration{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got)).To(Succeed())
			g.Expect(got.Finalizers).To(ContainElement(vmMigrationFinalizer))
		}, "3s", "250ms").Should(Succeed())

		By("the provider recovers: the staged disk is deleted and the VMMigration released")
		prov.answer(contracts.StagedDiskDeleted, nil)
		Eventually(gone(m), "45s", "250ms").Should(BeTrue())
		Expect(prov.lastRequest().Migration.Namespace).To(Equal(m.Namespace))
		Expect(prov.lastRequest().Migration.Name).To(Equal("mig"))

		var warnings int
		for _, e := range drainRecordedEvents(rec) {
			Expect(e).NotTo(ContainSubstring(stagingTestBucket))
			Expect(e).NotTo(ContainSubstring(stagingTestKeyValue))
			if strings.Contains(e, " "+migrationReasonStagingCleanupPending+" ") {
				warnings++
			}
		}
		Expect(warnings).To(Equal(1), "one Warning for the whole hold")
	})

	It("releases a held VMMigration at once on force-delete, with a StagedDiskLeftBehind Warning", func() {
		prov := newStagingProvider()
		prov.answer("", contracts.NewRetryableError("deleteStagedDisk: unavailable", nil))
		m := createReadyS3Migration()
		rec := startStagingManager(m.Namespace, prov)

		Eventually(func(g Gomega) {
			c := stagingConditionOf(g, m)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(migrationReasonStagingCleanupPending))
		}, "20s", "200ms").Should(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		Consistently(func() bool { return gone(m)() }, "2s", "250ms").Should(BeFalse())

		By("force-delete releases it without waiting for the backoff")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			got := &infravirtrigaudiov1beta1.VMMigration{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got); err != nil {
				return err
			}
			got.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			return k8sClient.Update(ctx, got)
		})).To(Succeed())
		Eventually(gone(m), "10s", "200ms").Should(BeTrue())

		var left int
		for _, e := range drainRecordedEvents(rec) {
			if strings.Contains(e, " "+migrationReasonStagedDiskLeftBehind+" ") {
				left++
				Expect(e).To(ContainSubstring(forceDeleteAnnotation))
				Expect(e).NotTo(ContainSubstring(stagingTestBucket))
			}
		}
		Expect(left).To(Equal(1))
	})
})
