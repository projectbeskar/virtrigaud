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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
)

// Certificate renewal relay (development builds only). The manager publishes
// the renewed certificate it had issued for a host in the host inventory
// (RenewedCertificate). The Provider delivers it with agent.v1
// InstallRenewedCertificate: to stage it while its key is not pinned, and to
// put it in use once the inventory lists its key among the host's pins (the
// manager adds the pin only after a signed answer of its own request names
// the staged certificate). Like the freeze relay it is best effort, before a
// call to the host, never failing that call.

// Renewal relay phases.
const (
	renewalPhaseStage    = "stage"
	renewalPhaseActivate = "activate"
)

// Relay backoff bounds of the renewal relay.
const (
	renewalRetryMin = 10 * time.Second
	renewalRetryMax = 10 * time.Minute
)

// pendingRenewal returns the host's renewed certificate, whether to put it
// in use (its key is pinned in the inventory), and the relay key, when it
// has not been delivered in that phase on connection gen yet.
func (e *hostEntry) pendingRenewal(gen uint64) (der []byte, activate bool, key string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	der = e.spec.RenewedCertificate
	if len(der) == 0 || e.spec.Credentials.AgentTLS == nil {
		return nil, false, "", false
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, false, "", false
	}
	spki := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	activate = slices.Contains(e.spec.Credentials.AgentTLS.KeyPins, hex.EncodeToString(spki[:]))
	phase := renewalPhaseStage
	if activate {
		phase = renewalPhaseActivate
	}
	sum := sha256.Sum256(der)
	key = hex.EncodeToString(sum[:]) + "/" + phase
	if (e.renewalDeliveredGen == gen && e.renewalDeliveredKey == key) || e.renewalUnsupportedGen == gen ||
		e.opts.Now().Before(e.renewalNextTry) {
		return nil, false, "", false
	}
	return bytes.Clone(der), activate, key, true
}

// renewalDelivered records that relay key was delivered on connection gen.
func (e *hostEntry) renewalDelivered(gen uint64, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.renewalDeliveredGen, e.renewalDeliveredKey = gen, key
	e.renewalFailures, e.renewalNextTry = 0, time.Time{}
}

// renewalFailed records a failed relay on connection gen: an agent that
// answers Unimplemented does not support renewal on that connection; any
// other failure backs off, from renewalRetryMin doubling up to
// renewalRetryMax.
func (e *hostEntry) renewalFailed(gen uint64, unsupported bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if unsupported {
		e.renewalUnsupportedGen = gen
		return
	}
	wait := min(renewalRetryMin<<min(e.renewalFailures, 8), renewalRetryMax)
	e.renewalFailures++
	e.renewalNextTry = e.opts.Now().Add(wait)
}

// relayRenewal delivers the host's renewed certificate, if the inventory has
// one not yet delivered in its phase on the current connection, with an
// agent.v1 InstallRenewedCertificate before the call that is being
// forwarded: activate=false while its key is not pinned, activate=true with
// the same bytes once it is. A failure is logged and the delivery is retried
// later with a backoff; the forwarded call goes on either way. The agent
// verifies the certificate itself; the Provider does not judge it, and does
// not need to verify the answer (its connection is authenticated by a pinned
// key).
func (p *Provider) relayRenewal(ctx context.Context, e *hostEntry) {
	cc, gen, err := e.connection()
	if err != nil || cc == nil {
		return
	}
	der, activate, key, ok := e.pendingRenewal(gen)
	if !ok {
		return
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	req := &agentv1.InstallRenewedCertificateRequest{AnswerNonce: nonce, Certificate: der, Activate: activate}
	cs, err := p.pool.invokeChecked(ctx, e, agentv1.AgentService_InstallRenewedCertificate_FullMethodName, req,
		&agentv1.InstallRenewedCertificateResponse{}, nil, true)
	if activate && status.Code(err) == codes.FailedPrecondition {
		// The agent has nothing staged (it lost its staged copy, or never got
		// it on this connection): stage the same bytes, then activate them.
		stage := &agentv1.InstallRenewedCertificateRequest{AnswerNonce: freshNonce(), Certificate: der}
		if _, serr := p.pool.invokeChecked(ctx, e, agentv1.AgentService_InstallRenewedCertificate_FullMethodName, stage,
			&agentv1.InstallRenewedCertificateResponse{}, nil, true); serr == nil {
			req.AnswerNonce = freshNonce()
			cs, err = p.pool.invokeChecked(ctx, e, agentv1.AgentService_InstallRenewedCertificate_FullMethodName, req,
				&agentv1.InstallRenewedCertificateResponse{}, nil, true)
		} else {
			err = serr
		}
	}
	if err != nil {
		unsupported := status.Code(err) == codes.Unimplemented
		e.renewalFailed(gen, unsupported)
		if unsupported {
			p.log.Info("the host's agent does not take renewed certificates; none is relayed on this connection", "host", e.id)
			return
		}
		p.log.Info("renewed certificate not relayed; it is sent again later", "host", e.id, "activate", activate,
			"code", status.Code(err).String())
		return
	}
	e.renewalDelivered(cs.gen, key)
	p.log.Info("renewed certificate relayed to the host's agent", "host", e.id, "activate", activate, "sha256", key[:64])
}

// freshNonce returns 16 random bytes (zeros if the system's randomness
// fails, which the agent then answers without a usable answer; the relay
// does not read it).
func freshNonce() []byte {
	n := make([]byte, nonceLen)
	_, _ = rand.Read(n)
	return n
}
