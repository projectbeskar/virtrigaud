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
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMClone controller's clustered flow (ADR-0007 Addendum
// A, slice 3): the clone lands on its source VM's host only, its target VM and
// pending host are recorded BEFORE the Clone RPC, and a landing host that is
// gone, cordoned, not Ready or excluded stops the clone with a condition —
// never a fallback to another host.

// targetKey is the clone target's key in clusteredCloneFixture.
var targetKey = client.ObjectKey{Namespace: "default", Name: "clone-c-target"}

// boundSource is a clustered source VM bound to host-alpha in pool-a.
func boundSource() *infrav1beta1.VirtualMachine {
	src := sourceVMWithID("default", "src-c", "prov-c", "default.src-c")
	src.UID = "uid-src-c"
	src.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	return src
}

// getTarget re-reads the clone's target VM.
func getTarget(t *testing.T, r *VMCloneReconciler) *infrav1beta1.VirtualMachine {
	t.Helper()
	got := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), targetKey, got))
	return got
}

// cloneReadyCondition returns the clone's Ready condition.
func cloneReadyCondition(t *testing.T, c *infrav1beta1.VMClone) *metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(c.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, cond)
	return cond
}

func TestVMClone_Clustered_TargetAndPendingHostRecordedBeforeTheClone(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)

	// At Clone time the target VM already exists, is unbound, and records the
	// landing host as its pending host (A2): capture it from the API.
	var atClone *infrav1beta1.VirtualMachine
	cp.onClone = func() {
		atClone = getTarget(t, r)
	}

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.Equal(t, 1, cp.cloneCnt)
	require.NotNil(t, atClone, "the target VM exists before the Clone RPC")
	assert.Empty(t, atClone.Status.ID)
	require.NotNil(t, atClone.Status.Placement)
	assert.Equal(t, "host-alpha", atClone.Status.Placement.PendingHost, "the pending host is recorded before the clone")
	require.NotNil(t, atClone.Status.BoundProvider, "the pending host binds the target to the clone's Provider")
	assert.Equal(t, "prov-c", atClone.Status.BoundProvider.Name)

	req := cp.lastClone
	assert.Equal(t, contracts.VMRef{ID: "default.src-c", HostID: "host-alpha",
		Owner: contracts.ObjectIdentity{UID: "uid-src-c", Namespace: "default", Name: "src-c"}}, req.Source,
		"routed to the source's host, with the source's owner")
	assert.Equal(t, "host-alpha", req.TargetHostID, "the clone lands on the source's host")
	assert.Equal(t, contracts.ObjectIdentity{UID: string(atClone.UID), Namespace: "default", Name: "clone-c-target"}, req.TargetVM,
		"the clone is stamped with the target VM's identity, uid included")
	assert.NotEmpty(t, req.TargetVM.UID)

	target := getTarget(t, r)
	assert.Equal(t, "default.clone-c-target", target.Status.ID)
	assert.Equal(t, "host-alpha", target.Status.Placement.Host)
	assert.Empty(t, target.Status.Placement.PendingHost, "the pending host is promoted to the binding")
	placed := meta.FindStatusCondition(target.Status.Conditions, k8s.ConditionPlaced)
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionTrue, placed.Status)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
}

// TestVMClone_Clustered_LandingHostBlockedNeverFallsBack: a landing host that
// is gone, not a host of the Provider, being deleted, cordoned, or not Ready
// stops the clone with a condition naming why. No Clone RPC is sent, no target
// VM is created, and no other host is ever tried.
func TestVMClone_Clustered_LandingHostBlockedNeverFallsBack(t *testing.T) {
	notReady := readyCloneHost("host-alpha", "prov-c")
	notReady.Status.Health = infrav1beta1.HostHealthNotReady
	unknown := readyCloneHost("host-alpha", "prov-c")
	unknown.Status.Health = infrav1beta1.HostHealthUnknown
	cordoned := readyCloneHost("host-alpha", "prov-c")
	cordoned.Spec.Schedulable = false
	deleting := readyCloneHost("host-alpha", "prov-c")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{infrav1beta1.HostInUseFinalizer}
	foreign := readyCloneHost("host-alpha", "prov-other")
	other := readyCloneHost("host-beta", "prov-c") // a Ready host the clone must NOT fall back to

	for name, tc := range map[string]struct {
		host   client.Object
		reason string
	}{
		"gone":                    {other, cloneReasonSourceHostGone},
		"another provider's host": {foreign, cloneReasonSourceHostGone},
		"being deleted":           {deleting, cloneReasonSourceHostCordoned},
		"cordoned":                {cordoned, cloneReasonSourceHostCordoned},
		"not ready":               {notReady, cloneReasonSourceHostNotReady},
		"health unknown":          {unknown, cloneReasonSourceHostNotReady},
	} {
		t.Run(name, func(t *testing.T) {
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
			extra := []client.Object{tc.host}
			if tc.host != other {
				extra = append(extra, other)
			}
			r, clone := clusteredCloneFixture(t, boundSource(), cp, extra...)
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

			assert.Zero(t, cp.cloneCnt, "no clone is sent to a host that cannot take it, and no other host is tried")
			got := getClone(t, r, clone)
			assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
			assert.Equal(t, tc.reason, cloneReadyCondition(t, got).Reason)
			assert.Contains(t, got.Status.Message, "host-alpha")
			err := r.Get(context.Background(), targetKey, &infrav1beta1.VirtualMachine{})
			assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "no target VM is created for a clone that cannot land")
		})
	}
}

// TestVMClone_Clustered_ExcludedSourceHostStops: the source's host is excluded
// for the target VM (a name conflict was found there): the clone stops with a
// condition and never re-sends the clone or tries another host.
func TestVMClone_Clustered_ExcludedSourceHostStops(t *testing.T) {
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-c-target", Namespace: "default", UID: "uid-target",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: "uid-default-clone-c"}},
		Spec:   infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"}, ClassRef: infrav1beta1.ObjectRef{Name: "src-class"}},
		Status: infrav1beta1.VirtualMachineStatus{Placement: &infrav1beta1.PlacementStatus{ExcludedHosts: []string{"host-alpha"}}},
	}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	r, clone := clusteredCloneFixture(t, boundSource(), cp, readyCloneHost("host-alpha", "prov-c"), readyCloneHost("host-beta", "prov-c"), target)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, cloneReasonSourceHostExcluded, cloneReadyCondition(t, got).Reason)
	assert.Empty(t, getTarget(t, r).Status.Placement.PendingHost, "nothing is recorded on an excluded host")
}

// TestVMClone_Clustered_NameConflictExcludesTheHost: the provider refuses the
// clone with a name conflict (a domain the target does not own holds the
// name): the host is excluded for the target, its pending host cleared, and
// the clone waits; it is not re-sent.
func TestVMClone_Clustered_NameConflictExcludesTheHost(t *testing.T) {
	cp := &clonerProvider{cloneErr: contracts.NewConflictError("clone: libvirt domain \"default.clone-c-target\" already exists", nil)}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Equal(t, 1, cp.cloneCnt, "the clone is not re-sent to a host where its name is taken")
	target := getTarget(t, r)
	assert.Empty(t, target.Status.ID)
	assert.Empty(t, target.Status.Placement.PendingHost, "the conflict proves nothing was created: the pending host is released")
	assert.Equal(t, []string{"host-alpha"}, target.Status.Placement.ExcludedHosts)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, cloneReasonSourceHostExcluded, cloneReadyCondition(t, got).Reason)
}

// TestVMClone_Clustered_UnreachableHostRetriesTheSameHost: a host-scoped
// unavailability keeps the pending host, and the retry goes to the same host.
func TestVMClone_Clustered_UnreachableHostRetriesTheSameHost(t *testing.T) {
	cp := &clonerProvider{cloneErr: contracts.NewHostUnavailableError("clone: host \"host-alpha\" is unreachable", nil)}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.GreaterOrEqual(t, cp.cloneCnt, 2, "the clone is retried")
	assert.Equal(t, "host-alpha", cp.lastClone.TargetHostID)
	assert.Equal(t, "host-alpha", cp.lastClone.Source.HostID)
	target := getTarget(t, r)
	assert.Equal(t, "host-alpha", target.Status.Placement.PendingHost, "the pending host is kept: a clone may exist there")
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, k8s.ReasonHostUnavailable, cloneReadyCondition(t, got).Reason)

	// Once the host answers, the clone completes on it and binds.
	cp.cloneErr = nil
	cp.cloneResp = contracts.CloneResponse{TargetVmID: "default.clone-c-target"}
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	target = getTarget(t, r)
	assert.Equal(t, "default.clone-c-target", target.Status.ID)
	assert.Equal(t, "host-alpha", target.Status.Placement.Host)
	assert.Empty(t, target.Status.Placement.PendingHost)
}

// TestVMClone_Clustered_CopyInProgressRetriesTheSameHost: the provider answers
// that an earlier attempt's copy still runs (retryable): the pending host is
// kept and the clone retried on the same host, never elsewhere.
func TestVMClone_Clustered_CopyInProgressRetriesTheSameHost(t *testing.T) {
	cp := &clonerProvider{cloneErr: contracts.NewRetryableError(`clone: clone VM on host "host-alpha": the same copy is still running`, nil)}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.GreaterOrEqual(t, cp.cloneCnt, 2)
	assert.Equal(t, "host-alpha", cp.lastClone.TargetHostID)
	assert.Equal(t, "host-alpha", getTarget(t, r).Status.Placement.PendingHost)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, cloneReasonRetrying, cloneReadyCondition(t, got).Reason)
}

// TestVMClone_Clustered_TargetPendingElsewhereIsAConflict: a target VM that
// records a pending create on another host is never overwritten.
func TestVMClone_Clustered_TargetPendingElsewhereIsAConflict(t *testing.T) {
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-c-target", Namespace: "default", UID: "uid-target",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: "uid-default-clone-c"}},
		Spec:   infrav1beta1.VirtualMachineSpec{ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"}, ClassRef: infrav1beta1.ObjectRef{Name: "src-class"}},
		Status: infrav1beta1.VirtualMachineStatus{Placement: &infrav1beta1.PlacementStatus{PendingHost: "host-beta"}},
	}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	r, clone := clusteredCloneFixture(t, boundSource(), cp, readyCloneHost("host-alpha", "prov-c"), target)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Equal(t, cloneReasonTargetConflict, cloneReadyCondition(t, got).Reason)
	assert.Equal(t, "host-beta", getTarget(t, r).Status.Placement.PendingHost, "never overwritten")

	// The failed clone does not remove a target it refused to use.
	reconcileClone(t, r, clone, 2)
	assert.Equal(t, "host-beta", getTarget(t, r).Status.Placement.PendingHost, "a target the clone refused is left alone")
}

// TestVMClone_Clustered_SourceWithOnlyAPendingHostIsNeverCloned: a source VM
// whose create is still pending (no confirmed binding) is never sent a clone;
// the provider is never asked to pick a host.
func TestVMClone_Clustered_SourceWithOnlyAPendingHostIsNeverCloned(t *testing.T) {
	src := boundSource()
	src.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a"}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	r, clone := clusteredCloneFixture(t, src, cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Equal(t, reasonVMUnbound, cloneReadyCondition(t, got).Reason)
	err := r.Get(context.Background(), targetKey, &infrav1beta1.VirtualMachine{})
	assert.Error(t, err, "no target VM is created")
}
