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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A clone or a migration whose source VM the provider refuses as not the
// source VirtualMachine's (VM_NOT_OWNED: a reused id) or as one whose owner
// cannot be told (OWNER_STAMP_UNREADABLE) is failed at once with a constant
// message, before anything is exported, snapshotted, cloned or powered off.

// ownerRefusals are the two owner refusals, as the transport maps them.
func ownerRefusals(op string) map[string]error {
	return map[string]error{
		"VM_NOT_OWNED":           vmNotOwnedAnswer(op),
		"OWNER_STAMP_UNREADABLE": ownerStampUnreadableAnswer(op),
	}
}

// assertMigrationFailedNotOwned asserts got failed with the constant message
// and carries no provider text.
func assertMigrationFailedNotOwned(t *testing.T, got *infrav1beta1.VMMigration) {
	t.Helper()
	assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
	assert.Equal(t, migrationSourceNotOwnedMessage, got.Status.Message)
	assert.NotContains(t, got.Status.Message, "rpc error")
	c := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionFailed)
	require.NotNil(t, c)
	assert.Equal(t, migrationSourceNotOwnedMessage, c.Message)
}

// TestVMMigration_SourceOwnerRefusal_DiskSizeCheckInValidating: the disk-size
// check's GetDiskInfo answered with an owner refusal fails the migration (it is
// not "a size that cannot be read", which holds it), before anything else.
func TestVMMigration_SourceOwnerRefusal_DiskSizeCheckInValidating(t *testing.T) {
	for name, refusal := range ownerRefusals("getDiskInfo") {
		t.Run(name, func(t *testing.T) {
			migration, objs := xnsMigration("")
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
			prov := &sizedMigrationProvider{sizes: &diskSizes{primary: 10 * gib, err: refusal}}
			r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)
			r.MaxDiskSize = qty("100Gi")

			_, err := r.handleValidatingPhase(context.Background(), migration)
			require.NoError(t, err)
			assertMigrationFailedNotOwned(t, getXNSMigration(t, r, migration))
			assert.Zero(t, prov.exports+prov.imports)
		})
	}
}

// TestVMMigration_SourceOwnerRefusal_DiskSizeCheckBeforeExport: the same for
// the read right before ExportDisk; no export is sent.
func TestVMMigration_SourceOwnerRefusal_DiskSizeCheckBeforeExport(t *testing.T) {
	for name, refusal := range ownerRefusals("getDiskInfo") {
		t.Run(name, func(t *testing.T) {
			prov := &sizedMigrationProvider{sizes: &diskSizes{primary: 10 * gib, err: refusal}}
			r, migration := exportingFixture(t, prov, qty("100Gi"))
			_, err := r.handleExportingPhase(context.Background(), migration)
			require.NoError(t, err)
			assertMigrationFailedNotOwned(t, getXNSMigration(t, r, migration))
			assert.Zero(t, prov.exports, "no ExportDisk")
		})
	}
}

// refusingExportProvider refuses every ExportDisk with exportErr.
type refusingExportProvider struct {
	sizedMigrationProvider
	exportErr error
}

func (p *refusingExportProvider) ExportDisk(_ context.Context, req contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.exports++
	p.lastExportDisk = req
	return contracts.ExportDiskResponse{}, p.exportErr
}

// TestVMMigration_SourceOwnerRefusal_ExportDisk: an ExportDisk the provider
// refuses (before reading any disk) fails the migration with the constant
// message, not the provider's answer.
func TestVMMigration_SourceOwnerRefusal_ExportDisk(t *testing.T) {
	for name, refusal := range ownerRefusals("exportDisk") {
		t.Run(name, func(t *testing.T) {
			sourceVM, sourceProvider, targetProvider, migration := directionFixture(
				infrav1beta1.ProviderTypeLibvirt, infrav1beta1.ProviderTypeVSphere)
			migration.Status.Phase = infrav1beta1.MigrationPhaseExporting
			migration.Status.DiskInfo = nil
			migration.Status.SnapshotID = "snap-1"
			prov := &refusingExportProvider{sizedMigrationProvider: sizedMigrationProvider{sizes: &diskSizes{primary: gib}}, exportErr: refusal}
			r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())

			_, err := r.handleExportingPhase(context.Background(), migration)
			require.NoError(t, err)
			assert.Equal(t, 1, prov.exports)
			assertMigrationFailedNotOwned(t, getXNSMigration(t, r, migration))
			assert.False(t, r.longOpAlreadyStarted(getXNSMigration(t, r, migration), longOpExport))
		})
	}
}

// refusingSnapshotProvider refuses every SnapshotCreate with err.
type refusingSnapshotProvider struct {
	stubProvider
	err   error
	calls int
}

func (p *refusingSnapshotProvider) SnapshotCreate(context.Context, contracts.SnapshotCreateRequest) (contracts.SnapshotCreateResponse, error) {
	p.calls++
	return contracts.SnapshotCreateResponse{}, p.err
}

// TestVMMigration_SourceOwnerRefusal_SnapshotCreate: the source snapshot
// refused as not the source VirtualMachine's fails the migration with the
// constant message.
func TestVMMigration_SourceOwnerRefusal_SnapshotCreate(t *testing.T) {
	for name, refusal := range ownerRefusals("snapshotCreate") {
		t.Run(name, func(t *testing.T) {
			vm, p, m := snapshotFixture()
			m.Status.SnapshotID = ""
			m.Status.Phase = infrav1beta1.MigrationPhaseSnapshotting
			prov := &refusingSnapshotProvider{err: refusal}
			r, c := newSnapshotReconciler(t, prov, vm, p, m)

			_, err := r.handleSnapshottingPhase(context.Background(), m)
			require.NoError(t, err)
			assert.Equal(t, 1, prov.calls)
			got := storedMigration(t, c, m)
			assert.Equal(t, infrav1beta1.MigrationPhaseFailed, got.Status.Phase)
			assert.Equal(t, migrationSourceNotOwnedMessage, got.Status.Message)
		})
	}
}

// refusingDescribeSource answers every Describe with err.
type refusingDescribeSource struct {
	powerSpyProvider
	err error
}

func (p *refusingDescribeSource) Describe(context.Context, contracts.VMRef) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{}, p.err
}

// TestValidatingPhase_SourcePowerOff_OwnerRefusalFailsBeforeThePatch: with
// powerOffBeforeMigration, a source the provider refuses fails the migration
// before the source VirtualMachine's spec.powerState is set to Off.
func TestValidatingPhase_SourcePowerOff_OwnerRefusalFailsBeforeThePatch(t *testing.T) {
	for name, refusal := range ownerRefusals("describe") {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			migration, objs := xnsMigration("")
			migration.Status.Phase = infrav1beta1.MigrationPhaseValidating
			migration.Spec.Source.PowerOffBeforeMigration = true
			var src *infrav1beta1.VirtualMachine
			for _, o := range objs {
				if vm, ok := o.(*infrav1beta1.VirtualMachine); ok {
					vm.Spec.PowerState = infrav1beta1.PowerStateOn
					src = vm
				}
			}
			require.NotNil(t, src)
			prov := &refusingDescribeSource{err: refusal}
			r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)

			_, err := r.handleValidatingPhase(ctx, migration)
			require.NoError(t, err)
			assertMigrationFailedNotOwned(t, getXNSMigration(t, r, migration))
			assert.Empty(t, prov.ops())
			got := &infrav1beta1.VirtualMachine{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(src), got))
			assert.Equal(t, infrav1beta1.PowerStateOn, got.Spec.PowerState, "the source VirtualMachine's spec is not changed")
		})
	}
}

// TestVMClone_SourceOwnerRefusal: a clone whose source the provider refuses —
// at the disk-size check's GetDiskInfo or at the Clone itself — is failed with
// the refusal's reason and a constant message; no clone is made from the
// disk-size refusal.
func TestVMClone_SourceOwnerRefusal(t *testing.T) {
	for name, refusal := range ownerRefusals("clone") {
		wantReason := ownerRefusalReason(refusal)
		t.Run("disk-size check/"+name, func(t *testing.T) {
			r, cp, rec, clone := cloneFixture(t, qty("100Gi"), &diskSizes{err: refusal})
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
			assert.Zero(t, cp.cloneCnt, "no Clone RPC")
			assertCloneFailedNotOwned(t, r, clone, rec, wantReason)
		})
		t.Run("Clone RPC/"+name, func(t *testing.T) {
			prov, src, clone := cloneDisclosureFixture()
			cp := &clonerProvider{cloneErr: refusal}
			r := newCloneReconciler(cloneTestScheme(t), &okResolver{provider: cp}, prov, src, clone)
			rec := record.NewFakeRecorder(32)
			r.Recorder = rec
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
			assert.Equal(t, 1, cp.cloneCnt, "refused once, never retried")
			assertCloneFailedNotOwned(t, r, clone, rec, wantReason)
		})
	}
}

// assertCloneFailedNotOwned asserts clone failed with reason and the constant
// message, recorded as a Warning event with no provider text.
func assertCloneFailedNotOwned(t *testing.T, r *VMCloneReconciler, clone *infrav1beta1.VMClone, rec *record.FakeRecorder, reason string) {
	t.Helper()
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, cloneSourceNotOwnedMessage, got.Status.Message)
	c := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionFailed)
	require.NotNil(t, c)
	assert.Equal(t, reason, c.Reason)
	assert.Equal(t, cloneSourceNotOwnedMessage, c.Message)
	events := drainEvents(rec)
	found := false
	for _, e := range events {
		assert.NotContains(t, e, "rpc error")
		if strings.Contains(e, cloneSourceNotOwnedMessage) {
			found = true
		}
	}
	assert.True(t, found, "a Warning event carries the constant message: %v", events)
}

// TestIsSourceOwnerRefused: only the disk-size checks' wrapped owner refusal
// is a *sourceOwnerRefusedError; the refusal kinds map to their reasons.
func TestIsSourceOwnerRefused(t *testing.T) {
	assert.True(t, isSourceOwnerRefused(&sourceOwnerRefusedError{err: vmNotOwnedAnswer("getDiskInfo")}))
	assert.False(t, isSourceOwnerRefused(vmNotOwnedAnswer("getDiskInfo")))
	assert.True(t, isOwnerRefusal(&sourceOwnerRefusedError{err: ownerStampUnreadableAnswer("getDiskInfo")}))
	assert.False(t, isOwnerRefusal(contracts.NewNotFoundError("gone", nil)))
	assert.Equal(t, k8s.ReasonVMNotOwned, ownerRefusalReason(vmNotOwnedAnswer("x")))
	assert.Equal(t, k8s.ReasonOwnerStampUnreadable, ownerRefusalReason(ownerStampUnreadableAnswer("x")))
}

// refusingSourceHypervisor answers every Describe of the source with
// VM_NOT_OWNED and counts Power calls.
type refusingSourceHypervisor struct {
	stubProvider
	powers int
}

func (p *refusingSourceHypervisor) Describe(context.Context, contracts.VMRef) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{}, vmNotOwnedAnswer("describe")
}

func (p *refusingSourceHypervisor) Power(context.Context, contracts.VMRef, contracts.PowerOp) (string, error) {
	p.powers++
	return "", nil
}

var _ = Describe("VMMigration of a source whose VMID was reused (envtest)", func() {
	It("fails the migration with a constant message before the source's spec is changed", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mig-not-owned-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		for _, name := range []string{"prov-1", "prov-2"} {
			p := holdTestProvider(ns.Name)
			p.Name = name
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.Runtime = &infrav1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
			p.Status.Conditions = []metav1.Condition{{Type: "ProviderAvailable", Status: metav1.ConditionTrue,
				Reason: "RemoteAvailable", Message: "ready", LastTransitionTime: metav1.Now()}}
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())
		vm := holdTestVM(ns.Name, "src-vm", infrav1beta1.ObservedPowerStateOn)
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, vm))).To(Succeed())
		})
		specBefore := vm.Spec.PowerState

		m := &infrav1beta1.VMMigration{
			ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: ns.Name,
				Finalizers: []string{infrav1beta1.VMMigrationFinalizer}},
			Spec: infrav1beta1.VMMigrationSpec{
				Source: infrav1beta1.MigrationSource{
					VMRef:                   infrav1beta1.LocalObjectReference{Name: "src-vm"},
					PowerOffBeforeMigration: true,
				},
				Target: infrav1beta1.MigrationTarget{Name: "web",
					ProviderRef: infrav1beta1.ObjectRef{Name: "prov-2"}},
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() {
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				got := &infrav1beta1.VMMigration{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(m), got); err != nil {
					return client.IgnoreNotFound(err)
				}
				got.Finalizers = nil
				return k8sClient.Update(ctx, got)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, m))).To(Succeed())
		})
		hv := &refusingSourceHypervisor{}
		rec := record.NewFakeRecorder(100)
		mr := NewVMMigrationReconciler(k8sClient, k8sClient, k8sClient.Scheme(), nil, rec, false)
		mr.providerInstanceFn = func(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) { return hv, nil }
		mReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
		get := func() *infrav1beta1.VMMigration {
			got := &infrav1beta1.VMMigration{}
			Expect(k8sClient.Get(ctx, mReq.NamespacedName, got)).To(Succeed())
			return got
		}

		for i := 0; i < 4 && get().Status.Phase != infrav1beta1.MigrationPhaseFailed; i++ {
			_, err := mr.Reconcile(ctx, mReq)
			Expect(err).NotTo(HaveOccurred())
		}
		got := get()
		Expect(got.Status.Phase).To(Equal(infrav1beta1.MigrationPhaseFailed))
		Expect(got.Status.Message).To(Equal(migrationSourceNotOwnedMessage))
		failed := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMMigrationConditionFailed)
		Expect(failed).NotTo(BeNil())
		Expect(failed.Message).To(Equal(migrationSourceNotOwnedMessage))
		Expect(failed.ObservedGeneration).To(Equal(got.Generation))
		Expect(hv.powers).To(BeZero())

		src := &infrav1beta1.VirtualMachine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(vm), src)).To(Succeed())
		Expect(src.Spec.PowerState).To(Equal(specBefore), "the source VirtualMachine's spec.powerState is not set to Off")
	})
})
