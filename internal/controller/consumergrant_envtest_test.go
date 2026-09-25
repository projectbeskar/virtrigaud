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

// envtestVMProvider is a thread-safe provider for a running VirtualMachine
// controller: Create names the VM after the request and Describe reports it
// running, so the controller settles instead of re-creating.
type envtestVMProvider struct {
	stubProvider
	creates atomic.Int32
}

func (p *envtestVMProvider) Create(_ context.Context, req contracts.CreateRequest) (contracts.CreateResponse, error) {
	p.creates.Add(1)
	return contracts.CreateResponse{ID: req.Name}, nil
}

func (p *envtestVMProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}, nil
}

// atomicResolver counts resolutions; safe for a running controller.
type atomicResolver struct {
	provider contracts.Provider
	calls    atomic.Int32
}

func (a *atomicResolver) GetProvider(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	a.calls.Add(1)
	return a.provider, nil
}

// envtestProvider is a valid Provider (the CRD's validation applies) with the
// given consumer selector.
func envtestProvider(ns, name string, sel *metav1.LabelSelector) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type:                infravirtrigaudiov1beta1.ProviderTypeLibvirt,
			Endpoint:            "qemu+ssh://virt@kvm-01/system",
			CredentialSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: "creds"},
			Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeSpec{
				Mode:  infravirtrigaudiov1beta1.RuntimeModeRemote,
				Image: "virtrigaud/provider-libvirt:test",
			},
			ConsumerNamespaceSelector: sel,
		},
	}
}

var _ = Describe("Cross-namespace consumer grant (envtest)", func() {
	ctx := context.Background()

	newNamespace := func(prefix string, labels map[string]string) *corev1.Namespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix, Labels: labels}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		return ns
	}

	It("persists spec.consumerNamespaceSelector on Provider, VMClass and VMImage, keeping unset and {} distinct", func() {
		ns := newNamespace("cg-crd-", nil)
		sel := &metav1.LabelSelector{
			MatchLabels: map[string]string{"virtrigaud.io/tenant": "gold"},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: corev1.LabelMetadataName, Operator: metav1.LabelSelectorOpIn, Values: []string{"team-a", "team-b"},
			}},
		}

		for name, want := range map[string]*metav1.LabelSelector{"unset": nil, "empty": {}, "selector": sel} {
			prov := envtestProvider(ns.Name, "p-"+name, want)
			Expect(k8sClient.Create(ctx, prov)).To(Succeed())
			class := envtestClass(ns.Name, "c-"+name, want)
			Expect(k8sClient.Create(ctx, class)).To(Succeed())
			img := &infravirtrigaudiov1beta1.VMImage{
				ObjectMeta: metav1.ObjectMeta{Name: "i-" + name, Namespace: ns.Name},
				Spec: infravirtrigaudiov1beta1.VMImageSpec{
					Source: infravirtrigaudiov1beta1.ImageSource{
						HTTP: &infravirtrigaudiov1beta1.HTTPImageSource{URL: "https://images.example.com/jammy.qcow2"},
					},
					ConsumerNamespaceSelector: want,
				},
			}
			Expect(k8sClient.Create(ctx, img)).To(Succeed())

			gotProv := &infravirtrigaudiov1beta1.Provider{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(prov), gotProv)).To(Succeed())
			gotClass := &infravirtrigaudiov1beta1.VMClass{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(class), gotClass)).To(Succeed())
			gotImg := &infravirtrigaudiov1beta1.VMImage{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(img), gotImg)).To(Succeed())
			for _, got := range []*metav1.LabelSelector{
				gotProv.Spec.ConsumerNamespaceSelector,
				gotClass.Spec.ConsumerNamespaceSelector,
				gotImg.Spec.ConsumerNamespaceSelector,
			} {
				if want == nil {
					Expect(got).To(BeNil(), name)
					continue
				}
				Expect(got).NotTo(BeNil(), "%s: {} must survive the round trip — it means every namespace", name)
				Expect(got.MatchLabels).To(Equal(want.MatchLabels), name)
				Expect(got.MatchExpressions).To(Equal(want.MatchExpressions), name)
			}
		}
	})

	It("refuses an ungranted cross-namespace Provider and proceeds within seconds of the grant (watches)", func() {
		mgrCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)

		infra := newNamespace("cg-infra-", nil)
		team := newNamespace("cg-team-", nil)
		labelled := newNamespace("cg-labelled-", nil)

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		prov := &envtestVMProvider{}
		resolver := &atomicResolver{provider: prov}
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

		// Two Providers in infra: one not shared yet, one shared with
		// namespaces labelled virtrigaud.io/shared-infra=true.
		byName := envtestProvider(infra.Name, "by-name", nil)
		byLabel := envtestProvider(infra.Name, "by-label", &metav1.LabelSelector{
			MatchLabels: map[string]string{"virtrigaud.io/shared-infra": "true"},
		})
		for _, p := range []*infravirtrigaudiov1beta1.Provider{byName, byLabel} {
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		}

		newVM := func(ns, providerName string) *infravirtrigaudiov1beta1.VirtualMachine {
			Expect(k8sClient.Create(ctx, envtestClass(ns, "small", nil))).To(Succeed())
			vm := &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
					ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: providerName, Namespace: infra.Name},
					ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
					ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
				},
			}
			Expect(k8sClient.Create(ctx, vm)).To(Succeed())
			return vm
		}
		readyReason := func(vm *infravirtrigaudiov1beta1.VirtualMachine) func(Gomega) string {
			return func(g Gomega) string {
				got := &infravirtrigaudiov1beta1.VirtualMachine{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got)).To(Succeed())
				c := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
				if c == nil {
					return ""
				}
				return c.Reason
			}
		}
		statusID := func(vm *infravirtrigaudiov1beta1.VirtualMachine) func(Gomega) string {
			return func(g Gomega) string {
				got := &infravirtrigaudiov1beta1.VirtualMachine{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got)).To(Succeed())
				return got.Status.ID
			}
		}

		vmByName := newVM(team.Name, "by-name")
		vmByLabel := newVM(labelled.Name, "by-label")

		By("refusing both VMs without resolving or calling any provider")
		Eventually(readyReason(vmByName), "20s", "200ms").Should(Equal(k8s.ReasonConsumerNotAllowed))
		Eventually(readyReason(vmByLabel), "20s", "200ms").Should(Equal(k8s.ReasonConsumerNotAllowed))
		Consistently(func() int32 { return resolver.calls.Load() }, "1s", "100ms").Should(BeZero())
		Expect(prov.creates.Load()).To(BeZero())

		By("a selector naming the namespace (kubernetes.io/metadata.name) re-drives the VM through the Provider watch")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(byName), byName)).To(Succeed())
		byName.Spec.ConsumerNamespaceSelector = selectNamespace(team.Name)
		Expect(k8sClient.Update(ctx, byName)).To(Succeed())
		Eventually(statusID(vmByName), "20s", "200ms").ShouldNot(BeEmpty())

		By("labelling the namespace re-drives the other VM through the Namespace watch")
		Expect(readyReason(vmByLabel)(Default)).To(Equal(k8s.ReasonConsumerNotAllowed))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(labelled), labelled)).To(Succeed())
		if labelled.Labels == nil {
			labelled.Labels = map[string]string{}
		}
		labelled.Labels["virtrigaud.io/shared-infra"] = "true"
		Expect(k8sClient.Update(ctx, labelled)).To(Succeed())
		Eventually(statusID(vmByLabel), "20s", "200ms").ShouldNot(BeEmpty())

		By("revoking the grant fails the bound VM closed")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(byName), byName)).To(Succeed())
		byName.Spec.ConsumerNamespaceSelector = nil
		Expect(k8sClient.Update(ctx, byName)).To(Succeed())
		Eventually(readyReason(vmByName), "20s", "200ms").Should(Equal(k8s.ReasonConsumerNotAllowed))
		Expect(statusID(vmByName)(Default)).NotTo(BeEmpty(), "nothing is unbound or deleted")
	})
})
