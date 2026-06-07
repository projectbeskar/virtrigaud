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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// TestVMSetStub_SetsControllerNotImplemented verifies the VMSet stub controller
// reports Ready=False / ControllerNotImplemented and does nothing else.
func TestVMSetStub_SetsControllerNotImplemented(t *testing.T) {
	s := cloneTestScheme(t)
	vmSet := &infrav1beta1.VMSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set-1", Namespace: "default"},
	}
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(vmSet).
		WithStatusSubresource(&infrav1beta1.VMSet{}).
		Build()
	r := &VMSetReconciler{Client: fc, Scheme: s}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(vmSet),
	})
	require.NoError(t, err)

	got := &infrav1beta1.VMSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(vmSet), got))
	ready := readyCondition(got.Status.Conditions, vmSetConditionReady)
	require.NotNil(t, ready, "expected Ready condition to be set")
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, vmSetReasonNotImplemented, ready.Reason)
	assert.Equal(t, vmSetNotImplementedMessage, ready.Message)
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration)
}
