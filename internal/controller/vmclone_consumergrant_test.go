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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMClone controller's enforcement of
// spec.consumerNamespaceSelector: the source VM's Provider must select the
// clone's namespace, and the Provider and VMClass the clone pins onto its
// target must select the target namespace — checked before the Clone RPC and
// before the target VirtualMachine is created.

// requireCloneConsumerRefused asserts Ready=False/ConsumerNotAllowed for the
// given object at the current generation, not terminal.
func requireCloneConsumerRefused(t *testing.T, got *infrav1beta1.VMClone, kind, ns, name string) {
	t.Helper()
	ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, k8s.ReasonConsumerNotAllowed, ready.Reason)
	assert.Equal(t, got.Generation, ready.ObservedGeneration)
	assert.Equal(t, (&ConsumerNotAllowedError{Kind: kind, Namespace: ns, Name: name}).Error(), ready.Message)
	assert.NotEqual(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase, "a refusal is recoverable, not terminal")
}

// countingCloneResolver counts every provider resolution.
type countingCloneResolver struct {
	provider contracts.Provider
	calls    int
}

func (c *countingCloneResolver) GetProvider(context.Context, *infrav1beta1.Provider) (contracts.Provider, error) {
	c.calls++
	return c.provider, nil
}

func TestVMCloneConsumer_SourceProviderInAnotherNamespace(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone("")
	src := sourceVMWithID(xnsSource, "src-vm", "shared", "vm-source-123")
	src.Spec.ProviderRef.Namespace = cgOwnerNS
	src.Spec.ClassRef = infrav1beta1.ObjectRef{Name: "src-class"}
	shared := runningProvider(cgOwnerNS, "shared")
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, rec := newXNSCloneReconciler(t, cp, clone, labeledNamespace(xnsSource, nil), shared)
	// Replace the fixture's source VM with one on the other namespace's Provider.
	require.NoError(t, r.Delete(ctx, sourceVMWithID(xnsSource, "src-vm", "prov-1", "")))
	require.NoError(t, r.Create(ctx, src))
	src.Status.ID = "vm-source-123"
	require.NoError(t, r.Status().Update(ctx, src))
	res := &countingCloneResolver{provider: cp}
	r.RemoteResolver = res

	result := reconcileClone(t, r, clone, 4)
	assert.Equal(t, consumerNotAllowedRetryInterval, result.RequeueAfter)
	got := getClone(t, r, clone)
	requireCloneConsumerRefused(t, got, consumerKindProvider, cgOwnerNS, "shared")
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Zero(t, cp.cloneCnt, "no Clone RPC")
	assert.Zero(t, res.calls, "no Provider is resolved to a client")
	assert.Equal(t, 1, countEvents(rec, k8s.ReasonConsumerNotAllowed), "one Warning event, on the transition")

	// Sharing the Provider lets the clone run and clears the refusal.
	p := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(shared), p))
	p.Spec.ConsumerNamespaceSelector = sharedWithNamespace(xnsSource)
	require.NoError(t, r.Update(ctx, p))
	reconcileClone(t, r, clone, 4)
	got = getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, got.Status.Phase)
	assert.Equal(t, 1, cp.cloneCnt)
}

func TestVMCloneConsumer_CrossNamespaceTargetNeedsProviderAndClassGrants(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		provSel, classSel *metav1.LabelSelector
		kind, objName     string
	}{
		"provider not shared with the target namespace": {sharedWithNamespace(xnsSource), &metav1.LabelSelector{}, consumerKindProvider, "prov-1"},
		"class not shared with the target namespace":    {&metav1.LabelSelector{}, nil, consumerKindVMClass, "src-class"},
	} {
		t.Run(name, func(t *testing.T) {
			clone := xnsClone(xnsTarget)
			cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
			r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)), labeledNamespace(xnsSource, nil))
			p := &infrav1beta1.Provider{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsSource, Name: "prov-1"}, p))
			p.Spec.ConsumerNamespaceSelector = tc.provSel
			require.NoError(t, r.Update(ctx, p))
			c := &infrav1beta1.VMClass{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsSource, Name: "src-class"}, c))
			c.Spec.ConsumerNamespaceSelector = tc.classSel
			require.NoError(t, r.Update(ctx, c))

			reconcileClone(t, r, clone, 4)
			got := getClone(t, r, clone)
			requireCloneConsumerRefused(t, got, tc.kind, xnsSource, tc.objName)
			assert.Zero(t, cp.cloneCnt, "no provider-side clone for a target that could not use it")
			assertNothingInNamespace(t, r.Client, xnsTarget)
		})
	}
}

func TestVMCloneConsumer_InFlightCloneKeepsItsStateWhenRevoked(t *testing.T) {
	ctx := context.Background()
	clone := xnsClone(xnsTarget)
	clone.Finalizers = []string{vmCloneFinalizer}
	clone.Status.Phase = infrav1beta1.ClonePhaseCloning
	clone.Status.TargetVMID = "vm-clone-1"
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)))
	p := &infrav1beta1.Provider{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: xnsSource, Name: "prov-1"}, p))
	p.Spec.ConsumerNamespaceSelector = nil
	require.NoError(t, r.Update(ctx, p))

	reconcileClone(t, r, clone, 2)
	got := getClone(t, r, clone)
	requireCloneConsumerRefused(t, got, consumerKindProvider, xnsSource, "prov-1")
	assert.Equal(t, infrav1beta1.ClonePhaseCloning, got.Status.Phase, "an in-flight clone keeps its phase")
	assert.Equal(t, "vm-clone-1", got.Status.TargetVMID, "and its target ID, to bind once granted")
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

func TestVMCloneConsumer_CloneRPCNotIssuedWhenRevokedLive(t *testing.T) {
	clone := xnsClone(xnsTarget)
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
	r, _ := newXNSCloneReconciler(t, cp, clone, grantNamespace(xnsTarget, strPtr(xnsSource)))
	revoked := xnsSharedProvider()
	revoked.Spec.ConsumerNamespaceSelector = nil
	r.APIReader = newLiveReader(t, grantNamespace(xnsTarget, strPtr(xnsSource)), nil, revoked, xnsSharedClass())

	reconcileClone(t, r, clone, 4)
	assert.Zero(t, cp.cloneCnt, "no Clone RPC when the live read shows the Provider no longer shared")
	requireCloneConsumerRefused(t, getClone(t, r, clone), consumerKindProvider, xnsSource, "prov-1")
	assertNothingInNamespace(t, r.Client, xnsTarget)
}

func TestVMCloneConsumer_ClassJSONNeverCarriesTheSelector(t *testing.T) {
	clone := xnsClone("")
	clone.Spec.Target.ClassRef = &infrav1beta1.LocalObjectReference{Name: "src-class"}
	r, _ := newXNSCloneReconciler(t, &clonerProvider{}, clone)
	data := r.classJSON(context.Background(), clone)
	require.NotEmpty(t, data)
	assert.Contains(t, data, `"cpu"`)
	assert.NotContains(t, data, "consumerNamespaceSelector")
}

func TestVMCloneConsumer_RefusedClonesMapping(t *testing.T) {
	mk := func(name string, phase infrav1beta1.ClonePhase, refused bool) *infrav1beta1.VMClone {
		c := xnsClone(xnsTarget)
		c.Name, c.UID = name, ""
		c.Status.Phase = phase
		if refused {
			c.Status.Conditions = []metav1.Condition{{Type: infrav1beta1.VMCloneConditionReady, Status: metav1.ConditionFalse, Reason: k8s.ReasonConsumerNotAllowed}}
		}
		return c
	}
	refused := mk("refused", infrav1beta1.ClonePhasePending, true)
	r, _ := newXNSCloneReconciler(t, &clonerProvider{}, refused,
		mk("running", infrav1beta1.ClonePhaseCloning, false),
		mk("done", infrav1beta1.ClonePhaseReady, true))

	reqs := r.clonesRefusedAsConsumers(context.Background(), "")
	require.Len(t, reqs, 1)
	assert.Equal(t, "refused", reqs[0].Name)
}
