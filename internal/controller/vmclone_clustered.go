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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/obs/logging"
	"github.com/projectbeskar/virtrigaud/internal/obs/metrics"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	utilk8s "github.com/projectbeskar/virtrigaud/internal/util/k8s"
)

// The VMClone controller's clustered-provider flow (ADR-0007 Addendum A, A1 +
// A2, slice 3). A clone on a clustered provider:
//
//   - lands on the source VM's bound host, and only there: disks are host-local
//     and a linked clone depends on its source disk. If that host is gone,
//     being deleted, cordoned, not Ready, or excluded for the target VM, the
//     clone waits with a condition saying why; it never falls back to another
//     host.
//   - creates its target VirtualMachine BEFORE the Clone RPC, so the clone can
//     be stamped with the target's identity (namespace, name, uid) and the
//     target's status.placement.pendingHost can be recorded — with a checked
//     status update — before anything is created on the host (A2). A lost
//     answer is then safe: the retry goes to the same host, where the clone
//     stamped with the target's uid is an idempotent success, and deleting the
//     target VM runs the finalizer's owner-checked Delete on the pending host.
//   - binds the target in one status write (status.id, the host, pendingHost
//     promoted), as on a single host (bindTargetVM).
//
// The single-host / thin-client flow (startClone) is unchanged.

const (
	// cloneReasonSourceHostGone: the source VM's host is not a Host of the
	// clone's Provider any more.
	cloneReasonSourceHostGone = "SourceHostGone"
	// cloneReasonSourceHostNotReady: the source VM's host is not Ready.
	cloneReasonSourceHostNotReady = "SourceHostNotReady"
	// cloneReasonSourceHostCordoned: the source VM's host takes no new
	// placement (spec.schedulable=false, or the Host is being deleted).
	cloneReasonSourceHostCordoned = "SourceHostCordoned"
	// cloneReasonSourceHostExcluded: the source VM's host is in the target
	// VM's status.placement.excludedHosts — a domain of the clone's name that
	// the target VM does not own exists there.
	cloneReasonSourceHostExcluded = "SourceHostExcluded"
	// cloneReasonRetrying: the clone is retried on the same host, e.g. while
	// an earlier attempt's copy still runs there.
	cloneReasonRetrying = "CloneRetrying"

	// cloneHostBlockedRetryInterval re-checks a clone whose landing host
	// cannot take it. Nothing the controller does changes that, so it is slow.
	cloneHostBlockedRetryInterval = 2 * time.Minute
	// cloneHostUnavailableRetryInterval retries a clone whose landing host
	// could not be reached, on the same host.
	cloneHostUnavailableRetryInterval = 30 * time.Second
)

// startClusteredClone issues the Clone RPC of a clone whose source is bound to
// host source.HostID on a clustered provider (see the file comment).
func (r *VMCloneReconciler) startClusteredClone(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	source contracts.VMRef,
	provider *infrav1beta1.Provider,
	providerInstance contracts.Provider,
	targetNamespace string,
	sourceVM *infrav1beta1.VirtualMachine,
	linked bool,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	host := source.HostID

	cloner, ok := providerInstance.(contracts.Cloner)
	if !ok {
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonUnsupported, "provider does not support clone"), nil
	}

	// 0. The provider must route clones (slice 3) — checked before anything is
	// created, so an older clustered provider (which would answer the Clone
	// with Unimplemented) never gets a target VirtualMachine made for it.
	if res, supported := r.gateRoutedClone(ctx, clone, providerInstance); !supported {
		return res, nil
	}

	// 1. The landing host must take a new placement. Checked before the target
	// VirtualMachine is created, so a clone that cannot land creates nothing.
	if reason, msg, err := r.cloneLandingHostProblem(ctx, provider, host); err != nil {
		logger.Error(err, "Failed to read the source VM's host", "host", host)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	} else if reason != "" {
		return r.waitForCloneHost(ctx, clone, reason, msg, cloneHostBlockedRetryInterval), nil
	}

	// 2. The target VirtualMachine, created by this clone (or by an earlier
	// reconcile of it).
	target, res, done, err := r.ensureClusteredCloneTarget(ctx, clone, sourceVM, provider, targetNamespace)
	if done {
		return res, err
	}
	if pl := target.Status.Placement; pl != nil && containsHost(pl.ExcludedHosts, host) {
		return r.waitForCloneHost(ctx, clone, cloneReasonSourceHostExcluded, fmt.Sprintf(
			"the source VM's host %s is excluded for the target VM %s/%s: a domain of the clone's name that the target does not own "+
				"exists there, and the clone can land on no other host. Resolve the name conflict and clear the target's "+
				"status.placement.excludedHosts", host, target.Namespace, target.Name), cloneHostBlockedRetryInterval), nil
	}

	// 3. The clone must fit in the free capacity of its host — the only host
	// it can land on. It is admitted, and assumed there, under the
	// Provider's assume lock shared with the VirtualMachine controller,
	// before the pending host is recorded and before anything is created on
	// the host (vmclone_capacity.go). A clone that does not fit waits; it
	// never falls back to another host.
	adm, res, admitted, err := r.admitClusteredClone(ctx, clone, target, provider, sourceVM, host)
	if !admitted {
		return res, err
	}

	// 4. Record the target's pending host, with its admitted size, before
	// anything is created on it.
	if res, recorded, err := r.recordClonePendingHost(ctx, clone, target, provider, sourceVM, host, adm); !recorded {
		return res, err
	}

	// 5. Re-read the grants from the API server right before the RPC, as the
	// single-host flow does.
	if allowed, res, err := r.confirmTargetNamespaceLive(ctx, clone, targetNamespace); !allowed {
		return res, err
	}
	if allowed, res, err := r.gateConsumers(ctx, r.liveReader(), clone, sourceVM, targetNamespace); !allowed {
		return res, err
	}

	// The clone is made at exactly the size it was admitted at (and recorded
	// with its pending host), never at whatever its VMClass says by now.
	classJSON, err := adm.cloneClassJSON()
	if err != nil {
		return ctrl.Result{}, err
	}
	req := contracts.CloneRequest{
		Source:        source,
		TargetHostID:  host,
		TargetName:    clone.Spec.Target.Name,
		TargetVM:      contracts.ObjectIdentity{UID: string(target.UID), Namespace: target.Namespace, Name: target.Name},
		Linked:        linked,
		ClassJSON:     classJSON,
		PlacementJSON: r.placementJSON(ctx, clone),
		CustomizeJSON: r.customizeJSON(ctx, clone),
	}

	now := metav1.Now()
	clone.Status.Phase = infrav1beta1.ClonePhaseCloning
	if clone.Status.StartTime == nil {
		clone.Status.StartTime = &now
	}
	if linked {
		clone.Status.ActualCloneType = infrav1beta1.CloneTypeLinkedClone
	} else {
		clone.Status.ActualCloneType = infrav1beta1.CloneTypeFullClone
	}
	utilk8s.SetCondition(&clone.Status.Conditions, infrav1beta1.VMCloneConditionCloning,
		metav1.ConditionTrue, infrav1beta1.VMCloneReasonCloning, fmt.Sprintf("Clone operation initiated on host %s", host))

	resp, err := cloner.Clone(ctx, req)
	if err != nil {
		return r.handleClusteredCloneError(ctx, clone, target, host, err)
	}

	clone.Status.TargetVMID = resp.TargetVmID
	clone.Status.TaskRef = resp.TaskRef
	// Persist the target VM ID before binding (see startClone).
	if err := r.updateStatus(ctx, clone); err != nil {
		return ctrl.Result{}, err
	}
	if resp.TaskRef == "" {
		logger.Info("Clone completed synchronously", "target_vm_id", resp.TargetVmID, "host", host)
		return r.bindTargetVM(ctx, clone, sourceVM, provider, targetNamespace, resp.TargetVmID, linked)
	}
	logger.Info("Clone task started", "task_ref", resp.TaskRef, "host", host)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// gateRoutedClone reports whether the clustered provider routes Clone to the
// source's host (GetCapabilities.supports_routed_clone, ADR-0007 Addendum A
// slice 3). It fails closed: a provider that does not report capabilities, or
// reports no routed clone, fails the clone (Unsupported); a capability query
// that fails is retried (Pending). Nothing is created in either case.
func (r *VMCloneReconciler) gateRoutedClone(ctx context.Context, clone *infrav1beta1.VMClone, providerInstance contracts.Provider) (ctrl.Result, bool) {
	reporter, ok := providerInstance.(contracts.CapabilityReporter)
	if !ok {
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonUnsupported,
			"the clustered provider does not report its capabilities, so it cannot be confirmed to route clones to a host"), false
	}
	caps, err := reporter.GetCapabilities(ctx)
	if err != nil {
		logging.FromContext(ctx).Info("GetCapabilities failed; not creating the clone's target yet", "error", err.Error())
		return r.markPending(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
			fmt.Sprintf("waiting for the provider's capabilities before a clustered clone: %v", err)), false
	}
	if !caps.SupportsRoutedClone {
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonUnsupported,
			"the clustered provider does not route clones to a host (supportsRoutedClone=false; a clustered provider older than "+
				"ADR-0007 Addendum A slice 3): upgrade the provider, then recreate the VMClone"), false
	}
	return ctrl.Result{}, true
}

// cloneLandingHostProblem reports why host — the source VM's bound host, where
// the clone must land — cannot take a new VM: it is not a Host of provider
// (gone), is being deleted or cordoned, or is not Ready. It returns an empty
// reason when the host can take the clone. The Host is looked up in the
// Provider's namespace, where a clustered Provider's Hosts live.
func (r *VMCloneReconciler) cloneLandingHostProblem(ctx context.Context, provider *infrav1beta1.Provider, host string) (string, string, error) {
	h := &infrav1beta1.Host{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: provider.Namespace, Name: host}, h); err != nil {
		if apierrors.IsNotFound(err) {
			return cloneReasonSourceHostGone, fmt.Sprintf(
				"the source VM's host %s no longer exists; a clone lands only on its source's host", host), nil
		}
		return "", "", fmt.Errorf("get Host %s/%s: %w", provider.Namespace, host, err)
	}
	switch {
	case h.Spec.ProviderRef.Name != provider.Name:
		return cloneReasonSourceHostGone, fmt.Sprintf(
			"the source VM's host %s is not a host of provider %s; a clone lands only on its source's host", host, provider.Name), nil
	case !h.DeletionTimestamp.IsZero():
		return cloneReasonSourceHostCordoned, fmt.Sprintf(
			"the source VM's host %s is being deleted and takes no new VM; a clone lands only on its source's host", host), nil
	case !h.Spec.Schedulable:
		return cloneReasonSourceHostCordoned, fmt.Sprintf(
			"the source VM's host %s is cordoned (spec.schedulable=false) and takes no new VM; a clone lands only on its source's host", host), nil
	case h.Status.Health != infrav1beta1.HostHealthReady:
		return cloneReasonSourceHostNotReady, fmt.Sprintf(
			"the source VM's host %s is not Ready (health %q); the clone waits for it and never lands on another host", host, h.Status.Health), nil
	}
	return "", "", nil
}

// waitForCloneHost records that the clone waits for its landing host (reason,
// msg on the Ready condition, Pending phase) and requeues after after. No
// provider call is made and no other host is tried.
func (r *VMCloneReconciler) waitForCloneHost(ctx context.Context, clone *infrav1beta1.VMClone, reason, msg string, after time.Duration) ctrl.Result {
	logging.FromContext(ctx).Info("Clone cannot land on its source VM's host yet; waiting", "reason", reason, "message", msg)
	res := r.markPending(ctx, clone, reason, msg)
	res.RequeueAfter = after
	return res
}

// ensureClusteredCloneTarget returns the clone's target VirtualMachine,
// creating it (after re-reading the grants live) when absent. A target that
// this clone did not create, that is already bound to a VM, or that references
// another Provider is never used (TargetConflict, Failed). done=true means the
// caller returns (res, err) as is.
func (r *VMCloneReconciler) ensureClusteredCloneTarget(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	sourceVM *infrav1beta1.VirtualMachine,
	provider *infrav1beta1.Provider,
	targetNamespace string,
) (*infrav1beta1.VirtualMachine, ctrl.Result, bool, error) {
	logger := logging.FromContext(ctx)
	key := client.ObjectKey{Namespace: targetNamespace, Name: clone.Spec.Target.Name}
	target := &infrav1beta1.VirtualMachine{}
	err := r.Get(ctx, key, target)
	switch {
	case apierrors.IsNotFound(err):
		if allowed, res, liveErr := r.confirmTargetNamespaceLive(ctx, clone, targetNamespace); !allowed {
			return nil, res, true, liveErr
		}
		if allowed, res, liveErr := r.gateConsumers(ctx, r.liveReader(), clone, sourceVM, targetNamespace); !allowed {
			return nil, res, true, liveErr
		}
		target = r.buildTargetVM(clone, sourceVM, targetNamespace)
		if createErr := r.Create(ctx, target); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				// Created concurrently: re-read (and re-check) it next time.
				return nil, ctrl.Result{Requeue: true}, true, nil
			}
			logger.Error(createErr, "Failed to create target VM CR", "vm", key.Name)
			return nil, r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError,
				fmt.Sprintf("failed to create target VM: %v", createErr)), true, nil
		}
		r.Recorder.Event(clone, "Normal", infrav1beta1.VMCloneReasonCloning,
			fmt.Sprintf("Created target VM %q before cloning onto its source's host", key.Name))
		// Record WHICH object this clone created, before anything else: only
		// it is ever used as the target or removed when the clone fails.
		clone.Status.TargetUID = string(target.UID)
		if err := r.updateStatus(ctx, clone); err != nil {
			return nil, ctrl.Result{}, true, fmt.Errorf("record the created target VM %s/%s on VMClone %s/%s: %w",
				target.Namespace, target.Name, clone.Namespace, clone.Name, err)
		}
	case err != nil:
		logger.Error(err, "Failed to get target VM CR", "vm", key.Name)
		return nil, ctrl.Result{RequeueAfter: 30 * time.Second}, true, nil
	default:
		if bindErr := cloneTargetBindable(clone, target, provider, ""); bindErr != nil {
			return nil, r.markFailed(ctx, clone, cloneReasonTargetConflict, bindErr.Error()), true, nil
		}
	}
	if target.UID == "" {
		// The API server assigns it on create; without it the clone cannot be
		// stamped, so nothing is sent.
		return nil, ctrl.Result{Requeue: true}, true, nil
	}
	return target, ctrl.Result{}, false, nil
}

// recordClonePendingHost durably records host — where the clone is about to
// be created — as the target VM's status.placement.pendingHost, with the
// source's pool, the Provider the target is bound through and the size the
// clone was admitted at (adm), BEFORE the Clone RPC (ADR-0007 Addendum A,
// A2). As for a Create it is a CHECKED status update (resourceVersion
// precondition): on a conflict the reconcile requeues and no clone is sent.
// A pendingHost already naming host is reused as is; one naming another host
// is never overwritten (the clone fails with TargetConflict). A write the API
// server refused releases the clone's capacity assumption; after an ambiguous
// failure it keeps counting until the record shows or its TTL passes.
func (r *VMCloneReconciler) recordClonePendingHost(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	target *infrav1beta1.VirtualMachine,
	provider *infrav1beta1.Provider,
	sourceVM *infrav1beta1.VirtualMachine,
	host string,
	adm clonePlacementAdmission,
) (ctrl.Result, bool, error) {
	pl := target.Status.Placement
	if pl == nil {
		pl = &infrav1beta1.PlacementStatus{}
		target.Status.Placement = pl
	}
	switch pending := strings.TrimSpace(pl.PendingHost); {
	case pending == host && pl.PendingResources != nil:
		// Recorded by an earlier attempt, with its admitted size.
		return ctrl.Result{}, true, nil
	case pending == host:
		// Pending without an admitted size: record the one just admitted.
	case pending != "" || (strings.TrimSpace(pl.Host) != "" && strings.TrimSpace(pl.Host) != host):
		r.forgetClonePlacement(target)
		return r.markFailed(ctx, clone, cloneReasonTargetConflict, fmt.Sprintf(
			"target VM %s/%s records a placement on another host (%q pending, %q bound); the clone lands only on its source's host %s",
			target.Namespace, target.Name, pl.PendingHost, pl.Host, host)), false, nil
	}

	recordBoundProvider(target, provider)
	now := metav1.Now()
	pl.PendingHost = host
	if sourceVM.Status.Placement != nil {
		pl.Pool = sourceVM.Status.Placement.Pool
	}
	pl.LastScheduledTime = &now
	pl.Reason = fmt.Sprintf("clone of %s pending on its host", sourceVM.Name)
	recordClonePendingSize(pl, adm)
	setPlacedCondition(target, metav1.ConditionFalse, k8s.ReasonCreatePending,
		fmt.Sprintf("clone pending on host %s (the source VM's host)", host))
	writeCtx, cancel := context.WithTimeout(ctx, pendingHostWriteTimeout)
	defer cancel()
	if err := r.Status().Update(writeCtx, target); err != nil {
		if pendingHostWriteRejected(err) {
			r.forgetClonePlacement(target)
		}
		if apierrors.IsConflict(err) {
			logging.FromContext(ctx).Info("Target pending-host write lost a resourceVersion race; requeueing without cloning", "host", host)
			return ctrl.Result{Requeue: true}, false, nil
		}
		return ctrl.Result{}, false, fmt.Errorf("record pending host %s for clone target %s/%s: %w", host, target.Namespace, target.Name, err)
	}
	return ctrl.Result{}, true, nil
}

// handleClusteredCloneError handles a failed Clone RPC on the clone's landing
// host:
//
//   - a previous incarnation of the target VM (VM_PREVIOUS_INCARNATION,
//     ADR-0007 A6 R2): the target keeps its pendingHost, the host is NOT
//     excluded, and the clone waits (RestorePending) until an administrator
//     re-attaches or removes that domain (holdCloneForPreviousIncarnation).
//   - a name conflict (Conflict / ALREADY_EXISTS): a domain of the clone's name
//     that the target VM does not own is on the host. The provider checks that
//     before copying anything, so nothing was created: as for a Create (A2
//     amendment), the host is excluded for the target and its pendingHost
//     cleared, in one checked status update. The clone can land nowhere else,
//     so it waits (SourceHostExcluded) until an administrator resolves it.
//   - the host unreachable, or the provider unavailable: the pendingHost is
//     kept and the clone is retried on the SAME host — a clone the provider
//     already made there is stamped with the target's uid and is an
//     idempotent success.
//   - anything else fails the clone. The target keeps its pendingHost, and the
//     failed clone removes it (removeFailedClusteredTarget): its finalizer runs
//     the owner-checked cleanup on the host.
func (r *VMCloneReconciler) handleClusteredCloneError(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	target *infrav1beta1.VirtualMachine,
	host string,
	err error,
) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)
	switch {
	case contracts.IsVMPreviousIncarnation(err):
		// ADR-0007 A6, R2: a previous incarnation of the TARGET VirtualMachine
		// exists on a host of the Provider. The target keeps its pendingHost
		// (the host is not excluded) and the clone waits; nothing is created
		// until an administrator re-attaches or removes it.
		return r.holdCloneForPreviousIncarnation(ctx, clone, target, host, err)
	case contracts.IsConflict(err):
		pl := target.Status.Placement
		if pl == nil {
			pl = &infrav1beta1.PlacementStatus{}
			target.Status.Placement = pl
		}
		excludeHost(pl, host)
		pl.PendingHost = ""
		pl.PendingResources = nil
		// Nothing holds capacity on the host any more.
		r.forgetClonePlacement(target)
		setPlacedCondition(target, metav1.ConditionFalse, k8s.ReasonHostExcluded, fmt.Sprintf(
			"the clone onto host %s was refused: a same-named domain this VirtualMachine does not own exists there, so nothing was created; "+
				"a clone lands only on its source's host", host))
		if uerr := r.Status().Update(ctx, target); uerr != nil {
			if apierrors.IsConflict(uerr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("record excluded host %s for clone target %s/%s: %w", host, target.Namespace, target.Name, uerr)
		}
		r.Recorder.Event(clone, "Warning", cloneReasonSourceHostExcluded, fmt.Sprintf("Clone refused on host %s: %v", host, err))
		return r.waitForCloneHost(ctx, clone, cloneReasonSourceHostExcluded, fmt.Sprintf(
			"clone refused on the source VM's host %s: %v", host, err), cloneHostBlockedRetryInterval), nil
	case contracts.IsHostUnavailable(err):
		logger.Info("Clone could not reach its landing host; retrying on the same host", "host", host, "error", err.Error())
		return r.waitForCloneHost(ctx, clone, k8s.ReasonHostUnavailable, fmt.Sprintf(
			"clone on host %s could not complete (%v); it is retried on the same host", host, err), cloneHostUnavailableRetryInterval), nil
	case contracts.IsRetryable(err):
		// E.g. the provider answered that an earlier attempt's copy is still
		// running on the host (slice 3 review), or the provider is briefly
		// unavailable: retry on the same host, never start elsewhere.
		logger.Info("Clone not done yet; retrying on the same host", "host", host, "error", err.Error())
		return r.waitForCloneHost(ctx, clone, cloneReasonRetrying, fmt.Sprintf(
			"clone on host %s is not done yet (%v); it is retried on the same host", host, err), cloneHostUnavailableRetryInterval), nil
	default:
		logger.Error(err, "Clone RPC failed", "host", host)
		r.Recorder.Event(clone, "Warning", infrav1beta1.VMCloneReasonProviderError, fmt.Sprintf("Clone failed: %v", err))
		return r.markFailed(ctx, clone, infrav1beta1.VMCloneReasonProviderError, fmt.Sprintf(
			"clone failed: %v (the target VM %s/%s this clone created is removed; its finalizer removes anything the clone left on host %s)",
			err, target.Namespace, target.Name, host)), nil
	}
}

// holdCloneForPreviousIncarnation is ADR-0007 A6, R2 for a clustered clone:
// the provider refused the Clone with VM_PREVIOUS_INCARNATION, because a
// domain stamped with the TARGET VirtualMachine's namespace and name under
// another UID exists on a host of the Provider. The target keeps its
// pendingHost (so it keeps counting on its host and nothing is excluded) and
// shows Placed=False/RestorePending — or Placed=False/OwnDomainOnAnotherHost
// when the domain is the target's own (stamped with its UID), which also holds
// the target's delete; the clone stays Pending with the same reason and one
// Warning event, and is re-checked with the blocked-VM backoff
// (blockedRetryBackoff: 15 s doubling to 5 min) — it is never failed for this
// (a failed clone would remove the target, and the next attempt would meet the
// same domain).
func (r *VMCloneReconciler) holdCloneForPreviousIncarnation(
	ctx context.Context,
	clone *infrav1beta1.VMClone,
	target *infrav1beta1.VirtualMachine,
	host string,
	err error,
) (ctrl.Result, error) {
	logging.FromContext(ctx).Info("Clone refused: a previous incarnation of the target VM (or its own domain) exists on a host "+
		"of the Provider; holding it (the host is not excluded)", "host", host, "target", target.Name, "error", err.Error())
	// The target's own domain elsewhere is held with its own reason, which
	// also keeps the target's finalizer if it is deleted meanwhile
	// (heldForOwnDomainElsewhere): its delete on the pending host would find
	// nothing and leave that domain running.
	reason, holdMsg := incarnationHold(err)
	setPlacedCondition(target, metav1.ConditionFalse, reason, holdMsg)
	if uerr := r.Status().Update(ctx, target); uerr != nil {
		if apierrors.IsConflict(uerr) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("record %s for clone target %s/%s: %w", reason, target.Namespace, target.Name, uerr)
	}
	metrics.RecordError(errReasonRestorePending, metrics.ComponentManager)
	msg := fmt.Sprintf("the clone's target VirtualMachine %s/%s is %s", target.Namespace, target.Name, holdMsg)
	if c := meta.FindStatusCondition(clone.Status.Conditions, infrav1beta1.VMCloneConditionReady); c == nil || c.Reason != reason {
		r.Recorder.Event(clone, "Warning", reason, msg)
	}
	return r.waitForCloneHost(ctx, clone, reason, msg,
		blockedRetryBackoff(createHoldSince(target))), nil
}

const (
	// cloneConditionTargetCleanup is the VMClone condition recording that a
	// failed clustered clone decided, once, what to do with the target
	// VirtualMachine it created: True/TargetRemoved (deleted) or
	// False/TargetKept (left, with why). Its presence means the decision is
	// never made again.
	cloneConditionTargetCleanup = "TargetCleanup"
	// cloneReasonTargetRemoved is the TargetCleanup reason (and the event
	// reason) when the target the clone created was deleted.
	cloneReasonTargetRemoved = "TargetRemoved"
	// cloneReasonTargetKept is the TargetCleanup reason when the target was
	// left in place.
	cloneReasonTargetKept = "TargetKept"
)

// removeFailedClusteredTarget deletes, ONCE, after a clone has failed for good
// (phase Failed, terminal), the target VirtualMachine a CLUSTERED clone
// created before its Clone RPC — so a failed clone does not leave an empty VM
// behind. The VirtualMachine's finalizer then runs the owner-checked Delete on
// its pending host, which removes a domain the clone may have defined there
// stamped as that VM's.
//
// It deletes only the object the clone created — the one whose uid it
// recorded (status.targetUID) when it created it; the clone-uid annotation
// alone is copyable and is never enough — and only when that target:
//   - is not bound (no status.id, no status.placement.host) — a clone that
//     succeeded and bound its target (even if the clone's own status write
//     was then lost) keeps it;
//   - is not one the clone refused to use (TargetConflict);
//   - is managed through a clustered Provider — a single-host clone creates
//     its target only after the clone succeeded, and is unchanged;
//   - is in a namespace that still grants the clone's namespace (live read).
//
// The delete carries the recorded uid and the checked resourceVersion as
// preconditions, so a target bound or replaced meanwhile is never deleted.
// The decision — removed, or kept and why — is recorded as the TargetCleanup
// condition, and is never made again: a VirtualMachine created later under
// the target name is never looked at. A failure to read or delete is retried
// without recording anything. Retryable, Pending and blocked-host states never
// reach here: only the terminal Failed phase does.
func (r *VMCloneReconciler) removeFailedClusteredTarget(ctx context.Context, clone *infrav1beta1.VMClone) ctrl.Result {
	if meta.FindStatusCondition(clone.Status.Conditions, cloneConditionTargetCleanup) != nil {
		return ctrl.Result{}
	}
	logger := logging.FromContext(ctx)
	retry := ctrl.Result{RequeueAfter: 30 * time.Second}
	targetNamespace := cloneTargetNamespace(clone)
	if clone.Spec.Target.Name == "" || clone.Status.TargetUID == "" {
		// Not a clone that created its target before cloning (single-host,
		// or it failed before creating one): nothing of its own to remove.
		return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
			"this clone recorded no target VirtualMachine of its own")
	}
	key := client.ObjectKey{Namespace: targetNamespace, Name: clone.Spec.Target.Name}
	target := &infrav1beta1.VirtualMachine{}
	if err := r.Get(ctx, key, target); err != nil {
		if apierrors.IsNotFound(err) {
			return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
				"the target VirtualMachine this clone created no longer exists")
		}
		logger.Error(err, "Failed to read the failed clone's target VM", "vm", key.Name)
		return retry
	}
	switch {
	case string(target.UID) != clone.Status.TargetUID:
		return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
			fmt.Sprintf("VirtualMachine %s/%s is not the one this clone created; it is left alone", target.Namespace, target.Name))
	case !target.DeletionTimestamp.IsZero():
		return r.recordTargetCleanup(ctx, clone, metav1.ConditionTrue, cloneReasonTargetRemoved,
			fmt.Sprintf("the target VirtualMachine %s/%s this clone created is being deleted", target.Namespace, target.Name))
	case !failedCloneOwnsUnboundTarget(clone, target):
		return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
			fmt.Sprintf("the target VirtualMachine %s/%s is bound; it is kept", target.Namespace, target.Name))
	case failedOnItsTarget(clone):
		return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
			fmt.Sprintf("the clone refused its target VirtualMachine %s/%s; it is left alone", target.Namespace, target.Name))
	}
	provider := &infrav1beta1.Provider{}
	if err := r.Get(ctx, vmProviderKey(target), provider); err != nil {
		if apierrors.IsNotFound(err) {
			// Its topology cannot be told: leave the target alone.
			return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
				"the target's Provider no longer exists; the target is left alone")
		}
		logger.Error(err, "Failed to read the failed clone target's Provider", "vm", key.Name)
		return retry
	}
	if !isClusterTopology(provider) {
		return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
			"the target is not on a clustered Provider; it is left alone")
	}
	if allowed, err := targetNamespaceAllowed(ctx, r.liveReader(), clone.Namespace, targetNamespace); err != nil {
		logger.Error(err, "Failed to check the target namespace grant before removing the failed clone's target")
		return retry
	} else if !allowed {
		return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
			"the target namespace no longer grants this clone's namespace; the target is left to its namespace")
	}

	uid, rv := target.UID, target.ResourceVersion
	if err := r.Delete(ctx, target, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return r.recordTargetCleanup(ctx, clone, metav1.ConditionFalse, cloneReasonTargetKept,
				"the target VirtualMachine this clone created no longer exists")
		case apierrors.IsConflict(err):
			// Changed since it was checked (possibly bound): check it again.
			return ctrl.Result{Requeue: true}
		}
		logger.Error(err, "Failed to remove the failed clone's target VM", "vm", key.Name)
		return retry
	}
	logger.Info("Removed the target VM the failed clone created", "vm", key.Name)
	msg := fmt.Sprintf("Removed target VM %s/%s: this clone created it and failed; its finalizer removes anything the clone left on host %q",
		target.Namespace, target.Name, pendingHostOf(target))
	r.Recorder.Event(clone, "Normal", cloneReasonTargetRemoved, msg)
	return r.recordTargetCleanup(ctx, clone, metav1.ConditionTrue, cloneReasonTargetRemoved, msg)
}

// recordTargetCleanup records the failed clone's one-time target decision as
// its TargetCleanup condition. A failed status write is retried (the decision
// is then made again, on the state it finds).
func (r *VMCloneReconciler) recordTargetCleanup(ctx context.Context, clone *infrav1beta1.VMClone,
	status metav1.ConditionStatus, reason, msg string) ctrl.Result {
	utilk8s.SetCondition(&clone.Status.Conditions, cloneConditionTargetCleanup, status, reason, msg)
	if err := r.updateStatus(ctx, clone); err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}
	}
	return ctrl.Result{}
}

// failedCloneOwnsUnboundTarget reports whether target is a VirtualMachine
// clone created (its UID marker) and that is not bound to any VM: no
// status.id and no bound host. It is not already being deleted.
func failedCloneOwnsUnboundTarget(clone *infrav1beta1.VMClone, target *infrav1beta1.VirtualMachine) bool {
	if clone.UID == "" || target.Annotations[CloneAnnotationCloneUID] != string(clone.UID) {
		return false
	}
	if !target.DeletionTimestamp.IsZero() || target.Status.ID != "" {
		return false
	}
	return target.Status.Placement == nil || strings.TrimSpace(target.Status.Placement.Host) == ""
}

// failedOnItsTarget reports whether clone failed because it refused its
// target (TargetConflict: e.g. the target records a placement on another
// host). A target the clone would not use is in a state the clone does not
// own, and is left alone.
func failedOnItsTarget(clone *infrav1beta1.VMClone) bool {
	for _, c := range clone.Status.Conditions {
		if c.Type == infrav1beta1.VMCloneConditionReady && c.Reason == cloneReasonTargetConflict {
			return true
		}
	}
	return false
}

// pendingHostOf is the host target's create or clone is pending on, or "".
func pendingHostOf(target *infrav1beta1.VirtualMachine) string {
	if target.Status.Placement == nil {
		return ""
	}
	return strings.TrimSpace(target.Status.Placement.PendingHost)
}

// containsHost reports whether hosts lists host.
func containsHost(hosts []string, host string) bool {
	for _, h := range hosts {
		if strings.TrimSpace(h) == host {
			return true
		}
	}
	return false
}
