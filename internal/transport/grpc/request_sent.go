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

package grpc

import (
	"context"
	"errors"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// Telling an RPC whose request never reached the provider from one whose
// outcome is unknown. Both fail with Unavailable (or, with WaitForReady, with
// DeadlineExceeded), and their messages overlap ("connection error: desc =
// ..."), so the text cannot tell them apart. The gRPC client's stats handler
// can: it reports an OutHeader once the request's headers were handed to the
// transport. An RPC that failed without one — no connection was made before
// its deadline, the name did not resolve, the circuit breaker refused it —
// never reached the provider. One that failed after it (the connection
// dropped mid-RPC, the provider pod was killed) may have been processed.

// requestSentKey is the context key of an RPC's *requestSent.
type requestSentKey struct{}

// requestSent records whether an RPC began (the gRPC client started it) and
// whether its request headers were sent.
type requestSent struct {
	begun atomic.Bool
	sent  atomic.Bool
}

// notSent reports whether the RPC provably never sent its request: it began
// (so the stats handler is in place and reporting) and no OutHeader followed.
// With no Begin at all — the stats handler is missing — nothing is proven,
// and the RPC counts as sent (fail closed: its outcome is unknown).
func (rs *requestSent) notSent() bool {
	return rs.begun.Load() && !rs.sent.Load()
}

// requestSentStatsHandler marks an RPC's requestSent on its Begin and
// OutHeader.
type requestSentStatsHandler struct{}

// TagRPC implements stats.Handler.
func (requestSentStatsHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

// HandleRPC implements stats.Handler: the RPC began, or its request's
// headers were sent.
func (requestSentStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	rs, ok := ctx.Value(requestSentKey{}).(*requestSent)
	if !ok {
		return
	}
	switch s.(type) {
	case *stats.Begin:
		rs.begun.Store(true)
	case *stats.OutHeader:
		rs.sent.Store(true)
	}
}

// breakerFastFailError is the circuit breaker's refusal of an RPC
// (providerCircuitBreakerInterceptor): the invoker never ran, so the request
// was never sent. It carries the Unavailable status the rest of the stack
// expects.
type breakerFastFailError struct{ st *status.Status }

func (e *breakerFastFailError) Error() string { return e.st.Err().Error() }

// GRPCStatus returns the Unavailable status.
func (e *breakerFastFailError) GRPCStatus() *status.Status { return e.st }

// TagConn implements stats.Handler.
func (requestSentStatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

// HandleConn implements stats.Handler.
func (requestSentStatsHandler) HandleConn(context.Context, stats.ConnStats) {}

// errRequestNotSent marks a requestNotSentError for mapGRPCError.
var errRequestNotSent = errors.New("request not sent")

// requestNotSentError is an RPC failure whose request never reached the
// provider. It keeps the original gRPC status (GRPCStatus), so metrics,
// breaker classification and mapGRPCError see the same code and message.
type requestNotSentError struct{ err error }

func (e *requestNotSentError) Error() string { return e.err.Error() }

// Unwrap returns the original error and the errRequestNotSent marker.
func (e *requestNotSentError) Unwrap() []error { return []error{e.err, errRequestNotSent} }

// GRPCStatus returns the original error's status.
func (e *requestNotSentError) GRPCStatus() *status.Status {
	st, _ := status.FromError(e.err)
	return st
}

// requestSentInterceptor is the outermost unary interceptor: it gives every
// RPC a requestSent the stats handler marks, and wraps an Unavailable or
// DeadlineExceeded failure whose request provably was never sent into a
// requestNotSentError: the circuit breaker's fast-fail (breakerFastFailError,
// the invoker never ran), or an RPC that began but sent no headers (no
// connection could be made). Anything else — headers sent, or no Begin
// reported at all (no stats handler) — is left as is: outcome unknown.
func requestSentInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		fullMethod string,
		req, reply interface{},
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		rs := &requestSent{}
		err := invoker(context.WithValue(ctx, requestSentKey{}, rs), fullMethod, req, reply, cc, opts...)
		if err == nil {
			return nil
		}
		var fastFail *breakerFastFailError
		if errors.As(err, &fastFail) {
			return &requestNotSentError{err: err}
		}
		if !rs.notSent() {
			return err
		}
		switch status.Code(err) {
		case codes.Unavailable, codes.DeadlineExceeded:
			return &requestNotSentError{err: err}
		}
		return err
	}
}
