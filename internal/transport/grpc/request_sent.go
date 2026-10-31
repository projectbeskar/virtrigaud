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

// The same stats handler also tells whether the PROVIDER ANSWERED an RPC: it
// reports an InTrailer only when grpc-go received the final HEADERS frame
// (END_STREAM) of a response it recognised as gRPC — a response whose
// content-type is application/grpc (http2_client.go operateHeaders). That
// covers a normal response (headers, message, trailers) and a trailers-only
// error response alike: the status the RPC returns is the one the server
// sent. Everything grpc-go synthesizes on the client is reported without one:
// no connection (refused, reset, TLS failure), the transport closing
// mid-RPC, the client's own deadline or cancellation, a stream the server
// reset (RST_STREAM, e.g. REFUSED_STREAM under load), and a non-gRPC HTTP
// response (a proxy's or load balancer's 502/503 page, whose status grpc-go
// derives from the HTTP code). The circuit breaker uses this to tell a
// provider that is unreachable or overloaded from one that answered that a
// single VM's operation failed (countsTowardBreaker).

// requestSentKey is the context key of an RPC's *requestSent.
type requestSentKey struct{}

// requestSent records whether an RPC began (the gRPC client started it),
// whether its request headers were sent, and whether the provider answered
// it (a gRPC response's trailers were received).
type requestSent struct {
	begun    atomic.Bool
	sent     atomic.Bool
	answered atomic.Bool
}

// providerAnswered reports whether the RPC's status came from the provider's
// gRPC server: grpc-go received the response's trailers (InTrailer). The
// stats handler runs on the transport's reader before the RPC completes, so
// by the time the invoker has returned the flag is final. Without the stats
// handler nothing is recorded and the RPC counts as not answered (fail
// closed: the breaker keeps counting its failure as before).
func (rs *requestSent) providerAnswered() bool {
	return rs.answered.Load()
}

// requestSentFrom returns the RPC's *requestSent carried by ctx, if any.
func requestSentFrom(ctx context.Context) (*requestSent, bool) {
	rs, ok := ctx.Value(requestSentKey{}).(*requestSent)
	return rs, ok
}

// withRequestSent returns ctx carrying the RPC's *requestSent, creating one
// when ctx has none (an interceptor chain without requestSentInterceptor).
func withRequestSent(ctx context.Context) (context.Context, *requestSent) {
	if rs, ok := requestSentFrom(ctx); ok {
		return ctx, rs
	}
	rs := &requestSent{}
	return context.WithValue(ctx, requestSentKey{}, rs), rs
}

// notSent reports whether the RPC provably never sent its request: it began
// (so the stats handler is in place and reporting) and no OutHeader followed.
// With no Begin at all — the stats handler is missing — nothing is proven,
// and the RPC counts as sent (fail closed: its outcome is unknown).
func (rs *requestSent) notSent() bool {
	return rs.begun.Load() && !rs.sent.Load()
}

// requestSentStatsHandler marks an RPC's requestSent on its Begin,
// OutHeader and InTrailer.
type requestSentStatsHandler struct{}

// TagRPC implements stats.Handler.
func (requestSentStatsHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

// HandleRPC implements stats.Handler: the RPC began, its request's headers
// were sent, or the provider's gRPC response trailers were received.
func (requestSentStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	rs, ok := requestSentFrom(ctx)
	if !ok {
		return
	}
	switch s.(type) {
	case *stats.Begin:
		rs.begun.Store(true)
	case *stats.OutHeader:
		rs.sent.Store(true)
	case *stats.InTrailer:
		rs.answered.Store(true)
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
