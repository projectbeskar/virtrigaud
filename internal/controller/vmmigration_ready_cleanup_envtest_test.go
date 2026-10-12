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
	storagemigration "github.com/projectbeskar/virtrigaud/internal/storage/migration"
)

// These envtest specs run the VMMigration controller against a real apiserver
// for the Ready path's status writes (maintainer decision D1, ADR-0011): a
// migration an earlier release completed deletes nothing and is closed for
// good; one this manager completed deletes its staged disk and — with a real
// UID precondition — only the source VirtualMachine it exported.
var _ = Describe("VMMigration Ready cleanup (envtest)", func() {
	type readyCase struct {
		ns       string
		m        *infravirtrigaudiov1beta1.VMMigration
		sourceVM *infravirtrigaudiov1beta1.VirtualMachine
		prov     *stagingProvider
		rec      *record.FakeRecorder
	}

	// setup creates, in a fresh namespace, the target Provider, the S3
	// Secret, the source VM and a Ready s3 migration with
	// deleteAfterMigration whose export was sent; stamp marks it completed by
	// this manager; mutate adjusts its status before the controller starts.
	setup := func(stamp bool, mutate func(c *readyCase)) *readyCase {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "readyclean-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		c := &readyCase{ns: nsObj.Name, prov: newStagingProvider()}
		target := holdTestProvider(c.ns)
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: stagingTestSecret, Namespace: c.ns},
			Data: map[string][]byte{
				storagemigration.CredKeyAccessKeyID:     []byte("AKID"),
				storagemigration.CredKeySecretAccessKey: []byte(stagingTestKeyValue),
			},
		})).To(Succeed())
		c.sourceVM = holdTestVM(c.ns, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOff)

		c.m = &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: c.ns, Finalizers: []string{vmMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{
					VMRef:                infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"},
					DeleteAfterMigration: true,
				},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
					PowerOn: true},
				Storage: &infravirtrigaudiov1beta1.MigrationStorage{
					Type: storagemigration.BackendS3,
					S3: &infravirtrigaudiov1beta1.S3StorageConfig{
						Bucket: stagingTestBucket, Endpoint: stagingTestEndpoint, UsePathStyle: true,
						CredentialsSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: stagingTestSecret},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, c.m)).To(Succeed())
		c.m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseReady
		c.m.Status.SourceVMUID = string(c.sourceVM.UID)
		digest, err := stagingDigest(c.m, target)
		Expect(err).NotTo(HaveOccurred())
		markStagedDiskMayExist(c.m, digest)
		if stamp {
			markPostMigrationCleanupPending(c.m)
		}
		if mutate != nil {
			mutate(c)
		}
		Expect(k8sClient.Status().Update(ctx, c.m)).To(Succeed())

		c.rec = record.NewFakeRecorder(200)
		startHoldTestManager(func(mgr ctrl.Manager) {
			r := NewVMMigrationReconciler(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme(), nil, c.rec, false)
			r.providerInstanceFn = onlyProviderIn(c.ns, c.prov)
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
		return c
	}

	stored := func(g Gomega, m *infravirtrigaudiov1beta1.VMMigration) *infravirtrigaudiov1beta1.VMMigration {
		got := &infravirtrigaudiov1beta1.VMMigration{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got)).To(Succeed())
		return got
	}

	countEvents := func(rec *record.FakeRecorder, reason string) int {
		n := 0
		for _, e := range drainRecordedEvents(rec) {
			if strings.Contains(e, " "+reason+" ") {
				n++
			}
		}
		return n
	}

	It("closes a migration an earlier release completed without deleting its source VM", func() {
		c := setup(false, nil)
		Eventually(func(g Gomega) {
			got := stored(g, c.m)
			g.Expect(got.Status.StorageInfo).NotTo(BeNil())
			g.Expect(got.Status.StorageInfo.CleanedUp).To(BeTrue())
			cond := meta.FindStatusCondition(got.Status.Conditions, migrationConditionPostMigrationCleanup)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(migrationReasonSourceDeletionSkippedPreUpgrade))
		}, "20s", "200ms").Should(Succeed())
		Consistently(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(c.sourceVM), &infravirtrigaudiov1beta1.VirtualMachine{})
		}, "2s", "250ms").Should(Succeed(), "the source VM is never deleted")
		Expect(countEvents(c.rec, migrationReasonSourceDeletionSkippedPreUpgrade)).To(Equal(1))
	})

	It("deletes the staged disk and the exported source VM of a migration this manager completed", func() {
		c := setup(true, nil)
		Eventually(func(g Gomega) {
			got := stored(g, c.m)
			g.Expect(got.Status.StorageInfo.CleanedUp).To(BeTrue())
			post := meta.FindStatusCondition(got.Status.Conditions, migrationConditionPostMigrationCleanup)
			g.Expect(post).NotTo(BeNil())
			g.Expect(post.Reason).To(Equal(migrationReasonPostMigrationCleanupComplete))
			staging := meta.FindStatusCondition(got.Status.Conditions, migrationConditionStagingCleanup)
			g.Expect(staging).NotTo(BeNil())
			g.Expect(staging.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(staging.Reason).To(Equal(migrationReasonStagedDiskDeleted))
		}, "20s", "200ms").Should(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(c.sourceVM), &infravirtrigaudiov1beta1.VirtualMachine{}))
		}, "10s", "200ms").Should(BeTrue(), "the exported source VM is deleted (UID precondition)")
		Expect(c.prov.calls.Load()).To(BeEquivalentTo(1))
	})

	It("never deletes a source VirtualMachine re-created under the same name", func() {
		c := setup(true, func(c *readyCase) {
			// The migration exported an earlier VirtualMachine of this name.
			c.m.Status.SourceVMUID = "00000000-0000-4000-8000-000000000001"
		})
		Eventually(func(g Gomega) {
			got := stored(g, c.m)
			g.Expect(got.Status.StorageInfo.CleanedUp).To(BeTrue())
		}, "20s", "200ms").Should(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c.sourceVM), &infravirtrigaudiov1beta1.VirtualMachine{})).To(Succeed())
		Eventually(func() int { return countEvents(c.rec, migrationReasonSourceDeletionRefused) }, "5s", "200ms").Should(Equal(1))
	})
})
