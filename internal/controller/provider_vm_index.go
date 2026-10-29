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
	"maps"
	"sync"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// specProviderIndex is the cache field index of every VirtualMachine by the
// Provider its spec.providerRef names (vmProviderKey, "namespace/name"; an
// empty namespace is the VM's own). It serves Provider.status.connectedVMs
// (countConnectedVMs), which counts the VMs that reference a Provider —
// bound or not — without listing every VirtualMachine in the cluster.
const specProviderIndex = "virtrigaud.io/spec-provider"

// specProviderIndexValue is the specProviderIndex extractor.
func specProviderIndexValue(obj client.Object) []string {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok {
		return nil
	}
	return []string{vmProviderKey(vm).String()}
}

// specProviderIndexRegistered records the field indexers (one per manager
// cache) specProviderIndex is registered in.
var specProviderIndexRegistered sync.Map

// indexSpecProvider registers specProviderIndex in mgr's cache once.
func indexSpecProvider(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if _, done := specProviderIndexRegistered.LoadOrStore(indexer, struct{}{}); done {
		return nil
	}
	if err := indexer.IndexField(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{},
		specProviderIndex, specProviderIndexValue); err != nil {
		specProviderIndexRegistered.Delete(indexer)
		return fmt.Errorf("index VirtualMachine by %s: %w", specProviderIndex, err)
	}
	return nil
}

// countSpecProviderVMs counts the VirtualMachines whose spec.providerRef names
// provider, through specProviderIndex. The listed items are shared with the
// cache (UnsafeDisableDeepCopy) and only read.
func countSpecProviderVMs(ctx context.Context, reader client.Reader, provider types.NamespacedName) (int32, error) {
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	if err := reader.List(ctx, &vms,
		client.MatchingFields{specProviderIndex: provider.String()}, client.UnsafeDisableDeepCopy); err != nil {
		return 0, fmt.Errorf("list VirtualMachines referencing Provider %s: %w", provider, err)
	}
	count := int32(0)
	for i := range vms.Items {
		// The key is re-checked (defense in depth): the index value is the key.
		if vmProviderKey(&vms.Items[i]) == provider {
			count++
		}
	}
	return count, nil
}

// hostInventoryChanged filters the Provider controller's Host watch to the
// events that can change what the Provider reconcile reads from a Host: its
// creation and deletion, and an update to its spec (providerRef, poolRef,
// endpoint, credentialSecretRef and labels are rendered into the
// host-inventory Secret), its metadata labels or annotations, its UID or its
// deletion timestamp. Host status — the inventory heartbeat the Host
// controller writes every minute, capacity, health, conditions — is not read
// by the Provider reconcile (reconcileHostInventorySecret reads spec and
// identity only), so a status-only write enqueues nothing. The spec is
// compared directly, not only through metadata.generation.
func hostInventoryChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldHost, okOld := e.ObjectOld.(*infravirtrigaudiov1beta1.Host)
			newHost, okNew := e.ObjectNew.(*infravirtrigaudiov1beta1.Host)
			if !okOld || !okNew {
				return true
			}
			return oldHost.UID != newHost.UID ||
				oldHost.Generation != newHost.Generation ||
				!equality.Semantic.DeepEqual(oldHost.Spec, newHost.Spec) ||
				!oldHost.DeletionTimestamp.Equal(newHost.DeletionTimestamp) ||
				!maps.Equal(oldHost.Labels, newHost.Labels) ||
				!maps.Equal(oldHost.Annotations, newHost.Annotations)
		},
	}
}
