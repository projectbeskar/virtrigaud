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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// Single-host adoption that stamps the adopted VM, against a real API server:
// the VirtualMachine is created without status, the VM is handed to it, and
// only then is status.id written through the status subresource; a refused
// transfer deletes the waiting VirtualMachine (preconditioned); a failure
// between creation and binding is completed by the next discovery.
var _ = Describe("Single-host adoption stamps the adopted VM (envtest)", func() {
	ctx := context.Background()

	newNamespace := func(prefix string) string {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		return ns.Name
	}

	// setup creates a ready single-host Provider "prov" in ns with adoption
	// on (plus extra annotations) and returns the reconciler over prov.
	setup := func(ns string, prov *fakeClusteredAdopter, annotations map[string]string) (*VMAdoptionReconciler, types.NamespacedName) {
		p := envtestProvider(ns, "prov", nil)
		// Its own hypervisor: Providers of the other specs (envtest keeps
		// their namespaces) must not be sibling Providers of this one.
		p.Spec.Endpoint = "qemu+ssh://virt@" + ns + "/system"
		p.Annotations = map[string]string{AdoptionAnnotation: "true"}
		for k, v := range annotations {
			p.Annotations[k] = v
		}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		p.Status.Conditions = []metav1.Condition{{Type: "ProviderAvailable", Status: metav1.ConditionTrue,
			Reason: "RemoteAvailable", Message: "ready", LastTransitionTime: metav1.Now()}}
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		r := &VMAdoptionReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			RemoteResolver: &stubResolver{provider: prov}}
		return r, client.ObjectKeyFromObject(p)
	}
	reconcile := func(r *VMAdoptionReconciler, key types.NamespacedName) ctrl.Result {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	rediscover := func(key types.NamespacedName) {
		p := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
		old := metav1.NewTime(time.Now().Add(-singleHostDiscoveryInterval - time.Second))
		p.Status.Adoption.LastDiscoveryTime = &old
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}
	getVM := func(ns, name string) (*infravirtrigaudiov1beta1.VirtualMachine, error) {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		return vm, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vm)
	}

	It("re-adopts a VM left stamped by a detached VirtualMachine: the VM carries its new owner before status.id is written", func() {
		ns := newNamespace("adopt-restamp-")
		prov := &fakeClusteredAdopter{caps: contracts.Capabilities{SupportsOwnerTransfer: true}, domains: []*fakeDomain{
			{id: "web", uuid: "uuid-web", owner: "uid-detached-vm", power: "On", cpu: 1, memMiB: 1024},
		}}
		r, key := setup(ns, prov, map[string]string{AdoptStampedVMsAnnotation: "true"})

		reconcile(r, key)
		vm, err := getVM(ns, "web")
		Expect(err).NotTo(HaveOccurred())
		Expect(vm.Status.ID).To(Equal("web"))
		Expect(vm.Status.BoundProvider).NotTo(BeNil())
		Expect(vm.Annotations).To(HaveKeyWithValue(AdoptedIDAnnotation, "web"))
		Expect(prov.find("", "web").owner).To(Equal(string(vm.UID)), "the stale stamp was replaced by the new owner's")
		transfers := prov.transfersTo("", "web")
		Expect(transfers).To(HaveLen(1))
		Expect(transfers[0].ReplaceableOwnerUIDs).To(Equal([]string{"uid-detached-vm"}))
		Expect(transfers[0].VM.Owner).To(Equal(contracts.ObjectIdentity{UID: string(vm.UID), Namespace: ns, Name: "web"}))
	})

	It("never stamps a VM whose stamp names a VirtualMachine that exists in another namespace", func() {
		ns := newNamespace("adopt-restamp-live-")
		other := newNamespace("adopt-restamp-tenant-")
		live := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: other, Name: "db"},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "elsewhere"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImageRef:    &infravirtrigaudiov1beta1.ObjectRef{Name: "img"},
			},
		}
		Expect(k8sClient.Create(ctx, live)).To(Succeed())
		prov := &fakeClusteredAdopter{caps: contracts.Capabilities{SupportsOwnerTransfer: true}, domains: []*fakeDomain{
			{id: "db", uuid: "uuid-db", owner: string(live.UID), power: "On"},
		}}
		r, key := setup(ns, prov, map[string]string{AdoptStampedVMsAnnotation: "true"})

		reconcile(r, key)
		Expect(prov.transfers).To(BeEmpty())
		_, err := getVM(ns, "db")
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(prov.find("", "db").owner).To(Equal(string(live.UID)))
	})

	It("finishes an adoption interrupted between the VirtualMachine's creation and its binding, and withdraws a refused one", func() {
		ns := newNamespace("adopt-restamp-window-")
		prov := &fakeClusteredAdopter{caps: contracts.Capabilities{SupportsOwnerTransfer: true},
			transferErr: contracts.NewRetryableError("provider restarting", nil),
			domains: []*fakeDomain{
				{id: "web", uuid: "uuid-web", power: "On"},
			}}
		r, key := setup(ns, prov, nil)

		By("a transfer that fails retryably leaves the VirtualMachine waiting, unbound")
		Expect(reconcile(r, key).RequeueAfter).To(Equal(singleHostDiscoveryInterval))
		waiting, err := getVM(ns, "web")
		Expect(err).NotTo(HaveOccurred())
		Expect(waiting.Status.ID).To(BeEmpty())
		Expect(prov.find("", "web").owner).To(BeEmpty())

		By("the next discovery hands the VM to the same VirtualMachine and binds it")
		prov.mu.Lock()
		prov.transferErr = nil
		prov.mu.Unlock()
		rediscover(key)
		reconcile(r, key)
		bound, err := getVM(ns, "web")
		Expect(err).NotTo(HaveOccurred())
		Expect(bound.UID).To(Equal(waiting.UID))
		Expect(bound.Status.ID).To(Equal("web"))
		Expect(prov.find("", "web").owner).To(Equal(string(bound.UID)))

		By("a transfer the provider refuses deletes the waiting VirtualMachine again")
		prov.mu.Lock()
		prov.domains = append(prov.domains, &fakeDomain{id: "db", uuid: "uuid-db", power: "Off"})
		prov.transferErr = contracts.NewConflictError("owned by another VirtualMachine", nil)
		prov.mu.Unlock()
		rediscover(key)
		reconcile(r, key)
		Expect(prov.transfersTo("", "db")).To(HaveLen(1))
		_, err = getVM(ns, "db")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the refused adoption's VirtualMachine is gone: %v", err)
		p := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, key, p)).To(Succeed())
		Expect(p.Status.Adoption.Message).To(ContainSubstring(string(skipTransferRefused)))
	})
})
