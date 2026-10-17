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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These envtest specs run the VMMigration and VMSnapshot controllers against a
// real apiserver for the durable requested-call markers (rc2 R6). The restart
// after a call is simulated by storing the marker (as the earlier leader did
// right before its call) with no result and starting a new manager: nothing
// is held in its memory.
var _ = Describe("Requested provider calls survive a manager restart (envtest)", func() {
	newNamespace := func(prefix string) string {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		return ns.Name
	}

	// exportingMigration creates, in ns, the source VM and Provider and a
	// pvc migration in the Exporting phase; when requestedAgo is not zero its
	// status carries an ExportRequested marker that old, with no result.
	exportingMigration := func(ns string, requestedAgo time.Duration) *infravirtrigaudiov1beta1.VMMigration {
		Expect(k8sClient.Create(ctx, holdTestProvider(ns))).To(Succeed())
		holdTestVM(ns, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOff)
		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns, Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web", ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"}},
				Storage: &infravirtrigaudiov1beta1.MigrationStorage{Type: "pvc",
					PVC: &infravirtrigaudiov1beta1.PVCStorageConfig{Name: "mig-pvc"}},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		m.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseExporting
		m.Status.StoragePVCName = "mig-pvc"
		if requestedAgo != 0 {
			setRequestMarker(&m.Status.Conditions, migrationConditionExportRequested, migrationConditionExportRequested,
				migrationRequests[longOpExport].requestedMessage, m.Generation, time.Now().Add(-requestedAgo))
		}
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		return m
	}

	startMigrationManager := func(ns string, prov contracts.Provider) {
		startHoldTestManager(func(mgr ctrl.Manager) {
			r := NewVMMigrationReconciler(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme(), nil, record.NewFakeRecorder(200), false)
			r.providerInstanceFn = onlyProviderIn(ns, prov)
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})
	}

	stored := func(g Gomega, m *infravirtrigaudiov1beta1.VMMigration) *infravirtrigaudiov1beta1.VMMigration {
		got := &infravirtrigaudiov1beta1.VMMigration{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got)).To(Succeed())
		return got
	}

	It("holds an ExportDisk whose outcome was lost when the provider does not report it re-send safe, and sends it on the operator's confirmation", func() {
		ns := newNamespace("r6-export-hold-")
		m := exportingMigration(ns, time.Hour)
		prov := &capsMigrationProvider{}
		startMigrationManager(ns, prov)

		Eventually(func(g Gomega) {
			ready := meta.FindStatusCondition(stored(g, m).Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).To(Equal(migrationReasonExportOutcomeUnknown))
			g.Expect(ready.Message).To(Equal(migrationRequests[longOpExport].operatorMessage))
		}, "20s", "100ms").Should(Succeed())
		Consistently(prov.exportCalls.Load, "2s", "100ms").Should(BeZero(), "never re-sent without a confirmation")

		got := stored(Default, m)
		got.Annotations = map[string]string{operationOutcomeAnnotation: operationOutcomeNotPerformedPrefix +
			requestMarkerAttemptID(got.Status.Conditions, migrationConditionExportRequested)}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())

		Eventually(func(g Gomega) {
			got := stored(g, m)
			g.Expect(got.Status.ExportID).To(Equal("export-1"))
			g.Expect(got.Annotations).NotTo(HaveKey(operationOutcomeAnnotation))
			g.Expect(migrationRequested(got, longOpExport)).To(BeFalse())
		}, "20s", "100ms").Should(Succeed())
		Expect(prov.exportCalls.Load()).To(BeEquivalentTo(1))
	})

	It("re-sends an ExportDisk whose outcome was lost once the earlier call's deadline passed, where the provider reports the backend re-send safe", func() {
		ns := newNamespace("r6-export-resend-")
		m := exportingMigration(ns, migrationResendSettleTime+time.Minute)
		prov := &capsMigrationProvider{caps: contracts.Capabilities{ResendSafeExportBackends: []string{"pvc"}}}
		startMigrationManager(ns, prov)

		Eventually(func(g Gomega) {
			got := stored(g, m)
			g.Expect(got.Status.ExportID).To(Equal("export-1"))
			g.Expect(got.Status.DiskInfo).NotTo(BeNil())
			g.Expect(got.Status.DiskInfo.SourceChecksum).To(Equal("checksum-1"))
			g.Expect(migrationRequested(got, longOpExport)).To(BeFalse())
		}, "20s", "100ms").Should(Succeed())
		Expect(prov.exportCalls.Load()).To(BeEquivalentTo(1))
	})

	It("sends nothing when the marker cannot be written over the read resourceVersion (real Conflict)", func() {
		ns := newNamespace("r6-export-conflict-")
		m := exportingMigration(ns, 0)
		prov := &capsMigrationProvider{}
		r := NewVMMigrationReconciler(k8sClient, k8sClient, k8sClient.Scheme(), nil, record.NewFakeRecorder(20), false)
		r.providerInstanceFn = onlyProviderIn(ns, prov)

		read := stored(Default, m)
		other := read.DeepCopy()
		other.Annotations = map[string]string{"example.com/poke": "1"}
		Expect(k8sClient.Update(ctx, other)).To(Succeed())

		_, err := r.handleExportingPhase(ctx, read)
		Expect(err).To(HaveOccurred())
		Expect(isMigrationStatusContention(err)).To(BeTrue())
		Expect(prov.exportCalls.Load()).To(BeZero())
		Expect(migrationRequested(stored(Default, m), longOpExport)).To(BeFalse())

		// From a fresh read the marker is written, then the export sent.
		_, err = r.handleExportingPhase(ctx, stored(Default, m))
		Expect(err).NotTo(HaveOccurred())
		Expect(prov.exportCalls.Load()).To(BeEquivalentTo(1))
		Expect(stored(Default, m).Status.ExportID).To(Equal("export-1"))
	})

	It("holds a single-host VMSnapshot whose SnapshotCreate outcome was lost, and creates it on the operator's confirmation", func() {
		ns := newNamespace("r6-snap-hold-")
		Expect(k8sClient.Create(ctx, holdTestProvider(ns))).To(Succeed())
		holdTestVM(ns, "web", infravirtrigaudiov1beta1.ObservedPowerStateOn)
		snap := &infravirtrigaudiov1beta1.VMSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-r6", Namespace: ns, Finalizers: []string{infravirtrigaudiov1beta1.VMSnapshotFinalizer}},
			Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "web"}},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		setRequestMarker(&snap.Status.Conditions, snapshotConditionRequested, snapshotConditionRequested,
			snapshotRequestedMessage, snap.Generation, time.Now().Add(-time.Hour))
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		prov := &snapshotTaskProvider{sync: true}
		startHoldTestManager(func(mgr ctrl.Manager) {
			r := NewVMSnapshotReconciler(mgr.GetClient(), mgr.GetScheme(), nil, record.NewFakeRecorder(100), false)
			r.APIReader = mgr.GetAPIReader()
			r.providerInstanceFn = onlyProviderIn(ns, prov)
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})

		storedSnap := func(g Gomega) *infravirtrigaudiov1beta1.VMSnapshot {
			got := &infravirtrigaudiov1beta1.VMSnapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(snap), got)).To(Succeed())
			return got
		}
		Eventually(func(g Gomega) {
			ready := meta.FindStatusCondition(storedSnap(g).Status.Conditions, infravirtrigaudiov1beta1.VMSnapshotConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).To(Equal(snapshotReasonOutcomeUnknown))
		}, "20s", "100ms").Should(Succeed())
		Consistently(prov.creates.Load, "2s", "100ms").Should(BeZero())

		got := storedSnap(Default)
		got.Annotations = map[string]string{operationOutcomeAnnotation: operationOutcomeNotPerformedPrefix +
			requestMarkerAttemptID(got.Status.Conditions, snapshotConditionRequested)}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())

		Eventually(func(g Gomega) {
			got := storedSnap(g)
			g.Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.SnapshotPhaseReady))
			g.Expect(got.Status.SnapshotID).To(Equal("snap-t"))
			g.Expect(snapshotRequested(got)).To(BeFalse())
			g.Expect(got.Annotations).NotTo(HaveKey(operationOutcomeAnnotation))
		}, "20s", "100ms").Should(Succeed())
		Expect(prov.creates.Load()).To(BeEquivalentTo(1))
	})
})
