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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A single-host delete carries the VM's owner, so an owner-checking provider
// (Proxmox) never destroys a VM stamped with another owner.
func TestHandleDeletion_SingleHostDeleteCarriesOwner(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{}
	vm := deletionVM("web")
	vm.UID = types.UID("uid-web")
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	_, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	require.Len(t, prov.deleteRefs, 1)
	assert.Empty(t, prov.deleteRefs[0].HostID, "still a single-host delete")
	assert.Equal(t, contracts.ObjectIdentity{UID: "uid-web", Namespace: "default", Name: "web"}, prov.deleteOwners[0])
	assert.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(vm), &infravirtrigaudiov1beta1.VirtualMachine{})))
}

// An adopted VirtualMachine's delete carries no owner: its hypervisor VM is
// unstamped, and its name may not be the VirtualMachine's.
func TestHandleDeletion_AdoptedDeleteCarriesNoOwner(t *testing.T) {
	ctx := context.Background()
	prov := &routingProvider{}
	vm := deletionVM("web-01")
	vm.UID = types.UID("uid-adopted")
	vm.Labels = map[string]string{AdoptedLabel: AdoptedLabelValue}
	r := newTestReconciler(coverageTestScheme(t), &stubResolver{provider: prov}, vm, deletionProviderCR())
	marked := markForDeletion(t, r, vm)

	_, err := r.handleDeletion(ctx, marked)
	require.NoError(t, err)
	require.Len(t, prov.deleteOwners, 1)
	assert.True(t, prov.deleteOwners[0].IsZero(), "an adopted VM's delete is unchanged")
}

func TestDeleteRef(t *testing.T) {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	vm.Name, vm.Namespace, vm.UID = "web", "team-a", "uid-1"
	owner := contracts.ObjectIdentity{UID: "uid-1", Namespace: "team-a", Name: "web"}

	assert.Equal(t, owner, deleteRef(vm, contracts.VMRef{ID: "100"}).Owner, "single-host: the owner is added")

	routed := contracts.VMRef{ID: "web", HostID: "h1", Owner: contracts.ObjectIdentity{UID: "uid-1"}}
	assert.Equal(t, routed, deleteRef(vm, routed), "a routed ref is left as built")

	noUID := vm.DeepCopy()
	noUID.UID = ""
	assert.True(t, deleteRef(noUID, contracts.VMRef{ID: "100"}).Owner.IsZero())
}
