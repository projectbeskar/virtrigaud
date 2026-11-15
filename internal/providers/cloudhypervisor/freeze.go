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
	"crypto/rand"
	"encoding/hex"
	"strings"

	agentv1 "github.com/projectbeskar/virtrigaud/proto/rpc/agent/v1"
)

// freezeKey identifies a freeze confirmation: the current enrolment it was
// made for and its enrolment ids, in order.
func freezeKey(forEnrolment string, ids []string) string {
	return forEnrolment + "/" + strings.Join(ids, ",")
}

// pendingFreeze returns the host's freeze confirmation as the agent.v1
// request fields, and its key, when it has not been delivered on connection
// gen yet; ok is false otherwise. The confirmation comes from the host
// inventory (the manager's), already validated with the entry.
func (e *hostEntry) pendingFreeze(gen uint64) (forEnrolment []byte, ids [][]byte, key string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.spec.FreezeConfirmation
	if f == nil {
		return nil, nil, "", false
	}
	key = freezeKey(f.ForEnrolmentID, f.EnrolmentIDs)
	if e.freezeDeliveredGen == gen && e.freezeDeliveredKey == key {
		return nil, nil, "", false
	}
	forEnrolment, err := hex.DecodeString(f.ForEnrolmentID)
	if err != nil {
		return nil, nil, "", false
	}
	for _, s := range f.EnrolmentIDs {
		id, err := hex.DecodeString(s)
		if err != nil {
			return nil, nil, "", false
		}
		ids = append(ids, id)
	}
	return forEnrolment, ids, key, true
}

// freezeDelivered records that confirmation key was delivered on connection
// gen.
func (e *hostEntry) freezeDelivered(gen uint64, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.freezeDeliveredGen, e.freezeDeliveredKey = gen, key
}

// relayFreeze delivers the host's freeze confirmation, if it has one not yet
// delivered on the current connection, with an agent.v1 AgentStatus before
// the call that is being forwarded. It is best effort: a failure is logged
// and the confirmation is sent again with a later call (the agent applies
// it idempotently). The confirmation is relayed exactly as the manager
// wrote it; the Provider does not judge it, and the agent applies it only to
// its own frozen predecessors of the named current enrolment.
func (p *Provider) relayFreeze(ctx context.Context, e *hostEntry) {
	cc, gen, err := e.connection()
	if err != nil || cc == nil {
		return
	}
	forEnrolment, ids, key, ok := e.pendingFreeze(gen)
	if !ok {
		return
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	req := &agentv1.AgentStatusRequest{AnswerNonce: nonce, ConfirmForEnrolmentId: forEnrolment, ConfirmFrozenEnrolmentIds: ids}
	cs, err := p.pool.invokeChecked(ctx, e, agentv1.AgentService_AgentStatus_FullMethodName, req, &agentv1.AgentStatusResponse{}, nil, true)
	if err != nil {
		p.log.Info("freeze confirmation not relayed; it is sent again with a later call", "host", e.id, "error", err.Error())
		return
	}
	e.freezeDelivered(cs.gen, key)
	p.log.Info("freeze confirmation relayed to the host's agent", "host", e.id, "enrolments", len(ids))
}
