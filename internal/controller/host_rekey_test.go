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
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// enrolmentFor is a pinned enrolment of id for providerUID at start epoch
// epoch, with key pin pin.
func enrolmentFor(id, providerUID string, epoch uint64, pin string) *currentEnrolment {
	return &currentEnrolment{Record: enrolmentRecord{EnrolmentID: id, ProviderUID: providerUID, InstallID: strings.Repeat("42", 16),
		StartEpoch: epoch}, KeyPins: []string{pin}, NotAfter: metav1.NewTime(time.Now().Add(time.Hour))}
}

// TestHostEnrolmentState_Rekey: the next record starts after the current
// enrolment's epoch and after every epoch a record was approved at, and at
// least at the Host's floor; each re-key moves the replaced enrolment to the
// predecessors and its key pins to the previous pins (bounded, oldest
// dropped). A re-key is blocked when it would change the Provider, when the
// predecessor list is full (nothing is dropped from it), and when no epoch
// is left.
func TestHostEnrolmentState_Rekey(t *testing.T) {
	const prov = "prov-uid"
	pin := func(i int) string { return fmt.Sprintf("%064x", i) }
	st := &hostEnrolmentState{}
	e, floored, ok := st.nextStartEpoch(0)
	assert.True(t, ok)
	assert.False(t, floored)
	assert.Equal(t, uint64(1), e, "a first record starts at epoch 1 (no epoch URI)")
	e, floored, _ = st.nextStartEpoch(9)
	assert.True(t, floored)
	assert.Equal(t, uint64(9), e, "the Host's floor raises a fresh Host's first record")
	r, _ := st.rekeyBlocked(prov)
	assert.Empty(t, r, "a first enrolment is never blocked")
	assert.Empty(t, st.rekey(enrolmentFor("e1", prov, 0, pin(1))), "a first enrolment replaces nothing")
	assert.Equal(t, uint64(baseMintEpochCounter), st.Current.mintEpoch())
	e, _, _ = st.nextStartEpoch(0)
	assert.Equal(t, uint64(2), e)
	e, floored, _ = st.nextStartEpoch(9)
	assert.False(t, floored, "the floor is ignored once enrolled")
	assert.Equal(t, uint64(2), e)

	// A record approved at epoch 4 and then refused: the next starts at 5.
	st.LastIssuedEpoch = 4
	e, _, _ = st.nextStartEpoch(0)
	assert.Equal(t, uint64(5), e)
	st.Rekey = &rekeyRequest{Value: "2026-11-15T10:00:00Z", Until: metav1.NewTime(time.Now().Add(time.Hour))}
	assert.Equal(t, "e1", st.rekey(enrolmentFor("e2", prov, 5, pin(2))))
	assert.Equal(t, uint64(5), st.Current.mintEpoch(), "grants are minted at the record's epoch")
	assert.Equal(t, []string{"e1"}, st.PredecessorEnrolmentIDs)
	assert.Equal(t, []string{pin(1)}, st.PreviousKeyPins)
	assert.Nil(t, st.Rekey, "a re-key closes its window")
	assert.Equal(t, "2026-11-15T10:00:00Z", st.LastRekeyRequest, "and its request is used")
	assert.True(t, st.keyPreviouslyPinned(pin(1)))
	assert.True(t, st.keyPreviouslyPinned(pin(2)))
	assert.False(t, st.keyPreviouslyPinned(pin(3)))
	assert.ElementsMatch(t, []string{"e1", "e2"}, st.usedEnrolmentIDs(), "the current and predecessor ids are never drawn")

	for i := 3; i < 3+maxPreviousKeyPins+2; i++ {
		st.rekey(enrolmentFor(fmt.Sprintf("e%d", i), prov, uint64(i+3), pin(i)))
	}
	assert.Len(t, st.PreviousKeyPins, maxPreviousKeyPins, "previous pins are bounded")
	assert.NotContains(t, st.PreviousKeyPins, pin(1), "the oldest is dropped")

	r, _ = st.rekeyBlocked("another-provider")
	assert.Equal(t, reasonEnrolProviderChanged, r, "a re-key never changes the Provider")
	r, msg := st.rekeyBlocked(prov)
	assert.Equal(t, reasonEnrolTooManyPredecessors, r)
	assert.Contains(t, msg, "predecessor")

	exhausted := &hostEnrolmentState{Current: enrolmentFor("x", prov, hostidentity.MaxStartEpoch, pin(1))}
	r, _ = exhausted.rekeyBlocked(prov)
	assert.Equal(t, reasonEnrolEpochExhausted, r)
	_, _, ok = exhausted.nextStartEpoch(0)
	assert.False(t, ok, "no record past the last epoch")

	// A state written before start epochs were recorded reads as epoch 1.
	assert.Equal(t, uint64(baseMintEpochCounter), (&currentEnrolment{}).mintEpoch())
	assert.Equal(t, uint64(baseMintEpochCounter), (*currentEnrolment)(nil).mintEpoch())
}

// TestStartEpochFloor: the floor annotation is a number from 1 to
// maxStartEpochFloor; anything else is ignored and reported invalid. It
// applies only to a fresh state.
func TestStartEpochFloor(t *testing.T) {
	h := &infravirtrigaudiov1beta1.Host{}
	f, invalid := startEpochFloor(h)
	assert.Zero(t, f)
	assert.False(t, invalid)
	for v, want := range map[string]uint64{"7": 7, " 12 ": 12, "65536": 65536, "0": 0, "-3": 0, "x": 0, "65537": 0, "4294967296": 0} {
		h.Annotations = map[string]string{hostidentity.AnnotationStartEpochFloor: v}
		f, invalid := startEpochFloor(h)
		assert.Equal(t, want, f, v)
		assert.Equal(t, want == 0, invalid, v)
	}
	approved := &hostEnrolmentState{LastIssuedEpoch: 1}
	assert.False(t, approved.fresh(), "a record was approved: not fresh")
	e, floored, _ := approved.nextStartEpoch(500)
	assert.False(t, floored)
	assert.Equal(t, uint64(2), e)
}

// TestPinsHostTrust_RekeyedEnrolment: after a re-key, the host's trust names
// the new enrolment under its epoch counter and lists the predecessors; an
// intent under a predecessor is a predecessor intent (asked about, never
// re-sent), one under the current enrolment is accepted, and any other is
// tampered. The store positions survive the re-key.
func TestPinsHostTrust_RekeyedEnrolment(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	ca := hostidentitytest.NewCA(t)
	prov := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default",
		UID: identityProviderUID}, Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor}}
	h := hostCR("host-a", "ch", nil)
	h.UID = "uid-host-a"
	h.Spec.Identity = "serial-1"
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(prov, h).Build()
	install := bytes.Repeat([]byte{0x42}, 16)
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", TrustDomain: "cluster.test", CABundle: ca.PEM, InstallID: install}
	src := &PinsHostTrust{Client: cli, Config: cfg, Epoch: operation.Epoch{Counter: baseMintEpochCounter, Holder: install}}

	first, second := strings.Repeat("a1", 16), strings.Repeat("b2", 16)
	l := loadedEnrolment{state: hostEnrolmentState{HostUID: string(h.UID), Identity: h.Spec.Identity}}
	l.state.rekey(enrolmentFor(first, string(identityProviderUID), 0, strings.Repeat("ab", 32)))
	require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l))
	rec := hostidentity.StoreRecord{StoreID: [16]byte{1}, EnrolmentID: bytes.Repeat([]byte{0xa1}, 16), Seq: 9, Head: [32]byte{9}}
	require.NoError(t, src.UpdateStoreState(ctx, prov, "host-a", func(st *HostStoreState) { st.Current = rec }))

	l, err := loadHostEnrolment(ctx, cli, cfg, h)
	require.NoError(t, err)
	assert.Equal(t, first, l.state.rekey(enrolmentFor(second, string(identityProviderUID), 2, strings.Repeat("cd", 32))))
	require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l))

	trust, err := src.HostTrust(ctx, prov, "host-a")
	require.NoError(t, err)
	assert.Equal(t, second, hex.EncodeToString(trust.Enrolment.EnrolmentID))
	assert.Equal(t, uint64(2), trust.Enrolment.Epoch.Counter, "grants of the new enrolment carry the next epoch counter")
	require.Len(t, trust.Predecessors, 1)
	assert.Equal(t, first, hex.EncodeToString(trust.Predecessors[0]))
	st, err := src.LoadStoreState(ctx, prov, "host-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(9), st.Current.Seq, "the store positions survive a re-key")

	r := &VirtualMachineReconciler{HostAnswers: &HostAnswers{Trust: src}}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{UID: "uid-vm"}}
	ia := identityAttempt{providerCR: prov}
	opID := bytes.Repeat([]byte{7}, 16)
	binding := bytes.Repeat([]byte{8}, 16)
	a := &infravirtrigaudiov1beta1.OperationAttempt{Kind: attemptKindOf(contracts.OperationKindPowerOn), Attempt: 1,
		OpID: hex.EncodeToString(opID), HostID: "host-a", BindingID: hex.EncodeToString(binding)}
	id := func(enrol string, counter uint64) contracts.OperationAttemptIdentity {
		e, _ := hex.DecodeString(enrol)
		return contracts.OperationAttemptIdentity{ProviderUID: string(identityProviderUID), HostID: "host-a", EnrolmentID: e,
			Epoch: contracts.OperationEpoch{Counter: counter, Holder: install}, VMUID: "uid-vm", BindingID: binding, OpID: opID, Attempt: 1}
	}
	check := func(i contracts.OperationAttemptIdentity) error {
		return r.checkIntentIdentity(ctx, vm, ia, a, contracts.OperationKindPowerOn, i)
	}
	assert.True(t, errors.Is(check(id(first, 1)), errIntentPredecessor), "an attempt under the replaced enrolment is a predecessor's")
	assert.NoError(t, check(id(second, 2)), "an attempt under the new enrolment and its epoch")
	assert.True(t, errors.Is(check(id(second, 1)), errIntentTampered), "the new enrolment under the old epoch counter is not its own")
	assert.True(t, errors.Is(check(id(strings.Repeat("c3", 16), 2)), errIntentTampered), "an enrolment the host never had")

	// Revoked after the re-key: no enrolment is trusted any more; every
	// attempt, a predecessor's too, is held (never sent, never closed as
	// performed or not performed).
	h.Spec.Revoked = true
	require.NoError(t, cli.Update(ctx, h))
	_, err = src.HostTrust(ctx, prov, "host-a")
	require.ErrorIs(t, err, operation.ErrNoEnrolment)
	for _, i := range []contracts.OperationAttemptIdentity{id(first, 1), id(second, 2)} {
		err := check(i)
		require.Error(t, err)
		assert.False(t, errors.Is(err, errIntentPredecessor), "a revoked Host's attempts are not polled as predecessors")
		assert.True(t, errors.Is(err, errIntentTampered), "held")
	}
}
