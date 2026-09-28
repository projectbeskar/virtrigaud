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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a clustered clone is made at EXACTLY the size it was
// admitted at: the Clone RPC carries the admitted size as its class override,
// a VMClass edited after admission does not change it (the size is frozen, as
// a pending create's is), and the bind records that size.

// sentClass decodes the class override a Clone RPC carried.
func sentClass(t *testing.T, cp *clonerProvider) infrav1beta1.VMClassSpec {
	t.Helper()
	require.NotNil(t, cp.lastClone, "a Clone RPC was sent")
	var spec infrav1beta1.VMClassSpec
	require.NoError(t, json.Unmarshal([]byte(cp.lastClone.ClassJSON), &spec))
	return spec
}

// pendingCloneTarget is the clone's target, already admitted on host-alpha at
// pending (with no balloon ceiling), sized from class.
func pendingCloneTarget(class string, pending infrav1beta1.PlacementResources) *infrav1beta1.VirtualMachine {
	ceiling := int64(0)
	return &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-c-target", Namespace: "default", UID: "uid-target",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: "uid-default-clone-c"}},
		Spec: infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"}, ClassRef: infrav1beta1.ObjectRef{Name: class}},
		Status: infrav1beta1.VirtualMachineStatus{Placement: &infrav1beta1.PlacementStatus{PendingHost: "host-alpha",
			PendingResources: &pending, MemoryCeilingMiB: &ceiling}},
	}
}

// TestVMClone_Clustered_RPCSendsTheAdmittedSize: the Clone RPC carries exactly
// the admitted size and headroom as its class override — also for a clone of
// its source's size — so what is created is what was admitted.
func TestVMClone_Clustered_RPCSendsTheAdmittedSize(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	r, clone := clusteredCloneFixture(t, sizedSource(), cp, smallCloneHost(8))
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	spec := sentClass(t, cp)
	assert.EqualValues(t, 4, spec.CPU)
	assert.EqualValues(t, 8192, spec.Memory.Value()/bytesPerMiB)
	require.NotNil(t, spec.PerformanceProfile)
	assert.True(t, spec.PerformanceProfile.MemoryHotAddEnabled, "the source was provisioned with a balloon ceiling")
}

// TestVMClone_Clustered_ClassChangedAfterAdmissionIsFrozen: the VMClass is
// edited after the clone was admitted (its pending host and size recorded) and
// before the Clone RPC. The clone is made at its ADMITTED size — the RPC
// carries it, and the bind records it — never at the class's new size.
func TestVMClone_Clustered_ClassChangedAfterAdmissionIsFrozen(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	grown := srcCloneClass()
	grown.Name = "tclass"
	grown.Spec.CPU = 16 // 2 when the clone was admitted
	target := pendingCloneTarget("tclass", infrav1beta1.PlacementResources{CPU: 2, MemoryMiB: 4096})
	r, clone := clusteredCloneFixture(t, boundSource(), cp, readyCloneHost("host-alpha", "prov-c"), grown, target)
	live := getClone(t, r, clone)
	live.Spec.Target.ClassRef = &infrav1beta1.LocalObjectReference{Name: "tclass"}
	require.NoError(t, r.Update(context.Background(), live))

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.Equal(t, 1, cp.cloneCnt)
	spec := sentClass(t, cp)
	assert.EqualValues(t, 2, spec.CPU, "the admitted size is sent, not the class's new 16 vCPU")
	assert.EqualValues(t, 4096, spec.Memory.Value()/bytesPerMiB)
	got := getTarget(t, r)
	require.NotNil(t, got.Status.CurrentResources)
	assert.EqualValues(t, 2, *got.Status.CurrentResources.CPU, "the bind records the size actually sent")
	assert.EqualValues(t, 4096, *got.Status.CurrentResources.MemoryMiB)
	assert.Nil(t, got.Status.Placement.PendingResources)
}

// TestVMClone_Clustered_UnreadableClassBeforeTheRPCWaits: a clone already
// admitted whose VMClass can no longer be read is not sent (its headroom is
// unknown): it waits with a condition instead of being sent without an
// override.
func TestVMClone_Clustered_UnreadableClassBeforeTheRPCWaits(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	src := boundSource()
	src.Spec.ClassRef = infrav1beta1.ObjectRef{Name: "deleted-class"}
	target := pendingCloneTarget("deleted-class", infrav1beta1.PlacementResources{CPU: 2, MemoryMiB: 4096})
	r, clone := clusteredCloneFixture(t, src, cp, readyCloneHost("host-alpha", "prov-c"), target)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt, "never sent without its override")
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Contains(t, cloneReadyCondition(t, got).Message, "size cannot be determined")
}
