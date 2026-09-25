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
	if !scheduler.Grows(current, desired) {
		return ctrl.Result{}, true, nil
	}
	uid := vmSchedulingUID(vm)

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
	var poolSpec infravirtrigaudiov1beta1.HostPoolSpec
	pool := &infravirtrigaudiov1beta1.HostPool{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: providerCR.Namespace, Name: host.Spec.PoolRef.Name}, pool); {
	case err == nil && pool.Spec.ProviderRef.Name == providerCR.Name:
		poolSpec = pool.Spec
	case err == nil, apierrors.IsNotFound(err):
		// No pool of this Provider: no overcommit (ratio 1.0), the
		// conservative reading.
	default:
		return ctrl.Result{}, false, fmt.Errorf("get HostPool %s/%s to admit a resize: %w", providerCR.Namespace, host.Spec.PoolRef.Name, err)
	}

	providerNN := types.NamespacedName{Namespace: providerCR.Namespace, Name: providerCR.Name}
	resize := scheduler.ResizeRequest{Host: *host, Pool: poolSpec, VMUID: uid, Current: current, Desired: desired}
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
		msg := fmt.Sprintf("%s; the VM keeps %d vCPU and %d MiB and the resize is retried (next check in %s)",
			tooBig.Error(), current.CPU, current.MemoryMiB, retryAfter)
		return r.refuseResize(ctx, vm, k8s.ReasonInsufficientHostCapacity, msg, retryAfter), false, nil
	default:
		// A malformed pool overcommit ratio: an administrator must fix it.
		msg := fmt.Sprintf("resize cannot be checked against its host %s: %v", hostID, checkErr)
		return r.refuseResize(ctx, vm, k8s.ReasonPlacementError, msg, placementConfigRetryInterval), false, nil
	}
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
	placed, err := r.placedWithAssumptions(ctx, assumptions, providerKey, provider, vm)
	if err != nil {
		return true, &placementInfraError{err: err}
	}
	resize.PlacedVMs = placed
	if err := scheduler.CheckResize(resize); err != nil {
		return true, err
	}
	assumptions.Assume(providerKey, assume.Assumption{
		UID: resize.VMUID, Namespace: vm.Namespace, Name: vm.Name, HostID: resize.Host.Name,
		Labels: vm.Labels, Resources: resize.Desired, Resize: true,
	})
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
