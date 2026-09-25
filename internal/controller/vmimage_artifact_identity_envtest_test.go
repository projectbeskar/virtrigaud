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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/imageartifact"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These specs run the manager side of ADR-0009 (Slice 2) against a real API
// server with the generated CRDs and the mock provider over gRPC: the source
// digest the provider's stamp echo confirms is recorded (and accepted by the
// CRD's pattern), a VMImage deleted and re-created under the same name gets a
// new artifact and never inherits its predecessor's, and an entry an earlier
// release recorded without a digest holds creates under onMissing Wait.
var _ = Describe("VMImage prepared-artifact identity in the manager (envtest)", func() {
	ctx := context.Background()

	newNamespace := func() *corev1.Namespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "img-artifact-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		return ns
	}
	// identityProviderEnv creates Provider ns/mock and returns it (with its
	// API-server UID) advertising image import and artifact identity in
	// memory, as the Provider controller would report them.
	identityProviderEnv := func(ns string) *infravirtrigaudiov1beta1.Provider {
		p := envtestProvider(ns, "mock", nil)
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		p.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{
			SupportsImageImport:           true,
			SupportsImageArtifactIdentity: true,
		}
		return p
	}
	newImage := func(ns string, onMissing infravirtrigaudiov1beta1.ImageMissingAction) *infravirtrigaudiov1beta1.VMImage {
		img := &infravirtrigaudiov1beta1.VMImage{
			ObjectMeta: metav1.ObjectMeta{Name: "ubuntu", Namespace: ns},
			Spec: infravirtrigaudiov1beta1.VMImageSpec{Source: infravirtrigaudiov1beta1.ImageSource{
				HTTP: &infravirtrigaudiov1beta1.HTTPImageSource{URL: "https://images.example.com/jammy.qcow2"},
			}},
		}
		if onMissing != "" {
			img.Spec.Prepare = &infravirtrigaudiov1beta1.ImagePrepare{OnMissing: onMissing}
		}
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		return img
	}
	reload := func(img *infravirtrigaudiov1beta1.VMImage) *infravirtrigaudiov1beta1.VMImage {
		got := &infravirtrigaudiov1beta1.VMImage{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(img), got)).To(Succeed())
		return got
	}
	vmFor := func(p *infravirtrigaudiov1beta1.Provider, img *infravirtrigaudiov1beta1.VMImage) *infravirtrigaudiov1beta1.VirtualMachine {
		return &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: p.Namespace},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: p.Name},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImageRef:    &infravirtrigaudiov1beta1.ObjectRef{Name: img.Name},
			},
		}
	}
	derivedName := func(img *infravirtrigaudiov1beta1.VMImage) string {
		digest, err := imageartifact.SourceDigest(img.Spec.Source)
		Expect(err).NotTo(HaveOccurred())
		name, err := imageartifact.ArtifactName(imageartifact.NameRuleLibvirt,
			contracts.ObjectIdentity{UID: string(img.UID), Namespace: img.Namespace, Name: img.Name}, digest)
		Expect(err).NotTo(HaveOccurred())
		return name
	}
	reconciler := func() *VirtualMachineReconciler {
		return &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(10)}
	}

	It("records the confirmed source digest, and a re-created VMImage gets a new artifact", func() {
		_, cli, stop, err := serveMockImageProvider(0)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stop)
		ns := newNamespace()
		provider := identityProviderEnv(ns.Name)
		key := imageProviderKey(provider)
		img := newImage(ns.Name, "")
		r := reconciler()

		requeue, err := r.EnsureImageOnProvider(ctx, vmFor(provider, img), reload(img), provider, cli)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())
		first := reload(img)
		digest, err := imageartifact.SourceDigest(first.Spec.Source)
		Expect(err).NotTo(HaveOccurred())
		entry := first.Status.ProviderStatus[key]
		Expect(entry.Available).To(BeTrue())
		Expect(entry.SourceDigest).To(Equal(digest), "the CRD keeps the digest the stamp echo confirmed")
		Expect(entry.ID).To(Equal(derivedName(first)))

		// Delete the VMImage and create it again under the same name.
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		again := newImage(ns.Name, "")
		Expect(again.UID).NotTo(Equal(first.UID))

		// A reconcile that read the deleted object before its prepare was
		// recorded neither prepares nor records anything for its successor.
		stale := first.DeepCopy()
		stale.Status = infravirtrigaudiov1beta1.VMImageStatus{}
		_, err = r.EnsureImageOnProvider(ctx, vmFor(provider, img), stale, provider, cli)
		Expect(err).To(MatchError(ContainSubstring("re-created")))
		Expect(reload(again).Status.ProviderStatus).To(BeEmpty())

		requeue, err = r.EnsureImageOnProvider(ctx, vmFor(provider, again), reload(again), provider, cli)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())
		recreated := reload(again).Status.ProviderStatus[key]
		Expect(recreated.Available).To(BeTrue())
		Expect(recreated.ID).To(Equal(derivedName(reload(again))))
		Expect(recreated.ID).NotTo(Equal(entry.ID), "a re-created VMImage never inherits its predecessor's artifact")
	})

	It("holds creates with SourceDigestMissing under onMissing Wait for an entry recorded without a digest", func() {
		ns := newNamespace()
		provider := identityProviderEnv(ns.Name)
		key := imageProviderKey(provider)
		img := newImage(ns.Name, infravirtrigaudiov1beta1.ImageMissingActionWait)
		legacy := reload(img)
		legacy.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{
			key: {Available: true, ProviderUID: string(provider.UID), ID: "ubuntu"},
		}
		legacy.Status.AvailableOn = []string{key}
		Expect(k8sClient.Status().Update(ctx, legacy)).To(Succeed())
		inst := &preparerProvider{}

		_, err := reconciler().EnsureImageOnProvider(ctx, vmFor(provider, img), reload(img), provider, inst)
		Expect(err).To(MatchError(errImagePrepareHold))
		Expect(inst.calls()).To(BeZero())
		got := reload(img)
		cond := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMImageConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(imageReasonSourceDigestMissing))
		Expect(cond.ObservedGeneration).To(Equal(got.Generation))
		Expect(got.Status.ProviderStatus[key].SourceDigest).To(BeEmpty(), "nothing is adopted")
	})
})
