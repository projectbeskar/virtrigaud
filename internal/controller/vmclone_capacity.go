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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// The capacity check of a CLUSTERED clone (ADR-0007 Addendum A, slice 3 +
// the scheduler-accuracy amendment; William's decision). A clone lands on its
// source VM's host and nowhere else, so it is not scheduled: it is admitted
// against that host's free capacity — what every VirtualMachine of the
// Provider, in any namespace, is committed to it, plus the placements other
// reconciles have chosen but not yet recorded — under the same per-Provider
// assume lock as the VirtualMachine controller's scheduling and resize gate.
// The lock covers informer-cache reads and in-memory work only. An admitted
// clone is assumed on the host until its target's pendingHost write is
// visible, so a concurrent create or clone of the same Provider counts it.
//
// A clone that does not fit sets Placed=False/Unschedulable on its target
// VirtualMachine (with only the clone's own size and host: no committed,
// capacity or free figure, which are derived from other tenants' VMs) and
// waits Pending, re-checked with a backoff. It never falls back to another
// host.

// cloneSizeUnknownError reports that a clustered clone's size cannot be
// determined (its VMClass is missing or may not be used from the target's
// namespace): the clone waits instead of being admitted blind.
type cloneSizeUnknownError struct{ reason string }

// Error implements error.
func (e *cloneSizeUnknownError) Error() string { return e.reason }

// PlacementAssumptions returns the VirtualMachine controller's per-Provider
// assume cache (creating it), so that another controller placing VMs on the
// same clustered Providers — the VMClone controller's clustered clone — takes
// the same per-Provider lock and counts the same assumptions. The manager
// passes it to the VMClone reconciler (VMCloneReconciler.Placements).
func (r *VirtualMachineReconciler) PlacementAssumptions() *assume.Cache {
	return r.placementAssumptions()
}

// placementAssumptions returns the per-Provider assume cache the clone
// admission shares with the VirtualMachine controller (Placements, wired by
// the manager). Without one — a reconciler built directly, as in unit tests —
// it uses a cache of its own.
func (r *VMCloneReconciler) placementAssumptions() *assume.Cache {
	if r.Placements != nil {
		return r.Placements
	}
	if c := r.ownPlacements.Load(); c != nil {
		return c
	}
	r.ownPlacements.CompareAndSwap(nil, assume.New(placementAssumeTTL, time.Now))
	return r.ownPlacements.Load()
}

// cloneShape is what a clustered clone will be made with: its size and its
// hot-add headroom. The Clone RPC sends exactly this (cloneClassJSON), so what
// is admitted is what is created.
type cloneShape struct {
	// size is the clone's vCPU and memory.
	size scheduler.ResourceRequest
	// memHotAdd provisions balloon headroom (a memory ceiling, counted against
	// the host); cpuHotAdd provisions vCPU headroom (not counted).
	memHotAdd, cpuHotAdd bool
}

// cloneShapeFor is the shape the clone's VM will have:
//
//   - with spec.target.classRef: that VMClass's size and hot-add settings;
//   - without: the source VM's own size — its status.currentResources when
//     recorded (what the provider applied to it), otherwise its VMClass with
//     any spec.resources override applied (effectiveResources) — with memory
//     headroom when the source was provisioned with a balloon ceiling (its
//     recorded status.placement.memoryCeilingMiB; the class's setting only
//     when that is unrecorded), and the class's vCPU headroom.
//
// The VMClass is read for the namespace that uses it (the consumer grant). A
// class that cannot be read, or a size that cannot be computed, is a
// cloneSizeUnknownError: the clone waits rather than being admitted — or sent
// — blind.
func (r *VMCloneReconciler) cloneShapeFor(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	target *infrav1beta1.VirtualMachine,
) (cloneShape, error) {
	sized := sourceVM
	if clone.Spec.Target.ClassRef != nil && clone.Spec.Target.ClassRef.Name != "" {
		sized = target
	}
	key, ok := vmClassKey(sized)
	if !ok {
		return cloneShape{}, &cloneSizeUnknownError{reason: fmt.Sprintf(
			"the clone's size cannot be determined: VirtualMachine %s/%s names no VMClass", sized.Namespace, sized.Name)}
	}
	class := &infrav1beta1.VMClass{}
	if err := getForConsumer(ctx, r.Client, key, class, sized.Namespace); err != nil {
		if apierrors.IsNotFound(err) || isConsumerNotAllowed(err) {
			return cloneShape{}, &cloneSizeUnknownError{reason: fmt.Sprintf(
				"the clone's size cannot be determined: VMClass %s is not available to namespace %s", key.Name, sized.Namespace)}
		}
		return cloneShape{}, fmt.Errorf("get VMClass %s to size the clone: %w", key, err)
	}
	shape := cloneShape{memHotAdd: memoryHotAdd(class),
		cpuHotAdd: class.Spec.PerformanceProfile != nil && class.Spec.PerformanceProfile.CPUHotAddEnabled}
	if sized == sourceVM {
		if pl := sourceVM.Status.Placement; pl != nil && pl.MemoryCeilingMiB != nil {
			shape.memHotAdd = *pl.MemoryCeilingMiB > 0
		}
		if hasRecordedSize(sourceVM) {
			cur := sourceVM.Status.CurrentResources
			shape.size = withMinimum(scheduler.ResourceRequest{CPU: *cur.CPU, MemoryMiB: *cur.MemoryMiB})
			return shape, nil
		}
	}
	cpu, mem, err := effectiveResources(sized, class)
	if err != nil {
		return cloneShape{}, &cloneSizeUnknownError{reason: fmt.Sprintf("the clone's size cannot be determined: %v", err)}
	}
	shape.size = withMinimum(scheduler.ResourceRequest{CPU: cpu, MemoryMiB: int64(mem)})
	return shape, nil
}

// hostPoolOf returns the HostPool of host when it belongs to provider. ok is
// false when the pool is missing or belongs to another Provider: the host's
// capacity (scaled by the pool's overcommit ratios) is then unknown, and the
// clone is refused rather than admitted against a guessed ratio (as a resize
// is, scheduler review N6).
func (r *VMCloneReconciler) hostPoolOf(ctx context.Context, provider *infrav1beta1.Provider, host *infrav1beta1.Host) (spec infrav1beta1.HostPoolSpec, ok bool, err error) {
	pool := &infrav1beta1.HostPool{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: host.Spec.PoolRef.Name}, pool); {
	case err == nil && pool.Spec.ProviderRef.Name == provider.Name:
		return pool.Spec, true, nil
	case err == nil, apierrors.IsNotFound(err):
		return infrav1beta1.HostPoolSpec{}, false, nil
	default:
		return infrav1beta1.HostPoolSpec{}, false, fmt.Errorf("get HostPool %s/%s to admit a clone: %w", provider.Namespace, host.Spec.PoolRef.Name, err)
	}
}

// clonePlacementAdmission is the size a clustered clone was admitted at and
// is made at: recorded with its target's pending host
// (status.placement.pendingResources and memoryCeilingMiB), sent in the Clone
// RPC (cloneClassJSON), and recorded as the target's size at bind.
type clonePlacementAdmission struct {
	// size is the clone's vCPU and memory.
	size scheduler.ResourceRequest
	// ceiling is the balloon ceiling the clone is provisioned with (0: none):
	// contracts.HotplugCeilingMemoryMiB of its memory with memory hot-add, as
	// for a create.
	ceiling int64
	// cpuHotAdd provisions vCPU headroom. It is not recorded (vCPU headroom is
	// not counted against the host) and is read from the clone's VMClass on
	// every attempt.
	cpuHotAdd bool
}

// admissionFor is the admission of shape: its size, and the balloon ceiling
// its memory hot-add setting provisions.
func admissionFor(shape cloneShape) clonePlacementAdmission {
	return clonePlacementAdmission{size: shape.size, ceiling: memoryCeilingFor(shape.memHotAdd, shape.size.MemoryMiB),
		cpuHotAdd: shape.cpuHotAdd}
}

// recordedAdmission is the admission recorded with a pending target's host
// (ok false when no admitted size is recorded), with cpuHotAdd, which is not.
func recordedAdmission(target *infrav1beta1.VirtualMachine, cpuHotAdd bool) (clonePlacementAdmission, bool) {
	pl := target.Status.Placement
	if pl == nil || pl.PendingResources == nil {
		return clonePlacementAdmission{}, false
	}
	adm := clonePlacementAdmission{
		size:      scheduler.ResourceRequest{CPU: pl.PendingResources.CPU, MemoryMiB: pl.PendingResources.MemoryMiB},
		cpuHotAdd: cpuHotAdd,
	}
	if pl.MemoryCeilingMiB != nil {
		adm.ceiling = *pl.MemoryCeilingMiB
	}
	return adm, true
}

// footprint is what the admitted clone counts at on its host — exactly as the
// accounting counts its target once the pending record shows it
// (admittedFootprint: the recorded size, raised to the recorded balloon
// ceiling).
func (a clonePlacementAdmission) footprint(target *infrav1beta1.VirtualMachine, host string) scheduler.ResourceRequest {
	pending := target.DeepCopy()
	pending.Status.ID = ""
	pending.Status.CurrentResources = nil
	if pending.Status.Placement == nil {
		pending.Status.Placement = &infrav1beta1.PlacementStatus{}
	}
	pending.Status.Placement.PendingHost = host
	recordClonePendingSize(pending.Status.Placement, a)
	return admittedFootprint(pending, nil)
}

// cloneClassJSON is the class override the Clone RPC carries: exactly the
// admitted size and headroom (a VMClassSpec with CPU, memory and the hot-add
// flags; the provider applies nothing else from it). So a clone is created at
// the size it was admitted at, whatever its VMClass says by then.
func (a clonePlacementAdmission) cloneClassJSON() (string, error) {
	spec := infrav1beta1.VMClassSpec{
		CPU:    a.size.CPU,
		Memory: *resource.NewQuantity(a.size.MemoryMiB*bytesPerMiB, resource.BinarySI),
		PerformanceProfile: &infrav1beta1.PerformanceProfile{
			CPUHotAddEnabled:    a.cpuHotAdd,
			MemoryHotAddEnabled: a.ceiling > 0,
		},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode the clone's admitted size: %w", err)
	}
	return string(data), nil
}

// admitClusteredClone admits the clone of sourceVM onto host — the source's
// own host — against that host's free capacity, before the target's
// pendingHost is recorded and before the Clone RPC. It returns admitted ==
// true with the admitted size, having assumed it on host under the Provider's
// assume lock; otherwise it has recorded why on the target VM and the clone
// and returns the requeue. A target that already records host as its pending
// host holds the capacity durably (an earlier attempt admitted it): it is
// admitted again without a check.
func (r *VMCloneReconciler) admitClusteredClone(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	target *infrav1beta1.VirtualMachine,
	provider *infrav1beta1.Provider,
	sourceVM *infrav1beta1.VirtualMachine,
	host string,
) (clonePlacementAdmission, ctrl.Result, bool, error) {
	logger := logging.FromContext(ctx)
	shape, err := r.cloneShapeFor(ctx, clone, sourceVM, target)
	var unknown *cloneSizeUnknownError
	switch {
	case stderrors.As(err, &unknown):
		return clonePlacementAdmission{}, r.waitForCloneHost(ctx, clone, k8s.ReasonPlacementError, unknown.reason, placementConfigRetryInterval), false, nil
	case err != nil:
		return clonePlacementAdmission{}, ctrl.Result{}, false, err
	}
	adm := admissionFor(shape)
	if pl := target.Status.Placement; pl != nil && strings.TrimSpace(pl.PendingHost) == host {
		// Admitted by an earlier attempt: the size is FROZEN at what was
		// admitted and recorded (as a pending create's is, #356). The Clone
		// RPC sends exactly that size (cloneClassJSON), so a VMClass edited
		// since cannot change what is created or make it outgrow what was
		// admitted; once bound, the target converges to its class through
		// the resize gate like any VM.
		if recorded, ok := recordedAdmission(target, shape.cpuHotAdd); ok {
			if recorded.size != adm.size || recorded.ceiling != adm.ceiling {
				logger.Info("The clone's VMClass changed after the clone was admitted; the clone is made at its admitted size",
					"admittedCPU", recorded.size.CPU, "admittedMemoryMiB", recorded.size.MemoryMiB,
					"classCPU", adm.size.CPU, "classMemoryMiB", adm.size.MemoryMiB)
			}
			return recorded, ctrl.Result{}, true, nil
		}
		// A pending host without a recorded size: admit it now (its own
		// records are excluded from the check) and record the size.
	}
	want := adm.footprint(target, host)

	// The host and its pool, from the cache, before the lock.
	h := &infrav1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: host}, h); err != nil {
		if apierrors.IsNotFound(err) {
			return clonePlacementAdmission{}, r.waitForCloneHost(ctx, clone, cloneReasonSourceHostGone, fmt.Sprintf(
				"the source VM's host %s no longer exists; a clone lands only on its source's host", host), cloneHostBlockedRetryInterval), false, nil
		}
		return clonePlacementAdmission{}, ctrl.Result{}, false, fmt.Errorf("get Host %s/%s to admit a clone: %w", provider.Namespace, host, err)
	}
	poolSpec, poolOK, err := r.hostPoolOf(ctx, provider, h)
	switch {
	case err != nil:
		return clonePlacementAdmission{}, ctrl.Result{}, false, err
	case !poolOK:
		msg := fmt.Sprintf("the clone cannot be admitted: the HostPool %q of its source VM's host %s does not exist or belongs to another "+
			"Provider, so the host's capacity is unknown", h.Spec.PoolRef.Name, host)
		return clonePlacementAdmission{}, r.refuseClonePlacement(ctx, clone, target, k8s.ReasonPlacementError, msg, placementConfigRetryInterval), false, nil
	}

	uid := vmSchedulingUID(target)
	providerNN := types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}
	providerKey := providerNN.String()
	assumptions := r.placementAssumptions()
	checkErr, locked, err := r.checkCloneFitUnderLock(ctx, assumptions, providerKey, providerNN, target, h, poolSpec, want, host)
	if !locked {
		logger.V(1).Info("Provider's placement lock is busy; requeueing the clone", "provider", providerKey)
		return clonePlacementAdmission{}, ctrl.Result{RequeueAfter: placementLockBusyRetry()}, false, nil
	}
	if err != nil {
		return clonePlacementAdmission{}, ctrl.Result{}, false, err
	}

	var tooBig *scheduler.ResizeDoesNotFitError
	switch {
	case checkErr == nil:
		r.unschedulable.reset(uid)
		logger.Info("Admitted the clone against its source host's free capacity", "host", host,
			"cpu", want.CPU, "memoryMiB", want.MemoryMiB)
		return adm, ctrl.Result{}, true, nil
	case stderrors.As(checkErr, &tooBig):
		retryAfter := r.unschedulable.next(uid, time.Now())
		logger.V(1).Info("Committed-capacity arithmetic of a refused clone (administrator detail)",
			"host", host, "detail", tooBig.Detail())
		// Only the clone's own size and host (review M3).
		msg := fmt.Sprintf("the clone (%d vCPU, %d MiB) does not fit in the free capacity of its source VM's host %s, "+
			"the only host it can land on; it is re-checked (next check in %s)", want.CPU, want.MemoryMiB, host, retryAfter)
		return clonePlacementAdmission{}, r.refuseClonePlacement(ctx, clone, target, k8s.ReasonUnschedulable, msg, retryAfter), false, nil
	default:
		msg := fmt.Sprintf("the clone cannot be checked against its source VM's host %s: %v", host, checkErr)
		return clonePlacementAdmission{}, r.refuseClonePlacement(ctx, clone, target, k8s.ReasonPlacementError, msg, placementConfigRetryInterval), false, nil
	}
}

// checkCloneFitUnderLock checks, holding provider's assume lock, whether want
// fits in the free capacity of host h (the target's own records excluded),
// and on success assumes it there for the target. The lock covers
// informer-cache reads and in-memory work only, and is released by a deferred
// call. locked is false, having done nothing, when the lock was not free
// within placementLockWait; err is a failed cache read; checkErr is the
// verdict.
func (r *VMCloneReconciler) checkCloneFitUnderLock(
	ctx context.Context,
	assumptions *assume.Cache,
	providerKey string,
	provider types.NamespacedName,
	target *infrav1beta1.VirtualMachine,
	h *infrav1beta1.Host,
	pool infrav1beta1.HostPoolSpec,
	want scheduler.ResourceRequest,
	host string,
) (checkErr error, locked bool, err error) {
	unlock, locked := assumptions.LockWithin(ctx, providerKey, placementLockWait)
	if !locked {
		return nil, false, nil
	}
	defer unlock()
	lockCtx, cancel := lockBoundContext(ctx)
	defer cancel()
	placed, err := placedWithAssumptionsFrom(lockCtx, r.Client, assumptions, providerKey, provider, target)
	if err != nil {
		return nil, true, err
	}
	uid := vmSchedulingUID(target)
	checkErr = scheduler.CheckResize(scheduler.ResizeRequest{
		Host: *h, Pool: pool, VMUID: uid, Desired: want, PlacedVMs: placed,
	})
	if checkErr == nil {
		assumptions.Assume(providerKey, assume.Assumption{
			UID: uid, Namespace: target.Namespace, Name: target.Name, HostID: host,
			Labels: target.Labels, Resources: want,
		})
	}
	return checkErr, true, nil
}

// forgetClonePlacement drops the target's assumption: its pendingHost write
// failed, so nothing holds the capacity.
func (r *VMCloneReconciler) forgetClonePlacement(target *infrav1beta1.VirtualMachine) {
	r.placementAssumptions().Forget(vmSchedulingUID(target))
}

// refuseClonePlacement records a clone that cannot be admitted: Placed=False
// with reason and msg on the target VM (bounded, after the lock is released)
// and the clone Pending with the same message, re-checked after retryAfter.
func (r *VMCloneReconciler) refuseClonePlacement(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	target *infrav1beta1.VirtualMachine,
	reason, msg string,
	retryAfter time.Duration,
) ctrl.Result {
	setPlacedCondition(target, metav1.ConditionFalse, reason, msg)
	writeCtx, cancel := context.WithTimeout(ctx, placementStatusWriteTimeout)
	defer cancel()
	if err := r.Status().Update(writeCtx, target); err != nil && !apierrors.IsConflict(err) {
		logging.FromContext(ctx).Error(err, "Failed to record the refused clone on its target VM", "vm", target.Name)
	}
	return r.waitForCloneHost(ctx, clone, reason, msg, retryAfter)
}

// recordClonePendingSize records, with the target's pending host, the size the
// clone was admitted at (status.placement.pendingResources: while the clone is
// pending the target counts at it) and the balloon ceiling it is provisioned
// with (status.placement.memoryCeilingMiB, 0 for none; for a clone of its
// source's size this is the source's own ceiling).
func recordClonePendingSize(pl *infrav1beta1.PlacementStatus, adm clonePlacementAdmission) {
	pl.PendingResources = &infrav1beta1.PlacementResources{CPU: adm.size.CPU, MemoryMiB: adm.size.MemoryMiB}
	ceiling := adm.ceiling
	pl.MemoryCeilingMiB = &ceiling
}

// clonedSize is what a bound clustered clone's target is recorded at: the
// size the Clone RPC sent (the admitted size) and its balloon ceiling.
type clonedSize struct {
	size    scheduler.ResourceRequest
	ceiling int64
}

// clonedSizeFor returns what the clone was made at: the admission recorded
// with its target's pending host, which the Clone RPC sent as the clone's
// size (cloneClassJSON). It returns nil when none is recorded (the bind then
// records no size, and the target counts as before).
func clonedSizeFor(target *infrav1beta1.VirtualMachine) *clonedSize {
	adm, ok := recordedAdmission(target, false)
	if !ok {
		return nil
	}
	return &clonedSize{size: adm.size, ceiling: adm.ceiling}
}

// recordOn writes the cloned size into a bound clone's target vm, in the bind's
// status write: status.currentResources (the size the clone was made at), its
// balloon ceiling (status.placement.memoryCeilingMiB, kept once bound), and no
// pending size any more (the VM is bound). A nil c records no size but still
// clears the pending size.
func (c *clonedSize) recordOn(vm *infrav1beta1.VirtualMachine) {
	if vm.Status.Placement != nil {
		vm.Status.Placement.PendingResources = nil
	}
	if c == nil {
		return
	}
	cpu, mem := c.size.CPU, c.size.MemoryMiB
	vm.Status.CurrentResources = &infrav1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
	if vm.Status.Placement != nil {
		ceiling := c.ceiling
		vm.Status.Placement.MemoryCeilingMiB = &ceiling
	}
}
