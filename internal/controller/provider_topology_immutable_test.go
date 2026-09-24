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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These envtest specs pin the CRD half of Provider.spec.topology immutability
// (ADR-0007 Addendum A): the x-kubernetes-validations transition rule
// `self == oldSelf` rejects any topology flip at the apiserver, while a
// Provider created without the field (defaulted to "single") keeps accepting
// ordinary updates — including a client that omits the field on update.
var _ = Describe("Provider spec.topology immutability (CRD CEL rule)", func() {
	ctx := context.Background()

	newProvider := func(name, topology string) *infravirtrigaudiov1beta1.Provider {
		return &infravirtrigaudiov1beta1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: infravirtrigaudiov1beta1.ProviderSpec{
				Type:                infravirtrigaudiov1beta1.ProviderTypeLibvirt,
				Topology:            topology,
				Endpoint:            "qemu+ssh://virt@kvm-01/system",
				CredentialSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: "creds"},
				Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeSpec{
					Mode:  infravirtrigaudiov1beta1.RuntimeModeRemote,
					Image: "virtrigaud/provider-libvirt:test",
				},
			},
		}
	}
	get := func(name string) *infravirtrigaudiov1beta1.Provider {
		p := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, p)).To(Succeed())
		return p
	}
	expectImmutable := func(err error) {
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.topology is immutable"))
	}

	It("defaults an omitted topology to single and still accepts ordinary updates", func() {
		p := newProvider("topo-unset", "")
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, p) })

		got := get("topo-unset")
		Expect(got.Spec.Topology).To(Equal(infravirtrigaudiov1beta1.ProviderTopologySingle))

		got.Spec.Endpoint = "qemu+ssh://virt@kvm-02/system"
		Expect(k8sClient.Update(ctx, got)).To(Succeed(), "an update that leaves topology alone is allowed")

		// A client that omits topology entirely (merge patch removing it) is
		// re-defaulted to "single" and stays allowed.
		Expect(k8sClient.Patch(ctx, get("topo-unset"),
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"topology":null}}`)))).To(Succeed())
		Expect(get("topo-unset").Spec.Topology).To(Equal(infravirtrigaudiov1beta1.ProviderTopologySingle))

		flip := get("topo-unset")
		flip.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		expectImmutable(k8sClient.Update(ctx, flip))
	})

	It("rejects cluster -> single, including by omitting the field", func() {
		p := newProvider("topo-cluster", infravirtrigaudiov1beta1.ProviderTopologyCluster)
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, p) })

		got := get("topo-cluster")
		got.Spec.Endpoint = "qemu+ssh://virt@kvm-03/system"
		Expect(k8sClient.Update(ctx, got)).To(Succeed(), "a clustered Provider stays updatable")

		flip := get("topo-cluster")
		flip.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologySingle
		expectImmutable(k8sClient.Update(ctx, flip))

		expectImmutable(k8sClient.Patch(ctx, get("topo-cluster"),
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"topology":null}}`))))
		Expect(get("topo-cluster").Spec.Topology).To(Equal(infravirtrigaudiov1beta1.ProviderTopologyCluster))
	})
})
