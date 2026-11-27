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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// The upgrade pre-flight writes every host's verified in-memory store
// position that is above the persisted one, and nothing else; the runnable
// exists only for a manager with a host ledger.
func TestPreflightRaisePositions(t *testing.T) {
	ctx := context.Background()
	stub := newIdentityStub()
	r, _ := identityFixture(t, stub)
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: clusteredNS, Name: "prov-cluster"}, p))
	trust, ok := r.HostAnswers.Trust.(*staticTrust)
	require.True(t, ok)
	for range 2 {
		_, _, verdict, err := r.HostAnswers.RefreshHostAnswer(ctx, p, stub, "host-alpha")
		require.NoError(t, err)
		require.Equal(t, answerVerified, verdict)
	}
	persisted, err := trust.LoadStoreState(ctx, p, "host-alpha")
	require.NoError(t, err)
	r.HostAnswers.mu.Lock()
	inMemory := r.HostAnswers.hosts[hostAnswerKey(p, "host-alpha")].store.Current.Seq
	r.HostAnswers.mu.Unlock()
	require.Greater(t, inMemory, persisted.Current.Seq, "the second answer's position is held in memory only")

	n, err := r.HostAnswers.PreflightRaisePositions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	after, err := trust.LoadStoreState(ctx, p, "host-alpha")
	require.NoError(t, err)
	assert.Equal(t, inMemory, after.Current.Seq, "the pre-flight persisted the in-memory position")
	n, err = r.HostAnswers.PreflightRaisePositions(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing above the persisted position is left")

	assert.Nil(t, PreflightRunnable(&HostAnswers{}, nil), "no pre-flight without a host ledger")
	open := make(chan struct{})
	never, ok := PreflightRunnable(&HostAnswers{Ledger: &HostLedger{}}, open).(preflightRunnable)
	require.True(t, ok)
	assert.False(t, never.led(), "a replica that never led writes nothing")
	close(open)
	assert.True(t, never.led(), "a replica that led writes on stop")
	nilElected, _ := PreflightRunnable(&HostAnswers{Ledger: &HostLedger{}}, nil).(preflightRunnable)
	assert.False(t, nilElected.led())
}
