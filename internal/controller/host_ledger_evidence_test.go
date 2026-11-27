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
	"crypto/rand"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// testLedgerOrigin is the store-origin evidence a stand-in agent signs.
type testLedgerOrigin struct {
	OriginEnrolmentID []byte
	EnrolmentCount    uint32
}

// testLedgerContinuity is a continuity confirmation a stand-in agent signs:
// the request named Seq and the agent signed Head there.
type testLedgerContinuity struct {
	Seq  uint64
	Head [32]byte
}

// testLedgerInit is what a stand-in agent signs into the host answer a
// test's ledger start is decided on.
type testLedgerInit struct {
	StoreID              [16]byte
	Position, Checkpoint HostLedgerPosition
	Origin               *testLedgerOrigin
	Continuity           *testLedgerContinuity
	Attestation          hostidentity.StoreAttestation
}

// signedLedgerEvidence signs a host answer with what in names, as an agent
// awaiting attestation would for a request with ledger version 2, verifies
// it with the answer verifier and returns its ledger evidence.
func signedLedgerEvidence(t ledgerTestT, provider *infravirtrigaudiov1beta1.Provider, hostID string, enrolment, _ []byte,
	now time.Time, in testLedgerInit) hostidentity.LedgerEvidence {
	t.Helper()
	h := newTestHost(t, provider, hostID, enrolment, operation.Epoch{Counter: 1, Holder: bytes.Repeat([]byte{1}, 16)})
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	pos := append(append(append([]byte(nil), in.StoreID[:]...), binary.BigEndian.AppendUint64(nil, in.Position.Seq)...), in.Position.Head[:]...)
	c := &providerv1.AnswerContext{Nonce: nonce, ProviderUid: string(provider.UID), HostId: hostID, InstallId: h.trust.InstallID,
		EnrolmentId: enrolment, ChainPosition: pos, AgentState: providerv1.AgentState_AGENT_STATE_UNATTESTED, AgentTime: timestamppb.Now(),
		LedgerVersionEcho: hostidentity.LedgerVersion}
	if in.Checkpoint.Seq > 0 {
		c.CheckpointSeq, c.CheckpointHead = in.Checkpoint.Seq, in.Checkpoint.Head[:]
	}
	var continuitySeq uint64
	if in.Continuity != nil {
		continuitySeq = in.Continuity.Seq
		c.ContinuityHead = bytes.Clone(in.Continuity.Head[:])
	}
	l := &providerv1.HostLedgerState{RequestedVersion: hostidentity.LedgerVersion, Version: 1,
		State: providerv1.LedgerSessionState_LEDGER_SESSION_STATE_AWAITING_ATTESTATION}
	if in.Origin != nil {
		l.StoreOriginEnrolmentId, l.StoreEnrolmentCount = bytes.Clone(in.Origin.OriginEnrolmentID), in.Origin.EnrolmentCount
	}
	if a := in.Attestation; a.Reported {
		l.Version = hostidentity.StoreAttestedLedgerVersion
		if a.EverAttested {
			l.StoreEverAttested, l.StoreFirstAttestedSeq, l.StoreAttestedRecordSeq = true, a.FirstSeq, a.RecordSeq
			l.StoreFirstAttestedEnrolmentId = bytes.Clone(a.FirstEnrolmentID)
		}
	}
	body := &providerv1.HostAnswer{Context: c, AgentApiVersion: 6, GrantVersions: []uint32{2},
		Mode: providerv1.AgentMode_AGENT_MODE_IDENTITY, Ledger: l}
	exp := h.trust.expectation(provider, hostID, hostidentity.BodyTypeHostAnswer, nonce, now)
	exp.LedgerVersion, exp.ContinuitySeq = hostidentity.LedgerVersion, continuitySeq
	v, err := hostidentity.VerifyAnswer(blobOf(h.signRaw(t, body)), exp)
	require.NoError(t, err)
	ev, err := v.LedgerEvidence()
	require.NoError(t, err)
	return ev
}

// checkEvidence refuses evidence of another store, of an earlier request
// than the host's latest observed ledger answer, of another enrolment, and
// evidence that is too old or was not built by the verifier; it accepts the
// latest evidence of the store.
func TestHostLedger_CheckEvidence(t *testing.T) {
	provider := &infravirtrigaudiov1beta1.Provider{}
	provider.Namespace, provider.Name, provider.UID = "ns", "p", "0b7a1c3e-1111-4222-8333-944455556666"
	enrol := bytes.Repeat([]byte{7}, 16)
	now := time.Now().UTC().Truncate(time.Second)
	l := &HostLedger{Now: func() time.Time { return now }}
	lh := &ledgerHost{provider: provider, hostID: "h", key: hostAnswerKey(provider, "h"), enrolment: enrol}
	var store, other [16]byte
	store[0], other[0] = 1, 2
	in := testLedgerInit{StoreID: store, Position: HostLedgerPosition{Seq: 3, Head: [32]byte{1}},
		Attestation: hostidentity.StoreAttestation{Reported: true}}

	old := signedLedgerEvidence(t, provider, "h", enrol, nil, now, in)
	l.Observe(provider, "h", old)
	_, err := l.checkEvidence(lh, store, old)
	require.NoError(t, err, "the latest evidence of the store is accepted")

	_, err = l.checkEvidence(lh, other, old)
	require.ErrorIs(t, err, errHostLedgerEvidence, "evidence of another store")

	latest := signedLedgerEvidence(t, provider, "h", enrol, nil, now, in)
	l.Observe(provider, "h", latest)
	_, err = l.checkEvidence(lh, store, old)
	require.ErrorIs(t, err, errHostLedgerEvidence, "evidence of an older nonce")
	_, err = l.checkEvidence(lh, store, latest)
	require.NoError(t, err)

	foreign := signedLedgerEvidence(t, provider, "h", bytes.Repeat([]byte{8}, 16), nil, now, in)
	l.Observe(provider, "h", foreign)
	_, err = l.checkEvidence(lh, store, foreign)
	require.ErrorIs(t, err, errHostLedgerEvidence, "evidence under another enrolment")

	l.Observe(provider, "h", latest)
	now = now.Add(ledgerEvidenceTTL + time.Second)
	_, err = l.checkEvidence(lh, store, latest)
	require.ErrorIs(t, err, errHostLedgerEvidence, "evidence too old")

	_, err = l.checkEvidence(lh, store, hostidentity.LedgerEvidence{})
	require.ErrorIs(t, err, errHostLedgerEvidence, "evidence not built by the verifier")
}
