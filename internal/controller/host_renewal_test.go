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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			assert.False(t, st.renewalHeldBack(pin, now.Add(renewalBackoff(i)+time.Second)))
		}
	}
	assert.True(t, st.renewalHeldBack(pin, now.Add(1000*time.Hour)), "given up for good")
	assert.False(t, st.renewalHeldBack(strings.Repeat("cc", 32), now), "another key is not held back")
	assert.Equal(t, renewalRetryMax, renewalBackoff(40))
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
