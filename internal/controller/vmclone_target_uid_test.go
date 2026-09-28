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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that a clustered clone identifies the target it created by
// its UID (status.targetUID), never by the copyable clone-uid annotation, and
// that a failed clone decides about its target exactly once.

func TestVMClone_Clustered_RecordsTheTargetItCreated(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	target := getTarget(t, r)
	require.NotEmpty(t, target.UID)
	assert.Equal(t, string(target.UID), getClone(t, r, clone).Status.TargetUID, "the created target's uid is recorded")
}

// TestVMClone_FailedClone_StaleAnnotationOnARecreatedVMIsNeverDeleted: the VM
// under the target name carries this clone's uid annotation (copied by GitOps
// or `kubectl get -o yaml | apply`) but is not the object the clone created.
// The failed clone leaves it alone, records that, and never looks again.
func TestVMClone_FailedClone_StaleAnnotationOnARecreatedVMIsNeverDeleted(t *testing.T) {
	recreated := cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{})
	recreated.UID = types.UID("uid-recreated")
	r, clone := failedCloneWithTarget(t, recreated, infrav1beta1.ProviderTopologyCluster)

	reconcileClone(t, r, clone, 2)

	assert.False(t, targetGone(t, r), "a re-created VM with a copied annotation is never deleted")
	cond := meta.FindStatusCondition(getClone(t, r, clone).Status.Conditions, cloneConditionTargetCleanup)
	require.NotNil(t, cond, "the decision is recorded")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, cloneReasonTargetKept, cond.Reason)
	assert.Contains(t, cond.Message, "is not the one this clone created")
}

// TestVMClone_FailedClone_DecidesOnce: once the decision is recorded, the
// failed clone never acts on its target name again — even for its own,
// unbound target.
func TestVMClone_FailedClone_DecidesOnce(t *testing.T) {
	r, clone := failedCloneWithTarget(t, cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{}), infrav1beta1.ProviderTopologyCluster)
	live := getClone(t, r, clone)
	meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{Type: cloneConditionTargetCleanup,
		Status: metav1.ConditionFalse, Reason: cloneReasonTargetKept, Message: "decided earlier"})
	require.NoError(t, r.Status().Update(context.Background(), live))

	reconcileClone(t, r, clone, 3)
	assert.False(t, targetGone(t, r), "a decision made once is not made again")

	// And the first decision (a removal) is recorded, so a VM created later
	// under the target name is never looked at.
	r2, clone2 := failedCloneWithTarget(t, cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{}), infrav1beta1.ProviderTopologyCluster)
	reconcileClone(t, r2, clone2, 1)
	require.True(t, targetGone(t, r2))
	cond := meta.FindStatusCondition(getClone(t, r2, clone2).Status.Conditions, cloneConditionTargetCleanup)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, cloneReasonTargetRemoved, cond.Reason)
	later := cloneTargetVM("uid-clone-c", infrav1beta1.VirtualMachineStatus{})
	later.UID = "uid-later"
	later.ResourceVersion = ""
	require.NoError(t, r2.Create(context.Background(), later))
	reconcileClone(t, r2, clone2, 2)
	assert.False(t, targetGone(t, r2), "a VM created after the removal is never deleted")
}

// TestVMClone_Clustered_ReplacedTargetIsNeverUsed: while the clone is in
// progress its target is deleted and re-created (carrying the annotation):
// the clone refuses it instead of cloning onto it, and never removes it.
func TestVMClone_Clustered_ReplacedTargetIsNeverUsed(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	replaced := cloneTargetVM("uid-default-clone-c", infrav1beta1.VirtualMachineStatus{})
	replaced.UID = "uid-replacement"
	r, clone := clusteredCloneFixture(t, boundSource(), cp, readyCloneHost("host-alpha", "prov-c"), replaced)
	live := getClone(t, r, clone)
	live.Status.TargetUID = "uid-original"
	require.NoError(t, r.Status().Update(context.Background(), live))

	reconcileClone(t, r, clone, 4)

	assert.Zero(t, cp.cloneCnt, "never cloned onto a VM the clone did not create")
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "was not created by this clone")
	assert.False(t, targetGone(t, r), "and never removed")
	cond := meta.FindStatusCondition(got.Status.Conditions, cloneConditionTargetCleanup)
	require.NotNil(t, cond)
	assert.Equal(t, cloneReasonTargetKept, cond.Reason)
}
