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
	stderrors "errors"
	"fmt"
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

// needsPowerOffErr is the manager-side form of a provider's
// RECONFIGURE_NEEDS_POWER_OFF refusal (the transport's mapGRPCError).
func needsPowerOffErr() error {
	return contracts.NewRetryableError("reconfigure: the VM is powered on and CPU hot-add is not enabled on it",
		fmt.Errorf("%w: %w", contracts.ErrReconfigureNeedsPowerOff, stderrors.New("rpc error")))
}

// TestReconcileVM_ReconfigureNeedsPowerOff_PendingUntilPoweredOff (rc1 lab
// finding V1): a CPU change a running vSphere VM cannot take (hot-add not
// enabled on it) is recorded as Reconfiguring=False/ResizePendingPowerOff with
// a constant message — not as a provider failure — and status.currentResources
// keeps the VM's size; it is not re-sent within the backoff, and once the
// owner sets spec.powerState: Off and the VM is off, the next Reconfigure is
// sent at once and its success records the new size.
func TestReconcileVM_ReconfigureNeedsPowerOff_PendingUntilPoweredOff(t *testing.T) {
	prov := newResultProvider()
	prov.err = needsPowerOffErr()
	r, clock := singleHostReconciler(t, prov)
	vm := sizedSingleHostVM(4, 8192, 8, 8192)
	ctx := context.Background()

	res, err := r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	require.Equal(t, 1, prov.calls)
	assert.Equal(t, 5*time.Second, res.RequeueAfter, "asked again on the VM's reconfigure backoff")
	cpu, _ := recorded(vm)
	assert.Equal(t, int32(4), cpu, "nothing was changed: the recorded size stays")
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, k8s.ReasonResizePendingPowerOff, c.Reason)
	assert.Equal(t, resizePendingPowerOffMessage, c.Message, "a constant message, not the provider's text")
	assert.Equal(t, vm.Generation, c.ObservedGeneration)
	assert.True(t, lastReconfigureFailed(vm), "the clustered memory ceiling is not lowered while it waits")

	// Within the backoff nothing is sent and the condition is unchanged.
	clock.t = clock.t.Add(2 * time.Second)
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 1, prov.calls)
	assert.Equal(t, *c, *meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring))

	// Once the backoff has passed it is asked again (the VM may have been
	// power-cycled meanwhile); still refused, still the same condition.
	clock.t = clock.t.Add(5 * time.Second)
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 2, prov.calls)
	assert.Equal(t, c.LastTransitionTime, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring).LastTransitionTime)

	// The owner powers the VM off: the spec change sends the Reconfigure at
	// once to the powered-off VM, which takes it.
	vm.Generation++
	vm.Spec.PowerState = infravirtrigaudiov1beta1.PowerStateOff
	prov.DescribeFn = func(context.Context, string) (contracts.DescribeResponse, error) {
		return contracts.DescribeResponse{Exists: true, PowerState: "Off"}, nil
	}
	prov.err = nil
	_, err = r.reconcileVM(ctx, vm)
	require.NoError(t, err)
	assert.Equal(t, 3, prov.calls)
	cpu, _ = recorded(vm)
	assert.Equal(t, int32(8), cpu, "applied to the powered-off VM: the new size is recorded")
	assert.Equal(t, k8s.ReasonReconcileSuccess, meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring).Reason)
	assert.False(t, lastReconfigureFailed(vm))
}

// TestRecordReconfigureFailure_OtherErrorsStayProviderError: only the marked
// refusal is ResizePendingPowerOff; an unmarked failure that merely mentions
// hot-add stays a provider failure with the provider's own text.
func TestRecordReconfigureFailure_OtherErrorsStayProviderError(t *testing.T) {
	prov := newResultProvider()
	r, _ := singleHostReconciler(t, prov)
	_, class := providerAndClass("default")
	vm := sizedSingleHostVM(4, 8192, 8, 8192)
	r.recordReconfigureFailure(vm, contracts.VMRef{ID: vm.Status.ID}, class,
		contracts.NewRetryableError("reconfigure: CPU hot-add is not enabled", nil))
	c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	require.NotNil(t, c)
	assert.Equal(t, k8s.ReasonProviderError, c.Reason)
	assert.Contains(t, c.Message, "CPU hot-add is not enabled")
}
