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

// vmNotOwnedAnswer is the error the transport client maps a provider's
// NotFound + VM_NOT_OWNED answer to (contracts.IsVMNotOwned).
func vmNotOwnedAnswer(op string) error {
	return contracts.NewNotFoundError(op+": the Proxmox VM at this VMID is not this VirtualMachine's",
		fmt.Errorf("%w: rpc error: code = NotFound", contracts.ErrVMNotOwned))
}

// ownerStampUnreadableAnswer is the error the transport client maps a
// provider's FailedPrecondition + OWNER_STAMP_UNREADABLE answer to.
func ownerStampUnreadableAnswer(op string) error {
	return contracts.NewConflictError(op+": refused: the VM carries an owner stamp that cannot be read",
		fmt.Errorf("%w: rpc error: code = FailedPrecondition", contracts.ErrOwnerStampUnreadable))
}

// eventsWithReason returns the recorded events of reason.
func eventsWithReason(rec *record.FakeRecorder, reason string) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, " "+reason+" ") {
			out = append(out, e)
		}
	}
	return out
}

// boundSingleHostVMWithUID is boundSingleHostVM for a VirtualMachine with a
// UID (an API server assigns one; the fake client does not), so its per-VM
// calls carry its owner.
func boundSingleHostVMWithUID(t *testing.T, resolver ProviderResolver) (*VirtualMachineReconciler, *infravirtrigaudiov1beta1.VirtualMachine) {
	t.Helper()
	prov, class := providerAndClass("default")
	vm := baseVM("default")
	vm.UID = "uid-test-vm"
	vm.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: prov.Name}
	r := newTestReconciler(coverageTestScheme(t), resolver, prov, class, vm)
	stored := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vm), stored))
	require.NotEmpty(t, stored.UID)
	return r, stored
}

// TestHoldOwnerRefusal_OnlyOwnerRefusals: only VM_NOT_OWNED and
// OWNER_STAMP_UNREADABLE are held; a plain NotFound or any other failure keeps
// its historical handling.
func TestHoldOwnerRefusal_OnlyOwnerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		held   bool
		reason string
	}{
		{"VM_NOT_OWNED", vmNotOwnedAnswer("describe"), true, k8s.ReasonVMNotOwned},
		{"OWNER_STAMP_UNREADABLE", ownerStampUnreadableAnswer("power"), true, k8s.ReasonOwnerStampUnreadable},
		{"plain NotFound", contracts.NewNotFoundError("describe: gone", nil), false, ""},
		{"image artifact", imageArtifactRefusal("power"), false, ""},
		{"retryable", contracts.NewRetryableError("describe: unavailable", nil), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold, ok := ownerRefusalHoldFor(tc.err)
			assert.Equal(t, tc.held, ok)
			assert.Equal(t, tc.reason, hold.reason)
		})
	}
}

// A single-host Describe answered VM_NOT_OWNED (the VM's VMID was reused) holds
// the VirtualMachine: status.id is kept and nothing is created in its place,
// and the request carried the VirtualMachine's identity as owner.
func TestReconcileVM_DescribeNotOwnedIsHeldNotRecreated(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{describeErr: vmNotOwnedAnswer("describe")}
	r, vm := boundSingleHostVMWithUID(t, &stubResolver{provider: prov})
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin, "no 5 s provider-error loop")
	require.Len(t, prov.describeRefs, 1)
	assert.Equal(t, contracts.VMRef{ID: "vm-100", Owner: vmOwnerIdentity(vm)}, prov.describeRefs[0],
		"a single-host Describe carries the VirtualMachine's owner")
	assert.Empty(t, prov.createReqs, "VM_NOT_OWNED is not \"gone\": nothing is created")

	held := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held))
	assert.Equal(t, "vm-100", held.Status.ID, "status.id is kept")
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonVMNotOwned, ready.Reason)
	assert.Equal(t, vmNotOwnedHoldMessage, ready.Message)
	assert.NotContains(t, ready.Message, "rpc error")
	assert.Len(t, eventsWithReason(rec, k8s.ReasonVMNotOwned), 1)

	rv := held.ResourceVersion
	_, err = r.reconcileVM(ctx, held.DeepCopy())
	require.NoError(t, err)
	again := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), again))
	assert.Equal(t, rv, again.ResourceVersion, "a repeated refusal leaves the status unchanged")
	assert.Empty(t, drainEvents(rec), "no further event")
	assert.Empty(t, prov.createReqs)
}

// A single-host Power refused VM_NOT_OWNED (the VMID reused between the
// Describe and the Power) or OWNER_STAMP_UNREADABLE is held the same way.
func TestReconcileVM_PowerOwnerRefusalIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		reason  string
		message string
	}{
		{"VM_NOT_OWNED", vmNotOwnedAnswer("power"), k8s.ReasonVMNotOwned, vmNotOwnedHoldMessage},
		{"OWNER_STAMP_UNREADABLE", ownerStampUnreadableAnswer("power"), k8s.ReasonOwnerStampUnreadable, ownerStampUnreadableHoldMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			prov := &routingProvider{
				describeResp: contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOff)},
				powerErr:     tc.err,
			}
			r, vm := boundSingleHostVMWithUID(t, &stubResolver{provider: prov})

			res, err := r.reconcileVM(ctx, vm)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin)
			require.Len(t, prov.powerRefs, 1)
			assert.Equal(t, vmOwnerIdentity(vm), prov.powerRefs[0].Owner, "a single-host Power carries the owner")

			held := &infravirtrigaudiov1beta1.VirtualMachine{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(vm), held))
			ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.reason, ready.Reason)
			assert.Equal(t, tc.message, ready.Message)
		})
	}
}

var _ = Describe("Owner refusals of single-host per-VM calls (envtest)", func() {
	It("holds a VirtualMachine whose VMID was reused on a constant condition, never re-creates it, and releases it on delete", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "vm-not-owned-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := holdTestVM(ns.Name, "reused", infravirtrigaudiov1beta1.ObservedPowerStateOn)
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

		rp := &routingProvider{describeErr: vmNotOwnedAnswer("describe"), deleteErr: vmNotOwnedAnswer("delete")}
		rec := record.NewFakeRecorder(100)
		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: rec}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		get := func() *infravirtrigaudiov1beta1.VirtualMachine {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
			return got
		}

		By("a Describe answered VM_NOT_OWNED is held")
		_, err := r.Reconcile(ctx, req) // finalizer
		Expect(err).NotTo(HaveOccurred())
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">=", blockedRetryMin))
		held := get()
		Expect(held.Status.ID).To(Equal(ns.Name+".reused"), "status.id is kept")
		ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(k8s.ReasonVMNotOwned))
		Expect(ready.Message).To(Equal(vmNotOwnedHoldMessage))
		Expect(ready.ObservedGeneration).To(Equal(held.Generation))
		Expect(rp.createReqs).To(BeEmpty(), "nothing is created in place of a VM that is not this VirtualMachine's")
		Expect(rp.describeRefs).NotTo(BeEmpty())
		Expect(rp.describeRefs[len(rp.describeRefs)-1].Owner.UID).To(Equal(string(held.UID)))

		By("a repeated refusal writes nothing and records no further event")
		rv := held.ResourceVersion
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(get().ResourceVersion).To(Equal(rv))
		Expect(eventsWithReason(rec, k8s.ReasonVMNotOwned)).To(HaveLen(1))
		Expect(rp.createReqs).To(BeEmpty())

		By("deleting the VirtualMachine leaves that VM untouched and releases it with a warning")
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, req.NamespacedName, &infravirtrigaudiov1beta1.VirtualMachine{}))).To(BeTrue())
		Expect(rp.deleteOwners).NotTo(BeEmpty())
		Expect(rp.deleteOwners[len(rp.deleteOwners)-1].UID).To(Equal(string(held.UID)))
	})

	It("holds a VirtualMachine whose VM's owner stamp cannot be read", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "vm-stamp-unreadable-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := holdTestVM(ns.Name, "tampered", infravirtrigaudiov1beta1.ObservedPowerStateOn)
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

		rp := &routingProvider{describeErr: ownerStampUnreadableAnswer("describe")}
		rec := record.NewFakeRecorder(100)
		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: rec}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		_, err := r.Reconcile(ctx, req) // finalizer
		Expect(err).NotTo(HaveOccurred())
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">=", blockedRetryMin))
		got := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, got)).To(Succeed())
		ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(k8s.ReasonOwnerStampUnreadable))
		Expect(ready.Message).To(Equal(ownerStampUnreadableHoldMessage))
		Expect(ready.ObservedGeneration).To(Equal(got.Generation))
		Expect(got.Status.ID).To(Equal(ns.Name + ".tampered"))
		Expect(rp.createReqs).To(BeEmpty())
		Expect(eventsWithReason(rec, k8s.ReasonOwnerStampUnreadable)).To(HaveLen(1))
	})
})
