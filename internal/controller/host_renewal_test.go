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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/devgate"
)

// TestRenewalStatePhases: the mirrored phase follows the renewal's progress.
func TestRenewalStatePhases(t *testing.T) {
	r := &renewalState{KeyPin: strings.Repeat("ab", 32)}
	assert.Equal(t, infravirtrigaudiov1beta1.HostAgentRenewalRequested, r.phase())
	r.Certificate = []byte{1}
	assert.Equal(t, infravirtrigaudiov1beta1.HostAgentRenewalIssued, r.phase())
	now := metav1.Now()
	r.PinnedAt = &now
	assert.Equal(t, infravirtrigaudiov1beta1.HostAgentRenewalOverlap, r.phase())
}

// TestCompleteRenewalPins: ending an overlap leaves only the new key pinned,
// records the old one, and takes the renewed expiry.
func TestCompleteRenewalPins(t *testing.T) {
	oldPin, newPin := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	na := metav1.NewTime(time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second))
	st := &hostEnrolmentState{Current: enrolmentFor(strings.Repeat("c3", 16), "uid", 2, oldPin),
		Renewal: &renewalState{KeyPin: newPin, NotAfter: &na}}
	st.Current.KeyPins = append(st.Current.KeyPins, newPin)
	removed := st.completeRenewalPins()
	assert.Equal(t, []string{oldPin}, removed)
	assert.Equal(t, []string{newPin}, st.Current.KeyPins)
	assert.Contains(t, st.PreviousKeyPins, oldPin)
	assert.Equal(t, na, st.Current.NotAfter)
	assert.Nil(t, st.Renewal)
	assert.Nil(t, st.completeRenewalPins(), "nothing to complete")

	// The renewed certificate's start replaces the old one's.
	nb := metav1.NewTime(time.Now().Truncate(time.Second))
	st.Renewal = &renewalState{KeyPin: oldPin, NotAfter: &na, NotBefore: &nb}
	st.completeRenewalPins()
	require.NotNil(t, st.Current.NotBefore)
	assert.Equal(t, nb, *st.Current.NotBefore)
}

// TestRenewalOverdueAt: five sixths of the pinned certificate's lifetime;
// nothing without its recorded start.
func TestRenewalOverdueAt(t *testing.T) {
	nb := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := &currentEnrolment{NotAfter: metav1.NewTime(nb.Add(30 * time.Hour)), NotBefore: &nb}
	at, ok := c.renewalOverdueAt()
	require.True(t, ok)
	assert.Equal(t, nb.Add(25*time.Hour), at)
	c.NotBefore = nil
	_, ok = c.renewalOverdueAt()
	assert.False(t, ok, "a state without the certificate's start derives no deadline")
	_, ok = (*currentEnrolment)(nil).renewalOverdueAt()
	assert.False(t, ok)
}

// TestMirrorReportsAnOverdueRenewal: past five sixths of the pinned
// certificate's lifetime with no renewal in progress, AgentEnrolled stays
// True with reason RenewalOverdue and one Warning event is emitted on the
// transition; before it, or with a renewal in progress, the reason is
// Enrolled.
func TestMirrorReportsAnOverdueRenewal(t *testing.T) {
	s := coverageTestScheme(t)
	host := &infravirtrigaudiov1beta1.Host{ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "infra"},
		Spec: infravirtrigaudiov1beta1.HostSpec{Identity: "id-1"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(host).WithStatusSubresource(&infravirtrigaudiov1beta1.Host{}).Build()
	rec := record.NewFakeRecorder(8)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(10 * time.Hour)
	r := &HostEnrolmentReconciler{Client: c, Recorder: rec, Now: func() time.Time { return now },
		Config: &HostEnrolmentConfig{Namespace: "host-agent-pki", TrustDomain: "cluster.test"}}
	nb := metav1.NewTime(start)
	st := &hostEnrolmentState{Current: enrolmentFor(strings.Repeat("c3", 16), "uid", 2, strings.Repeat("aa", 32))}
	st.Current.NotBefore, st.Current.NotAfter = &nb, metav1.NewTime(start.Add(30*time.Hour))
	reason := func() string {
		t.Helper()
		got := &infravirtrigaudiov1beta1.Host{}
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(host), got))
		cond := meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentEnrolled)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		host = got
		return cond.Reason
	}
	require.NoError(t, r.mirror(context.Background(), host, st, "uid"))
	assert.Equal(t, infravirtrigaudiov1beta1.HostReasonAgentEnrolled, reason(), "before the deadline")
	now = start.Add(26 * time.Hour)
	require.NoError(t, r.mirror(context.Background(), host, st, "uid"))
	assert.Equal(t, reasonAgentRenewalOverdue, reason())
	require.NoError(t, r.mirror(context.Background(), host, st, "uid"))
	assert.Len(t, rec.Events, 1, "one event on the transition")
	assert.Contains(t, <-rec.Events, eventReasonAgentRenewalOverdue)
	st.Renewal = &renewalState{KeyPin: strings.Repeat("bb", 32)}
	require.NoError(t, r.mirror(context.Background(), host, st, "uid"))
	assert.Equal(t, infravirtrigaudiov1beta1.HostReasonAgentEnrolled, reason(), "a renewal in progress is not overdue")
}

// TestRekeyClearsRenewal: a re-key ends a renewal of the replaced enrolment;
// its new key counts as previously pinned.
func TestRekeyClearsRenewal(t *testing.T) {
	newPin := strings.Repeat("bb", 32)
	st := &hostEnrolmentState{Current: enrolmentFor(strings.Repeat("c3", 16), "uid", 2, strings.Repeat("aa", 32)),
		Renewal: &renewalState{KeyPin: newPin}}
	st.rekey(enrolmentFor(strings.Repeat("d4", 16), "uid", 3, strings.Repeat("cc", 32)), time.Now())
	assert.Nil(t, st.Renewal)
	assert.True(t, st.keyPreviouslyPinned(newPin))
}

// TestResolveHostRenewal: only a development build renders a renewed
// certificate, and only while a renewal holds one.
func TestResolveHostRenewal(t *testing.T) {
	ctx := context.Background()
	h := hostCR("host-a", "ch", nil)
	h.UID = "uid-host-a"
	h.Spec.Identity = "serial-1"
	cfg := &HostEnrolmentConfig{Namespace: "pki"}
	c := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).Build()
	l, err := loadHostEnrolment(ctx, c, cfg, h)
	require.NoError(t, err)
	l.state.Current = enrolmentFor(strings.Repeat("c3", 16), "uid", 2, strings.Repeat("aa", 32))
	l.state.Renewal = &renewalState{KeyPin: strings.Repeat("bb", 32), Certificate: []byte{0x30, 0x01}}
	require.NoError(t, saveHostEnrolment(ctx, c, cfg, h, &l))
	got := resolveHostRenewal(ctx, c, cfg, h)
	if !devgate.Compiled {
		assert.Nil(t, got, "a release build renders no renewed certificate")
		return
	}
	assert.Equal(t, []byte{0x30, 0x01}, got)
	h.Spec.Revoked = true
	assert.Nil(t, resolveHostRenewal(ctx, c, cfg, h), "a revoked Host gets none")
}

// TestFailRenewalBackoff: each failure of a key's renewal doubles its wait
// (up to the cap) and the key is given up after maxRenewalFailures; a pinned
// renewal is never failed.
func TestFailRenewalBackoff(t *testing.T) {
	now := time.Now()
	pin := strings.Repeat("bb", 32)
	st := &hostEnrolmentState{}
	for i := 1; i <= maxRenewalFailures; i++ {
		st.Renewal = &renewalState{KeyPin: pin}
		a, gaveUp := st.failRenewal(now, "refused")
		assert.Nil(t, st.Renewal)
		assert.Equal(t, i, a.Failures)
		assert.Equal(t, i >= maxRenewalFailures, gaveUp)
		assert.True(t, st.renewalHeldBack(pin, now))
		if i < maxRenewalFailures {
			assert.False(t, st.renewalHeldBack(pin, now.Add(renewalBackoff(i, renewalRetryFloor)+time.Second)))
		}
	}
	assert.True(t, st.renewalHeldBack(pin, now.Add(1000*time.Hour)), "given up for good")
	assert.False(t, st.renewalHeldBack(strings.Repeat("cc", 32), now), "another key is not held back")
	assert.Equal(t, renewalRetryMax, renewalBackoff(40, renewalRetryBase))
	pinned := metav1.Now()
	st.Renewal = &renewalState{KeyPin: pin, PinnedAt: &pinned}
	_, gaveUp := st.failRenewal(now, "x")
	assert.False(t, gaveUp)
	assert.NotNil(t, st.Renewal, "a pinned renewal is never failed")
	for i := 0; i < 2*maxRenewalAttempts; i++ {
		st.Renewal = &renewalState{KeyPin: strings.Repeat(string(rune('a'+i%6)), 1) + strings.Repeat("0", 63)}
		st.failRenewal(now, "x")
	}
	assert.LessOrEqual(t, len(st.RenewalAttempts), maxRenewalAttempts)
}

// TestManagerIdentity: set, read, cleared by an empty username.
func TestManagerIdentity(t *testing.T) {
	cfg := &HostEnrolmentConfig{}
	_, ok := cfg.managerIdentity()
	assert.False(t, ok)
	cfg.SetManagerIdentity(ManagerIdentity{Username: "system:serviceaccount:a:b", Groups: []string{"g"}})
	id, ok := cfg.managerIdentity()
	assert.True(t, ok)
	assert.Equal(t, "system:serviceaccount:a:b", id.Username)
	cfg.SetManagerIdentity(ManagerIdentity{})
	_, ok = cfg.managerIdentity()
	assert.False(t, ok)
	name, err := newRenewalRequestName(strings.NewReader(strings.Repeat("x", 12)))
	require.NoError(t, err)
	assert.Equal(t, renewalRequestPrefix+"787878787878787878787878", name)
}

// TestRenewalBackoffFitsShortLifetimes: the first retry of a renewal comes
// well within the bound after which the agent draws a new key (half the
// lifetime left when it asked), down to one-hour certificates; long
// lifetimes keep the 10-minute base.
func TestRenewalBackoffFitsShortLifetimes(t *testing.T) {
	assert.Equal(t, renewalRetryBase, renewalRetryBaseFor(10*24*time.Hour))
	assert.Equal(t, renewalRetryFloor, renewalRetryBaseFor(2*time.Minute))
	for _, life := range []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
		now := time.Now()
		left := life / 3 // renewal at two thirds of the lifetime
		st := &hostEnrolmentState{Current: enrolmentFor(strings.Repeat("c3", 16), "uid", 2, strings.Repeat("aa", 32)),
			Renewal: &renewalState{KeyPin: strings.Repeat("bb", 32)}}
		st.Current.NotAfter = metav1.NewTime(now.Add(left))
		a, _ := st.failRenewal(now, "refused")
		wait := a.NextTry.Sub(now)
		assert.LessOrEqual(t, wait, left/4+time.Second, "lifetime %s: the retry comes well before the agent's new-key bound (left/2)", life)
		assert.LessOrEqual(t, wait, renewalRetryBase+time.Second)
	}
}

// TestMirroredRequeuesConflicts: a status mirror that lost an optimistic-lock
// race is requeued without an error; other errors are returned unchanged.
func TestMirroredRequeuesConflicts(t *testing.T) {
	conflict := fmt.Errorf("mirror enrolment status of Host a/b: %w",
		apierrors.NewConflict(schema.GroupResource{Group: "infra.virtrigaud.io", Resource: "hosts"}, "b", errors.New("modified")))
	res, err := mirrored(ctrl.Result{RequeueAfter: time.Minute}, conflict)
	require.NoError(t, err)
	assert.True(t, res.Requeue)
	other := errors.New("boom")
	res, err = mirrored(ctrl.Result{RequeueAfter: time.Minute}, other)
	assert.Equal(t, other, err)
	assert.Equal(t, time.Minute, res.RequeueAfter)
	res, err = mirrored(ctrl.Result{RequeueAfter: time.Minute}, nil)
	require.NoError(t, err)
	assert.Equal(t, time.Minute, res.RequeueAfter)
}
