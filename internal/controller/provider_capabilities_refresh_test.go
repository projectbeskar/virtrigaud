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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/util"
)

// countingCapabilityReader answers ReadCapabilities with caps (or err) and
// counts the reads.
type countingCapabilityReader struct {
	reads atomic.Int32
	caps  atomic.Pointer[contracts.Capabilities]
	err   atomic.Pointer[error]
}

func (c *countingCapabilityReader) ReadCapabilities(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Capabilities, error) {
	c.reads.Add(1)
	if e := c.err.Load(); e != nil {
		return contracts.Capabilities{}, *e
	}
	if p := c.caps.Load(); p != nil {
		return *p, nil
	}
	return contracts.Capabilities{}, nil
}

func (c *countingCapabilityReader) answer(caps contracts.Capabilities) { c.caps.Store(&caps) }

// rolledOut returns a Deployment of generation gen whose rollout of replicas
// replicas is complete.
func rolledOut(gen int64, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{UID: "dep-uid", Generation: gen},
		Spec:       appsv1.DeploymentSpec{Replicas: util.Int32Ptr(replicas)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: gen, Replicas: replicas, UpdatedReplicas: replicas,
			ReadyReplicas: replicas, AvailableReplicas: replicas,
		},
	}
}

func TestDeploymentRolloutComplete(t *testing.T) {
	assert.False(t, deploymentRolloutComplete(nil))
	assert.True(t, deploymentRolloutComplete(rolledOut(2, 1)))
	assert.True(t, deploymentRolloutComplete(rolledOut(2, 3)))

	unobserved := rolledOut(2, 1)
	unobserved.Status.ObservedGeneration = 1
	assert.False(t, deploymentRolloutComplete(unobserved), "the new spec is not observed yet")

	oldPodLeft := rolledOut(2, 1)
	oldPodLeft.Status.Replicas = 2 // surge: the old ReplicaSet's pod still counts
	oldPodLeft.Status.ReadyReplicas, oldPodLeft.Status.AvailableReplicas = 2, 2
	assert.False(t, deploymentRolloutComplete(oldPodLeft), "a pod of the superseded ReplicaSet is left")

	notUpdated := rolledOut(2, 2)
	notUpdated.Status.UpdatedReplicas = 1
	assert.False(t, deploymentRolloutComplete(notUpdated))

	notAvailable := rolledOut(2, 1)
	notAvailable.Status.AvailableReplicas = 0
	assert.False(t, deploymentRolloutComplete(notAvailable))

	scaledToZero := rolledOut(2, 0)
	assert.False(t, deploymentRolloutComplete(scaledToZero))

	defaulted := rolledOut(2, 1)
	defaulted.Spec.Replicas = nil
	assert.True(t, deploymentRolloutComplete(defaulted), "nil replicas means 1")
}

func TestCapabilityReadDue(t *testing.T) {
	now := time.Now()
	settle, refresh := 45*time.Second, 10*time.Minute
	const uid = types.UID("u1")

	due, _ := capabilityReadDue(nil, uid, "d/1", now, settle, refresh)
	assert.True(t, due, "never read")

	st := &capabilityReadState{uid: uid, revision: "d/1", readAt: now.Add(-10 * time.Second)}
	due, wait := capabilityReadDue(st, uid, "d/1", now, settle, refresh)
	assert.False(t, due)
	assert.Equal(t, 35*time.Second, wait, "the confirming read waits out the settle delay")

	st.readAt = now.Add(-settle)
	due, _ = capabilityReadDue(st, uid, "d/1", now, settle, refresh)
	assert.True(t, due, "the confirming read is due")

	st = &capabilityReadState{uid: uid, revision: "d/1", readAt: now.Add(-time.Minute), settled: true}
	due, wait = capabilityReadDue(st, uid, "d/1", now, settle, refresh)
	assert.False(t, due)
	assert.Equal(t, 9*time.Minute, wait)

	due, _ = capabilityReadDue(st, uid, "d/2", now, settle, refresh)
	assert.True(t, due, "a new runtime revision is read at once")
	due, _ = capabilityReadDue(st, "u2", "d/1", now, settle, refresh)
	assert.True(t, due, "a re-created Provider is read at once")

	st.readAt = now.Add(-refresh)
	due, _ = capabilityReadDue(st, uid, "d/1", now, settle, refresh)
	assert.True(t, due, "the periodic read is due")
}

func TestRequeueNoLaterThan(t *testing.T) {
	assert.Equal(t, ctrl.Result{RequeueAfter: time.Minute}, requeueNoLaterThan(ctrl.Result{}, time.Minute))
	assert.Equal(t, ctrl.Result{RequeueAfter: 30 * time.Second}, requeueNoLaterThan(ctrl.Result{RequeueAfter: 30 * time.Second}, time.Minute))
	assert.Equal(t, ctrl.Result{RequeueAfter: time.Minute}, requeueNoLaterThan(ctrl.Result{RequeueAfter: time.Hour}, time.Minute))
	assert.Equal(t, ctrl.Result{}, requeueNoLaterThan(ctrl.Result{}, 0))
}

// TestRefreshReportedCapabilities_Policy walks the read policy: nothing is
// read mid-rollout; the first read of a revision is confirmed after the
// settle delay; then only the periodic read; a new revision is read at once;
// a failed read keeps what was recorded and is retried.
func TestRefreshReportedCapabilities_Policy(t *testing.T) {
	ctx := context.Background()
	reader := &countingCapabilityReader{}
	reader.answer(contracts.Capabilities{SupportsLinkedClones: true})
	r := &ProviderReconciler{CapabilityReader: reader, capabilitySettleDelay: 45 * time.Second,
		capabilityRefreshInterval: 10 * time.Minute}
	p := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "u1"}}
	key := types.NamespacedName{Namespace: "ns", Name: "p"}.String()
	// The read is recorded as Reconcile records it after a successful status
	// write.
	refreshAndCommit := func(dep *appsv1.Deployment) time.Duration {
		after, read := r.refreshReportedCapabilities(ctx, p, dep)
		r.commitCapabilityRead(p, read)
		return after
	}

	midRollout := rolledOut(2, 1)
	midRollout.Status.Replicas = 2
	assert.Zero(t, refreshAndCommit(midRollout))
	assert.Zero(t, reader.reads.Load(), "no read while a superseded pod can answer")
	assert.Nil(t, p.Status.ReportedCapabilities)

	assert.Equal(t, 45*time.Second, refreshAndCommit(rolledOut(2, 1)))
	assert.Equal(t, int32(1), reader.reads.Load())
	require.NotNil(t, p.Status.ReportedCapabilities)
	assert.True(t, p.Status.ReportedCapabilities.SupportsLinkedClones)

	wait := refreshAndCommit(rolledOut(2, 1))
	assert.Equal(t, int32(1), reader.reads.Load(), "no read before the settle delay")
	assert.Greater(t, wait, 40*time.Second)

	// The settle delay has passed: the confirming read.
	v, _ := r.capabilityReads.Load(key)
	st, ok := v.(*capabilityReadState)
	require.True(t, ok)
	st.readAt = time.Now().Add(-time.Minute)
	reader.answer(contracts.Capabilities{SupportsLinkedClones: false})
	assert.Equal(t, 10*time.Minute, refreshAndCommit(rolledOut(2, 1)))
	assert.Equal(t, int32(2), reader.reads.Load())
	assert.False(t, p.Status.ReportedCapabilities.SupportsLinkedClones, "the confirming read wins")

	refreshAndCommit(rolledOut(2, 1))
	assert.Equal(t, int32(2), reader.reads.Load(), "settled: no read until the refresh interval")

	// A new revision (a new image): read at once.
	assert.Equal(t, 45*time.Second, refreshAndCommit(rolledOut(3, 1)))
	assert.Equal(t, int32(3), reader.reads.Load())

	// A failed read keeps the recorded capabilities and is retried.
	failure := errors.New("unavailable")
	reader.err.Store(&failure)
	assert.Equal(t, capabilityRetryInterval, refreshAndCommit(rolledOut(4, 1)))
	assert.Equal(t, int32(4), reader.reads.Load())
	require.NotNil(t, p.Status.ReportedCapabilities)
	assert.Equal(t, capabilityRetryInterval, refreshAndCommit(rolledOut(4, 1)),
		"the revision is still unread: retried")

	r.forgetCapabilityReads(types.NamespacedName{Namespace: "ns", Name: "p"})
	_, ok = r.capabilityReads.Load(key)
	assert.False(t, ok)
}

func TestCapabilitySettleDelayFor(t *testing.T) {
	r := &ProviderReconciler{}
	dep := rolledOut(1, 1)
	assert.Equal(t, defaultPodTerminationGracePeriod+capabilitySettleMargin, r.capabilitySettleDelayFor(dep))
	dep.Spec.Template.Spec.TerminationGracePeriodSeconds = util.Int64Ptr(60)
	assert.Equal(t, 60*time.Second+capabilitySettleMargin, r.capabilitySettleDelayFor(dep))
	assert.Equal(t, defaultCapabilityRefreshInterval, r.capabilityRefreshEvery())
}
