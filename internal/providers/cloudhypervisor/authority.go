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
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/projectbeskar/virtrigaud/internal/clustered"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// The host authority service.
//
// provider.v1.HostAuthority is the per-host service of the host ledger
// protocol: the manager asks a host's agent for an attestation challenge,
// and delivers attestation, ledger mark and retire-if-unknown grants. Every
// answer is signed by the host, and judged by the manager; every request is
// judged by the agent. The Provider only routes: it reads the request's
// target_host_id from a decoded copy, sends the exact request bytes to that
// host's agent, and returns the exact bytes of the agent's answer. It never
// decodes, checks or re-encodes the grant or the signed answer.
//
// Every failure the Provider reports itself is scoped to the addressed host,
// so that one bad host never counts toward the manager's Provider-wide
// breaker: a call the agent never received is HOST_UNAVAILABLE (not
// performed), and a call that was sent but got no usable answer is
// OUTCOME_UNKNOWN_RETRY (repeating it is safe: an attestation challenge is
// single use, a ledger mark is monotone and a retirement is idempotent).

// HostAuthorityAgentAPIVersion is the first agent API version that serves
// the host authority service. A call addressed to an agent of an older
// version is answered Unimplemented by the Provider, without reaching the
// agent (contracts.HostAuthorityUnsupportedReason).
const HostAuthorityAgentAPIVersion uint32 = 5

// metadataAgentAPIVersion is the ErrorInfo metadata key of the agent's API
// version in a HostAuthorityUnsupportedReason answer.
const metadataAgentAPIVersion = "agentApiVersion"

// authorityRoutes are the methods of the host authority service, each
// routed by the target_host_id of its request.
var authorityRoutes = map[string]route{
	providerv1.HostAuthority_Challenge_FullMethodName: {method: providerv1.HostAuthority_Challenge_FullMethodName, authority: true,
		newRequest: func() proto.Message { return &providerv1.ChallengeRequest{} }},
	providerv1.HostAuthority_Attest_FullMethodName: {method: providerv1.HostAuthority_Attest_FullMethodName, authority: true,
		newRequest: func() proto.Message { return &providerv1.AttestRequest{} }},
	providerv1.HostAuthority_LedgerMark_FullMethodName: {method: providerv1.HostAuthority_LedgerMark_FullMethodName, authority: true,
		newRequest: func() proto.Message { return &providerv1.LedgerMarkRequest{} }},
	providerv1.HostAuthority_RetireIfUnknown_FullMethodName: {method: providerv1.HostAuthority_RetireIfUnknown_FullMethodName, authority: true,
		newRequest: func() proto.Message { return &providerv1.RetireIfUnknownRequest{} }},
}

// AuthorityServiceDesc is the provider.v1 HostAuthority service description
// in which every method reads its request as the exact bytes that arrived
// and relays them unchanged, as ServiceDesc does for the Provider service.
// Provider.Register registers it.
func AuthorityServiceDesc() *grpc.ServiceDesc {
	d := providerv1.HostAuthority_ServiceDesc
	d.Methods = make([]grpc.MethodDesc, len(providerv1.HostAuthority_ServiceDesc.Methods))
	copy(d.Methods, providerv1.HostAuthority_ServiceDesc.Methods)
	for i, m := range d.Methods {
		full := "/" + d.ServiceName + "/" + m.MethodName
		if _, ok := authorityRoutes[full]; ok {
			d.Methods[i].Handler = rawHandler(full)
		}
	}
	return &d
}

// authorize routes one host authority call whose request arrived as the
// exact bytes raw, and returns a frame of the agent's exact answer.
func (p *Provider) authorize(ctx context.Context, r route, raw []byte, decoded proto.Message) (proto.Message, error) {
	h, ok := decoded.(interface{ GetTargetHostId() string })
	if !ok || strings.TrimSpace(h.GetTargetHostId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "the request names no target_host_id")
	}
	host := h.GetTargetHostId()
	out := &emptypb.Empty{}
	// mutating: a call that was sent and got no answer may have changed the
	// agent's state, so it is never reported as not performed.
	if err := p.forward(ctx, host, r.method, rawFrame(raw), out, true, HostAuthorityAgentAPIVersion); err != nil {
		return nil, authorityAnswer(ctx, host, err)
	}
	return out, nil
}

// authorityAnswer maps the error of one host authority call to host. The
// Provider's own refusals are already host-scoped and are returned
// unchanged, as is every answer of the agent the manager keeps out of its
// Provider-wide breaker, and the caller's own cancellation or deadline. An answer of the agent that the manager would count
// toward that breaker (Unavailable or DeadlineExceeded without a reason that
// scopes it, or Unknown without a message) is replaced by
// OUTCOME_UNKNOWN_RETRY: it says nothing about the Provider, and nothing
// about whether the agent acted on the call.
func authorityAnswer(ctx context.Context, host string, err error) error {
	st, ok := status.FromError(err)
	if !ok || ctx.Err() != nil || !countedProviderWide(st, err) {
		return err
	}
	return clustered.OutcomeUnknownRetryStatus(
		fmt.Sprintf("host %q answered %s to a host authority call; its outcome is unknown", host, st.Code()))
}

// countedProviderWide reports whether st, an answered status, is one the
// manager would count toward its Provider-wide breaker: Unavailable or
// DeadlineExceeded, or Unknown without a message, that carries no reason
// scoping it to a host, a VM or an operation.
func countedProviderWide(st *status.Status, err error) bool {
	switch {
	case st.Code() == codes.Unavailable, st.Code() == codes.DeadlineExceeded:
	case st.Code() == codes.Unknown && st.Message() == "":
	default:
		return false
	}
	for _, r := range []string{contracts.HostUnavailableReason, contracts.OutcomeUnknownRetryReason,
		contracts.VMOperationFailedReason} {
		if contracts.HasErrorInfoReason(err, r) {
			return false
		}
	}
	return !contracts.HasPartAReason(err)
}

// hostAuthorityUnsupported is the answer of a host authority call to host
// whose agent, of API version agent, does not serve the service:
// Unimplemented, with the HOST_AUTHORITY_UNSUPPORTED ErrorInfo naming the
// host. host is always one the Provider fronts (it made the agent's Hello).
func hostAuthorityUnsupported(host string, agent uint32) error {
	st := status.New(codes.Unimplemented, fmt.Sprintf(
		"host %q runs agent API version %d, which does not serve the host authority service (version %d does)",
		host, agent, HostAuthorityAgentAPIVersion))
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Reason: contracts.HostAuthorityUnsupportedReason,
		Domain: contracts.ErrorInfoDomain, Metadata: map[string]string{
			contracts.HostUnavailableHostKey: host,
			metadataAgentAPIVersion:          strconv.FormatUint(uint64(agent), 10),
		}})
	if err != nil {
		return st.Err()
	}
	return withInfo.Err()
}

// Challenge implements providerv1.HostAuthorityServer.
func (p *Provider) Challenge(ctx context.Context, req *providerv1.ChallengeRequest) (*providerv1.ChallengeResponse, error) {
	return typed(ctx, p, providerv1.HostAuthority_Challenge_FullMethodName, req, &providerv1.ChallengeResponse{})
}

// Attest implements providerv1.HostAuthorityServer.
func (p *Provider) Attest(ctx context.Context, req *providerv1.AttestRequest) (*providerv1.AuthorityResponse, error) {
	return typed(ctx, p, providerv1.HostAuthority_Attest_FullMethodName, req, &providerv1.AuthorityResponse{})
}

// LedgerMark implements providerv1.HostAuthorityServer.
func (p *Provider) LedgerMark(ctx context.Context, req *providerv1.LedgerMarkRequest) (*providerv1.AuthorityResponse, error) {
	return typed(ctx, p, providerv1.HostAuthority_LedgerMark_FullMethodName, req, &providerv1.AuthorityResponse{})
}

// RetireIfUnknown implements providerv1.HostAuthorityServer.
func (p *Provider) RetireIfUnknown(ctx context.Context, req *providerv1.RetireIfUnknownRequest) (*providerv1.AuthorityResponse, error) {
	return typed(ctx, p, providerv1.HostAuthority_RetireIfUnknown_FullMethodName, req, &providerv1.AuthorityResponse{})
}
