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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These specs run the single-host reconcile against a real API server and pin
// that status.phase (the PHASE printer column) is never empty on a VM the
// controller manages: a created VM reaches Provisioning and then Running, the
// phase follows its power state, a delete shows Deleting, and a reconcile that
// observes nothing new writes nothing (the phase is deterministic, so it never
// causes a status write on its own).
var _ = Describe("VirtualMachine status.phase (envtest)", func() {
	It("is set from the first status write, follows the power state and the delete, and never churns", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "vm-phase-"}}
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
		DeferCleanup(func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				got := &infravirtrigaudiov1beta1.VirtualMachine{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), got); err != nil {
					return client.IgnoreNotFound(err)
				}
				got.Finalizers = nil
				return k8sClient.Update(ctx, got)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vm))).To(Succeed())
		})

		rp := &routingProvider{onCreate: func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
			return contracts.CreateResponse{ID: req.Owner.Namespace + "." + req.Name}, nil
		}}
		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: record.NewFakeRecorder(100)}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		get := func() *infravirtrigaudiov1beta1.VirtualMachine {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
			return got
		}
		reconcileN := func(n int) {
			for i := 0; i < n; i++ {
				_, err := r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
			}
		}

		By("a created VM has a phase as soon as the Create is recorded")
		reconcileN(2) // finalizer; marker, Create, record
		Expect(rp.createReqs).To(HaveLen(1))
		got := get()
		Expect(got.Status.ID).NotTo(BeEmpty())
		Expect(got.Status.Phase).NotTo(BeEmpty())
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseProvisioning),
			"created, not described yet")

		By("a described, powered-on VM is Running")
		rp.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn), IPs: []string{"10.0.0.5"}}
		reconcileN(1)
		Expect(get().Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseRunning))

		By("an unchanged reconcile writes nothing")
		rv := get().ResourceVersion
		reconcileN(3)
		Expect(get().ResourceVersion).To(Equal(rv), "the derived phase causes no status write")

		By("a powered-off VM is Stopped")
		rp.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := get()
			cur.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
			return k8sClient.Update(ctx, cur)
		})).To(Succeed())
		reconcileN(2)
		Expect(get().Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseStopped))
		rv = get().ResourceVersion
		reconcileN(3)
		Expect(get().ResourceVersion).To(Equal(rv), "no churn while stopped")

		By("a delete shows Deleting while the provider's delete is retried")
		rp.deleteErr = errors.New("destroy failed")
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		reconcileN(1)
		got = get()
		Expect(got.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting))
		Expect(got.Status.ID).NotTo(BeEmpty(), "the patch changed the phase only")
		rv = got.ResourceVersion
		reconcileN(2)
		Expect(get().ResourceVersion).To(Equal(rv), "the Deleting phase is written once")
	})
})
