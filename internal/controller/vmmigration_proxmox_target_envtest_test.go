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
	"errors"

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

// This envtest spec drives a granted cross-namespace VMMigration into a
// Proxmox Provider through Reconcile against a real apiserver: it ends Failed
// at Validating with the constant ProxmoxCrossNamespaceTarget condition, and
// no provider is ever reached.
var _ = Describe("VMMigration into Proxmox in another namespace (envtest)", func() {
	ctx := context.Background()

	It("fails at Validating with a constant condition and no provider call", func() {
		src := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pxns-src-"}}
		tgt := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pxns-tgt-"}}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		Expect(k8sClient.Create(ctx, tgt)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, src)
			_ = k8sClient.Delete(ctx, tgt)
		})
		tgt.Annotations = map[string]string{AllowedSourceNamespacesAnnotation: src.Name}
		Expect(k8sClient.Update(ctx, tgt)).To(Succeed())

		remote := func(image string) *infravirtrigaudiov1beta1.ProviderRuntimeSpec {
			return &infravirtrigaudiov1beta1.ProviderRuntimeSpec{Mode: infravirtrigaudiov1beta1.RuntimeModeRemote, Image: image}
		}
		srcProv := &infravirtrigaudiov1beta1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "src-prov", Namespace: src.Name},
			Spec: infravirtrigaudiov1beta1.ProviderSpec{
				Type:                infravirtrigaudiov1beta1.ProviderTypeLibvirt,
				Endpoint:            "qemu+ssh://virt@kvm-01/system",
				CredentialSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: "creds"},
				Runtime:             remote("virtrigaud/provider-libvirt:test"),
			},
		}
		tgtProv := &infravirtrigaudiov1beta1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "tgt-prov", Namespace: src.Name},
			Spec: infravirtrigaudiov1beta1.ProviderSpec{
				Type:                      infravirtrigaudiov1beta1.ProviderTypeProxmox,
				Endpoint:                  "https://pve.example.com:8006",
				CredentialSecretRef:       infravirtrigaudiov1beta1.ObjectRef{Name: "creds"},
				Runtime:                   remote("virtrigaud/provider-proxmox:test"),
				ConsumerNamespaceSelector: selectNamespace(tgt.Name),
			},
		}
		Expect(k8sClient.Create(ctx, srcProv)).To(Succeed())
		Expect(k8sClient.Create(ctx, tgtProv)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(src.Name, "small", selectNamespace(tgt.Name)))).To(Succeed())

		srcVM := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: src.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "src-prov"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			},
		}
		Expect(k8sClient.Create(ctx, srcVM)).To(Succeed())
		srcVM.Status.ID = src.Name + ".src-vm"
		Expect(k8sClient.Status().Update(ctx, srcVM)).To(Succeed())

		migration := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "to-proxmox", Namespace: src.Name},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{
					Name:        "web",
					Namespace:   tgt.Name,
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "tgt-prov"},
					ClassRef:    &infravirtrigaudiov1beta1.LocalObjectReference{Name: "small"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, migration)).To(Succeed())

		rec := record.NewFakeRecorder(100)
		r := NewVMMigrationReconciler(k8sClient, k8sClient, k8sClient.Scheme(), nil, rec, false)
		providerCalls := 0
		r.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
			providerCalls++
			return nil, errors.New("no provider in this spec")
		}

		got := &infravirtrigaudiov1beta1.VMMigration{}
		for i := 0; i < 8; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(migration)})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(migration), got)).To(Succeed())
			if got.Status.Phase == infravirtrigaudiov1beta1.MigrationPhaseFailed {
				break
			}
		}

		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseFailed))
		Expect(got.Status.Message).To(Equal(proxmoxCrossNamespaceTargetMessage))
		validating := readyCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)
		Expect(validating).NotTo(BeNil())
		Expect(validating.Status).To(Equal(metav1.ConditionFalse))
		Expect(validating.Reason).To(Equal(ReasonProxmoxCrossNamespaceTarget))
		Expect(validating.Message).To(Equal(proxmoxCrossNamespaceTargetMessage))
		Expect(providerCalls).To(BeZero(), "no provider is reached")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: tgt.Name, Name: "web"},
			&infravirtrigaudiov1beta1.VirtualMachine{})).NotTo(Succeed(), "no target VM is created")
	})
})
