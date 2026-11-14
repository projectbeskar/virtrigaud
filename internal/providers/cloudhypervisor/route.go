//go:build virtrigaud_dev

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

package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/projectbeskar/virtrigaud/internal/clustered"
	"github.com/projectbeskar/virtrigaud/internal/operation/grantverify"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// Raw frames.
//
// The Provider's gRPC service reads the request of every forwarded method
// into an emptypb.Empty: a message with no fields, so the standard protobuf
// codec keeps every byte of the request as unknown fields, verbatim and in
// order, and writes them back verbatim when the message is sent on. That is
// how the outer request reaches the agent byte for byte as the manager sent
// it (the agent's verifier judges those bytes), with the Provider's ordinary
// gRPC server and the standard codec on both legs. The Provider decodes a
// separate copy only to route.

// rawFrame wraps b, the exact bytes of one message, for sending.
func rawFrame(b []byte) *emptypb.Empty {
	m := &emptypb.Empty{}
	m.ProtoReflect().SetUnknown(b)
	return m
}

// frameBytes is the exact bytes a frame was read from.
func frameBytes(m *emptypb.Empty) []byte { return m.ProtoReflect().GetUnknown() }

// Causes in the ErrorInfo metadata of a host-scoped refusal (MetadataCause).
const (
	// MetadataCause is the ErrorInfo metadata key that names why a host is
	// unavailable.
	MetadataCause = "cause"
	// CauseNotFronted: the host is not in the Provider's inventory (unknown,
	// removed, or its entry was refused).
	CauseNotFronted = "HostNotFronted"
	// CauseAgentIncompatible: the agent's API version is outside the
	// Provider's version and the one before it.
	CauseAgentIncompatible = "AgentIncompatible"
	// CauseAgentUnverified: the agent's Hello could not be verified (no or
	// an invalid signed answer, or the agent refused the Provider).
	CauseAgentUnverified = "AgentUnverified"
	// CauseAgentCertificateRefused: the agent's certificate failed the
	// host's pins, CA, profile or URI.
	CauseAgentCertificateRefused = "AgentCertificateRefused"
	// CauseAgentUnreachable: the agent could not be reached.
	CauseAgentUnreachable = "AgentUnreachable"
	// CauseBreakerOpen: the host's circuit breaker is open.
	CauseBreakerOpen = "BreakerOpen"
)

// hostUnavailable is the host-scoped refusal of a call to host:
// Unavailable with the HOST_UNAVAILABLE ErrorInfo, which the manager keeps
// out of its circuit breaker. host is named in the message and the metadata
// only when it is a host the Provider fronts (fronted); a host id the
// Provider does not know is never echoed.
func hostUnavailable(host string, fronted bool, cause string) error {
	md := map[string]string{MetadataCause: cause}
	msg := "the host is not fronted by this Provider"
	if fronted {
		md[contracts.HostUnavailableHostKey] = host
		msg = fmt.Sprintf("host %q is unavailable (%s)", host, cause)
	}
	st := status.New(codes.Unavailable, msg)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: contracts.HostUnavailableReason,
		Domain: contracts.HostUnavailableErrorDomain, Metadata: md})
	if err != nil {
		return st.Err()
	}
	return withInfo.Err()
}

// identityRequired is the refusal of a mutating call without an operation
// envelope.
func identityRequired() error {
	st := status.New(codes.FailedPrecondition, "this Provider performs a mutating call only with an operation grant")
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: contracts.IdentityRequiredReason,
		Domain: contracts.ErrorInfoDomain})
	if err != nil {
		return st.Err()
	}
	return withInfo.Err()
}

// outerFieldSet is the refusal of a mutating call that sets an outer field
// beside its operation envelope, as the agent answers it.
func outerFieldSet() error {
	st := status.New(codes.InvalidArgument, "a field of the outer request is set beside operation_envelope")
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: contracts.GrantMismatchReason,
		Domain: contracts.ErrorInfoDomain})
	if err != nil {
		return st.Err()
	}
	return withInfo.Err()
}

// The call state of one call to an agent.
//
// The client connection's stats handler records whether a call began,
// whether its request headers were handed to the transport (OutHeader) and
// whether the agent's gRPC server answered it (InTrailer: grpc-go received
// the trailers of a gRPC response). A call that failed after Begin without
// OutHeader never reached the agent; one that failed without InTrailer got
// no answer from it, whatever its status says.

// callStateKey is the context key of a call's *callState.
type callStateKey struct{}

// callState is what the stats handler recorded about one call.
type callState struct {
	begun, sent, answered atomic.Bool
}

// notSent reports whether the call provably never sent its request.
func (c *callState) notSent() bool { return c.begun.Load() && !c.sent.Load() }

// withCallState returns ctx carrying a new *callState.
func withCallState(ctx context.Context) (context.Context, *callState) {
	cs := &callState{}
	return context.WithValue(ctx, callStateKey{}, cs), cs
}

// callStatsHandler records the call state of each call.
type callStatsHandler struct{}

// TagRPC implements stats.Handler.
func (callStatsHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }

// HandleRPC implements stats.Handler.
func (callStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	cs, ok := ctx.Value(callStateKey{}).(*callState)
	if !ok {
		return
	}
	switch s.(type) {
	case *stats.Begin:
		cs.begun.Store(true)
	case *stats.OutHeader:
		cs.sent.Store(true)
	case *stats.InTrailer:
		cs.answered.Store(true)
	}
}

// TagConn implements stats.Handler.
func (callStatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn implements stats.Handler.
func (callStatsHandler) HandleConn(context.Context, stats.ConnStats) {}

// route is how the Provider handles one forwarded method.
type route struct {
	// method is the full method name.
	method string
	// newRequest returns an empty request of the method.
	newRequest func() proto.Message
	// mutating is set for a method that needs an operation grant.
	mutating bool
	// newResponse returns an empty response of a mutating method, whose
	// task reference the Provider host-encodes.
	newResponse func() proto.Message
}

// routes are the forwarded methods: the provider.v1 methods a per-host agent
// serves. Every other method is the Provider's own.
var routes = map[string]route{
	providerv1.Provider_Create_FullMethodName: {method: providerv1.Provider_Create_FullMethodName, mutating: true,
		newRequest:  func() proto.Message { return &providerv1.CreateRequest{} },
		newResponse: func() proto.Message { return &providerv1.CreateResponse{} }},
	providerv1.Provider_Delete_FullMethodName: {method: providerv1.Provider_Delete_FullMethodName, mutating: true,
		newRequest:  func() proto.Message { return &providerv1.DeleteRequest{} },
		newResponse: func() proto.Message { return &providerv1.TaskResponse{} }},
	providerv1.Provider_Power_FullMethodName: {method: providerv1.Provider_Power_FullMethodName, mutating: true,
		newRequest:  func() proto.Message { return &providerv1.PowerRequest{} },
		newResponse: func() proto.Message { return &providerv1.TaskResponse{} }},
	providerv1.Provider_Reconfigure_FullMethodName: {method: providerv1.Provider_Reconfigure_FullMethodName, mutating: true,
		newRequest:  func() proto.Message { return &providerv1.ReconfigureRequest{} },
		newResponse: func() proto.Message { return &providerv1.TaskResponse{} }},
	providerv1.Provider_Describe_FullMethodName: {method: providerv1.Provider_Describe_FullMethodName,
		newRequest: func() proto.Message { return &providerv1.DescribeRequest{} }},
	providerv1.Provider_TaskStatus_FullMethodName: {method: providerv1.Provider_TaskStatus_FullMethodName,
		newRequest: func() proto.Message { return &providerv1.TaskStatusRequest{} }},
	providerv1.Provider_GetHostInfo_FullMethodName: {method: providerv1.Provider_GetHostInfo_FullMethodName,
		newRequest: func() proto.Message { return &providerv1.GetHostInfoRequest{} }},
}

// hostOfMutation returns the host a mutating call is routed to: the
// target_host_id of a decoded copy of the request inside its envelope. The
// outer request must carry the envelope and nothing else (CheckOuter);
// nothing of the grant is judged here.
func hostOfMutation(r route, outer proto.Message) (string, error) {
	env, err := grantverify.CheckOuter(outer)
	if err != nil {
		var refusal *grantverify.Refusal
		switch {
		case errors.As(err, &refusal) && refusal.Rule == grantverify.RuleEnvelopeMissing:
			return "", identityRequired()
		case errors.As(err, &refusal) && refusal.InvalidArgument:
			return "", outerFieldSet()
		}
		return "", status.Error(codes.InvalidArgument, "the request cannot carry an operation envelope")
	}
	inner := r.newRequest()
	if err := proto.Unmarshal(env.GetRequest(), inner); err != nil {
		return "", status.Error(codes.InvalidArgument, "operation_envelope.request does not decode as the method's request")
	}
	h, ok := inner.(interface{ GetTargetHostId() string })
	if !ok || strings.TrimSpace(h.GetTargetHostId()) == "" {
		return "", status.Error(codes.InvalidArgument, "the request inside operation_envelope names no target_host_id")
	}
	return h.GetTargetHostId(), nil
}

// handle serves one forwarded call whose request arrived as the exact bytes
// raw. It returns a frame of the agent's exact answer for a read, and the
// decoded answer with a host-encoded task reference for a mutating call.
func (p *Provider) handle(ctx context.Context, method string, raw []byte) (proto.Message, error) {
	r, ok := routes[method]
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "method %s is not served by this Provider", method)
	}
	decoded := r.newRequest()
	if err := proto.Unmarshal(raw, decoded); err != nil {
		return nil, status.Error(codes.InvalidArgument, "the request does not decode")
	}
	if r.mutating {
		return p.mutate(ctx, r, raw, decoded)
	}
	host, forward, err := readRoute(decoded, raw, func(h string) bool { _, ok := p.pool.get(h); return ok })
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(host) == "" {
		return nil, status.Error(codes.InvalidArgument, "the request names no host")
	}
	out := &emptypb.Empty{}
	if err := p.forward(ctx, host, method, rawFrame(forward), out, false); err != nil {
		return nil, err
	}
	return out, nil
}

// readRoute returns the host a read is routed to and the bytes to forward:
// the request unchanged, except for a TaskStatus that names a host-encoded
// task, which is forwarded with the agent's own task reference.
func readRoute(decoded proto.Message, raw []byte, fronted func(string) bool) (string, []byte, error) {
	switch req := decoded.(type) {
	case *providerv1.DescribeRequest:
		return req.GetTargetHostId(), raw, nil
	case *providerv1.GetHostInfoRequest:
		return req.GetHostId(), raw, nil
	case *providerv1.TaskStatusRequest:
		if len(req.GetAnswerNonce()) != 0 {
			if req.GetTask() != nil {
				return "", nil, status.Error(codes.InvalidArgument, "a request with answer_nonce names the operation, not a task")
			}
			return req.GetTargetHostId(), raw, nil
		}
		host, inner, err := clustered.ParseHostTaskRef(req.GetTask().GetId())
		if err != nil {
			return "", nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if !fronted(string(host)) {
			// Not a reference this Provider issued: never dialed.
			return "", nil, status.Error(codes.NotFound,
				"the task's host is not fronted by this Provider; the task reference was not issued by it")
		}
		b, err := proto.Marshal(&providerv1.TaskStatusRequest{Task: &providerv1.TaskRef{Id: inner}})
		if err != nil {
			return "", nil, status.Error(codes.Internal, "the task request could not be encoded")
		}
		return string(host), b, nil
	}
	return "", nil, status.Error(codes.Internal, "no route for the request")
}

// mutate forwards a mutating call and host-encodes the task reference of its
// answer. The answer's other fields, signed answers included, are kept.
func (p *Provider) mutate(ctx context.Context, r route, raw []byte, decoded proto.Message) (proto.Message, error) {
	host, err := hostOfMutation(r, decoded)
	if err != nil {
		return nil, err
	}
	out := &emptypb.Empty{}
	if err := p.forward(ctx, host, r.method, rawFrame(raw), out, true); err != nil {
		return nil, err
	}
	resp := r.newResponse()
	if err := proto.Unmarshal(frameBytes(out), resp); err != nil {
		// The agent performed, or accepted, the call: its answer must not
		// read as "not performed".
		return nil, clustered.OutcomeUnknownRetryStatus(fmt.Sprintf("host %q answered with a response that does not decode", host))
	}
	if t, ok := resp.(interface{ GetTask() *providerv1.TaskRef }); ok && t.GetTask() != nil {
		t.GetTask().Id = clustered.EncodeHostTaskRef(clustered.HostID(host), t.GetTask().GetId())
	}
	return resp, nil
}

// forward sends req on method to host's agent, after its Hello, under its
// breaker, and maps a failure that is not the agent's own answer.
func (p *Provider) forward(ctx context.Context, host, method string, req, resp proto.Message, mutating bool) error {
	e, ok := p.pool.get(host)
	if !ok {
		return hostUnavailable(host, false, CauseNotFronted)
	}
	if err := p.pool.helloed(ctx, e); err != nil {
		return err
	}
	cs, err := p.pool.invoke(ctx, e, method, req, resp)
	return mapCallError(ctx, host, mutating, cs, err)
}

// mapCallError maps the result of one call to host's agent: the agent's own
// answer is returned unchanged; a call the breaker refused, or that never
// reached the agent, is host-scoped and not performed; a mutating call that
// was sent and got no answer may have been performed.
func mapCallError(ctx context.Context, host string, mutating bool, cs *callState, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errBreakerOpen):
		return hostUnavailable(host, true, CauseBreakerOpen)
	case errors.Is(err, errNotConnected):
		return hostUnavailable(host, true, CauseAgentUnreachable)
	case cs.answered.Load():
		return err
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	case cs.notSent():
		return hostUnavailable(host, true, notSentCause(err))
	case mutating:
		return clustered.OutcomeUnknownRetryStatus(fmt.Sprintf("host %q stopped answering; the operation's outcome is unknown", host))
	}
	return hostUnavailable(host, true, CauseAgentUnreachable)
}

// notSentCause names why a call never reached the agent.
func notSentCause(err error) string {
	if strings.Contains(status.Convert(err).Message(), ErrAgentRefused.Error()) {
		return CauseAgentCertificateRefused
	}
	return CauseAgentUnreachable
}

// invoke performs one call on e's connection under its breaker and returns
// its call state.
func (p *pool) invoke(ctx context.Context, e *hostEntry, method string, req, resp any, opts ...grpc.CallOption) (*callState, error) {
	ctx, cs := withCallState(ctx)
	err := guard(ctx, e.breaker, func(ctx context.Context) (bool, error) {
		if !e.awaitConnection(ctx, p.opts.ConnectTimeout) {
			return false, errNotConnected
		}
		err := e.conn.Invoke(ctx, method, req, resp, opts...)
		return cs.answered.Load(), err
	})
	return cs, err
}

// errNotConnected is returned by invoke when no connection to the agent was
// made within the connect timeout: nothing was sent.
var errNotConnected = errors.New("no connection to the agent")
