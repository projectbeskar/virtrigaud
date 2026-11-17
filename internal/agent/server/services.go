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
	"encoding/hex"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/projectbeskar/virtrigaud/internal/agent/opstore"
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
	if req.GetRenewalVersion() != 0 && len(req.GetAnswerNonce()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "renewal_version is set without answer_nonce")
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
		HostAnswer: p.s.hostAnswerWithInventory(ctx, req.GetAnswerNonce(), req.GetContinuitySeq(), req.GetInventoryVersion(), req.GetRenewalVersion()),
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

// AgentStatus answers with a signed HostAnswer. A freeze confirmation it
// carries is applied first (applyFreezeConfirmation), so the answer reflects
// it; the confirmation never changes the answer's success.
func (a *agentService) AgentStatus(ctx context.Context, req *agentv1.AgentStatusRequest) (*agentv1.AgentStatusResponse, error) {
	if err := checkNonce(req.GetAnswerNonce(), true); err != nil {
		return nil, err
	}
	a.s.applyFreezeConfirmation(ctx, req.GetConfirmForEnrolmentId(), req.GetConfirmFrozenEnrolmentIds())
	return &agentv1.AgentStatusResponse{HostAnswer: a.s.hostAnswer(ctx, req.GetAnswerNonce(), 0)}, nil
}

// applyFreezeConfirmation applies a relayed, unsigned freeze confirmation:
// only while the agent is READY, only for at most
// opstore.MaxFreezeConfirmations well-formed ids, and only to frozen
// predecessors of the active enrolment named by forEnrolment (the store
// checks). Each discard is logged with the enrolment ids, which are not
// secret; anything else is ignored with a log line.
func (s *Server) applyFreezeConfirmation(ctx context.Context, forEnrolment []byte, ids [][]byte) {
	if len(ids) == 0 && len(forEnrolment) == 0 {
		return
	}
	switch {
	case s.opts.Freezes == nil:
		s.opts.Log.Info("freeze confirmation ignored: this agent applies none")
		return
	case trustReady(s.opts.Agent) != providerv1.AgentState_AGENT_STATE_READY:
		s.opts.Log.Info("freeze confirmation ignored: the agent is not ready")
		return
	case len(ids) > opstore.MaxFreezeConfirmations:
		s.opts.Log.Info("freeze confirmation ignored: it names too many enrolments", "count", len(ids))
		return
	}
	done, err := s.opts.Freezes.ConfirmFrozen(ctx, forEnrolment, ids)
	for _, id := range done {
		s.opts.Log.Info("freeze confirmed: a frozen predecessor enrolment is discarded (its records are removed in chunks; compaction finishes an interrupted removal)",
			"enrolment", hex.EncodeToString(id), "for", hex.EncodeToString(forEnrolment))
	}
	if err != nil {
		s.opts.Log.Error(err, "freeze confirmation stopped", "for", hex.EncodeToString(forEnrolment))
	} else if len(done) == 0 {
		s.opts.Log.V(1).Info("freeze confirmation: nothing to discard", "for", hex.EncodeToString(forEnrolment), "count", len(ids))
	}
}

// InstallRenewedCertificate stages a renewed certificate, or puts the staged
// one in use (Renewals.Install), and answers with a signed HostAnswer: after
// activation it is signed with the new key and names no staged certificate.
// It needs no grant: the agent accepts only a certificate that chains to its
// pinned issuer, carries its current identities and certifies the key of its
// own pending renewal. An agent that serves no renewal (a release build)
// answers Unimplemented.
func (a *agentService) InstallRenewedCertificate(ctx context.Context, req *agentv1.InstallRenewedCertificateRequest) (*agentv1.InstallRenewedCertificateResponse, error) {
	if a.s.opts.Renewals == nil {
		return nil, status.Error(codes.Unimplemented, "certificate renewal is not served by this agent")
	}
	if err := checkNonce(req.GetAnswerNonce(), true); err != nil {
		return nil, err
	}
	if len(req.GetCertificate()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "certificate is empty")
	}
	if err := a.s.opts.Renewals.Install(ctx, req.GetCertificate(), req.GetActivate()); err != nil {
		return nil, err
	}
	return &agentv1.InstallRenewedCertificateResponse{HostAnswer: a.s.hostAnswer(ctx, req.GetAnswerNonce(), 0)}, nil
}

// trustReady returns the agent's trust state.
func trustReady(a Agent) providerv1.AgentState {
	st, _ := a.TrustState()
	return st
}
