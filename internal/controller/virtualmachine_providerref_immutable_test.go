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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// providerRefImmutableMessage is the fixed part of the CRD rule's message.
const providerRefImmutableMessage = "spec.providerRef is immutable once the VirtualMachine is bound"

// These envtest specs pin the CRD half of VirtualMachine.spec.providerRef
// immutability against a real apiserver: the root-level transition rule locks
// providerRef (name and namespace) once the STORED object is bound
// (status.id or status.placement.pendingHost), whatever the request carries,
// and leaves every other update — and an unbound VM — alone.
var _ = Describe("VirtualMachine spec.providerRef immutability (CRD CEL rule)", func() {
	ctx := context.Background()
	var ns string

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "vm-pref-"}}
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
	bindID := func(name, id string) {
		vm := get(name)
		vm.Status.ID = id
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
	}
	expectLocked := func(err error) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring(providerRefImmutableMessage))
		Expect(err.Error()).To(ContainSubstring("spec.providerRef"), "the rule reports the field it guards")
	}

	It("installs the rule (the CRD was accepted, so its CEL cost is within budget)", func() {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "virtualmachines.infra.virtrigaud.io"}, crd)).To(Succeed())
		versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
		Expect(err).NotTo(HaveOccurred())
		Expect(versions).NotTo(BeEmpty())
		version, ok := versions[0].(map[string]any)
		Expect(ok).To(BeTrue())
		rules, found, err := unstructured.NestedSlice(version, "schema", "openAPIV3Schema", "x-kubernetes-validations")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "the root schema carries x-kubernetes-validations")
		var messages []string
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			Expect(ok).To(BeTrue())
			if m, ok := rule["message"].(string); ok {
				messages = append(messages, m)
			}
		}
		Expect(messages).To(ContainElement(ContainSubstring(providerRefImmutableMessage)))
	})

	It("allows changing providerRef while the VM is unbound (no status at all)", func() {
		newVM("unbound")

		vm := get("unbound")
		Expect(vm.Status.ID).To(BeEmpty())
		vm.Spec.ProviderRef.Name = "prov-b"
		Expect(k8sClient.Update(ctx, vm)).To(Succeed(), "a typo fix before binding is allowed")

		vm = get("unbound")
		vm.Spec.ProviderRef.Namespace = "other-ns"
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())
		Expect(get("unbound").Spec.ProviderRef).To(Equal(infravirtrigaudiov1beta1.ObjectRef{Name: "prov-b", Namespace: "other-ns"}))
	})

	It("allows changing providerRef when the stored status carries no binding", func() {
		newVM("status-no-id")
		vm := get("status-no-id")
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhasePending
		vm.Status.Message = "provider rejected create"
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())

		vm = get("status-no-id")
		vm.Spec.ProviderRef.Name = "prov-b"
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())
	})

	It("rejects any providerRef change once status.id is set", func() {
		newVM("bound")
		bindID("bound", "100")

		vm := get("bound")
		vm.Spec.ProviderRef.Name = "prov-b"
		expectLocked(k8sClient.Update(ctx, vm))

		vm = get("bound")
		vm.Spec.ProviderRef.Namespace = "other-ns"
		expectLocked(k8sClient.Update(ctx, vm))

		// Unset -> the VM's own namespace spelled out is still a change: the rule
		// cannot read metadata.namespace, so it compares the reference as written.
		vm = get("bound")
		vm.Spec.ProviderRef.Namespace = ns
		expectLocked(k8sClient.Update(ctx, vm))

		expectLocked(k8sClient.Patch(ctx, get("bound"),
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"providerRef":{"name":"prov-b"}}}`))))

		Expect(get("bound").Spec.ProviderRef).To(Equal(infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a"}))
	})

	It("rejects the change even when the request's status claims the VM is unbound", func() {
		newVM("stale-status")
		bindID("stale-status", "vm-42")

		// A main-resource update cannot write status: the apiserver validates the
		// request against the STORED status, so clearing status.id in the body
		// does not unlock providerRef.
		vm := get("stale-status")
		vm.Status = infravirtrigaudiov1beta1.VirtualMachineStatus{}
		vm.Spec.ProviderRef.Name = "prov-b"
		expectLocked(k8sClient.Update(ctx, vm))
		Expect(get("stale-status").Status.ID).To(Equal("vm-42"))
	})

	It("locks providerRef when a clustered create is pending (placement.pendingHost)", func() {
		newVM("pending")
		vm := get("pending")
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{PendingHost: "kvm-01", Pool: "pool-a"}
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())

		vm = get("pending")
		Expect(vm.Status.ID).To(BeEmpty())
		vm.Spec.ProviderRef.Name = "prov-b"
		expectLocked(k8sClient.Update(ctx, vm))

		// A placement without a pending host (e.g. only excludedHosts) does not lock it.
		vm = get("pending")
		vm.Status.Placement = &infravirtrigaudiov1beta1.PlacementStatus{ExcludedHosts: []string{"kvm-01"}}
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		vm = get("pending")
		vm.Spec.ProviderRef.Name = "prov-b"
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())
	})

	It("allows every update of a bound VM that leaves providerRef unchanged", func() {
		newVM("bound-edits")
		bindID("bound-edits", "vm-7")

		vm := get("bound-edits")
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
		vm.Spec.Tags = []string{"team-a"}
		Expect(k8sClient.Update(ctx, vm)).To(Succeed(), "spec edits other than providerRef are allowed")

		vm = get("bound-edits")
		vm.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
		vm.Labels = map[string]string{"app": "db"}
		Expect(k8sClient.Update(ctx, vm)).To(Succeed(), "metadata edits (the orphan-on-delete annotation) are allowed")

		Expect(k8sClient.Patch(ctx, get("bound-edits"),
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"providerRef":{"name":"prov-a"}}}`)))).To(Succeed(),
			"re-asserting the same providerRef is not a change")

		// Status writes (the operator's) carry the stored spec and pass.
		vm = get("bound-edits")
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
		vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: ns, Name: "prov-a", UID: "u-1"}
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		Expect(get("bound-edits").Status.BoundProvider).To(Equal(
			&infravirtrigaudiov1beta1.BoundProviderRef{Namespace: ns, Name: "prov-a", UID: "u-1"}))

		// A status update cannot sneak a spec change in either: the spec in the
		// body is ignored on the status subresource.
		vm = get("bound-edits")
		vm.Spec.ProviderRef.Name = "prov-b"
		vm.Status.Message = "ok"
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		Expect(get("bound-edits").Spec.ProviderRef.Name).To(Equal("prov-a"))
	})

	It("keeps an explicit namespace locked as written", func() {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "explicit-ns", Namespace: ns},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-a", Namespace: "infra"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		bindID("explicit-ns", "vm-9")

		// Removing the namespace (falling back to the VM's own) is a change.
		expectLocked(k8sClient.Patch(ctx, get("explicit-ns"),
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"providerRef":{"namespace":null}}}`))))

		vm = get("explicit-ns")
		vm.Spec.ClassRef.Name = "large"
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())
	})

	It("unlocks only once the operator unbinds the VM (status.id cleared through the status subresource)", func() {
		newVM("rebind")
		bindID("rebind", "vm-1")

		vm := get("rebind")
		vm.Spec.ProviderRef.Name = "prov-b"
		expectLocked(k8sClient.Update(ctx, vm))

		// Only a writer of the status subresource (the operator, or an
		// administrator) can unbind a VM.
		bindID("rebind", "")
		vm = get("rebind")
		vm.Spec.ProviderRef.Name = "prov-b"
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())
	})
})
