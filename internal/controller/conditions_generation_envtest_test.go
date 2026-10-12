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

// Conditions set through internal/k8s record the object's generation
// (observedGeneration), and a reconcile that observes nothing new writes
// nothing: recording the generation never starts a status write loop.
var _ = Describe("Condition observedGeneration (envtest)", func() {
	It("records the generation on Ready/Provisioning and writes nothing on an unchanged reconcile", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cond-gen-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := holdTestVM(ns.Name, "web", infravirtrigaudiov1beta1.ObservedPowerStateOn)
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

		rp := &routingProvider{describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn),
			IPs: []string{"10.0.0.5"}}}
		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: record.NewFakeRecorder(100)}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		get := func() *infravirtrigaudiov1beta1.VirtualMachine {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
			return got
		}
		reconcileN := func(n int) {
			for i := 0; i < n; i++ {
				_, err := r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
			}
		}
		expectGeneration := func(got *infravirtrigaudiov1beta1.VirtualMachine) {
			for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
				c := meta.FindStatusCondition(got.Status.Conditions, condType)
				Expect(c).NotTo(BeNil(), condType)
				Expect(c.ObservedGeneration).To(Equal(got.Generation), condType)
			}
		}

		reconcileN(3)
		got := get()
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, k8s.ConditionReady)).To(BeTrue())
		expectGeneration(got)
		rv := got.ResourceVersion
		reconcileN(3)
		Expect(get().ResourceVersion).To(Equal(rv), "an unchanged reconcile writes nothing")

		By("a spec change is recorded once, then nothing is written again")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := get()
			cur.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
			return k8sClient.Update(ctx, cur)
		})).To(Succeed())
		Expect(get().Generation).To(BeNumerically(">", got.Generation))
		reconcileN(1)
		got = get()
		expectGeneration(got)
		rv = got.ResourceVersion
		reconcileN(3)
		Expect(get().ResourceVersion).To(Equal(rv), "no write loop after the generation is recorded")
	})
})
