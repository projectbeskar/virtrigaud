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
//   - Failed: status.currentResources is untouched and the VM gets
//     Reconfiguring=False/ProviderError. Because a failed Reconfigure may have
//     changed part of the persistent definition before it failed, it is sent
//     again even if the spec is reverted to the recorded size, until one
//     succeeds.
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

// recordRestartPending records a synchronous Reconfigure the provider applied
// to the VM's persistent definition only: status.currentResources raised to
// the desired size per resource that grows and kept per resource that shrinks
// (the invariant above), Reconfiguring=True/RestartRequired, and the VM still
// Running and Ready. If the effective resources cannot be computed (unexpected:
// the request was just built from them) status.currentResources is left as it
// is.
func (r *VirtualMachineReconciler) recordRestartPending(vm *infravirtrigaudiov1beta1.VirtualMachine, vmClass *infravirtrigaudiov1beta1.VMClass) {
	desiredCPU, desiredMem32, err := effectiveResources(vm, vmClass)
	if err == nil {
		cpu := max(r.getCurrentCPU(vm), desiredCPU)
		mem := max(r.getCurrentMemoryMiB(vm), int64(desiredMem32))
		if vm.Status.CurrentResources == nil {
			vm.Status.CurrentResources = &infravirtrigaudiov1beta1.VirtualMachineResources{}
		}
		vm.Status.CurrentResources.CPU = &cpu
		vm.Status.CurrentResources.MemoryMiB = &mem
	}
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

// pendingReconfigureRecheck reports whether the VM must be sent a Reconfigure
// even though its spec matches status.currentResources (due), and, for a
// change pending a restart, how long to wait before asking the provider again
// (wait > 0 means: do not send one now, whatever the spec says).
//
//   - Reconfiguring=True/RestartRequired: due once restartPendingRecheckInterval
//     has passed since the last Reconfigure, or at once when the spec changed
//     since (the condition's observedGeneration); otherwise wait.
//   - Reconfiguring=False/ProviderError (the last Reconfigure failed, possibly
//     after changing part of the definition): due.
//   - anything else: not due.
func (r *VirtualMachineReconciler) pendingReconfigureRecheck(vm *infravirtrigaudiov1beta1.VirtualMachine) (due bool, wait time.Duration) {
	cond := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReconfiguring)
	if cond == nil {
		return false, 0
	}
	switch {
	case cond.Status == metav1.ConditionFalse && cond.Reason == k8s.ReasonProviderError:
		return true, 0
	case cond.Status == metav1.ConditionTrue && cond.Reason == k8s.ReasonRestartRequired:
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
//   - A recorded ceiling above what the provider now reports — a confirmed
//     shrink lowered the domain's memory maximum (review R3), or the create
//     provisioned less than was scheduled — is lowered to it. Never while a
//     Reconfigure is in flight or pending a restart: the running domain's
//     maximum is then not necessarily what it boots with next.
//
// A recorded ceiling is never raised: a grow beyond it is recorded in
// status.currentResources, which the accounting counts at least. The value
// recorded is the reported maximum when it exceeds the recorded memory, else 0
// (no headroom beyond the VM's own size). The change is persisted with the
// reconcile's status write.
func (r *VirtualMachineReconciler) syncMemoryCeiling(ctx context.Context, vm *infravirtrigaudiov1beta1.VirtualMachine, ref contracts.VMRef, desc contracts.DescribeResponse) {
	pl := vm.Status.Placement
	if !ref.Routed() || pl == nil || pl.Host == "" || desc.MaxMemoryMiB <= 0 {
		return
	}
	reported := desc.MaxMemoryMiB
	if reported <= r.getCurrentMemoryMiB(vm) {
		reported = 0
	}
	logger := log.FromContext(ctx)
	switch {
	case pl.MemoryCeilingMiB == nil:
		pl.MemoryCeilingMiB = &reported
		logger.Info("Recorded the clustered VM's memory ceiling from its provider (once)", "memoryCeilingMiB", reported)
	case reported < *pl.MemoryCeilingMiB && vm.Status.ReconfigureTaskRef == "" && !restartPending(vm):
		logger.Info("Lowered the clustered VM's memory ceiling to what its provider reports",
			"from", *pl.MemoryCeilingMiB, "to", reported)
		pl.MemoryCeilingMiB = &reported
	}
}
