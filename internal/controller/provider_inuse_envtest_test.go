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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// These envtest specs run the Provider controller in a real manager against a
// real apiserver to pin the Provider in-use finalizer (ADR-0010) end to end:
// the finalizer is added to an existing Provider, a Provider being deleted is
// held with its runtime kept while VMs use it, the watches release it as soon
// as the last one goes (well before the 2-minute backstop), and force-delete
// releases it with a Warning event. envtest runs no namespace, garbage-
// collector, deployment or VirtualMachine controller: the specs do their part
// by hand (a VM delete through the Provider is its finalizer being removed).

// startProviderInUseManager starts a manager running only the Provider
// controller, its cache restricted to namespaces (so Providers of other specs
// are never touched), and stops it when the spec ends.
func startProviderInUseManager(namespaces ...string) {
	defaults := map[string]cache.Config{}
	for _, ns := range namespaces {
		defaults[ns] = cache.Config{}
	}
	mgrCtx, stop := context.WithCancel(ctx)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 k8sClient.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{DefaultNamespaces: defaults},
		Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect((&ProviderReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("provider-controller"),
	}).SetupWithManager(mgr)).To(Succeed())
	mgrDone := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(mgrDone)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	DeferCleanup(func() { stop(); <-mgrDone })
}

// inUseTestNamespace creates a namespace with a generated name.
func inUseTestNamespace(prefix string) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

// inUseEnvProvider creates a plaintext Provider prov-1 in ns WITHOUT the
// in-use finalizer, as a Provider created before the upgrade.
func inUseEnvProvider(ns string) *infravirtrigaudiov1beta1.Provider {
	p := holdTestProvider(ns)
	p.Spec.Runtime.Service = &infravirtrigaudiov1beta1.ProviderServiceSpec{
		Port: 9443, TLS: &infravirtrigaudiov1beta1.ProviderTLSSpec{Enabled: false},
	}
	Expect(k8sClient.Create(ctx, p)).To(Succeed())
	return p
}

// inUseEnvVM creates the VirtualMachine ns/name on providerNS/prov-1 with the
// VirtualMachine finalizer, as the VirtualMachine controller would have.
func inUseEnvVM(ns, name, providerNS string) {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Finalizers: []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1", Namespace: providerNS},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
		},
	}
	Expect(k8sClient.Create(ctx, vm)).To(Succeed())
	// Created on the hypervisor: an unbound VM holds its Provider only once it
	// may have something there (status.id here).
	vm.Status.ID = ns + "." + name
	Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
}

// deleteVMThroughProvider simulates the VirtualMachine controller deleting a
// VM through its Provider: deletion requested, then the finalizer removed.
func deleteVMThroughProvider(ns, name string) {
	key := types.NamespacedName{Namespace: ns, Name: name}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	Expect(k8sClient.Get(ctx, key, vm)).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vm))).To(Succeed())
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, key, vm); err != nil {
			return err
		}
		vm.Finalizers = nil
		return k8sClient.Update(ctx, vm)
	})).To(Succeed())
}

// providerRuntimeName is the name of the runtime objects of prov-1 in ns.
func providerRuntimeName(ns string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: fmt.Sprintf("virtrigaud-provider-%s-prov-1", ns)}
}

// waitProviderRunning waits for the finalizer and the runtime Deployment, marks
// the Deployment ready (no deployment controller runs in envtest) and waits for
// the runtime phase Running.
func waitProviderRunning(ns string) {
	key := types.NamespacedName{Namespace: ns, Name: "prov-1"}
	Eventually(func(g Gomega) {
		p := &infravirtrigaudiov1beta1.Provider{}
		g.Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
		g.Expect(p.Finalizers).To(ContainElement(infravirtrigaudiov1beta1.ProviderInUseFinalizer),
			"the finalizer is added to a Provider that existed before the upgrade")
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, providerRuntimeName(ns), dep)).To(Succeed())
	}, "20s", "100ms").Should(Succeed())
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, providerRuntimeName(ns), dep); err != nil {
			return err
		}
		dep.Status.Replicas, dep.Status.ReadyReplicas = 1, 1
		return k8sClient.Status().Update(ctx, dep)
	})).To(Succeed())
	Eventually(func(g Gomega) {
		p := &infravirtrigaudiov1beta1.Provider{}
		g.Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
		g.Expect(p.Status.Runtime).NotTo(BeNil())
		g.Expect(p.Status.Runtime.Phase).To(Equal(infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning))
	}, "20s", "100ms").Should(Succeed())
}

// deleteProvider requests the deletion of prov-1 in ns.
func deleteProvider(ns string) {
	p := &infravirtrigaudiov1beta1.Provider{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, p)).To(Succeed())
	Expect(k8sClient.Delete(ctx, p)).To(Succeed())
}

// expectProviderHeld waits until prov-1 in ns is held with DeleteBlocked=True
// and reason, and returns it.
func expectProviderHeld(ns, reason string) *infravirtrigaudiov1beta1.Provider {
	p := &infravirtrigaudiov1beta1.Provider{}
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, p)).To(Succeed())
		g.Expect(p.DeletionTimestamp.IsZero()).To(BeFalse())
		cond := meta.FindStatusCondition(p.Status.Conditions, k8s.ConditionDeleteBlocked)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(reason))
	}, "20s", "100ms").Should(Succeed())
	return p
}

// expectProviderGone waits until prov-1 in ns is gone. The timeout is far
// below the 2-minute backstop, so it proves the watches released it.
func expectProviderGone(ns string) {
	Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, &infravirtrigaudiov1beta1.Provider{})
		return apierrors.IsNotFound(err)
	}, "20s", "100ms").Should(BeTrue())
}

// providerEvents lists the events recorded on prov-1 in ns with reason.
func providerEvents(ns, reason string) []corev1.Event {
	var events corev1.EventList
	Expect(k8sClient.List(ctx, &events, client.InNamespace(ns))).To(Succeed())
	var out []corev1.Event
	for _, e := range events.Items {
		if e.InvolvedObject.Kind == "Provider" && e.InvolvedObject.Name == "prov-1" && e.Reason == reason {
			out = append(out, e)
		}
	}
	return out
}

var _ = Describe("Provider in-use finalizer (envtest)", func() {
	It("holds a Provider with its runtime while VMs use it, writes nothing on a repeated hold, and releases it when the last VM goes", func() {
		ns := inUseTestNamespace("pinuse-hold-")
		startProviderInUseManager(ns)
		inUseEnvProvider(ns)
		inUseEnvVM(ns, "web", "")
		inUseEnvVM(ns, "db", "")
		waitProviderRunning(ns)

		deleteProvider(ns)
		held := expectProviderHeld(ns, reasonProviderInUse)
		Expect(held.Status.Runtime.Phase).To(Equal(infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning),
			"the runtime stays Running so the VMs can still be deleted through it")
		cond := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
		Expect(cond.Message).To(ContainSubstring("2 VirtualMachine(s)"))
		Eventually(func() int { return len(providerEvents(ns, eventReasonProviderDeleteBlocked)) }, "10s", "100ms").Should(Equal(1))

		By("a repeated hold writes nothing")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(held), held)).To(Succeed())
		rv := held.ResourceVersion
		// Re-drive the held Provider through its Owns watch: an unrelated
		// annotation on its Deployment.
		for i := 0; i < 5; i++ {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, providerRuntimeName(ns), dep); err != nil {
					return err
				}
				if dep.Annotations == nil {
					dep.Annotations = map[string]string{}
				}
				dep.Annotations["example.com/poke"] = fmt.Sprintf("%d", i)
				return k8sClient.Update(ctx, dep)
			})).To(Succeed())
		}
		Consistently(func(g Gomega) {
			p := &infravirtrigaudiov1beta1.Provider{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(held), p)).To(Succeed())
			g.Expect(p.ResourceVersion).To(Equal(rv))
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, providerRuntimeName(ns), dep)).To(Succeed(), "the runtime is kept")
		}, "3s", "200ms").Should(Succeed())
		Expect(providerEvents(ns, eventReasonProviderDeleteBlocked)).To(HaveLen(1))

		By("deleting one VM keeps the hold; deleting the last releases the Provider through the watch")
		deleteVMThroughProvider(ns, "web")
		Eventually(func(g Gomega) {
			p := &infravirtrigaudiov1beta1.Provider{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(held), p)).To(Succeed())
			cond := meta.FindStatusCondition(p.Status.Conditions, k8s.ConditionDeleteBlocked)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Message).To(ContainSubstring("1 VirtualMachine(s)"))
		}, "20s", "100ms").Should(Succeed())
		deleteVMThroughProvider(ns, "db")
		expectProviderGone(ns)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, providerRuntimeName(ns), &appsv1.Deployment{}))).To(BeTrue(),
			"the runtime is torn down on release")
	})

	It("is held by a VM in another namespace until that VM goes", func() {
		providerNS := inUseTestNamespace("pinuse-owner-")
		consumerNS := inUseTestNamespace("pinuse-consumer-")
		startProviderInUseManager(providerNS, consumerNS)
		p := inUseEnvProvider(providerNS)
		// Share it with every namespace: only an allowed (or bound) consumer
		// holds a Provider.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
				return err
			}
			p.Spec.ConsumerNamespaceSelector = &metav1.LabelSelector{}
			return k8sClient.Update(ctx, p)
		})).To(Succeed())
		inUseEnvVM(consumerNS, "api", providerNS)
		waitProviderRunning(providerNS)

		deleteProvider(providerNS)
		held := expectProviderHeld(providerNS, reasonProviderInUse)
		cond := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked)
		Expect(cond.Message).To(ContainSubstring(consumerNS + " (1)"))
		Expect(cond.Message).NotTo(ContainSubstring("api"), "the message names no tenant object")

		deleteVMThroughProvider(consumerNS, "api")
		expectProviderGone(providerNS)
	})

	It("in a namespace deletion, holds the Provider without touching its runtime until its VMs are deleted through it", func() {
		ns := inUseTestNamespace("pinuse-nsdel-")
		startProviderInUseManager(ns)
		inUseEnvProvider(ns)
		inUseEnvVM(ns, "web", "")
		inUseEnvVM(ns, "db", "")
		waitProviderRunning(ns)

		By("the namespace is deleted; envtest has no namespace controller, so delete its content by hand")
		nsObj := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ns}, nsObj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, nsObj)).To(Succeed())
		for _, name := range []string{"web", "db"} {
			vm := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
		}
		deleteProvider(ns)

		held := expectProviderHeld(ns, reasonProviderInUseRuntimeNotKept)
		Expect(held.Status.Runtime.Phase).To(Equal(infravirtrigaudiov1beta1.ProviderRuntimePhaseRunning),
			"status.runtime is left as it was, so VM deletes keep trying the provider while its pod answers")

		By("the VMs are deleted through the Provider first, then the Provider goes")
		deleteVMThroughProvider(ns, "web")
		Consistently(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, &infravirtrigaudiov1beta1.Provider{})
		}, "2s", "200ms").Should(Succeed(), "one VM is left, so the Provider stays")
		deleteVMThroughProvider(ns, "db")
		expectProviderGone(ns)
	})

	It("is released at once by force-delete, with a Warning event", func() {
		ns := inUseTestNamespace("pinuse-force-")
		startProviderInUseManager(ns)
		inUseEnvProvider(ns)
		inUseEnvVM(ns, "web", "")
		waitProviderRunning(ns)

		deleteProvider(ns)
		expectProviderHeld(ns, reasonProviderInUse)
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			p := &infravirtrigaudiov1beta1.Provider{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "prov-1"}, p); err != nil {
				return err
			}
			p.Annotations = map[string]string{forceDeleteAnnotation: "true"}
			return k8sClient.Update(ctx, p)
		})).To(Succeed())

		expectProviderGone(ns)
		Eventually(func(g Gomega) {
			events := providerEvents(ns, eventReasonProviderForceDeleted)
			g.Expect(events).To(HaveLen(1))
			g.Expect(events[0].Type).To(Equal(corev1.EventTypeWarning))
			g.Expect(events[0].Message).To(ContainSubstring("1 VirtualMachine(s)"))
		}, "10s", "100ms").Should(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "web"}, vm)).To(Succeed())
		Expect(vm.DeletionTimestamp.IsZero()).To(BeTrue(), "the VM is not touched")
	})
})
