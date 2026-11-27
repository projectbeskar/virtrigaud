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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/hostidentity"
	"github.com/projectbeskar/virtrigaud/internal/hostinventory"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// invDigest is a test digest of s.
func invDigest(s string) []byte {
	d := sha256.Sum256([]byte(s))
	return d[:]
}

// testInventoryAllowList allows the cloud-hypervisor component of
// goodInventory and requires it.
func testInventoryAllowList(t testing.TB) *hostinventory.AllowList {
	t.Helper()
	hx := func(s string) string { return "sha256:" + hex.EncodeToString(invDigest(s)) }
	a, err := hostinventory.Parse([]byte(fmt.Sprintf(`{"version":1,"required":["cloud-hypervisor"],"components":[
		{"name":"cloud-hypervisor","version":"1.0.0","alternatives":[{"artifactDigest":%q,"files":[
			{"name":"cloud-hypervisor","digest":%q},{"name":"hypervisor-fw","digest":%q}]}]}]}`,
		hx("image"), hx("ch"), hx("fw"))))
	require.NoError(t, err)
	return a
}

// goodInventory is a complete, ready inventory for a request of q: the
// allow-listed component, every default required feature present.
func goodInventory(q contracts.HostAnswerQuery) *providerv1.HostInventory {
	inv := &providerv1.HostInventory{
		InventoryVersion: min(q.InventoryVersion, 1), State: providerv1.InventoryState_INVENTORY_STATE_COMPLETE, SnapshotAgeSeconds: 10,
		Components: []*providerv1.HostComponent{{Name: "cloud-hypervisor", State: providerv1.ComponentState_COMPONENT_STATE_READY,
			ObservedVersion: "1.0.0", DeclaredVersion: "1.0.0", Source: "sysext", ManifestOrigin: providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY,
			ArtifactSha256: invDigest("image"), Files: []*providerv1.ComponentFile{{Name: "cloud-hypervisor", Sha256: invDigest("ch")},
				{Name: "hypervisor-fw", Sha256: invDigest("fw")}}}},
		ProcessIdentityTier: providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFS_HANDLE,
		RequestedVersion:    q.InventoryVersion, SupportedVersion: 1,
	}
	for _, f := range defaultRequiredFeatures {
		inv.Features = append(inv.Features, &providerv1.HostFeature{Name: f, Result: providerv1.FeatureResult_FEATURE_RESULT_PRESENT})
	}
	return inv
}

// testPolicy is the default policy with the test allow-list.
func testPolicy(t testing.TB) *HostInventoryPolicy {
	p := DefaultHostInventoryPolicy()
	p.AllowList = testInventoryAllowList(t)
	return p
}

// invClock is a settable clock.
type invClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *invClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *invClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// invFixture is a Host reconciler for one host of a Provider whose type runs
// a per-host agent, with a signed test host behind an identity stub.
type invFixture struct {
	r        *HostReconciler
	stub     *identityStub
	signed   *signedHostStub
	provider *infravirtrigaudiov1beta1.Provider
	clock    *invClock
	events   *record.FakeRecorder
}

func newInvFixture(t *testing.T, policy *HostInventoryPolicy) *invFixture {
	t.Helper()
	providerCR := withRuntime(clusteredProviderCR("prov-cluster", clusteredNS))
	providerCR.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	providerCR.UID = identityProviderUID
	host := readyHost("host-alpha", clusteredNS, "pool-a", providerCR.Name)
	enrol := make([]byte, operation.IDLen)
	enrol[0] = 2
	// The enrolment controller's mirror of the pins Secret.
	host.Status = infravirtrigaudiov1beta1.HostStatus{Agent: &infravirtrigaudiov1beta1.HostAgentStatus{
		Mode: infravirtrigaudiov1beta1.HostAgentModeIdentity, EnrolmentID: hex.EncodeToString(enrol)}}
	s := coverageTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(providerCR, host).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementHostIndex, placementHostIndexValue).
		WithStatusSubresource(&infravirtrigaudiov1beta1.Host{}).Build()
	holder := make([]byte, operation.IDLen)
	holder[0] = 1
	h := newTestHost(t, providerCR, "host-alpha", enrol, operation.Epoch{Counter: 2, Holder: holder})
	stub := newIdentityStub()
	signed := newSignedHostStub(t, h)
	signed.inventory = goodInventory
	stub.setSigned(signed)
	clock := &invClock{now: time.Now()}
	events := record.NewFakeRecorder(64)
	answers := &HostAnswers{Trust: &staticTrust{hosts: map[string]*testHost{h.hostID: h}}, Inventory: policy, Now: clock.Now}
	r := &HostReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: stub}, HostAnswers: answers, Recorder: events}
	return &invFixture{r: r, stub: stub, signed: signed, provider: providerCR, clock: clock, events: events}
}

func (f *invFixture) reconcile(t *testing.T) *infravirtrigaudiov1beta1.Host {
	t.Helper()
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"}})
	require.NoError(t, err)
	h := &infravirtrigaudiov1beta1.Host{}
	require.NoError(t, f.r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"}, h))
	return h
}

func (f *invFixture) lastQuery(t *testing.T) contracts.HostAnswerQuery {
	t.Helper()
	q := f.signed.queries()
	require.NotEmpty(t, q)
	return q[len(q)-1]
}

func cond(t *testing.T, h *infravirtrigaudiov1beta1.Host, typ string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(h.Status.Conditions, typ)
	require.NotNil(t, c, "condition %s", typ)
	return c
}

func requireCond(t *testing.T, h *infravirtrigaudiov1beta1.Host, typ string, status metav1.ConditionStatus, reason string) *metav1.Condition {
	t.Helper()
	c := cond(t, h, typ)
	require.Equal(t, status, c.Status, "%s: %s", typ, c.Message)
	require.Equal(t, reason, c.Reason, "%s: %s", typ, c.Message)
	require.Equal(t, h.Generation, c.ObservedGeneration)
	return c
}

// A host whose verified inventory is complete, ready and allow-listed is
// Ready; every heartbeat asks for the inventory; the status mirror shows the
// inventory with its allow-list results; a transition is evented.
func TestHostInventory_ReadyHost(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	h := f.reconcile(t)
	assert.EqualValues(t, hostidentity.InventoryVersion, f.lastQuery(t).InventoryVersion, "the heartbeat asks for the inventory")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, h.Status.Health)
	requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionTrue,
		infravirtrigaudiov1beta1.HostReasonInventoryVerified)
	c := requireCond(t, h, infravirtrigaudiov1beta1.HostConditionComponentsReady, metav1.ConditionTrue,
		infravirtrigaudiov1beta1.HostReasonComponentsVerified)
	sha := f.r.HostAnswers.Inventory.AllowListSHA256()
	assert.Contains(t, c.Message, sha, "the condition message names the allow-list digest")
	requireCond(t, h, k8s.ConditionReady, metav1.ConditionTrue, reasonHostReady)
	m := h.Status.AgentInventory
	require.NotNil(t, m)
	assert.Equal(t, "Complete", m.State)
	assert.Equal(t, sha, m.AllowListSHA256)
	assert.Len(t, m.AnswerSHA256, 64)
	assert.EqualValues(t, 1, m.InventoryVersion)
	assert.Equal(t, "PidfsHandle", m.ProcessIdentityTier)
	require.Len(t, m.Components, 1)
	assert.Equal(t, infravirtrigaudiov1beta1.HostInventoryAllowListAllowed, m.Components[0].AllowList)
	assert.Equal(t, "ReadOnly", m.Components[0].ManifestOrigin)
	assert.Equal(t, "sha256:"+hex.EncodeToString(invDigest("image")), m.Components[0].ArtifactDigest)
	require.Len(t, m.Components[0].Files, 2)
	assert.Len(t, m.Features, len(defaultRequiredFeatures))
	select {
	case e := <-f.events.Events:
		assert.Contains(t, e, infravirtrigaudiov1beta1.HostReasonInventoryVerified)
		assert.Contains(t, e, m.AnswerSHA256)
		assert.Contains(t, e, sha)
	default:
		t.Fatal("no inventory Event")
	}
}

// A host of a Provider that runs no per-host agent is unaffected: no
// inventory is asked for and no inventory condition is set.
func TestHostInventory_NonAgentProviderUnaffected(t *testing.T) {
	s := coverageTestScheme(t)
	prov := clusterProvider("prov-a")
	host := hostCR("host-alpha", "prov-a", nil)
	stub := &stubProvider{GetHostInfoFn: func(_ context.Context, id string) (contracts.HostInfo, error) { return healthyHostInfo(id), nil }}
	r := newHostReconciler(s, &stubResolver{provider: stub}, prov, host)
	r.HostAnswers = &HostAnswers{Trust: &staticTrust{}}
	_, err := r.Reconcile(context.Background(), hostReq("host-alpha"))
	require.NoError(t, err)
	got := getHost(t, r.Client, "host-alpha")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, got.Status.Health)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified))
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, infravirtrigaudiov1beta1.HostConditionComponentsReady))
	assert.Nil(t, got.Status.AgentInventory)
}

// No freeze window: one heartbeat without a verified answer makes the host
// NotReady with both conditions False in the same status write, whatever
// the last good inventory was.
func TestHostInventory_NoFreezeWindow(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	require.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, f.reconcile(t).Status.Health)
	f.signed.unsigned = true
	h := f.reconcile(t)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthNotReady, h.Status.Health)
	requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
	requireCond(t, h, infravirtrigaudiov1beta1.HostConditionComponentsReady, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonComponentsInventoryNotVerified)
	ready := requireCond(t, h, k8s.ConditionReady, metav1.ConditionFalse, reasonAgentInventoryNotReady)
	assert.Contains(t, ready.Message, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified)
	assert.Contains(t, ready.Message, infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
	// The next good heartbeat restores it.
	f.signed.unsigned = false
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, f.reconcile(t).Status.Health)
}

// Every GetHostInfo error branch writes both conditions False/NoSignedAnswer
// in the same status write as the error.
func TestHostInventory_ErrorPathWithdraws(t *testing.T) {
	for name, err := range map[string]error{
		"not supported": contracts.NewNotSupportedError("no GetHostInfo"),
		"not found":     contracts.NewNotFoundError("unknown host", nil),
		"transport":     contracts.NewRetryableError("connection refused", nil),
	} {
		t.Run(name, func(t *testing.T) {
			f := newInvFixture(t, testPolicy(t))
			require.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, f.reconcile(t).Status.Health)
			f.signed.hostErr = err
			h := f.reconcile(t)
			assert.NotEqual(t, infravirtrigaudiov1beta1.HostHealthReady, h.Status.Health)
			requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
				infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
			requireCond(t, h, infravirtrigaudiov1beta1.HostConditionComponentsReady, metav1.ConditionFalse,
				infravirtrigaudiov1beta1.HostReasonComponentsInventoryNotVerified)
		})
	}
	// A first heartbeat that fails sets them too.
	f := newInvFixture(t, testPolicy(t))
	f.signed.hostErr = contracts.NewRetryableError("connection refused", nil)
	requireCond(t, f.reconcile(t), infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
}

// An answer whose inventory does not echo the version the manager sent
// (the relay changed the request) is no answer, and the very next heartbeat
// asks for the inventory again: no backoff.
func TestHostInventory_EchoMismatchNoBackoff(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.inventory = func(q contracts.HostAnswerQuery) *providerv1.HostInventory {
		relayed := q
		relayed.InventoryVersion = q.InventoryVersion + 1 // what the agent saw
		inv := goodInventory(relayed)
		inv.InventoryVersion, inv.SupportedVersion = 1, 1
		return inv
	}
	h := f.reconcile(t)
	requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
	requireCond(t, h, HostConditionAgentAnswer, metav1.ConditionFalse, reasonHostAnswerMissing)
	f.clock.advance(time.Second)
	f.reconcile(t)
	assert.EqualValues(t, 1, f.lastQuery(t).InventoryVersion, "the next heartbeat asks again")
}

// An inventory version that is not min(requested, supported) is the agent's
// fault: no answer, and a backoff.
func TestHostInventory_NotMinimumBacksOff(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.rawSign = true
	f.signed.inventory = func(q contracts.HostAnswerQuery) *providerv1.HostInventory {
		inv := goodInventory(q)
		inv.SupportedVersion, inv.InventoryVersion = 2, 2
		return inv
	}
	requireCond(t, f.reconcile(t), infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryInvalid)
	f.reconcile(t)
	assert.Zero(t, f.lastQuery(t).InventoryVersion, "the inventory is not asked for during the backoff")
}

// An inventory that breaks a field rule starts a backoff of 1, 2, 4, 5, 5
// minutes during which the inventory is not asked for; the rest of the host
// answer keeps working (the host answer condition is True and new operations
// may be signed); the host stays not ready with InventoryInvalid; a valid
// inventory after the backoff ends it.
func TestHostInventory_InvalidBackoff(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.rawSign = true
	f.signed.inventory = func(q contracts.HostAnswerQuery) *providerv1.HostInventory {
		inv := goodInventory(q)
		inv.Features[0].Name = "unlisted_probe"
		return inv
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, d := range want {
		h := f.reconcile(t)
		require.EqualValues(t, 1, f.lastQuery(t).InventoryVersion, "fault %d: asked", i)
		requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
			infravirtrigaudiov1beta1.HostReasonInventoryInvalid)
		assert.Equal(t, infravirtrigaudiov1beta1.HostHealthNotReady, h.Status.Health)
		// Just before the backoff ends: not asked; the answer without the
		// inventory is valid for everything else.
		f.clock.advance(d - time.Second)
		h = f.reconcile(t)
		require.Zero(t, f.lastQuery(t).InventoryVersion, "fault %d: not asked %s after it", i, d-time.Second)
		requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
			infravirtrigaudiov1beta1.HostReasonInventoryInvalid)
		requireCond(t, h, HostConditionAgentAnswer, metav1.ConditionTrue, reasonHostAnswerVerified)
		_, gerr := f.r.HostAnswers.grantReady(f.provider, "host-alpha", f.signed.host.trust.Enrolment.EnrolmentID)
		require.NoError(t, gerr, "new operations may still be signed")
		f.clock.advance(time.Second)
	}
	// The agent is fixed: the next heartbeat asks, verifies, and the host is
	// ready again; the backoff is over.
	f.signed.inventory = goodInventory
	h := f.reconcile(t)
	assert.EqualValues(t, 1, f.lastQuery(t).InventoryVersion)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, h.Status.Health)
	f.reconcile(t)
	assert.EqualValues(t, 1, f.lastQuery(t).InventoryVersion)
}

// A new agent version during the backoff ends it: the next heartbeat asks.
func TestHostInventory_BackoffResetOnAgentVersion(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.rawSign = true
	f.signed.inventory = func(q contracts.HostAnswerQuery) *providerv1.HostInventory {
		inv := goodInventory(q)
		inv.Components[0].StateReason = "not_a_reason"
		return inv
	}
	f.reconcile(t)
	f.reconcile(t) // in the backoff: records the agent version
	require.Zero(t, f.lastQuery(t).InventoryVersion)
	f.signed.agentVersion = "fixed"
	f.signed.inventory = goodInventory
	f.reconcile(t) // still not asked; sees the new version
	require.Zero(t, f.lastQuery(t).InventoryVersion)
	h := f.reconcile(t)
	assert.EqualValues(t, 1, f.lastQuery(t).InventoryVersion, "a new agent version ends the backoff")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, h.Status.Health)
}

// The inventory is always asked for; an answer without it (an agent that
// ignores the request) is still a valid answer for everything else: the
// host answer condition, new operations and store continuity work, while
// the host is not ready (InventoryMissing).
func TestHostInventory_AnswerWithoutInventory(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.inventory = nil
	h := f.reconcile(t)
	assert.EqualValues(t, 1, f.lastQuery(t).InventoryVersion)
	requireCond(t, h, HostConditionAgentAnswer, metav1.ConditionTrue, reasonHostAnswerVerified)
	_, gerr := f.r.HostAnswers.grantReady(f.provider, "host-alpha", f.signed.host.trust.Enrolment.EnrolmentID)
	require.NoError(t, gerr)
	requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryMissing)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthNotReady, h.Status.Health)
	// Store continuity still holds: a rolled-back store is caught.
	for range 4 {
		f.clock.advance(time.Second)
		f.reconcile(t)
	}
	f.signed.host.rollBack(3)
	h = f.reconcile(t)
	requireCond(t, h, HostConditionAgentStoreRegressed, metav1.ConditionTrue, reasonHostStoreRegressed)
}

// The status mirror is an output only: editing it changes no decision.
func TestHostInventory_StatusMirrorNeverRead(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	h := f.reconcile(t)
	require.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, h.Status.Health)
	// Someone with hosts/status access makes the mirror look broken: the
	// next heartbeat decides from the verified answer, not from it.
	h.Status.AgentInventory = &infravirtrigaudiov1beta1.HostAgentInventoryStatus{State: "Pending"}
	require.NoError(t, f.r.Status().Update(context.Background(), h))
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthReady, h.Status.Health)
	assert.Equal(t, "Complete", h.Status.AgentInventory.State, "the mirror is rewritten from the verified answer")
	// And making it look good does not make a host without a verified
	// inventory ready.
	f.signed.unsigned = true
	h.Status.AgentInventory = &infravirtrigaudiov1beta1.HostAgentInventoryStatus{State: "Complete"}
	require.NoError(t, f.r.Status().Update(context.Background(), h))
	h = f.reconcile(t)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthNotReady, h.Status.Health)
}

// The Provider's unsigned health can only make the host not ready.
func TestHostInventory_UnsignedHealthOnlyRestricts(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.hostHealth = contracts.HostHealthNotReady
	h := f.reconcile(t)
	requireCond(t, h, infravirtrigaudiov1beta1.HostConditionComponentsReady, metav1.ConditionTrue,
		infravirtrigaudiov1beta1.HostReasonComponentsVerified)
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthNotReady, h.Status.Health)
	requireCond(t, h, k8s.ConditionReady, metav1.ConditionFalse, reasonHostNotReady)
}

// A digest that is not allow-listed makes the host not schedulable with
// ComponentsReady=False/DigestNotAllowListed.
func TestHostInventory_DigestNotAllowListed(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.inventory = func(q contracts.HostAnswerQuery) *providerv1.HostInventory {
		inv := goodInventory(q)
		inv.Components[0].Files[1].Sha256 = invDigest("tampered")
		return inv
	}
	h := f.reconcile(t)
	c := requireCond(t, h, infravirtrigaudiov1beta1.HostConditionComponentsReady, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed)
	assert.Contains(t, c.Message, "cloud-hypervisor")
	assert.Equal(t, infravirtrigaudiov1beta1.HostHealthNotReady, h.Status.Health)
	assert.Equal(t, infravirtrigaudiov1beta1.HostInventoryAllowListNotAllowed, h.Status.AgentInventory.Components[0].AllowList)
}

// verifiedRefresh is a verified host read carrying inv, verified at at.
func verifiedRefresh(inv *providerv1.HostInventory, at time.Time) *hostRefresh {
	return &hostRefresh{verdict: answerVerified, asked: 1, verifiedAt: at, answer: hostidentity.VerifiedAnswer{
		Context: &providerv1.AnswerContext{AgentState: providerv1.AgentState_AGENT_STATE_READY},
		Host:    &providerv1.HostAnswer{Inventory: inv},
	}}
}

// TestEvaluateHostInventory_Verified: each AgentInventoryVerified reason, and
// the effective age as the agent's collection age plus the time since the
// manager verified the answer.
func TestEvaluateHostInventory_Verified(t *testing.T) {
	now := time.Now()
	p := testPolicy(t)
	q := contracts.HostAnswerQuery{InventoryVersion: 1}
	pending := &providerv1.HostInventory{InventoryVersion: 1, State: providerv1.InventoryState_INVENTORY_STATE_PENDING, RequestedVersion: 1, SupportedVersion: 1}
	stale := goodInventory(q)
	stale.SnapshotAgeSeconds = 200
	for name, tc := range map[string]struct {
		obs  inventoryObservation
		want string
	}{
		"no signed read":   {obs: inventoryObservation{inScope: true, noAnswer: "x"}, want: infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer},
		"refused answer":   {obs: inventoryObservation{inScope: true, refresh: &hostRefresh{verdict: answerNone}}, want: infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer},
		"store regressed":  {obs: inventoryObservation{inScope: true, refresh: &hostRefresh{verdict: answerRegressed}}, want: infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer},
		"invalid":          {obs: inventoryObservation{inScope: true, refresh: &hostRefresh{verdict: answerNone, inventoryBackoff: true}}, want: infravirtrigaudiov1beta1.HostReasonInventoryInvalid},
		"missing":          {obs: inventoryObservation{inScope: true, refresh: verifiedRefresh(nil, now)}, want: infravirtrigaudiov1beta1.HostReasonInventoryMissing},
		"pending":          {obs: inventoryObservation{inScope: true, refresh: verifiedRefresh(pending, now)}, want: infravirtrigaudiov1beta1.HostReasonInventoryPending},
		"over limit":       {obs: inventoryObservation{inScope: true, refresh: verifiedRefresh(overLimit(), now)}, want: infravirtrigaudiov1beta1.HostReasonInventoryOverLimit},
		"unreadable":       {obs: inventoryObservation{inScope: true, refresh: verifiedRefresh(unreadable(), now)}, want: infravirtrigaudiov1beta1.HostReasonInventoryUnreadable},
		"summed age stale": {obs: inventoryObservation{inScope: true, refresh: verifiedRefresh(stale, now.Add(-101*time.Second))}, want: infravirtrigaudiov1beta1.HostReasonInventoryStale},
		"summed age fresh": {obs: inventoryObservation{inScope: true, refresh: verifiedRefresh(stale, now.Add(-99*time.Second))}, want: infravirtrigaudiov1beta1.HostReasonInventoryVerified},
	} {
		t.Run(name, func(t *testing.T) {
			v := evaluateHostInventory(p, tc.obs, now)
			assert.Equal(t, tc.want, v.verified.reason, v.verified.message)
			if tc.want != infravirtrigaudiov1beta1.HostReasonInventoryVerified {
				assert.Equal(t, metav1.ConditionFalse, v.verified.status)
				assert.Equal(t, infravirtrigaudiov1beta1.HostReasonComponentsInventoryNotVerified, v.components.reason)
				assert.False(t, v.ready())
			}
		})
	}
}

func overLimit() *providerv1.HostInventory {
	return &providerv1.HostInventory{InventoryVersion: 1, State: providerv1.InventoryState_INVENTORY_STATE_OVER_LIMIT,
		StateReason: "too_many_components", RequestedVersion: 1, SupportedVersion: 1}
}

func unreadable() *providerv1.HostInventory {
	return &providerv1.HostInventory{InventoryVersion: 1, State: providerv1.InventoryState_INVENTORY_STATE_UNREADABLE,
		StateReason: "io_error", RequestedVersion: 1, SupportedVersion: 1}
}

// TestEvaluateHostInventory_Components: each ComponentsReady reason, in the
// documented order, and the allow-list results.
func TestEvaluateHostInventory_Components(t *testing.T) {
	now := time.Now()
	q := contracts.HostAnswerQuery{InventoryVersion: 1}
	comp := func(i *providerv1.HostInventory) *providerv1.HostComponent { return i.Components[0] }
	extra := func(state providerv1.ComponentState, reason string, origin providerv1.ManifestOrigin) func(*providerv1.HostInventory) {
		return func(i *providerv1.HostInventory) {
			c := &providerv1.HostComponent{Name: "firmware", State: state, StateReason: reason, ManifestOrigin: origin}
			if origin != providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE {
				c.DeclaredVersion = "2"
			}
			i.Components = append(i.Components, c)
		}
	}
	for name, tc := range map[string]struct {
		policy  func(*HostInventoryPolicy)
		edit    func(*providerv1.HostInventory)
		ctx     func(*providerv1.AnswerContext)
		status  metav1.ConditionStatus
		reason  string
		allowed infravirtrigaudiov1beta1.HostInventoryAllowList
	}{
		"verified": {status: metav1.ConditionTrue, reason: infravirtrigaudiov1beta1.HostReasonComponentsVerified,
			allowed: infravirtrigaudiov1beta1.HostInventoryAllowListAllowed},
		"agent not ready": {ctx: func(c *providerv1.AnswerContext) {
			c.AgentState, c.StateReason = providerv1.AgentState_AGENT_STATE_QUARANTINED, "rolled_back"
		}, reason: infravirtrigaudiov1beta1.HostReasonComponentsAgentNotReady},
		"unexpected extension": {edit: func(i *providerv1.HostInventory) {
			i.UnexpectedExtensions = []*providerv1.HostExtension{{Name: "debug", Kind: providerv1.ExtensionKind_EXTENSION_KIND_DIRECTORY}}
		}, reason: infravirtrigaudiov1beta1.HostReasonComponentsUnexpectedExtension},
		"unmanifested": {edit: extra(providerv1.ComponentState_COMPONENT_STATE_UNMANIFESTED, "present_undeclared",
			providerv1.ManifestOrigin_MANIFEST_ORIGIN_NONE), reason: infravirtrigaudiov1beta1.HostReasonComponentsUnmanifested},
		"manifest invalid": {edit: extra(providerv1.ComponentState_COMPONENT_STATE_MANIFEST_INVALID, "parse_error",
			providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE), reason: infravirtrigaudiov1beta1.HostReasonComponentsManifestInvalid},
		"manifest origin not accepted": {policy: func(p *HostInventoryPolicy) {
			p.AcceptedOrigins = []providerv1.ManifestOrigin{providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY}
		}, edit: func(i *providerv1.HostInventory) {
			comp(i).ManifestOrigin = providerv1.ManifestOrigin_MANIFEST_ORIGIN_WRITABLE
		},
			reason: infravirtrigaudiov1beta1.HostReasonComponentsManifestOriginNotAccepted},
		"missing": {edit: extra(providerv1.ComponentState_COMPONENT_STATE_MISSING, "not_installed", providerv1.ManifestOrigin_MANIFEST_ORIGIN_READ_ONLY),
			reason: infravirtrigaudiov1beta1.HostReasonComponentsMissing},
		"incompatible": {edit: func(i *providerv1.HostInventory) {
			comp(i).State, comp(i).StateReason, comp(i).RequiredRange = providerv1.ComponentState_COMPONENT_STATE_INCOMPATIBLE, "version_out_of_range", ">=2"
		}, reason: infravirtrigaudiov1beta1.HostReasonComponentsIncompatible},
		"unhealthy": {edit: func(i *providerv1.HostInventory) {
			comp(i).State, comp(i).StateReason = providerv1.ComponentState_COMPONENT_STATE_UNHEALTHY, "artifact_digest_mismatch"
		}, reason: infravirtrigaudiov1beta1.HostReasonComponentsUnhealthy},
		"no allow-list fails closed": {policy: func(p *HostInventoryPolicy) { p.AllowList = nil },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsAllowListNotConfigured, allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotChecked},
		"allow-list not in force": {policy: func(p *HostInventoryPolicy) { p.AllowList, p.AllowListErr = nil, errAllowListNotLoaded },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsAllowListUnverified},
		"insecure flag": {policy: func(p *HostInventoryPolicy) { p.AllowList, p.SkipAllowList = nil, true },
			status: metav1.ConditionTrue, reason: infravirtrigaudiov1beta1.HostReasonComponentsAllowListNotConfigured,
			allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotChecked},
		"artifact differs": {edit: func(i *providerv1.HostInventory) { comp(i).ArtifactSha256 = invDigest("other") },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed, allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotAllowed},
		"one file digest differs": {edit: func(i *providerv1.HostInventory) { comp(i).Files[0].Sha256 = invDigest("other") },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed, allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotAllowed},
		"extra file name": {edit: func(i *providerv1.HostInventory) {
			comp(i).Files = append(comp(i).Files, &providerv1.ComponentFile{Name: "z-extra", Sha256: invDigest("z")})
		}, reason: infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed, allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotAllowed},
		"missing file name": {edit: func(i *providerv1.HostInventory) { comp(i).Files = comp(i).Files[:1] },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed, allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotAllowed},
		"not listed": {edit: func(i *providerv1.HostInventory) { comp(i).ObservedVersion = "1.0.1" },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed, allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotListed},
		"digests not reported": {edit: func(i *providerv1.HostInventory) { comp(i).ArtifactSha256, comp(i).Files = nil, nil },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsDigestsNotReported, allowed: infravirtrigaudiov1beta1.HostInventoryAllowListNotChecked},
		"renamed component is not listed": {edit: func(i *providerv1.HostInventory) { comp(i).Name = "hypervisor" },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsDigestNotAllowListed},
		"feature missing": {edit: func(i *providerv1.HostInventory) {
			i.Features[0].Result = providerv1.FeatureResult_FEATURE_RESULT_ABSENT
		},
			reason: infravirtrigaudiov1beta1.HostReasonComponentsFeatureMissing},
		"feature not reported": {edit: func(i *providerv1.HostInventory) { i.Features = i.Features[1:] },
			reason: infravirtrigaudiov1beta1.HostReasonComponentsFeatureMissing},
		"identity tier not accepted": {policy: func(p *HostInventoryPolicy) {
			p.AcceptedTiers = []providerv1.ProcessIdentityTier{providerv1.ProcessIdentityTier_PROCESS_IDENTITY_TIER_PIDFD_STORE}
		}, reason: infravirtrigaudiov1beta1.HostReasonComponentsIdentityTierNotAccepted},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPolicy(t)
			if tc.policy != nil {
				tc.policy(p)
			}
			inv := goodInventory(q)
			if tc.edit != nil {
				tc.edit(inv)
			}
			require.NoError(t, hostidentity.VerifyInventory(inv, 1), "the fixture is a valid inventory")
			r := verifiedRefresh(inv, now)
			if tc.ctx != nil {
				tc.ctx(r.answer.Context)
			}
			v := evaluateHostInventory(p, inventoryObservation{inScope: true, refresh: r}, now)
			require.Equal(t, metav1.ConditionTrue, v.verified.status)
			status := tc.status
			if status == "" {
				status = metav1.ConditionFalse
			}
			assert.Equal(t, status, v.components.status, v.components.message)
			assert.Equal(t, tc.reason, v.components.reason, v.components.message)
			assert.Equal(t, status == metav1.ConditionTrue, v.ready())
			if tc.allowed != "" {
				require.NotNil(t, v.mirror)
				assert.Equal(t, tc.allowed, v.mirror.Components[0].AllowList)
			}
			if p.AllowList != nil {
				assert.Contains(t, v.components.message, p.AllowList.SHA256())
			}
		})
	}
}

// A required component reported in the right version but not ready is
// RequiredComponentAbsent once every component check before it passes.
func TestEvaluateHostInventory_RequiredAbsent(t *testing.T) {
	now := time.Now()
	p := testPolicy(t)
	hx := "sha256:" + hex.EncodeToString(invDigest("fw2"))
	a, err := hostinventory.Parse([]byte(fmt.Sprintf(`{"version":1,"required":["cloud-hypervisor","hypervisor-fw"],"components":[
		{"name":"cloud-hypervisor","version":"1.0.0","alternatives":[{"artifactDigest":%q,"files":[
		{"name":"cloud-hypervisor","digest":%q},{"name":"hypervisor-fw","digest":%q}]}]},
		{"name":"hypervisor-fw","version":"2","alternatives":[{"artifactDigest":"none","files":[{"name":"fw","digest":%q}]}]}]}`,
		"sha256:"+hex.EncodeToString(invDigest("image")), "sha256:"+hex.EncodeToString(invDigest("ch")),
		"sha256:"+hex.EncodeToString(invDigest("fw")), hx)))
	require.NoError(t, err)
	p.AllowList = a
	v := evaluateHostInventory(p, inventoryObservation{inScope: true, refresh: verifiedRefresh(goodInventory(contracts.HostAnswerQuery{InventoryVersion: 1}), now)}, now)
	assert.Equal(t, infravirtrigaudiov1beta1.HostReasonComponentsRequiredAbsent, v.components.reason, v.components.message)
	assert.Contains(t, v.components.message, "hypervisor-fw")
}

// The audit decision: a transition is recorded when the condition's last
// Event is at least a minute old; transitions within the window are counted
// and reported once the window ends; each condition has its own window.
func TestInventoryAuditObserve(t *testing.T) {
	var a inventoryAudit
	t0 := time.Now()
	require.True(t, a.observe("ns/h", "A", "R1", t0).transition)
	assert.Equal(t, eventDecision{}, a.observe("ns/h", "A", "R1", t0.Add(2*time.Minute)), "no change: nothing")
	assert.True(t, a.observe("ns/h", "B", "R2", t0.Add(time.Second)).transition, "each condition has its own window")
	assert.True(t, a.observe("ns/other", "A", "R2", t0.Add(time.Second)).transition, "each host has its own state")
	// Recover and fail again within the window of A's next Event.
	require.True(t, a.observe("ns/h", "A", "R2", t0.Add(3*time.Minute)).transition)
	assert.False(t, a.observe("ns/h", "A", "R1", t0.Add(3*time.Minute+10*time.Second)).transition)
	assert.False(t, a.observe("ns/h", "A", "R2", t0.Add(3*time.Minute+20*time.Second)).transition)
	d := a.observe("ns/h", "A", "R2", t0.Add(4*time.Minute+time.Second))
	assert.True(t, d.summary, "the window ended with suppressed transitions")
	assert.Equal(t, 2, d.suppressed)
	assert.Equal(t, []string{"R1", "R2"}, d.reasons)
	assert.Equal(t, eventDecision{}, a.observe("ns/h", "A", "R2", t0.Add(5*time.Minute)), "reported once")
	// A transition after the window carries the suppressed ones with it.
	require.Equal(t, eventDecision{}, a.observe("ns/h", "B", "R3", t0.Add(10*time.Second)))
	d = a.observe("ns/h", "B", "R4", t0.Add(2*time.Minute))
	assert.True(t, d.transition)
	assert.Equal(t, 1, d.suppressed)
	a.forget("ns/h")
	assert.True(t, a.observe("ns/h", "A", "R2", t0.Add(5*time.Minute)).transition, "a forgotten host starts afresh")
}

// Two outages inside one Event window are both visible in the Events: the
// summary recorded when the window ends lists every suppressed transition.
func TestHostInventory_TwoOutagesInOneWindow(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.reconcile(t)
	require.Len(t, drainEvents(f.events), 2, "one Event per condition")
	for _, unsigned := range []bool{true, false, true, false} {
		f.clock.advance(5 * time.Second)
		f.signed.unsigned = unsigned
		f.reconcile(t)
	}
	assert.Empty(t, drainEvents(f.events), "within the window")
	f.clock.advance(time.Minute)
	f.reconcile(t)
	events := drainEvents(f.events)
	require.Len(t, events, 2)
	for _, e := range events {
		assert.Contains(t, e, reasonInventoryTransitionsSuppressed)
		assert.Contains(t, e, "4 transition(s)")
	}
	assert.Equal(t, 2, strings.Count(events[0]+events[1], infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer+" "),
		"the AgentInventoryVerified summary names both outages: %v", events)
}

// A flapping Provider does not get the whole signed answer logged on every
// transition: once per answer and at most once per host per interval.
func TestHostInventory_SignedAnswerLoggedRarely(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	var mu sync.Mutex
	blobs, lines := 0, 0
	logger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(args, "Host inventory condition changed") || strings.Contains(args, `"condition"`) {
			lines++
		}
		if strings.Contains(args, `"signedAnswer"`) {
			blobs++
		}
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), logger)
	reconcile := func() {
		_, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"}})
		require.NoError(t, err)
	}
	reconcile()
	for i := 0; i < 10; i++ {
		f.clock.advance(15 * time.Second)
		f.signed.unsigned = i%2 == 0
		reconcile()
	}
	mu.Lock()
	assert.Equal(t, 1, blobs, "the whole signed answer is logged once")
	assert.Greater(t, lines, 10, "every transition is still logged, with the answer's SHA-256")
	mu.Unlock()
	f.clock.advance(inventoryBlobLogInterval)
	f.signed.unsigned = true
	reconcile()
	f.signed.unsigned = false
	reconcile()
	mu.Lock()
	assert.Equal(t, 2, blobs, "a new answer after the interval is logged in full")
	mu.Unlock()
}

// The metric carries one series per (host, condition) with the current
// reason from the closed set, and is dropped with the Host.
func TestHostInventory_ConditionMetric(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	// The registry is process-wide: start from no series of this host.
	f.r.forgetHostInventory(types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"})
	f.reconcile(t)
	val := func(cond, reason string) float64 {
		v, ok := inventoryMetric(t, "host-alpha", cond, reason)
		require.True(t, ok, "series %s/%s", cond, reason)
		return v
	}
	assert.Equal(t, 1.0, val(infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, infravirtrigaudiov1beta1.HostReasonInventoryVerified))
	f.signed.unsigned = true
	f.clock.advance(time.Second)
	f.reconcile(t)
	assert.Equal(t, 0.0, val(infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer))
	assert.Equal(t, 2, seriesOf(t, "host-alpha"), "one series per condition: the previous reason's was replaced")
	assert.Equal(t, metricReasonOther, metricReason("SomethingElse"))
	f.r.forgetHostInventory(types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"})
	assert.Zero(t, seriesOf(t, "host-alpha"))
}

// setComponentAllowListReadiness: a Provider whose type runs a per-host
// agent is Ready=False while a configured allow-list is not in force; other
// Providers and a manager whose allow-list is fine are untouched.
func TestSetComponentAllowListReadiness(t *testing.T) {
	ch := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor}}
	lv := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeLibvirt}}
	digestErr := &HostInventoryPolicy{AllowListErr: fmt.Errorf("%w: %w", errAllowListNotLoaded, hostinventory.ErrDigestMismatch)}
	sigErr := &HostInventoryPolicy{AllowListErr: fmt.Errorf("%w: %w", errAllowListNotLoaded, hostinventory.ErrUnverified)}
	for name, tc := range map[string]struct {
		p      *infravirtrigaudiov1beta1.Provider
		policy *HostInventoryPolicy
		want   string
	}{
		"digest mismatch":    {p: ch, policy: digestErr, want: k8s.ReasonAllowListDigestMismatch},
		"signature":          {p: ch, policy: sigErr, want: k8s.ReasonAllowListUnverified},
		"in force":           {p: ch, policy: testPolicy(t)},
		"no policy":          {p: ch},
		"other type ignored": {p: lv, policy: digestErr},
	} {
		t.Run(name, func(t *testing.T) {
			p := tc.p.DeepCopy()
			setComponentAllowListReadiness(p, tc.policy)
			c := meta.FindStatusCondition(p.Status.Conditions, k8s.ConditionReady)
			if tc.want == "" {
				assert.Nil(t, c)
				return
			}
			require.NotNil(t, c)
			assert.Equal(t, metav1.ConditionFalse, c.Status)
			assert.Equal(t, tc.want, c.Reason)
		})
	}
}

// The status mirror carries no free-form text the host could choose beyond
// the bounded fields of the verified inventory: an agent cannot put a path or
// a secret-looking string into status, Events or the conditions, because the
// verifier refuses every field outside its shape. (That no VM-scoped value
// is ever put into a field whose shape allows it is the agent's rule.)
func TestHostInventory_RedactionCanary(t *testing.T) {
	canaries := []string{"/etc/shadow", "s3cr3t=hunter2", "token: abc"}
	f := newInvFixture(t, testPolicy(t))
	f.signed.rawSign = true
	for _, canary := range canaries {
		for _, edit := range []func(*providerv1.HostInventory){
			func(i *providerv1.HostInventory) { i.Components[0].Source = canary },
			func(i *providerv1.HostInventory) { i.Components[0].ObservedVersion = canary },
			func(i *providerv1.HostInventory) { i.Components[0].Files[0].Name = canary },
			func(i *providerv1.HostInventory) { i.Components[0].StateReason = canary },
			func(i *providerv1.HostInventory) { i.Features[0].Reason = canary },
		} {
			f.signed.inventory = func(q contracts.HostAnswerQuery) *providerv1.HostInventory {
				inv := goodInventory(q)
				edit(inv)
				return inv
			}
			f.r.HostAnswers.imu.Lock()
			f.r.HostAnswers.invBackoff = nil
			f.r.HostAnswers.imu.Unlock()
			h := f.reconcile(t)
			text := fmt.Sprintf("%+v", h.Status)
			assert.NotContains(t, text, canary)
		}
	}
	close(f.events.Events)
	for e := range f.events.Events {
		for _, canary := range canaries {
			assert.False(t, strings.Contains(e, canary), "Event %q carries %q", e, canary)
		}
	}
}

// inventoryMetrics gathers the host inventory condition series of host.
func inventoryMetrics(t *testing.T, host string) []*dto.Metric {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	var out []*dto.Metric
	for _, mf := range mfs {
		if mf.GetName() != "virtrigaud_host_inventory_condition" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "host" && l.GetValue() == host {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// seriesOf counts the host inventory condition series of host.
func seriesOf(t *testing.T, host string) int { return len(inventoryMetrics(t, host)) }

// inventoryMetric returns the value of host's series for condition and reason.
func inventoryMetric(t *testing.T, host, condition, reason string) (float64, bool) {
	for _, m := range inventoryMetrics(t, host) {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["condition"] == condition && labels["reason"] == reason {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// An inventory that lists no component is not ready, whatever the
// allow-list requires.
func TestEvaluateHostInventory_NoComponents(t *testing.T) {
	now := time.Now()
	inv := goodInventory(contracts.HostAnswerQuery{InventoryVersion: 1})
	inv.Components = nil
	require.NoError(t, hostidentity.VerifyInventory(inv, 1))
	for name, p := range map[string]*HostInventoryPolicy{
		"with an allow-list": testPolicy(t),
		"insecure flag": func() *HostInventoryPolicy {
			p := DefaultHostInventoryPolicy()
			p.SkipAllowList = true
			return p
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			v := evaluateHostInventory(p, inventoryObservation{inScope: true, refresh: verifiedRefresh(inv, now)}, now)
			assert.Equal(t, metav1.ConditionFalse, v.components.status)
			assert.Equal(t, infravirtrigaudiov1beta1.HostReasonComponentsNoneReported, v.components.reason)
		})
	}
}

// The mirror is dropped when the host's enrolment changes before a new
// inventory verifies, and the mirror and conditions are removed when the
// host's Provider no longer runs a per-host agent.
func TestHostInventory_MirrorClearedOnEnrolmentChangeAndLeaving(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	h := f.reconcile(t)
	require.NotNil(t, h.Status.AgentInventory)
	assert.Equal(t, h.Status.Agent.EnrolmentID, h.Status.AgentInventory.EnrolmentID)
	// A failed heartbeat under the same enrolment keeps the last verified
	// inventory (its observedTime says how old it is).
	f.signed.unsigned = true
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	require.NotNil(t, h.Status.AgentInventory)
	// The host is re-enrolled: the old enrolment's inventory goes.
	h.Status.Agent.EnrolmentID = strings.Repeat("9", 32)
	require.NoError(t, f.r.Status().Update(context.Background(), h))
	f.clock.advance(time.Second)
	h = f.reconcile(t)
	assert.Nil(t, h.Status.AgentInventory)
	// So does an error heartbeat.
	h.Status.AgentInventory = &infravirtrigaudiov1beta1.HostAgentInventoryStatus{State: "Complete", EnrolmentID: strings.Repeat("1", 32)}
	f.r.withdrawHostInventory(context.Background(), h, true, "x")
	assert.Nil(t, h.Status.AgentInventory)

	// Leaving: a heartbeat of a Provider that runs no per-host agent.
	h.Status.AgentInventory = &infravirtrigaudiov1beta1.HostAgentInventoryStatus{State: "Complete"}
	f.r.applyHostInventory(context.Background(), h, inventoryObservation{})
	assert.Nil(t, h.Status.AgentInventory)
	assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified))
	assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionComponentsReady))
	// And an error heartbeat of such a Provider.
	h.Status.AgentInventory = &infravirtrigaudiov1beta1.HostAgentInventoryStatus{State: "Complete"}
	setHostCondition(h, infravirtrigaudiov1beta1.HostConditionComponentsReady, metav1.ConditionTrue, "x", "")
	lv := f.provider.DeepCopy()
	lv.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	base := h.DeepCopy()
	_, err := f.r.handleGetHostInfoError(context.Background(), h, base, lv, contracts.NewRetryableError("down", nil))
	require.NoError(t, err)
	assert.Nil(t, h.Status.AgentInventory)
	assert.Nil(t, meta.FindStatusCondition(h.Status.Conditions, infravirtrigaudiov1beta1.HostConditionComponentsReady))
}

// A deleted or vanished Host's backoff, audit state and metric series are
// dropped.
func TestHostInventory_ForgottenWithTheHost(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.signed.rawSign = true
	f.signed.inventory = func(q contracts.HostAnswerQuery) *providerv1.HostInventory {
		inv := goodInventory(q)
		inv.Features[0].Name = "unlisted_probe"
		return inv
	}
	f.reconcile(t)
	require.Zero(t, f.r.HostAnswers.inventoryAsk(f.provider, "host-alpha"), "in a backoff")
	require.NotZero(t, seriesOf(t, "host-alpha"))
	h := &infravirtrigaudiov1beta1.Host{}
	require.NoError(t, f.r.Get(context.Background(), types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"}, h))
	h.Finalizers = nil
	require.NoError(t, f.r.Update(context.Background(), h))
	require.NoError(t, f.r.Delete(context.Background(), h))
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"}})
	require.NoError(t, err)
	assert.EqualValues(t, hostidentity.InventoryVersion, f.r.HostAnswers.inventoryAsk(f.provider, "host-alpha"), "the backoff is gone")
	assert.Zero(t, seriesOf(t, "host-alpha"))
	f.r.inventoryEvents.mu.Lock()
	assert.Empty(t, f.r.inventoryEvents.hosts)
	f.r.inventoryEvents.mu.Unlock()
}

// The allow-list reason never hides an earlier Ready=False reason of the
// Provider: it keeps the first and adds the allow-list to the message.
func TestSetComponentAllowListReadiness_KeepsEarlierReason(t *testing.T) {
	p := &infravirtrigaudiov1beta1.Provider{Spec: infravirtrigaudiov1beta1.ProviderSpec{Type: infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor}}
	k8s.SetReadyCondition(&p.Status.Conditions, metav1.ConditionFalse, k8s.ReasonOperationSignerUnavailable, "no signer", 0)
	setComponentAllowListReadiness(p, &HostInventoryPolicy{AllowListErr: fmt.Errorf("%w: %w", errAllowListNotLoaded, hostinventory.ErrDigestMismatch)})
	c := meta.FindStatusCondition(p.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonOperationSignerUnavailable, c.Reason)
	assert.Contains(t, c.Message, "no signer")
	assert.Contains(t, c.Message, k8s.ReasonAllowListDigestMismatch)
}

// A heartbeat failure is audited like any other verdict: a transition log
// line, an Event, and the condition gauge flipped to NoSignedAnswer with 0;
// leaving a Provider that runs a per-host agent removes the series.
func TestHostInventory_WithdrawIsAudited(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	key := types.NamespacedName{Namespace: clusteredNS, Name: "host-alpha"}
	f.r.forgetHostInventory(key)
	f.reconcile(t)
	drainEvents(f.events)
	var mu sync.Mutex
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{}))
	f.clock.advance(time.Minute)
	f.signed.hostErr = contracts.NewRetryableError("connection refused", nil)
	_, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	mu.Lock()
	joined := strings.Join(lines, "\n")
	mu.Unlock()
	assert.Contains(t, joined, "Host inventory condition changed")
	assert.Contains(t, joined, infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
	events := drainEvents(f.events)
	require.NotEmpty(t, events)
	assert.Contains(t, strings.Join(events, "\n"), infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
	v, ok := inventoryMetric(t, "host-alpha", infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified,
		infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer)
	require.True(t, ok)
	assert.Zero(t, v)
	_, stale := inventoryMetric(t, "host-alpha", infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified,
		infravirtrigaudiov1beta1.HostReasonInventoryVerified)
	assert.False(t, stale, "the Verified series is gone")
	assert.Equal(t, 2, seriesOf(t, "host-alpha"))

	h := &infravirtrigaudiov1beta1.Host{}
	require.NoError(t, f.r.Get(context.Background(), key, h))
	f.r.applyHostInventory(context.Background(), h, inventoryObservation{})
	assert.Zero(t, seriesOf(t, "host-alpha"), "leaving removes the series")
}

// Whatever was published before (a status write that failed between two
// heartbeats included), a condition has exactly one series.
func TestHostInventory_OneSeriesPerCondition(t *testing.T) {
	ns, host := "metrics-ns", "host-series"
	metrics.DeleteHostInventoryConditions(ns, host)
	cond := infravirtrigaudiov1beta1.HostConditionComponentsReady
	metrics.SetHostInventoryCondition(ns, host, cond, "A", true)
	metrics.SetHostInventoryCondition(ns, host, cond, "B", false) // never saved
	metrics.SetHostInventoryCondition(ns, host, cond, "C", false)
	assert.Equal(t, 1, seriesOf(t, host))
	_, ok := inventoryMetric(t, host, cond, "C")
	assert.True(t, ok)
	metrics.DeleteHostInventoryConditions(ns, host)
}

// A condition whose status does not change keeps its transition time across
// heartbeats, although one sync sets Ready True from the Provider's answer
// and then False for the inventory.
func TestHostInventory_ReadyTransitionTimeStaysWhileFalse(t *testing.T) {
	f := newInvFixture(t, testPolicy(t))
	f.reconcile(t)
	f.signed.unsigned = true
	first := requireCond(t, f.reconcile(t), k8s.ConditionReady, metav1.ConditionFalse, reasonAgentInventoryNotReady).LastTransitionTime
	inv := requireCond(t, f.reconcile(t), infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer).LastTransitionTime
	time.Sleep(1100 * time.Millisecond) // transition times have second precision
	h := f.reconcile(t)
	ready := requireCond(t, h, k8s.ConditionReady, metav1.ConditionFalse, reasonAgentInventoryNotReady)
	assert.True(t, ready.LastTransitionTime.Equal(&first), "Ready stayed False: transition time %v, want %v", ready.LastTransitionTime, first)
	again := requireCond(t, h, infravirtrigaudiov1beta1.HostConditionAgentInventoryVerified, metav1.ConditionFalse,
		infravirtrigaudiov1beta1.HostReasonInventoryNoSignedAnswer).LastTransitionTime
	assert.True(t, again.Equal(&inv))
	// A real change still moves it.
	f.signed.unsigned = false
	time.Sleep(1100 * time.Millisecond)
	back := meta.FindStatusCondition(f.reconcile(t).Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, back)
	assert.Equal(t, metav1.ConditionTrue, back.Status)
	assert.True(t, back.LastTransitionTime.After(first.Time))
}

// keepTransitionTimes restores the read transition time only for a
// condition whose status is unchanged.
func TestKeepTransitionTimes(t *testing.T) {
	old := metav1.NewTime(time.Unix(1_700_000_000, 0))
	now := metav1.NewTime(time.Unix(1_700_000_100, 0))
	base := &infravirtrigaudiov1beta1.Host{Status: infravirtrigaudiov1beta1.HostStatus{Conditions: []metav1.Condition{
		{Type: "A", Status: metav1.ConditionFalse, LastTransitionTime: old},
		{Type: "B", Status: metav1.ConditionFalse, LastTransitionTime: old},
	}}}
	host := &infravirtrigaudiov1beta1.Host{Status: infravirtrigaudiov1beta1.HostStatus{Conditions: []metav1.Condition{
		{Type: "A", Status: metav1.ConditionFalse, Reason: "Other", LastTransitionTime: now},
		{Type: "B", Status: metav1.ConditionTrue, LastTransitionTime: now},
		{Type: "C", Status: metav1.ConditionTrue, LastTransitionTime: now},
	}}}
	keepTransitionTimes(host, base)
	assert.Equal(t, old, host.Status.Conditions[0].LastTransitionTime, "same status: kept")
	assert.Equal(t, now, host.Status.Conditions[1].LastTransitionTime, "status changed: new time")
	assert.Equal(t, now, host.Status.Conditions[2].LastTransitionTime, "new condition: new time")
}
