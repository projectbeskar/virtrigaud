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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/certrequest"
)

// pinsSecretWith is a pins Secret holding the enrolment state st and the
// store state ss, encoded as the manager writes them.
func pinsSecretWith(t *testing.T, st hostEnrolmentState, ss HostStoreState) *corev1.Secret {
	t.Helper()
	e, err := json.Marshal(st)
	require.NoError(t, err)
	s, err := encodeStoreState(ss)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "host-agent-pki", Name: hostEnrolmentSecretPrefix + "x", ResourceVersion: "1",
			Labels:      map[string]string{k8sLabelComponent: hostEnrolmentComponent},
			Annotations: map[string]string{certrequest.AnnotationHost: "default/host-a"},
		},
		Data: map[string][]byte{hostEnrolmentStateKey: e, hostStoreStateKey: s},
	}
}

// The pins Secret watches enqueue on a change of anything a watcher decides
// on, and not on the heartbeat's writes of the store positions alone.
func TestPinsTrustChanged(t *testing.T) {
	pin := strings.Repeat("aa", 32)
	cur := enrolmentFor(strings.Repeat("c3", 16), "uid-prov-a", 2, pin)
	base := hostEnrolmentState{Current: cur}
	enrolID, err := hostidentity.DecodeID(cur.Record.EnrolmentID)
	require.NoError(t, err)
	store := HostStoreState{Current: hostidentity.StoreRecord{StoreID: [16]byte{1}, EnrolmentID: enrolID, Seq: 10, Head: [32]byte{1}}}
	old := pinsSecretWith(t, base, store)
	p := pinsTrustChanged()
	update := func(n *corev1.Secret) bool {
		n.ResourceVersion = "2"
		return p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: n})
	}

	t.Run("store positions only", func(t *testing.T) {
		moved := store
		moved.Current.Seq, moved.Current.Head = 42, [32]byte{2}
		moved.Current.CheckpointSeq, moved.Current.CheckpointHead = 40, [32]byte{3}
		assert.False(t, update(pinsSecretWith(t, base, moved)), "a heartbeat position write does not enqueue")
		assert.False(t, update(pinsSecretWith(t, base, store)), "a write that changes nothing does not enqueue")
	})

	clone := func() hostEnrolmentState {
		c := *cur
		c.KeyPins = append([]string(nil), cur.KeyPins...)
		return hostEnrolmentState{Current: &c}
	}
	cases := map[string]func() *corev1.Secret{
		"pin change": func() *corev1.Secret {
			st := clone()
			st.Current.KeyPins = []string{strings.Repeat("bb", 32)}
			return pinsSecretWith(t, st, store)
		},
		"re-key": func() *corev1.Secret {
			st := hostEnrolmentState{Current: enrolmentFor(strings.Repeat("d4", 16), "uid-prov-a", 3, strings.Repeat("cc", 32)),
				PreviousKeyPins: []string{pin}}
			return pinsSecretWith(t, st, store)
		},
		"expiry change": func() *corev1.Secret {
			st := clone()
			st.Current.NotAfter = metav1.NewTime(time.Now().Add(-time.Minute))
			return pinsSecretWith(t, st, store)
		},
		"revocation": func() *corev1.Secret {
			st := hostEnrolmentState{RetiredEnrolmentIDs: []string{cur.Record.EnrolmentID}}
			return pinsSecretWith(t, st, store)
		},
		"store regression hold": func() *corev1.Secret {
			held := store
			held.RegressedReason, held.RegressedEnrolmentID = "StoreRolledBack", enrolID
			return pinsSecretWith(t, base, held)
		},
		"undecodable store state": func() *corev1.Secret {
			n := pinsSecretWith(t, base, store)
			n.Data[hostStoreStateKey] = []byte("{")
			return n
		},
		"another data key": func() *corev1.Secret {
			n := pinsSecretWith(t, base, store)
			n.Data["other"] = []byte("x")
			return n
		},
		"annotation": func() *corev1.Secret {
			n := pinsSecretWith(t, base, store)
			n.Annotations[certrequest.AnnotationHost] = "default/host-b"
			return n
		},
		"deletion": func() *corev1.Secret {
			n := pinsSecretWith(t, base, store)
			now := metav1.Now()
			n.DeletionTimestamp = &now
			return n
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			assert.True(t, update(mk()), "enqueues")
		})
	}

	assert.True(t, p.Create(event.CreateEvent{Object: old}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: old}))
	assert.True(t, p.Generic(event.GenericEvent{Object: old}))
}

// The Host controller's pins Secret watch enqueues only an update that lifts
// an expired certificate (a re-key); an enrolment, a revocation, a renewal of
// a valid certificate and store position writes wait for the Host's own
// schedule, so a host its Provider does not front is not polled for them.
func TestPinsLiftExpiry(t *testing.T) {
	now := time.Now()
	p := pinsLiftExpiry(func() time.Time { return now })
	withExpiry := func(id string, at time.Time) hostEnrolmentState {
		c := enrolmentFor(strings.Repeat(id, 16), "uid-prov-a", 2, strings.Repeat("aa", 32))
		c.NotAfter = metav1.NewTime(at)
		return hostEnrolmentState{Current: c}
	}
	sec := func(st hostEnrolmentState) *corev1.Secret { return pinsSecretWith(t, st, HostStoreState{}) }
	expired, valid := withExpiry("c3", now.Add(-time.Minute)), withExpiry("d4", now.Add(time.Hour))
	pending := hostEnrolmentState{Pending: &enrolmentRecord{EnrolmentID: strings.Repeat("e5", 16)}}
	for name, tc := range map[string]struct {
		old, new hostEnrolmentState
		want     bool
	}{
		"re-key of an expired certificate":   {expired, valid, true},
		"first enrolment pins a certificate": {pending, valid, false},
		"enrolment steps before the pin":     {hostEnrolmentState{}, pending, false},
		"revocation of an expired host":      {expired, hostEnrolmentState{}, false},
		"revocation of a valid host":         {valid, hostEnrolmentState{}, false},
		"renewal of a valid certificate":     {valid, withExpiry("f6", now.Add(2*time.Hour)), false},
		"still expired":                      {expired, withExpiry("f6", now.Add(-time.Second)), false},
		"nothing changed":                    {valid, valid, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, p.Update(event.UpdateEvent{ObjectOld: sec(tc.old), ObjectNew: sec(tc.new)}))
		})
	}
	undecodable := sec(valid)
	undecodable.Data[hostEnrolmentStateKey] = []byte("{")
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: sec(expired), ObjectNew: undecodable}))
	assert.False(t, p.Create(event.CreateEvent{Object: sec(valid)}))
	assert.False(t, p.Delete(event.DeleteEvent{Object: sec(valid)}))
	assert.False(t, p.Generic(event.GenericEvent{Object: sec(valid)}))
}
