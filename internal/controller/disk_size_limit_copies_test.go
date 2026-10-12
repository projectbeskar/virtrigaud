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
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A clone and a migration copy the source VM's REAL disks: they are checked
// by the sizes its provider reports (GetDiskInfo), never by its spec, and an
// unknown size refuses the copy while a limit applies (review H1, H2).

const gib = int64(1) << 30

// diskSizes answers GetDiskInfo with a primary disk of primary bytes and the
// disks listed, or err; it counts the calls.
type diskSizes struct {
	mu      sync.Mutex
	primary int64
	disks   []contracts.DiskCapacity
	err     error
	calls   int
	reqs    []contracts.GetDiskInfoRequest
}

func (d *diskSizes) answer(req contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.reqs = append(d.reqs, req)
	return contracts.GetDiskInfoResponse{VirtualSizeBytes: d.primary, Disks: d.disks}, d.err
}

// sizedCloner is a clonerProvider whose GetDiskInfo reports sizes.
type sizedCloner struct {
	clonerProvider
	sizes *diskSizes
}

func (p *sizedCloner) GetDiskInfo(_ context.Context, req contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	return p.sizes.answer(req)
}

func capacities(sizes ...int64) []contracts.DiskCapacity {
	out := make([]contracts.DiskCapacity, 0, len(sizes))
	for i, s := range sizes {
		out = append(out, contracts.DiskCapacity{ID: string(rune('a' + i)), Path: "/host/secret/path", VirtualSizeBytes: s})
	}
	return out
}

func diskCapClone(ns string) *infravirtrigaudiov1beta1.VMClone {
	return &infravirtrigaudiov1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: ns},
		Spec: infravirtrigaudiov1beta1.VMCloneSpec{
			Source: infravirtrigaudiov1beta1.CloneSource{VMRef: &infravirtrigaudiov1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infravirtrigaudiov1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
}

func sizedClass(ns, name, size string) *infravirtrigaudiov1beta1.VMClass {
	c := &infravirtrigaudiov1beta1.VMClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       infravirtrigaudiov1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}
	if size != "" {
		c.Spec.DiskDefaults = &infravirtrigaudiov1beta1.DiskDefaults{Size: resource.MustParse(size)}
	}
	return c
}

// cloneFixture is a single-host clone of src-vm (spec: a small class, no
// spec.disks — what a lowered class or a removed spec.disks entry leaves)
// whose provider reports sizes.
func cloneFixture(t *testing.T, limit *resource.Quantity, sizes *diskSizes, objs ...client.Object) (*VMCloneReconciler, *sizedCloner, *record.FakeRecorder, *infravirtrigaudiov1beta1.VMClone) {
	t.Helper()
	const ns = "default"
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := diskCapClone(ns)
	objs = append(objs, runningProvider(ns, "prov-1"), src, clone, sizedClass(ns, "src-class", "10Gi"))
	cp := &sizedCloner{clonerProvider: clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-999"}}, sizes: sizes}
	r := newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, objs...)
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	r.MaxDiskSize = limit
	return r, cp, rec, clone
}

func TestVMClone_DiskSizeLimit_RealSizes(t *testing.T) {
	const ns = "default"
	for _, tc := range []struct {
		name        string
		sizes       *diskSizes
		targetClass string // "" = no target class override
		reason      string // "" = the clone is issued
		inMessage   string
	}{
		{name: "the source's primary disk is above the limit although its class is small",
			sizes:  &diskSizes{disks: capacities(500 * gib)},
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "disk 0 of source VM src-vm asks for 500Gi, above the disk-size limit 100Gi: nothing is cloned"},
		{name: "a disk no longer in spec.disks is above the limit",
			sizes:  &diskSizes{disks: capacities(10*gib, 800*gib)},
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "disk 1 of source VM src-vm asks for 800Gi"},
		{name: "the target class override is above the limit", targetClass: "300Gi",
			sizes:  &diskSizes{disks: capacities(10 * gib)},
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "VMClass big-class spec.diskDefaults.size asks for 300Gi"},
		{name: "a provider that does not report disks fails closed",
			sizes:  &diskSizes{primary: 10 * gib},
			reason: k8s.ReasonDiskSizeLimitUnverifiable, inMessage: cloneSourceDisksUnreadableMessage},
		{name: "an unknown disk size fails closed",
			sizes:  &diskSizes{disks: capacities(10*gib, 0)},
			reason: k8s.ReasonDiskSizeLimitUnverifiable, inMessage: cloneSourceDisksUnreadableMessage},
		{name: "a failed read fails closed",
			sizes:  &diskSizes{err: errors.New("provider unreachable at 10.0.0.9:9443")},
			reason: k8s.ReasonDiskSizeLimitUnverifiable, inMessage: cloneSourceDisksUnreadableMessage},
		{name: "within the limit", targetClass: "100Gi", sizes: &diskSizes{disks: capacities(100*gib, 50*gib)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var extra []client.Object
			if tc.targetClass != "" {
				extra = append(extra, sizedClass(ns, "big-class", tc.targetClass))
			}
			r, cp, rec, clone := cloneFixture(t, qty("100Gi"), tc.sizes, extra...)
			if tc.targetClass != "" {
				stored := &infravirtrigaudiov1beta1.VMClone{}
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(clone), stored))
				stored.Spec.Target.ClassRef = &infravirtrigaudiov1beta1.LocalObjectReference{Name: "big-class"}
				require.NoError(t, r.Update(ctx, stored))
			}

			key := client.ObjectKeyFromObject(clone)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}) // adds the finalizer
			require.NoError(t, err)
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			require.NoError(t, err)

			got := &infravirtrigaudiov1beta1.VMClone{}
			require.NoError(t, r.Get(ctx, key, got))
			if tc.reason == "" {
				assert.Equal(t, 1, cp.cloneCnt, "a clone within the limit is issued")
				return
			}
			assert.Equal(t, diskSizeLimitRetryInterval, res.RequeueAfter)
			assert.Zero(t, cp.cloneCnt, "no Clone RPC")
			assert.Error(t, r.Get(ctx, client.ObjectKey{Namespace: ns, Name: "clone-target"}, &infravirtrigaudiov1beta1.VirtualMachine{}), "no target VM")
			assert.Equal(t, infravirtrigaudiov1beta1.ClonePhasePending, got.Status.Phase)
			ready := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMCloneConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			assert.Equal(t, tc.reason, ready.Reason)
			assert.Contains(t, ready.Message, tc.inMessage)
			assert.NotContains(t, ready.Message, "/host/secret/path", "no provider path in status")
			assert.NotContains(t, ready.Message, "10.0.0.9", "no provider error in status")
			assert.Len(t, diskLimitEvents(rec, tc.reason), 1)

			// A repeated hold writes nothing and records no event.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			again := &infravirtrigaudiov1beta1.VMClone{}
			require.NoError(t, r.Get(ctx, key, again))
			assert.Equal(t, got.ResourceVersion, again.ResourceVersion)
			assert.Empty(t, diskLimitEvents(rec, tc.reason))
			assert.Zero(t, cp.cloneCnt)
		})
	}
}

// TestVMClone_DiskSizeLimit_NoLimitNoRead: without a limit nothing changes —
// not even a GetDiskInfo is sent.
func TestVMClone_DiskSizeLimit_NoLimitNoRead(t *testing.T) {
	sizes := &diskSizes{disks: capacities(64 << 40)}
	r, cp, _, clone := cloneFixture(t, nil, sizes)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	assert.Equal(t, 1, cp.cloneCnt)
	assert.Zero(t, sizes.calls)
}

// ─── VMMigration ──────────────────────────────────────────────────────────────

// sizedMigrationProvider counts ExportDisk / ImportDisk and reports disk
// sizes.
type sizedMigrationProvider struct {
	stubProvider
	sizes          *diskSizes
	exports        int
	imports        int
	lastExportDisk contracts.ExportDiskRequest
	importErr      error
	lastImport     contracts.ImportDiskRequest
	// oldImage plays a provider image that predates the import-time limit
	// (it does not report SupportsImportSizeLimit).
	oldImage bool
	capsErr  error
}

func (p *sizedMigrationProvider) GetCapabilities(_ context.Context) (contracts.Capabilities, error) {
	return contracts.Capabilities{SupportsDiskImport: true, SupportsImportSizeLimit: !p.oldImage}, p.capsErr
}

func (p *sizedMigrationProvider) GetDiskInfo(_ context.Context, req contracts.GetDiskInfoRequest) (contracts.GetDiskInfoResponse, error) {
	return p.sizes.answer(req)
}

func (p *sizedMigrationProvider) ExportDisk(_ context.Context, req contracts.ExportDiskRequest) (contracts.ExportDiskResponse, error) {
	p.exports++
	p.lastExportDisk = req
	return contracts.ExportDiskResponse{ExportId: "exp-1"}, nil
}

func (p *sizedMigrationProvider) ImportDisk(_ context.Context, req contracts.ImportDiskRequest) (contracts.ImportDiskResponse, error) {
	p.imports++
	p.lastImport = req
	if p.importErr != nil {
		return contracts.ImportDiskResponse{}, p.importErr
	}
	return contracts.ImportDiskResponse{DiskId: "target-vm-migrated", Path: "/pool/target-vm-migrated.qcow2"}, nil
}

func (p *sizedMigrationProvider) Describe(_ context.Context, _ contracts.VMRef) (contracts.DescribeResponse, error) {
	return contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}, nil
}

func TestVMMigration_DiskSizeLimit_HeldInValidating(t *testing.T) {
	for _, tc := range []struct {
		name        string
		srcClass    string // the source VM's class size ("" = no class): never what is checked
		primary     int64  // the source disk's real size
		readErr     error
		targetClass string // the target class "cls" primary-disk size ("" = none)
		ann         *string
		reason      string // "" = proceeds
		inMessage   string
	}{
		{name: "the real disk is above the limit although the source class is small", srcClass: "10Gi", primary: 500 * gib,
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "the disk of source VM src-vm asks for 500Gi, above the disk-size limit 100Gi: nothing is migrated"},
		{name: "an adopted source (no class) with a large disk", primary: 200 * gib,
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "asks for 200Gi"},
		{name: "the target class above the limit", primary: 10 * gib, targetClass: "200Gi",
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "VMClass cls spec.diskDefaults.size asks for 200Gi"},
		{name: "the target Provider's annotation lowers the limit", primary: 80 * gib, ann: ann("50Gi"),
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "above the disk-size limit 50Gi"},
		{name: "an invalid target Provider annotation fails closed", primary: 1 * gib, ann: ann("0"),
			reason: k8s.ReasonDiskSizeLimitUnverifiable, inMessage: diskSizeLimitUnverifiableMessage},
		{name: "an unknown disk size fails closed", primary: 0,
			reason: k8s.ReasonDiskSizeLimitUnverifiable, inMessage: migrationSourceDiskUnreadableMessage},
		{name: "a failed read fails closed", primary: 10 * gib, readErr: errors.New("boom"),
			reason: k8s.ReasonDiskSizeLimitUnverifiable, inMessage: migrationSourceDiskUnreadableMessage},
		{name: "within the limit", srcClass: "500Gi", primary: 100 * gib, targetClass: "100Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			migration, objs := xnsMigration("")
			migration.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
			for _, o := range objs {
				switch obj := o.(type) {
				case *infravirtrigaudiov1beta1.VirtualMachine:
					if tc.srcClass != "" {
						obj.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{Name: "src-class"}
					}
				case *infravirtrigaudiov1beta1.VMClass:
					if tc.targetClass != "" {
						obj.Spec.DiskDefaults = &infravirtrigaudiov1beta1.DiskDefaults{Size: resource.MustParse(tc.targetClass)}
					}
				case *infravirtrigaudiov1beta1.Provider:
					if obj.Name == "tgt-prov" && tc.ann != nil {
						obj.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderMaxDiskSizeAnnotation: *tc.ann}
					}
				}
			}
			if tc.srcClass != "" {
				objs = append(objs, sizedClass(xnsSource, "src-class", tc.srcClass))
			}
			prov := &sizedMigrationProvider{sizes: &diskSizes{primary: tc.primary, err: tc.readErr}}
			r, rec := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)
			r.MaxDiskSize = qty("100Gi")

			res, err := r.handleValidatingPhase(ctx, migration)
			require.NoError(t, err)
			got := getXNSMigration(t, r, migration)
			if tc.reason == k8s.ReasonDiskSizeLimitUnverifiable && tc.ann != nil {
				assert.Zero(t, prov.sizes.calls, "an invalid limit refuses before any read")
			} else {
				require.Equal(t, 1, prov.sizes.calls, "the source's real disk size is read")
				assert.Equal(t, "vm-1", prov.sizes.reqs[0].VM.ID)
			}
			if tc.reason == "" {
				assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseExporting, got.Status.Phase, "a migration within the limit proceeds")
				return
			}
			assert.Equal(t, diskSizeLimitRetryInterval, res.RequeueAfter)
			assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseValidating, got.Status.Phase, "held, not failed")
			c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, tc.reason, c.Reason)
			assert.Contains(t, c.Message, tc.inMessage)
			assert.NotContains(t, c.Message, "boom")
			assert.Equal(t, migration.Generation, c.ObservedGeneration)
			assert.Zero(t, prov.exports+prov.imports, "no export or import")
			assert.Len(t, diskLimitEvents(rec, tc.reason), 1)

			// A repeated hold writes nothing and records no event.
			_, err = r.handleValidatingPhase(ctx, got.DeepCopy())
			require.NoError(t, err)
			again := getXNSMigration(t, r, migration)
			assert.Equal(t, got.ResourceVersion, again.ResourceVersion)
			assert.Empty(t, diskLimitEvents(rec, tc.reason))
		})
	}
}

// TestVMMigration_DiskSizeLimit_NoLimitNoRead: without a limit nothing
// changes — not even a GetDiskInfo is sent.
func TestVMMigration_DiskSizeLimit_NoLimitNoRead(t *testing.T) {
	migration, objs := xnsMigration("")
	migration.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
	prov := &sizedMigrationProvider{sizes: &diskSizes{primary: 64 << 40}}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)
	_, err := r.handleValidatingPhase(context.Background(), migration)
	require.NoError(t, err)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseExporting, getXNSMigration(t, r, migration).Status.Phase)
	assert.Zero(t, prov.sizes.calls)
}

// exportingFixture is the direction fixture in the Exporting phase, with a
// snapshot to export.
func exportingFixture(t *testing.T, prov *sizedMigrationProvider, limit *resource.Quantity) (*VMMigrationReconciler, *infravirtrigaudiov1beta1.VMMigration) {
	t.Helper()
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infravirtrigaudiov1beta1.ProviderTypeLibvirt, infravirtrigaudiov1beta1.ProviderTypeVSphere)
	migration.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseExporting
	migration.Status.DiskInfo = nil
	migration.Status.SnapshotID = "snap-1"
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())
	r.MaxDiskSize = limit
	return r, migration
}

// TestVMMigration_DiskSizeLimit_CheckedRightBeforeExport: the disk is read
// again right before ExportDisk — the same VM, disk and snapshot — so a disk
// grown after Validating is held there; a disk that fits is exported and its
// size recorded for the check before ImportDisk.
func TestVMMigration_DiskSizeLimit_CheckedRightBeforeExport(t *testing.T) {
	ctx := context.Background()

	grown := &sizedMigrationProvider{sizes: &diskSizes{primary: 300 * gib}}
	r, migration := exportingFixture(t, grown, qty("100Gi"))
	res, err := r.handleExportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, diskSizeLimitRetryInterval, res.RequeueAfter)
	assert.Zero(t, grown.exports, "no ExportDisk")
	require.Len(t, grown.sizes.reqs, 1)
	assert.Equal(t, "snap-1", grown.sizes.reqs[0].SnapshotId, "the disk read is the one exported")
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseExporting, got.Status.Phase)
	c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionExporting)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonDiskSizeExceedsLimit, c.Reason)
	assert.False(t, r.longOpAlreadyStarted(got, longOpExport), "a hold does not claim the export guard")

	fits := &sizedMigrationProvider{sizes: &diskSizes{primary: 100 * gib}}
	r, migration = exportingFixture(t, fits, qty("100Gi"))
	_, err = r.handleExportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, 1, fits.exports)
	got = getXNSMigration(t, r, migration)
	require.NotNil(t, got.Status.DiskInfo)
	require.NotNil(t, got.Status.DiskInfo.SourceSize, "the exported disk's size is recorded")
	assert.Equal(t, "100Gi", got.Status.DiskInfo.SourceSize.String())

	none := &sizedMigrationProvider{sizes: &diskSizes{primary: 100 * gib}}
	r, migration = exportingFixture(t, none, nil)
	_, err = r.handleExportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, 1, none.exports)
	assert.Zero(t, none.sizes.calls, "no read without a limit")
}

// TestVMMigration_DiskSizeLimit_CheckedRightBeforeImport: the recorded size is
// checked against the target's limit as it is now, and a limit with no
// recorded size refuses the import (fail closed).
func TestVMMigration_DiskSizeLimit_CheckedRightBeforeImport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string // "" = not recorded
		limit    *resource.Quantity
		reason   string // "" = imported
	}{
		{name: "recorded above the limit", recorded: "200Gi", limit: qty("100Gi"), reason: k8s.ReasonDiskSizeExceedsLimit},
		{name: "not recorded while a limit applies", limit: qty("100Gi"), reason: k8s.ReasonDiskSizeLimitUnverifiable},
		{name: "recorded within the limit", recorded: "100Gi", limit: qty("100Gi")},
		{name: "no limit, nothing recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sourceVM, sourceProvider, targetProvider, migration := directionFixture(
				infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt)
			if tc.recorded != "" {
				migration.Status.DiskInfo.SourceSize = qty(tc.recorded)
			}
			prov := &sizedMigrationProvider{sizes: &diskSizes{}}
			r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())
			r.MaxDiskSize = tc.limit

			_, err := r.handleImportingPhase(ctx, migration)
			require.NoError(t, err)
			got := getXNSMigration(t, r, migration)
			if tc.reason == "" {
				assert.Equal(t, 1, prov.imports)
				return
			}
			assert.Zero(t, prov.imports, "no ImportDisk")
			c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionImporting)
			require.NotNil(t, c)
			assert.Equal(t, tc.reason, c.Reason)
			assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseImporting, got.Status.Phase)
		})
	}
}

// providerRefusal is what the transport client returns for a provider's
// DISK_SIZE_EXCEEDS_LIMIT (or DISK_SIZE_UNVERIFIABLE) ImportDisk refusal.
func providerRefusal(marker error) error {
	return contracts.NewConflictError("importDisk: refused by the provider", fmt.Errorf("%w: %w", marker, errors.New("grpc status")))
}

// TestVMMigration_DiskSizeLimit_TargetProviderEnforcesAtImport (review
// H1-residual): the import carries the target's limit (max_virtual_size_bytes),
// so the provider checks the disk it is about to land — the staged object may
// have been replaced since the export. Its refusal holds the migration in
// Importing (never fails it); a refusal for the same limit is not re-sent; a
// changed limit sends the import again.
func TestVMMigration_DiskSizeLimit_TargetProviderEnforcesAtImport(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt)
	migration.Status.DiskInfo.SourceSize = qty("50Gi") // what was exported: within the limit
	prov := &sizedMigrationProvider{sizes: &diskSizes{}, importErr: providerRefusal(contracts.ErrDiskSizeExceedsLimit)}
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	r.MaxDiskSize = qty("100Gi")

	res, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, diskSizeLimitRetryInterval, res.RequeueAfter)
	require.Equal(t, 1, prov.imports)
	assert.Equal(t, int64(100)<<30, prov.lastImport.MaxVirtualSizeBytes, "the import carries the target's limit")

	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseImporting, got.Status.Phase, "held, not failed")
	c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionImporting)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonDiskSizeExceedsLimit, c.Reason)
	assert.Equal(t, providerImportRefusedMessage(int64(100)<<30), c.Message)
	assert.NotContains(t, c.Message, "grpc status", "the provider's text is not shown")
	assert.False(t, r.longOpAlreadyStarted(got, longOpImport), "the import guard is released")
	assert.Len(t, diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit), 1)

	// The same limit: the import (a full download or convert) is not re-sent.
	_, err = r.handleImportingPhase(ctx, got.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, 1, prov.imports, "not re-sent for the same limit")

	// A raised limit: the import is sent again, with the new limit.
	r.MaxDiskSize = qty("1Ti")
	prov.importErr = nil
	_, err = r.handleImportingPhase(ctx, getXNSMigration(t, r, migration))
	require.NoError(t, err)
	assert.Equal(t, 2, prov.imports)
	assert.Equal(t, int64(1)<<40, prov.lastImport.MaxVirtualSizeBytes)
}

// TestVMMigration_DiskSizeLimit_TargetCannotReadSize: a provider that could
// not read the staged disk's size holds the migration (fail closed) and the
// import is retried on the hold's re-check.
func TestVMMigration_DiskSizeLimit_TargetCannotReadSize(t *testing.T) {
	ctx := context.Background()
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt)
	migration.Status.DiskInfo.SourceSize = qty("50Gi")
	prov := &sizedMigrationProvider{sizes: &diskSizes{}, importErr: providerRefusal(contracts.ErrDiskSizeUnverifiable)}
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())
	r.MaxDiskSize = qty("100Gi")

	_, err := r.handleImportingPhase(ctx, migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionImporting)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonDiskSizeLimitUnverifiable, c.Reason)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseImporting, got.Status.Phase)

	_, err = r.handleImportingPhase(ctx, got.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, 2, prov.imports, "an unreadable size is retried")
}

// TestVMMigration_NoLimitSendsNoMaxSize: without a limit the import carries
// none (0), so nothing changes on the provider side.
func TestVMMigration_NoLimitSendsNoMaxSize(t *testing.T) {
	sourceVM, sourceProvider, targetProvider, migration := directionFixture(
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt)
	prov := &sizedMigrationProvider{sizes: &diskSizes{}}
	r, _ := directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, migration, s3CredsSecret())
	_, err := r.handleImportingPhase(context.Background(), migration)
	require.NoError(t, err)
	require.Equal(t, 1, prov.imports)
	assert.Zero(t, prov.lastImport.MaxVirtualSizeBytes)
}

// TestVMMigration_DiskSizeLimit_OldTargetImageHeld (review M1): while a limit
// applies, a target provider that does not report supports_import_size_limit
// (an older image would silently ignore max_virtual_size_bytes) holds the
// migration — in Validating before any export, and again before the import —
// with a constant "upgrade the provider image" message. Without a limit the
// migration is unaffected and no capability is even needed.
func TestVMMigration_DiskSizeLimit_OldTargetImageHeld(t *testing.T) {
	ctx := context.Background()

	// Validating: held before any export.
	migration, objs := xnsMigration("")
	migration.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
	prov := &sizedMigrationProvider{sizes: &diskSizes{primary: 10 * gib}, oldImage: true}
	r, _ := newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)
	r.MaxDiskSize = qty("100Gi")
	_, err := r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)
	got := getXNSMigration(t, r, migration)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseValidating, got.Status.Phase)
	c := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionValidating)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonDiskSizeLimitUnverifiable, c.Reason)
	assert.Equal(t, importSizeLimitUnsupportedMessage, c.Message)
	assert.Zero(t, prov.exports+prov.imports)

	// No limit: the same old image migrates as before.
	migration, objs = xnsMigration("")
	migration.Status.Phase = infravirtrigaudiov1beta1.MigrationPhaseValidating
	prov = &sizedMigrationProvider{sizes: &diskSizes{primary: 10 * gib}, oldImage: true}
	r, _ = newXNSMigrationReconciler(t, mountTestScheme(t), prov, append(objs, migration)...)
	_, err = r.handleValidatingPhase(ctx, migration)
	require.NoError(t, err)
	assert.Equal(t, infravirtrigaudiov1beta1.MigrationPhaseExporting, getXNSMigration(t, r, migration).Status.Phase)

	// Importing: held before ImportDisk (the target was upgraded away after
	// Validating, or the migration predates the check).
	sourceVM, sourceProvider, targetProvider, imp := directionFixture(
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt)
	imp.Status.DiskInfo.SourceSize = qty("50Gi")
	prov = &sizedMigrationProvider{sizes: &diskSizes{}, oldImage: true}
	r, _ = directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, imp, s3CredsSecret())
	r.MaxDiskSize = qty("100Gi")
	_, err = r.handleImportingPhase(ctx, imp)
	require.NoError(t, err)
	assert.Zero(t, prov.imports, "no ImportDisk into a provider that would ignore the limit")
	c = meta.FindStatusCondition(getXNSMigration(t, r, imp).Status.Conditions, infravirtrigaudiov1beta1.VMMigrationConditionImporting)
	require.NotNil(t, c)
	assert.Equal(t, importSizeLimitUnsupportedMessage, c.Message)

	// A failed capability query is retried, not held.
	sourceVM, sourceProvider, targetProvider, imp = directionFixture(
		infravirtrigaudiov1beta1.ProviderTypeVSphere, infravirtrigaudiov1beta1.ProviderTypeLibvirt)
	imp.Status.DiskInfo.SourceSize = qty("50Gi")
	prov = &sizedMigrationProvider{sizes: &diskSizes{}, capsErr: errors.New("unreachable")}
	r, _ = directionReconciler(t, prov, sourceVM, sourceProvider, targetProvider, imp, s3CredsSecret())
	r.MaxDiskSize = qty("100Gi")
	res, err := r.handleImportingPhase(ctx, imp)
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter)
	assert.Zero(t, prov.imports)
}
