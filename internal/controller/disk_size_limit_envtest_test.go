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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These specs prove the disk-size limit's condition writes against a real API
// server: the hold is persisted (status subresource, ObservedGeneration),
// a repeated hold writes nothing, and the hold ends with a spec edit.

// removeVMFinalizersOnCleanup deletes vm, and its finalizers, when the spec
// ends, so it never reaches another spec's provider.
func removeVMFinalizersOnCleanup(vm *infravirtrigaudiov1beta1.VirtualMachine) {
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
}

var _ = Describe("Disk-size limit (envtest)", func() {
	It("holds a VM create above the limit with a persisted condition, writes nothing on a repeat, and creates once the size fits", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "disk-cap-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		prov := holdTestProvider(ns.Name)
		Expect(k8sClient.Create(ctx, prov)).To(Succeed())
		prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
		Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: prov.Name},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
				Disks:        []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 200}},
			},
		}
		Expect(k8sClient.Create(ctx, vm)).To(Succeed())
		removeVMFinalizersOnCleanup(vm)
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}

		rp := &routingProvider{onCreate: func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
			return contracts.CreateResponse{ID: req.Owner.Namespace + "." + req.Name}, nil
		}}
		rec := record.NewFakeRecorder(100)
		limit := resource.MustParse("100Gi")
		r := &VirtualMachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			RemoteResolver: &stubResolver{provider: rp}, Recorder: rec, MaxDiskSize: &limit,
		}

		_, err := r.Reconcile(ctx, req) // adds the finalizer
		Expect(err).NotTo(HaveOccurred())
		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(diskSizeLimitRetryInterval))
		Expect(rp.createReqs).To(BeEmpty(), "no Create is sent")

		held := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, held)).To(Succeed())
		for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
			c := meta.FindStatusCondition(held.Status.Conditions, condType)
			Expect(c).NotTo(BeNil(), condType)
			Expect(c.Status).To(Equal(metav1.ConditionFalse), condType)
			Expect(c.Reason).To(Equal(k8s.ReasonDiskSizeExceedsLimit), condType)
			Expect(c.Message).To(ContainSubstring("spec.disks[data].sizeGiB asks for 200Gi, above the disk-size limit 100Gi"), condType)
			Expect(c.ObservedGeneration).To(Equal(held.Generation), condType)
		}
		Expect(diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit)).To(HaveLen(1))

		// A repeated hold writes nothing and records no event.
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		again := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, again)).To(Succeed())
		Expect(again.ResourceVersion).To(Equal(held.ResourceVersion), "a repeated hold does not write the VM")
		Expect(diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit)).To(BeEmpty())

		// Lowering the disk to the limit ends the hold: the VM is created.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			fresh := &infravirtrigaudiov1beta1.VirtualMachine{}
			if err := k8sClient.Get(ctx, req.NamespacedName, fresh); err != nil {
				return err
			}
			fresh.Spec.Disks[0].SizeGiB = 100
			return k8sClient.Update(ctx, fresh)
		})).To(Succeed())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(rp.createReqs).To(HaveLen(1))
		Expect(rp.createReqs[0].Disks[0].SizeGiB).To(Equal(int32(100)))
		created := &infravirtrigaudiov1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, req.NamespacedName, created)).To(Succeed())
		Expect(created.Status.ID).To(Equal(ns.Name + ".web"))
	})

	It("holds a VMMigration above the target's limit in Validating with a persisted condition", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "disk-cap-mig-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		ready := func(name string, ptype infravirtrigaudiov1beta1.ProviderType, annotations map[string]string) {
			p := holdTestProvider(ns.Name)
			p.Name = name
			p.Spec.Type = ptype
			p.Annotations = annotations
			if ptype == infravirtrigaudiov1beta1.ProviderTypeVSphere {
				p.Spec.Endpoint = "https://vcenter.example.invalid"
				p.Spec.Runtime.Image = "virtrigaud/provider-vsphere:test"
			}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
				Type: "ProviderAvailable", Status: metav1.ConditionTrue, Reason: "RemoteAvailable", Message: "ready",
			})
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		}
		ready("src-prov", infravirtrigaudiov1beta1.ProviderTypeVSphere, nil)
		ready("tgt-prov", infravirtrigaudiov1beta1.ProviderTypeLibvirt,
			map[string]string{infravirtrigaudiov1beta1.ProviderMaxDiskSizeAnnotation: "50Gi"})
		srcClass := envtestClass(ns.Name, "big", nil)
		srcClass.Spec.DiskDefaults = &infravirtrigaudiov1beta1.DiskDefaults{Size: resource.MustParse("80Gi")}
		Expect(k8sClient.Create(ctx, srcClass)).To(Succeed())
		src := &infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "src-vm", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: "src-prov"},
				ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "big"},
				ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-1", Format: "qcow2", Source: "manual"},
			},
		}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		removeVMFinalizersOnCleanup(src)
		src.Status.ID = "vm-1"
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())

		migration := &infravirtrigaudiov1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns.Name},
			Spec: infravirtrigaudiov1beta1.VMMigrationSpec{
				Source: infravirtrigaudiov1beta1.MigrationSource{VMRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
				Target: infravirtrigaudiov1beta1.MigrationTarget{
					Name:        "web",
					ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "tgt-prov"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, migration)).To(Succeed())
		migration.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
		Expect(k8sClient.Status().Update(ctx, migration)).To(Succeed())

		spy := &migrationSpy{}
		rec := record.NewFakeRecorder(100)
		ceiling := resource.MustParse("1Ti")
		r := &VMMigrationReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient, Recorder: rec, MaxDiskSize: &ceiling,
			providerInstanceFn: func(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
				return spy, nil
			},
		}

		get := func() *infravirtrigaudiov1beta1.VMMigration {
			m := &infravirtrigaudiov1beta1.VMMigration{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(migration), m)).To(Succeed())
			return m
		}
		res, err := r.handleValidatingPhase(ctx, get())
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(diskSizeLimitRetryInterval))

		held := get()
		Expect(held.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseValidating), "held, not failed")
		c := meta.FindStatusCondition(held.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(k8s.ReasonDiskSizeExceedsLimit))
		Expect(c.Message).To(ContainSubstring("VMClass big spec.diskDefaults.size asks for 80Gi, above the disk-size limit 50Gi: nothing is migrated"))
		Expect(c.ObservedGeneration).To(Equal(held.Generation))
		imports, exports, snapshots, power := spy.calls()
		Expect(imports + exports + snapshots + power).To(BeZero(), "no provider call")
		Expect(diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit)).To(HaveLen(1))

		_, err = r.handleValidatingPhase(ctx, get())
		Expect(err).NotTo(HaveOccurred())
		Expect(get().ResourceVersion).To(Equal(held.ResourceVersion), "a repeated hold does not write the migration")
		Expect(diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit)).To(BeEmpty())
	})
})
