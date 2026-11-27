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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// countingAuthority is a HostAuthorityCaller that records the grants it was
// asked to deliver and delivers none.
type countingAuthority struct{ attests, marks, retires int }

func (c *countingAuthority) Challenge(context.Context, contracts.ChallengeQuery) (contracts.AuthorityRead, error) {
	return contracts.AuthorityRead{}, errors.New("not reached")
}

func (c *countingAuthority) Attest(context.Context, contracts.AttestOrder) (contracts.AuthorityRead, error) {
	c.attests++
	return contracts.AuthorityRead{}, nil
}

func (c *countingAuthority) LedgerMark(context.Context, contracts.LedgerMarkOrder) (contracts.AuthorityRead, error) {
	c.marks++
	return contracts.AuthorityRead{}, nil
}

func (c *countingAuthority) RetireIfUnknown(context.Context, contracts.RetireOrder) (contracts.AuthorityRead, error) {
	c.retires++
	return contracts.AuthorityRead{}, nil
}

func ledgerTestProvider() *infravirtrigaudiov1beta1.Provider {
	p := &infravirtrigaudiov1beta1.Provider{}
	p.Namespace, p.Name, p.UID = "ns", "ch", "0b7a1c3e-1111-4222-8333-944455556666"
	return p
}

// No attestation grant is signed without a ledger object of the store that
// holds the session as begun by this manager: no record, a record that was
// never read from or written as a ledger object, a record that does not
// hold the session, and a record whose object cannot be read now are all
// refused, and nothing reaches the host.
func TestSendAttest_NeverWithoutALedgerObject(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	keys, err := NewHostLedgerKeys(randomLedgerKey(t), nil, randomLedgerKey(t))
	require.NoError(t, err)
	l := &HostLedger{Client: c, APIReader: c, Trust: &staticTrust{}, Keys: keys,
		Config: &HostEnrolmentConfig{Namespace: "enrol", InstallID: bytes.Repeat([]byte{1}, 16)}}
	h := &HostAnswers{Trust: &staticTrust{}, Ledger: l}
	auth := &countingAuthority{}
	provider := ledgerTestProvider()
	nonce := [16]byte{9}
	store := [16]byte{1}
	for name, rec := range map[string]*HostLedgerRecord{
		"no record":                 nil,
		"never a ledger object":     {StoreID: store, PendingSessions: []HostLedgerPendingSession{{ID: nonce}}},
		"session not begun":         {StoreID: store, resourceVersion: "7"},
		"object unreadable now":     {StoreID: store, resourceVersion: "7", PendingSessions: []HostLedgerPendingSession{{ID: nonce}}},
		"archived ledger of store":  {StoreID: store, resourceVersion: "7", Archived: true, PendingSessions: []HostLedgerPendingSession{{ID: nonce}}},
		"another session is begun:": {StoreID: store, resourceVersion: "7", PendingSessions: []HostLedgerPendingSession{{ID: [16]byte{8}}}},
	} {
		_, err := h.sendAttest(context.Background(), provider, auth, "h", HostTrust{}, rec, nonce)
		require.ErrorIs(t, err, errNotAttestable, name)
	}
	_, err = (&HostAnswers{}).sendAttest(context.Background(), provider, auth, "h", HostTrust{},
		&HostLedgerRecord{StoreID: store, resourceVersion: "7", PendingSessions: []HostLedgerPendingSession{{ID: nonce}}}, nonce)
	require.ErrorIs(t, err, errNotAttestable, "no host ledger")
	assert.Zero(t, auth.attests, "no attestation grant was asked for")
}

// No ledger mark is signed except from the record of this manager's own
// last write of the store's ledger, in an adopted session, while nobody
// wrote since.
func TestSendLedgerMark_OnlyFromOwnWrite(t *testing.T) {
	l := &HostLedger{}
	h := &HostAnswers{Ledger: l}
	auth := &countingAuthority{}
	provider := ledgerTestProvider()
	store := [16]byte{1}
	attested := &HostLedgerRecord{StoreID: store, WriteCounter: 4, AttestationID: [16]byte{5},
		AttestationState: infravirtrigaudiov1beta1.HostAgentLedgerAttestationAttested, Position: HostLedgerPosition{Seq: 10}}
	_, err := h.sendLedgerMark(context.Background(), provider, auth, "h", HostTrust{}, attested)
	require.ErrorIs(t, err, errNotMarkable, "no own write")
	l.own = map[[16]byte]*HostLedgerRecord{store: attested.clone()}
	newer := attested.clone()
	newer.WriteCounter = 5
	_, err = h.sendLedgerMark(context.Background(), provider, auth, "h", HostTrust{}, newer)
	require.ErrorIs(t, err, errNotMarkable, "someone else wrote since")
	pending := attested.clone()
	pending.AttestationState = infravirtrigaudiov1beta1.HostAgentLedgerAttestationPending
	l.own[store] = pending
	_, err = h.sendLedgerMark(context.Background(), provider, auth, "h", HostTrust{}, attested)
	require.ErrorIs(t, err, errNotMarkable, "no adopted session")
	assert.Zero(t, auth.marks)
	l.own[store] = attested.clone()
	_, err = h.sendLedgerMark(context.Background(), provider, auth, "h", HostTrust{}, attested)
	require.NoError(t, err)
	assert.Equal(t, 1, auth.marks)
}

// The grant sites of the host ledger: the controllers ask for an
// attestation grant only in sendAttest, for a ledger mark grant only in
// sendLedgerMark and for a retire-if-unknown grant only in sendRetire,
// build their orders only there, and a ledger record gets a resourceVersion
// (the mark of an existing, verified ledger object) only in verifySecret.
func TestLedgerGrantSites(t *testing.T) {
	allowed := map[string]string{"Attest": "sendAttest", "LedgerMark": "sendLedgerMark", "AttestOrder": "sendAttest",
		"LedgerMarkOrder": "sendLedgerMark", "RetireIfUnknown": "sendRetire", "RetireOrder": "sendRetire",
		"resourceVersion": "verifySecret"}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) // #nosec G304 -- this package's own files
		require.NoError(t, err)
		file, err := parser.ParseFile(token.NewFileSet(), f, src, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				name := ""
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Attest" || sel.Sel.Name == "LedgerMark" ||
						sel.Sel.Name == "RetireIfUnknown") {
						name = sel.Sel.Name
					}
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok && (sel.Sel.Name == "AttestOrder" || sel.Sel.Name == "LedgerMarkOrder" ||
						sel.Sel.Name == "RetireOrder") {
						name = sel.Sel.Name
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "resourceVersion" {
							name = "resourceVersion"
						}
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "resourceVersion" {
						name = "resourceVersion"
					}
				}
				if name != "" {
					assert.Equal(t, allowed[name], fd.Name.Name, "%s: %s outside its one site", f, name)
				}
				return true
			})
		}
	}
}

// Attestation, heartbeat and operation traffic draw from separate buckets:
// one exhausted never refuses another.
func TestLedgerBuckets_Split(t *testing.T) {
	now := time.Unix(1000, 0)
	all := []ledgerTraffic{trafficAttest, trafficHeartbeat, trafficOperation}
	for _, drained := range all {
		var b ledgerBuckets
		for b.allow(drained, now) {
		}
		for _, other := range all {
			if other != drained {
				assert.True(t, b.allow(other, now), "traffic %d is not starved by traffic %d", other, drained)
			}
		}
		assert.False(t, b.allow(drained, now))
		assert.True(t, b.allow(drained, now.Add(time.Second)), "the bucket refills")
	}
}

// A heartbeat raise is checked against the per-host throttle before it
// takes a heartbeat token: a throttled host drains nothing.
func TestHeartbeatThrottleBeforeToken(t *testing.T) {
	provider := ledgerTestProvider()
	now := time.Unix(5000, 0)
	l := &HostLedger{Now: func() time.Time { return now }}
	l.heartbeatAt = map[string]time.Time{hostAnswerKey(provider, "h"): now}
	assert.False(t, l.heartbeatDue(provider, "h"), "raised just now")
	var b ledgerBuckets
	for range 10 * heartbeatBucketBurst {
		if l.heartbeatDue(provider, "h") {
			b.allow(trafficHeartbeat, now)
		}
	}
	for range heartbeatBucketBurst {
		require.True(t, b.allow(trafficHeartbeat, now), "the throttled host took no token")
	}
	now = now.Add(ledgerHeartbeatRaiseInterval)
	assert.True(t, l.heartbeatDue(provider, "h"))
}

// A host whose agent speaks the host ledger protocol gets grant version 2
// only while the driver saw it attested in the ledger's adopted session of
// its store, and never version 1; other hosts keep version 1.
func TestGrantReady_StrictLedgerVersion(t *testing.T) {
	provider := ledgerTestProvider()
	now := time.Now()
	enrol := bytes.Repeat([]byte{3}, 16)
	session, store := [16]byte{5}, [16]byte{6}
	h := &HostAnswers{Ledger: &HostLedger{}, Now: func() time.Time { return now }}
	h.hosts = map[string]*hostAnswerState{hostAnswerKey(provider, "h"): {loaded: true, anchored: true, hostAnswerAt: now,
		hostAnswerEnrol: enrol, hostAgentState: providerv1.AgentState_AGENT_STATE_READY, hostAPIVersion: ledgerAgentAPIVersion,
		hostGrantVersions: []uint32{grantverify.LedgerGrantVersion}, hostSession: session, hostAnswerStore: store}}
	_, err := h.grantReady(provider, "h", enrol)
	require.ErrorIs(t, err, errLedgerNotAttested)
	_, ok := h.sendVersion(provider, "h")
	assert.False(t, ok, "nothing is sent to a ledger host that is not attested")

	h.updateLedger(provider, "h", func(st *hostLedgerState) { st.ready, st.session, st.store, st.generation = true, [16]byte{7}, store, 9 })
	_, err = h.grantReady(provider, "h", enrol)
	require.ErrorIs(t, err, errLedgerNotAttested, "attested in another session than the host's answer names")

	h.updateLedger(provider, "h", func(st *hostLedgerState) { st.session = session })
	v, err := h.grantReady(provider, "h", enrol)
	require.NoError(t, err)
	assert.Equal(t, grantverify.LedgerGrantVersion, v)
	v, ok = h.sendVersion(provider, "h")
	assert.True(t, ok)
	assert.Equal(t, grantverify.LedgerGrantVersion, v)

	noLedger := &HostAnswers{Now: h.Now, hosts: h.hosts}
	_, err = noLedger.grantReady(provider, "h", enrol)
	require.Error(t, err, "without a host ledger nothing is signed for a version-2-only agent")

	h.hosts[hostAnswerKey(provider, "old")] = &hostAnswerState{loaded: true, anchored: true, hostAnswerAt: now, hostAnswerEnrol: enrol,
		hostAgentState: providerv1.AgentState_AGENT_STATE_READY, hostAPIVersion: 4, hostGrantVersions: []uint32{grantverify.GrantVersion}}
	v, err = h.grantReady(provider, "old", enrol)
	require.NoError(t, err)
	assert.Equal(t, grantverify.GrantVersion, v, "an older agent keeps version 1")
}

// A recorded receipt counts as ledgered when it is at or below the ledger
// position in the current or a pending session, at or below the fencing
// position of an earlier session, or of a session or store the ledger does
// not know; a receipt above the fencing position of an earlier session
// never had a mark and does not count.
func TestReceiptLedgered(t *testing.T) {
	store := [16]byte{1}
	cur, pend, old, older := [16]byte{2}, [16]byte{3}, [16]byte{4}, [16]byte{5}
	rec := &HostLedgerRecord{StoreID: store, AttestationID: cur, Position: HostLedgerPosition{Seq: 100},
		PendingSessions: []HostLedgerPendingSession{{ID: pend}}, SessionHistory: []HostLedgerFencedSession{{ID: old, FencedAt: 50}}}
	rc := func(s [16]byte, seq int64, st [16]byte) *infravirtrigaudiov1beta1.OperationAttemptReceipt {
		return &infravirtrigaudiov1beta1.OperationAttemptReceipt{StoreID: hex.EncodeToString(st[:]), Seq: seq,
			AttestationID: hex.EncodeToString(s[:])}
	}
	for name, c := range map[string]struct {
		rc   *infravirtrigaudiov1beta1.OperationAttemptReceipt
		want bool
	}{
		"current, covered":           {rc(cur, 100, store), true},
		"current, above":             {rc(cur, 101, store), false},
		"pending, covered":           {rc(pend, 90, store), true},
		"pending, above":             {rc(pend, 101, store), false},
		"fenced, at fence":           {rc(old, 50, store), true},
		"fenced, above fence":        {rc(old, 51, store), false},
		"older than history":         {rc(older, 1000, store), true},
		"another store":              {rc(cur, 1000, [16]byte{9}), true},
		"malformed counts as ledger": {&infravirtrigaudiov1beta1.OperationAttemptReceipt{StoreID: "zz", Seq: 1}, true},
	} {
		assert.Equal(t, c.want, receiptLedgered(rec, c.rc), name)
	}
}

// Continuity with the ledger is read from the store and generation the
// attempt's authenticated intent covers: an attempt prepared without a
// ledger (generation 0), under another store or generation, or under an
// enrolment the generation does not list, is never continuous.
func TestLedgerContinuous(t *testing.T) {
	store := [16]byte{1}
	enrol := bytes.Repeat([]byte{3}, 16)
	rec := &HostLedgerRecord{StoreID: store, Generation: 7, GenerationEnrolments: [][]byte{enrol}}
	p := contracts.PreparedOperation{Identity: contracts.OperationAttemptIdentity{EnrolmentID: enrol}, LedgerStoreID: store[:],
		LedgerGeneration: 7}
	assert.True(t, ledgerContinuous(p, rec))
	v1 := p
	v1.LedgerStoreID, v1.LedgerGeneration = nil, 0
	assert.False(t, ledgerContinuous(v1, rec), "an intent of the first version counts as generation 0")
	other := p
	other.LedgerGeneration = 6
	assert.False(t, ledgerContinuous(other, rec), "another generation (a fresh ledger since)")
	otherStore := p
	otherStore.LedgerStoreID = bytes.Repeat([]byte{2}, 16)
	assert.False(t, ledgerContinuous(otherStore, rec))
	otherEnrol := p
	otherEnrol.Identity.EnrolmentID = bytes.Repeat([]byte{4}, 16)
	assert.False(t, ledgerContinuous(otherEnrol, rec))
}

// The ledger stamp of an intent is covered by its tag: it round-trips, and
// a changed generation, a changed store, or a removed stamp no longer
// authenticates; an unstamped intent reads as generation 0.
func TestIntentLedgerStamp_Authenticated(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, infravirtrigaudiov1beta1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	auth, err := operation.NewIntentAuthenticator(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	provider := ledgerTestProvider()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vm", UID: "vm-uid"}}
	opID := bytes.Repeat([]byte{0xa}, 16)
	p := contracts.PreparedOperation{Kind: contracts.OperationKindCreate, Identity: contracts.OperationAttemptIdentity{OpID: opID,
		VMUID: "vm-uid"}, RequestBytes: []byte{1, 2}, DigestSalt: bytes.Repeat([]byte{3}, 32),
		LedgerStoreID: bytes.Repeat([]byte{4}, 16), LedgerGeneration: 42}
	s, err := buildIntentSecret(provider, vm, p, scheme, auth)
	require.NoError(t, err)
	loader := intentLoader{auth: auth}
	got, err := loader.openIntentSecret(vm, hex.EncodeToString(opID), s)
	require.NoError(t, err)
	assert.Equal(t, uint64(42), got.LedgerGeneration)
	assert.Equal(t, p.LedgerStoreID, got.LedgerStoreID)

	for name, edit := range map[string]func(*corev1.Secret){
		"generation": func(s *corev1.Secret) { s.Data[intentKeyLedgerGeneration] = []byte("43") },
		"store":      func(s *corev1.Secret) { s.Data[intentKeyLedgerStore] = []byte(strings.Repeat("05", 16)) },
		"removed": func(s *corev1.Secret) {
			delete(s.Data, intentKeyLedgerGeneration)
			delete(s.Data, intentKeyLedgerStore)
		},
	} {
		c := s.DeepCopy()
		edit(c)
		_, err := loader.openIntentSecret(vm, hex.EncodeToString(opID), c)
		require.ErrorIs(t, err, errIntentTampered, name)
	}

	p.LedgerStoreID, p.LedgerGeneration = nil, 0
	s, err = buildIntentSecret(provider, vm, p, scheme, auth)
	require.NoError(t, err)
	got, err = loader.openIntentSecret(vm, hex.EncodeToString(opID), s)
	require.NoError(t, err)
	assert.Zero(t, got.LedgerGeneration)
	assert.NotContains(t, s.Data, intentKeyLedgerStore)
}

// A recorded receipt is bound to its attempt: its tag does not verify on
// another attempt or once edited.
func TestReceiptTag_BindsAttempt(t *testing.T) {
	auth, err := operation.NewIntentAuthenticator(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	r := &VirtualMachineReconciler{OperationIntentAuth: auth}
	vm := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{UID: "vm-uid"}}
	a := infravirtrigaudiov1beta1.OperationAttempt{Kind: infravirtrigaudiov1beta1.OperationAttemptKindCreate, Attempt: 1,
		OpID: strings.Repeat("ab", 16), Phase: infravirtrigaudiov1beta1.OperationAttemptPhaseSent}
	vr := &verifiedReceipt{generation: 3, marker: &infravirtrigaudiov1beta1.OperationAttemptReceipt{StoreID: strings.Repeat("01", 16), Seq: 9,
		Head: strings.Repeat("02", 32), AttestationID: strings.Repeat("03", 16)}}
	got := r.recordReceipt(vm, a, vr)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, got.Phase)
	assert.EqualValues(t, 3, got.LedgerGeneration)
	assert.True(t, r.receiptBound(vm, &got))
	other := got
	other.OpID = strings.Repeat("cd", 16)
	assert.False(t, r.receiptBound(vm, &other), "copied onto another attempt")
	edited := got
	rc := *got.Receipt
	rc.Seq = 8
	edited.Receipt = &rc
	assert.False(t, r.receiptBound(vm, &edited), "edited")
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptReceiptTagContext, operation.ReceiptAuthContext)
	unstamped := *vr
	unstamped.generation = 0
	assert.Zero(t, r.recordReceipt(vm, got, &unstamped).LedgerGeneration,
		"the display generation is left unset for an intent without one")
}
