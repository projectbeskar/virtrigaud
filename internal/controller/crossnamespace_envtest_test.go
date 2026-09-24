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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// This envtest spec runs the VMClone reconciler against a real apiserver with
// real Namespace objects: a clone into another namespace creates nothing there
// until that namespace's grant annotation lists the clone's namespace, and the
// VM it then creates references the Provider the clone ran on.
var _ = Describe("Cross-namespace VMClone target (envtest)", func() {
	ctx := context.Background()

	It("refuses an ungranted target namespace and proceeds once granted", func() {
		src := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "xns-src-"}}
		tgt := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "xns-tgt-"}}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		Expect(k8sClient.Create(ctx, tgt)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, src)
			_ = k8sClient.Delete(ctx, tgt)
		})

		prov := &infravirtrigaudiov1beta1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "prov-1", Namespace: src.Name},
			Spec: infravirtrigaudiov1beta1.ProviderSpec{
				Type:                infravirtrigaudiov1beta1.ProviderTypeLibvirt,
				Endpoint:            "qemu+ssh://virt@kvm-01/system",
				CredentialSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: "creds"},
				Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeSpec{
					Mode:  infravirtrigaudiov1beta1.RuntimeModeRemote,
					Image: "virtrigaud/provider-libvirt:test",
				},
			},
		}
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())

		srcVM := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: src.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			},
		}
		Expect(k8sClient.Create(ctx, srcVM)).To(Succeed())
		srcVM.Status.ID = src.Name + ".src-vm"
		Expect(k8sClient.Status().Update(ctx, srcVM)).To(Succeed())

		clone := &infravirtrigaudiov1beta1.VMClone{
			ObjectMeta: metav1.ObjectMeta{Name: "clone-x", Namespace: src.Name},
			Spec: infravirtrigaudiov1beta1.VMCloneSpec{
				Source: infravirtrigaudiov1beta1.CloneSource{VMRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target: infravirtrigaudiov1beta1.VMCloneTarget{
					Name:      "web",
					Namespace: tgt.Name,
					Labels:    map[string]string{"tenant": "attacker-chosen"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())

		cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: tgt.Name + ".web"}}
		r := &VMCloneReconciler{
			Client:         k8sClient,
			Scheme:         k8sClient.Scheme(),
			RemoteResolver: &stubResolver{provider: cp},
			Recorder:       record.NewFakeRecorder(50),
		}
		reconcileN := func(n int) reconcile.Result {
			var res reconcile.Result
			for i := 0; i < n; i++ {
				var err error
				res, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clone)})
				Expect(err).NotTo(HaveOccurred())
			}
			return res
		}

		By("refusing while the target namespace has no grant")
		res := reconcileN(4)
		Expect(res.RequeueAfter).To(Equal(crossNamespaceRecheckInterval))
		Expect(cp.cloneCnt).To(BeZero(), "no provider-side clone for a refused target")
		vms := &infravirtrigaudiov1beta1.VirtualMachineList{}
		Expect(k8sClient.List(ctx, vms, client.InNamespace(tgt.Name))).To(Succeed())
		Expect(vms.Items).To(BeEmpty())
		got := &infravirtrigaudiov1beta1.VMClone{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(clone), got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhasePending))
		ready := readyCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(ReasonTargetNamespaceNotAllowed))
		Expect(ready.ObservedGeneration).To(Equal(got.Generation))

		By("still refusing when the grant lists only other namespaces")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tgt), tgt)).To(Succeed())
		tgt.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: "team-z"}
		Expect(k8sClient.Update(ctx, tgt)).To(Succeed())
		reconcileN(2)
		Expect(cp.cloneCnt).To(BeZero())

		By("proceeding once the target namespace lists the clone's namespace")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(tgt), tgt)).To(Succeed())
		tgt.Annotations[AllowedSourceNamespacesAnnotation] = "team-z, " + src.Name
		Expect(k8sClient.Update(ctx, tgt)).To(Succeed())
		reconcileN(2)
		Expect(cp.cloneCnt).To(Equal(1))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(clone), got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhaseReady))

		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: tgt.Name, Name: "web"}, vm)).To(Succeed())
		Expect(vm.Status.ID).To(Equal(tgt.Name + ".web"))
		Expect(vm.Spec.ProviderRef).To(Equal(infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1", Namespace: src.Name}))
	})
})
