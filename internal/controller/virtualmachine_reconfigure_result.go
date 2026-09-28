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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// A Reconfigure the provider confirms has one of three results (the honest
// Reconfigure contract): applied (to the running VM and its persistent
// definition), applied to the persistent definition only — RestartRequired,
// taking effect at the VM's next power cycle — or failed.
//
// status.currentResources invariant. status.currentResources never records
// less than the VM can hold, now or after its next boot: per resource, it is
// the larger of the size the VM runs with and the size it boots with next.
//
//   - Applied: the desired size (the running and the next-boot size are the
//     same) — as since #354, written only after the provider confirmed it.
//   - RestartRequired: per resource, max(recorded, desired). A grow pending a
//     restart IS recorded — the next boot takes it, so the committed-capacity
//     accounting of a clustered Provider must count it from now on, and the
//     admitted resize's assumption settles. A shrink pending a restart is NOT
//     recorded: the running VM still holds its old size, and lowering the
//     counted size would let other VMs be placed into capacity it uses. The
//     VM gets Reconfiguring=True/RestartRequired, and the provider is asked
//     again every restartPendingRecheckInterval (sooner if the spec changes):
//     once the VM has been power-cycled it answers "applied", and the desired
//     size — the smaller one, for a shrink — is recorded.
//   - Failed: the VM gets Reconfiguring=False/ProviderError. A failed
//     Reconfigure may have applied part of the change before it failed — the
//     definition, or even the running VM — so on a clustered Provider
//     status.currentResources is recorded exactly as for RestartRequired, per
//     resource max(recorded, desired) (review H1): what failed may still run
//     at the larger size, and the committed-capacity accounting must not
//     count it below that. On a single-host Provider, which has no capacity
//     accounting, it is left untouched (#354: status reports only a size the
//     provider confirmed). Either way the Reconfigure is sent again — on a
//     per-VM backoff from 5 s doubling to 5 min (review H2), at once after a
//     spec change, and even if the spec is reverted to the recorded size —
//     until one succeeds, which then records what was applied.
//   - A clustered VM's recorded CPU is also raised to the vCPUs its provider
//     reports it has (DescribeResponse.vcpus), and its memory ceiling to the
//     memory maximum its provider reports, whenever those are higher
//     (syncRecordedCPU, syncMemoryCeiling): whatever a failed or partial
//     change left behind, the VM is never counted below what it holds.
//
// On a clustered Provider a shrink of a running VM is deferred until the VM is
// powered off (ShrinkPendingPowerOff, #356), and a powered-off domain takes a
// change persistently — which is applied, not pending — so a clustered shrink
// pending a restart only arises when the VM was started between the Describe
// and the Reconfigure; the invariant covers it.

// restartPendingRecheckInterval is how often the provider is asked again
// whether a change pending a restart has taken effect (the VM was power-cycled)
// while the spec stays the same.
const restartPendingRecheckInterval = 2 * time.Minute

// reconfigureRetryMaxInterval caps the per-VM backoff of re-sending a failed
// Reconfigure (review H2): it starts at providerErrorRetryInterval and doubles
// with each consecutive failure, so one tenant's persistently failing resize
// cannot hammer its provider every few seconds.
const reconfigureRetryMaxInterval = 5 * time.Minute

// recordRestartPending records a synchronous Reconfigure the provider applied
// to the VM's persistent definition only: status.currentResources raised to
// the desired size per resource that grows and kept per resource that shrinks
// (the invariant above), Reconfiguring=True/RestartRequired, and the VM still
// Running and Ready. If the effective resources cannot be computed (unexpected:
// the request was just built from them) status.currentResources is left as it
// is.
func (r *VirtualMachineReconciler) recordRestartPending(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) {
	desiredCPU, desiredMem32, _ := r.recordAtLeastDesired(vm, vmClass)
	msg := fmt.Sprintf("the new size (%d vCPU, %d MiB) is in the VM's persistent definition and takes effect at its next power cycle "+
		"(power off, then on; a reboot from inside the guest is not enough). Until then the running VM keeps its previous size; "+
		"status.currentResources counts the larger of the two", desiredCPU, desiredMem32)
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReconfiguring,
		Status:             metav1.ConditionTrue,
		Reason:             k8s.ReasonRestartRequired,
		Message:            msg,
		ObservedGeneration: vm.Generation,
	})
	vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
	k8s.SetReadyCondition(&vm.Status.Conditions, metav1.ConditionTrue, k8s.ReasonReconcileSuccess, "VM is ready")
}

// providerAdvertisesHonestReconfigure reports whether the Provider CR reports
// the honest Reconfigure result (status.reportedCapabilities.
// supportsHonestReconfigure). A nil ReportedCapabilities (not reported yet, or
// an older provider) reads as false.
func providerAdvertisesHonestReconfigure(provider *infravirtrigaudiov1beta1.Provider) bool {
	caps := provider.Status.ReportedCapabilities
	return caps != nil && caps.SupportsHonestReconfigure
}

// holdResizeWithoutHonestReconfigure holds a resize — grow or shrink — of a
// VM on a clustered Provider that does not report the honest Reconfigure
// result (review H3): an older provider can answer success for a change it
// did not apply, and the committed-capacity accounting would trust it. The VM
// keeps its size and gets Reconfiguring=False/ProviderLacksHonestReconfigure,
// re-checked every placementConfigRetryInterval; nothing is sent. It reports
// whether it held. A single-host Provider is resized as before, with a
// warning in the manager log.
func (r *VirtualMachineReconciler) holdResizeWithoutHonestReconfigure(
	ctx context.Context,
	vm *infravirtrigaudiov1beta1.VirtualMachine,
	ref contracts.VMRef,
	providerCR *infravirtrigaudiov1beta1.Provider,
) (ctrl.Result, bool) {
	if providerCR == nil || providerAdvertisesHonestReconfigure(providerCR) {
		return ctrl.Result{}, false
	}
	logger := log.FromContext(ctx)
	if !ref.Routed() {
		logger.Info("WARNING: the VM's Provider does not report supportsHonestReconfigure; its Reconfigure may report a change "+
			"applied that was not, so status.currentResources may not match the VM. Upgrade the provider image.",
			"provider", providerCR.Name)
		return ctrl.Result{}, false
	}
	msg := fmt.Sprintf("the VM is not resized: its Provider %s does not report supportsHonestReconfigure (an older provider image), "+
		"so a Reconfigure could report a change applied that was not, and the host's committed capacity would count a size the VM "+
		"does not have. Upgrade the provider image; the VM keeps its current size until then", providerCR.Name)
	logger.Info("Holding a clustered resize: the Provider does not report the honest Reconfigure result", "provider", providerCR.Name)
	setResizeRefused(vm, k8s.ReasonProviderLacksHonestReconfigure, msg)
	r.updatePlacementStatus(ctx, vm)
	return ctrl.Result{RequeueAfter: placementConfigRetryInterval}, true
}

// recordUnmarkedResult records a Reconfigure of a clustered VM that the
// provider answered without the honest-result marker (TaskResponse.
// honest_result, review L1): a provider image that predates the contract —
// rolled back, or not yet rolled out — while the manager still holds a
// capability snapshot saying otherwise. Its "success" may cover a change it
// did not apply, so it is recorded like a failure or a restart-pending change,
// at max(recorded, desired) per resource, with the capability hold's
// condition (Reconfiguring=False/ProviderLacksHonestReconfigure). The
// provider is asked again every restartPendingRecheckInterval (at once after a
// spec change); a marked answer then records what was applied.
func (r *VirtualMachineReconciler) recordUnmarkedResult(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) {
	r.recordAtLeastDesired(vm, vmClass)
	setResizeRefused(vm, k8s.ReasonProviderLacksHonestReconfigure,
		"the VM's Provider answered the resize without the honest-result marker (an older provider image), so the answer is not "+
			"trusted: the VM is counted at the larger of its previous and its requested size, and the resize is re-checked every "+
			"2 minutes. Upgrade the provider image")
	vm.Status.Phase = infravirtrigaudiov1beta1.VirtualMachinePhaseRunning
}

// recordAtLeastDesired raises status.currentResources, per resource, to the
// VM's desired size (effectiveResources) where that is larger, and keeps it
// where it is not: max(recorded, desired). It returns the desired size, and
// ok == false (nothing recorded) if the effective resources cannot be computed
// — unexpected, since a request was just built from them.
func (r *VirtualMachineReconciler) recordAtLeastDesired(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) (desiredCPU, desiredMemMiB int32, ok bool) {
	desiredCPU, desiredMemMiB, err := effectiveResources(vm, vmClass)
	if err != nil {
		return desiredCPU, desiredMemMiB, false
	}
	cpu := max(r.getCurrentCPU(vm), desiredCPU)
	mem := max(r.getCurrentMemoryMiB(vm), int64(desiredMemMiB))
	if vm.Status.CurrentResources == nil {
		vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{}
	}
	vm.Status.CurrentResources.CPU = &cpu
	vm.Status.CurrentResources.MemoryMiB = &mem
	return desiredCPU, desiredMemMiB, true
}

// recordReconfigureFailure records a Reconfigure that failed with err:
// Reconfiguring=False/ProviderError (observedGeneration set, so a later spec
// change is sent at once) and — on a clustered Provider, whose
// committed-capacity accounting trusts status.currentResources — the size
// counted at max(recorded, desired) per resource, because the failed call may
// have applied part of the change (review H1). A NotFound (the domain is gone
// or not this VM's: the provider's ownership check refused it before anything
// changed) records nothing; the caller hands it to the routed-error handling.
// It returns how long to wait before the Reconfigure is sent again: the VM's
// next step on the reconfigureRetry backoff (review H2), 0 for a NotFound.
func (r *VirtualMachineReconciler) recordReconfigureFailure(vm *infravirtrigaudiov1beta1.VirtualMachine, ref contracts.VMRef, vmClass *infravirtrigaudiov1beta1.VMClass, err error) time.Duration {
	if contracts.IsNotFound(err) {
		return 0
	}
	if ref.Routed() {
		r.recordAtLeastDesired(vm, vmClass)
	}
	retryAfter := r.reconfigureRetry.nextWithin(vmSchedulingUID(vm), r.now(), providerErrorRetryInterval, reconfigureRetryMaxInterval)
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:               k8s.ConditionReconfiguring,
		Status:             metav1.ConditionFalse,
		Reason:             k8s.ReasonProviderError,
		Message:            fmt.Sprintf("Failed to reconfigure VM: %v", err),
		ObservedGeneration: vm.Generation,
	})
	return retryAfter
}

// recordReconfigureSuccess forgets the VM's failed-Reconfigure backoff: the
// provider answered (applied, or applied to the definition pending a
// restart).
func (r *VirtualMachineReconciler) recordReconfigureSuccess(vm *infravirtrigaudiov1beta1.VirtualMachine) {
	r.reconfigureRetry.reset(vmSchedulingUID(vm))
}

// pendingReconfigureRecheck reports whether the VM must be sent a Reconfigure
// even though its spec matches status.currentResources (due), and how long to
// wait before asking the provider again (wait > 0 means: do not send one now,
// whatever the spec says).
//
//   - Reconfiguring=True/RestartRequired, or Reconfiguring=False/
//     ProviderLacksHonestReconfigure (an answer without the honest-result
//     marker, or the capability hold): due once restartPendingRecheckInterval
//     has passed since the last Reconfigure, or at once when the spec changed
//     since (the condition's observedGeneration); otherwise wait.
//   - Reconfiguring=False/ProviderError (the last Reconfigure failed, possibly
//     after changing part of the definition): due once the VM's
//     reconfigureRetry backoff (5 s doubling to 5 min) has passed, or at once
//     when the spec changed since; otherwise wait (review H2).
//   - anything else: not due.
func (r *VirtualMachineReconciler) pendingReconfigureRecheck(vm *infravirtrigaudiov1beta1.VirtualMachine) (due bool, wait time.Duration) {
	cond := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	if cond == nil {
		return false, 0
	}
	switch {
	case cond.Status == metav1.ConditionFalse && cond.Reason == k8s.ReasonProviderError:
		if cond.ObservedGeneration != vm.Generation {
			return true, 0
		}
		if w := r.reconfigureRetry.remaining(vmSchedulingUID(vm), r.now()); w > 0 {
			return false, w
		}
		return true, 0
	case cond.Status == metav1.ConditionTrue && cond.Reason == k8s.ReasonRestartRequired,
		cond.Status == metav1.ConditionFalse && cond.Reason == k8s.ReasonProviderLacksHonestReconfigure:
		// A change pending a restart, or an answer without the honest-result
		// marker (recordUnmarkedResult; the capability hold, which sends
		// nothing, re-checks the capability first on every pass).
		if cond.ObservedGeneration != vm.Generation || vm.Status.LastReconfigureTime == nil {
			return true, 0
		}
		if elapsed := r.now().Sub(vm.Status.LastReconfigureTime.Time); elapsed < restartPendingRecheckInterval {
			return false, restartPendingRecheckInterval - elapsed
		}
		return true, 0
	}
	return false, 0
}

// restartPending reports whether the VM's last Reconfigure left a change
// pending a restart.
func restartPending(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	cond := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	return cond != nil && cond.Status == metav1.ConditionTrue && cond.Reason == k8s.ReasonRestartRequired
}

// syncMemoryCeiling keeps status.placement.memoryCeilingMiB of a bound
// clustered VM true to what its provider reports (desc.MaxMemoryMiB: the most
// memory the guest can use without host action; 0 = not reported). The
// clustered accounting counts the VM's memory at the larger of this ceiling and
// status.currentResources, so it may never be below what the guest can reach.
//
//   - No ceiling recorded (a VM scheduled before the field existed): it is
//     recorded once from the provider (review R2). From then on the VMClass's
//     memory hot-add setting, which the VM's owner can flip, is never used to
//     size it again.
//   - A recorded ceiling below what the provider reports — more than both the
//     ceiling and the recorded memory — is raised to it at once (review H1): the
//     domain can reach that much, whatever a failed or partial Reconfigure
//     left behind, and counting it is always the conservative side.
//   - A recorded ceiling above what the provider now reports — a confirmed
//     shrink lowered the domain's memory maximum (review R3), or the create
//     provisioned less than was scheduled — is lowered to it. Never while a
//     Reconfigure is in flight, pending a restart, or failed and not yet
//     retried successfully: the running domain's maximum is then not
//     necessarily what it boots with next.
//
// The value recorded is the reported maximum when it exceeds the recorded
// memory, else 0 (no headroom beyond the VM's own size). The change is
// persisted with the reconcile's status write.
func (r *VirtualMachineReconciler) syncMemoryCeiling(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ref contracts.VMRef, desc contracts.DescribeResponse) {
	pl := vm.Status.Placement
	if !ref.Routed() || pl == nil || pl.Host == "" || desc.MaxMemoryMiB <= 0 {
		return
	}
	recordedMem := r.getCurrentMemoryMiB(vm)
	reported := desc.MaxMemoryMiB
	if reported <= recordedMem {
		reported = 0
	}
	logger := log.FromContext(ctx)
	switch {
	case pl.MemoryCeilingMiB == nil:
		pl.MemoryCeilingMiB = &reported
		logger.Info("Recorded the clustered VM's memory ceiling from its provider (once)", "memoryCeilingMiB", reported)
	case desc.MaxMemoryMiB > max(*pl.MemoryCeilingMiB, recordedMem):
		logger.Info("Raised the clustered VM's memory ceiling to what its provider reports",
			"from", *pl.MemoryCeilingMiB, "to", desc.MaxMemoryMiB)
		raised := desc.MaxMemoryMiB
		pl.MemoryCeilingMiB = &raised
	case reported < *pl.MemoryCeilingMiB && vm.Status.ReconfigureTaskRef == "" && !restartPending(vm) && !lastReconfigureFailed(vm):
		logger.Info("Lowered the clustered VM's memory ceiling to what its provider reports",
			"from", *pl.MemoryCeilingMiB, "to", reported)
		pl.MemoryCeilingMiB = &reported
	}
}

// lastReconfigureFailed reports whether the VM's last Reconfigure failed and has
// not been retried successfully since.
func lastReconfigureFailed(vm *infravirtrigaudiov1beta1.VirtualMachine) bool {
	cond := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == k8s.ReasonProviderError
}

// syncRecordedCPU raises the recorded CPU (status.currentResources.cpu) of a
// bound clustered VM to the vCPUs its provider reports it has online
// (desc.VCPUs; 0 = not reported) whenever that is higher (review H1): a
// failed or partial Reconfigure, or a change made outside VirtualMachine,
// can leave a domain with more vCPUs than recorded, and the
// committed-capacity accounting must never count it below what it runs with.
// It never lowers the recorded CPU — only a Reconfigure the provider confirms
// applied does — and does nothing for a VM with no recorded size.
func (r *VirtualMachineReconciler) syncRecordedCPU(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ref contracts.VMRef, desc contracts.DescribeResponse) {
	pl := vm.Status.Placement
	cur := vm.Status.CurrentResources
	if !ref.Routed() || pl == nil || pl.Host == "" || cur == nil || cur.CPU == nil || desc.VCPUs <= *cur.CPU {
		return
	}
	log.FromContext(ctx).Info("Raised the clustered VM's recorded CPU to the vCPUs its provider reports",
		"from", *cur.CPU, "to", desc.VCPUs)
	cpu := desc.VCPUs
	cur.CPU = &cpu
}
