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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The administrator's disk-size cap (backlog #7): no disk above the limit is
// created, none is grown beyond it, and a VM created before the limit with
// larger disks keeps running, unshrunk.

func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// diskLimitEvents returns the Warning events of reason in rec.
func diskLimitEvents(rec *record.FakeRecorder, reason string) []string {
	var out []string
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+reason) {
			out = append(out, e)
		}
	}
	return out
}

// diskCapFixture is a single-host create fixture: Provider test-prov and
// VMClass test-class in default, and the creatable VM "web" with disks.
func diskCapFixture(t *testing.T, limit *resource.Quantity, providerAnn *string, classSize string,
	disks ...infravirtrigaudiov1beta1.DiskSpec) (*VirtualMachineReconciler, *routingProvider, *record.FakeRecorder) {
	t.Helper()
	prov, class := providerAndClass("default")
	if providerAnn != nil {
		prov.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderMaxDiskSizeAnnotation: *providerAnn}
	}
	if classSize != "" {
		class.Spec.DiskDefaults = &infravirtrigaudiov1beta1.DiskDefaults{Size: resource.MustParse(classSize)}
	}
	vm := creatableVM("default")
	vm.Spec.Disks = disks
	rp := &routingProvider{}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: rp}, prov, class, vm)
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	r.MaxDiskSize = limit
	return r, rp, rec
}

func ann(s string) *string { return &s }

func TestReconcileVM_DiskSizeLimit_CreateRefused(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit       *resource.Quantity
		providerAnn *string
		classSize   string
		disks       []infravirtrigaudiov1beta1.DiskSpec
		reason      string
		inMessage   string
	}{
		{name: "an additional disk above the ceiling", limit: qty("100Gi"),
			disks:  []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 50}, {Name: "logs", SizeGiB: 101}},
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "spec.disks[logs].sizeGiB asks for 101Gi, above the disk-size limit 100Gi: nothing is created"},
		{name: "the class's primary disk above the ceiling", limit: qty("100Gi"), classSize: "200Gi",
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "VMClass test-class spec.diskDefaults.size asks for 200Gi"},
		{name: "a decimal class size above a binary ceiling", limit: qty("100Gi"), classSize: "108G",
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "VMClass test-class spec.diskDefaults.size asks for 108G"},
		{name: "the Provider's annotation alone", providerAnn: ann("50Gi"),
			disks:  []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 60}},
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "above the disk-size limit 50Gi"},
		{name: "the Provider's annotation lowers the ceiling", limit: qty("1Ti"), providerAnn: ann("50Gi"),
			disks:  []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 60}},
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "above the disk-size limit 50Gi"},
		{name: "the Provider's annotation cannot raise the ceiling", limit: qty("100Gi"), providerAnn: ann("1Ti"),
			disks:  []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 200}},
			reason: k8s.ReasonDiskSizeExceedsLimit, inMessage: "above the disk-size limit 100Gi"},
		{name: "an invalid Provider annotation fails closed", providerAnn: ann("lots"),
			disks:  []infravirtrigaudiov1beta1.DiskSpec{{Name: "data", SizeGiB: 1}},
			reason: k8s.ReasonDiskSizeLimitUnverifiable, inMessage: diskSizeLimitUnverifiableMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, rp, rec := diskCapFixture(t, tc.limit, tc.providerAnn, tc.classSize, tc.disks...)

			stored, _ := storedVM(t, r)
			res, err := r.reconcileVM(ctx, stored)
			require.NoError(t, err)
			assert.Equal(t, diskSizeLimitRetryInterval, res.RequeueAfter, "re-checked on the slow cadence, never hot")
			assert.Empty(t, rp.createReqs, "no Create is sent")

			held, _ := storedVM(t, r)
			assert.Empty(t, held.Status.ID)
			for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
				c := meta.FindStatusCondition(held.Status.Conditions, condType)
				require.NotNil(t, c, condType)
				assert.Equal(t, metav1.ConditionFalse, c.Status, condType)
				assert.Equal(t, tc.reason, c.Reason, condType)
				assert.Contains(t, c.Message, tc.inMessage, condType)
				assert.Equal(t, held.Generation, c.ObservedGeneration, condType)
			}
			assert.Len(t, diskLimitEvents(rec, tc.reason), 1, "one Warning event")

			// A repeated hold writes nothing and records no event.
			res, err = r.reconcileVM(ctx, held.DeepCopy())
			require.NoError(t, err)
			assert.Equal(t, diskSizeLimitRetryInterval, res.RequeueAfter)
			again, _ := storedVM(t, r)
			assert.Equal(t, held.ResourceVersion, again.ResourceVersion, "a repeated hold does not write the VM")
			assert.True(t, equality.Semantic.DeepEqual(held.Status, again.Status))
			assert.Empty(t, diskLimitEvents(rec, tc.reason))
			assert.Empty(t, rp.createReqs)
		})
	}
}

func TestReconcileVM_DiskSizeLimit_CreateWithinLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit *resource.Quantity
		ann   *string
	}{
		{name: "at the ceiling", limit: qty("100Gi")},
		{name: "no ceiling"},
		{name: "at the Provider's limit", ann: ann("100Gi")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, rp, _ := diskCapFixture(t, tc.limit, tc.ann, "100Gi", infravirtrigaudiov1beta1.DiskSpec{Name: "data", SizeGiB: 100})
			stored, _ := storedVM(t, r)
			_, err := r.reconcileVM(context.Background(), stored)
			require.NoError(t, err)
			require.Len(t, rp.createReqs, 1, "a disk AT the limit is created")
			assert.Equal(t, int32(100), rp.createReqs[0].Disks[0].SizeGiB)
			assert.Equal(t, int32(100), rp.createReqs[0].Class.DiskDefaults.SizeGiB)
		})
	}
}

// TestReconcileVM_DiskSizeLimit_NoRecreateAboveLimit: a vanished VM is
// re-created with new disks, so the limit applies; its id is kept.
func TestReconcileVM_DiskSizeLimit_NoRecreateAboveLimit(t *testing.T) {
	ctx := context.Background()
	r, rp, rec := diskCapFixture(t, qty("100Gi"), nil, "", infravirtrigaudiov1beta1.DiskSpec{Name: "data", SizeGiB: 500})
	rp.describeResp = contracts.DescribeResponse{Exists: false}
	stored, _ := storedVM(t, r)
	stored.Status.ID = "vm-100"
	stored.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: "test-prov"}
	require.NoError(t, r.Status().Update(ctx, stored))

	res, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)
	assert.Equal(t, diskSizeLimitRetryInterval, res.RequeueAfter)
	assert.Empty(t, rp.createReqs, "a vanished VM is not re-created with a disk above the limit")
	held, _ := storedVM(t, r)
	assert.Equal(t, "vm-100", held.Status.ID, "its id is kept")
	assert.Equal(t, k8s.ReasonDiskSizeExceedsLimit, meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady).Reason)
	assert.Len(t, diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit), 1)
}

// reconfigureRecorder records every Reconfigure request (and answers it
// synchronously and honestly).
type reconfigureRecorder struct {
	fakeDescribeProvider
	reqs   []contracts.CreateRequest
	result contracts.ReconfigureResult
	err    error
}

func (p *reconfigureRecorder) Reconfigure(_ context.Context, _ contracts.VMRef, req contracts.CreateRequest) (contracts.ReconfigureResult, error) {
	p.reqs = append(p.reqs, req)
	res := p.result
	res.Honest = true
	return res, p.err
}

// existingVMFixture is a created, running VM whose class asks for classSize
// and whose spec.disks are disks; currentCPU != the class's CPU makes it need
// a Reconfigure.
func existingVMFixture(t *testing.T, limit *resource.Quantity, classSize string, currentCPU int32,
	disks ...infravirtrigaudiov1beta1.DiskSpec) (*VirtualMachineReconciler, *reconfigureRecorder, *record.FakeRecorder) {
	t.Helper()
	prov, class := providerAndClass("default")
	if classSize != "" {
		class.Spec.DiskDefaults = &infravirtrigaudiov1beta1.DiskDefaults{Size: resource.MustParse(classSize)}
	}
	vm := creatableVM("default")
	vm.Spec.Disks = disks
	vm.Status.ID = "vm-100"
	vm.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: prov.Name}
	mem := class.Spec.Memory.Value() / (1 << 20)
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &currentCPU, MemoryMiB: &mem}
	p := &reconfigureRecorder{fakeDescribeProvider: fakeDescribeProvider{
		DescribeFn: func(context.Context, string) (contracts.DescribeResponse, error) {
			return contracts.DescribeResponse{Exists: true, PowerState: "On", IPs: []string{"10.0.0.5"}}, nil
		},
	}}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: p}, prov, class, vm)
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	r.MaxDiskSize = limit
	return r, p, rec
}

// TestReconcileVM_DiskSizeLimit_ExistingVMAboveLimitKeepsRunning: a VM created
// before the limit with larger disks is not broken: it is described, stays
// Ready, and nothing about the limit is reported while nothing grows.
func TestReconcileVM_DiskSizeLimit_ExistingVMAboveLimitKeepsRunning(t *testing.T) {
	r, p, rec := existingVMFixture(t, qty("100Gi"), "500Gi", 4, infravirtrigaudiov1beta1.DiskSpec{Name: "data", SizeGiB: 800})
	stored, _ := storedVM(t, r)
	_, err := r.reconcileVM(context.Background(), stored)
	require.NoError(t, err)
	got, _ := storedVM(t, r)
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, k8s.ConditionReady), "the VM keeps running and Ready")
	assert.Empty(t, p.reqs, "nothing is reconfigured")
	assert.Empty(t, diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit))
}

// TestReconcileVM_DiskSizeLimit_ReconfigureWithholdsGrowth: a Reconfigure of a
// VM whose spec asks for disks above the limit still applies its CPU change,
// but sends every over-limit disk size as 0 (leave the disk as it is), so no
// disk is grown beyond the limit and none is shrunk.
func TestReconcileVM_DiskSizeLimit_ReconfigureWithholdsGrowth(t *testing.T) {
	ctx := context.Background()
	r, p, rec := existingVMFixture(t, qty("100Gi"), "500Gi", 2,
		infravirtrigaudiov1beta1.DiskSpec{Name: "data", SizeGiB: 800}, infravirtrigaudiov1beta1.DiskSpec{Name: "small", SizeGiB: 50})
	stored, _ := storedVM(t, r)
	_, err := r.reconcileVM(ctx, stored)
	require.NoError(t, err)

	require.Len(t, p.reqs, 1, "the Reconfigure is sent")
	req := p.reqs[0]
	assert.Equal(t, int32(4), req.Class.CPU, "the CPU change goes ahead")
	assert.Zero(t, req.Class.DiskDefaults.SizeGiB, "the class's primary disk is not grown beyond the limit")
	assert.Zero(t, req.Disks[0].SizeGiB, "spec.disks[data] is not grown beyond the limit")
	assert.Equal(t, int32(50), req.Disks[1].SizeGiB, "a disk within the limit is sent as asked")

	got, _ := storedVM(t, r)
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, k8s.ConditionReady), "the VM stays Ready")
	assert.Equal(t, k8s.ReasonReconcileSuccess, meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReconfiguring).Reason,
		"the reconfigure itself succeeded")
	dc := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionDiskSizeWithinLimit)
	require.NotNil(t, dc)
	assert.Equal(t, metav1.ConditionFalse, dc.Status)
	assert.Equal(t, k8s.ReasonDiskSizeExceedsLimit, dc.Reason)
	assert.Equal(t, got.Generation, dc.ObservedGeneration)
	assert.Contains(t, dc.Message, "VMClass test-class spec.diskDefaults.size (500Gi), spec.disks[data].sizeGiB (800Gi): above the disk-size limit 100Gi, so not grown")
	require.NotNil(t, got.Status.CurrentResources)
	assert.Equal(t, int32(4), *got.Status.CurrentResources.CPU, "the applied CPU is recorded")
	assert.Len(t, diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit), 1, "one Warning event")

	// The same withheld growth on the next Reconfigure is not reported again.
	two := int32(2)
	got.Status.CurrentResources.CPU = &two
	_, err = r.reconcileVM(ctx, got)
	require.NoError(t, err)
	require.Len(t, p.reqs, 2)
	assert.Empty(t, diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit), "reported once")

	// A Reconfigure that withholds nothing (the limit was raised) clears it.
	r.MaxDiskSize = qty("1Ti")
	got, _ = storedVM(t, r)
	got.Status.CurrentResources.CPU = &two
	_, err = r.reconcileVM(ctx, got)
	require.NoError(t, err)
	require.Len(t, p.reqs, 3)
	assert.Equal(t, int32(800), p.reqs[2].Disks[0].SizeGiB)
	cleared, _ := storedVM(t, r)
	assert.Nil(t, meta.FindStatusCondition(cleared.Status.Conditions, k8s.ConditionDiskSizeWithinLimit))
}

// TestReconcileVM_DiskSizeLimit_WithheldGrowthRecordedOnEveryOutcome (review
// L4): an async task, a change pending a restart and a failed Reconfigure
// keep the DiskSizeWithinLimit=False record too.
func TestReconcileVM_DiskSizeLimit_WithheldGrowthRecordedOnEveryOutcome(t *testing.T) {
	for name, set := range map[string]func(*reconfigureRecorder){
		"async task":      func(p *reconfigureRecorder) { p.result.TaskRef = "task-1" },
		"restart pending": func(p *reconfigureRecorder) { p.result.RestartRequired = true },
		"failure":         func(p *reconfigureRecorder) { p.err = contracts.NewRetryableError("host busy", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			r, p, _ := existingVMFixture(t, qty("100Gi"), "", 2, infravirtrigaudiov1beta1.DiskSpec{Name: "data", SizeGiB: 800})
			set(p)
			stored, _ := storedVM(t, r)
			_, err := r.reconcileVM(context.Background(), stored)
			require.NoError(t, err)
			require.Len(t, p.reqs, 1)
			assert.Zero(t, p.reqs[0].Disks[0].SizeGiB)
			got, _ := storedVM(t, r)
			dc := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionDiskSizeWithinLimit)
			require.NotNil(t, dc, "the withheld growth is recorded")
			assert.Equal(t, metav1.ConditionFalse, dc.Status)
			assert.Equal(t, k8s.ReasonDiskSizeExceedsLimit, dc.Reason)
		})
	}
}

// TestReconcileVM_DiskSizeLimit_ReconfigureWithoutLimitUnchanged: with no
// limit a Reconfigure carries every disk size as before.
func TestReconcileVM_DiskSizeLimit_ReconfigureWithoutLimitUnchanged(t *testing.T) {
	r, p, rec := existingVMFixture(t, nil, "500Gi", 2, infravirtrigaudiov1beta1.DiskSpec{Name: "data", SizeGiB: 800})
	stored, _ := storedVM(t, r)
	_, err := r.reconcileVM(context.Background(), stored)
	require.NoError(t, err)
	require.Len(t, p.reqs, 1)
	assert.Equal(t, int32(500), p.reqs[0].Class.DiskDefaults.SizeGiB)
	assert.Equal(t, int32(800), p.reqs[0].Disks[0].SizeGiB)
	got, _ := storedVM(t, r)
	assert.Equal(t, k8s.ReasonReconcileSuccess, meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReconfiguring).Reason)
	assert.Empty(t, diskLimitEvents(rec, k8s.ReasonDiskSizeExceedsLimit))
}

func TestWithholdDiskGrowthOverLimit(t *testing.T) {
	newReq := func() contracts.CreateRequest {
		return contracts.CreateRequest{
			Class: contracts.VMClass{DiskDefaults: &contracts.DiskDefaults{Type: "thin", SizeGiB: 100}},
			Disks: []contracts.DiskSpec{{Name: "a", SizeGiB: 100}, {Name: "b", SizeGiB: 101}},
		}
	}
	prov := func(value *string) *infravirtrigaudiov1beta1.Provider {
		p := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
		if value != nil {
			p.Annotations = map[string]string{infravirtrigaudiov1beta1.ProviderMaxDiskSizeAnnotation: *value}
		}
		return p
	}

	req := newReq()
	assert.Nil(t, withholdDiskGrowthOverLimit(&req, nil, prov(nil), nil), "no limit withholds nothing")
	assert.Equal(t, newReq(), req)

	req = newReq()
	refusal := withholdDiskGrowthOverLimit(&req, qty("100Gi"), prov(nil), nil)
	require.NotNil(t, refusal)
	assert.Equal(t, k8s.ReasonDiskSizeExceedsLimit, refusal.reason)
	assert.Equal(t, int32(100), req.Class.DiskDefaults.SizeGiB, "at the limit is kept")
	assert.Equal(t, "thin", req.Class.DiskDefaults.Type)
	assert.Equal(t, int32(100), req.Disks[0].SizeGiB)
	assert.Zero(t, req.Disks[1].SizeGiB)
	assert.Contains(t, refusal.message, "spec.disks[b].sizeGiB (101Gi)")

	req = newReq()
	refusal = withholdDiskGrowthOverLimit(&req, qty("1Ti"), prov(ann("lots")), nil)
	require.NotNil(t, refusal, "an invalid annotation withholds every disk size (fail closed)")
	assert.Equal(t, k8s.ReasonDiskSizeLimitUnverifiable, refusal.reason)
	assert.Zero(t, req.Class.DiskDefaults.SizeGiB)
	assert.Zero(t, req.Disks[0].SizeGiB)
	assert.Zero(t, req.Disks[1].SizeGiB)
}

func TestParseMaxDiskSizeFlag(t *testing.T) {
	q, err := ParseMaxDiskSizeFlag("")
	require.NoError(t, err)
	assert.Nil(t, q, "empty means no ceiling")
	q, err = ParseMaxDiskSizeFlag("500Gi")
	require.NoError(t, err)
	assert.Equal(t, "500Gi", q.String())
	for _, bad := range []string{"none", "lots", "0", "500", "-5Gi"} {
		_, err := ParseMaxDiskSizeFlag(bad)
		assert.Error(t, err, bad)
	}
}

// TestReconcileVM_DiskSizeLimit_LibvirtDefaultPrimaryDisk (review M1): on a
// libvirt Provider a class without spec.diskDefaults.size still makes a 20 GiB
// primary disk, so a limit below that refuses the create; other provider
// types (whose default is the template's size, not known here) do not.
func TestReconcileVM_DiskSizeLimit_LibvirtDefaultPrimaryDisk(t *testing.T) {
	for _, tc := range []struct {
		ptype infravirtrigaudiov1beta1.ProviderType
		held  bool
	}{
		{ptype: infravirtrigaudiov1beta1.ProviderTypeLibvirt, held: true},
		{ptype: infravirtrigaudiov1beta1.ProviderTypeVSphere, held: false},
	} {
		t.Run(string(tc.ptype), func(t *testing.T) {
			ctx := context.Background()
			r, rp, _ := diskCapFixture(t, qty("10Gi"), nil, "")
			prov := &infravirtrigaudiov1beta1.Provider{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "test-prov"}, prov))
			prov.Spec.Type = tc.ptype
			require.NoError(t, r.Update(ctx, prov))

			stored, _ := storedVM(t, r)
			_, err := r.reconcileVM(ctx, stored)
			require.NoError(t, err)
			got, _ := storedVM(t, r)
			ready := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionReady)
			if !tc.held {
				assert.Len(t, rp.createReqs, 1)
				return
			}
			assert.Empty(t, rp.createReqs)
			require.NotNil(t, ready)
			assert.Equal(t, k8s.ReasonDiskSizeExceedsLimit, ready.Reason)
			assert.Contains(t, ready.Message, "the libvirt default primary disk (VMClass test-class sets no spec.diskDefaults.size) asks for 20Gi")
		})
	}
}
