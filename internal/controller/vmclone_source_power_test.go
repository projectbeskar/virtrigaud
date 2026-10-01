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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMClone controller's side of the libvirt clone
// source-state refusal (ADR-0007 Slice 5 lab, B2): a clone the provider
// refuses because its source VM is not powered off stays Pending with
// Ready=False/SourceMustBePoweredOff and a backoff — never Failed, its
// clustered target kept with its pending host — and proceeds once the source
// is powered off.

// sourceRunningAnswer is the Clone error the transport client returns for the
// provider's VM_SOURCE_RUNNING refusal (mapGRPCError).
func sourceRunningAnswer() error {
	return contracts.NewRetryableError(
		`clone: failed to clone VM: the clone's source VM is "running": power off the source VM to clone it `+
			`(a libvirt full clone copies the disk of a powered-off VM); nothing was copied`,
		fmt.Errorf("%w: %w", contracts.ErrVMSourceRunning, errors.New("rpc error: code = FailedPrecondition")))
}

// requireWaitingForPowerOff asserts clone waits for its source to be powered
// off: Pending, Ready=False and Cloning=False with SourceMustBePoweredOff.
func requireWaitingForPowerOff(t *testing.T, got *infrav1beta1.VMClone) {
	t.Helper()
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "never Failed")
	ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, cloneReasonSourceMustBePoweredOff, ready.Reason)
	assert.Contains(t, ready.Message, "power off the source VM to clone it")
	cloning := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionCloning)
	require.NotNil(t, cloning)
	assert.Equal(t, metav1.ConditionFalse, cloning.Status)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionFailed))
}

// powerOffWarnings counts the SourceMustBePoweredOff events recorded.
func powerOffWarnings(t *testing.T, r *VMCloneReconciler) int {
	t.Helper()
	n := 0
	for _, e := range recordedEvents(t, r) {
		if strings.Contains(e, cloneReasonSourceMustBePoweredOff) {
			n++
		}
	}
	return n
}

func TestVMClone_SingleHost_SourceRunningWaitsThenProceeds(t *testing.T) {
	s := cloneTestScheme(t)
	prov := runningProvider("default", "prov-1")
	src := sourceVMWithID("default", "src-vm", "prov-1", "default.src-vm")
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default"},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	cp := &clonerProvider{cloneErr: sourceRunningAnswer()}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, prov, src, clone)

	res := reconcileClone(t, r, clone, 4)
	got := getClone(t, r, clone)
	requireWaitingForPowerOff(t, got)
	assert.GreaterOrEqual(t, cp.cloneCnt, 2, "retried, not given up")
	assert.GreaterOrEqual(t, res.RequeueAfter, blockedRetryMin, "backed off")
	assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMax)
	assert.Equal(t, 1, powerOffWarnings(t, r), "one Warning event, on the transition only")
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-target"}, &infrav1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "a single-host clone creates its target only after the clone")
	assert.Empty(t, got.Status.TargetVMID)

	// The source is powered off: the next attempt clones and binds.
	cp.cloneErr = nil
	cp.cloneResp = contracts.CloneResponse{TargetVmID: "default.clone-target"}
	reconcileClone(t, r, clone, 2)
	got = getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	assert.Equal(t, "default.clone-target", got.Status.TargetVMID)
	ready := meta.FindStatusCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

func TestVMClone_Clustered_SourceRunningKeepsTheTargetAndProceeds(t *testing.T) {
	cp := &clonerProvider{cloneErr: sourceRunningAnswer()}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)

	reconcileClone(t, r, clone, 5)
	requireWaitingForPowerOff(t, getClone(t, r, clone))
	assert.GreaterOrEqual(t, cp.cloneCnt, 2, "retried on the same host")
	assert.Equal(t, "host-alpha", cp.lastClone.TargetHostID)
	target := getTarget(t, r)
	require.NotNil(t, target.Status.Placement)
	assert.Equal(t, "host-alpha", target.Status.Placement.PendingHost, "the pre-created target keeps its pending host and waits")
	assert.Empty(t, target.Status.Placement.ExcludedHosts, "the source's host is never excluded for this")
	assert.False(t, targetGone(t, r), "the target is never removed for this")
	assert.Equal(t, 1, powerOffWarnings(t, r))

	// The source is powered off: the clone proceeds on the same host and binds
	// the target it created.
	cp.cloneErr = nil
	cp.cloneResp = contracts.CloneResponse{TargetVmID: "default.clone-c-target"}
	reconcileClone(t, r, clone, 2)
	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	target = getTarget(t, r)
	assert.Equal(t, "default.clone-c-target", target.Status.ID)
	assert.Equal(t, "host-alpha", target.Status.Placement.Host)
	assert.Empty(t, target.Status.Placement.PendingHost, "the pending host was promoted to the binding")
}

// TestHoldCloneForRunningSource_Backoff: the backoff counts from when the
// hold began (the Ready condition's last transition), so a clone waiting
// longer is re-checked less often, up to blockedRetryMax.
func TestHoldCloneForRunningSource_Backoff(t *testing.T) {
	s := cloneTestScheme(t)
	began := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-1", Namespace: "default"},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-target"},
		},
	}
	r := newCloneReconciler(s, &stubResolver{provider: &clonerProvider{}}, clone)
	first := r.holdCloneForRunningSource(context.Background(), clone, sourceRunningAnswer())
	assert.Equal(t, blockedRetryMin, first.RequeueAfter, "the first hold is re-checked soon")
	msg := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady).Message

	// The same hold, begun two minutes ago.
	for i := range clone.Status.Conditions {
		if clone.Status.Conditions[i].Type == infrav1beta1.VMCloneConditionReady {
			clone.Status.Conditions[i].LastTransitionTime = began
		}
	}
	later := r.holdCloneForRunningSource(context.Background(), clone, sourceRunningAnswer())
	assert.InDelta(t, (2 * time.Minute).Seconds(), later.RequeueAfter.Seconds(), 5, "backed off by the time already held")
	ready := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	assert.Equal(t, began.Unix(), ready.LastTransitionTime.Unix(), "a repeated hold keeps its start")
	assert.Equal(t, msg, ready.Message, "and its message")
}

func TestClonesWaitingOnSource(t *testing.T) {
	s := cloneTestScheme(t)
	waiting := func(name, ns, source string, phase infrav1beta1.ClonePhase, reason string) *infrav1beta1.VMClone {
		c := &infrav1beta1.VMClone{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: infrav1beta1.VMCloneSpec{
				Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: source}},
				Target: infrav1beta1.VMCloneTarget{Name: name + "-target"},
			},
			Status: infrav1beta1.VMCloneStatus{Phase: phase},
		}
		if reason != "" {
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: infrav1beta1.VMCloneConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: "m"})
		}
		return c
	}
	r := newCloneReconciler(s, &stubResolver{provider: &clonerProvider{}},
		waiting("waits", "default", "web", infrav1beta1.ClonePhasePending, cloneReasonSourceMustBePoweredOff),
		waiting("other-reason", "default", "web", infrav1beta1.ClonePhasePending, infrav1beta1.VMCloneReasonSourceNotFound),
		waiting("other-source", "default", "db", infrav1beta1.ClonePhasePending, cloneReasonSourceMustBePoweredOff),
		waiting("other-namespace", "team-b", "web", infrav1beta1.ClonePhasePending, cloneReasonSourceMustBePoweredOff),
		waiting("done", "default", "web", infrav1beta1.ClonePhaseReady, cloneReasonSourceMustBePoweredOff),
	)
	vm := &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	assert.Equal(t, []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: "default", Name: "waits"}}},
		r.clonesWaitingOnSource(context.Background(), vm), "only unfinished clones of this VM that wait for its power-off")

	p := sourcePowerStateChanged()
	on := &infrav1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	on.Status.PowerState = infrav1beta1.ObservedPowerStateOn
	off := on.DeepCopy()
	off.Status.PowerState = infrav1beta1.ObservedPowerStateOff
	relabelled := on.DeepCopy()
	relabelled.Labels = map[string]string{"x": "y"}
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: on, ObjectNew: off}), "a power state change re-drives")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: on, ObjectNew: relabelled}), "any other change does not")
	assert.False(t, p.Create(event.CreateEvent{Object: on}))
	assert.False(t, p.Delete(event.DeleteEvent{Object: on}))
}
