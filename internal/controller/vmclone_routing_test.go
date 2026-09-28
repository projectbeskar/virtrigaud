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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the VMClone controller's side of ADR-0007 Addendum A, A1:
// the clone source is addressed through vmRefFor (routed to its bound host on a
// clustered provider, never sent without one), and the cloned target VM's
// binding is written in the same status write as its Status.ID.

func clusteredCloneFixture(t *testing.T, src *infrav1beta1.VirtualMachine, cp *clonerProvider, extra ...client.Object) (*VMCloneReconciler, *infrav1beta1.VMClone) {
	t.Helper()
	return clusteredCloneFixtureWithClass(t, src, cp, srcCloneClass(), extra...)
}

// clusteredCloneFixtureWithClass is clusteredCloneFixture with class as the
// source's VMClass ("src-class").
func clusteredCloneFixtureWithClass(t *testing.T, src *infrav1beta1.VirtualMachine, cp *clonerProvider,
	class *infrav1beta1.VMClass, extra ...client.Object) (*VMCloneReconciler, *infrav1beta1.VMClone) {
	t.Helper()
	prov := runningProvider("default", "prov-c")
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	prov.Spec.Topology = infrav1beta1.ProviderTopologyCluster
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-c", Namespace: "default"},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: src.Name}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-c-target"},
		},
	}
	objs := append([]client.Object{prov, src, clone, class, hostPoolCR("pool-a", "default", "prov-c")}, extra...)
	if len(extra) == 0 {
		objs = append(objs, readyCloneHost("host-alpha", "prov-c"))
	}
	// A slice 3 clustered provider routes clones (a test can clear it on cp).
	cp.caps.SupportsClustering = true
	cp.caps.SupportsRoutedClone = true
	return newClusteredCloneReconciler(t, cp, objs...), clone
}

// srcCloneClass is "src-class" in "default", the VMClass sourceVMWithID's
// source VMs use: 2 vCPU, 4 GiB.
func srcCloneClass() *infrav1beta1.VMClass {
	return &infrav1beta1.VMClass{
		ObjectMeta: metav1.ObjectMeta{Name: "src-class", Namespace: "default"},
		Spec:       infrav1beta1.VMClassSpec{CPU: 2, Memory: resource.MustParse("4Gi")},
	}
}

// readyCloneHost is a Ready, schedulable Host of provider in "default", with
// ample allocatable capacity (a clustered clone is admitted against it).
func readyCloneHost(name, provider string) *infrav1beta1.Host {
	return &infrav1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: infrav1beta1.HostSpec{
			ProviderRef: infrav1beta1.LocalObjectReference{Name: provider},
			PoolRef:     infrav1beta1.LocalObjectReference{Name: "pool-a"},
			Endpoint:    "qemu+ssh://virt@" + name + "/system",
			Schedulable: true,
		},
		Status: infrav1beta1.HostStatus{Health: infrav1beta1.HostHealthReady,
			AllocatableCPU: i32p(64), AllocatableMemoryMiB: i64p(1 << 20)},
	}
}

// newClusteredCloneReconciler is newCloneReconciler whose fake client assigns
// a UID on Create, as the API server does: the clustered clone stamps its
// target VirtualMachine's UID onto the clone.
func newClusteredCloneReconciler(t *testing.T, cp *clonerProvider, objs ...client.Object) *VMCloneReconciler {
	t.Helper()
	ensureCloneUIDs(objs...)
	s := cloneTestScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&infrav1beta1.VMClone{}, &infrav1beta1.VirtualMachine{}, &infrav1beta1.Host{}).
		WithIndex(&infrav1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID("uid-" + obj.GetNamespace() + "-" + obj.GetName()))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	return &VMCloneReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: cp}, Recorder: record.NewFakeRecorder(50)}
}

func TestVMClone_Clustered_SourceRoutedAndTargetBoundInSameWrite(t *testing.T) {
	src := sourceVMWithID("default", "src-c", "prov-c", "src-c")
	src.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "clone-c-target"}}
	r, clone := clusteredCloneFixture(t, src, cp)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.Equal(t, 1, cp.cloneCnt)
	assert.Equal(t, "src-c", cp.lastClone.Source.ID)
	assert.Equal(t, "host-alpha", cp.lastClone.Source.HostID, "the clone is routed to the source VM's bound host")

	target := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-c-target"}, target))
	assert.Equal(t, "clone-c-target", target.Status.ID)
	require.NotNil(t, target.Status.Placement, "the target's binding lands in the same write as Status.ID")
	assert.Equal(t, "host-alpha", target.Status.Placement.Host)
	assert.Equal(t, "pool-a", target.Status.Placement.Pool)
	assert.Empty(t, target.Status.Placement.PendingHost)
}

func TestVMClone_Clustered_UnboundSourceNeverCallsProvider(t *testing.T) {
	src := sourceVMWithID("default", "src-u", "prov-c", "src-u") // id, but no binding
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "x"}}
	r, clone := clusteredCloneFixture(t, src, cp)

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt, "an unbound clustered source is never sent a per-VM call")
	got := &infrav1beta1.VMClone{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(clone), got))
	assert.Equal(t, infrav1beta1.ClonePhasePending, got.Status.Phase)
	assert.Contains(t, got.Status.Message, "no confirmed host binding")
}

// resumeBindFixture is a clustered clone whose Clone RPC already returned
// (status.targetVMID persisted) and whose target VirtualMachine — created by
// this clone — already records placement: the bind resumes on it.
func resumeBindFixture(t *testing.T, targetPlacement *infrav1beta1.PlacementStatus) (*VMCloneReconciler, *infrav1beta1.VMClone, *clonerProvider) {
	t.Helper()
	src := sourceVMWithID("default", "src-r", "prov-c", "src-r")
	src.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	prov := runningProvider("default", "prov-c")
	prov.Spec.Type = infrav1beta1.ProviderTypeLibvirt
	prov.Spec.Topology = infrav1beta1.ProviderTopologyCluster
	target := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "clone-r-target",
			Namespace:   "default",
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{CloneAnnotationCloneUID: "uid-clone-r"},
		},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "prov-c"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "src-class"},
		},
		Status: infrav1beta1.VirtualMachineStatus{Placement: targetPlacement},
	}
	clone := &infrav1beta1.VMClone{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-r", Namespace: "default", UID: "uid-clone-r", Finalizers: []string{vmCloneFinalizer}},
		Spec: infrav1beta1.VMCloneSpec{
			Source: infrav1beta1.CloneSource{VMRef: &infrav1beta1.LocalObjectReference{Name: src.Name}},
			Target: infrav1beta1.VMCloneTarget{Name: "clone-r-target"},
		},
		Status: infrav1beta1.VMCloneStatus{Phase: infrav1beta1.ClonePhaseCloning, TargetVMID: "default.clone-r-target"},
	}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-r-target"}}
	return newCloneReconciler(cloneTestScheme(t), &stubResolver{provider: cp}, prov, src, target, clone), clone, cp
}

// TestVMClone_Clustered_BindKeepsPlacementFieldsItDoesNotOwn pins that binding
// the cloned VM writes only the placement fields the clone owns (host, pool,
// scheduling time and reason) and promotes a pending host that names the
// landing host (ADR-0007 Addendum A, A2). It never replaces the target's whole
// placement: excludedHosts survives the bind.
func TestVMClone_Clustered_BindKeepsPlacementFieldsItDoesNotOwn(t *testing.T) {
	r, clone, cp := resumeBindFixture(t, &infrav1beta1.PlacementStatus{
		PendingHost:   "host-alpha",
		ExcludedHosts: []string{"host-zeta"},
	})

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt, "a resumed bind never re-clones")
	target := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-r-target"}, target))
	assert.Equal(t, "default.clone-r-target", target.Status.ID)
	require.NotNil(t, target.Status.Placement)
	assert.Equal(t, "host-alpha", target.Status.Placement.Host)
	assert.Equal(t, "pool-a", target.Status.Placement.Pool)
	assert.NotNil(t, target.Status.Placement.LastScheduledTime)
	assert.Empty(t, target.Status.Placement.PendingHost, "the pending host the clone landed on is promoted to the binding")
	assert.Equal(t, []string{"host-zeta"}, target.Status.Placement.ExcludedHosts, "a field the clone does not own is preserved")
}

// TestVMClone_Clustered_BindNeverDropsAPendingHostElsewhere pins that a
// pending create on ANOTHER host — which may have left a domain there that
// only the finalizer's owner-checked cleanup can remove — is never dropped by
// the bind.
func TestVMClone_Clustered_BindNeverDropsAPendingHostElsewhere(t *testing.T) {
	r, clone, _ := resumeBindFixture(t, &infrav1beta1.PlacementStatus{PendingHost: "host-beta"})

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	target := &infrav1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "clone-r-target"}, target))
	require.NotNil(t, target.Status.Placement)
	assert.Equal(t, "host-beta", target.Status.Placement.PendingHost, "a pending create elsewhere is never dropped")
}
