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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
)

// A Provider whose cache shows nothing using it is not released while the API
// server shows a use the cache has not seen yet (ADR-0010 race, manager side).
func TestProviderInUse_LiveUsageHoldsWhatTheCacheMissed(t *testing.T) {
	ctx := context.Background()
	r, _ := newInUseReconciler(t, inUseProvider())

	// The API server already has a VM that recorded its Create (the marker)
	// on this Provider; the cache has not caught up.
	marked := inUseVM("default", "web", "", inUseProviderName)
	marked.Status.ID = ""
	marked.Status.Conditions = []metav1.Condition{{Type: k8s.ConditionProvisioning, Status: metav1.ConditionTrue,
		Reason: k8s.ReasonCreateRequested, Message: "requested", LastTransitionTime: metav1.Now()}}
	live := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(inUseProvider(), marked).Build()
	r.APIReader = live

	deleteInUseProvider(t, r)
	reconcileProvider(t, r)
	require.False(t, providerGone(t, r), "a use only the API server shows holds the Provider")
	blocked := meta.FindStatusCondition(getInUseProvider(t, r).Status.Conditions, k8s.ConditionDeleteBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, metav1.ConditionTrue, blocked.Status)

	// The VM is gone from the API server too: the Provider is released.
	require.NoError(t, live.Delete(ctx, marked))
	got := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := live.Get(ctx, client.ObjectKeyFromObject(marked), got); err == nil {
		got.Finalizers = nil
		require.NoError(t, live.Update(ctx, got))
	}
	reconcileProvider(t, r)
	assert.True(t, providerGone(t, r))
}

// pagingVMReader serves VirtualMachine Lists one item per page, through
// Limit/Continue, and records the options of every call.
type pagingVMReader struct {
	client.Reader
	vms   []infravirtrigaudiov1beta1.VirtualMachine
	calls []client.ListOptions
}

func (p *pagingVMReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	lo := client.ListOptions{}
	lo.ApplyOptions(opts)
	p.calls = append(p.calls, lo)
	start := 0
	if lo.Continue != "" {
		_, _ = fmt.Sscanf(lo.Continue, "%d", &start)
	}
	vmList := list.(*infravirtrigaudiov1beta1.VirtualMachineList)
	vmList.Items = []infravirtrigaudiov1beta1.VirtualMachine{*p.vms[start].DeepCopy()}
	vmList.Continue = ""
	if start+1 < len(p.vms) {
		vmList.Continue = fmt.Sprintf("%d", start+1)
	}
	return nil
}

func TestLiveIndexedReader_ReadsEveryPage(t *testing.T) {
	ctx := context.Background()
	reader := &pagingVMReader{vms: []infravirtrigaudiov1beta1.VirtualMachine{
		*inUseVM("default", "a", "", inUseProviderName),
		*inUseVM("default", "b", "", "another-provider"),
		*inUseVM("default", "c", "", inUseProviderName),
	}}
	vms, err := listProviderVMs(ctx, liveIndexedReader{Reader: reader}, inUseProviderKey)
	require.NoError(t, err)
	require.Len(t, vms, 2, "matches on every page are kept")
	assert.Equal(t, "a", vms[0].Name)
	assert.Equal(t, "c", vms[1].Name)
	require.Len(t, reader.calls, 3)
	for _, c := range reader.calls {
		assert.Equal(t, int64(liveListPageSize), c.Limit)
		assert.Nil(t, c.FieldSelector, "the API server is never sent the cache-only field selector")
	}
	assert.Equal(t, "2", reader.calls[2].Continue)
}

func TestLiveIndexedReader_FiltersByIndexAndRefusesOtherSelectors(t *testing.T) {
	ctx := context.Background()
	mine := inUseVM("default", "web", "", inUseProviderName)
	other := inUseVM("default", "db", "", "another-provider")
	r, _ := newInUseReconciler(t)
	live := liveIndexedReader{Reader: fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(mine, other).Build()}

	vms, err := listProviderVMs(ctx, live, inUseProviderKey)
	require.NoError(t, err)
	require.Len(t, vms, 1)
	assert.Equal(t, "web", vms[0].Name)

	var all infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, live.List(ctx, &all))
	assert.Len(t, all.Items, 2, "an unselected List is passed through")

	err = live.List(ctx, &all, client.MatchingFields{"spec.unknown": "x"})
	assert.Error(t, err, "a field the reader has no extractor for is refused, never ignored")
}
