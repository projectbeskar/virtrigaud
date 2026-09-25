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
	stderrors "errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// This file is the clustered resize gate (ADR-0007 Addendum A,
// scheduler-accuracy amendment; William's decision in the review): a
// Reconfigure that grows the CPU or memory of a VM on a clustered Provider is
// admitted against the free capacity of the VM's host, under the same
// per-Provider assume lock as scheduling, before the provider is asked to
// apply it. A shrink is always allowed. Single-host and thin-client Providers
// never reach it.

// desiredResources is the size the VM's spec asks for: effectiveResources —
// its VMClass size with any spec.resources override applied, bounds-checked —
// the same values needsReconfigure compares and a Reconfigure sends.
func desiredResources(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) (scheduler.ResourceRequest, error) {
	cpu, mem, err := effectiveResources(vm, vmClass)
	if err != nil {
		return scheduler.ResourceRequest{}, err
	}
	return scheduler.ResourceRequest{CPU: cpu, MemoryMiB: int64(mem)}, nil
}

// recordedResources is the size recorded in status.currentResources (zeros
// when unrecorded).
func (r *VirtualMachineReconciler) recordedResources(vm *infravirtrigaudiov1beta1.VirtualMachine) scheduler.ResourceRequest {
	return scheduler.ResourceRequest{CPU: r.getCurrentCPU(vm), MemoryMiB: r.getCurrentMemoryMiB(vm)}
}

// setResizeRefused records Reconfiguring=False/reason with msg on vm.
func setResizeRefused(vm *infravirtrigaudiov1beta1.VirtualMachine, reason, msg string) {
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReconfiguring,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
}

// admitClusteredResize decides whether the reconfigure the VM's spec asks for
// may be sent to its clustered Provider. It returns admitted == true when the
// VM does not grow, or when every growing resource fits in the free capacity
// of hostID (the VM's own current footprint excluded); an admitted resize-up
// is assumed at its new size, so a concurrent schedule or resize of the same
// Provider counts it before the provider has applied it and
// status.currentResources records it. Otherwise it has recorded
// Reconfiguring=False with the reason on vm, persisted the status, and
// returns the requeue; the VM keeps its current size and no provider call is
// made. A failed cache read is returned as an error.
//
// Like scheduling, the lock covers informer-cache reads and in-memory work
// only; every status write happens after it is released.
func (r *VirtualMachineReconciler) admitClusteredResize(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	hostID string,
) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	desired, err := desiredResources(vm, vmClass)
	if err != nil {
		// An invalid override. reconcileVM refuses it (needsReconfigure)
		// before it gets here; should one still arrive, the reconfigure path
		// refuses it the same way without a provider call, so there is nothing
		// to admit or refuse on capacity here.
		return ctrl.Result{}, true, nil
	}
	current := r.recordedResources(vm)
	uid := vmSchedulingUID(vm)
	// Compare what the VM counts at before and after: memory is counted at
	// its balloon ceiling when it has one (review N1), so a live memory grow
	// within the ceiling commits nothing new and needs no check.
	ceiling := memoryCeilingOf(vm, vmClass)
	currentFP, desiredFP := withMemoryCeiling(current, ceiling), withMemoryCeiling(desired, ceiling)
	if !scheduler.Grows(currentFP, desiredFP) {
		r.unschedulable.reset(uid)
		return ctrl.Result{}, true, nil
	}

	// The host and its pool's overcommit ratios, from the Provider's namespace.
	host := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: providerCR.Namespace, Name: hostID}, host); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, false, fmt.Errorf("get Host %s/%s to admit a resize: %w", providerCR.Namespace, hostID, err)
		}
		// Unknown capacity is not bookable: fail closed.
		msg := fmt.Sprintf("resize to %d vCPU and %d MiB is not applied: its host %s is not registered, so its free capacity is unknown; "+
			"the VM keeps %d vCPU and %d MiB", desired.CPU, desired.MemoryMiB, hostID, current.CPU, current.MemoryMiB)
		return r.refuseResize(ctx, vm, k8s.ReasonInsufficientHostCapacity, msg, r.unschedulable.next(uid, r.now())), false, nil
	}
	// The pool's overcommit ratios scale the host's capacity. A pool that is
	// missing, or belongs to another Provider, leaves the capacity unknown:
	// fail closed (review N6) rather than guess a ratio.
	pool := &infravirtrigaudiov1beta1.HostPool{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: providerCR.Namespace, Name: host.Spec.PoolRef.Name}, pool); {
	case err == nil && pool.Spec.ProviderRef.Name == providerCR.Name:
	case err == nil, apierrors.IsNotFound(err):
		msg := fmt.Sprintf("resize to %d vCPU and %d MiB is not applied: the HostPool %q of its host %s does not exist or belongs to another Provider, "+
			"so its capacity is unknown; the VM keeps %d vCPU and %d MiB", desired.CPU, desired.MemoryMiB, host.Spec.PoolRef.Name, hostID,
			current.CPU, current.MemoryMiB)
		return r.refuseResize(ctx, vm, k8s.ReasonPlacementError, msg, placementConfigRetryInterval), false, nil
	default:
		return ctrl.Result{}, false, fmt.Errorf("get HostPool %s/%s to admit a resize: %w", providerCR.Namespace, host.Spec.PoolRef.Name, err)
	}
	poolSpec := pool.Spec

	providerNN := types.NamespacedName{Namespace: providerCR.Namespace, Name: providerCR.Name}
	resize := scheduler.ResizeRequest{Host: *host, Pool: poolSpec, VMUID: uid, Current: currentFP, Desired: desiredFP}
	locked, checkErr := r.checkResizeUnderLock(ctx, r.placementAssumptions(), providerNN, vm, resize)
	if !locked {
		logger.V(1).Info("Provider's placement lock is busy; requeueing the resize", "provider", providerNN.String())
		return ctrl.Result{RequeueAfter: placementLockBusyRetry()}, false, nil
	}
	var infraErr *placementInfraError
	if stderrors.As(checkErr, &infraErr) {
		return ctrl.Result{}, false, infraErr.err
	}

	var tooBig *scheduler.ResizeDoesNotFitError
	switch {
	case checkErr == nil:
		r.unschedulable.reset(uid)
		logger.Info("Admitted a clustered resize-up against its host's free capacity",
			"host", hostID, "cpu", desired.CPU, "memoryMiB", desired.MemoryMiB)
		return ctrl.Result{}, true, nil
	case stderrors.As(checkErr, &tooBig):
		retryAfter := r.unschedulable.next(uid, r.now())
		// No committed, capacity or free figure (review M3); the arithmetic is
		// logged for administrators.
		logger.V(1).Info("Committed-capacity arithmetic of a refused resize (administrator detail)",
			"host", hostID, "detail", tooBig.Detail())
		msg := fmt.Sprintf("resizing to %d vCPU and %d MiB exceeds the free capacity of its host %s; the VM keeps %d vCPU and %d MiB and the resize is retried (next check in %s)",
			desired.CPU, desired.MemoryMiB, hostID, current.CPU, current.MemoryMiB, retryAfter)
		return r.refuseResize(ctx, vm, k8s.ReasonInsufficientHostCapacity, msg, retryAfter), false, nil
	default:
		// A malformed pool overcommit ratio: an administrator must fix it.
		msg := fmt.Sprintf("resize cannot be checked against its host %s: %v", hostID, checkErr)
		return r.refuseResize(ctx, vm, k8s.ReasonPlacementError, msg, placementConfigRetryInterval), false, nil
	}
}

// shrinks reports whether desired lowers current's CPU or memory.
func shrinks(current, desired scheduler.ResourceRequest) bool {
	return desired.CPU < current.CPU || desired.MemoryMiB < current.MemoryMiB
}

// poweredOff reports whether a Describe found the VM powered off.
func poweredOff(desc contracts.DescribeResponse) bool {
	return desc.PowerState == string(contracts.PowerStateOff)
}

// deferClusteredShrink holds a shrink of a RUNNING clustered VM until it is
// powered off (review N1). The libvirt provider cannot be trusted to have
// applied a live shrink: an unplug of vCPUs that are not hot-pluggable fails
// and is reported as a success needing a restart, and `setmem --live` only
// moves the balloon target, which the guest may ignore. Recording the smaller
// size in status.currentResources would make the VM count below what it still
// uses, and let other VMs be placed into memory it holds. So while the VM runs,
// no Reconfigure is sent for a change that lowers CPU or memory — a mixed change
// (one resource up, the other down) waits as a whole — the VM gets
// Reconfiguring=False/ShrinkPendingPowerOff, keeps counting at its current size,
// and is re-checked with the placement backoff. The operator never powers the
// VM off by itself; applyPendingShrinkWhileOff applies the change once it is
// observed off. It reports whether it deferred.
func (r *VirtualMachineReconciler) deferClusteredShrink(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	desc contracts.DescribeResponse,
) (ctrl.Result, bool) {
	desired, err := desiredResources(vm, vmClass)
	current := r.recordedResources(vm)
	if err != nil || poweredOff(desc) || !shrinks(current, desired) {
		return ctrl.Result{}, false
	}
	retryAfter := r.unschedulable.next(vmSchedulingUID(vm), r.now())
	msg := fmt.Sprintf("the VM asks for %d vCPU and %d MiB, less than the %d vCPU and %d MiB it holds; on a clustered Provider a running VM "+
		"is not shrunk, because a running guest can keep using what it has. The change is applied once the VM is powered off "+
		"(set spec.powerState: Off; the operator never powers it off by itself). Until then it counts at its current size (next check in %s)",
		desired.CPU, desired.MemoryMiB, current.CPU, current.MemoryMiB, retryAfter)
	log.FromContext(ctx).Info("Deferring a clustered shrink until the VM is powered off",
		"cpu", desired.CPU, "memoryMiB", desired.MemoryMiB, "currentCPU", current.CPU, "currentMemoryMiB", current.MemoryMiB)
	setResizeRefused(vm, k8s.ReasonShrinkPendingPowerOff, msg)
	r.updatePlacementStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: retryAfter}, true
}

// applyPendingShrinkWhileOff applies a deferred clustered shrink when the VM is
// observed powered off (review N1), before anything powers it on again: the
// provider then takes its offline (config) path, and only after it succeeds is
// the new size recorded in status.currentResources. A resource that grows in
// the same change still goes through the resize gate. It reports whether it
// handled the reconcile; it does nothing unless the VM needs a reconfigure
// that lowers CPU or memory.
func (r *VirtualMachineReconciler) applyPendingShrinkWhileOff(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider contracts.Provider,
	ref contracts.VMRef,
	providerCR *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
	vmImage *infravirtrigaudiov1beta1.VMImage,
	networks []*infravirtrigaudiov1beta1.VMNetworkAttachment,
) (ctrl.Result, bool, error) {
	needs, err := r.needsReconfigure(vm, vmClass)
	if err != nil || !needs {
		return ctrl.Result{}, false, nil // an invalid override is reported by the main path
	}
	desired, err := desiredResources(vm, vmClass)
	if err != nil || !shrinks(r.recordedResources(vm), desired) {
		return ctrl.Result{}, false, nil
	}
	res, admitted, err := r.admitClusteredResize(ctx, vm, providerCR, vmClass, ref.HostID)
	if err != nil || !admitted {
		return res, true, err
	}
	log.FromContext(ctx).Info("Applying a deferred clustered shrink while the VM is powered off",
		"cpu", desired.CPU, "memoryMiB", desired.MemoryMiB)
	res, err = r.reconfigureVM(ctx, vm, provider, ref, providerCR, vmClass, vmImage, networks)
	return res, true, err
}

// checkResizeUnderLock is the part of admitClusteredResize that runs under the
// Provider's assume lock: read what is committed, check the resize, and on a
// fit assume the VM at its new size. It returns locked == false, doing nothing,
// when the lock is not free within placementLockWait. err is a failed cache
// read (a *placementInfraError) or scheduler.CheckResize's verdict. The lock is
// released by a deferred call, so a panic inside cannot leave it held.
func (r *VirtualMachineReconciler) checkResizeUnderLock(
	ctx context.Context,
	assumptions *assume.Cache,
	provider types.NamespacedName,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	resize scheduler.ResizeRequest,
) (locked bool, err error) {
	providerKey := provider.String()
	unlock, locked := assumptions.LockWithin(ctx, providerKey, placementLockWait)
	if !locked {
		return false, nil
	}
	defer unlock()
	lockCtx, cancel := lockBoundContext(ctx)
	defer cancel()
	placed, err := r.placedWithAssumptions(lockCtx, assumptions, providerKey, provider, vm)
	if err != nil {
		return true, &placementInfraError{err: err}
	}
	resize.PlacedVMs = placed
	if err := scheduler.CheckResize(resize); err != nil {
		return true, err
	}
	assumptions.AssumeFor(providerKey, assume.Assumption{
		UID: resize.VMUID, Namespace: vm.Namespace, Name: vm.Name, HostID: resize.Host.Name,
		Labels: vm.Labels, Resources: resize.Desired, Resize: true,
	}, resizeAssumeTTL)
	return true, nil
}

// refuseResize records a refused resize on vm, persists the status (bounded,
// after the assume lock is released) and returns the requeue.
func (r *VirtualMachineReconciler) refuseResize(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	reason, msg string,
	retryAfter time.Duration,
) ctrl.Result {
	log.FromContext(ctx).Info("Not applying the resize: "+msg, "reason", reason)
	setResizeRefused(vm, reason, msg)
	metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
	r.updatePlacementStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: retryAfter}
}
