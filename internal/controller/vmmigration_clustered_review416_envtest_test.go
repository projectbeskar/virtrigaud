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
	"errors"
	"slices"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The #416 review findings on the migration into a clustered provider
// (ADR-0007 Addendum B), against a real apiserver with the VirtualMachine and
// VMMigration controllers running.
var _ = Describe("Migration into a clustered provider: #416 review (envtest)", func() {
	// pinAfterImport makes every Create fail with a name conflict, so the
	// target stays unbound after its import (ImportedDiskPinned), and returns
	// the first import once it was sent.
	pinAfterImport := func(f *routedFixture) contracts.ImportDiskRequest {
		var first contracts.ImportDiskRequest
		Eventually(func(g Gomega) {
			vm := f.getTarget(g)
			g.Expect(condReason(vm.Status.Conditions, k8s.ConditionPlaced)).To(Equal(k8s.ReasonImportedDiskPinned))
			_, imports, _, _ := f.prov.snapshot()
			g.Expect(imports).To(HaveLen(1))
			first = imports[0]
		}, "60s", "250ms").Should(Succeed())
		return first
	}
	deleteTarget := func(f *routedFixture) {
		Expect(k8sClient.Delete(ctx, &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: "web"}})).To(Succeed())
	}

	It("H1: a retry whose landed disk was deleted imports again onto its fresh target", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) {
			p.createErr = contracts.NewConflictError("create: a domain with this name exists", nil)
		})
		f := newRoutedFixture(prov, routedFixtureOptions{retry: true, hosts: []routedHost{
			{name: "kvm-01", cpu: 16, storageBytes: storageGiB(100)},
			{name: "kvm-02", cpu: 8, storageBytes: storageGiB(100)},
		}})
		first := pinAfterImport(f)

		By("the unbound target is deleted in Creating: the attempt fails, and the retry deletes its landed disk")
		prov.set(func(p *routedMigrationProvider) { p.createErr = nil })
		deleteTarget(f)

		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "90s", "250ms").Should(Succeed())
		exports, imports, creates, landed := prov.snapshot()
		Expect(landed).To(HaveLen(1))
		Expect(landed[0].TargetHostID).To(Equal(first.TargetHostID))
		Expect(imports).To(HaveLen(2), "imported again: the deleted disk is never handed to the fresh target")
		Expect(imports[1].TargetVM.UID).NotTo(Equal(first.TargetVM.UID))
		Expect(exports).To(Equal(1), "the staged object is still there and is not exported again")
		m := f.getMigration(Default)
		target := f.getTarget(Default)
		Expect(string(target.UID)).To(Equal(imports[1].TargetVM.UID))
		Expect(target.Spec.ImportedDisk.DiskID).To(Equal(m.Status.ImportID))
		Expect(m.Status.StorageInfo.ImportTarget.Host).To(Equal(imports[1].TargetHostID))
		Expect(creates[len(creates)-1].TargetHostID).To(Equal(imports[1].TargetHostID))
		Expect(meta.FindStatusCondition(m.Status.Conditions, migrationConditionImportRequested)).To(BeNil(), "no stale marker (L1)")
	})

	It("H1/L3: a retry whose landed disk is kept places its fresh target on that host only and reuses the disk", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) {
			p.createErr = contracts.NewConflictError("create: a domain with this name exists", nil)
		})
		f := newRoutedFixture(prov, routedFixtureOptions{retry: true, cleanupNever: true, hosts: []routedHost{
			{name: "kvm-01", cpu: 16, storageBytes: storageGiB(100)},
			{name: "kvm-02", cpu: 8, storageBytes: storageGiB(100)},
		}})
		first := pinAfterImport(f)
		Expect(first.TargetHostID).To(Equal("kvm-01"))

		By("kvm-02 becomes the scheduler's favourite before the retry")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			h := &infravirtrigaudiov1beta1.Host{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "kvm-02"}, h); err != nil {
				return err
			}
			cpu := int32(128)
			h.Status.AllocatableCPU = &cpu
			return k8sClient.Status().Update(ctx, h)
		})).To(Succeed())
		prov.set(func(p *routedMigrationProvider) { p.createErr = nil })
		deleteTarget(f)

		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "90s", "250ms").Should(Succeed())
		_, imports, creates, landed := prov.snapshot()
		Expect(landed).To(BeEmpty(), "cleanupPolicy Never keeps the landed disk")
		Expect(imports).To(HaveLen(1), "the kept disk is reused, never landed again elsewhere")
		target := f.getTarget(Default)
		Expect(string(target.UID)).NotTo(Equal(first.TargetVM.UID), "a fresh target")
		Expect(target.Status.Placement.Host).To(Equal("kvm-01"))
		Expect(creates[len(creates)-1].TargetHostID).To(Equal("kvm-01"))
		Expect(creates[len(creates)-1].Image.Path).To(Equal(f.getMigration(Default).Status.DiskInfo.TargetPath))
	})

	releaseWriteFails := func(errFor func() error) func(client.Object) error {
		var mu sync.Mutex
		failed := false
		return func(obj client.Object) error {
			vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
			if !ok || vm.Name != "web" || pendingHost(vm) != "" || vm.Status.Placement == nil ||
				!slices.Contains(vm.Status.Placement.ExcludedHosts, "kvm-01") {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			if failed {
				return nil
			}
			failed = true
			return errFor()
		}
	}

	It("M1: a release write that loses a conflict is retried in place", func() {
		prov := newRoutedMigrationProvider()
		prov.set(func(p *routedMigrationProvider) {
			p.importErrs = []error{contracts.NewConflictError("importDisk: a libvirt domain with the target's name exists", nil)}
		})
		f := newRoutedFixture(prov, routedFixtureOptions{
			hosts: []routedHost{{name: "kvm-01", cpu: 16, storageBytes: storageGiB(100)}, {name: "kvm-02", cpu: 8, storageBytes: storageGiB(100)}},
			migStatusErr: releaseWriteFails(func() error {
				return apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "virtualmachines"}, "web",
					errors.New("the object has been modified"))
			}),
		})
		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "90s", "250ms").Should(Succeed())
		_, imports, _, _ := prov.snapshot()
		Expect(hostsOf(imports)).To(Equal([]string{"kvm-01", "kvm-02"}))
	})

	It("M1: a crash between the migration write and the VM release heals itself", func() {
		prov := newRoutedMigrationProvider()
		conflict := func() error {
			return contracts.NewConflictError("importDisk: a libvirt domain with the target's name exists", nil)
		}
		prov.set(func(p *routedMigrationProvider) { p.importErrs = []error{conflict(), conflict()} })
		f := newRoutedFixture(prov, routedFixtureOptions{
			hosts:        []routedHost{{name: "kvm-01", cpu: 16, storageBytes: storageGiB(100)}, {name: "kvm-02", cpu: 8, storageBytes: storageGiB(100)}},
			migStatusErr: releaseWriteFails(func() error { return errors.New("connection reset by peer") }),
		})
		Eventually(func(g Gomega) {
			g.Expect(f.getMigration(g).Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
		}, "90s", "250ms").Should(Succeed())
		_, imports, _, _ := prov.snapshot()
		Expect(hostsOf(imports)).To(Equal([]string{"kvm-01", "kvm-01", "kvm-02"}),
			"the VM stayed on kvm-01, the import was refused there again and the release was made then")
	})

	It("M2: a target whose spec.importedDisk is changed during the import never completes the migration", func() {
		prov := newRoutedMigrationProvider()
		var f *routedFixture
		prov.set(func(p *routedMigrationProvider) {
			p.onImport = func(contracts.ImportDiskRequest) {
				Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
					vm := &infravirtrigaudiov1beta1.VirtualMachine{}
					if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "web"}, vm); err != nil {
						return err
					}
					vm.Spec.ImportedDisk.DiskID = "other-disk"
					vm.Spec.ImportedDisk.Path = "/var/lib/libvirt/images/other-disk.qcow2"
					return k8sClient.Update(ctx, vm)
				})).To(Succeed())
			}
		})
		f = newRoutedFixture(prov, routedFixtureOptions{hosts: []routedHost{{name: "kvm-01", cpu: 8, storageBytes: storageGiB(100)}}})

		Eventually(func(g Gomega) {
			m := f.getMigration(g)
			g.Expect(condReason(m.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionReady)).To(Equal(migrationReasonTargetDiskMismatch))
		}, "60s", "250ms").Should(Succeed())
		Consistently(func(g Gomega) {
			m := f.getMigration(g)
			g.Expect(m.Status.Phase).NotTo(Equal(infravirtrigaudiov1beta1.MigrationPhaseReady))
			g.Expect(m.Status.Phase).NotTo(Equal(infravirtrigaudiov1beta1.MigrationPhaseValidatingTarget))
		}, "3s", "250ms").Should(Succeed())
		Expect(drainRecordedEvents(f.rec)).To(ContainElement(ContainSubstring(migrationReasonTargetDiskMismatch)))
	})

	It("M3: a landing host that keeps answering not performed fails the attempt, and the unbound target is deleted", func() {
		prov := newRoutedMigrationProvider()
		notPerformed := func() error {
			return contracts.NewRetryableError("importDisk: not performed", contracts.ErrOperationNotPerformed)
		}
		prov.set(func(p *routedMigrationProvider) {
			for i := 0; i < 10; i++ {
				p.importErrs = append(p.importErrs, notPerformed())
			}
		})
		f := newRoutedFixture(prov, routedFixtureOptions{notPerformedBudget: 2 * time.Second,
			hosts: []routedHost{{name: "kvm-01", cpu: 8, storageBytes: storageGiB(100)}}})
		Eventually(func(g Gomega) {
			m := f.getMigration(g)
			g.Expect(m.Status.Phase).To(Equal(infravirtrigaudiov1beta1.MigrationPhaseFailed))
			g.Expect(condReason(m.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionFailed)).To(Equal(migrationReasonImportNotPerformed))
		}, "60s", "250ms").Should(Succeed())
		_, imports, _, _ := prov.snapshot()
		Expect(len(imports)).To(BeNumerically(">=", 2))
		Expect(len(imports)).To(BeNumerically("<", 10), "bounded, not retried forever")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "web"},
				&infravirtrigaudiov1beta1.VirtualMachine{}))
		}, "30s", "250ms").Should(BeTrue(), "the migration failed for good: its unbound target is deleted (B1.4)")
	})
})

// hostsOf lists the target hosts of imports.
func hostsOf(imports []contracts.ImportDiskRequest) []string {
	out := make([]string, 0, len(imports))
	for _, r := range imports {
		out = append(out, r.TargetHostID)
	}
	return out
}
