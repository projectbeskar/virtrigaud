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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// TestUnmanagedProviderVMs pins what adoption considers already managed. With
// namespaced libvirt domain names the operator cannot derive a domain's name
// from a VirtualMachine that has no status.id yet, and a shared host can be
// fronted by several Provider objects; the owner stamp the provider reports
// closes both gaps: a domain stamped with a LIVE VirtualMachine's UID is never
// re-adopted, and an unstamped domain is adoptable. A domain stamped only with
// UIDs of no VirtualMachine in this cluster is adoptable only with the
// Provider's adopt-stamped-vms opt-in.
func TestUnmanagedProviderVMs(t *testing.T) {
	provider := &infravirtrigaudiov1beta1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "libvirt", Namespace: "infra"}}
	vm := func(ns, name, uid, providerNS, statusID string) infravirtrigaudiov1beta1.VirtualMachine {
		v := infravirtrigaudiov1beta1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
			Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
				ProviderRef: infravirtrigaudiov1beta1.ObjectRef{Name: "libvirt", Namespace: providerNS},
			},
		}
		v.Status.ID = statusID
		return v
	}
	vms := []infravirtrigaudiov1beta1.VirtualMachine{
		vm("team-a", "web", "uid-a", "infra", "team-a.web"), // bound: matched by status.id
		vm("team-b", "web", "uid-b", "infra", ""),           // create in flight: no status.id yet
		vm("team-c", "db", "uid-c", "team-c", "team-c.db"),  // managed through ANOTHER Provider object
		vm("team-d", "legacy", "uid-d", "infra", ""),        // legacy bare-named create in flight
	}
	stamped := func(id string, uids string) contracts.VMInfo {
		info := contracts.VMInfo{ID: id, Name: id, ProviderRaw: map[string]string{}}
		if uids != "" {
			info.ProviderRaw[contracts.VMInfoOwnerUIDKey] = uids
		}
		return info
	}
	withState := func(info contracts.VMInfo, state string) contracts.VMInfo {
		info.ProviderRaw[contracts.VMInfoOwnerStampStateKey] = state
		return info
	}
	withArtifact := func(info contracts.VMInfo, kind string) contracts.VMInfo {
		info.ProviderRaw[contracts.VMInfoArtifactKey] = kind
		return info
	}
	all := []contracts.VMInfo{
		// Owner stamp the provider cannot rely on: never offered, even when
		// every UID it records is stale or there is none.
		withState(stamped("unreadable", ""), contracts.OwnerStampUnreadable),
		withState(stamped("unreadable-stale", "uid-deleted"), contracts.OwnerStampUnreadable),
		withState(stamped("multiple-stale", "uid-deleted,uid-gone"), contracts.OwnerStampMultiple),
		withState(stamped("future-state", ""), "some-future-state"), // any value fails closed
		// Marked by the provider as shared image state: never offered, whatever
		// the value (even empty or unknown).
		withArtifact(stamped("tmpl", ""), contracts.ArtifactTemplate),
		withArtifact(stamped("img", "uid-deleted"), contracts.ArtifactImage),
		withArtifact(stamped("artifact-empty", ""), ""),
		withArtifact(stamped("artifact-future", ""), "future-kind"),
		stamped("team-a.web", "uid-a"),         // managed (status.id)
		stamped("team-b.web", "uid-b"),         // managed (live owner stamp; no status.id yet)
		stamped("team-c.db", "uid-c"),          // managed via another Provider object (live owner stamp)
		stamped("legacy", ""),                  // managed (legacy name fallback)
		stamped("team-e.old", "uid-deleted"),   // no VirtualMachine here: opt-in only
		stamped("hand-made", ""),               // never created by VirtRigaud: adoptable
		stamped("two-stamps", "uid-x,uid-b"),   // any live UID keeps it managed
		stamped("garbage-stamp", " , ,uid-zz"), // no live UID: opt-in only
		stamped("blank-stamp", " , "),          // no UID at all: unstamped, adoptable
	}
	ids := func(infos []contracts.VMInfo) []string {
		var out []string
		for _, v := range infos {
			out = append(out, v.ID)
		}
		return out
	}

	// Default: a stamp naming no VirtualMachine in this cluster may be a live
	// VirtualMachine of another installation sharing the host, so it is
	// reported, never adopted.
	unmanaged, foreign := unmanagedProviderVMs(provider, all, vms)
	assert.ElementsMatch(t, []string{"hand-made", "blank-stamp"}, ids(unmanaged))
	assert.ElementsMatch(t, []string{"team-e.old", "garbage-stamp"}, ids(foreign))

	// Any value other than "true" is not the opt-in.
	provider.Annotations = map[string]string{AdoptStampedVMsAnnotation: "yes"}
	unmanaged, foreign = unmanagedProviderVMs(provider, all, vms)
	assert.ElementsMatch(t, []string{"hand-made", "blank-stamp"}, ids(unmanaged))
	assert.Len(t, foreign, 2)

	// Opted in: stale stamps are adoptable. Nothing else changes: live-owned,
	// unreliable-stamp and artifact VMs are still never offered.
	provider.Annotations = map[string]string{AdoptStampedVMsAnnotation: "true"}
	unmanaged, foreign = unmanagedProviderVMs(provider, all, vms)
	assert.ElementsMatch(t, []string{"team-e.old", "hand-made", "garbage-stamp", "blank-stamp"}, ids(unmanaged))
	assert.Empty(t, foreign)
}

func TestForeignStampedSummary(t *testing.T) {
	assert.Empty(t, foreignStampedSummary(0))
	assert.Contains(t, foreignStampedSummary(2), "2 not adopted")
	assert.Contains(t, foreignStampedSummary(2), AdoptStampedVMsAnnotation+"=true")
}

// TestSingleHostAdoption_RecentDiscoveryDoesNotList: a reconcile within
// singleHostDiscoveryInterval of the last discovery returns before ListVMs
// (which reads every VM of the hypervisor), and requeues for the rest of the
// interval. A stamped VM of no VirtualMachine here is reported, not adopted.
func TestSingleHostAdoption_RecentDiscoveryDoesNotList(t *testing.T) {
	prov := &fakeClusteredAdopter{domains: []*fakeDomain{
		{id: "foreign", uuid: "u-foreign", owner: "uid-other-cluster", power: "On"},
	}}
	provider := readyProvider(clusterNS, "prov-c")
	provider.Annotations = map[string]string{AdoptionAnnotation: "true"}
	r := clusteredAdoptionReconciler(t, prov, provider)

	reconcileAdoption(t, r)
	require.Equal(t, 1, prov.lists, "the first discovery lists")
	st := adoptionStatus(t, r)
	require.NotNil(t, st.LastDiscoveryTime)
	assert.Contains(t, st.Message, "1 not adopted", "the foreign-stamped VM is reported")
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	require.NoError(t, r.List(context.Background(), &vms))
	assert.Empty(t, vms.Items, "the foreign-stamped VM is not adopted")

	res := reconcileAdoption(t, r)
	assert.Equal(t, 1, prov.lists, "a discovery within the interval does not list again")
	assert.Positive(t, res.RequeueAfter)
	assert.LessOrEqual(t, res.RequeueAfter, singleHostDiscoveryInterval)

	r.setLastDiscovery(t, time.Now().Add(-singleHostDiscoveryInterval-time.Second))
	reconcileAdoption(t, r)
	assert.Equal(t, 2, prov.lists, "an older discovery lists again")
}
