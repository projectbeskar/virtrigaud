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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// This file holds the VirtualMachine controller's clustered-provider lifecycle
// (ADR-0007 Addendum A): the pendingHost write-before-Create (A2), the
// owner-checked finalizer target, the no-recreate rule (A4) and the Placed
// condition. Every function here is reached only for a VM whose Provider has
// topology: cluster; the single-host / thin-client paths never call into it.

// setPlacedCondition upserts the VM's Placed condition (ADR-0007 Addendum A,
// A2), stamping ObservedGeneration so a client can tell which spec generation
// the placement state reflects. meta.SetStatusCondition bumps
// LastTransitionTime only on an actual status change, so re-asserting the same
// state every reconcile is a no-op.
func setPlacedCondition(vm *infravirtrigaudiov1beta1.VirtualMachine, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionPlaced,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
}

// recordPendingHost durably records the host a clustered VM's Create is about
// to be sent to (status.placement.pendingHost, with its pool and the scheduler
// trace) BEFORE the Create is issued (ADR-0007 Addendum A, A2).
//
// The write is a CHECKED status update: r.Status().Update carries vm's
// resourceVersion, so the API server rejects it if the object changed since it
// was read. It deliberately does NOT go through the error-swallowing
// updateStatus — if the record is not durable, Create must not run:
//
//   - success: recorded == true and the caller proceeds to Create on p.hostID;
//   - conflict: recorded == false with a plain requeue. The scheduler choice is
//     NOT re-applied to a fresh copy; the next reconcile re-reads the VM and
//     either reuses a pendingHost another writer recorded or schedules afresh;
//   - any other error: recorded == false and the error is returned (backoff).
func (r *VirtualMachineReconciler) recordPendingHost(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	p *clusterPlacement,
) (ctrl.Result, bool, error) {
	pl := vm.Status.Placement
	if pl == nil {
		pl = &infravirtrigaudiov1beta1.PlacementStatus{}
		vm.Status.Placement = pl
	}
	// A pending host binds the VM (spec.providerRef locks with it): record the
	// Provider it is bound through in the same checked write.
	recordBoundProvider(vm, providerCR)
	now := metav1.Now()
	pl.PendingHost = p.hostID
	// The admitted size, in the same checked write (review N3).
	pl.PendingResources = &infravirtrigaudiov1beta1.PlacementResources{CPU: p.resources.CPU, MemoryMiB: p.resources.MemoryMiB}
	pl.Pool = p.poolName
	pl.LastScheduledTime = &now
	pl.Reason = p.reason
	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonCreatePending,
		fmt.Sprintf("create pending on host %s (pool %s)", p.hostID, p.poolName))

	// Bounded, so the assumption covering this write (placementAssumeTTL, twice
	// this bound) always outlives it.
	writeCtx, cancel := context.WithTimeout(ctx, pendingHostWriteTimeout)
	defer cancel()
	if err := r.Status().Update(writeCtx, vm); err != nil {
		// Release the scheduler's assumption only when the write provably did
		// not land (review L6). After an ambiguous failure — a timeout, a 5xx,
		// a broken connection — the pendingHost may be stored after all, so
		// the assumption keeps counting until the informer shows the record
		// (which settles it) or its TTL passes.
		if pendingHostWriteRejected(err) {
			r.placementAssumptions().Forget(vmSchedulingUID(vm))
		}
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).Info("Pending-host write lost a resourceVersion race; requeueing without creating (scheduler choice not re-applied)",
				"host", p.hostID)
			return ctrl.Result{Requeue: true}, false, nil
		}
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		return ctrl.Result{}, false, fmt.Errorf("record pending host %s for VirtualMachine %s/%s: %w", p.hostID, vm.Namespace, vm.Name, err)
	}
	return ctrl.Result{}, true, nil
}

// pendingHostWriteRejected reports whether a failed status write was refused
// by the API server outright, so it certainly did not change the stored
// object: a conflict, an invalid or bad request, forbidden, or not found.
func pendingHostWriteRejected(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) ||
		apierrors.IsForbidden(err) || apierrors.IsNotFound(err)
}

// refuseGrownPendingCreate stops the retry of a pending clustered Create whose
// size has grown beyond the size it was admitted at
// (status.placement.pendingResources, review N3). The CRD freezes the VM's
// spec.classRef and spec.resources while it is pending, but the VMClass's own
// content can still change, and req was rebuilt from it. It reports whether it
// refused.
//
// The Create is not sent, and the pending host is KEPT rather than released
// for re-scheduling: the first attempt may already have left a domain there
// (A2), so moving the VM elsewhere could leave two. The VM gets
// Placed=False and Provisioning=False with reason PendingSizeGrew and is
// re-checked every placementConfigRetryInterval; restoring the VMClass lets
// the retry continue, and deleting the VM cleans up the pending host as
// usual. A VM with no recorded size (written by an older manager) is not
// checked; a smaller size is allowed.
func (r *VirtualMachineReconciler) refuseGrownPendingCreate(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	host string,
	req contracts.CreateRequest,
) (ctrl.Result, bool) {
	pl := vm.Status.Placement
	if pl == nil || pl.PendingResources == nil {
		return ctrl.Result{}, false
	}
	admitted := pl.PendingResources
	cpu, mem := req.Class.CPU, int64(req.Class.MemoryMiB)
	if cpu <= admitted.CPU && mem <= admitted.MemoryMiB {
		return ctrl.Result{}, false
	}
	msg := fmt.Sprintf("the create pending on host %s was admitted at %d vCPU and %d MiB, but its VMClass now asks for %d vCPU and %d MiB; "+
		"it is not sent until the VMClass is restored (or the VirtualMachine deleted)", host, admitted.CPU, admitted.MemoryMiB, cpu, mem)
	log.FromContext(ctx).Info("Not retrying a pending clustered create that has grown since it was admitted", "host", host,
		"admittedCPU", admitted.CPU, "admittedMemoryMiB", admitted.MemoryMiB, "cpu", cpu, "memoryMiB", mem)
	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonPendingSizeGrew, msg)
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonPendingSizeGrew, msg)
	metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
	r.updatePlacementStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: placementConfigRetryInterval}, true
}

// promotePendingHost records the confirmed binding after the provider accepted
// the Create on host (ADR-0007 D3 / Addendum A, A2): host becomes
// status.placement.host and pendingHost is cleared, so from now on every
// per-VM call is routed to it. Pool, LastScheduledTime and Reason were written
// with the pending record and are kept.
func promotePendingHost(vm *infravirtrigaudiov1beta1.VirtualMachine, host string) {
	pl := vm.Status.Placement
	if pl == nil {
		pl = &infravirtrigaudiov1beta1.PlacementStatus{}
		vm.Status.Placement = pl
	}
	pl.Host = host
	pl.PendingHost = ""
	pl.PendingResources = nil
	// The exclusions only steer scheduling of an unbound VM (A2 amendment); a
	// bound VM is never re-scheduled by Create, so they are dropped here.
	pl.ExcludedHosts = nil
	if pl.LastScheduledTime == nil {
		now := metav1.Now()
		pl.LastScheduledTime = &now
	}
	setPlacedCondition(vm, metav1.ConditionTrue, k8s.ReasonBound, fmt.Sprintf("VM is bound to host %s", host))
}

// handleClusteredCreateError handles a failed Create of a clustered VM aimed at
// its pending host. pendingHost is KEPT for every failure but one: the create
// may have partly happened there, so a retry must go to the same host and the
// finalizer can still clean it up (A2).
//
//   - A name conflict (Conflict / ALREADY_EXISTS) is the exception: the
//     provider found a same-named domain this VM does not own on the host
//     BEFORE creating anything, so this VM has no domain there and the
//     attempt created nothing. The host is excluded and the VM re-scheduled
//     (handleClusteredCreateConflict, the A2 amendment).
//   - A host-scoped unavailability (the pending host is unknown, draining or
//     unreachable) sets Placed=False/HostUnavailable. The VM is never
//     re-scheduled automatically, because a domain may already exist on that
//     host; it waits for the host or for an administrator to clear pendingHost
//     (D8, report-only).
//   - Anything else — including a provider-level Unavailable — sets
//     Placed=False/CreatePending and then takes the unchanged rejected /
//     transient create handling.
func (r *VirtualMachineReconciler) handleClusteredCreateError(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	host string,
	err error,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	msg := providerErrorMessage(err)

	if contracts.IsConflict(err) {
		return r.handleClusteredCreateConflict(ctx, vm, host, msg)
	}

	if contracts.IsHostUnavailable(err) {
		logger.Info("Pending host is unreachable; retrying the create on the same host (never re-scheduled)",
			"host", host, "error", err.Error())
		setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonHostUnavailable, fmt.Sprintf(
			"pending host %s is unreachable; the create is retried on the same host and never re-scheduled "+
				"(an administrator may clear status.placement.pendingHost to release it): %s", host, msg))
		k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, k8s.ReasonProviderError,
			fmt.Sprintf("Failed to create VM: %s", msg))
		r.updateStatus(ctx, vm)
		return ctrl.Result{RequeueAfter: pendingHostUnavailableRetryInterval}, nil
	}

	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonCreatePending,
		fmt.Sprintf("create on host %s is not confirmed: %s", host, msg))
	if res, handled := r.handleRejectedCreate(ctx, vm, err); handled {
		return res, nil
	}
	logger.Error(err, "Failed to create VM", "host", host)
	reason, requeueAfter := providerFailureOutcome(err)
	k8s.SetProvisioningCondition(&vm.Status.Conditions, metav1.ConditionFalse, reason,
		fmt.Sprintf("Failed to create VM: %s", msg))
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// maxExcludedHosts caps status.placement.excludedHosts. It must equal the
// field's +kubebuilder:validation:MaxItems (virtualmachine_types.go); a test
// pins the two together.
const maxExcludedHosts = 16

// excludeHost adds host to pl.ExcludedHosts (ADR-0007 Addendum A, A2
// amendment). An empty or already-listed host is a no-op; when the list would
// exceed maxExcludedHosts the oldest entries are dropped, so it stays bounded.
// It reports whether an entry was dropped.
func excludeHost(pl *infravirtrigaudiov1beta1.PlacementStatus, host string) (dropped bool) {
	host = strings.TrimSpace(host)
	if host == "" || slices.Contains(pl.ExcludedHosts, host) {
		return false
	}
	pl.ExcludedHosts = append(pl.ExcludedHosts, host)
	if n := len(pl.ExcludedHosts); n > maxExcludedHosts {
		pl.ExcludedHosts = append([]string(nil), pl.ExcludedHosts[n-maxExcludedHosts:]...)
		return true
	}
	return false
}

// handleClusteredCreateConflict is the ADR-0007 Addendum A, A2 amendment
// (slice 2): the Create aimed at the pending host was refused with a name
// conflict (Conflict / ALREADY_EXISTS) — a domain of the same name that this
// VM does not own already exists there. The provider checks that BEFORE it
// creates anything, so the refusal proves that this VM has no domain on the
// host and that THIS attempt created nothing there. It does not prove the host
// is clean of this VM: an earlier attempt that failed part-way (before the
// domain was defined) may have left a "<name>-disk" volume or cloud-init files
// behind. Those were never reachable by the finalizer either — its
// owner-checked Delete acts only on a domain this VM owns and never cleans up
// by name on a clustered host — so releasing the host loses no cleanup; they
// remain for an administrator to remove. Keeping pendingHost would pin the VM
// to that host forever (only an administrator could release it); instead:
//
//   - the host is added to status.placement.excludedHosts (bounded; cleared
//     when the VM is bound), which the scheduler honours;
//   - pendingHost is cleared, so the next reconcile schedules afresh;
//   - Placed=False/HostExcluded, and Ready / Provisioning = False /
//     ProviderConflict with the provider's (non-secret) message.
//
// The record is a checked status update, like recordPendingHost. If it does
// not land, nothing is lost: pendingHost is still set, so the next reconcile
// retries the Create on the same host, gets the same conflict and records it
// again. The finalizer does not need the host either: its owner-checked delete
// there would find no domain of this VM's.
//
// If every candidate host ends up excluded, resolveClusterPlacement reports
// Placed=False/AllHostsExcluded and re-checks slowly; each conflict removes
// one candidate, so the prompt re-scheduling is bounded by the pool size. Once
// the list is full and a conflict had to drop its oldest entry (a pool with
// more conflicting hosts than the cap), the next attempt waits the slow
// vmCreateConflictRetryInterval instead, so even that case cannot turn into a
// fast create loop.
func (r *VirtualMachineReconciler) handleClusteredCreateConflict(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	host string,
	providerMsg string,
) (ctrl.Result, error) {
	pl := vm.Status.Placement
	if pl == nil {
		pl = &infravirtrigaudiov1beta1.PlacementStatus{}
		vm.Status.Placement = pl
	}
	retryAfter := createConflictRescheduleInterval
	if excludeHost(pl, host) {
		retryAfter = vmCreateConflictRetryInterval
	}
	pl.PendingHost = ""
	pl.PendingResources = nil
	// The VM holds nothing on the host any more; a leftover assumption of it
	// there must not keep counting.
	r.forgetPlacement(vm)

	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonHostExcluded, fmt.Sprintf(
		"create on host %s was refused because a same-named domain this VirtualMachine does not own exists there, "+
			"so this attempt created nothing on it; the host is excluded for this VM and it will be re-scheduled onto another host", host))
	msg := fmt.Sprintf("Provider rejected VM create on host %s (host excluded; re-scheduling): %s", host, providerMsg)
	for _, condType := range []string{k8s.ConditionReady, k8s.ConditionProvisioning} {
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             k8s.ReasonProviderConflict,
			Message:            msg,
			ObservedGeneration: vm.Generation,
		})
	}
	metrics.RecordError(errReasonProviderCreateRejected, metrics.ComponentManager)
	log.FromContext(ctx).Info("Create refused with a name conflict on the pending host; excluding the host and re-scheduling",
		"host", host, "excludedHosts", pl.ExcludedHosts)

	if err := r.Status().Update(ctx, vm); err != nil {
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).Info("Excluded-host write lost a resourceVersion race; requeueing (the create is retried on the same host)",
				"host", host)
			return ctrl.Result{Requeue: true}, nil
		}
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		return ctrl.Result{}, fmt.Errorf("record excluded host %s for VirtualMachine %s/%s: %w", host, vm.Namespace, vm.Name, err)
	}
	return ctrl.Result{RequeueAfter: retryAfter}, nil
}

// handleMissingOnBoundHost is ADR-0007 Addendum A, A4: the bound host reports
// that a clustered VM does not exist (Describe exists=false, or the provider's
// not-found on Describe, Power or Reconfigure — which a clustered provider
// also answers for a domain whose owner stamp is not this VM's). The VM is NOT
// re-created — neither on the bound host nor elsewhere — because without
// fencing that risks two running copies of one disk (D8). The controller
// records Ready=False/VMMissingOnHost and only re-checks slowly, so an
// administrator restoring the domain is noticed. Status.ID and the binding are
// left untouched. errReason is the metrics reason of the call that found it
// missing.
func (r *VirtualMachineReconciler) handleMissingOnBoundHost(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	ref contracts.VMRef,
	errReason string,
) (ctrl.Result, error) {
	msg := fmt.Sprintf("hypervisor VM %q is not present on its bound host %s; it is not re-created automatically "+
		"(no failover without fencing). Restore it on that host, or delete and re-create the VirtualMachine",
		ref.ID, ref.HostID)
	log.FromContext(ctx).Info("Clustered VM missing on its bound host; not re-creating", "id", ref.ID, "host", ref.HostID)
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonVMMissingOnHost,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	metrics.RecordError(errReason, metrics.ComponentManager)
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: vmMissingOnHostRetryInterval}, nil
}

// handleNotRoutable records why no per-VM call can be routed for vm and
// requeues without calling the provider (ADR-0007 Addendum A): an unbound
// clustered VM gets Placed=False/Unbound; a VM whose recorded clustered
// placement no longer matches its Provider's topology gets
// Ready=False/PlacementTopologyMismatch; a VM whose spec.providerRef no longer
// resolves to the Provider it is bound through gets
// Ready=False/ProviderRefMismatch (plus a warning event) and is re-checked
// slowly. Any other error is returned as-is. (A cross-namespace consumer
// refusal goes through refuseConsumer.)
func (r *VirtualMachineReconciler) handleNotRoutable(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, err error) (ctrl.Result, error) {
	if !markNotRoutable(vm, err) {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("Not calling the provider for this VM", "reason", err.Error())
	r.updateStatus(ctx, vm)
	switch {
	case isProviderRefMismatch(err):
		metrics.RecordError(errReasonProviderRefMismatch, metrics.ComponentManager)
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonProviderRefMismatch, err.Error())
		return ctrl.Result{RequeueAfter: providerRefMismatchRetryInterval}, nil
	case isPlacementTopologyMismatch(err):
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		return ctrl.Result{RequeueAfter: placementConfigRetryInterval}, nil
	}
	metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
	return ctrl.Result{RequeueAfter: placementUnboundRetryInterval}, nil
}

// refuseConsumer records that vm references a Provider, VMClass or VMImage in
// another namespace that does not select its own (cause, a
// *ConsumerNotAllowedError) and makes no provider call:
// Ready=False/ConsumerNotAllowed with ObservedGeneration, a Warning event on
// the transition only, and a slow recheck — the grant watches re-drive it as
// soon as access is granted. persisted is the status as read at the start of
// the reconcile: when nothing changed (a recheck of the same refusal) the
// status is not written again, so refused VMs cost no API writes.
func (r *VirtualMachineReconciler) refuseConsumer(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	persisted *infravirtrigaudiov1beta1.VirtualMachineStatus,
	cause error,
) (ctrl.Result, error) {
	newRefusal := consumerRefusalIsNew(vm.Status.Conditions, cause)
	markNotRoutable(vm, cause)
	metrics.RecordError(errReasonConsumerNotAllowed, metrics.ComponentManager)
	if newRefusal {
		log.FromContext(ctx).Info("Not calling the provider for this VM", "reason", cause.Error())
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonConsumerNotAllowed, cause.Error())
	}
	if persisted == nil || !equality.Semantic.DeepEqual(persisted, &vm.Status) {
		r.updateStatus(ctx, vm)
	}
	return ctrl.Result{RequeueAfter: consumerNotAllowedRetryInterval}, nil
}

// markNotRoutable sets the condition for a vmRefFor failure and reports
// whether err was one (unbound, a placement/topology mismatch, a provider
// reference mismatch, or an ungranted cross-namespace reference).
func markNotRoutable(vm *infravirtrigaudiov1beta1.VirtualMachine, err error) bool {
	switch {
	case isProviderRefMismatch(err):
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               k8s.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             k8s.ReasonProviderRefMismatch,
			Message:            err.Error(),
			ObservedGeneration: vm.Generation,
		})
	case isConsumerNotAllowed(err):
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               k8s.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             k8s.ReasonConsumerNotAllowed,
			Message:            err.Error(),
			ObservedGeneration: vm.Generation,
		})
	case isVMUnbound(err):
		setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonUnbound, err.Error())
	case isPlacementTopologyMismatch(err):
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type:               k8s.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             k8s.ReasonPlacementTopologyMismatch,
			Message:            err.Error(),
			ObservedGeneration: vm.Generation,
		})
	default:
		return false
	}
	return true
}

// handleBoundHostUnavailable records that a clustered VM's bound host is
// unknown, draining or unreachable (a host-scoped Unavailable from the
// provider on Describe, Power or Reconfigure) and re-checks it on
// boundHostUnavailableRetryInterval. The binding is kept; nothing is
// re-scheduled (D8). errReason is the metrics reason of the failed call.
func (r *VirtualMachineReconciler) handleBoundHostUnavailable(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	ref contracts.VMRef,
	err error,
	errReason string,
) (ctrl.Result, error) {
	log.FromContext(ctx).Info("Bound host is unavailable; re-checking later", "host", ref.HostID, "error", err.Error())
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonHostUnavailable,
		Message:            fmt.Sprintf("bound host %s is unavailable: %s", ref.HostID, providerErrorMessage(err)),
		ObservedGeneration: vm.Generation,
	})
	metrics.RecordError(errReason, metrics.ComponentManager)
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: boundHostUnavailableRetryInterval}, nil
}

// routedCallRetryAfter is the requeue after a failed per-VM provider call of
// the VM controller (Describe, Power, Reconfigure) that handleRoutedOpError
// did not take. It is one rule for every routed call, not a per-operation one:
// a routed (clustered) call the provider answers with Unimplemented
// (contracts NotSupported) is re-checked every
// routedOpNotSupportedRetryInterval rather than every few seconds, so a
// manager talking to an older clustered provider image that does not route
// that RPC yet (version skew) does not hammer it. Everything else — and every
// single-host / thin-client call — keeps the historical 5s cadence.
func routedCallRetryAfter(ref contracts.VMRef, err error) time.Duration {
	if ref.Routed() && contracts.IsNotSupported(err) {
		return routedOpNotSupportedRetryInterval
	}
	return providerErrorRetryInterval
}

// handleRoutedOpError handles a failed Power or Reconfigure of a CLUSTERED VM
// (ADR-0007 Addendum A, slice 2) and reports whether it did. Both are routed
// to the VM's bound host and owner-checked there, so two failures are
// host-level facts, handled exactly like the same answer to Describe:
//
//   - a host-scoped Unavailable (the bound host is unknown, draining or
//     unreachable): Ready=False/HostUnavailable, re-checked every
//     boundHostUnavailableRetryInterval rather than the 5s cadence;
//   - NotFound (the domain is gone, or its owner stamp is not this VM's): A4 —
//     Ready=False/VMMissingOnHost, never re-created, re-checked slowly.
//
// Every other error, and every error of a single-host call (ref not routed),
// is left to the caller's historical handling (handled == false).
func (r *VirtualMachineReconciler) handleRoutedOpError(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	ref contracts.VMRef,
	err error,
	errReason string,
) (ctrl.Result, bool) {
	if !ref.Routed() {
		return ctrl.Result{}, false
	}
	switch {
	case contracts.IsHostUnavailable(err):
		res, _ := r.handleBoundHostUnavailable(ctx, vm, ref, err, errReason)
		return res, true
	case contracts.IsNotFound(err):
		res, _ := r.handleMissingOnBoundHost(ctx, vm, ref, errReason)
		return res, true
	}
	return ctrl.Result{}, false
}

// deletionTarget decides which hypervisor VM the finalizer must delete for vm
// on provider (ADR-0007 Addendum A, A1/A2). It returns ok == false with the
// result to return when the finalizer must be retained without calling the
// provider; otherwise ok == true and ref is the VM to delete (an empty ref.ID
// means "nothing to delete on the provider").
//
//   - Status.ID set: the ref comes from vmRefFor — the bare id for a single-host
//     provider (unchanged), the bound host and the VM's owner for a clustered
//     one.
//   - Status.ID empty but pendingHost set (a clustered create in flight): an
//     owner-checked Delete is sent to the pending host, so a domain the create
//     already made there does not leak. The provider destroys it only if its
//     owner stamp is this VM's; anything else is reported not-found untouched.
//
// A VM for which no delete can be routed — a clustered VM with no binding, one
// whose recorded placement no longer matches its Provider's topology, or one
// bound through another Provider object (status.boundProvider) — is never sent
// a Delete: the finalizer is retained with the matching condition, unless the
// force-delete escape hatch is set (retainForUnroutableDelete).
func (r *VirtualMachineReconciler) deletionTarget(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	provider *infravirtrigaudiov1beta1.Provider,
) (contracts.VMRef, bool, ctrl.Result) {
	logger := log.FromContext(ctx)

	var (
		ref contracts.VMRef
		err error
	)
	if vm.Status.ID == "" {
		if err = checkVMProvider(vm, provider); err == nil {
			err = placementTopologyError(vm, provider)
		}
		if err == nil {
			ref, _ = pendingCreateRef(vm)
			logger.Info("VM has a create in flight; sending an owner-checked delete to its pending host",
				"id", ref.ID, "host", ref.HostID)
		}
	} else {
		ref, err = vmRefFor(vm, provider)
	}
	if err == nil {
		return ref, true, ctrl.Result{}
	}

	if res, retain := r.retainForUnroutableDelete(ctx, vm, err); retain {
		return contracts.VMRef{}, false, res
	}
	return contracts.VMRef{}, true, ctrl.Result{}
}

// retainForUnroutableDelete decides the finalizer of a VM being deleted whose
// provider Delete cannot be routed (err is the vmRefFor / provider-binding
// failure). By default the finalizer is retained (retain == true, with the
// matching condition set and the result to return): the hypervisor VM must not
// be silently orphaned, nor deleted through the wrong Provider. With the
// force-delete escape hatch set it returns retain == false and the caller
// removes the finalizer WITHOUT any provider call (the hypervisor VM may be
// left behind), exactly as for any other undeletable VM.
func (r *VirtualMachineReconciler) retainForUnroutableDelete(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	err error,
) (ctrl.Result, bool) {
	logger := log.FromContext(ctx)
	if hasForceDeleteAnnotation(vm) {
		logger.Error(err, "Cannot route the provider delete but force-delete annotation is set; removing finalizer (the provider VM may be orphaned)",
			"id", vm.Status.ID, "annotation", forceDeleteAnnotation)
		metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)
		return ctrl.Result{}, false
	}
	logger.Info("Cannot route the provider delete; retaining finalizer", "id", vm.Status.ID, "reason", err.Error())
	markNotRoutable(vm, err)
	metrics.RecordError(errReasonProviderDelete, metrics.ComponentManager)
	r.updateStatus(ctx, vm)
	if isProviderRefMismatch(err) {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonProviderRefMismatch, fmt.Sprintf(
			"not deleting the hypervisor VM through a Provider it is not bound to: %v. Set %s=true to detach it, or %s=true to drop the finalizer",
			err, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation))
		return ctrl.Result{RequeueAfter: providerRefMismatchRetryInterval}, true
	}
	if isConsumerNotAllowed(err) {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonConsumerNotAllowed, fmt.Sprintf(
			"not deleting the hypervisor VM through a Provider this namespace may not use: %v. Set %s=true to detach it, or %s=true to drop the finalizer",
			err, infravirtrigaudiov1beta1.VirtualMachineOrphanOnDeleteAnnotation, forceDeleteAnnotation))
		return ctrl.Result{RequeueAfter: consumerNotAllowedRetryInterval}, true
	}
	return ctrl.Result{RequeueAfter: vmDeleteRetryInterval}, true
}
