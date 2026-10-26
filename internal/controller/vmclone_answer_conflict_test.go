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
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin that the answer to a first single-host Clone attempt that
// proves nothing was made — and so drops the CloneRequested record — is not
// lost when the VMClone is edited while the provider runs the Clone
// (recordCloneAnswer). Lost, the record survived and the next answer was
// taken for one to a re-send.

// editingCloner is a stampingCloner whose first editFirst Clone calls label
// the stored VMClone before answering, moving its resourceVersion on as a
// tenant or a tool would.
type editingCloner struct {
	*stampingCloner
	r         *VMCloneReconciler
	key       client.ObjectKey
	editFirst int
	t         *testing.T
}

// Clone edits the VMClone (for the first editFirst calls), then answers as
// the stampingCloner does.
func (p *editingCloner) Clone(ctx context.Context, req contracts.CloneRequest) (contracts.CloneResponse, error) {
	if len(p.reqs) < p.editFirst {
		clone := &infrav1beta1.VMClone{}
		require.NoError(p.t, p.r.Get(ctx, p.key, clone))
		if clone.Labels == nil {
			clone.Labels = map[string]string{}
		}
		clone.Labels["edited"] = "during-clone"
		require.NoError(p.t, p.r.Update(ctx, clone))
	}
	return p.stampingCloner.Clone(ctx, req)
}

var _ contracts.Cloner = (*editingCloner)(nil)

// editingStampFixture is stampFixture with an editingCloner over sc.
func editingStampFixture(t *testing.T, sc *stampingCloner, editFirst int) (*VMCloneReconciler, *infrav1beta1.VMClone) {
	t.Helper()
	ec := &editingCloner{stampingCloner: sc, editFirst: editFirst, t: t}
	r, clone := stampFixture(t, ec)
	ec.r, ec.key = r, client.ObjectKeyFromObject(clone)
	return r, clone
}

// TestVMClone_SingleHost_FirstAttemptConflictSurvivesAnEdit: a first
// attempt's name Conflict fails the clone although the VMClone was edited
// during the Clone. Lost, the CloneRequested record survived, and the next
// attempt's Conflict — the same name, still taken — was held as an unknown
// outcome for ever.
func TestVMClone_SingleHost_FirstAttemptConflictSurvivesAnEdit(t *testing.T) {
	conflict := contracts.NewConflictError(`clone: libvirt domain "default.clone-target" already exists on the host and is not `+
		`owned by the clone's target VirtualMachine; nothing was cloned`, nil)
	r, clone := editingStampFixture(t, &stampingCloner{script: []cloneStep{{err: conflict}, {err: conflict}}}, 1)

	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Equal(t, "during-clone", got.Labels["edited"], "the edit is kept")
	assert.False(t, cloneRequested(got), "the definite answer dropped the request record")
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase, "a refusal of a first attempt: nothing was made")
	assert.Equal(t, k8s.ReasonProviderConflict, cloneReadyCondition(t, got).Reason)
}

// TestVMClone_SingleHost_NotPerformedSurvivesAnEdit: a first attempt answered
// "not performed" is held as a transient failure with no request on record,
// although the VMClone was edited during the Clone; so the next attempt's
// refusal fails the clone instead of being held as a re-send's answer.
func TestVMClone_SingleHost_NotPerformedSurvivesAnEdit(t *testing.T) {
	conflict := contracts.NewConflictError(`clone: libvirt domain "default.clone-target" already exists`, nil)
	r, clone := editingStampFixture(t, &stampingCloner{script: []cloneStep{
		{err: notPerformedCreate(t)},
		{err: conflict},
	}}, 1)

	reconcileCloneOnce(t, r, clone) // finalizer
	reconcileCloneOnce(t, r, clone)
	got := getClone(t, r, clone)
	assert.Equal(t, "during-clone", got.Labels["edited"])
	assert.False(t, cloneRequested(got), "nothing was made: no request on record")
	assert.Equal(t, cloneReasonRetrying, cloneReadyCondition(t, got).Reason)

	r.cloneAttempts.elapse(clone.UID)
	reconcileCloneOnce(t, r, clone)
	got = getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase, "a first attempt's refusal fails the clone")
	assert.Equal(t, k8s.ReasonProviderConflict, cloneReadyCondition(t, got).Reason)
}
