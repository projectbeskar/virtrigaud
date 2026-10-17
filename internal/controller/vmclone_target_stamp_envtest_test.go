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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// These envtest specs pin rc2 R4 against a real API server: a single-host
// VMClone creates its target before the Clone and sends the target's real
// UID; a Clone sent again after its record was lost, or after an unknown
// outcome, carries the same UID and is bound to the clone the first attempt
// made — and the CloneRequested / CloneOutcomeUnknown conditions are written
// with the clone's generation, once.

var _ = Describe("Single-host clone targets stamped with their owner (envtest)", func() {
	var ns *corev1.Namespace
	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "clone-stamp-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, holdTestProvider(ns.Name))).To(Succeed()) // libvirt: re-binds stamped clones
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		holdTestVM(ns.Name, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOff)
	})

	// reconcileUntil reconciles key until done reports true (at most n times).
	reconcileUntil := func(r *VMCloneReconciler, key client.ObjectKey, n int, done func() bool) {
		for i := 0; i < n && !done(); i++ {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	It("re-sends a Clone whose record was lost with the same target UID and binds the clone already made", func() {
		clone := newRecordTestClone(ns.Name)
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())
		key := client.ObjectKeyFromObject(clone)
		cp := &stampingCloner{}
		lost := &failingRecordClient{Client: k8sClient, failures: 1,
			err: apierrors.NewForbidden(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "vmclones/status"}, clone.Name,
				fmt.Errorf("denied"))}
		r := &VMCloneReconciler{Client: lost, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			RemoteResolver: &stubResolver{provider: cp}, Recorder: record.NewFakeRecorder(100)}

		// Finalizer, then the Clone: the provider makes it, its record is lost.
		var err error
		for i := 0; i < 3 && len(cp.reqs) == 0; i++ {
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		}
		Expect(err).To(HaveOccurred())
		Expect(cp.reqs).To(HaveLen(1))

		target := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "web"}, target)).To(Succeed())
		Expect(target.UID).NotTo(BeEmpty())
		Expect(target.Status.ID).To(BeEmpty())
		Expect(cp.reqs[0].TargetVM.UID).To(Equal(string(target.UID)), "the Clone carries the target's real UID")

		got := &infravirtrigaudiov1beta1.VMClone{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.TargetUID).To(Equal(string(target.UID)))
		Expect(got.Status.TargetVMID).To(BeEmpty(), "the record was lost")
		requested := meta.FindStatusCondition(got.Status.Conditions, cloneConditionRequested)
		Expect(requested).NotTo(BeNil(), "the requested Clone is on record")
		Expect(requested.Status).To(Equal(metav1.ConditionTrue))
		Expect(requested.ObservedGeneration).To(Equal(got.Generation))

		// The next reconciles send the Clone again: the provider re-binds.
		r.Client = k8sClient
		reconcileUntil(r, key, 4, func() bool {
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return got.Status.Phase == infravirtrigaudiov1beta1.ClonePhaseReady
		})
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhaseReady))
		Expect(meta.FindStatusCondition(got.Status.Conditions, cloneConditionRequested)).To(BeNil(), "the outcome is recorded")
		Expect(cp.reqs).To(HaveLen(2))
		Expect(cp.reqs[1].TargetVM).To(Equal(cp.reqs[0].TargetVM), "the re-send names the same target")
		Expect(cp.clones).To(HaveLen(1), "no second clone")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "web"}, target)).To(Succeed())
		Expect(target.Status.ID).To(Equal(ns.Name+".web"), "bound to the clone the first attempt made")
	})

	It("holds a Clone whose outcome is unknown with a constant condition, writes it once, then re-binds", func() {
		clone := newRecordTestClone(ns.Name)
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())
		key := client.ObjectKeyFromObject(clone)
		unknown := transportFailure("clone")
		cp := &stampingCloner{script: []cloneStep{{make: true, err: unknown}, {err: unknown}}}
		r := &VMCloneReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(),
			RemoteResolver: &stubResolver{provider: cp}, Recorder: record.NewFakeRecorder(100)}

		reconcileUntil(r, key, 3, func() bool { return len(cp.reqs) > 0 })
		Expect(cp.reqs).To(HaveLen(1))
		got := &infravirtrigaudiov1beta1.VMClone{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhasePending))
		ready := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(cloneReasonOutcomeUnknown))
		Expect(ready.Message).To(Equal(cloneOutcomeUnknownMessage))
		Expect(ready.ObservedGeneration).To(Equal(got.Generation))
		Expect(got.Status.Message).NotTo(ContainSubstring("10.96.12.7"), "no transport text in the status")

		// Before the backoff has passed, a reconcile sends nothing.
		rv := got.ResourceVersion
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(cp.reqs).To(HaveLen(1), "the re-send waits for its backoff")

		// The re-send fails the same way: nothing is written again.
		r.cloneAttempts.elapse(clone.UID)
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(cp.reqs).To(HaveLen(2))
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(got.ResourceVersion).To(Equal(rv), "a repeated hold writes nothing")

		// The provider answers: the third attempt re-binds the first one's clone.
		r.cloneAttempts.elapse(clone.UID)
		reconcileUntil(r, key, 3, func() bool {
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			return got.Status.Phase == infravirtrigaudiov1beta1.ClonePhaseReady
		})
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhaseReady))
		Expect(cp.clones).To(HaveLen(1), "one clone, whatever the number of attempts")
		target := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "web"}, target)).To(Succeed())
		Expect(target.Status.ID).To(Equal(ns.Name + ".web"))
		for _, req := range cp.reqs {
			Expect(req.TargetVM.UID).To(Equal(string(target.UID)))
		}
	})
})
