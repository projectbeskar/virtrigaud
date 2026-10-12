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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// The VMSnapshot controller sets its conditions through internal/util/k8s,
// whose SetCondition now records the object's generation instead of a
// per-change counter. A reconcile that observes nothing new must still write
// nothing: no resourceVersion change, so no self-triggered loop.
var _ = Describe("VMSnapshot conditions observedGeneration (envtest)", func() {
	It("records the generation and leaves resourceVersion unchanged on repeated reconciles", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "snap-cond-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, holdTestProvider(ns.Name))).To(Succeed())
		holdTestVM(ns.Name, "web", infravirtrigaudiov1beta1.ObservedPowerStateOn)

		snap := &infravirtrigaudiov1beta1.VMSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-cond", Namespace: ns.Name},
			Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "web"}},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		DeferCleanup(func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				got := &infravirtrigaudiov1beta1.VMSnapshot{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(snap), got); err != nil {
					return client.IgnoreNotFound(err)
				}
				got.Finalizers = nil
				return k8sClient.Update(ctx, got)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, snap))).To(Succeed())
		})

		spy := &snapshotCreateSpy{}
		r := NewVMSnapshotReconciler(k8sClient, k8sClient.Scheme(), nil, record.NewFakeRecorder(100), false)
		r.APIReader = k8sClient
		r.providerInstanceFn = onlyProviderIn(ns.Name, spy)
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)}
		get := func() *infravirtrigaudiov1beta1.VMSnapshot {
			got := &infravirtrigaudiov1beta1.VMSnapshot{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
			return got
		}

		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(get().Status.Phase).To(Equal(infravirtrigaudiov1beta1.SnapshotPhaseReady))
		}, "10s", "50ms").Should(Succeed())
		Expect(spy.creates.Load()).To(Equal(int32(1)))

		ready := get()
		Expect(ready.Status.Conditions).NotTo(BeEmpty())
		for _, c := range ready.Status.Conditions {
			Expect(c.ObservedGeneration).To(Equal(ready.Generation), c.Type)
		}
		rv := ready.ResourceVersion
		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(get().ResourceVersion).To(Equal(rv), "a repeated reconcile of a Ready snapshot writes nothing")
		Expect(spy.creates.Load()).To(Equal(int32(1)))
	})
})
