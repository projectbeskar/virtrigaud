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
	"errors"
	"sync"

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

// powerHypervisor is one source hypervisor seen by two controllers: each
// controller gets a powerHypervisorView naming it, so the test can prove which
// controller sent each Power call.
type powerHypervisor struct {
	stubProvider

	mu     sync.Mutex
	on     bool
	calls  map[string]int
	script []powerAnswer // consumed one per Power call; empty = succeed
}

// powerAnswer scripts one Power call: whether the power-off takes effect, and
// the error the call returns.
type powerAnswer struct {
	takesEffect bool
	err         error
}

// powerHypervisorView is the provider a controller is handed.
type powerHypervisorView struct {
	*powerHypervisor
	actor string
}

func (v powerHypervisorView) Describe(context.Context, contracts.VMRef) (contracts.DescribeResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	state := contracts.PowerStateOff
	if v.on {
		state = contracts.PowerStateOn
	}
	return contracts.DescribeResponse{Exists: true, PowerState: string(state)}, nil
}

func (v powerHypervisorView) Power(_ context.Context, _ contracts.VMRef, op contracts.PowerOp) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls[v.actor]++
	answer := powerAnswer{takesEffect: true}
	if len(v.script) > 0 {
		answer, v.script = v.script[0], v.script[1:]
	}
	if answer.takesEffect {
		v.on = op == contracts.PowerOpOn
	}
	return "", answer.err
}

func (h *powerHypervisor) callsBy(actor string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[actor]
}

func (h *powerHypervisor) isOn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.on
}

// The rc1 lab failure (F3): with powerOffBeforeMigration on a running source,
// the VMMigration controller set the source's spec.powerState to Off AND
// powered it off itself; the VirtualMachine controller powered it off too, the
// second `virsh destroy` failed on the stopped domain, and the migration
// failed terminally. Both controllers run here against a real apiserver and
// one shared hypervisor: only the VirtualMachine controller sends Power, a
// failed power-off — with or without effect — never fails the migration, and
// the migration proceeds to Exporting once the source is observed Off.
var _ = Describe("VMMigration source power-off has one actor (envtest)", func() {
	It("waits for the VirtualMachine controller to power the source off, through a failed power-off race, then exports", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mig-poweroff-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		// Source and target Providers, both available.
		for _, name := range []string{"prov-1", "prov-2"} {
			p := holdTestProvider(ns.Name)
			p.Name = name
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
			p.Status.Conditions = []metav1.Condition{{Type: "ProviderAvailable", Status: metav1.ConditionTrue,
				Reason: "RemoteAvailable", Message: "ready", LastTransitionTime: metav1.Now()}}
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := holdTestVM(ns.Name, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOn)
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

		hv := &powerHypervisor{on: true, calls: map[string]int{}}
		vmr := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: powerHypervisorView{powerHypervisor: hv, actor: "vm"}},
			Recorder:       record.NewFakeRecorder(100)}
		vmReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
		reconcileVM := func() {
			_, err := vmr.Reconcile(ctx, vmReq)
			Expect(err).NotTo(HaveOccurred())
		}
		getVM := func() *infravirtrigaudiov1beta1.VirtualMachine {
			got := &infravirtrigaudiov1beta1.VirtualMachine{}
			Expect(k8sClient.Get(ctx, vmReq.NamespacedName, got)).To(Succeed())
			return got
		}

		By("the running source VM is steady before the migration")
		reconcileVM() // finalizer
		reconcileVM()
		Expect(hv.callsBy("vm")).To(BeZero())

		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns.Name,
				Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{
					VMRef:                   infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"},
					PowerOffBeforeMigration: true,
				},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: "web",
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-2"}},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				got := &infravirtrigaudiov1beta1.VMMigration{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got); err != nil {
					return client.IgnoreNotFound(err)
				}
				got.Finalizers = nil
				return k8sClient.Update(ctx, got)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, m))).To(Succeed())
		})
		mr := NewVMMigrationReconciler(k8sClient, k8sClient, k8sClient.Scheme(), nil, record.NewFakeRecorder(100), false)
		mr.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
			return powerHypervisorView{powerHypervisor: hv, actor: "migration"}, nil
		}
		mReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
		reconcileMigration := func() ctrl.Result {
			res, err := mr.Reconcile(ctx, mReq)
			Expect(err).NotTo(HaveOccurred())
			return res
		}
		getMigration := func() *infravirtrigaudiov1beta1.VMMigration {
			got := &infravirtrigaudiov1beta1.VMMigration{}
			Expect(k8sClient.Get(ctx, mReq.NamespacedName, got)).To(Succeed())
			return got
		}
		expectWaiting := func() {
			got := getMigration()
			Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseValidating))
			c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal(reasonWaitingForSourcePowerOff))
			Expect(c.Message).To(Equal(waitingForSourcePowerOffMessage))
			Expect(c.ObservedGeneration).To(Equal(got.Generation))
		}

		By("the migration sets the source's desired power state to Off and waits, sending no Power call")
		reconcileMigration() // "" -> Pending
		reconcileMigration() // Pending -> Validating
		res := reconcileMigration()
		Expect(res.RequeueAfter).To(Equal(migrationPowerOffPollInterval))
		expectWaiting()
		Expect(getVM().Spec.PowerState).To(Equal(infravirtrigaudiov1beta1.PowerStateOff))
		Expect(hv.callsBy("migration")).To(BeZero())
		rv := getMigration().ResourceVersion
		reconcileMigration()
		Expect(getMigration().ResourceVersion).To(Equal(rv), "a repeated wait writes nothing")

		By("the VirtualMachine controller's power-off fails without effect: the migration keeps waiting")
		hv.mu.Lock()
		hv.script = []powerAnswer{
			{takesEffect: false, err: errors.New("failed to perform power operation on host")},
			// The race seen on the lab: the domain IS stopped (by the other
			// destroy), yet this call reports a failure.
			{takesEffect: true, err: errors.New("Failed to destroy domain: domain is not running")},
		}
		hv.mu.Unlock()
		reconcileVM()
		Expect(hv.callsBy("vm")).To(Equal(1))
		Expect(hv.isOn()).To(BeTrue())
		reconcileMigration()
		expectWaiting()
		Expect(getMigration().ResourceVersion).To(Equal(rv), "still nothing written while waiting")

		By("the VirtualMachine controller's next power-off stops the domain but reports an error (the race)")
		reconcileVM()
		Expect(hv.callsBy("vm")).To(Equal(2))
		Expect(hv.isOn()).To(BeFalse())

		By("the migration observes the source Off and proceeds to Exporting, never Failed")
		reconcileMigration()
		got := getMigration()
		Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseExporting))
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)).To(BeTrue())
		Expect(hv.callsBy("migration")).To(BeZero(), "the migration controller never sent Power")

		By("the VirtualMachine controller records the source Off and sends no further power-off")
		reconcileVM()
		src := getVM()
		Expect(src.Status.PowerState).To(Equal(infravirtrigaudiov1beta1.ObservedPowerStateOff))
		Expect(hv.callsBy("vm")).To(Equal(2))
		ready := meta.FindStatusCondition(src.Status.Conditions, k8s.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).NotTo(Equal(k8s.ReasonProviderError), "the source settles once it is observed Off")
		Expect(src.Spec.PowerState).To(Equal(infravirtrigaudiov1beta1.PowerStateOff),
			"the source is not powered back on by the migration (unchanged behaviour)")
	})
})
