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

// editingDeleteProvider is an asyncDeleteProvider whose Delete edits the
// stored VirtualMachine while it runs, so the object the reconcile read is
// stale (another resourceVersion) when the task is recorded.
type editingDeleteProvider struct {
	asyncDeleteProvider
	key client.ObjectKey
}

func (p *editingDeleteProvider) Delete(ctx context.Context, vm contracts.VMRef) (string, error) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &infravirtrigaudiov1beta1.VirtualMachine{}
		if err := k8sClient.Get(ctx, p.key, fresh); err != nil {
			return err
		}
		fresh.Labels = map[string]string{"example.com/edited": "during-delete"}
		return k8sClient.Update(ctx, fresh)
	})).To(Succeed())
	return p.asyncDeleteProvider.Delete(ctx, vm)
}

var _ = Describe("Asynchronous provider Delete (envtest)", func() {
	It("keeps the finalizer until the task completes, across a resourceVersion race and a manager restart", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "async-delete-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())

		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name,
				Finalizers: []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: prov.Name},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2",
					Source: "manual"},
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
		})
		vm.Status.ID = "vm-web-1"
		vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
		Expect(k8sClient.Delete(ctx, vm)).To(Succeed())

		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		p := &editingDeleteProvider{key: req.NamespacedName}
		p.deleteTask = "task-77"
		p.answer(contracts.TaskStatus{IsCompleted: false}, nil)
		reconciler := func() *VirtualMachineReconciler {
			return &VirtualMachineReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
				RemoteResolver: &stubResolver{provider: p}, Recorder: record.NewFakeRecorder(100),
			}
		}
		get := func() *infravirtrigaudiov1beta1.VirtualMachine {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			if err := k8sClient.Get(ctx, req.NamespacedName, got); err != nil {
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%v", err)
				return nil
			}
			return got
		}

		r := reconciler()
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(vmDeleteTaskPollMin))
		got := get()
		Expect(got).NotTo(BeNil())
		Expect(got.Finalizers).To(ContainElement(infravirtrigaudiov1beta1.VirtualMachineFinalizer))
		Expect(got.Labels).To(HaveKeyWithValue("example.com/edited", "during-delete"), "the concurrent edit is kept")
		Expect(got.Status.LastTaskRef).To(Equal("task-77"), "the record lands although the read object was stale")
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, k8s.ConditionDeleteTask)).To(BeTrue())
		Expect(got.Status.ID).To(Equal("vm-web-1"))
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.VirtualMachinePhaseDeleting))
		rv := got.ResourceVersion

		// Polls of the running task neither send Delete nor write the status.
		for range 3 {
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(get().ResourceVersion).To(Equal(rv), "an unchanged poll writes nothing")
		Expect(p.deleteCalls.Load()).To(BeEquivalentTo(1))
		Expect(p.statusCalls.Load()).To(BeEquivalentTo(3))

		// A manager restart: a new reconciler resumes the task from the API server.
		restarted := reconciler()
		_, err = restarted.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(get()).NotTo(BeNil())
		Expect(p.deleteCalls.Load()).To(BeEquivalentTo(1), "no Delete after a restart")

		p.answer(contracts.TaskStatus{IsCompleted: true}, nil)
		_, err = restarted.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(get()).To(BeNil(), "the finalizer is removed once the task completed")
		Expect(p.deleteCalls.Load()).To(BeEquivalentTo(1))
		Expect(p.statusCalls.Load()).To(BeEquivalentTo(5))
	})
})
