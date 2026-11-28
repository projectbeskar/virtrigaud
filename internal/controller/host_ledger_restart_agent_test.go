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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/agent/trust/trusttest"
)

// restartManager replaces the manager's host answers and host ledger with
// fresh ones (a manager restart, or a new leader): no own ledger write, no
// guard, no hold in memory; the same keys, trust and API server.
func (e *agentEnv) restartManager(t *testing.T) {
	t.Helper()
	old := e.ledger
	l := NewHostLedger(old.Client, old.APIReader, old.Trust, old.Keys, old.Config, nil)
	ha := &HostAnswers{Trust: e.trust, AttestRetryInterval: time.Millisecond}
	_, _, verdict, err := ha.RefreshHostAnswer(context.Background(), e.provider, e.cli, agentHost)
	require.NoError(t, err)
	require.Equal(t, answerVerified, verdict)
	ha.AttachLedger(l)
	e.r.HostAnswers, e.ledger = ha, l
}

// A ledger mark lost after the ledger was raised over a receipt, followed by
// a manager restart while the ledger does not move: the restarted manager
// has no write of its own to sign a mark from, so it writes the ledger
// again, unchanged, and signs the mark from that write; the parked operation
// then runs, exactly once. Before, the mark was never sent again and the
// operation stayed parked until some other write moved the ledger.
func TestAgentIdentity_LostMarkResentAfterManagerRestart(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	e.auth.dropMarks.Store(true)
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	vm := e.vm(t)
	a := findAttempt(vm, "Create")
	require.NotNil(t, a)
	require.Equal(t, infravirtrigaudiov1beta1.OperationAttemptPhaseAccepted, a.Phase, "the receipt was recorded")
	assert.Zero(t, e.createOps(t), "parked: the ledger mark was lost")

	provider := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, e.r.Get(ctx, client.ObjectKey{Namespace: trusttest.HostNS, Name: "prov-cluster"}, provider))
	store := e.r.HostAnswers.ledgerOf(e.provider, agentHost).store
	before, err := e.ledger.Load(ctx, provider, agentHost, store)
	require.NoError(t, err)
	require.GreaterOrEqual(t, int64(before.Position.Seq), a.Receipt.Seq, "the ledger covers the receipt")

	marks := e.auth.marks.Load()
	e.restartManager(t)
	e.auth.dropMarks.Store(false)
	require.Eventually(t, func() bool {
		e.r.HostAnswers.DriveHostLedger(ctx, e.provider, e.cli, agentHost)
		return e.auth.marks.Load() > marks
	}, 20*time.Second, 100*time.Millisecond, "the restarted manager signs a ledger mark the agent takes")
	after, err := e.ledger.Load(ctx, provider, agentHost, store)
	require.NoError(t, err)
	assert.Equal(t, before.Position, after.Position, "the ledger did not move: it was written again unchanged")
	assert.Greater(t, after.WriteCounter, before.WriteCounter)
	e.waitCreated(t)
	assert.Equal(t, 1, e.createOps(t), "exactly one Create")
}
