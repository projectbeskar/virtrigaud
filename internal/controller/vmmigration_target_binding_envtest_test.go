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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These envtest specs run the VMMigration controller against a real
// apiserver (real UIDs, a real informer cache) for the binding between a
// migration and the target VirtualMachine it created (security review M1): a
// VirtualMachine planted under the target's name — seen by the cache, found
// only at Validating-Target, or created in the window before the Create —
// fails the migration and is never touched; the migration's own
// VirtualMachine is recognized again after a manager restart that lost its
// recorded UID.
var _ = Describe("VMMigration target binding (envtest)", func() {
	const targetName = "web"

	noProvider := func(_ context.Context, p *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
		return nil, fmt.Errorf("no provider call is expected (provider %s/%s)", p.Namespace, p.Name)
	}

	// startManager starts a manager running only the VMMigration controller
	// and returns its stop function (also run when the spec ends).
	startManager := func() func() {
		mgrCtx, cancelMgr := context.WithCancel(ctx)
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		r := NewVMMigrationReconciler(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetScheme(), nil, record.NewFakeRecorder(200), false)
		r.providerInstanceFn = noProvider
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		var once sync.Once
		stop := func() { once.Do(func() { cancelMgr(); <-done }) }
		DeferCleanup(stop)
		return stop
	}

	// setupMigration creates a fresh namespace with the target Provider and a
	// VMMigration whose disk is imported, in phase (Creating or
	// Validating-Target), its status adjusted by mutate.
	setupMigration := func(phase infravirtrigaudiov1beta1.MigrationPhase,
		mutate func(*infravirtrigaudiov1beta1.VMMigration)) *infravirtrigaudiov1beta1.VMMigration {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "tgtbind-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		Expect(k8sClient.Create(ctx, holdTestProvider(nsObj.Name))).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(nsObj.Name, "small", nil))).To(Succeed())
		m := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: nsObj.Name,
				Finalizers: []string{infravirtrigaudiov1beta1.VMMigrationFinalizer}},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{Name: targetName,
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
					ClassRef:    &infravirtrigaudiov1beta1.LocalObjectReference{Name: "small"}},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		m.Status.Phase = phase
		m.Status.StartTime = &metav1.Time{Time: m.CreationTimestamp.Time}
		m.Status.ImportID = nsObj.Name + "." + targetName + "-migrated"
		m.Status.DiskInfo = &infravirtrigaudiov1beta1.MigrationDiskInfo{
			TargetPath:   "/var/lib/libvirt/images/" + m.Status.ImportID + ".qcow2",
			TargetFormat: "qcow2",
		}
		if mutate != nil {
			mutate(m)
		}
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		return m
	}

	// plant creates, as someone other than the migration, a VirtualMachine
	// under the target's name carrying every forgeable provenance field.
	plant := func(m *infravirtrigaudiov1beta1.VMMigration) *infravirtrigaudiov1beta1.VirtualMachine {
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: m.Namespace,
				Annotations: map[string]string{migrationProvenanceAnnotation: migrationProvenance(m)}},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-1"},
				ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: m.Status.ImportID,
					Path: m.Status.DiskInfo.TargetPath, Source: "migration",
					MigrationRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: m.Name}},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		return vm
	}

	markReady := func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), vm)).To(Succeed())
		vm.Status.ID = "vm-" + string(vm.UID)
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue,
			Reason: "Ready", ObservedGeneration: vm.Generation})
		Expect(k8sClient.Status().Update(ctx, vm)).To(Succeed())
	}

	stored := func(g Gomega, m *infravirtrigaudiov1beta1.VMMigration) *infravirtrigaudiov1beta1.VMMigration {
		got := &infravirtrigaudiov1beta1.VMMigration{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got)).To(Succeed())
		return got
	}

	expectFailedNotOwned := func(m *infravirtrigaudiov1beta1.VMMigration) {
		Eventually(func(g Gomega) {
			got := stored(g, m)
			g.Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseFailed))
			g.Expect(got.Status.Message).To(Equal(targetVMNotOwnedMessage))
			failed := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionFailed)
			g.Expect(failed).NotTo(BeNil())
			g.Expect(failed.Reason).To(Equal(ReasonTargetVMNotOwned))
			g.Expect(got.Status.TargetVMID).To(BeEmpty())
		}, "20s", "200ms").Should(Succeed())
	}

	expectUntouched := func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		Consistently(func(g Gomega) {
			now := &infravirtrigaudiov1beta1.VirtualMachine{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), now)).To(Succeed())
			g.Expect(now.UID).To(Equal(vm.UID))
			g.Expect(now.Annotations).NotTo(HaveKey("virtrigaud.io/migration-completed"))
			g.Expect(now.Annotations).NotTo(HaveKey(migrationTargetClaimAnnotation))
		}, "2s", "250ms").Should(Succeed())
	}

	It("fails on a VirtualMachine planted under the target's name before Creating, without sending a Create", func() {
		m := setupMigration(infravirtrigaudiov1beta1.MigrationPhaseCreating, nil)
		planted := plant(m)
		markReady(planted) // even a Ready one is never taken as the target
		startManager()

		expectFailedNotOwned(m)
		expectUntouched(planted)
		Expect(stored(Default, m).Status.TargetVMClaim).To(BeEmpty(), "no claim: no Create was attempted")
	})

	It("fails at Validating-Target on a VirtualMachine it did not create and never marks it completed", func() {
		m := setupMigration(infravirtrigaudiov1beta1.MigrationPhaseValidatingTarget, func(m *infravirtrigaudiov1beta1.VMMigration) {
			// The VM it created was deleted; someone created the name again.
			m.Status.TargetVMUID = "00000000-0000-4000-8000-0000000000aa"
		})
		planted := plant(m)
		markReady(planted)
		startManager()

		expectFailedNotOwned(m)
		expectUntouched(planted)
	})

	It("fails when the name is taken between its live check and its Create (AlreadyExists)", func() {
		m := setupMigration(infravirtrigaudiov1beta1.MigrationPhaseCreating, nil)
		direct, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		var planted *infravirtrigaudiov1beta1.VirtualMachine
		racing := interceptor.NewClient(direct, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine); ok && planted == nil {
					planted = plant(m)
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		r := NewVMMigrationReconciler(racing, direct, k8sClient.Scheme(), nil, record.NewFakeRecorder(50), false)
		r.providerInstanceFn = noProvider

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)})
		Expect(err).NotTo(HaveOccurred())
		Expect(planted).NotTo(BeNil(), "the Create was sent and answered AlreadyExists")
		expectFailedNotOwned(m)
		expectUntouched(planted)
	})

	It("records its own VM's UID, recognizes it again after a restart that lost the UID, and completes only with it", func() {
		m := setupMigration(infravirtrigaudiov1beta1.MigrationPhaseCreating, nil)
		stop := startManager()

		own := &infravirtrigaudiov1beta1.VirtualMachine{}
		Eventually(func(g Gomega) {
			got := stored(g, m)
			g.Expect(got.Status.TargetVMUID).NotTo(BeEmpty())
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: targetName}, own)).To(Succeed())
			g.Expect(string(own.UID)).To(Equal(got.Status.TargetVMUID))
			g.Expect(migrationTargetClaimDigest(own.Annotations[migrationTargetClaimAnnotation])).To(Equal(got.Status.TargetVMClaim))
		}, "20s", "200ms").Should(Succeed())

		// A restart that lost the UID record (the claim, written before the
		// Create, survives).
		stop()
		lost := stored(Default, m)
		lost.Status.TargetVMUID = ""
		Expect(k8sClient.Status().Update(ctx, lost)).To(Succeed())
		startManager()

		Eventually(func(g Gomega) {
			g.Expect(stored(g, m).Status.TargetVMUID).To(Equal(string(own.UID)))
		}, "20s", "200ms").Should(Succeed())
		vms := &infravirtrigaudiov1beta1.VirtualMachineList{}
		Expect(k8sClient.List(ctx, vms, client.InNamespace(m.Namespace))).To(Succeed())
		Expect(vms.Items).To(HaveLen(1), "the VM is not created again")
		Expect(stored(Default, m).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseCreating))

		// The controller does not watch VirtualMachines: the Creating phase
		// re-checks its target every targetVMWaitInterval.
		markReady(own)
		Eventually(func(g Gomega) {
			got := stored(g, m)
			g.Expect(got.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
			g.Expect(got.Status.TargetVMID).To(Equal("vm-" + string(own.UID)))
			vm := &infravirtrigaudiov1beta1.VirtualMachine{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(own), vm)).To(Succeed())
			g.Expect(vm.Annotations).To(HaveKeyWithValue("virtrigaud.io/migration-completed", "true"))
		}, targetVMWaitInterval+20*time.Second, "200ms").Should(Succeed())
	})
})
