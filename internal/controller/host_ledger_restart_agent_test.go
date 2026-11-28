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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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

// etcdSnapshot is a copy of what an etcd restore puts back: the VM (its
// status: the attempt markers), every Secret (the operation intents, the
// host ledger objects) and the pins state (the store state and the ledger
// index).
type etcdSnapshot struct {
	vm      *infravirtrigaudiov1beta1.VirtualMachine
	secrets []corev1.Secret
	stores  map[string]HostStoreState
}

func (e *agentEnv) etcdSnapshot(t *testing.T) etcdSnapshot {
	t.Helper()
	secs := &corev1.SecretList{}
	require.NoError(t, e.r.List(context.Background(), secs))
	vm := e.vm(t).DeepCopy()
	e.trust.mu.Lock()
	defer e.trust.mu.Unlock()
	stores := map[string]HostStoreState{}
	for k, v := range e.trust.stores {
		c := v
		c.LedgerIndex = v.LedgerIndex.clone()
		stores[k] = c
	}
	return etcdSnapshot{vm: vm, secrets: secs.Items, stores: stores}
}

// etcdRestore puts snap back: the VM status, every Secret as it was (those
// created since are deleted), and the pins state.
func (e *agentEnv) etcdRestore(t *testing.T, snap etcdSnapshot) {
	t.Helper()
	ctx := context.Background()
	vm := e.vm(t)
	vm.Status = *snap.vm.Status.DeepCopy()
	require.NoError(t, e.r.Status().Update(ctx, vm))
	cur := &corev1.SecretList{}
	require.NoError(t, e.r.List(ctx, cur))
	kept := map[types.NamespacedName]corev1.Secret{}
	for _, s := range snap.secrets {
		kept[types.NamespacedName{Namespace: s.Namespace, Name: s.Name}] = s
	}
	for i := range cur.Items {
		s := &cur.Items[i]
		key := types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
		old, ok := kept[key]
		if !ok {
			require.NoError(t, e.r.Delete(ctx, s))
			continue
		}
		s.Data, s.Labels, s.Annotations = old.Data, old.Labels, old.Annotations
		require.NoError(t, e.r.Update(ctx, s))
		delete(kept, key)
	}
	for _, s := range kept {
		c := s.DeepCopy()
		c.ResourceVersion = ""
		require.NoError(t, e.r.Create(ctx, c))
	}
	e.trust.mu.Lock()
	e.trust.stores = snap.stores
	e.trust.mu.Unlock()
}

// An etcd restore alone (the agent's store is not rolled back): the VM's
// attempt markers, the operation intents, the pins state and the host
// ledger go back to a snapshot taken while a Create was sent but lost,
// before the agent restarted into a new attestation session and the Create
// ran. The restarted manager finds the agent attested in a session the
// restored ledger does not hold and re-attests it (no agent restart)
// against the lower ledger; the agent keeps its higher mark, the attempt is
// decided from the agent's record, and the Create ran exactly once.
func TestAgentIdentity_EtcdRestoreRecoversByReattestation(t *testing.T) {
	e := newAgentEnv(t)
	ctx := context.Background()
	e.auth.dropMarks.Store(true)
	_, err := e.r.Reconcile(ctx, e.req)
	require.NoError(t, err)
	a := findAttempt(e.vm(t), "Create")
	require.NotNil(t, a)
	require.NotNil(t, a.Receipt, "accepted and ledgered; its ledger mark was lost, so it is parked")
	require.Zero(t, e.createOps(t))
	store := e.r.HostAnswers.ledgerOf(e.provider, agentHost).store
	snap := e.etcdSnapshot(t)
	restored, err := e.ledger.Load(ctx, e.provider, agentHost, store)
	require.NoError(t, err)
	e.auth.dropMarks.Store(false)

	// The agent restarts into a new session; it is re-attested, the
	// Create (its request bytes lost with the restart) is re-sent and
	// runs.
	require.NoError(t, e.host.Stop())
	_, _ = e.host.Wait(10 * time.Second)
	require.NoError(t, e.host.Restart())
	require.Eventually(t, e.driveReady, 20*time.Second, 300*time.Millisecond, "re-attested after the restart")
	e.waitCreated(t)
	require.Equal(t, 1, e.createOps(t))
	moved, err := e.ledger.Load(ctx, e.provider, agentHost, store)
	require.NoError(t, err)
	require.NotEqual(t, restored.AttestationID, moved.AttestationID, "the agent is in a session the snapshot does not hold")
	require.Greater(t, moved.WriteCounter, restored.WriteCounter)

	// etcd is restored while the manager is stopped, and the manager
	// starts again. (A manager that keeps running across the restore sees
	// its own ledger go back and holds the host persistently,
	// LedgerRegressed: the guard cannot tell a restore from a rollback.)
	e.etcdRestore(t, snap)
	e.restartManager(t)
	rec, err := e.ledger.Load(ctx, e.provider, agentHost, store)
	require.NoError(t, err, "a restarted manager reads the restored ledger (a full restore is consistent)")
	require.Equal(t, restored.Position, rec.Position)
	require.Equal(t, restored.AttestationID, rec.AttestationID)

	require.Eventually(t, e.driveReady, 20*time.Second, 300*time.Millisecond, "re-attested in a new session, no agent restart")
	rec, err = e.ledger.LoadBound(ctx, e.provider, agentHost, store)
	require.NoError(t, err)
	assert.NotEqual(t, restored.AttestationID, rec.AttestationID, "the re-attestation's session was adopted")
	assert.Contains(t, sessionIDs(rec.SessionHistory), restored.AttestationID, "the restored session is fenced")

	vm := e.waitCreated(t)
	c := findAttempt(vm, "Create")
	require.NotNil(t, c)
	assert.Equal(t, infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded, c.Outcome, "decided from the agent's record")
	assert.Equal(t, a.OpID, c.OpID, "the restored attempt, not a new one")
	assert.Equal(t, 1, e.createOps(t), "exactly one Create")
}

func sessionIDs(h []HostLedgerFencedSession) [][16]byte {
	out := make([][16]byte, 0, len(h))
	for _, f := range h {
		out = append(out, f.ID)
	}
	return out
}
