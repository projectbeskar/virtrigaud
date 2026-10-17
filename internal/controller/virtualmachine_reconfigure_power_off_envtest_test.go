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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// This spec proves against a real API server how a CPU/memory change the
// running VM cannot take (rc1 lab finding V1: vSphere without hot-add) is
// recorded: Reconfiguring=False/ResizePendingPowerOff is persisted with its
// observedGeneration, a repeated refusal writes nothing, and powering the VM
// off (spec.powerState: Off) applies the change.
var _ = Describe("Reconfigure pending a power-off (envtest)", func() {
	It("persists ResizePendingPowerOff, writes nothing on a repeat, and applies the change once the VM is off", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "resize-off-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: prov.Name},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		removeVMFinalizersOnCleanup(vm)
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}

		rp := &routingProvider{onCreate: func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
			return contracts.CreateResponse{ID: req.Owner.Namespace + "." + req.Name}, nil
		}}
		r := &VirtualMachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: record.NewFakeRecorder(100),
		}
		get := func() *infravirtrigaudiov1beta1.VirtualMachine {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
			return got
		}
		update := func(mutate func(*infravirtrigaudiov1beta1.VirtualMachine)) {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				fresh := get()
				mutate(fresh)
				return k8sClient.Update(ctx, fresh)
			})).To(Succeed())
		}

		_, err := r.Reconcile(ctx, req) // adds the finalizer
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req) // creates the VM
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.createReqs).To(HaveLen(1))
		rp.describeResp = contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.5"}}
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		created := get()
		Expect(created.Status.CurrentResources).NotTo(BeNil())
		Expect(*created.Status.CurrentResources.CPU).To(Equal(int32(2)))

		// The owner asks for 4 vCPUs; the running VM cannot take them.
		update(func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: i32p(4)}
		})
		rp.reconfigureErr = needsPowerOffErr()
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.reconfigureRefs).To(HaveLen(1))
		held := get()
		c := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReconfiguring)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(k8s.ReasonResizePendingPowerOff))
		Expect(c.Message).To(Equal(resizePendingPowerOffMessage))
		Expect(c.ObservedGeneration).To(Equal(held.Generation))
		Expect(*held.Status.CurrentResources.CPU).To(Equal(int32(2)), "nothing was changed")

		// A reconcile within the backoff sends nothing and leaves the
		// condition as it is. (It brings Ready's observedGeneration up to the
		// spec change, as for any failed Reconfigure; after that, a further
		// reconcile writes nothing at all.)
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.reconfigureRefs).To(HaveLen(1))
		again := get()
		Expect(*meta.FindStatusCondition(again.Status.Conditions, k8s.ConditionReconfiguring)).To(Equal(*c))
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.reconfigureRefs).To(HaveLen(1))
		Expect(get().ResourceVersion).To(Equal(again.ResourceVersion), "a repeated hold does not write the VM")

		// The owner powers the VM off: the change is sent at once and applied.
		update(func(v *infravirtrigaudiov1beta1.VirtualMachine) {
			v.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
		})
		rp.describeResp = contracts.DescribeResponse{Exists: true, PowerState: "Off"}
		rp.reconfigureErr = nil
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.reconfigureRefs).To(HaveLen(2))
		applied := get()
		Expect(*applied.Status.CurrentResources.CPU).To(Equal(int32(4)))
		c = meta.FindStatusCondition(applied.Status.Conditions, k8s.ConditionReconfiguring)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal(k8s.ReasonReconcileSuccess))
		Expect(c.ObservedGeneration).To(Equal(applied.Generation))
	})
})
