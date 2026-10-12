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
	SetCondition(&conds, "Ready", metav1.ConditionFalse, "Pending", "waiting", 2)
	if len(conds) != 1 || conds[0].ObservedGeneration != 2 {
		t.Fatalf("observedGeneration not recorded: %+v", conds)
	}

	// Nothing but the generation changed: only the generation moves (it is no
	// longer a per-change counter).
	old := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	conds[0].LastTransitionTime = old
	SetCondition(&conds, "Ready", metav1.ConditionFalse, "Pending", "waiting", 5)
	if conds[0].ObservedGeneration != 5 || !conds[0].LastTransitionTime.Equal(&old) {
		t.Fatalf("want generation 5 and the transition time kept; got %+v", conds[0])
	}

	// A repeated call changes nothing.
	before := conds[0]
	SetCondition(&conds, "Ready", metav1.ConditionFalse, "Pending", "waiting", 5)
	if conds[0] != before {
		t.Fatalf("a repeated call changed the condition: %+v -> %+v", before, conds[0])
	}

	// A message change moves the transition time (this package's historical
	// semantics), and the generation is the object's, not incremented.
	SetCondition(&conds, "Ready", metav1.ConditionFalse, "Pending", "still waiting", 5)
	if conds[0].LastTransitionTime.Equal(&old) || conds[0].ObservedGeneration != 5 {
		t.Fatalf("want a new transition time and generation 5; got %+v", conds[0])
	}
}

func TestSetCondition_NilSliceIsANoOp(t *testing.T) {
	SetCondition(nil, "Ready", metav1.ConditionTrue, "Ok", "ok", 1)
}
