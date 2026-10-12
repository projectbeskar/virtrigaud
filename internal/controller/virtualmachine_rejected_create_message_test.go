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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestHandleRejectedCreate_MessageIsSanitized (review L4).
func TestHandleRejectedCreate_MessageIsSanitized(t *testing.T) {
	vm := creatableVM("default")
	r := newTestReconciler(coverageTestScheme(t), nil, vm)
	err := contracts.NewConflictError("create: domain \"web\" exists\nowned by https://user:secret@host/x "+strings.Repeat("y", 600), nil)
	_, handled := r.handleRejectedCreate(context.Background(), vm, err, false)
	require.True(t, handled)
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, c)
	assert.NotContains(t, c.Message, "\n")
	assert.NotContains(t, c.Message, "secret")
	assert.Less(t, len(c.Message), 400, "bounded")
	assert.Equal(t, metav1.ConditionFalse, c.Status)
}
