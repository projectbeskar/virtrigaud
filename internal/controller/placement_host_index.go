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

// placementHostIndex is the cache field index of every VirtualMachine by each
// host it holds a placement on, qualified by its placement Provider:
// "<provider namespace>/<provider name>|<host>" for each of placementHosts(vm)
// (its confirmed host and its pending host, the same host once). A VM that no
// longer holds a placement (being deleted, its VirtualMachine finalizer gone)
// has no value. It lets the per-host accounting — the Host's committed-capacity
// gauges (committedOnHost) and its in-use check (vmsUsingHost) — read the VMs
// on one host instead of every VM of the Provider. Two Providers with
// same-named Hosts never share a value.
const placementHostIndex = "virtrigaud.io/placement-host"

// placementHostIndexValue is the placementHostIndex extractor.
func placementHostIndexValue(obj client.Object) []string {
	vm, ok := obj.(*infravirtrigaudiov1beta1.VirtualMachine)
	if !ok {
		return nil
	}
	hosts := placementHosts(vm)
	if len(hosts) == 0 {
		return nil
	}
	provider := placementProviderKey(vm)
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, placementHostIndexKey(provider, h))
	}
	return out
}

// placementHostIndexKey is the placementHostIndex value of host under provider.
// "|" cannot appear in a Provider or Host name (DNS-1123).
func placementHostIndexKey(provider types.NamespacedName, host string) string {
	return provider.String() + "|" + host
}

// placementHostIndexRegistered records the field indexers (one per manager
// cache) placementHostIndex is registered in.
var placementHostIndexRegistered sync.Map

// indexPlacementHost registers placementHostIndex in mgr's cache once.
func indexPlacementHost(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if _, done := placementHostIndexRegistered.LoadOrStore(indexer, struct{}{}); done {
		return nil
	}
	if err := indexer.IndexField(context.Background(), &infravirtrigaudiov1beta1.VirtualMachine{},
		placementHostIndex, placementHostIndexValue); err != nil {
		placementHostIndexRegistered.Delete(indexer)
		return fmt.Errorf("index VirtualMachine by %s: %w", placementHostIndex, err)
	}
	return nil
}

// listHostPlacementVMs lists, through placementHostIndex, the VirtualMachines
// whose placement under provider names host. The items are shared with the
// cache (UnsafeDisableDeepCopy): callers must treat them as read-only.
func listHostPlacementVMs(ctx context.Context, reader client.Reader, provider types.NamespacedName, host string) ([]infravirtrigaudiov1beta1.VirtualMachine, error) {
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	if err := reader.List(ctx, &vms,
		client.MatchingFields{placementHostIndex: placementHostIndexKey(provider, host)}, client.UnsafeDisableDeepCopy); err != nil {
		return nil, fmt.Errorf("list VirtualMachines on host %s of Provider %s: %w", host, provider, err)
	}
	return vms.Items, nil
}
