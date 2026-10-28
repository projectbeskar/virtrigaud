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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// This spec proves that status.placement.resizeReservation survives a real
// API server's status update through the generated CRD schema (an outdated
// schema would prune it, and the resize gate would then fail closed).

var _ = Describe("Durable resize reservation (envtest)", func() {
	It("persists status.placement.resizeReservation through the CRD schema and admits the resize", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "resize-res-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: "prov"},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		removeVMFinalizersOnCleanup(vm)
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-a"}
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())

		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		footprint := scheduler.ResourceRequest{CPU: 4, MemoryMiB: 8192}
		token, ok := r.placementAssumptions().AssumeFor("ns/prov", assume.Assumption{
			UID: vmSchedulingUID(vm), Namespace: vm.Namespace, Name: vm.Name, HostID: "host-a",
			Resources: footprint, Resize: true,
		}, resizeAssumeTTL)
		Expect(ok).To(BeTrue())

		_, admitted, err := r.recordResizeReservation(ctx, vm, resizeAdmission{token: token, footprint: footprint})
		Expect(err).NotTo(HaveOccurred())
		Expect(admitted).To(BeTrue(), "the API server's answer carries the reservation")

		got := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got)).To(Succeed())
		Expect(got.Status.Placement).NotTo(BeNil())
		Expect(got.Status.Placement.ResizeReservation).To(Equal(&infravirtrigaudiov1beta1.PlacementResources{CPU: 4, MemoryMiB: 8192}))
		a, _, found := r.placementAssumptions().Lookup(vmSchedulingUID(vm))
		Expect(found).To(BeTrue())
		Expect(a.Write).To(Equal(assume.WriteStored))
	})
})
