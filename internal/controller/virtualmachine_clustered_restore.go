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
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	"github.com/projectbeskar/virtrigaud/internal/scheduler"
	"github.com/projectbeskar/virtrigaud/internal/scheduler/assume"
)

// The restore guards a clustered VirtualMachine meets before it is first
// placed (ADR-0007 A6.2): R1, the restore marker and its hold, and R4, the
// pre-schedule uniqueness check. Both run only for a VM of a clustered
// Provider that has neither a status.id nor a status.placement.pendingHost —
// once per VM before its first placement, and again on each retry while it is
// held — never for a bound VM, a VM with a create in flight, or a VM waiting
// for adoption or its clone. Single-host and thin-client Providers never reach
// them (D9, A6 decision 3).
//
// Trust. The marker (infra.virtrigaud.io/placement-uid) is an annotation, so
// the VM's owner can write it: it is untrusted input. It is read only for the
// VM that carries it, and only to HOLD that VM; it never names a host, never
// selects one, never authorizes a bind, and never skips the scheduler or R4.
// A forged marker naming another UID therefore only makes the forger's own VM
// wait; removing it (or setting it to the VM's own UID) only lets R4 run.
// R4 acts on hypervisor truth — the owner stamps the provider reads on its
// hosts — and the only thing it ever records is the host where a domain
// stamped with the VM's OWN UID was found, which the create retry then proves
// again (the provider binds only a domain stamped with the requester's UID).

// errReasonPreScheduleCheck counts the reconciles whose pre-schedule
// uniqueness check (R4) could not run: the Provider lacks the owner filter,
// or its capabilities or its filtered listing failed.
const errReasonPreScheduleCheck = "preschedule-check"

// placementUIDMarker returns the VM's restore marker (R1), or "".
func placementUIDMarker(vm *infravirtrigaudiov1beta1.VirtualMachine) string {
	return strings.TrimSpace(vm.Annotations[infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation])
}

// markerNamesAnotherUID reports whether vm carries a restore marker naming a
// UID other than its own: the VirtualMachine entered placement under another
// UID (it was restored from a backup, or re-applied from an exported
// manifest). A missing marker, or one naming the VM's own UID, never holds.
func markerNamesAnotherUID(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	m := placementUIDMarker(vm)
	return m != "" && m != string(vm.UID)
}

// ensurePlacementUIDMarker sets vm's restore marker to its own UID (R1) when
// it names anything else or is missing. It is a metadata patch with an
// optimistic lock (vm's resourceVersion): a VM changed since it was read is
// not written, and the conflict is returned for the caller to requeue. On
// success vm carries the new resourceVersion and annotations, so a checked
// status write that follows is not refused; vm's in-memory status is kept as
// it is. A VM without a UID (never stored) is left alone.
func ensurePlacementUIDMarker(ctx context.Context, c client.Client, vm *infravirtrigaudiov1beta1.VirtualMachine) error {
	uid := string(vm.UID)
	if uid == "" || vm.Annotations[infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation] == uid {
		return nil
	}
	patched := vm.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation] = uid
	if err := c.Patch(ctx, patched, client.MergeFromWithOptions(vm, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("set %s on VirtualMachine %s/%s: %w",
			infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation, vm.Namespace, vm.Name, err)
	}
	vm.ResourceVersion = patched.ResourceVersion
	vm.Annotations = patched.Annotations
	return nil
}

// markPlacementUID is ensurePlacementUIDMarker for the VirtualMachine
// controller's first placement: it runs before the VM's first pendingHost
// write. ok is false when the reconcile must return res, err: a conflict
// requeues (nothing was written), any other failure is returned.
func (r *VirtualMachineReconciler) markPlacementUID(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine) (ctrl.Result, bool, error) {
	if err := ensurePlacementUIDMarker(ctx, r.Client, vm); err != nil {
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).Info("Placement-uid marker write lost a resourceVersion race; requeueing without placing the VM")
			return ctrl.Result{Requeue: true}, false, nil
		}
		metrics.RecordError(errReasonPlacement, metrics.ComponentManager)
		return ctrl.Result{}, false, err
	}
	return ctrl.Result{}, true, nil
}

// restoreMarkerMessage is the Placed / Provisioning message of a VM held by
// R1. It names no host, no domain and no UID.
var restoreMarkerMessage = fmt.Sprintf(
	"held: this VirtualMachine carries the restore marker %s naming another UID — it was restored from a backup or "+
		"re-applied from an exported manifest under a new UID — and it has no binding, so it is not scheduled and nothing "+
		"is created (ADR-0007 A6, R1). An administrator re-attaches its previous domain, or releases it by removing the "+
		"marker or setting it to this VirtualMachine's UID (the pre-schedule check then runs); see %s. Re-checked with a "+
		"backoff of up to %s", infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation, restorePendingRunbook, blockedRetryMax)

// preScheduleIncarnationMessage is the Placed / Provisioning message of a VM
// held by R4. It names no host, no domain and no UID.
var preScheduleIncarnationMessage = fmt.Sprintf(
	"held before scheduling: a domain VirtRigaud created for this VirtualMachine's namespace and name under another UID — "+
		"a previous incarnation, left by orphan-on-delete, a force-delete or a backup restore — exists on a host of its "+
		"Provider (or more than one such domain, or one whose owner cannot be read), and a clustered Provider holds at most "+
		"one per namespace and name (ADR-0007 A6, R4). Nothing is scheduled or created. An administrator must re-attach "+
		"that domain to this VirtualMachine or remove it; see %s. Re-checked with a backoff of up to %s",
	restorePendingRunbook, blockedRetryMax)

// lacksListOwnerFilterMessage is the Placed / Provisioning message of a VM
// whose Provider cannot run R4.
var lacksListOwnerFilterMessage = fmt.Sprintf(
	"held before scheduling: the clustered Provider does not report supportsListOwnerFilter (an older provider image), "+
		"so the pre-schedule check that no other domain exists for this VirtualMachine's namespace and name cannot run "+
		"(ADR-0007 A6, R4). Nothing is scheduled or created; upgrade the provider image. Re-checked with a backoff of up to %s",
	blockedRetryMax)

// uniquenessCheckFailedMessage is the Placed / Provisioning message of a VM
// whose R4 check failed. The provider's error goes to the manager log only:
// it may name the provider's endpoint.
var uniquenessCheckFailedMessage = fmt.Sprintf(
	"held before scheduling: the pre-schedule check (ADR-0007 A6, R4) could not ask the Provider's hosts whether a "+
		"domain for this VirtualMachine's namespace and name already exists; nothing is scheduled or created, and the "+
		"check is retried with a backoff of up to %s", blockedRetryMax)

// ownDomainUnknownHostMessage is the Placed / Provisioning message of a VM
// whose own domain R4 found on a host the manager cannot record: not a Host
// of its Provider. It names no host.
var ownDomainUnknownHostMessage = fmt.Sprintf(
	"held: a domain of this VirtualMachine — stamped with its own UID — exists on a host that is not a Host of its "+
		"Provider, so nothing is created. An administrator must restore that Host object (or remove the domain); "+
		"see %s. Re-checked with a backoff of up to %s", restorePendingRunbook, blockedRetryMax)

// guardFirstPlacement runs R1 and then R4 for a clustered VM that has neither
// a status.id nor a pendingHost, before anything about its create happens
// (image prepare, scheduling). done == true means the VM is held: the
// conditions are set and the reconcile returns res, err, sending nothing to
// the provider but the R4 queries. Otherwise the VM may be placed: host is
// "" (schedule it) or the host where R4 found the VM's own domain (record it
// as pendingHost without scheduling; the create retry binds the domain).
func (r *VirtualMachineReconciler) guardFirstPlacement(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerInstance contracts.Provider,
) (found *contracts.VMInfo, done bool, res ctrl.Result, err error) {
	// R1: a marker naming another UID holds the VM, whatever the hosts say
	// (so a host that cannot be reached never lets it through, decision 4).
	if markerNamesAnotherUID(vm) {
		log.FromContext(ctx).Info("Holding a clustered VM before placement: its restore marker names another UID (ADR-0007 A6, R1)",
			"marker", placementUIDMarker(vm))
		return nil, true, r.holdBeforePlacement(ctx, vm, k8s.ReasonRestorePending, restoreMarkerMessage, errReasonRestorePending), nil
	}
	return r.preScheduleUniquenessCheck(ctx, vm, providerInstance)
}

// preScheduleUniquenessCheck is R4 (ADR-0007 A6.2): before the VM is first
// scheduled, the Provider's hosts are asked — with one owner-filtered ListVMs,
// each host reporting only this VM's candidate domains — whether a domain
// stamped with the VM's namespace and name already exists:
//
//   - none: the VM is scheduled as usual (host == "");
//   - exactly one, stamped with the VM's OWN UID: its status was lost, or an
//     administrator re-stamped it; host is where it runs, recorded as
//     pendingHost without scheduling or capacity admission;
//   - one under another UID, more than one, or a candidate whose stamp cannot
//     be read: held RestorePending (the invariant: at most one per namespace
//     and name). A foreign or unstamped domain that merely has the name is not
//     counted (decision 2): the create on its host keeps the slice 2 rule.
//
// Hosts that could not be checked are not waited for (decision 4): the VM
// proceeds on the reachable hosts' evidence; a VM whose marker names another
// UID was already held by R1. A Provider that does not report the owner
// filter, or that answers without applying it, holds the VM
// (ProviderLacksListOwnerFilter); a failed capability query or listing holds
// it with UniquenessCheckFailed. Every hold backs off (blockedRetryBackoff).
func (r *VirtualMachineReconciler) preScheduleUniquenessCheck(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerInstance contracts.Provider,
) (*contracts.VMInfo, bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	v, list, lookupErr := lookupIncarnations(ctx, vm, providerInstance)
	if lookupErr != nil {
		logger.Info("Pre-schedule uniqueness check could not run; holding the VM", "reason", lookupErr.reason, "error", lookupErr.Error())
		msg := uniquenessCheckFailedMessage
		if lookupErr.reason == k8s.ReasonProviderLacksListOwnerFilter {
			msg = lacksListOwnerFilterMessage
		}
		return nil, true, r.holdBeforePlacement(ctx, vm, lookupErr.reason, msg, errReasonPreScheduleCheck), nil
	}
	if len(list.UnreachableHostIDs) > 0 {
		logger.Info("Pre-schedule uniqueness check: some hosts could not be checked; proceeding on the reachable hosts' evidence (ADR-0007 A6, decision 4)",
			"unreachableHosts", list.UnreachableHostIDs)
	}
	switch {
	case v.foreign > 0 || v.ambiguous > 0 || len(v.own) > 1:
		logger.Info("Holding a clustered VM before placement: its Provider holds another domain for its namespace and name (ADR-0007 A6, R4)",
			"previousIncarnations", v.foreign, "unreadable", v.ambiguous, "ownDomains", len(v.own))
		return nil, true, r.holdBeforePlacement(ctx, vm, k8s.ReasonRestorePending, preScheduleIncarnationMessage, errReasonRestorePending), nil
	case len(v.own) == 1:
		logger.Info("Pre-schedule uniqueness check found this VM's own domain; recording its host as the pending host (not scheduled)",
			"host", v.own[0].HostID)
		return &v.own[0], false, ctrl.Result{}, nil
	}
	return nil, false, ctrl.Result{}, nil
}

// lookupError is why R4's lookup could not run: reason is the Placed reason
// of the hold (ProviderLacksListOwnerFilter or UniquenessCheckFailed); the
// wrapped error is for the manager log only (it may name the provider's
// endpoint).
type lookupError struct {
	reason string
	err    error
}

func (e *lookupError) Error() string { return e.err.Error() }
func (e *lookupError) Unwrap() error { return e.err }

// errNoOwnerFilter is the lookupError cause of a provider that cannot filter.
var errNoOwnerFilter = errors.New("the provider does not report the owner-filtered ListVMs")

// lookupIncarnations is R4's lookup: one owner-filtered ListVMs of vm's
// namespace and name through providerInstance, classified. It needs the
// provider to report supportsListOwnerFilter AND to mark its answer
// owner_filter_applied (an unmarked answer is an unfiltered listing from a
// provider that ignored the filter — version skew — and is never classified).
func lookupIncarnations(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerInstance contracts.Provider) (incarnationVerdict, contracts.VMList, *lookupError) {
	lacks := func(err error) (incarnationVerdict, contracts.VMList, *lookupError) {
		return incarnationVerdict{}, contracts.VMList{}, &lookupError{reason: k8s.ReasonProviderLacksListOwnerFilter, err: err}
	}
	failed := func(step string, err error) (incarnationVerdict, contracts.VMList, *lookupError) {
		return incarnationVerdict{}, contracts.VMList{}, &lookupError{reason: k8s.ReasonUniquenessCheckFailed,
			err: fmt.Errorf("%s: %w", step, err)}
	}
	reporter, okCaps := providerInstance.(contracts.CapabilityReporter)
	lister, okList := providerInstance.(contracts.OwnerFilteredLister)
	if !okCaps || !okList {
		return lacks(errNoOwnerFilter)
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		return failed("capabilities", err)
	}
	if !caps.SupportsListOwnerFilter {
		return lacks(fmt.Errorf("%w (supportsListOwnerFilter=false)", errNoOwnerFilter))
	}
	filter := contracts.OwnerFilter{Namespace: vm.Namespace, Name: vm.Name}
	list, err := lister.ListVMsForOwner(ctx, filter)
	if err != nil {
		return failed("owner-filtered listing", err)
	}
	if !list.OwnerFilterApplied {
		return lacks(fmt.Errorf("%w (the answer does not carry owner_filter_applied)", errNoOwnerFilter))
	}
	return classifyIncarnations(vm, filter, list), list, nil
}

// moveToOwnDomain is R4's lookup for a clustered create answered with the
// VM's OWN domain on another host (contracts.IsVMOwnDomainElsewhere, from the
// cluster-wide disk guard): the pending host is not where the domain is — the
// domain's host was unreachable at the first placement, the status was
// restored with another pending host, or an administrator re-stamped a
// previous incarnation the guard had found on another host. When the lookup
// finds exactly that one own domain, on a Host of the Provider, and nothing
// else stamped with the VM's namespace and name, the pending host is moved
// there (a checked status write) and the create retry binds it — no
// administrator edits status. Moving is as safe as the slice 2 release: the
// provider answered before writing anything on the pending host.
//
// The move records the domain's own size (reattachPlacement: its current
// vCPUs and memory, its balloon maximum as the ceiling) as pendingResources,
// not the size admitted on the old pending host, and moves the placement
// assumption to the new host under the Provider's assume lock (no capacity
// admission: the domain already runs there).
//
// moved == false (and a nil error) leaves the caller's own-domain hold in
// place: the lookup could not run, found anything else, found the domain on
// the pending host itself, or the assume lock was busy (the next retry tries
// again).
func (r *VirtualMachineReconciler) moveToOwnDomain(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	providerInstance contracts.Provider,
	req contracts.CreateRequest,
	pending string,
) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)
	v, _, lookupErr := lookupIncarnations(ctx, vm, providerInstance)
	if lookupErr != nil {
		logger.Info("Could not look the VM's own domain up; keeping the hold", "reason", lookupErr.reason, "error", lookupErr.Error())
		return ctrl.Result{}, false, nil
	}
	if v.foreign > 0 || v.ambiguous > 0 || len(v.own) != 1 || v.own[0].HostID == pending {
		logger.Info("The VM's own domain is not on exactly one other host; keeping the hold",
			"ownDomains", len(v.own), "previousIncarnations", v.foreign, "unreadable", v.ambiguous)
		return ctrl.Result{}, false, nil
	}
	found := v.own[0]
	h, err := r.providerHost(ctx, providerCR, found.HostID)
	if err != nil || h == nil {
		return ctrl.Result{}, false, err
	}
	p := reattachPlacement(found, h, req,
		"re-attach: this VirtualMachine's own domain was found on this host (ADR-0007 A6, R4); the pending host moved here")
	if !r.assumeReattach(ctx, providerCR, vm, p) {
		logger.V(1).Info("Provider's placement lock is busy; keeping the hold until the next retry")
		return ctrl.Result{}, false, nil
	}

	pl := vm.Status.Placement
	if pl == nil {
		pl = &infravirtrigaudiov1beta1.PlacementStatus{}
		vm.Status.Placement = pl
	}
	now := metav1.Now()
	pl.PendingHost = p.hostID
	pl.Pool = p.poolName
	pl.PendingResources = &infravirtrigaudiov1beta1.PlacementResources{CPU: p.resources.CPU, MemoryMiB: p.resources.MemoryMiB}
	ceiling := min(p.memoryCeilingMiB, maxRecordedMemoryMiB)
	pl.MemoryCeilingMiB = &ceiling
	pl.LastScheduledTime = &now
	pl.Reason = p.reason
	setPlacedCondition(vm, metav1.ConditionFalse, k8s.ReasonCreatePending,
		"create pending on the host where this VirtualMachine's own domain was found (ADR-0007 A6, R4); the create retry binds it")
	writeCtx, cancel := context.WithTimeout(ctx, pendingHostWriteTimeout)
	defer cancel()
	if err := r.Status().Update(writeCtx, vm); err != nil {
		if pendingHostWriteRejected(err) {
			r.placementAssumptions().Forget(vmSchedulingUID(vm))
		}
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, false, fmt.Errorf("move the pending host of VirtualMachine %s/%s to its own domain's host: %w",
			vm.Namespace, vm.Name, err)
	}
	logger.Info("Moved the pending host to the host of the VM's own domain (ADR-0007 A6, R4)", "from", pending, "to", p.hostID,
		"cpu", p.resources.CPU, "memoryMiB", p.resources.MemoryMiB)
	return ctrl.Result{Requeue: true}, true, nil
}

// incarnationVerdict is what R4 found for one VM.
type incarnationVerdict struct {
	// own are the domains stamped with the VM's own UID (each with its host).
	own []contracts.VMInfo
	// foreign counts the domains stamped with the VM's namespace and name
	// under another UID.
	foreign int
	// ambiguous counts the candidates whose owner stamp could not be relied
	// on (unreadable, or more than one owner).
	ambiguous int
}

// classifyIncarnations sorts an owner-filtered listing for vm. Only a stamp
// recording vm's namespace and name counts, whatever its UID; a candidate the
// provider could not classify (owner_stamp_state) counts as ambiguous; an
// unstamped or foreign-stamped domain is ignored.
func classifyIncarnations(vm *infravirtrigaudiov1beta1.VirtualMachine, filter contracts.OwnerFilter, list contracts.VMList) incarnationVerdict {
	var v incarnationVerdict
	for _, info := range list.VMs {
		if info.ProviderRaw[contracts.VMInfoOwnerStampStateKey] != "" {
			v.ambiguous++
			continue
		}
		if !filter.Matches(info) {
			continue
		}
		uids := contracts.OwnerUIDs(info)
		if len(uids) == 1 && uids[0] == string(vm.UID) && strings.TrimSpace(info.HostID) != "" {
			info.HostID = strings.TrimSpace(info.HostID)
			v.own = append(v.own, info)
			continue
		}
		v.foreign++
	}
	return v
}

// holdBeforePlacement records that vm, never placed, is held by R1 or R4:
// Placed=False and Provisioning=False with reason and msg, one Warning event
// when the hold begins or changes reason, the metric errReason, and a retry
// with the blocked-VM backoff from when the hold began (createHoldSince: 15 s
// doubling to 5 min). Nothing is written about the placement, so the plain
// status update suffices.
func (r *VirtualMachineReconciler) holdBeforePlacement(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	reason, msg, errReason string,
) ctrl.Result {
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionPlaced)
	alreadyHeld := prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == reason
	setPlacedCondition(vm, metav1.ConditionFalse, reason, msg)
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionProvisioning,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	metrics.RecordError(errReason, metrics.ComponentManager)
	if !alreadyHeld {
		r.recordEvent(vm, corev1.EventTypeWarning, reason, msg)
	}
	r.updateStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: blockedRetryBackoff(createHoldSince(vm))}
}

// reattachReasonPrefix starts status.placement.reason of a pending placement
// R4 recorded for the VM's OWN domain (discoveredPlacement, moveToOwnDomain):
// the create retry there binds an existing domain, so it is sent at the
// domain's recorded size and the bind records that size as
// status.currentResources (isReattachPlacement). The reason is operator-written
// status, which tenants cannot write.
const reattachReasonPrefix = "re-attach:"

// isReattachPlacement reports whether pl is a pending re-attach placement
// with a recorded size.
func isReattachPlacement(pl *infravirtrigaudiov1beta1.PlacementStatus) bool {
	return pl != nil && pl.PendingHost != "" && pl.PendingResources != nil && strings.HasPrefix(pl.Reason, reattachReasonPrefix)
}

// reattachRequest sizes the create of a pending re-attach (isReattachPlacement)
// at the size recorded with its pending host — the size of the domain it
// binds — instead of the spec's. The provider binds the existing domain
// whatever the request's size (an idempotent success); had the domain vanished
// meanwhile, a fresh one is created at exactly the recorded and counted size,
// so the recorded status stays true. Any other request is returned unchanged.
func reattachRequest(vm *infravirtrigaudiov1beta1.VirtualMachine, req contracts.CreateRequest) contracts.CreateRequest {
	pl := vm.Status.Placement
	if !isReattachPlacement(pl) {
		return req
	}
	req.Class.CPU = pl.PendingResources.CPU
	req.Class.MemoryMiB = int32(min(pl.PendingResources.MemoryMiB, int64(math.MaxInt32))) // #nosec G115 -- bounded above
	return req
}

// recordReattachedSize records, on the bind of a re-attach, the size recorded
// with its pending host as status.currentResources: the domain's own size,
// which the next reconcile compares with the spec (a grow through the resize
// gate, a shrink when powered off). The memory ceiling recorded with the
// pending host is kept by promotePendingHost.
func recordReattachedSize(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	pr := vm.Status.Placement.PendingResources
	cpu, mem := pr.CPU, pr.MemoryMiB
	vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{CPU: &cpu, MemoryMiB: &mem}
}

// reattachSize is the size of the VM's own domain R4 found (found): its
// current vCPUs and memory (contracts.VMInfoCurrentVCPUsKey /
// VMInfoCurrentMemoryMiBKey, else its maxima), clamped and at least the
// minimum footprint, and the balloon ceiling to count it at: its memory
// maximum (found.MemoryMiB, libvirt's <memory>) when above its current
// memory, and never less than what a create of that size provisions (req's
// memory hot-add), so the create retry is never refused as grown. A size the
// provider did not report falls back to req's.
func reattachSize(found contracts.VMInfo, req contracts.CreateRequest) (scheduler.ResourceRequest, int64) {
	cpu, mem := found.CPU, found.MemoryMiB
	if n, err := strconv.ParseInt(found.ProviderRaw[contracts.VMInfoCurrentVCPUsKey], 10, 32); err == nil && n > 0 {
		cpu = int32(n)
	}
	if n, err := strconv.ParseInt(found.ProviderRaw[contracts.VMInfoCurrentMemoryMiBKey], 10, 64); err == nil && n > 0 {
		mem = n
	}
	if cpu <= 0 {
		cpu = req.Class.CPU
	}
	if mem <= 0 {
		mem = int64(req.Class.MemoryMiB)
	}
	res := withMinimum(scheduler.ResourceRequest{CPU: clampReportedVCPUs(cpu), MemoryMiB: clampReportedMemoryMiB(mem)})
	ceiling := memoryCeilingFor(requestsMemoryHotAdd(req), res.MemoryMiB)
	if maxMem := clampReportedMemoryMiB(found.MemoryMiB); maxMem > res.MemoryMiB && maxMem > ceiling {
		ceiling = maxMem
	}
	return res, ceiling
}

// reattachPlacement is the placement R4 records for the VM's own domain found
// on host h: that host and its pool, at the domain's own size (reattachSize).
func reattachPlacement(found contracts.VMInfo, h *infravirtrigaudiov1beta1.Host, req contracts.CreateRequest, reason string) *clusterPlacement {
	res, ceiling := reattachSize(found, req)
	return &clusterPlacement{hostID: h.Name, poolName: h.Spec.PoolRef.Name, reason: reason, resources: res, memoryCeilingMiB: ceiling}
}

// providerHost returns the Host named host when it is a Host of providerCR,
// nil (and no error) when it is not — never recorded as a placement.
func (r *VirtualMachineReconciler) providerHost(ctx context.Context, providerCR *infravirtrigaudiov1beta1.Provider,
	host string) (*infravirtrigaudiov1beta1.Host, error) {
	h := &infravirtrigaudiov1beta1.Host{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: providerCR.Namespace, Name: host}, h); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get Host %s/%s: %w", providerCR.Namespace, host, err)
	}
	if h.Spec.ProviderRef.Name != providerCR.Name {
		return nil, nil
	}
	return h, nil
}

// assumeReattach records p — a re-attach placement, not admitted — as vm's
// placement assumption under the Provider's assume lock, as a scheduled
// placement is (scheduleUnderLock): concurrent schedules of the Provider count
// the domain on its host from now on, before the pendingHost write lands. It
// reports false, assuming nothing, when the lock is busy.
func (r *VirtualMachineReconciler) assumeReattach(ctx context.Context, providerCR *infravirtrigaudiov1beta1.Provider,
	vm *infravirtrigaudiov1beta1.VirtualMachine, p *clusterPlacement) bool {
	assumptions := r.placementAssumptions()
	providerKey := types.NamespacedName{Namespace: providerCR.Namespace, Name: providerCR.Name}.String()
	unlock, locked := assumptions.LockWithin(ctx, providerKey, placementLockWait)
	if !locked {
		return false
	}
	defer unlock()
	assumptions.Assume(providerKey, assume.Assumption{
		UID:       vmSchedulingUID(vm),
		Namespace: vm.Namespace,
		Name:      vm.Name,
		HostID:    p.hostID,
		Labels:    vm.Labels, // Assume keeps its own copy
		Resources: withMemoryCeiling(p.resources, p.memoryCeilingMiB),
	})
	return true
}

// discoveredPlacement is the placement R4 records for a VM whose own domain
// (found) it found before the VM's first placement: that host and its pool,
// at the domain's own size (reattachSize; no capacity admission — the domain
// already runs there, A6 R4), assumed under the Provider's assume lock like a
// scheduled placement. A host that is not a Host of providerCR is never
// recorded: the VM is held (OwnDomainOnAnotherHost) and p is nil. A busy lock
// requeues shortly, recording nothing.
func (r *VirtualMachineReconciler) discoveredPlacement(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	providerCR *infravirtrigaudiov1beta1.Provider,
	req contracts.CreateRequest,
	found contracts.VMInfo,
) (*clusterPlacement, ctrl.Result, error) {
	h, err := r.providerHost(ctx, providerCR, found.HostID)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if h == nil {
		log.FromContext(ctx).Info("The VM's own domain was found on a host that is not a Host of its Provider; holding it", "host", found.HostID)
		return nil, r.holdBeforePlacement(ctx, vm, k8s.ReasonOwnDomainOnAnotherHost, ownDomainUnknownHostMessage, errReasonRestorePending), nil
	}
	p := reattachPlacement(found, h, req,
		"re-attach: the pre-schedule check found this VirtualMachine's own domain on this host (ADR-0007 A6, R4); not scheduled")
	if !r.assumeReattach(ctx, providerCR, vm, p) {
		log.FromContext(ctx).V(1).Info("Provider's placement lock is busy; requeueing")
		return nil, ctrl.Result{RequeueAfter: placementLockBusyRetry()}, nil
	}
	return p, ctrl.Result{}, nil
}

// restoredBindingMessage is the Ready message of a BOUND clustered VM whose
// restore marker names another UID and whose bound host reports its domain
// missing: its status was restored with the binding, but the domain there is
// still stamped with the UID it had before (ADR-0007 A6, R1). It names no host.
var restoredBindingMessage = fmt.Sprintf(
	"held: this VirtualMachine was restored with its binding under a new UID (its restore marker %s names another UID), "+
		"and the domain on its bound host is not stamped with this VirtualMachine's UID, so its owner-checked calls find "+
		"nothing there. It is not re-created. An administrator re-stamps that domain with this VirtualMachine's UID (the "+
		"marker is then rewritten automatically); see %s. Re-checked with a backoff of up to %s",
	infravirtrigaudiov1beta1.VirtualMachinePlacementUIDAnnotation, restorePendingRunbook, blockedRetryMax)

// holdRestoredBinding is handleMissingOnBoundHost for a VM whose restore
// marker names another UID (ADR-0007 A6, R1): Ready=False/RestorePending
// instead of VMMissingOnHost, one Warning event when the hold begins, the
// restore-pending metric, and the blocked-VM backoff from when Ready went
// RestorePending. The binding is kept and nothing is re-created (A4).
func (r *VirtualMachineReconciler) holdRestoredBinding(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ref contracts.VMRef) ctrl.Result {
	prev := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
	alreadyHeld := prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == k8s.ReasonRestorePending
	log.FromContext(ctx).Info("Restored clustered VM's domain is not stamped with its UID on its bound host; holding it (ADR-0007 A6, R1)",
		"id", ref.ID, "host", ref.HostID)
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonRestorePending,
		Message:            restoredBindingMessage,
		ObservedGeneration: vm.Generation,
	})
	metrics.RecordError(errReasonRestorePending, metrics.ComponentManager)
	if !alreadyHeld {
		r.recordEvent(vm, corev1.EventTypeWarning, k8s.ReasonRestorePending, restoredBindingMessage)
	}
	r.updateStatus(ctx, vm)
	retry := blockedRetryMin
	if alreadyHeld {
		retry = blockedRetryBackoff(conditionSince(vm.Status.Conditions, k8s.ConditionReady))
	}
	return ctrl.Result{RequeueAfter: retry}
}
