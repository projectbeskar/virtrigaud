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
	"sync"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
)

// placementProviderIndex is the cache field index of every VirtualMachine by
// its placement Provider (placementProviderKey, "namespace/name"). The
// committed-capacity reads — the scheduler's snapshot under the Provider's
// assume lock, the Host gauges and the Host in-use check — list only one
// Provider's VMs through it instead of every VM in the cluster.
const placementProviderIndex = "virtrigaud.io/placement-provider"

// placementProviderIndexValue is the placementProviderIndex extractor: every
// VirtualMachine is indexed, placed or not, because an assumption settles when
// its VM is no longer among the Provider's VMs.
func placementProviderIndexValue(obj client.Object) []string {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok {
		return nil
	}
	return []string{placementProviderKey(vm).String()}
}

// placementIndexRegistered records the field indexers (one per manager cache)
// placementProviderIndex is registered in. The VirtualMachine and Host
// controllers both use the index and both register it; the cache refuses a
// second registration of the same name.
var placementIndexRegistered sync.Map

// indexPlacementProvider registers placementProviderIndex in mgr's cache once.
func indexPlacementProvider(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if _, done := placementIndexRegistered.LoadOrStore(indexer, struct{}{}); done {
		return nil
	}
	if err := indexer.IndexField(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{},
		placementProviderIndex, placementProviderIndexValue); err != nil {
		placementIndexRegistered.Delete(indexer)
		return fmt.Errorf("index VirtualMachine by %s: %w", placementProviderIndex, err)
	}
	return nil
}

// listProviderVMs lists, through placementProviderIndex, the VirtualMachines
// whose placement Provider is provider. The items are shared with the cache
// (UnsafeDisableDeepCopy): callers must treat them as read-only.
func listProviderVMs(ctx context.Context, reader client.Reader, provider types.NamespacedName) ([]infravirtrigaudiov1beta1.VirtualMachine, error) {
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	if err := reader.List(ctx, &vms,
		client.MatchingFields{placementProviderIndex: provider.String()}, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("list VirtualMachines of Provider %s: %w", provider, err)
	}
	return vms.Items, nil
}
