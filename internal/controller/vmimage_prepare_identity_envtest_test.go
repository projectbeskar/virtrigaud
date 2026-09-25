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

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These specs run the per-Provider-identity VMImage prepare state against a
// real API server with the generated CRDs: the "<namespace>/<name>" keys and
// each entry's providerUID and taskRef survive the round trip (an older CRD
// would prune them), and bare-name state from an earlier release is migrated.
var _ = Describe("VMImage prepare state by Provider identity (envtest)", func() {
	ctx := context.Background()

	newNamespace := func(prefix string) *corev1.Namespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		return ns
	}
	// importingProvider creates Provider ns/name and returns it (with its
	// API-server UID) advertising image import in memory.
	importingProvider := func(ns, name string) *infravirtrigaudiov1beta1.Provider {
		p := envtestProvider(ns, name, nil)
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		Expect(p.UID).NotTo(BeEmpty())
		p.Status.ReportedCapabilities = &infravirtrigaudiov1beta1.ReportedCapabilities{
			SupportsImageImport:           true,
			SupportsImageArtifactIdentity: true,
		}
		return p
	}
	sharedImage := func(ns, name string) *infravirtrigaudiov1beta1.VMImage {
		img := &infravirtrigaudiov1beta1.VMImage{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: infravirtrigaudiov1beta1.VMImageSpec{
				Source: infravirtrigaudiov1beta1.ImageSource{
					HTTP: &infravirtrigaudiov1beta1.HTTPImageSource{URL: "https://images.example.com/jammy.qcow2"},
				},
				ConsumerNamespaceSelector: &metav1.LabelSelector{},
			},
		}
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		return img
	}
	vmUsingEnv := func(p *infravirtrigaudiov1beta1.Provider, img *infravirtrigaudiov1beta1.VMImage) *infravirtrigaudiov1beta1.VirtualMachine {
		return &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: p.Namespace},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: p.Name},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImageRef:    &infravirtrigaudiov1beta1.ObjectRef{Name: img.Name, Namespace: img.Namespace},
			},
		}
	}
	reload := func(img *infravirtrigaudiov1beta1.VMImage) *infravirtrigaudiov1beta1.VMImage {
		got := &infravirtrigaudiov1beta1.VMImage{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(img), got)).To(Succeed())
		return got
	}
	reconciler := func() *VirtualMachineReconciler {
		return &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(10)}
	}

	It("keeps independent entries, UIDs and tasks for same-named Providers in two namespaces", func() {
		owner := newNamespace("img-owner-")
		teamA := newNamespace("img-team-a-")
		teamB := newNamespace("img-team-b-")
		provA := importingProvider(teamA.Name, "vsphere")
		provB := importingProvider(teamB.Name, "vsphere")
		img := sharedImage(owner.Name, "ubuntu")
		r := reconciler()

		instB := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{TaskRef: "task-b", PreparedImageID: "ubuntu-b"}}
		requeue, err := r.EnsureImageOnProvider(ctx, vmUsingEnv(provB, img), reload(img), provB, instB)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeTrue())

		instA := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-a"}}
		requeue, err = r.EnsureImageOnProvider(ctx, vmUsingEnv(provA, img), reload(img), provA, instA)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())
		Expect(instA.calls()).To(Equal(1))

		got := reload(img)
		keyA, keyB := teamA.Name+"/vsphere", teamB.Name+"/vsphere"
		Expect(got.Status.ProviderStatus).To(HaveLen(2))
		Expect(got.Status.ProviderStatus[keyA].Available).To(BeTrue())
		Expect(got.Status.ProviderStatus[keyA].ProviderUID).To(Equal(string(provA.UID)))
		Expect(got.Status.ProviderStatus[keyA].ID).To(Equal("ubuntu-a"))
		Expect(got.Status.ProviderStatus[keyB].Available).To(BeFalse())
		Expect(got.Status.ProviderStatus[keyB].ProviderUID).To(Equal(string(provB.UID)))
		Expect(got.Status.ProviderStatus[keyB].TaskRef).To(Equal("task-b"), "the CRD keeps the per-Provider task")
		Expect(got.Status.AvailableOn).To(ConsistOf(keyA))
		Expect(got.Status.Ready).To(BeTrue(), "ready on team-a's Provider while team-b's prepare runs")
		Expect(got.Status.PrepareTaskRef).To(BeEmpty())
	})

	It("migrates bare-name state from an earlier release", func() {
		owner := newNamespace("img-legacy-")
		ownerProv := importingProvider(owner.Name, "vsphere")
		img := sharedImage(owner.Name, "ubuntu")

		// State as an earlier release wrote it.
		legacy := reload(img)
		legacy.Status.Ready = true
		legacy.Status.Phase = infravirtrigaudiov1beta1.ImagePhaseReady
		legacy.Status.PrepareTaskRef = "task-legacy"
		legacy.Status.ProviderStatus = map[string]infravirtrigaudiov1beta1.ProviderImageStatus{
			"vsphere": {Available: true, ID: "ubuntu-legacy"},
			"gone":    {Available: true, ID: "ubuntu-gone"},
		}
		legacy.Status.AvailableOn = []string{"vsphere", "gone"}
		Expect(k8sClient.Status().Update(ctx, legacy)).To(Succeed())

		inst := &preparerProvider{prepareResp: contracts.ImagePrepareResponse{PreparedImageID: "ubuntu-confirmed"}}
		requeue, err := reconciler().EnsureImageOnProvider(ctx, vmUsingEnv(ownerProv, img), reload(img), ownerProv, inst)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeFalse())
		Expect(inst.calls()).To(Equal(1), "the migrated entry is re-validated through its Provider")

		got := reload(img)
		key := owner.Name + "/vsphere"
		Expect(got.Status.ProviderStatus).To(HaveLen(1))
		Expect(got.Status.ProviderStatus).To(HaveKey(key))
		Expect(got.Status.ProviderStatus[key].ProviderUID).To(Equal(string(ownerProv.UID)))
		Expect(got.Status.ProviderStatus[key].ID).To(Equal("ubuntu-confirmed"))
		Expect(got.Status.AvailableOn).To(ConsistOf(key))
		Expect(got.Status.PrepareTaskRef).To(BeEmpty())
	})
})
