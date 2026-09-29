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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// owner is the UID stamped on the domain ("" = unstamped); ownerNS and
	// ownerName the namespace and name the stamp records.
	owner       string
	ownerNS     string
	ownerName   string
	cpu         int32
	memMiB      int64
	maxMemMiB   int64
	vcpusOnline int32
	power       string
	disks       []string
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
	describeErr error
	transfers   []contracts.TransferOwnerRequest
	lists       int
}

var (
	_ contracts.CapabilityReporter = (*fakeClusteredAdopter)(nil)
	_ contracts.OwnerTransferrer   = (*fakeClusteredAdopter)(nil)
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
		var disks []contracts.DiskInfo
		for _, path := range d.disks {
			disks = append(disks, contracts.DiskInfo{Path: path, Format: "qcow2"})
		}
		out.VMs = append(out.VMs, contracts.VMInfo{ID: d.id, Name: d.id, HostID: d.host, PowerState: d.power,
			CPU: d.cpu, MemoryMiB: d.memMiB, ProviderRaw: raw, OwnerNamespace: d.ownerNS, OwnerName: d.ownerName, Disks: disks})
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
	if f.describeErr != nil {
		return contracts.DescribeResponse{}, f.describeErr
	}
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
	return clusteredAdoptionReconcilerWith(t, prov, nil, objs...)
}

// clusteredAdoptionReconcilerWith is clusteredAdoptionReconciler with a hook
// run right after each create (e.g. what the VirtualMachine controller does
// to a new VM).
func clusteredAdoptionReconcilerWith(t *testing.T, prov contracts.Provider,
	afterCreate func(ctx context.Context, cl client.WithWatch, obj client.Object), objs ...client.Object) *VMAdoptionReconciler {
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
				if err := cl.Create(ctx, obj, opts...); err != nil {
					return err
				}
				if afterCreate != nil {
					afterCreate(ctx, cl, obj)
				}
				return nil
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
// unstamped domain (owner transfer, nothing replaceable) before it is bound to
// its host, and records its size from provider truth; a domain stamped for a
// VirtualMachine that still exists is never taken, and one stamped only by a
// VirtualMachine that no longer exists (a previous incarnation) is skipped and
// reported, never adopted (ADR-0007 A6, until A6.4).
func TestClusteredAdoption_AdoptsByHostAndIDAndRecordsTheBinding(t *testing.T) {
	live := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "team-a", UID: "uid-live"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "other", Namespace: "team-a"}},
	}
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 2, memMiB: 2048, maxMemMiB: 8192, vcpusOnline: 4, power: "On"},
		{host: "host-b", id: "web", uuid: "uuid-b-web", cpu: 1, memMiB: 1024, maxMemMiB: 1024, vcpusOnline: 1, power: "Off"},
		{host: "host-a", id: "team-a.db", uuid: "uuid-a-db", owner: "uid-live", ownerNS: "team-a", ownerName: "db", cpu: 1, memMiB: 1024, power: "On"},
		{host: "host-b", id: "team-b.old", uuid: "uuid-b-old", owner: "uid-deleted", ownerNS: "team-b", ownerName: "old", cpu: 1, memMiB: 1024, power: "On"},
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
	// The binding also sets the restore marker to the VM's own UID (ADR-0007
	// A6.2, R1), so an adopted VM is never held as restored.
	assert.Equal(t, map[string]string{AdoptedHostAnnotation: "host-a", AdoptedIDAnnotation: "web",
		infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation: string(vmA.UID)}, vmA.Annotations)
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
	assert.Equal(t, string(vmB.UID), vmB.Annotations[infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation])
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
	assert.Empty(t, tb[0].ReplaceableOwnerUIDs, "adoption never replaces a stamp")
	assert.Equal(t, string(vmA.UID), prov.find("host-a", "web").owner)
	assert.Equal(t, string(vmB.UID), prov.find("host-b", "web").owner)

	// The domain a live VirtualMachine owns was neither transferred nor adopted.
	assert.Empty(t, prov.transfersTo("host-a", "team-a.db"))
	assert.Equal(t, "uid-live", prov.find("host-a", "team-a.db").owner)
	// The previous incarnation was neither transferred nor adopted, and is
	// named with the A6 hint.
	assert.Empty(t, prov.transfersTo("host-b", "team-b.old"))
	assert.Equal(t, "uid-deleted", prov.find("host-b", "team-b.old").owner)
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &vms, client.InNamespace(clusterNS)))
	assert.Len(t, vms.Items, 2)

	st := adoptionStatus(t, r)
	assert.EqualValues(t, 2, st.AdoptedVMs)
	assert.EqualValues(t, 0, st.FailedAdoptions)
	assert.Contains(t, st.Message, "1 not adopted")
	assert.Contains(t, st.Message, "team-b/old (team-b.old on host-b)")
	assert.Contains(t, st.Message, "A6 runbook")
	assert.NotContains(t, st.Message, "team-a/db", "a live VirtualMachine's domain is managed, not reported")

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
// names it and discovery is retried sooner. No new adoption starts on the
// reachable hosts either: a copy of a candidate on the unknown host (a stale
// definition sharing its disk) could not be ruled out.
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
	assert.Empty(t, prov.transfersTo("host-a", "web"), "no new adoption while a host is unknown")
	err := r.Get(context.Background(), types.NamespacedName{Namespace: clusterNS, Name: clusteredAdoptedVMName("host-a", "web")},
		&infravirtrigaudiov1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "nothing is created for a held candidate")

	st := adoptionStatus(t, r)
	assert.Contains(t, st.Message, "host-c")
	assert.Contains(t, st.Message, "unknown (not absent)")
	assert.Contains(t, st.Message, "1 not adopted while a host is unknown")
	assert.Contains(t, st.Message, "web on host-a")
	assert.EqualValues(t, 1, st.DiscoveredVMs)
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
	assert.NotContains(t, vm.Status.Provider, contracts.VMInfoOwnerUIDKey, "no owner stamp UID is copied into status")
	assert.Equal(t, "uuid-a-web", vm.Status.Provider[contracts.VMInfoUUIDKey])
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
		"class shared: bound":                                {&metav1.LabelSelector{}, true},
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
// the owner transfer for good (the domain was taken over in between: a
// Conflict), the VirtualMachine this discovery created is not bound and is
// removed again (no status.id, so no provider call), and the failure is
// counted. A retryable failure (the host unreachable) keeps it waiting for the
// next discovery, and a refusal never removes a VirtualMachine this discovery
// did not create.
func TestClusteredAdoption_FailedTransferDoesNotBind(t *testing.T) {
	domains := func() []*fakeDomain {
		return []*fakeDomain{{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 1, memMiB: 1024, power: "On"}}
	}
	name := clusteredAdoptedVMName("host-a", "web")

	t.Run("refused for good: the created VM is removed", func(t *testing.T) {
		prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: domains(),
			transferErr: contracts.NewConflictError("owned by another VirtualMachine", nil)}
		r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))
		res := reconcileAdoption(t, r)
		assert.Equal(t, clusteredAdoptionRetryInterval, res.RequeueAfter)
		err := r.Get(context.Background(), types.NamespacedName{Namespace: clusterNS, Name: name}, &infravirtrigaudiov1beta1.VirtualMachine{})
		assert.True(t, apierrors.IsNotFound(err), "the stranded VM is removed: %v", err)
		assert.EqualValues(t, 1, adoptionStatus(t, r).FailedAdoptions)
	})

	t.Run("retryable: the created VM keeps waiting, unbound", func(t *testing.T) {
		prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: domains(),
			transferErr: contracts.NewHostUnavailableError("host unreachable", nil)}
		r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))
		reconcileAdoption(t, r)
		vm := adoptedVMGet(t, r, name)
		assert.Empty(t, vm.Status.ID)
		assert.Nil(t, vm.Status.Placement)
		assert.EqualValues(t, 1, adoptionStatus(t, r).FailedAdoptions)
	})

	t.Run("refused for good, VM left by an earlier discovery: removed too", func(t *testing.T) {
		prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: domains(),
			transferErr: contracts.NewConflictError("owned by another VirtualMachine", nil)}
		r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"),
			awaitingAdoptedVM("host-a", "web", "uid-waiting"))
		reconcileAdoption(t, r)
		err := r.Get(context.Background(), types.NamespacedName{Namespace: clusterNS, Name: name}, &infravirtrigaudiov1beta1.VirtualMachine{})
		assert.True(t, apierrors.IsNotFound(err), "a stranded VM from an earlier discovery is removed: %v", err)
	})

	t.Run("refused for good, the VM cannot be removed: named in the message", func(t *testing.T) {
		prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: domains(),
			transferErr: contracts.NewConflictError("owned by another VirtualMachine", nil)}
		r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"),
			awaitingAdoptedVM("host-a", "web", "uid-waiting"))
		ww, ok := r.Client.(client.WithWatch)
		require.True(t, ok)
		r.Client = interceptor.NewClient(ww, interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return fmt.Errorf("delete refused by a webhook")
			},
		})
		reconcileAdoption(t, r)
		assert.Equal(t, types.UID("uid-waiting"), adoptedVMGet(t, r, name).UID)
		msg := adoptionStatus(t, r).Message
		assert.Contains(t, msg, "wait for a domain the provider refused and could not be removed")
		assert.Contains(t, msg, name)
	})
}

// TestClusteredAdoption_FailureAfterTheTransferKeepsTheVM: when the owner
// transfer succeeded but the Describe that follows fails (even with NotFound),
// the adopting VM is kept — the domain already carries its stamp — and the
// next discovery completes the binding from that stamp.
func TestClusteredAdoption_FailureAfterTheTransferKeepsTheVM(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps,
		describeErr: contracts.NewNotFoundError("not found", nil),
		domains: []*fakeDomain{
			{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 1, memMiB: 1024, power: "On"},
		}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))
	name := clusteredAdoptedVMName("host-a", "web")

	reconcileAdoption(t, r)
	vm := adoptedVMGet(t, r, name)
	assert.Empty(t, vm.Status.ID, "not bound yet")
	assert.Equal(t, string(vm.UID), prov.find("host-a", "web").owner, "the transfer happened")
	assert.EqualValues(t, 1, adoptionStatus(t, r).FailedAdoptions)

	prov.describeErr = nil
	r.setLastDiscovery(t, time.Now().Add(-time.Hour))
	reconcileAdoption(t, r)
	assert.Equal(t, "web", adoptedVMGet(t, r, name).Status.ID, "the binding is completed from the stamp")
}

// TestClusteredAdoption_UnreliableStampIsNotAdopted: a VM whose stamp state
// the provider reports (unreadable, or several owners) is skipped before
// anything is created for it.
func TestClusteredAdoption_UnreliableStampIsNotAdopted(t *testing.T) {
	prov := &unreliableStampAdopter{fakeClusteredAdopter: fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", power: "On"},
	}}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))

	reconcileAdoption(t, r)
	assert.Empty(t, prov.transfers)
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &vms))
	assert.Empty(t, vms.Items, "nothing is created for it")
	assert.Contains(t, adoptionStatus(t, r).Message, "owner stamp unreadable")
}

// unreliableStampAdopter lists every VM with an unreadable owner stamp.
type unreliableStampAdopter struct{ fakeClusteredAdopter }

func (f *unreliableStampAdopter) ListVMs(ctx context.Context) (contracts.VMList, error) {
	list, err := f.fakeClusteredAdopter.ListVMs(ctx)
	for i := range list.VMs {
		list.VMs[i].ProviderRaw[contracts.VMInfoOwnerStampStateKey] = contracts.OwnerStampUnreadable
	}
	return list, err
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
	}}, []infravirtrigaudiov1beta1.VirtualMachine{bound}, adoptionGuards{})

	require.Len(t, plan.adopt, 1)
	assert.Equal(t, "host-b", plan.adopt[0].HostID, "a name bound on host-a says nothing about host-b's domain")
	require.Len(t, plan.skipped, 2, "no host id or no UUID: never adopted")
	for _, sk := range plan.skipped {
		assert.Equal(t, skipNoIdentity, sk.reason)
	}
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

// TestClusteredAdoptionMessage_CapsTheNamedHosts: the status message names at
// most unreachableHostsListed hosts and counts the rest.
func TestClusteredAdoptionMessage_CapsTheNamedHosts(t *testing.T) {
	var hosts []string
	for i := 0; i < 13; i++ {
		hosts = append(hosts, fmt.Sprintf("host-%02d", i))
	}
	msg := clusteredAdoptionMessage(1, 0, 0, nil, hosts)
	assert.Contains(t, msg, "13 host(s) could not be listed")
	assert.Contains(t, msg, "host-09")
	assert.NotContains(t, msg, "host-10")
	assert.Contains(t, msg, "and 3 more")
	assert.Equal(t, "Successfully adopted 2 VMs", clusteredAdoptionMessage(2, 0, 0, nil, nil))
}

// TestClusteredAdoption_PreviousIncarnationsAreNeverAdopted: a domain stamped
// only by VirtualMachines that no longer exist — orphaned with
// orphan-on-delete, or the old domain of a VirtualMachine restored with a new
// UID — is skipped and named with the A6 hint, even when the VirtualMachine it
// names by namespace and name exists again under a new UID. Nothing is created
// or transferred.
func TestClusteredAdoption_PreviousIncarnationsAreNeverAdopted(t *testing.T) {
	restored := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a", UID: "uid-restored"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c", Namespace: clusterNS}},
	}
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "team-a.web", uuid: "uuid-a-web", owner: "uid-before-restore", ownerNS: "team-a", ownerName: "web", power: "On"},
		{host: "host-a", id: "legacy.db", uuid: "uuid-a-db", owner: "uid-orphaned", power: "Off"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"), restored)

	reconcileAdoption(t, r)
	assert.Empty(t, prov.transfers, "no stamp is ever replaced by adoption")
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &vms, client.InNamespace(clusterNS)))
	assert.Empty(t, vms.Items, "nothing is adopted")
	st := adoptionStatus(t, r)
	assert.EqualValues(t, 0, st.AdoptedVMs)
	assert.EqualValues(t, 0, st.FailedAdoptions, "a skip is not a failure")
	assert.Contains(t, st.Message, "2 not adopted")
	assert.Contains(t, st.Message, "team-a/web (team-a.web on host-a)")
	assert.Contains(t, st.Message, "legacy.db on host-a", "an incarnation whose stamp names no namespace/name is named by its domain")
	assert.Contains(t, st.Message, "re-attach it per the ADR-0007 A6 runbook, or remove it")
}

// TestClusteredAdoption_CrossHostDuplicatesAreNeverAdopted: a brownfield
// domain defined on two hosts — the same UUID, or a disk a VM on another host
// uses (common on a shared NFS pool) — is skipped and reported on every host,
// so deleting a stale copy can never remove the running VM's disk. A
// duplicate of a managed domain is skipped too; the managed one is untouched.
func TestClusteredAdoption_CrossHostDuplicatesAreNeverAdopted(t *testing.T) {
	bound := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: clusterNS, UID: "uid-db"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c"}},
		Status: infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "db",
			Placement: &infravirtrigaudiov1beta1.PlacementStatus{Host: "host-a"}},
	}
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		// The same UUID on two hosts.
		{host: "host-a", id: "web", uuid: "uuid-web", power: "On", disks: []string{"/pool/web-a.qcow2"}},
		{host: "host-b", id: "web", uuid: "UUID-WEB", power: "Off", disks: []string{"/pool/web-b.qcow2"}},
		// Different UUIDs, one shared disk (a stale definition on another host).
		{host: "host-a", id: "app", uuid: "uuid-app-a", power: "On", disks: []string{"/pool/app.qcow2"}},
		{host: "host-b", id: "app-old", uuid: "uuid-app-b", power: "Off", disks: []string{"/pool/app.qcow2"}},
		// A stale copy of a managed domain.
		{host: "host-a", id: "db", uuid: "uuid-db", owner: "uid-db", power: "On", disks: []string{"/pool/db.qcow2"}},
		{host: "host-b", id: "db-copy", uuid: "uuid-db-copy", power: "Off", disks: []string{"/pool/db.qcow2"}},
		// Unique: adopted. The same disk twice on ONE host is not a cross-host duplicate.
		{host: "host-b", id: "solo", uuid: "uuid-solo", power: "On", disks: []string{"/pool/solo.qcow2", "/pool/solo.qcow2"}},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(),
		clusterHost("host-a", "prov-c"), clusterHost("host-b", "prov-c"), bound)

	reconcileAdoption(t, r)
	require.Len(t, prov.transfers, 1, "only the unique domain is adopted")
	assert.Equal(t, "solo", prov.transfers[0].VM.ID)
	st := adoptionStatus(t, r)
	assert.EqualValues(t, 1, st.AdoptedVMs)
	assert.Contains(t, st.Message, "5 not adopted")
	for _, name := range []string{"web on host-a", "web on host-b", "app on host-a", "app-old on host-b", "db-copy on host-b"} {
		assert.Contains(t, st.Message, name)
	}
	assert.Contains(t, st.Message, "defined on more than one host")
	assert.Equal(t, "db", adoptedVMGet(t, r, "db").Status.ID, "the managed VM is untouched")
}

// vmControllerTouch is what the VirtualMachine controller does to a new VM
// within milliseconds: it adds its finalizer (a new resourceVersion), so the
// adoption's copy of the object is stale by the time it writes the binding.
func vmControllerTouch(ctx context.Context, cl client.WithWatch, obj client.Object) {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok {
		return
	}
	latest := &infravirtrigaudiov1beta1.VirtualMachine{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(vm), latest); err != nil {
		return
	}
	latest.Finalizers = append(latest.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
	_ = cl.Update(ctx, latest)
}

// TestClusteredAdoption_BindingSurvivesAConcurrentWriter: the VirtualMachine
// controller bumps the new VM's resourceVersion between its create and the
// binding write; the write re-reads the VM and binds it anyway.
func TestClusteredAdoption_BindingSurvivesAConcurrentWriter(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 1, memMiB: 1024, power: "On"},
	}}
	r := clusteredAdoptionReconcilerWith(t, prov, vmControllerTouch, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))

	reconcileAdoption(t, r)
	vm := adoptedVMGet(t, r, clusteredAdoptedVMName("host-a", "web"))
	assert.Equal(t, "web", vm.Status.ID, "bound despite the concurrent write")
	assert.Equal(t, "host-a", boundHost(vm))
	assert.Contains(t, vm.Finalizers, infravirtrigaudiov1beta1.VirtualMachineFinalizer, "the concurrent writer's change is kept")
	st := adoptionStatus(t, r)
	assert.EqualValues(t, 1, st.AdoptedVMs)
	assert.EqualValues(t, 0, st.FailedAdoptions)
}

// TestClusteredAdoption_BindingDeferredWhenTheVMIsGoingAway: a VM deleted
// between its create and the binding write is not bound; the adoption is
// deferred (not a failure) and retried sooner.
func TestClusteredAdoption_BindingDeferredWhenTheVMIsGoingAway(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", cpu: 1, memMiB: 1024, power: "On"},
	}}
	deleteRightAway := func(ctx context.Context, cl client.WithWatch, obj client.Object) {
		vmControllerTouch(ctx, cl, obj) // a finalizer keeps it, being deleted
		latest := &infravirtrigaudiov1beta1.VirtualMachine{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), latest); err == nil {
			_ = cl.Delete(ctx, latest)
		}
	}
	r := clusteredAdoptionReconcilerWith(t, prov, deleteRightAway, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))

	res := reconcileAdoption(t, r)
	assert.Equal(t, clusteredAdoptionRetryInterval, res.RequeueAfter)
	vm := adoptedVMGet(t, r, clusteredAdoptedVMName("host-a", "web"))
	assert.Empty(t, vm.Status.ID, "a VM being deleted is never bound")
	st := adoptionStatus(t, r)
	assert.EqualValues(t, 0, st.AdoptedVMs)
	assert.EqualValues(t, 0, st.FailedAdoptions, "deferred, not failed")
	assert.Contains(t, st.Message, "1 deferred to the next discovery")
}

// TestConfirmOwnersGone_UsesTheUncachedReader: a replaceable owner the cache
// no longer shows but the API server does (a lagging cache) refuses the
// transfer; one gone from the API server, or re-created under a new UID,
// allows it; no uncached reader or an owner without namespace/name fails
// closed. (Adoption names no replaceable owner until A6.4; this is the check
// A6.4's re-attach relies on.)
func TestConfirmOwnersGone_UsesTheUncachedReader(t *testing.T) {
	s := coverageTestScheme(t)
	live := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "team-a", UID: "uid-live"}}
	recreated := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "team-a", UID: "uid-new"}}
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(live, recreated).Build()
	cache := fake.NewClientBuilder().WithScheme(s).Build() // lags: shows none of them
	r := &VMAdoptionReconciler{Client: cache, Scheme: s, APIReader: api}
	ctx := context.Background()

	require.NoError(t, r.confirmOwnersGone(ctx, nil))
	require.NoError(t, r.confirmOwnersGone(ctx, []contracts.ObjectIdentity{{UID: "uid-gone", Namespace: "team-a", Name: "gone"}}))
	require.NoError(t, r.confirmOwnersGone(ctx, []contracts.ObjectIdentity{{UID: "uid-old", Namespace: "team-a", Name: "db"}}),
		"the name exists again under another UID: the stamped owner is gone")
	err := r.confirmOwnersGone(ctx, []contracts.ObjectIdentity{{UID: "uid-live", Namespace: "team-a", Name: "web"}})
	require.ErrorIs(t, err, errOwnerStillExists, "the uncached read finds it although the cache does not")
	err = r.confirmOwnersGone(ctx, []contracts.ObjectIdentity{{UID: "uid-x"}})
	require.ErrorIs(t, err, errOwnerStillExists, "no namespace/name: fail closed")
	r.APIReader = nil
	err = r.confirmOwnersGone(ctx, []contracts.ObjectIdentity{{UID: "uid-gone", Namespace: "team-a", Name: "gone"}})
	require.ErrorIs(t, err, errOwnerStillExists, "no uncached reader: fail closed")
}

// TestClusteredAdoption_SharedHostEndpointIsNotAdoptedFrom: a Host whose
// endpoint (ignoring the SSH user) another Host object names — here one of
// another Provider in another namespace — is not adopted from.
func TestClusteredAdoption_SharedHostEndpointIsNotAdoptedFrom(t *testing.T) {
	other := clusterHost("elsewhere", "prov-other")
	other.Namespace = "other-ns"
	other.Spec.Endpoint = "qemu+ssh://root@HOST-A/system"
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "web", uuid: "uuid-a-web", power: "On"},
		{host: "host-b", id: "web", uuid: "uuid-b-web", power: "On"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(),
		clusterHost("host-a", "prov-c"), clusterHost("host-b", "prov-c"), other)

	reconcileAdoption(t, r)
	require.Len(t, prov.transfers, 1)
	assert.Equal(t, "host-b", prov.transfers[0].VM.HostID)
	assert.Contains(t, adoptionStatus(t, r).Message, "one clustered Provider per host endpoint")
	assert.Equal(t, endpointKey("qemu+ssh://virt@host-a/system"), endpointKey("qemu+ssh://root@HOST-A/system"))
	assert.NotEqual(t, endpointKey("qemu+ssh://virt@host-a/system"), endpointKey("qemu+ssh://virt@host-a:2222/system"))
}

// TestEndpointKey_DefaultPortNormalized (slice 4 review nit): an endpoint
// without a port names the scheme's default, so "h1" and "h1:22" are the same
// hypervisor; another port, scheme or host stays different.
func TestEndpointKey_DefaultPortNormalized(t *testing.T) {
	same := [][2]string{
		{"qemu+ssh://virt@h1/system", "qemu+ssh://root@h1:22/system"},
		{"QEMU+SSH://h1/system", "qemu+ssh://H1:22/session"},
		{"ssh://h1", "ssh://h1:22"},
		{"qemu+tcp://h1/system", "qemu+tcp://h1:16509/system"},
		{"qemu+tls://h1/system", "qemu+tls://h1:16514/system"},
		{"qemu+ssh://[fd00::1]/system", "qemu+ssh://[fd00::1]:22/system"},
	}
	for _, p := range same {
		assert.Equal(t, endpointKey(p[0]), endpointKey(p[1]), "%s vs %s", p[0], p[1])
	}
	different := [][2]string{
		{"qemu+ssh://h1/system", "qemu+ssh://h1:2222/system"},
		{"qemu+ssh://h1/system", "qemu+tcp://h1/system"},
		{"qemu+ssh://h1/system", "qemu+ssh://h2/system"},
		{"grpc://h1:9443", "grpc://h1:9444"},
	}
	for _, p := range different {
		assert.NotEqual(t, endpointKey(p[0]), endpointKey(p[1]), "%s vs %s", p[0], p[1])
	}
	assert.Equal(t, "grpc://h1:", endpointKey("grpc://h1"), "a scheme without a known default keeps an empty port")
	assert.Equal(t, "not a url", endpointKey(" not a url "), "an endpoint that does not parse is compared as written")
}

// TestClusteredAdoption_DomainManagedThroughASingleHostProviderIsSkipped: an
// unstamped domain whose name is the status.id of a VM bound through a
// single-host Provider (the same hypervisor fronted by both kinds) is not
// adopted; a clustered Provider's own ids do not count.
func TestClusteredAdoption_DomainManagedThroughASingleHostProviderIsSkipped(t *testing.T) {
	single := readyProvider(clusterNS, "prov-single")
	onSingle := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: clusterNS, UID: "uid-legacy"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-single"}},
		Status:     infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "legacy"},
	}
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
		{host: "host-a", id: "legacy", uuid: "uuid-a-legacy", power: "On"},
		{host: "host-a", id: "fresh", uuid: "uuid-a-fresh", power: "On"},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), single, clusterHost("host-a", "prov-c"), onSingle)

	reconcileAdoption(t, r)
	require.Len(t, prov.transfers, 1)
	assert.Equal(t, "fresh", prov.transfers[0].VM.ID)
	assert.Contains(t, adoptionStatus(t, r).Message, "managed through a single-host Provider")
}

// TestAmbiguousVMID: names virsh reads as a domain id or UUID are skipped.
func TestAmbiguousVMID(t *testing.T) {
	for id, want := range map[string]bool{
		"7": true, "-0": true, "+12": true, "0042": true,
		"1b4e28ba-2fa1-11d2-883f-0016d3cca427": true, "1B4E28BA2FA111D2883F0016D3CCA427": true,
		"web": false, "team-a.web": false, "7web": false, "": false, "deadbeef": false,
	} {
		assert.Equal(t, want, ambiguousVMID(id), id)
	}
}

// TestClusteredAdoption_HeldWhileAHostIsUnknownButBindingsComplete: while a
// host is unknown, a binding whose owner transfer already happened (the domain
// carries the waiting VM's stamp) is still completed; only new adoptions wait.
// This is the scenario that motivates the hold: a stale copy on the reachable
// host whose running twin (sharing its NFS disk) is on the unknown host.
func TestClusteredAdoption_HeldWhileAHostIsUnknownButBindingsComplete(t *testing.T) {
	waiting := awaitingAdoptedVM("host-a", "db", "uid-waiting")
	prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, unreachable: []string{"host-b"}, domains: []*fakeDomain{
		{host: "host-a", id: "db", uuid: "uuid-a-db", owner: "uid-waiting", cpu: 1, memMiB: 1024, power: "On"},
		// The stale copy: it looks unique because its twin is on host-b.
		{host: "host-a", id: "app-old", uuid: "uuid-a-app", power: "Off", disks: []string{"/nfs/app.qcow2"}},
		{host: "host-b", id: "app", uuid: "uuid-b-app", power: "On", disks: []string{"/nfs/app.qcow2"}},
	}}
	r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(),
		clusterHost("host-a", "prov-c"), clusterHost("host-b", "prov-c"), waiting)

	reconcileAdoption(t, r)
	assert.Equal(t, "db", adoptedVMGet(t, r, waiting.Name).Status.ID, "the pending binding is completed")
	assert.Empty(t, prov.transfersTo("host-a", "app-old"), "the stale copy is not adopted")
	assert.Contains(t, adoptionStatus(t, r).Message, "app-old on host-a")
}

// TestClusteredAdoption_SharedEndpointGuardFailsClosed: a Host whose endpoint
// a single-host Provider names is not adopted from; and when the Hosts cannot
// be listed, nothing is adopted in that pass.
func TestClusteredAdoption_SharedEndpointGuardFailsClosed(t *testing.T) {
	t.Run("a single-host Provider names the endpoint", func(t *testing.T) {
		single := readyProvider(clusterNS, "prov-single")
		single.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
		single.Spec.Endpoint = "qemu+ssh://root@host-a/system"
		prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
			{host: "host-a", id: "web", uuid: "uuid-a-web", power: "On"},
			{host: "host-b", id: "web", uuid: "uuid-b-web", power: "On"},
		}}
		r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), single,
			clusterHost("host-a", "prov-c"), clusterHost("host-b", "prov-c"))
		reconcileAdoption(t, r)
		require.Len(t, prov.transfers, 1)
		assert.Equal(t, "host-b", prov.transfers[0].VM.HostID)
	})

	t.Run("the Hosts cannot be listed: nothing is adopted", func(t *testing.T) {
		prov := &fakeClusteredAdopter{caps: routedAdoptionCaps, domains: []*fakeDomain{
			{host: "host-a", id: "web", uuid: "uuid-a-web", power: "On"},
		}}
		r := clusteredAdoptionReconciler(t, prov, clusteredAdoptionProvider(), clusterHost("host-a", "prov-c"))
		ww, ok := r.Client.(client.WithWatch)
		require.True(t, ok)
		r.Client = interceptor.NewClient(ww, interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isHosts := list.(*infravirtrigaudiov1beta1.HostList); isHosts {
					return fmt.Errorf("hosts unavailable")
				}
				return cl.List(ctx, list, opts...)
			},
		})
		res := reconcileAdoption(t, r)
		assert.Equal(t, clusteredAdoptionRetryInterval, res.RequeueAfter)
		assert.Empty(t, prov.transfers)
		var vms infravirtrigaudiov1beta1.VirtualMachineList
		require.NoError(t, r.List(context.Background(), &vms))
		assert.Empty(t, vms.Items)
		assert.Contains(t, adoptionStatus(t, r).Message, "list Hosts")
	})
}
