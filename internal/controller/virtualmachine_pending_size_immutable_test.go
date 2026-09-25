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
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// pendingSizeImmutableMessage is the fixed part of the CRD rule's message.
const pendingSizeImmutableMessage = "spec.classRef and spec.resources are immutable while a clustered create is pending"

// These envtest specs pin the CRD rule that freezes a VM's size while its
// clustered create is pending (review M2b): the scheduler admitted it at that
// size, and every other schedule counts it at that size until it is created.
var _ = Describe("VirtualMachine size immutability while a clustered create is pending (CRD CEL rule)", func() {
	ctx := context.Background()
	var ns string

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "vm-pending-size-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, n) })
	})

	newVM := func(name string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				Resources:   &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(2)},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		return vm
	}
	get := func(name string) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vm)).To(Succeed())
		return vm
	}
	setStatus := func(name, pending, id string) {
		vm := get(name)
		vm.Status.ID = id
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: pending}
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
	}
	expectLocked := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring(pendingSizeImmutableMessage))
	}
	grow := func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(64)}
	}

	It("allows resizing an unscheduled VM", func() {
		newVM("unscheduled")
		vm := get("unscheduled")
		grow(vm)
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())
	})

	It("locks spec.resources and spec.classRef while the create is pending", func() {
		newVM("pending")
		setStatus("pending", "host-alpha", "")

		vm := get("pending")
		grow(vm)
		expectLocked(k8sClient.Update(ctx, vm))

		vm = get("pending")
		vm.Spec.Resources = nil
		expectLocked(k8sClient.Update(ctx, vm))

		vm = get("pending")
		vm.Spec.ClassRef.Name = "huge"
		expectLocked(k8sClient.Update(ctx, vm))

		vm = get("pending")
		vm.Labels = map[string]string{"app": "web"}
		Expect(k8sClient.Update(ctx, vm)).To(Succeed(), "any other edit is allowed")
	})

	It("does not unlock through the status subresource", func() {
		newVM("status-path")
		setStatus("status-path", "host-alpha", "")
		vm := get("status-path")
		grow(vm) // a status update carries the stored spec: the spec change is dropped, not applied
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseProvisioning
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		Expect(*get("status-path").Spec.Resources.CPU).To(Equal(int32(2)))
	})

	It("unlocks once the VM is created (status.id set)", func() {
		newVM("created")
		setStatus("created", "host-alpha", "vm-1")
		vm := get("created")
		grow(vm)
		Expect(k8sClient.Update(ctx, vm)).To(Succeed(), "a created VM's resize is admitted by the controller instead")
	})
})
