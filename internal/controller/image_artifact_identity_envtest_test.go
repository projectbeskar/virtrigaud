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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
)

// These specs pin the ADR-0009 D8 status fields against a real API server with
// the generated CRDs: VMImage status.providerStatus[].sourceDigest and Provider
// status.reportedCapabilities.supportsImageArtifactIdentity survive a round
// trip (an older CRD would prune them), a malformed digest is refused by the
// schema, and the source digest of a stored VMImage is stable across reads
// (the API server's defaults are part of what is hashed).
var _ = Describe("Prepared-image artifact identity status fields (envtest)", func() {
	ctx := context.Background()

	newNamespace := func() *corev1.Namespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "img-identity-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		return ns
	}

	It("round-trips providerStatus[].sourceDigest and refuses a malformed one", func() {
		ns := newNamespace()
		img := &infravirtrigaudiov1beta1.VMImage{
			ObjectMeta: metav1.ObjectMeta{Name: "ubuntu", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VMImageSpec{Source: infravirtrigaudiov1beta1.ImageSource{
				HTTP: &infravirtrigaudiov1beta1.HTTPImageSource{URL: "https://images.example.com/jammy.qcow2"},
			}},
		}
		Expect(k8sClient.Create(ctx, img)).To(Succeed())

		stored := &infravirtrigaudiov1beta1.VMImage{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(img), stored)).To(Succeed())
		digest, err := imageartifact.SourceDigest(stored.Spec.Source)
		Expect(err).NotTo(HaveOccurred())

		key := ns.Name + "/libvirt"
		stored.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{
			key: {Available: true, ProviderUID: "8c1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a", SourceDigest: digest},
		}
		Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())

		got := &infravirtrigaudiov1beta1.VMImage{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(img), got)).To(Succeed())
		Expect(got.Status.ProviderStatus[key].SourceDigest).To(Equal(digest), "the CRD keeps sourceDigest")
		again, err := imageartifact.SourceDigest(got.Spec.Source)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(digest), "the digest of a stored spec.source is stable across reads")

		bad := got.DeepCopy()
		entry := bad.Status.ProviderStatus[key]
		entry.SourceDigest = "sha256:NOT-A-DIGEST"
		bad.Status.ProviderStatus[key] = entry
		err = k8sClient.Status().Update(ctx, bad)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "a malformed sourceDigest is refused: %v", err)
	})

	It("round-trips reportedCapabilities.supportsImageArtifactIdentity", func() {
		ns := newNamespace()
		p := envtestProvider(ns.Name, "libvirt", nil)
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		p.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{
			SupportsImageImport:           true,
			SupportsImageArtifactIdentity: true,
		}
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

		got := &infravirtrigaudiov1beta1.Provider{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), got)).To(Succeed())
		Expect(got.Status.ReportedCapabilities).NotTo(BeNil())
		Expect(got.Status.ReportedCapabilities.SupportsImageArtifactIdentity).To(BeTrue(), "the CRD keeps the capability")
	})
})
