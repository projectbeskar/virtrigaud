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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin the manager side of ADR-0007 A6.2: R1 (the restore marker
// infra.virtrigaud.io/placement-uid and its hold) and R4 (the pre-schedule
// uniqueness check over the owner-filtered ListVMs), plus the marker writes
// of the create, clone and adoption paths.

const markerKey = infrav1beta1.VirtualMachinePlacementUIDAnnotation

// r4Provider is a clustered provider that answers the pre-schedule check:
// GetCapabilities (caps / capsErr) and ListVMsForOwner (list / listErr), and
// records every filtered listing it is asked for.
type r4Provider struct {
	routingProvider

	caps    contracts.Capabilities
	capsErr error
	list    contracts.VMList
	listErr error

	mu        sync.Mutex
	capsCalls int
	filters   []contracts.OwnerFilter
}

func (p *r4Provider) GetCapabilities(context.Context) (contracts.Capabilities, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.capsCalls++
	return p.caps, p.capsErr
}

func (p *r4Provider) ListVMsForOwner(_ context.Context, f contracts.OwnerFilter) (contracts.VMList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.filters = append(p.filters, f)
	return p.list, p.listErr
}

func (p *r4Provider) listCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.filters)
}

// newR4Provider answers the check with an applied filter and list's VMs.
func newR4Provider(list contracts.VMList) *r4Provider {
	list.OwnerFilterApplied = true
	return &r4Provider{
		caps: contracts.Capabilities{SupportsClustering: true, SupportsListOwnerFilter: true},
		list: list,
	}
}

// stampedInfo is a VMInfo stamped with ns/name/uid on host.
func stampedInfo(host, id, ns, name, uid string) contracts.VMInfo {
	return contracts.VMInfo{ID: id, Name: id, HostID: host, OwnerNamespace: ns, OwnerName: name,
		ProviderRaw: map[string]string{contracts.VMInfoOwnerUIDKey: uid}}
}

// reconcileClustered runs one reconcileVM of the VM name, as a reconcile after
// the finalizer is in place would.
func reconcileClustered(t *testing.T, r *VirtualMachineReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.reconcileVM(context.Background(), getVM(t, r, name))
	require.NoError(t, err)
	return res
}

// writeLog records, in order, the marker patches and pendingHost status
// writes a fake client sees for VirtualMachines.
type writeLog struct {
	mu      sync.Mutex
	entries []string
}

func (w *writeLog) add(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, s)
}

func (w *writeLog) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.entries...)
}

// clusteredFixtureLogged is clusteredFixture on a fake client that logs the
// marker patches ("marker=<value>") and the status writes that record a
// pendingHost ("pendingHost=<host>"); failPatch, when set, fails every
// VirtualMachine patch with it.
func clusteredFixtureLogged(t *testing.T, prov contracts.Provider, log *writeLog, failPatch error,
	vm *infrav1beta1.VirtualMachine, extra ...client.Object) *VirtualMachineReconciler {
	t.Helper()
	providerCR := withRuntime(clusteredProviderCR("prov-cluster", clusteredNS))
	pool := hostPoolCR("pool-a", clusteredNS, providerCR.Name)
	host := readyHost("host-alpha", clusteredNS, pool.Name, providerCR.Name)
	objs := append([]client.Object{vm, providerCR, pool, host, smallVMClass(clusteredNS), minimalVMImage(clusteredNS)}, extra...)
	s := coverageTestScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithIndex(&infrav1beta1.VirtualMachine{}, placementProviderIndex, placementProviderIndexValue).
		WithStatusSubresource(&infrav1beta1.VirtualMachine{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if v, ok := obj.(*infrav1beta1.VirtualMachine); ok {
					if failPatch != nil {
						return failPatch
					}
					log.add("marker=" + v.Annotations[markerKey])
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if v, ok := obj.(*infrav1beta1.VirtualMachine); ok && v.Status.Placement != nil && v.Status.Placement.PendingHost != "" {
					log.add("pendingHost=" + v.Status.Placement.PendingHost)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	return &VirtualMachineReconciler{Client: fc, Scheme: s, RemoteResolver: &stubResolver{provider: prov}}
}

// ─── R1: the marker is written before the first pendingHost write ─────────────

// TestR1_Create_MarkerWrittenBeforeTheFirstPendingHost: a clustered VM's
// restore marker is set to its own UID before its pendingHost is recorded,
// and both are stored when the Create is sent.
func TestR1_Create_MarkerWrittenBeforeTheFirstPendingHost(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	prov := newR4Provider(contracts.VMList{})
	log := &writeLog{}
	r := clusteredFixtureLogged(t, prov, log, nil, vm)
	var atCreate *infrav1beta1.VirtualMachine
	prov.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		atCreate = getVM(t, r, "web")
		return contracts.CreateResponse{ID: req.Name}, nil
	}

	reconcileClustered(t, r, "web")

	require.Equal(t, []string{"marker=uid-web", "pendingHost=host-alpha"}, log.all(),
		"the marker is written first, then the pending host")
	require.NotNil(t, atCreate, "the Create was sent")
	assert.Equal(t, "uid-web", atCreate.Annotations[markerKey])
	assert.Equal(t, "host-alpha", atCreate.Status.Placement.PendingHost)
	bound := getVM(t, r, "web")
	assert.Equal(t, "web", bound.Status.ID)
	assert.Equal(t, "uid-web", bound.Annotations[markerKey])
	assert.Equal(t, 1, prov.listCalls(), "R4 ran once, before the first placement")
}

// TestR1_Create_NoPendingHostWithoutTheMarker: a marker write that fails —
// a lost resourceVersion race, or anything else — records no pendingHost and
// sends no Create.
func TestR1_Create_NoPendingHostWithoutTheMarker(t *testing.T) {
	for name, failure := range map[string]error{
		"conflict": apierrors.NewConflict(schema.GroupResource{Resource: "virtualmachines"}, "web", errors.New("changed")),
		"other":    errors.New("etcd unavailable"),
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-web"
			prov := newR4Provider(contracts.VMList{})
			log := &writeLog{}
			r := clusteredFixtureLogged(t, prov, log, failure, vm)

			_, err := r.reconcileVM(context.Background(), getVM(t, r, "web"))
			if name == "other" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Empty(t, log.all(), "no pendingHost is recorded without the marker")
			assert.Empty(t, prov.createReqs)
			assert.Empty(t, pendingHost(getVM(t, r, "web")))
		})
	}
}

// TestR1_CloneTarget_MarkerWrittenBeforeItsPendingHost: a clustered clone's
// target VM carries a marker naming its own UID when the Clone is sent — it
// was written before the target's pendingHost — and never the source's.
func TestR1_CloneTarget_MarkerWrittenBeforeItsPendingHost(t *testing.T) {
	src := boundSource()
	src.Annotations = map[string]string{markerKey: string(src.UID)}
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	r, clone := clusteredCloneFixture(t, src, cp)
	var atClone *infrav1beta1.VirtualMachine
	cp.onClone = func() { atClone = getTarget(t, r) }

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	require.NotNil(t, atClone)
	assert.Equal(t, string(atClone.UID), atClone.Annotations[markerKey], "the target's marker names its own UID")
	assert.NotEqual(t, string(src.UID), atClone.Annotations[markerKey], "never the source's")
	assert.Equal(t, "host-alpha", atClone.Status.Placement.PendingHost)
}

// TestR1_CloneTarget_NoPendingHostWithoutTheMarker: when the target's marker
// cannot be written, its pendingHost is not recorded and no Clone is sent.
func TestR1_CloneTarget_NoPendingHostWithoutTheMarker(t *testing.T) {
	cp := &clonerProvider{cloneResp: contracts.CloneResponse{TargetVmID: "default.clone-c-target"}}
	r, clone := clusteredCloneFixture(t, boundSource(), cp)
	base, ok := r.Client.(client.WithWatch)
	require.True(t, ok, "the fixture's client is a fake client.WithWatch")
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*infrav1beta1.VirtualMachine); ok {
				return apierrors.NewConflict(schema.GroupResource{Resource: "virtualmachines"}, obj.GetName(), errors.New("changed"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	reconcileTwice(t, r, client.ObjectKeyFromObject(clone))

	assert.Zero(t, cp.cloneCnt, "no clone without the target's marker")
	target := getTarget(t, r)
	assert.Empty(t, pendingHost(target), "no pending host without the marker")
	assert.Empty(t, target.Annotations[markerKey])
}

// TestR1_ReservedMarkerIsNeverInherited: a VMClone or VMMigration target is
// built from user annotations with the reserved domain dropped, so a marker
// in a clone's spec.target.annotations never reaches the target.
func TestR1_ReservedMarkerIsNeverInherited(t *testing.T) {
	assert.True(t, isReservedAnnotation(markerKey))
	got := userTargetAnnotations(map[string]string{markerKey: "uid-src", "team": "a"})
	assert.Equal(t, map[string]string{"team": "a"}, got)
}

// ─── R1: the hold ─────────────────────────────────────────────────────────────

// TestR1_ForeignMarkerHoldsAndNeverSchedules: a restored VM — its marker
// names another UID, it has no status — is held RestorePending on every
// reconcile: no R4 query, no image prepare, no scheduling, no pendingHost,
// no Create, one Warning event, a message naming no host and no UID, and the
// marker is NOT rewritten (it only ever holds).
func TestR1_ForeignMarkerHoldsAndNeverSchedules(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-restored"
	vm.Annotations = map[string]string{markerKey: "uid-original"}
	prov := newR4Provider(contracts.VMList{UnreachableHostIDs: []string{"host-alpha"}})
	r := clusteredFixture(t, prov, vm)
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	for i := 0; i < 3; i++ {
		res := reconcileClustered(t, r, "web")
		assert.Equal(t, blockedRetryMin, res.RequeueAfter, "attempt %d", i)
	}

	held := getVM(t, r, "web")
	assert.Empty(t, prov.createReqs, "no Create")
	assert.Zero(t, prov.listCalls(), "R1 holds before R4 runs")
	assert.Zero(t, prov.capsCalls)
	assert.Empty(t, pendingHost(held))
	assert.Empty(t, held.Status.ID)
	assert.Equal(t, "uid-original", held.Annotations[markerKey], "a hold never rewrites the marker")
	placed := placedCondition(held)
	require.NotNil(t, placed)
	assert.Equal(t, metav1.ConditionFalse, placed.Status)
	assert.Equal(t, k8s.ReasonRestorePending, placed.Reason)
	assert.Equal(t, k8s.ReasonRestorePending, provisioningReason(held))
	assert.Contains(t, placed.Message, "docs/clustered-restore.md", "the message points at the runbook")
	for _, leak := range []string{"host-alpha", "uid-original", "uid-restored", "default.web"} {
		assert.NotContains(t, placed.Message, leak)
	}
	events := drainEvents(rec)
	require.Len(t, events, 1, "one Warning event for the hold: %v", events)
	assert.True(t, strings.HasPrefix(events[0], "Warning "+k8s.ReasonRestorePending), events[0])
}

// TestR1_DeletingAHeldVMTouchesNothing: a VM held by R1 or R4 has no
// status.id and no pendingHost, so deleting it sends no provider call — the
// previous incarnation is never touched — and releases the finalizer.
func TestR1_DeletingAHeldVMTouchesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		marker string
		list   contracts.VMList
	}{
		"held by R1": {marker: "uid-original"},
		"held by R4": {list: contracts.VMList{VMs: []contracts.VMInfo{
			stampedInfo("host-alpha", "default.web", clusteredNS, "web", "uid-original")}}},
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-restored"
			vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
			if tc.marker != "" {
				vm.Annotations = map[string]string{markerKey: tc.marker}
			}
			prov := newR4Provider(tc.list)
			r := clusteredFixture(t, prov, vm)
			reconcileClustered(t, r, "web")
			require.Equal(t, k8s.ReasonRestorePending, placedCondition(getVM(t, r, "web")).Reason)

			_, err := r.handleDeletion(context.Background(), deletingClusterVM(t, r, "web"))
			require.NoError(t, err)
			assert.Empty(t, prov.deleteRefs, "no provider call for a VM that was never placed")
			assert.Empty(t, prov.createReqs)
			err = r.Get(context.Background(), client.ObjectKey{Namespace: clusteredNS, Name: "web"}, &infrav1beta1.VirtualMachine{})
			assert.True(t, apierrors.IsNotFound(err), "finalizer released")
		})
	}
}

// TestR1_ReleasedMarkerLetsR4Decide: removing the marker, or setting it to
// the VM's own UID, releases the hold; R4 then runs, and the scheduler — not
// the marker — places the VM.
func TestR1_ReleasedMarkerLetsR4Decide(t *testing.T) {
	for name, release := range map[string]func(vm *infrav1beta1.VirtualMachine){
		"removed": func(vm *infrav1beta1.VirtualMachine) { delete(vm.Annotations, markerKey) },
		"own uid": func(vm *infrav1beta1.VirtualMachine) { vm.Annotations[markerKey] = string(vm.UID) },
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-restored"
			vm.Annotations = map[string]string{markerKey: "uid-original"}
			prov := newR4Provider(contracts.VMList{})
			r := clusteredFixture(t, prov, vm)
			reconcileClustered(t, r, "web")
			require.Empty(t, prov.createReqs)

			got := getVM(t, r, "web")
			release(got)
			require.NoError(t, r.Update(context.Background(), got))
			reconcileClustered(t, r, "web")

			assert.Equal(t, 1, prov.listCalls(), "R4 ran")
			require.Len(t, prov.createReqs, 1)
			assert.Equal(t, "host-alpha", prov.createReqs[0].TargetHostID, "scheduled onto the pool's host")
			assert.Equal(t, "uid-restored", getVM(t, r, "web").Annotations[markerKey])
		})
	}
}

// TestR1_ForgedMarkerHoldsOnlyTheForgersVM: a tenant who writes a marker
// naming another VM's UID — or a host's name — only holds its own VM.
// The other VM, whose UID the forged marker names, is scheduled and created
// as usual, and the forged value never reaches the scheduler or the provider.
func TestR1_ForgedMarkerHoldsOnlyTheForgersVM(t *testing.T) {
	victim := clusterVM("db", clusteredNS, "prov-cluster")
	victim.UID = "uid-db"
	forger := clusterVM("web", clusteredNS, "prov-cluster")
	forger.UID = "uid-web"
	forger.Annotations = map[string]string{markerKey: "uid-db"}
	hostForger := clusterVM("api", clusteredNS, "prov-cluster")
	hostForger.UID = "uid-api"
	hostForger.Annotations = map[string]string{markerKey: "host-alpha"}
	prov := newR4Provider(contracts.VMList{})
	r := clusteredFixture(t, prov, victim, forger, hostForger)

	for _, name := range []string{"web", "api", "db"} {
		reconcileClustered(t, r, name)
	}

	require.Len(t, prov.createReqs, 1, "only the victim is created")
	assert.Equal(t, "db", prov.createReqs[0].Name)
	assert.Equal(t, "uid-db", prov.createReqs[0].Owner.UID)
	assert.Equal(t, "db", getVM(t, r, "db").Status.ID)
	assert.Equal(t, "uid-db", getVM(t, r, "db").Annotations[markerKey])
	for _, name := range []string{"web", "api"} {
		held := getVM(t, r, name)
		assert.Equal(t, k8s.ReasonRestorePending, placedCondition(held).Reason, name)
		assert.Empty(t, pendingHost(held), name)
	}
	for _, f := range prov.filters {
		assert.Equal(t, "db", f.Name, "R4 ran only for the unmarked VM")
	}
}

// TestR1_SingleHostIgnoresTheMarker: on a single-host Provider the marker is
// neither read nor written: a VM whose marker names another UID is created as
// before, no R4 query is made, and no Placed condition is set.
func TestR1_SingleHostIgnoresTheMarker(t *testing.T) {
	const ns = "default"
	vm := &infrav1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns, UID: "uid-web",
			Annotations: map[string]string{markerKey: "uid-original"}},
		Spec: infrav1beta1.VirtualMachineSpec{
			ProviderRef: infrav1beta1.ObjectRef{Name: "prov-single"},
			ClassRef:    infrav1beta1.ObjectRef{Name: "test-class"},
			ImageRef:    &infrav1beta1.ObjectRef{Name: "test-image"},
		},
	}
	prov := newR4Provider(contracts.VMList{})
	prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov},
		vm, withRuntime(singleProviderCR("prov-single", ns)), smallVMClass(ns), minimalVMImage(ns))

	reconcileClustered(t, r, "web")
	require.Len(t, prov.createReqs, 1, "created as before")
	assert.Empty(t, prov.createReqs[0].TargetHostID)
	reconcileClustered(t, r, "web") // bound: Describe

	got := getVM(t, r, "web")
	assert.Equal(t, "web", got.Status.ID)
	assert.Equal(t, "uid-original", got.Annotations[markerKey], "never written on a single-host Provider")
	assert.Zero(t, prov.listCalls())
	assert.Zero(t, prov.capsCalls)
	assert.Nil(t, placedCondition(got))
	assert.Nil(t, got.Status.Placement)
}

// TestR1_RestoredBindingIsRestorePendingThenTheMarkerIsRewritten: a VM
// restored WITH its binding under a new UID (marker naming another UID) whose
// owner-checked Describe finds nothing is Ready=False/RestorePending — not
// VMMissingOnHost — and never re-created; once the domain is re-stamped
// (Describe succeeds) the marker is rewritten to the VM's own UID.
func TestR1_RestoredBindingIsRestorePendingThenTheMarkerIsRewritten(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-restored"
	vm.Annotations = map[string]string{markerKey: "uid-original"}
	vm.Status.ID = "default.web"
	vm.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha", Pool: "pool-a"}
	prov := newR4Provider(contracts.VMList{})
	prov.describeErr = contracts.NewNotFoundError("describe: libvirt domain \"default.web\" is not owned by this VirtualMachine", nil)
	r := clusteredFixture(t, prov, vm)
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	res := reconcileClustered(t, r, "web")
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	reconcileClustered(t, r, "web")

	held := getVM(t, r, "web")
	ready := meta.FindStatusCondition(held.Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonRestorePending, ready.Reason)
	assert.NotContains(t, ready.Message, "host-alpha")
	assert.NotContains(t, ready.Message, "uid-original")
	assert.Empty(t, prov.createReqs, "never re-created (A4)")
	assert.Zero(t, prov.listCalls(), "a bound VM never runs R4")
	assert.Equal(t, "uid-original", held.Annotations[markerKey])
	holds := 0
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, "Warning "+k8s.ReasonRestorePending) {
			holds++
		}
	}
	assert.Equal(t, 1, holds, "one Warning event for the hold, not one per retry")

	// The administrator re-stamped the domain: Describe now finds it.
	prov.describeErr = nil
	prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}
	reconcileClustered(t, r, "web")
	assert.Equal(t, "uid-restored", getVM(t, r, "web").Annotations[markerKey], "rewritten after an owner-checked call succeeded")
}

// TestR1_WithoutAMarkerAMissingDomainIsStillVMMissingOnHost: A4 unchanged for
// a VM whose marker is its own (or absent).
func TestR1_WithoutAMarkerAMissingDomainIsStillVMMissingOnHost(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	vm.Status.ID = "default.web"
	vm.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha"}
	prov := newR4Provider(contracts.VMList{})
	prov.describeErr = contracts.NewNotFoundError("gone", nil)
	r := clusteredFixture(t, prov, vm)
	reconcileClustered(t, r, "web")
	ready := meta.FindStatusCondition(getVM(t, r, "web").Status.Conditions, k8s.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, k8s.ReasonVMMissingOnHost, ready.Reason)
}

// TestR1_BoundVMGetsItsMarkerAfterDescribe: a bound clustered VM placed before
// the marker existed gets it — its own UID — after an owner-checked Describe
// succeeds.
func TestR1_BoundVMGetsItsMarkerAfterDescribe(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	vm.Status.ID = "default.web"
	vm.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha"}
	prov := newR4Provider(contracts.VMList{})
	prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}
	r := clusteredFixture(t, prov, vm)
	reconcileClustered(t, r, "web")
	assert.Equal(t, "uid-web", getVM(t, r, "web").Annotations[markerKey])
	assert.Zero(t, prov.listCalls(), "R4 never runs for a bound VM")
	assert.Zero(t, prov.capsCalls)
}

// ─── R4: the pre-schedule uniqueness check ────────────────────────────────────

// TestR4_PreviousIncarnationHoldsBeforeScheduling: a domain stamped with the
// VM's namespace and name under another UID — on a host-local pool, where
// the disk guard would not see it — holds the VM RestorePending: nothing is
// scheduled or created, no pendingHost is recorded, and the message names no
// host.
func TestR4_PreviousIncarnationHoldsBeforeScheduling(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	prov := newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
		stampedInfo("host-bravo", "default.web", clusteredNS, "web", "uid-previous"),
	}})
	r := clusteredFixture(t, prov, vm, readyHost("host-bravo", clusteredNS, "pool-a", "prov-cluster"))
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	res := reconcileClustered(t, r, "web")
	assert.Equal(t, blockedRetryMin, res.RequeueAfter)
	reconcileClustered(t, r, "web")

	held := getVM(t, r, "web")
	assert.Empty(t, prov.createReqs)
	assert.Empty(t, pendingHost(held))
	assert.Equal(t, 2, prov.listCalls(), "R4 runs again on each retry while held")
	assert.Equal(t, contracts.OwnerFilter{Namespace: clusteredNS, Name: "web"}, prov.filters[0])
	placed := placedCondition(held)
	assert.Equal(t, k8s.ReasonRestorePending, placed.Reason)
	assert.Equal(t, k8s.ReasonRestorePending, provisioningReason(held))
	for _, leak := range []string{"host-bravo", "uid-previous", "default.web"} {
		assert.NotContains(t, placed.Message, leak)
	}
	assert.Len(t, drainEvents(rec), 1)
}

// TestR4_HoldBacksOff: a held VM is re-checked with the blocked-VM backoff,
// counted from when the hold began.
func TestR4_HoldBacksOff(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	prov := newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
		stampedInfo("host-alpha", "default.web", clusteredNS, "web", "uid-previous"),
	}})
	r := clusteredFixture(t, prov, vm)
	assert.Equal(t, blockedRetryMin, reconcileClustered(t, r, "web").RequeueAfter)

	got := getVM(t, r, "web")
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == k8s.ConditionPlaced {
			got.Status.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-3 * time.Minute))
		}
	}
	require.NoError(t, r.Status().Update(context.Background(), got))
	after := reconcileClustered(t, r, "web").RequeueAfter
	assert.GreaterOrEqual(t, after, 3*time.Minute)
	assert.LessOrEqual(t, after, blockedRetryMax)
}

// TestR4_OwnDomainRecordsItsHostAndTheCreateBindsIt: the VM's own domain —
// stamped with its UID, e.g. re-stamped by an administrator — on host-bravo
// is recorded as the pending host WITHOUT scheduling (host-bravo is cordoned,
// so the scheduler would never pick it), and the create retry there binds it.
func TestR4_OwnDomainRecordsItsHostAndTheCreateBindsIt(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	bravo := readyHost("host-bravo", clusteredNS, "pool-a", "prov-cluster")
	bravo.Spec.Schedulable = false
	prov := newR4Provider(contracts.VMList{
		VMs:                []contracts.VMInfo{stampedInfo("host-bravo", "default.web", clusteredNS, "web", "uid-web")},
		UnreachableHostIDs: []string{"host-charlie"},
	})
	r := clusteredFixture(t, prov, vm, bravo)
	var atCreate *infrav1beta1.VirtualMachine
	prov.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		atCreate = getVM(t, r, "web")
		return contracts.CreateResponse{ID: "default.web"}, nil // the idempotent bind
	}

	reconcileClustered(t, r, "web")

	require.Len(t, prov.createReqs, 1)
	assert.Equal(t, "host-bravo", prov.createReqs[0].TargetHostID, "the create goes where the domain is")
	require.NotNil(t, atCreate)
	pl := atCreate.Status.Placement
	assert.Equal(t, "host-bravo", pl.PendingHost)
	assert.Equal(t, "pool-a", pl.Pool)
	require.NotNil(t, pl.PendingResources, "it counts on its host at its pending size")
	assert.EqualValues(t, 2, pl.PendingResources.CPU)
	assert.Contains(t, pl.Reason, "R4")
	assert.Equal(t, "uid-web", atCreate.Annotations[markerKey], "the marker came first")

	bound := getVM(t, r, "web")
	assert.Equal(t, "default.web", bound.Status.ID)
	assert.Equal(t, "host-bravo", bound.Status.Placement.Host)
	assert.Equal(t, metav1.ConditionTrue, placedCondition(bound).Status)
}

// TestR4_UnreachableHostHoldsOnlyAForeignMarker: hosts that could not be
// checked do not hold a VM whose marker is its own or absent — it proceeds on
// the reachable hosts' evidence (decision 4) — while a VM whose marker names
// another UID is held whatever the hosts say (R1).
func TestR4_UnreachableHostHoldsOnlyAForeignMarker(t *testing.T) {
	unreachable := contracts.VMList{UnreachableHostIDs: []string{"host-bravo"}}
	for name, tc := range map[string]struct {
		marker string
		held   bool
	}{
		"no marker":      {marker: "", held: false},
		"own marker":     {marker: "uid-web", held: false},
		"foreign marker": {marker: "uid-original", held: true},
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-web"
			if tc.marker != "" {
				vm.Annotations = map[string]string{markerKey: tc.marker}
			}
			prov := newR4Provider(unreachable)
			r := clusteredFixture(t, prov, vm)
			reconcileClustered(t, r, "web")
			got := getVM(t, r, "web")
			if tc.held {
				assert.Empty(t, prov.createReqs)
				assert.Equal(t, k8s.ReasonRestorePending, placedCondition(got).Reason)
				return
			}
			require.Len(t, prov.createReqs, 1)
			assert.Equal(t, "host-alpha", prov.createReqs[0].TargetHostID)
			assert.Equal(t, "web", got.Status.ID)
		})
	}
}

// TestR4_WhatCountsAndWhatDoesNot: more than one own domain, own plus a
// previous incarnation, or a candidate whose stamp cannot be read holds the
// VM; an unstamped domain or one stamped for another VirtualMachine that
// merely has the name does not (decision 2: slice 2's exclusion handles it).
func TestR4_WhatCountsAndWhatDoesNot(t *testing.T) {
	unreadable := contracts.VMInfo{ID: "default.web", HostID: "host-alpha",
		ProviderRaw: map[string]string{contracts.VMInfoOwnerStampStateKey: contracts.OwnerStampUnreadable}}
	multiple := contracts.VMInfo{ID: "default.web", HostID: "host-alpha",
		ProviderRaw: map[string]string{contracts.VMInfoOwnerStampStateKey: contracts.OwnerStampMultiple,
			contracts.VMInfoOwnerUIDKey: "uid-web,uid-other"}}
	for name, tc := range map[string]struct {
		vms  []contracts.VMInfo
		held bool
	}{
		"two own domains": {vms: []contracts.VMInfo{
			stampedInfo("host-alpha", "default.web", clusteredNS, "web", "uid-web"),
			stampedInfo("host-bravo", "default.web", clusteredNS, "web", "uid-web")}, held: true},
		"own and a previous incarnation": {vms: []contracts.VMInfo{
			stampedInfo("host-alpha", "default.web", clusteredNS, "web", "uid-web"),
			stampedInfo("host-bravo", "web", clusteredNS, "web", "uid-previous")}, held: true},
		"unreadable stamp": {vms: []contracts.VMInfo{unreadable}, held: true},
		"two stamps":       {vms: []contracts.VMInfo{multiple}, held: true},
		"unstamped domain of the name": {vms: []contracts.VMInfo{
			{ID: "default.web", HostID: "host-alpha", ProviderRaw: map[string]string{}}}, held: false},
		"domain stamped for another VirtualMachine": {vms: []contracts.VMInfo{
			stampedInfo("host-alpha", "default.web", "other-ns", "web", "uid-x")}, held: false},
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-web"
			prov := newR4Provider(contracts.VMList{VMs: tc.vms})
			r := clusteredFixture(t, prov, vm, readyHost("host-bravo", clusteredNS, "pool-a", "prov-cluster"))
			reconcileClustered(t, r, "web")
			got := getVM(t, r, "web")
			if tc.held {
				assert.Empty(t, prov.createReqs)
				assert.Empty(t, pendingHost(got))
				assert.Equal(t, k8s.ReasonRestorePending, placedCondition(got).Reason)
				return
			}
			require.Len(t, prov.createReqs, 1, "scheduled as usual")
		})
	}
}

// TestR4_ProviderWithoutTheFilterHolds: a Provider that cannot answer the
// check — no owner-filtered ListVMs at all, a capability that says no, or an
// answer without the owner_filter_applied mark (version skew) — holds the VM
// with ProviderLacksListOwnerFilter; a failed capability query or listing
// holds it with UniquenessCheckFailed and names no endpoint. Nothing is
// scheduled or created.
func TestR4_ProviderWithoutTheFilterHolds(t *testing.T) {
	noCaps := newR4Provider(contracts.VMList{})
	noCaps.caps.SupportsListOwnerFilter = false
	unmarked := newR4Provider(contracts.VMList{})
	unmarked.list.OwnerFilterApplied = false
	capsFail := newR4Provider(contracts.VMList{})
	capsFail.capsErr = errors.New("dial tcp 10.0.0.5:9443: connection refused")
	listFail := newR4Provider(contracts.VMList{})
	listFail.listErr = contracts.NewUnavailableError("clustered libvirt provider registry not initialized", nil)

	for name, tc := range map[string]struct {
		prov   contracts.Provider
		reason string
		list   func() int
	}{
		"no owner-filtered ListVMs": {prov: &routingProvider{}, reason: k8s.ReasonProviderLacksListOwnerFilter},
		"capability not reported":   {prov: noCaps, reason: k8s.ReasonProviderLacksListOwnerFilter, list: noCaps.listCalls},
		"answer not marked":         {prov: unmarked, reason: k8s.ReasonProviderLacksListOwnerFilter},
		"capabilities failed":       {prov: capsFail, reason: k8s.ReasonUniquenessCheckFailed, list: capsFail.listCalls},
		"listing failed":            {prov: listFail, reason: k8s.ReasonUniquenessCheckFailed},
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-web"
			r := clusteredFixture(t, tc.prov, vm)
			res := reconcileClustered(t, r, "web")
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			got := getVM(t, r, "web")
			placed := placedCondition(got)
			require.NotNil(t, placed)
			assert.Equal(t, tc.reason, placed.Reason)
			assert.Equal(t, tc.reason, provisioningReason(got))
			assert.NotContains(t, placed.Message, "10.0.0.5")
			assert.Empty(t, pendingHost(got))
			if tc.list != nil {
				assert.Zero(t, tc.list(), "no listing is asked of a provider that cannot filter")
			}
			switch p := tc.prov.(type) {
			case *r4Provider:
				assert.Empty(t, p.createReqs)
			case *routingProvider:
				assert.Empty(t, p.createReqs)
			}
		})
	}
}

// TestR4_NeverRunsForABoundOrPendingVM: a bound VM's reconciles and a pending
// create's retries never ask the hosts again.
func TestR4_NeverRunsForABoundOrPendingVM(t *testing.T) {
	bound := clusterVM("bound", clusteredNS, "prov-cluster")
	bound.UID = "uid-bound"
	bound.Status.ID = "default.bound"
	bound.Status.Placement = &infrav1beta1.PlacementStatus{Host: "host-alpha"}
	pending := clusterVM("pending", clusteredNS, "prov-cluster")
	pending.UID = "uid-pending"
	pending.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a"}
	prov := newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
		stampedInfo("host-alpha", "default.pending", clusteredNS, "pending", "uid-previous"),
	}})
	prov.describeResp = contracts.DescribeResponse{Exists: true, PowerState: string(contracts.PowerStateOn)}
	r := clusteredFixture(t, prov, bound, pending)

	for i := 0; i < 3; i++ {
		reconcileClustered(t, r, "bound")
	}
	reconcileClustered(t, r, "pending")

	assert.Zero(t, prov.listCalls(), "R4 runs only before a VM's first placement")
	assert.Zero(t, prov.capsCalls)
	require.Len(t, prov.createReqs, 1, "the pending create is retried on its host")
	assert.Equal(t, "host-alpha", prov.createReqs[0].TargetHostID)
}

// TestR4_OwnDomainOnAHostTheProviderDoesNotFrontHolds: an own domain reported
// on a host that is not a Host of the Provider is never recorded.
func TestR4_OwnDomainOnAHostTheProviderDoesNotFrontHolds(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	prov := newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
		stampedInfo("host-gone", "default.web", clusteredNS, "web", "uid-web"),
	}})
	r := clusteredFixture(t, prov, vm)
	reconcileClustered(t, r, "web")
	got := getVM(t, r, "web")
	assert.Empty(t, prov.createReqs)
	assert.Empty(t, pendingHost(got))
	placed := placedCondition(got)
	assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, placed.Reason)
	assert.NotContains(t, placed.Message, "host-gone")
}

// TestR4_OwnDomainElsewhereMovesThePendingHost: a create on the pending host
// answered with the VM's OWN domain on another host (an administrator
// re-stamped a previous incarnation the disk guard had found elsewhere) moves
// the pending host to where R4's lookup finds that domain — a checked status
// write keeping the admitted size, no administrator status edit — and the
// create retry there binds it.
func TestR4_OwnDomainElsewhereMovesThePendingHost(t *testing.T) {
	vm := clusterVM("web", clusteredNS, "prov-cluster")
	vm.UID = "uid-web"
	vm.Annotations = map[string]string{markerKey: "uid-web"}
	vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a",
		PendingResources: &infrav1beta1.PlacementResources{CPU: 2, MemoryMiB: 4096}}
	prov := newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
		stampedInfo("host-bravo", "default.web", clusteredNS, "web", "uid-web"),
	}})
	prov.onCreate = func(req contracts.CreateRequest) (contracts.CreateResponse, error) {
		if req.TargetHostID == "host-bravo" {
			return contracts.CreateResponse{ID: "default.web"}, nil
		}
		return contracts.CreateResponse{}, ownDomainElsewhereErr("default.web")
	}
	r := clusteredFixture(t, prov, vm, readyHost("host-bravo", clusteredNS, "pool-b", "prov-cluster"))

	res := reconcileClustered(t, r, "web")
	assert.True(t, res.Requeue)
	moved := getVM(t, r, "web")
	assert.Equal(t, "host-bravo", moved.Status.Placement.PendingHost)
	assert.Equal(t, "pool-b", moved.Status.Placement.Pool)
	require.NotNil(t, moved.Status.Placement.PendingResources, "the admitted size is kept")
	assert.EqualValues(t, 2, moved.Status.Placement.PendingResources.CPU)
	assert.Equal(t, k8s.ReasonCreatePending, placedCondition(moved).Reason)
	assert.NotContains(t, placedCondition(moved).Message, "host-bravo")

	reconcileClustered(t, r, "web")
	require.Len(t, prov.createReqs, 2)
	assert.Equal(t, "host-bravo", prov.createReqs[1].TargetHostID)
	bound := getVM(t, r, "web")
	assert.Equal(t, "default.web", bound.Status.ID)
	assert.Equal(t, "host-bravo", bound.Status.Placement.Host)
}

// TestR4_OwnDomainElsewhereKeepsTheHoldWhenTheLookupCannotTell: without a
// single own domain on another Host of the Provider — nothing found, a
// previous incarnation too, or a provider without the filter — the pending
// host is not moved and the own-domain hold of A6.1 stands.
func TestR4_OwnDomainElsewhereKeepsTheHoldWhenTheLookupCannotTell(t *testing.T) {
	for name, prov := range map[string]*r4Provider{
		"nothing found": newR4Provider(contracts.VMList{}),
		"own and a previous incarnation": newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
			stampedInfo("host-bravo", "default.web", clusteredNS, "web", "uid-web"),
			stampedInfo("host-alpha", "web", clusteredNS, "web", "uid-previous")}}),
		"not a Host of the Provider": newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
			stampedInfo("host-gone", "default.web", clusteredNS, "web", "uid-web")}}),
		"no owner filter": func() *r4Provider {
			p := newR4Provider(contracts.VMList{})
			p.caps.SupportsListOwnerFilter = false
			return p
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-web"
			vm.Status.Placement = &infrav1beta1.PlacementStatus{PendingHost: "host-alpha", Pool: "pool-a"}
			prov.onCreate = func(contracts.CreateRequest) (contracts.CreateResponse, error) {
				return contracts.CreateResponse{}, ownDomainElsewhereErr("default.web")
			}
			r := clusteredFixture(t, prov, vm, readyHost("host-bravo", clusteredNS, "pool-a", "prov-cluster"))
			reconcileClustered(t, r, "web")
			held := getVM(t, r, "web")
			assert.Equal(t, "host-alpha", held.Status.Placement.PendingHost, "not moved")
			assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, placedCondition(held).Reason)
		})
	}
}

// TestR4_UnplacedOwnDomainHoldsTheDelete: a VM that was never placed but
// whose own domain R4 found on a host its Provider does not front
// (OwnDomainOnAnotherHost, no pendingHost) is not released on delete — no
// provider call can reach that domain, and releasing the VM would leave it
// running: DeleteBlocked=True/OwnDomainOnAnotherHost, finalizer kept. Force-
// delete and orphan-on-delete still release it at once.
func TestR4_UnplacedOwnDomainHoldsTheDelete(t *testing.T) {
	for name, escape := range map[string]string{
		"held":             "",
		"force-delete":     forceDeleteAnnotation,
		"orphan-on-delete": infrav1beta1.VirtualMachineOrphanOnDeleteAnnotation,
	} {
		t.Run(name, func(t *testing.T) {
			vm := clusterVM("web", clusteredNS, "prov-cluster")
			vm.UID = "uid-web"
			vm.Finalizers = []string{infrav1beta1.VirtualMachineFinalizer}
			prov := newR4Provider(contracts.VMList{VMs: []contracts.VMInfo{
				stampedInfo("host-gone", "default.web", clusteredNS, "web", "uid-web"),
			}})
			r := clusteredFixture(t, prov, vm)
			reconcileClustered(t, r, "web")
			held := getVM(t, r, "web")
			require.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, placedCondition(held).Reason)
			require.Empty(t, pendingHost(held), "never placed")

			if escape != "" {
				held.Annotations = map[string]string{escape: "true"}
				require.NoError(t, r.Update(context.Background(), held))
			}
			res, err := r.handleDeletion(context.Background(), deletingClusterVM(t, r, "web"))
			require.NoError(t, err)
			assert.Empty(t, prov.deleteRefs, "no provider call either way")
			getErr := r.Get(context.Background(), client.ObjectKey{Namespace: clusteredNS, Name: "web"}, &infrav1beta1.VirtualMachine{})
			if escape != "" {
				assert.True(t, apierrors.IsNotFound(getErr), "%s releases the finalizer", escape)
				return
			}
			require.NoError(t, getErr, "the finalizer is kept")
			assert.Equal(t, blockedRetryMin, res.RequeueAfter)
			got := getVM(t, r, "web")
			blocked := meta.FindStatusCondition(got.Status.Conditions, k8s.ConditionDeleteBlocked)
			require.NotNil(t, blocked)
			assert.Equal(t, metav1.ConditionTrue, blocked.Status)
			assert.Equal(t, k8s.ReasonOwnDomainOnAnotherHost, blocked.Reason)
			assert.NotContains(t, blocked.Message, "host-gone")
			assert.Contains(t, got.Finalizers, infrav1beta1.VirtualMachineFinalizer)
		})
	}
}

// ─── VMClone: HOST_UNAVAILABLE backs off ──────────────────────────────────────

// TestVMClone_Clustered_HostUnavailableBacksOff: a clone answered
// HOST_UNAVAILABLE (or VM_DISK_CHECK_FAILED) is retried on the same host with
// the blocked-VM backoff from when the target's placement was recorded — not
// every fixed 30 s.
func TestVMClone_Clustered_HostUnavailableBacksOff(t *testing.T) {
	for name, cloneErr := range map[string]error{
		"host unavailable":  contracts.NewHostUnavailableError(`clone: a host of the Provider is unreachable`, nil),
		"disk check failed": contracts.NewRetryableError("clone: disk check", fmt.Errorf("%w", contracts.ErrVMDiskCheckFailed)),
	} {
		t.Run(name, func(t *testing.T) {
			cp := &clonerProvider{cloneErr: cloneErr}
			r, clone := clusteredCloneFixture(t, boundSource(), cp)
			key := client.ObjectKeyFromObject(clone)
			var res ctrl.Result
			for i := 0; i < 4 && cp.cloneCnt == 0; i++ {
				var err error
				res, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
				require.NoError(t, err)
			}
			require.Equal(t, 1, cp.cloneCnt)
			assert.Equal(t, blockedRetryMin, res.RequeueAfter, "the first retry of the backoff")

			// The hold is three minutes old: the next retry waits about that long.
			target := getTarget(t, r)
			old := metav1.NewTime(time.Now().Add(-3 * time.Minute))
			target.Status.Placement.LastScheduledTime = &old
			for i := range target.Status.Conditions {
				target.Status.Conditions[i].LastTransitionTime = old
			}
			require.NoError(t, r.Status().Update(context.Background(), target))
			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			assert.Equal(t, 2, cp.cloneCnt)
			assert.GreaterOrEqual(t, res.RequeueAfter, 3*time.Minute)
			assert.LessOrEqual(t, res.RequeueAfter, blockedRetryMax)
			assert.Equal(t, "host-alpha", pendingHost(getTarget(t, r)), "the pending host is kept")
			assert.Equal(t, infrav1beta1.ClonePhasePending, getClone(t, r, clone).Status.Phase)
		})
	}
}
