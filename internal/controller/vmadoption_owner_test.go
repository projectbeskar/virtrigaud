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
	"testing"

	"github.com/stretchr/testify/assert"
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
// re-adopted, while an orphan (stamped by a deleted VirtualMachine) or an
// unstamped domain is adoptable.
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
	all := []contracts.VMInfo{
		stamped("team-a.web", "uid-a"),         // managed (status.id)
		stamped("team-b.web", "uid-b"),         // managed (live owner stamp; no status.id yet)
		stamped("team-c.db", "uid-c"),          // managed via another Provider object (live owner stamp)
		stamped("legacy", ""),                  // managed (legacy name fallback)
		stamped("team-e.old", "uid-deleted"),   // orphan of a deleted VirtualMachine: adoptable
		stamped("hand-made", ""),               // never created by VirtRigaud: adoptable
		stamped("two-stamps", "uid-x,uid-b"),   // any live UID keeps it managed
		stamped("garbage-stamp", " , ,uid-zz"), // no live UID: adoptable
	}

	var got []string
	for _, v := range unmanagedProviderVMs(provider, all, vms) {
		got = append(got, v.ID)
	}
	assert.ElementsMatch(t, []string{"team-e.old", "hand-made", "garbage-stamp"}, got)
}
