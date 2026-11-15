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

// enrolmentFor is a pinned enrolment of id for providerUID.
func enrolmentFor(id, providerUID string) *currentEnrolment {
	return &currentEnrolment{Record: enrolmentRecord{EnrolmentID: id, ProviderUID: providerUID, InstallID: strings.Repeat("42", 16)},
		KeyPins: []string{strings.Repeat("ab", 32)}, NotAfter: metav1.NewTime(time.Now().Add(time.Hour))}
}

// TestHostEnrolmentState_Rekey: a first enrolment issues grants under the
// base epoch counter; each re-key moves the replaced enrolment to the
// predecessors and issues the new one's grants under the next counter. A
// re-key is blocked when it would change the Provider or when the
// predecessor list is full; nothing is dropped from it.
func TestHostEnrolmentState_Rekey(t *testing.T) {
	const prov = "prov-uid"
	st := &hostEnrolmentState{}
	r, _ := st.rekeyBlocked(prov)
	assert.Empty(t, r, "a first enrolment is never blocked")
	assert.Empty(t, st.rekey(enrolmentFor("e1", prov)), "a first enrolment replaces nothing")
	assert.Equal(t, uint64(baseMintEpochCounter), st.Current.mintEpoch())
	assert.Empty(t, st.PredecessorEnrolmentIDs)

	assert.Equal(t, "e1", st.rekey(enrolmentFor("e2", prov)))
	assert.Equal(t, uint64(2), st.Current.mintEpoch())
	assert.Equal(t, []string{"e1"}, st.PredecessorEnrolmentIDs)
	assert.Equal(t, "e2", st.rekey(enrolmentFor("e3", prov)))
	assert.Equal(t, uint64(3), st.Current.mintEpoch())
	assert.Equal(t, []string{"e1", "e2"}, st.PredecessorEnrolmentIDs, "oldest first")

	r, _ = st.rekeyBlocked("another-provider")
	assert.Equal(t, reasonEnrolProviderChanged, r, "a re-key never changes the Provider")

	for i := len(st.PredecessorEnrolmentIDs); i < maxPredecessorEnrolmentIDs; i++ {
		st.PredecessorEnrolmentIDs = append(st.PredecessorEnrolmentIDs, strings.Repeat("c", i+1))
	}
	r, msg := st.rekeyBlocked(prov)
	assert.Equal(t, reasonEnrolTooManyPredecessors, r)
	assert.Contains(t, msg, "predecessor")

	// A state written before re-keys existed (no mint epoch) reads as the
	// base counter.
	assert.Equal(t, uint64(baseMintEpochCounter), (&currentEnrolment{}).mintEpoch())
	assert.Equal(t, uint64(baseMintEpochCounter), (*currentEnrolment)(nil).mintEpoch())
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
	l.state.rekey(enrolmentFor(first, string(identityProviderUID)))
	require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l))
	rec := hostidentity.StoreRecord{StoreID: [16]byte{1}, EnrolmentID: bytes.Repeat([]byte{0xa1}, 16), Seq: 9, Head: [32]byte{9}}
	require.NoError(t, src.UpdateStoreState(ctx, prov, "host-a", func(st *HostStoreState) { st.Current = rec }))

	l, err := loadHostEnrolment(ctx, cli, cfg, h)
	require.NoError(t, err)
	assert.Equal(t, first, l.state.rekey(enrolmentFor(second, string(identityProviderUID))))
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
}
