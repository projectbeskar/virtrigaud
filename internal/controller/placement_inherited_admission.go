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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// Inherited admission revalidation (ADR-0007 A5). A durable capacity
// admission — a pending create's or clone's pendingHost with its size, or a
// resize reservation — is executed (Create, Clone, Reconfigure) without a new
// check only when this manager instance recorded it itself or has already
// revalidated exactly that admission (assume.Cache.Establish). Any other one
// was recorded by an earlier instance, possibly by a write that landed after
// this instance started leading and had already given the capacity to
// another VM: it is checked first, under the Provider's lock, against the
// same snapshot and assumptions as every other decision, with the VM's own
// records excluded. One that no longer fits is held — nothing is sent, the
// record is kept and keeps counting — and re-checked with a backoff.
//
// The check needs no assumption of its own: the record it validates is
// already in the informer cache it was read from (the cache only moves
// forward), so every later decision counts it.

// errAdmissionHostUnknown is an inherited admission whose host or HostPool
// can no longer be read for its capacity: it fails closed.
var errAdmissionHostUnknown = stderrors.New("the admission's host or its HostPool is not registered with the Provider")

// providerKeyOf is the assume-cache key of providerCR.
func providerKeyOf(providerCR *infravirtrigaudiov1beta1.Provider) string {
	return types.NamespacedName{Namespace: providerCR.Namespace, Name: providerCR.Name}.String()
}

// recordedCeiling is pl's recorded memory ceiling, -1 when none is recorded.
func recordedCeiling(pl *infravirtrigaudiov1beta1.PlacementStatus) int64 {
	if pl == nil || pl.MemoryCeilingMiB == nil {
		return -1
	}
	return *pl.MemoryCeilingMiB
}

// pendingAdmissionOf is the admission vm's pending placement records (a
// scheduled create, or a clone target with ref the clone's UID).
func pendingAdmissionOf(vm *infravirtrigaudiov1beta1.VirtualMachine, providerKey string, kind assume.AdmissionKind, ref string) assume.Admission {
	adm := assume.Admission{Kind: kind, Provider: providerKey, Host: pendingHost(vm), MemoryCeilingMiB: -1, Ref: ref}
	if pl := vm.Status.Placement; pl != nil {
		if pl.PendingResources != nil {
			adm.Resources = scheduler.ResourceRequest{CPU: pl.PendingResources.CPU, MemoryMiB: pl.PendingResources.MemoryMiB}
		}
		adm.MemoryCeilingMiB = recordedCeiling(pl)
	}
	return adm
}

// resizeAdmissionOf is the admission vm's resize reservation records on its
// bound host; ok is false when it has none.
func resizeAdmissionOf(vm *infravirtrigaudiov1beta1.VirtualMachine, providerKey string) (assume.Admission, bool) {
	res, ok := resizeReservationOf(vm)
	if !ok {
		return assume.Admission{}, false
	}
	return assume.Admission{Kind: assume.AdmissionResize, Provider: providerKey, Host: boundHost(vm), Resources: res,
		MemoryCeilingMiB: recordedCeiling(vm.Status.Placement)}, true
}

// revalidateUnderLock checks, holding provider's assume lock, whether vm can
// hold desired on host h, growing from current (zero for a create or clone):
// scheduler.CheckResize against the committed placements and live
// assumptions, the VM's own records and assumption excluded (so its own
// pending record or reservation is never counted twice). locked is false,
// having done nothing, when the lock was not free within placementLockWait;
// err is a failed cache read; checkErr is the verdict.
func revalidateUnderLock(
	ctx context.Context,
	reader client.Reader,
	assumptions *assume.Cache,
	provider types.NamespacedName,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	h *infravirtrigaudiov1beta1.Host,
	pool infravirtrigaudiov1beta1.HostPoolSpec,
	current, desired scheduler.ResourceRequest,
) (checkErr error, locked bool, err error) {
	providerKey := provider.String()
	unlock, locked := assumptions.LockWithin(ctx, providerKey, placementLockWait)
	if !locked {
		return nil, false, nil
	}
	defer unlock()
	lockCtx, cancel := lockBoundContext(ctx)
	defer cancel()
	view, _, err := placedWithAssumptionsFrom(lockCtx, reader, assumptions, providerKey, provider, vm, false)
	if err != nil {
		return nil, true, err
	}
	return scheduler.CheckResize(scheduler.ResizeRequest{
		Host: *h, Pool: pool, VMUID: vmSchedulingUID(vm), Current: current, Desired: desired,
		PlacedVMs: view.placed, Committed: view.byHost,
	}), true, nil
}

// admissionHostAndPool reads host hostID and its HostPool from the cache for
// an admission check. The error is errAdmissionHostUnknown when either is
// missing or the pool belongs to another Provider.
func admissionHostAndPool(
	ctx context.Context,
	reader client.Reader,
	providerCR *infravirtrigaudiov1beta1.Provider,
	hostID string,
) (*infravirtrigaudiov1beta1.Host, infravirtrigaudiov1beta1.HostPoolSpec, error) {
	h := &infravirtrigaudiov1beta1.Host{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: providerCR.Namespace, Name: hostID}, h); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, infravirtrigaudiov1beta1.HostPoolSpec{}, errAdmissionHostUnknown
		}
		return nil, infravirtrigaudiov1beta1.HostPoolSpec{}, fmt.Errorf("get Host %s/%s: %w", providerCR.Namespace, hostID, err)
	}
	pool := &infravirtrigaudiov1beta1.HostPool{}
	switch err := reader.Get(ctx, types.NamespacedName{Namespace: providerCR.Namespace, Name: h.Spec.PoolRef.Name}, pool); {
	case err == nil && pool.Spec.ProviderRef.Name == providerCR.Name:
		return h, pool.Spec, nil
	case err == nil, apierrors.IsNotFound(err):
		return nil, infravirtrigaudiov1beta1.HostPoolSpec{}, errAdmissionHostUnknown
	default:
		return nil, infravirtrigaudiov1beta1.HostPoolSpec{}, fmt.Errorf("get HostPool %s/%s: %w", providerCR.Namespace, h.Spec.PoolRef.Name, err)
	}
}

// inheritedVerdict is the outcome of checking an inherited admission.
type inheritedVerdict int

const (
	// inheritedProceed: established by this process, or revalidated now.
	inheritedProceed inheritedVerdict = iota
	// inheritedBusy: the Provider's lock was not free; retry soon.
	inheritedBusy
	// inheritedHeld: the admission no longer fits (or cannot be checked).
	inheritedHeld
)

// checkInheritedAdmission establishes adm for vm or reports why it cannot be:
// it returns inheritedProceed at once for an admission this process
// established, and otherwise revalidates it (revalidateUnderLock, growing
// from current to desired), establishing it on success. A held verdict comes
// with checkErr (a *scheduler.ResizeDoesNotFitError, errAdmissionHostUnknown,
// or a malformed pool ratio). err is a failed cache read.
func checkInheritedAdmission(
	ctx context.Context,
	reader client.Reader,
	assumptions *assume.Cache,
	providerCR *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	adm assume.Admission,
	current, desired scheduler.ResourceRequest,
) (verdict inheritedVerdict, checkErr error, err error) {
	uid := vmSchedulingUID(vm)
	if assumptions.Established(uid, adm) {
		return inheritedProceed, nil, nil
	}
	h, pool, herr := admissionHostAndPool(ctx, reader, providerCR, adm.Host)
	if herr != nil {
		if stderrors.Is(herr, errAdmissionHostUnknown) {
			log.FromContext(ctx).Info("An admission recorded by an earlier manager instance names a host whose capacity cannot be read; holding it",
				"kind", adm.Kind, "host", adm.Host)
			return inheritedHeld, herr, nil
		}
		return inheritedHeld, nil, herr
	}
	provider := types.NamespacedName{Namespace: providerCR.Namespace, Name: providerCR.Name}
	checkErr, locked, err := revalidateUnderLock(ctx, reader, assumptions, provider, vm, h, pool, current, desired)
	switch {
	case !locked:
		return inheritedBusy, nil, nil
	case err != nil:
		return inheritedHeld, nil, err
	case checkErr != nil:
		log.FromContext(ctx).Info("An admission recorded by an earlier manager instance does not fit its host any more; holding it",
			"kind", adm.Kind, "host", adm.Host, "cpu", desired.CPU, "memoryMiB", desired.MemoryMiB)
		var tooBig *scheduler.ResizeDoesNotFitError
		if stderrors.As(checkErr, &tooBig) {
			log.FromContext(ctx).V(1).Info("Committed-capacity arithmetic of a held inherited admission (administrator detail)",
				"host", adm.Host, "detail", tooBig.Detail())
		}
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		return inheritedHeld, checkErr, nil
	}
	assumptions.Establish(uid, adm)
	log.FromContext(ctx).Info("Revalidated an admission recorded by an earlier manager instance", "kind", adm.Kind, "host", adm.Host)
	return inheritedProceed, nil, nil
}

// inheritedHoldMessage is the condition message of a held inherited
// admission. Like every placement message (ADR-0007 A5, scheduler-accuracy
// amendment) it carries the VM's own size only: no host name, and no
// committed, capacity or free figure (those derive from other tenants' VMs;
// the host and the arithmetic go to the manager log). It is the same on every
// re-check, so a repeated hold writes nothing.
func inheritedHoldMessage(what string, size scheduler.ResourceRequest, checkErr error) string {
	if stderrors.Is(checkErr, errAdmissionHostUnknown) {
		return fmt.Sprintf("%s was admitted by an earlier manager instance on a host whose capacity can no longer be checked "+
			"(the host or its HostPool is not registered with the Provider); it is held and re-checked", what)
	}
	return fmt.Sprintf("%s (%d vCPU, %d MiB) was admitted by an earlier manager instance on a host that no longer has room for "+
		"it; nothing is sent to the provider, the admission is kept and counted, and it is re-checked with a backoff of up to %s "+
		"(see the administrator recovery for inherited admissions in the clustered-provider documentation)",
		what, size.CPU, size.MemoryMiB, placementUnschedulableMaxRetryInterval)
}

// holdInheritedCreate checks a clustered VM's pending create before its
// Create (or re-sent Create) goes to host: held is true, with the requeue,
// when its admission was inherited and no longer fits. A re-attach is never
// held: its domain already runs on host and the bind admits nothing new.
func (r *VirtualMachineReconciler) holdInheritedCreate(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	vmClass *infravirtrigaudiov1beta1.VMClass,
) (ctrl.Result, bool, error) {
	if isReattachPlacement(vm.Status.Placement) {
		return ctrl.Result{}, false, nil
	}
	adm := pendingAdmissionOf(vm, providerKeyOf(providerCR), assume.AdmissionCreate, "")
	want := admittedFootprint(vm, vmClass)
	verdict, checkErr, err := checkInheritedAdmission(ctx, r.placementReader(), r.placementAssumptions(), providerCR, vm, adm,
		scheduler.ResourceRequest{}, want)
	switch {
	case err != nil:
		return ctrl.Result{}, true, err
	case verdict == inheritedBusy:
		return ctrl.Result{RequeueAfter: placementLockBusyRetry()}, true, nil
	case verdict == inheritedHeld:
		msg := inheritedHoldMessage("the VM's create", want, checkErr)
		setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonInheritedAdmissionOverCapacity, msg)
		r.updatePlacementStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: r.unschedulable.next(vmSchedulingUID(vm), r.now())}, true, nil
	}
	return ctrl.Result{}, false, nil
}
