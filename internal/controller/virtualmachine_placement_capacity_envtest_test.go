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
)

// A running VirtualMachine controller (10 concurrent reconciles, a real
// informer cache) creating many VMs at once against a clustered Provider whose
// only host fits a few of them (ADR-0007 Addendum A, scheduler-accuracy
// amendment).
var _ = Describe("Clustered scheduling against committed capacity (envtest)", func() {
	ctx := context.Background()

	It("places exactly as many VMs as the host fits and reports the rest Unschedulable", func() {
		const (
			vmCount = 8
			fits    = 3 // host: 6 vCPU; every VM: 2 vCPU
		)
		mgrCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "sched-cap-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })
		// Remove this spec's VMs once its manager has stopped (DeferCleanup is
		// LIFO, and the manager's stop is registered later). envtest never
		// finishes deleting a namespace, so VMs left behind with finalizers
		// would be reconciled by the next spec's manager.
		DeferCleanup(func() {
			var vms infravirtrigaudiov1beta1.VirtualMachineList
			Expect(k8sClient.List(ctx, &vms, client.InNamespace(ns.Name))).To(Succeed())
			for i := range vms.Items {
				key := client.ObjectKeyFromObject(&vms.Items[i])
				Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
					vm := &infravirtrigaudiov1beta1.VirtualMachine{}
					if err := k8sClient.Get(ctx, key, vm); err != nil {
						return client.IgnoreNotFound(err)
					}
					vm.Finalizers = nil
					return k8sClient.Update(ctx, vm)
				})).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &vms.Items[i]))).To(Succeed())
			}
			Eventually(func(g Gomega) {
				var left infravirtrigaudiov1beta1.VirtualMachineList
				g.Expect(k8sClient.List(ctx, &left, client.InNamespace(ns.Name))).To(Succeed())
				g.Expect(left.Items).To(BeEmpty())
			}, "10s", "100ms").Should(Succeed())
		})

		prov := envtestProvider(ns.Name, "clustered", nil)
		prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())

		Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.HostPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.HostPoolSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: prov.Name},
				Strategy:    infravirtrigaudiov1beta1.PoolStrategySpread,
			},
		})).To(Succeed())
		host := &infravirtrigaudiov1beta1.Host{
			ObjectMeta: metav1.ObjectMeta{Name: "kvm-01", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.HostSpec{
				ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: prov.Name},
				PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"},
				Endpoint:    "qemu+ssh://virt@kvm-01/system",
				Schedulable: true,
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		cpu, mem := int32(2*fits), int64(1<<20)
		host.Status = infravirtrigaudiov1beta1.HostStatus{
			Health: infravirtrigaudiov1beta1.HostHealthReady, AllocatableCPU: &cpu, AllocatableMemoryMiB: &mem,
		}
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		provider := &envtestVMProvider{}
		Expect((&VirtualMachineReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), RemoteResolver: &atomicResolver{provider: provider},
			Recorder: record.NewFakeRecorder(1000),
		}).SetupWithManager(mgr)).To(Succeed())
		mgrDone := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(mgrDone)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() { stop(); <-mgrDone })

		By("creating all VMs at once")
		for i := 0; i < vmCount; i++ {
			Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("vm-%d", i), Namespace: ns.Name},
				Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
					ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: prov.Name},
					ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
					ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: fmt.Sprintf("disk-%d", i), Format: "qcow2", Source: "manual"},
				},
			})).To(Succeed())
		}

		// tally counts the VMs holding the host (bound or pending) and the
		// VMs reported Unschedulable.
		tally := func(g Gomega) (holding, unschedulable int) {
			var vms infravirtrigaudiov1beta1.VirtualMachineList
			g.Expect(k8sClient.List(ctx, &vms, client.InNamespace(ns.Name))).To(Succeed())
			for i := range vms.Items {
				vm := &vms.Items[i]
				if pl := vm.Status.Placement; pl != nil && (pl.Host != "" || pl.PendingHost != "") {
					holding++
					continue
				}
				if c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionPlaced); c != nil && c.Reason == k8s.ReasonUnschedulable {
					g.Expect(c.Message).To(ContainSubstring("insufficient CPU on 1 of 1 candidate host(s): requested 2 vCPU, at most 0 free"))
					unschedulable++
				}
			}
			return holding, unschedulable
		}

		By("placing exactly the VMs the host fits")
		Eventually(func(g Gomega) {
			holding, unschedulable := tally(g)
			g.Expect(holding).To(Equal(fits))
			g.Expect(unschedulable).To(Equal(vmCount - fits))
		}, "30s", "200ms").Should(Succeed())

		By("never overbooking afterwards")
		Consistently(func(g Gomega) {
			holding, _ := tally(g)
			g.Expect(holding).To(Equal(fits))
			g.Expect(provider.creates.Load()).To(BeNumerically("==", fits))
		}, "3s", "200ms").Should(Succeed())
	})
})

// The VirtualMachine and Host controllers both use the placement-Provider field
// index (review L8) and both register it; one manager must accept both.
var _ = Describe("Placement-Provider field index registration (envtest)", func() {
	It("is registered once per manager, whichever controller comes first", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&HostReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		Expect((&VirtualMachineReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		Expect(indexPlacementProvider(mgr)).To(Succeed())
	})
})
