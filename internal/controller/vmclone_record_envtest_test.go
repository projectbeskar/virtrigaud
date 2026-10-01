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
	stderrors "errors"
	"net"
	"os"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These envtest specs pin, against a real API server (whose failed JSON Patch
// "test" is a 422 Invalid), that the record of an accepted clone lands only on
// the VMClone the reconcile read: never on a VMClone deleted and re-created
// under the same name while the provider copied, and never over a stored
// Failed.

// newRecordTestClone is a single-host clone of src-vm into "web" in ns.
func newRecordTestClone(ns string) *infravirtrigaudiov1beta1.VMClone {
	return &infravirtrigaudiov1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-r", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VMCloneSpec{
			Source: infravirtrigaudiov1beta1.CloneSource{VMRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infravirtrigaudiov1beta1.VMCloneTarget{Name: "web"},
		},
	}
}

var _ = Describe("The record of an accepted clone (envtest)", func() {
	var ns *corev1.Namespace
	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "clone-record-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, holdTestProvider(ns.Name))).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		holdTestVM(ns.Name, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOff)
	})

	It("is never written onto a VMClone re-created under the same name during the copy, and nothing is bound", func() {
		clone := newRecordTestClone(ns.Name)
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())
		key := client.ObjectKeyFromObject(clone)
		cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: ns.Name + ".web"}}
		r := &VMCloneReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			RemoteResolver: &stubResolver{provider: cp}, Recorder: record.NewFakeRecorder(100)}

		var recreatedUID string
		cp.onClone = func() {
			defer GinkgoRecover()
			old := &infravirtrigaudiov1beta1.VMClone{}
			Expect(k8sClient.Get(ctx, key, old)).To(Succeed())
			old.Finalizers = nil
			Expect(k8sClient.Update(ctx, old)).To(Succeed())
			Expect(k8sClient.Delete(ctx, old)).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &infravirtrigaudiov1beta1.VMClone{}))
			}, "5s", "50ms").Should(BeTrue())
			fresh := newRecordTestClone(ns.Name)
			Expect(k8sClient.Create(ctx, fresh)).To(Succeed())
			recreatedUID = string(fresh.UID)
		}

		for i := 0; i < 3 && cp.cloneCnt == 0; i++ {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(cp.cloneCnt).To(Equal(1))

		got := &infravirtrigaudiov1beta1.VMClone{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(string(got.UID)).To(Equal(recreatedUID))
		Expect(got.Status.TargetVMID).To(BeEmpty(), "the old clone's record never lands on the new VMClone")
		Expect(got.Status.Phase).To(BeEmpty())
		Expect(got.Status.StartTime).To(BeNil())
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "web"}, &infravirtrigaudiov1beta1.VirtualMachine{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing is bound")
	})

	It("never overwrites a stored Failed", func() {
		clone := newRecordTestClone(ns.Name)
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())
		clone.Status.Phase = infravirtrigaudiov1beta1.ClonePhasePending
		Expect(k8sClient.Status().Update(ctx, clone)).To(Succeed())
		before := clone.DeepCopy()

		// Meanwhile the clone was stored Failed.
		failed := clone.DeepCopy()
		failed.Status.Phase = infravirtrigaudiov1beta1.ClonePhaseFailed
		failed.Status.Message = "clone failed"
		Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())

		r := &VMCloneReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(10)}
		markCloneStarted(clone, false, metav1.Now(), cloneStartedMessage)
		clone.Status.TargetVMID = ns.Name + ".web"
		err := r.persistCloneStatus(ctx, clone, before)
		Expect(stderrors.Is(err, errCloneChanged)).To(BeTrue(), "%v", err)

		got := &infravirtrigaudiov1beta1.VMClone{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(clone), got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhaseFailed))
		Expect(got.Status.TargetVMID).To(BeEmpty())
	})

	It("binds a clone whose first record was applied but answered with a connection reset, without a second Clone", func() {
		clone := newRecordTestClone(ns.Name)
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())
		clone.Status.Phase = infravirtrigaudiov1beta1.ClonePhasePending
		Expect(k8sClient.Status().Update(ctx, clone)).To(Succeed())
		key := client.ObjectKeyFromObject(clone)

		cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: ns.Name + ".web"}}
		rec := record.NewFakeRecorder(100)
		r := &VMCloneReconciler{Client: &applyThenResetOnce{Client: k8sClient}, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			RemoteResolver: &stubResolver{provider: cp}, Recorder: rec}

		// Finalizer, then the Clone: its record is applied, the answer lost,
		// and the retry's own phase test fails (422) — errCloneChanged.
		var res ctrl.Result
		for i := 0; i < 3 && cp.cloneCnt == 0; i++ {
			var err error
			res, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(cp.cloneCnt).To(Equal(1))
		Expect(res.RequeueAfter).To(Equal(cloneStaleReadRetryInterval), "never an empty result: the next pass re-reads the clone")

		// The requeue binds the clone through its recorded target VM ID.
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		got := &infravirtrigaudiov1beta1.VMClone{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhaseReady))
		Expect(cp.cloneCnt).To(Equal(1), "no second Clone")
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "web"}, vm)).To(Succeed())
		Expect(vm.Status.ID).To(Equal(ns.Name+".web"), "bound to the clone that was made")
		for len(rec.Events) > 0 {
			Expect(<-rec.Events).NotTo(ContainSubstring(cloneReasonRecordNotSaved))
		}
	})

	It("lands on the VMClone it was made for despite a metadata edit", func() {
		clone := newRecordTestClone(ns.Name)
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())
		clone.Status.Phase = infravirtrigaudiov1beta1.ClonePhasePending
		Expect(k8sClient.Status().Update(ctx, clone)).To(Succeed())
		before := clone.DeepCopy()

		edited := clone.DeepCopy()
		edited.Annotations = map[string]string{"example.com/poke": "1"}
		Expect(k8sClient.Update(ctx, edited)).To(Succeed())

		r := &VMCloneReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), Recorder: record.NewFakeRecorder(10)}
		markCloneStarted(clone, false, metav1.Now(), cloneStartedMessage)
		clone.Status.TargetVMID = ns.Name + ".web"
		Expect(r.persistCloneStatus(ctx, clone, before)).To(Succeed())

		got := &infravirtrigaudiov1beta1.VMClone{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(clone), got)).To(Succeed())
		Expect(got.Status.TargetVMID).To(Equal(ns.Name + ".web"))
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhaseCloning))
		Expect(got.Annotations).To(HaveKeyWithValue("example.com/poke", "1"), "the concurrent edit is kept")
	})
})

// applyThenResetOnce applies the first VMClone status patch and then answers
// it with a connection reset (the API server applied it; the answer was lost),
// as a dropped connection does.
type applyThenResetOnce struct {
	client.Client
	done bool
}

// Status returns the status writer that loses the first answer.
func (c *applyThenResetOnce) Status() client.SubResourceWriter {
	return &applyThenResetWriter{SubResourceWriter: c.Client.Status(), c: c}
}

// applyThenResetWriter is applyThenResetOnce's status writer.
type applyThenResetWriter struct {
	client.SubResourceWriter
	c *applyThenResetOnce
}

// Patch applies the patch; the first VMClone patch's answer is a reset.
func (w *applyThenResetWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	err := w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
	if _, isClone := obj.(*infravirtrigaudiov1beta1.VMClone); isClone && !w.c.done && err == nil {
		w.c.done = true
		return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	}
	return err
}
