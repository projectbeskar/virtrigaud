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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the linked-clone gate: it fails CLOSED (a linked clone is
// issued only to a provider that says it can make one), and a libvirt linked
// clone into another namespace is refused — a cross-namespace grant (#340)
// covers a point-in-time copy, not ongoing access to the source's disk.

// linkedCloneFixture is a linked VMClone of src-vm (on Provider prov-1 of type
// providerType) in its own namespace.
func linkedCloneFixture(t *testing.T, providerType infrav1beta1.ProviderType, cp contracts.Provider) (*VMCloneReconciler, *infrav1beta1.VMClone) {
	t.Helper()
	ns := "default"
	prov := runningProvider(ns, "prov-1")
	prov.Spec.Type = providerType
	src := sourceVMWithID(ns, "src-vm", "prov-1", "vm-source-123")
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-linked", Namespace: ns},
		Spec: infrav1beta1.VMCloneSpec{
			Source:  infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: "src-vm"}},
			Target:  infrav1beta1.VMCloneTarget{Name: "clone-target"},
			Options: &infrav1beta1.CloneOptions{Type: infrav1beta1.CloneTypeLinkedClone},
		},
	}
	return newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, prov, src, clone), clone
}

func TestVMClone_LinkedCapabilityQueryFailsIsRetried(t *testing.T) {
	cp := &clonerProvider{capsErr: errors.New("provider unavailable"), cloneResp: contracts.CloneResponse{TargetVmID: "vm-1"}}
	r, clone := linkedCloneFixture(t, infrav1beta1.ProviderTypeVSphere, cp)
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase, "a failed capability query is waited on, never assumed")
	assert.Zero(t, cp.cloneCnt)

	// Once the capabilities can be read, the linked clone proceeds.
	cp.capsErr = nil
	cp.caps = contracts.Capabilities{SupportsLinkedClones: true}
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
	assert.Equal(t, 1, cp.cloneCnt)
	assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
}

// TestVMClone_LinkedSupportedProceeds pins that a provider reporting linked
// clones (vSphere, single-host libvirt) still gets them in its own namespace.
func TestVMClone_LinkedSupportedProceeds(t *testing.T) {
	for _, typ := range []infrav1beta1.ProviderType{infrav1beta1.ProviderTypeVSphere, infrav1beta1.ProviderTypeLibvirt} {
		t.Run(string(typ), func(t *testing.T) {
			cp := &clonerProvider{caps: contracts.Capabilities{SupportsLinkedClones: true}, cloneResp: contracts.CloneResponse{TargetVmID: "vm-clone-1"}}
			r, clone := linkedCloneFixture(t, typ, cp)
			reconcileTwice(t, r, client.ObjectKeyFromObject(clone))
			require.Equal(t, 1, cp.cloneCnt)
			assert.True(t, cp.lastClone.Linked)
			assert.Equal(t, infrav1beta1.ClonePhaseReady, getClone(t, r, clone).Status.Phase)
		})
	}
}

// xnsLinkedClone runs a VMClone of type cloneType from team-a into the granted
// team-b through a shared Provider of type providerType, and returns the
// clone as persisted.
func xnsLinkedClone(t *testing.T, providerType infrav1beta1.ProviderType, cloneType infrav1beta1.CloneType, cp *clonerProvider) *infrav1beta1.VMClone {
	t.Helper()
	clone := xnsClone(xnsTarget)
	clone.Spec.Options = &infrav1beta1.CloneOptions{Type: cloneType}
	prov := xnsSharedProvider()
	prov.Spec.Type = providerType
	s := xnsCloneScheme(t)
	objs := []client.Object{prov, xnsSharedClass(), sourceVMWithID(xnsSource, "src-vm", "prov-1", "vm-source-123"), clone,
		grantNamespace(xnsTarget, strPtr(xnsSource))}
	r := newCloneReconciler(s, &stubResolver{provider: cp}, objs...)
	reconcileClone(t, r, clone, 4)
	return getClone(t, r, clone)
}

func TestVMClone_LinkedCrossNamespace(t *testing.T) {
	t.Run("libvirt is refused, even with the grant", func(t *testing.T) {
		cp := &clonerProvider{caps: contracts.Capabilities{SupportsLinkedClones: true}, cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
		got := xnsLinkedClone(t, infrav1beta1.ProviderTypeLibvirt, infrav1beta1.CloneTypeLinkedClone, cp)
		assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
		assert.Zero(t, cp.cloneCnt)
		ready := readyCondition(got.Status.Conditions, infrav1beta1.VMCloneConditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, cloneReasonLinkedCrossNamespace, ready.Reason)
	})
	t.Run("vSphere proceeds", func(t *testing.T) {
		cp := &clonerProvider{caps: contracts.Capabilities{SupportsLinkedClones: true}, cloneResp: contracts.CloneResponse{TargetVmID: "vm-1"}}
		xnsLinkedClone(t, infrav1beta1.ProviderTypeVSphere, infrav1beta1.CloneTypeLinkedClone, cp)
		require.Equal(t, 1, cp.cloneCnt)
		assert.True(t, cp.lastClone.Linked)
	})
	t.Run("a libvirt full clone into another namespace proceeds", func(t *testing.T) {
		cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "team-b.web"}}
		xnsLinkedClone(t, infrav1beta1.ProviderTypeLibvirt, infrav1beta1.CloneTypeFullClone, cp)
		require.Equal(t, 1, cp.cloneCnt)
		assert.False(t, cp.lastClone.Linked)
	})
}

// TestVMClone_Clustered_LinkedIsRefused: a clustered libvirt provider does not
// advertise linked clones, so the gate refuses one before anything is created.
func TestVMClone_Clustered_LinkedIsRefused(t *testing.T) {
	cp := &clonerProvider{caps: contracts.Capabilities{SupportsClustering: true, SupportsLinkedClones: false}}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	live := getClone(t, r, clone)
	live.Spec.Options = &infrav1beta1.CloneOptions{Type: infrav1beta1.CloneTypeLinkedClone}
	require.NoError(t, r.Update(context.Background(), live))
	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	got := getClone(t, r, clone)
	assert.Equal(t, infrav1beta1.ClonePhaseFailed, got.Status.Phase)
	assert.Zero(t, cp.cloneCnt)
	err := r.Get(context.Background(), targetKey, &infrav1beta1.VirtualMachine{})
	assert.Error(t, err, "no target VM is created for a refused linked clone")
}
