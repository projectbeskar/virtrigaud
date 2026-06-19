/*
Copyright 2025.

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// powerSpyProvider drives ensureSourcePoweredOff: Describe reports On until a
// power-off is issued, then Off; Power records each op it receives.
type powerSpyProvider struct {
	stubProvider
	poweredOff atomic.Bool
	mu         sync.Mutex
	powerOps   []contracts.PowerOp
}

func (p *powerSpyProvider) Describe(_ context.Context, _ string) (contracts.DescribeResponse, error) {
	state := string(contracts.PowerStateOn)
	if p.poweredOff.Load() {
		state = string(contracts.PowerStateOff)
	}
	return contracts.DescribeResponse{Exists: true, PowerState: state}, nil
}

func (p *powerSpyProvider) Power(_ context.Context, _ string, op contracts.PowerOp) (string, error) {
	p.mu.Lock()
	p.powerOps = append(p.powerOps, op)
	p.mu.Unlock()
	p.poweredOff.Store(true)
	return "", nil
}

func (p *powerSpyProvider) ops() []contracts.PowerOp {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.PowerOp(nil), p.powerOps...)
}

var _ contracts.Provider = (*powerSpyProvider)(nil)

// TestEnsureSourcePoweredOff_PowersOffThenProceeds verifies the
// powerOffBeforeMigration gate (Bug H): a running source gets a hard power-off
// (PowerOpOff) and the step requeues (done=false); once the source reports Off it
// returns done=true and never re-issues the power-off.
func TestEnsureSourcePoweredOff_PowersOffThenProceeds(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{}
	sourceVM, sourceProvider, migration := snapshotFixture()
	sourceVM.Spec.PowerState = infrav1beta1.PowerStateOn
	migration.Spec.Source.PowerOffBeforeMigration = true
	r, c := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	// First pass: source is On → issue a hard power-off and requeue (not done).
	done, res, err := r.ensureSourcePoweredOff(ctx, migration)
	require.NoError(t, err)
	assert.False(t, done, "must requeue while the source is still On")
	assert.Greater(t, res.RequeueAfter, time.Duration(0), "must requeue to poll for Off")
	require.Len(t, prov.ops(), 1)
	assert.Equal(t, contracts.PowerOpOff, prov.ops()[0], "must issue a hard power-off")

	// The source VM's desired power state must be aligned to Off so the
	// VirtualMachine reconciler does not race the export back to On.
	var got infrav1beta1.VirtualMachine
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sourceVM), &got))
	assert.Equal(t, infrav1beta1.PowerStateOff, got.Spec.PowerState,
		"must set the source VM desired power state to Off")

	// Second pass: source now reports Off → proceed, with no further power-off.
	done, _, err = r.ensureSourcePoweredOff(ctx, migration)
	require.NoError(t, err)
	assert.True(t, done, "must proceed once the source is Off")
	assert.Len(t, prov.ops(), 1, "must not re-issue power-off once the source is Off")
}

// TestEnsureSourcePoweredOff_AlreadyOff verifies an already-stopped source is a
// no-op: it returns done=true immediately and issues no power-off.
func TestEnsureSourcePoweredOff_AlreadyOff(t *testing.T) {
	ctx := context.Background()
	prov := &powerSpyProvider{}
	prov.poweredOff.Store(true) // already Off
	sourceVM, sourceProvider, migration := snapshotFixture()
	migration.Spec.Source.PowerOffBeforeMigration = true
	r, _ := newSnapshotReconciler(t, prov, sourceVM, sourceProvider, migration)

	done, _, err := r.ensureSourcePoweredOff(ctx, migration)
	require.NoError(t, err)
	assert.True(t, done, "an already-off source proceeds immediately")
	assert.Empty(t, prov.ops(), "must not power off a source that is already Off")
}
