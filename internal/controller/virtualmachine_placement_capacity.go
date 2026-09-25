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
	"math/rand/v2"
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
	// placementLockWait bounds how long a reconcile waits for its Provider's
	// assume lock. The lock covers informer-cache reads and in-memory work
	// only (milliseconds), so this is only reached when something is badly
	// wrong; the reconcile then requeues instead of parking its worker.
	placementLockWait = 5 * time.Second
	// placementLockBusyRetryBase is the requeue after placementLockWait ran out;
	// up to one second of jitter is added so the waiters spread out.
	placementLockBusyRetryBase = time.Second
	// placementStatusWriteTimeout bounds each status write of the placement
	// path (made after the assume lock is released), so a slow API server
	// cannot hold a worker indefinitely.
	placementStatusWriteTimeout = 30 * time.Second
)

// placementLockBusyRetry is the jittered requeue of a reconcile that could not
// get its Provider's assume lock within placementLockWait.
func placementLockBusyRetry() time.Duration {
	return placementLockBusyRetryBase + rand.N(time.Second) // #nosec G404 -- jitter, not a secret
}

// updatePlacementStatus is updateStatus bounded by placementStatusWriteTimeout.
// It is only ever called with no assume lock held.
func (r *VirtualMachineReconciler) updatePlacementStatus(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) {
	writeCtx, cancel := context.WithTimeout(ctx, placementStatusWriteTimeout)
	defer cancel()
	r.updateStatus(writeCtx, vm)
}

// placementInfraError wraps an infrastructure failure (a cache read failed)
// met while scheduling, as opposed to a scheduler verdict: the reconcile
// returns it as an error (backoff) instead of reporting it on the VM.
type placementInfraError struct{ err error }

// Error implements error.
func (e *placementInfraError) Error() string { return e.err.Error() }

// Unwrap returns the underlying error.
func (e *placementInfraError) Unwrap() error { return e.err }

// scheduleAndAssume is the part of a clustered create that runs under the
// Provider's assume lock: read what is committed (informer cache plus live
// assumptions), schedule, and on success assume the pick so the next schedule
// of this Provider counts it even though the pendingHost write has not landed
// yet. It makes no API call. A cache failure is a *placementInfraError; any
// other error is the scheduler's verdict.
func (r *VirtualMachineReconciler) scheduleAndAssume(
	ctx context.Context,
	assumptions *assume.Cache,
	providerKey string,
	provider types.NamespacedName,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	req *scheduler.Request,
) (scheduler.Result, error) {
	if err := r.placementRequest(ctx, assumptions, providerKey, provider, vm, req); err != nil {
		return scheduler.Result{}, &placementInfraError{err: err}
	}
	result, err := scheduler.Schedule(*req)
	if err != nil {
		return result, err
	}
	assumptions.Assume(providerKey, assume.Assumption{
		UID:       vmSchedulingUID(vm),
		Namespace: vm.Namespace,
		Name:      vm.Name,
		HostID:    result.HostID,
		Labels:    vm.Labels, // Assume keeps its own copy
		Resources: req.Resources,
	})
	return result, nil
}

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

// The smallest footprint any VM is counted at: the minimums spec.resources
// accepts. A VM whose size cannot be read (no class it may use, nothing
// recorded) still holds at least this much.
const (
	minFootprintCPU       int32 = 1
	minFootprintMemoryMiB int64 = 128
)

// withMinimum raises r to the minimum footprint.
func withMinimum(r scheduler.ResourceRequest) scheduler.ResourceRequest {
	return scheduler.ResourceRequest{CPU: max(r.CPU, minFootprintCPU), MemoryMiB: max(r.MemoryMiB, minFootprintMemoryMiB)}
}

// pendingFootprint is the size a VM whose create is still pending counts at:
// effectiveResources — its VMClass with any spec.resources override applied,
// exactly what its Create sends — which the CRD keeps frozen while the create
// is pending (VirtualMachine XValidation). When that cannot be computed (no
// usable class, or an out-of-range override the Create will refuse), it counts
// at requestedFootprint instead, the conservative reading.
func pendingFootprint(vm *infravirtrigaudiov1beta1.VirtualMachine, class *infravirtrigaudiov1beta1.VMClass) scheduler.ResourceRequest {
	if class != nil {
		if cpu, mem, err := effectiveResources(vm, class); err == nil {
			return withMinimum(scheduler.ResourceRequest{CPU: cpu, MemoryMiB: int64(mem)})
		}
	}
	cpu, mem := classSize(class)
	return requestedFootprint(vm, cpu, mem)
}

// requestedFootprint is a VM's VMClass size (classCPU/classMemMiB) raised to
// any larger spec.resources override, at least the minimum footprint: the
// conservative size of a pending VM whose effective size cannot be computed
// (pendingFootprint).
func requestedFootprint(vm *infravirtrigaudiov1beta1.VirtualMachine, classCPU int32, classMemMiB int64) scheduler.ResourceRequest {
	out := scheduler.ResourceRequest{CPU: classCPU, MemoryMiB: classMemMiB}
	if r := vm.Spec.Resources; r != nil {
		if r.CPU != nil {
			out.CPU = max(out.CPU, *r.CPU)
		}
		if r.MemoryMiB != nil {
			out.MemoryMiB = max(out.MemoryMiB, *r.MemoryMiB)
		}
	}
	return withMinimum(out)
}

// classSize returns class's CPU and memory in MiB, or zeros for nil.
func classSize(class *infravirtrigaudiov1beta1.VMClass) (int32, int64) {
	if class == nil {
		return 0, 0
	}
	return class.Spec.CPU, class.Spec.Memory.Value() / bytesPerMiB
}

// pendingCreate reports whether vm's create is in flight and has never
// succeeded: status.placement.pendingHost set, status.id empty.
func pendingCreate(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	return pendingHost(vm) != "" && vm.Status.ID == ""
}

// admittedFootprint is what ANOTHER VM is counted at on its host — its
// admitted size, never a size its owner merely asks for (review M2):
//
//   - status.currentResources, per resource, when recorded: what the operator
//     recorded as applied by the provider (only the operator writes status);
//   - otherwise, for a VM whose create is pending, pendingFootprint — frozen
//     by the CRD while it is pending;
//   - otherwise (a bound VM with nothing recorded, e.g. bound before
//     currentResources existed), its VMClass size.
//
// spec.resources and spec.classRef of a created VM are ignored: its owner can
// change them at any time, and a clustered resize-up is admitted against
// committed capacity (admitClusteredResize) before the new size is applied and
// recorded. class is nil when the VM names none, it does not exist, or the VM's
// namespace may not use it; every VM counts at least the minimum footprint.
func admittedFootprint(vm *infravirtrigaudiov1beta1.VirtualMachine, class *infravirtrigaudiov1beta1.VMClass) scheduler.ResourceRequest {
	cpu, mem := classSize(class)
	base := withMinimum(scheduler.ResourceRequest{CPU: cpu, MemoryMiB: mem})
	if pendingCreate(vm) {
		base = pendingFootprint(vm, class)
	}
	if cur := vm.Status.CurrentResources; cur != nil {
		if cur.CPU != nil {
			base.CPU = *cur.CPU
		}
		if cur.MemoryMiB != nil {
			base.MemoryMiB = *cur.MemoryMiB
		}
	}
	return withMinimum(base)
}

// committedSnapshot is what one informer snapshot says about a clustered
// Provider's VirtualMachines.
type committedSnapshot struct {
	// placed are the VMs holding resources on the Provider's hosts, one entry
	// per (VM, host), the scheduled VM excluded.
	placed []scheduler.PlacedVM
	// recorded maps the scheduling UID of every VirtualMachine of the Provider
	// (the scheduled one included) to what its durable record says; it settles
	// assumptions (settled).
	recorded map[string]recordedVM
}

// recordedVM is what a VM's durable record says, as far as assumptions care.
type recordedVM struct {
	// hosts are the hosts its placement names (host / pendingHost).
	hosts []string
	// size is its recorded size (status.currentResources) and hasSize whether
	// both resources are recorded.
	size    scheduler.ResourceRequest
	hasSize bool
}

// settled reports whether assumption a is superseded by this snapshot: its VM
// is gone (or no longer holds a placement on this Provider); for a create, the
// VM's record names the assumed host; for an admitted resize, the VM left the
// host or its recorded size reached the admitted one. A record naming another
// host does not settle a create's assumption (e.g. a pendingHost the VM has
// since released).
func (s committedSnapshot) settled(a assume.Assumption) bool {
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

// committedPlacements lists every VirtualMachine, in every namespace, whose
// placement belongs to provider (placementProviderKey) and returns the
// resources each holds on the hosts its record names: a bound VM on its host, a
// VM with a create in flight on its pending host. A VM being deleted still
// counts until its finalizer is gone, because its domain and disks are still on
// the host. VMs in another namespace than self count toward capacity only: they
// are outside self's affinity scope (CapacityOnly), and nothing about them but
// their resources reaches the scheduler's no-fit message.
//
// Each VM counts at its admittedFootprint. Its VMClass is read (from the
// cache) only when that needs it, and only when the VM's namespace may use it
// (the consumer grant, as everywhere else).
func (r *VirtualMachineReconciler) committedPlacements(
	ctx context.Context,
	provider types.NamespacedName,
	self *infravirtrigaudiov1beta1.VirtualMachine,
) (committedSnapshot, error) {
	// Only this Provider's VMs, through the field index; read-only (shared
	// with the cache).
	vms, err := listProviderVMs(ctx, r.Client, provider)
	if err != nil {
		return committedSnapshot{}, err
	}
	snap := committedSnapshot{recorded: map[string]recordedVM{}}
	selfUID := vmSchedulingUID(self)
	classes := classMemo{}
	for i := range vms {
		other := &vms[i]
		// The key is re-checked (defense in depth: a VM counts against a
		// Provider only when its key is that Provider). A VM being deleted
		// whose finalizer is gone holds nothing any more; leaving it out of
		// recorded also settles any assumption of it.
		if placementProviderKey(other) != provider || !holdsPlacement(other) {
			continue
		}
		uid := vmSchedulingUID(other)
		hosts := placementHosts(other)
		snap.recorded[uid] = recordVM(other, hosts)
		if len(hosts) == 0 || uid == selfUID {
			continue
		}
		res, err := sizeForAccounting(ctx, r.Client, other, classes)
		if err != nil {
			return committedSnapshot{}, err
		}
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

// classMemoKey is one VMClass as seen from one consumer namespace: whether the
// namespace may use it depends on both.
type classMemoKey struct {
	class    types.NamespacedName
	consumer string
}

// classMemo memoises classForSizing over one accounting pass.
type classMemo map[classMemoKey]*infravirtrigaudiov1beta1.VMClass

// sizeForAccounting returns vm's admittedFootprint, reading its VMClass only
// when the footprint needs it (nothing recorded in status.currentResources).
func sizeForAccounting(ctx context.Context, reader client.Reader, vm *infravirtrigaudiov1beta1.VirtualMachine, seen classMemo) (scheduler.ResourceRequest, error) {
	if cur := vm.Status.CurrentResources; cur != nil && cur.CPU != nil && cur.MemoryMiB != nil {
		return admittedFootprint(vm, nil), nil
	}
	class, err := classForSizing(ctx, reader, vm, seen)
	if err != nil {
		return scheduler.ResourceRequest{}, err
	}
	return admittedFootprint(vm, class), nil
}

// classForSizing returns vm's VMClass for sizing, read through reader and
// memoised in seen. It is nil when the VM names none, the class does not
// exist, or the VM's namespace may not use it: a class in another namespace
// that does not select the VM's (spec.consumerNamespaceSelector) is refused
// here exactly as for a create, so a tenant cannot size its VM from a class it
// was never granted.
func classForSizing(
	ctx context.Context,
	reader client.Reader,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	seen classMemo,
) (*infravirtrigaudiov1beta1.VMClass, error) {
	key, ok := vmClassKey(vm)
	if !ok {
		return nil, nil
	}
	memoKey := classMemoKey{class: key, consumer: vm.Namespace}
	if c, done := seen[memoKey]; done {
		return c, nil
	}
	class := &infravirtrigaudiov1beta1.VMClass{}
	if err := getForConsumer(ctx, reader, key, class, vm.Namespace); err != nil {
		switch {
		case isConsumerNotAllowed(err):
			log.FromContext(ctx).V(1).Info("VMClass of a placed VM may not be used from its namespace; counting it at the minimum footprint",
				"class", key.String(), "vm", client.ObjectKeyFromObject(vm).String())
		case apierrors.IsNotFound(err):
			log.FromContext(ctx).V(1).Info("VMClass of a placed VM not found; counting it at the minimum footprint",
				"class", key.String(), "vm", client.ObjectKeyFromObject(vm).String())
		default:
			return nil, fmt.Errorf("get VMClass %s to size VirtualMachine %s/%s: %w", key, vm.Namespace, vm.Name, err)
		}
		class = nil
	}
	seen[memoKey] = class
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
	vms, err := listProviderVMs(ctx, reader, provider)
	if err != nil {
		return 0, 0, err
	}
	classes := classMemo{}
	for i := range vms {
		vm := &vms[i]
		if placementProviderKey(vm) != provider || !slices.Contains(placementHosts(vm), host) {
			continue
		}
		fp, err := sizeForAccounting(ctx, reader, vm, classes)
		if err != nil {
			return 0, 0, err
		}
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
	placed, err := r.placedWithAssumptions(ctx, cache, providerKey, provider, vm)
	if err != nil {
		return err
	}
	req.PlacedVMs = placed
	req.VMUID = vmSchedulingUID(vm)
	return nil
}

// placedWithAssumptions returns what is committed on provider's hosts, as seen
// from vm: the placements in the informer snapshot plus the live assumptions
// (create and resize), after settling those the snapshot supersedes
// (committedSnapshot.settled). Call it with provider's assume lock held.
func (r *VirtualMachineReconciler) placedWithAssumptions(
	ctx context.Context,
	cache *assume.Cache,
	providerKey string,
	provider types.NamespacedName,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
) ([]scheduler.PlacedVM, error) {
	snap, err := r.committedPlacements(ctx, provider, vm)
	if err != nil {
		return nil, err
	}
	placed := snap.placed
	for _, a := range cache.List(providerKey, snap.settled) {
		placed = append(placed, scheduler.PlacedVM{
			Name:         a.Name,
			UID:          a.UID,
			HostID:       a.HostID,
			Labels:       a.Labels,
			Resources:    a.Resources,
			CapacityOnly: a.Namespace != vm.Namespace,
		})
	}
	return placed, nil
}

// recordVM is what vm's durable record says for settling assumptions: the
// hosts its placement names and its recorded size.
func recordVM(vm *infravirtrigaudiov1beta1.VirtualMachine, hosts []string) recordedVM {
	rec := recordedVM{hosts: hosts}
	if cur := vm.Status.CurrentResources; cur != nil && cur.CPU != nil && cur.MemoryMiB != nil {
		rec.size = scheduler.ResourceRequest{CPU: *cur.CPU, MemoryMiB: *cur.MemoryMiB}
		rec.hasSize = true
	}
	return rec
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
