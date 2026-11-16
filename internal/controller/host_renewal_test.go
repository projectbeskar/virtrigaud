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
