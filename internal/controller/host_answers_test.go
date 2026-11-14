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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity/hostidentitytest"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// The pins Secret is the source of host trust: the current enrolment, its
// key pins and the sub-CA. The store state lives under its own key in the
// same Secret, written with a resourceVersion precondition, and survives a
// rewrite of the enrolment state.
func TestPinsHostTrust(t *testing.T) {
	ctx := context.Background()
	sch := newProviderTLSScheme(t)
	ca := hostidentitytest.NewCA(t)
	key := hostidentitytest.NewKey(t)
	pin, err := hostidentity.SPKIPin(&key.PublicKey)
	require.NoError(t, err)
	prov := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default",
		UID: identityProviderUID}, Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor}}
	h := hostCR("host-a", "ch", nil)
	h.UID = "uid-host-a"
	h.Spec.Identity = "serial-1"
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(prov, h).Build()
	install := bytes.Repeat([]byte{0x42}, 16)
	cfg := &HostEnrolmentConfig{Namespace: "host-agent-pki", TrustDomain: "cluster.test", CABundle: ca.PEM, InstallID: install}
	src := &PinsHostTrust{Client: cli, Config: cfg, Epoch: operation.Epoch{Counter: 1, Holder: install}}

	_, err = src.HostTrust(ctx, prov, "host-a")
	require.ErrorIs(t, err, operation.ErrNoEnrolment, "no pins Secret: no trust")

	enrol := strings.Repeat("a1", 16)
	l := loadedEnrolment{state: hostEnrolmentState{HostUID: string(h.UID), Identity: h.Spec.Identity, Current: &currentEnrolment{
		Record:   enrolmentRecord{EnrolmentID: enrol, ProviderUID: string(identityProviderUID), InstallID: hex.EncodeToString(install)},
		KeyPins:  []string{pin},
		NotAfter: metav1.NewTime(time.Now().Add(time.Hour)),
	}}}
	require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l))
	trust, err := src.HostTrust(ctx, prov, "host-a")
	require.NoError(t, err)
	assert.Equal(t, enrol, hex.EncodeToString(trust.Enrolment.EnrolmentID))
	assert.Equal(t, []string{pin}, trust.Pins)
	assert.Equal(t, "spiffe://cluster.test/virtrigaud/host/default/host-a", trust.HostURI)
	assert.Equal(t, install, trust.InstallID)
	assert.NotNil(t, trust.Anchors)

	other := prov.DeepCopy()
	other.UID = "0b7a1c3e-1111-4222-8333-000000000000"
	_, err = src.HostTrust(ctx, other, "host-a")
	require.ErrorIs(t, err, operation.ErrNoEnrolment, "an enrolment of another Provider is not trusted")

	rec := hostidentity.StoreRecord{StoreID: [16]byte{1}, EnrolmentID: trust.Enrolment.EnrolmentID, Seq: 9, Head: [32]byte{9}}
	require.NoError(t, src.UpdateStoreState(ctx, prov, "host-a", func(st *HostStoreState) { st.Current = rec }))
	st, err := src.LoadStoreState(ctx, prov, "host-a")
	require.NoError(t, err)
	assert.Equal(t, rec, st.Current)

	// The enrolment reconciler rewriting the enrolment state keeps the store
	// positions.
	l2, err := loadHostEnrolment(ctx, cli, cfg, h)
	require.NoError(t, err)
	l2.state.RetiredEnrolmentIDs = append(l2.state.RetiredEnrolmentIDs, strings.Repeat("b2", 16))
	require.NoError(t, saveHostEnrolment(ctx, cli, cfg, h, &l2))
	st, err = src.LoadStoreState(ctx, prov, "host-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(9), st.Current.Seq, "the store positions survive an enrolment write")

	// A write that read an older resourceVersion fails; it never overwrites.
	sec := &corev1.Secret{}
	require.NoError(t, cli.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: hostEnrolmentSecretName("default", "host-a")}, sec))
	stale := sec.DeepCopy()
	require.NoError(t, src.UpdateStoreState(ctx, prov, "host-a", func(st *HostStoreState) { st.Current.Seq = 12 }))
	stale.Data[hostStoreStateKey] = []byte(`{}`)
	err = cli.Update(ctx, stale)
	require.True(t, apierrors.IsConflict(err), "a stale write conflicts: %v", err)
	st, err = src.LoadStoreState(ctx, prov, "host-a")
	require.NoError(t, err)
	assert.Equal(t, uint64(12), st.Current.Seq)

	// A malformed store document is an error, never an empty state.
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sec), sec))
	sec.Data[hostStoreStateKey] = []byte(`{"current":{"storeID":"zz"}}`)
	require.NoError(t, cli.Update(ctx, sec))
	_, err = src.LoadStoreState(ctx, prov, "host-a")
	require.Error(t, err)
}

// A host whose store is restored to an earlier position (a store restored
// with its anchor) is held: nothing it answers closes an attempt, nothing new
// is signed for it, and the hold is persisted, so a manager restart keeps it.
func TestHostAnswers_StoreRegressionHoldsHost(t *testing.T) {
	stub := newIdentityStub()
	r, req := createdIdentityVM(t, stub)
	trust, ok := r.HostAnswers.Trust.(*staticTrust)
	require.True(t, ok)
	stub.signed.host.rollBack(3)
	stub.describeResp.PowerState = "Off"
	setVMSpec(t, r, func(vm *infravirtrigaudiov1beta1.VirtualMachine) {
		vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOn
	})
	reconcileN(t, r, req, 1)
	sends := len(stub.sent())
	reconcileN(t, r, req, 2)
	vm := idVM(t, r)
	assert.Equal(t, k8s.ReasonOperationHostStoreRegressed, readyReason(vm))
	assert.Len(t, stub.sent(), sends, "nothing more is sent to a regressed host")
	if a := findAttempt(vm, contracts.OperationKindPowerOn); a != nil {
		assert.NotEqual(t, infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal, a.Phase)
	}
	assert.NotEmpty(t, trust.stores["host-alpha"].RegressedReason, "the hold is persisted")

	// A restarted manager (fresh memory, same persisted state) still holds
	// the host.
	r.HostAnswers = &HostAnswers{Trust: trust}
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
	assert.NotEmpty(t, r.HostAnswers.RegressedReason(context.Background(), p, "host-alpha"))
	reconcileN(t, r, req, 1)
	assert.Equal(t, k8s.ReasonOperationHostStoreRegressed, readyReason(idVM(t, r)))
}

// A definite closure persists the host's store position before it is acted
// on.
func TestHostAnswers_DefiniteAnswerPersistsPositionFirst(t *testing.T) {
	stub := newIdentityStub()
	r, _ := createdIdentityVM(t, stub)
	trust, ok := r.HostAnswers.Trust.(*staticTrust)
	require.True(t, ok)
	st := trust.stores["host-alpha"]
	assert.GreaterOrEqual(t, st.Current.Seq, uint64(3), "the position of the answer that closed the Create is persisted")
	assert.Positive(t, trust.writes)
}

// Without a recent verified host answer from a READY agent, nothing new is
// signed for a host.
func TestHostAnswers_GrantGate(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	now := time.Now().Add(hostAnswerTTL + time.Minute)
	r.HostAnswers.Now = func() time.Time { return now }
	reconcileN(t, r, req, 2)
	assert.Equal(t, k8s.ReasonOperationHostAnswerUnavailable, readyReason(idVM(t, r)))
	assert.Empty(t, stub.sent())

	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
	stub.signed.host.state = 1 // UNATTESTED
	_, _, verdict, err := r.HostAnswers.RefreshHostAnswer(context.Background(), p, stub, "host-alpha")
	require.NoError(t, err)
	require.Equal(t, answerVerified, verdict)
	reconcileN(t, r, req, 1)
	assert.Equal(t, k8s.ReasonOperationHostAnswerUnavailable, readyReason(idVM(t, r)), "an agent that is not READY gets nothing new")
	assert.Empty(t, stub.sent())
}

// The Host heartbeat of a Provider that requires operation identity asks
// for a signed host answer and records whether it verified.
func TestHostReconciler_SignedHeartbeat(t *testing.T) {
	stub := newIdentityStub()
	r, _ := identityFixture(t, stub)
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
	hr := &HostReconciler{Client: r.Client, HostAnswers: r.HostAnswers}
	host := &infravirtrigaudiov1beta1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-alpha", Namespace: clusteredNS}}
	info, err := hr.getHostInfo(context.Background(), host, p, stub)
	require.NoError(t, err)
	assert.Equal(t, "host-alpha", info.ID)
	c := meta.FindStatusCondition(host.Status.Conditions, HostConditionAgentAnswer)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)

	stub.signed.unsigned = true
	_, err = hr.getHostInfo(context.Background(), host, p, stub)
	require.NoError(t, err)
	c = meta.FindStatusCondition(host.Status.Conditions, HostConditionAgentAnswer)
	assert.Equal(t, metav1.ConditionFalse, c.Status)

	stub.signed.unsigned = false
	stub.signed.host.rollBack(5)
	_, err = hr.getHostInfo(context.Background(), host, p, stub)
	require.NoError(t, err)
	c = meta.FindStatusCondition(host.Status.Conditions, HostConditionAgentStoreRegressed)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionTrue, c.Status)

	// A non-identity Provider is read as before.
	plain := p.DeepCopy()
	plain.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	fresh := &infravirtrigaudiov1beta1.Host{ObjectMeta: metav1.ObjectMeta{Name: "host-alpha", Namespace: clusteredNS}}
	_, err = hr.getHostInfo(context.Background(), fresh, plain, stub)
	require.NoError(t, err)
	assert.Empty(t, fresh.Status.Conditions, "no host answer condition for a non-identity Provider")
}

// flakyTrust fails the store-state writes of its inner trust: conflicts
// counts writes answered with a conflict, failures writes answered with
// another error.
type flakyTrust struct {
	*staticTrust
	mu        sync.Mutex
	conflicts int
	failures  int
}

func (f *flakyTrust) UpdateStoreState(ctx context.Context, p *infravirtrigaudiov1beta1.Provider, hostID string, update func(*HostStoreState)) error {
	f.mu.Lock()
	switch {
	case f.conflicts > 0:
		f.conflicts--
		f.mu.Unlock()
		return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "pins", errors.New("modified"))
	case f.failures > 0:
		f.failures--
		f.mu.Unlock()
		return errors.New("the API server is unavailable")
	}
	f.mu.Unlock()
	return f.staticTrust.UpdateStoreState(ctx, p, hostID, update)
}

// A store-regression hold survives a manager restart even when its first
// write fails: a conflict is retried at once, any other failure on the next
// read of the host, and forgetting the host's positions never drops the hold.
func TestHostAnswers_RegressionHoldIsAlwaysPersisted(t *testing.T) {
	ctx := context.Background()
	prov := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "ch", Namespace: "default", UID: identityProviderUID}}
	enrol := bytes.Repeat([]byte{3}, 16)
	h := newTestHost(t, prov, "host-a", enrol, operation.Epoch{Counter: 1, Holder: bytes.Repeat([]byte{1}, 16)})
	held := func(trust HostTrustSource) bool {
		return (&HostAnswers{Trust: trust}).RegressedReason(ctx, prov, "host-a") != ""
	}

	t.Run("a conflict is retried", func(t *testing.T) {
		inner := &staticTrust{hosts: map[string]*testHost{"host-a": h}}
		trust := &flakyTrust{staticTrust: inner, conflicts: 1}
		answers := &HostAnswers{Trust: trust}
		answers.markRegressed(ctx, prov, "host-a", hostidentity.ContinuityRolledBack, enrol)
		assert.True(t, held(inner), "a restarted manager still holds the host")
	})
	t.Run("a failed write is retried on the next read", func(t *testing.T) {
		inner := &staticTrust{hosts: map[string]*testHost{"host-a": h}}
		trust := &flakyTrust{staticTrust: inner, failures: 1}
		answers := &HostAnswers{Trust: trust}
		answers.markRegressed(ctx, prov, "host-a", hostidentity.ContinuityRolledBack, enrol)
		require.False(t, held(inner), "the first write failed")
		answers.forget(prov, "host-a")
		assert.NotEmpty(t, answers.RegressedReason(ctx, prov, "host-a"), "forgetting positions keeps the hold")
		assert.True(t, held(inner), "the next read wrote the hold; a restarted manager still holds the host")
	})
	t.Run("a failed position write keeps the hold", func(t *testing.T) {
		inner := &staticTrust{hosts: map[string]*testHost{"host-a": h}}
		trust := &flakyTrust{staticTrust: inner, failures: 100}
		answers := &HostAnswers{Trust: trust}
		answers.markRegressed(ctx, prov, "host-a", hostidentity.ContinuityRolledBack, enrol)
		answers.forget(prov, "host-a") // what a failed definite position write does
		assert.NotEmpty(t, answers.RegressedReason(ctx, prov, "host-a"))
		trust.mu.Lock()
		trust.failures = 0
		trust.mu.Unlock()
		assert.NotEmpty(t, answers.RegressedReason(ctx, prov, "host-a"))
		assert.True(t, held(inner))
	})
}

// The host's store position is persisted before an attempt is first sent,
// and before a re-send that a signed NO_RECORD authorised: a restart and a
// rollback between the persisted and the in-memory position cannot pass.
func TestHostAnswers_PositionPersistedBeforeEverySend(t *testing.T) {
	stub := newIdentityStub()
	r, req := identityFixture(t, stub)
	trust, ok := r.HostAnswers.Trust.(*staticTrust)
	require.True(t, ok)
	var persistedAtSend, seenAtSend []uint64
	calls := 0
	stub.onSend = func(p contracts.PreparedOperation) (contracts.OperationAnswer, error) {
		trust.mu.Lock()
		persistedAtSend = append(persistedAtSend, trust.stores["host-alpha"].Current.Seq)
		trust.mu.Unlock()
		stub.signed.host.mu.Lock()
		seenAtSend = append(seenAtSend, stub.signed.host.seq)
		stub.signed.host.mu.Unlock()
		calls++
		if calls == 1 {
			return contracts.OperationAnswer{}, errors.New("connection reset")
		}
		return contracts.OperationAnswer{ID: "vm-1"}, nil
	}
	reconcileN(t, r, req, 2)
	require.Len(t, persistedAtSend, 2)
	assert.Equal(t, seenAtSend[0], persistedAtSend[0], "the first send follows the persisted position of the last answer seen")
	assert.Equal(t, seenAtSend[1], persistedAtSend[1], "the re-send follows the persisted position of the NO_RECORD that authorised it")
}
