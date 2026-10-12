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
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// imageArtifactRefusal is the error the transport client maps a vSphere
// FailedPrecondition/VM_IS_IMAGE_ARTIFACT refusal to.
func imageArtifactRefusal(op string) error {
	return contracts.NewConflictError(op+": refused: the vSphere object is a template or an image artifact",
		fmt.Errorf("%w: rpc error: code = FailedPrecondition desc = refused", contracts.ErrVMIsImageArtifact))
}

// artifactEvents returns the VMIsImageArtifact events recorded so far.
func artifactEvents(rec *record.FakeRecorder) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, " "+k8s.ReasonVMIsImageArtifact+" ") {
			out = append(out, e)
		}
	}
	return out
}

func TestHandleDeletion_ImageArtifactRefusalIsHeld(t *testing.T) {
	ctx := context.Background()
	prov := &deleteStubProvider{err: imageArtifactRefusal("delete")}
	vm := deletionVM("vm-template")
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, deletionProviderCR())
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	marked := markForDeletion(t, r, vm)

	res, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin, "the blocked-VM backoff, not the 15 s failed-delete loop")
	assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMax)

	held := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held), "the finalizer is kept")
	assert.Contains(t, held.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonVMIsImageArtifact, ready.Reason)
	assert.Equal(t, imageArtifactDeleteMessage, ready.Message, "a constant message, never the provider's text")
	assert.Contains(t, ready.Message, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation)
	assert.Nil(t, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionDeleteBlocked),
		"a single-host VM never carries DeleteBlocked")
	assert.Len(t, drainEvents(rec), 1, "one Warning event when the hold begins")

	// A repeated refusal writes nothing and records no further event.
	rv := held.ResourceVersion
	_, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	again := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), again))
	assert.Equal(t, rv, again.ResourceVersion, "a repeated hold leaves the status unchanged")
	assert.Empty(t, drainEvents(rec))
	assert.EqualValues(t, 2, prov.calls.Load())

	// orphan-on-delete releases it without another provider call.
	again.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
	require.NoError(t, r.Update(ctx, again))
	_, err = r.handleDeletion(ctx, again)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
	assert.EqualValues(t, 2, prov.calls.Load())
}

// A Delete answered VM_NOT_OWNED releases the VirtualMachine (its own VM is
// gone; the VMID was reused) but records it: one constant VMLeftOnHypervisor
// Warning, never silent.
func TestHandleDeletion_VMNotOwnedReleasesWithAWarning(t *testing.T) {
	ctx := context.Background()
	notOwned := contracts.NewNotFoundError("delete: the VM at this VMID is not this VirtualMachine's",
		fmt.Errorf("%w: rpc error: code = NotFound", contracts.ErrVMNotOwned))
	prov := &deleteStubProvider{err: notOwned}
	vm := deletionVM("vm-restored")
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, deletionProviderCR())
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	marked := markForDeletion(t, r, vm)

	_, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
	events := drainEvents(rec)
	require.Len(t, events, 1)
	assert.Equal(t, "Warning "+eventReasonVMLeftOnHypervisor+" "+vmNotOwnedLeftMessage(marked), events[0])
	assert.Contains(t, events[0], `"100"`, "the event names the provider VM id")

	// A plain NotFound (the VM is gone) still releases silently.
	gone := deletionVM("vm-gone-silently")
	r2 := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: &deleteStubProvider{
		err: contracts.NewNotFoundError("delete: VM not found", nil)}}, gone, deletionProviderCR())
	rec2 := record.NewFakeRecorder(10)
	r2.Recorder = rec2
	_, err = r2.handleDeletion(ctx, markForDeletion(t, r2, gone))
	require.NoError(t, err)
	assert.Empty(t, drainEvents(rec2))
}

// A Delete refused because the VM's owner stamp cannot be read is held on a
// constant condition, one event and the blocked-VM backoff — not retried every
// 15 s as an ordinary failure.
func TestHandleDeletion_OwnerStampUnreadableIsHeld(t *testing.T) {
	ctx := context.Background()
	refusal := contracts.NewConflictError("delete: refused: the VM carries an owner stamp that cannot be read",
		fmt.Errorf("%w: rpc error: code = FailedPrecondition", contracts.ErrOwnerStampUnreadable))
	prov := &deleteStubProvider{err: refusal}
	vm := deletionVM("vm-stamp")
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, deletionProviderCR())
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	marked := markForDeletion(t, r, vm)

	res, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin)
	held := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held))
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonOwnerStampUnreadable, ready.Reason)
	assert.Equal(t, ownerStampUnreadableDeleteMessage, ready.Message)
	assert.Len(t, drainEvents(rec), 1)

	rv := held.ResourceVersion
	_, err = r.handleDeletion(ctx, held.DeepCopy())
	require.NoError(t, err)
	again := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), again))
	assert.Equal(t, rv, again.ResourceVersion)
	assert.Empty(t, drainEvents(rec))
}

func TestHandleDeletion_ImageArtifactForceDeleteStillWins(t *testing.T) {
	ctx := context.Background()
	prov := &deleteStubProvider{err: imageArtifactRefusal("delete")}
	vm := deletionVM("vm-template-force")
	vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	_, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
}

// A later ordinary failure replaces the image-artifact Ready (it no longer
// says why the delete waits).
func TestClearStaleDeleteHold_ImageArtifactReady(t *testing.T) {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{Type: k8s.ConditionReady, Status: metav1.ConditionFalse,
		Reason: k8s.ReasonVMIsImageArtifact, Message: imageArtifactDeleteMessage})
	assert.True(t, clearStaleDeleteHold(vm))
	assert.Equal(t, k8s.ReasonProviderError, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady).Reason)
}

// imageArtifactPowerProvider reports the VM Off and refuses every Power.
type imageArtifactPowerProvider struct {
	stubProvider
	powers int
}

func (p *imageArtifactPowerProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)}, nil
}

func (p *imageArtifactPowerProvider) Power(_ context.Context, _ contracts.VMRef, _ contracts.PowerOp) (string, error) {
	p.powers++
	return "", imageArtifactRefusal("power")
}

func TestReconcileVM_ImageArtifactPowerRefusalIsHeld(t *testing.T) {
	ctx := context.Background()
	prov := &imageArtifactPowerProvider{}
	r, vm := boundSingleHostVM(t, &stubResolver{provider: prov})
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 1, prov.powers)
	assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin, "no 5 s provider-error loop")

	held := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held))
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonVMIsImageArtifact, ready.Reason)
	assert.Equal(t, imageArtifactPowerMessage, ready.Message)
	assert.NotContains(t, ready.Message, "rpc error")
	assert.Len(t, drainEvents(rec), 1)

	rv := held.ResourceVersion
	_, err = r.reconcileVM(ctx, held.DeepCopy())
	require.NoError(t, err)
	again := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), again))
	assert.Equal(t, rv, again.ResourceVersion, "a repeated refusal leaves the status unchanged")
	assert.Empty(t, drainEvents(rec), "no further event")
}

var _ = Describe("Image-artifact refusals (envtest)", func() {
	It("holds a refused power change and a refused delete on constant conditions without status churn", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "img-artifact-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := holdTestVM(ns.Name, "tmpl", infravirtrigaudiov1beta1.ObservedPowerStateOff)
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

		rp := &routingProvider{
			describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)},
			powerErr:     imageArtifactRefusal("power"),
			deleteErr:    imageArtifactRefusal("delete"),
		}
		rec := record.NewFakeRecorder(100)
		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: rec}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		get := func() *infravirtrigaudiov1beta1.VirtualMachine {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
			return got
		}

		By("a refused power change is held")
		_, err := r.Reconcile(ctx, req) // finalizer
		Expect(err).NotTo(HaveOccurred())
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">=", blockedRetryMin))
		ready := meta.FindStatusCondition(get().Status.Conditions, k8s.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(k8s.ReasonVMIsImageArtifact))
		Expect(ready.Message).To(Equal(imageArtifactPowerMessage))
		Expect(ready.ObservedGeneration).To(Equal(get().Generation))
		rv := get().ResourceVersion
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(get().ResourceVersion).To(Equal(rv), "a repeated refusal writes nothing")
		Expect(artifactEvents(rec)).To(HaveLen(1))

		By("a refused delete is held, then released by orphan-on-delete")
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		res, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">=", blockedRetryMin))
		held := get()
		Expect(held.Finalizers).To(ContainElement(infravirtrigaudiov1beta1.VirtualMachineFinalizer))
		ready = meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
		Expect(ready.Reason).To(Equal(k8s.ReasonVMIsImageArtifact))
		Expect(ready.Message).To(Equal(imageArtifactDeleteMessage))
		rv = held.ResourceVersion
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(get().ResourceVersion).To(Equal(rv))
		Expect(artifactEvents(rec)).To(HaveLen(1), "one event for the delete hold")
		deletes := len(rp.deleteRefs)

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			got := get()
			got.Annotations = map[string]string{infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation: "true"}
			return k8sClient.Update(ctx, got)
		})).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, req.NamespacedName, &infravirtrigaudiov1beta1.VirtualMachine{}))).To(BeTrue())
		Expect(rp.deleteRefs).To(HaveLen(deletes), "orphan-on-delete makes no provider call")
	})
})
