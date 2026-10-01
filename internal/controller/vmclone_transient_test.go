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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestVMClone_Clustered_TransientReadBeforeTheCopyWaitsWithBackoff: the
// provider answered that a read on the host failed before anything was copied
// (ADR-0007 Slice 5 lab follow-up; mapped to a retryable error by the
// transport): the clone stays Pending with the blocked-VM backoff, keeps its
// target and pending host, is never failed, and proceeds once the host
// answers.
func TestVMClone_Clustered_TransientReadBeforeTheCopyWaitsWithBackoff(t *testing.T) {
	cp := &clonerProvider{cloneErr: contracts.NewRetryableError(
		`clone: clone VM on host "host-alpha": a read on the host failed before anything was copied; the clone is retried`, nil)}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)

	res := reconcileClone(t, r, clone, 6)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "never Failed")
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionFailed))
	ready := cloneReadyCondition(t, got)
	assert.Equal(t, cloneReasonRetrying, ready.Reason)
	assert.Contains(t, ready.Message, "a read on the host failed before anything was copied")
	assert.Contains(t, ready.Message, "with a backoff of up to "+blockedRetryMax.String())
	assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin)
	assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMax)
	assert.GreaterOrEqual(t, cp.cloneCnt, 2, "retried on the same host")
	assert.Equal(t, "host-alpha", cp.lastClone.TargetHostID)
	target := getTarget(t, r)
	require.NotNil(t, target.Status.Placement)
	assert.Equal(t, "host-alpha", target.Status.Placement.PendingHost, "the target keeps its pending host")
	assert.False(t, targetGone(t, r), "the target is never removed for a transient error")

	cp.cloneErr = nil
	cp.cloneResp = contracts.CloneResponse{TargetVmID: "default.clone-c-target"}
	reconcileClone(t, r, clone, 2)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
	assert.Equal(t, "default.clone-c-target", getTarget(t, r).Status.ID)
}
