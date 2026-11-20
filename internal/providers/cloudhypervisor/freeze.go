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
	"encoding/hex"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	if (e.freezeDeliveredGen == gen && e.freezeDeliveredKey == key) || e.freezeUnsupportedGen == gen ||
		(e.freezeFailedKey == key && e.opts.Now().Before(e.freezeNextTry)) {
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

// freezeDelivered records that confirmation key was settled on connection
// gen: the agent reported every id applied, already applied or not
// applicable.
func (e *hostEntry) freezeDelivered(gen uint64, key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.freezeDeliveredGen, e.freezeDeliveredKey = gen, key
	e.freezeFailures, e.freezeNextTry, e.freezeFailedKey = 0, time.Time{}, ""
}

// freezeFailed records that confirmation key was not settled on connection
// gen: an agent that answers Unimplemented does not support it on that
// connection; any other failure, and an answer that does not report every
// id applied, backs off, from freezeRetryMin doubling up to freezeRetryMax.
// The backoff belongs to key: a changed confirmation is relayed with the
// next call, and its own failures start from freezeRetryMin.
func (e *hostEntry) freezeFailed(gen uint64, key string, unsupported bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if unsupported {
		e.freezeUnsupportedGen = gen
		return
	}
	if e.freezeFailedKey != key {
		e.freezeFailedKey, e.freezeFailures = key, 0
	}
	wait := freezeRetryMin << min(e.freezeFailures, 8)
	if wait > freezeRetryMax {
		wait = freezeRetryMax
	}
	e.freezeFailures++
	e.freezeNextTry = e.opts.Now().Add(wait)
}

// freezeSettled reads the agent's freeze_results for the confirmation of
// ids: settled only when there is exactly one result for each id, in order,
// echoing it, and each outcome is ACCEPTED, ALREADY_APPLIED or
// NOT_APPLICABLE. Anything else (no results, as from an agent that predates
// them; a short, long or mismatched list; REFUSED_OVER_CAPACITY, DEFERRED or
// an outcome this Provider does not know) leaves the confirmation pending,
// to be relayed again: the agent applies it idempotently, so a redelivery
// costs one call, and a confirmation marked delivered that was not applied
// would leave the predecessor frozen. refused counts the ids refused over
// capacity.
func freezeSettled(ids [][]byte, results []*agentv1.FreezeConfirmationResult) (settled bool, refused int) {
	settled = len(results) == len(ids)
	for i, r := range results {
		switch r.GetOutcome() {
		case agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ACCEPTED,
			agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_ALREADY_APPLIED,
			agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_NOT_APPLICABLE:
		case agentv1.FreezeConfirmationOutcome_FREEZE_CONFIRMATION_OUTCOME_REFUSED_OVER_CAPACITY:
			refused++
			settled = false
		default:
			settled = false
		}
		if i >= len(ids) || !bytes.Equal(r.GetEnrolmentId(), ids[i]) {
			settled = false
		}
	}
	return settled, refused
}

// Relay backoff bounds.
const (
	freezeRetryMin = 10 * time.Second
	freezeRetryMax = 10 * time.Minute
)

// relayFreeze delivers the host's freeze confirmation, if it has one not yet
// settled on the current connection, with an agent.v1 AgentStatus before
// the call that is being forwarded. It is best effort and never fails that
// call. The confirmation counts as delivered only when the agent's answer
// reports every id applied, already applied or not applicable
// (freezeSettled); a failed call, a refusal over capacity, a deferral or an
// answer without results (an agent that predates them) is logged and the
// confirmation is sent again with a later call, after a backoff (the agent
// applies it idempotently). The confirmation is relayed exactly as the
// manager wrote it; the Provider does not judge it, and the agent applies it
// only to its own frozen predecessors of the named current enrolment.
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
	resp := &agentv1.AgentStatusResponse{}
	cs, err := p.pool.invokeChecked(ctx, e, agentv1.AgentService_AgentStatus_FullMethodName, req, resp, nil, true)
	if err != nil {
		unsupported := status.Code(err) == codes.Unimplemented
		e.freezeFailed(gen, key, unsupported)
		if unsupported {
			p.log.Info("the host's agent does not take freeze confirmations; none is relayed on this connection", "host", e.id)
			return
		}
		p.log.Info("freeze confirmation not relayed; it is sent again later", "host", e.id, "error", err.Error())
		return
	}
	settled, refused := freezeSettled(ids, resp.GetFreezeResults())
	if !settled {
		e.freezeFailed(cs.gen, key, false)
		p.log.Info("freeze confirmation not applied by the host's agent; it is sent again later", "host", e.id,
			"enrolments", len(ids), "results", len(resp.GetFreezeResults()), "refusedOverCapacity", refused)
		return
	}
	e.freezeDelivered(cs.gen, key)
	p.log.Info("freeze confirmation relayed to the host's agent", "host", e.id, "enrolments", len(ids))
}
