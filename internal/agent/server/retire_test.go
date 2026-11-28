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

package server_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/answer"
	"github.com/projectbeskar/virtrigaud/internal/agent/executor"
	"github.com/projectbeskar/virtrigaud/internal/agent/journal"
	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
	"github.com/projectbeskar/virtrigaud/internal/agent/server"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// retireGrant mints a retirement grant of o for session c.
func (e *authEnv) retireGrant(t *testing.T, o *op, c []byte, edit func(*operation.RetireMintRequest)) *providerv1.OperationGrant {
	t.Helper()
	r := operation.RetireMintRequest{Identity: o.id, IntentDigest: operation.IntentDigest(o.salt, o.request), AttestationID: c,
		IssuedAt: time.Now()}
	if edit != nil {
		edit(&r)
	}
	g, err := operation.MintRetireGrant(context.Background(), e.grants, r)
	require.NoError(t, err)
	return g
}

// retire delivers g on RetireIfUnknown and returns the verified answer.
func (e *authEnv) retire(t *testing.T, g *providerv1.OperationGrant) *providerv1.AuthorityAnswer {
	t.Helper()
	resp, err := e.ha.RetireIfUnknown(ctx(t), &providerv1.RetireIfUnknownRequest{TargetHostId: trusttest.HostName,
		AnswerNonce: nonce(0x36), Grant: g})
	require.NoError(t, err)
	a := &providerv1.AuthorityAnswer{}
	verifyBlob(t, e.opsEnv, resp.GetHostAnswer(), answer.BodyTypeAuthorityAnswer, a)
	require.Equal(t, nonce(0x36), a.GetContext().GetNonce())
	assert.Equal(t, int32(providerv1.GrantKind_GRANT_KIND_RETIRE_IF_UNKNOWN), a.GetGrantKind())
	return a
}

// An operation the agent never received is retired: APPLIED, the op_id
// echoed, state RETIRED, the session named; a later delivery of it never
// runs, and a second retirement is ALREADY_APPLIED.
func TestRetire_NeverDelivered(t *testing.T) {
	e := newAuthEnv(t)
	c := e.attestAt(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	a := e.retire(t, e.retireGrant(t, o, c, nil))
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")
	assert.Equal(t, o.id.OpID, a.GetOpId())
	assert.Equal(t, providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED, a.GetOpState())
	assert.Equal(t, c, a.GetContext().GetAttestationId())
	assert.Nil(t, a.GetMark(), "a retirement answer carries no mark")
	st := e.ledgerStatus(t, o)
	assert.Equal(t, providerv1.OperationRecordState_OPERATION_RECORD_STATE_RETIRED, st.GetState())
	assert.Equal(t, "never_received", st.GetOutcomeReason())
	_, _, _ = e.create(t, o, false)
	e.markTip(t)
	assert.Zero(t, e.exec.total(), "a retired operation never runs")
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, c, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_ALREADY_APPLIED, "")
}

// An operation the agent received is never retired: NOT_APPLICABLE with
// its state; with other identity fields, REFUSED op_known.
func TestRetire_ReceivedIsNotApplicable(t *testing.T) {
	e := newAuthEnv(t)
	c := e.attestAt(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	_, _, err := e.create(t, o, false)
	require.NoError(t, err)
	a := e.retire(t, e.retireGrant(t, o, c, nil))
	wantOutcome(t, a, providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_NOT_APPLICABLE, "")
	assert.Equal(t, providerv1.OperationRecordState_OPERATION_RECORD_STATE_ACCEPTED, a.GetOpState())
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, c, func(r *operation.RetireMintRequest) { r.Identity.Attempt = 5 })),
		providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED, answer.ReasonOpKnown)
}

// A retirement is refused for another session, a grant that does not
// verify, and an agent that is not attested; nothing is retired.
func TestRetire_Refusals(t *testing.T) {
	e := newAuthEnv(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, nonce(9), nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonNotAttested)
	c := e.attestAt(t)
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, nonce(9), nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonStaleAttestation)
	wantOutcome(t, e.retire(t, e.ledgerMarkGrant(t, c, e.store.Position(), nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonGrantInvalid)
	_, found, err := e.store.Lookup(ctx(t), opstore.OpRef{EnrolmentID: o.id.EnrolmentID, OpID: o.id.OpID})
	require.NoError(t, err)
	assert.False(t, found, "nothing was retired")
}

// A trace of the operation with no record quarantines the store and is
// REFUSED op_trace_found.
func TestRetire_TraceFound(t *testing.T) {
	e := newAuthEnv(t)
	c := e.attestAt(t)
	o := e.newOp(kOn, "vm-1", randID(), 1, powerReq("vm-1", providerv1.PowerOp_POWER_OP_ON))
	e.exec.mu.Lock()
	e.exec.effects[hex.EncodeToString(o.id.OpID)+"/power"] = 1
	e.exec.mu.Unlock()
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, c, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonOpTraceFound)
	st, reason := e.store.Journal().State()
	assert.Equal(t, journal.StateLost, st)
	assert.Equal(t, journal.LostUnknownOpTrace, reason)
}

// The per-Provider retirement limit is applied before the grant is
// verified or anything scanned: a call above it is RESOURCE_EXHAUSTED and
// decides nothing.
func TestRetire_PerProviderLimitBeforeWork(t *testing.T) {
	e := newAuthEnvWith(t, nil, func(o *server.Options) { o.RetiresPerSecond, o.RetireBurst = 0.001, 1 })
	c := e.attestAt(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, c, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_APPLIED, "")
	e.exec.mu.Lock()
	before := e.exec.traceCalls
	e.exec.mu.Unlock()
	other := e.newOp(kCreate, "vm-2", randID(), 1, createReq("vm-2", nil))
	_, err := e.ha.RetireIfUnknown(ctx(t), &providerv1.RetireIfUnknownRequest{TargetHostId: trusttest.HostName,
		AnswerNonce: nonce(0x37), Grant: e.retireGrant(t, other, c, nil)})
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "%v", err)
	e.exec.mu.Lock()
	after := e.exec.traceCalls
	e.exec.mu.Unlock()
	assert.Equal(t, before, after, "nothing was scanned above the limit")
	_, found, err := e.store.Lookup(ctx(t), opstore.OpRef{EnrolmentID: other.id.EnrolmentID, OpID: other.id.OpID})
	require.NoError(t, err)
	assert.False(t, found, "nothing was retired above the limit")
}

// noScanExec is an executor that cannot scan its objects for traces.
type noScanExec struct{ executor.Executor }

// An agent whose executor cannot scan for traces refuses a retirement with
// its own reason, trace_scan_unavailable, and retires nothing.
func TestRetire_TraceScanUnavailable(t *testing.T) {
	e := newAuthEnvWith(t, func(o *opstore.Options) { o.Executor = noScanExec{o.Executor} })
	c := e.attestAt(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, c, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonTraceScanUnavailable)
	_, found, err := e.store.Lookup(ctx(t), opstore.OpRef{EnrolmentID: o.id.EnrolmentID, OpID: o.id.OpID})
	require.NoError(t, err)
	assert.False(t, found, "nothing was retired")
}

// failingScanExec is an executor whose trace scan fails.
type failingScanExec struct{ executor.Executor }

func (failingScanExec) Traces(context.Context, executor.Operation) (executor.Trace, error) {
	return executor.Trace{}, errors.New("the scan failed")
}

// A trace scan that failed is store_busy (asked again soon), not
// trace_scan_unavailable, and retires nothing.
func TestRetire_TraceScanFailedIsStoreBusy(t *testing.T) {
	e := newAuthEnvWith(t, func(o *opstore.Options) { o.Executor = failingScanExec{o.Executor} })
	c := e.attestAt(t)
	o := e.newOp(kCreate, "vm-1", randID(), 1, createReq("vm-1", nil))
	wantOutcome(t, e.retire(t, e.retireGrant(t, o, c, nil)), providerv1.AuthorityOutcome_AUTHORITY_OUTCOME_REFUSED,
		answer.ReasonStoreBusy)
	_, found, err := e.store.Lookup(ctx(t), opstore.OpRef{EnrolmentID: o.id.EnrolmentID, OpID: o.id.OpID})
	require.NoError(t, err)
	assert.False(t, found, "nothing was retired")
}
