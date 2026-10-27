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
	"slices"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// The reference accounting: the committed-placement read as it was before
// the allocation-efficient snapshot (C′), kept verbatim as the oracle the
// equivalence tests compare the production path with. It lists the
// Provider's VMs (apimeta.SetList copies every object), records every VM, and
// lists every placement individually for the scheduler to merge.

// refSnapshot is the reference committedSnapshot.
type refSnapshot struct {
	placed   []scheduler.PlacedVM
	recorded map[string]recordedVM
}

// settled is the reference committedSnapshot.settled.
func (s refSnapshot) settled(a assume.Assumption) bool {
	rec, present := s.recorded[a.UID]
	if !present {
		return true
	}
	if a.Resize {
		return !slices.Contains(rec.hosts, a.HostID) ||
			(rec.hasSize && rec.size.CPU >= a.Resources.CPU && rec.size.MemoryMiB >= a.Resources.MemoryMiB)
	}
	return slices.Contains(rec.hosts, a.HostID)
}

// observe is the reference committedSnapshot.observe.
func (s refSnapshot) observe(a assume.Assumption) assume.Observation {
	if _, present := s.recorded[a.UID]; !present {
		return assume.Observation{}
	}
	return assume.Observation{Present: true, Settled: s.settled(a)}
}

// refCommittedPlacements is the reference committedPlacementsFrom.
func refCommittedPlacements(
	ctx context.Context,
	reader client.Reader,
	provider types.NamespacedName,
	self *infravirtrigaudiov1beta1.VirtualMachine,
) (refSnapshot, error) {
	vms, err := listProviderVMs(ctx, reader, provider)
	if err != nil {
		return refSnapshot{}, err
	}
	snap := refSnapshot{recorded: map[string]recordedVM{}}
	selfUID := vmSchedulingUID(self)
	classes := classMemo{}
	for i := range vms {
		other := &vms[i]
		if placementProviderKey(other) != provider || !holdsPlacement(other) {
			continue
		}
		uid := vmSchedulingUID(other)
		hosts := placementHosts(other)
		if len(hosts) == 0 || uid == selfUID {
			snap.recorded[uid] = recordedVM{hosts: hosts}
			continue
		}
		res, err := sizeForAccounting(ctx, reader, other, classes)
		if err != nil {
			return refSnapshot{}, err
		}
		_, reserved := resizeReservationOf(other)
		snap.recorded[uid] = recordedVM{hosts: hosts, size: res, hasSize: hasRecordedSize(other) || reserved}
		for _, h := range hosts {
			snap.placed = append(snap.placed, scheduler.PlacedVM{
				Name:         other.Name,
				UID:          uid,
				HostID:       h,
				Labels:       other.Labels,
				Resources:    res,
				CapacityOnly: other.Namespace != self.Namespace,
			})
		}
	}
	return snap, nil
}

// refPlacedWithAssumptions is the reference placedWithAssumptionsFrom.
func refPlacedWithAssumptions(
	ctx context.Context,
	reader client.Reader,
	cache *assume.Cache,
	providerKey string,
	provider types.NamespacedName,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
) ([]scheduler.PlacedVM, bool, error) {
	snap, err := refCommittedPlacements(ctx, reader, provider, vm)
	if err != nil {
		return nil, false, err
	}
	_, selfPresent := snap.recorded[vmSchedulingUID(vm)]
	placed := snap.placed
	for _, a := range cache.Observe(providerKey, snap.observe) {
		placed = append(placed, scheduler.PlacedVM{
			Name:         a.Name,
			UID:          a.UID,
			HostID:       a.HostID,
			Labels:       a.Labels,
			Resources:    a.Resources,
			CapacityOnly: a.Namespace != vm.Namespace,
		})
	}
	return placed, selfPresent, nil
}
