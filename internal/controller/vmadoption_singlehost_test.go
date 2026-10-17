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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// These tests pin single-host adoption through a provider that stamps
// (Capabilities.SupportsOwnerTransfer): every adopted VM is handed to its
// VirtualMachine (TransferOwner, compare-and-swap) before it is bound; a VM
// another tenant's live VirtualMachine owns — or that another Provider object
// for the same endpoint manages — is never stamped; a stale stamp is replaced
// only with the adopt-stamped-vms opt-in and after an uncached check; and
// every state between the VirtualMachine's creation and its binding is
// completed or undone by the next discovery.

// ownerTransferCaps are the capabilities of a single-host provider that stamps
// the VMs it adopts.
var ownerTransferCaps = contracts.Capabilities{SupportsOwnerTransfer: true}

// singleHostStampingProvider is a ready single-host libvirt Provider prov-c
// with adoption on (plus extra annotations).
func singleHostStampingProvider(annotations ...string) *infravirtrigaudiov1beta1.Provider {
	p := readyProvider(clusterNS, "prov-c")
	p.UID = "prov-c-uid"
	p.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeLibvirt
	p.Spec.Endpoint = "qemu+ssh://virt@kvm-01/system"
	p.Annotations = map[string]string{AdoptionAnnotation: "true"}
	for i := 0; i+1 < len(annotations); i += 2 {
		p.Annotations[annotations[i]] = annotations[i+1]
	}
	return p
}

// stampingReconciler is the adoption reconciler over prov with an uncached
// reader (the same fake client; it is never stale).
func stampingReconciler(t *testing.T, prov contracts.Provider, objs ...client.Object) *VMAdoptionReconciler {
	t.Helper()
	r := clusteredAdoptionReconciler(t, prov, objs...)
	r.APIReader = r.Client
	return r
}

// singleHostVMs returns every VirtualMachine in the cluster namespace.
func singleHostVMs(t *testing.T, r *VMAdoptionReconciler) []infravirtrigaudiov1beta1.VirtualMachine {
	t.Helper()
	var list infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &list, client.InNamespace(clusterNS)))
	return list.Items
}

// rediscover makes the next reconcile run a discovery.
func rediscover(t *testing.T, r *VMAdoptionReconciler) {
	t.Helper()
	r.setLastDiscovery(t, time.Now().Add(-singleHostDiscoveryInterval-time.Second))
}

// TestSingleHostStampedAdoption_StampsBeforeBinding: an unstamped VM is
// adopted by a VirtualMachine that carries its id; the VM is stamped with
// that VirtualMachine's identity (no host, nothing replaceable, the listed
// UUID) and only then bound.
func TestSingleHostStampedAdoption_StampsBeforeBinding(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: ownerTransferCaps, domains: []*fakeDomain{
		{id: "web", uuid: "uuid-web", power: "On", cpu: 2, memMiB: 2048},
	}}
	r := stampingReconciler(t, prov, singleHostStampingProvider())

	res := reconcileAdoption(t, r)
	assert.Equal(t, clusteredAdoptionInterval, res.RequeueAfter)

	vm := adoptedVMGet(t, r, "web")
	assert.Equal(t, "web", vm.Status.ID)
	assert.Equal(t, "web", vm.Annotations[AdoptedIDAnnotation])
	assert.Equal(t, AdoptedLabelValue, vm.Labels[AdoptedLabel])
	require.NotNil(t, vm.Status.BoundProvider)
	assert.Equal(t, "prov-c", vm.Status.BoundProvider.Name)
	assert.NotContains(t, vm.Status.Provider, contracts.VMInfoOwnerUIDKey)

	transfers := prov.transfersTo("", "web")
	require.Len(t, transfers, 1)
	assert.Equal(t, contracts.ObjectIdentity{UID: string(vm.UID), Namespace: clusterNS, Name: "web"}, transfers[0].VM.Owner)
	assert.Empty(t, transfers[0].VM.HostID, "a single-host transfer names no host")
	assert.Empty(t, transfers[0].ReplaceableOwnerUIDs, "an unstamped VM has nothing to replace")
	assert.Equal(t, "uuid-web", transfers[0].ExpectedUUID)
	assert.Equal(t, string(vm.UID), prov.find("", "web").owner, "the VM carries its new owner's stamp")
	assert.Contains(t, adoptionStatus(t, r).Message, "Successfully adopted 1 VMs")

	// The next discovery sees the VM owned by a live VirtualMachine: nothing
	// more is created or transferred.
	rediscover(t, r)
	reconcileAdoption(t, r)
	assert.Len(t, singleHostVMs(t, r), 1)
	assert.Len(t, prov.transfersTo("", "web"), 1)
}

// TestSingleHostStampedAdoption_StaleStampNeedsOptInAndUncachedProof: a VM
// left stamped by a VirtualMachine that was detached (its UID exists nowhere)
// is adopted only with adopt-stamped-vms, and its stale stamp is the only one
// the provider may replace. Without an uncached reader the stamp cannot be
// proven stale and nothing is adopted (fail closed).
func TestSingleHostStampedAdoption_StaleStampNeedsOptInAndUncachedProof(t *testing.T) {
	newProv := func() *fakeClusteredAdopter {
		return &fakeClusteredAdopter{caps: ownerTransferCaps, domains: []*fakeDomain{
			{id: "old", uuid: "uuid-old", owner: "uid-detached", power: "Off"},
		}}
	}

	t.Run("without the opt-in: reported, not adopted", func(t *testing.T) {
		prov := newProv()
		r := stampingReconciler(t, prov, singleHostStampingProvider())
		reconcileAdoption(t, r)
		assert.Empty(t, singleHostVMs(t, r))
		assert.Empty(t, prov.transfers)
		assert.Contains(t, adoptionStatus(t, r).Message, "1 not adopted")
	})

	t.Run("with the opt-in: the stale stamp is replaced", func(t *testing.T) {
		prov := newProv()
		r := stampingReconciler(t, prov, singleHostStampingProvider(AdoptStampedVMsAnnotation, "true"))
		reconcileAdoption(t, r)
		vm := adoptedVMGet(t, r, "old")
		assert.Equal(t, "old", vm.Status.ID)
		transfers := prov.transfersTo("", "old")
		require.Len(t, transfers, 1)
		assert.Equal(t, []string{"uid-detached"}, transfers[0].ReplaceableOwnerUIDs)
		assert.Equal(t, string(vm.UID), prov.find("", "old").owner)
	})

	t.Run("with the opt-in but no uncached reader: nothing adopted, retried", func(t *testing.T) {
		prov := newProv()
		r := stampingReconciler(t, prov, singleHostStampingProvider(AdoptStampedVMsAnnotation, "true"))
		r.APIReader = nil
		res := reconcileAdoption(t, r)
		assert.Empty(t, singleHostVMs(t, r))
		assert.Empty(t, prov.transfers)
		assert.Contains(t, adoptionStatus(t, r).Message, string(skipOwnerUnverified))
		assert.Equal(t, singleHostDiscoveryInterval, res.RequeueAfter)
	})
}

// TestSingleHostStampedAdoption_LiveOwnerOutsideTheCacheIsNeverStamped: the
// cache does not show the VirtualMachine a VM's stamp names (it lags), but the
// API server does: the VM is another tenant's and is neither transferred nor
// adopted, even with the opt-in.
func TestSingleHostStampedAdoption_LiveOwnerOutsideTheCacheIsNeverStamped(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: ownerTransferCaps, domains: []*fakeDomain{
		{id: "team-b.db", uuid: "uuid-db", owner: "uid-team-b-db", power: "On"},
	}}
	r := stampingReconciler(t, prov, singleHostStampingProvider(AdoptStampedVMsAnnotation, "true"))
	apiServer := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(
		&infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Namespace: clusterNS, Name: "prov-c", UID: "prov-c-uid"}},
		&infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "db", UID: "uid-team-b-db"}},
	).WithStatusSubresource(&infravirtrigaudiov1beta1.Provider{}).Build()
	r.APIReader = &listOnlyReader{Reader: r.Client, lister: apiServer}

	reconcileAdoption(t, r)
	assert.Empty(t, prov.transfers, "a live tenant's VM is never re-stamped")
	assert.Empty(t, singleHostVMs(t, r))
	assert.Contains(t, adoptionStatus(t, r).Message, string(skipOwnerLive))
	assert.NotContains(t, adoptionStatus(t, r).Message, "team-b/db", "the other owner is never named")
}

// listOnlyReader serves List from lister (the "API server") and Get from the
// embedded reader.
type listOnlyReader struct {
	client.Reader
	lister client.Reader
}

func (l *listOnlyReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return l.lister.List(ctx, list, opts...)
}

// TestSingleHostStampedAdoption_VMOfASiblingProviderIsNotTaken: an unstamped
// VM bound (by status.id) to a VirtualMachine of ANOTHER Provider object for
// the same endpoint is managed there: it is skipped and reported, never
// stamped (that would take it from the other tenant).
func TestSingleHostStampedAdoption_VMOfASiblingProviderIsNotTaken(t *testing.T) {
	sibling := singleHostStampingProvider()
	sibling.Namespace, sibling.Name, sibling.UID = "team-b", "kvm", "sibling-uid"
	sibling.Annotations = nil
	legacy := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "legacy", UID: "uid-legacy"},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "kvm"}},
		Status:     infravirtrigaudiov1beta1.VirtualMachineStatus{ID: "legacy"},
	}
	prov := &fakeClusteredAdopter{caps: ownerTransferCaps, domains: []*fakeDomain{
		{id: "legacy", uuid: "uuid-legacy", power: "On"},
	}}
	r := stampingReconciler(t, prov, singleHostStampingProvider(), sibling, legacy)

	reconcileAdoption(t, r)
	assert.Empty(t, prov.transfers)
	assert.Empty(t, singleHostVMs(t, r))
	assert.Contains(t, adoptionStatus(t, r).Message, string(skipBoundElsewhere))
}

// TestSingleHostStampedAdoption_RefusedTransferRemovesTheWaitingVM: a
// transfer the provider refuses for good (the VM became someone else's
// between the listing and the call) removes the VirtualMachine created for it
// — it has no status.id, so its deletion makes no provider call — and the
// refusal is reported.
func TestSingleHostStampedAdoption_RefusedTransferRemovesTheWaitingVM(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: ownerTransferCaps, transferErr: contracts.NewConflictError("owned by another", nil),
		domains: []*fakeDomain{{id: "web", uuid: "uuid-web", power: "On"}}}
	r := stampingReconciler(t, prov, singleHostStampingProvider())

	reconcileAdoption(t, r)
	require.Len(t, prov.transfers, 1)
	err := r.Get(context.Background(), types.NamespacedName{Namespace: clusterNS, Name: "web"}, &infravirtrigaudiov1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "the waiting VirtualMachine was removed: %v", err)
	assert.Contains(t, adoptionStatus(t, r).Message, string(skipTransferRefused))
}

// TestSingleHostStampedAdoption_RestartWindowIsCompleted: a failure (or a
// manager restart) between the VirtualMachine's creation and its binding is
// finished by the next discovery — first a transfer that failed retryably,
// then a binding lost after the VM was stamped — with exactly one
// VirtualMachine and no second VM.
func TestSingleHostStampedAdoption_RestartWindowIsCompleted(t *testing.T) {
	prov := &fakeClusteredAdopter{caps: ownerTransferCaps, transferErr: contracts.NewRetryableError("provider restarting", nil),
		domains: []*fakeDomain{{id: "web", uuid: "uuid-web", power: "On"}}}
	r := stampingReconciler(t, prov, singleHostStampingProvider())

	res := reconcileAdoption(t, r)
	assert.Equal(t, singleHostDiscoveryInterval, res.RequeueAfter, "a failed adoption is retried sooner")
	waiting := adoptedVMGet(t, r, "web")
	assert.Empty(t, waiting.Status.ID, "never bound before the VM carries its stamp")
	assert.Equal(t, "web", waiting.Annotations[AdoptedIDAnnotation])
	assert.Empty(t, prov.find("", "web").owner)

	// The transfer now works: the waiting VirtualMachine is reused.
	prov.mu.Lock()
	prov.transferErr = nil
	prov.mu.Unlock()
	rediscover(t, r)
	reconcileAdoption(t, r)
	bound := adoptedVMGet(t, r, "web")
	assert.Equal(t, waiting.UID, bound.UID, "the same VirtualMachine, not a second one")
	assert.Equal(t, "web", bound.Status.ID)
	assert.Equal(t, string(bound.UID), prov.find("", "web").owner)
	assert.Len(t, singleHostVMs(t, r), 1)

	// A binding lost after the stamp (crash between the transfer and the
	// status write): the listing shows the VM stamped with the waiting
	// VirtualMachine's UID; the binding is written without another transfer.
	lost := bound.DeepCopy()
	lost.Status.ID = ""
	lost.Status.BoundProvider = nil
	require.NoError(t, r.Status().Update(context.Background(), lost))
	before := len(prov.transfersTo("", "web"))
	rediscover(t, r)
	reconcileAdoption(t, r)
	assert.Equal(t, "web", adoptedVMGet(t, r, "web").Status.ID)
	assert.Len(t, prov.transfersTo("", "web"), before, "no second transfer")
}

// TestSingleHostStampedAdoption_WaitingVMWhoseVMIsTakenIsWithdrawn: a
// VirtualMachine left waiting for a VM that a live VirtualMachine owns now is
// removed (it would wait forever); the VM is untouched.
func TestSingleHostStampedAdoption_WaitingVMWhoseVMIsTakenIsWithdrawn(t *testing.T) {
	owner := &infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "web", UID: "uid-live"}}
	waiting := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: clusterNS, Name: "web", UID: "uid-waiting",
			Labels: map[string]string{AdoptedLabel: AdoptedLabelValue}, Annotations: map[string]string{AdoptedIDAnnotation: "web"}},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c", Namespace: clusterNS}},
	}
	prov := &fakeClusteredAdopter{caps: ownerTransferCaps, domains: []*fakeDomain{
		{id: "web", uuid: "uuid-web", owner: "uid-live", power: "On"},
	}}
	r := stampingReconciler(t, prov, singleHostStampingProvider(), owner, waiting)

	reconcileAdoption(t, r)
	err := r.Get(context.Background(), client.ObjectKeyFromObject(waiting), &infravirtrigaudiov1beta1.VirtualMachine{})
	assert.True(t, apierrors.IsNotFound(err), "withdrawn: %v", err)
	assert.Empty(t, prov.transfers)
	assert.Equal(t, "uid-live", prov.find("", "web").owner)
	assert.Contains(t, adoptionStatus(t, r).Message, "1 unfinished adoption(s) withdrawn")
}

// TestSingleHostAdoption_CapabilitiesGateTheStamp: through a provider that
// cannot stamp, adoption is unchanged (bound at once, unstamped) and the
// status says so; one whose capabilities cannot be read adopts nothing.
func TestSingleHostAdoption_CapabilitiesGateTheStamp(t *testing.T) {
	t.Run("older provider: adopted unstamped, reported", func(t *testing.T) {
		prov := &fakeClusteredAdopter{domains: []*fakeDomain{{id: "web", uuid: "uuid-web", power: "On"}}}
		r := stampingReconciler(t, prov, singleHostStampingProvider())
		reconcileAdoption(t, r)
		assert.Equal(t, "web", adoptedVMGet(t, r, "web").Status.ID)
		assert.Empty(t, prov.transfers)
		assert.Contains(t, adoptionStatus(t, r).Message, "supportsOwnerTransfer=false")
	})
	t.Run("capabilities unreadable: nothing adopted", func(t *testing.T) {
		prov := &fakeClusteredAdopter{capsErr: errors.New("unavailable"), domains: []*fakeDomain{{id: "web", uuid: "uuid-web"}}}
		r := stampingReconciler(t, prov, singleHostStampingProvider())
		res := reconcileAdoption(t, r)
		assert.Equal(t, singleHostDiscoveryInterval, res.RequeueAfter)
		assert.Empty(t, singleHostVMs(t, r))
		assert.Zero(t, prov.lists, "nothing is listed")
		assert.Equal(t, adoptionCapabilitiesUnknownMessage, adoptionStatus(t, r).Message)
	})
}

// TestPlanSingleHostAdoption pins the plan: artifacts, unreliable stamps and
// libvirt domain names virsh would read as an id are never candidates; a
// waiting adopter created by an older manager (no annotation) is never
// withdrawn; a clone target is never mistaken for an adopter.
func TestPlanSingleHostAdoption(t *testing.T) {
	provider := singleHostStampingProvider(AdoptStampedVMsAnnotation, "true")
	info := func(id, owner string, raw ...string) contracts.VMInfo {
		r := map[string]string{contracts.VMInfoUUIDKey: "u-" + id}
		if owner != "" {
			r[contracts.VMInfoOwnerUIDKey] = owner
		}
		for i := 0; i+1 < len(raw); i += 2 {
			r[raw[i]] = raw[i+1]
		}
		return contracts.VMInfo{ID: id, Name: id, ProviderRaw: r}
	}
	legacyWaiting := infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: clusterNS, Name: "old-mgr", UID: "uid-old-mgr", Labels: map[string]string{AdoptedLabel: AdoptedLabelValue}},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c"},
			ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "old-mgr", Source: adoptedDiskSource}},
	}
	cloneTarget := infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: clusterNS, Name: "clone", UID: "uid-clone", Labels: map[string]string{AdoptedLabel: AdoptedLabelValue}},
		Spec:       infravirtrigaudiov1beta1.VirtualMachineSpec{ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "prov-c"}},
	}
	live := infravirtrigaudiov1beta1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "team-x", Name: "x", UID: "uid-live"}}

	plan := planSingleHostAdoption(provider, []contracts.VMInfo{
		info("tmpl", "", contracts.VMInfoArtifactKey, contracts.ArtifactTemplate),
		info("broken", "", contracts.VMInfoOwnerStampStateKey, contracts.OwnerStampUnreadable),
		info("12", ""),
		info("old-mgr", "uid-live"),
		info("fresh", ""),
		info("stale", "uid-gone"),
	}, []infravirtrigaudiov1beta1.VirtualMachine{legacyWaiting, cloneTarget, live}, nil)

	var adopt []string
	for _, c := range plan.adopt {
		adopt = append(adopt, c.info.ID)
	}
	assert.ElementsMatch(t, []string{"fresh", "stale"}, adopt)
	assert.Empty(t, plan.release, "an older manager's waiting adopter is never withdrawn")
	assert.Equal(t, []string{string(skipAmbiguousName)}, func() []string {
		var out []string
		for _, sk := range plan.skipped {
			out = append(out, string(sk.reason))
		}
		return out
	}())
	assert.Equal(t, "", adoptionIDOf(&cloneTarget), "a clone target is not an adopter")
	assert.Equal(t, "old-mgr", adoptionIDOf(&legacyWaiting))
}
