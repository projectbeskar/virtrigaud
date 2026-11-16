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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
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
	vm.UID = types.UID("uid-" + name)
	vm.Status.OperationAttempts = []infravirtrigaudiov1beta1.OperationAttempt{{Kind: infravirtrigaudiov1beta1.OperationAttemptKindPowerOn,
		Attempt: 1, OpID: strings.Repeat("0a", 16), HostID: host, Phase: phase}}
	return vm
}

// countingReader counts the List calls made through it.
type countingReader struct {
	client.Reader
	lists atomic.Int32
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists.Add(1)
	return c.Reader.List(ctx, list, opts...)
}

// freezeEnv is a HostEnrolmentReconciler over a fake client holding objs,
// with the provider cache indexes when indexed, and a clock at now.
func freezeEnv(t *testing.T, now time.Time, indexed bool, auth operation.IntentAuthenticator,
	objs ...client.Object) (*HostEnrolmentReconciler, *record.FakeRecorder, *countingReader) {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(coverageTestScheme(t)).WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}).
		WithObjects(objs...)
	if indexed {
		b = b.WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, specProviderIndex, specProviderIndexValue).
			WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue)
	}
	c := b.Build()
	live := &countingReader{Reader: c}
	rec := record.NewFakeRecorder(16)
	return &HostEnrolmentReconciler{Client: c, APIReader: live, Recorder: rec, IntentAuth: auth, Now: func() time.Time { return now }},
		rec, live
}

// TestConfirmFreezes: a predecessor's freeze is confirmed only after its
// quiet period (plus the clock skew margin) and only while no attempt blocks
// it: an attempt not Terminal on the Host, of a VM of the Host's Provider
// (named in its spec, in another namespace, or only bound to it), or a
// Terminal Delete of a VM still being deleted. Without an intent key every
// such attempt blocks every predecessor. It is rendered for the agent with
// the current enrolment, and a revoked Host confirms nothing.
func TestConfirmFreezes(t *testing.T) {
	ctx := context.Background()
	prev := strings.Repeat("b2", 16)
	now := time.Now()
	past := now.Add(-time.Hour)

	// Within the quiet period (and its skew margin): waiting.
	r, _, _ := freezeEnv(t, now, false, nil)
	h, st := freezeHost(prev, now.Add(-predecessorQuietPeriod-predecessorClockSkew/2))
	changed, recheck, err := r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Positive(t, recheck)
	assert.Nil(t, freezeConfirmationOf(st))

	otherNS := vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent)
	otherNS.Namespace, otherNS.Spec.ProviderRef.Namespace = "tenant", "default"
	boundOnly := vmWithAttempt("vm-1", "host-a", "elsewhere", infravirtrigaudiov1beta1.OperationAttemptPhaseSent)
	boundOnly.Status.BoundProvider = &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: "default", Name: "ch"}
	deleting := vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal)
	deleting.Status.OperationAttempts[0].Kind = infravirtrigaudiov1beta1.OperationAttemptKindDelete
	deleting.Finalizers = []string{infravirtrigaudiov1beta1.VirtualMachineFinalizer}
	deleting.DeletionTimestamp = &metav1.Time{Time: past}
	for name, c := range map[string]struct {
		vm      *infravirtrigaudiov1beta1.VirtualMachine
		blocked bool
	}{
		"open on the host":                 {vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent), true},
		"prepared on the host":             {vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhasePrepared), true},
		"another namespace, same Provider": {otherNS, true},
		"bound to the Provider only":       {boundOnly, true},
		"deleting with a Terminal Delete":  {deleting, true},
		"terminal on the host":             {vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal), false},
		"open on another host":             {vmWithAttempt("vm-1", "host-b", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent), false},
		"open of another Provider":         {vmWithAttempt("vm-1", "host-a", "other", infravirtrigaudiov1beta1.OperationAttemptPhaseSent), false},
	} {
		t.Run(name, func(t *testing.T) {
			r, rec, _ := freezeEnv(t, now, false, nil, c.vm)
			h, st := freezeHost(prev, past)
			changed, recheck, err := r.confirmFreezes(ctx, h, st)
			require.NoError(t, err)
			assert.Equal(t, !c.blocked, changed)
			assert.Equal(t, c.blocked, recheck > 0)
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

	r, _, _ = freezeEnv(t, now, false, nil)
	h, st = freezeHost(prev, past)
	h.Spec.Revoked = true
	changed, _, err = r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.False(t, changed, "a revoked Host confirms nothing")

	// A predecessor recorded before times were kept starts its quiet period
	// now.
	r, _, _ = freezeEnv(t, now, false, nil)
	h, st = freezeHost(prev, now)
	st.PredecessorSince = nil
	changed, recheck, err = r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.True(t, changed, "its time is stamped")
	assert.Positive(t, recheck)
	assert.Empty(t, st.ConfirmedFrozenEnrolmentIDs)
	assert.Contains(t, st.PredecessorSince, prev)

	assert.True(t, st.discardPredecessor(prev))
	assert.Empty(t, st.PredecessorEnrolmentIDs)
	assert.Empty(t, st.ConfirmedFrozenEnrolmentIDs)
	assert.Equal(t, []string{prev}, st.DiscardedEnrolmentIDs)
	assert.Nil(t, freezeConfirmationOf(st))
	assert.False(t, st.discardPredecessor(prev))
}

// TestConfirmFreezes_AttemptEnrolmentFromIntent: with the intent key, only an
// attempt whose authenticated intent names the predecessor blocks it; an
// attempt under the current enrolment does not, and one whose intent is
// missing or does not authenticate blocks every predecessor.
func TestConfirmFreezes_AttemptEnrolmentFromIntent(t *testing.T) {
	ctx := context.Background()
	prev := strings.Repeat("b2", 16)
	now := time.Now()
	key := make([]byte, 32)
	key[0] = 7
	auth, err := operation.NewIntentAuthenticator(key)
	require.NoError(t, err)
	provider := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default", UID: identityProviderUID}}
	intentFor := func(vm *infravirtrigaudiov1beta1.VirtualMachine, enrolment string, sealWith operation.IntentAuthenticator) *corev1.Secret {
		e, _ := hex.DecodeString(enrolment)
		op, _ := hex.DecodeString(vm.Status.OperationAttempts[0].OpID)
		p := contracts.PreparedOperation{Kind: contracts.OperationKindPowerOn, Identity: contracts.OperationAttemptIdentity{
			ProviderUID: string(identityProviderUID), HostID: "host-a", EnrolmentID: e, VMUID: string(vm.UID), OpID: op, Attempt: 1},
			RequestBytes: []byte("r"), DigestSalt: bytes.Repeat([]byte{1}, 32)}
		s, err := buildIntentSecret(provider, vm, p, coverageTestScheme(t), sealWith)
		require.NoError(t, err)
		return s
	}
	h, st := freezeHost(prev, now.Add(-time.Hour))
	cur := st.Current.Record.EnrolmentID
	other, err := operation.NewIntentAuthenticator(bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	for name, c := range map[string]struct {
		enrolment string
		seal      operation.IntentAuthenticator
		noIntent  bool
		blocked   bool
	}{
		"under the current enrolment":       {enrolment: cur, seal: auth, blocked: false},
		"under the predecessor":             {enrolment: prev, seal: auth, blocked: true},
		"intent that does not authenticate": {enrolment: cur, seal: other, blocked: true},
		"no intent":                         {noIntent: true, blocked: true},
	} {
		t.Run(name, func(t *testing.T) {
			vm := vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent)
			objs := []client.Object{vm}
			if !c.noIntent {
				objs = append(objs, intentFor(vm, c.enrolment, c.seal))
			}
			r, _, _ := freezeEnv(t, now, false, auth, objs...)
			_, st := freezeHost(prev, now.Add(-time.Hour))
			changed, _, err := r.confirmFreezes(ctx, h, st)
			require.NoError(t, err)
			assert.Equal(t, !c.blocked, changed)
		})
	}
}

// TestConfirmFreezes_CacheFirstAndBackoff: when the cache already shows the
// predecessor blocked, the API server is not read; the recheck backs off
// from one minute, doubling, up to ten; once free, the API server confirms
// it and the backoff resets.
func TestConfirmFreezes_CacheFirstAndBackoff(t *testing.T) {
	ctx := context.Background()
	prev := strings.Repeat("b2", 16)
	now := time.Now()
	vm := vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent)
	r, _, live := freezeEnv(t, now, true, nil, vm)
	h, st := freezeHost(prev, now.Add(-time.Hour))
	var got []time.Duration
	for i := 0; i < 6; i++ {
		_, recheck, err := r.confirmFreezes(ctx, h, st)
		require.NoError(t, err)
		got = append(got, recheck)
	}
	assert.Equal(t, []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}, got)
	assert.Zero(t, live.lists.Load(), "the cache showed it blocked: no read of the API server")

	vm.Status.OperationAttempts[0].Phase = infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal
	require.NoError(t, r.Client.Status().Update(ctx, vm))
	changed, recheck, err := r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Zero(t, recheck)
	assert.Positive(t, live.lists.Load(), "confirmed from the API server")
	_, ok := r.freezeWaits.Load(h.Namespace + "/" + h.Name)
	assert.False(t, ok, "the backoff is reset")
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

// rekeyingTrust is a staticTrust whose host is re-keyed once rekeyed is set.
type rekeyingTrust struct {
	*staticTrust
	rekeyed atomic.Bool
	next    HostTrust
}

func (r *rekeyingTrust) HostTrust(ctx context.Context, p *infravirtrigaudiov1beta1.Provider, hostID string) (HostTrust, error) {
	if r.rekeyed.Load() {
		return r.next, nil
	}
	return r.staticTrust.HostTrust(ctx, p, hostID)
}

// TestIdentityCreate_RekeyBeforeFirstSendIsNotSent: an attempt minted under
// an enrolment that the host replaced before its first send is never sent;
// the VM is held, with no outcome concluded.
func TestIdentityCreate_RekeyBeforeFirstSendIsNotSent(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	st, ok := r.HostAnswers.Trust.(*staticTrust)
	require.True(t, ok)
	cur, err := st.HostTrust(context.Background(), nil, "host-alpha")
	require.NoError(t, err)
	next := cur
	next.Enrolment.EnrolmentID = bytes.Repeat([]byte{0x77}, 16)
	next.Predecessors = [][]byte{cur.Enrolment.EnrolmentID}
	rt := &rekeyingTrust{staticTrust: st, next: next}
	r.HostAnswers.Trust = rt
	// The host is re-keyed after the attempt's marker is written and its
	// position persisted, before its first send.
	r.operationCrashHook = func(_ context.Context, p operationCrashPoint) bool {
		if p == crashAfterPrepared {
			rt.rekeyed.Store(true)
		}
		return false
	}
	stub.onSend = func(contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		t.Fatal("an attempt minted under the replaced enrolment was sent")
		return contracts.OperationAnswer{}, nil
	}
	reconcileN(t, r, req, 1)
	assert.Empty(t, stub.sent())
	vm := idVM(t, r)
	assert.Equal(t, k8s.ReasonOperationHostRekeyed, readyReason(vm))
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, a.Phase, "no outcome is concluded")
}
