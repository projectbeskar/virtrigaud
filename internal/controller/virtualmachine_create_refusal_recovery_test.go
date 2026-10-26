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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestPersistedCreateRefusal_LatestAnswerDecides (review): restart recovery
// reads the answer to the latest Create, never an older refusal Ready still
// shows.
func TestPersistedCreateRefusal_LatestAnswerDecides(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	stale := createRefusalMessage(createRefusalHeadRejected, 10*time.Minute, t0.Add(10*time.Minute), "old")
	fresh := createRefusalMessage(createRefusalHeadFailed, 30*time.Second, t0.Add(30*time.Second), "new")
	cond := func(typ string, st metav1.ConditionStatus, reason, msg string, gen int64) metav1.Condition {
		return metav1.Condition{Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: gen}
	}
	for name, tc := range map[string]struct {
		conds      []metav1.Condition
		wantOK     bool
		wantDetail string
	}{
		"a stale Ready and a paced Provisioning: Provisioning": {
			conds: []metav1.Condition{
				cond(k8s.ConditionReady, metav1.ConditionFalse, k8s.ReasonValidationError, stale, 3),
				cond(k8s.ConditionProvisioning, metav1.ConditionFalse, k8s.ReasonProviderError, fresh, 3),
			},
			wantOK: true, wantDetail: "new",
		},
		"a stale Ready and a non-paced Provisioning (CREATE_NOT_PERFORMED): nothing": {
			conds: []metav1.Condition{
				cond(k8s.ConditionReady, metav1.ConditionFalse, k8s.ReasonValidationError, stale, 3),
				cond(k8s.ConditionProvisioning, metav1.ConditionFalse, k8s.ReasonProviderError, "Failed to create VM: nothing made", 3),
			},
		},
		"a refusal on Ready while Provisioning is the create marker (a re-send): Ready": {
			conds: []metav1.Condition{
				cond(k8s.ConditionReady, metav1.ConditionFalse, k8s.ReasonProviderError, fresh, 3),
				cond(k8s.ConditionProvisioning, metav1.ConditionTrue, k8s.ReasonCreateRequested, "marker", 3),
			},
			wantOK: true, wantDetail: "new",
		},
		"a stale Ready while an image prepare runs: nothing": {
			conds: []metav1.Condition{
				cond(k8s.ConditionReady, metav1.ConditionFalse, k8s.ReasonValidationError, stale, 3),
				cond(k8s.ConditionProvisioning, metav1.ConditionTrue, k8s.ReasonTaskInProgress, "Preparing image", 3),
			},
		},
		"a refusal of an older generation: nothing": {
			conds: []metav1.Condition{
				cond(k8s.ConditionReady, metav1.ConditionFalse, k8s.ReasonValidationError, fresh, 2),
				cond(k8s.ConditionProvisioning, metav1.ConditionFalse, k8s.ReasonValidationError, fresh, 2),
			},
		},
		"Ready only, no Provisioning: nothing": {
			conds: []metav1.Condition{cond(k8s.ConditionReady, metav1.ConditionFalse, k8s.ReasonValidationError, fresh, 3)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			vm := creatableVM("team-p")
			vm.Status.Conditions = tc.conds
			_, _, detail, ok := persistedCreateRefusal(vm)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantDetail, detail)
		})
	}
}

// TestCreateMarker_PacedRefusalOfAResendKeepsTheMarker (review): a re-send
// answered NotFound keeps the create marker, records the paced message on
// Ready, waits the backoff, and is recovered from Ready after a restart.
func TestCreateMarker_PacedRefusalOfAResendKeepsTheMarker(t *testing.T) {
	ctx := context.Background()
	r, rp, _ := createRecordFixture(t, infravirtrigaudiov1beta1.ProviderTypeVSphere, nil)
	rp.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
		return contracts.CreateResponse{}, contracts.NewNotFoundError("create: template not found", nil)
	}
	marked := seedMarker(t, r, time.Hour)
	res, err := r.reconcileVM(ctx, marked)
	require.NoError(t, err)
	require.Len(t, rp.createReqs, 1, "the Create was re-sent")
	assert.Equal(t, 30*time.Second, res.RequeueAfter, "paced, not every 5 s")

	held, _ := storedVM(t, r)
	assert.True(t, createRequested(held), "the first attempt's outcome is still unknown: the marker stays")
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonProviderError, ready.Reason)
	assert.True(t, strings.HasPrefix(ready.Message, createRefusalHeadFailed+createRefusalMessageWait), ready.Message)

	wait, _, detail, ok := persistedCreateRefusal(held)
	require.True(t, ok, "a restarted manager recovers the wait from Ready")
	assert.Equal(t, 30*time.Second, wait)
	assert.Contains(t, detail, "template not found")
}
