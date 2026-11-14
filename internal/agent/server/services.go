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

package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// providerService serves the provider.v1 Provider methods for this host.
// Every method it does not define is Unimplemented; every mutating method is
// refused by the server's gate before it is reached.
type providerService struct {
	providerv1.UnimplementedProviderServer
	s *Server
}

// GetHostInfo answers for this host only, with a signed HostAnswer when the
// request carries answer_nonce. When the request also carries an
// inventory_version of at least 1, the signed answer carries the host
// inventory; the other RPCs of the agent never do.
func (p *providerService) GetHostInfo(ctx context.Context, req *providerv1.GetHostInfoRequest) (*providerv1.HostInfo, error) {
	if err := checkNonce(req.GetAnswerNonce(), false); err != nil {
		return nil, err
	}
	if req.GetContinuitySeq() != 0 && len(req.GetAnswerNonce()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "continuity_seq is set without answer_nonce")
	}
	if req.GetInventoryVersion() != 0 && len(req.GetAnswerNonce()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "inventory_version is set without answer_nonce")
	}
	hostID := p.s.opts.Credentials.Load().Identity.HostID()
	if req.GetHostId() != "" && req.GetHostId() != hostID {
		return nil, status.Error(codes.InvalidArgument, "host_id names another host")
	}
	health := providerv1.HostHealth_HOST_HEALTH_NOT_READY
	if state, _ := p.s.opts.Agent.TrustState(); state == providerv1.AgentState_AGENT_STATE_READY {
		health = providerv1.HostHealth_HOST_HEALTH_READY
	}
	return &providerv1.HostInfo{
		Id:         hostID,
		Health:     health,
		HostAnswer: p.s.hostAnswerWithInventory(ctx, req.GetAnswerNonce(), req.GetContinuitySeq(), req.GetInventoryVersion()),
	}, nil
}

// agentService serves the internal agent.v1 service.
type agentService struct {
	agentv1.UnimplementedAgentServiceServer
	s *Server
}

// Hello answers the first call of a connection with a signed HostAnswer that
// states the agent's API version, under its own answer limit (Options
// HelloAnswersPerSecond) so that reads cannot starve it.
func (a *agentService) Hello(ctx context.Context, req *agentv1.HelloRequest) (*agentv1.HelloResponse, error) {
	if err := checkNonce(req.GetAnswerNonce(), true); err != nil {
		return nil, err
	}
	if req.GetCallerApiVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "caller_api_version must be at least 1")
	}
	return &agentv1.HelloResponse{HostAnswer: a.s.helloAnswer(ctx, req.GetAnswerNonce())}, nil
}

// AgentStatus answers with a signed HostAnswer.
func (a *agentService) AgentStatus(ctx context.Context, req *agentv1.AgentStatusRequest) (*agentv1.AgentStatusResponse, error) {
	if err := checkNonce(req.GetAnswerNonce(), true); err != nil {
		return nil, err
	}
	return &agentv1.AgentStatusResponse{HostAnswer: a.s.hostAnswer(ctx, req.GetAnswerNonce(), 0)}, nil
}

// InstallRenewedCertificate is not served yet: certificate renewal arrives
// with a later change.
func (a *agentService) InstallRenewedCertificate(context.Context, *agentv1.InstallRenewedCertificateRequest) (*agentv1.InstallRenewedCertificateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "certificate renewal is not served by this agent")
}
