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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// freezeHost is an enrolled Host of Provider ch with predecessor prev,
// replaced since.
func freezeHost(prev string, since time.Time) (*infravirtrigaudiov1beta1.Host, *hostEnrolmentState) {
	h := hostCR("host-a", "ch", nil)
	h.UID = "uid-host-a"
	h.Spec.Identity = "serial-1"
	st := &hostEnrolmentState{HostUID: string(h.UID), Identity: h.Spec.Identity,
		Current:                 enrolmentFor(strings.Repeat("c3", 16), string(identityProviderUID), 2, strings.Repeat("ab", 32)),
		PredecessorEnrolmentIDs: []string{prev}, PredecessorSince: map[string]metav1.Time{prev: metav1.NewTime(since)}}
	return h, st
}

// vmWithAttempt is a VirtualMachine of Provider ch with one attempt on host
// in phase.
func vmWithAttempt(name, host, provider string, phase infravirtrigaudiov1beta1.OperationAttemptPhase) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := clusterVM(name, "default", provider)
	vm.Status.OperationAttempts = []infravirtrigaudiov1beta1.OperationAttempt{{Kind: infravirtrigaudiov1beta1.OperationAttemptKindPowerOn,
		Attempt: 1, OpID: strings.Repeat("0a", 16), HostID: host, Phase: phase}}
	return vm
}

// TestConfirmFreezes: a predecessor's freeze is confirmed only after its
// quiet period and only while no VM of the Host's Provider has an open
// attempt on the Host; an open attempt on another host or of another
// Provider, or a Terminal one, does not block it. It is rendered for the
// agent with the current enrolment, and a revoked Host confirms nothing.
func TestConfirmFreezes(t *testing.T) {
	ctx := context.Background()
	prev := strings.Repeat("b2", 16)
	now := time.Now()
	newR := func(objs ...*infravirtrigaudiov1beta1.VirtualMachine) (*HostEnrolmentReconciler, *record.FakeRecorder) {
		b := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{})
		for _, o := range objs {
			b = b.WithObjects(o)
		}
		rec := record.NewFakeRecorder(16)
		return &HostEnrolmentReconciler{Client: b.Build(), Recorder: rec, Now: func() time.Time { return now }}, rec
	}

	// Within the quiet period: waiting.
	r, _ := newR()
	h, st := freezeHost(prev, now.Add(-time.Minute))
	changed, waiting, err := r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.True(t, waiting)
	assert.Nil(t, freezeConfirmationOf(st))

	// An open attempt on the Host blocks it; on another host, of another
	// Provider, or Terminal, it does not.
	for name, c := range map[string]struct {
		vm      *infravirtrigaudiov1beta1.VirtualMachine
		blocked bool
	}{
		"open on the host":         {vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent), true},
		"prepared on the host":     {vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhasePrepared), true},
		"terminal on the host":     {vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal), false},
		"open on another host":     {vmWithAttempt("vm-1", "host-b", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent), false},
		"open of another Provider": {vmWithAttempt("vm-1", "host-a", "other", infravirtrigaudiov1beta1.OperationAttemptPhaseSent), false},
	} {
		t.Run(name, func(t *testing.T) {
			r, rec := newR(c.vm)
			h, st := freezeHost(prev, now.Add(-time.Hour))
			changed, waiting, err := r.confirmFreezes(ctx, h, st)
			require.NoError(t, err)
			assert.Equal(t, !c.blocked, changed)
			assert.Equal(t, c.blocked, waiting)
			if c.blocked {
				assert.Empty(t, st.ConfirmedFrozenEnrolmentIDs)
				return
			}
			assert.Equal(t, []string{prev}, st.ConfirmedFrozenEnrolmentIDs)
			f := freezeConfirmationOf(st)
			require.NotNil(t, f)
			assert.Equal(t, st.Current.Record.EnrolmentID, f.ForEnrolmentID)
			assert.Equal(t, []string{prev}, f.EnrolmentIDs)
			require.Len(t, rec.Events, 1)
			ev := <-rec.Events
			assert.Contains(t, ev, eventReasonAgentFreezeConfirmed)
			assert.Contains(t, ev, prev)
		})
	}

	r, _ = newR()
	h, st = freezeHost(prev, now.Add(-time.Hour))
	h.Spec.Revoked = true
	changed, _, err = r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.False(t, changed, "a revoked Host confirms nothing")

	// A predecessor recorded before timestamps existed has no quiet period.
	r, _ = newR()
	h, st = freezeHost(prev, now)
	st.PredecessorSince = nil
	changed, _, err = r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.True(t, changed)

	assert.True(t, st.discardPredecessor(prev))
	assert.Empty(t, st.PredecessorEnrolmentIDs)
	assert.Empty(t, st.ConfirmedFrozenEnrolmentIDs)
	assert.Nil(t, freezeConfirmationOf(st))
	assert.False(t, st.discardPredecessor(prev))
}

// freezeStaticTrust is a staticTrust whose Host holds confirmed predecessors.
type freezeStaticTrust struct {
	*staticTrust
	fmu       sync.Mutex
	confirmed [][]byte
	discarded [][]byte
}

func (f *freezeStaticTrust) ConfirmedFrozen(context.Context, *infravirtrigaudiov1beta1.Provider, string) ([][]byte, error) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return slices.Clone(f.confirmed), nil
}

func (f *freezeStaticTrust) DiscardPredecessor(_ context.Context, _ *infravirtrigaudiov1beta1.Provider, _ string, id []byte) (bool, error) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	i := slices.IndexFunc(f.confirmed, func(b []byte) bool { return bytes.Equal(b, id) })
	if i < 0 {
		return false, nil
	}
	f.confirmed = slices.Delete(f.confirmed, i, i+1)
	f.discarded = append(f.discarded, id)
	return true, nil
}

// opReader is a HostAnswerReader that answers op reads with a signed host
// stub.
type opReader struct{ s *signedHostStub }

func (o opReader) OperationAnswer(_ context.Context, q contracts.OperationAnswerQuery) (contracts.TaskStatus, contracts.SignedRead, error) {
	return o.s.operationAnswer(q, nil)
}

func (o opReader) VMAnswer(_ context.Context, q contracts.VMAnswerQuery) (contracts.DescribeResponse, contracts.SignedRead, error) {
	return o.s.vmAnswer(q)
}

func (o opReader) HostAnswer(_ context.Context, q contracts.HostAnswerQuery) (contracts.HostInfo, contracts.SignedRead, error) {
	return o.s.hostAnswer(q)
}

// TestCheckFreezeDiscards: a confirmed predecessor is removed only on a
// verified signed answer that it is UNKNOWN_TO_AGENT (discarded); while the
// host still holds it (NO_RECORD), when the answer is unsigned, or when it
// is signed for another op_id or enrolment, it stays.
func TestCheckFreezeDiscards(t *testing.T) {
	ctx := context.Background()
	prov := withRuntime(clusteredProviderCR("prov-cluster", clusteredNS))
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	prov.UID = identityProviderUID
	holder := bytes.Repeat([]byte{1}, operation.IDLen)
	h := newTestHost(t, prov, "host-alpha", bytes.Repeat([]byte{0x44}, 16), operation.Epoch{Counter: 2, Holder: holder})
	prev := bytes.Repeat([]byte{0x33}, 16)
	stub := newSignedHostStub(t, h)
	trust := &freezeStaticTrust{staticTrust: &staticTrust{hosts: map[string]*testHost{"host-alpha": h}}, confirmed: [][]byte{prev}}
	answers := &HostAnswers{Trust: trust}

	// Still held by the host: NO_RECORD, kept.
	stub.predecessors = [][]byte{prev}
	assert.Empty(t, answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha"))
	// Unsigned (a Provider that strips the answer): kept.
	stub.predecessors, stub.unsigned = nil, true
	assert.Empty(t, answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha"))
	// Signed about another op_id or another enrolment: kept.
	stub.unsigned = false
	stub.editAnswer = func(a *providerv1.OperationAnswer) { a.OpId = bytes.Repeat([]byte{9}, 16) }
	assert.Empty(t, answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha"))
	stub.editAnswer = func(a *providerv1.OperationAnswer) { a.OpEnrolmentId = bytes.Repeat([]byte{8}, 16) }
	assert.Empty(t, answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha"))
	assert.Empty(t, trust.discarded)
	// Discarded by the host: removed.
	stub.editAnswer = nil
	removed := answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha")
	assert.Equal(t, []string{hex.EncodeToString(prev)}, removed)
	assert.Equal(t, [][]byte{prev}, trust.discarded)
	assert.Empty(t, answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha"), "nothing left to check")
}
