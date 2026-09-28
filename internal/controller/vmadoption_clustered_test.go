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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin clustered adoption (ADR-0007 Addendum A, A3 / slice 4) on
// the manager side: discovered VMs are keyed on (host id, VM id); each adopted
// VM's domain is handed to it with a compare-and-swap owner transfer before it
// is bound; the binding records the host, the Provider and the domain's size
// from provider truth; a domain stamped for an existing VirtualMachine is never
// taken; the consumer grant is respected; and an unreachable host is unknown,
// never empty.

// fakeDomain is one domain on a fake clustered provider's host.
type fakeDomain struct {
	host, id, uuid string
	// owner is the UID stamped on the domain ("" = unstamped).
	owner       string
	cpu         int32
	memMiB      int64
	maxMemMiB   int64
	vcpusOnline int32
	power       string
}

// fakeClusteredAdopter is a clustered provider: ListVMs across hosts (with
// unreachable hosts), GetCapabilities, a compare-and-swap TransferOwner and an
// owner-checked Describe, over an in-memory set of domains.
type fakeClusteredAdopter struct {
	stubProvider

	mu          sync.Mutex
	domains     []*fakeDomain
	unreachable []string
	caps        contracts.Capabilities
	capsErr     error
	transferErr error
	transfers   []contracts.TransferOwnerRequest
	lists       int
}

var (
	_ contracts.CapabilityReporter = (*fakeClusteredAdopter)(nil)
	_ contracts.OwnerTransferer    = (*fakeClusteredAdopter)(nil)
)

func (f *fakeClusteredAdopter) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	return f.caps, f.capsErr
}

func (f *fakeClusteredAdopter) ListVMs(context.Context) (contracts.VMList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	out := contracts.VMList{UnreachableHostIDs: append([]string(nil), f.unreachable...)}
	for _, d := range f.domains {
		if slices.Contains(f.unreachable, d.host) {
			continue
		}
		raw := map[string]string{contracts.VMInfoUUIDKey: d.uuid}
		if d.owner != "" {
			raw[contracts.VMInfoOwnerUIDKey] = d.owner
		}
		out.VMs = append(out.VMs, contracts.VMInfo{ID: d.id, Name: d.id, HostID: d.host, PowerState: d.power,
			CPU: d.cpu, MemoryMiB: d.memMiB, ProviderRaw: raw})
	}
	return out, nil
}

func (f *fakeClusteredAdopter) find(host, id string) *fakeDomain {
	for _, d := range f.domains {
		if d.host == host && d.id == id {
			return d
		}
	}
	return nil
}

func (f *fakeClusteredAdopter) TransferOwner(_ context.Context, req contracts.TransferOwnerRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transfers = append(f.transfers, req)
	if f.transferErr != nil {
		return f.transferErr
	}
	if slices.Contains(f.unreachable, req.VM.HostID) {
		return contracts.NewHostUnavailableError("host unreachable", nil)
	}
	d := f.find(req.VM.HostID, req.VM.ID)
	if d == nil || d.uuid != req.ExpectedUUID {
		return contracts.NewNotFoundError("not found", nil)
	}
	if d.owner != "" && d.owner != req.VM.Owner.UID && !slices.Contains(req.ReplaceableOwnerUIDs, d.owner) {
		return contracts.NewConflictError("owned by another VirtualMachine", nil)
	}
	d.owner = req.VM.Owner.UID
	return nil
}

func (f *fakeClusteredAdopter) Describe(_ context.Context, ref contracts.VMRef) (contracts.DescribeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.find(ref.HostID, ref.ID)
	if d == nil || d.owner == "" || d.owner != ref.Owner.UID {
		return contracts.DescribeResponse{Exists: false}, nil
	}
	return contracts.DescribeResponse{Exists: true, PowerState: d.power, MaxMemoryMiB: d.maxMemMiB, VCPUs: d.vcpusOnline}, nil
}

// transfersTo returns the recorded transfers to (host, id).
func (f *fakeClusteredAdopter) transfersTo(host, id string) []contracts.TransferOwnerRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []contracts.TransferOwnerRequest
	for _, t := range f.transfers {
		if t.VM.HostID == host && t.VM.ID == id {
			out = append(out, t)
		}
	}
	return out
}

const clusterNS = "infra"

// clusteredAdoptionProvider is a ready clustered Provider with adoption on.
func clusteredAdoptionProvider() *infravirtrigaudiov1beta1.Provider {
	p := readyProvider(clusterNS, "prov-c")
	p.UID = "prov-c-uid"
	p.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	p.Annotations = map[string]string{AdoptionAnnotation: "true"}
	return p
}

// clusterHost is a Host of provider in pool "pool-1".
func clusterHost(name, provider string) *infravirtrigaudiov1beta1.Host {
	return &infravirtrigaudiov1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: clusterNS},
		Spec: infravirtrigaudiov1beta1.HostSpec{
			ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: provider},
			PoolRef:     infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool-1"},
			Endpoint:    "qemu+ssh://virt@" + name + "/system",
		},
	}
}

// clusteredAdoptionReconciler builds the adoption reconciler over a fake
// client that assigns UIDs on create (as the API server does) and a resolver
// returning prov.
func clusteredAdoptionReconciler(t *testing.T, prov contracts.Provider, objs ...client.Object) *VMAdoptionReconciler {
	t.Helper()
	s := coverageTestScheme(t)
	var n int
	var mu sync.Mutex
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithIndex(&infravirtrigaudiov1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithStatusSubresource(&infravirtrigaudiov1beta1.VirtualMachine{}, &infravirtrigaudiov1beta1.Provider{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					mu.Lock()
					n++
					obj.SetUID(types.UID(fmt.Sprintf("uid-created-%d", n)))
					mu.Unlock()
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	return &VMAdoptionReconciler{Client: c, Scheme: s, RemoteResolver: &stubResolver{provider: prov}}
}

// reconcileAdoption runs one adoption reconcile of prov-c.
func reconcileAdoption(t *testing.T, r *VMAdoptionReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: clusterNS, Name: "prov-c"}})
	require.NoError(t, err)
	return res
}

// adoptedVMGet fetches a VirtualMachine in the cluster namespace.
func adoptedVMGet(t *testing.T, r *VMAdoptionReconciler, name string) *infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusterNS, Name: name}, vm))
	return vm
}

// adoptionStatus fetches prov-c's adoption status.
func adoptionStatus(t *testing.T, r *VMAdoptionReconciler) *infravirtrigaudiov1beta1.ProviderAdoptionStatus {
	t.Helper()
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusterNS, Name: "prov-c"}, p))
	require.NotNil(t, p.Status.Adoption)
	return p.Status.Adoption
}

// routedAdoptionCaps are the capabilities of a slice 4 clustered provider.
var routedAdoptionCaps = contracts.Capabilities{SupportsClustering: true, SupportsRoutedAdoption: true}

// TestClusteredAdoption_AdoptsByHostAndIDAndRecordsTheBinding: the same
// domain name on two hosts is two adoptions; each VirtualMachine is handed its
// domain (owner transfer, replacing only a deleted VirtualMachine's stamp)
// before it is bound to its host, and records its size from provider truth; a
// domain stamped for a VirtualMachine that still exists is never taken.
func TestClusteredAdoption_AdoptsByHostAndIDAndRecordsTheBinding(t *testing.T) {
	live := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "team-a", UID: "uid-live"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "other", Namespace: "team-a"}},
	}
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 2, memMiB: 2048, maxMemMiB: 8192, vcpusOnline: 4, power: "On"},
		{host: "host-b", id: "web", uuid: "uuid-b-web", owner: "uid-deleted", cpu: 1, memMiB: 1024, maxMemMiB: 1024, vcpusOnline: 1, power: "Off"},
		{host: "host-a", id: "team-a.db", uuid: "uuid-a-db", owner: "uid-live", cpu: 1, memMiB: 1024, power: "On"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(),
		clusterHost("host-a", "prov-c"), clusterHost("host-b", "prov-c"), live)

	res := reconcileAdoption(t, r)
	assert.Equal(t, clusteredAdoptionInterval, res.RequeueAfter)

	nameA, nameB := clusteredAdoptedVMName("host-a", "web"), clusteredAdoptedVMName("host-b", "web")
	require.NotEqual(t, nameA, nameB, "the same domain name on two hosts is two VirtualMachines")

	vmA := adoptedVMGet(t, r, nameA)
	assert.Equal(t, "web", vmA.Status.ID)
	require.NotNil(t, vmA.Status.Placement)
	assert.Equal(t, "host-a", vmA.Status.Placement.Host)
	assert.Equal(t, "pool-1", vmA.Status.Placement.Pool)
	assert.Empty(t, vmA.Status.Placement.PendingHost)
	assert.Equal(t, &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: clusterNS, Name: "prov-c", UID: "prov-c-uid"}, vmA.Status.BoundProvider)
	assert.Equal(t, map[string]string{AdoptedHostAnnotation: "host-a", AdoptedIDAnnotation: "web"}, vmA.Annotations)
	// Size from provider truth: effective size 2 vCPU / 2048 MiB, the CPU
	// raised to the 4 vCPUs Describe reports online, the ceiling Describe's
	// memory maximum.
	require.NotNil(t, vmA.Status.CurrentResources)
	assert.EqualValues(t, 4, *vmA.Status.CurrentResources.CPU)
	assert.EqualValues(t, 2048, *vmA.Status.CurrentResources.MemoryMiB)
	require.NotNil(t, vmA.Status.Placement.MemoryCeilingMiB)
	assert.EqualValues(t, 8192, *vmA.Status.Placement.MemoryCeilingMiB)

	vmB := adoptedVMGet(t, r, nameB)
	assert.Equal(t, "web", vmB.Status.ID)
	assert.Equal(t, "host-b", vmB.Status.Placement.Host)
	require.NotNil(t, vmB.Status.Placement.MemoryCeilingMiB)
	assert.EqualValues(t, 0, *vmB.Status.Placement.MemoryCeilingMiB, "no memory beyond its own size: ceiling 0")

	// Each domain was handed to its own VirtualMachine, compare-and-swap.
	ta := prov.transfersTo("host-a", "web")
	require.Len(t, ta, 1)
	assert.Equal(t, string(vmA.UID), ta[0].VM.Owner.UID)
	assert.Equal(t, clusterNS, ta[0].VM.Owner.Namespace)
	assert.Equal(t, nameA, ta[0].VM.Owner.Name)
	assert.Empty(t, ta[0].ReplaceableOwnerUIDs, "an unstamped domain has nothing to replace")
	assert.Equal(t, "uuid-a-web", ta[0].ExpectedUUID)
	tb := prov.transfersTo("host-b", "web")
	require.Len(t, tb, 1)
	assert.Equal(t, []string{"uid-deleted"}, tb[0].ReplaceableOwnerUIDs, "only a deleted VirtualMachine's stamp is replaceable")
	assert.Equal(t, string(vmA.UID), prov.find("host-a", "web").owner)
	assert.Equal(t, string(vmB.UID), prov.find("host-b", "web").owner)

	// The domain a live VirtualMachine owns was neither transferred nor adopted.
	assert.Empty(t, prov.transfersTo("host-a", "team-a.db"))
	assert.Equal(t, "uid-live", prov.find("host-a", "team-a.db").owner)
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &vms, client.InNamespace(clusterNS)))
	assert.Len(t, vms.Items, 2)

	st := adoptionStatus(t, r)
	assert.EqualValues(t, 2, st.AdoptedVMs)
	assert.EqualValues(t, 0, st.FailedAdoptions)

	// The committed-capacity accounting counts the adopted VM from now on, at
	// its recorded size and memory ceiling.
	cpu, mem, err := committedOnHost(context.Background(), r.Client, types.NamespacedName{Namespace: clusterNS, Name: "prov-c"}, "host-a")
	require.NoError(t, err)
	assert.EqualValues(t, 4, cpu)
	assert.EqualValues(t, 8192, mem)

	// A second discovery finds both domains managed by (host, id): nothing new.
	prov.transfers = nil
	r.setLastDiscovery(t, time.Now().Add(-time.Hour))
	reconcileAdoption(t, r)
	assert.Empty(t, prov.transfers, "bound VMs are managed; nothing is transferred again")
	require.NoError(t, r.List(context.Background(), &vms, client.InNamespace(clusterNS)))
	assert.Len(t, vms.Items, 2)
}

// setLastDiscovery backdates prov-c's last discovery so the next reconcile
// discovers again.
func (r *VMAdoptionReconciler) setLastDiscovery(t *testing.T, at time.Time) {
	t.Helper()
	p := &infravirtrigaudiov1beta1.Provider{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: clusterNS, Name: "prov-c"}, p))
	ts := metav1.NewTime(at)
	p.Status.Adoption.LastDiscoveryTime = &ts
	require.NoError(t, r.Status().Update(context.Background(), p))
}

// TestClusteredAdoption_UnreachableHostIsUnknownNotEmpty: a host the provider
// could not list is unknown. The VM bound to it is not touched (not unbound,
// not deleted, not re-adopted), a VirtualMachine waiting to bind a domain on
// it keeps waiting, nothing is sent to it, and the Provider's adoption status
// names it and discovery is retried sooner.
func TestClusteredAdoption_UnreachableHostIsUnknownNotEmpty(t *testing.T) {
	bound := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: clusterNS, UID: "uid-bound"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c"}},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "db",
			Placement:     &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-c"},
			BoundProvider: &infravirtrigaudiov1beta1.BoundProviderRef{Namespace: clusterNS, Name: "prov-c", UID: "prov-c-uid"}},
	}
	waiting := awaitingAdoptedVM("host-c", "cache", "uid-waiting")
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, unreachable: []string{"host-c"}, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 1, memMiB: 1024, power: "On"},
		{host: "host-c", id: "db", uuid: "uuid-c-db", owner: "uid-bound", power: "On"},
		{host: "host-c", id: "cache", uuid: "uuid-c-cache", owner: "uid-waiting", power: "On"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(),
		clusterHost("host-a", "prov-c"), clusterHost("host-c", "prov-c"), bound, waiting)
	boundBefore := adoptedVMGet(t, r, "db").Status.DeepCopy()
	waitingBefore := adoptedVMGet(t, r, waiting.Name).Status.DeepCopy()

	res := reconcileAdoption(t, r)
	assert.Equal(t, clusteredAdoptionRetryInterval, res.RequeueAfter, "an unreachable host is retried sooner")

	assert.Equal(t, boundBefore, &adoptedVMGet(t, r, "db").Status, "a VM on an unreachable host is not gone")
	assert.Equal(t, waitingBefore, &adoptedVMGet(t, r, waiting.Name).Status, "an adoption waiting on an unreachable host keeps waiting")
	assert.Empty(t, prov.transfersTo("host-c", "db"))
	assert.Empty(t, prov.transfersTo("host-c", "cache"))
	assert.Equal(t, "web", adoptedVMGet(t, r, clusteredAdoptedVMName("host-a", "web")).Status.ID, "the reachable host is adopted from")

	st := adoptionStatus(t, r)
	assert.Contains(t, st.Message, "host-c")
	assert.Contains(t, st.Message, "unknown (not absent)")
	assert.NotContains(t, st.Message, "qemu+ssh", "the host's endpoint never reaches the status")
}

// awaitingAdoptedVM is an adopted VirtualMachine created for (host, id) that
// is not bound yet.
func awaitingAdoptedVM(host, id, uid string) *infravirtrigaudiov1beta1.VirtualMachine {
	return &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusteredAdoptedVMName(host, id), Namespace: clusterNS, UID: types.UID(uid),
			Labels:      map[string]string{AdoptedLabel: AdoptedLabelValue},
			Annotations: map[string]string{AdoptedHostAnnotation: host, AdoptedIDAnnotation: id},
		},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c", Namespace: clusterNS},
			ClassRef:    infravirtrigaudiov1beta1.ObjectRef{Name: "adopted-1cpu-1024mb", Namespace: clusterNS},
		},
	}
}

// TestClusteredAdoption_CompletesALostBinding: the owner transfer succeeded
// but the binding write was lost. The domain now carries the waiting
// VirtualMachine's stamp; the next discovery completes that binding (an
// idempotent transfer, nothing replaced) instead of adopting the domain again.
func TestClusteredAdoption_CompletesALostBinding(t *testing.T) {
	waiting := awaitingAdoptedVM("host-a", "web", "uid-waiting")
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", owner: "uid-waiting", cpu: 1, memMiB: 1024, maxMemMiB: 1024, vcpusOnline: 1, power: "On"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"), waiting)

	reconcileAdoption(t, r)
	vm := adoptedVMGet(t, r, waiting.Name)
	assert.Equal(t, "web", vm.Status.ID)
	assert.Equal(t, "host-a", vm.Status.Placement.Host)
	transfers := prov.transfersTo("host-a", "web")
	require.Len(t, transfers, 1)
	assert.Empty(t, transfers[0].ReplaceableOwnerUIDs, "its own stamp is never 'replaced'")
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &vms))
	assert.Len(t, vms.Items, 1, "no second VirtualMachine")
}

// TestClusteredAdoption_StampForAWaitingVMOfAnotherKeyIsNotTaken: a domain
// stamped with the UID of a waiting adopted VirtualMachine that was created
// for ANOTHER (host, id) is that VirtualMachine's business: never completed
// into, never re-adopted.
func TestClusteredAdoption_StampForAWaitingVMOfAnotherKeyIsNotTaken(t *testing.T) {
	waiting := awaitingAdoptedVM("host-a", "web", "uid-waiting")
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-b", id: "web", uuid: "uuid-b-web", owner: "uid-waiting", power: "On"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(),
		clusterHost("host-a", "prov-c"), clusterHost("host-b", "prov-c"), waiting)

	reconcileAdoption(t, r)
	assert.Empty(t, prov.transfers)
	assert.Empty(t, adoptedVMGet(t, r, waiting.Name).Status.ID)
}

// TestClusteredAdoption_RespectsTheConsumerGrant: an existing adopted
// VirtualMachine waiting for (host, id) that references another namespace's
// VMClass is bound only when that class grants the Provider's namespace — and
// without the grant the domain's owner is not even transferred to it.
func TestClusteredAdoption_RespectsTheConsumerGrant(t *testing.T) {
	for name, tc := range map[string]struct {
		sel   *metav1.LabelSelector
		bound bool
	}{
		"class not shared: not bound, owner not transferred": {nil, false},
		"class shared: bound":                                 {&metav1.LabelSelector{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			waiting := awaitingAdoptedVM("host-a", "web", "uid-waiting")
			waiting.Spec.ClassRef = infravirtrigaudiov1beta1.ObjectRef{Name: "c", Namespace: "platform"}
			prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
				{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 1, memMiB: 1024, power: "On"},
			}}
			r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"),
				waiting, grantedClass("platform", "c", tc.sel))

			reconcileAdoption(t, r)
			vm := adoptedVMGet(t, r, waiting.Name)
			if tc.bound {
				assert.Equal(t, "web", vm.Status.ID)
				assert.Len(t, prov.transfersTo("host-a", "web"), 1)
				return
			}
			assert.Empty(t, vm.Status.ID)
			assert.Empty(t, prov.transfers, "an ungranted VirtualMachine is never handed the domain")
			assert.Empty(t, prov.find("host-a", "web").owner)
		})
	}
}

// TestClusteredAdoption_FailedTransferDoesNotBind: when the provider refuses
// the owner transfer (the domain was taken over in between), the created
// VirtualMachine is not bound — no status.id, no host — and the failure is
// counted.
func TestClusteredAdoption_FailedTransferDoesNotBind(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps,
		transferErr: contracts.NewConflictError("owned by another VirtualMachine", nil),
		domains: []*fakeDomain{
			{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 1, memMiB: 1024, power: "On"},
		}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))

	res := reconcileAdoption(t, r)
	assert.Equal(t, clusteredAdoptionRetryInterval, res.RequeueAfter)
	vm := adoptedVMGet(t, r, clusteredAdoptedVMName("host-a", "web"))
	assert.Empty(t, vm.Status.ID)
	assert.Nil(t, vm.Status.Placement)
	assert.EqualValues(t, 1, adoptionStatus(t, r).FailedAdoptions)
}

// TestClusteredAdoption_HostNotOfTheProviderIsNotAdoptedFrom: a listed host id
// that is not a Host of this Provider (or is being deleted) is never bound to.
func TestClusteredAdoption_HostNotOfTheProviderIsNotAdoptedFrom(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-x", id: "web", uuid: "uuid-x-web", power: "On"},
		{host: "host-y", id: "web", uuid: "uuid-y-web", power: "On"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-y", "someone-else"))

	reconcileAdoption(t, r)
	assert.Empty(t, prov.transfers)
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &vms))
	assert.Empty(t, vms.Items)
	assert.EqualValues(t, 2, adoptionStatus(t, r).FailedAdoptions)
}

// TestClusteredAdoption_CapabilityGate: a clustered provider that does not
// report supports_routed_adoption (older than slice 4), or whose capabilities
// cannot be read, is not listed and nothing is adopted.
func TestClusteredAdoption_CapabilityGate(t *testing.T) {
	for name, prov := range map[string]*fakeClusteredAdopter{
		"no routed adoption": {caps: contracts.Capabilities{SupportsClustering: true}},
		"capabilities fail":  {capsErr: fmt.Errorf("provider unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			prov.domains = []*fakeDomain{{host: "host-a", id: "web", uuid: "uuid-a-web", power: "On"}}
			r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))
			res := reconcileAdoption(t, r)
			assert.Equal(t, clusteredAdoptionRetryInterval, res.RequeueAfter)
			assert.Zero(t, prov.lists, "nothing is listed")
			assert.Empty(t, prov.transfers)
			var vms infravirtrigaudiov1beta1.VirtualMachineList
			require.NoError(t, r.List(context.Background(), &vms))
			assert.Empty(t, vms.Items)
			assert.NotEmpty(t, adoptionStatus(t, r).Message)
		})
	}
	r := clusteredAdoptionReconciler(t, &fakeClusteredAdopter{caps: contracts.Capabilities{SupportsClustering: true}},
		clusteredAdoptionProvider())
	reconcileAdoption(t, r)
	assert.Equal(t, clusteredAdoptionNotSupportedMessage, adoptionStatus(t, r).Message)
}

// TestPlanClusteredAdoption_KeysOnHostAndID: a VirtualMachine bound to
// (host-a, web) manages that domain only; host-b's "web" is still adoptable,
// and a VM listed without a host id or UUID is never adopted.
func TestPlanClusteredAdoption_KeysOnHostAndID(t *testing.T) {
	provider := clusteredAdoptionProvider()
	bound := infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: clusterNS, UID: "uid-web"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c"}},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "web",
			Placement: &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-a"}},
	}
	info := func(host, id string) contracts.VMInfo {
		return contracts.VMInfo{ID: id, Name: id, HostID: host, ProviderRaw: map[string]string{contracts.VMInfoUUIDKey: "u-" + host + id}}
	}
	noUUID := info("host-c", "x")
	delete(noUUID.ProviderRaw, contracts.VMInfoUUIDKey)
	plan := planClusteredAdoption(provider, contracts.VMList{VMs: []contracts.VMInfo{
		info("host-a", "web"), info("host-b", "web"), info("", "web"), noUUID,
	}}, []infravirtrigaudiov1beta1.VirtualMachine{bound})

	require.Len(t, plan.adopt, 1)
	assert.Equal(t, "host-b", plan.adopt[0].HostID, "a name bound on host-a says nothing about host-b's domain")
	assert.Equal(t, 2, plan.skipped, "no host id or no UUID: never adopted")
	assert.Empty(t, plan.complete)
}

// TestClusteredAdoptedVMName: deterministic, distinct per (host, id), a DNS
// label.
func TestClusteredAdoptedVMName(t *testing.T) {
	a := clusteredAdoptedVMName("host-a", "team-a.web")
	assert.Equal(t, a, clusteredAdoptedVMName("host-a", "team-a.web"), "deterministic: a retry finds the same VirtualMachine")
	assert.NotEqual(t, a, clusteredAdoptedVMName("host-b", "team-a.web"))
	assert.True(t, strings.HasPrefix(a, "team-a-web-"), a)
	long := clusteredAdoptedVMName("host-a", strings.Repeat("very-long-domain-name.", 10))
	assert.LessOrEqual(t, len(long), adoptedVMNameMaxLen)
	assert.Regexp(t, `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, long)
	assert.Regexp(t, `^vm-[0-9a-f]{10}$`, clusteredAdoptedVMName("host-a", "___"))
}

// TestAdoptedSize: the recorded size is the effective size, the CPU raised
// (never lowered) to the vCPUs online, and the memory ceiling Describe's
// maximum when it exceeds the memory.
func TestAdoptedSize(t *testing.T) {
	cpu2, mem2048 := int32(2), int64(2048)
	vm := &infravirtrigaudiov1beta1.VirtualMachine{Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
		Resources: &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu2, MemoryMiB: &mem2048}}}
	class := smallVMClass(clusterNS)
	info := contracts.VMInfo{CPU: 2, MemoryMiB: 2048}

	cpu, mem, ceiling := adoptedSize(vm, class, info, contracts.DescribeResponse{VCPUs: 1, MaxMemoryMiB: 2048})
	assert.EqualValues(t, 2, cpu, "never below the effective size")
	assert.EqualValues(t, 2048, mem)
	assert.EqualValues(t, 0, ceiling)

	cpu, _, ceiling = adoptedSize(vm, class, info, contracts.DescribeResponse{VCPUs: 6, MaxMemoryMiB: 8192})
	assert.EqualValues(t, 6, cpu)
	assert.EqualValues(t, 8192, ceiling)

	cpu, mem, _ = adoptedSize(vm, nil, contracts.VMInfo{CPU: 3, MemoryMiB: 4096}, contracts.DescribeResponse{})
	assert.EqualValues(t, 3, cpu, "no readable class: the listed size")
	assert.EqualValues(t, 4096, mem)
}
