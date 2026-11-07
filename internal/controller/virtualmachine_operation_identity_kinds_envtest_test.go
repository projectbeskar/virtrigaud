//go:build virtrigaud_dev

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
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// These specs run the signed power operations and Reconfigure, the
// serialisation of attempts across kinds, the Delete rule for an absent VM
// and the local refusal of operations with no signed path end to end, as the
// Create/Delete specs of virtualmachine_operation_identity_envtest_test.go do:
// a real API server, managers stopped at injected crash points and replaced,
// and the instrumented fake in operation-identity mode behind TLS gRPC.

// createVMWith creates a VM of the env with spec.powerState power.
func (e *identityEnv) createVMWith(ctx context.Context, name string, power infravirtrigaudiov1beta1.PowerState) client.ObjectKey {
	key := e.createVM(ctx, name)
	if power != infravirtrigaudiov1beta1.PowerStateOff {
		e.patchVM(ctx, key, func(vm *infravirtrigaudiov1beta1.VirtualMachine) { vm.Spec.PowerState = power })
	}
	return key
}

// patchVM edits the VM's spec (retrying on conflicts).
func (e *identityEnv) patchVM(ctx context.Context, key client.ObjectKey, edit func(*infravirtrigaudiov1beta1.VirtualMachine)) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm := getIdentityVM(ctx, Default, key)
		edit(vm)
		return k8sClient.Update(ctx, vm)
	})).To(Succeed())
}

// waitAttempt waits (finishing the fake's tasks) until the VM's attempt of
// kind is Terminal and Succeeded and no intent Secret is left.
func (e *identityEnv) waitAttempt(ctx context.Context, key client.ObjectKey, kind contracts.OperationKind) *infravirtrigaudiov1beta1.OperationAttempt {
	var a *infravirtrigaudiov1beta1.OperationAttempt
	Eventually(func(g Gomega) {
		g.Expect(e.target.FinishTasks()).To(Succeed())
		a = findAttempt(getIdentityVM(ctx, g, key), kind)
		g.Expect(a).NotTo(BeNil())
		g.Expect(a.Phase).To(Equal(infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal))
		g.Expect(a.Outcome).To(Equal(infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded))
		g.Expect(e.intentSecretCount(ctx, g)).To(BeZero())
	}, "30s", "100ms").Should(Succeed())
	return a
}

// readyReasonOf is the VM's Ready reason, or "".
func readyReasonOf(ctx context.Context, g Gomega, key client.ObjectKey) string {
	c := meta.FindStatusCondition(getIdentityVM(ctx, g, key).Status.Conditions, k8s.ConditionReady)
	if c == nil {
		return ""
	}
	return c.Reason
}

// identityKindCase is how a spec asks for a kind: the VM's initial power
// state and the spec change that asks for the kind.
type identityKindCase struct {
	kind    contracts.OperationKind
	wire    providerv1.OperationKind
	method  string
	initial infravirtrigaudiov1beta1.PowerState
	ask     func(*infravirtrigaudiov1beta1.VirtualMachine)
}

var (
	kindPowerOn = identityKindCase{contracts.OperationKindPowerOn, providerv1.OperationKind_OPERATION_KIND_POWER_ON,
		providerv1.Provider_Power_FullMethodName, infravirtrigaudiov1beta1.PowerStateOff,
		func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
		}}
	kindPowerOff = identityKindCase{contracts.OperationKindPowerOff, providerv1.OperationKind_OPERATION_KIND_POWER_OFF,
		providerv1.Provider_Power_FullMethodName, infravirtrigaudiov1beta1.PowerStateOn,
		func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
		}}
	kindShutdown = identityKindCase{contracts.OperationKindShutdownGraceful, providerv1.OperationKind_OPERATION_KIND_SHUTDOWN_GRACEFUL,
		providerv1.Provider_Power_FullMethodName, infravirtrigaudiov1beta1.PowerStateOn,
		func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOffGraceful
		}}
	// The fake applies 4 vCPU and 4096 MiB on every Reconfigure; the spec asks
	// for exactly that, so what it reports agrees with what was asked.
	kindReconfigure = identityKindCase{contracts.OperationKindReconfigure, providerv1.OperationKind_OPERATION_KIND_RECONFIGURE,
		providerv1.Provider_Reconfigure_FullMethodName, infravirtrigaudiov1beta1.PowerStateOff,
		func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			cpu, mem := int32(4), int64(4096)
			vm.Spec.Resources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
		}}
)

// settledVM creates a VM in c.initial with a manager that is then stopped:
// its Create (and, for a VM that starts on, its PowerOn) is over.
func (e *identityEnv) settledVM(ctx context.Context, name string, initial infravirtrigaudiov1beta1.PowerState) client.ObjectKey {
	stop := e.startManager(ctx, nil)
	key := e.createVMWith(ctx, name, initial)
	e.waitAttempt(ctx, key, contracts.OperationKindCreate)
	if initial == infravirtrigaudiov1beta1.PowerStateOn {
		e.waitAttempt(ctx, key, contracts.OperationKindPowerOn)
	}
	stop()
	return key
}

// forgeIntent replaces the intent Secret of attempt a (immutable: delete and
// re-create) with one whose request bytes were changed.
func (e *identityEnv) forgeIntent(ctx context.Context, a *infravirtrigaudiov1beta1.OperationAttempt) {
	secret := &corev1.Secret{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: intentSecretName(a.OpID)}, secret)).To(Succeed())
	forged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secret.Name, Namespace: secret.Namespace, Labels: secret.Labels,
		Annotations: secret.Annotations, OwnerReferences: secret.OwnerReferences}, Type: secret.Type, Data: map[string][]byte{}}
	for k, v := range secret.Data {
		forged.Data[k] = append([]byte(nil), v...)
	}
	forged.Data[intentKeyRequest] = append(forged.Data[intentKeyRequest], 0x01)
	Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
	Expect(k8sClient.Create(ctx, forged)).To(Succeed())
}

var _ = Describe("Operation identity: power, Reconfigure and serialisation against the instrumented fake (envtest, development build)", func() {
	ctx := context.Background()

	DescribeTable("a manager restarted at a crash point of a power operation or Reconfigure ends with one execution and a Terminal marker",
		func(c identityKindCase, point operationCrashPoint) {
			e := newIdentityEnv(ctx)
			key := e.settledVM(ctx, "crash-kind", c.initial)
			before := e.target.AcceptedOperations(c.wire)

			crashed := make(chan struct{})
			stop := e.startManager(ctx, crashAt(point, c.kind, crashed))
			e.patchVM(ctx, key, c.ask)
			Eventually(func(g Gomega) {
				g.Expect(e.target.FinishTasks()).To(Succeed())
				g.Expect(crashed).To(BeClosed())
			}, "30s", "100ms").Should(Succeed())
			stop()

			e.startManager(ctx, nil)
			a := e.waitAttempt(ctx, key, c.kind)
			Expect(a.Attempt).To(Equal(int64(1)))
			Consistently(func() int {
				return e.target.AcceptedOperations(c.wire) - before
			}, "1s", "100ms").Should(Equal(1), "exactly one %s was executed", c.kind)
			Expect(e.opIDsOf(c.method)).To(HaveKey(a.OpID), "every delivery of the attempt carried its op_id")
			if c.kind == contracts.OperationKindReconfigure {
				vm := getIdentityVM(ctx, Default, key)
				Expect(*vm.Status.CurrentResources.CPU).To(Equal(int32(4)), "the size the attempt asked for")
				Expect(*vm.Status.CurrentResources.MemoryMiB).To(Equal(int64(4096)))
			}
		},
		Entry("PowerOn, after Prepared", kindPowerOn, crashAfterPrepared),
		Entry("PowerOn, after the send", kindPowerOn, crashAfterSend),
		Entry("PowerOn, after the task's outcome is read", kindPowerOn, crashAfterReply),
		Entry("PowerOn, after Terminal", kindPowerOn, crashAfterTerminal),
		Entry("PowerOff, after Prepared", kindPowerOff, crashAfterPrepared),
		Entry("PowerOff, after the send", kindPowerOff, crashAfterSend),
		Entry("PowerOff, after the task's outcome is read", kindPowerOff, crashAfterReply),
		Entry("PowerOff, after Terminal", kindPowerOff, crashAfterTerminal),
		Entry("ShutdownGraceful, after Prepared", kindShutdown, crashAfterPrepared),
		Entry("ShutdownGraceful, after the send", kindShutdown, crashAfterSend),
		Entry("ShutdownGraceful, after the task's outcome is read", kindShutdown, crashAfterReply),
		Entry("ShutdownGraceful, after Terminal", kindShutdown, crashAfterTerminal),
		Entry("Reconfigure, after Prepared", kindReconfigure, crashAfterPrepared),
		Entry("Reconfigure, after the send", kindReconfigure, crashAfterSend),
		Entry("Reconfigure, after Terminal", kindReconfigure, crashAfterTerminal),
	)

	DescribeTable("a Reboot stopped at a crash point is finished by a new manager with one execution",
		func(point operationCrashPoint) {
			e := newIdentityEnv(ctx)
			key := e.settledVM(ctx, "reboot", infravirtrigaudiov1beta1.PowerStateOn)
			before := e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_REBOOT)

			// No VirtualMachine field asks for a Reboot: the attempt is started
			// directly, by a reconciler on the API server whose hook stops it
			// at point.
			var fired atomic.Bool
			r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
				RemoteResolver: identityResolver{p: e.client}, Recorder: record.NewFakeRecorder(100),
				OperationEnrolments: e.enrol, OperationIntentAuth: e.auth,
				operationCrashHook: func(_ context.Context, p operationCrashPoint) bool {
					return p == point && fired.CompareAndSwap(false, true)
				}}
			vm := getIdentityVM(ctx, Default, key)
			provider := &infravirtrigaudiov1beta1.Provider{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(e.provider), provider)).To(Succeed())
			ref, err := vmRefFor(vm, provider)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.startIdentityPowerOp(ctx, vm, provider, e.client, ref, contracts.OperationKindReboot)
			if !fired.Load() {
				// The attempt is Sent with its task: its outcome is read (and
				// the marker made Terminal) by the next drive.
				Expect(err).NotTo(HaveOccurred())
				Expect(e.target.FinishTasks()).To(Succeed())
				vm = getIdentityVM(ctx, Default, key)
				_, _, err = r.reconcileIdentityGate(ctx, vm, provider, e.client, nil)
			}
			Expect(fired.Load()).To(BeTrue(), "the attempt stopped at %s", point)
			Expect(err).To(MatchError(errOperationCrashed))

			e.startManager(ctx, nil)
			a := e.waitAttempt(ctx, key, contracts.OperationKindReboot)
			Expect(a.Attempt).To(Equal(int64(1)))
			Consistently(func() int {
				return e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_REBOOT) - before
			}, "1s", "100ms").Should(Equal(1), "exactly one Reboot was executed")
		},
		Entry("after Prepared", crashAfterPrepared),
		Entry("after the send", crashAfterSend),
		Entry("after the task's outcome is read", crashAfterReply),
		Entry("after Terminal", crashAfterTerminal),
	)

	It("waits for a power operation in flight before a Reconfigure, then sends one Reconfigure", func() {
		e := newIdentityEnv(ctx)
		key := e.settledVM(ctx, "serial", infravirtrigaudiov1beta1.PowerStateOn)
		e.startManager(ctx, nil)

		e.patchVM(ctx, key, kindPowerOff.ask)
		Eventually(func(g Gomega) {
			a := findAttempt(getIdentityVM(ctx, g, key), contracts.OperationKindPowerOff)
			g.Expect(a).NotTo(BeNil())
			g.Expect(a.Phase).To(Equal(infravirtrigaudiov1beta1.OperationAttemptPhaseSent))
		}, "30s", "100ms").Should(Succeed())
		e.patchVM(ctx, key, kindReconfigure.ask)
		Consistently(func(g Gomega) {
			g.Expect(e.delivered(providerv1.Provider_Reconfigure_FullMethodName)).To(BeZero())
			g.Expect(findAttempt(getIdentityVM(ctx, g, key), contracts.OperationKindReconfigure)).To(BeNil())
		}, "2s", "100ms").Should(Succeed(), "no Reconfigure while the power-off is in flight")

		e.waitAttempt(ctx, key, contracts.OperationKindPowerOff)
		e.waitAttempt(ctx, key, contracts.OperationKindReconfigure)
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_RECONFIGURE)).To(Equal(1))
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_POWER_OFF)).To(Equal(1))
	})

	It("waits for a power operation in flight before the Delete, then deletes once", func() {
		e := newIdentityEnv(ctx)
		key := e.settledVM(ctx, "serial-del", infravirtrigaudiov1beta1.PowerStateOn)
		e.startManager(ctx, nil)
		e.patchVM(ctx, key, kindPowerOff.ask)
		Eventually(func(g Gomega) {
			a := findAttempt(getIdentityVM(ctx, g, key), contracts.OperationKindPowerOff)
			g.Expect(a).NotTo(BeNil())
			g.Expect(a.Phase).To(Equal(infravirtrigaudiov1beta1.OperationAttemptPhaseSent))
		}, "30s", "100ms").Should(Succeed())
		Expect(k8sClient.Delete(ctx, getIdentityVM(ctx, Default, key))).To(Succeed())
		Consistently(func() int64 {
			return e.delivered(providerv1.Provider_Delete_FullMethodName)
		}, "2s", "100ms").Should(BeZero(), "no Delete while the power-off is in flight")
		e.waitDeleted(ctx, key)
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_DELETE)).To(Equal(1))
	})

	It("lets the Delete supersede a held PowerOn, sends the signed Delete, and releases only once it is confirmed", func() {
		e := newIdentityEnv(ctx)
		key := e.settledVM(ctx, "supersede", infravirtrigaudiov1beta1.PowerStateOff)
		crashed := make(chan struct{})
		stop := e.startManager(ctx, crashAt(crashAfterPrepared, contracts.OperationKindPowerOn, crashed))
		e.patchVM(ctx, key, kindPowerOn.ask)
		Eventually(crashed, "30s").Should(BeClosed())
		stop()
		held := findAttempt(getIdentityVM(ctx, Default, key), contracts.OperationKindPowerOn)
		Expect(held).NotTo(BeNil())
		e.forgeIntent(ctx, held)

		e.startManager(ctx, nil)
		Eventually(func(g Gomega) {
			g.Expect(readyReasonOf(ctx, g, key)).To(Equal(k8s.ReasonOperationIntentTampered))
		}, "30s", "100ms").Should(Succeed(), "the PowerOn is held")
		Expect(k8sClient.Delete(ctx, getIdentityVM(ctx, Default, key))).To(Succeed())

		Eventually(func(g Gomega) {
			vm := getIdentityVM(ctx, g, key)
			p := findAttempt(vm, contracts.OperationKindPowerOn)
			g.Expect(p.Outcome).To(Equal(infravirtrigaudiov1beta1.OperationAttemptOutcomeSuperseded))
			d := findAttempt(vm, contracts.OperationKindDelete)
			g.Expect(d).NotTo(BeNil())
			g.Expect(d.Phase).To(Equal(infravirtrigaudiov1beta1.OperationAttemptPhaseSent))
		}, "30s", "100ms").Should(Succeed())
		Expect(e.delivered(providerv1.Provider_Delete_FullMethodName)).To(BeNumerically(">=", 1), "the Delete is signed and sent")
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, &infravirtrigaudiov1beta1.VirtualMachine{})).To(Succeed())
		}, "1s", "100ms").Should(Succeed(), "not released before the Delete is confirmed")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: intentSecretName(held.OpID)}, &corev1.Secret{})).To(Succeed(),
			"the superseded attempt's intent Secret is kept")

		Eventually(func(g Gomega) {
			g.Expect(e.target.FinishTasks()).To(Succeed())
			err := k8sClient.Get(ctx, key, &infravirtrigaudiov1beta1.VirtualMachine{})
			g.Expect(err).To(HaveOccurred())
		}, "30s", "100ms").Should(Succeed(), "released once the Delete is confirmed")
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_DELETE)).To(Equal(1))
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_POWER_ON)).To(BeZero())
	})

	It("keeps a deleted VM whose Create is held: a Create is never superseded", func() {
		e := newIdentityEnv(ctx)
		crashed := make(chan struct{})
		stop := e.startManager(ctx, crashAt(crashAfterPrepared, contracts.OperationKindCreate, crashed))
		key := e.createVM(ctx, "held-create")
		Eventually(crashed, "30s").Should(BeClosed())
		stop()
		c := findAttempt(getIdentityVM(ctx, Default, key), contracts.OperationKindCreate)
		Expect(c).NotTo(BeNil())
		e.forgeIntent(ctx, c)

		e.startManager(ctx, nil)
		Eventually(func(g Gomega) {
			g.Expect(readyReasonOf(ctx, g, key)).To(Equal(k8s.ReasonOperationIntentTampered))
		}, "30s", "100ms").Should(Succeed())
		Expect(k8sClient.Delete(ctx, getIdentityVM(ctx, Default, key))).To(Succeed())
		Consistently(func(g Gomega) {
			vm := getIdentityVM(ctx, g, key)
			g.Expect(findAttempt(vm, contracts.OperationKindCreate).Phase).To(Equal(infravirtrigaudiov1beta1.OperationAttemptPhasePrepared))
			g.Expect(findAttempt(vm, contracts.OperationKindDelete)).To(BeNil())
		}, "2s", "100ms").Should(Succeed(), "held as before")
		Expect(e.delivered(providerv1.Provider_Delete_FullMethodName)).To(BeZero())
	})

	It("holds a power intent tampered with after Prepared, and sends nothing", func() {
		e := newIdentityEnv(ctx)
		key := e.settledVM(ctx, "tamper-power", infravirtrigaudiov1beta1.PowerStateOff)
		crashed := make(chan struct{})
		stop := e.startManager(ctx, crashAt(crashAfterPrepared, contracts.OperationKindPowerOn, crashed))
		e.patchVM(ctx, key, kindPowerOn.ask)
		Eventually(crashed, "30s").Should(BeClosed())
		stop()

		a := findAttempt(getIdentityVM(ctx, Default, key), contracts.OperationKindPowerOn)
		Expect(a).NotTo(BeNil())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: intentSecretName(a.OpID)}, secret)).To(Succeed())
		forged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secret.Name, Namespace: secret.Namespace, Labels: secret.Labels,
			Annotations: secret.Annotations, OwnerReferences: secret.OwnerReferences}, Type: secret.Type, Data: map[string][]byte{}}
		for k, v := range secret.Data {
			forged.Data[k] = append([]byte(nil), v...)
		}
		forged.Data[intentKeyRequest] = append(forged.Data[intentKeyRequest], 0x01)
		Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		Expect(k8sClient.Create(ctx, forged)).To(Succeed())
		powerDeliveries := e.delivered(providerv1.Provider_Power_FullMethodName)

		e.startManager(ctx, nil)
		Eventually(func(g Gomega) {
			g.Expect(readyReasonOf(ctx, g, key)).To(Equal(k8s.ReasonOperationIntentTampered))
		}, "30s", "100ms").Should(Succeed())
		Consistently(func() int64 {
			return e.delivered(providerv1.Provider_Power_FullMethodName)
		}, "1s", "100ms").Should(Equal(powerDeliveries), "nothing is sent")
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_POWER_ON)).To(BeZero())
	})

	It("never releases the Delete finalizer when the host reports the VM absent; force-delete does", func() {
		e := newIdentityEnv(ctx)
		e.startManager(ctx, nil)
		key := e.createVM(ctx, "absent")
		vm := e.waitCreated(ctx, key)
		Expect(e.target.RemoveVM("host-a", vm.Status.ID)).To(Succeed())

		Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(readyReasonOf(ctx, g, key)).To(Equal(k8s.ReasonOperationDeleteUnconfirmed))
		}, "30s", "100ms").Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, &infravirtrigaudiov1beta1.VirtualMachine{})).To(Succeed())
		}, "2s", "100ms").Should(Succeed(), "the finalizer is kept")
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_DELETE)).To(BeZero())

		e.patchVM(ctx, key, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
			vm.Annotations = map[string]string{forceDeleteAnnotation: "true"}
		})
		Eventually(func() error {
			return k8sClient.Get(ctx, key, &infravirtrigaudiov1beta1.VirtualMachine{})
		}, "30s", "100ms").Should(HaveOccurred(), "force-delete releases it")
	})

	It("refuses a snapshot of an identity VM locally: OperationIdentityUnsupported, no RPC", func() {
		e := newIdentityEnv(ctx)
		key := e.settledVM(ctx, "snap", infravirtrigaudiov1beta1.PowerStateOff)

		mgrCtx, cancel := context.WithCancel(ctx)
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: k8sClient.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Controller: ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		var resolves atomic.Int32
		sr := NewVMSnapshotReconciler(mgr.GetClient(), mgr.GetScheme(), nil, record.NewFakeRecorder(100), false)
		sr.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
			resolves.Add(1)
			return e.client, nil
		}
		Expect(sr.SetupWithManager(mgr)).To(Succeed())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() { cancel(); wg.Wait() })

		snap := &infravirtrigaudiov1beta1.VMSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-1", Namespace: e.ns},
			Spec:       infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: key.Name}},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		Eventually(func(g Gomega) {
			got := &infravirtrigaudiov1beta1.VMSnapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(snap), got)).To(Succeed())
			c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMSnapshotConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(k8s.ReasonOperationIdentityUnsupported))
		}, "30s", "100ms").Should(Succeed())
		Expect(resolves.Load()).To(BeZero())
		Expect(e.called(providerv1.Provider_SnapshotCreate_FullMethodName)).To(BeZero(), "no snapshot RPC reached the Provider")
	})
})
