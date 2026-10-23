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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The snapshot revert against a real API server (V2): status.snapshotRevert
// passes the CRD schema, the attempt is recorded through the status
// subresource before the call, and the revert is sent once.
var _ = Describe("VirtualMachine snapshot revert (envtest)", func() {
	It("reverts a powered-off VM once and records the outcome in status.snapshotRevert", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "snap-revert-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())

		vm := holdTestVM(ns.Name, "web", infravirtrigaudiov1beta1.ObservedPowerStateOff)
		DeferCleanup(func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				got := &infravirtrigaudiov1beta1.VirtualMachine{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got); err != nil {
					return client.IgnoreNotFound(err)
				}
				got.Finalizers = nil
				return k8sClient.Update(ctx, got)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vm))).To(Succeed())
		})
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got); err != nil {
				return err
			}
			got.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
			got.Spec.Snapshot = &infravirtrigaudiov1beta1.VMSnapshotOperation{
				RevertToRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "before-upgrade"}}
			return k8sClient.Update(ctx, got)
		})).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), vm)).To(Succeed())

		snap := &infravirtrigaudiov1beta1.VMSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "before-upgrade", Namespace: ns.Name},
			Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "web"}},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		snap.Status = infravirtrigaudiov1beta1.VMSnapshotStatus{
			Phase:         infravirtrigaudiov1beta1.SnapshotPhaseReady,
			SnapshotID:    "before-upgrade",
			VMUID:         string(vm.UID),
			BoundProvider: &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: ns.Name, Name: prov.Name, UID: string(prov.UID)},
		}
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		rp := &revertProvider{describe: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}}
		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: record.NewFakeRecorder(100)}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}

		got := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		Expect(rp.reverts).To(Equal([]string{"before-upgrade"}))
		Expect(got.Status.SnapshotRevert).NotTo(BeNil())
		Expect(got.Status.SnapshotRevert.Phase).To(Equal(infravirtrigaudiov1beta1.SnapshotRevertPhaseSucceeded))
		Expect(got.Status.SnapshotRevert.SnapshotUID).To(Equal(string(snap.UID)))
		Expect(got.Status.SnapshotRevert.Attempts).To(Equal(int32(1)))
		Expect(got.Status.SnapshotRevert.AttemptTime).NotTo(BeNil())
		Expect(got.Status.SnapshotRevert.CompletionTime).NotTo(BeNil())
		c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VirtualMachineConditionSnapshotReverted)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.ObservedGeneration).To(Equal(got.Generation))
		ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))

		// A GitOps re-apply of the same spec sends nothing.
		for i := 0; i < 2; i++ {
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(rp.reverts).To(HaveLen(1))
	})
})
