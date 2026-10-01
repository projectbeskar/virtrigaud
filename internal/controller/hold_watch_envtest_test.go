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
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These envtest specs run the VMClone and VMSnapshot controllers in a real
// manager against a real apiserver, to pin their watch predicates: a held
// clone, or a held snapshot delete, is never re-run by a metadata-only update
// of its object (a tenant re-annotating it in a loop would otherwise drive one
// provider call per write, ahead of the hold's backoff), while the events that
// must re-drive it still do — the source VM's power-off, the force-delete
// annotation.

// countingCloner is a Cloner whose answers can be changed while a manager
// runs; it counts the Clone calls. It is safe for concurrent use.
type countingCloner struct {
	stubProvider
	mu    sync.Mutex
	resp  contracts.CloneResponse
	err   error
	calls atomic.Int32
}

// Clone counts the call and returns the scripted answer.
func (p *countingCloner) Clone(_ context.Context, _ contracts.CloneRequest) (contracts.CloneResponse, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resp, p.err
}

// answer scripts the next Clone answers.
func (p *countingCloner) answer(resp contracts.CloneResponse, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resp, p.err = resp, err
}

// startHoldTestManager starts a manager on the envtest apiserver after setup
// registers its controllers, and stops it when the spec ends.
func startHoldTestManager(setup func(mgr ctrl.Manager)) {
	mgrCtx, stop := context.WithCancel(ctx)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 k8sClient.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	Expect(err).NotTo(HaveOccurred())
	setup(mgr)
	mgrDone := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(mgrDone)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	DeferCleanup(func() { stop(); <-mgrDone })
}

// annotateRepeatedly makes n metadata-only updates of obj (a new value of an
// unrelated annotation each time), as a tenant looping kubectl annotate would.
func annotateRepeatedly(obj client.Object, n int) {
	for i := 0; i < n; i++ {
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				return err
			}
			ann := obj.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann["example.com/poke"] = fmt.Sprintf("%d", i)
			obj.SetAnnotations(ann)
			return k8sClient.Update(ctx, obj)
		})).To(Succeed())
	}
}

// holdTestProvider is a libvirt Provider CR in ns (single host).
func holdTestProvider(ns string) *infravirtrigaudiov1beta1.Provider {
	return &infravirtrigaudiov1beta1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov-1", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.ProviderSpec{
			Type:                infravirtrigaudiov1beta1.ProviderTypeLibvirt,
			Endpoint:            "qemu+ssh://virt@kvm-01/system",
			CredentialSecretRef: infravirtrigaudiov1beta1.ObjectRef{Name: "creds"},
			Runtime: &infravirtrigaudiov1beta1.ProviderRuntimeSpec{
				Mode:  infravirtrigaudiov1beta1.RuntimeModeRemote,
				Image: "virtrigaud/provider-libvirt:test",
			},
		},
	}
}

// holdTestVM creates the provisioned VirtualMachine name in ns on prov-1,
// with the given observed power state.
func holdTestVM(ns, name string, power infravirtrigaudiov1beta1.ObservedPowerState) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
		},
	}
	Expect(k8sClient.Create(ctx, vm)).To(Succeed())
	vm.Status.ID = ns + "." + name
	vm.Status.PowerState = power
	Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
	return vm
}

var _ = Describe("Held clones and snapshot deletes ignore metadata-only updates (envtest)", func() {
	It("never re-runs a clone held for SourceMustBePoweredOff on a metadata-only update; the source's power-off still re-drives it", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "hold-clone-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		cp := &countingCloner{}
		cp.answer(contracts.CloneResponse{}, sourceRunningAnswer())
		startHoldTestManager(func(mgr ctrl.Manager) {
			Expect(NewVMCloneReconciler(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme(), &stubResolver{provider: cp},
				record.NewFakeRecorder(100)).SetupWithManager(mgr)).To(Succeed())
		})

		Expect(k8sClient.Create(ctx, holdTestProvider(ns.Name))).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		src := holdTestVM(ns.Name, "src-vm", infravirtrigaudiov1beta1.ObservedPowerStateOn)
		clone := &infravirtrigaudiov1beta1.VMClone{
			ObjectMeta: metav1.ObjectMeta{Name: "clone-h", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VMCloneSpec{
				Source: infravirtrigaudiov1beta1.CloneSource{VMRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target: infravirtrigaudiov1beta1.VMCloneTarget{Name: "web"},
			},
		}
		Expect(k8sClient.Create(ctx, clone)).To(Succeed())

		By("the clone waits for its source to be powered off")
		Eventually(func(g Gomega) {
			got := &infravirtrigaudiov1beta1.VMClone{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(clone), got)).To(Succeed())
			ready := readyCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).To(Equal(cloneReasonSourceMustBePoweredOff))
		}, "20s", "100ms").Should(Succeed())
		// The hold settles at once: its own status write does not re-run it.
		Consistently(cp.calls.Load, "1s", "100ms").Should(Equal(int32(1)))

		By("re-annotating the held clone in a loop makes no Clone call")
		annotateRepeatedly(clone, 10)
		Consistently(cp.calls.Load, "2s", "100ms").Should(Equal(int32(1)),
			"a metadata-only update never re-runs a held clone ahead of its backoff")

		By("the source's power-off re-drives it through the source power watch")
		cp.answer(contracts.CloneResponse{TargetVmID: ns.Name + ".web"}, nil)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(src), src)).To(Succeed())
		src.Status.PowerState = infravirtrigaudiov1beta1.ObservedPowerStateOff
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
		Eventually(func(g Gomega) {
			got := &infravirtrigaudiov1beta1.VMClone{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(clone), got)).To(Succeed())
			g.Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.ClonePhaseReady))
		}, "10s", "100ms").Should(Succeed(), "well before the 15 s backoff")
		Expect(cp.calls.Load()).To(Equal(int32(2)))
	})

	It("never re-runs a held snapshot delete on a metadata-only update; the force-delete annotation still releases it", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "hold-snap-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, holdTestProvider(ns.Name))).To(Succeed())
		holdTestVM(ns.Name, "web", infravirtrigaudiov1beta1.ObservedPowerStateOn)

		// A Ready snapshot, recorded before the controller starts.
		snap := &infravirtrigaudiov1beta1.VMSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-h", Namespace: ns.Name,
				Finalizers: []string{"snapshot.infra.virtrigaud.io/finalizer"}},
			Spec: infravirtrigaudiov1beta1.VMSnapshotSpec{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "web"}},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		now := metav1.NewTime(time.Now())
		snap.Status.Phase = infravirtrigaudiov1beta1.SnapshotPhaseReady
		snap.Status.SnapshotID = "snap-1"
		snap.Status.CreationTime = &now
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		spy := &snapshotDeleteSpy{}
		spy.answer(contracts.NewHostUnavailableError(`snapshotDelete: host "kvm-01" is unreachable`, nil))
		startHoldTestManager(func(mgr ctrl.Manager) {
			r := NewVMSnapshotReconciler(mgr.GetClient(), mgr.GetScheme(), nil, record.NewFakeRecorder(100), false)
			r.providerInstanceFn = func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
				return spy, nil
			}
			Expect(r.SetupWithManager(mgr)).To(Succeed())
		})

		By("the delete fails and is held")
		Expect(k8sClient.Delete(ctx, snap)).To(Succeed())
		Eventually(func(g Gomega) {
			got := &infravirtrigaudiov1beta1.VMSnapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(snap), got)).To(Succeed())
			deleting := readyCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMSnapshotConditionDeleting)
			g.Expect(deleting).NotTo(BeNil())
			g.Expect(deleting.Status).To(Equal(metav1.ConditionFalse))
		}, "20s", "100ms").Should(Succeed())
		Consistently(spy.calls.Load, "1s", "100ms").Should(Equal(int32(1)))

		By("re-annotating the held snapshot in a loop makes no SnapshotDelete call")
		annotateRepeatedly(snap, 10)
		Consistently(spy.calls.Load, "2s", "100ms").Should(Equal(int32(1)),
			"a metadata-only update never re-runs a held delete ahead of its backoff")

		By("the force-delete annotation releases it at once")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(snap), snap); err != nil {
				return err
			}
			snap.Annotations[forceDeleteAnnotation] = "true"
			return k8sClient.Update(ctx, snap)
		})).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(snap), &infravirtrigaudiov1beta1.VMSnapshot{}))
		}, "10s", "100ms").Should(BeTrue(), "well before the 15 s backoff")
		Expect(spy.calls.Load()).To(Equal(int32(2)))
	})
})
