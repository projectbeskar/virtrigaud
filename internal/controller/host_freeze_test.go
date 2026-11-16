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
	ctrl "sigs.k8s.io/controller-runtime"
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
	gets  atomic.Int32
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets.Add(1)
	return c.Reader.Get(ctx, key, obj, opts...)
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
	redactedFor := func(vm *infravirtrigaudiov1beta1.VirtualMachine, enrolment string) *corev1.Secret {
		e, _ := hex.DecodeString(enrolment)
		op, _ := hex.DecodeString(vm.Status.OperationAttempts[0].OpID)
		p := contracts.PreparedOperation{Kind: contracts.OperationKindPowerOn, Identity: contracts.OperationAttemptIdentity{
			ProviderUID: string(identityProviderUID), HostID: "host-a", EnrolmentID: e, VMUID: string(vm.UID), OpID: op, Attempt: 1},
			RequestBytes: []byte("r"), DigestSalt: bytes.Repeat([]byte{1}, 32)}
		s, err := buildRedactedIntentSecret(provider, vm, p, coverageTestScheme(t), auth)
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
		redacted  bool
		blocked   bool
	}{
		"redacted intent under the current enrolment": {enrolment: cur, redacted: true, blocked: false},
		"redacted intent under the predecessor":       {enrolment: prev, redacted: true, blocked: true},
		"under the current enrolment":                 {enrolment: cur, seal: auth, blocked: false},
		"under the predecessor":                       {enrolment: prev, seal: auth, blocked: true},
		"intent that does not authenticate":           {enrolment: cur, seal: other, blocked: true},
		"no intent":                                   {noIntent: true, blocked: true},
	} {
		t.Run(name, func(t *testing.T) {
			vm := vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent)
			objs := []client.Object{vm}
			switch {
			case c.redacted:
				objs = append(objs, redactedFor(vm, c.enrolment))
			case !c.noIntent:
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

// TestConfirmFreezes_IntentReadOncePerCheck: an attempt whose intent the
// cache pass authenticated is not read again by the API-server pass of the
// same check.
func TestConfirmFreezes_IntentReadOncePerCheck(t *testing.T) {
	ctx := context.Background()
	prev := strings.Repeat("b2", 16)
	now := time.Now()
	auth, err := operation.NewIntentAuthenticator(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	provider := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default", UID: identityProviderUID}}
	h, st := freezeHost(prev, now.Add(-time.Hour))
	vm := vmWithAttempt("vm-1", "host-a", "ch", infravirtrigaudiov1beta1.OperationAttemptPhaseSent)
	e, _ := hex.DecodeString(st.Current.Record.EnrolmentID)
	op, _ := hex.DecodeString(vm.Status.OperationAttempts[0].OpID)
	intent, err := buildIntentSecret(provider, vm, contracts.PreparedOperation{Kind: contracts.OperationKindPowerOn,
		Identity: contracts.OperationAttemptIdentity{ProviderUID: string(identityProviderUID), HostID: "host-a", EnrolmentID: e,
			VMUID: string(vm.UID), OpID: op, Attempt: 1}, RequestBytes: []byte("r"), DigestSalt: bytes.Repeat([]byte{1}, 32)},
		coverageTestScheme(t), auth)
	require.NoError(t, err)
	r, _, live := freezeEnv(t, now, true, auth, vm, intent)
	changed, _, err := r.confirmFreezes(ctx, h, st)
	require.NoError(t, err)
	assert.True(t, changed, "an attempt under the current enrolment does not block the predecessor")
	assert.Equal(t, int32(1), live.lists.Load(), "the cache did not show it blocked: the API server is read")
	assert.Equal(t, int32(1), live.gets.Load(), "the intent is read once in the check")
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

// TestFreezeBackoffForgottenWithTheHost: a gone Host's freeze recheck and
// probe backoffs are dropped.
func TestFreezeBackoffForgottenWithTheHost(t *testing.T) {
	r, _, _ := freezeEnv(t, time.Now(), false, nil)
	key := types.NamespacedName{Namespace: "default", Name: "gone"}
	r.Config = &HostEnrolmentConfig{Namespace: "enrolment"}
	r.freezeWaits.Store(key.String(), 3)
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	_, ok := r.freezeWaits.Load(key.String())
	assert.False(t, ok, "the recheck backoff of a gone Host is dropped")

	answers := &HostAnswers{}
	other := types.NamespacedName{Namespace: "default", Name: "other"}
	for _, k := range []string{freezeProbeKey(key, []byte{1}), freezeProbeKey(key, []byte{2}), freezeProbeKey(other, []byte{1})} {
		answers.freezeProbes.Store(k, &freezeProbe{})
	}
	answers.pruneFreezeProbes(key, [][]byte{{2}})
	_, kept := answers.freezeProbes.Load(freezeProbeKey(key, []byte{2}))
	_, pruned := answers.freezeProbes.Load(freezeProbeKey(key, []byte{1}))
	assert.True(t, kept, "a predecessor still confirmed keeps its backoff")
	assert.False(t, pruned, "a removed predecessor's backoff is dropped")
	answers.pruneFreezeProbes(key, nil)
	_, kept = answers.freezeProbes.Load(freezeProbeKey(key, []byte{2}))
	_, otherKept := answers.freezeProbes.Load(freezeProbeKey(other, []byte{1}))
	assert.False(t, kept, "a gone Host's backoffs are dropped")
	assert.True(t, otherKept, "another Host's are kept")
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
// verified signed answer from a ready agent that it is UNKNOWN_TO_AGENT
// (discarded); while the host still holds it (NO_RECORD), when the answer is
// unsigned, signed by an agent that is not ready, signed for another op_id or
// enrolment, or under the predecessor itself, it stays. Probes that do not
// show it discarded back off.
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
	var mu sync.Mutex
	now := time.Now()
	answers := &HostAnswers{Trust: trust, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	later := func() { mu.Lock(); now = now.Add(freezeProbeMax + time.Minute); mu.Unlock() }
	check := func() []string {
		t.Helper()
		later()
		return answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha")
	}

	// Still held by the host: NO_RECORD, kept.
	stub.predecessors = [][]byte{prev}
	assert.Empty(t, check())
	// Unsigned (a Provider that strips the answer): kept.
	stub.predecessors, stub.unsigned = nil, true
	assert.Empty(t, check())
	stub.unsigned = false
	for name, edit := range map[string]func(a *providerv1.OperationAnswer){
		"another op_id":     func(a *providerv1.OperationAnswer) { a.OpId = bytes.Repeat([]byte{9}, 16) },
		"another enrolment": func(a *providerv1.OperationAnswer) { a.OpEnrolmentId = bytes.Repeat([]byte{8}, 16) },
		"an agent that is not ready": func(a *providerv1.OperationAnswer) {
			a.Context.AgentState = providerv1.AgentState_AGENT_STATE_UNATTESTED
		},
		"answered under the predecessor": func(a *providerv1.OperationAnswer) { a.Context.EnrolmentId = prev },
	} {
		stub.editAnswer = edit
		assert.Empty(t, check(), name)
	}
	assert.Empty(t, trust.discarded)

	// Probes back off: without time passing, the host is not asked again.
	stub.editAnswer = nil
	stub.predecessors = [][]byte{prev}
	asked := len(stub.opQueries)
	answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha")
	answers.checkFreezeDiscards(ctx, prov, opReader{stub}, "host-alpha")
	assert.Len(t, stub.opQueries, asked, "a missed probe waits before the next")

	// Discarded by the host: removed.
	stub.predecessors = nil
	removed := check()
	assert.Equal(t, []string{hex.EncodeToString(prev)}, removed)
	assert.Equal(t, [][]byte{prev}, trust.discarded)
	assert.Empty(t, check(), "nothing left to check")
	n := 0
	answers.freezeProbes.Range(func(_, _ any) bool { n++; return true })
	assert.Zero(t, n, "no probe backoff is kept for a removed predecessor")
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

	// The next reconcile asks the host about it as a predecessor's attempt
	// (the re-keyed host still holds the predecessor and has no record of
	// it) and still never sends it.
	stub.signed.host.mu.Lock()
	stub.signed.host.trust = next
	stub.signed.host.mu.Unlock()
	stub.signed.predecessors = [][]byte{cur.Enrolment.EnrolmentID}
	asked := len(stub.signed.opQueries)
	reconcileN(t, r, req, 1)
	assert.Empty(t, stub.sent())
	require.Greater(t, len(stub.signed.opQueries), asked, "the attempt is asked about")
	q := stub.signed.opQueries[len(stub.signed.opQueries)-1]
	assert.Equal(t, a.OpID, hex.EncodeToString(q.OpID))
	assert.Equal(t, cur.Enrolment.EnrolmentID, q.OpEnrolmentID, "asked about under the predecessor")
	vm = idVM(t, r)
	a = findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, a.Phase, "still no outcome")
	assert.Empty(t, stub.sent())
}

// failingTrust is a staticTrust whose reads fail while failing is set.
type failingTrust struct {
	*staticTrust
	failing atomic.Bool
}

func (f *failingTrust) HostTrust(ctx context.Context, p *infravirtrigaudiov1beta1.Provider, hostID string) (HostTrust, error) {
	if f.failing.Load() {
		return HostTrust{}, errors.New("transient read failure")
	}
	return f.staticTrust.HostTrust(ctx, p, hostID)
}

// TestIdentityCreate_TrustReadFailureBeforeFirstSendWaits: a failed re-read
// of the host's enrolment before the first send is not a re-key: the VM is
// not held, the attempt is retried and sent once the read works again.
func TestIdentityCreate_TrustReadFailureBeforeFirstSendWaits(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	st, ok := r.HostAnswers.Trust.(*staticTrust)
	require.True(t, ok)
	ft := &failingTrust{staticTrust: st}
	r.HostAnswers.Trust = ft
	r.operationCrashHook = func(_ context.Context, p operationCrashPoint) bool {
		if p == crashAfterPrepared {
			ft.failing.Store(true)
		}
		return false
	}
	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, providerErrorRetryInterval, res.RequeueAfter)
	assert.Empty(t, stub.sent())
	vm := idVM(t, r)
	assert.NotEqual(t, k8s.ReasonOperationHostRekeyed, readyReason(vm), "a failed read is not a re-key")
	a := findAttempt(vm, contracts.OperationKindCreate)
	require.NotNil(t, a)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhasePrepared, a.Phase)

	r.operationCrashHook = nil
	ft.failing.Store(false)
	reconcileN(t, r, req, 2)
	require.Len(t, stub.sent(), 1, "sent once the read works")
	assert.Equal(t, a.OpID, hex.EncodeToString(stub.sent()[0].Identity.OpID), "the same attempt")
}
