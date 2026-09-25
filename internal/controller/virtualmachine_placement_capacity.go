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
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// This file holds the clustered scheduler's accuracy inputs (ADR-0007 Addendum
// A, scheduler-accuracy amendment): the resources committed to each host by
// the Provider's VirtualMachines, the in-process assume cache that covers the
// window before a placement's record is visible in the informer cache, and the
// backoff of a VM no host can take. Only the clustered create path reaches it;
// a single-host or thin-client Provider never schedules (D9).

const (
	// pendingHostWriteTimeout bounds the checked status update that records
	// status.placement.pendingHost before Create (A2).
	pendingHostWriteTimeout = time.Minute
	// placementAssumeTTL is how long an assumption lives at most: twice the
	// bound of the pendingHost write it covers, so it outlasts that write and
	// the informer catching up with it. It is a safety net only; on every
	// normal path the assumption ends earlier (the record appears in the
	// cache, or the write fails and the assumption is forgotten).
	placementAssumeTTL = 2 * pendingHostWriteTimeout
	// placementUnschedulableMaxRetryInterval caps the backoff of a VM that no
	// host can take. It starts at placementUnschedulableRetryInterval and
	// doubles with each consecutive no-fit. The VM controller does not watch
	// Hosts or other VMs, so this is also the longest a VM waits to notice
	// freed capacity.
	placementUnschedulableMaxRetryInterval = 2 * time.Minute
	// placementUnschedulableForget is how long after its last no-fit a VM's
	// backoff record is kept.
	placementUnschedulableForget = time.Hour
)

// placementAssumptions returns the reconciler's assume cache, creating it on
// first use: a manager whose Providers are all single-host never creates it.
func (r *VirtualMachineReconciler) placementAssumptions() *assume.Cache {
	if c := r.placements.Load(); c != nil {
		return c
	}
	r.placements.CompareAndSwap(nil, assume.New(placementAssumeTTL, r.now))
	return r.placements.Load()
}

// forgetPlacement drops vm's assumption and backoff record, if any. It never
// creates the assume cache.
func (r *VirtualMachineReconciler) forgetPlacement(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	uid := vmSchedulingUID(vm)
	if c := r.placements.Load(); c != nil {
		c.Forget(uid)
	}
	r.unschedulable.reset(uid)
}

// vmSchedulingUID is the identity the scheduler and the assume cache key a VM
// on: its Kubernetes UID, or namespace/name for an object that has none (only
// objects built by tests; the API server always sets a UID).
func vmSchedulingUID(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	if vm.UID != "" {
		return string(vm.UID)
	}
	return vm.Namespace + "/" + vm.Name
}

// holdsPlacement reports whether vm can still hold resources on a host: every
// VM except one being deleted whose VirtualMachine finalizer is already gone.
// Such a VM's hypervisor VM has been deleted (or orphaned) and only another
// controller's finalizer keeps the object; that foreign finalizer must neither
// keep capacity committed nor block a Host's decommissioning.
func holdsPlacement(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return vm.DeletionTimestamp.IsZero() || k8s.HasFinalizer(vm, infravirtrigaudiov1beta1.VirtualMachineFinalizer)
}

// placementHosts returns the hosts vm holds resources on: its confirmed
// binding and its pending host, the same host listed once. It is empty for a
// VM that no longer holds a placement (holdsPlacement).
func placementHosts(vm *infravirtrigaudiov1beta1.VirtualMachine) []string {
	if !holdsPlacement(vm) {
		return nil
	}
	var hosts []string
	if h := boundHost(vm); h != "" {
		hosts = append(hosts, h)
	}
	if h := pendingHost(vm); h != "" && !slices.Contains(hosts, h) {
		hosts = append(hosts, h)
	}
	return hosts
}

// footprint is the CPU/memory a VM holds or will hold on its host: the largest
// of its size as created (baseCPU/baseMemMiB, from its VMClass), its
// spec.resources overrides, and status.currentResources (what the operator last
// recorded as applied). Taking the largest counts a resize in progress at its
// larger size, which is the safe side for placement.
func footprint(vm *infravirtrigaudiov1beta1.VirtualMachine, baseCPU int32, baseMemMiB int64) scheduler.ResourceRequest {
	out := scheduler.ResourceRequest{CPU: baseCPU, MemoryMiB: baseMemMiB}
	for _, r := range []*infravirtrigaudiov1beta1.VirtualMachineResources{vm.Spec.Resources, vm.Status.CurrentResources} {
		if r == nil {
			continue
		}
		if r.CPU != nil {
			out.CPU = max(out.CPU, *r.CPU)
		}
		if r.MemoryMiB != nil {
			out.MemoryMiB = max(out.MemoryMiB, *r.MemoryMiB)
		}
	}
	return out
}

// classFootprint is footprint with the base size taken from class (nil: no
// class could be read, so only the overrides and current resources count).
func classFootprint(vm *infravirtrigaudiov1beta1.VirtualMachine, class *infravirtrigaudiov1beta1.VMClass) scheduler.ResourceRequest {
	var (
		cpu int32
		mem int64
	)
	if class != nil {
		cpu = class.Spec.CPU
		mem = class.Spec.Memory.Value() / bytesPerMiB
	}
	return footprint(vm, cpu, mem)
}

// committedSnapshot is what one informer snapshot says about a clustered
// Provider's VirtualMachines.
type committedSnapshot struct {
	// placed are the VMs holding resources on the Provider's hosts, one entry
	// per (VM, host), the scheduled VM excluded.
	placed []scheduler.PlacedVM
	// recorded maps the scheduling UID of every VirtualMachine of the Provider
	// (the scheduled one included) to the hosts its durable record names
	// (placement.host / placement.pendingHost; empty when it names none).
	recorded map[string][]string
}

// committedPlacements lists every VirtualMachine, in every namespace, whose
// placement belongs to provider (placementProviderKey) and returns the
// resources each holds on the hosts its record names: a bound VM on its host, a
// VM with a create in flight on its pending host. A VM being deleted still
// counts until its finalizer is gone, because its domain and disks are still on
// the host. VMs in another namespace than self count toward capacity only: they
// are outside self's affinity scope (CapacityOnly), and nothing about them but
// their resources reaches the scheduler's no-fit message.
//
// A VM's VMClass is read (from the cache) only to size it; the consumer grant
// is not checked, because nothing is created from the class here. A class that
// no longer exists sizes the VM from its overrides and current resources only.
func (r *VirtualMachineReconciler) committedPlacements(
	ctx context.Context,
	provider types.NamespacedName,
	self *infravirtrigaudiov1beta1.VirtualMachine,
) (committedSnapshot, error) {
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	if err := r.List(ctx, &vms); err != nil {
		return committedSnapshot{}, fmt.Errorf("list VirtualMachines for Provider %s: %w", provider, err)
	}
	snap := committedSnapshot{recorded: map[string][]string{}}
	selfUID := vmSchedulingUID(self)
	classes := map[types.NamespacedName]*infravirtrigaudiov1beta1.VMClass{}
	for i := range vms.Items {
		other := &vms.Items[i]
		// A VM being deleted whose finalizer is gone holds nothing any more;
		// leaving it out of recorded also settles any assumption of it.
		if placementProviderKey(other) != provider || !holdsPlacement(other) {
			continue
		}
		uid := vmSchedulingUID(other)
		hosts := placementHosts(other)
		snap.recorded[uid] = hosts
		if len(hosts) == 0 || uid == selfUID {
			continue
		}
		class, err := classForSizing(ctx, r.Client, other, classes)
		if err != nil {
			return committedSnapshot{}, err
		}
		res := classFootprint(other, class)
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

// classForSizing returns vm's VMClass for sizing, read through reader and
// memoised in seen; nil when the VM names none or it does not exist.
func classForSizing(
	ctx context.Context,
	reader client.Reader,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	seen map[types.NamespacedName]*infravirtrigaudiov1beta1.VMClass,
) (*infravirtrigaudiov1beta1.VMClass, error) {
	key, ok := vmClassKey(vm)
	if !ok {
		return nil, nil
	}
	if c, done := seen[key]; done {
		return c, nil
	}
	class := &infravirtrigaudiov1beta1.VMClass{}
	if err := reader.Get(ctx, key, class); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get VMClass %s to size VirtualMachine %s/%s: %w", key, vm.Namespace, vm.Name, err)
		}
		log.FromContext(ctx).V(1).Info("VMClass of a placed VM not found; sizing it from its overrides and current resources only",
			"class", key.String(), "vm", client.ObjectKeyFromObject(vm).String())
		class = nil
	}
	seen[key] = class
	return class, nil
}

// committedOnHost sums the footprint of every VirtualMachine whose placement
// belongs to provider and names host (bound or pending; being deleted
// included) — the same accounting the scheduler uses, for one host.
func committedOnHost(
	ctx context.Context,
	reader client.Reader,
	provider types.NamespacedName,
	host string,
) (cpu, memMiB int64, err error) {
	var vms infravirtrigaudiov1beta1.VirtualMachineList
	if err := reader.List(ctx, &vms); err != nil {
		return 0, 0, fmt.Errorf("list VirtualMachines for Host %s/%s: %w", provider.Namespace, host, err)
	}
	classes := map[types.NamespacedName]*infravirtrigaudiov1beta1.VMClass{}
	for i := range vms.Items {
		vm := &vms.Items[i]
		if placementProviderKey(vm) != provider || !slices.Contains(placementHosts(vm), host) {
			continue
		}
		class, err := classForSizing(ctx, reader, vm, classes)
		if err != nil {
			return 0, 0, err
		}
		fp := classFootprint(vm, class)
		cpu += max(int64(fp.CPU), 0)
		memMiB += max(fp.MemoryMiB, 0)
	}
	return cpu, memMiB, nil
}

// placementRequest completes req with what is committed on provider's hosts:
// the placements in the informer snapshot plus the live assumptions. Call it
// with provider's assume lock held. An assumption is settled — dropped, the
// durable record counting from now on — when the snapshot no longer has its VM
// or shows the VM's record on the assumed host; a record on another host (for
// example a stale pendingHost the VM has since released) does not settle it.
func (r *VirtualMachineReconciler) placementRequest(
	ctx context.Context,
	cache *assume.Cache,
	providerKey string,
	provider types.NamespacedName,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	req *scheduler.Request,
) error {
	snap, err := r.committedPlacements(ctx, provider, vm)
	if err != nil {
		return err
	}
	assumed := cache.List(providerKey, func(a assume.Assumption) bool {
		hosts, present := snap.recorded[a.UID]
		return !present || slices.Contains(hosts, a.HostID)
	})
	req.PlacedVMs = snap.placed
	for _, a := range assumed {
		req.PlacedVMs = append(req.PlacedVMs, scheduler.PlacedVM{
			Name:         a.Name,
			UID:          a.UID,
			HostID:       a.HostID,
			Labels:       a.Labels,
			Resources:    a.Resources,
			CapacityOnly: a.Namespace != vm.Namespace,
		})
	}
	req.VMUID = vmSchedulingUID(vm)
	return nil
}

// unschedulableBackoff paces the re-scheduling of VMs no host can take: the
// first no-fit waits placementUnschedulableRetryInterval, each consecutive one
// twice as long, up to placementUnschedulableMaxRetryInterval. It is in memory
// and per VM; a successful schedule or the VM's deletion resets it, and a
// record idle for placementUnschedulableForget is dropped. The zero value is
// ready to use and it is safe for concurrent use.
type unschedulableBackoff struct {
	mu      sync.Mutex
	entries map[string]*unschedulableEntry
}

// unschedulableEntry is one VM's run of consecutive no-fits.
type unschedulableEntry struct {
	failures int
	last     time.Time
}

// next records a no-fit of the VM uid at now and returns how long to wait
// before scheduling it again.
func (b *unschedulableBackoff) next(uid string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]*unschedulableEntry{}
	}
	for k, e := range b.entries {
		if now.Sub(e.last) > placementUnschedulableForget {
			delete(b.entries, k)
		}
	}
	e, ok := b.entries[uid]
	if !ok {
		e = &unschedulableEntry{}
		b.entries[uid] = e
	}
	e.failures++
	e.last = now
	d := placementUnschedulableRetryInterval
	for i := 1; i < e.failures && d < placementUnschedulableMaxRetryInterval; i++ {
		d *= 2
	}
	return min(d, placementUnschedulableMaxRetryInterval)
}

// reset forgets the VM uid's no-fits.
func (b *unschedulableBackoff) reset(uid string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, uid)
}
