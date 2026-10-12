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

package k8s

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetCondition_RecordsObservedGeneration(t *testing.T) {
	var conds []metav1.Condition
	SetReadyCondition(&conds, metav1.ConditionFalse, ReasonCreating, "creating", 3)
	if len(conds) != 1 || conds[0].ObservedGeneration != 3 {
		t.Fatalf("observedGeneration not recorded: %+v", conds)
	}

	// Same status and reason under a newer generation: the generation moves,
	// the transition time stays.
	old := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	conds[0].LastTransitionTime = old
	SetReadyCondition(&conds, metav1.ConditionFalse, ReasonCreating, "still creating", 4)
	if conds[0].ObservedGeneration != 4 || !conds[0].LastTransitionTime.Equal(&old) || conds[0].Message != "still creating" {
		t.Fatalf("same status/reason: want generation 4, unchanged transition time, new message; got %+v", conds[0])
	}

	// A repeated call with the same values changes nothing.
	before := conds[0]
	SetReadyCondition(&conds, metav1.ConditionFalse, ReasonCreating, "still creating", 4)
	if conds[0] != before {
		t.Fatalf("a repeated call changed the condition: %+v -> %+v", before, conds[0])
	}

	// A new reason is a transition.
	SetReadyCondition(&conds, metav1.ConditionFalse, ReasonProviderError, "failed", 4)
	if conds[0].LastTransitionTime.Equal(&old) {
		t.Fatalf("a reason change must move the transition time")
	}
}
