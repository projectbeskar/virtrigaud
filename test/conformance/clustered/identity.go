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

package clustered

import (
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// operationEnvelopeField is the name of the operation_envelope field of every
// mutating request message.
const operationEnvelopeField protoreflect.Name = "operation_envelope"

// SignedOp is one operation attempt the suite signs: what its grant names, its
// digest salt and the exact request bytes. A re-send of the same attempt
// reuses all of it with a freshly minted grant.
type SignedOp struct {
	// Identity is what the grant names.
	Identity operation.Identity
	// Salt is the attempt's digest salt.
	Salt []byte
	// Request is the exact serialised request.
	Request []byte
}

// SignedAnswer is a Provider's answer to a signed call.
type SignedAnswer struct {
	// TaskRef is the task reference, "" for a synchronous call.
	TaskRef string
	// ID is CreateResponse.id (Create only).
	ID string
}

// OperationIdentity returns how to sign operations for the Target. Only a
// case that needs FeatureOperationIdentity calls it; the probe made sure the
// Target implements OperationIdentityTarget.
func (e *Env) OperationIdentity() OperationIdentityConfig {
	e.T.Helper()
	it, ok := e.Target.(OperationIdentityTarget)
	require.True(e.T, ok, "the target offers operation identity, so it implements OperationIdentityTarget")
	cfg := it.OperationIdentity()
	require.NotNil(e.T, cfg.Signer, "the target supplies a signer")
	return cfg
}

// NewOp prepares attempt 1 of an operation of kind on host for req, owned by
// req's owner: a fresh op_id and salt, and req serialised once.
func (e *Env) NewOp(kind providerv1.OperationKind, host string, req proto.Message) *SignedOp {
	e.T.Helper()
	cfg := e.OperationIdentity()
	opID, err := operation.NewRandomID()
	require.NoError(e.T, err)
	binding, err := operation.NewRandomID()
	require.NoError(e.T, err)
	salt, err := operation.NewDigestSalt()
	require.NoError(e.T, err)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	require.NoError(e.T, err, "serialise the request")
	vmUID := ""
	if o, ok := req.(interface {
		GetOwner() *providerv1.ObjectIdentity
	}); ok {
		vmUID = o.GetOwner().GetUid()
	}
	return &SignedOp{
		Identity: operation.Identity{
			ProviderUID: cfg.ProviderUID,
			HostID:      host,
			EnrolmentID: cfg.Enrolments[host],
			Epoch:       cfg.Epoch,
			VMUID:       vmUID,
			BindingID:   binding,
			OpID:        opID,
			Kind:        kind,
			Attempt:     1,
			MintEpoch:   cfg.Epoch,
		},
		Salt:    salt,
		Request: b,
	}
}

// Grant mints a fresh grant for op.
func (e *Env) Grant(op *SignedOp) *providerv1.OperationGrant {
	e.T.Helper()
	g, err := operation.MintOperationGrant(e.ctx, e.OperationIdentity().Signer, operation.MintRequest{
		Identity:   op.Identity,
		DigestSalt: op.Salt,
		Request:    op.Request,
		IssuedAt:   time.Now(),
	})
	require.NoError(e.T, err, "mint the grant")
	return g
}

// Resign mints a grant for op, changes its body with mutate and signs the
// changed body again with the Target's signer: an authentic grant that says
// something other than op.
func (e *Env) Resign(op *SignedOp, mutate func(*providerv1.GrantBody)) *providerv1.OperationGrant {
	e.T.Helper()
	g := e.Grant(op)
	body := &providerv1.GrantBody{}
	require.NoError(e.T, proto.Unmarshal(g.GetBody(), body))
	mutate(body)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(e.T, err)
	sig, err := e.OperationIdentity().Signer.Sign(e.ctx, b, body.GetNotAfter().AsTime())
	require.NoError(e.T, err, "sign the changed body")
	return &providerv1.OperationGrant{Body: b, Signature: sig.Signature, SignerCertChain: sig.Chain}
}

// Envelope is op's request with a fresh grant.
func (e *Env) Envelope(op *SignedOp) *providerv1.OperationEnvelope {
	e.T.Helper()
	return &providerv1.OperationEnvelope{Request: op.Request, Grant: e.Grant(op)}
}

// SendSigned sends op, with a fresh grant, on the RPC its kind names, with no
// other field of the outer request set.
func (e *Env) SendSigned(op *SignedOp) (SignedAnswer, error) {
	e.T.Helper()
	return e.SendEnvelope(rpcOf(e, op.Identity.Kind), e.Envelope(op), nil)
}

// rpcOf is the full method a kind is sent on.
func rpcOf(e *Env, kind providerv1.OperationKind) string {
	e.T.Helper()
	row, ok := operation.RowFor(kind)
	require.True(e.T, ok, "kind %s has a row", kind)
	return row.FullMethod
}

// SendEnvelope sends env on the mutating RPC fullMethod (Create, Delete,
// Power or Reconfigure). outer, when not nil, sets further fields of the
// outer request beside the envelope.
func (e *Env) SendEnvelope(fullMethod string, env *providerv1.OperationEnvelope, outer func(proto.Message)) (SignedAnswer, error) {
	e.T.Helper()
	var req, resp proto.Message
	switch fullMethod {
	case providerv1.Provider_Create_FullMethodName:
		req, resp = &providerv1.CreateRequest{}, &providerv1.CreateResponse{}
	case providerv1.Provider_Delete_FullMethodName:
		req, resp = &providerv1.DeleteRequest{}, &providerv1.TaskResponse{}
	case providerv1.Provider_Power_FullMethodName:
		req, resp = &providerv1.PowerRequest{}, &providerv1.TaskResponse{}
	case providerv1.Provider_Reconfigure_FullMethodName:
		req, resp = &providerv1.ReconfigureRequest{}, &providerv1.TaskResponse{}
	default:
		require.Failf(e.T, "unsupported RPC", "the suite sends signed calls on Create, Delete, Power and Reconfigure, not %s", fullMethod)
	}
	r := req.ProtoReflect()
	fd := r.Descriptor().Fields().ByName(operationEnvelopeField)
	require.NotNil(e.T, fd, "%s has an operation_envelope field", r.Descriptor().FullName())
	r.Set(fd, protoreflect.ValueOfMessage(env.ProtoReflect()))
	if outer != nil {
		outer(req)
	}
	ctx, cancel := e.Call()
	defer cancel()
	if err := e.conn.Invoke(ctx, fullMethod, req, resp); err != nil {
		return SignedAnswer{}, err
	}
	switch a := resp.(type) {
	case *providerv1.CreateResponse:
		return SignedAnswer{ID: a.GetId(), TaskRef: a.GetTask().GetId()}, nil
	case *providerv1.TaskResponse:
		return SignedAnswer{TaskRef: a.GetTask().GetId()}, nil
	}
	return SignedAnswer{}, nil
}

// ScriptOutcome makes the next operation host accepts end with outcome
// (OutcomeScripter). Only a case that needs FeatureScriptedOutcomes calls it.
func (e *Env) ScriptOutcome(host string, outcome providerv1.TaskOutcome, reason string) {
	e.T.Helper()
	sc, ok := e.Target.(OutcomeScripter)
	require.True(e.T, ok, "the target offers scripted outcomes, so it implements OutcomeScripter")
	require.NoError(e.T, sc.ScriptOutcome(host, outcome, reason))
}

// FinishTasks completes the Target's tasks, when it has a TaskController.
func (e *Env) FinishTasks() {
	e.T.Helper()
	if ctl, ok := e.Target.(TaskController); ok {
		require.NoError(e.T, ctl.FinishTasks())
	}
}

// SettleSigned completes the task a signed call returned, if it returned one,
// and requires TaskStatus to report it done and SUCCEEDED.
func (e *Env) SettleSigned(a SignedAnswer, what string) {
	e.T.Helper()
	if a.TaskRef == "" {
		return
	}
	e.FinishTasks()
	st, err := e.TaskStatus(a.TaskRef)
	require.NoError(e.T, err, "%s: poll the task", what)
	require.True(e.T, st.GetDone(), "%s: the task is done once the host finished it", what)
	assert.Equal(e.T, providerv1.TaskOutcome_TASK_OUTCOME_SUCCEEDED, st.GetOutcome(), "%s: typed outcome", what)
}

// requireRefusedWith fails the case unless err is a refusal carrying the
// ErrorInfo reason in contracts.ErrorInfoDomain, and returns its wire form.
func requireRefusedWith(e *Env, err error, reason, what string) Wire {
	e.T.Helper()
	require.Error(e.T, err, "%s: must be refused", what)
	w := WireOf(err)
	require.NotEqual(e.T, codes.OK, w.Code, "%s: %s", what, w)
	r, ok := w.Reason(reason)
	require.True(e.T, ok, "%s: the refusal must carry a %s ErrorInfo: %s", what, reason, w)
	assert.Equal(e.T, contracts.ErrorInfoDomain, r.Domain, "%s: ErrorInfo domain", what)
	return w
}

// powerReq is a PowerRequest for vm, owned by OwnerA.
func powerReq(vm VM, op providerv1.PowerOp) *providerv1.PowerRequest {
	return &providerv1.PowerRequest{Id: vm.ID, Op: op, TargetHostId: vm.Host, Owner: OwnerA.Proto()}
}

// deleteReq is a DeleteRequest for vm, owned by OwnerA.
func deleteReq(vm VM) *providerv1.DeleteRequest {
	return &providerv1.DeleteRequest{Id: vm.ID, TargetHostId: vm.Host, Owner: OwnerA.Proto()}
}
