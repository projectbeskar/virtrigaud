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
	"context"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// refusingVMProvider refuses every Create as an invalid specification and
// counts them; safe for a running controller.
type refusingVMProvider struct {
	stubProvider
	creates atomic.Int32
}

func (p *refusingVMProvider) Create(context.Context, contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.creates.Add(1)
	return contracts.CreateResponse{}, contracts.NewInvalidSpecError("create: libvirt rejected the domain definition", nil)
}

var _ = Describe("A VM create the provider refuses (envtest)", func() {
	ctx := context.Background()

	It("is not re-sent in a loop, and is retried at once when the VMClass, VMImage, Provider or VM spec changes", func() {
		mgrCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "refusal-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		prov := &refusingVMProvider{}
		resolver := &atomicResolver{provider: prov, namespace: ns.Name}
		Expect((&VirtualMachineReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), RemoteResolver: resolver, Recorder: record.NewFakeRecorder(100),
		}).SetupWithManager(mgr)).To(Succeed())
		mgrDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(mgrDone)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() { stop(); <-mgrDone })

		provider := envtestProvider(ns.Name, "libvirt", nil)
		Expect(k8sClient.Create(ctx, provider)).To(Succeed())
		provider.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, provider)).To(Succeed())
		class := envtestClass(ns.Name, "small", nil)
		Expect(k8sClient.Create(ctx, class)).To(Succeed())
		image := &infravirtrigaudiov1beta1.VMImage{
			ObjectMeta: metav1.ObjectMeta{Name: "jammy", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VMImageSpec{
				Source: infravirtrigaudiov1beta1.ImageSource{
					HTTP: &infravirtrigaudiov1beta1.HTTPImageSource{URL: "https://images.example.com/jammy.qcow2"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, image)).To(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: provider.Name},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: class.Name},
				ImageRef:    &infravirtrigaudiov1beta1.ObjectRef{Name: image.Name},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())

		creates := func() int32 { return prov.creates.Load() }
		readyReason := func(g Gomega) string {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got)).To(Succeed())
			if c := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady); c != nil {
				return c.Reason
			}
			return ""
		}
		// edit re-reads obj, applies change and updates it (a spec change).
		edit := func(obj client.Object, change func()) {
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
				change()
				g.Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			}, "10s", "100ms").Should(Succeed())
		}

		By("refusing the create once, then waiting instead of re-sending it")
		Eventually(creates, "20s", "100ms").Should(BeNumerically("==", 1))
		Eventually(readyReason, "10s", "100ms").Should(Equal(k8s.ReasonValidationError))
		Consistently(creates, "3s", "100ms").Should(BeNumerically("==", 1))

		By("an edit of the VMClass retries the create at once")
		edit(class, func() { class.Spec.CPU = 3 })
		Eventually(creates, "10s", "100ms").Should(BeNumerically("==", 2))
		Consistently(creates, "2s", "100ms").Should(BeNumerically("==", 2))

		By("an edit of the VMImage retries the create at once")
		edit(image, func() { image.Spec.Source.HTTP.URL = "https://images.example.com/noble.qcow2" })
		Eventually(creates, "10s", "100ms").Should(BeNumerically("==", 3))
		Consistently(creates, "2s", "100ms").Should(BeNumerically("==", 3))

		By("an edit of the Provider retries the create at once")
		edit(provider, func() { provider.Spec.Runtime.Image = "virtrigaud/provider-libvirt:test-2" })
		Eventually(creates, "10s", "100ms").Should(BeNumerically("==", 4))
		Consistently(creates, "2s", "100ms").Should(BeNumerically("==", 4))

		By("a status-only update of the Provider does not")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(provider), provider)).To(Succeed())
		provider.Status.Runtime.Endpoint = "provider:9444"
		Expect(k8sClient.Status().Update(ctx, provider)).To(Succeed())
		Consistently(creates, "2s", "100ms").Should(BeNumerically("==", 4))

		By("an edit of the VM itself retries the create at once")
		edit(vm, func() { vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff })
		Eventually(creates, "10s", "100ms").Should(BeNumerically("==", 5))
		Consistently(creates, "2s", "100ms").Should(BeNumerically("==", 5))
	})
})
